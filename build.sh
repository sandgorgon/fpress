#!/bin/sh
# Build the single fpress binary: standard library only, no cgo, statically linked.
#
#   ./build.sh                      writes ./fpress for this machine
#   GOOS=windows GOARCH=amd64 ./build.sh   cross-compiles (writes ./fpress.exe)
#
# It then checks the promises: no package outside the Go standard library and
# this module, and (when it can be checked) a binary with no shared libraries.
set -eu
cd "$(dirname "$0")"

out=fpress
[ "${GOOS:-$(go env GOOS)}" = windows ] && out=fpress.exe

CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$out" ./cmd/fpress

# Every dependency must be the standard library or one of our own packages.
extra=$(CGO_ENABLED=0 go list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./cmd/fpress | grep -v '^fpress' || true)
if [ -n "$extra" ]; then
	echo "build.sh: third-party packages found:" >&2
	echo "$extra" >&2
	exit 1
fi

if [ -z "${GOOS:-}" ] && command -v ldd >/dev/null 2>&1; then
	if ldd "$out" >/dev/null 2>&1; then
		echo "build.sh: $out is dynamically linked" >&2
		exit 1
	fi
fi

echo "built $out ($(wc -c <"$out" | tr -d ' ') bytes): standard library only, no cgo, no shared libraries"
