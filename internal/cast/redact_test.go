package cast

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ludworks/internal/httpx"
)

// Tickets are capabilities: the router's access log must never contain one, neither for the
// ticket route itself nor for the CORS preflight answered before route matching.
func TestTicketsNeverLogged(t *testing.T) {
	var buf bytes.Buffer
	rt := httpx.NewRouter(httpx.Options{Log: slog.New(slog.NewTextHandler(&buf, nil))})
	rt.Handle("GET "+mediaRoute, httpx.Public(), func(w http.ResponseWriter, r *http.Request) error { return errTicket })
	ticket := strings.Repeat("Q", ticketLen)
	path := mediaPathPrefix + ticket + "/media/abcdefgh12/stream"

	pre := httptest.NewRequest("OPTIONS", path, nil)
	pre.Header.Set("Origin", "https://www.gstatic.com")
	pre.Header.Set("Access-Control-Request-Method", "GET")
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, pre)
	if rec.Code != 204 || rec.Header().Get("Access-Control-Allow-Origin") != "*" || rec.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("preflight: %d %v", rec.Code, rec.Header())
	}
	rec = httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	if rec.Code != 404 {
		t.Fatalf("get: %d", rec.Code)
	}
	if strings.Contains(buf.String(), ticket) || strings.Count(buf.String(), "[redacted]") != 2 {
		t.Fatalf("ticket in the access log:\n%s", buf.String())
	}
}
