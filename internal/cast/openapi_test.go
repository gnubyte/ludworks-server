package cast

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestRoutesMatchOpenAPI keeps the registered routes and the OpenAPI fragment in sync, and
// checks that every operation declares its scope.
func TestRoutesMatchOpenAPI(t *testing.T) {
	src, err := os.ReadFile("module.go")
	if err != nil {
		t.Fatal(err)
	}
	var routes []string
	for _, m := range regexp.MustCompile(`r\.Handle\("([A-Z]+) (/api/[^"]+|"\+mediaRoute)`).FindAllStringSubmatch(string(src), -1) {
		path := m[2]
		if path == `"+mediaRoute` {
			path = mediaRoute
		}
		// The ticket wildcard is named "…token" so the access log redacts it.
		routes = append(routes, m[1]+" "+strings.Replace(path, "{ticket_token}", "{ticket}", 1))
	}
	var doc struct {
		Paths map[string]map[string]struct {
			Scope string `json:"x-scope"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(openAPI, &doc); err != nil {
		t.Fatal(err)
	}
	var documented []string
	for p, ops := range doc.Paths {
		for m, op := range ops {
			documented = append(documented, strings.ToUpper(m)+" "+p)
			if op.Scope == "" {
				t.Errorf("%s %s has no x-scope", m, p)
			}
		}
	}
	slices.Sort(routes)
	slices.Sort(documented)
	if len(routes) != 13 || !slices.Equal(routes, documented) {
		t.Errorf("routes %v\ndocumented %v", routes, documented)
	}
}
