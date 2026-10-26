// Package webui serves the embedded single-page web UI.
//
// /assets/** are served as files with explicit MIME types (never the OS registry, which on
// Windows can map .js to text/plain and break module loading under nosniff), strong ETags
// and Cache-Control: no-cache (revalidate; assets are not content-hashed). Every other GET
// returns index.html so client-side routes deep-link. No directory listings, no dotfiles.
package webui

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

var contentTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".json":        "application/json",
	".map":         "application/json",
	".webmanifest": "application/manifest+json",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".jpg":         "image/jpeg",
	".jpeg":        "image/jpeg",
	".gif":         "image/gif",
	".webp":        "image/webp",
	".avif":        "image/avif",
	".ico":         "image/x-icon",
	".woff2":       "font/woff2",
	".woff":        "font/woff",
	".ttf":         "font/ttf",
	".otf":         "font/otf",
	".txt":         "text/plain; charset=utf-8",
	".md":          "text/plain; charset=utf-8",
	".wasm":        "application/wasm",
	".mp3":         "audio/mpeg",
	".ogg":         "audio/ogg",
	".wav":         "audio/wav",
}

// ContentType returns the MIME type used for a file name.
func ContentType(name string) string {
	if ct, ok := contentTypes[strings.ToLower(path.Ext(name))]; ok {
		return ct
	}
	return "application/octet-stream"
}

type asset struct {
	etag  string
	ctype string
}

// Handler serves the UI from an fs.FS containing index.html and assets/.
type Handler struct {
	fsys   fs.FS
	assets map[string]asset // fs path ("index.html", "assets/js/app.js") -> metadata
}

// New indexes fsys (computing ETags once; the embedded FS is immutable).
func New(fsys fs.FS) (*Handler, error) {
	h := &Handler{fsys: fsys, assets: map[string]asset{}}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != "." && strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		h.assets[p] = asset{etag: `"` + hex.EncodeToString(sum[:16]) + `"`, ctype: ContentType(p)}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("webui: index assets: %w", err)
	}
	if _, ok := h.assets["index.html"]; !ok {
		return nil, fmt.Errorf("webui: index.html missing")
	}
	return h, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := r.URL.Path
	if p == "/api" || strings.HasPrefix(p, "/api/") {
		// Defensive: the router never sends API paths here.
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if p == "/assets" || strings.HasPrefix(p, "/assets/") {
		name := strings.TrimPrefix(p, "/")
		if !safeName(name) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		a, ok := h.assets[name]
		if !ok { // missing file or a directory: no listings
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		h.serve(w, r, name, a)
		return
	}
	h.serve(w, r, "index.html", h.assets["index.html"])
}

func safeName(name string) bool {
	if !fs.ValidPath(name) || strings.ContainsAny(name, "\\\x00") {
		return false
	}
	for _, seg := range strings.Split(name, "/") {
		if strings.HasPrefix(seg, ".") {
			return false
		}
	}
	return true
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, name string, a asset) {
	f, err := h.fsys.Open(name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		b, err := io.ReadAll(f)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		rs = bytes.NewReader(b)
	}
	hd := w.Header()
	hd.Set("Content-Type", a.ctype)
	hd.Set("ETag", a.etag)
	hd.Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, name, time.Time{}, rs)
}
