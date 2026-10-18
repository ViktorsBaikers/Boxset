//go:build !race

package parse

import (
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/parse/corpus"
)

// coverage is the accounting of one committed corpus: collected is every case
// (quality and title), counted from the corpus itself, and ran is every one of
// them that a real parser run passed, disposed or failed.
type coverage struct {
	collected, ran, passed, disposed, failed, skipped, uncovered int
}

func (c coverage) problems(app string) []string {
	var out []string
	if c.collected == 0 {
		out = append(out, app+": no case was collected")
	}
	if c.ran != c.collected || c.uncovered != 0 {
		out = append(out, "uncovered cases of "+app)
	}
	if c.failed != 0 || c.skipped != 0 {
		out = append(out, app+": failed or skipped cases")
	}
	return out
}

// runCoverage runs every case of c through its real consumers (the quality
// consumer and the title consumer of the application) and accounts them.
func runCoverage(t *testing.T, app string, c *corpus.Corpus, byID map[string]corpus.Disposition) (coverage, qualityRun, titleRun) {
	t.Helper()
	cov := coverage{collected: len(c.Cases)}
	var (
		q qualityRun
		r titleRun
	)
	if app == "series" {
		now, loc := seriesRunClock(t)
		q = runQualityCorpus(newTestParser(t, time.Now), KindSeries, app, c, byID)
		r = runSeriesCorpus(newSeriesTestParser(t, now, loc), c, byID, corpus.TimeContext{Now: now, Location: loc})
	} else {
		q = runQualityCorpus(newTestParser(t, time.Now), KindMovie, app, c, byID)
		r = runTitleCorpus(newTestParser(t, time.Now), c, byID)
	}
	cov.passed, cov.disposed, cov.failed = q.passed+r.passed, q.disposed+r.disposed, q.failed+r.failed
	cov.ran = cov.passed + cov.disposed + cov.failed
	cov.skipped = q.cases + r.cases - cov.ran
	cov.uncovered = cov.collected - cov.ran
	return cov, q, r
}

// TestCorpusCoverage asserts that every case of both committed corpora
// actually ran against the real parser: the cases collected from each corpus
// equal the cases the quality and title consumers executed, with no uncovered,
// skipped or failed case, and each application's whole inventory (both scopes)
// reconciles against its per-scope counts separately from the selected scopes.
// A case an adapter does not reach is a failure, even when every other case
// passes.
func TestCorpusCoverage(t *testing.T) {
	disp := readDispositions(t)
	var all []corpus.Case
	corpora := map[string]*corpus.Corpus{}
	hashes := map[string]string{}
	for _, app := range []string{"series", "movie"} {
		c, h := readCorpusFile(t, app)
		corpora[app], hashes[app] = c, h
		all = append(all, c.Cases...)
	}
	if err := disp.Validate(&corpus.Corpus{Cases: all}); err != nil {
		t.Fatalf("dispositions: %v", err)
	}
	byID := dispositionsByID(disp)

	for _, app := range []string{"series", "movie"} {
		t.Run(app, func(t *testing.T) {
			c := corpora[app]
			cov, q, r := runCoverage(t, app, c, byID)
			for _, problem := range cov.problems(app) {
				t.Error(problem)
			}
			for _, problem := range append(q.problems, r.problems...) {
				t.Error(problem)
			}
			counts := scopeCounts(c)
			for _, problem := range q.reconcile(counts) {
				t.Error("quality: " + problem)
			}
			for _, problem := range r.reconcile(counts) {
				t.Error("title: " + problem)
			}
			// The two scopes together are every case of the corpus.
			if q.cases+r.cases != len(c.Cases) {
				t.Errorf("the consumers ran %d quality and %d title cases of %d", q.cases, r.cases, len(c.Cases))
			}
			t.Logf("corpus-coverage app=%s collected=%d ran=%d passed=%d disposed=%d failed=%d skipped=%d uncovered=%d sha256=%s",
				app, cov.collected, cov.ran, cov.passed, cov.disposed, cov.failed, cov.skipped, cov.uncovered, hashes[app])
		})
	}
}

// Coverage accounting discriminates: a corpus holding a case no adapter
// reaches, or a case that fails, is reported, so a green run cannot hide a case.
func TestCorpusCoverageReportsUnreachedAndFailedCases(t *testing.T) {
	const name = "Series.Title.S03E06.DVDRip.XviD-WiDE"
	good := seriesCase("t1", ownerSeriesTitle, map[string]corpus.Value{"SeasonNumber": intValue(3)}, str(name))
	quality := mixedCase("q", corpus.ScopeQuality, map[string]corpus.Value{"Quality": qualityEnum("DVD")}, name)

	cov, _, _ := runCoverage(t, "series", mixedCorpus(good, quality), nil)
	if p := cov.problems("series"); len(p) != 0 || cov.collected != 2 || cov.ran != 2 || cov.passed != 2 {
		t.Errorf("a fully run corpus is clean: %+v %v", cov, p)
	}

	failing := seriesCase("t2", ownerSeriesTitle, map[string]corpus.Value{"SeasonNumber": intValue(4)}, str(name))
	cov, _, _ = runCoverage(t, "series", mixedCorpus(good, quality, failing), nil)
	if p := cov.problems("series"); len(p) == 0 || cov.failed != 1 {
		t.Errorf("a failing case must be reported: %+v %v", cov, p)
	}

	// A title case whose owner has no adapter fails, it is never skipped.
	unreached := seriesCase("t3", "Parser.NoAdapter", map[string]corpus.Value{titleResultKey: str("x")}, str(name))
	cov, _, _ = runCoverage(t, "series", mixedCorpus(good, quality, unreached), nil)
	if p := cov.problems("series"); len(p) == 0 || cov.failed != 1 || cov.collected != 3 {
		t.Errorf("a case without an adapter must be a failure: %+v %v", cov, p)
	}

	if cov, _, _ = runCoverage(t, "series", mixedCorpus(), nil); len(cov.problems("series")) == 0 {
		t.Error("a corpus without a case must fail")
	}
}

// A retained difference needs a disposition bound to the case's exact expected
// and observed values and to an answered human question. A changed expectation,
// an unanswered question or a case that no longer differs is reported; a case
// relabeled as the other scope fails instead of passing unnoticed.
func TestDispositionValidity(t *testing.T) {
	const name = "Series.Title.S03E06.DVDRip.XviD-WiDE"
	want := map[string]corpus.Value{"SeasonNumber": intValue(9)}
	cs := seriesCase("d", ownerSeriesTitle, want, str(name))
	valid := corpus.Disposition{
		CaseID: "d", Expected: want, Observed: map[string]corpus.Value{"SeasonNumber": intValue(3)},
		Rationale: "r", Question: "q", Answer: "a", AnsweredBy: "owner",
	}
	validate := func(d corpus.Disposition, c corpus.Case) error {
		return (&corpus.Dispositions{Schema: corpus.DispositionsVersion, Dispositions: []corpus.Disposition{d}}).Validate(mixedCorpus(c))
	}
	if err := validate(valid, cs); err != nil {
		t.Fatalf("a bound, answered disposition must validate: %v", err)
	}

	// The accounted case is disposed, separately from the passed cases.
	r := runSeries(t, cs, map[string]corpus.Disposition{"d": valid})
	if r.disposed != 1 || r.passed != 0 || r.failed != 0 || len(r.problems) != 0 {
		t.Errorf("a disposed case is accounted apart: %+v", r)
	}

	changed := valid
	changed.Expected = map[string]corpus.Value{"SeasonNumber": intValue(8)}
	ownerless := valid
	ownerless.Answer, ownerless.AnsweredBy = "", ""
	other := valid
	other.CaseID = "other"
	for name, d := range map[string]corpus.Disposition{"changed expected value": changed, "unanswered": ownerless, "unknown case": other} {
		if err := validate(d, cs); err == nil {
			t.Errorf("%s must not validate", name)
		}
	}

	// Observed values that differ from the bound ones leave the case failing: the disposition is exact.
	moved := valid
	moved.Observed = map[string]corpus.Value{"SeasonNumber": intValue(4)}
	if r := runSeries(t, cs, map[string]corpus.Disposition{"d": moved}); r.failed != 1 || r.disposed != 0 {
		t.Errorf("a disposition bound to another observation must fail: %+v", r)
	}

	// A title case relabeled as a quality case is run by the quality consumer, which cannot assert its property.
	relabeled := cs
	relabeled.Scope = corpus.ScopeQuality
	if cov, _, _ := runCoverage(t, "series", mixedCorpus(relabeled), nil); len(cov.problems("series")) == 0 {
		t.Error("a case relabeled as the other scope must be reported")
	}
}
