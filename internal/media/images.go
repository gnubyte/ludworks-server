package media

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/imaging"
)

// Image limits.
const (
	maxPosterUpload  = 20 << 20 // PUT /poster body
	maxPosterEdge    = 1280     // stored poster (normalized JPEG)
	maxBackdropEdge  = 1920     // stored backdrop
	imageContentCSP  = "sandbox; default-src 'none'; img-src 'self' data:; frame-ancestors 'none'"
	imageCacheMaxAge = "private, no-cache"
)

// Size classes (longest edge in pixels).
var (
	posterSizes   = map[string]int{"sm": 160, "md": 320, "lg": 640}
	backdropSizes = map[string]int{"sm": 480, "md": 960, "lg": 1920}
)

// image kinds stored per item.
const (
	kindPoster   = "poster"
	kindBackdrop = "backdrop"
)

func imageDir(kind string) string {
	if kind == kindBackdrop {
		return "backdrops"
	}
	return "posters"
}

// normalizeImage decodes an uploaded/downloaded image (JPEG, PNG or GIF; pixel limits
// enforced before decoding) and re-encodes it as a JPEG of at most maxEdge pixels.
func normalizeImage(data []byte, maxEdge int) ([]byte, error) {
	if _, _, format, err := imaging.Size(bytes.NewReader(data)); err != nil {
		switch {
		case errors.Is(err, imaging.ErrTooLarge):
			return nil, core.NewError(http.StatusRequestEntityTooLarge, "payload_too_large", "Image dimensions are too large (max 50 MP)")
		case looksLikeWebP(data):
			return nil, core.NewError(http.StatusUnsupportedMediaType, "unsupported_media_type", "WebP images are not supported; use JPEG or PNG")
		default:
			return nil, core.NewError(http.StatusUnsupportedMediaType, "unsupported_media_type", "Unsupported image format; use JPEG or PNG")
		}
	} else if format == "" {
		return nil, core.ErrUnsupportedMedia
	}
	var out bytes.Buffer
	if err := imaging.Thumbnail(bytes.NewReader(data), maxEdge, &out); err != nil {
		if errors.Is(err, imaging.ErrTooLarge) {
			return nil, core.NewError(http.StatusRequestEntityTooLarge, "payload_too_large", "Image dimensions are too large (max 50 MP)")
		}
		return nil, core.NewError(http.StatusUnsupportedMediaType, "unsupported_media_type", "The image could not be decoded")
	}
	return out.Bytes(), nil
}

func looksLikeWebP(b []byte) bool {
	return len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP"
}

// storeImage writes a normalized JPEG for an item and records it (replacing any previous
// image of that kind, whose files are removed after the commit).
func (s *Service) storeImage(ctx context.Context, fileID, kind string, jpeg []byte) error {
	root, err := s.mediaRoot()
	if err != nil {
		return err
	}
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	rel := imageDir(kind) + "/" + fileID + "-" + hex.EncodeToString(rnd[:]) + ".jpg"
	tmp := rel + ".tmp"
	if err := writeRootFile(root, tmp, jpeg); err != nil {
		return err
	}
	if err := root.Rename(filepath.FromSlash(tmp), filepath.FromSlash(rel)); err != nil {
		_ = root.Remove(filepath.FromSlash(tmp))
		return err
	}
	col := "poster_path"
	if kind == kindBackdrop {
		col = "backdrop_path"
	}
	var old sql.NullString
	err = s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT `+col+` FROM media_items WHERE file_id = ?`, fileID).Scan(&old); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE media_items SET `+col+` = ?, updated_at = ? WHERE file_id = ?`,
			rel, db.FormatTime(s.now()), fileID)
		return err
	})
	if err != nil {
		_ = root.Remove(filepath.FromSlash(rel))
		if errors.Is(err, sql.ErrNoRows) {
			return errNotMedia
		}
		return err
	}
	if old.Valid {
		s.removeImageFiles(old.String)
	}
	s.publishUpdated(fileID)
	return nil
}

// clearImage removes an item's image of the given kind.
func (s *Service) clearImage(ctx context.Context, fileID, kind string) error {
	col := "poster_path"
	if kind == kindBackdrop {
		col = "backdrop_path"
	}
	var old sql.NullString
	err := s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT `+col+` FROM media_items WHERE file_id = ?`, fileID).Scan(&old); err != nil {
			return err
		}
		if !old.Valid {
			return nil
		}
		_, err := tx.ExecContext(ctx, `UPDATE media_items SET `+col+` = NULL, updated_at = ? WHERE file_id = ?`,
			db.FormatTime(s.now()), fileID)
		return err
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errNotMedia
		}
		return err
	}
	if old.Valid {
		s.removeImageFiles(old.String)
		s.publishUpdated(fileID)
	}
	return nil
}

// removeImageFiles deletes a stored image and its cached variants.
func (s *Service) removeImageFiles(rel string) {
	root, err := s.mediaRoot()
	if err != nil || !validStoredImage(rel) {
		return
	}
	_ = root.Remove(filepath.FromSlash(rel))
	stem := strings.TrimSuffix(filepath.Base(rel), ".jpg")
	for size := range backdropSizes {
		_ = root.Remove(filepath.Join("cache", stem+"-"+size+".jpg"))
	}
}

// validStoredImage checks a stored image path (defence in depth; paths are server-made).
func validStoredImage(rel string) bool {
	dir, name := filepath.Split(filepath.FromSlash(rel))
	dir = filepath.ToSlash(filepath.Clean(dir))
	if dir != "posters" && dir != "backdrops" {
		return false
	}
	if !strings.HasSuffix(name, ".jpg") || len(name) > 128 {
		return false
	}
	for _, r := range strings.TrimSuffix(name, ".jpg") {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

func writeRootFile(root *os.Root, rel string, data []byte) error {
	f, err := root.OpenFile(filepath.FromSlash(rel), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		_ = root.Remove(filepath.FromSlash(rel))
		return err
	}
	return nil
}

// imageVariant returns the cached JPEG of a stored image at a size class (generated on
// demand) and its ETag.
func (s *Service) imageVariant(ctx context.Context, rel, kind, size string) (*os.File, string, error) {
	if !validStoredImage(rel) {
		return nil, "", errNoImage
	}
	sizes := posterSizes
	if kind == kindBackdrop {
		sizes = backdropSizes
	}
	edge := sizes[size]
	root, err := s.mediaRoot()
	if err != nil {
		return nil, "", err
	}
	stem := strings.TrimSuffix(filepath.Base(rel), ".jpg")
	cacheRel := filepath.Join("cache", stem+"-"+size+".jpg")
	sum := sha256.Sum256([]byte(rel + "|" + size))
	etag := `"i` + hex.EncodeToString(sum[:10]) + `"`
	if f, err := root.Open(cacheRel); err == nil {
		return f, etag, nil
	}
	select {
	case s.imgSem <- struct{}{}:
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
	defer func() { <-s.imgSem }()
	if f, err := root.Open(cacheRel); err == nil {
		return f, etag, nil
	}
	src, err := root.Open(filepath.FromSlash(rel))
	if err != nil {
		return nil, "", errNoImage
	}
	var buf bytes.Buffer
	err = imaging.Thumbnail(io.LimitReader(src, maxPosterUpload), edge, &buf)
	src.Close()
	if err != nil {
		return nil, "", errNoImage
	}
	tmp := cacheRel + "." + hex.EncodeToString(sum[10:16]) + ".tmp"
	if err := writeRootFile(root, tmp, buf.Bytes()); err != nil {
		return nil, "", err
	}
	if err := root.Rename(tmp, cacheRel); err != nil {
		_ = root.Remove(tmp)
		return nil, "", err
	}
	f, err := root.Open(cacheRel)
	if err != nil {
		return nil, "", err
	}
	return f, etag, nil
}

var errNoImage = core.NotFound("No image available")

// serveJPEG writes an image file with caching headers.
func serveJPEG(w http.ResponseWriter, r *http.Request, f *os.File, etag string) error {
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	h := w.Header()
	h.Set("Content-Type", "image/jpeg")
	h.Set("ETag", etag)
	h.Set("Cache-Control", imageCacheMaxAge)
	h.Set("Content-Security-Policy", imageContentCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "", st.ModTime(), f)
	return nil
}

// ServePoster writes the custom poster, else the generated thumbnail, at size sm|md|lg.
func (s *Service) ServePoster(w http.ResponseWriter, r *http.Request, owner *core.Principal, fileID, size string) error {
	if size == "" {
		size = "md"
	}
	if _, ok := posterSizes[size]; !ok {
		return core.Validation(map[string]string{"size": "must be sm, md or lg"})
	}
	ctx := r.Context()
	it, err := s.itemFor(ctx, owner, fileID, needRead)
	if err != nil {
		return err
	}
	if it.PosterPath.Valid {
		if f, etag, err := s.imageVariant(ctx, it.PosterPath.String, kindPoster, size); err == nil {
			return serveJPEG(w, r, f, etag)
		}
	}
	abs, etag, err := s.files.ThumbnailFile(ctx, owner, fileID, size)
	if err != nil {
		return err
	}
	f, err := os.Open(abs)
	if err != nil {
		return errNoImage
	}
	return serveJPEG(w, r, f, etag)
}

// serveBackdrop writes the item's backdrop (404 when none).
func (s *Service) serveBackdrop(w http.ResponseWriter, r *http.Request, p *core.Principal, fileID, size string) error {
	if size == "" {
		size = "lg"
	}
	if _, ok := backdropSizes[size]; !ok {
		return core.Validation(map[string]string{"size": "must be sm, md or lg"})
	}
	it, err := s.itemFor(r.Context(), p, fileID, needRead)
	if err != nil {
		return err
	}
	if !it.BackdropPath.Valid {
		return errNoImage
	}
	f, etag, err := s.imageVariant(r.Context(), it.BackdropPath.String, kindBackdrop, size)
	if err != nil {
		return err
	}
	return serveJPEG(w, r, f, etag)
}

// putPoster stores an uploaded poster for p.
func (s *Service) putPoster(ctx context.Context, p *core.Principal, fileID string, body io.Reader) (Media, error) {
	if _, err := s.itemFor(ctx, p, fileID, needWrite); err != nil {
		return Media{}, err
	}
	data, err := io.ReadAll(io.LimitReader(body, maxPosterUpload+1))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return Media{}, core.ErrPayloadTooLarge
		}
		return Media{}, err
	}
	if len(data) > maxPosterUpload {
		return Media{}, core.ErrPayloadTooLarge
	}
	if len(data) == 0 {
		return Media{}, core.BadRequest("request body required")
	}
	select {
	case s.imgSem <- struct{}{}:
	case <-ctx.Done():
		return Media{}, ctx.Err()
	}
	jpeg, err := normalizeImage(data, maxPosterEdge)
	<-s.imgSem
	if err != nil {
		return Media{}, err
	}
	if err := s.storeImage(ctx, fileID, kindPoster, jpeg); err != nil {
		return Media{}, err
	}
	return s.mediaJSON(ctx, p, fileID)
}

// gc removes image files no row references and stale subtitle extractions.
func (s *Service) gc(ctx context.Context) error {
	root, err := s.mediaRoot()
	if err != nil {
		return err
	}
	used := map[string]bool{}
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT poster_path FROM media_items WHERE poster_path IS NOT NULL
		UNION ALL SELECT backdrop_path FROM media_items WHERE backdrop_path IS NOT NULL`)
	if err != nil {
		return err
	}
	for rs.Next() {
		var p string
		if err := rs.Scan(&p); err != nil {
			rs.Close()
			return err
		}
		used[p] = true
		used["stem:"+strings.TrimSuffix(filepath.Base(p), ".jpg")] = true
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return err
	}
	cutoff := time.Now().Add(-time.Hour)
	old := func(d fs.DirEntry) bool {
		info, err := d.Info()
		return err == nil && info.ModTime().Before(cutoff)
	}
	for _, dir := range []string{"posters", "backdrops"} {
		entries, err := fs.ReadDir(root.FS(), dir)
		if err != nil {
			continue
		}
		for _, d := range entries {
			if d.Type().IsRegular() && !used[dir+"/"+d.Name()] && old(d) {
				_ = root.Remove(filepath.Join(dir, d.Name()))
			}
		}
	}
	if entries, err := fs.ReadDir(root.FS(), "cache"); err == nil {
		for _, d := range entries {
			stem := d.Name()
			if i := strings.LastIndexByte(stem, '-'); i > 0 {
				stem = stem[:i]
			}
			if d.Type().IsRegular() && !used["stem:"+stem] && old(d) {
				_ = root.Remove(filepath.Join("cache", d.Name()))
			}
		}
	}
	subCutoff := time.Now().Add(-30 * 24 * time.Hour)
	if entries, err := fs.ReadDir(root.FS(), "subs"); err == nil {
		for _, d := range entries {
			if info, err := d.Info(); err == nil && d.Type().IsRegular() && info.ModTime().Before(subCutoff) {
				_ = root.Remove(filepath.Join("subs", d.Name()))
			}
		}
	}
	return ctx.Err()
}
