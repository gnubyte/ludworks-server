package httpx

import (
	"io"
	"net/http"
)

// recorder captures status and size for logging. It forwards io.ReaderFrom (so file
// responses keep using sendfile) and exposes Unwrap for http.ResponseController (Flush,
// SetWriteDeadline, Hijack on streaming routes).
type recorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (r *recorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		r.ResponseWriter.WriteHeader(code) // informational (e.g. 103), may repeat
		return
	}
	r.status = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *recorder) ReadFrom(src io.Reader) (int64, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	var n int64
	var err error
	if rf, ok := r.ResponseWriter.(io.ReaderFrom); ok {
		n, err = rf.ReadFrom(src)
	} else {
		n, err = io.Copy(struct{ io.Writer }{r.ResponseWriter}, src)
	}
	r.bytes += n
	return n, err
}

func (r *recorder) Flush() {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(r.ResponseWriter).Flush()
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// HeaderWritten reports whether the response status line has been sent (useful for streaming
// handlers deciding whether an error can still be rendered).
func HeaderWritten(w http.ResponseWriter) bool {
	if r, ok := w.(*recorder); ok {
		return r.wroteHeader
	}
	return false
}
