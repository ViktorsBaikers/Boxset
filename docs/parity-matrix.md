# Parser parity matrix

Local parity records for the native ports of the series and movie release-name parsers. The matrix below covers the quality
entry point only; title parsing rows are added when those ports land. A row is evidence of an observed comparison, not a claim of
completeness beyond the named entry point. Cross-app closure remains the closure audit's job.

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

## Dispositions

`testdata/parser/dispositions.json` holds none: no enabled case differs. A future disposition must bind the case identity, the
exact source expectation and observed native value, a rationale and an answered owner question; it must bind every distinct
mismatching input observation of the case, so a second input that differs with another observed value is not covered by the
first; a stale or ownerless record fails the quality corpus test. The whole per-file inventory (every scope) reconciles against
the header separately from the selected quality scope, so a later title scope or out-of-scope case cannot be counted as
quality.

## Known source-fidelity decisions

| Topic | Decision |
| --- | --- |
| Movie TELESYNC resolution | The result carries the release's parsed resolution, as the source assigns it on that result. |
| Version digit that `Convert.ToInt32` rejects (non-ASCII decimal digit matched by `\d`) | The source throws; the port returns `title_unrecognized` with no value. |
| Platform path semantics | Linux: only NUL is invalid in a path, only `/` separates in `Path.GetExtension`. |
| `Quality` shape | `Source`, `Resolution`, `Modifier` are exposed in addition to `Code` and the detections because the movie tests assert them. |
