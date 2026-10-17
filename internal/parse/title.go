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

// SeriesTitleInfo is the parsed series title of a release name: the title as
// parsed, the same without its year, the year (0 when the title has none) and
// every alternative title the name lists (empty when it lists one).
type SeriesTitleInfo struct {
	Title            string
	TitleWithoutYear string
	Year             int
	AllTitles        []string
}

// ReleaseType is the kind of release an EpisodeInfo describes.
type ReleaseType uint8

const (
	ReleaseTypeUnknown ReleaseType = iota
	ReleaseTypeSingleEpisode
	ReleaseTypeMultiEpisode
	ReleaseTypeSeasonPack
)

// EpisodeInfo is the parsed series title of a release name. Strings and lists
// follow the source's ParsedEpisodeInfo: empty means the source-native absence
// (a null string and an empty one are not distinguished), lists are empty when
// the source returns none. Languages holds stable Boxset language codes from the
// explicit map of vocab.go. AirDate is the source's date-only text of the air
// date, never a UTC instant. SpecialAbsoluteEpisodeNumbers are exact decimal
// text, never floats. The first season (SeasonNumber) and the predicates below
// derive from the stored fields; none is stored twice.
type EpisodeInfo struct {
	ReleaseTitle                  string
	SeriesTitle                   string
	AirDate                       string
	ReleaseGroup                  string
	ReleaseHash                   string
	ReleaseTokens                 string
	SeriesTitleInfo               SeriesTitleInfo
	Quality                       Quality
	SeasonNumbers                 []int
	EpisodeNumbers                []int
	AbsoluteEpisodeNumbers        []int
	SpecialAbsoluteEpisodeNumbers []string
	Languages                     []string
	FullSeason                    bool
	IsPartialSeason               bool
	IsSeasonExtra                 bool
	IsSeasonTitle                 bool
	IsSplitEpisode                bool
	IsMiniSeries                  bool
	Special                       bool
	SeasonPart                    int
	DailyPart                     *int
}

// SeasonNumber is the first season, or false when there is none.
func (e *EpisodeInfo) SeasonNumber() (int, bool) {
	if len(e.SeasonNumbers) == 0 {
		return 0, false
	}
	return e.SeasonNumbers[0], true
}

// IsMultiSeason reports whether the release spans more than one season.
func (e *EpisodeInfo) IsMultiSeason() bool { return len(e.SeasonNumbers) > 1 }

// IsDaily reports whether the release has an air date.
func (e *EpisodeInfo) IsDaily() bool { return !isBlank(e.AirDate) }

// IsAbsoluteNumbering reports whether the release has absolute episode numbers.
func (e *EpisodeInfo) IsAbsoluteNumbering() bool { return len(e.AbsoluteEpisodeNumbers) > 0 }

// IsPossibleSpecialEpisode is the source's predicate: a release without air
// date or series title and without episodes or in season 0, one with a series
// title and the special flag, or the single episode 0.
func (e *EpisodeInfo) IsPossibleSpecialEpisode() bool {
	season, hasSeason := e.SeasonNumber()
	return (isBlank(e.AirDate) && isBlank(e.SeriesTitle) && (len(e.EpisodeNumbers) == 0 || hasSeason && season == 0)) ||
		(!isBlank(e.SeriesTitle) && e.Special) ||
		(len(e.EpisodeNumbers) == 1 && e.EpisodeNumbers[0] == 0)
}

// IsPossibleSceneSeasonSpecial reports whether the release is episode 0 of a
// season other than 0.
func (e *EpisodeInfo) IsPossibleSceneSeasonSpecial() bool {
	season, hasSeason := e.SeasonNumber()
	return hasSeason && season != 0 && len(e.EpisodeNumbers) == 1 && e.EpisodeNumbers[0] == 0
}

// ReleaseType is the kind of release: multi-episode, single episode, season
// pack, or unknown.
func (e *EpisodeInfo) ReleaseType() ReleaseType {
	switch {
	case len(e.EpisodeNumbers) > 1 || len(e.AbsoluteEpisodeNumbers) > 1:
		return ReleaseTypeMultiEpisode
	case len(e.EpisodeNumbers) == 1 || len(e.AbsoluteEpisodeNumbers) == 1:
		return ReleaseTypeSingleEpisode
	case e.FullSeason:
		return ReleaseTypeSeasonPack
	}
	return ReleaseTypeUnknown
}

// ParsedTitle is the result of ParseTitle: exactly one kind-specific value
// is set on success.
type ParsedTitle struct {
	Kind    Kind
	Movie   *MovieInfo
	Episode *EpisodeInfo
}

// ParseTitle parses a release name as a title of kind. The name is validated
// (at most 4096 UTF-8 bytes, valid UTF-8) and the kind checked before any
// matching; the whole parse, including the quality, language and release
// group parsing it runs, is then one operation under one 250 ms elapsed-work
// budget, checked once more before the result is returned. KindMovie ports the
// movie reference's ParseMovieTitle and KindSeries the series reference's
// ParseTitle; a name it cannot parse is a title_unrecognized *ReasonError. On
// any error the ParsedTitle is the zero value.
func (p *Parser) ParseTitle(ctx context.Context, name string, kind Kind) (ParsedTitle, error) {
	return p.parseTitle(ctx, name, kind, false)
}

// parseTitle is ParseTitle with the source's isDir argument of
// ParseMovieTitle: a directory name also tries the year-first folder pattern.
func (p *Parser) parseTitle(ctx context.Context, name string, kind Kind, isDir bool) (ParsedTitle, error) {
	if kind != KindMovie && kind != KindSeries {
		return ParsedTitle{}, newReasonError(codeKindUnsupported, map[string]int64{"kind": int64(kind)})
	}
	if err := validateName(name); err != nil {
		return ParsedTitle{}, err
	}
	b := newBudget(ctx, p.now)
	if err := b.check(); err != nil {
		return ParsedTitle{}, err
	}
	var (
		movie   *MovieInfo
		episode *EpisodeInfo
		err     error
	)
	if kind == KindMovie {
		movie, err = p.movieTitle.parse(b, name, isDir)
	} else {
		episode, err = p.seriesTitle.parse(b, p.location, name)
	}
	if err != nil {
		return ParsedTitle{}, err
	}
	// The budget and context are checked once more before any usable value
	// leaves, so a result that completed after expiry or cancellation (and a
	// name nothing matched) is an error, never a pass.
	if err := b.check(); err != nil {
		return ParsedTitle{}, err
	}
	if movie == nil && episode == nil {
		return ParsedTitle{}, newReasonError(codeTitleUnrecognized, nil)
	}
	return ParsedTitle{Kind: kind, Movie: movie, Episode: episode}, nil
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
