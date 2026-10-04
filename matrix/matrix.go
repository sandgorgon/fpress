// Package matrix defines the 2D matrix-of-bytes abstraction that the rest of
// fpress operates on, plus a few concrete implementations.
//
// Coordinates are (x, y) with x the column in [0, Width()) and y the row in
// [0, Height()). Out-of-range access is a programmer error and panics.
package matrix

import "fmt"

// Matrix is a read-only 2D matrix of bytes.
type Matrix interface {
	Width() int
	Height() int
	At(x, y int) byte
}

// MutableMatrix is a Matrix whose cells can be written.
type MutableMatrix interface {
	Matrix
	Set(x, y int, v byte)
}

// RowAccessor is an optional fast path: Row returns row y as a slice of
// length Width(). Callers must not assume the slice is a copy.
type RowAccessor interface {
	Row(y int) []byte
}

// SubMatrix is an optional fast path for taking a zero-copy window.
type SubMatrix interface {
	Sub(x, y, w, h int) Matrix
}

// Bytes returns the matrix contents flattened row-major into a new slice.
func Bytes(m Matrix) []byte {
	w, h := m.Width(), m.Height()
	out := make([]byte, 0, w*h)
	ra, fast := m.(RowAccessor)
	for y := 0; y < h; y++ {
		if fast {
			out = append(out, ra.Row(y)...)
			continue
		}
		for x := 0; x < w; x++ {
			out = append(out, m.At(x, y))
		}
	}
	return out
}

// Equal reports whether a and b have the same dimensions and contents.
func Equal(a, b Matrix) bool {
	if a.Width() != b.Width() || a.Height() != b.Height() {
		return false
	}
	for y := 0; y < a.Height(); y++ {
		for x := 0; x < a.Width(); x++ {
			if a.At(x, y) != b.At(x, y) {
				return false
			}
		}
	}
	return true
}

// Copy copies src into dst. The dimensions must match.
func Copy(dst MutableMatrix, src Matrix) {
	if dst.Width() != src.Width() || dst.Height() != src.Height() {
		panic(fmt.Sprintf("matrix: Copy dimension mismatch: dst %dx%d, src %dx%d",
			dst.Width(), dst.Height(), src.Width(), src.Height()))
	}
	for y := 0; y < src.Height(); y++ {
		for x := 0; x < src.Width(); x++ {
			dst.Set(x, y, src.At(x, y))
		}
	}
}

func checkCell(w, h, x, y int) {
	if x < 0 || x >= w || y < 0 || y >= h {
		panic(fmt.Sprintf("matrix: cell (%d,%d) out of range for %dx%d matrix", x, y, w, h))
	}
}
