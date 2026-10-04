package fractal

import (
	"fpress/internal/par"
	"fpress/matrix"
)

// Probe answers one question cheaply: does this matrix contain the kind of
// self-similarity the fractal stage can use at all?
//
// It runs the real matcher (see match.go) on the two smallest block sizes with
// domain blocks only on an aligned grid, a fraction of the full search's cost,
// and counts how many blocks that are not just constant runs have an exact
// recipe. Real files of ordinary kinds (text, code, audio-like samples,
// compressed data) have none, and the full fractal step then costs several
// times the work of everything else and changes nothing; this lets a caller
// skip it. Matrices with tiles, repeated records, ramps or true fractal
// structure show many hits.
//
// The probe can only miss structure that needs the finer domain grid of the
// full search (copies at odd offsets); it never reports a hit that is not real.
type ProbeResult struct {
	Nodes    int // blocks examined that are not constant runs
	Hits     int // ...that have an exact recipe
	CopyHits int // ...of which are same-scale copies
}

// The threshold comes from measurement. Across text, executables, audio-like
// samples, terrain and a 2 MB collection of real files, at several fold widths,
// the share of blocks with a hit never exceeded 1.5% (tiny 4x4 patterns that
// repeat by chance); anything with real structure was far above: a mixed file
// 10.9%, repeated records 27-71%, a gradient 93%, tile maps 97-99%, a
// Sierpinski pattern 100%. 4% leaves a margin of at least 2.7x on both sides.
const (
	probeMinHits = 16 // fewer hits than this is chance, whatever the proportion
	probePerMil  = 40 // hits must also be at least this many per thousand blocks examined (4%)
)

// Worth reports whether the fractal stage is likely to find enough to matter.
func (p ProbeResult) Worth() bool {
	return p.Hits >= probeMinHits && p.Hits*1000 >= p.Nodes*probePerMil
}

// DecimatedHits is the number of hits that use the usual 2:1 decimated domains.
func (p ProbeResult) DecimatedHits() int { return p.Hits - p.CopyHits }

// Narrow returns opt with a family of domains switched off if the probe saw no
// hit in it: the full search then skips work that cannot pay. The probe looks
// at an aligned sample of the domains, so a family with hits only at odd
// offsets is missed; that is the price of the saving.
func (p ProbeResult) Narrow(opt Options) Options {
	if p.CopyHits == 0 {
		opt.NoSameScale = true
	}
	if p.DecimatedHits() == 0 && p.CopyHits > 0 {
		opt.NoDecimated = true
	}
	return opt
}

// Rate is the share of examined blocks that have a recipe (0 if none examined).
func (p ProbeResult) Rate() float64 {
	if p.Nodes == 0 {
		return 0
	}
	return float64(p.Hits) / float64(p.Nodes)
}

// Probe examines m. opt supplies Workers, AnchorPercent and NoSameScale; block
// sizes and strides are fixed by the probe.
func Probe(m matrix.Matrix, opt Options) (ProbeResult, error) {
	var res ProbeResult
	opt.Block, opt.MinBlock = 8, 4
	if err := opt.validate(); err != nil {
		return res, err
	}
	src := matrix.Bytes(m)
	w, h := m.Width(), m.Height()
	if len(src) == 0 {
		return res, nil
	}
	c := &Compressed{Width: w, Height: h, Block: opt.Block, Iterations: opt.Iterations, Start: mostFrequent(src)}
	if err := c.validate(); err != nil {
		return res, err
	}
	pw, ph := c.paddedDims()
	orig := matrix.NewDense(pw, ph)
	orig.Fill(c.Start)
	for y := 0; y < h; y++ {
		copy(orig.Row(y), src[y*w:(y+1)*w])
	}
	e := &encoder{opt: opt, c: c, orig: orig, st: &Stats{}, w: w, h: h, pw: pw, ph: ph, workers: par.Workers(opt.Workers)}

	for _, s := range []int{8, 4} {
		copyStride := s
		if opt.NoSameScale {
			copyStride = 0
		}
		lv := newLevelData(s, pw, ph, s, copyStride)
		e.matchLevel(lv)

		nodes := make([]int, lv.ny)
		hits := make([]int, lv.ny)
		copies := make([]int, lv.ny)
		par.For(lv.ny, e.workers, 1, func(ny int) {
			for nx := 0; nx < lv.nx; nx++ {
				ni := ny*lv.nx + nx
				if lv.skip[ni] == 0 || e.isFlat(nx*s, ny*s, s) {
					continue
				}
				nodes[ny]++
				t, ok := e.recipeFor(lv, ni, nx*s, ny*s)
				if !ok {
					continue
				}
				if t.SameScale && !e.denseEnough(blockInfo{realCells: e.realCells(nx*s, ny*s, s), skipMis: int(lv.skip[ni])}) {
					continue // the planner would not offer this copy
				}
				hits[ny]++
				if t.SameScale {
					copies[ny]++
				}
			}
		})
		for i := range nodes {
			res.Nodes += nodes[i]
			res.Hits += hits[i]
			res.CopyHits += copies[i]
		}
	}
	return res, nil
}

// isFlat reports whether the s x s block at (rx, ry) is one repeated value.
func (e *encoder) isFlat(rx, ry, s int) bool {
	pix, pw := e.orig.Pix(), e.pw
	v := pix[ry*pw+rx]
	for y := 0; y < s; y++ {
		for _, b := range pix[(ry+y)*pw+rx : (ry+y)*pw+rx+s] {
			if b != v {
				return false
			}
		}
	}
	return true
}
