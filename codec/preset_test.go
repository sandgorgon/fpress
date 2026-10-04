package codec

import (
	"bytes"
	"os"
	"testing"

	"fpress/prep"
)

func TestParsePreset(t *testing.T) {
	for name, want := range map[string]Preset{"default": PresetDefault, "": PresetDefault, "fast": PresetFast, "fastest": PresetFastest} {
		got, err := ParsePreset(name)
		if err != nil || got != want {
			t.Errorf("ParsePreset(%q) = %v, %v", name, got, err)
		}
		if name != "" && got.String() != name {
			t.Errorf("String() of %q = %q", name, got.String())
		}
	}
	if _, err := ParsePreset("turbo"); err == nil {
		t.Error("unknown preset accepted")
	}
}

// The default preset must stay exactly the default options.
func TestDefaultPresetIsTheDefaultOptions(t *testing.T) {
	a, b := PresetOptions(PresetDefault), DefaultOptions()
	if a != b {
		t.Errorf("PresetOptions(PresetDefault) differs from DefaultOptions:\n%+v\n%+v", a, b)
	}
}

func TestEveryPresetRoundTrips(t *testing.T) {
	inputs := map[string][]byte{
		"mixed":      mixedData(),
		"gradient":   gradient(100, 120),
		"sierpinski": sierpinskiBytes(128, 128),
		"random":     randomBytes(70000, 3),
		"text":       bytes.Repeat([]byte("presets must all produce decodable containers. "), 500),
		"tiny":       []byte("hi"),
		"empty":      nil,
	}
	for _, p := range []Preset{PresetDefault, PresetFast, PresetFastest} {
		opt := PresetOptions(p)
		opt.SegmentSize = 4096 // small enough that these inputs are segmented
		for name, data := range inputs {
			blob, err := Compress(data, opt)
			if err != nil {
				t.Fatalf("%v/%s: %v", p, name, err)
			}
			got, err := Decompress(blob)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("%v/%s: round trip failed: %v", p, name, err)
			}
		}
	}
}

func TestFastPresetWorksSegmentwise(t *testing.T) {
	data := mixedData()
	opt := PresetOptions(PresetFast)
	opt.SegmentSize = 4096
	blob, rep, err := CompressReport(data, opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("fast: segmented=%d chosen=%s segment modes %v", rep.Segmented, rep.Chosen, rep.SegModes)
	// No whole-input attempts at all.
	if rep.Flate != -1 || rep.PrepFlate != -1 || rep.Fractal != -1 || rep.CM != -1 {
		t.Errorf("whole-input modes ran despite SkipWhole: %+v", rep)
	}
	if rep.Segmented < 0 || rep.Chosen != ModeSegmented {
		t.Errorf("expected the segmented mode, got chosen=%s segmented=%d", rep.Chosen, rep.Segmented)
	}
	if rep.SegModes[ModeFractal] != 0 {
		t.Error("fast preset used the fractal stage")
	}
	if rep.SegModes[ModeCM] == 0 {
		t.Error("fast preset should still use the context-mixing coder on some segments")
	}
	if got, err := Decompress(blob); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("round trip failed: %v", err)
	}
}

func TestFastestPresetUsesNoSlowCoders(t *testing.T) {
	data := mixedData()
	opt := PresetOptions(PresetFastest)
	opt.SegmentSize = 4096
	_, rep, err := CompressReport(data, opt)
	if err != nil {
		t.Fatal(err)
	}
	if rep.SegModes[ModeCM] != 0 || rep.SegModes[ModeFractal] != 0 {
		t.Errorf("fastest used a slow coder: %v", rep.SegModes)
	}
	if rep.SegModes[ModeFlate]+rep.SegModes[ModePrep]+rep.SegModes[ModeStored] == 0 {
		t.Error("no segments at all?")
	}
}

// The presets are a trade-off in the right direction: each step down gives up
// compression, never gains it, on data where the slow coders matter.
func TestPresetsTradeSizeForSpeedInOrder(t *testing.T) {
	data := bytes.Repeat([]byte("It was the best of times, it was the worst of times, it was the age of wisdom, it was the age of foolishness. "), 400)
	data = append(data, gradient(100, 100)...)
	size := func(p Preset) int {
		opt := PresetOptions(p)
		opt.SegmentSize = 8192
		blob, err := Compress(data, opt)
		if err != nil {
			t.Fatal(err)
		}
		return len(blob)
	}
	def, fast, fastest := size(PresetDefault), size(PresetFast), size(PresetFastest)
	t.Logf("default=%d fast=%d fastest=%d", def, fast, fastest)
	if !(def <= fast && fast <= fastest) {
		t.Errorf("sizes not ordered default <= fast <= fastest: %d, %d, %d", def, fast, fastest)
	}
}

func TestSkipWholeStillRejectsRandomData(t *testing.T) {
	opt := PresetOptions(PresetFast)
	data := randomBytes(600000, 8) // longer than a fast-preset segment
	blob, rep, err := CompressReport(data, opt)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Rejected || rep.Chosen != ModeStored || rep.Segmented != -1 {
		t.Errorf("random input: rejected=%v chosen=%s segmented=%d", rep.Rejected, rep.Chosen, rep.Segmented)
	}
	if got, err := Decompress(blob); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("round trip failed: %v", err)
	}
}

// An input that fits in one segment has nothing to segment: the whole-input path runs.
func TestSkipWholeDoesNotApplyToShortInputs(t *testing.T) {
	opt := PresetOptions(PresetFast)
	_, rep, err := CompressReport(gradient(100, 100), opt) // 10 KB < 256 KiB
	if err != nil {
		t.Fatal(err)
	}
	if rep.Flate < 0 || rep.CM < 0 || rep.Segmented != -1 {
		t.Errorf("short input should use the whole-input path: flate=%d cm=%d segmented=%d", rep.Flate, rep.CM, rep.Segmented)
	}
}

func TestCMAttemptsLimit(t *testing.T) {
	cands := []prep.Candidate{
		{Width: 100, Chain: prep.Chain{{Kind: prep.DeltaUpSub}}},
		{Width: 64},
		{Width: 128},
	}
	if got := cmAttempts(cands, 10000, 0, false); len(got) != 3 {
		t.Errorf("automatic: %d attempts, want 3", len(got))
	}
	if got := cmAttempts(cands, 10000, 1, false); len(got) != 1 || got[0].Width != 100 {
		t.Errorf("limit 1: %+v, want only the best candidate", got)
	}
	if got := cmAttempts(cands, 10000, 2, false); len(got) != 2 {
		t.Errorf("limit 2: %d attempts", len(got))
	}
	if got := cmAttempts(cands, 10000, 9, false); len(got) != 3 {
		t.Errorf("limit above what exists: %d attempts, want 3", len(got))
	}
	if got := cmAttempts(nil, 10000, 0, false); len(got) != 1 {
		t.Errorf("no candidates: %d attempts, want just the plain stream", len(got))
	}
}

func TestFastFlateStillDecodes(t *testing.T) {
	data := bytes.Repeat([]byte("fast flate level "), 2000)
	for _, fast := range []bool{false, true} {
		opt := Options{Fractal: DefaultOptions().Fractal, DisableCM: true, DisableFractal: true, FastFlate: fast}
		blob, err := Compress(data, opt)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := Decompress(blob); err != nil || !bytes.Equal(got, data) {
			t.Fatalf("FastFlate=%v: round trip failed: %v", fast, err)
		}
	}
}

// Trying more layouts can only help: the first layout tried is the same one a
// single-layout run would use. This is the regression behind PresetFast
// trying three: with one, code-like data came out ~20% larger.
func TestFastPresetTriesEnoughLayouts(t *testing.T) {
	if got := PresetOptions(PresetFast).CMAttempts; got < 3 {
		t.Fatalf("PresetFast tries %d layouts per segment; with fewer, code-like data suffers", got)
	}
	data := mixedData()
	size := func(attempts int) int {
		opt := PresetOptions(PresetFast)
		opt.SegmentSize = 8192
		opt.CMAttempts = attempts
		blob, err := Compress(data, opt)
		if err != nil {
			t.Fatal(err)
		}
		return len(blob)
	}
	one, three := size(1), size(3)
	t.Logf("fast with 1 layout: %d bytes, with 3: %d", one, three)
	if three > one {
		t.Errorf("3 layouts (%d) gave a larger result than 1 (%d)", three, one)
	}
}

// On real executable code, the fast preset should land close to the default.
func TestFastPresetIsCloseToDefaultOnCode(t *testing.T) {
	b, err := os.ReadFile("/bin/ls")
	if err != nil || len(b) < 80000 {
		t.Skip("no /bin/ls to test with")
	}
	data := b[:80000]
	def, err := Compress(data, PresetOptions(PresetDefault))
	if err != nil {
		t.Fatal(err)
	}
	fast, err := Compress(data, PresetOptions(PresetFast))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("/bin/ls[80000]: default %d bytes, fast %d bytes (%+.1f%%)", len(def), len(fast), 100*(float64(len(fast))/float64(len(def))-1))
	if float64(len(fast)) > 1.05*float64(len(def)) {
		t.Errorf("fast (%d) is more than 5%% larger than default (%d) on executable code", len(fast), len(def))
	}
}
