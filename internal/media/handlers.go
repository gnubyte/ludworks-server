package media

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/httpx"
)

func principal(r *http.Request) *core.Principal { return core.MustPrincipal(r.Context()) }

func fileID(r *http.Request) (string, error) {
	id, err := httpx.PathID(r, "file_id")
	if err != nil {
		return "", errNotMedia
	}
	return id, nil
}

// GET /media
func (m *Module) list(w http.ResponseWriter, r *http.Request) error {
	lq, err := parseListQuery(&requestValues{q: r.URL.Query(), limit: httpx.ParseLimit(r, 50, 200)})
	if err != nil {
		return err
	}
	items, next, err := m.svc.list(r.Context(), principal(r), lq)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, httpx.NewPage(items, next))
}

// GET /media/facets
func (m *Module) facets(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	f, err := parseFilter(q, false)
	if err != nil {
		return err
	}
	if v := q.Get("space_id"); v != "" {
		if !httpx.ValidID(v) {
			return core.NotFound("Space not found")
		}
		f.SpaceID = v
	}
	out, err := m.svc.facets(r.Context(), principal(r), f)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

// GET /media/shows
func (m *Module) shows(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query().Get("q")
	if len(q) > 500 {
		return core.BadRequest("q: too long")
	}
	items, next, err := m.svc.shows(r.Context(), principal(r), q, r.URL.Query().Get("cursor"), httpx.ParseLimit(r, 50, 200))
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, httpx.NewPage(items, next))
}

// GET /media/shows/{show_key}
func (m *Module) show(w http.ResponseWriter, r *http.Request) error {
	out, err := m.svc.showDetail(r.Context(), principal(r), r.PathValue("show_key"))
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

// GET /media/{file_id}
func (m *Module) get(w http.ResponseWriter, r *http.Request) error {
	id, err := fileID(r)
	if err != nil {
		return err
	}
	p := principal(r)
	if _, err := m.svc.itemFor(r.Context(), p, id, needRead); err != nil {
		return err
	}
	out, err := m.svc.mediaJSON(r.Context(), p, id)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

// PATCH /media/{file_id}
func (m *Module) patch(w http.ResponseWriter, r *http.Request) error {
	id, err := fileID(r)
	if err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := httpx.DecodeJSON(r, &raw); err != nil {
		return err
	}
	if raw == nil {
		return core.BadRequest("expected a JSON object")
	}
	pt, err := parsePatch(raw, true)
	if err != nil {
		return err
	}
	out, err := m.svc.update(r.Context(), principal(r), id, pt)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

// POST /media/bulk
func (m *Module) bulk(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		FileIDs []string                   `json:"file_ids"`
		Set     map[string]json.RawMessage `json:"set"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	fe := map[string]string{}
	switch {
	case len(req.FileIDs) == 0:
		fe["file_ids"] = "required"
	case len(req.FileIDs) > maxBulkIDs:
		fe["file_ids"] = "at most 1000 ids"
	}
	for _, id := range req.FileIDs {
		if !httpx.ValidID(id) {
			fe["file_ids"] = "invalid id"
			break
		}
	}
	if req.Set == nil {
		fe["set"] = "required"
	}
	if len(fe) > 0 {
		return core.Validation(fe)
	}
	pt, err := parsePatch(req.Set, true)
	if err != nil {
		var ce *core.Error
		if asCoreError(err, &ce) && ce.Details != nil {
			return prefixFields(ce, "set.")
		}
		return err
	}
	if pt.empty() {
		return core.Validation(map[string]string{"set": "no fields to change"})
	}
	updated, errs, err := m.svc.bulkUpdate(r.Context(), principal(r), req.FileIDs, pt)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, map[string]any{"updated": updated, "errors": errs})
}

// POST /media/auto-tag
func (m *Module) autoTag(w http.ResponseWriter, r *http.Request) error {
	var req AutoTagRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	if req.SpaceID == "" {
		return core.Validation(map[string]string{"space_id": "required"})
	}
	out, err := m.svc.autoTag(r.Context(), principal(r), req)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

// POST /media/{file_id}/probe
func (m *Module) probe(w http.ResponseWriter, r *http.Request) error {
	id, err := fileID(r)
	if err != nil {
		return err
	}
	p := principal(r)
	it, err := m.svc.itemFor(r.Context(), p, id, needWrite)
	if err != nil {
		return err
	}
	// Probing reads (and for crafted files can burn) a fair amount of CPU: one manual probe
	// per user at a time, a few server-wide.
	release, ok := m.svc.probes.tryAcquire(p.UserID)
	if !ok {
		w.Header().Set("Retry-After", "5")
		return errProbeBusy
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if it.File.ParentID != "" {
		if err := m.svc.refreshSidecars(ctx, it.File.SpaceID, it.File.ParentID); err != nil {
			return err
		}
	}
	if err := m.svc.probeItem(ctx, id); err != nil && ctx.Err() != nil {
		return err
	}
	out, err := m.svc.mediaJSON(r.Context(), p, id)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

// POST /media/{file_id}/identify
func (m *Module) identify(w http.ResponseWriter, r *http.Request) error {
	id, err := fileID(r)
	if err != nil {
		return err
	}
	var req IdentifyRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	out, err := m.svc.identify(r.Context(), principal(r), id, req)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

// GET /media/tmdb/search
func (m *Module) tmdbSearch(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	query := strings.TrimSpace(q.Get("q"))
	if query == "" || len(query) > 200 {
		return core.BadRequest("q: required (1-200 characters)")
	}
	typ := q.Get("type")
	if typ == "" {
		typ = "movie"
	}
	if typ != "movie" && typ != "tv" {
		return core.BadRequest("type: must be movie or tv")
	}
	year := 0
	if v := q.Get("year"); v != "" {
		y, err := strconv.Atoi(v)
		if err != nil || y < 1800 || y > 3000 {
			return core.BadRequest("year: must be a year")
		}
		year = y
	}
	results, err := m.svc.tmdbSearch(r.Context(), query, typ, year)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, map[string]any{"results": results})
}

// PUT /media/{file_id}/poster
func (m *Module) putPoster(w http.ResponseWriter, r *http.Request) error {
	id, err := fileID(r)
	if err != nil {
		return err
	}
	if r.ContentLength > maxPosterUpload {
		return core.ErrPayloadTooLarge
	}
	body := http.MaxBytesReader(w, r.Body, maxPosterUpload)
	out, err := m.svc.putPoster(r.Context(), principal(r), id, body)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

// DELETE /media/{file_id}/poster
func (m *Module) deletePoster(w http.ResponseWriter, r *http.Request) error {
	return m.deleteImage(w, r, kindPoster)
}

// DELETE /media/{file_id}/backdrop
func (m *Module) deleteBackdrop(w http.ResponseWriter, r *http.Request) error {
	return m.deleteImage(w, r, kindBackdrop)
}

func (m *Module) deleteImage(w http.ResponseWriter, r *http.Request, kind string) error {
	id, err := fileID(r)
	if err != nil {
		return err
	}
	if _, err := m.svc.itemFor(r.Context(), principal(r), id, needWrite); err != nil {
		return err
	}
	if err := m.svc.clearImage(r.Context(), id, kind); err != nil {
		return err
	}
	return httpx.NoContent(w)
}

// PUT /media/{file_id}/progress
func (m *Module) progress(w http.ResponseWriter, r *http.Request) error {
	id, err := fileID(r)
	if err != nil {
		return err
	}
	var u ProgressUpdate
	if err := httpx.DecodeJSON(r, &u); err != nil {
		return err
	}
	if err := m.svc.setProgress(r.Context(), principal(r), id, u); err != nil {
		return err
	}
	return httpx.NoContent(w)
}

// GET /media/{file_id}/{asset}: poster | backdrop | stream | transcode.
func (m *Module) asset(w http.ResponseWriter, r *http.Request) error {
	id, err := fileID(r)
	if err != nil {
		return err
	}
	p := principal(r)
	switch r.PathValue("asset") {
	case "poster":
		return m.svc.ServePoster(w, r, p, id, r.URL.Query().Get("size"))
	case "backdrop":
		return m.svc.serveBackdrop(w, r, p, id, r.URL.Query().Get("size"))
	case "stream":
		return m.svc.ServeStream(w, r, p, id)
	case "transcode":
		start, height, err := ParseTranscodeParams(r)
		if err != nil {
			return err
		}
		return m.svc.ServeTranscode(w, r, p, id, start, height)
	}
	return core.ErrNotFound
}

// GET /media/{file_id}/subtitles/{index}.vtt
func (m *Module) subtitle(w http.ResponseWriter, r *http.Request) error {
	id, err := fileID(r)
	if err != nil {
		return err
	}
	index, ok := trimVTT(r.PathValue("name"))
	if !ok {
		return core.NotFound("Subtitle track not found")
	}
	return m.svc.ServeSubtitle(w, r, principal(r), id, index)
}

func asCoreError(err error, target **core.Error) bool {
	ce, ok := err.(*core.Error)
	if ok {
		*target = ce
	}
	return ok
}

// prefixFields prefixes the field names of a validation error.
func prefixFields(ce *core.Error, prefix string) *core.Error {
	d, ok := ce.Details.(map[string]any)
	if !ok {
		return ce
	}
	fields, ok := d["fields"].(map[string]string)
	if !ok {
		return ce
	}
	out := make(map[string]string, len(fields))
	for k, v := range fields {
		out[prefix+k] = v
	}
	return core.Validation(out)
}
