package transform

import (
	"bytes"
	"testing"

	"fpress/matrix"
)

// distinct returns an n x n matrix whose cells are all different, so any two
// different position permutations give different results.
func distinct(n int) *matrix.Dense {
	d := matrix.NewDense(n, n)
	for i := range d.Pix() {
		d.Pix()[i] = byte(i)
	}
	return d
}

func TestIsometriesAreInvertible(t *testing.T) {
	for _, n := range []int{1, 2, 3, 4, 7, 8} {
		src := distinct(n)
		for i := Isometry(0); i < NumIsometries; i++ {
			mid, back := matrix.NewDense(n, n), matrix.NewDense(n, n)
			Apply(i, mid, src)
			Apply(i.Inverse(), back, mid)
			if !matrix.Equal(src, back) {
				t.Errorf("n=%d iso=%d: inverse did not restore the matrix", n, i)
			}
		}
	}
}

func TestIsometriesAreDistinctAndPermute(t *testing.T) {
	src := distinct(4)
	seen := map[string]Isometry{}
	for i := Isometry(0); i < NumIsometries; i++ {
		out := matrix.NewDense(4, 4)
		Apply(i, out, src)
		key := string(matrix.Bytes(out))
		if prev, dup := seen[key]; dup {
			t.Errorf("isometries %d and %d give the same result", prev, i)
		}
		seen[key] = i
		// Pure permutation: same multiset of bytes.
		var count [256]int
		for _, b := range matrix.Bytes(out) {
			count[b]++
		}
		for v := 0; v < 16; v++ {
			if count[v] != 1 {
				t.Errorf("iso %d is not a permutation (value %d seen %d times)", i, v, count[v])
			}
		}
	}
	if len(seen) != NumIsometries {
		t.Fatalf("got %d distinct results, want %d", len(seen), NumIsometries)
	}
}

func TestKnownIsometries(t *testing.T) {
	src := matrix.FromBytes([]byte{1, 2, 3, 4}, 2) // 1 2 / 3 4
	cases := map[Isometry][]byte{
		IsoIdentity:      {1, 2, 3, 4},
		IsoRot90:         {3, 1, 4, 2},
		IsoRot180:        {4, 3, 2, 1},
		IsoRot270:        {2, 4, 1, 3},
		IsoFlipH:         {2, 1, 4, 3},
		IsoFlipV:         {3, 4, 1, 2},
		IsoTranspose:     {1, 3, 2, 4},
		IsoAntiTranspose: {4, 2, 3, 1},
	}
	for iso, want := range cases {
		out := matrix.NewDense(2, 2)
		Apply(iso, out, src)
		if !bytes.Equal(matrix.Bytes(out), want) {
			t.Errorf("iso %d: got %v want %v", iso, matrix.Bytes(out), want)
		}
	}
}

func TestDecimate2x(t *testing.T) {
	src := matrix.FromBytes([]byte{
		10, 99, 20, 99,
		99, 99, 99, 99,
		30, 99, 40, 99,
		99, 99, 99, 99,
	}, 4)
	dst := matrix.NewDense(2, 2)
	Decimate2x(dst, src)
	if want := []byte{10, 20, 30, 40}; !bytes.Equal(matrix.Bytes(dst), want) {
		t.Fatalf("got %v want %v", matrix.Bytes(dst), want)
	}
}

func TestDecimateWorksOnViews(t *testing.T) {
	big := distinct(8)
	dst := matrix.NewDense(2, 2)
	Decimate2x(dst, matrix.NewView(big, 2, 4, 4, 4))
	if dst.At(0, 0) != big.At(2, 4) || dst.At(1, 1) != big.At(4, 6) {
		t.Fatal("Decimate2x on a View read the wrong cells")
	}
}

func TestValueMaps(t *testing.T) {
	if (ValueMap{MapAdd, 5}).Apply(253) != 2 {
		t.Error("add should wrap mod 256")
	}
	if (ValueMap{MapXor, 0xFF}).Apply(0x0F) != 0xF0 {
		t.Error("xor wrong")
	}
	if (ValueMap{}).Apply(77) != 77 {
		t.Error("identity wrong")
	}
	if (ValueMap{MapIdentity, 3}).Valid() || !(ValueMap{MapAdd, 3}).Valid() || (ValueMap{MapKind(9), 0}).Valid() {
		t.Error("Valid wrong")
	}
	// Every map is a bijection on bytes.
	for _, m := range []ValueMap{{MapAdd, 200}, {MapXor, 77}} {
		var seen [256]bool
		for v := 0; v < 256; v++ {
			seen[m.Apply(byte(v))] = true
		}
		for v, ok := range seen {
			if !ok {
				t.Fatalf("%+v is not a bijection (missing %d)", m, v)
			}
		}
	}
}

// The worked example from the design discussion: shrink, flip, add 5, then
// correct the one wrong byte with an XOR residual.
func TestWorkedExample(t *testing.T) {
	source := matrix.FromBytes([]byte{
		10, 99, 20, 99,
		99, 99, 99, 99,
		30, 99, 40, 99,
		99, 99, 99, 99,
	}, 4)
	shrunk := matrix.NewDense(2, 2)
	Decimate2x(shrunk, source)
	flipped := matrix.NewDense(2, 2)
	Apply(IsoFlipH, flipped, shrunk)
	made := matrix.NewDense(2, 2)
	ValueMap{MapAdd, 5}.ApplyTo(made, flipped)
	if want := []byte{25, 15, 45, 35}; !bytes.Equal(matrix.Bytes(made), want) {
		t.Fatalf("recipe result %v, want %v", matrix.Bytes(made), want)
	}
	target := []byte{25, 15, 45, 36}
	var residual []byte
	for i, b := range matrix.Bytes(made) {
		residual = append(residual, b^target[i])
	}
	if want := []byte{0, 0, 0, 7}; !bytes.Equal(residual, want) {
		t.Fatalf("residual %v, want %v", residual, want)
	}
}
