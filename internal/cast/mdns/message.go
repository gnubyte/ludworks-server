package mdns

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Wire limits (RFC 1035, RFC 6762) and the caps applied to untrusted packets.
const (
	headerLen    = 12
	maxPacket    = 9000 // RFC 6762 §17: mDNS messages may be up to 9000 bytes; larger ones are dropped
	maxRecords   = 256  // resource records kept per message (answer + authority + additional)
	maxQuestions = 64   // questions kept per message (responses rarely carry any)
	maxNameWire  = 255  // encoded name length: labels with their length bytes plus the root byte
	maxLabel     = 63
	maxJumps     = 64  // compression pointers followed while decoding one name
	maxTXTParts  = 256 // character-strings read from one TXT record
	maxTXTPairs  = 64  // key/value pairs kept per instance
	maxTXTBytes  = 8 << 10
	maxTXTValue  = 255
)

// DNS constants used here.
const (
	typeA    = 1
	typePTR  = 12
	typeTXT  = 16
	typeAAAA = 28
	typeSRV  = 33

	classIN = 1
	// topBit is the cache-flush bit of a record's class and the unicast-response ("QU") bit of
	// a question's class (RFC 6762 §10.2, §5.4).
	topBit    = 0x8000
	classMask = 0x7fff

	flagQR     = 0x8000
	opcodeMask = 0x7800
	rcodeMask  = 0x000f
)

var (
	errShort     = errors.New("mdns: message truncated")
	errTooLarge  = errors.New("mdns: message too large")
	errCounts    = errors.New("mdns: section counts exceed the message size")
	errLabelType = errors.New("mdns: unsupported label type")
	errPointer   = errors.New("mdns: invalid compression pointer")
	errNameLong  = errors.New("mdns: name too long")
)

// message is a decoded DNS message.
type message struct {
	id, flags uint16
	questions []question
	records   []record // answer, authority and additional sections, in order
}

// question is one entry of the question section.
type question struct {
	name   []string // labels, verbatim (no escaping on the wire)
	qtype  uint16
	qclass uint16 // without the QU bit
	qu     bool   // unicast response requested
}

// record is a resource record of one of the types the browser uses (A, AAAA, PTR, SRV, TXT).
type record struct {
	name  []string
	rtype uint16
	class uint16 // without the cache-flush bit
	flush bool   // cache-flush bit: this record replaces older ones of the same name and type
	ttl   uint32

	target []string   // PTR target / SRV target
	port   uint16     // SRV
	txt    [][]byte   // TXT character-strings (views into the packet: copy before keeping)
	addr   netip.Addr // A / AAAA
}

func be16(b []byte) uint16 { return binary.BigEndian.Uint16(b) }
func be32(b []byte) uint32 { return binary.BigEndian.Uint32(b) }

// parseMessage decodes an untrusted DNS message. Records whose extent is known (from their
// rdlength) but whose data is malformed, or of a type or class the browser does not use, are
// skipped. When the framing itself is broken the records decoded so far are returned together
// with the error. A nil message means the packet is unusable.
func parseMessage(b []byte) (*message, error) {
	if len(b) > maxPacket {
		return nil, errTooLarge
	}
	if len(b) < headerLen {
		return nil, errShort
	}
	m := &message{id: be16(b[0:]), flags: be16(b[2:])}
	qd := int(be16(b[4:]))
	rr := int(be16(b[6:])) + int(be16(b[8:])) + int(be16(b[10:]))
	// A question takes at least 5 bytes (root name, type, class), a record at least 11: counts
	// that cannot fit are lies, and rejecting them early bounds every loop below.
	if qd*5+rr*11 > len(b)-headerLen {
		return nil, errCounts
	}
	off := headerLen
	for i := 0; i < qd; i++ {
		name, next, err := readName(b, off)
		if err != nil {
			return m, err
		}
		if next+4 > len(b) {
			return m, errShort
		}
		cls := be16(b[next+2:])
		if len(m.questions) < maxQuestions {
			m.questions = append(m.questions, question{name: name, qtype: be16(b[next:]), qclass: cls & classMask, qu: cls&topBit != 0})
		}
		off = next + 4
	}
	for i := 0; i < rr && i < maxRecords; i++ {
		r, next, ok, err := readRecord(b, off)
		if err != nil {
			return m, err
		}
		off = next
		if ok {
			m.records = append(m.records, r)
		}
	}
	return m, nil
}

// readRecord decodes the record at off. ok is false for records that are skipped (other
// types or classes, malformed data); next is then still the offset of the following record.
func readRecord(b []byte, off int) (r record, next int, ok bool, err error) {
	name, off, err := readName(b, off)
	if err != nil {
		return r, 0, false, err
	}
	if off+10 > len(b) {
		return r, 0, false, errShort
	}
	r.name = name
	r.rtype = be16(b[off:])
	cls := be16(b[off+2:])
	r.class, r.flush = cls&classMask, cls&topBit != 0
	r.ttl = be32(b[off+4:])
	rdlen := int(be16(b[off+8:]))
	off += 10
	next = off + rdlen
	if next > len(b) {
		return r, 0, false, errShort
	}
	if r.class != classIN {
		return r, next, false, nil
	}
	rd := b[off:next]
	switch r.rtype {
	case typeA:
		if rdlen != 4 {
			return r, next, false, nil
		}
		r.addr = netip.AddrFrom4([4]byte(rd))
	case typeAAAA:
		if rdlen != 16 {
			return r, next, false, nil
		}
		r.addr = netip.AddrFrom16([16]byte(rd))
	case typePTR:
		t, end, err := readName(b, off)
		if err != nil || end > next {
			return r, next, false, nil
		}
		r.target = t
	case typeSRV:
		if rdlen < 7 { // priority, weight, port, at least the root label
			return r, next, false, nil
		}
		r.port = be16(rd[4:])
		t, end, err := readName(b, off+6)
		if err != nil || end > next || r.port == 0 {
			return r, next, false, nil
		}
		r.target = t
	case typeTXT:
		parts, good := readTXT(rd)
		if !good {
			return r, next, false, nil
		}
		r.txt = parts
	default:
		return r, next, false, nil
	}
	return r, next, true, nil
}

// readName decodes the (possibly compressed) name at off and returns its labels and the
// offset just after the name's in-place bytes. Compression pointers must point strictly before
// the segment that contains them (so decoding always terminates), at most maxJumps are
// followed, labels use the 00 type only and the whole name stays within maxNameWire bytes.
func readName(b []byte, off int) ([]string, int, error) {
	var labels []string
	wire := 1 // the root byte
	end := -1 // offset after the in-place part, set at the first pointer or the root
	limit := off
	jumps := 0
	for {
		if off >= len(b) {
			return nil, 0, errShort
		}
		c := int(b[off])
		switch c & 0xc0 {
		case 0x00:
			if c == 0 {
				if end < 0 {
					end = off + 1
				}
				return labels, end, nil
			}
			if off+1+c > len(b) {
				return nil, 0, errShort
			}
			if wire += 1 + c; wire > maxNameWire {
				return nil, 0, errNameLong
			}
			labels = append(labels, string(b[off+1:off+1+c]))
			off += 1 + c
		case 0xc0:
			if off+1 >= len(b) {
				return nil, 0, errShort
			}
			ptr := int(be16(b[off:]) & 0x3fff)
			if ptr >= limit {
				return nil, 0, errPointer
			}
			if jumps++; jumps > maxJumps {
				return nil, 0, errPointer
			}
			if end < 0 {
				end = off + 2
			}
			limit, off = ptr, ptr
		default: // 0x40 (extended label types) and 0x80 are not used by mDNS
			return nil, 0, errLabelType
		}
	}
}

// readTXT splits TXT rdata into its character-strings.
func readTXT(rd []byte) ([][]byte, bool) {
	var parts [][]byte
	for i := 0; i < len(rd); {
		l := int(rd[i])
		i++
		if i+l > len(rd) {
			return nil, false
		}
		if len(parts) < maxTXTParts {
			parts = append(parts, rd[i:i+l])
		}
		i += l
	}
	return parts, true
}

// txtMap turns TXT character-strings into key/value pairs (RFC 6763 §6): keys are printable
// ASCII without '=', lower-cased; a string without '=' is a key with an empty value; the first
// occurrence of a key wins. Values are sanitised (valid UTF-8, no control characters, at most
// maxTXTValue bytes); at most maxTXTPairs pairs and maxTXTBytes bytes are kept.
func txtMap(parts [][]byte) map[string]string {
	out := map[string]string{}
	total := 0
	for _, p := range parts {
		if len(p) == 0 {
			continue
		}
		k, v, _ := strings.Cut(string(p), "=")
		if !validTXTKey(k) {
			continue
		}
		k = lowerASCII(k)
		if _, dup := out[k]; dup {
			continue
		}
		v = sanitize(v, maxTXTValue)
		if len(out) >= maxTXTPairs || total+len(k)+len(v) > maxTXTBytes {
			break
		}
		total += len(k) + len(v)
		out[k] = v
	}
	return out
}

func validTXTKey(k string) bool {
	if k == "" {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x20 || k[i] > 0x7e {
			return false
		}
	}
	return true
}

// sanitize makes untrusted text safe to keep and display: invalid UTF-8 and control
// characters become U+FFFD, and the result is cut to at most max bytes on a rune boundary.
func sanitize(s string, max int) string {
	clean := true
	for _, r := range s {
		if r == utf8.RuneError || unicode.IsControl(r) {
			clean = false
			break
		}
	}
	if clean && len(s) <= max {
		return s
	}
	var sb strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		i += n
		if r == utf8.RuneError || unicode.IsControl(r) {
			r = utf8.RuneError
		}
		if sb.Len()+utf8.RuneLen(r) > max {
			break
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

// buildQuery encodes a query message (ID 0 as RFC 6762 §18.1 asks for multicast queries;
// names uncompressed).
func buildQuery(qs []question) []byte {
	b := make([]byte, headerLen, 512)
	binary.BigEndian.PutUint16(b[4:], uint16(len(qs)))
	for _, q := range qs {
		b = appendName(b, q.name)
		cls := q.qclass
		if q.qu {
			cls |= topBit
		}
		b = binary.BigEndian.AppendUint16(b, q.qtype)
		b = binary.BigEndian.AppendUint16(b, cls)
	}
	return b
}

// appendName appends labels in uncompressed wire format. Callers only pass labels that were
// validated (configured service names, or names decoded by readName).
func appendName(b []byte, labels []string) []byte {
	for _, l := range labels {
		b = append(b, byte(len(l)))
		b = append(b, l...)
	}
	return append(b, 0)
}

// splitName splits a presentation-format name without escapes ("_googlecast._tcp.local")
// into labels; nil when a label is empty or too long.
func splitName(s string) []string {
	s = strings.TrimSuffix(s, ".")
	if s == "" {
		return nil
	}
	labels := strings.Split(s, ".")
	wire := 1
	for _, l := range labels {
		if l == "" || len(l) > maxLabel {
			return nil
		}
		wire += 1 + len(l)
	}
	if wire > maxNameWire {
		return nil
	}
	return labels
}

// lowerASCII lower-cases ASCII letters only (DNS names compare case-insensitively in ASCII,
// RFC 4343; other bytes are compared exactly).
func lowerASCII(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if b[j] >= 'A' && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

// labelsEqual compares names case-insensitively (ASCII).
func labelsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) || lowerASCII(a[i]) != lowerASCII(b[i]) {
			return false
		}
	}
	return true
}

// presentation renders labels in canonical lower-case presentation format without the
// trailing dot: '.' and '\' inside a label are escaped and bytes outside printable ASCII
// (space included) become \DDD, so distinct names never render alike.
func presentation(labels []string) string {
	var sb strings.Builder
	for i, l := range labels {
		if i > 0 {
			sb.WriteByte('.')
		}
		for j := 0; j < len(l); j++ {
			c := l[j]
			switch {
			case c >= 'A' && c <= 'Z':
				sb.WriteByte(c + 'a' - 'A')
			case c == '.' || c == '\\':
				sb.WriteByte('\\')
				sb.WriteByte(c)
			case c > 0x20 && c < 0x7f:
				sb.WriteByte(c)
			default:
				sb.WriteByte('\\')
				d := strconv.Itoa(int(c))
				sb.WriteString(strings.Repeat("0", 3-len(d)))
				sb.WriteString(d)
			}
		}
	}
	return sb.String()
}

// isLocal reports whether a name is in the ".local." domain.
func isLocal(labels []string) bool {
	return len(labels) >= 2 && lowerASCII(labels[len(labels)-1]) == "local"
}
