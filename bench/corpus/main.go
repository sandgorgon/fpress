// Command corpus writes the synthetic benchmark files used by bench/run.sh.
//
//	go run ./bench/corpus -out DIR
//
// Every file is generated from a fixed seed, so two runs (on any machine) write
// identical bytes and results can be compared between versions of fpress.
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
)

const side = 1024 // image-like files are side x side bytes (1 MiB)

func main() {
	out := flag.String("out", "corpus", "directory to write the files into")
	video := flag.Bool("video", false, "write only the five synthetic raw-video clips (RGB24, 320x180, 60 frames)")
	flag.Parse()
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	files := []struct {
		name string
		data []byte
	}{
		{"sierpinski.bin", sierpinski(side)},
		{"tilemap16.bin", tileMap(side, 16, 6, 5)},
		{"tilemap8-noisy.bin", noisy(tileMap(side, 8, 6, 5), 100)},
		{"records.bin", records(16384)},
		{"gradient.bin", gradient(side)},
		{"terrain.bin", terrain(side)},
		{"audio.bin", audio(1 << 20)},
		{"text.txt", text(1 << 20)},
		{"mixed.bin", mixed()},
		{"random.bin", random(1 << 20)},
		{"zeros.bin", make([]byte, 1<<20)},
	}
	if *video {
		files = []struct {
			name string
			data []byte
		}{
			{"video-screen.rgb", screenVideo()},
			{"video-cartoon.rgb", cartoonVideo()},
			{"video-camera.rgb", cameraVideo()},
			{"video-scroll.rgb", scrollVideo()},
			{"video-pan.rgb", panVideo()},
		}
	}
	for _, f := range files {
		p := filepath.Join(*out, f.name)
		if err := os.WriteFile(p, f.data, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("%-20s %9d bytes\n", f.name, len(f.data))
	}
}

// sierpinski is exactly self-similar: 255 where x&y == 0.
func sierpinski(n int) []byte {
	b := make([]byte, n*n)
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if x&y == 0 {
				b[y*n+x] = 255
			}
		}
	}
	return b
}

// tileMap is a grid of tiles drawn from a small palette of random tiles (sprite
// sheets, tile maps, glyph rows).
func tileMap(n, tile, palette int, seed int64) []byte {
	rng := rand.New(rand.NewSource(seed))
	tiles := make([][]byte, palette)
	for i := range tiles {
		tiles[i] = make([]byte, tile*tile)
		rng.Read(tiles[i])
	}
	b := make([]byte, n*n)
	for ty := 0; ty < n/tile; ty++ {
		for tx := 0; tx < n/tile; tx++ {
			p := tiles[rng.Intn(palette)]
			for y := 0; y < tile; y++ {
				copy(b[(ty*tile+y)*n+tx*tile:], p[y*tile:(y+1)*tile])
			}
		}
	}
	return b
}

// noisy replaces about one byte in oneIn with a random byte.
func noisy(b []byte, oneIn int) []byte {
	rng := rand.New(rand.NewSource(3))
	for i := range b {
		if rng.Intn(oneIn) == 0 {
			b[i] = byte(rng.Intn(256))
		}
	}
	return b
}

// records: 64-byte records with a counter, a timestamp, a few enumerated fields
// and a fixed text template.
func records(n int) []byte {
	rng := rand.New(rand.NewSource(8))
	var b bytes.Buffer
	ts := uint32(1_700_000_000)
	names := []string{"alpha", "bravo", "charlie", "delta", "echo"}
	for i := 0; i < n; i++ {
		rec := make([]byte, 64)
		binary.LittleEndian.PutUint32(rec[0:], uint32(i))
		ts += uint32(rng.Intn(5))
		binary.LittleEndian.PutUint32(rec[4:], ts)
		rec[8] = byte(rng.Intn(4))
		binary.LittleEndian.PutUint16(rec[10:], uint16(rng.Intn(1000)))
		copy(rec[16:], names[rng.Intn(len(names))])
		copy(rec[32:], "status=OK;region=eu-west;v=2")
		b.Write(rec)
	}
	return b.Bytes()
}

// gradient: a smooth ramp, the classic case for delta coding.
func gradient(n int) []byte {
	b := make([]byte, n*n)
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			b[y*n+x] = byte(x*3 + y)
		}
	}
	return b
}

// terrain: diamond-square fractal terrain, rough but only statistically self-similar.
func terrain(n int) []byte {
	rng := rand.New(rand.NewSource(9))
	g := make([][]float64, n+1)
	for i := range g {
		g[i] = make([]float64, n+1)
	}
	for step, amp := n, 128.0; step > 1; step, amp = step/2, amp/2 {
		h := step / 2
		for y := h; y < n; y += step {
			for x := h; x < n; x += step {
				g[y][x] = (g[y-h][x-h]+g[y-h][x+h]+g[y+h][x-h]+g[y+h][x+h])/4 + (rng.Float64()-.5)*amp
			}
		}
		for y := 0; y <= n; y += h {
			for x := 0; x <= n; x += h {
				if (x/h+y/h)%2 == 0 {
					continue
				}
				var s float64
				c := 0
				for _, d := range [][2]int{{-h, 0}, {h, 0}, {0, -h}, {0, h}} {
					if yy, xx := y+d[1], x+d[0]; yy >= 0 && yy <= n && xx >= 0 && xx <= n {
						s += g[yy][xx]
						c++
					}
				}
				g[y][x] = s/float64(c) + (rng.Float64()-.5)*amp
			}
		}
	}
	b := make([]byte, n*n)
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			b[y*n+x] = byte(int(g[y][x]) + 128)
		}
	}
	return b
}

// audio: 16-bit little-endian samples, two sine waves plus noise.
func audio(size int) []byte {
	rng := rand.New(rand.NewSource(4))
	b := make([]byte, size)
	for i := 0; i+1 < size; i += 2 {
		t := float64(i / 2)
		v := 9000*math.Sin(t*0.031) + 4000*math.Sin(t*0.173) + rng.NormFloat64()*120
		binary.LittleEndian.PutUint16(b[i:], uint16(int16(v)))
	}
	return b
}

// text: words drawn with a Zipf-like frequency from a small vocabulary, in lines.
func text(size int) []byte {
	rng := rand.New(rand.NewSource(2))
	vocab := strings.Fields(`the of and to in is that for it as was with be by on not he this are or his
from at which but have an had they you were their one all we can her has there been if more when will
would who so no she other its may these two like him into time has look than first water long little very
after words called just where most know get through back much before go good new write our used me man too
any day same right look think also around another came come work three word must because does part even place
well such here take why things help put years different away again off went old number great tell men say small
every found still between name should home big give air line set own under read last never us left end along
while might next sound below saw something thought both few those always looked show large often together asked
house don't world going want school important until form food keep children feet land side without boy once
animal life enough took sometimes four head above kind began almost live page got earth need far hand high year
mother light country father let night picture being study second soon story since white ever paper hard near
sentence better best across during today however sure knew try told young sun thing whole hear example heard
several change answer room sea against top turned learn point city play toward five using himself usually`)
	zipf := rand.NewZipf(rng, 1.15, 2, uint64(len(vocab)-1))
	var b bytes.Buffer
	col := 0
	for b.Len() < size {
		w := vocab[zipf.Uint64()]
		if col+len(w) > 78 {
			b.WriteByte('\n')
			col = 0
		} else if col > 0 {
			b.WriteByte(' ')
			col++
		}
		b.WriteString(w)
		col += len(w)
	}
	return b.Bytes()[:size]
}

func random(n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(1)).Read(b)
	return b
}

// mixed: regions that each want a different treatment, back to back.
func mixed() []byte {
	var b []byte
	b = append(b, gradient(256)...)
	b = append(b, random(64<<10)...)
	b = append(b, sierpinski(256)...)
	b = append(b, make([]byte, 32<<10)...)
	b = append(b, records(1024)...)
	b = append(b, text(64<<10)...)
	return b
}
