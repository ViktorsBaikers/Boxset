# S5 Bun toolchain: the eight web tools

This record describes how the eight web tools run on Bun against the public access shell (first a synthetic probe page, retired by the production shell), how a failure is reported and
how the four browser captures reach the root. The root observed the command below pass on 2026-10-16 (EVID-002): Bun 1.4.3 ran
all eight tools (component tests 2/2, Playwright 5/5, axe violations 0, the generated-message compile, four validators and four
captures) and every control in the failure check passed, in the `work` image `sha256:a335e86b43ec…` built from inputs
`4afa1fa30ee1…`; the raw records are in feature evidence. That run covers the source before the final controls described under
Failure and is not a claim of feature completion or of clean editor diagnostics. The `bun` column of the npm rows in
[s5-version-admission.md](s5-version-admission.md) reads `executed EVID-002 2026-10-16`.

## Commands

```sh
cap=".devrites/work/boxset-m00a-foundation/screenshots-$(date -u +%Y%m%dT%H%M%SZ)"
scripts/dev-run.sh --write docs/spikes/issue-drafts --capture "$cap" -- scripts/s5-toolchain.sh --tools
scripts/dev-run-smoke.sh --tools-failure
```

The first prints `S5-TOOLS-PASS tools=8 failed=0 skipped=0 bun_only=true` and, from the runner, `DEV-RUN-CAPTURE-PASS files=4`.
The second prints `S5-FAILURE-PROOF-PASS draft=retained downstream=0`. Both run in the pinned `work` image; the host runs no Go,
Bun or Node. `--tools` needs the runner's `BOXSET_CAPTURE_DIR=/capture` and 16-hex `BOXSET_CAPTURE_CHALLENGE` and otherwise
prints `S5-TOOLS-CAPTURE-UNSET` (status 2). The child has no network; the dev server and the production preview bind
`127.0.0.1` inside the container (ports 5173 and 4173) and nothing is published.

## The public shell

`web/src/routes/+page.svelte` is the public shell: the Boxset identity, an announced (`role="status"`) loading message and, when
the browser is offline, a reconnect message with a keyboard-reachable Retry that re-checks connectivity. It uses the native-control
wrappers `web/src/lib/ui/Button.svelte` and `Status.svelte`, styles only from the DESIGN.md `@theme` tokens in `web/src/app.css`
(Tailwind v4 through `@tailwindcss/vite`) and takes every string from a Paraglide message function compiled from
`web/messages/en.json`. It holds no private data, no auth form and makes no request. The retired toolchain probe (its route,
component, Vitest file and Playwright spec) no longer exists. The production build (`bun run build`: `vite build`, then
`web/scripts/precompress.ts`) writes `internal/server/dist` under the placeholder base `/__boxset_base__`; it is run only in a
runner child or a Docker stage, never in the owner tree, because adapter-static removes its whole output directory first.

The approved Archivo variable face is self-hosted from exact dev-only `@fontsource-variable/archivo` 5.3.0, using only
its exported Latin standard-normal WOFF2 (weight 100–900, width 62–125). The custom face keeps DESIGN.md's `Archivo` family
and Latin descriptors; the wordmark applies width 70. Precompress verifies emitted font bytes against the installed asset
and emits the original copyright-plus-OFL LICENSE as a content-addressed immutable text asset before normal compression.
The existing real-build HTTP suite and binary self-check compare both assets with the installed package. Ordinary browser
proof requires the self-hosted face to load, the actual rendered custom font, one Latin WOFF2 request and wordmark width 70.
Its responsive checks cover Loading and Offline at 320/768/1024/1440 and a 320–1920 overflow sweep, expanded pseudo text,
text-spacing overrides, light/dark and reduced motion, restoring exact content and viewport before the four captures.
These automated checks do not establish genuine text/browser zoom, manual announcements or whole-feature WCAG compliance.

### Production CSP

The Go loader keeps the script hashes computed after base rewriting. Its only style-attribute allowance is
`style-src-attr 'unsafe-hashes' 'sha256-S8qMpvofolR8Mpjy4kQvEm7m1q8clzU4dfDH0AmvZjo='`, pinned to the exact 143-byte
Kit 3.0.1 route-announcer literal. `style-src 'self'` and every other directive stay unchanged. The shared installed-source
assertion in `internal/server/shell_test.go` is also consumed by the Go browser launcher; a changed literal fails rather than
updating the production hash. `TestBrowserShellStylePolicy` uses fresh production documents at both bases to prove the matching
attribute is applied, a one-byte change and an unrelated attribute are blocked, and the restored announcer remains accessible.
It runs without capture publication:

```sh
scripts/dev-run.sh -- sh -euc 'bun --cwd=web run build; exec env CGO_ENABLED=0 go test -v -tags=e2e ./internal/server -run "^TestBrowserShellStylePolicy$"'
```

EVID-089 and `packets/csp-attribution-acquisition-03.log` in the access workspace retain the original unchanged-policy acquisition:
eight isolated documents, one actual offending announcer node per document, literal/source equality and SHA-256
`4bca8ca6fa1fa2547c3298f2e2442f126ee6d6af1c97353875f0c7d009af663a`. That diagnostic is historical prerequisite evidence, not
zero-violation acceptance of the changed candidate. The explicit `BOXSET_CSP_ATTRIBUTION=1` acquisition code and its adversarial
fixtures remain diagnostic only; the ordinary production command uses no diagnostic selector or environment and must observe
zero product violations before axe/challenge instrumentation. The approved declaration-only lock update does not change Kit
or its source, but it does change the candidate/lock identity; the earlier acquisition is not relabelled as a run of that identity.

### Private live source placement

Compose mounts `/src` read-only. A read-only anonymous `/src/web` volume copies up the image's web scaffold; its finite
source/config entries link into the read-only directory-backed checkout at `/checkout/source/web`. Individual-file binds
are not used, so an editor's atomic replacement remains visible without creating checkout entries. Outside that bind,
the image-owned `/checkout/node_modules` points to the isolated `/src/web/node_modules`. Named dependencies and
`.svelte-kit` cover generated paths; anonymous built `internal/server/dist` stays read-only. `make dev` renews both anonymous
volumes, preserving the named volumes' uid-1000 ownership.

`watch.ts` checks image/manifest stamps before children start, replaces only stale dependency-volume contents with the stamp
written last, and leaves `/config` and caches intact. Go fills missing modules offline from the image seed. Inlang settings,
including `.lix`, live in the uid-1000 `/tmp/i18n` copy; message inputs stay read-only, Paraglide output stays on `.svelte-kit`,
and backend executables stay on executable `/tmp`. `scripts/dev-run-smoke.sh --live-dev` starts from a clean uid-1000-owned
copy, inspects every consumed source bind (`RW=false`), requires `EROFS` on an owned source write, checks generated-path
ownership/writes, and compares checkout bytes and entries across generated-output creation and down/up. It retains the
fresh/current, stale-volume replacement, offline Go seed, config sentinel and stale-image refusal controls.

The reload control changes the backend's actual `/ping` body, requires the changed HTTP response plus a new build/PID,
then restores exact Go bytes and the original response. Dependency-copy controls use a genuinely unreadable seed file
and a finite pre-stamp interruption barrier only in the owned disposable copy. Each requires no current completion stamp
or child listener, an unchanged `/config` nonce, and a healthy same-volume retry resolving the new package. Named-volume
creation identities are compared across the transition. The synthetic `--benchmark-evidence` controls inject their
writable-record mutations into the uniquely identified `work` stage, restore the exact Dockerfile and retain the real
writable canaries and `record_mutable`/no-PASS judge assertions.

The directory-swap control checks arming after the real running-container inspection, so a concurrent start cannot bypass
restoration. Its child waits on the existing release gate until the disposable runner's real mount-identity read completes;
the host releases mounted directory B, not restored directory A. The fixture restores exact runner bytes and asserts both
directory identities as well as the original rejection and no-publication conditions.

The two resolvers have different jobs. Bun uses its default module canonicalization when the direct installed Vite
entrypoint loads native config and plugin packages through physical dependency ancestry. Bun's `--preserve-symlinks`
flag would split Kit/Vite class identity and is not used. Vite's own `resolve.preserveSymlinks: true` applies afterward:
it keeps component IDs on logical `/src/web` so existing relative generated imports reach the `.svelte-kit` volume.
The native config module remains cached across Vite's in-process restart; the existing Bun supervisor therefore restarts
the Vite child on parent-directory config events, with serialized replacement and a bounded shutdown. Source component
changes remain genuine Vite HMR, including exact source restoration in the same browser document.

The isolated pre-adoption discriminator exercised config/plugin imports, generated paths, source isolation, changed and
restored Vite-owned HTTP headers and genuine HMR before and after populated dependency replacement. This does not replace
the final canonical `--live-dev` run. That driver also rejects a writable source bind while both services remain healthy,
restores the exact config and requires its readonly checks to pass again.

The watcher keeps native Bun subprocess APIs. Its sole direct declaration dependency is dev-only `@types/bun` 1.4.3;
the lock supplies matching `bun-types` and upstream Node/Undici declarations, not a Node runtime. Check the complete native
declaration context without DOM libraries or suppressions:

```sh
scripts/dev-run.sh -- sh -euc 'exec bun web/node_modules/typescript/bin/tsc --noEmit --strict --target esnext --module preserve --moduleResolution bundler --types bun --typeRoots ./web/node_modules/@types,./web/node_modules --lib esnext deploy/dev/watch.ts'
```

### Retired assertion disposition

The original `Probe.svelte.test.ts` and `e2e/probe.spec.ts` at checkpoint
`d280bc702b17966c007d3f5ac3fa5d9427e5e526` are replaced as follows. Counts alone do not establish preservation.
`component` below means `scripts/dev-run.sh -- bun --cwd=web run test`; `browser` means the existing production
`TestBrowserShell` command with the four captures, and S5's dev/preview `--tools` consumer.

| Original assertion | Replacement assertion / disposition |
| --- | --- |
| Component: button visible (10) | `page.svelte.test.ts` offline Retry is visible before pointer activation |
| Component: `tagName === 'BUTTON'` (11) | Offline Retry explicitly has native `BUTTON` tag |
| Component: UA contains `HeadlessChrome` (12) | Same exact browser-engine assertion in the offline component test |
| Component: status `0` (13) | Obsolete toy count; real first-paint test asserts announced `Loading Boxset…` |
| Component: paragraph `Activations: 0` (14) | Obsolete probe-only presentation; public shell has no activation paragraph |
| Component: pointer count `1` (24) | Count obsolete; pointer Retry test changes connectivity without an event, clicks, then requires loading and Retry removal |
| Component: Enter count `2` (27) | Count obsolete; offline test changes connectivity without an event, presses Enter, then requires loading and Retry removal |
| Component: focused button (29) | Offline test asserts Retry focus before and after pointer/Enter |
| Component: `:focus-visible` (31) | Same exact assertion on Retry after Tab and pointer/Enter |
| Component: outline not `none` (32) | Same computed-style assertion on Retry |
| Browser: Bun worker (18) | `shell.spec.ts` retains `process.versions.bun`; S5 preload also checks tool/worker runtime identities |
| Browser: capture directory/challenge/origin (30–33) | Same three assertions in every shell base/viewport case |
| Browser: button visible (49) | Offline Retry visibility assertion at both bases, desktop/mobile |
| Browser: status `0`, pointer `1`, Enter `2` (50,53,55) | Toy counts obsolete; browser exercises Loading/Offline/Retry, pointer and Enter; component tests discriminate both activation effects |
| Browser: focus / focus-visible / outline (56–59) | Same Retry focus, selector and computed-outline assertions after pointer/Enter |
| Browser: axe zero violations (62) | Separate Loading and Offline axe results each equal `[]` |
| Browser: actual DPR/viewport check (70–74) | Same measured values, rejection marker and throw before challenge injection |
| Browser: button/capture-part boxes non-null (111,114) | Same assertions for Retry and both challenge parts |
| Browser: challenge parts do not overlap button (120) | Same rectangle-separation assertion against Retry |
| Browser: focus unchanged after labels and button focused (122–123) | Active element's exact outerHTML is unchanged and Retry remains focused after injection |
| Browser: no console/page errors (124) | Same `problems === []` in ordinary mode; no product CSP-event reset |
| Browser: four viewport screenshot writes (126–131) | Same fixed names/options, now capturing public Offline/Retry with visible focus |

The old probe does not own runner failure semantics. They remain executable in `scripts/dev-run-smoke.sh --tools-failure`:
`tf_fail` injects at `+page.svelte`, requires its exact file/line and redacted draft, one started tool and zero downstream tools;
`tf_write`, `tf_read`, `tf_stall`, `tf_success`, `tf_cap`, `tf_env`, `tf_log`, `tf_sink` and `tf_big` retain their specific
status, bounded-output, capture and cleanup assertions. Root still runs the complete production browser/capture, S5 tools,
failure, live-loop and admission commands and dispositions this assertion map before accepting retirement.

## The eight tools

Each tool is run as `bun <package bin script>` (no Node binary exists and no `node` shim is created), in this order; the first
failure stops the run and nothing after it starts. Every Bun process records its runtime through a `BUN_OPTIONS` preload, so
a tool, its workers and the checks the script itself runs are all counted, and any record that is not the admitted Bun fails
the tool.

| order | id | command | positive assertions |
| --- | --- | --- | --- |
| 1 | `vite-build` | `bun run build` (offline Paraglide compile, `vite build`, precompress into `internal/server/dist`), then `vite preview` on loopback under `/__boxset_base__/` | exit 0; `internal/server/dist/200.html` holding the placeholder; at least one `.js` chunk holding the loading message; no compressed sibling beside a placeholder-bearing `_app/` file and `.br`, `.zst` and `.gz` beside every other `_app/immutable/` file; the generated Paraglide functions return the five `en.json` values (a warning-only or zero-message compile fails here); preview serves the SvelteKit shell |
| 2 | `svelte-check` | `svelte-kit sync`, then `svelte-check --output machine --threshold warning --fail-on-warnings` | sync first; `COMPLETED n FILES 0 ERRORS 0 WARNINGS` with `n >= 3` |
| 3 | `eslint` | `eslint --max-warnings 0 --format json .` | at least 5 files, 0 errors and warnings; a planted unused variable on stdin is rejected (exit 1, `no-unused-vars`) |
| 4 | `prettier` | `prettier --check` over `src`, `e2e`, `scripts`, `messages` and the config files | exit 0 and "All matched files use Prettier code style!" (the status is checked before the output is read) |
| 5 | `vitest-browser` | `vitest run --reporter=json` (`browser.headless: true`, no channel) | at least 2 tests, all passed, 0 failed, 0 skipped, in the real `chromium-headless-shell` (user agent `HeadlessChrome`) |
| 6 | `vite-dev` | `vite dev --host 127.0.0.1 --port 5173 --strictPort` | serves loopback and transforms `Button.svelte` into a Svelte client module; stays up for step 7 |
| 7 | `playwright-axe` | `playwright test` against dev (root base) and preview (`/__boxset_base__/`) (`headless: true`, no channel) | Bun worker identity, four base/viewport maps and diagnostic-mechanism fixtures, 0 failed/skipped/flaky; per map: public identity and announced loading, deep link, native Retry pointer/keyboard/focus behavior, axe violations 0 in both states, no product `securitypolicyviolation`, console or page error; four challenge-validated captures preserve Retry focus |
| 8 | `sv` | `sv create <tmp> --template minimal --types ts --no-add-ons --no-install --no-dir-check`, then `svelte-kit sync` and `svelte-check` there | generated `package.json`, `vite.config.ts`, `tsconfig.json`, `src/routes/+page.svelte` and `typescript`; the generated project checks with 0 errors and 0 warnings |

`sv create` uses its built-in `minimal` template (no remote template). The generated project's default adapter package is not
an admitted dependency, so the disposable copy names the admitted `@sveltejs/adapter-static` instead; its packages are linked
from `web/node_modules` one by one.

Facts the shell configuration rests on (SvelteKit 3.0.1, read from the installed source): configuration lives in the
`sveltekit({...})` plugin options in `web/vite.config.ts`, and a `svelte.config.js` is a hard error
(`config_file_unsupported`), so that allowlisted path is intentionally absent; `$lib` is removed in favour of `#lib`, which needs a
`package.json` `imports` entry that the closed manifest does not admit, so the shell imports by relative path; the tsconfig extends `$app/tsconfig`.

Offline Paraglide: the official `paraglideVitePlugin` (`@inlang/paraglide-js` 2.26.0) sits beside `sveltekit(...)` in
`web/vite.config.ts` with project `./project.inlang` and the compiler-owned output `./.svelte-kit/paraglide` (inside the ignored
`.svelte-kit`; nothing generated is committed). `web/project.inlang/settings.json` is the only human-authored project file: base
and sole locale `en`, the local module `./node_modules/@inlang/plugin-message-format/dist/index.js` (resolved from `web/`) and
`pathPattern` `./messages/{locale}.json`. The plugin `@inlang/plugin-message-format` 4.4.0 is an exact dev dependency (the
version Paraglide 2.26.0's own template names, MIT, one declared dependency `flat`); it is admitted by report row ADM-122 and was
added by the admitted Bun in a network-enabled one-shot bootstrap container (see the admission report), after which every proof
child, the compile included, runs `--network none`. A missing module only warns and dev compile errors are swallowed, so the
`vite-build` assertion above is what fails a compile that produced no working messages. The compiler also writes
`project.inlang/.gitignore`, `README.md`, `.meta.json` and `cache/`; those are generated inside containers only.

## Captures

The Playwright spec visits each of `dev-desktop`, `dev-mobile`, `preview-desktop`, `preview-mobile` (1440x900 and 390x844,
`deviceScaleFactor` 1). It reads the actual `devicePixelRatio`, `innerWidth` and `innerHeight` with `page.evaluate` before
anything is injected and fails with `S5-CAPTURE-VIEWPORT-MISMATCH name=<name> dpr=<n> inner=<w>x<h>` unless they are 1 and the
mapped size. It then appends an `aria-hidden`, non-focusable challenge strip (66 opaque 8x8 cells at the bottom left: the
16-hex challenge most significant bit first, then mode 0 dev / 1 preview, then viewport 0 desktop / 1 mobile; black is 1,
white is 0) and the label `boxset-capture <challenge> <name> <innerWidth>x<innerHeight>@<devicePixelRatio>` directly above it,
checks that neither overlaps the button or moves focus, and takes a viewport-only screenshot to `/capture/<name>.png`. The
strip and label are protocol pixels read by `deploy/dev/capture-check`, not theme styling. The runner's validators, manifest and
publication are in the runner notes of the version-admission report.

## Failure

A failing tool prints `S5-TOOL-FAIL id=<id> exit=<n> reason=...`, `S5-TOOLS-FAIL tool=<id> exit=<n> bun_only=<bool>` and
`S5-TOOLS-BLOCKED dependents=<ids>`, and writes a bounded (60,000 bytes) draft straight to
`/src/docs/spikes/issue-drafts/s5-tools.md`, then exits 1. The draft is generated directly into that name (no intermediate or
temporary file, no rename); every construction status and the capped write's own status are checked, so a failed or partial
write prints `S5-DRAFT-WRITE-FAILED name=s5-tools.md` and never `S5-DRAFT-WRITTEN`. Each log's last 40 lines are read by their
own `tail`, whose status is checked apart from the consumer that caps them at 6000 bytes (the consumer drains what it does not
emit, so the cap never fails a good draft); a log that cannot be read fails the draft. With `--write docs/spikes/issue-drafts`
the runner publishes the draft redacted. The draft holds the failing tool, exit, command, blocked dependents and the last 40
lines of that tool's output; it is for owner review and is never filed by an agent. A normal pass writes none and leaves any
existing host draft untouched. Every tool's own exit status is checked before its output is compared, and server readiness has
one elapsed bound of 60 s (every request and body wait included), so a stalled or dead server ends in the named tool failure and
draft long before the runner's 600 s deadline. There is no Node or other fallback, and a failed or interrupted child publishes no
capture.

`scripts/dev-run-smoke.sh --tools-failure` drives ten controls in its own scratch copy, each on a real child and real Docker:
(1) `web/src/routes/+page.svelte` gets a genuine syntax error whose site carries the selected synthetic sentinel, so the real `vite build` exits
1: the draft keeps the tool, file and line with `[REDACTED]` where the sentinel was and no raw value, and the complete run holds
exactly one start record (`vite-build`); (2) the draft destination is a read-only directory: the exact write-failure marker,
nonzero, nothing later started, no false "written"; (3) a nonempty diagnostic log the child itself made unreadable (the scratch
vite config removes every permission from its own output logs): the draft's real `tail` gets `Permission denied`, the draft keeps
its nonempty header but the exact write-failure marker prints, never "written", nothing later starts and nothing is captured;
(4) a real preview server that never completes its response body: the named readiness failure (`exit=3`), a retained draft and
exit 1 (not 124), with no elapsed threshold (the run includes the image builds); (5) the real eight-tool success next to a
pre-seeded host draft: `S5-TOOLS-PASS`, no new draft, the pre-seeded bytes unchanged and the owned scratch captures published;
(6) the runner's capture rejection table, each case a zero-exit child leaving real input (missing, empty, link, oversize,
malformed plaintext carrying the sentinel, wrong dimensions, wrong challenge) and the selected word `file` in Docker's
missing-file message: a fixed reason, status 125, no published directory, no owned container left, and the passive scratch
observer finding no raw value on the host; (7) missing or malformed `BOXSET_CAPTURE_*` for `--tools` (challenge missing or
malformed, directory missing or `/wrong`, each beside a valid runner-provided sibling): `S5-TOOLS-CAPTURE-UNSET`, status 2, no
tool started, nothing captured; (8) a validator created by the real Docker with a non-`none` log driver (one token changed in the
scratch runner copy) is rejected with `log_driver_invalid` before it starts, and the passive observer reads the real Docker back
before the owned validator is removed: created, zero `StartedAt`; (9) a real host-sink write failure while the sink keeps draining
its input (a size-limited write with the file-size signal ignored, in the scratch runner copy) after the validator exited 0 (the
same child is accepted by the unchanged runner): the observer reads the faulted validator back as exited 0 with attachment
status 0, a nonzero sink status and a nonempty output shorter than the accepted one, and the run ends `capture_copy_failed` with
nothing published; (10) a readable diagnostic log far over the cap: the scratch vite config prints 64 numbered 4000-byte lines to
stderr before the real syntax error, so the log's last 40 lines far exceed both the 6000-byte draft cap and a 64 KiB pipe buffer.
The draft's own first tail line must be a whole numbered line, and the lines from it to the last numbered line must total more
than 65,536 bytes (that precondition fails a tail too small to force the drain); the tool's section is then exactly the 6000-byte
cap, the draft is written with `tool=vite-build exit=1` and `S5-DRAFT-WRITTEN`, `S5-DRAFT-WRITE-FAILED` is absent (the tail's own
status stays zero while its consumer drains), the one start record is `vite-build`, the blocked dependents are listed and nothing is
captured. `S5-FAILURE-PROOF-PASS draft=retained downstream=0` prints only after all ten pass.
