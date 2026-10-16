# S5 Bun toolchain: the eight web tools

This record describes how the eight web tools run on Bun against a synthetic public probe page, how a failure is reported and
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

## The probe

`web/src/routes/+page.svelte` renders `web/src/lib/toolchain-probe/Probe.svelte`: a Svelte 5 runes component with a native
`<button type="button">` and a count in an `<output>`. It authors no CSS, token, theme or palette: the button and its focus
indicator are the browser's own. Every visible string, and the page title, is a call of a Paraglide-generated message function
(`m.probe_title()`, `m.probe_button_label()`, `m.probe_count_label()`) compiled from `web/messages/en.json`; no source imports
that JSON. The page holds no private data and no access flow. It is prerendered with `ssr = false`, is never embedded or published, and
the access work removes the route and the `toolchain-probe` directory when its first real route lands.

## The eight tools

Each tool is run as `bun <package bin script>` (no Node binary exists and no `node` shim is created), in this order; the first
failure stops the run and nothing after it starts. Every Bun process records its runtime through a `BUN_OPTIONS` preload, so
a tool, its workers and the checks the script itself runs are all counted, and any record that is not the admitted Bun fails
the tool.

| order | id | command | positive assertions |
| --- | --- | --- | --- |
| 1 | `vite-build` | `vite build` (offline Paraglide compile first), then `vite preview` on loopback | exit 0; `build/index.html`; at least one `.js` chunk holding the probe label; the generated `m.probe_title`, `m.probe_button_label` and `m.probe_count_label` are functions returning the three `en.json` values (a warning-only or zero-message compile fails here); preview serves the SvelteKit shell |
| 2 | `svelte-check` | `svelte-kit sync`, then `svelte-check --output machine --threshold warning --fail-on-warnings` | sync first; `COMPLETED n FILES 0 ERRORS 0 WARNINGS` with `n >= 3` |
| 3 | `eslint` | `eslint --max-warnings 0 --format json .` | at least 5 files, 0 errors and warnings; a planted unused variable on stdin is rejected (exit 1, `no-unused-vars`) |
| 4 | `prettier` | `prettier --check` over `src`, `e2e`, `messages` and the config files | exit 0 and "All matched files use Prettier code style!" (the status is checked before the output is read) |
| 5 | `vitest-browser` | `vitest run --reporter=json` (`browser.headless: true`, no channel) | at least 2 tests, all passed, 0 failed, 0 skipped, in the real `chromium-headless-shell` (user agent `HeadlessChrome`) |
| 6 | `vite-dev` | `vite dev --host 127.0.0.1 --port 5173 --strictPort` | serves loopback and transforms `Probe.svelte` into a Svelte client module; stays up for step 7 |
| 7 | `playwright-axe` | `playwright test` against dev and preview (`headless: true`, no channel) | 5 tests (1 worker identity + 4 maps), 0 failed, 0 skipped, 0 flaky; per map: count 0, pointer click 1, Enter 2, labelled button focused and `:focus-visible` with a visible user-agent focus indicator (outline style not `none`), axe violations 0, no console or page error; four capture files exist |
| 8 | `sv` | `sv create <tmp> --template minimal --types ts --no-add-ons --no-install --no-dir-check`, then `svelte-kit sync` and `svelte-check` there | generated `package.json`, `vite.config.ts`, `tsconfig.json`, `src/routes/+page.svelte` and `typescript`; the generated project checks with 0 errors and 0 warnings |

`sv create` uses its built-in `minimal` template (no remote template). The generated project's default adapter package is not
an admitted dependency, so the disposable copy names the admitted `@sveltejs/adapter-static` instead; its packages are linked
from `web/node_modules` one by one.

Facts the probe configuration rests on (SvelteKit 3.0.1, read from the installed source): configuration lives in the
`sveltekit({...})` plugin options in `web/vite.config.ts`, and a `svelte.config.js` is a hard error
(`config_file_unsupported`), so that allowlisted path is intentionally absent; `$lib` is removed in favour of `#lib`, which needs a
`package.json` `imports` entry, so the probe imports by relative path; the tsconfig extends `$app/tsconfig`.

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
(1) `Probe.svelte` gets a genuine syntax error whose site carries the selected synthetic sentinel, so the real `vite build` exits
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
