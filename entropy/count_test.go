package entropy

import (
	"bytes"
	"sync/atomic"
	"testing"
)

// The counters must not change the output, and must end at the input length.
func TestEncodeCounting(t *testing.T) {
	for _, n := range []int{1, countStep - 1, countStep, 3*countStep + 17} {
		data := randomBytes(n, int64(n))
		var done atomic.Int64
		got := EncodeCounting(data, 7, &done)
		if !bytes.Equal(got, Encode(data, 7)) {
			t.Errorf("n=%d: EncodeCounting differs from Encode", n)
		}
		if done.Load() != int64(n) {
			t.Errorf("n=%d: counted %d bytes", n, done.Load())
		}
		var back atomic.Int64
		out, err := DecodeCounting(got, n, 7, &back)
		if err != nil || !bytes.Equal(out, data) {
			t.Errorf("n=%d: DecodeCounting did not restore the input: %v", n, err)
		}
		if back.Load() != int64(n) {
			t.Errorf("n=%d: decode counted %d bytes", n, back.Load())
		}
	}
}
