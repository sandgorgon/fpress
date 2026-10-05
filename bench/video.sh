#!/bin/bash
# Benchmark fpress on synthetic raw video (RGB24, 320x180, 60 frames, 10.4 MB each):
# a screen recording, flat-colour animation, noisy camera footage, a scrolling page and a panning scene.
#
#   bench/video.sh            run all three (about 5 minutes)
#   bench/video.sh -quick     only the fast preset
#
# Columns: fpress without any video hint, fpress with -video, and xz / zstd for reference.
set -u
cd "$(dirname "$0")/.."
quick=0; [ "${1:-}" = "-quick" ] && quick=1

work=$(mktemp -d); trap 'rm -rf "$work"' EXIT
./build.sh >/dev/null || exit 1
bin=$PWD/fpress
go run ./bench/corpus -video -out "$work" >/dev/null || exit 1

ext() { command -v "$1" >/dev/null 2>&1 || { echo -; return; }; "$@" <"$f" 2>/dev/null | wc -c | tr -d ' '; }
run() { # run PRESET [flags...]: prints "size seconds ok"
	local p=$1; shift
	local t0=$(date +%s.%N)
	"$bin" compress -preset "$p" "$@" "$f" "$work/o.fpr" 2>/dev/null
	local t1=$(date +%s.%N)
	"$bin" decompress "$work/o.fpr" "$work/b" 2>/dev/null
	local ok=ok; cmp -s "$f" "$work/b" || { ok=FAIL; fail=1; }
	echo "$(stat -c %s "$work/o.fpr") $(awk -v a="$t0" -v b="$t1" 'BEGIN{printf "%.0f", b-a}') $ok"
}

presets=(default fast); [ $quick = 1 ] && presets=(fast)
fail=0
echo "| clip | preset | no video flag | with -video 320x180 | compress s | xz -9e | zstd -19 --long | check |"
echo "|---|---|---:|---:|---:|---:|---:|---|"
for f in "$work"/*.rgb; do
	xz_=$(ext xz -9e -c); zs_=$(ext zstd -19 --long=27 -c -q)
	for p in "${presets[@]}"; do
		read a _ ok1 <<<"$(run "$p")"
		read b secs ok2 <<<"$(run "$p" -video 320x180)"
		chk=ok; [ "$ok1" = ok ] && [ "$ok2" = ok ] || chk=FAIL
		echo "| $(basename "$f" .rgb) | $p | $a | $b | $secs | $xz_ | $zs_ | $chk |"
	done
done
exit $fail
