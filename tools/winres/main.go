// Command winres generates the Windows resources of ludworks.exe in pure Go:
//
//	winres icon   -out packaging/windows/ludworks.ico [-png DIR]
//	winres syso   -version 0.1.0 -icon packaging/windows/ludworks.ico \
//	              -out cmd/ludworks/rsrc_windows_amd64.syso [-arch amd64]
//	winres verify -version 0.1.0 FILE.exe|FILE.syso
//
// "icon" draws the Ludworks mark (the web UI logo) into a multi-size .ico (16, 32, 48 and
// 256 px). "syso" writes a COFF object holding RT_GROUP_ICON/RT_ICON, an RT_VERSION
// VS_VERSIONINFO (file/product version, company, copyright …) and an RT_MANIFEST
// (asInvoker, Windows 10+ compatibility, long-path aware); the Go linker picks up *.syso
// files in the main package's directory automatically. "verify" reads the resources back
// from a linked executable (or a .syso) with debug/pe and checks the version and icon.
//
// The package scripts generate the .syso only while building the Windows binary and
// delete it afterwards; it is never committed (see .gitignore).
package main

import (
	"bytes"
	"debug/pe"
	"flag"
	"fmt"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

const usage = `Usage:
  winres icon   -out FILE.ico [-png DIR]
  winres syso   -version X.Y.Z -icon FILE.ico -out FILE.syso [-arch amd64|386|arm64] [string flags]
  winres verify -version X.Y.Z FILE.exe|FILE.syso
`

// Product metadata (overridable with flags on "syso").
const (
	defaultCompany     = "Ludworks"
	defaultProduct     = "Ludworks"
	defaultDescription = "Ludworks server"
	defaultCopyright   = "Copyright (c) 2026 Ludworks. All rights reserved."
	defaultInternal    = "ludworks"
	defaultOriginal    = "ludworks.exe"
)

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "icon":
		err = cmdIcon(args[1:], stdout, stderr)
	case "syso":
		err = cmdSyso(args[1:], stdout, stderr)
	case "verify":
		err = cmdVerify(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "winres: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	if err == errUsage {
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "winres:", err)
		return 1
	}
	return 0
}

var errUsage = fmt.Errorf("usage")

func cmdIcon(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("winres icon", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "", "output .ico")
	pngDir := fs.String("png", "", "also write ludworks-<size>.png previews into this directory")
	if err := fs.Parse(args); err != nil || *out == "" || fs.NArg() != 0 {
		if err == nil {
			fmt.Fprint(stderr, usage)
		}
		return errUsage
	}
	ico, err := LogoICO()
	if err != nil {
		return err
	}
	if err := writeFile(*out, ico); err != nil {
		return err
	}
	if *pngDir != "" {
		for _, s := range IconSizes {
			var buf bytes.Buffer
			if err := png.Encode(&buf, RenderLogo(s)); err != nil {
				return err
			}
			if err := writeFile(filepath.Join(*pngDir, "ludworks-"+strconv.Itoa(s)+".png"), buf.Bytes()); err != nil {
				return err
			}
		}
	}
	fmt.Fprintf(stdout, "winres: wrote %s (%d bytes, sizes %v)\n", *out, len(ico), IconSizes)
	return nil
}

// Options configure the generated resources.
type Options struct {
	Version     string
	Arch        string
	ICO         []byte
	Company     string
	Product     string
	Description string
	Copyright   string
	Internal    string
	Original    string
}

func cmdSyso(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("winres syso", flag.ContinueOnError)
	fs.SetOutput(stderr)
	o := Options{}
	fs.StringVar(&o.Version, "version", "", "semantic version, e.g. 0.1.0")
	fs.StringVar(&o.Arch, "arch", "amd64", "target architecture: amd64, 386 or arm64")
	icon := fs.String("icon", "", ".ico file to embed")
	out := fs.String("out", "", "output .syso (name it rsrc_windows_<arch>.syso)")
	fs.StringVar(&o.Company, "company", defaultCompany, "CompanyName")
	fs.StringVar(&o.Product, "product", defaultProduct, "ProductName")
	fs.StringVar(&o.Description, "description", defaultDescription, "FileDescription")
	fs.StringVar(&o.Copyright, "copyright", defaultCopyright, "LegalCopyright")
	fs.StringVar(&o.Internal, "internal-name", defaultInternal, "InternalName")
	fs.StringVar(&o.Original, "original-filename", defaultOriginal, "OriginalFilename")
	if err := fs.Parse(args); err != nil || o.Version == "" || *icon == "" || *out == "" || fs.NArg() != 0 {
		if err == nil {
			fmt.Fprint(stderr, usage)
		}
		return errUsage
	}
	ico, err := os.ReadFile(*icon)
	if err != nil {
		return err
	}
	o.ICO = ico
	obj, err := BuildSyso(o)
	if err != nil {
		return err
	}
	if err := writeFile(*out, obj); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "winres: wrote %s (%s, version %s, %d bytes)\n", *out, o.Arch, o.Version, len(obj))
	return nil
}

// BuildSyso assembles the resources and returns the COFF object.
func BuildSyso(o Options) ([]byte, error) {
	quad, pre, err := ParseSemver(o.Version)
	if err != nil {
		return nil, err
	}
	icons, err := DecodeICO(o.ICO)
	if err != nil {
		return nil, err
	}
	for _, s := range []string{o.Company, o.Product, o.Description, o.Copyright, o.Internal, o.Original} {
		if s == "" || len(s) > 256 || strings.ContainsAny(s, "\x00\r\n") {
			return nil, fmt.Errorf("invalid version string %q", s)
		}
	}
	var res []Resource
	// RT_ICON entries 1..n and the RT_GROUP_ICON (GRPICONDIR) that references them.
	var group bytes.Buffer
	le := littleEndian{&group}
	le.u16(0)
	le.u16(1)
	le.u16(uint16(len(icons)))
	for i, ic := range icons {
		id := uint16(i + 1)
		res = append(res, Resource{Type: rtIcon, ID: id, Lang: langEnUS, Data: ic.Data})
		group.WriteByte(dim(ic.Width))
		group.WriteByte(dim(ic.Height))
		group.WriteByte(0) // colour count
		group.WriteByte(0) // reserved
		le.u16(1)          // planes
		le.u16(ic.BitCount)
		le.u32(uint32(len(ic.Data)))
		le.u16(id)
	}
	res = append(res, Resource{Type: rtGroupIcon, ID: 1, Lang: langEnUS, Data: group.Bytes()})

	vi := VersionInfo{Version: quad, Prerelease: pre, Strings: []KeyValue{
		{"CompanyName", o.Company},
		{"FileDescription", o.Description},
		{"FileVersion", o.Version},
		{"InternalName", o.Internal},
		{"LegalCopyright", o.Copyright},
		{"OriginalFilename", o.Original},
		{"ProductName", o.Product},
		{"ProductVersion", o.Version},
	}}
	ver, err := vi.Encode()
	if err != nil {
		return nil, err
	}
	res = append(res, Resource{Type: rtVersion, ID: 1, Lang: langEnUS, Data: ver})
	res = append(res, Resource{Type: rtManifest, ID: 1, Lang: langEnUS, Data: manifest(quad, o.Arch, o.Description)})
	return WriteCOFF(o.Arch, res)
}

// manifest is the application manifest: run with the caller's token (no UAC prompt, no
// installer-detection heuristics), declare Windows 10/11 support and long-path awareness.
func manifest(q [4]uint16, arch, description string) []byte {
	procArch := map[string]string{"amd64": "amd64", "386": "x86", "arm64": "arm64"}[arch]
	return []byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<assembly xmlns="urn:schemas-microsoft-com:asm.v1" manifestVersion="1.0">
  <assemblyIdentity type="win32" name="Ludworks.Server" version="` + quadString(q) + `" processorArchitecture="` + procArch + `"/>
  <description>` + xmlEscape(description) + `</description>
  <trustInfo xmlns="urn:schemas-microsoft-com:asm.v3">
    <security>
      <requestedPrivileges>
        <requestedExecutionLevel level="asInvoker" uiAccess="false"/>
      </requestedPrivileges>
    </security>
  </trustInfo>
  <compatibility xmlns="urn:schemas-microsoft-com:compatibility.v1">
    <application>
      <supportedOS Id="{8e0f7a12-bfb3-4fe8-b9a5-48fd50a15a9a}"/>
    </application>
  </compatibility>
  <application xmlns="urn:schemas-microsoft-com:asm.v3">
    <windowsSettings>
      <longPathAware xmlns="http://schemas.microsoft.com/SMI/2016/WindowsSettings">true</longPathAware>
    </windowsSettings>
  </application>
</assembly>
`)
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;").Replace(s)
}

type littleEndian struct{ b *bytes.Buffer }

func (l littleEndian) u16(v uint16) { l.b.Write([]byte{byte(v), byte(v >> 8)}) }
func (l littleEndian) u32(v uint32) { l.u16(uint16(v)); l.u16(uint16(v >> 16)) }

func cmdVerify(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("winres verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	version := fs.String("version", "", "expected semantic version")
	if err := fs.Parse(args); err != nil || *version == "" || fs.NArg() != 1 {
		if err == nil {
			fmt.Fprint(stderr, usage)
		}
		return errUsage
	}
	sum, err := Verify(fs.Arg(0), *version)
	if err != nil {
		return err
	}
	fmt.Fprint(stdout, sum)
	return nil
}

// Verify opens a PE image or COFF object, reads its resources and checks the version
// resource, the icon group and the manifest. It returns a short human-readable summary.
func Verify(path, version string) (string, error) {
	quad, pre, err := ParseSemver(version)
	if err != nil {
		return "", err
	}
	f, err := pe.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	res, err := ReadResources(f)
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	byType := map[uint16][]Resource{}
	for _, r := range res {
		byType[r.Type] = append(byType[r.Type], r)
	}
	if len(byType[rtVersion]) != 1 {
		return "", fmt.Errorf("%s: want exactly one RT_VERSION resource, found %d", path, len(byType[rtVersion]))
	}
	pv, err := ParseVersionInfo(byType[rtVersion][0].Data)
	if err != nil {
		return "", err
	}
	if pv.FileVersion != quad || pv.ProductVersion != quad {
		return "", fmt.Errorf("%s: version resource is %s/%s, want %s", path,
			quadString(pv.FileVersion), quadString(pv.ProductVersion), quadString(quad))
	}
	if (pv.Flags&vsFFPrerelease != 0) != pre {
		return "", fmt.Errorf("%s: pre-release flag is %v, want %v", path, !pre, pre)
	}
	for _, k := range []string{"FileVersion", "ProductVersion"} {
		if pv.Strings[k] != version {
			return "", fmt.Errorf("%s: %s string is %q, want %q", path, k, pv.Strings[k], version)
		}
	}
	for _, k := range []string{"CompanyName", "FileDescription", "LegalCopyright", "OriginalFilename", "ProductName"} {
		if pv.Strings[k] == "" {
			return "", fmt.Errorf("%s: %s is missing", path, k)
		}
	}
	groups := byType[rtGroupIcon]
	if len(groups) != 1 {
		return "", fmt.Errorf("%s: want one RT_GROUP_ICON, found %d", path, len(groups))
	}
	g := groups[0].Data
	if len(g) < 6 {
		return "", fmt.Errorf("%s: RT_GROUP_ICON truncated", path)
	}
	n := int(g[4]) | int(g[5])<<8
	if len(g) != 6+14*n || n == 0 {
		return "", fmt.Errorf("%s: RT_GROUP_ICON has a bad size for %d images", path, n)
	}
	icons := map[uint16]Resource{}
	for _, r := range byType[rtIcon] {
		icons[r.ID] = r
	}
	var sizes []string
	for i := 0; i < n; i++ {
		e := g[6+14*i:]
		id := uint16(e[12]) | uint16(e[13])<<8
		size := uint32(e[8]) | uint32(e[9])<<8 | uint32(e[10])<<16 | uint32(e[11])<<24
		ic, ok := icons[id]
		if !ok || uint32(len(ic.Data)) != size {
			return "", fmt.Errorf("%s: icon %d referenced by the group is missing or has the wrong size", path, id)
		}
		sizes = append(sizes, strconv.Itoa(undim(e[0])))
	}
	if len(byType[rtManifest]) != 1 || !bytes.Contains(byType[rtManifest][0].Data, []byte(`level="asInvoker"`)) {
		return "", fmt.Errorf("%s: application manifest missing", path)
	}
	return fmt.Sprintf("%s: OK\n  version %s (%s)%s\n  %s, %s\n  icon sizes %s px, manifest present\n",
		path, version, quadString(pv.FileVersion), map[bool]string{true: " pre-release", false: ""}[pre],
		pv.Strings["ProductName"], pv.Strings["LegalCopyright"], strings.Join(sizes, "/")), nil
}

func writeFile(name string, data []byte) error {
	if dir := filepath.Dir(name); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := name + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, name)
}
