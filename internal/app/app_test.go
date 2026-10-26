package app

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"ludworks/internal/config"
	"ludworks/internal/httpx"
)

func testConfig(t *testing.T, tlsMode string) *config.Config {
	t.Helper()
	return &config.Config{
		DataDir: filepath.Join(t.TempDir(), "data"),
		Listen:  "127.0.0.1:0",
		TLS:     config.TLSConfig{Mode: tlsMode},
		Log:     config.LogConfig{Level: "info", Format: "text"},
	}
}

func startApp(t *testing.T, cfg *config.Config) (*App, string) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := New(context.Background(), cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := a.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	scheme := "http"
	if cfg.TLS.Mode != config.TLSOff {
		scheme = "https"
	}
	return a, scheme + "://" + a.Addr().String()
}

func TestSmokePlainHTTP(t *testing.T) {
	cfg := testConfig(t, config.TLSOff)
	a, base := startApp(t, cfg)
	client := &http.Client{Timeout: 10 * time.Second}
	get := func(path string) (*http.Response, string) {
		t.Helper()
		resp, err := client.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}

	resp, body := get("/api/v1/health")
	var h map[string]string
	if resp.StatusCode != 200 || json.Unmarshal([]byte(body), &h) != nil || h["status"] != "ok" || h["version"] == "" || h["time"] == "" {
		t.Fatalf("health: %d %s", resp.StatusCode, body)
	}
	if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("health headers: %v", resp.Header)
	}

	resp, body = get("/")
	if resp.StatusCode != 200 || !strings.Contains(strings.ToLower(body), "<!doctype html") || resp.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("index: %d %q", resp.StatusCode, body[:min(len(body), 80)])
	}
	resp, _ = get("/tv/channels/deep-link")
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("spa fallback: %d", resp.StatusCode)
	}

	resp, body = get("/api/openapi.json")
	var doc struct {
		OpenAPI string
		Paths   map[string]any
	}
	if resp.StatusCode != 200 || json.Unmarshal([]byte(body), &doc) != nil || doc.OpenAPI == "" || len(doc.Paths) < 2 {
		t.Fatalf("openapi: %d", resp.StatusCode)
	}
	if resp, _ = get("/api/docs"); resp.StatusCode != 200 {
		t.Fatalf("docs: %d", resp.StatusCode)
	}

	// Setup gate: stub auth reports setup required.
	for _, p := range []string{"/api/v1/files", "/api/v1/me", "/api/v1/admin/users"} {
		resp, body = get(p)
		if resp.StatusCode != 503 || !strings.Contains(body, `"setup_required"`) {
			t.Fatalf("%s: %d %s", p, resp.StatusCode, body)
		}
	}
	resp, body = get("/api/v1/does-not-exist")
	if resp.StatusCode != 503 {
		t.Fatalf("unknown api before setup: %d %s", resp.StatusCode, body)
	}

	// Data dir layout and secret key.
	for _, p := range []string{cfg.DBPath(), cfg.SecretKeyPath(), cfg.StorageDir(), cfg.TmpDir(), cfg.ThumbsDir(), cfg.BackupsDir(), cfg.TLSDir()} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if runtime.GOOS != "windows" && st.IsDir() && st.Mode().Perm() != 0o700 {
			t.Errorf("%s mode %v", p, st.Mode().Perm())
		}
	}
	// Module settings namespaces are registered.
	pub := a.Deps().Settings.Public(context.Background())
	for _, ns := range []string{"general", "security", "smtp", "imap", "files", "media", "tv"} {
		if _, ok := pub[ns]; !ok {
			t.Errorf("settings namespace %s missing", ns)
		}
	}
	if m, _ := pub["media"].(map[string]any); fmt.Sprint(m["tmdb_api_key"]) != "map[set:false]" {
		t.Errorf("media secret not redacted: %v", m["tmdb_api_key"])
	}
}

// Every route the generic rate limiter exempts (tus chunks, content Range requests) is
// registered under exactly that pattern, so a renamed route cannot silently lose (or keep
// a stale) exemption.
func TestRateLimitExemptRoutesExist(t *testing.T) {
	cfg := testConfig(t, config.TLSOff)
	a, _ := startApp(t, cfg)
	rt, ok := a.Handler().(*httpx.Router)
	if !ok {
		t.Fatal("handler is not the router")
	}
	wild := regexp.MustCompile(`\{[^}]+\}`)
	for _, p := range httpx.RateLimitExemptPatterns() {
		method, path, _ := strings.Cut(p, " ")
		r := httptest.NewRequest(method, wild.ReplaceAllString(path, "x"), nil)
		if got := rt.RouteFor(r); got != p {
			t.Errorf("exempt pattern %q: request %s %s is served by %q", p, method, r.URL.Path, got)
		}
	}
}

func TestSmokeSelfSignedTLS(t *testing.T) {
	cfg := testConfig(t, config.TLSSelfSigned)
	a, base := startApp(t, cfg)
	info := a.Deps().Info.CurrentTLS()
	if info == nil || info.FingerprintSHA256 == "" {
		t.Fatal("no TLS info")
	}
	var seen string
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{ForceAttemptHTTP2: true, TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, // we pin below, like clients do (TOFU)
		VerifyConnection: func(cs tls.ConnectionState) error {
			sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
			seen = fmt.Sprintf("% X", sum[:])
			return nil
		},
	}}}
	resp, err := client.Get(base + "/api/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Strict-Transport-Security") == "" {
		t.Fatalf("status %d hsts %q", resp.StatusCode, resp.Header.Get("Strict-Transport-Security"))
	}
	if strings.ReplaceAll(seen, " ", ":") != info.FingerprintSHA256 {
		t.Fatalf("served cert %s != reported %s", seen, info.FingerprintSHA256)
	}
	if resp.ProtoMajor != 2 {
		t.Errorf("HTTP/2 not negotiated: %s", resp.Proto)
	}
}

func TestRunGracefulShutdown(t *testing.T) {
	cfg := testConfig(t, config.TLSOff)
	a, err := New(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for a.Addr() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if a.Addr() == nil {
		t.Fatal("never started")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	// Data survives a restart (secret key reused, settings decryptable).
	b, err := New(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	b.Shutdown(context.Background())
}

func TestOpenCLI(t *testing.T) {
	cfg := testConfig(t, config.TLSOff)
	d, closeFn, err := OpenCLI(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	if d.Users == nil || d.Mailer == nil || d.Audit == nil || d.Settings == nil {
		t.Fatal("deps incomplete")
	}
}

func TestTLSHosts(t *testing.T) {
	dns, ips := tlsHosts("nas.example.com:8443")
	if dns[len(dns)-1] != "nas.example.com" {
		t.Fatal(dns)
	}
	_, ips2 := tlsHosts("192.0.2.7:8443")
	if !ips2[len(ips2)-1].Equal([]byte{192, 0, 2, 7}) {
		t.Fatal(ips2)
	}
	_, ips3 := tlsHosts(":8443")
	if len(ips3) != len(ips) {
		t.Fatal("unspecified host changed SANs")
	}
}

// The main server bounds HTTP/2 connections whose peer stops reading (it would otherwise pin
// stream and transcode slots) or disappears.
func TestHTTP2ConnectionTimeouts(t *testing.T) {
	cfg := testConfig(t, config.TLSSelfSigned)
	a, _ := startApp(t, cfg)
	h2 := a.servers[0].HTTP2
	if h2 == nil || h2.WriteByteTimeout != HTTP2WriteByteTimeout || h2.WriteByteTimeout <= 0 ||
		h2.SendPingTimeout != HTTP2SendPingTimeout || h2.PingTimeout != HTTP2PingTimeout {
		t.Fatalf("HTTP2 config = %+v", h2)
	}
}
