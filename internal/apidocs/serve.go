package apidocs

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/httpx"
	"ludworks/internal/webui"
)

//go:embed base.json
var baseJSON []byte

// Only the vendored files we need (see swagger-ui/VERSION and SHA256SUMS).
//
//go:embed swagger-ui/swagger-ui-bundle.js swagger-ui/swagger-ui.css swagger-ui/LICENSE swagger-ui/NOTICE
//go:embed swagger-ui/swagger-ui-bundle.js.LICENSE.txt swagger-ui/VERSION swagger-ui/SHA256SUMS
var swaggerFS embed.FS

//go:embed static/index.html static/init.js
var staticFS embed.FS

// Base returns the embedded base.json.
func Base() []byte { return baseJSON }

// DocsCSP is the page-specific policy for Swagger UI: like the global CSP, but Swagger UI
// needs inline styles and data: images.
const DocsCSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; " +
	"connect-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"

// Register mounts GET /api/openapi.json, /api/docs and /api/docs/{file} (all public).
func Register(r *httpx.Router, doc []byte) {
	docETag := etag(doc)
	r.HandleRaw("GET /api/openapi.json", httpx.Public(), http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "application/json; charset=utf-8")
		h.Set("Cache-Control", "no-cache")
		h.Set("ETag", docETag)
		http.ServeContent(w, req, "openapi.json", time.Time{}, bytes.NewReader(doc))
	}))
	page := func(w http.ResponseWriter, req *http.Request) {
		serveFile(w, req, staticFS, "static/index.html")
	}
	r.HandleRaw("GET /api/docs", httpx.Public(), http.HandlerFunc(page))
	r.HandleRaw("GET /api/docs/{$}", httpx.Public(), http.HandlerFunc(page))
	r.HandleRaw("GET /api/docs/{file}", httpx.Public(), http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch f := req.PathValue("file"); f {
		case "init.js":
			serveFile(w, req, staticFS, "static/init.js")
		case "swagger-ui-bundle.js", "swagger-ui.css", "LICENSE", "NOTICE", "swagger-ui-bundle.js.LICENSE.txt", "VERSION", "SHA256SUMS":
			serveFile(w, req, swaggerFS, "swagger-ui/"+f)
		default:
			httpx.WriteError(w, req, core.ErrNotFound)
		}
	}))
}

func serveFile(w http.ResponseWriter, req *http.Request, fsys fs.FS, name string) {
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		httpx.WriteError(w, req, core.ErrNotFound)
		return
	}
	h := w.Header()
	ct := webui.ContentType(name)
	if ct == "application/octet-stream" { // LICENSE, NOTICE, VERSION, SHA256SUMS
		ct = "text/plain; charset=utf-8"
	}
	h.Set("Content-Type", ct)
	h.Set("Content-Security-Policy", DocsCSP)
	h.Set("Cache-Control", "no-cache")
	h.Set("ETag", etag(b))
	http.ServeContent(w, req, name, time.Time{}, bytes.NewReader(b))
}

func etag(b []byte) string {
	s := sha256.Sum256(b)
	return `"` + hex.EncodeToString(s[:16]) + `"`
}
