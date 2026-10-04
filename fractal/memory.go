package fractal

import "math"

// Memory model. The fractal stage can be asked to work within a memory budget
// (Options.MemoryLimit, 8 GiB by default); it does so by choosing how big the
// tiles are and how many run at once. The constants come from measuring peak
// heap on a self-similar image (about 12 bytes per cell), on random data where
// every block is a raw anchor (about 26) and on a smooth ramp (about 22), with
// a safety margin on the worst.
const (
	// DefaultMemoryLimit is the budget when Options.MemoryLimit is 0.
	DefaultMemoryLimit = 8 << 30

	encodeBytesPerCell = 40 // peak working set per cell of a tile being encoded
	decodeBytesPerCell = 12 // ... being decoded (grids, residual, records)

	// Each worker that codes a stream holds a coder model (about 35 MB for the
	// default chunk size); budget a little more, plus some slack for the rest.
	perWorkerBytes = 64 << 20
	slackBytes     = 256 << 20

	minAutoTile = 64
)

func (o Options) memoryLimit() int64 {
	if o.MemoryLimit > 0 {
		return o.MemoryLimit
	}
	return DefaultMemoryLimit
}

// available is the budget left for tile data once the per-worker and fixed
// costs are set aside; never less than something workable.
func available(limit int64, workers int) int64 {
	a := limit - int64(workers)*perWorkerBytes - slackBytes
	return max(a, 16<<20)
}

// autoTile picks the tile side for a w x h matrix: the whole matrix as one tile
// (so any block can copy from any other) when it fits the budget, otherwise the
// largest square tiles for which about four can be in flight at once.
func autoTile(w, h int, opt Options, workers int, bytesPerCell int64) int {
	avail := available(opt.memoryLimit(), workers)
	cells := int64(w) * int64(h)
	if cells*bytesPerCell <= avail {
		return min(max(w, h, 1), maxTile) // one tile, at least as big as the matrix
	}
	side := int(math.Sqrt(float64(avail) / 4 / float64(bytesPerCell)))
	return clampTile(max(side, minAutoTile), opt.Block)
}

// clampTile keeps a tile side in [1, maxTile] and, when it can, a multiple of
// the block size so blocks do not straddle a tile edge.
func clampTile(side, block int) int {
	side = min(side, maxTile)
	if block > 0 && side > block {
		side -= side % block
	}
	return max(side, 1)
}

// tileConcurrency is how many tiles may be processed at once: as many as there
// are workers and tiles, but no more than the budget has room for.
func tileConcurrency(opt Options, workers, nTiles, tileCells int, bytesPerCell int64) int {
	avail := available(opt.memoryLimit(), workers)
	byMemory := int(avail / max(1, int64(tileCells)*bytesPerCell))
	return max(1, min(workers, nTiles, byMemory))
}

// MemoryBudget returns the memory budget in bytes the options plan around
// (MemoryLimit, or DefaultMemoryLimit when that is 0).
func (o Options) MemoryBudget() int64 { return o.memoryLimit() }
