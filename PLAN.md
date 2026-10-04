# fpress: lossless fractal-style compression of byte matrices (Go)

## 1. Goals and constraints

1. Main input is an object implementing an interface for a 2D matrix of bytes.
2. All compression work is expressed as transformations on objects of that interface.
3. Reconstruction must be **byte-exact**. Any file (text, executable, data) is treated as an opaque
   "pseudo-2D matrix of bytes". There is no image semantics: no brightness, contrast, PSNR or PGM.
4. Language: Go (go1.24.4 installed). Standard library only (`compress/flate`, `hash/crc32`, `sync`).
5. Honest expectation: arbitrary binaries have little 2D self-similarity. The system must be
   correct always, never much worse than storing the file, and must measure whether each stage
   helps (benchmark against `flate`).

## 2. Core idea

```
file bytes
  -> fold into matrix (width chosen/detected)
  -> [prep: reversible matrix ops that expose repetition]
  -> fractal stage: per-block "recipes" build an approximation A
  -> residual R = original XOR A   (mostly zeros / predictable)
  -> entropy-code R
decode: parse -> undo nothing yet -> run recipes N fixed rounds -> A -> A XOR R -> undo prep -> truncate to length
```

Correctness never depends on the recipes being good: the encoder runs the real decoder and stores
whatever difference remains.

## 3. Matrix abstraction (requirement 1)

```go
package matrix

type Matrix interface {
    Width() int
    Height() int
    At(x, y int) byte
}
type MutableMatrix interface {
    Matrix
    Set(x, y int, v byte)
}
// optional fast paths
type RowAccessor interface{ Row(y int) []byte }
type SubMatrix  interface{ Sub(x, y, w, h int) Matrix } // zero-copy view
```

Implementations: `Dense` (row-major slice), `View` (window onto another Matrix), `Func`
(callback, for tests), `FileMatrix` (over `io.ReaderAt`, so large files need not be loaded),
helper `FromBytes(data, width)` (tail padded; exact length kept in the header).

The encoder takes a `Matrix`; the decoder writes into any `MutableMatrix` (`DecodeInto`).

## 4. Transformations (requirement 2)

All integer, all byte-exact, all in package `transform` / `prep`. No floats anywhere
(Go may fuse multiply-adds on some architectures, so floats could differ between machines).

### 4.1 Recipe transforms (fractal stage)

A recipe says: build a small **range block** from a larger **domain block** elsewhere:

1. **Decimate 2x**: keep one byte of each 2x2 group (never average: bytes are symbols, not quantities).
2. **Isometry**: one of 8 (identity, 3 rotations, 4 flips). Pure position permutation.
3. **Value map**: identity, add constant mod 256, XOR constant. Experiment: GF(2^8) `a*v xor b`.
4. Raw fallback: store the block's bytes literally.

### 4.2 Neighbor-prediction recipes (second recipe family)

Predict a byte from neighbors (left, up, average, Paeth) and store the difference. Chosen per
block next to far-patch copying, by cost.

### 4.3 Prep stage (matrix operations, all exactly reversible, recorded in header)

1. **Width detection**: score candidate widths by how often a byte equals the byte above it
   (and autocorrelation peaks); store chosen width. May be chosen per tile.
2. **Row / column differences** (XOR or subtract mod 256 against the row above / column left).
3. **Bit-plane / stride de-interleave**: split into 8 bit-planes, or split every k-th byte
   (e.g. high bytes vs low bytes of 16-bit samples) into separate planes.
4. **Integer wavelet** (Haar / reversible 5/3 lifting) for smooth numeric-like data.
5. **Experiments**: low-rank factoring over GF(2^8) via Gaussian elimination; row sorting with
   stored permutation.

The encoder tries combinations and keeps the smallest result; "none" is always allowed.

## 5. Determinism rules (decoder must be bit-exact with the encoder's simulation)

- Integer ops only; explicit clamping/rounding rules.
- **Fixed iteration count** stored in the header; no epsilon early-stop.
- Double-buffered rounds (read previous iterate, write next); parallel workers write disjoint
  regions so results do not depend on worker count.
- The encoder decodes its own output and uses that A to compute R.

## 6. Convergence caveat and fallback design

Classic fractal decoding converges because maps are contractions (|s| < 1). Byte-symbol maps are
not. Cells in self-referential cycles keep the start value; offsets can accumulate around cycles.
Consequences: output is always exact (residual covers it), but compression quality is not
guaranteed by theory.

Mitigations:
- Decimation gives spatial contraction, so most chains terminate in a few rounds.
- Encoder measures true per-block mismatch after a trial decode and demotes bad blocks to raw;
  repeat until stable.
- **Start value** = most frequent byte in the file (stored in header; optionally try top 2-3).
- **Causal fallback mode**: each domain must lie entirely in already-decoded area (single pass,
  no cycles; effectively 2D LZ77 with isometries and value maps). Built if milestone 3 shows poor
  convergence.

## 7. Residual coding

- XOR residual (matched bytes -> 0).
- Phase 1: `compress/flate`.
- Phase 2: context-modeled adaptive arithmetic coder (contexts: neighbor residuals, recipe type).
- Recipes chosen by **total estimated bits** (recipe + residual cost), per Minimum Description
  Length, not by closeness.

## 8. Encoder search

- Hash index of domain blocks keyed by canonical content (after decimation, per isometry / value
  map); candidates by lookup, then verify. Replaces the exhaustive scan.
- Quad-tree range partition: split until a recipe pays for itself, down to raw at minimum size.
- Work in tiles (default 256x256), each self-contained: bounded memory, parallel encode/decode,
  per-tile mode (raw / flate / fractal / neighbor-predict) and per-tile width.
- Quick reject: if byte entropy is near 8 bits and the hash index finds almost no repeats, skip
  straight to store/flate.
- Goroutine worker pool across tiles.

## 9. Safety nets

- Encoder decodes in memory and compares byte-for-byte; error instead of emitting a bad stream.
- CRC32 (or SHA-256) of the original in the stream, checked on decode.
- Global fallback: if result is not smaller than plain `flate` (or raw), store that with a mode
  flag. Worst case is file plus a tiny header.

## 10. Container format

Magic, version, mode flag, original length, width, tile size, iteration count, start byte, prep
chain, checksum; then per tile: mode, quad-tree split bits (bit-packed, depth-first), transform
records (domain pos, isometry 3 bits, value-map kind 2 bits + constant 8 bits), compressed
residual. Decoder validates everything (bounds, truncation) and returns errors, never panics.

## 11. Package layout

```
fpress/
  go.mod
  matrix/     interface.go dense.go view.go frombytes.go filematrix.go
  transform/  decimate.go isometry.go valuemap.go
  prep/       width.go diff.go bitplane.go wavelet.go (rank.go, sort.go experimental)
  predict/    neighbor.go
  fractal/    encoder.go decoder.go index.go format.go options.go
  entropy/    flate wrapper, later arithmetic coder
  internal/metrics/  compression ratio, mismatch counts
  cmd/fpress/  compress | decompress | bench
```

CLI: `fpress compress in out`, `fpress decompress in out`, `fpress bench files...`
(ratio versus `flate`, convergence stats, time).

## 12. Tests

- Round-trip: random, all 0x00, all 0xFF, empty, 1 byte, lengths not multiple of width, and real
  files (the Go toolchain binary, `/bin/ls`, a text file, a PNG).
- Fuzz `Encode`->`Decode` identity; fuzz the parser (no panics).
- Each transform individually invertible (property tests).
- Same compressor on `Dense`, `View`, `Func`, `FileMatrix` gives identical output.
- Determinism: golden byte-stream for fixed input; same output with different worker counts.
- Convergence test: count cells of A that differ from the original after N rounds.
- Benchmarks versus `flate` per file type.

## 13. Milestones

1. `go mod init fpress`; `matrix` package + tests.
2. `transform` package (decimate, isometries, value maps) + invertibility tests.
3. Deterministic decoder + simple fixed-block encoder + flate residual + round-trip test.
   **Experiment: decide iterative vs causal mode from measured convergence.**
4. Container format, checksum, fallback mode, CLI.
5. `prep`: width detection, row/column diff, bit-planes; encoder picks chain by trial.
6. Quad-tree, hash index, MDL cost-based recipe choice, neighbor-prediction recipes, tiling.
7. Parallelism, quick-reject, per-tile modes.
8. Benchmarks versus `flate`; then wavelet, GF(2^8) maps, arithmetic coder, rank/sort
   experiments, kept only if they pay off.

## 14. Open questions

- Tile size and default width (assumed 256 / auto-detect).
- Is the benchmark bar "beat flate", or "just correct and measurable"? (Assumed: measure first.)

## 15. Status

Milestones 1-3 are built (`matrix`, `transform`, `fractal`), all tests pass, and `FuzzRoundTrip`
ran clean for 20s. Differences from the plan as written:

- Raw "anchor" blocks exist in the stream (decoder writes them every round). Without anchors the
  iteration has no real data to start from; with them, exactly self-similar data converges.
- Encoder skips domains overlapping the block being produced (self-feeding cycles).
- Refinement demotes only failing recipes whose input is already clean (root causes), not every
  recipe that reads from a bad block.
- `Compressed` is in-memory only; `ApproxSize()` is an estimate. Real serialization is milestone 4.

Experiment (`go test ./fractal -run TestConvergenceExperiment -v`): exact Sierpinski reconstructs
100% from recipes + 1 anchor; non-self-similar data (random, /bin/ls, smooth ramps) keeps few
recipes and falls back to anchors/residual, so the estimate does not beat plain flate yet.

### Milestone 4 status

Built: `fractal/format.go` (binary serialization with bounds-checked parsing), `codec/` (container:
magic, version, mode, length, CRC32; modes stored / flate / fractal; smallest wins; own output is
decoded and compared before return), `cmd/fpress` (`compress`, `decompress`, `bench`).

Differences from the plan: width is a simple power-of-two near sqrt(n) until milestone 5 adds
detection; the fractal attempt is capped at 64 KiB until tiling (milestone 6) because the search
cost grows roughly with the square of the input.

Hardening found by tests/fuzzing: flate streams must end cleanly (truncated tails were accepted);
an empty matrix with a huge claimed other dimension no longer loops. Fuzz targets: `FuzzUnmarshal`,
`FuzzDecompress`, `FuzzCompressRoundTrip`, `FuzzRoundTrip`.

First bench (`fpress bench`): Sierpinski 16384 B -> 219 B fractal (flate 533); random, text and
`/bin/ls` fall back to stored/flate because the fractal container is larger there.

### Milestone 5 status

Built: `prep/` (steps: delta-up/left with subtract or XOR, 8-plane bit packing, k-way
deinterleave; `Chain` with exact `Forward`/`Inverse`, binary form, and `Search`/`DetectWidths`)
and codec integration. New container mode 3 (prep + DEFLATE); mode 2 (fractal) now runs on the
prepared matrix. Both payloads start with `width` + `chain`.

Width detection scores each lag by the share of the data that has the same *difference* to the byte
one lag earlier (catches exact repeats and "same plus a constant"). The first version scored exact
equality only and missed gradients; a test caught it.

Search ranks (width, chain) hypotheses by DEFLATE size on a 64 KiB sample (a stand-in for "has
exploitable repetition"); real sizes decide the final mode. The fractal stage gets the best
candidate and the best chain-free one.

Bench highlights (bytes): gradient 543 -> 70, 48-byte records 1355 -> 175, 16-bit samples
37342 -> 22007, terrain 25883 -> 15399, all in prep+flate mode. Text, random, zeros and /bin/ls are
unchanged (flate/stored win). The fractal stage still only wins on exactly self-similar data.

### Milestone 6 status

Built:
- Hash index (`fractal/encoder.go`): exact-match lookup of domain blocks under every isometry and an
  add or xor map, via canonical form relative to the first byte. `Options.Exhaustive` keeps the old
  approximate scan for experiments.
- Quad-tree: each node takes the cheapest of recipe / leave to residual / raw anchor / split into
  four, by estimated bytes (`RecipeCost`, anchor overhead). Defaults: Block 16, MinBlock 4.
- Tiling (`fractal/tiled.go`): tiles encoded in parallel (deterministic for any worker count), each
  stored as raw, DEFLATE or fractal, whichever is smallest. Tiles are read under a lock and written
  back serially, so inputs and outputs need not be concurrency-safe. Default tile 512.
- The 64 KiB fractal cap is now 4 MiB. Encode speed is roughly linear: 158 KB in ~0.4 s.

Findings:
- Larger tiles help self-similar data (Sierpinski 512x512: 1762 B at tile 128, 841 B at tile 512)
  because one tile needs one seed anchor, not one per tile.
- The fractal stage must see the square fold: width/chain candidates are ranked by DEFLATE, which
  says little about 2D self-similarity. Attempts are now best-overall plus (default width, no chain).
- Tried: replanning a failed recipe as four smaller nodes. Worse: new recipe cycles, never anchored,
  refinement ran out of rounds. Reverted to same-size anchor/residual fallback.
- Pitfall: with large blocks (32) a failed block can fall under AnchorPercent and be left
  un-anchored; recipes that depended on it then never converge.
- Deferred: neighbor-prediction recipes. Prep delta chains already provide it and fit the iterative
  decoder; a per-block predictor would need in-pass dependencies. Per-tile chain choice is the
  better route (milestone 7).

Bench (bytes): Sierpinski 16 KB -> 185 (flate 533); Sierpinski 262 KB -> 841 (flate 3229);
everything else unchanged from milestone 5, with fractal tiles falling back to DEFLATE.

### Milestone 7 status

Built:
- `codec/block.go`: `encodeBlock` (the stored / flate / prep+flate / fractal selection, now reusable
  per byte range) and the quick reject `hopeless`: order-0 entropy >= 7.9 bits, fast DEFLATE saves
  < 1%, and no period found in the front of the data. Small inputs (< 4096 B) are never rejected.
  4 MiB of random data: 2.8 s -> stored almost immediately.
- `codec/segment.go`: mode 4, fixed-size segments (default 64 KiB, min 1 KiB), each independently
  encoded in the best of modes 0-3, in parallel; decoding is parallel too. Compress keeps it only if
  smaller than the best whole-file mode. This is the per-region width/chain/mode adaptivity planned
  for "per-tile modes", done at the 1D segment level, where it reuses everything else unchanged.
- CLI: `-segment`, `-no-reject`; `bench -ext` adds xz -9e, zstd -19, bzip2 -9 as references;
  CLI now has tests.

Reference comparison (bytes; lower is better), from `fpress bench -ext`:

| file | flate | fpress | xz | zstd | bzip2 |
|---|---|---|---|---|---|
| sierpinski 16 KB | 533 | 185 | 360 | 293 | 655 |
| sierpinski 262 KB | 3229 | 841 | 1208 | 1514 | 2654 |
| gradient 8 KB | 543 | 70 | 400 | 439 | 845 |
| records 14 KB | 1355 | 175 | 864 | 971 | 1475 |
| 16-bit audio-like 40 KB | 37342 | 22007 | 22168 | 34821 | 31391 |
| terrain 32 KB | 25883 | 15399 | 19172 | 21442 | 15644 |
| text 35 KB | 12078 | 12078 | 11452 | 11555 | 10706 |
| /bin/ls 159 KB | 72812 | 72235 | 60548 | 64392 | 69604 |
| mixed 238 KB | 120377 | 105282 | 97020 | 111062 | 120140 |
| big exe 897 KB | 458728 | 458728 | 369880 | 393301 | 446824 |

fpress wins on structured data and trails xz on general text/code: the backend is DEFLATE (32 KiB
window, Huffman only). The remaining gap is the entropy-coding/matching backend, not the transforms.

### Milestone 8 status

Built `entropy/`: a bitwise context-mixing arithmetic coder, integer-only (even its tables are built
with fixed-point integer arithmetic, so streams are identical on every architecture). Models:
orders 0-4 and 6, a long-match model, and, when a row length is given, three contexts from the row
above (above; above+left; above+above-left+above-right). A learned mixer (weight set chosen by
partial byte and match state) blends them, then two adaptive stages sharpen the result. Counters sit
in 16-entry cache-line buckets chosen per nibble. Tuned on a small corpus, then frozen as constants.

Container mode 5 (`cm`): optional fold + prep chain, then the coder with stride = prepared row
length. Tried per block: best DEFLATE-ranked candidate, a plain byte stream, and the best chain-free
candidate (fewer attempts for large inputs). Segmented mode (4) may use it per segment.
Throughput is about 1 MB/s each way; `MaxCMSize` (default 16 MiB) caps it.

Verification: golden checksum of an encoding (guards against accidental model changes); the full
test suite passes as a GOARCH=386 build; containers written by the amd64 binary decode on the 386
binary and vice versa, for every mode. This caught a real 32-bit build break (`maxCells = 1<<31`),
now 1<<30. Truncation tests caught an empty-payload slice panic in the new decoder. Fuzzed:
entropy Decode/RoundTrip, container Decompress, Compress round trip.

Final reference comparison, bytes (`fpress bench -ext`):

| file | flate | fpress | mode | xz | zstd | bzip2 |
|---|---|---|---|---|---|---|
| sierpinski 16 KB | 533 | 35 | cm | 360 | 293 | 655 |
| sierpinski 262 KB | 3229 | 841 | fractal | 1208 | 1514 | 2654 |
| gradient 8 KB | 543 | 30 | cm | 400 | 439 | 845 |
| records 14 KB | 1355 | 87 | cm | 864 | 971 | 1475 |
| 16-bit audio-like 40 KB | 37342 | 18853 | cm | 22168 | 34821 | 31391 |
| terrain 32 KB | 25883 | 12964 | cm | 19172 | 21442 | 15644 |
| terrain 480 KB | 324716 | 177542 | cm | 242528 | 257352 | 202548 |
| text 35 KB | 12078 | 9837 | cm | 11452 | 11555 | 10706 |
| /bin/ls 159 KB | 72812 | 61341 | cm | 60548 | 64392 | 69604 |
| mixed 238 KB | 120377 | 93779 | segmented | 97020 | 111062 | 120140 |
| big exe 897 KB | 458728 | 377306 | cm | 369880 | 393301 | 446824 |

Not done: coding the fractal stage's residual and tile fallbacks with the new coder (they still use
DEFLATE); GF(2^8) value maps, rank factoring and row sorting experiments.

### Fractal residual now uses the context-mixing coder

`Compressed.Residual` starts with a mode byte: 0 = every cell matched (nothing follows), 1 = DEFLATE,
2 = context-mixing coder with the tile width as row length. The encoder keeps the smallest; the slow
coder is skipped when over half the cells are wrong (the fractal stage has failed there anyway).
Container format version is now 2 (the fractal payload layout changed).

Most of the win is the zero marker: when recipes reproduce a tile exactly, DEFLATE still spent
37-274 bytes saying "all zeros" (a third of the Sierpinski 262 KB stream); now it is 1 byte.

Fractal-mode sizes, bytes (before -> after): sierpinski 16 KB 185 -> 148, sierpinski 262 KB
841 -> 567, gradient 76 -> 50, records 183 -> 109, 16-bit audio-like 22017 -> 21837. Sierpinski
262 KB is still best in fractal mode (567 vs cm 3189, xz 1208).

Also fixed: DEFLATE streams followed by extra bytes were accepted (fractal and codec copies of
inflateExact); both now reject trailing bytes.

Remaining cost in a working fractal stream is the transform records (~95% of the Sierpinski 262 KB
stream), still stored with DEFLATE.

### Fractal transform records now use the context-mixing coder too

`fractal/records.go`: records are stored column by column (flags, sizes, rx, ry, dx, dy, map
constants, raw anchor data), positions as zigzag deltas from the previous record, and the whole run
by `packBlob`: the smallest of stored / DEFLATE / context-mixing (cm skipped above 1 MiB). Container
format version is now 3.

Records alone, Sierpinski (old layout + DEFLATE -> new): 256: 227 -> 120 bytes; 512: 529 -> 192;
1024: 1396 -> 326. Each ingredient helped: cm alone about -29%, columns another -25%, deltas another
-35%.

Fractal-mode sizes, bytes (after residual step -> now): sierpinski 16 KB 148 -> 118, sierpinski
262 KB 567 -> 230 (xz 1208), gradient 50 -> 47, records 109 -> 106, 16-bit audio-like 21837 -> 18593,
/bin/ls 72622 -> 72384.

Defence in depth: a packed blob may not claim to expand beyond 16384:1 (the coder can compress long
runs by far more, so a tiny hostile input could otherwise demand a large allocation); every length is
checked against the bytes present before it is used.

### GF(2^8) value maps: measured, not integrated

Question: would a map `v' = a*v ^ b` over GF(2^8) (AES polynomial) let the fractal stage find
exact matches that identity/add/xor cannot? Measured before building any of it: for every 8x8
range block, does some decimated 16x16 domain match it exactly under any of the 8 isometries and
(a) add/xor or (b) GF-affine? Canonical form for GF: XOR the first byte, then scale so the first
nonzero byte is 1 (so one hash lookup covers the whole family).

Detector validated by a positive control (blocks built as GF-affine copies of decimated domains:
128/128 found by GF, 0 by add/xor).

| data | non-flat blocks | match via add/xor | match via GF | GF only |
|---|---|---|---|---|
| text, /bin/ls, 900 KB executable, records, 16-bit audio-like, terrain, mixed | 120 - 14015 | 0 | 0 | 0 |
| 3x+y gradient (8 KB file) | 120 | 0 | 100 | 100 |
| pure ramp 256x256 | 1024 | 0 | 1024 | 1024 |
| 3x+y ramp 256x256 | 1024 | 0 | 832 | 832 |
| bowl / smooth clean surface | ~1020 | 60 / 61 | 2 / 46 | 0 / 7 |
| x+y/2 ramp, checker, scaled Sierpinski | | 0 | 0 | 0 |

Why GF finds ramps: multiplying by the field element 2 equals integer doubling while values stay
below 128, so GF can say "same ramp at half the slope" - the classic fractal contrast scaling.

Why it is not worth integrating: real data has no exact matches under any map family; the only hit
is noise-free ramps, which the existing pipeline already codes in 38 bytes (256x256 ramp) and 186
bytes (3x+y ramp). A fractal stream with zero recipes already costs 37 bytes of fixed overhead
(13 container + 24 payload), so even a perfect recipe set could not beat the context-mixing mode.
Approximate GF matching (exhaustive search over 255 multipliers) would be ~255x slower and has no
natural data to help. Decision: leave the value maps as identity/add/xor.

### Rank factoring and row sorting: measured, not integrated

Same method as for GF value maps: measure whether the structure exists in real data, and what it
would have to beat, before building anything. Yardstick: the actual context-mixing coder.

**Rank factoring** (write M = A*B over GF(256) with small inner dimension r; stores r*(h+w) bytes).
Exact rank by Gaussian elimination over GF(256), whole matrix and aligned 16x16 / 32x32 blocks.
Detector validated by a planted rank-4 matrix (found: 4 of 128; a random matrix: full rank).
- Full rank, deficiency 0: gradient, 16-bit audio-like, terrain, text.
- Executables: deficiency 9-11 of 256 (runs of zero bytes and repeated rows, the trivial kind);
  storing factors would take more than the matrix (247*(256+256) > 256*256).
- Blocks of the Sierpinski pattern and the executables are often rank deficient (38-66% of
  blocks) for the same trivial reason: mostly-zero blocks, which the start value already handles.
- The only real hit is the synthetic fixed-record file (rank 4 of 48 columns), which already codes to
  87 bytes; the 300x4 coefficient matrix would itself need coding.

**Row sorting** (reorder rows to group similar ones; the permutation costs about log2(h!) bits).
Coded size in original order vs lexicographic and greedy nearest-neighbour order, permutation added:
- Net worse on text (+1368), terrain (+61), executables (+510), the gradient (+50), records (+299);
  Sierpinski goes 19 -> 297 bytes because sorting destroys the 2D self-similarity.
- Planted shuffled-record tables are where it wins: 24 distinct records shuffled 2341 -> 1841 (-13%
  net of permutation), noisy copies of 6 prototypes 3837 -> 2895 (-19%).
- The one real-data hit (16-bit audio-like, nearest-neighbour, -2.3%) was at an arbitrary width of
  200. At the layout the pipeline actually uses (width 502, deint2+up-sub) the nearest-neighbour
  order is the original order and the result is +49 bytes (the permutation).

Decision: neither is integrated. If your data includes unsorted fixed-width record tables, sorting
would pay roughly 13-19%; it would need a data-dependent chain step carrying its permutation, which
the current parameter-free prep steps do not support.

Side note from this run: since the fractal records now use the cm coder, the 16-bit audio-like file
is smallest in fractal mode (18593 B) rather than cm (18853 B).

### Speed presets (`fpress compress -preset default|fast|fastest`)

Profile of the default pipeline on 8 MiB of distinct real files: 38 s wall, 197 CPU-seconds, and
1.7 GB at 2 MiB (fractal tiles). Causes: the context-mixing coder runs once or twice over the whole
input (single-threaded in both directions) and then again under every segment's own candidates;
each segment trials ~48 width/chain combinations with DEFLATE; the fractal stage holds large hash
indexes per tile. 97% of CPU in the fast path is the coder itself (about 1 MB/s per core, ~0.4 MB/s
per core when 12 cores contend for memory).

New options: `SkipWhole` (segments only, after the quick reject), `CMAttempts` (layouts the coder
tries per block), `SearchSample` (bytes the layout search samples), `FastFlate` (DEFLATE default
level). `codec.PresetOptions(p)` bundles them; the CLI applies a preset first and then only flags
the user actually gave (tested, because a flag default must never undo a preset).

| preset | segments | modes | other |
|---|---|---|---|
| default | whole input, then 64 KiB segments | all, incl. fractal (<= 4 MiB) | unchanged |
| fast | 256 KiB, in parallel | stored/flate/prep + cm, 3 layouts per segment | no fractal, 16 KiB search sample, fast DEFLATE |
| fastest | 1 MiB, in parallel | stored/flate/prep only | no cm, no fractal |

Measured on 12 cores (compress wall time; decompress; output as % of input):

| input | default | fast | fastest |
|---|---|---|---|
| 2 MiB | 12.9 s, 2.3 s, 41.7% | 1.1 s, 0.5 s, 43.3% | 0.1 s, 0.0 s, 47.6% |
| 8 MiB | 25.2 s*, 9.3 s, 39.8% | 4.0 s, 2.0 s, 42.5% | 0.1 s, 0.0 s, 47.2% |
| 24 MiB | (not run) | 11.9 s, 5.4 s, 40.9% | 0.5 s, 0.1 s, 45.7% |

*default with the fractal stage disabled; with it, 37.8 s.
Decompression also gets faster: segments decode in parallel, where a whole-input coder is serial.

Memory: the coder's model is about 47 MB per live segment. Models are now pooled and reset
(`entropy.acquireModel`), tested to give byte-identical output to fresh ones across mixed strides,
sizes and concurrent use. `fast` on 8 MiB: 1006 MB -> 675 MB. On 24 MiB, `-workers 2/4/12`:
20.9 s / 14.8 s / 12.1 s and 597 / 720 / 1180 MB, so `-workers 4` is a good setting for a small
machine.

Also changed: the coder's second final-stage table shrank from 2^16 to 2^12 contexts (+0.04% size,
about 8% faster, 4 MB less per model); container format version is now 4.

Measured but not changed: counter table size. Doubling it twice gains about 1.3% size for 4x the
memory and 16% more time; halving it costs about 1%. It is derived from the input length, so a
per-preset size would need a stream parameter; not worth it for ~1%.

Known cost not addressed: the default preset on inputs of a few MiB is still slow and, with the
fractal stage on, memory-hungry (1.4 GB at 2 MiB). `MaxFractalSize` (4 MiB) is worth lowering if that
matters.

### Correction to the `fast` preset (tested with the fractal stage on, and tuned)

Testing `fast` with the fractal stage re-enabled (`-no-fractal=false`) showed two things.

**Fractal in `fast`.** It helps only when the whole self-similar image fits in one 256 KiB segment
(Sierpinski 262 KB: 3055 -> 230 bytes, equal to the default). Past one segment it loses most of the
benefit, because segments cannot see each other (1 MiB: default 661, fast 8227, fast+fractal 4060;
4 MiB: 2000 / 19870 / 15682). On ordinary data it gains 0.01-0.02% for ~4x the time and ~70% more
memory (8 MiB: 4.3 s -> 17.9 s; 24 MiB: 12 s -> 55 s). It stays off in `fast`.

**The first `fast` was mis-tuned.** It tried one coder layout per segment, picked by how well DEFLATE
liked it. DEFLATE's favourite layout is often a poor one for the context-mixing coder: /bin/ls was
72950 bytes against 61379 for the default (+19%, identical to `fastest`, i.e. the coder won nothing),
mixed.bin +23%. The earlier "4-7% larger" claim held only for the large files tested.

Tried and rejected: choosing the layout on a 16 or 32 KiB sample of the segment and then coding the
whole segment once. Unreliable (16 KiB picked the wrong layout on ls and mixed, and on a 2 MiB file
the result, 912219, was worse than the single layout's 907196), because the coder learns as it goes
and the start of a block is not representative. Removed.

Fix: `fast` tries three layouts in full per segment (`CMAttempts = 3`). Result vs default:

| file | default | fast (now) | fastest |
|---|---|---|---|
| /bin/ls 159 KB | 61379, 2.0 s | 61379, 0.6 s | 72950 |
| mixed 238 KB | 93758, 2.2 s | 102445, 0.9 s | 120459 |
| 900 KB executable | 377935, 8.0 s | 383427, 1.5 s | 459031 |
| 2 MiB | 873997, 13.1 s | 885711, 2.1 s | 999006 |
| 8 MiB | 3337360, 35.4 s | 3469176, 7.5 s | 3959673 |
| 24 MiB | not run | 10018370, 21.4 s | 11508192 |

So `fast` is within 0-4% of the default on code and large mixed files (9% on the 238 KB mixed file,
where the default's 64 KiB segmenting adapts better) at 3-5x the speed, unpack 4x faster.

## Parallelization plan (built; see "Parallel fractal and any-size input" at the end)

### Where we are

Parallel today: tiles in the fractal step (one tile per worker), segments in segmented mode, and
decoding of tiles and segments. Serial today: everything inside one tile (index build, match search,
trial-decode rounds, packing); the candidate attempts of one block (cm layouts, fractal attempts,
the ~48 layout trials); in the default preset, the whole-input pass runs fully before the segmented
pass starts; the entropy coder (one adaptive bitwise stream, serial in both directions). Measured
CPU/wall ratios: default on 2 MiB ~5 cores busy on average, `fast` on 24 MiB ~10.7.

### Design rules

1. **Shared tables are built once, then frozen.** A frozen table is read by any number of workers with
   no locks. This is the "common memory table": the domain pools and the match index.
2. **The only shared mutable state is the working grid**, double-buffered per round: workers read the
   previous round's grid and write disjoint cells; a barrier ends each round. No message passing for
   data; channels only hand out work and collect results into fixed slots.
3. **Output must not depend on scheduling.** Results go to indexed slots and are merged in a fixed
   order; sharded structures keep insertion order. Same bytes for any worker count (tests already
   check this for tiles and segments; extend to every new parallel section).
4. **One CPU budget for the whole run**, not a worker count per layer. Today nested parallelism is
   avoided by hand (segments force fractal workers to 1). Replace with a shared token pool: any task
   takes a token to run, nested work asks the same pool, so segments x tiles x attempts never
   oversubscribes and never starves.
5. **Memory is part of the design.** Parallel workers multiply memory (fractal indexes use ~1.4 GB at
   2 MiB; the coder ~47 MB per live model). A shared index replaces one per worker; the index becomes
   a compact open-addressed table instead of Go maps of slices; an optional memory limit caps workers.

### Phases

| # | Work | Format change | Expected effect | Risk |
|---|---|---|---|---|
| 0 | Instrument: per-stage wall time and cores busy, in `bench` | no | shows what actually waits | none |
| 1 | CPU budget (token pool); run independent attempts concurrently: the up-to-3 cm layouts, the fractal attempts, the ~48 layout trials, and (default preset) whole-input and segmented passes at the same time | no | default preset several times faster on mid-size files; `fast` a little | low |
| 2 | Parallel inside a fractal tile: eager sharded index build; plan the 16x16 root blocks in parallel; run each trial-decode round in parallel over transforms (barrier per round); mismatch counting in parallel; pack residual and records concurrently | rule added: range blocks must not overlap (encoder already satisfies it; needed so parallel decode is deterministic on hostile streams) | one big image/tile uses all cores; lower memory per worker | medium |
| 3 | Fractal across tiles: one shared global index and one global round loop; "tiles" become only a work split, not a similarity boundary; stream becomes one Compressed with chunked entropy blobs | yes (new stream layout, version bump) | large self-similar files stop losing similarity at tile edges (16 MiB Sierpinski: expect far below the 6194 bytes of 512-px tiles); memory shared rather than multiplied | high |
| 4 | Independent entropy chunks: records and residual blobs coded as several streams in parallel | yes | removes the serial tail on big streams; small size cost | low |

### What cannot be parallelized, and why

- **One cm stream.** Each bit's probability depends on every bit before it, so the decoder cannot run
  ahead. The only way to parallelize is independent streams (segments, phase 4), which cost some
  compression. Whole-input cm decode stays ~1 MB/s; for big files the segmented layout is the answer.
- **Rounds of the fractal iteration.** Round k+1 needs all of round k. Within a round everything is
  parallel; between rounds there is a barrier.
- **Demotion decisions** (which failed recipes to remove) depend on each other; only the counting
  that feeds them is parallel.

### Cross-tile question (phase 3), in plain terms

Today a tile can only copy from itself. Letting any block copy from anywhere in the image removes the
cost of splitting, but the decoder then needs the whole image in memory and all workers meet at a
barrier every round. A cheaper alternative (tiles may copy only from earlier tiles) is rejected: it
makes decoding sequential.

### Acceptance tests for every phase

Byte-identical output for workers 1, 2, 3, 8, 64; -race clean; existing golden and fuzz tests pass;
cross-architecture decode (amd64 writes, 386 reads); for phases that promise unchanged output
(0, 1, 2), the output equals today's byte for byte; measured wall time and peak memory on the 2, 8
and 24 MiB files and the 16 MiB Sierpinski image recorded here.

### Questions to settle before starting

1. Is cross-tile similarity (phase 3) worth a format change for your data?
2. A memory ceiling to design for (for example 2 GB, 8 GB)?
3. Which matters more first: speed of the default preset on mid-size files (phase 1), or fractal on
   one large image (phases 2-3)?


## Parallel fractal and any-size input (built)

Goal set by the user: parallelize the fractal implementation as far as it goes, let tasks share a
common table, manage memory (temporary files allowed), and make fractal compression work at any file
size. No format-compatibility constraints (still in development).

### What was built, and why this shape

**Profile first.** The fractal step alone took 2.8 s single-threaded for a 4 MiB self-similar image
(0.69 s on 12 workers: tiles already ran in parallel). The 18 s seen through the CLI was the rest of
the pipeline. Inside a tile nothing was parallel, 33% of CPU was `compress/flate` (the encoder
DEFLATE-compressing every tile just to compare modes), 28% was building the match index and 21% was
matrix interface calls.

1. **Parallel, fused rebuild** (`fractal/rebuild.go`). The trial-decode rounds run across
   goroutines (each transform writes only its own range block; barrier per round); the per-block
   decimate/orient/map steps are fused into one loop (a test proves equality with the composed
   matrix operations). New format rule: range blocks must not overlap (`checkDisjoint`, bitmap),
   so parallel decoding is deterministic even for hostile streams.
2. **Match search without an index** (`fractal/match.go`). The old design built a map of every
   domain block's canonical form under 8 isometries: memory grew with the image and the build was
   serial. Now the nodes that need a recipe are hashed into a small frozen table (the shared common
   table: sharded open addressing built in parallel, plus a bloom filter), and every domain block is
   *streamed* past it by parallel workers. A hit offers its entry id to the node group, which keeps
   only the lowest and highest few; those sets depend on the set of hits, not their order, so output
   is independent of scheduling. Each node then takes the first valid candidate (non-overlapping,
   verified). Memory is independent of the number of domain blocks. Verified three ways: output
   byte-identical to the old index on all nine golden cases; a brute-force agreement test (found iff
   a recipe exists, and every recipe reproduces its block); identical bytes for 1..64 workers.
3. **Planning in parallel**: the quad-tree DP runs per root block in parallel into fixed slots;
   mismatch counting for demotion and the residual computation are parallel loops.
4. **Chunked entropy coding** (`entropy/chunked.go`): one adaptive stream cannot be coded or
   decoded in parallel, so the residual and the transform records are coded as independent
   chunks (default 256 KiB) in parallel; +6.4% on text at that size (24% at 64 KiB: every chunk
   re-learns). One chunk costs 1 byte. Size caps for the slow coder raised accordingly.
5. **Encoder tile selection**: stored/DEFLATE are not computed when the fractal result is smaller
   than 1/1032 of the tile (DEFLATE's hard ratio limit, tested), otherwise DEFLATE runs alongside
   the fractal encode on spare cores. Tiles are read a row at a time.
6. **Memory model** (`fractal/memory.go`): measured peak heap: ~12 B/cell self-similar, ~26 B/cell
   worst case (all raw anchors), plus ~35 MB per worker for coder models. `Options.MemoryLimit`
   (default 8 GiB) drives: tile size (0 = automatic: the whole matrix as ONE tile when it fits,
   so any block can copy from anywhere; otherwise the largest tiles with ~4 in flight), how many
   tiles run at once, and the workers left over for each tile's own parallel stages. Tile side up
   to 32768.
7. **Candidate attempts run side by side** (`codec/block.go`): flate, layout search, prep+flate,
   the fractal attempts and the cm layouts are independent; they run concurrently into fixed slots
   and are compared afterwards in the original order. The whole-input pass and the segmented pass
   also run concurrently. Output unchanged.
8. **Any size** (`codec/stream.go`, new container mode `big`): `CompressStream` reads an
   `io.ReaderAt` and writes an `io.Writer`. A first pass computes the CRC (so the header can come
   first and the output can be a pipe). Inputs larger than the super-segment size (memory budget /
   128, at most 1 GiB) are cut into super-segments, each compressed by the same logic as
   `Compress` (all modes, the fractal engine on all cores) and verified by decoding before it is
   written; memory follows one super-segment, not the input. `DecompressStream` decodes the same
   way. The CLI streams, writes outputs under a temporary name and renames on success, and spills
   standard input to a temporary file. The old 1 GiB original-size cap and 4 MiB fractal cap are gone.

### Temporary files

Not needed for the fractal matcher: because domain blocks are streamed past a small table instead
of indexed, memory no longer grows with the number of domain blocks, and the work that does grow
with the input (the grids) is bounded per super-segment. Temp files are used only where an input
cannot be read twice (standard input is spilled to one) and for the atomic output rename.

### Known limits

- Similarity is found within a tile, and within a super-segment; a super-segment boundary is a wall
  (default 64 MiB at the default budget).
- Single-tile parallel speedup is about 3x on 12 cores (planning scales; the rest has serial
  stretches). Many tiles scale near-linearly.
- A prep chain that restructures the whole matrix (bit planes, deinterleave) applies per
  super-segment, not across the file.

### Results (12 logical cores)

Fractal step alone, 16 MiB self-similar image (Sierpinski 4096x4096), best of 2 runs:

| tiles | workers | time | speedup | size |
|---|---|---|---|---|
| one 4096 tile | 1 / 2 / 4 / 8 / 12 | 3.51 / 2.04 / 1.67 / 1.34 / 1.29 s | 1.0 / 1.7 / 2.1 / 2.6 / 2.7x | 1209 B |
| 64 tiles of 512 | 1 / 2 / 4 / 8 / 12 | 1.72 / 0.97 / 0.61 / 0.47 / 0.44 s | 1.0 / 1.8 / 2.8 / 3.7 / 3.9x | 6204 B |

The matcher/planner stage scales ~3-4x; DEFLATE-free tile selection, the serial copy of the
input, hashing of the whole grid and the final packing are the serial stretches. Output bytes are
identical for every worker count (tested for 1, 2, 3, 8, 64). One large tile is 5x smaller than
the same image in 64 tiles because blocks can copy from anywhere in it.

Whole pipeline, default preset: 16 MiB Sierpinski 48,906 B (old 4 MiB fractal cap) -> 1,226 B;
4 MiB Sierpinski 2,000 -> 636 B. Real-file collections unchanged in size.

Any size, `-memory 4G -width W -no-cm -no-prep -segment -1` (fractal-focused), super-segments of 32 MiB:

| input | compressed | ratio | compress | peak RSS | decompress | peak RSS |
|---|---|---|---|---|---|---|
| 1 GiB Sierpinski (32768^2) | 21,829 B | 49,188:1 | 1m50s | 555 MB | 21 s | 390 MB |
| 4 GiB Sierpinski (65536^2) | 81,407 B | 52,761:1 | 5m38s | 628 MB | 73 s | 401 MB |

Both decompress byte-for-byte identical. Memory stayed flat as the input quadrupled.

Verification: full suite, `GOARCH=386` suite, race detector (fractal, entropy, par, cmd, and the
concurrent parts of codec), eight fuzzers on every parser/decoder touched, and big containers
written by amd64 read by a 386 binary and the reverse. Container format version is now 5.

## Round: verify once, same-scale copies, the fractal gate

Three items from the improvement assessment, in the order built.

### 1. Verify once (`Options.NoSelfCheck`)
`fractal.Encode` decodes and compares its own output; the codec then verifies the container again,
and the streaming path a third time. The codec now turns the inner check off (the candidate it keeps
is verified once, at the outermost level; discarded candidates are never verified). Output
byte-identical. Default preset: 54.2 -> 45.5 CPU-s on 2 MB, 240 -> 203 on 8 MB; memory -9% / -24%.
(The wall time on a single big tile did not move: the inner check overlapped other work there.)

### 2. Same-scale copies (`Transform.SameScale`, stream flag bit 6)
A recipe could only copy from a 2x larger, subsampled region, so a repeated tile, glyph or record at
its own scale was not expressible. Measured first: exact same-scale copies exist for 100% of 8x8
blocks in a tile map and in a gradient, 59% in a records file, 9% in a mixed file, and for none in
text, code, audio-like samples or terrain; the fractal step found zero recipes in all of them.

Built into the matcher as a second family of domains sharing the one query table (entry id carries
a kind bit; the decimated kind has the lower ids, so it is preferred and old behaviour is unchanged
when copies are off: the nine original golden fingerprints are byte-identical with `NoSameScale`).

Problems found and fixed, in order (each by a measurement, not a guess):
- **Cycles.** With thousands of identical blocks each copied the lowest-numbered other copy, so the
  first two copied *each other* with nothing anchoring them; repair rounds fixed one pair per round.
  An 8x8 tile map came out 4.7x LARGER with copies (89,758 vs 19,193 B). Fix: a copy may only read
  data earlier in raster order (`copyIsCausal`); the first occurrence is an anchor, as in LZ77.
  After: 9,006 B (2.1x smaller).
- **Sparse blocks.** The decoder copies from its current approximation, in which a sparse block is
  still only the start value, so a copy of a sparse block always fails. Copies are offered only to
  blocks dense enough to be anchored (`denseEnough`).
- **Failure leaves a worse plan.** A parent split into children because each had a cheap copy
  becomes four anchors when the copies fail. A failed copy now sends its whole root block back to
  the plan the encoder made before copies existed (`planNodeOpt(..., false)`).
- **Cost model.** The planner assumes an anchor costs ~1 byte/cell; the context-mixing coder codes
  repeated short rows far more cheaply, so copies of tiny blocks can lose (mixed file +0.9%, records
  +24% in fractal mode). Rather than tune a threshold, `Encode` plans the shared match results a
  second time without copies when copies were used and keeps the smaller: never larger than before.
  (A first version re-ran the whole encode, matcher included; sharing the matches is the cheap form.)
- **Cost of the second family.** The match stage doubled (1.6 s -> 3.4 s per 32 MiB) and found
  nothing on the Sierpinski image. The probe (below) reports which families have hits, and the full
  search is narrowed to those (`ProbeResult.Narrow`, `Options.NoDecimated`).

Results (fractal-mode bytes, copies off -> on): tile map 1024x1024 of 8x8 tiles 19,490 -> 9,065;
512x512 of 16x16 tiles 5,862 -> 4,316; synthetic records, gradient, text, code, audio, terrain: not
larger. Final default output unchanged on every ordinary file.

### 3. The fractal gate (`fractal.Probe`, `Options.NoFractalGate`)
On ordinary files the fractal step costs several times everything else and changes nothing. Probe
runs the real matcher on blocks of 8 and 4 with domains on an aligned grid (about a quarter of the
full search), counting hits on non-constant blocks. Measured hit rates: ordinary data never above
**1.5%** (text, executables at several widths, audio-like, terrain, a 2 MB collection); structured
data never below **10.9%** (mixed file 10.9%, records 27-71%, gradient 93%, tile maps 97-99%,
Sierpinski 100%). Threshold 4% (and at least 16 hits), a margin of 2.7x each way. Blocks under 32 KiB
are not probed. Pinned by `TestProbeThreshold`.

An earlier version of the threshold patch silently failed to apply (gofmt had re-aligned the block)
and the gate tests still passed because their inputs sat far from either threshold; it showed up
only as an unexpectedly small speed-up. The patch is now asserted, and the threshold has its own test.

Default preset, ordinary files, output identical:

| input | gate off | gate on | floor (no fractal at all) |
|---|---|---|---|
| 2 MB real files | 76.7 CPU-s, 9.8 s, 1,266 MB | 31.7 CPU-s, 6.5 s, 550 MB | 24.1, 6.0 s, 404 MB |
| 8 MB real files | 305 CPU-s, 38.2 s, 1,795 MB | 116 CPU-s, 24.9 s, 668 MB | 86.5, 23.0 s, 580 MB |

Against the start of the round (8 MB: 240 CPU-s, 34.0 s, 2,429 MB): -52% CPU, -27% wall, -72% memory.
Cost on self-similar data: the probe adds about 10% (1 GiB Sierpinski: 110 s -> 121 s; 21,829 B and
decompression unchanged).

### Also found
A data race (found by the race detector on a new test): the segmented pass read a flag the
whole-input pass was writing when the two ran side by side. Harmless in practice (the flag was
always false there) but a race; the segmented pass no longer reads shared state.

### Still open from the assessment
Larger root blocks (dependency-aware planning was measured and dropped, see the end); references across
super-segments; denser domain grids for unaligned copies; approximate matching; cost-model
calibration.


### `decompress -memory` (done)
`DecodeOptions{MaxSize, MemoryLimit}` / `DecompressStreamOpt`; CLI `fpress decompress -memory 2G in out`.
The limit does two things. (1) It caps how many pieces decode in parallel: all CPUs, fewer if the
budget (minus 256 MiB of slack, at ~96 MiB per goroutine) allows fewer, never fewer than one;
pieces nested inside a parallel piece do not fan out again. (2) It refuses, with `ErrMemory` and
before allocating anything for the piece, a container whose single pieces cannot fit: the piece size
is chosen at compression time and stored in the file, so no parallelism setting can shrink it. The
per-mode estimate (`decodeNeed`): stored/flate 2x the piece, prep 4x, segmented 3x, fractal 12x
(grids dominate), cm 2x plus its model (~40 bytes per table slot, growing with the piece: 18 MiB for
a 64 KiB piece, ~90 MiB at most). Errors are reported as memory problems, never as corruption (an
earlier version wrapped them as "corrupt container"; caught by a test). No limit given = no check.

## Measurement: how much does the repair loop cost? (nothing built)
Question: before building dependency-aware planning, how much output is lost to the encoder's
repair loop (recipes that fail when really decoded are demoted to raw anchors)? Measured with
temporary counters in `demote` (removed again), default options, `EncodeTiled`, serialized size.

| input (1024x1024 unless noted) | size | demoted recipes | cells turned into anchors |
|---|---|---|---|
| Sierpinski (also 2048) | 349 B (619 B) | 1 | 256 (the seed block, needed anyway) |
| tile map 8x8 / 16x16 tiles | 9,006 / 2,494 B | 0 | 0 |
| repeated rows, smooth field | 1,150 / 1,779 B | 0 | 0 |
| tile map, 64 palette, 0.1% noise | 36,568 B | 0 | 0 |
| Sierpinski + 0.1% noise | 4,510 B | 1 | 512 |
| Sierpinski + 1% noise | 31,362 B | 2,081 | 65,712 |
| plasma (diamond-square terrain) | 12,367 B | 576 (hit the 32-round cap) | 4,736 |

Conclusions:
- On exactly self-similar data (the case the fractal stage is for) the repair loop is essentially
  idle: 0-1 demotions, nothing to gain. **Dependency-aware planning is not worth building for it.**
- The repair loop is worth keeping: switching it off (`MaxRefine=0`) makes noisy Sierpinski 3-14%
  larger (0.1%: 5,273 vs 4,510 B; 1%: 32,378 vs 31,362 B) and plasma 13% larger.
- Only noisy/approximate data (1% noise, plasma) demotes much. There I cannot separate "loss" from
  the noise's own cost (1% noise has ~19 KB of entropy by itself; output 31 KB), so an upper bound
  is all this gives. Plasma uses all 32 rounds, so the round count (not planning) is the only
  visible inefficiency, and plasma is not exact self-similarity. Not pursued.
- A tile map with 1% noise gets no recipes at all (64,398 B, all anchors): exact matching cannot
  cope with scattered noise. That is the "approximate matching" item, a bigger prize than repair.
Next candidates: larger root blocks, approximate matching (now with a concrete failing case).

## Measurement: is approximate matching worth building? (no)
Question: scattered noise (1% of cells changed) stopped the fractal stage finding recipes. Would
matching "nearly the same" blocks and patching the differences pay? Measured on 512x512 inputs
(temporary tests, removed), three ways.

1. **Do near-copies exist?** For every aligned 8x8 block, the fewest differing cells against any
   earlier block (same-scale) or any 2x-decimated block, on a stride-4 grid, identity isometry:

| input | exact | 1-2 cells off | 3-4 | 5-16 | none within 16 |
|---|---|---|---|---|---|
| Sierpinski + 0.1% noise | 94.7% | 5.3% | 0 | 0 | 0 |
| Sierpinski + 1% noise | 60.7% | 38.1% | 1.2% | 0 | 0 |
| tile map + 0.1% / 1% noise | 94% / 53% | 6% / 44% | 0 / 2.7% | 0 | 0.1% |
| plasma terrain | 0 | 0 | 0.1% | 15.7% | 84.4% |

   On noisy exact-structure data nearly every block has a near-copy (the rest are already exact
   copies). On terrain, which is only statistically self-similar, almost none do within 16 cells.
   So approximate matching helps noisy exact data and does not help terrain.

2. **But exact matches were already there and unused.** Half the blocks of the 1%-noise tile map
   have an exact copy, yet the fractal stage found none at the 16x16 root level (a 16x16 block
   is clean only 8% of the time) and kept anchors. This is not a matching problem; it is that
   root blocks are large (the "larger root blocks" item is the other direction: here smaller
   parts of a root should be allowed to copy while the rest stays raw).

3. **Does it beat what we already ship?** The shipped result is the cm coder, not fractal mode.
   Whole-pipeline sizes (`fpress bench`, bytes), and a best-case estimate for approximate
   matching = the exact version's fractal size + the entropy of the noise itself (free positions):

| 512x512 input | cm (shipped) | fractal now | exact version, fractal | noise floor | ideal approximate |
|---|---|---|---|---|---|
| tile map, no noise | 2,291 | 3,540 | 3,540 | 0 | - |
| tile map + 0.1% | 3,053 | 6,775 | 3,540 | 636 | ~4,180 (worse than cm) |
| tile map + 1% | 9,222 | 17,305 | 3,540 | 5,270 | ~8,810 (5% better) |
| Sierpinski, no noise | 76 | 231 | 231 | 0 | - |
| Sierpinski + 0.1% | 881 | 1,289 | 231 | 636 | ~870 (tie) |
| Sierpinski + 1% | 6,617 | 7,157 | 231 | 5,270 | ~5,500 (17% better) |

   Even assuming a perfect patch coder, approximate matching ties or loses to cm at 0.1% noise
   and gains 5-17% at 1%, on files far smaller than where fractal mode wins at all (exact
   Sierpinski: cm beats fractal at 512x512; fractal only wins at hundreds of MiB). Real patches
   go through flate/cm, not at the noise entropy, so the true gain is smaller.

Decision: **do not build approximate matching.** Terrain-like data has no near-copies, and noisy
exact data is already handled about as well by cm. Remaining candidate: larger/flexible root
blocks (point 2), to be measured next.
