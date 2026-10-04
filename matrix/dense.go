package matrix

import "fmt"

// Dense is a row-major matrix backed by a single byte slice.
type Dense struct {
	w, h int
	pix  []byte
}

var (
	_ MutableMatrix = (*Dense)(nil)
	_ RowAccessor   = (*Dense)(nil)
	_ SubMatrix     = (*Dense)(nil)
)

// NewDense returns a zero-filled w x h matrix.
func NewDense(w, h int) *Dense {
	if w < 0 || h < 0 {
		panic(fmt.Sprintf("matrix: negative dimensions %dx%d", w, h))
	}
	return &Dense{w: w, h: h, pix: make([]byte, w*h)}
}

// FromBytes folds data into a matrix of the given width, row-major. The last
// row is zero-padded if len(data) is not a multiple of width. The data is
// copied.
func FromBytes(data []byte, width int) *Dense {
	if width <= 0 {
		panic(fmt.Sprintf("matrix: FromBytes width must be positive, got %d", width))
	}
	d := NewDense(width, (len(data)+width-1)/width)
	copy(d.pix, data)
	return d
}

func (d *Dense) Width() int  { return d.w }
func (d *Dense) Height() int { return d.h }

func (d *Dense) At(x, y int) byte {
	checkCell(d.w, d.h, x, y)
	return d.pix[y*d.w+x]
}

func (d *Dense) Set(x, y int, v byte) {
	checkCell(d.w, d.h, x, y)
	d.pix[y*d.w+x] = v
}

// Row returns row y as a slice aliasing the matrix storage.
func (d *Dense) Row(y int) []byte {
	if y < 0 || y >= d.h {
		panic(fmt.Sprintf("matrix: row %d out of range for %dx%d matrix", y, d.w, d.h))
	}
	return d.pix[y*d.w : (y+1)*d.w : (y+1)*d.w]
}

// Pix returns the row-major backing slice.
func (d *Dense) Pix() []byte { return d.pix }

// Fill sets every cell to v.
func (d *Dense) Fill(v byte) {
	for i := range d.pix {
		d.pix[i] = v
	}
}

// Sub returns a zero-copy read-only window onto d.
func (d *Dense) Sub(x, y, w, h int) Matrix { return NewView(d, x, y, w, h) }
