package codec

import "fmt"

// Preset is a named bundle of Options trading compression for speed.
//
// The default does the most work: every mode on the whole input, then again
// segment by segment. Whole-input coding is single-threaded in both directions,
// and the repeated attempts multiply the cost, so on a large input the default
// can take minutes and gigabytes. The faster presets work segment by segment,
// where segments are encoded and decoded in parallel, and try fewer things per
// segment.
type Preset int

const (
	// PresetDefault is the smallest output. About 1 MB/s per attempt, several
	// attempts, plus the fractal stage on inputs up to 4 MiB.
	PresetDefault Preset = iota

	// PresetFast keeps the context-mixing coder (still much smaller than
	// DEFLATE on most data) but runs it on 256 KiB segments in parallel across
	// cores, with no whole-input pass and no fractal stage. Each segment tries
	// three layouts in full. (One layout, picked by how well DEFLATE likes it,
	// was tried first and was up to 20% larger on code-like data: DEFLATE's
	// favourite layout is often a poor one for this coder. Picking the layout
	// on a 16-32 KiB sample was tried too and was no more reliable.)
	PresetFast

	// PresetFastest uses DEFLATE-based modes only, on 1 MiB segments in
	// parallel. Roughly an order of magnitude faster than PresetFast, and
	// the output is larger.
	PresetFastest
)

func (p Preset) String() string {
	switch p {
	case PresetDefault:
		return "default"
	case PresetFast:
		return "fast"
	case PresetFastest:
		return "fastest"
	}
	return fmt.Sprintf("preset(%d)", int(p))
}

// ParsePreset converts a preset name to a Preset.
func ParsePreset(s string) (Preset, error) {
	switch s {
	case "default", "":
		return PresetDefault, nil
	case "fast":
		return PresetFast, nil
	case "fastest":
		return PresetFastest, nil
	}
	return 0, fmt.Errorf("unknown preset %q (want default, fast or fastest)", s)
}

// PresetOptions returns the Options for a preset. Fields can be adjusted
// afterwards.
func PresetOptions(p Preset) Options {
	o := DefaultOptions()
	switch p {
	case PresetFast:
		o.DisableFractal = true
		o.SkipWhole = true
		o.SegmentSize = 256 << 10
		o.CMAttempts = 3
		o.SearchSample = 16 << 10
		o.FastFlate = true
	case PresetFastest:
		o.DisableFractal = true
		o.DisableCM = true
		o.SkipWhole = true
		o.SegmentSize = 1 << 20
		o.SearchSample = 16 << 10
		o.FastFlate = true
	}
	return o
}
