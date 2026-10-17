//go:build !race

package parse

import (
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/parse/corpus"
)

// coverage is the accounting of one committed corpus: collected is every enabled
// scoped source case (quality and title), counted from the corpus itself, and ran
// is every one of them that a real parser run passed, disposed or failed.
type coverage struct {
	collected, ran, passed, disposed, failed, skipped, uncovered int
}

func (c coverage) problems(app string) []string {
	var out []string
	if c.collected == 0 {
		out = append(out, app+": no enabled scoped source case was collected")
	}
	if c.ran != c.collected || c.uncovered != 0 {
		out = append(out, "uncovered enabled cases of "+app)
	}
	if c.failed != 0 || c.skipped != 0 {
		out = append(out, app+": failed or skipped cases")
	}
	return out
}

// runCoverage runs every enabled scoped case of c through its real consumers (the
// quality consumer and the title consumer of the application) and accounts them.
func runCoverage(t *testing.T, app string, c *corpus.Corpus, byID map[string]corpus.Disposition) (coverage, qualityRun, titleRun) {
	t.Helper()
	var cov coverage
	for _, cs := range c.Cases {
		if cs.UpstreamEnabled && (cs.Scope == corpus.ScopeQuality || cs.Scope == corpus.ScopeTitle) {
			cov.collected++
		}
	}
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
	cov.skipped = q.enabled + r.enabled - cov.ran
	cov.uncovered = cov.collected - cov.ran
	return cov, q, r
}

// TestCorpusCoverage asserts that every enabled scoped source case of both
// committed corpora actually ran against the real parser: the cases collected
// from each corpus equal the cases the quality and title consumers executed, with
// no uncovered, skipped or failed case, and each application's whole inventory
// (all scopes) reconciles against its header separately from the selected scopes.
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
			for _, problem := range q.reconcile(c.Header.Counts) {
				t.Error("quality: " + problem)
			}
			for _, problem := range r.reconcile(c.Header.Counts) {
				t.Error("title: " + problem)
			}
			// The two scopes together are every enabled in-scope case of the header.
			if want := c.Header.Counts["enabled"]; cov.collected != want {
				t.Errorf("collected %d enabled scoped cases, header %d", cov.collected, want)
			}
			t.Logf("corpus-coverage app=%s collected=%d ran=%d passed=%d disposed=%d failed=%d skipped=%d uncovered=%d out_of_scope=%d sha256=%s",
				app, cov.collected, cov.ran, cov.passed, cov.disposed, cov.failed, cov.skipped, cov.uncovered, c.Header.Counts["out_of_scope"], hashes[app])
		})
	}
}

// Coverage accounting discriminates: a corpus holding an enabled case no adapter
// reaches, or a case that fails, is reported, so a green run cannot hide a case.
func TestCorpusCoverageReportsUnreachedAndFailedCases(t *testing.T) {
	const name = "Series.Title.S03E06.DVDRip.XviD-WiDE"
	good := seriesCase("t1", ownerSeriesTitle, map[string]corpus.Value{"SeasonNumber": intValue(3)}, str(name))
	quality := mixedCase("q", corpus.ScopeQuality, true, map[string]corpus.Value{"Quality": qualityEnum("DVD")}, name)

	cov, _, _ := runCoverage(t, "series", seriesCorpus(good, quality), nil)
	if p := cov.problems("series"); len(p) != 0 || cov.collected != 2 || cov.ran != 2 || cov.passed != 2 {
		t.Errorf("a fully run corpus is clean: %+v %v", cov, p)
	}

	failing := seriesCase("t2", ownerSeriesTitle, map[string]corpus.Value{"SeasonNumber": intValue(4)}, str(name))
	cov, _, _ = runCoverage(t, "series", seriesCorpus(good, quality, failing), nil)
	if p := cov.problems("series"); len(p) == 0 || cov.failed != 1 {
		t.Errorf("a failing case must be reported: %+v %v", cov, p)
	}

	// A case of an unknown scope is collected only if enabled and in scope: an enabled title
	// case whose owner has no adapter fails, it is never skipped.
	unreached := seriesCase("t3", "Parser.NoAdapter", map[string]corpus.Value{titleResultKey: str("x")}, str(name))
	cov, _, _ = runCoverage(t, "series", seriesCorpus(good, quality, unreached), nil)
	if p := cov.problems("series"); len(p) == 0 || cov.failed != 1 || cov.collected != 3 {
		t.Errorf("a case without an adapter must be a failure: %+v %v", cov, p)
	}

	empty := seriesCorpus(mixedCase("o", corpus.ScopeOutOfScope, true, nil, name))
	if cov, _, _ = runCoverage(t, "series", empty, nil); len(cov.problems("series")) == 0 {
		t.Error("a corpus without an enabled scoped case must fail")
	}
}

// A retained difference needs a disposition bound to the case's exact expected
// and observed values and to an answered human question. A changed expectation,
// an unanswered question, a disabled case or a case that no longer differs is
// reported; the original enabled case is neither lost nor relabeled disabled.
func TestDispositionValidity(t *testing.T) {
	const name = "Series.Title.S03E06.DVDRip.XviD-WiDE"
	want := map[string]corpus.Value{"SeasonNumber": intValue(9)}
	cs := seriesCase("d", ownerSeriesTitle, want, str(name))
	valid := corpus.Disposition{
		CaseID: "d", Expected: want, Observed: map[string]corpus.Value{"SeasonNumber": intValue(3)},
		Rationale: "r", Question: "q", Answer: "a", AnsweredBy: "owner",
	}
	validate := func(d corpus.Disposition, c corpus.Case) error {
		return (&corpus.Dispositions{Schema: corpus.SchemaVersion, Dispositions: []corpus.Disposition{d}}).Validate(seriesCorpus(c))
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
	// A disabled case cannot be disposed: a disposition never excuses a relabeled case.
	disabled := cs
	disabled.UpstreamEnabled = false
	if err := validate(valid, disabled); err == nil {
		t.Error("a disposition on a disabled case must not validate")
	}

	// Observed values that differ from the bound ones leave the case failing: the disposition is exact.
	moved := valid
	moved.Observed = map[string]corpus.Value{"SeasonNumber": intValue(4)}
	if r := runSeries(t, cs, map[string]corpus.Disposition{"d": moved}); r.failed != 1 || r.disposed != 0 {
		t.Errorf("a disposition bound to another observation must fail: %+v", r)
	}

	// Relabeling an enabled case disabled changes the inventory the header counts.
	enabled := seriesCorpus(cs)
	relabeled := cs
	relabeled.UpstreamEnabled = false
	now, loc := seriesRunClock(t)
	rr := runSeriesCorpus(newSeriesTestParser(t, now, loc), seriesCorpus(relabeled), nil, corpus.TimeContext{Now: now, Location: loc})
	if problems := rr.reconcile(enabled.Header.Counts); len(problems) == 0 {
		t.Error("a case relabeled disabled must be reported against the header counts")
	}
}
