# Portability record

Scope: AC-043 (eight release targets, CGO_ENABLED=0), AC-006 (lock, storage) and
AC-045 (child kill) as far as they differ per target. Every cell either cites a
source listed under **Sources** (read 2026-10-22 unless a version is named) or says
`not verified` with the exact lookup. This slice imports no ffprobe and no wazero:
sections 3 and 4 are documentation only.

## 1. Result classes

A PASS is only what the command below prints; this file records which class each
target belongs to, never a result the command did not print.

| Class | Meaning | Command | Targets |
| --- | --- | --- | --- |
| native | runs in the amd64 dev container | `scripts/dev-run.sh -- env CGO_ENABLED=0 go test ./internal/install/... ./internal/store/... ./internal/server/...` and `scripts/dev-run.sh -- env CGO_ENABLED=0 go test -tags=integration ./cmd/boxset/...` (the second runs the built binary) | linux/amd64 |
| emulated | the real binary and a JPEG test binary run under the container's user-mode QEMU; no performance claim | `scripts/dev-run.sh -- sh scripts/smoke-targets.sh` prints `SMOKE-TARGET-PASS target=<t> mode=qemu ping=ok jpeg=ok` per target | linux/arm64 (`qemu-aarch64`), linux/arm/v7 (`qemu-arm`) |
| compile-only | built with CGO_ENABLED=0 and checked for its executable format; nothing runs | `scripts/dev-run.sh -- sh scripts/build-targets.sh` prints `BUILD-TARGETS-PASS targets=8 cgo=0` | all eight |
| NOT-RUN | no runtime proof exists in this repository: the unsupported-host runtime is never counted as PASS | none | windows/amd64, windows/arm64, darwin/amd64, freebsd/amd64, freebsd/arm64 |

## 2. Mechanisms per target

| Target | Lifetime lock | TTY check | Owner-only file | Child kill | Storage refusal | Runtime |
| --- | --- | --- | --- | --- | --- | --- |
| linux/amd64 | `flock(LOCK_EX\|LOCK_NB)` on `boxset.lock` (`internal/install/lock_unix.go`); released by the kernel when the process exits | go-isatty `TIOCGWINSZ` (S3) | mode 0600, owner = effective uid, checked before any byte is written (`secretfile_unix.go`) | protected subreaper guardian, stable process handles and matching `ECHILD` completion (`internal/child/guardian_linux.go`); `TestChildManagedCompletion` covers normal exit, detachment and cleanup generations | `/proc/self/mountinfo` filesystem type (`mount_linux.go`, fixtures `internal/install/testdata/mountinfo-*.txt`) | native |
| linux/arm64 | as linux/amd64 | as linux/amd64 | as linux/amd64 | as linux/amd64 | as linux/amd64 | emulated |
| linux/arm/v7 | as linux/amd64 | as linux/amd64 | as linux/amd64 | as linux/amd64 | as linux/amd64 | emulated |
| windows/amd64 | `LockFileEx(EXCLUSIVE\|FAIL_IMMEDIATELY)` on byte 0 (`lock_windows.go`; flags S2); the errno of a contended lock is unproven, so every failure is reported as a held lock; release after a crash may lag (not verified: Microsoft `LockFileEx` page) | go-isatty `GetConsoleMode` (S3) | file created with a protected DACL granting full control to the current user only: SDDL `D:P(A;;FA;;;<sid>)` (`secretfile_windows.go`; `P` = SE_DACL_PROTECTED, S4; `SecurityDescriptorFromString`, `CreateFile`, S2). Unix permission bits mean nothing here | `CREATE_SUSPENDED`, job object with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`, `AssignProcessToJobObject`, then resume the primary thread (`CreateToolhelp32Snapshot`, `ResumeThread`); the symbols exist in x/sys v0.48.0 (S2); implemented in `internal/child/child_windows.go`, compiled for windows/amd64 and windows/arm64 by `go test -c` and `scripts/build-targets.sh`, runtime NOT-RUN | none: the refusal exists on Linux only. `not verified`: the Windows volume-type lookup (`GetVolumeInformation`) | NOT-RUN |
| windows/arm64 | as windows/amd64 | as windows/amd64 | as windows/amd64 | as windows/amd64 | as windows/amd64 | NOT-RUN |
| darwin/amd64 | same `flock` code as Linux; compiled for darwin (`go build`, this slice). BSD `flock` and `fcntl` locks interact: `not verified` (lookup: `man 2 flock` and `man 2 fcntl` on macOS) | go-isatty `TIOCGETA` (S3) | as linux/amd64, compiled for darwin | stronger managed operation unavailable before effects; fixed diagnostics retain `Setpgid` (S1) | none: Linux only. `not verified`: `statfs(2)` `f_fstypename` lookup on macOS | NOT-RUN |
| freebsd/amd64 | as darwin/amd64 (lookup: `man 2 flock` on FreeBSD) | go-isatty `TIOCGETA` (S3) | as linux/amd64, compiled for freebsd | as darwin/amd64 | none: Linux only. `not verified`: `statfs(2)` `f_fstypename` lookup on FreeBSD | NOT-RUN |
| freebsd/arm64 | as freebsd/amd64 | as freebsd/amd64 | as freebsd/amd64 | as freebsd/amd64 | as freebsd/amd64 | NOT-RUN |

Storage refusal (Linux only). The data directory is first resolved to one absolute
path with symlinks resolved; a part that does not exist yet is judged by its nearest
existing ancestor, and a value such as `file:/x` is a relative directory name, never a
SQLite URI. The filesystem is the mount that serves that path: the parser walks the
mount tree from the root by mount id and parent id, so a mount hidden under another
mount, or stacked on the same point, never decides, whatever its line order. Refused
types: `nfs`, `nfs4`, `cifs`, `smb3`, `fuse`, `fuse.*`, `fuseblk`, `fuseblk.*`,
`virtiofs`, `fakeowner`, `9p`, `drvfs`; any other type is accepted as local. If
`/proc/self/mountinfo` cannot be read, startup stops with `mount_check_failed`; there
is no `statfs` fallback. The `/proc/self/mountinfo` field layout is `not verified`
against `man 5 proc`; the parser is held only to the fixtures in
`internal/install/testdata`. Other targets accept every directory (no check).

### Managed child lifetime

Linux managed children require a prepared owner and a same-binary guardian. Both
keep `PR_SET_DUMPABLE=0` for their lifetime, have no effective, permitted or
inheritable `CAP_SYS_PTRACE`, clear `PR_SET_PTRACER`, and use no-new-privileges on
their launching threads. This disables kernel core files and same-UID debugger
attachment to these processes. Explicit `debug.SetCrashOutput` evidence remains
available through the independent fixed crash monitor.

Managed admission requires readable Yama policy 1, 2 or 3 continuously across
attempts and restart. Missing, malformed or zero policy refuses managed targets
before effects; it does not by itself prevent ordinary serve or fixed diagnostics.
Only a private unnamed seqpacket socket crosses Linux helper exec. Authenticated
kernel credentials precede the descriptor handoff. Managed completion requires
the guardian's matching attempt and kernel `ECHILD` after owned cleanup; leader
exit or helper disappearance alone is unknown. Future effect owners must retain
their durable attempt fences when completion is unknown. An administrator changing
policy and an already attached external debugger fall outside this protection claim.

Other Unix targets retain fixed diagnostic support and refuse the stronger
managed operation before target effects. Windows retains suspended startup,
non-breakaway Job assignment, explicit Job termination and an observed empty Job
before quiescent completion. Windows runtime remains NOT-RUN; compilation is the
only proof claimed here. The diagnostic leaf deliberately survives owner death
to publish its bounded redacted record only after crash-input EOF.

## 2a. Pre-migration snapshot barrier

Before a database that already holds tables is migrated, `serve` publishes a verified
snapshot under `backups/` and keeps the newest three. Publication needs a way to make
a new directory entry and a rename durable.

| Target | Mechanism | Runtime |
| --- | --- | --- |
| linux/amd64, linux/arm64, linux/arm/v7 | `fsync` of the data directory after `backups/` is created, of the snapshot file, and of `backups/` after the rename; a failure of any of them stops the migration before it changes the database | native (amd64), emulated (arm64, arm/v7: build and ping only, not the snapshot tests) |
| darwin/amd64, freebsd/amd64, freebsd/arm64 | the same unix `fsync` calls, compiled for the target. `not verified`: whether `fsync` is a durable barrier on these systems (lookup: `man 2 fsync` on macOS and FreeBSD) | NOT-RUN |
| windows/amd64, windows/arm64 | none established: `namespaceBarrier()` and `syncDir` return `ErrSnapshotUnsupported`, never success. A database that needs migrating is refused before `backups/`, a temporary file or any change exists: `serve` exits 3 with reason `snapshot_unsupported`. An empty database file and one with nothing pending open normally | NOT-RUN (compiled, including its test) |

If the clock stands at or before the newest published name, the new snapshot is named
just after it, so retention keeps the snapshot just made and the two before it.
Retention runs after publication: a failure to remove an older snapshot stops the
migration but leaves the published snapshot in place.

## 2b. Restore, journal and key-file barriers

`boxset restore --from FILE` writes `restore.journal`, keeps a recovery copy of the
replaced database and replaces the database file, all under the lifetime lock. Every
step needs a namespace-persistence barrier, and so does a journal continuation at
start.

| Target | Restore, journal and replacement | Key-file read/verify barrier | Runtime |
| --- | --- | --- | --- |
| linux/amd64, linux/arm64, linux/arm/v7 | the journal is written to `restore.journal.tmp`, `fsync`ed, renamed and the data directory `fsync`ed; the recovery copy, the staged snapshot and the replacement are `fsync`ed and the directory is `fsync`ed after the rename | `VerifySecretFile` checks the opened file (regular, owner = effective uid, no group or other bit, SHA-256 = its name), then `fsync`s the file and its directory | native (amd64), not run for arm64/arm/v7 |
| darwin/amd64, freebsd/amd64, freebsd/arm64 | the same unix calls, compiled for the target. `not verified`: whether `fsync` is a durable barrier on these systems | as linux/amd64, compiled for the target | NOT-RUN |
| windows/amd64, windows/arm64 | none established, so every restore, same-schema or older-schema and including a journal continuation, is refused before the lock, the snapshot file, a journal or a recovery copy exists: `boxset restore` and `serve` exit 3 with reason `restore_unsupported`, and a journal is neither resumed nor discarded | `FlushFileBuffers` on the opened read/write file handle flushes the file and its metadata; there is no directory handle to flush, so the file's own flush is the whole barrier. Compile-only | NOT-RUN (compiled with its tests; the reason is recorded in `docs/operations.md`) |

The Windows key-file publication path has no rename at all: an existing regular
file at the key's path is unlinked, the file is created under its final name with a
protected owner-only DACL in that one call, written and flushed with
`FlushFileBuffers` on the same handle. That flush is the whole barrier — there is no
directory handle to flush and no write-through rename to rely on. No hardware
power-loss certification is claimed for any target (ADR 0003).

## 3. ffprobe availability, licence and packaging (documentation only)

Nothing here admits ffprobe. FFmpeg's licence (S7): LGPL 2.1 or later, and the GPL
applies to all of FFmpeg when GPL-covered optional parts are compiled in. FFmpeg
itself provides only source; the page below lists where compiled builds come from (S6).

| Target | Where a build comes from | Licence of that build | Static packaging | Status |
| --- | --- | --- | --- | --- |
| linux/amd64 | Debian and Ubuntu official packages, RPM Fusion (S6, dynamic); BtbN "64-bit static and shared builds" (S6) | LGPL 2.1+ or GPL depending on the build's configure line (S7). `not verified`: the configure line of the chosen BtbN asset (lookup: run its `ffprobe -version` and read the `configuration:` line for `--enable-gpl`) | static builds are offered (S6). `not verified`: archive contents (lookup: list the chosen BtbN release archive, confirm `bin/ffprobe`, and run `ldd` on it) | listed |
| linux/arm64 | Debian official packages (S6, dynamic). The S6 static-build entry says "64-bit" only and does not name an architecture: `not verified` (lookup: the BtbN release asset list for an arm64 asset) | `not verified` (as above) | `not verified` | not verified |
| linux/arm/v7 | Debian official packages (S6, dynamic; per-architecture availability `not verified`, lookup: https://tracker.debian.org/pkg/ffmpeg). No static build is listed on S6 | `not verified` | `not verified` | not verified |
| windows/amd64 | gyan.dev builds and BtbN builds (S6) | `not verified` (lookup: each provider's build page, configure line of the chosen archive) | `not verified` (lookup: list the chosen archive, confirm `bin/ffprobe.exe` has no sibling DLLs) | listed |
| windows/arm64 | not listed on S6 | `not verified` | `not verified` | not verified |
| darwin/amd64 | evermeet.cx "Static builds for macOS 64-bit" (S6) | `not verified` (lookup: https://evermeet.cx/ffmpeg/) | the provider describes them as static (S6); archive contents `not verified` | listed |
| freebsd/amd64 | not listed on S6 | `not verified` (lookup: https://www.freshports.org/multimedia/ffmpeg/) | `not verified` | not verified |
| freebsd/arm64 | not listed on S6 | `not verified` (lookup as freebsd/amd64) | `not verified` | not verified |

## 4. Artwork engine

Source S8 (`internal/platform/platform.go` of wazero, main branch; the version Boxset
would pin is not admitted, so the pin may differ): the compiler is supported on
linux, darwin, freebsd, netbsd and windows; on amd64 only when the CPU reports SSE4.1;
on arm64 always, except that with the threads feature enabled it needs the CPU's
atomics feature (LSE); on every other architecture, including 32-bit arm, there is no
compiler and the interpreter is the engine. The plan's wording "arm64 only with LSE
atomics" is narrower than this source; this record follows the source, and the
wiring decision stays with the slice that imports wazero.

| Target | Engine | Image path |
| --- | --- | --- |
| linux/amd64, windows/amd64, darwin/amd64, freebsd/amd64 | compiler with SSE4.1, interpreter without | wazero where it runs; JPEG otherwise |
| linux/arm64, windows/arm64, freebsd/arm64 | compiler (atomics feature needed only with threads enabled) | wazero where it runs; JPEG otherwise |
| linux/arm/v7 | interpreter, no compiler | JPEG |

JPEG is the standard library `image/jpeg`. `TestJPEGRoundTrip` (`internal/install/portability_test.go`)
runs natively in the container and, as a cross-compiled test binary, under QEMU for
linux/arm64 and linux/arm/v7 through `scripts/smoke-targets.sh`. Image commitments
remain amd64, arm64 and arm/v7.

## Sources

- S1: Go 1.27.2 `src/syscall/exec_linux.go` lines 75-77 and 393-396, `src/syscall/exec_bsd.go` lines 19-21 and 115-119 (`SysProcAttr.Setpgid`, `setpgid`).
- S2: `golang.org/x/sys` v0.48.0 `windows`: `syscall_windows.go` 66-68 (`LOCKFILE_*`), `zsyscall_windows.go` 1785, 1881, 1983, 2923, 3195, 3388 (`AssignProcessToJobObject`, `CreateFile`, `CreateToolhelp32Snapshot`, `LockFileEx`, `ResumeThread`, `SetInformationJobObject`), `security_windows.go` 667, 709, 1454 (`GetCurrentProcessToken`, `GetTokenUser`, `SecurityDescriptorFromString`), `types_windows.go` 249 and 2552 (`CREATE_SUSPENDED`, `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`).
- S3: `github.com/mattn/go-isatty` v0.0.24 `isatty_tiocgwinsz.go` (linux), `isatty_bsd.go` (darwin, freebsd), `isatty_windows.go` 23 and 41-45.
- S4: https://learn.microsoft.com/en-us/windows/win32/secauthz/security-descriptor-string-format (dacl_flags `P` = SE_DACL_PROTECTED).
- S5: Go 1.27.2 `src/os/exec/exec.go` 281-314 (`Cmd.Cancel`, `Cmd.WaitDelay`).
- S6: https://ffmpeg.org/download.html ("Get packages & executable files").
- S7: https://ffmpeg.org/legal.html ("FFmpeg License").
- S8: https://raw.githubusercontent.com/tetratelabs/wazero/main/internal/platform/platform.go (`compilerPlatformSupports`).
