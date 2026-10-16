#!/bin/sh
# Canonical Boxset dev runner (host, repository root only):
#   scripts/dev-run.sh [--write docs/spikes/issue-drafts] [--timeout SECONDS] -- COMMAND [ARG...]
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
# (deploy/dev/redact -cap) and bounded to 64 KiB.
# Exit status: the child's (Docker's checked terminal state), 124 on the
# deadline, or 125 with a fixed reason word on stderr.
#
# --write docs/spikes/issue-drafts publishes the fixed draft names the child
# left under /src/docs/spikes/issue-drafts (s5-admission.md, s5-tools.md): only
# after the container stopped, each exported as one tar stream that must hold
# exactly one regular file of at most 64 KiB, streamed straight through the
# redactor (never spooled raw) and capped at 64 KiB, and only after the owned
# container was removed; a draft is published even when the child exits nonzero.
# The archive's member names are checked in a pipe and only fixed result tokens
# are kept; copy diagnostics pass through the capped redactor before they are kept.
# A later rename failure may leave earlier names published. There is no rollback
# and no durability claim. --ref, --capture and --bench belong to later work and
# fail with <flag>_unavailable. Bootstrap before the redactor exists prints only
# a fixed reason word; recovery: docs/spikes/s5-version-admission.md.
set -u
set -f
LC_ALL=C
export LC_ALL
umask 022

CAP=65536
DRAFTS='s5-admission.md s5-tools.md'
WRITE_CHAIN='docs docs/spikes docs/spikes/issue-drafts'
TIMEOUT=600
WRITE=
HAVE_WRITE=0
HAVE_TIMEOUT=0
RUNID=
T=
OWN=
R=

die() {
	printf 'dev-run: %s\n' "$1" >&2
	exit "${2:-125}"
}

usage() {
	printf '%s\n' 'usage: scripts/dev-run.sh [--write docs/spikes/issue-drafts] [--timeout SECONDS] -- COMMAND [ARG...]' >&2
	die usage
}

# Removes only what this invocation created: its labelled container (the label
# carries the random run id) and its scratch directory, if still ours.
cleanup() {
	if [ -n "$RUNID" ]; then
		_ids=$(timeout 30 docker ps -aq --filter "label=boxset.dev-run=$RUNID" 2>/dev/null)
		[ -z "$_ids" ] || timeout 60 docker rm -f $_ids >/dev/null 2>&1
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
	--ref | --capture | --bench) die "${1#--}_unavailable" ;;
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

case $TIMEOUT in
'' | 0* | *[!0-9]*) die timeout_invalid ;;
esac
{ [ "${#TIMEOUT}" -le 3 ] && [ "$TIMEOUT" -le 600 ]; } || die timeout_invalid
if [ "$HAVE_WRITE" = 1 ]; then
	[ "$WRITE" = docs/spikes/issue-drafts ] || die write_path_invalid
fi

{ [ -f deploy/dev/Dockerfile ] && [ -f scripts/dev-run.sh ] && [ -f .dockerignore ]; } || die not_repo_root

# write_chain_ok: no component of the write root is a link or a non-directory.
write_chain_ok() {
	for _c in $WRITE_CHAIN; do
		[ ! -L "$_c" ] || return 1
		[ ! -e "$_c" ] || [ -d "$_c" ] || return 1
	done
}
[ "$HAVE_WRITE" = 0 ] || write_chain_ok || die write_root_invalid

for _t in docker mktemp timeout od tr rm mv mkdir cat find sha256sum chmod grep head tar env sed; do
	command -v "$_t" >/dev/null 2>&1 || die host_tool_missing
done
# The daemon the caller selected (context, DOCKER_HOST) is resolved with the
# caller's own configuration and then pinned; the private configuration below
# must reach the same daemon or the run stops.
DID=$(timeout 30 docker info --format '{{.ID}}' 2>/dev/null) && [ -n "$DID" ] || die docker_unavailable
DH=$(timeout 30 docker context inspect --format '{{.Endpoints.docker.Host}}' 2>/dev/null) && [ -n "$DH" ] || die docker_unavailable

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
timeout 60 docker create --name "$CTR" --label "boxset.dev-run=$RUNID" --network none \
	--cap-drop ALL --security-opt no-new-privileges --log-driver=none --user 1000:1000 --workdir /src \
	--env-file "$T/lit" "$IMG" "$@" >/dev/null 2>&1 || die create_failed
[ "$(timeout 30 docker inspect --format '{{.HostConfig.LogConfig.Type}}' "$CTR" 2>/dev/null)" = none ] || die log_driver_invalid
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

# --- run -------------------------------------------------------------------------
# Output comes only from the attached stream. The left side of the pipeline
# reports its status through a fresh scratch file (POSIX sh keeps only the last
# command's status); a missing or non-numeric value is a failure.
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

# --- export (stopped container), checked removal, publish ----------------------------
# export_one NAME: `docker cp` writes one tar stream to FD 3; its diagnostics are first
# bounded (4097 raw bytes, so the redactor's own 4096 cap still overflows closed) and
# classified by sed, which stores only the fixed token `M` when a line says the file was
# not found (the raw line is never written anywhere), and only then pass through the capped
# redactor into $T/cperr, so a selected secret value cannot mask the classifier. At most
# STREAMCAP bytes of the stream are read, and GNU tar
# hands each regular entry's payload to EXPORT_CMD, which (inside tar's child shell,
# reading DR_* from the environment) accepts exactly one entry of the type f, the
# constant name and a raw size of at most CAP bytes and pipes it through the redactor
# straight into the private staging file DR_OUT. Raw bytes only ever travel through
# pipes. tar's verbose listing (its stdout; the callback's output never reaches it) is
# read through a pipe of at most 4096 bytes by list_one, which stores only the fixed
# token one|bad, never a name: anything tar lists beyond that one entry (links,
# directories, devices, more files) fails the check.
STREAMCAP=262144
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
export_one() {
	rm -f -- "$T/sp" "$T/ds" "$T/ts" "$T/rs" "$T/why" "$T/seen" "$T/lst" "$T/cperr" "$T/hs" "$T/cs" "$T/mis" "$T/p/$1"
	{
		{ timeout 60 docker cp "$CTR:/src/$WRITE/$1" - 2>&1 1>&3 3>&- </dev/null; printf '%s' "$?" >"$T/sp"; } |
			{ head -c 4097; printf '%s' "$?" >"$T/hs"; } |
			{ sed -e '/Could not find the file/{' -e h -e 's/.*/M/' -e "w $T/mis" -e g -e '}'; printf '%s' "$?" >"$T/cs"; } |
			"$R" -cap 4096 >"$T/cperr" 3>&-
		printf '%s' "$?" >"$T/ds"
	} 3>&1 | head -c "$STREAMCAP" |
		{ env DR_NAME="$1" DR_CAP="$CAP" DR_R="$R" DR_WHY="$T/why" DR_SEEN="$T/seen" DR_RS="$T/rs" DR_OUT="$T/p/$1" \
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
		if [ "$(cat "$T/mis" 2>/dev/null)" = M ] && [ ! -e "$T/seen" ]; then
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
if [ "$HAVE_WRITE" = 1 ]; then
	mkdir -m 0700 -- "$T/x" "$T/p" || die export_failed
	for _n in $DRAFTS; do
		export_one "$_n"
	done
fi
timeout 60 docker rm -f "$CTR" >/dev/null 2>&1 || die cleanup_failed
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
exit "$CHILD"
