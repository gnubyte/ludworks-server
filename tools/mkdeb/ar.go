package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// The .deb container is a System V / GNU "common" ar archive: the magic "!<arch>\n" and then,
// for every member, a 60-byte ASCII header followed by the data padded to an even length.
//
//	name[16] mtime[12] uid[6] gid[6] mode[8] size[10] "`\n"
const (
	arMagic     = "!<arch>\n"
	arHeaderLen = 60
)

type arWriter struct {
	w     io.Writer
	mtime int64
	began bool
}

func newArWriter(w io.Writer, mtime time.Time) *arWriter {
	return &arWriter{w: w, mtime: mtime.Unix()}
}

func (a *arWriter) add(name string, data []byte) error {
	if len(name) == 0 || len(name) > 15 || strings.ContainsAny(name, "/ \n") {
		return fmt.Errorf("ar: invalid member name %q", name)
	}
	if !a.began {
		if _, err := io.WriteString(a.w, arMagic); err != nil {
			return err
		}
		a.began = true
	}
	// dpkg-deb writes plain names padded with spaces (no GNU "/" terminator), uid/gid 0 and
	// mode 100644.
	hdr := fmt.Sprintf("%-16s%-12d%-6d%-6d%-8s%-10d`\n", name, a.mtime, 0, 0, "100644", len(data))
	if len(hdr) != arHeaderLen {
		return fmt.Errorf("ar: header for %q is %d bytes", name, len(hdr))
	}
	if _, err := io.WriteString(a.w, hdr); err != nil {
		return err
	}
	if _, err := a.w.Write(data); err != nil {
		return err
	}
	if len(data)%2 == 1 {
		if _, err := a.w.Write([]byte{'\n'}); err != nil {
			return err
		}
	}
	return nil
}

// arMember is one member read back from an archive.
type arMember struct {
	Name  string
	MTime int64
	UID   int
	GID   int
	Mode  uint32
	Data  []byte
}

// readAr parses an ar archive held in memory.
func readAr(b []byte) ([]arMember, error) {
	if !bytes.HasPrefix(b, []byte(arMagic)) {
		return nil, errors.New("not an ar archive (bad magic)")
	}
	var out []arMember
	off := len(arMagic)
	for off < len(b) {
		if len(b)-off < arHeaderLen {
			return nil, fmt.Errorf("ar: truncated header at offset %d", off)
		}
		h := b[off : off+arHeaderLen]
		if string(h[58:60]) != "`\n" {
			return nil, fmt.Errorf("ar: bad header terminator at offset %d", off)
		}
		field := func(lo, hi int) string { return strings.TrimRight(string(h[lo:hi]), " ") }
		name := strings.TrimSuffix(field(0, 16), "/")
		mtime, err1 := strconv.ParseInt(field(16, 28), 10, 64)
		uid, err2 := strconv.Atoi(field(28, 34))
		gid, err3 := strconv.Atoi(field(34, 40))
		mode, err4 := strconv.ParseUint(field(40, 48), 8, 32)
		size, err5 := strconv.ParseInt(field(48, 58), 10, 64)
		if err := errors.Join(err1, err2, err3, err4, err5); err != nil {
			return nil, fmt.Errorf("ar: member %q: bad header field: %w", name, err)
		}
		off += arHeaderLen
		if size < 0 || size > int64(len(b)-off) {
			return nil, fmt.Errorf("ar: member %q: size %d exceeds the archive", name, size)
		}
		out = append(out, arMember{Name: name, MTime: mtime, UID: uid, GID: gid, Mode: uint32(mode), Data: b[off : off+int(size)]})
		off += int(size)
		if size%2 == 1 {
			if off >= len(b) || b[off] != '\n' {
				return nil, fmt.Errorf("ar: member %q: missing padding byte", name)
			}
			off++
		}
	}
	return out, nil
}
