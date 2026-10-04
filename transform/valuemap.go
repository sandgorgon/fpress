package transform

import (
	"fmt"

	"fpress/matrix"
)

// MapKind selects how byte values are changed.
type MapKind uint8

const (
	MapIdentity MapKind = iota // v
	MapAdd                     // v + C (mod 256)
	MapXor                     // v ^ C
)

// ValueMap is a reversible byte-to-byte relabelling.
type ValueMap struct {
	Kind MapKind
	C    byte
}

// Apply maps a single byte.
func (m ValueMap) Apply(v byte) byte {
	switch m.Kind {
	case MapAdd:
		return v + m.C
	case MapXor:
		return v ^ m.C
	}
	return v
}

// Valid reports whether m is a well-formed map. Identity must carry C == 0 so
// that each map has a single canonical form.
func (m ValueMap) Valid() bool {
	switch m.Kind {
	case MapIdentity:
		return m.C == 0
	case MapAdd, MapXor:
		return true
	}
	return false
}

// ApplyTo writes m applied to every cell of src into dst (same dimensions).
func (m ValueMap) ApplyTo(dst matrix.MutableMatrix, src matrix.Matrix) {
	if dst.Width() != src.Width() || dst.Height() != src.Height() {
		panic(fmt.Sprintf("transform: ApplyTo dimension mismatch dst %dx%d src %dx%d",
			dst.Width(), dst.Height(), src.Width(), src.Height()))
	}
	for y := 0; y < src.Height(); y++ {
		for x := 0; x < src.Width(); x++ {
			dst.Set(x, y, m.Apply(src.At(x, y)))
		}
	}
}
