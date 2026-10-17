//go:build !race

package corpus

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testdataDir = "../../../testdata/parser"

// lockedInventory pins the complete committed inventories: the live source
// commit each corpus was extracted from, its exact per-file case counts
// (independently counted from the source tests: TestCase attributes plus
// TestCaseSource rows plus plain tests), its per-scope counts and the
// per-owner counts of every scope (quality: QualityParser.ParseQuality; title:
// the series and movie title parsers and the release group and language parsers they run;
// out_of_scope: the other tests of the title fixtures, retained with their
// arguments). A re-extraction at another commit, or any lost or added case,
// must update this table together with the corpora.
var lockedInventory = map[string]struct {
	commit string
	cases  int
	files  map[string]int
	scopes map[string]int // scope -> cases
	owners map[string]int // scope|owner -> cases
}{
	"series": {"57816c149d02f95543973fefb4780c4495f8121c", 1597, map[string]int{
		"tests/QualityParserFixture.cs":               320,
		"tests/ExtendedQualityParserRegex.cs":         30,
		"tests/AnimeVersionFixture.cs":                6,
		"tests/ParserFixture.cs":                      48,
		"tests/SingleEpisodeParserFixture.cs":         185,
		"tests/MultiEpisodeParserFixture.cs":          77,
		"tests/MiniSeriesEpisodeParserFixture.cs":     11,
		"tests/SeasonParserFixture.cs":                72,
		"tests/SeasonTitleParserFixture.cs":           21,
		"tests/AbsoluteEpisodeNumberParserFixture.cs": 167,
		"tests/AnimeMetadataParserFixture.cs":         36,
		"tests/DailyEpisodeParserFixture.cs":          57,
		"tests/CrapParserFixture.cs":                  31,
		"tests/HashedReleaseFixture.cs":               20,
		"tests/ReleaseGroupParserFixture.cs":          152,
		"tests/SubGroupParserFixture.cs":              1,
		"tests/UnicodeReleaseParserFixture.cs":        56,
		"tests/UrlFixture.cs":                         31,
		"tests/SeriesTitleInfoFixture.cs":             5,
		"tests/IsPossibleSpecialEpisodeFixture.cs":    7,
		"tests/LanguageParserFixture.cs":              221,
		"tests/NormalizeSeriesTitleFixture.cs":        43,
	}, map[string]int{
		ScopeQuality: 356, ScopeTitle: 1166, ScopeOutOfScope: 75,
	}, map[string]int{
		ScopeQuality + "|QualityParser.ParseQuality":                         356,
		ScopeTitle + "|Parser.ParseTitle":                                    754,
		ScopeTitle + "|Parser.ParseSeriesName":                               33,
		ScopeTitle + "|Parser.CleanSeriesTitle":                              44,
		ScopeTitle + "|ReleaseGroupParser.ParseReleaseGroup":                 166,
		ScopeTitle + "|LanguageParser.ParseLanguages":                        169,
		ScopeOutOfScope + "|Parser.ParseTitle":                               3,
		ScopeOutOfScope + "|Parser.ParsePath":                                22,
		ScopeOutOfScope + "|Parser.ParseSeasonTitle":                         15,
		ScopeOutOfScope + "|ParsedEpisodeInfo.IsPossibleSpecialEpisode":      2,
		ScopeOutOfScope + "|LanguageParser.ParseSubtitleLanguage":            12,
		ScopeOutOfScope + "|LanguageParser.ParseSubtitleLanguageInformation": 21,
	}},
	"movie": {"df2faa64c75e6081556d11232dc46523e7264ea1", 892, map[string]int{
		"tests/QualityParserFixture.cs":       287,
		"tests/ExtendedQualityParserRegex.cs": 30,
		"tests/AnimeVersionFixture.cs":        6,
		"tests/ParserFixture.cs":              112,
		"tests/EditionParserFixture.cs":       49,
		"tests/LanguageParserFixture.cs":      195,
		"tests/CrapParserFixture.cs":          26,
		"tests/HashedReleaseFixture.cs":       11,
		"tests/ReleaseGroupParserFixture.cs":  154,
		"tests/UrlFixture.cs":                 22,
	}, map[string]int{
		ScopeQuality: 323, ScopeTitle: 513, ScopeOutOfScope: 56,
	}, map[string]int{
		ScopeQuality + "|QualityParser.ParseQuality":                         323,
		ScopeTitle + "|Parser.ParseMovieTitle":                               321,
		ScopeTitle + "|ReleaseGroupParser.ParseReleaseGroup":                 161,
		ScopeTitle + "|LanguageParser.ParseLanguages":                        30,
		ScopeTitle + "|Parser.CleanMovieTitle":                               1,
		ScopeOutOfScope + "|Parser.ParseMovieTitle":                          3,
		ScopeOutOfScope + "|Parser.NormalizeImdbId":                          6,
		ScopeOutOfScope + "|Parser.ParseMoviePath":                           11,
		ScopeOutOfScope + "|LanguageParser.ParseSubtitleLanguage":            15,
		ScopeOutOfScope + "|LanguageParser.ParseSubtitleLanguageInformation": 21,
	}},
}

// qualityFixtures are the source files of the quality scope.
var qualityFixtures = map[string]bool{
	"tests/QualityParserFixture.cs": true, "tests/ExtendedQualityParserRegex.cs": true, "tests/AnimeVersionFixture.cs": true,
}

// TestCorpusFull reads the committed corpora through the canonical reader and
// reconciles their complete inventories, attribution and dispositions.
func TestCorpusFull(t *testing.T) {
	var all []Case
	for _, app := range []string{"series", "movie"} {
		want := lockedInventory[app]
		data, err := os.ReadFile(filepath.Join(testdataDir, app+".jsonl"))
		if err != nil {
			t.Fatalf("%s: %v", app, err)
		}
		if len(data) > MaxFileBytes {
			t.Fatalf("%s: %d bytes exceed the %d byte reader cap", app, len(data), MaxFileBytes)
		}
		c, err := Read(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: %v", app, err)
		}
		h := c.Header
		if h.App != app || h.SourceCommit != want.commit || h.License != "GPL-3.0" || h.SourceRepository != "env:BOXSET_REF_"+strings.ToUpper(app) {
			t.Errorf("%s: attribution %+v", app, h)
		}
		counts := h.Counts
		if len(c.Cases) != want.cases || counts["cases"] != want.cases || counts["enabled"]+counts["disabled"]+counts["out_of_scope"] != want.cases {
			t.Errorf("%s: cases=%d counts=%v, want %d cases", app, len(c.Cases), counts, want.cases)
		}
		if counts["enabled"] == 0 {
			t.Errorf("%s: no enabled case", app)
		}
		rawRows := 0
		for file, n := range want.files {
			if counts["cases:"+file] != n {
				t.Errorf("%s: %s has %d cases, want %d", app, file, counts["cases:"+file], n)
			}
			rawRows += counts["source:testcase_attrs:"+file] + counts["source:source_rows:"+file] + counts["source:plain_tests:"+file]
		}
		// The extractor's independent raw attribute/row/plain-test tally equals the case count.
		if rawRows != want.cases {
			t.Errorf("%s: raw TestCase attributes plus source rows plus plain tests = %d, want %d", app, rawRows, want.cases)
		}
		perScope, perOwner := map[string]int{}, map[string]int{}
		for _, cs := range c.Cases {
			perScope[cs.Scope]++
			perOwner[cs.Scope+"|"+cs.ScopeOwner]++
			// Quality cases come only from the quality fixtures, never a title case from one.
			if cs.ScopeOwner == "" || (cs.Scope == ScopeQuality) != qualityFixtures[cs.File] {
				t.Errorf("%s: case %s scope/file: %+v", app, cs.ID, cs)
			}
			_, err := cs.Inputs(TimeContext{Location: nil})
			if cs.Scope == ScopeOutOfScope {
				// Retained with its source arguments and no recorded input or assertion.
				if err == nil || len(cs.Expected) != 1 || len(cs.Args) == 0 && cs.Entry != "Test" {
					t.Errorf("%s: out-of-scope case %s: %+v (%v)", app, cs.ID, cs, err)
				}
				continue
			}
			// Release names are plain text but for the symbolic dates of the series daily
			// names, which only a calendar can materialize.
			names, err := cs.Inputs(TimeContext{Now: time.Unix(0, 0).UTC(), Location: time.UTC})
			if err != nil || len(names) == 0 || len(cs.Expected) < 2 {
				t.Errorf("%s: case %s inputs/expectation: %v %+v", app, cs.ID, err, cs)
			}
		}
		for scope, n := range want.scopes {
			if perScope[scope] != n {
				t.Errorf("%s: scope %s has %d cases, want %d", app, scope, perScope[scope], n)
			}
		}
		for owner, n := range want.owners {
			if perOwner[owner] != n {
				t.Errorf("%s: %s has %d cases, want %d", app, owner, perOwner[owner], n)
			}
		}
		if len(perScope) != len(want.scopes) || len(perOwner) != len(want.owners) {
			t.Errorf("%s: scopes %v owners %v, want %v %v", app, perScope, perOwner, want.scopes, want.owners)
		}
		canonical, err := Marshal(c)
		if err != nil || !bytes.Equal(canonical, data) {
			t.Errorf("%s: the committed bytes are not the canonical encoding (%v)", app, err)
		}
		sum := sha256.Sum256(data)
		t.Logf("corpus-full app=%s cases=%d enabled=%d disabled=%d out_of_scope=%d bytes=%d sha256=%s",
			app, len(c.Cases), counts["enabled"], counts["disabled"], counts["out_of_scope"], len(data), hex.EncodeToString(sum[:]))
		all = append(all, c.Cases...)
	}

	f, err := os.Open(filepath.Join(testdataDir, "dispositions.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	d, err := ReadDispositions(f)
	if err != nil {
		t.Fatalf("dispositions: %v", err)
	}
	if err := d.Validate(&Corpus{Cases: all}); err != nil {
		t.Errorf("dispositions: %v", err)
	}
	t.Logf("corpus-full dispositions=%d", len(d.Dispositions))
}
