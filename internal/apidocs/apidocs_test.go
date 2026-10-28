package apidocs

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ludworks/internal/httpx"
)

const testBase = `{"openapi":"3.1.0","info":{"title":"T","version":"1"},"paths":{"/api/v1/health":{"get":{"summary":"h"}}},
"tags":[{"name":"System"}],"components":{"schemas":{"Error":{"type":"object"}}}}`

func TestBuildMerges(t *testing.T) {
	a := Fragment{Name: "files", JSON: []byte(`{"tags":[{"name":"Files","description":"f"}],
		"paths":{"/api/v1/files/{id}":{"parameters":[{"name":"id","in":"path"}],"get":{"summary":"get"}}},
		"components":{"schemas":{"Entry":{"type":"object"},"Error":{"type":"object"}},"parameters":{"Limit":{"name":"limit","in":"query"}}}}`)}
	b := Fragment{Name: "media", JSON: []byte(`{"tags":[{"name":"Files","description":"other"},{"name":"Media"}],
		"paths":{"/api/v1/files/{id}":{"parameters":[{"name":"id","in":"path"}],"patch":{"summary":"patch"}},"/api/v1/media":{"get":{"x-n":12345678901234567890}}},
		"components":{"schemas":{"Media":{"type":"object"}}}}`)}
	out, err := Build([]byte(testBase), "9.9.9", []Fragment{a, b, {Name: "empty"}})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Info       struct{ Version string }
		Paths      map[string]map[string]any
		Tags       []struct{ Name, Description string }
		Components map[string]map[string]any
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Info.Version != "9.9.9" {
		t.Fatalf("version %q", doc.Info.Version)
	}
	item := doc.Paths["/api/v1/files/{id}"]
	if item["get"] == nil || item["patch"] == nil || item["parameters"] == nil || doc.Paths["/api/v1/health"]["get"] == nil {
		t.Fatalf("paths: %v", doc.Paths)
	}
	if len(doc.Tags) != 3 || doc.Tags[1].Name != "Files" || doc.Tags[1].Description != "f" {
		t.Fatalf("tags: %+v", doc.Tags)
	}
	if doc.Components["schemas"]["Entry"] == nil || doc.Components["schemas"]["Media"] == nil || doc.Components["parameters"]["Limit"] == nil {
		t.Fatalf("components: %v", doc.Components)
	}
	if !strings.Contains(string(out), "12345678901234567890") {
		t.Fatal("number precision lost")
	}
	// Deterministic output.
	out2, _ := Build([]byte(testBase), "9.9.9", []Fragment{a, b})
	if string(out) != string(out2) {
		t.Fatal("non-deterministic output")
	}
}

func TestBuildConflicts(t *testing.T) {
	cases := map[string][]Fragment{
		"duplicate operation": {
			{Name: "a", JSON: []byte(`{"paths":{"/x":{"get":{}}}}`)},
			{Name: "b", JSON: []byte(`{"paths":{"/x":{"get":{}}}}`)},
		},
		"duplicate of base": {{Name: "a", JSON: []byte(`{"paths":{"/api/v1/health":{"get":{}}}}`)}},
		"conflicting schema": {
			{Name: "a", JSON: []byte(`{"components":{"schemas":{"S":{"type":"object"}}}}`)},
			{Name: "b", JSON: []byte(`{"components":{"schemas":{"S":{"type":"string"}}}}`)},
		},
		"conflicting base schema": {{Name: "a", JSON: []byte(`{"components":{"schemas":{"Error":{"type":"string"}}}}`)}},
		"conflicting path params": {
			{Name: "a", JSON: []byte(`{"paths":{"/y":{"parameters":[1],"get":{}}}}`)},
			{Name: "b", JSON: []byte(`{"paths":{"/y":{"parameters":[2],"put":{}}}}`)},
		},
		"bad json":         {{Name: "a", JSON: []byte(`{"paths":`)}},
		"unknown key":      {{Name: "a", JSON: []byte(`{"webhooks":{}}`)}},
		"paths not object": {{Name: "a", JSON: []byte(`{"paths":[]}`)}},
	}
	for name, frags := range cases {
		if _, err := Build([]byte(testBase), "1", frags); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestUnresolvedRefs(t *testing.T) {
	doc := `{"paths":{"/a":{"get":{"responses":{"200":{"$ref":"#/components/responses/Ok"},"400":{"$ref":"#/components/responses/Missing"}}}}},
	"components":{"responses":{"Ok":{"description":"ok"}},"schemas":{"A~B/C":{}}},"x":{"$ref":"#/components/schemas/A~0B~1C"}}`
	missing, err := UnresolvedRefs([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 || missing[0] != "#/components/responses/Missing" {
		t.Fatalf("missing = %v", missing)
	}
}

// TestRealFragmentsMerge merges the checked-in base.json with every module's openapi.json
// exactly as the server does, so a conflicting or broken fragment fails CI.
func TestRealFragmentsMerge(t *testing.T) {
	mods := []string{"auth", "mail", "admin", "files", "media", "tv", "music", "notes", "vault", "browser", "sheets", "kanban", "cast", "system"}
	var frags []Fragment
	for _, m := range mods {
		b, err := os.ReadFile(filepath.Join("..", m, "openapi.json"))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		frags = append(frags, Fragment{Name: m, JSON: b})
	}
	out, err := Build(Base(), "test", frags)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := UnresolvedRefs(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) > 0 {
		t.Errorf("unresolved $refs in merged OpenAPI: %v", missing)
	}
}

func TestVendoredChecksums(t *testing.T) {
	f, err := swaggerFS.Open("swagger-ui/SHA256SUMS")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		b, err := fs.ReadFile(swaggerFS, "swagger-ui/"+name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != fields[0] {
			t.Errorf("%s: checksum mismatch (vendored file modified?)", name)
		}
		n++
	}
	if n < 3 {
		t.Fatalf("only %d checksums verified", n)
	}
}

func TestServe(t *testing.T) {
	doc, err := Build([]byte(testBase), "1.2.3", nil)
	if err != nil {
		t.Fatal(err)
	}
	r := httpx.NewRouter(httpx.Options{RateLimit: httpx.RateLimitOptions{Disabled: true}})
	Register(r, doc)
	get := func(path string, hdr ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		if len(hdr) == 2 {
			req.Header.Set(hdr[0], hdr[1])
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	w := get("/api/openapi.json")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"version":"1.2.3"`) || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("openapi: %d %s", w.Code, w.Header())
	}
	if w2 := get("/api/openapi.json", "If-None-Match", w.Header().Get("ETag")); w2.Code != 304 {
		t.Fatalf("etag: %d", w2.Code)
	}
	for _, p := range []string{"/api/docs", "/api/docs/"} {
		w = get(p)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `src="/api/docs/init.js"`) || strings.Contains(w.Body.String(), "<script>") {
			t.Fatalf("%s: %d", p, w.Code)
		}
		if w.Header().Get("Content-Security-Policy") != DocsCSP {
			t.Fatalf("%s CSP = %q", p, w.Header().Get("Content-Security-Policy"))
		}
	}
	want := map[string]string{
		"/api/docs/init.js":              "text/javascript; charset=utf-8",
		"/api/docs/swagger-ui-bundle.js": "text/javascript; charset=utf-8",
		"/api/docs/swagger-ui.css":       "text/css; charset=utf-8",
		"/api/docs/LICENSE":              "text/plain; charset=utf-8",
	}
	for p, ct := range want {
		if w := get(p); w.Code != 200 || w.Header().Get("Content-Type") != ct {
			t.Errorf("%s: %d %q", p, w.Code, w.Header().Get("Content-Type"))
		}
	}
	if w := get("/api/docs/index.js"); w.Code != http.StatusNotFound {
		t.Errorf("unknown docs file: %d", w.Code)
	}
	init := get("/api/docs/init.js").Body.String()
	if !strings.Contains(init, `url: "/api/openapi.json"`) {
		t.Fatal("init.js misconfigured")
	}
	// Tokens typed into "Authorize" must not be written to localStorage on the web UI's
	// origin (they would outlive sign-out); leftovers from older versions are removed.
	if strings.Contains(init, "persistAuthorization: true") || !strings.Contains(init, "persistAuthorization: false") ||
		!strings.Contains(init, `localStorage.removeItem("authorized")`) ||
		strings.Index(init, `removeItem("authorized")`) > strings.Index(init, "SwaggerUIBundle(") {
		t.Fatal("init.js persists Swagger authorization or does not purge it before Swagger UI starts")
	}
}
