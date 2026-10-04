package entropy

import (
	"bytes"
	"math/rand"
	"sync"
	"testing"
)

// poolInputs mixes sizes (so different table sizes and final-stage sizes),
// strides (so models are reused across different context counts) and content.
func poolInputs() []struct {
	data   []byte
	stride int
} {
	rng := rand.New(rand.NewSource(77))
	var out []struct {
		data   []byte
		stride int
	}
	add := func(d []byte, s int) {
		out = append(out, struct {
			data   []byte
			stride int
		}{d, s})
	}
	text := bytes.Repeat([]byte("pooled models must not leak state. "), 120)
	add(text, 0)
	add(text, 35)
	add(gradient(100, 60), 100)
	add(gradient(100, 60), 0)
	add(sierpinski(64), 64)
	add(randomBytes(5000, 9), 0)
	add(randomBytes(5000, 9), 50)
	add(randomBytes(300, 4), 0)
	add(text[:1000], 0)
	big := make([]byte, 40000)
	for i := range big {
		big[i] = byte(rng.Intn(6)) + byte(i/500)
	}
	add(big, 200)
	add(big, 0)
	add(big[:9000], 90)
	return out
}

func TestPooledModelsMatchFreshOnes(t *testing.T) {
	ins := poolInputs()
	fresh := make([][]byte, len(ins))
	for i, in := range ins {
		fresh[i] = encodeWith(newModel(in.data, len(in.data), in.stride), in.data)
	}
	// Several rounds in shuffled order, so a pooled model has always been used
	// for something different before.
	rng := rand.New(rand.NewSource(3))
	for round := 0; round < 6; round++ {
		for _, i := range rng.Perm(len(ins)) {
			in := ins[i]
			got := Encode(in.data, in.stride)
			if !bytes.Equal(got, fresh[i]) {
				t.Fatalf("round %d, input %d (len %d, stride %d): pooled encoding differs from a fresh model's", round, i, len(in.data), in.stride)
			}
			back, err := Decode(got, len(in.data), in.stride)
			if err != nil || !bytes.Equal(back, in.data) {
				t.Fatalf("round %d, input %d: decode failed: %v", round, i, err)
			}
		}
	}
}

func TestConcurrentUseIsDeterministic(t *testing.T) {
	ins := poolInputs()
	want := make([][]byte, len(ins))
	for i, in := range ins {
		want[i] = Encode(in.data, in.stride)
	}
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			for k := 0; k < 12; k++ {
				i := rng.Intn(len(ins))
				got := Encode(ins[i].data, ins[i].stride)
				if !bytes.Equal(got, want[i]) {
					errs <- "concurrent encoding differs from the serial one"
					return
				}
				back, err := Decode(got, len(ins[i].data), ins[i].stride)
				if err != nil || !bytes.Equal(back, ins[i].data) {
					errs <- "concurrent decode failed"
					return
				}
			}
		}(int64(g))
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// A larger golden than TestEncodingIsDeterministic's, big enough that the
// final-stage table has its full size (inputs under 4 KiB use fewer contexts).
func TestEncodingIsDeterministicLarge(t *testing.T) {
	data := make([]byte, 30000)
	rng := rand.New(rand.NewSource(12))
	for i := range data {
		data[i] = byte((i%97)*3) ^ byte(rng.Intn(5))
	}
	enc := Encode(data, 97)
	sum := uint32(2166136261)
	for _, c := range enc {
		sum = (sum ^ uint32(c)) * 16777619
	}
	t.Logf("golden large: len=%d fnv=%#x", len(enc), sum)
	if len(enc) != goldenLargeLen || sum != goldenLargeSum {
		t.Errorf("encoding changed: len=%d fnv=%#x, want len=%d fnv=%#x", len(enc), sum, goldenLargeLen, goldenLargeSum)
	}
}
