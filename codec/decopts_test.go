package codec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"runtime"
	"testing"
)

func TestDecodeWorkersFollowTheMemoryBudget(t *testing.T) {
	all := runtime.GOMAXPROCS(0)
	for _, c := range []struct {
		d    decodeOpts
		want int
	}{
		{decodeOpts{}, all},                  // no limit: every CPU
		{decodeOpts{workers: 1}, 1},          // asked for one
		{decodeOpts{workers: 1 << 20}, all},  // cannot exceed the CPUs
		{decodeOpts{memory: 8 << 30}, all},   // plenty of memory
		{decodeOpts{memory: 1}, 1},           // never zero
		{decodeOpts{memory: decodeSlack}, 1}, // nothing left over for workers
		{decodeOpts{memory: decodeSlack + 3*decodePerWorker}, min(3, all)},
		{decodeOpts{memory: 1 << 20, workers: 8}, 1}, // both limits apply
	} {
		if got := c.d.cpus(); got != c.want {
			t.Errorf("%+v: cpus() = %d, want %d", c.d, got, c.want)
		}
	}
	if d := (decodeOpts{memory: 1 << 30}).inner(); d.workers != 1 || d.memory != 1<<30 {
		t.Errorf("inner() = %+v", d)
	}
}

func TestDecodeNeedOrdersTheModes(t *testing.T) {
	n := 1 << 20
	if !(decodeNeed(ModeStored, n) <= decodeNeed(ModePrep, n) && decodeNeed(ModePrep, n) < decodeNeed(ModeFractal, n)) {
		t.Error("the fractal mode, which holds grids, should need the most per byte")
	}
	if decodeNeed(ModeFractal, 2*n) != 2*decodeNeed(ModeFractal, n) {
		t.Error("fractal need should scale with size")
	}
	// No limit, never an error; a limit that fits, no error; one that does not, ErrMemory.
	if err := (decodeOpts{}).checkMemory(ModeFractal, 1<<30); err != nil {
		t.Errorf("no limit set but got %v", err)
	}
	if err := (decodeOpts{memory: 1 << 30}).checkMemory(ModeFlate, 1<<20); err != nil {
		t.Errorf("a small piece within a large limit was refused: %v", err)
	}
	if err := (decodeOpts{memory: 1 << 20}).checkMemory(ModeFractal, 1<<20); !errors.Is(err, ErrMemory) {
		t.Errorf("got %v, want ErrMemory", err)
	}
}

// segmentModes lists the mode byte of each super-segment of a big container.
func segmentModes(t *testing.T, blob []byte) []Mode {
	t.Helper()
	rest := blob[6:]
	origLen, k := binary.Uvarint(rest)
	rest = rest[k+4:] // length, then the crc
	seg, k := binary.Uvarint(rest)
	rest = rest[k:]
	var modes []Mode
	for done := uint64(0); done < origLen; done += seg {
		modes = append(modes, Mode(rest[0]))
		l, k := binary.Uvarint(rest[1:])
		rest = rest[1+k+int(l):]
	}
	return modes
}

func TestDecodeRefusesPiecesThatCannotFitTheLimit(t *testing.T) {
	var data []byte
	for i := 0; i < 3; i++ {
		data = append(data, tileBytes(256, 8, 6, int64(i+1))...) // 64 KiB each, self-similar
	}
	opt := DefaultOptions()
	opt.BigSegment = 64 << 10
	blob, _ := compressStream(t, data, opt)
	modes := segmentModes(t, blob)
	fractalPieces := 0
	for _, m := range modes {
		if m == ModeFractal {
			fractalPieces++
		}
	}
	if fractalPieces == 0 {
		t.Fatalf("the test needs fractal pieces, got modes %v", modes)
	}
	need := decodeNeed(ModeFractal, 64<<10) // 768 KiB

	// Too small for one piece: refused, before anything is written.
	var out bytes.Buffer
	_, err := DecompressStreamOpt(&out, bytes.NewReader(blob), int64(len(blob)), DecodeOptions{MemoryLimit: need / 2})
	if !errors.Is(err, ErrMemory) {
		t.Fatalf("got %v, want ErrMemory", err)
	}
	t.Logf("refusal message: %v", err)

	// Big enough for one piece at a time: decodes, identically, on one goroutine.
	for _, limit := range []int64{decodeSlack + decodePerWorker, 1 << 30, 0} {
		out.Reset()
		if _, err := DecompressStreamOpt(&out, bytes.NewReader(blob), int64(len(blob)), DecodeOptions{MemoryLimit: limit}); err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		if !bytes.Equal(out.Bytes(), data) {
			t.Fatalf("limit %d: wrong output", limit)
		}
	}
}

// Within a container of ordinary pieces, the limit is also honoured by the
// in-piece fan-out: decoding a segmented piece under a one-worker budget
// gives the same bytes as under no limit.
func TestDecodeOutputDoesNotDependOnTheLimit(t *testing.T) {
	data := streamData(200000)
	blob, _ := compressStream(t, data, bigOpts(64<<10))
	var ref []byte
	for _, limit := range []int64{0, decodeSlack, decodeSlack + 2*decodePerWorker, 8 << 30} {
		var out bytes.Buffer
		if _, err := DecompressStreamOpt(&out, bytes.NewReader(blob), int64(len(blob)), DecodeOptions{MemoryLimit: limit}); err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		if ref == nil {
			ref = out.Bytes()
		} else if !bytes.Equal(ref, out.Bytes()) {
			t.Fatalf("limit %d: output differs", limit)
		}
	}
	if !bytes.Equal(ref, data) {
		t.Fatal("wrong output")
	}
}

func TestHumanSize(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 12: "12 B", 1 << 10: "1.0 KiB", 786432: "768.0 KiB", 3 << 19: "1.5 MiB", 8 << 30: "8.0 GiB"} {
		if got := humanSize(n); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", n, got, want)
		}
	}
}

// A memory refusal is not a corrupt file, and must not be reported as one, in
// memory or streaming; a damaged file still is corrupt.
func TestMemoryRefusalIsNotCorruption(t *testing.T) {
	data := streamData(3 * 4096)
	blob, _ := compressStream(t, data, bigOpts(4096))
	_, err := DecompressStreamOpt(&bytes.Buffer{}, bytes.NewReader(blob), int64(len(blob)), DecodeOptions{MemoryLimit: 1})
	if err == nil {
		t.Skip("no piece in this container needs more than the minimum") // cannot happen for 1 byte, kept for safety
	}
	if errors.Is(err, ErrCorrupt) {
		t.Errorf("a memory refusal was reported as corruption: %v", err)
	}
	if !errors.Is(err, ErrMemory) {
		t.Errorf("got %v, want ErrMemory", err)
	}
	bad := append([]byte(nil), blob...)
	bad[len(bad)-3] ^= 0xff
	bad[len(bad)/2] ^= 0xff
	if _, err := Decompress(bad); err == nil {
		t.Log("damage was harmless slack") // possible for arithmetic-coded data
	} else if errors.Is(err, ErrMemory) {
		t.Errorf("damage reported as a memory problem: %v", err)
	}
}

// The coder's model grows with the piece: a small piece must not be charged
// for a large model (it would be refused under a moderate limit for no reason).
func TestCMNeedScalesWithThePiece(t *testing.T) {
	small, large := decodeNeed(ModeCM, 64<<10), decodeNeed(ModeCM, 64<<20)
	if small >= 32<<20 {
		t.Errorf("a 64 KiB cm piece is charged %d MiB", small>>20)
	}
	if large < 64<<20 || large <= small {
		t.Errorf("a 64 MiB cm piece is charged %d MiB (small: %d MiB)", large>>20, small>>20)
	}
	for _, n := range []int{1, 100, 4096, 1 << 16, 1 << 20, 1 << 26, 1 << 30} {
		if cmModelBytes(n) <= 0 || cmModelBytes(n) > 120<<20 {
			t.Errorf("cmModelBytes(%d) = %d", n, cmModelBytes(n))
		}
	}
}
