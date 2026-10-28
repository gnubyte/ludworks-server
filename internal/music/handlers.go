package music

import (
	"encoding/json"
	"net/http"
	"strconv"

	"ludworks/internal/core"
	"ludworks/internal/httpx"
)

func principal(r *http.Request) *core.Principal { return core.MustPrincipal(r.Context()) }

// decodeOptional decodes an optional JSON body: an empty body leaves dst unchanged.
func decodeOptional(r *http.Request, dst any) error {
	if r.ContentLength == 0 || r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	err := httpx.DecodeJSON(r, dst)
	if ce, ok := err.(*core.Error); ok && ce.Message == "request body required" {
		return nil
	}
	return err
}

// trackID reads the {id} path value of /music/tracks/{id}… (invalid ids are 404).
func trackID(r *http.Request) (string, error) {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return "", errNotTrack
	}
	return id, nil
}

/* ------------------------------------------------------------------- tracks */

// GET /music/tracks
func (m *Module) listTracks(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	f, err := parseTrackFilter(q)
	if err != nil {
		return err
	}
	lq, err := parseListQuery(q, httpx.ParseLimit(r, 50, 200), trackSorts, "title")
	if err != nil {
		return err
	}
	items, next, err := m.svc.listTracks(r.Context(), principal(r), f, lq)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, httpx.NewPage(items, next))
}

// POST /music/tracks/lookup
func (m *Module) lookupTracks(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	if len(req.IDs) > maxLookupIDs {
		return core.Validation(map[string]string{"ids": "at most 500 ids"})
	}
	ctx := r.Context()
	p := principal(r)
	var valid []string
	for _, id := range req.IDs {
		if httpx.ValidID(id) {
			valid = append(valid, id)
		}
	}
	trs, err := m.svc.visibleTracks(ctx, p, uniq(valid))
	if err != nil {
		return err
	}
	found := map[string]bool{}
	for _, tr := range trs {
		found[tr.FileID] = true
	}
	missing := []string{}
	seen := map[string]bool{}
	for _, id := range req.IDs {
		if !found[id] && !seen[id] {
			seen[id] = true
			missing = append(missing, id)
		}
	}
	return httpx.WriteJSON(w, http.StatusOK, map[string]any{"tracks": m.svc.renderTracks(ctx, trs), "missing": missing})
}

// POST /music/tracks/bulk
func (m *Module) bulkTracks(w http.ResponseWriter, r *http.Request) error {
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
	tp, err := parseTrackPatch(req.Set)
	if err != nil {
		return prefixFields(err, "set.")
	}
	if tp.empty() {
		return core.Validation(map[string]string{"set": "no fields to change"})
	}
	updated, errs, err := m.svc.bulkPatch(r.Context(), principal(r), req.FileIDs, tp)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, map[string]any{"updated": updated, "errors": errs})
}

// prefixFields prefixes the field names of a validation error.
func prefixFields(err error, prefix string) error {
	ce, ok := err.(*core.Error)
	if !ok {
		return err
	}
	d, ok := ce.Details.(map[string]any)
	if !ok {
		return err
	}
	fields, ok := d["fields"].(map[string]string)
	if !ok {
		return err
	}
	out := make(map[string]string, len(fields))
	for k, v := range fields {
		out[prefix+k] = v
	}
	return core.Validation(out)
}

// GET /music/tracks/{id}
func (m *Module) getTrack(w http.ResponseWriter, r *http.Request) error {
	id, err := trackID(r)
	if err != nil {
		return err
	}
	tr, err := m.svc.trackFor(r.Context(), principal(r), id, needRead)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, tr.toAPI(m.svc.settings(r.Context()).FolderArt))
}

// PATCH /music/tracks/{id}
func (m *Module) patchTrack(w http.ResponseWriter, r *http.Request) error {
	id, err := trackID(r)
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
	tp, err := parseTrackPatch(raw)
	if err != nil {
		return err
	}
	ctx := r.Context()
	p := principal(r)
	if err := m.svc.applyPatch(ctx, p, id, tp); err != nil {
		return err
	}
	out, err := m.svc.trackJSON(ctx, p, id)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

// POST /music/tracks/{id}/probe
func (m *Module) probeTrack(w http.ResponseWriter, r *http.Request) error {
	id, err := trackID(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	p := principal(r)
	if err := m.svc.reprobe(ctx, p, id, httpx.QueryBool(r, "reset")); err != nil {
		if err == errProbeBusy {
			w.Header().Set("Retry-After", "5")
		}
		return err
	}
	out, err := m.svc.trackJSON(ctx, p, id)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

// GET /music/tracks/{id}/stream
func (m *Module) streamTrack(w http.ResponseWriter, r *http.Request) error {
	id, err := trackID(r)
	if err != nil {
		return err
	}
	return m.svc.ServeStream(w, r, principal(r), id)
}

// GET /music/tracks/{id}/transcode
func (m *Module) transcodeTrack(w http.ResponseWriter, r *http.Request) error {
	id, err := trackID(r)
	if err != nil {
		return err
	}
	format, bitrate, start, err := transcodeParams(r)
	if err != nil {
		return err
	}
	return m.svc.ServeTranscode(w, r, principal(r), id, format, bitrate, start)
}

// GET /music/tracks/{id}/download
func (m *Module) downloadTrack(w http.ResponseWriter, r *http.Request) error {
	id, err := trackID(r)
	if err != nil {
		return err
	}
	p := principal(r)
	tr, err := m.svc.trackFor(r.Context(), p, id, needRead)
	if err != nil {
		return err
	}
	return m.svc.serveDownload(w, r, p, tr, r.URL.Query().Get("quality"))
}

// GET /music/tracks/{id}/art
func (m *Module) trackArt(w http.ResponseWriter, r *http.Request) error {
	id, err := trackID(r)
	if err != nil {
		return err
	}
	return m.svc.ServeArt(w, r, principal(r), id, r.URL.Query().Get("size"))
}

// GET /music/tracks/{id}/lyrics
func (m *Module) trackLyrics(w http.ResponseWriter, r *http.Request) error {
	id, err := trackID(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	p := principal(r)
	tr, err := m.svc.trackFor(ctx, p, id, needRead)
	if err != nil {
		return err
	}
	l, err := m.svc.lyrics(ctx, p, tr)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, l)
}

// PUT /music/tracks/{id}/rating
func (m *Module) rateTrack(w http.ResponseWriter, r *http.Request) error {
	id, err := trackID(r)
	if err != nil {
		return err
	}
	var req map[string]json.RawMessage
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	raw, ok := req["rating"]
	if !ok {
		return core.Validation(map[string]string{"rating": "required (1-5 or null)"})
	}
	var rating *int
	if !isNull(raw) {
		var f float64
		if err := json.Unmarshal(raw, &f); err != nil || f != float64(int(f)) || f < 1 || f > 5 {
			return core.Validation(map[string]string{"rating": "must be 1-5 or null"})
		}
		v := int(f)
		rating = &v
	}
	if err := m.svc.setRating(r.Context(), principal(r), id, rating); err != nil {
		return err
	}
	return httpx.NoContent(w)
}

// PUT /music/favorites
func (m *Module) setFavorite(w http.ResponseWriter, r *http.Request) error {
	var req FavoriteRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	if err := m.svc.setFavorite(r.Context(), principal(r), req); err != nil {
		return err
	}
	return httpx.NoContent(w)
}

/* ---------------------------------------------------------- albums, artists */

// GET /music/albums
func (m *Module) listAlbums(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	f, err := parseAlbumFilter(q)
	if err != nil {
		return err
	}
	lq, err := parseListQuery(q, httpx.ParseLimit(r, 50, 200), albumSorts, "title")
	if err != nil {
		return err
	}
	items, next, err := m.svc.listAlbums(r.Context(), principal(r), f, lq)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, httpx.NewPage(items, next))
}

// GET /music/albums/{id}
func (m *Module) getAlbum(w http.ResponseWriter, r *http.Request) error {
	out, err := m.svc.albumDetail(r.Context(), principal(r), r.PathValue("id"))
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

// GET /music/albums/{id}/art
func (m *Module) albumArt(w http.ResponseWriter, r *http.Request) error {
	size := r.URL.Query().Get("size")
	if _, _, err := parseArtSize(size); err != nil {
		return err
	}
	art, err := m.svc.albumArtID(r.Context(), principal(r), r.PathValue("id"))
	if err != nil {
		return err
	}
	return m.svc.serveArt(w, r, art, size)
}

// PUT /music/albums/{id}/art
func (m *Module) putAlbumArt(w http.ResponseWriter, r *http.Request) error {
	if r.ContentLength > maxArtUpload {
		return core.ErrPayloadTooLarge
	}
	body := http.MaxBytesReader(w, r.Body, maxArtUpload)
	out, err := m.svc.setAlbumArt(r.Context(), principal(r), r.PathValue("id"), body)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

// DELETE /music/albums/{id}/art
func (m *Module) deleteAlbumArt(w http.ResponseWriter, r *http.Request) error {
	if err := m.svc.clearAlbumArt(r.Context(), principal(r), r.PathValue("id")); err != nil {
		return err
	}
	return httpx.NoContent(w)
}

// GET /music/artists
func (m *Module) listArtists(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	f, err := parseArtistFilter(q)
	if err != nil {
		return err
	}
	lq, err := parseListQuery(q, httpx.ParseLimit(r, 50, 200), artistSorts, "name")
	if err != nil {
		return err
	}
	items, next, err := m.svc.listArtists(r.Context(), principal(r), f, lq)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, httpx.NewPage(items, next))
}

// GET /music/artists/{id}
func (m *Module) getArtist(w http.ResponseWriter, r *http.Request) error {
	out, err := m.svc.artistDetail(r.Context(), principal(r), r.PathValue("id"))
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

// GET /music/artists/{id}/art
func (m *Module) artistArt(w http.ResponseWriter, r *http.Request) error {
	size := r.URL.Query().Get("size")
	if _, _, err := parseArtSize(size); err != nil {
		return err
	}
	art, err := m.svc.artistArtID(r.Context(), principal(r), r.PathValue("id"))
	if err != nil {
		return err
	}
	return m.svc.serveArt(w, r, art, size)
}

// GET /music/genres
func (m *Module) genres(w http.ResponseWriter, r *http.Request) error {
	out, err := m.svc.genres(r.Context(), principal(r), r.URL.Query().Get("space_id"))
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

// GET /music/facets
func (m *Module) facets(w http.ResponseWriter, r *http.Request) error {
	out, err := m.svc.facets(r.Context(), principal(r), r.URL.Query().Get("space_id"))
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

// GET /music/search
func (m *Module) search(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query().Get("q")
	if len(q) > 500 {
		return core.BadRequest("q: too long")
	}
	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return core.BadRequest("limit: must be between 1 and 50")
		}
		limit = min(n, 50)
	}
	out, err := m.svc.search(r.Context(), principal(r), q, limit)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

// GET /music/home
func (m *Module) home(w http.ResponseWriter, r *http.Request) error {
	out, err := m.svc.home(r.Context(), principal(r))
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

/* -------------------------------------------------------------------- plays */

// POST /music/plays
func (m *Module) recordPlays(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Plays []PlayInput `json:"plays"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	plays, err := parsePlays(req.Plays)
	if err != nil {
		return err
	}
	recorded, ignored, err := m.svc.recordPlays(r.Context(), principal(r), plays)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, map[string]int{"recorded": recorded, "ignored": ignored})
}

// GET /music/history
func (m *Module) history(w http.ResponseWriter, r *http.Request) error {
	items, next, err := m.svc.history(r.Context(), principal(r), r.URL.Query().Get("cursor"), httpx.ParseLimit(r, 50, 200))
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, httpx.NewPage(items, next))
}

// DELETE /music/history
func (m *Module) clearHistory(w http.ResponseWriter, r *http.Request) error {
	if err := m.svc.clearHistory(r.Context(), principal(r)); err != nil {
		return err
	}
	return httpx.NoContent(w)
}

/* ---------------------------------------------------------------- playlists */

// GET /music/playlists
func (m *Module) listPlaylists(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	f, err := parsePlaylistFilter(q)
	if err != nil {
		return err
	}
	lq, err := parseListQuery(q, httpx.ParseLimit(r, 50, 200), playlistSorts, "name")
	if err != nil {
		return err
	}
	items, next, err := m.svc.listPlaylists(r.Context(), principal(r), f, lq)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, httpx.NewPage(items, next))
}

// POST /music/playlists
func (m *Module) createPlaylist(w http.ResponseWriter, r *http.Request) error {
	var req PlaylistCreate
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	out, err := m.svc.createPlaylist(r.Context(), principal(r), req)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusCreated, out)
}

// POST /music/playlists/import
func (m *Module) importPlaylist(w http.ResponseWriter, r *http.Request) error {
	if r.ContentLength > maxM3UBytes {
		return core.ErrPayloadTooLarge
	}
	q := r.URL.Query()
	name := q.Get("name")
	if len(name) > 4*maxPlaylistName {
		return core.Validation(map[string]string{"name": "too long"})
	}
	body := http.MaxBytesReader(w, r.Body, maxM3UBytes)
	pl, unmatched, err := m.svc.importM3U(r.Context(), principal(r), name, q.Get("space_id"), q.Get("base_path"), body)
	if err != nil {
		return err // a *http.MaxBytesError renders as 413
	}
	return httpx.WriteJSON(w, http.StatusCreated, struct {
		Playlist
		Unmatched []string `json:"unmatched"`
	}{*pl, unmatched})
}

// GET /music/playlists/{id}
func (m *Module) getPlaylist(w http.ResponseWriter, r *http.Request) error {
	out, err := m.svc.getPlaylist(r.Context(), principal(r), r.PathValue("id"))
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

// PATCH /music/playlists/{id}
func (m *Module) patchPlaylist(w http.ResponseWriter, r *http.Request) error {
	var req PlaylistPatch
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	out, err := m.svc.patchPlaylist(r.Context(), principal(r), r.PathValue("id"), req)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

// DELETE /music/playlists/{id}
func (m *Module) deletePlaylist(w http.ResponseWriter, r *http.Request) error {
	if err := m.svc.deletePlaylist(r.Context(), principal(r), r.PathValue("id")); err != nil {
		return err
	}
	return httpx.NoContent(w)
}

type itemsRequest struct {
	TrackIDs    []string `json:"track_ids"`
	ItemIDs     []string `json:"item_ids"`
	Position    *int     `json:"position"`
	BaseVersion *int64   `json:"base_version"`
}

// POST /music/playlists/{id}/items
func (m *Module) insertItems(w http.ResponseWriter, r *http.Request) error {
	var req itemsRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	out, err := m.svc.insertAt(r.Context(), principal(r), r.PathValue("id"), req.TrackIDs, req.Position, req.BaseVersion)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

// PUT /music/playlists/{id}/items
func (m *Module) replaceItems(w http.ResponseWriter, r *http.Request) error {
	var req itemsRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	out, err := m.svc.replaceAll(r.Context(), principal(r), r.PathValue("id"), req.TrackIDs, req.BaseVersion)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

// POST /music/playlists/{id}/items/remove
func (m *Module) removeItems(w http.ResponseWriter, r *http.Request) error {
	var req itemsRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	out, err := m.svc.removeItems(r.Context(), principal(r), r.PathValue("id"), req.ItemIDs, req.BaseVersion)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

// POST /music/playlists/{id}/items/move
func (m *Module) moveItems(w http.ResponseWriter, r *http.Request) error {
	var req itemsRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	out, err := m.svc.moveItems(r.Context(), principal(r), r.PathValue("id"), req.ItemIDs, req.Position, req.BaseVersion)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

// GET /music/playlists/{id}/art
func (m *Module) playlistArt(w http.ResponseWriter, r *http.Request) error {
	return m.svc.servePlaylistArt(w, r, principal(r), r.PathValue("id"), r.URL.Query().Get("size"))
}

// PUT /music/playlists/{id}/art
func (m *Module) putPlaylistArt(w http.ResponseWriter, r *http.Request) error {
	if r.ContentLength > maxArtUpload {
		return core.ErrPayloadTooLarge
	}
	body := http.MaxBytesReader(w, r.Body, maxArtUpload)
	out, err := m.svc.setPlaylistArt(r.Context(), principal(r), r.PathValue("id"), body)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

// DELETE /music/playlists/{id}/art
func (m *Module) deletePlaylistArt(w http.ResponseWriter, r *http.Request) error {
	if err := m.svc.clearPlaylistArt(r.Context(), principal(r), r.PathValue("id")); err != nil {
		return err
	}
	return httpx.NoContent(w)
}

// POST /music/playlists/{id}/duplicate
func (m *Module) duplicatePlaylist(w http.ResponseWriter, r *http.Request) error {
	var req DuplicateRequest
	if err := decodeOptional(r, &req); err != nil {
		return err
	}
	out, err := m.svc.duplicatePlaylist(r.Context(), principal(r), r.PathValue("id"), req)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusCreated, out)
}

// GET /music/playlists/{id}/export.m3u8
func (m *Module) exportPlaylist(w http.ResponseWriter, r *http.Request) error {
	return m.svc.exportM3U(w, r, principal(r), r.PathValue("id"))
}

// POST /music/rescan
func (m *Module) rescan(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		SpaceID string `json:"space_id"`
		Path    string `json:"path"`
	}
	if err := decodeOptional(r, &req); err != nil {
		return err
	}
	if err := m.svc.rescan(r.Context(), principal(r), req.SpaceID, req.Path); err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusAccepted, map[string]string{"job": "music.rescan"})
}
