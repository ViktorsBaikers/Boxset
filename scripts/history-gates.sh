#!/bin/sh
# History gate for the immutable migration chain (AC-017). Host script: it needs
# git and the target branch's objects, which the dev image excludes (.dockerignore).
#   sh scripts/history-gates.sh [TARGET_REF]      (TARGET_REF defaults to main)
# Every migration file the target branch has at its tip must exist in the
# candidate (the working tree: committed, staged or edited) at the same path
# with the same bytes. The comparison is against the target's own tree, not the
# merge base, so it holds however the histories relate: a migration squash-merged
# into the target and then changed, renamed or deleted on the feature branch, a
# target that advanced after the fork, and unrelated histories are all caught.
# A new file at a path the target does not have passes. Consequently a branch
# that lacks a migration the target gained must take it from the target first
# (a rebase or merge): that absence is reported as a deletion.
# Prints `migration_changed <status> <path>` for each offender (M modified,
# D missing, T type changed) and then `HISTORY-GATE-FAIL changed=N` (exit 1), else
# `HISTORY-GATE-PASS added=N` (exit 0). An unusable ref or repository is exit 2,
# never a pass. Only git object commands run; no checkout, fetch or ref update.
set -eu
LC_ALL=C
export LC_ALL

TARGET=${1:-main}
DIR=internal/store/migrations

TOP=$(git rev-parse --show-toplevel) || exit 2
cd "$TOP"
if ! git rev-parse --verify --quiet "$TARGET^{commit}" >/dev/null; then
	printf 'history-gates: target ref %s is not a commit\n' "$TARGET" >&2
	exit 2
fi
# Working tree against the target's tip tree. --no-renames keeps a rename as a
# deletion of the old path plus an addition of the new one.
CHANGES=$(git -c core.quotepath=false diff --name-status --no-renames "$TARGET" -- "$DIR") || exit 2

changed=0
added=0
TAB=$(printf '\t')
while IFS=$TAB read -r status path; do
	[ -n "$status" ] || continue
	if [ "$status" = A ]; then
		added=$((added + 1))
	else
		changed=$((changed + 1))
		printf 'migration_changed %s %s\n' "$status" "$path"
	fi
done <<EOF
$CHANGES
EOF

if [ "$changed" -ne 0 ]; then
	printf 'HISTORY-GATE-FAIL changed=%s\n' "$changed"
	exit 1
fi
printf 'HISTORY-GATE-PASS added=%s\n' "$added"

DOC=internal/api/openapi.json
TYPES=web/src/lib/api.gen.ts
CLIENT=web/src/lib/api.ts
changed_file() {
	_before=$(git rev-parse --verify "$TARGET:$1" 2>/dev/null) || _before=absent
	if [ -f "$1" ]; then _after=$(git hash-object -- "$1") || exit 2; else _after=absent; fi
	[ "$_before" != "$_after" ]
}

RELEASE=$(git tag --list 'v[0-9]*.[0-9]*.[0-9]*' --sort=-version:refname | head -n 1)
if [ -z "$RELEASE" ]; then
	if changed_file "$DOC"; then
		if changed_file "$TYPES" && changed_file "$CLIENT"; then :; else
			printf 'api_lockstep_required: update the document, generated types and SPA client together\n'
			exit 1
		fi
	fi
else
	if [ ! -f "$DOC" ]; then
		printf 'api_document_missing: the published API document must remain present\n'
		exit 1
	fi
	BASELINE=$(git show "$RELEASE:$DOC") || exit 2
	scripts/dev-run.sh -- env CGO_ENABLED=0 go test ./internal/api -run '^TestOpenAPIReleaseBaseline$' -args -openapi-baseline "$BASELINE" || exit 1
fi
printf 'HISTORY-GATES-PASS\n'
