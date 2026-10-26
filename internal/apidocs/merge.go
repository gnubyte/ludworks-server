// Package apidocs builds the merged OpenAPI document (base.json + every module's fragment)
// served at /api/openapi.json, and serves the vendored Swagger UI at /api/docs.
package apidocs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// Fragment is one module's OpenAPI contribution:
// {"tags":[...],"paths":{...},"components":{"schemas":{...},...}}.
type Fragment struct {
	Name string // module name, for error messages
	JSON []byte // may be nil/empty
}

var httpMethods = map[string]bool{
	"get": true, "put": true, "post": true, "delete": true, "options": true, "head": true, "patch": true, "trace": true,
}

// Top-level fragment keys that are merged; "openapi", "info", "servers" and "security" are
// accepted but ignored (owned by base.json), as are "x-" extensions.
var ignoredTopLevel = map[string]bool{"openapi": true, "info": true, "servers": true, "security": true}

// Build deep-merges the fragments into base and sets info.version. Paths merge per
// operation; a path+method defined twice is an error, as is a component (schema, parameter,
// response, ...) defined twice with different content. Identical duplicates are fine. Tags
// merge by name (first description wins). Output is deterministic (sorted keys).
func Build(base []byte, version string, frags []Fragment) ([]byte, error) {
	doc, err := decodeObject(base)
	if err != nil {
		return nil, fmt.Errorf("apidocs: base.json: %w", err)
	}
	info, _ := doc["info"].(map[string]any)
	if info == nil {
		info = map[string]any{}
		doc["info"] = info
	}
	info["version"] = version
	paths := childMap(doc, "paths")
	comps := childMap(doc, "components")
	owner := map[string]string{} // "METHOD path" -> fragment name
	for p, item := range paths {
		if m, ok := item.(map[string]any); ok {
			for k := range m {
				if httpMethods[k] {
					owner[strings.ToUpper(k)+" "+p] = "base"
				}
			}
		}
	}
	compOwner := map[string]string{}
	for section, entries := range comps {
		if m, ok := entries.(map[string]any); ok {
			for name := range m {
				compOwner[section+"/"+name] = "base"
			}
		}
	}
	tagIndex := map[string]bool{}
	tags, _ := doc["tags"].([]any)
	for _, t := range tags {
		if m, ok := t.(map[string]any); ok {
			if n, _ := m["name"].(string); n != "" {
				tagIndex[n] = true
			}
		}
	}

	var errs []string
	for _, f := range frags {
		if len(bytes.TrimSpace(f.JSON)) == 0 {
			continue
		}
		fd, err := decodeObject(f.JSON)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: invalid JSON: %v", f.Name, err))
			continue
		}
		for k := range fd {
			if k != "paths" && k != "components" && k != "tags" && !ignoredTopLevel[k] && !strings.HasPrefix(k, "x-") {
				errs = append(errs, fmt.Sprintf("%s: unsupported top-level key %q", f.Name, k))
			}
		}
		if fp, ok := fd["paths"]; ok {
			fpm, ok := fp.(map[string]any)
			if !ok {
				errs = append(errs, f.Name+": paths must be an object")
			}
			for _, p := range sortedKeys(fpm) {
				item, ok := fpm[p].(map[string]any)
				if !ok {
					errs = append(errs, fmt.Sprintf("%s: path %s must be an object", f.Name, p))
					continue
				}
				dst := childMap(paths, p)
				for _, k := range sortedKeys(item) {
					v := item[k]
					if httpMethods[k] {
						key := strings.ToUpper(k) + " " + p
						if prev, dup := owner[key]; dup {
							errs = append(errs, fmt.Sprintf("duplicate operation %s (in %s and %s)", key, prev, f.Name))
							continue
						}
						owner[key] = f.Name
						dst[k] = v
						continue
					}
					if prev, exists := dst[k]; exists && !reflect.DeepEqual(prev, v) {
						errs = append(errs, fmt.Sprintf("conflicting %q on path %s (%s)", k, p, f.Name))
						continue
					}
					dst[k] = v
				}
			}
		}
		if fc, ok := fd["components"]; ok {
			fcm, ok := fc.(map[string]any)
			if !ok {
				errs = append(errs, f.Name+": components must be an object")
			}
			for _, section := range sortedKeys(fcm) {
				entries, ok := fcm[section].(map[string]any)
				if !ok {
					errs = append(errs, fmt.Sprintf("%s: components.%s must be an object", f.Name, section))
					continue
				}
				dst := childMap(comps, section)
				for _, name := range sortedKeys(entries) {
					def := entries[name]
					key := section + "/" + name
					if prev, exists := dst[name]; exists {
						if !reflect.DeepEqual(prev, def) {
							errs = append(errs, fmt.Sprintf("conflicting components.%s.%s (in %s and %s)", section, name, compOwner[key], f.Name))
						}
						continue
					}
					compOwner[key] = f.Name
					dst[name] = def
				}
			}
		}
		if ft, ok := fd["tags"].([]any); ok {
			for _, t := range ft {
				m, ok := t.(map[string]any)
				name, _ := m["name"].(string)
				if !ok || name == "" {
					errs = append(errs, f.Name+": tag without name")
					continue
				}
				if !tagIndex[name] {
					tagIndex[name] = true
					tags = append(tags, m)
				}
			}
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("apidocs: merge failed:\n  %s", strings.Join(errs, "\n  "))
	}
	if tags != nil {
		doc["tags"] = tags
	}
	return json.Marshal(doc)
}

// UnresolvedRefs lists local "$ref" targets ("#/components/...") that do not exist in doc.
func UnresolvedRefs(doc []byte) ([]string, error) {
	root, err := decodeObject(doc)
	if err != nil {
		return nil, err
	}
	missing := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			if ref, ok := t["$ref"].(string); ok && strings.HasPrefix(ref, "#/") && !resolve(root, ref) {
				missing[ref] = true
			}
			for _, c := range t {
				walk(c)
			}
		case []any:
			for _, c := range t {
				walk(c)
			}
		}
	}
	walk(root)
	out := make([]string, 0, len(missing))
	for r := range missing {
		out = append(out, r)
	}
	sort.Strings(out)
	return out, nil
}

func resolve(root map[string]any, ref string) bool {
	var cur any = root
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		if cur, ok = m[part]; !ok {
			return false
		}
	}
	return true
}

func decodeObject(b []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber() // keep numeric literals exact
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("not a JSON object")
	}
	return m, nil
}

func childMap(parent map[string]any, key string) map[string]any {
	if m, ok := parent[key].(map[string]any); ok {
		return m
	}
	m := map[string]any{}
	parent[key] = m
	return m
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
