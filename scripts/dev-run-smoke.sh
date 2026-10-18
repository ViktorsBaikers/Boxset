#!/bin/sh
# Small host-only runner/sentinel check (never part of the image context):
#   scripts/dev-run-smoke.sh [--tools-failure|--benchmark-evidence]
# Runs the real scripts/dev-run.sh and Docker from a unique, ownership-checked
# scratch snapshot of the admitted project inputs (explicit roots, with the final
# nested secret and cache exclusions of .dockerignore applied before archiving,
# so no owner-shaped nested file is ever copied), so repository drafts are never
# touched. Seven direct assertions print `ok R<n>` or `not ok R<n>`;
# `DEV-RUN-SMOKE-PASS checks=7` prints only when all seven pass, any failure exits 1.
# R1 also runs the `--ref` rejection (an unknown flag, `usage`) and the `--write testdata/parser` rejection
# (`write_path_invalid`), R3 asserts that the child sees no `/ref` directory and no unlisted environment name,
# R6 the redactor unit tests and R7 the Go admission negatives, all against synthetic repositories built only
# inside the scratch.
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
# `--benchmark-evidence` instead runs the benchmark evidence controls from the same scratch, with a
# synthetic one-core cpuset and a synthetic empty config directory of its own (never the owner's
# reservation) and nothing published into the repository: the real runner's `--bench` run of a child
# that tries to overwrite, unlink and replace the imported records of an earlier run and writes the
# benchmark package's synthetic control samples, then, as the next runner invocations from images
# refreshed with the published records, the final judge over the real host-written records (INVALID: one
# core, or date_rollover when a UTC date change falls between their start and end; two synthetic runs
# assert those two reasons separately), a run absent from the image, missing, hostile, oversized, after-state-swapped, identity-
# mismatched, link and overlapping records (each INVALID, exit 3) and, with independently valid
# synthetic records beside the same samples, PASS. `BENCH-EVIDENCE-PASS controls=N` prints only when all hold.
# Before those, eight controls on private sysfs trees prove the runner's backing-disk NVMe check (md slaves,
# a rotational member, ATA, a network transport, a lone disk, an empty slave list, a disk without a
# rotational flag, a disk whose device link dangles), and two short real --bench publications, each in its own
# mount namespace with a fixture bound over /sys and a df shim naming the scratch config an ext4 md0, prove the
# unmodified runner passes the result to every stage record (the accepting md true, one with a rotational member
# false); the scratch runner is then pointed at the first tree, as a scratch config directory has no disk source of its own.
set -u
LC_ALL=C
export LC_ALL

MODE=checks
case ${1:-} in
'') ;;
--tools-failure) MODE=tools-failure ;;
--benchmark-evidence) MODE=benchmark-evidence ;;
*)
	echo 'usage: scripts/dev-run-smoke.sh [--tools-failure|--benchmark-evidence]' >&2
	exit 2
	;;
esac
[ $# -le 1 ] || {
	echo 'usage: scripts/dev-run-smoke.sh [--tools-failure|--benchmark-evidence]' >&2
	exit 2
}
# No owner benchmark value is ever read here: the checks give the runner their own synthetic ones.
unset BOXSET_BENCH_CPUSET BOXSET_BENCH_CONFIG

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

# lbl: the containers carrying any dev-run label, sorted.
lbl() { docker ps -aq --filter label=boxset.dev-run | LC_ALL=C sort; }

# R1: argument errors give 125 and a fixed word before Docker is reachable (no PATH).
r1_one() {
	_w=$1
	shift
	(cd "$S/repo" && PATH=$S/empty exec /bin/sh scripts/dev-run.sh "$@") </dev/null >"$S/out" 2>&1
	[ $? = 125 ] && grep -qx "dev-run: $_w" "$S/out"
}
# r1_bench WORD CFG ARGS...: a `--bench` rejection with BOXSET_BENCH_CONFIG set to CFG (`-` leaves it
# unset), whatever the owner exported: 125 and the fixed word before Docker (no PATH).
r1_bench() {
	_w=$1
	_cf=$2
	shift 2
	(
		cd "$S/repo" || exit 2
		if [ "$_cf" = - ]; then unset BOXSET_BENCH_CONFIG; else BOXSET_BENCH_CONFIG=$_cf; export BOXSET_BENCH_CONFIG; fi
		PATH=$S/empty exec /bin/sh scripts/dev-run.sh "$@"
	) </dev/null >"$S/out" 2>&1
	[ $? = 125 ] && grep -qx "dev-run: $_w" "$S/out"
}
BRUN=docs/spikes/data/bench-20260101T000000Z
r1() {
	r1_one usage true &&
		r1_one timeout_invalid --timeout 0 -- true &&
		r1_one write_path_invalid --write ../x -- true &&
		r1_one usage --ref -- true &&
		r1_one write_path_invalid --write testdata/parser -- true &&
		r1_one capture_flag_invalid --capture &&
		r1_one capture_destination_invalid --capture d -- true &&
		r1_one capture_destination_invalid --capture .devrites/work/Bad/screenshots-20260101T000000Z -- true &&
		r1_one capture_flag_invalid --capture .devrites/work/zz/screenshots-20260101T000000Z --bench 1 -- true &&
		r1_one usage --bench 1 --bench 2 -- true &&
		r1_one write_path_invalid --write "$BRUN/samples" -- true &&
		r1_bench 'bench_invalid side=write' - --bench 1 -- true &&
		r1_bench 'bench_invalid side=cpuset' "$S/empty" --bench 3-1 --write "$BRUN/samples" -- true &&
		r1_bench 'bench_invalid side=cpuset' "$S/empty" --bench 01 --write "$BRUN/samples" -- true &&
		r1_bench 'bench_invalid side=cpuset' "$S/empty" --bench '1,,2' --write "$BRUN/samples" -- true &&
		r1_bench 'bench_invalid side=samples' "$S/empty" --bench 1 --write docs/spikes/issue-drafts -- true &&
		r1_bench 'bench_invalid side=samples' "$S/empty" --bench 1 --write "$BRUN/other" -- true &&
		r1_bench 'bench_invalid side=samples' "$S/empty" --bench 1 --write docs/spikes/data/bench-20260101T000000/samples -- true &&
		r1_bench 'bench_unavailable side=config' - --bench 1 --write "$BRUN/samples" -- true &&
		r1_bench 'bench_unavailable side=config' '' --bench 1 --write "$BRUN/samples" -- true &&
		r1_bench 'bench_invalid side=config' relative/dir --bench 1 --write "$BRUN/samples" -- true &&
		r1_bench 'bench_invalid side=config' "$S/repo/go.mod" --bench 1 --write "$BRUN/samples" -- true &&
		r1_bench 'bench_invalid side=config' "$S/empty/" --bench 1 --write "$BRUN/samples" -- true &&
		r1_bench 'bench_invalid side=config' "$S/no such dir" --bench 1 --write "$BRUN/samples" -- true &&
		mkdir -p "$S/repo/$BRUN" &&
		r1_bench 'bench_invalid side=samples' "$S/empty" --bench 1 --write "$BRUN/samples" -- true &&
		rmdir "$S/repo/$BRUN" "$S/repo/docs/spikes/data" &&
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
R3CMD='printf "ids %s %s\n" "$(id -u)" "$(id -g)"; printf "envnames"; env | while IFS= read -r l; do printf " %s" "${l%%=*}"; done; printf "\n"; printf "goflags %s\nhome %s\n" "$GOFLAGS" "$HOME"; sed 1,2d /proc/net/dev | while IFS=: read -r i _; do printf "net %s\n" "${i##* }"; done; if [ -e /ref ]; then echo refdir=present; else echo refdir=absent; fi'
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
		grep -Fxq 'refdir=absent' "$S/out" &&
		! grep -Eiq '^envnames.* (http|https|ftp|all|no)_proxy( |$)' "$S/out" &&
		! grep -Fq -e '127.0.0.1:9' -e synthetic-noproxy "$S/out" &&
		grep -Fxq 'goflags -mod=readonly' "$S/out" &&
		grep -Fxq 'home /home/boxset' "$S/out" &&
		grep -Fxq 'net lo' "$S/out" &&
		! grep -v '^net lo$' "$S/out" | grep -q '^net ' &&
		grep -q '^dev-run: image=sha256:[0-9a-f]* inputs=[0-9a-f]* .* log=none$' "$S/out" || return 1
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
}

# R6: the existing redaction-only Go tests pass through the runner (cgo off, as the repo's ordinary Go
# tests run); the selector names only main_test.go tests, at least one, and no controller or clock test.
RX='^(TestSelected|TestSelection|TestUsage|TestOverlaps|TestSecretSplit|TestURL|TestUndelimited|TestNUL|TestRaw|TestWriteError|TestTail|TestBinary|TestReadError|TestOneSelection)'
r6() {
	rr -- env CGO_ENABLED=0 go test ./deploy/dev/redact -run "$RX" -count=1 -v
	[ "$RC" = 0 ] && grep -q '^--- PASS' "$S/out" && ! grep -Eq -- '--- (FAIL|SKIP)' "$S/out" || return 1
	rr -- env CGO_ENABLED=0 go test ./deploy/dev/redact -list "$RX"
	[ "$RC" = 0 ] || return 1
	grep -o '^func Test[A-Za-z0-9_]*' "$S/repo/deploy/dev/redact/main_test.go" | sed 's/^func //' >"$S/names.main"
	grep '^Test' "$S/out" >"$S/listed"
	[ -s "$S/listed" ] && [ -z "$(grep -Fvx -f "$S/names.main" "$S/listed")" ] &&
		grep -Fxq -f "$S/names.main" "$S/listed"
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

# ---- --benchmark-evidence ---------------------------------------------------------------------
# Everything here is synthetic and stays in the scratch copy: a one-core cpuset taken from this
# process's own allowed list (never an owner reservation), an empty config directory of the smoke's
# own, run directories under the scratch's docs/spikes/data, and the benchmark package's synthetic
# control samples. The records the runner writes are real; the records of the valid control and of
# the hostile controls are written below, independently of the runner and of the Go validator.
BE_N=0
BED=docs/spikes/data
BEPRE=bench-20200101T000000Z
BE_IMG=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
BE_INP=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
BE_T0=1792232100
BE_HOSTILE='bench-20200102T000000Z bench-20200103T000000Z bench-20200104T000000Z bench-20200105T000000Z bench-20200106T000000Z bench-20200107T000000Z bench-20200108T000000Z bench-20200109T000000Z bench-20200112T000000Z bench-20200113T000000Z bench-20200114T000000Z'
# need DESC CMD...: the condition, or its description and status 1.
need() {
	_needd=$1
	shift
	"$@" || {
		echo "# not met: $_needd"
		return 1
	}
}
# none ARGS...: grep finds nothing.
none() { ! grep "$@"; }
# be_rec STAGE RUN START END OBS: one valid synthetic record (four same-NUMA cores without SMT
# siblings, no foreign workload, 4 GiB, no swap, a read-only empty ext4 NVMe config).
be_rec() {
	printf '{"schema":1,"stage":"%s","run":"%s","image":"%s","inputs":"%s","start_unix":%s,"end_unix":%s,"observations":%s,"cpus":4,"cores":4,"threads_per_core_max":1,"siblings_outside_set":0,"sibling_runnable_max":0,"numa_nodes":1,"foreign_allowed_max":0,"foreign_runnable_max":0,"load1_milli_max":20,"mem_available_kib_min":16777216,"swap_in_start":7,"swap_in_end":7,"swap_out_start":9,"swap_out_end":9,"mem_limit_bytes":4294967296,"memswap_limit_bytes":4294967296,"cpuset_matches":true,"config_fs":"ext4","config_nvme":true,"config_empty":true,"config_readonly":true}\n' \
		"$1" "$2" "$BE_IMG" "$BE_INP" "$3" "$4" "$5"
}
# be_set DIR RUN: the three valid records of RUN in DIR.
be_set() {
	mkdir -p "$1" && be_rec before "$2" "$BE_T0" "$BE_T0" 1 >"$1/before.json" &&
		be_rec during "$2" $((BE_T0 + 1)) $((BE_T0 + 9)) 9 >"$1/during.json" &&
		be_rec after "$2" $((BE_T0 + 10)) $((BE_T0 + 10)) 1 >"$1/after.json"
}
# be_expect NAME RC TEXT: the judge child's line for NAME in the merged output.
be_expect() {
	need "judged $1 rc=$2 $3" grep -Fxq "judged $1 rc=$2 $3" "$S/out" || return 1
	BE_N=$((BE_N + 1))
}
# BE_CHILD: the --bench child of the first run: it first tries to overwrite, append to, unlink,
# replace and chmod the imported records of the earlier run, to create entries beside them and to
# move or remove them and every ancestor up to /src/docs, and to write the read-only /config; it
# reports each attempt, the imported records' digests and decoys that must not be in the image,
# and creates its own look-alike of this run's before.json (the host's file is not reachable from
# here). Then it writes the benchmark package's synthetic control samples with the production writer.
BE_CHILD='old=/src/docs/spikes/data/$1
run=/src/docs/spikes/data/$2
t() { n=$1; shift; if "$@" >/dev/null 2>&1; then echo "canary $n=allowed"; else echo "canary $n=denied"; fi; }
echo own >/tmp/own
for f in before during after; do
	t overwrite-$f sh -c ": >$old/$f.json"
	t append-$f sh -c "printf x >>$old/$f.json"
	t unlink-$f rm "$old/$f.json"
	t replace-$f mv -f /tmp/own "$old/$f.json"
	t chmod-$f chmod u+w "$old/$f.json"
	printf "sum %s %s.json\n" "$(sha256sum <"$old/$f.json" | cut -c1-64)" $f
done
t overwrite-samples sh -c ": >$old/samples/samples.jsonl"
t unlink-samples rm "$old/samples/samples.jsonl"
t create-beside-records sh -c ": >$old/new.json"
t create-beside-samples sh -c ": >$old/samples/new.jsonl"
t remove-run rm -r "$old"
t move-run mv "$old" "$old.moved"
t move-data mv /src/docs/spikes/data /src/docs/spikes/data2
t remove-data rm -r /src/docs/spikes/data
t move-spikes mv /src/docs/spikes /src/docs/spikes2
t move-docs mv /src/docs /src/docs2
t write-config sh -c ": >/config/x"
printf "sum %s samples.jsonl\n" "$(sha256sum <"$old/samples/samples.jsonl" | cut -c1-64)"
[ -d /config ] && echo "canary config=present" || echo "canary config=absent"
for x in "$old/notes.txt" "$old/during.json.bak" "$old/samples/extra.jsonl" /src/docs/spikes/data/stray.json; do
	[ ! -e "$x" ] && echo "canary decoy=absent" || echo "canary decoy=present"
done
mkdir -p "$run/samples" && printf "{}" >"$run/before.json" && echo "canary fresh=created" || echo "canary fresh=denied"
exec go test ./bench/parser -run "^TestLatencyGateControlSamples\$" -count=1 -v -args -control-out "$run/samples"'
# BE_JUDGE: the final judge, built as production (CGO off), over each named run directory of the
# image this invocation was built with, one `judged NAME rc=N OUTPUT` line each, and what the image
# imported under docs/spikes/data with its mode and owner.
BE_JUDGE='CGO_ENABLED=0 go build -trimpath -o /tmp/parserbench ./bench/parser || exit 1
find docs/spikes/data -mindepth 1 -type f -printf "imported-f %m %u %p\n"
find docs/spikes/data -mindepth 1 -type d -printf "imported-d %m %u %p\n"
for d in "$@"; do
	o=$(/tmp/parserbench -judge "docs/spikes/data/$d")
	rc=$?
	printf "judged %s rc=%s %s\n" "$d" "$rc" "$o"
done'
# be_a: the real runner's --bench run (nothing from the owner: a one-core cpuset of this process, an
# empty config of the smoke's own). The image carries the earlier run's records; its child cannot
# change any of them and the host's own records of this run are published beside the samples.
be_a() {
	BE_CPU=$(sed -n 's/^Cpus_allowed_list:[[:space:]]*\([0-9][0-9]*\).*/\1/p' /proc/self/status)
	need 'a one-core cpuset' [ -n "$BE_CPU" ] || return 1
	mkdir "$S/benchcfg" || return 1
	_o=$S/repo/$BED/$BEPRE
	mkdir -p "$_o/samples" "$S/repo/$BED" || return 1
	printf 'old samples\n' >"$_o/samples/samples.jsonl" && be_set "$_o" "$BEPRE" || return 1
	printf 'decoy\n' >"$_o/notes.txt" && printf '{}\n' >"$_o/during.json.bak" && printf 'decoy\n' >"$_o/samples/extra.jsonl" &&
		printf '{}\n' >"$S/repo/$BED/stray.json" || return 1
	RUNC=bench-$(date -u +%Y%m%dT%H%M%SZ)
	obs_setup && printf '%s\n' "$S/benchcfg" >>"$S/obs.needles" || return 1
	RRENV="BOXSET_BENCH_CONFIG=$S/benchcfg"
	obs_rr --bench "$BE_CPU" --write "$BED/$RUNC/samples" -- sh -c "$BE_CHILD" sh "$BEPRE" "$RUNC"
	RRENV=
	need 'the owner config path never reached the runner scratch' obs_ok || return 1
	_r=$S/repo/$BED/$RUNC
	need 'child and publication succeeded' [ "$RC" = 0 ] &&
		need 'published line' grep -Fxq "DEV-RUN-BENCH-PUBLISHED run=$RUNC files=4" "$S/out" &&
		need 'go test control passed' grep -Eq '^--- PASS: TestLatencyGateControlSamples \(' "$S/out" || return 1
	need 'no attempt allowed' none -q '^canary .*=allowed$' "$S/out" &&
		need 'every attempt denied' [ "$(grep -c '^canary .*=denied$' "$S/out" | tr -d ' ')" = 26 ] &&
		need 'config mounted' grep -Fxq 'canary config=present' "$S/out" &&
		need 'decoys not imported' [ "$(grep -c '^canary decoy=absent$' "$S/out" | tr -d ' ')" = 4 ] &&
		need 'child made its own look-alike' grep -Fxq 'canary fresh=created' "$S/out" || return 1
	for _f in before during after; do
		_h=$(sha256sum <"$_o/$_f.json") || return 1
		need "$_f.json imported unchanged" grep -Fxq "sum ${_h%% *} $_f.json" "$S/out" || return 1
	done
	_h=$(sha256sum <"$_o/samples/samples.jsonl") || return 1
	need 'samples.jsonl imported unchanged' grep -Fxq "sum ${_h%% *} samples.jsonl" "$S/out" || return 1
	BE_IMG=$(sed -n 's/^dev-run: image=\(sha256:[0-9a-f]*\) inputs=\([0-9a-f]*\) .*/\1/p' "$S/out")
	BE_INP=$(sed -n 's/^dev-run: image=\(sha256:[0-9a-f]*\) inputs=\([0-9a-f]*\) .*/\2/p' "$S/out")
	need 'run identity line' [ "${#BE_IMG}" = 71 ] && [ "${#BE_INP}" = 64 ] || return 1
	need 'only the four names are published' [ "$(cd "$_r" && find . -mindepth 1 | LC_ALL=C sort | tr '\n' ' ')" = './after.json ./before.json ./during.json ./samples ./samples/samples.jsonl ' ] || return 1
	for _f in before during after; do
		_p=$_r/$_f.json
		_sz=$(wc -c <"$_p" | tr -d ' ')
		need "$_f.json is the host's own bounded record" [ -f "$_p" ] && [ ! -L "$_p" ] && [ -O "$_p" ] && [ "$_sz" -ge 1 ] && [ "$_sz" -le 65536 ] || return 1
		need "$_f.json is one fixed line" [ "$(wc -l <"$_p" | tr -d ' ')" = 1 ] && [ "$(grep -o '"[a-z0-9_]*":' "$_p" | wc -l | tr -d ' ')" = 29 ] || return 1
		need "$_f.json holds no path or host text" none -q -e '/' -e '\\' "$_p" || return 1
		need "$_f.json names its stage, run, image and inputs" grep -q "^{\"schema\":1,\"stage\":\"$_f\",\"run\":\"$RUNC\",\"image\":\"$BE_IMG\",\"inputs\":\"$BE_INP\"," "$_p" || return 1
		need "$_f.json observed one core" grep -q '"cpus":1,"cores":1,' "$_p" || return 1
		need "$_f.json carries the 4 GiB memory and swap limits and the matching cpuset" grep -q '"mem_limit_bytes":4294967296,"memswap_limit_bytes":4294967296,"cpuset_matches":true,' "$_p" || return 1
	done
	_n=$(sed -n 's/.*"observations":\([0-9][0-9]*\),.*/\1/p' "$_r/during.json")
	need 'the child was observed more than once' [ "${_n:-0}" -ge 2 ] || return 1
	need 'samples carry this run and image' grep -q "^{\"schema\":1,\"kind\":\"parser-latency-samples\",\"run\":\"$RUNC\",\"inputs_sha256\":\"$BE_INP\"," "$_r/samples/samples.jsonl" || return 1
	BE_N=$((BE_N + 1))
}
# be_reason RUN: `topology` when RUN's before start_unix and after end_unix fall on the same UTC date, else
# `date_rollover`; status 1 (nothing printed) when either time is absent. The one copy of the date rule.
be_reason() {
	_s0=$(sed -n 's/.*"start_unix":\([0-9][0-9]*\),.*/\1/p' "$S/repo/$BED/$1/before.json")
	_e1=$(sed -n 's/.*"end_unix":\([0-9][0-9]*\),.*/\1/p' "$S/repo/$BED/$1/after.json")
	[ -n "$_s0" ] && [ -n "$_e1" ] || return 1
	if [ "$(date -u -d "@$_s0" +%Y%m%d)" = "$(date -u -d "@$_e1" +%Y%m%d)" ]; then echo topology; else echo date_rollover; fi
}
# be_b: the next invocation. Its image is refreshed with what the first run published and with the
# smoke's hostile run directories; the final judge, built as production, reads them from that image.
# The real host-written records are INVALID for one core (topology), or date_rollover when their before start and
# after end fall on different UTC dates; the two synthetic runs 20200113 and 20200114 assert those reasons separately.
# Every other control is INVALID for its own reason; none is ever a PASS.
be_b() {
	_day=$(sed -n '1s/.*"start_date":"\([0-9]*\)".*/\1/p' "$S/repo/$BED/$RUNC/samples/samples.jsonl")
	need 'samples date' [ "${#_day}" = 8 ] || return 1
	BE_T0=$(date -u -d "$(printf '%s' "$_day" | sed 's/\(....\)\(..\)\(..\)/\1-\2-\3/') 12:00:00" +%s) || return 1
	_d=$S/repo/$BED
	be_set "$_d/bench-20200102T000000Z" bench-20200102T000000Z && rm "$_d/bench-20200102T000000Z/after.json" || return 1
	be_set "$_d/bench-20200103T000000Z" bench-20200103T000000Z && printf '{"host":"example.invalid"}\n' >"$_d/bench-20200103T000000Z/during.json" || return 1
	be_set "$_d/bench-20200104T000000Z" bench-20200104T000000Z || return 1
	_l=$(be_rec after bench-20200104T000000Z $((BE_T0 + 10)) $((BE_T0 + 10)) 1 | wc -c | tr -d ' ')
	{ be_rec after bench-20200104T000000Z $((BE_T0 + 10)) $((BE_T0 + 10)) 1 && head -c $((65537 - _l)) /dev/zero | tr '\0' ' '; } >"$_d/bench-20200104T000000Z/after.json" || return 1
	need 'oversized record is 64 KiB + 1' [ "$(wc -c <"$_d/bench-20200104T000000Z/after.json" | tr -d ' ')" = 65537 ] || return 1
	be_set "$_d/bench-20200105T000000Z" bench-20200105T000000Z && be_rec before bench-20200105T000000Z "$BE_T0" "$BE_T0" 1 >"$_d/bench-20200105T000000Z/after.json" || return 1
	be_set "$_d/bench-20200106T000000Z" bench-20200106T000000Z && be_rec during bench-20200107T000000Z $((BE_T0 + 1)) $((BE_T0 + 9)) 9 >"$_d/bench-20200106T000000Z/during.json" || return 1
	be_set "$_d/bench-20200107T000000Z" bench-20200107T000000Z && rm "$_d/bench-20200107T000000Z/before.json" && ln -s during.json "$_d/bench-20200107T000000Z/before.json" || return 1
	be_set "$_d/bench-20200108T000000Z" bench-20200108T000000Z &&
		sed -i 's/"foreign_allowed_max":0/"foreign_allowed_max":3/' "$_d/bench-20200108T000000Z/during.json" || return 1
	# Otherwise eligible, /config empty before and after but not during: the stage record the real writer
	# produces for a during-only change (before true, during false, after true).
	be_set "$_d/bench-20200112T000000Z" bench-20200112T000000Z &&
		sed -i 's/"config_empty":true/"config_empty":false/' "$_d/bench-20200112T000000Z/during.json" &&
		[ "$(grep -c '"config_empty":false' "$_d/bench-20200112T000000Z/during.json")" = 1 ] &&
		[ "$(grep -c '"config_empty":true' "$_d/bench-20200112T000000Z/before.json" "$_d/bench-20200112T000000Z/after.json" | tr '\n' ' ' | sed 's/[^ ]*://g')" = '1 1 ' ] || return 1
	# Same UTC day on one core: topology. After record ends one UTC day later: date_rollover.
	be_set "$_d/bench-20200113T000000Z" bench-20200113T000000Z &&
		sed -i 's/"cpus":4,"cores":4,/"cpus":1,"cores":1,/' "$_d/bench-20200113T000000Z/before.json" "$_d/bench-20200113T000000Z/during.json" "$_d/bench-20200113T000000Z/after.json" &&
		[ "$(grep -l '"cpus":1,"cores":1,' "$_d/bench-20200113T000000Z/before.json" "$_d/bench-20200113T000000Z/during.json" "$_d/bench-20200113T000000Z/after.json" | wc -l | tr -d ' ')" = 3 ] || return 1
	be_set "$_d/bench-20200114T000000Z" bench-20200114T000000Z &&
		be_rec after bench-20200114T000000Z $((BE_T0 + 10)) $((BE_T0 + 86400)) 1 >"$_d/bench-20200114T000000Z/after.json" || return 1
	rr -- sh -c "$BE_JUDGE" sh "$RUNC" $BE_HOSTILE
	need 'judge child ran' [ "$RC" = 0 ] || return 1
	_nfiles=$(grep -c '^imported-f ' "$S/out" | tr -d ' ')
	_ndirs=$(grep -c '^imported-d ' "$S/out" | tr -d ' ')
	need 'the image imported the records' [ "$_nfiles" -ge 20 ] && [ "$_ndirs" -ge 10 ] || return 1
	need 'imported files are all root-owned 0444' [ "$(grep -c '^imported-f 444 root ' "$S/out" | tr -d ' ')" = "$_nfiles" ] &&
		need 'imported directories are all root-owned 0555' [ "$(grep -c '^imported-d 555 root ' "$S/out" | tr -d ' ')" = "$_ndirs" ] &&
		need 'no decoy imported' none -q -e 'notes.txt' -e '[.]bak' -e 'extra.jsonl' -e 'stray.json' "$S/out" &&
		need 'run samples imported' grep -Fxq "imported-f 444 root docs/spikes/data/$RUNC/samples/samples.jsonl" "$S/out" || return 1
	# Each run's reason word comes from be_reason: topology (one core), or date_rollover when its records cross UTC midnight.
	_r0=$(be_reason "$RUNC") && _r13=$(be_reason bench-20200113T000000Z) && _r14=$(be_reason bench-20200114T000000Z) || {
		echo '# not met: record times'
		return 1
	}
	be_expect "$RUNC" 3 "PARSER-LATENCY-INVALID reason=$_r0" &&
		be_expect bench-20200113T000000Z 3 "PARSER-LATENCY-INVALID reason=$_r13" &&
		be_expect bench-20200114T000000Z 3 "PARSER-LATENCY-INVALID reason=$_r14" &&
		be_expect bench-20200102T000000Z 3 'PARSER-LATENCY-INVALID reason=record_missing' &&
		be_expect bench-20200103T000000Z 3 'PARSER-LATENCY-INVALID reason=record_incomplete' &&
		be_expect bench-20200104T000000Z 3 'PARSER-LATENCY-INVALID reason=record_oversized' &&
		be_expect bench-20200105T000000Z 3 'PARSER-LATENCY-INVALID reason=record_stage' &&
		be_expect bench-20200106T000000Z 3 'PARSER-LATENCY-INVALID reason=identity_mismatch' &&
		be_expect bench-20200107T000000Z 3 'PARSER-LATENCY-INVALID reason=record_nonregular' &&
		be_expect bench-20200108T000000Z 3 'PARSER-LATENCY-INVALID reason=overlap' &&
		be_expect bench-20200109T000000Z 3 'PARSER-LATENCY-INVALID reason=record_missing' &&
		be_expect bench-20200112T000000Z 3 'PARSER-LATENCY-INVALID reason=filesystem' &&
		need 'no pass line' none -q PARSER-LATENCY-PASS "$S/out"
}
# BE_FINAL: the final judge of the next invocation, built as production (CGO off). Before it reads
# the records it is about to judge, this child (UID 1000) tries to overwrite, append to, unlink,
# replace and chmod each of them, to create entries beside them, to replace, remove or move the run
# directory, its samples directory and every ancestor up to /src/docs; every attempt is denied and
# every digest, owner, mode and inode is unchanged afterwards. It then judges the run, a complete
# look-alike of it that this child makes inside the data directory (valid names and content, its own
# files, writable) and one it makes outside the image: only the run of the image can pass.
BE_FINAL='CGO_ENABLED=0 go build -trimpath -o /tmp/parserbench ./bench/parser || exit 1
run=docs/spikes/data/$1
snap() { for f in before.json during.json after.json samples/samples.jsonl; do stat -c "%u:%g:%a:%i:%s" "$run/$f"; sha256sum <"$run/$f"; done; for d in "$run" "$run/samples" docs/spikes/data docs/spikes docs; do stat -c "%u:%g:%a:%i" "$d"; done; }
t() { n=$1; shift; if "$@" >/dev/null 2>&1; then echo "canary $n=allowed"; else echo "canary $n=denied"; fi; }
before=$(snap)
echo own >/tmp/own
for f in before.json during.json after.json samples/samples.jsonl; do
	t overwrite-$f sh -c ": >$run/$f"
	t append-$f sh -c "printf x >>$run/$f"
	t unlink-$f rm "$run/$f"
	t replace-$f mv -f /tmp/own "$run/$f"
	t chmod-$f chmod u+w "$run/$f"
done
t create-beside-records sh -c ": >$run/new.json"
t create-beside-samples sh -c ": >$run/samples/new.jsonl"
cp -R --no-preserve=mode "$run" "$run.alt" >/dev/null 2>&1
t replace-run mv -T "$run.alt" "$run"
t remove-run rm -r "$run"
t move-run mv "$run" "$run.moved"
t remove-samples rm -r "$run/samples"
t move-samples mv "$run/samples" "$run/samples.moved"
t move-data mv docs/spikes/data docs/spikes/data2
t remove-data rm -r docs/spikes/data
t move-spikes mv docs/spikes docs/spikes2
t move-docs mv docs docs2
rm -rf "$run.alt"
after=$(snap)
[ "$before" = "$after" ] && echo "canary unchanged=yes" || echo "canary unchanged=no"
la=bench-20200110T000000Z
out=/tmp/bench-20200111T000000Z
for d in docs/spikes/data/$la $out; do
	mkdir -p "$d/samples" || exit 1
	for f in before.json during.json after.json samples/samples.jsonl; do sed "s/$1/$la/g" "$run/$f" >"$d/$f" || exit 1; done
done
for d in "$run" docs/spikes/data/$la $out; do
	o=$(/tmp/parserbench -judge "$d")
	rc=$?
	printf "judged %s rc=%s %s\n" "${d##*/}" "$rc" "$o"
done'
# be_c: the next invocation after the host-written records of the first run are replaced by
# independently written valid ones (they name the same image and inputs as its samples): the final
# judge, again from a refreshed image, accepts the very samples the first run exported, after the
# child that runs it failed to change, replace or remove any of what it reads; a complete child-made
# look-alike, in the data directory or outside the image, is INVALID and never a pass.
be_c() {
	be_set "$S/repo/$BED/$RUNC" "$RUNC" || return 1
	rr -- sh -c "$BE_FINAL" sh "$RUNC"
	need 'judge child ran' [ "$RC" = 0 ] || return 1
	need 'no attempt allowed' none -q '^canary .*=allowed$' "$S/out" &&
		need 'every attempt denied' [ "$(grep -c '^canary .*=denied$' "$S/out" | tr -d ' ')" = 31 ] &&
		need 'bytes, owners, modes and identities unchanged' grep -Fxq 'canary unchanged=yes' "$S/out" || return 1
	need 'valid control passes' grep -Eq "^judged $RUNC rc=0 PARSER-LATENCY-PASS eligible=VALID p50_ns=[0-9]+ p99_ns=[0-9]+ samples=[0-9]+ rounds=30 base=none\$" "$S/out" &&
		need 'a writable look-alike in the data directory is INVALID' grep -Fxq 'judged bench-20200110T000000Z rc=3 PARSER-LATENCY-INVALID reason=record_mutable' "$S/out" &&
		need 'a look-alike outside the image is INVALID' grep -Fxq 'judged bench-20200111T000000Z rc=3 PARSER-LATENCY-INVALID reason=run_location' "$S/out" &&
		need 'exactly one pass line' [ "$(grep -c 'PARSER-LATENCY-PASS' "$S/out" | tr -d ' ')" = 1 ] || return 1
	BE_N=$((BE_N + 3))
}
# be_d: host-side evidence faults, each on the real runner with a passive shim in front of one host
# tool (never of Docker's behaviour in the child) and each a fresh run directory that must not exist
# afterwards. A sampler that fails after a good observation (the final observation made after it
# succeeds, so only its status shows it), a /proc enumeration cut short after one readable task
# (find timed out), an unreadable live task, a /config that cannot be scanned and a daemon that is
# not this machine (another endpoint, another kernel) all fail closed with the fixed reason and
# publish nothing; a task that vanished while the walk ran does not (the same shims, the run is
# published); and a /config that gains an entry between the stages reaches the stage records.
BE_FASTCHILD='mkdir -p "$1" && printf "x\n" >"$1/samples.jsonl"; sleep "$2"; exit "$3"'
BE_RUNS=0
# be_shims: the shim directory. Each shim does nothing unless its DR_* word is given.
be_shims() {
	mkdir "$S/bshim" "$S/dirtask" || return 1
	cat >"$S/bshim/date" <<'EOF'
#!/bin/sh
if [ -n "${DR_MASK_AFTER_OBS:-}" ] && [ ! -e "$DR_MASK_FLAG" ]; then
	for f in "$DR_MASK_AFTER_OBS"/boxset-dev-run.*/f/during.obs; do
		[ -s "$f" ] || continue
		l=$(wc -l <"$f")
		r=$("$DR_REAL_DOCKER" ps -q --filter label=boxset.dev-run --filter status=running 2>/dev/null | wc -l)
		if [ "$l" -ge 1 ] && [ "$r" -ge 1 ] && [ ! -e "$DR_RELEASE" ] && mount -t tmpfs tmpfs "/proc/$DR_LMASK_PID" && [ ! -e "/proc/$DR_LMASK_PID/status" ]; then
			echo "$l running=$r gate=held" >"$DR_MASK_FLAG"
			: >"$DR_RELEASE"
		fi
	done
fi
if [ -n "${DR_DATE_FAIL_AT:-}${DR_DATE_TOUCH_AT:-}${DR_DATE_RENAME_AT:-}" ]; then
	n=$(cat "$DR_DATE_COUNT" 2>/dev/null)
	n=$((${n:-0} + 1))
	echo "$n" >"$DR_DATE_COUNT"
	[ "$n" != "${DR_DATE_FAIL_AT:-}" ] || exit 1
	[ "$n" != "${DR_DATE_TOUCH_AT:-}" ] || : >"$DR_CFG_DIR/during"
	[ "$n" != "${DR_DATE_UNTOUCH_AT:-}" ] || rm -f "$DR_CFG_DIR/during"
	if [ "$n" = "${DR_DATE_RENAME_AT:-}" ]; then
		mv "$DR_CFG_DIR" "$DR_CFG_DIR.old" && mkdir "$DR_CFG_DIR"
	fi
fi
exec "$DR_REAL_DATE" "$@"
EOF
	cat >"$S/bshim/find" <<'EOF'
#!/bin/sh
if [ "${1:-}" = /proc ]; then
	case ${DR_FIND_MODE:-} in
	partial)
		"$DR_REAL_FIND" "$@" | head -n 1
		exit 124
		;;
	fake)
		"$DR_REAL_FIND" "$@"
		echo "$DR_FAKE_STATUS"
		exit 0
		;;
	hidden)
		"$DR_REAL_FIND" "$@" 2>/dev/null | head -n 3
		exit 0
		;;
	live)
		echo "$DR_DIRTASK"
		"$DR_REAL_FIND" "$@"
		exit $?
		;;
	vanish)
		echo /proc/99999999/task/99999999/status
		echo "find: '/proc/99999999/task': No such file or directory" >&2
		"$DR_REAL_FIND" "$@" 2>/dev/null
		exit 1
		;;
	esac
fi
if [ -n "${DR_CFG_FAULT:-}" ] && [ "${1:-}" = "$DR_CFG_FAULT" ]; then
	c=$(cat "$DR_FIND_COUNT" 2>/dev/null)
	c=$((${c:-0} + 1))
	echo "$c" >"$DR_FIND_COUNT"
	if [ "$c" -ge "${DR_CFG_FAULT_FROM:-1}" ]; then
		echo "find: cannot open directory: Permission denied" >&2
		exit 1
	fi
fi
exec "$DR_REAL_FIND" "$@"
EOF
	cat >"$S/bshim/docker" <<'EOF'
#!/bin/sh
if [ -n "${DR_CTX_HOST:-}" ] && [ "${1:-}" = context ] && [ "${2:-}" = inspect ]; then
	echo "$DR_CTX_HOST"
	exit 0
fi
if [ -n "${DR_KERNEL:-}" ] && [ "${1:-}" = info ] && [ "${3:-}" = '{{.KernelVersion}}' ]; then
	echo "$DR_KERNEL"
	exit 0
fi
if [ -n "${DR_SWAP_DIR:-}" ]; then
	case ${1:-} in
	start)
		mv "$DR_SWAP_DIR" "$DR_SWAP_DIR.A" && mkdir "$DR_SWAP_DIR" && : >"$DR_SWAP_DIR.armed"
		;;
	inspect)
		if [ "${3:-}" = '{{.State.Pid}} {{.State.Running}}' ] && [ -e "$DR_SWAP_DIR.armed" ]; then
			out=$("$DR_REAL_DOCKER" "$@")
			rc=$?
			case $out in
			*' true')
				rm -f "$DR_SWAP_DIR.armed"
				mv "$DR_SWAP_DIR" "$DR_SWAP_DIR.B" && mv "$DR_SWAP_DIR.A" "$DR_SWAP_DIR"
				;;
			esac
			echo "$out"
			exit "$rc"
		fi
		;;
	esac
fi
if [ -n "${DR_MASK_PID:-}" ] && [ "${1:-}" = inspect ] && [ "${3:-}" = '{{.Id}}' ] && [ ! -e "$DR_MASK_FLAG" ]; then
	mount -t tmpfs tmpfs "/proc/$DR_MASK_PID" && [ ! -e "/proc/$DR_MASK_PID/status" ] && : >"$DR_MASK_FLAG"
fi
if [ -n "${DR_PID_FAKE:-}" ] && [ "${1:-}" = inspect ] && [ "${3:-}" = '{{.State.Pid}} {{.State.Running}}' ]; then
	echo "$DR_PID_FAKE true"
	exit 0
fi
if [ -n "${DR_CFG_TOUCH:-}" ] && [ "${1:-}" = inspect ] && [ "${3:-}" = '{{.State.Status}} {{.State.ExitCode}}' ]; then
	: >"$DR_CFG_TOUCH/late"
fi
exec "$DR_REAL_DOCKER" "$@"
EOF
	chmod 0755 "$S/bshim/date" "$S/bshim/find" "$S/bshim/docker" || return 1
	BE_SHIM="PATH=$S/bshim:$PATH DR_REAL_DATE=$(command -v date) DR_REAL_FIND=$(command -v find) DR_REAL_DOCKER=$(command -v docker) DR_DATE_COUNT=$S/date.count DR_FIND_COUNT=$S/find.count DR_DIRTASK=$S/dirtask"
}
# be_try CFG WORDS SLEEP: one --bench run of the smoke's own (a fresh run name, a child that writes its
# samples and stays SLEEP seconds) with the shim words WORDS; sets BE_RUN and RC.
be_try() {
	BE_RUNS=$((BE_RUNS + 1))
	BE_RUN=bench-20261017T12$(printf '%02d' "$BE_RUNS")00Z
	rm -f "$S/date.count"
	RRENV="BOXSET_BENCH_CONFIG=$1 $BE_SHIM $2"
	rr --bench "$BE_CPU" --write "$BED/$BE_RUN/samples" -- sh -c "$BE_FASTCHILD" sh "/src/$BED/$BE_RUN/samples" "$3" "${4:-0}"
	RRENV=
}
# be_closed WORD: the last be_try failed closed: 125, the fixed word alone as the runner's own line,
# no run directory or part file, nothing owned left, no config path in the output.
be_closed() {
	need "closed with $1" [ "$RC" = 125 ] && grep -Fxq "dev-run: $1" "$S/out" &&
		need 'nothing published' [ ! -e "$S/repo/$BED/$BE_RUN" ] && [ -z "$(find "$S/repo/$BED" -name '*.part*' -print)" ] &&
		need 'no pass or published line' none -q -e PARSER-LATENCY -e DEV-RUN-BENCH-PUBLISHED "$S/out" &&
		need 'no owner config path in the output' none -Fq -e "$S/benchcfg" "$S/out" || return 1
	BE_N=$((BE_N + 1))
}
be_d() {
	be_shims || return 1
	_r=$S/repo/$BED
	_cfg=$S/benchcfg
	# A sampler whose third date read fails (the second observation of the child's lifetime); the
	# final observation made by the runner (the fourth read) succeeds.
	be_try "$_cfg" "DR_DATE_FAIL_AT=3" 5
	need "the failure was the sampler second observation, and the runner stopped there (date reads $(cat "$S/date.count" 2>/dev/null))" [ "$(cat "$S/date.count" 2>/dev/null)" = 3 ] &&
		be_closed bench_evidence_failed || return 1
	be_try "$_cfg" "DR_FIND_MODE=partial" 1
	be_closed bench_evidence_failed || return 1
	be_try "$_cfg" "DR_FIND_MODE=live" 1
	be_closed bench_evidence_failed || return 1
	be_try "$_cfg" "DR_CFG_FAULT=$_cfg" 1
	be_closed bench_evidence_failed || return 1
	for _w in "DR_CTX_HOST=tcp://203.0.113.9:2375" "DR_CTX_HOST=ssh://203.0.113.9" "DR_KERNEL=0.0.0-not-this-machine"; do
		lbl >"$S/lbl.before"
		be_try "$_cfg" "$_w" 1
		be_closed 'bench_invalid side=daemon' &&
			need 'no endpoint printed' none -q -e 203.0.113 -e not-this-machine "$S/out" &&
			need 'no container created' [ "$(lbl)" = "$(cat "$S/lbl.before")" ] || return 1
	done
	# A task that vanished while /proc was walked is not a failure: the same shim chain publishes.
	be_try "$_cfg" "DR_FIND_MODE=vanish DR_DATE_FAIL_AT=99999" 1
	need 'a vanished task does not fail the run' [ "$RC" = 0 ] &&
		need 'published' grep -Fxq "DEV-RUN-BENCH-PUBLISHED run=$BE_RUN files=4" "$S/out" &&
		need 'four files' [ "$(cd "$_r/$BE_RUN" && find . -type f | wc -l | tr -d ' ')" = 4 ] || return 1
	BE_N=$((BE_N + 1))
	# A /config that gains an entry after the child ran is seen by the after record, not copied from before.
	mkdir "$S/benchcfg2" || return 1
	be_try "$S/benchcfg2" "DR_CFG_TOUCH=$S/benchcfg2" 1
	need 'published' [ "$RC" = 0 ] && grep -Fxq "DEV-RUN-BENCH-PUBLISHED run=$BE_RUN files=4" "$S/out" || return 1
	need 'before: /config empty' grep -q '"config_empty":true' "$_r/$BE_RUN/before.json" &&
		need 'after: /config not empty' grep -q '"config_empty":false' "$_r/$BE_RUN/after.json" &&
		need 'the late entry exists' [ -e "$S/benchcfg2/late" ] || return 1
	BE_N=$((BE_N + 1))
	# The mounted directory renamed and its path recreated while the child runs: the container stays on
	# the old directory while a scan of the path would report the new one, so it fails closed.
	mkdir "$S/benchcfg3" || return 1
	be_try "$S/benchcfg3" "DR_DATE_RENAME_AT=3 DR_CFG_DIR=$S/benchcfg3" 6
	need 'the directory was really replaced' [ -d "$S/benchcfg3.old" ] || return 1
	be_closed bench_evidence_failed || return 1
	# A /config that changes only while the child runs (a file appears and is gone again before the
	# after state) reaches the during record.
	mkdir "$S/benchcfg4" || return 1
	be_try "$S/benchcfg4" "DR_DATE_TOUCH_AT=3 DR_DATE_UNTOUCH_AT=5 DR_CFG_DIR=$S/benchcfg4" 8
	need 'published' [ "$RC" = 0 ] && grep -Fxq "DEV-RUN-BENCH-PUBLISHED run=$BE_RUN files=4" "$S/out" || return 1
	need 'before: /config empty' grep -q '"config_empty":true' "$_r/$BE_RUN/before.json" &&
		need 'during: /config not empty' grep -q '"config_empty":false' "$_r/$BE_RUN/during.json" &&
		need 'after: /config empty again' grep -q '"config_empty":true' "$_r/$BE_RUN/after.json" &&
		need 'the file is gone' [ ! -e "$S/benchcfg4/during" ] || return 1
	BE_N=$((BE_N + 1))
	# A /config read that fails after the first observation succeeded fails closed.
	rm -f "$S/find.count"
	be_try "$_cfg" "DR_CFG_FAULT=$_cfg DR_CFG_FAULT_FROM=3" 5
	need 'the failing read was not the first' [ "$(cat "$S/find.count" 2>/dev/null)" -ge 3 ] || return 1
	be_closed bench_evidence_failed || return 1
	# A /proc walk that returns well under the kernel's own task count (tasks hidden from the view) fails.
	be_try "$_cfg" "DR_FIND_MODE=hidden" 1
	be_closed bench_evidence_failed || return 1
	# A daemon whose container is not a task of this machine (a forwarded socket to a machine with the same
	# kernel release reports a pid that is not this container's): rejected, nothing published, no endpoint.
	sleep 60 &
	_fp=$!
	lbl >"$S/lbl.before"
	be_try "$_cfg" "DR_PID_FAKE=$_fp" 3
	kill "$_fp" 2>/dev/null
	wait "$_fp" 2>/dev/null
	be_closed 'bench_invalid side=daemon' &&
		need 'no endpoint printed' none -q -e 203.0.113 "$S/out" || return 1
	# The path swapped to another directory while the container starts and back as soon as it runs: the
	# container mounted a directory other than the observed one though every scan of the path reads the
	# observed one, so only the container's own view shows it.
	mkdir "$S/benchcfg5" || return 1
	be_try "$S/benchcfg5" "DR_SWAP_DIR=$S/benchcfg5" 4
	need 'the directory was really swapped and back' [ -d "$S/benchcfg5.B" ] && [ -d "$S/benchcfg5" ] || return 1
	be_closed bench_evidence_failed || return 1
	# A child that wrote its samples and then exited nonzero keeps that exact status; nothing is
	# published, no pass or published line is printed, and the owned containers are removed.
	be_try "$_cfg" "" 1 37
	need 'the nonzero child status is kept' [ "$RC" = 37 ] &&
		need 'nothing published' [ ! -e "$_r/$BE_RUN" ] && [ -z "$(find "$_r" -name '*.part*' -print)" ] &&
		need 'no pass or published line' none -q -e PARSER-LATENCY -e DEV-RUN-BENCH-PUBLISHED "$S/out" &&
		need 'owned containers removed' owned_gone || return 1
	BE_N=$((BE_N + 1))
	be_cap || return 1
	be_cg || return 1
	be_sib || return 1
	be_proc || return 1
}
# be_cap: the child-stdout cap is 256 KiB: a child that prints 200000 bytes (more than the old 64 KiB, as the
# canonical verbose benchmark-package test run does) succeeds with its status, and one that prints a byte
# over the cap is output_overflow, 125, never a success.
be_cap() {
	rr -- sh -c 'head -c 200000 /dev/zero | tr "\0" x; exit 7'
	need 'output of 200000 bytes fits and keeps the child status' [ "$RC" = 7 ] || return 1
	rr -- sh -c 'head -c 262145 /dev/zero | tr "\0" x'
	need 'one byte over 262144 is output_overflow, the last text of the output' [ "$RC" = 125 ] && grep -q 'dev-run: output_overflow$' "$S/out" || return 1
	BE_N=$((BE_N + 2))
	return 0
}
# be_cg: foreign overlap is counted from the effective cgroup cpuset, not from affinity alone. A copy of the
# runner reads a smoke-owned cgroup tree (one root file, so every task inherits it) instead of the host's.
# A busy task pinned to the reserved CPU is runnable and its affinity meets the set throughout. When the
# tree's effective cpuset excludes the set, neither it nor any other task counts (allowed 0, runnable 0);
# when it includes the set, the same tasks do (allowed and runnable above 0). Values are numbers only.
be_cg() {
	_r=$S/repo/$BED
	command -v taskset >/dev/null 2>&1 || return 1
	sed "s#cgroot=/sys/fs/cgroup#cgroot=$S/fakecg#" "$S/repo/scripts/dev-run.sh" >"$S/cgrunner.sh" &&
		[ "$(grep -c "cgroot=$S/fakecg" "$S/cgrunner.sh")" = 1 ] || return 1
	mkdir -p "$S/fakecg/cpuset" || return 1
	for _case in excluded included; do
		if [ "$_case" = excluded ]; then _cp=4094-4095; else _cp=$BE_CPU; fi
		printf '%s\n' "$_cp" >"$S/fakecg/cpuset.cpus.effective" && printf '%s\n' "$_cp" >"$S/fakecg/cpuset/cpuset.effective_cpus" || return 1
		taskset -c "$BE_CPU" sh -c 'while :; do :; done' &
		_bz=$!
		RSH=$S/cgrunner.sh
		be_try "$S/benchcfg" "" 4
		RSH=
		kill "$_bz" 2>/dev/null
		wait "$_bz" 2>/dev/null
		need "$_case: the run published" [ "$RC" = 0 ] && grep -Fxq "DEV-RUN-BENCH-PUBLISHED run=$BE_RUN files=4" "$S/out" || return 1
		if [ "$_case" = excluded ]; then
			need "excluded: affinity overlap outside the effective cpuset does not count, runnable or not" grep -q '"foreign_allowed_max":0,"foreign_runnable_max":0,' "$_r/$BE_RUN/during.json" || return 1
		else
			need "included: tasks whose effective cpuset holds the set count, runnable ones too" grep -Eq '"foreign_allowed_max":[1-9][0-9]*,"foreign_runnable_max":[1-9]' "$_r/$BE_RUN/during.json" || return 1
		fi
		BE_N=$((BE_N + 1))
	done
	return 0
}
# be_sib: a running foreign thread on an SMT sibling outside the set is recorded as sibling_runnable_max
# (numbers only), an idle sibling as 0. A copy of the runner reads a smoke-owned topology tree that names the
# one-core set's sibling: first an idle CPU number that no thread can run on, then a real CPU with a busy
# thread pinned to it (the pair; a busy thread on a CPU that is not the sibling counts nothing).
be_sib() {
	_r=$S/repo/$BED
	sed "s#^SYSCPU=/sys/devices/system/cpu\$#SYSCPU=$S/fakecpu#" "$S/repo/scripts/dev-run.sh" >"$S/sibrunner.sh" &&
		[ "$(grep -c "^SYSCPU=$S/fakecpu\$" "$S/sibrunner.sh")" = 1 ] || return 1
	_x=0
	[ "$BE_CPU" != 0 ] || _x=1
	mkdir -p "$S/fakecpu/cpu$BE_CPU/topology" || return 1
	printf '0\n' >"$S/fakecpu/cpu$BE_CPU/topology/physical_package_id" && printf '0\n' >"$S/fakecpu/cpu$BE_CPU/topology/core_id" || return 1
	for _case in idle busy busynl; do
		if [ "$_case" = idle ]; then _sb=4094; else _sb=$_x; fi
		printf '%s,%s\n' "$BE_CPU" "$_sb" >"$S/fakecpu/cpu$BE_CPU/topology/thread_siblings_list" || return 1
		if [ "$_case" = busynl ]; then
			# The busy thread is named with an embedded newline (a copy of the shell under that name), so its
			# stat record spans two lines.
			_nm=$S/nl/bu$(printf '\nsy')
			[ -e "$_nm" ] || { mkdir -p "$S/nl" && cp -L "$(command -v sh)" "$_nm"; } || return 1
			taskset -c "$_x" "$_nm" -c 'while :; do :; done' &
		else
			taskset -c "$_x" sh -c 'while :; do :; done' &
		fi
		_bz=$!
		RSH=$S/sibrunner.sh
		be_try "$S/benchcfg" "" 4
		RSH=
		kill "$_bz" 2>/dev/null
		wait "$_bz" 2>/dev/null
		need "$_case sibling: the run published" [ "$RC" = 0 ] && grep -Fxq "DEV-RUN-BENCH-PUBLISHED run=$BE_RUN files=4" "$S/out" || return 1
		if [ "$_case" = idle ]; then
			need "idle external sibling records none running on it" grep -q '"sibling_runnable_max":0,' "$_r/$BE_RUN/during.json" || return 1
		else
			need "$_case external sibling records a running thread" grep -Eq '"sibling_runnable_max":[1-9]' "$_r/$BE_RUN/during.json" || return 1
		fi
		BE_N=$((BE_N + 1))
	done
	# A stat record that ends right after the CPU field cannot delimit that token (it may be a truncated
	# number): the observation fails closed. The same record with one more field is parsed and counted, so the
	# fake task is really read. The fake task is a smoke-owned status and stat pair beside a real process id (its
	# cgroup is read from the real /proc), running state, affinity outside the set, CPU one of the siblings.
	printf '%s,4094\n' "$BE_CPU" >"$S/fakecpu/cpu$BE_CPU/topology/thread_siblings_list" || return 1
	sleep 60 &
	_fp=$!
	_fd=$S/fk/$_fp/task/$_fp
	mkdir -p "$_fd" || return 1
	printf 'Tgid:\t%s\nPPid:\t1\nState:\tR (running)\nVmSize:\t1 kB\nCpus_allowed_list:\t4094\n' "$_fp" >"$_fd/status" || return 1
	_pre="$_fp (fake) R"
	_i=2
	while [ "$_i" -le 36 ]; do
		_pre="$_pre 0"
		_i=$((_i + 1))
	done
	printf '%s 4094\n' "$_pre" >"$_fd/stat.trunc" && printf '%s 4094 0\n' "$_pre" >"$_fd/stat.full" || return 1
	for _case in trunc full; do
		cp "$_fd/stat.$_case" "$_fd/stat" || return 1
		RSH=$S/sibrunner.sh
		be_try "$S/benchcfg" "DR_FIND_MODE=fake DR_FAKE_STATUS=$_fd/status" 3
		RSH=
		if [ "$_case" = trunc ]; then
			be_closed bench_evidence_failed || { kill "$_fp" 2>/dev/null; return 1; }
		else
			need "complete fake stat record is read and counted" [ "$RC" = 0 ] && grep -Eq '"sibling_runnable_max":[1-9]' "$_r/$BE_RUN/during.json" || { kill "$_fp" 2>/dev/null; return 1; }
			BE_N=$((BE_N + 1))
		fi
	done
	kill "$_fp" 2>/dev/null
	wait "$_fp" 2>/dev/null
	return 0
}
# be_proc: a /proc that can hide tasks is rejected before anything runs, whoever the caller. In a private
# mount namespace, with a root-owned competing task running, /proc is mounted with hidepid=2, hidepid=4 and
# hidepid=ptraceable (each as an unprivileged user, for whom the competing task really is not there, and as
# root, who still sees it: the mount option alone rejects), and a tmpfs is mounted over that one task's
# /proc entry (it hides the task from everyone, root included, while the visible share of the kernel's task
# count stays far above the old 97% bound). Every case is 125 with the fixed reason, nothing created.
be_proc() {
	_dr=$S/repo/$BED
	chmod 0711 "$S" || return 1
	cat >"$S/proc.sh" <<'EOF'
#!/bin/sh
case $4 in
mask) mount -t tmpfs tmpfs "/proc/$1" || exit 99 ;;
stack) mount -t proc proc /proc || exit 99 ;;
delayed | later) : ;; # no mask yet: delayed is mounted by the docker shim after the startup guard, later by the date shim after a successful observation
*)
	# The one mount at /proc is replaced, not stacked, so that the mode is what is under test.
	umount -l /proc || exit 98
	mount -t proc -o "hidepid=$4" proc /proc || exit 99
	;;
esac
echo "canary proc-mounts=$(awk '$5 == "/proc"' /proc/self/mountinfo | wc -l)"
setpriv --reuid=65534 --regid=65534 --clear-groups sh -c '[ ! -e "/proc/$1/status" ] && echo "canary competing-task=hidden" || echo "canary competing-task=visible"' sh "$1"
[ -e "/proc/$1/status" ] && echo "canary root-sees-it=yes" || echo "canary root-sees-it=no"
v=$(find /proc -mindepth 4 -maxdepth 4 -path '/proc/[0-9]*/task/[0-9]*/status' 2>/dev/null | wc -l)
t=$(awk '{ split($4, a, "/"); print a[2] }' /proc/loadavg)
[ $((v * 100)) -ge $((t * 97)) ] && echo "canary share-above-old-threshold=yes" || echo "canary share-above-old-threshold=no"
case $4 in
later)
	# The child holds on a gate file the host creates in /config, not on a sleep: it is alive until released.
	exec sh scripts/dev-run.sh --bench 1 --timeout 120 --write "$3" -- sh -c 'while [ ! -e /config/release ]; do sleep 0.1; done'
	;;
esac
CH=0
case $2 in
nobody) exec setpriv --reuid=65534 --regid=65534 --clear-groups sh scripts/dev-run.sh --bench 1 --write "$3" -- sh -c 'sleep "$1"' x "$CH" ;;
*) exec sh scripts/dev-run.sh --bench 1 --write "$3" -- sh -c 'sleep "$1"' x "$CH" ;;
esac
EOF
	sleep 120 &
	_sp=$!
	lbl >"$S/lbl.before"
	_k=0
	for _mode in 2 4 ptraceable stack mask delayed later; do
		for _who in nobody root; do
			[ "$_mode" != delayed ] && [ "$_mode" != later ] || [ "$_who" = root ] || continue
			_x=
			if [ "$_mode" = delayed ]; then
				rm -f "$S/mask.applied"
				_x="$BE_SHIM DR_MASK_PID=$_sp DR_MASK_FLAG=$S/mask.applied"
			fi
			_cf=
			if [ "$_mode" = later ]; then
				rm -rf "$S/mask.applied" "$S/tmpl" "$S/benchcfgL"
				mkdir "$S/tmpl" "$S/benchcfgL" || return 1
				_cf=$S/benchcfgL
				_x="$BE_SHIM TMPDIR=$S/tmpl DR_MASK_AFTER_OBS=$S/tmpl DR_LMASK_PID=$_sp DR_MASK_FLAG=$S/mask.applied DR_RELEASE=$S/benchcfgL/release"
			fi
			_k=$((_k + 1))
			_rn=bench-20261017T13$(printf '%02d' "$_k")00Z
			(cd "$S/repo" && exec env BOXSET_BENCH_CONFIG=${_cf:-$S/benchcfg} $_x unshare -m --propagation private sh "$S/proc.sh" "$_sp" "$_who" "$BED/$_rn/samples" "$_mode") </dev/null >"$S/out" 2>&1
			RC=$?
			need "$_mode/$_who: rejected, status 125 and the fixed reason" [ "$RC" = 125 ] && grep -Fxq 'dev-run: bench_invalid side=proc' "$S/out" &&
				need "$_mode/$_who: no pass, no publication" none -q -e PARSER-LATENCY -e DEV-RUN-BENCH-PUBLISHED "$S/out" &&
				need "$_mode/$_who: nothing created" [ "$(lbl)" = "$(cat "$S/lbl.before")" ] && [ ! -e "$_dr/$_rn" ] || { kill "$_sp" 2>/dev/null; return 1; }
			if [ "$_who" = nobody ] && [ "$_mode" != stack ] || [ "$_mode" = mask ]; then
				need "$_mode/$_who: the competing task was really hidden from this caller" grep -Fxq 'canary competing-task=hidden' "$S/out" || { kill "$_sp" 2>/dev/null; return 1; }
			fi
			case $_mode in
			2 | 4 | ptraceable)
				need "$_mode/$_who: the restricted /proc is the one mount at /proc, so only the mode can be what rejects it" grep -Fxq 'canary proc-mounts=1' "$S/out" || { kill "$_sp" 2>/dev/null; return 1; }
				;;
			stack)
				need "$_mode/$_who: two mounts at /proc" grep -Fxq 'canary proc-mounts=2' "$S/out" || { kill "$_sp" 2>/dev/null; return 1; }
				;;
			mask)
				need "$_mode/$_who: root cannot see it either, and the visible share is above the old bound" grep -Fxq 'canary root-sees-it=no' "$S/out" &&
					need "$_mode/$_who: the old 97% rule would have accepted it" grep -Fxq 'canary share-above-old-threshold=yes' "$S/out" &&
					need "$_mode/$_who: only the one task entry is masked" grep -Fxq 'canary proc-mounts=1' "$S/out" || { kill "$_sp" 2>/dev/null; return 1; }
				;;
			esac
			if [ "$_mode" = delayed ] || [ "$_mode" = later ]; then
				need "$_mode/root: the mask was applied, hiding the task from root" [ -e "$S/mask.applied" ] || { kill "$_sp" 2>/dev/null; return 1; }
			fi
			if [ "$_mode" = later ]; then
				need "later/root: at mask time a successful observation existed and the child was still running, held on its gate ($(cat "$S/mask.applied" 2>/dev/null))" grep -Eq '^[1-9][0-9]* running=[1-9][0-9]* gate=held$' "$S/mask.applied" || { kill "$_sp" 2>/dev/null; return 1; }
				need "later/root: the gate was released only after the mask" [ -e "$S/benchcfgL/release" ] || { kill "$_sp" 2>/dev/null; return 1; }
			fi
			BE_N=$((BE_N + 1))
		done
	done
	kill "$_sp" 2>/dev/null
	wait "$_sp" 2>/dev/null
	return 0
}
# be_e: a disposable image-input fault. The scratch Dockerfile gets one extra last RUN that leaves an
# imported canonical fact writable by UID 1000 (every record and directory owned by it), or only a
# replacement-capable parent (the run directory owned by it, the files still root 0444), while every
# imported byte, and so the image manifest, stays the same. The next invocation's child shows the
# write access is real, and the final judge, built as production, returns record_mutable for the run
# that passes on the normal image (be_c) and never a pass line.
BE_FAULT='CGO_ENABLED=0 go build -trimpath -o /tmp/parserbench ./bench/parser || exit 1
run=docs/spikes/data/$1
for p in "$run/before.json" "$run/samples/samples.jsonl" "$run" "$run/samples"; do
	[ -w "$p" ] && echo "canary writable ${p#"$run"}=yes" || echo "canary writable ${p#"$run"}=no"
done
o=$(/tmp/parserbench -judge "$run")
printf "judged %s rc=%s %s\n" "$1" "$?" "$o"'
be_fault() {
	cp "$S/repo/deploy/dev/Dockerfile" "$S/Dockerfile.fault.orig" || return 1
	printf '\n%s\n' "$2" >>"$S/repo/deploy/dev/Dockerfile"
	rr -- sh -c "$BE_FAULT" sh "$RUNC"
	_frc=$RC
	cat "$S/Dockerfile.fault.orig" >"$S/repo/deploy/dev/Dockerfile" || return 1
	need "$1: the child ran" [ "$_frc" = 0 ] &&
		need "$1: write access is real" grep -Fxq "$3" "$S/out" &&
		need "$1: record_mutable and no pass" grep -Fxq "judged $RUNC rc=3 PARSER-LATENCY-INVALID reason=record_mutable" "$S/out" &&
		need "$1: no pass line" none -q 'PARSER-LATENCY-PASS' "$S/out" || return 1
	BE_N=$((BE_N + 1))
}
be_e() {
	be_fault 'records and directories writable' 'RUN chown -R boxset:boxset /src/docs/spikes/data && chmod -R u+w /src/docs/spikes/data' 'canary writable /before.json=yes' &&
		be_fault 'parent only writable' "RUN chown boxset:boxset /src/docs/spikes/data/$RUNC && chmod 0755 /src/docs/spikes/data/$RUNC" 'canary writable =yes'
}
# be_n: the local-NVMe proof of the runner's /config, on private sysfs trees (never a live host device).
# nv_disk ROOT NAME VENDOR ROT PATH: disk NAME at ROOT/devices/PATH with queue/rotational ROT, a device link
# to the controller directory beside it (vendor text VENDOR there, none when -) and block/NAME for it.
nv_disk() {
	mkdir -p "$1/block" "$1/devices/$5/$2/queue" "$1/devices/$5/ctrl" &&
		printf '%s\n' "$4" >"$1/devices/$5/$2/queue/rotational" &&
		ln -s ../ctrl "$1/devices/$5/$2/device" &&
		ln -s "$1/devices/$5/$2" "$1/block/$2" &&
		{ [ "$3" = - ] || printf '%s\n' "$3" >"$1/devices/$5/ctrl/vendor"; }
}
# nv_md ROOT ROTB: block md0 with two partition slaves, each of its own NVMe disk (the second rotational ROTB).
nv_md() {
	nv_disk "$1" da NVMe 0 pci/ctl-a && nv_disk "$1" db NVMe "$2" pci/ctl-b &&
		mkdir -p "$1/devices/pci/ctl-a/da/da1" "$1/devices/pci/ctl-b/db/db1" "$1/block/md0/slaves" &&
		: >"$1/devices/pci/ctl-a/da/da1/partition" && : >"$1/devices/pci/ctl-b/db/db1/partition" &&
		ln -s "$1/devices/pci/ctl-a/da/da1" "$1/block/md0/slaves/da1" &&
		ln -s "$1/devices/pci/ctl-b/db/db1" "$1/block/md0/slaves/db1"
}
# nv_is ROOT BLOCK WANT RC: the runner's self-check prints exactly WANT (and no path), with status RC.
nv_is() {
	_nvo=$(cd "$S/repo" && sh scripts/dev-run.sh --backing-nvme-check "$1" "/dev/$2" 2>&1 </dev/null)
	_nvc=$?
	need "$2: $3" [ "$_nvo" = "$3" ] &&
		need "$2: status $4" [ "$_nvc" = "$4" ] &&
		need "$2: no path in the output" [ "${_nvo#*/}" = "$_nvo" ] || return 1
	BE_N=$((BE_N + 1))
}
# nv_setup: a synthetic config directory, a df shim that answers only for it (an ext4 md0, a fixture name, not a
# host device; any other df is the real one) and the runner wrapper that binds the tree NV_TREE over /sys in a
# mount namespace of its own (private, so the host's /sys is never touched) before it runs the unmodified runner.
nv_setup() {
	mkdir "$S/nvcfg" "$S/nvshim" &&
		cat >"$S/nvshim/df" <<'EOF' &&
#!/bin/sh
for a; do l=$a; done
[ "$l" = "$NV_CFG" ] || exec "$NV_REAL_DF" "$@"
printf 'Filesystem Type\n/dev/md0 ext4\n'
EOF
		chmod 0755 "$S/nvshim/df" &&
		cat >"$S/nvwrap.sh" <<'EOF'
exec unshare -m sh -c 'mount --bind "$NV_TREE" /sys && exec sh scripts/dev-run.sh "$@"' sh "$@"
EOF
}
# nv_pub TREE WANT RUN: one short --bench publication of the unmodified scratch runner (its line stays
# `backing_nvme /sys "$1"`) over TREE bound as /sys: the host-written before, during and after records each carry
# config_nvme WANT (true or false) and the runner's output has no slash.
nv_pub() {
	RSH=$S/nvwrap.sh
	RRENV="BOXSET_BENCH_CONFIG=$S/nvcfg PATH=$S/nvshim:$PATH NV_REAL_DF=$(command -v df) NV_CFG=$S/nvcfg NV_TREE=$1"
	rr --bench "$BE_CPU" --write "$BED/$3/samples" -- sh -c "$BE_FASTCHILD" sh "/src/$BED/$3/samples" 1 0
	RRENV=
	RSH=
	need "$3: published" [ "$RC" = 0 ] &&
		need "$3: published line" grep -Fxq "DEV-RUN-BENCH-PUBLISHED run=$3 files=4" "$S/out" &&
		need "$3: the output has no slash" none -q -e / "$S/out" || return 1
	for _f in before during after; do
		need "$3: $_f.json config_nvme $2" grep -q "\"config_fs\":\"ext4\",\"config_nvme\":$2," "$S/repo/$BED/$3/$_f.json" || return 1
	done
	rm -rf "$S/repo/$BED/$3" || return 1
	BE_N=$((BE_N + 1))
}
# The one config call passes the literal /sys and no device-name test is left; the usage line is untouched.
# After the controls, the scratch copy of the runner (and so every real --bench run below) reads the first
# tree instead of this host's /sys, whose df source of the scratch is not a disk.
be_n() {
	_rn=$S/repo/scripts/dev-run.sh
	_nt=$S/nv
	need 'bench_config passes the literal /sys, once' [ "$(grep -Fxc "$(printf '\tbacking_nvme /sys "$1" || return 1')" "$_rn")" = 1 ] &&
		need 'no device-name test is left' none -q 'dev/nvme' "$_rn" &&
		need 'the flag is not in the usage line' none -q 'usage:.*backing-nvme' "$_rn" || return 1
	mkdir "$_nt" && nv_md "$_nt/t1" 0 && nv_md "$_nt/t2" 1 &&
		nv_disk "$_nt/t3" sda ATA 0 pci/ata && nv_disk "$_nt/t4" sdb NVMe 0 pci/iscsi &&
		nv_disk "$_nt/t5" d0 Samsung 0 pci/nvme && mkdir -p "$_nt/t6/block/md1/slaves" || return 1
	nv_disk "$_nt/t7" sdc NVMe 0 pci/nvme && rm "$_nt/t7/devices/pci/nvme/sdc/queue/rotational" &&
		nv_disk "$_nt/t8" sdd NVMe 0 pci/nvme && ln -sfn ../gone "$_nt/t8/devices/pci/nvme/sdd/device" || return 1
	nv_is "$_nt/t1" md0 nvme=1 0 &&
		nv_is "$_nt/t2" md0 nvme=0 0 &&
		nv_is "$_nt/t3" sda nvme=0 0 &&
		nv_is "$_nt/t4" sdb nvme=0 0 &&
		nv_is "$_nt/t5" d0 nvme=1 0 &&
		nv_is "$_nt/t6" md1 unreadable 1 &&
		nv_is "$_nt/t7" sdc unreadable 1 &&
		nv_is "$_nt/t8" sdd unreadable 1 || return 1
	BE_CPU=$(sed -n 's/^Cpus_allowed_list:[[:space:]]*\([0-9][0-9]*\).*/\1/p' /proc/self/status)
	need 'a one-core cpuset' [ -n "$BE_CPU" ] && nv_setup &&
		nv_pub "$_nt/t1" true bench-20261017T110100Z &&
		nv_pub "$_nt/t2" false bench-20261017T110200Z || return 1
	sed 's#^\([[:space:]]*\)backing_nvme /sys "\$1" || return 1$#\1backing_nvme '"$_nt"'/t1 /dev/md0 || return 1#' "$_rn" >"$S/nvrunner.sh" &&
		[ "$(grep -c "backing_nvme $_nt/t1 /dev/md0 || return 1\$" "$S/nvrunner.sh")" = 1 ] &&
		cat "$S/nvrunner.sh" >"$_rn"
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

if [ "$MODE" = benchmark-evidence ]; then
	for _s in be_n be_a be_b be_c be_d be_e; do
		if "$_s"; then
			echo "ok $_s"
		else
			echo "not ok $_s"
			head -c 3000 "$S/out" | sed 's/^/# /'
			exit 1
		fi
	done
	echo "BENCH-EVIDENCE-PASS controls=$BE_N"
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
