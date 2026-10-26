package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"
	"time"
)

// TarEntry is one entry of control.tar.gz or data.tar.gz.
type TarEntry struct {
	Name     string // as stored, e.g. "./usr/bin/ludworks"
	Type     byte
	Mode     uint32
	UID, GID int
	Uname    string
	Gname    string
	ModTime  time.Time
	Data     []byte
}

// Deb is a parsed .deb.
type Deb struct {
	Members   []arMember
	Control   *Control
	Conffiles []string
	MD5Sums   map[string]string // path (no leading ./) -> hex md5
	ControlFS []TarEntry
	DataFS    []TarEntry
}

// Limits for reading untrusted packages.
const (
	maxReadDecompressed = 2 << 30
	maxReadEntries      = 100000
)

// ReadDeb parses a .deb held in memory. Only gzip-compressed members are supported (that is
// what mkdeb writes); dpkg-deb understands the rest.
func ReadDeb(b []byte) (*Deb, error) {
	members, err := readAr(b)
	if err != nil {
		return nil, err
	}
	if len(members) < 3 || members[0].Name != "debian-binary" ||
		!strings.HasPrefix(members[1].Name, "control.tar") || !strings.HasPrefix(members[2].Name, "data.tar") {
		names := make([]string, len(members))
		for i, m := range members {
			names[i] = m.Name
		}
		return nil, fmt.Errorf("unexpected members %v (want debian-binary, control.tar.*, data.tar.*)", names)
	}
	if string(members[0].Data) != "2.0\n" {
		return nil, fmt.Errorf("debian-binary is %q, want \"2.0\\n\"", members[0].Data)
	}
	d := &Deb{Members: members, MD5Sums: map[string]string{}}
	if d.ControlFS, err = readTarGz(members[1]); err != nil {
		return nil, err
	}
	if d.DataFS, err = readTarGz(members[2]); err != nil {
		return nil, err
	}
	ctl := d.ControlFile("control")
	if ctl == nil {
		return nil, errors.New("control.tar has no ./control")
	}
	if d.Control, err = ParseControlLoose(string(ctl.Data)); err != nil {
		return nil, fmt.Errorf("control: %w", err)
	}
	if cf := d.ControlFile("conffiles"); cf != nil {
		for _, l := range strings.Split(strings.TrimRight(string(cf.Data), "\n"), "\n") {
			if l != "" {
				d.Conffiles = append(d.Conffiles, l)
			}
		}
	}
	if ms := d.ControlFile("md5sums"); ms != nil {
		for i, l := range strings.Split(strings.TrimRight(string(ms.Data), "\n"), "\n") {
			if l == "" {
				continue
			}
			sum, p, ok := strings.Cut(l, "  ")
			if !ok || len(sum) != 32 {
				return nil, fmt.Errorf("md5sums line %d is malformed", i+1)
			}
			d.MD5Sums[p] = sum
		}
	}
	return d, nil
}

// ParseControlLoose parses a control paragraph without mkdeb's template rules (it accepts
// Installed-Size and any field set).
func ParseControlLoose(text string) (*Control, error) {
	c := &Control{}
	for i, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		switch {
		case line == "":
			return nil, fmt.Errorf("line %d: empty line", i+1)
		case line[0] == ' ' || line[0] == '\t':
			if len(c.Fields) == 0 {
				return nil, fmt.Errorf("line %d: continuation before first field", i+1)
			}
			f := &c.Fields[len(c.Fields)-1]
			f.Continuation = append(f.Continuation, line[1:])
		default:
			name, value, ok := strings.Cut(line, ":")
			if !ok {
				return nil, fmt.Errorf("line %d: expected \"Field: value\"", i+1)
			}
			c.Fields = append(c.Fields, Field{Name: name, Value: strings.TrimSpace(value)})
		}
	}
	return c, nil
}

func readTarGz(m arMember) ([]TarEntry, error) {
	if !strings.HasSuffix(m.Name, ".gz") {
		return nil, fmt.Errorf("%s: only gzip-compressed members are supported", m.Name)
	}
	zr, err := gzip.NewReader(bytes.NewReader(m.Data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", m.Name, err)
	}
	defer zr.Close()
	tr := tar.NewReader(io.LimitReader(zr, maxReadDecompressed))
	var out []TarEntry
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", m.Name, err)
		}
		if len(out) >= maxReadEntries {
			return nil, fmt.Errorf("%s: too many entries", m.Name)
		}
		e := TarEntry{Name: h.Name, Type: h.Typeflag, Mode: uint32(h.Mode), UID: h.Uid, GID: h.Gid,
			Uname: h.Uname, Gname: h.Gname, ModTime: h.ModTime}
		if h.Typeflag == tar.TypeReg {
			if e.Data, err = io.ReadAll(tr); err != nil {
				return nil, fmt.Errorf("%s: %s: %w", m.Name, h.Name, err)
			}
		}
		out = append(out, e)
	}
}

// ControlFile returns the named entry of control.tar ("control", "postinst", …).
func (d *Deb) ControlFile(name string) *TarEntry {
	for i := range d.ControlFS {
		if strings.TrimPrefix(d.ControlFS[i].Name, "./") == name {
			return &d.ControlFS[i]
		}
	}
	return nil
}

// DataFile returns the data entry for an absolute install path such as "/usr/bin/ludworks".
func (d *Deb) DataFile(abs string) *TarEntry {
	want := "." + abs
	for i := range d.DataFS {
		if strings.TrimSuffix(d.DataFS[i].Name, "/") == want {
			return &d.DataFS[i]
		}
	}
	return nil
}

// VerifyMD5Sums checks that md5sums lists every non-conffile regular file with the right
// checksum, and nothing else.
func (d *Deb) VerifyMD5Sums() error {
	conf := map[string]bool{}
	for _, c := range d.Conffiles {
		conf[strings.TrimPrefix(c, "/")] = true
	}
	seen := map[string]bool{}
	var problems []string
	for _, e := range d.DataFS {
		if e.Type != tar.TypeReg {
			continue
		}
		p := strings.TrimPrefix(e.Name, "./")
		if conf[p] {
			continue
		}
		seen[p] = true
		want, ok := d.MD5Sums[p]
		if !ok {
			problems = append(problems, p+": missing from md5sums")
			continue
		}
		got := md5.Sum(e.Data)
		if hex.EncodeToString(got[:]) != want {
			problems = append(problems, p+": checksum mismatch")
		}
	}
	for p := range d.MD5Sums {
		if !seen[p] {
			problems = append(problems, p+": listed in md5sums but not a packaged regular file")
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// Describe prints a human-readable summary similar to dpkg-deb --info plus --contents.
func (d *Deb) Describe(w io.Writer) {
	fmt.Fprintln(w, "control:")
	for _, line := range strings.Split(strings.TrimRight(d.Control.String(), "\n"), "\n") {
		fmt.Fprintln(w, "  "+line)
	}
	fmt.Fprintln(w, "control.tar:")
	for _, e := range d.ControlFS {
		fmt.Fprintf(w, "  %s\n", formatEntry(e))
	}
	if len(d.Conffiles) > 0 {
		fmt.Fprintln(w, "conffiles:")
		for _, c := range d.Conffiles {
			fmt.Fprintln(w, "  "+c)
		}
	}
	fmt.Fprintln(w, "data.tar:")
	for _, e := range d.DataFS {
		fmt.Fprintf(w, "  %s\n", formatEntry(e))
	}
}

func formatEntry(e TarEntry) string {
	mode := fs.FileMode(e.Mode & 0o777)
	if e.Type == tar.TypeDir {
		mode |= fs.ModeDir
	}
	return fmt.Sprintf("%s %s/%s %10d %s %s", mode, e.Uname, e.Gname, len(e.Data),
		e.ModTime.UTC().Format("2006-01-02 15:04"), e.Name)
}
