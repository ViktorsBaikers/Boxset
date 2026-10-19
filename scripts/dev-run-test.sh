#!/bin/sh
# Host-side contract driver for scripts/dev-run.sh, repository root only:
#   scripts/dev-run-test.sh
# Runs the real runner (a SHA-256-identical copy) against an isolated checkout of
# the current allowlisted inputs and asserts what the shell lifecycle executes: exact argv
# and statuses, the host deadline (status 124, the identity line and no reason word), the
# stock child identity, the issue-draft publication (the child creates its own names; a name
# it never created stays absent), the refused write roots, binary-safe redaction, the capture
# handoff (real pinned chrome-headless-shell provider, validator and lifetime controls), the
# validator and redactor unit tests through the same runner, and the toolchain admission
# mutation, support-block and lock-export controls. Every rejecting mutant runs the same
# named assertion twice: RED against the SHA-bound mutant and GREEN against the original (a
# mutant or fault copy is a shell copy of the runner that changes one token of it).
# Exit: 0 all controls pass; 1 an assertion failed; 3 the browser provider is
# unavailable (T31-PROVIDER-UNAVAILABLE).
#   --tools-failure and --benchmark-evidence belong to later slices.
#   --classifier-self-check runs only the permanent self-check (no container, no build, no clock helper): the shared classifier, its case
#   latch and plain-credit gate, the plain-control targets, the parent-owned unknown observations, the lint-target shapes, the readiness
#   probes, the driver clock reader, the host-loss result classes, registration and watcher and the bounded terminal helper on a virtual
#   clock, the logging-shim receipts and natural-cell premise, the clock-recovery diagnostic and the hash-record oracle.
#   It is the exact deterministic command root runs before the full proof, which also begins with it.
# The runner is the shell lifecycle: it uses deploy/dev/redact only as -check, -cap and -tail and never execs -run, so no controller source is mutated
# here; a runner mutant is a shell copy (variant) that changes a token of scripts/dev-run.sh, or a compiled copy of the host helper's main.go (gvariant).
set -u
set -f
LC_ALL=C
export LC_ALL

SELFCHECK_ONLY=0
case "${1:-}" in
--tools-failure | --benchmark-evidence)
	printf 'DEV-RUN-TEST-UNOWNED: %s is owned by a later slice and is not implemented here\n' "$1" >&2
	exit 2
	;;
--classifier-self-check)
	if [ $# != 1 ]; then
		printf 'usage: scripts/dev-run-test.sh [--classifier-self-check]\n' >&2
		exit 2
	fi
	SELFCHECK_ONLY=1
	;;
'') ;;
*)
	printf 'usage: scripts/dev-run-test.sh [--classifier-self-check]\n' >&2
	exit 2
	;;
esac

for t in sh docker tar cp cmp mktemp sleep kill base64 comm sort find grep od tr sha256sum wc date head tail id cat timeout ln mv rm mkdir sed cut awk setsid env; do
	command -v "$t" >/dev/null 2>&1 || {
		printf 'dev-run-test: host tool %s missing\n' "$t" >&2
		exit 125
	}
done
{ [ -f scripts/dev-run.sh ] && [ -f deploy/dev/Dockerfile ]; } || {
	printf 'dev-run-test: run from the repository root\n' >&2
	exit 125
}
SRC=$(pwd -P)

TMP=$(mktemp -d) || exit 125
ISO=$TMP/iso
CANARY=$TMP/outside
MUT=$TMP/mut
BAR=$TMP/bar
PASSES=0
FAILS=0
OWNED=
OBS=
SETUP=0
REACHED=0
META=0
CASE_FAILED=0
PLAIN_RAN=0
PLAIN_WANT=
CLOCK=
SELF=$0
BGDIR=$ISO
mkdir -p "$MUT" "$BAR" "$TMP/hl"
# The proof-wide contamination records are pre-created: a record is one appended byte, so it needs no new inode and a
# full inode table cannot turn an unknown observation into a clean one.
{ true >"$TMP/unknown" && true >"$TMP/infra" && true >"$TMP/rerr"; } || exit 125
# Only containers named in a runner's own `container=` stderr line are ever
# removed here; there is no global label diff and no foreign deletion.
# shellcheck disable=SC2154 # c is the trap body's own loop variable
trap 'for c in $OWNED $(cat "$TMP/owned.list" 2>/dev/null); do docker rm -f "$c" >/dev/null 2>&1; done; rm -rf "$TMP"' EXIT
trap 'exit 125' HUP INT TERM

# pass NAME: every credit in this driver goes through the shared classifier: a plain control is credited only as a
# GREEN whose assertion already held, so its target must have been reached and its setup, infrastructure and unknown
# observations must be clear (rg_verdict); only the classifier's own meta-tests (META=1) are exempt.
pass() {
	if [ "$META" = 1 ] || rg_verdict green 0; then
		printf 'ok - %s\n' "$1"
		PASSES=$((PASSES + 1))
	else
		fail "$1" "credit refused by the classifier:$RG_WHY $OBS"
	fi
}
fail() {
	printf 'not ok - %s: %s\n' "$1" "$2"
	FAILS=$((FAILS + 1))
	CASE_FAILED=1
}
expect() { # expect NAME GOT WANT
	if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "got '$2', want '$3'"; fi
}
reason() { # reason NAME WORD: stderr carries the fixed word
	if grep -Fq "dev-run: $2" "$TMP/rerr"; then pass "$1"; else fail "$1" "stderr lacks 'dev-run: $2': $(head -c 400 "$TMP/rerr")"; fi
}
noreason() { # noreason NAME WORD
	if grep -Fq "dev-run: $2" "$TMP/rerr"; then fail "$1" "stderr unexpectedly has 'dev-run: $2'"; else pass "$1"; fi
}

# --- runner invocation ---------------------------------------------------------------
RUNNER=$ISO/scripts/dev-run.sh
rrx() { # rrx DIR RUNNER ARGS...: sets RS; stdout $TMP/rout, stderr $TMP/rerr
	_xd=$1
	_xr=$2
	shift 2
	(cd "$_xd" && "$_xr" "$@") >"$TMP/rout" 2>"$TMP/rerr"
	RS=$?
	rerr_seen
	_xc=$(cname)
	[ -z "$_xc" ] || case " $OWNED " in *" $_xc "*) ;; *) OWNED="$OWNED $_xc" ;; esac
}
# plain_target [WORD]: the target observation of a plain control's own runner run (its stderr is $TMP/rerr). The runner speaking is never evidence by
# itself (an earlier refusal speaks too): a control that expects the child to run is reached only when the runner's identity line, printed immediately
# before the child starts, is there; a control that expects the runner's own fixed refusal is reached only when that exact word is spoken. Anything else
# leaves the case unreached, so every credit of it is refused.
RUNNER_STARTED_RE='^dev-run: image=sha256:[0-9a-f]{64} inputs=[0-9a-f]{64} timeout=[0-9]+s container=boxset-dev-run-[0-9a-f]{16} log=none$'
plain_target() {
	if [ -n "${1:-}" ]; then
		grep -Fq "dev-run: $1" "$TMP/rerr" 2>/dev/null && reached
	else
		grep -Eq "$RUNNER_STARTED_RE" "$TMP/rerr" 2>/dev/null && reached
	fi
	return 0
}
# rrp [-w WORD] [-s STATUS] DIR RUNNER ARGS...: a plain control run in a fresh case; its target is plain_target and its intended final status is declared by plain_declare
# (STATUS, else 125 when a refusal WORD is expected, else 0): rg_verdict refuses every credit of the case while the real status is another one, whatever order its assertions run in
rrp() {
	_rpw=
	_rps=
	while :; do
		case ${1:-} in
		-w)
			_rpw=$2
			shift 2
			;;
		-s)
			_rps=$2
			shift 2
			;;
		*) break ;;
		esac
	done
	rg_reset
	rrx "$@"
	plain_target "$_rpw"
	plain_declare "$_rpw" "$_rps"
}
rr() { # rr [-w WORD] [-s STATUS] ARGS...: rrp against the isolated checkout and its runner copy
	_rra=
	while :; do
		case ${1:-} in
		-w | -s)
			_rra="$_rra $1 $2"
			shift 2
			;;
		*) break ;;
		esac
	done
	# shellcheck disable=SC2086
	rrp $_rra "$ISO" "$RUNNER" "$@"
}
# plain_declare WORD STATUS: the intended final runner status of one plain run: STATUS when given, else the runner's own refusal status 125 when a refusal WORD is expected,
# else 0. It is the one place a plain case states what its run must end with.
plain_declare() {
	PLAIN_RAN=1
	if [ -n "${2:-}" ]; then PLAIN_WANT=$2; elif [ -n "${1:-}" ]; then PLAIN_WANT=125; else PLAIN_WANT=0; fi
}
# plain_seen [WORD [STATUS]]: after a plain control's own runner invocation: its stderr is recorded, its target is plain_target and its intended status is declared
plain_seen() {
	rerr_seen
	plain_target "${1:-}"
	plain_declare "${1:-}" "${2:-}"
}
# ctl_begin: a fresh case for a plain control that reads host state directly (no runner run)
ctl_begin() {
	rg_reset
	reached
}
cname() { grep -o 'container=boxset-dev-run-[0-9a-f]\{16\}' "$TMP/rerr" | head -n 1 | cut -d= -f2; }
runid() { cname | sed 's/^boxset-dev-run-//'; }

# cstate_to SECONDS NAME: present | absent | unknown. Only an empty answer (absent) or the exact requested name
# (present) from a status-0 query is an observation; a failed or timed-out query and any other output are unknown,
# never absence.
cstate_to() {
	_cso=$(timeout "$1" docker ps -a --filter "name=^$2\$" --format '{{.Names}}' 2>/dev/null) || {
		printf unknown
		return
	}
	case $_cso in
	'') printf absent ;;
	"$2") printf present ;;
	*) printf unknown ;;
	esac
}
# cstate NAME: CST is present, absent or unknown (cstate_to); an unknown observation is never RED or GREEN evidence. It is recorded in the pre-created
# unknown record and, when that record cannot be written, as SETUP: this runs in the parent shell (never inside a command substitution that could drop
# the contamination), so a failed recording contaminates the case whoever reads CST, an assertion or only a diagnostic.
CST=
cstate() {
	CST=$(cstate_to 30 "$1")
	[ "$CST" != unknown ] || unknown_obs || SETUP=1
}
gone() { # gone NAME CONTEXT: the named owned container is absent by a status-checked query
	if [ -z "$1" ]; then
		fail "$2" "no container name was observed for this case"
		return
	fi
	cstate "$1"
	expect "$2" "$CST" absent
}

hexline() { printf 'ARG '; printf '%s' "$1" | od -An -v -tx1 | tr -d ' \n'; echo; }

# mutate_file FILE OLD NEW: exact-string replace of the first occurrence in place, read back: the
# written bytes must be the intended change (and a real change), else the construction failed.
mutate_file() {
	_mfc=$(cat "$1"; printf x)
	_mfc=${_mfc%x}
	case $_mfc in
	*"$2"*) ;;
	*)
		fail "mutant setup" "pattern not found in $1: $2"
		return 1
		;;
	esac
	_mfw="${_mfc%%"$2"*}$3${_mfc#*"$2"}"
	printf '%s' "$_mfw" >"$1"
	_mfr=$(cat "$1"; printf x)
	if [ "${_mfr%x}" != "$_mfw" ] || [ "$_mfw" = "$_mfc" ]; then
		fail "mutant setup" "the replacement was not read back as the intended change in $1: $2"
		return 1
	fi
}
# variant NAME [OLD NEW]...: a SHA-bound copy of the runner with injected seams. Every replacement must
# be constructed and read back; a partly built variant is removed and can never run or earn a RED.
variant() {
	_vnm=$1
	shift
	rm -f "$MUT/$_vnm.sh"
	cp "$SRC/scripts/dev-run.sh" "$MUT/$_vnm.sh"
	chmod +x "$MUT/$_vnm.sh"
	while [ $# -ge 2 ]; do
		if ! mutate_file "$MUT/$_vnm.sh" "$1" "$2"; then
			rm -f "$MUT/$_vnm.sh"
			return 1
		fi
		shift 2
	done
	return 0
}
# gvariant NAME FILE OLD NEW...: a SHA-bound compiled copy of the host helper (deploy/dev/redact), the only Go the runner executes (-check, -cap, -tail). A private
# context holds copies of the isolated checkout's .dockerignore, go.mod and deploy/dev/redact/; every replacement is applied to FILE (main.go) inside it, read
# back, and the changed file's SHA is recorded in $MUT/NAME.csha. $MUT/NAME.sh is a runner copy whose only change is the helper-build context operand, which now
# points at that context: callbacks keep their cwd (the isolated checkout) and the work image still builds from it. A partly built copy is removed and can never
# run or earn a RED; one that does not compile ends in redact_unavailable, which is infrastructure (INFRA_RE), never RED.
GCTX_OLD='--output "type=local,dest=$T/bin" . >/dev/null 2>&1 || die redact_unavailable'
gvariant() {
	_gvn=$1
	_gvf=$2
	shift 2
	rm -rf "$MUT/ctx-$_gvn" "$MUT/$_gvn.sh" "$MUT/$_gvn.csha"
	mkdir -p "$MUT/ctx-$_gvn/deploy/dev" || return 1
	if ! cp "$ISO/.dockerignore" "$ISO/go.mod" "$MUT/ctx-$_gvn/" || ! cp -R "$ISO/deploy/dev/redact" "$MUT/ctx-$_gvn/deploy/dev/"; then
		rm -rf "$MUT/ctx-$_gvn"
		return 1
	fi
	while [ $# -ge 2 ]; do
		if ! mutate_file "$MUT/ctx-$_gvn/deploy/dev/redact/$_gvf" "$1" "$2"; then
			rm -rf "$MUT/ctx-$_gvn"
			return 1
		fi
		shift 2
	done
	if ! fhash "$MUT/ctx-$_gvn/deploy/dev/redact/$_gvf" >"$MUT/$_gvn.csha"; then
		rm -rf "$MUT/ctx-$_gvn"
		return 1
	fi
	cp "$SRC/scripts/dev-run.sh" "$MUT/$_gvn.sh" && chmod +x "$MUT/$_gvn.sh" || return 1
	if ! mutate_file "$MUT/$_gvn.sh" "$GCTX_OLD" "--output \"type=local,dest=\$T/bin\" $MUT/ctx-$_gvn >/dev/null 2>&1 || die redact_unavailable"; then
		rm -f "$MUT/$_gvn.sh"
		return 1
	fi
}
# fiso NAME: an isolated checkout copy for Dockerfile/source fault copies; sets FISO.
fiso() {
	FISO=$TMP/f-$1
	rm -rf "$FISO"
	cp -a "$ISO" "$FISO"
	mkdir -p "$FISO/testdata" "$FISO/docs/spikes/issue-drafts"
}
# --- the shared classifier ----------------------------------------------------------------------------------
# Every RED and GREEN credit goes through rg_verdict: redgreen (runner, validator, redactor and S5 mutants),
# go_mutant (named Go tests), holds (single controls) and the host-loss results. A callback reports three things
# separately: REACHED (reached: set only after every prerequisite succeeded and the intended target observation
# was read), contamination (SETUP; an infrastructure failure on any runner stderr; an unknown Docker observation)
# and the assertion's own status. RED needs the target reached, the contamination clear and the assertion
# refused; GREEN needs the target reached, the contamination clear and the assertion held; every other
# combination is a failure, never RED or GREEN.
INFRA_RE='dev-run: (image_build_failed|docker_unavailable|redact_unavailable|host_tool_missing|capture_host_tool_missing|temp_dir_failed|not_repo_root|repo_path_unsupported)'
# CTRL_FATAL_RE: the controller's own panic or Go runtime fatal error prints its trace on the runner's stderr and exits 2 or 125. That text and
# its numeric status can never stand for an intended child result: it is infrastructure whatever the status coincides with.
CTRL_FATAL_RE='^(panic: |fatal error: |goroutine [0-9]+ \[|runtime: |\[signal )'
reached() { REACHED=1; }
# unknown_obs: record an unknown Docker observation as one appended byte of the pre-created record (it needs no new
# inode and crosses command substitutions); a record that could not be written fails and is never a clean observation
unknown_obs() { { printf u >>"$TMP/unknown"; } 2>/dev/null; }
# rerr_seen: called after every runner stderr capture; an infrastructure failure of any of several runners stays recorded
rerr_seen() {
	if grep -Eq "$INFRA_RE" "$TMP/rerr" 2>/dev/null || grep -Eq "$CTRL_FATAL_RE" "$TMP/rerr" 2>/dev/null; then { printf i >>"$TMP/infra"; } 2>/dev/null || SETUP=1; fi
}
# intended_refusal WORD FILE: FILE is the stderr of a run whose intended outcome is the fixed refusal WORD (an infrastructure word). Only that exact
# line is exempt from infrastructure attribution: the rest of the stream is judged by rerr_seen, so an earlier redactor, host-tool or setup failure stays
# contamination. Succeeds when the word was spoken; the caller adds its own positive observation of the injected step.
intended_refusal() {
	grep -Fxv "dev-run: $1" "$2" >"$TMP/rerr" 2>/dev/null
	[ $? -le 1 ] || SETUP=1
	rerr_seen
	grep -Fxq "dev-run: $1" "$2" 2>/dev/null
}
infra_failure() { [ -s "$TMP/infra" ] || grep -Eq "$INFRA_RE" "$TMP/rerr" 2>/dev/null || grep -Eq "$CTRL_FATAL_RE" "$TMP/rerr" 2>/dev/null; }
s5_done() { grep -Eq '^S5-(ADMISSION-(PASS|FAIL)|MANIFEST-ONLY-PASS) ' "$TMP/rout"; }
rg_reset() {
	OBS=
	SETUP=0
	REACHED=0
	CASE_FAILED=0
	PLAIN_RAN=0
	PLAIN_WANT=
	{ true >"$TMP/unknown" && true >"$TMP/infra" && true >"$TMP/rerr"; } 2>/dev/null || SETUP=1
}
# rg_verdict ROLE STATUS: ROLE red (the assertion must have been refused on a mutant) or green (it must hold on
# the original or a fault copy); STATUS is the callback's assertion status. Credited only when it returns 0;
# RG_WHY names every reason otherwise.
rg_verdict() {
	RG_WHY=
	[ "$REACHED" = 1 ] || RG_WHY="$RG_WHY the intended target was never reached;"
	[ "$SETUP" = 0 ] || RG_WHY="$RG_WHY a setup prerequisite failed;"
	! infra_failure || RG_WHY="$RG_WHY an infrastructure failure was recorded;"
	[ ! -s "$TMP/unknown" ] || RG_WHY="$RG_WHY an unknown Docker observation was recorded;"
	[ "$CASE_FAILED" = 0 ] || RG_WHY="$RG_WHY an earlier assertion of this case failed;"
	[ "$PLAIN_RAN" = 0 ] || { [ -n "$PLAIN_WANT" ] && [ "${RS:-}" = "$PLAIN_WANT" ]; } || RG_WHY="$RG_WHY the runner status ${RS:-none} is not the intended status ${PLAIN_WANT:-none} of this plain run;"
	case $1:$2 in
	red:0) RG_WHY="$RG_WHY the assertion held on the mutant;" ;;
	green:0) ;;
	green:*) RG_WHY="$RG_WHY the assertion failed on the original;" ;;
	red:*) ;;
	*) RG_WHY="$RG_WHY unknown role;" ;;
	esac
	[ -z "$RG_WHY" ]
}
# redgreen NAME FN RED_RUNNER GREEN_RUNNER: the same named assertion FN must be refused on the SHA-bound mutant
# (RED) and hold on the original or fault copy (GREEN), each through rg_verdict.
redgreen() {
	_rgn=$1
	_rgf=$2
	_rgr=$3
	_rgg=$4
	if [ ! -f "$_rgr" ] || [ ! -f "$_rgg" ]; then
		fail "$_rgn" "the mutant or the original was not constructed (setup failed above): $_rgr / $_rgg"
		return
	fi
	_rgs=$(sha256sum <"$_rgr")
	_rgcs=$(cat "${_rgr%.sh}.csha" 2>/dev/null)
	rg_reset
	_rgc=0
	"$_rgf" "$_rgr" || _rgc=$?
	if rg_verdict red "$_rgc"; then pass "$_rgn RED on mutant sha ${_rgs%% *}${_rgcs:+ (controller sha $_rgcs)} (observed: $OBS)"; else fail "$_rgn (mutant sha ${_rgs%% *}${_rgcs:+, controller sha $_rgcs})" "$RG_WHY $OBS"; fi
	rg_reset
	_rgc=0
	"${RG_GREEN_FN:-$_rgf}" "$_rgg" || _rgc=$?
	if rg_verdict green "$_rgc"; then pass "$_rgn GREEN on original"; else fail "$_rgn original" "$RG_WHY $OBS"; fi
}
# holds NAME FN ARGS...: one GREEN-only control (a fault copy that must be refused, a plain assertion) through
# the same classifier
holds() {
	_hn=$1
	_hf=$2
	shift 2
	rg_reset
	_hc=0
	"$_hf" "$@" || _hc=$?
	if rg_verdict green "$_hc"; then pass "$_hn"; else fail "$_hn" "$RG_WHY $OBS"; fi
}
# --- the monotonic clock ------------------------------------------------------------------------------------------
# Durations and deadlines use CLOCK_MONOTONIC through the shared `redact -clock` helper ("SECONDS.CC" floored to 10 ms), never wall-clock
# nanoseconds, which can step backward, and never /proc/uptime. MONO_NS is a lower bound: the true time is below MONO_NS + MONO_RES.
# shellcheck disable=SC2034 # used by the host-loss watcher and the closure-window oracle
MONO_RES=10000000
MONO_LAST=0
mono_parse() { # mono_parse TEXT: MONO_NS is the clock text in nanoseconds; a malformed text fails
	case $1 in [0-9]*.[0-9][0-9]) ;; *) return 1 ;; esac
	_mpi=${1%.??}
	_mpf=${1#"$_mpi".}
	case $_mpi$_mpf in *[!0-9]*) return 1 ;; esac
	_mpc=$_mpi$_mpf
	while case $_mpc in 0?*) true ;; *) false ;; esac; do _mpc=${_mpc#0}; done
	MONO_NS=$((_mpc * 10000000))
}
mono_ns() { # mono_ns: one reading of the shared CLOCK_MONOTONIC helper (`redact -clock`, floored to 10 ms); a failed, timed-out, malformed or backward reading fails and is never a time. There is no fallback clock.
	[ -n "$CLOCK" ] && [ -x "$CLOCK" ] || return 1
	_mt=$(timeout 2 "$CLOCK" -clock </dev/null 2>/dev/null) || return 1
	mono_parse "$_mt" || return 1
	[ "$MONO_NS" -ge "$MONO_LAST" ] || return 1
	MONO_LAST=$MONO_NS
}
# ns_budget NS: BUDGET is NS as the whole-seconds.milliseconds text a query budget takes, floored (never above NS)
ns_budget() {
	_nbm=$(($1 / 1000000))
	_nbf=$((_nbm % 1000))
	case $_nbf in ?) _nbf=00$_nbf ;; ??) _nbf=0$_nbf ;; esac
	BUDGET=$((_nbm / 1000)).$_nbf
}
# --- host-loss results: case-owned deferred state ---------------------------------------------------------------------
# Each registered case owns its contamination (launcher, readiness polling, runner stderr, unknown queries) in
# $HLD/KEY.taint; the deferred verdict loads it and never resets it away. A taint that could not even be written is kept
# in the shell (HL_TAINT_FAIL).
HLD=$TMP/hl
HL_TAINT_FAIL=
hl_taint() { # hl_taint KEY unknown|setup DETAIL
	{ printf '%s %s\n' "$2" "$3" >>"$HLD/$1.taint"; } 2>/dev/null || HL_TAINT_FAIL="$HL_TAINT_FAIL $1"
}
# shellcheck disable=SC2034 # HR_T and HR_D are the reading and deadline bound of a deferred result, set for the reporters of a host-loss proof
hl_result() { # hl_result KEY: sets HR_R HR_T HR_D (missing: never registered; unobserved: the watcher never read its registration)
	HR_R=missing
	HR_T=0
	HR_D=0
	if [ -s "$HLD/$1.res" ]; then
		read -r HR_R HR_T HR_D <"$HLD/$1.res"
	elif [ -e "$HLD/$1.reg" ] && [ ! -e "$HLD/$1.seen" ]; then
		HR_R=unobserved
	fi
}
hl_flags() { # hl_flags KEY: the result and the case's own contamination as classifier state. Only gone (observed absent inside
	# its own deadline) and expired-present (observed present by a query that started at or after it) are target
	# observations; an unknown result, an unobserved tail (late-gone) and every taint are contamination.
	hl_result "$1"
	rg_reset
	case $HR_R in
	gone | expired-present) reached ;;
	unknown | late-gone) unknown_obs || SETUP=1 ;;
	*) SETUP=1 ;;
	esac
	if [ -s "$HLD/$1.taint" ]; then
		while read -r _tk _td; do
			case $_tk in unknown) unknown_obs || SETUP=1 ;; *) SETUP=1 ;; esac
		done <"$HLD/$1.taint"
	fi
	case " $HL_TAINT_FAIL " in *" $1 "*) SETUP=1 ;; esac
}
hl_credit() { # hl_credit green|red KEY: the classifier verdict of one deferred host-loss result (the assertion holds iff gone)
	hl_flags "$2"
	_hcs=1
	[ "$HR_R" != gone ] || _hcs=0
	rg_verdict "$1" "$_hcs"
}
hl_terminal() { # hl_terminal STATE: the first status-checked query that started at or after the case's own deadline
	case $1 in
	present) printf expired-present ;;
	absent) printf late-gone ;;
	*) printf unknown ;;
	esac
}
# --- the trusted supervisor hash oracle ----------------------------------------------------------------------
# Four operands, four records: 64 lowercase hex, two spaces and that exact operand, in order, from a status-0 query.
SUP_HASH_PATHS='/usr/bin/stat /usr/bin/sh /usr/bin/cat /usr/local/share/boxset/inputs.sha256'
hash_records_ok() { # hash_records_ok TEXT
	_hrw=$SUP_HASH_PATHS
	_hrn=0
	while IFS= read -r _hrl; do
		_hrn=$((_hrn + 1))
		_hrd=${_hrl%%  *}
		set -- $_hrw
		[ $# -ge 1 ] || return 1
		[ "${#_hrd}" = 64 ] || return 1
		case $_hrd in *[!0-9a-f]*) return 1 ;; esac
		[ "$_hrl" = "$_hrd  $1" ] || return 1
		shift
		_hrw=$*
	done <<EOF
$1
EOF
	[ "$_hrn" = 4 ] && [ -z "$_hrw" ]
}
sup_hashes() { # sup_hashes CTR: prints the four validated records; nonzero and silent on a failed query or any malformed record
	_sh=$(timeout 30 docker exec -u 65534:65534 "$1" /usr/bin/sha256sum $SUP_HASH_PATHS 2>/dev/null) || return 1
	hash_records_ok "$_sh" || return 1
	printf '%s\n' "$_sh"
}
hr_expect() { expect "hash oracle: $1" "$(hash_records_ok "$2" && echo accepted || echo rejected)" "$3"; } # hr_expect NAME TEXT accepted|rejected
oracle_self_check() {
	_od=$(printf '%064d' 0)
	_o1="$_od  /usr/bin/stat"
	_o2="$_od  /usr/bin/sh"
	_o3="$_od  /usr/bin/cat"
	_o4="$_od  /usr/local/share/boxset/inputs.sha256"
	_nl='
'
	hr_expect "the four exact records are accepted" "$_o1$_nl$_o2$_nl$_o3$_nl$_o4" accepted
	hr_expect "three records are rejected" "$_o1$_nl$_o2$_nl$_o3" rejected
	hr_expect "five records are rejected" "$_o1$_nl$_o2$_nl$_o3$_nl$_o4$_nl$_o4" rejected
	hr_expect "a wrong operand path is rejected" "$_o1$_nl$_od  /usr/bin/zz$_nl$_o3$_nl$_o4" rejected
	hr_expect "records out of order are rejected" "$_o2$_nl$_o1$_nl$_o3$_nl$_o4" rejected
	hr_expect "a 63-digit digest is rejected" "$_o1$_nl$(printf '%063d' 0)  /usr/bin/sh$_nl$_o3$_nl$_o4" rejected
	hr_expect "an uppercase digest is rejected" "$_o1$_nl$(printf '%064d' 0 | tr 0 A)  /usr/bin/sh$_nl$_o3$_nl$_o4" rejected
	hr_expect "a one-space separator is rejected" "$_o1$_nl$_od /usr/bin/sh$_nl$_o3$_nl$_o4" rejected
	hr_expect "trailing text after an operand is rejected" "$_o1$_nl$_o2$_nl$_o3$_nl$_o4 x" rejected
	hr_expect "empty output is rejected" "" rejected
}
# --- the permanent classifier self-check ---------------------------------------------------------------------
# `scripts/dev-run-test.sh --classifier-self-check` runs only this (no container, no build) and the full proof
# begins with it. Ten outcome controls go through the real redgreen and rg_verdict with a synthetic target that
# sets exactly the state it describes through the real flag seams (reached, SETUP, a runner stderr infrastructure
# line, the unknown-observation flag): an unreached refusal and success are rejected; a reached refusal and
# success with setup, infrastructure or unknown set independently are rejected; only a clean reached refusal is
# RED and a clean reached success GREEN. Each guard is then removed from the extracted real rg_verdict text and
# the same five pairs must credit exactly that guard's two controls.
G_REACH='[ "$REACHED" = 1 ] || RG_WHY="$RG_WHY the intended target was never reached;"'
G_SETUP='[ "$SETUP" = 0 ] || RG_WHY="$RG_WHY a setup prerequisite failed;"'
G_INFRA='! infra_failure || RG_WHY="$RG_WHY an infrastructure failure was recorded;"'
G_UNK='[ ! -s "$TMP/unknown" ] || RG_WHY="$RG_WHY an unknown Docker observation was recorded;"'
G_CASE='[ "$CASE_FAILED" = 0 ] || RG_WHY="$RG_WHY an earlier assertion of this case failed;"'
G_PLAIN='[ "$PLAIN_RAN" = 0 ] || { [ -n "$PLAIN_WANT" ] && [ "${RS:-}" = "$PLAIN_WANT" ]; } || RG_WHY="$RG_WHY the runner status ${RS:-none} is not the intended status ${PLAIN_WANT:-none} of this plain run;"'
sc_state() { # the synthetic case state: exactly what SC_REACH SC_SETUP SC_INFRA SC_UNK describe, through the real flag seams
	[ "$SC_REACH" = 0 ] || reached
	[ "$SC_SETUP" = 0 ] || SETUP=1
	if [ "$SC_INFRA" != 0 ]; then
		printf 'dev-run: image_build_failed\n' >"$TMP/rerr"
		rerr_seen
	fi
	[ "$SC_UNK" = 0 ] || unknown_obs
}
sc_cb() { # the synthetic target: the mutant runner refuses (status 1), the original holds (status 0)
	sc_state
	case $1 in *sc-mut) return 1 ;; *) return 0 ;; esac
}
sc_credits() { # sc_credits NAME REACH SETUP INFRA UNK: SC_R / SC_G are 1 when the classifier credited RED / GREEN
	SC_REACH=$2
	SC_SETUP=$3
	SC_INFRA=$4
	SC_UNK=$5
	_scc=$(redgreen "selfcheck $1" sc_cb "$TMP/sc-mut" "$TMP/sc-orig")
	SC_R=0
	SC_G=0
	case $_scc in *"selfcheck $1 RED on mutant"*) SC_R=1 ;; esac
	case $_scc in *"selfcheck $1 GREEN on original"*) SC_G=1 ;; esac
}
sc_pairs() { # the five outcome pairs as "name:RG ..." (R, G are the credited flags)
	_scp=
	for _scx in 'unreached 0 0 0 0' 'setup 1 1 0 0' 'infra 1 0 1 0' 'unknown 1 0 0 1' 'clean 1 0 0 0'; do
		set -- $_scx
		sc_credits "$@"
		_scp="$_scp $1:$SC_R$SC_G"
	done
	printf '%s' "${_scp# }"
}
sc_pick() { # sc_pick RESULT NAME R|G: the credited flag of NAME's refusal (R) or success (G) control
	for _spk in $1; do
		[ "${_spk%%:*}" = "$2" ] || continue
		_spv=${_spk#*:}
		case $3 in R) printf '%s' "${_spv%?}" ;; G) printf '%s' "${_spv#?}" ;; esac
	done
}
sc_plain() { # sc_plain REACH SETUP INFRA UNK [WANT RS [FILE]]: 1 when the real plain `pass` credited a control in that state; WANT is the declared intended status (none: the run was made but its status never declared), RS the real one, FILE a perturbed rg_verdict
	_spo=$(
		META=0
		SC_REACH=$1 SC_SETUP=$2 SC_INFRA=$3 SC_UNK=$4
		rg_reset
		sc_state
		if [ -n "${5:-}" ]; then
			PLAIN_RAN=1
			PLAIN_WANT=$5
			[ "$5" != none ] || PLAIN_WANT=
			RS=$6
		fi
		[ -z "${7:-}" ] || . "$7"
		pass "plain control"
	)
	case $_spo in "ok - plain control") printf 1 ;; *) printf 0 ;; esac
}
plain_self_check() {
	sed -n '/^rg_verdict() {$/,/^}$/p' "$SELF" >"$TMP/plain-rgv.sh"
	cp "$TMP/plain-rgv.sh" "$TMP/plain-mut.sh"
	mutate_file "$TMP/plain-mut.sh" "$G_PLAIN" ':' || return
	expect "classifier self-check plain 1/5: a plain credit whose target was never reached is refused" "$(sc_plain 0 0 0 0)" 0
	expect "classifier self-check plain 2/5: a reached plain credit with a setup failure is refused" "$(sc_plain 1 1 0 0)" 0
	expect "classifier self-check plain 3/5: a reached plain credit with an infrastructure failure is refused" "$(sc_plain 1 0 1 0)" 0
	expect "classifier self-check plain 4/5: a reached plain credit with an unknown observation is refused" "$(sc_plain 1 0 0 1)" 0
	expect "classifier self-check plain 5/5: a clean reached plain credit is credited" "$(sc_plain 1 0 0 0)" 1
	expect "classifier self-check plain status 1/4: correct bytes with a late failure status (125 where 0 is intended) are refused" "$(sc_plain 1 0 0 0 0 125)" 0
	expect "classifier self-check plain status 2/4: the intended status held is credited" "$(sc_plain 1 0 0 0 0 0)" 1
	expect "classifier self-check plain status 3/4: another nonzero status than the intended one (7 wanted, 1 seen) is refused" "$(sc_plain 1 0 0 0 7 1)" 0
	expect "classifier self-check plain status 4/4: a plain run whose intended status was never declared is refused" "$(sc_plain 1 0 0 0 none 0)" 0
	expect "classifier self-check plain status: the gate-removed copy of the real classifier credits the wrong-status row, which counts as caught" "$(sc_caught "$(sc_plain 1 0 0 0 0 125 "$TMP/plain-mut.sh")" 1)" caught
}
sc_clock() { # sc_clock MODE TEXT PREV [FILE]: MONO_NS the real mono_ns reads from a CLOCK stub (MODE text: prints TEXT; fail: exits 1; hang: outlives its 2 s bound; none: CLOCK empty) after a previous reading PREV, or fail
	(
		_scd=$TMP/sck
		rm -rf "$_scd"
		mkdir -p "$_scd" || exit 1
		case $1 in
		text) printf '#!/bin/sh\nprintf "%%s\\n" "%s"\n' "$2" >"$_scd/clock" ;;
		fail) printf '#!/bin/sh\nexit 1\n' >"$_scd/clock" ;;
		hang) printf '#!/bin/sh\nsleep 5\nprintf "1.00\\n"\n' >"$_scd/clock" ;;
		esac
		chmod +x "$_scd/clock" 2>/dev/null
		CLOCK=$_scd/clock
		[ "$1" != none ] || CLOCK=
		[ -z "${4:-}" ] || . "$4"
		MONO_LAST=$3
		if mono_ns; then printf '%s' "$MONO_NS"; else printf fail; fi
	)
}
clock_self_check() {
	if ! sc_extract "$TMP/mono-real.sh" mono_ns; then
		fail "classifier self-check" "mono_ns could not be extracted from $SELF"
		return
	fi
	cp "$TMP/mono-real.sh" "$TMP/mono-mut.sh"
	mutate_file "$TMP/mono-mut.sh" '	_mt=$(timeout 2 "$CLOCK" -clock </dev/null 2>/dev/null) || return 1' '	if [ -z "$CLOCK" ]; then _mt=$(cut -d" " -f1 /proc/uptime) || return 1; else _mt=$(timeout 2 "$CLOCK" -clock </dev/null 2>/dev/null) || return 1; fi' || return
	mutate_file "$TMP/mono-mut.sh" '	[ -n "$CLOCK" ] && [ -x "$CLOCK" ] || return 1' ':' || return
	expect "classifier self-check clock: a well-formed helper reading is the floored text in nanoseconds" "$(sc_clock text 50879.64 0)" 50879640000000
	expect "classifier self-check clock: leading zeros are not octal" "$(sc_clock text 0.05 0)" 50000000
	expect "classifier self-check clock: a failed helper is not a time" "$(sc_clock fail '' 0)" fail
	expect "classifier self-check clock: a helper that outlives its 2 s bound (status 124) is not a time" "$(sc_clock hang '' 0)" fail
	expect "classifier self-check clock: text that is not digits.2digits is not a time" "$(sc_clock text 12.3 0):$(sc_clock text abc 0):$(sc_clock text '' 0):$(sc_clock text 1.2.34 0):$(sc_clock text 12.345 0):$(sc_clock text -1.00 0)" "fail:fail:fail:fail:fail:fail"
	expect "classifier self-check clock: a backward reading is not a time" "$(sc_clock text 12.30 12310000000)" fail
	expect "classifier self-check clock: an equal reading is accepted" "$(sc_clock text 12.31 12310000000)" 12310000000
	expect "classifier self-check clock: with CLOCK empty the real reader fails" "$(sc_clock none '' 0)" fail
	expect "classifier self-check clock: a /proc/uptime fallback copy completes a numeric reading with CLOCK empty, which counts as caught" "$(sc_isnum "$(sc_clock none '' 0 "$TMP/mono-mut.sh")")" caught
}
# --- the case latch (B01) ---------------------------------------------------------------------------------------------------------
# A case is one rg_reset scope. `fail` sets CASE_FAILED, rg_reset clears it and rg_verdict refuses every credit while it is set, so
# an intended outcome that failed can never be followed by a credited dependent control of the same case.
sc_latch() { # sc_latch ROW [RGV_FILE]: 1 when the real pass credited the dependent control of ROW
	_slo=$(
		META=0
		rg_reset
		reached
		[ -z "${2:-}" ] || . "$2"
		case $1 in
		failed) fail "intended outcome" "refused" >/dev/null ;;
		held) pass "intended outcome" >/dev/null ;;
		reset)
			fail "intended outcome" "refused" >/dev/null
			rg_reset
			reached
			;;
		esac
		pass "dependent control"
	)
	case $_slo in "ok - dependent control") printf 1 ;; *) printf 0 ;; esac
}
latch_self_check() {
	if ! sc_extract "$TMP/latch-rgv.sh" rg_verdict; then
		fail "classifier self-check" "rg_verdict could not be extracted from $SELF"
		return
	fi
	cp "$TMP/latch-rgv.sh" "$TMP/latch-mut.sh"
	mutate_file "$TMP/latch-mut.sh" "$G_CASE" ':' || return
	expect "classifier self-check latch 1/3: a failed intended outcome followed by a dependent credit is refused" "$(sc_latch failed)" 0
	expect "classifier self-check latch 2/3: a held intended outcome followed by a dependent credit is credited" "$(sc_latch held)" 1
	expect "classifier self-check latch 3/3: the next case after rg_reset is credited" "$(sc_latch reset)" 1
	expect "classifier self-check latch: the latch-removed copy credits the failed row, which counts as caught" "$([ "$(sc_latch failed "$TMP/latch-mut.sh")" = 1 ] && echo caught || echo missed)" caught
}
# --- the lint target (B02) ------------------------------------------------------------------------------------------------------------
# s5_lines LINE_RE: every blocked or unlisted line of the admission output is one the intended outcome names (a full-line ERE prefix); any other row
# is an unrelated failure: setup, never the target's outcome. An output with no such line (a PASS) holds vacuously.
s5_lines() {
	if grep -E '^ADMISSION-(BLOCKED|UNLISTED) ' "$TMP/rout" | grep -Ev "^($1)" | grep -q .; then
		OBS="$OBS; a blocked or unlisted row outside the intended outcome"
		SETUP=1
		return 1
	fi
	return 0
}
RA_LINT_ROW='ADMISSION-BLOCKED golangci-lint: planted printf defect not rejected with a named diagnostic \(status 0\):'
# ra_target: the lint target of the real admission is reached in exactly two shapes: (a) the CONTROL golangci-lint line with the PASS summary and no
# blocked or unlisted row; (b) the planted-defect block as the only blocked or unlisted row with S5-ADMISSION-FAIL blocked=1 unlisted=0 and status 1.
# Any other blocked or unlisted row is setup through s5_lines.
ra_target() {
	s5_lines "$RA_LINT_ROW" || return 1
	if grep -Eq '^CONTROL golangci-lint ' "$TMP/rout" && grep -Eq '^S5-ADMISSION-PASS blocked=0 unlisted=0 rows=' "$TMP/rout"; then
		reached
		return 0
	fi
	if [ "$(grep -Ec '^ADMISSION-(BLOCKED|UNLISTED) ' "$TMP/rout")" = 1 ] && grep -Eq "^$RA_LINT_ROW" "$TMP/rout" && grep -Eq '^S5-ADMISSION-FAIL blocked=1 unlisted=0' "$TMP/rout" && [ "${RS:-}" = 1 ]; then
		reached
		return 0
	fi
	return 1
}
sc_ra() { # sc_ra ROW [FILE]: reached, setup or no for one synthetic admission output through the real ra_target and s5_lines
	(
		TMP=$TMP/scra
		rm -rf "$TMP"
		mkdir -p "$TMP" || exit 1
		rg_reset
		RS=0
		case $1 in
		controlpass) printf 'CONTROL golangci-lint 2.14.0\nS5-ADMISSION-PASS blocked=0 unlisted=0 rows=3\n' >"$TMP/rout" ;;
		planted)
			printf 'ADMISSION-BLOCKED golangci-lint: planted printf defect not rejected with a named diagnostic (status 0): x\nS5-ADMISSION-FAIL blocked=1 unlisted=0\n' >"$TMP/rout"
			RS=1
			;;
		controlunrelated)
			printf 'CONTROL golangci-lint 2.14.0\nADMISSION-BLOCKED ADM-099: category_invalid\nS5-ADMISSION-FAIL blocked=1 unlisted=0\n' >"$TMP/rout"
			RS=1
			;;
		plantedunrelated)
			printf 'ADMISSION-BLOCKED golangci-lint: planted printf defect not rejected with a named diagnostic (status 0): x\nADMISSION-BLOCKED ADM-099: category_invalid\nS5-ADMISSION-FAIL blocked=2 unlisted=0\n' >"$TMP/rout"
			RS=1
			;;
		esac
		[ -z "${2:-}" ] || . "$2"
		ra_target >/dev/null
		if [ "$REACHED" = 1 ] && [ "$SETUP" = 0 ]; then printf reached; elif [ "$SETUP" = 1 ]; then printf setup; else printf no; fi
	)
}
SC_RA_WANT='controlpass:reached planted:reached controlunrelated:setup plantedunrelated:setup'
sc_ram() { # sc_ram [FILE]
	printf 'controlpass:%s planted:%s controlunrelated:%s plantedunrelated:%s' "$(sc_ra controlpass "${1:-}")" "$(sc_ra planted "${1:-}")" "$(sc_ra controlunrelated "${1:-}")" "$(sc_ra plantedunrelated "${1:-}")"
}
lint_self_check() {
	if ! sc_extract "$TMP/ra-real.sh" ra_target; then
		fail "classifier self-check" "ra_target could not be extracted from $SELF"
		return
	fi
	cp "$TMP/ra-real.sh" "$TMP/ra-mut.sh"
	mutate_file "$TMP/ra-mut.sh" '	if grep -Eq '"'"'^CONTROL golangci-lint '"'"' "$TMP/rout" && grep -Eq '"'"'^S5-ADMISSION-PASS blocked=0 unlisted=0 rows='"'"' "$TMP/rout"; then' '	if grep -Eq '"'"'^CONTROL golangci-lint '"'"' "$TMP/rout"; then' || return
	mutate_file "$TMP/ra-mut.sh" '	s5_lines "$RA_LINT_ROW" || return 1' ':' || return
	expect "classifier self-check lint target: the control with the PASS summary and the planted defect alone are reached; an unrelated block beside either is setup" "$(sc_ram)" "$SC_RA_WANT"
	expect "classifier self-check lint target: the copy with the unrelated-row filter removed reaches the control beside an unrelated block, which counts as caught" "$([ "$(sc_ra controlunrelated "$TMP/ra-mut.sh")" = reached ] && echo caught || echo missed)" caught
}
# --- clock acquisition and its recovery diagnostic ---------------------------------------------------------------------------------------
# The full driver acquires the shared CLOCK_MONOTONIC helper once, after the isolated checkout exists and before the first runner case: the same
# host-redact build the runner makes, exported into a private 0700 temp, then one valid reading. Any failure is status 125 with no credits and one
# fixed, secret-safe line naming the phase and the runner-notes recovery; raw build, environment and URL output never reach it.
clock_diag() { # clock_diag PHASE
	printf 'dev-run-test: clock unavailable phase=%s recovery=docs/spikes/s5-version-admission.md (runner notes: scripts/dev-run.sh -- true shows the redacted bootstrap diagnostics; the clock checks are never bypassed)\n' "$1" >&2
}
clock_build() { # clock_build DEST: the host-redact build of the isolated checkout, exactly the runner's
	(cd "$ISO" && docker build --file deploy/dev/Dockerfile --target host-redact --output "type=local,dest=$1" .)
}
clock_acquire() { # clock_acquire BUILDER...: BUILDER DEST builds and exports the helper (phase acquire); one valid reading is the probe (phase probe); sets CLOCK
	_cad=$TMP/clock-bin
	rm -rf "$_cad"
	mkdir -m 0700 "$_cad" || { clock_diag acquire; return 1; }
	"$@" "$_cad" >/dev/null 2>&1 || { clock_diag acquire; return 1; }
	{ [ -f "$_cad/redact" ] && [ -x "$_cad/redact" ]; } || { clock_diag acquire; return 1; }
	CLOCK=$_cad/redact
	MONO_LAST=0
	mono_ns || { CLOCK=; clock_diag probe; return 1; }
	return 0
}
sc_clockacq() { # sc_clockacq KIND [FILE]: the status, the diagnostic and the CLOCK of the real clock_acquire over a stub builder; the stub's own output carries a credential-bearing URL that must never surface
	(
		_scd=$TMP/sca
		rm -rf "$_scd"
		mkdir -p "$_scd" || exit 1
		cat >"$_scd/build" <<'BUILD'
#!/bin/sh
echo "build output https://user:pw@h.example/x?token=tt" >&2
echo "build output https://user:pw@h.example/x?token=tt"
case $SCA_KIND in
fail) exit 1 ;;
missing) exit 0 ;;
esac
printf '#!/bin/sh\n' >"$1/redact"
case $SCA_KIND in
garbage) printf 'echo abc\n' >>"$1/redact" ;;
failing) printf 'exit 1\n' >>"$1/redact" ;;
ok) printf 'echo 7.25\n' >>"$1/redact" ;;
esac
case $SCA_KIND in unusable) ;; *) chmod +x "$1/redact" ;; esac
exit 0
BUILD
		chmod +x "$_scd/build"
		TMP=$_scd
		SCA_KIND=$1
		export SCA_KIND
		CLOCK=
		[ -z "${2:-}" ] || . "$2"
		clock_acquire "$_scd/build" >/dev/null 2>"$_scd/diag"
		_scs=$?
		_sco=$(cat "$_scd/diag")
		case $_sco in *pw@* | *token=tt* | *"build output"*) _scs="$_scs-leak" ;; esac
		printf '%s|%s|%s' "$_scs" "$_sco" "${CLOCK:+clock}"
	)
}
clockdiag_self_check() {
	if ! sc_extract "$TMP/clockacq-real.sh" clock_diag clock_acquire; then
		fail "classifier self-check" "clock_diag or clock_acquire could not be extracted from $SELF"
		return
	fi
	_cdp='dev-run-test: clock unavailable phase='
	_cdr=' recovery=docs/spikes/s5-version-admission.md (runner notes: scripts/dev-run.sh -- true shows the redacted bootstrap diagnostics; the clock checks are never bypassed)'
	expect "classifier self-check clock acquisition: a failed build is phase=acquire with the recovery pointer and no raw output" "$(sc_clockacq fail)" "1|${_cdp}acquire${_cdr}|"
	expect "classifier self-check clock acquisition: a build that exports no binary is phase=acquire" "$(sc_clockacq missing)" "1|${_cdp}acquire${_cdr}|"
	expect "classifier self-check clock acquisition: an unusable (not executable) binary is phase=acquire" "$(sc_clockacq unusable)" "1|${_cdp}acquire${_cdr}|"
	expect "classifier self-check clock acquisition: malformed clock output fails the probe, phase=probe" "$(sc_clockacq garbage)" "1|${_cdp}probe${_cdr}|"
	expect "classifier self-check clock acquisition: a failing clock binary fails the probe, phase=probe" "$(sc_clockacq failing)" "1|${_cdp}probe${_cdr}|"
	expect "classifier self-check clock acquisition: a valid reading leaves the ordinary path unchanged (status 0, no diagnostic, CLOCK set)" "$(sc_clockacq ok)" "0||clock"
	cp "$TMP/clockacq-real.sh" "$TMP/clockacq-nophase.sh"
	cp "$TMP/clockacq-real.sh" "$TMP/clockacq-nopointer.sh"
	mutate_file "$TMP/clockacq-nophase.sh" 'phase=%s recovery=' 'recovery=' || return
	mutate_file "$TMP/clockacq-nopointer.sh" ' recovery=docs/spikes/s5-version-admission.md (runner notes: scripts/dev-run.sh -- true shows the redacted bootstrap diagnostics; the clock checks are never bypassed)' '' || return
	expect "classifier self-check clock acquisition: a diagnostic without its phase is exactly the phase-less recovery line, which counts as caught" "$(sc_caught "$(sc_clockacq garbage "$TMP/clockacq-nophase.sh")" "1|dev-run-test: clock unavailable${_cdr}|")" caught
	expect "classifier self-check clock acquisition: a diagnostic without its recovery pointer is exactly the bare phase line, which counts as caught" "$(sc_caught "$(sc_clockacq garbage "$TMP/clockacq-nopointer.sh")" "1|${_cdp}probe|")" caught
}
sc_hl() { # sc_hl WORD [TAINT]: "RG" with n for a verdict not credited: R is RED credited, G is GREEN credited for the deferred result WORD
	_sho=$(
		HLD=$TMP/hlsc
		HL_TAINT_FAIL=
		rm -rf "$HLD"
		mkdir -p "$HLD"
		printf '%s 1 2\n' "$1" >"$HLD/k.res"
		[ -z "${2:-}" ] || hl_taint k "$2" injected
		if hl_credit red k; then printf R; else printf n; fi
		if hl_credit green k; then printf G; else printf n; fi
	)
	printf '%s' "$_sho"
}
hl_self_check() {
	expect "classifier self-check host loss 1/9: absence observed inside the deadline is GREEN only" "$(sc_hl gone)" nG
	expect "classifier self-check host loss 2/9: survival observed by a query at or after the deadline is RED only" "$(sc_hl expired-present)" Rn
	expect "classifier self-check host loss 3/9: absence first seen after the deadline (the unobserved tail) is neither RED nor GREEN" "$(sc_hl late-gone)" nn
	expect "classifier self-check host loss 4/9: an unknown observation is neither RED nor GREEN" "$(sc_hl unknown)" nn
	expect "classifier self-check host loss 5/9: a result never observed by the watcher is neither" "$(sc_hl unobserved)" nn
	expect "classifier self-check host loss 6/9: absence inside the deadline with an earlier unknown query of the same case is not GREEN" "$(sc_hl gone unknown)" nn
	expect "classifier self-check host loss 7/9: survival with an earlier unknown query of the same case is not RED" "$(sc_hl expired-present unknown)" nn
	expect "classifier self-check host loss 8/9: absence inside the deadline with a failed launcher or readiness of the same case is not GREEN" "$(sc_hl gone setup)" nn
	expect "classifier self-check host loss 9/9: the terminal query states map to survivor, unobserved tail and unknown" "$(hl_terminal present):$(hl_terminal absent):$(hl_terminal unknown)" "expired-present:late-gone:unknown"
}
# --- permanent controls of the actual producers --------------------------------------------------------------------------
# Plain-control targets, parent-owned unknown observations, readiness probes, the cross-clock registration, the host-loss watcher, the bounded terminal
# helper and the natural-cell receipts. The real functions are extracted from this file (sc_extract) and run in a subshell; the time-dependent ones run on a
# virtual clock (sim_install: mono_ns, sleep and date read and advance one file, the query is a scenario stub with its own latency), so no control depends on
# host scheduling. Each real producer is then perturbed against the same assertions: a perturbation must change the outcomes.
sc_extract() { # sc_extract OUT NAME...: the real driver text of each named multi-line function, from this file; fails when one is missing
	_sce=$1
	shift
	: >"$_sce" || return 1
	for _scn in "$@"; do
		sed -n "/^$_scn() {/,/^}\$/p" "$SELF" >>"$_sce"
		grep -q "^$_scn() {" "$_sce" || return 1
	done
}
sc_rg() { # sc_rg NAME OUTPUT: R then G as 1 or 0: whether the redgreen OUTPUT credits RED on the mutant and GREEN on the original
	case $2 in *"$1 RED on mutant"*) printf 1 ;; *) printf 0 ;; esac
	case $2 in *"$1 GREEN on original"*) printf 1 ;; *) printf 0 ;; esac
}
sc_caught() { # sc_caught GOT WANT: a mutation row is caught only when the mutant's completed control output is exactly the wrong outcome the row promises; an empty, malformed or failed outcome is never a catch
	if [ -n "$1" ] && [ "$1" = "$2" ]; then printf caught; else printf missed; fi
}
sc_isnum() { # sc_isnum TEXT: caught when TEXT is a completed numeric reading
	case $1 in '' | *[!0-9]*) printf missed ;; *) printf caught ;; esac
}
sc_hlwrong() { # sc_hlwrong MUTANT: the exact credits (RED then GREEN) the host-loss mutation promises on its row
	case $1 in
	stale | uplaunch) printf Rn ;; # survival credited where the container was gone or only inside the unobserved tail
	label) printf nn ;; # the survivor is no longer credited at all
	forget | late | lowrun | recfallback | runstatus | payload | order) printf nG ;; # absence credited where the table refuses it
	esac
}
sc_cfwrong() { # sc_cfwrong MUTANT: the exact terminal matrix the terminal-helper mutation promises
	case $1 in
	late) printf 'absent=0:absent:0:0:1 late=0:absent:0:0:1 never=1::1:0:1 unk=0:absent:0:1:1' ;;
	budget) printf 'absent=0:absent:0:0:1 late=1::1:0:0 never=1::1:0:0 unk=0:absent:0:1:1' ;;
	source) printf 'absent=0:absent:0:0:1 late=1::1:0:1 never=1::0:0:1 unk=0:absent:0:1:1' ;;
	unk) printf 'absent=0:absent:0:0:1 late=1::1:0:1 never=1::1:0:1 unk=0:absent:0:0:1' ;;
	esac
}
# dshim_write DIR REAL CLOCKCMD: the logging `docker` PATH shim that passes every call to REAL. CLOCKCMD is a command printing one reading of the shared
# monotonic clock (the `redact -clock` text); there is no default clock. Each call takes the token T=<shim pid>.<start reading> and appends `S T START ARGS`
# to $DLOG (ARGS with LF and CR mapped to ?, the record at most 4096 bytes), then reads the whole line back; a record that cannot be written and read back
# makes the shim exit 125 without invoking REAL. A `docker ps` query then runs REAL and appends `E T END STATUS [ANSWER]` to $DLOG.q (read back likewise;
# a failure exits 125 with no answer), prints the answer and exits with REAL's status; every other subcommand execs REAL after its S record.
# DSHIM_HOLD=<pattern> holds a call whose ARGS contain the pattern after its S record: the shim writes its parent (the native process-group leader) to
# $BAR/hold.pid, creates $BAR/hold.ready and waits up to 120 s for $BAR/hold.go, then execs REAL; on a timeout it exits 125 without invoking REAL.
dshim_write() {
	mkdir -p "$1" || return 1
	cat >"$1/docker" <<SHIM
#!/bin/sh
_t=\$(timeout 2 $3 </dev/null 2>/dev/null) || exit 125
_k=\$\$.\$_t
_a=\$(printf '%s' "\$*" | tr '\n\r' '??')
_l="S \$_k \$_t \$_a"
[ "\${#_l}" -le 4096 ] || exit 125
{ printf '%s\n' "\$_l" >>"\$DLOG"; } 2>/dev/null && grep -Fxq -- "\$_l" "\$DLOG" 2>/dev/null || exit 125
if [ -n "\${DSHIM_HOLD:-}" ]; then
	case \$_a in
	*"\$DSHIM_HOLD"*)
		echo \$PPID >"\$BAR/hold.pid"
		: >"\$BAR/hold.ready"
		_n=0
		while [ ! -e "\$BAR/hold.go" ] && [ "\$_n" -lt 120 ]; do
			sleep 1
			_n=\$((_n + 1))
		done
		[ -e "\$BAR/hold.go" ] || exit 125
		;;
	esac
fi
case \$1 in
ps)
	_o=\$($2 "\$@")
	_s=\$?
	_e=\$(timeout 2 $3 </dev/null 2>/dev/null) || exit 125
	_q="E \$_k \$_e \$_s [\$_o]"
	{ printf '%s\n' "\$_q" >>"\$DLOG.q"; } 2>/dev/null && grep -Fxq -- "\$_q" "\$DLOG.q" 2>/dev/null || exit 125
	[ -z "\$_o" ] || printf '%s\n' "\$_o"
	exit "\$_s"
	;;
esac
exec $2 "\$@"
SHIM
	chmod +x "$1/docker"
}
# sim_install DIR: the virtual clock the real host-loss producers run on in a self-check subshell. DIR/t is the monotonic time in ns and DIR/clock prints it as
# the helper's text; mono_ns, sleep and cstate_to are replaced: cstate_to answers unknown for the first SIM_UNK queries, then absent from SIM_GONE on, and every
# query advances the clock by SIM_LAT and logs its start and budget in DIR/bq. There is no wall clock anywhere in it.
sim_install() {
	SIMD=$1
	cat >"$SIMD/clock" <<CLK
#!/bin/sh
read -r _t <"$SIMD/t"
printf '%s.%02d\n' \$((_t / 1000000000)) \$(((_t % 1000000000) / 10000000))
CLK
	chmod +x "$SIMD/clock"
	mono_ns() {
		read -r _sv <"$SIMD/t" || return 1
		MONO_NS=$_sv
		[ "$MONO_NS" -ge "$MONO_LAST" ] || return 1
		MONO_LAST=$MONO_NS
	}
	sleep() {
		read -r _sv <"$SIMD/t"
		printf '%s\n' "$((_sv + $1 * 1000000000))" >"$SIMD/t"
		_sn=$(cat "$SIMD/sn" 2>/dev/null)
		_sn=$((${_sn:-0} + 1))
		printf '%s' "$_sn" >"$SIMD/sn"
		if [ -e "$HLD/k.res" ] || [ "$_sn" -gt 400 ]; then true >"$HLD/stop"; fi
	}
	cstate_to() {
		read -r _sv <"$SIMD/t"
		printf '%s %s\n' "$_sv" "$1" >>"$SIMD/bq"
		_sq=$(cat "$SIMD/qn" 2>/dev/null)
		_sq=$((${_sq:-0} + 1))
		printf '%s' "$_sq" >"$SIMD/qn"
		_sv=$((_sv + SIM_LAT))
		printf '%s\n' "$_sv" >"$SIMD/t"
		if [ "$_sq" -le "$SIM_UNK" ]; then
			printf unknown
		elif [ "$_sv" -ge "$SIM_GONE" ]; then
			printf absent
		else
			printf present
		fi
	}
}
sc_pt() { # sc_pt KIND [FILE]: 1 when the real plain `pass` credited a control whose runner stderr is KIND (FILE: a perturbed copy of the real function)
	_pto=$(
		META=0
		_ptz=$(printf '%064d' 0)
		rg_reset
		[ -z "${2:-}" ] || . "$2"
		case $1 in
		speech)
			printf 'dev-run: supervisor_untrusted\n' >"$TMP/rerr"
			plain_target
			;;
		started)
			printf 'dev-run: image=sha256:%s inputs=%s timeout=600s container=boxset-dev-run-0123456789abcdef log=none\n' "$_ptz" "$_ptz" >"$TMP/rerr"
			plain_target
			;;
		word)
			printf 'dev-run: capture_missing\n' >"$TMP/rerr"
			plain_target capture_missing
			;;
		otherword)
			printf 'dev-run: capture_not_png\n' >"$TMP/rerr"
			plain_target capture_missing
			;;
		refusal)
			printf 'dev-run: image_build_failed\n' >"$TMP/xe"
			intended_refusal image_build_failed "$TMP/xe" && reached
			;;
		earlier)
			printf 'dev-run: redact_unavailable\ndev-run: image_build_failed\n' >"$TMP/xe"
			intended_refusal image_build_failed "$TMP/xe" && reached
			;;
		esac
		pass "plain control"
	)
	case $_pto in "ok - plain control") printf 1 ;; *) printf 0 ;; esac
}
plain_target_self_check() {
	if ! sc_extract "$TMP/pt-real.sh" plain_target || ! sc_extract "$TMP/ir-real.sh" intended_refusal; then
		fail "classifier self-check" "plain_target or intended_refusal could not be extracted from $SELF"
		return
	fi
	cp "$TMP/pt-real.sh" "$TMP/pt-mut.sh"
	cp "$TMP/ir-real.sh" "$TMP/ir-mut.sh"
	mutate_file "$TMP/pt-mut.sh" 'grep -Eq "$RUNNER_STARTED_RE" "$TMP/rerr" 2>/dev/null && reached' "grep -q '^dev-run: ' \"\$TMP/rerr\" 2>/dev/null && reached" || return
	mutate_file "$TMP/ir-mut.sh" 'grep -Fxv "dev-run: $1" "$2" >"$TMP/rerr" 2>/dev/null' ': >"$TMP/rerr"' || return
	expect "classifier self-check plain target 1/6: a refusal that spoke where the child was expected is not the child's target" "$(sc_pt speech)" 0
	expect "classifier self-check plain target 2/6: the runner's identity line is the child's target" "$(sc_pt started)" 1
	expect "classifier self-check plain target 3/6: the exact expected refusal word is a refusal control's target" "$(sc_pt word)" 1
	expect "classifier self-check plain target 4/6: another runner word is not" "$(sc_pt otherword)" 0
	expect "classifier self-check plain target 5/6: the intended infrastructure refusal alone is clean" "$(sc_pt refusal)" 1
	expect "classifier self-check plain target 6/6: an earlier infrastructure failure beside the intended one is contamination" "$(sc_pt earlier)" 0
	expect "classifier self-check plain target: restoring any runner speech as the target credits the refused-then-spoke control" "$(sc_pt speech "$TMP/pt-mut.sh")" 1
	expect "classifier self-check plain target: exempting the whole stderr stream credits the earlier infrastructure failure" "$(sc_pt earlier "$TMP/ir-mut.sh")" 1
}
sc_unrec_cb() { # the synthetic target of the parent-owned unknown-observation controls: the mutant runner refuses (status 1), the original holds (0); the only contamination is what the real cstate leaves
	reached
	cstate sc-x
	case $1 in *sc-mut) return 1 ;; *) return 0 ;; esac
}
sc_unrec() { # sc_unrec RECORD OBSERVATION [FILE]: RG credited flags of the real redgreen over the real cstate with a stubbed query; RECORD ok (writable) or refused (/dev/full), OBSERVATION u (unknown) or a (absent); FILE a perturbed cstate
	(
		_suf=${3:-}
		TMP=$TMP/scu
		rm -rf "$TMP"
		mkdir -p "$TMP" || exit 1
		: >"$TMP/sc-mut"
		: >"$TMP/sc-orig"
		: >"$TMP/infra"
		: >"$TMP/rerr"
		if [ "$1" = refused ]; then ln -s /dev/full "$TMP/unknown"; else : >"$TMP/unknown"; fi
		if [ "$2" = u ]; then
			cstate_to() { printf unknown; }
		else
			cstate_to() { printf absent; }
		fi
		[ -z "$_suf" ] || . "$_suf"
		sc_rg "selfcheck unrec" "$(redgreen "selfcheck unrec" sc_unrec_cb "$TMP/sc-mut" "$TMP/sc-orig")"
	)
}
unrecorded_self_check() {
	if ! sc_extract "$TMP/cs-real.sh" cstate; then
		fail "classifier self-check" "cstate could not be extracted from $SELF"
		return
	fi
	cp "$TMP/cs-real.sh" "$TMP/cs-mut.sh"
	mutate_file "$TMP/cs-mut.sh" 'unknown_obs || SETUP=1' 'unknown_obs || :' || return
	expect "classifier self-check unknown observation 1/4: an unknown query with a writable record is neither RED nor GREEN" "$(sc_unrec ok u)" 00
	expect "classifier self-check unknown observation 2/4: an unknown query whose record cannot be written is neither RED nor GREEN, from the parent-owned state" "$(sc_unrec refused u)" 00
	expect "classifier self-check unknown observation 3/4: an absent observation leaves a clean refusal RED and a clean success GREEN" "$(sc_unrec ok a)" 11
	expect "classifier self-check unknown observation 4/4: dropping the recording-failure propagation credits both polarities of the unrecorded observation" "$(sc_unrec refused u "$TMP/cs-mut.sh")" 11
}
sc_ready() { # sc_ready FIRST [FILE]: the deferred credits (RED then GREEN, n when refused) of a host-loss result whose first readiness probe answered FIRST (STATUS:PAYLOAD), each first answer followed by 0:present, through the real hl_start, bg_wait, cx_obs, hl_taint, hl_flags and classifier; FILE a perturbed cx_obs
	(
		_srs=$TMP/hlst.sh
		TMP=$TMP/scy
		rm -rf "$TMP"
		HLD=$TMP/h
		mkdir -p "$HLD" || exit 1
		HL_TAINT_FAIL=
		: >"$TMP/unknown"
		. "$_srs"
		[ -z "${2:-}" ] || . "$2"
		SC_FIRST=$1
		SC_N=0
		cx() {
			SC_N=$((SC_N + 1))
			if [ "$SC_N" -le 1 ]; then _sa=$SC_FIRST; else _sa=0:present; fi
			[ -z "${_sa#*:}" ] || printf '%s\n' "${_sa#*:}"
			return "${_sa%%:*}"
		}
		sleep() { :; }
		bg_runlog() { BG_PID=0; }
		bg_ctr() { printf boxset-dev-0123456789abcdef; }
		hl_start k "$SELF" -- x || exit 1
		printf 'gone 1 2\n' >"$HLD/k.res"
		_sr=n
		_sg=n
		hl_credit red k && _sr=R
		hl_credit green k && _sg=G
		printf '%s%s' "$_sr" "$_sg"
	)
}
SC_READY_WANT='1:nn absent:nG empty:nn maybe:nn 125:nn'
sc_readym() { # sc_readym [FILE]: the first-answer table: Docker status 1, an absence, status 0 with an empty payload, status 0 with another payload and status 125
	printf '1:%s absent:%s empty:%s maybe:%s 125:%s' "$(sc_ready 1: "${1:-}")" "$(sc_ready 0:absent "${1:-}")" "$(sc_ready 0: "${1:-}")" "$(sc_ready 0:maybe "${1:-}")" "$(sc_ready 125: "${1:-}")"
}
readiness_self_check() {
	if ! sc_extract "$TMP/hlst.sh" hl_start || ! sc_extract "$TMP/cxo-real.sh" cx_obs; then
		fail "classifier self-check" "hl_start or cx_obs could not be extracted from $SELF"
		return
	fi
	cp "$TMP/cxo-real.sh" "$TMP/cxo-st1.sh"
	cp "$TMP/cxo-real.sh" "$TMP/cxo-payload.sh"
	mutate_file "$TMP/cxo-st1.sh" '	BW_UNK=1' '	[ "$_cxs" != 1 ] || return 1
	BW_UNK=1' || return
	mutate_file "$TMP/cxo-payload.sh" '	BW_UNK=1' '	[ "$_cxs" != 0 ] || return 1
	BW_UNK=1' || return
	expect "classifier self-check readiness: only a status-0 probe that printed exactly present or absent is an observation; Docker status 1, 125, an empty or another payload leave the deferred case neither RED nor GREEN" "$(sc_readym)" "$SC_READY_WANT"
	expect "classifier self-check readiness: reading Docker status 1 as absence completes exactly the status-1 row as a clean GREEN, which counts as caught" "$(sc_caught "$(sc_readym "$TMP/cxo-st1.sh")" '1:nG absent:nG empty:nn maybe:nn 125:nn')" caught
	expect "classifier self-check readiness: leaving the payload unchecked completes exactly the empty and foreign-payload rows as clean GREENs, which counts as caught" "$(sc_caught "$(sc_readym "$TMP/cxo-payload.sh")" '1:nn absent:nG empty:nG maybe:nG 125:nn')" caught
}
# --- host-loss bounds (B04/B10) on the virtual clock through the actual shim, hl_running, hl_register, hl_watcher and hl_credit ------------------
# S is the logging shim's start reading of the run, 1000.00 s; D is 12 s; the container-only running inspect advances the virtual clock to its row's
# completion, and R is the next driver reading plus one 10 ms floor. GREEN needs an absence query that completed before S+D; RED needs presence seen
# by a query that started at or after R+D; everything between is the unobserved tail.
sc_hl1() { # sc_hl1 ROW [WATCHER_FILE [REG_FILE]]: the credits (RED then GREEN: R, G, n when refused) of the actual chain for one row, `refused:` when no registration was made
	(
		_swf=${2:-$TMP/hlw.sh}
		_srf=${3:-$TMP/hlreg.sh}
		TMP=$TMP/scw
		rm -rf "$TMP"
		SIMD=$TMP/s
		HLD=$TMP/h
		mkdir -p "$SIMD/bin" "$HLD" || exit 1
		HL_TAINT_FAIL=
		: >"$TMP/unknown"
		: >"$TMP/infra"
		: >"$TMP/rerr"
		_nm=boxset-dev-0123456789abcdef
		_runs=1
		_startns=1000000000000
		_inspns=1000040000000
		_inspst=0
		_inspout=true
		_gone=1001000000000
		_lat=50000000
		_unk=0
		case $1 in
		gone | nowall) ;;
		tail02) _gone=1011800000000 ;;
		tail09) _gone=1011100000000 ;;
		survive) _gone=99999999999999 ;;
		unk) _unk=1 ;;
		slow)
			_gone=1011500000000
			_lat=3000000000
			;;
		latestart-gone)
			_inspns=1003990000000
			_gone=1013000000000
			;;
		latestart-survive)
			_inspns=1003990000000
			_gone=1014500000000
			;;
		noreceipt) _runs=0 ;;
		duplicate) _runs=2 ;;
		notrunning) _inspst=1 ;;
		falseanswer) _inspout=false ;;
		payload) _inspout='true
extra' ;;
		backward) _startns=1005000000000 ;;
		esac
		. "$_srf"
		. "$_swf"
		sim_install "$SIMD"
		MONO_LAST=0
		SIM_GONE=$_gone
		SIM_LAT=$_lat
		SIM_UNK=$_unk
		: >"$SIMD/dlog"
		: >"$SIMD/dlog.q"
		printf '#!/bin/sh\nexit 0\n' >"$SIMD/real"
		chmod +x "$SIMD/real"
		dshim_write "$SIMD/shim" "$SIMD/real" "$SIMD/clock" || exit 1
		printf '%s\n' "$_startns" >"$SIMD/t"
		_i=0
		while [ "$_i" -lt "$_runs" ]; do
			DLOG=$SIMD/dlog "$SIMD/shim/docker" run -d --rm --name "$_nm" --label x img /usr/bin/sleep 120 || exit 1
			_i=$((_i + 1))
		done
		printf '%s\n' "$_inspns" >"$SIMD/insp.t"
		printf '%s\n' "$_inspout" >"$SIMD/insp.out"
		printf '%s\n' "$_inspst" >"$SIMD/insp.st"
		cat >"$SIMD/bin/docker" <<STUB
#!/bin/sh
cat "$SIMD/insp.t" >"$SIMD/t"
cat "$SIMD/insp.out"
exit "\$(cat "$SIMD/insp.st")"
STUB
		printf '#!/bin/sh\nprintf x >>"%s/dates"\nexec /bin/date "$@"\n' "$SIMD" >"$SIMD/bin/date"
		chmod +x "$SIMD/bin/docker" "$SIMD/bin/date"
		PATH=$SIMD/bin:$PATH
		_swr=
		if hl_running "$_nm" && hl_register k "$_nm" "$SIMD/dlog" "$HL_R" 12000000000; then hl_watcher; else _swr=refused:; fi
		_sr=n
		_sg=n
		hl_credit red k && _sr=R
		hl_credit green k && _sg=G
		_sx=
		[ "$1" != nowall ] || _sx=":dates=$(cat "$SIMD/dates" 2>/dev/null | wc -c | tr -d ' ')"
		printf '%s%s%s%s' "$_swr" "$_sr" "$_sg" "$_sx"
	)
}
SC_HL_ROWS='gone tail02 tail09 survive unk slow latestart-gone latestart-survive noreceipt duplicate notrunning falseanswer payload backward nowall'
sc_hlwant() { # sc_hlwant ROW: the credits the table fixes for ROW
	case $1 in
	gone) printf nG ;;
	survive) printf Rn ;;
	nowall) printf 'nG:dates=0' ;;
	noreceipt | duplicate | notrunning | falseanswer | payload | backward) printf 'refused:nn' ;;
	*) printf nn ;;
	esac
}
sc_hlm() { # sc_hlm [WATCHER_FILE [REG_FILE]]: the whole table
	_hlm=
	for _hlr in $SC_HL_ROWS; do _hlm="$_hlm $_hlr:$(sc_hl1 "$_hlr" "$@")"; done
	printf '%s' "${_hlm# }"
}
sc_hlwantall() {
	_hlw=
	for _hlr in $SC_HL_ROWS; do _hlw="$_hlw $_hlr:$(sc_hlwant "$_hlr")"; done
	printf '%s' "${_hlw# }"
}
watcher_self_check() {
	if ! sc_extract "$TMP/hlw.sh" hl_watcher || ! sc_extract "$TMP/hlreg.sh" hl_running hl_register; then
		fail "classifier self-check" "hl_watcher, hl_running or hl_register could not be extracted from $SELF"
		return
	fi
	for _scm in stale forget late label; do cp "$TMP/hlw.sh" "$TMP/hlw-$_scm.sh"; done
	mutate_file "$TMP/hlw-stale.sh" '_s=$(cstate_to 30 "$_nm")' '_s=present' || return
	mutate_file "$TMP/hlw-stale.sh" '_s=$(cstate_to "$BUDGET" "$_nm")' '_s=present' || return
	mutate_file "$TMP/hlw-forget.sh" 'if [ "$_s" = unknown ] || [ "$((MONO_NS + MONO_RES))" -gt "$_lo" ]; then' 'if [ "$((MONO_NS + MONO_RES))" -gt "$_lo" ]; then' || return
	mutate_file "$TMP/hlw-late.sh" 'if [ "$_s" = unknown ] || [ "$((MONO_NS + MONO_RES))" -gt "$_lo" ]; then' 'if [ "$_s" = unknown ]; then' || return
	mutate_file "$TMP/hlw-label.sh" '"$(hl_terminal "$_s")"' 'late-gone' || return
	for _scm in lowrun uplaunch recfallback runstatus payload order; do cp "$TMP/hlreg.sh" "$TMP/hlreg-$_scm.sh"; done
	mutate_file "$TMP/hlreg-lowrun.sh" '"$((_hs + _hdn))"' '"$((_hr + _hdn))"' || return
	mutate_file "$TMP/hlreg-uplaunch.sh" '"$((_hr + _hdn))" >"$HLD/$_hk.reg"' '"$((_hs + _hdn + MONO_RES))" >"$HLD/$_hk.reg"' || return
	mutate_file "$TMP/hlreg-recfallback.sh" '		{ hl_taint "$_hk" setup "no single logging-shim run record for $_hn ($_hc)"; return 1; }' '		_hs=$_hr' || return
	mutate_file "$TMP/hlreg-runstatus.sh" ' >"$TMP/hlr.out" 2>/dev/null || return 1' ' >"$TMP/hlr.out" 2>/dev/null' || return
	mutate_file "$TMP/hlreg-payload.sh" '	[ "$(cat "$TMP/hlr.out"; printf x)" = "$_hrt" ] || return 1' ':' || return
	mutate_file "$TMP/hlreg-order.sh" '	[ "$_hr" -ge "$_hs" ] || { hl_taint "$_hk" setup "the running witness precedes the shim start reading"; return 1; }' ':' || return
	expect "classifier self-check host loss bounds: the actual shim, running witness, registration, watcher and classifier credit absence inside S+D only, never an unobserved tail, a late completion, an unknown answer or a refused registration, survival at the deadline only as RED, and never read a wall clock" "$(sc_hlm)" "$(sc_hlwantall)"
	for _scm in stale:gone forget:unk late:slow label:survive; do
		expect "classifier self-check host loss bounds: the ${_scm%%:*} mutant of the actual watcher completes its row ${_scm#*:} with exactly its promised credits $(sc_hlwrong "${_scm%%:*}")" "$(sc_caught "$(sc_hl1 "${_scm#*:}" "$TMP/hlw-${_scm%%:*}.sh")" "$(sc_hlwrong "${_scm%%:*}")")" caught
	done
	for _scm in lowrun:latestart-gone uplaunch:latestart-survive recfallback:noreceipt runstatus:notrunning payload:falseanswer order:backward; do
		expect "classifier self-check host loss bounds: the ${_scm%%:*} mutant of the actual registration completes its row ${_scm#*:} with exactly its promised credits $(sc_hlwrong "${_scm%%:*}")" "$(sc_caught "$(sc_hl1 "${_scm#*:}" '' "$TMP/hlreg-${_scm%%:*}.sh")" "$(sc_hlwrong "${_scm%%:*}")")" caught
	done
}
sc_cf1() { # sc_cf1 GONE LAT UNK [FILE]: rc:final:SETUP:unknown record:budget fit of the real natural-cell cf_final on the virtual clock (GONE, LAT and UNK as in sc_watch1)
	(
		_scf=${4:-$TMP/cf.sh}
		TMP=$TMP/scf
		rm -rf "$TMP"
		SIMD=$TMP/s
		HLD=$TMP/h
		mkdir -p "$SIMD" "$HLD" || exit 1
		: >"$TMP/unknown"
		: >"$TMP/infra"
		: >"$TMP/rerr"
		printf '%s\n' 1000000000000 >"$SIMD/t"
		. "$_scf"
		sim_install "$SIMD"
		SIM_GONE=$1
		SIM_LAT=$2
		SIM_UNK=$3
		MONO_LAST=0
		SETUP=0
		OBS=
		CF_NATURAL=1
		_fin=
		_rc=0
		cf_final boxset-dev-0123456789abcdef || _rc=1
		_ok=1
		while read -r _bs _bb; do
			_bi=${_bb%%.*}
			_bf=${_bb#*.}
			_bm=$((1$_bf))
			[ "$((_bs + _bi * 1000000000 + (_bm - 1000) * 1000000 + MONO_RES))" -le 1100000000000 ] || _ok=0
		done <"$SIMD/bq"
		printf '%s:%s:%s:%s:%s' "$_rc" "$_fin" "$SETUP" "$([ -s "$TMP/unknown" ] && echo 1 || echo 0)" "$_ok"
	)
}
sc_cfm() { # sc_cfm [FILE]: the terminal matrix: absence inside the budget, an answer that completes after the absolute budget, never absent, an unknown answer before a later absence
	printf 'absent=%s late=%s never=%s unk=%s' "$(sc_cf1 1001000000000 50000000 0 "${1:-}")" "$(sc_cf1 1097000000000 6000000000 0 "${1:-}")" "$(sc_cf1 99999999999999 50000000 0 "${1:-}")" "$(sc_cf1 1001000000000 50000000 1 "${1:-}")"
}
SC_CF_WANT='absent=0:absent:0:0:1 late=1::1:0:1 never=1::1:0:1 unk=0:absent:0:1:1'
terminal_self_check() {
	if ! sc_extract "$TMP/cf.sh" cf_terminal cf_final; then
		fail "classifier self-check" "cf_terminal or cf_final could not be extracted from $SELF"
		return
	fi
	for _scm in late budget source unk; do cp "$TMP/cf.sh" "$TMP/cf-$_scm.sh"; done
	mutate_file "$TMP/cf-late.sh" '[ "$((MONO_NS + MONO_RES))" -le "$_ctend" ] || { CF_FIN=unknown; SETUP=1; return 1; }' ':' || return
	mutate_file "$TMP/cf-budget.sh" '[ "$_ctl" -le 30000000000 ] || _ctl=30000000000' '_ctl=30000000000' || return
	mutate_file "$TMP/cf-source.sh" '[ "$_ctl" -ge 1000000000 ] || { SETUP=1; return 1; }' '[ "$_ctl" -ge 1000000000 ] || return 1' || return
	mutate_file "$TMP/cf-unk.sh" 'unknown) unknown_obs || SETUP=1 ;;' 'unknown) : ;;' || return
	expect "classifier self-check terminal: the actual natural-cell final state accepts absence only inside one absolute budget with every query bounded by what is left, and records an unknown answer and every failed or expired observation as setup" "$(sc_cfm)" "$SC_CF_WANT"
	for _scm in late budget source unk; do
		expect "classifier self-check terminal: the $_scm mutant of the actual terminal helper completes its matrix with exactly its promised outcomes" "$(sc_caught "$(sc_cfm "$TMP/cf-$_scm.sh")" "$(sc_cfwrong "$_scm")")" caught
	done
}
sc_nat1() { # sc_nat1 KIND [FILE]: 1 when the real cf_natural accepted the receipts the real shim wrote for that cell; unrecorded: 1 when the refused S append never invoked the stub
	(
		_snf=${2:-$TMP/cfn.sh}
		TMP=$TMP/scn
		rm -rf "$TMP"
		SIMD=$TMP/s
		mkdir -p "$SIMD" || exit 1
		printf '%s\n' 1000000000000 >"$SIMD/t"
		cat >"$SIMD/stub" <<STUB
#!/bin/sh
: >"$SIMD/marker"
read -r _t <"$SIMD/t"
printf '%s\n' \$((_t + 50000000)) >"$SIMD/t"
[ -z "\${STUB_OUT:-}" ] || printf '%s\n' "\$STUB_OUT"
exit "\${STUB_ST:-0}"
STUB
		chmod +x "$SIMD/stub"
		sim_install "$SIMD"
		dshim_write "$TMP/dshim" "$SIMD/stub" "$SIMD/clock" || exit 1
		: >"$TMP/dlog"
		: >"$TMP/dlog.q"
		RECEIPT=$TMP/receipt
		. "$_snf"
		_nq() { DLOG=$TMP/dlog STUB_ST=$1 STUB_OUT=$2 "$TMP/dshim/docker" ps -a --filter "name=^boxset-dev-0123456789abcdef\$" --format '{{.Names}}' >/dev/null 2>&1; }
		_ns() { "$SIMD/clock" >"$RECEIPT.t"; }
		_adv() { read -r _t <"$SIMD/t"; printf '%s\n' "$((_t + $1))" >"$SIMD/t"; }
		case $1 in
		ok)
			_nq 0 ''
			_nq 0 ''
			_adv 1000000000
			_ns
			;;
		failed)
			_nq 125 ''
			_nq 0 ''
			_adv 1000000000
			_ns
			;;
		answered)
			_nq 0 boxset-dev-0123456789abcdef
			_nq 0 ''
			_adv 1000000000
			_ns
			;;
		late)
			_ns
			_adv 1000000000
			_nq 0 ''
			_nq 0 ''
			;;
		single)
			_nq 0 ''
			_adv 1000000000
			_ns
			;;
		refused)
			_nq 0 ''
			_nq 0 ''
			mv "$TMP/dlog.q" "$TMP/dlog.q.kept"
			mkdir "$TMP/dlog.q"
			_nq 0 ''
			rmdir "$TMP/dlog.q"
			mv "$TMP/dlog.q.kept" "$TMP/dlog.q"
			_adv 1000000000
			_ns
			;;
		unrecorded)
			rm -f "$SIMD/marker"
			mv "$TMP/dlog" "$TMP/dlog.kept"
			mkdir "$TMP/dlog"
			_nq 0 ''
			_us=$?
			if [ "$_us" = 125 ] && [ ! -e "$SIMD/marker" ]; then printf 1; else printf 0; fi
			exit 0
			;;
		esac
		if cf_natural boxset-dev-0123456789abcdef >/dev/null 2>&1; then printf 1; else printf 0; fi
	)
}
SC_NAT_WANT='ok:1 failed:0 answered:0 late:0 single:0 refused:0 unrecorded:1'
sc_natm() { # sc_natm [FILE]: the natural-cell matrix over the real shim's receipts
	printf 'ok:%s failed:%s answered:%s late:%s single:%s refused:%s unrecorded:%s' "$(sc_nat1 ok "${1:-}")" "$(sc_nat1 failed "${1:-}")" "$(sc_nat1 answered "${1:-}")" "$(sc_nat1 late "${1:-}")" "$(sc_nat1 single "${1:-}")" "$(sc_nat1 refused "${1:-}")" "$(sc_nat1 unrecorded "${1:-}")"
}
natural_self_check() {
	if ! sc_extract "$TMP/cfn.sh" cf_natural; then
		fail "classifier self-check" "cf_natural could not be extracted from $SELF"
		return
	fi
	cp "$TMP/cfn.sh" "$TMP/cfn-mut.sh"
	cp "$TMP/cfn.sh" "$TMP/cfn-recon.sh"
	mutate_file "$TMP/cfn-mut.sh" '		if [ "$_cqst" != 0 ] || [ "$_cqa" != '"'"'[]'"'"' ] || ! mono_parse "$_cqe" || [ "$((MONO_NS + MONO_RES))" -gt "$_cns" ]; then' '		if false; then' || return
	mutate_file "$TMP/cfn-recon.sh" '		[ "$_cqc" = 1 ] || { _cnbad=$((_cnbad + 1)); continue; }' '		[ "$_cqc" = 1 ] || continue' || return
	expect "classifier self-check natural cell: the actual premise accepts only queries with exactly one well-formed receipt, status 0, an empty exact answer and a completion at least 10 ms before the effect began, from the receipts the actual shim writes; a refused S append never invokes the stub" "$(sc_natm)" "$SC_NAT_WANT"
	expect "classifier self-check natural cell: dropping the status, answer and completion checks accepts the failed, answered and late cells" "$(sc_natm "$TMP/cfn-mut.sh")" 'ok:1 failed:1 answered:1 late:1 single:0 refused:0 unrecorded:1'
	expect "classifier self-check natural cell: the reconciliation-removed copy accepts the refused cell, which counts as caught" "$([ "$(sc_nat1 refused "$TMP/cfn-recon.sh")" = 1 ] && echo caught || echo missed)" caught
}
caught_self_check() {
	expect "classifier self-check mutation rows: a mutant that completes with exactly the wrong outcome its row promises is caught" "$(sc_caught nG nG)" caught
	expect "classifier self-check mutation rows: an empty outcome (a control that could not run) is never a catch" "$(sc_caught '' '')" missed
	expect "classifier self-check mutation rows: a different completed outcome than the promised one is not a catch" "$(sc_caught Rn nG)" missed
	expect "classifier self-check mutation rows: a watcher copy that cannot be read completes no outcome, so its row is never caught" "$(sc_caught "$(sc_hl1 gone "$TMP/no-such-watcher.sh" 2>/dev/null)" nG)" missed
	expect "classifier self-check mutation rows: a registration copy that cannot be read completes no outcome, so its row is never caught" "$(sc_caught "$(sc_hl1 gone '' "$TMP/no-such-registration.sh" 2>/dev/null)" nG)" missed
	expect "classifier self-check mutation rows: a terminal copy that cannot be read completes no matrix, so its row is never caught" "$(sc_caught "$(sc_cfm "$TMP/no-such-terminal.sh" 2>/dev/null)" "$(sc_cfwrong late)")" missed
	expect "classifier self-check mutation rows: a readiness copy that cannot be read completes no table, so its row is never caught" "$(sc_caught "$(sc_readym "$TMP/no-such-readiness.sh" 2>/dev/null)" '1:nG absent:nG empty:nn maybe:nn 125:nn')" missed
	expect "classifier self-check mutation rows: a clock reading that is not a number is never a catch" "$(sc_isnum fail):$(sc_isnum '')" "missed:missed"
}
sc_cxo() { # sc_cxo [FILE]: STATUS:BW_UNK:UNKNOWN-RECORD-BYTES of the real cx_obs against a Docker host that is not there; FILE a perturbed cx_obs
	(
		TMP=$TMP/sci
		rm -rf "$TMP"
		mkdir -p "$TMP" || exit 1
		: >"$TMP/unknown"
		DOCKER_HOST=unix://$TMP/no-such-docker.sock
		HOME=/nonexistent # the planned premise: no per-user Docker configuration or context can stand in for the private socket
		export DOCKER_HOST HOME
		[ -z "${1:-}" ] || . "$1"
		BW_UNK=0
		cx_obs boxset-dev-0123456789abcdef /tmp/ready >/dev/null 2>&1
		_cxr=$?
		printf '%s:%s:%s' "$_cxr" "$BW_UNK" "$(wc -c <"$TMP/unknown" | tr -d ' ')"
	)
}
sc_fatal() { # sc_fatal KIND PATH [FILE]: G when the real chain credited a clean GREEN of a case whose runner stderr is KIND (ord: an ordinary child line that mentions panic mid-line; panic: a controller panic trace) through the ordinary rerr_seen and infra_failure or the deferred hl_err_taint, n otherwise; FILE a perturbed copy of those functions. The real hl_err_taint is the extracted $TMP/het-real.sh, sourced here before any perturbation: a copy that is missing, unreadable, malformed or does not define it completes no outcome
	(
		_scf=$TMP/het-real.sh
		TMP=$TMP/scz
		rm -rf "$TMP"
		mkdir -p "$TMP" || exit 1
		HLD=$TMP/h
		mkdir -p "$HLD" || exit 1
		if [ ! -r "$_scf" ] || ! . "$_scf" || ! command -v hl_err_taint >/dev/null 2>&1; then exit 1; fi
		HL_TAINT_FAIL=
		rg_reset
		reached
		case $1 in
		ord) printf 'dev-run: image=sha256:0 container=boxset-dev-0\nsh: 1: a child said panic: and fatal error: in the middle of a line\n' >"$TMP/rerr" ;;
		panic) printf 'dev-run: image=sha256:0 container=boxset-dev-0\npanic: injected controller fatal\n\ngoroutine 1 [running]:\n' >"$TMP/rerr" ;;
		esac
		cp "$TMP/rerr" "$TMP/k.err"
		[ -z "${3:-}" ] || . "$3"
		case $2 in
		ordinary)
			rerr_seen
			if rg_verdict green 0; then printf G; else printf n; fi
			;;
		deferred)
			printf 'gone 1 2\n' >"$HLD/k.res"
			hl_err_taint k k
			if hl_credit green k; then printf G; else printf n; fi
			;;
		esac
	)
}
transport_self_check() {
	if ! sc_extract "$TMP/rs-real.sh" rerr_seen || ! sc_extract "$TMP/het-real.sh" hl_err_taint || ! sed -n '/^infra_failure() {.*}$/p' "$SELF" >>"$TMP/rs-real.sh" || ! grep -q '^infra_failure() {' "$TMP/rs-real.sh"; then
		fail "classifier self-check" "rerr_seen, infra_failure or hl_err_taint could not be extracted from $SELF"
		return
	fi
	cp "$TMP/rs-real.sh" "$TMP/rs-mut.sh"
	cp "$TMP/het-real.sh" "$TMP/het-mut.sh"
	# the complete ordinary fatal rejection: the recorder (rerr_seen) and the verdict's own independent scan (infra_failure)
	mutate_file "$TMP/rs-mut.sh" '|| grep -Eq "$CTRL_FATAL_RE" "$TMP/rerr" 2>/dev/null; then' '; then' || return
	mutate_file "$TMP/rs-mut.sh" ' || grep -Eq "$CTRL_FATAL_RE" "$TMP/rerr" 2>/dev/null; }' '; }' || return
	mutate_file "$TMP/het-mut.sh" '	! grep -Eq "$CTRL_FATAL_RE" "$TMP/$2.err" 2>/dev/null || hl_taint "$1" setup "controller fatal output"' ':' || return
	expect "classifier self-check fatal 1/4: an ordinary child line that only mentions panic mid-line is not controller-fatal (ordinary classifier)" "$(sc_fatal ord ordinary)" G
	expect "classifier self-check fatal 2/4: a controller panic trace is infrastructure and earns no ordinary credit" "$(sc_fatal panic ordinary)" n
	expect "classifier self-check fatal 3/4: an ordinary child line is not controller-fatal (deferred host-loss classifier)" "$(sc_fatal ord deferred)" G
	expect "classifier self-check fatal 4/4: a controller panic trace taints the deferred host-loss case, which earns no credit" "$(sc_fatal panic deferred)" n
	expect "classifier self-check fatal: removing the complete fatal rejection (the rerr_seen record and the infra_failure scan) from the real classifier credits the panic row, which counts as caught" "$(sc_caught "$(sc_fatal panic ordinary "$TMP/rs-mut.sh")" G)" caught
	expect "classifier self-check fatal: removing the fatal taint from the real hl_err_taint credits the deferred panic row, which counts as caught" "$(sc_caught "$(sc_fatal panic deferred "$TMP/het-mut.sh")" G)" caught
	_tx=$(HOME=/nonexistent DOCKER_HOST="unix://$TMP/no-such-docker.sock" timeout 30 docker exec -u 1000:1000 boxset-dev-0123456789abcdef /usr/bin/true 2>/dev/null)
	_ts=$?
	expect "classifier self-check transport: a Docker host that is not there fails the readiness exec with a nonzero status and no payload (the premise of the next rows)" "$([ "$_ts" != 0 ] && [ -z "$_tx" ] && echo premise)" premise
	expect "classifier self-check transport: through the real cx_obs that failure is an unknown observation (status 2, BW_UNK set, one unknown record), never absence" "$(sc_cxo)" "2:1:1"
	expect "classifier self-check transport: the status-1-as-absence copy reads the same failure as a clean absence, which counts as caught" "$(sc_caught "$(sc_cxo "$TMP/cxo-st1.sh")" "1:0:0")" caught
	expect "classifier self-check transport: the payload-unchecked copy still reads Docker status 1 as unknown" "$(sc_cxo "$TMP/cxo-payload.sh")" "2:1:1"
}
classifier_self_check() {
	META=1
	: >"$TMP/sc-mut"
	: >"$TMP/sc-orig"
	_scr=$(sc_pairs)
	expect "classifier self-check 1/10: a refusal whose target was never reached is not RED" "$(sc_pick "$_scr" unreached R)" 0
	expect "classifier self-check 2/10: a success whose target was never reached is not GREEN" "$(sc_pick "$_scr" unreached G)" 0
	expect "classifier self-check 3/10: a reached refusal with a setup failure is not RED" "$(sc_pick "$_scr" setup R)" 0
	expect "classifier self-check 4/10: a reached success with a setup failure is not GREEN" "$(sc_pick "$_scr" setup G)" 0
	expect "classifier self-check 5/10: a reached refusal with an infrastructure failure is not RED" "$(sc_pick "$_scr" infra R)" 0
	expect "classifier self-check 6/10: a reached success with an infrastructure failure is not GREEN" "$(sc_pick "$_scr" infra G)" 0
	expect "classifier self-check 7/10: a reached refusal with an unknown observation is not RED" "$(sc_pick "$_scr" unknown R)" 0
	expect "classifier self-check 8/10: a reached success with an unknown observation is not GREEN" "$(sc_pick "$_scr" unknown G)" 0
	expect "classifier self-check 9/10: a clean reached refusal is RED" "$(sc_pick "$_scr" clean R)" 1
	expect "classifier self-check 10/10: a clean reached success is GREEN" "$(sc_pick "$_scr" clean G)" 1
	_scw='unreached:00 setup:00 infra:00 unknown:00 clean:11'
	sed -n '/^rg_verdict() {$/,/^}$/p' "$SELF" >"$TMP/rgv-real.sh"
	if [ ! -s "$TMP/rgv-real.sh" ]; then
		fail "classifier self-check" "rg_verdict could not be extracted from $SELF"
		return
	fi
	for _scg in reach setup infra unknown; do
		case $_scg in
		reach) _sco=$G_REACH _scpair=unreached ;;
		setup) _sco=$G_SETUP _scpair=setup ;;
		infra) _sco=$G_INFRA _scpair=infra ;;
		unknown) _sco=$G_UNK _scpair=unknown ;;
		esac
		cp "$TMP/rgv-real.sh" "$TMP/rgv-$_scg.sh"
		mutate_file "$TMP/rgv-$_scg.sh" "$_sco" ':' || continue
		_scm=$(
			. "$TMP/rgv-$_scg.sh"
			sc_pairs
		)
		expect "classifier self-check: removing the $_scg guard from the real classifier credits exactly the $_scpair controls" "$_scm" "$(printf '%s' "$_scw" | sed "s/$_scpair:00/$_scpair:11/")"
	done
	plain_self_check
	clock_self_check
	latch_self_check
	lint_self_check
	clockdiag_self_check
	hl_self_check
	plain_target_self_check
	unrecorded_self_check
	readiness_self_check
	watcher_self_check
	terminal_self_check
	natural_self_check
	caught_self_check
	transport_self_check
	oracle_self_check
}

tree_hash() { # hash of the isolated checkout outside the write roots
	(cd "$ISO" && {
		find . ! -path './testdata/parser*' ! -path './docs/spikes/issue-drafts*' | LC_ALL=C sort
		find . -type f ! -path './testdata/parser/*' ! -path './docs/spikes/issue-drafts/*' -exec sha256sum {} + | LC_ALL=C sort
	} | sha256sum)
}
fhash() { sha256sum <"$1" | cut -d' ' -f1; } # fhash FILE

png_dims() {
	[ -f "$1" ] || { printf missing; return; }
	set -- $(od -An -tu1 -j16 -N8 "$1")
	[ $# = 8 ] || { printf unreadable; return; }
	printf '%sx%s' "$(($1 * 16777216 + $2 * 65536 + $3 * 256 + $4))" "$(($5 * 16777216 + $6 * 65536 + $7 * 256 + $8))"
}

capdir() {
	sleep 1
	printf '.devrites/work/boxset-test/screenshots-%s' "$(date -u +%Y%m%dT%H%M%SZ)"
}

# --- background runs with container-owned barriers --------------------------------------
# bg_run NAME RUNNER ARGS...: runner in the background; BG_PID, $TMP/NAME.out/.err.
bg_run() {
	_bn=$1
	_br=$2
	shift 2
	(cd "$BGDIR" && exec "$_br" "$@") >"$TMP/$_bn.out" 2>"$TMP/$_bn.err" &
	BG_PID=$!
}
# bg_runlog NAME RUNNER ARGS...: bg_run with the runner's Docker calls logged by the shim to the case's own $TMP/NAME.dlog (S records) and .dlog.q (E records),
# both pre-created and required to be the same regular files afterwards (dlogs_same). DSHIM_HOLD and BAR reach the shim through the inherited environment.
# BG_PID is the runner itself: the bootstrap execs its controller, so a signal to BG_PID is a signal to the controller.
bg_runlog() {
	_bn=$1
	_br=$2
	shift 2
	{ true >"$TMP/$_bn.dlog" && true >"$TMP/$_bn.dlog.q"; } || SETUP=1
	DL_ID0=$(dlog_ident "$TMP/$_bn.dlog" "$TMP/$_bn.dlog.q")
	(cd "$BGDIR" && PATH="$TMP/dshim:$PATH" DLOG="$TMP/$_bn.dlog" BAR="$BAR" DSHIM_HOLD="${DSHIM_HOLD:-}" exec "$_br" "$@") >"$TMP/$_bn.out" 2>"$TMP/$_bn.err" &
	BG_PID=$!
}
dlog_ident() { find "$1" "$2" -maxdepth 0 -type f -printf '%D:%i ' 2>/dev/null; } # dlog_ident DLOG DLOGQ: device:inode of both logs when both are regular files
dlogs_same() { # dlogs_same NAME: both logs of NAME are still the pre-created regular files (a replaced or removed log is setup, never evidence)
	[ -n "$DL_ID0" ] && [ "$(dlog_ident "$TMP/$1.dlog" "$TMP/$1.dlog.q")" = "$DL_ID0" ] || { OBS="$OBS; the shim logs of $1 are not the pre-created regular files"; SETUP=1; return 1; }
}
# bg_ctr NAME: print the container name from the background runner's own line.
bg_ctr() {
	_i=0
	while [ "$_i" -lt 900 ]; do
		_c=$(grep -o 'container=boxset-dev-run-[0-9a-f]\{16\}' "$TMP/$1.err" 2>/dev/null | head -n 1 | cut -d= -f2)
		if [ -n "$_c" ]; then
			printf '%s\n' "$_c" >>"$TMP/owned.list"
			printf '%s' "$_c"
			return 0
		fi
		kill -0 "$BG_PID" 2>/dev/null || return 1
		sleep 1
		_i=$((_i + 1))
	done
	return 1
}
# cx CTR ARGS...: test-owned exec as the child user (never a supervisor exec)
cx() {
	_cc=$1
	shift
	timeout 30 docker exec -u 1000:1000 "$_cc" "$@"
}
# cx_obs CTR FILE: one status-checked existence probe of FILE: a child-user shell prints exactly `present` or `absent`. It is an observation only when the
# transport status is 0 and stdout is exactly that one line (present returns 0, absent returns 1); a Docker CLI status 1, any other nonzero status, a timeout
# and an empty, extra or malformed output are an unknown observation, recorded for the current case (BW_UNK marks it for case-owned deferred state), never
# read as absence: return 2
BW_UNK=0
cx_obs() {
	cx "$1" /usr/bin/sh -c 'if [ -e "$1" ]; then echo present; else echo absent; fi' sh "$2" >"$TMP/cxo" 2>/dev/null
	_cxs=$?
	_cxv=$(cat "$TMP/cxo" 2>/dev/null; printf x)
	if [ "$_cxs" = 0 ]; then
		case $_cxv in
		"present
x") return 0 ;;
		"absent
x") return 1 ;;
		esac
	fi
	BW_UNK=1
	unknown_obs || SETUP=1
	return 2
}
# bg_wait CTR FILE: the child created FILE (status-checked polling, no blind sleep); BW_UNK is 1 when any probe failed, whatever a later one showed
bg_wait() {
	BW_UNK=0
	_bwi=0
	while [ "$_bwi" -lt 600 ]; do
		if cx_obs "$1" "$2"; then return 0; fi
		sleep 1
		_bwi=$((_bwi + 1))
	done
	return 1
}
bg_touch() { cx "$1" /usr/bin/touch "$2" >/dev/null 2>&1; } # bg_touch CTR FILE
bg_end() { # bg_end NAME: wait for the background runner, sets RS and copies its output
	wait "$BG_PID"
	RS=$?
	cp "$TMP/$1.out" "$TMP/rout"
	cp "$TMP/$1.err" "$TMP/rerr"
	rerr_seen
}
# The in-container barrier: announce, then wait for the host's release.
CBAR='touch /tmp/ready; n=0; while [ ! -e /tmp/go ] && [ "$n" -lt 900 ]; do sleep 1; n=$((n + 1)); done'
classifier_self_check
META=0
if [ "$SELFCHECK_ONLY" = 1 ]; then
	if [ "$FAILS" -eq 0 ]; then
		printf 'DEV-RUN-TEST-SELFCHECK-PASS controls=%s\n' "$PASSES"
		exit 0
	fi
	printf 'DEV-RUN-TEST-SELFCHECK-FAIL failed=%s passed=%s\n' "$FAILS" "$PASSES"
	exit 1
fi

# --- isolated checkout of the current allowlisted inputs ----------------------------------
list=
for p in deploy scripts .dockerignore go.mod go.sum .golangci.yml internal bench testdata web docs/spikes docs/adr; do
	[ -e "$p" ] && list="$list $p"
done
mkdir -p "$ISO" "$CANARY"
# shellcheck disable=SC2086
tar -cf - --exclude=web/node_modules --exclude=web/.svelte-kit --exclude=web/build \
	--exclude=docs/spikes/data --exclude=docs/spikes/issue-drafts --exclude=testdata/parser $list | tar -xf - -C "$ISO"
mkdir -p "$ISO/prototypes" "$ISO/design" "$ISO/.devrites/work/boxset-test"
printf 'OWNER_SECRET_CANARY=do-not-leak\n' >"$ISO/.env"
printf 'canary\n' >"$ISO/prototypes/canary.txt"
printf 'canary\n' >"$ISO/design/canary.txt"
printf 'outside canary\n' >"$CANARY/canary.txt"
ctl_begin
expect "runner copy is SHA-256 identical" "$(fhash "$SRC/scripts/dev-run.sh")" "$(fhash "$ISO/scripts/dev-run.sh")"
REV=$(grep '^ARG PLAYWRIGHT_CHROMIUM_REVISION=' "$ISO/deploy/dev/Dockerfile")
REV=${REV#ARG PLAYWRIGHT_CHROMIUM_REVISION=}

# ===== 1. exact argv, statuses, stdout/stderr (T02/T26) =========================================
set -- 'a b' "q'uote" 'dq"x' '$HOME;`id`|&' 'line1
line2' '' '-n' '--' '*'
want=$(for a in "$@"; do hexline "$a"; done)
rr -- sh -c 'for a in "$@"; do printf "ARG "; printf %s "$a" | od -An -v -tx1 | tr -d " \n"; echo; done' sh "$@"
expect "argv run exits 0" "$RS" 0
expect "argv reaches the child byte for byte" "$(cat "$TMP/rout")" "$want"
for code in 0 1 3; do
	rr -s "$code" -- sh -c "exit $code"
	expect "child exit $code is the runner status" "$RS" "$code"
done
rr -- sh -c 'printf stdout; printf stderr >&2'
expect "the both-streams run exits 0" "$RS" 0
case "$(cat "$TMP/rout")" in
stdoutstderr | stderrstdout) pass "both streams are captured, 12 bytes, order not asserted" ;;
*) fail "stdout and stderr" "got '$(cat "$TMP/rout")'" ;;
esac
expect "dev-run prints image, inputs, timeout and container identity" "$(grep -Ec '^dev-run: image=sha256:[0-9a-f]{64} inputs=[0-9a-f]{64} timeout=600s container=boxset-dev-run-[0-9a-f]{16} log=none$' "$TMP/rerr")" 1

# pre-deadline child 124 and 137 propagate; the deadline is observed, not guessed. The shell's host-deadline branch sets status 124 and prints no
# reason word, so neither a child's own 124 nor the deadline carries a word: both are the identity line alone.
rr -s 124 --timeout 1 -- sh -c 'exit 124'
expect "child exit 124 before the deadline propagates" "$RS" 124
noreason "child exit 124 is not reported as a timeout" timeout
# The image's /bin/sh is dash and the child is PID 1 of its namespace, which no member of that namespace can signal: a PID 1 `kill -KILL $$` exits 0.
# The killed process is therefore a nested shell.
rr -s 137 --timeout 5 -- sh -c 'sh -c "kill -KILL \$\$"'
expect "child kill -KILL (137) before the deadline propagates" "$RS" 137
noreason "child 137 is not reported as a timeout" timeout
a_pre_deadline() { # holds when a child exit 124 is neither rewritten nor reported: status 124 and the identity line alone
	rrx "$ISO" "$1" --timeout 10 -- sh -c 'exit 124'
	OBS="status $RS, $(grep -c . "$TMP/rerr") stderr lines"
	[ "$RS" = 124 ] && grep -Eq "$RUNNER_STARTED_RE" "$TMP/rerr" && reached
	[ "$RS" = 124 ] && [ "$(grep -c . "$TMP/rerr")" = 1 ]
}
holds "child exit 124 before the deadline stays 124 with the identity line alone (no deadline word)" a_pre_deadline "$RUNNER"

rr -s 124 --timeout 3 -- sh -c 'setsid sh -c "sleep 120" & sleep 120'
expect "real deadline exits 124" "$RS" 124
expect "the deadline prints the identity line once" "$(grep -Ec "$RUNNER_STARTED_RE" "$TMP/rerr")" 1
noreason "the deadline prints no reason word" timeout
gone "$(cname)" "no container survives the deadline (detached grandchild included)"

# ===== 2. identity, trust and the protected supervisor (T26) =========================================
rr -- sh -c 'set -- $(sed -n "s/^Groups://p" /proc/self/status); { [ $# = 0 ] || [ "$*" = 1000 ]; } && [ "$(id -u)" = 1000 ] && [ "$(id -g)" = 1000 ]'
expect "child is 1000:1000 and its Groups line is empty or exactly its primary GID 1000" "$RS" 0
TAMPER="$CBAR"'
tampered() { printf %s "$1" >/tmp/tamper.code; touch /tmp/tampered; n=0; while [ ! -e /tmp/exit ] && [ "$n" -lt 600 ]; do sleep 1; n=$((n + 1)); done; exit "$1"; }
for f in /usr/bin/stat /usr/bin/sh /usr/bin/cat; do
	if true 2>/dev/null >"$f"; then tampered 11; fi
	if chmod 0777 "$f" 2>/dev/null; then tampered 12; fi
	if rm -f "$f" 2>/dev/null && [ ! -e "$f" ]; then tampered 13; fi
	if mv "$f" /tmp/moved 2>/dev/null; then tampered 14; fi
done
if true 2>/dev/null >/usr/local/bin/stat; then tampered 15; fi
if true 2>/dev/null >/etc/ld.so.preload; then tampered 16; fi
if echo x 2>/dev/null >>/usr/local/share/boxset/inputs.sha256; then tampered 17; fi
touch /tmp/done
n=0; while [ ! -e /tmp/exit ] && [ "$n" -lt 600 ]; do sleep 1; n=$((n + 1)); done'
a_no_tamper() { # holds when the child cannot alter any supervisor binary or manifest. A successful attempt is the child's own receipt (/tmp/tampered, its code in /tmp/tamper.code), written with unaffected primitives while the child stays live
	# and held: the parent observes that exact success, and the independent hashes, before any release or cleanup. A generic early end of the child is not evidence.
	bg_run tamper "$1" -- sh -c "$TAMPER"
	_c=$(bg_ctr tamper) || { OBS="no container"; SETUP=1; return 1; }
	if ! bg_wait "$_c" /tmp/ready; then OBS="child never ready"; SETUP=1; kill -KILL "$BG_PID" 2>/dev/null; return 1; fi
	_pre=$(sup_hashes "$_c") || { OBS="the pre-attempt supervisor hash query failed or its four records are malformed"; SETUP=1; kill -KILL "$BG_PID" 2>/dev/null; return 1; }
	bg_touch "$_c" /tmp/go || { OBS="the child could not be released"; SETUP=1; kill -KILL "$BG_PID" 2>/dev/null; return 1; }
	_ts=
	_i=0
	while [ "$_i" -lt 600 ]; do
		if cx_obs "$_c" /tmp/tampered; then _ts=tampered; break; fi
		if cx_obs "$_c" /tmp/done; then _ts=done; break; fi
		kill -0 "$BG_PID" 2>/dev/null || { _ts=ended; break; }
		sleep 1
		_i=$((_i + 1))
	done
	case $_ts in
	tampered | done) ;;
	*)
		OBS="the child neither completed its attempts nor held at a successful one (state '${_ts:-timeout}')"
		SETUP=1
		kill -KILL "$BG_PID" 2>/dev/null
		bg_end tamper
		return 1
		;;
	esac
	_post=$(sup_hashes "$_c") || { OBS="the post-attempt supervisor hash query failed or its four records are malformed"; SETUP=1; bg_touch "$_c" /tmp/exit; bg_end tamper; return 1; }
	_tc=
	if [ "$_ts" = tampered ]; then _tc=$(cx "$_c" /usr/bin/cat /tmp/tamper.code 2>/dev/null) || _tc=; fi
	bg_touch "$_c" /tmp/exit
	bg_end tamper
	if [ "$_ts" = tampered ]; then
		OBS="a tamper attempt succeeded (code '${_tc:-unread}') with the child held live; hashes equal: $([ "$_pre" = "$_post" ] && echo yes || echo NO), released child status $RS"
		case $_tc in 11 | 12 | 13 | 14 | 15 | 16 | 17) ;; *) SETUP=1; return 1 ;; esac
		# an in-place truncation (code 11) must also show in the independent hashes taken while the child was held
		{ [ "$_tc" != 11 ] || [ "$_pre" != "$_post" ]; } || { SETUP=1; return 1; }
		reached
		return 1
	fi
	OBS="child status $RS, hashes equal: $([ "$_pre" = "$_post" ] && echo yes || echo NO)"
	reached
	[ "$RS" = 0 ] && [ "$_pre" = "$_post" ]
}
holds "child cannot write, truncate, replace or chmod supervisor binaries, preload or manifest; hashes unchanged" a_no_tamper "$RUNNER"

# ===== 3. issue-draft publication and the refused write roots (T26) ==========================================
# --write publishes only the fixed names the child created under /src/docs/spikes/issue-drafts (s5-admission.md and s5-tools.md, at most
# 64 KiB each) once the container stopped. /src/docs/spikes is 1777, so the child creates the root directory and its files itself; a name it never
# created is not an error and is not published. The root is an ordinary directory of the image: no mounted map and no precreated file. Every other path is
# dev-run: write_path_invalid (status 125, before Docker, no child).
DRAFTDIR=$ISO/docs/spikes/issue-drafts
MKD='mkdir -p /src/docs/spikes/issue-drafts'
rm -rf "$DRAFTDIR"
rr --write docs/spikes/issue-drafts -- sh -c "$MKD && printf draft >/src/docs/spikes/issue-drafts/s5-admission.md"
expect "initially absent draft root publishes s5-admission.md" "$RS:$(cat "$DRAFTDIR/s5-admission.md" 2>/dev/null)" "0:draft"
expect "a draft name the child never created stays absent and the run exits 0" "$([ -e "$DRAFTDIR/s5-tools.md" ] && echo present || echo absent):$(find "$DRAFTDIR" -mindepth 1 -printf '%f ' 2>/dev/null)" "absent:s5-admission.md "
rm -rf "$DRAFTDIR"
rr --write docs/spikes/issue-drafts -- sh -c "$MKD && printf adm >/src/docs/spikes/issue-drafts/s5-admission.md && printf tools >/src/docs/spikes/issue-drafts/s5-tools.md"
expect "both draft names the child created are published with its bytes" "$RS:$(cat "$DRAFTDIR/s5-admission.md" 2>/dev/null):$(cat "$DRAFTDIR/s5-tools.md" 2>/dev/null)" "0:adm:tools"
rm -rf "$DRAFTDIR"
rr --write docs/spikes/issue-drafts -- true
expect "a child that creates no draft publishes nothing and leaves no draft root" "$RS:$([ -e "$DRAFTDIR" ] && echo present || echo absent)" "0:absent"
mkdir -p "$DRAFTDIR"
printf 'prior\n' >"$DRAFTDIR/s5-admission.md"
chmod 0600 "$DRAFTDIR/s5-admission.md"
rr --write docs/spikes/issue-drafts -- sh -c "$MKD && printf new >/src/docs/spikes/issue-drafts/s5-admission.md"
expect "a published draft replaces the prior draft with the child's bytes and mode 0644" "$RS:$(cat "$DRAFTDIR/s5-admission.md" 2>/dev/null):$(find "$DRAFTDIR/s5-admission.md" -printf '%m' 2>/dev/null)" "0:new:644"
rm -rf "$DRAFTDIR"
rr -s 7 --write docs/spikes/issue-drafts -- sh -c "$MKD && printf draft >/src/docs/spikes/issue-drafts/s5-admission.md; exit 7"
expect "nonzero child: the draft is published and the status kept" "$RS:$(cat "$DRAFTDIR/s5-admission.md" 2>/dev/null)" "7:draft"
rm -rf "$DRAFTDIR"
rr -w draft_overflow --write docs/spikes/issue-drafts -- sh -c "$MKD && yes a | head -c 65537 >/src/docs/spikes/issue-drafts/s5-admission.md"
expect "a draft over 64 KiB is refused" "$RS" 125
reason "draft overflow reason" draft_overflow
expect "an oversize draft publishes nothing" "$([ -e "$DRAFTDIR" ] && echo present || echo absent)" absent
rm -rf "$DRAFTDIR"
rr --write docs/spikes/issue-drafts -- sh -c "$MKD && yes a | head -c 65536 >/src/docs/spikes/issue-drafts/s5-admission.md"
expect "an exactly 64 KiB draft publishes" "$RS:$(wc -c <"$DRAFTDIR/s5-admission.md" 2>/dev/null | tr -d ' ')" "0:65536"
rm -rf "$DRAFTDIR"

# refused paths: no child, no publication, nothing changed on the host
before=$(tree_hash)
for w in testdata/parser '../x' '/tmp/x' 'testdata/parser/../../x' './testdata/parser' 'testdata/parser/' 'testdata//parser' docs testdata docs/spikes 'docs/spikes/data' 'docs/spikes/issue-drafts/' 'docs/spikes/data/bench-20260101T000000Z/samples'; do
	rr -w write_path_invalid --write "$w" -- true
	if [ "$RS" = 125 ] && ! grep -Eq "$RUNNER_STARTED_RE" "$TMP/rerr"; then pass "write path '$w' refused ($(grep -o 'dev-run: [a-z_]*' "$TMP/rerr" | head -n 1))"; else fail "write path '$w'" "status $RS"; fi
done
expect "refused paths changed nothing on the host" "$(tree_hash)" "$before"
# a symlinked write root or ancestor is refused before Docker as write_root_invalid
mkdir -p "$ISO/docs/spikes"
ln -s "$CANARY" "$DRAFTDIR"
rr -w write_root_invalid --write docs/spikes/issue-drafts -- true
expect "symlinked write root refused" "$RS" 125
reason "symlink root reason" write_root_invalid
rm -f "$DRAFTDIR"
mv "$ISO/docs/spikes" "$ISO/docs/spikes.real"
ln -s "$CANARY" "$ISO/docs/spikes"
rr -w write_root_invalid --write docs/spikes/issue-drafts -- true
expect "symlinked ancestor refused" "$RS" 125
reason "symlink ancestor reason" write_root_invalid
rm -f "$ISO/docs/spikes"
mv "$ISO/docs/spikes.real" "$ISO/docs/spikes"
expect "canary directory untouched by refusals" "$(find "$CANARY" | LC_ALL=C sort | tr '\n' ' ')" "$CANARY $CANARY/canary.txt "
rm -rf "$DRAFTDIR"

# owned removal: a checked removal that fails ends the run cleanup_failed after the child ran; the mutant ignores that failure
variant fault-rm 'timeout 60 docker rm -f "$CTR" >/dev/null 2>&1 || die cleanup_failed' 'false || die cleanup_failed'
variant mut-cleanup-ignored 'timeout 60 docker rm -f "$CTR" >/dev/null 2>&1 || die cleanup_failed' 'false || :'
a_rm_fault() { # holds when a failed checked removal ends the run cleanup_failed
	rrx "$ISO" "$1" -- true
	_cn=$(cname)
	OBS="status $RS, container $_cn, $(grep -o 'dev-run: [a-z_]*' "$TMP/rerr" | tail -n 1)"
	[ -n "$_cn" ] || { SETUP=1; return 1; }
	{ { [ "$RS" = 125 ] && grep -Fq 'dev-run: cleanup_failed' "$TMP/rerr"; } || [ "$RS" = 0 ]; } && reached
	[ "$RS" = 125 ] && grep -Fq 'dev-run: cleanup_failed' "$TMP/rerr"
}
redgreen "ignored-cleanup mutant" a_rm_fault "$MUT/mut-cleanup-ignored.sh" "$MUT/fault-rm.sh"

# ===== 6. redaction and least environment through the real runner (T02) ========================================
S=zq-synthetic-secret-7X
secret_run() { # secret_run [-w WORD] [-s STATUS] VALUE ARGS...: parent secret via a selected variable name; a fresh plain case whose target is plain_target and whose intended status is declared as for rr
	_srw=
	_srs=
	while :; do
		case ${1:-} in
		-w)
			_srw=$2
			shift 2
			;;
		-s)
			_srs=$2
			shift 2
			;;
		*) break ;;
		esac
	done
	_v=$1
	shift
	rg_reset
	(cd "$ISO" && BOXSET_TEST_SECRET=$_v "$RUNNER" "$@") >"$TMP/rout" 2>"$TMP/rerr"
	RS=$?
	plain_seen "$_srw" "$_srs"
}
secret_run "$S" -- sh -c 'printf %s "$1"' x "$S"
expect "a secret the child prints is exactly the marker" "$RS:$(cat "$TMP/rout")" "0:[REDACTED]"
secret_run "$S" -- sh -c 'printf pre-; printf zq-synthetic; sleep 1; printf %s -secret-7X; printf -- -post'
expect "a secret split across writes is masked with prefix and suffix preserved" "$RS:$(cat "$TMP/rout")" "0:pre-[REDACTED]-post"
secret_run ab -- sh -c 'printf xaby'
expect "a two-byte secret is masked" "$(cat "$TMP/rout")" "x[REDACTED]y"
secret_run 'line1
line2' -- sh -c 'printf "a line1\nline2 b"'
expect "a multiline secret is masked whole" "$(cat "$TMP/rout")" "a [REDACTED] b"
secret_run unused -- sh -c 'printf "%s" "https://user:pw@h.example/p?token=tt&k=v#f=g"'
expect "a URL credential absent from the environment is masked" "$(cat "$TMP/rout")" "https://[REDACTED]@h.example/p?token=[REDACTED]&k=[REDACTED]#f=[REDACTED]"
secret_run https -- sh -c 'printf "%s" "https://u:p@h/x?k=v"'
expect "a selected scheme inside a credential-bearing URL is masked together with the userinfo and the query value" "$RS:$(cat "$TMP/rout")" "0:[REDACTED]://[REDACTED]@h/x?k=[REDACTED]"
secret_run @ -- sh -c 'printf "%s" "https://u:p@h/x?k=v"'
expect "a selected at sign inside a credential-bearing URL keeps the userinfo and the query value masked" "$RS:$(cat "$TMP/rout")" "0:https://[REDACTED]h/x?k=[REDACTED]"
secret_run REDACTED -- sh -c 'printf "1REDACTED2"'
expect "a marker-overlapping secret terminates with one marker" "$RS:$(cat "$TMP/rout")" "0:1[REDACTED]2"
secret_run abc -- sh -c 'printf xabcy'
expect "an unterminated output is redacted" "$(cat "$TMP/rout")" "x[REDACTED]y"
# a plain control whose bytes are right but whose final runner status is not: a runner copy whose checked owned removal always fails, so the run ends cleanup_failed (125) after the correct redacted output. Through
# the real plain classifier that run earns no credit, whatever order its assertions would have been made in.
plain_late_failure_control() {
	_plo=$(
		META=0
		rg_reset
		(cd "$ISO" && BOXSET_TEST_SECRET=ab "$MUT/fault-rm.sh" -- sh -c 'printf xaby') >"$TMP/rout" 2>"$TMP/rerr"
		RS=$?
		plain_seen
		_plc=$(cat "$TMP/rout")
		{ [ "$_plc" = 'x[REDACTED]y' ] && [ "$RS" = 125 ] && grep -Fq 'dev-run: cleanup_failed' "$TMP/rerr"; } && printf 'PREMISE\n'
		pass "a two-byte secret is masked (late cleanup failure)"
		docker rm -f "$(cname)" >/dev/null 2>&1
	)
	META=1
	expect "plain late-failure control: the premise holds (correct redacted bytes, final status 125 cleanup_failed, from the real provider)" "$(printf '%s\n' "$_plo" | grep -Fxc PREMISE)" 1
	expect "plain late-failure control: the credit of those correct bytes is refused because the final status is not the intended 0" "$(printf '%s\n' "$_plo" | grep -Fxc 'ok - a two-byte secret is masked (late cleanup failure)')" 0
	META=0
}
plain_late_failure_control
rr -- sh -c 'head -c 262144 /dev/zero | tr "\0" a'
expect "exactly 256 KiB raw passes" "$RS:$(wc -c <"$TMP/rout" | tr -d ' ')" "0:262144"
rr -w output_overflow -- sh -c 'head -c 262145 /dev/zero | tr "\0" a'
expect "256 KiB + 1 raw fails" "$RS" 125
reason "output overflow reason word" output_overflow
gone "$(cname)" "overflow: owned container removed at once"
rr -w output_overflow -- sh -c 'head -c 262145 /dev/zero'
expect "262145 NUL bytes fail as output_overflow (binary safe)" "$RS" 125
reason "NUL overflow reason" output_overflow
rr -- sh -c 'head -c 262144 /dev/zero'
expect "262144 NUL bytes pass and are all emitted" "$RS:$(wc -c <"$TMP/rout" | tr -d ' ')" "0:262144"
secret_run -w output_overflow a -- sh -c 'i=0; while [ $i -lt 32768 ]; do printf "a-"; i=$((i + 1)); done'
expect "a one-byte secret expanding past 256 KiB emitted is output_overflow" "$RS" 125
reason "expansion reason" output_overflow
a_unbounded() { # holds when an unbounded producer is cut off as output_overflow, never reported as the host deadline
	rrx "$ISO" "$1" --timeout 20 -- sh -c 'yes'
	OBS="status $RS, $(grep -o 'dev-run: [a-z_]*' "$TMP/rerr" | tail -n 1)"
	{ { [ "$RS" = 125 ] && grep -Fq 'dev-run: output_overflow' "$TMP/rerr"; } || [ "$RS" = 124 ]; } && reached
	[ "$RS" = 125 ] && grep -Fq 'dev-run: output_overflow' "$TMP/rerr"
}
holds "unbounded output stops the proof as output_overflow, not as the host deadline" a_unbounded "$RUNNER"
gone "$(cname)" "flooding child's container is gone"
rg_reset
(cd "$ISO" && "$RUNNER" -- sh -c 'printf out; sleep 1; printf more') >/dev/full 2>"$TMP/rerr"
RS=$?
plain_seen output_failed
expect "stdout to /dev/full is output_failed" "$RS" 125
reason "write failure reason" output_failed
a_redaction() { # holds when the child's secret never reaches the output
	(cd "$ISO" && BOXSET_TEST_SECRET=$S "$1" -- sh -c 'printf %s "$1"' x "$S") >"$TMP/rout" 2>"$TMP/rerr"
	RS=$?
	rerr_seen
	OBS="output '$(head -c 80 "$TMP/rout")'"
	[ "$RS" = 0 ] && [ -s "$TMP/rout" ] && reached
	[ "$RS" = 0 ] && ! grep -Fq "$S" "$TMP/rout" "$TMP/rerr"
}
# the host helper's -cap mode is the shell's only redaction of the child output: a compiled copy that passes the bytes through unredacted (the selection is still validated, -check and -tail unchanged) is RED
gvariant mut-redactor-bypass main.go 'return redactStream(newAutomaton(values), streamOpts{capN: *capN, raw: *raw, tail: *tail}, stdin, stdout)' 'if *capN >= 0 {
		_, _ = io.Copy(stdout, io.LimitReader(stdin, *capN))
		return exitOK
	}
	return redactStream(newAutomaton(values), streamOpts{capN: *capN, raw: *raw, tail: *tail}, stdin, stdout)'
redgreen "redactor-bypass mutant" a_redaction "$MUT/mut-redactor-bypass.sh" "$RUNNER"
# a draft is redacted like the output: an expanding value keeps the published draft, and a URL credential is masked in it
secret_run -s 1 ADM-003 --write docs/spikes/issue-drafts -- sh -c "$MKD && printf 'blocked ADM-003 end' >/src/docs/spikes/issue-drafts/s5-admission.md; exit 1"
expect "an expanding redaction keeps the published draft" "$RS:$(cat "$DRAFTDIR/s5-admission.md" 2>/dev/null)" "1:blocked [REDACTED] end"
rm -rf "$DRAFTDIR"
secret_run -s 1 unused --write docs/spikes/issue-drafts -- sh -c "$MKD && printf 'see https://aaaaaaaaa:bbbbbbbb@h/p?k=cccccccccccccccccccc end' >/src/docs/spikes/issue-drafts/s5-admission.md; exit 1"
expect "a shrinking redaction keeps the published draft" "$RS:$(cat "$DRAFTDIR/s5-admission.md" 2>/dev/null)" "1:see https://[REDACTED]@h/p?k=[REDACTED] end"
rm -rf "$DRAFTDIR"
secret_run "$S" -- env
envs=$(cat "$TMP/rout")
# the environment listing is the target: a failed run with empty output must not credit the absence or exactness assertions
case $envs in *HOME=*) [ "$RS" = 0 ] || SETUP=1 ;; *) SETUP=1 ;; esac
if printf '%s\n' "$envs" | grep -Fq "$S"; then fail "parent secret in child environment" "value present"; else pass "parent secret value is absent from the child"; fi
bad=
for line in $envs; do
	case ${line%%=*} in
	PATH | HOME | TMPDIR | LANG | LC_ALL | GOPATH | GOCACHE | GOMODCACHE | GOFLAGS | GOPROXY | GOSUMDB | GOTOOLCHAIN | GOTELEMETRY | GOENV | GOLANGCI_LINT_CACHE | BUN_INSTALL_CACHE_DIR | BUN_RUNTIME_TRANSPILER_CACHE_PATH | XDG_CACHE_HOME | DO_NOT_TRACK | PLAYWRIGHT_BROWSERS_PATH | PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD | HOSTNAME | PWD | SHLVL | _) ;;
	*) bad="$bad ${line%%=*}" ;;
	esac
done
expect "child environment is exactly the enumerated set" "$bad" ""
for v in PATH HOME TMPDIR LANG LC_ALL GOFLAGS GOPROXY GOTOOLCHAIN PLAYWRIGHT_BROWSERS_PATH GOCACHE; do
	case $envs in *"$v="*) pass "required variable $v survives" ;; *) fail "required variable $v" "missing" ;; esac
done

# nested secret-shaped files never enter the image
mkdir -p "$ISO/web/src" "$ISO/internal/x" "$ISO/bench" "$ISO/testdata"
for f in web/.env web/.env.local web/src/.env.production internal/x/.env bench/k.pem testdata/.npmrc; do
	printf 'NESTED_SENTINEL_%s=zz\n' "$(printf '%s' "$f" | tr '/.' '__')" >"$ISO/$f"
done
rr -- sh -c 'test -z "$(find /src \( -name ".env" -o -name ".env.*" -o -name "*.pem" -o -name "*.key" -o -name ".npmrc" -o -name ".netrc" \) -not -path "*/node_modules/*" -print)" && ! grep -rqs --exclude-dir=node_modules NESTED_SENTINEL /src'
expect "nested web/.env, web/.env.local, web/src/.env.production, internal/x/.env, bench/k.pem and testdata/.npmrc never enter the image" "$RS" 0
rm -rf "$ISO/web/src" "$ISO/internal/x" "$ISO/bench/k.pem" "$ISO/web/.env" "$ISO/web/.env.local" "$ISO/testdata"
rr -- sh -c 'test ! -e /var/run/docker.sock && ! command -v node >/dev/null && ! command -v docker >/dev/null && test ! -e /src/.env && test ! -e /src/reference && test ! -e /src/prototypes && test ! -e /src/design && test ! -e /src/.devrites && test ! -e /src/scripts/dev-run.sh && test ! -e /src/scripts/dev-run-test.sh && ! grep -rqs --exclude-dir=node_modules OWNER_SECRET_CANARY /src'
expect "no Node, Docker socket, .env, reference, prototypes, workspaces or runner/driver in the image" "$RS" 0
rr -- sh -c 'test -x /usr/local/libexec/boxset/capture-check && test ! -e /usr/local/bin/redact && test ! -e /usr/local/libexec/boxset/redact && test ! -e /src/redact'
expect "capture validator present and the host redactor absent from the proof image" "$RS" 0
rr -- sh -c "test \"\$(ls /ms-playwright)\" = chromium_headless_shell-$REV && test ! -e /ms-playwright/chromium-$REV"
expect "image holds only chromium_headless_shell and no full Chrome directory" "$RS" 0

# failing builds and start diagnostics are redacted tails
fiso fault-build
# the injected failing step prints its flood first and a credential-bearing URL line last, so the bounded tail still holds the line the redactor must mask
printf '\nRUN head -c 100000 /dev/zero | tr "\\0" x >&2; printf "\\nhttps://user:pw@h.example/x?token=tt\\n" >&2; exit 1\n' >>"$FISO/deploy/dev/Dockerfile"
rg_reset
(cd "$FISO" && BOXSET_TEST_SECRET=zz-build-secret "$FISO/scripts/dev-run.sh" -- true) >"$TMP/rout" 2>"$TMP/xerr"
RS=$?
# build_failed is this fault copy's intended outcome: only that exact line is exempt from infrastructure attribution (an earlier redactor,
# host or setup failure stays contamination), and the injected failing step must have run: its credential-bearing output line is in the tail, masked
if intended_refusal build_failed "$TMP/xerr" && grep -Fq 'https://[REDACTED]@h.example/x?token=[REDACTED]' "$TMP/xerr"; then reached; fi
expect "a failing image build is build_failed" "$RS" 125
if grep -Fq 'dev-run: build_failed' "$TMP/xerr"; then pass "build failure reason"; else fail "build failure reason" "stderr lacks 'dev-run: build_failed': $(head -c 400 "$TMP/xerr")"; fi
if grep -Fq 'user:pw' "$TMP/xerr"; then fail "build log credential" "URL userinfo leaked"; else pass "build diagnostics carry no URL credential"; fi
if grep -Eq '^\[dev-run: [0-9]+ earlier bytes omitted\]$' "$TMP/xerr" && [ "$(wc -c <"$TMP/xerr")" -le 70000 ]; then pass "build diagnostics are a bounded tail with the omitted-count line"; else fail "build tail" "size $(wc -c <"$TMP/xerr")"; fi
rm -rf "$FISO"

# ===== 8. destination and flag refusals =========================================================================
mkdir -p "$ISO/.devrites/work/boxset-test"
before=$(tree_hash)
for bad in "/tmp/screenshots-20260101T000000Z" ".devrites/work/boxset-test/../x/screenshots-20260101T000000Z" \
	".devrites/work/boxset-test/screenshots-2026" ".devrites/work/Bad_Slug/screenshots-20260101T000000Z" \
	".devrites/work/missing-parent/screenshots-20260101T000000Z" "./.devrites/work/boxset-test/screenshots-20260101T000000Z"; do
	rr -w capture_destination_invalid --capture "$bad" -- true
	if [ "$RS" = 125 ] && grep -Fq 'dev-run: capture_destination_invalid' "$TMP/rerr"; then pass "capture destination '$bad' refused"; else fail "capture destination '$bad'" "status $RS"; fi
done
existing=.devrites/work/boxset-test/screenshots-20260101T000000Z
mkdir "$ISO/$existing"
rr -w capture_destination_exists --capture "$existing" -- true
if [ "$RS" = 125 ] && grep -Fq 'dev-run: capture_destination_exists' "$TMP/rerr"; then pass "existing capture DIR refused"; else fail "existing DIR" "status $RS"; fi
rmdir "$ISO/$existing"
linkdir=.devrites/work/boxset-test/screenshots-20260101T000001Z
ln -s "$CANARY" "$ISO/$linkdir"
rr -w capture_destination_exists --capture "$linkdir" -- true
if [ "$RS" = 125 ] && grep -Fq 'dev-run: capture_destination_exists' "$TMP/rerr"; then pass "symlink capture DIR refused"; else fail "symlink DIR" "status $RS"; fi
rm -f "$ISO/$linkdir"
rr -w capture_flag_invalid --capture ".devrites/work/boxset-test/screenshots-20260101T000002Z" --bench 0-3 -- true
if [ "$RS" = 125 ] && grep -Fq 'dev-run: capture_flag_invalid' "$TMP/rerr"; then pass "--capture with --bench refused"; else fail "capture+bench" "status $RS"; fi
rr -w capture_flag_invalid --capture ".devrites/work/boxset-test/screenshots-20260101T000003Z" --capture ".devrites/work/boxset-test/screenshots-20260101T000004Z" -- true
if [ "$RS" = 125 ] && grep -Fq 'dev-run: capture_flag_invalid' "$TMP/rerr"; then pass "duplicate --capture refused"; else fail "duplicate capture" "status $RS"; fi
rr -w bench_invalid --bench 0-3 -- true
if [ "$RS" = 125 ] && grep -Fq 'dev-run: bench_invalid side=write' "$TMP/rerr"; then pass "--bench without --write is refused before Docker"; else fail "bench" "status $RS"; fi
mkdir -p "$TMP/shim"
for t in sh docker id mkdir rmdir ln rm mv head tail wc cat sha256sum timeout date grep find tr od dirname; do
	p=$(command -v "$t") && ln -sf "$p" "$TMP/shim/$t"
done
rg_reset
(cd "$ISO" && PATH=$TMP/shim "$RUNNER" --capture ".devrites/work/boxset-test/screenshots-20260101T000005Z" -- true) >"$TMP/rout" 2>"$TMP/xerr"
RS=$?
# the host-tool refusal is the intended outcome of these shim runs, judged on their own stderr
intended_refusal host_tool_missing "$TMP/xerr" && reached
if [ "$RS" = 125 ] && grep -Fq 'dev-run: host_tool_missing' "$TMP/xerr"; then pass "missing host tool refused before Docker (capture)"; else fail "host tool" "status $RS"; fi
rg_reset
(cd "$ISO" && PATH=$TMP/shim "$RUNNER" -- true) >"$TMP/rout" 2>"$TMP/xerr"
RS=$?
intended_refusal host_tool_missing "$TMP/xerr" && reached
if [ "$RS" = 125 ] && grep -Fq 'dev-run: host_tool_missing' "$TMP/xerr"; then pass "missing host tool refused before Docker"; else fail "host tool" "status $RS"; fi
[ -d "$ISO/.devrites/work/boxset-test" ] || SETUP=1
expect "refusals created no capture directory" "$(find "$ISO/.devrites" -mindepth 3 | wc -l | tr -d ' ')" 0
expect "refusals changed nothing else" "$(tree_hash)" "$before"

# ===== 9. capture provider: real pinned chrome-headless-shell, no Bun, no Playwright ==============================
HEAD_JS="B=/ms-playwright/chromium_headless_shell-$REV/chrome-headless-shell-linux64/chrome-headless-shell
if [ ! -x \"\$B\" ] || ! \"\$B\" --version >/dev/null 2>&1; then echo T31-PROVIDER-UNAVAILABLE; exit 3; fi
mkdir -p /tmp/boxset-shot
"
SHOOT_FN='
shoot() {
	name=$1; mode=$2; vp=$3; w=$4; h=$5
	bits=; rest=$BOXSET_CAPTURE_CHALLENGE
	while [ -n "$rest" ]; do
		c=${rest%"${rest#?}"}; rest=${rest#?}
		case $c in
		0) q=0000;; 1) q=0001;; 2) q=0010;; 3) q=0011;; 4) q=0100;; 5) q=0101;; 6) q=0110;; 7) q=0111;;
		8) q=1000;; 9) q=1001;; a) q=1010;; b) q=1011;; c) q=1100;; d) q=1101;; e) q=1110;; f) q=1111;;
		esac
		bits=$bits$q
	done
	bits=$bits$mode$vp
	html=/tmp/$name.html
	{
		printf "<!doctype html><meta charset=utf-8><style>html,body{margin:0;background:#fff}div{position:fixed;width:8px;height:8px}p{position:fixed;left:8px;margin:0;font:14px sans-serif;color:#000}</style>"
		printf "<p style=\"top:%spx\">boxset-capture %s %s.png %sx%s@1</p>" $((h-48)) "$BOXSET_CAPTURE_CHALLENGE" "$name" "$w" "$h"
		k=0; rest=$bits
		while [ -n "$rest" ]; do
			bit=${rest%"${rest#?}"}; rest=${rest#?}
			if [ "$bit" = 1 ]; then col="#000"; else col="#fff"; fi
			printf "<div style=\"left:%spx;top:%spx;background:%s\"></div>" $((8*(k%33))) $((h-16+8*(k/33))) "$col"
			k=$((k+1))
		done
	} >"$html"
	"$B" --headless --no-sandbox --hide-scrollbars --force-device-scale-factor=1 --window-size="$w,$h" --screenshot="/tmp/boxset-shot/$name.png" "file://$html" >/dev/null 2>&1
	[ -s "/tmp/boxset-shot/$name.png" ] || { echo T31-PROVIDER-UNAVAILABLE; exit 3; }
	cat "/tmp/boxset-shot/$name.png" >"/capture/$name.png" || exit 1
}
shoot_all() { shoot dev-desktop 0 0 1440 900; shoot dev-mobile 0 1 390 844; shoot preview-desktop 1 0 1440 900; shoot preview-mobile 1 1 390 844; }
'
BASE="$HEAD_JS$SHOOT_FN"

provider_unavailable() { [ "$RS" = 3 ] && grep -Fq T31-PROVIDER-UNAVAILABLE "$TMP/rout"; }
capture_run() { # capture_run [-w WORD] [-s STATUS] SCRIPT [runner args]: sets CAP RS; the case's target is plain_target (the child started, or the runner's own fixed WORD) and its intended status is declared as for rr
	_cra=
	while :; do
		case ${1:-} in
		-w | -s)
			_cra="$_cra $1 $2"
			shift 2
			;;
		*) break ;;
		esac
	done
	_script=$1
	shift
	CAP=$(capdir)
	# shellcheck disable=SC2086
	rr $_cra --capture "$CAP" "$@" -- sh -c "$BASE$_script"
}
assert_no_publication() { # assert_no_publication NAME
	if [ ! -e "$ISO/$CAP" ] && [ ! -L "$ISO/$CAP" ]; then pass "$1: nothing published, DIR removed"; else fail "$1" "DIR still present"; fi
}

capture_run 'shoot_all'
if provider_unavailable; then
	printf 'T31-PROVIDER-UNAVAILABLE: pinned chrome-headless-shell binary or flag missing\n'
	exit 3
fi
expect "provider capture exits 0" "$RS" 0
pass_line=$(grep '^DEV-RUN-CAPTURE-PASS' "$TMP/rout")
ch=${pass_line##*challenge=}
if grep -Eq "^DEV-RUN-CAPTURE-PASS files=4 bytes=[0-9]+ dir=$CAP challenge=[0-9a-f]{16}\$" "$TMP/rout"; then pass "capture PASS line is the runner's locator"; else fail "capture PASS line" "$(cat "$TMP/rout")"; fi
D=$ISO/$CAP
# The published directory is the target of every dependent observation below. When the capture did not complete (status,
# PASS locator and published directory) that failure is already retained above, and no dependent file-shape observation may
# be credited vacuously from a missing directory or abort the proof on a missing file.
CAPOK=0
if [ "$RS" = 0 ] && [ -n "$pass_line" ] && [ -d "$D" ]; then CAPOK=1; else SETUP=1; fi
expect "exactly five regular published entries" "$(find "$D" -mindepth 1 2>/dev/null | LC_ALL=C sort | tr '\n' ' ')" "$D/capture.manifest $D/dev-desktop.png $D/dev-mobile.png $D/preview-desktop.png $D/preview-mobile.png "
expect "no non-regular entries" "$(find "$D" -mindepth 1 ! -type f | wc -l | tr -d ' ')" 0
expect "files are 0644" "$(find "$D" -mindepth 1 ! -perm 644 | wc -l | tr -d ' ')" 0
expect "DIR is 0700" "$(find "$D" -maxdepth 0 -perm 700 | wc -l | tr -d ' ')" 1
expect "dev-desktop is 1440x900" "$(png_dims "$D/dev-desktop.png")" 1440x900
expect "preview-desktop is 1440x900" "$(png_dims "$D/preview-desktop.png")" 1440x900
expect "dev-mobile is 390x844" "$(png_dims "$D/dev-mobile.png")" 390x844
expect "preview-mobile is 390x844" "$(png_dims "$D/preview-mobile.png")" 390x844
want="boxset-capture 1
challenge $ch"
for n in dev-desktop dev-mobile preview-desktop preview-mobile; do
	if [ -f "$D/$n.png" ]; then
		set -- $(sha256sum "$D/$n.png")
		want="$want
$1 $(wc -c <"$D/$n.png" | tr -d ' ') $n.png"
	else
		SETUP=1
		want="$want
missing $n.png"
	fi
done
expect "manifest matches recomputed hashes, sizes and challenge" "$(cat "$D/capture.manifest" 2>/dev/null)" "$want"
replay_b64=
if [ "$CAPOK" = 1 ]; then
	cp "$D/dev-desktop.png" "$TMP/replay-A.png"
	replay_b64=$(base64 -w0 "$TMP/replay-A.png")
fi

# --- capture controls through the same runner (T31) -------------------------------------------------
capture_run -w capture_missing 'shoot_all; : >/capture/preview-mobile.png'
expect "an empty (truncated) preview-mobile is capture_missing" "$RS" 125
reason "empty reason" capture_missing
assert_no_publication "empty file"
capture_run -w capture_not_png 'shoot_all; cat /etc/passwd >/capture/dev-mobile.png'
expect "/etc/passwd bytes written in place are capture_not_png" "$RS" 125
reason "passwd reason" capture_not_png
if grep -Fq 'root:' "$TMP/rout" "$TMP/rerr"; then fail "passwd bytes" "leaked to output"; else pass "passwd bytes never published"; fi
assert_no_publication "passwd bytes"
capture_run -w capture_oversize 'shoot_all; head -c 2097153 /dev/zero >/capture/dev-mobile.png'
expect "2097153 bytes in place are capture_oversize" "$RS" 125
reason "oversize reason" capture_oversize
assert_no_publication "oversize"
capture_run -w capture_dimensions 'shoot_all; shoot dev-mobile 0 1 390 845'
expect "wrong dimensions refused" "$RS" 125
reason "dimensions reason" capture_dimensions
assert_no_publication "wrong dimensions"
APNG='cat > /tmp/apng.go <<"GO"
package main

import (
	"encoding/binary"
	"hash/crc32"
	"os"
)

func main() {
	d, err := os.ReadFile(os.Args[1])
	if err != nil {
		os.Exit(1)
	}
	chunk := binary.BigEndian.AppendUint32(nil, 8)
	chunk = append(chunk, "acTL"...)
	chunk = binary.BigEndian.AppendUint32(chunk, 1)
	chunk = binary.BigEndian.AppendUint32(chunk, 0)
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(chunk[4:]))
	at := 8 + 25
	out := append(append(append([]byte{}, d[:at]...), chunk...), d[at:]...)
	if err := os.WriteFile(os.Args[1], out, 0o644); err != nil {
		os.Exit(1)
	}
}
GO
(cd /tmp && go run apng.go /capture/dev-mobile.png)
'
capture_run -w capture_not_static "shoot_all; $APNG"
expect "APNG refused" "$RS" 125
reason "APNG reason" capture_not_static
assert_no_publication "APNG"
capture_run -w capture_challenge_mismatch "shoot_all; printf %s '$replay_b64' | base64 -d >/capture/dev-desktop.png"
# the replayed capture comes from the first capture: when that one did not complete there is nothing to replay, whatever the reset above cleared
[ -n "$replay_b64" ] || SETUP=1
expect "run-A replay under run-B challenge refused" "$RS" 125
reason "replay reason" capture_challenge_mismatch
assert_no_publication "replay"
capture_run -w capture_missing 'shoot_all; echo "DEV-RUN-CAPTURE-PASS files=4 bytes=1 dir=x challenge=0000000000000000"; : >/capture/dev-mobile.png'
expect "fake printed PASS without real files is refused" "$RS" 125
assert_no_publication "fake PASS"
capture_run -s 7 'shoot_all; exit 7'
expect "nonzero child exit propagates over valid files" "$RS" 7
assert_no_publication "nonzero child"
capture_run -s 124 'shoot_all; sleep 100' --timeout 3
expect "hung child times out" "$RS" 124
noreason "hung child's deadline prints no reason word" timeout
assert_no_publication "hung child"
gone "$(cname)" "hung child's container is gone"
rr -s 3 --capture "$(capdir)" -- sh -c 'B=/nonexistent; if [ ! -x "$B" ]; then echo T31-PROVIDER-UNAVAILABLE; exit 3; fi'
if [ "$RS" = 3 ] && grep -Fq T31-PROVIDER-UNAVAILABLE "$TMP/rout"; then pass "unavailable provider is exit 3, distinct from an assertion failure"; else fail "provider unavailable" "status $RS"; fi
capture_run -s 1 'shoot_all; false'
expect "a failed in-place copy or assertion is exit 1, not 3" "$RS" 1

# ---- closure-window helpers: cf_natural, cf_terminal and cf_final are the producers the permanent self-check above extracts from this file and perturbs.
# No full-run case uses them: the shell lifecycle has no reconciliation window (it removes its one owned container by label on exit).
# cf_natural NAME: the delayed cell is natural. Every S record of an exact-name `ps` of NAME in the shim's $TMP/dlog has exactly one well-formed E record with the same token in
# $TMP/dlog.q whose END is not before its START; there is no orphan, malformed or duplicate token; and there are at least two queries, each with status 0, the answer [] and
# END + 10 ms no later than the effect start, so each really saw the absence. Anything else is setup naming the tokens.
cf_natural() {
	_cne=$(cat "$RECEIPT.t" 2>/dev/null)
	if ! mono_parse "$_cne"; then
		OBS="$OBS; no effect start stamp"
		SETUP=1
		return 1
	fi
	_cns=$MONO_NS
	_cnn=0
	_cnbad=0
	_cntok=
	while read -r _cqk _cqt _cqs _cqr; do
		[ "$_cqk" = S ] || continue
		case $_cqr in "ps -a --filter name=^$1\$ --format {{.Names}}") ;; *) continue ;; esac
		_cnn=$((_cnn + 1))
		_cntok="$_cntok $_cqt"
		_cqc=$(grep -c "^E $_cqt " "$TMP/dlog.q" 2>/dev/null)
		[ "$_cqc" = 1 ] || { _cnbad=$((_cnbad + 1)); continue; }
		read -r _cqk2 _cqt2 _cqe _cqst _cqa <<EOF
$(grep "^E $_cqt " "$TMP/dlog.q")
EOF
		mono_parse "$_cqs" && _cqsn=$MONO_NS || _cqsn=
		if [ "$_cqst" != 0 ] || [ "$_cqa" != '[]' ] || ! mono_parse "$_cqe" || [ "$((MONO_NS + MONO_RES))" -gt "$_cns" ]; then
			_cnbad=$((_cnbad + 1))
		fi
		{ [ -n "$_cqsn" ] && [ "$MONO_NS" -ge "$_cqsn" ]; } || _cnbad=$((_cnbad + 1))
	done <"$TMP/dlog"
	# no duplicate token, and no E record without an S record of the same token
	[ -z "$(cut -d' ' -f2 "$TMP/dlog.q" 2>/dev/null | sort | uniq -d)" ] || _cnbad=$((_cnbad + 1))
	while read -r _cok _cot _cor; do
		[ "$_cok" = E ] || { _cnbad=$((_cnbad + 1)); continue; }
		[ "$(grep -c "^S $_cot " "$TMP/dlog" 2>/dev/null)" = 1 ] || _cnbad=$((_cnbad + 1))
	done <"$TMP/dlog.q"
	if [ "$_cnn" -lt 2 ] || [ "$_cnbad" != 0 ]; then
		OBS="$OBS; not a natural absent-before-effect cell ($_cnn queries, $_cnbad failed, unmatched, malformed, answered or completed after the effect began; tokens$_cntok)"
		SETUP=1
		return 1
	fi
	OBS="$OBS; $_cnn queries, each with one well-formed receipt, status 0 and the answer [], completed before the effect began (tokens$_cntok)"
	return 0
}
# cf_terminal NAME: CF_FIN is the bounded terminal state: status-checked queries (an unknown one is recorded, never absence) until NAME is absent,
# inside ONE absolute 100 monotonic seconds. Each query is bounded by the budget that is left (never longer, floor charged), an answer read after
# the budget ended is no observation, and a failed or expired observation sets SETUP here, where no caller can drop it: the effect of a late create
# ends by the container's own expiry.
cf_terminal() {
	CF_FIN=unknown
	mono_ns || { SETUP=1; return 1; }
	_ctend=$((MONO_NS + 100000000000))
	while :; do
		mono_ns || { SETUP=1; return 1; }
		_ctl=$((_ctend - MONO_NS - MONO_RES))
		[ "$_ctl" -ge 1000000000 ] || { SETUP=1; return 1; }
		[ "$_ctl" -le 30000000000 ] || _ctl=30000000000
		ns_budget "$_ctl"
		CF_FIN=$(cstate_to "$BUDGET" "$1")
		mono_ns || { CF_FIN=unknown; SETUP=1; return 1; }
		[ "$((MONO_NS + MONO_RES))" -le "$_ctend" ] || { CF_FIN=unknown; SETUP=1; return 1; }
		case $CF_FIN in
		absent) return 0 ;;
		unknown) unknown_obs || SETUP=1 ;;
		esac
		sleep 2
	done
}
# cf_final NAME: _fin is the final state of the issued NAME before any driver removal. A natural cell reads the bounded terminal observation, whose
# failure or expiry is setup and never absence; every other cell reads one status-checked query.
cf_final() {
	if [ "$CF_NATURAL" = 1 ]; then
		cf_terminal "$1" || { OBS="$OBS; the terminal observation of $1 failed or expired"; return 1; }
		_fin=$CF_FIN
	else
		cstate "$1"
		_fin=$CST
	fi
}
# ---- host-loss window helpers: hl_running, hl_register, hl_err_taint, hl_watcher and hl_start are the producers the permanent self-check above extracts from this file and perturbs.
# No full-run case uses them: the shell leaves no in-container backstop to observe after the runner is killed.
HL_S=
HL_R=
hl_running() { # hl_running NAME: HL_R is the upper bound of NAME's start event. The answer must be status 0 and exactly `true` plus LF; then R is the next driver reading plus one clock floor
	HL_R=
	_hrt=$(printf 'true\nx')
	timeout 30 docker container inspect --format '{{.State.Running}}' "$1" >"$TMP/hlr.out" 2>/dev/null || return 1
	[ "$(cat "$TMP/hlr.out"; printf x)" = "$_hrt" ] || return 1
	mono_ns || return 1
	HL_R=$((MONO_NS + MONO_RES))
}
# shellcheck disable=SC2034 # HL_S is the shim start reading of the registered case, set for the callers that judge the kill time
hl_register() { # hl_register KEY NAME DLOG R D_NS: the monotonic window [S+D, R+D] of NAME's own deadline. S is the start reading of the single logging-shim run record naming NAME in DLOG; any other shape (no record, two, a malformed reading, R before S) is a refused registration, a case-owned setup taint, never narrowed or replaced by a fallback
	_hk=$1
	_hn=$2
	_hd=$3
	_hr=$4
	_hdn=$5
	_hc=$(grep -Ec -- "^S [^ ]+ [0-9]+\.[0-9][0-9] run .* --name $_hn " "$_hd" 2>/dev/null)
	_hl=$(grep -E -- "^S [^ ]+ [0-9]+\.[0-9][0-9] run .* --name $_hn " "$_hd" 2>/dev/null)
	if [ "$_hc" = 1 ]; then
		set -- $_hl
		if mono_parse "$3"; then _hs=$MONO_NS; else _hc=malformed; fi
	fi
	if [ "$_hc" != 1 ]; then
		{ hl_taint "$_hk" setup "no single logging-shim run record for $_hn ($_hc)"; return 1; }
	fi
	[ "$_hr" -ge "$_hs" ] || { hl_taint "$_hk" setup "the running witness precedes the shim start reading"; return 1; }
	HL_S=$_hs
	printf '%s %s %s\n' "$_hn" "$((_hs + _hdn))" "$((_hr + _hdn))" >"$HLD/$_hk.reg"
}
hl_err_taint() { # hl_err_taint KEY RUNNAME: the runner stderr of that launch carries an infrastructure failure or a controller fatal trace
	! grep -Eq "$INFRA_RE" "$TMP/$2.err" 2>/dev/null || hl_taint "$1" setup "runner infrastructure failure"
	! grep -Eq "$CTRL_FATAL_RE" "$TMP/$2.err" 2>/dev/null || hl_taint "$1" setup "controller fatal output"
}
hl_watcher() { # one bounded query per pending key every 2 s: a query never starts in the unobservable final second and is never longer than the monotonic time left (minus the clock floor) to its own deadline; a failed, malformed, backward or late observation is terminal unknown
	set +f # this background scope globs the registration files; the driver's -f stays on everywhere else
	while [ -d "$HLD" ] && [ ! -e "$HLD/stop" ]; do
		for _f in "$HLD"/*.reg; do
			[ -e "$_f" ] || continue
			[ -s "$_f" ] || continue
			_k=${_f##*/}
			_k=${_k%.reg}
			[ -e "$HLD/$_k.res" ] && continue
			read -r _nm _lo _hi <"$_f"
			true >"$HLD/$_k.seen"
			if ! mono_ns; then
				printf 'unknown 0 %s\n' "$_lo" >"$HLD/$_k.res"
				continue
			fi
			_t=$MONO_NS
			if [ "$_t" -ge "$_hi" ]; then
				# the true time is at or past the true deadline: this query starts at or after it
				_s=$(cstate_to 30 "$_nm")
				mono_ns || _s=unknown
				printf '%s %s %s\n' "$(hl_terminal "$_s")" "$MONO_NS" "$_lo" >"$HLD/$_k.res"
				continue
			fi
			_rem=$((_lo - _t - MONO_RES))
			if [ "$_rem" -lt 1000000000 ]; then
				sleep 1
				continue
			fi
			[ "$_rem" -le 30000000000 ] || _rem=30000000000
			ns_budget "$_rem"
			_s=$(cstate_to "$BUDGET" "$_nm")
			mono_ns || _s=unknown
			if [ "$_s" = unknown ] || [ "$((MONO_NS + MONO_RES))" -gt "$_lo" ]; then
				printf 'unknown %s %s\n' "$MONO_NS" "$_lo" >"$HLD/$_k.res"
			elif [ "$_s" = absent ]; then
				printf 'gone %s %s\n' "$MONO_NS" "$_lo" >"$HLD/$_k.res"
			fi
		done
		sleep 2
	done
}
# shellcheck disable=SC2034 # HL_PID is the held case's runner, set for the callers that kill it
hl_start() { # hl_start NAME RUNNER ARGS...: runs a case with the logging shim to readiness; sets HL_PID HL_CID. A readiness probe that failed (any status but an exact present or absent) is that
	# case's own unknown observation, kept in its deferred taint whatever a later probe shows
	_hsn=$1
	_hsr=$2
	shift 2
	bg_runlog "$_hsn" "$_hsr" "$@"
	HL_PID=$BG_PID
	HL_CID=$(bg_ctr "$_hsn") || { fail "host loss $_hsn" "no container"; return 1; }
	bg_wait "$HL_CID" /tmp/gready
	_hsw=$?
	[ "$BW_UNK" = 0 ] || hl_taint "$_hsn" unknown "readiness probe of $HL_CID failed"
	[ "$_hsw" = 0 ] || { fail "host loss $_hsn" "child and detached grandchild never ready"; return 1; }
	return 0
}

# ===== 11. validator and redactor tests through the same runner (T31/T02 provider controls) =====================
mkdir -p "$ISO/deploy/dev"
rr --timeout 300 -- go test ./deploy/dev/capture-check ./deploy/dev/redact -count=1 -v
passed=$(grep -c '^--- PASS: ' "$TMP/rout")
expect "capture-check and redact tests run through the runner and pass" "$RS" 0
if [ "$passed" -ge 30 ] && ! grep -Eq '^--- (FAIL|SKIP)' "$TMP/rout" && [ "$(grep -c '^ok ' "$TMP/rout")" -ge 2 ]; then pass "validator and redactor report $passed passing top-level tests, no failures or skips"; else fail "validator/redactor results" "passed=$passed (want at least 30)"; fi
# a_go_test DIR NAME PKG SELECTOR INTENDED: SELECTOR is the -run regex that executes the Go test whose exact name is NAME.
# It holds on status 0 and the named PASS record. A named FAIL record is attributed to the intended assertion only when
# that assertion's own failure message (INTENDED, an ERE over the test's `file_test.go:LINE: message` output) is in the
# named test's output and nothing panicked: a build, import, setup or collection failure (no named record) and a fixture,
# helper or panic failure inside the same named test are setup, never RED.
a_go_test() {
	rrx "$1" "$1/scripts/dev-run.sh" --timeout 300 -- go test "$3" -run "$4" -count=1 -timeout 60s -v
	OBS="go test status $RS: $(grep -m1 -E '^(--- FAIL|FAIL|panic|# )' "$TMP/rout")"
	if [ "$RS" = 0 ] && grep -Eq "^--- PASS: $2 \(" "$TMP/rout"; then
		reached
		return 0
	fi
	grep -Eq "^--- FAIL: $2 \(" "$TMP/rout" || { SETUP=1; return 1; }
	if grep -Eq '^(panic:|[[:space:]]+panic:)|\[recovered\]' "$TMP/rout" || ! grep -Eq "^[[:space:]]+[a-z_]+_test\.go:[0-9]+: $5" "$TMP/rout"; then
		OBS="$OBS; the named test failed outside the intended assertion (fixture, helper or panic)"
		SETUP=1
		return 1
	fi
	reached
	return 1
}
a_animated() { a_go_test "$1" TestRejectsMalformedAndAnimated ./deploy/dev/capture-check '^TestRejectsMalformedAndAnimated$' 'status [0-9]+ \([a-z_]*\) with [0-9]+ output bytes, want [0-9]+ [a-z_]+ and none'; }
a_normalize() { a_go_test "$1" TestSixteenBitAlphaIsJudgedBeforeNormalization ./deploy/dev/capture-check '^TestSixteenBitAlphaIsJudgedBeforeNormalization$' 'alpha 0x[0-9a-f]+: status [0-9]+ \('; }
a_runs() { a_go_test "$1" TestOverlapsAndMarkerLookalikes ./deploy/dev/redact '^TestOverlapsAndMarkerLookalikes$' '(status [0-9]+ output .*, want 0 and |split at [0-9]+: status|one byte per read: status)'; }
a_compose() { a_go_test "$1" TestSelectedValuesInsideURLsDoNotHideThem ./deploy/dev/redact '^TestSelectedValuesInsideURLsDoNotHideThem$' '(status [0-9]+ output .*, want 0 and |split at [0-9]+: status|one byte per read: status|userinfo survived|emitted exactly|emitted cap\+1|tail: status)'; }
# a_compose_consumers DIR: the selected scheme and a credential-bearing URL through the real runner of the
# checkout DIR (its own host-redactor build): child stdout and the published nonzero-child draft must both
# carry the exact union redaction, with the child status unchanged.
a_compose_consumers() {
	_cd=$1
	mkdir -p "$_cd/docs/spikes/issue-drafts"
	rm -f "$_cd/docs/spikes/issue-drafts/s5-admission.md"
	(cd "$_cd" && BOXSET_TEST_SECRET=https "$_cd/scripts/dev-run.sh" --write docs/spikes/issue-drafts -- sh -c 'mkdir -p /src/docs/spikes/issue-drafts && printf "%s" "https://u:p@h/x?k=v"; printf "see https://u:p@h/x?k=v end" >/src/docs/spikes/issue-drafts/s5-admission.md; exit 1') >"$TMP/rout" 2>"$TMP/rerr"
	RS=$?
	rerr_seen
	_cc=$(cname)
	[ -z "$_cc" ] || case " $OWNED " in *" $_cc "*) ;; *) OWNED="$OWNED $_cc" ;; esac
	_co=$(cat "$TMP/rout")
	_cdr=$(cat "$_cd/docs/spikes/issue-drafts/s5-admission.md" 2>/dev/null)
	rm -f "$_cd/docs/spikes/issue-drafts/s5-admission.md"
	OBS="status $RS, output '$_co', draft '$_cdr'"
	[ "$RS" = 1 ] && [ -n "$_co" ] && [ -n "$_cdr" ] && reached
	[ "$RS" = 1 ] && [ "$_co" = "[REDACTED]://[REDACTED]@h/x?k=[REDACTED]" ] && [ "$_cdr" = "see [REDACTED]://[REDACTED]@h/x?k=[REDACTED] end" ]
}
# go_mutant TITLE FN FILE OLD NEW: FILE is mutated in an isolated checkout (the mutant must compile and
# reach the named test), the real mutated-file SHA is recorded, and the same assertion is then run on
# the original checkout (GREEN).
go_mutant() {
	_gt=$1
	_gf=$2
	_gfile=$3
	_gold=$4
	_gnew=$5
	shift 5
	fiso "mut-$_gf"
	_orig=$(fhash "$FISO/$_gfile")
	if ! mutate_file "$FISO/$_gfile" "$_gold" "$_gnew"; then rm -rf "$FISO"; return; fi
	while [ $# -ge 2 ]; do
		if ! mutate_file "$FISO/$_gfile" "$1" "$2"; then rm -rf "$FISO"; return; fi
		shift 2
	done
	_msha=$(fhash "$FISO/$_gfile")
	if [ "$_msha" = "$_orig" ]; then
		fail "$_gt" "the mutation left $_gfile byte-identical (sha $_orig)"
	else
		rg_reset
		_gc=0
		"$_gf" "$FISO" || _gc=$?
		if rg_verdict red "$_gc"; then pass "$_gt RED on mutated $_gfile sha $_msha (observed: $OBS)"; else fail "$_gt" "mutant ($_gfile sha $_msha): $RG_WHY $OBS"; fi
	fi
	rg_reset
	_gc=0
	"$_gf" "$ISO" || _gc=$?
	if rg_verdict green "$_gc"; then pass "$_gt GREEN on original $_gfile sha $_orig"; else fail "$_gt original" "$RG_WHY $OBS"; fi
	rm -rf "$FISO"
}
go_mutant "animated-check-removed validator mutant (compiling behavior mutant)" a_animated deploy/dev/capture-check/main.go 'if animated {' 'if animated && len(data) < 0 {'
go_mutant "normalize-before-strip validator mutant" a_normalize deploy/dev/capture-check/main.go 'if !stripMatches(img, t, challenge) {' 'if !stripMatches(truecolour(img), t, challenge) {'
go_mutant "replacement-text-feeds-URL-layer redactor mutant" a_compose deploy/dev/redact/main.go 'f.url.write(f.buf[f.start], len(f.ivs) > 0 && f.ivs[0].lo <= f.base)' 'if cov := len(f.ivs) > 0 && f.ivs[0].lo <= f.base; cov { for i := range len(marker) { f.url.write(marker[i], false) } } else { f.url.write(f.buf[f.start], false) }'
go_mutant "marker-per-byte redactor mutant (covered runs not merged)" a_runs deploy/dev/redact/main.go '	if cov {
		s.covered = true
		return
	}' '	if cov {
		s.putString(marker)
		return
	}'
go_mutant "replacement-text-feeds-URL-layer redactor mutant (actual runner output and draft consumers)" a_compose_consumers deploy/dev/redact/main.go 'f.url.write(f.buf[f.start], len(f.ivs) > 0 && f.ivs[0].lo <= f.base)' 'if cov := len(f.ivs) > 0 && f.ivs[0].lo <= f.base; cov { for i := range len(marker) { f.url.write(marker[i], false) } } else { f.url.write(f.buf[f.start], false) }'
go_mutant "replacement-rescan redactor mutant (compiling, bounded: the emitted marker is scanned once for the selected values)" a_runs deploy/dev/redact/main.go '	values, ok := selectValues(environ)' '	values, ok := selectValues(environ)
	rescanValues = values' '		s.putString(marker)' '		s.putString(rescanMarker(marker))' 'func (s *sink) putString(v string) {' 'var rescanValues [][]byte

func rescanMarker(v string) string {
	for _, val := range rescanValues {
		v = strings.ReplaceAll(v, string(val), marker)
	}
	return v
}

func (s *sink) putString(v string) {'
a_clock_reads() { a_go_test "$1" TestClockModeOutput ./deploy/dev/redact '^TestClockModeOutput$' 'status [0-9]+ output .*, want 0 and S\.CC plus LF'; }
a_clock_rounds() { a_go_test "$1" TestClockFormatsAndBounds ./deploy/dev/redact '^TestClockFormatsAndBounds$' 'formatClock\(5, 999999999\) = .*, want the floored 5\.99'; }
go_mutant "clock-reads-selection redactor mutant (the clock mode evaluates the secret selection)" a_clock_reads deploy/dev/redact/main.go '		return clockMode(stdout, clockGettime)' '		if _, ok := selectValues(environ); !ok {
			return exitUsage
		}
		return clockMode(stdout, clockGettime)'
go_mutant "clock-rounds redactor mutant (the reading is rounded to the nearest centisecond, not floored)" a_clock_rounds deploy/dev/redact/main.go 'return fmt.Sprintf("%d.%02d\n", sec, nsec/10000000), true' 'return fmt.Sprintf("%d.%02d\n", sec, (nsec+5000000)/10000000), true'

# ===== 12. admission mutation controls (T01), manifest-only roots in the real admitted image ============================
mkdir -p "$ISO/docs/spikes"
MUTS='set -u
R=/tmp/mut
S=/src/scripts/s5-toolchain.sh
SETUP_BAD=0
reset() { rm -rf "$R"; mkdir -p "$R/deploy/dev" "$R/docs/spikes" "$R/web"; cp /src/deploy/dev/Dockerfile "$R/deploy/dev/"; cp /src/docs/spikes/s5-version-admission.md "$R/docs/spikes/"; cp /src/web/package.json /src/web/bun.lock "$R/web/"; cp /src/go.mod "$R/"; REP=$R/docs/spikes/s5-version-admission.md; PKGJ=$R/web/package.json; DF=$R/deploy/dev/Dockerfile; SETUP_BAD=0; }
admit() { SCRIPT=${1:-$S}; out=$($SCRIPT --admission --root "$R" 2>&1); st=$?; }
# sedx FILE EXPR [N|+]: one sed edit that must change exactly N lines (default 1, + means at least one);
# a fixture edit that changed nothing is a setup failure, never a mutant outcome.
sedx() { cp "$1" "$R/before"; sed -i "$2" "$1"; _c=$(diff "$R/before" "$1" | grep -c "^<"); if [ "${3:-1}" = "+" ]; then [ "$_c" -ge 1 ] || SETUP_BAD=1; else [ "$_c" = "${3:-1}" ] || SETUP_BAD=1; fi; }
want() { # want NAME STATUS PATTERN
	if [ "$SETUP_BAD" = 1 ]; then echo "MUT $1 FAIL: the fixture edit did not make exactly the intended change"
	elif [ "$st" = "$2" ] && printf "%s\n" "$out" | grep -Eq "$3"; then echo "MUT $1 PASS"
	else echo "MUT $1 FAIL: status $st: $(printf "%s\n" "$out" | head -n 4 | tr "\n" "|")"; fi
}
wantpass() { want "$1" 0 "^S5-MANIFEST-ONLY-PASS blocked=0 unlisted=0"; }
wantnot() { # wantnot NAME STATUS PATTERN BADPATTERN: the intended outcome and no unintended block
	if ! printf "%s\n" "$out" | grep -Eq "$4"; then want "$1" "$2" "$3"; else echo "MUT $1 FAIL: unintended output: $(printf "%s\n" "$out" | head -n 4 | tr "\n" "|")"; fi
}
osids=$(grep -E "^\| ADM-[0-9]+ \| apt \| os-(library|font|data) \|" /src/docs/spikes/s5-version-admission.md | cut -d" " -f2)
want_all_os() { # want_all_os NAME PATTERN: every one of the 29 OS rows is blocked with PATTERN
	_n=0; _bad=0
	for id in $osids; do _n=$((_n + 1)); printf "%s\n" "$out" | grep -Eq "^ADMISSION-BLOCKED $id: $2" || _bad=1; done
	if [ "$SETUP_BAD" = 0 ] && [ "$st" = 1 ] && [ "$_bad" = 0 ] && [ "$_n" = 29 ]; then echo "MUT $1 PASS"; else echo "MUT $1 FAIL: status $st, os rows $_n (want 29), missing=$_bad, setup=$SETUP_BAD"; fi
}
cat >/tmp/licmut.js <<"JS"
const fs = require("fs");
const rep = process.env.S5_REP, id = process.env.S5_ID, key = process.env.S5_KEY, val = process.env.S5_VAL;
let hits = 0;
const out = fs.readFileSync(rep, "utf8").split("\n").map((l) => {
  if (!l.startsWith("| " + id + " |")) return l;
  const cells = l.split(" | ");
  const toks = cells[8].split(" ");
  const idx = [];
  toks.forEach((t, i) => { if (t.startsWith(key + "=")) idx.push(i); });
  if (idx.length !== 1) { console.error("tokens named " + key + ": " + idx.length); process.exit(3); }
  const before = toks.join(" ");
  if (val === "-") toks.splice(idx[0], 1); else toks[idx[0]] = key + "=" + val;
  if (toks.join(" ") === before) { console.error("no change"); process.exit(4); }
  cells[8] = toks.join(" ");
  hits++;
  return cells.join(" | ");
});
if (hits !== 1) { console.error("rows changed: " + hits); process.exit(5); }
fs.writeFileSync(rep, out.join("\n"));
JS
licmut() { # licmut ID KEY VALUE|-: structurally replace (or drop) the KEY= token of ID licence cell; exactly one token changes
	S5_REP=$REP S5_ID=$1 S5_KEY=$2 S5_VAL=$3 bun /tmp/licmut.js >/tmp/licmut.out 2>&1 || SETUP_BAD=1
}
today=$(date -u +%F)
dago() { date -u -d "$1 days ago" +%F; }
dahead() { date -u -d "$1 days" +%F; }
rebuild() { # rebuild CONTENTFILE [COMMIT] [RELEASE]: replace the support-list block with new fenced bytes and a matching header; every producer status, the checksum and the block read back are checked, else the fixture is a setup failure and never a scenario outcome
	f=$REP
	n=$(grep -n "^support-list " "$f" | cut -d: -f1)
	case $n in "" | *[!0-9]*) SETUP_BAD=1; return ;; esac
	tail -n +$((n + 2)) "$f" >"$R/t" || { SETUP_BAD=1; return; }
	m=$(grep -n -m 1 -x "\`\`\`" "$R/t" | cut -d: -f1)
	case $m in "" | *[!0-9]*) SETUP_BAD=1; return ;; esac
	m=$((n + 1 + m))
	commit=${2:-be1e8c21ab13b29cb831c7b5890d7420fe36cc56}
	rel=${3:-13}
	sha=$(sha256sum <"$1" | cut -d" " -f1)
	case $sha in "" | *[!0-9a-f]*) SETUP_BAD=1; return ;; esac
	[ "${#sha}" = 64 ] || { SETUP_BAD=1; return; }
	{
		head -n $((n - 1)) "$f" &&
		printf "support-list release=%s ref=master commit=%s sha256=%s url=https://salsa.debian.org/debian/debian-security-support/-/raw/%s/security-support.deb%s\n" "$rel" "$commit" "$sha" "$commit" "$rel" &&
		printf "\`\`\`security-support.deb%s\n" "$rel" &&
		cat "$1" &&
		printf "\`\`\`\n" &&
		tail -n +$((m + 1)) "$f"
	} >"$R/new" || { SETUP_BAD=1; return; }
	cp "$R/new" "$f" || { SETUP_BAD=1; return; }
	tail -n +$((n + 2)) "$f" >"$R/t2" || { SETUP_BAD=1; return; }
	awk "/^\`\`\`\$/ {exit} {print}" "$R/t2" >"$R/chk" || { SETUP_BAD=1; return; }
	cmp -s "$R/chk" "$1" || SETUP_BAD=1
}
blockbytes() { n=$(grep -n "^support-list " "$REP" | cut -d: -f1); case $n in "" | *[!0-9]*) return 1 ;; esac; tail -n +$((n + 2)) "$REP" >"$R/bb" && awk "/^\`\`\`\$/ {exit} {print}" "$R/bb"; }
reset; admit
wantpass original-structure
reset; grep -Eq "^\| ADM-012 .* debian-source gcc-defaults 1\.220 .* work=gcc-defaults/1\.233 " "$REP" || SETUP_BAD=1; admit
wantpass gcc-newer-same-source-work-admits
reset; sedx "$REP" "s/ecbadb99091a3f46e31f5f934b068b1864eafa7995211b39eaddf76996045fe5/ecbadb99091a3f46e31f5f934b068b1864eafa7995211b39eaddf76996045fff/"; admit
want tampered-checksum 1 "ADMISSION-BLOCKED ADM-003"
reset; sedx "$PKGJ" "s/\"svelte\": \"5.57.2\"/\"svelte\": \"^5.57.2\"/"; admit
want floating-caret 1 "ADMISSION-BLOCKED ADM-115"
reset; sedx "$PKGJ" "s/\"svelte\": \"5.57.2\"/\"svelte\": \"~5.57.2\"/"; admit
want floating-tilde 1 "ADMISSION-BLOCKED ADM-115"
reset; sedx "$PKGJ" "s/\"svelte\": \"5.57.2\"/\"svelte\": \"*\"/"; admit
want floating-star 1 "ADMISSION-BLOCKED ADM-115"
reset; sedx "$REP" "/^| ADM-022 /s/| 2.9.0-1~deb13u1 |/| >=2.9.0-1~deb13u1 |/"; admit
want apt-range-row 1 "ADMISSION-BLOCKED ADM-022: floating"
reset; sedx "$REP" "/^| ADM-022 /s/| 2.9.0-1~deb13u1 |/| * |/"; admit
want apt-wildcard-row 1 "ADMISSION-BLOCKED ADM-022: floating"
reset; sedx "$DF" "s/    libexpat1=2.9.0-1~deb13u1 /    libexpat1>=2.9.0-1~deb13u1 /"; admit
want apt-range-dockerfile 1 "ADMISSION-BLOCKED ADM-022"
reset; sedx "$DF" "s/    libexpat1=2.9.0-1~deb13u1 /    libexpat1=* /"; admit
want apt-wildcard-dockerfile 1 "ADMISSION-BLOCKED ADM-022"
reset; sedx "$DF" "s/    libexpat1=2.9.0-1~deb13u1 /    libexpat1=2.8.3 /"; admit
want literal-tilde-version-mismatch 1 "ADMISSION-BLOCKED ADM-022"
reset; d=$(dago 366); sedx "$REP" "/^| ADM-004 /s/ 2026-10-10 / $d /"; admit
want annual-366-days 1 "ADMISSION-BLOCKED ADM-004: maintenance_stale"
reset; d=$(dago 365); sedx "$REP" "/^| ADM-004 /s/ 2026-10-10 / $d /"; admit
wantpass annual-365-days-admits
reset; snap=$(sed -n "s/^ARG DEBIAN_SNAPSHOT=//p" "$DF"); [ -n "$snap" ] || SETUP_BAD=1; sedx "$REP" "/^| ADM-004 /s/ release c6da4a4d3010e5553438c60f6bd76d981976867c / release $snap /"; admit
want snapshot-id-as-maintenance 1 "ADMISSION-BLOCKED ADM-004: evidence_invalid"
reset; sedx "$REP" "/^| ADM-004 /s/ release c6da4a4d3010e5553438c60f6bd76d981976867c / release snapshot:20261005T000000Z /"; admit
want snapshot-timestamp-as-maintenance 1 "ADMISSION-BLOCKED ADM-004: evidence_invalid"
reset; sedx "$REP" "/^| ADM-004 /s#https://github.com/oven-sh/bun/releases/tag/bun-v1.4.3#http://snapshot.debian.org/archive/debian/20261005T000000Z/#"; admit
want snapshot-url-as-maintenance-bun 1 "ADMISSION-BLOCKED ADM-004: evidence_invalid"
reset; sedx "$REP" "/^| CMT-P07 /d"; admit
want missing-p07 1 "ADMISSION-BLOCKED CMT-P07"
reset; printf "\nFROM golang:1.27 AS extra\n" >>"$DF"; admit
want unlisted-base-image 1 "ADMISSION-UNLISTED image golang:1.27"
for sec in dependencies devDependencies optionalDependencies peerDependencies; do
	for fmt in 2 8 0; do
		reset
		S5_PKG=$PKGJ S5_FMT=$fmt S5_SEC=$sec bun -e "const fs=require(\"fs\");const p=JSON.parse(fs.readFileSync(process.env.S5_PKG,\"utf8\"));p[process.env.S5_SEC]=p[process.env.S5_SEC]||{};p[process.env.S5_SEC][\"left-pad\"]=\"1.3.0\";const f=Number(process.env.S5_FMT);fs.writeFileSync(process.env.S5_PKG,f?JSON.stringify(p,null,f):JSON.stringify(p));" || SETUP_BAD=1
		admit
		want "unlisted-$sec-indent-$fmt" 1 "ADMISSION-UNLISTED npm left-pad@1.3.0"
	done
done
reset; sedx "$REP" "/^| ADM-115 /s/| npm-dependency |/| os-library |/"; admit
want category-npm-as-os-library 1 "ADMISSION-BLOCKED ADM-115: category_invalid"
reset; sedx "$REP" "/^| ADM-003 /s/| direct-tool |/| os-library |/"; admit
want category-archive-as-os-library 1 "ADMISSION-BLOCKED ADM-003: category_invalid"
reset; sedx "$REP" "/^| ADM-041 /s/| direct-tool |/| os-data |/"; admit
want category-apt-fetch-as-os-data 1 "ADMISSION-BLOCKED ADM-041: category_invalid"
reset; sedx "$REP" "/^| ADM-012 /s/| direct-tool |/| os-library |/"; admit
want category-gcc-as-os-library 1 "ADMISSION-BLOCKED ADM-012: category_invalid"
reset; sedx "$REP" "/^| ADM-039 /s/| direct-tool |/| os-library |/"; admit
want category-qemu-as-os-library 1 "ADMISSION-BLOCKED ADM-039: category_invalid"
reset; sedx "$REP" "/^| ADM-001 /s/| base-image |/| os-data |/"; admit
want category-image-as-os-data 1 "ADMISSION-BLOCKED ADM-001: category_invalid"
reset; sedx "$REP" "/^| ADM-031 /s/| os-library |/| os-data |/"; admit
want category-os-data-name-outside-set 1 "ADMISSION-BLOCKED ADM-031: category_invalid"
reset; sedx "$REP" "/^| ADM-013 /s#| distro-support debian 13 trixie security-until=2028-08-09 https://www.debian.org/releases/trixie/ tracker=https://security-tracker.debian.org/tracker/source-package/alsa-lib lists=be1e8c21ab13b29cb831c7b5890d7420fe36cc56 status=clear |#| release 0123456789abcdef0123456789abcdef01234567 $(dago 30) https://example.test/r work=alsa-lib/1.2.14 |#"; admit
want os-row-with-release-evidence 1 "ADMISSION-BLOCKED ADM-013: evidence_invalid"
reset; sedx "$REP" "/^| ADM-012 /s#| upload 129064bfee8b0b638f68d8ce8957264501f04483 2026-08-02 [^ ]* work=gcc-defaults/1.233 |#| distro-support debian 13 trixie security-until=2028-08-09 https://www.debian.org/releases/trixie/ tracker=https://security-tracker.debian.org/tracker/source-package/gcc-defaults lists=be1e8c21ab13b29cb831c7b5890d7420fe36cc56 status=clear |#"; admit
want direct-tool-with-distro-support 1 "ADMISSION-BLOCKED ADM-012: evidence_invalid"
reset; sedx "$REP" "/^| ADM-012 /s#https://metadata.ftp-master.debian.org/changelogs/main/g/gcc-defaults/gcc-defaults_1.233_changelog#http://snapshot.debian.org/archive/debian/20261005T000000Z/#"; admit
want snapshot-url-as-maintenance 1 "ADMISSION-BLOCKED ADM-012: evidence_invalid"
reset; sedx "$REP" "/^| ADM-012 /s#work=gcc-defaults/1.233#work=gcc-14/14.4.0-2#"; admit
want gcc-work-subject-gcc14 1 "ADMISSION-BLOCKED ADM-012: evidence_invalid"
reset; d=$(dago 366); sedx "$REP" "/^| ADM-012 /s/ 2026-08-02 / $d /"; admit
want gcc-366-days 1 "ADMISSION-BLOCKED ADM-012: maintenance_stale"
reset; sedx "$REP" "/^| ADM-013 /s/ trixie security-until/ bookworm security-until/"; admit
want os-codename-bookworm 1 "ADMISSION-BLOCKED ADM-013: distro_unsupported"
reset; sedx "$REP" "/^| ADM-013 /s/security-until=2028-08-09/security-until=$(dago 1)/"; admit
want os-security-until-past 1 "ADMISSION-BLOCKED ADM-013: distro_unsupported"
reset; sedx "$DF" "s/Suites: trixie trixie-updates/Suites: trixie trixie-updates forky/"; admit
want dockerfile-suite-forky 1 "ADMISSION-BLOCKED ADM-013: distro_unsupported"
# support-list block controls (the original block evaluates every installed source clear)
blockbytes >/tmp/orig.block && [ -s /tmp/orig.block ] || { echo "MUTATIONS-SETUP the original support block could not be read"; exit 99; }
reset; { cat /tmp/orig.block; echo "libx11 limited trusted content only"; } >/tmp/b; rebuild /tmp/b; admit
want support-limited-libx11 1 "ADMISSION-BLOCKED ADM-031: distro_unsupported"
reset; { cat /tmp/orig.block; echo "libx11 non-supported 2:1.8.12-1 $today"; } >/tmp/b; rebuild /tmp/b; admit
want support-ended-today 1 "ADMISSION-BLOCKED ADM-031: distro_unsupported"
reset; t=$(dahead 1); { cat /tmp/orig.block; echo "libx11 non-supported 2:1.8.12-1 $t"; } >/tmp/b; rebuild /tmp/b; sedx "$REP" "/^| ADM-031 /s/status=clear/status=ends-$t/"; admit
wantpass support-ends-tomorrow-admits
reset; { cat /tmp/orig.block; echo "libx.* limited broad"; echo "libx11 supported override"; } >/tmp/b; rebuild /tmp/b; sedx "$REP" "/^| ADM-031 /s/status=clear/status=supported/"; admit
wantnot support-supported-override 1 "ADMISSION-BLOCKED ADM-032: distro_unsupported" "ADMISSION-BLOCKED ADM-031"
reset; { cat /tmp/orig.block; echo "libx1 limited prefix only"; } >/tmp/b; rebuild /tmp/b; admit
wantpass support-full-string-match
for tc in "unknown-status:libx11 maybe text" "nonsupported-without-version:libx11 non-supported" "bad-date:libx11 non-supported 2:1.8.12-1 2026-02-30" "bad-pattern:( limited text"; do
	name=${tc%%:*}; line=${tc#*:}
	reset; { cat /tmp/orig.block; echo "$line"; } >/tmp/b; rebuild /tmp/b; admit
	want_all_os "support-invalid-$name-all-os-rows" "evidence_invalid"
done
reset; n=$(grep -n "^support-list " "$REP" | cut -d: -f1); sedx "$REP" "$((n + 2))s/^#/# /"; admit
want_all_os support-flipped-fenced-byte-all-os-rows "evidence_invalid"
reset; sedx "$REP" "s#/-/raw/be1e8c21ab13b29cb831c7b5890d7420fe36cc56/#/-/raw/c14f35cca9e5cf73de8d00f86f562b836589af7e/#"; admit
want_all_os support-url-commit-differs-all-os-rows "evidence_invalid"
reset; rebuild /tmp/orig.block be1e8c21ab13b29cb831c7b5890d7420fe36cc56 12; admit
want_all_os support-release-12-all-os-rows "evidence_invalid"
reset; n=$(grep -n "^support-list " "$REP" | cut -d: -f1); sed -n "${n}p" "$REP" >>"$REP"; admit
want_all_os support-second-block-all-os-rows "evidence_invalid"
reset; sedx "$REP" "/^support-list /d"; admit
want_all_os support-missing-block-all-os-rows "evidence_invalid"
reset; sedx "$REP" "s#security-support.deb13#security-support-ended.deb13#g" +; admit
want_all_os support-obsolete-ended-file-all-os-rows "evidence_invalid"
reset; sedx "$REP" "/^| ADM-013 /s/status=clear/status=supported/"; admit
want row-status-differs-from-evaluation 1 "ADMISSION-BLOCKED ADM-013: evidence_invalid"
reset; sedx "$REP" "/^| ADM-013 /s#lists=be1e8c21ab13b29cb831c7b5890d7420fe36cc56#lists=c14f35cca9e5cf73de8d00f86f562b836589af7e#"; admit
want row-lists-commit-differs 1 "ADMISSION-BLOCKED ADM-013: evidence_invalid"
# source/licence provenance against the installed package
reset; sedx "$REP" "/^| ADM-013 /s#debian-source alsa-lib 1.2.14-1+deb13u1#debian-source alsa-lib 9.9.9#"; admit
want source-mismatch 1 "ADMISSION-BLOCKED ADM-013: source_mismatch"
# licence tokens are parsed from the existing cell and changed structurally; exactly one token differs
reset; $S --facts --root "$R" 2>/dev/null | grep "^FACT ADM-013 " >/tmp/fact; sha=$(sed -n "s/.*copyright-sha256=\([0-9a-f]*\).*/\1/p" /tmp/fact); dep=$(sed -n "s/.*dep5=\(.*\)$/\1/p" /tmp/fact)
flip=$(printf "%s" "$sha" | tr "0-9a-f" "1-9a-f0")
reset; licmut ADM-013 copyright-sha256 "$flip"; licmut ADM-013 copyright-sha256 "$sha"; admit
wantpass licence-installed-values-admit
reset; licmut ADM-013 copyright-sha256 "$flip"; admit
want licence-flipped-sha 1 "ADMISSION-BLOCKED ADM-013: licence_mismatch"
reset; licmut ADM-013 dep5 not-the-set; admit
want licence-flipped-dep5 1 "ADMISSION-BLOCKED ADM-013: licence_mismatch"
reset; licmut ADM-013 copyright-sha256 -; admit
want licence-missing-sha 1 "ADMISSION-BLOCKED ADM-013: licence_mismatch: licence cell lacks"
reset; licmut ADM-013 dep5 -; admit
want licence-missing-dep5 1 "ADMISSION-BLOCKED ADM-013: licence_mismatch: licence cell lacks"
# DEC-011 go rows: a pseudo-version is admitted for an indirect require only, with no ref, and only when its 12 hex are the prefix of the source commit
GOPSEUDO=v2.8.5-20261007231709-e8c451ca75d2
reset; sedx "$R/go.mod" "s#github.com/dlclark/regexp2/v2 v2.8.4#github.com/dlclark/regexp2/v2 $GOPSEUDO#"; sedx "$REP" "/^| ADM-200 /s#| github.com/dlclark/regexp2/v2 | v2.8.4 |#| github.com/dlclark/regexp2/v2 | $GOPSEUDO |#"; admit
want direct-pseudo-version-blocked 1 "ADMISSION-BLOCKED ADM-200: go version $GOPSEUDO is not an exact vMAJOR.MINOR.PATCH release"
reset; sedx "$R/go.mod" "s#github.com/dlclark/regexp2/v2 v2.8.4#github.com/dlclark/regexp2/v2 $GOPSEUDO // indirect#"; sedx "$REP" "/^| ADM-200 /s#| github.com/dlclark/regexp2/v2 | v2.8.4 |#| github.com/dlclark/regexp2/v2 | $GOPSEUDO |#"; sedx "$REP" "/^| ADM-200 /s# ref=refs/tags/v2.8.4 | Doug# | Doug#"; admit
wantpass indirect-pseudo-version-admits
reset; sedx "$R/go.mod" "s#github.com/dlclark/regexp2/v2 v2.8.4#github.com/dlclark/regexp2/v2 $GOPSEUDO // indirect#"; sedx "$REP" "/^| ADM-200 /s#| github.com/dlclark/regexp2/v2 | v2.8.4 |#| github.com/dlclark/regexp2/v2 | $GOPSEUDO |#"; sedx "$REP" "/^| ADM-200 /s# ref=refs/tags/v2.8.4 | Doug# | Doug#"; sedx "$REP" "/^| ADM-200 /s#commit=e8c451ca75d2#commit=f8c451ca75d2#"; admit
want indirect-pseudo-commit-mismatch 1 "ADMISSION-BLOCKED ADM-200: source_mismatch"
reset; sedx "$R/go.mod" "s#github.com/dlclark/regexp2/v2 v2.8.4#github.com/dlclark/regexp2/v2 $GOPSEUDO // indirect#"; sedx "$REP" "/^| ADM-200 /s#| github.com/dlclark/regexp2/v2 | v2.8.4 |#| github.com/dlclark/regexp2/v2 | $GOPSEUDO |#"; admit
want indirect-pseudo-with-ref-blocked 1 "ADMISSION-BLOCKED ADM-200: source_mismatch"
# a single-line require is read like a block entry: its class and its row rule do not change
reset; sedx "$R/go.mod" "s#\tgithub.com/dlclark/regexp2/v2 v2.8.4#)\n\nrequire github.com/dlclark/regexp2/v2 v2.8.4\n\nrequire (#"; admit
wantpass single-line-require-with-row-admits
reset; sedx "$R/go.mod" "s#\tgithub.com/dlclark/regexp2/v2 v2.8.4#)\n\nrequire github.com/dlclark/regexp2/v2 v2.8.4\n\nrequire example.invalid/unlisted v1.0.0\n\nrequire (#"; admit
want single-line-require-without-row-unlisted 1 "ADMISSION-UNLISTED go example.invalid/unlisted@v1\\.0\\.0"
reset; sedx "$R/go.mod" "s#\tgithub.com/dlclark/regexp2/v2 v2.8.4#)\n\nrequire github.com/dlclark/regexp2/v2 $GOPSEUDO // indirect\n\nrequire (#"; sedx "$REP" "/^| ADM-200 /s#| github.com/dlclark/regexp2/v2 | v2.8.4 |#| github.com/dlclark/regexp2/v2 | $GOPSEUDO |#"; sedx "$REP" "/^| ADM-200 /s# ref=refs/tags/v2.8.4 | Doug# | Doug#"; admit
wantpass single-line-indirect-pseudo-version-admits
reset; sedx "$R/go.mod" "s#\tgithub.com/dlclark/regexp2/v2 v2.8.4#)\n\nrequire github.com/dlclark/regexp2/v2 $GOPSEUDO\n\nrequire (#"; sedx "$REP" "/^| ADM-200 /s#| github.com/dlclark/regexp2/v2 | v2.8.4 |#| github.com/dlclark/regexp2/v2 | $GOPSEUDO |#"; admit
want single-line-direct-pseudo-version-blocked 1 "ADMISSION-BLOCKED ADM-200: go version $GOPSEUDO is not an exact vMAJOR.MINOR.PATCH release"
reset; sedx "$R/go.mod" "s#\tgithub.com/dlclark/regexp2/v2 v2.8.4#)\n\nrequire github.com/dlclark/regexp2/v2 v2.8.4\n\nrequire example.invalid/unlisted-indirect v1.0.0 // indirect\n\nrequire (#"; admit
want single-line-indirect-require-without-row-unlisted 1 "ADMISSION-UNLISTED go example.invalid/unlisted-indirect@v1\\.0\\.0"
echo MUTATIONS-DONE'
rr --timeout 600 -- sh -c "$MUTS"
# every scenario credit needs the script to have completed: its runner status 0 and its closing marker once; an aborted
# script is setup, never a per-scenario outcome
if [ "$RS" != 0 ] || [ "$(grep -c '^MUTATIONS-DONE$' "$TMP/rout")" != 1 ]; then SETUP=1; fi
expect "admission mutation script runner exits 0" "$RS" 0
expect "admission mutation script completed" "$(grep -c '^MUTATIONS-DONE$' "$TMP/rout")" 1
# The required scenario list is fixed here, independent of what the script printed.
REQ_MUTS='original-structure gcc-newer-same-source-work-admits tampered-checksum floating-caret floating-tilde floating-star apt-range-row apt-wildcard-row apt-range-dockerfile apt-wildcard-dockerfile literal-tilde-version-mismatch annual-366-days annual-365-days-admits snapshot-id-as-maintenance snapshot-timestamp-as-maintenance snapshot-url-as-maintenance-bun missing-p07 unlisted-base-image category-npm-as-os-library category-archive-as-os-library category-apt-fetch-as-os-data category-gcc-as-os-library category-qemu-as-os-library category-image-as-os-data category-os-data-name-outside-set os-row-with-release-evidence direct-tool-with-distro-support snapshot-url-as-maintenance gcc-work-subject-gcc14 gcc-366-days os-codename-bookworm os-security-until-past dockerfile-suite-forky support-limited-libx11 support-ended-today support-ends-tomorrow-admits support-supported-override support-full-string-match support-invalid-unknown-status-all-os-rows support-invalid-nonsupported-without-version-all-os-rows support-invalid-bad-date-all-os-rows support-invalid-bad-pattern-all-os-rows support-flipped-fenced-byte-all-os-rows support-url-commit-differs-all-os-rows support-release-12-all-os-rows support-second-block-all-os-rows support-missing-block-all-os-rows support-obsolete-ended-file-all-os-rows row-status-differs-from-evaluation row-lists-commit-differs source-mismatch licence-installed-values-admit licence-flipped-sha licence-flipped-dep5 licence-missing-sha licence-missing-dep5'
REQ_MUTS="$REQ_MUTS direct-pseudo-version-blocked indirect-pseudo-version-admits indirect-pseudo-commit-mismatch indirect-pseudo-with-ref-blocked single-line-require-with-row-admits single-line-require-without-row-unlisted single-line-indirect-pseudo-version-admits single-line-direct-pseudo-version-blocked single-line-indirect-require-without-row-unlisted"
for sec in dependencies devDependencies optionalDependencies peerDependencies; do
	for fmt in 2 8 0; do REQ_MUTS="$REQ_MUTS unlisted-$sec-indent-$fmt"; done
done
_nreq=0
for m in $REQ_MUTS; do
	_nreq=$((_nreq + 1))
	if [ "$(grep -c "^MUT $m " "$TMP/rout")" != 1 ]; then
		fail "admission: $m" "not reported exactly once: $(grep "^MUT $m " "$TMP/rout")"
	elif grep -q "^MUT $m PASS" "$TMP/rout"; then
		pass "admission: $m"
	else
		fail "admission: $m" "$(grep "^MUT $m " "$TMP/rout")"
	fi
done
expect "no admission scenario outside the required list was reported" "$(grep -c '^MUT ' "$TMP/rout")" "$_nreq"

# mutants of the admission script itself: the same named assertions RED then GREEN
mutate_file_copy() { # mutate_file_copy NAME OLD NEW...: SHA-bound copy of the admission script, removed if any replacement fails
	_mn=$1
	shift
	rm -f "$MUT/$_mn.sh"
	cp "$SRC/scripts/s5-toolchain.sh" "$MUT/$_mn.sh"
	chmod +x "$MUT/$_mn.sh"
	while [ $# -ge 2 ]; do
		if ! mutate_file "$MUT/$_mn.sh" "$1" "$2"; then
			rm -f "$MUT/$_mn.sh"
			return 1
		fi
		shift 2
	done
}
# s5_install SCRIPT DEST: the real admission script (or the mutant copy) is installed executable and read back
s5_install() {
	cp "$SRC/scripts/s5-toolchain.sh" "$2" || return 1
	[ "$1" = "$SRC/scripts/s5-toolchain.sh" ] || cp "$1" "$2" || return 1
	chmod +x "$2" || return 1
	cmp -s "$1" "$2"
}
# s5_fixture_ok: the in-image fixture was built, the unmodified copy passed under the original admission script (so no
# unrelated row or environment failure can stand for the target) and the intended edit changed the report; else setup
s5_fixture_ok() {
	if [ "$(grep -c '^FIXTURE-OK$' "$TMP/rout")" = 1 ] && ! grep -q '^FIXTURE-FAIL' "$TMP/rout"; then return 0; fi
	OBS="$OBS; fixture not established: $(grep -m1 '^FIXTURE-FAIL' "$TMP/rout")"
	SETUP=1
	return 1
}
# s5_only ALLOW_RE: every blocked or unlisted line of the admission output names only the intended rows
s5_only() {
	if grep -E '^ADMISSION-(BLOCKED|UNLISTED) ' "$TMP/rout" | grep -Ev "^ADMISSION-(BLOCKED|UNLISTED) ($1)([: ]|\$)" | grep -q .; then
		OBS="$OBS; blocked or unlisted rows outside the intended ones"
		SETUP=1
		return 1
	fi
	return 0
}
# S5FIX: the checked in-image fixture shared by the report-copy controls (copies checked; the unmodified copy must pass first)
S5FIX='R=/tmp/m; rm -rf $R && mkdir -p $R/deploy/dev $R/docs/spikes $R/web && cp /src/deploy/dev/Dockerfile $R/deploy/dev/ && cp /src/docs/spikes/s5-version-admission.md $R/docs/spikes/ && cp /src/web/package.json /src/web/bun.lock $R/web/ && cp /src/go.mod $R/ || { echo FIXTURE-FAIL copy; exit 97; }
REP=$R/docs/spikes/s5-version-admission.md
/src/scripts/s5-toolchain.sh --admission --root $R >$R/baseline.out 2>&1; grep -Eq "^S5-MANIFEST-ONLY-PASS blocked=0 unlisted=0" $R/baseline.out || { echo FIXTURE-FAIL baseline; exit 98; }'
# a_s5_scanner SCRIPT: an unlisted dependency in each of the four package.json sections, in compact
# JSON (no indentation to scan), is reported by the real reader; every section must be found.
a_s5_scanner() {
	s5_install "$1" "$ISO/scripts/s5-mut.sh" || { OBS="the admission script could not be installed"; SETUP=1; return 1; }
	rrx "$ISO" "$RUNNER" --timeout 300 -- sh -c 'for sec in dependencies devDependencies optionalDependencies peerDependencies; do R=/tmp/m; rm -rf $R && mkdir -p $R/deploy/dev $R/docs/spikes $R/web && cp /src/deploy/dev/Dockerfile $R/deploy/dev/ && cp /src/docs/spikes/s5-version-admission.md $R/docs/spikes/ && cp /src/web/package.json /src/web/bun.lock $R/web/ && cp /src/go.mod $R/ || { echo "SETUP-FAIL $sec"; continue; }; if ! S5_PKG=$R/web/package.json S5_SEC=$sec bun -e "const fs=require(\"fs\");const p=JSON.parse(fs.readFileSync(process.env.S5_PKG,\"utf8\"));p[process.env.S5_SEC]=p[process.env.S5_SEC]||{};p[process.env.S5_SEC][\"left-pad\"]=\"1.3.0\";fs.writeFileSync(process.env.S5_PKG,JSON.stringify(p));" || ! grep -q left-pad $R/web/package.json; then echo "SETUP-FAIL $sec"; continue; fi; out=$(/src/scripts/s5-mut.sh --admission --root $R 2>&1); st=$?; printf "%s\n" "$out" | grep "^ADMISSION-BLOCKED " | head -n 3 | sed "s/^/DETAIL $sec /"; echo "SECTION $sec STATUS $st COUNT $(printf "%s\n" "$out" | grep -c "ADMISSION-UNLISTED npm left-pad@1.3.0") OTHER $(printf "%s\n" "$out" | grep -E "^ADMISSION-(BLOCKED|UNLISTED) " | grep -vc "ADMISSION-UNLISTED npm left-pad@1.3.0") DONE $(printf "%s\n" "$out" | grep -Ec "^S5-(ADMISSION-(PASS|FAIL)|MANIFEST-ONLY-PASS) ")"; done; echo SCANNER-DONE'
	rm -f "$ISO/scripts/s5-mut.sh"
	# the scanner's runner status and EACH section's own reading are prerequisites: a section must have completed (the admission script's own summary
	# line), ended with its accepting 0 or rejecting 1, and carry no blocked row and no unlisted row but the planted dependency. A blocked row
	# (the Bun enumerator, an input or a support failure) prints a normal FAIL summary and status 1 too, so it is a setup failure that is never a
	# section's outcome; the real acceptance or rejection of the planted dependency stays the assertion input
	_pre=0
	{ [ "$RS" = 0 ] && [ "$(grep -c '^SECTION ' "$TMP/rout")" = 4 ] && ! grep -q '^SETUP-FAIL' "$TMP/rout" && [ "$(grep -c '^SCANNER-DONE$' "$TMP/rout")" = 1 ]; } && _pre=1
	for sec in dependencies devDependencies optionalDependencies peerDependencies; do
		[ "$(grep -Ec "^SECTION $sec STATUS [01] COUNT [0-9]+ OTHER 0 DONE 1\$" "$TMP/rout")" = 1 ] || _pre=0
	done
	[ "$_pre" = 1 ] || { OBS="scanner sections did not all complete cleanly: $(grep '^SECTION \|^SETUP-FAIL\|^DETAIL ' "$TMP/rout" | tr '\n' '|')"; SETUP=1; return 1; }
	reached
	_ok=1
	for sec in dependencies devDependencies optionalDependencies peerDependencies; do
		grep -qx "SECTION $sec STATUS 1 COUNT 1 OTHER 0 DONE 1" "$TMP/rout" || _ok=0
	done
	[ "$_ok" = 1 ]
}
# B07: the mutant replaces only the input of the unlisted-input loop (`done <"$T/deps"`) with a four-space line-regex derivation of web/package.json; enum_deps, $T/deps
# and the known-pin check stay byte-identical. Original, every section: STATUS 1 COUNT 1 OTHER 0 DONE 1; mutant: STATUS 0 COUNT 0 OTHER 0 DONE 1 (the prerequisite is met, the assertion refused).
mutate_file_copy s5-line-scanner 'done <"$T/deps"' 'done <<LNSCAN
$(grep -E "^    \"[^\"]+\": \"[^\"]+\",?\$" "$PKG" | sed "s/^    \"\\([^\"]*\\)\": \"\\([^\"]*\\)\",\\{0,1\\}\$/\\1\\t\\2/")
LNSCAN'
redgreen "line-scanner dependency mutant (compact JSON, all four sections)" a_s5_scanner "$MUT/s5-line-scanner.sh" "$SRC/scripts/s5-toolchain.sh"
mutate_file_copy s5-section-dropped '["dependencies","devDependencies","optionalDependencies","peerDependencies"]' '["devDependencies"]'
redgreen "section-dropping dependency mutant (only devDependencies enumerated)" a_s5_scanner "$MUT/s5-section-dropped.sh" "$SRC/scripts/s5-toolchain.sh"

# failing admission drafts: the child creates /src/docs/spikes/issue-drafts/s5-admission.md itself and the runner publishes it. Early missing-input drafts are
# fast; the final-admission drafts below run the real /src script after a real manifest check.
fiso fault-admission
rm -f "$FISO/docs/spikes/s5-version-admission.md"
a_admission_draft() { # holds when a failing admission leaves exactly its redacted draft with status 1
	s5_install "$1" "$FISO/scripts/s5-toolchain.sh" || { OBS="the admission script could not be installed"; SETUP=1; return 1; }
	find "$FISO/docs/spikes/issue-drafts" -mindepth 1 -delete
	rrx "$FISO" "$FISO/scripts/dev-run.sh" --write docs/spikes/issue-drafts -- scripts/s5-toolchain.sh --admission
	_d=$FISO/docs/spikes/issue-drafts/s5-admission.md
	OBS="status $RS, draft $([ -s "$_d" ] && echo published || echo absent)"
	# the target: the failing admission reached its draft step (the blocked input line and the FAIL summary)
	grep -Fq 'ADMISSION-BLOCKED inputs' "$TMP/rout" && s5_done && reached
	[ "$RS" = 1 ] && [ -s "$_d" ] && [ "$(wc -c <"$_d")" -le 65536 ] && grep -Fq 'ADMISSION-BLOCKED inputs' "$_d" && ! grep -Fq 'synthetic-not-a-credential' "$_d"
}
mutate_file_copy s5-draft-dropped 'if [ -n "$DRAFT" ]; then' 'if false; then'
cp "$SRC/scripts/dev-run.sh" "$FISO/scripts/dev-run.sh"
redgreen "draft-dropping admission mutant (early missing input)" a_admission_draft "$MUT/s5-draft-dropped.sh" "$SRC/scripts/s5-toolchain.sh"
rm -rf "$FISO"

# Final failed admission through /src: a real checksum defect in the report copy blocks ADM-003 in the
# real script, which publishes its redacted draft through the fixed map and keeps exit status 1.
fiso final-admission
SHAFLIP=$(sed -n 's/^| ADM-003 | archive | direct-tool | go | 1.27.2 | \([0-9a-f]*\) |.*/\1/p' "$FISO/docs/spikes/s5-version-admission.md")
FINAL_FIXTURE=0
if [ "${#SHAFLIP}" = 64 ]; then
	mutate_file "$FISO/docs/spikes/s5-version-admission.md" "$SHAFLIP" "$(printf '%s' "$SHAFLIP" | tr '0-9a-f' '1-9a-f0')" && cp "$SRC/scripts/dev-run.sh" "$FISO/scripts/dev-run.sh" && FINAL_FIXTURE=1
else
	fail "final admission setup" "ADM-003 pin not found in the report copy"
fi
a_final_failure_draft() { # a_final_failure_draft SCRIPT [SECRET]: exact blocked row, reason, bounded redacted draft, status 1
	[ "$FINAL_FIXTURE" = 1 ] || { OBS="the report copy's ADM-003 checksum defect was not constructed"; SETUP=1; return 1; }
	s5_install "$1" "$FISO/scripts/s5-toolchain.sh" || { OBS="the admission script could not be installed"; SETUP=1; return 1; }
	find "$FISO/docs/spikes/issue-drafts" -mindepth 1 -delete
	(cd "$FISO" && BOXSET_SECRET_SENTINEL=${2:-synthetic-not-a-credential} scripts/dev-run.sh --write docs/spikes/issue-drafts -- scripts/s5-toolchain.sh --admission) >"$TMP/rout" 2>"$TMP/rerr"
	RS=$?
	rerr_seen
	_d=$FISO/docs/spikes/issue-drafts/s5-admission.md
	OBS="status $RS, draft $([ -s "$_d" ] && echo "published $(wc -c <"$_d") bytes" || echo absent)"
	# the target: exactly the one intended blocked row (its ID is redacted when selected) and its reason reached the summary
	grep -Fq 'S5-ADMISSION-FAIL blocked=1 unlisted=0' "$TMP/rout" && grep -Eq '^ADMISSION-BLOCKED (ADM-003|\[REDACTED\]): Dockerfile GO_SHA256 differs from' "$TMP/rout" && reached
	[ "$RS" = 1 ] && [ -s "$_d" ] && [ "$(wc -c <"$_d")" -le 65536 ] || return 1
	grep -Fq 'S5-ADMISSION-FAIL blocked=1 unlisted=0' "$TMP/rout" || return 1
	if [ -n "${2:-}" ]; then
		grep -Fq 'ADMISSION-BLOCKED [REDACTED]: Dockerfile GO_SHA256 differs from' "$_d" && ! grep -Fq 'ADM-003' "$_d" "$TMP/rout"
	else
		grep -Fq 'ADMISSION-BLOCKED ADM-003: Dockerfile GO_SHA256 differs from' "$_d" && ! grep -Fq 'synthetic-not-a-credential' "$_d"
	fi
}
redgreen "draft-dropping mutant on a final blocked admission (ADM-003 row and reason)" a_final_failure_draft "$MUT/s5-draft-dropped.sh" "$SRC/scripts/s5-toolchain.sh"
a_final_failure_draft_secret() { a_final_failure_draft "$1" ADM-003; }
redgreen "draft-dropping mutant on a final blocked admission with the seven-byte row ID selected" a_final_failure_draft_secret "$MUT/s5-draft-dropped.sh" "$SRC/scripts/s5-toolchain.sh"
rm -rf "$FISO"

# category / support / linter admission-script mutants against the same named assertions
a_s5_report() { # a_s5_report SCRIPT SEDEXPR WANT_PATTERN: the report-copy control reports WANT_PATTERN with status 1
	s5_install "$1" "$ISO/scripts/s5-mut.sh" || { OBS="the admission script could not be installed"; SETUP=1; return 1; }
	rrx "$ISO" "$RUNNER" --timeout 300 -- sh -c "$S5FIX
sed -i '$2' \"\$REP\"
if cmp -s \"\$R/docs/spikes/s5-version-admission.md\" /src/docs/spikes/s5-version-admission.md; then echo FIXTURE-FAIL edit; exit 97; fi
echo FIXTURE-OK
/src/scripts/s5-mut.sh --admission --root \$R 2>&1"
	rm -f "$ISO/scripts/s5-mut.sh"
	OBS="status $RS, admission output: $(head -n 2 "$TMP/rout" | tr '\n' '|')"
	s5_fixture_ok || return 1
	s5_done || { SETUP=1; return 1; }
	s5_only "$4" || return 1
	reached
	[ "$RS" = 1 ] && grep -Eq "$3" "$TMP/rout"
}
a_cat_removed() { a_s5_report "$1" '/^| ADM-115 /s/| npm-dependency |/| os-library |/' 'ADMISSION-BLOCKED ADM-115: category_invalid' 'ADM-115|npm svelte@5\.57\.2'; }
mutate_file_copy s5-category-removed 'if [ "$cat_ok" = 0 ]; then' 'if false; then'
redgreen "category-check-removed mutant admits the relabelled npm row" a_cat_removed "$MUT/s5-category-removed.sh" "$SRC/scripts/s5-toolchain.sh"
mutate_file_copy s5-supported-ignored 'supported) _sup=1 ;;' 'supported) : ;;'
mutate_file_copy s5-prefix-match 'pcre_match() { LC_ALL=C grep -P -x -q -e "$1" 2>/dev/null; }' 'pcre_match() { LC_ALL=C grep -P -q -e "$1" 2>/dev/null; }'
# These controls need a modified support block, so they reuse the in-image rebuild helper:
SUPCTL="$S5FIX"'
sf() { echo "FIXTURE-FAIL $1"; exit 97; }
HDR="support-list release=13 ref=master commit=be1e8c21ab13b29cb831c7b5890d7420fe36cc56 sha256=%s url=https://salsa.debian.org/debian/debian-security-support/-/raw/be1e8c21ab13b29cb831c7b5890d7420fe36cc56/security-support.deb13"
[ "$(grep -c "^support-list " "$REP")" = 1 ] || sf support-header
n=$(grep -n "^support-list " "$REP" | cut -d: -f1)
case $n in "" | *[!0-9]*) sf support-line ;; esac
tail -n +$((n + 2)) "$REP" >/tmp/t || sf support-read
awk "/^\`\`\`\$/ {exit} {print}" /tmp/t >/tmp/o || sf support-block
m=$(grep -n -m 1 -x "\`\`\`" /tmp/t | cut -d: -f1)
case $m in "" | *[!0-9]*) sf support-fence ;; esac
m=$((n + 1 + m))
{ cat /tmp/o && printf "%s\n" "$ADD"; } >/tmp/b || sf support-compose
sha=$(sha256sum </tmp/b | cut -d" " -f1)
case $sha in "" | *[!0-9a-f]*) sf support-checksum ;; esac
[ "${#sha}" = 64 ] || sf support-checksum
{ head -n $((n - 1)) "$REP" && printf "$HDR\n" "$sha" && printf "\`\`\`security-support.deb13\n" && cat /tmp/b && printf "\`\`\`\n" && tail -n +$((m + 1)) "$REP"; } >$R/new || sf support-rebuild
cp $R/new "$REP" || sf support-install
tail -n +$((n + 2)) "$REP" >/tmp/t2 || sf support-read
awk "/^\`\`\`\$/ {exit} {print}" /tmp/t2 >/tmp/chk || sf support-readback
cmp -s /tmp/chk /tmp/b || sf support
[ "$(sha256sum </tmp/chk | cut -d" " -f1)" = "$sha" ] || sf support-sha
[ "$(sed -n "${n}p" "$REP")" = "$(printf "$HDR" "$sha")" ] || sf support-header
cp "$REP" $R/before || sf edit
sed -i "$SEDX" "$REP" || sf edit
if [ "$SEDX" != "s/^\$//" ] && cmp -s $R/before "$REP"; then sf edit; fi
echo FIXTURE-OK
/src/scripts/s5-mut.sh --admission --root $R 2>&1'
# a_support_ctl SCRIPT ADDLINE SEDEXPR WANT_STATUS PATTERN [BADPATTERN [ALLOW_RE [LINE_RE]]]: the intended outcome
# (status and decisive output) must occur and the unintended block must not. The fixture is built with every producer
# status, the rebuilt block's checksum, header and bytes and the edit read back (FIXTURE-OK); ALLOW_RE restricts the blocked
# rows by ID and LINE_RE, when given, restricts every blocked or unlisted line to the reason the target outcome names, so
# an unrelated completed failure is setup, never the mutant's RED.
a_support_ctl() {
	s5_install "$1" "$ISO/scripts/s5-mut.sh" || { OBS="the admission script could not be installed"; SETUP=1; return 1; }
	rrx "$ISO" "$RUNNER" --timeout 300 -- sh -c "ADD='$2' SEDX='$3'; $SUPCTL"
	rm -f "$ISO/scripts/s5-mut.sh"
	OBS="status $RS, admission output: $(head -n 3 "$TMP/rout" | tr '\n' '|')"
	s5_fixture_ok || return 1
	s5_done || { SETUP=1; return 1; }
	[ -z "${7:-}" ] || s5_only "$7" || return 1
	[ -z "${8:-}" ] || s5_lines "$8" || return 1
	reached
	[ "$RS" = "$4" ] && grep -Eq "$5" "$TMP/rout" && { [ -z "${6:-}" ] || ! grep -Eq "$6" "$TMP/rout"; }
}
a_supported_wins() { a_support_ctl "$1" 'libx.* limited broad
libx11 supported override' '/^| ADM-031 /s/status=clear/status=supported/' 1 'ADMISSION-BLOCKED ADM-032: distro_unsupported' 'ADMISSION-BLOCKED ADM-031' 'ADM-03[1-8]'; }
a_full_string() { a_support_ctl "$1" 'libx1 limited prefix only' 's/^$//' 0 '^S5-MANIFEST-ONLY-PASS blocked=0 unlisted=0' '' '' 'ADMISSION-BLOCKED ADM-031: distro_unsupported'; }
redgreen "supported-override-ignored mutant" a_supported_wins "$MUT/s5-supported-ignored.sh" "$SRC/scripts/s5-toolchain.sh"
redgreen "prefix-match mutant" a_full_string "$MUT/s5-prefix-match.sh" "$SRC/scripts/s5-toolchain.sh"
# A matcher evaluation error (grep status above 1) is never a nonmatch: through the real script every
# OS row is evidence_invalid and none becomes clear. The fault copy changes only the matcher seam.
a_eval_error() {
	a_support_ctl "$1" '' 's/^$//' 1 'ADMISSION-BLOCKED ADM-013: evidence_invalid: support-list pattern evaluation failed' '' '' 'ADMISSION-BLOCKED ADM-[0-9]+: evidence_invalid: support-list pattern evaluation failed'
	_r=$?
	_n=$(grep -c '^ADMISSION-BLOCKED ADM-[0-9]*: evidence_invalid: support-list pattern evaluation failed' "$TMP/rout")
	OBS="$OBS; $_n OS rows evidence_invalid (want 29)"
	[ "$_r" = 0 ] && [ "$_n" = 29 ]
}
mutate_file_copy s5-eval-error 'pcre_match() { LC_ALL=C grep -P -x -q -e "$1" 2>/dev/null; }' 'pcre_match() { cat >/dev/null; return 2; }'
mutate_file_copy s5-eval-nonmatch 'pcre_match() { LC_ALL=C grep -P -x -q -e "$1" 2>/dev/null; }' 'pcre_match() { cat >/dev/null; return 2; }' '[ "$_rc" -le 1 ] || { SSTAT=evaluation_error; return; }' ':'
redgreen "evaluation-error-as-nonmatch mutant" a_eval_error "$MUT/s5-eval-nonmatch.sh" "$MUT/s5-eval-error.sh"

# The complete real admission through the actual runner: the lint pair and the other controls pass,
# the admission prints its PASS, and a successful admission publishes no draft.
RA_TARGET=lint
a_real_admission() { # a_real_admission SCRIPT (RA_TARGET lint|pass): holds on status 0, the PASS line, the planted-defect lint control and no published draft
	s5_install "$1" "$ISO/scripts/s5-mut.sh" || { OBS="the admission script could not be installed"; SETUP=1; return 1; }
	mkdir -p "$ISO/docs/spikes/issue-drafts"
	rm -f "$ISO/docs/spikes/issue-drafts/s5-admission.md"
	rrx "$ISO" "$RUNNER" --write docs/spikes/issue-drafts --timeout 600 -- scripts/s5-mut.sh --admission
	rm -f "$ISO/scripts/s5-mut.sh"
	OBS="status $RS: $(grep -m1 'golangci-lint' "$TMP/rout"), draft $([ -e "$ISO/docs/spikes/issue-drafts/s5-admission.md" ] && echo published || echo absent)"
	# the target of each mutant is its own seam: the linter control (ra_target: the control with the PASS summary, or the planted-defect block as the only
	# blocked row) or the success summary where the success-only draft write sits; an admission that failed elsewhere or carries an unrelated block reaches neither
	case $RA_TARGET in
	lint) if s5_done; then ra_target >/dev/null; fi ;;
	pass) grep -Eq '^S5-ADMISSION-PASS blocked=0 unlisted=0 rows=[0-9]+$' "$TMP/rout" && reached ;;
	esac
	_ok=0
	[ "$RS" = 0 ] && grep -Fq 'CONTROL golangci-lint 2.14.0' "$TMP/rout" && ! grep -Fq 'ADMISSION-BLOCKED' "$TMP/rout" &&
		grep -Eq '^S5-ADMISSION-PASS blocked=0 unlisted=0 rows=[0-9]+$' "$TMP/rout" && [ ! -e "$ISO/docs/spikes/issue-drafts/s5-admission.md" ] && _ok=1
	rm -rf "$ISO/docs/spikes/issue-drafts"
	[ "$_ok" = 1 ]
}
mutate_file_copy s5-no-violation-lint 'fmt.Printf("%%d", "x")' 'fmt.Println("x")'
RA_TARGET=lint
redgreen "no-violation linter mutant" a_real_admission "$MUT/s5-no-violation-lint.sh" "$SRC/scripts/s5-toolchain.sh"
# refuses_both NAME FN MUTANT ORIGINAL: the target is unreachable (an unrelated block is setup through the same ra_target), so the shared classifier gives neither the
# RED credit on the mutant nor the GREEN credit on the original; this meta-control is the only credit and it needs both refusals and both setups
refuses_both() {
	_rbn=$1
	_rbf=$2
	rg_reset
	_rbc=0
	"$_rbf" "$3" || _rbc=$?
	_rbr=1
	rg_verdict red "$_rbc" && _rbr=0
	_rbs=$SETUP
	rg_reset
	_rbc=0
	"$_rbf" "$4" || _rbc=$?
	_rbg=1
	rg_verdict green "$_rbc" && _rbg=0
	_rbt=$SETUP
	META=1
	if [ "$_rbr" = 1 ] && [ "$_rbg" = 1 ] && [ "$_rbs" = 1 ] && [ "$_rbt" = 1 ]; then pass "$_rbn"; else fail "$_rbn" "RED refused=$_rbr GREEN refused=$_rbg setup $_rbs/$_rbt: $OBS"; fi
	META=0
}
mutate_file_copy s5-lint-unrelated-orig '# Browser: only Playwright'"'"'s chromium-headless-shell, resolvable and launching.' 'block ADM-099 "category_invalid"
# Browser: only Playwright'"'"'s chromium-headless-shell, resolvable and launching.'
mutate_file_copy s5-lint-unrelated-novio 'fmt.Printf("%%d", "x")' 'fmt.Println("x")' '# Browser: only Playwright'"'"'s chromium-headless-shell, resolvable and launching.' 'block ADM-099 "category_invalid"
# Browser: only Playwright'"'"'s chromium-headless-shell, resolvable and launching.'
RA_TARGET=lint
refuses_both "linter target beside an unrelated blocked row (actual runner and provider): the no-violation mutant earns no RED and the original no GREEN, both are setup" a_real_admission "$MUT/s5-lint-unrelated-novio.sh" "$MUT/s5-lint-unrelated-orig.sh"
mutate_file_copy s5-draft-on-success "printf 'S5-ADMISSION-PASS blocked=0 unlisted=0 rows=%s\\n' \"\$ROWS\"" "printf 'S5-ADMISSION-PASS blocked=0 unlisted=0 rows=%s\\n' \"\$ROWS\"; mkdir -p /src/docs/spikes/issue-drafts && printf stale >/src/docs/spikes/issue-drafts/s5-admission.md"
RA_TARGET=pass
redgreen "draft-written-on-success mutant" a_real_admission "$MUT/s5-draft-on-success.sh" "$SRC/scripts/s5-toolchain.sh"

# DEC-012 go rules at the real root of an isolated checkout, through that checkout's own runner: every module the checkout compiles needs a go row,
# and go.sum must be what `go mod tidy` writes. Each check is RED on a script mutant without it and GREEN on the original.
# s5_root_run SCRIPT DIR: SCRIPT (or its mutant) runs the real admission at the real root of DIR; the admission must reach its summary line
s5_root_run() {
	s5_install "$1" "$2/scripts/s5-mut.sh" || { OBS="the admission script could not be installed"; SETUP=1; return 1; }
	rrx "$2" "$2/scripts/dev-run.sh" --timeout 600 -- scripts/s5-mut.sh --admission
	rm -f "$2/scripts/s5-mut.sh"
	OBS="status $RS, admission output: $(grep -E '^(ADMISSION-|S5-ADMISSION)' "$TMP/rout" | head -n 4 | tr '\n' '|')"
	s5_done || { SETUP=1; return 1; }
}
GOSUM_FORGED_H1=h1:$(printf '%043d=' 0 | tr 0 A)
fiso s5-compiled
GC_FIXTURE=0
if [ "$(grep -c '^| ADM-200 | go | go-dependency |' "$ISO/docs/spikes/s5-version-admission.md")" = 1 ] &&
	grep -v '^| ADM-200 |' "$ISO/docs/spikes/s5-version-admission.md" >"$FISO/docs/spikes/s5-version-admission.md" &&
	! cmp -s "$ISO/docs/spikes/s5-version-admission.md" "$FISO/docs/spikes/s5-version-admission.md" &&
	cp "$SRC/scripts/dev-run.sh" "$FISO/scripts/dev-run.sh"; then
	GC_FIXTURE=1
else
	fail "compiled module admission setup" "the ADM-200 go row could not be removed from the report copy"
fi
a_s5_compiled() { # holds when the module the checkout compiles, left without a row, is reported ADMISSION-UNLISTED go-compiled
	[ "$GC_FIXTURE" = 1 ] || { OBS="the report copy without its compiled module row was not constructed"; SETUP=1; return 1; }
	s5_root_run "$1" "$FISO" || return 1
	s5_only 'go|go-compiled' || return 1
	grep -Fxq 'ADMISSION-UNLISTED go github.com/dlclark/regexp2/v2@v2.8.4' "$TMP/rout" && reached
	[ "$RS" = 1 ] && grep -Fxq 'ADMISSION-UNLISTED go-compiled github.com/dlclark/regexp2/v2@v2.8.4' "$TMP/rout"
}
mutate_file_copy s5-compiled-check-removed 'grep -Fq "| go | go-dependency | $cmod | $cver |" "$T/rows" || unlisted go-compiled "$cmod@$cver"' ':'
redgreen "compiled-module-check-removed mutant (a compiled module without a go row)" a_s5_compiled "$MUT/s5-compiled-check-removed.sh" "$SRC/scripts/s5-toolchain.sh"
rm -rf "$FISO"
fiso s5-tidy
TD_FIXTURE=0
if printf 'example.invalid/forged v1.0.0 %s\nexample.invalid/forged v1.0.0/go.mod %s\n' "$GOSUM_FORGED_H1" "$GOSUM_FORGED_H1" >>"$FISO/go.sum" &&
	! cmp -s "$ISO/go.sum" "$FISO/go.sum" && cp "$SRC/scripts/dev-run.sh" "$FISO/scripts/dev-run.sh"; then
	TD_FIXTURE=1
else
	fail "tidy-dirty go.sum setup" "the forged go.sum lines could not be appended to the checkout copy"
fi
a_s5_tidy() { # holds when a go.sum that go mod tidy would rewrite is reported by the tidy check itself
	[ "$TD_FIXTURE" = 1 ] || { OBS="the checkout copy with a tidy-dirty go.sum was not constructed"; SETUP=1; return 1; }
	s5_root_run "$1" "$FISO" || return 1
	s5_only 'go\.mod|go\.sum' || return 1
	grep -Fq 'ADMISSION-BLOCKED go.sum: go.sum differs from its regeneration' "$TMP/rout" && reached
	[ "$RS" = 1 ] && grep -Fq 'ADMISSION-BLOCKED go.mod: go mod tidy -diff is not clean' "$TMP/rout"
}
mutate_file_copy s5-tidy-check-removed 'if [ "$tdst" -ne 0 ] || [ -n "$tdout" ]; then' 'if false; then'
redgreen "tidy-check-removed mutant (a go.sum that go mod tidy would rewrite)" a_s5_tidy "$MUT/s5-tidy-check-removed.sh" "$SRC/scripts/s5-toolchain.sh"
rm -rf "$FISO"

# The root-owned H2 currency procedure (pre-run check, actual admission, post-run check,
# evidence acceptance and changed-post invalidation) is root evidence in the feature
# workspace; no copied decision helper in this driver stands in for it.

# ===== 13. ecosystem-scoped lock export and the next-consumer refresh (T01/T27) =================================
if [ ! -f "$ISO/web/bun.lock" ]; then
	fail "lock export" "web/bun.lock is absent: run the documented web lock bootstrap first (docs/spikes/s5-version-admission.md)"
else
	locksum=$(sha256sum <"$ISO/web/bun.lock")
	# DEC-012: the Go lock export of the unmodified checkout reproduces the committed go.mod and go.sum byte for byte
	rm -rf "$TMP/exp-orig"
	mkdir -p "$TMP/exp-orig"
	ctl_begin
	(cd "$ISO" && docker build -f deploy/dev/Dockerfile --target lock-export --build-arg LOCK_ECOSYSTEM=go --output "type=local,dest=$TMP/exp-orig" .) >"$TMP/lock-orig.log" 2>&1
	_lx=$?
	[ "$_lx" = 0 ] || SETUP=1
	expect "Go lock export of the unmodified checkout ran" "$_lx" 0
	expect "Go lock export reproduces the committed go.mod" "$(sha256sum <"$TMP/exp-orig/go.mod")" "$(sha256sum <"$ISO/go.mod")"
	expect "Go lock export reproduces the committed go.sum" "$(sha256sum <"$TMP/exp-orig/go.sum")" "$(sha256sum <"$ISO/go.sum")"
	mkdir -p "$ISO/internal/lockfixture"
	cat >"$ISO/internal/lockfixture/fixture.go" <<'GO'
package lockfixture

import "github.com/dlclark/regexp2/v2"

// Compile is a throwaway fixture that makes the Go lock export non-trivial.
func Compile() (*regexp2.Regexp, error) { return regexp2.Compile(`a+`, 0) }
GO
	printf 'module github.com/ViktorsBaikers/Boxset\n\ngo 1.27\n\nrequire github.com/dlclark/regexp2/v2 v2.8.4\n' >"$ISO/go.mod"
	rm -rf "$TMP/exp-go" "$TMP/exp-web"
	mkdir -p "$TMP/exp-go" "$TMP/exp-web"
	ctl_begin
	(cd "$ISO" && docker build -f deploy/dev/Dockerfile --target lock-export --build-arg LOCK_ECOSYSTEM=go --output "type=local,dest=$TMP/exp-go" .) >"$TMP/lock-go.log" 2>&1
	_lx=$?
	[ "$_lx" = 0 ] || SETUP=1
	expect "Go lock export ran" "$_lx" 0
	expect "Go export publishes only go.mod and go.sum" "$(cd "$TMP/exp-go" && find . -type f | LC_ALL=C sort | tr '\n' ' ')" "./go.mod ./go.sum "
	grep -Fq 'github.com/dlclark/regexp2/v2 v2.8.4' "$TMP/exp-go/go.sum" && pass "go.sum records the fixture dependency" || fail "go.sum" "regexp2 v2.8.4 missing"
	expect "Go export left web/bun.lock byte-identical" "$(sha256sum <"$ISO/web/bun.lock")" "$locksum"
	(cd "$ISO" && docker build -f deploy/dev/Dockerfile --target lock-export --build-arg LOCK_ECOSYSTEM=web --output "type=local,dest=$TMP/exp-web" .) >"$TMP/lock-web.log" 2>&1
	_lx=$?
	[ "$_lx" = 0 ] || SETUP=1
	expect "web lock export ran" "$_lx" 0
	expect "web export publishes only bun.lock" "$(cd "$TMP/exp-web" && find . -type f | tr '\n' ' ')" "./bun.lock "
	expect "regenerating from the bound lock is byte-identical" "$(sha256sum <"$TMP/exp-web/bun.lock")" "$locksum"
	a_go_export_scoped() { # holds when the Go export publishes only Go files
		rm -rf "$TMP/exp-m"
		mkdir -p "$TMP/exp-m"
		(cd "$ISO" && docker build -f "$1" --target lock-export --build-arg LOCK_ECOSYSTEM=go --output "type=local,dest=$TMP/exp-m" .) >"$TMP/lock-m.log" 2>&1 || { OBS="lock-export build failed: $(tail -n 1 "$TMP/lock-m.log")"; SETUP=1; return 1; }
		OBS="exported: $(cd "$TMP/exp-m" && find . -type f | LC_ALL=C sort | tr '\n' ' ')"
		[ -n "$(cd "$TMP/exp-m" && find . -type f)" ] && reached
		[ "$(cd "$TMP/exp-m" && find . -type f | LC_ALL=C sort | tr '\n' ' ')" = "./go.mod ./go.sum " ]
	}
	cp "$ISO/deploy/dev/Dockerfile" "$MUT/Dockerfile.orig"
	cp "$ISO/deploy/dev/Dockerfile" "$MUT/Dockerfile.cross"
	mutate_file "$MUT/Dockerfile.cross" 'cp go.mod /out/go.mod' 'cp go.mod /out/go.mod && cp /src/web/bun.lock /out/bun.lock'
	redgreen "cross-ecosystem export mutant" a_go_export_scoped "$MUT/Dockerfile.cross" "$MUT/Dockerfile.orig"
	rm -rf "$ISO/internal/lockfixture"
	cp "$SRC/go.mod" "$ISO/go.mod"
	before=$(sha256sum "$ISO/web/bun.lock" "$ISO/go.mod" | sha256sum)
	rr -- true
	expect "an ordinary runner refresh succeeds" "$RS" 0
	expect "an ordinary runner refresh neither regenerates nor republishes a lock" "$(sha256sum "$ISO/web/bun.lock" "$ISO/go.mod" | sha256sum)" "$before"
fi

# next-consumer freshness: changed host fixture and Go source reach the next real consumer; locks match host bytes
mkdir -p "$ISO/testdata/refresh" "$ISO/internal/refreshprobe"
printf 'fixture one\n' >"$ISO/testdata/refresh/fixture.txt"
printf 'package refreshprobe\n\nconst Version = 1\n' >"$ISO/internal/refreshprobe/probe.go"
CONSUME='sha256sum /src/testdata/refresh/fixture.txt /src/internal/refreshprobe/probe.go /src/go.mod /src/web/bun.lock'
hostsums() { (cd "$ISO" && for f in testdata/refresh/fixture.txt internal/refreshprobe/probe.go go.mod web/bun.lock; do set -- $(sha256sum "$f"); printf '%s  /src/%s\n' "$1" "$f"; done); }
rr -- sh -c "$CONSUME"
first=$(cat "$TMP/rout")
expect "first consumer reports the host fixture, Go source, go.mod and bun.lock hashes" "$RS:$first" "0:$(hostsums)"
printf 'fixture two, changed on the host\n' >"$ISO/testdata/refresh/fixture.txt"
printf 'package refreshprobe\n\nconst Version = 2\n' >"$ISO/internal/refreshprobe/probe.go"
a_refresh() { # holds when the next consumer observes exactly the new bytes
	rrx "$ISO" "$1" -- sh -c "$CONSUME"
	OBS="status $RS, consumer saw: $(head -c 120 "$TMP/rout" | tr '\n' ' ')"
	[ "$RS" = 0 ] && [ "$(grep -Ec '^[0-9a-f]{64}  /src/' "$TMP/rout")" = 4 ] && reached
	[ "$RS" = 0 ] && [ "$(cat "$TMP/rout")" = "$(hostsums)" ]
}
fiso stale-context
# the mutant is a runner copy whose work-image build reads the stale copy of the checkout instead of the current one
variant mut-refresh-skipped '--progress plain . 2>&1' "--progress plain $TMP/stale-context 2>&1"
rm -rf "$TMP/stale-context"
mv "$FISO" "$TMP/stale-context"
printf 'fixture one\n' >"$TMP/stale-context/testdata/refresh/fixture.txt"
printf 'package refreshprobe\n\nconst Version = 1\n' >"$TMP/stale-context/internal/refreshprobe/probe.go"
redgreen "refresh-skipped mutant (stale build context)" a_refresh "$MUT/mut-refresh-skipped.sh" "$RUNNER"
rm -rf "$TMP/stale-context" "$ISO/testdata" "$ISO/internal/refreshprobe"

if [ "$FAILS" -eq 0 ]; then
	printf 'DEV-RUN-TEST-PASS controls=%s\n' "$PASSES"
	exit 0
fi
printf 'DEV-RUN-TEST-FAIL failed=%s passed=%s\n' "$FAILS" "$PASSES"
exit 1
