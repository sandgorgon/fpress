package codec

import (
	"bytes"
	"math/rand"
	"testing"
)

// tileBytes is a tile map as raw bytes: 256x256 cells of 8x8 tiles from a palette of 6.
func tileBytes(size, tile, palette int, seed int64) []byte {
	rng := rand.New(rand.NewSource(seed))
	tiles := make([][]byte, palette)
	for i := range tiles {
		tiles[i] = randomBytes(tile*tile, seed*31+int64(i))
	}
	out := make([]byte, size*size)
	for ty := 0; ty < size/tile; ty++ {
		for tx := 0; tx < size/tile; tx++ {
			p := tiles[rng.Intn(palette)]
			for y := 0; y < tile; y++ {
				copy(out[(ty*tile+y)*size+tx*tile:], p[y*tile:(y+1)*tile])
			}
		}
	}
	return out
}

func ordinaryData() map[string][]byte {
	rng := rand.New(rand.NewSource(4))
	words := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta", "iota", "kappa"}
	var text []byte
	for len(text) < 120000 {
		text = append(text, words[rng.Intn(len(words))]...)
		text = append(text, ' ')
	}
	smooth := make([]byte, 100000)
	for i := range smooth {
		smooth[i] = byte(i/400) + byte(rng.Intn(3))
	}
	return map[string][]byte{"text": text[:120000], "noisy ramp": smooth, "random": randomBytes(100000, 3)}
}

func TestGateSkipsTheFractalStepOnOrdinaryData(t *testing.T) {
	for name, data := range ordinaryData() {
		opt := DefaultOptions()
		opt.NoQuickReject = true // let even the random input reach the fractal decision
		_, rep, err := CompressReport(data, opt)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%-10s gated=%v fractal=%d probe %d/%d (%.2f%%)", name, rep.FractalGated, rep.Fractal, rep.FractalProbe.Hits, rep.FractalProbe.Nodes, 100*rep.FractalProbe.Rate())
		if !rep.FractalGated || rep.Fractal != -1 {
			t.Errorf("%s: the fractal step ran although the data has no self-similarity (gated=%v fractal=%d)", name, rep.FractalGated, rep.Fractal)
		}
	}
}

func TestGateLetsStructuredDataThrough(t *testing.T) {
	for name, data := range map[string][]byte{
		"tile map":   tileBytes(256, 8, 6, 2),
		"sierpinski": sierpinskiBytes(256, 256),
		"gradient":   gradient(256, 200),
	} {
		_, rep, err := CompressReport(data, DefaultOptions())
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%-10s gated=%v fractal=%d probe %d/%d (%.1f%%)", name, rep.FractalGated, rep.Fractal, rep.FractalProbe.Hits, rep.FractalProbe.Nodes, 100*rep.FractalProbe.Rate())
		if rep.FractalGated || rep.Fractal < 0 {
			t.Errorf("%s: the gate blocked the fractal step although the data is self-similar (gated=%v fractal=%d)", name, rep.FractalGated, rep.Fractal)
		}
	}
}

// The gate is a speed measure only: it must not change what comes out.
func TestGateNeverChangesTheOutputSize(t *testing.T) {
	inputs := ordinaryData()
	inputs["tile map"] = tileBytes(256, 8, 6, 2)
	inputs["sierpinski"] = sierpinskiBytes(256, 256)
	inputs["gradient"] = gradient(256, 200)
	inputs["mixed"] = streamData(150000)
	for name, data := range inputs {
		on, off := DefaultOptions(), DefaultOptions()
		off.NoFractalGate = true
		a, _, err := CompressReport(data, on)
		if err != nil {
			t.Fatal(err)
		}
		b, _, err := CompressReport(data, off)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Errorf("%s: the gate changed the output (%d bytes with it, %d without)", name, len(a), len(b))
		}
	}
}

func TestGateIgnoresSmallBlocks(t *testing.T) {
	data := randomBytes(gateMinSize-1, 5)
	opt := DefaultOptions()
	opt.NoQuickReject = true
	_, rep, err := CompressReport(data, opt)
	if err != nil {
		t.Fatal(err)
	}
	if rep.FractalGated || rep.Fractal < 0 {
		t.Errorf("a block under the gate's minimum size should just run the fractal step (gated=%v fractal=%d)", rep.FractalGated, rep.Fractal)
	}
}
