package probe

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Probing must not read proportionally to file size. These tests build
// 4 GiB sparse files (no disk space used on ext4/APFS/NTFS-sparse) with the
// index at the far end and count the bytes Probe reads. Set
// LUDWORKS_TEST_SCRATCH to choose where the files go (default: t.TempDir).

const sparseSize = 4 << 30

func scratchDir(tb testing.TB) string {
	if d := os.Getenv("LUDWORKS_TEST_SCRATCH"); d != "" {
		dir, err := os.MkdirTemp(d, "probe-sparse-")
		if err != nil {
			tb.Fatal(err)
		}
		tb.Cleanup(func() { os.RemoveAll(dir) })
		return dir
	}
	return tb.TempDir()
}

func writeSparse(tb testing.TB, path string, head []byte, tail []byte) {
	tb.Helper()
	f, err := os.Create(path)
	if err != nil {
		tb.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(head); err != nil {
		tb.Fatal(err)
	}
	if _, err := f.WriteAt(tail, sparseSize-int64(len(tail))); err != nil {
		tb.Fatal(err)
	}
}

// sparseMP4 is ftyp + a ~4 GiB 64-bit mdat + moov at the end.
func sparseMP4(tb testing.TB, dir string) string {
	ftyp := box("ftyp", []byte("isom"), u32(0x200))
	moov := box("moov", mvhd(0, 1000, 7_200_000),
		trak(tkhd(0, 1, 1920, 1080, identity), mdhd(24000, 0, "und"), hdlr("vide", ""), avc1(1920, 1080)),
		trak(tkhd(0, 2, 0, 0, identity), mdhd(48000, 0, "eng"), hdlr("soun", ""), mp4a(0x40)))
	mdatLen := uint64(sparseSize - len(ftyp) - len(moov))
	head := cat(ftyp, u32(1), []byte("mdat"), u64(mdatLen))
	p := filepath.Join(dir, "big.mp4")
	writeSparse(tb, p, head, moov)
	return p
}

// sparseMKV is a live-style Matroska file without Duration: a huge first
// cluster and a final cluster at the very end (found by the tail scan).
func sparseMKV(tb testing.TB, dir string) string {
	hdr := ebmlHeader("matroska")
	info := el(idInfo, uintEl(idTimecodeScale, 1_000_000))
	tracks := el(idTracks, videoTrack(1, "V_MPEG4/ISO/AVC", 1280, 720), audioTrack(2, "A_AAC", 48000, 2, "eng"))
	last := cluster(7_199_000, 0, 960)
	segID := cat(ebmlIDBytes(idSegment), []byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})
	before := len(hdr) + len(segID) + len(info) + len(tracks)
	clusterHdrLen := 4 + 8
	bigData := uint64(sparseSize - before - clusterHdrLen - len(last))
	bigCluster := cat(ebmlIDBytes(idCluster), []byte{0x01}, u64(bigData)[1:], uintEl(idTimecode, 0))
	p := filepath.Join(dir, "big.mkv")
	writeSparse(tb, p, cat(hdr, segID, info, tracks, bigCluster), last)
	return p
}

func probeCounting(tb testing.TB, path string) (*Info, int64) {
	f, err := os.Open(path)
	if err != nil {
		tb.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	cr := &countingReader{r: f}
	info, err := Probe(cr, st.Size(), filepath.Base(path))
	if err != nil {
		tb.Fatal(err)
	}
	return info, cr.n.Load()
}

func TestProbeSparse4GB(t *testing.T) {
	if testing.Short() {
		t.Skip("creates 4 GiB sparse files")
	}
	if runtime.GOOS == "windows" && os.Getenv("LUDWORKS_TEST_SCRATCH") == "" {
		t.Skip("NTFS files are not sparse by default; set LUDWORKS_TEST_SCRATCH to run")
	}
	dir := scratchDir(t)
	info, n := probeCounting(t, sparseMP4(t, dir))
	if !approx(info.DurationSeconds, 7200) || info.VideoCodec != "h264" || info.Width != 1920 {
		t.Fatalf("mp4: %+v", info)
	}
	if n > 64<<10 {
		t.Fatalf("mp4: read %d bytes", n)
	}
	t.Logf("mp4: 4 GiB file, read %d bytes", n)

	info, n = probeCounting(t, sparseMKV(t, dir))
	if !approx(info.DurationSeconds, 7199.96) || info.VideoCodec != "h264" || info.AudioCodec != "aac" {
		t.Fatalf("mkv: %+v", info)
	}
	if n > 3<<20 {
		t.Fatalf("mkv: read %d bytes", n)
	}
	t.Logf("mkv: 4 GiB file, read %d bytes (tail scan)", n)
}

func BenchmarkProbeSparse4GB(b *testing.B) {
	dir := scratchDir(b)
	for _, c := range []struct {
		name string
		make func(testing.TB, string) string
	}{{"mp4-moov-at-end", sparseMP4}, {"mkv-no-duration", sparseMKV}} {
		path := c.make(b, dir)
		b.Run(c.name, func(b *testing.B) {
			var n int64
			for b.Loop() {
				_, n = probeCounting(b, path)
			}
			b.ReportMetric(float64(n), "bytes-read/op")
		})
	}
}
