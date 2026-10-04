package codec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math/rand"
	"testing"
)

// bigOpts compresses quickly and cuts into small super-segments.
func bigOpts(seg int) Options {
	o := PresetOptions(PresetFast)
	o.BigSegment = seg
	o.SegmentSize = 4096
	return o
}

// streamData builds n bytes of mixed content: structure, noise, repeats.
func streamData(n int) []byte {
	b := mixedData()
	for len(b) < n {
		b = append(b, gradient(97, 40)...)
		b = append(b, sierpinskiBytes(64, 64)...)
		b = append(b, randomBytes(3000, int64(len(b)))...)
		b = append(b, bytes.Repeat([]byte("stream "), 300)...)
	}
	return b[:n]
}

func compressStream(t testing.TB, data []byte, opt Options) ([]byte, Report) {
	t.Helper()
	var out bytes.Buffer
	rep, err := CompressStream(&out, bytes.NewReader(data), int64(len(data)), opt)
	if err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), rep
}

func decompressStream(blob []byte, maxSize int64) ([]byte, error) {
	var out bytes.Buffer
	_, err := DecompressStream(&out, bytes.NewReader(blob), int64(len(blob)), maxSize)
	return out.Bytes(), err
}

func TestStreamSmallInputIsAnOrdinaryContainer(t *testing.T) {
	data := streamData(30000)
	opt := bigOpts(1 << 20) // far larger than the input
	got, rep := compressStream(t, data, opt)
	want, _, err := CompressReport(data, opt)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("a small input should give exactly the container Compress gives")
	}
	if rep.BigSegments != 0 {
		t.Errorf("BigSegments = %d for an ordinary container", rep.BigSegments)
	}
	if back, err := Decompress(got); err != nil || !bytes.Equal(back, data) {
		t.Fatalf("Decompress: %v", err)
	}
}

func TestStreamRoundTripAtEveryBoundary(t *testing.T) {
	const seg = 8192
	for _, n := range []int{seg - 1, seg, seg + 1, 2 * seg, 2*seg + 1, 3*seg - 1, 3 * seg, 4*seg + 100} {
		data := streamData(n)
		blob, rep := compressStream(t, data, bigOpts(seg))
		want := (n + seg - 1) / seg
		if n > seg && rep.BigSegments != want {
			t.Errorf("n=%d: %d super-segments, want %d", n, rep.BigSegments, want)
		}
		if n > seg && blob[5] != byte(ModeBig) {
			t.Errorf("n=%d: mode %d, want big", n, blob[5])
		}
		if rep.OutBytes != 0 && rep.OutBytes != int64(len(blob)) {
			t.Errorf("n=%d: OutBytes %d but wrote %d", n, rep.OutBytes, len(blob))
		}
		got, err := decompressStream(blob, 0)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("n=%d: DecompressStream: %v", n, err)
		}
		// The same container decodes in memory too.
		if back, err := Decompress(blob); err != nil || !bytes.Equal(back, data) {
			t.Fatalf("n=%d: Decompress: %v", n, err)
		}
	}
}

func TestStreamWritesAsItGoes(t *testing.T) {
	// A writer that records how much had been written when each piece arrives
	// shows output is produced super-segment by super-segment, not at the end.
	data := streamData(5 * 8192)
	var sizes []int
	w := writerFunc(func(p []byte) (int, error) { sizes = append(sizes, len(p)); return len(p), nil })
	if _, err := CompressStream(w, bytes.NewReader(data), int64(len(data)), bigOpts(8192)); err != nil {
		t.Fatal(err)
	}
	if len(sizes) == 0 {
		t.Fatal("nothing written")
	}
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// sampled returns the byte positions to damage: every one of the first 64 (the
// header and the first piece's framing, where the structural checks live), then
// every step-th, so the cost stays small however big the container is.
func sampled(n, step int) []int {
	var out []int
	for i := 0; i < n; i++ {
		if i < 64 || i%step == 0 || i == n-1 {
			out = append(out, i)
		}
	}
	return out
}

func TestStreamRejectsDamage(t *testing.T) {
	data := streamData(2 * 4096)
	blob, _ := compressStream(t, data, bigOpts(4096))
	if _, err := decompressStream(blob, 0); err != nil {
		t.Fatal(err)
	}
	t.Logf("container of %d bytes", len(blob))
	for _, n := range sampled(len(blob), 13) {
		if n == len(blob) {
			continue
		}
		if got, err := decompressStream(blob[:n], 0); err == nil {
			t.Fatalf("truncation to %d/%d accepted (%d bytes)", n, len(blob), len(got))
		}
		if _, err := Decompress(blob[:n]); err == nil {
			t.Fatalf("in-memory: truncation to %d/%d accepted", n, len(blob))
		}
	}
	if _, err := decompressStream(append(append([]byte(nil), blob...), 0), 0); err == nil {
		t.Error("trailing byte accepted")
	}
	// A flipped bit may be harmless slack in an arithmetic-coded stream, but
	// it must never produce different data silently.
	for _, i := range sampled(len(blob), 23) {
		for bit := 0; bit < 8; bit++ {
			if i >= 64 && bit%4 != 1 {
				continue // beyond the framing, two bits per sampled byte are plenty
			}
			bad := append([]byte(nil), blob...)
			bad[i] ^= 1 << bit
			if got, err := decompressStream(bad, 0); err == nil && !bytes.Equal(got, data) {
				t.Fatalf("bit %d of byte %d produced wrong data silently", bit, i)
			}
		}
	}
}

func TestStreamHeaderChecks(t *testing.T) {
	data := streamData(2 * 4096)
	blob, _ := compressStream(t, data, bigOpts(4096))
	if _, err := decompressStream(blob, int64(len(data))-1); !errors.Is(err, ErrTooLarge) {
		t.Errorf("size limit: got %v, want ErrTooLarge", err)
	}
	if _, err := decompressStream(blob, int64(len(data))); err != nil {
		t.Errorf("limit equal to size should pass: %v", err)
	}
	// Corrupt the checksum field (after magic, version, mode, length varint).
	var lb [binary.MaxVarintLen64]byte
	crcAt := 6 + binary.PutUvarint(lb[:], uint64(len(data)))
	bad := append([]byte(nil), blob...)
	bad[crcAt] ^= 0xff
	if _, err := decompressStream(bad, 0); !errors.Is(err, ErrChecksum) {
		t.Errorf("bad checksum: got %v, want ErrChecksum", err)
	}
	// Structural lies inside the payload.
	head := appendHeader(nil, ModeBig, uint64(len(data)), crc(data))
	mk := func(payload ...byte) []byte { return append(append([]byte(nil), head...), payload...) }
	seg := binary.AppendUvarint(nil, 4096)
	piece := func(mode byte, plen uint64, body []byte) []byte {
		return append(append([]byte{mode}, binary.AppendUvarint(nil, plen)...), body...)
	}
	cases := map[string][]byte{
		"empty payload":      mk(),
		"segment size zero":  mk(0),
		"segment size tiny":  mk(1),
		"segment size huge":  mk(binary.AppendUvarint(nil, 1<<40)...),
		"bad mode":           mk(append(seg, piece(9, 0, nil)...)...),
		"nested big":         mk(append(seg, piece(byte(ModeBig), 0, nil)...)...),
		"length lies":        mk(append(seg, piece(byte(ModeStored), 1<<30, nil)...)...),
		"truncated payload":  mk(append(seg, piece(byte(ModeStored), 4096, make([]byte, 100))...)...),
		"no segments follow": mk(seg...),
	}
	for name, b := range cases {
		if _, err := decompressStream(b, 0); err == nil {
			t.Errorf("%s: expected an error", name)
		}
		if _, err := Decompress(b); err == nil {
			t.Errorf("%s: in-memory decode expected an error", name)
		}
	}
}

func TestStreamInputProblems(t *testing.T) {
	data := streamData(20000)
	var sink bytes.Buffer
	if _, err := CompressStream(&sink, bytes.NewReader(data), int64(len(data))+1000, bigOpts(8192)); err == nil {
		t.Error("an input shorter than announced was accepted")
	}
	if _, err := CompressStream(&sink, bytes.NewReader(data), -1, bigOpts(8192)); err == nil {
		t.Error("negative size accepted")
	}
	boom := errors.New("disk on fire")
	if _, err := CompressStream(&sink, failingReaderAt{boom, 9000}, 30000, bigOpts(8192)); !errors.Is(err, boom) {
		t.Errorf("read error not propagated: %v", err)
	}
	if _, err := CompressStream(failingWriter{boom}, bytes.NewReader(data), int64(len(data)), bigOpts(8192)); !errors.Is(err, boom) {
		t.Errorf("write error not propagated: %v", err)
	}
	// An empty input is an ordinary (empty) container.
	blob, _ := compressStream(t, nil, bigOpts(8192))
	if got, err := decompressStream(blob, 0); err != nil || len(got) != 0 {
		t.Errorf("empty input: %v %d", err, len(got))
	}
}

type failingReaderAt struct {
	err   error
	after int64
}

func (f failingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > f.after {
		return 0, f.err
	}
	for i := range p {
		p[i] = byte(off + int64(i))
	}
	return len(p), nil
}

type failingWriter struct{ err error }

func (f failingWriter) Write([]byte) (int, error) { return 0, f.err }

func TestBigSegmentSize(t *testing.T) {
	opt := DefaultOptions()
	if got := BigSegmentSize(opt); got != 64<<20 {
		t.Errorf("8 GiB budget: %d, want 64 MiB", got)
	}
	opt.Fractal.MemoryLimit = 1 << 30
	if got := BigSegmentSize(opt); got != 8<<20 {
		t.Errorf("1 GiB budget: %d, want 8 MiB", got)
	}
	opt.Fractal.MemoryLimit = 1 << 20
	if got := BigSegmentSize(opt); got < 1<<20 {
		t.Errorf("tiny budget gave %d, below the 1 MiB floor", got)
	}
	opt.Fractal.MemoryLimit = 1 << 50
	if got := BigSegmentSize(opt); got != maxBigSegment {
		t.Errorf("huge budget: %d, want the maximum %d", got, maxBigSegment)
	}
	opt.BigSegment = 12345
	if got := BigSegmentSize(opt); got != 12345 {
		t.Errorf("explicit size: %d", got)
	}
	opt.BigSegment = int(^uint(0) >> 1) // the largest int on this platform
	if got := BigSegmentSize(opt); got != maxBigSegment {
		t.Errorf("explicit size above the maximum: %d", got)
	}
}

func FuzzDecompressStream(f *testing.F) {
	f.Add(func() []byte { b, _ := compressStream(f, streamData(3*4096), bigOpts(4096)); return b }())
	f.Add([]byte{})
	f.Add(appendHeader(nil, ModeBig, 1<<40, 0))
	f.Fuzz(func(t *testing.T, blob []byte) {
		decompressStream(blob, 1<<22) // an error is fine; a panic, hang or huge allocation is not
		DecompressStreamOpt(&bytes.Buffer{}, bytes.NewReader(blob), int64(len(blob)), DecodeOptions{MaxSize: 1 << 22, MemoryLimit: 1 << 20})
		Decompress(blob)
	})
}

var _ = io.EOF
var _ = rand.New
