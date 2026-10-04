package matrix

import (
	"bytes"
	"math/rand"
	"testing"
)

func TestDenseBasics(t *testing.T) {
	d := NewDense(3, 2)
	d.Set(2, 1, 9)
	if d.At(2, 1) != 9 || d.At(0, 0) != 0 {
		t.Fatal("Set/At mismatch")
	}
	if got := d.Row(1); len(got) != 3 || got[2] != 9 {
		t.Fatalf("Row(1) = %v", got)
	}
	d.Fill(7)
	for _, b := range d.Pix() {
		if b != 7 {
			t.Fatal("Fill did not fill")
		}
	}
}

func TestFromBytesPadsLastRow(t *testing.T) {
	d := FromBytes([]byte{1, 2, 3, 4, 5}, 2)
	if d.Width() != 2 || d.Height() != 3 {
		t.Fatalf("dims %dx%d", d.Width(), d.Height())
	}
	want := []byte{1, 2, 3, 4, 5, 0}
	if !bytes.Equal(Bytes(d), want) {
		t.Fatalf("got %v want %v", Bytes(d), want)
	}
	if e := FromBytes(nil, 4); e.Width() != 4 || e.Height() != 0 {
		t.Fatalf("empty: %dx%d", e.Width(), e.Height())
	}
}

func TestFromBytesCopies(t *testing.T) {
	src := []byte{1, 2, 3, 4}
	d := FromBytes(src, 2)
	src[0] = 99
	if d.At(0, 0) != 1 {
		t.Fatal("FromBytes aliased its input")
	}
}

func TestViewCoordinatesAndNesting(t *testing.T) {
	d := NewDense(6, 6)
	for y := 0; y < 6; y++ {
		for x := 0; x < 6; x++ {
			d.Set(x, y, byte(y*6+x))
		}
	}
	v := NewView(d, 1, 2, 4, 3)
	if v.Width() != 4 || v.Height() != 3 || v.At(0, 0) != byte(2*6+1) || v.At(3, 2) != byte(4*6+4) {
		t.Fatal("view coordinates wrong")
	}
	vv := NewView(v, 1, 1, 2, 2)
	if vv.At(0, 0) != d.At(2, 3) || vv.At(1, 1) != d.At(3, 4) {
		t.Fatal("nested view coordinates wrong")
	}
	if !Equal(d.Sub(1, 2, 4, 3), v) {
		t.Fatal("Sub != NewView")
	}
}

func TestMutableViewWritesThrough(t *testing.T) {
	d := NewDense(4, 4)
	v := NewMutableView(d, 2, 1, 2, 2)
	v.Set(1, 1, 42)
	if d.At(3, 2) != 42 {
		t.Fatal("MutableView did not write through")
	}
	if v.At(1, 1) != 42 {
		t.Fatal("MutableView read mismatch")
	}
}

func mustPanic(t *testing.T, name string, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("%s: expected panic", name)
		}
	}()
	f()
}

func TestOutOfRangePanics(t *testing.T) {
	d := NewDense(3, 3)
	mustPanic(t, "At x", func() { d.At(3, 0) })
	mustPanic(t, "At y", func() { d.At(0, -1) })
	mustPanic(t, "Set", func() { d.Set(0, 3, 1) })
	mustPanic(t, "Row", func() { d.Row(3) })
	mustPanic(t, "view", func() { NewView(d, 2, 2, 2, 2) })
	mustPanic(t, "view neg", func() { NewView(d, -1, 0, 1, 1) })
	v := NewView(d, 0, 0, 2, 2)
	mustPanic(t, "view At", func() { v.At(2, 0) })
}

func TestFuncMatrix(t *testing.T) {
	f := Func{W: 4, H: 3, F: func(x, y int) byte { return byte(x*10 + y) }}
	if f.At(3, 2) != 32 {
		t.Fatal("Func.At wrong")
	}
	got := Bytes(f)
	if len(got) != 12 || got[0] != 0 || got[11] != 32 {
		t.Fatalf("Bytes(Func) = %v", got)
	}
}

func TestFileMatrix(t *testing.T) {
	data := make([]byte, 1000)
	rand.New(rand.NewSource(1)).Read(data)
	fm, err := NewFileMatrix(bytes.NewReader(data), int64(len(data)), 64)
	if err != nil {
		t.Fatal(err)
	}
	if fm.Width() != 64 || fm.Height() != 16 {
		t.Fatalf("dims %dx%d", fm.Width(), fm.Height())
	}
	if !Equal(fm, FromBytes(data, 64)) {
		t.Fatal("FileMatrix differs from Dense of same bytes")
	}
	if fm.Err() != nil {
		t.Fatal(fm.Err())
	}
	// Random access order exercises the row cache.
	d := FromBytes(data, 64)
	for i := 0; i < 500; i++ {
		x, y := rand.Intn(64), rand.Intn(16)
		if fm.At(x, y) != d.At(x, y) {
			t.Fatalf("mismatch at (%d,%d)", x, y)
		}
	}
	if _, err := NewFileMatrix(bytes.NewReader(nil), 0, 0); err == nil {
		t.Fatal("expected error for width 0")
	}
	empty, _ := NewFileMatrix(bytes.NewReader(nil), 0, 8)
	if empty.Height() != 0 {
		t.Fatal("empty file should have height 0")
	}
}

func TestCopyAndEqual(t *testing.T) {
	a := FromBytes([]byte{1, 2, 3, 4}, 2)
	b := NewDense(2, 2)
	Copy(b, a)
	if !Equal(a, b) {
		t.Fatal("Copy failed")
	}
	b.Set(1, 1, 0)
	if Equal(a, b) {
		t.Fatal("Equal missed a difference")
	}
	if Equal(a, NewDense(4, 1)) {
		t.Fatal("Equal ignored dimensions")
	}
	mustPanic(t, "Copy dims", func() { Copy(NewDense(1, 1), a) })
}
