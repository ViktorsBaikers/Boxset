# S1 parser spike: attributed quality corpora and the native quality parser

Status: quality parsing for the series and movie sides is implemented and checked against the complete extracted source test
corpora. Title parsing, custom-format evaluation, batch evaluation and the production latency benchmark are later work and
are not present in `internal/parse` yet. Nothing here is a performance claim and no ARM latency is claimed.

## What exists

| Part | Path | Role |
| --- | --- | --- |
| Native parser | `internal/parse/parser.go`, `limits.go`, `regex.go`, `vocab.go`, `quality_series.go`, `quality_movie.go` | `NewParser`, `ParseQuality`, bounded regexp2 matching, explicit code maps, the two ported quality parsers |
| Corpus contract | `internal/parse/corpus/corpus.go` | Header/Case/Value schema, bounded JSONL `Read`/`Marshal`, count reconciliation, dispositions, `TimeContext.Materialize` |
| Extractor | `internal/parse/corpus/extract/main.go` | Reads the reference C# test text (never executes it) and writes the two corpora |
| Corpora | `testdata/parser/series.jsonl`, `movie.jsonl`, `dispositions.json` | Attributed, counted, committed inputs and the (currently empty) owner dispositions |
| Emulation | `scripts/parse-emulated.sh` | Cross-compiled tests under `qemu-aarch64` and `qemu-arm` |
| Runner | `scripts/dev-run.sh --ref`, `--write testdata/parser` | Pinned Git-object reference text delivered by copy into the created container; fixed corpus write table with a raw-to-sanitized identity check |
| Producer | `deploy/dev/redact/ref_archive.go` | Private `-ref-archive` mode of the host redactor: lists and reads only the pinned Git objects and writes a bounded archive |

Sources (GPL-3.0; the corpus headers and the ported Go files carry only the licence identifier GPL-3.0 and name no project):
the series reference commit `57816c149d02f95543973fefb4780c4495f8121c` and the movie reference commit `df2faa64c75e6081556d11232dc46523e7264ea1`.
Dependency: `github.com/dlclark/regexp2/v2` v2.8.4, exact pin in `go.mod`/`go.sum`.

## Use

```go
p, err := parse.NewParser(parse.Options{}) // nil Now/Location select time.Now/time.Local
if err != nil { /* *parse.ReasonError: a built-in pattern failed to compile */ }

q, err := p.ParseQuality(ctx, "Movie.Title.2016.REMUX.1080p.BluRay.AVC.DTS-HD.MA.5.1-iFT", parse.KindMovie)
// q.Code == "remux-1080p", q.Source == "bluray", q.Modifier == "remux", q.Resolution == 1080,
// q.SourceDetection == "name", q.Revision == parse.Revision{Version: 1}
```

`KindMovie` runs the movie port and `KindSeries` the series port. A `*Parser` is immutable and safe for concurrent use.

### Limits and errors

Validation runs before any pattern is matched; a rejected input never reaches the budget clock.

| Condition | `ReasonError` code | Numeric params |
| --- | --- | --- |
| name longer than 4096 UTF-8 bytes (4096 admitted) | `release_name_too_long` | `limit_bytes`, `observed_bytes` |
| name is not valid UTF-8 | `release_name_invalid_utf8` | `offset` |
| kind is neither `KindMovie` nor `KindSeries` | `kind_unsupported` | `kind` |
| one 250 ms elapsed-work budget exhausted (also after a late successful match, and for a blank or unmatched name: the budget and context are checked once more before any usable value is returned) | `release_work_budget_exhausted` | `limit_ms` |
| regexp2 match timeout (fixed 50 ms) | `match_timeout` | `limit_ms` |
| regexp2 backtracking stack limit (library default) | `regex_stack_limit` | none |
| context cancelled / deadline exceeded | `evaluation_cancelled` / `decision_deadline_exceeded` | none |
| a version digit the source's `Convert.ToInt32` would throw on | `title_unrecognized` | none |
| a built-in pattern fails to compile (constructor) | `regex_compile_failed` | `pattern_index` |

```go
var re *parse.ReasonError
if errors.As(err, &re) { switch re.Reason.Code { case "release_name_too_long": /* ... */ } }
```

Handle by `Reason.Code`; the error text is fixed and never echoes the name, a pattern or a dependency error. On any error the
returned `Quality` is the zero value. The budget covers every nested helper of one call, is checked before and after each
match, and the regexp2 timeout is approximate (its default clock ticks about every 100 ms), so a running match can overshoot;
no hard deadline is claimed.

### Quality fields and code maps

`Quality{Code, Source, Resolution, Modifier, Revision, SourceDetection, ResolutionDetection, ModifierDetection,
RevisionDetection}`. Every source quality, source, modifier and detection member maps to a Boxset code in `vocab.go` through
an explicit table; nothing is derived from enum ordinals. `Source`, `Resolution` and `Modifier` were added to the planned
`Quality` shape because the movie tests assert them (including the TELESYNC resolution, which the source sets per release).
The series side has no modifier, so its `Modifier`/`ModifierDetection` stay empty.

Faithful-adapter decisions: `Trim`/whitespace follow `char.IsWhiteSpace`; `Path.GetInvalidPathChars` and `Path.GetExtension`
follow Linux (only NUL is invalid, only `/` separates); `ContainsIgnoreCase` is used only with ASCII needles; extension
lookups compare with per-rune upper-casing (ordinal ignore case). regexp2 runs on runes where .NET runs on UTF-16 units; they
agree on the Basic Multilingual Plane.

## Corpus contract

Each corpus is JSONL: a `Header` (schema 1, app `series` or `movie`, source locator `env:BOXSET_REF_SERIES` or `env:BOXSET_REF_MOVIE`, commit, source paths, licence `GPL-3.0`, counts), then one `Case`
per source test case with its file, method, entry (`TestCase#n`, `TestCaseSource:name#n`, `Test`), source line, upstream
status, scope, arguments exactly as written and the asserted result properties. `expected["$input"]` holds the release names
passed to `QualityParser.ParseQuality` (a `string` or a symbolic relative date); every other key is the asserted C# property
path (for example `Quality`, `Quality.Resolution`, `Revision.Version`). Values are tagged `string|bool|int|decimal|enum|array|
null|date`; an unknown tag, field or form rejects the file. Source paths, count keys and each case's `file` use the form `<area>/<file>`
(`tests/`, `parser/`, `qualities/`, relative to `/ref/<side>`). One shared validator, used by both `Read` and `Marshal`, enforces
the app's own locator, the licence exactly `GPL-3.0`, canonical paths in the header, the count keys and every case, every
contained value (decimal text, enum type and member identifiers, a date template the materializer supports and an offset within
±36500 days, recursively through arrays), and at most 32
nesting levels in both directions (exactly 32 is admitted, 33 is not). Every `a`/`A` immediately before a case-insensitive `rr` inside a tagged
string value is written as the JSON escape `\u0061`/`\u0041` (decoded values, input hashes and identities are unchanged), so fixture
bytes carry no upstream project name. Caps: 32 MiB per file, 256 KiB per record, 32 nesting levels,
streaming; oversize, partial (unterminated record or non-reconciling counts) or malformed input yields no usable case.

Counts reconcile per file: `cases = enabled + disabled + out_of_scope`, plus the extractor's raw tallies under `source:` (test
methods, `TestCase` attributes, `TestCaseSource` rows, setup methods, commented-out test forms). A commented-out test is counted,
not extracted. Duplicate rows are kept and get distinct identities
(`<app>:<file>:<method>:<input hash>:<ordinal>`); relative dates stay symbolic in identities and bytes and are materialized by
`corpus.TimeContext` with local-calendar `AddDays` (never a 24-hour duration).

An unsupported source form (loops other than `foreach` over an array, unknown class or method attributes, unsupported assertions,
interpolated strings, properties, a lone UTF-16 surrogate, …) fails extraction with `extract: unsupported app=… file=… line=… col=…
reason=…`; it is never skipped or relabelled.

### Regenerate and verify

The two reference repositories are located only by the host values `BOXSET_REF_SERIES` and `BOXSET_REF_MOVIE` (absolute paths of
existing directories that hold the Git repositories of the series and movie references, each containing its pinned commit); no
repository location is written into the repository.

```sh
scripts/dev-run.sh --ref --write testdata/parser -- sh -c 'exec go run ./internal/parse/corpus/extract -series /ref/series -movie /ref/movie -series-commit "$BOXSET_REF_SERIES_COMMIT" -movie-commit "$BOXSET_REF_MOVIE_COMMIT" -out testdata/parser'
scripts/dev-run.sh -- go test ./internal/parse/corpus/... ./internal/parse -run '^(TestCorpus|TestExtract|TestQualityCorpus|TestQualityBounds)' -count=1 -v
scripts/dev-run.sh -- scripts/parse-emulated.sh '^TestQualityCorpus$'   # arm64 and arm/v7 under QEMU user mode
```

`--ref` reads reference text only as Git objects at the runner's pinned commits (`REFPINS` in `scripts/dev-run.sh`: series
`57816c149d02f95543973fefb4780c4495f8121c`, movie `df2faa64c75e6081556d11232dc46523e7264ea1`); it never walks, copies or
mounts a checkout directory, and the checkout's HEAD, index and working tree are never read. Both roots are validated, series
first, before any Git or Docker call (an absolute path to an existing directory without control characters); each pin is then
checked with a bounded (30 s) `git rev-parse --verify '<pin>^{commit}'` under a cleared environment whose discovery cannot
leave the root, with no replace refs, no lazy fetch and no system configuration. After the container is created and before it
starts, the host redactor's private `-ref-archive` mode lists the three upstream directories at the pin (`src/NzbDrone.Core.Test/ParserTests` as
`/ref/<side>/tests`, `src/NzbDrone.Core/Parser` as `parser`, `src/NzbDrone.Core/Qualities` as `qualities`), validates the whole
listing (regular blobs only, `.cs` names, at most 256 files and 256 directories per area, 2 MiB per file, 16 MiB per side)
before reading any blob, and streams a deterministic archive of at most 18 MiB (the side plus its tar framing) into the container's six empty root-owned read-only directories
with `docker cp -`. The runner checks the producer's status apart from the receiver's, reads `/ref/<side>` back from the
unstarted container through the same 18 MiB cap, and hands the two checked pins to the child as `BOXSET_REF_SERIES_COMMIT` and
`BOXSET_REF_MOVIE_COMMIT` (the only reference values the child receives; roots never enter the image, a log, a mount or the
child). A failure exits 125 and prints only `dev-run: <reason> side=<series|movie>` (or `dev-run: export_changed`); never a path or
Git text:

| Reason | Recovery |
| --- | --- |
| `ref_unavailable` | The owner variable of that side is unset or empty: export `BOXSET_REF_SERIES`/`BOXSET_REF_MOVIE` as absolute paths. |
| `ref_invalid` | The value is relative, not an existing directory or holds a control character; or, from the producer, the listing of the pinned tree breaks a source rule (a missing upstream directory, a link, submodule or non-`.cs` entry, a name outside the allowed alphabet, nesting over 32, an area over 256 files or directories, a file over 2 MiB or a side over 16 MiB). Fix the path or choose a repository that holds the pinned tree. |
| `ref_commit_invalid` | The pinned commit is not a commit of that repository: `git` is missing from `PATH`, the root is not itself a Git repository root (a directory nested inside another repository does not count), the object is absent or not a commit, Git failed or exceeded 30 s, or Git refused the directory's ownership. |
| `ref_copy_failed` | The producer or `docker cp` failed or timed out, the producer status or the receiver status was not 0 (even after a readable archive was really delivered), the readback is larger than the producer's 18 MiB archive cap, or the delivered `/ref/<side>` is not exactly files and directories under the three areas owned by 0/0 with modes 0444/0555 and at least one file per area. Check the repository, Git and Docker, then rerun. |
| `export_changed` | With `--write testdata/parser` the stopped container's raw corpus file does not hash as the sanitized staged bytes (the redactor changed it, for example a value taken from a secret-named environment variable). Nothing was published; remove that text from the corpus source or the environment and rerun. |

The extractor reads the delivered text through `os.Root` (no path or link leaves `/ref/<side>`), keeps at most 2 MiB + 1 actual
bytes per file (a larger stream is never consumed beyond that; it rejects the file with its location), and bounds its interpreter before each step or append: 4,194,304
steps, 65,536 observations per side and the 32 MiB corpus cap on expanded bytes. Converting a recorded value (an argument, an
assertion or an entry-point input) charges every node to that same step and byte budget and checks the 14 array levels the
32-level record admits before the node is built, so a compact shared array that expands exponentially is rejected with its
location, not built. A date value the corpus cannot write or read (an unsupported format such as `ToString("HH")`, an offset beyond
±36500 days or one that overflows) also rejects with its location, before either corpus is written. C# string escapes decode UTF-16 surrogate pairs
faithfully and reject a lone surrogate; an unknown class or method attribute rejects with its location while `Ignore`/`Explicit`
are retained. The extractor opens each corpus
with truncate, checks every write and close, and prints `extract: write_failed file=<name>` (exit 1) on any write error; the
runner publishes the two names only when the child exited 0, from the stopped container, through the redactor, within the 32 MiB
cap, never from a link or non-regular entry, and only when each raw file hashes as its sanitized staged bytes. Repeated
extraction at fixed inputs reproduces identical bytes; the extractor prints
each source file's byte count and SHA-256 and each corpus's counts, size and SHA-256 for the evidence record.

Proof status. Runtime checks in this report are the parser, corpus and extractor unit tests, the producer's `TestRefArchive` tests, the
whole-module linter and `gofmt`, and the runner smoke's synthetic-repository controls (pinned objects against later HEAD,
staged, working-tree and untracked bytes, source-rule rejections, producer failure and receiver failure after a real delivery,
the 18 MiB readback bound, corpus export
identity). NOT-RUN here: extraction from the real reference repositories, which needs the two owner locations and is owned by the
root verification; until it runs, the
committed corpora are the retained extraction (their per-file hashes are recorded in the feature evidence), and no source
success is claimed. A Go consumer run counts for the same immutable runtime payload as an admission run only by the payload
comparison in `docs/spikes/s5-version-admission.md` (ordered layers, `Config` SHA-256, platform, `inputs=<sha>`), never by the image ID.

`TestCorpusFull` pins the exact per-file inventories and source commits; changing a reference commit means re-extracting and
updating that table together.
