package entropy

import (
	"bytes"
	"compress/flate"
	"math"
	"math/rand"
	"os"
	"testing"
)

func flateSize(b []byte) int {
	var buf bytes.Buffer
	fw, _ := flate.NewWriter(&buf, flate.BestCompression)
	fw.Write(b)
	fw.Close()
	return buf.Len()
}

func randomBytes(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func gradient(w, h int) []byte {
	b := make([]byte, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			b[y*w+x] = byte(x*3 + y)
		}
	}
	return b
}

func sierpinski(n int) []byte {
	b := make([]byte, n*n)
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if x&y == 0 {
				b[y*n+x] = 255
			}
		}
	}
	return b
}

func inputs() map[string][]byte {
	text := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog. pack my box with five dozen liquor jugs. "), 30)
	return map[string][]byte{
		"empty":    {},
		"one":      {42},
		"two":      {1, 2},
		"zeros":    make([]byte, 5000),
		"ones":     bytes.Repeat([]byte{255}, 3000),
		"random":   randomBytes(4000, 1),
		"text":     text,
		"gradient": gradient(100, 50),
		"sierp":    sierpinski(64),
		"alt":      bytes.Repeat([]byte{0, 255}, 1500),
		"runs":     append(bytes.Repeat([]byte{7}, 700), bytes.Repeat([]byte{9}, 700)...),
	}
}

func TestTablesAreSane(t *testing.T) {
	tablesOnce.Do(initTables)
	// squash is monotonic, symmetric, and matches the real logistic closely.
	for x := -stMax; x < stMax; x++ {
		if squash(x) > squash(x+1) {
			t.Fatalf("squash not monotonic at %d", x)
		}
	}
	for x := 0; x <= stMax; x++ {
		if got := squash(x) + squash(-x); got < 65534 || got > 65538 {
			t.Fatalf("squash(%d)+squash(-%d) = %d, want ~65536", x, x, got)
		}
	}
	for _, x := range []int{-3000, -1000, -256, -1, 0, 1, 256, 1000, 3000} {
		want := 65536 / (1 + math.Exp(-float64(x)/256))
		if got := float64(squash(x)); math.Abs(got-want) > 0.01*want+2 {
			t.Errorf("squash(%d) = %v, want about %v", x, got, want)
		}
	}
	// stretch inverts squash.
	for x := -2000; x <= 2000; x += 13 {
		tol := 3
		if x > 1500 || x < -1500 { // the 16-bit probabilities are coarse in the tails
			tol = 8
		}
		if got := stretch(squash(x)); got < x-tol || got > x+tol {
			t.Errorf("stretch(squash(%d)) = %d", x, got)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	for name, data := range inputs() {
		for _, stride := range []int{0, 1, 7, 50, 64, len(data) + 5} {
			enc := Encode(data, stride)
			got, err := Decode(enc, len(data), stride)
			if err != nil {
				t.Fatalf("%s stride %d: %v", name, stride, err)
			}
			if !bytes.Equal(got, data) {
				t.Fatalf("%s stride %d: round trip mismatch", name, stride)
			}
		}
	}
}

func TestEncodingIsDeterministic(t *testing.T) {
	data := append(gradient(100, 20), randomBytes(500, 3)...)
	a, b := Encode(data, 100), Encode(data, 100)
	if !bytes.Equal(a, b) {
		t.Fatal("two encodes differ")
	}
	// Golden checksum: guards against accidental changes to the model, which
	// would silently make old streams undecodable.
	sum := uint32(2166136261)
	for _, c := range a {
		sum = (sum ^ uint32(c)) * 16777619
	}
	t.Logf("golden: len=%d fnv=%#x", len(a), sum)
	if len(a) != goldenLen || sum != goldenSum {
		t.Errorf("encoding changed: len=%d fnv=%#x, want len=%d fnv=%#x (update the golden values only if the format change is intended)",
			len(a), sum, goldenLen, goldenSum)
	}
}

func TestDecodeIsStrict(t *testing.T) {
	data := bytes.Repeat([]byte("strict streams only. "), 40)
	enc := Encode(data, 0)
	for n := 0; n < len(enc); n++ {
		if got, err := Decode(enc[:n], len(data), 0); err == nil {
			t.Fatalf("truncation to %d/%d bytes accepted (%d bytes)", n, len(enc), len(got))
		}
	}
	if _, err := Decode(append(append([]byte(nil), enc...), 0), len(data), 0); err == nil {
		t.Error("trailing byte accepted")
	}
	if _, err := Decode(enc, len(data)+1, 0); err == nil {
		t.Error("longer claimed length accepted")
	}
	if _, err := Decode(nil, 0, 0); err != nil {
		t.Errorf("empty stream: %v", err)
	}
	if _, err := Decode([]byte{1}, 0, 0); err == nil {
		t.Error("bytes for an empty stream accepted")
	}
	if _, err := Decode(nil, 5, 0); err == nil {
		t.Error("missing data accepted")
	}
}

// Wrong stride, damaged bytes: never a panic, and never silently the same data.
func TestDecodeDamage(t *testing.T) {
	data := gradient(40, 30)
	enc := Encode(data, 40)
	if got, err := Decode(enc, len(data), 41); err == nil && bytes.Equal(got, data) {
		t.Error("decoding with the wrong stride still gave the right data")
	}
	for i := range enc {
		bad := append([]byte(nil), enc...)
		bad[i] ^= 0x10
		Decode(bad, len(data), 40) // may error or decode to something else; must not panic
	}
}

func FuzzRoundTrip(f *testing.F) {
	f.Add([]byte("hello hello hello"), uint8(0))
	f.Add(gradient(16, 8), uint8(16))
	f.Fuzz(func(t *testing.T, data []byte, stride uint8) {
		if len(data) > 4096 {
			t.Skip()
		}
		got, err := Decode(Encode(data, int(stride)), len(data), int(stride))
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("round trip failed: %v", err)
		}
	})
}

func FuzzDecode(f *testing.F) {
	f.Add(Encode([]byte("seed data seed data"), 0), uint16(19), uint8(0))
	f.Fuzz(func(t *testing.T, blob []byte, n uint16, stride uint8) {
		Decode(blob, int(n)%5000, int(stride)) // must not panic or hang
	})
}

// Compression quality against DEFLATE: a log and a loose guard, not a benchmark.
func TestBeatsDeflate(t *testing.T) {
	cases := map[string]struct {
		data   []byte
		stride int
		must   bool // must beat DEFLATE
	}{
		"text":     {inputs()["text"], 0, true},
		"gradient": {gradient(100, 50), 100, true},
		"sierp":    {sierpinski(128), 128, true},
		"zeros":    {make([]byte, 20000), 0, false},
		"random":   {randomBytes(20000, 4), 0, false},
	}
	for name, c := range cases {
		cm, fl := len(Encode(c.data, c.stride)), flateSize(c.data)
		t.Logf("%-9s %6d bytes: cm=%6d flate=%6d", name, len(c.data), cm, fl)
		if c.must && cm >= fl {
			t.Errorf("%s: cm (%d) did not beat flate (%d)", name, cm, fl)
		}
		if name == "random" && cm > len(c.data)+len(c.data)/50 {
			t.Errorf("random data expanded by more than 2%%: %d -> %d", len(c.data), cm)
		}
	}
	if b, err := os.ReadFile("/bin/ls"); err == nil && len(b) > 60000 {
		cm, fl := len(Encode(b[:60000], 0)), flateSize(b[:60000])
		t.Logf("/bin/ls[60K]: cm=%d flate=%d", cm, fl)
		if cm >= fl {
			t.Errorf("/bin/ls: cm (%d) did not beat flate (%d)", cm, fl)
		}
	}
}

func BenchmarkEncode(b *testing.B) {
	data := inputs()["text"]
	b.SetBytes(int64(len(data)))
	for i := 0; i < b.N; i++ {
		Encode(data, 0)
	}
}
