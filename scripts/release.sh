#!/bin/bash
# Cut a release: check everything, build binaries for every platform, tag, push, publish.
#
#   scripts/release.sh --dry-run    do every check and build into dist/, change nothing else
#   scripts/release.sh              the same, then tag vX.Y.Z, push, and create the GitHub release
#
# The version is the one in cmd/fpress/main.go. See RELEASING.md for the whole procedure.
set -euo pipefail
cd "$(dirname "$0")/.."

dry=0; [ "${1:-}" = "--dry-run" ] && dry=1
# pushes authenticate through gh, so no git credential setup is needed
die() { echo "release: $*" >&2; exit 1; }

ver=$(sed -n 's/^const version = "\(.*\)"$/\1/p' cmd/fpress/main.go)
[ -n "$ver" ] || die "cannot read the version from cmd/fpress/main.go"
tag=v$ver
echo "== releasing $tag"

# --- the tree must be exactly what is on main ---------------------------------
[ "$(git rev-parse --abbrev-ref HEAD)" = main ] || die "not on main"
[ -z "$(git status --porcelain)" ] || die "working tree is not clean"
if [ $dry = 0 ]; then
	git remote get-url origin >/dev/null 2>&1 || die "no 'origin' remote"
	git fetch -q origin
	[ "$(git rev-parse HEAD)" = "$(git rev-parse origin/main 2>/dev/null || echo none)" ] || die "main is not pushed (or origin/main differs): push first"
	git rev-parse -q --verify "refs/tags/$tag" >/dev/null && die "tag $tag already exists"
	gh release view "$tag" >/dev/null 2>&1 && die "GitHub release $tag already exists"
fi
grep -q "^## $ver" CHANGELOG.md || die "CHANGELOG.md has no '## $ver' section"
fmtver=$(sed -n 's/^\tversion = \([0-9]*\)$/\1/p' codec/codec.go)
grep -q "container format is at version $fmtver" README.md || die "README.md does not state container format version $fmtver"

# --- the code must pass ------------------------------------------------------
echo "== vet + tests"
go vet ./...
go test ./...
GOARCH=386 go test ./prep ./entropy ./codec ./cmd/... >/dev/null

# --- build every platform ----------------------------------------------------
rm -rf dist; mkdir dist
targets="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64"
for t in $targets; do
	os=${t%/*}; arch=${t#*/}
	name=fpress_${ver}_${os}_${arch}
	exe=fpress; [ $os = windows ] && exe=fpress.exe
	echo "== build $name"
	mkdir -p "dist/$name"
	CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags='-s -w' -o "dist/$name/$exe" ./cmd/fpress
	cp LICENSE README.md CHANGELOG.md "dist/$name/"
	if [ $os = windows ]; then
		(cd dist && python3 -c "import shutil,sys; shutil.make_archive('$name','zip','.','$name')")
	else
		tar -C dist -czf "dist/$name.tar.gz" "$name"
	fi
	rm -rf "dist/$name"
done
(cd dist && sha256sum * > SHA256SUMS)

# --- the native build must run, round-trip a file, and report the right version ---
echo "== smoke test"
here=$(go env GOOS)_$(go env GOARCH)
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
tar -C "$tmp" -xzf "dist/fpress_${ver}_${here}.tar.gz"
bin=$tmp/fpress_${ver}_${here}/fpress
"$bin" version | grep -q "fpress $ver " || die "built binary reports the wrong version"
head -c 300000 /dev/urandom | od -An -tx1 > "$tmp/in.txt"
"$bin" compress "$tmp/in.txt" "$tmp/in.fpr"
"$bin" decompress "$tmp/in.fpr" "$tmp/out.txt"
cmp "$tmp/in.txt" "$tmp/out.txt" || die "smoke test round trip failed"
command -v ldd >/dev/null && [ "$(go env GOOS)" = linux ] && { ldd "$bin" >/dev/null 2>&1 && die "binary is dynamically linked"; }

# --- release notes: the CHANGELOG section for this version ------------------
awk -v h="## $ver" 'index($0, h) == 1 {on=1; next} /^## / {on=0} on' CHANGELOG.md > dist/NOTES.md
[ -s dist/NOTES.md ] || die "release notes came out empty"

if [ $dry = 1 ]; then
	echo "== dry run done; artifacts in dist/:"; ls -l dist
	exit 0
fi

# --- publish -----------------------------------------------------------------
echo "== tag, push, publish"
git tag -a "$tag" -m "fpress $ver"
git -c credential.helper= -c credential.helper='!gh auth git-credential' push origin "$tag"
gh release create "$tag" dist/*.tar.gz dist/*.zip dist/SHA256SUMS \
	--title "fpress $ver" --notes-file dist/NOTES.md
echo "== released $tag"
gh release view "$tag" --json url -q .url
