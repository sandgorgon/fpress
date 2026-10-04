package fractal

import (
	"testing"

	"fpress/matrix"
)

func matrixEqualDense(a *matrix.Dense, b matrix.Matrix) bool { return matrix.Equal(a, b) }

func TestAutoTileUsesOneTileWhenTheMatrixFits(t *testing.T) {
	opt := DefaultOptions()
	for _, sh := range [][2]int{{1, 1}, {100, 80}, {513, 20}, {4096, 4096}, {10000, 3000}} {
		got := autoTile(sh[0], sh[1], opt, 12, encodeBytesPerCell)
		if got < max(sh[0], sh[1]) {
			t.Errorf("%v: tile %d is smaller than the matrix", sh, got)
		}
	}
	// A matrix wider than the largest tile cannot be one tile.
	if got := autoTile(maxTile*2, 10, opt, 12, encodeBytesPerCell); got != maxTile {
		t.Errorf("very wide matrix: tile %d, want the maximum %d", got, maxTile)
	}
}

func TestAutoTileShrinksToFitTheBudget(t *testing.T) {
	opt := DefaultOptions()
	opt.MemoryLimit = 1 << 30 // 1 GiB
	w, h := 40000, 40000      // far more than fits
	tile := autoTile(w, h, opt, 4, encodeBytesPerCell)
	if tile >= w {
		t.Fatalf("tile %d does not split a matrix that cannot fit", tile)
	}
	if tile%opt.Block != 0 {
		t.Errorf("tile %d is not a multiple of the block size %d", tile, opt.Block)
	}
	// About four tiles should fit in the budget at once.
	need := int64(tile) * int64(tile) * encodeBytesPerCell
	if avail := available(opt.MemoryLimit, 4); need*4 > avail+avail/8 {
		t.Errorf("tile %d needs %d MB; four of them exceed the %d MB available", tile, need>>20, avail>>20)
	}
	if tile < minAutoTile {
		t.Errorf("tile %d below the minimum", tile)
	}
}

func TestTileConcurrencyRespectsMemoryAndWorkers(t *testing.T) {
	opt := DefaultOptions()
	opt.MemoryLimit = 2 << 30
	// Plenty of room: limited by workers and tiles.
	if got := tileConcurrency(opt, 8, 100, 1000, encodeBytesPerCell); got != 8 {
		t.Errorf("got %d, want 8 (workers)", got)
	}
	if got := tileConcurrency(opt, 8, 3, 1000, encodeBytesPerCell); got != 3 {
		t.Errorf("got %d, want 3 (tiles)", got)
	}
	// Huge tiles: only one fits.
	if got := tileConcurrency(opt, 8, 100, 40_000_000, encodeBytesPerCell); got != 1 {
		t.Errorf("got %d, want 1 (memory)", got)
	}
	// Never zero, even when a single tile exceeds the budget.
	if got := tileConcurrency(opt, 8, 100, 1<<30, encodeBytesPerCell); got != 1 {
		t.Errorf("got %d, want 1", got)
	}
}

func TestEncodeUnderASmallMemoryLimitStillRoundTrips(t *testing.T) {
	opt := DefaultOptions()
	opt.MemoryLimit = 300 << 20 // small enough that the model splits a larger matrix
	m := sierpinski(300)
	tile := autoTile(300, 300, opt, 4, encodeBytesPerCell)
	tl, _, _, err := EncodeTiled(m, opt)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeTiled(tl, 0)
	if err != nil || !matrixEqualDense(got, m) {
		t.Fatalf("round trip failed (tile %d): %v", tile, err)
	}
}
