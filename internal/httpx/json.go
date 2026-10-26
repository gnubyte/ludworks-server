package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"ludworks/internal/core"
)

// DefaultMaxBody is the JSON body cap when a route does not override Access.MaxBody.
const DefaultMaxBody int64 = 1 << 20

// WriteJSON writes v as JSON with the given status. API responses are never cached.
func WriteJSON(w http.ResponseWriter, status int, v any) error {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	if h.Get("Cache-Control") == "" {
		h.Set("Cache-Control", "no-store")
	}
	w.WriteHeader(status)
	if status == http.StatusNoContent || status == http.StatusNotModified {
		return nil
	}
	return json.NewEncoder(w).Encode(v)
}

// NoContent writes 204.
func NoContent(w http.ResponseWriter) error {
	w.WriteHeader(http.StatusNoContent)
	return nil
}

type maxBodyKey struct{}

// DecodeJSON decodes the request body into dst. It enforces the route's body limit (default
// 1 MiB, also applied when the route is unlimited), accepts unknown fields (forward
// compatibility), rejects trailing data and non-JSON content types, and returns *core.Error
// values (400 bad_request, 413 payload_too_large, 415 unsupported_media_type).
func DecodeJSON(r *http.Request, dst any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mt, _, err := mime.ParseMediaType(ct)
		if err != nil || !(mt == "application/json" || strings.HasSuffix(mt, "+json")) {
			return core.NewError(http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		}
	}
	if r.Body == nil || r.Body == http.NoBody {
		return core.BadRequest("request body required")
	}
	limit, ok := r.Context().Value(maxBodyKey{}).(int64)
	if !ok || limit <= 0 {
		limit = DefaultMaxBody
	}
	body := http.MaxBytesReader(nil, r.Body, limit)
	dec := json.NewDecoder(body)
	if err := dec.Decode(dst); err != nil {
		return jsonError(err)
	}
	if _, err := dec.Token(); err != io.EOF {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return core.ErrPayloadTooLarge
		}
		return core.BadRequest("unexpected data after JSON body")
	}
	return nil
}

func jsonError(err error) error {
	var (
		mbe *http.MaxBytesError
		bre *BodyReadError
		se  *json.SyntaxError
		te  *json.UnmarshalTypeError
	)
	switch {
	case errors.Is(err, io.EOF):
		return core.BadRequest("request body required")
	case errors.As(err, &mbe):
		return core.ErrPayloadTooLarge
	case errors.As(err, &bre):
		return ErrBodyIncomplete
	case errors.As(err, &se):
		return core.BadRequest(fmt.Sprintf("malformed JSON at offset %d", se.Offset))
	case errors.Is(err, io.ErrUnexpectedEOF):
		return core.BadRequest("malformed JSON: unexpected end of body")
	case errors.As(err, &te):
		if te.Field != "" {
			return core.BadRequest(fmt.Sprintf("field %q: expected %s", te.Field, te.Type))
		}
		return core.BadRequest(fmt.Sprintf("expected JSON %s", te.Type))
	default:
		return core.BadRequest("invalid JSON body")
	}
}

// Page is the cursor-paginated list envelope.
type Page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

// NewPage builds a Page, turning nil items into [] and an empty next cursor into null.
func NewPage[T any](items []T, next string) Page[T] {
	if items == nil {
		items = []T{}
	}
	p := Page[T]{Items: items}
	if next != "" {
		p.NextCursor = &next
	}
	return p
}

// ParseLimit reads ?limit=, returning def when absent/invalid and clamping to [1, max].
func ParseLimit(r *http.Request, def, max int) int {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// ValidID reports whether s matches the API id format ^[A-Za-z0-9_-]{8,64}$.
func ValidID(s string) bool { return idPattern.MatchString(s) }

// PathID returns the path wildcard name, or a 404 not_found error when it is not a valid id
// (malformed ids can never exist, and 404 avoids leaking anything).
func PathID(r *http.Request, name string) (string, error) {
	v := r.PathValue(name)
	if !ValidID(v) {
		return "", core.ErrNotFound
	}
	return v, nil
}

// QueryBool parses ?key=1|true|yes (case-insensitive) as true.
func QueryBool(r *http.Request, key string) bool {
	switch strings.ToLower(r.URL.Query().Get(key)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

type errorBody struct {
	Error errorPayload `json:"error"`
}

type errorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

type fieldErrorer interface{ FieldErrors() map[string]string }

type loggerKey struct{}

func loggerFrom(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

// WriteError renders err as the standard JSON error body. *core.Error values (also when
// wrapped) are rendered as-is; validation errors from settings become 422; anything else is
// logged and rendered as a generic 500 that carries only the request id. If the response has
// already started, the error is only logged.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	ce := ToCoreError(r.Context(), err)
	if ce == nil { // client went away
		return
	}
	if rec, ok := w.(*recorder); ok && rec.wroteHeader {
		loggerFrom(r.Context()).Warn("error after response started", "request_id", core.RequestID(r.Context()), "err", err)
		return
	}
	switch ce.Status {
	case http.StatusUnauthorized:
		w.Header().Set("WWW-Authenticate", `Bearer realm="ludworks"`)
	}
	// Do not let a handler's partial headers (e.g. Content-Disposition) leak into the error.
	w.Header().Del("Content-Disposition")
	w.Header().Del("Content-Length")
	w.Header().Del("Content-Encoding")
	w.Header().Del("ETag")
	w.Header().Del("Last-Modified")
	_ = WriteJSON(w, ce.Status, errorBody{Error: errorPayload{Code: ce.Code, Message: ce.Message, Details: ce.Details}})
}

// ToCoreError maps any error to the *core.Error that would be rendered. It returns nil when
// the error is just the client disconnecting. Unknown errors are logged here.
func ToCoreError(ctx context.Context, err error) *core.Error {
	var (
		ce  *core.Error
		fe  fieldErrorer
		mbe *http.MaxBytesError
		bre *BodyReadError
	)
	switch {
	case errors.As(err, &ce):
		return ce
	case errors.As(err, &fe):
		return core.Validation(fe.FieldErrors())
	case errors.As(err, &mbe):
		return core.ErrPayloadTooLarge
	case errors.As(err, &bre):
		// The client stopped sending (aborted upload, dropped connection): not a server fault.
		loggerFrom(ctx).Info("request body incomplete", "request_id", core.RequestID(ctx), "err", bre.Err)
		return ErrBodyIncomplete
	case (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) && ctx.Err() != nil:
		return nil
	}
	rid := core.RequestID(ctx)
	loggerFrom(ctx).Error("internal error", "request_id", rid, "err", err)
	e := *core.ErrInternal
	if rid != "" {
		e.Details = map[string]any{"request_id": rid}
	}
	return &e
}
