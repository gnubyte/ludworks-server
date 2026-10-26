package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/media/probe"
	"ludworks/internal/media/subtitles"
)

// Playback limits.
const (
	maxStartSeconds  = 1e7
	defaultHeight    = 720
	subExtractLimit  = subtitles.MaxInputBytes
	subExtractTimout = 3 * time.Minute
	streamCSP        = "sandbox; default-src 'none'; media-src 'self'; frame-ancestors 'none'"
)

var transcodeHeights = map[int]bool{360: true, 480: true, 720: true, 1080: true}

// ServeStream writes the file's bytes (Range/If-Range/ETag capable, inline disposition).
func (s *Service) ServeStream(w http.ResponseWriter, r *http.Request, owner *core.Principal, fileID string) error {
	if _, err := s.itemFor(r.Context(), owner, fileID, needRead); err != nil {
		return err
	}
	return s.files.ServeContent(w, r, owner, fileID, false)
}

// ParseTranscodeParams validates ?start= and ?height= of a transcode request.
func ParseTranscodeParams(r *http.Request) (start float64, height int, err error) {
	q := r.URL.Query()
	if v := q.Get("start"); v != "" {
		start, err = strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(start) || math.IsInf(start, 0) || start < 0 || start > maxStartSeconds {
			return 0, 0, core.BadRequest("start: must be a number of seconds >= 0")
		}
	}
	height = defaultHeight
	if v := q.Get("height"); v != "" {
		height, err = strconv.Atoi(v)
		if err != nil || !transcodeHeights[height] {
			return 0, 0, core.BadRequest("height: must be 360, 480, 720 or 1080")
		}
	}
	return start, height, nil
}

// flushWriter flushes the response after every write so ffmpeg output reaches the client
// promptly, and records whether anything was written. Every write gets a fresh deadline
// (the main server has no WriteTimeout): a client that stops reading fails the write, which
// cancels ffmpeg (onFail) and so frees the transcode slot.
type flushWriter struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	timeout time.Duration
	onFail  func()
	written int64
	failed  bool
}

func (f *flushWriter) Write(p []byte) (int, error) {
	if f.failed {
		return 0, errClientStalled
	}
	_ = f.rc.SetWriteDeadline(time.Now().Add(f.timeout))
	n, err := f.w.Write(p)
	f.written += int64(n)
	if err == nil {
		if ferr := f.rc.Flush(); ferr != nil && !errors.Is(ferr, http.ErrNotSupported) {
			err = ferr
		}
	}
	if err != nil {
		f.failed = true
		if f.onFail != nil {
			f.onFail()
		}
	}
	return n, err
}

var errClientStalled = errors.New("media: transcode client stopped reading")

// ServeTranscode streams an ffmpeg fragmented-MP4 (H.264/AAC) rendition starting at
// startSeconds, scaled to at most height lines. 501 ffmpeg_unavailable without ffmpeg; 429
// when no transcode slot frees up within transcodeWait (maxTranscodes server-wide and
// maxTranscodesPerUser for the requesting user). ffmpeg is killed when the client goes away
// or stops reading for transcodeWriteTimeout.
func (s *Service) ServeTranscode(w http.ResponseWriter, r *http.Request, owner *core.Principal, fileID string, startSeconds float64, height int) error {
	ctx := r.Context()
	if math.IsNaN(startSeconds) || math.IsInf(startSeconds, 0) || startSeconds < 0 || startSeconds > maxStartSeconds {
		return core.BadRequest("start: must be a number of seconds >= 0")
	}
	if height == 0 {
		height = defaultHeight
	}
	if !transcodeHeights[height] {
		return core.BadRequest("height: must be 360, 480, 720 or 1080")
	}
	if _, err := s.itemFor(ctx, owner, fileID, needRead); err != nil {
		return err
	}
	ff := s.FFmpegPath(ctx)
	if ff == "" {
		return errFFmpegUnavailable
	}
	local, err := s.files.LocalPath(ctx, owner, fileID)
	if err != nil {
		return err
	}
	h := w.Header()
	h.Set("Content-Type", "video/mp4")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", streamCSP)
	h.Set("Content-Disposition", "inline")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return nil
	}
	// Charged to the caller, not to owner: tv programs transcode as the channel owner, and
	// one viewer of a shared channel must not be able to take every slot in its name.
	release, ok, err := s.transcodes.acquire(ctx, requester(r, owner), s.transcodeWait)
	if err != nil || !ok {
		h.Del("Content-Type")
		if err != nil {
			return err // the client went away while waiting
		}
		h.Set("Retry-After", "10")
		return errTranscodeBusy
	}
	defer release()
	tctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd, err := probe.TranscodeCommand(tctx, ff, local, startSeconds, height)
	if err != nil {
		return err
	}
	fw := &flushWriter{w: w, rc: http.NewResponseController(w), timeout: s.transcodeWriteTimeout, onFail: cancel}
	stderr := &capped{max: 4 << 10}
	cmd.Stdout, cmd.Stderr = fw, stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	err = cmd.Wait()
	if err != nil && ctx.Err() == nil && fw.written == 0 && !fw.failed {
		s.log.Warn("media: transcode failed", "id", fileID, "err", err, "stderr", stderr.String())
		h.Set("Content-Type", "application/json")
		return core.NewError(http.StatusInternalServerError, "internal", "Transcoding failed")
	}
	return nil
}

// capped keeps the first max bytes written to it.
type capped struct {
	bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.Len(); room > 0 {
		c.Buffer.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// ServeSubtitle writes subtitle track index as WebVTT.
func (s *Service) ServeSubtitle(w http.ResponseWriter, r *http.Request, owner *core.Principal, fileID string, index int) error {
	ctx := r.Context()
	it, err := s.itemFor(ctx, owner, fileID, needRead)
	if err != nil {
		return err
	}
	if index < 0 || index >= len(it.Subtitles) {
		return core.NotFound("Subtitle track not found")
	}
	sub := it.Subtitles[index]
	var (
		vtt  []byte
		etag string
	)
	switch {
	case sub.Format == "embedded" && sub.Stream != nil:
		vtt, etag, err = s.embeddedSubtitle(ctx, owner, it, *sub.Stream)
	case sub.FileID != "":
		vtt, etag, err = s.sidecarSubtitle(ctx, owner, sub)
	default:
		err = core.NotFound("Subtitle track not found")
	}
	if err != nil {
		return err
	}
	h := w.Header()
	h.Set("Content-Type", "text/vtt; charset=utf-8")
	h.Set("Cache-Control", "private, no-cache")
	h.Set("ETag", etag)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'; frame-ancestors 'none'")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(vtt))
	return nil
}

var errBadSubtitle = core.NotFound("Subtitle track could not be read")

func (s *Service) sidecarSubtitle(ctx context.Context, p *core.Principal, sub storedSub) ([]byte, string, error) {
	f, e, err := s.files.Open(ctx, p, sub.FileID)
	if err != nil {
		return nil, "", core.NotFound("Subtitle track not found")
	}
	defer f.Close()
	format := subtitles.FormatForExt(filepath.Ext(e.Name))
	if format == "" {
		return nil, "", errBadSubtitle
	}
	var out bytes.Buffer
	if err := subtitles.Convert(format, f, &out); err != nil {
		s.log.Debug("media: subtitle conversion failed", "id", sub.FileID, "err", err)
		return nil, "", errBadSubtitle
	}
	sum := sha256.Sum256([]byte(e.ID + "|" + strconv.FormatInt(e.Size, 10) + "|" + e.ModTime.String()))
	return out.Bytes(), `"s` + hex.EncodeToString(sum[:10]) + `"`, nil
}

// embeddedSubtitle extracts an embedded text track with ffmpeg (cached on disk).
func (s *Service) embeddedSubtitle(ctx context.Context, p *core.Principal, it *item, stream int) ([]byte, string, error) {
	ff := s.ffmpeg()
	if ff == "" {
		return nil, "", errFFmpegUnavailable
	}
	local, err := s.files.LocalPath(ctx, p, it.FileID)
	if err != nil {
		return nil, "", err
	}
	st, err := os.Stat(local)
	if err != nil {
		return nil, "", core.NotFound("File content is missing on disk")
	}
	sum := sha256.Sum256([]byte(it.FileID + "|" + strconv.Itoa(stream) + "|" + strconv.FormatInt(st.Size(), 10) + "|" + st.ModTime().UTC().String()))
	key := hex.EncodeToString(sum[:10])
	etag := `"e` + key + `"`
	root, err := s.mediaRoot()
	if err != nil {
		return nil, "", err
	}
	cacheRel := filepath.Join("subs", it.FileID+"-"+strconv.Itoa(stream)+"-"+key+".vtt")
	if b, err := readRootFile(root, cacheRel, subExtractLimit); err == nil {
		return b, etag, nil
	}
	select {
	case s.subSem <- struct{}{}:
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
	defer func() { <-s.subSem }()
	if b, err := readRootFile(root, cacheRel, subExtractLimit); err == nil {
		return b, etag, nil
	}
	xctx, cancel := context.WithTimeout(ctx, subExtractTimout)
	defer cancel()
	raw, err := probe.ExtractSubtitleVTT(xctx, ff, local, stream, subExtractLimit)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		s.log.Debug("media: subtitle extraction failed", "id", it.FileID, "stream", stream, "err", err)
		return nil, "", errBadSubtitle
	}
	var out bytes.Buffer
	if err := subtitles.NormalizeVTT(bytes.NewReader(raw), &out); err != nil {
		return nil, "", errBadSubtitle
	}
	tmp := cacheRel + ".tmp"
	if err := writeRootFile(root, tmp, out.Bytes()); err == nil {
		if err := root.Rename(tmp, cacheRel); err != nil {
			_ = root.Remove(tmp)
		}
	}
	return out.Bytes(), etag, nil
}

func readRootFile(root *os.Root, rel string, limit int64) ([]byte, error) {
	f, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("too large")
	}
	return b, nil
}

/* ----------------------------------------------------------------- progress */

// ProgressUpdate is PUT /media/{id}/progress.
type ProgressUpdate struct {
	PositionSeconds *float64 `json:"position_seconds"`
	Completed       *bool    `json:"completed"`
}

// completedFraction is the share of the duration after which an item counts as watched.
const completedFraction = 0.9

// setProgress records the caller's position. completed=true marks the item watched and
// resets the position; completed=false clears the watched flag; without it the item becomes
// watched once the position reaches 90% of the duration (and stays watched).
func (s *Service) setProgress(ctx context.Context, p *core.Principal, fileID string, u ProgressUpdate) error {
	if u.PositionSeconds == nil {
		return core.Validation(map[string]string{"position_seconds": "required"})
	}
	pos := *u.PositionSeconds
	if math.IsNaN(pos) || math.IsInf(pos, 0) || pos < 0 || pos > maxStartSeconds {
		return core.Validation(map[string]string{"position_seconds": "must be a number of seconds >= 0"})
	}
	it, err := s.itemFor(ctx, p, fileID, needRead)
	if err != nil {
		return err
	}
	completed := it.Completed
	switch {
	case u.Completed != nil && *u.Completed:
		completed, pos = true, 0
	case u.Completed != nil:
		completed = false
	case it.Duration.Valid && it.Duration.Float64 > 0 && pos >= completedFraction*it.Duration.Float64:
		completed = true
	}
	if it.Duration.Valid && pos > it.Duration.Float64 {
		pos = it.Duration.Float64
	}
	return s.saveProgress(ctx, s.d.DB.SQL, p.UserID, fileID, pos, completed)
}

func (s *Service) saveProgress(ctx context.Context, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, userID, fileID string, pos float64, completed bool) error {
	_, err := q.ExecContext(ctx, `INSERT INTO media_progress (user_id, file_id, position_seconds, completed, updated_at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT (user_id, file_id) DO UPDATE SET position_seconds = excluded.position_seconds,
		completed = excluded.completed, updated_at = excluded.updated_at`,
		userID, fileID, pos, db.Bool(completed), db.FormatTime(s.now()))
	if db.IsForeignKeyViolation(err) {
		return errNotMedia
	}
	return err
}

// setWatched sets the caller's watched flag (PATCH/bulk "watched"); marking watched resets
// the position, unmarking keeps it.
func (s *Service) setWatched(ctx context.Context, p *core.Principal, it *item, watched bool) error {
	pos := it.Position
	if watched {
		pos = 0
	}
	return s.saveProgress(ctx, s.d.DB.SQL, p.UserID, it.FileID, pos, watched)
}

func trimVTT(name string) (int, bool) {
	n, ok := strings.CutSuffix(name, ".vtt")
	if !ok || n == "" || len(n) > 6 {
		return 0, false
	}
	i, err := strconv.Atoi(n)
	if err != nil || i < 0 || strconv.Itoa(i) != n {
		return 0, false
	}
	return i, true
}
