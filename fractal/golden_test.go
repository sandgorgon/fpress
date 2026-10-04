package fractal

import (
	"hash/fnv"
	"math/rand"
	"testing"

	"fpress/matrix"
)

// goldenCases are inputs whose encoded bytes are fingerprinted. A change to
// any fingerprint means the encoder's output changed: refactors that promise
// identical output (parallelism, memory management) must leave all of them
// alone; a deliberate format or algorithm change updates them in one commit,
// with the reason.
func goldenCases() []struct {
	name string
	m    matrix.Matrix
	opt  Options
} {
	rng := rand.New(rand.NewSource(21))
	noisy := matrix.NewDense(200, 120)
	copy(noisy.Pix(), sparseStructured(200, 120))
	for i := 0; i < 150; i++ {
		noisy.Set(rng.Intn(200), rng.Intn(120), byte(rng.Intn(256)))
	}
	grad := matrix.NewDense(100, 80)
	for y := 0; y < 80; y++ {
		for x := 0; x < 100; x++ {
			grad.Set(x, y, byte(3*x+y))
		}
	}
	mixed := matrix.NewDense(150, 150)
	for y := 0; y < 150; y++ {
		for x := 0; x < 150; x++ {
			switch {
			case x < 50:
				mixed.Set(x, y, byte(x&y)*5)
			case x < 100:
				mixed.Set(x, y, byte(rng.Intn(256)))
			default:
				mixed.Set(x, y, 0)
			}
		}
	}
	one := DefaultOptions()
	one.NoSameScale = true // the nine cases below predate 2D-copy recipes; with them off they must not change
	one.Workers = 1
	one.Tile = 512 // fixed (the default now depends on the matrix); the tile side is part of the stream
	small := one
	small.Tile = 64
	tiny := one
	tiny.Block, tiny.MinBlock, tiny.Stride = 8, 4, 4
	exh := one
	exh.Exhaustive = true
	exh.Block, exh.MinBlock = 8, 4

	copies := one
	copies.NoSameScale = false
	return []struct {
		name string
		m    matrix.Matrix
		opt  Options
	}{
		{"tilemap-copies", tileMap(256, 8, 5, 3), copies},
		{"gradient-copies", grad, copies},
		{"mixed-copies", mixed, copies},
		{"sierpinski-128", sierpinski(128), one},
		{"sierpinski-256-tiles-of-64", sierpinski(256), small},
		{"sierpinski-300-odd", sierpinski(300), one},
		{"repeated-rows", repeatedRows(100, 60), one},
		{"sparse-noisy", noisy, one},
		{"gradient", grad, one},
		{"mixed-regions", mixed, one},
		{"sierpinski-block8", sierpinski(128), tiny},
		{"exhaustive-mixed", mixed, exh},
	}
}

func fingerprint(t testing.TB, m matrix.Matrix, opt Options) (uint64, int) {
	t.Helper()
	tl, _, _, err := EncodeTiled(m, opt)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := tl.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	h := fnv.New64a()
	h.Write(blob)
	return h.Sum64(), len(blob)
}

func TestEncoderGolden(t *testing.T) {
	for _, c := range goldenCases() {
		sum, n := fingerprint(t, c.m, c.opt)
		t.Logf("golden %-28s len=%-6d fnv=%#x", c.name, n, sum)
		if want, ok := goldenValues[c.name]; !ok || want.sum != sum || want.n != n {
			t.Errorf("%s: encoding changed: len=%d fnv=%#x, want len=%d fnv=%#x", c.name, n, sum, want.n, want.sum)
		}
	}
}
