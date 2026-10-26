package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
)

// freeHeadroom is kept free on the data volume beyond any upload (API.md §5.1). The database,
// its WAL and the session/audit tables live on the same volume, so writes must never eat into
// it: a full volume would make every SQLite write (logins included) fail.
const freeHeadroom int64 = 1 << 30

// errQuota and errNoSpace are the two 507 errors.
func errQuota(need, avail int64) error {
	return core.NewError(507, "quota_exceeded", "This would exceed your storage quota").
		WithDetails(map[string]any{"needed_bytes": need, "available_bytes": max(avail, 0)})
}

var errNoSpace = core.NewError(507, "insufficient_storage", "Not enough free disk space on the server")

// checkFree verifies that the data volume can take n more bytes from actor and still keep
// freeHeadroom free, counting what actor has already promised but not yet written: their
// incomplete tus uploads and their running simple uploads and copies. Only the actor's own
// promises count, so idle reservations of one user never block another; the writes
// themselves are guarded separately (writeGuard), which is what actually protects the volume.
func (s *Service) checkFree(ctx context.Context, actor string, n int64) error {
	free, err := s.freeSpace(s.TempDir())
	if err != nil {
		s.log.Warn("free space check failed", "err", err)
		return nil // do not block uploads when the platform cannot report free space
	}
	avail := free - freeHeadroom
	if n > 0 && actor != "" {
		var tus int64
		if err := s.d.DB.SQL.QueryRowContext(ctx, `SELECT COALESCE(SUM(length - received), 0) FROM uploads
			WHERE user_id = ? AND state = 'active' AND expires_at > ?`, actor, db.FormatTime(s.now())).Scan(&tus); err != nil {
			return err
		}
		avail -= tus + s.pendingBy(actor)
	}
	if n < 0 || avail < n {
		return errNoSpace
	}
	return nil
}

// guardFree fails with 507 when writing n more bytes would leave less than freeHeadroom free.
func (s *Service) guardFree(n int64) error {
	free, err := s.freeSpace(s.TempDir())
	if err != nil {
		return nil // platform cannot report free space; ENOSPC still maps to 507
	}
	if free-freeHeadroom < n {
		return errNoSpace
	}
	return nil
}

// guardEvery is how many bytes a guarded operation may write between two free-space probes.
// N concurrent writers can overshoot a probe by at most N × guardEvery, far below the headroom.
const guardEvery int64 = 8 << 20

// writeGuard keeps one operation's writes to the data volume (upload chunks, simple uploads,
// staged copies, temp files) out of the free-space headroom: before each guardEvery bytes it
// checks that the next stretch still fits and otherwise fails with 507 insufficient_storage
// instead of writing. It also records progress on the operation's reservation, if any.
type writeGuard struct {
	s         *Service
	res       *reservation // optional
	remaining int64        // bytes the operation still expects to write (≤ 0: unknown)
	since     int64        // bytes written since the last probe; < 0 forces a probe
}

func (s *Service) newWriteGuard(res *reservation, expect int64) *writeGuard {
	return &writeGuard{s: s, res: res, remaining: expect, since: -1}
}

// wrap applies the guard to w's writes (nil guard: w unchanged).
func (g *writeGuard) wrap(w io.Writer) io.Writer {
	if g == nil {
		return w
	}
	return guardedWriter{w: w, g: g}
}

type guardedWriter struct {
	w io.Writer
	g *writeGuard
}

func (gw guardedWriter) Write(p []byte) (int, error) {
	g := gw.g
	if n := int64(len(p)); g.since < 0 || g.since+n > guardEvery {
		need := guardEvery
		if g.remaining > 0 && g.remaining < need {
			need = g.remaining
		}
		if err := g.s.guardFree(max(need, n)); err != nil {
			return 0, err
		}
		g.since = 0
	}
	n, err := gw.w.Write(p)
	g.since += int64(n)
	g.remaining -= int64(n)
	if g.res != nil {
		g.res.written.Add(int64(n))
	}
	return n, err
}

// diskAlias reports whether rel, which the caller found on disk (Lstat succeeded) although
// the index has no entry at that exact path, exists only under a different stored name. That
// happens on case-insensitive file systems (Windows, macOS by default; also normalization-
// insensitive ones and NTFS 8.3 short names): "CASETEST" resolves to the existing
// "casetest". The index is case-sensitive, so adopting such a hit would make two index paths
// share one disk object and put files where the listing does not show them. It returns the
// sibling's stored name when it can be found.
func diskAlias(root *os.Root, rel string) (string, bool, error) {
	dir, base := filepath.Split(rel)
	if dir == "" {
		dir = "."
	}
	d, err := root.Open(dir)
	if err != nil {
		return "", false, err
	}
	names, err := d.Readdirnames(-1)
	d.Close()
	if err != nil {
		return "", false, err
	}
	for _, n := range names {
		if n == base {
			return "", false, nil // stored under exactly this name
		}
	}
	st, err := root.Lstat(rel)
	if err != nil {
		return "", false, err
	}
	// Likely candidates first (case variants), then anything else (normalization, 8.3).
	for _, pass := range []func(string) bool{func(n string) bool { return strings.EqualFold(n, base) }, func(string) bool { return true }} {
		for _, n := range names {
			if !pass(n) {
				continue
			}
			if o, err := root.Lstat(filepath.Join(dir, n)); err == nil && os.SameFile(st, o) {
				return n, true, nil
			}
		}
	}
	return "", true, nil
}

// errNameAlias is the 409 for a name that the server's file system cannot tell apart from an
// existing sibling.
func errNameAlias(name, existing string) error {
	msg := fmt.Sprintf("%q cannot be used here: this server's disk does not distinguish it from an existing item", name)
	if existing != "" {
		msg = fmt.Sprintf("%q cannot be used here: this server's disk does not distinguish it from the existing %q (names are not case-sensitive)", name, existing)
	}
	return core.NewError(409, "conflict", msg).WithDetails(map[string]any{"existing_name": existing})
}

// checkDiskAlias returns errNameAlias when rel (relative to root) exists on disk under a
// different stored name, and nil when it is free or stored under exactly this name.
func checkDiskAlias(root *os.Root, rel, name string) error {
	if _, err := root.Lstat(rel); err != nil {
		return nil
	}
	other, alias, err := diskAlias(root, rel)
	if err != nil {
		return err
	}
	if alias {
		return errNameAlias(name, other)
	}
	return nil
}

// checkUnindexedAlias is checkDiskAlias for the entry path p of space sp (p is not indexed).
func (s *Service) checkUnindexedAlias(sp *space, p, name string) error {
	sr, err := s.spaceRoot(sp)
	if err != nil {
		return err
	}
	defer sr.Close()
	return checkDiskAlias(sr, osRel(p), name)
}

/* ---------------------------------------------------------- confined areas */

// area is a directory tree under the data root that a disk operation is confined to: a
// space's root, a space's trash (storage/.trash/<space id>) or tmp/. Paths inside an area are
// resolved through the area's own os.Root, so a symlink planted inside a space (for example
// over an SMB/NFS share of a home folder, or by a restored backup) can never make a write,
// move, trash or restore reach another space or the server's own files such as ludworks.db or
// secret.key (ARCHITECTURE.md §2.10). The data root is used only to open areas.
type area struct {
	root *os.Root // opened on base
	data *os.Root // the data root (platforms without renameat; see renameBetween)
	base string   // data-root-relative path of root
}

func (a *area) Close() {
	if a != nil && a.root != nil {
		_ = a.root.Close()
	}
}

// errLinkOnDisk is returned when a folder on a path turned into a link (or another special
// file) on disk behind the server's back.
var errLinkOnDisk = core.Conflict("A folder on this path is a link or special file on disk (rescan the space)")

// plainDirs verifies that every component of rel (relative to root) is a plain directory —
// not a symlink, junction or other reparse point. last includes rel itself.
func plainDirs(root *os.Root, rel string, last bool) error {
	rel = filepath.Clean(rel)
	if rel == "." {
		return nil
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if !last {
		parts = parts[:len(parts)-1]
	}
	for i := range parts {
		st, err := root.Lstat(filepath.Join(parts[:i+1]...))
		if err != nil {
			return err
		}
		if st.Mode().Type() != fs.ModeDir {
			return errLinkOnDisk
		}
	}
	return nil
}

// openArea opens (creating if needed) the area at base, a data-root-relative path made only of
// server-managed directories, each of which must be a plain directory on disk.
func (s *Service) openArea(base string) (*area, error) {
	data, err := s.dataRoot()
	if err != nil {
		return nil, err
	}
	err = plainDirs(data, base, true)
	if errors.Is(err, os.ErrNotExist) {
		if err = data.MkdirAll(base, 0o700); err == nil {
			err = plainDirs(data, base, true)
		}
	}
	if err != nil {
		return nil, err
	}
	r, err := data.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	return &area{root: r, data: data, base: base}, nil
}

// spaceArea opens the live tree of sp (storage/<root_dir>); paths in it are osRel(entry path).
func (s *Service) spaceArea(sp *space) (*area, error) { return s.openArea(diskRel(sp, "")) }

// trashArea opens sp's trash (storage/.trash/<space id>); paths in it are trashRel(entry path).
func (s *Service) trashArea(sp *space) (*area, error) {
	return s.openArea(filepath.Join("storage", ".trash", sp.ID))
}

// tmpArea opens tmp/ (staging and temp files).
func (s *Service) tmpArea() (*area, error) { return s.openArea("tmp") }

// trashRel converts a trashed entry path ("/<id>/...") to an OS path relative to the trash area.
func trashRel(p string) string { return filepath.FromSlash(strings.TrimPrefix(p, "/")) }

// splitRel splits an area-relative path into its parent directory and final name.
func splitRel(rel string) (dir, name string, err error) {
	rel = filepath.Clean(rel)
	dir, name = filepath.Dir(rel), filepath.Base(rel)
	if !filepath.IsLocal(rel) || name == "." || name == ".." || name == string(filepath.Separator) {
		return "", "", fmt.Errorf("files: invalid path %q", rel)
	}
	return dir, name, nil
}

// renameChecked is renameBetween for platforms without renameat(2) (Windows): every folder
// above src and dst inside its area must be a plain directory — no symlink, junction or other
// reparse point — before the rename runs through the data root, so the data-root path names
// the same object as the confined one.
func renameChecked(a *area, src string, b *area, dst string) error {
	sdir, _, err := splitRel(src)
	if err != nil {
		return err
	}
	ddir, _, err := splitRel(dst)
	if err != nil {
		return err
	}
	if err := plainDirs(a.root, sdir, true); err != nil {
		return err
	}
	if err := plainDirs(b.root, ddir, true); err != nil {
		return err
	}
	return a.data.Rename(filepath.Join(a.base, src), filepath.Join(b.base, dst))
}

// moveBetween moves src (relative to area a) to dst (relative to area b; dst must not exist).
// Each path is resolved inside its own area only. Across volumes the tree is copied and the
// source removed.
func moveBetween(ctx context.Context, a *area, src string, b *area, dst string) error {
	var err error
	if a == b {
		err = a.root.Rename(src, dst)
	} else {
		err = renameBetween(a, src, b, dst)
	}
	if err == nil || !isCrossDevice(err) {
		return err
	}
	if err := copyTreeBetween(ctx, a.root, src, b.root, dst); err != nil {
		_ = b.root.RemoveAll(dst)
		return err
	}
	return a.root.RemoveAll(src)
}

/* ------------------------------------------------------------------ copies */

// copyBuf is the buffer size for file copies (large enough for spinning disks).
const copyBuf = 1 << 20

// ctxReader aborts a copy when ctx is cancelled.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// copyFileBetween copies the regular file src (relative to srcRoot) to dst (relative to
// dstRoot, which must not exist), syncs it and sets its modification time. A non-nil guard
// keeps the writes out of the free-space headroom.
func copyFileBetween(ctx context.Context, srcRoot *os.Root, src string, dstRoot *os.Root, dst string, modTime time.Time, g *writeGuard) (int64, error) {
	in, err := srcRoot.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return 0, err
	}
	if !st.Mode().IsRegular() {
		return 0, fmt.Errorf("%s: not a regular file", src)
	}
	out, err := dstRoot.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.CopyBuffer(g.wrap(out), ctxReader{ctx, in}, make([]byte, copyBuf))
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = dstRoot.Remove(dst)
		return 0, err
	}
	if modTime.IsZero() {
		modTime = st.ModTime()
	}
	_ = dstRoot.Chtimes(dst, modTime, modTime)
	return n, nil
}

// isCrossDevice reports whether a rename failed because source and target are on
// different volumes.
func isCrossDevice(err error) bool {
	var le *os.LinkError
	if errors.As(err, &le) {
		err = le.Err
	}
	return errors.Is(err, syscall.EXDEV) || isWindowsNotSameDevice(err)
}

// moveWithin renames src to dst inside root; across volumes it copies the tree and removes
// the source. dst must not exist. Only for server-managed paths (user deletion); entry paths
// go through moveBetween and their areas.
func moveWithin(ctx context.Context, root *os.Root, src, dst string) error {
	err := root.Rename(src, dst)
	if err == nil || !isCrossDevice(err) {
		return err
	}
	if err := copyTreeBetween(ctx, root, src, root, dst); err != nil {
		_ = root.RemoveAll(dst)
		return err
	}
	return root.RemoveAll(src)
}

// copyTreeBetween copies a file or directory tree src (in srcRoot) → dst (in dstRoot);
// symlinks and special files are skipped.
func copyTreeBetween(ctx context.Context, srcRoot *os.Root, src string, dstRoot *os.Root, dst string) error {
	st, err := srcRoot.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case st.Mode().IsRegular():
		_, err := copyFileBetween(ctx, srcRoot, src, dstRoot, dst, st.ModTime(), nil)
		return err
	case !st.IsDir():
		return nil
	}
	if err := dstRoot.Mkdir(dst, 0o700); err != nil {
		return err
	}
	d, err := srcRoot.Open(src)
	if err != nil {
		return err
	}
	ents, err := d.ReadDir(-1)
	d.Close()
	if err != nil {
		return err
	}
	for _, e := range ents {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := copyTreeBetween(ctx, srcRoot, src+string(os.PathSeparator)+e.Name(), dstRoot, dst+string(os.PathSeparator)+e.Name()); err != nil {
			return err
		}
	}
	_ = dstRoot.Chtimes(dst, st.ModTime(), st.ModTime())
	return nil
}
