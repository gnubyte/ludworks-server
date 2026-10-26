package main

import (
	"image"
	"image/color"
)

// The Ludworks mark is the web UI's logo (web/assets/img/logo.svg and favicon.svg): a rounded
// indigo square with three white "drive bays" — two full bars and a short one next to a
// translucent status light. It is rasterised here with exact integer arithmetic (16×16
// supersampling per pixel), so the result is identical on every platform and Go version.

type shapeKind int

const (
	roundRect shapeKind = iota
	circle
)

// shape coordinates are integers on the design grid (see design.grid).
type shape struct {
	kind           shapeKind
	x0, y0, x1, y1 int64 // roundRect bounds
	cx, cy         int64 // circle centre
	r              int64 // corner radius / circle radius
	white          bool  // foreground (white) or background
	opacity        int64 // 0..255
}

type rgb struct{ r, g, b int64 }

type design struct {
	grid     int64 // design units per image side
	from, to rgb   // background gradient along the top-left → bottom-right diagonal
	shapes   []shape
}

// largeMark is logo.svg (180×180) scaled ×4.
var largeMark = design{
	grid: 720,
	from: rgb{0x6b, 0x6b, 0xe0},
	to:   rgb{0x4a, 0x4a, 0xc4},
	shapes: []shape{
		{kind: roundRect, x0: 0, y0: 0, x1: 720, y1: 720, r: 160, opacity: 255},
		{kind: roundRect, x0: 180, y0: 192, x1: 540, y1: 270, r: 39, white: true, opacity: 255},
		{kind: roundRect, x0: 180, y0: 321, x1: 540, y1: 399, r: 39, white: true, opacity: 255},
		{kind: roundRect, x0: 180, y0: 450, x1: 382, y1: 528, r: 39, white: true, opacity: 255},
		{kind: circle, cx: 495, cy: 489, r: 42, white: true, opacity: 153},
	},
}

// smallMark is favicon.svg (32×32, thicker bars for tiny sizes) scaled ×20.
var smallMark = design{
	grid: 640,
	from: rgb{0x5b, 0x5b, 0xd6},
	to:   rgb{0x5b, 0x5b, 0xd6},
	shapes: []shape{
		{kind: roundRect, x0: 20, y0: 20, x1: 620, y1: 620, r: 160, opacity: 255},
		{kind: roundRect, x0: 160, y0: 170, x1: 480, y1: 240, r: 35, white: true, opacity: 255},
		{kind: roundRect, x0: 160, y0: 285, x1: 480, y1: 355, r: 35, white: true, opacity: 255},
		{kind: roundRect, x0: 160, y0: 400, x1: 340, y1: 470, r: 35, white: true, opacity: 255},
		{kind: circle, cx: 440, cy: 435, r: 38, white: true, opacity: 153},
	},
}

// IconSizes are the sizes written into the .ico.
var IconSizes = []int{16, 32, 48, 256}

// RenderLogo draws the mark at size×size pixels.
func RenderLogo(size int) *image.NRGBA {
	d := largeMark
	if size <= 32 {
		d = smallMark
	}
	return d.render(size)
}

const samples = 16 // per axis and pixel

func (d design) render(size int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	n := int64(size)
	// A sample with index s (0 ≤ s < size·samples) sits at design coordinate
	// (2s+1)·grid / (2·size·samples). Multiplying everything by k = 2·size·samples keeps the
	// inside tests in exact integers.
	k := 2 * n * samples
	const full = samples * samples
	const scale = full * 255 // coverage unit
	for py := int64(0); py < n; py++ {
		for px := int64(0); px < n; px++ {
			var bgCount, fg int64 // fg in units of 1/scale
			for _, s := range d.shapes {
				c := s.coverage(px, py, k, d.grid)
				if c == 0 {
					continue
				}
				if s.white {
					fg += c * s.opacity
				} else {
					bgCount += c
				}
			}
			if bgCount == 0 && fg == 0 {
				continue
			}
			// Background colour at the pixel centre: t = (x+y)/2 on the unit square.
			num, den := (2*px+1)+(2*py+1), 4*n
			bg := rgb{
				(d.from.r*(den-num) + d.to.r*num + den/2) / den,
				(d.from.g*(den-num) + d.to.g*num + den/2) / den,
				(d.from.b*(den-num) + d.to.b*num + den/2) / den,
			}
			// White "over" the background, all in units of 1/scale²:
			//   alpha = fg + bgA·(1−fg),  colour·alpha = 255·fg + bg·bgA·(1−fg)
			bgA := bgCount * 255
			alpha := fg*scale + bgA*(scale-fg)
			if alpha == 0 {
				continue
			}
			ch := func(c int64) uint8 { return uint8((255*fg*scale + c*bgA*(scale-fg) + alpha/2) / alpha) }
			img.SetNRGBA(int(px), int(py), color.NRGBA{
				R: ch(bg.r), G: ch(bg.g), B: ch(bg.b),
				A: uint8((alpha*255 + scale*scale/2) / (scale * scale)),
			})
		}
	}
	return img
}

// coverage counts the samples of pixel (px,py) inside the shape (0..samples²).
func (s shape) coverage(px, py, k, grid int64) int64 {
	// Quick reject on bounding boxes (pixel edges at px·2·samples·grid in k-scaled units).
	x0, x1, y0, y1 := s.x0, s.x1, s.y0, s.y1
	if s.kind == circle {
		x0, x1, y0, y1 = s.cx-s.r, s.cx+s.r, s.cy-s.r, s.cy+s.r
	}
	edge := 2 * samples * grid
	if px*edge > x1*k || (px+1)*edge < x0*k || py*edge > y1*k || (py+1)*edge < y0*k {
		return 0
	}
	var count int64
	for sy := py * samples; sy < (py+1)*samples; sy++ {
		y := (2*sy + 1) * grid
		for sx := px * samples; sx < (px+1)*samples; sx++ {
			x := (2*sx + 1) * grid
			if s.contains(x, y, k) {
				count++
			}
		}
	}
	return count
}

func (s shape) contains(x, y, k int64) bool {
	r := s.r * k
	switch s.kind {
	case circle:
		dx, dy := x-s.cx*k, y-s.cy*k
		return dx*dx+dy*dy <= r*r
	default:
		qx := clamp(x, s.x0*k+r, s.x1*k-r)
		qy := clamp(y, s.y0*k+r, s.y1*k-r)
		dx, dy := x-qx, y-qy
		return dx*dx+dy*dy <= r*r
	}
}

func clamp(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
