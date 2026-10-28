package airplay

// RAOP audio payloads are ALAC frames. The sender writes them in ALAC's uncompressed
// ("escape") mode, which needs no encoder: for a stereo 16-bit frame of 352 samples the
// bit stream (MSB first) is
//
//	3 bits   element type        1 (ID_CPE, channel pair)
//	4 bits   element instance    0
//	12 bits  unused              0
//	1 bit    partial frame       0 (the frame has the full 352 samples of the stream's fmtp)
//	2 bits   bytes shifted       0
//	1 bit    escape              1 (samples follow uncompressed)
//	352 × (16 bits left, 16 bits right), two's complement, big-endian
//	3 bits   element type        7 (ID_END)
//
// padded with zero bits to a byte boundary: 23 + 11264 + 3 bits = 1412 bytes. This is the
// layout Apple's ALAC encoder (EncodeStereoEscape) writes and its decoder reads.

// alacFrameBytes is the size of one escape frame of rtpFramesPerPacket stereo samples.
const alacFrameBytes = (23 + rtpFramesPerPacket*32 + 3 + 7) / 8

// bitWriter appends bits MSB first.
type bitWriter struct {
	buf []byte
	acc uint64
	n   uint // valid low bits in acc (< 8 between writes)
}

func (w *bitWriter) write(v uint32, bits uint) {
	w.acc = w.acc<<bits | uint64(v)&(1<<bits-1)
	w.n += bits
	for w.n >= 8 {
		w.n -= 8
		w.buf = append(w.buf, byte(w.acc>>w.n))
	}
}

// align pads with zero bits to the next byte boundary.
func (w *bitWriter) align() {
	if w.n > 0 {
		w.write(0, 8-w.n)
	}
}

// encodeALAC appends one escape frame carrying pcm (rtpFramesPerPacket frames of signed
// 16-bit little-endian interleaved stereo; shorter input is padded with silence).
func encodeALAC(dst, pcm []byte) []byte {
	w := bitWriter{buf: dst}
	w.write(1, 3)  // ID_CPE
	w.write(0, 4)  // element instance tag
	w.write(0, 12) // unused
	w.write(0, 1)  // partial frame
	w.write(0, 2)  // bytes shifted
	w.write(1, 1)  // escape: uncompressed
	for i := range rtpFramesPerPacket * 2 {
		var s uint32
		if 2*i+1 < len(pcm) {
			s = uint32(pcm[2*i]) | uint32(pcm[2*i+1])<<8 // little-endian in, big-endian out
		}
		w.write(s, 16)
	}
	w.write(7, 3) // ID_END
	w.align()
	return w.buf
}
