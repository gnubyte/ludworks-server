package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
)

// IconImage is one image of an .ico file. Data is the raw entry exactly as stored: a PNG
// stream (256 px) or a BITMAPINFOHEADER DIB with an AND mask (smaller sizes, which older
// shells render best).
type IconImage struct {
	Width, Height int
	BitCount      uint16
	Data          []byte
}

// EncodeICO builds an .ico from square images.
func EncodeICO(images []*image.NRGBA) ([]byte, error) {
	entries := make([]IconImage, 0, len(images))
	for _, img := range images {
		w, h := img.Rect.Dx(), img.Rect.Dy()
		if w != h || w < 1 || w > 256 {
			return nil, fmt.Errorf("icon images must be square and 1..256 px, got %dx%d", w, h)
		}
		var data []byte
		if w >= 256 {
			var buf bytes.Buffer
			enc := png.Encoder{CompressionLevel: png.BestCompression}
			if err := enc.Encode(&buf, img); err != nil {
				return nil, err
			}
			data = buf.Bytes()
		} else {
			data = encodeDIB(img)
		}
		entries = append(entries, IconImage{Width: w, Height: h, BitCount: 32, Data: data})
	}
	var b bytes.Buffer
	le := binary.LittleEndian
	_ = binary.Write(&b, le, [3]uint16{0, 1, uint16(len(entries))}) // ICONDIR
	offset := 6 + 16*len(entries)
	for _, e := range entries {
		_ = binary.Write(&b, le, iconDirEntry{
			Width: dim(e.Width), Height: dim(e.Height), Planes: 1, BitCount: e.BitCount,
			BytesInRes: uint32(len(e.Data)), Offset: uint32(offset),
		})
		offset += len(e.Data)
	}
	for _, e := range entries {
		b.Write(e.Data)
	}
	return b.Bytes(), nil
}

// iconDirEntry is ICONDIRENTRY (16 bytes).
type iconDirEntry struct {
	Width, Height, ColorCount, Reserved uint8
	Planes, BitCount                    uint16
	BytesInRes, Offset                  uint32
}

// dim encodes a width/height byte: 0 means 256.
func dim(n int) uint8 {
	if n >= 256 {
		return 0
	}
	return uint8(n)
}

func undim(b uint8) int {
	if b == 0 {
		return 256
	}
	return int(b)
}

// encodeDIB writes a 32-bit BI_RGB icon bitmap: header with doubled height, bottom-up BGRA
// rows (straight alpha), then the 1-bpp AND mask (1 = transparent), rows padded to 32 bits.
func encodeDIB(img *image.NRGBA) []byte {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	maskStride := ((w + 31) / 32) * 4
	var b bytes.Buffer
	le := binary.LittleEndian
	_ = binary.Write(&b, le, bitmapInfoHeader{
		Size: 40, Width: int32(w), Height: int32(2 * h), Planes: 1, BitCount: 32,
		SizeImage: uint32(w*h*4 + maskStride*h),
	})
	for y := h - 1; y >= 0; y-- {
		for x := 0; x < w; x++ {
			c := img.NRGBAAt(img.Rect.Min.X+x, img.Rect.Min.Y+y)
			b.Write([]byte{c.B, c.G, c.R, c.A})
		}
	}
	for y := h - 1; y >= 0; y-- {
		row := make([]byte, maskStride)
		for x := 0; x < w; x++ {
			if img.NRGBAAt(img.Rect.Min.X+x, img.Rect.Min.Y+y).A == 0 {
				row[x/8] |= 0x80 >> (x % 8)
			}
		}
		b.Write(row)
	}
	return b.Bytes()
}

// bitmapInfoHeader is BITMAPINFOHEADER (40 bytes).
type bitmapInfoHeader struct {
	Size                         uint32
	Width, Height                int32
	Planes, BitCount             uint16
	Compression, SizeImage       uint32
	XPelsPerMeter, YPelsPerMeter int32
	ClrUsed, ClrImportant        uint32
}

// DecodeICO parses an .ico into its raw entries.
func DecodeICO(b []byte) ([]IconImage, error) {
	if len(b) < 6 {
		return nil, errors.New("ico: truncated header")
	}
	le := binary.LittleEndian
	if le.Uint16(b[0:]) != 0 || le.Uint16(b[2:]) != 1 {
		return nil, errors.New("ico: not an icon file")
	}
	n := int(le.Uint16(b[4:]))
	if n == 0 || n > 64 || len(b) < 6+16*n {
		return nil, fmt.Errorf("ico: bad image count %d", n)
	}
	out := make([]IconImage, 0, n)
	for i := 0; i < n; i++ {
		var e iconDirEntry
		if err := binary.Read(bytes.NewReader(b[6+16*i:]), le, &e); err != nil {
			return nil, err
		}
		end := uint64(e.Offset) + uint64(e.BytesInRes)
		if e.BytesInRes == 0 || end > uint64(len(b)) {
			return nil, fmt.Errorf("ico: image %d out of bounds", i)
		}
		out = append(out, IconImage{
			Width: undim(e.Width), Height: undim(e.Height), BitCount: e.BitCount,
			Data: b[e.Offset:end],
		})
	}
	return out, nil
}

// Decode returns the pixels of an icon entry (PNG or 32-bit DIB).
func (e IconImage) Decode() (*image.NRGBA, error) {
	if bytes.HasPrefix(e.Data, []byte("\x89PNG\r\n\x1a\n")) {
		img, err := png.Decode(bytes.NewReader(e.Data))
		if err != nil {
			return nil, err
		}
		out := image.NewNRGBA(img.Bounds())
		for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
			for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
				out.Set(x, y, img.At(x, y))
			}
		}
		return out, nil
	}
	var hdr bitmapInfoHeader
	if err := binary.Read(bytes.NewReader(e.Data), binary.LittleEndian, &hdr); err != nil {
		return nil, err
	}
	w, h := int(hdr.Width), int(hdr.Height)/2
	if hdr.Size != 40 || hdr.BitCount != 32 || hdr.Compression != 0 || w != e.Width || h != e.Height {
		return nil, fmt.Errorf("ico: unsupported bitmap %+v", hdr)
	}
	pix := e.Data[40:]
	if len(pix) < w*h*4 {
		return nil, errors.New("ico: truncated bitmap")
	}
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		row := pix[(h-1-y)*w*4:]
		for x := 0; x < w; x++ {
			p := row[x*4:]
			img.SetNRGBA(x, y, color.NRGBA{R: p[2], G: p[1], B: p[0], A: p[3]})
		}
	}
	return img, nil
}

// LogoICO renders the logo at every size in IconSizes and encodes it as an .ico.
func LogoICO() ([]byte, error) {
	imgs := make([]*image.NRGBA, len(IconSizes))
	for i, s := range IconSizes {
		imgs[i] = RenderLogo(s)
	}
	return EncodeICO(imgs)
}
