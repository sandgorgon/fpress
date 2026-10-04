package entropy

import (
	"encoding/binary"
	"fmt"

	"fpress/internal/par"
)

// DefaultChunk is the usual chunk size for EncodeChunked: big enough that the
// models learn well, small enough that many chunks run side by side.
const DefaultChunk = 1 << 18

// EncodeChunked codes data as independent chunks of about chunk bytes (rounded
// up to whole rows when stride > 0), in parallel. A single stream cannot be
// coded or decoded in parallel because every bit depends on all the bits
// before it; chunks trade a little compression (each starts with an empty
// model) for being able to use every core in both directions.
//
// Layout: a uvarint chunk size, then per chunk a uvarint length and the
// stream; the number of chunks follows from len(data) and the chunk size. When
// everything fits in one chunk the layout is just a 0 byte followed by the one
// stream, which keeps small blobs small.
func EncodeChunked(data []byte, stride, chunk, workers int) []byte {
	chunk = effectiveChunk(len(data), stride, chunk)
	if len(data) == 0 {
		return nil
	}
	n := (len(data) + chunk - 1) / chunk
	if n == 1 {
		return append([]byte{0}, Encode(data, stride)...)
	}
	parts := make([][]byte, n)
	par.For(n, workers, 1, func(i int) {
		parts[i] = Encode(data[i*chunk:min(len(data), (i+1)*chunk)], stride)
	})
	out := binary.AppendUvarint(nil, uint64(chunk))
	for _, p := range parts {
		out = binary.AppendUvarint(out, uint64(len(p)))
		out = append(out, p...)
	}
	return out
}

// effectiveChunk applies the rounding rules: at least one row, whole rows when
// there are rows, and never more than the data (so short data is one chunk).
func effectiveChunk(n, stride, chunk int) int {
	if chunk <= 0 {
		chunk = DefaultChunk
	}
	if stride > 0 {
		chunk = max(1, chunk/stride) * stride
	}
	return max(1, min(chunk, max(n, 1)))
}

// DecodeChunked reverses EncodeChunked for n bytes. Malformed input (wrong
// chunk sizes, truncated or extra data, a damaged stream) is an error.
func DecodeChunked(blob []byte, n, stride, workers int) ([]byte, error) {
	if n == 0 {
		if len(blob) != 0 {
			return nil, fmt.Errorf("%w: data for an empty stream", errStream)
		}
		return nil, nil
	}
	c, k := binary.Uvarint(blob)
	if k <= 0 || c > uint64(n) {
		return nil, fmt.Errorf("%w: bad chunk size", errStream)
	}
	if c == 0 { // a single stream follows
		return Decode(blob[k:], n, stride)
	}
	chunk := int(c)
	if chunk != effectiveChunk(n, stride, chunk) {
		return nil, fmt.Errorf("%w: chunk size %d is not valid for %d bytes at stride %d", errStream, chunk, n, stride)
	}
	count := (n + chunk - 1) / chunk
	rest := blob[k:]
	if count > len(rest) { // every chunk needs at least a length byte
		return nil, fmt.Errorf("%w: %d chunks cannot fit in %d bytes", errStream, count, len(rest))
	}
	streams := make([][]byte, count)
	for i := range streams {
		l, k := binary.Uvarint(rest)
		if k <= 0 || l > uint64(len(rest)-k) {
			return nil, fmt.Errorf("%w: bad length for chunk %d", errStream, i)
		}
		streams[i], rest = rest[k:k+int(l)], rest[k+int(l):]
	}
	if len(rest) != 0 {
		return nil, fmt.Errorf("%w: %d bytes after the last chunk", errStream, len(rest))
	}
	out := make([]byte, n)
	errs := make([]error, count)
	par.For(count, workers, 1, func(i int) {
		lo, hi := i*chunk, min(n, (i+1)*chunk)
		var b []byte
		b, errs[i] = Decode(streams[i], hi-lo, stride)
		copy(out[lo:hi], b)
	})
	for i, err := range errs {
		if err != nil {
			return nil, fmt.Errorf("chunk %d: %w", i, err)
		}
	}
	return out, nil
}
