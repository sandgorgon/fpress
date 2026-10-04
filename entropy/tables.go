// Package entropy is a context-mixing arithmetic coder for bytes.
//
// Each byte is coded as 8 binary decisions. For every decision, several
// models (previous bytes of various orders, a long-match model, and, when the
// data is a matrix, the neighbours above) each predict the bit; a small
// learned mixer blends the predictions and a final adaptive stage sharpens
// the result; a binary arithmetic coder turns that probability into bits.
//
// Encoder and decoder run the same model, so everything here is integer
// arithmetic: no floating point at all, not even to build the tables. That
// matters because a float result can differ between CPU architectures (fused
// multiply-add, library exp) and a one-bit difference in a probability would
// make a stream undecodable on another machine.
package entropy

import (
	"math/bits"
	"sync"
)

// Probabilities are 16-bit (1..65535 = P(bit is 1) scaled by 65536). The
// "stretch" domain is the logit ln(p/(1-p)) in units of 1/256, limited to
// +-stMax, so a stretch value of 256 means odds of e to 1.
const (
	stMax    = 3071
	squashN  = 2*stMax + 1
	expStep  = 4311777323 // e^(1/256) in Q32 fixed point
	probBits = 16
)

var (
	tablesOnce sync.Once
	squashTab  [squashN]uint16 // index x+stMax -> probability
	stretchTab [1 << probBits]int16
	rateTab    [1024]int32 // adaptive learning rates: 65536 / (n + 1.5)
)

func initTables() {
	// e^(k/256) in Q32 by repeated fixed-point multiplication.
	var ex [stMax + 1]uint64
	ex[0] = 1 << 32
	for k := 0; k < stMax; k++ {
		hi, lo := bits.Mul64(ex[k], expStep)
		ex[k+1] = hi<<32 | lo>>32
	}
	for x := 0; x <= stMax; x++ {
		// p = 65536 * e^x / (1 + e^x)
		hi, lo := bits.Mul64(ex[x], 1<<probBits)
		q, _ := bits.Div64(hi, lo, ex[x]+(1<<32))
		p := min(max(int(q), 1), 65535)
		squashTab[stMax+x] = uint16(p)
		squashTab[stMax-x] = uint16(min(max(65536-p, 1), 65535))
	}
	// stretch is the inverse of squash: the smallest x whose squash reaches p.
	p := 0
	for x := -stMax; x <= stMax; x++ {
		v := int(squashTab[stMax+x])
		for p <= v && p < len(stretchTab) {
			stretchTab[p] = int16(x)
			p++
		}
	}
	for ; p < len(stretchTab); p++ {
		stretchTab[p] = stMax
	}
	for n := range rateTab {
		rateTab[n] = int32(131072 / (2*n + 3))
	}
}

func squash(x int) int {
	return int(squashTab[min(max(x, -stMax), stMax)+stMax])
}

func stretch(p16 int) int { return int(stretchTab[p16]) }
