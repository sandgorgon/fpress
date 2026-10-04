package matrix

// Func is a read-only matrix whose cells are computed on demand. It is handy
// for tests and for synthetic patterns that never need to be stored.
type Func struct {
	W, H int
	F    func(x, y int) byte
}

var _ Matrix = Func{}

func (f Func) Width() int  { return f.W }
func (f Func) Height() int { return f.H }

func (f Func) At(x, y int) byte {
	checkCell(f.W, f.H, x, y)
	return f.F(x, y)
}
