// SPDX-License-Identifier: GPL-3.0

package parse

// Release group parsing for the movie title parser. Control flow, pattern
// order and pattern text are semantic. The parser runs under the budget of
// the title parse that calls it and never starts its own.

import (
	"github.com/dlclark/regexp2/v2"
)

// releaseGroupTable is the immutable set of compiled release group patterns.
type releaseGroupTable struct {
	group          *regexp2.Regexp
	invalid        *regexp2.Regexp
	anime          *regexp2.Regexp
	exceptionExact *regexp2.Regexp
	exception      *regexp2.Regexp
	clean          *regexp2.Regexp
}

func newReleaseGroupTable() (*releaseGroupTable, error) {
	c := &compiler{}
	t := &releaseGroupTable{}
	t.group = c.re(`-(?<releasegroup>[a-z0-9]+(?<part2>-[a-z0-9]+)?(?!.+?(?:480p|576p|720p|1080p|2160p)))(?<!(?:WEB-(DL|Rip)|Blu-Ray|480p|576p|720p|1080p|2160p|DTS-HD|DTS-X|DTS-MA|DTS-ES|-ES|-EN|-CAT|-ENG|-JAP|-GER|-FRA|-FRE|-ITA|-HDRip|\d{1,2}-bit|[ ._]\d{4}-\d{2}|-\d{2}|tmdb(id)?-(?<tmdbid>\d+)|(?<imdbid>tt\d{7,8}))(?:\k<part2>)?)(?:\b|[-._ ]|$)|[-._ ]\[(?<releasegroup>[a-z0-9]+)\]$`, ic)
	t.invalid = c.re(`^([se]\d+|[0-9a-f]{8})$`, ic)
	t.anime = c.re(`^(?:\[(?<subgroup>(?!\s).+?(?<!\s))\](?:_|-|\s|\.)?)`, ic)
	// Exception release groups that do not follow -RlsGrp; a manual list by name
	// only. Be careful with this last: there is a high chance of false positives.
	t.exceptionExact = c.re(`\b(?<releasegroup>KRaLiMaRKo|E\.N\.D|D\-Z0N3|Koten_Gars|BluDragon|ZØNEHD|HQMUX|VARYG|YIFY|YTS(.(MX|LT|AG))?|TMd|Eml HDTeam|LMain|DarQ|BEN THE MEN|TAoE|QxR|126811)\b`, ic)
	// Groups whose releases end with RlsGroup) or RlsGroup].
	t.exception = c.re(`(?<=[._ \[])(?<releasegroup>(Silence|afm72|Panda|Ghost|MONOLITH|Tigole|Joy|ImE|UTR|t3nzin|Anime Time|Project Angel|Hakata Ramen|HONE|GiLG|Vyndros|SEV|Garshasp|Kappa|Natty|RCVR|SAMPA|YOGI|r00t|EDGE2020|RZeroX|FreetheFish|Anna|Bandi|Qman|theincognito|HDO|DusIctv|DHD|CtrlHD|-ZR-|ADC|XZVN|RH|Kametsu|Celdra)(?=\]|\)))`, ic)
	t.clean = c.re(`(-(RP|1|NZBGeek|Obfuscated|Obfuscation|Scrambled|sample|Pre|postbot|xpost|Rakuv[a-z0-9]*|WhiteRev|BUYMORE|AsRequested|AlternativeToRequested|GEROV|Z0iDS3N|Chamele0n|4P|4Planet|AlteZachen|RePACKPOST))+$`, ic)
	if c.err != nil {
		return nil, c.err
	}
	return t, nil
}

// parse returns the release group of a title, empty where there is none. t
// supplies the extension removal and the website and torrent suffix cleanup
// the two parsers share.
func (g *releaseGroupTable) parse(b *budget, t *movieTitleTable, title string) (string, error) {
	title, err := t.removeFileExtension(b, trimWhite(title))
	if err != nil {
		return "", err
	}
	if title, err = b.replace(t.common.websitePrefix, title, ""); err != nil {
		return "", err
	}
	if title, err = b.replace(t.common.torrentSuffix, title, ""); err != nil {
		return "", err
	}

	anime, err := b.first(g.anime, title)
	if err != nil {
		return "", err
	}
	if anime != nil {
		return groupText(anime, "subgroup"), nil
	}

	if title, err = b.replace(g.clean, title, ""); err != nil {
		return "", err
	}

	for _, re := range [...]*regexp2.Regexp{g.exceptionExact, g.exception} {
		m, err := b.last(re, title)
		if err != nil {
			return "", err
		}
		if m != nil {
			return groupText(m, "releasegroup"), nil
		}
	}

	m, err := b.last(g.group, title)
	if err != nil || m == nil {
		return "", err
	}
	group := groupText(m, "releasegroup")
	if _, numeric := tryParseInt(group); numeric {
		return "", nil
	}
	invalid, err := b.isMatch(g.invalid, group)
	if err != nil || invalid {
		return "", err
	}
	return group, nil
}
