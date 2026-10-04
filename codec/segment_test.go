package codec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/rand"
	"testing"

	"fpress/prep"
)

// mixedData joins regions that each want a different treatment.
func mixedData() []byte {
	var b []byte
	b = append(b, gradient(100, 40)...)       // delta-codable at width 100
	b = append(b, randomBytes(8192, 21)...)   // incompressible; spans a whole segment
	b = append(b, sierpinskiBytes(64, 64)...) // self-similar
	b = append(b, make([]byte, 3000)...)      // zeros
	rng := rand.New(rand.NewSource(6))
	rec := make([]byte, 48)
	rng.Read(rec)
	for i := 0; i < 90; i++ { // fixed-size records
		r := append([]byte(nil), rec...)
		r[0], r[47] = byte(i), byte(i*5)
		b = append(b, r...)
	}
	return b
}

func segOpts(size int) Options {
	o := DefaultOptions()
	o.SegmentSize = size
	return o
}

func TestSegmentedRoundTripSizes(t *testing.T) {
	base := mixedData()
	for _, segSize := range []int{1024, 4096, 5000} {
		for _, n := range []int{1025, 4095, 4096, 4097, 10000, 3 * 4096, len(base)} {
			data := base[:n]
			blob := segmentedContainer(t, data, segSize)
			if blob[5] != byte(ModeSegmented) {
				t.Fatal("not a segmented container")
			}
			got, err := Decompress(blob)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("segSize %d len %d: round trip failed: %v", segSize, n, err)
			}
		}
	}
}

func TestSegmentedWinsOnMixedData(t *testing.T) {
	data := mixedData()
	blob, rep, err := CompressReport(data, segOpts(4096))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("mixed %d B: flate=%d prep=%d fractal=%d cm=%d segmented=%d (segments by mode %v) chosen=%s",
		len(data), rep.Flate, rep.PrepFlate, rep.Fractal, rep.CM, rep.Segmented, rep.SegModes, rep.Chosen)
	whole := min(rep.Flate, rep.PrepFlate)
	for _, v := range []int{rep.Fractal, rep.CM} {
		if v >= 0 {
			whole = min(whole, v)
		}
	}
	if rep.Segmented < 0 || rep.Segmented >= whole {
		t.Errorf("segmented (%d) should beat the best whole-file mode (%d)", rep.Segmented, whole)
	}
	if rep.Chosen != ModeSegmented {
		t.Errorf("chosen %v, want segmented", rep.Chosen)
	}
	if used := rep.SegModes; used[ModeStored] == 0 || used[ModePrep]+used[ModeFractal]+used[ModeCM] == 0 {
		t.Errorf("expected a mix of modes across segments, got %v", used)
	}
	if got, err := Decompress(blob); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("round trip failed: %v", err)
	}
}

func TestSegmentOption(t *testing.T) {
	data := mixedData()
	_, rep, _ := CompressReport(data, segOpts(-1))
	if rep.Segmented != -1 {
		t.Error("negative SegmentSize should disable the mode")
	}
	_, rep, _ = CompressReport(data[:3000], segOpts(4096)) // not longer than one segment
	if rep.Segmented != -1 {
		t.Error("input within one segment should not try segmenting")
	}
	_, rep, _ = CompressReport(data, segOpts(1)) // clamped up to the minimum
	if rep.SegSize != minSegment {
		t.Errorf("segment size %d, want clamp to %d", rep.SegSize, minSegment)
	}
}

func TestSegmentedOutputIsIndependentOfWorkers(t *testing.T) {
	data := mixedData()
	var ref []byte
	for _, w := range []int{1, 2, 8} {
		o := segOpts(4096)
		o.Fractal.Workers = w
		blob, err := Compress(data, o)
		if err != nil {
			t.Fatal(err)
		}
		if ref == nil {
			ref = blob
		} else if !bytes.Equal(ref, blob) {
			t.Fatalf("output with %d workers differs", w)
		}
	}
}

// ---- quick reject ----------------------------------------------------------

func TestQuickRejectSkipsRandomData(t *testing.T) {
	data := randomBytes(200000, 13)
	blob, rep, err := CompressReport(data, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Rejected || rep.Chosen != ModeStored {
		t.Errorf("random data: rejected=%v chosen=%v", rep.Rejected, rep.Chosen)
	}
	if rep.Flate != -1 || rep.PrepFlate != -1 || rep.Fractal != -1 || rep.Segmented != -1 {
		t.Errorf("expensive modes ran despite the reject: %+v", rep)
	}
	if len(blob) > len(data)+16 {
		t.Errorf("stored output %d for %d input", len(blob), len(data))
	}
	if got, err := Decompress(blob); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("round trip failed: %v", err)
	}
	// The switch turns it off.
	o := DefaultOptions()
	o.NoQuickReject = true
	if _, rep, _ := CompressReport(data[:20000], o); rep.Rejected || rep.Flate < 0 {
		t.Error("NoQuickReject did not disable the reject")
	}
}

// Data that looks random to DEFLATE but has structure must not be rejected.
func TestQuickRejectSparesHiddenStructure(t *testing.T) {
	// A gradient whose rows each shift by one: uniform byte histogram, no
	// repeated strings for DEFLATE, but a clear period and a huge prep win.
	grad := gradient(1000, 300)
	if hopeless(grad) {
		t.Fatal("gradient was judged hopeless")
	}
	_, rep, err := CompressReport(grad, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("gradient 1000x300: flate=%d prep=%d chosen=%s", rep.Flate, rep.PrepFlate, rep.Chosen)
	if rep.Rejected || rep.Out > len(grad)/20 {
		t.Errorf("hidden structure lost: rejected=%v out=%d of %d", rep.Rejected, rep.Out, len(grad))
	}
	// Ordinary compressible inputs are never rejected either.
	for name, data := range map[string][]byte{
		"text":       bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog. "), 400),
		"zeros":      make([]byte, 50000),
		"sierpinski": sierpinskiBytes(128, 128),
	} {
		if hopeless(data) {
			t.Errorf("%s judged hopeless", name)
		}
	}
	// Small inputs are always tried.
	if hopeless(randomBytes(4000, 1)) {
		t.Error("input under 4096 bytes should never be rejected")
	}
}

// ---- damage ----------------------------------------------------------------

func TestSegmentedEveryTruncationFails(t *testing.T) {
	blob := segmentedContainer(t, mixedData()[:3500], 1024)
	for n := 0; n < len(blob); n++ {
		if _, err := Decompress(blob[:n]); err == nil {
			t.Fatalf("truncation to %d/%d bytes accepted", n, len(blob))
		}
	}
}

func TestSegmentedBitFlipsNeverGiveWrongData(t *testing.T) {
	data := mixedData()[:3500]
	blob := segmentedContainer(t, data, 1024)
	for i := range blob {
		for bit := 0; bit < 8; bit++ {
			bad := append([]byte(nil), blob...)
			bad[i] ^= 1 << bit
			if got, err := Decompress(bad); err == nil && !bytes.Equal(got, data) {
				t.Fatalf("flipping bit %d of byte %d produced wrong data silently", bit, i)
			}
		}
	}
}

func TestSegmentedRejectsBadStructure(t *testing.T) {
	data := bytes.Repeat([]byte("abcd"), 600) // 2400 bytes -> 3 segments of 1024 (last shorter)
	seg := func(mode byte, payload []byte) []byte {
		out := []byte{mode}
		out = binary.AppendUvarint(out, uint64(len(payload)))
		return append(out, payload...)
	}
	stored := func(n int) []byte { return seg(byte(ModeStored), data[:n]) }
	sizeHdr := func(size uint64) []byte { return binary.AppendUvarint(nil, size) }
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	mk := func(payload []byte) []byte { return container(ModeSegmented, len(data), crc(data), payload) }

	good := cat(sizeHdr(1024), stored(1024), stored(1024), stored(352))
	if got, err := Decompress(mk(good)); err != nil || !bytes.Equal(got, data[:0:0]) && len(got) != len(data) {
		// the stored segments hold data[:n] each, so the CRC will differ; only structure matters here
		if !errors.Is(err, ErrChecksum) {
			t.Fatalf("well-formed structure rejected: %v", err)
		}
	}
	cases := map[string][]byte{
		"segment size zero":  cat(sizeHdr(0), stored(1024), stored(1024), stored(352)),
		"segment too small":  cat(sizeHdr(100), stored(1024)),
		"segment too big":    cat(sizeHdr(1<<40), stored(1024)),
		"too few segments":   cat(sizeHdr(1024), stored(1024), stored(1024)),
		"too many segments":  cat(sizeHdr(1024), stored(1024), stored(1024), stored(352), stored(1)),
		"nested segmented":   cat(sizeHdr(1024), seg(byte(ModeSegmented), nil), stored(1024), stored(352)),
		"unknown mode":       cat(sizeHdr(1024), seg(9, nil), stored(1024), stored(352)),
		"length lies":        cat(sizeHdr(1024), []byte{byte(ModeStored), 0xff, 0x7f}, stored(1024)),
		"wrong segment size": cat(sizeHdr(1024), stored(1000), stored(1024), stored(352)),
		"no table":           sizeHdr(1024),
		"empty":              nil,
	}
	for name, payload := range cases {
		if _, err := Decompress(mk(payload)); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: got %v, want ErrCorrupt", name, err)
		}
	}
	if _, err := Decompress(container(ModeSegmented, 0, 0, sizeHdr(1024))); !errors.Is(err, ErrCorrupt) {
		t.Errorf("empty original: got %v, want ErrCorrupt", err)
	}
}

// ---- context-mixing mode ----------------------------------------------------

func TestCMModeRoundTripsAndWins(t *testing.T) {
	for name, data := range samples() {
		if len(data) == 0 {
			continue
		}
		for _, width := range []int{0, 1, 17, 64} {
			if width > len(data) {
				continue
			}
			blob := cmContainer(t, data, width, nil)
			got, err := Decompress(blob)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("%s width %d: round trip failed: %v", name, width, err)
			}
		}
	}
	// On text-like data the coder should beat DEFLATE by a clear margin.
	text := bytes.Repeat([]byte("It was the best of times, it was the worst of times, it was the age of wisdom. "), 60)
	text = append(text, randomBytes(100, 3)...)
	_, rep, err := CompressReport(text, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("text: flate=%d cm=%d chosen=%s", rep.Flate, rep.CM, rep.Chosen)
	if rep.CM < 0 || rep.CM >= rep.Flate {
		t.Errorf("cm (%d) should beat flate (%d) on text", rep.CM, rep.Flate)
	}
	// And the option turns it off.
	o := DefaultOptions()
	o.DisableCM = true
	if _, rep, _ := CompressReport(text, o); rep.CM != -1 || rep.Chosen == ModeCM {
		t.Errorf("DisableCM ignored: cm=%d chosen=%s", rep.CM, rep.Chosen)
	}
	o = DefaultOptions()
	o.MaxCMSize = 100
	if _, rep, _ := CompressReport(text, o); rep.CM != -1 {
		t.Errorf("MaxCMSize ignored: cm=%d", rep.CM)
	}
}

func TestCMModeRejectsBadHeaders(t *testing.T) {
	data := bytes.Repeat([]byte("abcdefgh"), 100)
	good := cmContainer(t, data, 0, nil)
	if _, err := Decompress(good); err != nil {
		t.Fatal(err)
	}
	stream := entropyOf(data, 0)
	mk := func(payload []byte) []byte { return container(ModeCM, len(data), crc(data), payload) }
	cases := map[string][]byte{
		"empty payload":       nil,
		"plain with chain":    append([]byte{0, 1, byte(prep.DeltaUpSub)}, stream...),
		"plain, no chain":     {0},
		"width > length":      append([]byte{0xff, 0x7f, 0}, stream...),
		"unknown step":        append([]byte{16, 1, 77}, stream...),
		"folded bitplanes":    append([]byte{15, 1, byte(prep.BitPlanes)}, stream...),
		"stream cut":          append([]byte{0, 0}, stream[:len(stream)/2]...),
		"stream with trailer": append(append([]byte{0, 0}, stream...), 1, 2, 3),
	}
	for name, payload := range cases {
		if _, err := Decompress(mk(payload)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := Decompress(container(ModeCM, 0, 0, []byte{0, 0})); err == nil {
		t.Error("empty original accepted")
	}
}

func TestFlateModesRejectTrailingBytes(t *testing.T) {
	data := bytes.Repeat([]byte("trailing bytes after a deflate stream are not allowed. "), 30)
	for name, blob := range map[string][]byte{
		"flate": container(ModeFlate, len(data), crc(data), deflate(data)),
		"prep":  prepContainer(t, data, 40, nil),
	} {
		if _, err := Decompress(blob); err != nil {
			t.Fatalf("%s: valid container rejected: %v", name, err)
		}
		if _, err := Decompress(append(append([]byte(nil), blob...), 0)); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: trailing byte: got %v, want ErrCorrupt", name, err)
		}
	}
}
