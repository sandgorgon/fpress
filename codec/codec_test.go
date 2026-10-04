package codec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/rand"
	"os"
	"testing"

	"fpress/prep"
)

func randomBytes(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// sierpinskiBytes is exactly self-similar data, laid out at the given width.
func sierpinskiBytes(width, height int) []byte {
	b := make([]byte, width*height)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			if x&y == 0 {
				b[y*width+x] = 255
			}
		}
	}
	return b
}

func samples() map[string][]byte {
	text := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog. "), 40)
	return map[string][]byte{
		"empty":      {},
		"one":        {7},
		"tiny":       []byte("hello"),
		"zeros":      make([]byte, 5000),
		"ones":       bytes.Repeat([]byte{0xFF}, 3000),
		"random":     randomBytes(4000, 1),
		"random-odd": randomBytes(4093, 2),
		"text":       text,
		"sierpinski": sierpinskiBytes(64, 64),
		"sierp-odd":  sierpinskiBytes(64, 64)[:4001],
		"ramp": func() []byte {
			b := make([]byte, 3000)
			for i := range b {
				b[i] = byte(i / 7)
			}
			return b
		}(),
	}
}

func TestRoundTripAllInputs(t *testing.T) {
	for name, data := range samples() {
		for _, opt := range []Options{
			DefaultOptions(),
			{Fractal: DefaultOptions().Fractal, Width: 64},
			{DisableFractal: true},
		} {
			blob, rep, err := CompressReport(data, opt)
			if err != nil {
				t.Fatalf("%s: Compress: %v", name, err)
			}
			got, err := Decompress(blob)
			if err != nil {
				t.Fatalf("%s: Decompress: %v", name, err)
			}
			if !bytes.Equal(got, data) {
				t.Fatalf("%s: round trip mismatch", name)
			}
			if rep.Out != len(blob) {
				t.Errorf("%s: report says %d bytes, got %d", name, rep.Out, len(blob))
			}
		}
	}
}

// The output must never be much bigger than the input, whatever the content.
func TestNeverMuchLargerThanInput(t *testing.T) {
	for name, data := range samples() {
		blob, err := Compress(data, DefaultOptions())
		if err != nil {
			t.Fatal(err)
		}
		if len(blob) > len(data)+16 {
			t.Errorf("%s: %d bytes in, %d bytes out", name, len(data), len(blob))
		}
	}
}

func TestModeSelection(t *testing.T) {
	_, rep, err := CompressReport(randomBytes(4000, 9), DefaultOptions())
	if err != nil || rep.Chosen != ModeStored {
		t.Errorf("random data should be stored, got %v (err %v)", rep.Chosen, err)
	}
	_, rep, err = CompressReport(make([]byte, 5000), DefaultOptions())
	if err != nil || rep.Chosen == ModeStored {
		t.Errorf("zeros should compress, got %v (err %v)", rep.Chosen, err)
	}
	// Disabling or exceeding the fractal limit must skip that candidate.
	_, rep, _ = CompressReport(sierpinskiBytes(64, 64), Options{DisableFractal: true})
	if rep.Fractal != -1 || rep.FractalStats != nil {
		t.Error("fractal attempted although disabled")
	}
	o := DefaultOptions()
	o.MaxFractalSize = 100
	_, rep, _ = CompressReport(sierpinskiBytes(64, 64), o)
	if rep.Fractal != -1 {
		t.Error("fractal attempted above MaxFractalSize")
	}
}

// A forced fractal container must itself round trip, whichever mode wins.
func TestFractalModeContainerDecodes(t *testing.T) {
	data := sierpinskiBytes(64, 64)
	_, rep, err := CompressReport(data, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Fractal < 0 {
		t.Fatal("fractal candidate missing")
	}
	// Rebuild the fractal container directly and decode it.
	blob := fractalContainer(t, data, 64)
	got, err := Decompress(blob)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("fractal-mode container did not round trip (err %v)", err)
	}
	if blob[5] != byte(ModeFractal) {
		t.Fatal("container is not in fractal mode")
	}
}

// A gradient is the classic case for prep: DEFLATE alone sees noise, but with
// the right width and a delta chain every row is a constant.
func gradient(w, h int) []byte {
	b := make([]byte, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			b[y*w+x] = byte(x*3 + y)
		}
	}
	return b
}

func TestPrepModeWinsOnGradient(t *testing.T) {
	data := gradient(100, 80)
	blob, rep, err := CompressReport(data, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("gradient: flate=%d prep=%d (width %d, %v) fractal=%d chosen=%s out=%d",
		rep.Flate, rep.PrepFlate, rep.PrepWidth, rep.PrepChain, rep.Fractal, rep.Chosen, rep.Out)
	if rep.PrepWidth != 100 || len(rep.PrepChain) == 0 {
		t.Errorf("expected width 100 with a delta chain, got %d %v", rep.PrepWidth, rep.PrepChain)
	}
	if rep.PrepFlate >= rep.Flate/2 {
		t.Errorf("prep (%d) should be well under half of plain flate (%d)", rep.PrepFlate, rep.Flate)
	}
	if got, err := Decompress(blob); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("round trip failed: %v", err)
	}
	// Turning prep off must lose that advantage but still round trip.
	off := DefaultOptions()
	off.DisablePrep = true
	blob2, rep2, err := CompressReport(data, off)
	if err != nil || rep2.PrepFlate != -1 {
		t.Fatalf("DisablePrep: err=%v prep=%d", err, rep2.PrepFlate)
	}
	if len(blob2) <= len(blob) {
		t.Errorf("without prep (%d) should be larger than with (%d)", len(blob2), len(blob))
	}
}

func TestPrepFindsRecordWidth(t *testing.T) {
	// 60 records of 48 bytes: a fixed template with a counter field.
	rng := rand.New(rand.NewSource(8))
	template := make([]byte, 48)
	rng.Read(template)
	var data []byte
	for i := 0; i < 60; i++ {
		rec := append([]byte(nil), template...)
		rec[0], rec[47] = byte(i), byte(i*5)
		data = append(data, rec...)
	}
	_, rep, err := CompressReport(data, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("records: flate=%d prep=%d (width %d, %v) chosen=%s", rep.Flate, rep.PrepFlate, rep.PrepWidth, rep.PrepChain, rep.Chosen)
	if rep.PrepWidth%48 != 0 {
		t.Errorf("expected a width that is a multiple of the 48-byte record, got %d", rep.PrepWidth)
	}
}

// Every catalogue chain must work end to end in both folded modes.
func TestEveryChainRoundTripsInAllFoldedModes(t *testing.T) {
	data := append(gradient(64, 24), randomBytes(500, 4)...)
	for _, width := range []int{32, 64, 40} {
		for _, chain := range prep.Catalogue {
			if _, _, err := chain.OutDims(width, (len(data)+width-1)/width); err != nil {
				continue
			}
			for name, blob := range map[string][]byte{
				"prep":    prepContainer(t, data, width, chain),
				"fractal": fractalContainerChain(t, data, width, chain),
				"cm":      cmContainer(t, data, width, chain),
			} {
				got, err := Decompress(blob)
				if err != nil || !bytes.Equal(got, data) {
					t.Fatalf("%s width %d chain %v: round trip failed: %v", name, width, chain, err)
				}
			}
		}
	}
}

func TestFoldedModesRejectBadHeaders(t *testing.T) {
	data := gradient(32, 32)
	chain := prep.Chain{{Kind: prep.DeltaUpSub}}
	good := prepContainer(t, data, 32, chain)
	if _, err := Decompress(good); err != nil {
		t.Fatal(err)
	}
	mk := func(mode Mode, payload []byte) []byte { return container(mode, len(data), crc(data), payload) }
	body := deflate(data)
	cases := map[string][]byte{
		"zero width":        mk(ModePrep, append([]byte{0, 0}, body...)),
		"width > length":    mk(ModePrep, append([]byte{0xff, 0x7f, 0}, body...)),
		"chain too long":    mk(ModePrep, append([]byte{32, 99}, body...)),
		"bitplanes bad dim": mk(ModePrep, append([]byte{33, 1, byte(prep.BitPlanes)}, body...)),
		"unknown step":      mk(ModePrep, append([]byte{32, 1, 77}, body...)),
		"no chain":          mk(ModePrep, []byte{32}),
		"fractal garbage":   mk(ModeFractal, []byte{32, 0, 1, 2, 3}),
		"empty original":    container(ModePrep, 0, 0, append([]byte{1, 0}, body...)),
	}
	for name, blob := range cases {
		if _, err := Decompress(blob); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: got %v, want ErrCorrupt", name, err)
		}
	}
}

// ---- damage ----------------------------------------------------------------

func containers(t testing.TB) map[string][]byte {
	t.Helper()
	data := sierpinskiBytes(32, 32)
	out := map[string][]byte{}
	out["stored"] = container(ModeStored, len(data), crc(data), data)
	flated, err := Compress(data, Options{DisableFractal: true})
	if err != nil {
		t.Fatal(err)
	}
	out["flate"] = flated
	out["fractal"] = fractalContainer(t, data, 32)
	out["prep"] = prepContainer(t, data, 32, prep.Chain{{Kind: prep.DeltaLeftSub}, {Kind: prep.DeltaUpSub}})
	out["prep-bitplanes"] = prepContainer(t, data, 32, prep.Chain{{Kind: prep.BitPlanes}})
	out["segmented"] = segmentedContainer(t, data, 1024)
	out["cm-plain"] = cmContainer(t, data, 0, nil)
	out["cm-2d"] = cmContainer(t, data, 32, nil)
	out["cm-chain"] = cmContainer(t, data, 32, prep.Chain{{Kind: prep.DeltaUpSub}})
	out["fractal-chain"] = fractalContainerChain(t, data, 32, prep.Chain{{Kind: prep.Deinterleave, K: 2}, {Kind: prep.DeltaUpXor}})
	return out
}

func TestEveryTruncationFails(t *testing.T) {
	for name, blob := range containers(t) {
		for n := 0; n < len(blob); n++ {
			if _, err := Decompress(blob[:n]); err == nil {
				t.Fatalf("%s: truncation to %d/%d bytes accepted", name, n, len(blob))
			}
		}
	}
}

// Flipping any single bit must either be rejected or still give the exact
// original; it may never silently give different data.
func TestBitFlipsNeverGiveWrongData(t *testing.T) {
	data := sierpinskiBytes(32, 32)
	for name, blob := range containers(t) {
		for i := range blob {
			for bit := 0; bit < 8; bit++ {
				bad := append([]byte(nil), blob...)
				bad[i] ^= 1 << bit
				got, err := Decompress(bad)
				if err == nil && !bytes.Equal(got, data) {
					t.Fatalf("%s: flipping bit %d of byte %d produced wrong data silently", name, bit, i)
				}
			}
		}
	}
}

func TestChecksumMismatchIsReported(t *testing.T) {
	data := []byte("some data that is stored verbatim")
	blob := container(ModeStored, len(data), crc(data)+1, data)
	if _, err := Decompress(blob); !errors.Is(err, ErrChecksum) {
		t.Fatalf("got %v, want ErrChecksum", err)
	}
}

func TestHeaderErrors(t *testing.T) {
	good := container(ModeStored, 3, crc([]byte("abc")), []byte("abc"))
	if _, err := Decompress(good); err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"empty":       {},
		"bad magic":   append([]byte("XXXX"), good[4:]...),
		"bad version": func() []byte { b := append([]byte(nil), good...); b[4] = 99; return b }(),
		"bad mode":    func() []byte { b := append([]byte(nil), good...); b[5] = 9; return b }(),
		"length lies": container(ModeStored, 5, crc([]byte("abc")), []byte("abc")),
		"trailing":    append(append([]byte(nil), good...), 'x'),
	}
	for name, blob := range cases {
		if _, err := Decompress(blob); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: got %v, want ErrCorrupt", name, err)
		}
	}
}

func TestSizeLimit(t *testing.T) {
	data := make([]byte, 5000)
	blob, err := Compress(data, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecompressLimit(blob, 4999); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
	if _, err := DecompressLimit(blob, 5000); err != nil {
		t.Fatalf("limit equal to size should pass: %v", err)
	}
	// A header claiming an enormous original must be refused before allocating.
	huge := binary.AppendUvarint(append([]byte(magic), version, byte(ModeFlate)), 1<<40)
	huge = append(huge, 0, 0, 0, 0)
	if _, err := Decompress(huge); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
}

func TestRealFile(t *testing.T) {
	b, err := os.ReadFile("/bin/ls")
	if err != nil || len(b) < 40000 {
		t.Skip("no /bin/ls to test with")
	}
	data := b[:40000]
	blob, rep, err := CompressReport(data, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decompress(blob)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("real file round trip failed: %v", err)
	}
	t.Logf("/bin/ls[40000]: stored=%d flate=%d fractal=%d chosen=%s out=%d",
		rep.Stored, rep.Flate, rep.Fractal, rep.Chosen, rep.Out)
}

func FuzzDecompress(f *testing.F) {
	for _, blob := range containers(f) {
		f.Add(blob)
	}
	f.Add([]byte{})
	f.Add([]byte("FPRS\x01\x02\xff\xff\xff\xff\xff\xff\xff\x7f"))
	f.Fuzz(func(t *testing.T, blob []byte) {
		Decompress(blob) // limited below; an error is fine, a panic or huge allocation is not
		DecompressLimit(blob, 1<<20)
	})
}

func FuzzCompressRoundTrip(f *testing.F) {
	f.Add([]byte("abcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabc"), uint8(0), uint8(0))
	f.Add(sierpinskiBytes(16, 16), uint8(16), uint8(1))
	f.Add(mixedData()[:3000], uint8(0), uint8(5))
	f.Fuzz(func(t *testing.T, data []byte, width, seg uint8) {
		if len(data) > 3500 {
			t.Skip()
		}
		opt := Options{Fractal: DefaultOptions().Fractal, Width: int(width), SegmentSize: minSegment + int(seg)*37}
		blob, err := Compress(data, opt)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decompress(blob)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("round trip failed: %v", err)
		}
	})
}
