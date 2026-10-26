package files

import (
	"fmt"
	"path"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"

	"ludworks/internal/core"
)

const (
	maxNameBytes = 255
	maxPathBytes = 4096
	maxDepth     = 128
)

// windowsReserved are device names Windows refuses as file names, with or without an
// extension (ARCHITECTURE.md §2 item 10).
var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true, "CONIN$": true, "CONOUT$": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"COM¹": true, "COM²": true, "COM³": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
	"LPT¹": true, "LPT²": true, "LPT³": true,
}

// windowsForbidden are characters NTFS cannot store. They are only rejected when the server
// itself runs on Windows (Linux file systems store them fine and media libraries often use
// "Title: Subtitle" names).
const windowsForbidden = `<>:"|?*`

// checkName validates a single file or folder name and returns a human-readable problem or "".
func checkName(name string) string {
	switch {
	case name == "":
		return "name must not be empty"
	case len(name) > maxNameBytes:
		return fmt.Sprintf("name must be at most %d bytes", maxNameBytes)
	case !utf8.ValidString(name):
		return "name must be valid UTF-8"
	case name == "." || name == "..":
		return "name must not be . or .."
	case strings.ContainsAny(name, "/\\\x00"):
		return `name must not contain / \ or NUL`
	case strings.ContainsFunc(name, func(r rune) bool { return unicode.IsControl(r) || r == utf8.RuneError || r == 0x2028 || r == 0x2029 }):
		return "name must not contain control characters"
	case strings.HasSuffix(name, ".") || strings.HasSuffix(name, " "):
		return "name must not end with a dot or a space"
	}
	base := name
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	if windowsReserved[strings.ToUpper(strings.TrimRight(base, " "))] {
		return "name is a reserved device name"
	}
	if runtime.GOOS == "windows" && strings.ContainsAny(name, windowsForbidden) {
		return `name must not contain any of < > : " | ? *`
	}
	return ""
}

// validName returns a 422 validation error for an invalid name.
func validName(field, name string) error {
	if msg := checkName(name); msg != "" {
		return core.Validation(map[string]string{field: msg})
	}
	return nil
}

// cleanPath normalises a client-supplied space-relative directory or file path: surrounding
// slashes are trimmed, "" is the space root, and every component must be a valid name.
// Backslashes are rejected (never treated as separators).
func cleanPath(field, p string) (string, error) {
	if len(p) > maxPathBytes {
		return "", core.Validation(map[string]string{field: "path is too long"})
	}
	p = strings.Trim(p, "/")
	if p == "" {
		return "", nil
	}
	parts := strings.Split(p, "/")
	if len(parts) > maxDepth {
		return "", core.Validation(map[string]string{field: "path is too deep"})
	}
	for _, c := range parts {
		if msg := checkName(c); msg != "" {
			return "", core.Validation(map[string]string{field: "invalid path component: " + msg})
		}
	}
	return p, nil
}

// joinPath joins a directory path and a name ("" is the root).
func joinPath(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// splitPath returns the parent directory path and the base name of p.
func splitPath(p string) (dir, name string) {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return "", p
	}
	return p[:i], p[i+1:]
}

// isWithin reports whether p equals dir or lies below it.
func isWithin(p, dir string) bool {
	if dir == "" {
		return true
	}
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// rebase replaces the prefix oldDir of p with newDir (p must be within oldDir).
func rebase(p, oldDir, newDir string) string {
	if p == oldDir {
		return newDir
	}
	rest := strings.TrimPrefix(p, oldDir+"/")
	return joinPath(newDir, rest)
}

// subtreeBounds returns [lo, hi) so that every descendant path of p (excluding p) satisfies
// lo <= path < hi in BINARY collation: "p/" <= x < "p0" ('0' follows '/').
func subtreeBounds(p string) (lo, hi string) { return p + "/", p + "0" }

// doubleExts are compound extensions kept together when generating "name (1).ext" names.
var doubleExts = []string{".tar.gz", ".tar.bz2", ".tar.xz", ".tar.zst", ".tar.lz", ".tar.lzma"}

// splitExt splits a file name into stem and extension for conflict renames. Directories and
// dotfiles without another dot have no extension.
func splitExt(name string, isDir bool) (stem, ext string) {
	if isDir {
		return name, ""
	}
	lower := strings.ToLower(name)
	for _, de := range doubleExts {
		if strings.HasSuffix(lower, de) && len(name) > len(de) {
			return name[:len(name)-len(de)], name[len(name)-len(de):]
		}
	}
	ext = path.Ext(name)
	if ext == name || ext == "." || len(ext) > 16 {
		return name, ""
	}
	return name[:len(name)-len(ext)], ext
}

// numberedName returns "stem (n)ext", trimmed so the result stays within maxNameBytes.
func numberedName(name string, isDir bool, n int) string {
	stem, ext := splitExt(name, isDir)
	suffix := fmt.Sprintf(" (%d)", n)
	for len(stem)+len(suffix)+len(ext) > maxNameBytes && len(stem) > 0 {
		_, size := utf8.DecodeLastRuneInString(stem)
		stem = stem[:len(stem)-size]
	}
	stem = strings.TrimRight(stem, " .")
	if stem == "" {
		stem = "file"
	}
	return stem + suffix + ext
}

// normalizeTags trims, de-duplicates (case-insensitively, first spelling wins) and validates
// tags. At most 100 tags of 1..64 characters without control characters.
func normalizeTags(field string, in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if utf8.RuneCountInString(t) > 64 || !utf8.ValidString(t) || strings.ContainsFunc(t, unicode.IsControl) {
			return nil, core.Validation(map[string]string{field: "tags must be 1-64 characters without control characters"})
		}
		k := strings.ToLower(t)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, t)
	}
	if len(out) > 100 {
		return nil, core.Validation(map[string]string{field: "at most 100 tags"})
	}
	return out, nil
}
