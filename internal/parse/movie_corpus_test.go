//go:build !race

package parse

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/parse/corpus"
)

// TestMovieCorpus runs every title case of the committed movie corpus
// through the real parser: file-name movie title cases through ParseTitle,
// folder-name cases ($isDir) through public ParseDirTitle, and the release
// group and language cases through the parsers those entries run. A case passes only when every
// assertion holds for every input; a retained mismatch needs an exact,
// answered disposition that binds every mismatching input observation, and a
// stale or ownerless disposition fails. The whole inventory (both scopes)
// reconciles against the corpus's per-scope counts separately from the selected
// title scope, whose count must be positive for every entry point.
func TestMovieCorpus(t *testing.T) {
	disp := readDispositions(t)
	var all []corpus.Case
	for _, app := range []string{"series", "movie"} {
		c, _ := readCorpusFile(t, app)
		all = append(all, c.Cases...)
	}
	if err := disp.Validate(&corpus.Corpus{Cases: all}); err != nil {
		t.Fatalf("dispositions: %v", err)
	}

	c, hash := readCorpusFile(t, "movie")
	byOwner := map[string]int{}
	for _, cs := range c.Cases {
		if cs.Scope == corpus.ScopeTitle {
			byOwner[cs.ScopeOwner]++
		}
	}
	for _, owner := range []string{ownerMovieTitle, ownerReleaseGroup, ownerLanguages, ownerCleanTitle} {
		if byOwner[owner] == 0 {
			t.Errorf("no title case of entry point %s", owner)
		}
	}

	r := runTitleCorpus(newTestParser(t, time.Now), c, dispositionsByID(disp))
	for _, problem := range append(r.problems, r.reconcile(scopeCounts(c))...) {
		t.Error(problem)
	}
	t.Logf("movie-title-corpus cases=%d title=%d (title=%d group=%d languages=%d clean=%d) passed=%d disposed=%d quality=%d inputs=%d sha256=%s",
		len(c.Cases), r.cases, byOwner[ownerMovieTitle], byOwner[ownerReleaseGroup], byOwner[ownerLanguages], byOwner[ownerCleanTitle],
		r.passed, r.disposed, r.other, r.inputs, hash)
}

// titleCase is one synthetic title-corpus case.
func titleCase(id, owner string, want map[string]corpus.Value, inputs ...string) corpus.Case {
	cs := mixedCase(id, corpus.ScopeTitle, want, inputs...)
	cs.ScopeOwner = owner
	return cs
}

func languageEnum(member string) corpus.Value {
	return corpus.Value{Kind: corpus.KindEnum, Type: "Language", Str: member}
}

func TestMovieCorpusConsumerReconcilesInventoryAndSelectedScopeSeparately(t *testing.T) {
	p := newTestParser(t, time.Now)
	const name = "The.Movie.Name.2015.German.DL.1080p.BluRay.x264-GRP"
	pass := map[string]corpus.Value{
		"PrimaryMovieTitle": str("The Movie Name"), "Year": {Kind: corpus.KindInt, Int: 2015},
		"Languages.Contain": {Kind: corpus.KindArray, Items: []corpus.Value{languageEnum("German"), languageEnum("Original")}},
		"ReleaseGroup":      str("GRP"), "Edition": str(""), "HardcodedSubs": {Kind: corpus.KindNull},
	}
	quality := mixedCase("q", corpus.ScopeQuality, map[string]corpus.Value{"Quality": qualityEnum("Bluray1080p")}, name)
	c := mixedCorpus(
		titleCase("t1", ownerMovieTitle, pass, name),
		titleCase("t2", ownerMovieTitle, pass, name),
		quality,
	)
	r := runTitleCorpus(p, c, nil)
	if len(r.problems) != 0 || r.cases != 2 || r.passed != 2 || r.other != 1 {
		t.Fatalf("run = %+v", r)
	}
	if problems := r.reconcile(scopeCounts(c)); len(problems) != 0 {
		t.Errorf("a consistent mixed inventory must reconcile: %v", problems)
	}

	// A corpus whose counts miscount the whole inventory is reported, whatever the selected scope did.
	counts := maps.Clone(scopeCounts(c))
	counts[corpus.ScopeQuality]++
	if problems := r.reconcile(counts); len(problems) == 0 {
		t.Error("a miscounted inventory must be reported")
	}

	// No selected title case is a failure, never a zero-selection pass: quality names do not stand in.
	none := mixedCorpus(quality)
	if problems := runTitleCorpus(p, none, nil).reconcile(scopeCounts(none)); len(problems) == 0 {
		t.Error("a corpus without a title case must fail")
	}
}

func TestMovieCorpusConsumerResolvesEveryEntryPointAndRejectsUnknownAssertions(t *testing.T) {
	p := newTestParser(t, time.Now)
	null := corpus.Value{Kind: corpus.KindNull}
	for _, tc := range []struct {
		name, owner, input string
		want               map[string]corpus.Value
	}{
		{"rejected hashed release has no title", ownerMovieTitle, "185d86a343e39f3341e35c4dad3f9959",
			map[string]corpus.Value{titleResultKey: null}},
		{"release group", ownerReleaseGroup, "Movie.Name.2022.1080p.BluRay.x264-VARYG",
			map[string]corpus.Value{titleResultKey: str("VARYG")}},
		{"no release group is null", ownerReleaseGroup, "Movie.Name.2022.1080p.BluRay.x264",
			map[string]corpus.Value{titleResultKey: null}},
		{"languages list", ownerLanguages, "Movie.Title.1994.French.1080p.XviD-LOL",
			map[string]corpus.Value{titleResultKey + ".Contain": {Kind: corpus.KindArray, Items: []corpus.Value{languageEnum("French")}}}},
		{"first language identity", ownerLanguages, "Movie.Title.1.WEB-DL.720p.Complete.Latino.YG",
			map[string]corpus.Value{"First().Id": languageEnum("SpanishLatino")}},
		{"alternative titles by index and count", ownerMovieTitle, "Skjegg.AKA.Rox.Beard.1965.CD1.CRiTERiON.DVDRip.XviD-KG.avi",
			map[string]corpus.Value{
				"MovieTitles.Count": {Kind: corpus.KindInt, Int: 3}, "MovieTitles[0]": str("Skjegg AKA Rox Beard"),
				"MovieTitles[1]": str("Skjegg"), "MovieTitles[2]": str("Rox Beard"),
				"Languages.First()": languageEnum("Unknown"), "Languages.Count": {Kind: corpus.KindInt, Int: 1},
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := mixedCorpus(titleCase("t", tc.owner, tc.want, tc.input))
			if r := runTitleCorpus(p, c, nil); r.passed != 1 || len(r.problems) != 0 {
				t.Errorf("run = %+v", r)
			}
		})
	}

	// A wrong expectation is a counted failure with the observed value, not a pass.
	wrong := mixedCorpus(titleCase("w", ownerReleaseGroup, map[string]corpus.Value{titleResultKey: str("OTHER")}, "Movie.Name.2022.1080p.BluRay.x264-VARYG"))
	if r := runTitleCorpus(p, wrong, nil); r.failed != 1 || r.passed != 0 || len(r.problems) != 1 {
		t.Errorf("a wrong expectation must fail once: %+v", r)
	}
	// An unknown property, member or entry point is a hard error, never a skipped assertion.
	for name, cs := range map[string]corpus.Case{
		"unknown property": titleCase("u1", ownerMovieTitle, map[string]corpus.Value{"Nonsense": str("x")}, "Movie.2015.1080p-GRP"),
		"unknown language": titleCase("u2", ownerLanguages, map[string]corpus.Value{titleResultKey + ".Contain": {Kind: corpus.KindArray, Items: []corpus.Value{languageEnum("Klingon")}}}, "Movie.2015.English"),
		"unknown owner":    titleCase("u3", "Parser.Other", map[string]corpus.Value{titleResultKey: str("x")}, "Movie.2015.English"),
	} {
		if r := runTitleCorpus(p, mixedCorpus(cs), nil); r.failed != 1 || len(r.problems) == 0 {
			t.Errorf("%s must fail: %+v", name, r)
		}
	}
}

func TestMovieCorpusDispositionBindsEveryMismatchingInput(t *testing.T) {
	p := newTestParser(t, time.Now)
	cs := titleCase("d", ownerMovieTitle, map[string]corpus.Value{"Year": {Kind: corpus.KindInt, Int: 1999}},
		"Movie.Name.2015.1080p.BluRay.x264-GRP", "Movie.Name.2016.1080p.BluRay.x264-GRP")
	c := mixedCorpus(cs)
	year := func(n int64) map[string]corpus.Value {
		return map[string]corpus.Value{"Year": {Kind: corpus.KindInt, Int: n}}
	}
	bind := func(observed int64) map[string]corpus.Disposition {
		return map[string]corpus.Disposition{"d": {CaseID: "d", Expected: year(1999), Observed: year(observed)}}
	}
	if r := runTitleCorpus(p, c, nil); r.failed != 1 || r.disposed != 0 {
		t.Errorf("an undisposed mismatch must fail: %+v", r)
	}
	// One observed value (2015) does not cover the other input's (2016).
	if r := runTitleCorpus(p, c, bind(2015)); r.failed != 1 || r.disposed != 0 {
		t.Errorf("a disposition must bind every distinct observation: %+v", r)
	}
	// A disposition on a case that passes is stale.
	pass := mixedCorpus(titleCase("d", ownerMovieTitle, year(2015), "Movie.Name.2015.1080p.BluRay.x264-GRP"))
	if r := runTitleCorpus(p, pass, bind(2015)); r.failed != 1 || r.passed != 0 {
		t.Errorf("a stale disposition must fail: %+v", r)
	}
	one := mixedCorpus(titleCase("d", ownerMovieTitle, year(1999), "Movie.Name.2015.1080p.BluRay.x264-GRP"))
	if r := runTitleCorpus(p, one, bind(2015)); r.disposed != 1 || r.failed != 0 || len(r.problems) != 0 {
		t.Errorf("an exact disposition must bind the one observed value: %+v", r)
	}
}

// longTail extends a release name with one long dotted word up to n bytes. A
// name whose title, or whose German tag, is thousands of characters long
// instead exceeds the 50 ms match timeout (the unanchored
// bracketed-alternative and German-remux patterns scan quadratically), which
// is itself a fail-closed outcome; a bare run of letters would be rejected as
// a hashed release.
func longTail(name string, n int) string {
	return name + "." + strings.Repeat("a", n-len(name)-1)
}

// A legitimate name of exactly 4096 bytes is the intended movie. The control
// runs on the real clock (the 250 ms budget and the 50 ms match timeout are
// wall time), so it is excluded under -race with the complete corpus run.
func TestMovieBoundsLegitimate4096ByteNameParses(t *testing.T) {
	const control = "Movie.Name.2015.1080p.BluRay.x264-GRP"
	name := longTail(control, 4096)
	if len(name) != 4096 {
		t.Fatalf("control is %d bytes", len(name))
	}
	pt, err := newTestParser(t, time.Now).ParseTitle(context.Background(), name, KindMovie)
	if err != nil {
		t.Fatalf("4096-byte control: %v", err)
	}
	m := pt.Movie
	if pt.Kind != KindMovie || m == nil || m.Year != 2015 || m.ReleaseGroup != "GRP" || m.OriginalTitle != name ||
		!slices.Equal(m.MovieTitles, []string{"Movie Name"}) || !slices.Equal(m.Languages, []string{"unknown"}) || m.Quality.Code != "bluray-1080p" {
		t.Errorf("4096-byte control parsed as %+v", pt)
	}
}

// A rejected title has no movie: every property assertion dereferences
// it and fails, whatever it expects. Only an explicit $result null may compare
// as null.
func TestMovieCorpusMissingMovieFailsEveryPropertyAssertion(t *testing.T) {
	p := newTestParser(t, time.Now)
	const hashed = "185d86a343e39f3341e35c4dad3f9959"
	null := corpus.Value{Kind: corpus.KindNull}
	zero := corpus.Value{Kind: corpus.KindInt}
	if r := runTitleCorpus(p, mixedCorpus(titleCase("n", ownerMovieTitle, map[string]corpus.Value{titleResultKey: null}, hashed)), nil); r.passed != 1 || r.failed != 0 {
		t.Fatalf("the explicit null result of a rejected title must pass: %+v", r)
	}
	for name, want := range map[string]map[string]corpus.Value{
		"HardcodedSubs null":    {"HardcodedSubs": null},
		"empty edition":         {"Edition": str("")},
		"empty release group":   {"ReleaseGroup": str("")},
		"null release group":    {"ReleaseGroup": null},
		"zero year":             {"Year": zero},
		"zero titles":           {"MovieTitles.Count": zero},
		"zero languages":        {"Languages.Count": zero},
		"no title at an index":  {"MovieTitles[3]": null},
		"result null and a key": {titleResultKey: null, "HardcodedSubs": null},
	} {
		r := runTitleCorpus(p, mixedCorpus(titleCase("m", ownerMovieTitle, want, hashed)), nil)
		if r.failed != 1 || r.passed != 0 || len(r.problems) == 0 {
			t.Errorf("%s: a missing movie must fail the assertion, got %+v", name, r)
		}
	}
}

// $isDir marks a folder-name case. The corpus sends it through public
// ParseDirTitle. ParseTitle has no directory argument and still rejects the
// year-first folder name.
func TestMovieCorpusDirectoryCasesUseThePublicDirectoryEntry(t *testing.T) {
	p := newTestParser(t, time.Now)
	const dirName = "(1995) Movie Name"
	want := map[string]corpus.Value{"PrimaryMovieTitle": str("Movie Name"), "Year": {Kind: corpus.KindInt, Int: 1995}}
	dir := map[string]corpus.Value{titleDirKey: {Kind: corpus.KindBool, Bool: true}}
	for k, v := range want {
		dir[k] = v
	}

	r := runTitleCorpus(p, mixedCorpus(titleCase("d", ownerMovieTitle, dir, dirName)), nil)
	if r.passed != 1 || r.failed != 0 {
		t.Errorf("the folder-name case must pass through ParseDirTitle: %+v", r)
	}
	pt, err := p.ParseDirTitle(context.Background(), dirName, KindMovie)
	if err != nil || pt.Movie == nil || len(pt.Movie.MovieTitles) == 0 || pt.Movie.MovieTitles[0] != "Movie Name" || pt.Movie.Year != 1995 {
		t.Errorf("ParseDirTitle(%q) = %+v, %v", dirName, pt, err)
	}
	// Without $isDir the same assertions run through ParseTitle, which rejects the name.
	if r := runTitleCorpus(p, mixedCorpus(titleCase("n", ownerMovieTitle, want, dirName)), nil); r.failed != 1 || r.passed != 0 {
		t.Errorf("ParseTitle must not read a year-first folder name: %+v", r)
	}
	_, err = p.ParseTitle(context.Background(), dirName, KindMovie)
	wantReason(t, err, codeTitleUnrecognized, map[string]int64{})

	// An ordinary case without $isDir stays on ParseTitle.
	plain := titleCase("p", ownerMovieTitle, map[string]corpus.Value{"Year": {Kind: corpus.KindInt, Int: 2015}}, "Movie.Name.2015.1080p.BluRay.x264-GRP")
	if r := runTitleCorpus(p, mixedCorpus(plain), nil); r.passed != 1 {
		t.Errorf("a case without $isDir is a ParseTitle case: %+v", r)
	}
}

// The release hash, release title and simple release title through the public
// ParseTitle. Expected values are worked out by hand from the title
// patterns, not read from a parser run.
func TestMovieCorpusPublicParseTitleReleaseFields(t *testing.T) {
	p := newTestParser(t, time.Now)
	for _, tc := range []struct {
		name                               string
		input                              string
		hash, releaseTitle, simpleRelTitle string
		group                              string
		titles                             []string
	}{
		{
			// The anime hash is the last eight-character bracket; the title group is
			// "Cowboy Bebop - Knockin' on Heaven's Door " (41 UTF-16 units, its trailing
			// space belongs to the group, which ends before the "v2 " version), at index 7.
			"anime hash", "[Arid] Cowboy Bebop - Knockin' on Heaven's Door v2 [00F4CDA0].mkv",
			"00F4CDA0", "[Arid] Cowboy Bebop - Knockin' on Heaven's Door v2 [00F4CDA0]", "[Arid] A Moviev2 [00F4CDA0]",
			"Arid", []string{"Cowboy Bebop - Knockin' on Heaven's Door"},
		},
		{
			// The resolution is stripped from the simple title before the patterns run,
			// so it can never be captured as the hash.
			"1280x720 is not the hash", "[Arid] Movie Name 2015 [1280x720]",
			"", "[Arid] Movie Name 2015 [1280x720]", "[Arid] A Movie 2015 [1280x720]",
			"Arid", []string{"Movie Name"},
		},
		{
			// Group index 7 and length 6 are UTF-16 units, while the bytes are 8 and 7.
			"non-ASCII title index", "[Ärid] Amélie 2001 [A1B2C3D4]",
			"A1B2C3D4", "[Ärid] Amélie 2001 [A1B2C3D4]", "[Ärid] A Movie 2001 [A1B2C3D4]",
			"Ärid", []string{"Amélie"},
		},
		{
			"extension removed, dotted title", "Movie.Name.2015.1080p.BluRay.x264-GRP.mkv",
			"", "Movie.Name.2015.1080p.BluRay.x264-GRP", "A.Movie.2015.1080p.BluRay.x264-GRP",
			"GRP", []string{"Movie Name"},
		},
		{
			// The simple release title drops a pipe with the white space before it.
			"pipe dropped", "Movie Name 2015 | 1080p BluRay x264-GRP",
			"", "Movie Name 2015 | 1080p BluRay x264-GRP", "A Movie 2015 1080p BluRay x264-GRP",
			"GRP", []string{"Movie Name"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pt, err := p.ParseTitle(context.Background(), tc.input, KindMovie)
			if err != nil || pt.Movie == nil {
				t.Fatalf("ParseTitle(%q) = %+v, %v", tc.input, pt, err)
			}
			m := pt.Movie
			if m.ReleaseHash != tc.hash || m.ReleaseTitle != tc.releaseTitle || m.SimpleReleaseTitle != tc.simpleRelTitle ||
				m.ReleaseGroup != tc.group || m.OriginalTitle != tc.input || !slices.Equal(m.MovieTitles, tc.titles) {
				t.Errorf("ParseTitle(%q):\n hash %q, want %q\n release title %q, want %q\n simple release title %q, want %q\n group %q, want %q\n original %q\n titles %q, want %q",
					tc.input, m.ReleaseHash, tc.hash, m.ReleaseTitle, tc.releaseTitle, m.SimpleReleaseTitle, tc.simpleRelTitle,
					m.ReleaseGroup, tc.group, m.OriginalTitle, m.MovieTitles, tc.titles)
			}
		})
	}
}

// A few public language codes, fixed by hand (the full member table is
// TestLanguageVocabularyMatchesIndependentMemberTable).
func TestMovieCorpusPublicParseTitleLanguageCodes(t *testing.T) {
	p := newTestParser(t, time.Now)
	for _, tc := range []struct {
		input string
		want  []string
	}{
		{"Movie.Title.1994.French.1080p.XviD-LOL", []string{"french"}},
		{"Movie.Title.2016.Latino.1080p.WEB-DL.x264-GRP", []string{"spanish-latino"}},
		{"Movie.Title.2016.Brazilian.1080p.WEB-DL.x264-GRP", []string{"portuguese-br"}},
		{"Movie.Title.2016.German.DL.1080p.BluRay.x264-GRP", []string{"german", "original"}},
		{"Movie.Title.2018.1080p.WEB-DL.x264-GRP", []string{"unknown"}},
	} {
		pt, err := p.ParseTitle(context.Background(), tc.input, KindMovie)
		if err != nil || pt.Movie == nil {
			t.Errorf("ParseTitle(%q) = %+v, %v", tc.input, pt, err)
			continue
		}
		if !slices.Equal(pt.Movie.Languages, tc.want) {
			t.Errorf("ParseTitle(%q).Languages = %q, want %q", tc.input, pt.Movie.Languages, tc.want)
		}
	}
}

// A non-ASCII title (Cyrillic: one UTF-16 unit but two bytes per
// letter). The title group is 26 units long and starts at 0; the simple title
// loses "2160p" and "H.265" (the title patterns read it), while the simple
// release title keeps both and loses only " |"; a byte length would cut the
// replacement 10 characters too far.
func TestMovieCorpusPublicParseTitleNonASCIIName(t *testing.T) {
	const name = "Енола Голмс / Enola Holmes (2020) UHD WEB-DL 2160p 4K HDR H.265 Ukr/Eng | Sub Ukr/Eng"
	pt, err := newTestParser(t, time.Now).ParseTitle(context.Background(), name, KindMovie)
	if err != nil || pt.Movie == nil {
		t.Fatalf("ParseTitle = %+v, %v", pt, err)
	}
	m := pt.Movie
	const simple = "A Movie (2020) UHD WEB-DL 2160p 4K HDR H.265 Ukr/Eng Sub Ukr/Eng"
	if m.ReleaseHash != "" || m.ReleaseTitle != name || m.SimpleReleaseTitle != simple || m.Year != 2020 ||
		!slices.Equal(m.MovieTitles, []string{"Енола Голмс / Enola Holmes", "Енола Голмс", "Enola Holmes"}) {
		t.Errorf("hash %q, release title %q, simple release title %q (want %q), year %d, titles %q",
			m.ReleaseHash, m.ReleaseTitle, m.SimpleReleaseTitle, simple, m.Year, m.MovieTitles)
	}
}

// Parser.CleanMovieTitle against literals of the normalization cases and of
// the accent case, fixed by hand. A letter whose decomposition is not handled is
// an error, not a wrong string.
func TestMovieCorpusCleanMovieTitleMatchesLiterals(t *testing.T) {
	p := newTestParser(t, nil)
	for in, want := range map[string]string{
		"Conan": "conan", "Castle (2009)": "castle2009", "Parenthood.2010": "parenthood2010",
		"Law_and_Order_SVU": "lawordersvu", "CaPitAl": "capital", "peri.od": "period",
		"this.^&%^**$%@#$!That": "thisthat", "test/test": "testtest", "90210": "90210", "24": "24",
		"I'm a cyborg, but that's OK": "imcyborgbutthatsok", "Im a cyborg, but thats ok": "imcyborgbutthatsok",
		"Test: Something à Deux": "testsomethingdeux", "Parler à": "parlera", "The Office": "theoffice",
		"The.Daily.Show": "thedailyshow", "Tokyo Ghoul A": "tokyoghoula", "Movie Title": "movietitle",
		"The Movie Title Bros.": "themovietitlebros", "Müller": "mueller", "Straße": "strasse", "": "",
		// The accent test: precomposed and decomposed forms fold alike.
		"Carniv\u00E0le": "carnivale", "Carniva\u0300le": "carnivale",
		"Amélie": "amelie", "Crème Brûlée": "cremebrulee", "Ångström": "angstroem",
	} {
		if got, err := p.CleanMovieTitle(in); err != nil || got != want {
			t.Errorf("CleanMovieTitle(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"Bjørn", "Łódź", "Енола", "Phở"} {
		if got, err := p.CleanMovieTitle(in); err == nil {
			t.Errorf("CleanMovieTitle(%q) = %q: a letter outside the handled decompositions must be an error", in, got)
		}
	}
}

// The accent case is a title case of the helper itself: its input is the
// string it normalizes and its expectation the folded title.
func TestMovieCorpusAccentTitleCase(t *testing.T) {
	p := newTestParser(t, time.Now)
	result := func(want string) map[string]corpus.Value { return map[string]corpus.Value{titleResultKey: str(want)} }
	if r := runTitleCorpus(p, mixedCorpus(titleCase("ok", ownerCleanTitle, result("carnivale"), "Carniv\u00E0le")), nil); r.passed != 1 || r.failed != 0 {
		t.Errorf("the accent must fold: %+v", r)
	}
	for label, cs := range map[string]corpus.Case{
		"unfolded expectation": titleCase("w", ownerCleanTitle, result("carnivàle"), "Carniv\u00E0le"),
		"another title":        titleCase("o", ownerCleanTitle, result("carnival"), "Carniv\u00E0le"),
		"unfoldable input":     titleCase("u", ownerCleanTitle, result("bjorn"), "Bj\u00F8rn"),
		"not a string":         titleCase("n", ownerCleanTitle, map[string]corpus.Value{titleResultKey: {Kind: corpus.KindInt}}, "Carniv\u00E0le"),
		"unknown property":     titleCase("p", ownerCleanTitle, map[string]corpus.Value{"Nonsense": str("x")}, "Carniv\u00E0le"),
	} {
		if r := runTitleCorpus(p, mixedCorpus(cs), nil); r.failed != 1 || r.passed != 0 || len(r.problems) == 0 {
			t.Errorf("%s must fail: %+v", label, r)
		}
	}
}

// A normalized-title assertion compares the parsed primary title and the expected
// title after normalization; a rejected name fails it.
func TestMovieCorpusNormalizedTitleAssertion(t *testing.T) {
	p := newTestParser(t, time.Now)
	const name = "[www.test.com] - Movie.Title.2023.720p.HDTV.X264-DIMENSION"
	norm := func(title string) map[string]corpus.Value {
		return map[string]corpus.Value{"MovieTitle.CleanMovieTitle": str(title)}
	}
	if r := runTitleCorpus(p, mixedCorpus(titleCase("ok", ownerMovieTitle, norm("Movie Title"), name)), nil); r.passed != 1 || r.failed != 0 {
		t.Errorf("the URL prefix must not reach the title: %+v", r)
	}
	// Only the normalization is compared, so the spelling of the expected title may differ.
	if r := runTitleCorpus(p, mixedCorpus(titleCase("ok", ownerMovieTitle, norm("movie.title"), name)), nil); r.passed != 1 {
		t.Errorf("both sides are normalized: %+v", r)
	}
	for label, cs := range map[string]corpus.Case{
		"another title":  titleCase("w", ownerMovieTitle, norm("Other Title"), name),
		"url kept":       titleCase("u", ownerMovieTitle, norm("www.test.com Movie Title"), name),
		"rejected name":  titleCase("r", ownerMovieTitle, norm("Movie Title"), "185d86a343e39f3341e35c4dad3f9959"),
		"unfoldable":     titleCase("a", ownerMovieTitle, norm("Movie Titlø"), name),
		"not a string":   titleCase("n", ownerMovieTitle, map[string]corpus.Value{"MovieTitle.CleanMovieTitle": {Kind: corpus.KindNull}}, name),
		"owner mismatch": titleCase("g", ownerReleaseGroup, norm("Movie Title"), name),
	} {
		if r := runTitleCorpus(p, mixedCorpus(cs), nil); r.failed != 1 || r.passed != 0 || len(r.problems) == 0 {
			t.Errorf("%s must fail: %+v", label, r)
		}
	}
}
