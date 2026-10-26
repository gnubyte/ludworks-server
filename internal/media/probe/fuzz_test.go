package probe

import (
	"bytes"
	"math"
	"testing"
	"time"
)

// checkProbe runs Probe on arbitrary bytes and asserts the safety
// properties: no panic, bounded reads, bounded time, sane output.
func checkProbe(t *testing.T, b []byte, name string) {
	cr := &countingReader{r: bytes.NewReader(b)}
	start := time.Now()
	info, err := Probe(cr, int64(len(b)), name)
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("probe took %v on %d bytes", el, len(b))
	}
	if n := cr.n.Load(); n > MaxProbeRead {
		t.Fatalf("read %d bytes, over budget", n)
	}
	if err != nil {
		return
	}
	if math.IsNaN(info.DurationSeconds) || info.DurationSeconds < 0 || info.Width < 0 || info.Height < 0 {
		t.Fatalf("insane info %+v", info)
	}
	for i, s := range info.Subtitles {
		if s.Index != i {
			t.Fatalf("subtitle index %d at %d", s.Index, i)
		}
	}
}

func FuzzMP4(f *testing.F) {
	f.Add(buildMP4(mp4Opts{timescale: 1000, duration: 5000, videoEntry: avc1(640, 360), audioEntry: mp4a(0x40), subs: true}))
	f.Add(buildMP4(mp4Opts{mvhdV: 1, timescale: 1000, duration: 5000, moovAtEnd: true, mdatSize: 64, videoEntry: visualEntry("encv", 64, 64, box("sinf", box("frma", []byte("hvc1"))))}))
	f.Add(fragmentedMP4(false, true, 3))
	f.Add(fragmentedMP4(false, false, 2))
	f.Fuzz(func(t *testing.T, b []byte) { checkProbe(t, b, "f.mp4") })
}

func FuzzMKV(f *testing.F) {
	f.Add(cat(ebmlHeader("webm"), el(idSegment, el(idInfo, f64El(idDuration, 1000)), el(idTracks, videoTrack(1, "V_VP9", 64, 64)), cluster(0, 0, 10))))
	f.Add(cat(ebmlHeader("matroska"), elUnknown(idSegment, el(idTracks, audioTrack(1, "A_OPUS", 48000, 2, "eng")), elUnknown(idCluster, uintEl(idTimecode, 5), simpleBlock(1, 3)))))
	f.Fuzz(func(t *testing.T, b []byte) { checkProbe(t, b, "f.mkv") })
}

func FuzzAVI(f *testing.F) {
	f.Add(buildAVI(true))
	f.Add(buildWAV(0))
	f.Fuzz(func(t *testing.T, b []byte) { checkProbe(t, b, "f.avi") })
}

func FuzzAudioAndTS(f *testing.F) {
	f.Add(buildFLAC(44100, 2, 1000, true), "a.flac")
	f.Add(cat(mp3Frame(nil), mp3Frame(nil)), "a.mp3")
	f.Add(buildTS(1), "a.ts")
	opus := cat([]byte("OpusHead"), []byte{1, 2}, le16(312), le32(48000), le16(0), []byte{0})
	f.Add(cat(oggPageBytes(1, 0, true, opus), oggPageBytes(1, 96000, false, zeros(4))), "a.ogg")
	f.Fuzz(func(t *testing.T, b []byte, name string) { checkProbe(t, b, name) })
}
