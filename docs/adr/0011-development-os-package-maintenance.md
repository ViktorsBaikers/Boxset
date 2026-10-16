# ADR 0011: Development-image Debian OS packages are admitted on distribution support

- **Status:** Accepted, 2026-10-15. Owner decision (q-2026-10-15-001): "Distro support for OS packages: use recorded supported-Debian/security-maintenance status for dev-image OS libraries, fonts and data; retain exact pins, licences/copyright hashes and actual compatibility; retain the twelve-month release/substantive-work rule for Go/npm dependencies, direct tools and base-image admission. No other safety, stack, Git or host-resource approval."
- **Supersedes in part:** AC-001's twelve-month release or substantive-commit test, only for the development image's Debian OS library, font and data rows defined below. Nothing else in AC-001 changes.

## Decision

The S5 admission report (`docs/spikes/s5-version-admission.md`) holds one row per introduced direct package, tool or image, and every row has a category. Each category allows only the kinds listed and needs only its own maintenance evidence:

| Category | Allowed kinds | Maintenance evidence |
| --- | --- | --- |
| `base-image` | image | annual: `release\|upload\|commit <immutable id> <YYYY-MM-DD> <https URL> work=<subject>/<version>` |
| `archive-snapshot` | snapshot | provenance only: `identity <timestamp> <URL>`; never maintenance evidence |
| `direct-tool` | archive, apt, apt-fetch: Go, Bun, golangci-lint, chrome-headless-shell, the compiler packages `gcc` and `libc6-dev`, `qemu-user`, `unzip` | annual, as above |
| `go-dependency` | go | annual, as above |
| `npm-dependency` | npm | annual, as above |
| `os-library` | apt whose installed `Section` is `libs` | distro support |
| `os-font` | apt whose installed `Section` is `fonts` | distro support |
| `os-data` | apt, the closed set `ca-certificates` and `tzdata` | distro support |

**Annual evidence.** `work=<subject>/<version>` names actual attributable release or substantive work on the dependency, dated within 365 days before the run date. The subject (split at the last `/`) is the row's Debian source package for apt and apt-fetch rows and the row's name otherwise. The work may be a newer release of the same source package or project than the installed pin; the version, pin and source cells keep the exact installed pin and the report never presents that pin as released on the evidence date. A snapshot or archive-index URL, identifier or date is never maintenance evidence.

**Distro-support evidence.** `distro-support debian <VERSION_ID> <codename> security-until=<YYYY-MM-DD> <release URL> tracker=<security-tracker source-package URL> lists=<commit> status=<clear|supported|ends-YYYY-MM-DD>`:

- distribution, version and codename equal the dev image `/etc/os-release` and the base-image row; `security-until` is not before the run date; the Dockerfile's only apt suites are `trixie`, `trixie-updates` and `trixie-security`, component `main`;
- the tracker page names the installed source package; `lists` is the commit of the report's single support-list block;
- the block is the immutable release file `security-support.deb<VERSION_ID>` of the Debian `debian-security-support` project at that commit, fenced verbatim with its SHA-256, and every non-comment line is `<pattern> supported`, `<pattern> limited` or `<pattern> non-supported <Debian version> <YYYY-MM-DD>`, each with optional text. Each pattern is a full-string PCRE match against the installed `${source:Package}` (binary name when empty); a matching `supported` line gives `supported`, otherwise a matching `limited` line or a `non-supported` line dated on or before the run date blocks as `distro_unsupported`, a later-dated `non-supported` line gives `ends-<earliest such date>`, and no match gives `clear`. `status` must equal that evaluation. The pre-merge `-ended` and `-limited` files never qualify.
- these statuses describe Debian security-support coverage only; they never claim that a package has no vulnerability.

**Currency.** The support block is data, so root checks, immediately before and again immediately after each actual admission run, that the upstream `master` head's release file has the block's SHA-256 and records both observations in feature evidence. A pre-run mismatch stops the run and routes the current bytes to the report writer; a post-run mismatch voids the result. The proof child never fetches and has no network.

**Stricter on ambiguity.** A row that could belong to an OS category and a non-OS category takes the non-OS one; widening an OS category is a plan change. Exact version pins, licence names with the installed `copyright-sha256` and DEP-5 `License:` set, `debian-source` equality with the installed source package and version, targets and actual compatibility are required for every Debian row.

**Blocking reasons.** A failing row blocks with exactly one of `category_invalid`, `evidence_invalid`, `maintenance_stale`, `distro_unsupported`, `licence_mismatch`, `source_mismatch`, or the existing pin, checksum, floating-version and unlisted reasons.

## Consequences

- The twelve-month release or substantive-commit rule stays for Go and npm dependencies, direct tools and the base image. P-07 is unchanged.
- Snapshot dates never substitute for maintenance evidence; they are provenance identity only.
- The rule applies only to the development image. It grants no other safety, stack, Git or host-resource approval and changes no principle.
- Mature, quiet OS library and font packages (for example `fonts-liberation`) are admitted while Debian 13 security support covers them, and leave the admitted set when that support is limited or ends.
