// SPDX-License-Identifier: GPL-3.0

package parse

// Language parsing for the series title parser, ported from the series reference
// parser (parser/LanguageParser.cs ParseLanguages, located by BOXSET_REF_SERIES)
// at commit 57816c149d02f95543973fefb4780c4495f8121c. Control flow, pattern order
// and pattern text follow the source; its Language members map to stable Boxset
// codes through the explicit table of vocab.go. The parser runs under the budget
// of the title parse that calls it and never starts its own.

import (
	"strings"

	"github.com/dlclark/regexp2/v2"
)

// seriesLanguageTable is the immutable set of compiled language patterns.
type seriesLanguageTable struct {
	clean         *regexp2.Regexp
	insensitive   *regexp2.Regexp
	caseSensitive *regexp2.Regexp
	germanDual    *regexp2.Regexp
	germanMulti   *regexp2.Regexp
}

func newSeriesLanguageTable() (*seriesLanguageTable, error) {
	c := &compiler{}
	t := &seriesLanguageTable{}
	t.clean = c.re(`.*?[_. ](S\d{2}(?:E\d{2,4})*[_. ].*)`, ic)
	// The source builds its language regex from the languages-only pattern: any
	// language not followed by further languages and a "subs" word.
	only := `(?<english>\b(?:ing|eng)\b)|(?<italian>\b(?:ita|italian)\b)|(?<german>(?:swiss)?german\b|videomann|ger[. ]dub|\bger\b)|(?<flemish>flemish)|(?<greek>greek)|(?<french>(?:_|\b)(?:FR|VF|VF2|VFF|VFI|VFQ|TRUEFRENCH|FRENCH|FRE|FRA)(?:_|\b))|(?<russian>\b(?:rus|ru)\b)|(?<hungarian>\b(?:HUNDUB|HUN)\b)|(?<hebrew>\bHebDub\b)|(?<polish>\b(?:PL\W?DUB|DUB\W?PL|LEK\W?PL|PL\W?LEK)\b)|(?<chinese>\[(?:CH[ST]|BIG5|GB)\]|简|繁|字幕|国语音轨[.+])|(?<bulgarian>\bbgaudio\b)|(?<spanish>\b(?:español|castellano|esp|spa(?!\(Latino\)))\b)|(?<ukrainian>\b(?:\dx?)?(?:ukr))|(?<thai>\b(?:THAI)\b)|(?<romanian>\b(?:RoDubbed|ROMANIAN)\b)|(?<catalan>[-,. ]cat[. ](?:DD|subs)|\b(?:catalan|catalán)\b)|(?<latvian>\b(?:lat|lav|lv)\b)|(?<turkish>\b(?:tur)\b)|(?<urdu>\burdu\b)|(?<romansh>\b(?:romansh|rumantsch|romansch)\b)|(?<georgian>\b(?:geo|ka|kat|georgian)\b)|(?<irish>\b(?:irish|gaeilge)\b)|(?<japanese>\(JA\)|JAP|JPN)|(?<portuguese>[_. ]por[_. ])|(?<original>\b(?:orig|original)\b)`
	t.insensitive = c.re(`(?:`+only+`)(?!(?:[-_. ](?:`+only+`))*[-_. ]subs?)`, ic)
	t.caseSensitive = c.re(`(?:(?i)(?<!SUB[\W|_|^]))(?:(?<lithuanian>\bLT\b)|(?<czech>\bCZ\b)|(?<polish>\bPL\b)|(?<bulgarian>\bBG\b)|(?<slovak>\bSK\b)|(?<german>\bDE\b))(?:(?i)(?![\W|_|^]SUB))`, regexp2.None)
	t.germanDual = c.re(`(?<!WEB[-_. ]?)\bDL\b`, ic)
	t.germanMulti = c.re(`\bML\b`, ic)
	if c.err != nil {
		return nil, c.err
	}
	return t, nil
}

// seriesLanguageNames are the English language names ParseLanguages looks for in
// the lower-cased title, in source order; a name list is any of its names.
var seriesLanguageNames = [...]struct {
	language int
	names    []string
}{
	{lgSpanish, []string{"spanish"}},
	{lgDanish, []string{"danish"}},
	{lgDutch, []string{"dutch"}},
	{lgJapanese, []string{"japanese"}},
	{lgIcelandic, []string{"icelandic"}},
	{lgChinese, []string{"mandarin", "cantonese", "chinese"}},
	{lgKorean, []string{"korean"}},
	{lgRussian, []string{"russian"}},
	{lgPolish, []string{"polish"}},
	{lgVietnamese, []string{"vietnamese"}},
	{lgSwedish, []string{"swedish"}},
	{lgNorwegian, []string{"norwegian"}},
	{lgFinnish, []string{"finnish"}},
	{lgTurkish, []string{"turkish"}},
	{lgPortuguese, []string{"portuguese"}},
	{lgHungarian, []string{"hungarian"}},
	{lgHebrew, []string{"hebrew"}},
	{lgArabic, []string{"arabic"}},
	{lgHindi, []string{"hindi"}},
	{lgMalayalam, []string{"malayalam"}},
	{lgUkrainian, []string{"ukrainian"}},
	{lgBulgarian, []string{"bulgarian"}},
	{lgSlovak, []string{"slovak"}},
	{lgPortugueseBR, []string{"brazilian", "dublado"}},
	{lgSpanishLatino, []string{"latino"}},
	{lgLatvian, []string{"latvian"}},
	{lgAzerbaijani, []string{"azerbaijani", "azerbaijan"}},
	{lgUzbek, []string{"uzbek", "uzbekistan"}},
}

// The groups ParseLanguages reads from each match, in source order. The
// insensitive list keeps the source's dutch entry, whose pattern has no such
// group and so never matches.
var (
	seriesCaseSensitiveGroups = [...]groupLanguage{
		{"lithuanian", lgLithuanian}, {"czech", lgCzech}, {"polish", lgPolish},
		{"bulgarian", lgBulgarian}, {"slovak", lgSlovak}, {"german", lgGerman},
	}
	seriesInsensitiveGroups = [...]groupLanguage{
		{"english", lgEnglish}, {"italian", lgItalian}, {"german", lgGerman}, {"flemish", lgFlemish},
		{"greek", lgGreek}, {"french", lgFrench}, {"russian", lgRussian}, {"dutch", lgDutch},
		{"hungarian", lgHungarian}, {"hebrew", lgHebrew}, {"polish", lgPolish}, {"chinese", lgChinese},
		{"bulgarian", lgBulgarian}, {"ukrainian", lgUkrainian}, {"spanish", lgSpanish}, {"thai", lgThai},
		{"romanian", lgRomanian}, {"catalan", lgCatalan}, {"latvian", lgLatvian}, {"turkish", lgTurkish},
		{"urdu", lgUrdu}, {"romansh", lgRomansh}, {"georgian", lgGeorgian}, {"irish", lgIrish},
		{"japanese", lgJapanese}, {"portuguese", lgPortuguese}, {"original", lgOriginal},
	}
)

// parse is LanguageParser.ParseLanguages: the language codes of a title in the
// source's order, without duplicates; Unknown when nothing matches.
func (t *seriesLanguageTable) parse(b *budget, title string) ([]string, error) {
	defs := languageDefs()
	title, err := b.replace(t.clean, title, "$1")
	if err != nil {
		return nil, err
	}
	var found []int
	lower := strings.ToLower(title)
	for _, n := range seriesLanguageNames {
		for _, name := range n.names {
			if strings.Contains(lower, name) {
				found = append(found, n.language)
				break
			}
		}
	}

	// The case-sensitive pattern reads its first match only.
	cs, err := b.first(t.caseSensitive, title)
	if err != nil {
		return nil, err
	}
	for _, g := range seriesCaseSensitiveGroups {
		if groupOK(cs, g.group) {
			found = append(found, g.language)
		}
	}
	// The case-insensitive pattern reads every match.
	m, err := b.first(t.insensitive, title)
	for m != nil && err == nil {
		for _, g := range seriesInsensitiveGroups {
			if groupOK(m, g.group) {
				found = append(found, g.language)
			}
		}
		m, err = b.next(t.insensitive, title, m)
	}
	if err != nil {
		return nil, err
	}

	if strings.Contains(lower, "english") {
		found = append(found, lgEnglish)
	}
	if len(found) == 0 {
		found = append(found, lgUnknown)
	}
	if len(found) == 1 && found[0] == lgGerman {
		dual, err := b.isMatch(t.germanDual, title)
		if err != nil {
			return nil, err
		}
		if dual {
			found = append(found, lgOriginal)
		} else {
			multi, err := b.isMatch(t.germanMulti, title)
			if err != nil {
				return nil, err
			}
			if multi {
				found = append(found, lgOriginal, lgEnglish)
			}
		}
	}

	var seen [lgCount]bool
	codes := make([]string, 0, len(found))
	for _, l := range found {
		if !seen[l] {
			seen[l] = true
			codes = append(codes, defs[l].code)
		}
	}
	return codes, nil
}
