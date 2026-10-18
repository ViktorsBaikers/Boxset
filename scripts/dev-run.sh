#!/bin/sh
# Canonical Boxset dev runner (host, repository root only):
#   scripts/dev-run.sh [--ref] [--write docs/spikes/issue-drafts|testdata/parser|docs/spikes/data/bench-<UTC>/samples] [--capture DIR] [--bench CPUSET] [--timeout SECONDS] -- COMMAND [ARG...]
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
# run: docs/spikes/issue-drafts (s5-admission.md, s5-tools.md; at most 64 KiB each)
# or testdata/parser (series.jsonl, movie.jsonl; at most 32 MiB each, the corpus
# reader cap). Only after the container stopped, each name is exported as one tar
# stream that must hold exactly one regular file within its cap, streamed straight
# through the redactor (never spooled raw) and capped, and published by a private
# part plus `mv` only after the owned container was removed. A draft is published
# even when the child exits nonzero; corpus files are published only when the child
# exited 0 and every name was exported (a missing, oversize, linked or non-regular
# name publishes nothing). The archive's member names are checked in a pipe and only
# fixed result tokens are kept; copy diagnostics pass through the capped redactor
# before they are kept. A later rename failure may leave earlier names published.
# There is no rollback and no durability claim.
#
# --ref delivers reference parser C# text, only as Git objects. The two reference repositories are
# located only by the host values BOXSET_REF_SERIES and BOXSET_REF_MOVIE (series first): each must
# be an absolute path to an existing directory free of control characters (unset or empty is
# ref_unavailable, anything else wrong is ref_invalid). The runner's REFPINS table pins each side to
# one commit and REFAREAS maps the three upstream directories of that commit to the neutral container
# areas /ref/<side>/{tests,parser,qualities}; no checkout directory is ever walked, copied or
# mounted, and the checkout's HEAD, index and working tree are never read. Before any Docker call
# each pin is checked by a bounded, environment-cleared Git (else ref_commit_invalid). After the
# container is created and before it starts, one archive per side is piped from the private
# producer mode of the host redactor into `docker cp -` (producer and receiver statuses are kept
# apart): a listing that breaks a source rule is ref_invalid, any other failure ref_copy_failed, and
# /ref/<side> of the unstarted container is read back through the producer's own 18 MiB archive
# cap (only files and directories under the three areas, owner 0/0, modes 0444/0555). A --ref
# failure prints only
# `dev-run: <reason> side=<side>`: never a root value or Git text. Roots never enter the image, a
# log, a mount or the child; the child gets only the two checked pins as BOXSET_REF_SERIES_COMMIT
# and BOXSET_REF_MOVIE_COMMIT, and the created container has no mount (else mounts_invalid). The
# reference is text for extraction only: it is never executed. A corpus export (--write
# testdata/parser) is published only when the raw file of the stopped container hashes as the
# sanitized staged bytes (else export_changed).
#
# --bench CPUSET (CPUSET = the owner's reserved cores, digits, commas and ranges) runs the child as a
# reference-profile benchmark: --cpuset-cpus CPUSET --memory 4g --memory-swap 4g and the host directory
# BOXSET_BENCH_CONFIG (an absolute path, read from the environment at run time and never written
# anywhere) mounted read-only at /config. It is valid only with --write of a fresh
# docs/spikes/data/bench-<UTC YYYYMMDDTHHMMSSZ>/samples root and never with --capture; every other
# form is rejected before Docker with `dev-run: bench_invalid side=<cpuset|write|samples|config|daemon>`
# or `dev-run: bench_unavailable side=config` (variable unset or empty), exit 125; the daemon must be
# a unix socket of this machine that runs this machine's kernel (the host /proc and /sys are what is
# observed), and no endpoint is ever printed. The container itself is proven to be this machine's: the pid
# the daemon reports must be a task of this /proc whose cgroup names the container, and its /config must be
# the device and inode every observation reads (a renamed and recreated path fails). A /proc mounted with
# hidepid, a second /proc mount or any mount over a task entry (bench_invalid side=proc), whoever the caller, is rejected before Docker. The host, never the
# child, observes the run: RUN/before.json (after the container exists, before it starts),
# RUN/during.json (every observation while it runs, once a second) and RUN/after.json (after it
# stopped) are aggregates of fixed numbers, enums and booleans only (schema: bench/parser/eligibility.go;
# no task listing, command line, environment, CPU list, path or host name). They are written from the
# host and published beside RUN/samples, never mounted into or writable by the child. Each
# observation reads the threads of this machine and the /config filesystem and entries again; it
# is complete or it fails: a /proc walk cut short, an unreadable live task, an unscannable /config
# (an unreadable directory is not an empty one) or a sampler that failed after any observation is
# bench_evidence_failed, and nothing is published (a task that vanished meanwhile is not a failure).
# The mount of /config is compared in memory; the owner value is never written to the scratch. After a
# child exit 0 only the stopped container's regular samples.jsonl is exported (the corpus rules: one
# regular file, at most 32 MiB, through the redactor) together with the three records, into the
# fresh RUN directory; a nonzero child, an observation that could not be read (bench_evidence_failed)
# or any export failure publishes nothing. Observation is not a reservation: the owner reserves the
# cores, and `bench/parser -judge RUN` (the next invocation, from a refreshed image) decides.
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
HAVE_REF=0
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
SAMPLER=
CID=

die() {
	printf 'dev-run: %s\n' "$1" >&2
	exit "${2:-125}"
}

usage() {
	printf '%s\n' 'usage: scripts/dev-run.sh [--ref] [--write docs/spikes/issue-drafts|testdata/parser|docs/spikes/data/bench-<UTC>/samples] [--capture DIR] [--bench CPUSET] [--timeout SECONDS] -- COMMAND [ARG...]' >&2
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

# Removes only what this invocation created: its labelled container (the label
# carries the random run id) and its scratch directory, if still ours.
cleanup() {
	[ -z "$SAMPLER" ] || : >"$T/f/stop" 2>/dev/null
	if [ -n "$RUNID" ]; then
		_ids=$(timeout 30 docker ps -aq --filter "label=boxset.dev-run=$RUNID" 2>/dev/null)
		[ -z "$_ids" ] || timeout 60 docker rm -f $_ids >/dev/null 2>&1
	fi
	# A capture directory this run created and did not finish publishing.
	if [ "$CAPMADE" = 1 ] && [ "$CAPDONE" = 0 ] && [ -d "$CAPTURE" ] && [ ! -L "$CAPTURE" ] && [ -O "$CAPTURE" ]; then
		rm -rf "$CAPTURE"
	fi
	if [ -n "$OWN" ] && [ -d "$T" ] && [ ! -L "$T" ] && [ -O "$T" ]; then
		rm -rf "$T"
	fi
	return 0
}
trap cleanup EXIT
trap 'printf "dev-run: interrupted\n" >&2; exit 125' HUP INT TERM

# --- arguments ---------------------------------------------------------------
while [ $# -gt 0 ]; do
	case $1 in
	--) break ;;
	--ref)
		[ "$HAVE_REF" = 0 ] || usage
		HAVE_REF=1
		;;
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
	BENCHCFG=${BOXSET_BENCH_CONFIG:-}
	[ -n "$BENCHCFG" ] || die 'bench_unavailable side=config'
	case $BENCHCFG in /*) ;; *) die 'bench_invalid side=config' ;; esac
	case $BENCHCFG in *[!A-Za-z0-9_./+@=-]* | */ | *//* | */./* | */../* | */. | */..) die 'bench_invalid side=config' ;; esac
	{ [ -d "$BENCHCFG" ] && [ ! -L "$BENCHCFG" ]; } || die 'bench_invalid side=config'
fi
case $TIMEOUT in
'' | 0* | *[!0-9]*) die timeout_invalid ;;
esac
{ [ "${#TIMEOUT}" -le 3 ] && [ "$TIMEOUT" -le 600 ]; } || die timeout_invalid
if [ "$HAVE_WRITE" = 1 ]; then
	case $WRITE in
	docs/spikes/issue-drafts) ;;
	testdata/parser)
		DRAFTS='series.jsonl movie.jsonl'
		WRITE_CHAIN='testdata testdata/parser'
		WCAP=33554432
		STREAMCAP=33751040
		;;
	docs/spikes/data/bench-*/samples)
		[ "$HAVE_BENCH" = 1 ] || die write_path_invalid
		DRAFTS='samples.jsonl'
		WRITE_CHAIN='docs docs/spikes docs/spikes/data'
		WCAP=33554432
		STREAMCAP=33751040
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

# --ref: reference text is read only as Git objects of the two reference repositories at their
# pinned commits, never from a checkout directory. REFPINS is the side table and REFAREAS the one
# reversible map between the neutral container areas and the upstream directories.
REFPINS='series:57816c149d02f95543973fefb4780c4495f8121c movie:df2faa64c75e6081556d11232dc46523e7264ea1'
REFAREAS='tests:src/NzbDrone.Core.Test/ParserTests parser:src/NzbDrone.Core/Parser qualities:src/NzbDrone.Core/Qualities'
# REFCAP is the producer's archive cap (refMaxStream in deploy/dev/redact/ref_archive.go, 18 MiB: the
# side plus its tar framing); the readback admits the same envelope and no more.
REFCAP=18874368

# ref_root SIDE ROOT: the owner value locates the repository only: set (else ref_unavailable), an
# absolute path to an existing directory free of control characters (else ref_invalid).
ref_root() {
	[ -n "$2" ] || die "ref_unavailable side=$1"
	case $2 in /*) ;; *) die "ref_invalid side=$1" ;; esac
	[ -d "$2" ] || die "ref_invalid side=$1"
	[ "$(printf '%s' "$2" | tr -d '\000-\037\177' | wc -c)" = "$(printf '%s' "$2" | wc -c)" ] || die "ref_invalid side=$1"
}
# ref_pinof SIDE: sets _pin to the side's pinned commit.
ref_pinof() {
	for _e in $REFPINS; do
		if [ "${_e%%:*}" = "$1" ]; then
			_pin=${_e#*:}
			return 0
		fi
	done
	die "ref_commit_invalid side=$1"
}
# ref_pin SIDE ROOT PIN: the pinned object must be a commit of that repository, checked by a bounded
# Git with a cleared environment whose repository discovery stops at the root's parent, no replace
# refs, no lazy fetch and no system configuration; stderr is discarded, the status checked and the
# answer must equal the pin. The checkout's HEAD, index and working tree are never read.
ref_pin() {
	_par=${2%/*}
	[ -n "$_par" ] || _par=/
	_hc=$(timeout 30 env -i PATH="${PATH:-}" LC_ALL=C GIT_CEILING_DIRECTORIES="$_par" GIT_NO_REPLACE_OBJECTS=1 GIT_NO_LAZY_FETCH=1 GIT_CONFIG_NOSYSTEM=1 git -C "$2" rev-parse --verify --quiet "$3^{commit}" 2>/dev/null </dev/null) || die "ref_commit_invalid side=$1"
	[ "$_hc" = "$3" ] || die "ref_commit_invalid side=$1"
}
SERIESROOT=
MOVIEROOT=
SERIESCOMMIT=
MOVIECOMMIT=
if [ "$HAVE_REF" = 1 ]; then
	for _t in tr wc timeout env; do
		command -v "$_t" >/dev/null 2>&1 || die host_tool_missing
	done
	SERIESROOT=${BOXSET_REF_SERIES:-}
	MOVIEROOT=${BOXSET_REF_MOVIE:-}
	ref_root series "$SERIESROOT"
	ref_root movie "$MOVIEROOT"
	ref_pinof series
	SERIESCOMMIT=$_pin
	ref_pinof movie
	MOVIECOMMIT=$_pin
	ref_pin series "$SERIESROOT" "$SERIESCOMMIT"
	ref_pin movie "$MOVIEROOT" "$MOVIECOMMIT"
fi

for _t in docker mktemp timeout od tr rm mv mkdir cat find sha256sum chmod grep head tar env sed wc; do
	command -v "$_t" >/dev/null 2>&1 || die host_tool_missing
done
if [ "$HAVE_BENCH" = 1 ]; then
	for _t in awk date df sort id timeout stat readlink; do
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
# Capture adds only its two synthetic variables; --ref only the two checked pins.
if [ "$HAVE_CAPTURE" = 1 ]; then
	printf 'BOXSET_CAPTURE_DIR=/capture\nBOXSET_CAPTURE_CHALLENGE=%s\n' "$CHAL" >>"$T/lit" || die create_failed
fi
if [ "$HAVE_REF" = 1 ]; then
	printf 'BOXSET_REF_SERIES_COMMIT=%s\nBOXSET_REF_MOVIE_COMMIT=%s\n' "$SERIESCOMMIT" "$MOVIECOMMIT" >>"$T/lit" || die create_failed
fi
# create_ctr CHILD_ARGV...: docker create of the owned container, never with a mount; the image is
# followed by the byte-exact child argv.
create_ctr() {
	if [ "$HAVE_BENCH" = 1 ]; then
		timeout 60 docker create --name "$CTR" --label "boxset.dev-run=$RUNID" --network none \
			--cap-drop ALL --security-opt no-new-privileges --log-driver=none --user 1000:1000 --workdir /src \
			--cpuset-cpus "$BENCHCPU" --memory 4g --memory-swap 4g \
			--mount "type=bind,source=$BENCHCFG,target=/config,readonly" \
			--env-file "$T/lit" "$IMG" "$@" >/dev/null 2>&1
	else
		timeout 60 docker create --name "$CTR" --label "boxset.dev-run=$RUNID" --network none \
			--cap-drop ALL --security-opt no-new-privileges --log-driver=none --user 1000:1000 --workdir /src \
			--env-file "$T/lit" "$IMG" "$@" >/dev/null 2>&1
	fi
}
create_ctr "$@" || die create_failed
[ "$(timeout 30 docker inspect --format '{{.HostConfig.LogConfig.Type}}' "$CTR" 2>/dev/null)" = none ] || die log_driver_invalid
# Effective mounts: none (the reference text is delivered by copy, never mounted), or with --bench
# exactly the one read-only /config bind of the owner's directory.
_mnt=$(timeout 30 docker inspect --format '{{range .Mounts}}{{.Type}} {{.Source}} {{.Destination}} {{.RW}}{{println}}{{end}}' "$CTR" 2>/dev/null) || die mounts_invalid
if [ "$HAVE_BENCH" = 1 ]; then
	[ "$_mnt" = "bind $BENCHCFG /config false" ] || die mounts_invalid
else
	[ -z "$_mnt" ] || die mounts_invalid
fi
_mnt=
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
# --ref delivery, before the container is ever started: one archive per side from the private
# producer mode of the host redactor (the single producer call site), piped into `docker cp -`
# beside it. The producer's status goes to its own file, apart from the receiver's.
ref_deliver() {
	_dside=$1
	set -- "$2" "$3" $REFAREAS
	rm -f -- "$T/ps" "$T/rc"
	{ { timeout 120 "$R" -ref-archive "$@" 2>/dev/null </dev/null; printf '%s' "$?" >"$T/ps"; } |
		{ timeout 120 docker cp - "$CTR:/ref/$_dside" >/dev/null 2>&1; printf '%s' "$?" >"$T/rc"; }; }
	_ps=$(cat "$T/ps" 2>/dev/null)
	case $_ps in '' | *[!0-9]*) die "ref_copy_failed side=$_dside" ;; esac
	[ "$_ps" != 3 ] || die "ref_invalid side=$_dside"
	[ "$_ps" = 0 ] || die "ref_copy_failed side=$_dside"
	[ "$(cat "$T/rc" 2>/dev/null)" = 0 ] || die "ref_copy_failed side=$_dside"
}
# ref_readback SIDE: /ref/<side> of the same not-yet-started container is read back through a cap and
# may hold only relative regular files and directories under the three areas (at least one file in
# each), owner 0/0, modes 0444 and 0555; diagnostics are discarded.
ref_readback() {
	_dside=$1
	rm -f -- "$T/rp" "$T/rt" "$T/rl"
	{ { timeout 60 docker cp "$CTR:/ref/$_dside" - 2>/dev/null </dev/null; printf '%s' "$?" >"$T/rp"; } |
		head -c "$((REFCAP + 1))" |
		{ tar -t -v --numeric-owner -f - 2>/dev/null; printf '%s' "$?" >"$T/rt"; } >"$T/rl"; }
	{ [ "$(cat "$T/rp" 2>/dev/null)" = 0 ] && [ "$(cat "$T/rt" 2>/dev/null)" = 0 ]; } || die "ref_copy_failed side=$_dside"
	_nt=0
	_np=0
	_nq=0
	while read -r _p _o _z _d _t _n; do
		[ "$_o" = 0/0 ] || die "ref_copy_failed side=$_dside"
		case $_p in
		d*)
			case $_n in
			"$_dside/") ;;
			"$_dside/tests/"* | "$_dside/parser/"* | "$_dside/qualities/"*)
				{ [ "$_p" = dr-xr-xr-x ] && case $_n in */) true ;; *) false ;; esac; } || die "ref_copy_failed side=$_dside"
				;;
			*) die "ref_copy_failed side=$_dside" ;;
			esac
			;;
		-r--r--r--)
			case $_n in
			"$_dside/tests/"*.cs) _nt=$((_nt + 1)) ;;
			"$_dside/parser/"*.cs) _np=$((_np + 1)) ;;
			"$_dside/qualities/"*.cs) _nq=$((_nq + 1)) ;;
			*) die "ref_copy_failed side=$_dside" ;;
			esac
			;;
		*) die "ref_copy_failed side=$_dside" ;;
		esac
	done <"$T/rl"
	{ [ "$_nt" -ge 1 ] && [ "$_np" -ge 1 ] && [ "$_nq" -ge 1 ]; } || die "ref_copy_failed side=$_dside"
}
if [ "$HAVE_REF" = 1 ]; then
	ref_deliver series "$SERIESROOT" "$SERIESCOMMIT"
	ref_readback series
	ref_deliver movie "$MOVIEROOT" "$MOVIECOMMIT"
	ref_readback movie
fi
timeout 60 docker cp "$CTR:/usr/local/share/boxset/inputs.sha256" "$T/inputs" >/dev/null 2>&1 || die inputs_unavailable
{ [ -f "$T/inputs" ] && [ ! -L "$T/inputs" ]; } || die inputs_unavailable
INPUTS=$(sha256sum <"$T/inputs") || die inputs_unavailable
INPUTS=${INPUTS%% *}
printf 'dev-run: image=%s inputs=%s timeout=%ss container=%s log=none%s\n' "$IMG" "$INPUTS" "$TIMEOUT" "$CTR" "$([ "$HAVE_REF" = 0 ] || printf ' ref=objects')" >&2

# --- benchmark host evidence (--bench) -----------------------------------------------------
# The host observes; the child never sees or writes these. Every record is aggregates of fixed
# numbers, enums and booleans (bench/parser/eligibility.go owns the schema): no CPU list, task
# listing, command line, environment, path or host name. A value that cannot be read fails closed
# (bench_evidence_failed, nothing published); a value that reads but is not eligible is recorded and
# the judge decides.
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
}
# bench_overlap: "ALLOWED RUNNABLE SEEN": the user threads outside this run's container (CID) whose
# allowed CPU list meets the reserved set, how many of them are runnable, and how many threads were
# read. Kernel threads (no address space), threads that exited while being read and the scanner
# itself (this function's own shell and everything below it) are not counted. The enumeration is
# complete or the call fails: the list of threads goes through a scratch file, the status of
# `find` is kept, and its diagnostics may only be "No such file or directory" (a task that vanished
# while it was walked); a timeout, any other diagnostic, a thread that is still there but cannot be
# read, or a read that ends early fails the observation (nothing is counted from it).
bench_overlap() {
	timeout 30 find /proc -mindepth 4 -maxdepth 4 -path '/proc/[0-9]*/task/[0-9]*/status' -print >"$T/f/tasks" 2>"$T/f/terr"
	_frc=$?
	case $_frc in 0 | 1) ;; *) return 1 ;; esac
	[ "$_frc" = 0 ] || [ -s "$T/f/terr" ] || return 1
	[ "$(grep -c -v -E '^([^ ]*/)?find: .*: No such file or directory$' "$T/f/terr")" = 0 ] || return 1
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
# bench_config: CFG_EXT4, CFG_NVME and CFG_EMPTY (1 or 0) of the read-only /config's backing directory,
# read now. A failed read of any kind (df, or the directory scan: an unreadable directory is not an empty
# one) fails the call; empty is only a scan that succeeded and found no entry. Names found are never kept.
bench_config() {
	_dfo=$(timeout 30 df --output=source,fstype -- "$BENCHCFG" 2>/dev/null) || return 1
	set -- $(printf '%s\n' "$_dfo" | awk 'NR == 2 { print $1, $2 }')
	[ $# = 2 ] || return 1
	CFG_EXT4=0
	[ "$2" != ext4 ] || CFG_EXT4=1
	backing_nvme /sys "$1" || return 1
	_cid=$(timeout 30 stat -c '%d:%i' -- "$BENCHCFG" 2>/dev/null) || return 1
	[ "$_cid" = "$CFG_ID" ] || return 1
	_first=$(timeout 30 find "$BENCHCFG" -mindepth 1 -maxdepth 1 -print -quit 2>/dev/null) || return 1
	CFG_EMPTY=0
	[ -n "$_first" ] || CFG_EMPTY=1
	_first=
}
# bench_obs FILE: appends one observation line `unix allowed runnable load_milli mem_available_kib
# swap_in swap_out ext4 nvme empty`; status 1 (nothing appended) when any value cannot be read. The
# /config facts are read again at every observation, so a change between stages reaches the record.
bench_obs() {
	_of=$1
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
	# The view must be the whole machine's: the tasks read are (nearly) all the kernel counts.
	_tot=$(awk '{ split($4, a, "/"); print a[2] }' /proc/loadavg 2>/dev/null) || return 1
	case $_tot in '' | *[!0-9]*) return 1 ;; esac
	[ $(($3 * 100)) -ge $((_tot * 97)) ] || return 1
	printf '%s %s %s %s %s %s %s %s %s\n' "$_u" "$1 $2" "$_la" "$_mem" "$_sw" "$CFG_EXT4" "$CFG_NVME" "$CFG_EMPTY" "$4" >>"$_of"
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
# nonnegative integer, else status 1) and the topology read now. The /config facts are true only when
# true at every observation of the stage.
bench_emit() {
	bench_topology || return 1
	awk -v stage="$1" -v run="${BENCHRUN##*/}" -v image="$IMG" -v inputs="$INPUTS" \
		-v cpus="$TOPO_CPUS" -v cores="$TOPO_CORES" -v tpc="$TOPO_TPC" -v sibout="$TOPO_SIBOUT" -v numa="$TOPO_NUMA" \
		-v memlim="$FACT_MEM" -v swaplim="$FACT_SWAP" -v cpusetok="$FACT_CPUSET" -v ro="$FACT_RO" '
NF != 11 { bad = 1; next }
{
	for (i = 1; i <= 11; i++) if ($i !~ /^[0-9]+$/) bad = 1
	n++
	if (n == 1) { first = $1; mem = $5; si = $6; so = $7; e4 = 1; nv = 1; em = 1 }
	last = $1
	ei = $6
	eo = $7
	if ($2 + 0 > am) am = $2 + 0
	if ($3 + 0 > rm) rm = $3 + 0
	if ($4 + 0 > lm) lm = $4 + 0
	if ($5 + 0 < mem + 0) mem = $5
	if ($8 != 1) e4 = 0
	if ($9 != 1) nv = 0
	if ($10 != 1) em = 0
	if ($11 + 0 > sm) sm = $11 + 0
}
END {
	if (bad || n < 1) exit 1
	printf "{\"schema\":1,\"stage\":\"%s\",\"run\":\"%s\",\"image\":\"%s\",\"inputs\":\"%s\",", stage, run, image, inputs
	printf "\"start_unix\":%.0f,\"end_unix\":%.0f,\"observations\":%.0f,", first, last, n
	printf "\"cpus\":%.0f,\"cores\":%.0f,\"threads_per_core_max\":%.0f,\"siblings_outside_set\":%.0f,\"numa_nodes\":%.0f,", cpus, cores, tpc, sibout, numa
	printf "\"foreign_allowed_max\":%.0f,\"foreign_runnable_max\":%.0f,\"sibling_runnable_max\":%.0f,\"load1_milli_max\":%.0f,\"mem_available_kib_min\":%.0f,", am, rm, sm, lm, mem
	printf "\"swap_in_start\":%.0f,\"swap_in_end\":%.0f,\"swap_out_start\":%.0f,\"swap_out_end\":%.0f,", si, ei, so, eo
	printf "\"mem_limit_bytes\":%.0f,\"memswap_limit_bytes\":%.0f,\"cpuset_matches\":%s,", memlim, swaplim, cpusetok
	printf "\"config_fs\":\"%s\",\"config_nvme\":%s,\"config_empty\":%s,\"config_readonly\":%s}\n", e4 ? "ext4" : "other", nv ? "true" : "false", em ? "true" : "false", ro
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
	bench_must bench_obs "$T/f/before.obs"
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
# export_same NAME: a corpus is published only when the stopped container's raw file hashes exactly
# as the sanitized staged bytes: raw bytes only travel through pipes into sha256sum, and a corpus the
# redactor would change is export_changed with nothing published (the container is still unremoved).
export_same() {
	rm -f -- "$T/sp" "$T/hs" "$T/cs" "$T/ds" "$T/mis" "$T/cperr" "$T/ts" "$T/rh"
	cp_out "/src/$WRITE/$1" | head -c "$STREAMCAP" |
		{ tar -x -O -f - 2>/dev/null; printf '%s' "$?" >"$T/ts"; } | head -c "$((WCAP + 1))" | sha256sum >"$T/rh"
	for _f in sp ds hs cs ts; do
		[ "$(cat "$T/$_f" 2>/dev/null)" = 0 ] || die export_failed
	done
	_rh=$(cat "$T/rh" 2>/dev/null)
	_rh=${_rh%% *}
	case $_rh in '' | *[!0-9a-f]*) die export_failed ;; esac
	_sh=$(sha256sum <"$T/p/$1") || die export_failed
	[ "$_rh" = "${_sh%% *}" ] || die export_changed
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
		[ "$WRITE" != testdata/parser ] || export_same "$_n"
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
	timeout 60 docker create --name "$_vc" --label "boxset.dev-run=$RUNID" --network none --read-only \
		--cap-drop ALL --security-opt no-new-privileges --log-driver=none --user 1000:1000 --memory 64m --memory-swap 64m --pids-limit 64 -i "$IMG" \
		/usr/local/libexec/boxset/capture-check -name "$1" -challenge "$CHAL" >/dev/null 2>&1 || die capture_copy_failed
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
	timeout 60 docker rm -f "$_vc" >/dev/null 2>&1 || die cleanup_failed
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
timeout 60 docker rm -f "$CTR" >/dev/null 2>&1 || die cleanup_failed
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
	# The fresh run directory is created now and never reused: an existing one stops the publication.
	write_chain_ok || die write_root_invalid
	mkdir -p -- "${BENCHRUN%/*}" || die publish_failed
	mkdir -- "$BENCHRUN" 2>/dev/null || die 'bench_invalid side=samples'
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
exit "$CHILD"
