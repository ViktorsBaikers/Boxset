#!/bin/sh
# Canonical Boxset dev runner (host, repository root only):
#   scripts/dev-run.sh [--write docs/spikes/issue-drafts|docs/spikes/data/bench-<UTC>/samples|internal/outbound/testdata/fixtures/<set>] [--capture DIR] [--bench CPUSET] [--bench-data PATH] [--timeout SECONDS] -- COMMAND [ARG...]
#
# Rebuilds the `work` image from the current allowlisted inputs (frozen locks,
# never a lock regeneration) and runs COMMAND with its exact argv in one fresh,
# uniquely named container: fixed user 1000:1000 whatever the host UID, no
# network, no capabilities, no daemon log (--log-driver=none, checked before the
# start) and only the literal environment listed below, read back from the
# created container before the start. Every Docker call runs with a private,
# empty client configuration on the daemon the caller selected (never another
# one), so no owner Docker proxy or credential setting reaches a build or the
# container. No host environment, .env, Docker socket, mount or port reaches the
# child. Stdout and stderr are merged through the host redactor
# (deploy/dev/redact -cap) and bounded to 256 KiB (262144 bytes; the canonical verbose benchmark-package
# test run is a little over 64 KiB). More output than that is output_overflow, exit 125, never a success.
# Exit status: the child's (Docker's checked terminal state), 124 on the
# deadline, or 125 with a fixed reason word on stderr.
#
# --write publishes the fixed names the child left under /src/<root>, one root per
# run: docs/spikes/issue-drafts (s5-admission.md, s5-tools.md; at most 64 KiB each),
# internal/outbound/testdata/fixtures/<set> (exchanges.json; at most 8 MiB; <set> is one
# lower-case [a-z0-9-] word of at most 32 characters and nothing else resolves it), or,
# with --bench, a fresh docs/spikes/data/bench-<UTC>/samples root (samples.jsonl; at most
# 32 MiB). Only after the container stopped, each name is exported as one tar
# stream that must hold exactly one regular file within its cap, streamed straight
# through the redactor (never spooled raw) and capped, and published by a private
# part plus `mv` only after the owned container was removed. A draft is published
# even when the child exits nonzero; a fixture set and benchmark samples are published
# only when the child exited 0 and every name was exported (a missing, oversize, linked
# or non-regular name publishes nothing). The archive's member names are checked in a
# pipe and only fixed result tokens are kept; copy diagnostics pass through the capped
# redactor before they are kept. A later rename failure may leave earlier names
# published. There is no rollback and no durability claim.
#
# --bench CPUSET (CPUSET = the owner's reserved cores, digits, commas and ranges) runs the child as a
# reference-profile benchmark: --cpuset-cpus CPUSET --memory 4g --memory-swap 4g and the host directory
# BOXSET_BENCH_CONFIG (an absolute path, read from the environment at run time and never written
# anywhere) mounted read-only at /config. It is valid only with --write of a fresh
# docs/spikes/data/bench-<UTC YYYYMMDDTHHMMSSZ>/samples root and never with --capture; every other
# form is rejected before Docker with `dev-run: bench_invalid side=<cpuset|write|samples|config|cgroup|daemon>`
# or `dev-run: bench_unavailable side=config` (variable unset or empty), exit 125; the daemon must be
# a unix socket of this machine that runs this machine's kernel (the host /proc and /sys are what is
# observed), and no endpoint is ever printed. The container itself is proven to be this machine's: the pid
# the daemon reports must be a task of this /proc whose cgroup names the container, and its /config must be
# the device and inode every observation reads (a renamed and recreated path fails). A /proc mounted with
# hidepid, a second /proc mount or any mount over a task entry (bench_invalid side=proc), whoever the caller, is rejected before Docker. The host, never the
# child, observes the run: RUN/before.json (after the container exists, before it starts),
# RUN/during.json (every observation while it runs, once a second) and RUN/after.json (after it
# stopped) are aggregates of fixed numbers, enums and booleans only (schema: bench/internal/benchprofile;
# no task listing, command line, environment, CPU list, path or host name). They are written from the
# host and published beside RUN/samples, never mounted into or writable by the child. Each
# observation reads the threads of this machine and the /config filesystem and entries again; it
# is complete or it fails: a /proc walk cut short, an unreadable live task, an unscannable /config
# (an unreadable directory is not an empty one) or a sampler that failed after any observation is
# bench_evidence_failed, and nothing is published (a task that vanished meanwhile is not a failure).
# The mount of /config is compared in memory; the owner value is never written to the scratch. After a
# child exit 0 only the stopped container's regular samples.jsonl is exported (one
# regular file, at most 32 MiB, through the redactor) together with the three records, into the
# fresh RUN directory; a nonzero child, an observation that could not be read (bench_evidence_failed)
# or any export failure publishes nothing. Observation is not a reservation: the owner reserves the
# cores, and the next invocation, from a refreshed image, decides by record version — `bench/parser -judge
# RUN` for a V1 run and `bench/access -judge RUN` for a data mode's V2 records.
#
# BOXSET_BENCH_CGROUP_PARENT (--bench only) names one top-level cgroup slice of this machine: at most 64
# characters of [A-Za-z0-9._-] ending `.slice`, with no `/`, no `..`, no whitespace, no newline and no
# leading dash, so it can never name a path below the cgroup root. The slice must exist and its effective
# cpuset (/sys/fs/cgroup/<slice>/cpuset.cpus.effective) must be exactly the --bench CPUSET, so a container
# cannot be placed in system.slice or any other slice that holds more cores than the reservation. Any
# other value, an absent or unreadable slice, a cpuset that differs, or a docker create that fails while a
# placement is set is `dev-run: bench_invalid side=cgroup`, exit 125; unset passes no cgroup parent.
# --timeout is whole seconds: above 600 s only together with --bench, and never above 3600; every other
# form is `dev-run: timeout_invalid`.
#
# --bench-data PATH (PATH = exactly the work child of this invocation's own --write run,
# docs/spikes/data/bench-<UTC>/work) is valid only with --bench; a missing --bench, an absolute path, a
# `..` component, a link, another stamp, a repeated flag or an existing run is
# `dev-run: bench_invalid side=data` before Docker, exit 125, never side=write. The host uid is never
# restricted: the child stays fixed 1000:1000. After the image build the runner creates RUN and then
# RUN/work (plain checked mkdir), keeps both device:inode pairs in memory, binds RUN/work writable at
# /bench-data beside the unchanged read-only /config, adds only the literal BOXSET_DATA_DIR=/bench-data to
# the child, and requires the created container to show exactly those two binds. Before the start, while
# the container is stopped, a tar holding exactly one entry — the directory bench-data, uid=gid=1000,
# mode 0700, no payload and no other entry — is streamed through `docker cp -a - CTR:/`, so the trusted
# daemon sets the fresh mount root; the host then requires RUN/work uid 1000, mode 0700, the same device
# and inode, and empty. A failure of that step is bench_evidence_failed, nothing starts and nothing is
# published, and there is no fallback to a world-writable directory, a privileged child, an added
# capability, a host chown or a host-uid requirement. A data run's records are V2: one JSON object per
# stage with exactly the keys {"schema":2,"stage","run","image","inputs","host","data"}, where host
# holds the V1 host fields unchanged in name, type, range and order and the five identity keys sit on the
# envelope; data is
# {"data_fs":"ext4"|"xfs"|"other","data_nvme","data_writable","data_bound","data_mounted"} and each value is
# true only where it held at every observation of its stage: the work directory's backing filesystem and
# NVMe device; owner uid 1000, owner write, no group or other write and a read-write bind; the same device
# and inode under the same RUN identity, empty at before; the mount, read through `docker inspect` at
# before and after and through /proc/<pid>/root/bench-data while the child runs. No path, user, host name
# or inode number is recorded. Without --bench-data the records stay the V1 objects unchanged.
#
# Cleanup in data mode is the recorded sequence and nothing else: only after the measured container's
# checked removal, and only while both identities still match, a bounded same-image helper container
# (fixed uid 1000, --cap-drop ALL, --network none, --log-driver=none, only the RUN/work bind at
# /bench-data) runs exactly `sh -euc 'chmod -R u+rwX /bench-data; find /bench-data -mindepth 1 -delete'`
# and is then removed with a checked removal; the host then removes the now-empty work through its own RUN
# and publishes the four files into the empty RUN. Every exit without publication attempts the same
# identity-gated cleanup. A mismatch, a helper failure, a non-empty work or a container still present is
# `dev-run: bench_data_cleanup_failed` and nothing is published; a mismatch or a failed cleanup never
# authorizes traversing a substituted path and may leave a reported synthetic residual
# (`dev-run: bench_data_residual=PATH`) for the owner to remove.
# Bootstrap before the redactor exists prints only a fixed reason word; recovery:
# docs/spikes/s5-version-admission.md.
#
# --capture DIR (DIR = .devrites/work/<slug>/screenshots-YYYYMMDDTHHMMSSZ, which must
# not exist; it is validated before Docker and never reused or deleted) adds only the
# synthetic BOXSET_CAPTURE_DIR=/capture and a fresh 16-hex BOXSET_CAPTURE_CHALLENGE to
# the child and, only when the child exited 0, takes the four fixed files
# /capture/{dev,preview}-{desktop,mobile}.png (one regular non-link entry each, at
# most 2 MiB) from the stopped container. Each file is streamed (never written on the
# host) straight into its own fresh validator container (the installed capture-check;
# fixed non-root user, 64 MiB memory, 30 s timeout, --log-driver=none read back by
# `docker inspect` before that validator starts, else log_driver_invalid); only the
# validator's canonical PNGs plus capture.manifest are kept. The sink status is checked
# apart from the validator's, every validator is removed (checked), and the child is
# removed (checked) before anything is published, into a DIR the runner creates (0700,
# files 0644) and removes again if publication fails. A nonzero child, or any
# export/validation failure, publishes nothing. Pass line:
# DEV-RUN-CAPTURE-PASS files=4 bytes=N dir=DIR challenge=HEX (printed only after
# four positive per-validator `dev-run: validator=... log=none` observations).
set -u
set -f
LC_ALL=C
export LC_ALL
umask 022

CAP=262144
DRAFTS='s5-admission.md s5-tools.md'
WRITE_CHAIN='docs docs/spikes docs/spikes/issue-drafts'
WCAP=65536
STREAMCAP=262144
TIMEOUT=600
WRITE=
HAVE_WRITE=0
HAVE_TIMEOUT=0
RUNID=
T=
OWN=
R=
CAPTURE=
HAVE_CAPTURE=0
HAVE_BENCH=0
CAPNAMES='dev-desktop dev-mobile preview-desktop preview-mobile'
CAPMAX=2097152
CAPSTREAM=2162688
CHAL=
CAPMADE=0
CAPDONE=0
BENCHCPU=
BENCHLIST=
BENCHRUN=
BENCHCFG=
BENCHCG=
HAVE_BENCHDATA=0
BENCHDATA=
WORKSRC=
RUN_DEVID=
WORK_DEVID=
DATADONE=0
DATAMADE=0
DATACLEANED=0
DATACLEANFAIL=0
DATARES=
DMNT=
MOUNT_RW=
SAMPLER=
CID=
CHILD=
BOUND=
REMOVED=
REMOVING=
PENDING=
CLEANFAIL=0
COMPLETE=0

die() {
	printf 'dev-run: %s\n' "$1" >&2
	exit "${2:-125}"
}

usage() {
	printf '%s\n' 'usage: scripts/dev-run.sh [--write docs/spikes/issue-drafts|docs/spikes/data/bench-<UTC>/samples|internal/outbound/testdata/fixtures/<set>] [--capture DIR] [--bench CPUSET] [--bench-data PATH] [--timeout SECONDS] -- COMMAND [ARG...]' >&2
	die usage
}

# backing_nvme SYSROOT SOURCE: CFG_NVME (1 or 0) for the block device SOURCE (/dev/<name>) as SYSROOT's sysfs
# shows it; status 1 when any fact cannot be read (the disks are never named). The members are the slaves of
# an md or device-mapper block (the block itself without slaves); a partition resolves to its parent disk.
# Every disk must be non-rotational, local (a pci or platform device path, no network transport word) and
# NVMe (vendor text, or a device path component naming nvme but not nvmf). Production passes the literal /sys.
backing_nvme() {
	[ $# = 2 ] || return 1
	case $2 in /dev/*) _bc=${2#/dev/} ;; *) return 1 ;; esac
	case $_bc in '' | . | .. | *[!A-Za-z0-9._-]*) return 1 ;; esac
	_bk=$1/block/$_bc
	[ -d "$_bk" ] || return 1
	if [ -d "$_bk/slaves" ]; then
		_bm=$(timeout 30 find "$_bk/slaves" -mindepth 1 -maxdepth 1 -print 2>/dev/null) || return 1
		[ -n "$_bm" ] || return 1
	else
		_bm=$_bk
	fi
	_nl='
'
	_bd=
	while IFS= read -r _bi; do
		[ -d "$_bi" ] || return 1
		_br=$(timeout 30 readlink -f -- "$_bi" 2>/dev/null) || return 1
		[ ! -e "$_bi/partition" ] || _br=${_br%/*}
		case $_nl$_bd in *"$_nl$_br$_nl"*) ;; *) _bd=$_bd$_br$_nl ;; esac
	done <<EOF
$_bm
EOF
	[ -n "$_bd" ] || return 1
	CFG_NVME=1
	while IFS= read -r _bx; do
		[ -n "$_bx" ] || continue
		_rv=
		[ -r "$_bx/queue/rotational" ] || return 1
		read -r _rv <"$_bx/queue/rotational" 2>/dev/null || [ -n "$_rv" ] || return 1
		[ "$_rv" = 0 ] || CFG_NVME=0
		[ -L "$_bx/device" ] || return 1
		_dl=$(timeout 30 readlink -f -- "$_bx/device" 2>/dev/null) || return 1
		[ -e "$_dl" ] || return 1
		_loc=0
		_rem=0
		_nv=0
		_bo=$IFS
		IFS=/
		set -- $_dl
		IFS=$_bo
		for _c in "$@"; do
			case $_c in pci* | platform*) _loc=1 ;; esac
			case $_c in iscsi* | fc* | srp* | rbd* | nbd* | nvmf*) _rem=1 ;; esac
			case $_c in *nvmf*) ;; *nvme*) _nv=1 ;; esac
		done
		[ "$_loc$_rem" = 10 ] || CFG_NVME=0
		_vn=
		[ ! -r "$_bx/device/vendor" ] || read -r _vn <"$_bx/device/vendor" 2>/dev/null || :
		case $_vn in *[Nn][Vv][Mm][Ee]*) _nv=1 ;; esac
		[ "$_nv" = 1 ] || CFG_NVME=0
	done <<EOF
$_bd
EOF
}
# Self-check, not a usage form: --backing-nvme-check SYSROOT /dev/NAME prints nvme=1, nvme=0 or unreadable.
if [ "${1:-}" = --backing-nvme-check ]; then
	{ [ $# = 3 ] && backing_nvme "$2" "$3"; } || {
		printf 'unreadable\n'
		exit 1
	}
	printf 'nvme=%s\n' "$CFG_NVME"
	exit 0
fi

full_id() {
	[ "${#1}" = 64 ] || return 1
	case $1 in *[!0-9a-f]*) return 1 ;; esac
}
# A receipt alone is insufficient: creation facts must agree before it becomes owned.
bind_container() {
	full_id "$1" || return 1
	case $2 in "$CTR" | "$CTR-v-dev-desktop" | "$CTR-v-dev-mobile" | "$CTR-v-preview-desktop" | "$CTR-v-preview-mobile" | "$CTR-v-bench-data") ;; *) return 1 ;; esac
	_bc=$(timeout 30 docker inspect --format '{{.Id}} {{.Name}} {{index .Config.Labels "boxset.dev-run"}} {{.Image}}' "$1" 2>/dev/null) || return 1
	[ "$_bc" = "$1 /$2 $RUNID $IMG" ] || return 1
	BOUND="$BOUND$1 $2
"
	PENDING=
}
container_absent() {
	full_id "$1" || return 1
	_ca=$(timeout 30 docker ps -a --no-trunc --filter "id=$1" --format '{{.ID}}' 2>/dev/null && printf x) || return 1
	[ "$_ca" = x ]
}
remove_container() {
	full_id "$1" || return 1
	case "
$BOUND" in *"
$1 $2
"*) ;; *) return 1 ;; esac
	case " $REMOVED " in *" $1 "*) return 0 ;; esac
	_rc=$(timeout 30 docker inspect --format '{{.Id}} {{.Name}} {{index .Config.Labels "boxset.dev-run"}} {{.Image}}' "$1" 2>/dev/null) || return 1
	[ "$_rc" = "$1 /$2 $RUNID $IMG" ] || return 1
	_rs=$(timeout 30 docker inspect --format '{{.State.Status}} {{.State.ExitCode}}' "$1" 2>/dev/null) || return 1
	case $_rs in
	running\ *)
		timeout 30 docker kill "$1" >/dev/null 2>&1 || return 1
		_rw=$(timeout 60 docker wait "$1" 2>/dev/null) || return 1
		case $_rw in '' | *[!0-9]*) return 1 ;; esac
		_rs=$(timeout 30 docker inspect --format '{{.State.Status}} {{.State.ExitCode}}' "$1" 2>/dev/null) || return 1
		[ "$_rs" = "exited $_rw" ] || return 1
		;;
	esac
	case $_rs in
	created\ * | exited\ *)
		_re=${_rs#* }
		case $_re in '' | *[!0-9]*) return 1 ;; esac
		;;
	*) return 1 ;;
	esac
	# Attached output and stream/sink statuses precede this terminal observation.
	_rc=$(timeout 30 docker inspect --format '{{.Id}} {{.Name}} {{index .Config.Labels "boxset.dev-run"}} {{.Image}}' "$1" 2>/dev/null) || return 1
	[ "$_rc" = "$1 /$2 $RUNID $IMG" ] || return 1
	[ "$CLEANFAIL" = 0 ] || return 1
	REMOVING=$1
	timeout 60 docker rm "$1" >/dev/null 2>&1 || return 1
	container_absent "$1" || return 1
	REMOVED="$REMOVED $1"
	REMOVING=
}
cleanup() {
	_incoming=$?
	trap - EXIT
	trap 'CLEANFAIL=1' HUP INT TERM
	[ -z "$REMOVING" ] || CLEANFAIL=1
	[ -z "$SAMPLER" ] || : >"$T/f/stop" 2>/dev/null
	if [ -n "$PENDING" ]; then
		_pi=$(timeout 30 docker inspect --format '{{.Id}}' "$PENDING" 2>/dev/null) && bind_container "$_pi" "$PENDING" || CLEANFAIL=1
	fi
	if [ "$CLEANFAIL" = 0 ]; then
		while read -r _ci _cn; do
			[ -n "$_ci" ] || continue
			remove_container "$_ci" "$_cn" || CLEANFAIL=1
		done <<EOF
$BOUND
EOF
	fi
	if [ "$CLEANFAIL" != 0 ]; then
		printf 'dev-run: cleanup_failed\n' >&2
		[ "$_incoming" != 0 ] || _incoming=125
	fi
	# Every exit without publication attempts the same identity-gated cleanup of the data directory. A
	# mismatch or a failed cleanup never traverses a substituted path and leaves a reported residual for
	# the owner to remove. The reason word has one owner: this path prints it, once, and only for a
	# cleanup that failed (the main path marks DATACLEANFAIL and stops without printing it), so one
	# failure is never reported twice; a publication failure keeps its own word and leaves this path the
	# residual of the directory it could not finish.
	if [ "${HAVE_BENCHDATA:-0}" = 1 ] && [ "${DATADONE:-0}" = 0 ] && [ "${DATAMADE:-0}" = 1 ]; then
		if [ "${DATACLEANED:-0}" = 0 ] && [ "${DATACLEANFAIL:-0}" = 0 ]; then
			bench_data_cleanup || DATACLEANFAIL=1
		fi
		DATARES=
		{ [ "$(timeout 30 stat -c '%d:%i' -- "$BENCHRUN" 2>/dev/null)" = "$RUN_DEVID" ] && rmdir -- "$BENCHRUN" 2>/dev/null; } || DATARES=$BENCHRUN
		if [ "${DATACLEANFAIL:-0}" = 1 ]; then
			printf 'dev-run: bench_data_cleanup_failed\n' >&2
			[ "$_incoming" != 0 ] || _incoming=125
		fi
		if [ -n "$DATARES" ]; then
			printf 'dev-run: bench_data_residual=%s\n' "$DATARES" >&2
			[ "$_incoming" != 0 ] || _incoming=125
		fi
	fi
	# A capture directory this run created and did not finish publishing.
	if [ "$CAPMADE" = 1 ] && [ "$CAPDONE" = 0 ] && [ -d "$CAPTURE" ] && [ ! -L "$CAPTURE" ] && [ -O "$CAPTURE" ]; then
		rm -rf "$CAPTURE"
	fi
	if [ -n "$OWN" ] && [ -d "$T" ] && [ ! -L "$T" ] && [ -O "$T" ]; then
		if [ "$CLEANFAIL" = 0 ] && [ "$COMPLETE" = 1 ] && [ "$_incoming" = "$CHILD" ]; then
			rm -rf "$T"
		else
			printf 'dev-run: diagnostics_retained=%s\n' "$T" >&2
		fi
	fi
	exit "$_incoming"
}
trap cleanup EXIT
trap 'printf "dev-run: interrupted\n" >&2; exit 125' HUP INT TERM

# --- arguments ---------------------------------------------------------------
while [ $# -gt 0 ]; do
	case $1 in
	--) break ;;
	--capture)
		{ [ $# -ge 2 ] && [ "$HAVE_CAPTURE" = 0 ]; } || die capture_flag_invalid
		HAVE_CAPTURE=1
		CAPTURE=$2
		shift
		;;
	--bench)
		{ [ $# -ge 2 ] && [ "$HAVE_BENCH" = 0 ]; } || usage
		HAVE_BENCH=1
		BENCHCPU=$2
		shift
		;;
	--bench-data)
		{ [ $# -ge 2 ] && [ "$HAVE_BENCHDATA" = 0 ]; } || die 'bench_invalid side=data'
		HAVE_BENCHDATA=1
		BENCHDATA=$2
		shift
		;;
	--write)
		{ [ $# -ge 2 ] && [ "$HAVE_WRITE" = 0 ]; } || usage
		HAVE_WRITE=1
		WRITE=$2
		shift
		;;
	--timeout)
		{ [ $# -ge 2 ] && [ "$HAVE_TIMEOUT" = 0 ]; } || usage
		HAVE_TIMEOUT=1
		TIMEOUT=$2
		shift
		;;
	*) usage ;;
	esac
	shift
done
[ "${1:-}" = -- ] || usage
shift
[ $# -gt 0 ] || usage

# --bench-data is valid only with --bench, whatever else the invocation gets wrong: a missing --bench is
# never reported as side=write.
if [ "$HAVE_BENCHDATA" = 1 ] && [ "$HAVE_BENCH" = 0 ]; then die 'bench_invalid side=data'; fi
# --timeout is whole seconds. Until 600 s every run may ask for it; above that only a benchmark run may,
# and never above 3600 s. The rule is checked here, before the benchmark configuration, so an out-of-range
# deadline is refused as timeout_invalid whatever else the invocation asks for.
case $TIMEOUT in
'' | 0* | *[!0-9]*) die timeout_invalid ;;
esac
if [ "$TIMEOUT" -le 600 ]; then
	[ "${#TIMEOUT}" -le 3 ] || die timeout_invalid
else
	{ [ "$HAVE_BENCH" = 1 ] && [ "${#TIMEOUT}" -le 4 ] && [ "$TIMEOUT" -le 3600 ]; } || die timeout_invalid
fi

# bench_ids LIST: IDS = the CPU ids of a list such as 0-3,8 (comma-separated N or N-M of at most four
# digits without leading zeros, N <= M, at most 1024 ids in all); status 1 for anything else. Builtins
# only, so it also runs before any host tool is reachable.
bench_ids() {
	IDS=
	_bn=0
	case $1 in '' | [,-]* | *[,-]) return 1 ;; esac
	case $1 in *[!0-9,-]*) return 1 ;; esac
	_bo=$IFS
	IFS=,
	set -- $1
	IFS=$_bo
	for _bp in "$@"; do
		_ba=${_bp%%-*}
		_bb=${_bp#*-}
		{ [ -n "$_ba" ] && [ -n "$_bb" ]; } || return 1
		case $_ba$_bb in *[!0-9]*) return 1 ;; esac
		case $_ba in 0?*) return 1 ;; esac
		case $_bb in 0?*) return 1 ;; esac
		{ [ "${#_ba}" -le 4 ] && [ "${#_bb}" -le 4 ] && [ "$_ba" -le "$_bb" ]; } || return 1
		_bi=$_ba
		while [ "$_bi" -le "$_bb" ]; do
			_bn=$((_bn + 1))
			[ "$_bn" -le 1024 ] || return 1
			IDS="$IDS $_bi"
			_bi=$((_bi + 1))
		done
	done
}
if [ "$HAVE_BENCH" = 1 ]; then
	[ "$HAVE_CAPTURE" = 0 ] || die capture_flag_invalid
	bench_ids "$BENCHCPU" || die 'bench_invalid side=cpuset'
	BENCHLIST=$IDS
	[ "$HAVE_WRITE" = 1 ] || die 'bench_invalid side=write'
	case $WRITE in
	docs/spikes/data/bench-[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T[0-9][0-9][0-9][0-9][0-9][0-9]Z/samples) ;;
	*) die 'bench_invalid side=samples' ;;
	esac
	BENCHRUN=${WRITE%/samples}
	if [ "$HAVE_BENCHDATA" = 1 ]; then
		# --bench-data is exactly the work child of this invocation's own write run: the same stamp, a
		# relative path without a link anywhere above it, and a run that does not exist yet.
		case $BENCHDATA in
		docs/spikes/data/bench-[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T[0-9][0-9][0-9][0-9][0-9][0-9]Z/work) ;;
		*) die 'bench_invalid side=data' ;;
		esac
		[ "${BENCHDATA%/work}" = "$BENCHRUN" ] || die 'bench_invalid side=data'
		{ [ ! -e "$BENCHRUN" ] && [ ! -L "$BENCHRUN" ]; } || die 'bench_invalid side=data'
		# Every component of this run's own path, judged directly in order: the three workspace directories, the
		# run itself and the work leaf. A link anywhere above the leaf is refused here, before Docker, so a
		# checkout whose data directory is a link cannot pass this block and fail later as write_root_invalid.
		for _c in docs docs/spikes docs/spikes/data "$BENCHRUN" "$BENCHDATA"; do
			[ ! -L "$_c" ] || die 'bench_invalid side=data'
			[ ! -e "$_c" ] || [ -d "$_c" ] || die 'bench_invalid side=data'
		done
		# The leaf's own ban, in the recorded wording. The fresh-run rule above already refuses the run directory when it
		# exists at all, and a link leaf implies an existing one, so this line restates the ban and is never the first refusal.
		{ [ ! -e "$BENCHDATA" ] && [ ! -L "$BENCHDATA" ]; } || die 'bench_invalid side=data'
		# The work directory as the daemon receives it: an absolute path without the characters a
		# --mount option would split on (the same rule the scratch directory meets).
		_hr=$(pwd -P 2>/dev/null) || die 'bench_invalid side=data'
		case $_hr in '' | *[,:]*) die 'bench_invalid side=data' ;; esac
		WORKSRC=$_hr/$BENCHRUN/work
	fi
	BENCHCFG=${BOXSET_BENCH_CONFIG:-}
	[ -n "$BENCHCFG" ] || die 'bench_unavailable side=config'
	case $BENCHCFG in /*) ;; *) die 'bench_invalid side=config' ;; esac
	case $BENCHCFG in *[!A-Za-z0-9_./+@=-]* | */ | *//* | */./* | */../* | */. | */..) die 'bench_invalid side=config' ;; esac
	{ [ -d "$BENCHCFG" ] && [ ! -L "$BENCHCFG" ]; } || die 'bench_invalid side=config'
	BENCHCG=${BOXSET_BENCH_CGROUP_PARENT:-}
	if [ -n "$BENCHCG" ]; then
		# One top-level slice name: no path, no `..`, no whitespace, no leading dash, no empty stem.
		case $BENCHCG in
		*.slice) ;;
		*) die 'bench_invalid side=cgroup' ;;
		esac
		case $BENCHCG in
		-* | *[!A-Za-z0-9._-]* | *..* | .slice) die 'bench_invalid side=cgroup' ;;
		esac
		[ "${#BENCHCG}" -le 64 ] || die 'bench_invalid side=cgroup'
		# The placement must be this machine's own slice for exactly the reserved cores: a slice that is
		# absent, unreadable, or holds any other cpuset (system.slice above all) would let competing work
		# share the measurement, so it is refused before Docker with its own reason word.
		_cgc=$(cat "/sys/fs/cgroup/$BENCHCG/cpuset.cpus.effective" 2>/dev/null) || die 'bench_invalid side=cgroup'
		bench_ids "$_cgc" || die 'bench_invalid side=cgroup'
		[ "$IDS" = "$BENCHLIST" ] || die 'bench_invalid side=cgroup'
	fi
fi
if [ "$HAVE_WRITE" = 1 ]; then
	case $WRITE in
	docs/spikes/issue-drafts) ;;
	docs/spikes/data/bench-*/samples)
		[ "$HAVE_BENCH" = 1 ] || die write_path_invalid
		DRAFTS='samples.jsonl'
		WRITE_CHAIN='docs docs/spikes docs/spikes/data'
		WCAP=33554432
		STREAMCAP=33751040
		;;
	internal/outbound/testdata/fixtures/*)
		# The set name is the only variable part of this root: one lower-case
		# [a-z0-9-] word of at most 32 characters. Anything else or a nested path is
		# not a set; the set's own directory is judged by the write chain below.
		_set=${WRITE#internal/outbound/testdata/fixtures/}
		case $_set in
		'' | *[!a-z0-9-]*) die write_path_invalid ;;
		esac
		[ "${#_set}" -le 32 ] || die write_path_invalid
		DRAFTS='exchanges.json'
		# The set directory is part of the chain: a linked or non-directory set is a
		# refused root, exactly as a linked draft root is.
		WRITE_CHAIN="internal internal/outbound internal/outbound/testdata internal/outbound/testdata/fixtures $WRITE"
		WCAP=8388608
		STREAMCAP=8585216
		;;
	*) die write_path_invalid ;;
	esac
fi

{ [ -f deploy/dev/Dockerfile ] && [ -f scripts/dev-run.sh ] && [ -f .dockerignore ]; } || die not_repo_root
# The benchmark run directory is fresh: it must not exist yet.
if [ "$HAVE_BENCH" = 1 ]; then
	{ [ ! -e "$BENCHRUN" ] && [ ! -L "$BENCHRUN" ]; } || die 'bench_invalid side=samples'
fi

# chain_ok PATH: every component of the relative PATH is a real directory, not a link.
chain_ok() {
	_cr=$1
	_cur=
	while [ -n "$_cr" ]; do
		_c=${_cr%%/*}
		case $_cr in */*) _cr=${_cr#*/} ;; *) _cr= ;; esac
		_cur=${_cur:+$_cur/}$_c
		{ [ -d "$_cur" ] && [ ! -L "$_cur" ]; } || return 1
	done
}
# The capture destination is validated before any Docker call: a fresh leaf under an
# existing workspace directory, never an existing entry.
if [ "$HAVE_CAPTURE" = 1 ]; then
	case $CAPTURE in
	.devrites/work/*/screenshots-[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T[0-9][0-9][0-9][0-9][0-9][0-9]Z) ;;
	*) die capture_destination_invalid ;;
	esac
	_rest=${CAPTURE#.devrites/work/}
	_slug=${_rest%%/*}
	case ${_rest#*/} in */*) die capture_destination_invalid ;; esac
	case $_slug in '' | [!a-z0-9]* | *[!a-z0-9-]*) die capture_destination_invalid ;; esac
	[ "${#_slug}" -le 64 ] || die capture_destination_invalid
	chain_ok ".devrites/work/$_slug" || die capture_destination_invalid
	if [ -e "$CAPTURE" ] || [ -L "$CAPTURE" ]; then die capture_destination_exists; fi
fi

# write_chain_ok: no component of the write root is a link or a non-directory.
write_chain_ok() {
	for _c in $WRITE_CHAIN; do
		[ ! -L "$_c" ] || return 1
		[ ! -e "$_c" ] || [ -d "$_c" ] || return 1
	done
}
[ "$HAVE_WRITE" = 0 ] || write_chain_ok || die write_root_invalid

for _t in docker mktemp timeout od tr rm mv mkdir rmdir cat find sha256sum chmod grep head tar env sed wc; do
	command -v "$_t" >/dev/null 2>&1 || die host_tool_missing
done
if [ "$HAVE_BENCH" = 1 ]; then
	for _t in awk date df findmnt sort id timeout stat readlink; do
		command -v "$_t" >/dev/null 2>&1 || die host_tool_missing
	done
	# The benchmark counts competing tasks in /proc: that view must be positively the whole machine's, and
	# a share of the kernel's task count is not such a proof (one hidden task is a tiny share). So the mount
	# table is the proof: exactly one mount at /proc, of type proc, with no hidepid other than 0/off and no
	# subset=pid (whoever the caller is; root is not proof), and no mount of any kind over a task entry
	# (/proc/<pid>, /proc/self, /proc/thread-self or anything below them) that could mask a task's tree. No
	# /proc mount line at all is not proof either. The proof is repeated around every observation, so a mask
	# applied after startup is rejected too; each observation also compares the tasks it read with the
	# kernel's own total, only as a sanity bound against a gross failure, never as proof.
	bench_proc_ok() {
		awk '
	{ for (i = 1; i <= NF && $i != "-"; i++) ; if (i > NF) next
	  mp = $5
	  if (mp ~ /^\/proc\/([0-9]+|self|thread-self)(\/|$)/) bad = 1
	  if (mp != "/proc") next
	  n++
	  if ($(i + 1) != "proc") bad = 1
	  s = $6 "," $(i + 3)
	  c = split(s, o, ",")
	  for (j = 1; j <= c; j++) {
	    if (o[j] ~ /^hidepid=/ && o[j] != "hidepid=0" && o[j] != "hidepid=off") bad = 1
	    if (o[j] ~ /^subset=/) bad = 1
	  }
	}
	END { exit (n == 1 && !bad) ? 0 : 1 }' /proc/self/mountinfo 2>/dev/null
	}
	# bench_must CMD...: a benchmark step that must succeed: status 4 is the proc proof failing, anything
	# else nonzero is an unreadable or incomplete observation.
	bench_must() {
		"$@"
		_bm=$?
		[ "$_bm" != 4 ] || die 'bench_invalid side=proc'
		[ "$_bm" = 0 ] || die bench_evidence_failed
	}
	bench_proc_ok || die 'bench_invalid side=proc'
fi
# The daemon the caller selected (context, DOCKER_HOST) is resolved with the
# caller's own configuration and then pinned; the private configuration below
# must reach the same daemon or the run stops.
DID=$(timeout 30 docker info --format '{{.ID}}' 2>/dev/null) && [ -n "$DID" ] || die docker_unavailable
DH=$(timeout 30 docker context inspect --format '{{.Endpoints.docker.Host}}' 2>/dev/null) && [ -n "$DH" ] || die docker_unavailable
# The benchmark observes this machine's /proc and /sys: its daemon must be a unix socket of this machine
# that runs this machine's kernel, else the run is rejected before anything is measured or built. The
# endpoint is never printed.
bench_local_daemon() {
	case $DH in unix:///*) ;; *) return 1 ;; esac
	[ -S "${DH#unix://}" ] || return 1
	_dk=$(timeout 30 docker info --format '{{.KernelVersion}}' 2>/dev/null) || return 1
	_lk=$(cat /proc/sys/kernel/osrelease 2>/dev/null) || return 1
	{ [ -n "$_lk" ] && [ "$_dk" = "$_lk" ]; } || return 1
}
[ "$HAVE_BENCH" = 0 ] || bench_local_daemon || die 'bench_invalid side=daemon'

# --- run-private scratch -------------------------------------------------------
T=$(mktemp -d "${TMPDIR:-/tmp}/boxset-dev-run.XXXXXX" 2>/dev/null) || die temp_dir_failed
OWN=1
case $T in *[,:]* | *'"'* | *' '*) die temp_dir_failed ;; esac
{ [ -d "$T" ] && [ ! -L "$T" ] && [ -O "$T" ]; } || die temp_dir_failed
mkdir -m 0700 -- "$T/dockercfg" || die temp_dir_failed
DOCKER_CONFIG=$T/dockercfg
DOCKER_HOST=$DH
export DOCKER_CONFIG DOCKER_HOST
unset DOCKER_CONTEXT
[ "$(timeout 30 docker info --format '{{.ID}}' 2>/dev/null)" = "$DID" ] || die docker_unavailable
RUNID=$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')
case $RUNID in
*[!0-9a-f]*) die temp_dir_failed ;;
????????????????) ;;
*) die temp_dir_failed ;;
esac
if [ "$HAVE_CAPTURE" = 1 ]; then
	CHAL=$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')
	case $CHAL in
	*[!0-9a-f]*) die temp_dir_failed ;;
	????????????????) ;;
	*) die temp_dir_failed ;;
	esac
fi

# --- redactor: built and exported to the scratch directory ----------------------
# The helper runs on the host so owner values never enter Docker. Output of this
# stage is discarded; any failure prints only the fixed reason word.
R=$T/bin/redact
mkdir -m 0700 -- "$T/bin" || die redact_unavailable
timeout --kill-after=10s 1800s docker build --file deploy/dev/Dockerfile --target host-redact \
	--output "type=local,dest=$T/bin" . >/dev/null 2>&1 || die redact_unavailable
[ -f "$R" ] || die redact_unavailable
"$R" -check </dev/null >/dev/null 2>&1 || die redact_unavailable

# --- work image ----------------------------------------------------------------
# Build output is kept only as a redacted tail and shown only when the build fails.
# The build status goes through its own fresh scratch file and must be a number.
rm -f -- "$T/bst"
{ timeout --kill-after=10s 1800s docker build --file deploy/dev/Dockerfile --target work \
	--iidfile "$T/iid" --progress plain . 2>&1 </dev/null; printf '%s' "$?" >"$T/bst"; } | "$R" -tail 16384 >"$T/tail"
RS=$?
SS=$(cat "$T/bst" 2>/dev/null)
if [ "$SS" != 0 ] || [ "$RS" != 0 ]; then
	[ "$RS" != 0 ] || cat "$T/tail" >&2
	die build_failed
fi
IMG=$(cat "$T/iid" 2>/dev/null)
case $IMG in
sha256:????????????????????????????????????????????????????????????????) ;;
*) die build_failed ;;
esac

# --- benchmark data directory (--bench-data) ---------------------------------------------------
# The host creates, observes and removes the child's data directory; the child only ever writes through
# its bind. Nothing here ever traverses a path whose identity stopped matching.
# bench_mount_devfs MOUNTINFO: "<major:minor> <fstype>" of the mount at /bench-data in a mountinfo table,
# or nothing when the table names none. A mountinfo line is `id parent major:minor root mount_point
# mount_options [optional...] - fstype source super_options`: the filesystem word is the field after the
# `-` separator, never a fixed position (field 9 is the mount source of a bind, e.g. /dev/md4).
bench_mount_devfs() {
	awk -v mp=/bench-data '
		{ for (i = 1; i <= NF && $i != "-"; i++) ; if (i > NF) next }
		$5 == mp { print $3, $(i + 1); exit }' "$1"
}
# bench_mounts: DMNT (1 or 0) for the created container's exact mount set — none, the one read-only
# /config bind, or with data mode that bind beside the writable work bind, whose running container must
# also show the work directory's device and inode at /bench-data — and MOUNT_RW (1 when the work bind is
# the expected read-write bind). Status 1 when a fact cannot be read; a set that reads but is not the
# expected one is DMNT=0 and stays recorded, so the judge decides. The /proc leg is a fact only while a
# running child shows it: a child that exits between the state read and the path read is a vanished
# process, not an unreadable observation, so one second inspection decides — no longer running makes the
# leg not applicable (the read mount set stands), still running with an unreadable root stays status 1.
bench_mounts() {
	DMNT=1
	MOUNT_RW=0
	_mi=$(timeout 30 docker inspect --format '{{range .Mounts}}{{.Type}} {{.Source}} {{.Destination}} {{.RW}}{{println}}{{end}}' "$CTR" 2>/dev/null) || return 1
	_n=0
	_cfg=0
	while IFS= read -r _ml; do
		[ -n "$_ml" ] || continue
		_n=$((_n + 1))
		case $_ml in
		"bind $BENCHCFG /config false") _cfg=$((_cfg + 1)) ;;
		"bind $WORKSRC /bench-data true") MOUNT_RW=1 ;;
		esac
	done <<EOF
$_mi
EOF
	case $HAVE_BENCH$HAVE_BENCHDATA in
	00) { [ "$_n" = 0 ] && [ "$_cfg" = 0 ] && [ "$MOUNT_RW" = 0 ]; } || DMNT=0 ;;
	10) { [ "$_n" = 1 ] && [ "$_cfg" = 1 ] && [ "$MOUNT_RW" = 0 ]; } || DMNT=0 ;;
	11) { [ "$_n" = 2 ] && [ "$_cfg" = 1 ] && [ "$MOUNT_RW" = 1 ]; } || DMNT=0 ;;
	*) DMNT=0 ;;
	esac
	if [ "$HAVE_BENCHDATA" = 1 ]; then
		_mp=$(timeout 30 docker inspect --format '{{.State.Pid}} {{.State.Running}}' "$CTR" 2>/dev/null) || return 1
		set -- $_mp
		[ $# = 2 ] || return 1
		if [ "$2" = true ]; then
			[ "$1" -gt 0 ] 2>/dev/null || return 1
			if _md=$(timeout 30 stat -c '%d:%i' -- "/proc/$1/root/bench-data" 2>/dev/null); then
				[ "$_md" = "$WORK_DEVID" ] || DMNT=0
			else
				_mp=$(timeout 30 docker inspect --format '{{.State.Running}}' "$CTR" 2>/dev/null) || return 1
				# A product that made its own process non-dumpable
				# (internal/child's PrepareManagedOwner) cannot expose
				# /proc/<pid>/root to this user. The mount table is still
				# world-readable: confirm the same filesystem is mounted at
				# /bench-data, beside the inspected bind. A container that
				# stopped meanwhile leaves the inspected mount set standing.
				if [ "$_mp" = true ]; then
					_mm=$(bench_mount_devfs "/proc/$1/mountinfo" 2>/dev/null)
					_awk=$?
					# The re-inspection after a failed read: a table that went away because the task
					# vanished is not a hard failure (the inspected mount set stands); a table that
					# cannot be read while the task still runs is an unreadable observation.
					if [ "$_awk" != 0 ]; then
						_mp=$(timeout 30 docker inspect --format '{{.State.Running}}' "$CTR" 2>/dev/null) || return 1
						[ "$_mp" != true ] || return 1
					else
						# The mount must be the work directory's own filesystem: same major:minor as
						# the host directory it binds, and a benchmark filesystem word.
						_hm=$(timeout 30 findmnt -n -o MAJ:MIN --target "$BENCHRUN/work" 2>/dev/null) || return 1
						_hm=$(printf '%s\n' "$_hm" | awk 'NR == 1 { gsub(/[ \t]/, ""); print }')
						case $_hm in '' | *[!0-9:]*) return 1 ;; esac
						set -- $_mm
						{ [ $# = 2 ] && [ "$1" = "$_hm" ]; } || DMNT=0
						case ${2:-} in
						ext4 | xfs) ;;
						*) DMNT=0 ;;
						esac
					fi
				fi
			fi
		fi
	fi
}
# bench_root: the child's data directory starts as the mount created for it — owned by the child's own
# uid, mode 0700 and empty. The stream holds exactly one entry, the literal directory bench-data
# (uid=gid=1000, mode 0700, no payload, no other entry), and the trusted daemon, never this host user,
# applies that ownership and mode to the fresh mount root. The host then requires the directory it kept by
# identity: uid 1000, mode 0700, the same device and inode, empty.
bench_root() {
	mkdir -m 0700 -- "$T/mnt" "$T/mnt/bench-data" || return 1
	{ tar -cf - --format=ustar --numeric-owner --owner=1000 --group=1000 --mtime=@0 -C "$T/mnt" bench-data; printf '%s' "$?" >"$T/tars"; } |
		timeout 60 docker cp -a - "$CTR:/" >/dev/null 2>&1
	_cps=$?
	{ [ "$(cat "$T/tars" 2>/dev/null)" = 0 ] && [ "$_cps" = 0 ]; } || return 1
	_rf=$(timeout 30 stat -c '%u %a %d:%i' -- "$BENCHRUN/work" 2>/dev/null) || return 1
	set -- $_rf
	{ [ $# = 3 ] && [ "$1" = 1000 ] && [ "$2" = 700 ] && [ "$3" = "$WORK_DEVID" ]; } || return 1
	_rl=$(timeout 30 find "$BENCHRUN/work" -mindepth 1 -maxdepth 1 -print -quit 2>/dev/null) || return 1
	[ -z "$_rl" ] || return 1
	[ "$(timeout 30 stat -c '%d:%i' -- "$BENCHRUN" 2>/dev/null)" = "$RUN_DEVID" ]
}
# bench_data_cleanup: the recorded cleanup of the data directory, and nothing else. The measured container
# must be gone and both identities must still match; a bounded same-image helper (fixed uid 1000, no
# capabilities, no network, no daemon log, only the work bind at /bench-data) empties the directory the
# child owns, is removed with a checked removal, and the host removes the now-empty work through its own
# run directory. A mismatch or any failure leaves the path alone and reports status 1.
bench_data_cleanup() {
	[ "$CLEANFAIL" = 0 ] || return 1
	{ [ -z "$CID" ] || container_absent "$CID"; } || return 1
	[ "$(timeout 30 stat -c '%d:%i' -- "$BENCHRUN" 2>/dev/null)" = "$RUN_DEVID" ] || return 1
	[ "$(timeout 30 stat -c '%d:%i' -- "$BENCHRUN/work" 2>/dev/null)" = "$WORK_DEVID" ] || return 1
	{ [ -d "$BENCHRUN" ] && [ ! -L "$BENCHRUN" ] && [ -d "$BENCHRUN/work" ] && [ ! -L "$BENCHRUN/work" ]; } || return 1
	_hc=$CTR-v-bench-data
	PENDING=$_hc
	timeout 60 docker create --name "$_hc" --label "boxset.dev-run=$RUNID" --network none \
		--cap-drop ALL --security-opt no-new-privileges --log-driver=none --user 1000:1000 \
		--mount "type=bind,source=$WORKSRC,target=/bench-data" "$IMG" \
		sh -euc 'chmod -R u+rwX /bench-data; find /bench-data -mindepth 1 -delete' >"$T/create.id" 2>/dev/null || return 1
	_hid=$(cat "$T/create.id" 2>/dev/null) || return 1
	bind_container "$_hid" "$_hc" || return 1
	_ho=$(timeout 30 docker inspect --format '{{.HostConfig.LogConfig.Type}} {{.Config.User}} {{range .Mounts}}{{.Destination}} {{.RW}}{{end}}' "$_hc" 2>/dev/null) || return 1
	[ "$_ho" = 'none 1000:1000 /bench-data true' ] || return 1
	timeout --foreground --kill-after=5s 120s docker start -a "$_hc" >/dev/null 2>&1 </dev/null || return 1
	_hs=$(timeout 30 docker inspect --format '{{.State.Status}} {{.State.ExitCode}}' "$_hc" 2>/dev/null) || return 1
	[ "$_hs" = 'exited 0' ] || return 1
	remove_container "$_hid" "$_hc" || return 1
	rmdir -- "$BENCHRUN/work"
}
if [ "$HAVE_BENCHDATA" = 1 ]; then
	# The run directory and its work child exist before the container is created: plain checked mkdir, host
	# owned, and both device:inode pairs are kept in memory for every later identity check.
	mkdir -p -- "${BENCHRUN%/*}" || die publish_failed
	mkdir -- "$BENCHRUN" 2>/dev/null || die 'bench_invalid side=data'
	DATAMADE=1
	mkdir -m 0700 -- "$BENCHRUN/work" 2>/dev/null || die bench_evidence_failed
	RUN_DEVID=$(timeout 30 stat -c '%d:%i' -- "$BENCHRUN" 2>/dev/null) || die bench_evidence_failed
	WORK_DEVID=$(timeout 30 stat -c '%d:%i' -- "$BENCHRUN/work" 2>/dev/null) || die bench_evidence_failed
	case $RUN_DEVID in [0-9]*:[0-9]*) ;; *) die bench_evidence_failed ;; esac
	case $WORK_DEVID in [0-9]*:[0-9]*) ;; *) die bench_evidence_failed ;; esac
fi

# --- container -------------------------------------------------------------------
CTR=boxset-dev-run-$RUNID
# The literal child environment, exactly as `docker create` receives it.
cat >"$T/lit" <<'EOF' || die create_failed
PATH=/usr/local/go/bin:/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin
HOME=/home/boxset
TMPDIR=/tmp
LANG=C.UTF-8
LC_ALL=C.UTF-8
GOPATH=/go
GOCACHE=/var/cache/go-build
GOMODCACHE=/go/pkg/mod
GOFLAGS=-mod=readonly
GOPROXY=off
GOSUMDB=off
GOTOOLCHAIN=local
GOTELEMETRY=off
GOENV=off
GOLANGCI_LINT_CACHE=/tmp/golangci-lint-cache
BUN_INSTALL_CACHE_DIR=/tmp/bun-install-cache
BUN_RUNTIME_TRANSPILER_CACHE_PATH=/tmp/bun-transpiler-cache
XDG_CACHE_HOME=/tmp/xdg-cache
PLAYWRIGHT_BROWSERS_PATH=/ms-playwright
PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1
DO_NOT_TRACK=1
EOF
# Capture adds only its two synthetic variables.
if [ "$HAVE_CAPTURE" = 1 ]; then
	printf 'BOXSET_CAPTURE_DIR=/capture\nBOXSET_CAPTURE_CHALLENGE=%s\n' "$CHAL" >>"$T/lit" || die create_failed
fi
# Data mode adds only its own synthetic data directory.
if [ "$HAVE_BENCHDATA" = 1 ]; then
	printf 'BOXSET_DATA_DIR=/bench-data\n' >>"$T/lit" || die create_failed
fi
# create_ctr CHILD_ARGV...: docker create of the owned container, with no mount, or with --bench the
# read-only /config bind and with --bench-data the writable work bind beside it; the image is followed by
# the byte-exact child argv. The limits and the bind set are built once here and every mode shares the one
# create below, so a change to either can never drift from the other; both mount readbacks still fail
# closed on whatever this line produced.
create_ctr() {
	PENDING=$CTR
	_lim=
	_mnt_cfg=
	_mnt_data=
	if [ "$HAVE_BENCH" = 1 ]; then
		_lim=1
		_mnt_cfg="type=bind,source=$BENCHCFG,target=/config,readonly"
		[ "$HAVE_BENCHDATA" = 0 ] || _mnt_data="type=bind,source=$WORKSRC,target=/bench-data"
	fi
	timeout 60 docker create --name "$CTR" --label "boxset.dev-run=$RUNID" --network none \
		--cap-drop ALL --security-opt no-new-privileges --log-driver=none --user 1000:1000 --workdir /src \
		${_lim:+--cpuset-cpus "$BENCHCPU" --memory 4g --memory-swap 4g} \
		${_lim:+${BENCHCG:+--cgroup-parent "$BENCHCG"}} \
		${_mnt_cfg:+--mount "$_mnt_cfg"} ${_mnt_data:+--mount "$_mnt_data"} \
		--env-file "$T/lit" "$IMG" "$@" >"$T/create.id" 2>/dev/null
}
if ! create_ctr "$@"; then
	# A create that fails while a placement is set is the placement's own refusal (the slice may have
	# gone, or the daemon may reject it), never the opaque create_failed.
	[ -z "$BENCHCG" ] || die 'bench_invalid side=cgroup'
	die create_failed
fi
CID=$(cat "$T/create.id" 2>/dev/null) || die create_failed
bind_container "$CID" "$CTR" || die create_failed
[ "$(timeout 30 docker inspect --format '{{.HostConfig.LogConfig.Type}}' "$CTR" 2>/dev/null)" = none ] || die log_driver_invalid
# Effective mounts: none, or with --bench exactly the one read-only /config bind of the owner's
# directory, and with --bench-data the writable work bind beside it.
bench_mounts || die mounts_invalid
[ "$DMNT" = 1 ] || die mounts_invalid
# With --bench-data the mount root is set here, while the container is stopped and its bind set verified.
[ "$HAVE_BENCHDATA" = 0 ] || bench_root || die bench_evidence_failed
# Effective environment: every created-container line is a literal above or the
# image's own, and every literal survived unchanged (nothing injected, nothing replaced).
timeout 30 docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$CTR" >"$T/eff" 2>/dev/null || die env_invalid
timeout 30 docker image inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$IMG" >"$T/img.env" 2>/dev/null || die env_invalid
[ -s "$T/eff" ] || die env_invalid
while IFS= read -r _l; do
	grep -Fxq -- "$_l" "$T/lit" || grep -Fxq -- "$_l" "$T/img.env" || die env_invalid
done <"$T/eff"
while IFS= read -r _l; do
	grep -Fxq -- "$_l" "$T/eff" || die env_invalid
done <"$T/lit"
timeout 60 docker cp "$CTR:/usr/local/share/boxset/inputs.sha256" "$T/inputs" >/dev/null 2>&1 || die inputs_unavailable
{ [ -f "$T/inputs" ] && [ ! -L "$T/inputs" ]; } || die inputs_unavailable
INPUTS=$(sha256sum <"$T/inputs") || die inputs_unavailable
INPUTS=${INPUTS%% *}
printf 'dev-run: image=%s inputs=%s timeout=%ss container=%s log=none\n' "$IMG" "$INPUTS" "$TIMEOUT" "$CTR" >&2

# --- benchmark host evidence (--bench) -----------------------------------------------------
# The host observes; the child never sees or writes these. Every record is aggregates of fixed
# numbers, enums and booleans (schema: bench/internal/benchprofile): no CPU list, task
# listing, command line, environment, path or host name. A value that cannot be read fails closed
# (bench_evidence_failed, nothing published); a value that reads but is not eligible is recorded and
# the judge decides, except the storage word, which refuses the run before measurement when it is neither
# ext4 nor XFS.
# bench_topology: TOPO_CPUS, TOPO_CORES, TOPO_TPC (largest SMT sibling count of a member), TOPO_SIBOUT
# (siblings that are not members) and TOPO_NUMA of the reserved set. An unreadable CPU zeroes cores,
# threads and nodes, which the judge rejects.
SYSCPU=/sys/devices/system/cpu
bench_topology() {
	TOPO_CPUS=0
	TOPO_TPC=0
	TOPO_SIBOUT=0
	TOPO_SIBLIST=
	_unk=0
	: >"$T/f/cores" && : >"$T/f/nodes" || return 1
	for _c in $BENCHLIST; do
		_d=$SYSCPU/cpu$_c
		TOPO_CPUS=$((TOPO_CPUS + 1))
		_pk=$(cat "$_d/topology/physical_package_id" 2>/dev/null) || _unk=1
		_co=$(cat "$_d/topology/core_id" 2>/dev/null) || _unk=1
		_sl=$(cat "$_d/topology/thread_siblings_list" 2>/dev/null) || _unk=1
		case $_pk$_co in '' | *[!0-9]*) _unk=1 ;; esac
		printf '%s:%s\n' "$_pk" "$_co" >>"$T/f/cores" || return 1
		if bench_ids "$_sl"; then
			_k=0
			for _s in $IDS; do
				_k=$((_k + 1))
				case " $BENCHLIST " in *" $_s "*) ;; *) TOPO_SIBOUT=$((TOPO_SIBOUT + 1)); TOPO_SIBLIST="$TOPO_SIBLIST $_s" ;; esac
			done
			[ "$_k" -le "$TOPO_TPC" ] || TOPO_TPC=$_k
		else
			_unk=1
		fi
		find "$_d" -maxdepth 1 -name 'node[0-9]*' -printf '%f\n' 2>/dev/null >>"$T/f/nodes"
	done
	TOPO_CORES=$(sort -u "$T/f/cores" | wc -l | tr -d ' ')
	TOPO_NUMA=$(sort -u "$T/f/nodes" | wc -l | tr -d ' ')
	if [ "$_unk" = 1 ]; then
		TOPO_CORES=0
		TOPO_TPC=0
		TOPO_NUMA=0
	fi
	# Where the per-CPU node links name none, the kernel's own possible list decides when it is
	# present; only an absent node sysfs (a kernel without NUMA) is inferred as exactly one node,
	# never a scan that merely found nothing.
	if [ "$TOPO_NUMA" = 0 ] && [ "$_unk" = 0 ]; then
		if [ -r /sys/devices/system/node/possible ]; then
			_np=$(cat /sys/devices/system/node/possible 2>/dev/null) && bench_ids "$_np" && set -- $IDS && TOPO_NUMA=$#
		elif [ ! -e /sys/devices/system/node ]; then
			TOPO_NUMA=1
		fi
	fi
}
# bench_overlap: "ALLOWED RUNNABLE SEEN": the user threads outside this run's container (CID) whose
# allowed CPU list meets the reserved set, how many of them are runnable, and how many threads were
# read. Kernel threads (no address space), threads that exited while being read and the scanner
# itself (this function's own shell and everything below it) are not counted. The enumeration is
# complete or the call fails: the list of threads goes through a scratch file, the status of
# `find` is kept, and a timeout, an unreadable live status file, or a read that ends early fails
# the observation (nothing is counted from it). A path that is already gone is a vanished task,
# whatever word find used. A sibling file this walk does not read (mem, fdinfo, and the rest) may
# return EINVAL while its status file is fine, so that diagnostic is not a failed read.
# bench_diag_ok LINE: status 0 when LINE does not spoil the enumeration.
bench_diag_ok() {
	_ln=$1
	case $_ln in
	*"No such file or directory") return 0 ;;
	esac
	_path=$(printf '%s\n' "$_ln" | sed -n "s/^\\([^ ]*\\/\\)\\{0,1\\}find: [‘']\\(.*\\)[’']: .*/\\2/p")
	[ -n "$_path" ] || return 1
	_kind=$(printf '%s\n' "$_path" | sed -n \
		-e 's#^/proc/[0-9][0-9]*/task/[0-9][0-9]*/status$#status#p' \
		-e 's#^/proc/[0-9][0-9]*$#ancestor#p' \
		-e 's#^/proc/[0-9][0-9]*/task$#ancestor#p' \
		-e 's#^/proc/[0-9][0-9]*/task/[0-9][0-9]*$#ancestor#p')
	case $_kind in
	status | ancestor)
		[ -e "$_path" ] || [ -L "$_path" ] || return 0
		return 1
		;;
	esac
	return 0
}
bench_overlap() {
	timeout 30 find /proc -mindepth 4 -maxdepth 4 -path '/proc/[0-9]*/task/[0-9]*/status' -print >"$T/f/tasks" 2>"$T/f/terr"
	_frc=$?
	case $_frc in 0 | 1) ;; *) return 1 ;; esac
	[ "$_frc" = 0 ] || [ -s "$T/f/terr" ] || return 1
	while IFS= read -r _ln; do
		bench_diag_ok "$_ln" || return 1
	done <"$T/f/terr"
	_ovo=$(awk -v set="$BENCHLIST" -v sibs="$TOPO_SIBLIST" -v cid="$CID" -v cgroot=/sys/fs/cgroup '
function exists(path) { return system("test -e \x27" path "\x27") == 0 }
# inlist: whether CPU c is in a CPU list such as 0-3,8.
function inlist(list, c,   i, k, p, rg, lo, hi) {
	k = split(list, p, ",")
	for (i = 1; i <= k; i++) {
		lo = p[i]
		hi = lo
		if (index(lo, "-")) {
			split(p[i], rg, "-")
			lo = rg[1]
			hi = rg[2]
		}
		if (c + 0 >= lo + 0 && c + 0 <= hi + 0 && p[i] != "") return 1
	}
	return 0
}
function intersects(list,   c) {
	for (c in want) if (inlist(list, c)) return 1
	return 0
}
# meets: a reserved CPU in both the affinity list and the effective cpuset list.
function meets(list, eff,   c) {
	for (c in want) if (inlist(list, c) && inlist(eff, c)) return 1
	return 0
}
# effcpus: the effective cpuset list of the cgroup a cgroup file names (cgroup v1 cpuset line or v2), found
# at the nearest ancestor that has the file; ALL when no ancestor has one (no cpuset restriction); GONE
# when the task vanished. An existing file that cannot be read, or a cgroup file that fails every read of
# a live task, fails the scan (bad).
function effcpus(cgf,   line, rc, try, v1, v2, key, base, suffix, p, f, e) {
	for (try = 1; try <= 3; try++) {
		v1 = ""
		v2 = ""
		while ((rc = (getline line < cgf)) > 0) {
			if (line ~ /^0::/) v2 = substr(line, 4)
			else if (line ~ /^[0-9]+:([^:]*,)?cpuset(,[^:]*)?:/) {
				v1 = line
				sub(/^[^:]*:[^:]*:/, "", v1)
			}
		}
		close(cgf)
		if (rc >= 0) break
		if (!exists(cgf)) return "GONE"
		if (try == 3) {
			bad = 1
			return "GONE"
		}
	}
	if (v1 != "") {
		key = "1" v1
		base = cgroot "/cpuset"
		suffix = "/cpuset.effective_cpus"
		p = v1
	} else if (v2 != "") {
		key = "2" v2
		base = cgroot
		suffix = "/cpuset.cpus.effective"
		p = v2
	} else return "ALL"
	sub(/ \(deleted\)$/, "", p)
	if (key in cache) return cache[key]
	cache[key] = "ALL"
	for (;;) {
		f = base p suffix
		e = ""
		rc = (getline e < f)
		if (rc >= 0) {
			close(f)
			cache[key] = e
			break
		}
		if (exists(f)) {
			bad = 1
			break
		}
		if (p == "") break
		sub(/\/[^\/]*$/, "", p)
	}
	return cache[key]
}
# readthread: up to three reads of one task. A task that is gone before or after a failed read is a
# vanished task, not a failure; one that fails every read while it is still there is an unreadable
# live task and fails the scan (bad).
function readthread(path,   line, f, tg, pp, st, vm, al, rc, try) {
	for (try = 1; try <= 3; try++) {
		tg = ""
		pp = ""
		st = ""
		vm = 0
		al = ""
		while ((rc = (getline line < path)) > 0) {
			split(line, f, /[ \t]+/)
			if (f[1] == "Tgid:") tg = f[2]
			else if (f[1] == "PPid:") pp = f[2]
			else if (f[1] == "State:") st = f[2]
			else if (f[1] == "VmSize:") vm = 1
			else if (f[1] == "Cpus_allowed_list:") al = f[2]
		}
		close(path)
		if (rc >= 0 && tg != "" && al != "") break
		if (!exists(path)) return
		if (try == 3) {
			bad = 1
			return
		}
	}
	seen++
	if (!(tg in par)) par[tg] = pp
	if (vm && intersects(al)) {
		n++
		who[n] = tg
		state[n] = st
		alw[n] = al
		cgf[n] = substr(path, 1, length(path) - 6) "cgroup"
	}
	# A running user thread, wherever its affinity allows: if it runs on an SMT sibling outside the set it
	# competes for a reserved core (checked in END from the CPU it last ran on).
	if (vm && st == "R") {
		m++
		rwho[m] = tg
		rstat[m] = substr(path, 1, length(path) - 6) "stat"
	}
}
function inside(pid,   d) {
	for (d = 0; (pid in par) && d < 64; d++) {
		if (pid == scan) return 1
		pid = par[pid]
	}
	return 0
}
BEGIN {
	k = split(set, ids, " ")
	for (i = 1; i <= k; i++) want[ids[i]] = 1
	getline self < "/proc/self/stat"
	split(self, sf, " ")
	scan = sf[4]
}
{ readthread($0) }
# cgroupof: 1 when the process is in the container of this run, 0 when it is not, -1 when it is gone;
# a process that fails every read while it is still there fails the scan (bad).
function cgroupof(pid,   cg, line, rc, mine, try) {
	cg = "/proc/" pid "/cgroup"
	for (try = 1; try <= 3; try++) {
		mine = 0
		while ((rc = (getline line < cg)) > 0) if (cid != "" && index(line, cid)) mine = 1
		close(cg)
		if (rc >= 0) return mine
		if (!exists(cg)) return -1
	}
	bad = 1
	return -1
}
# lastcpu: the CPU a thread is running on, when it is still running (field 39 of its stat file, read after
# the last closing parenthesis of the command name), else empty. A thread that is gone is skipped; one
# that still exists but cannot be read fails the scan (bad). No CPU number leaves this function but as a
# comparison against the sibling set.
function lastcpu(sf,   line, rc, rec, s, f, try, nf) {
	for (try = 1; try <= 3; try++) {
		# The whole record: the command name may hold spaces and newlines, so every line is read before
		# the fields after the final closing parenthesis are taken. A record that does not end in the
		# expected fields (the state a single letter, the CPU a number, and one numeric field after the CPU so
		# that the CPU token is delimited and cannot be a truncated number) is incomplete.
		rec = ""
		nf = 0
		while ((rc = (getline line < sf)) > 0) rec = (nf++ ? rec "\n" : "") line
		close(sf)
		if (rc >= 0 && nf > 0) {
			s = rec
			while (index(s, ")")) s = substr(s, index(s, ")") + 1)
			split(s, f, " ")
			if (f[1] ~ /^[A-Za-z]$/ && f[37] ~ /^[0-9]+$/ && f[38] ~ /^-?[0-9]+$/) break
		}
		if (!exists(sf)) return ""
		if (try == 3) {
			bad = 1
			return ""
		}
	}
	if (f[1] != "R") return ""
	return f[37]
}
END {
	for (j = 1; j <= n; j++) {
		if (inside(who[j])) continue
		cgm = cgroupof(who[j])
		if (cgm != 0) continue
		# Affinity alone is not competing work: the task counts only where its effective cgroup cpuset
		# (and its affinity) still contain a reserved CPU. A cgroup that cannot be read fails the scan.
		eff = effcpus(cgf[j])
		if (eff == "GONE") continue
		if (eff != "ALL" && !meets(alw[j], eff)) continue
		a++
		if (state[j] == "R") r++
	}
	ns = split(sibs, sl, " ")
	for (i = 1; i <= ns; i++) sibset[sl[i]] = 1
	for (j = 1; j <= m && ns > 0; j++) {
		if (inside(rwho[j])) continue
		if (cgroupof(rwho[j]) != 0) continue
		cpu = lastcpu(rstat[j])
		if (cpu != "" && (cpu in sibset)) sb++
	}
	if (bad) exit 2
	print a + 0, r + 0, seen + 0, sb + 0
}' "$T/f/tasks") || return 1
	printf '%s\n' "$_ovo"
}
# bench_backing DIR: BACK_FS (the filesystem word DIR is on: ext4, xfs or other), BACK_OK (1 when BACK_FS
# is a benchmark filesystem, ext4 or XFS) and BACK_NVME (1 when its block device is NVMe) from df and
# backing_nvme; status 1 when any fact cannot be read (the device is never named).
bench_backing() {
	_bso=$(timeout 30 df --output=source,fstype -- "$1" 2>/dev/null) || return 1
	set -- $(printf '%s\n' "$_bso" | awk 'NR == 2 { print $1, $2 }')
	[ $# = 2 ] || return 1
	BACK_FS=other
	BACK_OK=0
	case $2 in ext4 | xfs) BACK_FS=$2 BACK_OK=1 ;; esac
	backing_nvme /sys "$1" || return 1
	BACK_NVME=$CFG_NVME
}
# bench_config: CFG_FS (the /config backing filesystem word: ext4, xfs or other), CFG_NVME and CFG_EMPTY
# (1 or 0) of the read-only /config's backing directory, read now. A failed read of any kind (df, or the
# directory scan: an unreadable directory is not an empty one), or a filesystem that is neither ext4 nor
# XFS, fails the call before measurement; empty is only a scan that succeeded and found no entry. Names
# found are never kept.
bench_config() {
	bench_backing "$BENCHCFG" || return 1
	CFG_FS=$BACK_FS
	CFG_NVME=$BACK_NVME
	[ "$BACK_OK" = 1 ] || return 1
	_cid=$(timeout 30 stat -c '%d:%i' -- "$BENCHCFG" 2>/dev/null) || return 1
	[ "$_cid" = "$CFG_ID" ] || return 1
	_first=$(timeout 30 find "$BENCHCFG" -mindepth 1 -maxdepth 1 -print -quit 2>/dev/null) || return 1
	CFG_EMPTY=0
	[ -n "$_first" ] || CFG_EMPTY=1
	_first=
}
# bench_data_obs EMPTY: DFS (the work directory's filesystem word: ext4, xfs or other), DNV (whether its
# device is NVMe), DWR (owned by the child's uid with owner write, no group or other write, and the
# read-write bind), DBND (the work directory under the same run identity and, while EMPTY is 1, empty) and
# DMNT (the mount) for one observation; status 1 when any value cannot be read or the filesystem is neither
# ext4 nor XFS. A value that reads but does not hold is recorded as 0 and the judge decides, as for every
# host fact.
bench_data_obs() {
	_eq=${1:-0}
	bench_backing "$BENCHRUN/work" || return 1
	DFS=$BACK_FS
	DNV=$BACK_NVME
	[ "$BACK_OK" = 1 ] || return 1
	_ws=$(timeout 30 stat -c '%u %a %d:%i' -- "$BENCHRUN/work" 2>/dev/null) || return 1
	set -- $_ws
	[ $# = 3 ] || return 1
	case $2 in '' | *[!0-7]*) return 1 ;; esac
	DWR=0
	if [ "$1" = 1000 ] && [ $((0$2 & 0200)) -ne 0 ] && [ $((0$2 & 0022)) -eq 0 ]; then
		DWR=1
	fi
	DBND=0
	if [ "$3" = "$WORK_DEVID" ] && [ "$(timeout 30 stat -c '%d:%i' -- "$BENCHRUN" 2>/dev/null)" = "$RUN_DEVID" ]; then
		DBND=1
	fi
	bench_mounts || return 1
	[ "$MOUNT_RW" = 1 ] || DWR=0
	if [ "$DBND" = 1 ] && [ "$_eq" = 1 ]; then
		_we=$(timeout 30 find "$BENCHRUN/work" -mindepth 1 -maxdepth 1 -print -quit 2>/dev/null) || return 1
		[ -z "$_we" ] || DBND=0
	fi
}
# bench_obs FILE [EMPTY]: appends one observation line `unix allowed runnable load_milli mem_available_kib
# swap_in swap_out filesystem nvme empty`, then in data mode its five data facts; status 1 (nothing appended)
# when any value cannot be read. The /config and data facts are read again at every observation, so a
# change between stages reaches the record. EMPTY=1 (the before stage) also requires an empty data root.
bench_obs() {
	_of=$1
	_oeq=${2:-0}
	_u=$(date -u +%s) || return 1
	_la=$(awk '{ printf "%d", $1 * 1000 }' /proc/loadavg 2>/dev/null) || return 1
	_mem=$(awk '/^MemAvailable:/ { print $2 }' /proc/meminfo 2>/dev/null) || return 1
	_sw=$(awk '/^pswpin / { i = $2 } /^pswpout / { o = $2 } END { print i + 0, o + 0 }' /proc/vmstat 2>/dev/null) || return 1
	# The mount table is proven again around every scan: a mask mounted after startup (a task hidden behind
	# a tmpfs, or a restricted /proc) is rejected (status 4), never covered by the task-count bound.
	bench_proc_ok || return 4
	_ov=$(bench_overlap) || return 1
	bench_proc_ok || return 4
	bench_config || return 1
	set -- $_ov
	{ [ $# = 4 ] && [ "$3" -ge 1 ]; } 2>/dev/null || return 1
	# The view must be the whole machine's: the tasks read are (nearly) all the kernel counts. The kernel's own
	# total is read after the scan, so on a machine whose threads churn the running/total snapshot and the scan
	# can land on either side of a burst and read short of the bound with no task masked; a shortfall is
	# therefore re-read, never assumed. A masked view reads short at every reading, so one observation takes at
	# most three: the scan and total above, then two more of both.
	_try=0
	while :; do
		_tot=$(awk '{ split($4, a, "/"); print a[2] }' /proc/loadavg 2>/dev/null) || return 1
		case $_tot in '' | *[!0-9]*) return 1 ;; esac
		[ $(($3 * 100)) -ge $((_tot * 97)) ] && break
		[ "$_try" -lt 2 ] || return 1
		_try=$((_try + 1))
		bench_proc_ok || return 4
		_ov=$(bench_overlap) || return 1
		bench_proc_ok || return 4
		set -- $_ov
		{ [ $# = 4 ] && [ "$3" -ge 1 ]; } 2>/dev/null || return 1
	done
	# The whole line is composed before anything is appended: a fact that cannot be read leaves the
	# observation file exactly as it was.
	_line="$_u $1 $2 $_la $_mem $_sw $CFG_FS $CFG_NVME $CFG_EMPTY $4"
	if [ "$HAVE_BENCHDATA" = 1 ]; then
		bench_data_obs "$_oeq" || return 1
		_line="$_line $DFS $DNV $DWR $DBND $DMNT"
	fi
	printf '%s\n' "$_line" >>"$_of"
}
# bench_static: the container id and its limits as Docker created it, set once as CID and FACT_*.
bench_static() {
	CID=$(timeout 30 docker inspect --format '{{.Id}}' "$CTR" 2>/dev/null) || return 1
	case $CID in '' | *[!0-9a-f]*) return 1 ;; esac
	[ "${#CID}" = 64 ] || return 1
	_lim=$(timeout 30 docker inspect --format '{{.HostConfig.Memory}} {{.HostConfig.MemorySwap}} {{.HostConfig.CpusetCpus}}' "$CTR" 2>/dev/null) || return 1
	set -- $_lim
	[ $# = 3 ] || return 1
	case $1$2 in '' | *[!0-9]*) return 1 ;; esac
	FACT_MEM=$1
	FACT_SWAP=$2
	FACT_CPUSET=false
	[ "$3" != "$BENCHCPU" ] || FACT_CPUSET=true
	FACT_RO=true
	CFG_ID=$(timeout 30 stat -c '%d:%i' -- "$BENCHCFG" 2>/dev/null) || return 1
	case $CFG_ID in [0-9]*:[0-9]*) ;; *) return 1 ;; esac
	# The reserved set's SMT siblings outside it, for the sibling-runnable count of every observation.
	bench_topology
}
# bench_emit STAGE OBSFILE OUTFILE: the stage record from its observation lines (every field a
# nonnegative integer, else status 1) and the topology read now. The /config facts, and in data mode its
# data facts, are true only when true at every observation of the stage. Without --bench-data the record is
# the V1 object; with it the record is V2, the same host fields unchanged under their own key, the five
# identity keys on the envelope and the data block beside them.
bench_emit() {
	bench_topology || return 1
	awk -v stage="$1" -v run="${BENCHRUN##*/}" -v image="$IMG" -v inputs="$INPUTS" -v data="$HAVE_BENCHDATA" \
		-v cpus="$TOPO_CPUS" -v cores="$TOPO_CORES" -v tpc="$TOPO_TPC" -v sibout="$TOPO_SIBOUT" -v numa="$TOPO_NUMA" \
		-v memlim="$FACT_MEM" -v swaplim="$FACT_SWAP" -v cpusetok="$FACT_CPUSET" -v ro="$FACT_RO" '
BEGIN { nf = data ? 16 : 11 }
NF != nf { bad = 1; next }
{
	for (i = 1; i <= nf; i++) if (i != 8 && (!data || i != 12) && $i !~ /^[0-9]+$/) bad = 1
	if ($8 !~ /^(ext4|xfs|other)$/) bad = 1
	if (data && $12 !~ /^(ext4|xfs|other)$/) bad = 1
	n++
	if (n == 1) { first = $1; mem = $5; si = $6; so = $7; fs = $8; nv = 1; em = 1; dfs = $12; dnve = 1; dwre = 1; dbde = 1; dmne = 1 }
	last = $1
	ei = $6
	eo = $7
	if ($2 + 0 > am) am = $2 + 0
	if ($3 + 0 > rm) rm = $3 + 0
	if ($4 + 0 > lm) lm = $4 + 0
	if ($5 + 0 < mem + 0) mem = $5
	if ($8 != fs) fs = "other"
	if ($9 != 1) nv = 0
	if ($10 != 1) em = 0
	if ($11 + 0 > sm) sm = $11 + 0
	if (data) {
		if ($12 != dfs) dfs = "other"
		if ($13 != 1) dnve = 0
		if ($14 != 1) dwre = 0
		if ($15 != 1) dbde = 0
		if ($16 != 1) dmne = 0
	}
}
END {
	if (bad || n < 1) exit 1
	if (data)
		printf "{\"schema\":2,\"stage\":\"%s\",\"run\":\"%s\",\"image\":\"%s\",\"inputs\":\"%s\",\"host\":{", stage, run, image, inputs
	else
		printf "{\"schema\":1,\"stage\":\"%s\",\"run\":\"%s\",\"image\":\"%s\",\"inputs\":\"%s\",", stage, run, image, inputs
	printf "\"start_unix\":%.0f,\"end_unix\":%.0f,\"observations\":%.0f,", first, last, n
	printf "\"cpus\":%.0f,\"cores\":%.0f,\"threads_per_core_max\":%.0f,\"siblings_outside_set\":%.0f,\"numa_nodes\":%.0f,", cpus, cores, tpc, sibout, numa
	printf "\"foreign_allowed_max\":%.0f,\"foreign_runnable_max\":%.0f,\"sibling_runnable_max\":%.0f,\"load1_milli_max\":%.0f,\"mem_available_kib_min\":%.0f,", am, rm, sm, lm, mem
	printf "\"swap_in_start\":%.0f,\"swap_in_end\":%.0f,\"swap_out_start\":%.0f,\"swap_out_end\":%.0f,", si, ei, so, eo
	printf "\"mem_limit_bytes\":%.0f,\"memswap_limit_bytes\":%.0f,\"cpuset_matches\":%s,", memlim, swaplim, cpusetok
	printf "\"config_fs\":\"%s\",\"config_nvme\":%s,\"config_empty\":%s,\"config_readonly\":%s", fs, nv ? "true" : "false", em ? "true" : "false", ro
	if (data)
		printf "},\"data\":{\"data_fs\":\"%s\",\"data_nvme\":%s,\"data_writable\":%s,\"data_bound\":%s,\"data_mounted\":%s}}\n", dfs, dnve ? "true" : "false", dwre ? "true" : "false", dbde ? "true" : "false", dmne ? "true" : "false"
	else
		printf "}\n"
}' "$2" >"$3"
}
# bench_prove: the container runs on this machine's kernel and has the observed directory mounted. The
# daemon reports the pid of the container's init in its own pid namespace; that pid must be a task of
# this /proc whose cgroup names this very container (a forwarded socket to another machine, even one
# with the same kernel release, cannot show that), and its /config must be the device and inode the
# facts are read from. Status 3: not this machine's container; 1: anything else unproven. The
# container must be seen running; one that already exited cannot be observed.
bench_prove() {
	_try=0
	while :; do
		_st=$(timeout 30 docker inspect --format '{{.State.Pid}} {{.State.Running}}' "$CTR" 2>/dev/null) || return 1
		set -- $_st
		[ $# = 2 ] || return 1
		if [ "$2" = true ] && [ "$1" -gt 0 ] 2>/dev/null; then break; fi
		_try=$((_try + 1))
		[ "$_try" -lt 50 ] || return 1
		sleep 0.2
	done
	_pid=$1
	grep -Fq -- "$CID" "/proc/$_pid/cgroup" 2>/dev/null || return 3
	_mid=$(timeout 30 stat -c '%d:%i' -- "/proc/$_pid/root/config" 2>/dev/null) || return 1
	[ "$_mid" = "$CFG_ID" ] || return 1
}
# bench_sampler: one observation per second into during.obs until the runner leaves the stop file. An
# observation that fails ends it with status 1 (3 when the container is not this machine's), which the
# runner checks before anything is published.
bench_sampler() {
	bench_prove || exit $?
	while [ ! -e "$T/f/stop" ]; do
		bench_obs "$T/f/during.obs" || exit $?
		sleep 1
	done
}
if [ "$HAVE_BENCH" = 1 ]; then
	mkdir -m 0700 -- "$T/f" || die bench_evidence_failed
	bench_must bench_static
	bench_must bench_obs "$T/f/before.obs" 1
	bench_must bench_emit before "$T/f/before.obs" "$T/f/before.json"
fi

# --- run -------------------------------------------------------------------------
# Output comes only from the attached stream. The left side of the pipeline
# reports its status through a fresh scratch file (POSIX sh keeps only the last
# command's status); a missing or non-numeric value is a failure.
if [ "$HAVE_BENCH" = 1 ]; then
	bench_sampler >/dev/null 2>&1 </dev/null &
	SAMPLER=$!
fi
rm -f -- "$T/st"
{ timeout --foreground --kill-after=5s "${TIMEOUT}s" docker start -a "$CTR" 2>&1 </dev/null; printf '%s' "$?" >"$T/st"; } | "$R" -cap "$CAP"
RS=$?
if [ "$RS" != 0 ]; then
	[ "$RS" != 3 ] || die output_overflow
	die output_failed
fi
SS=$(cat "$T/st" 2>/dev/null)
case $SS in '' | *[!0-9]*) die status_unknown ;; esac
STATE=$(timeout 30 docker inspect --format '{{.State.Status}} {{.State.ExitCode}}' "$CTR" 2>/dev/null)
case $STATE in
'exited '[0-9]*)
	CHILD=${STATE#exited }
	# `docker start -a` returns the child's code, so the two agree; only a fired
	# host deadline (124, or 137 after timeout's KILL) may differ and then stays 124.
	if [ "$SS" != "$CHILD" ]; then
		case $SS in 124 | 137) CHILD=124 ;; *) die status_unknown ;; esac
	fi
	;;
'created 126' | 'created 127')
	# The child never started: Docker's own result, with a failed `docker start`.
	[ "$SS" != 0 ] || die status_unknown
	CHILD=${STATE#created }
	;;
'running '*)
	# Only a host deadline may leave the container running.
	case $SS in 124 | 137) ;; *) die status_unknown ;; esac
	timeout 30 docker kill "$CTR" >/dev/null 2>&1 || die container_stop_failed
	timeout 60 docker wait "$CTR" >/dev/null 2>&1 || die container_stop_failed
	CHILD=124
	;;
*) die status_unknown ;;
esac
case $CHILD in '' | *[!0-9]*) die status_unknown ;; esac
if [ "$HAVE_BENCH" = 1 ]; then
	# The stopped container is observed last: the sampler ends after its current observation.
	: >"$T/f/stop" || die bench_evidence_failed
	wait "$SAMPLER" 2>/dev/null
	_sst=$?
	SAMPLER=
	# A sampler that failed after any observation must not be covered by the observations made now.
	[ "$_sst" != 3 ] || die 'bench_invalid side=daemon'
	[ "$_sst" = 4 ] && die 'bench_invalid side=proc'
	[ "$_sst" = 0 ] || die bench_evidence_failed
	bench_must bench_obs "$T/f/during.obs"
	bench_must bench_emit during "$T/f/during.obs" "$T/f/during.json"
	bench_must bench_obs "$T/f/after.obs"
	bench_must bench_emit after "$T/f/after.obs" "$T/f/after.json"
fi

# --- export (stopped container), checked removal, publish ----------------------------
# cp_out SRC: `docker cp` of one path of the stopped child as a tar stream on stdout. Its
# diagnostics are first bounded (4097 raw bytes, so the redactor's own 4096 cap still overflows
# closed) and classified by sed, which stores only the fixed token `M` when a line says the file
# was not found (the raw line is never written anywhere), and only then pass through the capped
# redactor into $T/cperr, so a selected secret value cannot mask the classifier. Raw bytes only
# ever travel through pipes; the statuses are in $T/sp (docker), hs, cs and ds.
# export_one NAME: at most STREAMCAP bytes of that stream are read, and GNU tar hands each regular
# entry's payload to EXPORT_CMD, which (inside tar's child shell, reading DR_* from the
# environment) accepts exactly one entry of the type f, the constant name and a raw size of at
# most CAP bytes and pipes it through the redactor straight into the private staging file DR_OUT.
# tar's verbose listing (its stdout; the callback's output never reaches it) is read through a
# pipe of at most 4096 bytes by list_one, which stores only the fixed token one|bad, never a name:
# anything tar lists beyond that one entry (links, directories, devices, more files) fails the check.
EXPORT_CMD='case $TAR_FILETYPE:$TAR_REALNAME in "f:$DR_NAME") ;; *) echo type >"$DR_WHY"; exit 1 ;; esac
case $TAR_SIZE in "" | *[!0-9]*) echo type >"$DR_WHY"; exit 1 ;; esac
[ "$TAR_SIZE" -le "$DR_CAP" ] || { echo size >"$DR_WHY"; exit 1; }
echo ran >>"$DR_SEEN"
"$DR_R" -cap "$DR_CAP" >"$DR_OUT"; _s=$?
echo "$_s" >"$DR_RS"
exit "$_s"'
# list_one NAME: stdin is tar's listing; the token is `one` only for exactly the line NAME.
list_one() {
	_lt=bad
	if IFS= read -r _l1 && [ "$_l1" = "$1" ] && ! IFS= read -r _l2 && [ -z "$_l2" ]; then _lt=one; fi
	printf '%s' "$_lt" >"$T/lst"
}
cp_out() {
	rm -f -- "$T/sp" "$T/hs" "$T/cs" "$T/ds" "$T/mis" "$T/cperr"
	{
		{ timeout 60 docker cp "$CTR:$1" - 2>&1 1>&3 3>&- </dev/null; printf '%s' "$?" >"$T/sp"; } |
			{ head -c 4097; printf '%s' "$?" >"$T/hs"; } |
			{ sed -e '/Could not find the file/{' -e h -e 's/.*/M/' -e "w $T/mis" -e g -e '}'; printf '%s' "$?" >"$T/cs"; } |
			"$R" -cap 4096 >"$T/cperr" 3>&-
		printf '%s' "$?" >"$T/ds"
	} 3>&1
}
export_one() {
	rm -f -- "$T/ts" "$T/rs" "$T/why" "$T/seen" "$T/lst" "$T/p/$1"
	cp_out "/src/$WRITE/$1" | head -c "$STREAMCAP" |
		{ env DR_NAME="$1" DR_CAP="$WCAP" DR_R="$R" DR_WHY="$T/why" DR_SEEN="$T/seen" DR_RS="$T/rs" DR_OUT="$T/p/$1" \
			tar -C "$T/x" -x -f - -v --to-command="$EXPORT_CMD" 2>/dev/null; printf '%s' "$?" >"$T/ts"; } |
		head -c 4096 | list_one "$1"
	case $(cat "$T/why" 2>/dev/null) in
	'') ;;
	size) die draft_overflow ;;
	*) die export_invalid ;;
	esac
	_sp=$(cat "$T/sp" 2>/dev/null)
	case $_sp in '' | *[!0-9]*) die export_failed ;; esac
	[ "$(cat "$T/ds" 2>/dev/null)" = 0 ] && [ "$(cat "$T/hs" 2>/dev/null)" = 0 ] && [ "$(cat "$T/cs" 2>/dev/null)" = 0 ] || die export_failed
	if [ "$_sp" != 0 ]; then
		# A draft the child never wrote is not an error; every other failure is.
		if [ "$WRITE" = docs/spikes/issue-drafts ] && [ "$(cat "$T/mis" 2>/dev/null)" = M ] && [ ! -e "$T/seen" ]; then
			rm -f -- "$T/p/$1"
			return 0
		fi
		die export_failed
	fi
	[ "$(cat "$T/seen" 2>/dev/null)" = ran ] || die export_invalid
	case $(cat "$T/rs" 2>/dev/null) in
	3) die draft_overflow ;;
	0) ;;
	*) die export_failed ;;
	esac
	[ "$(cat "$T/lst" 2>/dev/null)" = one ] || die export_invalid
	[ "$(cat "$T/ts" 2>/dev/null)" = 0 ] || die export_failed
}
if [ "$HAVE_WRITE" = 1 ] && { [ "$WRITE" = docs/spikes/issue-drafts ] || [ "$CHILD" = 0 ]; }; then
	mkdir -m 0700 -- "$T/x" "$T/p" || die export_failed
	for _n in $DRAFTS; do
		export_one "$_n"
	done
fi

# --- capture (stopped container, child exit 0 only) --------------------------------------------
# Each fixed file streams straight from the stopped child (cp_out, same classify-then-redact
# diagnostics) through the same pipe-only tar construction into its own fresh validator
# container; the raw payload is never written on the host. CAP_CMD accepts exactly one regular
# entry of the constant name and a raw size of at most CAPMAX bytes, runs the validator on it
# (diagnostics discarded, only its exit word is used) and writes only the validator's canonical
# PNG, through a bounded sink whose own status is kept apart from the validator's.
CAP_CMD='case $TAR_FILETYPE:$TAR_REALNAME in "f:$DR_NAME") ;; *) echo type >"$DR_WHY"; exit 1 ;; esac
case $TAR_SIZE in "" | *[!0-9]*) echo type >"$DR_WHY"; exit 1 ;; esac
[ "$TAR_SIZE" -le "$DR_CAP" ] || { echo size >"$DR_WHY"; exit 1; }
echo ran >>"$DR_SEEN"
{ timeout --foreground --kill-after=5s 30s docker start -a -i "$DR_V" 2>/dev/null; printf "%s" "$?" >"$DR_VS"; } |
	{ head -c "$DR_SINK" >"$DR_OUT"; printf "%s" "$?" >"$DR_KS"; }'
# new_validator NAME: a fresh validator container (fixed non-root identity, memory, pids, no
# network, read-only root, daemon log disabled), created BEFORE any capture byte is read. Its
# effective log driver, identity and memory are read back before it is ever started, and one
# positive log observation line is printed; sets _vc.
VOBS=0
new_validator() {
	_vc=$CTR-v-$1
	PENDING=$_vc
	timeout 60 docker create --name "$_vc" --label "boxset.dev-run=$RUNID" --network none --read-only \
		--cap-drop ALL --security-opt no-new-privileges --log-driver=none --user 1000:1000 --memory 64m --memory-swap 64m --pids-limit 64 -i "$IMG" \
		/usr/local/libexec/boxset/capture-check -name "$1" -challenge "$CHAL" >"$T/create.id" 2>/dev/null || die capture_copy_failed
	_vid=$(cat "$T/create.id" 2>/dev/null) || die capture_copy_failed
	bind_container "$_vid" "$_vc" || die capture_copy_failed
	_ob=$(timeout 30 docker inspect --format '{{.Name}} {{.HostConfig.LogConfig.Type}} {{.Config.User}} {{.HostConfig.Memory}} {{index .Config.Labels "boxset.dev-run"}}' "$_vc" 2>/dev/null)
	case $_ob in "/$_vc none "*) ;; *) die log_driver_invalid ;; esac
	[ "$_ob" = "/$_vc none 1000:1000 67108864 $RUNID" ] || die capture_copy_failed
	printf 'dev-run: validator=%s log=none container=%s\n' "$1" "$_vc" >&2
	VOBS=$((VOBS + 1))
}
# cap_one NAME: stream /capture/NAME.png into its validator; accept only the validator's canonical
# PNG, with the validator's exit word, the sink's own status and the checked validator removal
# all required. The exit word maps to the vetted reason.
cap_one() {
	new_validator "$1"
	rm -f -- "$T/ts" "$T/why" "$T/seen" "$T/lst" "$T/vs" "$T/ks" "$T/cap/$1.png"
	cp_out "/capture/$1.png" | head -c "$CAPSTREAM" |
		{ env DR_NAME="$1.png" DR_CAP="$CAPMAX" DR_SINK=$((CAPMAX + 1)) DR_V="$_vc" DR_WHY="$T/why" DR_SEEN="$T/seen" DR_VS="$T/vs" DR_KS="$T/ks" DR_OUT="$T/cap/$1.png" \
			tar -C "$T/x" -x -f - -v --to-command="$CAP_CMD" 2>/dev/null; printf '%s' "$?" >"$T/ts"; } |
		head -c 4096 | list_one "$1.png"
	case $(cat "$T/why" 2>/dev/null) in
	'') ;;
	size) die capture_oversize ;;
	*) die capture_copy_failed ;;
	esac
	_sp=$(cat "$T/sp" 2>/dev/null)
	case $_sp in '' | *[!0-9]*) die capture_copy_failed ;; esac
	if [ "$_sp" != 0 ]; then
		if [ "$(cat "$T/mis" 2>/dev/null)" = M ] && [ ! -e "$T/seen" ]; then die capture_missing; fi
		die capture_copy_failed
	fi
	[ "$(cat "$T/ds" 2>/dev/null)" = 0 ] && [ "$(cat "$T/hs" 2>/dev/null)" = 0 ] && [ "$(cat "$T/cs" 2>/dev/null)" = 0 ] || die capture_copy_failed
	{ [ "$(cat "$T/seen" 2>/dev/null)" = ran ] && [ "$(cat "$T/lst" 2>/dev/null)" = one ] && [ "$(cat "$T/ts" 2>/dev/null)" = 0 ]; } || die capture_copy_failed
	_vs=$(cat "$T/vs" 2>/dev/null)
	case $_vs in '' | *[!0-9]*) die capture_copy_failed ;; esac
	[ "$(timeout 30 docker inspect --format '{{.State.Status}} {{.State.ExitCode}}' "$_vc" 2>/dev/null)" = "exited $_vs" ] || die capture_copy_failed
	remove_container "$_vid" "$_vc" || { CLEANFAIL=1; die cleanup_failed; }
	case $_vs in
	0) ;;
	10) die capture_missing ;;
	11) die capture_oversize ;;
	12) die capture_not_png ;;
	13) die capture_not_static ;;
	14) die capture_dimensions ;;
	15) die capture_challenge_mismatch ;;
	*) die capture_copy_failed ;;
	esac
	[ "$(cat "$T/ks" 2>/dev/null)" = 0 ] || die capture_copy_failed
	{ [ -f "$T/cap/$1.png" ] && [ ! -L "$T/cap/$1.png" ]; } || die capture_copy_failed
	_sz=$(wc -c <"$T/cap/$1.png" | tr -d ' ')
	case $_sz in '' | *[!0-9]*) die capture_copy_failed ;; esac
	{ [ "$_sz" -ge 20 ] && [ "$_sz" -le "$CAPMAX" ]; } || die capture_copy_failed
	[ "$(head -c 8 "$T/cap/$1.png" | od -An -tx1 | tr -d ' \n')" = 89504e470d0a1a0a ] || die capture_copy_failed
}
CAPOK=0
if [ "$HAVE_CAPTURE" = 1 ] && [ "$CHILD" = 0 ]; then
	mkdir -m 0700 -- "$T/cap" || die capture_copy_failed
	[ -d "$T/x" ] || mkdir -m 0700 -- "$T/x" || die capture_copy_failed
	for _n in $CAPNAMES; do
		cap_one "$_n"
	done
	[ "$VOBS" = 4 ] || die log_driver_invalid
fi
remove_container "$CID" "$CTR" || { CLEANFAIL=1; [ "$CHILD" != 0 ] && die cleanup_failed "$CHILD"; die cleanup_failed; }
if [ "${HAVE_BENCHDATA:-0}" = 1 ]; then
	# The recorded cleanup runs only now, after the measured container's checked removal, and only while
	# both identities still match; nothing is published unless it completed. A failure only marks it and
	# stops: the exit path prints the one reason word and the residual, so nothing is printed twice.
	bench_data_cleanup || { DATACLEANFAIL=1; exit 125; }
	DATACLEANED=1
	if [ ! -f "$T/p/samples.jsonl" ]; then
		# Nothing will be published: the run directory this invocation created goes with the data directory
		# it held.
		{ [ "$(timeout 30 stat -c '%d:%i' -- "$BENCHRUN" 2>/dev/null)" = "$RUN_DEVID" ] && rmdir -- "$BENCHRUN"; } ||
			{ DATACLEANFAIL=1; exit 125; }
		DATADONE=1
	fi
fi
if [ "$HAVE_CAPTURE" = 1 ] && [ "$CHILD" = 0 ]; then
	printf 'boxset-capture 1\nchallenge %s\n' "$CHAL" >"$T/cap/capture.manifest" || die capture_copy_failed
	CAPBYTES=0
	for _n in $CAPNAMES; do
		_h=$(sha256sum <"$T/cap/$_n.png") || die capture_copy_failed
		_h=${_h%% *}
		_b=$(wc -c <"$T/cap/$_n.png" | tr -d ' ')
		printf '%s %s %s.png\n' "$_h" "$_b" "$_n" >>"$T/cap/capture.manifest" || die capture_copy_failed
		CAPBYTES=$((CAPBYTES + _b))
	done
	CAPOK=1
fi
if [ "$HAVE_BENCH" = 1 ] && [ -f "$T/p/samples.jsonl" ]; then
	if [ "$HAVE_BENCHDATA" = 1 ]; then
		# The run directory was created before the child started and holds nothing but the files published
		# now: it is never reused, replaced or created a second time.
		{ [ "$(timeout 30 stat -c '%d:%i' -- "$BENCHRUN" 2>/dev/null)" = "$RUN_DEVID" ] && [ ! -L "$BENCHRUN" ]; } || die 'bench_invalid side=samples'
		_dir=$(timeout 30 find "$BENCHRUN" -mindepth 1 -maxdepth 1 -print -quit 2>/dev/null) || die 'bench_invalid side=samples'
		[ -z "$_dir" ] || die 'bench_invalid side=samples'
	else
		# The fresh run directory is created now and never reused: an existing one stops the publication.
		write_chain_ok || die write_root_invalid
		mkdir -p -- "${BENCHRUN%/*}" || die publish_failed
		mkdir -- "$BENCHRUN" 2>/dev/null || die 'bench_invalid side=samples'
	fi
fi
if [ "$HAVE_WRITE" = 1 ]; then
	for _n in $DRAFTS; do
		[ -f "$T/p/$_n" ] || continue
		write_chain_ok || die write_root_invalid
		mkdir -p -- "$WRITE" || die publish_failed
		_final=$WRITE/$_n
		if [ -L "$_final" ] || { [ -e "$_final" ] && [ ! -f "$_final" ]; }; then die publish_failed; fi
		_part=$(mktemp "$WRITE/.$_n.part.XXXXXX") || die publish_failed
		if ! { [ -f "$_part" ] && [ ! -L "$_part" ] && [ -O "$_part" ]; }; then die publish_failed; fi
		if ! { cat "$T/p/$_n" >"$_part" && chmod 0644 "$_part" && mv -f -T -- "$_part" "$_final"; }; then
			rm -f -- "$_part"
			die publish_failed
		fi
	done
fi
if [ "$HAVE_BENCH" = 1 ] && [ -f "$T/p/samples.jsonl" ]; then
	# The host's own records, beside (never inside) the exported samples directory.
	for _n in before during after; do
		_sz=$(wc -c <"$T/f/$_n.json" | tr -d ' ')
		case $_sz in '' | *[!0-9]*) die bench_evidence_failed ;; esac
		[ "$_sz" -le 65536 ] || die bench_evidence_failed
		_part=$(mktemp "$BENCHRUN/.$_n.part.XXXXXX") || die publish_failed
		if ! { [ -f "$_part" ] && [ ! -L "$_part" ] && [ -O "$_part" ]; }; then die publish_failed; fi
		if ! { cat "$T/f/$_n.json" >"$_part" && chmod 0644 "$_part" && mv -f -T -- "$_part" "$BENCHRUN/$_n.json"; }; then
			rm -f -- "$_part"
			die publish_failed
		fi
	done
	printf 'DEV-RUN-BENCH-PUBLISHED run=%s files=4\n' "${BENCHRUN##*/}"
	[ "$HAVE_BENCHDATA" = 0 ] || DATADONE=1
fi
if [ "$CAPOK" = 1 ]; then
	chain_ok "${CAPTURE%/*}" || die capture_root_changed
	mkdir -m 0700 -- "$CAPTURE" 2>/dev/null || die capture_destination_exists
	CAPMADE=1
	{ [ -d "$CAPTURE" ] && [ ! -L "$CAPTURE" ] && [ -O "$CAPTURE" ]; } || die capture_root_changed
	for _n in $CAPNAMES capture.manifest; do
		_f=$_n
		[ "$_n" = capture.manifest ] || _f=$_n.png
		{ chmod 0644 "$T/cap/$_f" && mv -f -T -- "$T/cap/$_f" "$CAPTURE/$_f"; } || die publish_failed
	done
	[ "$(find "$CAPTURE" -mindepth 1 -type f -perm 644 | wc -l | tr -d ' ')" = 5 ] && [ "$(find "$CAPTURE" -mindepth 1 | wc -l | tr -d ' ')" = 5 ] || die capture_root_changed
	CAPDONE=1
	printf 'DEV-RUN-CAPTURE-PASS files=4 bytes=%s dir=%s challenge=%s\n' "$CAPBYTES" "$CAPTURE" "$CHAL"
fi
COMPLETE=1
exit "$CHILD"
