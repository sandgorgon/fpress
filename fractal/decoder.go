package fractal

import (
	"bytes"
	"compress/flate"
	"errors"
	"fmt"
	"io"

	"fpress/entropy"
	"fpress/matrix"
)

// Decode reconstructs the original matrix exactly.
func Decode(c *Compressed) (*matrix.Dense, error) { return decode(c, 0) }

func decode(c *Compressed, workers int) (*matrix.Dense, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	dst := matrix.NewDense(c.Width, c.Height)
	if err := decodeInto(dst, c, workers); err != nil {
		return nil, err
	}
	return dst, nil
}

// DecodeInto reconstructs the original matrix into any MutableMatrix, which
// must have the original's dimensions.
func DecodeInto(dst matrix.MutableMatrix, c *Compressed) error { return decodeInto(dst, c, 0) }

// decodeInto is DecodeInto with an explicit goroutine count (0 = all CPUs).
func decodeInto(dst matrix.MutableMatrix, c *Compressed, workers int) error {
	if err := c.validate(); err != nil {
		return err
	}
	if dst.Width() != c.Width || dst.Height() != c.Height {
		return fmt.Errorf("fractal: destination is %dx%d, stream describes %dx%d",
			dst.Width(), dst.Height(), c.Width, c.Height)
	}
	if c.Width == 0 || c.Height == 0 {
		return nil // no cells to write, however large the other dimension claims to be
	}
	res, err := decodeResidual(c, workers)
	if err != nil {
		return err
	}
	a := reconstruct(c, workers)
	for y := 0; y < c.Height; y++ {
		row := a.Row(y)
		for x := 0; x < c.Width; x++ {
			dst.Set(x, y, row[x]^res[y*c.Width+x])
		}
	}
	return nil
}

// decodeResidual returns the XOR residual (Width*Height bytes, row-major). The
// first byte of c.Residual says how the rest is stored.
func decodeResidual(c *Compressed, workers int) ([]byte, error) {
	n := c.Width * c.Height
	if n == 0 {
		return nil, nil
	}
	if len(c.Residual) < 1 {
		return nil, errors.New("fractal: residual: missing")
	}
	mode, body := c.Residual[0], c.Residual[1:]
	var res []byte
	var err error
	switch mode {
	case residualZero:
		if len(body) != 0 {
			return nil, errors.New("fractal: residual: data after an all-zero marker")
		}
		return make([]byte, n), nil
	case residualFlate:
		res, err = inflateExact(body, n)
	case residualCM:
		res, err = entropy.DecodeChunked(body, n, c.Width, workers)
	default:
		return nil, fmt.Errorf("fractal: residual: unknown mode %d", mode)
	}
	if err != nil {
		return nil, fmt.Errorf("fractal: residual: %w", err)
	}
	return res, nil
}

// inflateExact decodes a DEFLATE stream that must hold exactly n bytes and end
// cleanly: truncated, overlong and trailing-garbage streams are all errors.
func inflateExact(data []byte, n int) ([]byte, error) {
	src := bytes.NewReader(data) // a ByteReader, so flate never reads ahead of what it uses
	r := flate.NewReader(src)
	defer r.Close()
	out := make([]byte, n)
	if _, err := io.ReadFull(r, out); err != nil {
		return nil, err
	}
	var extra [1]byte
	if k, err := r.Read(extra[:]); k != 0 {
		return nil, errors.New("longer than expected")
	} else if err != io.EOF {
		return nil, io.ErrUnexpectedEOF
	}
	if src.Len() != 0 {
		return nil, fmt.Errorf("%d bytes after the end of the stream", src.Len())
	}
	return out, nil
}
