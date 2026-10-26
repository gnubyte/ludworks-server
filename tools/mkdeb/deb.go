package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// File is a regular file in the package; Path has no leading slash.
type File struct {
	Path     string
	Mode     uint32
	Data     []byte
	Conffile bool
}

// Dir is a directory in the package; Path has no leading slash.
type Dir struct {
	Path string
	Mode uint32
}

// Package is a fully loaded, validated binary package ready to be written.
type Package struct {
	MTime   time.Time
	Files   []File
	Dirs    []Dir
	Scripts map[string][]byte

	control       *Control
	installedSize int64
}

// Name returns the Package field.
func (p *Package) Name() string { v, _ := p.control.Get("Package"); return v }

// Version returns the Version field.
func (p *Package) Version() string { v, _ := p.control.Get("Version"); return v }

// Arch returns the Architecture field.
func (p *Package) Arch() string { v, _ := p.control.Get("Architecture"); return v }

// FileCount is the number of regular files.
func (p *Package) FileCount() int { return len(p.Files) }

// InstalledSize is the computed Installed-Size in KiB.
func (p *Package) InstalledSize() int64 { return p.installedSize }

// finalize validates the layout, adds implicit parent directories, sorts everything and
// computes Installed-Size.
func (p *Package) finalize() error {
	if len(p.Files) == 0 {
		return errors.New("the package installs no files")
	}
	if len(p.Files)+len(p.Dirs) > maxEntries {
		return fmt.Errorf("more than %d entries", maxEntries)
	}
	files := map[string]bool{}
	for _, f := range p.Files {
		if files[f.Path] {
			return fmt.Errorf("/%s is listed twice", f.Path)
		}
		files[f.Path] = true
	}
	dirs := map[string]uint32{}
	explicit := map[string]bool{}
	for _, d := range p.Dirs {
		if explicit[d.Path] {
			return fmt.Errorf("directory /%s is listed twice", d.Path)
		}
		if files[d.Path] {
			return fmt.Errorf("/%s is both a file and a directory", d.Path)
		}
		explicit[d.Path] = true
		dirs[d.Path] = d.Mode
	}
	addParents := func(p string) error {
		for dir := path.Dir(p); dir != "."; dir = path.Dir(dir) {
			if files[dir] {
				return fmt.Errorf("/%s is a file but also the parent of /%s", dir, p)
			}
			if _, ok := dirs[dir]; !ok {
				dirs[dir] = 0o755
			}
		}
		return nil
	}
	for _, f := range p.Files {
		if err := addParents(f.Path); err != nil {
			return err
		}
	}
	for _, d := range p.Dirs {
		if err := addParents(d.Path); err != nil {
			return err
		}
	}
	p.Dirs = p.Dirs[:0]
	for d, m := range dirs {
		p.Dirs = append(p.Dirs, Dir{Path: d, Mode: m})
	}
	sort.Slice(p.Dirs, func(i, j int) bool { return p.Dirs[i].Path < p.Dirs[j].Path })
	sort.Slice(p.Files, func(i, j int) bool { return p.Files[i].Path < p.Files[j].Path })

	for name, data := range p.Scripts {
		if err := checkScript(name, data); err != nil {
			return err
		}
	}

	// Installed-Size: every regular file rounded up to whole KiB, plus 1 KiB per directory
	// (the same estimate dpkg-gencontrol uses).
	var kib int64
	for _, f := range p.Files {
		kib += (int64(len(f.Data)) + 1023) / 1024
	}
	kib += int64(len(p.Dirs))
	p.installedSize = kib
	p.control.insertAfter(Field{Name: "Installed-Size", Value: strconv.FormatInt(kib, 10)},
		"Maintainer", "Architecture")
	return nil
}

// checkScript catches the classic maintainer-script packaging mistakes: CRLF line endings
// (from a Windows checkout), a missing interpreter line and NUL bytes.
func checkScript(name string, data []byte) error {
	switch {
	case len(data) == 0:
		return fmt.Errorf("%s is empty", name)
	case !bytes.HasPrefix(data, []byte("#!/")):
		return fmt.Errorf("%s must start with an interpreter line such as #!/bin/sh", name)
	case bytes.IndexByte(data, '\r') >= 0:
		return fmt.Errorf("%s contains carriage returns; it must use LF line endings", name)
	case bytes.IndexByte(data, 0) >= 0:
		return fmt.Errorf("%s contains NUL bytes", name)
	}
	return nil
}

// Conffiles lists the conffiles (absolute paths) in sorted order.
func (p *Package) Conffiles() []string {
	var out []string
	for _, f := range p.Files {
		if f.Conffile {
			out = append(out, "/"+f.Path)
		}
	}
	return out
}

// MD5Sums renders the md5sums control file (conffiles excluded, like dh_md5sums).
func (p *Package) MD5Sums() []byte {
	var b bytes.Buffer
	for _, f := range p.Files {
		if f.Conffile {
			continue
		}
		sum := md5.Sum(f.Data)
		b.WriteString(hex.EncodeToString(sum[:]))
		b.WriteString("  ")
		b.WriteString(f.Path)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// ControlText is the rendered control file.
func (p *Package) ControlText() string { return p.control.String() }

// Build returns the .deb bytes.
func (p *Package) Build() ([]byte, error) {
	ctl, err := p.controlTar()
	if err != nil {
		return nil, fmt.Errorf("control.tar.gz: %w", err)
	}
	data, err := p.dataTar()
	if err != nil {
		return nil, fmt.Errorf("data.tar.gz: %w", err)
	}
	var out bytes.Buffer
	aw := newArWriter(&out, p.MTime)
	for _, m := range []struct {
		name string
		data []byte
	}{
		{"debian-binary", []byte("2.0\n")},
		{"control.tar.gz", ctl},
		{"data.tar.gz", data},
	} {
		if err := aw.add(m.name, m.data); err != nil {
			return nil, err
		}
	}
	return out.Bytes(), nil
}

// WriteFile builds the package and writes it atomically to name.
func (p *Package) WriteFile(name string) error {
	data, err := p.Build()
	if err != nil {
		return err
	}
	dir := filepath.Dir(name)
	tmp, err := os.CreateTemp(dir, ".mkdeb-*.deb")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// CreateTemp uses 0600; a package is not secret.
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), name)
}

type tarEntry struct {
	name string // "./" or "./path" (directories end in "/")
	mode uint32
	dir  bool
	data []byte
}

func (p *Package) controlTar() ([]byte, error) {
	entries := []tarEntry{
		{name: "./", mode: 0o755, dir: true},
		{name: "./control", mode: 0o644, data: []byte(p.ControlText())},
		{name: "./md5sums", mode: 0o644, data: p.MD5Sums()},
	}
	if cf := p.Conffiles(); len(cf) > 0 {
		entries = append(entries, tarEntry{name: "./conffiles", mode: 0o644, data: []byte(strings.Join(cf, "\n") + "\n")})
	}
	names := make([]string, 0, len(p.Scripts))
	for n := range p.Scripts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		entries = append(entries, tarEntry{name: "./" + n, mode: 0o755, data: p.Scripts[n]})
	}
	sort.SliceStable(entries[1:], func(i, j int) bool { return entries[1+i].name < entries[1+j].name })
	return p.tarGz(entries)
}

func (p *Package) dataTar() ([]byte, error) {
	entries := make([]tarEntry, 0, 1+len(p.Dirs)+len(p.Files))
	entries = append(entries, tarEntry{name: "./", mode: 0o755, dir: true})
	for _, d := range p.Dirs {
		entries = append(entries, tarEntry{name: "./" + d.Path + "/", mode: d.Mode, dir: true})
	}
	for _, f := range p.Files {
		entries = append(entries, tarEntry{name: "./" + f.Path, mode: f.Mode, data: f.Data})
	}
	// Lexicographic order puts every directory before its contents ("./etc/" < "./etc/x").
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	return p.tarGz(entries)
}

// tarGz writes a deterministic GNU tar (the format dpkg-deb itself produces) compressed
// with gzip: root:root ownership, fixed mtime, no gzip name or timestamp.
func (p *Package) tarGz(entries []tarEntry) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		h := &tar.Header{
			Name:    e.name,
			Mode:    int64(e.mode),
			Uid:     0,
			Gid:     0,
			Uname:   "root",
			Gname:   "root",
			ModTime: p.MTime,
			Format:  tar.FormatGNU,
		}
		if e.dir {
			h.Typeflag = tar.TypeDir
		} else {
			h.Typeflag = tar.TypeReg
			h.Size = int64(len(e.data))
		}
		if err := tw.WriteHeader(h); err != nil {
			return nil, fmt.Errorf("%s: %w", e.name, err)
		}
		if !e.dir {
			if _, err := tw.Write(e.data); err != nil {
				return nil, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
