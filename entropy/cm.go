package entropy

import (
	"fmt"
	"sync/atomic"
)

// Encode compresses data. stride is the row length when the bytes form a
// matrix (0 for a plain byte stream); it adds contexts built from the bytes
// directly above, which helps when rows resemble each other. The decoder must
// be given the same stride.
func Encode(data []byte, stride int) []byte {
	if len(data) == 0 {
		return nil
	}
	return EncodeCounting(data, stride, nil)
}

// countStep is how many bytes EncodeCounting codes between updates of its
// counter.
const countStep = 1 << 16

// EncodeCounting is Encode that also adds to done, as it goes, the number of
// bytes coded so far (every 64 KiB, and the rest at the end), so another
// goroutine can follow a long encode. done may be nil. The output is the same
// as Encode's.
func EncodeCounting(data []byte, stride int, done *atomic.Int64) []byte {
	if len(data) == 0 {
		return nil
	}
	m := acquireModel(data, len(data), max(stride, 0))
	defer m.release()
	return encodeWith(m, data, done)
}

func encodeWith(m *model, data []byte, done *atomic.Int64) []byte {
	e := newEncoder()
	e.out = make([]byte, 0, len(data)/2+16)
	for k, b := range data {
		for i := 7; i >= 0; i-- {
			bit := int(b>>uint(i)) & 1
			e.encode(bit, m.predict())
			m.update(bit)
		}
		if done != nil && (k+1)%countStep == 0 {
			done.Add(countStep)
		}
	}
	if done != nil {
		done.Add(int64(len(data) % countStep))
	}
	return e.finish()
}

// Decode reverses Encode, producing exactly n bytes. The stream must be used
// up exactly: truncated or over-long input is an error.
func Decode(blob []byte, n, stride int) ([]byte, error) {
	if n == 0 {
		if len(blob) != 0 {
			return nil, fmt.Errorf("%w: data for an empty stream", errStream)
		}
		return nil, nil
	}
	if n < 0 || len(blob) < 4 {
		return nil, fmt.Errorf("%w: too short", errStream)
	}
	out := make([]byte, n)
	m := acquireModel(out, n, max(stride, 0))
	defer m.release()
	return decodeWith(m, blob, out)
}

func decodeWith(m *model, blob, out []byte) ([]byte, error) {
	n := len(out)
	d := newDecoder(blob)
	for p := 0; p < n; p++ {
		c := 0
		for i := 0; i < 8; i++ {
			bit := d.decode(m.predict())
			c = c<<1 | bit
			if i == 7 {
				out[p] = byte(c) // the model reads this when it advances
			}
			m.update(bit)
		}
		if d.overrun {
			return nil, fmt.Errorf("%w: truncated", errStream)
		}
	}
	if d.pos != len(blob) {
		return nil, fmt.Errorf("%w: %d unused bytes", errStream, len(blob)-d.pos)
	}
	return out, nil
}
