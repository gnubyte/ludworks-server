package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Spec is the JSON package description.
type Spec struct {
	// Control is the path of the control file template (relative to the spec).
	Control string `json:"control"`
	// Files are the regular files installed by the package.
	Files []FileSpec `json:"files"`
	// Dirs are directories with an explicit mode (parents of files are added as 0755).
	Dirs []DirSpec `json:"dirs"`
	// Scripts maps maintainer script names (preinst, postinst, prerm, postrm) to files.
	Scripts map[string]string `json:"scripts"`
}

// FileSpec is one installed regular file.
type FileSpec struct {
	Src      string `json:"src"`
	Dst      string `json:"dst"`
	Mode     string `json:"mode"`
	Conffile bool   `json:"conffile"`
}

// DirSpec is one directory with an explicit mode.
type DirSpec struct {
	Dst  string `json:"dst"`
	Mode string `json:"mode"`
}

// Limits keep a malformed spec from producing something absurd.
const (
	maxSpecBytes    = 1 << 20
	maxScriptBytes  = 1 << 20
	maxControlBytes = 64 << 10
	maxFileBytes    = 1 << 30
	maxEntries      = 10000
)

var (
	varNameRe     = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	pkgNameRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]+$`)
	archRe        = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	upstreamRe    = regexp.MustCompile(`^[0-9][A-Za-z0-9.+~-]*$`)
	revisionRe    = regexp.MustCompile(`^[A-Za-z0-9+.~]+$`)
	fieldNameRe   = regexp.MustCompile(`^[A-Za-z0-9][!-9;-~]*$`)
	pathSegmentRe = regexp.MustCompile(`^[A-Za-z0-9_@+,=][A-Za-z0-9._@+,=-]*$`)
	maintainerRe  = regexp.MustCompile(`^[^<>]+ <[^<>@\s]+@[^<>@\s]+>$`)
	modeRe        = regexp.MustCompile(`^0?[0-7]{3}$`)
)

var scriptNames = map[string]bool{"preinst": true, "postinst": true, "prerm": true, "postrm": true}

func validVarName(s string) bool { return varNameRe.MatchString(s) }

// expand replaces ${NAME} references. Unknown names and unterminated references are errors,
// so a typo cannot silently produce an empty path or field.
func expand(s string, vars map[string]string) (string, error) {
	var b strings.Builder
	for {
		i := strings.Index(s, "${")
		if i < 0 {
			b.WriteString(s)
			return b.String(), nil
		}
		b.WriteString(s[:i])
		j := strings.IndexByte(s[i:], '}')
		if j < 0 {
			return "", fmt.Errorf("unterminated ${ in %q", s)
		}
		name := s[i+2 : i+j]
		v, ok := vars[name]
		if !validVarName(name) || !ok {
			return "", fmt.Errorf("undefined variable ${%s}", name)
		}
		b.WriteString(v)
		s = s[i+j+1:]
	}
}

// ValidateVersion checks a Debian version string: [epoch:]upstream[-revision].
func ValidateVersion(v string) error {
	if v == "" || len(v) > 128 {
		return errors.New("version must be 1-128 characters")
	}
	rest := v
	if e, after, ok := strings.Cut(v, ":"); ok {
		if _, err := strconv.ParseUint(e, 10, 32); err != nil {
			return fmt.Errorf("version %q: invalid epoch", v)
		}
		rest = after
	}
	upstream := rest
	if i := strings.LastIndexByte(rest, '-'); i >= 0 {
		upstream = rest[:i]
		if !revisionRe.MatchString(rest[i+1:]) {
			return fmt.Errorf("version %q: invalid Debian revision", v)
		}
	}
	if !upstreamRe.MatchString(upstream) || strings.Contains(upstream, ":") {
		return fmt.Errorf("version %q: upstream version must start with a digit and use only [A-Za-z0-9.+~-]", v)
	}
	return nil
}

// parseMode parses an octal permission string such as "0755". Only permission bits are
// allowed: setuid, setgid and sticky bits are refused for a security-first package.
func parseMode(s string, dir bool) (uint32, error) {
	if s == "" {
		if dir {
			return 0o755, nil
		}
		return 0, errors.New("mode is required")
	}
	if !modeRe.MatchString(s) {
		return 0, fmt.Errorf("mode %q: expected an octal permission like 0644", s)
	}
	m, _ := strconv.ParseUint(s, 8, 32)
	if m&0o400 == 0 {
		return 0, fmt.Errorf("mode %q: must be readable by its owner", s)
	}
	if dir && m&0o100 == 0 {
		return 0, fmt.Errorf("mode %q: a directory must be searchable by its owner", s)
	}
	if m&0o002 != 0 {
		return 0, fmt.Errorf("mode %q: world-writable entries are not allowed", s)
	}
	return uint32(m), nil
}

// cleanDst validates an absolute install path and returns it without the leading slash.
func cleanDst(p string) (string, error) {
	if !strings.HasPrefix(p, "/") || p == "/" || strings.HasSuffix(p, "/") || path.Clean(p) != p {
		return "", fmt.Errorf("install path %q must be absolute and clean (no trailing slash, //, . or ..)", p)
	}
	if len(p) > 1024 {
		return "", fmt.Errorf("install path %q is too long", p)
	}
	rel := p[1:]
	for _, seg := range strings.Split(rel, "/") {
		if !pathSegmentRe.MatchString(seg) || seg == "." || seg == ".." {
			return "", fmt.Errorf("install path %q: segment %q uses characters outside [A-Za-z0-9._@+,=-]", p, seg)
		}
	}
	return rel, nil
}

// LoadSpec reads the spec and every file it references and returns the assembled package.
func LoadSpec(specPath string, vars map[string]string, mtime time.Time) (*Package, error) {
	raw, err := readLimited(specPath, maxSpecBytes)
	if err != nil {
		return nil, err
	}
	var spec Spec
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return nil, fmt.Errorf("spec %s: %w", specPath, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("spec %s: trailing data after the JSON object", specPath)
	}
	base := filepath.Dir(specPath)
	resolve := func(p string) (string, error) {
		p, err := expand(p, vars)
		if err != nil {
			return "", err
		}
		if p == "" {
			return "", errors.New("empty source path")
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(base, filepath.FromSlash(p))
		}
		return p, nil
	}

	if spec.Control == "" {
		return nil, fmt.Errorf("spec %s: \"control\" is required", specPath)
	}
	ctlPath, err := resolve(spec.Control)
	if err != nil {
		return nil, fmt.Errorf("control: %w", err)
	}
	tmpl, err := readLimited(ctlPath, maxControlBytes)
	if err != nil {
		return nil, err
	}
	ctlText, err := expand(string(tmpl), vars)
	if err != nil {
		return nil, fmt.Errorf("control %s: %w", ctlPath, err)
	}
	ctl, err := ParseControl(ctlText)
	if err != nil {
		return nil, fmt.Errorf("control %s: %w", ctlPath, err)
	}

	pkg := &Package{MTime: mtime, control: ctl, Scripts: map[string][]byte{}}
	for _, f := range spec.Files {
		rel, err := cleanDst(f.Dst)
		if err != nil {
			return nil, err
		}
		mode, err := parseMode(f.Mode, false)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Dst, err)
		}
		src, err := resolve(f.Src)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Dst, err)
		}
		data, err := readRegular(src, maxFileBytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Dst, err)
		}
		if f.Conffile && !strings.HasPrefix(rel, "etc/") {
			return nil, fmt.Errorf("conffile %s must live under /etc", f.Dst)
		}
		pkg.Files = append(pkg.Files, File{Path: rel, Mode: mode, Data: data, Conffile: f.Conffile})
	}
	for _, d := range spec.Dirs {
		rel, err := cleanDst(d.Dst)
		if err != nil {
			return nil, err
		}
		mode, err := parseMode(d.Mode, true)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", d.Dst, err)
		}
		pkg.Dirs = append(pkg.Dirs, Dir{Path: rel, Mode: mode})
	}
	names := make([]string, 0, len(spec.Scripts))
	for name := range spec.Scripts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !scriptNames[name] {
			return nil, fmt.Errorf("scripts: unsupported maintainer script %q (preinst, postinst, prerm, postrm)", name)
		}
		src, err := resolve(spec.Scripts[name])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		data, err := readRegular(src, maxScriptBytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		pkg.Scripts[name] = data
	}
	if err := pkg.finalize(); err != nil {
		return nil, err
	}
	return pkg, nil
}

func readLimited(p string, limit int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s: larger than %d bytes", p, limit)
	}
	return data, nil
}

// readRegular reads a regular file; symlinks and special files are refused so the package
// contents are exactly what the spec names.
func readRegular(p string, limit int64) ([]byte, error) {
	st, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", p)
	}
	return readLimited(p, limit)
}

// Field is one control field. Value holds the first line; Continuation the following lines
// without their leading space.
type Field struct {
	Name         string
	Value        string
	Continuation []string
}

// Control is a parsed control paragraph that keeps the field order.
type Control struct{ Fields []Field }

// Get returns a field's first-line value.
func (c *Control) Get(name string) (string, bool) {
	for _, f := range c.Fields {
		if strings.EqualFold(f.Name, name) {
			return f.Value, true
		}
	}
	return "", false
}

// ParseControl parses and validates a single binary-package control paragraph.
func ParseControl(text string) (*Control, error) {
	if strings.ContainsRune(text, '\r') {
		return nil, errors.New("contains carriage returns (use LF line endings)")
	}
	c := &Control{}
	seen := map[string]bool{}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, line := range lines {
		for _, r := range line {
			if r < 0x20 && r != '\t' || r == 0x7f {
				return nil, fmt.Errorf("line %d: control character", i+1)
			}
		}
		switch {
		case line == "" || strings.TrimSpace(line) == "":
			return nil, fmt.Errorf("line %d: empty line (a control file holds one paragraph; use \" .\" for a blank description line)", i+1)
		case line[0] == ' ' || line[0] == '\t':
			if len(c.Fields) == 0 {
				return nil, fmt.Errorf("line %d: continuation line before the first field", i+1)
			}
			f := &c.Fields[len(c.Fields)-1]
			f.Continuation = append(f.Continuation, line[1:])
		case line[0] == '#':
			return nil, fmt.Errorf("line %d: comments are not allowed in a binary control file", i+1)
		default:
			name, value, ok := strings.Cut(line, ":")
			if !ok || !fieldNameRe.MatchString(name) {
				return nil, fmt.Errorf("line %d: expected \"Field: value\"", i+1)
			}
			key := strings.ToLower(name)
			if seen[key] {
				return nil, fmt.Errorf("line %d: duplicate field %s", i+1, name)
			}
			seen[key] = true
			c.Fields = append(c.Fields, Field{Name: name, Value: strings.TrimSpace(value)})
		}
	}
	for _, req := range []string{"Package", "Version", "Architecture", "Maintainer", "Description"} {
		if v, ok := c.Get(req); !ok || v == "" {
			return nil, fmt.Errorf("required field %s is missing or empty", req)
		}
	}
	if _, ok := c.Get("Installed-Size"); ok {
		return nil, errors.New("Installed-Size is computed by mkdeb; remove it from the template")
	}
	name, _ := c.Get("Package")
	if !pkgNameRe.MatchString(name) {
		return nil, fmt.Errorf("Package %q: invalid package name", name)
	}
	ver, _ := c.Get("Version")
	if err := ValidateVersion(ver); err != nil {
		return nil, err
	}
	arch, _ := c.Get("Architecture")
	if !archRe.MatchString(arch) {
		return nil, fmt.Errorf("Architecture %q: invalid", arch)
	}
	maint, _ := c.Get("Maintainer")
	if !maintainerRe.MatchString(maint) {
		return nil, fmt.Errorf("Maintainer %q: expected \"Name <email>\"", maint)
	}
	for _, f := range c.Fields {
		if len(f.Continuation) > 0 && !strings.EqualFold(f.Name, "Description") &&
			!strings.EqualFold(f.Name, "Depends") && !strings.EqualFold(f.Name, "Recommends") &&
			!strings.EqualFold(f.Name, "Suggests") && !strings.EqualFold(f.Name, "Conffiles") {
			return nil, fmt.Errorf("field %s must be a single line", f.Name)
		}
		if f.Value == "" && len(f.Continuation) == 0 {
			return nil, fmt.Errorf("field %s is empty", f.Name)
		}
	}
	return c, nil
}

// String renders the paragraph in canonical form.
func (c *Control) String() string {
	var b strings.Builder
	for _, f := range c.Fields {
		b.WriteString(f.Name)
		b.WriteString(": ")
		b.WriteString(f.Value)
		b.WriteByte('\n')
		for _, l := range f.Continuation {
			b.WriteByte(' ')
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// insertAfter adds a field after the first existing field in after (or at the end).
func (c *Control) insertAfter(f Field, after ...string) {
	for _, a := range after {
		for i := range c.Fields {
			if strings.EqualFold(c.Fields[i].Name, a) {
				c.Fields = append(c.Fields[:i+1], append([]Field{f}, c.Fields[i+1:]...)...)
				return
			}
		}
	}
	c.Fields = append(c.Fields, f)
}
