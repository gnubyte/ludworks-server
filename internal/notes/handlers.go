package notes

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
)

// Request size limits per route.
const (
	maxNoteRequest = 16 << 20 // JSON with a 5 MiB body (escaping can inflate it)
)

// optional captures presence and null-ness of a JSON field.
type optional[T any] struct {
	Set  bool
	Null bool
	V    T
}

func (o *optional[T]) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Null = true
		return nil
	}
	return json.Unmarshal(b, &o.V)
}

func uid(r *http.Request) string { return core.MustPrincipal(r.Context()).UserID }

func idPtr(o optional[string], field string) (*string, error) {
	if !o.Set || o.Null || o.V == "" {
		return nil, nil
	}
	if !httpx.ValidID(o.V) {
		return nil, core.Validation(map[string]string{field: "invalid id"})
	}
	v := o.V
	return &v, nil
}

/* ------------------------------------------------------------------ folders */

func (m *Module) listFolders(w http.ResponseWriter, r *http.Request) error {
	fs, err := m.svc.ListFolders(r.Context(), uid(r))
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, fs)
}

func (m *Module) createFolder(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name     string           `json:"name"`
		ParentID optional[string] `json:"parent_id"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	parent, err := idPtr(in.ParentID, "parent_id")
	if err != nil {
		return err
	}
	f, err := m.svc.CreateFolder(r.Context(), uid(r), in.Name, parent)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusCreated, f)
}

func (m *Module) updateFolder(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Name     optional[string] `json:"name"`
		ParentID optional[string] `json:"parent_id"`
		Position optional[int]    `json:"position"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	var p FolderPatch
	if in.Name.Set && !in.Name.Null {
		p.Name = &in.Name.V
	}
	if in.ParentID.Set {
		p.ParentSet = true
		if p.ParentID, err = idPtr(in.ParentID, "parent_id"); err != nil {
			return err
		}
	}
	if in.Position.Set && !in.Position.Null {
		p.Position = &in.Position.V
	}
	f, err := m.svc.UpdateFolder(r.Context(), uid(r), id, p)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, f)
}

func (m *Module) deleteFolder(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	if err := m.svc.DeleteFolder(r.Context(), uid(r), id); err != nil {
		return err
	}
	return httpx.NoContent(w)
}

/* -------------------------------------------------------------------- notes */

func (m *Module) listNotes(w http.ResponseWriter, r *http.Request) error {
	qv := r.URL.Query()
	q := ListQuery{
		FolderID: qv.Get("folder_id"),
		Q:        qv.Get("q"),
		Tag:      qv.Get("tag"),
		Trashed:  httpx.QueryBool(r, "trashed"),
		Sort:     qv.Get("sort"),
		Order:    qv.Get("order"),
		Cursor:   qv.Get("cursor"),
		Limit:    httpx.ParseLimit(r, 50, 200),
	}
	if qv.Has("pinned") && qv.Get("pinned") != "" {
		b := httpx.QueryBool(r, "pinned")
		q.Pinned = &b
	}
	items, next, err := m.svc.List(r.Context(), uid(r), q)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, httpx.NewPage(items, next))
}

type noteBody struct {
	Title       optional[string]   `json:"title"`
	Body        optional[string]   `json:"body"`
	FolderID    optional[string]   `json:"folder_id"`
	Tags        optional[[]string] `json:"tags"`
	Pinned      optional[bool]     `json:"pinned"`
	BaseVersion optional[int64]    `json:"base_version"`
}

func (m *Module) createNote(w http.ResponseWriter, r *http.Request) error {
	var in noteBody
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	folder, err := idPtr(in.FolderID, "folder_id")
	if err != nil {
		return err
	}
	ni := NoteInput{Body: in.Body.V, FolderID: folder, Tags: in.Tags.V, Pinned: in.Pinned.V}
	if in.Title.Set && !in.Title.Null {
		ni.Title = &in.Title.V
	}
	n, err := m.svc.Create(r.Context(), uid(r), ni)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusCreated, n)
}

func (m *Module) getNote(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	n, err := m.svc.Get(r.Context(), uid(r), id)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, n)
}

func (m *Module) updateNote(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in noteBody
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	var p NotePatch
	if in.Title.Set {
		t := in.Title.V // null clears the explicit title, like ""
		p.Title = &t
	}
	if in.Body.Set && !in.Body.Null {
		p.Body = &in.Body.V
	}
	if in.FolderID.Set {
		p.FolderSet = true
		if p.FolderID, err = idPtr(in.FolderID, "folder_id"); err != nil {
			return err
		}
	}
	if in.Tags.Set {
		tags := in.Tags.V
		if tags == nil {
			tags = []string{}
		}
		p.Tags = &tags
	}
	if in.Pinned.Set && !in.Pinned.Null {
		p.Pinned = &in.Pinned.V
	}
	if in.BaseVersion.Set && !in.BaseVersion.Null {
		p.BaseVersion = &in.BaseVersion.V
	}
	n, err := m.svc.Update(r.Context(), uid(r), id, p)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, n)
}

func (m *Module) deleteNote(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	if err := m.svc.Delete(r.Context(), uid(r), id, httpx.QueryBool(r, "permanent")); err != nil {
		return err
	}
	return httpx.NoContent(w)
}

func (m *Module) restoreNote(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	n, err := m.svc.Restore(r.Context(), uid(r), id)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, n)
}

func (m *Module) listTags(w http.ResponseWriter, r *http.Request) error {
	tags, err := m.svc.Tags(r.Context(), uid(r))
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, tags)
}

func (m *Module) changes(w http.ResponseWriter, r *http.Request) error {
	var since *time.Time
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := db.ParseTime(v)
		if err != nil {
			return core.BadRequest("since must be an RFC 3339 timestamp")
		}
		since = &t
	}
	started := false
	err := m.svc.writeChanges(r.Context(), uid(r), since, w, func() {
		started = true
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
	})
	if err != nil && started {
		m.d.Log.Warn("notes: changes stream failed", "request_id", core.RequestID(r.Context()), "err", err)
		return nil
	}
	return err
}

/* ----------------------------------------------------------- import/export */

func (m *Module) importNotes(w http.ResponseWriter, r *http.Request) error {
	qv := r.URL.Query()
	var folder *string
	if v := qv.Get("folder_id"); v != "" {
		if !httpx.ValidID(v) {
			return core.NotFound("folder not found")
		}
		folder = &v
	}
	filename := qv.Get("filename")
	if len(filename) > 255 {
		return core.Validation(map[string]string{"filename": "must be at most 255 bytes"})
	}
	if r.Body == nil || r.Body == http.NoBody {
		return core.BadRequest("request body required")
	}
	tmp, err := os.CreateTemp(m.d.Config.TmpDir(), "notes-import-*")
	if err != nil {
		return err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()
	n, err := io.Copy(tmp, io.LimitReader(r.Body, MaxImportBytes+1))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return core.NewError(http.StatusRequestEntityTooLarge, "payload_too_large", "Imports are limited to 50 MiB")
		}
		return storageError(err)
	}
	if n > MaxImportBytes {
		return core.NewError(http.StatusRequestEntityTooLarge, "payload_too_large", "Imports are limited to 50 MiB")
	}
	if n == 0 {
		return core.BadRequest("the file is empty")
	}
	res, err := m.svc.Import(r.Context(), uid(r), folder, filename, tmp, n)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, res)
}

func (m *Module) exportNotes(w http.ResponseWriter, r *http.Request) error {
	qv := r.URL.Query()
	if f := qv.Get("format"); f != "" && f != "zip" {
		return core.BadRequest("format must be zip")
	}
	var folder *string
	if v := qv.Get("folder_id"); v != "" {
		if !httpx.ValidID(v) {
			return core.NotFound("folder not found")
		}
		folder = &v
	}
	ctx := r.Context()
	name, err := m.svc.ExportArchiveName(ctx, uid(r), folder)
	if err != nil {
		return err
	}
	h := w.Header()
	h.Set("Content-Type", "application/zip")
	h.Set("Content-Disposition", contentDisposition("attachment", name))
	h.Set("Cache-Control", "no-store")
	cw := &countingWriter{w: w}
	if err := m.svc.ExportZip(ctx, uid(r), folder, cw); err != nil {
		if cw.n == 0 && !httpx.HeaderWritten(w) {
			return err
		}
		m.d.Log.Warn("notes: export stream failed", "request_id", core.RequestID(ctx), "err", err)
		panic(http.ErrAbortHandler) // abort the connection so the client sees a truncated download
	}
	return nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// noteFile serves GET /notes/{id}/{file}; only "export.md" exists. (A dedicated
// /notes/{id}/export.md pattern would conflict with /notes/attachments/{id} in ServeMux.)
func (m *Module) noteFile(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	if r.PathValue("file") != "export.md" {
		return core.ErrNotFound
	}
	b, name, err := m.svc.ExportNote(r.Context(), uid(r), id)
	if err != nil {
		return err
	}
	h := w.Header()
	h.Set("Content-Type", "text/markdown; charset=utf-8")
	h.Set("Content-Disposition", contentDisposition("attachment", name))
	_, err = w.Write(b)
	return err
}

/* ------------------------------------------------------------- attachments */

func (m *Module) uploadAttachment(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	filename := r.URL.Query().Get("filename")
	if strings.TrimSpace(filename) == "" {
		return core.Validation(map[string]string{"filename": "is required"})
	}
	if r.Body == nil || r.Body == http.NoBody {
		return core.BadRequest("the file is empty")
	}
	a, err := m.svc.AddAttachment(r.Context(), uid(r), id, filename, r.Body)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusCreated, a)
}

func (m *Module) getAttachment(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	f, a, err := m.svc.OpenAttachment(r.Context(), uid(r), id)
	if err != nil {
		return err
	}
	defer f.Close()
	serveAttachment(w, r, f, a)
	return nil
}
