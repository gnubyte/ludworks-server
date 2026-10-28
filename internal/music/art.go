package music

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/draw"
	"image/jpeg"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/imaging"
)

// Cover art. Every distinct picture (embedded, folder image or upload) is decoded once under
// the shared imaging budget, re-encoded as a JPEG of at most artMaxEdge pixels and stored as
// art/<id>.jpg, where id is a hash of the source bytes; size variants are generated on demand.

const (
	artMaxEdge     = 1200     // stored art (the xl size)
	maxArtUpload   = 20 << 20 // PUT …/art body
	maxFolderImage = 50 << 20 // folder images larger than this are ignored
	artContentCSP  = "sandbox; default-src 'none'; img-src 'self' data:; frame-ancestors 'none'"
	artCache       = "private, no-cache"
)

// artSizes are the size classes (longest edge in pixels).
var artSizes = map[string]int{"sm": 160, "md": 320, "lg": 640, "xl": artMaxEdge}

var errNoArt = core.NotFound("No cover art available")

// validArtID checks a stored art id (32 lowercase hex characters; server-made, checked as
// defence in depth before it becomes a file name).
func validArtID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, r := range id {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func artRel(id string) string { return filepath.Join("art", id+".jpg") }

func parseArtSize(size string) (string, int, error) {
	if size == "" {
		size = "md"
	}
	edge, ok := artSizes[size]
	if !ok {
		return "", 0, core.Validation(map[string]string{"size": "must be sm, md, lg or xl"})
	}
	return size, edge, nil
}

// storeArt stores a picture (JPEG, PNG or GIF; pixel limits enforced before decoding) and
// returns its art id. A picture stored before is not decoded again.
func (s *Service) storeArt(ctx context.Context, data []byte) (string, error) {
	sum := sha256.Sum256(data)
	id := hex.EncodeToString(sum[:16])
	root, err := s.musicRoot()
	if err != nil {
		return "", err
	}
	if st, err := root.Stat(artRel(id)); err == nil && st.Mode().IsRegular() {
		now := time.Now()
		_ = root.Chtimes(artRel(id), now, now) // keeps it clear of the GC grace period
		return id, nil
	}
	select {
	case s.imgSem <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-s.imgSem }()
	var out bytes.Buffer
	if err := imaging.ThumbnailContext(ctx, bytes.NewReader(data), artMaxEdge, &out); err != nil {
		return "", err
	}
	if err := writeAtomic(root, artRel(id), out.Bytes()); err != nil {
		return "", err
	}
	return id, nil
}

// writeAtomic writes data to rel through a temporary file and a rename.
func writeAtomic(root *os.Root, rel string, data []byte) error {
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	tmp := rel + "." + hex.EncodeToString(rnd[:]) + ".tmp"
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	if err := root.Rename(tmp, rel); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	return nil
}

// normalizeUpload validates an uploaded image (like media posters: JPEG/PNG/GIF ≤ 20 MiB,
// ≤ 50 MP; WebP 415) and stores it as art.
func (s *Service) normalizeUpload(ctx context.Context, body io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxArtUpload+1))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return "", core.ErrPayloadTooLarge
		}
		return "", err
	}
	if len(data) > maxArtUpload {
		return "", core.ErrPayloadTooLarge
	}
	if len(data) == 0 {
		return "", core.BadRequest("request body required")
	}
	if _, _, format, err := imaging.Size(bytes.NewReader(data)); err != nil {
		switch {
		case errors.Is(err, imaging.ErrTooLarge):
			return "", core.NewError(http.StatusRequestEntityTooLarge, "payload_too_large", "Image dimensions are too large (max 50 MP)")
		case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
			return "", core.NewError(http.StatusUnsupportedMediaType, "unsupported_media_type", "WebP images are not supported; use JPEG or PNG")
		default:
			return "", core.NewError(http.StatusUnsupportedMediaType, "unsupported_media_type", "Unsupported image format; use JPEG or PNG")
		}
	} else if format == "" {
		return "", core.ErrUnsupportedMedia
	}
	id, err := s.storeArt(ctx, data)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if errors.Is(err, imaging.ErrTooLarge) {
			return "", core.NewError(http.StatusRequestEntityTooLarge, "payload_too_large", "Image dimensions are too large (max 50 MP)")
		}
		return "", core.NewError(http.StatusUnsupportedMediaType, "unsupported_media_type", "The image could not be decoded")
	}
	return id, nil
}

// folderImageArt reads a folder image file and stores it as art.
func (s *Service) folderImageArt(ctx context.Context, fileID string) (string, error) {
	f, e, err := s.files.Open(ctx, s.sys, fileID)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if e.Size > maxFolderImage {
		return "", errors.New("folder image too large")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFolderImage+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxFolderImage {
		return "", errors.New("folder image too large")
	}
	actx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	return s.storeArt(actx, data)
}

// artVariant opens the JPEG of an art id at a size class (generating it if needed) and
// returns its ETag.
func (s *Service) artVariant(ctx context.Context, id, size string) (*os.File, string, error) {
	if !validArtID(id) {
		return nil, "", errNoArt
	}
	size, edge, err := parseArtSize(size)
	if err != nil {
		return nil, "", err
	}
	root, err := s.musicRoot()
	if err != nil {
		return nil, "", err
	}
	etag := `"a` + id[:20] + "-" + size + `"`
	if edge == artMaxEdge {
		f, err := root.Open(artRel(id))
		if err != nil {
			return nil, "", errNoArt
		}
		return f, etag, nil
	}
	cacheRel := filepath.Join("art", "cache", id+"-"+size+".jpg")
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
	src, err := root.Open(artRel(id))
	if err != nil {
		return nil, "", errNoArt
	}
	var buf bytes.Buffer
	err = imaging.ThumbnailContext(ctx, io.LimitReader(src, maxArtUpload), edge, &buf)
	src.Close()
	if err != nil {
		return nil, "", errNoArt
	}
	if err := writeAtomic(root, cacheRel, buf.Bytes()); err != nil {
		return nil, "", err
	}
	f, err := root.Open(cacheRel)
	if err != nil {
		return nil, "", err
	}
	return f, etag, nil
}

// serveJPEG writes an image file with caching and sandboxing headers.
func serveJPEG(w http.ResponseWriter, r *http.Request, f *os.File, etag string) error {
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	h := w.Header()
	h.Set("Content-Type", "image/jpeg")
	h.Set("ETag", etag)
	h.Set("Cache-Control", artCache)
	h.Set("Content-Security-Policy", artContentCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "", st.ModTime(), f)
	return nil
}

// serveArt writes an art id at a size class.
func (s *Service) serveArt(w http.ResponseWriter, r *http.Request, artID, size string) error {
	if artID == "" {
		if _, _, err := parseArtSize(size); err != nil {
			return err
		}
		return errNoArt
	}
	f, etag, err := s.artVariant(r.Context(), artID, size)
	if err != nil {
		return err
	}
	return serveJPEG(w, r, f, etag)
}

// readArt returns the JPEG bytes of an art id at a size class.
func (s *Service) readArt(ctx context.Context, artID, size string) ([]byte, error) {
	f, _, err := s.artVariant(ctx, artID, size)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxArtUpload))
}

/* ------------------------------------------------------------------ mosaics */

// mosaic returns a 2×2 mosaic of four art ids at a size class (cached on disk by its inputs).
func (s *Service) mosaic(ctx context.Context, ids [4]string, size string) (*os.File, string, error) {
	size, edge, err := parseArtSize(size)
	if err != nil {
		return nil, "", err
	}
	for _, id := range ids {
		if !validArtID(id) {
			return nil, "", errNoArt
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(ids[:], ",") + "|" + size))
	key := hex.EncodeToString(sum[:16])
	etag := `"m` + key[:20] + `"`
	root, err := s.musicRoot()
	if err != nil {
		return nil, "", err
	}
	rel := filepath.Join("art", "mosaic", key+".jpg")
	if f, err := root.Open(rel); err == nil {
		now := time.Now()
		_ = root.Chtimes(rel, now, now)
		return f, etag, nil
	}
	tile := edge / 2
	canvas := image.NewRGBA(image.Rect(0, 0, 2*tile, 2*tile))
	for i, id := range ids {
		img, err := s.coverTile(ctx, root, id, tile)
		if err != nil {
			return nil, "", err
		}
		at := image.Pt((i%2)*tile, (i/2)*tile)
		b := img.Bounds()
		// Centre-crop to the tile.
		off := image.Pt(b.Min.X+(b.Dx()-tile)/2, b.Min.Y+(b.Dy()-tile)/2)
		draw.Draw(canvas, image.Rectangle{Min: at, Max: at.Add(image.Pt(tile, tile))}, img, off, draw.Src)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, canvas, &jpeg.Options{Quality: imaging.JPEGQuality}); err != nil {
		return nil, "", err
	}
	if err := writeAtomic(root, rel, buf.Bytes()); err != nil {
		return nil, "", err
	}
	f, err := root.Open(rel)
	if err != nil {
		return nil, "", err
	}
	return f, etag, nil
}

// coverTile scales stored art so its shorter edge covers tile pixels (decoding through the
// shared imaging budget) and decodes the result — a small JPEG this module produced.
func (s *Service) coverTile(ctx context.Context, root *os.Root, id string, tile int) (image.Image, error) {
	src, err := root.Open(artRel(id))
	if err != nil {
		return nil, errNoArt
	}
	data, err := io.ReadAll(io.LimitReader(src, maxArtUpload))
	src.Close()
	if err != nil {
		return nil, err
	}
	w, h, _, err := imaging.Size(bytes.NewReader(data))
	if err != nil {
		return nil, errNoArt
	}
	edge := tile
	if short := min(w, h); short > 0 && short < max(w, h) {
		edge = (tile*max(w, h) + short - 1) / short
	}
	edge = min(edge, imaging.MaxThumbEdge)
	select {
	case s.imgSem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	var buf bytes.Buffer
	err = imaging.ThumbnailContext(ctx, bytes.NewReader(data), edge, &buf)
	<-s.imgSem
	if err != nil {
		return nil, errNoArt
	}
	return jpeg.Decode(&buf)
}

/* ------------------------------------------------------------------------ GC */

// gc removes art no row references, stale mosaics and renditions and leftover temp files.
func (s *Service) gc(ctx context.Context) error {
	root, err := s.musicRoot()
	if err != nil {
		return err
	}
	used := map[string]bool{}
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT art_id FROM music_tracks WHERE art_id IS NOT NULL
		UNION SELECT art_id FROM music_folder_art WHERE art_id IS NOT NULL
		UNION SELECT art_id FROM music_album_art
		UNION SELECT art_id FROM music_playlists WHERE art_id IS NOT NULL`)
	if err != nil {
		return err
	}
	for rs.Next() {
		var id string
		if err := rs.Scan(&id); err != nil {
			rs.Close()
			return err
		}
		used[id] = true
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return err
	}
	older := func(d fs.DirEntry, age time.Duration) bool {
		info, err := d.Info()
		return err == nil && info.ModTime().Before(time.Now().Add(-age))
	}
	if entries, err := fs.ReadDir(root.FS(), "art"); err == nil {
		for _, d := range entries {
			id, ok := strings.CutSuffix(d.Name(), ".jpg")
			if d.Type().IsRegular() && (!ok || !used[id]) && older(d, time.Hour) {
				_ = root.Remove(filepath.Join("art", d.Name()))
			}
		}
	}
	if entries, err := fs.ReadDir(root.FS(), filepath.Join("art", "cache")); err == nil {
		for _, d := range entries {
			id, _, _ := strings.Cut(d.Name(), "-")
			if d.Type().IsRegular() && !used[id] && older(d, time.Hour) {
				_ = root.Remove(filepath.Join("art", "cache", d.Name()))
			}
		}
	}
	if entries, err := fs.ReadDir(root.FS(), filepath.Join("art", "mosaic")); err == nil {
		for _, d := range entries {
			if d.Type().IsRegular() && older(d, 7*24*time.Hour) {
				_ = root.Remove(filepath.Join("art", "mosaic", d.Name()))
			}
		}
	}
	if entries, err := fs.ReadDir(root.FS(), "tmp"); err == nil {
		for _, d := range entries {
			if older(d, 24*time.Hour) && !s.render.encoding(d.Name()) {
				_ = root.RemoveAll(filepath.Join("tmp", d.Name()))
			}
		}
	}
	if err := s.gcRenditions(ctx, root); err != nil {
		return err
	}
	return ctx.Err()
}
