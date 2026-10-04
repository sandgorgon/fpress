package entropy

import (
	"bytes"
	"math/rand"
	"testing"
)

func TestChunkedRoundTrip(t *testing.T) {
	text := bytes.Repeat([]byte("chunked streams decode in parallel. "), 700)
	cases := map[string]struct {
		data   []byte
		stride int
	}{
		"empty":    {nil, 0},
		"one":      {[]byte{9}, 0},
		"text":     {text, 0},
		"rows":     {gradient(100, 300), 100},
		"sierp":    {sierpinski(256), 256},
		"random":   {randomBytes(70000, 5), 0},
		"odd rows": {gradient(37, 211), 37},
	}
	for name, c := range cases {
		for _, chunk := range []int{0, 1, 100, 4096, 20000, 1 << 20} {
			for _, workers := range []int{1, 4} {
				enc := EncodeChunked(c.data, c.stride, chunk, workers)
				got, err := DecodeChunked(enc, len(c.data), c.stride, workers)
				if err != nil || !bytes.Equal(got, c.data) {
					t.Fatalf("%s chunk %d workers %d: %v", name, chunk, workers, err)
				}
			}
		}
	}
}

func TestChunkedOutputIsIndependentOfWorkers(t *testing.T) {
	data := append(gradient(100, 400), randomBytes(30000, 6)...)
	ref := EncodeChunked(data, 100, 8000, 1)
	for _, w := range []int{2, 3, 8, 64} {
		if !bytes.Equal(ref, EncodeChunked(data, 100, 8000, w)) {
			t.Fatalf("%d workers gave different bytes", w)
		}
	}
}

func TestChunksAlignToRows(t *testing.T) {
	if got := effectiveChunk(1000, 100, 250); got != 200 {
		t.Errorf("chunk 250 at stride 100 = %d, want 200 (whole rows)", got)
	}
	if got := effectiveChunk(1000, 100, 30); got != 100 {
		t.Errorf("a chunk smaller than a row = %d, want one row", got)
	}
	if got := effectiveChunk(50, 0, 1<<20); got != 50 {
		t.Errorf("short data = %d, want one chunk of 50", got)
	}
}

func TestChunkedCostsLittle(t *testing.T) {
	// Varied text: words drawn from a vocabulary, so there is real entropy to code.
	rng := rand.New(rand.NewSource(3))
	vocab := make([]string, 400)
	for i := range vocab {
		w := make([]byte, 2+rng.Intn(8))
		for j := range w {
			w[j] = byte('a' + rng.Intn(26))
		}
		vocab[i] = string(w)
	}
	var data []byte
	for len(data) < 1<<20 {
		data = append(data, vocab[rng.Intn(len(vocab))]...)
		data = append(data, ' ')
	}
	one := len(Encode(data, 0))
	small := len(EncodeChunked(data, 0, 1<<16, 4))
	def := len(EncodeChunked(data, 0, DefaultChunk, 4))
	t.Logf("%d bytes: one stream %d | 64 KiB chunks %d (+%.1f%%) | default %d KiB chunks %d (+%.1f%%)",
		len(data), one, small, 100*(float64(small)/float64(one)-1), DefaultChunk>>10, def, 100*(float64(def)/float64(one)-1))
	// Every chunk starts with an empty model and must re-learn the vocabulary,
	// so smaller chunks cost more; the default is chosen to keep this modest.
	if float64(def) > 1.15*float64(one) {
		t.Errorf("default chunks cost more than 15%%: %d vs %d", def, one)
	}
	if small < def {
		t.Errorf("smaller chunks should not compress better than larger ones (%d vs %d)", small, def)
	}
}

func TestChunkedRejectsMalformed(t *testing.T) {
	data := gradient(50, 200)
	enc := EncodeChunked(data, 50, 2000, 2)
	if _, err := DecodeChunked(enc, len(data), 50, 2); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < len(enc); n++ {
		if _, err := DecodeChunked(enc[:n], len(data), 50, 2); err == nil {
			t.Fatalf("truncation to %d/%d accepted", n, len(enc))
		}
	}
	if _, err := DecodeChunked(append(append([]byte(nil), enc...), 0), len(data), 50, 2); err == nil {
		t.Error("trailing byte accepted")
	}
	if _, err := DecodeChunked(enc, len(data)+1, 50, 2); err == nil {
		t.Error("wrong length accepted")
	}
	if _, err := DecodeChunked(enc, len(data), 51, 2); err == nil {
		t.Error("wrong stride accepted")
	}
	for name, b := range map[string][]byte{
		"empty": nil, "zero chunk, garbage stream": {0, 1, 2}, "huge chunk": {0xff, 0xff, 0x7f, 1},
		"count > bytes": {1, 1}, "length lies": {50, 0xff, 0x7f},
	} {
		if _, err := DecodeChunked(b, 100, 0, 2); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	// damaged bytes must not panic
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 200; i++ {
		bad := append([]byte(nil), enc...)
		bad[rng.Intn(len(bad))] ^= byte(1 + rng.Intn(255))
		DecodeChunked(bad, len(data), 50, 2)
	}
}

func FuzzDecodeChunked(f *testing.F) {
	f.Add(EncodeChunked(gradient(20, 40), 20, 200, 1), uint16(800), uint8(20))
	f.Add([]byte{}, uint16(0), uint8(0))
	f.Fuzz(func(t *testing.T, blob []byte, n uint16, stride uint8) {
		DecodeChunked(blob, int(n)%4000, int(stride), 2) // an error is fine; a panic or hang is not
	})
}

func TestSingleChunkIsOneByteOfOverhead(t *testing.T) {
	data := bytes.Repeat([]byte("small blobs stay small. "), 100)
	plain, chunked := len(Encode(data, 0)), len(EncodeChunked(data, 0, DefaultChunk, 4))
	if chunked != plain+1 {
		t.Errorf("one chunk costs %d bytes over a plain stream, want 1", chunked-plain)
	}
	// And the multi-chunk form still round trips right at the boundary.
	for _, n := range []int{99, 100, 101, 199, 200, 201} {
		d := randomBytes(n, int64(n))
		got, err := DecodeChunked(EncodeChunked(d, 0, 100, 2), n, 0, 2)
		if err != nil || !bytes.Equal(got, d) {
			t.Fatalf("n=%d: %v", n, err)
		}
	}
}
