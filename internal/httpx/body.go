package httpx

import (
	"errors"
	"io"
	"net/http"

	"ludworks/internal/core"
)

// ErrBodyIncomplete is rendered when the request body could not be read to its end because of
// the client: the connection was closed or reset mid-body, fewer bytes arrived than
// Content-Length declared, the chunked encoding was malformed, or the client stalled past the
// idle timeout. It is a client error (400), not an internal one.
var ErrBodyIncomplete = core.BadRequest("The request body ended early: the client disconnected or sent less data than it declared")

// BodyReadError wraps a failure to read the request body (as opposed to a failure of whatever
// the handler was writing it to). The router wraps every request body so handlers can simply
// return the error from io.Copy and get the right status; errors.Is/As still see the cause.
type BodyReadError struct{ Err error }

func (e *BodyReadError) Error() string { return "reading request body: " + e.Err.Error() }
func (e *BodyReadError) Unwrap() error { return e.Err }

// clientBody tags read errors of the underlying request body. io.EOF passes through untouched
// (it is the normal end of the body, and callers compare it with ==), and so does
// *http.MaxBytesError, which already maps to 413.
type clientBody struct{ rc io.ReadCloser }

func (b *clientBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if err != nil && err != io.EOF {
		var mbe *http.MaxBytesError
		var bre *BodyReadError
		if !errors.As(err, &mbe) && !errors.As(err, &bre) {
			err = &BodyReadError{Err: err}
		}
	}
	return n, err
}

func (b *clientBody) Close() error { return b.rc.Close() }

// wrapBody installs clientBody on r (a no-op for requests without a body).
func wrapBody(r *http.Request) {
	if r.Body == nil || r.Body == http.NoBody {
		return
	}
	if _, ok := r.Body.(*clientBody); ok {
		return
	}
	r.Body = &clientBody{rc: r.Body}
}
