package entropy

import (
	"bytes"
	"math/rand"
	"testing"
)

// lookAlike builds a "frame" out of a few small shapes drawn at random, so that
// short contexts are everywhere and only a long context says where you are.
func lookAlike(chunks int, seed int64) []byte {
	rng := rand.New(rand.NewSource(seed))
	var shapes [10][24]byte
	for i := range shapes {
		rng.Read(shapes[i][:])
	}
	var f []byte
	for i := 0; i < chunks; i++ {
		f = append(f, shapes[rng.Intn(len(shapes))][:]...)
	}
	return f
}

// A repeat of the whole frame far back must cost almost nothing, even though
// every short context in it also occurs, many times, nearer by.
func TestLongMatchFindsFarRepeatAmongLookAlikes(t *testing.T) {
	frame := lookAlike(3000, 1) // 72,000 bytes
	one := len(Encode(frame, 0))
	var data []byte
	for i := 0; i < 6; i++ {
		data = append(data, frame...)
	}
	blob := Encode(data, 0)
	t.Logf("one frame %d B, six copies %d B", one, len(blob))
	if len(blob) > one+one/5 {
		t.Errorf("five repeats of the frame added %d bytes (one frame is %d): the far repeat was not found", len(blob)-one, one)
	}
	got, err := Decode(blob, len(data), 0)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("round trip failed: %v", err)
	}
}

// The rolling hash must agree with hashing the window from scratch: a repeat
// that starts exactly longMatch bytes in is found, one byte earlier is not.
func TestRollingHashMatchesWindow(t *testing.T) {
	data := lookAlike(400, 2)
	m := newModel(data, len(data), 0)
	for m.pos < len(data)-1 {
		b := data[m.pos]
		for i := 7; i >= 0; i-- {
			m.predict()
			m.update(int(b>>uint(i)) & 1)
		}
		if m.pos >= longMatch {
			var want uint32
			for _, c := range data[m.pos-longMatch : m.pos] {
				want = want*longMul + uint32(c) + 1
			}
			if m.lroll != want {
				t.Fatalf("at %d the rolling hash is %#x, recomputed %#x", m.pos, m.lroll, want)
			}
		}
	}
}
