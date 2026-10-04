package fractal

import (
	"math/bits"
	"runtime"
	"sync"
	"sync/atomic"

	"fpress/internal/par"
	"fpress/transform"
)

// Exact-match search without a big index.
//
// The planner wants, for every quad-tree node (a square range block), a domain
// block elsewhere in the matrix that reproduces it exactly under an isometry
// and an add or xor value map. Two blocks are related that way exactly when
// their "canonical forms" are equal: the block's values with its first value
// subtracted (add maps) or xored out (xor maps), after the isometry. So the
// search is a join on a 64-bit hash of the canonical form.
//
// The earlier design built a table of every domain block's canonical form
// under all 8 isometries: memory grew with the image, and building it was one
// long serial loop. This one turns the join around:
//
//  1. Hash the nodes that still need a recipe (usually few) into a small query
//     table, built once and then read-only: the shared table all workers use.
//  2. Stream every domain block past it. Workers take disjoint ranges of
//     domains, hash each block under each isometry and probe the table. Almost
//     every probe misses, and a small bloom filter rejects those cheaply.
//  3. A probe that hits offers the domain's entry id (domain<<3 | isometry) to
//     that group of nodes, which keeps only the few lowest and highest ids it
//     has seen.
//  4. Each node then takes the first of its group's ids whose domain does not
//     overlap the node and which really matches (a hash match is verified).
//
// Memory is independent of the number of domain blocks, every step but the
// table build is a parallel loop over disjoint work, and the outcome does not
// depend on scheduling: a group's lowest and highest ids are a function of the
// set of hits, not the order they arrive in.

const (
	noRecipe = ^uint32(0)
	candK    = 4 // lowest and highest entry ids kept per group of nodes

	fnvOffset = 14695981039346656037
	fnvPrime  = 1099511628211
	xorSalt   = 0x9E3779B97F4A7C15 // keeps add and xor keys in separate namespaces
)

// domainKind describes one family of domain blocks for a level: those of side
// scale*s read every scale-th cell (scale 2: the usual decimated domain; scale
// 1: a same-scale copy), placed every `stride` cells.
type domainKind struct {
	scale, stride, dxN, dyN int
}

func (k domainKind) count() int { return k.dxN * k.dyN }

// levelData is everything the planner knows about one range block size s.
type levelData struct {
	s      int
	nx, ny int // node grid (pw/s by ph/s)

	skip   []int32  // per node: real cells that differ from the start value
	recipe []uint32 // per node: (entry<<1 | isXor), or noRecipe
	recC   []uint8  // per node: the value map constant of the recipe

	// kinds[0] is the decimated domain (scale 2), kinds[1] the same-scale copy
	// (scale 1). An entry id is kind<<29 | domain<<3 | isometry, so with equal
	// matches the decimated kind (lower ids) is the one a node picks.
	kinds [2]domainKind
	perms [transform.NumIsometries][]int32 // output cell -> cell of the decimated s x s block
}

const kindShift = 29

func newLevelData(s, pw, ph, stride2, stride1 int) *levelData {
	l := &levelData{s: s, nx: pw / s, ny: ph / s}
	l.skip = make([]int32, l.nx*l.ny)
	l.recipe = make([]uint32, l.nx*l.ny)
	for i := range l.recipe {
		l.recipe[i] = noRecipe
	}
	l.recC = make([]uint8, l.nx*l.ny)
	for k, scale := range [2]int{2, 1} {
		stride := stride2
		if scale == 1 {
			stride = stride1
		}
		if stride <= 0 { // this kind is switched off
			l.kinds[k] = domainKind{scale: scale}
			continue
		}
		// Entry ids hold the domain number in 26 bits, so keep the count below
		// 2^26 by spacing the domains out further for enormous matrices.
		for {
			dk := domainKind{scale: scale, stride: stride}
			if side := scale * s; pw >= side && ph >= side {
				dk.dxN, dk.dyN = (pw-side)/stride+1, (ph-side)/stride+1
			}
			l.kinds[k] = dk
			if dk.count() < 1<<26 {
				break
			}
			stride *= 2
		}
	}
	for iso := transform.Isometry(0); iso < transform.NumIsometries; iso++ {
		p := make([]int32, s*s)
		for y := 0; y < s; y++ {
			for x := 0; x < s; x++ {
				sx, sy := iso.Source(s, x, y)
				p[y*s+x] = int32(sy*s + sx)
			}
		}
		l.perms[iso] = p
	}
	return l
}

func (l *levelData) numDomains() int { return l.kinds[0].count() + l.kinds[1].count() }

// hashPair hashes the add-canonical and xor-canonical forms of v read through
// perm (nil = in order). Both are FNV-1a over the bytes; the xor key is salted.
func hashPair(v []byte, perm []int32) (add, xor uint64) {
	ha, hx := uint64(fnvOffset), uint64(fnvOffset)
	if perm == nil {
		base := v[0]
		for _, b := range v {
			ha = (ha ^ uint64(b-base)) * fnvPrime
			hx = (hx ^ uint64(b^base)) * fnvPrime
		}
	} else {
		base := v[perm[0]]
		for _, p := range perm {
			b := v[p]
			ha = (ha ^ uint64(b-base)) * fnvPrime
			hx = (hx ^ uint64(b^base)) * fnvPrime
		}
	}
	return nz(ha), nz(hx ^ xorSalt)
}

func nz(k uint64) uint64 { // 0 marks an empty table slot
	if k == 0 {
		return 1
	}
	return k
}

// ---- the shared query table ------------------------------------------------

type qShard struct {
	keys    []uint64 // open addressing; 0 = empty
	grp     []uint32 // local group id of each slot
	mask    uint64
	nGroups uint32
	slot    []atomic.Uint32 // per local group: 1 + index of its candidate slot, 0 = none yet
}

type queryTable struct {
	shift  uint // key>>shift selects the shard
	shards []qShard
	bloom  []uint64
	bmask  uint64
	slab   candSlab
}

func (q *queryTable) bloomBit(key uint64) (word, bit uint64) {
	h := (key * 0x9E3779B97F4A7C15) >> 20
	return (h >> 6) & q.bmask, h & 63
}

// find returns the shard and local group of key.
func (q *queryTable) find(key uint64) (shard int, local uint32, ok bool) {
	w, b := q.bloomBit(key)
	if q.bloom[w]>>b&1 == 0 {
		return 0, 0, false
	}
	shard = int(key >> q.shift)
	sh := &q.shards[shard]
	for i := (key * 0x2545F4914F6CDD1D >> 17) & sh.mask; ; i = (i + 1) & sh.mask {
		switch sh.keys[i] {
		case key:
			return shard, sh.grp[i], true
		case 0:
			return 0, 0, false
		}
	}
}

// buildQueryTable builds the table over keys (one per item; items hash into
// groups of equal keys) and returns, for each item, its group id (shard<<24 |
// local). Construction is parallel: items are partitioned by shard in a fixed
// order, then each shard is built by one goroutine.
func buildQueryTable(keys []uint64, workers int) (*queryTable, []uint32) {
	n := len(keys)
	shardBits := uint(0)
	for shardBits < 8 && n>>(shardBits+1) >= 1<<12 {
		shardBits++
	}
	q := &queryTable{shift: 64 - shardBits}
	if shardBits == 0 {
		q.shift = 63 // key>>63 is 0 or 1; keep it in range by using two shards below
	}
	nShards := 1 << shardBits
	if shardBits == 0 {
		nShards = 2
	}
	q.shards = make([]qShard, nShards)

	// Partition the item numbers by shard, keeping item order within a shard:
	// each worker counts its own contiguous range, then writes into its share.
	w := min(workers, max(1, n/4096))
	rng := func(i int) (int, int) { return i * n / w, (i + 1) * n / w }
	counts := make([][]int, w)
	par.For(w, w, 1, func(i int) {
		c := make([]int, nShards)
		lo, hi := rng(i)
		for _, k := range keys[lo:hi] {
			c[k>>q.shift]++
		}
		counts[i] = c
	})
	start := make([]int, nShards+1)
	for s := 0; s < nShards; s++ {
		for i := 0; i < w; i++ {
			start[s+1] += counts[i][s]
		}
		start[s+1] += start[s]
	}
	offset := make([][]int, w) // where worker i starts writing for shard s
	for i := range offset {
		offset[i] = make([]int, nShards)
	}
	for s := 0; s < nShards; s++ {
		pos := start[s]
		for i := 0; i < w; i++ {
			offset[i][s] = pos
			pos += counts[i][s]
		}
	}
	order := make([]uint32, n)
	par.For(w, w, 1, func(i int) {
		lo, hi := rng(i)
		off := offset[i]
		for j := lo; j < hi; j++ {
			s := keys[j] >> q.shift
			order[off[s]] = uint32(j)
			off[s]++
		}
	})

	group := make([]uint32, n)
	par.For(nShards, workers, 1, func(s int) {
		items := order[start[s]:start[s+1]]
		sh := &q.shards[s]
		size := 1 << bits.Len(uint(max(2*len(items), 8)))
		sh.keys, sh.grp, sh.mask = make([]uint64, size), make([]uint32, size), uint64(size-1)
		for _, it := range items {
			key := keys[it]
			i := (key * 0x2545F4914F6CDD1D >> 17) & sh.mask
			for sh.keys[i] != 0 && sh.keys[i] != key {
				i = (i + 1) & sh.mask
			}
			if sh.keys[i] == 0 {
				sh.keys[i], sh.grp[i] = key, sh.nGroups
				sh.nGroups++
			}
			group[it] = uint32(s)<<24 | sh.grp[i]
		}
		sh.slot = make([]atomic.Uint32, sh.nGroups)
	})

	// A bloom filter over all keys: nearly every domain probe misses, and this
	// answers most of them from a few megabytes that stay in cache.
	bw := 1 << bits.Len(uint(max(n/4, 1024))) // 64-bit words: 16 bits per key
	q.bloom, q.bmask = make([]uint64, bw), uint64(bw-1)
	par.For(n, workers, 4096, func(i int) {
		wd, bt := q.bloomBit(keys[i])
		atomic.OrUint64(&q.bloom[wd], 1<<bt)
	})
	return q, group
}

// ---- candidate ids kept per group -------------------------------------------

type candSlot struct {
	mu          sync.Mutex
	lowMax      atomic.Uint32 // largest id in low, or max uint32 while low is not full
	highMin     atomic.Uint32 // smallest id in high, or 0 while high is not full
	nLow, nHigh uint8
	low, high   [candK]uint32
}

const slabChunk = 1 << 14

// candSlab hands out candSlots; chunks are allocated on demand so memory
// follows the number of groups that actually get a hit.
type candSlab struct {
	next   atomic.Uint32
	chunks []atomic.Pointer[[slabChunk]candSlot]
}

func (s *candSlab) init(maxSlots int) {
	s.chunks = make([]atomic.Pointer[[slabChunk]candSlot], maxSlots/slabChunk+1)
}

func (s *candSlab) alloc() (uint32, *candSlot) {
	i := s.next.Add(1) - 1
	ch := &s.chunks[i/slabChunk]
	p := ch.Load()
	if p == nil {
		np := new([slabChunk]candSlot)
		if ch.CompareAndSwap(nil, np) {
			p = np
		} else {
			p = ch.Load()
		}
	}
	c := &p[i%slabChunk]
	c.lowMax.Store(^uint32(0))
	return i + 1, c
}

func (s *candSlab) get(idx uint32) *candSlot {
	i := idx - 1
	return &s.chunks[i/slabChunk].Load()[i%slabChunk]
}

// offer records entry id as a candidate, keeping the candK lowest and the
// candK highest ids seen. The result depends only on the set of ids offered.
func (c *candSlot) offer(id uint32) {
	if id >= c.lowMax.Load() && id <= c.highMin.Load() {
		return // neither among the lowest nor the highest
	}
	c.mu.Lock()
	if c.nLow < candK {
		c.low[c.nLow] = id
		c.nLow++
	} else if m := maxIndex(c.low[:]); id < c.low[m] {
		c.low[m] = id
	}
	if c.nLow == candK {
		c.lowMax.Store(c.low[maxIndex(c.low[:])])
	}
	if c.nHigh < candK {
		c.high[c.nHigh] = id
		c.nHigh++
	} else if m := minIndex(c.high[:]); id > c.high[m] {
		c.high[m] = id
	}
	if c.nHigh == candK {
		c.highMin.Store(c.high[minIndex(c.high[:])])
	}
	c.mu.Unlock()
}

func maxIndex(a []uint32) int {
	m := 0
	for i, v := range a {
		if v > a[m] {
			m = i
		}
	}
	return m
}

func minIndex(a []uint32) int {
	m := 0
	for i, v := range a {
		if v < a[m] {
			m = i
		}
	}
	return m
}

// candidates returns the distinct ids held, ascending.
func (c *candSlot) candidates(dst []uint32) []uint32 {
	dst = append(dst[:0], c.low[:c.nLow]...)
	for _, v := range c.high[:c.nHigh] {
		dup := false
		for _, u := range dst {
			if u == v {
				dup = true
				break
			}
		}
		if !dup {
			dst = append(dst, v)
		}
	}
	for i := 1; i < len(dst); i++ { // insertion sort: at most 2*candK items
		for j := i; j > 0 && dst[j] < dst[j-1]; j-- {
			dst[j], dst[j-1] = dst[j-1], dst[j]
		}
	}
	return dst
}

// ---- running one level -------------------------------------------------------

// matchLevel fills lv.skip and lv.recipe for every node of the level.
func (e *encoder) matchLevel(lv *levelData) {
	s, pw := lv.s, e.pw
	pix, start := e.orig.Pix(), e.c.Start

	// 1. For each node, how many real cells differ from the start value.
	par.For(lv.ny, e.workers, 1, func(ny int) {
		for nx := 0; nx < lv.nx; nx++ {
			rx, ry := nx*s, ny*s
			rw, rh := max(0, min(s, e.w-rx)), max(0, min(s, e.h-ry))
			n := int32(0)
			for y := 0; y < rh; y++ {
				for _, v := range pix[(ry+y)*pw+rx : (ry+y)*pw+rx+rw] {
					if v != start {
						n++
					}
				}
			}
			lv.skip[ny*lv.nx+nx] = n
		}
	})
	if lv.numDomains() == 0 {
		return
	}
	var active []int32
	for i, n := range lv.skip {
		if n > 0 {
			active = append(active, int32(i))
		}
	}
	if len(active) == 0 {
		return
	}

	// 2. The query table over the nodes that still need a recipe.
	keys := make([]uint64, 2*len(active)) // item 2j: add key, 2j+1: xor key
	scratch := make([][]byte, par.Workers(e.workers))
	for i := range scratch {
		scratch[i] = make([]byte, s*s)
	}
	par.ForWorker(len(active), e.workers, 64, func(w, j int) {
		ni := int(active[j])
		rx, ry := (ni%lv.nx)*s, (ni/lv.nx)*s
		tgt := scratch[w]
		for y := 0; y < s; y++ {
			copy(tgt[y*s:(y+1)*s], pix[(ry+y)*pw+rx:(ry+y)*pw+rx+s])
		}
		keys[2*j], keys[2*j+1] = hashPair(tgt, nil)
	})
	q, group := buildQueryTable(keys, e.workers)
	keys = nil
	slots := 0
	for i := range q.shards {
		slots += int(q.shards[i].nGroups)
	}
	q.slab.init(slots)

	// 3. Stream every domain block, of each kind, past the table.
	for k, dk := range lv.kinds {
		nd, scale := dk.count(), dk.scale
		if nd == 0 {
			continue
		}
		par.ForWorker(nd, e.workers, 32, func(w, d int) {
			dx, dy := (d%dk.dxN)*dk.stride, (d/dk.dxN)*dk.stride
			dec := scratch[w]
			for py := 0; py < s; py++ {
				row := (dy+scale*py)*pw + dx
				for px := 0; px < s; px++ {
					dec[py*s+px] = pix[row+scale*px]
				}
			}
			for iso := 0; iso < transform.NumIsometries; iso++ {
				ka, kx := hashPair(dec, lv.perms[iso])
				id := uint32(k)<<kindShift | uint32(d)<<3 | uint32(iso)
				q.offerKey(ka, id)
				q.offerKey(kx, id)
			}
		})
	}

	// 4. Each node takes its group's first valid candidate.
	par.ForWorker(len(active), e.workers, 64, func(w, j int) {
		ni := int(active[j])
		rx, ry := (ni%lv.nx)*s, (ni/lv.nx)*s
		var buf [2 * candK]uint32
		for kind := 0; kind < 2; kind++ {
			g := group[2*j+kind]
			sh := &q.shards[g>>24]
			idx := sh.slot[g&0xffffff].Load()
			if idx == 0 {
				continue
			}
			for _, id := range q.slab.get(idx).candidates(buf[:0]) {
				if c, ok := e.verifyEntry(lv, rx, ry, id, kind == 1); ok {
					lv.recipe[ni], lv.recC[ni] = id<<1|uint32(kind), c
					break
				}
			}
			if lv.recipe[ni] != noRecipe {
				break // the add maps are tried before the xor maps
			}
		}
	})
}

const claiming = ^uint32(0) // a slot index being allocated by some worker

func (q *queryTable) offerKey(key uint64, id uint32) {
	shard, local, ok := q.find(key)
	if !ok {
		return
	}
	slot := &q.shards[shard].slot[local]
	idx := slot.Load()
	for idx == 0 || idx == claiming {
		if idx == 0 && slot.CompareAndSwap(0, claiming) {
			idx, _ = q.slab.alloc()
			slot.Store(idx)
			break
		}
		runtime.Gosched() // another worker is allocating this group's slot
		idx = slot.Load()
	}
	q.slab.get(idx).offer(id)
}

// verifyEntry checks a candidate (kind<<29 | domain<<3 | isometry): its domain
// must not overlap the node, and it must really reproduce the node under an
// add (or xor) map. A passing check returns the map's constant (0 = identity).
func (e *encoder) verifyEntry(lv *levelData, rx, ry int, id uint32, isXor bool) (uint8, bool) {
	s, pw := lv.s, e.pw
	dk := lv.kinds[id>>kindShift&1]
	d, iso := int(id>>3&(1<<26-1)), id&7
	dx, dy := (d%dk.dxN)*dk.stride, (d/dk.dxN)*dk.stride
	if overlaps(dx, dy, dk.scale*s, rx, ry, s) {
		return 0, false // a domain containing its own range block cannot anchor it
	}
	if dk.scale == 1 && !copyIsCausal(dx, dy, s, rx, ry) {
		return 0, false
	}
	pix := e.orig.Pix()
	perm := lv.perms[iso]
	cell := func(i int) byte { // output cell i of the oriented domain
		p := int(perm[i])
		return pix[(dy+dk.scale*(p/s))*pw+dx+dk.scale*(p%s)]
	}
	var c byte
	if isXor {
		c = pix[ry*pw+rx] ^ cell(0)
	} else {
		c = pix[ry*pw+rx] - cell(0)
	}
	for y := 0; y < s; y++ {
		for x := 0; x < s; x++ {
			t, g := cell(y*s+x), pix[(ry+y)*pw+rx+x]
			if isXor {
				t ^= c
			} else {
				t += c
			}
			if t != g {
				return 0, false // a hash collision, not a match
			}
		}
	}
	return c, true
}

// copyIsCausal reports whether the same-scale domain at (dx, dy) lies entirely
// before the range block at (rx, ry) in raster order: all its cells are in
// earlier rows, or in the block's first row and to its left. A same-scale copy
// may only read such data. Then every dependency points backwards, so there
// are no cycles (two identical blocks can never copy from each other): the first
// occurrence of a pattern is an anchor, later ones copy it, as in LZ77.
func copyIsCausal(dx, dy, s, rx, ry int) bool {
	last := dy + s - 1 // the domain's last row
	return last < ry || (last == ry && dx+s-1 < rx)
}

// recipeFor turns a node's stored result into a Transform.
func (e *encoder) recipeFor(lv *levelData, ni, rx, ry int) (Transform, bool) {
	r := lv.recipe[ni]
	if r == noRecipe {
		return Transform{}, false
	}
	id, isXor := r>>1, r&1 == 1
	dk := lv.kinds[id>>kindShift&1]
	d := int(id >> 3 & (1<<26 - 1))
	t := Transform{
		RX: rx, RY: ry, Size: lv.s,
		DX: (d % dk.dxN) * dk.stride, DY: (d / dk.dxN) * dk.stride,
		Iso:       transform.Isometry(id & 7),
		SameScale: dk.scale == 1,
	}
	if c := lv.recC[ni]; c != 0 {
		if isXor {
			t.Map = transform.ValueMap{Kind: transform.MapXor, C: c}
		} else {
			t.Map = transform.ValueMap{Kind: transform.MapAdd, C: c}
		}
	}
	return t, true
}
