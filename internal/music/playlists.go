package music

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
)

// Playlists (API.md §12). Private playlists are visible to their owner only, shared ones to
// every user, who sees only the tracks they can read (unavailable_count counts the others,
// which are never listed). Positions are 0-based in the list a caller sees; operations map
// them onto the stored order so items others cannot see keep their places.

const (
	maxPlaylistItems  = 10_000
	maxPlaylists      = 1000 // per owner
	maxPlaylistName   = 200
	maxPlaylistDesc   = 2000
	maxItemsPerInsert = 1000
)

// Playlist is the Playlist object of API.md §12.
type Playlist struct {
	ID               string      `json:"id"`
	Name             string      `json:"name"`
	Description      string      `json:"description"`
	OwnerID          string      `json:"owner_id"`
	OwnerName        string      `json:"owner_name"`
	Visibility       string      `json:"visibility"`
	Collaborative    bool        `json:"collaborative"`
	Kind             string      `json:"kind"`
	Rules            *SmartRules `json:"rules"`
	TrackCount       int64       `json:"track_count"`
	DurationSeconds  float64     `json:"duration_seconds"`
	UnavailableCount int64       `json:"unavailable_count"`
	ArtURL           *string     `json:"art_url"`
	HasCustomArt     bool        `json:"has_custom_art"`
	CanEdit          bool        `json:"can_edit"`
	CreatedAt        string      `json:"created_at"`
	UpdatedAt        string      `json:"updated_at"`
	Version          int64       `json:"version"`
}

// PlaylistItem is the PlaylistItem object of API.md §12.
type PlaylistItem struct {
	ID       string `json:"id"`
	Position int    `json:"position"`
	AddedAt  string `json:"added_at"`
	AddedBy  string `json:"added_by"`
	Track    Track  `json:"track"`
}

// PlaylistDetail is a playlist with the items the caller sees.
type PlaylistDetail struct {
	Playlist
	Items []PlaylistItem `json:"items"`
}

// playlistRow is a music_playlists row.
type playlistRow struct {
	ID, OwnerID, OwnerName, Name, Description, Visibility, Kind string
	Collaborative                                               bool
	Rules                                                       *SmartRules
	ArtID                                                       sql.NullString
	Version                                                     int64
	CreatedAt, UpdatedAt                                        db.Time
}

const playlistCols = `p.id, p.owner_id, COALESCE(NULLIF(u.display_name, ''), u.username, ''), p.name, p.description, p.visibility,
	p.kind, p.collaborative, p.rules, p.art_id, p.version, p.created_at, p.updated_at`

const playlistFrom = `FROM music_playlists p LEFT JOIN users u ON u.id = p.owner_id`

func scanPlaylist(sc scanner, extra ...any) (*playlistRow, error) {
	var (
		r      playlistRow
		collab int64
		rules  sql.NullString
	)
	dest := []any{&r.ID, &r.OwnerID, &r.OwnerName, &r.Name, &r.Description, &r.Visibility, &r.Kind, &collab, &rules, &r.ArtID,
		&r.Version, &r.CreatedAt, &r.UpdatedAt}
	if err := sc.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}
	r.Collaborative = collab == 1
	if rules.Valid {
		var sr SmartRules
		if err := json.Unmarshal([]byte(rules.String), &sr); err == nil {
			r.Rules = &sr
		}
	}
	return &r, nil
}

var errNoPlaylist = core.NotFound("Playlist not found")

func (r *playlistRow) isOwner(p *core.Principal) bool { return r.OwnerID == p.UserID }

// canEditItems reports whether p may add, remove and move items.
func (r *playlistRow) canEditItems(p *core.Principal) bool {
	return r.isOwner(p) || (r.Kind == "manual" && r.Visibility == "shared" && r.Collaborative)
}

// playlistFor loads a playlist p can see (someone else's private playlist is 404).
func (s *Service) playlistFor(ctx context.Context, q db.Querier, p *core.Principal, id string) (*playlistRow, error) {
	if !httpx.ValidID(id) {
		return nil, errNoPlaylist
	}
	r, err := scanPlaylist(q.QueryRowContext(ctx, `SELECT `+playlistCols+` `+playlistFrom+` WHERE p.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNoPlaylist
	}
	if err != nil {
		return nil, err
	}
	if r.Visibility != "shared" && !r.isOwner(p) {
		return nil, errNoPlaylist
	}
	return r, nil
}

/* ------------------------------------------------------------- caller's view */

// itemRow is a stored item with its visibility for one caller.
type itemRow struct {
	ID, FileID, AddedBy string
	AddedAt             db.Time
	Visible             bool
}

// loadItems returns all items of a manual playlist in stored order, marking the ones visible
// to the caller (readable spaces: readable).
func loadItems(ctx context.Context, q db.Querier, playlistID string, readable []string) ([]itemRow, error) {
	rs, err := q.QueryContext(ctx, `SELECT i.id, i.file_id, COALESCE(i.added_by, ''), i.added_at,
		(`+eligibleSQL+` AND f.space_id IN (SELECT value FROM json_each(?)))
		FROM music_playlist_items i JOIN files f ON f.id = i.file_id WHERE i.playlist_id = ? ORDER BY i.position, i.id`,
		mustJSON(readable), playlistID)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []itemRow
	for rs.Next() {
		var (
			it  itemRow
			vis sql.NullInt64
		)
		if err := rs.Scan(&it.ID, &it.FileID, &it.AddedBy, &it.AddedAt, &vis); err != nil {
			return nil, err
		}
		it.Visible = vis.Valid && vis.Int64 == 1
		out = append(out, it)
	}
	return out, rs.Err()
}

// playlistView is a playlist as one caller sees it.
type playlistView struct {
	row         *playlistRow
	items       []itemRow // manual: every item (Visible marks the caller's)
	ids         []string  // the caller's tracks in order
	unavailable int64
}

func (s *Service) view(ctx context.Context, q db.Querier, p *core.Principal, r *playlistRow) (*playlistView, error) {
	v := &playlistView{row: r}
	if r.Kind == "smart" {
		if r.Rules == nil {
			return v, nil
		}
		ids, err := s.evaluateSmart(ctx, p, r.ID, r.Rules)
		if err != nil {
			return nil, err
		}
		v.ids = ids
		return v, nil
	}
	readable, err := s.files.ReadableSpaceIDs(ctx, p)
	if err != nil {
		return nil, err
	}
	if v.items, err = loadItems(ctx, q, r.ID, readable); err != nil {
		return nil, err
	}
	for _, it := range v.items {
		if it.Visible {
			v.ids = append(v.ids, it.FileID)
		} else {
			v.unavailable++
		}
	}
	return v, nil
}

// mosaicArts are the covers of the first four distinct albums among ids (fewer when the
// playlist has fewer albums with art).
func (s *Service) mosaicArts(ctx context.Context, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ids = ids[:min(len(ids), 2000)]
	folderArt := s.settings(ctx).FolderArt
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT t.file_id, t.album_id, aa.art_id, t.art_id, fa.art_id FROM music_tracks t
		CROSS JOIN files f ON f.id = t.file_id
		LEFT JOIN music_album_art aa ON aa.album_id = t.album_id AND aa.space_id = f.space_id
		LEFT JOIN music_folder_art fa ON fa.file_id = t.folder_image_id AND fa.state = 1
		WHERE t.file_id IN (SELECT value FROM json_each(?))`, mustJSON(ids))
	if err != nil {
		return nil, err
	}
	type info struct{ album, art string }
	byID := map[string]info{}
	for rs.Next() {
		var (
			id, album         string
			custom, emb, fold sql.NullString
		)
		if err := rs.Scan(&id, &album, &custom, &emb, &fold); err != nil {
			rs.Close()
			return nil, err
		}
		in := info{album: album}
		switch {
		case custom.Valid:
			in.art = custom.String
		case emb.Valid:
			in.art = emb.String
		case folderArt && fold.Valid:
			in.art = fold.String
		}
		byID[id] = in
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var arts []string
	for _, id := range ids {
		in, ok := byID[id]
		if !ok || in.art == "" {
			continue
		}
		key := in.album
		if key == "" {
			key = "track:" + id
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		arts = append(arts, in.art)
		if len(arts) == 4 {
			break
		}
	}
	return arts, nil
}

// toAPI renders a view (counts, duration and art for the caller).
func (s *Service) playlistJSON(ctx context.Context, p *core.Principal, v *playlistView) (Playlist, error) {
	r := v.row
	out := Playlist{ID: r.ID, Name: r.Name, Description: r.Description, OwnerID: r.OwnerID, OwnerName: r.OwnerName,
		Visibility: r.Visibility, Collaborative: r.Collaborative, Kind: r.Kind, Rules: r.Rules,
		TrackCount: int64(len(v.ids)), UnavailableCount: v.unavailable, CanEdit: r.canEditItems(p),
		CreatedAt: db.FormatTime(r.CreatedAt.Time), UpdatedAt: db.FormatTime(r.UpdatedAt.Time), Version: r.Version}
	if r.Kind == "manual" {
		out.Rules = nil
	}
	if len(v.ids) > 0 {
		for start := 0; start < len(v.ids); start += 5000 {
			end := min(start+5000, len(v.ids))
			var d sql.NullFloat64
			// Duplicates count once per occurrence.
			if err := s.d.DB.SQL.QueryRowContext(ctx, `SELECT SUM(COALESCE(t.duration_seconds, 0)) FROM json_each(?) j
				JOIN music_tracks t ON t.file_id = j.value`, mustJSON(v.ids[start:end])).Scan(&d); err != nil {
				return Playlist{}, err
			}
			out.DurationSeconds += d.Float64
		}
	}
	if r.ArtID.Valid {
		out.HasCustomArt = true
		out.ArtURL = strPtr("/api/v1/music/playlists/" + r.ID + "/art?v=" + artVersion(r.ArtID.String))
		return out, nil
	}
	arts, err := s.mosaicArts(ctx, v.ids)
	if err != nil {
		return Playlist{}, err
	}
	if len(arts) > 0 {
		sum := sha256.Sum256([]byte(strings.Join(arts, ",")))
		out.ArtURL = strPtr("/api/v1/music/playlists/" + r.ID + "/art?v=" + hex.EncodeToString(sum[:6]))
	}
	return out, nil
}

// detail renders a view with its items.
func (s *Service) playlistDetail(ctx context.Context, p *core.Principal, v *playlistView) (*PlaylistDetail, error) {
	pl, err := s.playlistJSON(ctx, p, v)
	if err != nil {
		return nil, err
	}
	out := &PlaylistDetail{Playlist: pl, Items: []PlaylistItem{}}
	trs, err := loadTracksByID(ctx, s.d.DB.SQL, p.UserID, uniq(v.ids))
	if err != nil {
		return nil, err
	}
	folderArt := s.settings(ctx).FolderArt
	byID := map[string]Track{}
	for _, tr := range trs {
		byID[tr.FileID] = tr.toAPI(folderArt)
	}
	if v.row.Kind == "smart" {
		for i, id := range v.ids {
			tr, ok := byID[id]
			if !ok {
				continue
			}
			out.Items = append(out.Items, PlaylistItem{ID: id, Position: i, AddedAt: tr.AddedAt, AddedBy: v.row.OwnerID, Track: tr})
		}
		return out, nil
	}
	pos := 0
	for _, it := range v.items {
		if !it.Visible {
			continue
		}
		tr, ok := byID[it.FileID]
		if !ok {
			continue
		}
		out.Items = append(out.Items, PlaylistItem{ID: it.ID, Position: pos, AddedAt: db.FormatTime(it.AddedAt.Time),
			AddedBy: it.AddedBy, Track: tr})
		pos++
	}
	return out, nil
}

func uniq(ids []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// getPlaylist returns a playlist with its items for p.
func (s *Service) getPlaylist(ctx context.Context, p *core.Principal, id string) (*PlaylistDetail, error) {
	r, err := s.playlistFor(ctx, s.d.DB.SQL, p, id)
	if err != nil {
		return nil, err
	}
	v, err := s.view(ctx, s.d.DB.SQL, p, r)
	if err != nil {
		return nil, err
	}
	return s.playlistDetail(ctx, p, v)
}

// playlistSummary renders a playlist without items.
func (s *Service) playlistSummary(ctx context.Context, p *core.Principal, r *playlistRow) (Playlist, error) {
	v, err := s.view(ctx, s.d.DB.SQL, p, r)
	if err != nil {
		return Playlist{}, err
	}
	return s.playlistJSON(ctx, p, v)
}

/* ------------------------------------------------------------------ listings */

type playlistFilter struct {
	Terms []string
	Kind  string
	Owner string // me | others | all
}

var playlistSorts = []string{"name", "updated", "created"}

func parsePlaylistFilter(q url.Values) (playlistFilter, error) {
	f := playlistFilter{Kind: q.Get("kind"), Owner: q.Get("owner")}
	if v := q.Get("q"); v != "" {
		if len(v) > 500 {
			return f, core.BadRequest("q: too long")
		}
		f.Terms = queryTerms(v)
		if len(f.Terms) == 0 {
			f.Terms = []string{"\x00"} // no searchable characters: matches nothing
		}
	}
	switch f.Kind {
	case "", "manual", "smart":
	default:
		return f, core.BadRequest("kind: must be manual or smart")
	}
	switch f.Owner {
	case "":
		f.Owner = "all"
	case "me", "others", "all":
	default:
		return f, core.BadRequest("owner: must be me, others or all")
	}
	return f, nil
}

func playlistWhere(p *core.Principal, f *playlistFilter) *whereBuilder {
	w := &whereBuilder{}
	switch f.Owner {
	case "me":
		w.add(`p.owner_id = ?`, p.UserID)
	case "others":
		w.add(`p.owner_id <> ? AND p.visibility = 'shared'`, p.UserID)
	default:
		w.add(`(p.owner_id = ? OR p.visibility = 'shared')`, p.UserID)
	}
	if f.Kind != "" {
		w.add(`p.kind = ?`, f.Kind)
	}
	for _, t := range f.Terms {
		w.add(`p.name_key LIKE ? ESCAPE '\'`, "%"+db.EscapeLike(t)+"%")
	}
	return w
}

func (s *Service) listPlaylists(ctx context.Context, p *core.Principal, f playlistFilter, lq listQuery) ([]Playlist, string, error) {
	hash := filterHash([]any{"playlists", f, lq.Sort, lq.Order, p.UserID})
	keys := []string{`p.name_key`}
	switch lq.Sort {
	case "updated":
		keys = []string{`p.updated_at`}
	case "created":
		keys = []string{`p.created_at`}
	}
	pq := pageQuery{cols: playlistCols, from: playlistFrom, where: playlistWhere(p, &f), keys: keys, idExpr: "p.id"}
	rows, next, err := keysetPage(ctx, s.d.DB.SQL, pq, lq, hash, scanPlaylist, func(r *playlistRow) string { return r.ID })
	if err != nil {
		return nil, "", err
	}
	out := make([]Playlist, 0, len(rows))
	for _, r := range rows {
		pl, err := s.playlistSummary(ctx, p, r)
		if err != nil {
			return nil, "", err
		}
		out = append(out, pl)
	}
	return out, next, nil
}

// playlistsByID renders the playlists p can see among ids, in order.
func (s *Service) playlistsByID(ctx context.Context, p *core.Principal, ids []string) ([]Playlist, error) {
	out := []Playlist{}
	for _, id := range ids {
		r, err := s.playlistFor(ctx, s.d.DB.SQL, p, id)
		if err != nil {
			if errors.Is(err, errNoPlaylist) {
				continue
			}
			return nil, err
		}
		pl, err := s.playlistSummary(ctx, p, r)
		if err != nil {
			return nil, err
		}
		out = append(out, pl)
	}
	return out, nil
}

// searchPlaylists returns visible playlists whose name contains every term.
func (s *Service) searchPlaylists(ctx context.Context, p *core.Principal, terms []string, limit int) ([]Playlist, error) {
	f := playlistFilter{Terms: terms, Owner: "all"}
	w := playlistWhere(p, &f)
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT `+playlistCols+` `+playlistFrom+` WHERE `+w.sql()+`
		ORDER BY p.name_key, p.id LIMIT ?`, append(w.args, limit)...)
	if err != nil {
		return nil, err
	}
	var rows []*playlistRow
	for rs.Next() {
		r, err := scanPlaylist(rs)
		if err != nil {
			rs.Close()
			return nil, err
		}
		rows = append(rows, r)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, err
	}
	out := []Playlist{}
	for _, r := range rows {
		pl, err := s.playlistSummary(ctx, p, r)
		if err != nil {
			return nil, err
		}
		out = append(out, pl)
	}
	return out, nil
}

/* ------------------------------------------------------------------- writes */

// PlaylistCreate is POST /music/playlists.
type PlaylistCreate struct {
	Name          string          `json:"name"`
	Description   string          `json:"description"`
	Visibility    string          `json:"visibility"`
	Collaborative bool            `json:"collaborative"`
	Kind          string          `json:"kind"`
	Rules         json.RawMessage `json:"rules"`
	TrackIDs      []string        `json:"track_ids"`
}

func checkName(name string, fe map[string]string) string {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		fe["name"] = "required"
	case !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl):
		fe["name"] = "must be a single line"
	case utf8.RuneCountInString(name) > maxPlaylistName:
		fe["name"] = fmt.Sprintf("at most %d characters", maxPlaylistName)
	}
	return name
}

func checkDescription(d string, fe map[string]string) string {
	if !utf8.ValidString(d) {
		fe["description"] = "invalid UTF-8"
		return ""
	}
	d = cleanMultiline(d, maxPlaylistDesc+1)
	if utf8.RuneCountInString(d) > maxPlaylistDesc {
		fe["description"] = fmt.Sprintf("at most %d characters", maxPlaylistDesc)
	}
	return d
}

func checkVisibility(v string, fe map[string]string) string {
	switch v {
	case "":
		return "private"
	case "private", "shared":
		return v
	}
	fe["visibility"] = "must be private or shared"
	return v
}

// checkTrackIDs validates ids as tracks p can read (422 naming the first bad one).
func (s *Service) checkTrackIDs(ctx context.Context, p *core.Principal, field string, ids []string, max int) error {
	if len(ids) > max {
		return core.Validation(map[string]string{field: fmt.Sprintf("at most %d tracks", max)})
	}
	for _, id := range ids {
		if !httpx.ValidID(id) {
			return core.Validation(map[string]string{field: "invalid track id"})
		}
	}
	trs, err := s.visibleTracks(ctx, p, uniq(ids))
	if err != nil {
		return err
	}
	ok := map[string]bool{}
	for _, tr := range trs {
		ok[tr.FileID] = true
	}
	for _, id := range ids {
		if !ok[id] {
			return core.Validation(map[string]string{field: "unknown or unreadable track " + id})
		}
	}
	return nil
}

// createPlaylist creates a playlist owned by p.
func (s *Service) createPlaylist(ctx context.Context, p *core.Principal, in PlaylistCreate) (*Playlist, error) {
	fe := map[string]string{}
	name := checkName(in.Name, fe)
	desc := checkDescription(in.Description, fe)
	vis := checkVisibility(in.Visibility, fe)
	kind := in.Kind
	switch kind {
	case "":
		kind = "manual"
	case "manual", "smart":
	default:
		fe["kind"] = "must be manual or smart"
	}
	var rules *SmartRules
	hasRules := len(in.Rules) > 0 && !isNull(in.Rules)
	switch {
	case kind == "smart" && !hasRules:
		fe["rules"] = "required for smart playlists"
	case kind == "manual" && hasRules:
		fe["rules"] = "only smart playlists have rules"
	case kind == "smart" && len(in.TrackIDs) > 0:
		fe["track_ids"] = "smart playlists have no manual items"
	}
	if len(fe) > 0 {
		return nil, core.Validation(fe)
	}
	if hasRules {
		r, err := validateRules(in.Rules)
		if err != nil {
			return nil, err
		}
		rules = r
	}
	if err := s.checkTrackIDs(ctx, p, "track_ids", in.TrackIDs, maxPlaylistItems); err != nil {
		return nil, err
	}
	id := core.NewID()
	now := db.FormatTime(s.now())
	err := s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM music_playlists WHERE owner_id = ?`, p.UserID).Scan(&n); err != nil {
			return err
		}
		if n >= maxPlaylists {
			return core.Conflict(fmt.Sprintf("You have reached the limit of %d playlists", maxPlaylists))
		}
		var rulesJSON any
		if rules != nil {
			b, _ := json.Marshal(rules)
			rulesJSON = string(b)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO music_playlists (id, owner_id, name, name_key, description, visibility,
			collaborative, kind, rules, version, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			id, p.UserID, name, fold(name), desc, vis, db.Bool(in.Collaborative), kind, rulesJSON, now, now); err != nil {
			return err
		}
		return insertItems(ctx, tx, id, in.TrackIDs, 0, p.UserID, now)
	})
	if err != nil {
		return nil, err
	}
	r, err := s.playlistFor(ctx, s.d.DB.SQL, p, id)
	if err != nil {
		return nil, err
	}
	pl, err := s.playlistSummary(ctx, p, r)
	return &pl, err
}

func insertItems(ctx context.Context, tx *sql.Tx, playlistID string, fileIDs []string, from int, addedBy, now string) error {
	for i, fid := range fileIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO music_playlist_items (id, playlist_id, position, file_id, added_at, added_by)
			VALUES (?, ?, ?, ?, ?, ?)`, core.NewID(), playlistID, from+i, fid, now, addedBy); err != nil {
			return err
		}
	}
	return nil
}

// conflictCurrent is the 409 for a stale base_version, carrying the current playlist.
func (s *Service) conflictCurrent(ctx context.Context, p *core.Principal, r *playlistRow) error {
	pl, err := s.playlistSummary(ctx, p, r)
	if err != nil {
		return err
	}
	return core.Conflict("The playlist was changed by someone else; reload it and try again").WithDetails(map[string]any{"current": pl})
}

// PlaylistPatch is PATCH /music/playlists/{id}.
type PlaylistPatch struct {
	Name          *string         `json:"name"`
	Description   *string         `json:"description"`
	Visibility    *string         `json:"visibility"`
	Collaborative *bool           `json:"collaborative"`
	Rules         json.RawMessage `json:"rules"`
	BaseVersion   *int64          `json:"base_version"`
}

// patchPlaylist changes a playlist's settings (owner only).
func (s *Service) patchPlaylist(ctx context.Context, p *core.Principal, id string, in PlaylistPatch) (*Playlist, error) {
	r, err := s.playlistFor(ctx, s.d.DB.SQL, p, id)
	if err != nil {
		return nil, err
	}
	if !r.isOwner(p) {
		return nil, core.Forbidden("Only the owner can change this playlist")
	}
	fe := map[string]string{}
	var name, desc, vis string
	if in.Name != nil {
		name = checkName(*in.Name, fe)
	}
	if in.Description != nil {
		desc = checkDescription(*in.Description, fe)
	}
	if in.Visibility != nil {
		vis = checkVisibility(*in.Visibility, fe)
		if *in.Visibility == "" {
			fe["visibility"] = "must be private or shared"
		}
	}
	var rulesJSON sql.NullString
	if len(in.Rules) > 0 {
		switch {
		case r.Kind != "smart":
			fe["rules"] = "only smart playlists have rules"
		case isNull(in.Rules):
			fe["rules"] = "smart playlists need rules"
		}
	}
	if len(fe) > 0 {
		return nil, core.Validation(fe)
	}
	if len(in.Rules) > 0 {
		rules, err := validateRules(in.Rules)
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(rules)
		rulesJSON = sql.NullString{String: string(b), Valid: true}
	}
	err = s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		cur, err := s.playlistFor(ctx, tx, p, id)
		if err != nil {
			return err
		}
		if !cur.isOwner(p) {
			return core.Forbidden("Only the owner can change this playlist")
		}
		if in.BaseVersion != nil && *in.BaseVersion != cur.Version {
			return errStale
		}
		// Unchanged fields keep the values of the row read in this transaction.
		if in.Name == nil {
			name = cur.Name
		}
		if in.Description == nil {
			desc = cur.Description
		}
		if in.Visibility == nil {
			vis = cur.Visibility
		}
		collab := cur.Collaborative
		if in.Collaborative != nil {
			collab = *in.Collaborative
		}
		if !rulesJSON.Valid && cur.Rules != nil {
			b, _ := json.Marshal(cur.Rules)
			rulesJSON = sql.NullString{String: string(b), Valid: true}
		}
		_, err = tx.ExecContext(ctx, `UPDATE music_playlists SET name = ?, name_key = ?, description = ?, visibility = ?,
			collaborative = ?, rules = ?, version = version + 1, updated_at = ? WHERE id = ?`,
			name, fold(name), desc, vis, db.Bool(collab), rulesJSON, db.FormatTime(s.now()), id)
		return err
	})
	if err != nil {
		return nil, s.mapStale(ctx, p, id, err)
	}
	r, err = s.playlistFor(ctx, s.d.DB.SQL, p, id)
	if err != nil {
		return nil, err
	}
	pl, err := s.playlistSummary(ctx, p, r)
	return &pl, err
}

// errStale marks a base_version mismatch inside a transaction.
var errStale = errors.New("music: stale playlist version")

// mapStale turns errStale into the 409 with the current playlist.
func (s *Service) mapStale(ctx context.Context, p *core.Principal, id string, err error) error {
	if !errors.Is(err, errStale) {
		return err
	}
	r, lerr := s.playlistFor(ctx, s.d.DB.SQL, p, id)
	if lerr != nil {
		return lerr
	}
	return s.conflictCurrent(ctx, p, r)
}

// deletePlaylist deletes a playlist (owner only).
func (s *Service) deletePlaylist(ctx context.Context, p *core.Principal, id string) error {
	r, err := s.playlistFor(ctx, s.d.DB.SQL, p, id)
	if err != nil {
		return err
	}
	if !r.isOwner(p) {
		return core.Forbidden("Only the owner can delete this playlist")
	}
	_, err = s.d.DB.SQL.ExecContext(ctx, `DELETE FROM music_playlists WHERE id = ?`, id)
	return err
}

/* --------------------------------------------------------------- item edits */

// itemOp computes a new stored order from the current items (with the caller's visibility).
type itemOp func(items []itemRow) ([]itemRow, error)

// editItems runs an item operation: permission and kind checks, the base_version check, the
// 10 000 item limit, renumbering and the version bump, all in one transaction.
func (s *Service) editItems(ctx context.Context, p *core.Principal, id string, baseVersion *int64, op itemOp) (*PlaylistDetail, error) {
	r, err := s.playlistFor(ctx, s.d.DB.SQL, p, id)
	if err != nil {
		return nil, err
	}
	if r.Kind == "smart" {
		return nil, core.Conflict("Smart playlists are read-only lists; change their rules instead")
	}
	if !r.canEditItems(p) {
		return nil, core.Forbidden("Only the owner can change the items of this playlist")
	}
	readable, err := s.files.ReadableSpaceIDs(ctx, p)
	if err != nil {
		return nil, err
	}
	now := db.FormatTime(s.now())
	err = s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		cur, err := s.playlistFor(ctx, tx, p, id)
		if err != nil {
			return err
		}
		if cur.Kind == "smart" {
			return core.Conflict("Smart playlists are read-only lists; change their rules instead")
		}
		if !cur.canEditItems(p) {
			return core.Forbidden("Only the owner can change the items of this playlist")
		}
		if baseVersion != nil && *baseVersion != cur.Version {
			return errStale
		}
		old, err := loadItems(ctx, tx, id, readable)
		if err != nil {
			return err
		}
		next, err := op(old)
		if err != nil {
			return err
		}
		if len(next) > maxPlaylistItems {
			return core.Conflict(fmt.Sprintf("A playlist holds at most %d items", maxPlaylistItems))
		}
		if err := writeItems(ctx, tx, id, old, next, p.UserID, now); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE music_playlists SET version = version + 1, updated_at = ? WHERE id = ?`, now, id)
		return err
	})
	if err != nil {
		if db.IsForeignKeyViolation(err) {
			return nil, core.Validation(map[string]string{"track_ids": "a track was deleted meanwhile"})
		}
		return nil, s.mapStale(ctx, p, id, err)
	}
	return s.getPlaylist(ctx, p, id)
}

// writeItems stores the new order: removed items are deleted, new ones (ID "") inserted and
// positions renumbered 0..n-1.
func writeItems(ctx context.Context, tx *sql.Tx, playlistID string, old, next []itemRow, addedBy, now string) error {
	keep := map[string]bool{}
	for _, it := range next {
		if it.ID != "" {
			keep[it.ID] = true
		}
	}
	oldPos := map[string]int{}
	for i, it := range old {
		oldPos[it.ID] = i
		if !keep[it.ID] {
			if _, err := tx.ExecContext(ctx, `DELETE FROM music_playlist_items WHERE id = ?`, it.ID); err != nil {
				return err
			}
		}
	}
	for i, it := range next {
		if it.ID == "" {
			if _, err := tx.ExecContext(ctx, `INSERT INTO music_playlist_items (id, playlist_id, position, file_id, added_at, added_by)
				VALUES (?, ?, ?, ?, ?, ?)`, core.NewID(), playlistID, i, it.FileID, now, addedBy); err != nil {
				return err
			}
			continue
		}
		if p, ok := oldPos[it.ID]; !ok || p != i {
			if _, err := tx.ExecContext(ctx, `UPDATE music_playlist_items SET position = ? WHERE id = ?`, i, it.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// globalIndex maps a position in the caller's view to an index in the stored order: before the
// view's item at that position, or the end of the list.
func globalIndex(items []itemRow, viewPos int) int {
	n := 0
	for i, it := range items {
		if !it.Visible {
			continue
		}
		if n == viewPos {
			return i
		}
		n++
	}
	return len(items)
}

func newItems(fileIDs []string) []itemRow {
	out := make([]itemRow, len(fileIDs))
	for i, id := range fileIDs {
		out[i] = itemRow{FileID: id, Visible: true}
	}
	return out
}

// insertAt inserts tracks at a view position (nil = append).
func (s *Service) insertAt(ctx context.Context, p *core.Principal, id string, trackIDs []string, position *int, base *int64) (*PlaylistDetail, error) {
	if len(trackIDs) == 0 {
		return nil, core.Validation(map[string]string{"track_ids": "required"})
	}
	if position != nil && *position < 0 {
		return nil, core.Validation(map[string]string{"position": "must be >= 0"})
	}
	if err := s.checkTrackIDs(ctx, p, "track_ids", trackIDs, maxItemsPerInsert); err != nil {
		return nil, err
	}
	return s.editItems(ctx, p, id, base, func(items []itemRow) ([]itemRow, error) {
		at := len(items)
		if position != nil {
			at = globalIndex(items, *position)
		}
		return slices.Concat(items[:at], newItems(trackIDs), items[at:]), nil
	})
}

// replaceItems replaces the caller's view by trackIDs; items the caller cannot see keep their
// slots in the stored order.
func (s *Service) replaceAll(ctx context.Context, p *core.Principal, id string, trackIDs []string, base *int64) (*PlaylistDetail, error) {
	if trackIDs == nil {
		return nil, core.Validation(map[string]string{"track_ids": "required"})
	}
	if err := s.checkTrackIDs(ctx, p, "track_ids", trackIDs, maxPlaylistItems); err != nil {
		return nil, err
	}
	return s.editItems(ctx, p, id, base, func(items []itemRow) ([]itemRow, error) {
		// Reuse existing items of the same track (keeps added_at/added_by), in order.
		pool := map[string][]itemRow{}
		for _, it := range items {
			if it.Visible {
				pool[it.FileID] = append(pool[it.FileID], it)
			}
		}
		repl := make([]itemRow, 0, len(trackIDs))
		for _, fid := range trackIDs {
			if q := pool[fid]; len(q) > 0 {
				repl = append(repl, q[0])
				pool[fid] = q[1:]
			} else {
				repl = append(repl, itemRow{FileID: fid, Visible: true})
			}
		}
		out := make([]itemRow, 0, len(items)+len(repl))
		k := 0
		for _, it := range items {
			if !it.Visible {
				out = append(out, it)
				continue
			}
			if k < len(repl) {
				out = append(out, repl[k])
				k++
			}
		}
		return append(out, repl[k:]...), nil
	})
}

// removeItems removes the listed items the caller can see (unknown ids are ignored).
func (s *Service) removeItems(ctx context.Context, p *core.Principal, id string, itemIDs []string, base *int64) (*PlaylistDetail, error) {
	if len(itemIDs) == 0 || len(itemIDs) > maxPlaylistItems {
		return nil, core.Validation(map[string]string{"item_ids": fmt.Sprintf("1-%d item ids are required", maxPlaylistItems)})
	}
	drop := map[string]bool{}
	for _, iid := range itemIDs {
		drop[iid] = true
	}
	return s.editItems(ctx, p, id, base, func(items []itemRow) ([]itemRow, error) {
		out := make([]itemRow, 0, len(items))
		for _, it := range items {
			if it.Visible && drop[it.ID] {
				continue
			}
			out = append(out, it)
		}
		return out, nil
	})
}

// moveItems moves the listed items (keeping their relative order) so the first lands at
// position in the caller's list without them.
func (s *Service) moveItems(ctx context.Context, p *core.Principal, id string, itemIDs []string, position *int, base *int64) (*PlaylistDetail, error) {
	fe := map[string]string{}
	if len(itemIDs) == 0 || len(itemIDs) > maxPlaylistItems {
		fe["item_ids"] = fmt.Sprintf("1-%d item ids are required", maxPlaylistItems)
	}
	if position == nil || *position < 0 {
		fe["position"] = "required (>= 0)"
	}
	if len(fe) > 0 {
		return nil, core.Validation(fe)
	}
	move := map[string]bool{}
	for _, iid := range itemIDs {
		move[iid] = true
	}
	return s.editItems(ctx, p, id, base, func(items []itemRow) ([]itemRow, error) {
		var moved, rest []itemRow
		found := 0
		for _, it := range items {
			if it.Visible && move[it.ID] {
				moved = append(moved, it)
				found++
			} else {
				rest = append(rest, it)
			}
		}
		if found != len(uniq(itemIDs)) {
			return nil, core.Validation(map[string]string{"item_ids": "unknown item id"})
		}
		at := globalIndex(rest, *position)
		return slices.Concat(rest[:at], moved, rest[at:]), nil
	})
}

/* ---------------------------------------------------------------------- art */

// setPlaylistArt stores custom art (owner only).
func (s *Service) setPlaylistArt(ctx context.Context, p *core.Principal, id string, body io.Reader) (*Playlist, error) {
	r, err := s.playlistFor(ctx, s.d.DB.SQL, p, id)
	if err != nil {
		return nil, err
	}
	if !r.isOwner(p) {
		return nil, core.Forbidden("Only the owner can change this playlist")
	}
	artID, err := s.normalizeUpload(ctx, body)
	if err != nil {
		return nil, err
	}
	if _, err := s.d.DB.SQL.ExecContext(ctx, `UPDATE music_playlists SET art_id = ?, version = version + 1, updated_at = ? WHERE id = ?`,
		artID, db.FormatTime(s.now()), id); err != nil {
		return nil, err
	}
	if r, err = s.playlistFor(ctx, s.d.DB.SQL, p, id); err != nil {
		return nil, err
	}
	pl, err := s.playlistSummary(ctx, p, r)
	return &pl, err
}

// clearPlaylistArt removes custom art (owner only).
func (s *Service) clearPlaylistArt(ctx context.Context, p *core.Principal, id string) error {
	r, err := s.playlistFor(ctx, s.d.DB.SQL, p, id)
	if err != nil {
		return err
	}
	if !r.isOwner(p) {
		return core.Forbidden("Only the owner can change this playlist")
	}
	if !r.ArtID.Valid {
		return nil
	}
	_, err = s.d.DB.SQL.ExecContext(ctx, `UPDATE music_playlists SET art_id = NULL, version = version + 1, updated_at = ? WHERE id = ?`,
		db.FormatTime(s.now()), id)
	return err
}

// servePlaylistArt writes the custom art, else the mosaic of the caller's view (a single
// cover with fewer than four albums); 404 when there is nothing to show.
func (s *Service) servePlaylistArt(w http.ResponseWriter, r *http.Request, p *core.Principal, id, size string) error {
	ctx := r.Context()
	if _, _, err := parseArtSize(size); err != nil {
		return err
	}
	row, err := s.playlistFor(ctx, s.d.DB.SQL, p, id)
	if err != nil {
		return err
	}
	if row.ArtID.Valid {
		return s.serveArt(w, r, row.ArtID.String, size)
	}
	v, err := s.view(ctx, s.d.DB.SQL, p, row)
	if err != nil {
		return err
	}
	arts, err := s.mosaicArts(ctx, v.ids)
	if err != nil {
		return err
	}
	switch {
	case len(arts) == 0:
		return errNoArt
	case len(arts) < 4:
		return s.serveArt(w, r, arts[0], size)
	}
	f, etag, err := s.mosaic(ctx, [4]string{arts[0], arts[1], arts[2], arts[3]}, size)
	if err != nil {
		return err
	}
	return serveJPEG(w, r, f, etag)
}

/* --------------------------------------------------------------- duplicates */

// DuplicateRequest is POST /music/playlists/{id}/duplicate.
type DuplicateRequest struct {
	Name   *string `json:"name"`
	Freeze bool    `json:"freeze"`
}

// duplicatePlaylist copies a playlist p can see into a new private playlist of p's: the items
// p can see (manual), the rules (smart) or the current list (smart with freeze).
func (s *Service) duplicatePlaylist(ctx context.Context, p *core.Principal, id string, in DuplicateRequest) (*Playlist, error) {
	r, err := s.playlistFor(ctx, s.d.DB.SQL, p, id)
	if err != nil {
		return nil, err
	}
	fe := map[string]string{}
	name := truncateRunes(r.Name+" (copy)", maxPlaylistName)
	if in.Name != nil {
		name = checkName(*in.Name, fe)
	}
	if len(fe) > 0 {
		return nil, core.Validation(fe)
	}
	create := PlaylistCreate{Name: name, Description: r.Description, Kind: r.Kind}
	if r.Kind == "smart" && !in.Freeze {
		b, _ := json.Marshal(r.Rules)
		create.Rules = b
	} else {
		v, err := s.view(ctx, s.d.DB.SQL, p, r)
		if err != nil {
			return nil, err
		}
		create.Kind, create.TrackIDs = "manual", v.ids
		if create.TrackIDs == nil {
			create.TrackIDs = []string{}
		}
	}
	pl, err := s.createPlaylist(ctx, p, create)
	if err != nil {
		return nil, err
	}
	if r.ArtID.Valid {
		if _, err := s.d.DB.SQL.ExecContext(ctx, `UPDATE music_playlists SET art_id = ? WHERE id = ?`, r.ArtID.String, pl.ID); err != nil {
			return nil, err
		}
		nr, err := s.playlistFor(ctx, s.d.DB.SQL, p, pl.ID)
		if err != nil {
			return nil, err
		}
		out, err := s.playlistSummary(ctx, p, nr)
		return &out, err
	}
	return pl, nil
}
