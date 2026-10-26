package files

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/events"
)

// pending collects events to publish once the structural lock is released.
type pending []func()

func (p *pending) add(topic string, ev Event, bus *events.Bus) {
	*p = append(*p, func() { bus.Publish(topic, ev) })
}

func (p pending) flush() {
	for _, f := range p {
		f()
	}
}

func (s *Service) ev(pd *pending, topic string, ev Event) { pd.add(topic, ev, s.d.Events) }

// ownerFor returns the owner of new content written by actor into sp: home spaces always
// belong to their owner (quota is charged there), libraries to the uploader.
func ownerFor(sp *space, actor string) string {
	if sp.isHome() {
		return sp.OwnerID
	}
	return actor
}

func actorID(p *core.Principal) string {
	if p == nil {
		return ""
	}
	return p.UserID
}

/* ------------------------------------------------------------------ quota */

// reservation is an in-memory promise of n bytes by an operation that is still streaming or
// staging data (a simple upload or a copy; tus uploads promise through their uploads row). It
// counts against owner's quota and against actor's free-space allowance until released.
type reservation struct {
	owner, actor string
	n            int64
	written      atomic.Int64 // bytes already written (they show up in the free-space probe)
}

// admit is the admission check for n new bytes charged to owner and written by actor: quota
// and free space are checked and the promise is recorded under one lock, so concurrent
// requests cannot all pass against the same usage. record (if set) stores the promise
// durably — the tus uploads row — and nil is returned; otherwise an in-memory reservation is
// returned, which the caller must release once the bytes are committed or abandoned. Commits
// re-check the quota under treeMu right before inserting, so check and insert are atomic.
func (s *Service) admit(ctx context.Context, owner, actor string, n int64, record func() error) (*reservation, error) {
	s.admitMu.Lock()
	defer s.admitMu.Unlock()
	if err := s.checkQuota(ctx, owner, n, "", nil); err != nil {
		return nil, err
	}
	if err := s.checkFree(ctx, actor, n); err != nil {
		return nil, err
	}
	if record != nil {
		return nil, record()
	}
	r := &reservation{owner: owner, actor: actor, n: n}
	s.resMu.Lock()
	s.inflight[r] = struct{}{}
	s.resMu.Unlock()
	return r, nil
}

// release drops an in-memory reservation (nil is a no-op).
func (s *Service) release(r *reservation) {
	if r == nil {
		return
	}
	s.resMu.Lock()
	delete(s.inflight, r)
	s.resMu.Unlock()
}

// reservedFor sums the in-memory reservations charged to owner, except excl.
func (s *Service) reservedFor(owner string, excl *reservation) int64 {
	s.resMu.Lock()
	defer s.resMu.Unlock()
	var n int64
	for r := range s.inflight {
		if r != excl && r.owner == owner {
			n += r.n
		}
	}
	return n
}

// pendingBy sums what actor's in-memory reservations have not written yet.
func (s *Service) pendingBy(actor string) int64 {
	s.resMu.Lock()
	defer s.resMu.Unlock()
	var n int64
	for r := range s.inflight {
		if r.actor == actor {
			n += max(r.n-r.written.Load(), 0)
		}
	}
	return n
}

// quotaUsage returns the bytes charged to userID: every file they own (home and libraries,
// trash included), the declared length of the other incomplete uploads that will be charged
// to them (into their home, or by them into a library) and the running simple uploads and
// copies charged to them (except excl).
func (s *Service) quotaUsage(ctx context.Context, q db.Querier, userID, exclUpload string, excl *reservation) (int64, error) {
	var used, reserved int64
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(size), 0) FROM files WHERE owner_id = ? AND is_dir = 0`, userID).Scan(&used); err != nil {
		return 0, err
	}
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(u.length), 0) FROM uploads u JOIN spaces sp ON sp.id = u.space_id
		WHERE u.state = 'active' AND u.id <> ? AND CASE WHEN sp.kind = 'home' THEN sp.owner_id = ? ELSE u.user_id = ? END`,
		exclUpload, userID, userID).Scan(&reserved); err != nil {
		return 0, err
	}
	return used + reserved + s.reservedFor(userID, excl), nil
}

// checkQuota fails with 507 quota_exceeded when adding n bytes for userID exceeds their quota.
// exclUpload / excl name the caller's own promise of these bytes, which is not counted twice.
func (s *Service) checkQuota(ctx context.Context, userID string, n int64, exclUpload string, excl *reservation) error {
	if userID == "" || n <= 0 {
		return nil
	}
	var quota int64
	err := s.d.DB.SQL.QueryRowContext(ctx, `SELECT quota_bytes FROM users WHERE id = ?`, userID).Scan(&quota)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && quota <= 0) {
		return nil
	}
	if err != nil {
		return err
	}
	used, err := s.quotaUsage(ctx, s.d.DB.SQL, userID, exclUpload, excl)
	if err != nil {
		return err
	}
	if used+n > quota {
		return errQuota(n, quota-used)
	}
	return nil
}

/* ---------------------------------------------------------------- folders */

// ensureDirLocked creates dirPath (and parents) on disk and in the index. treeMu is held.
func (s *Service) ensureDirLocked(ctx context.Context, sp *space, dirPath, owner string, pd *pending) (*row, error) {
	root, err := rowByPath(ctx, s.d.DB.SQL, sp.ID, "")
	if err != nil {
		return nil, err
	}
	if root == nil {
		now := s.now()
		if err := insertRootRow(ctx, s.d.DB.SQL, sp.ID, ownerFor(sp, owner), now); err != nil {
			return nil, err
		}
		if root, err = rowByPath(ctx, s.d.DB.SQL, sp.ID, ""); err != nil || root == nil {
			return nil, fmt.Errorf("files: space root row missing: %w", err)
		}
	}
	if dirPath == "" {
		return root, nil
	}
	sr, err := s.spaceRoot(sp)
	if err != nil {
		return nil, err
	}
	defer sr.Close()
	cur := root
	parts := strings.Split(dirPath, "/")
	for i := range parts {
		p := strings.Join(parts[:i+1], "/")
		r, err := rowByPath(ctx, s.d.DB.SQL, sp.ID, p)
		if err != nil {
			return nil, err
		}
		if r != nil {
			if !r.IsDir {
				return nil, core.Conflict(fmt.Sprintf("%q is a file, not a folder", p))
			}
			// An indexed folder must still be a plain directory on disk: one swapped for a link
			// behind the server's back is refused instead of written through; one deleted is
			// recreated (the index says it exists).
			switch st, err := sr.Lstat(osRel(p)); {
			case errors.Is(err, os.ErrNotExist):
				if err := sr.Mkdir(osRel(p), 0o700); err != nil && !errors.Is(err, os.ErrExist) {
					return nil, err
				}
			case err != nil:
				return nil, err
			case st.Mode().Type() != fs.ModeDir:
				return nil, errLinkOnDisk
			}
			cur = r
			continue
		}
		st, err := sr.Lstat(osRel(p))
		switch {
		case err == nil:
			// Not indexed but present: adopt it only when it is stored under exactly this
			// name. On a case-insensitive disk "CASETEST" finds "casetest", and adopting it
			// would give one directory two index paths.
			if other, alias, err := diskAlias(sr, osRel(p)); err != nil {
				return nil, err
			} else if alias {
				return nil, errNameAlias(parts[i], other)
			}
			if st.Mode().Type() != fs.ModeDir {
				return nil, core.Conflict(fmt.Sprintf("%q exists on disk and is not a folder (rescan the space)", p))
			}
		case !errors.Is(err, os.ErrNotExist):
			return nil, err
		default:
			if err := sr.Mkdir(osRel(p), 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return nil, err
			}
		}
		now := s.now()
		nr := &row{Entry: Entry{ID: core.NewID(), SpaceID: sp.ID, ParentID: cur.ID, Path: p, Name: parts[i], IsDir: true,
			Mime: DirMime, ModTime: now, Tags: []string{}, OwnerID: ownerFor(sp, owner), CreatedAt: now, UpdatedAt: now}}
		if err := s.d.DB.Tx(ctx, func(tx *sql.Tx) error { return insertRow(ctx, tx, nr) }); err != nil {
			return nil, err
		}
		s.ev(pd, events.FileCreated, Event{Kind: KindCreated, Entry: nr.Entry})
		cur = nr
	}
	return cur, nil
}

// EnsureDir creates path (and parents) in a space and returns the directory entry.
func (s *Service) EnsureDir(ctx context.Context, p *core.Principal, spaceID, path string) (*Entry, error) {
	path, err := cleanPath("path", path)
	if err != nil {
		return nil, err
	}
	sp, _, err := s.spaceFor(ctx, s.d.DB.SQL, p, spaceID, accWrite)
	if err != nil {
		return nil, err
	}
	var pd pending
	s.treeMu.Lock()
	r, err := s.ensureDirLocked(ctx, sp, path, actorID(p), &pd)
	s.treeMu.Unlock()
	pd.flush()
	if err != nil {
		return nil, err
	}
	e := r.Entry
	return &e, nil
}

// CreateFolder creates name inside the existing directory parentPath (409 if it exists).
func (s *Service) CreateFolder(ctx context.Context, p *core.Principal, spaceID, parentPath, name string) (*Entry, error) {
	parentPath, err := cleanPath("path", parentPath)
	if err != nil {
		return nil, err
	}
	if err := validName("name", name); err != nil {
		return nil, err
	}
	sp, _, err := s.spaceFor(ctx, s.d.DB.SQL, p, spaceID, accWrite)
	if err != nil {
		return nil, err
	}
	var pd pending
	s.treeMu.Lock()
	defer func() { s.treeMu.Unlock(); pd.flush() }()
	parent, err := rowByPath(ctx, s.d.DB.SQL, sp.ID, parentPath)
	if err != nil {
		return nil, err
	}
	if parent == nil && parentPath == "" {
		if parent, err = s.ensureDirLocked(ctx, sp, "", actorID(p), &pd); err != nil {
			return nil, err
		}
	}
	if parent == nil || !parent.IsDir {
		return nil, core.NotFound("Parent folder not found")
	}
	target := joinPath(parentPath, name)
	if ex, err := rowByPath(ctx, s.d.DB.SQL, sp.ID, target); err != nil {
		return nil, err
	} else if ex != nil {
		return nil, core.Conflict("An item with this name already exists")
	}
	r, err := s.ensureDirLocked(ctx, sp, target, actorID(p), &pd)
	if err != nil {
		return nil, err
	}
	e := r.Entry
	return &e, nil
}

/* ------------------------------------------------------------------ commit */

// commitReq describes a file entering a space from a temp file.
type commitReq struct {
	SpaceID, DirPath, Name string
	TmpPath                string // absolute path inside TempDir
	Opts                   CreateOpts
	Mime                   string       // client hint, used only when detection is inconclusive
	ExclUpload             string       // upload whose quota reservation is being converted
	Res                    *reservation // in-memory reservation being converted (simple upload)
}

// CommitFile atomically moves a temp file (inside TempDir, same volume) into a space. With
// Overwrite an existing file is replaced and its previous version goes to the trash (the
// entry keeps its id); without it an existing name is 409. Missing parent folders are created.
func (s *Service) CommitFile(ctx context.Context, p *core.Principal, spaceID, dirPath, name, tmpPath string, o CreateOpts) (*Entry, error) {
	return s.commit(ctx, p, commitReq{SpaceID: spaceID, DirPath: dirPath, Name: name, TmpPath: tmpPath, Opts: o})
}

// CreateFromReader writes a new file from r (streamed to a temp file, then committed).
func (s *Service) CreateFromReader(ctx context.Context, p *core.Principal, spaceID, dirPath, name string, r io.Reader, o CreateOpts) (*Entry, error) {
	if err := validName("name", name); err != nil {
		return nil, err
	}
	if _, err := cleanPath("path", dirPath); err != nil {
		return nil, err
	}
	if _, _, err := s.spaceFor(ctx, s.d.DB.SQL, p, spaceID, accWrite); err != nil {
		return nil, err
	}
	tmp, err := s.createTemp("create-")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful commit
	_, err = io.CopyBuffer(s.newWriteGuard(nil, 0).wrap(tmp), ctxReader{ctx, r}, make([]byte, copyBuf))
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, mapDiskErr(err)
	}
	return s.commit(ctx, p, commitReq{SpaceID: spaceID, DirPath: dirPath, Name: name, TmpPath: tmpName, Opts: o})
}

// createTemp creates an owner-only temp file in TempDir.
func (s *Service) createTemp(prefix string) (*os.File, error) {
	if err := os.MkdirAll(s.TempDir(), 0o700); err != nil {
		return nil, err
	}
	return os.CreateTemp(s.TempDir(), prefix+"*.tmp")
}

// mapDiskErr turns "disk full" into 507.
func mapDiskErr(err error) error {
	if errors.Is(err, syscall.ENOSPC) {
		return errNoSpace
	}
	return err
}

// tmpRel converts an absolute temp path into a path relative to the tmp area, refusing
// anything outside TempDir.
func (s *Service) tmpRel(abs string) (string, error) {
	rel, err := filepath.Rel(s.TempDir(), abs)
	if err != nil || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("files: temp file %q is not inside %s", abs, s.TempDir())
	}
	return rel, nil
}

func (s *Service) commit(ctx context.Context, p *core.Principal, c commitReq) (*Entry, error) {
	if err := validName("name", c.Name); err != nil {
		return nil, err
	}
	dirPath, err := cleanPath("path", c.DirPath)
	if err != nil {
		return nil, err
	}
	normTags, err := normalizeTags("tags", c.Opts.Tags)
	if err != nil {
		return nil, err
	}
	if err := s.checkCategory(ctx, c.Opts.Category, c.Opts.Subcategory); err != nil {
		return nil, err
	}
	sp, _, err := s.spaceFor(ctx, s.d.DB.SQL, p, c.SpaceID, accWrite)
	if err != nil {
		return nil, err
	}
	tmp, err := s.tmpArea()
	if err != nil {
		return nil, err
	}
	defer tmp.Close()
	tmpRel, err := s.tmpRel(c.TmpPath)
	if err != nil {
		return nil, err
	}
	st, err := tmp.root.Lstat(tmpRel)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("files: temp file %q is not a regular file", c.TmpPath)
	}
	size := st.Size()
	// The new content belongs to whoever the quota charges: the home owner in a home space,
	// the uploader in a library (also when replacing someone else's file there).
	owner := ownerFor(sp, actorID(p))
	mt := s.sniffTemp(tmp.root, tmpRel, c.Name, c.Mime)
	modTime := c.Opts.ModTime.UTC().Truncate(time.Millisecond)
	if modTime.IsZero() || modTime.Year() < 1601 || modTime.Year() > 9999 {
		modTime = st.ModTime().UTC().Truncate(time.Millisecond)
	} else {
		_ = tmp.root.Chtimes(tmpRel, modTime, modTime)
	}

	var pd pending
	s.treeMu.Lock()
	defer func() { s.treeMu.Unlock(); pd.flush() }()

	// Quota is checked under treeMu, right before the insert: every insert that charges
	// bytes (commit, copy, move into a home) re-checks under this lock, so concurrent
	// requests cannot all pass against the same usage.
	if err := s.checkQuota(ctx, owner, size, c.ExclUpload, c.Res); err != nil {
		return nil, err
	}
	dir, err := s.ensureDirLocked(ctx, sp, dirPath, owner, &pd)
	if err != nil {
		return nil, err
	}
	sa, err := s.spaceArea(sp)
	if err != nil {
		return nil, err
	}
	defer sa.Close()
	target := joinPath(dirPath, c.Name)
	existing, err := rowByPath(ctx, s.d.DB.SQL, sp.ID, target)
	if err != nil {
		return nil, err
	}
	targetRel := osRel(target)
	diskSt, diskErr := sa.root.Lstat(targetRel)
	onDisk := diskErr == nil
	if existing == nil && onDisk {
		// An unindexed file of this exact name is replaced (with Overwrite) like an indexed
		// one; a differently-cased sibling that the disk cannot tell apart never is.
		if other, alias, err := diskAlias(sa.root, targetRel); err != nil {
			return nil, err
		} else if alias {
			return nil, errNameAlias(c.Name, other)
		}
	}
	switch {
	case existing != nil && existing.IsDir, existing == nil && onDisk && diskSt.IsDir():
		return nil, core.Conflict("A folder with this name already exists")
	case (existing != nil || onDisk) && !c.Opts.Overwrite:
		return nil, core.Conflict("A file with this name already exists")
	}
	now := s.now()
	actor := actorID(p)

	// The previous version (indexed or not) goes to the trash under a new id. It keeps its
	// owner, who already paid for those bytes and sees the item in their trash.
	var oldVersion *row
	var trash *area
	if existing != nil || onDisk {
		oldVersion = &row{Entry: Entry{ID: core.NewID(), SpaceID: sp.ID, Name: c.Name, Mime: mimeByName(c.Name),
			ModTime: now, Tags: []string{}, OwnerID: owner, CreatedAt: now, UpdatedAt: now}}
		if existing != nil {
			oldVersion.Entry = existing.Entry
			oldVersion.ID = core.NewID()
			oldVersion.ThumbState = existing.ThumbState
		} else {
			oldVersion.Size = diskSt.Size()
			oldVersion.ModTime = diskSt.ModTime().UTC().Truncate(time.Millisecond)
		}
		oldVersion.ParentID = dir.ID
		oldVersion.Path = "/" + oldVersion.ID
		oldVersion.TrashedAt = db.NewNullTime(now)
		oldVersion.TrashOrigin = sql.NullString{String: target, Valid: true}
		oldVersion.TrashedBy = sql.NullString{String: actor, Valid: actor != ""}
		if trash, err = s.trashArea(sp); err != nil {
			return nil, err
		}
		defer trash.Close()
		if err := moveBetween(ctx, sa, targetRel, trash, trashRel(oldVersion.Path)); err != nil {
			return nil, mapDiskErr(err)
		}
	}
	undoOld := func() {
		if oldVersion != nil {
			_ = moveBetween(context.Background(), trash, trashRel(oldVersion.Path), sa, targetRel)
		}
	}
	if err := moveBetween(ctx, tmp, tmpRel, sa, targetRel); err != nil {
		undoOld()
		return nil, mapDiskErr(err)
	}

	var out row
	err = s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		if oldVersion != nil {
			if err := insertRow(ctx, tx, oldVersion); err != nil {
				return err
			}
		}
		if existing != nil {
			// The entry keeps its id but the new bytes belong to owner, whose quota was just
			// checked; leaving the previous owner on it would charge them for content they
			// never uploaded (and let the uploader bypass their own quota).
			out = *existing
			out.Size, out.Mime, out.ModTime, out.SHA256, out.ThumbState, out.UpdatedAt, out.OwnerID = size, mt, modTime, "", 0, now, owner
			if c.Opts.Category != "" {
				out.Category, out.Subcategory = c.Opts.Category, c.Opts.Subcategory
			}
			if len(normTags) > 0 {
				out.Tags = normTags
			}
			_, err := tx.ExecContext(ctx, `UPDATE files SET size = ?, mime = ?, mod_time = ?, sha256 = '', thumb_state = 0,
				category = ?, subcategory = ?, tags = ?, owner_id = ?, updated_at = ? WHERE id = ?`,
				size, mt, db.FormatTime(modTime), out.Category, out.Subcategory, db.JSON[[]string]{V: out.Tags}, owner, db.FormatTime(now), out.ID)
			return err
		}
		out = row{Entry: Entry{ID: core.NewID(), SpaceID: sp.ID, ParentID: dir.ID, Path: target, Name: c.Name, Size: size,
			Mime: mt, ModTime: modTime, Category: c.Opts.Category, Subcategory: c.Opts.Subcategory, Tags: normTags,
			OwnerID: owner, CreatedAt: now, UpdatedAt: now}}
		return insertRow(ctx, tx, &out)
	})
	if err != nil {
		_ = moveBetween(context.Background(), sa, targetRel, tmp, tmpRel)
		undoOld()
		return nil, err
	}
	if oldVersion != nil {
		s.ev(&pd, events.FileDeleted, Event{Kind: KindTrashed, Entry: oldVersion.Entry})
	}
	if existing != nil {
		s.ev(&pd, events.FileUpdated, Event{Kind: KindUpdated, Entry: out.Entry})
	} else {
		s.ev(&pd, events.FileCreated, Event{Kind: KindCreated, Entry: out.Entry})
	}
	s.scheduleHash()
	e := out.Entry
	return &e, nil
}

// sniffTemp detects the MIME type of a staged file.
func (s *Service) sniffTemp(root *os.Root, rel, name, hint string) string {
	mt := "application/octet-stream"
	if f, err := root.Open(rel); err == nil {
		mt = detectMimeReader(name, f)
		f.Close()
	} else {
		mt = detectMime(name, nil)
	}
	if mt == "application/octet-stream" && hint != "" {
		if h := strings.ToLower(strings.TrimSpace(hint)); validMimeHint(h) {
			return h
		}
	}
	return mt
}

// validMimeHint accepts a client MIME hint only from an allowlist (hintMimes: the known
// extension types minus active content). A denylist cannot work here: browsers execute many
// more types as JavaScript than any list of "bad" types names.
func validMimeHint(m string) bool { return hintMimes[m] && !activeContent(m) }

/* ------------------------------------------------------------------- meta */

// SetMeta updates category/subcategory/tags (nil = unchanged). Changing the category clears
// a subcategory that does not belong to the new category.
func (s *Service) SetMeta(ctx context.Context, p *core.Principal, id string, category, subcategory *string, tags *[]string) (*Entry, error) {
	r, _, err := s.entryFor(ctx, s.d.DB.SQL, p, id, accWrite)
	if err != nil {
		return nil, err
	}
	out, changed, err := s.applyMeta(ctx, r, category, subcategory, tags, nil, nil)
	if err != nil || !changed {
		return out, err
	}
	s.d.Events.Publish(events.FileUpdated, Event{Kind: KindMeta, Entry: *out})
	return out, nil
}

// applyMeta computes and stores new metadata; add/remove apply to tags after a replacement.
func (s *Service) applyMeta(ctx context.Context, r *row, category, subcategory *string, tags *[]string, addTags, removeTags []string) (*Entry, bool, error) {
	cat, sub := r.Category, r.Subcategory
	if category != nil {
		cat = strings.TrimSpace(*category)
		if cat != r.Category && subcategory == nil {
			sub = ""
		}
	}
	if subcategory != nil {
		sub = strings.TrimSpace(*subcategory)
	}
	if err := s.checkCategory(ctx, cat, sub); err != nil {
		return nil, false, err
	}
	newTags := r.Tags
	if tags != nil {
		t, err := normalizeTags("tags", *tags)
		if err != nil {
			return nil, false, err
		}
		newTags = t
	}
	if len(addTags) > 0 || len(removeTags) > 0 {
		rm := map[string]bool{}
		for _, t := range removeTags {
			rm[strings.ToLower(strings.TrimSpace(t))] = true
		}
		merged := make([]string, 0, len(newTags)+len(addTags))
		for _, t := range append(append([]string{}, newTags...), addTags...) {
			if !rm[strings.ToLower(strings.TrimSpace(t))] {
				merged = append(merged, t)
			}
		}
		t, err := normalizeTags("add_tags", merged)
		if err != nil {
			return nil, false, err
		}
		newTags = t
	}
	e := r.Entry
	if cat == r.Category && sub == r.Subcategory && equalTags(newTags, r.Tags) {
		return &e, false, nil
	}
	now := s.now()
	err := s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE files SET category = ?, subcategory = ?, tags = ?, updated_at = ? WHERE id = ? AND trashed_at IS NULL`,
			cat, sub, db.JSON[[]string]{V: newTags}, db.FormatTime(now), r.ID)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	e.Category, e.Subcategory, e.Tags, e.UpdatedAt = cat, sub, newTags, now
	return &e, true, nil
}

func equalTags(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

/* ------------------------------------------------------------------ rename */

// Rename renames an entry in place (409 if the name is taken).
func (s *Service) Rename(ctx context.Context, p *core.Principal, id, newName string) (*Entry, error) {
	if err := validName("name", newName); err != nil {
		return nil, err
	}
	r, sp, err := s.entryFor(ctx, s.d.DB.SQL, p, id, accWrite)
	if err != nil {
		return nil, err
	}
	if r.Path == "" {
		return nil, core.BadRequest("The space root cannot be renamed")
	}
	if r.Name == newName {
		e := r.Entry
		return &e, nil
	}
	var pd pending
	s.treeMu.Lock()
	defer func() { s.treeMu.Unlock(); pd.flush() }()
	if r, err = rowByID(ctx, s.d.DB.SQL, id); err != nil {
		return nil, err
	} else if r == nil || r.TrashedAt.Valid || r.SpaceID != sp.ID {
		return nil, errEntryNotFound
	}
	dir, _ := splitPath(r.Path)
	target := joinPath(dir, newName)
	sa, err := s.spaceArea(sp)
	if err != nil {
		return nil, err
	}
	defer sa.Close()
	root := sa.root // confined to the space: a symlink inside it cannot lead elsewhere
	if ex, err := rowByPath(ctx, s.d.DB.SQL, sp.ID, target); err != nil {
		return nil, err
	} else if ex != nil {
		return nil, core.Conflict("An item with this name already exists")
	}
	if st, err := root.Lstat(osRel(target)); err == nil {
		// Case-only renames on case-insensitive file systems see the source itself.
		src, serr := root.Lstat(osRel(r.Path))
		if serr != nil || !os.SameFile(st, src) {
			if other, alias, err := diskAlias(root, osRel(target)); err == nil && alias {
				return nil, errNameAlias(newName, other)
			}
			return nil, core.Conflict("An item with this name already exists on disk (rescan the space)")
		}
	}
	if err := root.Rename(osRel(r.Path), osRel(target)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, core.NotFound("File content is missing on disk (rescan the space)")
		}
		return nil, err
	}
	now := s.now()
	err = s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		if err := moveSubtreeRows(ctx, tx, sp.ID, r.Path, sp.ID, target, "", now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE files SET name = ?, mime = CASE WHEN is_dir = 1 THEN mime ELSE ? END WHERE id = ?`,
			newName, s.renamedMime(r, newName), r.ID)
		return err
	})
	if err != nil {
		_ = root.Rename(osRel(target), osRel(r.Path))
		return nil, err
	}
	out, err := rowByID(ctx, s.d.DB.SQL, id)
	if err != nil || out == nil {
		return nil, fmt.Errorf("files: reload after rename: %w", err)
	}
	s.ev(&pd, events.FileMoved, Event{Kind: KindRenamed, Entry: out.Entry, OldPath: r.Path, Recursive: r.IsDir})
	e := out.Entry
	return &e, nil
}

// renamedMime re-derives the MIME type from a new extension when it is specific.
func (s *Service) renamedMime(r *row, newName string) string {
	if m := mimeByName(newName); m != "" && m != "application/octet-stream" {
		return m
	}
	if mimeByName(r.Name) == r.Mime { // type came from the old extension: sniff again
		if m := mimeByName(newName); m != "" {
			return m
		}
		return "application/octet-stream"
	}
	return r.Mime
}

// moveSubtreeRows rewrites the space and path prefix of a subtree (the entry at oldPath and
// every descendant). newOwner != "" also reassigns ownership.
func moveSubtreeRows(ctx context.Context, tx *sql.Tx, oldSpace, oldPath, newSpace, newPath, newOwner string, now time.Time) error {
	lo, hi := subtreeBounds(oldPath)
	q := `UPDATE files SET space_id = ?, path = ? || substr(path, length(?) + 1), updated_at = ?,
		owner_id = CASE WHEN ? <> '' THEN ? ELSE owner_id END
		WHERE space_id = ? AND (path = ? OR (path >= ? AND path < ?))`
	_, err := tx.ExecContext(ctx, q, newSpace, newPath, oldPath, db.FormatTime(now), newOwner, newOwner, oldSpace, oldPath, lo, hi)
	return err
}

/* ------------------------------------------------------------- move & copy */

// Conflict policies for move/copy.
const (
	ConflictFail      = "fail"
	ConflictRename    = "rename"
	ConflictOverwrite = "overwrite"
)

// ItemError is a per-item failure of a bulk operation.
type ItemError struct {
	ID    string `json:"id"`
	Code  string `json:"code,omitempty"`
	Error string `json:"error"`
}

func itemError(id string, err error) ItemError {
	var ce *core.Error
	if errors.As(err, &ce) {
		return ItemError{ID: id, Code: ce.Code, Error: ce.Message}
	}
	return ItemError{ID: id, Code: "internal", Error: "Internal error"}
}

// resolveTarget picks the destination name per policy (treeMu held). root is the destination
// space's root. It returns the target path and the existing row to overwrite (nil when free).
func (s *Service) resolveTarget(ctx context.Context, root *os.Root, dsp *space, dir, name string, isDir bool, policy string) (string, *row, error) {
	target := joinPath(dir, name)
	ex, err := rowByPath(ctx, s.d.DB.SQL, dsp.ID, target)
	if err != nil {
		return "", nil, err
	}
	_, derr := root.Lstat(osRel(target))
	if ex == nil && derr != nil {
		return target, nil, nil
	}
	switch policy {
	case ConflictRename:
		for n := 1; n < 10000; n++ {
			cand := joinPath(dir, numberedName(name, isDir, n))
			r, err := rowByPath(ctx, s.d.DB.SQL, dsp.ID, cand)
			if err != nil {
				return "", nil, err
			}
			if _, derr := root.Lstat(osRel(cand)); r == nil && derr != nil {
				return cand, nil, nil
			}
		}
		return "", nil, core.Conflict("Could not find a free name")
	case ConflictOverwrite:
		if ex == nil {
			if other, alias, err := diskAlias(root, osRel(target)); err == nil && alias {
				return "", nil, errNameAlias(name, other)
			}
			return "", nil, core.Conflict("An item with this name exists on disk but not in the index (rescan the space)")
		}
		return target, ex, nil
	default:
		return "", nil, core.Conflict("An item with this name already exists at the destination")
	}
}

// subtreeSize sums file sizes in a live subtree, optionally only those not owned by owner.
func subtreeSize(ctx context.Context, q db.Querier, r *row, notOwner string) (int64, error) {
	if !r.IsDir {
		if notOwner != "" && r.OwnerID == notOwner {
			return 0, nil
		}
		return r.Size, nil
	}
	lo, hi := subtreeBounds(r.Path)
	var n int64
	err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(size), 0) FROM files WHERE space_id = ? AND is_dir = 0
		AND path >= ? AND path < ? AND owner_id <> ?`, r.SpaceID, lo, hi, notOwner).Scan(&n)
	return n, err
}

// destDirFor validates the destination of a move/copy.
func (s *Service) destDirFor(ctx context.Context, p *core.Principal, spaceID, dirPath string) (*space, *row, error) {
	dirPath, err := cleanPath("dest_path", dirPath)
	if err != nil {
		return nil, nil, err
	}
	dsp, _, err := s.spaceFor(ctx, s.d.DB.SQL, p, spaceID, accWrite)
	if err != nil {
		return nil, nil, err
	}
	dir, err := rowByPath(ctx, s.d.DB.SQL, dsp.ID, dirPath)
	if err != nil {
		return nil, nil, err
	}
	if dir == nil && dirPath == "" {
		var pd pending
		s.treeMu.Lock()
		dir, err = s.ensureDirLocked(ctx, dsp, "", actorID(p), &pd)
		s.treeMu.Unlock()
		pd.flush()
		if err != nil {
			return nil, nil, err
		}
	}
	if dir == nil || !dir.IsDir || dir.TrashedAt.Valid {
		return nil, nil, core.NotFound("Destination folder not found")
	}
	return dsp, dir, nil
}

// Move moves entries into a directory (possibly in another space).
func (s *Service) Move(ctx context.Context, p *core.Principal, ids []string, destSpace, destPath, policy string) ([]Entry, []ItemError, error) {
	dsp, dir, err := s.destDirFor(ctx, p, destSpace, destPath)
	if err != nil {
		return nil, nil, err
	}
	moved, errs := []Entry{}, []ItemError{}
	for _, id := range ids {
		e, err := s.moveOne(ctx, p, id, dsp, dir.ID, policy)
		if err != nil {
			errs = append(errs, itemError(id, err))
			continue
		}
		moved = append(moved, *e)
	}
	return moved, errs, nil
}

func (s *Service) moveOne(ctx context.Context, p *core.Principal, id string, dsp *space, dirID, policy string) (*Entry, error) {
	r, sp, err := s.entryFor(ctx, s.d.DB.SQL, p, id, accWrite)
	if err != nil {
		return nil, err
	}
	if r.Path == "" {
		return nil, core.BadRequest("The space root cannot be moved")
	}
	var pd pending
	s.treeMu.Lock()
	defer func() { s.treeMu.Unlock(); pd.flush() }()
	// Re-read under the lock: both may have changed since the permission check.
	if r, err = rowByID(ctx, s.d.DB.SQL, id); err != nil {
		return nil, err
	} else if r == nil || r.TrashedAt.Valid || r.SpaceID != sp.ID {
		return nil, errEntryNotFound
	}
	dir, err := rowByID(ctx, s.d.DB.SQL, dirID)
	if err != nil {
		return nil, err
	} else if dir == nil || dir.TrashedAt.Valid || !dir.IsDir || dir.SpaceID != dsp.ID {
		return nil, core.NotFound("Destination folder not found")
	}
	if r.SpaceID == dsp.ID && r.IsDir && isWithin(dir.Path, r.Path) {
		return nil, core.BadRequest("A folder cannot be moved into itself")
	}
	if r.SpaceID == dsp.ID && r.ParentID == dir.ID {
		e := r.Entry
		return &e, nil // already there
	}
	from, err := s.spaceArea(sp)
	if err != nil {
		return nil, err
	}
	defer from.Close()
	to := from
	if dsp.ID != sp.ID {
		if to, err = s.spaceArea(dsp); err != nil {
			return nil, err
		}
		defer to.Close()
	}
	target, overwrite, err := s.resolveTarget(ctx, to.root, dsp, dir.Path, r.Name, r.IsDir, policy)
	if err != nil {
		return nil, err
	}
	newOwner := ""
	if dsp.isHome() {
		newOwner = dsp.OwnerID
		need, err := subtreeSize(ctx, s.d.DB.SQL, r, newOwner)
		if err != nil {
			return nil, err
		}
		if err := s.checkQuota(ctx, newOwner, need, "", nil); err != nil {
			return nil, err
		}
	}
	if overwrite != nil {
		if overwrite.ID == r.ID {
			e := r.Entry
			return &e, nil
		}
		if err := s.trashLocked(ctx, to, dsp, overwrite, actorID(p), &pd); err != nil {
			return nil, err
		}
	}
	if dir.Path != "" {
		if err := to.root.MkdirAll(osRel(dir.Path), 0o700); err != nil {
			return nil, err
		}
	}
	src, dst := osRel(r.Path), osRel(target)
	if err := moveBetween(ctx, from, src, to, dst); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, core.NotFound("File content is missing on disk (rescan the space)")
		}
		return nil, mapDiskErr(err)
	}
	_, newName := splitPath(target)
	now := s.now()
	err = s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		if err := moveSubtreeRows(ctx, tx, sp.ID, r.Path, dsp.ID, target, newOwner, now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE files SET parent_id = ?, name = ? WHERE id = ?`, dir.ID, newName, r.ID)
		return err
	})
	if err != nil {
		_ = moveBetween(context.Background(), to, dst, from, src)
		return nil, err
	}
	out, err := rowByID(ctx, s.d.DB.SQL, id)
	if err != nil || out == nil {
		return nil, fmt.Errorf("files: reload after move: %w", err)
	}
	ev := Event{Kind: KindMoved, Entry: out.Entry, OldPath: r.Path, Recursive: r.IsDir}
	if sp.ID != dsp.ID {
		ev.OldSpace = sp.ID
	}
	s.ev(&pd, events.FileMoved, ev)
	e := out.Entry
	return &e, nil
}

// Copy copies entries (recursively) into a directory, possibly in another space.
func (s *Service) Copy(ctx context.Context, p *core.Principal, ids []string, destSpace, destPath, policy string) ([]Entry, []ItemError, error) {
	dsp, dir, err := s.destDirFor(ctx, p, destSpace, destPath)
	if err != nil {
		return nil, nil, err
	}
	copied, errs := []Entry{}, []ItemError{}
	for _, id := range ids {
		e, err := s.copyOne(ctx, p, id, dsp, dir.ID, policy)
		if err != nil {
			errs = append(errs, itemError(id, err))
			continue
		}
		copied = append(copied, *e)
	}
	return copied, errs, nil
}

func (s *Service) copyOne(ctx context.Context, p *core.Principal, id string, dsp *space, dirID, policy string) (*Entry, error) {
	r, sp, err := s.entryFor(ctx, s.d.DB.SQL, p, id, accRead)
	if err != nil {
		return nil, err
	}
	if r.Path == "" {
		return nil, core.BadRequest("The space root cannot be copied")
	}
	rows := []*row{r}
	if r.IsDir {
		lo, hi := subtreeBounds(r.Path)
		sub, err := queryRows(ctx, s.d.DB.SQL, `SELECT `+entryCols+` FROM files f WHERE f.space_id = ? AND f.path >= ? AND f.path < ?
			AND f.trashed_at IS NULL ORDER BY f.path`, sp.ID, lo, hi)
		if err != nil {
			return nil, err
		}
		rows = append(rows, sub...)
	}
	if len(rows) > maxTreeEntries {
		return nil, core.NewError(413, "payload_too_large", fmt.Sprintf("Folders with more than %d entries cannot be copied", maxTreeEntries))
	}
	var total int64
	for _, x := range rows {
		total += x.Size
	}
	actor := actorID(p)
	owner := ownerFor(dsp, actor)
	// Reserve the bytes against the owner's quota and the free space now, atomically with the
	// check, so parallel copies cannot all pass against the same usage while they stage.
	res, err := s.admit(ctx, owner, actor, total, nil)
	if err != nil {
		return nil, err
	}
	defer s.release(res)

	// Copy the bytes into a staging area first (no lock held).
	srcRoot, err := s.spaceRoot(sp)
	if err != nil {
		return nil, err
	}
	defer srcRoot.Close()
	tmp, err := s.tmpArea()
	if err != nil {
		return nil, err
	}
	defer tmp.Close()
	root := tmp.root
	stage := "copy-" + core.NewID()
	defer func() { _ = root.RemoveAll(stage) }()
	guard := s.newWriteGuard(res, total)
	present := make([]*row, 0, len(rows))
	for i, x := range rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rel := stage
		if i > 0 {
			rel = filepath.Join(stage, filepath.FromSlash(strings.TrimPrefix(x.Path, r.Path+"/")))
		}
		if x.IsDir {
			if err := root.Mkdir(rel, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				if errors.Is(err, os.ErrNotExist) {
					continue // parent was missing on disk
				}
				return nil, mapDiskErr(err)
			}
			present = append(present, x)
			continue
		}
		if _, err := copyFileBetween(ctx, srcRoot, osRel(x.Path), root, rel, x.ModTime, guard); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				if i == 0 {
					return nil, core.NotFound("File content is missing on disk (rescan the space)")
				}
				continue
			}
			return nil, mapDiskErr(err)
		}
		present = append(present, x)
	}

	var pd pending
	s.treeMu.Lock()
	defer func() { s.treeMu.Unlock(); pd.flush() }()
	dir, err := rowByID(ctx, s.d.DB.SQL, dirID)
	if err != nil {
		return nil, err
	} else if dir == nil || dir.TrashedAt.Valid || !dir.IsDir || dir.SpaceID != dsp.ID {
		return nil, core.NotFound("Destination folder not found")
	}
	to, err := s.spaceArea(dsp)
	if err != nil {
		return nil, err
	}
	defer to.Close()
	target, overwrite, err := s.resolveTarget(ctx, to.root, dsp, dir.Path, r.Name, r.IsDir, policy)
	if err != nil {
		return nil, err
	}
	// Authoritative quota check, under treeMu right before the insert (the admission above
	// only reserved): what is actually inserted, not counting this copy's own reservation.
	var copied int64
	for _, x := range present {
		copied += x.Size
	}
	if err := s.checkQuota(ctx, owner, copied, "", res); err != nil {
		return nil, err
	}
	if overwrite != nil {
		if overwrite.IsDir && r.IsDir && isWithin(r.Path, overwrite.Path) && overwrite.SpaceID == r.SpaceID {
			return nil, core.BadRequest("Cannot overwrite a folder with a copy of its own content")
		}
		if err := s.trashLocked(ctx, to, dsp, overwrite, actor, &pd); err != nil {
			return nil, err
		}
	}
	if dir.Path != "" {
		if err := to.root.MkdirAll(osRel(dir.Path), 0o700); err != nil {
			return nil, err
		}
	}
	dst := osRel(target)
	if err := moveBetween(ctx, tmp, stage, to, dst); err != nil {
		return nil, mapDiskErr(err)
	}
	now := s.now()
	idMap := map[string]string{}
	newRows := make([]*row, 0, len(present))
	for i, x := range present {
		nr := &row{Entry: x.Entry, ThumbState: x.ThumbState}
		nr.ID = core.NewID()
		idMap[x.ID] = nr.ID
		nr.SpaceID = dsp.ID
		nr.OwnerID = owner
		nr.CreatedAt, nr.UpdatedAt = now, now
		if i == 0 {
			nr.Path = target
			_, nr.Name = splitPath(target)
			nr.ParentID = dir.ID
		} else {
			nr.Path = rebase(x.Path, r.Path, target)
			nr.ParentID = idMap[x.ParentID]
		}
		newRows = append(newRows, nr)
	}
	err = s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		for _, nr := range newRows {
			if err := insertRow(ctx, tx, nr); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = to.root.RemoveAll(dst)
		return nil, err
	}
	s.ev(&pd, events.FileCreated, Event{Kind: KindCreated, Entry: newRows[0].Entry, Recursive: r.IsDir})
	s.scheduleHash()
	e := newRows[0].Entry
	return &e, nil
}

/* ------------------------------------------------------------------- trash */

// Trash moves an entry (and its subtree) to the trash.
func (s *Service) Trash(ctx context.Context, p *core.Principal, id string) error {
	r, sp, err := s.entryFor(ctx, s.d.DB.SQL, p, id, accWrite)
	if err != nil {
		return err
	}
	if r.Path == "" {
		return core.BadRequest("The space root cannot be deleted")
	}
	sa, err := s.spaceArea(sp)
	if err != nil {
		return err
	}
	defer sa.Close()
	var pd pending
	s.treeMu.Lock()
	defer func() { s.treeMu.Unlock(); pd.flush() }()
	if r, err = rowByID(ctx, s.d.DB.SQL, id); err != nil {
		return err
	} else if r == nil || r.TrashedAt.Valid || r.SpaceID != sp.ID {
		return errEntryNotFound
	}
	return s.trashLocked(ctx, sa, sp, r, actorID(p), &pd)
}

// trashLocked moves r (in sa, the area of sp) to storage/.trash/<space>/<id> and marks the
// subtree trashed.
func (s *Service) trashLocked(ctx context.Context, sa *area, sp *space, r *row, actor string, pd *pending) error {
	trash, err := s.trashArea(sp)
	if err != nil {
		return err
	}
	defer trash.Close()
	trashPath := "/" + r.ID
	src, dst := osRel(r.Path), trashRel(trashPath)
	missing := false
	if err := moveBetween(ctx, sa, src, trash, dst); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return mapDiskErr(err)
		}
		missing = true // already gone on disk: still trash the index rows
	}
	now := s.now()
	err = s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		lo, hi := subtreeBounds(r.Path)
		if _, err := tx.ExecContext(ctx, `UPDATE files SET path = ? || substr(path, length(?) + 1), trashed_at = ?, updated_at = ?
			WHERE space_id = ? AND (path = ? OR (path >= ? AND path < ?))`,
			trashPath, r.Path, db.FormatTime(now), db.FormatTime(now), sp.ID, r.Path, lo, hi); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE files SET trash_origin = ?, trashed_by = ? WHERE id = ?`, r.Path, nullStr(actor), r.ID)
		return err
	})
	if err != nil {
		if !missing {
			_ = moveBetween(context.Background(), trash, dst, sa, src)
		}
		return err
	}
	e := r.Entry
	e.UpdatedAt = now
	s.ev(pd, events.FileDeleted, Event{Kind: KindTrashed, Entry: e, Recursive: r.IsDir})
	return nil
}

// ownsInTrashedSubtree reports whether userID owns any entry of the trashed root r's subtree
// (r included): those bytes count against their quota, so the item is in their trash.
func ownsInTrashedSubtree(ctx context.Context, q db.Querier, r *row, userID string) (bool, error) {
	if r.OwnerID == userID {
		return true, nil
	}
	lo, hi := subtreeBounds(r.Path)
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM files WHERE space_id = ? AND path >= ? AND path < ?
		AND owner_id = ? LIMIT 1)`, r.SpaceID, lo, hi, userID).Scan(&n)
	return n > 0, err
}

// trashItem loads a trashed root visible to p: in a space p can write, and either p's home,
// trashed by p, containing something p owns (and is charged for), or p is an admin.
func (s *Service) trashItem(ctx context.Context, p *core.Principal, id string) (*row, *space, error) {
	notFound := core.NotFound("Trash item not found")
	if !validIDString(id) {
		return nil, nil, notFound
	}
	r, err := rowByID(ctx, s.d.DB.SQL, id)
	if err != nil {
		return nil, nil, err
	}
	if r == nil || !r.TrashedAt.Valid || !r.TrashOrigin.Valid {
		return nil, nil, notFound
	}
	sp, err := loadSpace(ctx, s.d.DB.SQL, r.SpaceID)
	if err != nil {
		return nil, nil, err
	}
	if sp == nil {
		return nil, nil, notFound
	}
	lvl, err := s.accessLevel(ctx, s.d.DB.SQL, p, sp)
	if err != nil {
		return nil, nil, err
	}
	if lvl < accWrite {
		return nil, nil, notFound
	}
	if !(sp.isHome() || r.TrashedBy.String == p.UserID || p.IsAdmin()) {
		owns, err := ownsInTrashedSubtree(ctx, s.d.DB.SQL, r, p.UserID)
		if err != nil {
			return nil, nil, err
		}
		if !owns {
			return nil, nil, notFound
		}
	}
	return r, sp, nil
}

// Restore moves a trashed entry back to its original location. Missing parent folders are
// recreated; a taken name is 409 unless rename is set (then "name (1).ext" is used).
func (s *Service) Restore(ctx context.Context, p *core.Principal, id string, rename bool) (*Entry, error) {
	r, sp, err := s.trashItem(ctx, p, id)
	if err != nil {
		return nil, err
	}
	sa, err := s.spaceArea(sp)
	if err != nil {
		return nil, err
	}
	defer sa.Close()
	trash, err := s.trashArea(sp)
	if err != nil {
		return nil, err
	}
	defer trash.Close()
	var pd pending
	s.treeMu.Lock()
	defer func() { s.treeMu.Unlock(); pd.flush() }()
	if r, err = rowByID(ctx, s.d.DB.SQL, id); err != nil {
		return nil, err
	} else if r == nil || !r.TrashedAt.Valid || !r.TrashOrigin.Valid {
		return nil, core.NotFound("Trash item not found")
	}
	origin := r.TrashOrigin.String
	dirPath, name := splitPath(origin)
	if !strings.HasPrefix(r.Path, "/") {
		return nil, fmt.Errorf("files: trashed row %s has live path", r.ID)
	}
	dir, err := s.ensureDirLocked(ctx, sp, dirPath, r.OwnerID, &pd)
	if err != nil {
		return nil, err
	}
	policy := ConflictFail
	if rename {
		policy = ConflictRename
	}
	target, _, err := s.resolveTarget(ctx, sa.root, sp, dirPath, name, r.IsDir, policy)
	if err != nil {
		var ce *core.Error
		if !rename && errors.As(err, &ce) && ce.Status == 409 {
			return nil, core.Conflict("An item with this name already exists at the original location; restore with ?rename=1")
		}
		return nil, err
	}
	src, dst := trashRel(r.Path), osRel(target)
	if err := moveBetween(ctx, trash, src, sa, dst); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, core.NotFound("The trashed content is missing on disk")
		}
		return nil, mapDiskErr(err)
	}
	_, newName := splitPath(target)
	now := s.now()
	err = s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		lo, hi := subtreeBounds(r.Path)
		if _, err := tx.ExecContext(ctx, `UPDATE files SET path = ? || substr(path, length(?) + 1), trashed_at = NULL,
			trash_origin = NULL, trashed_by = NULL, updated_at = ?
			WHERE space_id = ? AND (path = ? OR (path >= ? AND path < ?))`,
			target, r.Path, db.FormatTime(now), sp.ID, r.Path, lo, hi); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE files SET parent_id = ?, name = ? WHERE id = ?`, dir.ID, newName, r.ID)
		return err
	})
	if err != nil {
		_ = moveBetween(context.Background(), sa, dst, trash, src)
		return nil, err
	}
	out, err := rowByID(ctx, s.d.DB.SQL, id)
	if err != nil || out == nil {
		return nil, fmt.Errorf("files: reload after restore: %w", err)
	}
	s.ev(&pd, events.FileCreated, Event{Kind: KindRestored, Entry: out.Entry, Recursive: r.IsDir})
	e := out.Entry
	return &e, nil
}

// purgeResult counts a permanent deletion.
type purgeResult struct {
	Deleted int
	Freed   int64
}

// DeleteTrashed permanently deletes a trash item.
func (s *Service) DeleteTrashed(ctx context.Context, p *core.Principal, id string) error {
	r, sp, err := s.trashItem(ctx, p, id)
	if err != nil {
		return err
	}
	var pd pending
	s.treeMu.Lock()
	_, err = s.purgeLocked(ctx, sp, r.ID, &pd)
	s.treeMu.Unlock()
	pd.flush()
	return err
}

// purgeLocked permanently deletes the trashed root id: index rows go now, disk content is
// moved out of storage and removed in the background.
func (s *Service) purgeLocked(ctx context.Context, sp *space, id string, pd *pending) (purgeResult, error) {
	var res purgeResult
	r, err := rowByID(ctx, s.d.DB.SQL, id)
	if err != nil || r == nil || !r.TrashedAt.Valid {
		return res, err
	}
	lo, hi := subtreeBounds(r.Path)
	err = s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT count(*), COALESCE(SUM(size), 0) FROM files
			WHERE space_id = ? AND (path = ? OR (path >= ? AND path < ?))`, sp.ID, r.Path, lo, hi).Scan(&res.Deleted, &res.Freed); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM files WHERE space_id = ? AND (path = ? OR (path >= ? AND path < ?))`, sp.ID, r.Path, lo, hi)
		return err
	})
	if err != nil {
		return purgeResult{}, err
	}
	s.removeLater(diskRel(sp, r.Path))
	s.ev(pd, events.FileDeleted, Event{Kind: KindDeleted, Entry: r.Entry, Recursive: r.IsDir})
	return res, nil
}

// removeLater moves rel (data-root-relative) to tmp/ and deletes it in a background job.
func (s *Service) removeLater(rel string) {
	root, err := s.dataRoot()
	if err != nil {
		return
	}
	victim := rel
	tmp := filepath.Join("tmp", "delete-"+core.NewID())
	if err := root.Rename(rel, tmp); err == nil {
		victim = tmp
	} else if errors.Is(err, os.ErrNotExist) {
		return
	}
	s.d.Jobs.Go("files.remove", func(context.Context) error {
		if err := root.RemoveAll(victim); err != nil {
			s.log.Warn("removing deleted files failed", "path", victim, "err", err)
		}
		return nil
	})
}

// ownsTrashedSQL is true when the caller (bound twice) owns the trashed root f or anything in
// its subtree: those bytes count against their quota, so they must be able to see and purge
// them even when another library member trashed them (or replaced their file).
const ownsTrashedSQL = `(f.owner_id = ? OR EXISTS (SELECT 1 FROM files c WHERE c.space_id = f.space_id
	AND c.path >= f.path || '/' AND c.path < f.path || '0' AND c.owner_id = ?))`

// trashScope is the SQL condition (on files f joined with spaces sp) selecting the trashed
// roots in p's trash: their home, plus, in libraries they can still write, items they trashed
// or that contain files they own.
func trashScope(p *core.Principal) (string, []any) {
	u := p.UserID
	if p.IsAdmin() {
		return `f.trash_origin IS NOT NULL AND ((sp.kind = 'home' AND sp.owner_id = ?) OR f.trashed_by = ?
			OR (sp.kind = 'library' AND ` + ownsTrashedSQL + `))`, []any{u, u, u, u}
	}
	return `f.trash_origin IS NOT NULL AND ((sp.kind = 'home' AND sp.owner_id = ?) OR (sp.kind = 'library'
		AND (f.trashed_by = ? OR ` + ownsTrashedSQL + `)
		AND EXISTS (SELECT 1 FROM space_members m WHERE m.space_id = sp.id AND m.user_id IN (?, '*') AND m.access = 'write')))`,
		[]any{u, u, u, u, u}
}

// EmptyTrash permanently deletes everything in p's trash.
func (s *Service) EmptyTrash(ctx context.Context, p *core.Principal) (purgeResult, error) {
	cond, args := trashScope(p)
	var total purgeResult
	for {
		items, err := s.trashRoots(ctx, `SELECT f.id, f.space_id FROM files f JOIN spaces sp ON sp.id = f.space_id WHERE `+cond+` LIMIT 200`, args...)
		if err != nil {
			return total, err
		}
		if len(items) == 0 {
			return total, nil
		}
		n, err := s.purgeMany(ctx, items)
		total.Deleted += n.Deleted
		total.Freed += n.Freed
		if err != nil || n.Deleted == 0 {
			return total, err
		}
	}
}

type trashRef struct{ ID, SpaceID string }

func (s *Service) trashRoots(ctx context.Context, q string, args ...any) ([]trashRef, error) {
	rs, err := s.d.DB.SQL.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []trashRef
	for rs.Next() {
		var t trashRef
		if err := rs.Scan(&t.ID, &t.SpaceID); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rs.Err()
}

func (s *Service) purgeMany(ctx context.Context, items []trashRef) (purgeResult, error) {
	var total purgeResult
	var pd pending
	s.treeMu.Lock()
	defer func() { s.treeMu.Unlock(); pd.flush() }()
	spaces := map[string]*space{}
	for _, it := range items {
		sp := spaces[it.SpaceID]
		if sp == nil {
			var err error
			if sp, err = loadSpace(ctx, s.d.DB.SQL, it.SpaceID); err != nil {
				return total, err
			}
			if sp == nil {
				continue
			}
			spaces[it.SpaceID] = sp
		}
		n, err := s.purgeLocked(ctx, sp, it.ID, &pd)
		if err != nil {
			return total, err
		}
		total.Deleted += n.Deleted
		total.Freed += n.Freed
	}
	return total, nil
}

func validIDString(id string) bool {
	if len(id) < 8 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
