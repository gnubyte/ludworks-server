package files

import (
	"archive/zip"
	"bufio"
	"bytes"
	"compress/flate"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/httpx"
	"ludworks/internal/imaging"
	"ludworks/internal/media/probe"
)

// contentCSP is sent with every user-file response: the content can never run script or
// navigate as our origin even if a browser decides to render it.
const contentCSP = "sandbox; default-src 'none'; img-src 'self' data:; media-src 'self'; style-src 'unsafe-inline'; frame-ancestors 'none'"

// pdfCSP lets the built-in PDF viewer render a PDF framed by the web UI preview.
const pdfCSP = "sandbox allow-scripts; default-src 'none'; img-src 'self' data: blob:; style-src 'unsafe-inline'; object-src 'self'; frame-ancestors 'self'"

// strongETag derives a strong validator from id, size and modification time.
func strongETag(id string, size int64, mod time.Time) string {
	h := sha256.Sum256([]byte(id + "|" + strconv.FormatInt(size, 10) + "|" + strconv.FormatInt(mod.UnixNano(), 10)))
	return `"` + hex.EncodeToString(h[:16]) + `"`
}

// contentDisposition builds an RFC 6266 header with an RFC 5987 UTF-8 filename.
func contentDisposition(kind, name string) string {
	if v := mime.FormatMediaType(kind, map[string]string{"filename": name}); v != "" {
		return v
	}
	return kind
}

// ServeContent writes a file's bytes with Range/If-Range/ETag support. Unless download is
// set, safe types (images, video, audio, PDF, text/plain) are inline; everything else is an
// attachment. Every response carries a sandboxing CSP and nosniff.
func (s *Service) ServeContent(w http.ResponseWriter, r *http.Request, p *core.Principal, id string, download bool) error {
	ctx := r.Context()
	row, sp, err := s.entryFor(ctx, s.d.DB.SQL, p, id, accRead)
	if err != nil {
		return err
	}
	if row.IsDir {
		return core.BadRequest("Folders are downloaded as ZIP (GET /files/{id}/zip)")
	}
	f, err := s.openFile(sp, row.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	inline := !download && inlineSafe(row.Mime)
	h := w.Header()
	h.Set("Content-Type", servedContentType(row.Mime, inline))
	if inline {
		h.Set("Content-Disposition", contentDisposition("inline", row.Name))
	} else {
		h.Set("Content-Disposition", contentDisposition("attachment", row.Name))
	}
	h.Set("ETag", strongETag(row.ID, st.Size(), st.ModTime()))
	h.Set("Cache-Control", "private, no-cache")
	h.Set("X-Content-Type-Options", "nosniff")
	if inline && row.Mime == "application/pdf" {
		h.Set("Content-Security-Policy", pdfCSP)
		h.Set("X-Frame-Options", "SAMEORIGIN")
	} else {
		h.Set("Content-Security-Policy", contentCSP)
	}
	h.Set("Accept-Ranges", "bytes")
	http.ServeContent(w, r, "", st.ModTime(), f)
	return nil
}

func (m *Module) content(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	return m.svc.ServeContent(w, r, core.MustPrincipal(r.Context()), id, httpx.QueryBool(r, "download"))
}

/* -------------------------------------------------------------- thumbnails */

const (
	thumbUnknown = 0
	thumbOK      = 1
	thumbFailed  = 2

	maxThumbInput = imaging.MaxInputBytes
)

var thumbSizes = map[string]int{"sm": 160, "md": 320, "lg": 640}

// toolCache holds the ffmpeg detection result; it is refreshed in the background so
// requests never wait for `ffmpeg -version`.
type toolCache struct {
	mu        sync.RWMutex
	ffmpeg    string
	autoThumb bool
	checked   bool
}

type mediaSettings struct {
	FFmpegPath     string `json:"ffmpeg_path"`
	FFprobePath    string `json:"ffprobe_path"`
	AutoThumbnails *bool  `json:"auto_thumbnails"`
}

// refreshTools re-detects ffmpeg from the media settings (or PATH).
func (s *Service) refreshTools(ctx context.Context) {
	var ms mediaSettings
	if err := s.d.Settings.Get(ctx, "media", &ms); err != nil {
		ms = mediaSettings{}
	}
	ff, _, _ := probe.DetectTools(ms.FFmpegPath, ms.FFprobePath)
	auto := ms.AutoThumbnails == nil || *ms.AutoThumbnails
	s.tools.mu.Lock()
	s.tools.ffmpeg, s.tools.autoThumb, s.tools.checked = ff, auto, true
	s.tools.mu.Unlock()
}

func (s *Service) ffmpegPath() string {
	s.tools.mu.RLock()
	defer s.tools.mu.RUnlock()
	return s.tools.ffmpeg
}

// videoThumbsEnabled reports whether video thumbnails can be generated right now.
func (s *Service) videoThumbsEnabled(context.Context) bool {
	s.tools.mu.RLock()
	defer s.tools.mu.RUnlock()
	return s.tools.ffmpeg != "" && s.tools.autoThumb
}

var errNoThumb = core.NotFound("No thumbnail available")

// ThumbnailFile returns the absolute path of a cached JPEG thumbnail (generating it if
// needed) and its ETag. size is sm, md or lg. Files without a thumbnail are 404.
func (s *Service) ThumbnailFile(ctx context.Context, p *core.Principal, id, size string) (string, string, error) {
	edge, ok := thumbSizes[size]
	if !ok {
		return "", "", core.Validation(map[string]string{"size": "must be sm, md or lg"})
	}
	row, sp, err := s.entryFor(ctx, s.d.DB.SQL, p, id, accRead)
	if err != nil {
		return "", "", err
	}
	if !s.caps(ctx).has(&row.Entry, row.ThumbState) {
		return "", "", errNoThumb
	}
	f, err := s.openFile(sp, row.Path)
	if err != nil {
		return "", "", errNoThumb
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", "", err
	}
	key := strings.Trim(strongETag(row.ID, st.Size(), st.ModTime()), `"`)[:16]
	name := fmt.Sprintf("%s-%s-%s.jpg", row.ID, size, key)
	abs := filepath.Join(s.d.Config.ThumbsDir(), name)
	etag := `"t` + key + size + `"`
	if fi, err := os.Stat(abs); err == nil && fi.Size() > 0 {
		return abs, etag, nil
	}

	select {
	case s.thumbSem <- struct{}{}:
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
	defer func() { <-s.thumbSem }()
	if fi, err := os.Stat(abs); err == nil && fi.Size() > 0 {
		return abs, etag, nil // generated by a concurrent request
	}
	var buf bytes.Buffer
	switch {
	case thumbImage(row.Mime):
		err = imaging.ThumbnailContext(ctx, f, edge, &buf) // shares the process-wide decode budget
	case thumbVideo(row.Mime):
		err = s.videoThumb(ctx, sp, row, f, st.Size(), edge, &buf)
	default:
		err = errNoThumb
	}
	if err != nil {
		if ctx.Err() == nil && !errors.Is(err, context.DeadlineExceeded) {
			s.log.Debug("thumbnail failed", "id", row.ID, "err", err)
			s.setThumbState(row.ID, thumbFailed)
		}
		return "", "", errNoThumb
	}
	if err := os.MkdirAll(s.d.Config.ThumbsDir(), 0o700); err != nil {
		return "", "", err
	}
	tmp, err := os.CreateTemp(s.d.Config.ThumbsDir(), ".thumb-*")
	if err != nil {
		return "", "", err
	}
	_, werr := tmp.Write(buf.Bytes())
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		return "", "", errors.Join(werr, cerr)
	}
	if err := os.Rename(tmp.Name(), abs); err != nil {
		os.Remove(tmp.Name())
		return "", "", err
	}
	if row.ThumbState != thumbOK {
		s.setThumbState(row.ID, thumbOK)
	}
	return abs, etag, nil
}

func (s *Service) videoThumb(ctx context.Context, sp *space, row *row, f *os.File, size int64, edge int, w io.Writer) error {
	ff := s.ffmpegPath()
	if ff == "" {
		return errNoThumb
	}
	at := 0.0
	if info, err := probe.Probe(f, size, row.Name); err == nil && info.DurationSeconds > 0 {
		at = info.DurationSeconds * 0.1
	}
	local := filepath.Join(s.storageDir(), filepath.FromSlash(sp.RootDir), filepath.FromSlash(row.Path))
	// ffmpeg opens the path itself, without the space root's confinement: only hand it over
	// when it names the very file opened through the space root (a folder swapped for a
	// symlink on disk could otherwise point it at another space).
	fst, err := f.Stat()
	if err != nil {
		return err
	}
	if lst, err := os.Stat(local); err != nil || !os.SameFile(fst, lst) {
		return errNoThumb
	}
	tctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	jpg, err := probe.VideoThumbnail(tctx, ff, local, at, edge)
	if err != nil {
		return err
	}
	_, err = w.Write(jpg)
	return err
}

func (s *Service) setThumbState(id string, state int) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = s.d.DB.SQL.ExecContext(ctx, `UPDATE files SET thumb_state = ? WHERE id = ?`, state, id)
}

// ServeThumbnail writes a JPEG thumbnail (size sm|md|lg, default md).
func (s *Service) ServeThumbnail(w http.ResponseWriter, r *http.Request, p *core.Principal, id, size string) error {
	if size == "" {
		size = "md"
	}
	abs, etag, err := s.ThumbnailFile(r.Context(), p, id, size)
	if err != nil {
		return err
	}
	f, err := os.Open(abs)
	if err != nil {
		return errNoThumb
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	h := w.Header()
	h.Set("Content-Type", "image/jpeg")
	h.Set("ETag", etag)
	h.Set("Cache-Control", "private, max-age=3600")
	h.Set("Content-Security-Policy", contentCSP)
	http.ServeContent(w, r, "", st.ModTime(), f)
	return nil
}

func (m *Module) thumbnail(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	return m.svc.ServeThumbnail(w, r, core.MustPrincipal(r.Context()), id, r.URL.Query().Get("size"))
}

/* --------------------------------------------------------------------- zip */

func (m *Module) zipFolder(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	dir, sp, err := m.svc.entryFor(ctx, m.d.DB.SQL, p, id, accRead)
	if err != nil {
		return err
	}
	if !dir.IsDir {
		return core.BadRequest("Only folders can be downloaded as ZIP; use /content for files")
	}
	cond, args := subtreeCond(dir.Path)
	var n int
	if err := m.d.DB.SQL.QueryRowContext(ctx, `SELECT count(*) FROM files f WHERE f.space_id = ? AND f.trashed_at IS NULL AND `+cond,
		append([]any{sp.ID}, args...)...).Scan(&n); err != nil {
		return err
	}
	if n > maxTreeEntries {
		return core.NewError(413, "payload_too_large", fmt.Sprintf("The folder has %d entries; ZIP downloads are limited to %d", n, maxTreeEntries))
	}
	base := dir.Name
	if dir.Path == "" {
		base = sp.Name
	}
	sr, err := m.svc.spaceRoot(sp)
	if err != nil {
		return err
	}
	defer sr.Close()

	h := w.Header()
	h.Set("Content-Type", "application/zip")
	h.Set("Content-Disposition", contentDisposition("attachment", base+".zip"))
	h.Set("Content-Security-Policy", contentCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)

	bw := bufio.NewWriterSize(w, 256<<10)
	zw := zip.NewWriter(bw)
	zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(out, flate.BestSpeed)
	})
	if _, err := zw.CreateHeader(&zip.FileHeader{Name: base + "/", Method: zip.Store, Modified: dir.ModTime}); err != nil {
		return err
	}
	buf := make([]byte, copyBuf)
	last := ""
	for {
		rows, err := queryRows(ctx, m.d.DB.SQL, `SELECT `+entryCols+` FROM files f WHERE f.space_id = ? AND f.trashed_at IS NULL AND `+cond+
			` AND f.path > ? ORDER BY f.path LIMIT 2000`, append(append([]any{sp.ID}, args...), last)...)
		if err != nil {
			return err
		}
		for _, x := range rows {
			last = x.Path
			rel := x.Path
			if dir.Path != "" {
				rel = strings.TrimPrefix(x.Path, dir.Path+"/")
			}
			name := base + "/" + rel
			if x.IsDir {
				if _, err := zw.CreateHeader(&zip.FileHeader{Name: name + "/", Method: zip.Store, Modified: x.ModTime}); err != nil {
					return err
				}
				continue
			}
			if err := zipFile(ctx, zw, sr, x, name, buf); err != nil {
				return err
			}
		}
		if len(rows) < 2000 {
			break
		}
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return bw.Flush()
}

func zipFile(ctx context.Context, zw *zip.Writer, sr *os.Root, x *row, name string, buf []byte) error {
	f, err := sr.Open(osRel(x.Path))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // vanished from disk: skip
		}
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return err
	}
	method := zip.Deflate
	if alreadyCompressed(x.Name) || st.Size() == 0 {
		method = zip.Store
	}
	fh := &zip.FileHeader{Name: name, Method: method, Modified: st.ModTime()}
	fh.UncompressedSize64 = uint64(st.Size())
	zf, err := zw.CreateHeader(fh)
	if err != nil {
		return err
	}
	_, err = io.CopyBuffer(zf, ctxReader{ctx, io.LimitReader(f, st.Size())}, buf)
	return err
}

// thumbsGC removes cached thumbnails of deleted files and superseded variants.
func (s *Service) thumbsGC(ctx context.Context) error {
	dir := s.d.Config.ThumbsDir()
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	latest := map[string]os.DirEntry{} // id-size → newest file
	for _, e := range ents {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := e.Name()
		if strings.HasPrefix(name, ".thumb-") {
			if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > time.Hour {
				os.Remove(filepath.Join(dir, name))
			}
			continue
		}
		parts := strings.SplitN(strings.TrimSuffix(name, ".jpg"), "-", 3)
		if len(parts) != 3 {
			continue
		}
		var exists int
		if err := s.d.DB.SQL.QueryRowContext(ctx, `SELECT count(*) FROM files WHERE id = ?`, parts[0]).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			os.Remove(filepath.Join(dir, name))
			continue
		}
		k := parts[0] + "-" + parts[1]
		if prev, ok := latest[k]; ok {
			pi, _ := prev.Info()
			ci, _ := e.Info()
			if pi != nil && ci != nil && ci.ModTime().After(pi.ModTime()) {
				os.Remove(filepath.Join(dir, prev.Name()))
				latest[k] = e
			} else {
				os.Remove(filepath.Join(dir, name))
			}
			continue
		}
		latest[k] = e
	}
	return nil
}
