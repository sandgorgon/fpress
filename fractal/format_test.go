package fractal

import (
	"bytes"
	"testing"

	"fpress/matrix"
)

func sampleCompressed(t testing.TB) *Compressed {
	t.Helper()
	c, _, err := Encode(sierpinski(64), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Transforms) == 0 {
		t.Fatal("sample has no transforms")
	}
	return c
}

func TestMarshalRoundTrip(t *testing.T) {
	for name, m := range map[string]matrix.Matrix{
		"sierpinski":    sierpinski(64),
		"repeated-rows": repeatedRows(64, 48),
		"random":        matrix.FromBytes(randomBytes(900, 3), 30),
		"empty":         matrix.FromBytes(nil, 4),
	} {
		c, _, err := Encode(m, DefaultOptions())
		if err != nil {
			t.Fatal(err)
		}
		blob, err := c.MarshalBinary()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var back Compressed
		if err := back.UnmarshalBinary(blob); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, err := Decode(&back)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !matrix.Equal(got, m) {
			t.Fatalf("%s: decoded matrix differs after Marshal/Unmarshal", name)
		}
		again, _ := back.MarshalBinary()
		if !bytes.Equal(blob, again) {
			t.Fatalf("%s: Marshal is not stable across a round trip", name)
		}
	}
}

func TestUnmarshalRejectsDamage(t *testing.T) {
	blob, err := sampleCompressed(t).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	// Every truncation must be an error.
	for n := 0; n < len(blob); n++ {
		var c Compressed
		if err := c.UnmarshalBinary(blob[:n]); err == nil {
			t.Fatalf("truncation to %d/%d bytes was accepted", n, len(blob))
		}
	}
	// Trailing garbage must be an error.
	var c Compressed
	if err := c.UnmarshalBinary(append(append([]byte(nil), blob...), 0)); err == nil {
		t.Fatal("trailing byte accepted")
	}
}

func FuzzUnmarshal(f *testing.F) {
	blob, err := sampleCompressed(f).MarshalBinary()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(blob)
	f.Add([]byte{})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f})
	f.Fuzz(func(t *testing.T, data []byte) {
		var c Compressed
		if err := c.UnmarshalBinary(data); err != nil {
			return
		}
		// Anything that parses must be safe to hand to the decoder, provided
		// it does not ask for an enormous matrix.
		if c.Width*c.Height > 1<<20 {
			return
		}
		pw, ph := c.paddedDims()
		if pw*ph > 1<<22 || len(c.Transforms)*c.Iterations > 1<<22 {
			return
		}
		Decode(&c) // an error is fine; a panic is not
	})
}
