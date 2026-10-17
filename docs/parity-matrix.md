# Parser parity matrix

Local parity records for the native ports of the series and movie release-name parsers. The matrix covers the quality entry
point of both sides and the title entry points of both sides. A row is evidence of an
observed comparison, not a claim of completeness beyond the named entry point. Cross-app closure remains the closure audit's job.

Legend: enabled = upstream-enabled in-scope cases; disabled = upstream `Ignore`/`Explicit`, retained and not asserted;
out of scope = retained, owned by later entry points; disposed = a retained mismatch with an answered owner disposition.
The passed/disposed/disabled/out-of-scope columns below count the selected quality scope; the header counts cover every scope.

## Quality (`QualityParser.ParseQuality`)

| App | Source commit | Cases | Enabled | Disabled | Out of scope | Passed | Disposed | Inputs parsed |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Series | `57816c149d02f95543973fefb4780c4495f8121c` | 356 | 356 | 0 | 0 | 356 | 0 | 398 |
| Movie | `df2faa64c75e6081556d11232dc46523e7264ea1` | 323 | 323 | 0 | 0 | 323 | 0 | 365 |

Per-file inventories (source area `tests/`; cases = `TestCase` attributes plus `TestCaseSource` rows):

| File | Series cases | Movie cases |
| --- | --- | --- |
| `QualityParserFixture.cs` | 320 (290 attributes + 30 rows) | 287 (273 attributes + 14 rows) |
| `ExtendedQualityParserRegex.cs` | 30 | 30 |
| `AnimeVersionFixture.cs` | 6 | 6 |

The movie fixture contains one commented-out `parsing_our_own_quality_enum_name` block; it is counted
(`source:commented_test_forms`) but is not a source case. Inputs parsed exceed cases because the `should_parse_quality_from_other_source`
rows run four separator variants each, all lowered into one case with four recorded inputs.

Asserted properties covered (exact source property paths): `Quality`, `Quality.Source`, `Quality.Resolution`, `Quality.Modifier`,
`Revision.Version`, `Revision.Real`, `Revision.IsRepack`, `SourceDetectionSource`, `ResolutionDetectionSource`; the consumer
rejects any other property path or an enum member without an explicit code mapping.

## Movie title (`Parser.ParseMovieTitle`, `ReleaseGroupParser.ParseReleaseGroup`, `LanguageParser.ParseLanguages`)

Scope `title` of `movie.jsonl` (commit `df2faa64c75e6081556d11232dc46523e7264ea1`): the `ParseTitle` movie branch and the
release group and language parsers it runs under the same budget. Every row is a source case with recorded arguments, inputs
and every asserted property; all enabled cases pass and none is disposed.

| Entry point | Cases | Enabled | Disabled | Passed | Disposed |
| --- | --- | --- | --- | --- | --- |
| `Parser.ParseMovieTitle` (through `ParseTitle`; the 126 `isDir` cases through its directory form) | 321 | 321 | 0 | 321 | 0 |
| `ReleaseGroupParser.ParseReleaseGroup` | 161 | 161 | 0 | 161 | 0 |
| `LanguageParser.ParseLanguages` | 30 | 30 | 0 | 30 | 0 |
| `Parser.CleanMovieTitle` (the accent test: the helper's own folded result) | 1 | 1 | 0 | 1 | 0 |

Per-file inventories of the title fixtures (cases = `TestCase` attributes plus `TestCaseSource` rows plus plain `[Test]` methods):

| File | Cases | Title | Out of scope |
| --- | --- | --- | --- |
| `ParserFixture.cs` | 112 | 106 | 6 |
| `EditionParserFixture.cs` | 49 | 49 | 0 |
| `LanguageParserFixture.cs` | 195 | 159 | 36 |
| `CrapParserFixture.cs` | 26 | 23 | 3 |
| `HashedReleaseFixture.cs` | 11 | 0 | 11 |
| `ReleaseGroupParserFixture.cs` | 154 | 154 | 0 |
| `UrlFixture.cs` | 22 | 22 | 0 |

Asserted properties covered (exact source property paths): `PrimaryMovieTitle`, `MovieTitle.CleanMovieTitle` (the parsed
primary title after `CleanMovieTitle`, recorded with the expected title before normalization: the source normalizes both sides),
`MovieTitles.Count`, `MovieTitles[n]`, `ReleaseGroup`, `Year`, `Edition`, `HardcodedSubs`, `ImdbId`, `TmdbId`, `Languages.Count`, `Languages.First()`,
`Languages.Contain` (a membership assertion, recorded under the path plus `.Contain` as the ordered list of members), and the
reserved keys `$result` (the assertion on the entry point's own result: `null` for a rejected name, the group text of
`ParseReleaseGroup`) and `$isDir` (the `isDir` argument of `ParseMovieTitle`, recorded only when true). The language entry point
asserts `$result.Contain` and `First().Id`. The consumer maps language members through the explicit code table of `vocab.go`,
compares the source's null string with the empty text, and rejects any other property path, entry point or enum member.

Retained out of scope with their source arguments and no recorded input or assertion (56 cases, scope `out_of_scope`): tests of
entry points outside the title branch, plus the tests that have no recordable input.

| Owner | Cases | Why not a title assertion |
| --- | --- | --- |
| `Parser.ParseMoviePath` | 11 | path entry point (`FileInfo` directory and file name semantics), not a release-name string |
| `LanguageParser.ParseSubtitleLanguage` / `ParseSubtitleLanguageInformation` | 15 / 21 | subtitle file naming, not a release title |
| `Parser.NormalizeImdbId` | 6 | ID normalization helper, not a title result |
| `Parser.ParseMovieTitle` generated names | 3 | the names come from an MD5 chain and a random generator, so there is no input to record; `TestHostileMovieTitleRejectsGeneratedHashNames` runs the same generators (100 chained hashes, 2 x 500 random 32/40-character names) against `ParseTitle` |

`tests/ParsingServiceTests/GetMovieFixture.cs` calls `ParseMovieTitle` only to build the argument of a mocked repository
verification and asserts no parse result, so it is not part of the inventory. The language and slug fixtures other than the
listed ones, the numeral and ISO language tests and the scene checker assert no title result and are not extracted.

## Series title (`Parser.ParseTitle`, `ReleaseGroupParser.ParseReleaseGroup`, `LanguageParser.ParseLanguages`)

Scope `title` of `series.jsonl` (commit `57816c149d02f95543973fefb4780c4495f8121c`): the `ParseTitle` series branch (with the
`ParseSeriesName` and `CleanSeriesTitle` helpers) and the release group and language parsers it runs under the same budget.
1166 enabled title cases ran (754 `ParseTitle`, 33 `ParseSeriesName`, 44 `CleanSeriesTitle`, 166 `ParseReleaseGroup`, 169
`ParseLanguages`), 1166 passed, 0 disposed, 0 disabled. 75 other-entry cases stay out of scope. `TestCorpusCoverage` asserts every
enabled scoped case ran. The 43 cases of `NormalizeSeriesTitleFixture.cs` are `CleanSeriesTitle` title cases, `Ríkið` (expected
`rikid`) included. The series source names the Brazilian member `PortugueseBrazil`; it maps to `portuguese-br`. The series extractor
records the release group the reference tests name after the two upstream products as `Neutral` (one documented substitution in
`internal/parse/corpus/extract/main.go`, applied to every recorded input and expected string before identities are hashed); a
`CleanSeriesTitle` expectation that carries it is compared lower-cased.

## Known movie-title source-fidelity decisions

| Topic | Decision |
| --- | --- |
| Null results | `ParseTitle` returns `title_unrecognized` where the source returns null (a name with no letter or digit, a password/yenc name, a hashed release, no matching pattern); a null string field is the empty string. |
| Directory form | `ParseTitle` is the file form and has no `isDir` argument. The `isDir` argument of the source (a year-first folder pattern) is reachable only through the private `parseTitle`; the 126 corpus cases whose source call passes it run through that form and are counted apart (`private_isDir` in the `TestMovieCorpus` log), never as public `ParseTitle` coverage, and the others run through `ParseTitle`. The public `ParseTitle` refuses a year-first name (`TestMovieCorpusDirectoryCasesAreProvedOnlyOnThePrivateAdapter`). |
| Reversed names | A name containing the reversed-title marker is converted exactly as the source converts it: the interpolated character array formats as its type name, so the title becomes that text plus the untouched tail. |
| File extension helper | The source's extension helper is outside the delivered areas. The port removes a trailing dot plus two to four letters or digits only when it is a media extension of the movie quality table (or `.par2`/`.nzb`), as the source's helper does by this author's reading; the corpus cases that carry `.mkv`, `.mp4`, `.avi` and similar extensions pass, but no case pins the `.par2`/`.nzb` members or an extension outside the table. |
| Language members | The `Language` declaration is outside the delivered areas; the extractor accepts the members the language parser names, and `vocab.go` maps every source member (including those the parser never produces) to a code. |
| Accent removal | The source's `CleanMovieTitle` ends in `RemoveAccent`, an extension outside the delivered areas, and the standard library has no Unicode normalization. The consumer ports `CleanMovieTitle` (normalization pattern, lower-casing, umlaut spelling) and `RemoveAccent` as the standard-library equivalent of decompose, drop non-spacing marks, recompose: a table of the lower-case Latin-1 Supplement and Latin Extended-A letters that have a canonical decomposition, plus removal of combining marks of already decomposed text. Letters without a decomposition (ø, æ, œ, ł, đ and the like) are deliberately absent, as in the source, with one exception: ð folds to d, because the series source asserts that `Ríkið` cleans to `rikid` (`CleanSeriesTitle` shares the table). Any other non-ASCII letter (Cyrillic, Vietnamese, Latin Extended-B) is a hard error, never a wrongly folded string, because it might carry a decomposition the table does not know. The 15 URL title cases are ASCII and the accent test (`Carnivàle`, precomposed) folds to `carnivale`; both run as title cases. |
| String indexes | The title group's index is converted to UTF-16 code units for the simple-release-title replacement; a range outside the string is the source's exception, reported as no result. |
| Long names | A 4096-byte name whose bulk is a trailing word parses (18 ms in the dev image). A name whose title, or whose German tag, is thousands of characters long exceeds the 50 ms match timeout in the source's unanchored bracketed-alternative and German-remux patterns and fails closed with `match_timeout`; it never returns a partial result. |

## Dispositions

`testdata/parser/dispositions.json` holds none: no enabled case differs. A future disposition must bind the case identity, the
exact source expectation and observed native value, a rationale and an answered owner question; it must bind every distinct
mismatching input observation of the case, so a second input that differs with another observed value is not covered by the
first; a stale or ownerless record fails the quality and movie corpus tests. The whole per-file inventory (every scope)
reconciles against the header separately from the selected scope, so a title or out-of-scope case cannot be counted as quality
and a quality name cannot stand in for a title case (a title case from a quality fixture fails).

## Known source-fidelity decisions

| Topic | Decision |
| --- | --- |
| Movie TELESYNC resolution | The result carries the release's parsed resolution, as the source assigns it on that result. |
| Version digit that `Convert.ToInt32` rejects (non-ASCII decimal digit matched by `\d`) | The source throws; the port returns `title_unrecognized` with no value. |
| Platform path semantics | Linux: only NUL is invalid in a path, only `/` separates in `Path.GetExtension`. |
| `Quality` shape | `Source`, `Resolution`, `Modifier` are exposed in addition to `Code` and the detections because the movie tests assert them. |
