#!/bin/sh
# Canonical Boxset dev runner (host, repository root only):
#   scripts/dev-run.sh [--ref] [--write PATH] [--capture DIR] [--bench CPUSET] [--timeout SECONDS] -- COMMAND [ARG...]
#
# Rebuilds the `work` image from the current allowlisted inputs (frozen
# dependencies, no lock regeneration) and starts it as a labelled, network-less,
# capability-less container whose PID 1 is a finite `sleep` run as 65534. COMMAND
# runs as the fixed unprivileged user 1000:1000 (whatever the host UID) through
# `docker exec` with its exact argv, in its own process group, under a host
# deadline. Child stdout+stderr pass through the host redactor (deploy/dev/redact)
# before display; build/start diagnostics only as a redacted tail. A --write root
# is a supervisor-owned protected fixed-name map on a 32 MiB tmpfs: the child
# writes the precreated files in place and nothing reaches the host until the
# producer is quiescent, the container is checked closed and the staged parts
# are published by rename. Exit status: the child's, or 124 on deadline, or 125
# with a fixed reason word on stderr. No host environment, Docker socket, .env,
# workspace, reference text (other than --ref) or port reaches the child.
#
# This script is only the bootstrap. It parses and validates the arguments,
# audits what it can without touching anything, builds the runner-private host
# helper (deploy/dev/redact: the redactor and the native lifecycle controller),
# and then `exec`s `redact -run`. The controller keeps this process's PID,
# environment, cwd, umask and stdio and is the only lifetime authority: no shell
# survives the exec and there is no result handoff. Runner notes and the
# recovery of a failing bootstrap: docs/spikes/s5-version-admission.md.
set -u
set -f
LC_ALL=C
export LC_ALL
umask 022

REF_DIRS='src/NzbDrone.Core.Test/ParserTests src/NzbDrone.Core.Test/Languages src/NzbDrone.Core/Parser src/NzbDrone.Core/Qualities src/NzbDrone.Core/Languages'

REF=0
WRITE=
HAVE_WRITE=0
CAPTURE=
HAVE_CAPTURE=0
HAVE_BENCH=0
TIMEOUT=600
HAVE_TIMEOUT=0
RUNID=
TMPD=
TMPD_ID=
TMPD_OWN=
REPO=
SLUG=
LEAF=
WRITE_COMPONENTS=
REDACT=

die() {
	printf 'dev-run: %s\n' "$1" >&2
	exit "${2:-125}"
}

usage() {
	printf '%s\n' 'usage: scripts/dev-run.sh [--ref] [--write PATH] [--capture DIR] [--bench CPUSET] [--timeout SECONDS] -- COMMAND [ARG...]' >&2
	die usage
}

# The only thing the bootstrap ever removes is the private directory whose
# creation succeeded in this invocation and whose identity is still the acquired
# one: never a pre-existing collision, never a replacement.
cleanup() {
	if [ -n "$TMPD_OWN" ] && [ "$(find "$TMPD" -maxdepth 0 -printf '%D:%i' 2>/dev/null)" = "$TMPD_ID" ]; then
		rm -rf "$TMPD"
	fi
	return 0
}
trap cleanup EXIT
trap 'printf "dev-run: interrupted\n" >&2; exit 125' HUP INT TERM

# chain_ok PATH: every component of the relative PATH is a real directory.
chain_ok() {
	_cur=
	_old=$IFS
	IFS=/
	set -- $1
	IFS=$_old
	for _c in "$@"; do
		_cur=${_cur:+$_cur/}$_c
		{ [ -d "$_cur" ] && [ ! -L "$_cur" ]; } || return 1
	done
}

# --- arguments ---------------------------------------------------------------
while [ $# -gt 0 ]; do
	case $1 in
	--) break ;;
	--ref)
		[ "$REF" = 0 ] || usage
		REF=1
		;;
	--write)
		[ $# -ge 2 ] && [ "$HAVE_WRITE" = 0 ] || usage
		HAVE_WRITE=1
		WRITE=$2
		shift
		;;
	--capture)
		{ [ $# -ge 2 ] && [ "$HAVE_CAPTURE" = 0 ]; } || die capture_flag_invalid
		HAVE_CAPTURE=1
		CAPTURE=$2
		shift
		;;
	--bench)
		[ $# -ge 2 ] && [ "$HAVE_BENCH" = 0 ] || usage
		HAVE_BENCH=1
		shift
		;;
	--timeout)
		[ $# -ge 2 ] && [ "$HAVE_TIMEOUT" = 0 ] || usage
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

if [ "$HAVE_CAPTURE" = 1 ] && [ "$HAVE_BENCH" = 1 ]; then die capture_flag_invalid; fi
case $TIMEOUT in
'' | 0* | *[!0-9]*) die timeout_invalid ;;
esac
{ [ "$TIMEOUT" -ge 1 ] && [ "$TIMEOUT" -le 600 ]; } || die timeout_invalid
# Benchmark mode (CPUSET reservation, BOXSET_BENCH_CONFIG, host facts and its
# fixed write root) is owned by the benchmark slice; refusing is the only honest
# behaviour here.
[ "$HAVE_BENCH" = 0 ] || die bench_unavailable

{ [ -f deploy/dev/Dockerfile ] && [ -f scripts/dev-run.sh ] && [ -f .dockerignore ]; } || die not_repo_root
REPO=$(pwd -P)
case $REPO in *[,:]* | *'"'* | *' '*) die repo_path_unsupported ;; esac

# --- destination validation (before any Docker or write) ---------------------
if [ "$HAVE_CAPTURE" = 1 ]; then
	case $CAPTURE in
	.devrites/work/*/screenshots-[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T[0-9][0-9][0-9][0-9][0-9][0-9]Z) ;;
	*) die capture_destination_invalid ;;
	esac
	_rest=${CAPTURE#.devrites/work/}
	SLUG=${_rest%%/*}
	LEAF=${_rest#*/}
	case $LEAF in */*) die capture_destination_invalid ;; esac
	case $SLUG in
	'' | [!a-z0-9]* | *[!a-z0-9-]*) die capture_destination_invalid ;;
	esac
	[ "${#SLUG}" -le 64 ] || die capture_destination_invalid
	chain_ok ".devrites/work/$SLUG" || die capture_destination_invalid
	if [ -e "$CAPTURE" ] || [ -L "$CAPTURE" ]; then die capture_destination_exists; fi
fi
if [ "$HAVE_WRITE" = 1 ]; then
	case $WRITE in
	testdata/parser | docs/spikes/issue-drafts) ;;
	*) die write_path_invalid ;;
	esac
	WRITE_COMPONENTS=$(printf '%s' "$WRITE" | tr / ' ')
	# Read-only precheck: every lexical component that already exists must be a
	# real directory; the first missing one ends the check and nothing is created
	# (the controller creates and pins the root).
	_cur=
	for _c in $WRITE_COMPONENTS; do
		_cur=${_cur:+$_cur/}$_c
		[ ! -L "$_cur" ] || die write_root_symlink
		[ -e "$_cur" ] || break
		[ -d "$_cur" ] || die write_root_invalid
	done
fi

# --- reference text audit ----------------------------------------------------
# The two reference checkouts are located only by the owner values BOXSET_REF_SERIES
# and BOXSET_REF_MOVIE (absolute, link-free directory paths).
audit_references() {
	for _side in series movie; do
		case $_side in series) _root=${BOXSET_REF_SERIES:-} ;; *) _root=${BOXSET_REF_MOVIE:-} ;; esac
		case $_root in /?*) ;; *) die reference_input_invalid ;; esac
		case $_root in *[,:\"]* | */) die reference_input_invalid ;; esac
		for _rel in $REF_DIRS; do
			_d=$_root/$_rel
			(cd / && chain_ok "${_d#/}") || die reference_input_invalid
			[ -z "$(find "$_d" ! -type f ! -type d -print -quit)" ] || die reference_input_invalid
			[ -z "$(find "$_d" -type f \( ! -name '*.cs' -o -perm /111 \) -print -quit)" ] || die reference_input_invalid
			[ -z "$(find "$_d" -type f ! -perm -o=r -print -quit)" ] || die reference_input_invalid
			[ -z "$(find "$_d" -type d ! -perm -o=rx -print -quit)" ] || die reference_input_invalid
		done
	done
}
[ "$REF" = 0 ] || audit_references

# --- host tools ----------------------------------------------------------------
for _t in sh docker id mkdir rmdir ln rm mv head tail wc od tr cat sha256sum timeout date grep find sleep; do
	command -v "$_t" >/dev/null 2>&1 || {
		if [ "$HAVE_CAPTURE" = 1 ]; then die capture_host_tool_missing; else die host_tool_missing; fi
	}
done
timeout 30 docker version --format '{{.Server.Version}}' >/dev/null 2>&1 || die docker_unavailable

# --- run-private directory ---------------------------------------------------------
RUNID=$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')
case $RUNID in
*[!0-9a-f]*) die temp_dir_failed ;;
????????????????) ;;
*) die temp_dir_failed ;;
esac
# A relative TMPDIR names a directory under the original working directory; it is
# resolved here, before anything is created, exported or exec'd, and the inherited
# environment is left as it is. An absolute one keeps its literal spelling apart
# from doubled and trailing slashes.
_tb=${TMPDIR:-/tmp}
case $_tb in
/*)
	while case $_tb in *//*) true ;; *) false ;; esac; do _tb=${_tb%%//*}/${_tb#*//}; done
	while case $_tb in */) true ;; *) false ;; esac; do _tb=${_tb%/}; done
	case $_tb/ in */./* | */../*) die temp_dir_failed ;; esac
	;;
*)
	_tb=$(cd -P -- "$_tb" 2>/dev/null && pwd -P) || die temp_dir_failed
	while case $_tb in */) true ;; *) false ;; esac; do _tb=${_tb%/}; done
	;;
esac
TMPD=$_tb/boxset-dev-run.$RUNID
case $TMPD in *[,:]* | *'"'* | *' '*) die temp_dir_failed ;; esac
mkdir -m 0700 -- "$TMPD" || die temp_dir_failed
TMPD_ID=$(find "$TMPD" -maxdepth 0 -printf '%D:%i' 2>/dev/null)
if [ -z "$TMPD_ID" ]; then
	rmdir -- "$TMPD" 2>/dev/null
	die temp_dir_failed
fi
TMPD_OWN=1

# --- redactor/controller: built and exported to the private directory ----------------
# The helper runs on the host so owner values never enter Docker. Build/export
# output is discarded; any failure leaves no usable helper.
REDACT=$TMPD/bin/redact
mkdir -m 0700 -- "$TMPD/bin" || die redact_unavailable
timeout --kill-after=10s 1800s docker build --file deploy/dev/Dockerfile --target host-redact \
	--output "type=local,dest=$TMPD/bin" . >/dev/null 2>&1 || die redact_unavailable
[ -f "$REDACT" ] || die redact_unavailable
"$REDACT" -check </dev/null >/dev/null 2>&1 || die redact_unavailable

# --- hand over -----------------------------------------------------------------------
# From here the controller is the runner: same PID, environment, cwd, umask and
# stdio; its exit status is the runner's. Caught traps reset on exec, ignored
# signals stay ignored.
exec "$REDACT" -run "$REPO" "$RUNID" "$TMPD" "$TIMEOUT" "$REF" "${WRITE:--}" "${CAPTURE:--}" -- "$@"
