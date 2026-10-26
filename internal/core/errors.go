package core

import (
	"fmt"
	"net/http"
)

// Error is an API error rendered by httpx as {"error":{"code","message","details"}}.
// Return it (possibly wrapped with %w) from handlers and services.
type Error struct {
	Status  int
	Code    string
	Message string
	Details any
}

func (e *Error) Error() string { return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message) }

// NewError creates an Error.
func NewError(status int, code, msg string) *Error {
	return &Error{Status: status, Code: code, Message: msg}
}

// WithDetails returns a copy of e carrying details (ready-made errors are shared values, so
// never mutate them in place).
func (e *Error) WithDetails(details any) *Error {
	c := *e
	c.Details = details
	return &c
}

// Ready-made errors. Treat as immutable.
var (
	ErrBadRequest       = NewError(http.StatusBadRequest, "bad_request", "Bad request")
	ErrUnauthorized     = NewError(http.StatusUnauthorized, "unauthorized", "Authentication required")
	ErrForbidden        = NewError(http.StatusForbidden, "forbidden", "You do not have permission to do this")
	ErrNotFound         = NewError(http.StatusNotFound, "not_found", "Not found")
	ErrConflict         = NewError(http.StatusConflict, "conflict", "Conflict")
	ErrGone             = NewError(http.StatusGone, "gone", "Gone")
	ErrPayloadTooLarge  = NewError(http.StatusRequestEntityTooLarge, "payload_too_large", "Request body too large")
	ErrUnsupportedMedia = NewError(http.StatusUnsupportedMediaType, "unsupported_media_type", "Unsupported media type")
	ErrRateLimited      = NewError(http.StatusTooManyRequests, "rate_limited", "Too many requests")
	ErrSetupRequired    = NewError(http.StatusServiceUnavailable, "setup_required", "Initial setup has not been completed")
	ErrInternal         = NewError(http.StatusInternalServerError, "internal", "Internal server error")
	ErrNotImplemented   = NewError(http.StatusNotImplemented, "not_implemented", "Not implemented")
)

// BadRequest returns a 400 bad_request error.
func BadRequest(msg string) *Error { return NewError(http.StatusBadRequest, "bad_request", msg) }

// NotFound returns a 404 not_found error.
func NotFound(msg string) *Error { return NewError(http.StatusNotFound, "not_found", msg) }

// Forbidden returns a 403 forbidden error.
func Forbidden(msg string) *Error { return NewError(http.StatusForbidden, "forbidden", msg) }

// Unauthorized returns a 401 unauthorized error.
func Unauthorized(msg string) *Error { return NewError(http.StatusUnauthorized, "unauthorized", msg) }

// Conflict returns a 409 conflict error.
func Conflict(msg string) *Error { return NewError(http.StatusConflict, "conflict", msg) }

// NotImplemented returns a 501 not_implemented error.
func NotImplemented(msg string) *Error {
	return NewError(http.StatusNotImplemented, "not_implemented", msg)
}

// Validation returns a 422 validation_failed error with details {"fields": fields}.
func Validation(fields map[string]string) *Error {
	return &Error{
		Status:  http.StatusUnprocessableEntity,
		Code:    "validation_failed",
		Message: "Validation failed",
		Details: map[string]any{"fields": fields},
	}
}
