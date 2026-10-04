package fractal

import (
	"bytes"
	"errors"
	"fmt"
	"runtime"
	"sync"

	"fpress/internal/par"
	"fpress/matrix"
)

// A Tiled matrix is cut into square tiles (edge tiles may be smaller). Each
// tile is compressed on its own, in whichever of three ways is smallest, so
// the cost of the search is bounded by the tile size rather than the input
// size, tiles can be encoded and decoded in parallel, and a region that does
// not suit the fractal stage can fall back without dragging the rest down.
//
// Binary layout (varints as in format.go):
//
//	width height tile
//	per tile, in raster order: mode (1 byte), len(data), data
//
// where data is the raw tile bytes (TileStored), a DEFLATE stream of them
// (TileFlate), or a Compressed in its binary form (TileFractal).

// TileMode says how one tile is stored.
type TileMode uint8

const (
	TileStored  TileMode = 0
	TileFlate   TileMode = 1
	TileFractal TileMode = 2
)

// Tile is one compressed tile.
type Tile struct {
	Mode TileMode
	Data []byte
}

// Tiled is a matrix compressed tile by tile.
type Tiled struct {
	Width, Height int
	Tile          int
	Tiles         []Tile
}

func (t *Tiled) grid() (tx, ty int) {
	if t.Width == 0 || t.Height == 0 {
		return 0, 0
	}
	return (t.Width + t.Tile - 1) / t.Tile, (t.Height + t.Tile - 1) / t.Tile
}

// rect returns tile i's position and size within the matrix.
func (t *Tiled) rect(i int) (x0, y0, w, h int) {
	tx, _ := t.grid()
	x0, y0 = (i%tx)*t.Tile, (i/tx)*t.Tile
	return x0, y0, min(t.Tile, t.Width-x0), min(t.Tile, t.Height-y0)
}

func (t *Tiled) validate() error {
	if t.Width < 0 || t.Height < 0 || t.Width > maxCells || t.Height > maxCells {
		return fmt.Errorf("fractal: invalid tiled dimensions %dx%d", t.Width, t.Height)
	}
	if t.Tile < 1 || t.Tile > maxTile {
		return fmt.Errorf("fractal: invalid tile size %d", t.Tile)
	}
	tx, ty := t.grid()
	if int64(tx)*int64(ty) != int64(len(t.Tiles)) {
		return fmt.Errorf("fractal: %d tiles, expected %dx%d", len(t.Tiles), tx, ty)
	}
	for i, tile := range t.Tiles {
		if tile.Mode > TileFractal {
			return fmt.Errorf("fractal: tile %d: unknown mode %d", i, tile.Mode)
		}
	}
	return nil
}

// ---- encoding --------------------------------------------------------------

// TileStats tallies how tiles were stored; the fractal-specific fields
// describe only the tiles that ended up in fractal mode.
type TileStats struct {
	Tiles, Stored, Flate, Fractal int
	FractalCells                  int
}

// EncodeTiled compresses m tile by tile (opt.Tile; 0 = chosen to fit the memory
// budget, one tile if the whole matrix fits) using up to
// opt.Workers goroutines. Output is deterministic and does not depend on the
// worker count. m may be any Matrix, including ones that are not safe for
// concurrent use: tile contents are read one tile at a time under a lock.
func EncodeTiled(m matrix.Matrix, opt Options) (*Tiled, Stats, TileStats, error) {
	var st Stats
	var ts TileStats
	if err := opt.validate(); err != nil {
		return nil, st, ts, err
	}
	workers := par.Workers(opt.Workers)
	tile := opt.Tile
	if tile == 0 {
		tile = autoTile(m.Width(), m.Height(), opt, workers, encodeBytesPerCell)
	}
	out := &Tiled{Width: m.Width(), Height: m.Height(), Tile: tile}
	tx, ty := out.grid()
	n := tx * ty
	out.Tiles = make([]Tile, n)
	if n == 0 {
		return out, st, ts, nil
	}

	// Tiles run side by side as far as the memory budget allows; the workers
	// left over go to each tile's own parallel stages.
	concurrent := tileConcurrency(opt, workers, n, min(tile, m.Width())*min(tile, m.Height()), encodeBytesPerCell)
	tileOpt := opt
	tileOpt.Workers = max(1, workers/concurrent)

	type result struct {
		st  Stats
		err error
	}
	results := make([]result, n)
	var readMu sync.Mutex
	jobs := make(chan int)
	var wg sync.WaitGroup
	for k := 0; k < concurrent; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				x0, y0, w, h := out.rect(i)
				d := matrix.NewDense(w, h)
				readMu.Lock()
				readTile(d, m, x0, y0)
				readMu.Unlock()
				out.Tiles[i], results[i].st, results[i].err = encodeTile(d, tileOpt)
			}
		}()
	}
	for i := 0; i < n; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	for i, r := range results {
		if r.err != nil {
			return nil, st, ts, fmt.Errorf("fractal: tile %d: %w", i, r.err)
		}
		_, _, w, h := out.rect(i)
		st.Cells += w * h
		ts.Tiles++
		switch out.Tiles[i].Mode {
		case TileStored:
			ts.Stored++
		case TileFlate:
			ts.Flate++
		case TileFractal:
			ts.Fractal++
			ts.FractalCells += w * h
			st.RangeBlocks += r.st.RangeBlocks
			st.Recipes += r.st.Recipes
			st.CopyRecipes += r.st.CopyRecipes
			st.Anchors += r.st.Anchors
			st.Demoted += r.st.Demoted
			st.RefineRounds = max(st.RefineRounds, r.st.RefineRounds)
			st.ExactCells += r.st.ExactCells
			st.ResidualNonZero += r.st.ResidualNonZero
			st.ResidualPacked += r.st.ResidualPacked
		}
	}
	return out, st, ts, nil
}

// maxDeflateRatio is the best compression DEFLATE can achieve on any input
// (a run of identical bytes: one 258-byte match costs about two bits).
const maxDeflateRatio = 1032

// encodeTile picks the smallest of stored, DEFLATE and the fractal encoding.
// Ties go to the simpler mode.
//
// DEFLATE of the whole tile is the slowest part when the fractal encoding
// works well, and it cannot win when the fractal encoding is smaller than
// 1/1032 of the tile, so then it is not computed. When it is needed and
// there are spare goroutines, it runs alongside the fractal encode.
func encodeTile(d *matrix.Dense, opt Options) (Tile, Stats, error) {
	pix := d.Pix()
	stage("tile-start")
	var fl []byte
	var flDone chan struct{}
	if par.Workers(opt.Workers) > 1 && len(pix) >= 1<<16 {
		flDone = make(chan struct{})
		go func() { fl = deflate(pix); close(flDone) }()
	}
	c, st, err := Encode(d, opt)
	if err != nil {
		return Tile{}, st, err
	}
	blob, err := c.marshal(opt.Workers)
	if err != nil {
		return Tile{}, st, err
	}
	stage("marshal")
	if len(blob)*maxDeflateRatio < len(pix) {
		return Tile{Mode: TileFractal, Data: blob}, st, nil // stored and DEFLATE cannot be smaller
	}
	if flDone != nil {
		<-flDone
	} else {
		fl = deflate(pix)
	}
	best := Tile{Mode: TileStored, Data: pix}
	if len(fl) < len(best.Data) {
		best = Tile{Mode: TileFlate, Data: fl}
	}
	if len(blob) < len(best.Data) {
		best = Tile{Mode: TileFractal, Data: blob}
	}
	return best, st, nil
}

// ---- decoding --------------------------------------------------------------

// DecodeTiled reconstructs the matrix using up to workers goroutines
// (0 = GOMAXPROCS).
func DecodeTiled(t *Tiled, workers int) (*matrix.Dense, error) {
	if err := t.validate(); err != nil {
		return nil, err
	}
	dst := matrix.NewDense(t.Width, t.Height)
	if err := DecodeTiledInto(dst, t, workers); err != nil {
		return nil, err
	}
	return dst, nil
}

// DecodeTiledWith is DecodeTiled with an explicit memory budget in bytes
// (0 = DefaultMemoryLimit), which bounds how many tiles are decoded at once.
func DecodeTiledWith(t *Tiled, workers int, memoryLimit int64) (*matrix.Dense, error) {
	if err := t.validate(); err != nil {
		return nil, err
	}
	dst := matrix.NewDense(t.Width, t.Height)
	if err := DecodeTiledLimit(dst, t, workers, memoryLimit); err != nil {
		return nil, err
	}
	return dst, nil
}

// DecodeTiledInto reconstructs into any MutableMatrix of the right size.
// Tiles are decoded in parallel into private buffers and copied into dst one
// at a time, so dst need not be safe for concurrent use.
func DecodeTiledInto(dst matrix.MutableMatrix, t *Tiled, workers int) error {
	return DecodeTiledLimit(dst, t, workers, 0)
}

// DecodeTiledLimit is DecodeTiledInto planning its parallelism around a memory
// budget in bytes (0 = DefaultMemoryLimit).
func DecodeTiledLimit(dst matrix.MutableMatrix, t *Tiled, workers int, memoryLimit int64) error {
	if err := t.validate(); err != nil {
		return err
	}
	if dst.Width() != t.Width || dst.Height() != t.Height {
		return fmt.Errorf("fractal: destination is %dx%d, stream describes %dx%d",
			dst.Width(), dst.Height(), t.Width, t.Height)
	}
	n := len(t.Tiles)
	if n == 0 {
		return nil
	}
	if workers == 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	type result struct {
		i   int
		d   *matrix.Dense
		err error
	}
	// Tiles run side by side; when there are fewer tiles than workers, each
	// tile gets the spare goroutines for its own rebuild rounds.
	concurrent := tileConcurrency(Options{MemoryLimit: memoryLimit}, workers, n,
		min(t.Tile, t.Width)*min(t.Tile, t.Height), decodeBytesPerCell)
	inner := max(1, workers/concurrent)
	jobs := make(chan int)
	results := make(chan result)
	var wg sync.WaitGroup
	for k := 0; k < concurrent; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				d, err := t.decodeTile(i, inner)
				results <- result{i, d, err}
			}
		}()
	}
	go func() {
		for i := 0; i < n; i++ {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	firstErr, firstIdx := error(nil), n
	for r := range results {
		if r.err != nil {
			if r.i < firstIdx {
				firstErr, firstIdx = fmt.Errorf("fractal: tile %d: %w", r.i, r.err), r.i
			}
			continue
		}
		x0, y0, w, h := t.rect(r.i)
		matrix.Copy(matrix.NewMutableView(dst, x0, y0, w, h), r.d)
	}
	return firstErr
}

func (t *Tiled) decodeTile(i, workers int) (*matrix.Dense, error) {
	_, _, w, h := t.rect(i)
	tile := t.Tiles[i]
	switch tile.Mode {
	case TileStored, TileFlate:
		raw := tile.Data
		if tile.Mode == TileFlate {
			var err error
			if raw, err = inflateExact(tile.Data, w*h); err != nil {
				return nil, err
			}
		} else if len(raw) != w*h {
			return nil, fmt.Errorf("stored tile has %d bytes, want %d", len(raw), w*h)
		}
		d := matrix.NewDense(w, h)
		copy(d.Pix(), raw)
		return d, nil
	case TileFractal:
		var c Compressed
		if err := c.unmarshal(tile.Data, workers); err != nil {
			return nil, err
		}
		if c.Width != w || c.Height != h {
			return nil, fmt.Errorf("fractal tile is %dx%d, want %dx%d", c.Width, c.Height, w, h)
		}
		if c.Block > maxTiledBlock {
			return nil, fmt.Errorf("block size %d exceeds the tiled limit %d", c.Block, maxTiledBlock)
		}
		return decode(&c, workers)
	}
	return nil, fmt.Errorf("unknown tile mode %d", tile.Mode)
}

// ---- serialization ---------------------------------------------------------

// MarshalBinary serializes t.
func (t *Tiled) MarshalBinary() ([]byte, error) {
	if err := t.validate(); err != nil {
		return nil, err
	}
	var out []byte
	out = appendUvarint(out, t.Width)
	out = appendUvarint(out, t.Height)
	out = appendUvarint(out, t.Tile)
	for _, tile := range t.Tiles {
		out = append(out, byte(tile.Mode))
		out = appendUvarint(out, len(tile.Data))
		out = append(out, tile.Data...)
	}
	return out, nil
}

// UnmarshalBinary parses data into t, replacing its contents. Tile contents
// are checked when decoded, not here, but the tile count is checked against
// the bytes available before anything is allocated for it.
func (t *Tiled) UnmarshalBinary(data []byte) error {
	p := &reader{r: bytes.NewReader(data)}
	n := Tiled{
		Width:  p.uvarint(maxCells),
		Height: p.uvarint(maxCells),
		Tile:   p.uvarint(maxTile),
	}
	if p.err != nil {
		return p.err
	}
	if n.Tile < 1 {
		return errors.New("fractal: zero tile size")
	}
	tx, ty := n.grid()
	count := int64(tx) * int64(ty)
	if count*2 > int64(p.r.Len()) { // every tile needs at least a mode and a length byte
		return fmt.Errorf("fractal: %d tiles cannot fit in %d bytes", count, p.r.Len())
	}
	n.Tiles = make([]Tile, 0, count)
	for i := int64(0); i < count; i++ {
		mode := TileMode(p.byte())
		body := p.bytes(p.uvarint(maxCells))
		if p.err != nil {
			return p.err
		}
		n.Tiles = append(n.Tiles, Tile{Mode: mode, Data: body})
	}
	if p.r.Len() != 0 {
		return errors.New("fractal: trailing bytes after tiles")
	}
	if err := n.validate(); err != nil {
		return err
	}
	*t = n
	return nil
}

// readTile copies the w x h tile of m at (x0, y0) into d, a row at a time when
// m can hand out rows.
func readTile(d *matrix.Dense, m matrix.Matrix, x0, y0 int) {
	w, h := d.Width(), d.Height()
	if ra, ok := m.(matrix.RowAccessor); ok {
		for y := 0; y < h; y++ {
			copy(d.Row(y), ra.Row(y0 + y)[x0:x0+w])
		}
		return
	}
	matrix.Copy(d, matrix.NewView(m, x0, y0, w, h))
}
