# S1 parser spike: frozen fixtures, the native quality parser and the movie and series title parsers

Status: quality parsing for the series and movie sides, title parsing for both kinds (`ParseTitle` with `KindMovie` or `KindSeries`,
with the language and release group parsing each runs) and custom-format compilation, evaluation and batch evaluation are
implemented. Quality and title parsing are checked against the complete frozen fixture tables, including the series
episode title cases (daily, absolute, anime, multi-episode, season pack, special, Unicode, title-only) in
`internal/parse/title_series.go`, `language_series.go` and `releasegroup_series.go`. The production latency benchmark and its
eligibility judge are implemented (see "Production latency benchmark"); no eligible measurement has been run here, so nothing in
this report is a performance claim, and no ARM latency claim is made.

## What exists

| Part | Path | Role |
| --- | --- | --- |
| Native parser | `internal/parse/parser.go`, `limits.go`, `regex.go`, `vocab.go`, `quality_series.go`, `quality_movie.go`, `title.go`, `title_movie.go`, `title_series.go`, `language_movie.go`, `language_series.go`, `releasegroup_movie.go`, `releasegroup_series.go` | `NewParser`, `ParseQuality`, `ParseTitle`, bounded regexp2 matching, explicit code maps, the two quality parsers and the movie and series title, language and release group parsers |
| Format evaluator | `internal/parse/formats.go` | `CompileFormats`, `Evaluate`, `EvaluateBatch`, typed `FormatResult`/`ReleaseResult`; the compiled payload never leaves the `Parser` |
| Fixture contract | `internal/parse/corpus/corpus.go` | Header/Case/Value schema 2, bounded JSONL `Read`/`Marshal`, dispositions (format version 1), `TimeContext.Materialize` |
| Fixtures | `testdata/parser/series.jsonl`, `movie.jsonl`, `dispositions.json` | The asserted cases (series 1522, movie 836) and the (currently empty) owner dispositions; new cases are added by hand |
| Emulation | `scripts/parse-emulated.sh` | Cross-compiled tests under `qemu-aarch64` and `qemu-arm` |
| Runner | `scripts/dev-run.sh` | Fixed-user, network-less container with fixed-name draft and benchmark write maps |
| Benchmark | `bench/parser/main.go`, `eligibility.go` | Production per-call latency CLI (`-out`) and the final judge (`-judge`); the host-evidence schema and its eligibility rules |
| Benchmark runner | `scripts/dev-run.sh --bench CPUSET`, `scripts/dev-run-smoke.sh --benchmark-evidence` | Reference-profile child limits, read-only `/config`, host-written before/during/after records; the evidence controls |

Licence: parser source files carry only the licence identifier GPL-3.0 and name no project.
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

`KindMovie` runs the movie parsers and `KindSeries` the series parsers of both `ParseQuality` and `ParseTitle`; a series name parses to
`ParsedTitle{Kind: KindSeries, Episode: *EpisodeInfo}` (for example `Series.Title.S03E06.1080p.BluRay.x264-GRP` has
`Episode.SeriesTitle == "Series Title"`, `SeasonNumbers == []int{3}` and `EpisodeNumbers == []int{6}`). A
`*Parser` is immutable and safe for concurrent use.

### Limits and errors

Validation runs before any pattern is matched; a rejected input never reaches the budget clock.

| Condition | `ReasonError` code | Numeric params |
| --- | --- | --- |
| name longer than 4096 UTF-8 bytes (4096 admitted) | `release_name_too_long` | `limit_bytes`, `observed_bytes` |
| name is not valid UTF-8 | `release_name_invalid_utf8` | `offset` |
| kind is neither `KindMovie` nor `KindSeries` (`ParseQuality`, `ParseTitle`) | `kind_unsupported` | `kind` |
| one 250 ms elapsed-work budget exhausted (also after a late successful match, and for a blank or unmatched name: the budget and context are checked once more before any usable value is returned) | `release_work_budget_exhausted` | `limit_ms` |
| regexp2 match timeout (fixed 50 ms) | `match_timeout` | `limit_ms` |
| regexp2 backtracking stack limit (library default) | `regex_stack_limit` | none |
| context cancelled / deadline exceeded | `evaluation_cancelled` / `decision_deadline_exceeded` | none |
| a version digit that does not parse as an integer; a name with no title (a daily name whose date is not a calendar date, or lies beyond the 14 day air date limit, included) | `title_unrecognized` | none |
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
RevisionDetection}`. Every quality, source, modifier and detection member maps to a Boxset code in `vocab.go` through
an explicit table; nothing is derived from enum ordinals. `Source`, `Resolution` and `Modifier` were added to the planned
`Quality` shape because the movie fixtures assert them (including the TELESYNC resolution, which is set per release).
The series side has no modifier, so its `Modifier`/`ModifierDetection` stay empty.

Implementation decisions: trimming follows Unicode white space; only NUL is an invalid path character and only `/` separates
when a file extension is read (Linux); the case-insensitive substring test is used only with ASCII needles; extension
lookups compare with per-rune upper-casing (ordinal ignore case). regexp2 runs on runes while indexes are counted in UTF-16
units; they agree on the Basic Multilingual Plane.

## Movie titles

`ParseTitle(ctx, name, parse.KindMovie)` parses a movie release name: the name preparation, the title patterns in their
semantic order, the release group, language, edition and ID parsing and the cleanup patterns. The tables compile
once with the `Parser`. It returns `ParsedTitle{Kind, Movie *MovieInfo}`; `MovieInfo` carries `MovieTitles`, `Languages`
(Boxset codes from the explicit member table in `vocab.go`, for example `portuguese-br`), `OriginalTitle`, `ReleaseTitle`,
`SimpleReleaseTitle`, `ReleaseGroup`, `ReleaseHash`, `Edition`, `ImdbID`, `HardcodedSubs`, `Quality`, `Year` and `TmdbID`. The
primary title is `MovieTitles[0]`; an absent value is the empty string or 0.

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
zero value. Generated names (an MD5 chain, random 32- and 40-character hashes) are run against
`ParseTitle` (`TestHostileMovieTitleRejectsGeneratedHashNames`) because the fixtures cannot record generated inputs. The fixtures assert
no `ReleaseHash`, `ReleaseTitle` or `SimpleReleaseTitle`, so `TestMovieCorpusPublicParseTitleReleaseFields` fixes them by hand
through the public `ParseTitle` (an anime hash, a `1280x720` name that has no hash, a non-ASCII title index), and the language
member table is checked against an independent literal table in `TestLanguageVocabularyMatchesIndependentMemberTable`.

Pattern guards. A pattern is not run on a text its own pattern text shows it cannot match. `NewParser` reads each built-in pattern for
facts every match must satisfy: the fewest characters, the literal texts (with the word boundaries the pattern puts around them)
and the adjacent character pairs, with what a character class matches asked of regexp2 itself; the guard of a pattern with a
start anchor also names the start. A pattern without a usable fact has no guard, and a text that is not ASCII is always left to
regexp2. A pattern ruled out answers as an unmatched regexp would and the budget is still checked before and after it, so the clock
reads, the reasons and the results are those of running it; after a first match the same facts about the rest of the text, or a
start anchor, show that no further match exists. The guards are built with the tables, immutable, hold no result and no cache, and
a pattern replaced after construction (the hostile tests do) has none.

Measured limit (one dev-image run): a 4096-byte name whose bulk is a trailing dotted word parsed in about 18 ms. A name whose
title, or whose German tag, is thousands of characters long exceeds the 50 ms match timeout in the unanchored
bracketed-alternative pattern (about 290 ms for 4000 characters when it runs without a timeout) or its German-remux quality
pattern (about 700 ms) and fails closed with `match_timeout`; without a timeout it would return a result.

The decisions are listed in `docs/parity-matrix.md` (null results, the `isDir` directory form, the reversed-title
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

## Fixture contract

Each fixture is JSONL, schema 2: a `Header` `{"schema":2,"app":"series"|"movie"}`, then one `Case` per asserted case with only
`id` (`<app>:<NNNN>`, the 1-based zero-padded ordinal), `scope` (`quality` or `title`), `scopeOwner` (the entry point the case
runs), `args` (the arguments exactly as written) and `expected` (the asserted result properties). `expected["$input"]` holds
the release names passed to the scoped entry point (`QualityParser.ParseQuality` for scope `quality`; `Parser.ParseMovieTitle`,
`ReleaseGroupParser.ParseReleaseGroup`, `LanguageParser.ParseLanguages` and `Parser.CleanMovieTitle` for movie scope `title`;
`Parser.ParseTitle`, `Parser.ParseSeriesName`, `Parser.CleanSeriesTitle`, `ReleaseGroupParser.ParseReleaseGroup` and
`LanguageParser.ParseLanguages` for series scope `title`; a `string` or a symbolic relative date); every other key is the
asserted property path (for example `Quality`, `Quality.Resolution`, `Revision.Version`, `PrimaryMovieTitle`,
`MovieTitles[1]`, `Languages.Count`, `Languages.First()`). Title cases add three reserved or derived keys: `$result` (an
assertion on the entry point's own result, `null` for a rejected name), `<list>.Contain` (a membership assertion, the ordered
array of expected members) and `$isDir` (the directory argument of the movie title parse, present only when true). Values are
tagged `string|bool|int|decimal|enum|array|null|date`; an unknown tag, field or form rejects the file. One shared validator,
used by both `Read` and `Marshal`, enforces schema 2, the app, unique non-empty ids, the scope (`quality` or `title`), a
non-empty `scopeOwner`, every contained value (decimal text, enum type and member identifiers, a date template the
materializer supports and an offset within ±36500 days, recursively through arrays), and at most 32 nesting levels in both
directions (exactly 32 is admitted, 33 is not). Every `a`/`A` immediately before a case-insensitive `rr` inside a tagged
string value is written as the JSON escape `\u0061`/`\u0041` (decoded values and ids are unchanged), so fixture bytes carry no
upstream project name. Caps: 32 MiB per file, 256 KiB per record, 32 nesting levels, streaming; oversize, partial
(unterminated record) or malformed input yields no usable case.

The fixtures hold exactly the asserted cases: series 1522 (356 quality, 1166 title) and movie 836 (323 quality, 513 title), in
a fixed order. Relative dates stay symbolic in the bytes and are materialized by `corpus.TimeContext` with local-calendar
`AddDays` (never a 24-hour duration). The series fixtures hold the release group `Neutral` wherever a group name would name an
upstream product; `CleanSeriesTitle` output is lower case, so a lower-case expectation is compared with `Neutral` lower-cased.
`TestCorpusFull` locks the schema, the SHA-256 of both files, the per-scope and per-owner counts, the id ordinals, zero
dispositions and that the committed bytes are the canonical `Marshal` encoding.

`testdata/parser/dispositions.json` has its own format version, 1, independent of the fixture schema: `{"schema":1,"dispositions":[]}`
is read by `corpus.ReadDispositions`, which rejects any other version.

### Add a case

New cases are added by hand. Append the case to the fixture of its application with the next ordinal id (`<app>:<NNNN>`; an id
is never reused or renumbered), and keep the file canonical: decode it with `corpus.ReadFile`, append the `corpus.Case`, and
write the `corpus.Marshal` bytes. In the same change update the locked SHA-256 and the per-scope and per-owner counts in
`internal/parse/corpus/corpus_full_test.go`, then run:

```sh
scripts/dev-run.sh -- go test ./internal/parse/corpus/... ./internal/parse -run '^(TestCorpus|TestQualityCorpus|TestQualityBounds|TestMovie|TestTitle)' -count=1 -v
scripts/dev-run.sh -- scripts/parse-emulated.sh '^(TestQualityCorpus|TestMovieCorpus)$'   # arm64 and arm/v7 under QEMU user mode
```

A Go consumer run counts for the same immutable runtime payload as an admission run only by the payload
comparison in `docs/spikes/s5-version-admission.md` (ordered layers, `Config` SHA-256, platform, `inputs=<sha>`), never by the image ID.


## Production latency benchmark

`bench/parser` measures full `ParseTitle` calls on the wall clock and `-judge` decides whether a recorded run proves the accepted
budgets: pooled p50 at most 100000 ns and pooled p99 at most 1000000 ns (nearest rank; no mean stands in for the p99), plus, when a
comparable earlier run is given with `-base`, no more than 10% regression of either. Without a comparable base the budgets are an
absolute baseline only. No ARM latency claim is made; `GOARCH` other than `amd64` is INVALID.

Measuring (`-rounds 30 -seed 1 -out DIR`, built `CGO_ENABLED=0`) loads every title case of both fixtures through the corpus
reader and `TimeContext.Materialize`, constructs the parser once (reported as `compile_ns`, never part of a sample), runs one
discarded warmup pass, then 30 rounds, each every materialized name once in a seeded shuffle. Each sample is one `ParseTitle` call
including validation, quality, language and release group work and the budget checks. It writes exactly `DIR/samples.jsonl`,
opened with truncate in place, every write and close checked; a write error prints `parserbench: write_failed file=samples.jsonl`
and exits 1. The header of that file records the run, the SHA-256 of the image input manifest, of both corpora, of `go.mod` plus
`go.sum` and of the parser and corpus-reader sources, the hash of the materialized names, the Go version, platform, build settings
and the UTC offset and dates of the run. Every timed call must reach the same outcome class as its warmup call (the measurement stops
with `outcome_changed` otherwise, and with `parse_failed` for an error that is not the parser's own reason). Measuring prints a
diagnostic line and never a verdict; `-inputs` belongs to measuring only, the judge always reads the image's own manifest.

### Owner setup

The owner reserves the cores; nothing here, and no agent, changes host scheduling, and a container's own cpuset is never taken as
proof of host exclusivity.

1. Reserve four physical cores of one NUMA node, giving `--bench` one logical CPU of each distinct physical core, and keep the other SMT
   siblings of those cores idle (reserve them at the kernel or cgroup level too, for example an isolated CPU set), so that no other user
   thread is allowed to run on the set or on those siblings, and start the runner from a shell whose own affinity excludes
   all of them. The cpuset is given to `--bench` as digits, commas and ranges (`0-3`, `2,4-5`; at most 1024 ids, no leading zeros).
2. Provision `BOXSET_BENCH_CONFIG`: an empty directory on a local NVMe ext4 filesystem (local NVMe is proved from the backing disks,
   md slaves included: each local, non-rotational and reporting NVMe in its sysfs vendor or transport; the runner records only the
   boolean), given as an absolute path of letters, digits and `_ . / + @ = -` without a trailing slash,
   `.`/`..` component or link. The runner reads both owner values from the environment at run time and never writes either into a
   file, a log or the image.
3. Keep at least 4 GiB of memory available, no swap activity during the run, and a plain `/proc` that shows every task (no `hidepid`
   other than 0, no `subset=pid`, no mount over a task entry; being root does not replace this). The runner needs host root access to the daemon's tasks
   (`/proc/<pid>/root`), which is how it proves that the container it measured runs on this machine with the observed `/config`.

```sh
run="docs/spikes/data/bench-$(date -u +%Y%m%dT%H%M%SZ)"
scripts/dev-run.sh --bench "$BOXSET_BENCH_CPUSET" --write "$run/samples" -- sh -c 'CGO_ENABLED=0 go build -trimpath -o /tmp/parserbench ./bench/parser && /tmp/parserbench -rounds 30 -seed 1 -out "$1"' parserbench "$run/samples"
scripts/dev-run.sh -- sh -c 'CGO_ENABLED=0 go build -trimpath -o /tmp/parserbench ./bench/parser && /tmp/parserbench -judge "$1"' parserbench "$run"
```

`--bench CPUSET` is valid only with `--write` of a fresh `docs/spikes/data/bench-<UTC YYYYMMDDTHHMMSSZ>/samples` root and never with
`--capture`. It applies `--cpuset-cpus CPUSET --memory 4g --memory-swap 4g` and mounts `BOXSET_BENCH_CONFIG` read-only at
`/config`; every other form is rejected before Docker with exit 125:

| Reason | Recovery |
| --- | --- |
| `capture_flag_invalid` | `--bench` and `--capture` never combine. |
| `bench_invalid side=cpuset` | The value is not a list of ids or ranges as above. |
| `bench_invalid side=write` | `--bench` needs `--write "$run/samples"`. |
| `bench_invalid side=samples` | The root is not exactly `docs/spikes/data/bench-<UTC>/samples`, or the run directory already exists: choose a fresh UTC stamp. |
| `bench_unavailable side=config` | `BOXSET_BENCH_CONFIG` is unset or empty. |
| `bench_invalid side=config` | The value is not an absolute directory path of the allowed characters, or is a link. |
| `bench_invalid side=daemon` | The Docker endpoint is not a unix socket of this machine, its daemon runs another kernel, or the container the daemon reports is not a task of this machine's `/proc` whose cgroup names that container (a socket forwarded to another machine, even with the same kernel release). Select the local daemon (no endpoint is printed). |
| `host_tool_missing` | `awk`, `date`, `df`, `sort`, `id`, `stat` and `timeout` must be on the host `PATH`. |
| `mounts_invalid` | The created container's mounts are not exactly the one read-only `/config` bind. |
| `bench_evidence_failed` | A host observation could not be read or is incomplete: `/proc` or `/sys`, Docker's limits, the `/config` filesystem or directory scan (an unreadable directory is not an empty one), the mounted `/config` directory no longer being the device and inode that was observed first and that the running container has mounted (read through `/proc/<pid>/root`), a `/proc` walk that was cut short, that met a live task it could not read (a task that vanished meanwhile is fine) or that read under 97% of the kernel's own task count, a container that was not seen running or that exited before it could be observed, or a sampler that failed after any observation. Nothing was published; fix the host access and run again with a new stamp. |
| `bench_invalid side=proc` | The mount table does not show one plain `/proc`: it is mounted with `hidepid` (anything but 0/off) or `subset=pid`, more than one mount sits at `/proc` (or one that is not type proc), a mount of any kind sits over a task entry (`/proc/<pid>`, `/proc/self`, `/proc/thread-self` or below), or no `/proc` mount is found: a competing task for the reserved cores could be hidden. Run from a `/proc` that shows every task; being root does not replace this, and a share of the kernel's task count is not accepted as proof. |

After a child exit 0 the runner exports only the stopped container's regular `samples.jsonl` (the corpus rules: one regular file,
at most 32 MiB, through the redactor) and publishes it with the host's records, all four under the fresh run directory:
`RUN/samples/samples.jsonl`, `RUN/before.json` (after the container exists, before it starts), `RUN/during.json` (every
observation while it runs, once a second) and `RUN/after.json` (after it stopped). A nonzero child publishes nothing and keeps its
status. The three records are written from the host, are not mounted into the child and cannot be written by it. Each is a single
line of fixed numbers, enums and booleans (the schema and the rules are owned by `bench/parser/eligibility.go`): the topology of
the reserved set (counts of logical CPUs, cores, SMT siblings, NUMA nodes), the most non-benchmark user threads that were allowed
to run, or runnable, on a reserved CPU, load, minimum available memory, the cumulative swap-in and swap-out page counters at the
start and end of the stage, the container's memory limits and whether its cpuset was applied, and the `/config` filesystem. They
hold no CPU list, task listing, command line, environment, path or host name, at most 64 KiB each and 192 KiB together. The runner's
merged child output is separately bounded to 256 KiB (enough for the verbose `go test -v` run of this package); more than that is
`output_overflow`, exit 125. The record cap is unchanged.

The next runner invocation builds its image with the published records: `.dockerignore` re-includes exactly
`docs/spikes/data/bench-*/samples/samples.jsonl` and `bench-*/before.json`, `during.json` and `after.json`, and the image makes
those files and every directory below `docs/spikes/data` root-owned and read-only (`docs/spikes/data` itself and the ancestors stay
root-owned and sticky), so the child can overwrite, unlink or replace none of them. `-judge RUN_DIR` reads them from that image
and only from it: the run must be `docs/spikes/data/bench-<UTC>` of the image it runs in, its four files and its run and samples
directories must be root-owned and unwritable, and every file must be byte for byte what the image's input manifest
(`/usr/local/share/boxset/inputs.sha256`) lists for its place under `/src`. It exits 0 and prints `PARSER-LATENCY-PASS eligible=VALID …` only when
the evidence is eligible and both budgets hold; 1 `PARSER-LATENCY-FAIL reason=…` when it is eligible and a budget (`p50_budget`,
`p99_budget`) or the regression bound (`p50_regression`, `p99_regression`) is breached; 3 `PARSER-LATENCY-INVALID reason=…`
whenever the evidence cannot support a claim, which is decided first. With `-base RUN_DIR` (another run of the same image) a base
is compared only when it is itself complete, eligible and tied to its own run, inputs, clean production build and dates, and measured the same
corpora and case inputs on linux/amd64 with 30 rounds; another lock or parser source, date or compile time does not matter, and an
incomparable base is reported as `base=incomparable`, never compared. Measuring never prints the pass line. A published run
is data: do not edit it, and commit it only on purpose.

### INVALID and its recovery

Every INVALID is fixed by getting a new, complete, eligible run; a record is never edited.

| Reason | Meaning and recovery |
| --- | --- |
| `record_missing`, `samples_missing` | A record or `samples.jsonl` is not in the image the judge was built with: the run was not published, was published after the image was built, or is not under `docs/spikes/data/bench-*`. Publish, then judge from the next invocation. |
| `record_nonregular`, `samples_nonregular` | The entry is a link or not a regular file. Run again. |
| `record_oversized`, `samples_oversized` | A record is over 64 KiB (192 KiB together) or `samples.jsonl` is over 32 MiB. Run again. |
| `record_unreadable`, `samples_unreadable`, `samples_malformed`, `record_malformed`, `record_incomplete`, `samples_incomplete`, `record_stage` | The file is not the schema's bounded exact form, lacks a key (including `go`, `utc_offset_sec` and `compile_ns` of the samples header), has an extra key, holds a `null` for a required value, holds the wrong stage, or `samples.jsonl` is truncated, has a repeated or missing item, a non-positive duration or an extra line. Run again from the same commit. |
| `identity_mismatch`, `run_mismatch`, `inputs_mismatch` | The run directory name, image or input manifest hash differ between the three records, or between them and the samples. Run again; never mix files of two runs. |
| `run_location`, `image_manifest`, `image_mismatch`, `record_mutable` | The run directory is not exactly `docs/spikes/data/bench-<UTC>` of the image the judge runs in, the image manifest cannot be read, a record's bytes are not the digest the image's manifest lists for its place in `/src` (a look-alike, or a file that changed), or a record, the run or samples directory, or an ancestor can be written by someone other than root (the ancestors may only be sticky). Judge a run that the next runner invocation imported. |
| `order`, `incomplete_sampling` | The three stages are not in time order, or fewer than one observation before and after, or two during the run. A very short child cannot be observed; use the full command. |
| `date_rollover` | The run crossed a UTC midnight, the UTC offset is not 0, or the dates of the samples and the records differ. Run again away from midnight. |
| `topology`, `smt`, `numa` | `topology`: the reserved set is not four CPUs on four distinct physical cores, or its counts changed during the run, or a CPU could not be read. `smt`: two members of the set share a physical core (a sibling inside the set), or a user thread outside the benchmark container was running on an SMT sibling of a member that is not itself a member (`sibling_runnable_max` above 0, one count per observation, no CPU number and no thread name recorded). Siblings outside the set are valid while idle (`threads_per_core_max` of 2 with `siblings_outside_set` above 0 and `sibling_runnable_max` 0): the owner keeps them idle. The load is taken from the CPU a running thread last ran on, read from the complete `stat` record of the thread (its name may contain spaces and newlines; a record that does not end in the expected fields fails the observation), so keep the runner, Docker and every other job off those siblings too. `numa`: the set spans more than one NUMA node. Reserve four whole physical cores of one node, one thread of each in the set (step 1). |
| `overlap` | A user thread outside the benchmark container could run on a reserved CPU, or was runnable on one. A thread counts only where both its affinity and its effective cgroup cpuset (`cpuset.cpus.effective`, or `cpuset.effective_cpus` on cgroup v1, taken from the nearest ancestor cgroup that has the file; no such file means no cpuset restriction) contain a reserved CPU, so an affinity mask that still names the set does not count once its cgroup excludes it, while a runnable or allowed thread whose effective cpuset includes the set does. A cgroup that cannot be read fails the observation (`bench_evidence_failed`). Reserve the cores at the kernel or cgroup level (for example by restricting the slices' cpusets) and start the runner outside them. No CPU list or cpuset value is recorded or printed. |
| `swap` | Host swap-in or swap-out pages changed between the first and last observation. Free memory and run again. |
| `memory` | The container's memory or memory-plus-swap limit is not exactly 4 GiB, or less than 4 GiB was available. |
| `container` | Docker did not apply the cpuset it was given. |
| `filesystem` | `/config` is not an empty, read-only, directly NVMe-backed ext4 directory. |
| `rounds`, `platform`, `build` | The samples are not 30 rounds of a linux/amd64 production build with cgo off and without the race detector. |
| `corpus_changed`, `lock_changed`, `source_changed`, `names_changed`, `reference_invalid` | The corpora, `go.mod`/`go.sum` or the parser sources the judge sees differ from those measured, the materialized names or case identities differ, or the judge cannot read them. Measure again from the commit that is being judged. |

`scripts/dev-run-smoke.sh --benchmark-evidence` proves the evidence path without the owner's reservation: it gives the real runner a
synthetic one-core cpuset and an empty config directory of its own, in a scratch copy of the repository. A child there tries to
overwrite, append to, unlink, replace, `chmod`, move or remove the imported records of an earlier run and every ancestor, to write
`/config` and to create entries beside the records (all 26 attempts are denied, the records' digests are unchanged, the decoy
files the `.dockerignore` does not admit are absent from the image, and every imported file is root-owned 0444 and directory 0555)
and writes the synthetic control samples; the runner publishes exactly its own four files, which the smoke checks for shape,
bound and absence of any path text, and the owner config path never reaches the runner's scratch (a passive Docker shim reads the
scratch at the container's removal for it, and prints nothing). The host evidence faults each fail closed with the fixed reason
and publish nothing: a sampler that fails after a good observation, a `/proc` walk cut short after one readable task or returning
well under the kernel's own task count, an unreadable live task, an unscannable `/config`, a `/config` read that fails after the first
observation succeeded, the mounted directory renamed and its path recreated while the child runs, a Docker endpoint that is not a
unix socket of this machine, a daemon that runs another kernel, a daemon whose container is not a task of this machine (a forwarded
socket to a machine with the same kernel release) and a `/proc` that can hide tasks (`hidepid` 2, 4 or `ptraceable`, each for an
unprivileged caller and for root, and a mount over one competing task's `/proc` entry that hides it from root as well while the
share of the kernel's task count stays above 97%, applied before the run, after the runner's startup check and before its first
observation, and after a successful observation while the child is still running (the child waits on a gate file the host creates in `/config`, and the mask is applied only when a completed observation exists and the daemon lists the container running, then the gate is released), since the mount table is proven again around every observation); a task
that vanished while `/proc` was walked does not fail the run, a `/config` that gains an entry after the child ran shows in the
after record and one that changes only while the child runs shows in the during record, and the final judge, given otherwise eligible
records that are empty before and after but not during, answers INVALID `filesystem`. The final judge's image also gets two
disposable faults (a record, or only the run directory, left writable by UID 1000 with the same bytes): the child confirms the write
access and the judge answers `record_mutable`, never a pass.
As the next invocations from refreshed images, the final judge, whose child first fails (as UID
1000) to overwrite, append to, unlink, replace, `chmod`, move or remove any record it is about to read, its run and samples
directories and every ancestor (31 attempts denied, bytes, owners, modes and inodes unchanged), then rejects the real host-written
records (one core), a run that is not in the image, a missing record, a hostile record, an oversized record, an after-state
swapped for another stage, a record of another run, a link and an overlapping workload, a complete look-alike the child makes
inside the data directory (its own, writable files) and one outside the image, each INVALID with exit 3 and never a pass line, and, with
independently written valid records beside the same samples, passes.

Proof status. The deterministic eligibility, verdict and provenance checks (`TestEligibility…`, `TestLatencyGate…`) and the full
corpus measurement with its write-failure control (`TestBenchmarkCorpus`, excluded under `-race`) are unit tests and make no
performance claim; their durations are synthetic or diagnostic. NOT-RUN here: the eligible measurement itself, which needs the
owner's reservation and `BOXSET_BENCH_CONFIG`; until a run of the command above is judged PASS, AC-021 stays open and an INVALID
or FAIL run never closes it.
