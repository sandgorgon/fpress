package codec

import (
	"errors"
	"fmt"
	"math/bits"
	"runtime"
)

// DecodeOptions controls DecompressStreamOpt.
type DecodeOptions struct {
	// MaxSize refuses originals larger than this many bytes (0 = no practical limit).
	MaxSize int64

	// MemoryLimit, when positive, is the memory budget in bytes the decoder
	// plans around. It does two things: it limits how many pieces are decoded
	// in parallel, and it refuses, with ErrMemory, a container whose single
	// pieces are too big to decode within it (the piece size is fixed by the
	// file, so no setting of parallelism can shrink it). The estimates err on
	// the high side. 0 means no limit and all CPUs.
	MemoryLimit int64
}

// ErrMemory means the container cannot be decoded within the memory limit.
var ErrMemory = errors.New("fpress: not enough memory budget to decode")

// decodeOpts is what the decoders pass around.
type decodeOpts struct {
	workers int   // goroutines this decode may use (0 = all CPUs)
	memory  int64 // memory budget (0 = none)
}

const (
	decodePerWorker = 96 << 20 // coder models and buffers per goroutine, with margin
	decodeSlack     = 256 << 20
)

// cpus is the number of goroutines this decode may use: all CPUs unless fewer
// were asked for or fewer fit the memory budget.
func (d decodeOpts) cpus() int {
	n := runtime.GOMAXPROCS(0)
	if d.workers > 0 {
		n = min(n, d.workers)
	}
	if d.memory > 0 {
		n = min(n, int(max(d.memory-decodeSlack, 0)/decodePerWorker))
	}
	return max(n, 1)
}

// inner is for work nested inside a piece that is itself one of several decoded
// in parallel: it must not fan out again.
func (d decodeOpts) inner() decodeOpts { return decodeOpts{workers: 1, memory: d.memory} }

// decodeNeed estimates the memory needed to decode one piece of n bytes in the
// given mode, beyond the goroutine overhead counted by cpus. The factors come
// from the memory model of each mode (the fractal grids dominate: about 12
// bytes per cell); they are deliberately on the high side.
func decodeNeed(mode Mode, n int) int64 {
	switch mode {
	case ModeFractal:
		return 12 * int64(n)
	case ModePrep:
		return 4 * int64(n)
	case ModeSegmented:
		return 3 * int64(n)
	case ModeCM:
		return 2*int64(n) + cmModelBytes(n)
	}
	return 2 * int64(n) // stored, flate
}

// cmModelBytes is the size of the context-mixing coder's tables for a stream
// of n bytes: nine counter tables and a match table of 4-byte slots, 2^tb of
// each, with tb growing with log2(n) up to 21 (see entropy.tableBits), plus
// the smaller fixed tables.
func cmModelBytes(n int) int64 {
	tb := min(max(bits.Len(uint(n))+1, 12), 21)
	return 40<<tb + 8<<20
}

// checkMemory returns ErrMemory if a piece cannot be decoded within the budget.
func (d decodeOpts) checkMemory(mode Mode, n int) error {
	if d.memory <= 0 {
		return nil
	}
	if need := decodeNeed(mode, n); need > d.memory {
		return fmt.Errorf("%w: a %v piece of %s needs about %s and the limit is %s; raise the limit or decompress where more memory is available",
			ErrMemory, mode, humanSize(int64(n)), humanSize(need), humanSize(d.memory))
	}
	return nil
}

// humanSize writes a byte count with a sensible unit (1.5 MiB, 768 KiB, 12 B).
func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
