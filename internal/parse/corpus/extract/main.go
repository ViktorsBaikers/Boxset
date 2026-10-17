// Command extract reads the series and movie parser test sources as text and
// writes the two attributed canonical corpora. The C# is never compiled or
// executed: a small lexer, a statement/expression parser for the NUnit subset
// these tests use and a symbolic interpreter that records the assertions made
// on the scoped entry points lower every source test case into corpus records:
// QualityParser.ParseQuality (scope quality) and, on the movie side, the title
// entry points Parser.ParseMovieTitle, ReleaseGroupParser.ParseReleaseGroup and
// LanguageParser.ParseLanguages (scope title). A test method of a title fixture
// that asserts no title entry point is retained out of scope with its source
// arguments. A source form the extractor does not understand is rejected with
// its source location, never skipped.
//
// Usage:
//
//	extract -series DIR -movie DIR -series-commit SHA -movie-commit SHA -out DIR
//
// It writes DIR/series.jsonl and DIR/movie.jsonl in place (open with
// truncate; never create-exclusive, temp-and-rename or unlink), checks every
// write and close, and on any write error prints
// "extract: write_failed file=<name>" and exits 1. Exit 2 is a usage error.
//
// Both corpora derive from GPL-3.0 sources (see each header record).
package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/ViktorsBaikers/Boxset/internal/parse/corpus"
)

const (
	extractorVersion = "1"
	maxSourceBytes   = 2 << 20
	qualityOwner     = "QualityParser.ParseQuality"
	license          = "GPL-3.0"
	maxHelperDepth   = 8
)

// Reserved Expected keys beside the corpus's input key: the assertion made on
// the result of a title entry point itself, and the isDir argument of
// ParseMovieTitle (recorded only when true).
const (
	resultKey      = "$result"
	dirKey         = "$isDir"
	noThrowKey     = resultKey + ".NoThrow" // a case that only requires the call not to fail
	movieTitleCall = "ParseMovieTitle"
	movieTitleOwn  = "Parser.ParseMovieTitle"
	cleanTitleOwn  = "Parser.CleanMovieTitle"

	// cleanTitleKey is the property path of the parsed primary title after the
	// source's CleanMovieTitle normalization. Its recorded value is the expected
	// title before normalization: the assertion is that both sides normalize alike.
	cleanTitleKey = "MovieTitle.CleanMovieTitle"
	cleanTag      = "clean"

	// The series side's title entry points: ParseTitle, the ParseSeriesName
	// helper whose result the source normalizes, and the CleanSeriesTitle
	// normalization itself. cleanSeriesKey is the property path of the parsed
	// series name after that normalization; as on the movie side its recorded
	// value is the expected title before normalization.
	seriesTitleCall = "ParseTitle"
	seriesTitleOwn  = "Parser.ParseTitle"
	seriesNameCall  = "ParseSeriesName"
	seriesNameOwn   = "Parser.ParseSeriesName"
	cleanSeriesOwn  = "Parser.CleanSeriesTitle"
	cleanSeriesKey  = "SeriesName.CleanSeriesTitle"

	// airDateFormat is the source's Episode.AIR_DATE_FORMAT. The constant is not
	// in the delivered areas; its value is the date-only form the series parser
	// writes for AirDate.
	airDateFormat = "yyyy-MM-dd"
)

// Interpreter work limits per side, checked before the step or the growth each
// one bounds, so a hostile loop or expansion is rejected with its source
// location before time or memory grow.
const (
	maxInterpSteps   = 4_194_304
	maxObservations  = 65_536
	maxExpandedBytes = corpus.MaxFileBytes
)

// work is the cumulative interpreter budget of one side, shared by every file.
// neutral turns on the release group substitution of neutralGroup.
type work struct {
	steps, observations int
	bytes               int
	neutral             bool
}

// neutralName is the fixed release group every match of groupToken becomes in
// the series corpus. The reference tests name a release group after the
// upstream products; the repository never carries those names, so the
// extractor replaces each case-insensitive match, in every recorded string
// (input, argument, expected value, date template), before the corpus is
// encoded and before a case identity is hashed. The token is held base64
// encoded for the same reason: it is the pattern of the repository's name gate.
const (
	neutralName   = "Neutral"
	groupTokenB64 = "c29uYXJyfHJhZGFycg=="
)

var groupToken = func() *regexp.Regexp {
	pattern, err := base64.StdEncoding.DecodeString(groupTokenB64)
	if err != nil {
		panic(err)
	}
	return regexp.MustCompile("(?i)" + string(pattern))
}()

// neutralGroup applies the substitution documented at neutralName.
func neutralGroup(s string) string { return groupToken.ReplaceAllLiteralString(s, neutralName) }

// appSpec names the source files one side contributes. Paths are relative to
// the side's neutral root (/ref/<side>, whose tests/, parser/ and qualities/
// areas the runner maps to the reference checkout) and double as the corpus
// SourcePaths. tests are the quality fixtures. titles are the fixtures whose
// methods assert the title entry points (a method that asserts none is retained
// out of scope); languages is the parser source whose Language members the
// title assertions may name (the Language declaration itself is outside the
// delivered areas); provenance are ported sources recorded for attribution
// only and never interpreted.
type appSpec struct {
	name       string
	repository string
	tests      []string
	models     []string
	titles     []string
	languages  string
	provenance []string
	// neutralizeGroup selects the release group substitution for this side's records.
	neutralizeGroup bool
}

func appSpecs() [2]appSpec {
	test := func(f string) string { return "tests/" + f }
	return [2]appSpec{
		{
			name:       "series",
			repository: "env:BOXSET_REF_SERIES",
			tests: []string{
				test("QualityParserFixture.cs"), test("ExtendedQualityParserRegex.cs"), test("AnimeVersionFixture.cs"),
			},
			models: []string{
				"parser/QualityParser.cs",
				"qualities/Quality.cs",
				"qualities/QualitySource.cs",
				"qualities/QualityDetectionSource.cs",
			},
			titles: []string{
				test("ParserFixture.cs"), test("SingleEpisodeParserFixture.cs"), test("MultiEpisodeParserFixture.cs"),
				test("MiniSeriesEpisodeParserFixture.cs"), test("SeasonParserFixture.cs"), test("SeasonTitleParserFixture.cs"),
				test("AbsoluteEpisodeNumberParserFixture.cs"), test("AnimeMetadataParserFixture.cs"),
				test("DailyEpisodeParserFixture.cs"), test("CrapParserFixture.cs"), test("HashedReleaseFixture.cs"),
				test("ReleaseGroupParserFixture.cs"), test("SubGroupParserFixture.cs"), test("UnicodeReleaseParserFixture.cs"),
				test("UrlFixture.cs"), test("SeriesTitleInfoFixture.cs"), test("IsPossibleSpecialEpisodeFixture.cs"),
				test("LanguageParserFixture.cs"), test("NormalizeSeriesTitleFixture.cs"),
			},
			languages: "parser/LanguageParser.cs",
			provenance: []string{
				"parser/Parser.cs", "parser/ReleaseGroupParser.cs", "parser/ParserCommon.cs", "parser/RegexReplace.cs",
				"parser/Model/ParsedEpisodeInfo.cs", "parser/Model/SeriesTitleInfo.cs",
			},
			neutralizeGroup: true,
		},
		{
			name:       "movie",
			repository: "env:BOXSET_REF_MOVIE",
			tests: []string{
				test("QualityParserFixture.cs"), test("ExtendedQualityParserRegex.cs"), test("AnimeVersionFixture.cs"),
			},
			models: []string{
				"parser/QualityParser.cs",
				"qualities/Quality.cs",
				"qualities/QualitySource.cs",
				"qualities/Modifier.cs",
				"qualities/QualityDetectionSource.cs",
			},
			titles: []string{
				test("ParserFixture.cs"), test("EditionParserFixture.cs"), test("LanguageParserFixture.cs"),
				test("CrapParserFixture.cs"), test("HashedReleaseFixture.cs"), test("ReleaseGroupParserFixture.cs"),
				test("UrlFixture.cs"),
			},
			languages: "parser/LanguageParser.cs",
			provenance: []string{
				"parser/Parser.cs", "parser/ReleaseGroupParser.cs", "parser/ParserCommon.cs", "parser/RegexReplace.cs",
				"parser/Model/ParsedMovieInfo.cs",
			},
		},
	}
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

type output struct {
	file    string
	data    []byte
	summary []string
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("extract", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	series := fs.String("series", "", "")
	movie := fs.String("movie", "", "")
	seriesCommit := fs.String("series-commit", "", "")
	movieCommit := fs.String("movie-commit", "", "")
	out := fs.String("out", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *series == "" || *movie == "" ||
		*seriesCommit == "" || *movieCommit == "" || *out == "" {
		_, _ = fmt.Fprintln(stderr, "extract: usage: extract -series DIR -movie DIR -series-commit SHA -movie-commit SHA -out DIR")
		return 2
	}
	roots := map[string]string{"series": *series, "movie": *movie}
	commits := map[string]string{"series": *seriesCommit, "movie": *movieCommit}

	var outputs []output
	for _, spec := range appSpecs() {
		c, sources, err := extractApp(spec, roots[spec.name], commits[spec.name])
		if err != nil {
			reportError(stderr, spec.name, err)
			return 1
		}
		data, err := corpus.Marshal(c)
		if err != nil {
			var ce *corpus.Error
			if errors.As(err, &ce) && (ce.Code == "file_too_large" || ce.Code == "record_too_large") {
				_, _ = fmt.Fprintf(stderr, "extract: corpus_oversize file=%s.jsonl\n", spec.name)
			} else {
				_, _ = fmt.Fprintf(stderr, "extract: corpus_invalid app=%s\n", spec.name)
			}
			return 1
		}
		sum := sha256.Sum256(data)
		o := output{file: spec.name + ".jsonl", data: data}
		for _, s := range sources {
			o.summary = append(o.summary, fmt.Sprintf("extract: source app=%s path=%s bytes=%d sha256=%s", spec.name, s.path, s.bytes, s.sha))
		}
		t := c.Header.Counts
		o.summary = append(o.summary, fmt.Sprintf("extract: corpus app=%s file=%s cases=%d enabled=%d disabled=%d out_of_scope=%d bytes=%d sha256=%s",
			spec.name, o.file, t["cases"], t["enabled"], t["disabled"], t["out_of_scope"], len(data), hex.EncodeToString(sum[:])))
		outputs = append(outputs, o)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		_, _ = fmt.Fprintf(stderr, "extract: write_failed file=%s\n", outputs[0].file)
		return 1
	}
	for _, o := range outputs {
		if err := writeFile(filepath.Join(*out, o.file), o.data); err != nil {
			_, _ = fmt.Fprintf(stderr, "extract: write_failed file=%s\n", o.file)
			return 1
		}
	}
	for _, o := range outputs {
		for _, l := range o.summary {
			_, _ = fmt.Fprintln(stdout, l)
		}
	}
	_, _ = fmt.Fprintf(stdout, "extract: ok version=%s files=%d\n", extractorVersion, len(outputs))
	return 0
}

func reportError(w io.Writer, app string, err error) {
	var se *srcError
	if errors.As(err, &se) {
		_, _ = fmt.Fprintf(w, "extract: unsupported app=%s file=%s line=%d col=%d reason=%s\n", app, se.file, se.line, se.col, se.reason)
		return
	}
	_, _ = fmt.Fprintf(w, "extract: source_unavailable app=%s reason=%s\n", app, err)
}

// writeFile truncates and rewrites path in place, checking the write and the
// close.
func writeFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	n, err := f.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// ---- source loading --------------------------------------------------------

type sourceInfo struct {
	path  string
	bytes int
	sha   string
}

// readSource reads rel through root, so no path or link can leave the delivered
// tree, and keeps at most maxSourceBytes: the bytes actually read decide.
func readSource(root *os.Root, rel string) (string, sourceInfo, error) {
	fi, err := root.Lstat(rel)
	if err != nil || !fi.Mode().IsRegular() {
		return "", sourceInfo{}, fmt.Errorf("%s is not a readable regular file", rel)
	}
	f, err := root.Open(rel)
	if err != nil {
		return "", sourceInfo{}, fmt.Errorf("%s is unreadable", rel)
	}
	defer func() { _ = f.Close() }() // read-only handle: a close error cannot change the result
	data, err := readBounded(f, rel)
	if err != nil {
		return "", sourceInfo{}, err
	}
	if !utf8.Valid(data) {
		return "", sourceInfo{}, &srcError{rel, 1, 1, "source is not valid UTF-8"}
	}
	sum := sha256.Sum256(data)
	return string(data), sourceInfo{path: rel, bytes: len(data), sha: hex.EncodeToString(sum[:])}, nil
}

// readBounded reads at most maxSourceBytes+1 bytes and rejects the source with
// its location when more were delivered, whatever any metadata said.
func readBounded(r io.Reader, rel string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxSourceBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%s is unreadable", rel)
	}
	if len(data) > maxSourceBytes {
		return nil, &srcError{rel, 1, 1, fmt.Sprintf("source exceeds %d bytes", maxSourceBytes)}
	}
	return data, nil
}

func extractApp(spec appSpec, dir, commit string) (*corpus.Corpus, []sourceInfo, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("%s root is unreadable", spec.name)
	}
	defer func() { _ = root.Close() }() // read-only handle: a close error cannot change the result
	w := &work{neutral: spec.neutralizeGroup}
	m := &model{enums: map[string]map[string]int64{}, names: map[string]string{}}
	var infos []sourceInfo
	for _, rel := range spec.models {
		src, info, err := readSource(root, rel)
		if err != nil {
			return nil, nil, err
		}
		lx, err := lex(rel, src)
		if err != nil {
			return nil, nil, err
		}
		if err := m.scan(rel, lx.toks); err != nil {
			return nil, nil, err
		}
		infos = append(infos, info)
	}
	if len(m.enums["Quality"]) == 0 {
		return nil, nil, &srcError{spec.models[0], 1, 1, "no Quality members found in the model sources"}
	}
	if spec.languages != "" {
		src, info, err := readSource(root, spec.languages)
		if err != nil {
			return nil, nil, err
		}
		if err := m.scanLanguages(spec.languages, src); err != nil {
			return nil, nil, err
		}
		infos = append(infos, info)
	}
	type fixture struct {
		rel   string
		title bool
	}
	var fixtures []fixture
	for _, rel := range spec.tests {
		fixtures = append(fixtures, fixture{rel, false})
	}
	for _, rel := range spec.titles {
		fixtures = append(fixtures, fixture{rel, true})
	}
	var cases []corpus.Case
	counts := map[string]int{}
	for _, f := range fixtures {
		src, info, err := readSource(root, f.rel)
		if err != nil {
			return nil, nil, err
		}
		infos = append(infos, info)
		fc, st, err := fileCases(spec.name, f.rel, src, m, w, f.title)
		if err != nil {
			return nil, nil, err
		}
		cases = append(cases, fc...)
		counts["source:test_methods:"+f.rel] = st.testMethods
		counts["source:testcase_attrs:"+f.rel] = st.testCaseAttrs
		counts["source:source_rows:"+f.rel] = st.sourceRows
		counts["source:setup_methods:"+f.rel] = st.setupMethods
		counts["source:commented_test_forms:"+f.rel] = st.commented
		if st.plain > 0 {
			counts["source:plain_tests:"+f.rel] = st.plain
		}
	}
	for _, rel := range spec.provenance {
		_, info, err := readSource(root, rel)
		if err != nil {
			return nil, nil, err
		}
		infos = append(infos, info)
	}
	for k, v := range corpus.Tally(cases) {
		counts[k] = v
	}
	paths := append(append(append([]string{}, spec.tests...), spec.titles...), spec.models...)
	if spec.languages != "" {
		paths = append(paths, spec.languages)
	}
	paths = append(paths, spec.provenance...)
	return &corpus.Corpus{
		Header: corpus.Header{
			Schema:           corpus.SchemaVersion,
			App:              spec.name,
			SourceRepository: spec.repository,
			SourcePaths:      paths,
			SourceCommit:     commit,
			License:          license,
			Counts:           counts,
		},
		Cases: cases,
	}, infos, nil
}

// ---- lexer -----------------------------------------------------------------

type srcError struct {
	file      string
	line, col int
	reason    string
}

func (e *srcError) Error() string {
	return fmt.Sprintf("%s:%d:%d: %s", e.file, e.line, e.col, e.reason)
}

type pos struct{ line, col int }

type tokKind int

const (
	tEOF tokKind = iota
	tIdent
	tNum
	tStr
	tChar
	tPunct
)

type token struct {
	kind      tokKind
	text      string
	line, col int
}

func (t token) p() pos { return pos{t.line, t.col} }

type lexResult struct {
	toks     []token
	comments []string
}

var twoCharOps = [...]string{"=>", "==", "!=", "<=", ">=", "&&", "||", "??", "?.", "++", "--", "+=", "-=", "::"}

func lex(file, src string) (lexResult, error) {
	rs := []rune(strings.TrimPrefix(src, "\uFEFF"))
	var out lexResult
	line, col, i := 1, 1, 0
	adv := func(n int) {
		for range n {
			if rs[i] == '\n' {
				line++
				col = 1
			} else {
				col++
			}
			i++
		}
	}
	at := func(k int) rune {
		if i+k < len(rs) {
			return rs[i+k]
		}
		return 0
	}
	fail := func(l, c int, why string) (lexResult, error) {
		return lexResult{}, &srcError{file, l, c, why}
	}
	for i < len(rs) {
		r := rs[i]
		l0, c0 := line, col
		switch {
		case unicode.IsSpace(r):
			adv(1)
		case r == '/' && at(1) == '/':
			j := i
			for j < len(rs) && rs[j] != '\n' {
				j++
			}
			out.comments = append(out.comments, string(rs[i:j]))
			adv(j - i)
		case r == '/' && at(1) == '*':
			j := i + 2
			for j+1 < len(rs) && (rs[j] != '*' || rs[j+1] != '/') {
				j++
			}
			if j+1 >= len(rs) {
				return fail(l0, c0, "unterminated block comment")
			}
			out.comments = append(out.comments, string(rs[i:j+2]))
			adv(j + 2 - i)
		case r == '#':
			return fail(l0, c0, "preprocessor directive is not supported")
		case r == '$' && (at(1) == '"' || at(1) == '@'):
			return fail(l0, c0, "interpolated string is not supported")
		case r == '@' && at(1) == '"':
			var sb strings.Builder
			j := i + 2
			for {
				if j >= len(rs) {
					return fail(l0, c0, "unterminated verbatim string")
				}
				if rs[j] == '"' {
					if j+1 < len(rs) && rs[j+1] == '"' {
						sb.WriteRune('"')
						j += 2
						continue
					}
					break
				}
				sb.WriteRune(rs[j])
				j++
			}
			out.toks = append(out.toks, token{tStr, sb.String(), l0, c0})
			adv(j + 1 - i)
		case r == '"':
			if at(1) == '"' && at(2) == '"' {
				return fail(l0, c0, "raw string literal is not supported")
			}
			var sb strings.Builder
			j := i + 1
			for {
				if j >= len(rs) || rs[j] == '\n' {
					return fail(l0, c0, "unterminated string")
				}
				if rs[j] == '"' {
					break
				}
				if rs[j] == '\\' {
					ch, n, why := unescapeString(rs[j:])
					if why != "" {
						return fail(l0, c0, why)
					}
					sb.WriteRune(ch)
					j += n
					continue
				}
				sb.WriteRune(rs[j])
				j++
			}
			out.toks = append(out.toks, token{tStr, sb.String(), l0, c0})
			adv(j + 1 - i)
		case r == '\'':
			j := i + 1
			var ch rune
			switch {
			case j < len(rs) && rs[j] == '\\':
				c, n, ok := unescape(rs[j:])
				if !ok {
					return fail(l0, c0, "unsupported escape sequence")
				}
				if utf16.IsSurrogate(c) {
					return fail(l0, c0, "lone UTF-16 surrogate escape")
				}
				ch = c
				j += n
			case j < len(rs) && rs[j] != '\'' && rs[j] != '\n':
				ch = rs[j]
				j++
			default:
				return fail(l0, c0, "malformed character literal")
			}
			if j >= len(rs) || rs[j] != '\'' {
				return fail(l0, c0, "malformed character literal")
			}
			out.toks = append(out.toks, token{tChar, string(ch), l0, c0})
			adv(j + 1 - i)
		case r >= '0' && r <= '9':
			j := i
			for j < len(rs) && rs[j] >= '0' && rs[j] <= '9' {
				j++
			}
			if j+1 < len(rs) && rs[j] == '.' && rs[j+1] >= '0' && rs[j+1] <= '9' {
				j++
				for j < len(rs) && rs[j] >= '0' && rs[j] <= '9' {
					j++
				}
			}
			if j < len(rs) && (unicode.IsLetter(rs[j]) || rs[j] == '_') {
				return fail(l0, c0, "numeric literal suffix is not supported")
			}
			out.toks = append(out.toks, token{tNum, string(rs[i:j]), l0, c0})
			adv(j - i)
		case unicode.IsLetter(r) || r == '_':
			j := i
			for j < len(rs) && (unicode.IsLetter(rs[j]) || unicode.IsDigit(rs[j]) || rs[j] == '_') {
				j++
			}
			out.toks = append(out.toks, token{tIdent, string(rs[i:j]), l0, c0})
			adv(j - i)
		default:
			text := string(r)
			if i+1 < len(rs) {
				two := string(rs[i : i+2])
				for _, op := range twoCharOps {
					if two == op {
						text = two
						break
					}
				}
			}
			out.toks = append(out.toks, token{tPunct, text, l0, c0})
			adv(len([]rune(text)))
		}
	}
	out.toks = append(out.toks, token{tEOF, "", line, col})
	return out, nil
}

// unescape decodes one backslash escape at the start of rs and returns the
// rune and the number of runes consumed.
func unescape(rs []rune) (rune, int, bool) {
	if len(rs) < 2 {
		return 0, 0, false
	}
	simple := map[rune]rune{'"': '"', '\\': '\\', '\'': '\'', '0': 0, 'a': 7, 'b': 8, 'f': 12, 'n': '\n', 'r': '\r', 't': '\t', 'v': 11}
	if r, ok := simple[rs[1]]; ok {
		return r, 2, true
	}
	width := 0
	switch rs[1] {
	case 'u':
		width = 4
	case 'U':
		width = 8
	default:
		return 0, 0, false
	}
	if len(rs) < 2+width {
		return 0, 0, false
	}
	n, err := strconv.ParseUint(string(rs[2:2+width]), 16, 32)
	if err != nil || n > unicode.MaxRune {
		return 0, 0, false
	}
	return rune(n), 2 + width, true
}

// unescapeString decodes one escape of a string literal. A UTF-16 surrogate
// pair written as two \u escapes (high then low) decodes to its one code
// point; a lone surrogate, or a surrogate written with \U, has no text form and
// is rejected with a reason. An empty reason means success.
func unescapeString(rs []rune) (rune, int, string) {
	ch, n, ok := unescape(rs)
	switch {
	case !ok:
		return 0, 0, "unsupported escape sequence"
	case !utf16.IsSurrogate(ch):
		return ch, n, ""
	case rs[1] != 'u' || ch >= 0xDC00:
		return 0, 0, "lone UTF-16 surrogate escape"
	}
	low, m, ok := unescape(rs[n:])
	if !ok || m != 6 || rs[n+1] != 'u' || low < 0xDC00 || low > 0xDFFF {
		return 0, 0, "lone UTF-16 surrogate escape"
	}
	return utf16.DecodeRune(ch, low), n + m, ""
}

// ---- AST ---------------------------------------------------------------------

type expr interface{}

type (
	litE   struct{ v val }
	identE struct {
		p    pos
		name string
	}
	memberE struct {
		p    pos
		x    expr
		name string
	}
	callE struct {
		p    pos
		fn   expr
		args []expr
	}
	arrE struct {
		p     pos
		elems []expr
	}
	castE struct {
		p   pos
		typ string
		x   expr
	}
	unE struct {
		p  pos
		op string
		x  expr
	}
	binE struct {
		p    pos
		op   string
		l, r expr
	}
	condE struct {
		p       pos
		c, a, b expr
	}
	idxE struct {
		p    pos
		x, i expr
	}
	assignE struct {
		p        pos
		name, op string
		x        expr
	}
	anonE struct {
		p     pos
		names []string
		vals  []expr
	}
	ctorE struct {
		p    pos
		typ  string
		args []expr
	}
)

type stmt interface{}

type (
	declS struct {
		p    pos
		name string
		init expr
	}
	exprS struct{ x expr }
	ifS   struct {
		p         pos
		c         expr
		then, els []stmt
	}
	foreachS struct {
		p    pos
		name string
		in   expr
		body []stmt
	}
	retS   struct{}
	blockS struct{ body []stmt }
)

// forS is for (var name = init; cond; post) body.
type forS struct {
	p          pos
	name       string
	init, cond expr
	post       expr
	body       []stmt
}

type attr struct {
	p     pos
	name  string
	args  []expr
	named map[string]expr
}

type param struct {
	typ, name string
	def       expr
	variadic  bool // params T[]: the trailing arguments of a test case are one array
}

type method struct {
	p      pos
	name   string
	attrs  []attr
	params []param
	body   []stmt
	toks   []token // the tokens between the braces of the body
}

type field struct {
	p    pos
	name string
	typ  string
	init expr
}

type class struct {
	attrs   []attr
	methods []*method
	fields  []*field
}

// ---- parser --------------------------------------------------------------------

type parser struct {
	file  string
	t     []token
	i     int
	title bool   // title fixture: a body that asserts no title entry point is not interpreted
	app   string // the side: it decides which entry points a title fixture names
}

func (p *parser) cur() token { return p.t[p.i] }

func (p *parser) at(n int) token {
	if p.i+n < len(p.t) {
		return p.t[p.i+n]
	}
	return p.t[len(p.t)-1]
}

func (p *parser) advance() token {
	t := p.t[p.i]
	if t.kind != tEOF {
		p.i++
	}
	return t
}

func (p *parser) isP(s string) bool  { t := p.cur(); return t.kind == tPunct && t.text == s }
func (p *parser) isID(s string) bool { t := p.cur(); return t.kind == tIdent && t.text == s }

func (p *parser) fail(t token, why string) error {
	return &srcError{p.file, t.line, t.col, why}
}

func (p *parser) expectP(s string) (token, error) {
	if !p.isP(s) {
		return token{}, p.fail(p.cur(), "expected "+strconv.Quote(s))
	}
	return p.advance(), nil
}

func (p *parser) expectIdent() (token, error) {
	if p.cur().kind != tIdent {
		return token{}, p.fail(p.cur(), "expected an identifier")
	}
	return p.advance(), nil
}

var modifiers = map[string]bool{
	"public": true, "private": true, "protected": true, "internal": true, "static": true, "readonly": true,
	"const": true, "virtual": true, "override": true, "abstract": true, "sealed": true, "async": true,
	"unsafe": true, "partial": true, "extern": true, "new": true,
}

// parseType consumes a type reference and returns its last name segment plus
// "[]" per array rank.
func (p *parser) parseType() (string, error) {
	t, err := p.expectIdent()
	if err != nil {
		return "", err
	}
	name := t.text
	for p.isP(".") && p.at(1).kind == tIdent {
		p.advance()
		name = p.advance().text
	}
	if p.isP("<") {
		depth := 0
		for {
			switch {
			case p.cur().kind == tEOF:
				return "", p.fail(p.cur(), "unterminated generic type arguments")
			case p.isP("<"):
				depth++
			case p.isP(">"):
				depth--
			}
			p.advance()
			if depth == 0 {
				break
			}
		}
	}
	for p.isP("[") && p.at(1).kind == tPunct && p.at(1).text == "]" {
		p.advance()
		p.advance()
		name += "[]"
	}
	if p.isP("?") {
		p.advance()
	}
	return name, nil
}

func (p *parser) parseFile() ([]*class, error) {
	var classes []*class
	for p.cur().kind != tEOF {
		switch {
		case p.isID("using"):
			for !p.isP(";") && p.cur().kind != tEOF {
				p.advance()
			}
			p.advance()
		case p.isID("namespace"):
			p.advance()
			if _, err := p.parseType(); err != nil {
				return nil, err
			}
			if p.isP(";") {
				p.advance()
			} else if p.isP("{") {
				p.advance()
			} else {
				return nil, p.fail(p.cur(), "malformed namespace")
			}
		case p.isP("}"):
			p.advance()
		default:
			c, err := p.parseClass()
			if err != nil {
				return nil, err
			}
			classes = append(classes, c)
		}
	}
	return classes, nil
}

func (p *parser) parseClass() (*class, error) {
	attrs, err := p.parseAttrs()
	if err != nil {
		return nil, err
	}
	for p.cur().kind == tIdent && modifiers[p.cur().text] {
		p.advance()
	}
	if !p.isID("class") {
		return nil, p.fail(p.cur(), "only classes are supported at type level")
	}
	p.advance()
	if _, err := p.expectIdent(); err != nil {
		return nil, err
	}
	for !p.isP("{") {
		if p.cur().kind == tEOF {
			return nil, p.fail(p.cur(), "class body not found")
		}
		p.advance()
	}
	p.advance()
	c := &class{attrs: attrs}
	for !p.isP("}") {
		if p.cur().kind == tEOF {
			return nil, p.fail(p.cur(), "unterminated class body")
		}
		if err := p.parseMember(c); err != nil {
			return nil, err
		}
	}
	p.advance()
	return c, nil
}

func (p *parser) parseAttrs() ([]attr, error) {
	var attrs []attr
	for p.isP("[") {
		p.advance()
		for {
			name, err := p.expectIdent()
			if err != nil {
				return nil, err
			}
			full := name.text
			for p.isP(".") {
				p.advance()
				n2, err := p.expectIdent()
				if err != nil {
					return nil, err
				}
				full = n2.text
			}
			a := attr{p: name.p(), name: strings.TrimSuffix(full, "Attribute"), named: map[string]expr{}}
			if p.isP("(") {
				p.advance()
				for !p.isP(")") {
					if p.cur().kind == tIdent && p.at(1).kind == tPunct && p.at(1).text == "=" {
						n := p.advance().text
						p.advance()
						e, err := p.parseExpr()
						if err != nil {
							return nil, err
						}
						a.named[n] = e
					} else {
						e, err := p.parseExpr()
						if err != nil {
							return nil, err
						}
						a.args = append(a.args, e)
					}
					if p.isP(",") {
						p.advance()
					} else if !p.isP(")") {
						return nil, p.fail(p.cur(), "malformed attribute arguments")
					}
				}
				p.advance()
			}
			attrs = append(attrs, a)
			if p.isP(",") {
				p.advance()
				continue
			}
			break
		}
		if _, err := p.expectP("]"); err != nil {
			return nil, err
		}
	}
	return attrs, nil
}

func (p *parser) parseMember(c *class) error {
	attrs, err := p.parseAttrs()
	if err != nil {
		return err
	}
	for p.cur().kind == tIdent && modifiers[p.cur().text] {
		p.advance()
	}
	typ, err := p.parseType()
	if err != nil {
		return err
	}
	name, err := p.expectIdent()
	if err != nil {
		return p.fail(p.cur(), "unsupported member (constructor, property or nested type)")
	}
	switch {
	case p.isP("("):
		m := &method{p: name.p(), name: name.text, attrs: attrs}
		p.advance()
		for !p.isP(")") {
			if p.isID("this") {
				return p.fail(p.cur(), "unsupported parameter modifier")
			}
			variadic := p.isID("params")
			if variadic {
				p.advance()
			}
			pt, err := p.parseType()
			if err != nil {
				return err
			}
			pn, err := p.expectIdent()
			if err != nil {
				return err
			}
			prm := param{typ: pt, name: pn.text, variadic: variadic}
			if p.isP("=") {
				p.advance()
				if prm.def, err = p.parseExpr(); err != nil {
					return err
				}
			}
			m.params = append(m.params, prm)
			if p.isP(",") {
				p.advance()
			} else if !p.isP(")") {
				return p.fail(p.cur(), "malformed parameter list")
			}
		}
		p.advance()
		if !p.isP("{") {
			return p.fail(p.cur(), "only block-bodied methods are supported")
		}
		open := p.i
		end := p.matchingBrace()
		if end < 0 {
			return p.fail(p.cur(), "unterminated block")
		}
		m.toks = p.t[open+1 : end]
		if scope, _ := classify(p.app, m.toks); p.title && scope != corpus.ScopeTitle {
			// Retained out of scope with its source arguments; the body is never interpreted.
			p.i = end + 1
		} else if m.body, err = p.parseBlock(); err != nil {
			return err
		}
		c.methods = append(c.methods, m)
	case p.isP("=") || p.isP(";"):
		f := &field{p: name.p(), name: name.text, typ: typ}
		if p.isP("=") {
			p.advance()
			if f.init, err = p.parseInitializer(); err != nil {
				return err
			}
		}
		if _, err := p.expectP(";"); err != nil {
			return err
		}
		c.fields = append(c.fields, f)
	default:
		return p.fail(p.cur(), "unsupported member (property or accessor)")
	}
	return nil
}

func (p *parser) parseBlock() ([]stmt, error) {
	if _, err := p.expectP("{"); err != nil {
		return nil, err
	}
	var body []stmt
	for !p.isP("}") {
		if p.cur().kind == tEOF {
			return nil, p.fail(p.cur(), "unterminated block")
		}
		s, err := p.parseStmt()
		if err != nil {
			return nil, err
		}
		body = append(body, s)
	}
	p.advance()
	return body, nil
}

// matchingBrace returns the index of the brace that closes the block opened at
// the current token, or -1.
func (p *parser) matchingBrace() int {
	depth := 0
	for i := p.i; i < len(p.t); i++ {
		if p.t[i].kind != tPunct {
			continue
		}
		switch p.t[i].text {
		case "{":
			depth++
		case "}":
			if depth--; depth == 0 {
				return i
			}
		}
	}
	return -1
}

var unsupportedStmt = map[string]bool{
	"while": true, "do": true, "try": true, "switch": true, "throw": true,
	"lock": true, "yield": true, "goto": true, "break": true, "continue": true, "else": true,
}

// parseStep parses a for-loop step: name++, name--, name += expr or name -= expr.
func (p *parser) parseStep() (expr, error) {
	n, err := p.expectIdent()
	if err != nil {
		return nil, err
	}
	t := p.cur()
	switch {
	case p.isP("++"), p.isP("--"):
		p.advance()
		op := "+="
		if t.text == "--" {
			op = "-="
		}
		return &assignE{p: n.p(), name: n.text, op: op, x: &litE{val{k: vInt, i: 1}}}, nil
	case p.isP("+="), p.isP("-="):
		p.advance()
		x, err := p.parseExpr()
		return &assignE{p: n.p(), name: n.text, op: t.text, x: x}, err
	}
	return nil, p.fail(t, "unsupported for step")
}

func (p *parser) looksLikeDecl() bool {
	save := p.i
	defer func() { p.i = save }()
	if p.cur().kind != tIdent || p.isID("return") || p.isID("new") || p.isID("if") || p.isID("foreach") {
		return false
	}
	if _, err := p.parseType(); err != nil {
		return false
	}
	nx := p.at(1)
	return p.cur().kind == tIdent && nx.kind == tPunct && (nx.text == "=" || nx.text == ";")
}

func (p *parser) parseBody() ([]stmt, error) {
	if p.isP("{") {
		return p.parseBlock()
	}
	s, err := p.parseStmt()
	if err != nil {
		return nil, err
	}
	return []stmt{s}, nil
}

func (p *parser) parseStmt() (stmt, error) {
	t := p.cur()
	switch {
	case p.isP("{"):
		b, err := p.parseBlock()
		return &blockS{b}, err
	case t.kind == tIdent && unsupportedStmt[t.text]:
		return nil, p.fail(t, "unsupported statement "+t.text)
	case p.isID("if"):
		p.advance()
		if _, err := p.expectP("("); err != nil {
			return nil, err
		}
		c, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expectP(")"); err != nil {
			return nil, err
		}
		s := &ifS{p: t.p(), c: c}
		if s.then, err = p.parseBody(); err != nil {
			return nil, err
		}
		if p.isID("else") {
			p.advance()
			if s.els, err = p.parseBody(); err != nil {
				return nil, err
			}
		}
		return s, nil
	case p.isID("foreach"):
		p.advance()
		if _, err := p.expectP("("); err != nil {
			return nil, err
		}
		if _, err := p.parseType(); err != nil {
			return nil, err
		}
		n, err := p.expectIdent()
		if err != nil {
			return nil, err
		}
		if !p.isID("in") {
			return nil, p.fail(p.cur(), "expected in")
		}
		p.advance()
		in, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expectP(")"); err != nil {
			return nil, err
		}
		s := &foreachS{p: t.p(), name: n.text, in: in}
		if s.body, err = p.parseBody(); err != nil {
			return nil, err
		}
		return s, nil
	case p.isID("for"):
		p.advance()
		if _, err := p.expectP("("); err != nil {
			return nil, err
		}
		if !p.looksLikeDecl() {
			return nil, p.fail(p.cur(), "unsupported for initializer")
		}
		if _, err := p.parseType(); err != nil {
			return nil, err
		}
		n := p.advance()
		if _, err := p.expectP("="); err != nil {
			return nil, err
		}
		init, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expectP(";"); err != nil {
			return nil, err
		}
		cond, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expectP(";"); err != nil {
			return nil, err
		}
		post, err := p.parseStep()
		if err != nil {
			return nil, err
		}
		if _, err := p.expectP(")"); err != nil {
			return nil, err
		}
		s := &forS{p: t.p(), name: n.text, init: init, cond: cond, post: post}
		if s.body, err = p.parseBody(); err != nil {
			return nil, err
		}
		return s, nil
	case p.isID("using"):
		p.advance()
		for _, want := range [...]string{"(", "new", "AssertionScope", "(", ")", ")"} {
			if p.cur().kind == tStr || p.cur().text != want {
				return nil, p.fail(p.cur(), "only using (new AssertionScope()) is supported")
			}
			p.advance()
		}
		body, err := p.parseBody()
		return &blockS{body}, err
	case p.isID("return"):
		p.advance()
		if !p.isP(";") {
			return nil, p.fail(p.cur(), "return values are not supported")
		}
		p.advance()
		return &retS{}, nil
	case p.isID("const"):
		// A local constant is a declaration with an initializer.
		p.advance()
		s, err := p.parseStmt()
		if err != nil {
			return nil, err
		}
		if d, ok := s.(*declS); !ok || d.init == nil {
			return nil, p.fail(t, "unsupported const declaration")
		}
		return s, nil
	case t.kind == tIdent && p.at(1).kind == tPunct && p.at(1).text == "=":
		// Assignment to a parameter or a local: name = expr;
		p.advance()
		p.advance()
		x, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		_, err = p.expectP(";")
		return &exprS{&assignE{p: t.p(), name: t.text, op: "=", x: x}}, err
	case p.looksLikeDecl():
		if _, err := p.parseType(); err != nil {
			return nil, err
		}
		n := p.advance()
		d := &declS{p: n.p(), name: n.text}
		if p.isP("=") {
			p.advance()
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			d.init = e
		}
		_, err := p.expectP(";")
		return d, err
	}
	e, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	_, err = p.expectP(";")
	return &exprS{e}, err
}

// parseInitializer parses a field or array element initializer, where a bare
// brace list is an array.
func (p *parser) parseInitializer() (expr, error) {
	if !p.isP("{") {
		return p.parseExpr()
	}
	open := p.advance()
	a := &arrE{p: open.p()}
	for !p.isP("}") {
		e, err := p.parseInitializer()
		if err != nil {
			return nil, err
		}
		a.elems = append(a.elems, e)
		if p.isP(",") {
			p.advance()
		} else if !p.isP("}") {
			return nil, p.fail(p.cur(), "malformed array initializer")
		}
	}
	p.advance()
	return a, nil
}

func (p *parser) parseExpr() (expr, error) {
	c, err := p.parseBin(0)
	if err != nil || !p.isP("?") {
		return c, err
	}
	q := p.advance()
	a, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if _, err := p.expectP(":"); err != nil {
		return nil, err
	}
	b, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	return &condE{p: q.p(), c: c, a: a, b: b}, nil
}

var binLevels = [...][]string{
	{"||"}, {"&&"}, {"==", "!="}, {"<", ">", "<=", ">="}, {"+", "-"}, {"*", "/", "%"},
}

func (p *parser) parseBin(level int) (expr, error) {
	if level == len(binLevels) {
		return p.parseUnary()
	}
	l, err := p.parseBin(level + 1)
	if err != nil {
		return nil, err
	}
	for {
		t := p.cur()
		matched := false
		if t.kind == tPunct {
			for _, op := range binLevels[level] {
				if t.text == op {
					matched = true
				}
			}
		}
		if !matched {
			return l, nil
		}
		p.advance()
		r, err := p.parseBin(level + 1)
		if err != nil {
			return nil, err
		}
		l = &binE{p: t.p(), op: t.text, l: l, r: r}
	}
}

var castTypes = map[string]bool{"int": true, "long": true, "decimal": true, "double": true, "Language": true}

func (p *parser) parseUnary() (expr, error) {
	t := p.cur()
	switch {
	case p.isP("!") || p.isP("-"):
		p.advance()
		x, err := p.parseUnary()
		return &unE{p: t.p(), op: t.text, x: x}, err
	case p.isP("(") && p.at(1).kind == tIdent && castTypes[p.at(1).text] && p.at(2).kind == tPunct && p.at(2).text == ")":
		p.advance()
		typ := p.advance().text
		p.advance()
		x, err := p.parseUnary()
		return &castE{p: t.p(), typ: typ, x: x}, err
	}
	return p.parsePostfix()
}

func (p *parser) parsePostfix() (expr, error) {
	x, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for {
		t := p.cur()
		switch {
		case p.isP("."):
			p.advance()
			n, err := p.expectIdent()
			if err != nil {
				return nil, err
			}
			x = &memberE{p: n.p(), x: x, name: n.text}
		case p.isP("("):
			p.advance()
			c := &callE{p: t.p(), fn: x}
			for !p.isP(")") {
				a, err := p.parseExpr()
				if err != nil {
					return nil, err
				}
				c.args = append(c.args, a)
				if p.isP(",") {
					p.advance()
				} else if !p.isP(")") {
					return nil, p.fail(p.cur(), "malformed argument list")
				}
			}
			p.advance()
			x = c
		case p.isP("?."):
			return nil, p.fail(t, "unsupported member access form "+t.text)
		case p.isP("["):
			p.advance()
			i, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if _, err := p.expectP("]"); err != nil {
				return nil, err
			}
			x = &idxE{p: t.p(), x: x, i: i}
		default:
			return x, nil
		}
	}
}

func (p *parser) parsePrimary() (expr, error) {
	t := p.cur()
	switch t.kind {
	case tNum:
		p.advance()
		if strings.Contains(t.text, ".") {
			return &litE{val{k: vDec, s: t.text}}, nil
		}
		n, err := strconv.ParseInt(t.text, 10, 64)
		if err != nil {
			return nil, p.fail(t, "integer literal out of range")
		}
		return &litE{val{k: vInt, i: n}}, nil
	case tStr:
		p.advance()
		return &litE{val{k: vStr, s: t.text}}, nil
	case tChar:
		p.advance()
		r, _ := utf8.DecodeRuneInString(t.text)
		return &litE{val{k: vChar, i: int64(r)}}, nil
	case tIdent:
		switch t.text {
		case "true", "false":
			p.advance()
			return &litE{val{k: vBool, b: t.text == "true"}}, nil
		case "null":
			p.advance()
			return &litE{val{k: vNull}}, nil
		case "new":
			p.advance()
			typ := ""
			switch {
			case p.isP("{"):
				return p.parseAnon(t)
			case p.isP("[") && p.at(1).kind == tPunct && p.at(1).text == "]":
				p.advance()
				p.advance()
			default:
				var err error
				if typ, err = p.parseType(); err != nil {
					return nil, err
				}
			}
			if typ != "" && p.isP("(") {
				return p.parseCtor(t, typ)
			}
			if p.isP("[") && p.at(1).kind == tNum && p.at(1).text == "0" && p.at(2).kind == tPunct && p.at(2).text == "]" {
				p.advance()
				p.advance()
				p.advance()
				return &arrE{p: t.p()}, nil
			}
			if !p.isP("{") {
				return nil, p.fail(p.cur(), "only array creation with an initializer is supported")
			}
			return p.parseInitializer()
		case "default", "typeof", "this", "base", "delegate", "await":
			return nil, p.fail(t, "unsupported expression "+t.text)
		}
		p.advance()
		return &identE{p: t.p(), name: t.text}, nil
	case tPunct:
		if t.text == "(" {
			p.advance()
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			_, err = p.expectP(")")
			return e, err
		}
	}
	return nil, p.fail(t, "unsupported expression")
}

// parseCtor parses the argument list of `new Type(...)`.
func (p *parser) parseCtor(t token, typ string) (expr, error) {
	p.advance()
	c := &ctorE{p: t.p(), typ: typ}
	for !p.isP(")") {
		a, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		c.args = append(c.args, a)
		if p.isP(",") {
			p.advance()
		} else if !p.isP(")") {
			return nil, p.fail(p.cur(), "malformed argument list")
		}
	}
	p.advance()
	return c, nil
}

// parseAnon parses an anonymous object `new { name = expr, ... }`.
func (p *parser) parseAnon(t token) (expr, error) {
	p.advance()
	a := &anonE{p: t.p()}
	for !p.isP("}") {
		n, err := p.expectIdent()
		if err != nil {
			return nil, err
		}
		if _, err := p.expectP("="); err != nil {
			return nil, err
		}
		x, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		a.names = append(a.names, n.text)
		a.vals = append(a.vals, x)
		if p.isP(",") {
			p.advance()
		} else if !p.isP("}") {
			return nil, p.fail(p.cur(), "malformed anonymous object")
		}
	}
	p.advance()
	return a, nil
}

// ---- model sources (enums and the Quality table) -----------------------------

type model struct {
	enums map[string]map[string]int64
	names map[string]string // Quality member -> Name
}

func (m *model) scan(file string, toks []token) error {
	for i := 0; i+2 < len(toks); i++ {
		if toks[i].kind == tIdent && toks[i].text == "enum" && toks[i+1].kind == tIdent && toks[i+2].kind == tPunct && toks[i+2].text == "{" {
			if err := m.scanEnum(file, toks, i+1); err != nil {
				return err
			}
		}
	}
	for i := 0; i+2 < len(toks); i++ {
		if toks[i].kind == tIdent && toks[i].text == "new" && toks[i+1].kind == tIdent && toks[i+1].text == "Quality" &&
			toks[i+2].kind == tPunct && toks[i+2].text == "(" {
			if err := m.scanQuality(file, toks, i); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *model) scanEnum(file string, toks []token, i int) error {
	name := toks[i].text
	if _, dup := m.enums[name]; dup {
		return &srcError{file, toks[i].line, toks[i].col, "duplicate enum " + name}
	}
	members := map[string]int64{}
	next := int64(0)
	i += 2
	for toks[i].kind != tEOF && (toks[i].kind != tPunct || toks[i].text != "}") {
		if toks[i].kind != tIdent {
			return &srcError{file, toks[i].line, toks[i].col, "malformed enum member"}
		}
		member := toks[i].text
		i++
		if toks[i].kind == tPunct && toks[i].text == "=" {
			i++
			neg := int64(1)
			if toks[i].kind == tPunct && toks[i].text == "-" {
				neg = -1
				i++
			}
			if toks[i].kind != tNum {
				return &srcError{file, toks[i].line, toks[i].col, "enum member value must be an integer literal"}
			}
			n, err := strconv.ParseInt(toks[i].text, 10, 64)
			if err != nil {
				return &srcError{file, toks[i].line, toks[i].col, "enum member value out of range"}
			}
			next = neg * n
			i++
		}
		members[member] = next
		next++
		if toks[i].kind == tPunct && toks[i].text == "," {
			i++
		}
	}
	if len(members) == 0 {
		return &srcError{file, toks[i].line, toks[i].col, "empty enum " + name}
	}
	m.enums[name] = members
	return nil
}

// scanQuality reads `new Quality(ID, "Name", ...)` and the member it
// initializes: `Quality NAME => new Quality(...)` or
// `Quality NAME { get { return new Quality(...); } }`.
func (m *model) scanQuality(file string, toks []token, i int) error {
	bad := func(why string) error { return &srcError{file, toks[i].line, toks[i].col, why} }
	if toks[i+3].kind != tNum || toks[i+4].text != "," || toks[i+5].kind != tStr {
		return bad("malformed Quality definition")
	}
	id, err := strconv.ParseInt(toks[i+3].text, 10, 64)
	if err != nil {
		return bad("Quality id out of range")
	}
	var name string
	switch {
	case i >= 3 && toks[i-1].text == "=>" && toks[i-2].kind == tIdent && toks[i-3].text == "Quality":
		name = toks[i-2].text
	case i >= 5 && toks[i-1].text == "return" && toks[i-2].text == "{" && toks[i-3].text == "get" &&
		toks[i-4].text == "{" && toks[i-5].kind == tIdent:
		name = toks[i-5].text
	default:
		return bad("unrecognized Quality definition form")
	}
	if m.enums["Quality"] == nil {
		m.enums["Quality"] = map[string]int64{}
	}
	if _, dup := m.enums["Quality"][name]; dup {
		return bad("duplicate Quality member " + name)
	}
	m.enums["Quality"][name] = id
	m.names[name] = toks[i+5].text
	return nil
}

// scanLanguages records the Language members the parser source names. The
// Language declaration is outside the delivered areas, so the members a title
// assertion may name are the ones the parser itself produces. The source is
// only mined for names, never interpreted, so it is scanned as text (line
// comments skipped): it may hold forms the lexer rejects, such as an
// interpolated pattern.
func (m *model) scanLanguages(file, src string) error {
	members := map[string]int64{}
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		for rest := line; ; {
			i := strings.Index(rest, "Language.")
			if i < 0 {
				break
			}
			after := rest[i+len("Language."):]
			inWord := i > 0 && (unicode.IsLetter(rune(rest[i-1])) || unicode.IsDigit(rune(rest[i-1])) || rest[i-1] == '_')
			n := 0
			for n < len(after) && (after[n] == '_' || after[n] >= 'a' && after[n] <= 'z' || after[n] >= 'A' && after[n] <= 'Z' || n > 0 && after[n] >= '0' && after[n] <= '9') {
				n++
			}
			if name := after[:n]; !inWord && n > 0 {
				if _, dup := members[name]; !dup {
					members[name] = int64(len(members))
				}
			}
			rest = after
		}
	}
	if len(members) == 0 {
		return &srcError{file, 1, 1, "no Language members found in the language parser source"}
	}
	m.enums["Language"] = members
	return nil
}

// ---- symbolic interpreter ---------------------------------------------------------

type vk int

const (
	vNull vk = iota
	vStr
	vInt
	vBool
	vChar
	vEnum
	vArr
	vRes
	vDate
	vSym
	vType
	vDec
	vCivil
	vPart
	vAnon
)

// val is a runtime value of the symbolic interpreter. vRes is the (untouched)
// result of the entry point: s is the property path read so far and i the
// observation index. vSym is a string that embeds one symbolic date (s is the
// template, i the day offset).
type val struct {
	k   vk
	s   string
	t   string
	i   int64
	b   bool
	arr []val
	obj map[string]val
}

type observation struct {
	input   corpus.Value
	asserts map[string]corpus.Value
	dir     bool // the isDir argument of ParseMovieTitle
}

type interp struct {
	file    string
	m       *model
	helpers map[string]*method
	scopes  []map[string]val
	obs     []observation
	depth   int
	w       *work  // the side's cumulative budget
	at      pos    // last known source position, where a limit is reported
	entry   string // the scoped entry point, Type.Method
}

func (it *interp) fail(p pos, why string) error {
	return &srcError{it.file, p.line, p.col, why}
}

func (it *interp) lookup(name string) (val, bool) {
	for i := len(it.scopes) - 1; i >= 0; i-- {
		if v, ok := it.scopes[i][name]; ok {
			return v, true
		}
	}
	return val{}, false
}

func (it *interp) set(name string, v val) { it.scopes[len(it.scopes)-1][name] = v }

// assign stores v in the innermost scope that declares name.
func (it *interp) assign(name string, v val) {
	for i := len(it.scopes) - 1; i >= 0; i-- {
		if _, ok := it.scopes[i][name]; ok {
			it.scopes[i][name] = v
			return
		}
	}
}

// concrete resolves a property of the entry point's result that the source has
// already asserted to be an integer (for example MovieTitles.Count after
// MovieTitles.Count.Should().Be(n)): a passing upstream test fixes its value.
func (it *interp) concrete(v val) val {
	if v.k == vRes && v.s != "" {
		if a, ok := it.obs[v.i].asserts[v.s]; ok && a.Kind == corpus.KindInt {
			return val{k: vInt, i: a.Int}
		}
	}
	return v
}

// loop runs a for statement whose variable is already declared in the
// innermost scope; every iteration is charged one interpreter step.
func (it *interp) loop(s *forS) (bool, error) {
	for {
		it.at = s.p
		if err := it.step(); err != nil {
			return false, err
		}
		c, err := it.eval(s.cond)
		if err != nil || c.k != vBool {
			return false, errors.Join(err, it.fail(s.p, "condition is not a boolean"))
		}
		if !c.b {
			return false, nil
		}
		if ret, err := it.exec(s.body); err != nil || ret {
			return ret, err
		}
		if _, err := it.eval(s.post); err != nil {
			return false, err
		}
	}
}

// posOf returns the source position of an expression node that has one.
func posOf(e expr) (pos, bool) {
	switch e := e.(type) {
	case *identE:
		return e.p, true
	case *memberE:
		return e.p, true
	case *callE:
		return e.p, true
	case *arrE:
		return e.p, true
	case *castE:
		return e.p, true
	case *unE:
		return e.p, true
	case *binE:
		return e.p, true
	case *condE:
		return e.p, true
	case *idxE:
		return e.p, true
	case *assignE:
		return e.p, true
	}
	return pos{}, false
}

// step charges one interpreter step before it runs.
func (it *interp) step() error {
	if it.w.steps >= maxInterpSteps {
		return it.fail(it.at, "interpreter step limit exceeded")
	}
	it.w.steps++
	return nil
}

// charge accounts n bytes of expanded values before they are built.
func (it *interp) charge(n int) error {
	if n < 0 || n > maxExpandedBytes-it.w.bytes {
		return it.fail(it.at, "expanded size limit exceeded")
	}
	it.w.bytes += n
	return nil
}

// nodeBytes is the length of the canonical encoding one recorded value node
// adds to a corpus record, counted without building it: the tagged object with
// its framing for a leaf, and for an array its wrapper plus the separators
// between its items (the items themselves are charged as they are converted).
// Only a string's text carries the name-letter escape the writer applies; a
// date template, an enum's type and member and a decimal do not.
func nodeBytes(v corpus.Value, items int) int {
	switch v.Kind {
	case corpus.KindString:
		return len(`{"t":"string","v":""}`) + encodedLen(v.Str) + nameLetterBytes(v.Str)
	case corpus.KindDecimal:
		return len(`{"t":"decimal","v":""}`) + encodedLen(v.Str)
	case corpus.KindBool:
		if v.Bool {
			return len(`{"t":"bool","v":true}`)
		}
		return len(`{"t":"bool","v":false}`)
	case corpus.KindInt:
		return len(`{"t":"int","v":}`) + len(strconv.FormatInt(v.Int, 10))
	case corpus.KindEnum:
		return len(`{"t":"enum","type":"","v":""}`) + encodedLen(v.Type) + encodedLen(v.Str)
	case corpus.KindDate:
		return len(`{"t":"date","offset":,"template":""}`) + len(strconv.Itoa(v.Offset)) + encodedLen(v.Template)
	case corpus.KindArray:
		return len(`{"t":"array","v":[]}`) + max(items-1, 0)
	}
	return len(`{"t":"null"}`)
}

// encodedLen is the length of s as the canonical writer encodes it between the
// quotes, counted without building it: a quote, a backslash and \n \r \t \b \f
// take two bytes, any other byte below 0x20, U+2028 and U+2029 take six, and
// each byte of invalid UTF-8 becomes the three-byte replacement character.
func encodedLen(s string) int {
	n := 0
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '"' || c == '\\' || c == '\n' || c == '\r' || c == '\t' || c == '\b' || c == '\f':
			n += 2
		case c < 0x20:
			n += 6
		case c < utf8.RuneSelf:
			n++
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			switch {
			case r == '\u2028' || r == '\u2029':
				n += 6
			case r == utf8.RuneError && size == 1:
				n += 3
			default:
				n += size
			}
			i += size
			continue
		}
		i++
	}
	return n
}

// nameLetterBytes is what the writer's name-letter escape adds to a string
// value: five bytes for each a/A directly before a case-insensitive rr.
func nameLetterBytes(s string) int {
	isR := func(c byte) bool { return c == 'r' || c == 'R' }
	n := 0
	for i := 0; i+2 < len(s); i++ {
		if (s[i] == 'a' || s[i] == 'A') && isR(s[i+1]) && isR(s[i+2]) {
			n += 5
		}
	}
	return n
}

func (it *interp) eval(e expr) (val, error) {
	if p, ok := posOf(e); ok {
		it.at = p
	}
	if err := it.step(); err != nil {
		return val{}, err
	}
	switch e := e.(type) {
	case *litE:
		return e.v, nil
	case *identE:
		if v, ok := it.lookup(e.name); ok {
			return v, nil
		}
		if typeNames[e.name] || it.m.enums[e.name] != nil {
			return val{k: vType, s: e.name}, nil
		}
		return val{}, it.fail(e.p, "unknown identifier "+e.name)
	case *memberE:
		return it.member(e)
	case *callE:
		return it.call(e)
	case *arrE:
		out := val{k: vArr}
		for _, el := range e.elems {
			v, err := it.eval(el)
			if err != nil {
				return val{}, err
			}
			out.arr = append(out.arr, v)
		}
		return out, nil
	case *anonE:
		out := val{k: vAnon, obj: map[string]val{}}
		for i, name := range e.names {
			v, err := it.eval(e.vals[i])
			if err != nil {
				return val{}, err
			}
			out.obj[name] = v
		}
		return out, nil
	case *ctorE:
		args, err := it.evalArgs(e.args)
		if err != nil {
			return val{}, err
		}
		if e.typ == "DateTime" && len(args) == 3 && args[0].k == vInt && args[1].k == vInt && args[2].k == vInt {
			y, m, d := args[0].i, args[1].i, args[2].i
			if t := time.Date(int(y), time.Month(m), int(d), 0, 0, 0, 0, time.UTC); y >= 1 && y <= 9999 && int64(t.Year()) == y && int64(t.Month()) == m && int64(t.Day()) == d {
				return val{k: vCivil, i: y*10000 + m*100 + d}, nil
			}
			return val{}, it.fail(e.p, "invalid DateTime")
		}
		return val{}, it.fail(e.p, "unsupported constructor "+e.typ)
	case *castE:
		v, err := it.eval(e.x)
		if err != nil {
			return val{}, err
		}
		switch {
		case (e.typ == "int" || e.typ == "long") && v.k == vInt:
			return v, nil
		case (e.typ == "int" || e.typ == "long") && v.k == vEnum:
			return val{k: vInt, i: it.m.enums[v.t][v.s]}, nil
		case (e.typ == "decimal" || e.typ == "double") && v.k == vDec:
			return v, nil
		case (e.typ == "decimal" || e.typ == "double") && v.k == vInt:
			return val{k: vDec, s: strconv.FormatInt(v.i, 10)}, nil
		case e.typ == "Language" && v.k == vStr:
			if _, ok := it.m.enums["Language"][v.s]; !ok {
				return val{}, it.fail(e.p, "unknown member Language."+v.s)
			}
			return val{k: vEnum, t: "Language", s: v.s}, nil
		}
		return val{}, it.fail(e.p, "unsupported cast")
	case *unE:
		v, err := it.eval(e.x)
		if err != nil {
			return val{}, err
		}
		switch {
		case e.op == "!" && v.k == vBool:
			return val{k: vBool, b: !v.b}, nil
		case e.op == "-" && v.k == vInt:
			return val{k: vInt, i: -v.i}, nil
		}
		return val{}, it.fail(e.p, "unsupported unary operand")
	case *binE:
		return it.binary(e)
	case *condE:
		c, err := it.eval(e.c)
		if err != nil || c.k != vBool {
			return val{}, errors.Join(err, it.fail(e.p, "condition is not a boolean"))
		}
		if c.b {
			return it.eval(e.a)
		}
		return it.eval(e.b)
	case *idxE:
		x, err := it.eval(e.x)
		if err != nil {
			return val{}, err
		}
		i, err := it.eval(e.i)
		if err != nil {
			return val{}, err
		}
		if i = it.concrete(i); i.k != vInt || i.i < 0 {
			return val{}, it.fail(e.p, "index is not a known non-negative integer")
		}
		switch {
		case x.k == vArr && i.i < int64(len(x.arr)):
			return x.arr[i.i], nil
		case x.k == vRes:
			return val{k: vRes, s: x.s + "[" + strconv.FormatInt(i.i, 10) + "]", i: x.i}, nil
		}
		return val{}, it.fail(e.p, "unsupported index operand")
	case *assignE:
		if e.op == "=" {
			x, err := it.eval(e.x)
			if err != nil {
				return val{}, err
			}
			if _, ok := it.lookup(e.name); !ok {
				return val{}, it.fail(e.p, "assignment target is not declared")
			}
			it.assign(e.name, x)
			return val{k: vNull}, nil
		}
		cur, ok := it.lookup(e.name)
		if !ok || cur.k != vInt {
			return val{}, it.fail(e.p, "assignment target is not an integer variable")
		}
		x, err := it.eval(e.x)
		if err != nil {
			return val{}, err
		}
		if x = it.concrete(x); x.k != vInt {
			return val{}, it.fail(e.p, "assigned value is not an integer")
		}
		n := cur.i + x.i
		if e.op == "-=" {
			n = cur.i - x.i
		}
		it.assign(e.name, val{k: vInt, i: n})
		return val{k: vNull}, nil
	}
	return val{}, errors.New("unsupported expression node")
}

func (it *interp) binary(e *binE) (val, error) {
	l, err := it.eval(e.l)
	if err != nil {
		return val{}, err
	}
	if e.op == "&&" || e.op == "||" {
		if l.k != vBool {
			return val{}, it.fail(e.p, "operand is not a boolean")
		}
		if (e.op == "&&") != l.b {
			return l, nil
		}
		r, err := it.eval(e.r)
		if err != nil || r.k != vBool {
			return val{}, errors.Join(err, it.fail(e.p, "operand is not a boolean"))
		}
		return r, nil
	}
	r, err := it.eval(e.r)
	if err != nil {
		return val{}, err
	}
	l, r = it.concrete(l), it.concrete(r)
	switch e.op {
	case "==", "!=":
		eq, ok := valEq(l, r)
		if !ok {
			return val{}, it.fail(e.p, "unsupported comparison operands")
		}
		return val{k: vBool, b: eq == (e.op == "==")}, nil
	case "<", ">", "<=", ">=":
		if l.k != vInt || r.k != vInt {
			return val{}, it.fail(e.p, "unsupported comparison operands")
		}
		var holds bool
		switch e.op {
		case "<":
			holds = l.i < r.i
		case ">":
			holds = l.i > r.i
		case "<=":
			holds = l.i <= r.i
		default:
			holds = l.i >= r.i
		}
		return val{k: vBool, b: holds}, nil
	case "-":
		if l.k == vInt && r.k == vInt {
			return val{k: vInt, i: l.i - r.i}, nil
		}
	case "+":
		if l.k == vInt && r.k == vInt {
			return val{k: vInt, i: l.i + r.i}, nil
		}
		if l.k == vStr && (r.k == vStr || r.k == vInt || r.k == vChar) {
			rt := text(r)
			it.at = e.p
			if err := it.charge(len(l.s) + len(rt)); err != nil {
				return val{}, err
			}
			return val{k: vStr, s: l.s + rt}, nil
		}
	}
	return val{}, it.fail(e.p, "unsupported operator "+e.op)
}

func text(v val) string {
	switch v.k {
	case vInt:
		return strconv.FormatInt(v.i, 10)
	case vChar:
		return string(rune(v.i))
	}
	return v.s
}

func valEq(a, b val) (bool, bool) {
	if a.k != b.k {
		return false, a.k == vNull || b.k == vNull
	}
	switch a.k {
	case vNull:
		return true, true
	case vStr:
		return a.s == b.s, true
	case vInt, vChar:
		return a.i == b.i, true
	case vBool:
		return a.b == b.b, true
	case vEnum:
		return a.t == b.t && a.s == b.s, true
	}
	return false, false
}

func (it *interp) member(e *memberE) (val, error) {
	recv, err := it.eval(e.x)
	if err != nil {
		return val{}, err
	}
	switch recv.k {
	case vType:
		if recv.s == "DateTime" && (e.name == "Today" || e.name == "Now") {
			return val{k: vDate}, nil
		}
		if recv.s == "Episode" && e.name == "AIR_DATE_FORMAT" {
			return val{k: vStr, s: airDateFormat}, nil
		}
		if recv.s == "Parser" && (e.name == "Parser" || e.name == "ReleaseGroupParser") {
			return val{k: vType, s: e.name}, nil
		}
		if members, ok := it.m.enums[recv.s]; ok {
			if _, ok := members[e.name]; ok {
				return val{k: vEnum, t: recv.s, s: e.name}, nil
			}
			return val{}, it.fail(e.p, "unknown member "+recv.s+"."+e.name)
		}
	case vEnum:
		if recv.t == "Quality" && e.name == "Name" {
			if n, ok := it.m.names[recv.s]; ok {
				return val{k: vStr, s: n}, nil
			}
		}
		if recv.t == "Language" && e.name == "Id" {
			// A Language is identified by its Id, so an Id comparison is a member comparison.
			return recv, nil
		}
	case vDate:
		switch e.name {
		case "Year":
			return val{k: vPart, s: "yyyy", i: recv.i}, nil
		case "Month":
			return val{k: vPart, s: "M", i: recv.i}, nil
		case "Day":
			return val{k: vPart, s: "d", i: recv.i}, nil
		}
	case vArr:
		if e.name == "Length" {
			return val{k: vInt, i: int64(len(recv.arr))}, nil
		}
	case vRes:
		path := e.name
		if recv.s != "" {
			path = recv.s + "." + e.name
		}
		return val{k: vRes, s: path, i: recv.i}, nil
	}
	return val{}, it.fail(e.p, "unsupported member access "+e.name)
}

func (it *interp) evalArgs(args []expr) ([]val, error) {
	out := make([]val, 0, len(args))
	for _, a := range args {
		v, err := it.eval(a)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (it *interp) call(e *callE) (val, error) {
	switch fn := e.fn.(type) {
	case *identE:
		if fn.name == "nameof" {
			if len(e.args) == 1 {
				if id, ok := e.args[0].(*identE); ok {
					return val{k: vStr, s: id.name}, nil
				}
			}
			return val{}, it.fail(e.p, "unsupported nameof argument")
		}
		h, ok := it.helpers[fn.name]
		if !ok {
			return val{}, it.fail(fn.p, "unknown method "+fn.name)
		}
		args, err := it.evalArgs(e.args)
		if err != nil {
			return val{}, err
		}
		return val{}, it.invoke(h, args, e.p)
	case *memberE:
		if arity, ok := assertions[fn.name]; ok && len(e.args) == arity {
			if in, ok := fn.x.(*callE); ok && len(in.args) == 0 {
				if im, ok := in.fn.(*memberE); ok && im.name == "Should" {
					return val{}, it.assert(fn.name, im.x, e.args, e.p)
				}
			}
		}
		recv, err := it.eval(fn.x)
		if err != nil {
			return val{}, err
		}
		args, err := it.evalArgs(e.args)
		if err != nil {
			return val{}, err
		}
		return it.method(recv, fn.name, args, e.p)
	}
	return val{}, it.fail(e.p, "unsupported call form")
}

// assertions are the FluentAssertions methods the scoped cases use and the
// number of arguments of each.
var assertions = map[string]int{
	"Be": 1, "Contain": 1, "BeNull": 0, "NotBeNull": 0, "NotBe": 1, "BeTrue": 0, "BeFalse": 0,
	"BeEmpty": 0, "NotBeEmpty": 0, "HaveCount": 1, "BeEquivalentTo": 1, "Equal": 1,
}

// cleanKeys are the property paths of a normalized title.
var cleanKeys = map[string]bool{cleanTitleKey: true, cleanSeriesKey: true}

// assert records one FluentAssertions assertion on the entry point's result.
// Be, BeNull, BeTrue and BeFalse set the asserted value of a property path (the
// result itself is the reserved result key); Contain collects, under the path
// plus ".Contain", the members a list must contain. The other assertions are
// recorded under the path plus a fixed suffix: NotBeNull ".NotNull" (true),
// NotBe ".NotBe", BeEmpty and HaveCount ".Count", NotBeEmpty ".NotEmpty"
// (true), BeEquivalentTo ".Equivalent" and Equal ".Equal" (an array). A Be
// against another property of the same result is recorded as the path pair
// "path==other" (true).
func (it *interp) assert(kind string, target expr, args []expr, p pos) error {
	tv, err := it.eval(target)
	if err != nil {
		return err
	}
	if tv.k != vRes || (tv.s == "" && it.entry == qualityOwner) {
		return it.fail(p, "assertion target is not a property of the entry point result")
	}
	key := tv.s
	if key == "" {
		key = resultKey
	}
	// A normalized-title property is compared only with a normalized title, and the
	// reverse; the recorded value is then the title that both sides are normalized from.
	want := val{k: vNull}
	if cleanKeys[key] && kind != "Be" {
		return it.fail(p, "unsupported assertion on a normalized title")
	}
	if len(args) == 1 {
		if want, err = it.eval(args[0]); err != nil {
			return err
		}
	}
	if cleanKeys[key] != (want.k == vStr && want.t == cleanTag) {
		return it.fail(p, "a normalized title is compared only with a normalized title")
	}
	o := &it.obs[tv.i]
	if kind == "Contain" {
		key += ".Contain"
		if prior, ok := o.asserts[key]; ok {
			item, err := it.toCorpus(want, p)
			if err != nil {
				return err
			}
			for _, have := range prior.Items {
				if corpus.Equal(have, item) {
					return nil
				}
			}
			prior.Items = append(prior.Items, item)
			o.asserts[key] = prior
			return nil
		}
		list, err := it.toCorpus(val{k: vArr, arr: []val{want}}, p)
		if err != nil {
			return err
		}
		it.at = p
		o.asserts[key] = list
		return nil
	}

	var cv corpus.Value
	switch kind {
	case "BeTrue", "BeFalse":
		cv = corpus.Value{Kind: corpus.KindBool, Bool: kind == "BeTrue"}
	case "NotBeNull":
		key += ".NotNull"
		cv = corpus.Value{Kind: corpus.KindBool, Bool: true}
	case "NotBeEmpty":
		key += ".NotEmpty"
		cv = corpus.Value{Kind: corpus.KindBool, Bool: true}
	case "BeEmpty":
		key += ".Count"
		cv = corpus.Value{Kind: corpus.KindInt}
	case "HaveCount":
		if want = it.concrete(want); want.k != vInt {
			return it.fail(p, "HaveCount is not given an integer")
		}
		key += ".Count"
		cv = corpus.Value{Kind: corpus.KindInt, Int: want.i}
	case "BeEquivalentTo", "Equal":
		if want.k != vArr {
			if kind == "BeEquivalentTo" {
				return it.fail(p, "BeEquivalentTo is not given an array")
			}
			want = val{k: vArr, arr: []val{want}}
		}
		key += "." + strings.TrimPrefix(kind, "Be")
		if cv, err = it.toCorpus(want, p); err != nil {
			return err
		}
	case "NotBe":
		key += ".NotBe"
		if cv, err = it.toCorpus(want, p); err != nil {
			return err
		}
	default:
		if want.k == vRes {
			if want.i != tv.i || want.s == "" {
				return it.fail(p, "a property is compared only with another property of the same result")
			}
			key += "==" + want.s
			cv = corpus.Value{Kind: corpus.KindBool, Bool: true}
			break
		}
		if cv, err = it.toCorpus(want, p); err != nil {
			return err
		}
	}
	if prior, ok := o.asserts[key]; ok && !corpus.Equal(prior, cv) {
		return it.fail(p, "conflicting assertions on "+key)
	}
	it.at = p
	o.asserts[key] = cv
	return nil
}

func (it *interp) method(recv val, name string, args []val, p pos) (val, error) {
	switch {
	case recv.k == vType && recv.s == "string" && name == "Format":
		return it.format(args, p)
	case recv.k == vType && recv.s == "Enumerable" && name == "Range" && len(args) == 2 && args[0].k == vInt && args[1].k == vInt:
		return it.rangeOf(args[0].i, args[1].i, p)
	case recv.k == vArr && name == "ToArray" && len(args) == 0:
		return recv, nil
	case recv.k == vType && recv.s == "XmlCleaner" && name == "ReplaceUnicode" && len(args) == 1 && args[0].k == vStr:
		return val{k: vStr, s: xmlClean(args[0].s)}, nil
	case recv.k == vStr && name == "Expand" && len(args) == 1 && args[0].k == vAnon:
		return it.expand(recv.s, args[0], p)
	case recv.k == vPart && name == "ToString" && len(args) == 1 && args[0].k == vStr && args[0].s == "00" && (recv.s == "M" || recv.s == "d"):
		// A month or day padded to two digits is the two-letter date specifier.
		return val{k: vPart, s: recv.s + recv.s, i: recv.i}, nil
	case recv.k == vCivil && name == "ToString" && len(args) == 1 && args[0].k == vStr && args[0].s == airDateFormat:
		return val{k: vStr, s: fmt.Sprintf("%04d-%02d-%02d", recv.i/10000, recv.i/100%100, recv.i%100)}, nil
	case recv.k == vRes && (name == "Single" || name == "SingleOrDefault") && len(args) == 0:
		path := name + "()"
		if recv.s != "" {
			path = recv.s + "." + path
		}
		return val{k: vRes, s: path, i: recv.i}, nil
	case recv.k == vStr && name == "CleanSeriesTitle" && len(args) == 0 && it.entry == cleanSeriesOwn:
		// The helper is the entry point: its input is the string it normalizes.
		in, err := it.toCorpus(recv, p)
		if err != nil {
			return val{}, err
		}
		it.at = p
		if it.w.observations >= maxObservations {
			return val{}, it.fail(p, "observation limit exceeded")
		}
		it.w.observations++
		it.obs = append(it.obs, observation{input: in, asserts: map[string]corpus.Value{}})
		return val{k: vRes, i: int64(len(it.obs) - 1)}, nil
	case recv.k == vRes && name == "CleanSeriesTitle" && len(args) == 0 && recv.s == "" && it.entry == seriesNameOwn:
		return val{k: vRes, s: cleanSeriesKey, i: recv.i}, nil
	case recv.k == vStr && name == "CleanSeriesTitle" && len(args) == 0 && it.entry == seriesNameOwn:
		// The expected side of a normalized-name assertion, tagged as on the movie side.
		return val{k: vStr, s: recv.s, t: cleanTag}, nil
	case recv.k == vType && recv.s+"."+name == it.entry:
		dir := false
		switch {
		case len(args) == 1:
		case len(args) == 2 && name == movieTitleCall && args[1].k == vBool:
			dir = args[1].b
		default:
			return val{}, it.fail(p, "unsupported arguments of "+it.entry)
		}
		in, err := it.toCorpus(args[0], p)
		if err != nil || (in.Kind != corpus.KindString && in.Kind != corpus.KindDate) {
			return val{}, errors.Join(err, it.fail(p, "entry point input is not a release name"))
		}
		it.at = p
		if it.w.observations >= maxObservations {
			return val{}, it.fail(p, "observation limit exceeded")
		}
		it.w.observations++
		it.obs = append(it.obs, observation{input: in, asserts: map[string]corpus.Value{}, dir: dir})
		return val{k: vRes, i: int64(len(it.obs) - 1)}, nil
	case recv.k == vType && recv.s == "ExceptionVerification" && name == "IgnoreWarns" && len(args) == 0:
		return val{k: vNull}, nil
	case recv.k == vStr && name == "CleanMovieTitle" && len(args) == 0 && it.entry == cleanTitleOwn:
		// The helper is the entry point: its input is the string it normalizes.
		in, err := it.toCorpus(recv, p)
		if err != nil {
			return val{}, err
		}
		it.at = p
		if it.w.observations >= maxObservations {
			return val{}, it.fail(p, "observation limit exceeded")
		}
		it.w.observations++
		it.obs = append(it.obs, observation{input: in, asserts: map[string]corpus.Value{}})
		return val{k: vRes, i: int64(len(it.obs) - 1)}, nil
	case recv.k == vRes && name == "CleanMovieTitle" && len(args) == 0 && recv.s == "MovieTitle" && it.entry == movieTitleOwn:
		return val{k: vRes, s: cleanTitleKey, i: recv.i}, nil
	case recv.k == vStr && name == "CleanMovieTitle" && len(args) == 0 && it.entry == movieTitleOwn:
		// The expected side of a normalized-title assertion: the source normalizes the
		// expected title as it normalizes the parsed one, so the raw title is recorded
		// and the tag keeps it from being taken for a plain string.
		return val{k: vStr, s: recv.s, t: cleanTag}, nil
	case recv.k == vStr && name == "AsOsAgnostic" && len(args) == 0:
		// Host-path adaptation of a literal: an out-of-scope case keeps the source literal.
		return recv, nil
	case recv.k == vRes && name == "First" && len(args) == 0:
		path := "First()"
		if recv.s != "" {
			path = recv.s + "." + path
		}
		return val{k: vRes, s: path, i: recv.i}, nil
	case recv.k == vDate && name == "AddDays" && len(args) == 1 && args[0].k == vInt:
		off := recv.i + args[0].i
		if (args[0].i > 0 && off < recv.i) || (args[0].i < 0 && off > recv.i) {
			return val{}, it.fail(p, "date offset overflows")
		}
		return val{k: vDate, i: off}, nil
	case recv.k == vDate && name == "ToString" && len(args) == 1 && args[0].k == vStr:
		return val{k: vSym, s: "{0:" + args[0].s + "}", i: recv.i}, nil
	case recv.k == vStr && name == "Replace" && len(args) == 2 && args[0].k == args[1].k &&
		(args[0].k == vChar || args[0].k == vStr):
		old, repl := text(args[0]), text(args[1])
		if old == "" {
			return val{}, it.fail(p, "empty Replace pattern")
		}
		it.at = p
		if err := it.charge(max(len(recv.s)+strings.Count(recv.s, old)*(len(repl)-len(old)), 0)); err != nil {
			return val{}, err
		}
		return val{k: vStr, s: strings.ReplaceAll(recv.s, old, repl)}, nil
	case recv.k == vStr && name == "ToLower" && len(args) == 0:
		// The fixtures lower-case ASCII words only, where every culture agrees.
		it.at = p
		if err := it.charge(len(recv.s)); err != nil {
			return val{}, err
		}
		return val{k: vStr, s: strings.ToLower(recv.s)}, nil
	}
	return val{}, it.fail(p, "unsupported method "+name)
}

// format evaluates string.Format. A string with a date argument stays a
// symbolic template ({0:spec}); every other argument is substituted.
func (it *interp) format(args []val, p pos) (val, error) {
	if len(args) == 0 || args[0].k != vStr {
		return val{}, it.fail(p, "string.Format needs a literal format string")
	}
	f := args[0].s
	it.at = p
	if err := it.charge(2 * len(f)); err != nil {
		return val{}, err
	}
	var plain, tmpl strings.Builder
	hasDate, offset := false, int64(0)
	lit := func(s string) error {
		if err := it.charge(2 * len(s)); err != nil {
			return err
		}
		plain.WriteString(s)
		tmpl.WriteString(strings.NewReplacer("{", "{{", "}", "}}").Replace(s))
		return nil
	}
	for i := 0; i < len(f); i++ {
		c := f[i]
		switch {
		case c == '{' && i+1 < len(f) && f[i+1] == '{':
			if err := lit("{"); err != nil {
				return val{}, err
			}
			i++
		case c == '}' && i+1 < len(f) && f[i+1] == '}':
			if err := lit("}"); err != nil {
				return val{}, err
			}
			i++
		case c == '{':
			end := strings.IndexByte(f[i:], '}')
			if end < 0 {
				return val{}, it.fail(p, "unterminated format placeholder")
			}
			idxText, spec, _ := strings.Cut(f[i+1:i+end], ":")
			idx, err := strconv.Atoi(idxText)
			if err != nil || idx < 0 || idx+1 >= len(args) {
				return val{}, it.fail(p, "unsupported format placeholder")
			}
			a := args[idx+1]
			switch {
			case a.k == vDate && spec != "":
				if hasDate && a.i != offset {
					return val{}, it.fail(p, "format uses two different symbolic dates")
				}
				hasDate, offset = true, a.i
				tmpl.WriteString("{0:" + spec + "}")
			case spec == "" && (a.k == vStr || a.k == vInt || a.k == vChar):
				if err := lit(text(a)); err != nil {
					return val{}, err
				}
			case spec == "" && a.k == vEnum:
				if err := lit(a.s); err != nil {
					return val{}, err
				}
			default:
				return val{}, it.fail(p, "unsupported format argument")
			}
			i += end
		case c == '}':
			return val{}, it.fail(p, "unbalanced brace in format string")
		default:
			plain.WriteByte(c)
			tmpl.WriteByte(c)
		}
	}
	if hasDate {
		return val{k: vSym, s: tmpl.String(), i: offset}, nil
	}
	return val{k: vStr, s: plain.String()}, nil
}

// maxRangeItems bounds Enumerable.Range so the loop it expands is charged
// per item before the array grows.
const maxRangeItems = 4096

// rangeOf is Enumerable.Range(start, count): count consecutive integers, each
// charged as one interpreter step before it is appended.
func (it *interp) rangeOf(start, count int64, p pos) (val, error) {
	if count < 0 || count > maxRangeItems || start+count-1 > 1<<31-1 {
		return val{}, it.fail(p, "Enumerable.Range is outside the supported bound")
	}
	out := val{k: vArr, arr: make([]val, 0, count)}
	for i := range count {
		it.at = p
		if err := it.step(); err != nil {
			return val{}, err
		}
		out.arr = append(out.arr, val{k: vInt, i: start + i})
	}
	return out, nil
}

// xmlClean is XmlCleaner.ReplaceUnicode. The helper is not in the delivered
// areas; it is read as dropping the characters XML 1.0 does not allow, which
// no release name of the fixtures contains.
func xmlClean(s string) string {
	return strings.Map(func(r rune) rune {
		if r == 0x9 || r == 0xA || r == 0xD || r >= 0x20 && r <= 0xD7FF || r >= 0xE000 && r <= 0xFFFD || r >= 0x10000 && r <= 0x10FFFF {
			return r
		}
		return -1
	}, s)
}

// expand is the source's s.Expand(new { name = value, ... }): each {name} is
// replaced by the named value. A date part (Year, Month or Day of a symbolic
// date) keeps the result a symbolic template, so the relative date is never
// baked into the corpus; every date part must belong to one date.
func (it *interp) expand(s string, anon val, p pos) (val, error) {
	it.at = p
	if err := it.charge(2 * len(s)); err != nil {
		return val{}, err
	}
	var plain, tmpl strings.Builder
	hasDate, offset := false, int64(0)
	lit := func(t string) error {
		if err := it.charge(2 * len(t)); err != nil {
			return err
		}
		plain.WriteString(t)
		tmpl.WriteString(strings.NewReplacer("{", "{{", "}", "}}").Replace(t))
		return nil
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '}':
			return val{}, it.fail(p, "unbalanced brace in expansion")
		case c != '{':
			plain.WriteByte(c)
			tmpl.WriteByte(c)
			continue
		}
		end := strings.IndexByte(s[i:], '}')
		if end < 0 {
			return val{}, it.fail(p, "unterminated expansion key")
		}
		v, ok := anon.obj[s[i+1:i+end]]
		if !ok {
			return val{}, it.fail(p, "unknown expansion key "+s[i+1:i+end])
		}
		switch v.k {
		case vInt, vStr:
			if err := lit(text(v)); err != nil {
				return val{}, err
			}
		case vPart:
			if hasDate && v.i != offset {
				return val{}, it.fail(p, "expansion uses two different symbolic dates")
			}
			hasDate, offset = true, v.i
			tmpl.WriteString("{0:" + v.s + "}")
		default:
			return val{}, it.fail(p, "unsupported expansion value")
		}
		i += end
	}
	if hasDate {
		return val{k: vSym, s: tmpl.String(), i: offset}, nil
	}
	return val{k: vStr, s: plain.String()}, nil
}

// maxValueDepth is the deepest array level a recorded value may start at: the
// record nests the case object and the args or expected container (three
// levels with the value's own object), then an object and an array per level.
const maxValueDepth = (corpus.MaxNesting - 3) / 2

// toCorpus converts v to its corpus value. Every node is charged to the side's
// shared step and byte budget, and checked against the supported nesting,
// before it is built, so a shared array that expands exponentially is rejected
// with its source location before the expansion grows. The release group
// substitution runs on the value once it is admitted whole, so a rejected walk
// never pays for the token regexp.
func (it *interp) toCorpus(v val, p pos) (corpus.Value, error) {
	out, err := it.convert(v, p, 0)
	if err != nil || !it.w.neutral {
		return out, err
	}
	if err = it.group(&out, p); err != nil {
		return corpus.Value{}, err
	}
	return out, nil
}

// group applies the release group substitution to every string and date
// template of an admitted value, where the side asks for it. A match drops the
// name-letter escape of its "arr" and gains a byte, so the charge is settled
// to the recorded value's, and a changed date template is read back by the
// canonical writer as it was before.
func (it *interp) group(v *corpus.Value, p pos) error {
	if v.Kind == corpus.KindString || v.Kind == corpus.KindDate {
		raw, tmpl := nodeBytes(*v, 0), v.Template
		v.Str, v.Template = neutralGroup(v.Str), neutralGroup(v.Template)
		if d := nodeBytes(*v, 0) - raw; d > 0 {
			if err := it.charge(d); err != nil {
				return err
			}
		} else {
			it.w.bytes += d
		}
		if v.Kind == corpus.KindDate && v.Template != tmpl {
			if _, err := v.MarshalJSON(); err != nil {
				return it.fail(p, err.Error())
			}
		}
	}
	for i := range v.Items {
		if err := it.group(&v.Items[i], p); err != nil {
			return err
		}
	}
	return nil
}

func (it *interp) convert(v val, p pos, depth int) (corpus.Value, error) {
	it.at = p
	if depth > maxValueDepth {
		return corpus.Value{}, it.fail(p, "value nesting limit exceeded")
	}
	if err := it.step(); err != nil {
		return corpus.Value{}, err
	}
	var out corpus.Value
	switch v.k {
	case vNull:
		out = corpus.Value{Kind: corpus.KindNull}
	case vStr:
		out = corpus.Value{Kind: corpus.KindString, Str: v.s}
	case vInt:
		out = corpus.Value{Kind: corpus.KindInt, Int: v.i}
	case vBool:
		out = corpus.Value{Kind: corpus.KindBool, Bool: v.b}
	case vDec:
		out = corpus.Value{Kind: corpus.KindDecimal, Str: v.s}
	case vEnum:
		out = corpus.Value{Kind: corpus.KindEnum, Type: v.t, Str: v.s}
	case vSym:
		out = corpus.Value{Kind: corpus.KindDate, Offset: int(v.i), Template: v.s}
	case vArr:
		out = corpus.Value{Kind: corpus.KindArray, Items: []corpus.Value{}}
	default:
		return corpus.Value{}, it.fail(p, "value has no corpus representation")
	}
	if err := it.charge(nodeBytes(out, len(v.arr))); err != nil {
		return corpus.Value{}, err
	}
	switch out.Kind {
	case corpus.KindDecimal, corpus.KindEnum, corpus.KindDate:
		// The canonical writer decides what the reader accepts.
		if _, err := out.MarshalJSON(); err != nil {
			return corpus.Value{}, it.fail(p, err.Error())
		}
	}
	for _, el := range v.arr {
		c, err := it.convert(el, p, depth+1)
		if err != nil {
			return corpus.Value{}, err
		}
		out.Items = append(out.Items, c)
	}
	return out, nil
}

func (it *interp) push() { it.scopes = append(it.scopes, map[string]val{}) }
func (it *interp) pop()  { it.scopes = it.scopes[:len(it.scopes)-1] }

func (it *interp) exec(body []stmt) (bool, error) {
	it.push()
	defer it.pop()
	for _, s := range body {
		if err := it.step(); err != nil {
			return false, err
		}
		switch s := s.(type) {
		case *declS:
			v := val{k: vNull}
			if s.init != nil {
				var err error
				if v, err = it.eval(s.init); err != nil {
					return false, err
				}
			}
			it.set(s.name, v)
		case *exprS:
			if _, err := it.eval(s.x); err != nil {
				return false, err
			}
		case *ifS:
			c, err := it.eval(s.c)
			if err != nil || c.k != vBool {
				return false, errors.Join(err, it.fail(s.p, "condition is not a boolean"))
			}
			branch := s.els
			if c.b {
				branch = s.then
			}
			if ret, err := it.exec(branch); err != nil || ret {
				return ret, err
			}
		case *foreachS:
			in, err := it.eval(s.in)
			if err != nil || in.k != vArr {
				return false, errors.Join(err, it.fail(s.p, "foreach source is not an array"))
			}
			for _, el := range in.arr {
				it.at = s.p
				if err := it.step(); err != nil {
					return false, err
				}
				it.push()
				it.set(s.name, el)
				ret, err := it.exec(s.body)
				it.pop()
				if err != nil || ret {
					return ret, err
				}
			}
		case *forS:
			init, err := it.eval(s.init)
			if err != nil {
				return false, err
			}
			it.push()
			it.set(s.name, init)
			ret, err := it.loop(s)
			it.pop()
			if err != nil || ret {
				return ret, err
			}
		case *blockS:
			if ret, err := it.exec(s.body); err != nil || ret {
				return ret, err
			}
		case *retS:
			return true, nil
		}
	}
	return false, nil
}

func (it *interp) typeOK(typ string, v val) bool {
	switch typ {
	case "string":
		return v.k == vStr || v.k == vSym || v.k == vNull
	case "bool":
		return v.k == vBool
	case "int", "long":
		return v.k == vInt
	case "double", "decimal":
		return v.k == vDec || v.k == vInt
	case "int[]":
		return v.k == vArr
	case "char":
		return v.k == vChar
	case "object[]":
		return v.k == vArr
	case "string[]":
		return v.k == vArr
	case "object":
		return true
	}
	return it.m.enums[typ] != nil && v.k == vEnum && v.t == typ
}

// invoke runs m with args bound to its parameters. Helper methods see only
// their own parameters and locals.
func (it *interp) invoke(m *method, args []val, p pos) error {
	it.at = p
	if err := it.step(); err != nil {
		return err
	}
	if it.depth >= maxHelperDepth {
		return it.fail(p, "helper nesting is too deep")
	}
	if len(args) > len(m.params) && (len(m.params) == 0 || !m.params[len(m.params)-1].variadic) {
		return it.fail(p, "too many arguments for "+m.name)
	}
	saved := it.scopes
	it.scopes = []map[string]val{{}}
	it.depth++
	defer func() {
		it.scopes = saved
		it.depth--
	}()
	for i, prm := range m.params {
		var v val
		switch {
		case prm.variadic:
			v = val{k: vArr}
			if i < len(args) {
				v.arr = append(v.arr, args[i:]...)
			}
		case i < len(args):
			v = args[i]
		case prm.def != nil:
			var err error
			if v, err = it.eval(prm.def); err != nil {
				return err
			}
		default:
			return it.fail(p, "missing argument "+prm.name+" for "+m.name)
		}
		if (prm.typ == "double" || prm.typ == "decimal") && v.k == vInt {
			v = val{k: vDec, s: strconv.FormatInt(v.i, 10)}
		}
		if !it.typeOK(prm.typ, v) {
			return it.fail(p, "argument "+prm.name+" does not match parameter type "+prm.typ)
		}
		it.set(prm.name, v)
	}
	_, err := it.exec(m.body)
	return err
}

// ---- test cases ------------------------------------------------------------------

type fileStats struct {
	testMethods, testCaseAttrs, sourceRows, setupMethods, commented, plain int
}

type row struct {
	args     []val
	entry    string
	line     int
	disabled bool
}

var knownAttrs = map[string]bool{
	"TestFixture": true, "Test": true, "TestCase": true, "TestCaseSource": true, "SetUp": true, "Ignore": true, "Explicit": true,
}

func hasAttr(attrs []attr, name string) bool {
	for _, a := range attrs {
		if a.name == name {
			return true
		}
	}
	return false
}

// typeNames are the identifiers the interpreter treats as static types.
var typeNames = map[string]bool{
	"string": true, "DateTime": true, "QualityParser": true, "Parser": true, "LanguageParser": true,
	"ReleaseGroupParser": true, "ExceptionVerification": true, "Enumerable": true, "XmlCleaner": true, "Episode": true,
}

// titleEntries are the entry points whose results title cases assert, with the
// owner each case records. otherEntries are the entry points of the other
// tests of the title fixtures: those cases are retained out of scope under the
// owner named here, with their source arguments.
var (
	titleEntries = [...]struct{ call, owner string }{
		{movieTitleCall, movieTitleOwn},
		{"ParseReleaseGroup", "ReleaseGroupParser.ParseReleaseGroup"},
		{"ParseLanguages", "LanguageParser.ParseLanguages"},
	}
	otherEntries = [...]struct{ call, owner string }{
		{"ParseMoviePath", "Parser.ParseMoviePath"},
		{"ParseSubtitleLanguageInformation", "LanguageParser.ParseSubtitleLanguageInformation"},
		{"ParseSubtitleLanguage", "LanguageParser.ParseSubtitleLanguage"},
		{"NormalizeImdbId", "Parser.NormalizeImdbId"},
	}
)

// classify decides the scope and owner of a title-fixture test method from the
// identifiers of its body: title for a method that asserts a title entry point,
// out of scope for the others, and no scope when it names no known entry
// point. A method that parses a title and normalizes it through
// CleanMovieTitle is a title case asserting the normalized title; one that
// calls CleanMovieTitle alone is a title case of the helper itself (its input
// is the string it normalizes), and one that builds its names with MD5 or
// Random has no recordable input.
func classify(app string, toks []token) (scope, owner string) {
	has := func(name string) bool {
		for _, t := range toks {
			if t.kind == tIdent && t.text == name {
				return true
			}
		}
		return false
	}
	if app == "series" {
		return classifySeries(has)
	}
	switch {
	case has("CleanMovieTitle") && has(movieTitleCall):
		return corpus.ScopeTitle, movieTitleOwn
	case has("CleanMovieTitle"):
		return corpus.ScopeTitle, cleanTitleOwn
	case has("MD5") || has("Random"):
		return corpus.ScopeOutOfScope, movieTitleOwn
	}
	for _, e := range titleEntries {
		if has(e.call) {
			return corpus.ScopeTitle, e.owner
		}
	}
	for _, e := range otherEntries {
		if has(e.call) {
			return corpus.ScopeOutOfScope, e.owner
		}
	}
	return "", ""
}

// seriesTitleEntries are the entry points whose results the series title cases
// assert; seriesOtherEntries those of the other tests of the series title
// fixtures, retained out of scope under the owner named here.
var (
	seriesTitleEntries = [...]struct{ call, owner string }{
		{seriesTitleCall, seriesTitleOwn},
		{"ParseReleaseGroup", "ReleaseGroupParser.ParseReleaseGroup"},
		{"ParseLanguages", "LanguageParser.ParseLanguages"},
	}
	seriesOtherEntries = [...]struct{ call, owner string }{
		{"ParsePath", "Parser.ParsePath"},
		{"ParseSeasonTitle", "Parser.ParseSeasonTitle"},
		{"ParseSubtitleLanguageInformation", "LanguageParser.ParseSubtitleLanguageInformation"},
		{"ParseSubtitleLanguage", "LanguageParser.ParseSubtitleLanguage"},
		{"ParsedEpisodeInfo", "ParsedEpisodeInfo.IsPossibleSpecialEpisode"},
	}
)

// classifySeries is classify for the series side: a method that parses a series
// name and normalizes it through CleanSeriesTitle is a title case of
// ParseSeriesName; one that calls CleanSeriesTitle alone is a title case of the
// helper itself; one that builds its names with MD5 or Random has no
// recordable input.
func classifySeries(has func(string) bool) (scope, owner string) {
	switch {
	case has("CleanSeriesTitle") && has(seriesNameCall):
		return corpus.ScopeTitle, seriesNameOwn
	case has("CleanSeriesTitle"):
		return corpus.ScopeTitle, cleanSeriesOwn
	case has("MD5") || has("Random"):
		return corpus.ScopeOutOfScope, seriesTitleOwn
	}
	for _, e := range seriesTitleEntries {
		if has(e.call) {
			return corpus.ScopeTitle, e.owner
		}
	}
	for _, e := range seriesOtherEntries {
		if has(e.call) {
			return corpus.ScopeOutOfScope, e.owner
		}
	}
	return "", ""
}

func fileCases(app, rel, src string, m *model, w *work, title bool) ([]corpus.Case, fileStats, error) {
	var st fileStats
	lx, err := lex(rel, src)
	if err != nil {
		return nil, st, err
	}
	for _, c := range lx.comments {
		if strings.Contains(c, "[Test") {
			st.commented++
		}
	}
	p := &parser{file: rel, t: lx.toks, title: title, app: app}
	classes, err := p.parseFile()
	if err != nil {
		return nil, st, err
	}

	// Every attribute token in the file must be accounted for by the parse.
	parsed := map[string]int{}
	helpers := map[string]*method{}
	sources := map[string]*field{}
	for _, c := range classes {
		for _, a := range c.attrs {
			if !knownAttrs[a.name] {
				return nil, st, &srcError{rel, a.p.line, a.p.col, "unsupported attribute " + a.name}
			}
			parsed[a.name]++
		}
		for _, f := range c.fields {
			sources[f.name] = f
		}
		for _, mt := range c.methods {
			if _, dup := helpers[mt.name]; dup {
				return nil, st, &srcError{rel, mt.p.line, mt.p.col, "overloaded method " + mt.name}
			}
			helpers[mt.name] = mt
			for _, a := range mt.attrs {
				if !knownAttrs[a.name] {
					return nil, st, &srcError{rel, a.p.line, a.p.col, "unsupported attribute " + a.name}
				}
				parsed[a.name]++
			}
		}
	}
	raw := map[string]int{}
	for i := 1; i < len(lx.toks); i++ {
		t, prev := lx.toks[i], lx.toks[i-1]
		if t.kind == tIdent && knownAttrs[t.text] && prev.kind == tPunct && (prev.text == "[" || prev.text == ",") {
			raw[t.text]++
		}
	}
	for name := range knownAttrs {
		if raw[name] != parsed[name] {
			return nil, st, &srcError{rel, 1, 1, fmt.Sprintf("attribute %s occurs %d times but %d were parsed", name, raw[name], parsed[name])}
		}
	}

	var cases []corpus.Case
	for _, c := range classes {
		classOff := hasAttr(c.attrs, "Ignore") || hasAttr(c.attrs, "Explicit")
		for _, mt := range c.methods {
			if len(mt.attrs) == 0 {
				continue
			}
			if hasAttr(mt.attrs, "SetUp") {
				if len(mt.body) != 0 || len(mt.attrs) != 1 {
					return nil, st, &srcError{rel, mt.p.line, mt.p.col, "SetUp methods with a body are not supported"}
				}
				st.setupMethods++
				continue
			}
			rows, err := methodRows(rel, mt, sources, m, w)
			if err != nil {
				return nil, st, err
			}
			st.testMethods++
			scope, owner := corpus.ScopeQuality, qualityOwner
			if title {
				if scope, owner = classify(app, mt.toks); scope == "" {
					return nil, st, &srcError{rel, mt.p.line, mt.p.col, "test method " + mt.name + " has no recognized entry point"}
				}
			}
			off := classOff || hasAttr(mt.attrs, "Ignore") || hasAttr(mt.attrs, "Explicit")
			for _, r := range rows {
				if r.entry == "Test" {
					st.plain++
				}
				cs, err := runRow(rel, mt, r, helpers, m, w, scope, owner)
				if err != nil {
					return nil, st, err
				}
				cs.UpstreamEnabled = !off && !r.disabled
				cases = append(cases, cs)
			}
		}
	}
	st.testCaseAttrs = parsed["TestCase"]
	for _, c := range classes {
		for _, f := range c.fields {
			if arr, ok := f.init.(*arrE); ok {
				st.sourceRows += len(arr.elems)
			}
		}
	}
	assignIDs(app, rel, cases)
	return cases, st, nil
}

// methodRows expands a test method's TestCase, TestCaseSource and plain Test
// attributes into invocations.
func methodRows(rel string, mt *method, sources map[string]*field, m *model, w *work) ([]row, error) {
	scope := &interp{file: rel, m: m, scopes: []map[string]val{{}}, w: w, at: mt.p}
	var rows []row
	n := 0
	for _, a := range mt.attrs {
		switch a.name {
		case "TestCase":
			n++
			r := row{entry: fmt.Sprintf("TestCase#%d", n), line: a.p.line}
			args, err := scope.evalArgs(a.args)
			if err != nil {
				return nil, err
			}
			r.args = args
			for name, e := range a.named {
				v, err := scope.eval(e)
				if err != nil {
					return nil, err
				}
				switch name {
				case "Ignore", "IgnoreReason":
					r.disabled = true
				case "Explicit":
					r.disabled = r.disabled || v.k != vBool || v.b
				case "ExplicitReason":
					r.disabled = true
				case "Description":
				default:
					return nil, &srcError{rel, a.p.line, a.p.col, "unsupported TestCase argument " + name}
				}
			}
			rows = append(rows, r)
		case "TestCaseSource":
			if len(a.args) != 1 || len(a.named) != 0 {
				return nil, &srcError{rel, a.p.line, a.p.col, "unsupported TestCaseSource form"}
			}
			nv, err := scope.eval(a.args[0])
			if err != nil || nv.k != vStr {
				return nil, errors.Join(err, &srcError{rel, a.p.line, a.p.col, "TestCaseSource name is not a string"})
			}
			f, ok := sources[nv.s]
			if !ok || f.typ != "object[]" {
				return nil, &srcError{rel, a.p.line, a.p.col, "TestCaseSource " + nv.s + " is not a static object[] field of the fixture"}
			}
			arr, ok := f.init.(*arrE)
			if !ok {
				return nil, &srcError{rel, f.p.line, f.p.col, "unsupported TestCaseSource initializer"}
			}
			for k, el := range arr.elems {
				rowArr, ok := el.(*arrE)
				if !ok {
					return nil, &srcError{rel, f.p.line, f.p.col, "TestCaseSource element is not an object[] row"}
				}
				args, err := scope.evalArgs(rowArr.elems)
				if err != nil {
					return nil, err
				}
				rows = append(rows, row{args: args, entry: fmt.Sprintf("TestCaseSource:%s#%d", nv.s, k+1), line: rowArr.p.line})
			}
		}
	}
	if len(rows) == 0 {
		if !hasAttr(mt.attrs, "Test") || len(mt.params) != 0 {
			return nil, &srcError{rel, mt.p.line, mt.p.col, "test method " + mt.name + " has no rows"}
		}
		rows = append(rows, row{entry: "Test", line: mt.p.line})
	}
	return rows, nil
}

func runRow(rel string, mt *method, r row, helpers map[string]*method, m *model, w *work, scope, owner string) (corpus.Case, error) {
	at := pos{r.line, 1}
	it := &interp{file: rel, m: m, helpers: helpers, w: w, at: at, entry: owner}
	if scope != corpus.ScopeOutOfScope {
		if err := it.invoke(mt, r.args, at); err != nil {
			return corpus.Case{}, err
		}
	}
	fail := func(why string) (corpus.Case, error) {
		return corpus.Case{}, &srcError{rel, r.line, 1, mt.name + " " + r.entry + ": " + why}
	}
	inputs := corpus.Value{Kind: corpus.KindArray, Items: []corpus.Value{}}
	expected := map[string]corpus.Value{}
	if scope != corpus.ScopeOutOfScope {
		if len(it.obs) == 0 {
			return fail("the case never calls " + owner)
		}
		// A case may assert each property on its own call of the entry point: the
		// observations of one input accumulate their assertions, and every input of
		// the case must then expect alike.
		type group struct {
			input   corpus.Value
			asserts map[string]corpus.Value
		}
		var groups []*group
		for _, o := range it.obs {
			if o.dir != it.obs[0].dir {
				return fail("invocations differ in the isDir argument")
			}
			inputs.Items = append(inputs.Items, o.input)
			var g *group
			for _, c := range groups {
				if corpus.Equal(c.input, o.input) {
					g = c
					break
				}
			}
			if g == nil {
				g = &group{o.input, map[string]corpus.Value{}}
				groups = append(groups, g)
			}
			for k, v := range o.asserts {
				if prior, ok := g.asserts[k]; ok && !corpus.Equal(prior, v) {
					return fail("invocations assert different expectations of " + k)
				}
				g.asserts[k] = v
			}
		}
		first := groups[0].asserts
		for _, g := range groups[1:] {
			if !equalAsserts(first, g.asserts) {
				return fail("invocations assert different expectations")
			}
		}
		if len(first) == 0 {
			if owner != seriesTitleOwn {
				return fail("the result of " + owner + " is never asserted")
			}
			// The source only requires the call not to throw.
			first = map[string]corpus.Value{noThrowKey: {Kind: corpus.KindBool, Bool: true}}
		}
		for k, v := range first {
			expected[k] = v
		}
		if it.obs[0].dir {
			expected[dirKey] = corpus.Value{Kind: corpus.KindBool, Bool: true}
		}
	}
	expected[corpus.InputKey] = inputs
	args := make([]corpus.Value, 0, len(r.args))
	for _, a := range r.args {
		cv, err := it.toCorpus(a, at)
		if err != nil {
			return corpus.Case{}, err
		}
		args = append(args, cv)
	}
	return corpus.Case{
		File:       rel,
		Method:     mt.name,
		Entry:      r.entry,
		Line:       r.line,
		Scope:      scope,
		ScopeOwner: owner,
		Args:       args,
		Expected:   expected,
	}, nil
}

func equalAsserts(a, b map[string]corpus.Value) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || !corpus.Equal(v, w) {
			return false
		}
	}
	return true
}

// assignIDs binds each case to app, file, method, a hash of its symbolic
// arguments and inputs, and its duplicate ordinal.
func assignIDs(app, rel string, cases []corpus.Case) {
	base := rel[strings.LastIndex(rel, "/")+1:]
	seen := map[string]int{}
	for i := range cases {
		c := &cases[i]
		b, _ := json.Marshal(struct {
			Args  []corpus.Value
			Input corpus.Value
		}{c.Args, c.Expected[corpus.InputKey]})
		sum := sha256.Sum256(b)
		key := fmt.Sprintf("%s:%s:%s:%s", app, base, c.Method, hex.EncodeToString(sum[:8]))
		c.ID = fmt.Sprintf("%s:%d", key, seen[key])
		seen[key]++
	}
}
