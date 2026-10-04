package fractal

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Binary layout of a Compressed (MarshalBinary / UnmarshalBinary). Every
// integer is an unsigned LEB128 varint unless noted.
//
//	width height block iterations
//	start                      1 byte
//	nTransforms
//	len(tblob) tblob           the transform records, see records.go (columns, delta-coded
//	                           positions, stored with the smallest of stored/DEFLATE/cm)
//	len(residual) residual     the XOR residual, see Compressed.Residual
//
// Parsing never trusts a length before checking it against what is actually
// available, so a malformed stream gives an error, not a panic or a huge
// allocation. (UnmarshalBinary alone still permits a large claimed matrix; a
// caller that needs a tighter bound, like package codec, checks it.)

const (
	maxIterations     = 1 << 10
	maxTransformBytes = 27 // flags + five 5-byte varints + map constant
)

func appendUvarint(b []byte, v int) []byte { return binary.AppendUvarint(b, uint64(v)) }

func deflate(b []byte) []byte {
	var buf bytes.Buffer
	fw, _ := flate.NewWriter(&buf, flate.BestCompression)
	fw.Write(b) // a bytes.Buffer never fails
	fw.Close()
	return buf.Bytes()
}

// MarshalBinary serializes c.
func (c *Compressed) MarshalBinary() ([]byte, error) { return c.marshal(0) }

// marshal is MarshalBinary with an explicit goroutine count (0 = all CPUs).
func (c *Compressed) marshal(workers int) ([]byte, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	if c.Iterations > maxIterations {
		return nil, fmt.Errorf("fractal: %d iterations exceeds the format limit %d", c.Iterations, maxIterations)
	}
	packed := packBlob(encodeRecords(c.Transforms), workers)

	var out []byte
	for _, v := range []int{c.Width, c.Height, c.Block, c.Iterations} {
		out = binary.AppendUvarint(out, uint64(v))
	}
	out = append(out, c.Start)
	out = binary.AppendUvarint(out, uint64(len(c.Transforms)))
	out = binary.AppendUvarint(out, uint64(len(packed)))
	out = append(out, packed...)
	out = binary.AppendUvarint(out, uint64(len(c.Residual)))
	out = append(out, c.Residual...)
	return out, nil
}

// UnmarshalBinary parses data into c, replacing its contents. It validates the
// result, so a nil error means c can be passed to Decode.
func (c *Compressed) UnmarshalBinary(data []byte) error { return c.unmarshal(data, 0) }

// unmarshal is UnmarshalBinary with an explicit goroutine count (0 = all CPUs).
func (c *Compressed) unmarshal(data []byte, workers int) error {
	p := &reader{r: bytes.NewReader(data)}
	var n Compressed
	n.Width = p.uvarint(maxCells)
	n.Height = p.uvarint(maxCells)
	n.Block = p.uvarint(maxBlock)
	n.Iterations = p.uvarint(maxIterations)
	n.Start = p.byte()
	count := p.uvarint(maxCells)
	packed := p.bytes(p.uvarint(maxCells))
	n.Residual = p.bytes(p.uvarint(maxCells))
	if p.err != nil {
		return p.err
	}
	if p.r.Len() != 0 {
		return errors.New("fractal: trailing bytes after stream")
	}
	if err := n.validate(); err != nil { // dimensions, before any transform
		return err
	}
	pw, ph := n.paddedDims()
	if int64(count) > int64(pw)*int64(ph) {
		return fmt.Errorf("fractal: %d transforms cannot fit a %dx%d grid", count, pw, ph)
	}

	limit := int64(count)*maxTransformBytes + int64(pw)*int64(ph) + 64
	blob, err := unpackBlob(packed, limit, workers)
	if err != nil {
		return err
	}
	ts, err := decodeRecords(blob, count, pw, ph)
	if err != nil {
		return err
	}
	n.Transforms = ts
	if err := n.validate(); err != nil {
		return err
	}
	*c = n
	return nil
}

// reader is a bounds-checked, sticky-error byte reader.
type reader struct {
	r   *bytes.Reader
	err error
}

func (p *reader) fail(err error) {
	if p.err == nil {
		p.err = err
	}
}

func (p *reader) uvarint(limit int) int { return int(p.uvarint64(uint64(limit))) }

// uvarint64 reads a varint of at most limit; on any problem it records the
// error and returns 0.
func (p *reader) uvarint64(limit uint64) uint64 {
	if p.err != nil {
		return 0
	}
	v, err := binary.ReadUvarint(p.r)
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		p.fail(fmt.Errorf("fractal: %w", err))
		return 0
	}
	if v > limit {
		p.fail(fmt.Errorf("fractal: value %d exceeds limit %d", v, limit))
		return 0
	}
	return v
}

func (p *reader) byte() byte {
	if p.err != nil {
		return 0
	}
	b, err := p.r.ReadByte()
	if err != nil {
		p.fail(fmt.Errorf("fractal: %w", io.ErrUnexpectedEOF))
		return 0
	}
	return b
}

// bytes returns the next n bytes, checking n against what remains first.
func (p *reader) bytes(n int) []byte {
	if p.err != nil {
		return nil
	}
	if n > p.r.Len() {
		p.fail(fmt.Errorf("fractal: need %d bytes, %d remain: %w", n, p.r.Len(), io.ErrUnexpectedEOF))
		return nil
	}
	b := make([]byte, n)
	io.ReadFull(p.r, b)
	return b
}
