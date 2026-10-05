# Changelog

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
