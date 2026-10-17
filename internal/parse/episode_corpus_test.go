//go:build !race

package parse

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/parse/corpus"
)

// TestEpisodeCorpus runs every enabled title case of the committed series corpus
// through the real parser: the series title cases through ParseTitle (and the
// ParseSeriesName and CleanSeriesTitle helpers the source normalizes through)
// and the release group and language cases through the parsers ParseTitle runs.
// A case passes only when every source assertion holds for every input; a
// retained mismatch needs an exact, answered disposition that binds every
// mismatching input observation, and a stale or ownerless disposition fails. The
// whole inventory (all scopes) reconciles against the header separately from the
// selected title scope, whose count must be positive for every entry point and
// come only from the title fixtures.
func TestEpisodeCorpus(t *testing.T) {
	disp := readDispositions(t)
	var all []corpus.Case
	for _, app := range []string{"series", "movie"} {
		c, _ := readCorpusFile(t, app)
		all = append(all, c.Cases...)
	}
	if err := disp.Validate(&corpus.Corpus{Cases: all}); err != nil {
		t.Fatalf("dispositions: %v", err)
	}

	c, hash := readCorpusFile(t, "series")
	byOwner := map[string]int{}
	for _, cs := range c.Cases {
		if cs.Scope == corpus.ScopeTitle {
			byOwner[cs.ScopeOwner]++
		}
	}
	for _, owner := range []string{ownerSeriesTitle, ownerSeriesName, ownerCleanSeries, ownerReleaseGroup, ownerLanguages} {
		if byOwner[owner] == 0 {
			t.Errorf("no title case of entry point %s", owner)
		}
	}

	now, loc := seriesRunClock(t)
	r := runSeriesCorpus(newSeriesTestParser(t, now, loc), c, dispositionsByID(disp), corpus.TimeContext{Now: now, Location: loc})
	for _, problem := range append(r.problems, r.reconcile(c.Header.Counts)...) {
		t.Error(problem)
	}
	t.Logf("series-title-corpus cases=%d title=%d (title=%d name=%d clean=%d group=%d languages=%d) enabled=%d passed=%d disposed=%d disabled=%d quality=%d out_of_scope=%d inputs=%d sha256=%s",
		len(c.Cases), r.enabled+r.disabled, byOwner[ownerSeriesTitle], byOwner[ownerSeriesName], byOwner[ownerCleanSeries],
		byOwner[ownerReleaseGroup], byOwner[ownerLanguages], r.enabled, r.passed, r.disposed, r.disabled,
		r.otherEnabled+r.otherDisabled, r.outOfScope, r.inputs, hash)
}

// seriesCase is one synthetic series title-corpus case over a title fixture file.
func seriesCase(id, owner string, want map[string]corpus.Value, inputs ...corpus.Value) corpus.Case {
	items := inputs
	expected := map[string]corpus.Value{corpus.InputKey: {Kind: corpus.KindArray, Items: items}}
	for k, v := range want {
		expected[k] = v
	}
	return corpus.Case{
		ID: id, File: "tests/SingleEpisodeParserFixture.cs", Method: "m", Entry: "TestCase#1", Line: 1,
		UpstreamEnabled: true, Scope: corpus.ScopeTitle, ScopeOwner: owner,
		Args: []corpus.Argument{items[0]}, Expected: expected,
	}
}

func seriesCorpus(cases ...corpus.Case) *corpus.Corpus {
	c := mixedCorpus(cases...)
	c.Header.SourcePaths = []string{"tests/SingleEpisodeParserFixture.cs"}
	return c
}

// runSeries runs one synthetic case on the real parser at the committed run clock.
func runSeries(t *testing.T, cs corpus.Case, byID map[string]corpus.Disposition) titleRun {
	t.Helper()
	now, loc := seriesRunClock(t)
	return runSeriesCorpus(newSeriesTestParser(t, now, loc), seriesCorpus(cs), byID, corpus.TimeContext{Now: now, Location: loc})
}

// A synthetic case per property family proves the consumer resolves real parser
// results, with the expected values worked out by hand from the source.
func TestEpisodeCorpusConsumerResolvesEveryPropertyFamily(t *testing.T) {
	null := corpus.Value{Kind: corpus.KindNull}
	truth := boolValue(true)
	ints := func(n ...int) corpus.Value {
		items := make([]corpus.Value, len(n))
		for i, v := range n {
			items[i] = intValue(v)
		}
		return corpus.Value{Kind: corpus.KindArray, Items: items}
	}
	dec := func(s string) corpus.Value { return corpus.Value{Kind: corpus.KindDecimal, Str: s} }
	for _, tc := range []struct {
		name  string
		input string
		want  map[string]corpus.Value
	}{
		{"single episode", "Series.Title.S03E06.DVDRip.XviD-WiDE", map[string]corpus.Value{
			"$result.NotNull": truth, "SeriesTitle": str("Series Title"), "SeasonNumber": intValue(3),
			"EpisodeNumbers.Count": intValue(1), "EpisodeNumbers.First()": intValue(6), "EpisodeNumbers.SingleOrDefault()": intValue(6),
			"AbsoluteEpisodeNumbers.Count": intValue(0), "FullSeason": boolValue(false), "ReleaseGroup": str("WiDE"),
			"SeasonNumbers.Equal": ints(3), "IsMultiSeason": boolValue(false), "ReleaseTitle": str("Series.Title.S03E06.DVDRip.XviD-WiDE"),
		}},
		{"multi episode is order insensitive", "Series.S03E01-06.DUAL.BDRip.XviD.AC3.-HELLYWOOD", map[string]corpus.Value{
			"EpisodeNumbers.EquivalentTo": ints(6, 5, 4, 3, 2, 1), "SeasonNumber": intValue(3),
		}},
		{"season pack", "Series.Title.S01-S09.1080p.AMZN.WEB-DL.DDP2.0.H.264-NTb", map[string]corpus.Value{
			"FullSeason": boolValue(true), "IsMultiSeason": boolValue(true), "SeasonNumbers.Equal": ints(1, 2, 3, 4, 5, 6, 7, 8, 9),
			"EpisodeNumbers.Count": intValue(0), "IsPartialSeason": boolValue(false),
		}},
		{"partial season", "The.Series.S07.Vol.1.1080p.NF.WEBRip.DD5.1.x264-NTb", map[string]corpus.Value{
			"IsPartialSeason": truth, "SeasonPart": intValue(1), "FullSeason": boolValue(false),
		}},
		{"absolute anime with hash and group", "[SubDESU]_Show_One_07_(1280x720_x264-AAC)_[6B7FD717]", map[string]corpus.Value{
			"AbsoluteEpisodeNumbers.Single()": intValue(7), "SeriesTitle": str("Show One"), "ReleaseGroup": str("SubDESU"),
			"ReleaseHash": str("6B7FD717"), "SeasonNumber": intValue(0), "EpisodeNumbers.SingleOrDefault()": intValue(0),
		}},
		{"special decimal is exact text", "[Anime Time] Series no Mayo - 12.5.mkv", map[string]corpus.Value{
			"SpecialAbsoluteEpisodeNumbers.First()": dec("12.5"), "SpecialAbsoluteEpisodeNumbers.EquivalentTo": corpus.Value{Kind: corpus.KindArray, Items: []corpus.Value{dec("12.50")}},
			"SpecialAbsoluteEpisodeNumbers.Count": intValue(1), "AbsoluteEpisodeNumbers.Count": intValue(0), "FullSeason": boolValue(false),
		}},
		{"daily episode", "A Late Talk Show - 2011-04-12 - Gov. Deval Patrick", map[string]corpus.Value{
			"AirDate": str("2011-04-12"), "SeriesTitle": str("A Late Talk Show"), "DailyPart": null, "EpisodeNumbers.Count": intValue(0),
		}},
		{"daily part", "Series.Title.2015.09.07.Part.2.720p.HULU.WEBRip.AAC2.0.H.264-RlsGrp", map[string]corpus.Value{
			"DailyPart": intValue(2), "AirDate": str("2015-09-07"),
		}},
		{"mini series", "The.Big.Series.Leader.Part.2.DSR.XviD-SYS", map[string]corpus.Value{
			"SeasonNumber": intValue(1), "EpisodeNumbers.First()": intValue(2), "SeriesTitle": str("The Big Series Leader"),
		}},
		{"split episode", "Series.Title.S06E01b.Fade.Out.Fade.in.Part.2.1080p.DSNP.WEB-DL.AAC2.0.H.264-FLUX", map[string]corpus.Value{
			"IsSplitEpisode": truth, "EpisodeNumbers.First()": intValue(1),
		}},
		{"special season extra", "The.Series.S03.Extras.02.720p", map[string]corpus.Value{
			"IsSeasonExtra": truth, "FullSeason": truth, "IsSeasonTitle": boolValue(false),
		}},
		{"possible special", "Title.the.Series.S05.Special.HDTV.x264-2HD", map[string]corpus.Value{
			"IsPossibleSpecialEpisode": truth, "Special": truth,
		}},
		{"multiple titles", "Серия: Спадок / Series: Legacy / S2E1-4 of 10 (2023) WEB-DL 1080p Ukr/Eng | sub Eng", map[string]corpus.Value{
			"SeriesTitleInfo.AllTitles.EquivalentTo": corpus.Value{Kind: corpus.KindArray, Items: []corpus.Value{str("Series: Legacy"), str("Серия: Спадок")}},
			"Languages.Count":                        intValue(2), "SeriesTitleInfo.Title==SeriesTitleInfo.TitleWithoutYear": boolValue(true),
		}},
		{"title year", "Series 2022 S03E14 720p HDTV X264-DIMENSION", map[string]corpus.Value{
			"SeriesTitleInfo.Year": intValue(2022), "SeriesTitleInfo.TitleWithoutYear": str("Series"), "SeriesTitleInfo.Title": str("Series 2022"),
		}},
		{"language contain", "Series.Title.S01E03.Ger.Dub.AAC.1080p.WebDL.x264-TKP21", map[string]corpus.Value{
			"Languages.Contain": corpus.Value{Kind: corpus.KindArray, Items: []corpus.Value{languageEnum("German")}},
		}},
		{"quality detection from the extension", "Series.S01E02.Chained.Title.mkv", map[string]corpus.Value{
			"Quality.SourceDetectionSource":     {Kind: corpus.KindEnum, Type: "QualityDetectionSource", Str: "Extension"},
			"Quality.ResolutionDetectionSource": {Kind: corpus.KindEnum, Type: "QualityDetectionSource", Str: "Extension"},
			"Quality.Quality.NotBe":             {Kind: corpus.KindEnum, Type: "Quality", Str: "Unknown"},
		}},
		{"rejected title is the explicit null", "Season 3", map[string]corpus.Value{"$result": null}},
		{"call that must not fail", "Show TV - Series Title : 02 Road From Code [S04].mp4", map[string]corpus.Value{"$result.NoThrow": truth}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runSeries(t, seriesCase("t", ownerSeriesTitle, tc.want, str(tc.input)), nil)
			if r.passed != 1 || r.failed != 0 || len(r.problems) != 0 {
				t.Errorf("run = %+v", r)
			}
		})
	}
}

// Every other entry point of the series title corpus resolves too.
func TestEpisodeCorpusConsumerResolvesEveryEntryPoint(t *testing.T) {
	null := corpus.Value{Kind: corpus.KindNull}
	for _, tc := range []struct {
		name, owner string
		input       string
		want        map[string]corpus.Value
	}{
		{"release group", ownerReleaseGroup, "Series.Title.S01E09.1080p.DSNP.WEB-DL.DDP2.0.H.264-VARYG", map[string]corpus.Value{titleResultKey: str("VARYG")}},
		{"no release group is null", ownerReleaseGroup, "Series.S02E01.720p.WEB-DL.DD5.1.H.264.mkv", map[string]corpus.Value{titleResultKey: null}},
		{"languages contain", ownerLanguages, "Title.the.Series.2009.S01E14.French.HDTV.XviD-LOL", map[string]corpus.Value{
			titleResultKey + ".Contain": {Kind: corpus.KindArray, Items: []corpus.Value{languageEnum("French")}}}},
		{"languages equivalent", ownerLanguages, "The Final Series (2020) S04 Complete 720p NF WEBRip [Hindi+English] Dual audio", map[string]corpus.Value{
			titleResultKey + ".EquivalentTo": {Kind: corpus.KindArray, Items: []corpus.Value{languageEnum("English"), languageEnum("Hindi")}}}},
		{"parsed series name normalized", ownerSeriesName, "[ www.Torrenting.com ] - Series.S03E14.720p.HDTV.X264-DIMENSION", map[string]corpus.Value{
			"SeriesName.CleanSeriesTitle": str("series")}},
		{"unparseable name is normalized instead", ownerSeriesName, "Bad Format", map[string]corpus.Value{
			"SeriesName.CleanSeriesTitle": str("badformat")}},
		{"clean title", ownerCleanSeries, "Carniv\u00E0le", map[string]corpus.Value{titleResultKey: str("carnivale")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if r := runSeries(t, seriesCase("t", tc.owner, tc.want, str(tc.input)), nil); r.passed != 1 || len(r.problems) != 0 {
				t.Errorf("run = %+v", r)
			}
		})
	}

	// A wrong expectation is a counted failure with the observed value, not a pass.
	wrong := seriesCase("w", ownerSeriesTitle, map[string]corpus.Value{"SeriesTitle": str("Other")}, str("Series.Title.S03E06.DVDRip.XviD-WiDE"))
	if r := runSeries(t, wrong, nil); r.failed != 1 || r.passed != 0 || len(r.problems) != 1 {
		t.Errorf("a wrong expectation must fail once: %+v", r)
	}
	// An unknown property, member or entry point is a hard error, never a skipped assertion.
	for name, cs := range map[string]corpus.Case{
		"unknown property": seriesCase("u1", ownerSeriesTitle, map[string]corpus.Value{"Nonsense": str("x")}, str("Series.S01E02")),
		"unknown language": seriesCase("u2", ownerLanguages, map[string]corpus.Value{titleResultKey + ".Contain": {
			Kind: corpus.KindArray, Items: []corpus.Value{languageEnum("Klingon")}}}, str("Series.S01E02.English")),
		"unknown owner": seriesCase("u3", "Parser.Other", map[string]corpus.Value{titleResultKey: str("x")}, str("Series.S01E02.English")),
		"wrong kind":    seriesCase("u4", ownerSeriesTitle, map[string]corpus.Value{"SeasonNumber": str("1")}, str("Series.S01E02")),
	} {
		if r := runSeries(t, cs, nil); r.failed != 1 || len(r.problems) == 0 {
			t.Errorf("%s must fail: %+v", name, r)
		}
	}
}

// A rejected title is the source's null: every property assertion dereferences
// it and fails, whatever it expects. Only the explicit $result null, the
// not-null assertion and the no-throw requirement are about the result itself.
func TestEpisodeCorpusMissingEpisodeFailsEveryPropertyAssertion(t *testing.T) {
	const hashed = "185d86a343e39f3341e35c4dad3f9959"
	null := corpus.Value{Kind: corpus.KindNull}
	zero := intValue(0)
	if r := runSeries(t, seriesCase("n", ownerSeriesTitle, map[string]corpus.Value{titleResultKey: null}, str(hashed)), nil); r.passed != 1 || r.failed != 0 {
		t.Fatalf("the explicit null result of a rejected title must pass: %+v", r)
	}
	if r := runSeries(t, seriesCase("nt", ownerSeriesTitle, map[string]corpus.Value{titleResultKey + ".NoThrow": boolValue(true)}, str(hashed)), nil); r.passed != 1 {
		t.Fatalf("a rejected title does not throw: %+v", r)
	}
	for name, want := range map[string]map[string]corpus.Value{
		"not null":              {titleResultKey + ".NotNull": boolValue(true)},
		"empty series title":    {"SeriesTitle": str("")},
		"null release group":    {"ReleaseGroup": null},
		"zero season":           {"SeasonNumber": zero},
		"null season":           {"SeasonNumber": null},
		"zero episodes":         {"EpisodeNumbers.Count": zero},
		"empty absolute":        {"AbsoluteEpisodeNumbers.Count": zero},
		"not a full season":     {"FullSeason": boolValue(false)},
		"no special":            {"Special": boolValue(false)},
		"first episode":         {"EpisodeNumbers.First()": zero},
		"possible special":      {"IsPossibleSpecialEpisode": boolValue(false)},
		"result null and a key": {titleResultKey: null, "SeriesTitle": str("")},
	} {
		r := runSeries(t, seriesCase("m", ownerSeriesTitle, want, str(hashed)), nil)
		if r.failed != 1 || r.passed != 0 || len(r.problems) == 0 {
			t.Errorf("%s: a missing episode must fail the assertion, got %+v", name, r)
		}
	}
}

// A single call or an index past the list throws in the source, so the assertion
// fails rather than reading a default.
func TestEpisodeCorpusElementReadsThatThrowFailTheAssertion(t *testing.T) {
	const multi = "Series.S03E01-06.DUAL.BDRip.XviD.AC3.-HELLYWOOD" // six episodes, no absolute number
	for name, want := range map[string]map[string]corpus.Value{
		"single of none":               {"AbsoluteEpisodeNumbers.Single()": intValue(0)},
		"single or default of several": {"EpisodeNumbers.SingleOrDefault()": intValue(1)},
		"first of none":                {"AbsoluteEpisodeNumbers.First()": intValue(0)},
	} {
		r := runSeries(t, seriesCase("e", ownerSeriesTitle, want, str(multi)), nil)
		// The unknown path is a hard error; the two reads that throw are mismatches.
		if r.failed != 1 || r.passed != 0 || len(r.problems) == 0 {
			t.Errorf("%s: must fail, got %+v", name, r)
		}
	}
	// First of an empty list is read as absent, never as zero.
	if r := runSeries(t, seriesCase("e", ownerSeriesTitle, map[string]corpus.Value{"EpisodeNumbers.First()": intValue(0)},
		str("[SubDESU]_Show_One_07_(1280x720_x264-AAC)_[6B7FD717]")), nil); r.failed != 1 || r.passed != 0 {
		t.Errorf("First() of no episode must fail even when it expects 0: %+v", r)
	}
}

func TestEpisodeCorpusDispositionBindsEveryMismatchingInput(t *testing.T) {
	cs := seriesCase("d", ownerSeriesTitle, map[string]corpus.Value{"SeasonNumber": intValue(9)},
		str("Series.Title.S03E06.DVDRip.XviD-WiDE"), str("Series.Title.S04E06.DVDRip.XviD-WiDE"))
	season := func(n int) map[string]corpus.Value { return map[string]corpus.Value{"SeasonNumber": intValue(n)} }
	bind := func(observed int) map[string]corpus.Disposition {
		return map[string]corpus.Disposition{"d": {CaseID: "d", Expected: season(9), Observed: season(observed)}}
	}
	if r := runSeries(t, cs, nil); r.failed != 1 || r.disposed != 0 {
		t.Errorf("an undisposed mismatch must fail: %+v", r)
	}
	// One observed value (3) does not cover the other input's (4).
	if r := runSeries(t, cs, bind(3)); r.failed != 1 || r.disposed != 0 {
		t.Errorf("a disposition must bind every distinct observation: %+v", r)
	}
	pass := seriesCase("d", ownerSeriesTitle, season(3), str("Series.Title.S03E06.DVDRip.XviD-WiDE"))
	if r := runSeries(t, pass, bind(3)); r.failed != 1 || r.passed != 0 {
		t.Errorf("a stale disposition must fail: %+v", r)
	}
	one := seriesCase("d", ownerSeriesTitle, season(9), str("Series.Title.S03E06.DVDRip.XviD-WiDE"))
	if r := runSeries(t, one, bind(3)); r.disposed != 1 || r.failed != 0 || len(r.problems) != 0 {
		t.Errorf("an exact disposition must bind the one observed value: %+v", r)
	}
}

// The consumer reconciles the whole inventory (all scopes) separately from the
// selected title scope and refuses a title case taken from a quality fixture.
func TestEpisodeCorpusConsumerReconcilesInventoryAndSelectedScopeSeparately(t *testing.T) {
	const name = "Series.Title.S03E06.DVDRip.XviD-WiDE"
	pass := map[string]corpus.Value{"SeasonNumber": intValue(3)}
	quality := mixedCase("q", corpus.ScopeQuality, true, map[string]corpus.Value{"Quality": qualityEnum("DVD")}, name)
	outside := mixedCase("o", corpus.ScopeOutOfScope, true, nil, name)
	disabled := seriesCase("t2", ownerSeriesTitle, pass, str(name))
	disabled.UpstreamEnabled = false
	c := seriesCorpus(seriesCase("t1", ownerSeriesTitle, pass, str(name)), disabled, quality, outside)
	now, loc := seriesRunClock(t)
	p := newSeriesTestParser(t, now, loc)
	tc := corpus.TimeContext{Now: now, Location: loc}
	r := runSeriesCorpus(p, c, nil, tc)
	if len(r.problems) != 0 || r.enabled != 1 || r.disabled != 1 || r.passed != 1 || r.otherEnabled != 1 || r.outOfScope != 1 {
		t.Fatalf("run = %+v", r)
	}
	if problems := r.reconcile(c.Header.Counts); len(problems) != 0 {
		t.Errorf("a consistent mixed inventory must reconcile: %v", problems)
	}
	counts := map[string]int{}
	for k, v := range c.Header.Counts {
		counts[k] = v
	}
	counts["cases"]++
	if problems := r.reconcile(counts); len(problems) == 0 {
		t.Error("a miscounted inventory must be reported")
	}
	none := seriesCorpus(quality, outside)
	if problems := runSeriesCorpus(p, none, nil, tc).reconcile(none.Header.Counts); len(problems) == 0 {
		t.Error("a corpus without an enabled title case must fail: quality names do not stand in")
	}
	subst := seriesCase("t3", ownerSeriesTitle, pass, str(name))
	subst.File = "tests/QualityParserFixture.cs"
	if r := runSeriesCorpus(p, seriesCorpus(subst), nil, tc); r.failed != 1 || len(r.problems) == 0 {
		t.Errorf("a title case from a quality fixture must fail: %+v", r)
	}
}

// longSeriesTail extends a release name with one long dotted word up to n bytes.
func longSeriesTail(name string, n int) string {
	return name + "." + strings.Repeat("a", n-len(name)-1)
}

// A legitimate name of exactly 4096 bytes is the intended episode. The control
// runs on the real clock (the 250 ms budget and the 50 ms match timeout are wall
// time), so it is excluded under -race with the complete corpus run.
func TestEpisodeBoundsLegitimate4096ByteNameParses(t *testing.T) {
	const control = "Series.Title.S03E06.1080p.BluRay.x264-GRP"
	name := longSeriesTail(control, 4096)
	if len(name) != 4096 {
		t.Fatalf("control is %d bytes", len(name))
	}
	pt, err := newTestParser(t, time.Now).ParseTitle(context.Background(), name, KindSeries)
	if err != nil {
		t.Fatalf("4096-byte control: %v", err)
	}
	e := pt.Episode
	if pt.Kind != KindSeries || e == nil || pt.Movie != nil || e.SeriesTitle != "Series Title" || !slices.Equal(e.EpisodeNumbers, []int{6}) ||
		!slices.Equal(e.SeasonNumbers, []int{3}) || e.ReleaseTitle != name || e.Quality.Code != "bluray-1080p" {
		t.Errorf("4096-byte control parsed as %+v", pt)
	}
}

// The symbolic dates of the source's daily tests, on the two adjacent local dates
// of an independent control: America/New_York, where on 2026-10-25 at 00:30 local
// the UTC date is already 2026-10-25 but DST ends a week later (2026-11-01).
// Expected names are worked out by hand: +14 is 2026-11-08 and +15 is 2026-11-09;
// an implementation that adds 14 x 24 hours to the instant reads 2026-11-07.
func TestEpisodeBoundsDailyLimitUsesTheLocalCalendarAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		now  time.Time
		// plus14 and plus15 are the +14 and +15 local dates, by hand.
		plus14, plus15 string
	}{
		{"just after midnight local", time.Date(2026, 10, 25, 0, 30, 0, 0, loc), "2026.11.08", "2026.11.09"},
		{"just before midnight local", time.Date(2026, 10, 25, 23, 30, 0, 0, loc), "2026.11.08", "2026.11.09"},
		{"the next local date", time.Date(2026, 10, 26, 0, 30, 0, 0, loc), "2026.11.09", "2026.11.10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newSeriesTestParser(t, tc.now, loc)
			ctx := context.Background()
			accepted, err := p.ParseTitle(ctx, "A.Talk.Show."+tc.plus14+".1080p.WEB-DL.H264-RlsGrp", KindSeries)
			if err != nil || accepted.Episode == nil || accepted.Episode.AirDate != strings.ReplaceAll(tc.plus14, ".", "-") {
				t.Errorf("+14 = %+v, %v; want the air date %s", accepted, err, tc.plus14)
			}
			_, err = p.ParseTitle(ctx, "A.Talk.Show."+tc.plus15+".1080p.WEB-DL.H264-RlsGrp", KindSeries)
			wantReason(t, err, codeTitleUnrecognized, map[string]int64{})

			// The shared materializer, driven by the same clock, produces those names
			// from the symbolic source IDs: +14 and +15 of the one template.
			mc := corpus.TimeContext{Now: tc.now, Location: loc}
			for off, want := range map[int]string{14: tc.plus14, 15: tc.plus15} {
				got, err := mc.Materialize(corpus.Value{Kind: corpus.KindDate, Offset: off, Template: "A.Talk.Show.{0:yyyy.MM.dd}.1080p.WEB-DL.H264-RlsGrp"})
				if err != nil || got != "A.Talk.Show."+want+".1080p.WEB-DL.H264-RlsGrp" {
					t.Errorf("Materialize(+%d) = %q, %v; want the date %s", off, got, err, want)
				}
			}
		})
	}
}

// A daily name whose date is not a calendar date is not a title: a 29 February
// of a year that is not a leap year, a 30 February and day zero are rejected as
// title_unrecognized with no parsed value, while 29 February of a leap year is
// the control that parses.
func TestEpisodeBoundsRejectsDatesOffTheCalendar(t *testing.T) {
	p := newSeriesTestParser(t, time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC), time.UTC)
	ctx := context.Background()
	const tail = ".1080p.WEB-DL.H264-RlsGrp"

	pt, err := p.ParseTitle(ctx, "A.Talk.Show.2024.02.29"+tail, KindSeries)
	if err != nil || pt.Episode == nil || pt.Episode.AirDate != "2024-02-29" {
		t.Fatalf("leap day control = %+v, %v; want the air date 2024-02-29", pt, err)
	}
	for _, tc := range []struct{ name, date string }{
		{"29 February of a non-leap year", "2023.02.29"},
		{"30 February of a leap year", "2024.02.30"},
		{"day zero", "2024.02.00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pt, err := p.ParseTitle(ctx, "A.Talk.Show."+tc.date+tail, KindSeries)
			wantReason(t, err, codeTitleUnrecognized, map[string]int64{})
			if pt != (ParsedTitle{}) {
				t.Errorf("%s parsed as %+v; want no value", tc.date, pt)
			}
		})
	}
}

// The derived methods of a parsed episode, asserted through the public
// ParseTitle with every expectation worked out by hand from the source's
// definitions: a daily release has an air date and no episode numbers (unknown
// release type); an absolute release has absolute numbers; episode 0 of a
// season other than 0 is a scene season special; a release without a season
// but with episodes is a mini series of season 1. Each method has a true and a
// false case, so a constant or a swapped condition fails.
func TestEpisodeCorpusDerivedMethods(t *testing.T) {
	p := newSeriesTestParser(t, time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC), time.UTC)
	for _, tc := range []struct {
		name                          string
		input                         string
		daily, absolute, sceneSpecial bool
		release                       ReleaseType
		mini                          bool
	}{
		{"single episode", "Series.Title.S03E06.1080p.BluRay.x264-GRP", false, false, false, ReleaseTypeSingleEpisode, false},
		{"multi episode", "Series.Title.S03E06E07.1080p.BluRay.x264-GRP", false, false, false, ReleaseTypeMultiEpisode, false},
		{"season pack", "Series.Title.S03.1080p.BluRay.x264-GRP", false, false, false, ReleaseTypeSeasonPack, false},
		{"daily", "A.Talk.Show.2024.02.29.1080p.WEB-DL.H264-RlsGrp", true, false, false, ReleaseTypeUnknown, false},
		{"absolute", "[SubDESU] High School DxD New 07 (1280x720 x264 AAC) [6B7FD717]", false, true, false, ReleaseTypeSingleEpisode, false},
		{"scene season special", "Series.Title.S02E00.720p.HDTV.x264-GRP", false, false, true, ReleaseTypeSingleEpisode, false},
		{"episode 0 of season 0", "Series.Title.S00E00.720p.HDTV.x264-GRP", false, false, false, ReleaseTypeSingleEpisode, false},
		{"mini series", "The.Big.Series.Leader.Part.2.DSR.XviD-SYS", false, false, false, ReleaseTypeSingleEpisode, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pt, err := p.ParseTitle(context.Background(), tc.input, KindSeries)
			if err != nil || pt.Episode == nil {
				t.Fatalf("ParseTitle(%q) = %+v, %v", tc.input, pt, err)
			}
			e := pt.Episode
			if got := e.IsDaily(); got != tc.daily {
				t.Errorf("IsDaily = %v, want %v", got, tc.daily)
			}
			if got := e.IsAbsoluteNumbering(); got != tc.absolute {
				t.Errorf("IsAbsoluteNumbering = %v, want %v", got, tc.absolute)
			}
			if got := e.IsPossibleSceneSeasonSpecial(); got != tc.sceneSpecial {
				t.Errorf("IsPossibleSceneSeasonSpecial = %v, want %v", got, tc.sceneSpecial)
			}
			if got := e.ReleaseType(); got != tc.release {
				t.Errorf("ReleaseType = %d, want %d", got, tc.release)
			}
			if e.IsMiniSeries != tc.mini {
				t.Errorf("IsMiniSeries = %v, want %v", e.IsMiniSeries, tc.mini)
			}
		})
	}
}

// An absolute range such as "01-12" has no ordinary episode numbers, so only the
// absolute clause of ReleaseType can make it a multi episode release.
func TestEpisodeCorpusAbsoluteRangeIsMultiEpisode(t *testing.T) {
	p := newSeriesTestParser(t, time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC), time.UTC)
	pt, err := p.ParseTitle(context.Background(), "[Doutei] Some Good - 01-12 [BD]", KindSeries)
	if err != nil || pt.Episode == nil {
		t.Fatalf("ParseTitle = %+v, %v", pt, err)
	}
	e := pt.Episode
	if len(e.EpisodeNumbers) != 0 {
		t.Errorf("EpisodeNumbers = %v, want none", e.EpisodeNumbers)
	}
	if want := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}; !slices.Equal(e.AbsoluteEpisodeNumbers, want) {
		t.Errorf("AbsoluteEpisodeNumbers = %v, want %v", e.AbsoluteEpisodeNumbers, want)
	}
	if !e.IsAbsoluteNumbering() {
		t.Error("IsAbsoluteNumbering = false, want true")
	}
	if got := e.ReleaseType(); got != ReleaseTypeMultiEpisode {
		t.Errorf("ReleaseType = %d, want %d", got, ReleaseTypeMultiEpisode)
	}
}
