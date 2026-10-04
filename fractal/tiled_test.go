package fractal

import (
	"bytes"
	"math/rand"
	"testing"

	"fpress/matrix"
)

func tiledOpts(tile, workers int) Options {
	o := DefaultOptions()
	o.Tile, o.Workers = tile, workers
	return o
}

func roundTripTiled(t *testing.T, name string, m matrix.Matrix, opt Options) (*Tiled, TileStats) {
	t.Helper()
	tl, _, ts, err := EncodeTiled(m, opt)
	if err != nil {
		t.Fatalf("%s: EncodeTiled: %v", name, err)
	}
	blob, err := tl.MarshalBinary()
	if err != nil {
		t.Fatalf("%s: Marshal: %v", name, err)
	}
	var back Tiled
	if err := back.UnmarshalBinary(blob); err != nil {
		t.Fatalf("%s: Unmarshal: %v", name, err)
	}
	got, err := DecodeTiled(&back, opt.Workers)
	if err != nil {
		t.Fatalf("%s: DecodeTiled: %v", name, err)
	}
	if !matrix.Equal(got, m) {
		t.Fatalf("%s: decoded matrix differs from the original", name)
	}
	return tl, ts
}

func TestTiledRoundTrip(t *testing.T) {
	shapes := [][2]int{{0, 0}, {0, 7}, {7, 0}, {1, 1}, {5, 3}, {16, 16}, {17, 16}, {64, 64}, {100, 37}, {130, 130}, {300, 70}}
	for _, tile := range []int{8, 16, 64, 256} {
		for _, sh := range shapes {
			w, h := sh[0], sh[1]
			rng := rand.New(rand.NewSource(int64(w*1000 + h + tile)))
			data := make([]byte, w*h)
			for i := range data {
				// part structured, part noise
				if (i/w+i%max(w, 1))%7 < 4 {
					data[i] = byte((i % max(w, 1)) / 3)
				} else {
					data[i] = byte(rng.Intn(4))
				}
			}
			m := matrix.NewDense(w, h)
			copy(m.Pix(), data)
			roundTripTiled(t, "mixed", m, tiledOpts(tile, 3))
		}
	}
}

func TestTiledStructuredData(t *testing.T) {
	roundTripTiled(t, "sierpinski-200", sierpinski(200), tiledOpts(64, 4))
	roundTripTiled(t, "repeated-rows", repeatedRows(150, 90), tiledOpts(50, 2))
	roundTripTiled(t, "smooth", smoothField(333, 111), tiledOpts(128, 0))
}

func TestTiledPicksTheRightModePerTile(t *testing.T) {
	// Left half: random (nothing helps -> stored). Right half: zeros (flate
	// or fractal wins). A self-similar tile somewhere should go fractal.
	const n = 128
	m := matrix.NewDense(n*2, n)
	rng := rand.New(rand.NewSource(1))
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			m.Set(x, y, byte(rng.Intn(256)))
		}
	}
	tl, ts := roundTripTiled(t, "halves", m, tiledOpts(n, 2))
	if tl.Tiles[0].Mode != TileStored {
		t.Errorf("random tile stored as mode %d, want stored", tl.Tiles[0].Mode)
	}
	if tl.Tiles[1].Mode == TileStored {
		t.Errorf("all-zero tile should not be stored raw")
	}
	if ts.Tiles != 2 || ts.Stored+ts.Flate+ts.Fractal != 2 {
		t.Errorf("tile stats inconsistent: %+v", ts)
	}
	_, ts = roundTripTiled(t, "sierpinski", sierpinski(128), tiledOpts(128, 1))
	if ts.Fractal != 1 {
		t.Errorf("self-similar tile should be fractal, got %+v", ts)
	}
}

func TestTiledOutputIsIndependentOfWorkers(t *testing.T) {
	m := sierpinski(256)
	var ref []byte
	for _, workers := range []int{1, 2, 3, 8, 64} {
		tl, _, _, err := EncodeTiled(m, tiledOpts(64, workers))
		if err != nil {
			t.Fatal(err)
		}
		blob, _ := tl.MarshalBinary()
		if ref == nil {
			ref = blob
		} else if !bytes.Equal(ref, blob) {
			t.Fatalf("output with %d workers differs from 1 worker", workers)
		}
	}
}

// The matrix passed in need not be safe for concurrent use, and any
// implementation must give the same result.
func TestTiledAcceptsAnyMatrix(t *testing.T) {
	data := make([]byte, 90*70)
	rand.New(rand.NewSource(5)).Read(data)
	for i := range data {
		data[i] %= 6
	}
	dense := matrix.FromBytes(data, 90)
	fm, err := matrix.NewFileMatrix(bytes.NewReader(data), int64(len(data)), 90) // has an unsynchronised cache
	if err != nil {
		t.Fatal(err)
	}
	big := matrix.NewDense(120, 100)
	matrix.Copy(matrix.NewMutableView(big, 11, 13, 90, 70), dense)

	ref, _, _, err := EncodeTiled(dense, tiledOpts(32, 1))
	if err != nil {
		t.Fatal(err)
	}
	refBlob, _ := ref.MarshalBinary()
	for name, m := range map[string]matrix.Matrix{
		"func": matrix.Func{W: 90, H: 70, F: dense.At},
		"file": fm,
		"view": matrix.NewView(big, 11, 13, 90, 70),
	} {
		tl, _, _, err := EncodeTiled(m, tiledOpts(32, 8))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		blob, _ := tl.MarshalBinary()
		if !bytes.Equal(blob, refBlob) {
			t.Errorf("%s: output differs from the Dense encoding", name)
		}
	}
}

// capture (defined in fractal_test.go) is a plain map: not safe for concurrent writes.

func TestDecodeTiledIntoUnsafeDestinationWithWorkers(t *testing.T) {
	src := sierpinski(200)
	tl, _, _, err := EncodeTiled(src, tiledOpts(32, 4))
	if err != nil {
		t.Fatal(err)
	}
	dst := &capture{w: 200, h: 200, m: map[[2]int]byte{}}
	if err := DecodeTiledInto(dst, tl, 8); err != nil { // would race if tiles wrote to dst concurrently
		t.Fatal(err)
	}
	if !matrix.Equal(dst, src) {
		t.Fatal("wrong contents")
	}
	if err := DecodeTiledInto(&capture{w: 199, h: 200, m: map[[2]int]byte{}}, tl, 1); err == nil {
		t.Fatal("expected a dimension error")
	}
}

func TestTiledRejectsDamage(t *testing.T) {
	tl, _, _, err := EncodeTiled(sierpinski(96), tiledOpts(48, 2))
	if err != nil {
		t.Fatal(err)
	}
	blob, _ := tl.MarshalBinary()
	for n := 0; n < len(blob); n++ {
		var x Tiled
		if err := x.UnmarshalBinary(blob[:n]); err == nil {
			// A prefix can only parse if the cut falls after the last tile.
			t.Fatalf("truncation to %d/%d bytes parsed", n, len(blob))
		}
	}
	var x Tiled
	if err := x.UnmarshalBinary(append(append([]byte(nil), blob...), 1)); err == nil {
		t.Fatal("trailing byte accepted")
	}

	clone := func() *Tiled {
		c := *tl
		c.Tiles = append([]Tile(nil), tl.Tiles...)
		return &c
	}
	bad := map[string]func(*Tiled){
		"bad mode":        func(c *Tiled) { c.Tiles[0].Mode = 9 },
		"stored wrong":    func(c *Tiled) { c.Tiles[0] = Tile{TileStored, []byte{1, 2, 3}} },
		"flate garbage":   func(c *Tiled) { c.Tiles[0] = Tile{TileFlate, []byte{1, 2, 3}} },
		"fractal garbage": func(c *Tiled) { c.Tiles[0] = Tile{TileFractal, []byte{1, 2, 3}} },
		"tile count":      func(c *Tiled) { c.Tiles = c.Tiles[:len(c.Tiles)-1] },
		"zero tile":       func(c *Tiled) { c.Tile = 0 },
		"huge tile":       func(c *Tiled) { c.Tile = 1 << 20 },
	}
	for name, mutate := range bad {
		c := clone()
		mutate(c)
		if _, err := DecodeTiled(c, 2); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	// A fractal tile of the wrong shape must be refused.
	other, _, _, _ := EncodeTiled(sierpinski(64), tiledOpts(64, 1))
	c := clone()
	c.Tiles[0] = other.Tiles[0]
	if _, err := DecodeTiled(c, 1); err == nil {
		t.Error("tile of the wrong dimensions accepted")
	}
}

func TestTiledOptionsValidation(t *testing.T) {
	m := matrix.NewDense(4, 4)
	for name, mod := range map[string]func(*Options){
		"tile too big":     func(o *Options) { o.Tile = maxTile + 1 },
		"negative workers": func(o *Options) { o.Workers = -1 },
		"min > block":      func(o *Options) { o.Block, o.MinBlock = 4, 8 },
		"not power of two": func(o *Options) { o.Block, o.MinBlock = 12, 4 },
		"block over limit": func(o *Options) { o.Block = maxTiledBlock + 1 },
	} {
		o := DefaultOptions()
		mod(&o)
		if _, _, _, err := EncodeTiled(m, o); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func FuzzTiledUnmarshal(f *testing.F) {
	tl, _, _, err := EncodeTiled(sierpinski(64), tiledOpts(32, 1))
	if err != nil {
		f.Fatal(err)
	}
	blob, _ := tl.MarshalBinary()
	f.Add(blob)
	f.Add([]byte{})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0x07, 0xff, 0xff, 0xff, 0xff, 0x07, 0x01})
	f.Fuzz(func(t *testing.T, data []byte) {
		var x Tiled
		if err := x.UnmarshalBinary(data); err != nil {
			return
		}
		if x.Width*x.Height > 1<<20 {
			return
		}
		DecodeTiled(&x, 2) // an error is fine; a panic is not
	})
}

// The shortcut in encodeTile rests on this: no DEFLATE output is smaller than
// 1/1032 of its input, even for the most compressible input there is.
func TestDeflateRatioBound(t *testing.T) {
	for _, n := range []int{1 << 10, 1 << 16, 1 << 20, 1 << 24} {
		for _, v := range []byte{0, 255} {
			out := deflate(bytes.Repeat([]byte{v}, n))
			if len(out)*maxDeflateRatio < n {
				t.Fatalf("DEFLATE compressed %d identical bytes to %d (ratio beyond %d:1)", n, len(out), maxDeflateRatio)
			}
		}
	}
}

func TestReadTileMatchesGenericCopy(t *testing.T) {
	src := matrix.NewDense(50, 40)
	rand.New(rand.NewSource(1)).Read(src.Pix())
	for _, c := range [][4]int{{0, 0, 50, 40}, {7, 3, 20, 30}, {49, 39, 1, 1}} {
		fast, slow := matrix.NewDense(c[2], c[3]), matrix.NewDense(c[2], c[3])
		readTile(fast, src, c[0], c[1])
		readTile(slow, matrix.Func{W: 50, H: 40, F: src.At}, c[0], c[1]) // not a RowAccessor
		if !matrix.Equal(fast, slow) {
			t.Fatalf("tile %v differs between the row and generic paths", c)
		}
	}
}
