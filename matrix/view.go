package matrix

import "fmt"

// View is a read-only rectangular window onto another Matrix. It copies
// nothing: reads go straight through to the underlying matrix.
type View struct {
	base   Matrix
	x0, y0 int
	w, h   int
}

var _ Matrix = (*View)(nil)

// NewView returns the w x h window of m whose top-left corner is (x, y). It
// panics if the window does not lie entirely inside m.
func NewView(m Matrix, x, y, w, h int) *View {
	checkRect(m.Width(), m.Height(), x, y, w, h)
	if v, ok := m.(*View); ok { // flatten nested views
		return &View{base: v.base, x0: v.x0 + x, y0: v.y0 + y, w: w, h: h}
	}
	return &View{base: m, x0: x, y0: y, w: w, h: h}
}

func (v *View) Width() int  { return v.w }
func (v *View) Height() int { return v.h }

func (v *View) At(x, y int) byte {
	checkCell(v.w, v.h, x, y)
	return v.base.At(v.x0+x, v.y0+y)
}

// MutableView is a writable window onto a MutableMatrix.
type MutableView struct {
	View
	mbase MutableMatrix
}

var _ MutableMatrix = (*MutableView)(nil)

// NewMutableView is NewView for writable matrices.
func NewMutableView(m MutableMatrix, x, y, w, h int) *MutableView {
	checkRect(m.Width(), m.Height(), x, y, w, h)
	return &MutableView{View: View{base: m, x0: x, y0: y, w: w, h: h}, mbase: m}
}

func (v *MutableView) Set(x, y int, b byte) {
	checkCell(v.w, v.h, x, y)
	v.mbase.Set(v.x0+x, v.y0+y, b)
}

func checkRect(mw, mh, x, y, w, h int) {
	if x < 0 || y < 0 || w < 0 || h < 0 || x > mw-w || y > mh-h {
		panic(fmt.Sprintf("matrix: window (%d,%d) %dx%d outside %dx%d matrix", x, y, w, h, mw, mh))
	}
}
