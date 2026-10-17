package corpus

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

const commit = "0123456789abcdef0123456789abcdef01234567"

func testCase(id string, enabled bool, expected map[string]Value) Case {
	if expected == nil {
		expected = map[string]Value{"Quality": {Kind: KindEnum, Type: "Quality", Str: "SDTV"}}
	}
	return Case{
		ID: id, File: "tests/a.cs", Method: "m", Entry: "TestCase#1", Line: 3, UpstreamEnabled: enabled,
		Scope: ScopeQuality, ScopeOwner: "QualityParser.ParseQuality",
		Args:     []Argument{{Kind: KindString, Str: "name"}},
		Expected: expected,
	}
}

func newCorpus(cases ...Case) *Corpus {
	return &Corpus{
		Header: Header{
			Schema: SchemaVersion, App: "series", SourceRepository: "env:BOXSET_REF_SERIES",
			SourceCommit: commit, SourcePaths: []string{"tests/a.cs"}, License: "GPL-3.0", Counts: Tally(cases),
		},
		Cases: cases,
	}
}

func mustMarshal(t *testing.T, c *Corpus) []byte {
	t.Helper()
	data, err := Marshal(c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return data
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var ce *Error
	if !errors.As(err, &ce) || ce.Code != code {
		t.Errorf("error = %v, want corpus error %q", err, code)
	}
}

func TestCorpusRoundTripPreservesEveryValueKind(t *testing.T) {
	all := map[string]Value{
		"s":   {Kind: KindString, Str: "a<b>&\u2028é\"\\"},
		"b":   {Kind: KindBool, Bool: true},
		"i":   {Kind: KindInt, Int: -9007199254740993},
		"d":   {Kind: KindDecimal, Str: "-12.50"},
		"e":   {Kind: KindEnum, Type: "Resolution", Str: "R720p"},
		"a":   {Kind: KindArray, Items: []Value{{Kind: KindNull}, {Kind: KindArray, Items: []Value{}}}},
		"dt":  {Kind: KindDate, Offset: -3, Template: "x.{0:yyyy-MM-dd}.{{y}}"},
		"nil": {Kind: KindNull},
	}
	c := newCorpus(testCase("series:a.cs:m:00:0", true, all), testCase("series:a.cs:m:01:0", false, nil))
	data := mustMarshal(t, c)
	got, err := Read(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got.Cases) != 2 || got.Header.Counts["enabled"] != 1 || got.Header.Counts["disabled"] != 1 || got.Header.Counts["out_of_scope"] != 0 {
		t.Fatalf("counts/cases: %+v", got.Header.Counts)
	}
	for k, want := range all {
		if !Equal(got.Cases[0].Expected[k], want) {
			t.Errorf("value %s changed: %+v != %+v", k, got.Cases[0].Expected[k], want)
		}
	}
	again := mustMarshal(t, got)
	if !bytes.Equal(data, again) {
		t.Error("encoding must be canonical: decode then encode changed the bytes")
	}
	if got.Header.SourceRepository == "" || got.Header.SourceCommit != commit || got.Header.License != "GPL-3.0" {
		t.Errorf("attribution lost: %+v", got.Header)
	}
}

func TestCorpusMarshalEscapesNameLettersLosslessly(t *testing.T) {
	in := Value{Kind: KindString, Str: "Xarr.xARR.arRx.\x1arr.a\"rr"}
	plain, err := in.MarshalJSON()
	if err != nil || string(plain) != `{"t":"string","v":"Xarr.xARR.arRx.\u001arr.a\"rr"}` {
		t.Fatalf("Value.MarshalJSON must stay unescaped so input hashes are unchanged: %s %v", plain, err)
	}
	c := newCorpus(testCase("series:a.cs:m:00:0", true, map[string]Value{
		"s": in, "a": {Kind: KindArray, Items: []Value{in}}, "e": {Kind: KindEnum, Type: "Quality", Str: "Xarr"},
	}))
	c.Cases[0].Args = []Argument{in}
	data := mustMarshal(t, c)
	text := string(data)
	const want = `{"t":"string","v":"X\u0061rr.x\u0041RR.\u0061rRx.\u001arr.a\"rr"}`
	if n := strings.Count(text, want); n != 3 {
		t.Errorf("escaped string Value appears %d times, want 3 (argument, value, array item): %s", n, text)
	}
	if !strings.Contains(text, `"type":"Quality","v":"Xarr"`) {
		t.Errorf("an enum member must not be rewritten: %s", text)
	}
	got, err := Read(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !Equal(got.Cases[0].Args[0], in) || !Equal(got.Cases[0].Expected["s"], in) || !Equal(got.Cases[0].Expected["a"].Items[0], in) {
		t.Error("escaping changed a decoded value")
	}
	if !bytes.Equal(data, mustMarshal(t, got)) {
		t.Error("Marshal of a read corpus must reproduce its bytes")
	}
}

func TestCorpusRejectsMalformedPartialAndUnknown(t *testing.T) {
	valid := mustMarshal(t, newCorpus(testCase("id-1", true, nil), testCase("id-2", true, nil)))
	lines := strings.SplitAfter(string(valid), "\n")
	decimal := string(mustMarshal(t, newCorpus(testCase("id-1", true, map[string]Value{"d": {Kind: KindDecimal, Str: "1.5"}}))))
	tests := []struct {
		name, code string
		data       string
	}{
		{"empty file", "header_missing", ""},
		{"unterminated final record", "partial_record", string(valid[:len(valid)-1])},
		{"missing trailing case", "count_mismatch", lines[0] + lines[1]},
		{"malformed record", "malformed_record", lines[0] + "{not json}\n"},
		{"unknown value tag", "unknown_tag", strings.Replace(string(valid), `"t":"enum"`, `"t":"mystery"`, 1)},
		{"unknown field", "malformed_record", strings.Replace(string(valid), `"line":3`, `"line":3,"extra":1`, 1)},
		{"trailing data", "malformed_record", lines[0] + strings.TrimSuffix(lines[1], "\n") + " {}\n" + lines[2]},
		{"duplicate id", "duplicate_id", lines[0] + lines[1] + lines[1]},
		{"bad commit", "header_invalid", strings.Replace(string(valid), commit, "ABC", 1)},
		{"decimal not exact", "malformed_record", strings.Replace(decimal, `"1.5"`, `"1.5e3"`, 1)},
		{"blank record", "malformed_record", lines[0] + "\n" + lines[1] + lines[2]},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Read(strings.NewReader(tc.data))
			if got != nil {
				t.Errorf("a rejected corpus must yield no usable case, got %d", len(got.Cases))
			}
			wantCode(t, err, tc.code)
		})
	}
}

func TestCorpusRejectsMismatchedCounts(t *testing.T) {
	c := newCorpus(testCase("id-1", true, nil))
	c.Header.Counts["enabled"] = 5
	if _, err := Marshal(c); err == nil {
		t.Fatal("Marshal must reject non-reconciling counts")
	}
	good := mustMarshal(t, newCorpus(testCase("id-1", true, nil)))
	bad := strings.Replace(string(good), `"disabled":0`, `"disabled":1`, 1)
	_, err := Read(strings.NewReader(bad))
	wantCode(t, err, "count_mismatch")
}

// padded returns a corpus with one case per size, each carrying a string
// argument of that many bytes.
func padded(sizes ...int) *Corpus {
	cs := make([]Case, len(sizes))
	for i, n := range sizes {
		cs[i] = testCase("id-"+strconv.Itoa(i), true, nil)
		cs[i].Args = []Argument{{Kind: KindString, Str: strings.Repeat("a", n)}}
	}
	return newCorpus(cs...)
}

func TestCorpusRecordSizeBoundary(t *testing.T) {
	probe := mustMarshal(t, padded(0))
	headerLen := bytes.IndexByte(probe, '\n') + 1
	caseLen := len(probe) - headerLen - 1 // without the newline
	n := MaxRecordBytes - caseLen
	at := mustMarshal(t, padded(n))
	if got, err := Read(bytes.NewReader(at)); err != nil || len(got.Cases) != 1 {
		t.Fatalf("a record of exactly %d bytes must be admitted: %v", MaxRecordBytes, err)
	}
	if _, err := Marshal(padded(n + 1)); err == nil {
		t.Error("Marshal must reject a record above the cap")
	}
	// A hand-built max+1 record is rejected by the reader without truncation.
	over := bytes.Replace(at, []byte(strings.Repeat("a", n)), []byte(strings.Repeat("a", n+1)), 1)
	got, err := Read(bytes.NewReader(over))
	if got != nil {
		t.Error("an oversize record must yield no usable case")
	}
	wantCode(t, err, "record_too_large")
}

func TestCorpusFileSizeBoundary(t *testing.T) {
	// 129 records of about 255 KB each, grown so the file is exactly the cap
	// while every record stays under the record cap.
	sizes := make([]int, 129)
	for i := range sizes {
		sizes[i] = 255000
	}
	extra := MaxFileBytes - len(mustMarshal(t, padded(sizes...)))
	for i := range sizes {
		sizes[i] += extra / len(sizes)
	}
	sizes[len(sizes)-1] += extra % len(sizes)
	at := mustMarshal(t, padded(sizes...))
	if len(at) != MaxFileBytes {
		t.Fatalf("fixture is %d bytes, want exactly %d", len(at), MaxFileBytes)
	}
	got, err := Read(bytes.NewReader(at))
	if err != nil || len(got.Cases) != 129 {
		t.Fatalf("a file of exactly %d bytes must be admitted: %v", MaxFileBytes, err)
	}
	last := sizes[len(sizes)-1]
	sizes[len(sizes)-1]++
	if _, err := Marshal(padded(sizes...)); err == nil {
		t.Error("Marshal must reject a file above the cap")
	}
	// One byte past the cap, spliced into the last record, is rejected without truncation.
	over := bytes.Replace(at, []byte(strings.Repeat("a", last)), []byte(strings.Repeat("a", last+1)), 1)
	if len(over) != MaxFileBytes+1 {
		t.Fatalf("over-cap fixture is %d bytes", len(over))
	}
	got, err = Read(bytes.NewReader(over))
	if got != nil {
		t.Error("an oversize file must yield no usable case")
	}
	wantCode(t, err, "file_too_large")
}

func nestedAround(base Value, depth int) Value {
	for range depth {
		base = Value{Kind: KindArray, Items: []Value{base}}
	}
	return base
}

func nested(depth int) Value { return nestedAround(Value{Kind: KindNull}, depth) }

// recordDepth is the deepest object/array nesting of one record, counted here and not by the reader.
func recordDepth(line string) int {
	depth, deepest, inString, escaped := 0, 0, false, false
	for i := range len(line) {
		switch c := line[i]; {
		case inString:
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
		case c == '"':
			inString = true
		case c == '{' || c == '[':
			depth++
			deepest = max(deepest, depth)
		case c == '}' || c == ']':
			depth--
		}
	}
	return deepest
}

func TestCorpusNestingBoundary(t *testing.T) {
	// Record depth: case object 1, expected object 2, each nested array value is an
	// object plus an array (2 levels each), the innermost null value one object.
	const admitted = (MaxNesting - 3) / 2
	ok := newCorpus(testCase("id-1", true, map[string]Value{"n": nested(admitted)}))
	data := mustMarshal(t, ok)
	if got, err := Read(bytes.NewReader(data)); err != nil || len(got.Cases) != 1 {
		t.Fatalf("nesting %d (depth %d) must be admitted: %v", admitted, 3+2*admitted, err)
	}

	// Marshal rejects what Read would reject, with the record's number.
	deep := newCorpus(testCase("id-1", true, map[string]Value{"n": nested(admitted + 1)}))
	out, err := Marshal(deep)
	if out != nil {
		t.Error("an over-deep record must not be emitted")
	}
	wantCode(t, err, "nesting_too_deep")
	var ce *Error
	if !errors.As(err, &ce) || ce.Record != 2 {
		t.Errorf("Marshal error = %v, want record 2", err)
	}

	// A hand-built record one value level deeper is rejected by the reader.
	over := strings.Replace(string(data), `{"t":"null"}`, `{"t":"array","v":[{"t":"null"}]}`, 1)
	if over == string(data) {
		t.Fatal("the nesting splice found no innermost value")
	}
	got, err := Read(strings.NewReader(over))
	if got != nil {
		t.Error("over-deep record must yield no usable case")
	}
	wantCode(t, err, "nesting_too_deep")
}

func TestCorpusAdmitsRecordDepth32AndRejectsDeeper(t *testing.T) {
	empty := Value{Kind: KindArray, Items: []Value{}}
	// Fixed by hand: the case object and the args or expected container are two levels, a value is
	// one object, and each array level adds an array plus the object of its item. A null at array
	// level 14 is depth 31; an empty array at level 14 is an object and an array, so exactly depth 32.
	for _, where := range []string{"expected", "args"} {
		place := func(v Value) *Corpus {
			c := newCorpus(testCase("id-1", true, nil))
			if where == "expected" {
				c.Cases[0].Expected = map[string]Value{"n": v}
			} else {
				c.Cases[0].Args = []Argument{v}
			}
			return c
		}
		get := func(cs Case) Value {
			if where == "expected" {
				return cs.Expected["n"]
			}
			return cs.Args[0]
		}
		var control string
		for _, tc := range []struct {
			depth int
			v     Value
		}{{31, nested(14)}, {32, nestedAround(empty, 14)}} {
			data := mustMarshal(t, place(tc.v))
			line := strings.Split(string(data), "\n")[1]
			if got := recordDepth(line); got != tc.depth {
				t.Fatalf("%s: fixture record depth = %d, want %d", where, got, tc.depth)
			}
			back, err := Read(bytes.NewReader(data))
			if err != nil || !Equal(get(back.Cases[0]), tc.v) {
				t.Errorf("%s: depth %d must be admitted with its exact value: %v", where, tc.depth, err)
			}
			if !bytes.Equal(data, mustMarshal(t, back)) {
				t.Errorf("%s: depth %d must re-encode to the same bytes", where, tc.depth)
			}
			control = string(data)
		}

		// Depth 33 (a null one level below the empty array) and 34 (an empty array one level deeper).
		for depth, v := range map[int]Value{33: nested(15), 34: nestedAround(empty, 15)} {
			out, err := Marshal(place(v))
			if out != nil {
				t.Errorf("%s: depth %d must not be emitted", where, depth)
			}
			wantCode(t, err, "nesting_too_deep")
			var ce *Error
			if !errors.As(err, &ce) || ce.Record != 2 {
				t.Errorf("%s: depth %d error = %v, want record 2", where, depth, err)
			}
		}
		over := strings.Replace(control, `"v":[]`, `"v":[{"t":"null"}]`, 1)
		if over == control || recordDepth(strings.Split(over, "\n")[1]) != 33 {
			t.Fatalf("%s: the depth 33 splice is not depth 33", where)
		}
		got, err := Read(strings.NewReader(over))
		if got != nil {
			t.Errorf("%s: a depth 33 record must yield no usable case", where)
		}
		wantCode(t, err, "nesting_too_deep")
		var ce *Error
		if !errors.As(err, &ce) || ce.Record != 2 {
			t.Errorf("%s: Read error = %v, want record 2", where, err)
		}
	}
}

func TestCorpusDispositionsRejectStaleAndOwnerless(t *testing.T) {
	c := newCorpus(testCase("id-1", true, map[string]Value{"Quality": {Kind: KindEnum, Type: "Quality", Str: "SDTV"}}), testCase("id-2", false, nil))
	valid := Disposition{
		CaseID:    "id-1",
		Expected:  map[string]Value{"Quality": {Kind: KindEnum, Type: "Quality", Str: "SDTV"}},
		Observed:  map[string]Value{"Quality": {Kind: KindEnum, Type: "Quality", Str: "DVD"}},
		Rationale: "r", Question: "q", Answer: "a", AnsweredBy: "owner",
	}
	if err := (&Dispositions{Schema: 1, Dispositions: []Disposition{valid}}).Validate(c); err != nil {
		t.Fatalf("valid disposition rejected: %v", err)
	}
	mutate := map[string]func(d *Disposition){
		"unknown case":  func(d *Disposition) { d.CaseID = "nope" },
		"disabled case": func(d *Disposition) { d.CaseID = "id-2" },
		"ownerless":     func(d *Disposition) { d.AnsweredBy = "" },
		"unanswered":    func(d *Disposition) { d.Answer = "" },
		"stale expectation": func(d *Disposition) {
			d.Expected = map[string]Value{"Quality": {Kind: KindEnum, Type: "Quality", Str: "DVD"}}
		},
		"extra key": func(d *Disposition) {
			d.Expected = map[string]Value{"Other": {Kind: KindNull}}
			d.Observed = d.Expected
		},
		"key mismatch": func(d *Disposition) { d.Observed = map[string]Value{"Other": {Kind: KindNull}} },
	}
	for name, m := range mutate {
		d := valid
		m(&d)
		if err := (&Dispositions{Schema: 1, Dispositions: []Disposition{d}}).Validate(c); err == nil {
			t.Errorf("%s must be rejected", name)
		}
	}
	dup := &Dispositions{Schema: 1, Dispositions: []Disposition{valid, valid}}
	if dup.Validate(c) == nil {
		t.Error("duplicate dispositions must be rejected")
	}
	if _, err := ReadDispositions(strings.NewReader(`{"schema":1,"dispositions":[],"extra":1}`)); err == nil {
		t.Error("unknown disposition fields must be rejected")
	}
	if d, err := ReadDispositions(strings.NewReader(`{"schema":1,"dispositions":[]}`)); err != nil || len(d.Dispositions) != 0 {
		t.Errorf("empty dispositions: %v", err)
	}
}

func TestMaterializeUsesLocalCalendarDays(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	v := Value{Kind: KindDate, Offset: 14, Template: "n.{0:yyyy.MM.dd}"}
	now := time.Date(2026, 10, 25, 0, 30, 0, 0, ny)
	got, err := TimeContext{Now: now, Location: ny}.Materialize(v)
	if err != nil || got != "n.2026.11.08" {
		t.Errorf("calendar +14 = %q (%v), want n.2026.11.08", got, err)
	}
	// An independent computation of the 24-hour-duration mutant differs.
	if mutant := now.Add(14 * 24 * time.Hour).Format("2006.01.02"); mutant != "2026.11.07" {
		t.Fatalf("control: duration mutant = %s", mutant)
	}
	// 22:30 in New York is already the next day in UTC: the local calendar decides.
	late := time.Date(2026, 10, 25, 22, 30, 0, 0, ny)
	local, errL := TimeContext{Now: late, Location: ny}.Materialize(v)
	utc, errU := TimeContext{Now: late, Location: time.UTC}.Materialize(v)
	if errL != nil || errU != nil || local != "n.2026.11.08" || utc != "n.2026.11.09" {
		t.Errorf("local=%q (%v) utc=%q (%v)", local, errL, utc, errU)
	}
	for _, bad := range []string{"x.{0}", "x.{1:yyyy}", "x.{0:HH}", "x.}"} {
		if _, err := (TimeContext{Now: now, Location: ny}).Materialize(Value{Kind: KindDate, Template: bad}); err == nil {
			t.Errorf("template %q must be rejected", bad)
		}
	}
	if _, err := (TimeContext{Now: now}).Materialize(v); err == nil {
		t.Error("a missing Location must be an error")
	}
}

// rawJSONL encodes c as the record stream Marshal would write but without validating c, so a
// corpus broken in exactly one field can be fed to Read.
func rawJSONL(t *testing.T, c *Corpus) string {
	t.Helper()
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(c.Header); err != nil {
		t.Fatal(err)
	}
	for _, cs := range c.Cases {
		if err := enc.Encode(cs); err != nil {
			t.Fatal(err)
		}
	}
	return sb.String()
}

func TestCorpusProvenanceIsValidatedByReadAndMarshal(t *testing.T) {
	type side struct{ app, otherApp, locator, other string }
	sides := []side{
		{"series", "movie", "env:BOXSET_REF_SERIES", "env:BOXSET_REF_MOVIE"},
		{"movie", "series", "env:BOXSET_REF_MOVIE", "env:BOXSET_REF_SERIES"},
	}
	// base is a fully valid corpus of one side; every rejection below breaks exactly one of its fields.
	base := func(s side) *Corpus {
		c := newCorpus(testCase("id-1", true, nil))
		c.Header.App, c.Header.SourceRepository = s.app, s.locator
		return c
	}
	// rejects asserts that Marshal of c gives no output and Read of the same records gives no corpus, both with code.
	rejects := func(t *testing.T, c *Corpus, code string) {
		t.Helper()
		out, err := Marshal(c)
		if out != nil {
			t.Error("Marshal must emit nothing for a corpus the reader rejects")
		}
		wantCode(t, err, code)
		got, err := Read(strings.NewReader(rawJSONL(t, c)))
		if got != nil {
			t.Error("a rejected corpus must yield no usable case")
		}
		wantCode(t, err, code)
	}
	badPaths := []string{"src/a.cs", "a.cs", "/tests/a.cs", "tests/../a.cs", "tests//a.cs", "tests/./a.cs", "tests/", "TESTS/a.cs", "tests", ""}

	for _, s := range sides {
		for _, tc := range []struct {
			name   string
			mutate func(c *Corpus)
		}{
			{"locator of the other side", func(c *Corpus) { c.Header.SourceRepository = s.other }},
			{"foreign locator", func(c *Corpus) { c.Header.SourceRepository = "env:OTHER" }},
			{"locator lower case", func(c *Corpus) { c.Header.SourceRepository = strings.ToLower(s.locator) }},
			{"locator without its prefix", func(c *Corpus) { c.Header.SourceRepository = strings.TrimPrefix(s.locator, "env:") }},
			{"app of the other side", func(c *Corpus) { c.Header.App = s.otherApp }},
			{"unknown app", func(c *Corpus) { c.Header.App = "anime" }},
			{"licence of another version", func(c *Corpus) { c.Header.License = "GPL-3.0-or-later" }},
			{"licence lower case", func(c *Corpus) { c.Header.License = "gpl-3.0" }},
			{"another licence", func(c *Corpus) { c.Header.License = "MIT" }},
			{"no licence", func(c *Corpus) { c.Header.License = "" }},
			{"no source paths", func(c *Corpus) { c.Header.SourcePaths = nil }},
			{"duplicate source path", func(c *Corpus) { c.Header.SourcePaths = []string{"tests/a.cs", "tests/a.cs"} }},
			{"unnamed source count", func(c *Corpus) { c.Header.Counts["source::tests/a.cs"] = 1 }},
			{"negative source count", func(c *Corpus) { c.Header.Counts["source:rows:tests/a.cs"] = -1 }},
			{"count key of another kind", func(c *Corpus) { c.Header.Counts["rows:tests/a.cs"] = 1 }},
			{"count key of a file with no case", func(c *Corpus) { c.Header.Counts["cases:tests/other.cs"] = 1 }},
		} {
			t.Run(s.app+" "+tc.name, func(t *testing.T) {
				c := base(s)
				tc.mutate(c)
				rejects(t, c, "header_invalid")
			})
		}
		for _, p := range badPaths {
			t.Run(s.app+" header path "+strconv.Quote(p), func(t *testing.T) {
				c := base(s)
				c.Header.SourcePaths = []string{p}
				rejects(t, c, "header_invalid")
			})
			t.Run(s.app+" count key path "+strconv.Quote(p), func(t *testing.T) {
				c := base(s)
				c.Header.Counts["source:rows:"+p] = 1
				rejects(t, c, "header_invalid")
			})
		}
		for _, p := range append([]string{"tests/b.cs"}, badPaths...) {
			t.Run(s.app+" case file "+strconv.Quote(p), func(t *testing.T) {
				c := base(s)
				c.Cases[0].File = p
				rejects(t, c, "case_invalid")
			})
		}

		// Controls: every area, a nested file and a recorded source count, each on its own side.
		for _, p := range []string{"tests/a.cs", "parser/a.cs", "qualities/a.cs", "tests/sub/dir/a.cs"} {
			t.Run(s.app+" control "+p, func(t *testing.T) {
				c := base(s)
				c.Cases[0].File = p
				c.Header.SourcePaths = []string{p}
				c.Header.Counts = Tally(c.Cases)
				c.Header.Counts["source:rows:"+p] = 3
				data := mustMarshal(t, c)
				got, err := Read(bytes.NewReader(data))
				if err != nil || got.Header.App != s.app || got.Header.SourceRepository != s.locator || got.Header.Counts["source:rows:"+p] != 3 || got.Cases[0].File != p {
					t.Errorf("Read of the %s side: %+v, %v", s.app, got, err)
				}
			})
		}
	}
}

func TestCorpusReadAndMarshalAgreeOnContainedValues(t *testing.T) {
	const tmpl = "x.{0:yyyy.MM.dd}"
	date := func(off int, tm string) (Value, string) {
		return Value{Kind: KindDate, Offset: off, Template: tm}, `{"t":"date","offset":` + strconv.Itoa(off) + `,"template":"` + tm + `"}`
	}
	decimal := func(s string) (Value, string) {
		return Value{Kind: KindDecimal, Str: s}, `{"t":"decimal","v":"` + s + `"}`
	}
	enum := func(ty, m string) (Value, string) {
		wire := `{"t":"enum","type":"` + ty + `","v":"` + m + `"}`
		if ty == "" {
			wire = `{"t":"enum","v":"` + m + `"}`
		}
		return Value{Kind: KindEnum, Type: ty, Str: m}, wire
	}
	type vc struct {
		name string
		v    Value
		wire string
		ok   bool
	}
	var cases []vc
	add := func(name string, ok bool, v Value, wire string) { cases = append(cases, vc{name, v, wire, ok}) }
	for _, d := range []struct {
		name string
		off  int
		tm   string
		ok   bool
	}{
		{"date valid", 0, tmpl, true},
		{"date offset at the limit", 36500, tmpl, true},
		{"date offset at the negative limit", -36500, tmpl, true},
		{"date quoted literal", 1, "x.{0:yyyy'-'MM}", true},
		{"date offset over the limit", 36501, tmpl, false},
		{"date offset under the limit", -36501, tmpl, false},
		{"date unsupported specifier", 0, "x.{0:HH}", false},
		{"date bare placeholder", 0, "x.{0}", false},
		{"date other placeholder", 0, "x.{1:yyyy}", false},
		{"date unbalanced brace", 0, "x.}", false},
		{"date unterminated literal", 0, "x.{0:'yyyy}", false},
		{"date empty template", 0, "", false},
	} {
		v, w := date(d.off, d.tm)
		add(d.name, d.ok, v, w)
	}
	for _, d := range []struct {
		name, s string
		ok      bool
	}{
		{"decimal valid", "-12.50", true}, {"decimal whole", "7", true},
		{"decimal exponent", "1.5e3", false}, {"decimal empty", "", false}, {"decimal sign only", "-", false},
		{"decimal no fraction digits", "1.", false}, {"decimal no whole digits", ".5", false},
		{"decimal space", " 1", false}, {"decimal plus", "+1", false},
	} {
		v, w := decimal(d.s)
		add(d.name, d.ok, v, w)
	}
	for _, d := range []struct {
		name, ty, m string
		ok          bool
	}{
		{"enum valid", "Quality", "SDTV", true}, {"enum underscore", "_T1", "R_2", true},
		{"enum no type", "", "SDTV", false}, {"enum digit-leading type", "1Q", "SDTV", false},
		{"enum dashed member", "Quality", "SD-TV", false}, {"enum no member", "Quality", "", false},
		{"enum dotted type", "A.B", "SDTV", false},
	} {
		v, w := enum(d.ty, d.m)
		add(d.name, d.ok, v, w)
	}

	const placeholder = `{"t":"string","v":"PLACEHOLDER"}`
	arr := func(v Value) Value {
		return Value{Kind: KindArray, Items: []Value{{Kind: KindArray, Items: []Value{v}}}}
	}
	arrWire := func(w string) string { return `{"t":"array","v":[{"t":"array","v":[` + w + `]}]}` }
	for _, loc := range []struct {
		name  string
		args  bool
		inArr bool
	}{{"args", true, false}, {"expected", false, false}, {"nested in args", true, true}, {"nested in expected", false, true}} {
		place := func(v Value) *Corpus {
			c := newCorpus(testCase("id-1", true, nil))
			if loc.args {
				c.Cases[0].Args = []Argument{v}
			} else {
				c.Cases[0].Expected = map[string]Value{"x": v}
			}
			return c
		}
		got := func(cs Case) Value {
			if loc.args {
				return cs.Args[0]
			}
			return cs.Expected["x"]
		}
		holder := string(mustMarshal(t, place(Value{Kind: KindString, Str: "PLACEHOLDER"})))
		for _, tc := range cases {
			t.Run(loc.name+" "+tc.name, func(t *testing.T) {
				v, wire := tc.v, tc.wire
				if loc.inArr {
					v, wire = arr(v), arrWire(wire)
				}
				raw := strings.Replace(holder, placeholder, wire, 1)
				if raw == holder {
					t.Fatal("the wire splice found no placeholder")
				}
				data, err := Marshal(place(v))
				back, rerr := Read(strings.NewReader(raw))
				if !tc.ok {
					if data != nil || back != nil {
						t.Errorf("a malformed value must be rejected by both: Marshal output %v, Read corpus %v", data != nil, back != nil)
					}
					wantCode(t, err, "malformed_record")
					wantCode(t, rerr, "malformed_record")
					var ce *Error
					if !errors.As(err, &ce) || ce.Record != 2 || !errors.As(rerr, &ce) || ce.Record != 2 {
						t.Errorf("both must reject at record 2: %v, %v", err, rerr)
					}
					return
				}
				if err != nil || rerr != nil {
					t.Fatalf("a valid value must be accepted by both: Marshal %v, Read %v", err, rerr)
				}
				if string(data) != raw {
					t.Errorf("Marshal wrote %q, the hand-written record is %q", data, raw)
				}
				if !Equal(got(back.Cases[0]), v) {
					t.Errorf("Read changed the value: %+v != %+v", got(back.Cases[0]), v)
				}
			})
		}
	}
}
