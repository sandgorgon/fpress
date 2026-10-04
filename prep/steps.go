package prep

import "fpress/matrix"

// forward applies the step to src, returning a new matrix.
func (s Step) forward(src *matrix.Dense) *matrix.Dense {
	w, h := src.Width(), src.Height()
	switch s.Kind {
	case DeltaUpSub, DeltaUpXor:
		out := matrix.NewDense(w, h)
		for y := 0; y < h; y++ {
			in, o := src.Row(y), out.Row(y)
			if y == 0 {
				copy(o, in)
				continue
			}
			above := src.Row(y - 1)
			if s.Kind == DeltaUpSub {
				for x := range o {
					o[x] = in[x] - above[x]
				}
			} else {
				for x := range o {
					o[x] = in[x] ^ above[x]
				}
			}
		}
		return out
	case DeltaFrameSub, DeltaFrameXor:
		out := matrix.NewDense(w, h)
		for y := 0; y < h; y++ {
			in, o := src.Row(y), out.Row(y)
			if y < s.K {
				copy(o, in)
				continue
			}
			prev := src.Row(y - s.K)
			if s.Kind == DeltaFrameSub {
				for x := range o {
					o[x] = in[x] - prev[x]
				}
			} else {
				for x := range o {
					o[x] = in[x] ^ prev[x]
				}
			}
		}
		return out
	case DeltaLeftSub, DeltaLeftXor:
		out := matrix.NewDense(w, h)
		for y := 0; y < h; y++ {
			in, o := src.Row(y), out.Row(y)
			if w == 0 {
				continue
			}
			o[0] = in[0]
			if s.Kind == DeltaLeftSub {
				for x := 1; x < w; x++ {
					o[x] = in[x] - in[x-1]
				}
			} else {
				for x := 1; x < w; x++ {
					o[x] = in[x] ^ in[x-1]
				}
			}
		}
		return out
	case BitPlanes:
		ow := w / 8
		out := matrix.NewDense(ow, 8*h)
		for y := 0; y < h; y++ {
			in := src.Row(y)
			for p := 0; p < 8; p++ {
				o := out.Row(p*h + y)
				for x := 0; x < ow; x++ {
					var b byte
					for j := 0; j < 8; j++ {
						b |= ((in[8*x+j] >> p) & 1) << j
					}
					o[x] = b
				}
			}
		}
		return out
	case Deinterleave:
		ow := w / s.K
		out := matrix.NewDense(ow, s.K*h)
		for y := 0; y < h; y++ {
			in := src.Row(y)
			for p := 0; p < s.K; p++ {
				o := out.Row(p*h + y)
				for x := 0; x < ow; x++ {
					o[x] = in[x*s.K+p]
				}
			}
		}
		return out
	}
	panic("prep: invalid step")
}

// inverse undoes forward. (w, h) are the dimensions forward was applied to.
func (s Step) inverse(src *matrix.Dense, w, h int) *matrix.Dense {
	switch s.Kind {
	case DeltaUpSub, DeltaUpXor:
		out := matrix.NewDense(w, h)
		for y := 0; y < h; y++ {
			d, o := src.Row(y), out.Row(y)
			if y == 0 {
				copy(o, d)
				continue
			}
			above := out.Row(y - 1)
			if s.Kind == DeltaUpSub {
				for x := range o {
					o[x] = d[x] + above[x]
				}
			} else {
				for x := range o {
					o[x] = d[x] ^ above[x]
				}
			}
		}
		return out
	case DeltaFrameSub, DeltaFrameXor:
		out := matrix.NewDense(w, h)
		for y := 0; y < h; y++ {
			d, o := src.Row(y), out.Row(y)
			if y < s.K {
				copy(o, d)
				continue
			}
			prev := out.Row(y - s.K)
			if s.Kind == DeltaFrameSub {
				for x := range o {
					o[x] = d[x] + prev[x]
				}
			} else {
				for x := range o {
					o[x] = d[x] ^ prev[x]
				}
			}
		}
		return out
	case DeltaLeftSub, DeltaLeftXor:
		out := matrix.NewDense(w, h)
		for y := 0; y < h; y++ {
			d, o := src.Row(y), out.Row(y)
			if w == 0 {
				continue
			}
			o[0] = d[0]
			if s.Kind == DeltaLeftSub {
				for x := 1; x < w; x++ {
					o[x] = d[x] + o[x-1]
				}
			} else {
				for x := 1; x < w; x++ {
					o[x] = d[x] ^ o[x-1]
				}
			}
		}
		return out
	case BitPlanes:
		out := matrix.NewDense(w, h)
		ow := w / 8
		for y := 0; y < h; y++ {
			o := out.Row(y)
			for p := 0; p < 8; p++ {
				d := src.Row(p*h + y)
				for x := 0; x < ow; x++ {
					for j := 0; j < 8; j++ {
						o[8*x+j] |= ((d[x] >> j) & 1) << p
					}
				}
			}
		}
		return out
	case Deinterleave:
		out := matrix.NewDense(w, h)
		ow := w / s.K
		for y := 0; y < h; y++ {
			o := out.Row(y)
			for p := 0; p < s.K; p++ {
				d := src.Row(p*h + y)
				for x := 0; x < ow; x++ {
					o[x*s.K+p] = d[x]
				}
			}
		}
		return out
	}
	panic("prep: invalid step")
}
