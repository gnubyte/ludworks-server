package mdns

import (
	"encoding/binary"
	"net/netip"
	"strings"
)

// Sections of a message being built.
const (
	secAnswer = iota + 1
	secAuthority
	secAdditional
)

// pktBuilder builds DNS messages for tests. With compress set, names are compressed against
// suffixes written earlier (as real responders do).
type pktBuilder struct {
	b        []byte
	compress bool
	names    map[string]int
	counts   [4]int // questions, answers, authority, additional
}

func newPkt(flags uint16, compress bool) *pktBuilder {
	p := &pktBuilder{b: make([]byte, headerLen), compress: compress, names: map[string]int{}}
	binary.BigEndian.PutUint16(p.b[2:], flags)
	return p
}

// newResponse starts an authoritative mDNS response (QR=1, AA=1).
func newResponse(compress bool) *pktBuilder { return newPkt(0x8400, compress) }

// L splits a dotted name into labels (no escapes).
func L(s string) []string { return strings.Split(s, ".") }

func (p *pktBuilder) name(labels []string) {
	for i := range labels {
		key := presentation(labels[i:])
		if off, ok := p.names[key]; ok && p.compress {
			p.b = binary.BigEndian.AppendUint16(p.b, 0xc000|uint16(off))
			return
		}
		if len(p.b) < 0x3fff {
			p.names[key] = len(p.b)
		}
		p.b = append(p.b, byte(len(labels[i])))
		p.b = append(p.b, labels[i]...)
	}
	p.b = append(p.b, 0)
}

func (p *pktBuilder) question(name []string, qtype, qclass uint16) *pktBuilder {
	p.name(name)
	p.b = binary.BigEndian.AppendUint16(p.b, qtype)
	p.b = binary.BigEndian.AppendUint16(p.b, qclass)
	p.counts[0]++
	return p
}

// rr writes a record header and the data produced by data, patching rdlength.
func (p *pktBuilder) rr(sec int, name []string, typ, class uint16, ttl uint32, data func()) *pktBuilder {
	p.name(name)
	p.b = binary.BigEndian.AppendUint16(p.b, typ)
	p.b = binary.BigEndian.AppendUint16(p.b, class)
	p.b = binary.BigEndian.AppendUint32(p.b, ttl)
	at := len(p.b)
	p.b = append(p.b, 0, 0)
	data()
	binary.BigEndian.PutUint16(p.b[at:], uint16(len(p.b)-at-2))
	p.counts[sec]++
	return p
}

func cls(flush bool) uint16 {
	if flush {
		return classIN | topBit
	}
	return classIN
}

func (p *pktBuilder) ptr(sec int, owner, target []string, ttl uint32) *pktBuilder {
	return p.rr(sec, owner, typePTR, classIN, ttl, func() { p.name(target) })
}

func (p *pktBuilder) srv(sec int, owner, target []string, port uint16, ttl uint32, flush bool) *pktBuilder {
	return p.rr(sec, owner, typeSRV, cls(flush), ttl, func() {
		p.b = append(p.b, 0, 0, 0, 0)
		p.b = binary.BigEndian.AppendUint16(p.b, port)
		p.name(target)
	})
}

func (p *pktBuilder) txt(sec int, owner []string, ttl uint32, flush bool, parts ...string) *pktBuilder {
	return p.rr(sec, owner, typeTXT, cls(flush), ttl, func() {
		for _, s := range parts {
			p.b = append(p.b, byte(len(s)))
			p.b = append(p.b, s...)
		}
	})
}

func (p *pktBuilder) addr(sec int, host []string, ip string, ttl uint32, flush bool) *pktBuilder {
	a := netip.MustParseAddr(ip)
	typ := uint16(typeA)
	if a.Is6() {
		typ = typeAAAA
	}
	return p.rr(sec, host, typ, cls(flush), ttl, func() { p.b = append(p.b, a.AsSlice()...) })
}

// raw appends bytes as-is (for malformed packets).
func (p *pktBuilder) raw(b ...byte) *pktBuilder { p.b = append(p.b, b...); return p }

func (p *pktBuilder) bytes() []byte {
	out := append([]byte(nil), p.b...)
	for i, n := range p.counts {
		binary.BigEndian.PutUint16(out[4+2*i:], uint16(n))
	}
	return out
}

// Realistic fixtures.

const ccID = "0123456789abcdef0123456789abcdef"

var (
	svcCast    = L("_googlecast._tcp.local")
	svcAirPlay = L("_airplay._tcp.local")
	svcRAOP    = L("_raop._tcp.local")
	ccInstance = append([]string{"Chromecast-" + ccID}, svcCast...)
	ccHost     = L("0123-abcd.local")
	apInstance = append([]string{"Living Room"}, svcAirPlay...)
	raInstance = append([]string{"AABBCCDDEEFF@Living Room"}, svcRAOP...)
	appleHost  = L("Living-Room.local")
)

// chromecastResponse is what a Chromecast answers to a PTR query for _googlecast._tcp.
func chromecastResponse(compress bool) []byte {
	return newResponse(compress).
		ptr(secAnswer, svcCast, ccInstance, 120).
		txt(secAdditional, ccInstance, 4500, true, "id="+ccID, "cd=AB12CD34EF56AB12CD34EF56AB12CD34", "rm=",
			"ve=05", "md=Chromecast", "ic=/setup/icon.png", "fn=Living Room TV", "ca=4101", "st=0",
			"bs=FA8FCA000000", "nf=1", "rs=").
		srv(secAdditional, ccInstance, ccHost, 8009, 120, true).
		addr(secAdditional, ccHost, "192.168.1.20", 120, true).
		bytes()
}

// appleResponse is an Apple TV's answer for both _airplay._tcp and _raop._tcp.
func appleResponse(compress bool) []byte {
	return newResponse(compress).
		ptr(secAnswer, svcAirPlay, apInstance, 4500).
		ptr(secAnswer, svcRAOP, raInstance, 4500).
		srv(secAdditional, apInstance, appleHost, 7000, 120, true).
		txt(secAdditional, apInstance, 4500, true, "acl=0", "deviceid=AA:BB:CC:DD:EE:FF",
			"features=0x4A7FDFD5,0x3C155FDE", "flags=0x244", "model=AppleTV11,1",
			"pk=5c1b2c4a7f3e0d9b8a6f4e3d2c1b0a99887766554433221100ffeeddccbbaa99", "srcvers=670.6.2", "vv=2").
		srv(secAdditional, raInstance, appleHost, 7000, 120, true).
		txt(secAdditional, raInstance, 4500, true, "cn=0,1,2,3", "da=true", "et=0,3,5", "ft=0x4A7FDFD5,0x3C155FDE",
			"md=0,1,2", "am=AppleTV11,1", "sf=0x244", "tp=UDP", "vn=65537", "vs=670.6.2", "pw=false").
		addr(secAdditional, appleHost, "192.168.1.30", 120, true).
		addr(secAdditional, appleHost, "fe80::1", 120, true).
		addr(secAdditional, appleHost, "2001:db8::30", 120, true).
		bytes()
}
