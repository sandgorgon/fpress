package main

import (
	"math"
	"math/rand"
)

// Synthetic raw video: RGB24, frames stored one after another, no headers.
// 320x180 pixels, 60 frames, 172,800 bytes per frame (960 bytes per row).
const (
	vw, vh, vframes = 320, 180, 60
	vstride         = vw * 3
)

type frame [vh][vw][3]byte

func (f *frame) fill(x0, y0, w, h int, r, g, b byte) {
	for y := max(y0, 0); y < min(y0+h, vh); y++ {
		for x := max(x0, 0); x < min(x0+w, vw); x++ {
			f[y][x] = [3]byte{r, g, b}
		}
	}
}

func (f *frame) bytes(dst []byte) []byte {
	for y := 0; y < vh; y++ {
		for x := 0; x < vw; x++ {
			dst = append(dst, f[y][x][:]...)
		}
	}
	return dst
}

// screenVideo: a static desktop (panels and rows of "text" made from a few glyph
// tiles), a mouse-cursor square that moves, and a clock that changes once a second.
// Everything outside those is exactly identical from frame to frame.
func screenVideo() []byte {
	rng := rand.New(rand.NewSource(11))
	var glyphs [8][8 * 8][3]byte
	for i := range glyphs {
		for j := range glyphs[i] {
			if rng.Intn(3) == 0 {
				glyphs[i][j] = [3]byte{20, 20, 30}
			} else {
				glyphs[i][j] = [3]byte{235, 235, 240}
			}
		}
	}
	var base frame
	base.fill(0, 0, vw, vh, 235, 235, 240)
	base.fill(0, 0, vw, 14, 40, 60, 110)       // title bar
	base.fill(0, 14, 60, vh-14, 210, 215, 225) // side panel
	for row := 0; row < 14; row++ {            // text lines
		for col := 0; col < 28; col++ {
			g := glyphs[rng.Intn(len(glyphs))]
			for j := 0; j < 64; j++ {
				base[22+row*10+j/8][70+col*8+j%8] = g[j]
			}
		}
	}
	var out []byte
	for n := 0; n < vframes; n++ {
		f := base
		sec := n / 10
		for d := 0; d < 4; d++ { // clock digits, change every 10 frames
			g := glyphs[(sec+d*3)%len(glyphs)]
			for j := 0; j < 64; j++ {
				f[3+j/8][vw-40+d*8+j%8] = g[j]
			}
		}
		cx, cy := 20+n*4, 40+int(30*math.Sin(float64(n)/6))
		f.fill(cx, cy, 6, 6, 255, 40, 40)
		out = f.bytes(out)
	}
	return out
}

// cartoonVideo: flat-colour animation: a gradient sky, ground, and a few sprites
// moving at different speeds. No noise.
func cartoonVideo() []byte {
	var out []byte
	for n := 0; n < vframes; n++ {
		var f frame
		for y := 0; y < vh; y++ {
			c := byte(120 + y/3)
			for x := 0; x < vw; x++ {
				f[y][x] = [3]byte{c / 2, c, 255}
			}
		}
		f.fill(0, 140, vw, 40, 60, 150, 60)
		for s := 0; s < 4; s++ {
			x := (s*90 + n*(2+s)) % (vw + 30)
			f.fill(x-20, 110-s*18, 20, 20, byte(200-s*40), byte(60+s*50), byte(30+s*60))
			f.fill(x-16, 114-s*18, 4, 4, 255, 255, 255)
		}
		out = f.bytes(out)
	}
	return out
}

// cameraVideo: a textured scene that pans one pixel per frame, with sensor noise
// (Gaussian, sigma 2) added independently to every frame.
func cameraVideo() []byte {
	rng := rand.New(rand.NewSource(5))
	const sw = vw + vframes + 8
	scene := make([][3]float64, sw*vh)
	for y := 0; y < vh; y++ {
		for x := 0; x < sw; x++ {
			v := 110 + 50*math.Sin(float64(x)/23+float64(y)/31) + 25*math.Sin(float64(x)/5.1) + 15*math.Sin(float64(y)/3.7+float64(x)/9)
			scene[y*sw+x] = [3]float64{v, v * 0.9, v * 0.8}
		}
	}
	var out []byte
	for n := 0; n < vframes; n++ {
		for y := 0; y < vh; y++ {
			for x := 0; x < vw; x++ {
				p := scene[y*sw+x+n]
				for c := 0; c < 3; c++ {
					out = append(out, byte(math.Max(0, math.Min(255, p[c]+rng.NormFloat64()*2))))
				}
			}
		}
	}
	return out
}

// scrollVideo: a web-page style screen recording. A tall page of "text" (lines made
// from a few glyph tiles) scrolls up 3 rows per frame under a fixed title bar and
// a fixed side panel. Every pixel of the page moves, none changes.
func scrollVideo() []byte {
	rng := rand.New(rand.NewSource(21))
	var glyphs [10][8 * 8][3]byte
	for i := range glyphs {
		for j := range glyphs[i] {
			if rng.Intn(3) == 0 {
				glyphs[i][j] = [3]byte{20, 20, 30}
			} else {
				glyphs[i][j] = [3]byte{245, 245, 240}
			}
		}
	}
	const pageH = vh + vframes*3 + 16
	page := make([][vw]([3]byte), pageH)
	for y := range page {
		for x := range page[y] {
			page[y][x] = [3]byte{245, 245, 240}
		}
	}
	for line := 0; line+10 < pageH; line += 10 {
		if rng.Intn(6) == 0 {
			continue // blank line between paragraphs
		}
		for col := 0; col < 30; col++ {
			if rng.Intn(5) == 0 {
				continue
			}
			g := glyphs[rng.Intn(len(glyphs))]
			for j := 0; j < 64; j++ {
				page[line+j/8][70+col*8+j%8] = g[j]
			}
		}
	}
	var out []byte
	for n := 0; n < vframes; n++ {
		var f frame
		for y := 0; y < vh; y++ {
			for x := 0; x < vw; x++ {
				f[y][x] = page[y+n*3][x]
			}
		}
		f.fill(0, 0, vw, 14, 40, 60, 110)
		f.fill(0, 14, 60, vh-14, 210, 215, 225)
		out = f.bytes(out)
	}
	return out
}

// panVideo: flat-colour animation seen through a camera that pans right 2 pixels
// per frame (hills and clouds are part of the scene, a sprite moves on its own).
func panVideo() []byte {
	const sw = vw + vframes*2 + 4
	scene := make([][sw][3]byte, vh)
	for y := 0; y < vh; y++ {
		c := byte(120 + y/3)
		for x := 0; x < sw; x++ {
			scene[y][x] = [3]byte{c / 2, c, 255}
		}
	}
	for x := 0; x < sw; x++ { // hills
		top := 120 + int(18*math.Sin(float64(x)/17)) + int(9*math.Sin(float64(x)/5.3))
		for y := top; y < vh; y++ {
			scene[y][x] = [3]byte{60, byte(130 + (y-top)/2), 60}
		}
	}
	for c := 0; c < 9; c++ { // clouds and trees: flat rectangles at fixed scene positions
		x0, y0 := c*47+11, 20+(c*29)%50
		for y := y0; y < y0+10; y++ {
			for x := x0; x < x0+24 && x < sw; x++ {
				scene[y][x] = [3]byte{250, 250, 250}
			}
		}
	}
	var out []byte
	for n := 0; n < vframes; n++ {
		var f frame
		for y := 0; y < vh; y++ {
			for x := 0; x < vw; x++ {
				f[y][x] = scene[y][x+n*2]
			}
		}
		f.fill(40+n*3%200, 100, 14, 14, 200, 40, 40) // sprite with its own motion
		out = f.bytes(out)
	}
	return out
}
