# Changelog

## 0.1.1

Container format unchanged (version 6): files from 0.1.0 open in 0.1.1 and the other way round.

- Compressing or decompressing a file onto itself (or onto a link to it) is refused; before, the
  original was replaced by the result.
- Output to a device or pipe such as `/dev/null` works (it failed with "permission denied").
- `fpress help` prints to standard output and exits 0. `compress -h`, `decompress -h` and
  `bench -h` show a usage line, the common flags first, plain descriptions and every default.
- Command-line mistakes exit with status 2 (they exited with 1), and a flag given after the file
  names gets its own message.
- README: install from a release, things to know, reading `bench` output, troubleshooting.

## 0.1.0 (first release)

Container format version 6. Files written by earlier development builds do not open; from here on,
0.x releases may still change the format (the changelog will say when).

- Lossless compressor for any file: stored, DEFLATE, reshape + DEFLATE, context mixing,
  fractal block copies, per-slice selection, and streaming for files of any size.
- Context-mixing coder with a match model that follows repeats at any distance (including a
  64-byte-context lookup that finds "the same row, one frame ago" in video).
- `-video WIDTHxHEIGHT[:format]` for raw video: frame differencing, frame-aware slicing.
- Presets `default`, `fast`, `fastest`; `-memory` and `-workers`; `decompress -memory`.
- Checks its own output before writing; checksum in every container; damaged input gives an error.
- One static binary, standard library only (`./build.sh` builds it and verifies that).
- `bench/run.sh` and `bench/video.sh` reproduce every number in the README.

Known limits: video results are from generated clips only; the default preset is slow
(seconds per MB); only tested on Linux (it cross-compiles for macOS and Windows).
