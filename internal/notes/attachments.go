package notes

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
)

// MaxAttachmentBytes caps a single attachment.
const MaxAttachmentBytes = 25 << 20

// inlineImageTypes are the only types served with Content-Disposition: inline. SVG is
// deliberately absent (it can carry script).
var inlineImageTypes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
	"image/bmp": true, "image/avif": true, "image/x-icon": true, "image/vnd.microsoft.icon": true,
}

// servedTypes are content types served as-is (always as a download unless an inline
// image); anything else is served as application/octet-stream.
var servedTypes = map[string]bool{
	"application/pdf": true, "application/zip": true, "application/json": true,
	"text/plain": true, "text/csv": true, "text/markdown": true,
	"audio/mpeg": true, "audio/ogg": true, "audio/wave": true, "audio/wav": true, "audio/aiff": true, "audio/midi": true,
	"video/mp4": true, "video/webm": true, "video/avi": true, "application/ogg": true,
	"font/woff": true, "font/woff2": true, "font/ttf": true, "font/otf": true,
}

// sanitizeFilename keeps the base name of a client-supplied file name and rejects names
// that are empty, too long or contain control characters.
func sanitizeFilename(name string) (string, string) {
	name = strings.ToValidUTF8(name, "�")
	name = strings.ReplaceAll(name, `\`, "/")
	name = path.Base(strings.TrimSpace(name))
	name = strings.TrimSpace(name)
	switch {
	case name == "" || name == "." || name == ".." || name == "/":
		return "", "is required"
	case strings.ContainsFunc(name, unicode.IsControl):
		return "", "must not contain control characters"
	case len(name) > 255:
		return "", "must be at most 255 bytes"
	}
	return name, ""
}

// sniffMime determines the stored MIME type from content, refined by extension for types
// content sniffing cannot tell apart.
func sniffMime(head []byte, filename string) string {
	ct := http.DetectContentType(head)
	base, _, _ := mime.ParseMediaType(ct)
	ext := strings.ToLower(path.Ext(filename))
	switch {
	case base == "text/plain" || base == "application/octet-stream":
		switch ext {
		case ".md", ".markdown":
			return "text/markdown"
		case ".csv":
			return "text/csv"
		case ".json":
			return "application/json"
		case ".svg":
			if base == "text/plain" {
				return "image/svg+xml"
			}
		}
	case base == "text/xml" && ext == ".svg":
		return "image/svg+xml"
	}
	if base == "" {
		return "application/octet-stream"
	}
	return base
}

func (s *Service) root() (*os.Root, error) {
	if s.attachDir == "" {
		return nil, errors.New("notes: no attachment directory")
	}
	if err := os.MkdirAll(s.attachDir, 0o700); err != nil {
		return nil, err
	}
	return os.OpenRoot(s.attachDir)
}

func isDiskFull(err error) bool {
	if errors.Is(err, syscall.ENOSPC) {
		return true
	}
	var errno syscall.Errno
	if runtime.GOOS == "windows" && errors.As(err, &errno) {
		return errno == 112 || errno == 39 // ERROR_DISK_FULL, ERROR_HANDLE_DISK_FULL
	}
	return false
}

func storageError(err error) error {
	if isDiskFull(err) {
		return core.NewError(http.StatusInsufficientStorage, "insufficient_storage", "The server is out of disk space")
	}
	return err
}

// AddAttachment stores r (at most 25 MiB) as an attachment of the user's note.
func (s *Service) AddAttachment(ctx context.Context, uid, noteID, filename string, r io.Reader) (*Attachment, error) {
	name, problem := sanitizeFilename(filename)
	if problem != "" {
		return nil, core.Validation(map[string]string{"filename": problem})
	}
	if _, err := getNoteTx(ctx, s.d.DB.SQL, uid, noteID); err != nil {
		return nil, err
	}
	root, err := s.root()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	id := core.NewID()
	tmp := ".tmp-" + id
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, storageError(err)
	}
	committed := false
	defer func() {
		if !committed {
			f.Close()
			_ = root.Remove(tmp)
		}
	}()
	head := make([]byte, 512)
	hn, rerr := io.ReadFull(r, head)
	head = head[:hn]
	// io.ReadFull reports a short (but complete) body with exactly these sentinels; anything
	// else, including a wrapped io.ErrUnexpectedEOF from a client that hung up mid-body, is a
	// read failure and must not be stored as a truncated attachment.
	if rerr != nil && rerr != io.ErrUnexpectedEOF && rerr != io.EOF { //nolint:errorlint // sentinels from io.ReadFull
		return nil, rerr
	}
	if hn == 0 {
		return nil, core.BadRequest("the file is empty")
	}
	if _, err := f.Write(head); err != nil {
		return nil, storageError(err)
	}
	n, err := io.Copy(f, io.LimitReader(r, MaxAttachmentBytes-int64(hn)+1))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, core.ErrPayloadTooLarge
		}
		return nil, storageError(err)
	}
	size := int64(hn) + n
	if size > MaxAttachmentBytes {
		return nil, core.NewError(http.StatusRequestEntityTooLarge, "payload_too_large", "Attachments are limited to 25 MiB")
	}
	if err := f.Sync(); err != nil {
		return nil, storageError(err)
	}
	if err := f.Close(); err != nil {
		return nil, storageError(err)
	}
	if err := root.Rename(tmp, id); err != nil {
		return nil, err
	}
	committed = true
	att := &Attachment{ID: id, URL: "/api/v1/notes/attachments/" + id, Filename: name, Mime: sniffMime(head, name), Size: size}
	err = s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := getNoteTx(ctx, tx, uid, noteID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO note_attachments (id, note_id, user_id, filename, mime, size, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, id, noteID, uid, att.Filename, att.Mime, att.Size, db.FormatTime(core.Now()))
		return err
	})
	if err != nil {
		_ = root.Remove(id)
		return nil, err
	}
	return att, nil
}

// OpenAttachment opens the user's attachment for reading.
func (s *Service) OpenAttachment(ctx context.Context, uid, id string) (*os.File, *Attachment, error) {
	var a Attachment
	err := s.d.DB.SQL.QueryRowContext(ctx, `SELECT id, note_id, filename, mime, size, created_at FROM note_attachments WHERE id = ? AND user_id = ?`,
		id, uid).Scan(&a.ID, &a.noteID, &a.Filename, &a.Mime, &a.Size, &a.createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, core.NotFound("attachment not found")
	}
	if err != nil {
		return nil, nil, err
	}
	a.URL = "/api/v1/notes/attachments/" + a.ID
	root, err := s.root()
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	f, err := root.Open(a.ID)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, core.NotFound("attachment not found")
	}
	if err != nil {
		return nil, nil, err
	}
	return f, &a, nil
}

// serveAttachment writes the attachment with safe headers (inline only for raster images).
func serveAttachment(w http.ResponseWriter, r *http.Request, f *os.File, a *Attachment) {
	h := w.Header()
	ct := a.Mime
	disp := "attachment"
	switch {
	case inlineImageTypes[ct]:
		disp = "inline"
	case strings.HasPrefix(ct, "text/") && servedTypes[ct]:
		ct += "; charset=utf-8"
	case !servedTypes[ct]:
		ct = "application/octet-stream"
	}
	h.Set("Content-Type", ct)
	h.Set("Content-Disposition", contentDisposition(disp, a.Filename))
	h.Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'; sandbox")
	h.Set("Cache-Control", "private, no-cache")
	h.Set("ETag", `"`+a.ID+`"`)
	mod := time.Time{}
	if t, err := db.ParseTime(a.createdAt); err == nil {
		mod = t
	}
	http.ServeContent(w, r, "", mod, f)
}

// contentDisposition builds a header value with an ASCII fallback and RFC 5987 filename*.
func contentDisposition(disp, filename string) string {
	if v := mime.FormatMediaType(disp, map[string]string{"filename": filename}); v != "" {
		return v
	}
	fallback := strings.Map(func(r rune) rune {
		if r < 0x20 || r >= 0x7f || r == '"' || r == '\\' || r == '/' {
			return '_'
		}
		return r
	}, filename)
	return fmt.Sprintf(`%s; filename="%s"`, disp, fallback)
}

// runGC deletes the files of removed attachments and is safe to run concurrently with
// uploads (only queued ids are touched).
func (s *Service) runGC(ctx context.Context) error {
	root, err := s.root()
	if err != nil {
		return err
	}
	defer root.Close()
	for {
		rows, err := s.d.DB.SQL.QueryContext(ctx, `SELECT id FROM note_attachment_gc LIMIT 500`)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if len(ids) == 0 {
			return nil
		}
		for _, id := range ids {
			if httpx.ValidID(id) {
				if err := root.Remove(id); err != nil && !errors.Is(err, fs.ErrNotExist) {
					s.d.Log.Warn("notes: remove attachment file", "id", id, "err", err)
				}
			}
		}
		if _, err := s.d.DB.SQL.ExecContext(ctx, `DELETE FROM note_attachment_gc WHERE id IN (`+placeholders(len(ids))+`)`, anySlice(ids)...); err != nil {
			return err
		}
		if len(ids) < 500 {
			return nil
		}
	}
}

// sweepOrphans removes attachment files that have no database row (e.g. a crash between
// writing the file and committing the row) and stale temp files, once they are older than
// an hour so in-flight uploads are never touched.
func (s *Service) sweepOrphans(ctx context.Context) error {
	if s.attachDir == "" {
		return nil
	}
	entries, err := os.ReadDir(s.attachDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	root, err := s.root()
	if err != nil {
		return err
	}
	defer root.Close()
	cutoff := time.Now().Add(-time.Hour)
	for _, e := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || info.ModTime().After(cutoff) {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".tmp-") {
			_ = root.Remove(name)
			continue
		}
		if !httpx.ValidID(name) || !utf8.ValidString(name) {
			continue
		}
		var x int
		err = s.d.DB.SQL.QueryRowContext(ctx, `SELECT 1 FROM note_attachments WHERE id = ?`, name).Scan(&x)
		if errors.Is(err, sql.ErrNoRows) {
			_ = root.Remove(name)
		} else if err != nil {
			return err
		}
	}
	return nil
}
