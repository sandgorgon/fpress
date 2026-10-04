package fractal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"fpress/entropy"
	"fpress/transform"
)

// Transform records are stored column by column rather than record by record,
// so that bytes of the same kind sit together (the coders then see long runs
// of similar values), and positions are stored as differences from the
// previous record, since consecutive blocks are neighbours. For n records:
//
//	flags      n bytes: bit0 = raw (then no other bit may be set); otherwise
//	           bits1-3 = isometry, bits4-5 = value map kind, bit6 = same-scale
//	           domain (copied as is rather than decimated); bit7 must be zero
//	sizes      n varints: range block side
//	rx         n zigzag varints: delta from the previous record's rx (first from 0)
//	ry         n zigzag varints: likewise
//	dx         one zigzag varint per non-raw record: delta from the previous
//	           non-raw record's dx
//	dy         likewise
//	constants  one byte per non-raw record whose value map is not the identity
//	raw data   for each raw record, size*size literal bytes
//
// The whole run is then stored by packBlob.

// encodeRecords serializes ts in the column layout.
func encodeRecords(ts []Transform) []byte {
	n := len(ts)
	flags := make([]byte, 0, n)
	var sizes, rx, ry, dx, dy, consts, raw []byte
	prx, pry, pdx, pdy := 0, 0, 0, 0
	for i := range ts {
		t := &ts[i]
		if t.Raw {
			flags = append(flags, 1)
		} else {
			f := byte(t.Iso)<<1 | byte(t.Map.Kind)<<4
			if t.SameScale {
				f |= 1 << 6
			}
			flags = append(flags, f)
		}
		sizes = binary.AppendUvarint(sizes, uint64(t.Size))
		rx = binary.AppendUvarint(rx, zigzag(t.RX-prx))
		ry = binary.AppendUvarint(ry, zigzag(t.RY-pry))
		prx, pry = t.RX, t.RY
		if t.Raw {
			raw = append(raw, t.Data...)
			continue
		}
		dx = binary.AppendUvarint(dx, zigzag(t.DX-pdx))
		dy = binary.AppendUvarint(dy, zigzag(t.DY-pdy))
		pdx, pdy = t.DX, t.DY
		if t.Map.Kind != transform.MapIdentity {
			consts = append(consts, t.Map.C)
		}
	}
	return bytes.Join([][]byte{flags, sizes, rx, ry, dx, dy, consts, raw}, nil)
}

func zigzag(d int) uint64 { v := int64(d); return uint64(v<<1) ^ uint64(v>>63) }

// delta reads a zigzag varint and applies it to *prev, requiring the result to
// stay inside [0, maxCells].
func (p *reader) delta(prev *int) int {
	v := p.uvarint64(2 * maxCells)
	d := int64(v>>1) ^ -int64(v&1)
	next := int64(*prev) + d
	if p.err == nil && (next < 0 || next > maxCells) {
		p.fail(fmt.Errorf("fractal: position %d out of range", next))
	}
	*prev = int(next)
	return *prev
}

// decodeRecords parses count records from blob for a pw x ph grid. Every
// length is checked against the bytes actually present before it is used.
func decodeRecords(blob []byte, count, pw, ph int) ([]Transform, error) {
	b := &reader{r: bytes.NewReader(blob)}
	flags := b.bytes(count)
	if b.err != nil {
		return nil, b.err
	}
	for i, f := range flags {
		if f&1 != 0 && f != 1 || f&1 == 0 && f>>7 != 0 {
			return nil, fmt.Errorf("fractal: transform %d: reserved bits set in flags %#x", i, f)
		}
	}
	ts := make([]Transform, count) // count <= len(blob): the flags were just read
	for i, f := range flags {
		if f == 1 {
			ts[i].Raw = true
		} else {
			ts[i].Iso = transform.Isometry(f >> 1 & 7)
			ts[i].Map.Kind = transform.MapKind(f >> 4 & 3)
			ts[i].SameScale = f>>6&1 == 1
		}
	}
	for i := range ts {
		ts[i].Size = b.uvarint(maxCells)
	}
	prx, pry := 0, 0
	for i := range ts {
		ts[i].RX = b.delta(&prx)
	}
	for i := range ts {
		ts[i].RY = b.delta(&pry)
	}
	pdx, pdy := 0, 0
	for i := range ts {
		if !ts[i].Raw {
			ts[i].DX = b.delta(&pdx)
		}
	}
	for i := range ts {
		if !ts[i].Raw {
			ts[i].DY = b.delta(&pdy)
		}
	}
	for i := range ts {
		if !ts[i].Raw && ts[i].Map.Kind != transform.MapIdentity {
			ts[i].Map.C = b.byte()
		}
	}
	for i := range ts {
		if ts[i].Raw {
			if ts[i].Size > pw || ts[i].Size > ph { // checked before allocating Size*Size
				return nil, fmt.Errorf("fractal: transform %d: size %d exceeds the grid", i, ts[i].Size)
			}
			ts[i].Data = b.bytes(ts[i].Size * ts[i].Size)
		}
		if b.err != nil {
			break
		}
	}
	if b.err != nil {
		return nil, fmt.Errorf("fractal: transform data: %w", b.err)
	}
	if b.r.Len() != 0 {
		return nil, errors.New("fractal: trailing bytes in transform data")
	}
	return ts, nil
}

// ---- storing the serialized records ----------------------------------------

// A packed blob is: mode byte, uvarint raw length, then the data.
const (
	blobStored = 0
	blobFlate  = 1
	blobCM     = 2

	// The slow coder is not tried on blobs larger than this. (It is chunked,
	// so large blobs run on all cores; the cap just bounds the time.)
	maxCMBlob = 1 << 27

	// A hostile stream could claim an enormous raw blob and make the decoder
	// allocate for it. Real blobs compress far less than this, so a claimed
	// expansion beyond this ratio is rejected before anything is allocated.
	maxBlobRatio = 1 << 14
)

// packBlob stores tb in the smallest of: as is, DEFLATE, or the
// context-mixing coder.
func packBlob(tb []byte, workers int) []byte {
	header := func(mode byte) []byte {
		return binary.AppendUvarint([]byte{mode}, uint64(len(tb)))
	}
	best := append(header(blobStored), tb...)
	if len(tb) == 0 {
		return best
	}
	if fl := append(header(blobFlate), deflate(tb)...); len(fl) < len(best) {
		best = fl
	}
	if len(tb) <= maxCMBlob {
		if cm := append(header(blobCM), entropy.EncodeChunked(tb, 0, entropy.DefaultChunk, workers)...); len(cm) < len(best) {
			best = cm
		}
	}
	return best
}

// unpackBlob reverses packBlob. limit is the largest raw length any valid
// stream of this shape can have.
func unpackBlob(packed []byte, limit int64, workers int) ([]byte, error) {
	if len(packed) < 1 {
		return nil, errors.New("fractal: transform data: missing")
	}
	mode := packed[0]
	n, k := binary.Uvarint(packed[1:])
	if k <= 0 {
		return nil, errors.New("fractal: transform data: bad length")
	}
	if n > uint64(limit) {
		return nil, errors.New("fractal: transform data larger than any valid stream")
	}
	data := packed[1+k:]
	raw := int(n)
	switch mode {
	case blobStored:
		if len(data) != raw {
			return nil, fmt.Errorf("fractal: transform data: stored blob has %d bytes, header says %d", len(data), raw)
		}
		return data, nil
	case blobFlate, blobCM:
		if uint64(raw) > 1024+uint64(len(data))*maxBlobRatio {
			return nil, errors.New("fractal: transform data: implausible expansion")
		}
		var out []byte
		var err error
		if mode == blobFlate {
			out, err = inflateExact(data, raw)
		} else {
			out, err = entropy.DecodeChunked(data, raw, 0, workers)
		}
		if err != nil {
			return nil, fmt.Errorf("fractal: transform data: %w", err)
		}
		return out, nil
	}
	return nil, fmt.Errorf("fractal: transform data: unknown mode %d", mode)
}
