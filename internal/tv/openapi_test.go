package tv

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestRoutesMatchOpenAPI keeps the registered routes and the OpenAPI fragment in sync.
func TestRoutesMatchOpenAPI(t *testing.T) {
	src, err := os.ReadFile("module.go")
	if err != nil {
		t.Fatal(err)
	}
	var routes []string
	for _, m := range regexp.MustCompile(`r\.Handle\("([A-Z]+) (/api/[^"]+)"`).FindAllStringSubmatch(string(src), -1) {
		// ServeMux cannot express "{index}.vtt"; the handler parses "{name}".
		routes = append(routes, m[1]+" "+strings.Replace(m[2], "/subtitles/{name}", "/subtitles/{index}.vtt", 1))
	}
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(openAPI, &doc); err != nil {
		t.Fatal(err)
	}
	var documented []string
	for p, ops := range doc.Paths {
		for m := range ops {
			if m != "parameters" {
				documented = append(documented, strings.ToUpper(m)+" "+p)
			}
		}
	}
	slices.Sort(routes)
	slices.Sort(documented)
	if !slices.Equal(routes, documented) {
		for _, r := range routes {
			if !slices.Contains(documented, r) {
				t.Errorf("route not documented: %s", r)
			}
		}
		for _, d := range documented {
			if !slices.Contains(routes, d) {
				t.Errorf("documented but not routed: %s", d)
			}
		}
	}
}
