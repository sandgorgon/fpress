package fractal

import (
	"bytes"
	"errors"
	"fmt"
	"math"

	"fpress/entropy"
	"fpress/internal/par"
	"fpress/matrix"
	"fpress/transform"
)

// Options tunes the encoder. None of them affect correctness: reconstruction
// is exact for any values.
type Options struct {
	Block      int // largest range block side (the grid unit)
	MinBlock   int // smallest range block side; 0 means Block (no quad-tree)
	Iterations int // decoder rounds, stored in the stream

	// Stride is the spacing between candidate domain blocks for the largest
	// range size; smaller sizes scale it down proportionally (minimum 1).
	Stride int

	// RecipeCost is the estimated cost, in residual-byte equivalents, of
	// storing one recipe. Every choice (recipe, leave to residual, raw anchor,
	// split into four) is made by the lowest estimated total cost.
	RecipeCost int

	// MaxRefine bounds the encode/trial-decode/demote loop that removes
	// recipes which do not behave as predicted when actually iterated.
	MaxRefine int

	// AnchorPercent: a block with no cheaper option is stored as a raw anchor
	// when more than this percentage of its cells differ from the start value
	// (anchors give other recipes real data to iterate from). 100 disables
	// anchors, so such blocks are left to the residual.
	AnchorPercent int

	// NoSameScale turns off recipes that copy a block of their own size (2D
	// copies); only decimated 2:1 domains are used, as before they existed.
	NoSameScale bool

	// NoDecimated turns off the usual recipes (copies from a 2x larger,
	// subsampled domain), leaving only same-scale copies. Like NoSameScale it
	// only saves the cost of searching a family of domains that has nothing in
	// it; callers learn which families have anything from Probe.
	NoDecimated bool

	// CopyMinSize is the smallest block a same-scale copy is offered to (0 =
	// every size). Copies of tiny blocks are rarely a gain: the context-mixing
	// coder already codes repeated short rows cheaply, and a record costs
	// several bytes.
	CopyMinSize int

	// Exhaustive searches every domain block, isometry and value map for the
	// best approximate match instead of looking up exact matches in a hash
	// index. It is far slower and rarely finds more.
	Exhaustive bool

	// NoSelfCheck skips the encoder's own decode-and-compare of what it just
	// produced. Encode verifies by default, so a bug can never return a bad
	// stream; a caller that verifies the final result itself (package codec
	// does, once, for the candidate it actually keeps) can skip the inner check
	// and not pay for it on candidates that are thrown away.
	NoSelfCheck bool

	// Tile is the side of the square tiles used by EncodeTiled. 0 chooses
	// automatically: the whole matrix as one tile when it fits MemoryLimit
	// (blocks can then copy from anywhere in it), otherwise the largest tiles
	// that do. Tiles are independent, so smaller ones lose similarity across
	// their edges but run in parallel with less memory each.
	Tile int

	// MemoryLimit is the budget, in bytes, the encoder plans its tile size and
	// parallelism around (0 = 8 GiB). It is an estimate-based plan, not a hard
	// cap: the model errs on the high side.
	MemoryLimit int64
	// Workers is the number of tiles encoded or decoded at once (0 = GOMAXPROCS).
	Workers int
}

const (
	maxTiledBlock = 1 << 10
	maxTile       = 1 << 15 // a tile of maxTile x maxTile is 2^30 cells, the most one Compressed holds
	anchorCost    = 5       // estimated bytes of record overhead for a raw anchor
)

// DefaultOptions returns the default settings.
func DefaultOptions() Options {
	return Options{
		Block: 16, MinBlock: 4, Iterations: 12, Stride: 8,
		RecipeCost: 4, MaxRefine: 32, AnchorPercent: 25,
	}
}

// sizes lists the range block sizes from largest to smallest.
func (o Options) sizes() []int {
	minB := o.MinBlock
	if minB == 0 {
		minB = o.Block
	}
	var out []int
	for s := o.Block; s >= minB; s /= 2 {
		out = append(out, s)
	}
	return out
}

func (o Options) validate() error {
	switch {
	case o.Block < 1 || o.Block > maxTiledBlock:
		return fmt.Errorf("fractal: Block must be in [1,%d], got %d", maxTiledBlock, o.Block)
	case o.Iterations < 1:
		return fmt.Errorf("fractal: Iterations must be >= 1, got %d", o.Iterations)
	case o.Stride < 1:
		return fmt.Errorf("fractal: Stride must be >= 1, got %d", o.Stride)
	case o.RecipeCost < 0 || o.MaxRefine < 0:
		return errors.New("fractal: RecipeCost and MaxRefine must be >= 0")
	case o.AnchorPercent < 0:
		return errors.New("fractal: AnchorPercent must be >= 0")
	case o.Tile < 0 || o.Tile > maxTile:
		return fmt.Errorf("fractal: Tile must be in [0,%d], got %d", maxTile, o.Tile)
	case o.Workers < 0:
		return errors.New("fractal: Workers must be >= 0")
	}
	if o.MinBlock != 0 {
		ok := o.MinBlock >= 1 && o.MinBlock <= o.Block
		s := o.Block
		for ok && s > o.MinBlock {
			ok = s%2 == 0
			s /= 2
		}
		if !ok || s != o.MinBlock {
			return fmt.Errorf("fractal: Block %d must be MinBlock %d times a power of two", o.Block, o.MinBlock)
		}
	}
	return nil
}

// Stats describes what the encoder did. ExactCells is the convergence
// measure: how many cells the iterated recipes reproduced with no help from
// the residual.
type Stats struct {
	Cells           int
	RangeBlocks     int
	Recipes         int // transforms that copy from a domain block
	CopyRecipes     int // ...of which copy a block of their own size
	Anchors         int // raw transforms
	Demoted         int // recipes removed because they failed when actually decoded
	RefineRounds    int
	ExactCells      int
	ResidualNonZero int
	ResidualPacked  int // bytes of flate-compressed residual
}

// stageHook, when set, is called as the encoder finishes each stage. It is a
// profiling aid for tests; leave it nil in production.
var stageHook func(stage string)

func stage(name string) {
	if stageHook != nil {
		stageHook(name)
	}
}

// Encode compresses m. The result always decodes back to exactly m; Encode
// checks this itself and returns an error rather than a faulty stream.
func Encode(m matrix.Matrix, opt Options) (*Compressed, Stats, error) {
	var st Stats
	if err := opt.validate(); err != nil {
		return nil, st, err
	}
	src := matrix.Bytes(m)
	w, h := m.Width(), m.Height()
	c := &Compressed{
		Width: w, Height: h, Block: opt.Block, Iterations: opt.Iterations,
		Start: mostFrequent(src),
	}
	if err := c.validate(); err != nil {
		return nil, st, err
	}
	st.Cells = len(src)
	if len(src) == 0 {
		return c, st, nil
	}

	pw, ph := c.paddedDims()
	orig := matrix.NewDense(pw, ph)
	orig.Fill(c.Start)
	for y := 0; y < h; y++ {
		copy(orig.Row(y), src[y*w:(y+1)*w])
	}

	stage("prepare")
	e := &encoder{opt: opt, c: c, orig: orig, st: &st, w: w, h: h, pw: pw, ph: ph, workers: par.Workers(opt.Workers)}
	e.match()
	stage("match")
	if err := e.finish(src, true); err != nil {
		return nil, st, err
	}
	best := e

	// Same-scale copies are a bet that the data repeats at its own scale. Whether
	// they pay depends on what the coder would have done anyway with the data
	// they replace, which the planner can only estimate. So when copies were
	// used, plan the same matches once more without them (the match search, the
	// expensive part, is shared) and keep the smaller. A result is therefore
	// never larger than it would be if copies did not exist.
	if st.CopyRecipes > 0 {
		alt := e.fork()
		if err := alt.finish(src, false); err != nil {
			return nil, st, err
		}
		if a, b := marshalSize(alt.c, e.workers), marshalSize(e.c, e.workers); a >= 0 && b >= 0 && a <= b {
			best = alt
		}
	}
	c, st = best.c, *best.st

	// Safety net: never hand back a stream that does not reproduce the input.
	stage("count")
	if !opt.NoSelfCheck {
		got, err := Decode(c)
		if err != nil {
			return nil, st, fmt.Errorf("fractal: internal error, own output failed to decode: %w", err)
		}
		if !bytes.Equal(matrix.Bytes(got), src) {
			return nil, st, errors.New("fractal: internal error, round trip mismatch")
		}
	}
	stage("self-check")
	return c, st, nil
}

// marshalSize is the serialized size of c, or -1 if it cannot be serialized.
func marshalSize(c *Compressed, workers int) int {
	b, err := c.marshal(workers)
	if err != nil {
		return -1
	}
	return len(b)
}

// finish plans from the match results, repairs the plan by trial decoding,
// computes the residual and packs it, leaving the result in e.c and e.st.
func (e *encoder) finish(src []byte, allowCopy bool) error {
	e.planRoots(allowCopy)
	stage("plan")
	a := e.refine()
	stage("refine")

	w, h, st, c := e.w, e.h, e.st, e.c
	res := make([]byte, len(src))
	wrong := make([]int, h) // wrong cells per row, summed afterwards in a fixed order
	par.For(h, e.workers, 16, func(y int) {
		row, out, n := a.Row(y), res[y*w:(y+1)*w], 0
		for x, v := range src[y*w : (y+1)*w] {
			if r := v ^ row[x]; r != 0 {
				out[x] = r
				n++
			}
		}
		wrong[y] = n
	})
	for _, n := range wrong {
		st.ResidualNonZero += n
	}
	st.ExactCells = len(src) - st.ResidualNonZero
	stage("residual")
	c.Residual = packResidual(res, w, st.ResidualNonZero, e.workers)
	stage("pack-residual")
	st.ResidualPacked = len(c.Residual)
	for i := range c.Transforms {
		if c.Transforms[i].Raw {
			st.Anchors++
		} else {
			st.Recipes++
			if c.Transforms[i].SameScale {
				st.CopyRecipes++
			}
		}
	}
	return nil
}

func mostFrequent(b []byte) byte {
	var count [256]int
	for _, v := range b {
		count[v]++
	}
	best := 0
	for v := 1; v < 256; v++ {
		if count[v] > count[best] {
			best = v
		}
	}
	return byte(best)
}

// blockInfo is what the encoder remembers about the block behind each
// transform, so a failed recipe can be replaced later.
type blockInfo struct {
	realCells int // cells inside the original matrix (not padding)
	skipMis   int // real cells that differ from the start value
}

type encoder struct {
	opt          Options
	c            *Compressed
	orig         *matrix.Dense // original, padded with the start value
	st           *Stats
	w, h, pw, ph int

	infos   []blockInfo // parallel to c.Transforms
	lvls    map[int]*levelData
	exh     map[int]*level // pooled domain blocks, only for Options.Exhaustive
	minB    int
	workers int // goroutines for the parallel stages (>= 1)
}

// level holds the candidate domain blocks of one range block size s for the
// exhaustive search: each 2s x 2s domain, decimated to s x s.
type level struct {
	s          int
	domX, domY []int
	pool       []byte // pool[d*s*s:(d+1)*s*s]
	perms      [transform.NumIsometries][]int
}

// nodePlan is the cheapest way found to cover one quad-tree node.
type nodePlan struct {
	ts    []Transform
	infos []blockInfo
	cost  int
}

// plan covers every grid cell with transforms, choosing for each quad-tree
// node the cheapest of: a recipe, leaving it to the residual (or a raw
// anchor), or splitting into four. The original data stands in for what the
// decoder will see; refine checks that assumption afterwards.
//
// Recipes for every node of every size are found first, level by level, by a
// parallel join (see match.go); then each root block's tree is planned
// independently, in parallel, into its own slot.
func (e *encoder) plan() {
	e.match()
	e.planRoots(true)
}

// match finds the recipes for every node of every size. It depends only on the
// original data, so its result (e.lvls) can be shared by several plans.
func (e *encoder) match() {
	sizes := e.opt.sizes()
	e.minB = sizes[len(sizes)-1]
	e.lvls = map[int]*levelData{}
	e.exh = map[int]*level{}
	for _, s := range sizes {
		stride := max(1, e.opt.Stride*s/e.opt.Block)
		copyStride := stride
		if e.opt.NoSameScale {
			copyStride = 0
		}
		if e.opt.NoDecimated {
			stride = 0
		}
		lv := newLevelData(s, e.pw, e.ph, stride, copyStride)
		e.lvls[s] = lv
		if e.opt.Exhaustive {
			e.exh[s] = e.buildLevel(s)
		} else {
			e.matchLevel(lv)
		}
	}
}

// planRoots plans every root block from the match results, into e.c and e.infos.
func (e *encoder) planRoots(allowCopy bool) {
	R := e.opt.Block
	nx, ny := e.pw/R, e.ph/R
	plans := make([]nodePlan, nx*ny)
	par.For(len(plans), e.workers, 1, func(i int) {
		plans[i] = e.planNodeOpt((i%nx)*R, (i/nx)*R, R, allowCopy)
	})
	e.st.RangeBlocks += len(plans)
	for _, np := range plans {
		e.c.Transforms = append(e.c.Transforms, np.ts...)
		e.infos = append(e.infos, np.infos...)
	}
}

// fork returns an encoder over the same data and match results with an empty
// plan, for planning the same matrix a second way.
func (e *encoder) fork() *encoder {
	f := *e
	c := *e.c
	c.Transforms, c.Residual = nil, nil
	st := Stats{Cells: e.st.Cells}
	f.c, f.st, f.infos = &c, &st, nil
	return &f
}

func (e *encoder) realCells(rx, ry, s int) int {
	return max(0, min(s, e.w-rx)) * max(0, min(s, e.h-ry))
}

func (e *encoder) planNode(rx, ry, s int) nodePlan { return e.planNodeOpt(rx, ry, s, true) }

// planNodeOpt is planNode with same-scale copies switched off when allowCopy is
// false: the plan the encoder made before copies existed, which is what a
// failed copy is replaced with.
func (e *encoder) planNodeOpt(rx, ry, s int, allowCopy bool) nodePlan {
	lv := e.lvls[s]
	var tgt []byte
	var real []int
	skipMis, realCells := 0, e.realCells(rx, ry, s)
	if e.opt.Exhaustive { // the exhaustive search needs the cells themselves
		tgt, real = make([]byte, s*s), make([]int, 0, s*s)
		for y := 0; y < s; y++ {
			for x := 0; x < s; x++ {
				v := e.orig.At(rx+x, ry+y)
				tgt[y*s+x] = v
				if rx+x < e.w && ry+y < e.h {
					real = append(real, y*s+x)
					if v != e.c.Start {
						skipMis++
					}
				}
			}
		}
	} else {
		skipMis = int(lv.skip[(ry/s)*lv.nx+rx/s])
	}
	if skipMis == 0 {
		return nodePlan{} // already equal to the start value
	}
	info := blockInfo{realCells: realCells, skipMis: skipMis}

	var best nodePlan
	best.cost = math.MaxInt
	if e.opt.Exhaustive {
		if l := e.exh[s]; len(l.domX) > 0 {
			if t, mis, ok := e.searchAll(l, rx, ry, tgt, real); ok {
				best = nodePlan{ts: []Transform{t}, infos: []blockInfo{info}, cost: e.opt.RecipeCost + mis}
			}
		}
	} else if t, ok := e.recipeFor(lv, (ry/s)*lv.nx+rx/s, rx, ry); ok && (!t.SameScale || (allowCopy && s >= e.opt.CopyMinSize && e.denseEnough(info))) {
		best = nodePlan{ts: []Transform{t}, infos: []blockInfo{info}, cost: e.opt.RecipeCost}
	}
	if t, ok := e.fallback(rx, ry, s, info); ok {
		if c := info.realCells + anchorCost; c < best.cost {
			best = nodePlan{ts: []Transform{t}, infos: []blockInfo{info}, cost: c}
		}
	} else if skipMis < best.cost {
		best = nodePlan{cost: skipMis}
	}
	if s > e.minB {
		half := s / 2
		split := nodePlan{}
		for _, off := range [4][2]int{{0, 0}, {half, 0}, {0, half}, {half, half}} {
			child := e.planNodeOpt(rx+off[0], ry+off[1], half, allowCopy)
			split.ts = append(split.ts, child.ts...)
			split.infos = append(split.infos, child.infos...)
			split.cost += child.cost
			if split.cost >= best.cost {
				break // already no cheaper than the best so far
			}
		}
		if split.cost < best.cost {
			best = split
		}
	}
	return best
}

// denseEnough reports whether a block is dense enough to be stored as real data
// in the approximation (the same test fallback uses to choose an anchor over
// leaving the block to the residual). A same-scale copy is only offered to such
// blocks: the decoder copies from its current approximation, in which a sparse
// block is still just the start value, so a copy of a sparse block would come
// out empty, fail the trial decode, and be demoted at the cost of repair rounds
// and a worse fallback. Sparse blocks are cheap to leave to the residual.
func (e *encoder) denseEnough(info blockInfo) bool {
	return info.skipMis*100 > e.opt.AnchorPercent*info.realCells
}

// fallback returns a raw anchor for the block when the block is far enough
// from the start value to be worth anchoring; otherwise the block is left for
// the residual.
func (e *encoder) fallback(rx, ry, s int, info blockInfo) (Transform, bool) {
	if info.skipMis*100 <= e.opt.AnchorPercent*info.realCells {
		return Transform{}, false
	}
	data := make([]byte, s*s)
	for y := 0; y < s; y++ {
		copy(data[y*s:(y+1)*s], e.orig.Row(ry + y)[rx:rx+s])
	}
	return Transform{RX: rx, RY: ry, Size: s, Raw: true, Data: data}, true
}

// buildLevel decimates every domain block for the exhaustive search.
func (e *encoder) buildLevel(s int) *level {
	l := &level{s: s}
	for iso := transform.Isometry(0); iso < transform.NumIsometries; iso++ {
		p := make([]int, s*s)
		for y := 0; y < s; y++ {
			for x := 0; x < s; x++ {
				sx, sy := iso.Source(s, x, y)
				p[y*s+x] = sy*s + sx
			}
		}
		l.perms[iso] = p
	}
	stride := max(1, e.opt.Stride*s/e.opt.Block)
	tmp := matrix.NewDense(s, s)
	for dy := 0; dy+2*s <= e.ph; dy += stride {
		for dx := 0; dx+2*s <= e.pw; dx += stride {
			transform.Decimate2x(tmp, matrix.NewView(e.orig, dx, dy, 2*s, 2*s))
			l.domX = append(l.domX, dx)
			l.domY = append(l.domY, dy)
			l.pool = append(l.pool, tmp.Pix()...)
		}
	}
	return l
}

// searchAll tries every domain block, isometry and value map and returns the
// one agreeing with tgt on the most real cells.
func (e *encoder) searchAll(l *level, rx, ry int, tgt []byte, real []int) (Transform, int, bool) {
	s := l.s
	RR := s * s
	n := len(real)
	bestMis := n + 1
	var best Transform
	found := false

	var hAdd, hXor [256]int32
	for d := range l.domX {
		if overlaps(l.domX[d], l.domY[d], 2*s, rx, ry, s) {
			continue
		}
		dom := l.pool[d*RR : (d+1)*RR]
		for iso := transform.Isometry(0); iso < transform.NumIsometries; iso++ {
			perm := l.perms[iso]
			clear(hAdd[:])
			clear(hXor[:])
			eq := 0
			var addMax, xorMax int32
			var addC, xorC byte
			for _, i := range real {
				t, g := dom[perm[i]], tgt[i]
				if t == g {
					eq++
				}
				k := g - t
				if hAdd[k]++; hAdd[k] > addMax {
					addMax, addC = hAdd[k], k
				}
				k = g ^ t
				if hXor[k]++; hXor[k] > xorMax {
					xorMax, xorC = hXor[k], k
				}
			}
			// Prefer the simplest map on ties: identity, then add, then xor.
			matches, vm := eq, transform.ValueMap{}
			if int(addMax) > matches {
				matches, vm = int(addMax), transform.ValueMap{Kind: transform.MapAdd, C: addC}
			}
			if int(xorMax) > matches {
				matches, vm = int(xorMax), transform.ValueMap{Kind: transform.MapXor, C: xorC}
			}
			if mis := n - matches; mis < bestMis {
				bestMis = mis
				best = Transform{RX: rx, RY: ry, Size: s, DX: l.domX[d], DY: l.domY[d], Iso: iso, Map: vm}
				found = true
				if mis == 0 {
					return best, 0, true
				}
			}
		}
	}
	return best, bestMis, found
}

// refine trial-decodes the plan and replaces recipes that do not pay off when
// actually iterated (for example because their domain depends on blocks that
// are themselves wrong). Replacement only ever removes recipes, so the loop
// ends. It returns the approximation A for the final transform list.
func (e *encoder) refine() *matrix.Dense {
	for round := 0; ; round++ {
		a := reconstruct(e.c, e.workers)
		e.st.RefineRounds = round
		if round >= e.opt.MaxRefine {
			return a
		}
		if e.demote(a) == 0 {
			return a
		}
	}
}

// demote removes failing recipes, but only the ones whose input is already as
// good as it will get: a failing recipe whose domain overlaps no other failing
// recipe's range block. Recipes that merely read from a failing block are kept,
// because they may be fine once that block is fixed. If every failure depends
// on another failure (a cycle), the single worst one goes.
func (e *encoder) demote(a *matrix.Dense) int {
	// Counting wrong cells per recipe is independent per recipe: do it in
	// parallel, into fixed slots, then decide sequentially.
	counts := make([]int, len(e.c.Transforms))
	par.For(len(counts), e.workers, 32, func(i int) {
		if t := e.c.Transforms[i]; !t.Raw {
			counts[i] = e.actualMismatches(a, t)
		}
	})
	var failing []int
	mis := map[int]int{}
	for i, t := range e.c.Transforms {
		if t.Raw {
			continue
		}
		if m := counts[i]; m+e.opt.RecipeCost >= e.infos[i].skipMis {
			failing = append(failing, i)
			mis[i] = m
		}
	}
	if len(failing) == 0 {
		return 0
	}
	victims := map[int]bool{}
	for _, i := range failing {
		t := e.c.Transforms[i]
		clean := true
		for _, j := range failing {
			u := e.c.Transforms[j]
			if overlaps(t.DX, t.DY, 2*t.Size, u.RX, u.RY, u.Size) {
				clean = false
				break
			}
		}
		if clean {
			victims[i] = true
		}
	}
	if len(victims) == 0 {
		worst := failing[0]
		for _, i := range failing {
			if mis[i] > mis[worst] {
				worst = i
			}
		}
		victims[worst] = true
	}

	// A failed copy sends its whole root block back to the plan the encoder made
	// before copies existed. Replacing just the failed block is not enough: the
	// planner may have split a parent into children because each had a cheap
	// copy, and four anchors cost more than the one parent anchor it would
	// otherwise have chosen. So each root is either all copy-enhanced and
	// working, or exactly the old plan.
	R := e.opt.Block
	rootOf := func(t Transform) [2]int { return [2]int{t.RX / R, t.RY / R} }
	replace := map[[2]int]bool{}
	for i, t := range e.c.Transforms {
		if victims[i] && t.SameScale {
			replace[rootOf(t)] = true
		}
	}
	keptT := e.c.Transforms[:0:0]
	keptI := e.infos[:0:0]
	emitted := map[[2]int]bool{}
	for i, t := range e.c.Transforms {
		if root := rootOf(t); replace[root] {
			if !emitted[root] {
				emitted[root] = true
				np := e.planNodeOpt(root[0]*R, root[1]*R, R, false)
				keptT, keptI = append(keptT, np.ts...), append(keptI, np.infos...)
			}
			if victims[i] {
				e.st.Demoted++
			}
			continue
		}
		if victims[i] {
			e.st.Demoted++
			if raw, ok := e.fallback(t.RX, t.RY, t.Size, e.infos[i]); ok {
				keptT, keptI = append(keptT, raw), append(keptI, e.infos[i])
			}
			continue
		}
		keptT, keptI = append(keptT, t), append(keptI, e.infos[i])
	}
	e.c.Transforms, e.infos = keptT, keptI
	return len(victims)
}

// overlaps reports whether the square of side an at (ax, ay) intersects the
// square of side bn at (bx, by).
func overlaps(ax, ay, an, bx, by, bn int) bool {
	return ax < bx+bn && bx < ax+an && ay < by+bn && by < ay+an
}

// actualMismatches counts real cells of t's range block where the iterated
// approximation differs from the original.
func (e *encoder) actualMismatches(a *matrix.Dense, t Transform) int {
	mis := 0
	for y := 0; y < t.Size && t.RY+y < e.h; y++ {
		for x := 0; x < t.Size && t.RX+x < e.w; x++ {
			if a.At(t.RX+x, t.RY+y) != e.orig.At(t.RX+x, t.RY+y) {
				mis++
			}
		}
	}
	return mis
}

// packResidual stores the XOR residual (w cells per row) in the smallest of:
// nothing, if every cell matched; DEFLATE; or the context-mixing coder (in
// independent chunks, run in parallel), which can use the neighbours above.
// The slow coder is skipped when over half the cells are wrong: then the
// fractal stage has failed on this matrix anyway and the caller will pick
// another way to store it.
func packResidual(res []byte, w, nonZero, workers int) []byte {
	if nonZero == 0 {
		return []byte{residualZero}
	}
	best := append([]byte{residualFlate}, deflate(res)...)
	if nonZero*2 <= len(res) && len(res) <= maxCMResidual {
		if cm := append([]byte{residualCM}, entropy.EncodeChunked(res, w, entropy.DefaultChunk, workers)...); len(cm) < len(best) {
			best = cm
		}
	}
	return best
}
