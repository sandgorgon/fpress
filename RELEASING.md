# Releasing fpress

One script does the checking, building, tagging and publishing: `scripts/release.sh`. It needs Go,
`gh` (logged in with push rights), `tar`, `sha256sum` and `python3`.

## Steps

1. **Decide the number.** Patch (`0.1.1`) for fixes, minor (`0.2.0`) for features or anything that
   changes the container format. Until 1.0 the format may change in any release; say so in the changelog.
2. **Edit three things, in one commit on `main`:**
   - `const version` in `cmd/fpress/main.go`;
   - a new `## X.Y.Z` section at the top of `CHANGELOG.md` (the script publishes it as the release
     notes, so write it for users: what changed, and any format break);
   - if the container format changed: `version` in `codec/codec.go`, and the "container format is at
     version N" line in the README Status section. The script refuses to continue if they disagree.
3. **If the numbers in the README may have moved** (coder or fractal changes), rerun
   `bench/run.sh` and `bench/video.sh` and update the tables first.
4. **Push `main`** (`git -c credential.helper='!gh auth git-credential' push`, if git has no GitHub login of its own). The script refuses to release a commit that is not on `origin/main`.
5. **Dry run:** `scripts/release.sh --dry-run`. It runs `go vet` and all tests (plus the 32-bit
   build), builds every platform into `dist/`, writes `SHA256SUMS`, and smoke-tests the native binary
   (version string, a round trip, statically linked). It changes nothing else.
6. **Release:** `scripts/release.sh`. It creates the annotated tag `vX.Y.Z`, pushes it, and creates the
   GitHub release with the archives, checksums and notes.
7. **Check it:** open the release page, download one archive, verify with `sha256sum -c SHA256SUMS`,
   run `fpress version`.

## What gets published

`fpress_X.Y.Z_{linux,darwin}_{amd64,arm64}.tar.gz`, `fpress_X.Y.Z_windows_amd64.zip` (each holds the
binary, LICENSE, README and CHANGELOG) and `SHA256SUMS`. Binaries are static, stdlib-only, built
with `CGO_ENABLED=0 -trimpath`. Only the Linux amd64 build is smoke-tested by the script.

## Installing a release

```sh
gh release download vX.Y.Z --repo sandgorgon/fpress --pattern '*linux_amd64.tar.gz' --pattern SHA256SUMS
sha256sum --ignore-missing -c SHA256SUMS
tar -xzf fpress_X.Y.Z_linux_amd64.tar.gz
install -m 755 fpress_X.Y.Z_linux_amd64/fpress ~/.local/bin/fpress
```

## If something goes wrong

- Script stopped before "tag, push, publish": nothing was changed remotely; fix and rerun.
- Tag pushed but the GitHub release failed: `gh release create vX.Y.Z dist/*.tar.gz dist/*.zip dist/SHA256SUMS --title "fpress X.Y.Z" --notes-file dist/NOTES.md`.
- A bad release: `gh release delete vX.Y.Z --cleanup-tag`, fix, and release a new patch version (do
  not reuse a published number).
