package airplay

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

// bitReader reads MSB-first bits; written for the tests, independently of bitWriter.
type bitReader struct {
	b   []byte
	pos int // bit position
}

func (r *bitReader) read(n int) (uint32, error) {
	if r.pos+n > 8*len(r.b) {
		return 0, errors.New("read past the end of the frame")
	}
	var v uint32
	for range n {
		bit := r.b[r.pos/8] >> (7 - r.pos%8) & 1
		v = v<<1 | uint32(bit)
		r.pos++
	}
	return v, nil
}

// decodeALACEscape decodes a stereo 16-bit ALAC frame the way Apple's ALACDecoder reads one
// (element tag, instance tag, 12 unused bits, partial-frame/shift/escape bits, samples,
// then the next element tag, which must be ID_END) and returns little-endian PCM. Only
// uncompressed frames are supported, which is all the sender writes.
func decodeALACEscape(frame []byte, frameLength int) ([]byte, error) {
	r := &bitReader{b: frame}
	get := func(n int) uint32 {
		v, err := r.read(n)
		if err != nil {
			panic(err)
		}
		return v
	}
	var out []byte
	var perr error
	func() {
		defer func() {
			if e := recover(); e != nil {
				perr = fmt.Errorf("%v", e)
			}
		}()
		if tag := get(3); tag != 1 {
			perr = fmt.Errorf("element tag %d, want ID_CPE", tag)
			return
		}
		get(4) // element instance tag
		if unused := get(12); unused != 0 {
			perr = fmt.Errorf("unused header bits %#x", unused)
			return
		}
		partial, shift, escape := get(1), get(2), get(1)
		if shift != 0 || escape != 1 {
			perr = fmt.Errorf("shift %d escape %d: not an uncompressed 16-bit frame", shift, escape)
			return
		}
		n := frameLength
		if partial == 1 {
			n = int(get(16)<<16 | get(16))
		}
		for range n {
			for range 2 {
				s := int16(get(16))
				out = binary.LittleEndian.AppendUint16(out, uint16(s))
			}
		}
		if end := get(3); end != 7 {
			perr = fmt.Errorf("element tag %d after the samples, want ID_END", end)
			return
		}
		// The rest must be zero padding inside the last byte.
		if rest := 8*len(frame) - r.pos; rest >= 8 {
			perr = fmt.Errorf("%d bits left after ID_END", rest)
		} else if rest > 0 && get(rest) != 0 {
			perr = errors.New("padding bits are not zero")
		}
	}()
	return out, perr
}

// bitString builds the expected frame from the format description as a string of '0'/'1'.
func expectedALACBits(samples []int16) string {
	var b strings.Builder
	b.WriteString("001")          // ID_CPE
	b.WriteString("0000")         // element instance tag
	b.WriteString("000000000000") // unused
	b.WriteString("0")            // partial frame: no
	b.WriteString("00")           // bytes shifted
	b.WriteString("1")            // escape (uncompressed)
	for _, s := range samples {
		fmt.Fprintf(&b, "%016b", uint16(s))
	}
	b.WriteString("111") // ID_END
	for b.Len()%8 != 0 {
		b.WriteByte('0')
	}
	return b.String()
}

func packBits(s string) []byte {
	out := make([]byte, len(s)/8)
	for i := range len(s) {
		if s[i] == '1' {
			out[i/8] |= 0x80 >> (i % 8)
		}
	}
	return out
}

func pcmOf(samples []int16) []byte {
	var b []byte
	for _, s := range samples {
		b = binary.LittleEndian.AppendUint16(b, uint16(s))
	}
	return b
}

func TestALACFrameBitLayout(t *testing.T) {
	samples := make([]int16, 2*rtpFramesPerPacket)
	for i := range rtpFramesPerPacket {
		samples[2*i] = int16(i * 93)       // left
		samples[2*i+1] = int16(-1 - i*179) // right, negative
	}
	samples[0], samples[1] = -32768, 32767 // extremes first
	got := encodeALAC(nil, pcmOf(samples))
	want := packBits(expectedALACBits(samples))
	if len(got) != alacFrameBytes || alacFrameBytes != 1412 {
		t.Fatalf("frame is %d bytes (const %d), want 1412", len(got), alacFrameBytes)
	}
	if !bytes.Equal(got, want) {
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("byte %d: got %08b want %08b", i, got[i], want[i])
			}
		}
	}
	// The header in bytes: 001 0000 0000|0000 0000|0000 0 00 1 then the MSB of the
	// first sample (-32768 = 0x8000): 0x20 0x00 0x03.
	if got[0] != 0x20 || got[1] != 0x00 || got[2] != 0x03 {
		t.Errorf("header bytes % x", got[:3])
	}
	// 23 header bits and 11264 sample bits put ID_END (111) at bits 11287–11289: the last
	// bit of byte 1410 and the first two of byte 1411, followed by six zero pad bits.
	if got[1410]&1 != 1 || got[1411] != 0xC0 {
		t.Errorf("tail bytes %08b %08b", got[1410], got[1411])
	}
}

func TestALACRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for _, frames := range []int{rtpFramesPerPacket, 100, 1, 0} {
		pcm := make([]byte, frames*pcmFrameBytes)
		for i := range pcm {
			pcm[i] = byte(rng.Uint32())
		}
		frame := encodeALAC([]byte{0xAA}, pcm) // appends after existing bytes
		if frame[0] != 0xAA {
			t.Fatal("encodeALAC overwrote dst")
		}
		got, err := decodeALACEscape(frame[1:], rtpFramesPerPacket)
		if err != nil {
			t.Fatalf("%d frames: %v", frames, err)
		}
		want := append(pcm, make([]byte, pcmPacketBytes-len(pcm))...) // padded with silence
		if !bytes.Equal(got, want) {
			t.Fatalf("%d frames: PCM differs after the round trip", frames)
		}
	}
}

func TestALACDecoderRejectsDamage(t *testing.T) {
	frame := encodeALAC(nil, make([]byte, pcmPacketBytes))
	for name, mutate := range map[string]func([]byte){
		"element tag": func(b []byte) { b[0] ^= 0x80 },
		"unused bits": func(b []byte) { b[1] |= 0x10 },
		"no escape":   func(b []byte) { b[2] &^= 0x02 },
		"end tag":     func(b []byte) { b[len(b)-1] = 0 },
	} {
		b := bytes.Clone(frame)
		mutate(b)
		if _, err := decodeALACEscape(b, rtpFramesPerPacket); err == nil {
			t.Errorf("%s: damaged frame decoded", name)
		}
	}
}
