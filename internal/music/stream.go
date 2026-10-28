package music

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"ludworks/internal/core"
)

// Playback: the file itself (stream, original download), live ffmpeg transcodes and the cached
// download renditions (renditions.go).

const (
	maxStartSeconds  = 1e7
	defaultBitrate   = 160
	minBitrate       = 64
	maxBitrate       = 320
	streamCSP        = "sandbox; default-src 'none'; media-src 'self'; frame-ancestors 'none'"
	defaultTranscode = "aac"
)

// parseTranscode validates format, bitrate (kbps, 0 = default) and start of a transcode.
func parseTranscode(format string, bitrate int, start float64) (string, int, error) {
	if format == "" {
		format = defaultTranscode
	}
	if _, ok := transcodeFormats[format]; !ok {
		return "", 0, core.BadRequest("format: must be aac, mp3 or opus")
	}
	if bitrate == 0 {
		bitrate = defaultBitrate
	}
	if bitrate < minBitrate || bitrate > maxBitrate {
		return "", 0, core.BadRequest("bitrate: must be between 64 and 320 kbps")
	}
	if math.IsNaN(start) || math.IsInf(start, 0) || start < 0 || start > maxStartSeconds {
		return "", 0, core.BadRequest("start: must be a number of seconds >= 0")
	}
	return format, bitrate, nil
}

// transcodeParams reads ?format=&bitrate=&start=.
func transcodeParams(r *http.Request) (format string, bitrate int, start float64, err error) {
	q := r.URL.Query()
	format = q.Get("format")
	if v := q.Get("bitrate"); v != "" {
		if bitrate, err = strconv.Atoi(v); err != nil {
			return "", 0, 0, core.BadRequest("bitrate: must be between 64 and 320 kbps")
		}
	}
	if v := q.Get("start"); v != "" {
		if start, err = strconv.ParseFloat(v, 64); err != nil {
			return "", 0, 0, core.BadRequest("start: must be a number of seconds >= 0")
		}
	}
	format, bitrate, err = parseTranscode(format, bitrate, start)
	return format, bitrate, start, err
}

// flushWriter flushes after every write so ffmpeg output reaches the client promptly. Every
// write gets a fresh deadline (the server has no WriteTimeout): a client that stops reading
// fails the write, which cancels ffmpeg (onFail) and frees the transcode slot.
type flushWriter struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	timeout time.Duration
	onFail  func()
	written int64
	failed  bool
}

var errClientStalled = errors.New("music: transcode client stopped reading")

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

// serveTranscode streams a live ffmpeg transcode of tr. The slot is charged to the requesting
// user (requester); ffmpeg is killed when the client goes away or stops reading.
func (s *Service) serveTranscode(w http.ResponseWriter, r *http.Request, p *core.Principal, tr *track, format string, bitrate int, start float64) error {
	ctx := r.Context()
	ff := s.ffmpegPath(ctx)
	if ff == "" {
		return errFFmpegUnavailable
	}
	local, err := s.files.LocalPath(ctx, p, tr.FileID)
	if err != nil {
		return err
	}
	h := w.Header()
	h.Set("Content-Type", transcodeFormats[format].contentType)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", streamCSP)
	h.Set("Content-Disposition", "inline")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return nil
	}
	release, ok, err := s.transcodes.acquire(ctx, requester(r, p), s.transcodeWait)
	if err != nil || !ok {
		h.Del("Content-Type")
		h.Del("Content-Disposition")
		if err != nil {
			return err // the client went away while waiting
		}
		h.Set("Retry-After", "10")
		return errTranscodeBusy
	}
	defer release()
	tctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd, err := transcodeCommand(tctx, ff, local, format, bitrate, start)
	if err != nil {
		return err
	}
	fw := &flushWriter{w: w, rc: http.NewResponseController(w), timeout: s.transcodeWriteTimeout, onFail: cancel}
	stderr := &capped{max: maxStderr}
	cmd.Stdout, cmd.Stderr = fw, stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	err = cmd.Wait()
	if err != nil && ctx.Err() == nil && fw.written == 0 && !fw.failed {
		s.log.Warn("music: transcode failed", "id", tr.FileID, "err", err, "stderr", stderr.String())
		h.Set("Content-Type", "application/json")
		h.Del("Content-Disposition")
		return core.NewError(http.StatusInternalServerError, "internal", "Transcoding failed")
	}
	return nil
}

// downloadName is the attachment name of a download: the file name, with .m4a for renditions.
func downloadName(name, quality string) string {
	if quality == "original" {
		return name
	}
	return strings.TrimSuffix(name, path.Ext(name)) + ".m4a"
}

func contentDisposition(kind, name string) string {
	if v := mime.FormatMediaType(kind, map[string]string{"filename": name}); v != "" {
		return v
	}
	return kind
}

// renditionETag ties a download to the source version and the quality.
func renditionETag(fileID string, size int64, modTime time.Time, quality string) string {
	sum := sha256.Sum256([]byte(fileID + "|" + strconv.FormatInt(size, 10) + "|" + strconv.FormatInt(modTime.UnixNano(), 10) + "|" + quality))
	return `"d` + hex.EncodeToString(sum[:16]) + `"`
}

// servedType is the content type a track's own bytes are served with: its audio type
// (Ogg sniffed as application/ogg is audio/ogg), never anything a browser could execute.
func servedType(mime string) string {
	switch {
	case mime == "application/ogg":
		return "audio/ogg"
	case strings.HasPrefix(mime, "audio/") && !strings.ContainsAny(mime, ";, "):
		return mime
	}
	return "application/octet-stream"
}

// serveFile writes the track's own bytes with Range/If-Range/ETag: inline for streams, as an
// attachment for downloads (quality "original", or a rendition served as is).
func (s *Service) serveFile(w http.ResponseWriter, r *http.Request, p *core.Principal, tr *track, inline bool, quality string) error {
	f, e, err := s.files.Open(r.Context(), p, tr.FileID)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	ct := servedType(e.Mime)
	h := w.Header()
	switch {
	case inline:
		h.Set("Content-Disposition", contentDisposition("inline", e.Name))
		quality = "stream"
	case quality != "original":
		ct = "audio/mp4"
		h.Set("Content-Disposition", contentDisposition("attachment", downloadName(e.Name, quality)))
	default:
		h.Set("Content-Disposition", contentDisposition("attachment", downloadName(e.Name, quality)))
	}
	h.Set("Content-Type", ct)
	h.Set("ETag", renditionETag(e.ID, st.Size(), st.ModTime(), quality))
	h.Set("Cache-Control", "private, no-cache")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", streamCSP)
	h.Set("Accept-Ranges", "bytes")
	http.ServeContent(w, r, "", st.ModTime(), f)
	return nil
}
