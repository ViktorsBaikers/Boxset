#!/bin/sh
# Controls for scripts/history-gates.sh, run against real Git history in
# throwaway repositories (host: sh and git only):
#   sh scripts/history-gates-test.sh
# Every control runs the real guard against the real target branch `main`.
# Rejections (`migration_changed`, exit 1): a migration the target has that the
# candidate modifies, renames, deletes or turns into a link, committed or only
# in the working tree; and the histories a merge-base comparison misses: a
# migration squash-merged into the target and then changed on the feature
# branch, a target that advanced after the fork with different bytes at the same
# path, a branch missing a migration the target gained, and an unrelated history.
# Passes (exit 0): an untouched tree, an added migration, a squash-merged
# migration left as it was, a cherry-picked identical migration, and a feature
# branch that merged the advanced target and added its own migration.
# `HISTORY-GATES-TEST-PASS` prints only when every control behaved.
set -eu
LC_ALL=C
export LC_ALL

HERE=$(cd -- "$(dirname -- "$0")" && pwd -P)
GUARD=$HERE/history-gates.sh
[ -f "$GUARD" ] || {
	printf 'history-gates-test: %s is missing\n' "$GUARD" >&2
	exit 2
}

TMP=$(mktemp -d) || exit 2
trap 'rm -rf "$TMP"' EXIT
trap 'exit 2' HUP INT TERM
# The throwaway repositories never read the caller's Git configuration.
GIT_CONFIG_GLOBAL=/dev/null
GIT_CONFIG_SYSTEM=/dev/null
export GIT_CONFIG_GLOBAL GIT_CONFIG_SYSTEM

M=internal/store/migrations
A_BODY='-- +goose Up\nCREATE TABLE a (id INTEGER PRIMARY KEY);\n'
B_BODY='-- +goose Up\nCREATE TABLE b (id INTEGER PRIMARY KEY);\n'
B_OTHER='-- +goose Up\nCREATE TABLE b (id INTEGER PRIMARY KEY, extra TEXT);\n'
C_BODY='-- +goose Up\nCREATE TABLE c (id INTEGER PRIMARY KEY);\n'

CONTROLS=0
FAILED=0

# newrepo NAME: an empty repository on branch main, current directory.
newrepo() {
	cd "$TMP"
	rm -rf "$1"
	mkdir "$1"
	cd "$1"
	git init -q
	git config user.name control
	git config user.email control@example.invalid
	git config commit.gpgsign false
	git checkout -q -b main
	mkdir -p "$M"
	printf 'control\n' >README
	git add -A
	git commit -q -m base
}

commit_all() {
	git add -A
	git commit -q -m "$1"
}

# mig FILE BODY: write a migration with printf escapes expanded.
mig() {
	printf -- "$2" >"$M/$1"
}

# run_guard: the real guard's output and status, as "<status>|<one-line output>"
run_guard() {
	_o=$(sh "$GUARD" main 2>&1) && _s=0 || _s=$?
	printf '%s|%s' "$_s" "$(printf '%s' "$_o" | tr '\n' ' ')"
}

# red NAME: the guard just run must have failed with migration_changed
red() {
	CONTROLS=$((CONTROLS + 1))
	_r=$(run_guard)
	case $_r in
	1\|*migration_changed*HISTORY-GATE-FAIL*) printf 'RED %s: %s\n' "$1" "${_r#*|}" ;;
	*)
		FAILED=$((FAILED + 1))
		printf 'FAIL %s: want exit 1 with migration_changed, got %s\n' "$1" "$_r"
		;;
	esac
}

# green NAME: the guard just run must have passed
green() {
	CONTROLS=$((CONTROLS + 1))
	_r=$(run_guard)
	case $_r in
	0\|*HISTORY-GATE-PASS*) printf 'GREEN %s: %s\n' "$1" "${_r#*|}" ;;
	*)
		FAILED=$((FAILED + 1))
		printf 'FAIL %s: want exit 0 with HISTORY-GATE-PASS, got %s\n' "$1" "$_r"
		;;
	esac
}

# --- the target already holds a merged migration; feature forks from it -----------------
newrepo merged
mig 00001_a.sql "$A_BODY"
commit_all 'merged migration'
git checkout -q -b feature
FORK=$(git rev-parse HEAD)

# reset: the feature branch back at the fork, no stray file
reset() {
	git checkout -q -f feature
	git reset -q --hard "$FORK"
	git clean -fdq
}

reset
green 'untouched tree'

reset
printf -- '-- edited\n' >>"$M/00001_a.sql"
red 'modify, working tree'

reset
printf -- '-- edited\n' >>"$M/00001_a.sql"
commit_all 'modify'
red 'modify, committed'

reset
git mv "$M/00001_a.sql" "$M/00001_b.sql"
red 'rename, working tree'

reset
git mv "$M/00001_a.sql" "$M/00001_b.sql"
commit_all 'rename'
red 'rename, committed'

reset
rm "$M/00001_a.sql"
red 'delete, working tree'

reset
git rm -q "$M/00001_a.sql"
commit_all 'delete'
red 'delete, committed'

reset
rm "$M/00001_a.sql"
ln -s elsewhere "$M/00001_a.sql"
git add "$M/00001_a.sql"
red 'type change to a link, staged'

reset
mig 00002_b.sql "$B_BODY"
git add "$M/00002_b.sql"
green 'add, staged'

reset
mig 00002_b.sql "$B_BODY"
commit_all 'add'
green 'add, committed'

reset
mig 00002_b.sql "$B_BODY"
printf -- '-- edited\n' >>"$M/00001_a.sql"
commit_all 'add and modify'
red 'add together with a modify'

# An unusable target ref is exit 2, never a pass.
reset
CONTROLS=$((CONTROLS + 1))
if sh "$GUARD" no-such-ref >/dev/null 2>&1; then _bs=0; else _bs=$?; fi
if [ "$_bs" = 2 ]; then
	printf 'RED unknown target ref: exit 2\n'
else
	FAILED=$((FAILED + 1))
	printf 'FAIL unknown target ref: want exit 2, got %s\n' "$_bs"
fi

# --- squash merge: the target got the migration as one new commit with no ancestry link --
# A merge-base comparison sees the migration as an addition on the feature branch
# whatever happens to it afterwards; the target's own tree does not.
newrepo squash
git checkout -q -b feature
mig 00001_a.sql "$A_BODY"
commit_all 'feature adds the migration'
FEATURE_TIP=$(git rev-parse HEAD)
git checkout -q main
git merge -q --squash feature
commit_all 'squash of the feature'
git checkout -q feature

reset_squash() {
	git checkout -q -f feature
	git reset -q --hard "$FEATURE_TIP"
	git clean -fdq
}

reset_squash
green 'squash-merged, left as merged'

reset_squash
printf -- '-- edited later\n' >>"$M/00001_a.sql"
commit_all 'edit after the squash'
red 'squash-merged, then modified'

reset_squash
git mv "$M/00001_a.sql" "$M/00001_renamed.sql"
commit_all 'rename after the squash'
red 'squash-merged, then renamed'

reset_squash
git rm -q "$M/00001_a.sql"
commit_all 'delete after the squash'
red 'squash-merged, then deleted'

# --- the target advanced after the fork -----------------------------------------------------
newrepo advanced
mig 00001_a.sql "$A_BODY"
commit_all 'merged migration'
git checkout -q -b feature
FORK=$(git rev-parse HEAD)
git checkout -q main
mig 00002_b.sql "$B_BODY"
commit_all 'target gains migration 2'
git checkout -q feature

reset() {
	git checkout -q -f feature
	git reset -q --hard "$FORK"
	git clean -fdq
}

reset
red 'branch behind the target: its migration is missing'

reset
mig 00002_b.sql "$B_OTHER"
commit_all 'feature adds migration 2 with other bytes'
red 'target advanced: same path, different bytes'

reset
mig 00002_b.sql "$B_BODY"
commit_all 'feature carries the same migration 2'
green 'target advanced: identical bytes at the same path'

reset
mig 00003_c.sql "$C_BODY"
commit_all 'feature adds its own migration 3'
red 'behind the target with its own addition'

git merge -q -m 'merge the target' main
green 'merged the target, own addition kept'

# --- unrelated history ---------------------------------------------------------------------
newrepo related
mig 00001_a.sql "$A_BODY"
commit_all 'merged migration'
git checkout -q --orphan unrelated
git rm -rfq .
printf 'other\n' >README
mkdir -p "$M"
mig 00001_a.sql "$A_BODY"
commit_all 'unrelated root with the same migration'
green 'unrelated history, identical migration'
printf -- '-- edited\n' >>"$M/00001_a.sql"
commit_all 'edit'
red 'unrelated history, modified migration'

newrepo contract
mkdir -p internal/api web/src/lib
printf '{"paths":{}}\n' >internal/api/openapi.json
printf 'types v1\n' >web/src/lib/api.gen.ts
printf 'client v1\n' >web/src/lib/api.ts
commit_all 'initial API contract'
printf '{"paths":{"/api/v1/setup":{}}}\n' >internal/api/openapi.json
CONTROLS=$((CONTROLS + 1))
_r=$(run_guard)
case $_r in
1\|*api_lockstep_required*) printf 'RED contract without consumer: %s\n' "${_r#*|}" ;;
*) FAILED=$((FAILED + 1)); printf 'FAIL contract without consumer: %s\n' "$_r" ;;
esac
printf 'types v2\n' >web/src/lib/api.gen.ts
printf 'client v2\n' >web/src/lib/api.ts
green 'contract and consumers change together'
git tag v0.1.0
rm internal/api/openapi.json
CONTROLS=$((CONTROLS + 1))
_r=$(run_guard)
case $_r in
1\|*api_document_missing*) printf 'RED published document removed: %s\n' "${_r#*|}" ;;
*) FAILED=$((FAILED + 1)); printf 'FAIL published document removed: %s\n' "$_r" ;;
esac

if [ "$FAILED" -ne 0 ]; then
	printf 'HISTORY-GATES-TEST-FAIL failed=%s controls=%s\n' "$FAILED" "$CONTROLS"
	exit 1
fi
printf 'HISTORY-GATES-TEST-PASS controls=%s\n' "$CONTROLS"
