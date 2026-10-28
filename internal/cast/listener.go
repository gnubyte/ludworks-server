package cast

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/httpx"
)

// The cast listener (cast.media_port) is a plain-HTTP server for devices that cannot trust
// the server's (self-signed) certificate. It serves nothing but the ticket route: every
// other path is 404, nothing reads a request body, and it has its own per-address limiter
// for requests that are not Range reads.

// Listener limits.
const (
	listenerRPS   = 30
	listenerBurst = 120
)

// castListener is a running cast listener.
type castListener struct {
	port int
	srv  *http.Server
	done chan struct{}
}

// startListener binds port on every interface and serves the ticket route until close.
func (m *Module) startListener(ctx context.Context, port int) (*castListener, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	l := &castListener{port: port, done: make(chan struct{})}
	l.srv = &http.Server{
		Handler:           m.listenerHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(m.log.Handler(), slog.LevelDebug),
		BaseContext:       func(net.Listener) context.Context { return ctx },
		// No ReadTimeout/WriteTimeout: responses are long media streams. Requests carry no
		// body, and stalled readers are dropped by the media services' write deadlines.
	}
	go func() {
		defer close(l.done)
		if err := l.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			m.log.Error("cast: media listener stopped", "port", port, "err", err)
		}
	}()
	m.log.Info("cast: media listener started (plain HTTP, ticket routes only)", "port", port)
	return l, nil
}

// close stops the listener, giving open streams a moment to finish.
func (l *castListener) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := l.srv.Shutdown(ctx); err != nil {
		_ = l.srv.Close()
	}
	<-l.done
}

// listenerHandler serves the ticket route only.
func (m *Module) listenerHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+mediaRoute, func(w http.ResponseWriter, r *http.Request) {
		// Devices fetch directly from the listener: the TCP peer is the device.
		if err := m.serveMedia(w, r, peerIP(r), true); err != nil {
			writeListenerError(w, r, err)
		}
	})
	limiter := httpx.NewKeyedLimiter(listenerRPS, listenerBurst)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer := peerIP(r)
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; sandbox")
		h.Set("Cache-Control", "no-store")
		rw := &statusWriter{ResponseWriter: w}
		defer func() {
			if p := recover(); p != nil {
				if p == http.ErrAbortHandler {
					panic(p)
				}
				m.log.Error("cast: panic in media listener", "panic", p, "stack", string(debug.Stack()))
				if !rw.wrote {
					writeListenerError(rw, r, core.ErrInternal)
				}
			}
		}()
		rangeRead := r.Method == http.MethodGet && strings.HasPrefix(r.Header.Get("Range"), "bytes=")
		if !rangeRead {
			if ok, wait := limiter.Allow(httpx.RateKeyForIP(peer.String())); !ok {
				h.Set("Retry-After", strconv.Itoa(int(wait/time.Second)+1))
				writeListenerError(rw, r, core.ErrRateLimited)
				return
			}
		}
		if !strings.HasPrefix(r.URL.Path, mediaPathPrefix) {
			writeListenerError(rw, r, core.ErrNotFound)
			return
		}
		if r.Method == http.MethodOptions {
			mediaPreflight(rw)
			return
		}
		ua := r.UserAgent()
		if len(ua) > 512 {
			ua = ua[:512]
		}
		ctx := core.WithRequestMeta(r.Context(), core.RequestMeta{ID: core.NewID(), IP: peer.String(), UserAgent: ua})
		r = r.WithContext(ctx)
		if _, pattern := mux.Handler(r); pattern == "" {
			setMediaHeaders(h)
			writeListenerError(rw, r, core.ErrNotFound)
			return
		}
		mux.ServeHTTP(rw, r)
	})
}

// statusWriter remembers whether the response started (errors after that are only logged).
type statusWriter struct {
	http.ResponseWriter
	wrote bool
}

func (s *statusWriter) WriteHeader(code int) {
	s.wrote = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	s.wrote = true
	return s.ResponseWriter.Write(b)
}

// ReadFrom keeps the sendfile fast path of http.ServeContent.
func (s *statusWriter) ReadFrom(src io.Reader) (int64, error) {
	s.wrote = true
	if rf, ok := s.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(src)
	}
	return io.Copy(struct{ io.Writer }{s.ResponseWriter}, src)
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// writeListenerError renders err like the main router does (unless the response started).
func writeListenerError(w http.ResponseWriter, r *http.Request, err error) {
	if sw, ok := w.(*statusWriter); ok && sw.wrote {
		return
	}
	ce := httpx.ToCoreError(r.Context(), err)
	if ce == nil {
		return
	}
	httpx.WriteError(w, r, ce)
}
