package transform

import (
	"fmt"

	"fpress/matrix"
)

// Decimate2x shrinks src to half size by keeping the top-left byte of every
// 2x2 group. src must be exactly twice the size of dst in each dimension.
// Unlike averaging, every output byte is a byte that exists in the input.
func Decimate2x(dst matrix.MutableMatrix, src matrix.Matrix) {
	if src.Width() != 2*dst.Width() || src.Height() != 2*dst.Height() {
		panic(fmt.Sprintf("transform: Decimate2x needs src twice dst, got dst %dx%d src %dx%d",
			dst.Width(), dst.Height(), src.Width(), src.Height()))
	}
	for y := 0; y < dst.Height(); y++ {
		for x := 0; x < dst.Width(); x++ {
			dst.Set(x, y, src.At(2*x, 2*y))
		}
	}
}
