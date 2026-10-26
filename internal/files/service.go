package files

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
)

// Entry is a file or directory in a space (ARCHITECTURE.md §3.2).
type Entry struct {
	ID, SpaceID, ParentID, Path, Name string // Path is space-relative, "/"-separated, no leading slash; root dir has Path ""
	IsDir                             bool
	Size                              int64
	Mime                              string
	ModTime                           time.Time
	Category, Subcategory             string
	Tags                              []string
	SHA256                            string // "" until computed
	OwnerID                           string
	CreatedAt, UpdatedAt              time.Time
}

// CreateOpts controls file creation.
type CreateOpts struct {
	Overwrite             bool
	ModTime               time.Time
	Category, Subcategory string
	Tags                  []string
}

// Event is the payload of the file.* events (published after the change committed).
//
// Kind is one of the Kind* constants. For directories Recursive is true: the change applied
// to the whole subtree, and no separate events are published for descendants (use
// Service.SubtreeIDs to enumerate them). OldPath is set for moves and renames.
type Event struct {
	Kind      string
	Entry     Entry
	OldPath   string
	OldSpace  string // previous space id for cross-space moves ("" otherwise)
	Recursive bool
}

// Event kinds.
const (
	KindCreated  = "created"  // topic file.created: new file/folder (upload, copy, mkdir, rescan)
	KindRestored = "restored" // topic file.created: restored from the trash
	KindUpdated  = "updated"  // topic file.updated: content replaced (overwrite) or metadata changed
	KindMeta     = "meta"     // topic file.updated: category/subcategory/tags/sha256 changed only
	KindMoved    = "moved"    // topic file.moved: moved (possibly to another space)
	KindRenamed  = "renamed"  // topic file.moved: renamed in place
	KindTrashed  = "trashed"  // topic file.deleted: moved to the trash (row kept, trashed_at set)
	KindDeleted  = "deleted"  // topic file.deleted: permanently deleted (row gone)
)

// Access levels to a space.
const (
	accNone = iota
	accRead
	accWrite
	accOwner
)

var accessNames = [...]string{"", "read", "write", "owner"}

// Service is the files service shared with other modules. All methods return *core.Error
// values for client errors; missing and unreadable objects are both 404 (never leak
// existence). Methods enforce space permissions for the given principal but not token
// scopes: the calling module's route enforces its own scope.
type Service struct {
	d       *core.Deps
	log     *slog.Logger
	dataDir string

	rootOnce sync.Once
	root     *os.Root // <data>
	rootErr  error

	// treeMu serialises structural changes (disk renames + index updates). Heavy I/O
	// (copying, hashing, streaming uploads) happens outside it.
	treeMu sync.Mutex

	thumbSem  chan struct{}
	hashDirty atomic.Bool
	tools     toolCache
	tax       taxonomyCache

	upMu    sync.Mutex
	upLocks map[string]*sync.Mutex

	// admitMu serialises admissions (quota + free-space check and recording the promise), so
	// concurrent requests cannot all pass against the same usage figure.
	admitMu sync.Mutex
	// resMu guards inflight, the byte promises of simple uploads and copies that are still
	// streaming or staging (tus uploads promise through their uploads row). Lock order:
	// admitMu, treeMu → resMu.
	resMu    sync.Mutex
	inflight map[*reservation]struct{}

	userMu sync.Mutex // serialises user.deleted / user.delete_files handling

	now func() time.Time
	// freeSpace reports free bytes on the volume holding path (replaceable in tests).
	freeSpace func(path string) (int64, error)
}

func newService(d *core.Deps) *Service {
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{
		d: d, log: log.With("module", "files"), dataDir: d.Config.DataDir,
		thumbSem: make(chan struct{}, 2), upLocks: map[string]*sync.Mutex{}, inflight: map[*reservation]struct{}{},
		now: core.Now, freeSpace: diskFree,
	}
}

// dataRoot returns the os.Root opened on the data directory. Disk operations on entries go
// through an area opened from it (a space, its trash or tmp; see area), never through the data
// root itself: os.Root follows symlinks that stay inside it, and the data root also holds
// other spaces, ludworks.db and secret.key.
func (s *Service) dataRoot() (*os.Root, error) {
	s.rootOnce.Do(func() {
		if err := os.MkdirAll(s.dataDir, 0o700); err != nil {
			s.rootErr = err
			return
		}
		s.root, s.rootErr = os.OpenRoot(s.dataDir)
	})
	return s.root, s.rootErr
}

// TempDir returns <data>/tmp (same volume as storage).
func (s *Service) TempDir() string { return s.d.Config.TmpDir() }

func (s *Service) storageDir() string { return s.d.Config.StorageDir() }

/* ------------------------------------------------------------------ rows */

// row is an index row with the internal columns.
type row struct {
	Entry
	TrashedAt   db.NullTime
	TrashOrigin sql.NullString
	TrashedBy   sql.NullString
	ThumbState  int
}

const entryCols = `f.id, f.space_id, f.parent_id, f.path, f.name, f.is_dir, f.size, f.mime, f.mod_time,
	f.category, f.subcategory, f.tags, f.sha256, f.owner_id, f.created_at, f.updated_at,
	f.trashed_at, f.trash_origin, f.trashed_by, f.thumb_state`

type scanner interface{ Scan(dest ...any) error }

func scanRow(sc scanner) (*row, error) {
	var (
		r       row
		parent  sql.NullString
		isDir   int64
		modTime db.Time
		created db.Time
		updated db.Time
		tags    db.JSON[[]string]
	)
	if err := sc.Scan(&r.ID, &r.SpaceID, &parent, &r.Path, &r.Name, &isDir, &r.Size, &r.Mime, &modTime,
		&r.Category, &r.Subcategory, &tags, &r.SHA256, &r.OwnerID, &created, &updated,
		&r.TrashedAt, &r.TrashOrigin, &r.TrashedBy, &r.ThumbState); err != nil {
		return nil, err
	}
	r.ParentID = parent.String
	r.IsDir = isDir == 1
	r.ModTime, r.CreatedAt, r.UpdatedAt = modTime.Time, created.Time, updated.Time
	r.Tags = tags.V
	if r.Tags == nil {
		r.Tags = []string{}
	}
	return &r, nil
}

func queryRows(ctx context.Context, q db.Querier, query string, args ...any) ([]*row, error) {
	rs, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []*row
	for rs.Next() {
		r, err := scanRow(rs)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rs.Err()
}

// rowByID loads any row (trashed or not); nil, nil when missing.
func rowByID(ctx context.Context, q db.Querier, id string) (*row, error) {
	r, err := scanRow(q.QueryRowContext(ctx, `SELECT `+entryCols+` FROM files f WHERE f.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// rowByPath loads the row at path in a space; nil, nil when missing.
func rowByPath(ctx context.Context, q db.Querier, spaceID, p string) (*row, error) {
	r, err := scanRow(q.QueryRowContext(ctx, `SELECT `+entryCols+` FROM files f WHERE f.space_id = ? AND f.path = ?`, spaceID, p))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func insertRow(ctx context.Context, q db.Querier, r *row) error {
	_, err := q.ExecContext(ctx, `INSERT INTO files (id, space_id, parent_id, path, name, is_dir, size, mime, mod_time,
		category, subcategory, tags, sha256, owner_id, trashed_at, trash_origin, trashed_by, thumb_state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.SpaceID, nullStr(r.ParentID), r.Path, r.Name, db.Bool(r.IsDir), r.Size, r.Mime, db.FormatTime(r.ModTime),
		r.Category, r.Subcategory, db.JSON[[]string]{V: r.Tags}, r.SHA256, r.OwnerID, r.TrashedAt,
		nullStr(r.TrashOrigin.String), nullStr(r.TrashedBy.String), r.ThumbState,
		db.FormatTime(r.CreatedAt), db.FormatTime(r.UpdatedAt))
	return err
}

/* ---------------------------------------------------------------- spaces */

// space is a storage root.
type space struct {
	ID, Kind, Name, OwnerID, RootDir string
	CreatedAt                        time.Time
}

func (sp *space) isHome() bool { return sp.Kind == "home" }

func scanSpace(sc scanner) (*space, error) {
	var sp space
	var owner sql.NullString
	var created db.Time
	if err := sc.Scan(&sp.ID, &sp.Kind, &sp.Name, &owner, &sp.RootDir, &created); err != nil {
		return nil, err
	}
	sp.OwnerID, sp.CreatedAt = owner.String, created.Time
	return &sp, nil
}

const spaceCols = `id, kind, name, owner_id, root_dir, created_at`

func loadSpace(ctx context.Context, q db.Querier, id string) (*space, error) {
	sp, err := scanSpace(q.QueryRowContext(ctx, `SELECT `+spaceCols+` FROM spaces WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return sp, err
}

// accessLevel resolves p's access to sp (ARCHITECTURE.md §3.3): the home owner is owner,
// admins can write everything, otherwise library membership (user or '*') applies.
func (s *Service) accessLevel(ctx context.Context, q db.Querier, p *core.Principal, sp *space) (int, error) {
	if p == nil || p.UserID == "" {
		return accNone, nil
	}
	if sp.isHome() && sp.OwnerID == p.UserID {
		return accOwner, nil
	}
	if p.IsAdmin() {
		return accWrite, nil
	}
	if sp.isHome() {
		return accNone, nil
	}
	rs, err := q.QueryContext(ctx, `SELECT access FROM space_members WHERE space_id = ? AND user_id IN (?, '*')`, sp.ID, p.UserID)
	if err != nil {
		return accNone, err
	}
	defer rs.Close()
	lvl := accNone
	for rs.Next() {
		var a string
		if err := rs.Scan(&a); err != nil {
			return accNone, err
		}
		switch a {
		case "write":
			lvl = max(lvl, accWrite)
		case "read":
			lvl = max(lvl, accRead)
		}
	}
	return lvl, rs.Err()
}

var errNoWrite = core.Forbidden("You only have read access to this space")

// spaceFor loads a space and checks access: 404 when it is missing or unreadable, 403 when
// it is readable but need is write.
func (s *Service) spaceFor(ctx context.Context, q db.Querier, p *core.Principal, spaceID string, need int) (*space, int, error) {
	if !httpx.ValidID(spaceID) {
		return nil, accNone, core.NotFound("Space not found")
	}
	sp, err := loadSpace(ctx, q, spaceID)
	if err != nil {
		return nil, accNone, err
	}
	if sp == nil {
		return nil, accNone, core.NotFound("Space not found")
	}
	lvl, err := s.accessLevel(ctx, q, p, sp)
	if err != nil {
		return nil, accNone, err
	}
	if lvl < accRead {
		return nil, accNone, core.NotFound("Space not found")
	}
	if need >= accWrite && lvl < accWrite {
		return nil, lvl, errNoWrite
	}
	return sp, lvl, nil
}

var errEntryNotFound = core.NotFound("File not found")

// entryFor loads a live (non-trashed) entry and checks access to its space.
func (s *Service) entryFor(ctx context.Context, q db.Querier, p *core.Principal, id string, need int) (*row, *space, error) {
	if !httpx.ValidID(id) {
		return nil, nil, errEntryNotFound
	}
	r, err := rowByID(ctx, q, id)
	if err != nil {
		return nil, nil, err
	}
	if r == nil || r.TrashedAt.Valid {
		return nil, nil, errEntryNotFound
	}
	sp, _, err := s.spaceFor(ctx, q, p, r.SpaceID, need)
	if err != nil {
		var ce *core.Error
		if errors.As(err, &ce) && ce.Status == 404 {
			return nil, nil, errEntryNotFound
		}
		return nil, nil, err
	}
	return r, sp, nil
}

// CanRead reports read access to a space.
func (s *Service) CanRead(ctx context.Context, p *core.Principal, spaceID string) bool {
	_, _, err := s.spaceFor(ctx, s.d.DB.SQL, p, spaceID, accRead)
	return err == nil
}

// CanWrite reports write access to a space.
func (s *Service) CanWrite(ctx context.Context, p *core.Principal, spaceID string) bool {
	_, _, err := s.spaceFor(ctx, s.d.DB.SQL, p, spaceID, accWrite)
	return err == nil
}

// ReadableSpaceIDs lists the spaces p may read (for SQL filters in other modules). For
// admins this includes every library and every home space.
func (s *Service) ReadableSpaceIDs(ctx context.Context, p *core.Principal) ([]string, error) {
	if p == nil || p.UserID == "" {
		return []string{}, nil
	}
	var (
		rs  *sql.Rows
		err error
	)
	if p.IsAdmin() {
		rs, err = s.d.DB.SQL.QueryContext(ctx, `SELECT id FROM spaces ORDER BY id`)
	} else {
		rs, err = s.d.DB.SQL.QueryContext(ctx, `SELECT id FROM spaces WHERE kind = 'home' AND owner_id = ?1
			UNION SELECT space_id FROM space_members WHERE user_id IN (?1, '*') ORDER BY 1`, p.UserID)
	}
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []string{}
	for rs.Next() {
		var id string
		if err := rs.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rs.Err()
}

// HomeSpaceID returns (creating on demand) the user's home space id. It returns 404 when the
// user does not exist.
func (s *Service) HomeSpaceID(ctx context.Context, userID string) (string, error) {
	if !httpx.ValidID(userID) {
		return "", core.NotFound("User not found")
	}
	var id string
	err := s.d.DB.SQL.QueryRowContext(ctx, `SELECT id FROM spaces WHERE kind = 'home' AND owner_id = ?`, userID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	root, err := s.dataRoot()
	if err != nil {
		return "", err
	}
	rootDir := "home/" + userID
	if err := root.MkdirAll(filepath.Join("storage", filepath.FromSlash(rootDir)), 0o700); err != nil {
		return "", fmt.Errorf("files: create home dir: %w", err)
	}
	now := s.now()
	newID := core.NewID()
	err = s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		// Another request may have created it meanwhile.
		if err := tx.QueryRowContext(ctx, `SELECT id FROM spaces WHERE kind = 'home' AND owner_id = ?`, userID).Scan(&id); err == nil {
			return nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE id = ?`, userID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return core.NotFound("User not found")
		}
		id = newID
		if _, err := tx.ExecContext(ctx, `INSERT INTO spaces (id, kind, name, owner_id, root_dir, created_at, updated_at)
			VALUES (?, 'home', 'Home', ?, ?, ?, ?)`, id, userID, rootDir, db.FormatTime(now), db.FormatTime(now)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO home_spaces (user_id, space_id) VALUES (?, ?)`, userID, id); err != nil {
			return err
		}
		return insertRootRow(ctx, tx, id, userID, now)
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

func insertRootRow(ctx context.Context, q db.Querier, spaceID, ownerID string, now time.Time) error {
	return insertRow(ctx, q, &row{Entry: Entry{
		ID: core.NewID(), SpaceID: spaceID, Path: "", Name: "", IsDir: true, Mime: DirMime, ModTime: now,
		Tags: []string{}, OwnerID: ownerID, CreatedAt: now, UpdatedAt: now,
	}})
}

// Get returns an entry (read permission enforced; trashed entries are not found).
func (s *Service) Get(ctx context.Context, p *core.Principal, id string) (*Entry, error) {
	r, _, err := s.entryFor(ctx, s.d.DB.SQL, p, id, accRead)
	if err != nil {
		return nil, err
	}
	e := r.Entry
	return &e, nil
}

// Open opens a file for reading (read permission enforced). The caller closes the file.
// Directories return 400.
func (s *Service) Open(ctx context.Context, p *core.Principal, id string) (*os.File, *Entry, error) {
	r, sp, err := s.entryFor(ctx, s.d.DB.SQL, p, id, accRead)
	if err != nil {
		return nil, nil, err
	}
	if r.IsDir {
		return nil, nil, core.BadRequest("Cannot open a directory")
	}
	f, err := s.openFile(sp, r.Path)
	if err != nil {
		return nil, nil, err
	}
	e := r.Entry
	return f, &e, nil
}

// openFile opens a regular file through the space root. A file missing on disk is 404.
func (s *Service) openFile(sp *space, p string) (*os.File, error) {
	sr, err := s.spaceRoot(sp)
	if err != nil {
		return nil, err
	}
	defer sr.Close()
	f, err := sr.Open(osRel(p))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, core.NotFound("File content is missing on disk (rescan the space)")
		}
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, core.NotFound("File content is missing on disk (rescan the space)")
	}
	return f, nil
}

// LocalPath returns the absolute on-disk path of a file (read-checked), for ffmpeg/ffprobe.
func (s *Service) LocalPath(ctx context.Context, p *core.Principal, id string) (string, error) {
	r, sp, err := s.entryFor(ctx, s.d.DB.SQL, p, id, accRead)
	if err != nil {
		return "", err
	}
	sr, err := s.spaceRoot(sp)
	if err != nil {
		return "", err
	}
	defer sr.Close()
	st, err := sr.Lstat(osRel(r.Path))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", core.NotFound("File content is missing on disk (rescan the space)")
		}
		return "", err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return "", core.NotFound("File is a symbolic link")
	}
	return filepath.Join(s.storageDir(), filepath.FromSlash(sp.RootDir), filepath.FromSlash(r.Path)), nil
}

// SubtreeIDs returns the ids of every entry below id (excluding id itself), live or trashed,
// for consumers of recursive events. It performs no permission check (the id comes from an
// event); it returns an empty list for unknown ids.
func (s *Service) SubtreeIDs(ctx context.Context, id string) ([]string, error) {
	r, err := rowByID(ctx, s.d.DB.SQL, id)
	if err != nil || r == nil || !r.IsDir {
		return []string{}, err
	}
	var rs *sql.Rows
	if r.Path == "" {
		rs, err = s.d.DB.SQL.QueryContext(ctx, `SELECT id FROM files WHERE space_id = ? AND path <> '' AND trashed_at IS NULL`, r.SpaceID)
	} else {
		lo, hi := subtreeBounds(r.Path)
		rs, err = s.d.DB.SQL.QueryContext(ctx, `SELECT id FROM files WHERE space_id = ? AND path >= ? AND path < ?`, r.SpaceID, lo, hi)
	}
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []string{}
	for rs.Next() {
		var x string
		if err := rs.Scan(&x); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rs.Err()
}

/* ------------------------------------------------------------- disk paths */

// osRel converts a live space-relative path to an OS path relative to the space root.
func osRel(p string) string {
	if p == "" {
		return "."
	}
	return filepath.FromSlash(p)
}

// diskRel returns the data-root-relative OS path of an entry path in sp: live paths live
// under storage/<root_dir>/, trashed paths ("/<id>/...") under storage/.trash/<space id>/.
func diskRel(sp *space, p string) string {
	if strings.HasPrefix(p, "/") {
		return filepath.Join("storage", ".trash", sp.ID, filepath.FromSlash(p[1:]))
	}
	base := filepath.Join("storage", filepath.FromSlash(sp.RootDir))
	if p == "" {
		return base
	}
	return filepath.Join(base, filepath.FromSlash(p))
}

// spaceRoot opens an os.Root on the space directory (created if missing). Symlinks inside a
// space can never reach another space or anything outside. The caller closes it.
func (s *Service) spaceRoot(sp *space) (*os.Root, error) {
	a, err := s.spaceArea(sp)
	if err != nil {
		return nil, err
	}
	return a.root, nil
}

/* ------------------------------------------------------------- user locks */

func (s *Service) uploadLock(id string) *sync.Mutex {
	s.upMu.Lock()
	defer s.upMu.Unlock()
	m, ok := s.upLocks[id]
	if !ok {
		m = &sync.Mutex{}
		s.upLocks[id] = m
	}
	return m
}

func (s *Service) dropUploadLock(id string) {
	s.upMu.Lock()
	delete(s.upLocks, id)
	s.upMu.Unlock()
}
