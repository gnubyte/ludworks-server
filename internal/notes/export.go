package notes

import (
	"archive/zip"
	"context"
	"database/sql"
	"errors"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"ludworks/internal/core"
	"ludworks/internal/db"
)

var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// exportName turns a title or folder name into a portable file/directory name (no path
// separators, no characters Windows forbids, no reserved device names, ≤ 120 runes).
func exportName(title string) string {
	s := strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r):
			return -1
		case strings.ContainsRune(`/\:*?"<>|`, r):
			return '_'
		}
		return r
	}, title)
	s = collapseSpace(s)
	if utf8.RuneCountInString(s) > 120 {
		s = string([]rune(s)[:120])
	}
	s = strings.TrimRight(s, ". ")
	s = strings.TrimLeft(s, ". ")
	if s == "" {
		return "Untitled"
	}
	if windowsReserved[strings.ToUpper(strings.SplitN(s, ".", 2)[0])] {
		s = "_" + s
	}
	return s
}

// uniqueName returns name (+ext) or "name (n)" (+ext) so that it is unique in used
// (case-insensitively, like Windows and macOS file systems).
func uniqueName(used map[string]bool, name, ext string) string {
	cand := name + ext
	for i := 2; used[strings.ToLower(cand)]; i++ {
		cand = name + " (" + itoa(i) + ")" + ext
	}
	used[strings.ToLower(cand)] = true
	return cand
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}

// exportEntry is a note to export with its archive path.
type exportEntry struct {
	id   string
	path string
	mod  time.Time
}

// ExportArchiveName is the download file name for an export.
func (s *Service) ExportArchiveName(ctx context.Context, uid string, folderID *string) (string, error) {
	date := time.Now().UTC().Format("2006-01-02")
	if folderID == nil {
		return "Notes " + date + ".zip", nil
	}
	f, err := getFolderTx(ctx, s.d.DB.SQL, uid, *folderID)
	if err != nil {
		return "", err
	}
	return exportName(f.Name) + " " + date + ".zip", nil
}

// planExport lays out the archive: folder directories and note file paths. With folderID
// the archive root contains that folder (as a directory) and its subtree.
func (s *Service) planExport(ctx context.Context, uid string, folderID *string) (dirs []string, entries []exportEntry, err error) {
	type frow struct {
		id, name string
		parent   *string
	}
	var folders []frow
	children := map[string][]int{}
	rows, err := s.d.DB.SQL.QueryContext(ctx, `SELECT id, name, parent_id FROM notes_folders WHERE user_id = ? ORDER BY position, name_key, id`, uid)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var f frow
		var p sql.NullString
		if err := rows.Scan(&f.id, &f.name, &p); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if p.Valid {
			f.parent = &p.String
		}
		folders = append(folders, f)
		children[deref(f.parent)] = append(children[deref(f.parent)], len(folders)-1)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	dirPath := map[string]string{} // folder id → archive dir ("" = root)
	usedIn := map[string]map[string]bool{}
	used := func(dir string) map[string]bool {
		if usedIn[dir] == nil {
			usedIn[dir] = map[string]bool{}
		}
		return usedIn[dir]
	}
	var walk func(parentKey, parentDir string, depth int)
	walk = func(parentKey, parentDir string, depth int) {
		if depth > MaxFolderDepth+1 {
			return
		}
		for _, i := range children[parentKey] {
			f := folders[i]
			d := parentDir + uniqueName(used(parentDir), exportName(f.name), "") + "/"
			dirPath[f.id] = d
			dirs = append(dirs, d)
			walk(f.id, d, depth+1)
		}
	}
	if folderID != nil {
		var root *frow
		for i := range folders {
			if folders[i].id == *folderID {
				root = &folders[i]
				break
			}
		}
		if root == nil {
			return nil, nil, core.NotFound("folder not found")
		}
		d := uniqueName(used(""), exportName(root.name), "") + "/"
		dirPath[root.id] = d
		dirs = append(dirs, d)
		walk(root.id, d, 1)
	} else {
		dirPath[""] = ""
		walk("", "", 0)
	}

	nrows, err := s.d.DB.SQL.QueryContext(ctx, `SELECT id, folder_id, title, updated_at FROM notes
		WHERE user_id = ? AND trashed_at IS NULL ORDER BY pinned DESC, title_sort, created_at, id`, uid)
	if err != nil {
		return nil, nil, err
	}
	defer nrows.Close()
	for nrows.Next() {
		var id, title string
		var folder sql.NullString
		var updated db.Time
		if err := nrows.Scan(&id, &folder, &title, &updated); err != nil {
			return nil, nil, err
		}
		dir, ok := dirPath[folder.String]
		if !ok {
			continue // outside the exported subtree
		}
		entries = append(entries, exportEntry{id: id, path: dir + uniqueName(used(dir), exportName(title), ".md"), mod: updated.Time})
	}
	return dirs, entries, nrows.Err()
}

// ExportZip streams the user's notes (or one folder subtree) as a ZIP of Markdown files.
func (s *Service) ExportZip(ctx context.Context, uid string, folderID *string, w io.Writer) error {
	dirs, entries, err := s.planExport(ctx, uid, folderID)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(w)
	for _, d := range dirs {
		if _, err := zw.CreateHeader(&zip.FileHeader{Name: d, Method: zip.Store, Modified: time.Now().UTC()}); err != nil {
			return err
		}
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, err := s.Get(ctx, uid, e.id)
		var ce *core.Error
		if errors.As(err, &ce) {
			continue // deleted meanwhile
		}
		if err != nil {
			return err
		}
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: e.path, Method: zip.Deflate, Modified: e.mod})
		if err != nil {
			return err
		}
		if _, err := fw.Write(markdownFile(n)); err != nil {
			return err
		}
	}
	return zw.Close()
}

// ExportNote renders one note as a Markdown file and returns it with its file name.
func (s *Service) ExportNote(ctx context.Context, uid, id string) ([]byte, string, error) {
	n, err := s.Get(ctx, uid, id)
	if err != nil {
		return nil, "", err
	}
	return markdownFile(n), exportName(n.Title) + ".md", nil
}
