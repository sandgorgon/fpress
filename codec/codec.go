// Package codec is the public face of fpress: it turns a byte slice into a
// self-describing, checksummed container and back.
//
// Container layout:
//
//	"FPRS"   4 bytes magic
//	version  1 byte (currently 1)
//	mode     1 byte: 0 stored, 1 flate, 2 fractal, 3 prep+flate, 4 segmented, 5 context-mixing, 6 big
//	origLen  uvarint, length of the original data
//	crc32    4 bytes little-endian, IEEE CRC of the original data
//	payload  mode-specific, runs to the end of the container
//
// Modes 2 and 3 fold the data into a matrix of a stated width, run a prep.Chain
// of reversible matrix operations over it, and then compress the result: mode 3
// with DEFLATE, mode 2 with the fractal stage. Both payloads start with
//
//	width    uvarint, fold width of the original data
//	chain    prep.Chain binary form
//
// followed by DEFLATE data (mode 3) or a fractal.Tiled (mode 2).
//
// Mode 5 is a context-mixing arithmetic coder (package entropy), optionally
// after a fold and prep chain; its payload starts with
//
//	width    uvarint, 0 for a plain byte stream (then the chain is empty)
//	chain    prep.Chain binary form
//
// and the coder is told the prepared row length, so it can use the bytes
// above as context.
//
// Mode 4 cuts the data into segments and encodes each in whichever of modes
// 0-3 suits it best (see segment.go).
//
// Compress tries every applicable mode and keeps the smallest, so the output
// is never much larger than the input: the worst case is the data plus an
// 11-12 byte header.
package codec

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"

	"fpress/entropy"
	"fpress/fractal"
	"fpress/internal/par"
	"fpress/matrix"
	"fpress/prep"
)

// Mode says how a container's payload is encoded.
type Mode byte

const (
	ModeStored  Mode = 0 // the bytes themselves
	ModeFlate   Mode = 1 // DEFLATE
	ModeFractal Mode = 2 // prep chain, then fractal recipes + residual
	ModePrep    Mode = 3 // prep chain, then DEFLATE

	// ModeSegmented encodes fixed-size segments independently, each in one
	// of modes 0-3. Only valid at the top level.
	ModeSegmented Mode = 4

	// ModeCM codes the (optionally folded and prepared) bytes with the
	// context-mixing coder.
	ModeCM Mode = 5

	// ModeBig holds a very large input as independently compressed
	// super-segments, each in one of modes 0-5, so that memory use follows the
	// super-segment size and not the input size (see stream.go).
	ModeBig Mode = 6

	numModes = 7
)

func (m Mode) String() string {
	switch m {
	case ModeStored:
		return "stored"
	case ModeFlate:
		return "flate"
	case ModeFractal:
		return "fractal"
	case ModePrep:
		return "prep+flate"
	case ModeSegmented:
		return "segmented"
	case ModeCM:
		return "cm"
	case ModeBig:
		return "big"
	}
	return fmt.Sprintf("mode(%d)", byte(m))
}

const (
	magic   = "FPRS"
	version = 5

	// DefaultMaxSize is the largest original length Decompress will accept.
	DefaultMaxSize = 1 << 30

	// minPrepSize: below this the extra headers outweigh any gain.
	minPrepSize = 64

	defaultMaxCM = 16 << 20
)

var (
	// ErrCorrupt means the container is malformed.
	ErrCorrupt = errors.New("fpress: corrupt container")
	// ErrChecksum means the container parsed but the decoded data failed its CRC.
	ErrChecksum = errors.New("fpress: checksum mismatch")
	// ErrTooLarge means the container claims more data than the caller allows.
	ErrTooLarge = errors.New("fpress: original size exceeds limit")
)

// Options controls Compress.
type Options struct {
	Fractal fractal.Options

	// Width of the matrix the data is folded into. 0 chooses automatically
	// (the default width plus periods detected in the data).
	Width int

	// MaxFractalSize: inputs larger than this skip the fractal attempt.
	// 0 means no limit: the fractal stage works within Fractal.MemoryLimit and
	// its cost is linear in the input size.
	MaxFractalSize int

	// NoFractalGate turns off the probe that skips the fractal step when the
	// data shows no self-similarity (see fractal.Probe). Without the gate the
	// fractal step always runs, which on ordinary files costs a lot and changes
	// nothing.
	NoFractalGate bool

	// DisableFractal restricts the choice to stored, flate and prep+flate.
	DisableFractal bool

	// DisablePrep turns off the prep search: no prep+flate mode, and the
	// fractal stage sees the data folded at the default width with no chain.
	DisablePrep bool

	// FrameRows marks the input as raw video: frames of FrameRows rows each,
	// laid end to end, with Width the bytes in one row (pixels per row times
	// bytes per pixel). Layouts that store each row as its change from the same
	// row one frame earlier are then tried, and segments are made several frames
	// long so they can use them. 0 means ordinary data. It needs Width.
	FrameRows int

	// SegmentSize is the segment length for the segmented mode: 0 means the
	// default (64 KiB), a negative value disables the mode. It only applies
	// to inputs longer than one segment.
	SegmentSize int

	// DisableCM turns off the context-mixing mode.
	DisableCM bool

	// MaxCMSize: inputs larger than this skip the context-mixing mode, which
	// codes about 1 MB/s. 0 means the default (16 MiB).
	MaxCMSize int

	// SkipWhole: when the input is longer than one segment, do not also try the
	// whole-file modes; use only the segmented mode (after the quick reject).
	// The whole-file coders run on one goroutine; segments run in parallel, for
	// encoding and for decoding.
	SkipWhole bool

	// CMAttempts caps how many (width, chain) layouts the context-mixing coder
	// tries per block. 0 chooses by size (3, or fewer for large blocks).
	CMAttempts int

	// SearchSample is how many bytes of a block the layout search samples
	// (0 = 64 KiB). Smaller is faster and slightly less well informed.
	SearchSample int

	// FastFlate uses DEFLATE's default level instead of its best, for a small
	// loss in the DEFLATE-based modes and much less time.
	FastFlate bool

	// BigSegment is the super-segment size CompressStream cuts large inputs
	// into (0 = chosen from the memory budget; see BigSegmentSize).
	BigSegment int

	// NoQuickReject turns off the early exit for data that looks
	// incompressible (see hopeless).
	NoQuickReject bool
}

// DefaultOptions returns the default settings.
func DefaultOptions() Options {
	return Options{Fractal: fractal.DefaultOptions(), MaxFractalSize: 0}
}

// Report describes one Compress call. Sizes are total container bytes; a
// candidate that was not tried is -1.
type Report struct {
	OrigLen int
	Stored  int
	Flate   int

	PrepFlate    int // best prep+flate candidate
	PrepWidth    int
	PrepChain    prep.Chain
	Fractal      int // best fractal candidate
	FractalWidth int
	FractalChain prep.Chain
	FractalStats *fractal.Stats // nil if no fractal attempt was made
	FractalTiles fractal.TileStats
	FractalProbe fractal.ProbeResult // what the gate saw (zero if it did not run)
	FractalGated bool                // the gate decided the fractal step was not worth running

	CM      int // best context-mixing candidate
	CMWidth int
	CMChain prep.Chain

	BigSegments int   // super-segments written by CompressStream (0 for an ordinary container)
	OutBytes    int64 // total output size for CompressStream

	Rejected  bool // quick reject fired for the whole input
	Segmented int  // segmented candidate; -1 if not tried
	SegSize   int
	SegModes  [numModes]int // segments per mode in the segmented candidate

	Chosen Mode
	Out    int
}

// Compress encodes data in the smallest applicable mode.
func Compress(data []byte, opt Options) ([]byte, error) {
	out, _, err := CompressReport(data, opt)
	return out, err
}

// CompressReport is Compress plus a Report of every candidate's size.
func CompressReport(data []byte, opt Options) ([]byte, Report, error) {
	rep := Report{OrigLen: len(data), Stored: -1, Flate: -1, PrepFlate: -1, Fractal: -1, CM: -1, Segmented: -1}
	sum := crc32.ChecksumIEEE(data)
	mode, payload, err := compressPayload(data, opt, &rep)
	if err != nil {
		return nil, rep, err
	}
	best := container(mode, len(data), sum, payload)

	// Final safety net: the container must decode to exactly the input.
	back, err := DecompressLimit(best, max(len(data), 1))
	if err != nil {
		return nil, rep, fmt.Errorf("fpress: internal error, own output failed to decode: %w", err)
	}
	if !bytes.Equal(back, data) {
		return nil, rep, errors.New("fpress: internal error, round trip mismatch")
	}
	rep.Chosen, rep.Out = mode, len(best)
	return best, rep, nil
}

// compressPayload picks the smallest encoding of data and returns its mode and
// payload (without a container header), filling in rep along the way.
func compressPayload(data []byte, opt Options, rep *Report) (Mode, []byte, error) {
	seg := resolveSegmentSize(opt)
	segmenting := seg > 0 && len(data) > seg

	// The whole-input encodings and the segmented encoding are independent, so
	// they run at the same time (the whole-input coders are single-threaded and
	// would otherwise leave most cores idle while they work).
	var mode Mode
	var payload []byte
	var info blockInfo
	var err error
	var segPayload []byte
	var segInfo segInfo
	var segErr error
	wholeSkipped := opt.SkipWhole && segmenting
	if wholeSkipped {
		// Segments only. The quick reject still looks at the whole input, and
		// stored is the fallback the segmented candidate has to beat.
		mode, payload = ModeStored, data
		info = blockInfo{stored: len(data), flate: -1, prepFlate: -1, fractal: -1, cm: -1}
		info.rejected = !opt.NoQuickReject && hopelessFor(data, opt)
	}
	// runSegmented must not look at info: when it runs next to the whole-input
	// pass, that pass is writing it. Every branch that reaches it has already
	// ruled out the quick reject, so there is nothing to check.
	runSegmented := func() {
		if segmenting {
			segPayload, segInfo, segErr = encodeSegmented(data, seg, opt)
		}
	}
	if wholeSkipped {
		if !info.rejected {
			runSegmented()
		}
	} else if segmenting && !opt.NoQuickReject && hopelessFor(data, opt) {
		// Random-looking input: the whole-input pass will reject it; do not
		// start the segmented pass at all.
		mode, payload, info, err = encodeBlock(data, opt)
	} else {
		par.Do(2,
			func() { mode, payload, info, err = encodeBlock(data, opt) },
			runSegmented)
	}
	if err != nil {
		return 0, nil, err
	}
	if segErr != nil {
		return 0, nil, segErr
	}

	// Report sizes are whole-container sizes: payload plus the header.
	hdr := len(container(ModeStored, len(data), 0, nil))
	size := func(payloadLen int) int {
		if payloadLen < 0 {
			return -1
		}
		return payloadLen + hdr
	}
	rep.Rejected = info.rejected
	rep.Stored, rep.Flate = size(info.stored), size(info.flate)
	rep.PrepFlate, rep.PrepWidth, rep.PrepChain = size(info.prepFlate), info.prepWidth, info.prepChain
	rep.Fractal, rep.FractalWidth, rep.FractalChain = size(info.fractal), info.fractalWidth, info.fractalChain
	rep.FractalStats, rep.FractalTiles = info.fractalStats, info.fractalTiles
	rep.FractalProbe, rep.FractalGated = info.fractalProbe, info.fractalGated
	rep.CM, rep.CMWidth, rep.CMChain = size(info.cm), info.cmWidth, info.cmChain

	if segPayload != nil {
		rep.Segmented, rep.SegSize, rep.SegModes = size(len(segPayload)), seg, segInfo.modes
		if len(segPayload) < len(payload) {
			mode, payload = ModeSegmented, segPayload
		}
	}
	return mode, payload, nil
}

// candidates returns the (width, chain) hypotheses to try, best first.
func candidates(data []byte, opt Options) []prep.Candidate {
	if opt.DisablePrep {
		w := opt.Width
		if w <= 0 {
			w = prep.DefaultWidth(len(data))
		}
		return []prep.Candidate{{Width: min(w, len(data))}}
	}
	return prep.Search(data, prep.SearchOptions{Width: opt.Width, MaxSample: opt.SearchSample, FrameRows: opt.FrameRows})
}

// fractalAttempts picks which candidates get the (expensive) fractal stage:
// the best candidate overall, and the plain square fold with no chain. The
// latter is there because widths and chains are ranked by how well DEFLATE
// does, which says little about 2D self-similarity: that lives in the natural
// square layout, which DEFLATE may rank poorly.
func fractalAttempts(cands []prep.Candidate, dataLen int) []prep.Candidate {
	if len(cands) == 0 {
		return nil
	}
	out := []prep.Candidate{cands[0]}
	square := prep.DefaultWidth(dataLen)
	for _, c := range cands[1:] {
		if len(c.Chain) == 0 && c.Width == square {
			return append(out, c)
		}
	}
	if len(cands[0].Chain) > 0 { // no square fold among the candidates (fixed width?)
		for _, c := range cands[1:] {
			if len(c.Chain) == 0 {
				return append(out, c)
			}
		}
	}
	return out
}

func deflate(b []byte) []byte { return deflateAt(b, flate.BestCompression) }

func deflateAt(b []byte, level int) []byte {
	var buf bytes.Buffer
	fw, _ := flate.NewWriter(&buf, level)
	fw.Write(b)
	fw.Close()
	return buf.Bytes()
}

// foldHeader is the start shared by modes 2 and 3: width, then the chain.
func foldHeader(width int, chain prep.Chain) []byte {
	return chain.AppendBinary(binary.AppendUvarint(nil, uint64(width)))
}

// prepare folds data at the given width and runs the chain.
func prepare(data []byte, width int, chain prep.Chain) (*matrix.Dense, error) {
	return chain.Forward(matrix.FromBytes(data, width))
}

func prepFlatePayload(data []byte, width int, chain prep.Chain, level int) ([]byte, error) {
	m, err := prepare(data, width, chain)
	if err != nil {
		return nil, err
	}
	return append(foldHeader(width, chain), deflateAt(m.Pix(), level)...), nil
}

// gateMinSize: below this the fractal step is cheap enough to just run.
const gateMinSize = 32 << 10

// fractalPayload runs the tiled fractal stage on the prepared matrix. Unless
// the gate is off, a cheap probe first checks that the matrix has any
// self-similarity the stage can use; if not, skipped is true and no payload is
// produced.
func fractalPayload(data []byte, c prep.Candidate, opt fractal.Options, gate bool) (payload []byte, st fractal.Stats, ts fractal.TileStats, probe fractal.ProbeResult, skipped bool, err error) {
	opt.NoSelfCheck = true // the container is verified once, for the candidate that is kept
	m, err := prepare(data, c.Width, c.Chain)
	if err != nil {
		return nil, st, ts, probe, false, err
	}
	if gate && len(data) >= gateMinSize {
		if probe, err = fractal.Probe(m, opt); err != nil {
			return nil, st, ts, probe, false, err
		}
		if !probe.Worth() {
			return nil, st, ts, probe, true, nil
		}
		opt = probe.Narrow(opt) // search only the families of domains the probe saw hits in
	}
	tiled, st, ts, err := fractal.EncodeTiled(m, opt)
	if err != nil {
		return nil, st, ts, probe, false, err
	}
	blob, err := tiled.MarshalBinary()
	if err != nil {
		return nil, st, ts, probe, false, err
	}
	return append(foldHeader(c.Width, c.Chain), blob...), st, ts, probe, false, nil
}

func container(mode Mode, origLen int, sum uint32, payload []byte) []byte {
	out := appendHeader(make([]byte, 0, len(payload)+16), mode, uint64(origLen), sum)
	return append(out, payload...)
}

// appendHeader appends the container header: magic, version, mode, original
// length and the CRC of the original.
func appendHeader(dst []byte, mode Mode, origLen uint64, sum uint32) []byte {
	dst = append(dst, magic...)
	dst = append(dst, version, byte(mode))
	dst = binary.AppendUvarint(dst, origLen)
	return binary.LittleEndian.AppendUint32(dst, sum)
}

// Decompress decodes a container, accepting originals up to DefaultMaxSize.
func Decompress(blob []byte) ([]byte, error) {
	return DecompressLimit(blob, DefaultMaxSize)
}

// DecompressLimit decodes a container, refusing any whose original is larger
// than maxSize bytes. Use it on untrusted input.
func DecompressLimit(blob []byte, maxSize int) ([]byte, error) {
	if len(blob) < len(magic)+2 || string(blob[:len(magic)]) != magic {
		return nil, fmt.Errorf("%w: bad magic", ErrCorrupt)
	}
	if v := blob[len(magic)]; v != version {
		return nil, fmt.Errorf("%w: unsupported version %d", ErrCorrupt, v)
	}
	mode := Mode(blob[len(magic)+1])
	rest := blob[len(magic)+2:]
	n, k := binary.Uvarint(rest)
	if k <= 0 {
		return nil, fmt.Errorf("%w: bad length field", ErrCorrupt)
	}
	if n > uint64(maxSize) {
		return nil, fmt.Errorf("%w: %d > %d", ErrTooLarge, n, maxSize)
	}
	origLen := int(n)
	rest = rest[k:]
	if len(rest) < 4 {
		return nil, fmt.Errorf("%w: truncated header", ErrCorrupt)
	}
	want := binary.LittleEndian.Uint32(rest)
	payload := rest[4:]

	out, err := decodePayload(mode, payload, origLen)
	if err != nil {
		return nil, wrapCorrupt(err)
	}
	if crc32.ChecksumIEEE(out) != want {
		return nil, ErrChecksum
	}
	return out, nil
}

// decodePayload decodes one mode's payload into exactly n bytes.
func decodePayload(mode Mode, payload []byte, n int) ([]byte, error) {
	return decodePayloadOpt(mode, payload, n, decodeOpts{})
}

func decodePayloadOpt(mode Mode, payload []byte, n int, d decodeOpts) ([]byte, error) {
	if err := d.checkMemory(mode, n); err != nil {
		return nil, err
	}
	switch mode {
	case ModeStored:
		if len(payload) != n {
			return nil, fmt.Errorf("stored payload is %d bytes, header says %d", len(payload), n)
		}
		return append([]byte(nil), payload...), nil
	case ModeFlate:
		return inflateExact(payload, n)
	case ModeFractal:
		return decodeFractal(payload, n, d)
	case ModePrep:
		return decodePrep(payload, n)
	case ModeSegmented:
		return decodeSegmented(payload, n, d)
	case ModeCM:
		return decodeCM(payload, n)
	case ModeBig:
		var out bytes.Buffer
		out.Grow(n)
		if err := decodeBigTo(&out, bytes.NewReader(payload), uint64(n), d); err != nil {
			return nil, err
		}
		return out.Bytes(), nil
	}
	return nil, fmt.Errorf("unknown mode %d", byte(mode))
}

func inflateExact(payload []byte, n int) ([]byte, error) {
	src := bytes.NewReader(payload) // a ByteReader, so flate never reads ahead of what it uses
	fr := flate.NewReader(src)
	defer fr.Close()
	out := make([]byte, n)
	if _, err := io.ReadFull(fr, out); err != nil {
		return nil, fmt.Errorf("flate payload: %w", err)
	}
	var extra [1]byte
	if k, err := fr.Read(extra[:]); k != 0 {
		return nil, errors.New("flate payload longer than header says")
	} else if err != io.EOF { // the stream must also end cleanly
		return nil, fmt.Errorf("flate payload: %w", io.ErrUnexpectedEOF)
	}
	if src.Len() != 0 {
		return nil, fmt.Errorf("flate payload: %d bytes after the end of the stream", src.Len())
	}
	return out, nil
}

// parseFold reads the width and chain that start a mode 2 or 3 payload and
// returns the original fold shape, the shape after the chain, and the rest.
func parseFold(payload []byte, origLen int) (w, h int, chain prep.Chain, ow, oh int, rest []byte, err error) {
	if origLen == 0 {
		err = errors.New("empty original cannot use a folded mode")
		return
	}
	v, k := binary.Uvarint(payload)
	if k <= 0 || v < 1 || v > uint64(origLen) {
		err = errors.New("bad fold width")
		return
	}
	w = int(v)
	h = (origLen + w - 1) / w
	chain, n, err := prep.ParseChain(payload[k:])
	if err != nil {
		return
	}
	if ow, oh, err = chain.OutDims(w, h); err != nil {
		return
	}
	return w, h, chain, ow, oh, payload[k+n:], nil
}

func decodePrep(payload []byte, origLen int) ([]byte, error) {
	w, h, chain, ow, _, rest, err := parseFold(payload, origLen)
	if err != nil {
		return nil, err
	}
	raw, err := inflateExact(rest, w*h)
	if err != nil {
		return nil, err
	}
	back, err := chain.Inverse(matrix.FromBytes(raw, ow), w, h)
	if err != nil {
		return nil, err
	}
	return back.Pix()[:origLen], nil
}

func decodeFractal(payload []byte, origLen int, d decodeOpts) ([]byte, error) {
	w, _, chain, ow, oh, rest, err := parseFold(payload, origLen)
	if err != nil {
		return nil, err
	}
	var t fractal.Tiled
	if err := t.UnmarshalBinary(rest); err != nil {
		return nil, err
	}
	// The tiled matrix must be exactly the prepared fold of the original. That
	// also bounds the decoder's allocations by the (already limited) origLen.
	if t.Width != ow || t.Height != oh {
		return nil, fmt.Errorf("fractal matrix is %dx%d, fold of %d bytes at width %d gives %dx%d",
			t.Width, t.Height, origLen, w, ow, oh)
	}
	m, err := fractal.DecodeTiledWith(&t, d.cpus(), d.memory)
	if err != nil {
		return nil, err
	}
	back, err := chain.Inverse(m, w, (origLen+w-1)/w)
	if err != nil {
		return nil, err
	}
	return back.Pix()[:origLen], nil
}

// cmPayload codes data folded at width and prepared by chain. width 0 means a
// plain byte stream.
func cmPayload(data []byte, width int, chain prep.Chain) ([]byte, error) {
	if width == 0 {
		return append(foldHeader(0, nil), entropy.Encode(data, 0)...), nil
	}
	m, err := prepare(data, width, chain)
	if err != nil {
		return nil, err
	}
	return append(foldHeader(width, chain), entropy.Encode(m.Pix(), m.Width())...), nil
}

func decodeCM(payload []byte, origLen int) ([]byte, error) {
	if origLen == 0 {
		return nil, errors.New("empty original cannot use the cm mode")
	}
	v, k := binary.Uvarint(payload)
	if k <= 0 {
		return nil, errors.New("truncated cm header")
	}
	if v == 0 { // plain stream
		chain, n, err := prep.ParseChain(payload[k:])
		if err != nil {
			return nil, err
		}
		if len(chain) != 0 {
			return nil, errors.New("plain cm stream with a prep chain")
		}
		return entropy.Decode(payload[k+n:], origLen, 0)
	}
	w, h, chain, ow, _, rest, err := parseFold(payload, origLen)
	if err != nil {
		return nil, err
	}
	raw, err := entropy.Decode(rest, w*h, ow)
	if err != nil {
		return nil, err
	}
	back, err := chain.Inverse(matrix.FromBytes(raw, ow), w, h)
	if err != nil {
		return nil, err
	}
	return back.Pix()[:origLen], nil
}
