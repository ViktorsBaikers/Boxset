//go:build !race

package parse

import (
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/parse/corpus"
)

// TestQualityCorpus runs every enabled source case of both committed corpora
// through the real parser. A case passes only when every source assertion
// holds for every input; a retained mismatch needs an exact, answered
// disposition that binds every mismatching input observation, and a stale or
// ownerless disposition fails. The whole inventory (all scopes) reconciles
// against the header separately from the selected quality scope.
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
			for _, problem := range append(r.problems, r.reconcile(c.Header.Counts)...) {
				t.Error(problem)
			}
			counts := c.Header.Counts
			t.Logf("quality-corpus app=%s cases=%d enabled=%d passed=%d disposed=%d disabled=%d out_of_scope=%d inputs=%d sha256=%s",
				app.name, len(c.Cases), counts["enabled"], r.passed, r.disposed, r.disabled, r.outOfScope, r.inputs, hashes[app.name])
		})
	}
}
