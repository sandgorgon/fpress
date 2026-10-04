#!/bin/bash
# Benchmark fpress against a fixed set of files and print a markdown table.
#
#   bench/run.sh                  build, generate the synthetic corpus, run default + fast presets
#   bench/run.sh -quick           only the fast preset (the default preset is slow on some files)
#   bench/run.sh FILE...          also run these files (your own data)
#   CORPUS=dir bench/run.sh       reuse/keep the generated corpus in dir
#
# For every file and preset it compresses, decompresses, and compares the result
# byte for byte; any mismatch is reported as FAIL and the script exits non-zero.
# xz -9e, zstd -19 and bzip2 -9 are added as reference columns when installed.
# Time is wall-clock seconds; memory is the compressor's peak resident size in MB
# (needs GNU time at /usr/bin/time; shown as "-" without it).
set -u
cd "$(dirname "$0")/.."

quick=0
if [ "${1:-}" = "-quick" ]; then quick=1; shift; fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
corpus=${CORPUS:-$work/corpus}

./build.sh >/dev/null || exit 1
bin=$PWD/fpress
go run ./bench/corpus -out "$corpus" >/dev/null || exit 1

files=("$corpus"/*)
files+=("$@")

presets=(default fast)
[ $quick = 1 ] && presets=(fast)

gnutime=""
[ -x /usr/bin/time ] && /usr/bin/time -f %M true 2>/dev/null && gnutime=/usr/bin/time

# size of the file after an external compressor, or "-" if it is not installed
ext() {
	command -v "$1" >/dev/null 2>&1 || { echo -; return; }
	"$@" <"$f" 2>/dev/null | wc -c | tr -d ' '
}

echo "fpress benchmark: $(nproc 2>/dev/null || echo ?) cores, $(go version | cut -d" " -f3)"
echo
echo "| file | size | preset | fpress | ratio | compress s | decompress s | peak MB | xz -9e | zstd -19 | bzip2 -9 | check |"
echo "|---|---:|---|---:|---:|---:|---:|---:|---:|---:|---:|---|"

fail=0
for f in "${files[@]}"; do
	[ -f "$f" ] || { echo "skipping $f" >&2; continue; }
	size=$(stat -c %s "$f" 2>/dev/null || stat -f %z "$f")
	xz_=$(ext xz -9e -c); zs_=$(ext zstd -19 -c -q); bz_=$(ext bzip2 -9 -c)
	for p in "${presets[@]}"; do
		t0=$(date +%s.%N)
		if [ -n "$gnutime" ]; then
			$gnutime -o "$work/mem" -f %M "$bin" compress -preset "$p" "$f" "$work/out.fpr" 2>/dev/null
		else
			"$bin" compress -preset "$p" "$f" "$work/out.fpr" 2>/dev/null
		fi
		t1=$(date +%s.%N)
		"$bin" decompress "$work/out.fpr" "$work/back" 2>/dev/null
		t2=$(date +%s.%N)
		if cmp -s "$f" "$work/back"; then ok=ok; else ok=FAIL; fail=1; fi
		out=$(stat -c %s "$work/out.fpr" 2>/dev/null || stat -f %z "$work/out.fpr")
		mem=-; [ -n "$gnutime" ] && mem=$(( $(cat "$work/mem") / 1024 ))
		awk -v n="$(basename "$f")" -v s="$size" -v p="$p" -v o="$out" -v a="$t0" -v b="$t1" -v c="$t2" \
			-v m="$mem" -v x="$xz_" -v z="$zs_" -v bz="$bz_" -v ok="$ok" 'BEGIN {
			printf "| %s | %d | %s | %d | %.4f | %.1f | %.1f | %s | %s | %s | %s | %s |\n",
				n, s, p, o, (s ? o / s : 0), b - a, c - b, m, x, z, bz, ok }'
		rm -f "$work/out.fpr" "$work/back"
	done
done
exit $fail
