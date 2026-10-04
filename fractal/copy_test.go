package fractal

import (
	"math/rand"
	"testing"

	"fpress/matrix"
	"fpress/transform"
)

// tileMap is a grid of tiles drawn from a small palette of random tiles: the
// kind of data (sprite sheets, tile maps, glyph rows) with exact copies of
// blocks at the same scale.
func tileMap(size, tile, palette int, seed int64) *matrix.Dense {
	rng := rand.New(rand.NewSource(seed))
	tiles := make([][]byte, palette)
	for i := range tiles {
		tiles[i] = make([]byte, tile*tile)
		rng.Read(tiles[i])
	}
	m := matrix.NewDense(size, size)
	for ty := 0; ty < size/tile; ty++ {
		for tx := 0; tx < size/tile; tx++ {
			p := tiles[rng.Intn(palette)]
			for y := 0; y < tile; y++ {
				for x := 0; x < tile; x++ {
					m.Set(tx*tile+x, ty*tile+y, p[y*tile+x])
				}
			}
		}
	}
	return m
}

func TestSameScaleCopiesShrinkTileMaps(t *testing.T) {
	m := tileMap(1024, 8, 6, 5) // 16384 tiles from 6 palette tiles: repeats dominate
	size := func(noCopy bool) (int, Stats) {
		opt := DefaultOptions()
		opt.NoSameScale = noCopy
		tl, st, _, err := EncodeTiled(m, opt)
		if err != nil {
			t.Fatal(err)
		}
		blob, _ := tl.MarshalBinary()
		got, err := DecodeTiled(tl, 0)
		if err != nil || !matrix.Equal(got, m) {
			t.Fatalf("noCopy=%v: round trip failed: %v", noCopy, err)
		}
		return len(blob), st
	}
	without, _ := size(true)
	with, st := size(false)
	t.Logf("tile map 1024x1024 (6 random 8x8 tiles): without copies %d bytes, with %d bytes (%d copy recipes)", without, with, st.CopyRecipes)
	if st.CopyRecipes == 0 {
		t.Error("no same-scale recipes were used on a tile map")
	}
	// Before copies were made acyclic this came out ~4.7x LARGER (thousands of
	// identical blocks copying from each other and never anchored).
	if with*3 > without*2 {
		t.Errorf("same-scale copies should shrink this tile map to at most 2/3: %d vs %d", with, without)
	}
	// With the feature off no recipe may be same-scale.
	opt := DefaultOptions()
	opt.NoSameScale = true
	c, _, err := Encode(m, opt)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range c.Transforms {
		if tr.SameScale {
			t.Fatal("a same-scale recipe appeared with NoSameScale set")
		}
	}
}

func TestSameScaleValidationAndFormat(t *testing.T) {
	good := Compressed{Width: 32, Height: 32, Block: 16, Iterations: 4}
	good.Transforms = []Transform{{RX: 0, RY: 0, Size: 16, DX: 16, DY: 16, SameScale: true}}
	if err := good.validate(); err != nil {
		t.Fatalf("a valid same-scale recipe was rejected: %v", err)
	}
	// A same-scale domain is only Size wide, so it may sit where a 2:1 domain could not.
	edge := good
	edge.Transforms = []Transform{{RX: 0, RY: 0, Size: 16, DX: 16, DY: 16, SameScale: true}}
	wide := edge
	wide.Transforms = []Transform{{RX: 0, RY: 0, Size: 16, DX: 16, DY: 16, SameScale: false}}
	if err := wide.validate(); err == nil {
		t.Error("a 2:1 domain running off the grid was accepted")
	}
	bad := map[string]Transform{
		"domain off the grid": {RX: 0, RY: 0, Size: 16, DX: 17, DY: 0, SameScale: true},
		"negative domain":     {RX: 0, RY: 0, Size: 16, DX: -1, DY: 0, SameScale: true},
		"raw with a domain":   {RX: 0, RY: 0, Size: 16, Raw: true, Data: make([]byte, 256), SameScale: true},
	}
	for name, tr := range bad {
		c := good
		c.Transforms = []Transform{tr}
		if err := c.validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// The flag survives serialization.
	ts := []Transform{
		{RX: 0, RY: 0, Size: 8, DX: 8, DY: 8, SameScale: true, Iso: transform.IsoRot90, Map: transform.ValueMap{Kind: transform.MapAdd, C: 3}},
		{RX: 8, RY: 0, Size: 8, DX: 0, DY: 0},
	}
	back, err := decodeRecords(encodeRecords(ts), 2, 32, 32)
	if err != nil || !back[0].SameScale || back[1].SameScale {
		t.Fatalf("same-scale flag lost: %+v %v", back, err)
	}
}

func TestSameScaleRoundTripsOnManyInputs(t *testing.T) {
	rng := rand.New(rand.NewSource(12))
	for trial := 0; trial < 12; trial++ {
		w, h := 20+rng.Intn(120), 20+rng.Intn(120)
		m := matrix.NewDense(w, h)
		tile := 2 + rng.Intn(10)
		pal := make([][]byte, 1+rng.Intn(5))
		for i := range pal {
			pal[i] = make([]byte, tile*tile)
			rng.Read(pal[i])
		}
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				ty, tx := y/tile, x/tile
				p := pal[(ty*7+tx*3)%len(pal)]
				m.Set(x, y, p[(y%tile)*tile+x%tile]+byte(trial%3)*byte(rng.Intn(2)))
			}
		}
		opt := DefaultOptions()
		opt.Block, opt.MinBlock, opt.Stride = 8, 4, 2
		opt.Workers = 1 + trial%3
		roundTripTiled(t, "tiles", m, opt)
	}
}

func TestCopyIsCausal(t *testing.T) {
	// The node is the 8x8 block at (16, 16). A domain is earlier in raster
	// order if its last row is above the node's first row, or is that row and
	// the domain ends left of the node's first column.
	for _, c := range []struct {
		dx, dy int
		want   bool
	}{
		{0, 0, true},    // above and to the left
		{40, 0, true},   // above and to the right: earlier rows are all earlier
		{16, 8, true},   // directly above, last row 15
		{0, 9, true},    // last row 16 is the node's row, columns 0..7 end left of column 16
		{8, 9, true},    // columns 8..15: still left of column 16
		{9, 9, false},   // columns 9..16 reach the node's first column
		{0, 10, false},  // last row 17 is below the node's first row
		{20, 12, false}, // overlaps the node
		{0, 16, false},  // starts on the node's row and runs below it
	} {
		if got := copyIsCausal(c.dx, c.dy, 8, 16, 16); got != c.want {
			t.Errorf("copyIsCausal(%d,%d) = %v, want %v", c.dx, c.dy, got, c.want)
		}
	}
}

// Identical blocks must never be able to copy from each other: every copy reads
// data that comes earlier in raster order, so the first occurrence is an
// anchor. This is the regression test for the cycle bug.
func TestCopiesFormNoCycles(t *testing.T) {
	m := tileMap(256, 8, 3, 9) // 1024 tiles, only 3 distinct
	opt := DefaultOptions()
	opt.Block, opt.MinBlock, opt.Stride, opt.Workers = 8, 8, 4, 1
	opt.MaxRefine = 0 // no repair loop: the plan itself must already be acyclic
	// Look at the copy plan itself, not at what Encode finally keeps (which is
	// whichever of the with-copies and without-copies plans is smaller).
	e := newTestEncoder(m, opt)
	e.match()
	if err := e.finish(matrix.Bytes(m), true); err != nil {
		t.Fatal(err)
	}
	if e.st.CopyRecipes == 0 {
		t.Fatal("expected same-scale copies")
	}
	if e.st.Demoted != 0 {
		t.Errorf("%d recipes needed demotion although MaxRefine is 0", e.st.Demoted)
	}
	a := reconstruct(e.c, 1)
	wrong := 0
	for y := 0; y < m.Height(); y++ {
		for x := 0; x < m.Width(); x++ {
			if a.At(x, y) != m.At(x, y) {
				wrong++
			}
		}
	}
	if wrong != 0 {
		t.Errorf("%d cells differ after the iterations: the copies did not converge to the original", wrong)
	}
}

// Turning copies on may only ever help: a tile is encoded with and without
// them when they were used, and the smaller kept. This is the regression test
// for inputs where copies replaced data the coder already handled cheaply
// (a mixed file came out 0.9% larger before the guard).
func TestCopiesNeverMakeOutputLarger(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	mixed := matrix.NewDense(256, 256)
	for y := 0; y < 256; y++ {
		for x := 0; x < 256; x++ {
			switch {
			case y < 64:
				mixed.Set(x, y, byte(3*x+y)) // gradient rows: copies abound
			case y < 128:
				mixed.Set(x, y, byte(rng.Intn(256))) // noise
			case y < 192:
				mixed.Set(x, y, byte((x/4+y/4)&1)*200) // checker
			default:
				mixed.Set(x, y, byte(x&y)*3) // fractal-ish
			}
		}
	}
	inputs := map[string]matrix.Matrix{
		"mixed": mixed, "tile map": tileMap(256, 8, 6, 2), "sierpinski": sierpinski(256),
		"repeated rows": repeatedRows(200, 150), "smooth": smoothField(200, 160),
		"noise": matrix.FromBytes(randomBytes(40000, 8), 200),
	}
	for name, m := range inputs {
		size := func(noCopy bool) int {
			opt := DefaultOptions()
			opt.NoSameScale = noCopy
			tl, _, _, err := EncodeTiled(m, opt)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := tl.MarshalBinary()
			return len(b)
		}
		with, without := size(false), size(true)
		t.Logf("%-14s without copies %6d  with %6d", name, without, with)
		if with > without {
			t.Errorf("%s: copies made the output larger (%d > %d)", name, with, without)
		}
	}
}
