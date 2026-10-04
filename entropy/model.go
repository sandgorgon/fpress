package entropy

import (
	"math/bits"
	"sync"
)

const (
	numOrders = 6 // orders 0, 1, 2, 3, 4, 6
	num2D     = 3 // contexts built from the row above (only when stride > 0)
	maxCtx    = numOrders + num2D
	maxIn     = maxCtx + 2 // + match model + bias
	minMatch  = 5
	maxMatch  = 65535
	apmRate   = 7
	maxA2Bits = 12 // contexts of the second final-stage table (more cost time and cache for ~0.04%)
)

// Tuned on a small corpus of text, code, tables, gradients and a self-similar
// pattern; the values were flat around these.
const (
	counterMax = 60    // adaptation floor: rate -> 1/(counterMax+1.5)
	mixLR      = 72    // mixer learning rate
	initW      = 12000 // initial mixer weight (65536 = 1.0)
	probClamp  = 8     // keep final probabilities in [probClamp, 65536-probClamp]
)

// model predicts the next bit from everything coded so far.
type model struct {
	tb     int // log2 of the counter table size
	stride int
	buf    []byte // all bytes of the stream; only buf[:pos] is ever read
	pos    int
	c0     int // bits of the current byte so far, with a leading 1
	bitPos int

	nctx  int
	tab   [maxCtx][]uint32 // hashed counters: 22-bit probability, 10-bit count
	shift uint
	hash  [maxCtx]uint32 // context hash for the current byte
	base  [maxCtx]uint32 // start of the 16-counter bucket for the current nibble
	idx   [maxCtx]uint32 // counter used for the current bit

	nin int
	in  [maxIn]int32 // stretched predictions fed to the mixer

	w      []int32 // mixer weights, one set per (partial byte, match state)
	wbase  int
	mixOut int // probability the mixer produced
	mixX   int // ...and its stretch

	a1, a2 apm
	a2bits int

	// match model
	mt        []int32 // hash of the last minMatch bytes -> position after them
	mshift    uint
	mptr      int
	mlen      int
	mpred     int // predicted next byte, -1 if none
	mexp      int // expected value of the current bit, -1 if none
	mctx      int
	mtab      [64 * 2]uint32
	finalProb int
}

func tableBits(n int) int { return min(max(bits.Len(uint(n))+1, 12), 21) }

// The tables dominate a model's memory (about 47 MB for a 256 KiB input), and a
// parallel caller builds one per segment, so finished models go back into a
// pool by table size and are reset for the next stream instead of being
// reallocated. reset restores exactly the state a new model starts in; tests
// check that a reused model produces the same bytes as a fresh one.
var modelPools [22]sync.Pool

func acquireModel(buf []byte, n, stride int) *model {
	tb := tableBits(n)
	var m *model
	if v := modelPools[tb].Get(); v != nil {
		m = v.(*model)
	} else {
		m = &model{tb: tb}
	}
	m.reset(buf, n, stride)
	return m
}

func (m *model) release() {
	m.buf = nil
	modelPools[m.tb].Put(m)
}

// newModel builds a model that is never pooled (used by tests).
func newModel(buf []byte, n, stride int) *model {
	m := &model{tb: tableBits(n)}
	m.reset(buf, n, stride)
	return m
}

// reset puts m into the state of a brand-new model for a stream of n bytes.
func (m *model) reset(buf []byte, n, stride int) {
	tablesOnce.Do(initTables)
	tab, mt, w, a1, a2 := m.tab, m.mt, m.w, m.a1, m.a2 // keep the big allocations
	*m = model{tb: m.tb, stride: stride, buf: buf, c0: 1, mpred: -1, mexp: -1}
	m.tab, m.mt, m.w, m.a1, m.a2 = tab, mt, w, a1, a2

	m.nctx = numOrders
	if stride > 0 {
		m.nctx += num2D
	}
	m.shift = uint(32 - m.tb)
	for i := 0; i < m.nctx; i++ {
		if m.tab[i] == nil {
			m.tab[i] = make([]uint32, 1<<m.tb)
		} else {
			clear(m.tab[i])
		}
	}
	m.nin = m.nctx + 2
	if need := 256 * 3 * m.nin; cap(m.w) < need {
		m.w = make([]int32, need)
	} else {
		m.w = m.w[:need]
	}
	for i := range m.w {
		m.w[i] = initW
	}
	if m.mt == nil {
		m.mt = make([]int32, 1<<m.tb)
	} else {
		clear(m.mt)
	}
	m.mshift = m.shift
	m.a1.init(256)
	m.a2bits = min(max(bits.Len(uint(n)), 8), maxA2Bits) // fewer contexts for small inputs: cheaper to set up
	m.a2.init(1 << m.a2bits)
	m.setContexts()
}

func (m *model) at(back int) uint32 { // the byte `back` positions ago, 0 before the start
	if back < 1 || m.pos-back < 0 {
		return 0
	}
	return uint32(m.buf[m.pos-back])
}

func hashStep(h, v uint32) uint32 {
	h = (h ^ v) * 0x9E3779B1
	return h ^ h>>15
}

// setContexts computes the context hashes for the next byte.
func (m *model) setContexts() {
	h := uint32(0)
	m.hash[0] = 0
	order := 1
	for _, upTo := range [...]int{1, 2, 3, 4, 6} {
		for ; order <= upTo; order++ {
			h = hashStep(h, m.at(order)+1)
		}
		m.hash[numOrdersIndex(upTo)] = h + uint32(upTo)*0x1000193
	}
	if m.stride > 0 {
		a := m.at(m.stride)
		l := m.at(1)
		al := m.at(m.stride + 1)
		ar := uint32(0)
		if m.pos-m.stride+1 >= 0 && m.stride > 1 {
			ar = m.at(m.stride - 1)
		}
		m.hash[numOrders] = hashStep(hashStep(0x51ED27, a), 1)
		m.hash[numOrders+1] = hashStep(hashStep(hashStep(0x7F4A7C15, a), l), 2)
		m.hash[numOrders+2] = hashStep(hashStep(hashStep(hashStep(0x2F0B4A27, a), al), ar), 3)
	}
}

func numOrdersIndex(upTo int) int {
	switch upTo {
	case 1:
		return 1
	case 2:
		return 2
	case 3:
		return 3
	case 4:
		return 4
	}
	return 5 // order 6
}

func counterP16(c uint32) int { return int(((c >> 10) ^ (1 << 21)) >> 6) }

func updateCounter(c *uint32, bit int, limit uint32) {
	v := *c
	n := v & 1023
	p := int32((v >> 10) ^ (1 << 21))
	target := int32(0)
	if bit != 0 {
		target = 1<<22 - 1
	}
	p += int32((int64(target-p) * int64(rateTab[n])) >> 16)
	if n < limit {
		n++
	}
	*c = (uint32(p)^(1<<21))<<10 | n
}

// predict returns P(next bit is 1) as a 16-bit probability.
func (m *model) predict() int {
	// Counters live in 16-entry buckets (one cache line) chosen once per
	// nibble from the context hash and the bits of the byte seen so far; the
	// bits within the nibble then pick the entry.
	if m.bitPos == 0 || m.bitPos == 4 {
		salt := uint32(m.c0) * 0x2545F491
		for i := 0; i < m.nctx; i++ {
			m.base[i] = (((m.hash[i] ^ salt) * 0x9E3779B1) >> m.shift) &^ 15
		}
	}
	nib := uint32(m.c0)
	if m.bitPos >= 4 {
		nib = 1<<uint(m.bitPos-4) | uint32(m.c0)&(1<<uint(m.bitPos-4)-1)
	}
	k := 0
	for i := 0; i < m.nctx; i++ {
		ix := m.base[i] + nib
		m.idx[i] = ix
		m.in[k] = int32(stretch(counterP16(m.tab[i][ix])))
		k++
	}

	m.mexp = -1
	if m.mlen > 0 && m.mpred >= 0 && (m.mpred|256)>>(8-m.bitPos) == m.c0 {
		m.mexp = (m.mpred >> (7 - m.bitPos)) & 1
	}
	mstate := 0
	if m.mexp >= 0 {
		m.mctx = min(m.mlen, 63)*2 + m.mexp
		m.in[k] = int32(stretch(counterP16(m.mtab[m.mctx])))
		mstate = 1
		if m.mlen >= 16 {
			mstate = 2
		}
	} else {
		m.in[k] = 0
	}
	k++
	m.in[k] = 256 // bias

	m.wbase = (m.c0 + 256*mstate) * m.nin
	w := m.w[m.wbase : m.wbase+m.nin]
	var dot int64
	for i := 0; i < m.nin; i++ {
		dot += int64(w[i]) * int64(m.in[i])
	}
	m.mixX = min(max(int(dot>>16), -stMax), stMax)
	m.mixOut = squash(m.mixX)

	c1 := int(m.at(1))
	p1 := m.a1.refine(m.mixX, m.c0)
	p2 := m.a2.refine(m.mixX, (m.c0|c1<<8)&(1<<m.a2bits-1))
	p := (2*m.mixOut + p1 + p2*5 + 4) >> 3
	m.finalProb = min(max(p, probClamp), 65536-probClamp)
	return m.finalProb
}

// update learns from the bit that was actually coded.
func (m *model) update(bit int) {
	for i := 0; i < m.nctx; i++ {
		updateCounter(&m.tab[i][m.idx[i]], bit, counterMax)
	}
	if m.mexp >= 0 {
		updateCounter(&m.mtab[m.mctx], bit, counterMax)
		if bit != m.mexp {
			m.mlen = 0
		}
	}

	err := int64((bit << 16) - m.mixOut)
	w := m.w[m.wbase : m.wbase+m.nin]
	for i := 0; i < m.nin; i++ {
		w[i] += int32((int64(m.in[i])*err*mixLR + 1<<19) >> 20)
	}
	m.a1.update(bit)
	m.a2.update(bit)

	m.c0 = m.c0<<1 | bit
	m.bitPos++
	if m.bitPos == 8 {
		m.byteDone()
	}
}

// byteDone runs after the last bit of a byte: the byte has been stored in
// buf[pos] by the caller, so advance and prepare the next byte's contexts.
func (m *model) byteDone() {
	m.pos++
	m.c0, m.bitPos = 1, 0
	m.setContexts()

	if m.mlen > 0 {
		m.mptr++
		if m.mlen < maxMatch {
			m.mlen++
		}
	}
	if m.pos >= minMatch {
		var h uint32
		for i := 1; i <= minMatch; i++ {
			h = hashStep(h, m.at(i))
		}
		h = (h * 0x9E3779B1) >> m.mshift
		if m.mlen == 0 {
			if cand := int(m.mt[h]); cand > 0 {
				l := 0
				for l < 32 && cand-1-l >= 0 && m.buf[cand-1-l] == m.buf[m.pos-1-l] {
					l++
				}
				if l >= minMatch {
					m.mptr, m.mlen = cand, l
				}
			}
		}
		m.mt[h] = int32(m.pos)
	}
	m.mpred = -1
	if m.mlen > 0 {
		m.mpred = int(m.buf[m.mptr])
	}
}

// apm refines a probability given a small context: it maps the stretched
// input, interpolating between 33 bins per context, to a learned output.
type apm struct {
	t   []uint16
	idx int
}

func (a *apm) init(contexts int) {
	if need := contexts * 33; cap(a.t) < need {
		a.t = make([]uint16, need)
	} else {
		a.t = a.t[:need]
	}
	a.idx = 0
	var row [33]uint16
	for j := range row {
		row[j] = uint16(min(squash((j-16)*192), 65535))
	}
	for c := 0; c < contexts; c++ {
		copy(a.t[c*33:], row[:])
	}
}

func (a *apm) refine(x, cx int) int {
	pos := x + stMax + 1 // 1 .. 6143
	lo, w := pos/192, pos%192
	i := cx*33 + lo
	a.idx = i
	if w >= 96 {
		a.idx = i + 1
	}
	return (int(a.t[i])*(192-w) + int(a.t[i+1])*w) / 192
}

func (a *apm) update(bit int) {
	g := bit<<16 + bit<<apmRate - bit - bit
	a.t[a.idx] = uint16(int(a.t[a.idx]) + (g-int(a.t[a.idx]))>>apmRate)
}
