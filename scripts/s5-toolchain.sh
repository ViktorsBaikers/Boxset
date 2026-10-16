#!/bin/sh
# S5 toolchain admission, run inside the work image through scripts/dev-run.sh:
#   scripts/dev-run.sh --write docs/spikes/issue-drafts -- scripts/s5-toolchain.sh --admission
# It cross-checks every row of docs/spikes/s5-version-admission.md against the
# pins in deploy/dev/Dockerfile, web/package.json, web/bun.lock and go.mod and
# against the installed tools, applies the ADR 0011 maintenance rule of each
# row's category, then executes real controls (Go with cgo, Go cross builds under
# QEMU, Bun, the linter with a planted defect, the headless-shell browser, the
# capture validator, environment and exposure checks). `S5-ADMISSION-PASS` is
# printed only when nothing is blocked or unlisted and those controls ran at the
# real root /src; every failing run keeps its
# exit status 1 and writes a bounded owner-reviewable draft to
# /src/docs/spikes/issue-drafts/s5-admission.md inside the container; the runner
# publishes it redacted and capped after the container stopped. `--root DIR`
# names the root by its physical path (so /src/. is /src); any other root is a
# manifest-only copy: no controls, no draft and the distinct final line
# `S5-MANIFEST-ONLY-PASS`, never admission. `--facts` prints
# the installed per-package facts a row's licence cell needs; it admits nothing.
# The support-list block is immutable report data; this script never fetches.
set -u
set -f
LC_ALL=C
export LC_ALL

usage() {
	printf '%s\n' 'usage: s5-toolchain.sh (--admission|--facts) [--root DIR]' >&2
	exit 2
}

MODE=
ROOT=/src
while [ $# -gt 0 ]; do
	case $1 in
	--admission) MODE=admission ;;
	--facts) MODE=facts ;;
	--tools)
		# Owned by the SLICE that runs the eight web tools; never a success stub.
		printf '%s\n' 'S5-TOOLS-UNAVAILABLE: the eight-tool Bun execution is not part of toolchain admission' >&2
		exit 2
		;;
	--root)
		shift
		ROOT=${1:-}
		[ -n "$ROOT" ] || usage
		;;
	*) usage ;;
	esac
	shift
done
[ -n "$MODE" ] || usage
# The root is its physical path, so an alias of /src is the real root.
_r=$(cd -- "$ROOT" 2>/dev/null && pwd -P) && ROOT=$_r

DOCKERFILE=$ROOT/deploy/dev/Dockerfile
REPORT=$ROOT/docs/spikes/s5-version-admission.md
PKG=$ROOT/web/package.json
LOCK=$ROOT/web/bun.lock
GOMOD=$ROOT/go.mod
BROWSERS=${PLAYWRIGHT_BROWSERS_PATH:-/ms-playwright}
CHECKER=/usr/local/libexec/boxset/capture-check
FETCH_COPYRIGHT=/usr/local/share/boxset/evidence/apt-fetch/unzip.copyright
DRAFT=
[ "$ROOT" != /src ] || DRAFT=/src/docs/spikes/issue-drafts/s5-admission.md

BLOCKED=0
UNLISTED=0
ROWS=0

T=$(mktemp -d) || exit 125
trap 'rm -rf "$T"' EXIT
trap 'exit 125' HUP INT TERM
: >"$T/draft"

block() {
	printf 'ADMISSION-BLOCKED %s: %s\n' "$1" "$2"
	printf 'ADMISSION-BLOCKED %s: %s\n' "$1" "$2" >>"$T/draft"
	BLOCKED=$((BLOCKED + 1))
}
unlisted() {
	printf 'ADMISSION-UNLISTED %s %s\n' "$1" "$2"
	printf 'ADMISSION-UNLISTED %s %s\n' "$1" "$2" >>"$T/draft"
	UNLISTED=$((UNLISTED + 1))
}
ok() { printf 'CONTROL %s ok\n' "$1"; }

# fail_out: every failing admission exits 1 and retains a bounded draft at the
# fixed draft name; a write failure is reported and never swallowed.
fail_out() {
	printf 'S5-ADMISSION-FAIL blocked=%s unlisted=%s\n' "$BLOCKED" "$UNLISTED"
	if [ -n "$DRAFT" ]; then
		{
			printf '# S5 admission failed\n\n'
			printf 'blocked=%s unlisted=%s\n\n' "$BLOCKED" "$UNLISTED"
			head -c 60000 "$T/draft"
			printf '\nOwner review: this draft is never filed by agents; stack, pin or policy replacement needs the owner.\n'
		} >"$T/draft.out"
		if ! { mkdir -p "${DRAFT%/*}" && cat "$T/draft.out" >"$DRAFT"; } 2>/dev/null; then
			printf 'S5-DRAFT-WRITE-FAILED name=s5-admission.md\n'
		fi
	fi
	exit 1
}

# ver_ge A B: dotted numeric A >= B (missing parts are 0).
ver_ge() {
	_a=$1
	_b=$2
	while :; do
		_ah=${_a%%.*}
		_bh=${_b%%.*}
		[ -n "$_ah" ] || _ah=0
		[ -n "$_bh" ] || _bh=0
		[ "$_ah" -gt "$_bh" ] && return 0
		[ "$_ah" -lt "$_bh" ] && return 1
		case $_a in *.*) _a=${_a#*.} ;; *) _a= ;; esac
		case $_b in *.*) _b=${_b#*.} ;; *) _b= ;; esac
		[ -z "$_a$_b" ] && return 0
	done
}

TODAY=$(date -u +%F)

# valid_date YYYY-MM-DD: a real calendar date.
valid_date() {
	case $1 in [0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]) ;; *) return 1 ;; esac
	[ "$(date -u -d "$1" +%F 2>/dev/null)" = "$1" ]
}
dsec() { date -u -d "$1" +%s; }
# age_days DATE: whole days from DATE to the run date.
age_days() { printf '%s' "$((($(dsec "$TODAY") - $(dsec "$1")) / 86400))"; }

# --- facts ---------------------------------------------------------------------------
# copyright_file NAME KIND: the licence evidence file, resolved inside /usr/share/doc
# (or the retained unzip copyright file for the build-only apt-fetch row).
copyright_file() {
	if [ "$2" = apt-fetch ]; then
		[ -f "$FETCH_COPYRIGHT" ] && printf '%s' "$FETCH_COPYRIGHT"
		return
	fi
	_cf=$(readlink -f "/usr/share/doc/$1/copyright" 2>/dev/null) || return 1
	case $_cf in /usr/share/doc/*) ;; *) return 1 ;; esac
	[ -f "$_cf" ] && printf '%s' "$_cf"
}

# dep5_set FILE: the sorted, comma-joined set of the file's DEP-5 License: values
# (whitespace runs become _), or none.
dep5_set() {
	_ds=$(grep -h '^License:' "$1" | sed 's/^License:[[:space:]]*//; s/[[:space:]]*$//; s/[[:space:]][[:space:]]*/_/g' | LC_ALL=C sort -u | tr '\n' ',')
	_ds=${_ds%,}
	printf '%s' "${_ds:-none}"
}

OSREL=$(. /etc/os-release && printf '%s %s %s' "${ID:-}" "${VERSION_ID:-}" "${VERSION_CODENAME:-}")
OS_ID=${OSREL%% *}
_rest=${OSREL#* }
OS_VERSION=${_rest%% *}
OS_CODENAME=${_rest#* }

if [ "$MODE" = facts ]; then
	grep '^| ADM-' "$REPORT" | while IFS='|' read -r _ id kind category name version _; do
		id=${id# }
		id=${id% }
		kind=${kind# }
		kind=${kind% }
		name=${name# }
		name=${name% }
		case $kind in apt | apt-fetch) ;; *) continue ;; esac
		if [ "$kind" = apt ]; then
			info=$(dpkg-query -W -f='${Version}|${Section}|${source:Package}|${source:Version}' "$name" 2>/dev/null) || {
				printf 'FACT %s unavailable\n' "$id"
				continue
			}
		else
			info="-|-|$name|-"
		fi
		cf=$(copyright_file "$name" "$kind") || {
			printf 'FACT %s no-copyright-file\n' "$id"
			continue
		}
		set -- $(sha256sum "$cf")
		printf 'FACT %s installed=%s section=%s source=%s source-version=%s copyright-sha256=%s dep5=%s\n' \
			"$id" "${info%%|*}" "$(printf '%s' "$info" | cut -d'|' -f2)" "$(printf '%s' "$info" | cut -d'|' -f3)" \
			"$(printf '%s' "$info" | cut -d'|' -f4)" "$1" "$(dep5_set "$cf")"
	done
	exit 0
fi

for f in "$DOCKERFILE" "$REPORT" "$PKG" "$LOCK" "$GOMOD"; do
	[ -s "$f" ] || block "inputs" "missing or empty $f"
done
[ "$BLOCKED" -eq 0 ] || fail_out

SNAPSHOT=$(grep '^ARG DEBIAN_SNAPSHOT=' "$DOCKERFILE") && SNAPSHOT=${SNAPSHOT#ARG DEBIAN_SNAPSHOT=}
GO_V=
BUN_V=
LINT_V=
CHROME_SHELL_V=

grep '^| ADM-' "$REPORT" >"$T/rows"
grep '^| CMT-' "$REPORT" >"$T/cmts"

# --- support-list block (immutable report data) --------------------------------------------
# SUPPORT_BAD is empty when the single block is valid, else the reason; a bad
# block makes every OS row evidence_invalid.
SUPPORT_BAD=
SUPPORT_COMMIT=
: >"$T/support.entries"
parse_support() {
	_n=$(grep -c '^support-list ' "$REPORT")
	[ "$_n" = 1 ] || { SUPPORT_BAD="support-list blocks: $_n, want 1"; return; }
	_ln=$(grep -n '^support-list ' "$REPORT")
	_ln=${_ln%%:*}
	_head=$(sed -n "${_ln}p" "$REPORT")
	printf '%s\n' "$_head" | grep -Eq '^support-list release=[0-9]+ ref=master commit=[0-9a-f]{40} sha256=[0-9a-f]{64} url=https://salsa\.debian\.org/debian/debian-security-support/-/raw/[0-9a-f]{40}/security-support\.deb[0-9]+$' ||
		{ SUPPORT_BAD="support-list header malformed"; return; }
	set -- $_head
	_rel=${2#release=}
	_commit=${4#commit=}
	_sha=${5#sha256=}
	_url=${6#url=}
	[ "$_rel" = "$OS_VERSION" ] || { SUPPORT_BAD="support-list release $_rel is not the image VERSION_ID $OS_VERSION"; return; }
	case $_url in
	"https://salsa.debian.org/debian/debian-security-support/-/raw/$_commit/security-support.deb$_rel") ;;
	*) SUPPORT_BAD="support-list url does not name the block's commit and release"; return ;;
	esac
	_fence=$(sed -n "$((_ln + 1))p" "$REPORT")
	[ "$_fence" = "\`\`\`security-support.deb$_rel" ] || { SUPPORT_BAD="support-list fence does not open with security-support.deb$_rel"; return; }
	_end=$(tail -n +"$((_ln + 2))" "$REPORT" | grep -n -m 1 -x '```')
	[ -n "$_end" ] || { SUPPORT_BAD="support-list fence never closes"; return; }
	_end=$((_ln + 1 + ${_end%%:*}))
	if [ "$_end" -le "$((_ln + 2))" ]; then SUPPORT_BAD="support-list block is empty"; return; fi
	sed -n "$((_ln + 2)),$((_end - 1))p" "$REPORT" >"$T/support.file"
	_bytes=$(wc -c <"$T/support.file")
	[ "$_bytes" -le 65536 ] || { SUPPORT_BAD="support-list block exceeds 65536 bytes"; return; }
	set -- $(sha256sum "$T/support.file")
	[ "$1" = "$_sha" ] || { SUPPORT_BAD="support-list sha256 does not match the fenced bytes"; return; }
	SUPPORT_COMMIT=$_commit
	while IFS= read -r _l; do
		case $_l in '' | '#'*) continue ;; esac
		set -- $_l
		[ $# -ge 2 ] || { SUPPORT_BAD="support-list line has no status"; return; }
		_pat=$1
		printf '' | LC_ALL=C grep -P -x -e "$_pat" >/dev/null 2>&1
		[ $? -le 1 ] || { SUPPORT_BAD="support-list pattern is not valid for grep -P"; return; }
		case $2 in
		supported | limited) printf '%s %s\n' "$_pat" "$2" >>"$T/support.entries" ;;
		non-supported)
			[ $# -ge 4 ] || { SUPPORT_BAD="non-supported line lacks version or date"; return; }
			printf '%s' "$3" | grep -Eq '^([0-9]+:)?[0-9][A-Za-z0-9.+~-]*$' || { SUPPORT_BAD="non-supported version is not a Debian version"; return; }
			valid_date "$4" || { SUPPORT_BAD="non-supported date is not a valid YYYY-MM-DD"; return; }
			printf '%s non-supported %s %s\n' "$_pat" "$3" "$4" >>"$T/support.entries"
			;;
		*) SUPPORT_BAD="support-list status word $2 is unknown"; return ;;
		esac
	done <"$T/support.file"
}
parse_support

# pcre_match PATTERN: stdin is one installed source package name; a full-string
# PCRE match. Status 0 match, 1 no match, anything above 1 an evaluation error.
pcre_match() { LC_ALL=C grep -P -x -q -e "$1" 2>/dev/null; }

# support_eval SOURCE: sets SSTAT to supported, clear, ends-DATE, distro_unsupported
# or evaluation_error with upstream check-support-status semantics: each pattern is
# a full-string PCRE match against the installed source package name. A matcher
# error is never a nonmatch: it leaves SSTAT evaluation_error, never clear.
SSTAT=
support_eval() {
	_sup=0
	_lim=0
	_ended=0
	_ends=
	while read -r _pat _st _ver _dt; do
		printf '%s\n' "$1" | pcre_match "$_pat"
		_rc=$?
		[ "$_rc" -le 1 ] || { SSTAT=evaluation_error; return; }
		[ "$_rc" = 0 ] || continue
		case $_st in
		supported) _sup=1 ;;
		limited) _lim=1 ;;
		non-supported)
			if [ "$(dsec "$_dt")" -le "$(dsec "$TODAY")" ]; then
				_ended=1
			elif [ -z "$_ends" ] || [ "$(dsec "$_dt")" -lt "$(dsec "$_ends")" ]; then
				_ends=$_dt
			fi
			;;
		esac
	done <"$T/support.entries"
	if [ "$_sup" = 1 ]; then
		SSTAT=supported
	elif [ "$_lim" = 1 ] || [ "$_ended" = 1 ]; then
		SSTAT=distro_unsupported
	elif [ -n "$_ends" ]; then
		SSTAT=ends-$_ends
	else
		SSTAT=clear
	fi
}

# Dockerfile apt suites: only trixie, trixie-updates and trixie-security (main).
SUITES_OK=1
suites=$(grep -o 'Suites: [a-z0-9 -]*' "$DOCKERFILE" | sed 's/^Suites: //' | tr ' ' '\n' | grep -v '^$' | LC_ALL=C sort -u | tr '\n' ' ')
[ "$suites" = 'trixie trixie-security trixie-updates ' ] || SUITES_OK=0
[ "$(grep -o 'Components: [a-z0-9 -]*' "$DOCKERFILE" | grep -cvx 'Components: main')" = 0 ] || SUITES_OK=0

# --- direct dependencies of web/package.json, parsed structurally by the admitted Bun ---
# enum_deps PACKAGE_JSON: name<TAB>version over dependencies, devDependencies,
# optionalDependencies and peerDependencies, independent of formatting.
enum_deps() {
	S5_PKG=$1 bun -e 'const p=JSON.parse(require("fs").readFileSync(process.env.S5_PKG,"utf8"));for(const s of ["dependencies","devDependencies","optionalDependencies","peerDependencies"])for(const [k,v] of Object.entries(p[s]||{}))console.log(k+"\t"+v)'
}
enum_deps "$PKG" >"$T/deps" 2>"$T/deps.err" || block bun "cannot enumerate web/package.json dependencies: $(head -c 300 "$T/deps.err")"

# --- inventory rows ---------------------------------------------------------------------
# Row cells: ID | kind | category | name | version | pin | source | publisher | licence | maintenance | targets | bun
while IFS='|' read -r _ id kind category name version pin source publisher licence maint targets bun _; do
	id=${id# }
	id=${id% }
	kind=${kind# }
	kind=${kind% }
	category=${category# }
	category=${category% }
	name=${name# }
	name=${name% }
	version=${version# }
	version=${version% }
	pin=${pin# }
	pin=${pin% }
	source=${source# }
	source=${source% }
	publisher=${publisher# }
	publisher=${publisher% }
	licence=${licence# }
	licence=${licence% }
	maint=${maint# }
	maint=${maint% }
	targets=${targets# }
	targets=${targets% }
	bun=${bun# }
	bun=${bun% }
	ROWS=$((ROWS + 1))

	if [ "$(grep -c "^| $id |" "$T/rows")" != 1 ]; then block "$id" "duplicate id"; fi
	if [ "$(grep -c "^| ADM-[0-9]* | $kind | [a-z-]* | $name |" "$T/rows")" != 1 ]; then block "$id" "duplicate $kind $name"; fi
	_empty=0
	for field in "$category" "$name" "$version" "$pin" "$source" "$publisher" "$licence" "$maint" "$targets" "$bun"; do
		[ -n "$field" ] || _empty=1
	done
	[ "$_empty" = 0 ] || block "$id" "empty column"
	case $licence in
	UNKNOWN* | UNLICENSED* | *"SEE LICENSE"*) block "$id" "unapproved licence $licence" ;;
	esac

	# installed facts for apt rows
	section=
	srcpkg=
	srcver=
	instver=
	if [ "$kind" = apt ]; then
		if got=$(dpkg-query -W -f='${Version}|${Section}|${source:Package}|${source:Version}' "$name" 2>/dev/null); then
			instver=${got%%|*}
			_r=${got#*|}
			section=${_r%%|*}
			_r=${_r#*|}
			srcpkg=${_r%%|*}
			srcver=${_r#*|}
			[ -n "$srcpkg" ] || srcpkg=$name
		fi
	fi

	# --- category (ADR 0011): only these kinds and names
	cat_ok=1
	case $category in
	base-image) [ "$kind" = image ] || cat_ok=0 ;;
	archive-snapshot) [ "$kind" = snapshot ] || cat_ok=0 ;;
	direct-tool)
		case $kind:$name in
		archive:go | archive:bun | archive:golangci-lint | archive:chrome-headless-shell | apt:gcc | apt:libc6-dev | apt:qemu-user | apt-fetch:unzip) ;;
		*) cat_ok=0 ;;
		esac
		;;
	go-dependency) [ "$kind" = go ] || cat_ok=0 ;;
	npm-dependency) [ "$kind" = npm ] || cat_ok=0 ;;
	os-library) { [ "$kind" = apt ] && [ "$section" = libs ]; } || cat_ok=0 ;;
	os-font) { [ "$kind" = apt ] && [ "$section" = fonts ]; } || cat_ok=0 ;;
	os-data)
		{ [ "$kind" = apt ] && case $name in ca-certificates | tzdata) true ;; *) false ;; esac; } || cat_ok=0
		;;
	*) cat_ok=0 ;;
	esac
	if [ "$cat_ok" = 0 ]; then
		block "$id" "category_invalid $category for $kind $name"
		continue
	fi

	# --- exact pin syntax
	case $version in
	'' | latest | next | *'^'* | *'*'* | *' '* | *'>'* | *'<'* | *'='*) block "$id" "floating version $version" ;;
	esac
	case $kind in
	apt | apt-fetch)
		printf '%s' "$version" | grep -Eq '^([0-9]+:)?[0-9][A-Za-z0-9.+~-]*$' || block "$id" "floating version $version is not an exact Debian version"
		;;
	esac

	# --- per-kind pin verification
	case $kind in
	image)
		grep -Fq "FROM $name:$version@$pin AS " "$DOCKERFILE" || block "$id" "Dockerfile does not pin $name:$version@$pin"
		;;
	snapshot)
		[ "$pin" = "$version" ] || block "$id" "snapshot pin differs from version"
		grep -Fxq "ARG DEBIAN_SNAPSHOT=$version" "$DOCKERFILE" || block "$id" "Dockerfile snapshot differs"
		;;
	archive)
		up=$(printf '%s' "$name" | tr 'a-z-' 'A-Z_')
		grep -Fxq "ARG ${up}_VERSION=$version" "$DOCKERFILE" || block "$id" "Dockerfile ${up}_VERSION differs from $version"
		grep -Fxq "ARG ${up}_SHA256=$pin" "$DOCKERFILE" || block "$id" "Dockerfile ${up}_SHA256 differs from $pin"
		case $pin in
		*[!0-9a-f]* | '') block "$id" "pin is not a lowercase SHA-256" ;;
		*) [ "${#pin}" -eq 64 ] || block "$id" "pin is not a SHA-256" ;;
		esac
		case $version in *[!0-9.]* | .* | *..* | *.) block "$id" "archive version $version is not exact dotted" ;; esac
		case $name in
		go) GO_V=$version ;;
		bun) BUN_V=$version ;;
		golangci-lint) LINT_V=$version ;;
		chrome-headless-shell) CHROME_SHELL_V=$version ;;
		esac
		;;
	apt | apt-fetch)
		[ "$pin" = "snapshot:$SNAPSHOT" ] || block "$id" "pin $pin is not snapshot:$SNAPSHOT"
		grep -Fxq "    $name=$version \\" "$DOCKERFILE" || block "$id" "Dockerfile does not pin $name=$version"
		if [ "$kind" = apt ]; then
			if [ -n "$instver" ]; then
				[ "$instver" = "$version" ] || block "$id" "installed $name is $instver, not $version"
				case $section in
				non-free* | contrib*) block "$id" "$name is outside Debian main" ;;
				esac
			else
				block "$id" "$name is not installed"
			fi
		else
			dpkg-query -W "$name" >/dev/null 2>&1 && block "$id" "$name must be absent from the dev image"
		fi
		# source_mismatch: debian-source <package> <version> <snapshot URL>
		set -- $source
		if [ "$kind" = apt ]; then
			{ [ $# = 4 ] && [ "$1" = debian-source ] && [ "$2" = "$srcpkg" ] && [ "$3" = "$srcver" ] && [ "$4" = "http://snapshot.debian.org/archive/debian/$SNAPSHOT/" ]; } ||
				block "$id" "source_mismatch: source cell '$source' is not debian-source $srcpkg $srcver http://snapshot.debian.org/archive/debian/$SNAPSHOT/"
		else
			{ [ $# = 4 ] && [ "$1" = debian-source ] && [ "$2" = "$name" ] && [ "$3" = "$version" ] && [ "$4" = "http://snapshot.debian.org/archive/debian/$SNAPSHOT/" ]; } ||
				block "$id" "source_mismatch: source cell '$source' is not debian-source $name $version http://snapshot.debian.org/archive/debian/$SNAPSHOT/"
		fi
		# licence_mismatch: copyright-sha256=<hex> recomputed from the installed file and the DEP-5 License: set
		want_sha=
		want_dep5=
		for tok in $licence; do
			case $tok in
			copyright-sha256=*) want_sha=${tok#copyright-sha256=} ;;
			dep5=*) want_dep5=${tok#dep5=} ;;
			esac
		done
		if cf=$(copyright_file "$name" "$kind"); then
			set -- $(sha256sum "$cf")
			got_sha=$1
			got_dep5=$(dep5_set "$cf")
			if [ -z "$want_sha" ] || [ -z "$want_dep5" ]; then
				block "$id" "licence_mismatch: licence cell lacks copyright-sha256= or dep5= (installed copyright-sha256=$got_sha dep5=$got_dep5)"
			elif [ "$want_sha" != "$got_sha" ] || [ "$want_dep5" != "$got_dep5" ]; then
				block "$id" "licence_mismatch: copyright-sha256=$want_sha dep5=$want_dep5, installed $got_sha $got_dep5"
			fi
		else
			block "$id" "licence_mismatch: no copyright file inside /usr/share/doc for $name"
		fi
		;;
	npm)
		case $version in
		*[!0-9.]* | '' | .* | *..* | *.) block "$id" "npm version $version is not exact" ;;
		esac
		case $pin in sha512-*) ;; *) block "$id" "pin is not an npm sha512 integrity" ;; esac
		printf '%s\t%s\n' "$name" "$version" | grep -Fxqf - "$T/deps" 2>/dev/null || block "$id" "web/package.json does not pin $name $version exactly"
		if ! grep -F "\"$name@$version\"" "$LOCK" | grep -Fq "$pin"; then
			block "$id" "web/bun.lock lacks $name@$version with the recorded integrity"
		fi
		[ "$bun" = pending-SLICE-002 ] || block "$id" "Bun compatibility must stay pending-SLICE-002 until executed ($bun)"
		if [ "$ROOT" = /src ]; then
			grep -Eq "\"version\": ?\"$version\"" "/src/web/node_modules/$name/package.json" 2>/dev/null ||
				block "$id" "installed node_modules/$name is not $version"
		fi
		;;
	*) block "$id" "unknown kind $kind" ;;
	esac

	# --- maintenance evidence by category (ADR 0011)
	case $category in
	archive-snapshot)
		set -- $maint
		if [ $# = 3 ] && [ "$1" = identity ] && [ "$2" = "$version" ] && case $3 in http://* | https://*) true ;; *) false ;; esac; then
			:
		else
			block "$id" "evidence_invalid: snapshot rows need 'identity <timestamp> <URL>' (provenance only)"
		fi
		;;
	os-library | os-font | os-data)
		set -- $maint
		if [ "${1:-}" != distro-support ]; then
			block "$id" "evidence_invalid: OS rows need distro-support evidence, not '${maint%% *}'"
		elif [ -n "$SUPPORT_BAD" ]; then
			block "$id" "evidence_invalid: $SUPPORT_BAD"
		elif [ $# != 9 ]; then
			block "$id" "evidence_invalid: distro-support cell has $# words, want 9"
		else
			ev_dist=$2
			ev_ver=$3
			ev_code=$4
			ev_until=${5#security-until=}
			ev_url=$6
			ev_tracker=${7#tracker=}
			ev_lists=${8#lists=}
			ev_status=${9#status=}
			bad_ev=
			[ "$5" != "$ev_until" ] && [ "$7" != "$ev_tracker" ] && [ "$8" != "$ev_lists" ] && [ "$9" != "$ev_status" ] || bad_ev="a distro-support key is missing"
			valid_date "$ev_until" || bad_ev="security-until is not a valid date"
			case $ev_url in https://*) ;; *) bad_ev="release URL is not https" ;; esac
			[ "$ev_tracker" = "https://security-tracker.debian.org/tracker/source-package/$srcpkg" ] || bad_ev="tracker does not name installed source package $srcpkg"
			[ "$ev_lists" = "$SUPPORT_COMMIT" ] || bad_ev="lists= is not the support-list block commit"
			case $ev_status in clear | supported | ends-[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]) ;; *) bad_ev="status is not clear, supported or ends-DATE" ;; esac
			support_eval "$srcpkg"
			if [ -n "$bad_ev" ]; then
				block "$id" "evidence_invalid: $bad_ev"
			elif [ "$SSTAT" = evaluation_error ]; then
				block "$id" "evidence_invalid: support-list pattern evaluation failed for $srcpkg"
			elif [ "$SUITES_OK" = 0 ] || [ "$ev_dist" != "$OS_ID" ] || [ "$ev_ver" != "$OS_VERSION" ] || [ "$ev_code" != "$OS_CODENAME" ]; then
				block "$id" "distro_unsupported: evidence is for $ev_dist $ev_ver $ev_code but the image is $OS_ID $OS_VERSION $OS_CODENAME with apt suites ok=$SUITES_OK"
			elif [ "$(dsec "$ev_until")" -lt "$(dsec "$TODAY")" ]; then
				block "$id" "distro_unsupported: security support ended $ev_until"
			elif [ "$SSTAT" = distro_unsupported ]; then
				block "$id" "distro_unsupported: debian-security-support lists $srcpkg as limited or ended"
			elif [ "$ev_status" != "$SSTAT" ]; then
				block "$id" "evidence_invalid: status=$ev_status but the support-list block evaluates $srcpkg as $SSTAT"
			fi
		fi
		;;
	*)
		# base-image, direct-tool, go-dependency, npm-dependency: annual release/upload/commit evidence
		set -- $maint
		if [ $# != 5 ]; then
			block "$id" "evidence_invalid: maintenance cell has $# words, want '<release|upload|commit> <id> <YYYY-MM-DD> <https URL> work=<subject>/<version>'"
		else
			ev_type=$1
			ev_id=$2
			ev_date=$3
			ev_url=$4
			ev_work=${5#work=}
			bad_ev=
			case $ev_type in release | upload | commit) ;; *) bad_ev="type $ev_type is not release, upload or commit" ;; esac
			case $maint in *snapshot.debian.org* | *snapshot:* | *"$SNAPSHOT"*) bad_ev="a snapshot URL, identifier or date is never maintenance evidence" ;; esac
			case $ev_id in '' | *[!A-Za-z0-9:._+~=/-]*) bad_ev="immutable id is empty or has unexpected characters" ;; esac
			case $ev_url in https://?*) ;; *) bad_ev="URL is not https" ;; esac
			[ "$5" != "$ev_work" ] || bad_ev="work= is missing"
			valid_date "$ev_date" || bad_ev="date is not a valid YYYY-MM-DD"
			subj=${ev_work%/*}
			wver=${ev_work##*/}
			{ [ -n "$subj" ] && [ "$subj" != "$ev_work" ] && [ -n "$wver" ]; } || bad_ev="work= is not <subject>/<version>"
			if [ "$kind" = apt ] || [ "$kind" = apt-fetch ]; then
				set -- $source
				want_subj=$2
			else
				want_subj=$name
			fi
			[ "$subj" = "$want_subj" ] || bad_ev="work subject $subj is not $want_subj"
			if [ -n "$bad_ev" ]; then
				block "$id" "evidence_invalid: $bad_ev"
			else
				age=$(age_days "$ev_date")
				if [ "$age" -lt 0 ]; then
					block "$id" "evidence_invalid: evidence dated $ev_date is after the run date $TODAY"
				elif [ "$age" -gt 365 ]; then
					block "$id" "maintenance_stale: $ev_type dated $ev_date is $age days before $TODAY"
				fi
			fi
		fi
		;;
	esac
done <"$T/rows"
[ "$ROWS" -gt 0 ] || block inventory "no inventory rows"

# --- unlisted direct inputs -------------------------------------------------------------
while IFS="$(printf '\t')" read -r pkg ver; do
	[ -n "$pkg" ] || continue
	grep -Fq "| npm-dependency | $pkg | $ver |" "$T/rows" || unlisted npm "$pkg@$ver"
done <"$T/deps"
grep -E '^    [a-z0-9][a-z0-9.+-]*=[^ "$;]+ \\$' "$DOCKERFILE" >"$T/apt"
while IFS= read -r line; do
	line=${line#    }
	line=${line% \\}
	pkg=${line%%=*}
	ver=${line#*=}
	grep -Fq "| $pkg | $ver |" "$T/rows" || unlisted apt "$pkg=$ver"
done <"$T/apt"
adds=$(grep -c '^ADD ' "$DOCKERFILE")
sums=$(grep -c '^ADD --checksum=sha256:' "$DOCKERFILE")
archives=$(grep -c '^| ADM-[0-9]* | archive |' "$T/rows")
[ "$adds" = "$sums" ] || unlisted dockerfile "ADD without --checksum"
[ "$adds" = "$archives" ] || unlisted dockerfile "$adds ADD instructions but $archives archive rows"
grep '^FROM ' "$DOCKERFILE" >"$T/froms"
while IFS= read -r line; do
	ref=${line#FROM }
	ref=${ref%% *}
	case $ref in
	base | fetch | dev | lock | capture-check-build | redact-build | work | scratch) ;;
	*)
		img=${ref%%:*}
		iver=${ref#*:}
		ipin=${iver#*@}
		iver=${iver%%@*}
		grep -Fq "| image | base-image | $img | $iver | $ipin |" "$T/rows" || unlisted image "$ref"
		;;
	esac
done <"$T/froms"
if grep -Eq '^(require|replace|toolchain)|^[[:space:]]' "$GOMOD"; then unlisted go.mod "dependency or toolchain line"; fi
GODIR=$(grep '^go ' "$GOMOD") && GODIR=${GODIR#go }
[ -n "${GODIR:-}" ] || block go.mod "no go directive"
if grep -Eq 'npm i|npm ci|yarn |pnpm |npx |nodejs|apt-get install[^&]*nodejs' "$DOCKERFILE"; then
	unlisted dockerfile "Node or alternative package manager"
fi
if grep -Eq 'chrome-linux64|chromium-\$\{|chrome-for-testing-public/[^ ]*/chrome-linux64' "$DOCKERFILE"; then
	unlisted dockerfile "full Chrome archive"
fi
if grep -Eq '"(workspaces|overrides|resolutions|scripts)"' "$PKG"; then block web/package.json "unexpected workspaces/overrides/scripts"; fi

# --- commitments ------------------------------------------------------------------------
cmt() {
	_row=$(grep "^| $1 |" "$T/cmts") || {
		block "$1" "commitment row missing"
		return 1
	}
	CMT_TEXT=$_row
}
if cmt CMT-TARGETS; then
	for t in linux/amd64 linux/arm64 linux/arm windows/amd64 windows/arm64 darwin/amd64 freebsd/amd64 freebsd/arm64; do
		case " $CMT_TEXT " in *" $t "*) ;; *) block CMT-TARGETS "target $t missing" ;; esac
		go tool dist list | grep -Fxq "$t" || block CMT-TARGETS "Go toolchain does not list $t"
	done
fi
if cmt CMT-PLATFORMS; then
	for p in linux/amd64 linux/arm64 linux/arm/v7; do
		case $CMT_TEXT in *"$p"*) ;; *) block CMT-PLATFORMS "platform $p missing" ;; esac
	done
fi
if cmt CMT-IMAGES; then
	for s in dev lock work; do
		case $CMT_TEXT in *"$s"*) ;; *) block CMT-IMAGES "image $s missing" ;; esac
		grep -Eq "^FROM .* AS $s\$" "$DOCKERFILE" || block CMT-IMAGES "Dockerfile has no stage $s"
	done
fi
if cmt CMT-P07; then case $CMT_TEXT in *openapi-fetch*) ;; *) block CMT-P07 "openapi-fetch exception not recorded" ;; esac; fi
if cmt CMT-BUN; then case $CMT_TEXT in *SLICE-002*) ;; *) block CMT-BUN "Bun compatibility not recorded as pending SLICE-002" ;; esac; fi
cmt CMT-REGEXP2 || true
cmt CMT-GOMOD || true

# A manifest that already blocks or lists something unlisted fails now; the real
# controls (and the environment/exposure checks, which only mean something inside
# the work image at /src) run for the real root only. Any other --root is a
# manifest-only inspection of a copy and never prints the admission PASS line.
if [ "$BLOCKED" -ne 0 ] || [ "$UNLISTED" -ne 0 ]; then fail_out; fi
if [ "$ROOT" = /src ]; then
# --- real controls ----------------------------------------------------------------------
export GOCACHE=${GOCACHE:-/tmp/gocache}
mkdir -p "$T/ctl" "$T/lint"
cat >"$T/ctl/main.go" <<'GO'
package main

/*
static int answer(void) { return 42; }
*/
import "C"

import (
	"fmt"
	"runtime"
)

func main() { fmt.Println("boxset-control", runtime.GOOS, runtime.GOARCH, C.answer()) }
GO
cat >"$T/ctl/plain.go" <<'GO'
package main

import (
	"fmt"
	"runtime"
)

func main() { fmt.Println("boxset-control", runtime.GOOS, runtime.GOARCH) }
GO

# Go toolchain: exact version, native cgo build and run through the C compiler.
want="go version go$GO_V linux/amd64"
got=$(go version 2>&1)
if [ "$got" = "$want" ]; then ok "go $GO_V"; else block go "toolchain says '$got', want '$want'"; fi
if [ -n "${GODIR:-}" ] && ! ver_ge "$GO_V" "$GODIR"; then block go "toolchain $GO_V is older than go.mod directive $GODIR"; fi
if out=$(cd "$T/ctl" && CGO_ENABLED=1 go run main.go 2>&1) && [ "$out" = "boxset-control linux amd64 42" ]; then
	ok "go cgo build with gcc"
else
	block go "cgo control failed: $out"
fi

# Cross builds executed under QEMU user emulation.
if (cd "$T/ctl" && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o hello-arm64 plain.go) &&
	out=$(qemu-aarch64 "$T/ctl/hello-arm64" 2>&1) && [ "$out" = "boxset-control linux arm64" ]; then
	ok "qemu-aarch64 runs a Go arm64 binary"
else
	block qemu "arm64 control failed: ${out:-build failed}"
fi
out=
if (cd "$T/ctl" && GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -o hello-arm plain.go) &&
	out=$(qemu-arm "$T/ctl/hello-arm" 2>&1) && [ "$out" = "boxset-control linux arm" ]; then
	ok "qemu-arm runs a Go arm/v7 binary"
else
	block qemu "arm/v7 control failed: ${out:-build failed}"
fi

# Bun runs, and nothing Node-shaped exists.
if [ "$(bun --version 2>&1)" = "$BUN_V" ] && [ "$(bun -e 'console.log(process.versions.bun)' 2>&1)" = "$BUN_V" ]; then
	ok "bun $BUN_V executes"
else
	block bun "bun is not $BUN_V"
fi
for n in node nodejs npm npx yarn pnpm corepack; do
	command -v "$n" >/dev/null 2>&1 && block node "$n is installed"
done

# Linter: built with a Go that supports the module directive, rejects a planted
# printf defect with its named diagnostic and accepts the restored clean fixture.
lv=$(golangci-lint version 2>&1)
case $lv in
*"has version $LINT_V "*) ;; *) block golangci-lint "version output '$lv' lacks $LINT_V" ;;
esac
bw=${lv#*built with go}
bw=${bw%% *}
if [ "$bw" = "$lv" ] || [ -z "${GODIR:-}" ] || ! ver_ge "$bw" "$GODIR"; then
	block golangci-lint "linter built with Go '${bw:-?}' does not support go.mod directive ${GODIR:-?}"
else
	printf 'module controlmod\n\ngo %s\n' "$GODIR" >"$T/lint/go.mod"
	printf 'package main\n\nimport "fmt"\n\nfunc main() { fmt.Printf("%%d", "x") }\n' >"$T/lint/main.go"
	lout=$(cd "$T/lint" && GOLANGCI_LINT_CACHE="${GOLANGCI_LINT_CACHE:-$T/lintcache}" golangci-lint run ./... 2>&1)
	lst=$?
	if [ "$lst" != 0 ] && case $lout in *printf*) true ;; *) false ;; esac; then
		printf 'package main\n\nimport "fmt"\n\nfunc main() { fmt.Println("clean") }\n' >"$T/lint/main.go"
		if lout=$(cd "$T/lint" && GOLANGCI_LINT_CACHE="${GOLANGCI_LINT_CACHE:-$T/lintcache}" golangci-lint run ./... 2>&1); then
			ok "golangci-lint $LINT_V (built with go$bw) rejects a planted printf defect and accepts the clean fixture"
		else
			block golangci-lint "clean fixture run failed: $lout"
		fi
	else
		block golangci-lint "planted printf defect not rejected with a named diagnostic (status $lst): $lout"
	fi
fi

# Browser: only Playwright's chromium-headless-shell, resolvable and launching.
rev=$(grep '^ARG PLAYWRIGHT_CHROMIUM_REVISION=' "$DOCKERFILE") && rev=${rev#ARG PLAYWRIGHT_CHROMIUM_REVISION=}
shell=$BROWSERS/chromium_headless_shell-$rev/chrome-headless-shell-linux64/chrome-headless-shell
dir=${shell%/*/*}
if [ -x "$shell" ] && [ -f "$dir/INSTALLATION_COMPLETE" ]; then
	if ldd "$shell" 2>&1 | grep -Fq 'not found'; then
		block chromium "$shell has unresolved libraries"
	elif vout=$("$shell" --version 2>&1) && case $vout in *"$CHROME_SHELL_V"*) true ;; *) false ;; esac; then
		ok "chrome-headless-shell $CHROME_SHELL_V"
	else
		block chromium "$shell --version said '$vout'"
	fi
else
	block chromium "$shell or its INSTALLATION_COMPLETE is missing"
fi
holds=$(find "$BROWSERS" -mindepth 1 -maxdepth 1 -printf '%f\n' 2>/dev/null | LC_ALL=C sort | tr '\n' ' ')
[ "$holds" = "chromium_headless_shell-$rev " ] || block chromium "$BROWSERS holds '$holds', want only chromium_headless_shell-$rev"

# Capture validator: installed root-owned and read-only, and it runs.
if [ -n "$(find "$CHECKER" -maxdepth 0 -type f -user root -perm 555 2>/dev/null)" ]; then
	"$CHECKER" -name dev-desktop -challenge 0000000000000000 </dev/null >/dev/null 2>&1
	st=$?
	[ "$st" -eq 10 ] && ok "capture-check rejects empty input with status 10" || block capture-check "empty input gave status $st, want 10"
else
	block capture-check "$CHECKER is not an executable root:root 0555 file"
fi

# Environment: required variables survive, nothing else, no parent secrets.
for v in PATH HOME TMPDIR LANG LC_ALL GOPATH GOCACHE GOFLAGS GOPROXY GOTOOLCHAIN PLAYWRIGHT_BROWSERS_PATH; do
	eval "[ -n \"\${$v:-}\" ]" || block env "required variable $v missing"
done
env >"$T/env"
while IFS= read -r line; do
	case ${line%%=*} in
	PATH | HOME | TMPDIR | LANG | LC_ALL | GOPATH | GOCACHE | GOMODCACHE | GOFLAGS | GOPROXY | GOSUMDB | GOTOOLCHAIN | GOTELEMETRY | GOENV) ;;
	GOLANGCI_LINT_CACHE | BUN_INSTALL_CACHE_DIR | BUN_RUNTIME_TRANSPILER_CACHE_PATH | XDG_CACHE_HOME | DO_NOT_TRACK) ;;
	PLAYWRIGHT_BROWSERS_PATH | PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD | BOXSET_CAPTURE_DIR | BOXSET_CAPTURE_CHALLENGE) ;;
	HOSTNAME | PWD | OLDPWD | SHLVL | _) ;;
	*)
		case $line in
		[A-Za-z_]*=*) block env "unexpected variable ${line%%=*}" ;;
		esac
		;;
	esac
done <"$T/env"
if [ -n "${BOXSET_SECRET_SENTINEL+x}" ] || grep -Fq 'synthetic-not-a-credential' "$T/env"; then
	block env "parent secret sentinel reached the child"
else
	ok "no parent secret in the child environment"
fi

# Exposure: no owner/reference/workspace input, no host runner, no socket, no ports.
for p in .env reference prototypes design .devrites scripts/dev-run.sh scripts/dev-run-smoke.sh scripts/dev-run-hardening.sh scripts/dev-run-test.sh; do
	[ ! -e "/src/$p" ] || block exposure "/src/$p is present in the work image"
done
[ -z "$(find /src \( -name '.env' -o -name '.env.*' -o -name '*.pem' -o -name '*.key' -o -name '.npmrc' -o -name '.netrc' \) -not -path '*/node_modules/*' -print -quit 2>/dev/null)" ] ||
	block exposure "a nested secret-shaped file is present in the work image"
[ -z "$(find /src -name '*.cs' -not -path '*/node_modules/*' -print -quit 2>/dev/null)" ] || block exposure "reference C# text is present in the work image"
[ ! -e /var/run/docker.sock ] && ! command -v docker >/dev/null 2>&1 || block exposure "Docker socket or CLI is present"
if [ -r /proc/net/tcp ] && grep -Eq '^ *[0-9]+: [0-9A-F:]+ [0-9A-F:]+ 0A ' /proc/net/tcp /proc/net/tcp6 2>/dev/null; then
	block exposure "a TCP listener exists"
fi
while IFS=: read -r ifname rest; do
	[ -n "$rest" ] || continue
	ifname=${ifname##* }
	[ "$ifname" = lo ] || block exposure "network interface $ifname present"
done </proc/net/dev
[ -d /src/web/node_modules ] || block exposure "frozen dependency install is missing"
fi
if [ "$BLOCKED" -eq 0 ] && [ "$UNLISTED" -eq 0 ]; then
	if [ "$ROOT" = /src ]; then
		printf 'S5-ADMISSION-PASS blocked=0 unlisted=0 rows=%s\n' "$ROWS"
	else
		printf 'S5-MANIFEST-ONLY-PASS blocked=0 unlisted=0 rows=%s\n' "$ROWS"
	fi
	exit 0
fi
fail_out
