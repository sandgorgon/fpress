package prep

import (
	"bytes"
	"math/rand"
	"testing"

	"fpress/matrix"
)

func randomMatrix(w, h int, seed int64) *matrix.Dense {
	d := matrix.NewDense(w, h)
	rand.New(rand.NewSource(seed)).Read(d.Pix())
	return d
}

// every chain worth testing: the whole catalogue plus some extra combinations.
func allChains() []Chain {
	extra := []Chain{
		{{Kind: DeltaUpSub}, {Kind: DeltaUpSub}},
		{{Kind: BitPlanes}, {Kind: Deinterleave, K: 2}},
		{{Kind: Deinterleave, K: 16}},
		{{Kind: Deinterleave, K: 3}, {Kind: DeltaLeftXor}, {Kind: DeltaUpXor}},
		{{Kind: DeltaLeftSub}, {Kind: BitPlanes}, {Kind: DeltaUpSub}, {Kind: DeltaLeftXor}},
		{{Kind: DeltaFrameSub, K: 1}},
		{{Kind: DeltaFrameSub, K: 3}},
		{{Kind: DeltaFrameXor, K: 4}},
		{{Kind: DeltaFrameSub, K: 100}}, // taller than most test shapes: nothing to difference
		{{Kind: DeltaFrameSub, K: 2}, {Kind: DeltaLeftSub}},
		{{Kind: DeltaFrameXor, K: 65535}},
	}
	return append(append([]Chain(nil), Catalogue...), extra...)
}

func TestEveryChainInvertsExactly(t *testing.T) {
	shapes := [][2]int{{1, 1}, {8, 1}, {8, 8}, {16, 5}, {48, 7}, {24, 3}, {96, 13}, {64, 64}, {5, 9}, {1, 30}}
	applied := 0
	for _, chain := range allChains() {
		for _, sh := range shapes {
			m := randomMatrix(sh[0], sh[1], int64(sh[0]*31+sh[1]))
			orig := append([]byte(nil), m.Pix()...)
			pre, err := chain.Forward(m)
			if err != nil {
				continue // chain does not fit this shape
			}
			applied++
			if pre.Width()*pre.Height() != m.Width()*m.Height() {
				t.Fatalf("%v on %v changed the cell count", chain, sh)
			}
			back, err := chain.Inverse(pre, sh[0], sh[1])
			if err != nil {
				t.Fatalf("%v on %v: Inverse: %v", chain, sh, err)
			}
			if !bytes.Equal(back.Pix(), orig) || back.Width() != sh[0] || back.Height() != sh[1] {
				t.Fatalf("%v on %v: round trip mismatch", chain, sh)
			}
			if !bytes.Equal(m.Pix(), orig) {
				t.Fatalf("%v on %v: Forward modified its input", chain, sh)
			}
		}
	}
	if applied < 100 {
		t.Fatalf("only %d chain/shape combinations applied; the test is too weak", applied)
	}
}

func TestInverseWorksOnAnyMatrixImplementation(t *testing.T) {
	m := randomMatrix(32, 6, 4)
	chain := Chain{{Kind: BitPlanes}, {Kind: DeltaUpXor}}
	pre, _ := chain.Forward(m)
	// Wrap the prepared matrix so it is not a *Dense.
	wrapped := matrix.Func{W: pre.Width(), H: pre.Height(), F: pre.At}
	back, err := chain.Inverse(wrapped, 32, 6)
	if err != nil || !bytes.Equal(back.Pix(), m.Pix()) {
		t.Fatalf("Inverse on a Func matrix failed: %v", err)
	}
}

// The example from the design discussion: rows that change slowly become zeros.
func TestDeltaUpExample(t *testing.T) {
	m := matrix.FromBytes([]byte{
		50, 60, 70, 80,
		50, 60, 71, 80,
		50, 61, 71, 80,
	}, 4)
	pre, _ := Chain{{Kind: DeltaUpSub}}.Forward(m)
	want := []byte{
		50, 60, 70, 80,
		0, 0, 1, 0,
		0, 1, 0, 0,
	}
	if !bytes.Equal(pre.Pix(), want) {
		t.Fatalf("got %v want %v", pre.Pix(), want)
	}
}

func TestDeltaLeftWrapsModulo256(t *testing.T) {
	m := matrix.FromBytes([]byte{250, 3, 10}, 3)
	pre, _ := Chain{{Kind: DeltaLeftSub}}.Forward(m)
	if want := []byte{250, 9, 7}; !bytes.Equal(pre.Pix(), want) {
		t.Fatalf("got %v want %v", pre.Pix(), want)
	}
}

func TestBitPlanesLayout(t *testing.T) {
	// One row of 8 cells, 0x01 in every cell: plane 0 is all ones (0xFF), the rest zero.
	m := matrix.FromBytes(bytes.Repeat([]byte{1}, 8), 8)
	pre, err := Chain{{Kind: BitPlanes}}.Forward(m)
	if err != nil {
		t.Fatal(err)
	}
	if pre.Width() != 1 || pre.Height() != 8 {
		t.Fatalf("dims %dx%d", pre.Width(), pre.Height())
	}
	if want := []byte{0xFF, 0, 0, 0, 0, 0, 0, 0}; !bytes.Equal(pre.Pix(), want) {
		t.Fatalf("got %v want %v", pre.Pix(), want)
	}
}

func TestDeinterleaveSeparatesHighAndLowBytes(t *testing.T) {
	// 16-bit little-endian samples: low byte varies, high byte constant.
	m := matrix.FromBytes([]byte{1, 9, 2, 9, 3, 9, 4, 9}, 8)
	pre, err := Chain{{Kind: Deinterleave, K: 2}}.Forward(m)
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte{1, 2, 3, 4, 9, 9, 9, 9}; !bytes.Equal(pre.Pix(), want) || pre.Width() != 4 || pre.Height() != 2 {
		t.Fatalf("got %v (%dx%d)", pre.Pix(), pre.Width(), pre.Height())
	}
}

func TestStepsRejectBadShapes(t *testing.T) {
	m := randomMatrix(10, 4, 1)
	for _, c := range []Chain{
		{{Kind: BitPlanes}},           // 10 % 8 != 0
		{{Kind: Deinterleave, K: 4}},  // 10 % 4 != 0
		{{Kind: Deinterleave, K: 1}},  // stride too small
		{{Kind: Deinterleave, K: 17}}, // stride too large
		{{Kind: Kind(99)}},            // unknown
		{{Kind: DeltaUpSub, K: 3}},    // stray parameter
		make(Chain, MaxSteps+1),       // too long
	} {
		if _, err := c.Forward(m); err == nil {
			t.Errorf("%v: expected an error", c)
		}
	}
	if _, err := (Chain{}).Inverse(randomMatrix(3, 3, 1), 4, 4); err == nil {
		t.Error("Inverse accepted a matrix of the wrong shape")
	}
}

func TestChainBinaryRoundTrip(t *testing.T) {
	for _, c := range allChains() {
		b := c.AppendBinary([]byte{0xAA})[1:] // leading byte proves append semantics
		got, n, err := ParseChain(append(b, 0xEE))
		if err != nil || n != len(b) || !got.Equal(c) {
			t.Errorf("%v: parsed %v, consumed %d of %d, err %v", c, got, n, len(b), err)
		}
	}
}

func TestParseChainRejectsGarbage(t *testing.T) {
	for name, b := range map[string][]byte{
		"empty":        {},
		"too many":     {MaxSteps + 1},
		"truncated":    {2, byte(DeltaUpSub)},
		"unknown kind": {1, 77},
		"missing K":    {1, byte(Deinterleave)},
		"bad K":        {1, byte(Deinterleave), 99},
		"frame no K":   {1, byte(DeltaFrameSub)},
		"frame half K": {1, byte(DeltaFrameXor), 5},
		"frame K zero": {1, byte(DeltaFrameSub), 0, 0},
	} {
		if _, _, err := ParseChain(b); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func FuzzChainRoundTrip(f *testing.F) {
	f.Add([]byte("abcdefgh12345678abcdefgh12345678"), uint8(8), uint8(3))
	f.Fuzz(func(t *testing.T, data []byte, width, pick uint8) {
		if len(data) == 0 || len(data) > 4096 || width == 0 {
			t.Skip()
		}
		chains := allChains()
		chain := chains[int(pick)%len(chains)]
		m := matrix.FromBytes(data, int(width))
		pre, err := chain.Forward(m)
		if err != nil {
			return
		}
		back, err := chain.Inverse(pre, m.Width(), m.Height())
		if err != nil || !bytes.Equal(back.Pix(), m.Pix()) {
			t.Fatalf("%v: round trip failed (%v)", chain, err)
		}
	})
}

// ---- width detection and search -------------------------------------------

// records: n fixed-size records sharing a template, with a counter field.
func records(size, n int) []byte {
	rng := rand.New(rand.NewSource(3))
	template := make([]byte, size)
	rng.Read(template)
	var out []byte
	for i := 0; i < n; i++ {
		rec := append([]byte(nil), template...)
		rec[0] = byte(i)
		rec[size-1] = byte(i * 7)
		out = append(out, rec...)
	}
	return out
}

func TestDetectWidthsFindsRecordSize(t *testing.T) {
	for _, size := range []int{48, 100, 37, 256} {
		got := DetectWidths(records(size, 60), 3)
		found := false
		for _, w := range got {
			if w == size {
				found = true
			}
			if w != size && w%size == 0 {
				t.Errorf("size %d: multiple %d reported alongside the period", size, w)
			}
		}
		if !found {
			t.Errorf("record size %d not found, got %v", size, got)
		}
	}
}

func TestDetectWidthsIgnoresNoise(t *testing.T) {
	random := make([]byte, 20000)
	rand.New(rand.NewSource(5)).Read(random)
	if got := DetectWidths(random, 3); len(got) != 0 {
		t.Errorf("random data produced widths %v", got)
	}
	if got := DetectWidths(nil, 3); got != nil {
		t.Errorf("nil data produced %v", got)
	}
	if got := DetectWidths([]byte("abc"), 3); got != nil {
		t.Errorf("tiny data produced %v", got)
	}
}

func TestDetectWidthsFindsGradientRowLength(t *testing.T) {
	const w, h = 100, 80
	grad := make([]byte, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			grad[y*w+x] = byte(x*3 + y)
		}
	}
	got := DetectWidths(grad, 3)
	if len(got) == 0 || got[0] != w {
		t.Errorf("gradient of width %d: got %v", w, got)
	}
}

func TestDefaultWidth(t *testing.T) {
	for n, want := range map[int]int{1: 8, 64: 8, 100: 8, 4096: 64, 65536: 256, 70000: 256, 1 << 20: 1024} {
		if got := DefaultWidth(n); got != want {
			t.Errorf("DefaultWidth(%d) = %d, want %d", n, got, want)
		}
	}
}

func TestSearchPrefersTheRightWidthAndChain(t *testing.T) {
	// A smooth gradient: each row is the row above plus a constant.
	const w, h = 100, 80
	grad := make([]byte, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			grad[y*w+x] = byte(x*3 + y)
		}
	}
	got := Search(grad, SearchOptions{})
	if len(got) == 0 {
		t.Fatal("no candidates")
	}
	best := got[0]
	if best.Width != w || len(best.Chain) == 0 {
		t.Errorf("gradient: best = width %d chain %v, want width %d with a delta chain", best.Width, best.Chain, w)
	}
	plain := -1
	for _, c := range got {
		if c.Width == best.Width && len(c.Chain) == 0 {
			plain = c.Cost
		}
	}
	if best.Cost >= plain {
		t.Errorf("best cost %d is no better than the unprepared %d", best.Cost, plain)
	}
}

func TestSearchIsDeterministicAndNeverEmpty(t *testing.T) {
	data := records(64, 40)
	a, b := Search(data, SearchOptions{}), Search(data, SearchOptions{})
	if len(a) == 0 || len(a) != len(b) {
		t.Fatal("empty or unstable candidate list")
	}
	for i := range a {
		if a[i].Width != b[i].Width || a[i].Cost != b[i].Cost || !a[i].Chain.Equal(b[i].Chain) {
			t.Fatalf("candidate %d differs between runs", i)
		}
	}
	for i := 1; i < len(a); i++ {
		if a[i].Cost < a[i-1].Cost {
			t.Fatal("candidates are not sorted by cost")
		}
	}
	if Search(nil, SearchOptions{}) != nil {
		t.Error("empty data should give no candidates")
	}
	// Fixed width restricts the search to that width.
	for _, c := range Search(data, SearchOptions{Width: 40}) {
		if c.Width != 40 {
			t.Fatalf("fixed width 40 produced a width-%d candidate", c.Width)
		}
	}
	// A fixed width wider than the data still yields something usable.
	if got := Search([]byte("short"), SearchOptions{Width: 100}); len(got) == 0 || got[0].Width > 5 {
		t.Errorf("oversized width: %v", got)
	}
}

func TestFrameDeltaMakesRepeatedFramesZero(t *testing.T) {
	const w, rows, frames = 12, 5, 4
	frame := randomMatrix(w, rows, 77).Pix()
	var data []byte
	for i := 0; i < frames; i++ {
		data = append(data, frame...)
	}
	m := matrix.FromBytes(data, w)
	for _, kind := range []Kind{DeltaFrameSub, DeltaFrameXor} {
		pre, err := Chain{{Kind: kind, K: rows}}.Forward(m)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(pre.Pix()[:w*rows], frame) {
			t.Error("the first frame must be kept whole")
		}
		for i, b := range pre.Pix()[w*rows:] {
			if b != 0 {
				t.Fatalf("kind %d: byte %d of the later frames is %d, want 0", kind, i, b)
			}
		}
	}
}

func TestSearchFindsFrameDeltaOnVideo(t *testing.T) {
	const w, rows, frames = 60, 40, 6
	rng := rand.New(rand.NewSource(5))
	frame := make([]byte, w*rows)
	rng.Read(frame)
	var data []byte
	for i := 0; i < frames; i++ {
		f := append([]byte(nil), frame...)
		f[i] ^= 0xFF // one pixel changes per frame
		data = append(data, f...)
	}
	best := Search(data, SearchOptions{Width: w, FrameRows: rows})[0]
	if len(best.Chain) == 0 || (best.Chain[0].Kind != DeltaFrameSub && best.Chain[0].Kind != DeltaFrameXor) || best.Chain[0].K != rows {
		t.Errorf("expected a frame delta of %d rows to win, got %v", rows, best.Chain)
	}
	for _, c := range Search(data, SearchOptions{Width: w}) {
		for _, s := range c.Chain {
			if s.Kind == DeltaFrameSub || s.Kind == DeltaFrameXor {
				t.Fatal("frame chains must only be tried when FrameRows is set")
			}
		}
	}
}
