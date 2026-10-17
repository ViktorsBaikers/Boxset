// SPDX-License-Identifier: GPL-3.0

package parse

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf16"

	"github.com/dlclark/regexp2/v2"
)

// MovieInfo is the parsed movie title of a release name. Strings and lists
// follow the source's ParsedMovieInfo: empty means the source-native absence
// (a null string and an empty one are not distinguished), lists are empty when
// the source returns none, Year and TmdbID are 0 when unknown. Languages holds
// stable Boxset language codes from the explicit map of vocab.go. The primary
// title is the first MovieTitles item, never a second stored field.
type MovieInfo struct {
	MovieTitles        []string
	Languages          []string
	OriginalTitle      string
	ReleaseTitle       string
	SimpleReleaseTitle string
	ReleaseGroup       string
	ReleaseHash        string
	Edition            string
	ImdbID             string
	HardcodedSubs      string
	Quality            Quality
	Year               int
	TmdbID             int
}

// ParsedTitle is the result of ParseTitle: exactly one kind-specific value
// is set on success.
type ParsedTitle struct {
	Kind  Kind
	Movie *MovieInfo
}

// ParseTitle parses a release name as a title of kind. The name is validated
// (at most 4096 UTF-8 bytes, valid UTF-8) and the kind checked before any
// matching; the whole parse, including the quality, language and release
// group parsing it runs, is then one operation under one 250 ms elapsed-work
// budget, checked once more before the result is returned. KindMovie ports the
// movie reference's ParseMovieTitle; a name it cannot parse is a
// title_unrecognized *ReasonError, and KindSeries has no title parser yet and
// is a kind_unsupported *ReasonError. On any error the ParsedTitle is the zero
// value.
func (p *Parser) ParseTitle(ctx context.Context, name string, kind Kind) (ParsedTitle, error) {
	return p.parseTitle(ctx, name, kind, false)
}

// parseTitle is ParseTitle with the source's isDir argument of
// ParseMovieTitle: a directory name also tries the year-first folder pattern.
func (p *Parser) parseTitle(ctx context.Context, name string, kind Kind, isDir bool) (ParsedTitle, error) {
	if kind != KindMovie {
		return ParsedTitle{}, newReasonError(codeKindUnsupported, map[string]int64{"kind": int64(kind)})
	}
	if err := validateName(name); err != nil {
		return ParsedTitle{}, err
	}
	b := newBudget(ctx, p.now)
	if err := b.check(); err != nil {
		return ParsedTitle{}, err
	}
	info, err := p.movieTitle.parse(b, name, isDir)
	if err != nil {
		return ParsedTitle{}, err
	}
	// The budget and context are checked once more before any usable value
	// leaves, so a result that completed after expiry or cancellation (and a
	// name nothing matched) is an error, never a pass.
	if err := b.check(); err != nil {
		return ParsedTitle{}, err
	}
	if info == nil {
		return ParsedTitle{}, newReasonError(codeTitleUnrecognized, nil)
	}
	return ParsedTitle{Kind: KindMovie, Movie: info}, nil
}

// ---- budgeted replacement and split of the source's Regex API -----------------

// replace is Regex.Replace with a replacement pattern. The budget is checked
// before and after, so a late success after expiry is an error, not a pass.
func (b *budget) replace(re *regexp2.Regexp, s, replacement string) (string, error) {
	if err := b.check(); err != nil {
		return "", err
	}
	out, err := re.Replace(s, replacement, -1, -1)
	if err != nil {
		return "", b.failed(err)
	}
	if err := b.check(); err != nil {
		return "", err
	}
	return out, nil
}

// replaceFunc is Regex.Replace with a match evaluator. The evaluator cannot
// fail: its first error is kept in *evalErr (the evaluator returns the match
// unchanged after it) and surfaces after the replacement.
func (b *budget) replaceFunc(re *regexp2.Regexp, s string, evalErr *error, fn func(regexp2.Match) string) (string, error) {
	if err := b.check(); err != nil {
		return "", err
	}
	out, err := re.ReplaceFunc(s, fn, -1, -1)
	if *evalErr != nil {
		return "", *evalErr
	}
	if err != nil {
		return "", b.failed(err)
	}
	if err := b.check(); err != nil {
		return "", err
	}
	return out, nil
}

// split is Regex.Split for a pattern without capture groups: the text between
// successive matches, with the empty edges the source keeps.
func (b *budget) split(re *regexp2.Regexp, s string) ([]string, error) {
	var parts []string
	prev := 0
	m, err := b.first(re, s)
	for m != nil && err == nil {
		at, n := m.ByteRange()
		parts = append(parts, s[prev:at])
		prev = at + n
		m, err = b.next(re, m)
	}
	if err != nil {
		return nil, err
	}
	return append(parts, s[prev:]), nil
}

// ---- string helpers of the source's extension methods --------------------------

// groupText is Match.Groups[name].Value: the text of the group's last capture,
// empty when the group did not take part.
func groupText(m *regexp2.Match, name string) string {
	if !groupOK(m, name) {
		return ""
	}
	return m.GroupByName(name).String()
}

// utf16Len is string.Length: the length of s in UTF-16 code units.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r >= 0x10000 {
			n++
		}
	}
	return n
}

// utf16Suffix is s.Substring(n) over UTF-16 code units.
func utf16Suffix(s string, n int) string {
	units := utf16.Encode([]rune(s))
	return string(utf16.Decode(units[min(n, len(units)):]))
}

// replaceUTF16 is s.Remove(index, length).Insert(index, insert) over UTF-16
// code units. ok is false where the source throws: the range is outside s.
func replaceUTF16(s string, index, length int, insert string) (string, bool) {
	units := utf16.Encode([]rune(s))
	if index < 0 || length < 0 || index+length > len(units) {
		return "", false
	}
	out := append([]uint16{}, units[:index]...)
	out = append(out, utf16.Encode([]rune(insert))...)
	out = append(out, units[index+length:]...)
	return string(utf16.Decode(out)), true
}

// tryParseInt is int.TryParse(string, out int) under the invariant culture: an
// optional sign and ASCII digits between optional ASCII white space, within
// the int32 range. Other Unicode digits, empty text and overflow do not parse.
func tryParseInt(s string) (int, bool) {
	s = strings.Trim(s, "\t\n\v\f\r ")
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}
	if s == "" {
		return 0, false
	}
	var n int64
	for _, c := range []byte(s) {
		if c < '0' || c > '9' {
			return 0, false
		}
		if n = n*10 + int64(c-'0'); n > 1<<31 {
			return 0, false
		}
	}
	if neg {
		n = -n
	}
	if n > 1<<31-1 || n < -(1<<31) {
		return 0, false
	}
	return int(n), true
}

// hasLetterOrDigit is title.Any(char.IsLetterOrDigit): the source tests UTF-16
// code units, so a code point outside the BMP (a surrogate pair) is neither.
func hasLetterOrDigit(s string) bool {
	for _, r := range s {
		if r < 0x10000 && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			return true
		}
	}
	return false
}
