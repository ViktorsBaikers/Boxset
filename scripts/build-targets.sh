#!/bin/sh
# Release portability of the production command. Run inside the admitted
# container, from the repository root, through the runner:
#   scripts/dev-run.sh -- sh scripts/build-targets.sh
#
# Builds ./cmd/boxset with CGO_ENABLED=0 for the eight release targets
# (linux/amd64, linux/arm64, linux/arm with GOARM=7, windows/amd64,
# windows/arm64, darwin/amd64, freebsd/amd64, freebsd/arm64). Each binary must
# exist, start with the magic bytes of its platform's executable format and
# record CGO_ENABLED=0 in its own build information (`go version -m`).
#
# This is a compile proof. It runs nothing: runtime on linux/arm64 and
# linux/arm/v7 is scripts/smoke-targets.sh, and the other five targets have no
# runtime proof here (docs/portability.md).
#
# Prints BUILD-TARGETS-PASS targets=8 cgo=0 only when all eight pass.
set -u
LC_ALL=C
export LC_ALL

fail() {
	printf 'build-targets: %s\n' "$1" >&2
	exit 1
}

[ $# -eq 0 ] || fail usage
{ [ -f go.mod ] && [ -d cmd/boxset ]; } || fail not_repo_root
for _t in go od grep mktemp tr rm; do
	command -v "$_t" >/dev/null 2>&1 || fail "tool_missing $_t"
done

T=$(mktemp -d "${TMPDIR:-/tmp}/build-targets.XXXXXX") || fail temp_dir_failed
{ [ -d "$T" ] && [ ! -L "$T" ] && [ -O "$T" ]; } || fail temp_dir_failed
trap 'if [ -d "$T" ] && [ ! -L "$T" ] && [ -O "$T" ]; then rm -rf "$T"; fi' EXIT
trap 'exit 1' HUP INT TERM

# GOOS, GOARCH, leading bytes of the executable format (ELF, PE "MZ", 64-bit little-endian Mach-O).
TARGETS='linux amd64 7f454c46
linux arm64 7f454c46
linux arm 7f454c46
windows amd64 4d5a
windows arm64 4d5a
darwin amd64 cffaedfe
freebsd amd64 7f454c46
freebsd arm64 7f454c46'

built=0
while read -r goos goarch magic; do
	bin=$T/boxset.$goos.$goarch
	GOOS=$goos GOARCH=$goarch GOARM=7 CGO_ENABLED=0 go build -trimpath -o "$bin" ./cmd/boxset </dev/null || fail "build_failed $goos/$goarch"
	[ -s "$bin" ] || fail "no_binary $goos/$goarch"
	got=$(od -An -tx1 -N$((${#magic} / 2)) "$bin" | tr -d ' \n')
	[ "$got" = "$magic" ] || fail "wrong_format $goos/$goarch got=$got want=$magic"
	go version -m "$bin" </dev/null | grep -q 'CGO_ENABLED=0' || fail "cgo_enabled $goos/$goarch"
	printf 'build-targets: %s/%s ok\n' "$goos" "$goarch"
	built=$((built + 1))
done <<EOF
$TARGETS
EOF
[ "$built" = 8 ] || fail "target_count $built"
printf 'BUILD-TARGETS-PASS targets=8 cgo=0\n'
