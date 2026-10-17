# S1 parser spike: attributed corpora, the native quality parser and the movie title parser

Status: quality parsing for the series and movie sides, movie title parsing (`ParseTitle` with `KindMovie`, with the language and
release group parsing it runs) and custom-format compilation, evaluation and batch evaluation are implemented. Quality and movie
title parsing are checked against the complete extracted source test corpora. Series title parsing and the production latency
benchmark are later work and are not present in `internal/parse` yet (`ParseTitle` with `KindSeries` is an explicit
`kind_unsupported` error). Nothing here is a performance claim and no ARM latency claim is made.

## What exists

| Part | Path | Role |
| --- | --- | --- |
| Native parser | `internal/parse/parser.go`, `limits.go`, `regex.go`, `vocab.go`, `quality_series.go`, `quality_movie.go`, `title.go`, `title_movie.go`, `language_movie.go`, `releasegroup_movie.go` | `NewParser`, `ParseQuality`, `ParseTitle`, bounded regexp2 matching, explicit code maps, the two ported quality parsers and the ported movie title, language and release group parsers |
| Format evaluator | `internal/parse/formats.go` | `CompileFormats`, `Evaluate`, `EvaluateBatch`, typed `FormatResult`/`ReleaseResult`; the compiled payload never leaves the `Parser` |
| Corpus contract | `internal/parse/corpus/corpus.go` | Header/Case/Value schema, bounded JSONL `Read`/`Marshal`, count reconciliation, dispositions, `TimeContext.Materialize` |
| Extractor | `internal/parse/corpus/extract/main.go` | Reads the reference C# test text (never executes it) and writes the two corpora: quality cases on both sides, title cases on the movie side |
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

n, err := p.ParseTitle(ctx, "Movie.Name.2015.German.DL.1080p.BluRay.x264-GRP", parse.KindMovie)
// n.Kind == parse.KindMovie; n.Movie.MovieTitles == []string{"Movie Name"}, n.Movie.Year == 2015,
// n.Movie.ReleaseGroup == "GRP", n.Movie.Languages == []string{"german", "original"}, n.Movie.Quality.Code == "bluray-1080p"
```

`KindMovie` runs the movie port and `KindSeries` the series port of `ParseQuality`; `ParseTitle` has only the movie branch. A
`*Parser` is immutable and safe for concurrent use.

### Limits and errors

Validation runs before any pattern is matched; a rejected input never reaches the budget clock.

| Condition | `ReasonError` code | Numeric params |
| --- | --- | --- |
| name longer than 4096 UTF-8 bytes (4096 admitted) | `release_name_too_long` | `limit_bytes`, `observed_bytes` |
| name is not valid UTF-8 | `release_name_invalid_utf8` | `offset` |
| kind is neither `KindMovie` nor `KindSeries` (`ParseQuality`), or is not `KindMovie` (`ParseTitle`; the series title parser lands later) | `kind_unsupported` | `kind` |
| one 250 ms elapsed-work budget exhausted (also after a late successful match, and for a blank or unmatched name: the budget and context are checked once more before any usable value is returned) | `release_work_budget_exhausted` | `limit_ms` |
| regexp2 match timeout (fixed 50 ms) | `match_timeout` | `limit_ms` |
| regexp2 backtracking stack limit (library default) | `regex_stack_limit` | none |
| context cancelled / deadline exceeded | `evaluation_cancelled` / `decision_deadline_exceeded` | none |
| a version digit the source's `Convert.ToInt32` would throw on; a title the source's `ParseMovieTitle` returns null for | `title_unrecognized` | none |
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

## Movie titles

`ParseTitle(ctx, name, parse.KindMovie)` ports the movie reference's `ParseMovieTitle` (`parser/Parser.cs`, with
`parser/ReleaseGroupParser.cs`, `parser/LanguageParser.cs` `ParseLanguages` and the `parser/ParserCommon.cs` cleanup patterns) at
commit `df2faa64c75e6081556d11232dc46523e7264ea1`. Semantic pattern order and pattern text follow the source; the tables compile
once with the `Parser`. It returns `ParsedTitle{Kind, Movie *MovieInfo}`; `MovieInfo` carries `MovieTitles`, `Languages`
(Boxset codes from the explicit member table in `vocab.go`, for example `portuguese-br`), `OriginalTitle`, `ReleaseTitle`,
`SimpleReleaseTitle`, `ReleaseGroup`, `ReleaseHash`, `Edition`, `ImdbID`, `HardcodedSubs`, `Quality`, `Year` and `TmdbID`. The
primary title is `MovieTitles[0]`; an absent source value is the empty string or 0.

Fail closed, one budget. The name is validated and the kind checked before any matching. One 250 ms budget, created by
`ParseTitle`, covers the title patterns and every subparser it runs (the quality, language, release group, hardcoded-subs,
edition and ID parsing): none starts a budget of its own, and the budget and context are checked before and after every match,
the replacement helpers included, and once more before a result is returned. `TestBudgetMovieTitleIsSharedByEverySubparser` sweeps
every clock read of `ParseTitle` from 2 to its last: for each k a clock with a one-read blip returns a time past the budget at read k
only, and the parse must stop with the budget reason, no value and exactly k reads (no work after the exhaustion). A stage that
started a budget of its own reads the clock to create it; at that read the blip puts the new deadline a whole budget later, so the
parse finishes instead of failing and the test catches it.
`TestHostileMovieTitleMatchInAnySubparserFailsClosed` injects a catastrophic pattern into each of the title, quality, language
and release group tables in turn and gets `match_timeout` with no value; a 4096-byte legitimate name parses (`TestMovieBounds...`) and
4097 bytes, invalid UTF-8, an unsupported kind, a hashed release and a cancelled or expired context are structured errors with the
zero value. The source's own generated-name tests (an MD5 chain, random 32- and 40-character hashes) are reproduced against
`ParseTitle` (`TestHostileMovieTitleRejectsGeneratedHashNames`) because the corpus cannot record generated inputs. The source asserts
no `ReleaseHash`, `ReleaseTitle` or `SimpleReleaseTitle`, so `TestMovieCorpusPublicParseTitleReleaseFields` fixes them by hand
through the public `ParseTitle` (an anime hash, a `1280x720` name that has no hash, a non-ASCII title index), and the language
member table is checked against an independent literal table in `TestLanguageVocabularyMatchesIndependentMemberTable`.

Measured limit (one dev-image run): a 4096-byte name whose bulk is a trailing dotted word parsed in about 18 ms. A name whose
title, or whose German tag, is thousands of characters long exceeds the 50 ms match timeout in the source's unanchored
bracketed-alternative pattern (about 290 ms for 4000 characters when it runs without a timeout) or its German-remux quality
pattern (about 700 ms) and fails closed with `match_timeout`; the source has no timeout and would return a result.

Faithful-adapter decisions are listed in `docs/parity-matrix.md` (null results, the `isDir` directory form, the reversed-title
marker, the file extension helper, UTF-16 indexes, language members).

## Custom formats: compile, evaluate, batch

```go
specs := []parse.FormatSpec{{
	ID: "hdtv-720p",
	Conditions: []parse.ConditionSpec{
		{Pattern: `720p`, Required: true},
		{Pattern: `hdtv`, Required: true},
		{Pattern: `\bnuked\b`, Negate: true, Required: true},
	},
}}
if err := p.CompileFormats(ctx, 1, specs); err != nil { /* *parse.ReasonError: correct the profile or use a higher revision */ }

results, err := p.Evaluate(ctx, "Show.S01E01.720p.HDTV.x264-GRP")
// results[0] == parse.FormatResult{ID: "hdtv-720p", Outcome: parse.OutcomeMatched}; on any error results is nil

rows, err := p.EvaluateBatch(ctx, names)
if err != nil { /* terminal: rows are diagnostic only, authorize nothing */ }
for _, r := range rows { /* r.Index, r.State (Completed|Rejected|Unscheduled), r.Formats, r.Reason */ }
```

Matching. A format matches when every `Required` condition holds and, if it has any non-required condition, at least one of
those holds; `Negate` inverts one condition. A format with no conditions matches nothing. Patterns are .NET-style regexp2
sources and compile case-insensitively (use `(?-i)` to opt out). Every condition of every format is evaluated, even when the
outcome is already decided, so an error in any condition fails the whole evaluation instead of being skipped behind an earlier
result. `OutcomeIndeterminate` exists in the typed vocabulary but is never returned in a usable list: an evaluation that did not
complete is an error (single) or a `Rejected` row (batch), never a format outcome.

Fail closed. `Evaluate` returns the complete ordered list or `(nil, *ReasonError)`. A match timeout, the backtracking stack
limit, an exhausted budget or a cancelled context is an error; neither negation nor an earlier match turns it into a match, and
a match that completes after the budget expired (checked after every successful match and once more after the last format) is an
error too. One 250 ms budget covers the whole evaluation; the regexp2 timeout is fixed at 50 ms.

| Condition | `ReasonError` code | Numeric params |
| --- | --- | --- |
| more than 256 formats | `too_many_formats` | `limit`, `observed` |
| more than 32 conditions in a format | `too_many_conditions` | `format_index`, `limit`, `observed` |
| regex source over 8192 UTF-8 bytes | `regex_source_too_long` | `format_index`, `condition_index`, `limit_bytes`, `observed_bytes` |
| format ID over 64 UTF-8 bytes / not valid UTF-8 | `format_id_too_long` / `format_id_invalid_utf8` | `format_index`, `limit_bytes`, `observed_bytes` / `offset` |
| regex source not valid UTF-8 or not compilable | `regex_compile_failed` | `format_index`, `condition_index` |
| same revision, different content / lower revision | `format_revision_conflict` / `format_revision_stale` | none |
| `Evaluate`/`EvaluateBatch` before any `CompileFormats` | `format_not_configured` | none |
| two evaluations already admitted | `evaluator_busy` | `limit` |
| batch of more than 1024 names / over 4 MiB of names | `too_many_releases` / `batch_name_bytes_exceeded` | `limit`, `observed` / `limit_bytes`, `observed_bytes` |
| a batch name over 4096 bytes or not valid UTF-8 | `release_name_too_long` / `release_name_invalid_utf8` | the name params plus `index` |
| 30 s batch decision deadline reached | `decision_deadline_exceeded` | `limit_ms` |

All counts, sizes and encodings are checked before anything is compiled or matched (for a profile, in format then condition
order, so the first violation by index is reported); the reasons never carry an ID, a pattern, a name or a dependency error, and
`errors.As(err, &re)` reaches the `*ReasonError` through wrapping. Match, budget, stack, cancellation and deadline codes are the
ones in the limits table above.

Revisions. `CompileFormats(ctx, revision, specs)` takes a revision that never decreases. The same revision with the same
content is a no-op (nothing is recompiled); changed content needs a higher revision. A profile is compiled once per revision,
completely, and only then published; any failure leaves the previous profile current. The compiled payload is private to the
`Parser`. `Evaluate` and `EvaluateBatch` pin the profile that is current at entry; at most two evaluations are admitted at once
(a third gets `evaluator_busy`, there is no queue), so publication can never leave more than two replaced profiles alive, and
each is reclaimed with its last reader, on success, error or cancellation alike. Retained format source is a private copy.

Batch. `EvaluateBatch` validates the count (1024), aggregate bytes (4 MiB) and every name before any work and allocates its
output once; a validation, configuration or admission error returns nil rows. The whole batch uses the profile pinned at entry.
Each release gets a fresh 250 ms budget under one 30 s decision deadline, and the result has exactly one row per name at that
name's index. A release that fails on its own (its match timeout, stack limit or budget) is `Rejected` with its reason and no
formats, and the batch continues. Cancellation or the 30 s deadline is terminal: the release in flight and every later one are
`Unscheduled` carrying the terminal reason, and the terminal error is returned with the rows. With any non-nil error every row,
earlier `Completed` ones included, is diagnostic only and must not authorize an effect. The output backing is bounded by the
admitted maxima (a compile-time check keeps it far under the 64 MiB cap), so no runtime output-cap reason exists.

Timeout overshoot. regexp2 checks its timeout against a coarse clock, so a running match can overshoot the 50 ms timeout and no
hard deadline is claimed. `TestTimeoutOvershootReport` (excluded under `-race`) runs 100 real default-clock evaluations of a
catastrophic pattern and logs the distribution; one run in the dev image logged elapsed beyond the 50 ms timeout of min 147.7 ms,
median 150.4 ms, p99 150.7 ms, max 150.7 ms. That is a measurement of one run, not a bound.

```sh
scripts/dev-run.sh -- go test ./internal/parse -run '^TestTimeoutOvershootReport$' -count=1 -v
```

## Corpus contract

Each corpus is JSONL: a `Header` (schema 1, app `series` or `movie`, source locator `env:BOXSET_REF_SERIES` or `env:BOXSET_REF_MOVIE`, commit, source paths, licence `GPL-3.0`, counts), then one `Case`
per source test case with its file, method, entry (`TestCase#n`, `TestCaseSource:name#n`, `Test`), source line, upstream
status, scope, arguments exactly as written and the asserted result properties. `expected["$input"]` holds the release names
passed to the scoped entry point (`QualityParser.ParseQuality` for scope `quality`; `Parser.ParseMovieTitle`,
`ReleaseGroupParser.ParseReleaseGroup` or `LanguageParser.ParseLanguages` for scope `title`; a `string` or a symbolic relative
date); every other key is the asserted C# property path (for example `Quality`, `Quality.Resolution`, `Revision.Version`,
`PrimaryMovieTitle`, `MovieTitles[1]`, `Languages.Count`, `Languages.First()`). Title cases add three reserved or derived keys:
`$result` (an assertion on the entry point's own result, `null` for a rejected name), `<list>.Contain` (a membership assertion, the
ordered array of expected members) and `$isDir` (the `isDir` argument of `ParseMovieTitle`, present only when true). Values are tagged `string|bool|int|decimal|enum|array|
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
methods, `TestCase` attributes, `TestCaseSource` rows, plain `[Test]` methods when a file has any, setup methods, commented-out
test forms). A commented-out test is counted, not extracted. Duplicate rows are kept and get distinct identities
(`<app>:<file>:<method>:<input hash>:<ordinal>`); relative dates stay symbolic in identities and bytes and are materialized by
`corpus.TimeContext` with local-calendar `AddDays` (never a 24-hour duration).

Title scope (movie side). The extractor reads seven title fixtures (`ParserFixture`, `EditionParserFixture`,
`LanguageParserFixture`, `CrapParserFixture`, `HashedReleaseFixture`, `ReleaseGroupParserFixture`, `UrlFixture`) and classifies
each test method by the identifiers of its body: a method that asserts one of the three title entry points becomes scope
`title`; every other method is retained as scope `out_of_scope` under its own entry point with its arguments, an empty `$input`
and no assertion, and its body is never interpreted (`docs/parity-matrix.md` lists the owners and reasons). A method that names
no known entry point rejects the extraction. The assertion forms are `Should().Be`, `Contain` and `BeNull`, `for` loops bounded
by an already asserted count, indexers, `First()`, `.Id` on a language member, `using (new AssertionScope())` and `new[] {…}`
arrays. The `Language` declaration is outside the delivered areas (`languages/` is study-only), so the members an assertion may
name are those `parser/LanguageParser.cs` itself names; the ported sources (`Parser.cs`, `ReleaseGroupParser.cs`,
`ParserCommon.cs`, `RegexReplace.cs`, `Model/ParsedMovieInfo.cs`) are recorded as attributed source paths with their hashes and
are never interpreted. The series side extracts no title case until its title port lands, so `series.jsonl` is unchanged.

An unsupported source form (loops other than `foreach` and a counted `for`, other `using` statements, unknown class or method
attributes, unsupported assertions, interpolated strings, properties, a lone UTF-16 surrogate, …) fails extraction with
`extract: unsupported app=… file=… line=… col=… reason=…`; it is never skipped or relabelled, and no partial output is published.

### Regenerate and verify

The two reference repositories are located only by the host values `BOXSET_REF_SERIES` and `BOXSET_REF_MOVIE` (absolute paths of
existing directories that hold the Git repositories of the series and movie references, each containing its pinned commit); no
repository location is written into the repository.

```sh
scripts/dev-run.sh --ref --write testdata/parser -- sh -c 'exec go run ./internal/parse/corpus/extract -series /ref/series -movie /ref/movie -series-commit "$BOXSET_REF_SERIES_COMMIT" -movie-commit "$BOXSET_REF_MOVIE_COMMIT" -out testdata/parser'
scripts/dev-run.sh -- go test ./internal/parse/corpus/... ./internal/parse -run '^(TestCorpus|TestExtract|TestQualityCorpus|TestQualityBounds|TestMovie|TestTitle)' -count=1 -v
scripts/dev-run.sh -- scripts/parse-emulated.sh '^(TestQualityCorpus|TestMovieCorpus)$'   # arm64 and arm/v7 under QEMU user mode
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
success is claimed. The title section of `movie.jsonl` was produced by running this extractor over a Git-object extraction of
the movie reference's `parser/`, `tests/` and `qualities/` areas at the pinned commit (the same three areas `--ref` delivers, in
a scratch container with no network and no mount of any checkout), not by `--ref`; the quality records are byte-identical to
the earlier extraction, repeated extraction is byte-identical, and `series.jsonl` was not regenerated. A Go consumer run counts for the same immutable runtime payload as an admission run only by the payload
comparison in `docs/spikes/s5-version-admission.md` (ordered layers, `Config` SHA-256, platform, `inputs=<sha>`), never by the image ID.

`TestCorpusFull` pins the exact per-file inventories and source commits; changing a reference commit means re-extracting and
updating that table together.
