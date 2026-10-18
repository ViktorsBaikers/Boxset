//go:build !race

package parse

import (
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/parse/corpus"
)

// TestQualityCorpus runs every quality case of both committed corpora
// through the real parser. A case passes only when every assertion
// holds for every input; a retained mismatch needs an exact, answered
// disposition that binds every mismatching input observation, and a stale or
// ownerless disposition fails. The whole inventory (both scopes) reconciles
// against the corpus's per-scope counts separately from the selected quality scope.
func TestQualityCorpus(t *testing.T) {
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

	for _, app := range []struct {
		name string
		kind Kind
	}{{"series", KindSeries}, {"movie", KindMovie}} {
		t.Run(app.name, func(t *testing.T) {
			c := corpora[app.name]
			r := runQualityCorpus(newTestParser(t, time.Now), app.kind, app.name, c, byID)
			for _, problem := range append(r.problems, r.reconcile(scopeCounts(c))...) {
				t.Error(problem)
			}
			t.Logf("quality-corpus app=%s cases=%d quality=%d passed=%d disposed=%d inputs=%d sha256=%s",
				app.name, len(c.Cases), r.cases, r.passed, r.disposed, r.inputs, hashes[app.name])
		})
	}
}
