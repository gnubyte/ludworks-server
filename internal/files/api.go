package files

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
)

/* -------------------------------------------------------------- JSON shapes */

// APIEntry is the Entry JSON of API.md §5.
type APIEntry struct {
	ID           string   `json:"id"`
	SpaceID      string   `json:"space_id"`
	ParentID     *string  `json:"parent_id"`
	Path         string   `json:"path"`
	Name         string   `json:"name"`
	IsDir        bool     `json:"is_dir"`
	Size         int64    `json:"size"`
	Mime         string   `json:"mime"`
	ModTime      string   `json:"mod_time"`
	Category     string   `json:"category"`
	Subcategory  string   `json:"subcategory"`
	Tags         []string `json:"tags"`
	SHA256       string   `json:"sha256"`
	OwnerID      string   `json:"owner_id"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
	HasThumbnail bool     `json:"has_thumbnail"`
}

type apiTrashEntry struct {
	APIEntry
	TrashedAt    string `json:"trashed_at"`
	OriginalPath string `json:"original_path"`
}

// thumbCaps says which thumbnails can be produced right now.
type thumbCaps struct{ video bool }

func (s *Service) caps(ctx context.Context) thumbCaps {
	return thumbCaps{video: s.videoThumbsEnabled(ctx)}
}

func (c thumbCaps) has(e *Entry, state int) bool {
	if e.IsDir || state == thumbFailed {
		return false
	}
	switch {
	case thumbImage(e.Mime):
		return e.Size > 0 && e.Size <= maxThumbInput
	case thumbVideo(e.Mime):
		return c.video && e.Size > 0
	}
	return false
}

func toAPI(e *Entry, state int, c thumbCaps) APIEntry {
	tags := e.Tags
	if tags == nil {
		tags = []string{}
	}
	out := APIEntry{
		ID: e.ID, SpaceID: e.SpaceID, Path: e.Path, Name: e.Name, IsDir: e.IsDir, Size: e.Size, Mime: e.Mime,
		ModTime: db.FormatTime(e.ModTime), Category: e.Category, Subcategory: e.Subcategory, Tags: tags,
		SHA256: e.SHA256, OwnerID: e.OwnerID, CreatedAt: db.FormatTime(e.CreatedAt), UpdatedAt: db.FormatTime(e.UpdatedAt),
		HasThumbnail: c.has(e, state),
	}
	if e.ParentID != "" {
		pid := e.ParentID
		out.ParentID = &pid
	}
	return out
}

func rowAPI(r *row, c thumbCaps) APIEntry { return toAPI(&r.Entry, r.ThumbState, c) }

// entryAPI converts an Entry from a service call, looking up its thumbnail state.
func (s *Service) entryAPI(ctx context.Context, e *Entry) APIEntry {
	var state int
	_ = s.d.DB.SQL.QueryRowContext(ctx, `SELECT thumb_state FROM files WHERE id = ?`, e.ID).Scan(&state)
	return toAPI(e, state, s.caps(ctx))
}

/* --------------------------------------------------------------- listings */

type breadcrumb struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type listing struct {
	Dir         APIEntry     `json:"dir"`
	Breadcrumbs []breadcrumb `json:"breadcrumbs"`
	Items       []APIEntry   `json:"items"`
	NextCursor  *string      `json:"next_cursor"`
}

// listCursor is the keyset position: is_dir, sort value (string or int), name, id.
type listCursor struct {
	D  int    `json:"d"`
	VS string `json:"s,omitempty"`
	VI int64  `json:"i,omitempty"`
	N  string `json:"n"`
	ID string `json:"id"`
	K  string `json:"k"` // sort key the cursor belongs to
}

var sortCols = map[string]string{"name": "", "size": "f.size", "mod_time": "f.mod_time", "type": "f.mime"}

// resolveSpace returns the requested space, defaulting to the caller's home.
func (m *Module) resolveSpace(ctx context.Context, p *core.Principal, spaceID string) (*space, error) {
	if spaceID == "" {
		id, err := m.svc.HomeSpaceID(ctx, p.UserID)
		if err != nil {
			return nil, err
		}
		spaceID = id
	}
	sp, _, err := m.svc.spaceFor(ctx, m.d.DB.SQL, p, spaceID, accRead)
	return sp, err
}

// dirRow loads a live directory row (the root is created on demand).
func (m *Module) dirRow(ctx context.Context, sp *space, p *core.Principal, dirPath string) (*row, error) {
	dir, err := rowByPath(ctx, m.d.DB.SQL, sp.ID, dirPath)
	if err != nil {
		return nil, err
	}
	if dir == nil && dirPath == "" {
		var pd pending
		m.svc.treeMu.Lock()
		dir, err = m.svc.ensureDirLocked(ctx, sp, "", p.UserID, &pd)
		m.svc.treeMu.Unlock()
		pd.flush()
		if err != nil {
			return nil, err
		}
	}
	if dir == nil || !dir.IsDir || dir.TrashedAt.Valid {
		return nil, core.NotFound("Folder not found")
	}
	return dir, nil
}

func (m *Module) listFiles(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	q := r.URL.Query()
	dirPath, err := cleanPath("path", q.Get("path"))
	if err != nil {
		return err
	}
	sp, err := m.resolveSpace(ctx, p, q.Get("space_id"))
	if err != nil {
		return err
	}
	dir, err := m.dirRow(ctx, sp, p, dirPath)
	if err != nil {
		return err
	}
	sortKey := q.Get("sort")
	if sortKey == "" {
		sortKey = "name"
	}
	col, ok := sortCols[sortKey]
	if !ok {
		return core.Validation(map[string]string{"sort": "must be one of name, size, mod_time, type"})
	}
	order := strings.ToLower(q.Get("order"))
	if order == "" {
		order = "asc"
	}
	if order != "asc" && order != "desc" {
		return core.Validation(map[string]string{"order": "must be asc or desc"})
	}
	limit := httpx.ParseLimit(r, 50, 200)
	cmp, dirn := ">", "ASC"
	if order == "desc" {
		cmp, dirn = "<", "DESC"
	}
	const nameExpr = "f.name COLLATE NOCASE"
	where := `f.parent_id = ? AND f.trashed_at IS NULL AND f.space_id = ?`
	args := []any{dir.ID, sp.ID}
	if c := q.Get("cursor"); c != "" {
		var cur listCursor
		if err := httpx.DecodeCursor(c, &cur); err != nil || cur.K != sortKey+":"+order {
			return httpx.ErrInvalidCursor
		}
		var v any = cur.VS
		if col == "f.size" {
			v = cur.VI
		}
		if col == "" {
			where += fmt.Sprintf(` AND (f.is_dir < ? OR (f.is_dir = ? AND (%[1]s %[2]s ? OR (%[1]s = ? AND f.id %[2]s ?))))`, nameExpr, cmp)
			args = append(args, cur.D, cur.D, cur.N, cur.N, cur.ID)
		} else {
			where += fmt.Sprintf(` AND (f.is_dir < ? OR (f.is_dir = ? AND (%[1]s %[2]s ? OR (%[1]s = ? AND (%[3]s %[2]s ? OR (%[3]s = ? AND f.id %[2]s ?))))))`,
				col, cmp, nameExpr)
			args = append(args, cur.D, cur.D, v, v, cur.N, cur.N, cur.ID)
		}
	}
	orderBy := fmt.Sprintf(`f.is_dir DESC, %[1]s %[2]s, f.id %[2]s`, nameExpr, dirn)
	if col != "" {
		orderBy = fmt.Sprintf(`f.is_dir DESC, %[1]s %[3]s, %[2]s %[3]s, f.id %[3]s`, col, nameExpr, dirn)
	}
	rows, err := queryRows(ctx, m.d.DB.SQL, `SELECT `+entryCols+` FROM files f WHERE `+where+` ORDER BY `+orderBy+` LIMIT ?`,
		append(args, limit+1)...)
	if err != nil {
		return err
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		cur := listCursor{D: int(db.Bool(last.IsDir)), N: last.Name, ID: last.ID, K: sortKey + ":" + order}
		switch col {
		case "f.size":
			cur.VI = last.Size
		case "f.mod_time":
			cur.VS = db.FormatTime(last.ModTime)
		case "f.mime":
			cur.VS = last.Mime
		}
		next = httpx.EncodeCursor(cur)
	}
	caps := m.svc.caps(ctx)
	out := listing{Dir: rowAPI(dir, caps), Breadcrumbs: breadcrumbs(sp, dirPath), Items: make([]APIEntry, 0, len(rows))}
	if dirPath == "" {
		out.Dir.Name = sp.Name
	}
	for _, x := range rows {
		out.Items = append(out.Items, rowAPI(x, caps))
	}
	if next != "" {
		out.NextCursor = &next
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

func breadcrumbs(sp *space, dirPath string) []breadcrumb {
	out := []breadcrumb{{Name: sp.Name, Path: ""}}
	if dirPath == "" {
		return out
	}
	parts := strings.Split(dirPath, "/")
	for i, part := range parts {
		out = append(out, breadcrumb{Name: part, Path: strings.Join(parts[:i+1], "/")})
	}
	return out
}

// maxTreeEntries caps /files/tree, folder ZIPs and recursive copies.
const maxTreeEntries = 200_000

type treeItem struct {
	ID      string `json:"id"`
	Path    string `json:"path"`
	IsDir   bool   `json:"is_dir"`
	Size    int64  `json:"size"`
	ModTime string `json:"mod_time"`
	SHA256  string `json:"sha256"`
}

func (m *Module) fileTree(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	q := r.URL.Query()
	dirPath, err := cleanPath("path", q.Get("path"))
	if err != nil {
		return err
	}
	sp, err := m.resolveSpace(ctx, p, q.Get("space_id"))
	if err != nil {
		return err
	}
	dir, err := m.dirRow(ctx, sp, p, dirPath)
	if err != nil {
		return err
	}
	cond, args := subtreeCond(dir.Path)
	var n int
	if err := m.d.DB.SQL.QueryRowContext(ctx, `SELECT count(*) FROM files f WHERE f.space_id = ? AND f.trashed_at IS NULL AND `+cond,
		append([]any{sp.ID}, args...)...).Scan(&n); err != nil {
		return err
	}
	if n > maxTreeEntries {
		return core.NewError(413, "payload_too_large", fmt.Sprintf("The folder has %d entries; the tree is limited to %d", n, maxTreeEntries))
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	bw := bufio.NewWriterSize(w, 64<<10)
	_, _ = bw.WriteString(`{"items":[`)
	first := true
	last := ""
	written := 0
	for written < maxTreeEntries {
		rs, err := m.d.DB.SQL.QueryContext(ctx, `SELECT f.id, f.path, f.is_dir, f.size, f.mod_time, f.sha256 FROM files f
			WHERE f.space_id = ? AND f.trashed_at IS NULL AND `+cond+` AND f.path > ? ORDER BY f.path LIMIT 5000`,
			append(append([]any{sp.ID}, args...), last)...)
		if err != nil {
			return err
		}
		count := 0
		for rs.Next() {
			var it treeItem
			var isDir int64
			var mt db.Time
			if err := rs.Scan(&it.ID, &it.Path, &isDir, &it.Size, &mt, &it.SHA256); err != nil {
				rs.Close()
				return err
			}
			it.IsDir, it.ModTime = isDir == 1, db.FormatTime(mt.Time)
			b, _ := json.Marshal(it)
			if !first {
				_ = bw.WriteByte(',')
			}
			first = false
			_, _ = bw.Write(b)
			last = it.Path
			count++
			written++
		}
		err = rs.Err()
		rs.Close()
		if err != nil {
			return err
		}
		if count < 5000 {
			break
		}
	}
	_, _ = bw.WriteString("]}\n")
	return bw.Flush()
}

// subtreeCond selects the descendants of dirPath (excluding itself) in table alias f.
func subtreeCond(dirPath string) (string, []any) {
	if dirPath == "" {
		return `f.path <> ''`, nil
	}
	lo, hi := subtreeBounds(dirPath)
	return `f.path >= ? AND f.path < ?`, []any{lo, hi}
}

/* ------------------------------------------------------------------- search */

type searchCursor struct {
	Off int    `json:"o"`
	Q   string `json:"q"`
}

// listedSpaceIDs are the spaces shown in GET /spaces: the caller's home and the libraries
// they belong to (admins: every library). Other users' homes are never searched implicitly.
func (s *Service) listedSpaceIDs(ctx context.Context, p *core.Principal) ([]string, error) {
	var rs *sql.Rows
	var err error
	if p.IsAdmin() {
		rs, err = s.d.DB.SQL.QueryContext(ctx, `SELECT id FROM spaces WHERE (kind = 'home' AND owner_id = ?) OR kind = 'library'`, p.UserID)
	} else {
		rs, err = s.d.DB.SQL.QueryContext(ctx, `SELECT id FROM spaces WHERE kind = 'home' AND owner_id = ?1
			UNION SELECT space_id FROM space_members WHERE user_id IN (?1, '*')`, p.UserID)
	}
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []string
	for rs.Next() {
		var id string
		if err := rs.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rs.Err()
}

func (m *Module) searchFiles(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	q := r.URL.Query()
	limit := httpx.ParseLimit(r, 50, 200)
	text := strings.TrimSpace(q.Get("q"))
	if len(text) > 500 {
		return core.Validation(map[string]string{"q": "query is too long"})
	}
	var spaceIDs []string
	if sid := q.Get("space_id"); sid != "" {
		sp, _, err := m.svc.spaceFor(ctx, m.d.DB.SQL, p, sid, accRead)
		if err != nil {
			return err
		}
		spaceIDs = []string{sp.ID}
	} else {
		ids, err := m.svc.listedSpaceIDs(ctx, p)
		if err != nil {
			return err
		}
		spaceIDs = ids
	}
	if len(spaceIDs) == 0 {
		return httpx.WriteJSON(w, http.StatusOK, httpx.NewPage([]APIEntry{}, ""))
	}
	from := `files f`
	where := []string{`f.trashed_at IS NULL`, `f.path <> ''`}
	var args, orderArgs []any
	order := `f.mod_time DESC, f.id`
	fingerprint := q.Encode()
	if text != "" {
		match := db.FTS5Query(text)
		if match == "" {
			return httpx.WriteJSON(w, http.StatusOK, httpx.NewPage([]APIEntry{}, ""))
		}
		from = `files_fts JOIN files_fts_map fm ON fm.rowid = files_fts.rowid JOIN files f ON f.id = fm.file_id`
		where = append(where, `files_fts MATCH ?`)
		args = append(args, match)
		// Rank by where the terms matched in the row itself: all in the name, then all in name or
		// tags, then anywhere (path). bm25() is not used: files_fts is one index shared by every
		// space, and bm25's corpus statistics (row count, document frequency, average length)
		// would let other users' files change the order of a caller's results — a
		// document-frequency oracle on names in spaces the caller cannot read.
		order = `CASE WHEN fm.rowid IN (SELECT rowid FROM files_fts WHERE files_fts MATCH ?) THEN 0
			WHEN fm.rowid IN (SELECT rowid FROM files_fts WHERE files_fts MATCH ?) THEN 1 ELSE 2 END, f.mod_time DESC, f.id`
		orderArgs = append(orderArgs, "{name} : ("+match+")", "{name tags} : ("+match+")")
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(spaceIDs)), ",")
	where = append(where, `f.space_id IN (`+ph+`)`)
	for _, id := range spaceIDs {
		args = append(args, id)
	}
	if v := q.Get("category"); v != "" {
		where = append(where, `f.category = ?`)
		args = append(args, v)
	}
	if v := q.Get("subcategory"); v != "" {
		where = append(where, `f.subcategory = ?`)
		args = append(args, v)
	}
	if v := strings.TrimSpace(q.Get("tag")); v != "" {
		where = append(where, `EXISTS (SELECT 1 FROM json_each(f.tags) t WHERE t.value = ? COLLATE NOCASE)`)
		args = append(args, v)
	}
	switch q.Get("type") {
	case "":
	case "file":
		where = append(where, `f.is_dir = 0`)
	case "dir":
		where = append(where, `f.is_dir = 1`)
	default:
		return core.Validation(map[string]string{"type": "must be file or dir"})
	}
	if v := q.Get("mime_prefix"); v != "" {
		if len(v) > 100 {
			return core.Validation(map[string]string{"mime_prefix": "too long"})
		}
		where = append(where, `substr(f.mime, 1, length(?)) = ?`)
		args = append(args, v, v)
	}
	off := 0
	if c := q.Get("cursor"); c != "" {
		var cur searchCursor
		if err := httpx.DecodeCursor(c, &cur); err != nil || cur.Q != cursorFingerprint(fingerprint) || cur.Off < 0 || cur.Off > 100_000 {
			return httpx.ErrInvalidCursor
		}
		off = cur.Off
	}
	rows, err := queryRows(ctx, m.d.DB.SQL, `SELECT `+entryCols+` FROM `+from+` WHERE `+strings.Join(where, " AND ")+
		` ORDER BY `+order+` LIMIT ? OFFSET ?`, append(append(args, orderArgs...), limit+1, off)...)
	if err != nil {
		return err
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		next = httpx.EncodeCursor(searchCursor{Off: off + limit, Q: cursorFingerprint(fingerprint)})
	}
	caps := m.svc.caps(ctx)
	items := make([]APIEntry, 0, len(rows))
	for _, x := range rows {
		items = append(items, rowAPI(x, caps))
	}
	return httpx.WriteJSON(w, http.StatusOK, httpx.NewPage(items, next))
}

// cursorFingerprint binds a search cursor to its query (minus the cursor itself).
func cursorFingerprint(encoded string) string {
	parts := strings.Split(encoded, "&")
	keep := parts[:0]
	for _, p := range parts {
		if !strings.HasPrefix(p, "cursor=") && !strings.HasPrefix(p, "limit=") {
			keep = append(keep, p)
		}
	}
	s := strings.Join(keep, "&")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

/* --------------------------------------------------------------- one entry */

func (m *Module) getFile(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	row, _, err := m.svc.entryFor(ctx, m.d.DB.SQL, core.MustPrincipal(ctx), id, accRead)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, rowAPI(row, m.svc.caps(ctx)))
}

type entryPatch struct {
	Name        *string   `json:"name"`
	Category    *string   `json:"category"`
	Subcategory *string   `json:"subcategory"`
	Tags        *[]string `json:"tags"`
}

func (m *Module) patchFile(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in entryPatch
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	// Validate everything before changing anything.
	if in.Name != nil {
		if err := validName("name", *in.Name); err != nil {
			return err
		}
	}
	if in.Tags != nil {
		if _, err := normalizeTags("tags", *in.Tags); err != nil {
			return err
		}
	}
	cur, _, err := m.svc.entryFor(ctx, m.d.DB.SQL, p, id, accWrite)
	if err != nil {
		return err
	}
	if in.Category != nil || in.Subcategory != nil {
		cat, sub := cur.Category, cur.Subcategory
		if in.Category != nil {
			cat = strings.TrimSpace(*in.Category)
			if cat != cur.Category && in.Subcategory == nil {
				sub = ""
			}
		}
		if in.Subcategory != nil {
			sub = strings.TrimSpace(*in.Subcategory)
		}
		if err := m.svc.checkCategory(ctx, cat, sub); err != nil {
			return err
		}
	}
	if in.Name != nil && *in.Name != cur.Name {
		if _, err := m.svc.Rename(ctx, p, id, *in.Name); err != nil {
			return err
		}
	}
	e, err := m.svc.SetMeta(ctx, p, id, in.Category, in.Subcategory, in.Tags)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, m.svc.entryAPI(ctx, e))
}

func (m *Module) trashFile(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	if err := m.svc.Trash(ctx, core.MustPrincipal(ctx), id); err != nil {
		return err
	}
	return httpx.NoContent(w)
}

type folderReq struct {
	SpaceID string `json:"space_id"`
	Path    string `json:"path"`
	Name    string `json:"name"`
}

func (m *Module) createFolder(w http.ResponseWriter, r *http.Request) error {
	var in folderReq
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	if in.SpaceID == "" {
		id, err := m.svc.HomeSpaceID(ctx, p.UserID)
		if err != nil {
			return err
		}
		in.SpaceID = id
	}
	e, err := m.svc.CreateFolder(ctx, p, in.SpaceID, in.Path, in.Name)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusCreated, m.svc.entryAPI(ctx, e))
}

/* ------------------------------------------------------------- bulk calls */

type transferReq struct {
	IDs         []string `json:"ids"`
	DestSpaceID string   `json:"dest_space_id"`
	DestPath    string   `json:"dest_path"`
	OnConflict  string   `json:"on_conflict"`
}

func checkIDs(ids []string) error {
	if len(ids) == 0 || len(ids) > 1000 {
		return core.Validation(map[string]string{"ids": "1-1000 ids are required"})
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !httpx.ValidID(id) {
			return core.Validation(map[string]string{"ids": "invalid id " + strconv.Quote(id)})
		}
		if seen[id] {
			return core.Validation(map[string]string{"ids": "duplicate id " + id})
		}
		seen[id] = true
	}
	return nil
}

func (m *Module) decodeTransfer(r *http.Request) (*transferReq, error) {
	var in transferReq
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return nil, err
	}
	if err := checkIDs(in.IDs); err != nil {
		return nil, err
	}
	switch in.OnConflict {
	case "":
		in.OnConflict = ConflictFail
	case ConflictFail, ConflictRename, ConflictOverwrite:
	default:
		return nil, core.Validation(map[string]string{"on_conflict": "must be fail, rename or overwrite"})
	}
	if in.DestSpaceID == "" {
		return nil, core.Validation(map[string]string{"dest_space_id": "required"})
	}
	return &in, nil
}

func (m *Module) apiEntries(ctx context.Context, es []Entry) []APIEntry {
	out := make([]APIEntry, 0, len(es))
	for i := range es {
		out = append(out, m.svc.entryAPI(ctx, &es[i]))
	}
	return out
}

func (m *Module) moveFiles(w http.ResponseWriter, r *http.Request) error {
	in, err := m.decodeTransfer(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	moved, errs, err := m.svc.Move(ctx, core.MustPrincipal(ctx), in.IDs, in.DestSpaceID, in.DestPath, in.OnConflict)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, map[string]any{"moved": m.apiEntries(ctx, moved), "errors": errs})
}

func (m *Module) copyFiles(w http.ResponseWriter, r *http.Request) error {
	in, err := m.decodeTransfer(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	copied, errs, err := m.svc.Copy(ctx, core.MustPrincipal(ctx), in.IDs, in.DestSpaceID, in.DestPath, in.OnConflict)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, map[string]any{"copied": m.apiEntries(ctx, copied), "errors": errs})
}

func (m *Module) bulkDelete(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		IDs []string `json:"ids"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	if err := checkIDs(in.IDs); err != nil {
		return err
	}
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	deleted, errs := []string{}, []ItemError{}
	for _, id := range in.IDs {
		if err := m.svc.Trash(ctx, p, id); err != nil {
			errs = append(errs, itemError(id, err))
			continue
		}
		deleted = append(deleted, id)
	}
	return httpx.WriteJSON(w, http.StatusOK, map[string]any{"deleted": deleted, "errors": errs})
}

type bulkMetaReq struct {
	IDs         []string `json:"ids"`
	Category    *string  `json:"category"`
	Subcategory *string  `json:"subcategory"`
	AddTags     []string `json:"add_tags"`
	RemoveTags  []string `json:"remove_tags"`
}

func (m *Module) bulkMeta(w http.ResponseWriter, r *http.Request) error {
	var in bulkMetaReq
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	if err := checkIDs(in.IDs); err != nil {
		return err
	}
	if len(in.AddTags) > 100 || len(in.RemoveTags) > 100 {
		return core.Validation(map[string]string{"add_tags": "at most 100 tags"})
	}
	if _, err := normalizeTags("add_tags", in.AddTags); err != nil {
		return err
	}
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	if in.Category != nil { // fail fast on an unknown category; per-entry checks follow
		sub := ""
		if in.Subcategory != nil {
			sub = *in.Subcategory
		}
		if err := m.svc.checkCategory(ctx, strings.TrimSpace(*in.Category), strings.TrimSpace(sub)); err != nil {
			return err
		}
	}
	updated := 0
	errs := []ItemError{}
	for _, id := range in.IDs {
		cur, _, err := m.svc.entryFor(ctx, m.d.DB.SQL, p, id, accWrite)
		if err != nil {
			errs = append(errs, itemError(id, err))
			continue
		}
		e, changed, err := m.svc.applyMeta(ctx, cur, in.Category, in.Subcategory, nil, in.AddTags, in.RemoveTags)
		if err != nil {
			errs = append(errs, itemError(id, err))
			continue
		}
		if changed {
			updated++
			m.d.Events.Publish("file.updated", Event{Kind: KindMeta, Entry: *e})
		}
	}
	return httpx.WriteJSON(w, http.StatusOK, map[string]any{"updated": updated, "errors": errs})
}

/* ------------------------------------------------------------------- trash */

type trashCursor struct {
	T  string `json:"t"`
	ID string `json:"id"`
}

func (m *Module) listTrash(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	limit := httpx.ParseLimit(r, 50, 200)
	cond, args := trashScope(p)
	if c := r.URL.Query().Get("cursor"); c != "" {
		var cur trashCursor
		if err := httpx.DecodeCursor(c, &cur); err != nil {
			return err
		}
		cond += ` AND (f.trashed_at < ? OR (f.trashed_at = ? AND f.id < ?))`
		args = append(args, cur.T, cur.T, cur.ID)
	}
	rows, err := queryRows(ctx, m.d.DB.SQL, `SELECT `+entryCols+` FROM files f JOIN spaces sp ON sp.id = f.space_id
		WHERE `+cond+` ORDER BY f.trashed_at DESC, f.id DESC LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return err
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		next = httpx.EncodeCursor(trashCursor{T: db.FormatTime(last.TrashedAt.Time), ID: last.ID})
	}
	caps := m.svc.caps(ctx)
	items := make([]apiTrashEntry, 0, len(rows))
	for _, x := range rows {
		e := rowAPI(x, caps)
		e.Path = x.TrashOrigin.String
		e.HasThumbnail = false
		items = append(items, apiTrashEntry{APIEntry: e, TrashedAt: db.FormatTime(x.TrashedAt.Time), OriginalPath: x.TrashOrigin.String})
	}
	return httpx.WriteJSON(w, http.StatusOK, httpx.NewPage(items, next))
}

func (m *Module) restoreTrash(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	e, err := m.svc.Restore(ctx, core.MustPrincipal(ctx), id, httpx.QueryBool(r, "rename"))
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, m.svc.entryAPI(ctx, e))
}

func (m *Module) deleteTrash(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	if err := m.svc.DeleteTrashed(ctx, core.MustPrincipal(ctx), id); err != nil {
		return err
	}
	return httpx.NoContent(w)
}

func (m *Module) emptyTrash(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	res, err := m.svc.EmptyTrash(ctx, core.MustPrincipal(ctx))
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, map[string]any{"deleted": res.Deleted, "freed_bytes": res.Freed})
}

/* ------------------------------------------------------------------ rescan */

func (m *Module) rescan(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		SpaceID string `json:"space_id"`
		Path    string `json:"path"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	if in.SpaceID == "" {
		return core.Validation(map[string]string{"space_id": "required"})
	}
	dirPath, err := cleanPath("path", in.Path)
	if err != nil {
		return err
	}
	sp, _, err := m.svc.spaceFor(ctx, m.d.DB.SQL, p, in.SpaceID, accWrite)
	if err != nil {
		return err
	}
	if dirPath != "" {
		if d, err := rowByPath(ctx, m.d.DB.SQL, sp.ID, dirPath); err != nil {
			return err
		} else if d == nil || !d.IsDir {
			return core.NotFound("Folder not found")
		}
	}
	m.svc.ScheduleRescan(sp.ID, dirPath)
	return httpx.WriteJSON(w, http.StatusAccepted, map[string]string{"job": "rescan"})
}

// timeParam parses an optional RFC 3339 timestamp.
func timeParam(field, v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, core.Validation(map[string]string{field: "must be an RFC 3339 timestamp"})
	}
	return t.UTC(), nil
}
