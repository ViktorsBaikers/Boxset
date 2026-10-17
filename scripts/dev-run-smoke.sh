#!/bin/sh
# Small host-only runner/sentinel check (never part of the image context):
#   scripts/dev-run-smoke.sh [--tools-failure]
# Runs the real scripts/dev-run.sh and Docker from a unique, ownership-checked
# scratch snapshot of the admitted project inputs (explicit roots, with the final
# nested secret and cache exclusions of .dockerignore applied before archiving,
# so no owner-shaped nested file is ever copied), so repository drafts are never
# touched. Seven direct assertions print `ok R<n>` or `not ok R<n>`;
# `DEV-RUN-SMOKE-PASS checks=7` prints only when all seven pass, any failure exits 1.
# R1 also runs the `--ref` root and pinned-commit rejections of the neutral reference values
# (BOXSET_REF_SERIES, BOXSET_REF_MOVIE) and R3 the pinned Git-object delivery, source-rule, producer-
# and receiver-after-delivery and readback-cap controls, R5 the corpus export identity, R6 the producer
# unit tests and R7 the Go admission negatives, all against synthetic repositories built only inside the scratch.
# `--tools-failure` instead runs the real eight-tool script in the scratch copy through ten
# controls, each on a real child and Docker: a genuine compiler syntax error carrying the
# sentinel (exactly one start record, redacted draft with tool/file/line), an unwritable draft
# destination, a diagnostic log the child itself made unreadable (a real EACCES while the draft is
# built), a preview server that never completes its response body, a real eight-tool success
# next to a pre-seeded host draft, the capture rejection table (missing, empty, link, oversize,
# malformed, wrong-dimension, wrong-challenge, selected-word diagnostic, no raw host staging),
# missing or malformed capture variables, a non-none validator log driver (observed never started),
# a failed host sink write (observed after a validator that exited 0) and a readable diagnostic log
# whose last 40 lines are far over both the 6000-byte draft cap and a pipe buffer (the capped draft
# is still written); only then `S5-FAILURE-PROOF-PASS draft=retained downstream=0` prints.
set -u
LC_ALL=C
export LC_ALL

MODE=checks
case ${1:-} in
'') ;;
--tools-failure) MODE=tools-failure ;;
*)
	echo 'usage: scripts/dev-run-smoke.sh [--tools-failure]' >&2
	exit 2
	;;
esac
[ $# -le 1 ] || {
	echo 'usage: scripts/dev-run-smoke.sh [--tools-failure]' >&2
	exit 2
}

{ [ -f scripts/dev-run.sh ] && [ -f deploy/dev/Dockerfile ] && [ -f .dockerignore ]; } || {
	echo 'dev-run-smoke: run from the repository root' >&2
	exit 2
}
S=$(mktemp -d) || exit 2
OWN=1
trap '[ -z "$OWN" ] || { [ -d "$S" ] && [ ! -L "$S" ] && [ -O "$S" ] && rm -rf "$S"; }' EXIT
trap 'exit 1' HUP INT TERM
{ [ -d "$S" ] && [ ! -L "$S" ] && [ -O "$S" ]; } || exit 2

# snap SRC TAR: archive the inputs .dockerignore admits (explicit roots and subtrees plus
# the two host scripts the checks run) with every final nested exclusion of .dockerignore
# (and web/build, web/test-results) applied by name before tar reads anything. A missing
# optional root is skipped; an unreadable one fails the archive.
snap() {
	(
		cd "$1" || exit 2
		_l=
		for _p in .dockerignore go.mod go.sum .golangci.yml deploy/dev/Dockerfile deploy/dev/capture-check deploy/dev/redact internal bench testdata web scripts/dev-run.sh scripts/s5-toolchain.sh docs/spikes/*.md; do
			if [ -e "$_p" ] || [ -L "$_p" ]; then _l="$_l $_p"; fi
		done
		tar -c -f "$2" --exclude=.env '--exclude=.env.*' '--exclude=*.pem' '--exclude=*.key' --exclude=.npmrc --exclude=.netrc \
			--exclude=.git --exclude=.devrites --exclude=node_modules --exclude=.svelte-kit --exclude=.cache \
			--exclude=web/build --exclude=web/test-results $_l
	)
}
# An entry name that must never be in a snapshot (listing lines, one name per line).
SECRET_RE='(^|/)(\.env(\.[^/]*)?|[^/]*\.pem|[^/]*\.key|\.npmrc|\.netrc|\.git|\.devrites|node_modules|\.svelte-kit|\.cache)(/|$)|^web/(build|test-results)(/|$)'
mkdir "$S/repo" "$S/empty" || exit 2
snap . "$S/ctx.tar" && tar -x -f "$S/ctx.tar" -C "$S/repo" || exit 2

SENTINEL=synthetic-not-a-credential
URL='https://synthetic-user:synthetic-url-pass@example.invalid/p?token=synthetic-query-secret'
export BOXSET_SECRET_SENTINEL=$SENTINEL DEV_RUN_SMOKE_HOST_MARKER=1
ALLOWED='PATH HOME TMPDIR LANG LC_ALL GOPATH GOCACHE GOMODCACHE GOFLAGS GOPROXY GOSUMDB GOTOOLCHAIN GOTELEMETRY GOENV GOLANGCI_LINT_CACHE BUN_INSTALL_CACHE_DIR BUN_RUNTIME_TRANSPILER_CACHE_PATH XDG_CACHE_HOME PLAYWRIGHT_BROWSERS_PATH PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD DO_NOT_TRACK PWD OLDPWD SHLVL _ HOSTNAME'
DRAFTS=$S/repo/docs/spikes/issue-drafts
RC=
RRENV=

# rr ARGS...: the scratch copy's own runner (under the extra `env` words in $RRENV);
# merged output in $S/out, status in $RC.
rr() {
	(cd "$S/repo" && exec env $RRENV sh ${RSH:-scripts/dev-run.sh} "$@") </dev/null >"$S/out" 2>&1
	RC=$?
}

# ctr_gone: the container named by the runner's identification line in $S/out is removed.
ctr_gone() {
	_c=$(sed -n 's/.* container=\([^ ]*\) log=none$/\1/p' "$S/out")
	[ -n "$_c" ] && ! docker inspect "$_c" >/dev/null 2>&1
}

# names_ok LINE: every name after the leading `envnames` word is in the enumerated list.
names_ok() {
	_seen=0
	for _n in $(sed -n 's/^envnames//p' "$S/out"); do
		_seen=1
		case " $ALLOWED " in *" $_n "*) ;; *) return 1 ;; esac
	done
	[ "$_seen" = 1 ]
}

# T26 `--ref` fixtures, all inside the unique scratch and never from an owner reference. TD is a
# smoke-owned directory of links to exactly the host tools the runner's `--ref` validation and pin
# check need (never docker), TDNG the same without git. REFD carries a unique non-secret marker. The
# synthetic repositories hold the runner's three upstream directories with small .cs files (one
# nested) and one committed .cs outside them: plain (a directory, no Git), git and git2 (distinct
# repositories whose retained control commit is followed by a later HEAD commit changing every file, a
# staged edit, working-tree edits and an untracked .cs file in each directory), git2/inner (nested in
# git2, no repository of its own) and bad, whose three commits lack the qualities directory, add a
# symlink blob in the parser directory and add one .cs blob of 2 MiB + 1 byte.
# sgit DIR ARGS...: Git on a fixture only: cleared environment, the tool directory as PATH, scratch
# HOME, no system or global configuration, a smoke-local identity and empty owned template and hooks
# directories; no inherited GIT_* value ever enters.
sgit() {
	_gd=$1
	shift
	env -i PATH="$TD" HOME="$S/githome" LC_ALL=C GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null \
		GIT_AUTHOR_NAME=smoke GIT_AUTHOR_EMAIL=smoke@example.invalid GIT_COMMITTER_NAME=smoke GIT_COMMITTER_EMAIL=smoke@example.invalid \
		git -C "$_gd" -c core.hooksPath="$S/gh" -c init.templateDir="$S/gt" -c commit.gpgsign=false "$@"
}
REFPAIRS='tests:src/NzbDrone.Core.Test/ParserTests parser:src/NzbDrone.Core/Parser qualities:src/NzbDrone.Core/Qualities'
# ref_repo DIR TAG: a repository whose first commit is the control commit.
ref_repo() {
	mkdir -p "$1" && sgit "$1" init -q "--template=$S/gt" -b main || return 1
	for _p in $REFPAIRS; do
		_u=${_p#*:}
		mkdir -p "$1/$_u/sub" && printf 'class A%s {}\n' "$2" >"$1/$_u/a.cs" && printf 'class N%s {}\n' "$2" >"$1/$_u/sub/n.cs" || return 1
	done
	mkdir -p "$1/outside" && printf 'class O%s {}\n' "$2" >"$1/outside/o.cs" &&
		sgit "$1" add -A && sgit "$1" commit -q -m "control $2"
}
# ref_dirty DIR: a later HEAD commit changing every file, then a staged edit, working-tree edits and
# an untracked .cs file in each directory.
ref_dirty() {
	for _f in $(sgit "$1" ls-files); do
		printf '// later\n' >>"$1/$_f" || return 1
	done
	sgit "$1" add -A && sgit "$1" commit -q -m later || return 1
	_k=0
	for _p in $REFPAIRS; do
		_u=${_p#*:}
		_k=$((_k + 1))
		printf 'class U {}\n' >"$1/$_u/untracked.cs" && printf '// worktree\n' >>"$1/$_u/sub/n.cs" || return 1
		if [ "$_k" = 1 ]; then
			printf '// staged\n' >>"$1/$_u/a.cs" && sgit "$1" add "$_u/a.cs" || return 1
		else
			printf '// worktree\n' >>"$1/$_u/a.cs" || return 1
		fi
	done
}
# ref_head DIR: the commit HEAD names.
ref_head() { sgit "$1" rev-parse --verify 'HEAD^{commit}'; }
ref_fixtures() {
	MARK=refmark$(od -An -N6 -tx1 /dev/urandom | tr -d ' \n')
	REFD=$S/$MARK
	TD=$S/tools
	TDNG=$S/tools-nogit
	mkdir "$TD" "$TDNG" "$S/githome" "$S/gh" "$S/gt" "$S/pin" "$REFD" "$REFD/plain" || return 1
	for _t in tr wc timeout env; do
		_p=$(command -v "$_t") && [ -x "$_p" ] && ln -s "$_p" "$TD/$_t" && ln -s "$_p" "$TDNG/$_t" || return 1
	done
	_p=$(command -v git) && [ -x "$_p" ] && ln -s "$_p" "$TD/git" || return 1
	ref_repo "$REFD/git" S && CS=$(ref_head "$REFD/git") && ref_dirty "$REFD/git" || return 1
	ref_repo "$REFD/git2" M && CM=$(ref_head "$REFD/git2") && ref_dirty "$REFD/git2" || return 1
	mkdir "$REFD/git2/inner" || return 1
	ref_repo "$REFD/bad" B && _bc=$(ref_head "$REFD/bad") || return 1
	set -- $REFPAIRS
	_t0=${1#*:}
	_pa=${2#*:}
	_qu=${3#*:}
	sgit "$REFD/bad" rm -q -r "$_qu" && sgit "$REFD/bad" commit -q -m noq && BADQ=$(ref_head "$REFD/bad") && sgit "$REFD/bad" reset -q --hard "$_bc" &&
		ln -s ../a.cs "$REFD/bad/$_pa/link.cs" && sgit "$REFD/bad" add -A && sgit "$REFD/bad" commit -q -m link && BADL=$(ref_head "$REFD/bad") && sgit "$REFD/bad" reset -q --hard "$_bc" &&
		head -c 2097153 /dev/zero | tr '\0' x >"$REFD/bad/$_t0/big.cs" && sgit "$REFD/bad" add -A && sgit "$REFD/bad" commit -q -m big && BADB=$(ref_head "$REFD/bad") || return 1
	[ "${#CS}" = 40 ] && [ "${#CM}" = 40 ] && [ "$CS" != "$CM" ] || return 1
	_nl='
'
	_tab=$(printf '\t')
	cp -R "$REFD/plain" "$REFD/p${_nl}n" && cp -R "$REFD/plain" "$REFD/p${_tab}t"
}
# pin_copy OUT SERIESPIN MOVIEPIN: a smoke-owned copy of the scratch runner whose side table holds
# other pins; each real pin must occur exactly once in the runner and each new pin once in the copy.
pin_copy() {
	set -- "$1" "$2" "$3" $(sed -n "s/^REFPINS='series:\([0-9a-f]*\) movie:\([0-9a-f]*\)'\$/\1 \2/p" "$S/repo/scripts/dev-run.sh")
	[ $# = 5 ] || return 1
	for _p in "$4" "$5"; do
		[ "$(grep -o "$_p" "$S/repo/scripts/dev-run.sh" | wc -l | tr -d ' ')" = 1 ] || return 1
	done
	sed -e "s/$4/$2/" -e "s/$5/$3/" "$S/repo/scripts/dev-run.sh" >"$1" || return 1
	[ "$(grep -o "$2" "$1" | wc -l | tr -d ' ')" = 1 ] && [ "$(grep -o "$3" "$1" | wc -l | tr -d ' ')" = 1 ]
}
# r1_ref WORD TOOLS SERIES MOVIE [RUNNER]: the runner's `--ref` run (the scratch runner unless a pinned
# copy is given) from `env -i` with only the tool directory TOOLS as PATH and the two selected
# NAME=value words (`-` leaves a variable unset), so owner BOXSET_REF_* values never reach it: status
# 125 and merged output exactly `dev-run: WORD`, never a root value, scratch path, sentinel or Git
# text. A run that reached Docker could only report host_tool_missing.
r1_ref() {
	_w=$1
	_tl=$2
	_sv=$3
	_mv=$4
	_rn=${5:-scripts/dev-run.sh}
	set -- env -i "PATH=$_tl"
	[ "$_sv" = - ] || set -- "$@" "BOXSET_REF_SERIES=$_sv"
	[ "$_mv" = - ] || set -- "$@" "BOXSET_REF_MOVIE=$_mv"
	(cd "$S/repo" && exec "$@" /bin/sh "$_rn" --ref -- true) </dev/null >"$S/out" 2>&1
	[ $? = 125 ] && [ "$(cat "$S/out")" = "dev-run: $_w" ]
}
# r1_ref_side SIDE: every root rejection of one side alone, the other side being the plain directory.
r1_ref_side() {
	for _c in unset empty relative newline tab missing; do
		case $_c in
		unset) _v=- _w=ref_unavailable ;;
		empty) _v= _w=ref_unavailable ;;
		relative) _v=../$MARK/plain _w=ref_invalid ;;
		newline) _v="$REFD/p${_nl}n" _w=ref_invalid ;;
		tab) _v="$REFD/p${_tab}t" _w=ref_invalid ;;
		missing) _v=$REFD/$SENTINEL _w=ref_invalid ;;
		esac
		if [ "$1" = series ]; then
			r1_ref "$_w side=series" "$TD" "$_v" "$REFD/plain"
		else
			r1_ref "$_w side=movie" "$TD" "$REFD/plain" "$_v"
		fi || return 1
	done
}
# r1_ref_commits: the pin check of each side. The unmodified scratch runner enforces its real pins
# (the checkout's HEAD is never used); with a copy pinned to the control commits, a root no Git
# repository answers for (no Git, no repository, a repository lacking the pin, a pin that names a
# blob, a directory nested in another repository) is ref_commit_invalid for the first failing side.
r1_ref_commits() {
	set -- $REFPAIRS
	_bl=$(sgit "$REFD/git" rev-parse --verify "$CS:${1#*:}/a.cs") || return 1
	pin_copy "$S/pin/ok.sh" "$CS" "$CM" && pin_copy "$S/pin/blob.sh" "$_bl" "$CM" || return 1
	r1_ref 'ref_commit_invalid side=series' "$TD" "$REFD/git" "$REFD/git2" &&
		r1_ref 'ref_commit_invalid side=series' "$TDNG" "$REFD/plain" "$REFD/plain" "$S/pin/ok.sh" &&
		r1_ref 'ref_commit_invalid side=series' "$TD" "$REFD/plain" "$REFD/plain" "$S/pin/ok.sh" &&
		r1_ref 'ref_commit_invalid side=series' "$TD" "$REFD/git2" "$REFD/git2" "$S/pin/ok.sh" &&
		r1_ref 'ref_commit_invalid side=series' "$TD" "$REFD/git" "$REFD/git2" "$S/pin/blob.sh" &&
		r1_ref 'ref_commit_invalid side=movie' "$TD" "$REFD/git" "$REFD/git2/inner" "$S/pin/ok.sh"
}
# lbl: the containers carrying any dev-run label, sorted.
lbl() { docker ps -aq --filter label=boxset.dev-run | LC_ALL=C sort; }
# r3_expect SIDE REPO COMMIT: one `f SHA256 444 0 /ref/SIDE/AREA/REL` line per .cs file of the control
# commit under the three upstream directories, the bytes read as Git objects (never the checkout).
r3_expect() {
	for _p in $REFPAIRS; do
		_a=${_p%%:*}
		_u=${_p#*:}
		for _f in $(sgit "$2" ls-tree -r --name-only "$3" -- "$_u/" | grep '[.]cs$'); do
			_h=$(sgit "$2" cat-file blob "$3:$_f" | sha256sum) || return 1
			printf 'f %s 444 0 /ref/%s/%s/%s\n' "${_h%% *}" "$1" "$_a" "${_f#"$_u"/}"
		done
	done | LC_ALL=C sort
}
# r3_differs REPO COMMIT: no delivered file equals the later HEAD, staged or working-tree bytes.
r3_differs() {
	for _p in $REFPAIRS; do
		_u=${_p#*:}
		for _f in $(sgit "$1" ls-tree -r --name-only "$2" -- "$_u/" | grep '[.]cs$'); do
			_c=$(sgit "$1" cat-file blob "$2:$_f" | sha256sum) || return 1
			for _o in "$(sgit "$1" cat-file blob "HEAD:$_f" | sha256sum)" "$(sgit "$1" cat-file blob ":$_f" | sha256sum)" "$(sha256sum <"$1/$_f")"; do
				[ "${_o%% *}" != "${_c%% *}" ] || return 1
			done
		done
	done
}
# R3SCRIPT: what the --ref child reports: its mount table, environment, file listing with digest, mode and
# owner, and whether it can create, overwrite or unlink under each side.
R3SCRIPT='sed "s/^/MI /" /proc/self/mountinfo; env | sed "s/^/ENV /"; for f in $(find /ref -type f | LC_ALL=C sort); do printf "f %s %s %s\n" "$(sha256sum <"$f" | cut -c1-64)" "$(stat -c "%a %u" "$f")" "$f"; done; for s in series movie; do c=fail; o=fail; u=fail; touch /ref/$s/tests/new.cs 2>/dev/null && c=ok; { printf x >>/ref/$s/tests/a.cs; } 2>/dev/null && o=ok; rm -f /ref/$s/tests/a.cs 2>/dev/null && u=ok; [ -f /ref/$s/tests/a.cs ] || u=ok; printf "attempt %s create=%s overwrite=%s unlink=%s\n" "$s" "$c" "$o" "$u"; done'
# r3_pinned: the pinned-object delivery on git (series) and git2 (movie) whose paths carry the marker. The
# created container has no mount; the merged output holds neither the marker nor any scratch root; each
# side's file set is exactly the control commit's .cs files under the three directories with the pinned
# blobs' digests (never the later HEAD, staged, working-tree, untracked or outside bytes), read-only
# files owned by 0; with conflicting host commit values and a GIT_DIR/GIT_WORK_TREE aimed at the other
# repository the child still observes exactly the two pins. This proves delivery, not extraction.
r3_pinned() {
	_cs=1111111111111111111111111111111111111111
	_cm=2222222222222222222222222222222222222222
	RSH=$S/pin/ok.sh
	RRENV="BOXSET_REF_SERIES=$REFD/git BOXSET_REF_MOVIE=$REFD/git2 BOXSET_REF_SERIES_COMMIT=$_cs BOXSET_REF_MOVIE_COMMIT=$_cm GIT_DIR=$REFD/git2/.git GIT_WORK_TREE=$REFD/git2"
	rr --ref -- sh -c "$R3SCRIPT"
	RRENV=
	RSH=
	{ r3_expect series "$REFD/git" "$CS" && r3_expect movie "$REFD/git2" "$CM"; } | LC_ALL=C sort >"$S/want.files" || return 1
	grep '^f ' "$S/out" | LC_ALL=C sort >"$S/got.files"
	[ "$RC" = 0 ] && [ -s "$S/want.files" ] && cmp -s "$S/want.files" "$S/got.files" &&
		[ "$(wc -l <"$S/got.files" | tr -d ' ')" = 12 ] &&
		r3_differs "$REFD/git" "$CS" && r3_differs "$REFD/git2" "$CM" &&
		grep -q '^MI ' "$S/out" &&
		grep -Fxq 'attempt series create=fail overwrite=fail unlink=fail' "$S/out" &&
		grep -Fxq 'attempt movie create=fail overwrite=fail unlink=fail' "$S/out" &&
		grep -Fxq "ENV BOXSET_REF_SERIES_COMMIT=$CS" "$S/out" &&
		grep -Fxq "ENV BOXSET_REF_MOVIE_COMMIT=$CM" "$S/out" &&
		! grep -Fq -e "$MARK" -e "$S" -e "$_cs" -e "$_cm" "$S/out" &&
		! grep -Eq '^ENV (GIT_|BOXSET_REF_(SERIES|MOVIE)=)' "$S/out" &&
		grep -q ' log=none ref=objects$' "$S/out" || return 1
	for _n in $(sed -n 's/^ENV \([A-Za-z_][A-Za-z0-9_]*\)=.*/\1/p' "$S/out"); do
		case " $ALLOWED BOXSET_REF_SERIES_COMMIT BOXSET_REF_MOVIE_COMMIT " in *" $_n "*) ;; *) return 1 ;; esac
	done
}
# r3_bad: a pinned copy with series pinned to each bad commit (no qualities directory, a symlink blob
# in parser, a .cs blob of 2 MiB + 1 byte) and movie to git2's control commit, run with --write
# testdata/parser over pre-seeded corpora and a child that would print REF-CHILD-RAN: status 125 and
# exactly `dev-run: ref_invalid side=series`, no child, corpora unchanged, no part file, no new
# labelled container.
r3_bad() {
	_cd=$S/repo/testdata/parser
	[ -f "$_cd/series.jsonl" ] && [ -f "$_cd/movie.jsonl" ] && sha256sum "$_cd/series.jsonl" "$_cd/movie.jsonl" >"$S/seed.sum" || return 1
	for _bc in $BADQ $BADL $BADB; do
		pin_copy "$S/pin/bad.sh" "$_bc" "$CM" || return 1
		lbl >"$S/lbl.before"
		RSH=$S/pin/bad.sh
		RRENV="BOXSET_REF_SERIES=$REFD/bad BOXSET_REF_MOVIE=$REFD/git2"
		rr --ref --write testdata/parser -- sh -c 'echo REF-CHILD-RAN'
		RRENV=
		RSH=
		[ "$RC" = 125 ] && [ "$(cat "$S/out")" = 'dev-run: ref_invalid side=series' ] && ! grep -q REF-CHILD-RAN "$S/out" &&
			sha256sum -c "$S/seed.sum" >/dev/null 2>&1 && [ -z "$(find "$_cd" -name '*.part*' -print)" ] &&
			[ "$(lbl)" = "$(cat "$S/lbl.before")" ] || return 1
	done
}
# r3_fault_setup: the smoke-owned fault pieces of the delivery controls. A copy of the pinned runner
# has its single producer call site wrapped (exactly one occurrence is required) by a forwarding
# producer: it runs the unchanged real producer, records its actual status and exits with DR_FAULT_EXIT,
# or with that status when DR_FAULT_EXIT is empty. A forwarding docker shim runs the real docker and,
# for the delivery `docker cp -`, records its actual status and exits with DR_SHIM_EXIT, or with that
# status when DR_SHIM_EXIT is empty; for the readback of the series side it can instead stream the
# file DR_SHIM_READBACK. R3DK is the real docker.
r3_fault_setup() {
	mkdir "$S/fault" "$S/shim" || return 1
	[ "$(grep -cF '"$R" -ref-archive' "$S/pin/ok.sh")" = 1 ] &&
		sed 's|"\$R" -ref-archive|"$DR_FAULT" "$R" -ref-archive|' "$S/pin/ok.sh" >"$S/fault/dev-run.sh" &&
		[ "$(grep -cF '"$DR_FAULT" "$R" -ref-archive' "$S/fault/dev-run.sh")" = 1 ] || return 1
	cat >"$S/fault/producer.sh" <<'EOF' || return 1
#!/bin/sh
real=$1
shift
"$real" "$@"
s=$?
printf '%s' "$s" >"$DR_FAULT_STATUS"
exit "${DR_FAULT_EXIT:-$s}"
EOF
	cat >"$S/shim/docker" <<'EOF' || return 1
#!/bin/sh
if [ "${1:-}" = cp ] && [ "${3:-}" = - ] && [ -n "${DR_SHIM_READBACK:-}" ]; then
	case ${2:-} in *:/ref/series) exec cat "$DR_SHIM_READBACK" ;; esac
fi
"$DR_SHIM_REAL" "$@"
s=$?
if [ "${1:-}" = cp ] && [ "${2:-}" = - ]; then
	printf '%s' "$s" >"$DR_SHIM_STATUS"
	exit "${DR_SHIM_EXIT:-$s}"
fi
exit "$s"
EOF
	chmod 0755 "$S/fault/producer.sh" "$S/shim/docker" || return 1
	R3DK=$(command -v docker) || return 1
	sha256sum "$S/repo/testdata/parser/series.jsonl" "$S/repo/testdata/parser/movie.jsonl" >"$S/seed.sum"
}
# r3_fault PEXIT SEXIT: the series delivery with the real producer and the real receiver `docker cp`
# both run to completion, each status recorded from the actual command (both must be 0), and then
# one of them is made to report nonzero: PEXIT the producer (nonzero after delivery), SEXIT the
# receiver (nonzero after a real, successful copy); the other keeps its real status. The runner must
# give status 125 `ref_copy_failed side=series`, never start the child, leave both corpora unchanged
# and no part file or new labelled container. A runner that ignores the producer status, or the
# receiver status, fails here: the readback of the really delivered side would otherwise succeed and
# the child would run.
r3_fault() {
	_cd=$S/repo/testdata/parser
	rm -f "$S/fault.status" "$S/shim.status"
	lbl >"$S/lbl.before"
	RSH=$S/fault/dev-run.sh
	RRENV="PATH=$S/shim:$PATH DR_SHIM_REAL=$R3DK DR_SHIM_STATUS=$S/shim.status DR_SHIM_EXIT=$2 DR_FAULT=$S/fault/producer.sh DR_FAULT_STATUS=$S/fault.status DR_FAULT_EXIT=$1 BOXSET_REF_SERIES=$REFD/git BOXSET_REF_MOVIE=$REFD/git2"
	rr --ref --write testdata/parser -- sh -c 'echo REF-CHILD-RAN'
	RRENV=
	RSH=
	[ "$(cat "$S/fault.status" 2>/dev/null)" = 0 ] && [ "$(cat "$S/shim.status" 2>/dev/null)" = 0 ] || return 1
	[ "$RC" = 125 ] && [ "$(cat "$S/out")" = 'dev-run: ref_copy_failed side=series' ] && ! grep -q REF-CHILD-RAN "$S/out" &&
		sha256sum -c "$S/seed.sum" >/dev/null 2>&1 && [ -z "$(find "$_cd" -name '*.part*' -print)" ] &&
		[ "$(lbl)" = "$(cat "$S/lbl.before")" ]
}
# r3_cap: the readback admits exactly the producer's own envelope. The producer caps its archive at
# 18 MiB (refMaxStream); the readback of the series side, replaced by a hand-built tar of the three
# areas (owner 0/0, modes 0444/0555) of exactly that many bytes, is accepted and the child runs, while
# the same tar 2048 bytes larger (2 KiB, so the cut of the capped stream falls inside the file data,
# not the end blocks) is ref_copy_failed side=series with no child and no owned container left. The
# tar is seven headers, the one-block parser and qualities files, the two end blocks and the padded
# tests file.
r3_cap() {
	_cap=$((18 * 1048576))
	grep -Eq '^[[:space:]]*refMaxStream[[:space:]]+= 18 << 20' "$S/repo/deploy/dev/redact/ref_archive.go" &&
		[ "$(sed -n 's/^REFCAP=//p' "$S/repo/scripts/dev-run.sh")" = "$_cap" ] || return 1
	_st=$S/capside
	mkdir -p "$_st/series/tests" "$_st/series/parser" "$_st/series/qualities" &&
		printf 'class P {}\n' >"$_st/series/parser/a.cs" && printf 'class Q {}\n' >"$_st/series/qualities/a.cs" || return 1
	for _n in exact over; do
		case $_n in exact) _big=$((_cap - 5632)) ;; *) _big=$((_cap - 5632 + 2048)) ;; esac
		head -c "$_big" /dev/zero >"$_st/series/tests/a.cs" &&
			tar -c -b 1 --format=ustar --owner=0 --group=0 --numeric-owner --mode=a-w -f "$S/cap.$_n" -C "$_st" series || return 1
	done
	[ "$(wc -c <"$S/cap.exact" | tr -d ' ')" = "$_cap" ] && [ "$(wc -c <"$S/cap.over" | tr -d ' ')" = $((_cap + 2048)) ] || return 1
	for _n in exact over; do
		lbl >"$S/lbl.before"
		RSH=$S/pin/ok.sh
		RRENV="PATH=$S/shim:$PATH DR_SHIM_REAL=$R3DK DR_SHIM_STATUS=$S/shim.status DR_SHIM_READBACK=$S/cap.$_n BOXSET_REF_SERIES=$REFD/git BOXSET_REF_MOVIE=$REFD/git2"
		rr --ref -- sh -c 'echo REF-CHILD-RAN'
		RRENV=
		RSH=
		case $_n in
		exact) { [ "$RC" = 0 ] && grep -q REF-CHILD-RAN "$S/out"; } || return 1 ;;
		*) { [ "$RC" = 125 ] && [ "$(cat "$S/out")" = 'dev-run: ref_copy_failed side=series' ]; } || return 1 ;;
		esac
		[ "$(lbl)" = "$(cat "$S/lbl.before")" ] || return 1
	done
}
r3_ref() {
	r3_pinned && r3_bad && r3_fault_setup && r3_fault 1 '' && r3_fault '' 1 && r3_cap
}

# R1: argument errors give 125 and a fixed word before Docker is reachable (no PATH).
r1_one() {
	_w=$1
	shift
	(cd "$S/repo" && PATH=$S/empty exec /bin/sh scripts/dev-run.sh "$@") </dev/null >"$S/out" 2>&1
	[ $? = 125 ] && grep -qx "dev-run: $_w" "$S/out"
}
r1() {
	r1_one usage true &&
		r1_one timeout_invalid --timeout 0 -- true &&
		r1_one write_path_invalid --write ../x -- true &&
		ref_fixtures && r1_ref_side series && r1_ref_side movie && r1_ref_commits &&
		r1_one capture_flag_invalid --capture &&
		r1_one capture_destination_invalid --capture d -- true &&
		r1_one capture_destination_invalid --capture .devrites/work/Bad/screenshots-20260101T000000Z -- true &&
		r1_one capture_flag_invalid --capture .devrites/work/zz/screenshots-20260101T000000Z --bench 1 -- true &&
		r1_one bench_unavailable --bench 1 -- true &&
		mkdir -p "$S/repo/.devrites/work/zz/screenshots-20260101T000000Z" &&
		printf prior >"$S/repo/.devrites/work/zz/screenshots-20260101T000000Z/marker" &&
		r1_one capture_destination_exists --capture .devrites/work/zz/screenshots-20260101T000000Z -- true &&
		[ "$(cat "$S/repo/.devrites/work/zz/screenshots-20260101T000000Z/marker")" = prior ]
}

# R2: exact argv reaches the child; the child's status is the runner's; a child that outlives
# --timeout 1 gives 124 and leaves no owned container.
r2() {
	rr -- sh -c 'for a in "$0" "$@"; do printf "<%s>" "$a"; done; printf "\n"; exit 3' x 'a b' '' -n
	[ "$RC" = 3 ] && grep -Fxq '<x><a b><><-n>' "$S/out" || return 1
	rr --timeout 1 -- sleep 60
	[ "$RC" = 124 ] && ctr_gone
}

# R3: identity, environment names, network and log driver of the child, with a synthetic
# Docker client configuration (proxy block) in the runner's environment: it must reach
# neither build nor the container, so no proxy name or value shows up in the child.
# Both builds (the redactor build ancestry and the work stage) carry a disposable assertion
# under a fresh nonce (so no cache can satisfy it) that the proxy block is absent; the same
# assertions stop a build that is given the unsafe configuration directly.
R3CMD='printf "ids %s %s\n" "$(id -u)" "$(id -g)"; printf "envnames"; env | while IFS= read -r l; do printf " %s" "${l%%=*}"; done; printf "\n"; printf "goflags %s\nhome %s\n" "$GOFLAGS" "$HOME"; sed 1,2d /proc/net/dev | while IFS=: read -r i _; do printf "net %s\n" "${i##* }"; done'
R3PAT='^(http|https|ftp|all|no)_proxy=|127[.]0[.]0[.]1:9|synthetic-noproxy'
# r3_df TAG: the scratch Dockerfile with the two disposable assertions under TAG.
r3_df() {
	for _a in redact-build work; do
		printf "RUN : %s-%s && ! env | grep -Eiq '%s'\n" "$1" "$_a" "$R3PAT" >"$S/chk.$_a" || return 1
	done
	sed -e "/^FROM dev AS redact-build\$/r $S/chk.redact-build" -e "/^FROM dev AS work\$/r $S/chk.work" "$S/Dockerfile.orig" >"$S/Dockerfile.new" &&
		cat "$S/Dockerfile.new" >"$S/repo/deploy/dev/Dockerfile"
}
# r3_neg TARGET TAG: a direct build with the unsafe configuration stops at that site's assertion.
r3_neg() {
	(cd "$S/repo" && DOCKER_CONFIG=$S/dockercfg DOCKER_HOST=$_dh exec docker build --file deploy/dev/Dockerfile --target "$1" --progress plain . </dev/null) >"$S/out" 2>&1
	[ $? != 0 ] && grep -Fq "RUN : $2-$1 &&" "$S/out" && grep -q 'exit code: 1' "$S/out"
}
r3() {
	_dh=$(docker context inspect --format '{{.Endpoints.docker.Host}}' 2>/dev/null) && [ -n "$_dh" ] || return 1
	mkdir "$S/dockercfg" &&
		printf '{"proxies":{"default":{"httpProxy":"http://127.0.0.1:9","httpsProxy":"http://127.0.0.1:9","noProxy":"synthetic-noproxy.invalid"}}}\n' >"$S/dockercfg/config.json" &&
		cp "$S/repo/deploy/dev/Dockerfile" "$S/Dockerfile.orig" || return 1
	_t=$(od -An -N6 -tx1 /dev/urandom | tr -d ' \n')
	_ok=1
	r3_df "neg$_t" && r3_neg redact-build "neg$_t" && r3_neg work "neg$_t" || _ok=0
	if [ "$_ok" = 1 ] && r3_df "run$_t"; then
		RRENV="DOCKER_HOST=$_dh DOCKER_CONFIG=$S/dockercfg"
		rr -- sh -c "$R3CMD"
		RRENV=
	else
		_ok=0
	fi
	cat "$S/Dockerfile.orig" >"$S/repo/deploy/dev/Dockerfile" || return 1
	[ "$_ok" = 1 ] && [ "$RC" = 0 ] &&
		grep -Fxq 'ids 1000 1000' "$S/out" &&
		names_ok &&
		! grep -Eiq '^envnames.* (http|https|ftp|all|no)_proxy( |$)' "$S/out" &&
		! grep -Fq -e '127.0.0.1:9' -e synthetic-noproxy "$S/out" &&
		grep -Fxq 'goflags -mod=readonly' "$S/out" &&
		grep -Fxq 'home /home/boxset' "$S/out" &&
		grep -Fxq 'net lo' "$S/out" &&
		! grep -v '^net lo$' "$S/out" | grep -q '^net ' &&
		grep -q '^dev-run: image=sha256:[0-9a-f]* inputs=[0-9a-f]* .* log=none$' "$S/out" || return 1
	r3_ref
}

# R4: a secret value printed by the child never reaches the runner's output.
r4() {
	rr -- echo "$SENTINEL"
	[ "$RC" = 0 ] && grep -Fxq '[REDACTED]' "$S/out" && ! grep -Fq "$SENTINEL" "$S/out" && grep -q ' log=none$' "$S/out"
}

# snap_ok: the same snapshot construction, run over a synthetic tree whose nested
# secret-shaped, private and cache entries all exist, keeps the ordinary entries and
# no other (neither archive listing, extracted tree nor archive bytes), and the real
# snapshot holds no such name either.
snap_ok() {
	_d=$S/synth
	mkdir -p "$_d/internal/a/.git" "$_d/web/src" "$_d/web/build" "$_d/web/test-results" "$_d/web/node_modules/m" "$_d/deploy/dev/redact/.cache" || return 1
	for _f in internal/a/.env internal/a/.env.local web/.npmrc web/src/x.pem web/src/y.key deploy/dev/redact/.netrc internal/a/.git/config web/build/b web/test-results/t web/node_modules/m/i deploy/dev/redact/.cache/c; do
		printf '%s\n' "$SENTINEL" >"$_d/$_f" || return 1
	done
	printf 'ok\n' >"$_d/internal/a/ok.go" && printf 'ok\n' >"$_d/web/src/ok.ts" && printf 'x\n' >"$_d/.dockerignore" || return 1
	snap "$_d" "$S/synth.tar" && mkdir "$S/synthx" && tar -x -f "$S/synth.tar" -C "$S/synthx" || return 1
	tar -t -f "$S/synth.tar" | sed 's|/$||' >"$S/synth.list" || return 1
	(cd "$S/synthx" && find . -mindepth 1 | sed 's|^\./||') >"$S/synthx.list" || return 1
	grep -Fxq internal/a/ok.go "$S/synth.list" && grep -Fxq web/src/ok.ts "$S/synthx.list" &&
		! grep -Eq "$SECRET_RE" "$S/synth.list" "$S/synthx.list" &&
		! grep -aFq "$SENTINEL" "$S/synth.tar" || return 1
	tar -t -f "$S/ctx.tar" | sed 's|/$||' >"$S/ctx.list" && [ -s "$S/ctx.list" ] && ! grep -Eq "$SECRET_RE" "$S/ctx.list"
}

# obs_setup: a passive, test-only wrapper around the real Docker (same arguments, streams and
# status). After a real `docker rm` returns it scans only this test's own TMPDIR, and appends
# the fixed token `observed` when it sees a known runner scratch file (so the scratch scan really
# reaches the runner's scratch) and `leak` when any synthetic secret is in a file or a name there.
# When the run selects BOXSET_SECRET_TEST, a nonempty retained `cperr` (the redacted copy
# diagnostic, never the whole scratch: ordinary manifests legitimately hold common words) must
# hold the redaction marker and not the selected value: `diag` when it does, `diagleak` when not.
# When the run selects DR_OBS_STATE, it first reads each removed container back from the real
# Docker and appends `state NAME STATUS STARTEDAT EXIT RUNID vs=N ks=N bytes=N`: the actual state
# at the removal, and for a validator the retained attachment status, host sink status and size of
# its canonical output (`-` where absent).
# ponytail: this is a retained-at-removal snapshot; use syscall tracing only if a proof of
# transient writes is ever needed.
obs_setup() {
	_od=$(command -v docker) && mkdir "$S/obs" "$S/tmp5" || return 1
	printf '%s\n' "$SENTINEL" synthetic-url-pass synthetic-query-secret >"$S/obs.needles" || return 1
	cat >"$S/obs/docker" <<'EOF' || return 1
#!/bin/sh
if [ "${1:-}" = rm ] && [ -n "${DR_OBS_STATE:-}" ]; then
	_t=$(find "$DR_OBS_TMP" -name vs -type f 2>/dev/null | sed 1q)
	for _a in "$@"; do
		case $_a in rm | -*) continue ;; esac
		_i=$("$DR_OBS_DOCKER" inspect --format '{{.Name}} {{.State.Status}} {{.State.StartedAt}} {{.State.ExitCode}} {{index .Config.Labels "boxset.dev-run"}}' "$_a" 2>/dev/null) || continue
		_v=-
		_k=-
		_b=-
		_n=${_i%% *}
		case $_n in
		*-v-*)
			_d=${_t%/vs}
			[ -f "$_d/vs" ] && _v=$(cat "$_d/vs")
			[ -f "$_d/ks" ] && _k=$(cat "$_d/ks")
			[ -f "$_d/cap/${_n##*-v-}.png" ] && _b=$(wc -c <"$_d/cap/${_n##*-v-}.png" | tr -d ' ')
			;;
		esac
		printf 'state %s vs=%s ks=%s bytes=%s\n' "$_i" "$_v" "$_k" "$_b" >>"$DR_OBS_OUT"
	done
fi
"$DR_OBS_DOCKER" "$@"
_s=$?
if [ "${1:-}" = rm ]; then
	grep -rlaF -e GOTELEMETRY=off "$DR_OBS_TMP" >/dev/null 2>&1 && echo observed >>"$DR_OBS_OUT"
	_l=0
	grep -rlaF -f "$DR_OBS_NEEDLES" "$DR_OBS_TMP" >/dev/null 2>&1 && _l=1
	while IFS= read -r _n; do
		[ -z "$(find "$DR_OBS_TMP" -name "*$_n*" -print 2>/dev/null)" ] || _l=1
	done <"$DR_OBS_NEEDLES"
	[ "$_l" = 0 ] || echo leak >>"$DR_OBS_OUT"
	if [ -n "${BOXSET_SECRET_TEST:-}" ]; then
		find "$DR_OBS_TMP" -name cperr -type f -size +0c -print 2>/dev/null | while IFS= read -r _f; do
			if grep -qaF '[REDACTED]' "$_f" && ! grep -qaF -e "$BOXSET_SECRET_TEST" "$_f"; then echo diag; else echo diagleak; fi
		done >>"$DR_OBS_OUT"
	fi
fi
exit "$_s"
EOF
	chmod 0755 "$S/obs/docker" &&
		OBS="PATH=$S/obs:$PATH TMPDIR=$S/tmp5 DR_OBS_DOCKER=$_od DR_OBS_TMP=$S/tmp5 DR_OBS_OUT=$S/obs.tok DR_OBS_NEEDLES=$S/obs.needles"
}
# obs_rr ARGS...: rr with the observer in front of Docker and the runner's scratch under it.
obs_rr() {
	: >"$S/obs.tok"
	_k=$RRENV
	RRENV="$RRENV $OBS"
	rr "$@"
	RRENV=$_k
}
# obs_ok: the observer saw the runner's scratch at the removal and found no synthetic secret
# (and no unredacted selected value in the retained copy diagnostic).
obs_ok() { grep -qx observed "$S/obs.tok" && ! grep -qx leak "$S/obs.tok" && ! grep -qx diagleak "$S/obs.tok"; }
# obs_diag: the retained copy diagnostic was nonempty and redacted (a missing draft was reported).
obs_diag() { grep -qx diag "$S/obs.tok"; }

# r5_bad WORD SCRIPT [ARG]: a child leaving an invalid draft gives status 125 and WORD, keeps the
# prior draft byte for byte, and leaves no part file, no container and no secret in the scratch.
r5_bad() {
	obs_rr --write docs/spikes/issue-drafts -- sh -c "$2" x "${3:-}"
	[ "$RC" = 125 ] && grep -qx "dev-run: $1" "$S/out" && ctr_gone &&
		cmp -s "$S/prior" "$DRAFTS/s5-admission.md" &&
		[ -z "$(find "$DRAFTS" -name '*.part*' -print)" ] && obs_ok
}

# r5_identity: the corpus export identity (T27). With the sentinel selected, a zero-exit child that
# writes a sentinel-free series.jsonl/movie.jsonl pair under --write testdata/parser publishes both
# byte-identical to the child's bytes; a zero-exit child whose movie.jsonl holds the selected value
# is export_changed (the raw bytes of the stopped container do not hash as the sanitized staged
# bytes): status 125, both pre-seeded scratch corpora unchanged and no part file left.
r5_identity() {
	_cd=$S/repo/testdata/parser
	rr --write testdata/parser -- sh -c 'printf "series-rows\n" >testdata/parser/series.jsonl && printf "movie-rows\n" >testdata/parser/movie.jsonl'
	printf 'series-rows\n' >"$S/want.series" && printf 'movie-rows\n' >"$S/want.movie" || return 1
	[ "$RC" = 0 ] && cmp -s "$S/want.series" "$_cd/series.jsonl" && cmp -s "$S/want.movie" "$_cd/movie.jsonl" &&
		[ -z "$(find "$_cd" -name '*.part*' -print)" ] && ! grep -Fq "$SENTINEL" "$S/out" && ctr_gone || return 1
	sha256sum "$_cd/series.jsonl" "$_cd/movie.jsonl" >"$S/corpora.sum" || return 1
	rr --write testdata/parser -- sh -c 'printf "series-rows-2\n" >testdata/parser/series.jsonl && printf "movie %s\n" "$1" >testdata/parser/movie.jsonl' x "$SENTINEL"
	[ "$RC" = 125 ] && grep -qx 'dev-run: export_changed' "$S/out" && ! grep -Fq "$SENTINEL" "$S/out" &&
		sha256sum -c "$S/corpora.sum" >/dev/null 2>&1 &&
		[ -z "$(find "$_cd" -name '*.part*' -print)" ]
}

# R5: combined sentinel check. The child writes the value and a secret URL into the issue
# draft and lists its environment names, then exits 1: status kept, draft published
# redacted without either, no part file, owned container gone, runner output clean, and the
# observer saw no raw value in the runner's scratch. Then an oversized draft, a draft whose
# raw size fits but whose redaction expands past the cap, a symlink and a directory holding a
# secret-named entry in the draft's place each fail the export with the prior draft intact
# and (the observer again) no raw name in the scratch, and the snapshot drops nested secrets.
r5() {
	snap_ok && obs_setup || return 1
	obs_rr --write docs/spikes/issue-drafts -- sh -c 'mkdir -p docs/spikes/issue-drafts && printf "draft %s %s\n" "$1" "$2" >docs/spikes/issue-drafts/s5-admission.md; printf "envnames"; env | while IFS= read -r l; do printf " %s" "${l%%=*}"; done; printf "\n"; exit 1' x "$SENTINEL" "$URL"
	[ "$RC" = 1 ] || return 1
	grep -Fq 'draft [REDACTED] https://' "$DRAFTS/s5-admission.md" &&
		grep -Fq '@example.invalid/p?token=' "$DRAFTS/s5-admission.md" &&
		! grep -Fq -e "$SENTINEL" -e synthetic-url-pass -e synthetic-query-secret "$DRAFTS/s5-admission.md" &&
		! grep -Fq -e "$SENTINEL" -e synthetic-url-pass -e synthetic-query-secret "$S/out" &&
		names_ok &&
		[ -z "$(find "$DRAFTS" -name '*.part*' -print)" ] &&
		ctr_gone && obs_ok || return 1
	cp "$DRAFTS/s5-admission.md" "$S/prior" || return 1
	r5_bad draft_overflow 'mkdir -p docs/spikes/issue-drafts && head -c 70000 /dev/zero | tr "\0" x >docs/spikes/issue-drafts/s5-admission.md' &&
		r5_bad export_invalid 'mkdir -p docs/spikes/issue-drafts && ln -s /etc/passwd docs/spikes/issue-drafts/s5-admission.md' &&
		r5_bad export_invalid 'mkdir -p "docs/spikes/issue-drafts/s5-admission.md/$1" && : >"docs/spikes/issue-drafts/s5-admission.md/$1/f"' "$SENTINEL" || return 1
	RRENV=BOXSET_SECRET_SHORT=zq
	r5_bad draft_overflow 'mkdir -p docs/spikes/issue-drafts && yes zq | head -n 20000 >docs/spikes/issue-drafts/s5-admission.md'
	_e5=$?
	RRENV=
	[ "$_e5" = 0 ] || return 1
	# A selected value that also occurs in docker's missing-file diagnostic ("file") must not hide
	# the missing sibling: a failing child with one draft still publishes it (status 1), and a
	# silent child with no draft returns 0 and leaves the prior draft untouched.
	RRENV=BOXSET_SECRET_TEST=file
	obs_rr --write docs/spikes/issue-drafts -- sh -c 'mkdir -p docs/spikes/issue-drafts && printf "draft file %s\n" "$1" >docs/spikes/issue-drafts/s5-admission.md; exit 1' x "$SENTINEL"
	_e5=$RC
	[ "$_e5" = 1 ] &&
		grep -Fxq 'draft [REDACTED] [REDACTED]' "$DRAFTS/s5-admission.md" &&
		[ ! -e "$DRAFTS/s5-tools.md" ] &&
		! grep -Fq "$SENTINEL" "$DRAFTS/s5-admission.md" "$S/out" &&
		[ -z "$(find "$DRAFTS" -name '*.part*' -print)" ] &&
		ctr_gone && obs_ok && obs_diag &&
		cp "$DRAFTS/s5-admission.md" "$S/prior" || { RRENV=; return 1; }
	obs_rr --write docs/spikes/issue-drafts -- true
	_e5=$RC
	[ "$_e5" = 0 ] && cmp -s "$S/prior" "$DRAFTS/s5-admission.md" && [ ! -e "$DRAFTS/s5-tools.md" ] &&
		[ -z "$(find "$DRAFTS" -name '*.part*' -print)" ] && ctr_gone && obs_ok || { RRENV=; return 1; }
	# Missing first draft, present second: only this run's scratch copy of the first host draft is
	# removed; the second name is still exported, redacted and published, with the child's status.
	rm -f -- "$DRAFTS/s5-admission.md" || { RRENV=; return 1; }
	obs_rr --write docs/spikes/issue-drafts -- sh -c 'mkdir -p docs/spikes/issue-drafts && printf "tools file %s\n" "$1" >docs/spikes/issue-drafts/s5-tools.md; exit 1' x "$SENTINEL"
	_e5=$RC
	RRENV=
	printf 'tools [REDACTED] [REDACTED]\n' >"$S/want.tools" || return 1
	[ "$_e5" = 1 ] && cmp -s "$S/want.tools" "$DRAFTS/s5-tools.md" && [ ! -e "$DRAFTS/s5-admission.md" ] &&
		! grep -Fq "$SENTINEL" "$DRAFTS/s5-tools.md" "$S/out" &&
		[ -z "$(find "$DRAFTS" -name '*.part*' -print)" ] && ctr_gone && obs_ok || return 1
	r5_identity
}

# R6: the existing redaction-only Go tests and the bounded TestRefArchive producer tests pass through
# the runner (cgo off, as the repo's ordinary Go tests run); the selector names only main_test.go
# and ref_archive_test.go tests, at least one of each, and no controller or clock test.
RX='^(TestSelected|TestSelection|TestUsage|TestOverlaps|TestSecretSplit|TestURL|TestUndelimited|TestNUL|TestRaw|TestWriteError|TestTail|TestBinary|TestReadError|TestOneSelection|TestRefArchive)'
r6() {
	rr -- env CGO_ENABLED=0 go test ./deploy/dev/redact -run "$RX" -count=1 -v
	[ "$RC" = 0 ] && grep -q '^--- PASS' "$S/out" && ! grep -Eq -- '--- (FAIL|SKIP)' "$S/out" || return 1
	grep '^--- PASS: TestRefArchive' "$S/out" >/dev/null && grep '^--- PASS: ' "$S/out" | grep -v 'TestRefArchive' >/dev/null || return 1
	rr -- env CGO_ENABLED=0 go test ./deploy/dev/redact -list "$RX"
	[ "$RC" = 0 ] || return 1
	grep -o '^func Test[A-Za-z0-9_]*' "$S/repo/deploy/dev/redact/main_test.go" | sed 's/^func //' >"$S/names.main"
	grep -o '^func Test[A-Za-z0-9_]*' "$S/repo/deploy/dev/redact/ref_archive_test.go" | sed 's/^func //' >"$S/names.ref"
	cat "$S/names.main" "$S/names.ref" >"$S/names"
	grep '^Test' "$S/out" >"$S/listed"
	[ -s "$S/listed" ] && [ -z "$(grep -Fvx -f "$S/names" "$S/listed")" ] &&
		grep -Fxq -f "$S/names.main" "$S/listed" && grep -Fxq -f "$S/names.ref" "$S/listed"
}

# R7: the root alias /src/. gets the real admission controls (an unexpected child variable
# is blocked beside them, never a PASS), a valid small noncanonical manifest copy prints only
# the distinct manifest-only line (no admission PASS, no runtime control), then the real
# admission through the runner blocks an impossible executed Bun date and a stale annual evidence
# date (the latter retaining the draft).
# r7_go_real FIELD OLD NEW WORD: one real-root admission run (the scratch /src) whose ADM go row has
# one cell mutated: status 1, the named reason for that row, never a PASS. The valid control is the
# real-root run above, whose only block is its planted variable. The report is always restored.
r7_go_real() {
	_f=$S/repo/docs/spikes/s5-version-admission.md
	_row=$(grep -m1 '^| ADM-[0-9]* | go | go-dependency |' "$S/report.orig") || return 1
	_gid=${_row#'| '}
	_gid=${_gid%% *}
	sed -E "/^\\| $_gid /s#$2#$3#" "$S/report.orig" >"$S/report.new" && ! cmp -s "$S/report.orig" "$S/report.new" || return 1
	cat "$S/report.new" >"$_f" || return 1
	rr -- scripts/s5-toolchain.sh --admission
	_e=$RC
	cat "$S/report.orig" >"$_f" || return 1
	[ "$_e" = 1 ] && grep -q "^ADMISSION-BLOCKED $_gid: $4" "$S/out" && ! grep -q '^S5-ADMISSION-PASS' "$S/out"
}
# r7_go: the go row's licence SHA-256, origin commit and h1: pin each mutated in their own real-root
# run, then a manifest-only copy with one extra go.mod directive and one require without a row.
r7_go() {
	cp "$S/repo/docs/spikes/s5-version-admission.md" "$S/report.orig" || return 1
	r7_go_real licence 'license-sha256=[0-9a-f]{64}' 'license-sha256=0000000000000000000000000000000000000000000000000000000000000000' 'licence_mismatch' &&
		r7_go_real source ' origin=(https:[^ ]*) commit=[0-9a-f]{40} ref=' ' origin=\1 commit=1111111111111111111111111111111111111111 ref=' 'source_mismatch' &&
		r7_go_real pin ' h1:[A-Za-z0-9+/]{43}= \|' ' h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA= |' "go.sum lacks '" || return 1
	rr -- sh -c 'd=$(mktemp -d) && mkdir -p "$d/deploy/dev" "$d/docs/spikes" "$d/web" && cp /src/deploy/dev/Dockerfile "$d/deploy/dev/" && cp /src/docs/spikes/s5-version-admission.md "$d/docs/spikes/" && cp /src/web/package.json /src/web/bun.lock "$d/web/" && cp /src/go.mod "$d/" && printf "\nreplace example.invalid/x => example.invalid/y v1.0.0\nrequire example.invalid/norow v1.0.0\n" >>"$d/go.mod" && exec scripts/s5-toolchain.sh --admission --root "$d"'
	[ "$RC" = 1 ] &&
		grep -qx 'ADMISSION-UNLISTED go.mod directive replace' "$S/out" &&
		grep -qx 'ADMISSION-UNLISTED go example.invalid/norow@v1.0.0' "$S/out" &&
		grep -qx 'S5-ADMISSION-FAIL blocked=0 unlisted=2' "$S/out" &&
		! grep -q -e '^S5-ADMISSION-PASS' -e '^S5-MANIFEST-ONLY-PASS' "$S/out"
}

r7() {
	rr -- env DEV_RUN_SMOKE_CHILD_MARKER=1 scripts/s5-toolchain.sh --admission --root /src/.
	[ "$RC" = 1 ] &&
		grep -qx 'ADMISSION-BLOCKED env: unexpected variable DEV_RUN_SMOKE_CHILD_MARKER' "$S/out" &&
		grep -qx 'S5-ADMISSION-FAIL blocked=1 unlisted=0' "$S/out" &&
		grep -qx 'CONTROL go cgo build with gcc ok' "$S/out" &&
		! grep -q '^S5-ADMISSION-PASS' "$S/out" || return 1
	rr -- sh -c 'd=$(mktemp -d) && mkdir -p "$d/deploy/dev" "$d/docs/spikes" "$d/web" && cp /src/deploy/dev/Dockerfile "$d/deploy/dev/" && cp /src/docs/spikes/s5-version-admission.md "$d/docs/spikes/" && cp /src/web/package.json /src/web/bun.lock "$d/web/" && cp /src/go.mod "$d/" && exec scripts/s5-toolchain.sh --admission --root "$d"'
	[ "$RC" = 0 ] &&
		grep -q '^S5-MANIFEST-ONLY-PASS blocked=0 unlisted=0 rows=[0-9]' "$S/out" &&
		! grep -q -e '^S5-ADMISSION-PASS' -e '^CONTROL ' "$S/out" || return 1
	_f=$S/repo/docs/spikes/s5-version-admission.md
	_row=$(grep -m1 '^| ADM-[0-9]* | npm | npm-dependency |' "$_f") || return 1
	_row=${_row#'| '}
	_id=${_row%% *}
	_old=$(date -u -d '366 days ago' +%F) || return 1
	# The real executed form is admitted above (the one blocked row is the env variable); an
	# impossible executed Bun date gets its own diagnostic, then the real report is restored.
	grep -q "^| $_id .* | executed EVID-002 2026-10-16 |\$" "$_f" || return 1
	sed -E "/^\| $_id /s/ \| executed EVID-002 [0-9-]+ \|\$/ | executed EVID-002 2026-02-30 |/" "$_f" >"$S/report.new" &&
		grep -q "^| $_id .* | executed EVID-002 2026-02-30 |\$" "$S/report.new" &&
		cp "$_f" "$S/report.orig" && cat "$S/report.new" >"$_f" || return 1
	rr -- scripts/s5-toolchain.sh --admission
	_e=$RC
	cat "$S/report.orig" >"$_f" || return 1
	[ "$_e" = 1 ] &&
		grep -qx "ADMISSION-BLOCKED $_id: recorded Bun result date 2026-02-30 is not a valid date" "$S/out" &&
		grep -qx 'S5-ADMISSION-FAIL blocked=1 unlisted=0' "$S/out" &&
		! grep -q '^S5-ADMISSION-PASS' "$S/out" || return 1
	r7_go || return 1
	sed -E "/^\| $_id /s/ [0-9]{4}-[0-9]{2}-[0-9]{2} (https:)/ $_old \1/" "$_f" >"$S/report.new" &&
		grep -q "^| $_id .* $_old https:" "$S/report.new" &&
		cat "$S/report.new" >"$_f" || return 1
	rr --write docs/spikes/issue-drafts -- scripts/s5-toolchain.sh --admission
	[ "$RC" = 1 ] &&
		grep -q "^ADMISSION-BLOCKED $_id: maintenance_stale" "$S/out" &&
		grep -q "^ADMISSION-BLOCKED $_id: maintenance_stale" "$DRAFTS/s5-admission.md" &&
		! grep -Fq "$SENTINEL" "$DRAFTS/s5-admission.md" &&
		[ -z "$(find "$DRAFTS" -name '*.part*' -print)" ]
}

# runid: the run id of the runner's identification line in $S/out.
runid() { sed -n 's/^dev-run: image=.* container=boxset-dev-run-\([0-9a-f]*\) log=none$/\1/p' "$S/out"; }
# owned_gone: no container labelled with the run id of the runner's identification line is left
# (the producer and every validator).
owned_gone() {
	_r=$(runid)
	[ -n "$_r" ] && [ -z "$(docker ps -aq --filter "label=boxset.dev-run=$_r")" ]
}
# capdir: a fresh capture destination under the scratch copy's own workspace.
capdir() {
	mkdir -p "$S/repo/.devrites/work/smoke-tools-failure" || return 1
	printf '.devrites/work/smoke-tools-failure/screenshots-%s' "$(date -u +%Y%m%dT%H%M%SZ)"
}
# no_cap: the capture destination was never published.
no_cap() { [ ! -e "$S/repo/$_cap" ] && [ ! -L "$S/repo/$_cap" ]; }
PROBE=$S/repo/web/src/lib/toolchain-probe/Probe.svelte
VCONF=$S/repo/web/vite.config.ts
RUNNER=$S/repo/scripts/dev-run.sh
# starts_one: the complete run's start records are exactly the one given tool.
starts_one() { [ "$(grep '^S5-TOOL-START ' "$S/out")" = "S5-TOOL-START id=$1" ]; }
# bad_probe: a probe whose compiler syntax error site carries the selected synthetic sentinel.
bad_probe() {
	printf '<script lang="ts">\n\tlet count = $state(0; // %s\n</script>\n\n<button type="button">{count}</button>\n' "$SENTINEL" >"$PROBE"
}

# tf_fail: a real tool (vite build) fails at a genuine compiler syntax error carrying the sentinel:
# overall nonzero, attribution, the redacted draft keeping tool, file and line with the marker where
# the sentinel was and the raw value absent, exactly one start record in the whole run, no capture.
tf_fail() {
	_cap=$(capdir) && bad_probe || return 1
	rr --write docs/spikes/issue-drafts --capture "$_cap" -- scripts/s5-toolchain.sh --tools
	_d=$DRAFTS/s5-tools.md
	_down=$(($(grep -c '^S5-TOOL-START ' "$S/out") - 1))
	[ "$RC" = 1 ] &&
		starts_one vite-build &&
		grep -qx 'S5-TOOL-FAIL id=vite-build exit=1 reason=vite build exited nonzero' "$S/out" &&
		grep -qx 'S5-TOOLS-FAIL tool=vite-build exit=1 bun_only=true' "$S/out" &&
		grep -qx 'S5-TOOLS-BLOCKED dependents=svelte-check eslint prettier vitest-browser vite-dev playwright-axe sv' "$S/out" &&
		grep -qx 'S5-DRAFT-WRITTEN name=s5-tools.md' "$S/out" &&
		! grep -q '^S5-TOOLS-PASS' "$S/out" &&
		[ -s "$_d" ] &&
		grep -q '^tool=vite-build exit=1 ' "$_d" &&
		grep -q 'blocked dependents: svelte-check eslint prettier' "$_d" &&
		grep -Eq 'Probe\.svelte:[0-9]+' "$_d" &&
		grep -Fq '[REDACTED]' "$_d" &&
		! grep -Fq "$SENTINEL" "$_d" "$S/out" &&
		[ -z "$(find "$DRAFTS" -name '*.part*' -print)" ] &&
		[ ! -e "$DRAFTS/s5-admission.md" ] &&
		no_cap && ctr_gone
}

# tf_write: the draft destination is genuinely unwritable (a read-only directory the child itself
# made): the exact write-failure marker, nonzero, no later tool, no false "written", no capture.
tf_write() {
	_cap=$(capdir) && bad_probe && rm -f "$DRAFTS/s5-tools.md" || return 1
	rr --write docs/spikes/issue-drafts --capture "$_cap" -- sh -c 'mkdir -p docs/spikes/issue-drafts && chmod a-w docs/spikes/issue-drafts && exec scripts/s5-toolchain.sh --tools'
	[ "$RC" = 1 ] &&
		starts_one vite-build &&
		grep -qx 'S5-DRAFT-WRITE-FAILED name=s5-tools.md' "$S/out" &&
		! grep -q '^S5-DRAFT-WRITTEN' "$S/out" &&
		[ ! -e "$DRAFTS/s5-tools.md" ] &&
		no_cap && owned_gone
}

# tf_read: a nonempty diagnostic log is genuinely unreadable: the scratch vite config removes every
# permission from the tool's own output logs, so the draft's real tail gets EACCES as the file's
# owner. The draft already holds its nonempty header, but its construction failed: the exact
# write-failure marker, nonzero, nothing later started, no false "written", no capture.
tf_read() {
	_cap=$(capdir) && bad_probe && rm -f "$DRAFTS/s5-tools.md" || return 1
	sed -e 's/^export default defineConfig({/import { fchmodSync } from "node:fs";\nfchmodSync(1, 0);\nfchmodSync(2, 0);\nexport default defineConfig({/' "$S/vite.orig" >"$VCONF" && ! cmp -s "$S/vite.orig" "$VCONF" || return 1
	rr --write docs/spikes/issue-drafts --capture "$_cap" -- scripts/s5-toolchain.sh --tools
	cp "$S/vite.orig" "$VCONF" || return 1
	[ "$RC" = 1 ] &&
		starts_one vite-build &&
		grep -qx 'S5-TOOL-FAIL id=vite-build exit=1 reason=vite build exited nonzero' "$S/out" &&
		grep -Eq '^tail: .*vite-build\.(out|err).*Permission denied' "$S/out" &&
		grep -qx 'S5-DRAFT-WRITE-FAILED name=s5-tools.md' "$S/out" &&
		! grep -q '^S5-DRAFT-WRITTEN' "$S/out" &&
		[ -s "$DRAFTS/s5-tools.md" ] &&
		grep -q '^tool=vite-build exit=1 ' "$DRAFTS/s5-tools.md" &&
		grep -Eq '^## vite-build\.(out|err) \(last 40 lines\)$' "$DRAFTS/s5-tools.md" &&
		no_cap && owned_gone &&
		echo "# tf_read: tail EACCES on the owned log, partial draft $(wc -c <"$DRAFTS/s5-tools.md" | tr -d ' ') bytes, S5-DRAFT-WRITE-FAILED"
}

# tf_stall: a real preview server stays alive and never completes its response body: the readiness
# failure is named, the draft is retained and the run ends with status 1 (not the runner's 124),
# with no later tool. No elapsed threshold: the run includes the image builds.
tf_stall() {
	_cap=$(capdir) && cp "$S/Probe.orig" "$PROBE" && rm -f "$DRAFTS/s5-tools.md" || return 1
	sed -e 's/^export default defineConfig({/const stall = { name: "stall", configurePreviewServer(server) { server.middlewares.use((_, res) => { res.writeHead(200, { "content-type": "text\/html" }); res.write("<!doctype html>"); }); } };\nexport default defineConfig({/' \
		-e 's/^\tplugins: \[/\tplugins: [stall,/' "$S/vite.orig" >"$VCONF" && ! cmp -s "$S/vite.orig" "$VCONF" || return 1
	rr --write docs/spikes/issue-drafts --capture "$_cap" -- scripts/s5-toolchain.sh --tools
	cp "$S/vite.orig" "$VCONF" || return 1
	[ "$RC" = 1 ] &&
		starts_one vite-build &&
		grep -qx 'S5-TOOL-FAIL id=vite-build exit=3 reason=vite preview did not serve the build on loopback' "$S/out" &&
		grep -qx 'S5-DRAFT-WRITTEN name=s5-tools.md' "$S/out" &&
		grep -q '^tool=vite-build exit=3 ' "$DRAFTS/s5-tools.md" &&
		no_cap && ctr_gone
}

# tf_success: the real eight-tool run succeeds with a pre-seeded host draft: it writes no new draft,
# leaves the pre-seeded bytes unchanged and publishes the owned scratch captures.
tf_success() {
	_cap=$(capdir) && printf 'preseeded host draft\n' >"$S/seed" && cp "$S/seed" "$DRAFTS/s5-tools.md" || return 1
	rr --write docs/spikes/issue-drafts --capture "$_cap" -- scripts/s5-toolchain.sh --tools
	[ "$RC" = 0 ] &&
		grep -qx 'S5-TOOLS-PASS tools=8 failed=0 skipped=0 bun_only=true' "$S/out" &&
		! grep -q -e '^S5-DRAFT-WRITTEN' -e '^S5-DRAFT-WRITE-FAILED' -e '^S5-TOOL-FAIL' "$S/out" &&
		cmp -s "$S/seed" "$DRAFTS/s5-tools.md" &&
		[ ! -e "$DRAFTS/s5-admission.md" ] &&
		grep -q "^DEV-RUN-CAPTURE-PASS files=4 .* dir=$_cap " "$S/out" &&
		[ "$(find "$S/repo/$_cap" -mindepth 1 -type f | wc -l | tr -d ' ')" = 5 ] &&
		owned_gone
}

# cap_rej WORD ARGS...: a zero-exit child leaving real capture input is rejected with WORD: runner
# status 125, no published capture, no owned container left, and the observer saw no raw value in
# the runner's scratch (the raw producer bytes are never staged on the host).
cap_rej() {
	_w=$1
	shift
	_cap=$(capdir) || return 1
	obs_rr --capture "$_cap" -- "$@"
	[ "$RC" = 125 ] && grep -qx "dev-run: $_w" "$S/out" && ! grep -q '^DEV-RUN-CAPTURE-PASS' "$S/out" &&
		! grep -Fq "$SENTINEL" "$S/out" && no_cap && owned_gone && obs_ok
}
# tf_cap: the runner's capture rejection table, each case a real child input.
tf_cap() {
	mkdir -p "$S/repo/testdata" || return 1
	obs_setup && printf 'package main\n\nimport (\n\t"image"\n\t"image/png"\n\t"os"\n\t"strconv"\n)\n\nfunc main() {\n\tw, _ := strconv.Atoi(os.Args[1])\n\th, _ := strconv.Atoi(os.Args[2])\n\tch, _ := strconv.ParseUint(os.Args[3], 16, 64)\n\tif os.Args[4] == "flip" {\n\t\tch = ^ch\n\t}\n\timg := image.NewNRGBA(image.Rect(0, 0, w, h))\n\tfor i := range img.Pix {\n\t\timg.Pix[i] = 255\n\t}\n\tfor k := 0; k < 64; k++ {\n\t\tif ch>>(63-k)&1 == 0 {\n\t\t\tcontinue\n\t\t}\n\t\tfor y := h - 16 + 8*(k/33); y < h-8+8*(k/33); y++ {\n\t\t\tfor x := 8 * (k %% 33); x < 8*(k%%33)+8; x++ {\n\t\t\t\tif x >= w || y >= h {\n\t\t\t\t\tcontinue\n\t\t\t\t}\n\t\t\t\ti := img.PixOffset(x, y)\n\t\t\t\timg.Pix[i], img.Pix[i+1], img.Pix[i+2] = 0, 0, 0\n\t\t\t}\n\t\t}\n\t}\n\tif png.Encode(os.Stdout, img) != nil {\n\t\tos.Exit(1)\n\t}\n}\n' >"$S/repo/testdata/pnggen.go" || return 1
	cap_rej capture_missing true &&
		cap_rej capture_missing sh -c ': >/capture/dev-desktop.png' &&
		cap_rej capture_copy_failed sh -c 'ln -s /etc/passwd /capture/dev-desktop.png' &&
		cap_rej capture_oversize sh -c 'head -c 3145728 /dev/zero >/capture/dev-desktop.png' &&
		cap_rej capture_not_png sh -c 'printf "%s\n" "$1" >/capture/dev-desktop.png' x "$SENTINEL" &&
		cap_rej capture_dimensions sh -c 'go run testdata/pnggen.go 100 100 "$BOXSET_CAPTURE_CHALLENGE" keep >/capture/dev-desktop.png' &&
		cap_rej capture_challenge_mismatch sh -c 'go run testdata/pnggen.go 1440 900 "$BOXSET_CAPTURE_CHALLENGE" flip >/capture/dev-desktop.png' || return 1
	# A selected value that also occurs in Docker's missing-file sentence must not hide the missing
	# capture, and the retained copy diagnostic is redacted, never raw.
	RRENV=BOXSET_SECRET_TEST=file
	cap_rej capture_missing true
	_e=$?
	RRENV=
	[ "$_e" = 0 ] && obs_diag
}

# tf_env: missing or malformed capture variables (the challenge and the directory, each missing or
# malformed beside a valid runner-provided sibling) stop --tools with the fixed word and publish nothing.
tf_env() {
	for _v in 'env -u BOXSET_CAPTURE_CHALLENGE' 'env -u BOXSET_CAPTURE_DIR' 'env BOXSET_CAPTURE_CHALLENGE=zzzzzzzzzzzzzzzz' 'env BOXSET_CAPTURE_DIR=/wrong'; do
		_cap=$(capdir) || return 1
		rr --capture "$_cap" -- $_v scripts/s5-toolchain.sh --tools
		[ "$RC" = 2 ] && grep -qx 'S5-TOOLS-CAPTURE-UNSET' "$S/out" && ! grep -q '^S5-TOOL-START' "$S/out" && no_cap && owned_gone || return 1
	done
}

# tf_log: a validator created by the real Docker with a non-none log driver (a one-token change in
# the scratch runner copy) is rejected before it starts: fixed word, no validator observation, no
# capture; and the passive observer, reading the real Docker back before the owned validator is
# removed, sees it created and never started (zero StartedAt).
tf_log() {
	_cap=$(capdir) || return 1
	[ -d "$S/obs" ] || obs_setup || return 1
	sed '/--memory 64m/s/--log-driver=none/--log-driver=local/' "$S/runner.orig" >"$RUNNER" &&
		[ "$(grep -c -- '--log-driver=local' "$RUNNER")" = 1 ] || return 1
	RRENV=DR_OBS_STATE=1
	obs_rr --capture "$_cap" -- true
	_e=$RC
	RRENV=
	cp "$S/runner.orig" "$RUNNER" || return 1
	_r=$(runid)
	[ "$_e" = 125 ] && grep -qx 'dev-run: log_driver_invalid' "$S/out" && ! grep -q 'dev-run: validator=' "$S/out" &&
		! grep -q '^DEV-RUN-CAPTURE-PASS' "$S/out" && no_cap && owned_gone &&
		grep -qx "state /boxset-dev-run-$_r-v-dev-desktop created 0001-01-01T00:00:00Z 0 $_r vs=- ks=- bytes=-" "$S/obs.tok" &&
		echo "# tf_log: owned validator observed created, StartedAt=0001-01-01T00:00:00Z, before removal"
}

# tf_sink: after a real validator exit 0 the host sink fails partway while still draining its input
# (a real size-limited write in the scratch runner copy, the file-size signal ignored). The same
# child through the unchanged runner is first accepted by the validator (it stops only at the next,
# missing, file). The passive observer then reads the faulted validator back before its removal:
# exited 0, attachment status 0, a nonzero sink status and a nonempty output shorter than the
# accepted one; only then the fixed word, no publication and no pass line.
tf_sink() {
	_cap=$(capdir) || return 1
	[ -d "$S/obs" ] || obs_setup || return 1
	RRENV=DR_OBS_STATE=1
	obs_rr --capture "$_cap" -- sh -c 'go run testdata/pnggen.go 1440 900 "$BOXSET_CAPTURE_CHALLENGE" keep >/capture/dev-desktop.png'
	RRENV=
	grep -qx 'dev-run: capture_missing' "$S/out" && grep -q 'dev-run: validator=dev-mobile ' "$S/out" || return 1
	_r=$(runid)
	_vb=$(sed -n "s#^state /boxset-dev-run-$_r-v-dev-desktop exited [^ ]* 0 $_r vs=0 ks=0 bytes=\([1-9][0-9]*\)\$#\1#p" "$S/obs.tok")
	[ -n "$_vb" ] || return 1
	sed 's#{ head -c "\$DR_SINK" >"\$DR_OUT";#{ (ulimit -f 1; trap "" XFSZ; head -c "$DR_SINK" || { _k=$?; cat >/dev/null; exit "$_k"; }) >"$DR_OUT";#' "$S/runner.orig" >"$RUNNER" &&
		[ "$(grep -c 'ulimit -f 1; trap "" XFSZ' "$RUNNER")" = 1 ] || return 1
	RRENV=DR_OBS_STATE=1
	obs_rr --capture "$_cap" -- sh -c 'go run testdata/pnggen.go 1440 900 "$BOXSET_CAPTURE_CHALLENGE" keep >/capture/dev-desktop.png'
	_e=$RC
	RRENV=
	cp "$S/runner.orig" "$RUNNER" || return 1
	_r=$(runid)
	set -- $(sed -n "s#^state /boxset-dev-run-$_r-v-dev-desktop exited [^ ]* 0 $_r vs=0 ks=\([1-9][0-9]*\) bytes=\([1-9][0-9]*\)\$#\1 \2#p" "$S/obs.tok")
	[ $# = 2 ] && [ "$2" -lt "$_vb" ] &&
		[ "$_e" = 125 ] && grep -qx 'dev-run: capture_copy_failed' "$S/out" && ! grep -q 'dev-run: validator=dev-mobile ' "$S/out" &&
		! grep -q '^DEV-RUN-CAPTURE-PASS' "$S/out" && no_cap && owned_gone &&
		echo "# tf_sink: faulted validator exit 0, attachment 0, sink status $1, output $2 of $_vb bytes, then capture_copy_failed"
}

# tf_big: the real vite build fails with a readable stderr log whose last 40 lines are far over the
# draft's 6000-byte cap and over a 64 KiB pipe buffer (the scratch vite config prints 64 numbered
# 4000-byte lines before the real syntax error). The draft's own first tail line proves how many
# whole numbered lines the tail held, so a tail too small to force the drain fails here. The tool's
# section is then exactly the 6000-byte cap, the draft is written (never the write-failure marker:
# the tail's status stays zero), the tool and exit are attributed, nothing later starts and nothing
# is captured.
tf_big() {
	_cap=$(capdir) && bad_probe && rm -f "$DRAFTS/s5-tools.md" || return 1
	sed -e 's/^export default defineConfig({/for (let i = 0; i < 64; i++) console.error("OVERCAP-" + i + "-" + "x".repeat(4000));\nexport default defineConfig({/' "$S/vite.orig" >"$VCONF" && ! cmp -s "$S/vite.orig" "$VCONF" || return 1
	rr --write docs/spikes/issue-drafts --capture "$_cap" -- scripts/s5-toolchain.sh --tools
	cp "$S/vite.orig" "$VCONF" || return 1
	_d=$DRAFTS/s5-tools.md
	[ -s "$_d" ] && sed -n '/^## vite-build\.err (last 40 lines)$/,/^Owner review:/p' "$_d" | sed '1d;$d' >"$S/sec" || return 1
	_i=$(sed -n '1s/^OVERCAP-\([0-9][0-9]*\)-x\{4000\}$/\1/p' "$S/sec")
	[ -n "$_i" ] && [ "$_i" -lt 64 ] && _tail=$(((64 - _i) * 4000)) && [ "$_tail" -gt 65536 ] || return 1
	[ "$RC" = 1 ] &&
		starts_one vite-build &&
		grep -qx 'S5-TOOL-FAIL id=vite-build exit=1 reason=vite build exited nonzero' "$S/out" &&
		grep -qx 'S5-TOOLS-FAIL tool=vite-build exit=1 bun_only=true' "$S/out" &&
		grep -qx 'S5-TOOLS-BLOCKED dependents=svelte-check eslint prettier vitest-browser vite-dev playwright-axe sv' "$S/out" &&
		grep -qx 'S5-DRAFT-WRITTEN name=s5-tools.md' "$S/out" &&
		! grep -q -e '^S5-DRAFT-WRITE-FAILED' -e '^S5-TOOLS-PASS' -e '^tail:' "$S/out" &&
		grep -q '^tool=vite-build exit=1 ' "$_d" &&
		[ "$(wc -c <"$S/sec" | tr -d ' ')" = 6002 ] &&
		[ "$(wc -c <"$_d" | tr -d ' ')" -lt 8000 ] &&
		! grep -Fq "$SENTINEL" "$_d" "$S/out" &&
		no_cap && owned_gone &&
		echo "# tf_big: tail held >= $_tail bytes in whole numbered lines from OVERCAP-$_i, section capped at 6000 bytes, draft $(wc -c <"$_d" | tr -d ' ') bytes written"
}

if [ "$MODE" = tools-failure ]; then
	cp "$PROBE" "$S/Probe.orig" && cp "$VCONF" "$S/vite.orig" && cp "$RUNNER" "$S/runner.orig" || exit 2
	for _s in tf_fail tf_write tf_read tf_stall tf_success tf_cap tf_env tf_log tf_sink tf_big; do
		if "$_s"; then
			echo "ok $_s"
		else
			echo "not ok $_s"
			head -c 3000 "$S/out" | sed 's/^/# /'
			exit 1
		fi
	done
	echo "S5-FAILURE-PROOF-PASS draft=retained downstream=$_down"
	exit 0
fi

PASS=0
for n in 1 2 3 4 5 6 7; do
	if "r$n"; then
		echo "ok R$n"
		PASS=$((PASS + 1))
	else
		echo "not ok R$n"
		head -c 3000 "$S/out" | sed 's/^/# /'
	fi
done
if [ "$PASS" = 7 ]; then
	echo 'DEV-RUN-SMOKE-PASS checks=7'
	exit 0
fi
exit 1
