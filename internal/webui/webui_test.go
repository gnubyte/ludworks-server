package webui

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"ludworks/web"
)

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":             {Data: []byte("<!doctype html><title>x</title>")},
		"assets/js/app.js":       {Data: []byte("export const x = 1;")},
		"assets/css/app.css":     {Data: []byte("body{}")},
		"assets/img/logo.svg":    {Data: []byte("<svg/>")},
		"assets/fonts/a.woff2":   {Data: []byte("wOF2")},
		"assets/data/x.json":     {Data: []byte("{}")},
		"assets/favicon.ico":     {Data: []byte("ico")},
		"assets/img/p.png":       {Data: []byte("png")},
		"assets/.keep":           {Data: []byte("")},
		"assets/.secret/key.txt": {Data: []byte("nope")},
	}
}

func get(h http.Handler, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestMIMETypes(t *testing.T) {
	h, err := New(testFS())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"/assets/js/app.js":     "text/javascript; charset=utf-8",
		"/assets/css/app.css":   "text/css; charset=utf-8",
		"/assets/img/logo.svg":  "image/svg+xml",
		"/assets/fonts/a.woff2": "font/woff2",
		"/assets/data/x.json":   "application/json",
		"/assets/favicon.ico":   "image/x-icon",
		"/assets/img/p.png":     "image/png",
	}
	for p, ct := range want {
		w := get(h, "GET", p, nil)
		if w.Code != 200 || w.Header().Get("Content-Type") != ct || w.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: %d %q %q", p, w.Code, w.Header().Get("Content-Type"), w.Header().Get("Cache-Control"))
		}
	}
}

func TestSPAFallbackAnd404s(t *testing.T) {
	h, _ := New(testFS())
	for _, p := range []string{"/", "/files", "/tv/channels/123", "/notes?x=1", "/index.html"} {
		w := get(h, "GET", p, nil)
		if w.Code != 200 || w.Header().Get("Content-Type") != "text/html; charset=utf-8" || w.Body.String() != "<!doctype html><title>x</title>" {
			t.Errorf("%s: %d %q", p, w.Code, w.Header().Get("Content-Type"))
		}
		if w.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: cache-control %q", p, w.Header().Get("Cache-Control"))
		}
	}
	for _, p := range []string{"/assets/missing.js", "/assets/js", "/assets/js/", "/assets", "/assets/.keep",
		"/assets/.secret/key.txt", "/assets/../index.html", "/assets/js/..%2fapp.js", "/api/v1/x"} {
		if w := get(h, "GET", p, nil); w.Code != 404 {
			t.Errorf("%s: %d, want 404", p, w.Code)
		}
	}
	if w := get(h, "POST", "/", nil); w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("POST: %d", w.Code)
	}
}

func TestETagAndHEAD(t *testing.T) {
	h, _ := New(testFS())
	w := get(h, "GET", "/assets/js/app.js", nil)
	etag := w.Header().Get("ETag")
	if len(etag) < 10 || etag[0] != '"' {
		t.Fatalf("etag %q", etag)
	}
	if w := get(h, "GET", "/assets/js/app.js", map[string]string{"If-None-Match": etag}); w.Code != 304 || w.Body.Len() != 0 {
		t.Fatalf("conditional: %d", w.Code)
	}
	if w := get(h, "GET", "/assets/js/app.js", map[string]string{"If-None-Match": `"other"`}); w.Code != 200 {
		t.Fatalf("mismatched etag: %d", w.Code)
	}
	if w := get(h, "GET", "/deep/link", map[string]string{"If-None-Match": get(h, "GET", "/", nil).Header().Get("ETag")}); w.Code != 304 {
		t.Fatalf("index conditional: %d", w.Code)
	}
	w = get(h, "HEAD", "/assets/css/app.css", nil)
	if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Length") != "6" {
		t.Fatalf("HEAD: %d len=%d cl=%q", w.Code, w.Body.Len(), w.Header().Get("Content-Length"))
	}
}

func TestEmbeddedUIIndexes(t *testing.T) {
	if _, err := New(web.FS()); err != nil {
		t.Fatal(err)
	}
}
