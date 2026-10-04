// Package fractal implements lossless fractal-style compression of a byte
// matrix: recipes (transforms) build an approximation A of the matrix by
// iteration, and an XOR residual (original ^ A) makes reconstruction exact.
//
// The decoder is deterministic and uses integer byte operations only, so the
// encoder can run it to learn exactly what A the decoder will produce.
package fractal

import (
	"fmt"

	"fpress/transform"
)

// Transform is one recipe for a Size x Size range block of the padded grid.
type Transform struct {
	RX, RY int // top-left of the range block being produced
	Size   int // range block side; a domain block is 2*Size on a side

	// Raw transforms store the block literally (an "anchor"): Data holds
	// Size*Size row-major bytes. DX, DY, Iso and Map are unused.
	Raw  bool
	Data []byte

	// Otherwise the block is: take the domain at (DX, DY), apply Iso, then
	// apply Map. The domain is 2*Size on a side and is decimated to Size (every
	// other cell) unless SameScale, when it is Size on a side and copied as it
	// is: a plain 2D copy, for repeated tiles, glyphs and records.
	DX, DY    int
	SameScale bool
	Iso       transform.Isometry
	Map       transform.ValueMap
}

// domainSide is the side of the domain block this recipe reads.
func (t *Transform) domainSide() int {
	if t.SameScale {
		return t.Size
	}
	return 2 * t.Size
}

// Compressed is the in-memory form of a compressed matrix. Binary
// serialization arrives with the container format (milestone 4).
type Compressed struct {
	Width, Height int // dimensions of the original matrix
	Block         int // grid unit; the working grid is padded to a multiple of it
	Iterations    int // number of decoder rounds (fixed, so decoding is deterministic)
	Start         byte
	Transforms    []Transform
	Residual      []byte // XOR residual over the Width x Height cells, row-major: a mode byte, then the data
}

// Residual storage modes (the first byte of Compressed.Residual).
const (
	residualZero  = 0 // every cell matched: nothing follows
	residualFlate = 1 // DEFLATE stream
	residualCM    = 2 // context-mixing stream, row length = Width

	// Past this many cells the context-mixing coder (about 1 MB/s per core) is not tried.
	maxCMResidual = 1 << 27
)

const (
	maxCells = 1 << 30
	maxBlock = 1 << 16
)

func roundUp(n, unit int) int { return (n + unit - 1) / unit * unit }

func (c *Compressed) paddedDims() (pw, ph int) {
	return roundUp(c.Width, c.Block), roundUp(c.Height, c.Block)
}

// validate checks every field the decoder relies on, so malformed input is an
// error rather than a panic.
func (c *Compressed) validate() error {
	if c.Width < 0 || c.Height < 0 {
		return fmt.Errorf("fractal: negative dimensions %dx%d", c.Width, c.Height)
	}
	if c.Block < 1 || c.Block > maxBlock {
		return fmt.Errorf("fractal: invalid block size %d", c.Block)
	}
	if c.Iterations < 1 {
		return fmt.Errorf("fractal: invalid iteration count %d", c.Iterations)
	}
	if c.Width > maxCells || c.Height > maxCells {
		return fmt.Errorf("fractal: dimensions %dx%d too large", c.Width, c.Height)
	}
	pw, ph := c.paddedDims()
	if pw != 0 && ph > maxCells/pw {
		return fmt.Errorf("fractal: padded grid %dx%d too large", pw, ph)
	}
	for i := range c.Transforms {
		t := &c.Transforms[i]
		if t.Size < 1 || t.Size > pw || t.Size > ph {
			return fmt.Errorf("fractal: transform %d: invalid size %d", i, t.Size)
		}
		if t.RX < 0 || t.RY < 0 || t.RX > pw-t.Size || t.RY > ph-t.Size {
			return fmt.Errorf("fractal: transform %d: range block (%d,%d) size %d outside %dx%d grid",
				i, t.RX, t.RY, t.Size, pw, ph)
		}
		if t.Raw {
			if len(t.Data) != t.Size*t.Size {
				return fmt.Errorf("fractal: transform %d: raw data has %d bytes, want %d",
					i, len(t.Data), t.Size*t.Size)
			}
			if t.SameScale {
				return fmt.Errorf("fractal: transform %d: a raw block has no domain", i)
			}
			continue
		}
		d := t.domainSide()
		if d > pw || d > ph || t.DX < 0 || t.DY < 0 || t.DX > pw-d || t.DY > ph-d {
			return fmt.Errorf("fractal: transform %d: domain block (%d,%d) size %d outside %dx%d grid",
				i, t.DX, t.DY, d, pw, ph)
		}
		if t.Iso >= transform.NumIsometries {
			return fmt.Errorf("fractal: transform %d: invalid isometry %d", i, t.Iso)
		}
		if !t.Map.Valid() {
			return fmt.Errorf("fractal: transform %d: invalid value map %+v", i, t.Map)
		}
	}
	return checkDisjoint(c.Transforms, pw, ph)
}

// ApproxSize is a rough estimate of the serialized size in bytes (header, a
// fixed cost per recipe, literal bytes for raw blocks, and the compressed
// residual). It exists so milestone 3 can compare against plain flate before
// the real container format lands.
func (c *Compressed) ApproxSize() int {
	n := 16
	for i := range c.Transforms {
		if c.Transforms[i].Raw {
			n += 5 + len(c.Transforms[i].Data)
		} else {
			n += 7
		}
	}
	return n + len(c.Residual)
}
