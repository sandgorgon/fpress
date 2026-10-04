package fractal

import (
	"bytes"
	"compress/flate"
	"math/rand"
	"os"
	"testing"

	"fpress/matrix"
)

// ---- test inputs -----------------------------------------------------------

func randomBytes(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func constant(n int, v byte) []byte {
	return bytes.Repeat([]byte{v}, n)
}

// sierpinski is exactly self-similar: 255 where x&y == 0.
func sierpinski(n int) matrix.Matrix {
	return matrix.Func{W: n, H: n, F: func(x, y int) byte {
		if x&y == 0 {
			return 255
		}
		return 0
	}}
}

// repeatedRows: every row is the same short pattern, shifted values every 8 rows.
func repeatedRows(w, h int) matrix.Matrix {
	return matrix.Func{W: w, H: h, F: func(x, y int) byte {
		return byte((x*7)%23) + byte(y/8)
	}}
}

// smoothField: slowly varying values, as from a sensor grid.
func smoothField(w, h int) matrix.Matrix {
	return matrix.Func{W: w, H: h, F: func(x, y int) byte { return byte((x + y) / 4) }}
}

func flateSize(b []byte) int {
	var buf bytes.Buffer
	fw, _ := flate.NewWriter(&buf, flate.BestCompression)
	fw.Write(b)
	fw.Close()
	return buf.Len()
}

// ---- helpers ---------------------------------------------------------------

func roundTrip(t *testing.T, name string, m matrix.Matrix, opt Options) Stats {
	t.Helper()
	c, st, err := Encode(m, opt)
	if err != nil {
		t.Fatalf("%s: Encode: %v", name, err)
	}
	got, err := Decode(c)
	if err != nil {
		t.Fatalf("%s: Decode: %v", name, err)
	}
	if !matrix.Equal(got, m) {
		t.Fatalf("%s: decoded matrix differs from the original", name)
	}
	return st
}

func report(t *testing.T, name string, m matrix.Matrix, opt Options) {
	t.Helper()
	c, st, err := Encode(m, opt)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	raw := matrix.Bytes(m)
	t.Logf("%-24s cells=%6d exact=%5.1f%% recipes=%4d anchors=%4d demoted=%3d rounds=%d | approx=%6dB flate=%6dB raw=%6dB",
		name, st.Cells, 100*float64(st.ExactCells)/float64(max(st.Cells, 1)),
		st.Recipes, st.Anchors, st.Demoted, st.RefineRounds,
		c.ApproxSize(), flateSize(raw), len(raw))
}

// ---- correctness: exact reconstruction -------------------------------------

func TestRoundTripExact(t *testing.T) {
	cases := []struct {
		name string
		m    matrix.Matrix
	}{
		{"empty", matrix.FromBytes(nil, 8)},
		{"zero-width", matrix.NewDense(0, 5)},
		{"one-byte", matrix.FromBytes([]byte{42}, 1)},
		{"zeros", matrix.FromBytes(constant(1024, 0), 32)},
		{"all-0xFF", matrix.FromBytes(constant(1024, 0xFF), 32)},
		{"random-32x32", matrix.FromBytes(randomBytes(1024, 1), 32)},
		{"random-not-multiple", matrix.FromBytes(randomBytes(1000, 2), 37)},
		{"tiny-3x3", matrix.FromBytes(randomBytes(9, 3), 3)},
		{"narrow-1xN", matrix.FromBytes(randomBytes(100, 4), 1)},
		{"wide-Nx1", matrix.FromBytes(randomBytes(100, 5), 100)},
		{"sierpinski-64", sierpinski(64)},
		{"sierpinski-50-odd", sierpinski(50)},
		{"repeated-rows", repeatedRows(64, 48)},
		{"smooth-field", smoothField(60, 45)},
	}
	for _, opt := range []Options{
		DefaultOptions(),
		{Block: 4, Iterations: 6, Stride: 2, RecipeCost: 2, MaxRefine: 3, AnchorPercent: 25},
		{Block: 1, Iterations: 3, Stride: 1, RecipeCost: 1, MaxRefine: 2, AnchorPercent: 100},
		{Block: 16, Iterations: 20, Stride: 4, RecipeCost: 8, MaxRefine: 0, AnchorPercent: 0},
		{Block: 32, MinBlock: 2, Iterations: 12, Stride: 4, RecipeCost: 3, MaxRefine: 8, AnchorPercent: 40},
		{Block: 8, MinBlock: 4, Iterations: 8, Stride: 2, RecipeCost: 2, MaxRefine: 8, AnchorPercent: 25, Exhaustive: true},
	} {
		for _, tc := range cases {
			roundTrip(t, tc.name, tc.m, opt)
		}
	}
}

func TestRoundTripRandomShapes(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	for i := 0; i < 40; i++ {
		w, h := 1+rng.Intn(40), 1+rng.Intn(40)
		data := randomBytes(w*h, int64(i))
		if i%2 == 0 { // make half of them low-entropy
			for j := range data {
				data[j] %= 4
			}
		}
		opt := DefaultOptions()
		opt.Block = 1 + rng.Intn(16)
		opt.MinBlock = 0
		if opt.Block%2 == 0 {
			opt.MinBlock = opt.Block / 2
		}
		opt.Stride = 1 + rng.Intn(6)
		opt.Exhaustive = i%5 == 0
		roundTrip(t, "random-shape", matrix.FromBytes(data, w), opt)
	}
}

func FuzzRoundTrip(f *testing.F) {
	f.Add([]byte("hello hello hello hello"), uint8(5), uint8(4))
	f.Add(constant(64, 7), uint8(8), uint8(2))
	f.Add([]byte{}, uint8(1), uint8(1))
	f.Fuzz(func(t *testing.T, data []byte, width, block uint8) {
		if len(data) > 4096 || width == 0 || block == 0 || block > 16 {
			t.Skip()
		}
		opt := DefaultOptions()
		opt.Block = int(block)
		opt.MinBlock = 0
		opt.Stride = int(block)
		c, _, err := Encode(matrix.FromBytes(data, int(width)), opt)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(c)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(matrix.Bytes(got)[:len(data)], data) {
			t.Fatal("round trip mismatch")
		}
	})
}

// ---- the interface requirement ---------------------------------------------

// Any Matrix implementation with the same contents must encode identically.
func TestEncodeIsIndependentOfMatrixImplementation(t *testing.T) {
	data := randomBytes(40*30, 7)
	for i := range data {
		data[i] %= 5
	}
	dense := matrix.FromBytes(data, 40)
	fn := matrix.Func{W: 40, H: 30, F: func(x, y int) byte { return data[y*40+x] }}
	fm, err := matrix.NewFileMatrix(bytes.NewReader(data), int64(len(data)), 40)
	if err != nil {
		t.Fatal(err)
	}
	// A view onto a larger matrix, offset so coordinates must be translated.
	big := matrix.NewDense(60, 50)
	matrix.Copy(matrix.NewMutableView(big, 7, 9, 40, 30), dense)
	view := matrix.NewView(big, 7, 9, 40, 30)

	ref, _, err := Encode(dense, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	for name, m := range map[string]matrix.Matrix{"func": fn, "file": fm, "view": view} {
		got, _, err := Encode(m, DefaultOptions())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.Start != ref.Start || !bytes.Equal(got.Residual, ref.Residual) || len(got.Transforms) != len(ref.Transforms) {
			t.Errorf("%s: output differs from the Dense encoding", name)
		}
	}
}

// capture is a MutableMatrix that is not Dense, to prove DecodeInto only needs the interface.
type capture struct {
	w, h int
	m    map[[2]int]byte
}

func (c *capture) Width() int           { return c.w }
func (c *capture) Height() int          { return c.h }
func (c *capture) At(x, y int) byte     { return c.m[[2]int{x, y}] }
func (c *capture) Set(x, y int, v byte) { c.m[[2]int{x, y}] = v }

func TestDecodeIntoAnyMutableMatrix(t *testing.T) {
	src := repeatedRows(30, 20)
	c, _, err := Encode(src, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	dst := &capture{w: 30, h: 20, m: map[[2]int]byte{}}
	if err := DecodeInto(dst, c); err != nil {
		t.Fatal(err)
	}
	if !matrix.Equal(dst, src) {
		t.Fatal("DecodeInto produced the wrong contents")
	}
	if err := DecodeInto(&capture{w: 29, h: 20, m: map[[2]int]byte{}}, c); err == nil {
		t.Fatal("expected a dimension mismatch error")
	}
}

// ---- determinism and robustness --------------------------------------------

func TestEncodeIsDeterministic(t *testing.T) {
	m := repeatedRows(48, 48)
	a, _, _ := Encode(m, DefaultOptions())
	b, _, _ := Encode(m, DefaultOptions())
	if a.Start != b.Start || !bytes.Equal(a.Residual, b.Residual) || len(a.Transforms) != len(b.Transforms) {
		t.Fatal("two encodes of the same input differ")
	}
	for i := range a.Transforms {
		x, y := a.Transforms[i], b.Transforms[i]
		if x.RX != y.RX || x.RY != y.RY || x.DX != y.DX || x.DY != y.DY || x.Iso != y.Iso || x.Map != y.Map || x.Raw != y.Raw {
			t.Fatalf("transform %d differs", i)
		}
	}
}

func TestDecodeRejectsMalformedStreams(t *testing.T) {
	good, _, err := Encode(sierpinski(64), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(good.Transforms) == 0 {
		t.Fatal("test needs at least one transform")
	}
	clone := func() *Compressed {
		c := *good
		c.Transforms = append([]Transform(nil), good.Transforms...)
		c.Residual = append([]byte(nil), good.Residual...)
		return &c
	}
	firstRecipe := func(c *Compressed) *Transform {
		for i := range c.Transforms {
			if !c.Transforms[i].Raw {
				return &c.Transforms[i]
			}
		}
		t.Fatal("no recipe transform")
		return nil
	}
	cases := map[string]func(*Compressed){
		"negative width":     func(c *Compressed) { c.Width = -1 },
		"zero block":         func(c *Compressed) { c.Block = 0 },
		"zero iterations":    func(c *Compressed) { c.Iterations = 0 },
		"range out of grid":  func(c *Compressed) { c.Transforms[0].RX = 1000 },
		"negative range":     func(c *Compressed) { c.Transforms[0].RY = -1 },
		"zero size":          func(c *Compressed) { c.Transforms[0].Size = 0 },
		"huge size":          func(c *Compressed) { c.Transforms[0].Size = 1 << 30 },
		"domain out of grid": func(c *Compressed) { firstRecipe(c).DX = 1000 },
		"bad isometry":       func(c *Compressed) { firstRecipe(c).Iso = 9 },
		"bad value map":      func(c *Compressed) { firstRecipe(c).Map.Kind = 7 },
		"truncated residual": func(c *Compressed) { c.Residual = c.Residual[:len(c.Residual)/2] },
		"empty residual":     func(c *Compressed) { c.Residual = nil },
		"garbage residual":   func(c *Compressed) { c.Residual = []byte{0xde, 0xad, 0xbe, 0xef} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := clone()
			mutate(c)
			if _, err := Decode(c); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestOptionsValidation(t *testing.T) {
	m := matrix.FromBytes([]byte{1, 2, 3, 4}, 2)
	for name, opt := range map[string]Options{
		"block":      {Block: 0, Iterations: 1, Stride: 1},
		"iterations": {Block: 1, Iterations: 0, Stride: 1},
		"stride":     {Block: 1, Iterations: 1, Stride: 0},
		"negative":   {Block: 1, Iterations: 1, Stride: 1, RecipeCost: -1},
	} {
		if _, _, err := Encode(m, opt); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// ---- the milestone-3 experiment: how well does the iteration behave? -------

// TestConvergenceExperiment does not assert a compression target; it prints
// how much of each input the iterated recipes reproduce on their own, and the
// resulting size against plain flate. Run with -v to read it.
func TestConvergenceExperiment(t *testing.T) {
	opt := DefaultOptions()
	noAnchors := opt
	noAnchors.AnchorPercent = 100

	inputs := []struct {
		name string
		m    matrix.Matrix
	}{
		{"sierpinski-64", sierpinski(64)},
		{"sierpinski-128", sierpinski(128)},
		{"repeated-rows-64x64", repeatedRows(64, 64)},
		{"smooth-field-64x64", smoothField(64, 64)},
		{"random-64x64", matrix.FromBytes(randomBytes(4096, 11), 64)},
	}
	if b, err := os.ReadFile("/bin/ls"); err == nil && len(b) >= 16384 {
		inputs = append(inputs, struct {
			name string
			m    matrix.Matrix
		}{"/bin/ls[16K]@128", matrix.FromBytes(b[:16384], 128)})
	}
	for _, in := range inputs {
		report(t, in.name+" (anchors)", in.m, opt)
		report(t, in.name+" (no anchors)", in.m, noAnchors)
	}
}

func TestSierpinskiBenefitsFromAnchors(t *testing.T) {
	withA := roundTrip(t, "sierpinski", sierpinski(64), DefaultOptions())
	noA := DefaultOptions()
	noA.AnchorPercent = 100
	withoutA := roundTrip(t, "sierpinski", sierpinski(64), noA)
	t.Logf("with anchors: exact=%d/%d recipes=%d anchors=%d | without: exact=%d/%d recipes=%d",
		withA.ExactCells, withA.Cells, withA.Recipes, withA.Anchors,
		withoutA.ExactCells, withoutA.Cells, withoutA.Recipes)
	if withA.ExactCells <= withoutA.ExactCells {
		t.Errorf("anchors should let the iteration reproduce more cells on exactly self-similar data")
	}
}

// Skipping the inner self-check must not change a single output byte.
func TestNoSelfCheckGivesIdenticalBytes(t *testing.T) {
	for name, m := range map[string]matrix.Matrix{
		"sierpinski":    sierpinski(200),
		"repeated-rows": repeatedRows(120, 90),
		"noise":         matrix.FromBytes(randomBytes(5000, 3), 50),
	} {
		a, b := DefaultOptions(), DefaultOptions()
		b.NoSelfCheck = true
		ta, _, _, err := EncodeTiled(m, a)
		if err != nil {
			t.Fatal(err)
		}
		tb, _, _, err := EncodeTiled(m, b)
		if err != nil {
			t.Fatal(err)
		}
		ba, _ := ta.MarshalBinary()
		bb, _ := tb.MarshalBinary()
		if !bytes.Equal(ba, bb) {
			t.Errorf("%s: NoSelfCheck changed the output", name)
		}
	}
}
