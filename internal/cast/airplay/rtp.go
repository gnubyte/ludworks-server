package airplay

import (
	"encoding/binary"
	"time"
)

// RAOP packets. Audio goes to the receiver's server port as RTP (version 2, payload type
// 0x60, the marker bit on the first packet after RECORD or FLUSH); timing, sync and resend
// packets use an 8-byte RTP-like header without SSRC whose second byte is the payload type
// with the marker bit set:
//
//	timing request  0xD2 (receiver → our timing port), 32 bytes:
//	                header(8) | reference NTP(8) | receive NTP(8) | send NTP(8)
//	timing reply    0xD3 (to the requester), 32 bytes:
//	                header(8) | origin = request's send NTP(8) | our receive NTP(8) | our send NTP(8)
//	sync            0xD4 (to the receiver's control port, once per second), 20 bytes:
//	                0x80 (0x90 with the extension bit on the first after RECORD/FLUSH) 0xD4 0x00 0x07 |
//	                RTP time playing now (= next RTP time − latency)(4) | NTP now(8) | next RTP time(4)
//	resend request  0xD5 (receiver → our control port), 8 bytes: header(4) | first seq(2) | count(2)
//	resend reply    0xD6 (to the receiver's control port): 0x80 0xD6 seq(2) | the original audio packet
//
// NTP timestamps are 32.32 fixed-point seconds since 1900-01-01.

const (
	rtpSampleRate      = 44100
	rtpFramesPerPacket = 352
	pcmFrameBytes      = 4 // 16-bit stereo
	pcmPacketBytes     = rtpFramesPerPacket * pcmFrameBytes
	rtpHeaderBytes     = 12

	ptAudio       = 0x60
	ptTimingReq   = 0x52
	ptTimingReply = 0x53
	ptSync        = 0x54
	ptResendReq   = 0x55
	ptResendReply = 0x56
	rtpMarker     = 0x80
)

// ntpTime is an NTP timestamp (32.32 fixed point seconds since 1900).
type ntpTime uint64

// ntpUnixOffset is the number of seconds from 1900-01-01 to 1970-01-01.
const ntpUnixOffset = 2208988800

func ntpFromTime(t time.Time) ntpTime {
	secs := uint64(t.Unix() + ntpUnixOffset)
	frac := (uint64(t.Nanosecond()) << 32) / uint64(time.Second)
	return ntpTime(secs<<32 | frac)
}

func (n ntpTime) time() time.Time {
	secs := int64(n>>32) - ntpUnixOffset
	nanos := (uint64(n&0xFFFFFFFF) * uint64(time.Second)) >> 32
	return time.Unix(secs, int64(nanos))
}

// putAudioHeader writes the 12-byte RTP header of an audio packet.
func putAudioHeader(b []byte, marker bool, seq uint16, ts, ssrc uint32) {
	b[0] = 0x80
	b[1] = ptAudio
	if marker {
		b[1] |= rtpMarker
	}
	binary.BigEndian.PutUint16(b[2:], seq)
	binary.BigEndian.PutUint32(b[4:], ts)
	binary.BigEndian.PutUint32(b[8:], ssrc)
}

// syncPacket builds a sync packet: at NTP time now the receiver should be playing RTP time
// next−latency while the sender is at next.
func syncPacket(first bool, now ntpTime, next, latency uint32) []byte {
	b := make([]byte, 20)
	b[0] = 0x80
	if first {
		b[0] = 0x90
	}
	b[1] = ptSync | rtpMarker
	b[3] = 0x07
	binary.BigEndian.PutUint32(b[4:], next-latency)
	binary.BigEndian.PutUint64(b[8:], uint64(now))
	binary.BigEndian.PutUint32(b[16:], next)
	return b
}

// timingReply answers a timing request received at recv; xmit is our send time. ok is
// false for anything that is not a well-formed timing request.
func timingReply(req []byte, recv, xmit ntpTime) (reply []byte, ok bool) {
	if len(req) != 32 || req[1] != ptTimingReq|rtpMarker {
		return nil, false
	}
	b := make([]byte, 32)
	b[0] = 0x80
	b[1] = ptTimingReply | rtpMarker
	copy(b[2:4], req[2:4])
	copy(b[8:16], req[24:32])
	binary.BigEndian.PutUint64(b[16:], uint64(recv))
	binary.BigEndian.PutUint64(b[24:], uint64(xmit))
	return b, true
}

// parseResendRequest reads a resend request.
func parseResendRequest(b []byte) (first, count uint16, ok bool) {
	if len(b) < 8 || b[1] != ptResendReq|rtpMarker {
		return 0, 0, false
	}
	return binary.BigEndian.Uint16(b[4:]), binary.BigEndian.Uint16(b[6:]), true
}

// resendReply wraps a retransmitted audio packet.
func resendReply(pkt []byte) []byte {
	b := make([]byte, 4+len(pkt))
	b[0] = 0x80
	b[1] = ptResendReply | rtpMarker
	copy(b[2:4], pkt[2:4])
	copy(b[4:], pkt)
	return b
}

// sentPacket is an audio packet kept for retransmission and for resuming after a pause.
type sentPacket struct {
	seq    uint16
	pkt    []byte // the packet as sent (header and possibly encrypted payload)
	pcm    []byte // its PCM (pcmPacketBytes)
	frame  int64  // media frame of its first sample (relative to the item's source start)
	frames int    // real media frames in it (fewer at the end; 0 for padding)
}

// packetRing keeps the most recent packets by sequence number.
type packetRing struct {
	slots []sentPacket
	used  []bool
}

const ringPackets = 512 // ≈ 4 s of audio

func newPacketRing() *packetRing {
	return &packetRing{slots: make([]sentPacket, ringPackets), used: make([]bool, ringPackets)}
}

func (r *packetRing) put(p sentPacket) {
	i := int(p.seq) % len(r.slots)
	r.slots[i], r.used[i] = p, true
}

func (r *packetRing) get(seq uint16) (sentPacket, bool) {
	i := int(seq) % len(r.slots)
	if !r.used[i] || r.slots[i].seq != seq {
		return sentPacket{}, false
	}
	return r.slots[i], true
}

func (r *packetRing) reset() {
	clear(r.used)
}
