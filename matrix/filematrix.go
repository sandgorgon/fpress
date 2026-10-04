package matrix

import (
	"errors"
	"fmt"
	"io"
)

// FileMatrix presents the bytes of an io.ReaderAt as a row-major matrix of the
// given width, so large inputs need not be loaded into memory. The final row
// is zero-padded when the size is not a multiple of the width.
//
// Reads go through a one-row cache. A FileMatrix is not safe for concurrent
// use. I/O errors do not panic: the affected cells read as zero and the first
// error is available from Err.
type FileMatrix struct {
	r      io.ReaderAt
	size   int64
	width  int
	height int

	cacheY int // row held in cache, -1 if none
	cache  []byte
	err    error
}

var _ Matrix = (*FileMatrix)(nil)

// NewFileMatrix wraps r, which holds size bytes, as a matrix of the given width.
func NewFileMatrix(r io.ReaderAt, size int64, width int) (*FileMatrix, error) {
	if width <= 0 {
		return nil, fmt.Errorf("matrix: width must be positive, got %d", width)
	}
	if size < 0 {
		return nil, errors.New("matrix: negative size")
	}
	rows := (size + int64(width) - 1) / int64(width)
	if rows > int64(^uint(0)>>1) {
		return nil, errors.New("matrix: too many rows")
	}
	return &FileMatrix{
		r: r, size: size, width: width, height: int(rows),
		cacheY: -1, cache: make([]byte, width),
	}, nil
}

func (f *FileMatrix) Width() int  { return f.width }
func (f *FileMatrix) Height() int { return f.height }

// Err returns the first I/O error encountered, if any.
func (f *FileMatrix) Err() error { return f.err }

func (f *FileMatrix) At(x, y int) byte {
	checkCell(f.width, f.height, x, y)
	if f.cacheY != y {
		f.load(y)
	}
	return f.cache[x]
}

func (f *FileMatrix) load(y int) {
	clear(f.cache)
	f.cacheY = y
	off := int64(y) * int64(f.width)
	n := min(int64(f.width), f.size-off)
	if n <= 0 {
		return
	}
	got, err := f.r.ReadAt(f.cache[:n], off)
	if err != nil && !(errors.Is(err, io.EOF) && int64(got) == n) && f.err == nil {
		f.err = err
	}
}
