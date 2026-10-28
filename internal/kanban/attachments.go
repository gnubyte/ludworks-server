package kanban

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/files"
	"ludworks/internal/httpx"
	"ludworks/internal/imaging"
)

// Thumbnail states (kanban_attachments.thumb_state).
const (
	thumbUnknown = 0
	thumbOK      = 1
	thumbFailed  = 2
)

var thumbSizes = map[string]int{"sm": 160, "md": 320}

// freeHeadroom is kept free on the data volume beyond any attachment, like file uploads
// (API.md §5).
const freeHeadroom int64 = 1 << 30

var (
	errNoSpace   = core.NewError(http.StatusInsufficientStorage, "insufficient_storage", "Not enough free disk space on the server")
	errTooLarge  = core.NewError(http.StatusRequestEntityTooLarge, "payload_too_large", "Attachments are limited to 50 MiB")
	errBoardFull = core.NewError(http.StatusInsufficientStorage, "quota_exceeded", "The board's attachments are limited to 2 GiB")
	errNoThumb   = core.NotFound("No thumbnail available")
)

const attachmentBoardQuery = `SELECT t.board_id, m.role FROM kanban_attachments t
	LEFT JOIN kanban_members m ON m.board_id = t.board_id AND m.user_id = ? WHERE t.id = ?`

/* ------------------------------------------------------------------ disk */

func openRoot(dir string) (*os.Root, error) {
	if dir == "" {
		return nil, errors.New("kanban: no data directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return os.OpenRoot(dir)
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
		return errNoSpace
	}
	return err
}

// checkFree verifies that writing n more bytes keeps freeHeadroom free on the data volume.
// Attachments are small (≤ 50 MiB), so concurrent writers cannot overshoot the headroom.
func (m *Module) checkFree(n int64) error {
	if err := os.MkdirAll(m.attachDir, 0o700); err != nil {
		return storageError(err)
	}
	free, err := m.freeSpace(m.attachDir)
	if err != nil {
		m.d.Log.Warn("kanban: free space check failed", "err", err)
		return nil // the platform cannot report free space; ENOSPC still maps to 507
	}
	if free-freeHeadroom < n {
		return errNoSpace
	}
	return nil
}

/* ---------------------------------------------------------------- upload */

// attachTarget checks that uid may add an attachment to the card (editor) and that the card has
// room; it is checked before the bytes are streamed and again when the row is inserted.
func attachTarget(ctx context.Context, q db.Querier, cardID, uid string, size int64) (*cardRef, error) {
	card, rl, err := cardFor(ctx, q, cardID, uid)
	if err != nil {
		return nil, err
	}
	if err := needRole(rl, roleEditor); err != nil {
		return nil, err
	}
	var n int
	var total int64
	if err := q.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM kanban_attachments WHERE card_id = ?),
		(SELECT COALESCE(sum(size), 0) FROM kanban_attachments WHERE board_id = ?)`, cardID, card.BoardID).Scan(&n, &total); err != nil {
		return nil, err
	}
	if n >= MaxAttachments {
		return nil, tooMany("attachments on this card", MaxAttachments)
	}
	if size > 0 && total+size > MaxBoardAttachBytes {
		return nil, errBoardFull
	}
	return card, nil
}

// addAttachment stores body (at most MaxAttachmentBytes; declared < 0 when unknown) as an
// attachment of the card and records it.
func (m *Module) addAttachment(ctx context.Context, uid, cardID, filename string, body io.Reader, declared int64) (*Attachment, error) {
	if declared > MaxAttachmentBytes {
		return nil, errTooLarge
	}
	if _, err := attachTarget(ctx, m.d.DB.SQL, cardID, uid, max(declared, 0)); err != nil {
		return nil, err
	}
	need := declared
	if need < 0 {
		need = MaxAttachmentBytes
	}
	if err := m.checkFree(need); err != nil {
		return nil, err
	}
	root, err := openRoot(m.attachDir)
	if err != nil {
		return nil, storageError(err)
	}
	defer root.Close()
	id := core.NewID()
	tmp := ".tmp-" + id
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, storageError(err)
	}
	stored := false
	defer func() {
		if !stored {
			f.Close()
			_ = root.Remove(tmp)
		}
	}()
	head := make([]byte, 512)
	hn, rerr := io.ReadFull(body, head)
	head = head[:hn]
	// io.ReadFull reports a short (but complete) body with exactly these sentinels; anything
	// else (a client that hung up mid-body) is a read failure, never a truncated attachment.
	if rerr != nil && rerr != io.ErrUnexpectedEOF && rerr != io.EOF { //nolint:errorlint // sentinels from io.ReadFull
		return nil, rerr
	}
	if hn == 0 {
		return nil, core.BadRequest("The file is empty")
	}
	if _, err := f.Write(head); err != nil {
		return nil, storageError(err)
	}
	n, err := io.Copy(f, io.LimitReader(body, MaxAttachmentBytes-int64(hn)+1))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, errTooLarge
		}
		return nil, storageError(err)
	}
	size := int64(hn) + n
	if size > MaxAttachmentBytes {
		return nil, errTooLarge
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
	stored = true
	mimeType := detectMime(filename, head)
	var out *Attachment
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		card, err := attachTarget(ctx, tx, cardID, uid, size)
		if err != nil {
			return err
		}
		ts := now()
		if _, err := tx.ExecContext(ctx, `INSERT INTO kanban_attachments (id, card_id, board_id, filename, mime, size, uploaded_by, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, id, cardID, card.BoardID, filename, mimeType, size, uid, ts); err != nil {
			return err
		}
		if err := logActivity(ctx, tx, card.BoardID, cardID, uid, "attachment.added",
			map[string]any{"attachment_id": id, "filename": filename, "size": size, "title": card.Title}, ts); err != nil {
			return err
		}
		if err := touch(ctx, tx, card.BoardID, ts); err != nil {
			return err
		}
		out, err = getAttachment(ctx, tx, id)
		return err
	})
	if err != nil {
		_ = root.Remove(id)
		return nil, err
	}
	return out, nil
}

func (m *Module) uploadAttachment(w http.ResponseWriter, r *http.Request) error {
	cardID, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	name, problem := cleanFilename(r.URL.Query().Get("filename"))
	if problem != "" {
		return validation("filename", problem)
	}
	if r.Body == nil || r.Body == http.NoBody {
		return core.BadRequest("The file is empty")
	}
	a, err := m.addAttachment(r.Context(), uidOf(r), cardID, name, r.Body, r.ContentLength)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusCreated, a)
}

// attachFromFile copies a Files entry the caller can read into an attachment.
func (m *Module) attachFromFile(w http.ResponseWriter, r *http.Request) error {
	cardID, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		FileID string `json:"file_id"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	if !httpx.ValidID(in.FileID) {
		return validation("file_id", "is required")
	}
	if m.files == nil {
		return core.NotImplemented("Files are not available")
	}
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	if !p.Has("files:read") {
		return core.Forbidden("Missing required scope: files:read")
	}
	// Board access first: a viewer learns nothing about the file.
	if _, err := attachTarget(ctx, m.d.DB.SQL, cardID, p.UserID, 0); err != nil {
		return err
	}
	f, entry, err := m.files.Open(ctx, p, in.FileID)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() > MaxAttachmentBytes {
		return errTooLarge
	}
	name, problem := cleanFilename(entry.Name)
	if problem != "" {
		name = "file"
	}
	a, err := m.addAttachment(ctx, p.UserID, cardID, name, io.LimitReader(f, st.Size()), st.Size())
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusCreated, a)
}

/* --------------------------------------------------------------- serving */

// readableAttachment loads an attachment uid may read (any member of its board).
func readableAttachment(ctx context.Context, q db.Querier, id, uid string) (*Attachment, role, error) {
	_, rl, err := lookupBoard(ctx, q, attachmentBoardQuery, id, uid, errAttachmentNotFound)
	if err != nil {
		return nil, roleNone, err
	}
	a, err := getAttachment(ctx, q, id)
	if err != nil {
		return nil, roleNone, err
	}
	return a, rl, nil
}

func (m *Module) openAttachment(id string) (*os.File, error) {
	root, err := openRoot(m.attachDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(id)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, core.NotFound("The attachment's content is missing")
	}
	return f, err
}

// strongETag derives a validator from id, size, name and modification time.
func strongETag(a *Attachment, mod time.Time) string {
	h := sha256.Sum256([]byte(a.ID + "|" + strconv.FormatInt(a.Size, 10) + "|" + a.Filename + "|" + strconv.FormatInt(mod.UnixNano(), 10)))
	return `"` + hex.EncodeToString(h[:16]) + `"`
}

// getAttachmentContent serves the bytes with the Files content headers: browser images inline
// (unless download=1) with their type, everything else an application/octet-stream download;
// always nosniff and a sandboxing CSP.
func (m *Module) getAttachmentContent(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	a, _, err := readableAttachment(r.Context(), m.d.DB.SQL, id, uidOf(r))
	if err != nil {
		return err
	}
	f, err := m.openAttachment(a.ID)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	inline := !httpx.QueryBool(r, "download") && inlineImage(a.Mime)
	h := w.Header()
	h.Set("Content-Type", servedContentType(a.Mime, inline))
	if inline {
		h.Set("Content-Disposition", contentDisposition("inline", a.Filename))
	} else {
		h.Set("Content-Disposition", contentDisposition("attachment", a.Filename))
	}
	h.Set("ETag", strongETag(a, st.ModTime()))
	h.Set("Cache-Control", "private, no-cache")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", contentCSP)
	h.Set("Accept-Ranges", "bytes")
	http.ServeContent(w, r, "", st.ModTime(), f)
	return nil
}

func (m *Module) getAttachmentThumbnail(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	size := r.URL.Query().Get("size")
	if size == "" {
		size = "md"
	}
	edge, ok := thumbSizes[size]
	if !ok {
		return validation("size", "must be sm or md")
	}
	ctx := r.Context()
	a, _, err := readableAttachment(ctx, m.d.DB.SQL, id, uidOf(r))
	if err != nil {
		return err
	}
	if !thumbable(a.Mime) || a.thumbState == thumbFailed {
		return errNoThumb
	}
	name := a.ID + "-" + size + ".jpg"
	data, err := m.thumbnail(ctx, a, name, edge)
	if err != nil {
		return err
	}
	h := w.Header()
	h.Set("Content-Type", "image/jpeg")
	h.Set("ETag", `"t`+a.ID+size+`"`)
	h.Set("Cache-Control", "private, max-age=3600")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", contentCSP)
	mod, _ := db.ParseTime(a.CreatedAt)
	http.ServeContent(w, r, "", mod, bytes.NewReader(data))
	return nil
}

// thumbnail returns a cached JPEG thumbnail, generating it (under the shared image decode
// budget) when needed. Attachments never change, so a cached thumbnail stays valid.
func (m *Module) thumbnail(ctx context.Context, a *Attachment, name string, edge int) ([]byte, error) {
	troot, err := openRoot(m.thumbDir)
	if err != nil {
		return nil, err
	}
	defer troot.Close()
	if b, err := readSmall(troot, name); err == nil {
		return b, nil
	}
	select {
	case m.thumbSem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-m.thumbSem }()
	if b, err := readSmall(troot, name); err == nil {
		return b, nil // made by a concurrent request
	}
	f, err := m.openAttachment(a.ID)
	if err != nil {
		return nil, errNoThumb
	}
	defer f.Close()
	var buf bytes.Buffer
	if err := imaging.ThumbnailContext(ctx, f, edge, &buf); err != nil {
		if ctx.Err() == nil {
			m.d.Log.Debug("kanban: thumbnail failed", "id", a.ID, "err", err)
			m.setThumbState(a.ID, thumbFailed)
		}
		return nil, errNoThumb
	}
	tmp := ".tmp-" + core.NewID()
	if err := writeFile(troot, tmp, buf.Bytes()); err == nil {
		if err := troot.Rename(tmp, name); err != nil {
			_ = troot.Remove(tmp)
		}
	}
	if a.thumbState != thumbOK {
		m.setThumbState(a.ID, thumbOK)
	}
	return buf.Bytes(), nil
}

func readSmall(root *os.Root, name string) ([]byte, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 16<<20))
	if err == nil && len(b) == 0 {
		err = io.ErrUnexpectedEOF
	}
	return b, err
}

func writeFile(root *os.Root, name string, b []byte) error {
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		_ = root.Remove(name)
		return errors.Join(werr, cerr)
	}
	return nil
}

func (m *Module) setThumbState(id string, state int) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = m.d.DB.SQL.ExecContext(ctx, `UPDATE kanban_attachments SET thumb_state = ? WHERE id = ?`, state, id)
}

/* -------------------------------------------------------- rename, delete */

func (m *Module) patchAttachment(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Filename string `json:"filename"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	name, problem := cleanFilename(in.Filename)
	if problem != "" {
		return validation("filename", problem)
	}
	ctx := r.Context()
	uid := uidOf(r)
	var out *Attachment
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		a, rl, err := readableAttachment(ctx, tx, id, uid)
		if err != nil {
			return err
		}
		if err := needRole(rl, roleEditor); err != nil {
			return err
		}
		if name != a.Filename {
			ts := now()
			if _, err := tx.ExecContext(ctx, `UPDATE kanban_attachments SET filename = ? WHERE id = ?`, name, id); err != nil {
				return err
			}
			if err := logActivity(ctx, tx, a.boardID, a.CardID, uid, "attachment.renamed",
				map[string]any{"attachment_id": id, "filename": name, "old_filename": a.Filename}, ts); err != nil {
				return err
			}
			if err := touch(ctx, tx, a.boardID, ts); err != nil {
				return err
			}
		}
		out, err = getAttachment(ctx, tx, id)
		return err
	})
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

// deleteAttachment removes an attachment (editors, and its uploader while still a member); a
// card cover using it is cleared by the schema trigger.
func (m *Module) deleteAttachment(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	uid := uidOf(r)
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		a, rl, err := readableAttachment(ctx, tx, id, uid)
		if err != nil {
			return err
		}
		if rl < roleEditor && a.UploadedBy != uid {
			return core.Forbidden("Only editors and the uploader can delete an attachment")
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM kanban_attachments WHERE id = ?`, id); err != nil {
			return err
		}
		ts := now()
		if err := logActivity(ctx, tx, a.boardID, a.CardID, uid, "attachment.deleted",
			map[string]any{"attachment_id": id, "filename": a.Filename}, ts); err != nil {
			return err
		}
		return touch(ctx, tx, a.boardID, ts)
	})
	if err != nil {
		return err
	}
	m.kickGC()
	return httpx.NoContent(w)
}

/* --------------------------------------------------------- save to Files */

func (m *Module) saveToFiles(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		SpaceID   string `json:"space_id"`
		Path      string `json:"path"`
		Name      string `json:"name"`
		Overwrite bool   `json:"overwrite"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	if in.SpaceID != "" && !httpx.ValidID(in.SpaceID) {
		return core.NotFound("Space not found")
	}
	if m.files == nil {
		return core.NotImplemented("Files are not available")
	}
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	if !p.Has("files:write") {
		return core.Forbidden("Missing required scope: files:write")
	}
	a, _, err := readableAttachment(ctx, m.d.DB.SQL, id, p.UserID)
	if err != nil {
		return err
	}
	name := a.Filename
	if strings.TrimSpace(in.Name) != "" {
		name = in.Name
	}
	space := in.SpaceID
	if space == "" {
		if space, err = m.files.HomeSpaceID(ctx, p.UserID); err != nil {
			return err
		}
	}
	f, err := m.openAttachment(a.ID)
	if err != nil {
		return err
	}
	defer f.Close()
	mod, _ := db.ParseTime(a.CreatedAt)
	e, err := m.files.CreateFromReader(ctx, p, space, in.Path, name, f, files.CreateOpts{Overwrite: in.Overwrite, ModTime: mod})
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusCreated, entryJSON(e))
}

// entryJSON renders a Files entry like the Files API does (API.md §5 Entry).
func entryJSON(e *files.Entry) map[string]any {
	var parent any
	if e.ParentID != "" {
		parent = e.ParentID
	}
	tags := e.Tags
	if tags == nil {
		tags = []string{}
	}
	return map[string]any{
		"id": e.ID, "space_id": e.SpaceID, "parent_id": parent, "path": e.Path, "name": e.Name,
		"is_dir": e.IsDir, "size": e.Size, "mime": e.Mime, "mod_time": db.FormatTime(e.ModTime),
		"category": e.Category, "subcategory": e.Subcategory, "tags": tags, "sha256": e.SHA256,
		"owner_id": e.OwnerID, "created_at": db.FormatTime(e.CreatedAt), "updated_at": db.FormatTime(e.UpdatedAt),
		"has_thumbnail": thumbable(e.Mime),
	}
}

/* ---------------------------------------------------------- housekeeping */

// runGC deletes the files and cached thumbnails of removed attachments; only queued ids are
// touched, so it is safe next to uploads.
func (m *Module) runGC(ctx context.Context) error {
	root, err := openRoot(m.attachDir)
	if err != nil {
		return err
	}
	defer root.Close()
	troot, err := openRoot(m.thumbDir)
	if err != nil {
		return err
	}
	defer troot.Close()
	for {
		rows, err := m.d.DB.SQL.QueryContext(ctx, `SELECT id FROM kanban_attachment_gc LIMIT 500`)
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
		if err := rows.Err(); err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		for _, id := range ids {
			if !httpx.ValidID(id) {
				continue
			}
			if err := root.Remove(id); err != nil && !errors.Is(err, fs.ErrNotExist) {
				m.d.Log.Warn("kanban: remove attachment file", "id", id, "err", err)
			}
			for size := range thumbSizes {
				_ = troot.Remove(id + "-" + size + ".jpg")
			}
			if _, err := m.d.DB.SQL.ExecContext(ctx, `DELETE FROM kanban_attachment_gc WHERE id = ?`, id); err != nil {
				return err
			}
		}
		if len(ids) < 500 {
			return nil
		}
	}
}

// sweepOrphans removes attachment files without a database row (a crash between writing the
// file and committing the row), stale temp files and thumbnails of unknown attachments, once
// they are an hour old so in-flight uploads are never touched.
func (m *Module) sweepOrphans(ctx context.Context) error {
	cutoff := time.Now().Add(-time.Hour)
	exists := func(id string) (bool, error) {
		var x int
		err := m.d.DB.SQL.QueryRowContext(ctx, `SELECT 1 FROM kanban_attachments WHERE id = ?`, id).Scan(&x)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return err == nil, err
	}
	for _, dir := range []string{m.attachDir, m.thumbDir} {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		root, err := openRoot(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if ctx.Err() != nil {
				root.Close()
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
			id := name
			if dir == m.thumbDir {
				id, _, _ = strings.Cut(name, "-")
			}
			if !httpx.ValidID(id) {
				continue
			}
			ok, err := exists(id)
			if err != nil {
				root.Close()
				return err
			}
			if !ok {
				_ = root.Remove(name)
			}
		}
		root.Close()
	}
	return nil
}
