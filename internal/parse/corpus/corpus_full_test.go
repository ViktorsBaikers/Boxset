//go:build !race

package corpus

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testdataDir = "../../../testdata/parser"

// lockedFixtures pins the complete committed fixtures: the exact SHA-256 of
// each file, its per-scope counts and the per-owner counts of every scope
// (quality: QualityParser.ParseQuality; title: the series and movie title
// parsers and the release group and language parsers they run). A hand-added,
// changed or lost case must update this table together with the fixtures.
var lockedFixtures = map[string]struct {
	sha256 string
	scopes map[string]int // scope -> cases
	owners map[string]int // scope|owner -> cases
}{
	"series": {"4d9a0e28b819e75b0cf76f87cdc681dc40aea3cf8a7e0c101d4c2736967a7c27", map[string]int{
		ScopeQuality: 356, ScopeTitle: 1166,
	}, map[string]int{
		ScopeQuality + "|QualityParser.ParseQuality":         356,
		ScopeTitle + "|Parser.ParseTitle":                    754,
		ScopeTitle + "|Parser.ParseSeriesName":               33,
		ScopeTitle + "|Parser.CleanSeriesTitle":              44,
		ScopeTitle + "|ReleaseGroupParser.ParseReleaseGroup": 166,
		ScopeTitle + "|LanguageParser.ParseLanguages":        169,
	}},
	"movie": {"ee357e89075be3cf5954382531265ede1466bc38dc16b29cc2188d6ce5f79aef", map[string]int{
		ScopeQuality: 323, ScopeTitle: 513,
	}, map[string]int{
		ScopeQuality + "|QualityParser.ParseQuality":         323,
		ScopeTitle + "|Parser.ParseMovieTitle":               321,
		ScopeTitle + "|ReleaseGroupParser.ParseReleaseGroup": 161,
		ScopeTitle + "|LanguageParser.ParseLanguages":        30,
		ScopeTitle + "|Parser.CleanMovieTitle":               1,
	}},
}

// TestCorpusFull reads the committed fixtures through the canonical reader and
// reconciles their complete inventories, canonical bytes and dispositions.
func TestCorpusFull(t *testing.T) {
	for _, app := range []string{"series", "movie"} {
		want := lockedFixtures[app]
		data, err := os.ReadFile(filepath.Join(testdataDir, app+".jsonl"))
		if err != nil {
			t.Fatalf("%s: %v", app, err)
		}
		if len(data) > MaxFileBytes {
			t.Fatalf("%s: %d bytes exceed the %d byte reader cap", app, len(data), MaxFileBytes)
		}
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != want.sha256 {
			t.Errorf("%s: sha256 %s, locked %s", app, got, want.sha256)
		}
		c, err := Read(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: %v", app, err)
		}
		if h := c.Header; h.Schema != 2 || h.App != app {
			t.Errorf("%s: header %+v, want schema 2 and app %s", app, h, app)
		}
		perScope, perOwner := map[string]int{}, map[string]int{}
		for i, cs := range c.Cases {
			perScope[cs.Scope]++
			perOwner[cs.Scope+"|"+cs.ScopeOwner]++
			if cs.ID != fmt.Sprintf("%s:%04d", app, i+1) {
				t.Errorf("%s: case %d has id %s, want the 1-based ordinal", app, i+1, cs.ID)
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
		t.Logf("corpus-full app=%s cases=%d quality=%d title=%d bytes=%d sha256=%s",
			app, len(c.Cases), perScope[ScopeQuality], perScope[ScopeTitle], len(data), hex.EncodeToString(sum[:]))
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
	if d.Schema != DispositionsVersion || len(d.Dispositions) != 0 {
		t.Errorf("dispositions: version %d with %d records, want version %d and none", d.Schema, len(d.Dispositions), DispositionsVersion)
	}
}
