package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

var epoch = time.Unix(1767225600, 0).UTC() // 2026-01-01T00:00:00Z

const testControl = `Package: demo
Version: ${VERSION}
Architecture: ${ARCH}
Maintainer: Demo Team <demo@example.invalid>
Depends: adduser
Section: net
Priority: optional
Description: demo package for mkdeb tests
 A longer description over
 two lines.
 .
 And a second paragraph.
`

// fixture writes a spec directory and returns the spec path.
func fixture(t *testing.T, mutate func(dir string, files map[string]string)) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"control":     testControl,
		"bin/demo":    "\x7fELF fake binary " + strings.Repeat("x", 3000),
		"demo.conf":   "{\"listen\": \":8443\"}\n",
		"unit":        "[Service]\nExecStart=/usr/bin/demo\n",
		"postinst":    "#!/bin/sh\nset -e\necho configured\n",
		"prerm":       "#!/bin/sh\nset -e\n",
		"mkdeb.json":  "",
		"odd-size.md": "abc", // odd length exercises tar/ar padding
	}
	files["mkdeb.json"] = `{
  "control": "control",
  "files": [
    {"src": "${BINARY}", "dst": "/usr/bin/demo", "mode": "0755"},
    {"src": "demo.conf", "dst": "/etc/demo/demo.conf", "mode": "0640", "conffile": true},
    {"src": "unit", "dst": "/usr/lib/systemd/system/demo.service", "mode": "0644"},
    {"src": "odd-size.md", "dst": "/usr/share/doc/demo/README.md", "mode": "644"}
  ],
  "dirs": [{"dst": "/etc/demo", "mode": "0750"}, {"dst": "/var/lib/demo"}],
  "scripts": {"postinst": "postinst", "prerm": "prerm"}
}`
	if mutate != nil {
		mutate(dir, files)
	}
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "mkdeb.json")
}

func vars(spec string) map[string]string {
	return map[string]string{
		"VERSION": "1.2.3~rc.1",
		"ARCH":    "amd64",
		"BINARY":  filepath.Join(filepath.Dir(spec), "bin", "demo"),
	}
}

func buildFixture(t *testing.T, spec string, mtime time.Time) []byte {
	t.Helper()
	pkg, err := LoadSpec(spec, vars(spec), mtime)
	if err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	b, err := pkg.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return b
}

func TestBuildLayout(t *testing.T) {
	spec := fixture(t, nil)
	b := buildFixture(t, spec, epoch)

	members, err := readAr(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 3 {
		t.Fatalf("got %d ar members", len(members))
	}
	for i, want := range []string{"debian-binary", "control.tar.gz", "data.tar.gz"} {
		m := members[i]
		if m.Name != want || m.UID != 0 || m.GID != 0 || m.Mode != 0o100644 || m.MTime != epoch.Unix() {
			t.Errorf("member %d = %+v (want %s, root, 100644, epoch)", i, m, want)
		}
	}
	// Raw header check: dpkg-deb style names without a "/" terminator.
	if !bytes.HasPrefix(b, []byte("!<arch>\ndebian-binary   "+strconv.FormatInt(epoch.Unix(), 10))) {
		t.Errorf("unexpected archive start %q", b[:40])
	}

	deb, err := ReadDeb(b)
	if err != nil {
		t.Fatal(err)
	}
	ctl := deb.ControlFile("control")
	wantCtl := "Package: demo\nVersion: 1.2.3~rc.1\nArchitecture: amd64\nMaintainer: Demo Team <demo@example.invalid>\n" +
		"Installed-Size: 19\nDepends: adduser\nSection: net\nPriority: optional\n" +
		"Description: demo package for mkdeb tests\n A longer description over\n two lines.\n .\n And a second paragraph.\n"
	if got := string(ctl.Data); got != wantCtl {
		t.Errorf("control:\n%s\nwant:\n%s", got, wantCtl)
	}

	// Installed-Size = ceil per file + 1 per directory.
	var kib int64
	dirs := 0
	for _, e := range deb.DataFS {
		switch e.Type {
		case tar.TypeReg:
			kib += (int64(len(e.Data)) + 1023) / 1024
		case tar.TypeDir:
			if e.Name != "./" {
				dirs++
			}
		}
	}
	if got := kib + int64(dirs); got != 19 {
		t.Errorf("recomputed Installed-Size = %d, want 19 as written into control", got)
	}

	// control.tar: exact names, modes and ownership.
	wantCtlFS := map[string]uint32{"./": 0o755, "./control": 0o644, "./md5sums": 0o644, "./conffiles": 0o644,
		"./postinst": 0o755, "./prerm": 0o755}
	if len(deb.ControlFS) != len(wantCtlFS) {
		t.Errorf("control.tar has %d entries, want %d", len(deb.ControlFS), len(wantCtlFS))
	}
	for _, e := range deb.ControlFS {
		mode, ok := wantCtlFS[e.Name]
		if !ok || e.Mode != mode {
			t.Errorf("control.tar entry %s mode %o (want %o, known=%v)", e.Name, e.Mode, mode, ok)
		}
		checkOwner(t, e)
	}
	if deb.ControlFS[0].Name != "./" {
		t.Errorf("first control entry is %s", deb.ControlFS[0].Name)
	}
	if got := string(deb.ControlFile("conffiles").Data); got != "/etc/demo/demo.conf\n" {
		t.Errorf("conffiles = %q", got)
	}

	// data.tar: parents before children, explicit dir modes kept, implicit parents 0755.
	wantData := []struct {
		name string
		mode uint32
		dir  bool
	}{
		{"./", 0o755, true},
		{"./etc/", 0o755, true},
		{"./etc/demo/", 0o750, true},
		{"./etc/demo/demo.conf", 0o640, false},
		{"./usr/", 0o755, true},
		{"./usr/bin/", 0o755, true},
		{"./usr/bin/demo", 0o755, false},
		{"./usr/lib/", 0o755, true},
		{"./usr/lib/systemd/", 0o755, true},
		{"./usr/lib/systemd/system/", 0o755, true},
		{"./usr/lib/systemd/system/demo.service", 0o644, false},
		{"./usr/share/", 0o755, true},
		{"./usr/share/doc/", 0o755, true},
		{"./usr/share/doc/demo/", 0o755, true},
		{"./usr/share/doc/demo/README.md", 0o644, false},
		{"./var/", 0o755, true},
		{"./var/lib/", 0o755, true},
		{"./var/lib/demo/", 0o755, true},
	}
	if len(deb.DataFS) != len(wantData) {
		var names []string
		for _, e := range deb.DataFS {
			names = append(names, e.Name)
		}
		t.Fatalf("data.tar entries:\n%s", strings.Join(names, "\n"))
	}
	for i, w := range wantData {
		e := deb.DataFS[i]
		isDir := e.Type == tar.TypeDir
		if e.Name != w.name || e.Mode != w.mode || isDir != w.dir {
			t.Errorf("data[%d] = %s %o dir=%v, want %s %o dir=%v", i, e.Name, e.Mode, isDir, w.name, w.mode, w.dir)
		}
		checkOwner(t, e)
	}
	if got := string(deb.DataFile("/usr/share/doc/demo/README.md").Data); got != "abc" {
		t.Errorf("README content %q", got)
	}

	// md5sums: every regular file except the conffile.
	if len(deb.MD5Sums) != 3 {
		t.Errorf("md5sums has %d entries: %v", len(deb.MD5Sums), deb.MD5Sums)
	}
	if _, ok := deb.MD5Sums["etc/demo/demo.conf"]; ok {
		t.Error("conffile listed in md5sums")
	}
	bin := deb.DataFile("/usr/bin/demo")
	sum := md5.Sum(bin.Data)
	if deb.MD5Sums["usr/bin/demo"] != hex.EncodeToString(sum[:]) {
		t.Error("md5sum of /usr/bin/demo is wrong")
	}
	if err := deb.VerifyMD5Sums(); err != nil {
		t.Error(err)
	}

	// gzip members carry no name and no timestamp.
	for _, m := range members[1:] {
		zr, err := gzip.NewReader(bytes.NewReader(m.Data))
		if err != nil {
			t.Fatal(err)
		}
		if zr.Name != "" || !zr.ModTime.IsZero() {
			t.Errorf("%s: gzip header name=%q mtime=%v", m.Name, zr.Name, zr.ModTime)
		}
	}
}

func checkOwner(t *testing.T, e TarEntry) {
	t.Helper()
	if e.UID != 0 || e.GID != 0 || e.Uname != "root" || e.Gname != "root" {
		t.Errorf("%s owned by %d/%d %s/%s, want root:root", e.Name, e.UID, e.GID, e.Uname, e.Gname)
	}
	if !e.ModTime.Equal(epoch) {
		t.Errorf("%s mtime %v, want %v", e.Name, e.ModTime, epoch)
	}
}

func TestDeterministic(t *testing.T) {
	spec := fixture(t, nil)
	a := buildFixture(t, spec, epoch)
	b := buildFixture(t, spec, epoch)
	if !bytes.Equal(a, b) {
		t.Fatal("two builds of the same inputs differ")
	}
	c := buildFixture(t, spec, epoch.Add(time.Hour))
	if bytes.Equal(a, c) {
		t.Fatal("a different SOURCE_DATE_EPOCH produced the same bytes")
	}
	// Host file modes and timestamps must not leak into the package.
	if runtime.GOOS != "windows" {
		bin := filepath.Join(filepath.Dir(spec), "bin", "demo")
		if err := os.Chmod(bin, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(bin, time.Now(), time.Now().Add(-48*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if d := buildFixture(t, spec, epoch); !bytes.Equal(a, d) {
			t.Fatal("host file mode/mtime changed the package")
		}
	}
}

func TestValidation(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(dir string, f map[string]string)
		vars    map[string]string
		wantErr string
	}{
		{"bad package name", func(_ string, f map[string]string) {
			f["control"] = strings.Replace(f["control"], "Package: demo", "Package: Demo_Pkg", 1)
		}, nil, "invalid package name"},
		{"bad version", nil, map[string]string{"VERSION": "v1.0"}, "upstream version must start with a digit"},
		{"bad revision", nil, map[string]string{"VERSION": "1.0-r_1"}, "invalid Debian revision"},
		{"bad arch", nil, map[string]string{"ARCH": "AMD64"}, "Architecture"},
		{"missing description", func(_ string, f map[string]string) {
			f["control"] = f["control"][:strings.Index(f["control"], "Description:")]
		}, nil, "Description is missing"},
		{"blank line in description", func(_ string, f map[string]string) {
			f["control"] = strings.Replace(f["control"], " .\n", "\n", 1)
		}, nil, "empty line"},
		{"installed-size in template", func(_ string, f map[string]string) {
			f["control"] = strings.Replace(f["control"], "Section: net", "Installed-Size: 1\nSection: net", 1)
		}, nil, "Installed-Size is computed"},
		{"duplicate field", func(_ string, f map[string]string) {
			f["control"] = strings.Replace(f["control"], "Section: net", "Section: net\nsection: web", 1)
		}, nil, "duplicate field"},
		{"crlf control", func(_ string, f map[string]string) {
			f["control"] = strings.ReplaceAll(f["control"], "\n", "\r\n")
		}, nil, "carriage returns"},
		{"bad maintainer", func(_ string, f map[string]string) {
			f["control"] = strings.Replace(f["control"], "Demo Team <demo@example.invalid>", "demo", 1)
		}, nil, "Maintainer"},
		{"undefined variable", func(_ string, f map[string]string) {
			f["control"] = strings.Replace(f["control"], "Section: net", "Section: ${SECTION}", 1)
		}, nil, "undefined variable ${SECTION}"},
		{"unknown spec key", func(_ string, f map[string]string) {
			f["mkdeb.json"] = strings.Replace(f["mkdeb.json"], `"control"`, `"contrl": "x", "control"`, 1)
		}, nil, "unknown field"},
		{"crlf script", func(_ string, f map[string]string) {
			f["postinst"] = "#!/bin/sh\r\nset -e\r\n"
		}, nil, "carriage returns"},
		{"script without shebang", func(_ string, f map[string]string) {
			f["prerm"] = "set -e\n"
		}, nil, "interpreter line"},
		{"unsupported script", func(_ string, f map[string]string) {
			f["mkdeb.json"] = strings.Replace(f["mkdeb.json"], `"prerm": "prerm"`, `"config": "prerm"`, 1)
		}, nil, "unsupported maintainer script"},
		{"setuid mode", func(_ string, f map[string]string) {
			f["mkdeb.json"] = strings.Replace(f["mkdeb.json"], `"mode": "0755"`, `"mode": "4755"`, 1)
		}, nil, "octal permission"},
		{"world writable", func(_ string, f map[string]string) {
			f["mkdeb.json"] = strings.Replace(f["mkdeb.json"], `"mode": "0755"`, `"mode": "0777"`, 1)
		}, nil, "world-writable"},
		{"missing mode", func(_ string, f map[string]string) {
			f["mkdeb.json"] = strings.Replace(f["mkdeb.json"], `, "mode": "0755"`, ``, 1)
		}, nil, "mode is required"},
		{"relative dst", func(_ string, f map[string]string) {
			f["mkdeb.json"] = strings.Replace(f["mkdeb.json"], `"/usr/bin/demo"`, `"usr/bin/demo"`, 1)
		}, nil, "must be absolute"},
		{"dotdot dst", func(_ string, f map[string]string) {
			f["mkdeb.json"] = strings.Replace(f["mkdeb.json"], `"/usr/bin/demo"`, `"/usr/../etc/shadow"`, 1)
		}, nil, "must be absolute and clean"},
		{"weird dst chars", func(_ string, f map[string]string) {
			f["mkdeb.json"] = strings.Replace(f["mkdeb.json"], `"/usr/bin/demo"`, `"/usr/bin/de mo"`, 1)
		}, nil, "characters outside"},
		{"duplicate dst", func(_ string, f map[string]string) {
			f["mkdeb.json"] = strings.Replace(f["mkdeb.json"], `"/usr/lib/systemd/system/demo.service"`, `"/usr/bin/demo"`, 1)
		}, nil, "listed twice"},
		{"file under file", func(_ string, f map[string]string) {
			f["mkdeb.json"] = strings.Replace(f["mkdeb.json"], `"/usr/lib/systemd/system/demo.service"`, `"/usr/bin/demo/x"`, 1)
		}, nil, "is a file but also the parent"},
		{"conffile outside etc", func(_ string, f map[string]string) {
			f["mkdeb.json"] = strings.Replace(f["mkdeb.json"], `"/etc/demo/demo.conf"`, `"/usr/share/demo.conf"`, 1)
		}, nil, "must live under /etc"},
		{"missing source", func(_ string, f map[string]string) {
			f["mkdeb.json"] = strings.Replace(f["mkdeb.json"], `"demo.conf"`, `"nope.conf"`, 1)
		}, nil, "nope.conf"},
		{"trailing json", func(_ string, f map[string]string) {
			f["mkdeb.json"] += "{}"
		}, nil, "trailing data"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := fixture(t, tc.mutate)
			v := vars(spec)
			for k, val := range tc.vars {
				v[k] = val
			}
			_, err := LoadSpec(spec, v, epoch)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestSymlinkSourceRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	spec := fixture(t, nil)
	dir := filepath.Dir(spec)
	if err := os.Remove(filepath.Join(dir, "unit")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hostname", filepath.Join(dir, "unit")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSpec(spec, vars(spec), epoch); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("err = %v", err)
	}
}

func TestVersions(t *testing.T) {
	for _, v := range []string{"0.1.0", "1:2.0", "1.0-1", "1.0~rc.1", "1.0+git5.gabc", "2.0-1ubuntu1~22.04"} {
		if err := ValidateVersion(v); err != nil {
			t.Errorf("%s: %v", v, err)
		}
	}
	for _, v := range []string{"", "v1", "1.0-", "a:1.0", "1.0 beta", "1.0-r_1", "-1"} {
		if err := ValidateVersion(v); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
}

func TestExpand(t *testing.T) {
	vars := map[string]string{"A": "x", "B_2": "y"}
	got, err := expand("${A}/$B/${B_2}$", vars)
	if err != nil || got != "x/$B/y$" {
		t.Fatalf("got %q, %v", got, err)
	}
	for _, bad := range []string{"${A", "${a}", "${C}", "${}"} {
		if _, err := expand(bad, vars); err == nil {
			t.Errorf("%q expanded", bad)
		}
	}
}

func TestBuildTime(t *testing.T) {
	now := func() time.Time { return time.Unix(1000, 500) }
	if got, _ := buildTime("", "", now); got.Unix() != 1000 {
		t.Errorf("default = %v", got)
	}
	if got, _ := buildTime("", "42", now); got.Unix() != 42 {
		t.Errorf("env = %v", got)
	}
	if got, _ := buildTime("7", "42", now); got.Unix() != 7 {
		t.Errorf("flag = %v", got)
	}
	for _, bad := range []string{"-1", "abc", "99999999999"} {
		if _, err := buildTime("", bad, now); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestReadDebRejectsGarbage(t *testing.T) {
	spec := fixture(t, nil)
	good := buildFixture(t, spec, epoch)
	cases := map[string][]byte{
		"empty":        nil,
		"bad magic":    []byte("!<arch>x"),
		"truncated":    good[:100],
		"wrong order":  swapMembers(t, good),
		"bad checksum": corruptData(t, good),
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			deb, err := ReadDeb(b)
			if err == nil {
				err = deb.VerifyMD5Sums()
			}
			if err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func swapMembers(t *testing.T, b []byte) []byte {
	members, err := readAr(b)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	aw := newArWriter(&out, epoch)
	for _, i := range []int{0, 2, 1} {
		if err := aw.add(members[i].Name, members[i].Data); err != nil {
			t.Fatal(err)
		}
	}
	return out.Bytes()
}

// corruptData rewrites data.tar.gz with one byte of /usr/bin/demo flipped.
func corruptData(t *testing.T, b []byte) []byte {
	members, err := readAr(b)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := readTarGz(members[2])
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		data := e.Data
		if e.Name == "./usr/bin/demo" {
			data = append([]byte{}, data...)
			data[0] ^= 0xff
		}
		h := &tar.Header{Name: e.Name, Typeflag: e.Type, Mode: int64(e.Mode), Size: int64(len(data)), ModTime: e.ModTime}
		if e.Type == tar.TypeDir {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	zw.Close()
	var out bytes.Buffer
	aw := newArWriter(&out, epoch)
	_ = aw.add(members[0].Name, members[0].Data)
	_ = aw.add(members[1].Name, members[1].Data)
	_ = aw.add(members[2].Name, buf.Bytes())
	return out.Bytes()
}

func TestCLI(t *testing.T) {
	spec := fixture(t, nil)
	out := filepath.Join(t.TempDir(), "demo_1.0_amd64.deb")
	var stdout, stderr bytes.Buffer
	t.Setenv("SOURCE_DATE_EPOCH", strconv.FormatInt(epoch.Unix(), 10))
	code := run([]string{"build", "-spec", spec, "-version", "1.0", "-arch", "arm64",
		"-var", "BINARY=" + filepath.Join(filepath.Dir(spec), "bin", "demo"), "-out", out}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("build exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "demo 1.0 arm64") {
		t.Errorf("stdout %q", stdout.String())
	}
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(out); err != nil || st.Mode().Perm() != 0o644 {
			t.Errorf("output mode %v, %v", st.Mode(), err)
		}
	}
	stdout.Reset()
	if code := run([]string{"inspect", out}, &stdout, &stderr); code != 0 {
		t.Fatalf("inspect exit %d: %s", code, stderr.String())
	}
	for _, want := range []string{"Architecture: arm64", "-rwxr-xr-x root/root", "./usr/bin/demo",
		"conffiles:\n  /etc/demo/demo.conf", "md5sums: OK (3 files)", "2026-01-01 00:00"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("inspect output lacks %q:\n%s", want, stdout.String())
		}
	}
	// Usage errors.
	for _, args := range [][]string{nil, {"bogus"}, {"build"}, {"build", "-var", "lower=x"}, {"build", "-var", "VERSION=1"}, {"inspect"}} {
		if code := run(args, io.Discard, io.Discard); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

// TestRealPackagingSpec builds the actual packaging/deb spec with a stand-in binary.
func TestRealPackagingSpec(t *testing.T) {
	spec := filepath.Join("..", "..", "packaging", "deb", "mkdeb.json")
	bin := filepath.Join(t.TempDir(), "ludworks")
	if err := os.WriteFile(bin, []byte("stand-in binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	pkg, err := LoadSpec(spec, map[string]string{"VERSION": "0.1.0", "ARCH": "amd64", "BINARY": bin}, epoch)
	if err != nil {
		t.Fatal(err)
	}
	b, err := pkg.Build()
	if err != nil {
		t.Fatal(err)
	}
	deb, err := ReadDeb(b)
	if err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]string{
		"Package": "ludworks", "Version": "0.1.0", "Architecture": "amd64",
		"Maintainer": "Ludworks Project <maintainers@ludworks.invalid>", "Depends": "adduser",
		"Recommends": "ffmpeg", "Section": "net", "Priority": "optional",
	} {
		if got, _ := deb.Control.Get(field); got != want {
			t.Errorf("%s = %q, want %q", field, got, want)
		}
	}
	if !slices.Equal(deb.Conffiles, []string{"/etc/ludworks/config.json"}) {
		t.Errorf("conffiles = %v", deb.Conffiles)
	}
	for p, mode := range map[string]uint32{
		"/usr/bin/ludworks":                                          0o755,
		"/usr/lib/systemd/system/ludworks.service":                   0o644,
		"/etc/ludworks/config.json":                                  0o640,
		"/etc/ludworks":                                              0o755,
		"/usr/share/doc/ludworks/copyright":                          0o644,
		"/usr/share/doc/ludworks/examples/bind-privileged-port.conf": 0o644,
	} {
		e := deb.DataFile(p)
		if e == nil || e.Mode != mode {
			t.Errorf("%s: %+v, want mode %o", p, e, mode)
		}
	}
	if deb.DataFile("/var/lib/ludworks") != nil {
		t.Error("/var/lib/ludworks must be created by postinst (owned by the service user), not shipped")
	}
	for _, s := range []string{"postinst", "prerm", "postrm"} {
		e := deb.ControlFile(s)
		if e == nil || e.Mode != 0o755 || !bytes.HasPrefix(e.Data, []byte("#!/bin/sh\n")) {
			t.Errorf("%s missing, not 0755 or not a /bin/sh script", s)
		}
	}
}

// TestDpkgDeb cross-checks the output with the real dpkg-deb when it is installed.
func TestDpkgDeb(t *testing.T) {
	dpkg, err := exec.LookPath("dpkg-deb")
	if err != nil {
		t.Skip("dpkg-deb not installed")
	}
	spec := fixture(t, nil)
	debPath := filepath.Join(t.TempDir(), "demo.deb")
	pkg, err := LoadSpec(spec, vars(spec), epoch)
	if err != nil {
		t.Fatal(err)
	}
	if err := pkg.WriteFile(debPath); err != nil {
		t.Fatal(err)
	}
	runOut := func(args ...string) []byte {
		t.Helper()
		cmd := exec.Command(dpkg, args...)
		cmd.Env = append(os.Environ(), "TZ=UTC", "LC_ALL=C")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("dpkg-deb %v: %v\n%s", args, err, stderr.String())
		}
		if stderr.Len() > 0 {
			t.Errorf("dpkg-deb %v warned: %s", args, stderr.String())
		}
		return out
	}

	info := string(runOut("--info", debPath))
	for _, want := range []string{"new Debian package, version 2.0.", "Package: demo", "Version: 1.2.3~rc.1",
		"Installed-Size: 19", "postinst", "prerm", "conffiles"} {
		if !strings.Contains(info, want) {
			t.Errorf("--info lacks %q:\n%s", want, info)
		}
	}
	if got := strings.TrimSpace(string(runOut("--field", debPath, "Package"))); got != "demo" {
		t.Errorf("--field Package = %q", got)
	}
	if got := strings.TrimSpace(string(runOut("--showformat=${Version} ${Architecture}", "--show", debPath))); got != "1.2.3~rc.1 amd64" {
		t.Errorf("--show = %q", got)
	}
	contents := string(runOut("--contents", debPath))
	for _, want := range []string{"-rwxr-xr-x root/root", "./usr/bin/demo", "drwxr-x--- root/root", "./etc/demo/",
		"-rw-r----- root/root", "./etc/demo/demo.conf", "2026-01-01 00:00"} {
		if !strings.Contains(contents, want) {
			t.Errorf("--contents lacks %q:\n%s", want, contents)
		}
	}

	// --fsys-tarfile and --ctrl-tarfile hand back plain tars; read them with Go.
	fsys := tarNames(t, runOut("--fsys-tarfile", debPath))
	if fsys["./usr/bin/demo"] != 0o755 || fsys["./etc/demo/"] != 0o750 {
		t.Errorf("--fsys-tarfile modes: %v", fsys)
	}
	ctrl := tarNames(t, runOut("--ctrl-tarfile", debPath))
	if ctrl["./postinst"] != 0o755 || ctrl["./md5sums"] != 0o644 {
		t.Errorf("--ctrl-tarfile modes: %v", ctrl)
	}

	// Extract both parts like dpkg would and compare.
	root := t.TempDir()
	runOut("-x", debPath, root)
	got, err := os.ReadFile(filepath.Join(root, "usr", "bin", "demo"))
	if err != nil || !bytes.Equal(got, deb(t, debPath).DataFile("/usr/bin/demo").Data) {
		t.Errorf("extracted binary differs: %v", err)
	}
	if st, err := os.Stat(filepath.Join(root, "usr", "bin", "demo")); err != nil || st.Mode().Perm() != 0o755 {
		t.Errorf("extracted mode %v %v", st.Mode(), err)
	}
	ctlDir := filepath.Join(t.TempDir(), "DEBIAN")
	runOut("-e", debPath, ctlDir)
	if st, err := os.Stat(filepath.Join(ctlDir, "postinst")); err != nil || st.Mode().Perm() != 0o755 {
		t.Errorf("postinst mode %v %v", st.Mode(), err)
	}
	if b, err := os.ReadFile(filepath.Join(ctlDir, "conffiles")); err != nil || string(b) != "/etc/demo/demo.conf\n" {
		t.Errorf("conffiles %q %v", b, err)
	}
	// md5sums verify against the extracted tree with md5sum -c when available.
	if md5sum, err := exec.LookPath("md5sum"); err == nil {
		cmd := exec.Command(md5sum, "--quiet", "-c", filepath.Join(ctlDir, "md5sums"))
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("md5sum -c: %v\n%s", err, out)
		}
	}
}

func deb(t *testing.T, p string) *Deb {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	d, err := ReadDeb(b)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func tarNames(t *testing.T, b []byte) map[string]uint32 {
	t.Helper()
	out := map[string]uint32{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Uid != 0 || h.Gid != 0 {
			t.Errorf("%s owned by %d:%d", h.Name, h.Uid, h.Gid)
		}
		out[h.Name] = uint32(h.Mode)
	}
}
