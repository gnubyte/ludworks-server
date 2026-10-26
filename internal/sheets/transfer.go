package sheets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/files"
	"ludworks/internal/httpx"
	"ludworks/internal/sheets/csvio"
	"ludworks/internal/sheets/engine"
	"ludworks/internal/sheets/xls"
	"ludworks/internal/sheets/xlsx"
)

// Content types of the export formats.
var contentTypes = map[string]string{
	"xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	"xls":  "application/vnd.ms-excel",
	"csv":  "text/csv; charset=utf-8",
}

var (
	magicXLSX = []byte("PK\x03\x04")
	magicXLS  = []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}
)

// sniffFormat guesses a format from the first bytes: "xlsx" (ZIP), "xls" (OLE2 compound
// file), "csv" (text) or "" (binary data of an unknown kind).
func sniffFormat(head []byte) string {
	switch {
	case bytes.HasPrefix(head, magicXLSX):
		return "xlsx"
	case bytes.HasPrefix(head, magicXLS):
		return "xls"
	case bytes.HasPrefix(head, []byte{0xFF, 0xFE}), bytes.HasPrefix(head, []byte{0xFE, 0xFF}):
		return "csv" // UTF-16 text with BOM
	case bytes.IndexByte(head, 0) >= 0:
		return ""
	}
	return "csv"
}

func normFormat(f string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(f)) {
	case "":
		return "", true
	case "xlsx", "xlsm":
		return "xlsx", true
	case "xls":
		return "xls", true
	case "csv", "tsv", "txt":
		return "csv", true
	}
	return "", false
}

// formatFromName maps a file extension to an import format ("" = unknown).
func formatFromName(name string) string {
	f, ok := normFormat(strings.TrimPrefix(strings.ToLower(path.Ext(name)), "."))
	if !ok {
		return ""
	}
	return f
}

// acquireTransfer bounds concurrent imports/exports.
func (m *Module) acquireTransfer(ctx context.Context) (func(), error) {
	t := time.NewTimer(lockWaitBudget)
	defer t.Stop()
	select {
	case m.xfer <- struct{}{}:
		return func() { <-m.xfer }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.C:
		return nil, core.NewError(http.StatusServiceUnavailable, "busy", "Too many imports or exports are running; try again shortly")
	}
}

func (m *Module) tempFile() (*os.File, func(), error) {
	f, err := os.CreateTemp(m.d.Config.TmpDir(), "sheets-*.tmp")
	if err != nil {
		return nil, nil, err
	}
	return f, func() { f.Close(); os.Remove(f.Name()) }, nil
}

// parseWorkbook reads an uploaded spreadsheet. format "" means "detect"; magic bytes win
// over a declared binary format so a mislabelled file fails cleanly.
func parseWorkbook(f *os.File, size int64, format, sheetName string) (*engine.Workbook, error) {
	head := make([]byte, 512)
	n, err := f.ReadAt(head, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	head = head[:n]
	sniffed := sniffFormat(head)
	switch {
	case format == "":
		format = sniffed
	case (format == "xlsx" || format == "xls") && sniffed != format:
		return nil, core.NewError(http.StatusUnsupportedMediaType, "unsupported_media_type",
			"The file is not a valid "+strings.ToUpper(format)+" spreadsheet")
	case format == "csv" && sniffed != "csv":
		return nil, core.NewError(http.StatusUnsupportedMediaType, "unsupported_media_type",
			"The file is not CSV text")
	}
	var wb *engine.Workbook
	switch format {
	case "xlsx":
		wb, err = xlsx.Read(f, size)
	case "xls":
		wb, err = xls.Read(f, size)
	case "csv":
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		var sh *engine.Sheet
		sh, err = csvio.Read(f, csvio.ReadOptions{MaxBytes: maxImportBytes, SheetName: sheetName})
		if err == nil {
			wb = &engine.Workbook{Sheets: []*engine.Sheet{sh}}
		}
	default:
		return nil, core.NewError(http.StatusUnsupportedMediaType, "unsupported_media_type",
			"Unsupported file: upload an XLSX, XLS or CSV spreadsheet")
	}
	if err != nil {
		return nil, codecError(err, format)
	}
	if len(wb.Sheets) == 0 {
		return nil, fieldError("file", "The spreadsheet contains no sheets")
	}
	return wb, nil
}

// csvSheetName derives a sheet name from a file name (falls back to Sheet1).
func csvSheetName(name string) string {
	base := strings.TrimSuffix(name, path.Ext(name))
	var b strings.Builder
	for _, r := range base {
		if strings.ContainsRune(":\\/?*[]'", r) || unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	s := strings.TrimSpace(b.String())
	if rs := []rune(s); len(rs) > engine.MaxSheetNameLen {
		s = strings.TrimSpace(string(rs[:engine.MaxSheetNameLen]))
	}
	if engine.ValidateSheetName(s) != nil {
		return "Sheet1"
	}
	return s
}

// defaultName shortens a file name into a workbook name.
func defaultName(fileName string) string {
	n := strings.TrimSpace(strings.TrimSuffix(fileName, path.Ext(fileName)))
	n = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, n)
	if rs := []rune(n); len(rs) > maxNameLen {
		n = string(rs[:maxNameLen])
	}
	if strings.TrimSpace(n) == "" {
		return "Imported workbook"
	}
	return n
}

func (m *Module) importWorkbook(w http.ResponseWriter, r *http.Request) error {
	p := core.MustPrincipal(r.Context())
	q := r.URL.Query()
	format, ok := normFormat(q.Get("format"))
	if !ok {
		return core.BadRequest("format must be xlsx, xls or csv")
	}
	name := "Imported workbook"
	if raw := q.Get("name"); raw != "" {
		n, err := validName(raw)
		if err != nil {
			return err
		}
		name = n
	}
	if r.ContentLength > maxImportBytes {
		return core.NewError(http.StatusRequestEntityTooLarge, "payload_too_large", "Imports are limited to 50 MiB")
	}
	done, err := m.acquireTransfer(r.Context())
	if err != nil {
		return err
	}
	defer done()
	f, cleanup, err := m.tempFile()
	if err != nil {
		return err
	}
	defer cleanup()
	n, err := io.Copy(f, io.LimitReader(r.Body, maxImportBytes+1))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return core.ErrPayloadTooLarge
		}
		return err
	}
	if n > maxImportBytes {
		return core.NewError(http.StatusRequestEntityTooLarge, "payload_too_large", "Imports are limited to 50 MiB")
	}
	if n == 0 {
		return core.BadRequest("The request body is empty: send the file's bytes")
	}
	wb, err := parseWorkbook(f, n, format, csvSheetName(name))
	if err != nil {
		return err
	}
	meta, err := m.insertWorkbook(r.Context(), p, name, wb, true)
	if err != nil {
		return err
	}
	return writeVersioned(w, r, http.StatusCreated, meta.Version, mustJSON(summaryOf(*meta, wb)))
}

func (m *Module) importFromFiles(w http.ResponseWriter, r *http.Request) error {
	p := core.MustPrincipal(r.Context())
	var in struct {
		FileID string `json:"file_id"`
		Name   string `json:"name"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	if in.FileID == "" {
		return fieldError("file_id", "file_id is required")
	}
	if !httpx.ValidID(in.FileID) {
		return core.NotFound("File not found")
	}
	name := ""
	if strings.TrimSpace(in.Name) != "" {
		n, err := validName(in.Name)
		if err != nil {
			return err
		}
		name = n
	}
	if m.files == nil {
		return core.NotImplemented("Files are not available")
	}
	if !p.Has("files:read") {
		return core.Forbidden("Missing required scope: files:read")
	}
	done, err := m.acquireTransfer(r.Context())
	if err != nil {
		return err
	}
	defer done()
	f, entry, err := m.files.Open(r.Context(), p, in.FileID)
	if err != nil {
		return err
	}
	defer f.Close()
	if entry != nil && entry.IsDir {
		return fieldError("file_id", "That is a folder, not a spreadsheet")
	}
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() > maxImportBytes {
		return core.NewError(http.StatusRequestEntityTooLarge, "payload_too_large", "Imports are limited to 50 MiB")
	}
	if st.Size() == 0 {
		return fieldError("file_id", "The file is empty")
	}
	fileName := ""
	if entry != nil {
		fileName = entry.Name
	}
	if name == "" {
		name = defaultName(fileName)
	}
	wb, err := parseWorkbook(f, st.Size(), formatFromName(fileName), csvSheetName(fileName))
	if err != nil {
		return err
	}
	meta, err := m.insertWorkbook(r.Context(), p, name, wb, true)
	if err != nil {
		return err
	}
	return writeVersioned(w, r, http.StatusCreated, meta.Version, mustJSON(summaryOf(*meta, wb)))
}

// exportOptions selects what to write.
type exportOptions struct {
	format  string
	sheetID string
	raw     bool
}

// writeExport renders the caller's workbook into f (under the workbook lock) and returns
// the suggested file name.
func (m *Module) writeExport(ctx context.Context, p *core.Principal, id string, o exportOptions, f *os.File) (string, error) {
	var fileName string
	err := m.withWorkbook(ctx, p, id, func(e *entry) error {
		wb := e.eng.Workbook()
		base := safeFileName(e.meta.Name)
		switch o.format {
		case "csv":
			sh := wb.Sheets[0]
			if o.sheetID != "" {
				var err error
				if sh, _, err = sheetOf(e, o.sheetID); err != nil {
					return err
				}
			}
			if len(wb.Sheets) > 1 {
				base = safeFileName(e.meta.Name + " - " + sh.Name)
			}
			fileName = base + ".csv"
			return csvio.Write(f, sh, csvio.WriteOptions{Raw: o.raw, BOM: true})
		case "xlsx":
			fileName = base + ".xlsx"
			return xlsx.Write(f, wb)
		default:
			fileName = base + ".xls"
			if err := xls.Write(f, wb); err != nil {
				if errors.Is(err, xls.ErrTooLarge) {
					return fieldError("format", "The workbook exceeds the XLS limits (65,536 rows × 256 columns, 4,000 styles); export it as XLSX instead")
				}
				return err
			}
			return nil
		}
	})
	if err != nil {
		return "", err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	return fileName, nil
}

func parseExportQuery(r *http.Request) (exportOptions, error) {
	q := r.URL.Query()
	o := exportOptions{raw: httpx.QueryBool(r, "raw")}
	f := strings.ToLower(strings.TrimSpace(q.Get("format")))
	switch f {
	case "", "xlsx":
		o.format = "xlsx"
	case "xls", "csv":
		o.format = f
	default:
		return o, core.BadRequest("format must be xlsx, xls or csv")
	}
	if sid := q.Get("sheet_id"); sid != "" {
		if !httpx.ValidID(sid) {
			return o, errSheetNotFound
		}
		o.sheetID = sid
	}
	return o, nil
}

func (m *Module) exportWorkbook(w http.ResponseWriter, r *http.Request) error {
	p := core.MustPrincipal(r.Context())
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return errWorkbookNotFound
	}
	o, err := parseExportQuery(r)
	if err != nil {
		return err
	}
	done, err := m.acquireTransfer(r.Context())
	if err != nil {
		return err
	}
	defer done()
	f, cleanup, err := m.tempFile()
	if err != nil {
		return err
	}
	defer cleanup()
	name, err := m.writeExport(r.Context(), p, id, o, f)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		return err
	}
	h := w.Header()
	h.Set("Content-Type", contentTypes[o.format])
	h.Set("Content-Disposition", contentDisposition(name))
	h.Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return nil
	}
	_, err = io.Copy(w, f)
	return err
}

func (m *Module) exportToFiles(w http.ResponseWriter, r *http.Request) error {
	p := core.MustPrincipal(r.Context())
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return errWorkbookNotFound
	}
	var in struct {
		Format    string `json:"format"`
		SpaceID   string `json:"space_id"`
		Path      string `json:"path"`
		Name      string `json:"name"`
		SheetID   string `json:"sheet_id"`
		Overwrite bool   `json:"overwrite"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	fe := map[string]string{}
	o := exportOptions{}
	switch f := strings.ToLower(strings.TrimSpace(in.Format)); f {
	case "xlsx", "xls", "csv":
		o.format = f
	default:
		fe["format"] = "format must be xlsx, xls or csv"
	}
	if !httpx.ValidID(in.SpaceID) {
		fe["space_id"] = "space_id is required"
	}
	dir := strings.Trim(strings.TrimSpace(in.Path), "/")
	if len(in.Path) > 4096 || strings.ContainsRune(dir, 0) || strings.Contains(dir, "\\") {
		fe["path"] = "Invalid folder path"
	} else {
		for _, seg := range strings.Split(dir, "/") {
			if dir != "" && (seg == "" || seg == "." || seg == "..") {
				fe["path"] = "Invalid folder path"
				break
			}
		}
	}
	name := strings.TrimSpace(in.Name)
	if name != "" {
		if msg := checkFileName(name); msg != "" {
			fe["name"] = msg
		}
	}
	if in.SheetID != "" {
		if !httpx.ValidID(in.SheetID) {
			return errSheetNotFound
		}
		o.sheetID = in.SheetID
	}
	if len(fe) > 0 {
		return core.Validation(fe)
	}
	if m.files == nil {
		return core.NotImplemented("Files are not available")
	}
	if !p.Has("files:write") {
		return core.Forbidden("Missing required scope: files:write")
	}
	done, err := m.acquireTransfer(r.Context())
	if err != nil {
		return err
	}
	defer done()
	f, cleanup, err := m.tempFile()
	if err != nil {
		return err
	}
	defer cleanup()
	suggested, err := m.writeExport(r.Context(), p, id, o, f)
	if err != nil {
		return err
	}
	if name == "" {
		name = suggested
	} else if !strings.EqualFold(path.Ext(name), "."+o.format) {
		name += "." + o.format
	}
	if len(name) > 255 {
		return fieldError("name", "The file name is too long")
	}
	entry, err := m.files.CreateFromReader(r.Context(), p, in.SpaceID, dir, name, f, files.CreateOpts{
		Overwrite: in.Overwrite, ModTime: core.Now(), Category: "document", Subcategory: "spreadsheet",
	})
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, entryJSON(entry))
}

// checkFileName applies the Files naming rules (ARCHITECTURE.md §2.10) early for a clear 422.
func checkFileName(name string) string {
	switch {
	case !utf8.ValidString(name) || len(name) > 255:
		return "File names must be valid UTF-8 and at most 255 bytes"
	case name == "." || name == "..":
		return "Invalid file name"
	case strings.ContainsAny(name, "/\\\x00"):
		return "File names cannot contain / or \\"
	case strings.HasSuffix(name, ".") || strings.HasSuffix(name, " "):
		return "File names cannot end with a dot or space"
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "File names cannot contain control characters"
		}
	}
	return ""
}

// safeFileName turns a workbook name into a portable file name stem.
func safeFileName(name string) string {
	s := strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r), strings.ContainsRune(`/\:*?"<>|`, r):
			return '_'
		}
		return r
	}, name)
	s = strings.Trim(strings.TrimSpace(s), ".")
	if rs := []rune(s); len(rs) > 120 {
		s = string(rs[:120])
	}
	if s == "" {
		return "workbook"
	}
	return s
}

// contentDisposition builds an attachment header with an ASCII fallback and RFC 5987 name.
func contentDisposition(name string) string {
	ascii := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' || r == '%' {
			return '_'
		}
		return r
	}, name)
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, ascii, rfc5987(name))
}

// rfc5987 percent-encodes everything outside RFC 5987's attr-char set.
func rfc5987(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.IndexByte("!#$&+-.^_`|~", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// entryJSON renders a files.Entry in the API Entry shape (API.md §5).
func entryJSON(e *files.Entry) map[string]any {
	var parent any
	if e.ParentID != "" {
		parent = e.ParentID
	}
	tags := e.Tags
	if tags == nil {
		tags = []string{}
	}
	return map[string]any{
		"id": e.ID, "space_id": e.SpaceID, "parent_id": parent, "path": e.Path, "name": e.Name,
		"is_dir": e.IsDir, "size": e.Size, "mime": e.Mime, "mod_time": db.FormatTime(e.ModTime),
		"category": e.Category, "subcategory": e.Subcategory, "tags": tags, "sha256": e.SHA256,
		"owner_id": e.OwnerID, "created_at": db.FormatTime(e.CreatedAt), "updated_at": db.FormatTime(e.UpdatedAt),
		"has_thumbnail": false,
	}
}
