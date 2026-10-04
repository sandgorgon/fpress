package fractal

import (
	"fmt"

	"fpress/internal/par"
	"fpress/matrix"
	"fpress/transform"
)

// reconstruct runs the transforms for c.Iterations rounds and returns the
// approximation A over the padded grid. The stream must already be validated
// (in particular, its range blocks must not overlap).
//
// Each round reads only the previous round's grid (double buffering) and every
// transform writes only its own range block, so the transforms of a round can
// run on any number of goroutines in any order with the same result; the end
// of each round is the barrier. Cells that no transform writes stay at the
// start value.
func reconstruct(c *Compressed, workers int) *matrix.Dense {
	pw, ph := c.paddedDims()
	cur, next := matrix.NewDense(pw, ph), matrix.NewDense(pw, ph)
	workers = par.Workers(workers)
	if len(c.Transforms) < 64 || pw*ph < 1<<15 {
		workers = 1 // too little work to be worth sharing
	}
	fillParallel(cur.Pix(), c.Start, workers)
	geoms := make([]map[[2]int]*blockGeom, workers) // per worker: (size, scale) -> lookup
	for i := range geoms {
		geoms[i] = map[[2]int]*blockGeom{}
	}
	for it := 0; it < c.Iterations; it++ {
		fillParallel(next.Pix(), c.Start, workers)
		par.ForWorker(len(c.Transforms), workers, 16, func(w, i int) {
			t := &c.Transforms[i]
			scale := 2
			if t.SameScale {
				scale = 1
			}
			g := geoms[w][[2]int{t.Size, scale}]
			if g == nil {
				g = newBlockGeom(t.Size, scale)
				geoms[w][[2]int{t.Size, scale}] = g
			}
			applyFused(next, cur, t, g)
		})
		cur, next = next, cur
	}
	return cur
}

func fillParallel(p []byte, v byte, workers int) {
	const chunk = 1 << 16
	par.For((len(p)+chunk-1)/chunk, workers, 1, func(i int) {
		s := p[i*chunk : min(len(p), (i+1)*chunk)]
		for j := range s {
			s[j] = v
		}
	})
}

// blockGeom says, for each isometry, which cell of the domain feeds each cell
// of an s x s range block: the decimate-then-orient steps fused into a lookup.
// px[iso][i], py[iso][i] are the domain-relative column and row. With scale 2
// the domain is 2s x 2s and every other cell is read; with scale 1 it is s x s.
type blockGeom struct {
	px, py [transform.NumIsometries][]int32
}

func newBlockGeom(s, scale int) *blockGeom {
	g := &blockGeom{}
	for iso := transform.Isometry(0); iso < transform.NumIsometries; iso++ {
		g.px[iso], g.py[iso] = make([]int32, s*s), make([]int32, s*s)
		for y := 0; y < s; y++ {
			for x := 0; x < s; x++ {
				sx, sy := iso.Source(s, x, y)
				g.px[iso][y*s+x], g.py[iso][y*s+x] = int32(scale*sx), int32(scale*sy)
			}
		}
	}
	return g
}

// applyFused writes the range block of t into next. For a recipe it is
// exactly Decimate2x (unless SameScale), then the isometry, then the value map,
// applied to the domain block of cur (a test checks this against the composed
// operations).
func applyFused(next, cur *matrix.Dense, t *Transform, g *blockGeom) {
	s, stride := t.Size, next.Width()
	np, cp := next.Pix(), cur.Pix()
	if t.Raw {
		for y := 0; y < s; y++ {
			o := (t.RY+y)*stride + t.RX
			copy(np[o:o+s], t.Data[y*s:(y+1)*s])
		}
		return
	}
	px, py := g.px[t.Iso], g.py[t.Iso]
	for y := 0; y < s; y++ {
		o := (t.RY+y)*stride + t.RX
		dst := np[o : o+s]
		xs, ys := px[y*s:(y+1)*s], py[y*s:(y+1)*s]
		switch t.Map.Kind {
		case transform.MapAdd:
			c := t.Map.C
			for x := range dst {
				dst[x] = cp[(t.DY+int(ys[x]))*stride+t.DX+int(xs[x])] + c
			}
		case transform.MapXor:
			c := t.Map.C
			for x := range dst {
				dst[x] = cp[(t.DY+int(ys[x]))*stride+t.DX+int(xs[x])] ^ c
			}
		default:
			for x := range dst {
				dst[x] = cp[(t.DY+int(ys[x]))*stride+t.DX+int(xs[x])]
			}
		}
	}
}

// checkDisjoint reports an error if any two range blocks share a cell. A
// parallel rebuild is only deterministic when none do, so this is a rule of
// the format; the encoder never produces overlaps.
func checkDisjoint(ts []Transform, pw, ph int) error {
	if len(ts) < 2 {
		return nil
	}
	bits := make([]uint64, (pw*ph+63)/64)
	for i := range ts {
		t := &ts[i]
		for y := 0; y < t.Size; y++ {
			if !claim(bits, (t.RY+y)*pw+t.RX, t.Size) {
				return fmt.Errorf("fractal: transform %d: range block (%d,%d) size %d overlaps another", i, t.RX, t.RY, t.Size)
			}
		}
	}
	return nil
}

// claim marks bits [start, start+n) and reports whether all were clear.
func claim(bits []uint64, start, n int) bool {
	for n > 0 {
		w, b := start>>6, uint(start&63)
		take := min(n, 64-int(b))
		var mask uint64
		if take == 64 {
			mask = ^uint64(0)
		} else {
			mask = (1<<uint(take) - 1) << b
		}
		if bits[w]&mask != 0 {
			return false
		}
		bits[w] |= mask
		start += take
		n -= take
	}
	return true
}
