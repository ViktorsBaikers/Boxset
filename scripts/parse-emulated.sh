#!/bin/sh
# ARM correctness of the parser corpora. Run inside the admitted container, from
# the repository root, through the runner:
#   scripts/dev-run.sh -- scripts/parse-emulated.sh [SELECTOR]
#
# Cross-compiles the real test binaries of internal/parse and
# internal/parse/corpus for linux/arm64 and linux/arm (v7) with CGO_ENABLED=0,
# checks each binary is an ELF for exactly that architecture and runs it under
# the container's user-mode QEMU interpreter (qemu-aarch64, qemu-arm) from its
# package directory (so the committed testdata/parser corpora are read).
#
# SELECTOR is a Go test regexp (as -test.run). On each target it is matched
# against the ACTUAL listing of each ARM binary (-test.list); the run executes
# the union of those matches and the mandatory tests (TestQualityCorpus and
# TestQualityBounds in internal/parse, TestCorpusFull in internal/parse/corpus),
# so a selector that names a test of only one package still runs the mandatory
# corpus tests of both. A selector that matches no test in any package of a
# target fails from the listings, before any test of that target runs; nothing
# is credited for a test that did not run. Without a SELECTOR only the mandatory
# tests run.
#
# Every executed test must report `--- PASS`, no FAIL or SKIP may appear, and the
# corpus summaries must show a positive per-application count. A missing
# interpreter, a wrong binary, a build, list or run failure, or a zero count
# fails the run. No ARM performance claim is made: emulated timing means nothing.
#
# Prints PARSE-EMULATED-PASS targets=2 binaries=4 only when all four runs pass.
set -u
LC_ALL=C
export LC_ALL

fail() {
	printf 'parse-emulated: %s\n' "$1" >&2
	exit 1
}

[ $# -le 1 ] || fail usage
SEL=${1:-}

{ [ -f go.mod ] && [ -d internal/parse ] && [ -d testdata/parser ]; } || fail not_repo_root
for _t in go od grep mktemp tr rm sed sort; do
	command -v "$_t" >/dev/null 2>&1 || fail "tool_missing $_t"
done

T=$(mktemp -d "${TMPDIR:-/tmp}/parse-emulated.XXXXXX") || fail temp_dir_failed
{ [ -d "$T" ] && [ ! -L "$T" ] && [ -O "$T" ]; } || fail temp_dir_failed
trap 'if [ -d "$T" ] && [ ! -L "$T" ] && [ -O "$T" ]; then rm -rf "$T"; fi' EXIT
trap 'exit 1' HUP INT TERM

# GOARCH, QEMU interpreter suffix, ELF e_machine.
TARGETS='arm64 aarch64 183
arm arm 40'
# package, mandatory tests (space separated).
PKGS='./internal/parse TestQualityCorpus TestQualityBounds
./internal/parse/corpus TestCorpusFull'

runs=0
while read -r goarch qemu machine; do
	command -v "qemu-$qemu" >/dev/null 2>&1 || fail "interpreter_missing qemu-$qemu"

	# Build every package's binary and take its actual listing for the selector.
	matched=0
	idx=0
	while read -r pkg mandatory; do
		idx=$((idx + 1))
		dir=${pkg#./}
		bin=$T/$goarch.$idx.test
		GOOS=linux GOARCH=$goarch GOARM=7 CGO_ENABLED=0 go test -c -o "$bin" "$pkg" </dev/null || fail "build_failed $goarch $pkg"
		[ "$(od -An -tx1 -N4 "$bin" | tr -d ' \n')" = 7f454c46 ] || fail "not_elf $goarch $pkg"
		set -- $(od -An -tu1 -j18 -N2 "$bin")
		{ [ "${1:-}" = "$machine" ] && [ "${2:-}" = 0 ]; } || fail "wrong_binary $goarch $pkg"

		: >"$T/$goarch.$idx.extra"
		if [ -n "$SEL" ]; then
			(cd "$dir" && "qemu-$qemu" "$bin" -test.list "$SEL") >"$T/$goarch.$idx.list" 2>/dev/null </dev/null || fail "list_failed $goarch $pkg"
			grep -E '^Test[A-Za-z0-9_]*$' "$T/$goarch.$idx.list" >"$T/$goarch.$idx.extra"
			matched=$((matched + $(grep -c . "$T/$goarch.$idx.extra")))
		fi
		for n in $mandatory; do
			printf '%s\n' "$n"
		done >"$T/$goarch.$idx.names"
		cat "$T/$goarch.$idx.extra" >>"$T/$goarch.$idx.names"
		sort -u "$T/$goarch.$idx.names" >"$T/$goarch.$idx.run"
	done <<EOF
$PKGS
EOF
	if [ -n "$SEL" ] && [ "$matched" = 0 ]; then
		fail "selector_matches_nothing $goarch"
	fi

	idx=0
	while read -r pkg mandatory; do
		idx=$((idx + 1))
		dir=${pkg#./}
		bin=$T/$goarch.$idx.test
		names=$(cat "$T/$goarch.$idx.run")
		pattern="^($(printf '%s' "$names" | tr '\n' '|' | sed -e 's/|$//'))\$"
		out=$T/out.$runs
		runs=$((runs + 1))
		(cd "$dir" && "qemu-$qemu" "$bin" -test.run "$pattern" -test.v -test.count=1 -test.timeout=500s) >"$out" 2>&1 </dev/null
		status=$?
		if [ "$status" != 0 ]; then
			sed -e 's/^/  | /' "$out" | tr -cd '[:print:]\n' | sed -n '1,40p' >&2
			fail "run_failed $goarch $pkg exit=$status"
		fi
		if grep -Eq '^[[:space:]]*(--- (FAIL|SKIP)|FAIL|SKIP)|no tests to run' "$out"; then
			fail "failed_or_skipped $goarch $pkg"
		fi
		count=0
		for n in $names; do
			grep -q "^--- PASS: $n " "$out" || fail "not_passed $goarch $pkg $n"
			count=$((count + 1))
		done
		printf 'parse-emulated: %s %s passed=%s\n' "$goarch" "$pkg" "$count"
		grep -h -E 'quality-corpus app=|corpus-full app=' "$out" | sed -e "s/^[^:]*:[0-9]*: /$goarch /"
		case $pkg in
		./internal/parse)
			for app in series movie; do
				grep -Eq "quality-corpus app=$app .*passed=[1-9][0-9]*" "$out" || fail "zero_count $goarch $app"
			done
			;;
		*)
			for app in series movie; do
				grep -Eq "corpus-full app=$app .*cases=[1-9][0-9]*" "$out" || fail "zero_count $goarch $app"
			done
			;;
		esac
	done <<EOF
$PKGS
EOF
done <<EOF
$TARGETS
EOF
[ "$runs" = 4 ] || fail "run_count $runs"
printf 'PARSE-EMULATED-PASS targets=2 binaries=%s\n' "$runs"
