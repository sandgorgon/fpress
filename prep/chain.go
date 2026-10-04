// Package prep holds reversible matrix operations that rearrange bytes so that
// repetition becomes easier for later stages (flate, the fractal stage) to
// find. Every step is exact: Inverse(Forward(m)) is m, byte for byte.
//
// A Chain is an ordered list of steps. Steps may change the matrix shape, so
// the original dimensions are needed to invert a chain.
package prep

import (
	"errors"
	"fmt"
	"strings"

	"fpress/matrix"
)

// Kind identifies a step type. The numeric values are part of the stream
// format.
type Kind uint8

const (
	// DeltaUpSub replaces each row with (row - row above) mod 256.
	DeltaUpSub Kind = iota + 1
	// DeltaUpXor replaces each row with (row ^ row above).
	DeltaUpXor
	// DeltaLeftSub replaces each cell with (cell - cell to its left) mod 256.
	DeltaLeftSub
	// DeltaLeftXor replaces each cell with (cell ^ cell to its left).
	DeltaLeftXor
	// BitPlanes splits every byte into its 8 bits and gathers bit p of
	// every cell into plane p, packing 8 neighbours per output byte. The
	// planes are stacked vertically: w x h becomes (w/8) x (8h). Needs w % 8 == 0.
	BitPlanes
	// Deinterleave gathers every K-th column into its own plane and stacks
	// the planes: w x h becomes (w/K) x (K*h). Needs w % K == 0. It separates,
	// for example, the high and low bytes of 16-bit samples.
	Deinterleave
)

const (
	MaxSteps = 8  // longest chain accepted
	MinK     = 2  // Deinterleave stride bounds
	MaxK     = 16 //
)

// Step is one operation. K is used only by Deinterleave.
type Step struct {
	Kind Kind
	K    int
}

func (s Step) String() string {
	switch s.Kind {
	case DeltaUpSub:
		return "up-sub"
	case DeltaUpXor:
		return "up-xor"
	case DeltaLeftSub:
		return "left-sub"
	case DeltaLeftXor:
		return "left-xor"
	case BitPlanes:
		return "bitplanes"
	case Deinterleave:
		return fmt.Sprintf("deint%d", s.K)
	}
	return fmt.Sprintf("kind(%d)", s.Kind)
}

func (s Step) check() error {
	if s.Kind < DeltaUpSub || s.Kind > Deinterleave {
		return fmt.Errorf("prep: unknown step kind %d", s.Kind)
	}
	if s.Kind == Deinterleave {
		if s.K < MinK || s.K > MaxK {
			return fmt.Errorf("prep: deinterleave stride %d outside [%d,%d]", s.K, MinK, MaxK)
		}
	} else if s.K != 0 {
		return fmt.Errorf("prep: step %v takes no parameter", s)
	}
	return nil
}

// outDims returns the shape after the step, or an error if the step cannot
// apply to a w x h matrix.
func (s Step) outDims(w, h int) (int, int, error) {
	switch s.Kind {
	case BitPlanes:
		if w%8 != 0 {
			return 0, 0, fmt.Errorf("prep: bitplanes needs a width divisible by 8, got %d", w)
		}
		return w / 8, h * 8, nil
	case Deinterleave:
		if w%s.K != 0 {
			return 0, 0, fmt.Errorf("prep: deint%d needs a width divisible by %d, got %d", s.K, s.K, w)
		}
		return w / s.K, h * s.K, nil
	}
	return w, h, nil
}

// Chain is an ordered list of steps; the empty chain does nothing.
type Chain []Step

func (c Chain) String() string {
	if len(c) == 0 {
		return "none"
	}
	parts := make([]string, len(c))
	for i, s := range c {
		parts[i] = s.String()
	}
	return strings.Join(parts, "+")
}

// Equal reports whether two chains contain the same steps.
func (c Chain) Equal(o Chain) bool {
	if len(c) != len(o) {
		return false
	}
	for i := range c {
		if c[i] != o[i] {
			return false
		}
	}
	return true
}

// OutDims returns the shape of a w x h matrix after the whole chain, or an
// error if any step cannot apply.
func (c Chain) OutDims(w, h int) (int, int, error) {
	if len(c) > MaxSteps {
		return 0, 0, fmt.Errorf("prep: chain has %d steps, limit %d", len(c), MaxSteps)
	}
	if w < 1 || h < 1 {
		return 0, 0, errors.New("prep: matrix must be non-empty")
	}
	for _, s := range c {
		if err := s.check(); err != nil {
			return 0, 0, err
		}
		var err error
		if w, h, err = s.outDims(w, h); err != nil {
			return 0, 0, err
		}
	}
	return w, h, nil
}

// Forward applies the chain. The result may alias m when the chain is empty
// and m is a *matrix.Dense; callers must not modify it in that case.
func (c Chain) Forward(m matrix.Matrix) (*matrix.Dense, error) {
	if _, _, err := c.OutDims(m.Width(), m.Height()); err != nil {
		return nil, err
	}
	cur := toDense(m)
	for _, s := range c {
		cur = s.forward(cur)
	}
	return cur, nil
}

// Inverse undoes Forward. m is the prepared matrix and (w, h) the dimensions
// of the matrix that was originally passed to Forward.
func (c Chain) Inverse(m matrix.Matrix, w, h int) (*matrix.Dense, error) {
	ow, oh, err := c.OutDims(w, h)
	if err != nil {
		return nil, err
	}
	if m.Width() != ow || m.Height() != oh {
		return nil, fmt.Errorf("prep: prepared matrix is %dx%d, chain on %dx%d gives %dx%d",
			m.Width(), m.Height(), w, h, ow, oh)
	}
	// Dimensions going into each step, so each can be inverted in turn.
	dims := make([][2]int, len(c))
	cw, ch := w, h
	for i, s := range c {
		dims[i] = [2]int{cw, ch}
		cw, ch, _ = s.outDims(cw, ch)
	}
	cur := toDense(m)
	for i := len(c) - 1; i >= 0; i-- {
		cur = c[i].inverse(cur, dims[i][0], dims[i][1])
	}
	return cur, nil
}

func toDense(m matrix.Matrix) *matrix.Dense {
	if d, ok := m.(*matrix.Dense); ok {
		return d
	}
	d := matrix.NewDense(m.Width(), m.Height())
	matrix.Copy(d, m)
	return d
}

// ---- stream form -----------------------------------------------------------

// AppendBinary appends the chain: a count byte, then per step a kind byte
// (followed by K for Deinterleave).
func (c Chain) AppendBinary(b []byte) []byte {
	b = append(b, byte(len(c)))
	for _, s := range c {
		b = append(b, byte(s.Kind))
		if s.Kind == Deinterleave {
			b = append(b, byte(s.K))
		}
	}
	return b
}

// ParseChain reads a chain from the front of b and returns it with the number
// of bytes consumed.
func ParseChain(b []byte) (Chain, int, error) {
	if len(b) < 1 {
		return nil, 0, errors.New("prep: truncated chain")
	}
	n, pos := int(b[0]), 1
	if n > MaxSteps {
		return nil, 0, fmt.Errorf("prep: chain has %d steps, limit %d", n, MaxSteps)
	}
	var c Chain
	for i := 0; i < n; i++ {
		if pos >= len(b) {
			return nil, 0, errors.New("prep: truncated chain")
		}
		s := Step{Kind: Kind(b[pos])}
		pos++
		if s.Kind == Deinterleave {
			if pos >= len(b) {
				return nil, 0, errors.New("prep: truncated chain")
			}
			s.K = int(b[pos])
			pos++
		}
		if err := s.check(); err != nil {
			return nil, 0, err
		}
		c = append(c, s)
	}
	return c, pos, nil
}
