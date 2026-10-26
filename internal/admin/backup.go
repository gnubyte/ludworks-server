package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"time"

	"github.com/ncruces/go-sqlite3"

	"ludworks/internal/core"
	"ludworks/internal/httpx"
)

// backupName matches files this module writes; pruning never touches anything else.
var backupName = regexp.MustCompile(`^ludworks-\d{8}T\d{6}Z(-\d{1,3})?\.db$`)

var errInsufficientStorage = core.NewError(http.StatusInsufficientStorage, "insufficient_storage", "Not enough disk space for the backup")

// dbBackup writes a consistent online copy with VACUUM INTO and keeps the newest maxBackups.
func (m *Module) dbBackup(w http.ResponseWriter, r *http.Request) error {
	// The copy should finish even if the admin closes the page.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Minute)
	defer cancel()
	path, size, err := m.backup(ctx, time.Now().UTC())
	if err != nil {
		return err
	}
	m.audit(ctx, "admin.db_backup", "backup", filepath.Base(path), map[string]any{"size": size})
	return httpx.WriteJSON(w, http.StatusOK, map[string]any{"path": path, "size": size})
}

func (m *Module) backup(ctx context.Context, now time.Time) (string, int64, error) {
	m.backupMu.Lock()
	defer m.backupMu.Unlock()
	dir := m.d.Config.BackupsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", 0, err
	}
	base := "ludworks-" + now.Format("20060102T150405Z")
	path := filepath.Join(dir, base+".db")
	for i := 2; fileExists(path); i++ {
		if i > 999 {
			return "", 0, errors.New("admin: too many backups in the same second")
		}
		path = filepath.Join(dir, fmt.Sprintf("%s-%d.db", base, i))
	}
	if _, err := m.d.DB.SQL.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		os.Remove(path) // never leave a partial copy behind
		if errors.Is(err, sqlite3.FULL) || errors.Is(err, sqlite3.IOERR_WRITE) {
			return "", 0, errInsufficientStorage
		}
		return "", 0, fmt.Errorf("admin: backup: %w", err)
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(path, 0o600)
	}
	st, err := os.Stat(path)
	if err != nil {
		return "", 0, err
	}
	if err := pruneBackups(dir, maxBackups); err != nil {
		m.log.Warn("pruning old backups failed", "err", err)
	}
	return path, st.Size(), nil
}

func fileExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// pruneBackups deletes all but the newest keep backups (names sort chronologically).
func pruneBackups(dir string, keep int) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range ents {
		if e.Type().IsRegular() && backupName.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Slice(names, func(i, j int) bool { return backupLess(names[i], names[j]) })
	var errs []error
	for len(names) > keep {
		if err := os.Remove(filepath.Join(dir, names[0])); err != nil {
			errs = append(errs, err)
		}
		names = names[1:]
	}
	return errors.Join(errs...)
}

// backupLess orders by timestamp, then by the numeric collision suffix.
func backupLess(a, b string) bool {
	ta, sa := splitBackup(a)
	tb, sb := splitBackup(b)
	if ta != tb {
		return ta < tb
	}
	return sa < sb
}

func splitBackup(name string) (string, int) {
	ts := name[len("ludworks-") : len("ludworks-")+16]
	n := 1
	if rest := name[len("ludworks-")+16:]; len(rest) > len(".db") {
		fmt.Sscanf(rest, "-%d.db", &n)
	}
	return ts, n
}
