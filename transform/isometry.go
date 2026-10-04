// Package transform holds the byte-exact matrix transformations used by the
// fractal stage. Everything here moves or relabels bytes: nothing averages,
// scales or rounds, because the bytes are opaque symbols, not quantities.
package transform

import (
	"fmt"

	"fpress/matrix"
)

// Isometry is one of the 8 symmetries of a square (the dihedral group D4).
// It only permutes cell positions; values are untouched.
type Isometry uint8

const (
	IsoIdentity Isometry = iota
	IsoRot90
	IsoRot180
	IsoRot270
	IsoFlipH // mirror left-right
	IsoFlipV // mirror top-bottom
	IsoTranspose
	IsoAntiTranspose

	NumIsometries = 8
)

// Source reports which cell of an n x n source feeds destination cell (x, y)
// when the isometry is applied.
func (i Isometry) Source(n, x, y int) (sx, sy int) {
	switch i {
	case IsoIdentity:
		return x, y
	case IsoRot90:
		return y, n - 1 - x
	case IsoRot180:
		return n - 1 - x, n - 1 - y
	case IsoRot270:
		return n - 1 - y, x
	case IsoFlipH:
		return n - 1 - x, y
	case IsoFlipV:
		return x, n - 1 - y
	case IsoTranspose:
		return y, x
	case IsoAntiTranspose:
		return n - 1 - y, n - 1 - x
	}
	panic(fmt.Sprintf("transform: invalid isometry %d", i))
}

// Inverse returns the isometry that undoes i.
func (i Isometry) Inverse() Isometry {
	switch i {
	case IsoRot90:
		return IsoRot270
	case IsoRot270:
		return IsoRot90
	}
	return i // every other isometry is its own inverse
}

// Apply writes iso(src) into dst. Both must be square and the same size.
func Apply(iso Isometry, dst matrix.MutableMatrix, src matrix.Matrix) {
	n := dst.Width()
	if dst.Height() != n || src.Width() != n || src.Height() != n {
		panic(fmt.Sprintf("transform: Apply needs equal square matrices, got dst %dx%d src %dx%d",
			dst.Width(), dst.Height(), src.Width(), src.Height()))
	}
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			sx, sy := iso.Source(n, x, y)
			dst.Set(x, y, src.At(sx, sy))
		}
	}
}
