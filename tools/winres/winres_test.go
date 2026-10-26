package main

import (
	"bytes"
	"debug/pe"
	"image"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderLogo(t *testing.T) {
	for _, size := range IconSizes {
		img := RenderLogo(size)
		if img.Rect.Dx() != size || img.Rect.Dy() != size {
			t.Fatalf("size %d: got %v", size, img.Rect)
		}
		// Rounded corners are transparent, the centre column of the background is opaque.
		if a := img.NRGBAAt(0, 0).A; a != 0 {
			t.Errorf("size %d: corner alpha %d", size, a)
		}
		mid := size / 2
		if c := img.NRGBAAt(size/8+1, mid); c.A != 255 || c.B < 0xc0 || c.R > 0x70 {
			t.Errorf("size %d: background pixel %+v is not opaque indigo", size, c)
		}
		// Somewhere on the first bar row there is pure white.
		bar := size * 230 / 720
		if size <= 32 {
			bar = size * 205 / 640
		}
		white := false
		for x := 0; x < size; x++ {
			if c := img.NRGBAAt(x, bar); c.R == 255 && c.G == 255 && c.B == 255 && c.A == 255 {
				white = true
			}
		}
		if !white {
			t.Errorf("size %d: no white bar on row %d", size, bar)
		}
	}
	if !bytes.Equal(RenderLogo(48).Pix, RenderLogo(48).Pix) {
		t.Fatal("rendering is not deterministic")
	}
}

func TestICORoundTrip(t *testing.T) {
	ico, err := LogoICO()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := DecodeICO(ico)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(IconSizes) {
		t.Fatalf("%d entries", len(entries))
	}
	for i, e := range entries {
		if e.Width != IconSizes[i] || e.Height != IconSizes[i] || e.BitCount != 32 {
			t.Errorf("entry %d: %dx%d %dbpp", i, e.Width, e.Height, e.BitCount)
		}
		isPNG := bytes.HasPrefix(e.Data, []byte("\x89PNG"))
		if isPNG != (e.Width == 256) {
			t.Errorf("entry %d: png=%v", i, isPNG)
		}
		got, err := e.Decode()
		if err != nil {
			t.Fatal(err)
		}
		samePixels(t, got, RenderLogo(e.Width))
	}
	if _, err := EncodeICO([]*image.NRGBA{image.NewNRGBA(image.Rect(0, 0, 10, 12))}); err == nil {
		t.Error("non-square image accepted")
	}
	for _, bad := range [][]byte{nil, []byte("PK\x03\x04xxxx"), {0, 0, 1, 0, 1, 0}} {
		if _, err := DecodeICO(bad); err == nil {
			t.Errorf("garbage %q accepted", bad)
		}
	}
}

func samePixels(t *testing.T, a, b *image.NRGBA) {
	t.Helper()
	if a.Rect != b.Rect {
		t.Fatalf("bounds %v vs %v", a.Rect, b.Rect)
	}
	for y := a.Rect.Min.Y; y < a.Rect.Max.Y; y++ {
		for x := a.Rect.Min.X; x < a.Rect.Max.X; x++ {
			if a.NRGBAAt(x, y) != b.NRGBAAt(x, y) {
				t.Fatalf("pixel (%d,%d): %v vs %v", x, y, a.NRGBAAt(x, y), b.NRGBAAt(x, y))
			}
		}
	}
}

// The committed icon must be exactly what "winres icon" draws (pixels, not bytes, so a
// different zlib in a future Go release does not matter).
func TestCommittedIconIsCurrent(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "packaging", "windows", "ludworks.ico"))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := DecodeICO(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(IconSizes) {
		t.Fatalf("committed icon has %d images; regenerate with: go run ./tools/winres icon -out packaging/windows/ludworks.ico", len(entries))
	}
	for _, e := range entries {
		img, err := e.Decode()
		if err != nil {
			t.Fatal(err)
		}
		samePixels(t, img, RenderLogo(e.Width))
	}
}

func TestSemver(t *testing.T) {
	cases := map[string]struct {
		quad [4]uint16
		pre  bool
	}{
		"0.1.0":              {[4]uint16{0, 1, 0, 0}, false},
		"1.22.333":           {[4]uint16{1, 22, 333, 0}, false},
		"2.0.0-rc.1":         {[4]uint16{2, 0, 0, 0}, true},
		"0.1.0+3.gabc1234":   {[4]uint16{0, 1, 0, 0}, false},
		"0.2.0-beta+exp.sha": {[4]uint16{0, 2, 0, 0}, true},
	}
	for v, want := range cases {
		q, pre, err := ParseSemver(v)
		if err != nil || q != want.quad || pre != want.pre {
			t.Errorf("%s: %v %v %v", v, q, pre, err)
		}
	}
	for _, bad := range []string{"", "1", "1.2", "v1.2.3", "01.2.3", "1.2.3.4", "1.2.70000", "1.2.3-", "1.2.3 "} {
		if _, _, err := ParseSemver(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestVersionInfoRoundTrip(t *testing.T) {
	vi := VersionInfo{Version: [4]uint16{1, 2, 3, 0}, Prerelease: true, Strings: []KeyValue{
		{"CompanyName", "Ludworks"}, {"FileVersion", "1.2.3-rc.1"}, {"ProductName", "Ludwörks ✓"}, {"Odd", "x"},
	}}
	b, err := vi.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if len(b)%2 != 0 || int(b[0])|int(b[1])<<8 != len(b) {
		t.Fatalf("root wLength %d vs %d bytes", int(b[0])|int(b[1])<<8, len(b))
	}
	pv, err := ParseVersionInfo(b)
	if err != nil {
		t.Fatal(err)
	}
	if pv.FileVersion != vi.Version || pv.ProductVersion != vi.Version || pv.Flags&vsFFPrerelease == 0 {
		t.Errorf("fixed info %+v", pv)
	}
	for _, kv := range vi.Strings {
		if pv.Strings[kv.Key] != kv.Value {
			t.Errorf("%s = %q, want %q", kv.Key, pv.Strings[kv.Key], kv.Value)
		}
	}
	if len(pv.Translations) != 1 || pv.Translations[0] != [2]uint16{0x0409, 1200} {
		t.Errorf("translations %v", pv.Translations)
	}
	if _, err := (VersionInfo{Strings: []KeyValue{{"A", "1"}, {"A", "2"}}}).Encode(); err == nil {
		t.Error("duplicate key accepted")
	}
	for _, bad := range [][]byte{nil, b[:10], append([]byte{0xff, 0xff}, b[2:]...)} {
		if _, err := ParseVersionInfo(bad); err == nil {
			t.Error("garbage accepted")
		}
	}
}

func testOptions(t *testing.T, version, arch string) Options {
	t.Helper()
	ico, err := LogoICO()
	if err != nil {
		t.Fatal(err)
	}
	return Options{Version: version, Arch: arch, ICO: ico, Company: defaultCompany, Product: defaultProduct,
		Description: defaultDescription, Copyright: defaultCopyright, Internal: defaultInternal, Original: defaultOriginal}
}

func TestSysoStructure(t *testing.T) {
	obj, err := BuildSyso(testOptions(t, "0.1.0", "amd64"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := pe.NewFile(bytes.NewReader(obj))
	if err != nil {
		t.Fatal(err)
	}
	if f.Machine != pe.IMAGE_FILE_MACHINE_AMD64 || len(f.Sections) != 1 || f.Sections[0].Name != ".rsrc" {
		t.Fatalf("header %+v sections %d", f.FileHeader, len(f.Sections))
	}
	if len(f.Symbols) != 1 || f.Symbols[0].Name != ".rsrc" || f.Symbols[0].SectionNumber != 1 || f.Symbols[0].StorageClass != 3 {
		t.Fatalf("symbols %+v", f.Symbols)
	}
	res, err := ReadResources(f)
	if err != nil {
		t.Fatal(err)
	}
	// 4 icons + group + version + manifest, one relocation per data entry.
	if len(res) != 7 || len(f.Sections[0].Relocs) != 7 {
		t.Fatalf("%d resources, %d relocations", len(res), len(f.Sections[0].Relocs))
	}
	for _, r := range f.Sections[0].Relocs {
		if r.Type != 3 || r.SymbolTableIndex != 0 {
			t.Errorf("relocation %+v", r)
		}
	}
	// Resources are sorted by type and id, as Windows requires for its binary search.
	for i := 1; i < len(res); i++ {
		if res[i-1].Type > res[i].Type || res[i-1].Type == res[i].Type && res[i-1].ID >= res[i].ID {
			t.Errorf("resources out of order at %d", i)
		}
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "rsrc.syso")
	if err := os.WriteFile(p, obj, 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := Verify(p, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sum, "icon sizes 16/32/48/256 px") || !strings.Contains(sum, "0.1.0.0") {
		t.Errorf("summary %q", sum)
	}
	if _, err := Verify(p, "0.1.1"); err == nil {
		t.Error("wrong version accepted")
	}
	if _, err := BuildSyso(testOptions(t, "0.1.0", "mips")); err == nil {
		t.Error("unknown arch accepted")
	}
	bad := testOptions(t, "0.1.0", "amd64")
	bad.Copyright = "a\r\nb"
	if _, err := BuildSyso(bad); err == nil {
		t.Error("CR/LF in a version string accepted")
	}
	// Deterministic.
	again, _ := BuildSyso(testOptions(t, "0.1.0", "amd64"))
	if !bytes.Equal(obj, again) {
		t.Error("syso output is not deterministic")
	}
}

// TestLinkWithGo links the .syso into real Windows executables with the Go toolchain and
// reads the resources back from the PE image.
func TestLinkWithGo(t *testing.T) {
	if testing.Short() {
		t.Skip("links executables")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not on PATH")
	}
	for _, arch := range []string{"amd64", "arm64", "386"} {
		t.Run(arch, func(t *testing.T) {
			dir := t.TempDir()
			write := func(name, content string) {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			write("go.mod", "module wrtest\n\ngo 1.22\n")
			write("main.go", "package main\n\nfunc main() { println(\"hi\") }\n")
			obj, err := BuildSyso(testOptions(t, "1.2.3-rc.1", arch))
			if err != nil {
				t.Fatal(err)
			}
			write("rsrc_windows_"+arch+".syso", string(obj))
			exe := filepath.Join(dir, "out.exe")
			cmd := exec.Command(goBin, "build", "-trimpath", "-ldflags=-s -w", "-o", exe, ".")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "GOOS=windows", "GOARCH="+arch, "CGO_ENABLED=0", "GOFLAGS=", "GOWORK=off")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("go build: %v\n%s", err, out)
			}
			sum, err := Verify(exe, "1.2.3-rc.1")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(sum, "pre-release") {
				t.Errorf("summary %q", sum)
			}
			f, err := pe.Open(exe)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			// The resource data directory must point at the .rsrc section.
			var dd pe.DataDirectory
			switch oh := f.OptionalHeader.(type) {
			case *pe.OptionalHeader64:
				dd = oh.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_RESOURCE]
			case *pe.OptionalHeader32:
				dd = oh.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_RESOURCE]
			}
			if s := f.Section(".rsrc"); s == nil || dd.VirtualAddress != s.VirtualAddress || dd.Size == 0 {
				t.Errorf("resource data directory %+v does not match .rsrc", dd)
			}
		})
	}
}

func TestCLI(t *testing.T) {
	dir := t.TempDir()
	ico := filepath.Join(dir, "x.ico")
	var out, errb bytes.Buffer
	if code := run([]string{"icon", "-out", ico, "-png", dir}, &out, &errb); code != 0 {
		t.Fatalf("icon: %d %s", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "ludworks-256.png")); err != nil {
		t.Error(err)
	}
	syso := filepath.Join(dir, "rsrc_windows_amd64.syso")
	if code := run([]string{"syso", "-version", "0.1.0", "-icon", ico, "-out", syso}, &out, &errb); code != 0 {
		t.Fatalf("syso: %d %s", code, errb.String())
	}
	out.Reset()
	if code := run([]string{"verify", "-version", "0.1.0", syso}, &out, &errb); code != 0 {
		t.Fatalf("verify: %d %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "OK") {
		t.Errorf("verify output %q", out.String())
	}
	if code := run([]string{"verify", "-version", "9.9.9", syso}, &out, &errb); code != 1 {
		t.Errorf("verify of wrong version exit %d", code)
	}
	for _, args := range [][]string{nil, {"nope"}, {"icon"}, {"syso", "-version", "1.0.0"}, {"verify", syso}} {
		if code := run(args, &out, &errb); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	if code := run([]string{"syso", "-version", "1.0", "-icon", ico, "-out", syso}, &out, &errb); code != 1 {
		t.Errorf("bad version: exit %d", code)
	}
}
