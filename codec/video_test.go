package codec

import (
	"bytes"
	"math/rand"
	"testing"
)

func TestParseVideo(t *testing.T) {
	for _, c := range []struct {
		spec          string
		width, frames int
	}{
		{"320x180", 960, 180},
		{"320x180:rgb24", 960, 180},
		{"64x48:gray8", 64, 48},
		{"64x48:gray16", 128, 48},
		{"64x48:RGBA", 256, 48},
		{"64x48:yuv420", 64, 72},
	} {
		w, r, err := ParseVideo(c.spec)
		if err != nil || w != c.width || r != c.frames {
			t.Errorf("%s: got %d,%d,%v want %d,%d", c.spec, w, r, err, c.width, c.frames)
		}
	}
	for _, bad := range []string{"", "320", "320x", "x180", "0x10", "320x180:foo", "64x47:yuv420", "axb", "10x70000"} {
		if _, _, err := ParseVideo(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

// A static scene with a small moving square and no noise: every frame but the
// first is almost identical to the one before.
func staticVideo(w, h, frames int) []byte {
	rng := rand.New(rand.NewSource(12))
	bg := make([]byte, w*h*3)
	rng.Read(bg)
	var out []byte
	for n := 0; n < frames; n++ {
		f := append([]byte(nil), bg...)
		for y := 10; y < 14; y++ {
			for x := 4 * n; x < 4*n+4 && x < w; x++ {
				copy(f[(y*w+x)*3:], []byte{255, 0, 0})
			}
		}
		out = append(out, f...)
	}
	return out
}

func TestVideoModeRoundTripsAndShrinks(t *testing.T) {
	const w, h, frames = 160, 100, 10
	data := staticVideo(w, h, frames)
	plain := DefaultOptions()
	plain.DisableFractal = true
	base, _, err := CompressReport(data, plain)
	if err != nil {
		t.Fatal(err)
	}

	vid := plain
	vid.Width, vid.FrameRows = w*3, h
	blob, rep, err := CompressReport(data, vid)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decompress(blob)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("video container did not round trip: %v", err)
	}
	t.Logf("%d B of video: %d bytes ordinary, %d with the video mode (chosen %s)", len(data), len(base), len(blob), rep.Chosen)
	// The background is random, so only differencing can find the repeats.
	if len(blob)*4 > len(base) {
		t.Errorf("video mode should be at least 4x smaller here: %d vs %d", len(blob), len(base))
	}
}

func TestVideoModeNeverMuchWorseOnNoise(t *testing.T) {
	// Pure noise: the frame delta only doubles it, so it must not be chosen
	// and the result must stay close to the ordinary one.
	const w, h, frames = 48, 32, 12
	data := make([]byte, w*h*3*frames)
	rand.New(rand.NewSource(1)).Read(data)
	vid := DefaultOptions()
	vid.Width, vid.FrameRows = w*3, h
	blob, _, err := CompressReport(data, vid)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Decompress(blob); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("round trip failed: %v", err)
	}
	if len(blob) > len(data)+64 {
		t.Errorf("noise grew from %d to %d", len(data), len(blob))
	}
}

func TestVideoSegmentsHoldSeveralFrames(t *testing.T) {
	o := DefaultOptions()
	o.Width, o.FrameRows = 960, 180
	fb := 960 * 180
	if seg := resolveSegmentSize(o); seg < 2*fb || seg%fb != 0 {
		t.Errorf("video segment %d is not a whole number (>= 2) of %d-byte frames", seg, fb)
	}
	o.SegmentSize = -1
	if resolveSegmentSize(o) != 0 {
		t.Error("an explicit -segment -1 must still disable segments")
	}
}
