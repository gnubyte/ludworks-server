package imaging

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func encodePNG(t testing.TB, m image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, m); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func encodeJPEG(t testing.TB, m image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, m, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func thumb(t *testing.T, data []byte, maxEdge int) image.Image {
	t.Helper()
	var out bytes.Buffer
	if err := Thumbnail(bytes.NewReader(data), maxEdge, &out); err != nil {
		t.Fatalf("Thumbnail: %v", err)
	}
	m, err := jpeg.Decode(&out)
	if err != nil {
		t.Fatalf("output is not a JPEG: %v", err)
	}
	return m
}

func near(t *testing.T, what string, got color.Color, r, g, b uint8, tol int) {
	t.Helper()
	gr, gg, gb, _ := got.RGBA()
	d := func(a uint32, b uint8) int { return int(math.Abs(float64(int(a>>8) - int(b)))) }
	if d(gr, r) > tol || d(gg, g) > tol || d(gb, b) > tol {
		t.Fatalf("%s: got (%d,%d,%d) want (%d,%d,%d) ±%d", what, gr>>8, gg>>8, gb>>8, r, g, b, tol)
	}
}

func halves(w, h int, left, right color.NRGBA) *image.NRGBA {
	m := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if x < w/2 {
				m.SetNRGBA(x, y, left)
			} else {
				m.SetNRGBA(x, y, right)
			}
		}
	}
	return m
}

var (
	red  = color.NRGBA{255, 0, 0, 255}
	blue = color.NRGBA{0, 0, 255, 255}
)

func TestThumbnailDownscaleIsLinearAreaAverage(t *testing.T) {
	// A 1-pixel black/white checkerboard averages to 50% *linear* light,
	// which is sRGB ≈ 188 — a gamma-naive filter would give 128.
	m := image.NewGray(image.Rect(0, 0, 200, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 200; x++ {
			if (x+y)%2 == 0 {
				m.SetGray(x, y, color.Gray{255})
			}
		}
	}
	out := thumb(t, encodePNG(t, m), 50)
	if b := out.Bounds(); b.Dx() != 50 || b.Dy() != 25 {
		t.Fatalf("size %v", b)
	}
	for _, p := range []image.Point{{0, 0}, {25, 12}, {49, 24}} {
		near(t, "checker", out.At(p.X, p.Y), 188, 188, 188, 3)
	}
}

func TestThumbnailHalvesAndAspect(t *testing.T) {
	out := thumb(t, encodePNG(t, halves(400, 200, red, blue)), 100)
	if b := out.Bounds(); b.Dx() != 100 || b.Dy() != 50 {
		t.Fatalf("size %v", b)
	}
	near(t, "left", out.At(10, 25), 255, 0, 0, 12)
	near(t, "right", out.At(90, 25), 0, 0, 255, 12)

	// Extreme aspect ratios keep at least one pixel.
	out = thumb(t, encodePNG(t, image.NewGray(image.Rect(0, 0, 1000, 3))), 100)
	if b := out.Bounds(); b.Dx() != 100 || b.Dy() != 1 {
		t.Fatalf("thin size %v", b)
	}
	// Portrait.
	out = thumb(t, encodePNG(t, image.NewGray(image.Rect(0, 0, 300, 900))), 160)
	if b := out.Bounds(); b.Dx() != 53 || b.Dy() != 160 {
		t.Fatalf("portrait size %v", b)
	}
}

func TestThumbnailNeverUpscales(t *testing.T) {
	out := thumb(t, encodePNG(t, halves(30, 20, red, blue)), 640)
	if b := out.Bounds(); b.Dx() != 30 || b.Dy() != 20 {
		t.Fatalf("upscaled to %v", b)
	}
}

func TestThumbnailTransparencyOnWhite(t *testing.T) {
	m := image.NewNRGBA(image.Rect(0, 0, 10, 10))
	for i := range m.Pix {
		if i%4 == 3 {
			m.Pix[i] = 128 // 50% black over white → 50% linear → sRGB ≈ 188
		}
	}
	near(t, "half alpha", thumb(t, encodePNG(t, m), 10).At(5, 5), 188, 188, 188, 3)

	// GIF with a transparent index: first frame only, transparency → white.
	pal := color.Palette{color.NRGBA{0, 0, 0, 0}, color.NRGBA{0, 255, 0, 255}}
	g := image.NewPaletted(image.Rect(0, 0, 8, 8), pal)
	for x := 4; x < 8; x++ {
		for y := 0; y < 8; y++ {
			g.SetColorIndex(x, y, 1)
		}
	}
	g2 := image.NewPaletted(image.Rect(0, 0, 8, 8), pal) // second frame all transparent
	var gb bytes.Buffer
	if err := gif.EncodeAll(&gb, &gif.GIF{Image: []*image.Paletted{g, g2}, Delay: []int{10, 10}}); err != nil {
		t.Fatal(err)
	}
	out := thumb(t, gb.Bytes(), 8)
	near(t, "gif transparent", out.At(1, 4), 255, 255, 255, 6)
	near(t, "gif green", out.At(6, 4), 0, 255, 0, 12)
}

// exifJPEG builds a JPEG whose APP1 segment carries the given orientation.
func exifJPEG(t testing.TB, m image.Image, orientation uint16, bo binary.ByteOrder) []byte {
	t.Helper()
	j := encodeJPEG(t, m)
	var tiff bytes.Buffer
	if bo == binary.BigEndian {
		tiff.WriteString("MM")
	} else {
		tiff.WriteString("II")
	}
	w := func(v any) { _ = binary.Write(&tiff, bo, v) }
	w(uint16(42))
	w(uint32(8))
	w(uint16(2)) // two entries: an unrelated tag first, then orientation
	w(uint16(0x010F))
	w(uint16(2))
	w(uint32(4))
	tiff.WriteString("Ack\x00")
	w(uint16(0x0112))
	w(uint16(3))
	w(uint32(1))
	w(orientation)
	w(uint16(0))
	w(uint32(0)) // next IFD
	payload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	seg := []byte{0xFF, 0xE1, byte((len(payload) + 2) >> 8), byte(len(payload) + 2)}
	out := append([]byte{0xFF, 0xD8}, seg...)
	out = append(out, payload...)
	return append(out, j[2:]...)
}

func TestThumbnailEXIFOrientation(t *testing.T) {
	src := halves(40, 20, red, blue) // red left, blue right
	for _, bo := range []binary.ByteOrder{binary.BigEndian, binary.LittleEndian} {
		cases := []struct {
			o                uint16
			w, h             int
			redAt, blueAt    image.Point
			sizeW, sizeH     int
			wantOrientParsed int
		}{
			{1, 40, 20, image.Pt(5, 10), image.Pt(35, 10), 40, 20, 1},
			{2, 40, 20, image.Pt(35, 10), image.Pt(5, 10), 40, 20, 2},
			{3, 40, 20, image.Pt(35, 10), image.Pt(5, 10), 40, 20, 3},
			{4, 40, 20, image.Pt(5, 10), image.Pt(35, 10), 40, 20, 4},
			{5, 20, 40, image.Pt(10, 5), image.Pt(10, 35), 20, 40, 5},
			{6, 20, 40, image.Pt(10, 5), image.Pt(10, 35), 20, 40, 6},
			{7, 20, 40, image.Pt(10, 35), image.Pt(10, 5), 20, 40, 7},
			{8, 20, 40, image.Pt(10, 35), image.Pt(10, 5), 20, 40, 8},
		}
		for _, c := range cases {
			data := exifJPEG(t, src, c.o, bo)
			if got := exifOrientation(data); got != c.wantOrientParsed {
				t.Fatalf("orientation %d parsed as %d", c.o, got)
			}
			out := thumb(t, data, 640)
			if b := out.Bounds(); b.Dx() != c.w || b.Dy() != c.h {
				t.Fatalf("o=%d size %v", c.o, b)
			}
			near(t, "red", out.At(c.redAt.X, c.redAt.Y), 255, 0, 0, 40)
			near(t, "blue", out.At(c.blueAt.X, c.blueAt.Y), 0, 0, 255, 40)
			w, h, f, err := Size(bytes.NewReader(data))
			if err != nil || w != c.sizeW || h != c.sizeH || f != "jpeg" {
				t.Fatalf("Size o=%d: %d %d %s %v", c.o, w, h, f, err)
			}
		}
	}
	// Orientation 6 also changes which pixel ends up top-left precisely.
	rot := orient(rgbaFrom(halves(4, 2, red, blue)), 6)
	if rot.Rect.Dx() != 2 || rot.Rect.Dy() != 4 || rot.RGBAAt(0, 0).R != 255 || rot.RGBAAt(0, 3).B != 255 {
		t.Fatal("orient 6 pixel mapping")
	}
}

func rgbaFrom(m image.Image) *image.RGBA {
	b := m.Bounds()
	out := image.NewRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			out.Set(x, y, m.At(x, y))
		}
	}
	return out
}

// pngHeader returns a PNG signature + IHDR only (no pixel data), claiming
// the given dimensions.
func pngHeader(w, h uint32) []byte {
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8], ihdr[9] = 8, 6 // 8-bit RGBA
	_ = binary.Write(&b, binary.BigEndian, uint32(13))
	chunk := append([]byte("IHDR"), ihdr...)
	b.Write(chunk)
	_ = binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(chunk))
	return b.Bytes()
}

func TestPixelLimits(t *testing.T) {
	for _, d := range [][2]uint32{{30000, 30000}, {10000, 6000}, {20001, 10}, {10, 20001}} {
		var out bytes.Buffer
		if err := Thumbnail(bytes.NewReader(pngHeader(d[0], d[1])), 160, &out); !errors.Is(err, ErrTooLarge) {
			t.Fatalf("%v: expected ErrTooLarge, got %v", d, err)
		}
		if _, _, _, err := Size(bytes.NewReader(pngHeader(d[0], d[1]))); !errors.Is(err, ErrTooLarge) {
			t.Fatalf("%v: Size expected ErrTooLarge, got %v", d, err)
		}
	}
	// Within limits, the header alone reports the size.
	w, h, f, err := Size(bytes.NewReader(pngHeader(7000, 7000)))
	if err != nil || w != 7000 || h != 7000 || f != "png" {
		t.Fatalf("Size: %d %d %s %v", w, h, f, err)
	}
	// …but decoding a truncated file fails cleanly.
	var out bytes.Buffer
	if err := Thumbnail(bytes.NewReader(pngHeader(100, 100)), 160, &out); err == nil {
		t.Fatal("expected decode error for header-only PNG")
	}
}

func TestRejectsUnsupportedAndBadArgs(t *testing.T) {
	var out bytes.Buffer
	for _, data := range [][]byte{[]byte("BM\x00\x00not a bmp we decode"), []byte("RIFF\x00\x00\x00\x00WEBPVP8 "), {}, []byte("<svg/>")} {
		if err := Thumbnail(bytes.NewReader(data), 160, &out); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("%q: expected ErrUnsupported, got %v", data, err)
		}
	}
	png := encodePNG(t, image.NewGray(image.Rect(0, 0, 4, 4)))
	for _, e := range []int{0, -1, MaxThumbEdge + 1} {
		if err := Thumbnail(bytes.NewReader(png), e, &out); err == nil {
			t.Fatalf("maxEdge %d accepted", e)
		}
	}
}

func TestResizeAllPixelTypes(t *testing.T) {
	const w, h = 16, 8
	fill := func(set func(x, y int)) {
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				set(x, y)
			}
		}
	}
	gray := image.NewGray(image.Rect(0, 0, w, h))
	fill(func(x, y int) { gray.SetGray(x, y, color.Gray{200}) })
	gray16 := image.NewGray16(image.Rect(0, 0, w, h))
	fill(func(x, y int) { gray16.SetGray16(x, y, color.Gray16{200 << 8}) })
	rgba := image.NewRGBA(image.Rect(0, 0, w, h))
	fill(func(x, y int) { rgba.SetRGBA(x, y, color.RGBA{200, 0, 0, 255}) })
	nrgba64 := image.NewNRGBA64(image.Rect(0, 0, w, h))
	fill(func(x, y int) { nrgba64.SetNRGBA64(x, y, color.NRGBA64{200 << 8, 0, 0, 0xFFFF}) })
	rgba64 := image.NewRGBA64(image.Rect(0, 0, w, h)) // generic fallback path
	fill(func(x, y int) { rgba64.SetRGBA64(x, y, color.RGBA64{0, 200 << 8, 0, 0xFFFF}) })
	cmyk := image.NewCMYK(image.Rect(0, 0, w, h))
	fill(func(x, y int) { cmyk.SetCMYK(x, y, color.CMYK{0, 255, 255, 55}) }) // ≈ (200,0,0)
	pal := image.NewPaletted(image.Rect(0, 0, w, h), color.Palette{color.RGBA{0, 0, 200, 255}})
	ycc := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
	yy, cb, cr := color.RGBToYCbCr(200, 0, 0)
	for i := range ycc.Y {
		ycc.Y[i] = yy
	}
	for i := range ycc.Cb {
		ycc.Cb[i], ycc.Cr[i] = cb, cr
	}
	premul := image.NewRGBA(image.Rect(0, 0, w, h)) // 50% red, premultiplied
	fill(func(x, y int) { premul.SetRGBA(x, y, color.RGBA{128, 0, 0, 128}) })

	cases := []struct {
		name    string
		img     image.Image
		r, g, b uint8
	}{
		{"gray", gray, 200, 200, 200},
		{"gray16", gray16, 200, 200, 200},
		{"rgba", rgba, 200, 0, 0},
		{"nrgba64", nrgba64, 200, 0, 0},
		{"rgba64", rgba64, 0, 200, 0},
		{"cmyk", cmyk, 200, 0, 0},
		{"paletted", pal, 0, 0, 200},
		{"ycbcr420", ycc, 200, 0, 0},
		{"premultiplied", premul, 255, 188, 188},
		{"subimage", rgba.SubImage(image.Rect(3, 2, 13, 7)), 200, 0, 0},
	}
	for _, c := range cases {
		b := c.img.Bounds()
		dw, dh := fit(b.Dx(), b.Dy(), 4)
		out := resize(c.img, dw, dh)
		for y := 0; y < dh; y++ {
			for x := 0; x < dw; x++ {
				near(t, c.name, out.RGBAAt(x, y), c.r, c.g, c.b, 2)
			}
		}
	}
}

func TestSpansWeights(t *testing.T) {
	for _, nm := range [][2]int{{10, 3}, {7, 7}, {1000, 333}, {5, 1}} {
		sp := spans(nm[0], nm[1])
		cover := make([]float64, nm[0])
		for _, s := range sp {
			var sum float64
			for k, w := range s.w {
				sum += float64(w)
				cover[s.lo+k] += float64(w)
			}
			if math.Abs(sum-1) > 1e-5 {
				t.Fatalf("%v: weights sum %v", nm, sum)
			}
		}
		// Every source pixel contributes exactly its share (m/n) overall.
		for i, c := range cover {
			if want := float64(nm[1]) / float64(nm[0]); math.Abs(c-want) > 1e-5 {
				t.Fatalf("%v: pixel %d coverage %v want %v", nm, i, c, want)
			}
		}
	}
}

func FuzzExifOrientation(f *testing.F) {
	f.Add(exifJPEG(f, image.NewGray(image.Rect(0, 0, 2, 2)), 6, binary.BigEndian))
	f.Add([]byte{0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x02})
	f.Fuzz(func(t *testing.T, b []byte) {
		if o := exifOrientation(b); o < 1 || o > 8 {
			t.Fatalf("orientation %d", o)
		}
	})
}

func FuzzThumbnail(f *testing.F) {
	f.Add(encodePNG(f, halves(8, 4, red, blue)))
	f.Add(encodeJPEG(f, halves(8, 4, red, blue)))
	f.Fuzz(func(t *testing.T, b []byte) {
		var out bytes.Buffer
		_ = Thumbnail(bytes.NewReader(b), 32, &out)
	})
}

func BenchmarkThumbnail12MPJPEG(b *testing.B) {
	m := image.NewYCbCr(image.Rect(0, 0, 4000, 3000), image.YCbCrSubsampleRatio420)
	for i := range m.Y {
		m.Y[i] = uint8(i)
	}
	var src bytes.Buffer
	if err := jpeg.Encode(&src, m, &jpeg.Options{Quality: 90}); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(src.Len()))
	for b.Loop() {
		var out bytes.Buffer
		if err := Thumbnail(bytes.NewReader(src.Bytes()), 320, &out); err != nil {
			b.Fatal(err)
		}
	}
}

// pngHeaderOf is pngHeader with a chosen bit depth, colour type and interlace method.
func pngHeaderOf(w, h uint32, depth, colorType, interlace byte) []byte {
	b := pngHeader(w, h)
	ihdr := b[16:29]
	ihdr[8], ihdr[9], ihdr[12] = depth, colorType, interlace
	binary.BigEndian.PutUint32(b[29:], crc32.ChecksumIEEE(b[12:29]))
	return b
}

// jpegHeader returns SOI + a frame header (SOF0 or SOF2) + an empty SOS, claiming the given
// dimensions and per-component sampling factors (0x11 = 1×1, 0x22 = 2×2 ...).
func jpegHeader(w, h uint16, progressive bool, sampling ...byte) []byte {
	var b bytes.Buffer
	b.Write([]byte{0xFF, 0xD8})
	marker := byte(0xC0)
	if progressive {
		marker = 0xC2
	}
	seg := []byte{8, byte(h >> 8), byte(h), byte(w >> 8), byte(w), byte(len(sampling))}
	for i, s := range sampling {
		seg = append(seg, byte(i+1), s, 0)
	}
	b.Write([]byte{0xFF, marker, byte((len(seg) + 2) >> 8), byte(len(seg) + 2)})
	b.Write(seg)
	b.Write([]byte{0xFF, 0xDA, 0x00, 0x08, 1, 1, 0, 0, 63, 0})
	return b.Bytes()
}

// Finding: the pixel limit alone did not bound decode memory. A 49 MP 16-bit RGBA PNG
// decodes to ~400 MB and a progressive CMYK JPEG of the same size to over 1 GB, both from a
// tiny, highly compressible file.
func TestDecodeMemoryBudget(t *testing.T) {
	tooBig := map[string][]byte{
		"16-bit RGBA PNG":            pngHeaderOf(7000, 7000, 16, 6, 0),
		"16-bit gray+alpha PNG":      pngHeaderOf(7000, 7000, 16, 4, 0),
		"interlaced 8-bit RGBA PNG":  pngHeaderOf(7000, 7000, 8, 6, 1),
		"progressive CMYK JPEG":      jpegHeader(7000, 7000, true, 0x11, 0x11, 0x11, 0x11),
		"progressive 4:4:4 JPEG":     jpegHeader(7000, 7000, true, 0x11, 0x11, 0x11),
		"baseline CMYK JPEG 7000²":   jpegHeader(7000, 7000, false, 0x11, 0x11, 0x11, 0x11),
		"progressive odd-sampled JP": jpegHeader(7000, 7000, true, 0x11, 0x22, 0x11),
	}
	for name, data := range tooBig {
		if _, _, _, err := Size(bytes.NewReader(data)); !errors.Is(err, ErrTooLarge) {
			t.Errorf("%s: Size: want ErrTooLarge, got %v", name, err)
		}
		var out bytes.Buffer
		if err := Thumbnail(bytes.NewReader(data), 160, &out); !errors.Is(err, ErrTooLarge) {
			t.Errorf("%s: Thumbnail: want ErrTooLarge, got %v", name, err)
		}
	}
	// Ordinary large photos and scans stay within the budget.
	fine := map[string][]byte{
		"49 MP 8-bit RGBA PNG":         pngHeader(7000, 7000),
		"49 MP 8-bit gray PNG":         pngHeaderOf(7000, 7000, 8, 0, 0),
		"25 MP 16-bit RGB PNG":         pngHeaderOf(5000, 5000, 16, 2, 0),
		"49 MP baseline 4:2:0 JPEG":    jpegHeader(7000, 7000, false, 0x22, 0x11, 0x11),
		"49 MP baseline 4:4:4 JPEG":    jpegHeader(7000, 7000, false, 0x11, 0x11, 0x11),
		"24 MP progressive 4:2:0 JPEG": jpegHeader(6000, 4000, true, 0x22, 0x11, 0x11),
		"24 MP progressive gray JPEG":  jpegHeader(6000, 4000, true, 0x11),
		"20 MP baseline CMYK JPEG":     jpegHeader(5000, 4000, false, 0x11, 0x11, 0x11, 0x11),
	}
	for name, data := range fine {
		if _, _, _, err := Size(bytes.NewReader(data)); err != nil {
			t.Errorf("%s: Size: %v", name, err)
		}
	}
	// The estimate covers the buffer the decoder really returns.
	pal := image.NewPaletted(image.Rect(0, 0, 300, 200), color.Palette{color.Black, color.White})
	for _, m := range []image.Image{image.NewNRGBA64(image.Rect(0, 0, 300, 200)), image.NewGray(image.Rect(0, 0, 300, 200)),
		image.NewNRGBA(image.Rect(0, 0, 300, 200)), image.NewGray16(image.Rect(0, 0, 300, 200)), pal} {
		data := encodePNG(t, m)
		cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		dec, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if est, buf := decodeBytes(cfg, format, data), pixBytes(dec); est < buf || est > 8*300*200 {
			t.Errorf("%T: estimate %d, decoded buffer %d", m, est, buf)
		}
	}
	jpg := encodeJPEG(t, image.NewRGBA(image.Rect(0, 0, 300, 200)))
	cfg, _, _ := image.DecodeConfig(bytes.NewReader(jpg))
	dec, err := jpeg.Decode(bytes.NewReader(jpg))
	if err != nil {
		t.Fatal(err)
	}
	if est, buf := decodeBytes(cfg, "jpeg", jpg), pixBytes(dec); est < buf || est > 300*200*8 {
		t.Errorf("baseline JPEG estimate %d, decoded buffer %d", est, buf)
	}
}

// pixBytes is the size of a decoded image's pixel buffers.
func pixBytes(m image.Image) int64 {
	switch m := m.(type) {
	case *image.YCbCr:
		return int64(len(m.Y) + len(m.Cb) + len(m.Cr))
	case *image.Gray:
		return int64(len(m.Pix))
	case *image.Gray16:
		return int64(len(m.Pix))
	case *image.NRGBA:
		return int64(len(m.Pix))
	case *image.NRGBA64:
		return int64(len(m.Pix))
	case *image.RGBA:
		return int64(len(m.Pix))
	case *image.RGBA64:
		return int64(len(m.Pix))
	case *image.Paletted:
		return int64(len(m.Pix))
	}
	return 0
}

// All decodes share one memory budget, whatever module asks (files, media, TV logos).
func TestDecodesShareOneMemoryBudget(t *testing.T) {
	data := encodePNG(t, image.NewNRGBA(image.Rect(0, 0, 64, 64)))

	// While the whole budget is taken, a decode waits (and gives up with its context).
	if err := decodeMem.acquire(context.Background(), decodeBudget); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	err := ThumbnailContext(cctx, bytes.NewReader(data), 32, io.Discard)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("decode ran outside the budget: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- Thumbnail(bytes.NewReader(data), 32, io.Discard) }()
	select {
	case err := <-done:
		t.Fatalf("Thumbnail did not wait for the budget: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	decodeMem.release(decodeBudget)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// Concurrency is bounded by memory: at most decodeBudget/minDecodeWeight at once.
	var cur, peak atomic.Int32
	testDecodeHook = func() {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		cur.Add(-1)
	}
	defer func() { testDecodeHook = nil }()
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := Thumbnail(bytes.NewReader(data), 32, io.Discard); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if max := int32(decodeBudget / minDecodeWeight); peak.Load() > max || peak.Load() < 2 {
		t.Fatalf("peak concurrent decodes %d, want 2..%d", peak.Load(), max)
	}
	decodeMem.mu.Lock()
	free, waiting := decodeMem.free, len(decodeMem.waiters)
	decodeMem.mu.Unlock()
	if free != decodeBudget || waiting != 0 {
		t.Fatalf("budget not returned: free %d, waiters %d", free, waiting)
	}
}

func TestMemSemFIFOAndCancel(t *testing.T) {
	m := newMemSem(100)
	bg := context.Background()
	if err := m.acquire(bg, 70); err != nil {
		t.Fatal(err)
	}
	order := make(chan int, 2)
	go func() { _ = m.acquire(bg, 50); order <- 1 }()
	time.Sleep(20 * time.Millisecond)
	go func() { _ = m.acquire(bg, 10); order <- 2 }() // fits, but must not overtake the first waiter
	time.Sleep(20 * time.Millisecond)
	cctx, cancel := context.WithCancel(bg)
	errc := make(chan error, 1)
	go func() { errc <- m.acquire(cctx, 100) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	select {
	case n := <-order:
		t.Fatalf("waiter %d ran before release (the 10-byte one must not overtake the 50-byte one)", n)
	default:
	}
	m.release(70)
	if a, b := <-order, <-order; a+b != 3 {
		t.Fatalf("order %d %d", a, b)
	}
	m.release(60)
	if m.free != 100 || len(m.waiters) != 0 {
		t.Fatalf("free %d waiters %d", m.free, len(m.waiters))
	}
}
