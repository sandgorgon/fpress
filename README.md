# fpress

fpress is a lossless file compressor written in Go. You give it any file, it gives you a smaller
file, and `decompress` gives back exactly the bytes you started with. It ships as **one
self-contained binary**: only the Go standard library, no cgo, no shared libraries, nothing to install
next to it.

What makes it different is one extra trick. Besides the usual tools (a general-purpose coder and
some data reshaping), fpress can notice when a file is made of *copies of itself*, at any distance,
and describe the file as a short list of "this block is a copy of that block" instructions. On
data like that (tile maps, sprite sheets, fractal-like images, repeating records) the result can be
hundreds or thousands of times smaller than other compressors manage. On ordinary data it
gets out of the way, and the result is competitive with `xz` and `zstd -19`, usually a little smaller.

It is built as a real tool, not a demo: it checks its own work before it writes anything, handles
files of any size, and rejects damaged input instead of returning garbage.

## Quick start

```sh
./build.sh                                   # writes ./fpress (needs Go 1.24 or newer)

./fpress compress   photo.raw photo.fpr      # make it smaller
./fpress decompress photo.fpr photo.raw      # get the identical file back

./fpress compress -preset fast big.log big.fpr   # much quicker, slightly larger
./fpress bench some-file                         # try every method and show the sizes
```

Use `-` for standard input or output (`cat data | fpress compress - data.fpr`).

| flag | what it does |
|---|---|
| `-preset default\|fast\|fastest` | speed against size, see below |
| `-memory 4G` | memory budget the compressor plans around (default 8G) |
| `-workers N` | how many CPU cores to use (default: all) |
| `-video 1280x720:rgb24` | the file is raw video; see "Raw video" below |
| `decompress -memory 2G` | on decompress: stay within this budget, or refuse with a clear error |

`fpress compress -h` lists the tuning flags for people who want to experiment.

## What it does, in plain terms

fpress treats a file as a flat sheet of bytes, rows of numbers laid side by side, whether the file
is a picture, a program or a text document. It tries several ways of shrinking that sheet and
keeps whichever one produced the smallest result:

1. **Store it** as is. Used when nothing helps (already compressed or random data). The cost of
   trying and failing is 13 bytes.
2. **Deflate** (the algorithm behind zip and gzip). A quick baseline.
3. **Reshape, then compress.** It searches for the row width that lines up repeated structure
   (records of 48 bytes, an image 640 pixels wide) and applies reversible steps such as "store each
   byte as the difference from the one before it". A gradient that looks like noise to other tools becomes a
   few hundred bytes.
4. **Context mixing.** A slower, stronger coder that predicts every byte from the bytes around it
   (left, above, earlier in the row, and a match model that follows repeats at any distance) using
   several small models that vote. This is the workhorse for
   text, programs and audio-like data, and it wins most files.
5. **The fractal stage.** The file is cut into blocks, and each block is matched against every
   other part of the file. If a block is an exact copy of another one (possibly rotated, flipped,
   or shifted in value), it is replaced by a short recipe: "copy from there, turned this way". A
   file made of repeated patterns collapses to a list of recipes. Whatever the recipes get wrong is
   stored as a correction, so the result is always exact, whether the recipes were good or not.
6. **Slice and pick.** On mixed files each 64 KiB slice picks the best of the above, so a file that is
   half text and half image gets the right tool for each half.

Safety checks you can rely on:

- The compressor decodes its own output and compares it with the input before it writes a file.
  If anything differs it reports an error rather than writing a bad file.
- Every container carries a checksum. Truncated or damaged files give an error on decompress and
  never crash or return wrong data (all the parsers are fuzz-tested).
- Decompression is deterministic: integer arithmetic only, no floating point, so a file
  written on one machine decodes identically on another (tested between 64-bit and 32-bit builds).

## How well does it work?

Measured on a 12-core machine, one run each. The files come from `bench/corpus` and are generated
with fixed seeds, so you can reproduce every row (see "Reproducing" below). Sizes are in bytes.

### Generated files (each 1 MiB unless noted)

| file | what it is | original | fpress | xz -9e | zstd -19 | bzip2 -9 |
|---|---|---:|---:|---:|---:|---:|
| sierpinski | exactly self-similar image | 1,048,576 | **359** | 2,208 | 2,922 | 5,614 |
| gradient | smooth ramp | 1,048,576 | **215** | 792 | 981 | 2,746 |
| tilemap16 | grid of 16x16 tiles from a palette of 6 | 1,048,576 | **3,830** | 34,736 | 49,198 | 35,883 |
| tilemap8-noisy | same idea, 8x8 tiles, 1% of bytes corrupted | 1,048,576 | **34,203** | 88,216 | 109,540 | 97,028 |
| records | 16,384 fixed-size 64-byte records | 1,048,576 | **53,348** | 77,796 | 109,639 | 96,977 |
| terrain | rough fractal landscape | 1,048,576 | **84,163** | 106,144 | 119,670 | 123,936 |
| audio | 16-bit samples, two tones plus noise | 1,048,576 | **662,339** | 978,840 | 1,009,853 | 832,854 |
| text | English-like words, 80-column lines | 1,048,576 | **209,137** | 280,252 | 280,037 | 249,694 |
| mixed | five of the above, joined | 360,448 | **85,313** | 92,464 | 93,533 | 96,881 |
| random | incompressible | 1,048,576 | 1,048,589 | 1,048,696 | **1,048,610** | 1,053,322 |
| zeros | all zero bytes | 1,048,576 | 205 | 292 | 50 | **45** |

That is the default preset. Two honest notes: on a block of zeros `zstd` and `bzip2` are better
(a few hundred bytes either way), and the generated files are shaped to show what each method does. They
say little about your data. The next table is closer to everyday files.

### Real files

| file | original | fpress default | fpress fast | xz -9e | zstd -19 |
|---|---:|---:|---:|---:|---:|
| Go source code (1.7 MB of `net/http`) | 1,778,519 | **306,858** | 351,788 | 352,404 | 358,078 |
| compiled program (Go binary, 3.4 MB) | 3,567,314 | 1,942,042 | 1,952,271 | **1,914,776** | 1,982,783 |
| a `.gz` file (already compressed) | 470,125 | 470,138 | 470,138 | 470,216 | **469,906** |

On source text fpress is about 13% smaller than `xz`. On a compiled program it is within about 1.5% of
`xz`, slightly behind it. Already compressed data cannot be shrunk by anyone; fpress adds 13 bytes.

### Very large files

The fractal stage works at any size. It reads the input in pieces, so memory stays flat as the file grows
(set by `-memory`, not by file size). Two self-similar images, compressed with
`-memory 4G -no-cm -no-prep` and restored byte for byte:

| input | compressed | ratio | compress time | peak memory | decompress time |
|---|---:|---:|---:|---:|---:|
| 1 GiB image | 21,829 B | 49,188 : 1 | 1 min 50 s | 555 MB | 21 s |
| 4 GiB image | 81,407 B | 52,761 : 1 | 5 min 38 s | 628 MB | 73 s |

(These two rows were measured earlier in the project, before some later speed-ups, and not re-run
for this table.)

### Raw video

Raw video has no header, so fpress cannot tell where one frame ends. Tell it with `-video`, and it
compares each frame with the one before, storing only what changed:

```sh
./fpress compress -video 1280x720:rgb24 clip.rgb clip.fpr
```

Formats: `gray8`, `gray16`, `rgb24`, `bgr24`, `rgba`, `bgra` and `yuv420` (planar). The default is `rgb24`.
Tested on five generated clips (RGB, 320x180, 60 frames, 10.4 MB each; `bench/video.sh` reproduces them),
default preset, in bytes:

| clip | no hint | `-video` | xz -9e | zstd -19 |
|---|---:|---:|---:|---:|
| screen recording (static desktop, moving cursor) | 7,672 | **6,189** | 6,936 | 8,072 |
| flat-colour animation | 15,087 | **3,748** | 6,228 | 19,241 |
| scrolling page of text | 10,610 | **8,276** | 9,188 | 11,517 |
| panning scene | 37,284 | **19,456** | 22,168 | 35,369 |
| camera footage with sensor noise | 4,867,712 | 4,804,961 | 6,447,220 | 6,779,961 |

Two things make this work. The compressor's matcher looks for repeats of the last 64 bytes at any
distance, so it finds "the same row, one frame ago" even though that is 170,000 bytes back. (Before
that was added, fpress was 4 to 20 times *worse* than `xz` on these clips.) And `-video` adds the
frame difference, which turns a static scene into almost nothing and lets a moving cursor or sprite stand out. Even
without the hint the first helps a lot; the hint buys another 20-50% on clean video.

Be realistic about this. On noisy camera footage the hint changes little: differencing two noisy frames
makes the noise worse, so the compressor keeps whichever is smaller (still about 25% smaller than `xz`).
There is no motion tracking. Content that moves as an exact copy (scrolling, a pan) works; sub-pixel motion or lighting
changes do not. Compressing 10 MB takes about 20-30 s (decompress 7-12 s). `-preset fast` is about 3 times
quicker but cuts the file into independent pieces, so it is much larger on video (28 KB on the screen
recording). A dedicated lossless video codec such as FFV1 will usually do better on camera footage. The clips
are synthetic; I have not tested real video.

### Speed and memory

The default preset trades time for size. Times for the 1 MiB files above, compress / decompress:

| preset | typical time | peak memory | what it gives up |
|---|---|---|---|
| `default` | 2-9 s / 0.1-1.2 s | 200 MB - 1 GB | nothing: tries everything |
| `fast` | about 1 s / 0.2-0.4 s | about 180 MB | no fractal stage; 0-5% larger on ordinary files |
| `fastest` | about 0.1 s / near zero | about 15 MB | no context mixing and no fractal stage: deflate-class sizes (the text file is 326,648 B, 17% bigger than `xz`) |

The fractal stage is the cost of `default`. `fast` loses it, which is why the same Sierpinski
image is 359 bytes with `default` and 6,771 with `fast`, and the tile map is 3,830 against 35,501.
If your data is not self-similar, `fast` costs you almost nothing. Before spending time on
the fractal stage the compressor takes a quick sample of the file and skips the stage when the file shows
no repeated blocks, which cut the CPU time on ordinary files by more than half in our measurements.

Decompression with the context-mixing coder is several times quicker than compression, but still slow
by `zstd` standards (about a second per MiB in the table above). Plan for that if you decompress large files often.

## When to use it, when not to

Good fit:
- data with repetition at a distance: tile maps, sprite sheets, game levels, CAD-like grids, scanned
  forms, repeating records, synthetic images;
- text, source code and logs, where it beat `xz` by 13-25% in the tests above, in exchange for time;
- a mix of data types in one file;
- screen recordings and animation stored as raw frames (use `-video`).

Poor fit:
- speed-critical paths (use `-preset fast` or `zstd`);
- camera video (see "Raw video"): works, but it is a plain-pixel coder with no motion search;
- photographs and natural images: nearby pixels are similar but not identical, and exact matching finds
  little (the other methods still apply, but I have not measured photographs);
- noisy near-repeats: if every copy differs by a few corrupted bytes, the fractal stage finds
  nothing and the context-mixing coder does the work (see `tilemap8-noisy`; the measurements are in `PLAN.md`);
- data that is already compressed or encrypted.

## Building

```sh
./build.sh                        # this machine
GOOS=windows GOARCH=amd64 ./build.sh   # or any target Go supports
```

`build.sh` builds with `CGO_ENABLED=0`, strips the binary (about 2.3 MB), and then verifies that no
package outside the standard library is used and that the binary has no shared libraries. A test
(`TestBuildsWithStandardLibraryOnly`) fails the build if a dependency ever creeps in. There is no `go.sum`
because there is nothing to download. Tested to cross-compile for Linux (amd64, arm64, 386),
macOS (amd64, arm64) and Windows (amd64).

The only way the program touches anything outside itself is the optional `bench -ext` flag, which runs `xz`,
`zstd` and `bzip2` if they happen to be installed, to print comparison columns.

## Reproducing the numbers

```sh
bench/run.sh              # builds, generates the corpus, runs default and fast, prints a markdown table
bench/run.sh -quick       # fast preset only (about 20 seconds)
bench/run.sh my.dat ...   # also benchmarks your own files
go run ./bench/corpus -out DIR   # just write the generated files
bench/video.sh            # the raw-video table above (about 3 minutes)
```

Every file is compressed, decompressed and compared byte for byte; the script exits with an error if
any differ. Timings depend on your machine, but the sizes of the generated files are identical on every run.

## Testing

```sh
go vet ./... && go test ./...        # about a minute
GOARCH=386 go test ./fractal ./codec ./cmd/...   # 32-bit build
```

The suite covers round trips on all kinds of input and shapes, the file-format parsers against
malformed input, identical output regardless of core count, and fuzzers on every decoder. It
has also been run under Go's race detector.

## How it is organised

| package | role |
|---|---|
| `matrix` | the 2D byte-sheet that everything works on |
| `transform` | reversible block operations (rotations, flips, value shifts) |
| `prep` | reversible reshaping steps and row-width detection |
| `fractal` | block matching, planning, the copy recipes and their decoder |
| `entropy` | the context-mixing coder (integer arithmetic only) |
| `codec` | the container format, method selection, slicing, very-large-file streaming |
| `cmd/fpress` | the command-line tool |
| `bench` | the benchmark runner and the generated corpus |

`PLAN.md` is the full engineering record: the design, every measurement (including the ideas that
were measured and dropped), the problems found along the way, and what is still open.

## License

MIT; see `LICENSE`.

## Status

This is version 0.1.0 (`fpress version` prints it). It works and is tested, but it is early:
**files written by one 0.x version may not open in the next.** The container format is at version 6
and changed once already, when the match model improved. Until 1.0, keep the original or decompress
with the same version you compressed with. Ideas still open, none of them measured to be a
big win so far: matching across the boundaries of very large files, denser search for copies that
are not aligned to the block grid, and calibrating the planner's cost estimates.
