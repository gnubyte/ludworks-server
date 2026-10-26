package sheets

import (
	"compress/gzip"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"ludworks/internal/core"
	"ludworks/internal/sheets/csvio"
	"ludworks/internal/sheets/engine"
	"ludworks/internal/sheets/xls"
	"ludworks/internal/sheets/xlsx"
)

var (
	errWorkbookNotFound = core.NotFound("Workbook not found")
	errSheetNotFound    = core.NotFound("Sheet not found")
	errRecalcTimeout    = core.NewError(http.StatusUnprocessableEntity, "recalc_timeout",
		"The workbook took too long to recalculate; the change was not applied")
	errBusy = core.NewError(http.StatusServiceUnavailable, "busy",
		"The workbook is busy with another operation; try again shortly")
	errConflict = core.Conflict("The workbook was changed or deleted elsewhere; reload it and try again")
)

// fieldError returns a 422 validation_failed error for one field.
func fieldError(field, msg string) *core.Error {
	return core.Validation(map[string]string{field: msg})
}

// engineError maps engine sentinel errors to API errors; field names the request field the
// error relates to.
func engineError(err error, field string) error {
	if err == nil {
		return nil
	}
	var ce *core.Error
	switch {
	case errors.As(err, &ce):
		return err
	case errors.Is(err, engine.ErrSheetNotFound):
		return errSheetNotFound
	case errors.Is(err, engine.ErrDuplicateSheet):
		return core.Conflict("A sheet with that name already exists")
	case errors.Is(err, engine.ErrLastSheet):
		return core.Conflict("A workbook must keep at least one sheet")
	case errors.Is(err, engine.ErrInterrupted):
		return errRecalcTimeout
	case errors.Is(err, engine.ErrInvalidRef), errors.Is(err, engine.ErrInvalidSheetName),
		errors.Is(err, engine.ErrTooManySheets), errors.Is(err, engine.ErrTooManyCells),
		errors.Is(err, engine.ErrCellTooLong), errors.Is(err, engine.ErrFormulaSyntax),
		errors.Is(err, engine.ErrFormulaTooDeep), errors.Is(err, engine.ErrFormulaArgs),
		errors.Is(err, engine.ErrInvalidStyle), errors.Is(err, engine.ErrOutOfBounds),
		errors.Is(err, engine.ErrInvalidArgument):
		return fieldError(field, capitalize(err.Error()))
	}
	return err
}

// codecError maps import/export codec errors.
func codecError(err error, format string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, xlsx.ErrTooLarge), errors.Is(err, xls.ErrTooLarge), errors.Is(err, csvio.ErrTooLarge),
		errors.Is(err, engine.ErrTooManyCells), errors.Is(err, engine.ErrTooManySheets):
		return core.NewError(http.StatusRequestEntityTooLarge, "payload_too_large",
			"The spreadsheet exceeds the size limits: "+err.Error())
	case errors.Is(err, xls.ErrEncrypted):
		return fieldError("file", "Password-protected XLS files are not supported")
	case errors.Is(err, xls.ErrUnsupported):
		return fieldError("file", "Unsupported XLS version: "+err.Error())
	case errors.Is(err, xlsx.ErrInvalid), errors.Is(err, xls.ErrInvalid), errors.Is(err, csvio.ErrInvalid):
		return fieldError("file", "Not a valid "+strings.ToUpper(format)+" file: "+err.Error())
	}
	return engineError(err, "file")
}

func capitalize(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}

// validName checks a workbook name: 1..200 characters after trimming, no control characters.
func validName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", fieldError("name", "Name is required")
	}
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) > maxNameLen {
		return "", fieldError("name", "Name must be at most 200 characters")
	}
	for _, r := range name {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return "", fieldError("name", "Name must not contain control characters")
		}
	}
	return name, nil
}

// checkIfMatch enforces an optional If-Match header against the workbook version.
func checkIfMatch(r *http.Request, version int64) error {
	h := strings.TrimSpace(r.Header.Get("If-Match"))
	if h == "" || h == "*" {
		return nil
	}
	for _, part := range strings.Split(h, ",") {
		tag := strings.TrimPrefix(strings.TrimSpace(part), "W/")
		tag = strings.Trim(tag, `"`)
		if v, err := strconv.ParseInt(tag, 10, 64); err == nil && v == version {
			return nil
		}
	}
	return errConflict.WithDetails(map[string]any{"version": version})
}

// writeJSONBytes writes a pre-encoded JSON body, gzip-compressed when large and accepted.
func writeJSONBytes(w http.ResponseWriter, r *http.Request, status int, body []byte) error {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	if h.Get("Cache-Control") == "" {
		h.Set("Cache-Control", "no-store")
	}
	if len(body) >= 32<<10 && acceptsGzip(r) {
		h.Set("Content-Encoding", "gzip")
		h.Add("Vary", "Accept-Encoding")
		w.WriteHeader(status)
		zw, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
		if err != nil {
			return err
		}
		if _, err := zw.Write(body); err != nil {
			return err
		}
		return zw.Close()
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, err := w.Write(body)
	return err
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.EqualFold(strings.TrimSpace(name), "gzip") {
			return !strings.Contains(strings.ReplaceAll(params, " ", ""), "q=0") ||
				strings.Contains(params, "q=0.")
		}
	}
	return false
}
