package tv

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ludworks/internal/config"
	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
	"ludworks/internal/imaging"
)

// ensurePrivateDir creates dir (0700) when missing.
func ensurePrivateDir(dir string) error {
	return os.MkdirAll(dir, 0o700)
}

// logoPath is the logo file of a channel. Channel ids are validated server ids, so the name
// cannot escape the directory.
func (m *Module) logoPath(channelID string) string {
	return filepath.Join(m.logosDir(), channelID+".jpg")
}

func (m *Module) removeLogoFile(channelID string) {
	if !httpx.ValidID(channelID) {
		return
	}
	if err := os.Remove(m.logoPath(channelID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		m.log.Warn("removing channel logo failed", "channel", channelID, "err", err)
	}
}

var errUnsupportedImage = core.NewError(http.StatusUnsupportedMediaType, "unsupported_media_type",
	"Logos must be JPEG, PNG or GIF images")

// putLogo stores a channel logo: the body (≤ 20 MiB) is decoded with the size-checked
// imaging package and re-encoded as a JPEG of at most 512 px, so only server-made images are
// ever served.
func (m *Module) putLogo(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	c, err := m.channelFromPath(r, true)
	if err != nil {
		return err
	}
	if r.Body == nil || r.Body == http.NoBody {
		return core.BadRequest("image body required")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxLogoBytes+1))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return core.ErrPayloadTooLarge
		}
		return core.BadRequest("could not read the image")
	}
	if len(body) > MaxLogoBytes {
		return core.ErrPayloadTooLarge
	}
	if len(body) == 0 {
		return core.BadRequest("image body required")
	}
	var out bytes.Buffer
	if err := imaging.Thumbnail(bytes.NewReader(body), logoEdge, &out); err != nil {
		switch {
		case errors.Is(err, imaging.ErrTooLarge):
			return core.NewError(http.StatusRequestEntityTooLarge, "payload_too_large", "The image dimensions are too large")
		default:
			return errUnsupportedImage
		}
	}
	if err := ensurePrivateDir(m.logosDir()); err != nil {
		return err
	}
	if err := config.WriteFileAtomic(m.logoPath(c.ID), out.Bytes()); err != nil {
		return err
	}
	sum := sha256.Sum256(out.Bytes())
	ver := hex.EncodeToString(sum[:6])
	if _, err := m.d.DB.SQL.ExecContext(ctx, `UPDATE tv_channels SET logo_version = ?, updated_at = ? WHERE id = ?`,
		ver, db.FormatTime(m.now()), c.ID); err != nil {
		return err
	}
	return m.respondChannel(w, r, c.ID, http.StatusOK)
}

func (m *Module) deleteLogo(w http.ResponseWriter, r *http.Request) error {
	c, err := m.channelFromPath(r, true)
	if err != nil {
		return err
	}
	if _, err := m.d.DB.SQL.ExecContext(r.Context(), `UPDATE tv_channels SET logo_version = NULL, updated_at = ? WHERE id = ?`,
		db.FormatTime(m.now()), c.ID); err != nil {
		return err
	}
	m.removeLogoFile(c.ID)
	return httpx.NoContent(w)
}

func (m *Module) getLogo(w http.ResponseWriter, r *http.Request) error {
	c, err := m.channelFromPath(r, false)
	if err != nil {
		return err
	}
	if c.LogoVersion == "" {
		return core.NotFound("This channel has no logo")
	}
	f, err := os.Open(m.logoPath(c.ID))
	if errors.Is(err, os.ErrNotExist) {
		return core.NotFound("This channel has no logo")
	}
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	h := w.Header()
	h.Set("Content-Type", "image/jpeg")
	h.Set("ETag", `"`+c.LogoVersion+`"`)
	h.Set("Cache-Control", "private, max-age=86400")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	http.ServeContent(w, r, "", st.ModTime(), f)
	return nil
}

// sweepLogos removes logo files whose channel no longer exists (deleted with a user or a
// lineup).
func (m *Module) sweepLogos(ctx context.Context) error {
	ents, err := os.ReadDir(m.logosDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-time.Minute) // never race a logo being uploaded right now
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), ".jpg")
		if !ok || !httpx.ValidID(id) || e.IsDir() {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().After(cutoff) {
			continue
		}
		var n int
		if err := m.d.DB.SQL.QueryRowContext(ctx, `SELECT count(*) FROM tv_channels WHERE id = ? AND logo_version IS NOT NULL`, id).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			m.removeLogoFile(id)
		}
	}
	return nil
}
