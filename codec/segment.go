package codec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"sync"
)

// Mode 4 splits the original into fixed-size segments and encodes each one on
// its own, in whichever of modes 0-3 and 5 is smallest for that segment. Real files
// mix kinds of data (code, tables, text, noise); one width and chain for the
// whole file suits only one of them, while here each stretch picks its own.
//
// Payload layout:
//
//	segSize                      uvarint, bytes per segment (the last may be shorter)
//	per segment, in order:
//	    mode                     1 byte, 0-3 or 5 (a segment cannot itself be segmented)
//	    len(payload) payload     uvarint, then that mode's payload for this segment
//
// The number of segments is ceil(origLen / segSize). Segments are independent,
// so they are encoded and decoded in parallel.

const (
	defaultSegment = 64 << 10
	minSegment     = 1 << 10
	maxSegment     = 1 << 26
)

// segInfo summarises an encodeSegmented call.
type segInfo struct {
	size  int
	modes [numModes]int // how many segments used each mode (never ModeSegmented)
}

func resolveSegmentSize(opt Options) int {
	if opt.FrameRows > 0 && opt.Width > 0 && opt.SegmentSize >= 0 {
		// Video: a segment must hold several frames or there is nothing to
		// difference against (the first frame of a segment is stored whole).
		// Up to 8 frames, fewer when frames are big enough that the context
		// mixing coder (capped at MaxCMSize) would no longer fit a segment.
		fb := opt.FrameRows * opt.Width
		maxCM := opt.MaxCMSize
		if maxCM == 0 {
			maxCM = defaultMaxCM
		}
		return min(max(fb, fb*min(8, max(2, maxCM/fb))), maxSegment)
	}
	switch {
	case opt.SegmentSize < 0:
		return 0 // disabled
	case opt.SegmentSize == 0:
		return defaultSegment
	}
	return min(max(opt.SegmentSize, minSegment), maxSegment)
}

func encodeSegmented(data []byte, size int, opt Options) ([]byte, segInfo, error) {
	info := segInfo{size: size}
	n := (len(data) + size - 1) / size
	type result struct {
		mode    Mode
		payload []byte
		err     error
	}
	results := make([]result, n)

	// Parallelise across segments; each segment's fractal stage then runs on
	// one goroutine so the machine is not oversubscribed.
	workers := opt.Fractal.Workers
	if workers == 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	sub := opt
	sub.Fractal.Workers = 1
	jobs := make(chan int)
	var wg sync.WaitGroup
	for k := 0; k < min(workers, n); k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				seg := data[i*size : min(len(data), (i+1)*size)]
				mode, payload, _, err := encodeBlock(seg, sub)
				results[i] = result{mode, payload, err}
			}
		}()
	}
	for i := 0; i < n; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	out := binary.AppendUvarint(nil, uint64(size))
	for i, r := range results {
		if r.err != nil {
			return nil, info, fmt.Errorf("segment %d: %w", i, r.err)
		}
		info.modes[r.mode]++
		out = append(out, byte(r.mode))
		out = binary.AppendUvarint(out, uint64(len(r.payload)))
		out = append(out, r.payload...)
	}
	return out, info, nil
}

func decodeSegmented(payload []byte, origLen int, d decodeOpts) ([]byte, error) {
	if origLen == 0 {
		return nil, errors.New("empty original cannot be segmented")
	}
	v, k := binary.Uvarint(payload)
	if k <= 0 || v < minSegment || v > maxSegment {
		return nil, errors.New("bad segment size")
	}
	size := int(v)
	rest := payload[k:]
	n := (origLen + size - 1) / size
	if n*2 > len(rest) { // every segment needs at least a mode and a length byte
		return nil, fmt.Errorf("%d segments cannot fit in %d bytes", n, len(rest))
	}

	type seg struct {
		mode Mode
		data []byte
	}
	segs := make([]seg, n)
	for i := range segs {
		if len(rest) < 1 {
			return nil, errors.New("truncated segment table")
		}
		mode := Mode(rest[0])
		if mode == ModeSegmented || mode > ModeCM { // no nesting; Big is not a segment mode
			return nil, fmt.Errorf("segment %d: invalid mode %d", i, mode)
		}
		l, k := binary.Uvarint(rest[1:])
		if k <= 0 || l > uint64(len(rest)-1-k) {
			return nil, fmt.Errorf("segment %d: bad length", i)
		}
		segs[i] = seg{mode, rest[1+k : 1+k+int(l)]}
		rest = rest[1+k+int(l):]
	}
	if len(rest) != 0 {
		return nil, errors.New("trailing bytes after segments")
	}

	out := make([]byte, origLen)
	errs := make([]error, n)
	jobs := make(chan int)
	var wg sync.WaitGroup
	inner := d.inner() // the pieces already run side by side
	for k := 0; k < min(d.cpus(), n); k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				lo, hi := i*size, min(origLen, (i+1)*size)
				b, err := decodePayloadOpt(segs[i].mode, segs[i].data, hi-lo, inner)
				if err != nil {
					errs[i] = err
					continue
				}
				copy(out[lo:hi], b)
			}
		}()
	}
	for i := 0; i < n; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			return nil, fmt.Errorf("segment %d: %w", i, err)
		}
	}
	return out, nil
}
