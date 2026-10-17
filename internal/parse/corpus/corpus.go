// Package corpus owns the canonical attributed source-corpus schema: the
// tagged Value model, the bounded streaming JSONL reader and writer, the
// per-file count reconciliation, the disposition records and the shared
// local-calendar materializer. Corpus files are derived from GPL-3.0 test
// sources of the two reference checkouts; every file carries the neutral
// provenance (side, locator, commit, source paths and licence) in its header
// record.
//
// The extractor (internal/parse/corpus/extract) is the only provider; the
// native parser tests and benchmark are consumers that decode exclusively
// through Read.
package corpus

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Schema limits. A corpus file is rejected, never truncated, beyond them.
const (
	SchemaVersion  = 1
	MaxFileBytes   = 32 << 20
	MaxRecordBytes = 256 << 10
	MaxNesting     = 32

	// License is the exact licence header of every corpus: the reference
	// sources are GPL-3.0 and so are the files derived from them.
	License = "GPL-3.0"

	// InputKey is the reserved Expected key that holds the release names (as
	// string or symbolic date values) passed to the scoped entry point, in call
	// order. Every other Expected key is the exact source property path that
	// the source test asserts on the entry point's result.
	InputKey = "$input"

	ScopeQuality    = "quality"
	ScopeTitle      = "title"
	ScopeOutOfScope = "out_of_scope"

	maxDispositionBytes = 1 << 20
	maxDateOffset       = 36500
)

// Value kinds (the wire tag "t").
const (
	KindString  = "string"
	KindBool    = "bool"
	KindInt     = "int"
	KindDecimal = "decimal"
	KindEnum    = "enum"
	KindArray   = "array"
	KindNull    = "null"
	KindDate    = "date"
)

// Error is the only error type Read and Marshal return for a rejected file.
// Code is a fixed token; Record is the 1-based record number (0 for the file).
type Error struct {
	Code   string
	Record int
}

func (e *Error) Error() string {
	return fmt.Sprintf("corpus: %s record=%d", e.Code, e.Record)
}

var errUnknownTag = errors.New("corpus: unknown value tag")

// Value is one tagged argument or expected value.
type Value struct {
	Kind     string
	Str      string  // string text, exact decimal text or enum member
	Type     string  // enum type name
	Int      int64   // int
	Bool     bool    // bool
	Items    []Value // array
	Offset   int     // date: calendar days after the TimeContext day
	Template string  // date: exact source format template, e.g. "A.{0:yyyy.MM.dd}"
}

// Argument is a source test argument.
type Argument = Value

type wireValue struct {
	T        string          `json:"t"`
	Type     string          `json:"type,omitempty"`
	V        json.RawMessage `json:"v,omitempty"`
	Offset   *int            `json:"offset,omitempty"`
	Template string          `json:"template,omitempty"`
}

func marshalPlain(x any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(x); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// MarshalJSON writes the tagged form.
func (v Value) MarshalJSON() ([]byte, error) { return v.marshal(false) }

// escValue is a Value that Marshal writes with name-letter escapes. It is a
// separate type so Value.MarshalJSON, and with it every input hash that is
// derived from it, stays byte-for-byte unchanged.
type escValue Value

func (v escValue) MarshalJSON() ([]byte, error) { return Value(v).marshal(true) }

// escapeNameLetters rewrites an encoded JSON string literal so that every a/A
// immediately preceding a case-insensitive "rr" is the escape \u0061/\u0041.
// Existing escape sequences are copied whole; the decoded string is unchanged.
func escapeNameLetters(s []byte) []byte {
	out := make([]byte, 0, len(s)+16)
	isR := func(c byte) bool { return c == 'r' || c == 'R' }
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\\':
			n := 2
			if i+1 < len(s) && s[i+1] == 'u' {
				n = 6
			}
			end := min(i+n, len(s))
			out = append(out, s[i:end]...)
			i = end - 1
		case c == 'a' && i+2 < len(s) && isR(s[i+1]) && isR(s[i+2]):
			out = append(out, `\u0061`...)
		case c == 'A' && i+2 < len(s) && isR(s[i+1]) && isR(s[i+2]):
			out = append(out, `\u0041`...)
		default:
			out = append(out, c)
		}
	}
	return out
}

// validContent rejects a Value whose own content the reader would reject: a
// malformed decimal, an enum with a non-identifier type or member, a date with
// an empty or unsupported template or an offset out of range. Read and Marshal
// both route every Value through it; an array is checked item by item as each
// item is decoded or encoded.
func (v Value) validContent() error {
	switch v.Kind {
	case KindDecimal:
		if !validDecimal(v.Str) {
			return errors.New("corpus: invalid decimal")
		}
	case KindEnum:
		if !isIdent(v.Type) || !isIdent(v.Str) {
			return errors.New("corpus: invalid enum")
		}
	case KindDate:
		if v.Template == "" || v.Offset < -maxDateOffset || v.Offset > maxDateOffset {
			return errors.New("corpus: invalid date")
		}
		_, err := formatTemplate(v.Template, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
		return err
	}
	return nil
}

func (v Value) marshal(escape bool) ([]byte, error) {
	if err := v.validContent(); err != nil {
		return nil, err
	}
	w := wireValue{T: v.Kind}
	var err error
	switch v.Kind {
	case KindString, KindDecimal:
		w.V, err = marshalPlain(v.Str)
		if escape && v.Kind == KindString && err == nil {
			w.V = escapeNameLetters(w.V)
		}
	case KindBool:
		w.V, err = marshalPlain(v.Bool)
	case KindInt:
		w.V = []byte(strconv.FormatInt(v.Int, 10))
	case KindEnum:
		w.Type = v.Type
		w.V, err = marshalPlain(v.Str)
	case KindArray:
		if escape {
			items := make([]escValue, len(v.Items))
			for i, it := range v.Items {
				items[i] = escValue(it)
			}
			w.V, err = marshalPlain(items)
			break
		}
		items := v.Items
		if items == nil {
			items = []Value{}
		}
		w.V, err = marshalPlain(items)
	case KindNull:
	case KindDate:
		off := v.Offset
		w.Offset = &off
		w.Template = v.Template
	default:
		return nil, errUnknownTag
	}
	if err != nil {
		return nil, err
	}
	return marshalPlain(w)
}

// UnmarshalJSON strictly decodes the tagged form; an unknown tag, an unknown
// field or a field that does not belong to the tag is an error.
func (v *Value) UnmarshalJSON(b []byte) error {
	var w wireValue
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return err
	}
	out := Value{Kind: w.T}
	var err error
	switch w.T {
	case KindString:
		err = onlyV(w)
		if err == nil {
			err = decodeStrict(w.V, &out.Str)
		}
	case KindDecimal:
		err = onlyV(w)
		if err == nil {
			err = decodeStrict(w.V, &out.Str)
		}
	case KindBool:
		err = onlyV(w)
		if err == nil {
			err = decodeStrict(w.V, &out.Bool)
		}
	case KindInt:
		err = onlyV(w)
		if err == nil {
			out.Int, err = strconv.ParseInt(string(w.V), 10, 64)
		}
	case KindEnum:
		if len(w.V) == 0 || w.Offset != nil || w.Template != "" {
			err = errors.New("corpus: invalid enum")
			break
		}
		out.Type = w.Type
		err = decodeStrict(w.V, &out.Str)
	case KindArray:
		if w.Type != "" || w.Offset != nil || w.Template != "" || len(w.V) == 0 || w.V[0] != '[' {
			err = errors.New("corpus: invalid array")
			break
		}
		err = json.Unmarshal(w.V, &out.Items)
		if out.Items == nil {
			out.Items = []Value{}
		}
	case KindNull:
		if w.Type != "" || len(w.V) != 0 || w.Offset != nil || w.Template != "" {
			err = errors.New("corpus: invalid null")
		}
	case KindDate:
		if w.Type != "" || len(w.V) != 0 || w.Offset == nil || w.Template == "" {
			err = errors.New("corpus: invalid date")
			break
		}
		out.Offset = *w.Offset
		out.Template = w.Template
	default:
		return errUnknownTag
	}
	if err == nil {
		err = out.validContent()
	}
	if err != nil {
		return err
	}
	*v = out
	return nil
}

func onlyV(w wireValue) error {
	if w.Type != "" || len(w.V) == 0 || w.Offset != nil || w.Template != "" {
		return errors.New("corpus: invalid value fields")
	}
	return nil
}

func decodeStrict(raw json.RawMessage, into any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return errors.New("corpus: null value")
	}
	return json.Unmarshal(raw, into)
}

func validDecimal(s string) bool {
	s = strings.TrimPrefix(s, "-")
	whole, frac, hasFrac := strings.Cut(s, ".")
	digits := func(t string) bool {
		if t == "" {
			return false
		}
		for i := range len(t) {
			if t[i] < '0' || t[i] > '9' {
				return false
			}
		}
		return true
	}
	return digits(whole) && (!hasFrac || digits(frac))
}

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		c := s[i]
		ok := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && c >= '0' && c <= '9')
		if !ok {
			return false
		}
	}
	return true
}

// Header is the first record of every corpus file.
type Header struct {
	Schema           int            `json:"schema"`
	App              string         `json:"app"`
	SourceRepository string         `json:"sourceRepository"`
	SourceCommit     string         `json:"sourceCommit"`
	SourcePaths      []string       `json:"sourcePaths"`
	License          string         `json:"license"`
	Counts           map[string]int `json:"counts"`
}

// Case is one retained source test case.
type Case struct {
	ID              string           `json:"id"`
	File            string           `json:"file"`
	Method          string           `json:"method"`
	Entry           string           `json:"entry"`
	Line            int              `json:"line"`
	UpstreamEnabled bool             `json:"upstreamEnabled"`
	Scope           string           `json:"scope"`
	ScopeOwner      string           `json:"scopeOwner"`
	Args            []Argument       `json:"args"`
	Expected        map[string]Value `json:"expected"`
}

// Corpus is a fully validated corpus file.
type Corpus struct {
	Header Header
	Cases  []Case
}

// CountKey names a reconciliation count: kind is cases, enabled, disabled or
// out_of_scope; an empty file names the corpus total.
func CountKey(kind, file string) string {
	if file == "" {
		return kind
	}
	return kind + ":" + file
}

var countKinds = [...]string{"cases", "enabled", "disabled", "out_of_scope"}

// Tally returns the canonical count map for cases: the four totals plus the
// four per-file counts of every file that carries at least one case. Enabled
// and disabled count in-scope cases by upstream status; out_of_scope counts
// every case whose scope is out_of_scope, whatever its upstream status.
func Tally(cases []Case) map[string]int {
	t := map[string]int{}
	for _, k := range countKinds {
		t[CountKey(k, "")] = 0
	}
	for _, c := range cases {
		kind := "enabled"
		switch {
		case c.Scope == ScopeOutOfScope:
			kind = "out_of_scope"
		case !c.UpstreamEnabled:
			kind = "disabled"
		}
		for _, k := range countKinds {
			if _, ok := t[CountKey(k, c.File)]; !ok {
				t[CountKey(k, c.File)] = 0
			}
		}
		for _, f := range [2]string{"", c.File} {
			t[CountKey("cases", f)]++
			t[CountKey(kind, f)]++
		}
	}
	return t
}

// Validate checks the schema invariants of a whole corpus.
func (c *Corpus) Validate() error {
	h := c.Header
	switch {
	case h.Schema != SchemaVersion:
		return &Error{"header_invalid", 1}
	case h.App != "series" && h.App != "movie":
		return &Error{"header_invalid", 1}
	case h.SourceRepository != "env:BOXSET_REF_"+strings.ToUpper(h.App) || h.License != License || len(h.SourcePaths) == 0 || h.Counts == nil:
		return &Error{"header_invalid", 1}
	case !validCommit(h.SourceCommit):
		return &Error{"header_invalid", 1}
	}
	paths := map[string]bool{}
	for _, p := range h.SourcePaths {
		if !canonicalPath(p) || paths[p] {
			return &Error{"header_invalid", 1}
		}
		paths[p] = true
	}
	ids := map[string]bool{}
	for i, cs := range c.Cases {
		rec := i + 2
		switch {
		case cs.ID == "" || cs.Method == "" || cs.Entry == "" || cs.Line < 1 || cs.ScopeOwner == "":
			return &Error{"case_invalid", rec}
		case !paths[cs.File]:
			return &Error{"case_invalid", rec}
		case cs.Scope != ScopeQuality && cs.Scope != ScopeTitle && cs.Scope != ScopeOutOfScope:
			return &Error{"case_invalid", rec}
		case cs.Args == nil || cs.Expected == nil:
			return &Error{"case_invalid", rec}
		}
		if ids[cs.ID] {
			return &Error{"duplicate_id", rec}
		}
		ids[cs.ID] = true
	}
	want := Tally(c.Cases)
	for k, v := range want {
		if got, ok := h.Counts[k]; !ok || got != v {
			return &Error{"count_mismatch", 1}
		}
	}
	for k, v := range h.Counts {
		if _, ok := want[k]; ok {
			continue
		}
		// Any other key is a recorded source count, source:<name>:<area>/<file>.
		parts := strings.SplitN(k, ":", 3)
		if len(parts) != 3 || parts[0] != "source" || parts[1] == "" || v < 0 || !canonicalPath(parts[2]) {
			return &Error{"header_invalid", 1}
		}
	}
	return nil
}

// canonicalPath reports whether p is <area>/<file> under a known source area,
// with non-empty components other than . and ..
func canonicalPath(p string) bool {
	area, rest, ok := strings.Cut(p, "/")
	if !ok || (area != "tests" && area != "parser" && area != "qualities") {
		return false
	}
	for comp := range strings.SplitSeq(rest, "/") {
		if comp == "" || comp == "." || comp == ".." {
			return false
		}
	}
	return true
}

func validCommit(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// escCase is a Case whose argument and expected Values marshal as escValue.
// Its shallower args and expected fields replace the embedded Case's, keeping
// the record's field order.
type escCase struct {
	Case
	Args     []escValue          `json:"args"`
	Expected map[string]escValue `json:"expected"`
}

// Marshal validates c and returns its canonical JSONL bytes (one record per
// line, every line LF-terminated, map keys sorted, tagged string Values written
// with escapeNameLetters). It rejects a record above MaxRecordBytes or nested
// deeper than MaxNesting, or a file above MaxFileBytes; it never truncates.
func Marshal(c *Corpus) ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	emit := func(rec int, x any) error {
		start := buf.Len()
		if err := enc.Encode(x); err != nil {
			return &Error{"malformed_record", rec}
		}
		if buf.Len()-start-1 > MaxRecordBytes {
			return &Error{"record_too_large", rec}
		}
		if err := checkNesting(buf.Bytes()[start:buf.Len()-1], rec); err != nil {
			return err
		}
		if buf.Len() > MaxFileBytes {
			return &Error{"file_too_large", 0}
		}
		return nil
	}
	if err := emit(1, c.Header); err != nil {
		return nil, err
	}
	for i := range c.Cases {
		cs := c.Cases[i]
		rec := escCase{Case: cs, Args: make([]escValue, len(cs.Args)), Expected: make(map[string]escValue, len(cs.Expected))}
		for j, a := range cs.Args {
			rec.Args[j] = escValue(a)
		}
		for k, v := range cs.Expected {
			rec.Expected[k] = escValue(v)
		}
		if err := emit(i+2, rec); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// ReadFile opens path and decodes it with Read.
func ReadFile(path string) (*Corpus, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return Read(f)
}

// Read decodes a corpus: at most MaxFileBytes, at most MaxRecordBytes per
// record, nesting at most MaxNesting, streaming. A file that is oversize,
// partial (an unterminated record or counts that do not reconcile),
// malformed or carries an unknown tag yields (nil, *Error): no usable case.
func Read(r io.Reader) (*Corpus, error) {
	lr := &io.LimitedReader{R: r, N: MaxFileBytes + 1}
	br := bufio.NewReaderSize(lr, 64<<10)
	var c Corpus
	total := 0
	for rec := 1; ; rec++ {
		line, err := readRecord(br, lr, rec)
		if errors.Is(err, io.EOF) {
			if rec == 1 {
				return nil, &Error{"header_missing", 1}
			}
			break
		}
		if err != nil {
			return nil, err
		}
		total += len(line) + 1
		if total > MaxFileBytes {
			return nil, &Error{"file_too_large", 0}
		}
		if err := checkNesting(line, rec); err != nil {
			return nil, err
		}
		if rec == 1 {
			err = decodeStrictRecord(line, &c.Header, rec)
		} else {
			var cs Case
			err = decodeStrictRecord(line, &cs, rec)
			if err == nil {
				c.Cases = append(c.Cases, cs)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func readRecord(br *bufio.Reader, lr *io.LimitedReader, rec int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(buf)+len(chunk) > MaxRecordBytes+1 {
			return nil, &Error{"record_too_large", rec}
		}
		buf = append(buf, chunk...)
		switch {
		case err == nil:
			return buf[:len(buf)-1], nil
		case errors.Is(err, bufio.ErrBufferFull):
		case errors.Is(err, io.EOF):
			if lr.N == 0 {
				return nil, &Error{"file_too_large", 0}
			}
			if len(buf) == 0 {
				return nil, io.EOF
			}
			return nil, &Error{"partial_record", rec}
		default:
			return nil, &Error{"read_failed", rec}
		}
	}
}

func checkNesting(line []byte, rec int) error {
	depth, inString, escaped := 0, false, false
	for _, b := range line {
		switch {
		case inString:
			switch {
			case escaped:
				escaped = false
			case b == '\\':
				escaped = true
			case b == '"':
				inString = false
			}
		case b == '"':
			inString = true
		case b == '{' || b == '[':
			depth++
			if depth > MaxNesting {
				return &Error{"nesting_too_deep", rec}
			}
		case b == '}' || b == ']':
			depth--
		}
	}
	return nil
}

func decodeStrictRecord(line []byte, into any, rec int) error {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		if errors.Is(err, errUnknownTag) {
			return &Error{"unknown_tag", rec}
		}
		return &Error{"malformed_record", rec}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return &Error{"malformed_record", rec}
	}
	return nil
}

// ---- dispositions ----------------------------------------------------------

// Disposition binds one retained source case whose native result differs from
// its source expectation to exact values, a rationale and an answered human
// question. It never removes or relabels the case.
type Disposition struct {
	CaseID     string           `json:"caseId"`
	Expected   map[string]Value `json:"expected"`
	Observed   map[string]Value `json:"observed"`
	Rationale  string           `json:"rationale"`
	Question   string           `json:"question"`
	Answer     string           `json:"answer"`
	AnsweredBy string           `json:"answeredBy"`
}

// Dispositions is the content of testdata/parser/dispositions.json.
type Dispositions struct {
	Schema       int           `json:"schema"`
	Dispositions []Disposition `json:"dispositions"`
}

// ReadDispositions decodes a bounded (1 MiB) strict dispositions file.
func ReadDispositions(r io.Reader) (*Dispositions, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxDispositionBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxDispositionBytes {
		return nil, &Error{"file_too_large", 0}
	}
	var d Dispositions
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		if errors.Is(err, errUnknownTag) {
			return nil, &Error{"unknown_tag", 0}
		}
		return nil, &Error{"malformed_record", 0}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, &Error{"malformed_record", 0}
	}
	if d.Schema != SchemaVersion || d.Dispositions == nil {
		return nil, &Error{"header_invalid", 0}
	}
	return &d, nil
}

// Validate rejects a stale or ownerless disposition: each must name a unique
// enabled in-scope case of c, carry a non-empty rationale, question, answer
// and answering owner, and bind expected values that still equal that case's
// recorded expectation exactly (same keys on both sides, none for the input).
func (d *Dispositions) Validate(c *Corpus) error {
	cases := map[string]*Case{}
	for i := range c.Cases {
		cases[c.Cases[i].ID] = &c.Cases[i]
	}
	seen := map[string]bool{}
	for i, disp := range d.Dispositions {
		fail := func(why string) error {
			return fmt.Errorf("corpus: disposition %d (%s): %s", i, disp.CaseID, why)
		}
		cs, ok := cases[disp.CaseID]
		switch {
		case !ok:
			return fail("unknown case")
		case seen[disp.CaseID]:
			return fail("duplicate")
		case !cs.UpstreamEnabled || cs.Scope == ScopeOutOfScope:
			return fail("case is not an enabled in-scope case")
		case disp.Rationale == "" || disp.Question == "" || disp.Answer == "" || disp.AnsweredBy == "":
			return fail("ownerless or unanswered")
		case len(disp.Expected) == 0 || len(disp.Expected) != len(disp.Observed):
			return fail("expected and observed values must be non-empty with the same keys")
		}
		seen[disp.CaseID] = true
		for k, want := range disp.Expected {
			if k == InputKey {
				return fail("input is not an assertion")
			}
			if _, ok := disp.Observed[k]; !ok {
				return fail("observed key missing")
			}
			if got, ok := cs.Expected[k]; !ok || !Equal(got, want) {
				return fail("expected value no longer equals the source case")
			}
		}
	}
	return nil
}

// Equal reports deep equality of two values.
func Equal(a, b Value) bool {
	if a.Kind != b.Kind || a.Str != b.Str || a.Type != b.Type || a.Int != b.Int || a.Bool != b.Bool ||
		a.Offset != b.Offset || a.Template != b.Template || len(a.Items) != len(b.Items) {
		return false
	}
	for i := range a.Items {
		if !Equal(a.Items[i], b.Items[i]) {
			return false
		}
	}
	return true
}

// ---- shared temporal contract ----------------------------------------------

// TimeContext is the evaluation clock of one corpus run: Now is the instant
// and Location the local calendar that source "today" means.
type TimeContext struct {
	Now      time.Time
	Location *time.Location
}

// Materialize returns the release name of v. A string is itself. A symbolic
// date is source-native local-calendar DateTime.Today.AddDays(Offset) in
// Location (calendar arithmetic, never a 24-hour duration), formatted with
// the exact source template.
func (tc TimeContext) Materialize(v Value) (string, error) {
	switch v.Kind {
	case KindString:
		return v.Str, nil
	case KindDate:
		if tc.Location == nil {
			return "", errors.New("corpus: TimeContext needs a Location")
		}
		y, m, d := tc.Now.In(tc.Location).Date()
		return formatTemplate(v.Template, time.Date(y, m, d+v.Offset, 0, 0, 0, 0, tc.Location))
	}
	return "", fmt.Errorf("corpus: a %s value is not a release name", v.Kind)
}

// Inputs materializes the case's recorded entry-point inputs.
func (c Case) Inputs(tc TimeContext) ([]string, error) {
	in, ok := c.Expected[InputKey]
	if !ok || in.Kind != KindArray || len(in.Items) == 0 {
		return nil, fmt.Errorf("corpus: case %s has no recorded inputs", c.ID)
	}
	names := make([]string, 0, len(in.Items))
	for _, v := range in.Items {
		s, err := tc.Materialize(v)
		if err != nil {
			return nil, err
		}
		names = append(names, s)
	}
	return names, nil
}

// formatTemplate expands a .NET composite format string whose only argument
// is one date: {0:spec} placeholders and {{ }} escapes. A bare {0} (culture
// dependent) and every other placeholder are rejected.
func formatTemplate(tmpl string, t time.Time) (string, error) {
	var sb strings.Builder
	for i := 0; i < len(tmpl); i++ {
		c := tmpl[i]
		switch {
		case c == '{' && i+1 < len(tmpl) && tmpl[i+1] == '{':
			sb.WriteByte('{')
			i++
		case c == '}' && i+1 < len(tmpl) && tmpl[i+1] == '}':
			sb.WriteByte('}')
			i++
		case c == '{':
			end := strings.IndexByte(tmpl[i:], '}')
			if end < 0 || !strings.HasPrefix(tmpl[i:], "{0:") {
				return "", errors.New("corpus: unsupported date placeholder")
			}
			s, err := formatSpec(tmpl[i+3:i+end], t)
			if err != nil {
				return "", err
			}
			sb.WriteString(s)
			i += end
		case c == '}':
			return "", errors.New("corpus: unbalanced brace in date template")
		default:
			sb.WriteByte(c)
		}
	}
	return sb.String(), nil
}

// formatSpec formats a .NET custom date specifier restricted to yyyy, yy, MM,
// M, dd, d, the separators . - _ space , : / and quoted literals.
func formatSpec(spec string, t time.Time) (string, error) {
	var sb strings.Builder
	for i := 0; i < len(spec); {
		c := spec[i]
		run := 1
		for i+run < len(spec) && spec[i+run] == c {
			run++
		}
		switch {
		case c == 'y' && run == 4:
			fmt.Fprintf(&sb, "%04d", t.Year())
		case c == 'y' && run == 2:
			fmt.Fprintf(&sb, "%02d", t.Year()%100)
		case c == 'M' && run == 2:
			fmt.Fprintf(&sb, "%02d", int(t.Month()))
		case c == 'M' && run == 1:
			fmt.Fprintf(&sb, "%d", int(t.Month()))
		case c == 'd' && run == 2:
			fmt.Fprintf(&sb, "%02d", t.Day())
		case c == 'd' && run == 1:
			fmt.Fprintf(&sb, "%d", t.Day())
		case c == '\'':
			end := strings.IndexByte(spec[i+1:], '\'')
			if end < 0 {
				return "", errors.New("corpus: unterminated quoted literal in date format")
			}
			sb.WriteString(spec[i+1 : i+1+end])
			i += end + 2
			continue
		case strings.IndexByte(".-_ ,:/", c) >= 0:
			sb.WriteString(strings.Repeat(string(c), run))
		default:
			return "", errors.New("corpus: unsupported date format specifier")
		}
		i += run
	}
	return sb.String(), nil
}

// SortedKeys returns the keys of m in order.
func SortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
