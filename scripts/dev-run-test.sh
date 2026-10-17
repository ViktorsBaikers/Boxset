#!/bin/sh
# Host-side contract driver for scripts/dev-run.sh, repository root only:
#   scripts/dev-run-test.sh
# Runs the real runner (a SHA-256-identical copy) against an isolated checkout of
# the current allowlisted inputs and asserts the runner trust boundary: exact argv
# and statuses, the protected fixed-name write maps (live 32 MiB cap, identity,
# ancestors, trusted supervisor, quiescence, staged publication, rollback and the
# commit point), binary-safe redaction, host loss, the capture handoff (real pinned
# chrome-headless-shell provider, validator, authority and lifetime controls), the
# validator and redactor unit tests through the same runner, and the toolchain
# admission mutation, support-block and lock-export controls. Every rejecting
# mutant runs the same named assertion twice: RED against the SHA-bound mutant and
# GREEN against the original (a fault copy changes only its injected seam).
# Exit: 0 all controls pass; 1 an assertion failed; 3 the browser provider is
# unavailable (T31-PROVIDER-UNAVAILABLE).
#   --tools-failure and --benchmark-evidence belong to later slices.
#   --classifier-self-check runs only the permanent self-check (no container, no build, no clock helper): the shared classifier, its case
#   latch and plain-credit gate, the plain-control targets, the parent-owned unknown observations, the lint-target shapes, the readiness
#   probes, the driver clock reader, the host-loss result classes, registration and watcher and the bounded terminal helper on a virtual
#   clock, the logging-shim receipts and natural-cell premise, the clock-recovery diagnostic and the hash-record oracle.
#   It is the exact deterministic command root runs before the full proof, which also begins with it.
# The runner is a bootstrap that execs the native lifecycle controller (deploy/dev/redact, controller.go): every runner mutant and fault
# copy is a compiled copy of that package (gvariant, in a private build context); only bootstrap controls remain shell copies (variant).
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
RUNNER_STARTED_RE='^dev-run: image=sha256:[0-9a-f]{64} inputs=[0-9a-f]{64} timeout=[0-9]+s container=boxset-dev-[0-9a-f]{16}$'
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
cname() { grep -o 'container=boxset-dev-[0-9a-f]*' "$TMP/rerr" | head -n 1 | cut -d= -f2; }
runid() { cname | sed 's/^boxset-dev-//'; }

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
# gvariant NAME FILE OLD NEW...: a SHA-bound compiled copy of the host helper (deploy/dev/redact). A private context holds copies
# of the isolated checkout's .dockerignore, go.mod and deploy/dev/redact/; every replacement is applied to FILE (controller.go or main.go)
# inside it, read back, and the changed file's SHA is recorded in $MUT/NAME.csha. $MUT/NAME.sh is a runner copy whose only change is the
# helper-build context operand, which now points at that context: callbacks keep their cwd (the isolated checkout) and the work image
# still builds from it. A partly built copy is removed and can never run or earn a RED; one that does not compile ends in
# redact_unavailable, which is infrastructure (INFRA_RE), never RED.
GCTX_OLD='--output "type=local,dest=$TMPD/bin" . >/dev/null 2>&1 || die redact_unavailable'
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
	if ! mutate_file "$MUT/$_gvn.sh" "$GCTX_OLD" "--output \"type=local,dest=\$TMPD/bin\" $MUT/ctx-$_gvn >/dev/null 2>&1 || die redact_unavailable"; then
		rm -f "$MUT/$_gvn.sh"
		return 1
	fi
}
# gobar SECONDS: the Go statements of an inserted barrier at a real statement of the controller: its own PID goes to $BAR/helper.pid, $BAR/ready is created and
# $BAR/go releases it, polled for at most SECONDS. There is no production hook: the statements exist only in a compiled copy.
gobar() {
	printf '%s' '_ = os.WriteFile("'$BAR'/helper.pid", []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); _ = os.WriteFile("'$BAR'/ready", nil, 0o644); for i := 0; i < '"$1"'; i++ { if _, err := os.Stat("'$BAR'/go"); err == nil { break }; time.Sleep(time.Second) }'
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
s5_done() { grep -Eq '^S5-ADMISSION-(PASS|FAIL) ' "$TMP/rout"; }
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
			printf 'dev-run: image=sha256:%s inputs=%s timeout=600s container=boxset-dev-0123456789abcdef\n' "$_ptz" "$_ptz" >"$TMP/rerr"
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
		SIM_PAUSE=0
		SIM_PN=0
		SIM_WHERE=after
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
		_c=$(grep -o 'container=boxset-dev-[0-9a-f]*' "$TMP/$1.err" 2>/dev/null | head -n 1 | cut -d= -f2)
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
clock_acquire clock_build || exit 125
REALDOCKER=$(command -v docker)
dshim_write "$TMP/dshim" "$REALDOCKER" "$CLOCK -clock" || exit 125
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
expect "dev-run prints image, inputs, timeout and container identity" "$(grep -Ec '^dev-run: image=sha256:[0-9a-f]{64} inputs=[0-9a-f]{64} timeout=600s container=boxset-dev-[0-9a-f]{16}$' "$TMP/rerr")" 1

# pre-deadline child 124 and 137 propagate; the deadline is observed, not guessed
rr -s 124 --timeout 1 -- sh -c 'exit 124'
expect "child exit 124 before the deadline propagates" "$RS" 124
noreason "child exit 124 is not reported as a timeout" timeout
rr -s 137 --timeout 1 -- sh -c 'kill -KILL $$'
expect "child kill -KILL (137) before the deadline propagates" "$RS" 137
noreason "child 137 is not reported as a timeout" timeout
a_pre_deadline() { # holds when a child exit 124 is not rewritten into a deadline
	rrx "$ISO" "$1" --timeout 10 -- sh -c 'exit 124'
	OBS="status $RS, $(grep -c 'dev-run: timeout' "$TMP/rerr") timeout lines"
	[ "$RS" = 124 ] && grep -Fq 'dev-run: image=' "$TMP/rerr" && reached
	[ "$RS" = 124 ] && ! grep -Fq 'dev-run: timeout' "$TMP/rerr"
}
gvariant mut-elapsed-heuristic controller.go 'if fi, err := os.Stat(c.tmpd + "/tmo"); err == nil && fi.Size() > 0 {' 'if fi, err := os.Stat(c.tmpd + "/tmo"); r.status == 124 || r.status == 137 || err == nil && fi.Size() > 0 {'
redgreen "elapsed-heuristic deadline mutant" a_pre_deadline "$MUT/mut-elapsed-heuristic.sh" "$RUNNER"

rr -w timeout -s 124 --timeout 3 -- sh -c 'setsid sh -c "sleep 120" & sleep 120'
expect "real deadline exits 124" "$RS" 124
reason "deadline reason word" timeout
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
# bg_wait_or_exit CTR FILE: FILE exists, or the background runner ended first (status 1)
bg_wait_or_exit() {
	_bei=0
	while [ "$_bei" -lt 600 ]; do
		if cx_obs "$1" "$2"; then return 0; fi
		kill -0 "$BG_PID" 2>/dev/null || return 1
		sleep 1
		_bei=$((_bei + 1))
	done
	return 1
}
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
gvariant mut-root-child controller.go 'childUser = "1000:1000"' 'childUser = "0:0"'
rrp -w supervisor_untrusted "$ISO" "$MUT/mut-root-child.sh" -- true
expect "a -u 0:0 child is refused before the child runs" "$RS" 125
reason "root child reason" supervisor_untrusted
gvariant mut-root-child-nopreflight controller.go 'childUser = "1000:1000"' 'childUser = "0:0"' 'func (c *ctl) supervisorPreflight() bool {' 'func (c *ctl) supervisorPreflight() bool {
	return true'
redgreen "root-child with the trust preflight removed rewrites /usr/bin/stat" a_no_tamper "$MUT/mut-root-child-nopreflight.sh" "$RUNNER"

fiso fault-usr
printf '\nRUN touch /usr/bin/zz-writable && chmod 0666 /usr/bin/zz-writable\n' >>"$FISO/deploy/dev/Dockerfile"
rrp -w supervisor_untrusted "$FISO" "$FISO/scripts/dev-run.sh" -- true
expect "a Dockerfile fault copy leaving one other-writable file under /usr is refused" "$RS" 125
reason "other-writable /usr reason" supervisor_untrusted
rm -rf "$FISO"

# Groups predicate (T26): each role's identity status is presented through the child_status/pid1_status
# seams of fault copies. Empty and the sole primary GID are accepted (the child runs); a missing Groups
# line, a repeated primary, any other GID, an unparseable token and the other role's primary GID are
# refused with supervisor_untrusted before the child. A run option that adds a group is refused likewise.
# The same assertion fails on the matching predicate mutant.
a_grp_ok() { # holds when the intended identity runs the child
	rrx "$ISO" "$1" -- sh -c 'printf ok'
	OBS="status $RS, output '$(cat "$TMP/rout")', $(grep -o 'dev-run: [a-z_]*' "$TMP/rerr" | tail -n 1)"
	{ { [ "$RS" = 0 ] && [ "$(cat "$TMP/rout")" = ok ]; } || { [ "$RS" = 125 ] && grep -Fq 'dev-run: supervisor_untrusted' "$TMP/rerr"; }; } && reached
	[ "$RS" = 0 ] && [ "$(cat "$TMP/rout")" = ok ]
}
a_grp_refused() { # holds when the identity is refused before the child
	rrx "$ISO" "$1" -- sh -c 'printf ran'
	OBS="status $RS, output '$(cat "$TMP/rout")', $(grep -o 'dev-run: [a-z_]*' "$TMP/rerr" | tail -n 1)"
	{ { [ "$RS" = 125 ] && grep -Fq 'dev-run: supervisor_untrusted' "$TMP/rerr"; } || [ "$(cat "$TMP/rout")" = ran ]; } && reached
	[ "$RS" = 125 ] && grep -Fq 'dev-run: supervisor_untrusted' "$TMP/rerr" && [ -z "$(cat "$TMP/rout")" ]
}
GRP_CHILD_OLD='return r.text()'
GRP_PID1_OLD='return c.supCall(10, "/usr/bin/cat", "/proc/1/status").text()'
GRP_PRED='!ok || !(len(g) == 0 || (len(g) == 1 && g[0] == id))'
gseam() { # gseam ROLE MODE GROUPS: the Go identity-status seam presenting a rewritten (set) or removed (missing) Groups line
	_gsk='if strings.HasPrefix(l, "Groups:") {'
	[ "$2" != set ] || _gsk=$_gsk'
			kept = append(kept, "Groups:\t'"$3"'")'
	_gsk=$_gsk'
			continue
		}'
	case $1 in
	child) _gsh='t, ok := r.text()' ;;
	pid1) _gsh='t, ok := c.supCall(10, "/usr/bin/cat", "/proc/1/status").text()' ;;
	esac
	printf '%s\n\tvar kept []string\n\tfor _, l := range strings.Split(t, "\\n") {\n\t\t%s\n\t\tkept = append(kept, l)\n\t}\n\treturn strings.Join(kept, "\\n"), ok' "$_gsh" "$_gsk"
}
grp_case() { # grp_case ROLE NAME EXPECT(ok|refused) MODE GROUPS MUTANT
	case $1 in
	child) _gco=$GRP_CHILD_OLD ;;
	pid1) _gco=$GRP_PID1_OLD ;;
	esac
	_gcn=$(gseam "$1" "$4" "$5")
	case $6 in
	reject-empty) _gcr='!ok || !(len(g) == 1 && g[0] == id)' ;;
	primary-rejected) _gcr='!ok || !(len(g) == 0)' ;;
	missing-as-empty) _gcr='!(len(g) == 0 || (len(g) == 1 && g[0] == id))' ;;
	accept-any) _gcr='len(g) < 0' ;;
	*)
		fail "Groups $1 $2 ($6 mutant)" "unknown mutant kind"
		return
		;;
	esac
	# The seam copy and the mutant are compiled copies of the same package, each read back by mutate_file; the mutant must differ from its seam copy before anything runs.
	if ! gvariant "grp-$1-$2" controller.go "$_gco" "$_gcn" || ! gvariant "mut-grp-$1-$2" controller.go "$_gco" "$_gcn" "$GRP_PRED" "$_gcr"; then return; fi
	if [ "$(cat "$MUT/grp-$1-$2.csha")" = "$(cat "$MUT/mut-grp-$1-$2.csha")" ]; then
		fail "Groups $1 $2 ($6 mutant)" "the mutant is byte-identical to its seam copy"
		return
	fi
	if [ "$3" = ok ]; then _gca=a_grp_ok; else _gca=a_grp_refused; fi
	redgreen "Groups $1 $2 ($6 mutant)" "$_gca" "$MUT/mut-grp-$1-$2.sh" "$MUT/grp-$1-$2.sh"
}
for _role in child pid1; do
	if [ "$_role" = child ]; then _p=1000 _o2=65534; else _p=65534 _o2=1000; fi
	grp_case "$_role" empty ok set '' reject-empty
	grp_case "$_role" primary ok set "$_p" primary-rejected
	grp_case "$_role" missing refused missing '' missing-as-empty
	grp_case "$_role" extra refused set "$_p 4242" accept-any
	grp_case "$_role" duplicate refused set "$_p $_p" accept-any
	grp_case "$_role" malformed refused set x accept-any
	grp_case "$_role" other-role refused set "$_o2" accept-any
done
gvariant fault-groupadd controller.go '"--tmpfs", scratchTmpfs}' '"--tmpfs", scratchTmpfs, "--group-add", "4242"}'
gvariant mut-groupadd-accept controller.go '"--tmpfs", scratchTmpfs}' '"--tmpfs", scratchTmpfs, "--group-add", "4242"}' "$GRP_PRED" 'len(g) < 0'
redgreen "run option --group-add 4242 (Groups predicate accepting any entry reaches the child)" a_grp_refused "$MUT/mut-groupadd-accept.sh" "$MUT/fault-groupadd.sh"

# ===== 3. protected maps: preflight facts, in-container denials, caps (T26) ==============================
MAPFACTS='grep " /src/testdata/parser tmpfs " /proc/self/mounts >/tmp/m || exit 21
set -- $(cat /tmp/m)
case ",$4," in *,rw,*) ;; *) exit 22 ;; esac
for tok in nosuid nodev noexec size=32768k nr_inodes=8 mode=755 uid=65534 gid=65534; do case ",$4," in *",$tok,"*) ;; *) exit 23 ;; esac; done
for n in series.jsonl movie.jsonl; do
	[ "$(stat -c "%u:%g %a %h %F %s" /src/testdata/parser/$n)" = "65534:65534 666 1 regular empty file 0" ] || exit 24
done
[ "$(ls -A /src/testdata/parser | wc -l)" = 2 ] || exit 25
for d in / /src /src/testdata; do
	set -- $(stat -c "%u %a" $d)
	[ "$1" = 0 ] || exit 26
done'
rr --write testdata/parser -- sh -c "$MAPFACTS"
expect "write map: tmpfs options, two precreated empty 0666 65534 single-link files, root-owned ancestors" "$RS" 0

DENIALS='before=$(stat -c %i /src/testdata/parser/series.jsonl /src/testdata/parser/movie.jsonl)
if touch /src/testdata/parser/probe 2>/dev/null; then exit 31; fi
if ln -s /src/escape.bin /src/testdata/parser/x 2>/dev/null; then exit 32; fi
if mkdir /src/testdata/parser/d 2>/dev/null; then exit 33; fi
if mkfifo /src/testdata/parser/f 2>/dev/null; then exit 34; fi
if rm /src/testdata/parser/series.jsonl 2>/dev/null; then exit 35; fi
if mv /src/testdata/parser/series.jsonl /src/testdata/parser/t 2>/dev/null; then exit 36; fi
if ln -sf /src/escape.bin /src/testdata/parser/series.jsonl 2>/dev/null; then exit 37; fi
if chmod 0644 /src/testdata/parser/series.jsonl 2>/dev/null; then exit 38; fi
if ln /src/testdata/parser/series.jsonl /tmp/h 2>/dev/null; then exit 39; fi
if mv /src/testdata /src/t2 2>/dev/null; then exit 40; fi
if rmdir /src/testdata/parser 2>/dev/null; then exit 41; fi
if mv /src/docs/spikes /src/x 2>/dev/null; then exit 42; fi
[ "$(stat -c %i /src/testdata/parser/series.jsonl /src/testdata/parser/movie.jsonl)" = "$before" ] || exit 43
[ ! -e /src/escape.bin ] || exit 44
printf kept >/src/testdata/parser/series.jsonl || exit 45'
rr --write testdata/parser -- sh -c "$DENIALS"
expect "child cannot create, link, rename, unlink, chmod or replace anything on a write-root path" "$RS" 0
expect "the in-place write still publishes" "$(cat "$ISO/testdata/parser/series.jsonl" 2>/dev/null)" kept
expect "zero-byte movie.jsonl publishes nothing" "$([ -e "$ISO/testdata/parser/movie.jsonl" ] && echo present || echo absent)" absent
rm -rf "$ISO/testdata"

DRAFTMAP="$(printf '%s' "$MAPFACTS" | sed 's|/src/testdata/parser|/src/docs/spikes/issue-drafts|g; s|series.jsonl|s5-admission.md|g; s|movie.jsonl|s5-tools.md|g; s| /src/testdata; do| /src/docs /src/docs/spikes; do|')"
rr --write docs/spikes/issue-drafts -- sh -c "$DRAFTMAP"
expect "draft map has the same protected shape with s5-admission.md and s5-tools.md" "$RS" 0

# allowed writes, cold roots (require-pre-existing-root mutant RED)
a_cold_root() { # holds when an absent approved write root is created and published
	rm -rf "$ISO/testdata"
	rrx "$ISO" "$1" --write testdata/parser -- sh -c 'printf cold >/src/testdata/parser/series.jsonl'
	OBS="status $RS, host content '$(cat "$ISO/testdata/parser/series.jsonl" 2>/dev/null)'"
	{ { [ "$RS" = 0 ] && [ -n "$(cat "$ISO/testdata/parser/series.jsonl" 2>/dev/null)" ]; } || { [ "$RS" = 125 ] && grep -Fq 'dev-run: write_root_create_failed' "$TMP/rerr"; }; } && reached
	[ "$RS" = 0 ] && [ "$(cat "$ISO/testdata/parser/series.jsonl" 2>/dev/null)" = cold ]
}
gvariant mut-require-root controller.go 'if err := c.repoRoot.Mkdir(cur, 0o755); err != nil {' 'if err := errors.New("creation disabled"); err != nil {'
redgreen "require-pre-existing-root mutant" a_cold_root "$MUT/mut-require-root.sh" "$RUNNER"
rr --write docs/spikes/issue-drafts -- sh -c 'printf draft >/src/docs/spikes/issue-drafts/s5-admission.md'
expect "initially absent draft root publishes s5-admission.md" "$RS:$(cat "$ISO/docs/spikes/issue-drafts/s5-admission.md" 2>/dev/null)" "0:draft"
rm -rf "$ISO/testdata" "$ISO/docs/spikes/issue-drafts"

# refused paths change nothing
before=$(tree_hash)
for w in '../x' '/tmp/x' 'testdata/parser/../../x' './testdata/parser' 'testdata/parser/' 'testdata//parser' docs testdata 'docs/spikes/data'; do
	rr -w write_path_invalid --write "$w" -- true
	if [ "$RS" = 125 ]; then pass "write path '$w' refused ($(grep -o 'dev-run: [a-z_]*' "$TMP/rerr" | head -n 1))"; else fail "write path '$w'" "status $RS"; fi
done
expect "refused paths changed nothing on the host" "$(tree_hash)" "$before"
mkdir -p "$ISO/testdata"
ln -s "$CANARY" "$ISO/testdata/parser"
rr -w write_root_symlink --write testdata/parser -- true
expect "symlinked write root refused" "$RS" 125
reason "symlink root reason" write_root_symlink
rm -f "$ISO/testdata/parser"
mkdir -p "$ISO/docs/spikes"
mv "$ISO/docs/spikes" "$ISO/docs/spikes.real"
ln -s "$CANARY" "$ISO/docs/spikes"
rr -w write_root_symlink --write docs/spikes/issue-drafts -- true
expect "symlinked ancestor refused" "$RS" 125
reason "symlink ancestor reason" write_root_symlink
rm -f "$ISO/docs/spikes"
mv "$ISO/docs/spikes.real" "$ISO/docs/spikes"
expect "canary directory untouched by refusals" "$(find "$CANARY" | LC_ALL=C sort | tr '\n' ' ')" "$CANARY $CANARY/canary.txt "
rm -rf "$ISO/testdata" "$ISO/docs/spikes/issue-drafts"

# preflight mutants: faults change only the start seam and the original refuses
a_refuse_root() { # holds when the runner refuses a wrongly shaped write map before the child
	rrx "$ISO" "$1" --write testdata/parser -- sh -c 'touch /tmp/ran'
	OBS="status $RS, $(grep -o 'dev-run: [a-z_]*' "$TMP/rerr" | tail -n 1)"
	[ "$RS" = 125 ] && grep -Fq 'dev-run: write_root_invalid' "$TMP/rerr" && reached
	[ "$RS" = 125 ] && grep -Fq 'dev-run: write_root_invalid' "$TMP/rerr"
}
gvariant fault-size64 controller.go 'size=33554432,nr_inodes=8,mode=0755,uid=65534' 'size=67108864,nr_inodes=8,mode=0755,uid=65534'
gvariant fault-nosize controller.go 'size=33554432,nr_inodes=8,mode=0755,uid=65534' 'nr_inodes=8,mode=0755,uid=65534'
gvariant fault-childowned controller.go 'size=33554432,nr_inodes=8,mode=0755,uid=65534,gid=65534"' 'size=33554432,nr_inodes=8,mode=0700,uid=1000,gid=1000"'
gvariant fault-noprecreate controller.go '!c.precreate("/src/"+c.write, c.mapNames)' 'false'
for f in fault-size64 fault-nosize fault-childowned fault-noprecreate; do
	holds "$f is refused with write_root_invalid before the child" a_refuse_root "$MUT/$f.sh"
done
fiso fault-ancestor
mutate_file "$FISO/deploy/dev/Dockerfile" 'chmod 1777 "$d"' 'chmod 0777 "$d"'
rrp -w write_root_invalid "$FISO" "$FISO/scripts/dev-run.sh" --write testdata/parser -- touch /tmp/ran
expect "Dockerfile fault copy without the 1777 ancestor rule gives write_root_invalid" "$RS" 125
reason "ancestor rule reason" write_root_invalid
a_ancestor_refused() { # a_ancestor_refused RUNNER: with the Dockerfile fault copy (no 1777 ancestor rule) the runner refuses before the child
	rrx "$FISO" "$1" --write testdata/parser -- sh -c 'printf ran'
	OBS="status $RS, child output '$(cat "$TMP/rout")', $(grep -o 'dev-run: [a-z_]*' "$TMP/rerr" | tail -n 1)"
	{ { [ "$RS" = 125 ] && grep -Fq 'dev-run: write_root_invalid' "$TMP/rerr"; } || [ "$(cat "$TMP/rout")" = ran ]; } && reached
	[ "$RS" = 125 ] && grep -Fq 'dev-run: write_root_invalid' "$TMP/rerr" && [ -z "$(cat "$TMP/rout")" ]
}
gvariant mut-no-ancestor-check controller.go 'if len(f) != 5 || !ancestorOK(f[2], f[3], f[4]) {' 'if len(f) != 5 {'
redgreen "ancestor-preflight-removed mutant (Dockerfile without the 1777 ancestor rule) reaches the child" a_ancestor_refused "$MUT/mut-no-ancestor-check.sh" "$FISO/scripts/dev-run.sh"
rm -rf "$FISO"

# exact limit, live +1, split +1, refuse-then-truncate, swallow-then-exit-0, sparse
rm -rf "$ISO/testdata"
mkdir -p "$ISO/testdata/parser"
printf 'prior movie\n' >"$ISO/testdata/parser/movie.jsonl"
priorR=$(fhash "$ISO/testdata/parser/movie.jsonl")
zero32=$(head -c 33554432 /dev/zero | sha256sum | cut -d' ' -f1)
rr --write testdata/parser -- sh -c 'head -c 33554432 /dev/zero >/src/testdata/parser/series.jsonl'
expect "exact 33554432-byte write publishes" "$RS:$(wc -c <"$ISO/testdata/parser/series.jsonl" | tr -d ' '):$(fhash "$ISO/testdata/parser/series.jsonl")" "0:33554432:$zero32"
noreason "exact limit adds no write_cap_reached" write_cap_reached
rm -f "$ISO/testdata/parser/series.jsonl"
CAPSH='head -c 33554433 /dev/zero >/src/testdata/parser/series.jsonl && printf PRODUCED'
rr -s 1 --write testdata/parser -- sh -c "$CAPSH"
expect "live +1: producer fails at the refused byte, status 1 unchanged" "$RS" 1
if grep -Fq PRODUCED "$TMP/rout"; then fail "live +1 sentinel" "completion sentinel was produced"; else pass "live +1: PRODUCED never printed"; fi
grep -Fqi 'space' "$TMP/rout" && pass "live +1: the producer's ENOSPC error is in the redacted output" || fail "live +1 ENOSPC" "$(head -c 300 "$TMP/rout")"
expect "live +1: exactly the cap line and nothing else from the runner about the root" "$(grep -c 'dev-run: write_cap_reached root=testdata/parser' "$TMP/rerr")" 1
expect "live +1: nothing published, prior movie.jsonl unchanged" "$(fhash "$ISO/testdata/parser/movie.jsonl"):$([ -e "$ISO/testdata/parser/series.jsonl" ] && echo present || echo absent)" "$priorR:absent"
gone "$(cname)" "live +1: owned container absent after the run (status-checked)"
rr -s 1 --write testdata/parser -- sh -c 'head -c 16777216 /dev/zero >/src/testdata/parser/series.jsonl && head -c 16777217 /dev/zero >/src/testdata/parser/movie.jsonl && printf PRODUCED'
expect "split +1 across both map files gives status 1, no sentinel, cap line" "$RS:$(grep -c PRODUCED "$TMP/rout"):$(grep -c 'write_cap_reached root=testdata/parser' "$TMP/rerr")" "1:0:1"
expect "split +1 publishes nothing" "$(fhash "$ISO/testdata/parser/movie.jsonl"):$([ -e "$ISO/testdata/parser/series.jsonl" ] && echo present || echo absent)" "$priorR:absent"
rr -s 1 --write testdata/parser -- sh -c 'head -c 33554433 /dev/zero >/src/testdata/parser/series.jsonl && printf PRODUCED; s=$?; truncate -s 0 /src/testdata/parser/series.jsonl; exit $s'
expect "refuse-then-truncate: status 1, no sentinel, no cap line, nothing published" "$RS:$(grep -c PRODUCED "$TMP/rout"):$(grep -c write_cap_reached "$TMP/rerr"):$([ -e "$ISO/testdata/parser/series.jsonl" ] && echo present || echo absent):$(fhash "$ISO/testdata/parser/movie.jsonl")" "1:0:0:absent:$priorR"
rr --write testdata/parser -- sh -c 'head -c 33554433 /dev/zero >/src/testdata/parser/series.jsonl; exit 0'
expect "swallow-then-exit-0 publishes exactly 33554432 bytes, never more" "$RS:$(wc -c <"$ISO/testdata/parser/series.jsonl" | tr -d ' ')" "0:33554432"
rm -f "$ISO/testdata/parser/series.jsonl"
rr -w write_audit_failed --write testdata/parser -- sh -c 'truncate -s 33554433 /src/testdata/parser/series.jsonl'
expect "a sparse 33554433-byte map file is write_audit_failed with zero host writes" "$RS:$([ -e "$ISO/testdata/parser/series.jsonl" ] && echo present || echo absent)" "125:absent"
reason "sparse reason" write_audit_failed
rr -w write_audit_failed --write testdata/parser -- sh -c 'truncate -s 20971520 /src/testdata/parser/series.jsonl; truncate -s 20971520 /src/testdata/parser/movie.jsonl'
expect "two 20 MiB sparse files exceeding the cap together are write_audit_failed" "$RS:$(fhash "$ISO/testdata/parser/movie.jsonl")" "125:$priorR"

# unprotected-map mutant: child-owned staging root, no precreation, no preflight
a_map_bound() { # holds when container writable-layer growth stays below 1 MiB through the map
	bg_run mapb "$1" --write testdata/parser -- sh -c 'ln -s /src/escape.bin /src/testdata/parser/series.jsonl 2>/dev/null; head -c 33554433 /dev/zero >/src/testdata/parser/series.jsonl 2>/dev/null; touch /tmp/ready; n=0; while [ ! -e /tmp/go ] && [ "$n" -lt 600 ]; do sleep 1; n=$((n + 1)); done'
	_c=$(bg_ctr mapb) || { OBS="no container"; SETUP=1; return 1; }
	if ! bg_wait "$_c" /tmp/ready; then OBS="not ready"; SETUP=1; kill -KILL "$BG_PID" 2>/dev/null; return 1; fi
	_sz=$(timeout 30 docker inspect -s -f '{{.SizeRw}}' "$_c" 2>/dev/null) || _sz=
	bg_touch "$_c" /tmp/go || SETUP=1
	bg_end mapb
	OBS="SizeRw ${_sz:-unknown}"
	case $_sz in '' | *[!0-9]*) SETUP=1; return 1 ;; esac
	reached
	[ "$_sz" -lt 1048576 ] && [ ! -e "$ISO/escape.bin" ]
}
gvariant mut-unprotected-map controller.go 'size=33554432,nr_inodes=8,mode=0755,uid=65534,gid=65534"' 'size=33554432,nr_inodes=8,mode=0700,uid=1000,gid=1000"' '!c.precreate("/src/"+c.write, c.mapNames)' 'false' '!c.writePreflight()' 'false'
redgreen "unprotected-map mutant (child-owned root, no precreation, no preflight)" a_map_bound "$MUT/mut-unprotected-map.sh" "$RUNNER"
rm -rf "$ISO/testdata"
# the live map seen from the host: while the producer is held before completion the host write root stays exactly as it was. The child's write is first observed inside the container (the
# target premise); an unsafe bind or unstaged implementation shows it on the host at that same moment. Nothing is written outside the isolated, test-owned roots.
canary_state() { { find "$CANARY" | LC_ALL=C sort; find "$CANARY" -type f -exec sha256sum {} + | LC_ALL=C sort; } | sha256sum; } # names and content hashes of everything outside the checkout
live_host() { (cd "$ISO/testdata/parser" 2>/dev/null && { find . | LC_ALL=C sort; find . -type f -exec sha256sum {} + | LC_ALL=C sort; }) | sha256sum; }
a_live_hostwrite() { # a_live_hostwrite RUNNER: holds when nothing reaches the host write root before the producer completes
	rm -rf "$ISO/testdata"
	mkdir -p "$ISO/testdata/parser"
	printf 'prior movie\n' >"$ISO/testdata/parser/movie.jsonl"
	_lh0=$(live_host)
	_lc0=$(canary_state)
	bg_run live "$1" --write testdata/parser -- sh -c "printf transient >/src/testdata/parser/series.jsonl; $CBAR; printf done >/src/testdata/parser/series.jsonl"
	_c=$(bg_ctr live) || { OBS="no container"; SETUP=1; return 1; }
	if ! bg_wait "$_c" /tmp/ready; then OBS="not ready"; SETUP=1; kill -KILL "$BG_PID" 2>/dev/null; bg_end live; return 1; fi
	_lw=$(cx "$_c" /usr/bin/cat /src/testdata/parser/series.jsonl 2>/dev/null)
	_lh1=$(live_host)
	_lc1=$(canary_state)
	bg_touch "$_c" /tmp/go || SETUP=1
	bg_end live
	OBS="the producer, held before completion, wrote '$_lw' into its map; host write root unchanged: $([ "$_lh0" = "$_lh1" ] && echo yes || echo NO), canary unchanged: $([ "$_lc0" = "$_lc1" ] && echo yes || echo NO)"
	[ "$_lw" = transient ] || { SETUP=1; return 1; }
	reached
	[ "$_lh0" = "$_lh1" ] && [ "$_lc0" = "$_lc1" ]
}
BIND_OLD='args = append(args, "--tmpfs", "/src/"+c.write+writeTmpfsOpts)'
gvariant mut-live-bind controller.go "$BIND_OLD" '_ = os.Chmod(c.repo+"/"+c.write, 0o777)
		args = append(args, "--mount", "type=bind,source="+c.repo+"/"+c.write+",target=/src/"+c.write)' '!c.writePreflight()' 'false'
redgreen "live-map unsafe bind (the host write root is exposed before completion)" a_live_hostwrite "$MUT/mut-live-bind.sh" "$RUNNER"
rm -rf "$ISO/testdata"

# drafts: nonzero child publishes its redacted draft; corpus root publishes nothing
mkdir -p "$ISO/testdata/parser" "$ISO/docs/spikes/issue-drafts"
printf 'old\n' >"$ISO/testdata/parser/series.jsonl"
rr -s 5 --write testdata/parser -- sh -c 'printf new >/src/testdata/parser/series.jsonl; exit 5'
expect "nonzero child: corpus root publishes nothing and keeps the status" "$RS:$(cat "$ISO/testdata/parser/series.jsonl")" "5:old"
rr -s 7 --write docs/spikes/issue-drafts -- sh -c 'printf draft >/src/docs/spikes/issue-drafts/s5-admission.md; exit 7'
expect "nonzero child: the draft is published and the status kept" "$RS:$(cat "$ISO/docs/spikes/issue-drafts/s5-admission.md")" "7:draft"
rm -f "$ISO/docs/spikes/issue-drafts/s5-admission.md"

# issue-drafts root filled through s5-tools.md with a nonzero child (cap-ignoring publication mutant RED). Like the draft-length controls below it is reached only by its own positive receipt (DRCPT)
# written by a compiled copy at the seam under test: an unrelated exporter, audit or redaction failure speaks the same fixed word, leaves the receipt absent or different and earns neither polarity.
DRCPT=$TMP/draft.receipt
drcpt() { printf '%s' '{ f, _ := os.OpenFile("'$DRCPT'", os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644); fmt.Fprintf(f, '"$1"'); f.Close() }'; }
neither_polarity() { # neither_polarity NAME FN RUNNER: the same assertion FN on RUNNER earns neither RED nor GREEN: the failure it meets is not its target
	_npn=$1
	_npf=$2
	rg_reset
	_npc=0
	"$_npf" "$3" || _npc=$?
	_npr=n
	_npg=n
	rg_verdict red "$_npc" && _npr=R
	rg_verdict green "$_npc" && _npg=G
	META=1
	expect "$_npn" "$_npr$_npg" nn
	META=0
}
a_full_draft_root() { # holds when a full draft root publishes nothing
	rm -f "$ISO/docs/spikes/issue-drafts/s5-admission.md" "$ISO/docs/spikes/issue-drafts/s5-tools.md"
	: >"$DRCPT" || { OBS="the seam receipt could not be created"; SETUP=1; return 1; }
	rrx "$ISO" "$1" --write docs/spikes/issue-drafts -- sh -c 'printf draft >/src/docs/spikes/issue-drafts/s5-admission.md; head -c 33554432 /dev/zero >/src/docs/spikes/issue-drafts/s5-tools.md 2>/dev/null; exit 1'
	OBS="status $RS, seam receipt '$(cat "$DRCPT")', admission draft $([ -e "$ISO/docs/spikes/issue-drafts/s5-admission.md" ] && echo published || echo absent)"
	# the target: the runner's own audit found the protected map full (free 0) under a nonzero child, read at the cap seam
	[ "$(cat "$DRCPT")" = "free 0 cs 1" ] && reached
	[ "$RS" = 1 ] && grep -Fq 'dev-run: write_cap_reached root=docs/spikes/issue-drafts' "$TMP/rerr" && [ ! -e "$ISO/docs/spikes/issue-drafts/s5-admission.md" ] && [ ! -e "$ISO/docs/spikes/issue-drafts/s5-tools.md" ]
}
DCAP_OLD='	if cs != 0 {
		if free == "0" {'
DCAP_NEW='	'"$(drcpt '"free %s cs %d\n", free, cs')"'
	if cs != 0 {
		if free == "0" {'
gvariant fault-draft-full controller.go "$DCAP_OLD" "$DCAP_NEW"
gvariant mut-cap-ignoring controller.go "$DCAP_OLD" "$DCAP_NEW" 'if free == "0" {' 'if free == "0" && false {'
redgreen "cap-ignoring publication mutant" a_full_draft_root "$MUT/mut-cap-ignoring.sh" "$MUT/fault-draft-full.sh"

# redaction changes draft length: expanding and shrinking drafts keep a valid current-run draft. The target of both is the receipt of the size comparison: the audited raw size and the redacted part size.
DSIZE_OLD='return err == nil && fi.Size() >= 1 && fi.Size() <= capDraft'
DSIZE_RCPT='if err == nil {
			'"$(drcpt '"part %d raw %d\n", fi.Size(), size')"'
		}
		'
gvariant fault-draft-size controller.go "$DSIZE_OLD" "$DSIZE_RCPT$DSIZE_OLD"
gvariant mut-raw-eq-emitted controller.go "$DSIZE_OLD" "${DSIZE_RCPT}return err == nil && fi.Size() == size"
a_draft_expand() { # holds when a 7-byte selected value expands to 10 bytes and the draft is kept
	rm -f "$ISO/docs/spikes/issue-drafts/s5-admission.md"
	: >"$DRCPT" || { OBS="the seam receipt could not be created"; SETUP=1; return 1; }
	(cd "$ISO" && BOXSET_TEST_SECRET=ADM-003 "$1" --write docs/spikes/issue-drafts -- sh -c 'printf "blocked ADM-003 end" >/src/docs/spikes/issue-drafts/s5-admission.md; exit 1') >"$TMP/rout" 2>"$TMP/rerr"
	RS=$?
	rerr_seen
	OBS="status $RS, seam receipt '$(cat "$DRCPT")', draft '$(cat "$ISO/docs/spikes/issue-drafts/s5-admission.md" 2>/dev/null)'"
	# the target: the audited raw size (19 bytes) met the redacted part (22 bytes) at the size comparison
	[ "$(cat "$DRCPT")" = "part 22 raw 19" ] && reached
	[ "$RS" = 1 ] && [ "$(cat "$ISO/docs/spikes/issue-drafts/s5-admission.md" 2>/dev/null)" = "blocked [REDACTED] end" ]
}
redgreen "raw-size-equals-emitted-size mutant drops the expanded draft" a_draft_expand "$MUT/mut-raw-eq-emitted.sh" "$MUT/fault-draft-size.sh"
DRAFT_SHRINK_IN='see https://aaaaaaaaa:bbbbbbbb@h/p?k=cccccccccccccccccccc end'
DRAFT_SHRINK_OUT='see https://[REDACTED]@h/p?k=[REDACTED] end'
a_draft_shrink() { # holds when an 18-byte userinfo and 20-byte query value shrink the draft but it is kept
	rm -f "$ISO/docs/spikes/issue-drafts/s5-admission.md"
	: >"$DRCPT" || { OBS="the seam receipt could not be created"; SETUP=1; return 1; }
	rrx "$ISO" "$1" --write docs/spikes/issue-drafts -- sh -c 'printf "see https://aaaaaaaaa:bbbbbbbb@h/p?k=cccccccccccccccccccc end" >/src/docs/spikes/issue-drafts/s5-admission.md; exit 1'
	OBS="status $RS, seam receipt '$(cat "$DRCPT")', draft '$(cat "$ISO/docs/spikes/issue-drafts/s5-admission.md" 2>/dev/null)'"
	# the target: the audited raw size met the shorter redacted part at the size comparison
	[ "$(cat "$DRCPT")" = "part $(printf %s "$DRAFT_SHRINK_OUT" | wc -c | tr -d ' ') raw $(printf %s "$DRAFT_SHRINK_IN" | wc -c | tr -d ' ')" ] && reached
	[ "$RS" = 1 ] && [ "$(cat "$ISO/docs/spikes/issue-drafts/s5-admission.md" 2>/dev/null)" = "$DRAFT_SHRINK_OUT" ]
}
redgreen "size-compare mutant drops the shrunk draft" a_draft_shrink "$MUT/mut-raw-eq-emitted.sh" "$MUT/fault-draft-size.sh"

# exporter prefix fault: the raw count catches a truncated draft (raw-count-removed mutant RED). The target is the receipt of the fault itself: a 5-byte prefix of the complete export got through.
PREFIX_BODY='a complete draft body'
a_prefix_draft() { # holds when a prefix-only export is write_audit_failed with no draft
	rm -f "$ISO/docs/spikes/issue-drafts/s5-admission.md"
	: >"$DRCPT" || { OBS="the seam receipt could not be created"; SETUP=1; return 1; }
	rrx "$ISO" "$1" --write docs/spikes/issue-drafts -- sh -c 'printf "a complete draft body" >/src/docs/spikes/issue-drafts/s5-admission.md; exit 1'
	OBS="status $RS, seam receipt '$(cat "$DRCPT")', draft $([ -e "$ISO/docs/spikes/issue-drafts/s5-admission.md" ] && echo published || echo absent)"
	# the target: the exporter delivered exactly its 5-byte prefix of the complete export of the whole body
	[ "$(cat "$DRCPT")" = "export 5 $(printf %s "$PREFIX_BODY" | wc -c | tr -d ' ')" ] && reached
	[ "$RS" = 1 ] && grep -Fq 'dev-run: write_audit_failed root=docs/spikes/issue-drafts' "$TMP/rerr" && [ ! -e "$ISO/docs/spikes/issue-drafts/s5-admission.md" ]
}
PREFIX_OLD='lim := io.LimitReader(in, capMap+1)'
PREFIX_NEW='lim := io.Reader(io.LimitReader(in, capMap+1))
			{
				var head [5]byte
				n, _ := io.ReadFull(lim, head[:])
				rest, _ := io.Copy(io.Discard, lim)
				'"$(drcpt '"export %d %d\n", n, int64(n)+rest')"'
				lim = bytes.NewReader(head[:n])
			}'
gvariant fault-prefix controller.go "$PREFIX_OLD" "$PREFIX_NEW"
gvariant mut-raw-removed controller.go "$PREFIX_OLD" "$PREFIX_NEW" 'streamOpts{capN: capDraft, raw: size, tail: -1}' 'streamOpts{capN: capDraft, raw: -1, tail: -1}'
redgreen "raw-count-removed mutant publishes the truncated draft" a_prefix_draft "$MUT/mut-raw-removed.sh" "$MUT/fault-prefix.sh"
# an unrelated exporter failure (the supervisor export command fails outright) says write_audit_failed with a nonzero child and no draft, as the target's refusal does, but leaves
# every seam receipt absent or different: the three draft controls earn neither polarity on it
gvariant fault-draft-unrelated controller.go 'return c.sup(30, "/usr/bin/cat", "--", "/src/"+c.write+"/"+name)' 'return c.sup(30, "/usr/bin/sh", "-c", "exit 1")' "$DSIZE_OLD" "$DSIZE_RCPT$DSIZE_OLD" "$PREFIX_OLD" "$PREFIX_NEW"
neither_polarity "an unrelated exporter failure earns neither RED nor GREEN for the expanded-draft control" a_draft_expand "$MUT/fault-draft-unrelated.sh"
neither_polarity "an unrelated exporter failure earns neither RED nor GREEN for the shrunk-draft control" a_draft_shrink "$MUT/fault-draft-unrelated.sh"
neither_polarity "an unrelated exporter failure earns neither RED nor GREEN for the prefix-export control" a_prefix_draft "$MUT/fault-draft-unrelated.sh"

# draft publication faults after the commit point and at the rename seam
a_draft_cleanup() { # holds when a backup-unlink failure after commit keeps the new draft
	mkdir -p "$ISO/docs/spikes/issue-drafts"
	find "$ISO/docs/spikes/issue-drafts" -mindepth 1 -delete
	printf 'prior draft\n' >"$ISO/docs/spikes/issue-drafts/s5-admission.md"
	rrx "$ISO" "$1" --write docs/spikes/issue-drafts -- sh -c 'printf "new draft" >/src/docs/spikes/issue-drafts/s5-admission.md; exit 1'
	_id=$(runid)
	OBS="status $RS, draft '$(cat "$ISO/docs/spikes/issue-drafts/s5-admission.md" 2>/dev/null)'"
	[ "$RS" = 1 ] && grep -Fq "dev-run: publish_cleanup_failed root=docs/spikes/issue-drafts name=.s5-admission.md.$_id.prev" "$TMP/rerr" && reached
	[ "$RS" = 1 ] && [ "$(cat "$ISO/docs/spikes/issue-drafts/s5-admission.md")" = "new draft" ] &&
		[ "$(cat "$ISO/docs/spikes/issue-drafts/.s5-admission.md.$_id.prev" 2>/dev/null)" = "prior draft" ] &&
		grep -Fq "dev-run: publish_cleanup_failed root=docs/spikes/issue-drafts name=.s5-admission.md.$_id.prev" "$TMP/rerr"
}
UNLINK_OLD='func (c *ctl) unlinkBackup(name string) error { return c.wroot.Remove(name) }'
ROLLBACK_OLD='reason = "publish_cleanup_failed"'
ROLLBACK_NEW='c.rollback()
			reason = "publish_cleanup_failed"'
gvariant fault-cleanup-draft controller.go "$UNLINK_OLD" 'func (c *ctl) unlinkBackup(name string) error { return errors.New("injected unlink failure") }'
gvariant mut-cleanup-rollback controller.go "$UNLINK_OLD" 'func (c *ctl) unlinkBackup(name string) error { return errors.New("injected unlink failure") }' "$ROLLBACK_OLD" "$ROLLBACK_NEW"
redgreen "post-commit cleanup failure treated as rollback restores the prior draft" a_draft_cleanup "$MUT/mut-cleanup-rollback.sh" "$MUT/fault-cleanup-draft.sh"
find "$ISO/docs/spikes/issue-drafts" -maxdepth 1 -name '.s5-admission.md.*.prev' -delete
a_draft_rename_after() { # holds when a failure after the rename took effect restores the prior draft byte-exactly
	mkdir -p "$ISO/docs/spikes/issue-drafts"
	find "$ISO/docs/spikes/issue-drafts" -mindepth 1 -delete
	printf 'prior draft\n' >"$ISO/docs/spikes/issue-drafts/s5-admission.md"
	_pi=$(find "$ISO/docs/spikes/issue-drafts/s5-admission.md" -printf '%i')
	rrx "$ISO" "$1" --write docs/spikes/issue-drafts -- sh -c 'printf "new draft" >/src/docs/spikes/issue-drafts/s5-admission.md; exit 1'
	_now=$(find "$ISO/docs/spikes/issue-drafts/s5-admission.md" -printf '%i' 2>/dev/null)
	OBS="status $RS, draft '$(cat "$ISO/docs/spikes/issue-drafts/s5-admission.md" 2>/dev/null)', inode $_pi -> $_now"
	[ "$RS" = 1 ] && grep -Fq 'write_publish_failed root=docs/spikes/issue-drafts' "$TMP/rerr" && reached
	[ "$RS" = 1 ] && [ "$(cat "$ISO/docs/spikes/issue-drafts/s5-admission.md")" = "prior draft" ] && [ "$_now" = "$_pi" ] &&
		grep -Fq 'write_publish_failed root=docs/spikes/issue-drafts' "$TMP/rerr" &&
		[ -z "$(find "$ISO/docs/spikes/issue-drafts" -name '.*.part' -o -name '.*.prev')" ]
}
RENAME_OLD='func (c *ctl) renameOne(src, dst string) error { return c.wroot.Rename(src, dst) }'
RENAME_AFTER='func (c *ctl) renameOne(src, dst string) error {
	if err := c.wroot.Rename(src, dst); err != nil {
		return err
	}
	return errors.New("injected failure after the rename took effect")
}'
gvariant fault-rename-after controller.go "$RENAME_OLD" "$RENAME_AFTER"
gvariant mut-rollback-removed controller.go "$RENAME_OLD" "$RENAME_AFTER" 'func (c *ctl) rollback() {' 'func (c *ctl) rollback() {
	return'
redgreen "rollback-removed mutant leaves the new draft in place" a_draft_rename_after "$MUT/mut-rollback-removed.sh" "$MUT/fault-rename-after.sh"
rm -rf "$ISO/docs/spikes/issue-drafts" "$ISO/testdata"

# ===== 4. publication on the corpus root: hard links, foreign targets, rollback (T26) ===============
mkdir -p "$ISO/testdata/parser"
printf 'prior series\n' >"$ISO/testdata/parser/series.jsonl"
printf 'prior movie\n' >"$ISO/testdata/parser/movie.jsonl"
printf 'outside bytes\n' >"$CANARY/linked.bin"
rm -f "$ISO/testdata/parser/movie.jsonl"
ln "$CANARY/linked.bin" "$ISO/testdata/parser/movie.jsonl"
BOTH='printf newS >/src/testdata/parser/series.jsonl; printf newR >/src/testdata/parser/movie.jsonl'
a_hardlink() { # holds when a hard-linked target is replaced by rename, never written in place
	rm -rf "$ISO/testdata"
	mkdir -p "$ISO/testdata/parser"
	rm -f "$CANARY/linked.bin"
	printf 'outside bytes\n' >"$CANARY/linked.bin"
	ln "$CANARY/linked.bin" "$ISO/testdata/parser/movie.jsonl"
	rrx "$ISO" "$1" --write testdata/parser -- sh -c "$BOTH"
	_links=$(find "$ISO/testdata/parser/movie.jsonl" -printf '%n' 2>/dev/null)
	_rad=$(cat "$ISO/testdata/parser/movie.jsonl" 2>/dev/null)
	_can=$(cat "$CANARY/linked.bin" 2>/dev/null)
	OBS="status $RS, movie '$_rad', links $_links, outside '$_can'"
	# the publication seam ran and its intended effect is observed from the files themselves, independently of the runner's
	# status: the new bytes sit at the destination and either the publish succeeded or the in-place write changed the outside
	# file. An intentionally faulty publication is not required to succeed, so its 125 is not a reason to call it unreached.
	{ [ "$_rad" = newR ] && { [ "$RS" = 0 ] || [ "$_can" = newR ]; }; } && reached
	[ "$RS" = 0 ] && [ "$_rad" = newR ] && [ "$_links" = 1 ] && [ "$_can" = "outside bytes" ]
}
gvariant mut-inplace-publish controller.go "$RENAME_OLD" 'func (c *ctl) renameOne(src, dst string) error {
	data, err := c.wroot.ReadFile(src)
	if err != nil {
		return err
	}
	f, err := c.wroot.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	f.Close()
	if err != nil {
		return err
	}
	return c.wroot.Remove(src)
}'
redgreen "bind/in-place publication mutant" a_hardlink "$MUT/mut-inplace-publish.sh" "$RUNNER"
printf 'prior series\n' >"$ISO/testdata/parser/series.jsonl"
rm -f "$ISO/testdata/parser/movie.jsonl"
mkdir "$ISO/testdata/parser/movie.jsonl"
rr -w write_publish_failed --write testdata/parser -- sh -c "$BOTH"
expect "an existing directory at movie.jsonl: write_publish_failed, zero renames, prior files unchanged" "$RS:$(cat "$ISO/testdata/parser/series.jsonl")" "125:prior series"
reason "directory target reason" write_publish_failed
rmdir "$ISO/testdata/parser/movie.jsonl"
ln -s "$CANARY/canary.txt" "$ISO/testdata/parser/movie.jsonl"
rr -w write_publish_failed --write testdata/parser -- sh -c "$BOTH"
expect "a symlink at movie.jsonl: write_publish_failed and the outside canary unchanged" "$RS:$(cat "$CANARY/canary.txt")" "125:outside canary"
rm -f "$ISO/testdata/parser/movie.jsonl"
expect "refusals left no owned part or backup" "$(find "$ISO/testdata/parser" -name '.*' ! -name . | wc -l | tr -d ' ')" 0

# second rename fails after the first succeeded: three prior-state shapes. The prior device:inode,
# bytes and link count of every existing name are recorded before the fault and compared afterwards.
ident_h() { find "$1" -maxdepth 0 -printf '%D:%i' 2>/dev/null; } # device:inode of a host path, empty when absent
links_h() { find "$1" -maxdepth 0 -printf '%n' 2>/dev/null; }
a_second_rename() { # a_second_rename RUNNER (SHAPE global): holds when every shape returns to its exact prior state
	_P=$ISO/testdata/parser
	rm -rf "$_P"
	mkdir -p "$_P"
	case $SHAPE in
	absent) ;;
	mixed) printf 'prior movie\n' >"$_P/movie.jsonl" ;;
	linked)
		printf 'prior series\n' >"$CANARY/s.bin"
		ln "$CANARY/s.bin" "$_P/series.jsonl"
		printf 'prior movie\n' >"$_P/movie.jsonl"
		;;
	esac
	_s0=$(ident_h "$_P/series.jsonl")
	_r0=$(ident_h "$_P/movie.jsonl")
	rrx "$ISO" "$1" --write testdata/parser -- sh -c "$BOTH"
	[ "$RS" = 125 ] && grep -Fq 'write_publish_failed root=testdata/parser' "$TMP/rerr" && reached
	_s=$(cat "$_P/series.jsonl" 2>/dev/null)
	_r=$(cat "$_P/movie.jsonl" 2>/dev/null)
	OBS="$SHAPE: status $RS series '$_s' ($(ident_h "$_P/series.jsonl") was $_s0) movie '$_r' ($(ident_h "$_P/movie.jsonl") was $_r0)"
	[ "$RS" = 125 ] || return 1
	grep -Fq 'write_publish_failed root=testdata/parser' "$TMP/rerr" || return 1
	[ -z "$(find "$_P" -name '.*.part' -o -name '.*.prev')" ] || return 1
	grep -Fq PASS "$TMP/rout" && return 1
	case $SHAPE in
	absent) [ ! -e "$_P/series.jsonl" ] && [ ! -e "$_P/movie.jsonl" ] ;;
	mixed) [ ! -e "$_P/series.jsonl" ] && [ "$_r" = "prior movie" ] && [ "$(ident_h "$_P/movie.jsonl")" = "$_r0" ] && [ "$(links_h "$_P/movie.jsonl")" = 1 ] ;;
	linked)
		[ "$_s" = "prior series" ] && [ "$(ident_h "$_P/series.jsonl")" = "$_s0" ] && [ "$(links_h "$_P/series.jsonl")" = 2 ] &&
			[ "$(cat "$CANARY/s.bin")" = "prior series" ] && [ "$_r" = "prior movie" ] && [ "$(ident_h "$_P/movie.jsonl")" = "$_r0" ] && [ "$(links_h "$_P/movie.jsonl")" = 1 ]
		;;
	esac
}
RENAME_SECOND='func (c *ctl) renameOne(src, dst string) error {
	if dst == "movie.jsonl" {
		return errors.New("injected rename failure")
	}
	return c.wroot.Rename(src, dst)
}'
gvariant fault-second-rename controller.go "$RENAME_OLD" "$RENAME_SECOND"
gvariant mut-new-inode-kept controller.go "$RENAME_OLD" "$RENAME_SECOND" ' else if err := c.wroot.Remove(n); err != nil && !errors.Is(err, fs.ErrNotExist) { // the new inode of a name that had none
				c.conflict(n)
			}' ''
for SHAPE in absent mixed; do
	redgreen "new-inode-rollback-removed mutant, shape $SHAPE" a_second_rename "$MUT/mut-new-inode-kept.sh" "$MUT/fault-second-rename.sh"
done
SHAPE=linked
holds "second rename fails with both targets existing and series hard-linked: both prior inodes (series link count 2), bytes and canary restored" a_second_rename "$MUT/fault-second-rename.sh"

# foreign replacement or deletion between the first rename and the failure: present and absent priors.
# The barrier loop is bounded and records its subshell pid.
a_foreign() { # a_foreign PRIOR RUNNER (FACT global replace|delete)
	_P=$ISO/testdata/parser
	rm -rf "$_P"
	find "$BAR" -mindepth 1 -delete
	mkdir -p "$_P"
	[ "$PRIOR" = present ] && printf 'prior series\n' >"$_P/series.jsonl"
	_s0=$(ident_h "$_P/series.jsonl")
	printf 'foreign bytes\n' >"$TMP/foreign"
	bg_run foreign "$1" --write testdata/parser -- sh -c "$BOTH"
	_i=0
	while [ ! -e "$BAR/ready" ] && [ "$_i" -lt 900 ]; do
		kill -0 "$BG_PID" 2>/dev/null || break
		sleep 1
		_i=$((_i + 1))
	done
	[ -e "$BAR/ready" ] || { OBS="barrier never reached"; SETUP=1; bg_end foreign; return 1; }
	_fo=0
	if [ "$FACT" = delete ]; then
		{ rm -f "$_P/series.jsonl" && [ ! -e "$_P/series.jsonl" ]; } || _fo=1
	else
		{ cp "$TMP/foreign" "$_P/.foreign.new" && mv -T "$_P/.foreign.new" "$_P/series.jsonl" && [ "$(cat "$_P/series.jsonl")" = "foreign bytes" ]; } || _fo=1
	fi
	: >"$BAR/go"
	bg_end foreign
	[ "$_fo" = 0 ] || { OBS="the foreign replacement or deletion was not performed"; SETUP=1; return 1; }
	_id=$(runid)
	[ "$RS" = 125 ] && grep -Fq 'write_publish_failed root=testdata/parser' "$TMP/rerr" && reached
	OBS="$FACT/$PRIOR: status $RS series '$(cat "$_P/series.jsonl" 2>/dev/null)' ($(ident_h "$_P/series.jsonl") was $_s0)"
	[ "$RS" = 125 ] || return 1
	if [ "$FACT" = delete ]; then
		grep -Fq 'publish_rollback_conflict' "$TMP/rerr" && return 1
		[ ! -e "$_P/.series.jsonl.$_id.prev" ] || return 1
		if [ "$PRIOR" = present ]; then
			[ "$(cat "$_P/series.jsonl" 2>/dev/null)" = "prior series" ] && [ "$(ident_h "$_P/series.jsonl")" = "$_s0" ]
		else
			[ ! -e "$_P/series.jsonl" ]
		fi
		return
	fi
	[ "$(cat "$_P/series.jsonl")" = "foreign bytes" ] || return 1
	grep -Fq 'dev-run: publish_rollback_conflict root=testdata/parser name=series.jsonl' "$TMP/rerr" || return 1
	if [ "$PRIOR" = present ]; then
		[ "$(cat "$_P/.series.jsonl.$_id.prev" 2>/dev/null)" = "prior series" ]
	else
		[ ! -e "$_P/.series.jsonl.$_id.prev" ]
	fi
}
FOREIGN_NEW='func (c *ctl) renameOne(src, dst string) error {
	if dst == "movie.jsonl" {
		return errors.New("injected rename failure")
	}
	if err := c.wroot.Rename(src, dst); err != nil {
		return err
	}
	if dst == "series.jsonl" {
		'"$(gobar 180)"'
	}
	return nil
}'
gvariant fault-foreign controller.go "$RENAME_OLD" "$FOREIGN_NEW"
gvariant mut-unconditional-restore controller.go "$RENAME_OLD" "$FOREIGN_NEW" 'case curOK && cur == p:' 'case true:'
FACT=replace
for PRIOR in present absent; do
	redgreen "unconditional-restore mutant, prior series $PRIOR" a_foreign "$MUT/mut-unconditional-restore.sh" "$MUT/fault-foreign.sh"
done
# the published name deleted at the barrier with its backup present: absent-current branch restores the
# prior inode by no-overwrite link and removes the backup
gvariant mut-absent-current-ignored controller.go "$RENAME_OLD" "$FOREIGN_NEW" 'case !curOK && !c.isSymlink(n):' 'case false:'
FACT=delete
for PRIOR in present absent; do
	redgreen "absent-current-branch-ignored mutant, prior series $PRIOR" a_foreign "$MUT/mut-absent-current-ignored.sh" "$MUT/fault-foreign.sh"
done
FACT=replace

# post-commit: first backup unlink succeeds, second fails; no rollback
a_post_commit() { # holds when a post-commit cleanup failure keeps both new names, the first backup gone and the second kept
	_P=$ISO/testdata/parser
	rm -rf "$_P"
	mkdir -p "$_P"
	printf 'prior series\n' >"$_P/series.jsonl"
	printf 'prior movie\n' >"$_P/movie.jsonl"
	rrx "$ISO" "$1" --write testdata/parser -- sh -c "$BOTH"
	_id=$(runid)
	[ "$RS" = 125 ] && grep -Fq "dev-run: publish_cleanup_failed root=testdata/parser name=.movie.jsonl.$_id.prev" "$TMP/rerr" && reached
	OBS="status $RS series '$(cat "$_P/series.jsonl" 2>/dev/null)' movie '$(cat "$_P/movie.jsonl" 2>/dev/null)', first backup $([ -e "$_P/.series.jsonl.$_id.prev" ] && echo kept || echo gone)"
	[ "$RS" = 125 ] && [ "$(cat "$_P/series.jsonl")" = newS ] && [ "$(cat "$_P/movie.jsonl")" = newR ] &&
		[ ! -e "$_P/.series.jsonl.$_id.prev" ] &&
		[ "$(cat "$_P/.movie.jsonl.$_id.prev" 2>/dev/null)" = "prior movie" ] &&
		[ -z "$(find "$_P" -name '.*.part')" ] &&
		grep -Fq "dev-run: publish_cleanup_failed root=testdata/parser name=.movie.jsonl.$_id.prev" "$TMP/rerr"
}
SECOND_UNLINK='func (c *ctl) unlinkBackup(name string) error {
	if strings.HasPrefix(name, ".movie.jsonl.") {
		return errors.New("injected unlink failure")
	}
	return c.wroot.Remove(name)
}'
gvariant fault-second-unlink controller.go "$UNLINK_OLD" "$SECOND_UNLINK"
gvariant mut-cleanup-as-rollback controller.go "$UNLINK_OLD" "$SECOND_UNLINK" "$ROLLBACK_OLD" "$ROLLBACK_NEW"
redgreen "mutant treating post-commit cleanup failure as rollback" a_post_commit "$MUT/mut-cleanup-as-rollback.sh" "$MUT/fault-second-unlink.sh"

# backups unlinked before the commit re-check lose the prior file under an identity fault
a_commit_order() { # holds when an identity fault after the renames restores the prior series.jsonl
	rm -rf "$ISO/testdata/parser"
	mkdir -p "$ISO/testdata/parser"
	printf 'prior series\n' >"$ISO/testdata/parser/series.jsonl"
	rrx "$ISO" "$1" --write testdata/parser -- sh -c 'printf newS >/src/testdata/parser/series.jsonl'
	[ "$RS" = 125 ] && grep -Fq 'dev-run: write_root_changed' "$TMP/rerr" && reached
	OBS="status $RS series '$(cat "$ISO/testdata/parser/series.jsonl" 2>/dev/null)'"
	[ "$RS" = 125 ] && [ "$(cat "$ISO/testdata/parser/series.jsonl" 2>/dev/null)" = "prior series" ]
}
IDFAULT_OLD='if !c.rootUnchanged() { // the identity re-check after the renames'
IDFAULT_NEW='if true || !c.rootUnchanged() { // the identity re-check after the renames'
gvariant fault-identity controller.go "$IDFAULT_OLD" "$IDFAULT_NEW"
gvariant mut-unlink-before-commit controller.go "$IDFAULT_OLD" "$IDFAULT_NEW" "$IDFAULT_NEW" 'for _, n := range c.staged {
		_ = c.wroot.Remove("." + n + "." + c.runid + ".prev")
	}
	if true || !c.rootUnchanged() { // the identity re-check after the renames'
redgreen "backup-unlinked-before-commit mutant loses the prior file" a_commit_order "$MUT/mut-unlink-before-commit.sh" "$MUT/fault-identity.sh"

# a part whose identity cannot be held authorises no rename: no renames at all, prior bytes and inodes preserved, nothing owned left behind
a_ident_fault() { # a_ident_fault RUNNER
	_P=$ISO/testdata/parser
	rm -rf "$_P"
	mkdir -p "$_P"
	printf 'prior series\n' >"$_P/series.jsonl"
	printf 'prior movie\n' >"$_P/movie.jsonl"
	_s0=$(ident_h "$_P/series.jsonl")
	_r0=$(ident_h "$_P/movie.jsonl")
	rrx "$ISO" "$1" --write testdata/parser -- sh -c "$BOTH"
	{ { [ "$RS" = 125 ] && grep -Fq 'write_publish_failed root=testdata/parser' "$TMP/rerr"; } || { [ "$RS" = 0 ] && [ "$(cat "$_P/series.jsonl" 2>/dev/null)" = newS ]; }; } && reached
	OBS="status $RS series '$(cat "$_P/series.jsonl" 2>/dev/null)' ($(ident_h "$_P/series.jsonl") was $_s0) movie '$(cat "$_P/movie.jsonl" 2>/dev/null)' ($(ident_h "$_P/movie.jsonl") was $_r0)"
	[ "$RS" = 125 ] && grep -Fq 'write_publish_failed root=testdata/parser' "$TMP/rerr" &&
		[ "$(cat "$_P/series.jsonl")" = "prior series" ] && [ "$(ident_h "$_P/series.jsonl")" = "$_s0" ] &&
		[ "$(cat "$_P/movie.jsonl")" = "prior movie" ] && [ "$(ident_h "$_P/movie.jsonl")" = "$_r0" ] &&
		[ -z "$(find "$_P" -name '.*.part' -o -name '.*.prev')" ]
}
IDENT_OLD='func (c *ctl) partIdent(part string) (fileID, bool) {
	return c.identOf(part)
}'
IDENT_NEW='func (c *ctl) partIdent(part string) (fileID, bool) {
	if strings.HasPrefix(part, ".series.jsonl.") {
		return fileID{}, false
	}
	return c.identOf(part)
}'
gvariant fault-ident-part controller.go "$IDENT_OLD" "$IDENT_NEW"
gvariant mut-ident-unchecked controller.go "$IDENT_OLD" "$IDENT_NEW" 'if !ok { // an unheld part identity authorises no rename
			return c.failPub("write_publish_failed")
		}' '_ = ok'
redgreen "unheld part identity (identity-unchecked mutant renames anyway)" a_ident_fault "$MUT/mut-ident-unchecked.sh" "$MUT/fault-ident-part.sh"

# canary_state (names and content hashes of everything outside the checkout) is defined with the live-map controls above
# Distinct transitions after the write root was acquired, with the child held at the in-container barrier;
# the child then writes the real map name. MODE: leaf (parser replaced by a canary symlink), ancestor-link
# (testdata replaced by a canary symlink), ancestor-dir (testdata moved away and a fresh real tree created).
a_root_transition() { # a_root_transition MODE RUNNER
	rm -rf "$ISO/testdata" "$ISO/testdata.moved"
	mkdir -p "$ISO/testdata/parser"
	printf 'c\n' >"$ISO/testdata/parser/keep"
	_h0=$(fhash "$ISO/testdata/parser/keep")
	bg_run swap "$2" --write testdata/parser -- sh -c "$CBAR; printf late >/src/testdata/parser/series.jsonl"
	_c=$(bg_ctr swap) || { OBS="no container"; SETUP=1; return 1; }
	bg_wait "$_c" /tmp/ready || { OBS="not ready"; SETUP=1; kill -KILL "$BG_PID" 2>/dev/null; bg_end swap; return 1; }
	_cs0=$(canary_state)
	case $1 in
	leaf)
		mv "$ISO/testdata/parser" "$ISO/testdata/parser.moved"
		ln -s "$CANARY" "$ISO/testdata/parser"
		_mv=$ISO/testdata/parser.moved
		;;
	ancestor-link)
		mv "$ISO/testdata" "$ISO/testdata.moved"
		ln -s "$CANARY" "$ISO/testdata"
		_mv=$ISO/testdata.moved/parser
		;;
	ancestor-dir)
		mv "$ISO/testdata" "$ISO/testdata.moved"
		mkdir -p "$ISO/testdata/parser"
		_mv=$ISO/testdata.moved/parser
		;;
	esac
	bg_touch "$_c" /tmp/go
	bg_end swap
	_new=$([ -e "$ISO/testdata/parser/series.jsonl" ] && echo present || echo absent)
	OBS="$1: status $RS, replacement series $_new, moved keep '$(cat "$_mv/keep" 2>/dev/null)'"
	[ "$RS" = 125 ] && grep -Fq 'dev-run: write_root_changed' "$TMP/rerr" && reached
	_ok=0
	[ "$RS" = 125 ] && grep -Fq 'dev-run: write_root_changed' "$TMP/rerr" && [ "$_cs0" = "$(canary_state)" ] && [ "$_new" = absent ] &&
		[ "$(find "$_mv" -mindepth 1 | LC_ALL=C sort | tr '\n' ' ')" = "$_mv/keep " ] && [ "$(fhash "$_mv/keep")" = "$_h0" ] && _ok=1
	rm -rf "$ISO/testdata" "$ISO/testdata.moved"
	[ "$_ok" = 1 ]
}
for MODE in leaf ancestor-link ancestor-dir; do
	holds "write root $MODE replaced while the child is held, then the child writes series.jsonl: write_root_changed, canary and moved object bytes and entries unchanged, replacement unwritten" a_root_transition "$MODE" "$RUNNER"
done
# Transitions during acquisition itself (validation and creation), held at a compiled barrier before the write-root OpenRoot: the
# controller refuses a swapped leaf or ancestor with write_root_symlink before any child runs; the acquisition checks removed (mutant) reach the child.
# The pinned-root open and its identity check are not the only refusal a swapped root meets: the image copies the swapped symlink, so the
# supervisor's own map preflight (writePreflight) would refuse the run before any child existed and mask the removed check.
# The mutant therefore pins the repository root instead of the leaf, drops the acquisition identity check and that masking preflight together, so the only remaining refusals
# come after the child ran; the original refuses at acquisition with write_root_symlink and no child.
ROOTHOLD_OLD='wroot, err := c.repoRoot.OpenRoot(c.write)'
ROOTHOLD_NEW="$(gobar 120)
	wroot, err := c.repoRoot.OpenRoot(c.write)"
a_acq_transition() { # a_acq_transition RUNNER (ACQ global cold-leaf|cold-ancestor|warm-leaf): the earliest accepted boundary is the acquisition itself. Every Docker call of the bootstrap precedes the barrier;
	# whatever the controller itself calls after the swap is native work attempted past the acquisition (the shim records the S line of each call before it execs the real CLI), so the original must make none and the mutant
	# must positively show its first native call, the work-image build over the swapped tree, whatever a downstream Docker safety check later says.
	rm -rf "$ISO/testdata" "$ISO/testdata.moved" "$ISO/testdata/parser.moved"
	mkdir -p "$CANARY/parser"
	[ "$ACQ" != warm-leaf ] || { mkdir -p "$ISO/testdata/parser"; printf 'c\n' >"$ISO/testdata/parser/keep"; }
	find "$BAR" -mindepth 1 -delete
	_cs0=$(canary_state)
	bg_runlog acq "$1" --write testdata/parser -- sh -c 'printf ran'
	_i=0
	while [ ! -e "$BAR/ready" ] && kill -0 "$BG_PID" 2>/dev/null && [ "$_i" -lt 300 ]; do sleep 1; _i=$((_i + 1)); done
	[ -e "$BAR/ready" ] || { OBS="acquisition barrier never reached"; SETUP=1; bg_end acq; rmdir "$CANARY/parser" 2>/dev/null; return 1; }
	_pre=$(grep -c '^S ' "$TMP/acq.dlog" 2>/dev/null)
	_fo=0
	case $ACQ in
	cold-leaf | warm-leaf)
		{ mv "$ISO/testdata/parser" "$ISO/testdata/parser.moved" && ln -s "$CANARY" "$ISO/testdata/parser"; } || _fo=1
		_mv=$ISO/testdata/parser.moved
		;;
	cold-ancestor)
		{ mv "$ISO/testdata" "$ISO/testdata.moved" && ln -s "$CANARY" "$ISO/testdata"; } || _fo=1
		_mv=$ISO/testdata.moved
		;;
	esac
	: >"$BAR/go"
	bg_end acq
	dlogs_same acq || { rmdir "$CANARY/parser" 2>/dev/null; return 1; }
	[ "$_fo" = 0 ] || { OBS="the acquisition swap was not performed"; SETUP=1; rmdir "$CANARY/parser" 2>/dev/null; return 1; }
	_post=$(grep -c '^S ' "$TMP/acq.dlog" 2>/dev/null)
	_new=$((${_post:-0} - ${_pre:-0}))
	_first=$(grep '^S ' "$TMP/acq.dlog" 2>/dev/null | sed -n "$((${_pre:-0} + 1))p")
	case $_first in *" build "*"--target work"*) _bw=1 ;; *) _bw=0 ;; esac
	OBS="$ACQ: status $RS, child output '$(cat "$TMP/rout")', runner words '$(grep -o 'dev-run: [a-z_]*' "$TMP/rerr" | tr '\n' ' ')', native Docker calls after the acquisition barrier $_new (first: ${_first:-none}), moved entries '$(find "$_mv" -mindepth 1 | LC_ALL=C sort | tr '\n' ' ')'"
	{ { [ "$RS" = 125 ] && grep -Fq 'dev-run: write_root_symlink' "$TMP/rerr" && [ "$_new" = 0 ]; } || { [ "$_new" -ge 1 ] && [ "$_bw" = 1 ]; }; } && reached
	_ok=0
	[ "$RS" = 125 ] && grep -Fq 'dev-run: write_root_symlink' "$TMP/rerr" && [ "$_new" = 0 ] && [ -z "$(cat "$TMP/rout")" ] && [ "$_cs0" = "$(canary_state)" ] &&
		if [ "$ACQ" = warm-leaf ]; then [ "$(find "$_mv" -mindepth 1 | tr '\n' ' ')" = "$_mv/keep " ] && [ "$(cat "$_mv/keep")" = c ]; else [ -z "$(find "$_mv" -mindepth 1 -not -path "$_mv/parser" | head -n 1)" ]; fi && _ok=1
	rm -rf "$ISO/testdata" "$ISO/testdata.moved" "$CANARY/parser"
	[ "$_ok" = 1 ]
}
gvariant fault-root-hold controller.go "$ROOTHOLD_OLD" "$ROOTHOLD_NEW"
gvariant mut-no-phys-check controller.go "$ROOTHOLD_OLD" "$ROOTHOLD_NEW" "$ROOTHOLD_OLD" 'wroot, err := c.repoRoot.OpenRoot(".")' 'if r := c.writeRootCheck(); r != "" {' 'if r := ""; r != "" {' '!c.writePreflight()' 'false'
for ACQ in cold-leaf cold-ancestor warm-leaf; do
	redgreen "acquisition-transition $ACQ (physical-path check removed mutant reaches the child)" a_acq_transition "$MUT/mut-no-phys-check.sh" "$MUT/fault-root-hold.sh"
done

# ===== 5. quiescence: detached writers, zombies, non-dumpable, threads, verifier faults (T26) ===============
# Readiness is deterministic. The child records the pid under test and either holds at the in-container
# barrier (non-dumpable, thread leader) or the runner is held at the export seam, which runs after
# quiescence and the audit but before any byte is exported (detached writer, zombie). The driver reads
# every task state of that pid as the 65534 supervisor and asserts the state each control claims.
mkdir -p "$ISO/testdata/parser"
task_states() { # task_states CTR PID: the State letter of every task of PID, one per line, or gone; a failed query fails (the one-shot control file makes the first query fail, to prove that a failure followed by success is still refused)
	if [ -e "$TMP/ts.failonce" ]; then
		rm -f "$TMP/ts.failonce"
		return 1
	fi
	timeout 30 docker exec -u 65534:65534 "$1" /usr/bin/sh -c 'if [ ! -d /proc/$0 ]; then echo gone; exit 0; fi; for t in /proc/$0/task/*; do while IFS= read -r l; do case $l in State:*) set -- $l; echo "$2" ;; esac; done <"$t/status"; done' "$2" 2>/dev/null
}
EXPORTHOLD_OLD='func (c *ctl) exportPart(name string, size int64) bool {'
EXPORTHOLD_NEW="$EXPORTHOLD_OLD
	$(gobar 120)"
live_tasks() { printf '%s\n' "$1" | tr ' ' '\n' | grep -v '^$' | grep -Evc '^(Z|gone)$'; } # live_tasks "S Z ...": tasks neither zombie nor gone
# a_export_hold NAME RUNNER SCRIPT PIDFILE: runs SCRIPT through the held runner; sets AH_STATES (the task states of the
# pid in /tmp/PIDFILE, read by a status-checked supervisor exec at the hold; with AH_WAIT set, polled until exactly
# that state or 20 reads), AH_PUB (published series.jsonl) and RS. The runner is released only after the read.
AH_WAIT=
a_export_hold() {
	rm -f "$ISO/testdata/parser/series.jsonl"
	find "$BAR" -mindepth 1 -delete
	bg_run "$1" "$2" --write testdata/parser -- sh -c "$3"
	AH_STATES=
	AH_C=$(bg_ctr "$1") || { AH_C=; }
	_i=0
	while [ ! -e "$BAR/ready" ] && kill -0 "$BG_PID" 2>/dev/null && [ "$_i" -lt 300 ]; do
		sleep 1
		_i=$((_i + 1))
	done
	if [ -e "$BAR/ready" ] && [ -n "$AH_C" ]; then
		_wp=$(cx "$AH_C" /usr/bin/cat "/tmp/$4" 2>/dev/null) || { _wp=; unknown_obs || SETUP=1; }
		case $_wp in '' | *[!0-9]*) _wp= ;; esac
		_k=0
		while [ -n "$_wp" ] && [ "$_k" -lt 20 ]; do
			if _as=$(task_states "$AH_C" "$_wp"); then
				AH_STATES=$(printf '%s' "$_as" | tr '\n' ' ')
				[ -n "$AH_WAIT" ] || break
				[ "$(printf '%s' "$AH_STATES" | tr -d ' ')" != "$AH_WAIT" ] || break
			else
				AH_STATES=
				unknown_obs || SETUP=1
			fi
			sleep 1
			_k=$((_k + 1))
		done
		cx "$AH_C" /usr/bin/touch /tmp/go >/dev/null 2>&1 || SETUP=1
		true >"$BAR/go" || SETUP=1
	fi
	bg_end "$1"
	AH_PUB=$(cat "$ISO/testdata/parser/series.jsonl" 2>/dev/null)
}
DETACH='printf early >/src/testdata/parser/series.jsonl
setsid sh -c "echo \$\$ >/tmp/writer.pid; while [ ! -e /tmp/go ]; do /usr/bin/sleep 0.1; done; printf LATE >>/src/testdata/parser/series.jsonl; while :; do /usr/bin/sleep 1; done" >/dev/null 2>&1 &
n=0; while [ ! -s /tmp/writer.pid ] && [ "$n" -lt 100 ]; do /usr/bin/sleep 0.1; n=$((n + 1)); done
[ -s /tmp/writer.pid ] || exit 91'
a_detached() { # holds when no writer task is live at the pre-export boundary and the published bytes are exactly the child's
	a_export_hold det "$1" "$DETACH" writer.pid
	OBS="status $RS, writer task states at the export seam '${AH_STATES:-seam not reached}', published '$AH_PUB'"
	[ -n "$AH_STATES" ] && { [ "$(live_tasks "$AH_STATES")" -ge 1 ] || { [ "$RS" = 0 ] && [ "$AH_PUB" = early ]; }; } && reached
	[ -n "$AH_STATES" ] && [ "$(live_tasks "$AH_STATES")" = 0 ] && [ "$RS" = 0 ] && [ "$AH_PUB" = early ]
}
gvariant fault-export-hold controller.go "$EXPORTHOLD_OLD" "$EXPORTHOLD_NEW"
gvariant mut-no-quiesce controller.go "$EXPORTHOLD_OLD" "$EXPORTHOLD_NEW" 'func (c *ctl) quiesce() bool {' 'func (c *ctl) quiesce() bool {
	return true'
redgreen "quiescence-removed mutant (a live detached writer at the pre-export seam)" a_detached "$MUT/mut-no-quiesce.sh" "$MUT/fault-export-hold.sh"
# A control on the real callback: one task-state query fails and the next succeeds with the intended premise; the assertion
# then holds, yet the classifier must still refuse the case for the recorded unknown observation. This is a classifier
# meta-control (META), the only plain credit that bypasses the gate, and it is credited only when every other part is clean.
a_detached_failonce() { # a_detached_failonce RUNNER
	true >"$TMP/ts.failonce" || { OBS="the one-shot failure could not be armed"; SETUP=1; return 1; }
	a_detached "$1"
}
refuses_unknown() { # refuses_unknown NAME FN ARGS...
	_ruN=$1
	_ruF=$2
	shift 2
	rg_reset
	_ruC=0
	"$_ruF" "$@" || _ruC=$?
	_ruK=0
	rg_verdict green "$_ruC" || _ruK=1
	META=1
	if [ "$_ruK" = 1 ] && [ -s "$TMP/unknown" ] && [ "$REACHED" = 1 ] && [ "$SETUP" = 0 ] && [ "$_ruC" = 0 ]; then
		pass "$_ruN"
	else
		fail "$_ruN" "assertion status $_ruC, reached $REACHED, setup $SETUP, unknown record $([ -s "$TMP/unknown" ] && echo set || echo clear), refused $_ruK: $OBS"
	fi
	META=0
	rm -f "$TMP/ts.failonce"
}
refuses_unknown "a failed task-state query followed by a successful one is still an unknown observation: the detached-writer case on the held original is refused" a_detached_failonce "$MUT/fault-export-hold.sh"

ZOMBIE='/usr/bin/sleep 30 &
echo $! >/tmp/sleeper.pid
printf zombie >/src/testdata/parser/series.jsonl'
# The zombie premise is read while the runner is held after its kill and before its verifier, so the verdict of
# the rejecting target cannot hide the premise: the held copy changes only the quiescence seam, the mutant is the
# same copy with the verifier rejecting every UID-1000 entry.
QUIESCE_OLD='v := c.do(&callSpec{kind: kOther, argv: c.sup(10, "/usr/bin/sh", "-c", verifyScript, "sh"), bound: supBound(10)})'
QUIESCE_HOLD="$(gobar 120)
		$QUIESCE_OLD"
a_zombie() { # holds when the killed UID-1000 sleeper is a terminal zombie before the verifier and does not stop publication
	AH_WAIT=Z
	a_export_hold zom "$1" "$ZOMBIE" sleeper.pid
	AH_WAIT=
	OBS="status $RS, sleeper task states before the verifier '${AH_STATES:-seam not reached}', published '$AH_PUB', $(grep -o 'dev-run: [a-z_]*' "$TMP/rerr" | tail -n 1)"
	[ "$(printf '%s' "$AH_STATES" | tr -d ' ')" = Z ] && { { [ "$RS" = 0 ] && [ "$AH_PUB" = zombie ]; } || { [ "$RS" = 125 ] && grep -Fq 'dev-run: quiesce_failed' "$TMP/rerr"; }; } && reached
	[ "$(printf '%s' "$AH_STATES" | tr -d ' ')" = Z ] && [ "$RS" = 0 ] && [ "$AH_PUB" = zombie ]
}
gvariant fault-quiesce-hold controller.go "$QUIESCE_OLD" "$QUIESCE_HOLD"
gvariant mut-reject-every-uid1000 controller.go "$QUIESCE_OLD" "$QUIESCE_HOLD" '[ "$state" = Z ] || bad=1' 'bad=1'
redgreen "verifier mutant rejecting every UID-1000 entry (a zombie sleeper)" a_zombie "$MUT/mut-reject-every-uid1000.sh" "$MUT/fault-quiesce-hold.sh"

KILL_OLD='"-c", "kill -KILL -1"}'
KILL_NEW='"-c", "true"}'
NONDUMP='cat >/tmp/nd.c <<"CSRC"
#include <fcntl.h>
#include <stdio.h>
#include <sys/prctl.h>
#include <unistd.h>
int main(void) {
	char b[32];
	int fd, n;
	if (prctl(PR_SET_DUMPABLE, 0, 0, 0, 0) != 0 || prctl(PR_GET_DUMPABLE, 0, 0, 0, 0) != 0) return 2;
	n = snprintf(b, sizeof b, "%d\n", (int)getpid());
	fd = open("/tmp/ns.pid.tmp", O_WRONLY | O_CREAT | O_TRUNC, 0644);
	if (fd < 0 || write(fd, b, n) != n || close(fd) != 0 || rename("/tmp/ns.pid.tmp", "/tmp/ns.pid") != 0) return 3;
	for (;;) sleep(1);
}
CSRC
gcc -o /tmp/nd /tmp/nd.c || exit 92
setsid /tmp/nd >/dev/null 2>&1 &
n=0; while [ ! -s /tmp/ns.pid ] && [ "$n" -lt 100 ]; do /usr/bin/sleep 0.1; n=$((n + 1)); done
[ -s /tmp/ns.pid ] || exit 93
printf x >/src/testdata/parser/series.jsonl
'"$CBAR"
a_nondumpable() { # holds when a live UID-1000 process that set PR_SET_DUMPABLE 0 natively is caught as live, judged from its Uid line
	rm -f "$ISO/testdata/parser/series.jsonl"
	bg_run nd "$1" --write testdata/parser -- sh -c "$NONDUMP"
	_c=$(bg_ctr nd) || { OBS="no container"; SETUP=1; return 1; }
	if ! bg_wait "$_c" /tmp/ready; then OBS="child never ready (compile or readiness failure is setup)"; SETUP=1; kill -KILL "$BG_PID" 2>/dev/null; bg_end nd; return 1; fi
	_np=$(cx "$_c" /usr/bin/cat /tmp/ns.pid 2>/dev/null) || _np=
	case $_np in
	'' | *[!0-9]*)
		OBS="no usable pid recorded by the native process"
		SETUP=1
		bg_touch "$_c" /tmp/go
		bg_end nd
		return 1
		;;
	esac
	# every premise is read from the exact task status file the verifier reads, while the child is held and
	# before the runner is released into its rejecting target
	_tf=/proc/$_np/task/$_np/status
	_states=$(task_states "$_c" "$_np") || { _states=; unknown_obs || SETUP=1; }
	_states=$(printf '%s' "$_states" | tr '\n' ' ')
	_own=$(cx "$_c" /usr/bin/stat -c %u "$_tf" 2>/dev/null) || { _own=; unknown_obs || SETUP=1; }
	_uid=$(cx "$_c" /usr/bin/sh -c 'while IFS= read -r l; do case $l in Uid:*) set -- $l; echo "$2 $3" ;; esac; done <"$0"' "$_tf" 2>/dev/null) || { _uid=; unknown_obs || SETUP=1; }
	_rel=0
	bg_touch "$_c" /tmp/go && _rel=1
	bg_end nd
	OBS="status $RS, task states '$_states', live Uid '$_uid', $_tf owner '$_own', $(grep -o 'dev-run: [a-z_]*' "$TMP/rerr" | tail -n 1)"
	# the intended authority condition is positively established before the verdict: a live UID-1000 task whose
	# task status file is root-owned while its Uid line reads 1000 1000; anything else is setup, never target evidence
	if [ "$_rel" != 1 ] || ! { [ "$_own" = 0 ] && [ "$_uid" = "1000 1000" ] && [ "$(live_tasks "$_states")" -ge 1 ]; }; then
		OBS="$OBS; fixture premise (live UID 1000 task, non-dumpable, root-owned task status file with a 1000 1000 Uid line) not established"
		SETUP=1
		return 1
	fi
	{ { [ "$RS" = 125 ] && grep -Fq 'dev-run: quiesce_failed' "$TMP/rerr"; } || { [ "$RS" = 0 ] && [ -e "$ISO/testdata/parser/series.jsonl" ]; }; } && reached
	[ "$RS" = 125 ] && grep -Fq 'dev-run: quiesce_failed' "$TMP/rerr"
}
gvariant fault-kill-removed controller.go "$KILL_OLD" "$KILL_NEW"
gvariant mut-dir-owner-verifier controller.go "$KILL_OLD" "$KILL_NEW" 'Uid:*) set -- $line; uids="$2 $3" ;;' 'Uid:*) uids="$(/usr/bin/stat -c %u "$t/status") 0" ;;'
redgreen "proc-file-owner verifier (the owner of the exact task status file it reads, not its Uid line) misses the non-dumpable live process" a_nondumpable "$MUT/mut-dir-owner-verifier.sh" "$MUT/fault-kill-removed.sh"

THREAD='cat >/tmp/th.c <<"CSRC"
#include <pthread.h>
#include <fcntl.h>
#include <unistd.h>
static void *w(void *a) { int fd = open("/src/testdata/parser/series.jsonl", O_WRONLY | O_APPEND); for (;;) { if (fd >= 0) (void)write(fd, "t", 1); usleep(1000); } return a; }
int main(void) { pthread_t t; pthread_create(&t, 0, w, 0); pthread_exit(0); }
CSRC
gcc -pthread -o /tmp/th /tmp/th.c || exit 42
printf lead >/src/testdata/parser/series.jsonl
setsid /tmp/th >/dev/null 2>&1 &
echo $! >/tmp/th.pid
sleep 1
'"$CBAR"
a_thread_leader() { # holds when a live second thread under a zombie leader is observed and caught
	rm -f "$ISO/testdata/parser/series.jsonl"
	bg_run thr "$1" --write testdata/parser -- sh -c "$THREAD"
	_c=$(bg_ctr thr) || { OBS="no container"; SETUP=1; return 1; }
	if ! bg_wait "$_c" /tmp/ready; then OBS="child never ready (compile failure fails the case)"; SETUP=1; kill -KILL "$BG_PID" 2>/dev/null; bg_end thr; return 1; fi
	_tp=$(cx "$_c" /usr/bin/cat /tmp/th.pid 2>/dev/null) || _tp=
	case $_tp in
	'' | *[!0-9]*)
		OBS="no usable pid recorded by the native thread process"
		SETUP=1
		bg_touch "$_c" /tmp/go
		bg_end thr
		return 1
		;;
	esac
	# the premise (a zombie leader with a live second thread, real and effective UID 1000) is read while the
	# child is held, before the runner is released into its rejecting target
	_k=0
	_states=
	_prem=0
	while [ "$_k" -lt 20 ]; do
		if _as=$(task_states "$_c" "$_tp"); then
			_states=$(printf '%s' "$_as" | tr '\n' ' ')
			if [ "$(printf '%s' "$_states" | tr ' ' '\n' | grep -c '^Z$')" -ge 1 ] && [ "$(live_tasks "$_states")" -ge 1 ]; then
				_prem=1
				break
			fi
		else
			unknown_obs || SETUP=1
		fi
		sleep 1
		_k=$((_k + 1))
	done
	_uid=$(cx "$_c" /usr/bin/sh -c 'while IFS= read -r l; do case $l in Uid:*) set -- $l; echo "$2 $3" ;; esac; done <"/proc/$0/status"' "$_tp" 2>/dev/null) || { _uid=; unknown_obs || SETUP=1; }
	_rel=0
	bg_touch "$_c" /tmp/go && _rel=1
	bg_end thr
	OBS="status $RS, thread task states '$_states', live Uid '$_uid', $(grep -o 'dev-run: [a-z_]*' "$TMP/rerr" | tail -n 1)"
	if [ "$_prem" != 1 ] || [ "$_rel" != 1 ] || [ "$_uid" != "1000 1000" ]; then
		OBS="$OBS; premise (zombie leader with a live second thread, real and effective UID 1000) not established"
		SETUP=1
		return 1
	fi
	{ { [ "$RS" = 125 ] && grep -Fq 'dev-run: quiesce_failed' "$TMP/rerr"; } || [ "$RS" = 0 ] || grep -Fq 'dev-run: write_audit_failed' "$TMP/rerr"; } && reached
	[ "$RS" = 125 ] && grep -Fq 'dev-run: quiesce_failed' "$TMP/rerr"
}
gvariant mut-status-only-verifier controller.go "$KILL_OLD" "$KILL_NEW" 'for t in "$p"/task/[0-9]*; do' 'for t in "$p"; do'
redgreen "verifier reading only /proc/<pid>/status accepts the zombie leader (a live second thread)" a_thread_leader "$MUT/mut-status-only-verifier.sh" "$MUT/fault-kill-removed.sh"

a_verifier_fault() { # holds when a status that cannot be parsed or read rejects: quiesce_failed, no publication
	rm -f "$ISO/testdata/parser/series.jsonl"
	rrx "$ISO" "$1" --write testdata/parser -- sh -c 'printf x >/src/testdata/parser/series.jsonl'
	OBS="status $RS, $(grep -o 'dev-run: [a-z_]*' "$TMP/rerr" | tail -n 1), published $([ -e "$ISO/testdata/parser/series.jsonl" ] && echo yes || echo no)"
	{ { [ "$RS" = 125 ] && grep -Fq 'dev-run: quiesce_failed' "$TMP/rerr"; } || { [ "$RS" = 0 ] && [ -e "$ISO/testdata/parser/series.jsonl" ]; }; } && reached
	[ "$RS" = 125 ] && grep -Fq 'dev-run: quiesce_failed' "$TMP/rerr" && [ ! -e "$ISO/testdata/parser/series.jsonl" ]
}
gvariant fault-verifier-nostate controller.go 'State:*) set -- $line; state=$2 ;;' 'State:*) ;;'
gvariant mut-verifier-accepts controller.go 'State:*) set -- $line; state=$2 ;;' 'State:*) ;;' 'if [ -z "$state" ] || [ -z "$uids" ]; then' 'if false; then'
redgreen "verifier accepting a status with no State: line" a_verifier_fault "$MUT/mut-verifier-accepts.sh" "$MUT/fault-verifier-nostate.sh"
gvariant fault-verifier-unreadable controller.go 'done <"$t/status"' 'done <"$t/status.unreadable"'
gvariant mut-verifier-unreadable-as-gone controller.go 'done <"$t/status"' 'done <"$t/status.unreadable"' '[ -e "$t" ] && bad=1' ':'
redgreen "verifier treating an unreadable task status as a vanished task" a_verifier_fault "$MUT/mut-verifier-unreadable-as-gone.sh" "$MUT/fault-verifier-unreadable.sh"
rm -rf "$ISO/testdata"

# inputs exec fault and owned rm fault
a_inputs() { # holds when an inputs exec failure stops the run before the child
	rrx "$ISO" "$1" -- sh -c 'touch /tmp/ran; printf ran'
	OBS="status $RS, child output '$(cat "$TMP/rout")'"
	{ { [ "$RS" = 125 ] && grep -Fq 'dev-run: inputs_unavailable' "$TMP/rerr"; } || [ "$(cat "$TMP/rout")" = ran ]; } && reached
	[ "$RS" = 125 ] && grep -Fq 'dev-run: inputs_unavailable' "$TMP/rerr" && [ -z "$(cat "$TMP/rout")" ]
}
INPUTS_OLD='argv: c.sup(10, "/usr/bin/cat", "/usr/local/share/boxset/inputs.sha256"),'
INPUTS_NEW='argv: c.sup(10, "/usr/bin/sh", "-c", `printf "%064d  /src/x\n" 0; exit 1`),'
gvariant fault-inputs controller.go "$INPUTS_OLD" "$INPUTS_NEW"
gvariant mut-inputs-status controller.go "$INPUTS_OLD" "$INPUTS_NEW" 'if r.startErr != nil || r.status != 0 || !r.outOK || !ir.ok {' 'if r.startErr != nil || !r.outOK || !ir.ok {'
redgreen "status-ignoring inputs mutant" a_inputs "$MUT/mut-inputs-status.sh" "$MUT/fault-inputs.sh"
a_rm_fault() { # holds when a failed owned removal is cleanup_failed with the owned name
	rrx "$ISO" "$1" -- true
	_cn=$(cname)
	cstate "$_cn"
	OBS="status $RS, container $_cn $CST"
	[ -n "$_cn" ] || { SETUP=1; return 1; }
	{ { [ "$RS" = 125 ] && grep -Fq "dev-run: cleanup_failed container=$_cn" "$TMP/rerr"; } || [ "$RS" = 0 ]; } && reached
	_ok=1
	[ "$RS" = 125 ] && grep -Fq "dev-run: cleanup_failed container=$_cn" "$TMP/rerr" || _ok=0
	docker rm -f "$_cn" >/dev/null 2>&1
	[ "$_ok" = 1 ]
}
gvariant fault-rm controller.go 'return []string{"timeout", "30", "docker", "rm", "-f", name}' 'return []string{"/bin/sh", "-c", "exit 1"}'
gvariant mut-cleanup-ignored controller.go 'return []string{"timeout", "30", "docker", "rm", "-f", name}' 'return []string{"/bin/sh", "-c", "exit 1"}' 'c.bad("cleanup_failed container=" + n)' '_ = n'
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
# a plain control whose bytes are right but whose final runner status is not: the compiled copy fails every owned removal, so the run ends cleanup_failed (125) after the correct redacted output. Through
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
rr -- sh -c 'head -c 65536 /dev/zero | tr "\0" a'
expect "exactly 64 KiB raw passes" "$RS:$(wc -c <"$TMP/rout" | tr -d ' ')" "0:65536"
rr -w output_overflow -- sh -c 'head -c 65537 /dev/zero | tr "\0" a'
expect "64 KiB + 1 raw fails" "$RS" 125
reason "output overflow reason word" output_overflow
gone "$(cname)" "overflow: owned container removed at once"
rr -w output_overflow -- sh -c 'head -c 65537 /dev/zero'
expect "65537 NUL bytes fail as output_overflow (binary safe)" "$RS" 125
reason "NUL overflow reason" output_overflow
rr -- sh -c 'head -c 65536 /dev/zero'
expect "65536 NUL bytes pass and are all emitted" "$RS:$(wc -c <"$TMP/rout" | tr -d ' ')" "0:65536"
secret_run -w output_overflow a -- sh -c 'i=0; while [ $i -lt 32768 ]; do printf "a-"; i=$((i + 1)); done'
expect "a one-byte secret expanding past 64 KiB emitted is output_overflow" "$RS" 125
reason "expansion reason" output_overflow
a_unbounded() { # holds when an unbounded producer is cut off as output_overflow, never reported as the host deadline
	rrx "$ISO" "$1" --timeout 20 -- sh -c 'yes'
	OBS="status $RS, $(grep -o 'dev-run: [a-z_]*' "$TMP/rerr" | tail -n 1)"
	{ { [ "$RS" = 125 ] && grep -Fq 'dev-run: output_overflow' "$TMP/rerr"; } || { [ "$RS" = 124 ] && grep -Fq 'dev-run: timeout' "$TMP/rerr"; }; } && reached
	[ "$RS" = 125 ] && grep -Fq 'dev-run: output_overflow' "$TMP/rerr" && ! grep -Fq 'dev-run: timeout' "$TMP/rerr"
}
holds "unbounded output stops the proof as output_overflow, not as the host deadline" a_unbounded "$RUNNER"
gone "$(cname)" "flooding child's container is gone"
gvariant mut-pipe-held controller.go '			p.readers[w.idx].Close()
			if st, _ := w.v.(int); s.removeOnOut' '			if st, _ := w.v.(int); s.removeOnOut' '			c.rmNote(s.removeOnOut, st == 0)
			rm, rmCh, rmDue = nil, nil, nil' '			c.rmNote(s.removeOnOut, st == 0)
			p.readers[0].Close()
			rm, rmCh, rmDue = nil, nil, nil'
redgreen "output-stage read end held through the owned removal mutant" a_unbounded "$MUT/mut-pipe-held.sh" "$RUNNER"
rg_reset
(cd "$ISO" && "$RUNNER" -- sh -c 'printf out; sleep 1; printf more') >/dev/full 2>"$TMP/rerr"
RS=$?
plain_seen output_write_failed
expect "stdout to /dev/full is output_write_failed" "$RS" 125
reason "write failure reason" output_write_failed
a_redaction() { # holds when the child's secret never reaches the output
	(cd "$ISO" && BOXSET_TEST_SECRET=$S "$1" -- sh -c 'printf %s "$1"' x "$S") >"$TMP/rout" 2>"$TMP/rerr"
	RS=$?
	rerr_seen
	OBS="output '$(head -c 80 "$TMP/rout")'"
	[ "$RS" = 0 ] && [ -s "$TMP/rout" ] && reached
	[ "$RS" = 0 ] && ! grep -Fq "$S" "$TMP/rout" "$TMP/rerr"
}
gvariant mut-redactor-bypass controller.go 'return redactStream(c.ac, streamOpts{capN: capOut, raw: -1, tail: -1}, in, c.out)' '_, _ = io.Copy(c.out, in)
			return 0'
redgreen "redactor-bypass mutant" a_redaction "$MUT/mut-redactor-bypass.sh" "$RUNNER"
a_length_filter() { # holds when raw overflow is enforced inside the redactor, not by a length filter
	rrx "$ISO" "$1" -- sh -c 'head -c 65537 /dev/zero | tr "\0" a'
	OBS="status $RS"
	{ { [ "$RS" = 125 ] && grep -Fq 'dev-run: output_overflow' "$TMP/rerr"; } || { [ "$(wc -c <"$TMP/rout" | tr -d ' ')" = 65536 ] && ! grep -Fq 'dev-run: output_overflow' "$TMP/rerr"; }; } && reached
	[ "$RS" = 125 ] && grep -Fq 'dev-run: output_overflow' "$TMP/rerr"
}
gvariant mut-length-filter controller.go 'return redactStream(c.ac, streamOpts{capN: capOut, raw: -1, tail: -1}, in, c.out)' 'return redactStream(c.ac, streamOpts{capN: 1000000000, raw: -1, tail: -1}, io.LimitReader(in, capOut), c.out)'
redgreen "length-filter mutant" a_length_filter "$MUT/mut-length-filter.sh" "$RUNNER"
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
# image_build_failed is this fault copy's intended outcome: only that exact line is exempt from infrastructure attribution (an earlier redactor,
# host or setup failure stays contamination), and the injected failing step must have run: its credential-bearing output line is in the tail, masked
if intended_refusal image_build_failed "$TMP/xerr" && grep -Fq 'https://[REDACTED]@h.example/x?token=[REDACTED]' "$TMP/xerr"; then reached; fi
expect "a failing image build is image_build_failed" "$RS" 125
if grep -Fq 'dev-run: image_build_failed' "$TMP/xerr"; then pass "build failure reason"; else fail "build failure reason" "stderr lacks 'dev-run: image_build_failed': $(head -c 400 "$TMP/xerr")"; fi
if grep -Fq 'user:pw' "$TMP/xerr"; then fail "build log credential" "URL userinfo leaked"; else pass "build diagnostics carry no URL credential"; fi
if grep -Eq '^\[dev-run: [0-9]+ earlier bytes omitted\]$' "$TMP/xerr" && [ "$(wc -c <"$TMP/xerr")" -le 70000 ]; then pass "build diagnostics are a bounded tail with the omitted-count line"; else fail "build tail" "size $(wc -c <"$TMP/xerr")"; fi
rm -rf "$FISO"
a_start_diag() { # holds when start diagnostics appear only as a redacted tail
	rrx "$ISO" "$1" -- true
	OBS="status $RS, $(grep -c 'u:p@' "$TMP/rerr") leaked userinfo lines"
	[ "$RS" = 125 ] && grep -Fq 'dev-run: container_start_failed' "$TMP/rerr" && reached
	[ "$RS" = 125 ] && grep -Fq 'dev-run: container_start_failed' "$TMP/rerr" && ! grep -Fq 'u:p@' "$TMP/rerr" && grep -Fq '[REDACTED]' "$TMP/rerr"
}
gvariant fault-start controller.go '"--tmpfs", scratchTmpfs}' '"--tmpfs", scratchTmpfs, "--memory", "https://u:p@h.example/x"}'
gvariant mut-start-bypass controller.go '"--tmpfs", scratchTmpfs}' '"--tmpfs", scratchTmpfs, "--memory", "https://u:p@h.example/x"}' 'stderr: c.tailFn(4096),' 'stderr: func(r io.Reader) any { var b bytes.Buffer; _, _ = io.Copy(&b, r); return tailRes{b.Bytes(), 0} },'
redgreen "start-failure diagnostics redacted (bypass mutant)" a_start_diag "$MUT/mut-start-bypass.sh" "$MUT/fault-start.sh"

# ===== 7. reference mounts ================================================================================
# The reference checkouts are located only by the owner values BOXSET_REF_SERIES and BOXSET_REF_MOVIE; the driver copies their
# text into the isolated checkout, points both values there for these controls and restores them afterwards.
if [ -d "${BOXSET_REF_SERIES:-}" ] && [ -d "${BOXSET_REF_MOVIE:-}" ]; then
	REF_S0=$BOXSET_REF_SERIES
	REF_M0=$BOXSET_REF_MOVIE
	for side in series movie; do
		case $side in series) _rsrc=$REF_S0 ;; *) _rsrc=$REF_M0 ;; esac
		for rel in src/NzbDrone.Core.Test/ParserTests src/NzbDrone.Core.Test/Languages src/NzbDrone.Core/Parser src/NzbDrone.Core/Qualities src/NzbDrone.Core/Languages; do
			mkdir -p "$ISO/ref/$side/$rel"
			cp -R "$_rsrc/$rel/." "$ISO/ref/$side/$rel/" 2>/dev/null || true
		done
	done
	BOXSET_REF_SERIES=$ISO/ref/series
	BOXSET_REF_MOVIE=$ISO/ref/movie
	export BOXSET_REF_SERIES BOXSET_REF_MOVIE
	rr --ref -- sh -c 'test -r /ref/series/parser && test -r /ref/movie/qualities && ! touch /ref/series/parser/x 2>/dev/null && test ! -e /ref/series/decoy.sh'
	expect "reference text mounts read-only and nothing else" "$RS" 0
	decoy=$ISO/ref/series/src/NzbDrone.Core/Parser/bad.sh
	printf '#!/bin/sh\n' >"$decoy"
	chmod +x "$decoy"
	rr -w reference_input_invalid --ref -- true
	if [ "$RS" = 125 ] && grep -Fq 'dev-run: reference_input_invalid' "$TMP/rerr"; then pass "reference executable / non-text file is refused"; else fail "reference audit" "status $RS"; fi
	rm -f "$decoy"
	BOXSET_REF_SERIES=$REF_S0
	BOXSET_REF_MOVIE=$REF_M0
else
	fail "reference mounts" "BOXSET_REF_SERIES and BOXSET_REF_MOVIE do not name reference checkouts on this host: required for the --ref controls"
fi
rr -- sh -c 'test ! -e /ref'
expect "no reference mount without --ref" "$RS" 0

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
rr -w bench_unavailable --bench 0-3 -- true
if [ "$RS" = 125 ] && grep -Fq 'dev-run: bench_unavailable' "$TMP/rerr"; then pass "--bench is refused until its owner slice lands"; else fail "bench" "status $RS"; fi
mkdir -p "$TMP/shim"
for t in sh docker id mkdir rmdir ln rm mv head tail wc cat sha256sum timeout date grep find tr od dirname; do
	p=$(command -v "$t") && ln -sf "$p" "$TMP/shim/$t"
done
rg_reset
(cd "$ISO" && PATH=$TMP/shim "$RUNNER" --capture ".devrites/work/boxset-test/screenshots-20260101T000005Z" -- true) >"$TMP/rout" 2>"$TMP/xerr"
RS=$?
# the host-tool refusal is the intended outcome of these shim runs, judged on their own stderr
intended_refusal capture_host_tool_missing "$TMP/xerr" && reached
if [ "$RS" = 125 ] && grep -Fq 'dev-run: capture_host_tool_missing' "$TMP/xerr"; then pass "missing host tool refused before Docker (capture)"; else fail "host tool" "status $RS"; fi
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

# write-path spike: redirection, direct --screenshot and Bun each write in place on the precreated
# inode. Every producer's status is observed, the previous bytes are cleared before each producer so a
# leftover cannot pass for its output, and the child prints a completion signal; the run is then
# refused for exactly the final invalid-PNG reason, not for any unrelated 125.
SPIKE='n=/capture/dev-desktop.png
i0=$(stat -c %i $n)
printf redirect >$n || exit 51
[ "$(stat -c %i $n)" = "$i0" ] || exit 52
shoot_to() { "$B" --headless --no-sandbox --hide-scrollbars --force-device-scale-factor=1 --window-size=1440,900 --screenshot=$n "file:///tmp/dev-desktop.html" >/dev/null 2>&1; }
printf "<p>x</p>" >/tmp/dev-desktop.html
printf "" >$n || exit 57
[ ! -s $n ] || exit 58
shoot_to || exit 53
[ -s $n ] || exit 59
[ "$(stat -c %i $n)" = "$i0" ] || exit 54
[ "$(od -An -tx1 -N8 $n | tr -d " \n")" = 89504e470d0a1a0a ] || exit 60
printf "" >$n || exit 61
[ ! -s $n ] || exit 62
bun -e "require(\"fs\").writeFileSync(\"/capture/dev-desktop.png\", \"bun\")" || exit 55
[ "$(stat -c %i $n)" = "$i0" ] && [ "$(cat $n)" = bun ] || exit 56
echo SPIKE-DONE'
capture_run -w capture_not_png "$SPIKE"
if [ "$RS" = 125 ] && grep -Fq SPIKE-DONE "$TMP/rout" && grep -Fq 'dev-run: capture_not_png' "$TMP/rerr"; then pass "write-path spike: redirect, browser (PNG signature) and Bun each wrote in place, run refused only for the final invalid PNG (capture_not_png)"; else fail "write-path spike" "status $RS $(head -c 200 "$TMP/rout") $(head -c 200 "$TMP/rerr")"; fi

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
capture_run -w timeout -s 124 'shoot_all; sleep 100' --timeout 3
expect "hung child times out" "$RS" 124
assert_no_publication "hung child"
gone "$(cname)" "hung child's container is gone"
rr -s 3 --capture "$(capdir)" -- sh -c 'B=/nonexistent; if [ ! -x "$B" ]; then echo T31-PROVIDER-UNAVAILABLE; exit 3; fi'
if [ "$RS" = 3 ] && grep -Fq T31-PROVIDER-UNAVAILABLE "$TMP/rout"; then pass "unavailable provider is exit 3, distinct from an assertion failure"; else fail "provider unavailable" "status $RS"; fi
capture_run -s 1 'shoot_all; false'
expect "a failed in-place copy or assertion is exit 1, not 3" "$RS" 1
# a child whose Wait outcome is unknown: the compiled copy reports status -1 for the child exec. The status-7 control of this section run through it: its intended status assertion fails and the dependent
# no-publication credit is refused through the same classifier (a different nonzero status or a signal is never substituted for the unknown one).
gvariant fault-child-status-unknown controller.go 'return r.status, rs, tmo, true' 'return -1, rs, tmo, true'
status_unknown_control() {
	_suo=$(
		META=0
		RUNNER=$MUT/fault-child-status-unknown.sh
		capture_run -s 7 'exit 7'
		printf 'PREMISE %s %s\n' "$RS" "$(grep -c 'dev-run: child_status_unknown' "$TMP/rerr")"
		expect "T31 status-7 child through the unknown-status copy" "$RS" 7
		assert_no_publication "unknown child status"
	)
	META=1
	expect "unknown child status: the compiled copy reports 125 child_status_unknown once, where the child exited 7" "$(printf '%s\n' "$_suo" | grep -Fxc 'PREMISE 125 1')" 1
	expect "unknown child status: the intended status assertion fails" "$(printf '%s\n' "$_suo" | grep -c '^not ok - T31 status-7 child through the unknown-status copy')" 1
	expect "unknown child status: the dependent no-publication credit is refused" "$(printf '%s\n' "$_suo" | grep -c '^ok - unknown child status')" 0
	META=0
}
status_unknown_control

# map denials, ENOSPC bound and storage (SizeRw) with a held browserless child
CAPDEN_A='before=$(stat -c %i /capture/dev-desktop.png /capture/dev-mobile.png /capture/preview-desktop.png /capture/preview-mobile.png)
if rm /capture/dev-desktop.png 2>/dev/null; then exit 61; fi
if mv /capture/dev-desktop.png /capture/t 2>/dev/null; then exit 62; fi
if ln -s /src/capture.bin /capture/x 2>/dev/null; then exit 63; fi
if ln /capture/dev-desktop.png /tmp/h 2>/dev/null; then exit 64; fi
if mkdir /capture/d 2>/dev/null; then exit 65; fi
if mkfifo /capture/f 2>/dev/null; then exit 66; fi
if touch /capture/probe 2>/dev/null; then exit 67; fi
if chmod 0600 /capture/dev-desktop.png 2>/dev/null; then exit 68; fi
[ "$(stat -c %i /capture/dev-desktop.png /capture/dev-mobile.png /capture/preview-desktop.png /capture/preview-mobile.png)" = "$before" ] || exit 69
if head -c 9437184 /dev/zero >/capture/dev-desktop.png 2>/dev/null; then exit 70; fi
[ "$(stat -c %s /capture/dev-desktop.png)" -le 8454144 ] || exit 71
[ ! -e /src/capture.bin ] || exit 72'
CAPDEN="$CAPDEN_A
$CBAR
exit 0"
a_capture_bound() { # holds when /capture denials hold and SizeRw stays below 1 MiB with the container up
	CAP=$(capdir)
	bg_run capd "$1" --capture "$CAP" -- sh -c "$CAPDEN"
	_c=$(bg_ctr capd) || { OBS="no container"; SETUP=1; return 1; }
	if ! bg_wait "$_c" /tmp/ready; then OBS="not ready (child denial failed)"; SETUP=1; kill -KILL "$BG_PID" 2>/dev/null; bg_end capd; return 1; fi
	_sz=$(timeout 30 docker inspect -s -f '{{.SizeRw}}' "$_c" 2>/dev/null) || _sz=
	bg_touch "$_c" /tmp/go || SETUP=1
	bg_end capd
	OBS="SizeRw ${_sz:-unknown}, child status $RS"
	case $_sz in '' | *[!0-9]*) SETUP=1; return 1 ;; esac
	reached
	[ "$_sz" -lt 1048576 ] && [ ! -e "$ISO/capture.bin" ]
}
holds "child rm/mv/ln -s/ln/mkdir/mkfifo/touch/chmod in /capture fail, 9 MiB write hits ENOSPC at the cap, SizeRw below 1 MiB" a_capture_bound "$RUNNER"
gvariant mut-capture-unprotected controller.go 'size=8454144,nr_inodes=8,mode=0755,uid=65534,gid=65534"' 'size=8454144,nr_inodes=8,mode=0700,uid=1000,gid=1000"' '!c.precreate("/capture", pngNames())' 'false' '!c.capturePreflight()' 'false'
a_capture_symlink_bound() { # holds when a symlink through /capture cannot grow the container layer
	CAP=$(capdir)
	bg_run capu "$1" --capture "$CAP" -- sh -c "ln -s /src/capture.bin /capture/dev-desktop.png 2>/dev/null; head -c 9437184 /dev/zero >/capture/dev-desktop.png 2>/dev/null; touch /tmp/ready; n=0; while [ ! -e /tmp/go ] && [ \$n -lt 600 ]; do sleep 1; n=\$((n + 1)); done"
	_c=$(bg_ctr capu) || { OBS="no container"; SETUP=1; return 1; }
	if ! bg_wait "$_c" /tmp/ready; then OBS="not ready"; SETUP=1; kill -KILL "$BG_PID" 2>/dev/null; bg_end capu; return 1; fi
	_sz=$(timeout 30 docker inspect -s -f '{{.SizeRw}}' "$_c" 2>/dev/null) || _sz=
	bg_touch "$_c" /tmp/go || SETUP=1
	bg_end capu
	OBS="SizeRw ${_sz:-unknown}"
	case $_sz in '' | *[!0-9]*) SETUP=1; return 1 ;; esac
	reached
	[ "$_sz" -lt 1048576 ]
}
redgreen "unprotected-capture-map mutant (child-owned dir, symlink into /src)" a_capture_symlink_bound "$MUT/mut-capture-unprotected.sh" "$RUNNER"

# capability preflight mutants (fault copies are refused by the unchanged preflight)
a_refuse() { # a_refuse WORD RUNNER: the runner refuses before the child produces output
	rrx "$ISO" "$2" --capture "$(capdir)" -- sh -c 'printf ran'
	OBS="status $RS, $(grep -o 'dev-run: [a-z_]*' "$TMP/rerr" | tail -n 1)"
	{ { [ "$RS" = 125 ] && grep -Fq "dev-run: $1" "$TMP/rerr"; } || [ "$(cat "$TMP/rout")" = ran ]; } && reached
	[ "$RS" = 125 ] && grep -Fq "dev-run: $1" "$TMP/rerr" && [ -z "$(cat "$TMP/rout")" ]
}
a_refuse_untrusted() { a_refuse supervisor_untrusted "$1"; }
a_refuse_capmissing() { a_refuse capture_capability_missing "$1"; }
PID1USER_OLD='"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--user", supUser,'
gvariant fault-pid1-same-uid controller.go "$PID1USER_OLD" '"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--user", childUser,'
gvariant mut-pid1-same-uid controller.go "$PID1USER_OLD" '"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--user", childUser,' '!procOK(t, "65534")' 'false'
redgreen "PID1-same-UID mutant" a_refuse_untrusted "$MUT/mut-pid1-same-uid.sh" "$MUT/fault-pid1-same-uid.sh"
# the nonzero CapBnd premise is the controller's own read of the unprivileged child's status in the compiled copy (receipt), never assumed from the removed --cap-drop; the mutant removes only the capability predicate
CAPRCPT=$TMP/capbnd.receipt
CAPRCPT_OLD='t, ok := c.childStatus()'
CAPRCPT_NEW='t, ok := c.childStatus()
	{ f, _ := os.OpenFile("'$CAPRCPT'", os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644); cb, _ := statusField(t, "CapBnd"); fmt.Fprintf(f, "capbnd %s\n", strings.Join(cb, " ")); f.Close() }'
gvariant fault-no-capdrop controller.go "$PID1USER_OLD" '"--security-opt", "no-new-privileges", "--user", supUser,' "$CAPRCPT_OLD" "$CAPRCPT_NEW"
gvariant mut-no-capdrop controller.go "$PID1USER_OLD" '"--security-opt", "no-new-privileges", "--user", supUser,' "$CAPRCPT_OLD" "$CAPRCPT_NEW" 'len(f) != 1 || f[0] != "0000000000000000"' 'len(f) != 1'
a_capdrop() { # a_capdrop RUNNER: a container started without --cap-drop ALL gives its unprivileged child a nonzero bounding set (observed at the controller's own identity read) and the preflight refuses before the child
	: >"$CAPRCPT" || { OBS="the CapBnd receipt could not be created"; SETUP=1; return 1; }
	rrx "$ISO" "$1" --capture "$(capdir)" -- sh -c 'printf ran'
	_cb=$(head -n 1 "$CAPRCPT")
	OBS="status $RS, $(grep -o 'dev-run: [a-z_]*' "$TMP/rerr" | tail -n 1), CapBnd receipt '$_cb'"
	case $_cb in
	"capbnd 0000000000000000" | "capbnd " | "")
		OBS="$OBS; the nonzero CapBnd premise was not observed"
		SETUP=1
		return 1
		;;
	"capbnd "[0-9a-f]*) ;;
	*)
		SETUP=1
		return 1
		;;
	esac
	{ { [ "$RS" = 125 ] && grep -Fq 'dev-run: supervisor_untrusted' "$TMP/rerr"; } || [ "$(cat "$TMP/rout")" = ran ]; } && reached
	[ "$RS" = 125 ] && grep -Fq 'dev-run: supervisor_untrusted' "$TMP/rerr" && [ -z "$(cat "$TMP/rout")" ]
}
redgreen "missing-cap-drop (CapBnd nonzero) mutant" a_capdrop "$MUT/mut-no-capdrop.sh" "$MUT/fault-no-capdrop.sh"
gvariant fault-no-tmpfs-options controller.go 'size=8454144,nr_inodes=8,mode=0755,uid=65534,gid=65534"' 'size=8454144,mode=0755,uid=65534,gid=65534"'
gvariant mut-no-tmpfs-options controller.go 'size=8454144,nr_inodes=8,mode=0755,uid=65534,gid=65534"' 'size=8454144,mode=0755,uid=65534,gid=65534"' '!c.capturePreflight()' 'false'
redgreen "missing-tmpfs-options mutant" a_refuse_capmissing "$MUT/mut-no-tmpfs-options.sh" "$MUT/fault-no-tmpfs-options.sh"
gvariant fault-regular-layer controller.go '"/capture:rw,nosuid,nodev,noexec,size=8454144' '"/capturex:rw,nosuid,nodev,noexec,size=8454144'
gvariant mut-regular-layer controller.go '"/capture:rw,nosuid,nodev,noexec,size=8454144' '"/capturex:rw,nosuid,nodev,noexec,size=8454144' '!c.precreate("/capture", pngNames())' 'false' '!c.capturePreflight()' 'false'
redgreen "regular-layer /capture mutant" a_refuse_capmissing "$MUT/mut-regular-layer.sh" "$MUT/fault-regular-layer.sh"

# authority: PID 1, forged control files and markers, detached writers, kill -KILL -1. Expected-denied
# writes use the non-special command `true`, so a refused redirection fails that command only and the
# fixture always reaches its barrier (an exit 8x means a denial did not hold).
AUTH_A='shoot_all
if kill -0 1 2>/dev/null; then exit 81; fi
if kill -STOP 1 2>/dev/null; then exit 82; fi
if kill -KILL 1 2>/dev/null; then exit 83; fi
if mkdir /run/boxset-capture 2>/dev/null; then exit 84; fi
if true 2>/dev/null >/run/boxset-capture-exited; then exit 85; fi
if true 2>/dev/null >/run/boxset-capture-release; then exit 86; fi
echo "DEV-RUN-CAPTURE-PASS files=4 bytes=1 dir=forged challenge=0000000000000000"
setsid sleep 300 >/dev/null 2>&1 &
echo $! >/tmp/sleeper.pid'
AUTH="$AUTH_A
$CBAR
exit 9"
CAP=$(capdir)
rg_reset
bg_run auth "$RUNNER" --capture "$CAP" -- sh -c "$BASE$AUTH"
c=$(bg_ctr auth)
if bg_wait "$c" /tmp/ready; then
	reached
	pid1=$(timeout 30 docker inspect -f '{{.State.Pid}} {{.State.Status}}' "$c" 2>/dev/null)
	[ -n "$pid1" ] || unknown_obs
	expect "held child after forging: PID 1 running (host inspect), runner waiting, DIR empty" "$(kill -0 "$BG_PID" 2>/dev/null && echo alive):$(find "$ISO/$CAP" -mindepth 1 | wc -l | tr -d ' '):${pid1#* }" "alive:0:running"
	# the child reached the barrier only after every denial held and the forged PASS and sleeper were produced
	expect "authority fixture reached its barrier: forged PASS printed, detached sleeper recorded alive" "$(cx "$c" /usr/bin/sh -c 'kill -0 "$(cat /tmp/sleeper.pid)" && echo alive' 2>/dev/null)" alive
	# interference attempted by the test as the child user, then PID 1 is inspected again
	_kz=$(cx "$c" /usr/bin/sh -c 'kill -0 1 2>/dev/null; a=$?; kill -STOP 1 2>/dev/null; b=$?; kill -KILL 1 2>/dev/null; d=$?; echo "$a$b$d"' 2>/dev/null)
	expect "child kill -0, -STOP and -KILL on PID 1 all fail (EPERM by design)" "$([ -n "$_kz" ] && case $_kz in *0*) echo some-succeeded ;; *) echo all-failed ;; esac)" all-failed
	pid1b=$(timeout 30 docker inspect -f '{{.State.Pid}} {{.State.Status}}' "$c" 2>/dev/null)
	[ -n "$pid1b" ] || unknown_obs
	_st1=$(timeout 30 docker exec -u 65534:65534 "$c" /usr/bin/cat /proc/1/status 2>/dev/null | grep '^State:')
	expect "PID 1 re-inspected after the attempted interference: same host pid, running, not stopped" "${pid1b}:$(printf '%s' "$_st1" | grep -Ec '^State:[[:space:]]+[SR] ')" "${pid1}:1"
	bg_touch "$c" /tmp/go || SETUP=1
	bg_end auth
	expect "release into exit 9 propagates 9" "$RS" 9
	assert_no_publication "forged markers with nonzero child"
	if [ "$(grep -c '^DEV-RUN-CAPTURE-PASS' "$TMP/rout")" = 1 ] && ! grep -Eq "^DEV-RUN-CAPTURE-PASS files=4 bytes=[0-9]+ dir=$CAP challenge=" "$TMP/rout"; then pass "forged PASS line is the child's own and is not the runner's"; else fail "forged PASS" "$(cat "$TMP/rout")"; fi
else
	fail "authority barrier" "child never reached the barrier"
	kill -KILL "$BG_PID" 2>/dev/null
fi
capture_run -s 137 'shoot_all; kill -KILL -1'
expect "isolated kill -KILL -1 gives exit 137" "$RS" 137
assert_no_publication "kill -KILL -1"
gone "$(cname)" "kill -KILL -1: container removed"

# transfer faults: exporter, validator, limiter and manifest statuses are all checked. Each injected seam writes its own
# receipt line (XRCPT) when it runs: the exporter and limiter seams carry the real status of their own prerequisite, the
# validator seam the validator container's real exit, and the manifest seams can only run after all four files were
# exported, limited and validated. A generic capture_copy_failed after an earlier, unrelated refusal leaves that receipt
# absent or different and is setup, never evidence for the intended seam.
XRCPT=$TMP/xfer.receipt
a_transfer_fault() { # a_transfer_fault RUNNER (TF global exporter|limiter|validator|manifest-content|manifest-status): holds when the injected seam failure is capture_copy_failed with no publication
	CAP=$(capdir)
	true >"$XRCPT" || { OBS="the seam receipt could not be created"; SETUP=1; return 1; }
	rrx "$ISO" "$1" --capture "$CAP" -- sh -c "${BASE}shoot_all"
	_xr=$(head -n 1 "$XRCPT" 2>/dev/null)
	OBS="status $RS, $(grep -o 'dev-run: [a-z_]*' "$TMP/rerr" | tail -n 1), DIR $([ -e "$ISO/$CAP" ] && echo present || echo absent), first seam receipt '$_xr'"
	case $TF in
	exporter) _xw='exporter dev-desktop 0' ;;
	limiter) _xw='limiter 0' ;;
	validator) _xw='validator 9' ;;
	manifest-content) _xw='manifest-content' ;;
	manifest-status) _xw='manifest-status' ;;
	*) _xw='unknown seam' ;;
	esac
	[ "$_xr" = "$_xw" ] || { OBS="$OBS; the injected $TF seam did not run with its prerequisite established (want '$_xw')"; SETUP=1; return 1; }
	{ { [ "$RS" = 125 ] && grep -Fq 'dev-run: capture_copy_failed' "$TMP/rerr"; } || { [ "$RS" = 0 ] && grep -Fq 'DEV-RUN-CAPTURE-PASS' "$TMP/rout"; }; } && reached
	_rid=$(runid)
	cstate "boxset-dev-$_rid-v-dev-desktop"
	_v=$CST
	[ "$RS" = 125 ] && grep -Fq 'dev-run: capture_copy_failed' "$TMP/rerr" && [ ! -e "$ISO/$CAP" ] && [ "$_v" = absent ]
}
# grcpt FORMAT ARGS: a Go statement appending one receipt line of the injected seam to XRCPT (FORMAT and ARGS as the fmt.Fprintf operands)
grcpt() { printf '%s' '{ f, _ := os.OpenFile("'$XRCPT'", os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644); fmt.Fprintf(f, '"$1"'); f.Close() }'; }
EXP_OLD='if xr.startErr != nil {'
EXP_NEW="$(grcpt '"exporter %s %d\n", name, xr.status')
		xr.status = 7
		if xr.startErr != nil {"
EXP_CHK='if cr.n != capPNG+1 && xr.status != 0 {
			return "capture_copy_failed"
		}'
gvariant fault-exporter controller.go "$EXP_OLD" "$EXP_NEW"
gvariant mut-exporter-status-ignored controller.go "$EXP_OLD" "$EXP_NEW" "$EXP_CHK" '_ = xr.status'
TF=exporter
redgreen "exporter fault (valid bytes then exit 7), status-ignoring mutant" a_transfer_fault "$MUT/mut-exporter-status-ignored.sh" "$MUT/fault-exporter.sh"
VAL_OLD='"/usr/local/libexec/boxset/capture-check", "-name", name, "-challenge", c.challenge}'
VAL_NEW='"/usr/bin/sh", "-c", "cat; exit 9"}'
VAL_RCPT_OLD='_ = c.croot.Remove(raw)'
VAL_RCPT_NEW="_ = c.croot.Remove(raw)
		$(grcpt '"validator %d\n", vr.status')"
gvariant fault-validator controller.go "$VAL_OLD" "$VAL_NEW" "$VAL_RCPT_OLD" "$VAL_RCPT_NEW"
gvariant mut-validator-status-ignored controller.go "$VAL_OLD" "$VAL_NEW" "$VAL_RCPT_OLD" "$VAL_RCPT_NEW" 'return "capture_copy_failed" // any other validator status' '_ = vr.status'
TF=validator
redgreen "validator fault (valid bytes then nonzero), status-ignoring mutant" a_transfer_fault "$MUT/mut-validator-status-ignored.sh" "$MUT/fault-validator.sh"
LIM_OLD='limOK := xr.outOK && cr.err == nil'
LIM_NEW="$(grcpt '"limiter %d\n", xr.status')
		cr.err = errors.New(\"injected limiter failure\")
		limOK := xr.outOK && cr.err == nil"
gvariant fault-limiter controller.go "$LIM_OLD" "$LIM_NEW"
gvariant mut-limiter-ignored controller.go "$LIM_OLD" "$LIM_NEW" "$LIM_OLD" 'limOK := true'
TF=limiter
redgreen "limiter failure, status-ignoring mutant" a_transfer_fault "$MUT/mut-limiter-ignored.sh" "$MUT/fault-limiter.sh"
WM_OLD='if _, err := io.WriteString(f, content); err != nil {'
WM_NEW="$(grcpt '"manifest-content\n"')
	if _, err := io.WriteString(f, \"x\"); err != nil {"
gvariant fault-manifest-write controller.go "$WM_OLD" "$WM_NEW"
gvariant mut-manifest-unchecked controller.go "$WM_OLD" "$WM_NEW" 'if err != nil || len(data) != len(manifest) || sha256.Sum256(data) != sha256.Sum256([]byte(manifest)) {' 'if err != nil || len(data) < 0 {'
TF=manifest-content
redgreen "manifest content fault (length/hash), unchecked mutant" a_transfer_fault "$MUT/mut-manifest-unchecked.sh" "$MUT/fault-manifest-write.sh"
WMS_OLD='return f.Close()'
WMS_NEW="$(grcpt '"manifest-status\n"')
	f.Close()
	return errors.New(\"injected manifest status\")"
gvariant fault-manifest-status controller.go "$WMS_OLD" "$WMS_NEW"
gvariant mut-manifest-status-ignored controller.go "$WMS_OLD" "$WMS_NEW" 'if err := c.writeManifest(manifest); err != nil {
		return "capture_copy_failed"
	}' '_ = c.writeManifest(manifest)'
TF=manifest-status
redgreen "manifest writer fault (complete bytes then nonzero status), unchecked-status mutant" a_transfer_fault "$MUT/mut-manifest-status-ignored.sh" "$MUT/fault-manifest-status.sh"

# post-quiescence capture map audit: a changed inode with perfectly valid PNG bytes is refused by the
# runner's own consumer of the recorded inode map, nothing is published and the closure is checked
a_capture_map_changed() { # a_capture_map_changed RUNNER: only the inode changes; every other fact is held and inspected
	CAP=$(capdir)
	rm -f "$HOOKOK"
	rrx "$ISO" "$1" --capture "$CAP" -- sh -c "${BASE}shoot_all"
	OBS="status $RS, $(grep -o 'dev-run: [a-z_]*' "$TMP/rerr" | tail -n 1), DIR $([ -e "$ISO/$CAP" ] && echo present || echo absent)"
	[ -e "$HOOKOK" ] || { OBS="$OBS; the inode-only fixture did not establish its facts"; SETUP=1; return 1; }
	{ { [ "$RS" = 125 ] && grep -Fq 'dev-run: capture_map_changed' "$TMP/rerr"; } || { [ "$RS" = 0 ] && grep -Fq 'DEV-RUN-CAPTURE-PASS' "$TMP/rout"; }; } && reached
	cstate "$(cname)"
	[ "$RS" = 125 ] && grep -Fq 'dev-run: capture_map_changed' "$TMP/rerr" && ! grep -Fq 'DEV-RUN-CAPTURE-PASS' "$TMP/rout" &&
		[ ! -e "$ISO/$CAP" ] && [ "$CST" = absent ]
}
# The replacement keeps owner, group, mode, link count, type and bytes of the recorded file and differs
# only by inode; the fixture proves each fact before it moves the replacement into place and signals it.
HOOKOK=$TMP/hook.ok
HOOKSH='cd /capture && /usr/bin/cp dev-desktop.png t.tmp && /usr/bin/chmod 0666 t.tmp && [ "$(/usr/bin/stat -c "%u %g %a %h %F" t.tmp)" = "$(/usr/bin/stat -c "%u %g %a %h %F" dev-desktop.png)" ] && [ "$(/usr/bin/stat -c %i t.tmp)" != "$(/usr/bin/stat -c %i dev-desktop.png)" ] && [ "$(/usr/bin/sha256sum <t.tmp)" = "$(/usr/bin/sha256sum <dev-desktop.png)" ] && /usr/bin/mv -f t.tmp dev-desktop.png'
MAPCHG_OLD='// quiescence is established; no map has been audited yet'
MAPCHG_NEW=$MAPCHG_OLD'
	if r := c.supCall(10, "/usr/bin/sh", "-c", `'"$HOOKSH"'`); r.startErr == nil && r.status == 0 {
		_ = os.WriteFile("'$HOOKOK'", nil, 0o644)
	}'
gvariant fault-map-changed controller.go "$MAPCHG_OLD" "$MAPCHG_NEW"
gvariant mut-map-audit-removed controller.go "$MAPCHG_OLD" "$MAPCHG_NEW" 'func (c *ctl) captureAudit() bool {' 'func (c *ctl) captureAudit() bool {
	return true'
gvariant mut-map-inode-compare-removed controller.go "$MAPCHG_OLD" "$MAPCHG_NEW" '} else if w, found := want[name]; !found || w != ino {' '} else if w, found := want[name]; !found || (w != ino && false) {'
redgreen "capture-map audit removed (post-quiescence inode change with valid bytes)" a_capture_map_changed "$MUT/mut-map-audit-removed.sh" "$MUT/fault-map-changed.sh"
redgreen "capture-map inode comparison removed alone (only the inode changes, valid bytes and every other fact held)" a_capture_map_changed "$MUT/mut-map-inode-compare-removed.sh" "$MUT/fault-map-changed.sh"

# ---- owned-name closure (T26). The driver observes the runner's own Docker calls through a logging PATH
# shim and the reconciliation window through a logging query seam; it never queries by label.
# the logging shim (dshim_write over $REALDOCKER) was written right after the clock acquisition
rrlog() { # rrlog DIR RUNNER ARGS...: like rrx, with the runner's Docker calls logged to $TMP/dlog
	_xd=$1
	_xr=$2
	shift 2
	true >"$TMP/dlog" && true >"$TMP/dlog.q" || SETUP=1
	(cd "$_xd" && PATH="$TMP/dshim:$PATH" DLOG="$TMP/dlog" timeout 240 "$_xr" "$@") >"$TMP/rout" 2>"$TMP/rerr"
	RS=$?
	rerr_seen
	_xc=$(cname)
	[ -z "$_xc" ] || case " $OWNED " in *" $_xc "*) ;; *) OWNED="$OWNED $_xc" ;; esac
}
dqueries() { grep -Fc -- "ps -a --filter name=^$1\$" "$TMP/dlog"; } # dqueries NAME: logged exact-name queries
# an issued main create that returned nonzero or timed out is a retained failure whatever later queries show.
# The issued name is observed at the actual issue seam: every fault copy records $CTR before its injected
# result, so the name exists even when Docker is never invoked. A real effect is a receipt, never a guess:
# the after and delayed faults run the actual `docker run` and write its real status and stdout to RECEIPT,
# and the driver credits an effect only on a successful one (status 0 and the container id; for a validator
# status 0 and its canonical PNG byte count). The delayed fault lands its effect after the failure has been
# reported and holds the runner's first exact-name query of the issued name until the effect has landed, so
# the late create is observed and removed by the runner's own closure. The final state of the issued name is
# read, status-checked, before this driver removes anything: driver removal is never runner closure.
ISSUELOG=$TMP/issued.seen
RECEIPT=$TMP/create.receipt
CF_EFFECT=none
# The create seams of a fault copy are Go statements at the controller's argv builders; there is no production hook. GO_ISSUE records the owned name at the seam,
# before the injected result, so it exists even when Docker is never invoked. An effect script runs the real `docker run` after a delay, stamps its start with the shared
# clock first and writes its real status and output atomically to the receipt: a real effect is a receipt, never a guess.
GO_ISSUE_C='func() { f, _ := os.OpenFile("'$ISSUELOG'", os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644); fmt.Fprintln(f, c.ctr); f.Close() }()'
GO_ISSUE_V='func() { f, _ := os.OpenFile("'$ISSUELOG'", os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644); fmt.Fprintln(f, vname); f.Close() }()'
EFFECT_C=$TMP/effect-main.sh
EFFECT_V=$TMP/effect-validator.sh
cat >"$EFFECT_C" <<EFFECT || exit 125
#!/bin/sh
echo \$\$ >"$RECEIPT.pid"
sleep "\$1"
shift
$CLOCK -clock >"$RECEIPT.t"
o=\$(timeout --kill-after=10s 60s docker "\$@")
x=\$?
printf '%s %s' "\$x" "\$o" >"$RECEIPT.tmp" && mv "$RECEIPT.tmp" "$RECEIPT"
EFFECT
cat >"$EFFECT_V" <<EFFECT || exit 125
#!/bin/sh
echo \$\$ >"$RECEIPT.pid"
sleep "\$1"
vt=\$2
shift 2
$CLOCK -clock >"$RECEIPT.t"
timeout --kill-after=2s "\${vt}s" docker run "\$@" >"$RECEIPT.out"
x=\$?
printf '%s %s' "\$x" "\$(wc -c <"$RECEIPT.out" | tr -d ' ')" >"$RECEIPT.tmp" && mv "$RECEIPT.tmp" "$RECEIPT"
EFFECT
RUNOLD='return append([]string{"timeout", "--kill-after=10s", "60s", "docker"}, args...)'
VOLD1='return []string{"timeout", "--kill-after=2s", strconv.Itoa(left) + "s", "docker", "run", "--rm",'
VNEW1='real := []string{"timeout", "--kill-after=2s", strconv.Itoa(left) + "s", "docker", "run", "--rm",'
VOLD2='"-challenge", c.challenge}
}'
QSEAM_OLD='func (c *ctl) queryName(secs string, bound time.Duration, name, phase string) (string, bool) {'
QHOLD_C=$QSEAM_OLD'
	if phase == "first" {
		for i := 0; i < 90; i++ {
			if fi, err := os.Stat("'$RECEIPT'"); err == nil && fi.Size() > 0 {
				break
			}
			time.Sleep(time.Second)
		}
	}'
QHOLD_V=$QSEAM_OLD'
	if phase == "first" && strings.Contains(name, "-v-") {
		for i := 0; i < 90; i++ {
			if fi, err := os.Stat("'$RECEIPT'"); err == nil && fi.Size() > 0 {
				break
			}
			time.Sleep(time.Second)
		}
	}'
# the track-successful-creates-only mutant: a name is issued only after its call returned status 0 (instead of when its native process started)
SO1_OLD='if s.issue != "" {
		c.issued = append(c.issued, s.issue)
	}'
SO1_NEW='_ = s.issue'
SO2_OLD='return p.wait()'
SO2_NEW='res := p.wait()
	if p.s.issue != "" && res.status == 0 {
		c.issued = append(c.issued, p.s.issue)
	}
	return res'
cf_go() { # cf_go none|after|timeout|delayed: the Go statements of the main create seam
	case $1 in
	none) printf '%s\n\treturn []string{"/bin/sh", "-c", "exit 1"}' "$GO_ISSUE_C" ;;
	after) printf '%s\n\treturn append([]string{"/bin/sh", "-c", `/bin/sh "$@"; exit 1`, "sh", "%s", "0"}, args...)' "$GO_ISSUE_C" "$EFFECT_C" ;;
	timeout) printf '%s\n\treturn []string{"timeout", "1", "sleep", "3"}' "$GO_ISSUE_C" ;;
	delayed) printf '%s\n\treturn append([]string{"/bin/sh", "-c", `setsid /bin/sh "$@" >/dev/null 2>&1 </dev/null & _i=0; while [ ! -s "%s" ] && [ "$_i" -lt 500 ]; do sleep 0.01; _i=$((_i + 1)); done; exit 1`, "sh", "%s", "%s"}, args...)' "$GO_ISSUE_C" "$RECEIPT.pid" "$EFFECT_C" "$CF_DELAY" ;;
	esac
}
vf_go() { # vf_go none|after|timeout|delayed: the Go statements of the validator create seam (real is the original argv; the effect scripts receive the original arguments and stdin)
	case $1 in
	none) printf '\t_ = real\n\t%s\n\treturn []string{"/bin/sh", "-c", "exit 125"}' "$GO_ISSUE_V" ;;
	after) printf '\t%s\n\treturn append([]string{"/bin/sh", "-c", `/bin/sh "$@"; exit 125`, "sh", "%s", "0", strconv.Itoa(left)}, real[5:]...)' "$GO_ISSUE_V" "$EFFECT_V" ;;
	timeout) printf '\t_ = real\n\t%s\n\treturn []string{"timeout", "1", "sleep", "3"}' "$GO_ISSUE_V" ;;
	delayed) printf '\t%s\n\treturn append([]string{"/bin/sh", "-c", `exec 7<&0; setsid /bin/sh "$@" <&7 >/dev/null 2>&1 & _i=0; while [ ! -s "%s" ] && [ "$_i" -lt 500 ]; do sleep 0.01; _i=$((_i + 1)); done; exec 7<&-; exit 125`, "sh", "%s", "%s", strconv.Itoa(left)}, real[5:]...)' "$GO_ISSUE_V" "$RECEIPT.pid" "$EFFECT_V" "$CF_DELAY" ;;
	esac
}
# cf_effect NAME RECEIPT_RE: the effect premise of the injected cell. none: Docker is never invoked, so no
# receipt may exist; after and delayed: the actual `docker run` wrote its receipt (the late one is awaited,
# up to 90 s) and it must be a successful create. An unestablished premise is setup, never target evidence.
cf_effect() {
	if [ "$CF_EFFECT" = none ]; then
		[ ! -e "$RECEIPT" ] && return 0
		OBS="$OBS; a receipt exists for a cell that never invokes Docker"
	else
		if [ "$CF_EFFECT" = delayed ] && [ ! -s "$RECEIPT.pid" ]; then
			OBS="$OBS; the escaped effect session never announced itself before its parent returned"
			SETUP=1
			docker rm -f "$1" >/dev/null 2>&1
			return 1
		fi
		_cw=0
		while [ ! -s "$RECEIPT" ] && [ "$_cw" -lt 90 ]; do
			sleep 1
			_cw=$((_cw + 1))
		done
		_cr=$(cat "$RECEIPT" 2>/dev/null)
		if printf '%s\n' "$_cr" | grep -Eq "$2"; then
			OBS="$OBS; effect receipt '$(printf '%s' "$_cr" | cut -c1-24)'"
			return 0
		fi
		OBS="$OBS; no successful effect receipt (status and output '$(printf '%s' "$_cr" | cut -c1-80)')"
	fi
	SETUP=1
	docker rm -f "$1" >/dev/null 2>&1
	return 1
}
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
CF_HOLD=0
CF_NATURAL=0
CF_RUNARGS=
CF_DELAY=2
a_create_failure() { # a_create_failure RUNNER (CF_EFFECT none|after|delayed, CF_HOLD, CF_NATURAL and CF_RUNARGS globals): container_start_failed plus cleanup_failed for the observed issued name, its real effect read from the receipt and its final absence from a status-checked query read before any driver removal
	true >"$ISSUELOG" || { OBS="the issue log could not be created"; SETUP=1; return 1; }
	rm -f "$RECEIPT" "$RECEIPT.tmp" "$RECEIPT.t" "$RECEIPT.pid"
	# shellcheck disable=SC2086
	rrlog "$ISO" "$1" $CF_RUNARGS -- sh -c 'printf ran'
	_n=$(head -n 1 "$ISSUELOG")
	[ -z "$_n" ] || case " $OWNED " in *" $_n "*) ;; *) OWNED="$OWNED $_n" ;; esac
	_q=0
	[ -z "$_n" ] || _q=$(dqueries "$_n")
	OBS="status $RS, issued '$_n', $_q queries, $(grep -o 'dev-run: [a-z_]*' "$TMP/rerr" | tr '\n' ' ')"
	[ -n "$_n" ] || { SETUP=1; return 1; }
	cf_effect "$_n" '^0 [0-9a-f]{64}$' || return 1
	[ "$CF_NATURAL" != 1 ] || cf_natural "$_n" || { docker rm -f "$_n" >/dev/null 2>&1; return 1; }
	cf_final "$_n" || { docker rm -f "$_n" >/dev/null 2>&1; return 1; }
	OBS="$OBS; final state of the issued name before any driver removal: $_fin"
	[ "$RS" = 125 ] && grep -Fq 'dev-run: container_start_failed' "$TMP/rerr" && reached
	_ok=0
	[ "$RS" = 125 ] && grep -Fq 'dev-run: container_start_failed' "$TMP/rerr" && grep -Fq "dev-run: cleanup_failed container=$_n" "$TMP/rerr" &&
		[ -z "$(cat "$TMP/rout")" ] && [ "$_q" -ge 2 ] && [ "$_fin" = absent ] && _ok=1
	docker rm -f "$_n" >/dev/null 2>&1
	[ "$_ok" = 1 ]
}
cf_variant() { # cf_variant NAME KIND [OLD NEW]...: a main-create fault copy; a held delayed cell also holds the first exact-name query until the late effect has landed
	_cvn=$1
	_cvk=$2
	shift 2
	if [ "$CF_HOLD" = 1 ]; then
		gvariant "$_cvn" controller.go "$RUNOLD" "$(cf_go "$_cvk")" "$QSEAM_OLD" "$QHOLD_C" "$@"
	else
		gvariant "$_cvn" controller.go "$RUNOLD" "$(cf_go "$_cvk")" "$@"
	fi
}
# an unknown Wait outcome of the create (the real `docker run` ran, its status is reported as -1) is retained and reconciled like a failed or timed-out create; the issued name is tracked from the start
CF_UNK_OLD='tail, tok := r.tail()
	if r.status != 0 || !r.errOK || !tok {'
CF_UNK_NEW='r.status = -1 // an unknown Wait outcome
	tail, tok := r.tail()
	if r.status != 0 || !r.errOK || !tok {'
for cf in none after timeout delayed unknown; do
	CF_HOLD=0
	case $cf in
	none) CF_EFFECT=none ;;
	after | unknown) CF_EFFECT=after ;;
	timeout) CF_EFFECT=none ;;
	delayed) CF_EFFECT=delayed CF_HOLD=1 ;;
	esac
	if [ "$cf" = unknown ]; then
		cf_variant "cf-$cf" after "$CF_UNK_OLD" "$CF_UNK_NEW"
		cf_variant "mut-cf-nolatch-$cf" after 'c.failCreate(c.ctr)' '_ = 0' "$CF_UNK_OLD" "$CF_UNK_NEW"
		cf_variant "mut-cf-successonly-$cf" after "$SO1_OLD" "$SO1_NEW" "$SO2_OLD" "$SO2_NEW" "$CF_UNK_OLD" "$CF_UNK_NEW"
	else
		cf_variant "cf-$cf" "$cf"
		cf_variant "mut-cf-nolatch-$cf" "$cf" 'c.failCreate(c.ctr)' '_ = 0'
		cf_variant "mut-cf-successonly-$cf" "$cf" "$SO1_OLD" "$SO1_NEW" "$SO2_OLD" "$SO2_NEW"
	fi
	redgreen "issued main create $cf: create-failure-latch-removed mutant" a_create_failure "$MUT/mut-cf-nolatch-$cf.sh" "$MUT/cf-$cf.sh"
	redgreen "issued main create $cf: track-successful-creates-only mutant" a_create_failure "$MUT/mut-cf-successonly-$cf.sh" "$MUT/cf-$cf.sh"
done
# the genuine delayed-effect sibling perturbs the create seam only: the controller's own queries are untouched, so its closure
# really observes absence before the late effect lands; the effect receipt, the absent-before-effect shim receipts and the bounded
# terminal state (the container's own expiry, --timeout 1) are all read before the driver removes anything
CF_HOLD=0
CF_EFFECT=delayed
CF_NATURAL=1
CF_RUNARGS='--timeout 1'
CF_DELAY=3
cf_variant cf-natural delayed
cf_variant mut-cf-nolatch-natural delayed 'c.failCreate(c.ctr)' '_ = 0'
redgreen "issued main create with the effect landing after the controller's own absence observations: create-failure-latch-removed mutant" a_create_failure "$MUT/mut-cf-nolatch-natural.sh" "$MUT/cf-natural.sh"
CF_NATURAL=0
CF_RUNARGS=
CF_DELAY=2
CF_EFFECT=none

# a validator whose create failed is retained as failed; its name is observed at the same seam; never-issued names are not queried.
# The before-effect, effect-before-error, timeout and delayed-effect siblings run through the real validator boundary:
# the after and delayed faults run the original `docker run` argument vector (isolation, limits, label, image, native
# `timeout --kill-after=1s 10s` and the capture-check command) with the original raw-image stdin, and receipt its
# status and canonical PNG size. Native 10 s + 1 s expiry itself is exercised by the stalled and host-loss controls.
a_validator_create_failure() { # a_validator_create_failure RUNNER (CF_EFFECT none|after|delayed, CF_HOLD and CF_NATURAL globals)
	true >"$ISSUELOG" || { OBS="the issue log could not be created"; SETUP=1; return 1; }
	rm -f "$RECEIPT" "$RECEIPT.tmp" "$RECEIPT.out" "$RECEIPT.t" "$RECEIPT.pid"
	CAP=$(capdir)
	rrlog "$ISO" "$1" --capture "$CAP" -- sh -c "${BASE}shoot_all"
	_rid=$(runid)
	_vi=boxset-dev-$_rid-v-dev-desktop
	_never=0
	for _w in dev-mobile preview-desktop preview-mobile; do
		_never=$((_never + $(grep -Fc -- "boxset-dev-$_rid-v-$_w" "$TMP/dlog")))
	done
	OBS="status $RS, $(dqueries "$_vi") queries of the issued validator, $_never log lines for never-issued names, $(grep -o 'dev-run: [a-z_]*' "$TMP/rerr" | tr '\n' ' ')"
	[ -n "$_rid" ] || { SETUP=1; return 1; }
	[ "$(head -n 1 "$ISSUELOG")" = "$_vi" ] || { OBS="$OBS; the issue seam did not observe the validator name"; SETUP=1; return 1; }
	OWNED="$OWNED $_vi"
	cf_effect "$_vi" '^0 [1-9][0-9]*$' || return 1
	[ "$CF_NATURAL" != 1 ] || cf_natural "$_vi" || { docker rm -f "$_vi" >/dev/null 2>&1; return 1; }
	cf_final "$_vi" || { docker rm -f "$_vi" >/dev/null 2>&1; return 1; }
	OBS="$OBS; final state of the issued validator before any driver removal: $_fin"
	[ "$RS" = 125 ] && grep -Fq 'dev-run: capture_copy_failed' "$TMP/rerr" && reached
	_ok=0
	[ "$RS" = 125 ] && grep -Fq 'dev-run: capture_copy_failed' "$TMP/rerr" && grep -Fq "dev-run: cleanup_failed container=$_vi" "$TMP/rerr" &&
		[ "$(dqueries "$_vi")" -ge 2 ] && [ "$_never" = 0 ] && [ ! -e "$ISO/$CAP" ] && [ "$_fin" = absent ] && _ok=1
	docker rm -f "$_vi" >/dev/null 2>&1
	[ "$_ok" = 1 ]
}
VLATCH_OLD='c.failCreate(vname) // a failed, timed-out or unknown create is retained'
vf_variant() { # vf_variant NAME KIND [OLD NEW]...: a validator-create fault copy; a held delayed cell also holds the validator's first query
	_cvn=$1
	_cvk=$2
	shift 2
	_cvt='"-challenge", c.challenge}
'"$(vf_go "$_cvk")"'
}'
	if [ "$CF_HOLD" = 1 ]; then
		gvariant "$_cvn" controller.go "$VOLD1" "$VNEW1" "$VOLD2" "$_cvt" "$QSEAM_OLD" "$QHOLD_V" "$@"
	else
		gvariant "$_cvn" controller.go "$VOLD1" "$VNEW1" "$VOLD2" "$_cvt" "$@"
	fi
}
VF_UNK_OLD='if vs := vr.status; vs < 0 || vs == 124 || vs == 125 || vs == 126 || vs == 127 || vs == 137 {'
VF_UNK_NEW='vr.status = -1 // an unknown Wait outcome
		if vs := vr.status; vs < 0 || vs == 124 || vs == 125 || vs == 126 || vs == 127 || vs == 137 {'
for vf in none after timeout delayed unknown; do
	CF_HOLD=0
	case $vf in
	none) CF_EFFECT=none ;;
	after | unknown) CF_EFFECT=after ;;
	timeout) CF_EFFECT=none ;;
	delayed) CF_EFFECT=delayed CF_HOLD=1 ;;
	esac
	if [ "$vf" = unknown ]; then
		vf_variant "vf-$vf" after "$VF_UNK_OLD" "$VF_UNK_NEW"
		vf_variant "mut-vf-nolatch-$vf" after "$VLATCH_OLD" '_ = vname' "$VF_UNK_OLD" "$VF_UNK_NEW"
		vf_variant "mut-vf-successonly-$vf" after "$SO1_OLD" "$SO1_NEW" "$SO2_OLD" "$SO2_NEW" "$VF_UNK_OLD" "$VF_UNK_NEW"
	else
		vf_variant "vf-$vf" "$vf"
		vf_variant "mut-vf-nolatch-$vf" "$vf" "$VLATCH_OLD" '_ = vname'
		vf_variant "mut-vf-successonly-$vf" "$vf" "$SO1_OLD" "$SO1_NEW" "$SO2_OLD" "$SO2_NEW"
	fi
	redgreen "issued validator create failure ($vf): latch-removed mutant" a_validator_create_failure "$MUT/mut-vf-nolatch-$vf.sh" "$MUT/vf-$vf.sh"
	redgreen "issued validator create failure ($vf): track-successful-creates-only mutant" a_validator_create_failure "$MUT/mut-vf-successonly-$vf.sh" "$MUT/vf-$vf.sh"
done
# the genuine delayed-effect sibling perturbs the validator create seam only: the controller's own queries are untouched and see
# the natural absence before the late effect lands, the effect receipt is the real original argv/stdin run, and the final state is read after
CF_HOLD=0
CF_EFFECT=delayed
CF_NATURAL=1
CF_DELAY=3
vf_variant vf-natural delayed
vf_variant mut-vf-nolatch-natural delayed "$VLATCH_OLD" '_ = vname'
redgreen "issued validator create with the effect landing after the controller's own absence observations: latch-removed mutant" a_validator_create_failure "$MUT/mut-vf-nolatch-natural.sh" "$MUT/vf-natural.sh"
CF_NATURAL=0
CF_DELAY=2
CF_EFFECT=none

# removal that took effect and then returned nonzero stays a failure although the name is later absent. The fault seam at the removal argv runs the
# real `docker rm -f`, writes its real status to the receipt RMRCPT and only then fails, so the effect is established before the refusal and is never
# attributed to the controller's final unchecked removal or to a later absence.
RMRCPT=$TMP/rm.receipt
rm_effect() { # rm_effect NAME: the real removal ran and succeeded before the injected nonzero status
	if grep -Fxq "$1 0" "$RMRCPT" 2>/dev/null; then
		OBS="$OBS; real removal receipt '$1 0'"
		return 0
	fi
	OBS="$OBS; no successful real-removal receipt for $1 ('$(tr '\n' '|' <"$RMRCPT" 2>/dev/null)')"
	SETUP=1
	return 1
}
a_rm_after_effect() { # a_rm_after_effect RUNNER
	true >"$RMRCPT" || { OBS="the removal receipt could not be created"; SETUP=1; return 1; }
	rrlog "$ISO" "$1" -- true
	_cn=$(cname)
	cstate "$_cn"
	_cst=$CST
	OBS="status $RS, container $_cn $_cst, $(grep -o 'dev-run: [a-z_]*' "$TMP/rerr" | tr '\n' ' ')"
	[ -n "$_cn" ] || { SETUP=1; return 1; }
	rm_effect "$_cn" || { docker rm -f "$_cn" >/dev/null 2>&1; return 1; }
	{ { [ "$RS" = 125 ] && grep -Fq "dev-run: cleanup_failed container=$_cn" "$TMP/rerr"; } || [ "$RS" = 0 ]; } && reached
	_ok=0
	[ "$RS" = 125 ] && grep -Fq "dev-run: cleanup_failed container=$_cn" "$TMP/rerr" && [ "$_cst" = absent ] && _ok=1
	docker rm -f "$_cn" >/dev/null 2>&1
	[ "$_ok" = 1 ]
}
RM_OLD='return []string{"timeout", "30", "docker", "rm", "-f", name}'
RM_AFTER_NEW='return []string{"/bin/sh", "-c", `timeout 30 docker rm -f "$1"; x=$?; printf "%s %s\n" "$1" "$x" >>'$RMRCPT'; exit 1`, "sh", name}'
gvariant fault-rm-after-effect controller.go "$RM_OLD" "$RM_AFTER_NEW"
gvariant mut-rm-status-ignored controller.go "$RM_OLD" "$RM_AFTER_NEW" 'return c.rmNote(name, r.startErr == nil && r.status == 0)' '_ = r
	return true'
redgreen "owned removal after effect then nonzero: removal-status-ignoring mutant" a_rm_after_effect "$MUT/mut-rm-status-ignored.sh" "$MUT/fault-rm-after-effect.sh"

# the asynchronous output-stage removal (the redactor stopped on overflow) is the controller's own synchronous owned removal: its failure survives the later absence
a_rm_async() { # a_rm_async RUNNER
	true >"$RMRCPT" || { OBS="the removal receipt could not be created"; SETUP=1; return 1; }
	rrx "$ISO" "$1" -- sh -c 'head -c 65537 /dev/zero | tr "\0" a'
	_cn=$(cname)
	cstate "$_cn"
	_cst=$CST
	OBS="status $RS, container $_cn $_cst, $(grep -o 'dev-run: [a-z_]*' "$TMP/rerr" | tr '\n' ' ')"
	[ -n "$_cn" ] || { SETUP=1; return 1; }
	rm_effect "$_cn" || { docker rm -f "$_cn" >/dev/null 2>&1; return 1; }
	[ "$RS" = 125 ] && grep -Fq 'dev-run: output_overflow' "$TMP/rerr" && reached
	_ok=0
	[ "$RS" = 125 ] && grep -Fq 'dev-run: output_overflow' "$TMP/rerr" && grep -Fq "dev-run: cleanup_failed container=$_cn" "$TMP/rerr" && [ "$_cst" = absent ] && _ok=1
	docker rm -f "$_cn" >/dev/null 2>&1
	[ "$_ok" = 1 ]
}
gvariant mut-rm-async-status-dropped controller.go "$RM_OLD" "$RM_AFTER_NEW" 'c.rmNote(s.removeOnOut, st == 0)' 'c.rmNote(s.removeOnOut, true)'
redgreen "output-stage owned removal after effect then nonzero: stage-status-dropping mutant" a_rm_async "$MUT/mut-rm-async-status-dropped.sh" "$MUT/fault-rm-after-effect.sh"

# one absolute 15 s reconciliation window shared by every issued name, read as exact CLOCK_MONOTONIC nanoseconds by the controller's one clock function when its
# removal step ends; each confirmation query is hard-bounded by the time left floored to milliseconds, no grace. The query seam of a fault copy sits at the controller's
# confirmation call and logs, per query, S (the controller's own reading, resolution charge 0, the native budget, the name, the shared deadline and the true
# reconciliation start) and, on return, E (a reading of the same clock, the name and the query's status).
QLOG=$TMP/qlog
QCNT=$TMP/qcnt
QCALLS=$TMP/qcalls
QOLD='out, ok := c.queryName(fmt.Sprintf("%d.%03d", ms/1000, ms%1000), time.Duration(ms)*time.Millisecond, n, "confirm")'
# gq ARGV [MARGIN]: the Go call-site seam: log S, run the fault argv (a Go []string over secs and n) under the hard bound of the time left plus MARGIN seconds, log E
gq() {
	printf '%s' 'out, ok := func() (string, bool) {
		secs := fmt.Sprintf("%d.%03d", ms/1000, ms%1000)
		{ f, _ := os.OpenFile("'$QLOG'", os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644); fmt.Fprintf(f, "S %d %s %s %d %d\n", at, secs, n, end, t0); f.Close() }
		r := c.do(&callSpec{kind: kReconcile, argv: '"$1"', stdout: captureFn(4096), bound: time.Duration(ms)*time.Millisecond + '"${2:-3}"'*time.Second})
		sec, nsec, _ := clockGettime()
		{ f, _ := os.OpenFile("'$QLOG'", os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644); fmt.Fprintf(f, "E %d %s %d\n", sec*1000000000+nsec, n, r.status); f.Close() }
		o, ok := r.text()
		return strings.TrimRight(o, "\n"), ok
	}()'
}
# gerr RETURN: the first confirmation query returns RETURN (a Go multi-value), later ones are the real query
gerr() {
	printf '%s' 'out, ok := func() (string, bool) {
		nb, _ := os.ReadFile("'$QCNT'")
		k, _ := strconv.Atoi(strings.TrimSpace(string(nb)))
		k++
		_ = os.WriteFile("'$QCNT'", []byte(strconv.Itoa(k)), 0o644)
		if k == 1 {
			return '"$1"'
		}
		return c.queryName(fmt.Sprintf("%d.%03d", ms/1000, ms%1000), time.Duration(ms)*time.Millisecond, n, "confirm")
	}()'
}
secs_ns() { # secs_ns SECONDS[.FRACTION]: integer nanoseconds of the controller's whole-seconds.milliseconds budget
	case $1 in '' | *[!0-9.]*)
		printf 0
		return
		;;
	esac
	_sni=${1%%.*}
	_snf=
	case $1 in *.*) _snf=${1#*.} ;; esac
	_snf=${_snf}000000000
	_snf=${_snf%"${_snf#?????????}"}
	while :; do
		case $_snf in 0?*) _snf=${_snf#0} ;; *) break ;; esac
	done
	printf '%s' "$((${_sni:-0} * 1000000000 + _snf))"
}
a_closure_window() { # a_closure_window RUNNER: one shared 15 s window, strictly; every confirmation bounded by the time left, no grace
	true >"$QLOG" || { OBS="the query log could not be created"; SETUP=1; return 1; }
	CAP=$(capdir)
	rrlog "$ISO" "$1" --capture "$CAP" -- sh -c "${BASE}shoot_all"
	[ -s "$QLOG" ] || { OBS="no confirmation query was logged, status $RS"; SETUP=1; return 1; }
	_wn=0
	_wx=0
	_wreset=0
	_wspan=0
	_wover=0
	_wearly=0
	_wbadst=0
	_wd0=
	_wsb=0
	while read -r _wk _wa _wb _wc _wd _wt; do
		case $_wk in
		S)
			_wn=$((_wn + 1))
			[ -n "$_wd0" ] || _wd0=$_wd
			[ "$_wd" = "$_wd0" ] || _wreset=$((_wreset + 1))
			[ "$((_wd - _wt))" = 15000000000 ] || _wspan=$((_wspan + 1))
			[ "$_wt" -le "$_wa" ] || _wearly=$((_wearly + 1))
			_wsb=$(secs_ns "$_wb")
			# the controller's readings are exact nanoseconds (resolution charge 0): its reading plus the native budget never exceeds the deadline
			[ "$((_wa + _wsb))" -le "$_wd" ] || _wover=$((_wover + 1))
			;;
		E)
			_wx=$((_wx + 1))
			# the completion record keeps the query's status: the native timeout's 124 or 0. The completion boundary itself is judged by the
			# scripted-clock cases below, never by a real-time scheduling allowance here
			case $_wc in 0 | 124) ;; *) _wbadst=$((_wbadst + 1)) ;; esac
			;;
		esac
	done <"$QLOG"
	OBS="status $RS, $_wn confirmation queries ($_wx completed), $_wspan deadlines not exactly 15 s after the controller's own reconciliation start, $_wreset deadline resets, $_wover queries bounded beyond the time left, $_wearly queries before the window opened, $_wbadst unexpected query statuses, $(grep -c 'dev-run: cleanup_failed container=' "$TMP/rerr") cleanup_failed lines"
	reached
	[ "$RS" = 125 ] && [ "$_wspan" = 0 ] && [ "$_wreset" = 0 ] && [ "$_wover" = 0 ] && [ "$_wearly" = 0 ] && [ "$_wbadst" = 0 ] && [ "$_wx" = "$_wn" ] &&
		grep -Fq "dev-run: cleanup_failed container=$(cname)" "$TMP/rerr" && ! grep -Fq 'DEV-RUN-CAPTURE-PASS' "$TMP/rout" && [ ! -e "$ISO/$CAP" ]
}
gvariant fault-window-stall controller.go "$QOLD" "$(gq '[]string{"timeout", secs, "sleep", "100"}')"
gvariant fault-window-slow controller.go "$QOLD" "$(gq '[]string{"timeout", secs, "sh", "-c", "sleep 3; echo $0", n}')"
gvariant mut-window-reset controller.go "$QOLD" "$(gq '[]string{"timeout", secs, "sleep", "100"}')" 'left := end - at' 'end = at + 15000000000
			left := end - at'
gvariant mut-window-fixed controller.go "$QOLD" "$(gq '[]string{"timeout", secs, "sh", "-c", "sleep 3; echo $0", n}')" 'ms := left / 1000000' 'ms := int64(15000)'
gvariant mut-window-attempts controller.go "$QOLD" "$(gq '[]string{"timeout", secs, "sh", "-c", "sleep 3; echo $0", n}')" 'end := t0 + 15000000000' 'end := t0 + 3600000000000'
gvariant mut-window-grace controller.go "$QOLD" "$(gq '[]string{"timeout", secs, "sleep", "100"}')" 'end := t0 + 15000000000' 'end := t0 + 17000000000'
gvariant mut-window-grace1 controller.go "$QOLD" "$(gq '[]string{"timeout", secs, "sleep", "100"}')" 'end := t0 + 15000000000' 'end := t0 + 16000000000'
gvariant mut-window-late controller.go "$QOLD" "$(gq '[]string{"timeout", secs, "sleep", "100"}')" 'secs := fmt.Sprintf("%d.%03d", ms/1000, ms%1000)' 'secs := fmt.Sprintf("%d", ms/1000+2)'
gvariant mut-window-late1 controller.go "$QOLD" "$(gq '[]string{"timeout", secs, "sleep", "100"}')" 'secs := fmt.Sprintf("%d.%03d", ms/1000, ms%1000)' 'secs := fmt.Sprintf("%d", ms/1000+1)'
redgreen "per-name-reset window mutant (stalled confirmation queries)" a_closure_window "$MUT/mut-window-reset.sh" "$MUT/fault-window-stall.sh"
redgreen "fixed-per-query-budget mutant (slow-present confirmation queries)" a_closure_window "$MUT/mut-window-fixed.sh" "$MUT/fault-window-slow.sh"
redgreen "attempt-count (no absolute deadline) mutant (slow-present confirmation queries)" a_closure_window "$MUT/mut-window-attempts.sh" "$MUT/fault-window-slow.sh"
redgreen "two-second-grace window mutant (stalled confirmation queries)" a_closure_window "$MUT/mut-window-grace.sh" "$MUT/fault-window-stall.sh"
redgreen "one-second-grace window mutant (stalled confirmation queries)" a_closure_window "$MUT/mut-window-grace1.sh" "$MUT/fault-window-stall.sh"
redgreen "late-return budget mutant (queries allowed two seconds past the time left)" a_closure_window "$MUT/mut-window-late.sh" "$MUT/fault-window-stall.sh"
redgreen "late-return budget mutant (queries allowed one second past the time left)" a_closure_window "$MUT/mut-window-late1.sh" "$MUT/fault-window-stall.sh"

# a failed or malformed (a successful answer that is not the exact name) confirmation query stays a failure
# although the next confirmation reports absence
a_closure_error_absence() { # a_closure_error_absence RUNNER
	rm -f "$QCNT"
	rrlog "$ISO" "$1" -- true
	_cn=$(cname)
	_qc=$(cat "$QCNT" 2>/dev/null)
	OBS="status $RS, ${_qc:-0} confirmation queries, container $_cn, $(grep -o 'dev-run: [a-z_]*' "$TMP/rerr" | tr '\n' ' ')"
	[ -n "$_cn" ] || { SETUP=1; return 1; }
	case $_qc in '' | 0 | 1) OBS="$OBS; no confirmation followed the failed one"; SETUP=1; docker rm -f "$_cn" >/dev/null 2>&1; return 1 ;; esac
	{ { [ "$RS" = 125 ] && grep -Fq "dev-run: cleanup_failed container=$_cn" "$TMP/rerr"; } || [ "$RS" = 0 ]; } && reached
	_ok=0
	cstate "$_cn"
	[ "$RS" = 125 ] && grep -Fq "dev-run: cleanup_failed container=$_cn" "$TMP/rerr" && [ "$CST" = absent ] && _ok=1
	docker rm -f "$_cn" >/dev/null 2>&1
	[ "$_ok" = 1 ]
}
gvariant fault-window-error controller.go "$QOLD" "$(gerr '"", false')"
gvariant mut-window-error-forgotten controller.go "$QOLD" "$(gerr '"", false')" 'fail(n) // a failed confirmation stays a failure' '_ = n'
redgreen "confirmation-error-forgotten mutant (first confirmation fails, the next reports absence)" a_closure_error_absence "$MUT/mut-window-error-forgotten.sh" "$MUT/fault-window-error.sh"
gvariant fault-window-malformed controller.go "$QOLD" "$(gerr '"not-the-name", true')"
gvariant mut-window-malformed-forgotten controller.go "$QOLD" "$(gerr '"not-the-name", true')" 'fail(n) // a foreign answer is not absence' '_ = n'
redgreen "malformed-success-forgotten mutant (the first confirmation prints a foreign name, the next reports absence)" a_closure_error_absence "$MUT/mut-window-malformed-forgotten.sh" "$MUT/fault-window-malformed.sh"
# a confirmation that reports absence only after the shared deadline is a failure, not a success: late by two seconds on the real
# clock here (no whole-second grace); the fractional boundaries are the scripted-clock cases below
a_closure_late() { # a_closure_late RUNNER
	rrlog "$ISO" "$1" -- true
	_cn=$(cname)
	OBS="status $RS, container $_cn, $(grep -o 'dev-run: [a-z_]*' "$TMP/rerr" | tr '\n' ' ')"
	[ -n "$_cn" ] || { SETUP=1; return 1; }
	{ { [ "$RS" = 125 ] && grep -Fq "dev-run: cleanup_failed container=$_cn" "$TMP/rerr"; } || [ "$RS" = 0 ]; } && reached
	_ok=0
	cstate "$_cn"
	[ "$RS" = 125 ] && grep -Fq "dev-run: cleanup_failed container=$_cn" "$TMP/rerr" && [ "$CST" = absent ] && _ok=1
	docker rm -f "$_cn" >/dev/null 2>&1
	[ "$_ok" = 1 ]
}
gvariant fault-window-late controller.go "$QOLD" "$(gq '[]string{"/bin/sh", "-c", "sleep \"$0\"; sleep 2", secs}' 5)"
gvariant mut-window-late-accepted controller.go "$QOLD" "$(gq '[]string{"/bin/sh", "-c", "sleep \"$0\"; sleep 2", secs}' 5)" 'if done, err := c.now(); err != nil || done >= end {' 'if done, err := c.now(); err != nil || done < 0 {'
redgreen "late-success-accepted mutant (absence reported two seconds after the shared deadline)" a_closure_late "$MUT/mut-window-late-accepted.sh" "$MUT/fault-window-late.sh"

# Deterministic clock cases through the real closure. The compiled fault replaces the raw reader of the controller's one clock function while the closure runs: it reads no
# real clock and calls no helper. Reading n counts from the closure's first reading (1 the reconciliation start t0 = 1000 s, 2 the admission of a confirmation query, 3 its
# completion, 4 the loop-continuation check, reached only while a name is still pending). Each reading is the scripted value for its number (`N NS`), a failure (`N err`),
# an invalid reading (`N bad`) or the previous reading plus one second. The exact-nanosecond completion is late at the deadline and inside one nanosecond before it; every grace
# (10 ms, 250 ms, 1 s) and the inclusive comparison are mutants of that one boundary and no case depends on process scheduling. A counting query seam that answers "present" holds
# the closure in its loop, so the exhausted admission (no query once nothing of the window remains) and the continuation failure (no further reading or query after it) are
# observed from the seam's own call count and the script's own count. The consumer's status, the publication and the container's state are observed independently of the scripted
# clock; the script's own count proves the targeted reading was reached.
CLKF=$TMP/clk.f
CLKN=$TMP/clk.n
CLKV=$TMP/clk.v
CLKT=$TMP/clk.t
CK=
CK_CALLS=0
CK_LINE=
CLKSEAM_OLD='sec, nsec, err := clockGettime()'
CLKSEAM_NEW='sec, nsec, err := c.scripted()'
CLKDEF_OLD='type callKind int'
CLKDEF_NEW='func (c *ctl) scripted() (int64, int64, error) {
	if !c.closureRan {
		return 999, 0, nil // one scripted domain for every reading of this copy: the entry reading precedes the first closure reading (1000 s)
	}
	nb, _ := os.ReadFile("'$CLKN'")
	n, _ := strconv.Atoi(strings.TrimSpace(string(nb)))
	n++
	_ = os.WriteFile("'$CLKN'", []byte(strconv.Itoa(n)), 0o644)
	vb, _ := os.ReadFile("'$CLKV'")
	v, err := strconv.ParseInt(strings.TrimSpace(string(vb)), 10, 64)
	if err != nil {
		v = 999000000000
	}
	nv := v + 1000000000
	sc, _ := os.ReadFile("'$CLKF'")
	for _, l := range strings.Split(string(sc), "\n") {
		f := strings.Fields(l)
		if len(f) != 2 || f[0] != strconv.Itoa(n) {
			continue
		}
		{ lf, _ := os.OpenFile("'$CLKT'", os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644); fmt.Fprintf(lf, "%d %s\n", n, f[1]); lf.Close() }
		switch f[1] {
		case "err":
			return 0, 0, errors.New("scripted clock failure")
		case "bad":
			return -1, 0, nil
		}
		if x, e := strconv.ParseInt(f[1], 10, 64); e == nil {
			nv = x
		}
	}
	_ = os.WriteFile("'$CLKV'", []byte(strconv.FormatInt(nv, 10)), 0o644)
	return nv / 1000000000, nv % 1000000000, nil
}

type callKind int'
QCOUNT_NEW='_ = ms
			out, ok := func() (string, bool) { f, _ := os.OpenFile("'$QCALLS'", os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644); _, _ = f.WriteString("x"); f.Close(); return n, true }()'
ck_script() { # ck_script LINES...: the scripted closure clock; a reading with no line is the previous reading plus one second. CK_LINE is the first line, the targeted reading the case must see consumed
	CK_LINE=${1:-}
	: >"$CLKF" || { SETUP=1; return 1; }
	for _ckl in "$@"; do printf '%s\n' "$_ckl" >>"$CLKF" || SETUP=1; done
}
a_clock_case() { # a_clock_case RUNNER (CK err-t0|invalid-admission|backward-completion|late-boundary|late-100|late-600|inside-boundary|exhausted-admission|err-continuation, CK_CALLS globals)
	rm -f "$CLKN" "$CLKV" "$QCALLS" "$CLKT"
	rm -rf "$ISO/testdata"
	mkdir -p "$ISO/testdata/parser"
	rrlog "$ISO" "$1" --write testdata/parser -- sh -c 'printf pub >/src/testdata/parser/series.jsonl'
	_cn=$(cname)
	cstate "$_cn"
	_cst=$CST
	_kn=$(cat "$CLKN" 2>/dev/null)
	case $_kn in '' | *[!0-9]*) _kn=0 ;; esac
	_qc=$({ wc -c <"$QCALLS"; } 2>/dev/null | tr -d ' ')
	case $_qc in '' | *[!0-9]*) _qc=0 ;; esac
	_pub=no
	[ ! -e "$ISO/testdata/parser/series.jsonl" ] || _pub=yes
	OBS="$CK: status $RS, container $_cn $_cst, clock readings $_kn (targeted reading $CK_CALLS, scripted line '$CK_LINE' consumed: $(grep -Fxc "$CK_LINE" "$CLKT" 2>/dev/null)), confirmation queries $_qc, published $_pub, $(grep -o 'dev-run: [a-z_]*' "$TMP/rerr" | tr '\n' ' ')"
	if [ -z "$_cn" ] || [ "$_kn" -lt "$CK_CALLS" ] || ! grep -Fxq "$CK_LINE" "$CLKT" 2>/dev/null; then
		SETUP=1
		[ -z "$_cn" ] || docker rm -f "$_cn" >/dev/null 2>&1
		rm -rf "$ISO/testdata"
		return 1
	fi
	reached
	_ok=0
	case $CK in
	inside-boundary)
		[ "$RS" = 0 ] && [ "$_pub" = yes ] && ! grep -Fq 'dev-run: cleanup_failed' "$TMP/rerr" && [ "$_cst" = absent ] && _ok=1
		;;
	exhausted-admission)
		[ "$RS" = 125 ] && [ "$_pub" = no ] && grep -Fq "dev-run: cleanup_failed container=$_cn" "$TMP/rerr" && [ "$_cst" = absent ] && [ "$_qc" = 0 ] && _ok=1
		;;
	err-t0)
		[ "$RS" = 125 ] && [ "$_pub" = no ] && grep -Fq "dev-run: cleanup_failed container=$_cn" "$TMP/rerr" && [ "$_cst" = absent ] && [ "$_qc" = 0 ] && [ "$_kn" = "$CK_CALLS" ] && _ok=1
		;;
	err-continuation)
		[ "$RS" = 125 ] && [ "$_pub" = no ] && grep -Fq "dev-run: cleanup_failed container=$_cn" "$TMP/rerr" && [ "$_cst" = absent ] && [ "$_qc" = 1 ] && [ "$_kn" = "$CK_CALLS" ] && _ok=1
		;;
	*)
		[ "$RS" = 125 ] && [ "$_pub" = no ] && grep -Fq "dev-run: cleanup_failed container=$_cn" "$TMP/rerr" && [ "$_cst" = absent ] && _ok=1
		;;
	esac
	docker rm -f "$_cn" >/dev/null 2>&1
	rm -rf "$ISO/testdata"
	[ "$_ok" = 1 ]
}
gclock() { # gclock NAME [Q [OLD NEW]...]: the scripted-clock fault copy (Q: with the counting query seam) plus optional mutant replacements
	_gcn=$1
	_gcq=$2
	shift 2
	if [ "$_gcq" = Q ]; then
		gvariant "$_gcn" controller.go "$CLKSEAM_OLD" "$CLKSEAM_NEW" "$CLKDEF_OLD" "$CLKDEF_NEW" "$QOLD" "$QCOUNT_NEW" "$@"
	else
		gvariant "$_gcn" controller.go "$CLKSEAM_OLD" "$CLKSEAM_NEW" "$CLKDEF_OLD" "$CLKDEF_NEW" "$@"
	fi
}
gclock fault-clock N
gclock fault-clock-q Q
gclock mut-clock-t0-ignored Q 'for err == nil && len(pend) > 0 { // no reconciliation window without a reading' '_ = err
	for len(pend) > 0 { // no reconciliation window without a reading'
gclock mut-clock-admit-ignored N 'if err != nil { // an unusable clock admits no further confirmation of any name
				pend = append(next, pend[i:]...)
				break confirm
			}' '_ = err'
gclock mut-clock-monotonic-removed N 'if v < 0 || v < c.lastClock {' 'if v < 0 {'
gclock mut-clock-resolution-grace N 'if done, err := c.now(); err != nil || done >= end {' 'if done, err := c.now(); err != nil || done >= end+10000000 {'
gclock mut-clock-grace-250 N 'if done, err := c.now(); err != nil || done >= end {' 'if done, err := c.now(); err != nil || done >= end+250000000 {'
gclock mut-clock-grace-1s N 'if done, err := c.now(); err != nil || done >= end {' 'if done, err := c.now(); err != nil || done >= end+1000000000 {'
gclock mut-clock-boundary-inclusive N 'if done, err := c.now(); err != nil || done >= end {' 'if done, err := c.now(); err != nil || done > end {'
gclock mut-clock-exhaust-ignored Q 'if left < 1000000 {' 'if false {'
gclock mut-clock-continue-ignored Q 'if err != nil || end-at <= 1000000000 {' 'if (err != nil && false) || end-at <= 1000000000 {'
CK=err-t0
CK_CALLS=1
ck_script '1 err' '2 1000000000'
redgreen "closure clock read failing at the reconciliation start: status-ignoring mutant" a_clock_case "$MUT/mut-clock-t0-ignored.sh" "$MUT/fault-clock-q.sh"
CK=invalid-admission
CK_CALLS=2
ck_script '2 bad'
redgreen "closure clock reading that is out of range at a query admission: status-ignoring mutant" a_clock_case "$MUT/mut-clock-admit-ignored.sh" "$MUT/fault-clock.sh"
CK=backward-completion
CK_CALLS=3
ck_script '3 10000000'
redgreen "closure clock stepping backward at a query completion: monotonic-check-removed mutant" a_clock_case "$MUT/mut-clock-monotonic-removed.sh" "$MUT/fault-clock.sh"
CK=late-boundary
CK_CALLS=3
ck_script '3 1015000000000'
redgreen "closure completion read exactly at the deadline (late, resolution charge 0): 10 ms resolution-grace mutant" a_clock_case "$MUT/mut-clock-resolution-grace.sh" "$MUT/fault-clock.sh"
redgreen "closure completion read exactly at the deadline (late): inclusive-comparison mutant" a_clock_case "$MUT/mut-clock-boundary-inclusive.sh" "$MUT/fault-clock.sh"
CK=late-100
CK_CALLS=3
ck_script '3 1015100000000'
redgreen "closure completion read 15.10 s after the start (a tenth of a second late): 250 ms-grace mutant" a_clock_case "$MUT/mut-clock-grace-250.sh" "$MUT/fault-clock.sh"
CK=late-600
CK_CALLS=3
ck_script '3 1015600000000'
redgreen "closure completion read 15.60 s after the start (six tenths of a second late): one-second-grace mutant" a_clock_case "$MUT/mut-clock-grace-1s.sh" "$MUT/fault-clock.sh"
CK=inside-boundary
CK_CALLS=3
ck_script '3 1014999999999'
holds "closure completion read one nanosecond before the deadline is inside: admitted, published and the container closed" a_clock_case "$MUT/fault-clock.sh"
# a remaining budget under 1 ms at a query admission starts no query, and a clock failure at the loop-continuation reading ends the loop
CK=exhausted-admission
CK_CALLS=2
ck_script '2 1014999500000'
redgreen "closure query admission with 0.5 ms of the window left: admission-guard-removed mutant starts a query" a_clock_case "$MUT/mut-clock-exhaust-ignored.sh" "$MUT/fault-clock-q.sh"
CK=err-continuation
CK_CALLS=4
ck_script '4 err'
redgreen "closure clock reading failing at the loop-continuation check with a name still pending: status-ignoring mutant keeps looping" a_clock_case "$MUT/mut-clock-continue-ignored.sh" "$MUT/fault-clock-q.sh"

# a clock that fails or steps backward ends the whole confirmation phase for every outstanding name. Two names are issued (the container and the first validator, which a capture whose child leaves empty files
# reaches without a browser). Readings count as above: 2 and 3 the admission and completion of the first name, 4 and 5 those of the second. The first name is confirmed by one query; the failure then admits no
# further confirmation of either name, and both stay failed.
a_clock_multi() { # a_clock_multi RUNNER (CK, CK_CALLS and CK_LINE globals)
	rm -f "$CLKN" "$CLKV" "$QCALLS" "$CLKT"
	CAP=$(capdir)
	rrlog "$ISO" "$1" --capture "$CAP" -- sh -c true
	_cn=$(cname)
	_vn=${_cn}-v-dev-desktop
	cstate "$_cn"
	_cst=$CST
	cstate "$_vn"
	_vst=$CST
	_kn=$(cat "$CLKN" 2>/dev/null)
	case $_kn in '' | *[!0-9]*) _kn=0 ;; esac
	_qc=$({ wc -c <"$QCALLS"; } 2>/dev/null | tr -d ' ')
	case $_qc in '' | *[!0-9]*) _qc=0 ;; esac
	_runs=$(grep -Ec '^S [^ ]+ [0-9]+\.[0-9][0-9] run ' "$TMP/dlog")
	OBS="$CK: status $RS, names $_cn $_cst and $_vn $_vst, $_runs run records, clock readings $_kn (targeted reading $CK_CALLS, scripted line '$CK_LINE' consumed: $(grep -Fxc "$CK_LINE" "$CLKT" 2>/dev/null)), confirmation queries $_qc, $(grep -o 'dev-run: [a-z_]*' "$TMP/rerr" | tr '\n' ' ')"
	if [ -z "$_cn" ] || [ "$_runs" != 2 ] || [ "$_kn" -lt "$CK_CALLS" ] || ! grep -Fxq "$CK_LINE" "$CLKT" 2>/dev/null; then
		SETUP=1
		[ -z "$_cn" ] || docker rm -f "$_cn" "$_vn" >/dev/null 2>&1
		rm -rf "$ISO/$CAP"
		return 1
	fi
	reached
	_ok=0
	[ "$RS" = 125 ] && grep -Fq "dev-run: cleanup_failed container=$_cn" "$TMP/rerr" && grep -Fq "dev-run: cleanup_failed container=$_vn" "$TMP/rerr" &&
		[ "$_qc" = 1 ] && [ "$_kn" = "$CK_CALLS" ] && [ "$_cst" = absent ] && [ "$_vst" = absent ] && _ok=1
	docker rm -f "$_cn" "$_vn" >/dev/null 2>&1
	rm -rf "$ISO/$CAP"
	[ "$_ok" = 1 ]
}
MULTI_ADM_OLD='if err != nil { // an unusable clock admits no further confirmation of any name
				pend = append(next, pend[i:]...)
				break confirm
			}'
MULTI_ADM_NEW='if err != nil {
				fail(n)
				continue
			}'
MULTI_CMP_OLD='fail(n)
				if err != nil {
					pend = append(next, pend[i:]...)
					break confirm
				}
				next = append(next, n)'
MULTI_CMP_NEW='fail(n)
				next = append(next, n)'
gclock mut-clock-multi-admit Q "$MULTI_ADM_OLD" "$MULTI_ADM_NEW"
gclock mut-clock-multi-cmp Q "$MULTI_CMP_OLD" "$MULTI_CMP_NEW"
CK=multi-admission-err
CK_CALLS=4
ck_script '4 err'
redgreen "closure clock failing at the second name's admission with later valid readings: the per-name-continue mutant keeps confirming" a_clock_multi "$MUT/mut-clock-multi-admit.sh" "$MUT/fault-clock-q.sh"
CK=multi-completion-err
CK_CALLS=3
ck_script '3 err'
redgreen "closure clock failing at the first name's completion with later valid readings: the continue mutant admits the second name" a_clock_multi "$MUT/mut-clock-multi-cmp.sh" "$MUT/fault-clock-q.sh"
CK=multi-backward
CK_CALLS=4
ck_script '4 10000000'
redgreen "closure clock stepping backward at the second name's admission: the per-name-continue mutant keeps reading" a_clock_multi "$MUT/mut-clock-multi-admit.sh" "$MUT/fault-clock-q.sh"

# stalled validator: killed by the in-container 10 s + 1 s, within the 30 s budget
a_stalled_validator() { # holds when a stalled validator is killed by its own native 10 s + 1 s and the run ends within the shared 30 s transfer budget. Both intervals start at the logging shim's own reading before the validator's
	# docker run (the effect), never at a later first observation, so each reported duration really is an upper bound of the lifetime it names.
	CAP=$(capdir)
	bg_runlog stall "$1" --capture "$CAP" -- sh -c "${BASE}shoot_all"
	_c=$(bg_ctr stall) || { OBS="no container"; SETUP=1; return 1; }
	_vn=boxset-dev-${_c#boxset-dev-}-v-dev-desktop
	_seen=0
	_tg=0
	_i=0
	while kill -0 "$BG_PID" 2>/dev/null && [ "$_i" -lt 600 ]; do
		cstate "$_vn"
		if [ "$CST" = present ]; then
			_seen=1
		elif [ "$CST" = absent ] && [ "$_seen" = 1 ] && [ "$_tg" = 0 ]; then
			mono_ns && _tg=$MONO_NS
		fi
		sleep 1
		_i=$((_i + 1))
	done
	mono_ns || { OBS="the monotonic clock could not be read at the end of the run"; SETUP=1; bg_end stall; return 1; }
	_t2=$MONO_NS
	bg_end stall
	dlogs_same stall || return 1
	_vs=
	_vl=$(grep -E -- "^S [^ ]+ [0-9]+\.[0-9][0-9] run .* --name $_vn " "$TMP/stall.dlog" 2>/dev/null)
	if [ "$(printf '%s\n' "$_vl" | grep -c .)" = 1 ]; then
		set -- $_vl
		mono_parse "$3" && _vs=$MONO_NS
	fi
	[ -n "$_vs" ] || { OBS="no single shim run record of the validator, so no interval can start before its effect"; SETUP=1; return 1; }
	[ "$_seen" = 1 ] && [ "$_tg" != 0 ] || { OBS="the validator was never seen present and then absent"; SETUP=1; return 1; }
	_dur=$((_t2 + MONO_RES - _vs))
	_vlife=$((_tg + MONO_RES - _vs))
	OBS="status $RS, from the validator's own run record: the run ended within $((_dur / 1000000)) ms and the validator was first seen gone within $((_vlife / 1000000)) ms (upper bounds)"
	{ { [ "$RS" = 125 ] && grep -Fq 'dev-run: capture_copy_failed' "$TMP/rerr"; } || [ "$_dur" -gt 30000000000 ] || [ "$_vlife" -gt 20000000000 ]; } && reached
	cstate "$_vn"
	[ "$_dur" -le 30000000000 ] && [ "$_vlife" -le 20000000000 ] && [ "$RS" = 125 ] && grep -Fq 'dev-run: capture_copy_failed' "$TMP/rerr" && [ "$CST" = absent ]
}

gvariant fault-stalled controller.go '"/usr/local/libexec/boxset/capture-check", "-name", name, "-challenge", c.challenge}' '"/usr/bin/sleep", "100"}'
gvariant mut-stalled-unbounded controller.go '"/usr/local/libexec/boxset/capture-check", "-name", name, "-challenge", c.challenge}' '"/usr/bin/sleep", "100"}' '"timeout", "--kill-after=2s", strconv.Itoa(left) + "s", "docker", "run",' '"docker", "run",' '"/usr/bin/timeout", "--kill-after=1s", "10s", ' '"/usr/bin/env", ' 'bound: validatorBound(left),' 'bound: time.Hour,'
redgreen "stalled validator (native timeouts removed mutant runs past the 30 s budget)" a_stalled_validator "$MUT/mut-stalled-unbounded.sh" "$MUT/fault-stalled.sh"
# only the in-container timeout removed: the outer wrapper alone ends the validator, at the transfer budget rather than at its own native 10 s + 1 s
gvariant mut-stalled-inner-removed controller.go '"/usr/local/libexec/boxset/capture-check", "-name", name, "-challenge", c.challenge}' '"/usr/bin/sleep", "100"}' '"/usr/bin/timeout", "--kill-after=1s", "10s", ' '"/usr/bin/env", '
redgreen "stalled validator (inner in-container timeout removed alone: only the outer wrapper ends it, at the transfer budget)" a_stalled_validator "$MUT/mut-stalled-inner-removed.sh" "$MUT/fault-stalled.sh"

# F1: acquired-object authority (DIR and feature-parent swaps, first-part rename)
swap_control() { # swap_control RUNNER MODE: sets RS and CAP; MODE dir | parent
	CAP=$(capdir)
	rg_reset
	bg_run swapc "$1" --capture "$CAP" -- sh -c "${BASE}shoot_all; $CBAR"
	_c=$(bg_ctr swapc) || { RS=125; SETUP=1; return 1; }
	if bg_wait "$_c" /tmp/ready; then
		reached
		case $2 in
		dir)
			mv "$ISO/$CAP" "$ISO/$CAP.moved"
			ln -s "$CANARY" "$ISO/$CAP"
			;;
		parent)
			mv "$ISO/.devrites/work/boxset-test" "$ISO/.devrites/work/boxset-test.moved"
			ln -s "$CANARY" "$ISO/.devrites/work/boxset-test"
			;;
		esac
		bg_touch "$_c" /tmp/go || SETUP=1
	else
		SETUP=1
		kill -KILL "$BG_PID" 2>/dev/null
	fi
	bg_end swapc
}
CS_BEFORE=$(canary_state)
swap_control "$RUNNER" dir
if [ "$RS" = 125 ] && grep -Fq 'dev-run: capture_root_changed' "$TMP/rerr"; then pass "DIR swapped for a symlink mid-run gives capture_root_changed"; else fail "DIR swap" "status $RS"; fi
expect "canary state unchanged (DIR swap)" "$(canary_state)" "$CS_BEFORE"
expect "moved object holds nothing (DIR swap)" "$(find "$ISO/$CAP.moved" -mindepth 1 | wc -l | tr -d ' ')" 0
rm -rf "$ISO/$CAP.moved"
rm -f "$ISO/$CAP"
CS_BEFORE=$(canary_state)
swap_control "$RUNNER" parent
if [ "$RS" = 125 ] && grep -Fq 'dev-run: capture_root_changed' "$TMP/rerr"; then pass "feature parent .devrites/work/<slug> swapped for a symlink gives capture_root_changed"; else fail "parent swap" "status $RS"; fi
expect "canary state unchanged (parent swap)" "$(canary_state)" "$CS_BEFORE"
if find "$CANARY" -name '*.png' -o -name 'capture.manifest' -o -name '.*.part' | grep -q .; then fail "parent swap canary" "capture bytes reached the canary"; else pass "no capture bytes reached the canary under a parent swap"; fi
rm -f "$ISO/.devrites/work/boxset-test"
mv "$ISO/.devrites/work/boxset-test.moved" "$ISO/.devrites/work/boxset-test"

# moving DIR outside after the first complete .part (fault-copy barrier). The barrier loops are
# bounded and record their subshell pid so a forced runner loss can reap exactly that helper.
a_dir_moved_at_transfer() { # holds when DIR moved away after the first part is capture_root_changed
	rm -rf "$TMP/moved-out"
	find "$BAR" -mindepth 1 -delete
	CAP=$(capdir)
	bg_run movd "$1" --capture "$CAP" -- sh -c "${BASE}shoot_all"
	_i=0
	while [ ! -e "$BAR/ready" ] && [ "$_i" -lt 900 ]; do
		kill -0 "$BG_PID" 2>/dev/null || break
		sleep 1
		_i=$((_i + 1))
	done
	[ -e "$BAR/ready" ] || { OBS="barrier never reached"; SETUP=1; bg_end movd; return 1; }
	mv "$ISO/$CAP" "$TMP/moved-out" || { OBS="the approved DIR could not be moved"; SETUP=1; : >"$BAR/go"; bg_end movd; return 1; }
	: >"$BAR/go"
	bg_end movd
	_left=$(find "$TMP/moved-out" -mindepth 1 | wc -l | tr -d ' ')
	OBS="status $RS, moved object holds $_left entries, approved path $([ -e "$ISO/$CAP" ] && echo present || echo absent)"
	{ { [ "$RS" = 125 ] && grep -Fq 'dev-run: capture_root_changed' "$TMP/rerr"; } || { [ "$RS" = 0 ] && grep -Fq 'DEV-RUN-CAPTURE-PASS' "$TMP/rout"; }; } && reached
	_ok=0
	[ "$RS" = 125 ] && grep -Fq 'dev-run: capture_root_changed' "$TMP/rerr" && ! grep -Fq 'DEV-RUN-CAPTURE-PASS' "$TMP/rout" && [ "$_left" = 0 ] && [ ! -e "$ISO/$CAP" ] && _ok=1
	rm -rf "$TMP/moved-out"
	[ "$_ok" = 1 ]
}
gvariant fault-dir-barrier controller.go 'sum += pr.n // staged and checked' 'sum += pr.n // staged and checked
		if name == "dev-desktop" { '"$(gobar 180)"' }'
holds "DIR moved outside after the first .part: capture_root_changed, no PASS, moved object emptied, approved path absent" a_dir_moved_at_transfer "$MUT/fault-dir-barrier.sh"

# Acquisition authority, observed at the first-write seam. The seam is the first producer of any
# staged byte (the pipeline's output file is opened with it), so the assertion looks into the
# pre-swapped object while the runner is held there, before any cleanup can erase a write. The
# original must never reach the seam; the acquisition-check-removed mutant reaches it with a
# staged file already in the moved object. The final-authority check has its own control below.
FIRSTWRITE_OLD='// the staged raw file exists; the exporter has not run'
gvariant fault-first-write controller.go "$FIRSTWRITE_OLD" "$FIRSTWRITE_OLD
		$(gobar 60)"
gvariant mut-no-acquisition-check controller.go "$FIRSTWRITE_OLD" "$FIRSTWRITE_OLD
		$(gobar 60)" 'if !c.captureAuthority() {' 'if false {'
a_pre_swapped_acquisition() { # holds when a DIR swapped before the first write is refused with zero writes into the moved object
	rm -rf "$TMP/pre-moved"
	find "$BAR" -mindepth 1 -delete
	find "$ISO/.devrites/work/boxset-test" -maxdepth 1 -name 'screenshots-*' -exec rm -rf {} +
	CAP=$(capdir)
	bg_run preswap "$1" --capture "$CAP" -- sh -c "${BASE}shoot_all; $CBAR"
	_c=$(bg_ctr preswap) || { OBS="no container"; SETUP=1; return 1; }
	bg_wait "$_c" /tmp/ready || { OBS="not ready"; SETUP=1; kill -KILL "$BG_PID" 2>/dev/null; return 1; }
	{ mv "$ISO/$CAP" "$TMP/pre-moved" && mkdir "$ISO/$CAP"; } || { OBS="the pre-swap could not be performed"; SETUP=1; bg_touch "$_c" /tmp/go; bg_end preswap; return 1; }
	bg_touch "$_c" /tmp/go || { OBS="the child could not be released"; SETUP=1; kill -KILL "$BG_PID" 2>/dev/null; return 1; }
	_i=0
	while [ ! -e "$BAR/ready" ] && kill -0 "$BG_PID" 2>/dev/null && [ "$_i" -lt 300 ]; do
		sleep 1
		_i=$((_i + 1))
	done
	_reached=no
	_seen=0
	if [ -e "$BAR/ready" ]; then
		_reached=yes
		_seen=$(find "$TMP/pre-moved" -mindepth 1 | wc -l | tr -d ' ')
		: >"$BAR/go"
	fi
	bg_end preswap
	OBS="status $RS, first-write seam reached: $_reached, entries in the moved object at that seam: $_seen"
	{ { [ "$RS" = 125 ] && grep -Fq 'dev-run: capture_root_changed' "$TMP/rerr"; } || [ "$_reached" = yes ]; } && reached
	_ok=0
	[ "$RS" = 125 ] && grep -Fq 'dev-run: capture_root_changed' "$TMP/rerr" && [ "$_reached" = no ] && [ "$_seen" = 0 ] && _ok=1
	rm -rf "$TMP/pre-moved" "$ISO/$CAP"
	[ "$_ok" = 1 ]
}
redgreen "acquisition-check-removed mutant stages a file in the pre-swapped object (observed at the first-write seam)" a_pre_swapped_acquisition "$MUT/mut-no-acquisition-check.sh" "$MUT/fault-first-write.sh"
gvariant mut-no-final-check controller.go 'sum += pr.n // staged and checked' 'sum += pr.n // staged and checked
		if name == "dev-desktop" { '"$(gobar 180)"' }' 'if !c.captureAuthority() {' 'if !c.captureAuthorityKEEP() {' 'if !c.captureAuthority() {' 'if false {' 'if !c.captureAuthority() {' 'if false {' 'captureAuthorityKEEP' 'captureAuthority'
a_final_authority() { # holds when a DIR moved at the transfer barrier never publishes into the moved object
	a_dir_moved_at_transfer "$1"
}
redgreen "final-check-removed mutant publishes into the moved object" a_final_authority "$MUT/mut-no-final-check.sh" "$MUT/fault-dir-barrier.sh"

# ===== 9b. the native lifecycle controller: cancellation, repeated and inherited signals, bootstrap and exec, relative PATH and TMPDIR (T02/T26/T31, B05/B08) ======
# Positions: C is the container, V1..V4 the validators of dev-desktop, dev-mobile, preview-desktop and preview-mobile. Every cell runs through the logging shim: $TMP/cc.dlog
# holds the S records of every Docker call and $TMP/cc.dlog.q the E records of every `ps`; "after the signal" means the DLOG lines past the line count taken when the signal is sent.
# A compiled barrier sits at a real statement of the controller's start primitive: the controller's PID goes to $BAR/helper.pid, $BAR/ready is created and $BAR/go releases it.
CC_NAME=cc
CC_KIND=
CC_POS=
CC_ACK=ack
CC_DELAY=0
CC_NOACK_SLEEP=1
CC_BAD=
CC_RID=
SD_SIG=
SD_MODE=
RS_KIND=
BOOT_MODE=
pos_index() { case $1 in C) printf 0 ;; V1) printf 1 ;; V2) printf 2 ;; V3) printf 3 ;; V4) printf 4 ;; esac; }
pos_name() { # pos_name RID POS
	case $2 in
	C) printf 'boxset-dev-%s' "$1" ;;
	V1) printf 'boxset-dev-%s-v-dev-desktop' "$1" ;;
	V2) printf 'boxset-dev-%s-v-dev-mobile' "$1" ;;
	V3) printf 'boxset-dev-%s-v-preview-desktop' "$1" ;;
	V4) printf 'boxset-dev-%s-v-preview-mobile' "$1" ;;
	esac
}
pos_cond() { # the Go condition that matches the call issuing POS's name
	case $1 in
	C) printf 's.issue == c.ctr' ;;
	V1) printf 'strings.HasSuffix(s.issue, "-v-dev-desktop")' ;;
	V2) printf 'strings.HasSuffix(s.issue, "-v-dev-mobile")' ;;
	V3) printf 'strings.HasSuffix(s.issue, "-v-preview-desktop")' ;;
	V4) printf 'strings.HasSuffix(s.issue, "-v-preview-mobile")' ;;
	esac
}
pos_hold() { # the shim pattern that matches the real run of POS
	case $1 in
	C) printf 'run -d --rm --name' ;;
	V1) printf -- '-v-dev-desktop' ;;
	V2) printf -- '-v-dev-mobile' ;;
	V3) printf -- '-v-preview-desktop' ;;
	V4) printf -- '-v-preview-mobile' ;;
	esac
}
CC_START_OLD='func (c *ctl) start(s *callSpec) (*proc, error) {
	if s.kind != kReconcile && c.cancelled.Load() {'
CC_GAP_OLD='cmd := c.command(s.argv)'
CC_CHECK_OLD='if s.kind != kReconcile && c.cancelled.Load() {'
# the three insertion points: before N's start checkpoint (pre), between the checkpoint and the start (gap), and a non-existent argv0 for N's start alone (startfail)
cc_pre_new() { printf 'func (c *ctl) start(s *callSpec) (*proc, error) {\n\tif %s {\n\t\t%s\n\t}\n\tif s.kind != kReconcile && c.cancelled.Load() {' "$(pos_cond "$1")" "$(gobar 180)"; }
cc_gap_new() { printf 'if %s {\n\t\t%s\n\t}\n\tcmd := c.command(s.argv)' "$(pos_cond "$1")" "$(gobar 180)"; }
cc_sf_new() { printf 'argv := s.argv\n\tif %s {\n\t\targv = []string{"/nonexistent/dev-run-argv0"}\n\t}\n\tcmd := c.command(argv)' "$(pos_cond "$1")"; }
cc_build() { # cc_build NAME pre|gap|startfail POS [OLD NEW]...: the compiled copy of one cell
	_cbn=$1
	_cbk=$2
	_cbp=$3
	shift 3
	case $_cbk in
	pre) gvariant "$_cbn" controller.go "$CC_START_OLD" "$(cc_pre_new "$_cbp")" "$@" ;;
	gap) gvariant "$_cbn" controller.go "$CC_GAP_OLD" "$(cc_gap_new "$_cbp")" "$@" ;;
	startfail) gvariant "$_cbn" controller.go "$CC_GAP_OLD" "$(cc_sf_new "$_cbp")" "$@" ;;
	esac
}
cc_wait_file() { # cc_wait_file FILE: FILE exists (a barrier or hold was reached) while the runner lives, within 900 s
	_cwi=0
	while [ ! -e "$1" ] && kill -0 "$BG_PID" 2>/dev/null && [ "$_cwi" -lt 900 ]; do
		sleep 1
		_cwi=$((_cwi + 1))
	done
	[ -e "$1" ]
}
cc_ack() { # cc_ack SECONDS: the controller's existing interrupted line is on its stderr
	_cai=0
	while [ "$(grep -c '^dev-run: interrupted$' "$TMP/$CC_NAME.err" 2>/dev/null)" = 0 ] && [ "$_cai" -lt "$1" ]; do
		sleep 1
		_cai=$((_cai + 1))
	done
	[ "$(grep -c '^dev-run: interrupted$' "$TMP/$CC_NAME.err" 2>/dev/null)" -ge 1 ]
}
cc_abort() { # cc_abort: end a stuck case, its process group included, and collect it
	kill -s KILL -- "-$BG_PID" 2>/dev/null
	kill -KILL "$BG_PID" 2>/dev/null
	wait "$BG_PID" 2>/dev/null || true
}
cc_setup() { # cc_setup MESSAGE: an unestablished premise is setup, never target evidence
	OBS="$OBS; $1"
	SETUP=1
	: >"$BAR/go"
	: >"$BAR/hold.go"
	: >"$BAR/consumer.go"
	sleep 1
	cc_abort
	bg_end "$CC_NAME"
}
cc_rid() { # cc_rid: CC_RID is the run id of the case, from the shim's first owned name or else the controller's own argv
	CC_RID=$(grep -o -m1 'boxset-dev-[0-9a-f]\{16\}' "$TMP/$CC_NAME.dlog" 2>/dev/null | head -n 1)
	CC_RID=${CC_RID#boxset-dev-}
	if [ -z "$CC_RID" ]; then CC_RID=$(tr '\0' '\n' <"/proc/$BG_PID/cmdline" 2>/dev/null | sed -n 4p); fi
	case $CC_RID in
	[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]) ;;
	*) CC_RID= ;;
	esac
}
cc_names() { # the owned names of the issued positions, in the DLOG's run order
	grep -E '^S [^ ]+ [0-9]+\.[0-9][0-9] run ' "$TMP/$CC_NAME.dlog" 2>/dev/null | sed -n 's/.* --name \([^ ]*\) .*/\1/p'
}
cc_named() { # cc_named NAME: the number of DLOG records naming exactly NAME
	_cnn=0
	while read -r _k _t _s _r; do
		case " $_r " in *" $1 "* | *"=^$1\$ "*) _cnn=$((_cnn + 1)) ;; esac
	done <"$TMP/$CC_NAME.dlog"
	printf '%s' "$_cnn"
}
cc_runs() { # cc_runs NAME: the number of `run` records naming NAME
	_cnr=0
	while read -r _k _t _s _r; do
		case " $_r " in *" run "*" --name $1 "*) _cnr=$((_cnr + 1)) ;; esac
	done <"$TMP/$CC_NAME.dlog"
	printf '%s' "$_cnr"
}
cc_cmds() { # cc_cmds ps|rm|run: the number of records of that Docker command
	_cnc=0
	while read -r _k _t _s _r; do
		case $_r in "$1 "*) _cnc=$((_cnc + 1)) ;; esac
	done <"$TMP/$CC_NAME.dlog"
	printf '%s' "$_cnc"
}
cc_psfirst() { # cc_psfirst NAME...: the first ps queries of the case are exactly NAME... in order (the first-phase pass of the closure)
	_cpw=$*
	_cpg=$(grep -E '^S [^ ]+ [0-9]+\.[0-9][0-9] ps -a --filter name=\^' "$TMP/$CC_NAME.dlog" | sed -n 's/.*name=^\([^ ]*\)\$ --format.*/\1/p' | head -n "$#" | tr '\n' ' ')
	[ "${_cpg% }" = "${_cpw% }" ]
}
cc_after() { # cc_after SIGN [ALLOWED_RUN]: every DLOG record past line SIGN is an exact-name ps or an rm -f of an issued name, except the one explicitly committed run ALLOWED_RUN
	tail -n +"$(($1 + 1))" "$TMP/$CC_NAME.dlog" >"$TMP/cc.after"
	_cab=0
	_cau=0
	_cal=$(cc_names | tr '\n' ' ')
	while read -r _ak _at _as _ar; do
		case $_ar in
		"ps -a --filter name=^"*'$ --format {{.Names}}')
			_cx=${_ar#"ps -a --filter name=^"}
			_cx=${_cx%'$ --format {{.Names}}'}
			;;
		"rm -f "*) _cx=${_ar#"rm -f "} ;;
		"run "*" --name "*)
			_cx=${_ar#*" --name "}
			_cx=${_cx%% *}
			if [ -n "${2:-}" ] && [ "$_cx" = "$2" ] && [ "$_cau" = 0 ]; then
				_cau=1
				continue
			fi
			_cab=$((_cab + 1))
			continue
			;;
		*)
			_cab=$((_cab + 1))
			continue
			;;
		esac
		case " $_cal " in *" $_cx "*) ;; *) _cab=$((_cab + 1)) ;; esac
	done <"$TMP/cc.after"
	[ "$_cab" = 0 ]
}
cc_procs() { # cc_procs RID: the number of live host processes whose cmdline holds the run id
	_cpn=0
	for _cpf in $(find /proc -maxdepth 2 -name cmdline -path '/proc/[0-9]*/cmdline' 2>/dev/null); do
		case $(tr '\0' ' ' <"$_cpf" 2>/dev/null) in *"$1"*) _cpn=$((_cpn + 1)) ;; esac
	done
	printf '%s' "$_cpn"
}
cc_bad() { CC_BAD="$CC_BAD $1"; }
# cc_handled SIGN [ALLOWED_RUN]: the assertions every handled-cancellation cell makes
cc_handled() {
	[ "$RS" = 125 ] || cc_bad "status-$RS"
	[ "$(grep -c '^dev-run: interrupted$' "$TMP/rerr")" = 1 ] || cc_bad "interrupted-lines-$(grep -c '^dev-run: interrupted$' "$TMP/rerr")"
	! grep -Fq DEV-RUN-CAPTURE-PASS "$TMP/rout" || cc_bad capture-pass
	[ -z "$CAP" ] || [ ! -e "$ISO/$CAP" ] || cc_bad capture-dir-present
	[ "$(find "$ISO/testdata" "$ISO/docs/spikes/issue-drafts" 2>/dev/null | LC_ALL=C sort | tr '\n' ' ')" = "$_ccm0" ] || cc_bad host-map-names-changed
	cc_after "$1" "${2:-}" || cc_bad work-after-the-signal
}
# cc_final: every issued name is absent by a final status-checked driver query read before any driver removal; no host process holds the run id
cc_final() {
	for _cfn in $(cc_names); do
		OWNED="$OWNED $_cfn"
		cstate "$_cfn"
		[ "$CST" = absent ] || cc_bad "present-$_cfn"
	done
	[ -z "$CC_RID" ] || [ "$(cc_procs "$CC_RID")" = 0 ] || cc_bad host-process-left
	for _cfn in $(cc_names); do docker rm -f "$_cfn" >/dev/null 2>&1; done
}
cc_later() { # cc_later IDX: the number of DLOG records naming a position at or after IDX
	_clt=0
	for _clp in C V1 V2 V3 V4; do
		[ "$(pos_index "$_clp")" -ge "$1" ] || continue
		_clt=$((_clt + $(cc_named "$(pos_name "$CC_RID" "$_clp")")))
	done
	printf '%s' "$_clt"
}
cc_earlier() { # cc_earlier IDX: the names of the positions before IDX, in order
	for _cep in C V1 V2 V3 V4; do
		[ "$(pos_index "$_cep")" -lt "$1" ] || continue
		pos_name "$CC_RID" "$_cep"
		printf ' '
	done
}
a_cc() { # a_cc RUNNER (CC_KIND pre|gap|inflight|group|nativegrp|startfail|preissue, CC_POS, CC_ACK ack|noack, CC_DELAY 0|1)
	CC_NAME=cc
	CC_BAD=
	CC_RID=
	find "$BAR" -mindepth 1 -delete
	_ccm0=$(find "$ISO/testdata" "$ISO/docs/spikes/issue-drafts" 2>/dev/null | LC_ALL=C sort | tr '\n' ' ')
	_ccr=$1
	CAP=
	if [ "$CC_POS" = C ]; then
		set -- -- true
	else
		CAP=$(capdir)
		set -- --capture "$CAP" -- sh -c "${BASE}shoot_all"
	fi
	DSHIM_HOLD=
	case $CC_KIND in inflight | group | nativegrp) DSHIM_HOLD=$(pos_hold "$CC_POS") ;; esac
	if [ "$CC_KIND" = group ]; then bg_runlog cc timeout 900 "$_ccr" "$@"; else bg_runlog cc "$_ccr" "$@"; fi
	DSHIM_HOLD=
	_ccs=0
	case $CC_KIND in
	startfail | preissue) bg_end cc ;;
	*)
		case $CC_KIND in pre | gap) _ccw=$BAR/ready ;; *) _ccw=$BAR/hold.ready ;; esac
		if ! cc_wait_file "$_ccw"; then
			cc_setup "the barrier or hold was never reached"
			return 1
		fi
		cc_rid
		_ccs=$(wc -l <"$TMP/cc.dlog" | tr -d ' ')
		case $CC_KIND in
		pre | gap)
			kill -s TERM "$BG_PID"
			if [ "$CC_DELAY" = 1 ]; then
				if ! cc_wait_file "$BAR/consumer.ready"; then
					cc_setup "the delayed consumer was never held"
					return 1
				fi
				if [ "$(grep -c '^dev-run: interrupted$' "$TMP/cc.err")" != 0 ]; then
					cc_setup "the acknowledgement appeared while the consumer was held"
					return 1
				fi
				if [ "$CC_ACK" = noack ]; then
					: >"$BAR/go"
					sleep "$CC_NOACK_SLEEP"
					: >"$BAR/consumer.go"
				else
					: >"$BAR/consumer.go"
					if ! cc_ack 30; then
						cc_setup "no acknowledgement after the consumer was released"
						return 1
					fi
					kill -0 "$BG_PID" 2>/dev/null || cc_bad owner-not-alive
					: >"$BAR/go"
				fi
			else
				if ! cc_ack 30; then
					cc_setup "no acknowledgement of the handled signal"
					return 1
				fi
				: >"$BAR/go"
			fi
			;;
		inflight | group)
			if [ "$CC_KIND" = group ]; then kill -s TERM -- "-$BG_PID"; else kill -s TERM "$BG_PID"; fi
			if ! cc_ack 30; then
				cc_setup "no acknowledgement of the handled signal"
				return 1
			fi
			sleep 2
			kill -0 "$BG_PID" 2>/dev/null || cc_bad owner-died-in-flight
			[ "$(wc -l <"$TMP/cc.dlog" | tr -d ' ')" = "$_ccs" ] || cc_bad new-record-while-held
			if [ "$CC_KIND" = group ]; then kill -0 "$(cat "$BAR/hold.pid" 2>/dev/null)" 2>/dev/null || cc_bad native-group-gone; fi
			: >"$BAR/hold.go"
			;;
		nativegrp)
			_ccp=$(cat "$BAR/hold.pid" 2>/dev/null)
			case $_ccp in
			'' | *[!0-9]*)
				cc_setup "no native process-group leader was recorded"
				return 1
				;;
			esac
			if ! kill -s TERM -- "-$_ccp"; then
				cc_setup "the native group could not be signalled"
				return 1
			fi
			sleep 3
			: >"$BAR/hold.go"
			;;
		esac
		bg_end cc
		;;
	esac
	dlogs_same cc || return 1
	[ -n "$CC_RID" ] || cc_rid
	_cci=$(pos_index "$CC_POS")
	_ccn=$(pos_name "$CC_RID" "$CC_POS")
	case $CC_KIND in
	startfail | preissue)
		if [ "$CC_POS" = C ]; then grep -Fq -- '--target work' "$TMP/cc.dlog" && reached; else grep -Eq "$RUNNER_STARTED_RE" "$TMP/rerr" && reached; fi
		;;
	*) reached ;;
	esac
	[ -n "$CC_RID" ] || { [ "$CC_KIND" = startfail ] && [ "$CC_POS" = C ]; } || { OBS="$OBS; no run id"; SETUP=1; return 1; }
	case $CC_KIND in
	pre)
		cc_handled "$_ccs"
		[ "$(cc_later "$_cci")" = 0 ] || cc_bad "records-at-or-after-$CC_POS"
		for _cen in $(cc_earlier "$_cci"); do [ "$(cc_runs "$_cen")" = 1 ] || cc_bad "runs-$_cen"; done
		# shellcheck disable=SC2046
		cc_psfirst $(cc_earlier "$_cci") || cc_bad first-phase-queries
		;;
	gap)
		cc_handled "$_ccs" "$_ccn"
		[ "$(cc_runs "$_ccn")" = 1 ] || cc_bad "runs-$CC_POS"
		[ "$(grep -n -- " --name $_ccn " "$TMP/cc.dlog" | head -n 1 | cut -d: -f1)" -gt "$_ccs" ] || cc_bad "run-before-the-signal"
		[ "$(cc_later "$((_cci + 1))")" = 0 ] || cc_bad "records-after-$CC_POS"
		for _cen in $(cc_earlier "$_cci"); do [ "$(cc_runs "$_cen")" = 1 ] || cc_bad "runs-$_cen"; done
		# shellcheck disable=SC2046
		cc_psfirst $(cc_earlier "$_cci") "$_ccn" || cc_bad first-phase-queries
		;;
	inflight | group)
		cc_handled "$_ccs"
		[ "$(cc_runs "$_ccn")" = 1 ] || cc_bad "runs-$CC_POS"
		[ "$(cc_later "$((_cci + 1))")" = 0 ] || cc_bad "records-after-$CC_POS"
		! grep -Fq 'dev-run: cleanup_failed' "$TMP/rerr" || cc_bad cleanup-failed
		# shellcheck disable=SC2046
		cc_psfirst $(cc_earlier "$_cci") "$_ccn" || cc_bad first-phase-queries
		;;
	nativegrp)
		[ "$RS" = 125 ] || cc_bad "status-$RS"
		! grep -Fq 'dev-run: interrupted' "$TMP/rerr" || cc_bad interrupted-line
		[ "$(cc_later "$((_cci + 1))")" = 0 ] || cc_bad "records-after-$CC_POS"
		if [ "$CC_POS" = C ]; then
			grep -Fq 'dev-run: container_start_failed' "$TMP/rerr" || cc_bad no-container_start_failed
			grep -Fq "dev-run: cleanup_failed container=$_ccn" "$TMP/rerr" || cc_bad no-cleanup_failed-C
			cc_psfirst "$_ccn" || cc_bad first-phase-query-C
		else
			grep -Fq 'dev-run: capture_copy_failed' "$TMP/rerr" || cc_bad no-capture_copy_failed
			[ "$(cc_named "$_ccn")" -ge 1 ] || cc_bad "$CC_POS-never-queried"
		fi
		;;
	startfail)
		[ "$RS" = 125 ] || cc_bad "status-$RS"
		! grep -Fq 'dev-run: interrupted' "$TMP/rerr" || cc_bad interrupted-line
		if [ "$CC_POS" = C ]; then
			grep -Fq 'dev-run: container_start_failed' "$TMP/rerr" || cc_bad no-container_start_failed
			{ [ "$(cc_cmds ps)" = 0 ] && [ "$(cc_cmds rm)" = 0 ]; } || cc_bad "ps-or-rm-records"
		else
			grep -Fq 'dev-run: capture_copy_failed' "$TMP/rerr" || cc_bad no-capture_copy_failed
			[ "$(cc_later "$_cci")" = 0 ] || cc_bad "records-at-or-after-$CC_POS"
			# shellcheck disable=SC2046
			cc_psfirst $(cc_earlier "$_cci") || cc_bad first-phase-queries
		fi
		;;
	preissue)
		[ "$RS" = 125 ] || cc_bad "status-$RS"
		! grep -Fq 'dev-run: interrupted' "$TMP/rerr" || cc_bad interrupted-line
		grep -Fq 'dev-run: capture_copy_failed' "$TMP/rerr" || cc_bad no-capture_copy_failed
		[ "$(cc_later "$_cci")" = 0 ] || cc_bad "records-at-or-after-$CC_POS"
		# shellcheck disable=SC2046
		cc_psfirst $(cc_earlier "$_cci") || cc_bad first-phase-queries
		;;
	esac
	cc_final
	OBS="$CC_KIND($CC_POS): status $RS, run $CC_RID, failed predicates:${CC_BAD:- none}"
	[ -z "$CC_BAD" ]
}
a_cc_noack() { # the acknowledgement-removed fixture: the start barrier is released while the consumer is still held
	CC_ACK=noack
	a_cc "$1"
	_cnk=$?
	CC_ACK=ack
	return "$_cnk"
}
for P in C V1 V2 V3 V4; do
	CC_POS=$P
	cc_build "cc-pre-$P" pre "$P"
	cc_build "cc-gap-$P" gap "$P"
	cc_build "cc-sf-$P" startfail "$P"
	CC_KIND=pre
	holds "pre($P): TERM at the barrier before $P's start checkpoint, released after the acknowledgement, starts nothing at or after $P" a_cc "$MUT/cc-pre-$P.sh"
	CC_KIND=gap
	holds "gap($P): TERM between $P's checkpoint and its start leaves exactly its one committed create, then reconciliation only" a_cc "$MUT/cc-gap-$P.sh"
	CC_KIND=startfail
	holds "startfail($P): a non-existent argv0 for $P's start alone issues nothing and is that step's failure" a_cc "$MUT/cc-sf-$P.sh"
done
for P in C V2; do
	CC_POS=$P
	CC_KIND=inflight
	holds "inflight($P): TERM while $P's create is held keeps the owner alive, starts nothing, and reconciles without cleanup_failed" a_cc "$RUNNER"
	CC_KIND=group
	holds "group($P): TERM to the runner's process group while $P's create is held (inflight window, native group untouched)" a_cc "$RUNNER"
	CC_KIND=nativegrp
	holds "nativegrp($P): TERM to $P's native process group fails its create, retained and reconciled, with no interrupted line" a_cc "$RUNNER"
done
CC_POS=V2
CC_KIND=preissue
gvariant cc-preissue-V2 controller.go 'if xr.startErr != nil {' 'if name == "dev-mobile" {
			xr.status = 7
		}
		if xr.startErr != nil {'
holds "pre-issue(V2): only V2's exporter returns 7 before its validator start: nothing names V2 or later, V1 reconciled" a_cc "$MUT/cc-preissue-V2.sh"
# the cancellation latch is synchronized with its acknowledgement; the delayed-consumer fault holds before latching and the acknowledgement-removed fixture is caught
CONS_OLD='if s != syscall.SIGPIPE {'
CONS_NEW='if s != syscall.SIGPIPE {
					_ = os.WriteFile("'$BAR'/consumer.ready", nil, 0o644)
					for i := 0; i < 120; i++ {
						if _, err := os.Stat("'$BAR'/consumer.go"); err == nil {
							break
						}
						time.Sleep(time.Second)
					}'
for P in C V2; do
	cc_build "cc-delay-pre-$P" pre "$P" "$CONS_OLD" "$CONS_NEW"
	cc_build "cc-delay-gap-$P" gap "$P" "$CONS_OLD" "$CONS_NEW"
	CC_POS=$P
	for CC_KIND in pre gap; do
		CC_DELAY=1
		RG_GREEN_FN=a_cc
		CC_NOACK_SLEEP=1
		[ "$CC_KIND" != gap ] || CC_NOACK_SLEEP=3
		redgreen "$CC_KIND($P) with a delayed signal consumer: the acknowledgement-removed fixture releases the barrier while the consumer is held and is caught" a_cc_noack "$MUT/cc-delay-$CC_KIND-$P.sh" "$MUT/cc-delay-$CC_KIND-$P.sh"
		RG_GREEN_FN=
		CC_DELAY=0
		CC_NOACK_SLEEP=1
	done
done
CC_KIND=pre
# mutants of the controller, each judged on its own cell
CC_POS=C
CC_KIND=pre
cc_build mut-no-cancel-check-C pre C "$CC_CHECK_OLD" 'if false {'
redgreen "pre(C): no cancellation check before a start (a run record names C after the signal)" a_cc "$MUT/mut-no-cancel-check-C.sh" "$MUT/cc-pre-C.sh"
CC_POS=V2
cc_build mut-no-cancel-check-V2 pre V2 "$CC_CHECK_OLD" 'if false {'
redgreen "pre(V2): no cancellation check before a start (a run record names V2 after the signal)" a_cc "$MUT/mut-no-cancel-check-V2.sh" "$MUT/cc-pre-V2.sh"
cc_build mut-trackall-V2 pre V2 '	for _, name := range captureNames {
		left := int(endSec - time.Now().Unix())' '	for _, nm := range captureNames {
		c.issued = append(c.issued, c.ctr+"-v-"+nm)
	}
	for _, name := range captureNames {
		left := int(endSec - time.Now().Unix())'
redgreen "pre(V2): track-every-allocated-name mutant queries V2 to V4" a_cc "$MUT/mut-trackall-V2.sh" "$MUT/cc-pre-V2.sh"
RECBEFORE_OLD='	if err := cmd.Start(); err != nil {
		closeAll()
		return nil, err
	}
	if s.issue != "" {
		c.issued = append(c.issued, s.issue)
	}'
RECBEFORE_NEW='	if s.issue != "" {
		c.issued = append(c.issued, s.issue)
	}
	if err := cmd.Start(); err != nil {
		closeAll()
		return nil, err
	}'
CC_KIND=startfail
for P in C V2; do
	CC_POS=$P
	cc_build "mut-record-before-start-$P" startfail "$P" "$RECBEFORE_OLD" "$RECBEFORE_NEW"
	redgreen "startfail($P): a name recorded before its start is queried" a_cc "$MUT/mut-record-before-start-$P.sh" "$MUT/cc-sf-$P.sh"
done
CC_POS=C
CC_KIND=nativegrp
gvariant mut-record-success-only controller.go "$SO1_OLD" "$SO1_NEW" "$SO2_OLD" "$SO2_NEW"
redgreen "nativegrp(C): a name recorded only after a successful create is neither queried nor retained" a_cc "$MUT/mut-record-success-only.sh" "$RUNNER"
CC_POS=V2
CC_KIND=gap
cc_build mut-cancel-exits-V2 gap V2 'c.intrOnce.Do(func() { c.say("interrupted") })' 'c.intrOnce.Do(func() { c.say("interrupted") })
	os.Exit(125)'
redgreen "gap(V2): an immediate exit on cancellation leaves C present" a_cc "$MUT/mut-cancel-exits-V2.sh" "$MUT/cc-gap-V2.sh"
cc_build mut-cleanup-container-only-V2 gap V2 'names := append([]string(nil), c.issued...)' 'names := append([]string(nil), c.issued[:1]...)'
redgreen "gap(V2): a closure over the container only never queries V1" a_cc "$MUT/mut-cleanup-container-only-V2.sh" "$MUT/cc-gap-V2.sh"
CC_POS=C
CC_KIND=inflight
gvariant mut-cancel-kills-create controller.go 'if s.kind == kOther {' 'if s.kind != kReconcile {'
redgreen "inflight(C): cancellation that kills the create leaves a retained failure" a_cc "$MUT/mut-cancel-kills-create.sh" "$RUNNER"
# repeated signals and dispositions (T02/T26/T31): the first TERM is acknowledged once, later ones change nothing; HUP and INT cancel unless inherited ignored
a_rs() { # a_rs RUNNER (RS_KIND create|reconcile, CC_POS C|V2): a second TERM while a create, or an owned reconciliation call, is held
	CC_NAME=cc
	CC_BAD=
	CC_RID=
	find "$BAR" -mindepth 1 -delete
	_ccm0=$(find "$ISO/testdata" "$ISO/docs/spikes/issue-drafts" 2>/dev/null | LC_ALL=C sort | tr '\n' ' ')
	_ccr=$1
	CAP=
	DSHIM_HOLD=
	case $RS_KIND in
	create)
		DSHIM_HOLD=$(pos_hold "$CC_POS")
		if [ "$CC_POS" = C ]; then
			set -- -- true
		else
			CAP=$(capdir)
			set -- --capture "$CAP" -- sh -c "${BASE}shoot_all"
		fi
		;;
	reconcile)
		DSHIM_HOLD='ps -a --filter name=^boxset-dev-'
		set -- -- sh -c "$CBAR"
		;;
	esac
	bg_runlog cc "$_ccr" "$@"
	DSHIM_HOLD=
	if [ "$RS_KIND" = reconcile ]; then
		_c=$(bg_ctr cc) || { cc_setup "no container"; return 1; }
		OWNED="$OWNED $_c"
		if ! bg_wait "$_c" /tmp/ready; then
			cc_setup "the child never reached its barrier"
			return 1
		fi
		_ccs=$(wc -l <"$TMP/cc.dlog" | tr -d ' ')
		cc_rid
		kill -s TERM "$BG_PID"
		if ! cc_ack 30; then
			cc_setup "no acknowledgement of the first TERM"
			return 1
		fi
		if ! cc_wait_file "$BAR/hold.ready"; then
			cc_setup "the owned reconciliation call was never held"
			return 1
		fi
	else
		if ! cc_wait_file "$BAR/hold.ready"; then
			cc_setup "the create was never held"
			return 1
		fi
		_ccs=$(wc -l <"$TMP/cc.dlog" | tr -d ' ')
		cc_rid
		kill -s TERM "$BG_PID"
		if ! cc_ack 30; then
			cc_setup "no acknowledgement of the first TERM"
			return 1
		fi
	fi
	kill -s TERM "$BG_PID" 2>/dev/null
	sleep 2
	kill -0 "$BG_PID" 2>/dev/null || cc_bad owner-died-on-the-second-signal
	[ "$(grep -c '^dev-run: interrupted$' "$TMP/cc.err")" = 1 ] || cc_bad interrupted-lines-while-held
	: >"$BAR/hold.go"
	bg_end cc
	dlogs_same cc || return 1
	reached
	[ -n "$CC_RID" ] || { OBS="$OBS; no run id"; SETUP=1; return 1; }
	_cci=$(pos_index "$CC_POS")
	_ccn=$(pos_name "$CC_RID" "$CC_POS")
	cc_handled "$_ccs"
	if [ "$RS_KIND" = reconcile ]; then
		[ "$(cc_later 1)" = 0 ] || cc_bad validator-work
	else
		[ "$(cc_runs "$_ccn")" = 1 ] || cc_bad "runs-$CC_POS"
		[ "$(cc_later "$((_cci + 1))")" = 0 ] || cc_bad "records-after-$CC_POS"
		! grep -Fq 'dev-run: cleanup_failed' "$TMP/rerr" || cc_bad cleanup-failed
	fi
	cc_final
	OBS="$RS_KIND($CC_POS): status $RS, run $CC_RID, failed predicates:${CC_BAD:- none}"
	[ -z "$CC_BAD" ]
}
# a_sigdisp RUNNER (SD_SIG HUP|INT, SD_MODE default|ignored): the inherited disposition is established before exec (env --default-signal or --ignore-signal) and read back from the controller's own SigIgn mask at a
# compiled barrier BEFORE its handlers are installed (the runner is the barrier copy), so a wrong inherited disposition is an unestablished premise (setup). After the registration the mask and the signal's
# effect are the controller's own behaviour and are judged as assertions: a default HUP or INT cancels like TERM, an inherited-ignored one stays ignored and lets the run continue to the intended child result (7).
a_sigdisp() {
	CC_NAME=sd
	CC_BAD=
	CC_RID=
	find "$BAR" -mindepth 1 -delete
	_ccm0=$(find "$ISO/testdata" "$ISO/docs/spikes/issue-drafts" 2>/dev/null | LC_ALL=C sort | tr '\n' ' ')
	CAP=
	_sr=$1
	DSHIM_HOLD=
	if [ "$SD_MODE" = ignored ]; then
		bg_runlog sd env --ignore-signal=HUP,INT "$_sr" -- sh -c "$CBAR; exit 7"
	else
		bg_runlog sd env --default-signal=HUP,INT "$_sr" -- sh -c "$CBAR; exit 7"
	fi
	if ! cc_wait_file "$BAR/ready"; then
		cc_setup "the barrier before the handler registration was never reached"
		return 1
	fi
	_sg=$(sed -n 's/^SigIgn:[[:space:]]*//p' "/proc/$BG_PID/status" 2>/dev/null)
	case $_sg in
	'' | *[!0-9a-fA-F]*)
		cc_setup "the controller's signal dispositions could not be read"
		return 1
		;;
	esac
	_sm=$((0x$_sg & 3))
	if { [ "$SD_MODE" = ignored ] && [ "$_sm" != 3 ]; } || { [ "$SD_MODE" = default ] && [ "$_sm" != 0 ]; }; then
		cc_setup "the inherited disposition ($SD_MODE) was not established before the registration: SigIgn mask $_sm"
		return 1
	fi
	: >"$BAR/go"
	_c=$(bg_ctr sd) || { cc_setup "no container"; return 1; }
	OWNED="$OWNED $_c"
	if ! bg_wait "$_c" /tmp/ready; then
		cc_setup "the child never reached its barrier"
		return 1
	fi
	_sg2=$(sed -n 's/^SigIgn:[[:space:]]*//p' "/proc/$BG_PID/status" 2>/dev/null)
	case $_sg2 in
	'' | *[!0-9a-fA-F]*)
		cc_setup "the controller's signal dispositions could not be read after the registration"
		return 1
		;;
	esac
	[ "$((0x$_sg2 & 3))" = "$_sm" ] || cc_bad "disposition-changed-by-the-registration-$_sm-to-$((0x$_sg2 & 3))"
	cc_rid
	_ccs=$(wc -l <"$TMP/sd.dlog" | tr -d ' ')
	reached
	kill -s "$SD_SIG" "$BG_PID" 2>/dev/null
	if [ "$SD_MODE" = ignored ]; then
		sleep 2
		_sdx=0 # 1 once the signal's own cancellation of the target is positively observed: the controller exited or printed its interrupted line
		kill -0 "$BG_PID" 2>/dev/null || { cc_bad owner-died; _sdx=1; }
		! grep -Fq 'dev-run: interrupted' "$TMP/sd.err" || { cc_bad interrupted-line; _sdx=1; }
		# a target the signal already cancelled may have removed its own container, so its release is not demanded; any other failed release is setup
		bg_touch "$_c" /tmp/go || [ "$_sdx" = 1 ] || SETUP=1
		bg_end sd
		[ "$RS" = 7 ] || cc_bad "status-$RS"
		! grep -Fq 'dev-run: interrupted' "$TMP/rerr" || cc_bad interrupted-line
	else
		cc_ack 30 || cc_bad no-acknowledgement
		_wi=0
		while kill -0 "$BG_PID" 2>/dev/null && [ "$_wi" -lt 120 ]; do
			sleep 1
			_wi=$((_wi + 1))
		done
		if kill -0 "$BG_PID" 2>/dev/null; then
			cc_bad owner-still-running
			kill -KILL "$BG_PID" 2>/dev/null
		fi
		bg_end sd
		cc_handled "$_ccs"
	fi
	dlogs_same sd || return 1
	cc_final
	OBS="$SD_MODE $SD_SIG: status $RS, failed predicates:${CC_BAD:- none}"
	[ -z "$CC_BAD" ]
}
for RS_KIND in create reconcile; do
	for CC_POS in C V2; do
		[ "$RS_KIND" = create ] || [ "$CC_POS" = C ] || continue
		holds "repeated TERM ($RS_KIND, $CC_POS): the owner survives a second TERM, one interrupted line, reconciliation only, nothing published" a_rs "$RUNNER"
	done
done
RS_KIND=create
CC_POS=C
gvariant mut-defaults-before-ack controller.go 'func (c *ctl) interrupt() {
	c.latch()' 'func (c *ctl) interrupt() {
	c.latch()
	signal.Reset(syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)'
gvariant mut-exit-on-second controller.go 'func (c *ctl) interrupt() {
	c.latch()' 'func (c *ctl) interrupt() {
	if c.cancelled.Load() {
		os.Exit(125)
	}
	c.latch()'
redgreen "repeated TERM (create, C): defaults restored before the first acknowledgement" a_rs "$MUT/mut-defaults-before-ack.sh" "$RUNNER"
redgreen "repeated TERM (create, C): exit only on the second signal" a_rs "$MUT/mut-exit-on-second.sh" "$RUNNER"
SD_OLD='	stop := c.installSignals()'
SD_NEW="	$(gobar 120)
	stop := c.installSignals()"
gvariant fault-sig-barrier controller.go "$SD_OLD" "$SD_NEW"
gvariant mut-no-hup-int controller.go "$SD_OLD" "$SD_NEW" 'for _, s := range []os.Signal{syscall.SIGHUP, syscall.SIGINT} {' 'for _, s := range []os.Signal{} {'
gvariant mut-register-ignored controller.go "$SD_OLD" "$SD_NEW" 'if !signal.Ignored(s) {' 'if true {'
for SD_SIG in HUP INT; do
	SD_MODE=default
	holds "$SD_SIG with the default disposition cancels like TERM (disposition read back from the controller's SigIgn mask before registration)" a_sigdisp "$MUT/fault-sig-barrier.sh"
	SD_MODE=ignored
	holds "inherited-ignored $SD_SIG: the run continues to the intended child result and prints no interrupted line" a_sigdisp "$MUT/fault-sig-barrier.sh"
done
SD_SIG=HUP
SD_MODE=default
redgreen "default HUP: HUP and INT registration omitted" a_sigdisp "$MUT/mut-no-hup-int.sh" "$MUT/fault-sig-barrier.sh"
SD_MODE=ignored
redgreen "inherited-ignored HUP: ignored HUP and INT registered anyway (the wrong disposition after registration is the assertion, not setup)" a_sigdisp "$MUT/mut-register-ignored.sh" "$MUT/fault-sig-barrier.sh"
SD_SIG=INT
redgreen "inherited-ignored INT: ignored HUP and INT registered anyway" a_sigdisp "$MUT/mut-register-ignored.sh" "$MUT/fault-sig-barrier.sh"
# bootstrap and exec controls (T26)
a_exec_identity() { # a_exec_identity RUNNER: the runner PID is the controller (argv[1] -run, argv[0] .../bin/redact) and every child of it is a timeout wrapper
	CC_NAME=ex
	CC_BAD=
	find "$BAR" -mindepth 1 -delete
	CAP=
	bg_runlog ex "$1" -- sh -c "$CBAR"
	_c=$(bg_ctr ex) || { cc_setup "no container"; return 1; }
	OWNED="$OWNED $_c"
	if ! bg_wait "$_c" /tmp/ready; then
		cc_setup "the child never reached its barrier"
		return 1
	fi
	_av=$(tr '\0' '\n' <"/proc/$BG_PID/cmdline" 2>/dev/null)
	_a0=$(printf '%s\n' "$_av" | sed -n 1p)
	_a1=$(printf '%s\n' "$_av" | sed -n 2p)
	_kids=0
	_badk=0
	for _sf in $(find /proc -maxdepth 2 -name stat -path '/proc/[0-9]*/stat' 2>/dev/null); do
		read -r _sl <"$_sf" 2>/dev/null || continue
		_sr=${_sl##*) }
		set -- $_sr
		[ "${2:-}" = "$BG_PID" ] || continue
		_sc=${_sl#*(}
		_sc=${_sc%)*}
		_kids=$((_kids + 1))
		[ "$_sc" = timeout ] || _badk=$((_badk + 1))
	done
	bg_touch "$_c" /tmp/go || SETUP=1
	bg_end ex
	reached
	OBS="argv[0] '$_a0', argv[1] '$_a1', $_kids children of the runner PID, $_badk of them not timeout wrappers"
	[ "$_a1" = -run ] && case $_a0 in */bin/redact) true ;; *) false ;; esac && [ "$_kids" -ge 1 ] && [ "$_badk" = 0 ]
}
variant mut-fork-controller 'exec "$REDACT" -run' '"$REDACT" -run'
redgreen "bootstrap that forks the controller instead of exec'ing it" a_exec_identity "$MUT/mut-fork-controller.sh" "$RUNNER"
a_boot_term() { # a_boot_term RUNNER (BOOT_MODE pid|group): TERM while the helper build is held ends the run at the bootstrap: interrupted, no work build, no run, private TMPDIR empty
	CC_NAME=bt
	CC_BAD=
	find "$BAR" -mindepth 1 -delete
	rm -rf "$TMP/boot-tmp"
	mkdir -m 0700 "$TMP/boot-tmp" || { SETUP=1; return 1; }
	DSHIM_HOLD='--target host-redact'
	if [ "$BOOT_MODE" = group ]; then
		bg_runlog bt env TMPDIR="$TMP/boot-tmp" timeout 900 "$1" -- true
	else
		bg_runlog bt env TMPDIR="$TMP/boot-tmp" "$1" -- true
	fi
	DSHIM_HOLD=
	if ! cc_wait_file "$BAR/hold.ready"; then
		cc_setup "the helper build was never held"
		return 1
	fi
	reached
	if [ "$BOOT_MODE" = group ]; then kill -s TERM -- "-$BG_PID"; else kill -s TERM "$BG_PID"; fi
	sleep 2
	: >"$BAR/hold.go"
	bg_end bt
	dlogs_same bt || return 1
	OBS="status $RS, $(grep -Fc -- '--target work' "$TMP/bt.dlog") work builds, $(CC_NAME=bt cc_cmds run) runs, TMPDIR holds '$(ls -A "$TMP/boot-tmp" | tr '\n' ' ')'"
	[ "$RS" = 125 ] && [ "$(grep -c '^dev-run: interrupted$' "$TMP/rerr")" = 1 ] && [ "$(grep -Fc -- '--target work' "$TMP/bt.dlog")" = 0 ] && [ "$(CC_NAME=bt cc_cmds run)" = 0 ] && [ -z "$(ls -A "$TMP/boot-tmp")" ]
}
for BOOT_MODE in pid group; do
	holds "boot-term ($BOOT_MODE): TERM during the held helper build: 125 interrupted, no work build or run record, private TMPDIR empty" a_boot_term "$RUNNER"
done
a_child_cancel() { # a_child_cancel RUNNER: the child held at CBAR, TERM to the runner PID: the child exec is killed and the run ends within 60 s with no publication
	CC_NAME=cp
	CC_BAD=
	CC_RID=
	find "$BAR" -mindepth 1 -delete
	_ccm0=$(find "$ISO/testdata" "$ISO/docs/spikes/issue-drafts" 2>/dev/null | LC_ALL=C sort | tr '\n' ' ')
	CAP=
	bg_runlog cp "$1" -- sh -c "$CBAR"
	_c=$(bg_ctr cp) || { cc_setup "no container"; return 1; }
	OWNED="$OWNED $_c"
	if ! bg_wait "$_c" /tmp/ready; then
		cc_setup "the child never reached its barrier"
		return 1
	fi
	cc_rid
	_ccs=$(wc -l <"$TMP/cp.dlog" | tr -d ' ')
	kill -s TERM "$BG_PID"
	if ! cc_ack 30; then
		cc_setup "no acknowledgement of the handled signal"
		return 1
	fi
	reached
	_wi=0
	while kill -0 "$BG_PID" 2>/dev/null && [ "$_wi" -lt 60 ]; do
		sleep 1
		_wi=$((_wi + 1))
	done
	if kill -0 "$BG_PID" 2>/dev/null; then
		cc_bad "runner-alive-60s-after-the-signal"
		kill -KILL "$BG_PID" 2>/dev/null
	fi
	bg_end cp
	dlogs_same cp || return 1
	cc_handled "$_ccs"
	cc_final
	OBS="child-phase cancel: status $RS, failed predicates:${CC_BAD:- none}"
	[ -z "$CC_BAD" ]
}
holds "child-phase-cancel: TERM with the child held kills the child exec and ends the run within 60 s, nothing published" a_child_cancel "$RUNNER"
gvariant mut-cancel-waits-child controller.go '		kind: kOther,
		argv: argv,' '		kind: kCreate,
		argv: argv,'
redgreen "child exec that is not killed on cancellation: the runner is still alive 60 s after the signal" a_child_cancel "$MUT/mut-cancel-waits-child.sh" "$RUNNER"
a_epipe() { # a_epipe RUNNER: a closed stdout is an EPIPE write error (output_write_failed, C absent), never SIGPIPE death
	rm -f "$TMP/epipe.rs"
	{ (cd "$ISO" && "$1" -- sh -c 'printf a; sleep 3; head -c 70000 /dev/zero') 2>"$TMP/rerr"; echo $? >"$TMP/epipe.rs"; } | head -c 1 >/dev/null
	RS=$(cat "$TMP/epipe.rs" 2>/dev/null)
	rerr_seen
	_cn=$(cname)
	[ -z "$_cn" ] || OWNED="$OWNED $_cn"
	{ [ -n "$_cn" ] && [ -n "$RS" ]; } && reached
	cstate "${_cn:-boxset-dev-none}"
	OBS="status $RS, container ${_cn:-none} $CST, $(grep -o 'dev-run: [a-z_]*' "$TMP/rerr" | tail -n 1)"
	_ok=0
	[ "$RS" = 125 ] && grep -Fq 'dev-run: output_write_failed' "$TMP/rerr" && [ "$CST" = absent ] && _ok=1
	[ -z "$_cn" ] || docker rm -f "$_cn" >/dev/null 2>&1
	[ "$_ok" = 1 ]
}
holds "epipe: stdout closed while the child still writes is output_write_failed (125) and C is absent" a_epipe "$RUNNER"
gvariant mut-no-sigpipe-notify controller.go 'sigs := []os.Signal{syscall.SIGTERM, syscall.SIGPIPE}' 'sigs := []os.Signal{syscall.SIGTERM}'
redgreen "SIGPIPE not notified: the controller dies of SIGPIPE (141) and C stays present" a_epipe "$MUT/mut-no-sigpipe-notify.sh" "$RUNNER"
# ---- the sinks of the full controller (NC07, NC12, NC17) ------------------------------------------------------------------------------------------------------------------
host_dead() { # host_dead PID: the task is gone or a zombie
	_hds=$(sed -n 's/^State:[[:space:]]*\(.\).*/\1/p' "/proc/$1/status" 2>/dev/null)
	[ -z "$_hds" ] || [ "$_hds" = Z ]
}
# NC07: the full controller with a stderr that is full and never read. A compiled copy queues a 70,000-byte diagnostic at the child start, so the pipe is full before the closure; the owner must still finish its
# run (and its checked closure), while the mutant that writes diagnostics synchronously holds the lifetime owner in that write.
BLKRAW_OLD='	c.say(fmt.Sprintf("image=%s inputs=%s timeout=%ds container=%s", c.img, c.inputs, c.timeout, c.ctr))'
BLKRAW_NEW='	c.raw(bytes.Repeat([]byte("x"), 70000))
	c.say(fmt.Sprintf("image=%s inputs=%s timeout=%ds container=%s", c.img, c.inputs, c.timeout, c.ctr))'
BLKSAY_OLD='c.diag.put(diagItem{b: []byte("dev-run: " + msg + "\n")})'
BLKSAY_NEW='_, _ = c.diag.w.Write([]byte("dev-run: " + msg + "\n"))'
BLKRAWW_OLD='c.diag.put(diagItem{b: bytes.Clone(b)})'
BLKRAWW_NEW='_, _ = c.diag.w.Write(b)'
BLK=ok
a_stderr_blocked() { # a_stderr_blocked RUNNER (BLK ok|cleanup): the run ends by itself with its own status although stderr is full; cleanup also fails the owned removal, so the diagnostics of the checked closure meet the blocked sink
	rm -f "$TMP/blk.fifo"
	mkfifo "$TMP/blk.fifo" || { OBS="the stderr fifo could not be made"; SETUP=1; return 1; }
	{ true >"$TMP/blk.dlog" && true >"$TMP/blk.dlog.q"; } || SETUP=1
	exec 5<>"$TMP/blk.fifo"
	(cd "$ISO" && PATH="$TMP/dshim:$PATH" DLOG="$TMP/blk.dlog" exec "$1" -- sh -c 'printf ran') >"$TMP/rout" 2>&5 &
	_bp=$!
	_i=0
	while kill -0 "$_bp" 2>/dev/null && [ "$_i" -lt 200 ]; do
		sleep 1
		_i=$((_i + 1))
	done
	_held=0
	if kill -0 "$_bp" 2>/dev/null; then
		_held=1
		kill -KILL "$_bp" 2>/dev/null
	fi
	wait "$_bp" 2>/dev/null
	RS=$?
	_nb=$(dd iflag=nonblock bs=1048576 count=1 <&5 2>/dev/null | wc -c | tr -d ' ')
	exec 5<&-
	_cn=$(grep -E '^S [^ ]+ [0-9]+\.[0-9][0-9] run ' "$TMP/blk.dlog" 2>/dev/null | sed -n 's/.* --name \([^ ]*\) .*/\1/p' | head -n 1)
	[ -z "$_cn" ] || OWNED="$OWNED $_cn"
	cstate "${_cn:-boxset-dev-none}"
	OBS="$BLK: status $RS, still held by the sink after 200 s: $_held, bytes left in the full pipe $_nb, container ${_cn:-none} $CST, output '$(cat "$TMP/rout")'"
	[ "$_nb" -ge 60000 ] || { OBS="$OBS; the stderr pipe was not full"; SETUP=1; return 1; }
	[ -n "$_cn" ] || { SETUP=1; return 1; }
	reached
	_ok=0
	case $BLK in
	ok) [ "$_held" = 0 ] && [ "$RS" = 0 ] && [ "$(cat "$TMP/rout")" = ran ] && [ "$CST" = absent ] && _ok=1 ;;
	cleanup) [ "$_held" = 0 ] && [ "$RS" = 125 ] && [ "$(cat "$TMP/rout")" = ran ] && _ok=1 ;;
	esac
	docker rm -f "$_cn" >/dev/null 2>&1
	[ "$_ok" = 1 ]
}
gvariant fault-blk-ok controller.go "$BLKRAW_OLD" "$BLKRAW_NEW"
gvariant mut-blk-ok controller.go "$BLKRAW_OLD" "$BLKRAW_NEW" "$BLKSAY_OLD" "$BLKSAY_NEW" "$BLKRAWW_OLD" "$BLKRAWW_NEW"
gvariant fault-blk-cleanup controller.go "$BLKRAW_OLD" "$BLKRAW_NEW" "$RM_OLD" 'return []string{"/bin/sh", "-c", "exit 1"}'
gvariant mut-blk-cleanup controller.go "$BLKRAW_OLD" "$BLKRAW_NEW" "$RM_OLD" 'return []string{"/bin/sh", "-c", "exit 1"}' "$BLKSAY_OLD" "$BLKSAY_NEW" "$BLKRAWW_OLD" "$BLKRAWW_NEW"
BLK=ok
redgreen "blocked stderr: diagnostics written synchronously hold the lifetime owner (full unread stderr, checked closure)" a_stderr_blocked "$MUT/mut-blk-ok.sh" "$MUT/fault-blk-ok.sh"
BLK=cleanup
redgreen "blocked stderr with a failed owned removal: synchronous cleanup diagnostics hold the lifetime owner" a_stderr_blocked "$MUT/mut-blk-cleanup.sh" "$MUT/fault-blk-cleanup.sh"
# NC17: the stdout of the runner is a full pipe nobody reads, with a producer that still writes: bounded output_write_failed (125), the owned container reconciled, nothing published
a_stdout_blocked() { # a_stdout_blocked RUNNER
	rm -rf "$ISO/testdata"
	mkdir -p "$ISO/testdata/parser"
	rm -f "$TMP/blkout.fifo"
	mkfifo "$TMP/blkout.fifo" || { OBS="the stdout fifo could not be made"; SETUP=1; return 1; }
	exec 6<>"$TMP/blkout.fifo"
	dd if=/dev/zero bs=65536 count=1 >&6 2>/dev/null || { OBS="the stdout pipe could not be filled"; SETUP=1; exec 6<&-; return 1; }
	if dd if=/dev/zero bs=1 count=1 oflag=nonblock >&6 2>/dev/null; then
		OBS="the stdout pipe was not full"
		SETUP=1
		exec 6<&-
		return 1
	fi
	(cd "$ISO" && exec "$1" --write testdata/parser --timeout 3 -- sh -c 'printf pub >/src/testdata/parser/series.jsonl; head -c 1000 /dev/zero | tr "\0" a') >"$TMP/blkout.fifo" 2>"$TMP/bo.err" &
	_bp=$!
	_i=0
	while kill -0 "$_bp" 2>/dev/null && [ "$_i" -lt 90 ]; do
		sleep 1
		_i=$((_i + 1))
	done
	_held=0
	if kill -0 "$_bp" 2>/dev/null; then
		_held=1
		kill -KILL "$_bp" 2>/dev/null
	fi
	wait "$_bp" 2>/dev/null
	RS=$?
	exec 6<&-
	cp "$TMP/bo.err" "$TMP/rerr"
	rerr_seen
	_cn=$(cname)
	[ -z "$_cn" ] || OWNED="$OWNED $_cn"
	cstate "${_cn:-boxset-dev-none}"
	OBS="status $RS, still held by the sink after 90 s: $_held, container ${_cn:-none} $CST, $(grep -o 'dev-run: [a-z_]*' "$TMP/rerr" | tail -n 1), published series $([ -e "$ISO/testdata/parser/series.jsonl" ] && echo yes || echo no)"
	grep -Eq "$RUNNER_STARTED_RE" "$TMP/rerr" && reached
	[ "$_held" = 0 ] && [ "$RS" = 125 ] && grep -Fq 'dev-run: output_write_failed' "$TMP/rerr" && [ "$CST" = absent ] && [ ! -e "$ISO/testdata/parser/series.jsonl" ]
}
holds "blocked stdout: a producer writing into a full unread stdout ends the run as output_write_failed within the bound, the owned container is absent and nothing is published" a_stdout_blocked "$RUNNER"
rm -rf "$ISO/testdata"
# NC12: the output stop starts the owned removal inside the parent's own wait. The compiled copy runs a TERM-ignoring child instead of the exec (it writes its pid, floods past the cap, then sleeps) and the shim
# holds the removal: the parent must still be killed at its own bound (--timeout 1: 6 s) while the removal is held. The mutant that waits for the removal synchronously leaves it alive.
RMHELD_PID=$TMP/rmheld.pid
RMH_OLD='	argv = append(argv, c.argv...)'
RMH_NEW='	argv = []string{"/bin/sh", "-c", `echo $$ >'$RMHELD_PID'; head -c 70000 /dev/zero; trap "" TERM HUP INT; sleep 100`}'
RMH_SYNC_OLD='if rm = c.startRm(s.removeOnOut); rm != nil {
					rmCh, rmTimer = rm.waitCh, time.NewTimer(time.Until(rm.deadline))
					rmDue = rmTimer.C
				}'
gvariant fault-rm-held controller.go "$RMH_OLD" "$RMH_NEW"
gvariant mut-rm-held-sync controller.go "$RMH_OLD" "$RMH_NEW" "$RMH_SYNC_OLD" 'c.rmOwned(s.removeOnOut)'
a_rm_held() { # a_rm_held RUNNER
	CC_NAME=rh
	CC_BAD=
	find "$BAR" -mindepth 1 -delete
	rm -f "$RMHELD_PID"
	DSHIM_HOLD='rm -f boxset-dev-'
	bg_runlog rh "$1" --timeout 1 -- true
	DSHIM_HOLD=
	if ! cc_wait_file "$BAR/hold.ready"; then
		cc_setup "the owned removal was never held"
		return 1
	fi
	_pp=$(cat "$RMHELD_PID" 2>/dev/null)
	case $_pp in
	'' | *[!0-9]*)
		cc_setup "the fault child recorded no pid"
		return 1
		;;
	esac
	if host_dead "$_pp"; then
		cc_setup "the parent was not live when its removal was held"
		return 1
	fi
	reached
	_i=0
	while ! host_dead "$_pp" && [ "$_i" -lt 20 ]; do
		sleep 1
		_i=$((_i + 1))
	done
	kill -0 "$BG_PID" 2>/dev/null || cc_bad owner-gone-while-held
	host_dead "$_pp" || cc_bad "parent-alive-20-s-into-a-held-removal"
	: >"$BAR/hold.go"
	bg_end rh
	dlogs_same rh || return 1
	{ [ "$RS" = 125 ] && grep -Fq 'dev-run: output_overflow' "$TMP/rerr"; } || cc_bad "status-$RS"
	_cn=$(cname)
	[ -z "$_cn" ] || OWNED="$OWNED $_cn"
	cstate "${_cn:-boxset-dev-none}"
	[ "$CST" = absent ] || cc_bad "present-$_cn"
	OBS="removal held: parent pid $_pp, failed predicates:${CC_BAD:- none}"
	[ -z "$CC_BAD" ]
}
redgreen "output-failure removal held: the parent is killed at its own bound while the removal runs (synchronous removal mutant leaves it)" a_rm_held "$MUT/mut-rm-held-sync.sh" "$MUT/fault-rm-held.sh"
# NC04: a real TERM, held by a compiled barrier at each publication window of the write root (the barrier statements exist only in the compiled copy). Every window also records each rename at the real renameOne seam (PUBRCPT),
# the target-attributable evidence of how far publication got. partial: between two renames; backup: after the first backup of the prior file, before any rename; post: after the commit point, before the backups are removed.
PUBRCPT=$TMP/pub.receipt
gpr() { printf '%s' '{ f, _ := os.OpenFile("'$PUBRCPT'", os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644); fmt.Fprintf(f, '"$1"'); f.Close() }'; }
PUBREN_NEW='func (c *ctl) renameOne(src, dst string) error {
	if err := c.wroot.Rename(src, dst); err != nil {
		return err
	}
	'"$(gpr '"rename %s\n", dst')"'
	return nil
}'
PUBREN_HOLD='func (c *ctl) renameOne(src, dst string) error {
	if err := c.wroot.Rename(src, dst); err != nil {
		return err
	}
	'"$(gpr '"rename %s\n", dst')"'
	if dst == "series.jsonl" {
		'"$(gobar 180)"'
	}
	return nil
}'
PUBCHK1_OLD='if c.cancelled.Load() {
			return c.failPub(reasonCancelled)
		}
	}
	if !c.rootUnchanged() { // the identity re-check after the renames'
PUBCHK1_NEW='_ = c.cancelled.Load()
	}
	if !c.rootUnchanged() { // the identity re-check after the renames'
PUBCHK2_OLD='if c.cancelled.Load() {
		return c.failPub(reasonCancelled)
	}
	// Commit point:'
PUBCHK2_NEW='_ = c.cancelled.Load()
	// Commit point:'
PUBPREP_OLD='		c.partP[n] = pid
		if c.cancelled.Load() { // after the accounted backup of this name'
PUBPREP_NEW='		c.partP[n] = pid
		if n == "series.jsonl" {
			'"$(gobar 180)"'
		}
		if c.cancelled.Load() { // after the accounted backup of this name'
PUBPREPCHK_OLD='if c.cancelled.Load() { // after the accounted backup of this name
			return c.failPub(reasonCancelled)
		}'
PUBPOST_OLD='	reason := ""
	for _, n := range c.staged {
		if _, had := c.partQ[n]; !had {'
PUBPOST_NEW='	'"$(gobar 180)"'
	reason := ""
	for _, n := range c.staged {
		if _, had := c.partQ[n]; !had {'
PUBPOSTRB_OLD='	return reason
}

func (c *ctl) publishWrite() bool {'
PUBPOSTRB_NEW='	if c.cancelled.Load() {
		c.rollback()
		return reasonCancelled
	}
	return reason
}

func (c *ctl) publishWrite() bool {'
PUBM=partial
a_pub_signal() { # a_pub_signal RUNNER (PUBM partial|backup|post)
	_P=$ISO/testdata/parser
	rm -rf "$_P"
	mkdir -p "$_P"
	printf 'prior series\n' >"$_P/series.jsonl"
	printf 'prior movie\n' >"$_P/movie.jsonl"
	_s0=$(ident_h "$_P/series.jsonl")
	_r0=$(ident_h "$_P/movie.jsonl")
	_cs0=$(canary_state)
	find "$BAR" -mindepth 1 -delete
	true >"$PUBRCPT" || { OBS="the rename receipt could not be created"; SETUP=1; return 1; }
	CC_NAME=pubsig
	bg_run pubsig "$1" --write testdata/parser -- sh -c "$BOTH"
	if ! cc_wait_file "$BAR/ready"; then
		cc_setup "the publication barrier was never reached"
		return 1
	fi
	_pm=ok
	case $PUBM in
	partial) [ "$(cat "$PUBRCPT")" = "rename series.jsonl" ] || _pm="the first rename is not the only one that took effect" ;;
	backup) { [ -z "$(cat "$PUBRCPT")" ] && [ -n "$(find "$_P" -maxdepth 1 -name '.series.jsonl.*.prev')" ]; } || _pm="the first backup was not prepared with no rename yet" ;;
	post) { [ "$(cat "$_P/series.jsonl")" = newS ] && [ "$(cat "$_P/movie.jsonl")" = newR ]; } || _pm="both renames did not complete before the commit barrier" ;;
	esac
	if [ "$_pm" != ok ]; then
		cc_setup "$_pm"
		return 1
	fi
	kill -s TERM "$BG_PID"
	if ! cc_ack 30; then
		cc_setup "no acknowledgement of the handled signal"
		return 1
	fi
	: >"$BAR/go"
	bg_end pubsig
	reached
	_nint=$(grep -c '^dev-run: interrupted$' "$TMP/rerr")
	_dots=$(find "$_P" -name '.*' ! -name . | wc -l | tr -d ' ')
	_rc=$(tr '\n' ' ' <"$PUBRCPT")
	OBS="$PUBM: status $RS, interrupted lines $_nint, series '$(cat "$_P/series.jsonl" 2>/dev/null)' movie '$(cat "$_P/movie.jsonl" 2>/dev/null)', owned dot entries $_dots, renames '$_rc'"
	{ [ "$RS" = 125 ] && [ "$_nint" = 1 ] && [ "$_cs0" = "$(canary_state)" ] && ! grep -Fq 'PASS' "$TMP/rout"; } || return 1
	case $PUBM in
	partial)
		[ "$(cat "$_P/series.jsonl")" = "prior series" ] && [ "$(ident_h "$_P/series.jsonl")" = "$_s0" ] && [ "$(cat "$_P/movie.jsonl")" = "prior movie" ] &&
			[ "$(ident_h "$_P/movie.jsonl")" = "$_r0" ] && [ "$_dots" = 0 ] && [ "$_rc" = "rename series.jsonl " ]
		;;
	backup)
		[ "$(cat "$_P/series.jsonl")" = "prior series" ] && [ "$(ident_h "$_P/series.jsonl")" = "$_s0" ] && [ "$(cat "$_P/movie.jsonl")" = "prior movie" ] &&
			[ "$(ident_h "$_P/movie.jsonl")" = "$_r0" ] && [ "$_dots" = 0 ] && [ -z "$_rc" ]
		;;
	post)
		[ "$(cat "$_P/series.jsonl")" = newS ] && [ "$(cat "$_P/movie.jsonl")" = newR ] && [ "$_dots" = 0 ] && [ "$_rc" = "rename series.jsonl rename movie.jsonl " ]
		;;
	esac
}
PUBM=partial
gvariant fault-pub-partial controller.go "$RENAME_OLD" "$PUBREN_HOLD"
gvariant mut-pub-partial controller.go "$RENAME_OLD" "$PUBREN_HOLD" "$PUBCHK1_OLD" "$PUBCHK1_NEW" "$PUBCHK2_OLD" "$PUBCHK2_NEW"
redgreen "TERM between two renames: the cancellation checks removed publish through the signal" a_pub_signal "$MUT/mut-pub-partial.sh" "$MUT/fault-pub-partial.sh"
PUBM=backup
gvariant fault-pub-backup controller.go "$RENAME_OLD" "$PUBREN_NEW" "$PUBPREP_OLD" "$PUBPREP_NEW"
gvariant mut-pub-backup controller.go "$RENAME_OLD" "$PUBREN_NEW" "$PUBPREP_OLD" "$PUBPREP_NEW" "$PUBPREPCHK_OLD" '_ = c.cancelled.Load()'
redgreen "TERM during backup preparation: the backup-phase check removed renames anyway" a_pub_signal "$MUT/mut-pub-backup.sh" "$MUT/fault-pub-backup.sh"
PUBM=post
gvariant fault-pub-post controller.go "$RENAME_OLD" "$PUBREN_NEW" "$PUBPOST_OLD" "$PUBPOST_NEW"
gvariant mut-pub-post controller.go "$RENAME_OLD" "$PUBREN_NEW" "$PUBPOST_OLD" "$PUBPOST_NEW" "$PUBPOSTRB_OLD" "$PUBPOSTRB_NEW"
redgreen "TERM after the commit point: rolling back after the commit loses the published files" a_pub_signal "$MUT/mut-pub-post.sh" "$MUT/fault-pub-post.sh"
rm -rf "$ISO/testdata"
# NC04 capture: the same real TERM, held at the last cancellation check of the capture publication (all five names already linked) and after the capture is committed (post)
CAPM=final
a_cap_signal() { # a_cap_signal RUNNER (CAPM final|post)
	find "$BAR" -mindepth 1 -delete
	CAP=$(capdir)
	CC_NAME=capsig
	_cs0=$(canary_state)
	bg_run capsig "$1" --capture "$CAP" -- sh -c "${BASE}shoot_all"
	if ! cc_wait_file "$BAR/ready"; then
		cc_setup "the capture publication barrier was never reached"
		return 1
	fi
	_n5=$(find "$ISO/$CAP" -mindepth 1 -maxdepth 1 | wc -l | tr -d ' ')
	if [ "$_n5" != 5 ]; then
		cc_setup "the capture was not fully linked at the barrier ($_n5 entries)"
		return 1
	fi
	kill -s TERM "$BG_PID"
	if ! cc_ack 30; then
		cc_setup "no acknowledgement of the handled signal"
		return 1
	fi
	: >"$BAR/go"
	bg_end capsig
	reached
	_nint=$(grep -c '^dev-run: interrupted$' "$TMP/rerr")
	_ents=$(find "$ISO/$CAP" -mindepth 1 -maxdepth 1 -printf '%f\n' 2>/dev/null | LC_ALL=C sort | tr '\n' ' ')
	OBS="$CAPM: status $RS, interrupted lines $_nint, DIR $([ -e "$ISO/$CAP" ] && echo present || echo absent) holding '$_ents'"
	_ok=0
	if [ "$RS" = 125 ] && [ "$_nint" = 1 ] && [ "$_cs0" = "$(canary_state)" ] && ! grep -Fq 'DEV-RUN-CAPTURE-PASS' "$TMP/rout"; then
		case $CAPM in
		final) [ ! -e "$ISO/$CAP" ] && _ok=1 ;;
		post) [ "$_ents" = "capture.manifest dev-desktop.png dev-mobile.png preview-desktop.png preview-mobile.png " ] && _ok=1 ;;
		esac
	fi
	rm -rf "$ISO/$CAP"
	[ "$_ok" = 1 ]
}
CAPFIN_OLD='if c.cancelled.Load() { // the last check: past it the capture is committed'
CAPFIN_NEW="$(gobar 180)"'
	if c.cancelled.Load() { // the last check: past it the capture is committed'
CAPFINCHK_OLD='if c.cancelled.Load() { // the last check: past it the capture is committed
		return reasonCancelled
	}'
CAPPOST_OLD='c.captureOK = true'
CAPPOST_NEW='c.captureOK = true
				'"$(gobar 180)"
CAPABORT_OLD='if c.captureClaimed && !c.captureOK {'
CAPABORT_NEW='if c.captureClaimed && (!c.captureOK || c.cancelled.Load()) {'
CAPM=final
gvariant fault-cap-final controller.go "$CAPFIN_OLD" "$CAPFIN_NEW"
gvariant mut-cap-final controller.go "$CAPFIN_OLD" "$CAPFIN_NEW" "$CAPFINCHK_OLD" '_ = 0'
redgreen "TERM in the final capture window (all five names linked): without the last check the capture is committed through the signal" a_cap_signal "$MUT/mut-cap-final.sh" "$MUT/fault-cap-final.sh"
CAPM=post
gvariant fault-cap-post controller.go "$CAPPOST_OLD" "$CAPPOST_NEW"
gvariant mut-cap-post controller.go "$CAPPOST_OLD" "$CAPPOST_NEW" "$CAPABORT_OLD" "$CAPABORT_NEW"
redgreen "TERM after the capture is committed: aborting a committed capture on cancellation removes it" a_cap_signal "$MUT/mut-cap-post.sh" "$MUT/fault-cap-post.sh"
# NC13: the final capture locator is delivered to a stdout that is closed or blocked (a full pipe) at a barrier right before it is written; the capture stays published, the status is 125 output_write_failed
ANNM=closed
a_announce() { # a_announce RUNNER (ANNM closed|blocked)
	find "$BAR" -mindepth 1 -delete
	CAP=$(capdir)
	rm -f "$TMP/ann.fifo"
	mkfifo "$TMP/ann.fifo" || { OBS="the stdout fifo could not be made"; SETUP=1; return 1; }
	exec 4<>"$TMP/ann.fifo"
	# the child scope closes its inherited copy of the read/write end once its own stdout writer is open and before exec: a copy left in the controller would stay a reader
	(cd "$ISO" && exec 4<&- && exec "$1" --capture "$CAP" -- sh -c "${BASE}shoot_all") >"$TMP/ann.fifo" 2>"$TMP/ann.err" &
	BG_PID=$!
	_i=0
	while [ ! -e "$BAR/ready" ] && kill -0 "$BG_PID" 2>/dev/null && [ "$_i" -lt 900 ]; do
		sleep 1
		_i=$((_i + 1))
	done
	_n5=$(find "$ISO/$CAP" -mindepth 1 -maxdepth 1 2>/dev/null | wc -l | tr -d ' ')
	if [ ! -e "$BAR/ready" ] || [ "$_n5" != 5 ]; then
		OBS="the barrier before the final locator was not reached with a committed capture ($_n5 entries)"
		SETUP=1
		exec 4<&-
		kill -KILL "$BG_PID" 2>/dev/null
		wait "$BG_PID" 2>/dev/null
		rm -rf "$ISO/$CAP"
		return 1
	fi
	case $ANNM in
	closed)
		exec 4<&-
		# the closed-reader premise is established before the barrier is released: a non-blocking write open of a FIFO with no reader fails (ENXIO)
		if dd if=/dev/null of="$TMP/ann.fifo" bs=1 count=0 oflag=nonblock 2>/dev/null; then
			OBS="the stdout fifo still has a reader after the driver and the child closed theirs"
			SETUP=1
			: >"$BAR/go"
			kill -KILL "$BG_PID" 2>/dev/null
			wait "$BG_PID" 2>/dev/null
			rm -rf "$ISO/$CAP"
			return 1
		fi
		;;
	blocked)
		dd if=/dev/zero bs=65536 count=1 >&4 2>/dev/null
		if dd if=/dev/zero bs=1 count=1 oflag=nonblock >&4 2>/dev/null; then
			OBS="the stdout pipe was not full"
			SETUP=1
			: >"$BAR/go"
			exec 4<&-
			kill -KILL "$BG_PID" 2>/dev/null
			wait "$BG_PID" 2>/dev/null
			rm -rf "$ISO/$CAP"
			return 1
		fi
		;;
	esac
	: >"$BAR/go"
	_i=0
	while kill -0 "$BG_PID" 2>/dev/null && [ "$_i" -lt 60 ]; do
		sleep 1
		_i=$((_i + 1))
	done
	_held=0
	if kill -0 "$BG_PID" 2>/dev/null; then
		_held=1
		kill -KILL "$BG_PID" 2>/dev/null
	fi
	wait "$BG_PID" 2>/dev/null
	RS=$?
	exec 4<&- 2>/dev/null
	cp "$TMP/ann.err" "$TMP/rerr"
	rerr_seen
	_cn=$(cname)
	[ -z "$_cn" ] || OWNED="$OWNED $_cn"
	cstate "${_cn:-boxset-dev-none}"
	_ents=$(find "$ISO/$CAP" -mindepth 1 -maxdepth 1 -printf '%f\n' 2>/dev/null | LC_ALL=C sort | tr '\n' ' ')
	OBS="$ANNM: status $RS, still held by the sink after 60 s: $_held, container ${_cn:-none} $CST, DIR holds '$_ents', $(grep -o 'dev-run: [a-z_]*' "$TMP/rerr" | tail -n 1)"
	reached
	_ok=0
	[ "$_held" = 0 ] && [ "$RS" = 125 ] && grep -Fq 'dev-run: output_write_failed' "$TMP/rerr" && [ "$CST" = absent ] &&
		[ "$_ents" = "capture.manifest dev-desktop.png dev-mobile.png preview-desktop.png preview-mobile.png " ] && _ok=1
	rm -rf "$ISO/$CAP"
	[ "$_ok" = 1 ]
}
ANN_OLD='if c.capture != "" && c.captureOK && !c.announce('
ANN_NEW='if c.capture != "" && c.captureOK {
		'"$(gobar 180)"'
	}
	if c.capture != "" && c.captureOK && !c.announce('
ANN_MUT_OLD='// the capture stays published: nothing after the commit point rolls back
			c.die("output_write_failed", statusFail)'
ANN_MUT_NEW='c.setStatus(0)'
gvariant fault-announce controller.go "$ANN_OLD" "$ANN_NEW"
gvariant mut-announce controller.go "$ANN_OLD" "$ANN_NEW" "$ANN_MUT_OLD" "$ANN_MUT_NEW"
for ANNM in closed blocked; do
	redgreen "final capture locator to a $ANNM stdout: a delivery failure ignored leaves status 0 although the locator never arrived" a_announce "$MUT/mut-announce.sh" "$MUT/fault-announce.sh"
done
# relative PATH (T26): forwarding timeout and Docker shims in a relative entry under the original repository cwd serve the bootstrap and every controller call
a_relpath() { # a_relpath RUNNER
	rm -rf "$ISO/rb"
	mkdir -p "$ISO/rb" || { SETUP=1; return 1; }
	: >"$TMP/relpath.rcpt"
	_rt=$(command -v timeout)
	_rd=$(command -v docker)
	printf '#!/bin/sh\nprintf "timeout %%s PATH=%%s\\n" "$*" "$PATH" >>"%s"\nexec "%s" "$@"\n' "$TMP/relpath.rcpt" "$_rt" >"$ISO/rb/timeout"
	printf '#!/bin/sh\nprintf "docker %%s PATH=%%s\\n" "$*" "$PATH" >>"%s"\nexec "%s" "$@"\n' "$TMP/relpath.rcpt" "$_rd" >"$ISO/rb/docker"
	chmod +x "$ISO/rb/timeout" "$ISO/rb/docker" || { SETUP=1; return 1; }
	_rp=rb:$PATH
	# the lookup premise, independent of the controller: through this PATH the relative entry is the first match and is found as a relative path
	_rv=$(cd "$ISO" && PATH=$_rp && command -v timeout)
	[ "$_rv" = rb/timeout ] || { OBS="the relative lookup premise failed ('$_rv')"; SETUP=1; rm -rf "$ISO/rb"; return 1; }
	(cd "$ISO" && PATH=$_rp "$1" -- sh -c 'printf rel; exit 3') >"$TMP/rout" 2>"$TMP/xerr"
	RS=$?
	# an intended refusal (a missing-executable reading of the relative lookup) is the fixed image_build_failed of the controller's first native call; only that exact line is exempt from infrastructure attribution
	_spoken=0
	intended_refusal image_build_failed "$TMP/xerr" && _spoken=1
	_cn=$(cname)
	[ -z "$_cn" ] || OWNED="$OWNED $_cn"
	_ro=$(cat "$TMP/rout")
	_rr=$(grep -c '^timeout .*docker run ' "$TMP/relpath.rcpt")
	_rx=$(grep -c '^timeout .*docker exec ' "$TMP/relpath.rcpt")
	_rdk=$(grep -c '^docker run ' "$TMP/relpath.rcpt")
	_rq=$(grep -c '^docker ps ' "$TMP/relpath.rcpt")
	_rbad=$(grep -vcF "PATH=$_rp" "$TMP/relpath.rcpt")
	_rw=$(grep -c -- '--target work' "$TMP/relpath.rcpt")
	_rb=$(grep -c -- '--target host-redact' "$TMP/relpath.rcpt")
	OBS="status $RS, output '$_ro', forwarded: timeout-run $_rr, timeout-exec $_rx, docker-run $_rdk, docker-ps $_rq, work builds $_rw, bootstrap builds $_rb, receipts with a changed PATH $_rbad, image_build_failed spoken $_spoken"
	# reached: the original ran the child (identity line); the mutant is reached only by its positive refusal evidence: the bootstrap forwarded through the relative entry and the controller's work build never did
	{ { [ "$RS" = 3 ] && grep -Eq "$RUNNER_STARTED_RE" "$TMP/rerr"; } || { [ "$_spoken" = 1 ] && [ "$_rw" = 0 ] && [ "$_rb" -ge 1 ]; }; } && reached
	rm -rf "$ISO/rb"
	[ "$RS" = 3 ] && [ "$_ro" = rel ] && [ "$_rr" -ge 1 ] && [ "$_rx" -ge 1 ] && [ "$_rdk" -ge 1 ] && [ "$_rq" -ge 1 ] && [ "$_rbad" = 0 ]
}
gvariant mut-errdot-missing controller.go '	if errors.Is(cmd.Err, exec.ErrDot) {
		cmd.Err = nil
	}' '	_ = cmd'
redgreen "relative PATH entry: exec.ErrDot treated as a missing executable" a_relpath "$MUT/mut-errdot-missing.sh" "$RUNNER"
# relative TMPDIR (T26): the bootstrap resolves its private directory against the original cwd before creation, export and exec, without changing the environment
a_reltmp() { # a_reltmp RUNNER
	rm -rf "$ISO/rt"
	mkdir "$ISO/rt" || { SETUP=1; return 1; }
	printf 'canary bytes\n' >"$ISO/rt/canary"
	(cd "$ISO" && TMPDIR=rt "$1" -- sh -c 'printf rel; exit 3') >"$TMP/rout" 2>"$TMP/xerr"
	RS=$?
	_spoken=0
	intended_refusal redact_unavailable "$TMP/xerr" && _spoken=1
	_cn=$(cname)
	[ -z "$_cn" ] || OWNED="$OWNED $_cn"
	{ { [ "$RS" = 3 ] && grep -Eq "$RUNNER_STARTED_RE" "$TMP/rerr"; } || [ "$_spoken" = 1 ]; } && reached
	_ro=$(cat "$TMP/rout")
	_rl=$(ls -A "$ISO/rt" | tr '\n' ' ')
	OBS="status $RS, output '$_ro', rt holds '$_rl', canary '$(cat "$ISO/rt/canary" 2>/dev/null)'"
	_ok=0
	[ "$RS" = 3 ] && [ "$_ro" = rel ] && [ "$_rl" = "canary " ] && [ "$(cat "$ISO/rt/canary")" = "canary bytes" ] && _ok=1
	rm -rf "$ISO/rt"
	[ "$_ok" = 1 ]
}
variant mut-tmpdir-relative '_tb=$(cd -P -- "$_tb" 2>/dev/null && pwd -P) || die temp_dir_failed' '_tb=$_tb'
redgreen "relative TMPDIR: the normalization-removed bootstrap" a_reltmp "$MUT/mut-tmpdir-relative.sh" "$RUNNER"
a_tmpcol() { # a_tmpcol RUNNER (a fixed-RUNID copy): a pre-existing private-directory name is refused and never removed
	_tc=$TMP/tcol
	rm -rf "$_tc"
	mkdir -p "$_tc/boxset-dev-run.0123456789abcdef" || { SETUP=1; return 1; }
	printf 'foreign\n' >"$_tc/boxset-dev-run.0123456789abcdef/canary"
	(cd "$ISO" && TMPDIR="$_tc" "$1" -- true) >"$TMP/rout" 2>"$TMP/xerr"
	RS=$?
	intended_refusal temp_dir_failed "$TMP/xerr" && reached
	OBS="status $RS, foreign canary '$(cat "$_tc/boxset-dev-run.0123456789abcdef/canary" 2>/dev/null)'"
	[ "$RS" = 125 ] && [ "$(cat "$_tc/boxset-dev-run.0123456789abcdef/canary" 2>/dev/null)" = foreign ]
}
RUNID_OLD='RUNID=$(od -An -N8 -tx1 /dev/urandom | tr -d '"' \n'"')'
variant fixed-runid "$RUNID_OLD" 'RUNID=0123456789abcdef'
variant mut-cleanup-unowned "$RUNID_OLD" 'RUNID=0123456789abcdef' 'if [ -n "$TMPD_OWN" ] && [ "$(find "$TMPD" -maxdepth 0 -printf '"'%D:%i'"' 2>/dev/null)" = "$TMPD_ID" ]; then' 'if true; then'
redgreen "bootstrap TMPD collision: cleanup of a directory this invocation did not create" a_tmpcol "$MUT/mut-cleanup-unowned.sh" "$MUT/fixed-runid.sh"
# NC05: a backup name that already exists is never this run's. The bootstrap copy has the fixed RUNID, so the run's own backup name is known before the run; the pre-existing name keeps its bytes and inode while
# publication refuses (write_publish_failed). The mutant records ownership before the no-overwrite link succeeds, so its rollback removes what it never created.
a_prev_collision() { # a_prev_collision RUNNER (SHAPE foreign|target-link)
	_P=$ISO/testdata/parser
	rm -rf "$_P"
	mkdir -p "$_P"
	printf 'prior series\n' >"$_P/series.jsonl"
	_bk=$_P/.series.jsonl.0123456789abcdef.prev
	case $SHAPE in
	foreign) printf 'collision bytes\n' >"$_bk" ;;
	target-link) ln "$_P/series.jsonl" "$_bk" ;;
	esac
	_s0=$(ident_h "$_P/series.jsonl")
	_b0=$(ident_h "$_bk")
	_bb=$(cat "$_bk" 2>/dev/null)
	[ -n "$_b0" ] || { OBS="the pre-existing backup name could not be created"; SETUP=1; return 1; }
	rrx "$ISO" "$1" --write testdata/parser -- sh -c 'printf newS >/src/testdata/parser/series.jsonl'
	OBS="$SHAPE: status $RS, backup '$(cat "$_bk" 2>/dev/null)' (inode $(ident_h "$_bk") was $_b0), series '$(cat "$_P/series.jsonl" 2>/dev/null)'"
	{ [ "$RS" = 125 ] && grep -Fq 'write_publish_failed root=testdata/parser' "$TMP/rerr"; } && reached
	[ "$RS" = 125 ] && grep -Fq 'write_publish_failed root=testdata/parser' "$TMP/rerr" && [ "$(cat "$_bk" 2>/dev/null)" = "$_bb" ] && [ "$(ident_h "$_bk")" = "$_b0" ] &&
		[ "$(cat "$_P/series.jsonl")" = "prior series" ] && [ "$(ident_h "$_P/series.jsonl")" = "$_s0" ] && [ -z "$(find "$_P" -name '.*.part')" ]
}
PREVLINK_OLD='if err := c.wroot.Link(n, bak); err != nil {
				return c.failPub("write_publish_failed")
			}
			if got, ok := c.identOf(bak); !ok || got != qid {'
PREVLINK_NEW='c.partP[n], c.partQ[n] = pid, qid
			if err := c.wroot.Link(n, bak); err != nil {
				return c.failPub("write_publish_failed")
			}
			if got, ok := c.identOf(bak); !ok || got != qid {'
if gvariant prev-collision-orig controller.go && mutate_file "$MUT/prev-collision-orig.sh" "$RUNID_OLD" 'RUNID=0123456789abcdef'; then :; else rm -f "$MUT/prev-collision-orig.sh"; fi
if gvariant mut-prev-collision controller.go "$PREVLINK_OLD" "$PREVLINK_NEW" && mutate_file "$MUT/mut-prev-collision.sh" "$RUNID_OLD" 'RUNID=0123456789abcdef'; then :; else rm -f "$MUT/mut-prev-collision.sh"; fi
for SHAPE in foreign target-link; do
	redgreen "pre-existing backup name ($SHAPE): ownership recorded before the link is acquired removes it" a_prev_collision "$MUT/mut-prev-collision.sh" "$MUT/prev-collision-orig.sh"
done
rm -rf "$ISO/testdata"
a_rejpriv() { # a_rejpriv: a malformed private cleanup path is refused with the fixed word and never cleaned
	_rd=$TMP/rejpriv
	rm -rf "$_rd"
	mkdir -p "$_rd/keep" || { SETUP=1; return 1; }
	printf 'foreign\n' >"$_rd/keep/canary"
	(cd "$ISO" && PATH=/nonexistent "$CLOCK" -run "$ISO" 0123456789abcdef "$_rd/keep" 5 0 - - -- true) >"$TMP/rout" 2>"$TMP/xerr"
	RS=$?
	intended_refusal redact_unavailable "$TMP/xerr" && reached
	OBS="status $RS, stderr '$(cat "$TMP/xerr")', canary '$(cat "$_rd/keep/canary" 2>/dev/null)'"
	[ "$RS" = 125 ] && [ "$(cat "$TMP/xerr")" = 'dev-run: redact_unavailable' ] && [ "$(cat "$_rd/keep/canary" 2>/dev/null)" = foreign ]
}
holds "a rejected private TMPD (wrong basename) is dev-run: redact_unavailable with status 125 and the foreign directory survives" a_rejpriv

# ===== 10. host loss: the in-container PID 1 backstop (isolated, proven-live, test-owned) =====================
# Every owned name is observed against its own absolute deadline by a test-owned watcher that keeps running while later
# cases start, so a late disappearance is never credited. Every time here is CLOCK_MONOTONIC through the shared `redact -clock` reader: no StartedAt, wall reading
# or conversion exists. The owned deadline is Docker's recorded container-start event plus D (a main container TIMEOUT + 60 + 10 s, a validator 21 s). Its lower
# bound S is the start reading of the case's single logging-shim record of the `run` call naming the container (taken before the shim execs the real CLI); its upper
# bound R is the first driver reading after `docker container inspect --format {{.State.Running}}` answered exactly `true` (a witness of the start event only, never of
# a timer inside the container) plus one 10 ms floor. The registration is the window [S+D, R+D]: the true deadline is at least S+D and below R+D. Only status-checked
# queries count; a failed query is unknown, never absence.
#   gone             a query that completed before lo (floor charged) found the name absent: the only GREEN evidence.
#   expired-present  a query that started at or after hi found the name still present: survival at the deadline, the RED
#                    evidence of a mutant (the original survivor criterion). A last in-time sighting proves nothing.
#   late-gone        the first query at or after hi found it absent: it may have disappeared in the unobserved final
#                    fraction, so the tail is unknown and is never GREEN (and never RED).
# Each case owns its contamination (launcher, readiness polling, runner stderr) in $HLD/KEY.taint; the deferred verdict
# loads it (hl_flags) and never resets it away. Mutants are judged by the same predicate.
HLCMD='touch /tmp/ready; setsid sh -c "touch /tmp/gready; exec sleep 300" >/dev/null 2>&1 & while [ ! -e /tmp/gready ]; do sleep 1; done; exec sleep 300'
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
hl_watcher &
HL_WATCH=$!
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
hl_kill() { # hl_kill NAME: runner alive, SIGKILL succeeds
	kill -0 "$HL_PID" 2>/dev/null || { fail "host loss $1" "runner not alive before the kill"; return 1; }
	kill -KILL "$HL_PID" || { fail "host loss $1" "SIGKILL failed"; return 1; }
	wait "$HL_PID" 2>/dev/null || true
	return 0
}
# hl_reap_helper CONTEXT: a forced runner loss at a host barrier leaves the capture-stage subshell
# in its (bounded) barrier loop; kill exactly the recorded test-owned helper and verify it is gone.
hl_reap_helper() {
	if [ ! -s "$BAR/helper.pid" ]; then fail "$1" "the barrier seam recorded no helper pid"; return; fi
	_hrp=$(cat "$BAR/helper.pid")
	kill -KILL "$_hrp" 2>/dev/null
	_hri=0
	while kill -0 "$_hrp" 2>/dev/null && [ "$_hri" -lt 10 ]; do
		sleep 1
		_hri=$((_hri + 1))
	done
	if kill -0 "$_hrp" 2>/dev/null; then
		fail "$1" "the barrier helper $_hrp survived the forced loss"
	else
		ctl_begin
		pass "$1: no orphan barrier helper survives the forced loss"
	fi
}
gvariant mut-nobackstop controller.go 'args = append(args, c.img, "/usr/bin/sleep", strconv.Itoa(c.timeout+60))' 'args = append(args, c.img, "/usr/bin/sleep", "86400")'
gvariant mut-norm controller.go 'args := []string{"run", "-d", "--rm", "--name", c.ctr,' 'args := []string{"run", "-d", "--name", c.ctr,'
gvariant mut-late-backstop controller.go 'args = append(args, c.img, "/usr/bin/sleep", strconv.Itoa(c.timeout+60))' 'args = append(args, c.img, "/usr/bin/sleep", strconv.Itoa(c.timeout+60+40))'
hl_case() { # hl_case KEY RUNNER ARGS...: after readiness the container's running witness R is read, the runner is killed, and the case is registered with its own window; the kill must complete before S + TIMEOUT
	_nm=$1
	if hl_start "$@"; then
		if hl_running "$HL_CID" && hl_kill "$_nm"; then
			_kd=
			mono_ns && _kd=$MONO_NS
			hl_err_taint "$_nm" "$_nm"
			rg_reset
			reached
			if [ -n "$_kd" ] && dlogs_same "$_nm" && hl_register "$_nm" "$HL_CID" "$TMP/$_nm.dlog" "$HL_R" 130000000000 && [ "$((_kd + MONO_RES))" -le "$((HL_S + 60000000000))" ]; then
				pass "host loss $_nm: runner killed before its own S+TIMEOUT"
			else
				hl_taint "$_nm" setup "the shim run record, the running witness or the kill time could not be established"
				fail "host loss $_nm" "no single shim run record, no running witness, or the kill came too late (S '${HL_S:-none}', kill '${_kd:-unread}')"
			fi
			return 0
		fi
		kill -KILL "$HL_PID" 2>/dev/null
		wait "$HL_PID" 2>/dev/null || true
		hl_taint "$_nm" setup "the container was not proven running or the runner was not killable"
		fail "host loss $_nm" "the running witness failed or the runner could not be killed"
	fi
	return 1
}
hl_case orig "$RUNNER" --timeout 60 -- sh -c "$HLCMD"
hl_case nobackstop "$MUT/mut-nobackstop.sh" --timeout 60 -- sh -c "$HLCMD"
hl_case norm "$MUT/mut-norm.sh" --timeout 60 -- sh -c "$HLCMD"
hl_case late "$MUT/mut-late-backstop.sh" --timeout 60 -- sh -c "$HLCMD"
CAP=$(capdir)
hl_case cap "$RUNNER" --timeout 60 --capture "$CAP" -- sh -c "$HLCMD"
CAPHL=$CAP
# transfer-phase host loss: held at the transfer barrier, then SIGKILL
find "$BAR" -mindepth 1 -delete
CAP=$(capdir)
CAPTR=$CAP
gvariant fault-transfer-hold controller.go 'sum += pr.n // staged and checked' 'sum += pr.n // staged and checked
		if name == "dev-desktop" { '"$(gobar 180)"' }'
bg_runlog hltr "$MUT/fault-transfer-hold.sh" --timeout 60 --capture "$CAP" -- sh -c "${BASE}shoot_all"
HL_PID=$BG_PID
HLC_tr=$(bg_ctr hltr) || HLC_tr=
_i=0
while [ ! -e "$BAR/ready" ] && [ "$_i" -lt 900 ]; do kill -0 "$HL_PID" 2>/dev/null || break; sleep 1; _i=$((_i + 1)); done
rg_reset
if [ -e "$BAR/ready" ] && kill -0 "$HL_PID" 2>/dev/null && hl_running "$HLC_tr" && kill -KILL "$HL_PID"; then
	wait "$HL_PID" 2>/dev/null || true
	_kd=
	mono_ns && _kd=$MONO_NS
	hl_err_taint tr hltr
	reached
	if [ -n "$_kd" ] && dlogs_same hltr && hl_register tr "$HLC_tr" "$TMP/hltr.dlog" "$HL_R" 130000000000 && [ "$((_kd + MONO_RES))" -le "$((HL_S + 60000000000))" ]; then
		pass "host loss at the transfer barrier: runner killed while transferring"
	else
		hl_taint tr setup "the shim run record, the running witness or the kill time could not be established"
		fail "host loss transfer" "no single shim run record, no running witness, or the kill came too late"
	fi
	hl_reap_helper "host loss at the transfer barrier"
else
	fail "host loss transfer" "barrier not reached, the container not proven running or the runner not killable"
fi
# stalled validator after runner loss: the validator is gone by its own S+11+10 and the runner-less main container by its own S+TIMEOUT+60+10. The
# readiness polling's own queries are case-owned: a failed one is recorded as that case's unknown observation, whatever a later query shows.
find "$BAR" -mindepth 1 -delete
CAP=$(capdir)
CAPST=$CAP
gvariant fault-stalled controller.go '"/usr/local/libexec/boxset/capture-check", "-name", name, "-challenge", c.challenge}' '"/usr/bin/sleep", "100"}'
bg_runlog hlst "$MUT/fault-stalled.sh" --timeout 60 --capture "$CAP" -- sh -c "${BASE}shoot_all"
HL_PID=$BG_PID
HLC_st=$(bg_ctr hlst) || HLC_st=
_rid=${HLC_st#boxset-dev-}
VN=boxset-dev-$_rid-v-dev-desktop
_i=0
_vs=unknown
while [ "$_i" -lt 600 ]; do
	_vs=$(cstate_to 30 "$VN")
	if [ "$_vs" = unknown ]; then
		hl_taint stval unknown "readiness query of $VN"
		hl_taint stmain unknown "readiness query of $VN"
	fi
	[ "$_vs" != present ] || break
	kill -0 "$HL_PID" 2>/dev/null || break
	sleep 1
	_i=$((_i + 1))
done
rg_reset
if [ "$_vs" = present ] && hl_running "$VN" && _rv=$HL_R && hl_running "$HLC_st" && _rm=$HL_R && kill -KILL "$HL_PID"; then
	wait "$HL_PID" 2>/dev/null || true
	_kd=
	mono_ns && _kd=$MONO_NS
	hl_err_taint stval hlst
	hl_err_taint stmain hlst
	reached
	if [ -n "$_kd" ] && dlogs_same hlst && hl_register stval "$VN" "$TMP/hlst.dlog" "$_rv" 21000000000 && hl_register stmain "$HLC_st" "$TMP/hlst.dlog" "$_rm" 130000000000 && [ "$((_kd + MONO_RES))" -le "$((HL_S + 60000000000))" ]; then
		pass "host loss with a stalled validator: runner killed while the validator runs"
	else
		hl_taint stval setup "the shim run records, a running witness or the kill time could not be established"
		hl_taint stmain setup "the shim run records, a running witness or the kill time could not be established"
		fail "host loss stalled validator" "no single shim run record, no running witness, or the kill came too late"
	fi
else
	fail "host loss stalled validator" "validator container never appeared, not proven running or runner not killable"
fi
# a real controller fatal after readiness (a compiled panic raised once the driver has seen the child ready) taints its deferred case whatever the container later does
find "$BAR" -mindepth 1 -delete
gvariant fault-ctrl-fatal controller.go 'func (c *ctl) runChild() (cs, rs int, tmo, ok bool) {' 'func (c *ctl) runChild() (cs, rs int, tmo, ok bool) {
	go func() {
		for i := 0; i < 1800; i++ {
			if _, err := os.Stat("'$BAR'/fatal"); err == nil {
				panic("injected controller fatal")
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()'
rg_reset
if hl_start hlfatal "$MUT/fault-ctrl-fatal.sh" --timeout 60 -- sh -c "$HLCMD"; then
	: >"$BAR/fatal"
	_i=0
	while kill -0 "$HL_PID" 2>/dev/null && [ "$_i" -lt 60 ]; do
		sleep 1
		_i=$((_i + 1))
	done
	wait "$HL_PID" 2>/dev/null || true
	if grep -Eq "$CTRL_FATAL_RE" "$TMP/hlfatal.err" 2>/dev/null && hl_running "$HL_CID"; then
		hl_err_taint fatal hlfatal
		dlogs_same hlfatal && hl_register fatal "$HL_CID" "$TMP/hlfatal.dlog" "$HL_R" 130000000000 || hl_taint fatal setup "the shim run record or the running witness could not be established"
	else
		hl_taint fatal setup "the controller fatal was not observed with the container still running"
		fail "host loss controller fatal" "no fatal trace on the runner's stderr, or the container was not running afterwards"
	fi
fi
# every registered case resolves at its own deadline; unregistered cases already failed above
_i=0
while [ "$_i" -lt 200 ]; do
	_open=0
	for _k in orig nobackstop norm late cap tr stval stmain fatal; do
		if [ -e "$HLD/$_k.reg" ] && [ ! -e "$HLD/$_k.res" ]; then _open=1; fi
	done
	[ "$_open" = 0 ] && break
	sleep 3
	_i=$((_i + 1))
done
true >"$HLD/stop" || fail "host loss watcher" "the watcher could not be stopped"
wait "$HL_WATCH" 2>/dev/null || true
hl_prov() { # hl_prov RUNNER: the wrapper sha and, for a compiled copy, the controller source sha it holds beside the original controller sha (the same provenance redgreen records)
	_hpv="runner sha $(fhash "$1")"
	if [ -s "${1%.sh}.csha" ]; then _hpv="$_hpv, controller sha $(cat "${1%.sh}.csha") (original controller sha $(fhash "$ISO/deploy/dev/redact/controller.go"))"; fi
	printf '%s' "$_hpv"
}
hl_green() { # hl_green KEY CONTEXT RUNNER: absent by a status-checked query inside its own deadline, through the shared classifier
	if hl_credit green "$1"; then pass "$2 (absence observed $(((HR_D - HR_T) / 1000000)) ms before its own deadline, floor charged; $(hl_prov "$3"))"; else fail "$2" "$RG_WHY result $HR_R (observed at $HR_T, own deadline lower bound $HR_D; $(hl_prov "$3"))"; fi
}
hl_red() { # hl_red KEY CONTEXT MUTANT ORIGINAL: the same predicate fails on the mutant: a query that started at or after its own deadline still found it present
	_hos=$(fhash "$4")
	if [ ! -s "${3%.sh}.csha" ] || [ "$(cat "${3%.sh}.csha")" = "$(fhash "$ISO/deploy/dev/redact/controller.go")" ]; then
		fail "$2" "the mutant carries no compiled-controller provenance (its controller sha record is missing or equals the original controller)"
		return
	fi
	if hl_credit red "$1"; then pass "$2 RED on mutant ($(hl_prov "$3")): still present to a status-checked query that started at or after its own deadline (the restored original runner sha $_hos is GREEN above)"; else fail "$2" "$RG_WHY result $HR_R on mutant ($(hl_prov "$3")) (observed at $HR_T, own deadline lower bound $HR_D)"; fi
}
hl_green orig "host loss: runner-less container gone by its StartedAt+TIMEOUT+60+10 (child and detached grandchild with it)" "$RUNNER"
hl_red nobackstop "host loss: backstop-disabled mutant (restored original GREEN above)" "$MUT/mut-nobackstop.sh" "$RUNNER"
hl_red norm "host loss: no---rm mutant (restored original GREEN above)" "$MUT/mut-norm.sh" "$RUNNER"
hl_red late "host loss: late-backstop mutant that disappears after its own deadline but before the batch's latest" "$MUT/mut-late-backstop.sh" "$RUNNER"
hl_green cap "host loss during the capture child: container gone by its own deadline" "$RUNNER"
hl_green tr "host loss at the transfer barrier: container gone by its own deadline" "$MUT/fault-transfer-hold.sh"
hl_green stmain "host loss with a stalled validator: the runner-less main container gone by its own deadline" "$MUT/fault-stalled.sh"
hl_green stval "host loss with a stalled validator: the named validator gone by its own StartedAt+11+10" "$MUT/fault-stalled.sh"
ctl_begin
for _d in "$CAPHL" "$CAPTR" "$CAPST"; do
	[ -d "$ISO/$_d" ] || SETUP=1
	expect "host loss: nothing published into $_d" "$(find "$ISO/$_d" \( -name '*.png' -o -name capture.manifest \) 2>/dev/null | wc -l | tr -d ' ')" 0
done
for _o in cap hltr hlst; do [ -e "$TMP/$_o.out" ] || SETUP=1; done
expect "host loss: no runner printed a capture PASS" "$(cat "$TMP/cap.out" "$TMP/hltr.out" "$TMP/hlst.out" 2>/dev/null | grep -c '^DEV-RUN-CAPTURE-PASS')" 0
for _c in $(cat "$HLD/nobackstop.reg" "$HLD/norm.reg" "$HLD/late.reg" "$HLD/fatal.reg" 2>/dev/null | cut -d' ' -f1); do docker rm -f "$_c" >/dev/null 2>&1; done
# the controller fatal case resolved gone, yet its fatal taint refuses both polarities (no credit through the deferred classifier)
_hg=n
_hr=n
hl_credit green fatal && _hg=G
hl_credit red fatal && _hr=R
META=1
expect "host loss with a controller fatal after readiness: the container is gone by its deadline, so the case would be GREEN, but its fatal taint refuses both polarities" "$HR_R:$_hg$_hr" "gone:nn"
expect "host loss with a controller fatal after readiness: the taint is exactly the fatal output" "$(cat "$HLD/fatal.taint" 2>/dev/null)" "setup controller fatal output"
META=0
rm -rf "$ISO/$CAPHL" "$ISO/$CAPTR" "$ISO/$CAPST"

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
	(cd "$_cd" && BOXSET_TEST_SECRET=https "$_cd/scripts/dev-run.sh" --write docs/spikes/issue-drafts -- sh -c 'printf "%s" "https://u:p@h/x?k=v"; printf "see https://u:p@h/x?k=v end" >/src/docs/spikes/issue-drafts/s5-admission.md; exit 1') >"$TMP/rout" 2>"$TMP/rerr"
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
wantpass() { want "$1" 0 "^S5-ADMISSION-PASS blocked=0 unlisted=0"; }
wantnot() { # wantnot NAME STATUS PATTERN BADPATTERN: the intended outcome and no unintended block
	if ! printf "%s\n" "$out" | grep -Eq "$4"; then want "$1" "$2" "$3"; else echo "MUT $1 FAIL: unintended output: $(printf "%s\n" "$out" | head -n 4 | tr "\n" "|")"; fi
}
osids=$(grep -E "^\| ADM-[0-9]+ \| apt \| os-(library|font|data) \|" /src/docs/spikes/s5-version-admission.md | cut -d" " -f2)
want_all_os() { # want_all_os NAME PATTERN: every one of the 28 OS rows is blocked with PATTERN
	_n=0; _bad=0
	for id in $osids; do _n=$((_n + 1)); printf "%s\n" "$out" | grep -Eq "^ADMISSION-BLOCKED $id: $2" || _bad=1; done
	if [ "$SETUP_BAD" = 0 ] && [ "$st" = 1 ] && [ "$_bad" = 0 ] && [ "$_n" = 28 ]; then echo "MUT $1 PASS"; else echo "MUT $1 FAIL: status $st, os rows $_n (want 28), missing=$_bad, setup=$SETUP_BAD"; fi
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
reset; sedx "$REP" "/^| ADM-022 /s/| 2.8.3-1~deb13u1 |/| >=2.8.3-1~deb13u1 |/"; admit
want apt-range-row 1 "ADMISSION-BLOCKED ADM-022: floating"
reset; sedx "$REP" "/^| ADM-022 /s/| 2.8.3-1~deb13u1 |/| * |/"; admit
want apt-wildcard-row 1 "ADMISSION-BLOCKED ADM-022: floating"
reset; sedx "$DF" "s/    libexpat1=2.8.3-1~deb13u1 /    libexpat1>=2.8.3-1~deb13u1 /"; admit
want apt-range-dockerfile 1 "ADMISSION-BLOCKED ADM-022"
reset; sedx "$DF" "s/    libexpat1=2.8.3-1~deb13u1 /    libexpat1=* /"; admit
want apt-wildcard-dockerfile 1 "ADMISSION-BLOCKED ADM-022"
reset; sedx "$DF" "s/    libexpat1=2.8.3-1~deb13u1 /    libexpat1=2.8.3 /"; admit
want literal-tilde-version-mismatch 1 "ADMISSION-BLOCKED ADM-022"
reset; d=$(dago 366); sedx "$REP" "/^| ADM-004 /s/ 2026-10-10 / $d /"; admit
want annual-366-days 1 "ADMISSION-BLOCKED ADM-004: maintenance_stale"
reset; d=$(dago 365); sedx "$REP" "/^| ADM-004 /s/ 2026-10-10 / $d /"; admit
wantpass annual-365-days-admits
reset; sedx "$REP" "/^| ADM-004 /s/ release c6da4a4d3010e5553438c60f6bd76d981976867c / release 20261005T000000Z /"; admit
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
echo MUTATIONS-DONE'
rr --timeout 600 -- sh -c "$MUTS"
# every scenario credit needs the script to have completed: its runner status 0 and its closing marker once; an aborted
# script is setup, never a per-scenario outcome
if [ "$RS" != 0 ] || [ "$(grep -c '^MUTATIONS-DONE$' "$TMP/rout")" != 1 ]; then SETUP=1; fi
expect "admission mutation script runner exits 0" "$RS" 0
expect "admission mutation script completed" "$(grep -c '^MUTATIONS-DONE$' "$TMP/rout")" 1
# The required scenario list is fixed here, independent of what the script printed.
REQ_MUTS='original-structure gcc-newer-same-source-work-admits tampered-checksum floating-caret floating-tilde floating-star apt-range-row apt-wildcard-row apt-range-dockerfile apt-wildcard-dockerfile literal-tilde-version-mismatch annual-366-days annual-365-days-admits snapshot-id-as-maintenance snapshot-timestamp-as-maintenance snapshot-url-as-maintenance-bun missing-p07 unlisted-base-image category-npm-as-os-library category-archive-as-os-library category-apt-fetch-as-os-data category-gcc-as-os-library category-qemu-as-os-library category-image-as-os-data category-os-data-name-outside-set os-row-with-release-evidence direct-tool-with-distro-support snapshot-url-as-maintenance gcc-work-subject-gcc14 gcc-366-days os-codename-bookworm os-security-until-past dockerfile-suite-forky support-limited-libx11 support-ended-today support-ends-tomorrow-admits support-supported-override support-full-string-match support-invalid-unknown-status-all-os-rows support-invalid-nonsupported-without-version-all-os-rows support-invalid-bad-date-all-os-rows support-invalid-bad-pattern-all-os-rows support-flipped-fenced-byte-all-os-rows support-url-commit-differs-all-os-rows support-release-12-all-os-rows support-second-block-all-os-rows support-missing-block-all-os-rows support-obsolete-ended-file-all-os-rows row-status-differs-from-evaluation row-lists-commit-differs source-mismatch licence-installed-values-admit licence-flipped-sha licence-flipped-dep5 licence-missing-sha licence-missing-dep5'
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
/src/scripts/s5-toolchain.sh --admission --root $R >$R/baseline.out 2>&1; grep -Eq "^S5-ADMISSION-PASS blocked=0 unlisted=0" $R/baseline.out || { echo FIXTURE-FAIL baseline; exit 98; }'
# a_s5_scanner SCRIPT: an unlisted dependency in each of the four package.json sections, in compact
# JSON (no indentation to scan), is reported by the real reader; every section must be found.
a_s5_scanner() {
	s5_install "$1" "$ISO/scripts/s5-mut.sh" || { OBS="the admission script could not be installed"; SETUP=1; return 1; }
	rrx "$ISO" "$RUNNER" --timeout 300 -- sh -c 'for sec in dependencies devDependencies optionalDependencies peerDependencies; do R=/tmp/m; rm -rf $R && mkdir -p $R/deploy/dev $R/docs/spikes $R/web && cp /src/deploy/dev/Dockerfile $R/deploy/dev/ && cp /src/docs/spikes/s5-version-admission.md $R/docs/spikes/ && cp /src/web/package.json /src/web/bun.lock $R/web/ && cp /src/go.mod $R/ || { echo "SETUP-FAIL $sec"; continue; }; if ! S5_PKG=$R/web/package.json S5_SEC=$sec bun -e "const fs=require(\"fs\");const p=JSON.parse(fs.readFileSync(process.env.S5_PKG,\"utf8\"));p[process.env.S5_SEC]=p[process.env.S5_SEC]||{};p[process.env.S5_SEC][\"left-pad\"]=\"1.3.0\";fs.writeFileSync(process.env.S5_PKG,JSON.stringify(p));" || ! grep -q left-pad $R/web/package.json; then echo "SETUP-FAIL $sec"; continue; fi; out=$(/src/scripts/s5-mut.sh --admission --root $R 2>&1); st=$?; printf "%s\n" "$out" | grep "^ADMISSION-BLOCKED " | head -n 3 | sed "s/^/DETAIL $sec /"; echo "SECTION $sec STATUS $st COUNT $(printf "%s\n" "$out" | grep -c "ADMISSION-UNLISTED npm left-pad@1.3.0") OTHER $(printf "%s\n" "$out" | grep -E "^ADMISSION-(BLOCKED|UNLISTED) " | grep -vc "ADMISSION-UNLISTED npm left-pad@1.3.0") DONE $(printf "%s\n" "$out" | grep -Ec "^S5-ADMISSION-(PASS|FAIL) ")"; done; echo SCANNER-DONE'
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

# failing admission drafts, written in place through the fixed map. Early missing-input drafts are
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
a_admission_capfull() { # holds when a full draft root makes S5 report the draft write failure and publish nothing
	s5_install "$1" "$FISO/scripts/s5-toolchain.sh" || { OBS="the admission script could not be installed"; SETUP=1; return 1; }
	find "$FISO/docs/spikes/issue-drafts" -mindepth 1 -delete
	rrx "$FISO" "$FISO/scripts/dev-run.sh" --write docs/spikes/issue-drafts --timeout 300 -- sh -c 'head -c 33554432 /dev/zero >/src/docs/spikes/issue-drafts/s5-tools.md; [ "$(stat -c %s /src/docs/spikes/issue-drafts/s5-tools.md)" = 33554432 ] && echo FILL-OK; /src/scripts/s5-toolchain.sh --admission'
	OBS="status $RS, $(grep -c S5-DRAFT-WRITE-FAILED "$TMP/rout") failure lines, fill $(grep -c '^FILL-OK$' "$TMP/rout")"
	# the target: the draft root is really full and the failing admission reached its draft write
	[ "$(grep -c '^FILL-OK$' "$TMP/rout")" = 1 ] && grep -Fq 'ADMISSION-BLOCKED inputs' "$TMP/rout" && s5_done && reached
	[ "$RS" = 1 ] && grep -Fq 'S5-DRAFT-WRITE-FAILED name=s5-admission.md' "$TMP/rout" && grep -Fq 'dev-run: write_cap_reached root=docs/spikes/issue-drafts' "$TMP/rerr" && [ ! -e "$FISO/docs/spikes/issue-drafts/s5-admission.md" ]
}
mutate_file_copy s5-draft-error-swallowed 'if ! cat "$T/draft.out" >"$DRAFT" 2>/dev/null; then' 'if false; then'
redgreen "draft-write-error-swallowing admission mutant" a_admission_capfull "$MUT/s5-draft-error-swallowed.sh" "$SRC/scripts/s5-toolchain.sh"
redgreen "draft-dropping mutant under a full draft root" a_admission_capfull "$MUT/s5-draft-dropped.sh" "$SRC/scripts/s5-toolchain.sh"
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
a_cat_removed() { a_s5_report "$1" '/^| ADM-115 /s/| npm-dependency |/| os-library |/' 'ADMISSION-BLOCKED ADM-115: category_invalid' 'ADM-115'; }
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
libx11 supported override' '/^| ADM-031 /s/status=clear/status=supported/' 1 'ADMISSION-BLOCKED ADM-032: distro_unsupported' 'ADMISSION-BLOCKED ADM-031' 'ADM-03[12]'; }
a_full_string() { a_support_ctl "$1" 'libx1 limited prefix only' 's/^$//' 0 '^S5-ADMISSION-PASS blocked=0 unlisted=0' '' '' 'ADMISSION-BLOCKED ADM-031: distro_unsupported'; }
redgreen "supported-override-ignored mutant" a_supported_wins "$MUT/s5-supported-ignored.sh" "$SRC/scripts/s5-toolchain.sh"
redgreen "prefix-match mutant" a_full_string "$MUT/s5-prefix-match.sh" "$SRC/scripts/s5-toolchain.sh"
# A matcher evaluation error (grep status above 1) is never a nonmatch: through the real script every
# OS row is evidence_invalid and none becomes clear. The fault copy changes only the matcher seam.
a_eval_error() {
	a_support_ctl "$1" '' 's/^$//' 1 'ADMISSION-BLOCKED ADM-013: evidence_invalid: support-list pattern evaluation failed' '' '' 'ADMISSION-BLOCKED ADM-[0-9]+: evidence_invalid: support-list pattern evaluation failed'
	_r=$?
	_n=$(grep -c '^ADMISSION-BLOCKED ADM-[0-9]*: evidence_invalid: support-list pattern evaluation failed' "$TMP/rout")
	OBS="$OBS; $_n OS rows evidence_invalid (want 28)"
	[ "$_r" = 0 ] && [ "$_n" = 28 ]
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
mutate_file_copy s5-draft-on-success "printf 'S5-ADMISSION-PASS blocked=0 unlisted=0 rows=%s\\n' \"\$ROWS\"" "printf 'S5-ADMISSION-PASS blocked=0 unlisted=0 rows=%s\\n' \"\$ROWS\"; [ -z \"\$DRAFT\" ] || printf stale >\"\$DRAFT\""
RA_TARGET=pass
redgreen "draft-written-on-success mutant" a_real_admission "$MUT/s5-draft-on-success.sh" "$SRC/scripts/s5-toolchain.sh"

# The root-owned H2 currency procedure (pre-run check, actual admission, post-run check,
# evidence acceptance and changed-post invalidation) is root evidence in the feature
# workspace; no copied decision helper in this driver stands in for it.

# ===== 13. ecosystem-scoped lock export and the next-consumer refresh (T01/T27) =================================
if [ ! -f "$ISO/web/bun.lock" ]; then
	fail "lock export" "web/bun.lock is absent: run the documented web lock bootstrap first (docs/spikes/s5-version-admission.md)"
else
	locksum=$(sha256sum <"$ISO/web/bun.lock")
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
mkdir -p "$ISO/testdata/parser" "$ISO/internal/refreshprobe"
printf 'fixture one\n' >"$ISO/testdata/parser/series.jsonl"
printf 'package refreshprobe\n\nconst Version = 1\n' >"$ISO/internal/refreshprobe/probe.go"
CONSUME='sha256sum /src/testdata/parser/series.jsonl /src/internal/refreshprobe/probe.go /src/go.mod /src/web/bun.lock'
hostsums() { (cd "$ISO" && for f in testdata/parser/series.jsonl internal/refreshprobe/probe.go go.mod web/bun.lock; do set -- $(sha256sum "$f"); printf '%s  /src/%s\n' "$1" "$f"; done); }
rr -- sh -c "$CONSUME"
first=$(cat "$TMP/rout")
expect "first consumer reports the host fixture, Go source, go.mod and bun.lock hashes" "$RS:$first" "0:$(hostsums)"
printf 'fixture two, changed on the host\n' >"$ISO/testdata/parser/series.jsonl"
printf 'package refreshprobe\n\nconst Version = 2\n' >"$ISO/internal/refreshprobe/probe.go"
a_refresh() { # holds when the next consumer observes exactly the new bytes
	rrx "$ISO" "$1" -- sh -c "$CONSUME"
	OBS="status $RS, consumer saw: $(head -c 120 "$TMP/rout" | tr '\n' ' ')"
	[ "$RS" = 0 ] && [ "$(grep -Ec '^[0-9a-f]{64}  /src/' "$TMP/rout")" = 4 ] && reached
	[ "$RS" = 0 ] && [ "$(cat "$TMP/rout")" = "$(hostsums)" ]
}
fiso stale-context
gvariant mut-refresh-skipped controller.go '"--progress", "plain", "."}' '"--progress", "plain", "'$TMP'/stale-context"}'
rm -rf "$TMP/stale-context"
mv "$FISO" "$TMP/stale-context"
printf 'fixture one\n' >"$TMP/stale-context/testdata/parser/series.jsonl" 2>/dev/null || true
printf 'package refreshprobe\n\nconst Version = 1\n' >"$TMP/stale-context/internal/refreshprobe/probe.go"
redgreen "refresh-skipped mutant (stale build context)" a_refresh "$MUT/mut-refresh-skipped.sh" "$RUNNER"
rm -rf "$TMP/stale-context" "$ISO/testdata" "$ISO/internal/refreshprobe"

if [ "$FAILS" -eq 0 ]; then
	printf 'DEV-RUN-TEST-PASS controls=%s\n' "$PASSES"
	exit 0
fi
printf 'DEV-RUN-TEST-FAIL failed=%s passed=%s\n' "$FAILS" "$PASSES"
exit 1
