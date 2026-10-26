package system

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
)

// Bounds that keep one request cheap however large the installation is.
const (
	maxSpaces       = 1000   // spaces listed (and library roots measured)
	maxUsers        = 10000  // rows in the admin per-user table
	maxUploadParts  = 100000 // directory entries scanned in <data>/tmp/uploads
	statDiskTimeout = 5 * time.Second
	maxVolumeLabels = 3 // labels joined per volume before "+N"
)

// UserUsage is the caller's own usage (API.md §4 "user").
type UserUsage struct {
	UsageBytes int64            `json:"usage_bytes"`
	QuotaBytes int64            `json:"quota_bytes"`
	ByCategory map[string]int64 `json:"by_category"`
	TrashBytes int64            `json:"trash_bytes"`
}

// Volume is one distinct filesystem backing the server's data.
type Volume struct {
	Path       string `json:"path"`
	Label      string `json:"label"`
	TotalBytes int64  `json:"total_bytes"`
	FreeBytes  int64  `json:"free_bytes"`
	UsedBytes  int64  `json:"used_bytes"`
}

// SpaceUsage is the usage of one readable space.
type SpaceUsage struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	UsageBytes int64  `json:"usage_bytes"`
	FileCount  int64  `json:"file_count"`
}

// UserStorage is one row of the admin per-user table.
type UserStorage struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	UsageBytes int64  `json:"usage_bytes"`
	QuotaBytes int64  `json:"quota_bytes"`
}

// Storage is the GET /system/storage response. Admin-only fields are empty arrays (volumes,
// users) or omitted (uploads_pending_bytes, data_dir) for everyone else.
type Storage struct {
	User                UserUsage     `json:"user"`
	Volumes             []Volume      `json:"volumes"`
	Spaces              []SpaceUsage  `json:"spaces"`
	Users               []UserStorage `json:"users"`
	UploadsPendingBytes *int64        `json:"uploads_pending_bytes,omitempty"`
	DataDir             *string       `json:"data_dir,omitempty"`
}

// isAdminView reports whether p may see server-wide storage: the admin role, and for
// personal access tokens also the "admin" scope (same rule as admin routes).
func isAdminView(p *core.Principal) bool { return p.IsAdmin() && p.Has(core.ScopeAdmin) }

func (m *Module) getStorage(w http.ResponseWriter, r *http.Request) error {
	p := core.MustPrincipal(r.Context())
	st, err := m.Storage(r.Context(), p)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, st)
}

// tableSet records which of the tables owned by other modules exist yet (the files module
// creates files/spaces/space_members; this module only reads them).
type tableSet struct{ files, spaces, members bool }

func existingTables(ctx context.Context, q db.Querier) (tableSet, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name IN ('files', 'spaces', 'space_members')`)
	if err != nil {
		return tableSet{}, err
	}
	defer rows.Close()
	var t tableSet
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return tableSet{}, err
		}
		switch name {
		case "files":
			t.files = true
		case "spaces":
			t.spaces = true
		case "space_members":
			t.members = true
		}
	}
	return t, rows.Err()
}

// libraryRoot is a library space's directory (for the volume list).
type libraryRoot struct{ name, dir string }

// Storage computes the storage overview for p.
func (m *Module) Storage(ctx context.Context, p *core.Principal) (*Storage, error) {
	admin := isAdminView(p)
	out := &Storage{
		User:    UserUsage{ByCategory: map[string]int64{}},
		Volumes: []Volume{}, Spaces: []SpaceUsage{}, Users: []UserStorage{},
	}
	var roots []libraryRoot
	err := m.d.DB.ReadTx(ctx, func(tx *sql.Tx) error {
		out.User = UserUsage{ByCategory: map[string]int64{}}
		out.Spaces, out.Users, roots = []SpaceUsage{}, []UserStorage{}, nil
		t, err := existingTables(ctx, tx)
		if err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT quota_bytes FROM users WHERE id = ?`, p.UserID).
			Scan(&out.User.QuotaBytes); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if t.files {
			if err := userUsage(ctx, tx, p.UserID, &out.User); err != nil {
				return err
			}
		}
		if t.spaces {
			if out.Spaces, err = spaceUsage(ctx, tx, t, p.UserID, admin); err != nil {
				return err
			}
		}
		if !admin {
			return nil
		}
		if out.Users, err = usersUsage(ctx, tx, t.files); err != nil {
			return err
		}
		if t.spaces {
			roots, err = libraryRoots(ctx, tx)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if !admin {
		return out, nil
	}
	out.Volumes = m.volumes(ctx, roots)
	pending := pendingUploadBytes(filepath.Join(m.d.Config.TmpDir(), "uploads"))
	dataDir := m.d.Config.DataDir
	out.UploadsPendingBytes, out.DataDir = &pending, &dataDir
	return out, nil
}

// userUsage fills usage, trash and per-category bytes of the files owned by userID.
func userUsage(ctx context.Context, q db.Querier, userID string, u *UserUsage) error {
	rows, err := q.QueryContext(ctx, `
		SELECT category, trashed_at IS NOT NULL, COALESCE(SUM(size), 0)
		FROM files WHERE owner_id = ? AND is_dir = 0
		GROUP BY category, trashed_at IS NOT NULL`, userID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cat     sql.NullString
			trashed bool
			size    int64
		)
		if err := rows.Scan(&cat, &trashed, &size); err != nil {
			return err
		}
		if trashed {
			u.TrashBytes = addSat(u.TrashBytes, size)
			continue
		}
		u.UsageBytes = addSat(u.UsageBytes, size)
		u.ByCategory[cat.String] = addSat(u.ByCategory[cat.String], size)
	}
	return rows.Err()
}

// spaceUsage lists the spaces the user can read: their own home plus every library they are
// a member of (directly or through "*"); admins see every library. Other users' homes are
// left out — admins get those numbers in the per-user table.
func spaceUsage(ctx context.Context, q db.Querier, t tableSet, userID string, admin bool) ([]SpaceUsage, error) {
	// The SQL is assembled only from constant fragments; every value is a bound parameter.
	sel := `SELECT s.id, s.name, s.kind, 0, 0 FROM spaces s`
	group := ``
	if t.files {
		sel = `SELECT s.id, s.name, s.kind, COALESCE(SUM(f.size), 0), COUNT(f.id) FROM spaces s
			LEFT JOIN files f ON f.space_id = s.id AND f.is_dir = 0 AND f.trashed_at IS NULL`
		group = ` GROUP BY s.id`
	}
	member := `0`
	if t.members {
		member = `EXISTS (SELECT 1 FROM space_members m WHERE m.space_id = s.id AND m.user_id IN (?1, '*'))`
	}
	query := sel + `
		WHERE (s.kind = 'home' AND s.owner_id = ?1) OR (s.kind = 'library' AND (?2 OR ` + member + `))` +
		group + `
		ORDER BY s.kind = 'home' DESC, s.name COLLATE NOCASE, s.id
		LIMIT ` + itoa(maxSpaces)
	rows, err := q.QueryContext(ctx, query, userID, db.Bool(admin))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SpaceUsage{}
	for rows.Next() {
		var s SpaceUsage
		var name sql.NullString
		if err := rows.Scan(&s.ID, &name, &s.Kind, &s.UsageBytes, &s.FileCount); err != nil {
			return nil, err
		}
		s.Name = name.String
		out = append(out, s)
	}
	return out, rows.Err()
}

// usersUsage returns every account with its usage (non-trashed files it owns) and quota.
func usersUsage(ctx context.Context, q db.Querier, haveFiles bool) ([]UserStorage, error) {
	query := `SELECT u.id, u.username, u.quota_bytes, 0 FROM users u`
	if haveFiles {
		query = `SELECT u.id, u.username, u.quota_bytes, COALESCE(x.total, 0) FROM users u
			LEFT JOIN (SELECT owner_id, SUM(size) AS total FROM files
			           WHERE is_dir = 0 AND trashed_at IS NULL GROUP BY owner_id) x ON x.owner_id = u.id`
	}
	rows, err := q.QueryContext(ctx, query+` ORDER BY u.username COLLATE NOCASE LIMIT `+itoa(maxUsers))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UserStorage{}
	for rows.Next() {
		var u UserStorage
		if err := rows.Scan(&u.ID, &u.Username, &u.QuotaBytes, &u.UsageBytes); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func libraryRoots(ctx context.Context, q db.Querier) ([]libraryRoot, error) {
	rows, err := q.QueryContext(ctx, `SELECT name, root_dir FROM spaces WHERE kind = 'library'
		ORDER BY name COLLATE NOCASE, id LIMIT `+itoa(maxSpaces))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []libraryRoot
	for rows.Next() {
		var name, dir sql.NullString
		if err := rows.Scan(&name, &dir); err != nil {
			return nil, err
		}
		out = append(out, libraryRoot{name: name.String, dir: dir.String})
	}
	return out, rows.Err()
}

// volumeCandidate is a directory whose filesystem is reported.
type volumeCandidate struct{ path, label string }

// candidates lists the data directory, the storage directory and every library root.
// Relative library roots are resolved under <data>/storage and must stay inside it;
// absolute roots (libraries on other disks) are used as they are.
func (m *Module) candidates(roots []libraryRoot) []volumeCandidate {
	cfg := m.d.Config
	out := []volumeCandidate{{cfg.DataDir, "data"}, {cfg.StorageDir(), "storage"}}
	for _, r := range roots {
		dir := r.dir
		switch {
		case dir == "":
			continue
		case filepath.IsAbs(dir):
			dir = filepath.Clean(dir)
		case filepath.IsLocal(dir):
			dir = filepath.Join(cfg.StorageDir(), dir)
		default:
			continue // "../x" or similar: never measure outside the storage tree
		}
		label := "library"
		if n := strings.TrimSpace(r.name); n != "" {
			label = "library: " + n
		}
		out = append(out, volumeCandidate{dir, label})
	}
	return out
}

// volumes measures each candidate directory and merges those on the same filesystem. The
// first directory seen for a filesystem is reported as its path; labels are combined.
func (m *Module) volumes(ctx context.Context, roots []libraryRoot) []Volume {
	type acc struct {
		v      Volume
		labels []string
		more   int
	}
	var order []string
	byID := map[string]*acc{}
	for _, c := range m.candidates(roots) {
		if ctx.Err() != nil {
			break
		}
		ds, err := m.statWithTimeout(c.path)
		if err != nil {
			m.log().Debug("volume not measured", "label", c.label, "err", err)
			continue
		}
		if a, ok := byID[ds.ID]; ok {
			if len(a.labels) < maxVolumeLabels {
				a.labels = append(a.labels, c.label)
			} else {
				a.more++
			}
			continue
		}
		used := ds.Total - min(ds.Free, ds.Total)
		byID[ds.ID] = &acc{v: Volume{
			Path: c.path, TotalBytes: toInt64(ds.Total), FreeBytes: toInt64(min(ds.Avail, ds.Total)), UsedBytes: toInt64(used),
		}, labels: []string{c.label}}
		order = append(order, ds.ID)
	}
	out := make([]Volume, 0, len(order))
	for _, id := range order {
		a := byID[id]
		a.v.Label = strings.Join(a.labels, ", ")
		if a.more > 0 {
			a.v.Label += " +" + itoa(a.more)
		}
		out = append(out, a.v)
	}
	return out
}

// statWithTimeout calls statDisk but gives up after statDiskTimeout, so a hung network
// mount cannot block the request (the stuck call finishes in the background).
func (m *Module) statWithTimeout(path string) (diskStat, error) {
	type res struct {
		ds  diskStat
		err error
	}
	ch := make(chan res, 1)
	go func() {
		ds, err := m.statDisk(path)
		ch <- res{ds, err}
	}()
	t := time.NewTimer(m.statTimeout)
	defer t.Stop()
	select {
	case r := <-ch:
		return r.ds, r.err
	case <-t.C:
		return diskStat{}, errors.New("statfs timed out")
	}
}

// pendingUploadBytes sums the sizes of incomplete tus uploads (<dir>/*.part). Only regular
// files count; symlinks are never followed.
func pendingUploadBytes(dir string) int64 {
	f, err := os.Open(dir)
	if err != nil {
		return 0
	}
	defer f.Close()
	var total int64
	seen := 0
	for seen < maxUploadParts {
		ents, err := f.ReadDir(256)
		for _, e := range ents {
			seen++
			if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".part") {
				continue
			}
			if info, err := e.Info(); err == nil && info.Mode().IsRegular() {
				total = addSat(total, info.Size())
			}
		}
		if err != nil { // io.EOF at the end, or a read error: report what was counted
			break
		}
	}
	return total
}

func addSat(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

func toInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}
