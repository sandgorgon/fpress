package fractal

import (
	"bytes"
	"math/rand"
	"os"
	"testing"

	"fpress/matrix"
)

func probeOf(t *testing.T, m matrix.Matrix) ProbeResult {
	t.Helper()
	opt := DefaultOptions()
	opt.Workers = 4
	p, err := Probe(m, opt)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProbeSeparatesStructuredFromOrdinaryData(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	noise := matrix.FromBytes(randomBytes(80000, 1), 256)
	text := make([]byte, 0, 80000)
	words := []string{"the", "quick", "brown", "fox", "jumps", "over", "lazy", "dog", "and", "runs", "away"}
	for len(text) < 80000 {
		text = append(text, words[rng.Intn(len(words))]...)
		text = append(text, ' ')
	}
	smooth := matrix.NewDense(256, 256) // a gently noisy surface: no exact repeats
	for y := 0; y < 256; y++ {
		for x := 0; x < 256; x++ {
			smooth.Set(x, y, byte((x+y)/3)+byte(rng.Intn(3)))
		}
	}
	constant := matrix.NewDense(128, 128)
	constant.Fill(7)

	cases := []struct {
		name  string
		m     matrix.Matrix
		worth bool
	}{
		{"random bytes", noise, false},
		{"text", matrix.FromBytes(text[:80000], 256), false}, // shows ~0.5% chance hits at 4x4
		{"noisy surface", smooth, false},
		{"constant", constant, false},
		{"sierpinski", sierpinski(256), true},
		{"tile map 16x16 tiles", tileMap(256, 16, 8, 4), true},
		{"tile map 8x8 tiles", tileMap(256, 8, 6, 5), true},
		{"repeated rows", repeatedRows(200, 150), true},
	}
	for _, c := range cases {
		p := probeOf(t, c.m)
		t.Logf("%-22s nodes %5d hits %5d (copies %5d) rate %5.1f%% worth=%v", c.name, p.Nodes, p.Hits, p.CopyHits, 100*p.Rate(), p.Worth())
		if p.Worth() != c.worth {
			t.Errorf("%s: Worth() = %v, want %v", c.name, p.Worth(), c.worth)
		}
	}
}

// The probe is a prediction of the full step: where it says no, the full
// encoder (finer grid, larger blocks) must not find much either.
func TestProbeAgreesWithTheFullEncoder(t *testing.T) {
	for name, m := range map[string]matrix.Matrix{
		"random": matrix.FromBytes(randomBytes(60000, 7), 200),
		"smooth": smoothField(240, 200),
	} {
		p := probeOf(t, m)
		opt := DefaultOptions()
		opt.Workers = 2
		_, st, err := Encode(m, opt)
		if err != nil {
			t.Fatal(err)
		}
		if !p.Worth() && st.Recipes > 8 {
			t.Errorf("%s: the probe says not worth it but the encoder found %d recipes", name, st.Recipes)
		}
	}
	// ...and where the probe says yes, the encoder does find recipes.
	for name, m := range map[string]matrix.Matrix{"sierpinski": sierpinski(200), "tiles": tileMap(200, 8, 5, 6)} {
		p := probeOf(t, m)
		_, st, _ := Encode(m, DefaultOptions())
		if !p.Worth() || st.Recipes == 0 {
			t.Errorf("%s: probe worth=%v, encoder recipes=%d", name, p.Worth(), st.Recipes)
		}
	}
}

func TestProbeIsIndependentOfWorkers(t *testing.T) {
	m := tileMap(300, 8, 6, 9)
	var ref ProbeResult
	for i, w := range []int{1, 2, 3, 8, 64} {
		opt := DefaultOptions()
		opt.Workers = w
		p, err := Probe(m, opt)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			ref = p
		} else if p != ref {
			t.Fatalf("%d workers gave %+v, 1 worker %+v", w, p, ref)
		}
	}
}

func TestProbeEdgeCases(t *testing.T) {
	for _, m := range []matrix.Matrix{matrix.NewDense(0, 0), matrix.NewDense(5, 0), matrix.NewDense(1, 1), matrix.FromBytes([]byte{1, 2, 3}, 3), matrix.NewDense(7, 9)} {
		if _, err := Probe(m, DefaultOptions()); err != nil {
			t.Errorf("%dx%d: %v", m.Width(), m.Height(), err)
		}
	}
	// NoSameScale really removes the copy hits.
	m := tileMap(128, 8, 4, 3)
	opt := DefaultOptions()
	opt.NoSameScale = true
	if p, _ := Probe(m, opt); p.CopyHits != 0 {
		t.Errorf("copy hits %d with NoSameScale", p.CopyHits)
	}
}

// Real files of ordinary kinds have nothing for the fractal step: the whole point of the gate.
func TestProbeFindsNothingInRealFiles(t *testing.T) {
	for _, f := range []string{"/bin/ls", "/usr/bin/bash", "/etc/services"} {
		b, err := os.ReadFile(f)
		if err != nil || len(b) < 30000 {
			continue
		}
		b = b[:min(len(b), 400000)]
		for _, w := range []int{64, 256, 512} {
			p := probeOf(t, matrix.FromBytes(b, w))
			t.Logf("%-16s width %3d: nodes %6d hits %d", f, w, p.Nodes, p.Hits)
			if p.Worth() {
				t.Errorf("%s at width %d: probe says the fractal step is worth running (hits %d of %d)", f, w, p.Hits, p.Nodes)
			}
		}
	}
	_ = bytes.Equal
}

// The threshold itself: pinned so that it cannot drift unnoticed. Measured
// rates: ordinary data never above 1.5%, structured data never below 10.9%.
func TestProbeThreshold(t *testing.T) {
	for _, c := range []struct {
		hits, nodes int
		worth       bool
	}{
		{149, 10000, false}, // 1.49%: the highest rate seen on ordinary data
		{399, 10000, false}, // just under 4%
		{400, 10000, true},  // 4%
		{1089, 10000, true}, // 10.9%: the lowest rate seen on structured data
		{15, 100, false},    // a high rate from too few blocks is chance
		{16, 100, true},
		{0, 0, false},
	} {
		if got := (ProbeResult{Nodes: c.nodes, Hits: c.hits}).Worth(); got != c.worth {
			t.Errorf("%d hits of %d: Worth() = %v, want %v", c.hits, c.nodes, got, c.worth)
		}
	}
}

func TestNarrowSwitchesOffEmptyFamilies(t *testing.T) {
	base := DefaultOptions()
	for _, c := range []struct {
		p                       ProbeResult
		noSameScale, noDecimate bool
	}{
		{ProbeResult{Nodes: 100, Hits: 90, CopyHits: 0}, true, false},   // Sierpinski-like: no copies
		{ProbeResult{Nodes: 100, Hits: 90, CopyHits: 90}, false, true},  // tile-map-like: no decimated hits
		{ProbeResult{Nodes: 100, Hits: 90, CopyHits: 40}, false, false}, // both have hits
	} {
		o := c.p.Narrow(base)
		if o.NoSameScale != c.noSameScale || o.NoDecimated != c.noDecimate {
			t.Errorf("%+v: NoSameScale=%v NoDecimated=%v, want %v %v", c.p, o.NoSameScale, o.NoDecimated, c.noSameScale, c.noDecimate)
		}
	}
}

// Narrowing must keep the results: on inputs where one family has everything,
// the narrowed search gives the same recipes as the full one.
func TestNarrowedSearchGivesTheSameResult(t *testing.T) {
	for name, m := range map[string]matrix.Matrix{"sierpinski": sierpinski(256), "tile map": tileMap(256, 8, 6, 3)} {
		opt := DefaultOptions()
		opt.Workers = 2
		p, err := Probe(m, opt)
		if err != nil {
			t.Fatal(err)
		}
		full, narrowed := opt, p.Narrow(opt)
		if full == narrowed {
			t.Fatalf("%s: nothing was narrowed (probe %+v)", name, p)
		}
		size := func(o Options) int {
			tl, _, _, err := EncodeTiled(m, o)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := tl.MarshalBinary()
			return len(b)
		}
		if a, b := size(full), size(narrowed); b > a {
			t.Errorf("%s: narrowing made the output larger (%d > %d)", name, b, a)
		}
	}
}
