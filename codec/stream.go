package codec

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
)

// Streaming: inputs of any size.
//
// Compress and Decompress hold the whole input and output in memory. For
// larger inputs, CompressStream reads from an io.ReaderAt and writes to an
// io.Writer, one super-segment at a time:
//
//   - The input is cut into super-segments sized from the memory budget
//     (Options.BigSegment, by default budget/128, at most 1 GiB).
//   - Each super-segment goes through exactly the logic Compress uses (every
//     mode, the fractal stage on all cores, segmenting inside it), and the
//     result is checked by decoding it before it is written.
//   - Segments are written as they finish, so memory follows one super-segment,
//     not the input.
//
// An input that fits in one super-segment is compressed by Compress and is an
// ordinary container; a larger one becomes a ModeBig container:
//
//	segSize                      uvarint, bytes per super-segment (the last may be shorter)
//	per super-segment, in order:
//	    mode                     1 byte, 0-5 (a super-segment may itself be segmented)
//	    len(payload) payload     uvarint, then that mode's payload
//
// The header carries the CRC of the whole original, so the first thing
// CompressStream does is a pass over the input to compute it; that is what lets
// the output be written to a pipe.

const (
	maxBigSegment     = 1 << 30
	minBigSegment     = 1 << 12
	bigBytesPerByte   = 128 // memory planned per input byte of a super-segment
	bigPayloadSlack   = 1 << 20
	copyBuf           = 4 << 20
	defaultStreamMax  = int64(1) << 62
	maxPayloadDivisor = 1024 // a payload may exceed its segment by segLen/1024 + slack
)

// BigSegmentSize returns the super-segment size CompressStream will use.
func BigSegmentSize(opt Options) int {
	if opt.BigSegment > 0 {
		return min(max(opt.BigSegment, minBigSegment), maxBigSegment)
	}
	s := int(min(opt.Fractal.MemoryBudget()/bigBytesPerByte, maxBigSegment))
	return max(s&^(1<<20-1), 1<<20) // whole MiB, at least one
}

// CompressStream compresses size bytes of r to w. The Report describes the
// outcome (for a ModeBig container, only its totals).
func CompressStream(w io.Writer, r io.ReaderAt, size int64, opt Options) (Report, error) {
	var rep Report
	if size < 0 {
		return rep, errors.New("fpress: negative size")
	}
	sum, err := checksumAt(r, size)
	if err != nil {
		return rep, err
	}
	big := int64(BigSegmentSize(opt))
	if size <= big {
		data := make([]byte, size)
		if err := readFullAt(r, data, 0); err != nil {
			return rep, err
		}
		out, rep, err := CompressReport(data, opt)
		if err != nil {
			return rep, err
		}
		_, err = w.Write(out)
		return rep, err
	}

	rep = Report{OrigLen: int(min(size, int64(^uint(0)>>1))), Chosen: ModeBig}
	bw := bufio.NewWriterSize(w, 1<<20)
	head := appendHeader(nil, ModeBig, uint64(size), sum)
	head = binary.AppendUvarint(head, uint64(big))
	out := int64(len(head))
	if _, err := bw.Write(head); err != nil {
		return rep, err
	}
	data := make([]byte, big)
	for off := int64(0); off < size; off += big {
		n := int(min(big, size-off))
		data = data[:n]
		if err := readFullAt(r, data, off); err != nil {
			return rep, err
		}
		var segRep Report
		mode, payload, err := compressPayload(data, opt, &segRep)
		if err != nil {
			return rep, fmt.Errorf("super-segment at %d: %w", off, err)
		}
		// Each piece is verified before it is written.
		back, err := decodePayload(mode, payload, n)
		if err != nil {
			return rep, fmt.Errorf("fpress: internal error, super-segment at %d failed to decode: %w", off, err)
		}
		if string(back) != string(data) {
			return rep, fmt.Errorf("fpress: internal error, super-segment at %d did not round trip", off)
		}
		piece := binary.AppendUvarint([]byte{byte(mode)}, uint64(len(payload)))
		if _, err := bw.Write(piece); err != nil {
			return rep, err
		}
		if _, err := bw.Write(payload); err != nil {
			return rep, err
		}
		out += int64(len(piece) + len(payload))
		rep.BigSegments++
	}
	rep.OutBytes = out
	return rep, bw.Flush()
}

// DecompressStream decodes the container read from r (size bytes) into w and
// returns the number of bytes written. Originals larger than maxSize (0 = no
// practical limit) are refused. Output is written as it is decoded, so on an
// error w may hold a prefix of the original; the checksum is checked at the
// end, and callers writing to a file should discard it on error.
func DecompressStream(w io.Writer, r io.ReaderAt, size int64, maxSize int64) (int64, error) {
	return DecompressStreamOpt(w, r, size, DecodeOptions{MaxSize: maxSize})
}

// DecompressStreamOpt is DecompressStream with options (see DecodeOptions).
func DecompressStreamOpt(w io.Writer, r io.ReaderAt, size int64, opt DecodeOptions) (int64, error) {
	maxSize := opt.MaxSize
	if maxSize <= 0 {
		maxSize = defaultStreamMax
	}
	d := decodeOpts{memory: opt.MemoryLimit}
	br := bufio.NewReaderSize(io.NewSectionReader(r, 0, size), copyBuf)
	var fixed [len(magic) + 2]byte
	if _, err := io.ReadFull(br, fixed[:]); err != nil || string(fixed[:len(magic)]) != magic {
		return 0, fmt.Errorf("%w: bad magic", ErrCorrupt)
	}
	if v := fixed[len(magic)]; v != version {
		return 0, fmt.Errorf("%w: unsupported version %d", ErrCorrupt, v)
	}
	mode := Mode(fixed[len(magic)+1])
	n, err := binary.ReadUvarint(br)
	if err != nil {
		return 0, fmt.Errorf("%w: bad length field", ErrCorrupt)
	}
	if n > uint64(maxSize) {
		return 0, fmt.Errorf("%w: %d > %d", ErrTooLarge, n, maxSize)
	}
	var crcBytes [4]byte
	if _, err := io.ReadFull(br, crcBytes[:]); err != nil {
		return 0, fmt.Errorf("%w: truncated header", ErrCorrupt)
	}
	want := binary.LittleEndian.Uint32(crcBytes[:])

	cw := &crcWriter{w: w, h: crc32.NewIEEE()}
	if mode == ModeBig {
		if err := decodeBigTo(cw, br, n, d); err != nil {
			return cw.n, wrapCorrupt(err)
		}
	} else {
		if n > maxBigSegment {
			return 0, fmt.Errorf("%w: %d bytes is too large for mode %v", ErrCorrupt, n, mode)
		}
		payload, err := io.ReadAll(br)
		if err != nil {
			return 0, err
		}
		out, err := decodePayloadOpt(mode, payload, int(n), d)
		if err != nil {
			return 0, wrapCorrupt(err)
		}
		if _, err := cw.Write(out); err != nil {
			return cw.n, err
		}
	}
	if cw.h.Sum32() != want {
		return cw.n, ErrChecksum
	}
	return cw.n, nil
}

// wrapCorrupt reports err as a corrupt container, unless it already says what
// is wrong: a memory limit is not corruption, and neither is a size limit.
func wrapCorrupt(err error) error {
	if errors.Is(err, ErrCorrupt) || errors.Is(err, ErrMemory) || errors.Is(err, ErrTooLarge) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrCorrupt, err)
}

// decodeBigTo decodes a ModeBig payload read from r, writing origLen bytes to
// w one super-segment at a time, and requires the payload to end exactly there.
func decodeBigTo(w io.Writer, r io.Reader, origLen uint64, d decodeOpts) error {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReader(r)
	}
	if origLen == 0 {
		return errors.New("empty original cannot be a big container")
	}
	seg, err := binary.ReadUvarint(br)
	if err != nil || seg < minBigSegment || seg > maxBigSegment {
		return errors.New("bad super-segment size")
	}
	for done := uint64(0); done < origLen; {
		segLen := min(seg, origLen-done)
		mode, err := br.ReadByte()
		if err != nil || Mode(mode) > ModeCM {
			return fmt.Errorf("super-segment at %d: bad mode", done)
		}
		plen, err := binary.ReadUvarint(br)
		if err != nil || plen > segLen+segLen/maxPayloadDivisor+bigPayloadSlack {
			return fmt.Errorf("super-segment at %d: bad length", done)
		}
		if err := d.checkMemory(Mode(mode), int(segLen)); err != nil {
			return err // before allocating anything for it
		}
		payload := make([]byte, plen)
		if _, err := io.ReadFull(br, payload); err != nil {
			return fmt.Errorf("super-segment at %d: truncated: %w", done, io.ErrUnexpectedEOF)
		}
		out, err := decodePayloadOpt(Mode(mode), payload, int(segLen), d)
		if err != nil {
			return fmt.Errorf("super-segment at %d: %w", done, err)
		}
		if _, err := w.Write(out); err != nil {
			return err
		}
		done += segLen
	}
	if _, err := br.ReadByte(); err != io.EOF {
		return errors.New("bytes after the last super-segment")
	}
	return nil
}

// ---- small I/O helpers --------------------------------------------------------

type crcWriter struct {
	w io.Writer
	h hash.Hash32
	n int64
}

func (c *crcWriter) Write(p []byte) (int, error) {
	c.h.Write(p)
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func checksumAt(r io.ReaderAt, size int64) (uint32, error) {
	h := crc32.NewIEEE()
	if _, err := io.CopyBuffer(h, io.NewSectionReader(r, 0, size), make([]byte, copyBuf)); err != nil {
		return 0, err
	}
	return h.Sum32(), nil
}

// readFullAt fills p from r at off; a short read is an error (the input was
// shorter than the size it was announced with).
func readFullAt(r io.ReaderAt, p []byte, off int64) error {
	n, err := r.ReadAt(p, off)
	if n == len(p) {
		return nil
	}
	if err == nil || err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return fmt.Errorf("fpress: reading input at %d: %w", off+int64(n), err)
}
