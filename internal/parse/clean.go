// SPDX-License-Identifier: GPL-3.0

package parse

import (
	"strconv"
	"strings"
	"unicode"

	"github.com/dlclark/regexp2/v2"
)

// CleanMovieTitle is the public movie-title normalizer named by the fixture
// table (Parser.CleanMovieTitle). A blank or all-digit title is unchanged;
// otherwise the title is lower-cased, German umlauts are spelled out and
// accents are removed.
func (p *Parser) CleanMovieTitle(title string) (string, error) {
	if err := validateName(title); err != nil {
		return "", err
	}
	return cleanMovieTitle(title)
}

// CleanSeriesTitle is the public series-title normalizer named by the fixture
// table (Parser.CleanSeriesTitle). A blank or all-digit title is unchanged;
// otherwise a percent sign after digits reads "percent", and the title is
// lower-cased and stripped of accents.
func (p *Parser) CleanSeriesTitle(title string) (string, error) {
	if err := validateName(title); err != nil {
		return "", err
	}
	return cleanSeriesTitle(title)
}

// cleanMovieTitleRe is the normalization pattern: the common words (a, an,
// the, and, or, of) that are neither first, last nor part of an acronym, and
// every non-word character and underscore.
var cleanMovieTitleRe = func() *regexp2.Regexp {
	re, err := regexp2.Compile(`((?:\b|_)(?<!^|[^a-zA-Z0-9_']\w[^a-zA-Z0-9_'])([aà](?!$|[^a-zA-Z0-9_']\w[^a-zA-Z0-9_'])|an|the|and|or|of)(?!$)(?:\b|_))|\W|_`, ic)
	if err != nil {
		panic(err)
	}
	re.MatchTimeout = matchTimeout
	return re
}()

var germanUmlauts = strings.NewReplacer("ä", "ae", "ö", "oe", "ü", "ue", "Ä", "Ae", "Ö", "Oe", "Ü", "Ue", "ß", "ss")

// accentFold maps each lower-case letter of Latin-1 Supplement and Latin
// Extended-A that has a canonical decomposition to its base letter, and ð to d
// (it has no decomposition, but a fixture asserts that "Ríkið" cleans to
// "rikid"). The other letters without a decomposition (ø, æ, œ, ł, đ, ħ, ŋ, ŧ,
// ı, ĳ, ĸ, ŀ, ŉ, ſ) are deliberately absent: normalization form D leaves them
// as they are.
var accentFold = func() *strings.Replacer {
	var pairs []string
	for base, accented := range map[string]string{
		"a": "àáâãåāăą", "c": "çćĉċč", "d": "ďð", "e": "èéêëēĕėęě", "g": "ĝğġģ", "h": "ĥ", "i": "ìíîïĩīĭį",
		"j": "ĵ", "k": "ķ", "l": "ĺļľ", "n": "ñńņň", "o": "òóôõōŏő", "r": "ŕŗř", "s": "śŝşš", "t": "ţť",
		"u": "ùúûũūŭůűų", "w": "ŵ", "y": "ýÿŷ", "z": "źżž",
	} {
		for _, r := range accented {
			pairs = append(pairs, string(r), base)
		}
	}
	return strings.NewReplacer(pairs...)
}()

// removeAccent removes accents (decompose, drop the non-spacing
// marks, recompose) for lower-case text, with the standard library only: the
// letters of accentFold lose their accent and a combining mark of already
// decomposed text is dropped. The standard library has no decomposition table
// beyond that, so any other non-ASCII letter is an error: it might carry a
// decomposition, and a silently unfolded title would be wrong.
func removeAccent(s string) (string, error) {
	s = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Mn, r) {
			return -1
		}
		return r
	}, accentFold.Replace(s))
	for _, r := range s {
		if r > unicode.MaxASCII {
			return "", newReasonError(codeAccentUnhandled, map[string]int64{"codepoint": int64(r)})
		}
	}
	return s, nil
}

// cleanMovieTitle normalizes a title: a blank or all-digit title
// is returned as it is, otherwise the normalized title is lower-cased, its
// German umlauts spelled out and its accents removed.
func cleanMovieTitle(title string) (string, error) {
	if isBlank(title) {
		return title, nil
	}
	if _, err := strconv.ParseInt(strings.Trim(title, " \t\n\v\f\r"), 10, 64); err == nil {
		return title, nil
	}
	out, err := cleanMovieTitleRe.Replace(title, "", -1, -1)
	if err != nil {
		return "", err
	}
	return removeAccent(germanUmlauts.Replace(strings.ToLower(out)))
}

// cleanSeriesRe is the normalization pattern of cleanSeriesTitle: the common
// words (a, an, the, and, or, of) that are neither first nor last, and every
// non-word character and underscore; percentRe is its percent rule.
var cleanSeriesRe, percentRe = func() (*regexp2.Regexp, *regexp2.Regexp) {
	clean, err := regexp2.Compile(`((?:\b|_)(?<!^)([aà](?!$)|an|the|and|or|of)(?!$)(?:\b|_))|\W|_`, ic)
	if err != nil {
		panic(err)
	}
	percent, err := regexp2.Compile(`(?<=\b\d+)%`, regexp2.None)
	if err != nil {
		panic(err)
	}
	clean.MatchTimeout, percent.MatchTimeout = matchTimeout, matchTimeout
	return clean, percent
}()

// cleanSeriesTitle normalizes a title: a blank or all-digit title
// is returned as it is, otherwise "%" after digits reads "percent", the
// normalized title is lower-cased and its accents removed.
func cleanSeriesTitle(title string) (string, error) {
	if isBlank(title) {
		return title, nil
	}
	if _, err := strconv.ParseInt(strings.Trim(title, " \t\n\v\f\r"), 10, 64); err == nil {
		return title, nil
	}
	title, err := percentRe.Replace(title, "percent", -1, -1)
	if err != nil {
		return "", err
	}
	out, err := cleanSeriesRe.Replace(title, "", -1, -1)
	if err != nil {
		return "", err
	}
	return removeAccent(strings.ToLower(out))
}
