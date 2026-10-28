#!/bin/sh
# Emulated ARM runtime smoke of the production command. Run inside the admitted
# container, from the repository root, through the runner, with the web shell built
# first in the same child (the server refuses to start without it):
#   scripts/dev-run.sh -- sh -euc 'bun --cwd=web run build; exec sh scripts/smoke-targets.sh'
#
# For linux/arm64 and linux/arm/v7 (GOARM=7), with CGO_ENABLED=0:
#   1. builds the real ./cmd/boxset binary and starts it under the container's
#      user-mode QEMU interpreter (qemu-aarch64, qemu-arm) on a fresh data
#      directory and a loopback port, waits until GET /ping answers 200 `ok` with
#      X-Boxset-State: ready, then stops it with SIGTERM and requires exit 0;
#   2. cross-compiles the test binary of ./internal/install and runs
#      TestJPEGRoundTrip (the standard library image/jpeg round trip, the artwork
#      fallback where no wazero compiler exists) under the same interpreter.
# Each binary must be an ELF for exactly its architecture. A missing
# interpreter, a build, start, ping or test failure, a SKIP, or a wrong binary
# fails the run. Emulated timing means nothing: no ARM performance claim is made.
#
# Prints, per target, SMOKE-TARGET-PASS target=<target> mode=qemu ping=ok jpeg=ok.
set -u
LC_ALL=C
export LC_ALL

fail() {
	printf 'smoke-targets: %s\n' "$1" >&2
	exit 1
}

[ $# -eq 0 ] || fail usage
{ [ -f go.mod ] && [ -d cmd/boxset ] && [ -d internal/install ]; } || fail not_repo_root
for _t in go od grep mktemp tr rm sleep kill timeout bash; do
	command -v "$_t" >/dev/null 2>&1 || fail "tool_missing $_t"
done

T=$(mktemp -d "${TMPDIR:-/tmp}/smoke-targets.XXXXXX") || fail temp_dir_failed
{ [ -d "$T" ] && [ ! -L "$T" ] && [ -O "$T" ]; } || fail temp_dir_failed
PID=
cleanup() {
	if [ -n "$PID" ]; then kill -KILL "$PID" 2>/dev/null; fi
	if [ -d "$T" ] && [ ! -L "$T" ] && [ -O "$T" ]; then rm -rf "$T"; fi
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

# GOARCH, QEMU interpreter suffix, ELF e_machine, target label, loopback port.
TARGETS='arm64 aarch64 183 linux/arm64 18484
arm arm 40 linux/arm/v7 18485'

# is_elf BINARY MACHINE: an ELF whose e_machine is MACHINE.
is_elf() {
	[ "$(od -An -tx1 -N4 "$1" | tr -d ' \n')" = 7f454c46 ] || return 1
	set -- "$1" "$2" $(od -An -tu1 -j18 -N2 "$1")
	[ "${3:-}" = "$2" ] && [ "${4:-}" = 0 ]
}

# alive PID: the process exists and is not a zombie (kill -0 succeeds on one that was not waited for).
alive() {
	[ -r "/proc/$1/status" ] && ! grep -q '^State:[[:space:]]*Z' "/proc/$1/status"
}

# ping PORT: the raw HTTP response of GET /ping on the loopback, empty on any failure.
ping() {
	timeout 5 bash -c 'exec 3<>/dev/tcp/127.0.0.1/"$1" && printf "GET /ping HTTP/1.0\r\nHost: localhost\r\n\r\n" >&3 && cat <&3' ping "$1" 2>/dev/null | tr -d '\r'
}

passed=0
while read -r goarch qemu machine label port; do
	command -v "qemu-$qemu" >/dev/null 2>&1 || fail "interpreter_missing qemu-$qemu"

	bin=$T/boxset.$goarch
	GOOS=linux GOARCH=$goarch GOARM=7 CGO_ENABLED=0 go build -trimpath -o "$bin" ./cmd/boxset </dev/null || fail "build_failed $label"
	is_elf "$bin" "$machine" || fail "wrong_binary $label boxset"
	tb=$T/install.$goarch.test
	GOOS=linux GOARCH=$goarch GOARM=7 CGO_ENABLED=0 go test -c -o "$tb" ./internal/install </dev/null || fail "test_build_failed $label"
	is_elf "$tb" "$machine" || fail "wrong_binary $label install.test"

	data=$T/data.$goarch
	mkdir "$data" || fail "data_dir_failed $label"
	BOXSET_DATA_DIR=$data BOXSET_PORT=$port "qemu-$qemu" "$bin" serve >"$T/serve.$goarch" 2>&1 </dev/null &
	PID=$!
	ready=0
	i=0
	while [ "$i" -lt 120 ]; do
		alive "$PID" || fail "serve_exited $label: $(tr -cd '[:print:]\n' <"$T/serve.$goarch" | tr '\n' ' ')"
		resp=$(ping "$port")
		if printf '%s\n' "$resp" | grep -Eq '^HTTP/1\.[01] 200 ' &&
			printf '%s\n' "$resp" | grep -Eq '^X-Boxset-State: ready$' &&
			printf '%s\n' "$resp" | grep -Eq '^ok$'; then
			ready=1
			break
		fi
		i=$((i + 1))
		sleep 1
	done
	[ "$ready" = 1 ] || fail "not_ready $label"
	kill -TERM "$PID" || fail "signal_failed $label"
	# A server that does not stop within 60 s is killed, which fails the status check below.
	(sleep 60; kill -KILL "$PID" 2>/dev/null) &
	WATCHDOG=$!
	wait "$PID"
	status=$?
	kill "$WATCHDOG" 2>/dev/null
	wait "$WATCHDOG" 2>/dev/null
	PID=
	[ "$status" = 0 ] || fail "serve_exit $label status=$status"

	out=$T/jpeg.$goarch
	(cd internal/install && "qemu-$qemu" "$tb" -test.run '^TestJPEGRoundTrip$' -test.v -test.count=1 -test.timeout=300s) >"$out" 2>&1 </dev/null || fail "jpeg_run_failed $label"
	if grep -Eq '^[[:space:]]*(--- (FAIL|SKIP)|FAIL|SKIP)|no tests to run' "$out"; then fail "jpeg_failed_or_skipped $label"; fi
	grep -q '^--- PASS: TestJPEGRoundTrip ' "$out" || fail "jpeg_not_passed $label"

	printf 'SMOKE-TARGET-PASS target=%s mode=qemu ping=ok jpeg=ok\n' "$label"
	passed=$((passed + 1))
done <<EOF
$TARGETS
EOF
[ "$passed" = 2 ] || fail "target_count $passed"
