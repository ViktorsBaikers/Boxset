package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/parse/corpus"
)

const (
	testCommit = "0123456789abcdef0123456789abcdef01234567"
	fixtureRel = "tests/QualityParserFixture.cs"
)

// A synthetic source fixture with real NUnit forms: direct TestCase rows
// (duplicates, verbatim and escaped strings, a multi-line attribute), an
// ignored method, a TestCaseSource with a loop, a plain test, symbolic dates
// and a helper that carries the shared assertions.
const fixtureQuality = `using FluentAssertions;
using NUnit.Framework;

namespace Fixture.ParserTests
{
    [TestFixture]
    public class QualityParserFixture
    {
        public static object[] OtherSourceCases =
        {
            new object[] { "SD TV", Quality.SDTV },
            new object[] { "720p BluRay", Quality.Bluray720p },
        };

        [TestCase("Show.S01E01.HDTV.x264-GRP", false)]
        [TestCase("Show.S01E01.PROPER.HDTV.x264-GRP", true)]
        [TestCase("Show.S01E01.HDTV.x264-GRP", false)]
        [TestCase("Show.S01E01.HDTV.x264-GRP", false)]
        [TestCase(@"E:\Downloads\tv\Show.S01E01.avi", false)]
        [TestCase(
            "Multi \"Line\" Case",
            false)]
        public void should_parse_sdtv(string title, bool proper)
        {
            ParseAndVerifyQuality(title, Quality.SDTV, proper);
        }

        [Ignore("not run upstream")]
        [TestCase("Ignored.Case", false)]
        public void ignored_method(string title, bool proper)
        {
            ParseAndVerifyQuality(title, Quality.SDTV, proper);
        }

        [Test]
        [TestCaseSource(nameof(OtherSourceCases))]
        public void should_parse_other_source(string qualityString, Quality quality)
        {
            foreach (var c in new char[] { '-', '.', ' ' })
            {
                var title = string.Format("My series S01E01 {0}", qualityString.Replace(' ', c));

                ParseAndVerifyQuality(title, quality, false);
            }
        }

        [Test]
        public void plain_test()
        {
            var result = QualityParser.ParseQuality("Plain.Name.S01E01.720p");
            result.Quality.Should().Be(Quality.Bluray720p);
        }

        [TestCase(14)]
        [TestCase(15)]
        public void symbolic_dates(int days)
        {
            var airDate = DateTime.Today.AddDays(days);
            var title = string.Format("A.Talk.Show.{0:yyyy.MM.dd}.1080p.WEB-DL.H264-Group", airDate);
            QualityParser.ParseQuality(title).Quality.Should().Be(Quality.SDTV);
        }

        private void ParseAndVerifyQuality(string title, Quality quality, bool proper)
        {
            var result = QualityParser.ParseQuality(title);
            result.Quality.Should().Be(quality);

            var version = proper ? 2 : 1;
            result.Revision.Version.Should().Be(version);
        }
    }
}
`

const fixtureExtended = `using NUnit.Framework;

namespace Fixture.ParserTests
{
    [TestFixture]
    public class ExtendedQualityParserRegex
    {
        [SetUp]
        public void Setup()
        {
            // nothing to set up
        }

        [TestCase("Title.REAL.PROPER.HDTV", 1)]
        [TestCase("Title.HDTV", 0)]
        public void should_parse_reality_from_title(string title, int reality)
        {
            QualityParser.ParseQuality(title).Revision.Real.Should().Be(reality);
        }
    }
}
`

const fixtureAnime = `using NUnit.Framework;

namespace Fixture.ParserTests
{
    [TestFixture]
    public class AnimeVersionFixture
    {
        [TestCase("[Group] Anime - 01v2 (1080p)", 2)]
        public void should_be_able_to_parse_repack(string title, int version)
        {
            var result = QualityParser.ParseQuality(title);
            result.Revision.Version.Should().Be(version);
        }
    }
}
`

// Synthetic title fixtures with the real forms of the movie title tests:
// ParseMovieTitle cases asserted through property chains, an assertion scope,
// a loop over the alternative titles with indexers and an isDir argument,
// membership and count assertions on the languages, a null result, an ignored
// method, and the out-of-scope tests of the same file (another entry point, a
// normalization helper, run-time generated names).
const fixtureTitleParser = `using System.Linq;
using FluentAssertions;
using FluentAssertions.Execution;
using NUnit.Framework;

namespace Fixture.ParserTests
{
    [TestFixture]
    public class ParserFixture
    {
        [TestCase("The.Movie.2015.1080p.BluRay.x264-GRP", "The Movie")]
        [TestCase("Anime Movie (1999)", "Anime Movie")]
        public void should_parse_movie_title(string postTitle, string title)
        {
            Parser.Parser.ParseMovieTitle(postTitle).PrimaryMovieTitle.Should().Be(title);
        }

        [TestCase("[Group] Some Movie (2016) v2 [97681524].mkv", "Some Movie", "Group", 2016)]
        public void should_parse_anime_movie_title(string postTitle, string title, string releaseGroup, int year)
        {
            var movie = Parser.Parser.ParseMovieTitle(postTitle);
            using (new AssertionScope())
            {
                movie.PrimaryMovieTitle.Should().Be(title);
                movie.ReleaseGroup.Should().Be(releaseGroup);
                movie.Year.Should().Be(year);
            }
        }

        [TestCase("[www.test.com] - Movie.Title.2023.720p.HDTV.X264-DIMENSION", "Movie Title")]
        [TestCase("test.net - Movie.Title.2023.720p.HDTV.X264-DIMENSION", "Movie Title")]
        public void should_not_parse_url_in_name(string postTitle, string title)
        {
            var result = Parser.Parser.ParseMovieTitle(postTitle).MovieTitle.CleanMovieTitle();
            result.Should().Be(title.CleanMovieTitle());
        }

        [TestCase("Movie.One.AKA.Movie.Two.1965.DVDRip.XviD-KG.avi",
            new string[]
            {
                "Movie One AKA Movie Two",
                "Movie One",
                "Movie Two"
            })]
        public void should_parse_movie_alternative_titles(string postTitle, string[] parsedTitles)
        {
            var movieInfo = Parser.Parser.ParseMovieTitle(postTitle, true);

            movieInfo.MovieTitles.Count.Should().Be(parsedTitles.Length);

            for (var i = 0; i < movieInfo.MovieTitles.Count; i += 1)
            {
                movieInfo.MovieTitles[i].Should().Be(parsedTitles[i]);
            }
        }

        [TestCase("Movie.Title.1994.French.English.1080p.XviD-LOL")]
        public void should_parse_language_french_english(string postTitle)
        {
            var result = Parser.Parser.ParseMovieTitle(postTitle, true);

            result.Languages.Should().Contain(Language.French);
            result.Languages.Should().Contain(Language.English);
            result.Languages.Should().Contain(Language.French);
        }

        [TestCase("The.German.Doctor.2013.LIMITED.DVDRip.x264-RedBlade")]
        public void should_not_parse_wrong_language_in_title(string postTitle)
        {
            var parsed = Parser.Parser.ParseMovieTitle(postTitle, true);
            parsed.Languages.Count.Should().Be(1);
            parsed.Languages.First().Should().Be(Language.Unknown);
        }

        [TestCase("76El6LcgLzqb426WoVFg1vVVVGx4uCYopQkfjmLe")]
        public void should_not_parse_crap(string title)
        {
            Parser.Parser.ParseMovieTitle(title).Should().BeNull();
            ExceptionVerification.IgnoreWarns();
        }

        [Ignore("not run upstream")]
        [TestCase("Ignored.Movie.2000.720p", "Ignored Movie")]
        public void ignored_title(string postTitle, string title)
        {
            Parser.Parser.ParseMovieTitle(postTitle).PrimaryMovieTitle.Should().Be(title);
        }

        [TestCase("1234567", "tt1234567")]
        public void should_normalize_imdbid(string imdbid, string normalized)
        {
            Parser.Parser.NormalizeImdbId(imdbid).Should().BeEquivalentTo(normalized);
        }

        [Test]
        public void should_remove_accents_from_title()
        {
            const string title = "Carniv\u00E0le";

            title.CleanMovieTitle().Should().Be("carnivale");
        }

        [Test]
        public void should_not_parse_md5()
        {
            var hash = "SEED";
            var hashAlgo = System.Security.Cryptography.MD5.Create();
            var success = 0;
            for (var i = 0; i < 100; i++)
            {
                hash = BitConverter.ToString(hashAlgo.ComputeHash(Encoding.Default.GetBytes(hash))).Replace("-", "");

                if (Parser.Parser.ParseMovieTitle(hash) == null)
                {
                    success++;
                }
            }

            success.Should().Be(100);
        }
    }
}
`

const fixtureTitleEdition = `using FluentAssertions;
using NUnit.Framework;

namespace Fixture.ParserTests
{
    [TestFixture]
    public class EditionParserFixture
    {
        [TestCase("Movie Title 2012 Directors Cut", "Directors Cut")]
        public void should_parse_edition(string postTitle, string edition)
        {
            var parsed = Parser.Parser.ParseMovieTitle(postTitle);
            parsed.Edition.Should().Be(edition);
        }
    }
}
`

const fixtureTitleGroups = `using FluentAssertions;
using NUnit.Framework;

namespace Fixture.ParserTests
{
    [TestFixture]
    public class ReleaseGroupParserFixture
    {
        [TestCase("Movie.Name.2022.1080p.BluRay.x264-VARYG", "VARYG")]
        [TestCase("Movie.Name.2022.1080p.BluRay.x264", null)]
        public void should_parse_release_group(string title, string expected)
        {
            Parser.ReleaseGroupParser.ParseReleaseGroup(title).Should().Be(expected);
        }

        [TestCase("Terrible.Anime.Title.2020.DBOX.480p.x264-iKaos [v3] [6AFFEF6B]")]
        public void should_not_parse_anime_hash_as_release_group(string title)
        {
            Parser.ReleaseGroupParser.ParseReleaseGroup(title).Should().BeNull();
        }
    }
}
`

const fixtureTitleLanguages = `using System.Linq;
using FluentAssertions;
using NUnit.Framework;

namespace Fixture.ParserTests
{
    [TestFixture]
    public class LanguageParserFixture
    {
        [TestCase("Movie Title 1994 French 1080p")]
        public void should_parse_language_french(string postTitle)
        {
            var result = LanguageParser.ParseLanguages(postTitle);
            result.Should().Contain(Language.French);
        }

        [TestCase("Movie title 7x4 audio latino")]
        public void should_parse_language_spanish_latino(string postTitle)
        {
            var result = LanguageParser.ParseLanguages(postTitle);
            result.First().Id.Should().Be(Language.SpanishLatino.Id);
        }

        [TestCase("Movie Title - 2022.en.sub")]
        public void should_parse_subtitle_language_english(string fileName)
        {
            var result = LanguageParser.ParseSubtitleLanguage(fileName);
            result.Should().Be(Language.English);
        }

        [TestCase("Name (2020) - [AAC 2.0].testtitle.default.eng.forced.ass", new[] { "default", "forced" }, "testtitle", "English")]
        [TestCase("Name (2020) - [AAC 2.0].ru-something-else.srt", new string[0], "something-else", "Russian")]
        public void should_parse_title_and_tags(string postTitle, string[] expectedTags, string expectedTitle, string expectedLanguage)
        {
            var subtitleTitleInfo = LanguageParser.ParseSubtitleLanguageInformation(postTitle);

            subtitleTitleInfo.LanguageTags.Should().BeEquivalentTo(expectedTags);
            subtitleTitleInfo.Language.Should().BeEquivalentTo((Language)expectedLanguage);
        }
    }
}
`

const fixtureTitleHashed = `using FluentAssertions;
using NUnit.Framework;

namespace Fixture.ParserTests
{
    [TestFixture]
    public class HashedReleaseFixture
    {
        public static object[] HashedReleaseParserCases =
        {
            new object[]
            {
                @"C:\Test\Some.Hashed.Release.2018.720p-Mercury\0e895c37.mkv".AsOsAgnostic(),
                "Some Hashed Release",
                Quality.SDTV,
                "Mercury"
            },
            new object[]
            {
                @"C:\Test\Movie.2018\abc.mkv".AsOsAgnostic(),
                "Movie",
                Quality.Bluray720p,
                null
            }
        };

        [Test]
        [TestCaseSource(nameof(HashedReleaseParserCases))]
        public void should_properly_parse_hashed_releases(string path, string title, Quality quality, string releaseGroup)
        {
            var result = Parser.Parser.ParseMoviePath(path);
            result.PrimaryMovieTitle.Should().Be(title);
        }
    }
}
`

// fixtureTitleEmpty stands in for a title fixture a test does not exercise.
const fixtureTitleEmpty = "using NUnit.Framework;\n"

const (
	// modelLanguages names the Language members the parser produces; the
	// Language declaration itself is not part of the delivered areas.
	modelLanguages = `namespace Fixture.Parser
{
    public static class LanguageParser
    {
        public static List<Language> ParseLanguages(string title)
        {
            languages.Add(Language.English);
            languages.Add(Language.French);
            languages.Add(Language.Unknown);
            languages.Add(Language.SpanishLatino);
        }
    }
}
`
	// provenanceStub is a ported source recorded for attribution only: it holds
	// a form the lexer rejects, so reading it proves it is never interpreted.
	provenanceStub = "// ported source\nvar s = $\"{title}\";\n"
)

const (
	modelResolution = `namespace Fixture.Parser
{
    public class QualityParser { }

    public enum Resolution
    {
        Unknown,
        R480p = 480,
        R720p = 720
    }
}
`
	modelQuality = `namespace Fixture.Qualities
{
    public class Quality
    {
        public static Quality Unknown => new Quality(0, "Unknown", QualitySource.Unknown, 0);
        public static Quality SDTV => new Quality(1, "SDTV", QualitySource.Television, 480); // trailing
        public static Quality Bluray720p
        {
            get { return new Quality(6, "Bluray-720p", QualitySource.Bluray, 720); }
        }
    }
}
`
	modelSource    = "namespace Fixture.Qualities\n{\n    public enum QualitySource\n    {\n        Unknown,\n        Television,\n        Bluray\n    }\n}\n"
	modelDetection = "namespace Fixture.Qualities\n{\n    public enum QualityDetectionSource\n    {\n        Unknown,\n        Name,\n        Extension,\n        MediaInfo\n    }\n}\n"
	modelModifier  = "namespace Fixture.Qualities\n{\n    public enum Modifier\n    {\n        NONE = 0,\n        REMUX\n    }\n}\n"
)

// writeTree lays out a synthetic source tree for spec under a fresh directory.
func writeTree(t *testing.T, spec appSpec, quality string) string {
	t.Helper()
	root := t.TempDir()
	content := map[string]string{
		spec.tests[0]: quality,
		spec.tests[1]: fixtureExtended,
		spec.tests[2]: fixtureAnime,
	}
	for _, m := range spec.models {
		switch {
		case strings.HasSuffix(m, "QualityParser.cs"):
			content[m] = modelResolution
		case strings.HasSuffix(m, "/Quality.cs"):
			content[m] = modelQuality
		case strings.HasSuffix(m, "QualitySource.cs"):
			content[m] = modelSource
		case strings.HasSuffix(m, "QualityDetectionSource.cs"):
			content[m] = modelDetection
		case strings.HasSuffix(m, "Modifier.cs"):
			content[m] = modelModifier
		default:
			t.Fatalf("unexpected model path %s", m)
		}
	}
	for _, rel := range spec.titles {
		switch rel {
		case "tests/ParserFixture.cs":
			content[rel] = fixtureTitleParser
		case "tests/EditionParserFixture.cs":
			content[rel] = fixtureTitleEdition
		case "tests/ReleaseGroupParserFixture.cs":
			content[rel] = fixtureTitleGroups
		case "tests/LanguageParserFixture.cs":
			content[rel] = fixtureTitleLanguages
		case "tests/HashedReleaseFixture.cs":
			content[rel] = fixtureTitleHashed
		default:
			content[rel] = fixtureTitleEmpty
		}
	}
	if spec.languages != "" {
		content[spec.languages] = modelLanguages
	}
	for _, rel := range spec.provenance {
		content[rel] = provenanceStub
	}
	for rel, text := range content {
		writeSource(t, root, rel, text)
	}
	return root
}

// writeSource writes one synthetic source file under root.
func writeSource(t *testing.T, root, rel, text string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func lineOf(t *testing.T, src, needle string) int {
	t.Helper()
	i := strings.Index(src, needle)
	if i < 0 {
		t.Fatalf("needle %q not in fixture", needle)
	}
	return strings.Count(src[:i], "\n") + 1
}

func extractSynthetic(t *testing.T, spec appSpec) *corpus.Corpus {
	t.Helper()
	root := writeTree(t, spec, fixtureQuality)
	c, _, err := extractApp(spec, root, testCommit)
	if err != nil {
		t.Fatalf("extractApp: %v", err)
	}
	return c
}

func caseBy(t *testing.T, c *corpus.Corpus, method, entry string) corpus.Case {
	t.Helper()
	for _, cs := range c.Cases {
		if cs.Method == method && cs.Entry == entry {
			return cs
		}
	}
	t.Fatalf("no case %s %s", method, entry)
	return corpus.Case{}
}

func str(s string) corpus.Value      { return corpus.Value{Kind: corpus.KindString, Str: s} }
func num(n int64) corpus.Value       { return corpus.Value{Kind: corpus.KindInt, Int: n} }
func enum(ty, m string) corpus.Value { return corpus.Value{Kind: corpus.KindEnum, Type: ty, Str: m} }

func wantValue(t *testing.T, got map[string]corpus.Value, key string, want corpus.Value) {
	t.Helper()
	if g, ok := got[key]; !ok || !corpus.Equal(g, want) {
		t.Errorf("expected[%s] = %+v, want %+v", key, got[key], want)
	}
}

func TestExtractRecordsEverySourceCase(t *testing.T) {
	for _, spec := range appSpecs() {
		t.Run(spec.name, func(t *testing.T) {
			c := extractSynthetic(t, spec)
			// Round-trip through the one canonical reader.
			data, err := corpus.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			got, err := corpus.Read(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("corpus.Read: %v", err)
			}
			h := got.Header
			if h.App != spec.name || h.SourceRepository != spec.repository || h.SourceCommit != testCommit || h.License != "GPL-3.0" {
				t.Errorf("attribution lost: %+v", h)
			}
			// The quality fixtures give 15 cases (14 enabled, 1 ignored); the movie side's title
			// fixtures add 24: 16 enabled title cases, 1 ignored title case and 7 out of scope.
			wantCounts := map[string]int{"cases": 15, "enabled": 14, "disabled": 1, "out_of_scope": 0}
			if len(spec.titles) > 0 {
				wantCounts = map[string]int{"cases": 39, "enabled": 30, "disabled": 2, "out_of_scope": 7}
			}
			for k, want := range wantCounts {
				if h.Counts[k] != want {
					t.Errorf("counts[%s] = %d, want %d", k, h.Counts[k], want)
				}
			}
			if h.Counts["cases:"+fixtureRel] != 12 || h.Counts["disabled:"+fixtureRel] != 1 ||
				h.Counts["source:testcase_attrs:"+fixtureRel] != 9 || h.Counts["source:source_rows:"+fixtureRel] != 2 ||
				h.Counts["source:setup_methods:"+spec.tests[1]] != 1 {
				t.Errorf("per-file counts: %v", h.Counts)
			}

			// Direct rows keep order, duplicates and exact arguments.
			sdtv := []corpus.Case{}
			for _, cs := range got.Cases {
				if cs.Method == "should_parse_sdtv" {
					sdtv = append(sdtv, cs)
				}
			}
			if len(sdtv) != 6 {
				t.Fatalf("should_parse_sdtv rows = %d, want 6", len(sdtv))
			}
			ids := map[string]bool{}
			for _, cs := range sdtv {
				ids[cs.ID] = true
			}
			if len(ids) != 6 {
				t.Errorf("duplicate rows must keep distinct identities: %v", ids)
			}
			prefix := strings.TrimSuffix(sdtv[0].ID, ":0")
			if sdtv[2].ID != prefix+":1" || sdtv[3].ID != prefix+":2" {
				t.Errorf("duplicate ordinals: %s %s %s", sdtv[0].ID, sdtv[2].ID, sdtv[3].ID)
			}
			if !corpus.Equal(sdtv[4].Args[0], str(`E:\Downloads\tv\Show.S01E01.avi`)) {
				t.Errorf("verbatim string: %+v", sdtv[4].Args[0])
			}
			multi := sdtv[5]
			if !corpus.Equal(multi.Args[0], str(`Multi "Line" Case`)) || multi.Line != lineOf(t, fixtureQuality, "[TestCase(\n") {
				t.Errorf("multiline attribute: args=%+v line=%d", multi.Args, multi.Line)
			}
			proper := sdtv[1]
			wantValue(t, proper.Expected, "Revision.Version", num(2))
			wantValue(t, proper.Expected, "Quality", enum("Quality", "SDTV"))
			wantValue(t, proper.Expected, corpus.InputKey, corpus.Value{Kind: corpus.KindArray, Items: []corpus.Value{str("Show.S01E01.PROPER.HDTV.x264-GRP")}})
			wantValue(t, sdtv[0].Expected, "Revision.Version", num(1))
			if sdtv[0].Entry != "TestCase#1" || !sdtv[0].UpstreamEnabled || sdtv[0].Scope != corpus.ScopeQuality {
				t.Errorf("case status: %+v", sdtv[0])
			}

			// Upstream-ignored cases are retained, disabled, never relabelled.
			ign := caseBy(t, got, "ignored_method", "TestCase#1")
			if ign.UpstreamEnabled {
				t.Error("ignored case must stay disabled")
			}
			wantValue(t, ign.Expected, "Quality", enum("Quality", "SDTV"))

			// TestCaseSource rows, enum arguments, derived inputs from the loop.
			src := caseBy(t, got, "should_parse_other_source", "TestCaseSource:OtherSourceCases#1")
			if len(src.Args) != 2 || !corpus.Equal(src.Args[1], enum("Quality", "SDTV")) {
				t.Errorf("source args: %+v", src.Args)
			}
			wantValue(t, src.Expected, corpus.InputKey, corpus.Value{Kind: corpus.KindArray, Items: []corpus.Value{
				str("My series S01E01 SD-TV"), str("My series S01E01 SD.TV"), str("My series S01E01 SD TV"),
			}})
			wantValue(t, src.Expected, "Quality", enum("Quality", "SDTV"))

			// Plain test: no arguments, entry Test.
			plain := caseBy(t, got, "plain_test", "Test")
			if len(plain.Args) != 0 {
				t.Errorf("plain args: %+v", plain.Args)
			}
			wantValue(t, plain.Expected, "Quality", enum("Quality", "Bluray720p"))

			// Other fixture files and helper-free assertions.
			real := caseBy(t, got, "should_parse_reality_from_title", "TestCase#1")
			wantValue(t, real.Expected, "Revision.Real", num(1))
			if real.File != spec.tests[1] {
				t.Errorf("file attribution: %s", real.File)
			}
		})
	}
}

func TestExtractSymbolicDatesStayStable(t *testing.T) {
	c := extractSynthetic(t, appSpecs()[0])
	plus14 := caseBy(t, c, "symbolic_dates", "TestCase#1")
	plus15 := caseBy(t, c, "symbolic_dates", "TestCase#2")
	in14 := plus14.Expected[corpus.InputKey].Items[0]
	in15 := plus15.Expected[corpus.InputKey].Items[0]
	const tmpl = "A.Talk.Show.{0:yyyy.MM.dd}.1080p.WEB-DL.H264-Group"
	if in14.Kind != corpus.KindDate || in14.Offset != 14 || in14.Template != tmpl || in15.Offset != 15 {
		t.Fatalf("symbolic inputs: %+v %+v", in14, in15)
	}
	if plus14.ID == plus15.ID || strings.Contains(plus14.ID, "2026") {
		t.Errorf("identities must be symbolic and distinct: %s %s", plus14.ID, plus15.ID)
	}

	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		now            time.Time
		want14, want15 string
	}{
		// DST ends 2026-11-01: calendar days, not 24-hour durations.
		{time.Date(2026, 10, 25, 0, 30, 0, 0, ny), "A.Talk.Show.2026.11.08.1080p.WEB-DL.H264-Group", "A.Talk.Show.2026.11.09.1080p.WEB-DL.H264-Group"},
		{time.Date(2026, 10, 26, 0, 30, 0, 0, ny), "A.Talk.Show.2026.11.09.1080p.WEB-DL.H264-Group", "A.Talk.Show.2026.11.10.1080p.WEB-DL.H264-Group"},
	} {
		ctx := corpus.TimeContext{Now: tc.now, Location: ny}
		got14, err14 := ctx.Materialize(in14)
		got15, err15 := ctx.Materialize(in15)
		if err14 != nil || err15 != nil || got14 != tc.want14 || got15 != tc.want15 {
			t.Errorf("now=%s: +14=%q (%v) +15=%q (%v)", tc.now, got14, err14, got15, err15)
		}
	}
	// The extraction bytes never depend on the extraction day.
	a, _ := corpus.Marshal(c)
	b, _ := corpus.Marshal(extractSynthetic(t, appSpecs()[0]))
	if !bytes.Equal(a, b) {
		t.Error("repeated extraction must be byte-identical")
	}
}

func TestExtractRejectsUnsupportedFormsWithLocation(t *testing.T) {
	const method = `
        [TestCase("Show.HDTV")]
        public void case_under_test(string title)
        {
            %BODY%
        }
`
	wrap := func(members string) string {
		return "namespace N\n{\n    [TestFixture]\n    public class QualityParserFixture\n    {" + members + "    }\n}\n"
	}
	body := func(b string) string { return wrap(strings.Replace(method, "%BODY%", b, 1)) }
	tests := []struct {
		name, src, needle, reason string
	}{
		{"loop statement", body("while (true) { }"), "while (true)", "unsupported statement while"},
		{"unknown attribute", wrap("\n        [Category(\"slow\")]\n        [TestCase(\"x\")]\n        public void m(string title) { QualityParser.ParseQuality(title).Quality.Should().Be(Quality.SDTV); }\n"), "[Category", "unsupported attribute Category"},
		{"never asserted", body("QualityParser.ParseQuality(title);"), "QualityParser.ParseQuality(title);", "is never asserted"},
		{"unsupported assertion", body("QualityParser.ParseQuality(title).Quality.Should().NotBe(Quality.SDTV);"), "NotBe", "unsupported method"},
		{"interpolated string", body("var t = $\"{title}\";"), "$\"", "interpolated string is not supported"},
		{"property member", wrap("\n        public int Count { get; set; }\n"), "{ get;", "unsupported member"},
		{"missing source", wrap("\n        [Test]\n        [TestCaseSource(\"Nope\")]\n        public void m(string title) { }\n"), "[TestCaseSource", "is not a static object[] field"},
		{"divergent loop expectations", body("foreach (var c in new char[] { 'a', 'b' }) { QualityParser.ParseQuality(title).Revision.Version.Should().Be(c == 'a' ? 1 : 2); }"), "foreach", "invocations assert different expectations"},
		{"unknown enum member", body("QualityParser.ParseQuality(title).Quality.Should().Be(Quality.Nonexistent);"), "Quality.Nonexistent", "unknown member Quality.Nonexistent"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := appSpecs()[0]
			root := writeTree(t, spec, tc.src)
			_, _, err := extractApp(spec, root, testCommit)
			var se *srcError
			if !errors.As(err, &se) {
				t.Fatalf("want *srcError, got %v", err)
			}
			if se.file != fixtureRel || !strings.Contains(se.reason, tc.reason) {
				t.Errorf("error = %v, want file %s reason containing %q", se, fixtureRel, tc.reason)
			}
			if wantLine := lineOf(t, tc.src, tc.needle); tc.name != "divergent loop expectations" && tc.name != "never asserted" && se.line != wantLine {
				t.Errorf("location line %d, want %d (%v)", se.line, wantLine, se)
			}
			if se.line < 1 || se.col < 1 {
				t.Errorf("location must be 1-based: %v", se)
			}
		})
	}
}

func runExtract(t *testing.T, out string, sroot, rroot string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run([]string{"-series", sroot, "-movie", rroot, "-series-commit", testCommit, "-movie-commit", testCommit, "-out", out}, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestExtractPublishesBothCorporaDeterministically(t *testing.T) {
	specs := appSpecs()
	sroot := writeTree(t, specs[0], fixtureQuality)
	rroot := writeTree(t, specs[1], fixtureQuality)
	out := filepath.Join(t.TempDir(), "parser")

	code, stdout, stderr := runExtract(t, out, sroot, rroot)
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	var first [2][]byte
	for i, name := range []string{"series.jsonl", "movie.jsonl"} {
		data, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		first[i] = data
		if _, err := corpus.Read(bytes.NewReader(data)); err != nil {
			t.Errorf("%s does not decode through corpus.Read: %v", name, err)
		}
		sum := sha256.Sum256(data)
		if !strings.Contains(stdout, "file="+name) || !strings.Contains(stdout, "sha256="+hex.EncodeToString(sum[:])) {
			t.Errorf("summary lacks %s hash:\n%s", name, stdout)
		}
	}
	if !strings.Contains(stdout, "extract: ok version=1 files=2") || !strings.Contains(stdout, "cases=15 enabled=14 disabled=1 out_of_scope=0") {
		t.Errorf("summary: %s", stdout)
	}
	if !strings.Contains(stdout, "extract: source app=series path="+fixtureRel+" bytes=") {
		t.Errorf("source hashes are not reported:\n%s", stdout)
	}
	// Re-extraction truncates in place and reproduces the same bytes.
	if code, _, stderr := runExtract(t, out, sroot, rroot); code != 0 {
		t.Fatalf("second run exit %d: %s", code, stderr)
	}
	for i, name := range []string{"series.jsonl", "movie.jsonl"} {
		data, _ := os.ReadFile(filepath.Join(out, name))
		if !bytes.Equal(data, first[i]) {
			t.Errorf("%s changed on repeated extraction", name)
		}
	}
}

func TestExtractWriteFailureIsFatal(t *testing.T) {
	specs := appSpecs()
	sroot := writeTree(t, specs[0], fixtureQuality)
	rroot := writeTree(t, specs[1], fixtureQuality)
	out := t.TempDir()
	if err := os.Symlink("/dev/full", filepath.Join(out, "series.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("/dev/full"); err != nil {
		t.Fatalf("/dev/full is required for the write-failure control: %v", err)
	}
	code, stdout, stderr := runExtract(t, out, sroot, rroot)
	if code != 1 || !strings.Contains(stderr, "extract: write_failed file=series.jsonl") {
		t.Errorf("exit %d stderr %q, want exit 1 with write_failed", code, stderr)
	}
	if strings.Contains(stdout, "extract: ok") {
		t.Errorf("a failed write must not print a success summary: %s", stdout)
	}
}

func TestExtractUsageAndSourceFailures(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-series", "x"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "extract: usage") {
		t.Errorf("usage: exit %d %q", code, stderr.String())
	}
	specs := appSpecs()
	sroot := writeTree(t, specs[0], fixtureQuality)
	rroot := writeTree(t, specs[1], fixtureQuality)
	// A bad commit is rejected by the corpus schema before anything is written.
	out := t.TempDir()
	stderr.Reset()
	args := []string{"-series", sroot, "-movie", rroot, "-series-commit", "not-a-commit", "-movie-commit", testCommit, "-out", out}
	if code := run(args, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "extract: corpus_invalid app=series") {
		t.Errorf("bad commit: exit %d %q", code, stderr.String())
	}
	if entries, _ := os.ReadDir(out); len(entries) != 0 {
		t.Errorf("a rejected corpus must write nothing: %v", entries)
	}
	// A missing source file is a source failure, never a skipped file.
	if err := os.Remove(filepath.Join(rroot, filepath.FromSlash(specs[1].tests[2]))); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if code, _, se := runExtract(t, out, sroot, rroot); code != 1 || !strings.Contains(se, "extract: source_unavailable app=movie") {
		t.Errorf("missing source: exit %d %q", code, se)
	}
}

// oneMethod is a one-method fixture class: attr is the attribute line(s) of the
// method and body the statements of its body.
func oneMethod(attr, body string) string {
	return "namespace N\n{\n    [TestFixture]\n    public class QualityParserFixture\n    {\n        " + attr +
		"\n        public void m(string title)\n        {\n" + body + "\n        }\n    }\n}\n"
}

// classSource is a one-case fixture whose class carries classAttrs.
func classSource(classAttrs string) string {
	return "namespace N\n{\n    " + classAttrs + "\n    public class QualityParserFixture\n    {\n        [TestCase(\"Show.HDTV\")]\n" +
		"        public void m(string title)\n        {\n            QualityParser.ParseQuality(title).Quality.Should().Be(Quality.SDTV);\n        }\n    }\n}\n"
}

const assertSDTV = "            QualityParser.ParseQuality(title).Quality.Should().Be(Quality.SDTV);"

// extractRejects extracts a one-file fixture and returns its located rejection.
func extractRejects(t *testing.T, src string) *srcError {
	t.Helper()
	spec := appSpecs()[0]
	_, _, err := extractApp(spec, writeTree(t, spec, src), testCommit)
	var se *srcError
	if !errors.As(err, &se) {
		t.Fatalf("want *srcError, got %v", err)
	}
	if se.file != fixtureRel || se.line < 1 || se.col < 1 {
		t.Fatalf("rejection must carry a 1-based location in %s: %v", fixtureRel, se)
	}
	return se
}

func TestExtractReadsSourceThroughRootWithinActualByteBound(t *testing.T) {
	spec := appSpecs()[0]
	// The accepted bound is pinned here as a literal, never taken from the production constant.
	const limit = 2 * 1024 * 1024
	var se *srcError
	located := func(err error) bool {
		return errors.As(err, &se) && se.file == fixtureRel && se.line == 1 && se.col == 1 && strings.Contains(se.reason, "exceeds")
	}
	// The bytes actually delivered decide, whatever any metadata said.
	if data, err := readBounded(strings.NewReader(strings.Repeat("a", limit)), fixtureRel); err != nil || len(data) != limit {
		t.Errorf("a source of exactly %d bytes must be admitted: %d, %v", limit, len(data), err)
	}
	data, err := readBounded(strings.NewReader(strings.Repeat("a", limit+1)), fixtureRel)
	if !located(err) || data != nil {
		t.Errorf("one byte over must be rejected with its location and no payload: %v (%d bytes kept)", err, len(data))
	}

	// A far larger generated stream: exactly limit+1 bytes of it are ever consumed, the rejection
	// is located and no payload is kept. A stream of exactly the limit is admitted whole.
	endless := &countingReader{}
	big, err := readBounded(endless, fixtureRel)
	if !located(err) || big != nil {
		t.Errorf("an endless stream must be rejected with its location and no payload: %v (%d bytes kept)", err, len(big))
	}
	if endless.n != 2097153 {
		t.Errorf("an endless stream was consumed for %d bytes, want exactly 2097153", endless.n)
	}
	exact := &countingReader{limit: limit}
	if data, err := readBounded(exact, fixtureRel); err != nil || len(data) != limit || exact.n != limit {
		t.Errorf("a stream of exactly %d bytes must be admitted whole: %d bytes, consumed %d, %v", limit, len(data), exact.n, err)
	}

	// The same bound through the real reader on a delivered tree.
	pad := func(n int) string { return fixtureQuality + "//" + strings.Repeat("x", n-len(fixtureQuality)-2) }
	if _, _, err := extractApp(spec, writeTree(t, spec, pad(limit)), testCommit); err != nil {
		t.Errorf("a source of exactly %d bytes must extract: %v", limit, err)
	}
	_, _, err = extractApp(spec, writeTree(t, spec, pad(limit+1)), testCommit)
	var over *srcError
	if !errors.As(err, &over) || over.file != fixtureRel || !strings.Contains(over.reason, "exceeds") {
		t.Errorf("an oversize source must be rejected with its location: %v", err)
	}

	// A directory link that leaves the delivered tree is refused, never followed.
	root := writeTree(t, spec, fixtureQuality)
	elsewhere := filepath.Join(t.TempDir(), "tests")
	if err := os.Rename(filepath.Join(root, "tests"), elsewhere); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(root, "tests")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := extractApp(spec, root, testCommit); err == nil {
		t.Error("a directory link leaving the delivered tree must be refused")
	}

	// So is a source file that is itself a link.
	root = writeTree(t, spec, fixtureQuality)
	file := filepath.Join(root, filepath.FromSlash(spec.tests[1]))
	target := filepath.Join(t.TempDir(), "linked.cs")
	if err := os.Rename(file, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, file); err != nil {
		t.Fatal(err)
	}
	if _, _, err := extractApp(spec, root, testCommit); err == nil {
		t.Error("a linked source file must be refused")
	}
}

func TestExtractDecodesSurrogatePairsAndRejectsLoneSurrogates(t *testing.T) {
	spec := appSpecs()[0]
	literal := func(lit string) string { return oneMethod("[TestCase("+lit+")]", assertSDTV) }

	// The release name itself carries the pair (an escaped surrogate pair, a \U escape and a BMP
	// escape) before a real quality tail, so the recorded input, the consumer's inputs and the case
	// identity all depend on the decoded text, not the argument alone. An ordinary name is the control.
	for _, tc := range []struct{ lit, want string }{
		{`"Pair \uD83D\uDE00 and \U0001F600 and \u00e9.720p.HDTV"`, "Pair \U0001F600 and \U0001F600 and \u00e9.720p.HDTV"},
		{`"Plain.Show.720p.HDTV"`, "Plain.Show.720p.HDTV"},
	} {
		c, _, err := extractApp(spec, writeTree(t, spec, literal(tc.lit)), testCommit)
		if err != nil {
			t.Fatalf("%s must decode: %v", tc.lit, err)
		}
		data, err := corpus.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		back, err := corpus.Read(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("the decoded name must survive the canonical corpus: %v", err)
		}
		// The identity is hashed by hand from the recorded name: app, file, method, the first 8 bytes of
		// the SHA-256 of the arguments and input, and the duplicate ordinal.
		record := `{"Args":[{"t":"string","v":"` + tc.want + `"}],"Input":{"t":"array","v":[{"t":"string","v":"` + tc.want + `"}]}}`
		sum := sha256.Sum256([]byte(record))
		wantID := "series:QualityParserFixture.cs:m:" + hex.EncodeToString(sum[:8]) + ":0"
		for where, cs := range map[string]corpus.Case{"extracted": caseBy(t, c, "m", "TestCase#1"), "read back": caseBy(t, back, "m", "TestCase#1")} {
			if !corpus.Equal(cs.Args[0], str(tc.want)) {
				t.Errorf("%s: argument = %+v, want %q", where, cs.Args[0], tc.want)
			}
			wantValue(t, cs.Expected, corpus.InputKey, corpus.Value{Kind: corpus.KindArray, Items: []corpus.Value{str(tc.want)}})
			if in, err := cs.Inputs(corpus.TimeContext{}); err != nil || len(in) != 1 || in[0] != tc.want {
				t.Errorf("%s: consumer inputs = %q (%v), want %q", where, in, err, tc.want)
			}
			if cs.ID != wantID {
				t.Errorf("%s: identity = %s, want %s", where, cs.ID, wantID)
			}
		}
	}

	for _, lit := range []string{
		`"\uD83D"`, `"\uDE00"`, `"\uDE00\uD83D"`, `"a\uD83Dz"`, `"\uD83D\u0041"`, `"\U0000D83D"`, `"\uD83D\U0000DE00"`, `'\uD83D'`,
	} {
		src := literal(lit)
		se := extractRejects(t, src)
		if se.reason != "lone UTF-16 surrogate escape" || se.line != lineOf(t, src, "[TestCase") {
			t.Errorf("%s: rejected as %v, want the lone surrogate at line %d", lit, se, lineOf(t, src, "[TestCase"))
		}
	}
}

func TestExtractRejectsUnknownClassAttributesAndKeepsIgnoreExplicit(t *testing.T) {
	for _, attrs := range []string{"[TestFixture]\n    [Category(\"slow\")]", "[TestFixture, Category(\"slow\")]"} {
		src := classSource(attrs)
		if se := extractRejects(t, src); se.reason != "unsupported attribute Category" || se.line != lineOf(t, src, "Category") {
			t.Errorf("%q: rejected as %v, want the class attribute at line %d", attrs, se, lineOf(t, src, "Category"))
		}
	}
	spec := appSpecs()[0]
	for attrs, enabled := range map[string]bool{
		"[TestFixture]": true, "[TestFixture]\n    [Ignore(\"why\")]": false, "[TestFixture]\n    [Explicit]": false,
	} {
		c, _, err := extractApp(spec, writeTree(t, spec, classSource(attrs)), testCommit)
		if err != nil {
			t.Fatalf("%q: %v", attrs, err)
		}
		if cs := caseBy(t, c, "m", "TestCase#1"); cs.UpstreamEnabled != enabled {
			t.Errorf("%q: upstream enabled = %v, want %v", attrs, cs.UpstreamEnabled, enabled)
		}
	}
}

func TestExtractBoundsInterpreterWorkWithLocation(t *testing.T) {
	array := func(n int) string { return "new char[] { 'a'" + strings.Repeat(", 'a'", n-1) + " }" }
	loop := func(arr, inner string) string {
		return "foreach (var a in " + arr + ") { foreach (var b in " + arr + ") { " + inner + " } }"
	}
	tests := []struct {
		name, src, reason, needle string
	}{
		{"steps", oneMethod(`[TestCase("x")]`, "            "+loop(array(100), loop(array(100), "var t = title;"))), "interpreter step limit exceeded", "foreach"},
		{"observations", oneMethod(`[TestCase("x")]`, "            "+loop(array(300), "QualityParser.ParseQuality(title).Quality.Should().Be(Quality.SDTV);")), "observation limit exceeded", "QualityParser.ParseQuality"},
	}
	// Each declaration doubles the string, so the 25th would exceed the 32 MiB corpus cap before it is built.
	var doubling strings.Builder
	doubling.WriteString("            var a0 = \"x\";\n")
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&doubling, "            var a%d = a%d + a%d;\n", i, i-1, i-1)
	}
	tests = append(tests, struct{ name, src, reason, needle string }{
		"expanded bytes", oneMethod(`[TestCase("x")]`, doubling.String()), "expanded size limit exceeded", "var a25 =",
	})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			se := extractRejects(t, tc.src)
			if se.reason != tc.reason {
				t.Errorf("rejected as %v, want reason %q", se, tc.reason)
			}
			if tc.name == "expanded bytes" && se.line != lineOf(t, tc.src, tc.needle) {
				t.Errorf("limit reported at line %d, want %d (%v)", se.line, lineOf(t, tc.src, tc.needle), se)
			}
		})
	}
}

// countingReader streams 'a' bytes (endless when limit is 0) and counts the bytes it delivered.
type countingReader struct{ limit, n int }

func (r *countingReader) Read(p []byte) (int, error) {
	if r.limit > 0 {
		if r.n >= r.limit {
			return 0, io.EOF
		}
		p = p[:min(len(p), r.limit-r.n)]
	}
	for i := range p {
		p[i] = 'a'
	}
	r.n += len(p)
	return len(p), nil
}

// arrayDecls declares a0 as an empty array and each next ai as an array of references to its
// predecessor: two of them (shared, so 2^i leaves once expanded) up to doubling, one after that.
func arrayDecls(n, doubling int) string {
	var b strings.Builder
	b.WriteString("            var a0 = new object[] { };\n")
	for i := 1; i <= n; i++ {
		if i <= doubling {
			fmt.Fprintf(&b, "            var a%d = new object[] { a%d, a%d };\n", i, i-1, i-1)
		} else {
			fmt.Fprintf(&b, "            var a%d = new object[] { a%d };\n", i, i-1)
		}
	}
	return b.String()
}

func TestExtractRejectsSharedArrayExpansionAtEveryCallerWithoutExpanding(t *testing.T) {
	// a30 is 2^30 leaves once expanded; conversion is charged and depth-checked per node before it
	// builds, so the rejection comes after about the supported depth's worth of nodes.
	hostile := arrayDecls(30, 30)
	tooDeep := "new object[] { " + strings.Repeat("new object[] { ", 15) + `"x"` + strings.Repeat(" }", 15) + " }"
	for _, tc := range []struct{ name, src, needle string }{
		{"assertion", oneMethod(`[TestCase("x")]`, hostile+"            QualityParser.ParseQuality(title).Quality.Should().Be(a30);"), "Should()"},
		{"entry input", oneMethod(`[TestCase("x")]`, hostile+"            QualityParser.ParseQuality(a30).Quality.Should().Be(Quality.SDTV);"), "ParseQuality(a30)"},
		{"argument", strings.Replace(oneMethod("[TestCase("+tooDeep+")]", `            QualityParser.ParseQuality("x").Quality.Should().Be(Quality.SDTV);`), "string title", "object title", 1), "[TestCase"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			se := extractRejects(t, tc.src)
			runtime.ReadMemStats(&after)
			if se.reason != "value nesting limit exceeded" || se.line != lineOf(t, tc.src, tc.needle) {
				t.Errorf("rejected as %v, want the nesting limit at line %d", se, lineOf(t, tc.src, tc.needle))
			}
			if grew := after.TotalAlloc - before.TotalAlloc; grew > 16<<20 {
				t.Errorf("the rejection allocated %d bytes: the shared expansion was built before it was refused", grew)
			}
		})
	}
}

func TestExtractAdmitsSharedArraysUpToTheSupportedNesting(t *testing.T) {
	spec := appSpecs()[0]
	// a14 is an empty array wrapped by 3 doublings and 11 singletons: array level 14, the deepest the
	// corpus record admits (nesting 32). The same body one level deeper is refused.
	src := func(n int) string {
		return oneMethod(`[TestCase("x")]`, arrayDecls(n, 3)+fmt.Sprintf("            QualityParser.ParseQuality(title).Quality.Should().Be(a%d);", n))
	}
	c, _, err := extractApp(spec, writeTree(t, spec, src(14)), testCommit)
	if err != nil {
		t.Fatalf("level 14 must be admitted: %v", err)
	}
	want := corpus.Value{Kind: corpus.KindArray, Items: []corpus.Value{}}
	for i := 1; i <= 14; i++ {
		if i <= 3 {
			want = corpus.Value{Kind: corpus.KindArray, Items: []corpus.Value{want, want}}
		} else {
			want = corpus.Value{Kind: corpus.KindArray, Items: []corpus.Value{want}}
		}
	}
	cs := caseBy(t, c, "m", "TestCase#1")
	wantValue(t, cs.Expected, "Quality", want)
	data, err := corpus.Marshal(c)
	if err != nil {
		t.Fatalf("the corpus must admit what the extractor admitted: %v", err)
	}
	if back, err := corpus.Read(bytes.NewReader(data)); err != nil || !corpus.Equal(back.Cases[0].Expected["Quality"], want) {
		t.Errorf("the shared expansion must survive the canonical corpus: %v", err)
	}
	if se := extractRejects(t, src(15)); se.reason != "value nesting limit exceeded" {
		t.Errorf("level 15 rejected as %v, want the nesting limit", se)
	}
}

func TestExtractChargesEveryConvertedNodeBeforeBuildingIt(t *testing.T) {
	// Fixed by hand from the canonical encoding: {"t":"null"} is 12 bytes and an array of three
	// items is its 20-byte wrapper `{"t":"array","v":[]}` plus two separators, 22 bytes.
	const treeBytes = 4*22 + 9*12
	leaf := val{k: vNull}
	arr := func(items ...val) val { return val{k: vArr, arr: items} }
	level := arr(leaf, leaf, leaf)
	tree := arr(level, level, level) // 13 nodes, nine of them reached through one shared array
	const nodes = 13
	at := pos{7, 9}
	convert := func(w *work, v val) (corpus.Value, error) {
		return (&interp{file: fixtureRel, w: w}).toCorpus(v, at)
	}
	rejected := func(t *testing.T, err error, reason string) {
		t.Helper()
		var se *srcError
		if !errors.As(err, &se) || se.file != fixtureRel || se.line != at.line || se.col != at.col || se.reason != reason {
			t.Errorf("error = %v, want %q at %s:%d:%d", err, reason, fixtureRel, at.line, at.col)
		}
	}
	null := corpus.Value{Kind: corpus.KindNull}
	row := corpus.Value{Kind: corpus.KindArray, Items: []corpus.Value{null, null, null}}
	wantTree := corpus.Value{Kind: corpus.KindArray, Items: []corpus.Value{row, row, row}}

	// A budget of exactly the tree's bytes (and, apart, steps) admits it and is then spent; one less
	// refuses with the location and never passes the limit.
	w := &work{bytes: maxExpandedBytes - treeBytes}
	if got, err := convert(w, tree); err != nil || !corpus.Equal(got, wantTree) || w.bytes != maxExpandedBytes {
		t.Errorf("the exact byte budget must admit the tree: %v (charged to %d of %d)", err, w.bytes, maxExpandedBytes)
	}
	w = &work{bytes: maxExpandedBytes - treeBytes + 1}
	_, err := convert(w, tree)
	rejected(t, err, "expanded size limit exceeded")
	if w.bytes > maxExpandedBytes {
		t.Errorf("charged %d past the %d limit", w.bytes, maxExpandedBytes)
	}
	w = &work{steps: maxInterpSteps - nodes}
	if got, err := convert(w, tree); err != nil || !corpus.Equal(got, wantTree) || w.steps != maxInterpSteps {
		t.Errorf("the exact step budget must admit the tree: %v (stepped to %d of %d)", err, w.steps, maxInterpSteps)
	}
	w = &work{steps: maxInterpSteps - nodes + 1}
	_, err = convert(w, tree)
	rejected(t, err, "interpreter step limit exceeded")

	// 4^13 leaves once expanded and within the supported nesting: only the budget bounds it, and the
	// conversion stops after exactly the nodes the budget allows, building about as many.
	hostile := arr(leaf, leaf, leaf, leaf)
	for range 12 {
		hostile = arr(hostile, hostile, hostile, hostile)
	}
	w = &work{bytes: maxExpandedBytes - 2000}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err = convert(w, hostile)
	runtime.ReadMemStats(&after)
	rejected(t, err, "expanded size limit exceeded")
	// The next node (at most a 23-byte four-item array) did not fit, so it was refused unspent.
	if w.bytes > maxExpandedBytes || w.bytes <= maxExpandedBytes-23 {
		t.Errorf("the budget of 2000 bytes was charged to %d, want within 23 bytes below %d", w.bytes, maxExpandedBytes)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 4<<20 {
		t.Errorf("a budget of 100 nodes allocated %d bytes", grew)
	}

	// The supported nesting: array level 14 is admitted, 15 is not, whether the innermost value is a
	// null or an empty array (corpus record depth 31 and 32 against 33 and 34).
	for _, base := range []val{leaf, arr()} {
		for levels, ok := range map[int]bool{14: true, 15: false} {
			v := base
			for range levels {
				v = arr(v)
			}
			_, err := convert(&work{}, v)
			if ok && err != nil {
				t.Errorf("%d array levels around %v must be admitted: %v", levels, base.k, err)
			}
			if !ok {
				rejected(t, err, "value nesting limit exceeded")
			}
		}
	}
}

func TestExtractRejectsUnrepresentableDatesWithLocation(t *testing.T) {
	spec := appSpecs()[0]
	input := func(date string) string {
		return oneMethod(`[TestCase("x")]`, "            QualityParser.ParseQuality("+date+").Quality.Should().Be(Quality.SDTV);")
	}
	for _, tc := range []struct{ name, src, needle, reason string }{
		{"unsupported format as input", input(`DateTime.Today.ToString("HH")`), "ParseQuality(", "unsupported date format specifier"},
		{"unsupported format as assertion", oneMethod(`[TestCase("x")]`, "            QualityParser.ParseQuality(title).Quality.Should().Be(DateTime.Today.ToString(\"HH\"));"), "Should()", "unsupported date format specifier"},
		{"offset over the limit", input(`DateTime.Today.AddDays(36501).ToString("yyyy.MM.dd")`), "ParseQuality(", "invalid date"},
		{"offset under the limit", input(`DateTime.Today.AddDays(-36501).ToString("yyyy.MM.dd")`), "ParseQuality(", "invalid date"},
		{"offset summed over the limit", input(`DateTime.Today.AddDays(36000).AddDays(501).ToString("yyyy")`), "ParseQuality(", "invalid date"},
		{"offset that wraps around", input(`DateTime.Today.AddDays(9223372036854775807).AddDays(9223372036854775807).ToString("yyyy")`), "ParseQuality(", "date offset overflows"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			se := extractRejects(t, tc.src)
			if !strings.Contains(se.reason, tc.reason) || se.line != lineOf(t, tc.src, tc.needle) {
				t.Errorf("rejected as %v, want %q at line %d", se, tc.reason, lineOf(t, tc.src, tc.needle))
			}
		})
	}

	// Controls: the offsets at both limits extract, stay symbolic and survive the canonical corpus.
	for _, off := range []int{36500, -36500} {
		c, _, err := extractApp(spec, writeTree(t, spec, input(fmt.Sprintf(`DateTime.Today.AddDays(%d).ToString("yyyy.MM.dd")`, off))), testCommit)
		if err != nil {
			t.Fatalf("offset %d must be admitted: %v", off, err)
		}
		in := caseBy(t, c, "m", "TestCase#1").Expected[corpus.InputKey].Items[0]
		if in.Kind != corpus.KindDate || in.Offset != off || in.Template != "{0:yyyy.MM.dd}" {
			t.Errorf("offset %d recorded as %+v", off, in)
		}
		data, err := corpus.Marshal(c)
		if err != nil {
			t.Fatalf("offset %d must be written: %v", off, err)
		}
		if _, err := corpus.Read(bytes.NewReader(data)); err != nil {
			t.Errorf("offset %d must be read back: %v", off, err)
		}
	}
}

func TestExtractRunRejectingADatePublishesNothingAndKeepsPriorOutput(t *testing.T) {
	specs := appSpecs()
	bad := oneMethod(`[TestCase("x")]`, `            QualityParser.ParseQuality(DateTime.Today.ToString("HH")).Quality.Should().Be(Quality.SDTV);`)
	for badSide := range specs {
		t.Run(specs[badSide].name, func(t *testing.T) {
			var roots [2]string
			for i, spec := range specs {
				src := fixtureQuality
				if i == badSide {
					src = bad
				}
				roots[i] = writeTree(t, spec, src)
			}
			out := t.TempDir()
			prior := map[string]string{"series.jsonl": "prior series\n", "movie.jsonl": "prior movie\n"}
			for name, text := range prior {
				if err := os.WriteFile(filepath.Join(out, name), []byte(text), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			code, stdout, stderr := runExtract(t, out, roots[0], roots[1])
			want := fmt.Sprintf("extract: unsupported app=%s file=%s line=%d col=", specs[badSide].name, fixtureRel, lineOf(t, bad, "ParseQuality("))
			if code != 1 || !strings.Contains(stderr, want) || !strings.Contains(stderr, "reason=corpus: unsupported date format specifier") || stdout != "" {
				t.Errorf("exit %d stdout %q stderr %q, want exit 1 with %q and no summary", code, stdout, stderr, want)
			}
			for name, text := range prior {
				if got, err := os.ReadFile(filepath.Join(out, name)); err != nil || string(got) != text {
					t.Errorf("%s = %q (%v), want the prior output kept", name, got, err)
				}
			}
			if entries, err := os.ReadDir(out); err != nil || len(entries) != len(prior) {
				t.Errorf("the output directory holds %d entries (%v), want only the prior files", len(entries), err)
			}
		})
	}
}

func TestExtractChargesStringsAtTheirEncodedLength(t *testing.T) {
	// Hand-counted encoded lengths between the quotes: ordinary text is itself, not a multiple.
	for _, tc := range []struct {
		s    string
		want int
	}{
		{"", 0}, {"Show.S01E01.720p.HDTV", 21}, {"é", 2}, {"\U0001F600", 4},
		{"\"", 2}, {`\`, 2}, {"\n\r\t\b\f", 10}, {"\x01", 6}, {"\x1f", 6}, {"\u2028\u2029", 12}, {"\xff", 3},
		{"arr", 3}, {"xARRx", 5},
	} {
		if got := encodedLen(tc.s); got != tc.want {
			t.Errorf("encodedLen(%q) = %d, want %d", tc.s, got, tc.want)
		}
	}
	for _, tc := range []struct {
		s    string
		want int
	}{{"arr", 5}, {"xARRx", 5}, {"aRr.Arr", 10}, {"ar", 0}, {"bar", 0}, {"aarr", 5}, {"arrr", 5}} {
		if got := nameLetterBytes(tc.s); got != tc.want {
			t.Errorf("nameLetterBytes(%q) = %d, want %d", tc.s, got, tc.want)
		}
	}
}

// marshalLen is the length of the canonical corpus bytes of a one-case corpus whose only argument is v.
func marshalLen(t *testing.T, v corpus.Value) int {
	t.Helper()
	cs := corpus.Case{
		ID: "id", File: "tests/a.cs", Method: "m", Entry: "Test", Line: 1, UpstreamEnabled: true,
		Scope: corpus.ScopeQuality, ScopeOwner: qualityOwner, Args: []corpus.Value{v}, Expected: map[string]corpus.Value{},
	}
	c := &corpus.Corpus{
		Header: corpus.Header{
			Schema: corpus.SchemaVersion, App: "series", SourceRepository: "env:BOXSET_REF_SERIES", SourceCommit: testCommit,
			SourcePaths: []string{"tests/a.cs"}, License: license, Counts: corpus.Tally([]corpus.Case{cs}),
		},
		Cases: []corpus.Case{cs},
	}
	data, err := corpus.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal %+v: %v", v, err)
	}
	return len(data)
}

func TestExtractNodeChargeEqualsTheWritersEncoding(t *testing.T) {
	null := corpus.Value{Kind: corpus.KindNull}
	base := marshalLen(t, null)
	nulls := func(n int) corpus.Value {
		a := corpus.Value{Kind: corpus.KindArray, Items: []corpus.Value{}}
		for range n {
			a.Items = append(a.Items, null)
		}
		return a
	}
	for _, v := range []corpus.Value{
		null, {Kind: corpus.KindBool, Bool: true}, {Kind: corpus.KindBool}, {Kind: corpus.KindInt, Int: 0},
		{Kind: corpus.KindInt, Int: -9007199254740993}, {Kind: corpus.KindDecimal, Str: "-12.50"},
		{Kind: corpus.KindString}, {Kind: corpus.KindString, Str: "Show.S01E01.720p.HDTV"},
		{Kind: corpus.KindString, Str: "arr aRR xARRx"}, {Kind: corpus.KindString, Str: "a\"r\\r"},
		{Kind: corpus.KindString, Str: "\x01\n"}, {Kind: corpus.KindString, Str: "\u2028"}, {Kind: corpus.KindString, Str: "é\U0001F600"},
		{Kind: corpus.KindString, Str: "\xff"},
		{Kind: corpus.KindEnum, Type: "Quality", Str: "SDTV"}, {Kind: corpus.KindEnum, Type: "arr", Str: "Arr"},
		{Kind: corpus.KindDate, Offset: 14, Template: "A.{0:yyyy.MM.dd}"}, {Kind: corpus.KindDate, Offset: -36500, Template: "arr{0:yyyy}"},
		nulls(0), nulls(1), nulls(3),
	} {
		items := len(v.Items)
		// The array's items are charged on their own; the writer's total is the node plus its items.
		got := nodeBytes(v, items) + items*nodeBytes(null, 0)
		if want := marshalLen(t, v) - base + nodeBytes(null, 0); got != want {
			t.Errorf("%s %q %q: charged %d, the writer encodes %d", v.Kind, v.Str, v.Template, got, want)
		}
	}
}

func TestExtractChargeIsExactAtTheBudgetEdge(t *testing.T) {
	at := pos{7, 9}
	arr := func(items ...val) val { return val{k: vArr, arr: items} }
	// admits reports whether v converts with exactly need bytes of budget left, and where it is refused otherwise.
	admits := func(t *testing.T, v val, need int) bool {
		t.Helper()
		w := &work{bytes: maxExpandedBytes - need}
		_, err := (&interp{file: fixtureRel, w: w}).toCorpus(v, at)
		if err == nil {
			return w.bytes == maxExpandedBytes
		}
		var se *srcError
		if !errors.As(err, &se) || se.file != fixtureRel || se.line != at.line || se.col != at.col || se.reason != "expanded size limit exceeded" {
			t.Errorf("error = %v, want the located expanded size limit at %s:%d:%d", err, fixtureRel, at.line, at.col)
		}
		if w.bytes > maxExpandedBytes {
			t.Errorf("charged %d past the %d limit", w.bytes, maxExpandedBytes)
		}
		return false
	}
	for _, tc := range []struct {
		name string
		v    val
		need int // the real canonical encoding, counted by hand
	}{
		// A date template with arr is not a string: no name-letter escape is charged.
		{"date template with arr", val{k: vSym, s: "arr{0:yyyy}", i: 5}, len(`{"t":"date","offset":5,"template":"arr{0:yyyy}"}`)},
		{"enum with arr", val{k: vEnum, t: "arr", s: "arr"}, len(`{"t":"enum","type":"arr","v":"arr"}`)},
		// A string is: arr is written as \u0061rr.
		{"string with arr", val{k: vStr, s: "arr"}, len(`{"t":"string","v":"\u0061rr"}`)},
		// A hundred one-byte strings: each leaf is 22 bytes with its framing, plus the wrapper and 99
		// separators. The raw bytes plus a flat 16 per node (1716) would have admitted it far earlier.
		{"short strings", arr(manyStr(100)...), len(`{"t":"array","v":[]}`) + 99 + 100*len(`{"t":"string","v":"a"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !admits(t, tc.v, tc.need) {
				t.Errorf("the exact real encoding (%d bytes) must be admitted", tc.need)
			}
			if admits(t, tc.v, tc.need-1) {
				t.Errorf("one byte under the real encoding (%d) must be refused", tc.need-1)
			}
		})
	}
}

func manyStr(n int) []val {
	out := make([]val, n)
	for i := range out {
		out[i] = val{k: vStr, s: "a"}
	}
	return out
}

func TestExtractRejectsSharedArrayThatOnlyEncodedSizeExpands(t *testing.T) {
	spec := appSpecs()[0]
	// s10 is 1024 bytes. Each of the 8192 leaves of a12 costs 1040 raw bytes (about 8.5 MB, inside the
	// 32 MiB budget) but 6160 once its control characters are encoded (about 50 MB, past it).
	decls := func(unit string) string {
		var b strings.Builder
		fmt.Fprintf(&b, "            var s0 = \"%s\";\n", unit)
		for i := 1; i <= 10; i++ {
			fmt.Fprintf(&b, "            var s%d = s%d + s%d;\n", i, i-1, i-1)
		}
		b.WriteString("            var a0 = new object[] { s10, s10 };\n")
		for i := 1; i <= 12; i++ {
			fmt.Fprintf(&b, "            var a%d = new object[] { a%d, a%d };\n", i, i-1, i-1)
		}
		return b.String()
	}
	assert := "            QualityParser.ParseQuality(title).Quality.Should().Be(a12);"
	hostile := oneMethod(`[TestCase("x")]`, decls(`\u0001`)+assert)

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	se := extractRejects(t, hostile)
	runtime.ReadMemStats(&after)
	if se.reason != "expanded size limit exceeded" || se.line != lineOf(t, hostile, "Should()") {
		t.Errorf("rejected as %v, want the expanded size limit at line %d", se, lineOf(t, hostile, "Should()"))
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 16<<20 {
		t.Errorf("the rejection allocated %d bytes: the encoded expansion was built before it was refused", grew)
	}

	// Control: the same shape with ordinary text of the same raw size is admitted whole.
	c, _, err := extractApp(spec, writeTree(t, spec, oneMethod(`[TestCase("x")]`, decls("x")+assert)), testCommit)
	if err != nil {
		t.Fatalf("ordinary text of the same raw size must be admitted: %v", err)
	}
	if got := caseBy(t, c, "m", "TestCase#1").Expected["Quality"]; got.Kind != corpus.KindArray || len(got.Items) != 2 {
		t.Errorf("control value = %+v", got.Kind)
	}
}

func movieSpec() appSpec { return appSpecs()[1] }

// titleCorpus extracts the synthetic movie tree and reads it back through the
// one canonical reader.
func titleCorpus(t *testing.T) *corpus.Corpus {
	t.Helper()
	spec := movieSpec()
	c, _, err := extractApp(spec, writeTree(t, spec, fixtureQuality), testCommit)
	if err != nil {
		t.Fatalf("extractApp: %v", err)
	}
	data, err := corpus.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := corpus.Read(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("corpus.Read: %v", err)
	}
	return got
}

func arr(items ...corpus.Value) corpus.Value {
	return corpus.Value{Kind: corpus.KindArray, Items: append([]corpus.Value{}, items...)}
}

var null = corpus.Value{Kind: corpus.KindNull}

// wantKeys asserts the exact set of recorded property keys of a case.
func wantKeys(t *testing.T, cs corpus.Case, keys ...string) {
	t.Helper()
	want := append([]string{}, keys...)
	slices.Sort(want)
	if got := corpus.SortedKeys(cs.Expected); !slices.Equal(got, want) {
		t.Errorf("%s %s: expected keys %v, want %v", cs.Method, cs.Entry, got, want)
	}
}

func TestExtractRecordsEveryTitleCaseWithItsAssertedProperties(t *testing.T) {
	got := titleCorpus(t)
	input := func(names ...string) corpus.Value {
		items := make([]corpus.Value, len(names))
		for i, n := range names {
			items[i] = str(n)
		}
		return arr(items...)
	}
	title := func(cs corpus.Case, owner string) {
		t.Helper()
		if cs.Scope != corpus.ScopeTitle || cs.ScopeOwner != owner || !cs.UpstreamEnabled {
			t.Errorf("%s %s: scope %s owner %s enabled %v, want an enabled title case of %s", cs.Method, cs.Entry, cs.Scope, cs.ScopeOwner, cs.UpstreamEnabled, owner)
		}
	}
	const movieTitle, groupParser, languageParser = "Parser.ParseMovieTitle", "ReleaseGroupParser.ParseReleaseGroup", "LanguageParser.ParseLanguages"

	// A property chain through the primary title.
	cs := caseBy(t, got, "should_parse_movie_title", "TestCase#1")
	title(cs, movieTitle)
	wantKeys(t, cs, corpus.InputKey, "PrimaryMovieTitle")
	wantValue(t, cs.Expected, corpus.InputKey, input("The.Movie.2015.1080p.BluRay.x264-GRP"))
	wantValue(t, cs.Expected, "PrimaryMovieTitle", str("The Movie"))
	if len(cs.Args) != 2 || !corpus.Equal(cs.Args[1], str("The Movie")) {
		t.Errorf("arguments: %+v", cs.Args)
	}

	// A local result asserted inside an assertion scope: every property is recorded.
	cs = caseBy(t, got, "should_parse_anime_movie_title", "TestCase#1")
	title(cs, movieTitle)
	wantKeys(t, cs, corpus.InputKey, "PrimaryMovieTitle", "ReleaseGroup", "Year")
	wantValue(t, cs.Expected, "ReleaseGroup", str("Group"))
	wantValue(t, cs.Expected, "Year", num(2016))

	// A parsed title and its expected title are both normalized by the source; the recorded
	// value is the title before normalization, under the normalized property path.
	cs = caseBy(t, got, "should_not_parse_url_in_name", "TestCase#2")
	title(cs, movieTitle)
	wantKeys(t, cs, corpus.InputKey, "MovieTitle.CleanMovieTitle")
	wantValue(t, cs.Expected, corpus.InputKey, input("test.net - Movie.Title.2023.720p.HDTV.X264-DIMENSION"))
	wantValue(t, cs.Expected, "MovieTitle.CleanMovieTitle", str("Movie Title"))
	if len(cs.Args) != 2 || !corpus.Equal(cs.Args[0], str("test.net - Movie.Title.2023.720p.HDTV.X264-DIMENSION")) {
		t.Errorf("arguments: %+v", cs.Args)
	}

	// The loop over the alternative titles is bounded by the asserted count and indexes the results.
	cs = caseBy(t, got, "should_parse_movie_alternative_titles", "TestCase#1")
	title(cs, movieTitle)
	wantKeys(t, cs, corpus.InputKey, "$isDir", "MovieTitles.Count", "MovieTitles[0]", "MovieTitles[1]", "MovieTitles[2]")
	wantValue(t, cs.Expected, "$isDir", corpus.Value{Kind: corpus.KindBool, Bool: true})
	wantValue(t, cs.Expected, "MovieTitles.Count", num(3))
	for i, want := range []string{"Movie One AKA Movie Two", "Movie One", "Movie Two"} {
		wantValue(t, cs.Expected, fmt.Sprintf("MovieTitles[%d]", i), str(want))
	}
	if len(cs.Args) != 2 || !corpus.Equal(cs.Args[1], arr(str("Movie One AKA Movie Two"), str("Movie One"), str("Movie Two"))) {
		t.Errorf("array argument: %+v", cs.Args)
	}

	// Membership assertions collect the members in order, once each.
	cs = caseBy(t, got, "should_parse_language_french_english", "TestCase#1")
	title(cs, movieTitle)
	wantKeys(t, cs, corpus.InputKey, "$isDir", "Languages.Contain")
	wantValue(t, cs.Expected, "Languages.Contain", arr(enum("Language", "French"), enum("Language", "English")))

	cs = caseBy(t, got, "should_not_parse_wrong_language_in_title", "TestCase#1")
	wantKeys(t, cs, corpus.InputKey, "$isDir", "Languages.Count", "Languages.First()")
	wantValue(t, cs.Expected, "Languages.Count", num(1))
	wantValue(t, cs.Expected, "Languages.First()", enum("Language", "Unknown"))

	// A null result is recorded on the result itself.
	cs = caseBy(t, got, "should_not_parse_crap", "TestCase#1")
	title(cs, movieTitle)
	wantKeys(t, cs, corpus.InputKey, "$result")
	wantValue(t, cs.Expected, "$result", null)

	// The helper's own test: the string it normalizes is the recorded input, its folded
	// form the asserted result; there are no arguments (a plain test with a local const).
	cs = caseBy(t, got, "should_remove_accents_from_title", "Test")
	title(cs, "Parser.CleanMovieTitle")
	wantKeys(t, cs, corpus.InputKey, "$result")
	wantValue(t, cs.Expected, corpus.InputKey, input("Carniv\u00E0le"))
	wantValue(t, cs.Expected, "$result", str("carnivale"))
	if len(cs.Args) != 0 {
		t.Errorf("a plain test has no arguments: %+v", cs.Args)
	}

	// Upstream-ignored title cases stay retained and disabled.
	cs = caseBy(t, got, "ignored_title", "TestCase#1")
	if cs.UpstreamEnabled || cs.Scope != corpus.ScopeTitle {
		t.Errorf("ignored title case: %+v", cs)
	}
	wantValue(t, cs.Expected, "PrimaryMovieTitle", str("Ignored Movie"))

	cs = caseBy(t, got, "should_parse_edition", "TestCase#1")
	title(cs, movieTitle)
	wantKeys(t, cs, corpus.InputKey, "Edition")
	wantValue(t, cs.Expected, "Edition", str("Directors Cut"))

	// The release group and language parsers are entry points of their own.
	cs = caseBy(t, got, "should_parse_release_group", "TestCase#1")
	title(cs, groupParser)
	wantKeys(t, cs, corpus.InputKey, "$result")
	wantValue(t, cs.Expected, "$result", str("VARYG"))
	cs = caseBy(t, got, "should_parse_release_group", "TestCase#2")
	wantValue(t, cs.Expected, "$result", null)
	cs = caseBy(t, got, "should_not_parse_anime_hash_as_release_group", "TestCase#1")
	title(cs, groupParser)
	wantValue(t, cs.Expected, "$result", null)

	cs = caseBy(t, got, "should_parse_language_french", "TestCase#1")
	title(cs, languageParser)
	wantKeys(t, cs, corpus.InputKey, "$result.Contain")
	wantValue(t, cs.Expected, "$result.Contain", arr(enum("Language", "French")))
	cs = caseBy(t, got, "should_parse_language_spanish_latino", "TestCase#1")
	title(cs, languageParser)
	wantKeys(t, cs, corpus.InputKey, "First().Id")
	wantValue(t, cs.Expected, "First().Id", enum("Language", "SpanishLatino"))

	// Tests of other entry points are retained out of scope with their source arguments and no
	// recorded input or assertion; their bodies are never interpreted.
	for _, tc := range []struct {
		method, entry, owner string
		args                 []corpus.Value
	}{
		{"should_normalize_imdbid", "TestCase#1", "Parser.NormalizeImdbId", []corpus.Value{str("1234567"), str("tt1234567")}},
		{"should_not_parse_md5", "Test", movieTitle, nil},
		{"should_parse_subtitle_language_english", "TestCase#1", "LanguageParser.ParseSubtitleLanguage", []corpus.Value{str("Movie Title - 2022.en.sub")}},
		{"should_parse_title_and_tags", "TestCase#1", "LanguageParser.ParseSubtitleLanguageInformation", []corpus.Value{
			str("Name (2020) - [AAC 2.0].testtitle.default.eng.forced.ass"), arr(str("default"), str("forced")), str("testtitle"), str("English")}},
		{"should_parse_title_and_tags", "TestCase#2", "LanguageParser.ParseSubtitleLanguageInformation", []corpus.Value{
			str("Name (2020) - [AAC 2.0].ru-something-else.srt"), arr(), str("something-else"), str("Russian")}},
		{"should_properly_parse_hashed_releases", "TestCaseSource:HashedReleaseParserCases#1", "Parser.ParseMoviePath", []corpus.Value{
			str(`C:\Test\Some.Hashed.Release.2018.720p-Mercury\0e895c37.mkv`), str("Some Hashed Release"), enum("Quality", "SDTV"), str("Mercury")}},
		{"should_properly_parse_hashed_releases", "TestCaseSource:HashedReleaseParserCases#2", "Parser.ParseMoviePath", []corpus.Value{
			str(`C:\Test\Movie.2018\abc.mkv`), str("Movie"), enum("Quality", "Bluray720p"), null}},
	} {
		cs := caseBy(t, got, tc.method, tc.entry)
		if cs.Scope != corpus.ScopeOutOfScope || cs.ScopeOwner != tc.owner || !cs.UpstreamEnabled {
			t.Errorf("%s %s: scope %s owner %s, want out of scope under %s", tc.method, tc.entry, cs.Scope, cs.ScopeOwner, tc.owner)
		}
		wantKeys(t, cs, corpus.InputKey)
		wantValue(t, cs.Expected, corpus.InputKey, arr())
		if len(cs.Args) != len(tc.args) {
			t.Errorf("%s %s: arguments %+v, want %+v", tc.method, tc.entry, cs.Args, tc.args)
			continue
		}
		for i, want := range tc.args {
			if !corpus.Equal(cs.Args[i], want) {
				t.Errorf("%s %s: argument %d = %+v, want %+v", tc.method, tc.entry, i, cs.Args[i], want)
			}
		}
	}

	// The inventory: every source case of every title fixture is counted, and a plain test (no
	// TestCase attribute, no source row) is counted as such.
	h := got.Header
	for file, want := range map[string]int{
		"tests/ParserFixture.cs": 13, "tests/EditionParserFixture.cs": 1, "tests/ReleaseGroupParserFixture.cs": 3,
		"tests/LanguageParserFixture.cs": 5, "tests/HashedReleaseFixture.cs": 2, "tests/CrapParserFixture.cs": 0, "tests/UrlFixture.cs": 0,
	} {
		raw := h.Counts["source:testcase_attrs:"+file] + h.Counts["source:source_rows:"+file] + h.Counts["source:plain_tests:"+file]
		if h.Counts["cases:"+file] != want || raw != want {
			t.Errorf("%s: %d cases and %d raw rows, want %d", file, h.Counts["cases:"+file], raw, want)
		}
	}
	if h.Counts["source:plain_tests:tests/ParserFixture.cs"] != 2 || h.Counts["source:source_rows:tests/HashedReleaseFixture.cs"] != 2 {
		t.Errorf("plain test and row counts: %v", h.Counts)
	}
	if _, ok := h.Counts["source:plain_tests:tests/EditionParserFixture.cs"]; ok {
		t.Error("a file without plain tests must not record a plain-test count")
	}
	// Attribution covers the title fixtures, the language source and the ported provenance sources.
	for _, p := range append(append([]string{"parser/LanguageParser.cs"}, movieSpec().titles...), movieSpec().provenance...) {
		if !slices.Contains(h.SourcePaths, p) {
			t.Errorf("source path %s is not attributed", p)
		}
	}
	ids := map[string]bool{}
	for _, cs := range got.Cases {
		if ids[cs.ID] {
			t.Errorf("duplicate case identity %s", cs.ID)
		}
		ids[cs.ID] = true
	}
	again := titleCorpus(t)
	a, _ := corpus.Marshal(got)
	b, _ := corpus.Marshal(again)
	if !bytes.Equal(a, b) {
		t.Error("repeated title extraction must be byte-identical")
	}
}

func TestExtractSeriesSideRecordsNoTitleCases(t *testing.T) {
	spec := appSpecs()[0]
	if len(spec.titles) != 0 || spec.languages != "" || len(spec.provenance) != 0 {
		t.Fatalf("the series side must not extract titles yet: %+v", spec)
	}
	for _, cs := range extractSynthetic(t, spec).Cases {
		if cs.Scope != corpus.ScopeQuality {
			t.Errorf("series case %s has scope %s", cs.ID, cs.Scope)
		}
	}
}

// titleMethod is a one-method title fixture class: attr is the attribute line of
// the method and body the statements of its body.
func titleMethod(attr, body string) string {
	return "using NUnit.Framework;\n\nnamespace F\n{\n    [TestFixture]\n    public class ParserFixture\n    {\n        " + attr +
		"\n        public void m(string title)\n        {\n" + body + "\n        }\n    }\n}\n"
}

// extractTitleRejects extracts the movie tree with ParserFixture replaced by src
// and returns the located rejection.
func extractTitleRejects(t *testing.T, src string) *srcError {
	t.Helper()
	spec := movieSpec()
	root := writeTree(t, spec, fixtureQuality)
	writeSource(t, root, "tests/ParserFixture.cs", src)
	_, _, err := extractApp(spec, root, testCommit)
	var se *srcError
	if !errors.As(err, &se) {
		t.Fatalf("want *srcError, got %v", err)
	}
	if se.file != "tests/ParserFixture.cs" || se.line < 1 || se.col < 1 {
		t.Errorf("location %s:%d:%d", se.file, se.line, se.col)
	}
	return se
}

func TestExtractRejectsUnsupportedTitleFormsWithLocation(t *testing.T) {
	const attr = `[TestCase("x")]`
	const call = "            var info = Parser.Parser.ParseMovieTitle(title);\n"
	for _, tc := range []struct {
		name, body, reason, at string
	}{
		{"no entry point", "            var x = title;", "no recognized entry point", "void m("},
		{"unknown language member", "            Parser.Parser.ParseMovieTitle(title).Languages.Should().Contain(Language.Klingon);", "unknown member Language.Klingon", "Klingon"},
		{"while loop", call + "            while (true) { }", "unsupported statement while", "while"},
		{"loop bound the source never asserted", call + "            for (var i = 0; i < info.MovieTitles.Count; i++)\n            {\n                info.MovieTitles[i].Should().Be(\"x\");\n            }", "unsupported comparison operands", "info.MovieTitles.Count; i++"},
		{"other using", "            using (new Other())\n            {\n                Parser.Parser.ParseMovieTitle(title).Year.Should().Be(1);\n            }", "only using (new AssertionScope()) is supported", "Other"},
		{"conflicting assertions", call + "            info.Year.Should().Be(1);\n            info.Year.Should().Be(2);", "conflicting assertions on Year", "info.Year.Should().Be(2)"},
		{"result never asserted", call, "the result of Parser.ParseMovieTitle is never asserted", attr},
		{"entry point never called", "            var ParseMovieTitle = 1;", "the case never calls Parser.ParseMovieTitle", attr},
		{"unknown assertion form", call + "            info.Year.Should().BeGreaterThan(1);", "unsupported method Should", "BeGreaterThan"},
		{"normalized title against a plain string", "            var r = Parser.Parser.ParseMovieTitle(title).MovieTitle.CleanMovieTitle();\n            r.Should().Be(\"x\");", "a normalized title is compared only with a normalized title", `r.Should().Be("x")`},
		{"plain title against a normalized string", "            Parser.Parser.ParseMovieTitle(title).PrimaryMovieTitle.Should().Be(title.CleanMovieTitle());", "a normalized title is compared only with a normalized title", "PrimaryMovieTitle"},
		{"membership of a normalized title", "            Parser.Parser.ParseMovieTitle(title).MovieTitle.CleanMovieTitle().Should().Contain(\"x\");", "unsupported assertion on a normalized title", "CleanMovieTitle().Should()"},
		{"const without initializer", call + "            const string x;", "unsupported const declaration", "const string x;"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := titleMethod(attr, tc.body)
			se := extractTitleRejects(t, src)
			if !strings.Contains(se.reason, tc.reason) {
				t.Errorf("reason = %q, want %q", se.reason, tc.reason)
			}
			if want := lineOf(t, src, tc.at); se.line != want {
				t.Errorf("line = %d, want %d (%s)", se.line, want, se)
			}
		})
	}
}

func TestExtractRunRejectingATitleFixturePublishesNothingAndKeepsPriorOutput(t *testing.T) {
	specs := appSpecs()
	sroot := writeTree(t, specs[0], fixtureQuality)
	rroot := writeTree(t, specs[1], fixtureQuality)
	bad := titleMethod(`[TestCase("x")]`, "            var x = title;")
	writeSource(t, rroot, "tests/ParserFixture.cs", bad)
	out := t.TempDir()
	prior := map[string]string{"series.jsonl": "prior series\n", "movie.jsonl": "prior movie\n"}
	for name, text := range prior {
		if err := os.WriteFile(filepath.Join(out, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	code, stdout, stderr := runExtract(t, out, sroot, rroot)
	want := fmt.Sprintf("extract: unsupported app=movie file=tests/ParserFixture.cs line=%d col=", lineOf(t, bad, "void m("))
	if code != 1 || !strings.Contains(stderr, want) || !strings.Contains(stderr, "no recognized entry point") || stdout != "" {
		t.Errorf("exit %d stdout %q stderr %q, want exit 1 with %q and no summary", code, stdout, stderr, want)
	}
	for name, text := range prior {
		if got, err := os.ReadFile(filepath.Join(out, name)); err != nil || string(got) != text {
			t.Errorf("%s = %q (%v), want the prior output kept", name, got, err)
		}
	}
}
