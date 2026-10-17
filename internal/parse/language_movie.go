// SPDX-License-Identifier: GPL-3.0

package parse

// Language parsing for the movie title parser, ported from the movie reference
// parser (parser/LanguageParser.cs ParseLanguages, located by
// BOXSET_REF_MOVIE) at commit df2faa64c75e6081556d11232dc46523e7264ea1.
// Control flow, pattern order and pattern text follow the source; its Language
// members map to stable Boxset codes through the explicit table of vocab.go.
// The parser runs under the budget of the title parse that calls it and never
// starts its own.

import (
	"strings"

	"github.com/dlclark/regexp2/v2"
)

// languageTable is the immutable set of compiled language patterns.
type languageTable struct {
	insensitive   *regexp2.Regexp
	caseSensitive *regexp2.Regexp
	germanDual    *regexp2.Regexp
	germanMulti   *regexp2.Regexp
}

func newLanguageTable() (*languageTable, error) {
	c := &compiler{}
	t := &languageTable{}
	t.insensitive = c.re(`(?:\W|_|^)(?<english>\beng\b)|
		(?<italian>\b(?:ita|italian)\b)|
		(?<german>(?:swiss)?german\b|videomann|ger[. ]dub|\bger\b)|
		(?<flemish>flemish)|
		(?<bulgarian>bgaudio)|
		(?<romanian>rodubbed)|
		(?<brazilian>\b(dublado|pt-BR)\b)|
		(?<greek>greek)|
		(?<french>\b(?:FR|VO|VF|VFF|VFQ|VFI|VF2|TRUEFRENCH|FRENCH|FRE|FRA)\b)|
		(?<russian>\b(?:rus|ru)\b)|
		(?<hungarian>\b(?:HUNDUB|HUN)\b)|
		(?<hebrew>\b(?:HebDub|HebDubbed)\b)|
		(?<polish>\b(?:PL\W?DUB|DUB\W?PL|LEK\W?PL|PL\W?LEK)\b)|
		(?<chinese>\[(?:CH[ST]|BIG5|GB)\]|简|繁|字幕)|
		(?<ukrainian>(?:(?:\dx)?UKR))|
		(?<spanish>\b(?:español|castellano)\b)|
		(?<catalan>\b(?:catalan?|catalán|català)\b)|
		(?<latvian>\b(?:lat|lav|lv)\b)|
		(?<telugu>\btel\b)|
		(?<vietnamese>\bVIE\b)|
		(?<japanese>\bJAP\b)|
		(?<korean>\bKOR\b)|
		(?<urdu>\burdu\b)|
		(?<romansh>\b(?:romansh|rumantsch|romansch)\b)|
		(?<mongolian>\b(?:mongolian|khalkha)\b)|
		(?<georgian>\b(?:georgian|geo|ka|kat)\b)|
		(?<original>\b(?:orig|original)\b)`, ic|regexp2.IgnorePatternWhitespace)
	t.caseSensitive = c.re(`(?:(?i)(?<!SUB[\W|_|^]))(?:(?<english>\bEN\b)|
		(?<lithuanian>\bLT\b)|
		(?<czech>\bCZ\b)|
		(?<polish>\bPL\b)|
		(?<bulgarian>\bBG\b)|
		(?<slovak>\bSK\b)|
		(?<german>\bDE\b)|
		(?<spanish>\b(?<!DTS[._ -])ES\b))(?:(?i)(?![\W|_|^]SUB))`, regexp2.IgnorePatternWhitespace)
	t.germanDual = c.re(`(?<!WEB[-_. ]?)\bDL\b`, ic)
	t.germanMulti = c.re(`\bML\b`, ic)
	if c.err != nil {
		return nil, c.err
	}
	return t, nil
}

// languageNames are the English language names ParseLanguages looks for in the
// lower-cased title, in source order; a name list is any of its names.
var languageNames = [...]struct {
	language int
	names    []string
}{
	{lgEnglish, []string{"english"}},
	{lgSpanish, []string{"spanish"}},
	{lgDanish, []string{"danish"}},
	{lgDutch, []string{"dutch"}},
	{lgJapanese, []string{"japanese"}},
	{lgIcelandic, []string{"icelandic"}},
	{lgChinese, []string{"mandarin", "cantonese", "chinese"}},
	{lgKorean, []string{"korean"}},
	{lgRussian, []string{"russian"}},
	{lgRomanian, []string{"romanian"}},
	{lgHindi, []string{"hindi"}},
	{lgArabic, []string{"arabic"}},
	{lgThai, []string{"thai"}},
	{lgBulgarian, []string{"bulgarian"}},
	{lgPolish, []string{"polish"}},
	{lgVietnamese, []string{"vietnamese"}},
	{lgSwedish, []string{"swedish"}},
	{lgNorwegian, []string{"norwegian"}},
	{lgFinnish, []string{"finnish"}},
	{lgTurkish, []string{"turkish"}},
	{lgPortuguese, []string{"portuguese"}},
	{lgPortugueseBR, []string{"brazilian"}},
	{lgHungarian, []string{"hungarian"}},
	{lgHebrew, []string{"hebrew"}},
	{lgUkrainian, []string{"ukrainian"}},
	{lgPersian, []string{"persian"}},
	{lgBengali, []string{"bengali"}},
	{lgSlovak, []string{"slovak"}},
	{lgLatvian, []string{"latvian"}},
	{lgSpanishLatino, []string{"latino"}},
	{lgTamil, []string{"tamil"}},
	{lgTelugu, []string{"telugu"}},
	{lgMalayalam, []string{"malayalam"}},
	{lgKannada, []string{"kannada"}},
	{lgAlbanian, []string{"albanian"}},
	{lgAfrikaans, []string{"afrikaans"}},
	{lgMarathi, []string{"marathi"}},
	{lgTagalog, []string{"tagalog"}},
}

// groupLanguage names the language a pattern group stands for.
type groupLanguage struct {
	group    string
	language int
}

// caseSensitiveGroups and insensitiveGroups are the groups ParseLanguages reads
// from each match, in source order. The insensitive list keeps the source's
// dutch entry, whose pattern has no such group and so never matches.
var (
	caseSensitiveGroups = [...]groupLanguage{
		{"english", lgEnglish}, {"lithuanian", lgLithuanian}, {"czech", lgCzech}, {"polish", lgPolish},
		{"bulgarian", lgBulgarian}, {"slovak", lgSlovak}, {"spanish", lgSpanish}, {"german", lgGerman},
	}
	insensitiveGroups = [...]groupLanguage{
		{"english", lgEnglish}, {"italian", lgItalian}, {"german", lgGerman}, {"flemish", lgFlemish},
		{"greek", lgGreek}, {"french", lgFrench}, {"russian", lgRussian}, {"bulgarian", lgBulgarian},
		{"brazilian", lgPortugueseBR}, {"dutch", lgDutch}, {"hungarian", lgHungarian}, {"hebrew", lgHebrew},
		{"polish", lgPolish}, {"chinese", lgChinese}, {"spanish", lgSpanish}, {"catalan", lgCatalan},
		{"ukrainian", lgUkrainian}, {"latvian", lgLatvian}, {"romanian", lgRomanian}, {"telugu", lgTelugu},
		{"vietnamese", lgVietnamese}, {"japanese", lgJapanese}, {"korean", lgKorean}, {"urdu", lgUrdu},
		{"romansh", lgRomansh}, {"mongolian", lgMongolian}, {"georgian", lgGeorgian}, {"original", lgOriginal},
	}
)

// parse is LanguageParser.ParseLanguages: the language codes of a title in the
// source's order, without duplicates; Unknown when nothing matches.
func (t *languageTable) parse(b *budget, title string) ([]string, error) {
	defs := languageDefs()
	var found []int
	lower := strings.ToLower(title)
	for _, n := range languageNames {
		for _, name := range n.names {
			if strings.Contains(lower, name) {
				found = append(found, n.language)
				break
			}
		}
	}

	// Case-sensitive, then case-insensitive.
	for _, pass := range [...]struct {
		re     *regexp2.Regexp
		groups []groupLanguage
	}{{t.caseSensitive, caseSensitiveGroups[:]}, {t.insensitive, insensitiveGroups[:]}} {
		m, err := b.first(pass.re, title)
		for m != nil && err == nil {
			for _, g := range pass.groups {
				if groupOK(m, g.group) {
					found = append(found, g.language)
				}
			}
			m, err = b.next(pass.re, m)
		}
		if err != nil {
			return nil, err
		}
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
