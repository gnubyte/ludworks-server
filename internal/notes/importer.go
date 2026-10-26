package notes

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"ludworks/internal/core"
)

// Import limits (API.md §8).
const (
	MaxImportBytes       = 50 << 20  // request body
	MaxImportEntries     = 10_000    // archive entries
	MaxImportTotalBytes  = 256 << 20 // decompressed Markdown across the archive
	maxImportErrors      = 1000
	importBatch          = 200
	maxImportPathSegment = MaxFolderDepth + 1
)

// ImportError reports one file that could not be imported.
type ImportError struct {
	File  string `json:"file"`
	Error string `json:"error"`
}

// ImportResult is the response of POST /notes/import.
type ImportResult struct {
	Imported       int           `json:"imported"`
	FoldersCreated int           `json:"folders_created"`
	Errors         []ImportError `json:"errors"`
}

func (r *ImportResult) fail(file, msg string) {
	if len(r.Errors) < maxImportErrors {
		r.Errors = append(r.Errors, ImportError{File: file, Error: msg})
	}
}

// importItem is a parsed note waiting to be inserted.
type importItem struct {
	dir  []string // folder path segments below the target folder
	file string
	in   NoteInput
}

// isMarkdownName reports whether a file name has an importable extension.
func isMarkdownName(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".md", ".markdown", ".txt":
		return true
	}
	return false
}

// decodeText converts file bytes to a UTF-8 string: BOMs are honoured (UTF-8, UTF-16
// LE/BE), invalid UTF-8 is decoded as Windows-1252/Latin-1, and line endings become \n.
// Binary content (NUL bytes) is rejected.
func decodeText(b []byte) (string, error) {
	var s string
	switch {
	case bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}):
		s = string(b[3:])
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}), bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		le := b[0] == 0xFF
		b = b[2:]
		u := make([]uint16, 0, len(b)/2)
		for i := 0; i+1 < len(b); i += 2 {
			if le {
				u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
			} else {
				u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
			}
		}
		s = string(utf16.Decode(u))
	case utf8.Valid(b):
		s = string(b)
	default:
		rs := make([]rune, len(b))
		for i, c := range b {
			rs[i] = rune(c)
		}
		s = string(rs)
	}
	if strings.ContainsRune(s, 0) {
		return "", errors.New("not a text file")
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return s, nil
}

// fileStem is the file name without its extension.
func fileStem(name string) string {
	return strings.TrimSpace(strings.TrimSuffix(name, path.Ext(name)))
}

// parseImportFile turns file contents into a NoteInput, honouring front matter. Without a
// front-matter title, the file name becomes the explicit title unless it matches the title
// derived from the first line (so exported notes round-trip without gaining a title).
func parseImportFile(name string, b []byte) (NoteInput, error) {
	text, err := decodeText(b)
	if err != nil {
		return NoteInput{}, err
	}
	fm, body, _ := splitFrontMatter(text)
	if len(body) > MaxBodyBytes {
		return NoteInput{}, errors.New("note exceeds 5 MiB")
	}
	in := NoteInput{Body: body}
	if fm.Title != nil {
		t, problem := normalizeTitle(*fm.Title)
		if problem != "" {
			t = truncateRunes(cleanText(*fm.Title), MaxTitleRunes, false)
		}
		if t != "" {
			in.Title = &t
		}
	}
	if in.Title == nil && !startsWithHeading(body) {
		stem := fileStem(name)
		derivedTitle := analyzeBody(body).FirstLine
		if stem != "" && !sameTitle(stem, derivedTitle) {
			t := truncateRunes(cleanText(stem), MaxTitleRunes, false)
			in.Title = &t
		}
	}
	for _, raw := range fm.Tags {
		if t, problem := normalizeTag(raw); problem == "" {
			in.Tags = append(in.Tags, t)
		}
	}
	if len(in.Tags) > MaxTags {
		in.Tags = in.Tags[:MaxTags]
	}
	if fm.Created != nil {
		in.Created = *fm.Created
	}
	if fm.Updated != nil {
		in.Updated = *fm.Updated
	}
	if fm.Pinned != nil {
		in.Pinned = *fm.Pinned
	}
	return in, nil
}

// startsWithHeading reports whether the first non-blank line is an ATX heading, which is
// taken as the author's title.
func startsWithHeading(body string) bool {
	for _, line := range strings.Split(prefixLines(body, 64), "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		return reHeading.MatchString(t)
	}
	return false
}

func prefixLines(s string, n int) string {
	i := 0
	for p := 0; p < len(s) && i < n; p++ {
		if s[p] == '\n' {
			i++
			if i == n {
				return s[:p]
			}
		}
	}
	return s
}

// sameTitle reports whether a file stem was generated from title by the exporter
// (sanitized, possibly with a " (n)" de-duplication suffix or "Untitled").
func sameTitle(stem, title string) bool {
	want := exportName(title)
	if strings.EqualFold(stem, want) {
		return true
	}
	if i := strings.LastIndex(stem, " ("); i > 0 && strings.HasSuffix(stem, ")") {
		if _, err := fmt.Sscanf(stem[i+2:len(stem)-1], "%d", new(int)); err == nil && strings.EqualFold(stem[:i], want) {
			return true
		}
	}
	return false
}

// safeArchivePath validates a ZIP entry name and returns its segments. It rejects absolute
// paths, drive letters, ".." segments and over-deep paths (zip-slip defence; nothing is
// ever written to disk, but folder names must still be sane).
func safeArchivePath(name string) ([]string, error) {
	name = strings.ToValidUTF8(name, "�")
	n := strings.ReplaceAll(name, `\`, "/")
	if strings.HasPrefix(n, "/") || (len(n) >= 2 && n[1] == ':') {
		return nil, errors.New("unsafe path (absolute) rejected")
	}
	var segs []string
	for _, s := range strings.Split(n, "/") {
		switch s {
		case "", ".":
			continue
		case "..":
			return nil, errors.New("unsafe path (..) rejected")
		}
		if strings.ContainsFunc(s, unicode.IsControl) {
			return nil, errors.New("unsafe path (control characters) rejected")
		}
		segs = append(segs, s)
	}
	if len(segs) > maxImportPathSegment {
		return nil, errors.New("path is nested too deeply")
	}
	return segs, nil
}

func skippedEntry(segs []string) bool {
	for _, s := range segs {
		if strings.HasPrefix(s, ".") || s == "__MACOSX" || strings.EqualFold(s, "Thumbs.db") || strings.EqualFold(s, "desktop.ini") {
			return true
		}
	}
	return false
}

// Import reads a ZIP archive or a single Markdown file from src (already size-limited by
// the caller) into folderID (nil = top level).
func (s *Service) Import(ctx context.Context, uid string, folderID *string, filename string, src *os.File, size int64) (*ImportResult, error) {
	if folderID != nil {
		if err := folderOwned(ctx, s.d.DB.SQL, uid, *folderID); err != nil {
			return nil, err
		}
	}
	res := &ImportResult{Errors: []ImportError{}}
	head := make([]byte, 4)
	hn, _ := src.ReadAt(head, 0)
	isZip := hn == 4 && bytes.Equal(head, []byte("PK\x03\x04")) || (hn == 4 && bytes.Equal(head, []byte("PK\x05\x06")))
	if !isZip && strings.EqualFold(path.Ext(filename), ".zip") {
		return nil, core.BadRequest("the file is not a valid ZIP archive")
	}
	var items []importItem
	var dirs [][]string
	if isZip {
		zr, err := zip.NewReader(src, size)
		if err != nil {
			return nil, core.BadRequest("the file is not a valid ZIP archive")
		}
		if len(zr.File) > MaxImportEntries {
			return nil, core.NewError(http.StatusRequestEntityTooLarge, "payload_too_large", "The archive has more than 10000 entries")
		}
		var total int64
		for _, f := range zr.File {
			segs, err := safeArchivePath(f.Name)
			if err != nil {
				res.fail(f.Name, err.Error())
				continue
			}
			if len(segs) == 0 || skippedEntry(segs) {
				continue
			}
			if f.FileInfo().IsDir() || strings.HasSuffix(f.Name, "/") {
				dirs = append(dirs, segs)
				continue
			}
			display := strings.Join(segs, "/")
			if !isMarkdownName(segs[len(segs)-1]) {
				res.fail(display, "unsupported file type (only .md, .markdown and .txt are imported)")
				continue
			}
			if f.UncompressedSize64 > MaxBodyBytes+maxFrontMatterBytes {
				res.fail(display, "note exceeds 5 MiB")
				continue
			}
			if total+int64(f.UncompressedSize64) > MaxImportTotalBytes {
				res.fail(display, "import size limit reached (256 MiB of Markdown per import)")
				continue
			}
			b, err := readZipEntry(f, MaxBodyBytes+maxFrontMatterBytes)
			if err != nil {
				res.fail(display, err.Error())
				continue
			}
			total += int64(len(b))
			in, err := parseImportFile(segs[len(segs)-1], b)
			if err != nil {
				res.fail(display, err.Error())
				continue
			}
			items = append(items, importItem{dir: segs[:len(segs)-1], file: display, in: in})
		}
	} else {
		name, problem := sanitizeFilename(filename)
		if problem != "" {
			name = "Imported note.md"
		}
		if !isMarkdownName(name) && filename != "" {
			return nil, core.NewError(http.StatusUnsupportedMediaType, "unsupported_media_type", "Only .md, .markdown, .txt or .zip files can be imported")
		}
		if size > MaxBodyBytes+maxFrontMatterBytes {
			return nil, bodyTooLarge()
		}
		b := make([]byte, size)
		if _, err := src.ReadAt(b, 0); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		in, err := parseImportFile(name, b)
		if err != nil {
			return nil, core.Validation(map[string]string{"file": err.Error()})
		}
		items = append(items, importItem{file: name, in: in})
	}
	if err := s.storeImport(ctx, uid, folderID, dirs, items, res); err != nil {
		return nil, err
	}
	return res, nil
}

func readZipEntry(f *zip.File, limit int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, errors.New("unreadable archive entry")
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, errors.New("corrupt archive entry")
	}
	if int64(len(b)) > limit {
		return nil, errors.New("note exceeds 5 MiB")
	}
	return b, nil
}

// storeImport creates folders on demand and inserts notes in batches (one transaction per
// batch, so a large import never blocks other writers for long).
func (s *Service) storeImport(ctx context.Context, uid string, base *string, dirs [][]string, items []importItem, res *ImportResult) error {
	folders := map[string]*string{"": base} // path key → folder id
	ensure := func(ctx context.Context, tx *sql.Tx, segs []string, ts time.Time) (*string, error) {
		key := ""
		parent := base
		for _, seg := range segs {
			name, problem := normalizeFolderName(seg)
			if problem != "" {
				name = "Folder"
			}
			key += "/" + sortKey(name)
			if id, ok := folders[key]; ok {
				parent = id
				continue
			}
			var existing string
			err := tx.QueryRowContext(ctx, `SELECT id FROM notes_folders WHERE user_id = ? AND COALESCE(parent_id, '') = ? AND name_key = ?`,
				uid, deref(parent), sortKey(name)).Scan(&existing)
			switch {
			case err == nil:
				id := existing
				folders[key] = &id
				parent = &id
			case errors.Is(err, sql.ErrNoRows):
				f, err := createFolderTx(ctx, tx, uid, name, parent, ts)
				if err != nil {
					return nil, err
				}
				id := f.ID
				folders[key] = &id
				parent = &id
				res.FoldersCreated++
			default:
				return nil, err
			}
		}
		return parent, nil
	}
	if len(dirs) > 0 {
		snapshot := cloneFolders(folders)
		created, nerr := res.FoldersCreated, len(res.Errors)
		err := s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
			folders = cloneFolders(snapshot)
			res.FoldersCreated, res.Errors = created, res.Errors[:nerr]
			ts, err := tick(ctx, tx)
			if err != nil {
				return err
			}
			for _, d := range dirs {
				if _, err := ensure(ctx, tx, d, ts); err != nil {
					var ce *core.Error
					if errors.As(err, &ce) {
						res.fail(strings.Join(d, "/")+"/", ce.Message)
						continue
					}
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	for start := 0; start < len(items); start += importBatch {
		batch := items[start:min(start+importBatch, len(items))]
		snapshot := cloneFolders(folders)
		created, imported, nerr := res.FoldersCreated, res.Imported, len(res.Errors)
		err := s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
			// The transaction may be retried: reset the bookkeeping it mutates.
			folders = cloneFolders(snapshot)
			res.FoldersCreated, res.Imported, res.Errors = created, imported, res.Errors[:nerr]
			ts, err := tick(ctx, tx)
			if err != nil {
				return err
			}
			for _, it := range batch {
				fid, err := ensure(ctx, tx, it.dir, ts)
				if err != nil {
					var ce *core.Error
					if errors.As(err, &ce) {
						res.fail(it.file, ce.Message)
						continue
					}
					return err
				}
				title, tags, err := validateInput(&it.in)
				if err != nil {
					res.fail(it.file, errorText(err))
					continue
				}
				created, updated := ts, ts
				if !it.in.Created.IsZero() {
					created = it.in.Created
					updated = created
				}
				if !it.in.Updated.IsZero() {
					updated = it.in.Updated
				}
				if _, err := insertNote(ctx, tx, &noteRow{
					ID: core.NewID(), UserID: uid, FolderID: fid, ExplicitTitle: title, Body: it.in.Body, Tags: tags,
					Pinned: it.in.Pinned, Version: 1, CreatedAt: created, UpdatedAt: updated, ChangedAt: ts,
				}); err != nil {
					return err
				}
				res.Imported++
			}
			return nil
		})
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return nil
}

func cloneFolders(m map[string]*string) map[string]*string {
	out := make(map[string]*string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func errorText(err error) string {
	var ce *core.Error
	if errors.As(err, &ce) {
		if f, ok := ce.Details.(map[string]any); ok {
			if fields, ok := f["fields"].(map[string]string); ok {
				var parts []string
				for k, v := range fields {
					parts = append(parts, k+" "+v)
				}
				return strings.Join(parts, "; ")
			}
		}
		return ce.Message
	}
	return err.Error()
}
