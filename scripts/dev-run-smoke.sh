#!/bin/sh
# Small host-only runner/sentinel check (never part of the image context):
#   scripts/dev-run-smoke.sh
# Runs the real scripts/dev-run.sh and Docker from a unique, ownership-checked
# scratch snapshot of the admitted project inputs (explicit roots, with the final
# nested secret and cache exclusions of .dockerignore applied before archiving,
# so no owner-shaped nested file is ever copied), so repository drafts are never
# touched. Seven direct assertions print `ok R<n>` or `not ok R<n>`;
# `DEV-RUN-SMOKE-PASS checks=7` prints only when all seven pass, any failure exits 1.
set -u
LC_ALL=C
export LC_ALL

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
	(cd "$S/repo" && exec env $RRENV sh scripts/dev-run.sh "$@") </dev/null >"$S/out" 2>&1
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
		r1_one ref_unavailable --ref -- true &&
		r1_one capture_unavailable --capture d -- true &&
		r1_one bench_unavailable --bench 1 -- true
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
		grep -q '^dev-run: image=sha256:[0-9a-f]* inputs=[0-9a-f]* .* log=none$' "$S/out"
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
# ponytail: this is a retained-at-removal snapshot; use syscall tracing only if a proof of
# transient writes is ever needed.
obs_setup() {
	_od=$(command -v docker) && mkdir "$S/obs" "$S/tmp5" || return 1
	printf '%s\n' "$SENTINEL" synthetic-url-pass synthetic-query-secret >"$S/obs.needles" || return 1
	cat >"$S/obs/docker" <<'EOF' || return 1
#!/bin/sh
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
		[ -z "$(find "$DRAFTS" -name '*.part*' -print)" ] && ctr_gone && obs_ok
}

# R6: the existing redaction-only Go tests pass through the runner (cgo off, as the repo's
# ordinary Go tests run); the selector names only main_test.go tests.
RX='^(TestSelected|TestSelection|TestUsage|TestOverlaps|TestSecretSplit|TestURL|TestUndelimited|TestNUL|TestRaw|TestWriteError|TestTail|TestBinary|TestReadError|TestOneSelection)'
r6() {
	rr -- env CGO_ENABLED=0 go test ./deploy/dev/redact -run "$RX" -count=1 -v
	[ "$RC" = 0 ] && grep -q '^--- PASS' "$S/out" && ! grep -Eq -- '--- (FAIL|SKIP)' "$S/out" || return 1
	rr -- env CGO_ENABLED=0 go test ./deploy/dev/redact -list "$RX"
	[ "$RC" = 0 ] || return 1
	grep -o '^func Test[A-Za-z0-9_]*' "$S/repo/deploy/dev/redact/main_test.go" | sed 's/^func //' >"$S/names"
	grep '^Test' "$S/out" >"$S/listed"
	[ -s "$S/listed" ] && [ -z "$(grep -Fvx -f "$S/names" "$S/listed")" ]
}

# R7: the root alias /src/. gets the real admission controls (an unexpected child variable
# is blocked beside them, never a PASS), a valid small noncanonical manifest copy prints only
# the distinct manifest-only line (no admission PASS, no runtime control), then the real
# admission through the runner blocks a stale annual evidence date and retains the draft.
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
