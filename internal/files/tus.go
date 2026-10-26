package files

import (
	"context"
	"crypto/sha1" //nolint:gosec // tus checksum extension (integrity, not security)
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
)

// tus 1.0.0 server (API.md §5.1): core + creation, termination, checksum (sha1, sha256)
// and expiration. Data is streamed to <data>/tmp/uploads/<id>.part; nothing is buffered in
// memory and every size is an int64, so uploads of 100+ GiB work.

const (
	tusVersion    = "1.0.0"
	tusExtensions = "creation,termination,checksum,expiration"
	tusChecksums  = "sha1,sha256"

	maxActiveUploads   = 1000
	maxMetadataHeader  = 8 << 10
	simpleUploadMax    = 2 << 30 // 2 GiB when files.max_upload_bytes is unlimited
	bodyIdleTimeout    = 2 * time.Minute
	completedRetention = 24 * time.Hour
)

// filesSettings reads the "files" namespace (defaults on error).
func (s *Service) filesSettings(ctx context.Context) Settings {
	st := DefaultSettings
	if err := s.d.Settings.Get(ctx, "files", &st); err != nil {
		return DefaultSettings
	}
	return st
}

type uploadMeta struct {
	Overwrite   bool     `json:"overwrite,omitempty"`
	ModTime     string   `json:"mod_time,omitempty"`
	Category    string   `json:"category,omitempty"`
	Subcategory string   `json:"subcategory,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Mime        string   `json:"mime,omitempty"`
}

type upload struct {
	ID, UserID, SpaceID, DirPath, Filename string
	Length, Received                       int64
	Meta                                   uploadMeta
	State                                  string
	FileID, FilePath                       string
	CreatedAt, ExpiresAt                   time.Time
}

const uploadCols = `id, user_id, space_id, dir_path, filename, length, received, meta, state, file_id, file_path, created_at, expires_at`

func scanUpload(sc scanner) (*upload, error) {
	var (
		u              upload
		meta           db.JSON[uploadMeta]
		fid, fpath     sql.NullString
		created, expir db.Time
	)
	if err := sc.Scan(&u.ID, &u.UserID, &u.SpaceID, &u.DirPath, &u.Filename, &u.Length, &u.Received, &meta, &u.State,
		&fid, &fpath, &created, &expir); err != nil {
		return nil, err
	}
	u.Meta, u.FileID, u.FilePath, u.CreatedAt, u.ExpiresAt = meta.V, fid.String, fpath.String, created.Time, expir.Time
	return &u, nil
}

func (s *Service) loadUpload(ctx context.Context, id, userID string) (*upload, error) {
	u, err := scanUpload(s.d.DB.SQL.QueryRowContext(ctx, `SELECT `+uploadCols+` FROM uploads WHERE id = ? AND user_id = ?`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, core.NotFound("Upload not found")
	}
	return u, err
}

// partRel is the data-root-relative path of an upload's data file.
func partRel(id string) string { return filepath.Join("tmp", "uploads", id+".part") }

func (s *Service) partAbs(id string) string { return filepath.Join(s.TempDir(), "uploads", id+".part") }

func setTusHeaders(h http.Header) {
	h.Set("Tus-Resumable", tusVersion)
	h.Set("Cache-Control", "no-store")
}

func checkTusResumable(w http.ResponseWriter, r *http.Request) error {
	if r.Header.Get("Tus-Resumable") != tusVersion {
		w.Header().Set("Tus-Version", tusVersion)
		return core.NewError(http.StatusPreconditionFailed, "precondition_failed", "Tus-Resumable: 1.0.0 is required")
	}
	return nil
}

func (m *Module) tusOptions(w http.ResponseWriter, r *http.Request) error {
	h := w.Header()
	setTusHeaders(h)
	h.Set("Tus-Version", tusVersion)
	h.Set("Tus-Extension", tusExtensions)
	h.Set("Tus-Checksum-Algorithm", tusChecksums)
	if max := m.svc.filesSettings(r.Context()).MaxUploadBytes; max > 0 {
		h.Set("Tus-Max-Size", strconv.FormatInt(max, 10))
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// parseMetadata decodes "key b64,key2 b64" (tus Upload-Metadata).
func parseMetadata(h string) (map[string]string, error) {
	out := map[string]string{}
	if len(h) > maxMetadataHeader {
		return nil, core.BadRequest("Upload-Metadata is too large")
	}
	for _, part := range strings.Split(h, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, val, _ := strings.Cut(part, " ")
		if key == "" || len(key) > 64 || strings.ContainsAny(key, " \t") {
			return nil, core.BadRequest("Upload-Metadata: invalid key")
		}
		if _, dup := out[key]; dup {
			return nil, core.BadRequest("Upload-Metadata: duplicate key " + key)
		}
		dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(val))
		if err != nil {
			return nil, core.BadRequest("Upload-Metadata: value of " + key + " is not valid base64")
		}
		out[key] = string(dec)
	}
	return out, nil
}

func metaBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// uploadTarget is a validated upload destination.
type uploadTarget struct {
	sp      *space
	dirPath string
	name    string
	meta    uploadMeta
	owner   string // charged for the bytes (ownerFor)
}

// prepareUpload validates destination, name, metadata, conflicts and limits before any byte
// is accepted. Quota and free space are checked by the caller's admission (admit), which
// also records the promise.
func (s *Service) prepareUpload(ctx context.Context, p *core.Principal, spaceID, dirPath, name string, length int64, meta uploadMeta, simple bool) (*uploadTarget, error) {
	if err := validName("filename", name); err != nil {
		return nil, err
	}
	dirPath, err := cleanPath("path", dirPath)
	if err != nil {
		return nil, err
	}
	if meta.ModTime != "" {
		if _, err := timeParam("mod_time", meta.ModTime); err != nil {
			return nil, err
		}
	}
	if err := s.checkCategory(ctx, meta.Category, meta.Subcategory); err != nil {
		return nil, err
	}
	if meta.Tags, err = normalizeTags("tags", meta.Tags); err != nil {
		return nil, err
	}
	if spaceID == "" {
		if spaceID, err = s.HomeSpaceID(ctx, p.UserID); err != nil {
			return nil, err
		}
	}
	sp, _, err := s.spaceFor(ctx, s.d.DB.SQL, p, spaceID, accWrite)
	if err != nil {
		return nil, err
	}
	max := s.filesSettings(ctx).MaxUploadBytes
	if simple && (max <= 0 || max > simpleUploadMax) {
		max = simpleUploadMax
	}
	if max > 0 && length > max {
		return nil, core.NewError(http.StatusRequestEntityTooLarge, "payload_too_large", "The file is larger than the upload limit").
			WithDetails(map[string]any{"max_bytes": max})
	}
	// Conflicts with existing entries (re-checked at commit), so a doomed upload fails before
	// any data is sent. Components missing from the index are also checked on disk: on a
	// case-insensitive disk they may alias a differently-cased sibling.
	target := joinPath(dirPath, name)
	parts := strings.Split(target, "/")
	for i := range parts {
		p := strings.Join(parts[:i+1], "/")
		r, err := rowByPath(ctx, s.d.DB.SQL, sp.ID, p)
		if err != nil {
			return nil, err
		}
		if i < len(parts)-1 {
			if r != nil && !r.IsDir {
				return nil, core.Conflict("The target path contains a file where a folder is expected")
			}
		} else if r != nil && (r.IsDir || !meta.Overwrite) {
			return nil, core.Conflict("A file or folder with this name already exists")
		}
		if r == nil {
			if err := s.checkUnindexedAlias(sp, p, parts[i]); err != nil {
				return nil, err
			}
			break // nothing below an unindexed component is indexed either
		}
	}
	return &uploadTarget{sp: sp, dirPath: dirPath, name: name, meta: meta, owner: ownerFor(sp, p.UserID)}, nil
}

func (m *Module) tusCreate(w http.ResponseWriter, r *http.Request) error {
	if err := checkTusResumable(w, r); err != nil {
		return err
	}
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	if r.Header.Get("Upload-Defer-Length") != "" {
		return core.BadRequest("Upload-Defer-Length is not supported")
	}
	if r.ContentLength > 0 {
		return core.BadRequest("Creation with upload is not supported; send data with PATCH")
	}
	length, err := strconv.ParseInt(r.Header.Get("Upload-Length"), 10, 64)
	if err != nil || length < 0 {
		return core.BadRequest("Upload-Length must be a non-negative integer")
	}
	md, err := parseMetadata(r.Header.Get("Upload-Metadata"))
	if err != nil {
		return err
	}
	if md["filename"] == "" {
		return core.Validation(map[string]string{"filename": "Upload-Metadata filename is required"})
	}
	if md["space_id"] == "" {
		return core.Validation(map[string]string{"space_id": "Upload-Metadata space_id is required"})
	}
	meta := uploadMeta{Overwrite: metaBool(md["overwrite"]), ModTime: md["mod_time"], Category: strings.TrimSpace(md["category"]),
		Subcategory: strings.TrimSpace(md["subcategory"]), Mime: md["mime"]}
	if t := md["tags"]; t != "" {
		meta.Tags = strings.Split(t, ",")
	}
	tgt, err := m.svc.prepareUpload(ctx, p, md["space_id"], md["path"], md["filename"], length, meta, false)
	if err != nil {
		return err
	}
	var active int
	if err := m.d.DB.SQL.QueryRowContext(ctx, `SELECT count(*) FROM uploads WHERE user_id = ? AND state = 'active'`, p.UserID).Scan(&active); err != nil {
		return err
	}
	if active >= maxActiveUploads {
		return core.Conflict("Too many incomplete uploads; finish or cancel some first")
	}
	root, err := m.svc.dataRoot()
	if err != nil {
		return err
	}
	if err := root.MkdirAll(filepath.Join("tmp", "uploads"), 0o700); err != nil {
		return err
	}
	id := core.NewID()
	f, err := root.OpenFile(partRel(id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	now := m.svc.now()
	exp := now.Add(time.Duration(m.svc.filesSettings(ctx).UploadExpiryHours) * time.Hour)
	// Quota and free space are checked and the uploads row (the reservation) inserted under
	// one lock, so parallel creations cannot all pass against the same usage.
	_, err = m.svc.admit(ctx, tgt.owner, p.UserID, length, func() error {
		return m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO uploads (id, user_id, space_id, dir_path, filename, length, received, meta, state,
				created_at, updated_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, 0, ?, 'active', ?, ?, ?)`,
				id, p.UserID, tgt.sp.ID, tgt.dirPath, tgt.name, length, db.JSON[uploadMeta]{V: tgt.meta},
				db.FormatTime(now), db.FormatTime(now), db.FormatTime(exp))
			return err
		})
	})
	if err != nil {
		_ = root.Remove(partRel(id))
		return err
	}
	h := w.Header()
	setTusHeaders(h)
	h.Set("Location", "/api/v1/uploads/"+id)
	h.Set("Upload-Expires", exp.UTC().Format(http.TimeFormat))
	if length == 0 {
		u, err := m.svc.loadUpload(ctx, id, p.UserID)
		if err != nil {
			return err
		}
		e, err := m.svc.completeUpload(ctx, p, u)
		if err != nil {
			return err
		}
		setCompletionHeaders(h, e.ID, e.Path)
		h.Set("Upload-Offset", "0")
	}
	w.WriteHeader(http.StatusCreated)
	return nil
}

func setCompletionHeaders(h http.Header, id, path string) {
	h.Set("Ludworks-File-Id", id)
	h.Set("Ludworks-File-Path", escapeHeaderPath(path))
}

// escapeHeaderPath percent-encodes each path segment so the header stays ASCII.
func escapeHeaderPath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// activeUpload loads the caller's upload for HEAD/PATCH, answering 410 (and purging) when
// it expired.
func (s *Service) activeUpload(ctx context.Context, p *core.Principal, id string) (*upload, error) {
	u, err := s.loadUpload(ctx, id, p.UserID)
	if err != nil {
		return nil, err
	}
	if !s.now().Before(u.ExpiresAt) {
		s.dropUpload(context.WithoutCancel(ctx), u.ID)
		return nil, core.NewError(http.StatusGone, "gone", "The upload expired")
	}
	return u, nil
}

func (m *Module) tusHead(w http.ResponseWriter, r *http.Request) error {
	if err := checkTusResumable(w, r); err != nil {
		return err
	}
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return core.NotFound("Upload not found")
	}
	ctx := r.Context()
	u, err := m.svc.activeUpload(ctx, core.MustPrincipal(ctx), id)
	if err != nil {
		return err
	}
	h := w.Header()
	setTusHeaders(h)
	h.Set("Upload-Offset", strconv.FormatInt(u.Received, 10))
	h.Set("Upload-Length", strconv.FormatInt(u.Length, 10))
	h.Set("Upload-Expires", u.ExpiresAt.UTC().Format(http.TimeFormat))
	if u.State == "completed" {
		setCompletionHeaders(h, u.FileID, u.FilePath)
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

// parseChecksum parses "Upload-Checksum: <algo> <base64>".
func parseChecksum(v string) (hash.Hash, []byte, error) {
	if v == "" {
		return nil, nil, nil
	}
	algo, b64, ok := strings.Cut(strings.TrimSpace(v), " ")
	if !ok {
		return nil, nil, core.BadRequest("Upload-Checksum must be '<algorithm> <base64 digest>'")
	}
	sum, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, nil, core.BadRequest("Upload-Checksum digest is not valid base64")
	}
	var h hash.Hash
	switch strings.ToLower(algo) {
	case "sha1":
		h = sha1.New() //nolint:gosec
	case "sha256":
		h = sha256.New()
	default:
		return nil, nil, core.BadRequest("Unsupported checksum algorithm (use sha1 or sha256)")
	}
	if len(sum) != h.Size() {
		return nil, nil, core.BadRequest("Upload-Checksum digest has the wrong length")
	}
	return h, sum, nil
}

// deadlineReader extends the connection read deadline while data keeps arriving, so a
// stalled client cannot hold an upload lock forever (the server has no read timeout).
type deadlineReader struct {
	r    io.Reader
	rc   *http.ResponseController
	last time.Time
}

func (d *deadlineReader) Read(p []byte) (int, error) {
	if now := time.Now(); now.Sub(d.last) > 5*time.Second {
		_ = d.rc.SetReadDeadline(now.Add(bodyIdleTimeout))
		d.last = now
	}
	return d.r.Read(p)
}

var errChecksum = core.NewError(460, "checksum_mismatch", "Upload-Checksum does not match the received data; the chunk was discarded")

func (m *Module) tusPatch(w http.ResponseWriter, r *http.Request) {
	if err := m.tusPatchErr(w, r); err != nil {
		httpx.WriteError(w, r, err)
	}
}

func (m *Module) tusPatchErr(w http.ResponseWriter, r *http.Request) error {
	if err := checkTusResumable(w, r); err != nil {
		return err
	}
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return core.NotFound("Upload not found")
	}
	if ct := r.Header.Get("Content-Type"); !strings.EqualFold(strings.TrimSpace(strings.Split(ct, ";")[0]), "application/offset+octet-stream") {
		return core.NewError(http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/offset+octet-stream")
	}
	offset, err := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
	if err != nil || offset < 0 {
		return core.BadRequest("Upload-Offset must be a non-negative integer")
	}
	sum, want, err := parseChecksum(r.Header.Get("Upload-Checksum"))
	if err != nil {
		return err
	}
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	if _, err := m.svc.activeUpload(ctx, p, id); err != nil {
		return err
	}
	lock := m.svc.uploadLock(id)
	if !lock.TryLock() {
		return core.NewError(http.StatusLocked, "locked", "Another request is currently writing to this upload")
	}
	defer lock.Unlock()
	u, err := m.svc.activeUpload(ctx, p, id) // fresh state under the lock
	if err != nil {
		return err
	}
	h := w.Header()
	setTusHeaders(h)
	if u.State == "completed" {
		if offset != u.Length {
			h.Set("Upload-Offset", strconv.FormatInt(u.Length, 10))
			return core.Conflict("The upload is already complete")
		}
		h.Set("Upload-Offset", strconv.FormatInt(u.Length, 10))
		setCompletionHeaders(h, u.FileID, u.FilePath)
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	if offset != u.Received {
		h.Set("Upload-Offset", strconv.FormatInt(u.Received, 10))
		return core.Conflict("Upload-Offset does not match the current offset")
	}
	remaining := u.Length - u.Received
	if r.ContentLength > remaining {
		return core.NewError(http.StatusRequestEntityTooLarge, "payload_too_large", "The chunk exceeds the declared Upload-Length")
	}

	if remaining > 0 && r.ContentLength != 0 {
		n, err := m.svc.writeChunk(r, w, u, remaining, sum, want)
		if n > 0 {
			u.Received += n
		}
		if err != nil {
			return err
		}
	}
	if u.Received < u.Length {
		h.Set("Upload-Offset", strconv.FormatInt(u.Received, 10))
		h.Set("Upload-Expires", u.ExpiresAt.UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	e, err := m.svc.completeUpload(context.WithoutCancel(ctx), p, u)
	if err != nil {
		var ce *core.Error
		if errors.As(err, &ce) && (ce.Status == http.StatusConflict || ce.Status == http.StatusUnprocessableEntity) {
			// The destination became unusable (name taken meanwhile): the upload cannot
			// complete as declared, so it is discarded; the client recreates it.
			m.svc.dropUpload(context.WithoutCancel(ctx), u.ID)
		}
		return err
	}
	h.Set("Upload-Offset", strconv.FormatInt(u.Length, 10))
	setCompletionHeaders(h, e.ID, e.Path)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// writeChunk appends the request body to the .part file at u.Received. On a checksum
// mismatch, an oversize body or an interrupted checksummed chunk nothing is kept. It returns
// the number of bytes durably appended.
func (s *Service) writeChunk(r *http.Request, w http.ResponseWriter, u *upload, remaining int64, sum hash.Hash, want []byte) (int64, error) {
	root, err := s.dataRoot()
	if err != nil {
		return 0, err
	}
	f, err := root.OpenFile(partRel(u.ID), os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.dropUpload(context.WithoutCancel(r.Context()), u.ID)
			return 0, core.NewError(http.StatusGone, "gone", "The upload data is gone; start a new upload")
		}
		return 0, err
	}
	defer f.Close()
	if _, err := f.Seek(u.Received, io.SeekStart); err != nil {
		return 0, err
	}
	body := &deadlineReader{r: io.LimitReader(r.Body, remaining+1), rc: http.NewResponseController(w)}
	// The guard stops the chunk with 507 before it would write into the free-space headroom
	// (whatever other uploads promised or wrote meanwhile); the rules for interrupted chunks
	// below then apply: without a checksum the bytes written so far are kept.
	dst := s.newWriteGuard(nil, remaining).wrap(f)
	if sum != nil {
		dst = io.MultiWriter(dst, sum)
	}
	n, copyErr := io.CopyBuffer(dst, body, make([]byte, copyBuf))
	discard := func() { _ = f.Truncate(u.Received) }
	switch {
	case n > remaining:
		discard()
		return 0, core.NewError(http.StatusRequestEntityTooLarge, "payload_too_large", "The chunk exceeds the declared Upload-Length")
	case sum != nil && copyErr != nil:
		discard()
		return 0, mapDiskErr(copyErr)
	case sum != nil && subtle.ConstantTimeCompare(sum.Sum(nil), want) != 1:
		discard()
		return 0, errChecksum
	case copyErr != nil && n == 0:
		return 0, mapDiskErr(copyErr)
	}
	if err := f.Sync(); err != nil {
		discard()
		return 0, mapDiskErr(err)
	}
	// The client may already be gone (interrupted chunk): record what arrived regardless.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer cancel()
	now := s.now()
	exp := now.Add(time.Duration(s.filesSettings(ctx).UploadExpiryHours) * time.Hour)
	if exp.Before(u.ExpiresAt) {
		exp = u.ExpiresAt
	}
	dbErr := s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE uploads SET received = ?, updated_at = ?, expires_at = ? WHERE id = ?`,
			u.Received+n, db.FormatTime(now), db.FormatTime(exp), u.ID)
		return err
	})
	if dbErr != nil {
		discard()
		return 0, dbErr
	}
	u.ExpiresAt = exp
	if copyErr != nil { // partial chunk kept (no checksum); the client resumes from HEAD
		return n, mapDiskErr(copyErr)
	}
	return n, nil
}

// completeUpload commits a fully received upload into its space.
func (s *Service) completeUpload(ctx context.Context, p *core.Principal, u *upload) (*Entry, error) {
	opts := CreateOpts{Overwrite: u.Meta.Overwrite, Category: u.Meta.Category, Subcategory: u.Meta.Subcategory, Tags: u.Meta.Tags}
	if u.Meta.ModTime != "" {
		opts.ModTime, _ = timeParam("mod_time", u.Meta.ModTime)
	}
	e, err := s.commit(ctx, p, commitReq{SpaceID: u.SpaceID, DirPath: u.DirPath, Name: u.Filename, TmpPath: s.partAbs(u.ID),
		Opts: opts, Mime: u.Meta.Mime, ExclUpload: u.ID})
	if err != nil {
		return nil, err
	}
	now := s.now()
	exp := now.Add(completedRetention)
	if err := s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE uploads SET state = 'completed', received = length, file_id = ?, file_path = ?,
			updated_at = ?, expires_at = ? WHERE id = ?`, e.ID, e.Path, db.FormatTime(now), db.FormatTime(exp), u.ID)
		return err
	}); err != nil {
		s.log.Warn("marking upload complete failed", "upload", u.ID, "err", err)
	}
	return e, nil
}

// dropUpload deletes an upload row and its data file.
func (s *Service) dropUpload(ctx context.Context, id string) {
	_ = s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM uploads WHERE id = ?`, id)
		return err
	})
	if root, err := s.dataRoot(); err == nil {
		_ = root.Remove(partRel(id))
	}
	s.dropUploadLock(id)
}

func (m *Module) tusDelete(w http.ResponseWriter, r *http.Request) error {
	if err := checkTusResumable(w, r); err != nil {
		return err
	}
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return core.NotFound("Upload not found")
	}
	ctx := r.Context()
	u, err := m.svc.loadUpload(ctx, id, core.MustPrincipal(ctx).UserID)
	if err != nil {
		return err
	}
	lock := m.svc.uploadLock(u.ID)
	if !lock.TryLock() {
		return core.NewError(http.StatusLocked, "locked", "The upload is currently receiving data")
	}
	m.svc.dropUpload(context.WithoutCancel(ctx), u.ID)
	lock.Unlock()
	setTusHeaders(w.Header())
	w.WriteHeader(http.StatusNoContent)
	return nil
}

type apiUpload struct {
	ID        string `json:"id"`
	Filename  string `json:"filename"`
	SpaceID   string `json:"space_id"`
	Path      string `json:"path"`
	Offset    int64  `json:"offset"`
	Length    int64  `json:"length"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
}

func (m *Module) listUploads(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	rs, err := m.d.DB.SQL.QueryContext(ctx, `SELECT `+uploadCols+` FROM uploads WHERE user_id = ? AND state = 'active' AND expires_at > ?
		ORDER BY created_at DESC LIMIT 1000`, p.UserID, db.FormatTime(m.svc.now()))
	if err != nil {
		return err
	}
	defer rs.Close()
	out := []apiUpload{}
	for rs.Next() {
		u, err := scanUpload(rs)
		if err != nil {
			return err
		}
		out = append(out, apiUpload{ID: u.ID, Filename: u.Filename, SpaceID: u.SpaceID, Path: u.DirPath, Offset: u.Received,
			Length: u.Length, CreatedAt: db.FormatTime(u.CreatedAt), ExpiresAt: db.FormatTime(u.ExpiresAt)})
	}
	if err := rs.Err(); err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

// purgeUploads removes expired uploads and orphaned data files.
func (s *Service) purgeUploads(ctx context.Context) error {
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT id FROM uploads WHERE expires_at <= ? LIMIT 10000`, db.FormatTime(s.now()))
	if err != nil {
		return err
	}
	var ids []string
	for rs.Next() {
		var id string
		if err := rs.Scan(&id); err != nil {
			rs.Close()
			return err
		}
		ids = append(ids, id)
	}
	rs.Close()
	for _, id := range ids {
		lock := s.uploadLock(id)
		if lock.TryLock() {
			s.dropUpload(ctx, id)
			lock.Unlock()
		}
	}
	// Orphaned .part files (e.g. rows removed by a cascade) and stale staging leftovers.
	dir := filepath.Join(s.TempDir(), "uploads")
	if ents, err := os.ReadDir(dir); err == nil {
		for _, e := range ents {
			id, ok := strings.CutSuffix(e.Name(), ".part")
			if !ok {
				continue
			}
			var n int
			if err := s.d.DB.SQL.QueryRowContext(ctx, `SELECT count(*) FROM uploads WHERE id = ?`, id).Scan(&n); err != nil || n > 0 {
				continue
			}
			if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > time.Hour {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	if ents, err := os.ReadDir(s.TempDir()); err == nil {
		root, rerr := s.dataRoot()
		for _, e := range ents {
			n := e.Name()
			if rerr != nil || !(strings.HasPrefix(n, "copy-") || strings.HasPrefix(n, "delete-") || strings.HasPrefix(n, "create-") || strings.HasPrefix(n, "upload-")) {
				continue
			}
			if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > 24*time.Hour {
				_ = root.RemoveAll(filepath.Join("tmp", n))
			}
		}
	}
	return nil
}

/* ----------------------------------------------------------- simple upload */

func (m *Module) simpleUpload(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	q := r.URL.Query()
	if r.ContentLength < 0 {
		return core.BadRequest("Content-Length is required")
	}
	name := q.Get("name")
	if name == "" {
		return core.Validation(map[string]string{"name": "required"})
	}
	meta := uploadMeta{Overwrite: httpx.QueryBool(r, "overwrite"), ModTime: q.Get("mod_time"),
		Category: strings.TrimSpace(q.Get("category")), Subcategory: strings.TrimSpace(q.Get("subcategory"))}
	tgt, err := m.svc.prepareUpload(ctx, p, q.Get("space_id"), q.Get("path"), name, r.ContentLength, meta, true)
	if err != nil {
		return err
	}
	// Reserve the declared length against quota and free space while the body streams, so
	// parallel uploads cannot all pass against the same usage.
	res, err := m.svc.admit(ctx, tgt.owner, p.UserID, r.ContentLength, nil)
	if err != nil {
		return err
	}
	defer m.svc.release(res)
	tmp, err := m.svc.createTemp("upload-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	body := &deadlineReader{r: io.LimitReader(r.Body, r.ContentLength+1), rc: http.NewResponseController(w)}
	n, err := io.CopyBuffer(m.svc.newWriteGuard(res, r.ContentLength).wrap(tmp), body, make([]byte, copyBuf))
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return mapDiskErr(err)
	}
	if n != r.ContentLength {
		return core.BadRequest("The body length does not match Content-Length")
	}
	opts := CreateOpts{Overwrite: tgt.meta.Overwrite, Category: tgt.meta.Category, Subcategory: tgt.meta.Subcategory}
	if tgt.meta.ModTime != "" {
		opts.ModTime, _ = timeParam("mod_time", tgt.meta.ModTime)
	}
	e, err := m.svc.commit(context.WithoutCancel(ctx), p, commitReq{SpaceID: tgt.sp.ID, DirPath: tgt.dirPath, Name: tgt.name,
		TmpPath: tmpName, Opts: opts, Mime: r.Header.Get("Content-Type"), Res: res})
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusCreated, m.svc.entryAPI(ctx, e))
}
