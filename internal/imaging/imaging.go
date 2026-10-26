// Package imaging makes JPEG thumbnails from untrusted JPEG, PNG and GIF
// images using only the standard library.
//
// Safety: image dimensions are read with image.DecodeConfig before any pixel
// buffer is allocated and anything over MaxPixels or MaxDimension is
// rejected (ARCHITECTURE.md §2.16), input bytes are capped, and only the
// three formats above are decoded no matter which other decoders happen to be
// registered with package image. Because bytes per pixel differ widely (a
// 16-bit RGBA PNG decodes to 8 B/px, a progressive CMYK JPEG needs over 20),
// the memory the decoder will allocate is also estimated from the header and
// capped at MaxDecodeBytes, and all decodes in the process share one memory
// budget, whichever module asks (file thumbnails, media posters, TV logos).
//
// Quality: downscaling is an exact area average (box filter with fractional
// pixel coverage) performed in linear light on premultiplied alpha, which
// avoids the darkening and aliasing of naive sRGB/nearest-neighbour scaling.
// Images are never upscaled. JPEG EXIF orientation is honoured.
package imaging

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"sync"
)

const (
	// MaxPixels is the largest image (width × height) that will be decoded.
	MaxPixels = 50_000_000
	// MaxDecodeBytes caps the memory a single decode may need, estimated from
	// the header (pixel format, bit depth, interlacing, JPEG sampling and
	// progressive coefficient buffers) before anything is allocated.
	MaxDecodeBytes = 256 << 20
	// decodeBudget is the memory shared by all concurrent decodes in the
	// process: a decode waits until its estimate fits, so large images are
	// serialised while small ones still run in parallel.
	decodeBudget = MaxDecodeBytes
	// minDecodeWeight is the least a decode is charged (bounds concurrency).
	minDecodeWeight = 16 << 20
	// MaxDimension is the largest accepted width or height.
	MaxDimension = 20000
	// MaxInputBytes caps how much of the input stream is read.
	MaxInputBytes = 256 << 20
	// maxHeaderBytes bounds how much is buffered while reading the header
	// (JPEG APPn segments such as EXIF/XMP/ICC precede the frame header).
	maxHeaderBytes = 16 << 20
	// MaxThumbEdge is the largest maxEdge Thumbnail accepts.
	MaxThumbEdge = 4096
	// JPEGQuality is the encoder quality used for thumbnails.
	JPEGQuality = 82
)

var (
	// ErrTooLarge means the image's declared dimensions exceed the limits.
	ErrTooLarge = errors.New("imaging: image dimensions exceed limits")
	// ErrUnsupported means the data is not a JPEG, PNG or GIF image.
	ErrUnsupported = errors.New("imaging: unsupported image format")
)

// header is what sniffing an image stream yields.
type header struct {
	cfg         image.Config
	format      string    // "jpeg", "png" or "gif"
	orientation int       // EXIF orientation 1..8 (1 for non-JPEG)
	mem         int64     // estimated bytes the decoder allocates
	rest        io.Reader // the full stream again, from byte 0
}

// sniff reads just enough of r to learn the format, dimensions and decode
// memory, enforces the limits, and returns a reader that replays the consumed
// bytes.
func sniff(r io.Reader) (*header, error) {
	lr := io.LimitReader(r, MaxInputBytes)
	var buf bytes.Buffer
	cfg, format, err := image.DecodeConfig(io.TeeReader(io.LimitReader(lr, maxHeaderBytes), &buf))
	if err != nil {
		if errors.Is(err, image.ErrFormat) {
			return nil, ErrUnsupported
		}
		return nil, fmt.Errorf("imaging: read header: %w", err)
	}
	switch format {
	case "jpeg", "png", "gif":
	default:
		return nil, ErrUnsupported
	}
	if err := checkDims(cfg.Width, cfg.Height); err != nil {
		return nil, err
	}
	h := &header{cfg: cfg, format: format, orientation: 1, mem: decodeBytes(cfg, format, buf.Bytes())}
	if h.mem > MaxDecodeBytes {
		return nil, fmt.Errorf("%w: %dx%d %s needs about %d MiB to decode", ErrTooLarge, cfg.Width, cfg.Height, format, h.mem>>20)
	}
	if format == "jpeg" {
		// APP1/EXIF precedes SOF, so it is inside the bytes DecodeConfig read.
		h.orientation = exifOrientation(buf.Bytes())
	}
	h.rest = io.MultiReader(bytes.NewReader(buf.Bytes()), lr)
	return h, nil
}

// decodeBytes estimates what the standard decoders allocate for an image with
// this header (hdr holds the bytes DecodeConfig consumed). It errs high.
func decodeBytes(cfg image.Config, format string, hdr []byte) int64 {
	px := int64(cfg.Width) * int64(cfg.Height)
	switch format {
	case "gif":
		return px // first frame only: one palette index per pixel, within the screen
	case "png":
		var bpp int64 = 8
		switch cfg.ColorModel {
		case color.GrayModel, color.RGBAModel, color.NRGBAModel:
			bpp = 4 // gray with a tRNS chunk (after IHDR, unseen here) decodes to NRGBA
		case color.Gray16Model, color.RGBA64Model, color.NRGBA64Model:
			bpp = 8
		default:
			if _, ok := cfg.ColorModel.(color.Palette); ok {
				bpp = 1
			}
		}
		n := px * bpp
		// Adam7 decodes each pass into its own image next to the full one; the
		// largest pass holds half the rows.
		if len(hdr) > 28 && string(hdr[12:16]) == "IHDR" && hdr[28] != 0 {
			n += n / 2
		}
		return n
	case "jpeg":
		return jpegDecodeBytes(cfg, hdr)
	}
	return px * 8
}

// jpegComp is one frame component's sampling factors.
type jpegComp struct{ h, v int64 }

// jpegFrame finds the frame header (SOF0/1/2) in a JPEG prefix.
func jpegFrame(b []byte) (comps []jpegComp, progressive, ok bool) {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return nil, false, false
	}
	for i := 2; i+4 <= len(b); {
		if b[i] != 0xFF {
			return nil, false, false
		}
		marker := b[i+1]
		switch {
		case marker == 0xFF:
			i++
			continue
		case marker == 0x01 || marker >= 0xD0 && marker <= 0xD8:
			i += 2
			continue
		case marker == 0xDA || marker == 0xD9:
			return nil, false, false
		}
		segLen := int(b[i+2])<<8 | int(b[i+3])
		if segLen < 2 || i+2+segLen > len(b) {
			return nil, false, false
		}
		if marker == 0xC0 || marker == 0xC1 || marker == 0xC2 {
			seg := b[i+4 : i+2+segLen]
			if len(seg) < 6 {
				return nil, false, false
			}
			n := int(seg[5])
			if n < 1 || len(seg) < 6+3*n {
				return nil, false, false
			}
			for k := 0; k < n; k++ {
				hv := seg[7+3*k]
				comps = append(comps, jpegComp{h: max(int64(hv>>4), 1), v: max(int64(hv&0x0F), 1)})
			}
			return comps, marker == 0xC2, true
		}
		i += 2 + segLen
	}
	return nil, false, false
}

// jpegDecodeBytes mirrors image/jpeg's allocations: the component planes
// (padded to whole MCUs; non-standard subsampling is expanded to full
// resolution), the black plane and RGBA/CMYK copy of 4-component images, the
// RGBA copy of RGB images, and for progressive images the coefficients of
// every 8×8 block of every component (64 int32 = 256 bytes).
func jpegDecodeBytes(cfg image.Config, hdr []byte) int64 {
	w, h := int64(cfg.Width), int64(cfg.Height)
	comps, progressive, ok := jpegFrame(hdr)
	if !ok || len(comps) < 1 {
		return w * h * 26 // unknown layout: 4 progressive full-resolution components + copies
	}
	if len(comps) == 1 {
		comps[0] = jpegComp{1, 1} // image/jpeg ignores the factors of a single component
	}
	var maxH, maxV int64 = 1, 1
	for _, c := range comps {
		maxH, maxV = max(maxH, c.h), max(maxV, c.v)
	}
	mxx, myy := (w+8*maxH-1)/(8*maxH), (h+8*maxV-1)/(8*maxV)
	full := 8 * maxH * mxx * 8 * maxV * myy
	n := full
	if len(comps) >= 3 {
		chroma := 2 * full
		c0, c1, c2 := comps[0], comps[1], comps[2]
		if c1 == c2 && c0.h == maxH && c0.v == maxV && maxH%c1.h == 0 && maxV%c1.v == 0 {
			chroma = 2 * full / ((maxH / c1.h) * (maxV / c1.v))
		}
		n += chroma
		switch {
		case len(comps) >= 4:
			n += 8*comps[3].h*mxx*8*comps[3].v*myy + 4*w*h
		case cfg.ColorModel == color.RGBAModel:
			n += 4 * w * h
		}
	}
	if progressive {
		for _, c := range comps {
			n += mxx * myy * c.h * c.v * 256
		}
	}
	return n
}

func checkDims(w, h int) error {
	if w <= 0 || h <= 0 {
		return fmt.Errorf("imaging: invalid dimensions %dx%d", w, h)
	}
	if w > MaxDimension || h > MaxDimension || int64(w)*int64(h) > MaxPixels {
		return fmt.Errorf("%w: %dx%d", ErrTooLarge, w, h)
	}
	return nil
}

// Size reports an image's display dimensions (after EXIF orientation, so a
// portrait phone photo stored landscape reports portrait) and its format
// ("jpeg", "png", "gif") without decoding pixels. Images over the limits
// return ErrTooLarge.
func Size(r io.Reader) (width, height int, format string, err error) {
	h, err := sniff(r)
	if err != nil {
		return 0, 0, "", err
	}
	w, ht := h.cfg.Width, h.cfg.Height
	if h.orientation >= 5 {
		w, ht = ht, w
	}
	return w, ht, h.format, nil
}

// Thumbnail decodes a JPEG, PNG or GIF (first frame) from r, scales it so
// its longest edge is at most maxEdge (never upscaling), applies EXIF
// orientation, flattens transparency onto white and writes a baseline JPEG
// (quality JPEGQuality) to w. It waits as long as needed for the shared decode
// memory budget; ThumbnailContext can give up.
func Thumbnail(r io.Reader, maxEdge int, w io.Writer) error {
	return ThumbnailContext(context.Background(), r, maxEdge, w)
}

// ThumbnailContext is Thumbnail that stops waiting for the shared decode
// memory budget when ctx ends.
func ThumbnailContext(ctx context.Context, r io.Reader, maxEdge int, w io.Writer) error {
	if maxEdge <= 0 || maxEdge > MaxThumbEdge {
		return fmt.Errorf("imaging: invalid maxEdge %d", maxEdge)
	}
	h, err := sniff(r)
	if err != nil {
		return err
	}
	dw, dh := fit(h.cfg.Width, h.cfg.Height, maxEdge)
	weight := min(max(h.mem+2*4*int64(dw)*int64(dh), minDecodeWeight), decodeBudget)
	if err := decodeMem.acquire(ctx, weight); err != nil {
		return err
	}
	defer decodeMem.release(weight)
	if testDecodeHook != nil {
		testDecodeHook()
	}
	var src image.Image
	switch h.format {
	case "jpeg":
		src, err = jpeg.Decode(h.rest)
	case "png":
		src, err = png.Decode(h.rest)
	case "gif":
		src, err = gif.Decode(h.rest)
	}
	if err != nil {
		return fmt.Errorf("imaging: decode %s: %w", h.format, err)
	}
	b := src.Bounds()
	if err := checkDims(b.Dx(), b.Dy()); err != nil {
		return err
	}
	dw, dh = fit(b.Dx(), b.Dy(), maxEdge)
	out := orient(resize(src, dw, dh), h.orientation)
	if err := jpeg.Encode(w, out, &jpeg.Options{Quality: JPEGQuality}); err != nil {
		return fmt.Errorf("imaging: encode: %w", err)
	}
	return nil
}

// testDecodeHook runs while a decode holds its share of the budget (tests).
var testDecodeHook func()

// decodeMem is the decode memory budget shared by the whole process.
var decodeMem = newMemSem(decodeBudget)

// memSem is a FIFO weighted semaphore over bytes of memory.
type memSem struct {
	mu      sync.Mutex
	free    int64
	waiters []*memWaiter
}

type memWaiter struct {
	n     int64
	ready chan struct{}
}

func newMemSem(n int64) *memSem { return &memSem{free: n} }

// acquire takes n bytes of the budget, waiting (first come, first served)
// until they are free or ctx ends. n must not exceed the budget.
func (m *memSem) acquire(ctx context.Context, n int64) error {
	m.mu.Lock()
	if len(m.waiters) == 0 && n <= m.free {
		m.free -= n
		m.mu.Unlock()
		return nil
	}
	w := &memWaiter{n: n, ready: make(chan struct{})}
	m.waiters = append(m.waiters, w)
	m.mu.Unlock()
	select {
	case <-w.ready:
		return nil
	case <-ctx.Done():
		m.mu.Lock()
		select {
		case <-w.ready:
			m.free += n // granted just as ctx ended: give it back
		default:
			for i, x := range m.waiters {
				if x == w {
					m.waiters = append(m.waiters[:i], m.waiters[i+1:]...)
					break
				}
			}
		}
		m.grant()
		m.mu.Unlock()
		return ctx.Err()
	}
}

// release returns n bytes to the budget.
func (m *memSem) release(n int64) {
	m.mu.Lock()
	m.free += n
	m.grant()
	m.mu.Unlock()
}

// grant wakes waiters in order while the first one fits. m.mu is held.
func (m *memSem) grant() {
	for len(m.waiters) > 0 && m.waiters[0].n <= m.free {
		w := m.waiters[0]
		m.waiters = m.waiters[1:]
		m.free -= w.n
		close(w.ready)
	}
}

// fit returns the largest size with the source aspect ratio whose longest
// edge is ≤ maxEdge, without upscaling.
func fit(w, h, maxEdge int) (int, int) {
	if w <= maxEdge && h <= maxEdge {
		return w, h
	}
	if w >= h {
		return maxEdge, max(1, int(math.Round(float64(h)*float64(maxEdge)/float64(w))))
	}
	return max(1, int(math.Round(float64(w)*float64(maxEdge)/float64(h)))), maxEdge
}

// ---------------------------------------------------------------------------
// EXIF orientation

// exifOrientation scans JPEG marker segments up to the first scan for an
// APP1 "Exif" segment and returns IFD0's Orientation tag (0x0112), or 1.
func exifOrientation(b []byte) int {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return 1
	}
	i := 2
	for i+4 <= len(b) {
		if b[i] != 0xFF {
			return 1
		}
		marker := b[i+1]
		switch {
		case marker == 0xFF: // fill byte
			i++
			continue
		case marker == 0x01 || marker >= 0xD0 && marker <= 0xD8: // standalone markers
			i += 2
			continue
		case marker == 0xDA || marker == 0xD9: // start of scan / end of image
			return 1
		}
		segLen := int(b[i+2])<<8 | int(b[i+3])
		if segLen < 2 || i+2+segLen > len(b) {
			return 1
		}
		seg := b[i+4 : i+2+segLen]
		if marker == 0xE1 && len(seg) >= 6 && string(seg[:6]) == "Exif\x00\x00" {
			if o := tiffOrientation(seg[6:]); o != 0 {
				return o
			}
		}
		i += 2 + segLen
	}
	return 1
}

// tiffOrientation reads the Orientation tag from a TIFF header's IFD0.
func tiffOrientation(t []byte) int {
	if len(t) < 8 {
		return 0
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 0
	}
	if bo.Uint16(t[2:]) != 42 {
		return 0
	}
	off := int64(bo.Uint32(t[4:]))
	if off < 8 || off+2 > int64(len(t)) {
		return 0
	}
	n := int64(bo.Uint16(t[off:]))
	for k := int64(0); k < n && k < 1024; k++ {
		e := off + 2 + k*12
		if e+12 > int64(len(t)) {
			return 0
		}
		if bo.Uint16(t[e:]) != 0x0112 {
			continue
		}
		typ, cnt := bo.Uint16(t[e+2:]), bo.Uint32(t[e+4:])
		if typ != 3 || cnt < 1 { // SHORT, inline value
			return 0
		}
		if v := int(bo.Uint16(t[e+8:])); v >= 1 && v <= 8 {
			return v
		}
		return 0
	}
	return 0
}

// orient applies an EXIF orientation transform. Orientations 5–8 swap the
// width and height.
func orient(m *image.RGBA, o int) *image.RGBA {
	if o <= 1 || o > 8 {
		return m
	}
	sw, sh := m.Rect.Dx(), m.Rect.Dy()
	dw, dh := sw, sh
	if o >= 5 {
		dw, dh = sh, sw
	}
	out := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for dy := 0; dy < dh; dy++ {
		for dx := 0; dx < dw; dx++ {
			var sx, sy int
			switch o {
			case 2: // mirror horizontal
				sx, sy = sw-1-dx, dy
			case 3: // rotate 180
				sx, sy = sw-1-dx, sh-1-dy
			case 4: // mirror vertical
				sx, sy = dx, sh-1-dy
			case 5: // transpose
				sx, sy = dy, dx
			case 6: // rotate 90° clockwise
				sx, sy = dy, sh-1-dx
			case 7: // transverse
				sx, sy = sw-1-dy, sh-1-dx
			case 8: // rotate 90° counter-clockwise
				sx, sy = sw-1-dy, dx
			}
			si := sy*m.Stride + sx*4
			di := dy*out.Stride + dx*4
			copy(out.Pix[di:di+4], m.Pix[si:si+4])
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Resampling

// srgbToLinear maps an 8-bit sRGB component to linear light.
var srgbToLinear = func() (t [256]float32) {
	for i := range t {
		c := float64(i) / 255
		if c <= 0.04045 {
			t[i] = float32(c / 12.92)
		} else {
			t[i] = float32(math.Pow((c+0.055)/1.055, 2.4))
		}
	}
	return t
}()

// linearToSRGB maps linear light (quantised to 1/linSteps) back to 8-bit
// sRGB. 16k steps keep the error well under one output level even in the
// shadows, where the curve is steepest.
const linSteps = 1 << 14

var linearToSRGB = func() []uint8 {
	t := make([]uint8, linSteps+1)
	for i := range t {
		l := float64(i) / linSteps
		var c float64
		if l <= 0.0031308 {
			c = l * 12.92
		} else {
			c = 1.055*math.Pow(l, 1/2.4) - 0.055
		}
		t[i] = uint8(math.Min(255, math.Max(0, math.Round(c*255))))
	}
	return t
}()

func toSRGB(v float32) uint8 {
	i := int(v*linSteps + 0.5)
	if i < 0 {
		i = 0
	} else if i > linSteps {
		i = linSteps
	}
	return linearToSRGB[i]
}

// span lists the source pixels covering one destination pixel and their
// normalised coverage weights.
type span struct {
	lo int
	w  []float32
}

// spans computes exact box-filter coverage for scaling n source pixels to m
// destination pixels (m ≤ n).
func spans(n, m int) []span {
	out := make([]span, m)
	scale := float64(n) / float64(m)
	for i := range out {
		start, end := float64(i)*scale, float64(i+1)*scale
		lo := int(math.Floor(start))
		hi := min(int(math.Ceil(end)), n)
		ws := make([]float32, 0, hi-lo)
		for s := lo; s < hi; s++ {
			cover := math.Min(end, float64(s+1)) - math.Max(start, float64(s))
			ws = append(ws, float32(cover/scale))
		}
		out[i] = span{lo: lo, w: ws}
	}
	return out
}

// resize area-averages src to dw×dh in linear light, compositing any
// transparency onto white. Memory use is O(source width + output size): one
// source row is converted at a time.
func resize(src image.Image, dw, dh int) *image.RGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	xs, ys := spans(sw, dw), spans(sh, dh)
	fetch := rowFetcher(src)

	raw := make([]uint8, sw*4)   // one source row, non-premultiplied 8-bit RGBA
	lin := make([]float32, sw*4) // same row, premultiplied linear
	row := make([]float32, dw*4) // horizontally resampled cached row
	acc := make([]float32, dw*4) // vertical accumulator
	rowY := -1

	loadRow := func(y int) {
		fetch(y, raw)
		for x := 0; x < sw; x++ {
			p := raw[x*4 : x*4+4 : x*4+4]
			a := float32(p[3]) / 255
			lin[x*4] = srgbToLinear[p[0]] * a
			lin[x*4+1] = srgbToLinear[p[1]] * a
			lin[x*4+2] = srgbToLinear[p[2]] * a
			lin[x*4+3] = a
		}
		for dx, sp := range xs {
			var r, g, bl, a float32
			for k, wt := range sp.w {
				q := (sp.lo + k) * 4
				r += lin[q] * wt
				g += lin[q+1] * wt
				bl += lin[q+2] * wt
				a += lin[q+3] * wt
			}
			row[dx*4], row[dx*4+1], row[dx*4+2], row[dx*4+3] = r, g, bl, a
		}
		rowY = y
	}

	out := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for dy, sp := range ys {
		clear(acc)
		for k, wt := range sp.w {
			if y := sp.lo + k; y != rowY {
				loadRow(y)
			}
			for i, v := range row {
				acc[i] += v * wt
			}
		}
		o := out.Pix[dy*out.Stride : dy*out.Stride+dw*4]
		for dx := 0; dx < dw; dx++ {
			a := min(max(acc[dx*4+3], 0), 1)
			bg := 1 - a // white background shows through
			o[dx*4] = toSRGB(acc[dx*4] + bg)
			o[dx*4+1] = toSRGB(acc[dx*4+1] + bg)
			o[dx*4+2] = toSRGB(acc[dx*4+2] + bg)
			o[dx*4+3] = 255
		}
	}
	return out
}

// rowFetcher returns a function that writes row y (0-based within bounds)
// of img as non-premultiplied 8-bit RGBA into dst. Concrete types avoid the
// per-pixel interface allocation of image.At.
func rowFetcher(img image.Image) func(y int, dst []uint8) {
	b := img.Bounds()
	w := b.Dx()
	switch m := img.(type) {
	case *image.YCbCr:
		return func(y int, dst []uint8) {
			py := b.Min.Y + y
			for x := 0; x < w; x++ {
				px := b.Min.X + x
				yi, ci := m.YOffset(px, py), m.COffset(px, py)
				r, g, bl := color.YCbCrToRGB(m.Y[yi], m.Cb[ci], m.Cr[ci])
				dst[x*4], dst[x*4+1], dst[x*4+2], dst[x*4+3] = r, g, bl, 255
			}
		}
	case *image.Gray:
		return func(y int, dst []uint8) {
			s := m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):]
			for x := 0; x < w; x++ {
				v := s[x]
				dst[x*4], dst[x*4+1], dst[x*4+2], dst[x*4+3] = v, v, v, 255
			}
		}
	case *image.NRGBA:
		return func(y int, dst []uint8) {
			i := m.PixOffset(b.Min.X, b.Min.Y+y)
			copy(dst, m.Pix[i:i+w*4])
		}
	case *image.RGBA:
		return func(y int, dst []uint8) {
			s := m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):]
			for x := 0; x < w; x++ {
				r, g, bl, a := s[x*4], s[x*4+1], s[x*4+2], s[x*4+3]
				if a != 0 && a != 255 {
					r = uint8(min(255, uint32(r)*255/uint32(a)))
					g = uint8(min(255, uint32(g)*255/uint32(a)))
					bl = uint8(min(255, uint32(bl)*255/uint32(a)))
				}
				dst[x*4], dst[x*4+1], dst[x*4+2], dst[x*4+3] = r, g, bl, a
			}
		}
	case *image.Paletted:
		var pal [256][4]uint8 // indexes beyond the palette stay transparent
		for i, c := range m.Palette {
			if i >= 256 {
				break
			}
			n := color.NRGBAModel.Convert(c).(color.NRGBA)
			pal[i] = [4]uint8{n.R, n.G, n.B, n.A}
		}
		return func(y int, dst []uint8) {
			s := m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):]
			for x := 0; x < w; x++ {
				copy(dst[x*4:x*4+4], pal[s[x]][:])
			}
		}
	case *image.CMYK:
		return func(y int, dst []uint8) {
			s := m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):]
			for x := 0; x < w; x++ {
				r, g, bl := color.CMYKToRGB(s[x*4], s[x*4+1], s[x*4+2], s[x*4+3])
				dst[x*4], dst[x*4+1], dst[x*4+2], dst[x*4+3] = r, g, bl, 255
			}
		}
	case *image.Gray16:
		return func(y int, dst []uint8) {
			s := m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):]
			for x := 0; x < w; x++ {
				v := s[x*2] // high byte (big-endian)
				dst[x*4], dst[x*4+1], dst[x*4+2], dst[x*4+3] = v, v, v, 255
			}
		}
	case *image.NRGBA64:
		return func(y int, dst []uint8) {
			s := m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):]
			for x := 0; x < w; x++ {
				dst[x*4], dst[x*4+1], dst[x*4+2], dst[x*4+3] = s[x*8], s[x*8+2], s[x*8+4], s[x*8+6]
			}
		}
	}
	return func(y int, dst []uint8) {
		for x := 0; x < w; x++ {
			n := color.NRGBAModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA)
			dst[x*4], dst[x*4+1], dst[x*4+2], dst[x*4+3] = n.R, n.G, n.B, n.A
		}
	}
}
