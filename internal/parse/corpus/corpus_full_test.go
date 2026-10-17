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
)

const testdataDir = "../../../testdata/parser"

// lockedInventory pins the complete committed inventories: the live source
// commit each corpus was extracted from and its exact per-file case counts
// (independently counted from the source tests: TestCase attributes plus
// TestCaseSource rows). A re-extraction at another commit, or any lost or
// added case, must update this table together with the corpora.
var lockedInventory = map[string]struct {
	commit string
	cases  int
	files  map[string]int
}{
	"series": {"57816c149d02f95543973fefb4780c4495f8121c", 356, map[string]int{
		"tests/QualityParserFixture.cs":       320,
		"tests/ExtendedQualityParserRegex.cs": 30,
		"tests/AnimeVersionFixture.cs":        6,
	}},
	"movie": {"df2faa64c75e6081556d11232dc46523e7264ea1", 323, map[string]int{
		"tests/QualityParserFixture.cs":       287,
		"tests/ExtendedQualityParserRegex.cs": 30,
		"tests/AnimeVersionFixture.cs":        6,
	}},
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
			rawRows += counts["source:testcase_attrs:"+file] + counts["source:source_rows:"+file]
		}
		// The extractor's independent raw attribute/row tally equals the case count.
		if rawRows != want.cases {
			t.Errorf("%s: raw TestCase attributes plus source rows = %d, want %d", app, rawRows, want.cases)
		}
		for _, cs := range c.Cases {
			names, err := cs.Inputs(TimeContext{Location: nil})
			// Quality names carry no symbolic date, so no location is required.
			if err != nil || len(names) == 0 {
				t.Errorf("%s: case %s inputs: %v", app, cs.ID, err)
			}
			if cs.Scope != ScopeQuality || cs.ScopeOwner == "" || len(cs.Expected) < 2 {
				t.Errorf("%s: case %s scope/expectation: %+v", app, cs.ID, cs)
			}
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
