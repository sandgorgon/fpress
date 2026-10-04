package fractal

import (
	"bytes"
	"math/rand"
	"testing"

	"fpress/matrix"
	"fpress/transform"
)

// applyComposed is the definition of a recipe in terms of the matrix
// operations: decimate the domain, orient it, map the values.
func applyComposed(next, cur *matrix.Dense, t *Transform) {
	s := t.Size
	if t.Raw {
		for y := 0; y < s; y++ {
			copy(next.Row(t.RY + y)[t.RX:t.RX+s], t.Data[y*s:(y+1)*s])
		}
		return
	}
	shrunk, oriented := matrix.NewDense(s, s), matrix.NewDense(s, s)
	if t.SameScale {
		matrix.Copy(shrunk, matrix.NewView(cur, t.DX, t.DY, s, s))
	} else {
		transform.Decimate2x(shrunk, matrix.NewView(cur, t.DX, t.DY, 2*s, 2*s))
	}
	transform.Apply(t.Iso, oriented, shrunk)
	t.Map.ApplyTo(matrix.NewMutableView(next, t.RX, t.RY, s, s), oriented)
}

func TestFusedApplyEqualsTheComposedOperations(t *testing.T) {
	rng := rand.New(rand.NewSource(31))
	const pw, ph = 70, 60
	cur := matrix.NewDense(pw, ph)
	rng.Read(cur.Pix())
	for trial := 0; trial < 400; trial++ {
		s := []int{1, 2, 3, 4, 8, 16}[rng.Intn(6)]
		if 2*s > pw || 2*s > ph {
			continue
		}
		tr := Transform{
			Size: s, RX: rng.Intn(pw - s + 1), RY: rng.Intn(ph - s + 1),
			DX: rng.Intn(pw - 2*s + 1), DY: rng.Intn(ph - 2*s + 1),
			Iso:       transform.Isometry(rng.Intn(8)),
			SameScale: rng.Intn(3) == 0,
		}
		if tr.SameScale { // the domain is only s wide, so it may sit further right and down
			tr.DX, tr.DY = rng.Intn(pw-s+1), rng.Intn(ph-s+1)
		}
		switch rng.Intn(3) {
		case 1:
			tr.Map = transform.ValueMap{Kind: transform.MapAdd, C: byte(rng.Intn(256))}
		case 2:
			tr.Map = transform.ValueMap{Kind: transform.MapXor, C: byte(rng.Intn(256))}
		}
		if rng.Intn(5) == 0 {
			tr.Raw, tr.Data = true, make([]byte, s*s)
			rng.Read(tr.Data)
		}
		want, got := matrix.NewDense(pw, ph), matrix.NewDense(pw, ph)
		applyComposed(want, cur, &tr)
		scale := 2
		if tr.SameScale {
			scale = 1
		}
		applyFused(got, cur, &tr, newBlockGeom(s, scale))
		if !bytes.Equal(want.Pix(), got.Pix()) {
			t.Fatalf("trial %d: fused and composed differ for %+v", trial, tr)
		}
	}
}

func TestRangeBlocksMayNotOverlap(t *testing.T) {
	const pw, ph = 32, 32
	ok := []Transform{{RX: 0, RY: 0, Size: 8, Raw: true, Data: make([]byte, 64)}, {RX: 8, RY: 0, Size: 8, Raw: true, Data: make([]byte, 64)}, {RX: 0, RY: 8, Size: 4, Raw: true, Data: make([]byte, 16)}}
	if err := checkDisjoint(ok, pw, ph); err != nil {
		t.Fatal(err)
	}
	// Overlaps of every shape: same block, partial, corner-touching cell, containment, a row shared across a word boundary.
	for name, extra := range map[string]Transform{
		"same":     {RX: 0, RY: 0, Size: 8, Raw: true, Data: make([]byte, 64)},
		"partial":  {RX: 4, RY: 4, Size: 8, Raw: true, Data: make([]byte, 64)},
		"one cell": {RX: 7, RY: 7, Size: 2, Raw: true, Data: make([]byte, 4)},
		"inside":   {RX: 2, RY: 2, Size: 2, Raw: true, Data: make([]byte, 4)},
		"wide row": {RX: 30, RY: 0, Size: 2, Raw: true, Data: make([]byte, 4)}, // x 30..31 vs nothing: must be fine below
	} {
		list := append(append([]Transform(nil), ok...), extra)
		err := checkDisjoint(list, pw, ph)
		if name == "wide row" {
			if err != nil {
				t.Errorf("%s: unexpected overlap error: %v", name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: overlap not detected", name)
		}
	}
	// Blocks that touch edge to edge are not overlaps, including across the
	// 64-bit word boundaries of the bitmap.
	var edge []Transform
	for x := 0; x < 130; x += 5 {
		edge = append(edge, Transform{RX: x, RY: 3, Size: 5, Raw: true, Data: make([]byte, 25)})
	}
	if err := checkDisjoint(edge, 135, 10); err != nil {
		t.Errorf("adjacent blocks reported as overlapping: %v", err)
	}
}

func TestValidateRejectsOverlap(t *testing.T) {
	c, _, err := Encode(sierpinski(64), DefaultOptions())
	if err != nil || len(c.Transforms) == 0 {
		t.Fatal(err)
	}
	dup := *c
	dup.Transforms = append(append([]Transform(nil), c.Transforms...), c.Transforms[0])
	if _, err := Decode(&dup); err == nil {
		t.Fatal("a stream with overlapping range blocks was accepted")
	}
}

// Decoding must not depend on the number of goroutines.
func TestRebuildIsIndependentOfWorkers(t *testing.T) {
	for _, m := range []matrix.Matrix{sierpinski(256), repeatedRows(300, 200)} {
		opt := DefaultOptions()
		opt.Workers = 1
		c, _, err := Encode(m, opt)
		if err != nil {
			t.Fatal(err)
		}
		ref := reconstruct(c, 1)
		for _, w := range []int{2, 3, 8, 64} {
			if got := reconstruct(c, w); !bytes.Equal(got.Pix(), ref.Pix()) {
				t.Fatalf("rebuild with %d workers differs from 1", w)
			}
		}
	}
}
