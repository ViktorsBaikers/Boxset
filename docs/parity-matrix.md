# Parser fixture inventory

Local records for the native series and movie release-name parsers. The inventory covers the quality entry point and the
title entry points of both sides, as held by the frozen fixtures `testdata/parser/series.jsonl` and
`testdata/parser/movie.jsonl` (schema 2, see `docs/spikes/s1-parser.md`). A row is evidence of an observed comparison, not a
claim of completeness beyond the named entry point.

Legend: every fixture case is asserted; disposed = a mismatch with an answered owner disposition. A fixture holds only
asserted cases, grouped by `scope` (`quality` or `title`) and `scopeOwner` (the entry point the case runs).
`TestCorpusFull` locks the per-scope and per-owner counts and the SHA-256 of both files.

## Quality (`QualityParser.ParseQuality`)

| App | Cases | Passed | Disposed | Inputs parsed |
| --- | --- | --- | --- | --- |
| Series | 356 | 356 | 0 | 398 |
| Movie | 323 | 323 | 0 | 365 |

Inputs parsed exceed cases because the rows for other-source names run four separator variants each, all lowered into one
case with four recorded inputs.

Asserted properties covered: `Quality`, `Quality.Source`, `Quality.Resolution`, `Quality.Modifier`, `Revision.Version`,
`Revision.Real`, `Revision.IsRepack`, `SourceDetectionSource`, `ResolutionDetectionSource`; the consumer rejects any other
property path or an enum member without an explicit code mapping.

## Movie title (`Parser.ParseMovieTitle`, `ReleaseGroupParser.ParseReleaseGroup`, `LanguageParser.ParseLanguages`)

Scope `title` of `movie.jsonl`: the `ParseTitle` movie branch and the release group and language parsers it runs under the
same budget. Every row is a case with recorded arguments, inputs and every asserted property; all cases pass and none is
disposed.

| Entry point | Cases | Passed | Disposed |
| --- | --- | --- | --- |
| `Parser.ParseMovieTitle` (file names through `ParseTitle`; the 126 `$isDir` cases through public `ParseDirTitle`) | 321 | 321 | 0 |
| `ReleaseGroupParser.ParseReleaseGroup` | 161 | 161 | 0 |
| `LanguageParser.ParseLanguages` | 30 | 30 | 0 |
| `Parser.CleanMovieTitle` (production method; the accent case) | 1 | 1 | 0 |

Asserted properties covered: `PrimaryMovieTitle`, `MovieTitle.CleanMovieTitle` (the parsed primary title after
`CleanMovieTitle`, recorded with the expected title before normalization: both sides are normalized), `MovieTitles.Count`,
`MovieTitles[n]`, `ReleaseGroup`, `Year`, `Edition`, `HardcodedSubs`, `ImdbId`, `TmdbId`, `Languages.Count`,
`Languages.First()`, `Languages.Contain` (a membership assertion, recorded under the path plus `.Contain` as the ordered list
of members), and the reserved keys `$result` (the assertion on the entry point's own result: `null` for a rejected name, the
group text of `ParseReleaseGroup`) and `$isDir` (marks a folder-name case, recorded only when true; the consumer calls public `ParseDirTitle`).
The language entry point asserts `$result.Contain` and `First().Id`. The consumer maps language members through the explicit
code table of `vocab.go`, compares a null string with the empty text, and rejects any other property path, entry point or
enum member.

Generated hashed names (an MD5 chain and random 32- and 40-character names) have no recordable input; the hostile tests
(`TestHostileMovieTitleRejectsGeneratedHashNames`, `TestHostileSeriesTitleRejectsGeneratedHashNames`) run the same generators
(100 chained hashes, 2 x 500 random names) against `ParseTitle`.

## Series title (`Parser.ParseTitle`, `ReleaseGroupParser.ParseReleaseGroup`, `LanguageParser.ParseLanguages`)

Scope `title` of `series.jsonl`: the `ParseTitle` series branch (with the `ParseSeriesName` and `CleanSeriesTitle` helpers)
and the release group and language parsers it runs under the same budget. 1166 title cases ran:

| Entry point | Cases | Passed | Disposed |
| --- | --- | --- | --- |
| `Parser.ParseTitle` | 754 | 754 | 0 |
| `Parser.ParseSeriesName` | 33 | 33 | 0 |
| `Parser.CleanSeriesTitle` | 44 | 44 | 0 |
| `ReleaseGroupParser.ParseReleaseGroup` | 166 | 166 | 0 |
| `LanguageParser.ParseLanguages` | 169 | 169 | 0 |

`TestCorpusCoverage` asserts every case of both fixtures ran. The `CleanSeriesTitle` cases include `Ríkið` (expected
`rikid`). The series fixtures name the Brazilian member `PortugueseBrazil`; it maps to `portuguese-br`. The fixtures hold the
release group `Neutral` wherever a group name would name an upstream product (one documented substitution, kept in every
recorded input and expected string); a `CleanSeriesTitle` expectation that carries it is compared lower-cased.

## Movie-title decisions

| Topic | Decision |
| --- | --- |
| Null results | `ParseTitle` returns `title_unrecognized` where a name has no title (a name with no letter or digit, a password/yenc name, a hashed release, no matching pattern); a missing string field is the empty string. |
| Directory form | `ParseTitle` is the file form and has no `isDir` argument. The 126 folder-name fixture cases (`$isDir`, including `movie:0403`) pass through public `ParseDirTitle`, which also tries the year-first folder pattern. `ParseTitle` still refuses a year-first name (`TestMovieCorpusDirectoryCasesUseThePublicDirectoryEntry`). |
| Reversed names | A name containing the reversed-title marker is converted so that the interpolated character array formats as its type name: the title becomes that text plus the untouched tail. |
| File extension helper | A trailing dot plus two to four letters or digits is removed only when it is a media extension of the movie quality table (or `.par2`/`.nzb`); the fixture cases that carry `.mkv`, `.mp4`, `.avi` and similar extensions pass, but no case pins the `.par2`/`.nzb` members or an extension outside the table. |
| Language members | `vocab.go` maps every language member, including those the parser never produces, to a code. |
| Accent removal | `CleanMovieTitle` ends in accent removal, and the standard library has no Unicode normalization. The parser implements `CleanMovieTitle` (normalization pattern, lower-casing, umlaut spelling) and accent removal as the standard-library equivalent of decompose, drop non-spacing marks, recompose: a table of the lower-case Latin-1 Supplement and Latin Extended-A letters that have a canonical decomposition, plus removal of combining marks of already decomposed text. Letters without a decomposition (ø, æ, œ, ł, đ and the like) are deliberately absent, with one exception: ð folds to d, because a series fixture asserts that `Ríkið` cleans to `rikid` (`CleanSeriesTitle` shares the table). Any other non-ASCII letter (Cyrillic, Vietnamese, Latin Extended-B) is a hard error, never a wrongly folded string, because it might carry a decomposition the table does not know. The 15 URL title cases are ASCII and the accent case (`Carnivàle`, precomposed) folds to `carnivale`; both run as title cases. |
| String indexes | The title group's index is converted to UTF-16 code units for the simple-release-title replacement; a range outside the string is reported as no result. |
| Long names | A 4096-byte name whose bulk is a trailing word parses (18 ms in the dev image). A name whose title, or whose German tag, is thousands of characters long exceeds the 50 ms match timeout in the unanchored bracketed-alternative and German-remux patterns and fails closed with `match_timeout`; it never returns a partial result. |

## Dispositions

`testdata/parser/dispositions.json` (format version 1, independent of the fixture schema) holds none: no case differs. A
future disposition must bind the case identity, the exact expectation and observed native value, a rationale and an answered
owner question; it must bind every distinct mismatching input observation of the case, so a second input that differs with
another observed value is not covered by the first; a stale or ownerless record fails the quality and title corpus tests.
The consumers reconcile the whole inventory (both scopes) against the per-scope case counts separately from the selected
scope, so a title case cannot be counted as quality and a quality name cannot stand in for a title case.

## Quality and title decisions

| Topic | Decision |
| --- | --- |
| Movie TELESYNC resolution | The result carries the release's parsed resolution. |
| Version digit that does not parse as an integer (non-ASCII decimal digit matched by `\d`) | The parse returns `title_unrecognized` with no value. |
| Platform path semantics | Linux: only NUL is invalid in a path, only `/` separates when a file extension is read. |
| `Quality` shape | `Source`, `Resolution`, `Modifier` are exposed in addition to `Code` and the detections because the movie fixtures assert them. |


[codebase-memory] Coverage note: this file is not reliably represented in the graph (status=excluded, kind=not_indexed_dir: excluded subtree, freshness=missing). action=read_source_and_reindex. (best-effort signal)

[codebase-memory] Coverage note: this file is not reliably represented in the graph (status=excluded, kind=not_indexed_dir: excluded subtree, freshness=missing). action=read_source_and_reindex. (best-effort signal)

[codebase-memory] Coverage note: this file is not reliably represented in the graph (status=excluded, kind=not_indexed_dir: excluded subtree, freshness=missing). action=read_source_and_reindex. (best-effort signal)

[codebase-memory] Coverage note: this file is not reliably represented in the graph (status=excluded, kind=not_indexed_dir: excluded subtree, freshness=missing). action=read_source_and_reindex. (best-effort signal)

[codebase-memory] Coverage note: this file is not reliably represented in the graph (status=excluded, kind=not_indexed_dir: excluded subtree, freshness=missing). action=read_source_and_reindex. (best-effort signal)