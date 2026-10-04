package fractal

import (
	"bytes"
	"math/rand"
	"testing"

	"fpress/matrix"
)

// sparseStructured is a residual that is mostly zero with a regular lattice of
// values that repeat down the rows: cheap for a coder that sees the row above.
func sparseStructured(w, h int) []byte {
	res := make([]byte, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x += 7 {
			res[y*w+x] = byte(17 + x/7)
		}
	}
	return res
}

func nonZero(b []byte) int {
	n := 0
	for _, v := range b {
		if v != 0 {
			n++
		}
	}
	return n
}

func TestPackResidualPicksTheSmallestMode(t *testing.T) {
	const w, h = 70, 60
	zero := make([]byte, w*h)
	if got := packResidual(zero, w, 0, 0); !bytes.Equal(got, []byte{residualZero}) {
		t.Errorf("all-zero residual packed as %v", got)
	}

	sparse := sparseStructured(w, h)
	got := packResidual(sparse, w, nonZero(sparse), 0)
	flateOnly := 1 + len(deflate(sparse))
	t.Logf("sparse structured: mode %d, %d bytes (flate alone %d)", got[0], len(got), flateOnly)
	if got[0] != residualCM || len(got) >= flateOnly {
		t.Errorf("sparse structured residual should use cm and beat flate: mode %d, %d vs %d", got[0], len(got), flateOnly)
	}

	dense := make([]byte, w*h)
	rand.New(rand.NewSource(1)).Read(dense)
	if got := packResidual(dense, w, nonZero(dense), 0); got[0] != residualFlate {
		t.Errorf("dense residual should skip the slow coder, got mode %d", got[0])
	}

	// Whatever was chosen, it decodes to the original.
	for name, res := range map[string][]byte{"zero": zero, "sparse": sparse, "dense": dense} {
		c := &Compressed{Width: w, Height: h, Residual: packResidual(res, w, nonZero(res), 0)}
		back, err := decodeResidual(c, 0)
		if err != nil || !bytes.Equal(back, res) {
			t.Errorf("%s: residual round trip failed: %v", name, err)
		}
	}
}

func TestDecodeResidualRejectsMalformed(t *testing.T) {
	const w, h = 70, 60
	sparse := sparseStructured(w, h)
	cm := packResidual(sparse, w, nonZero(sparse), 0)
	fl := append([]byte{residualFlate}, deflate(sparse)...)
	if cm[0] != residualCM {
		t.Fatal("test needs a cm residual")
	}
	bad := map[string][]byte{
		"missing":          nil,
		"unknown mode":     {9, 1, 2, 3},
		"zero with data":   {residualZero, 0},
		"cm truncated":     cm[:len(cm)/2],
		"cm trailing":      append(append([]byte(nil), cm...), 0),
		"cm empty body":    {residualCM},
		"flate truncated":  fl[:len(fl)/2],
		"flate trailing":   append(append([]byte(nil), fl...), 0),
		"flate wrong size": append([]byte{residualFlate}, deflate(sparse[:100])...),
	}
	for name, r := range bad {
		if _, err := decodeResidual(&Compressed{Width: w, Height: h, Residual: r}, 0); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if res, err := decodeResidual(&Compressed{Width: 0, Height: 5}, 0); err != nil || res != nil {
		t.Errorf("empty matrix: %v %v", res, err)
	}
}

// When the recipes reproduce everything, the residual costs one byte, not the
// ~270 bytes DEFLATE needs to say "262144 zeros".
func TestExactReconstructionHasAOneByteResidual(t *testing.T) {
	c, st, err := Encode(sierpinski(256), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if st.ResidualNonZero != 0 {
		t.Skip("recipes did not reproduce the matrix exactly")
	}
	if !bytes.Equal(c.Residual, []byte{residualZero}) {
		t.Errorf("residual = %d bytes, want the 1-byte zero marker", len(c.Residual))
	}
	if st.ResidualPacked != 1 {
		t.Errorf("ResidualPacked = %d, want 1", st.ResidualPacked)
	}
}

// End to end: a matrix whose recipes are imperfect still round trips with the
// cm residual in play.
func TestImperfectReconstructionRoundTrips(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	m := matrix.NewDense(200, 120)
	copy(m.Pix(), sparseStructured(200, 120))
	for i := 0; i < 150; i++ { // some noise on top
		m.Set(rng.Intn(200), rng.Intn(120), byte(rng.Intn(256)))
	}
	c, st, err := Encode(m, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("residual mode %d, %d packed bytes, %d nonzero cells", c.Residual[0], st.ResidualPacked, st.ResidualNonZero)
	got, err := Decode(c)
	if err != nil || !matrix.Equal(got, m) {
		t.Fatalf("round trip failed: %v", err)
	}
}
