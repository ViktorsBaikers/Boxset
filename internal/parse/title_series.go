// SPDX-License-Identifier: GPL-3.0

package parse

// Series title parsing, ported from the series reference parser (parser/Parser.cs
// ParseTitle and the parser/ParserCommon.cs and parser/RegexReplace.cs members it
// uses, located by BOXSET_REF_SERIES) at commit
// 57816c149d02f95543973fefb4780c4495f8121c. Control flow, pattern order and
// pattern text follow the source: the first pattern in source order that matches
// and yields a result decides, so the order of the tables is semantic.
//
// Faithful-adapter notes:
//   - Every pattern is compiled once with the table and all matching goes
//     through the shared budget of the public operation (regex.go): the quality,
//     language and release group parsing ParseTitle runs share that one budget
//     and never get a fresh one.
//   - The source's exception handler turns any exception into "unable to
//     parse". The exceptions its code can raise (a number that does not parse, an
//     invalid or too distant air date) are errUnparseable here and end the parse
//     as the source's null, while a budget, timeout or cancellation failure is an
//     error and never a quiet no-match.
//   - Group indexes of the source are UTF-16 code units: byte offsets are
//     converted before they index the release title.
//   - Numbers are text until they are integers or exact decimals: a special
//     absolute episode number is kept as its decimal text, never a float.
//   - The source reads its clock as DateTime.Now: the budget clock and the
//     calendar of Options.Location stand in for it.

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"

	"github.com/dlclark/regexp2/v2"
)

// errUnparseable is an exception of the source's ParseTitle: the whole parse is
// the source's null.
var errUnparseable = errors.New("parse: unparseable release name")

// substitution is one RegexReplace of the source's ParserCommon: a pattern and
// its replacement format.
type substitution struct {
	re          *regexp2.Regexp
	replacement string
}

// seriesTitleTable is the immutable set of compiled patterns of the series title
// parser, with the language and release group tables it runs.
type seriesTitleTable struct {
	quality *seriesTable
	lang    *seriesLanguageTable
	group   *seriesGroupTable
	common  commonTable

	preSubstitutions []substitution
	titles           []*regexp2.Regexp // ReportTitleRegex, in source order
	rejectHashed     []*regexp2.Regexp
	seasonFolder     *regexp2.Regexp
	reversed         *regexp2.Regexp
	simpleTitle      *regexp2.Regexp
	sixDigitAirDate  *regexp2.Regexp
	cleanBrackets    *regexp2.Regexp
	yearInTitle      *regexp2.Regexp
	titleComponents  *regexp2.Regexp
	requestInfo      *regexp2.Regexp
	fileExtension    *regexp2.Regexp
}

func newSeriesTitleTable(q *seriesTable) (*seriesTitleTable, error) {
	c := &compiler{}
	t := &seriesTitleTable{quality: q}
	t.preSubstitutions = []substitution{
		// These are functions shared between different parser functions
		// they are not intended to be used outside of them parsing.
		// Korean series without season number, replace with S01Exxx and remove airdate
		{c.re(`\.E(\d{2,4})\.\d{6}\.(.*-(F1RST|NEXT))$`, regexp2.None), ".S01E$1.$2"},
		// Some Chinese anime releases contain both English and Chinese titles, remove the Chinese title and replace with normal anime pattern
		{c.re(`^\[(?:(?<subgroup>[^\]]+?)(?:[\u4E00-\u9FCC]+)?)\]\[(?<title>[^\]]+?)(?:\s(?<chinesetitle>[\u4E00-\u9FCC][^\]]*?))\]\[(?:(?:[\u4E00-\u9FCC]+?)?(?<episode>\d{1,4})(?:[\u4E00-\u9FCC]+?)?)\]`, regexp2.None), "[${subgroup}] ${title} - ${episode} - "},
		// Chinese LoliHouse/ZERO/Lilith-Raws/Skymoon-Raws/orion origin releases don't use the expected brackets, normalize using brackets
		{c.re(`^\[(?<subgroup>[^\]]*?(?:LoliHouse|ZERO|Lilith-Raws|Skymoon-Raws|orion origin)[^\]]*?)\](?<title>[^\[\]]+?)(?: - (?<episode>[0-9-]+)\s*|\[第?(?<episode>[0-9]+(?:-[0-9]+)?)话?(?:END|完)?\])\[`, regexp2.None), "[${subgroup}][${title}][${episode}]["},
		// Most Chinese anime releases contain additional brackets/separators for chinese and non-chinese titles, remove junk first and if it has S0x as season number, convert it to Sx
		{c.re(`^\[(?<subgroup>[^\]]+)\](?:\s?★[^\[ -]+\s?)?\[?(?:(?<chinesetitle>(?=[^\]]*?[\u4E00-\u9FCC])[^\]]*?)(?:\]\[|\s*[_/·]\s*)){0,2}(?<title>[^\[\]]+?)(?:\s(?:S?(?<!\d+)((0)(?<season>\d)|(?<season>[1-9]\d))(?!\d+)))\]?(?:\[\d{4}\])?\[第?(?<episode>[0-9]+(?:-[0-9]+)?)(?:话|集)?(?: ?END|完| ?Fin)?\]`, regexp2.None), "[${subgroup}] ${title} S${season} - ${episode} "},
		// Some Chinese releases don't include a separation between Chinese and English titles within the same bracketed group
		{c.re(`^\[(?<subgroup>[^\]]+)\]\[(?<chinesetitle>(?<![^a-zA-Z0-9])[^a-zA-Z0-9]+)(?<title>[^\]]+?)\](?:\[\d{4}\])?\[第?(?<episode>[0-9]+(?:-[0-9]+)?)(?:话|集)?(?: ?END|完| ?Fin)?\]`, regexp2.None), "[${subgroup}] ${title} - ${episode} "},
		// Chinese season packs, that may not actually have the full season so treated as multi-episode
		{c.re(`^\[(?<subgroup>[^\]]+)\](?:\s?★[^\[ -]+\s?)?\[?(?:(?<chinesetitle>(?=[^\]]*?[\u4E00-\u9FCC])[^\]]*?)(?:\]\[|\s*[_/·]\s*)){0,2}(?<title>[^\]]+?)\]?(?:\[\d{4}\])?\[(?:第|全)?(?<episode>[0-9]{1,4}-[0-9]{1,4})(?:话|集)?\](?:\[(?<language>.+?)\+.+?\])[_. ](?<title>.+?)S(?<season>[0-9]{1,2})(?<rest>.+?)$`, regexp2.None), "${title}S${season}E${episode}.${language}${rest}"},
		// Chinese season packs, that actually have the full season so treated as multi-episode
		{c.re(`^\[(?<subgroup>[^\]]+)\](?:\s?★[^\[ -]+\s?)?\[?(?:(?<chinesetitle>(?=[^\]]*?[\u4E00-\u9FCC])[^\]]*?)(?:\]\[|\s*[_/·]\s*)){0,2}(?<title>[^\]]+?)\]?(?:\[\d{4}\])?\[(?:全)?(?<episode>[0-9]{1,4})(?:话|集)?\](?:\[(?<language>.+?)\+.+?\])[_. ](?<title>.+?)S(?<season>[0-9]{1,2})(?<rest>.+?)$`, regexp2.None), "${title}S${season}.${language}${rest}"},
		// Most Chinese anime releases contain additional brackets/separators for chinese and non-chinese titles, remove junk and replace with normal anime pattern
		{c.re(`^\[(?<subgroup>[^\]]+)\](?:\s?★[^\[ -]+\s?)?\[?(?:(?<chinesetitle>(?=[^\]]*?[\u4E00-\u9FCC])[^\]]*?)(?:\]\[|\s*[_/·]\s*)){0,2}(?<title>[^\]]+?)\]?(?:\[\d{4}\])?\[第?(?<episode>[0-9]{1,4}(?:-[0-9]{1,4})?)(?:话|集)?(?: ?END|完| ?Fin)?\]`, regexp2.None), "[${subgroup}] ${title} - ${episode} "},
		// Some Chinese anime releases contain both Chinese and English titles, remove the Chinese title first and if it has S0x as season number, convert it to Sx
		{c.re(`^\[(?<subgroup>[^\]]+)\](?:\s)(?:(?<chinesetitle>(?=[^\]]*?[\u4E00-\u9FCC])[^\]]*?)(?:\s/\s))(?<title>[^\[\]]+?)(?:\s(?:S?(?<!\d+)((0)(?<season>\d)|(?<season>[1-9]\d))(?!\d+)))(?:[- ]+)(?<episode>[0-9]+(?:-[0-9]+)?)话?(?:END|完)?`, regexp2.None), "[${subgroup}] ${title} S${season} - ${episode} "},
		// Some Chinese anime releases contain both English and Chinese titles, remove the Chinese title and replace with normal anime pattern
		{c.re(`^\[(?<subgroup>[^\]]+)\](?:\s)(?:(?<title>[^\]]+?)(?:\s/\s))(?<chinesetitle>(?=[^\]]*?[\u4E00-\u9FCC])[^\]]*?)(?:[- ]+)(?<episode>[0-9]+(?:-[0-9]+)?(?![a-z]))话?(?:END|完)?`, regexp2.None), "[${subgroup}] ${title} - ${episode} "},
		// Some Chinese anime releases contain both Chinese and English titles, remove the Chinese title and replace with normal anime pattern
		{c.re(`^\[(?<subgroup>[^\]]+)\](?:\s)(?:(?<chinesetitle>(?=[^\]]*?[\u4E00-\u9FCC])[^\]]*?)(?:\s/\s))(?<title>[^\]]+?)(?:[- ]+)(?<episode>[0-9]+(?:-[0-9]+)?(?![a-z]))话?(?:END|完)?`, regexp2.None), "[${subgroup}] ${title} - ${episode} "},
		// GM-Team releases with lots of square brackets
		{c.re(`^\[(?<subgroup>[^\]]+)\](?:(?<chinesubgroup>\[(?=[^\]]*?[\u4E00-\u9FCC])[^\]]*\])+)\[(?<title>[^\]]+?)\](?<junk>\[[^\]]+\])*\[(?<episode>[0-9]+(?:-[0-9]+)?)( END| Fin)?\]`, regexp2.None), "[${subgroup}] ${title} - ${episode} "},
		// Some Chinese anime releases contain both Chinese and English titles separated by | instead of /, remove the Chinese title and replace with normal anime pattern
		{c.re(`^\[(?<subgroup>[^\]]+)\](?:\s)(?:(?<chinesetitle>(?=[^\]]*?[\u4E00-\u9FCC])[^\]]*?)(?:\s\|\s))(?<title>[^\]]+?)(?:[- ]+)(?<episode>[0-9]+(?:-[0-9]+)?(?![a-z]))话?(?:END|完)?`, regexp2.None), "[${subgroup}] ${title} - ${episode} "},
		// Spanish releases with information in brackets
		{c.re(`^(?<title>.+?(?=[ ._-]\()).+?\((?<year>\d{4})\/(?<info>S[^\/]+)`, regexp2.None), "${title} (${year}) - ${info} "},
	}
	t.titles = []*regexp2.Regexp{
		// Daily episode with year in series title and air time after date (Plex DVR format)
		c.re(`^^(?<title>.+?\((?<titleyear>\d{4})\))[-_. ]+(?<airyear>19[4-9]\d|20\d\d)(?<sep>[-_]?)(?<airmonth>0\d|1[0-2])\k<sep>(?<airday>[0-2]\d|3[01])[-_. ]\d{2}[-_. ]\d{2}[-_. ]\d{2}`, ic),
		// Daily episodes without title (2018-10-12, 20181012) (Strict pattern to avoid false matches)
		c.re(`^(?<airyear>19[6-9]\d|20\d\d)(?<sep>[-_]?)(?<airmonth>0\d|1[0-2])\k<sep>(?<airday>[0-2]\d|3[01])(?!\d)`, ic),
		// Multi-Part episodes without a title (S01E05.S01E06)
		c.re(`^(?:\W*S(?<season>(?<!\d+)(?:\d{1,2}|\d{4})(?!\d+))(?:e{1,2}(?<episode>\d{1,3}(?!\d+)))+){2,}`, ic),
		// Multi-Part episodes without a title (1x05.1x06)
		c.re(`^(?:\W*(?<season>(?<!\d+)(?:\d{1,2}|\d{4})(?!\d+))(?:x{1,2}(?<episode>\d{1,3}(?!\d+)))+){2,}`, ic),
		// Episodes without a title, Multi (S01E04E05, 1x04x05, etc)
		c.re(`^(?:S?(?<season>(?<!\d+)(?:\d{1,2}|\d{4})(?!\d+))(?:(?:[-_]|[ex]){1,2}(?<episode>\d{2,3}(?!\d+))){2,})`, ic),
		// Split episodes (S01E05a, S01E05b, etc)
		c.re(`^(?<title>.+?)(?:S?(?<season>(?<!\d+)(?:\d{1,2}|\d{4})(?!\d+))(?:(?:[-_ ]?[ex])(?<episode>\d{2,3}(?!\d+))(?<splitepisode>[a-d])(?:[ _.])))`, ic),
		// Episodes without a title, Single (S01E05, 1x05)
		c.re(`^(?:S?(?<season>(?<!\d+)(?:\d{1,2}|\d{4})(?!\d+))(?:(?:[-_ ]?[ex])(?<episode>\d{2,3}(?!\d+))))`, ic),
		// Anime - [SubGroup] Title Absolute (Season+Episode)
		c.re(`^(?:\[(?<subgroup>.+?)\](?:_|-|\s|\.)?)(?<title>.+?)[-_. ]+(?<absoluteepisode>(?<!\d+)\d{2,3}(\.\d{1,2})?(?!\d+))(?:[-_. ])+\((?:S(?<season>(?<!\d+)\d{1,2}(?!\d+))(?:(?:[ex]|\W[ex]){1,2}(?<episode>\d{2}(?!\d+))))(?:v\d+)?(?:\)(?!\d+)).*?(?<hash>[(\[][0-9A-F]{8}[)\]])?$`, ic),
		// Anime - [SubGroup] Title Absolute - Season+Episode
		c.re(`^(?:\[(?<subgroup>.+?)\](?:_|-|\s|\.)?)(?<title>.+?)[-_. ]+(?<absoluteepisode>(?<!\d+)\d{2,3}(\.\d{1,2})?(?!\d+))(?:[-_. ](?<![()\[!]))+(?:S(?<season>(?<!\d+)\d{1,2}(?!\d+))(?:(?:[ex]|\W[ex]){1,2}(?<episode>\d{2}(?!\d+))))(?:v\d+)?(?:[_. ](?!\d+)).*?(?<hash>[(\[][0-9A-F]{8}[)\]])$`, ic),
		// Anime - [SubGroup] Title Season+Episode
		c.re(`^(?:\[(?<subgroup>.+?)\](?:_|-|\s|\.)?)(?<title>.+?)(?:[-_\W](?<![()\[!]))+(?:S?(?<season>(?<!\d+)\d{1,2}(?!\d+))(?:(?:[ex]|\W[ex]){1,2}(?<episode>\d{2}(?!\d+)))+)(?:v\d+)?(?:[_. ](?!\d+)).*?(?<hash>[(\[][0-9A-F]{8}[)\]])?$`, ic),
		// Anime - [SubGroup] Title Episode Absolute Episode Number ([SubGroup] Series Title Episode 01)
		c.re(`^(?:\[(?<subgroup>.+?)\][-_. ]?)(?<title>.+?)[-_. ]+?(?:Episode)(?:[-_. ]+(?<absoluteepisode>(?<!\d+)\d{2,3}(\.\d{1,2})?(?!\d+)))+.*?(?<hash>[(\[][0-9A-F]{8}[)\]])?$`, ic),
		// Anime - [SubGroup] Title Absolute Episode Number + Season+Episode
		c.re(`^(?:\[(?<subgroup>.+?)\](?:_|-|\s|\.)?)(?<title>.+?)(?:(?:[-_\W](?<![()\[!]))+(?<absoluteepisode>\d{2,3}(\.\d{1,2})?))+(?:_|-|\s|\.)+(?:S?(?<season>(?<!\d+)\d{1,2}(?!\d+))(?:(?:\-|[ex]|\W[ex]){1,2}(?<episode>\d{2}(?!\d+)))+).*?(?<hash>[(\[][0-9A-F]{8}[)\]])?(?:$|\.)`, ic),
		// Anime - [SubGroup] Title Multi-episode + optional Absolute Episode Number
		c.re(`^(?:\[(?<subgroup>.+?)\](?:_|-|\s|\.)?)(?<title>.+?)(?:[-_\W](?<![()\[!]))+(?:S?(?<season>(?<!\d+)\d{1,2}(?!\d+))(?:(?:\-|[ex]|\W[ex]){1,2}(?<episode>\d{2}(?!\d+))){2,})(?:(?:_|-|\s|\.)+(?<absoluteepisode>(?<!\d+)\d{2,3}(\.\d{1,2})?(?!\d+|\-[a-z])))*.*?(?<hash>[(\[][0-9A-F]{8}[)\]])?$`, ic),
		// Anime - [SubGroup] Title Single episode + Absolute Episode Number
		c.re(`^(?:\[(?<subgroup>.+?)\](?:_|-|\s|\.)?)(?<title>.+?)(?:[-_\W](?<![()\[!]))+(?:S?(?<season>(?<!\d+)\d{1,2}(?!\d+))(?:(?:\-|[ex]|\W[ex]){1,2}(?<episode>\d{2}(?!\d+))))(?:(?:_|-|\s|\.)+(?<absoluteepisode>(?<!\d+)\d{2,3}(\.\d{1,2})?(?!\d+|\-[a-z])))+.*?(?<hash>[(\[][0-9A-F]{8}[)\]])?$`, ic),
		// Anime - [SubGroup] Title with trailing number Absolute Episode Number - Batch separated with tilde
		c.re(`^\[(?<subgroup>.+?)\][-_. ]?(?<title>.+?[^-]+?)(?:(?<![-_. ]|\b[0]\d+) - )[-_. ]?(?<absoluteepisode>\d{2,3}(\.\d{1,2})?(?!\d+))\s?~\s?(?<absoluteepisode>\d{2,3}(\.\d{1,2})?(?!\d+))(?:[-_. ]+(?<special>special|ova|ovd))?.*?(?<hash>[(\[][0-9A-F]{8}[)\]])?(?:$|\.mkv)`, ic),
		// Anime - [SubGroup] Title with season number in brackets Absolute Episode Number
		c.re(`^\[(?<subgroup>.+?)\][-_. ]?(?<title>[^-]+?)[_. ]+?\(Season[_. ](?<season>\d+)\)[-_. ]+?(?:[-_. ]?(?<absoluteepisode>\d{2,3}(\.\d{1,2})?(?!\d+)))+(?:[-_. ]+(?<special>special|ova|ovd))?.*?(?<hash>[(\[][0-9A-F]{8}[)\]])?(?:$|\.mkv)`, ic),
		// Anime - [SubGroup] Title with trailing 3-digit number and sub title - Absolute Episode Number
		c.re(`^\[(?<subgroup>[^\]]+?)\][-_. ]?(?<title>[^]]+?)(?:[-_. ]{3}?(?<absoluteepisode>\d{2}(\.\d{1,2})?(?!-?\d+|-[a-z]+)))+(?:[-_. ]+(?<special>special|ova|ovd))?.*?(?<hash>[(\[][0-9A-F]{8}[)\]])?(?:$|\.mkv)`, ic),
		// Anime - [SubGroup] Multi-season pack
		c.re(`^\[(?<subgroup>.+?)\][-_. ]?(?<title>.+?)[-_. ]+S(?<season>(?<!\d+)(?:\d{1,2})(?!\d+))(?:(?:-S?|[-_. ]{1,3}S)(?<season>(?<!\d+)(?:\d{1,2})(?!\d+))){1,}`, ic),
		// Anime - [SubGroup] Partial season pack
		c.re(`^\[(?<subgroup>.+?)\][-_. ]?(?<title>.+?)(?:\W+S(?<season>(?<!\d+)(?:\d{1,2})(?!\d+))\W+(?:(?:(?:Part|Vol)\W?|(?<!\d+\W+)e|p)(?<seasonpart>\d{1,2}(?!\d+)))+)`, ic),
		// Anime - [SubGroup] Title with trailing number Absolute Episode Number
		c.re(`^\[(?<subgroup>.+?)\][-_. ]?(?<title>[^-]+?)(?:(?<![-_. ]|\b[0]\d+) - )(?:[-_. ]?(?<absoluteepisode>\d{2,3}(\.\d{1,2})?(?!\d+)))+(?:[-_. ]+(?<special>special|ova|ovd))?.*?(?<hash>[(\[][0-9A-F]{8}[)\]])?(?:$|\.mkv)`, ic),
		// Anime - [SubGroup] Title with trailing number S## (Full season)
		c.re(`^\[(?<subgroup>.+?)\][-_. ]?(?<title>.+?)[-_. ]+(?:S(?<season>(?<!\d+)(?:\d{1,2}|\d{4})(?![ex]?\d+))).+?(?:$|\.mkv)`, ic),
		// Anime - [SubGroup] Title with trailing number Absolute Episode Number
		c.re(`^\[(?<subgroup>.+?)\][-_. ]?(?<title>[^-]+?)(?:(?<![-_. ]|\b[0]\d+)[_ ]+)(?:[-_. ]?(?<absoluteepisode>\d{3}(\.\d{1,2})?(?!\d+|-[a-z]+)))+(?:[-_. ]+(?<special>special|ova|ovd))?.*?(?<hash>[(\[][0-9A-F]{8}[)\]])?(?:$|\.mkv)`, ic),
		// Anime - [SubGroup] Title - Absolute Episode Number
		c.re(`^\[(?<subgroup>.+?)\][-_. ]?(?<title>.+?)(?:(?<!\b[0]\d+))(?:[. ]-[. ](?<absoluteepisode>\d{2,3}(\.\d{1,2})?(?!\d+|[-])))+(?:[-_. ]+(?<special>special|ova|ovd))?.*?(?<hash>[(\[][0-9A-F]{8}[)\]])?(?:$|\.mkv)`, ic),
		// Anime - [SubGroup] Title Absolute Episode Number - Absolute Episode Number (batches without full separator between title and absolute episode numbers)
		c.re(`^\[(?<subgroup>.+?)\][-_. ]?(?<title>.+?)(?:(?<!\b[0]\d+))(?<absoluteepisode>\d{2,3}(\.\d{1,2})?(?!\d+|[-]))[. ]-[. ](?<absoluteepisode>\d{2,3}(\.\d{1,2})?(?!\d+|[-]))(?:[-_. ]+(?<special>special|ova|ovd))?.*?(?<hash>[(\[][0-9A-F]{8}[)\]])?(?:$|\.mkv)`, ic),
		// Anime - [SubGroup] Title (Season only)
		c.re(`^(?:\[(?<subgroup>.+?)\][-_. ])?(?<title>.+?)[-_. ]+?[\[(](?:S|Season|Saison|Series|Stagione|Sezon)[-_. ]?(?<season>\d{1,2}(?![-_. ]?\d+))(?:[-_. )\]]|$)+(?<extras>EXTRAS|SUBPACK)?(?!\\)`, ic),
		// Anime - [SubGroup] Title Absolute Episode Number
		c.re(`^\[(?<subgroup>.+?)\][-_. ]?(?<title>.+?)[-_. ]+\(?(?:[-_. ]?#?(?<absoluteepisode>\d{2,3}(\.\d{1,2})?(?!\d+|-[a-z]+)))+\)?(?:[-_. ]+(?<special>special|ova|ovd))?.*?(?<hash>[(\[][0-9A-F]{8}[)\]])?(?:$|\.mkv)`, ic),
		// Multi-episode Repeated (S01E05 - S01E06)
		c.re(`^(?<title>.+?)(?:(?:[-_\W](?<![()\[!]))+S(?<season>(?<!\d+)(?:\d{1,2}|\d{4})(?!\d+))(?:(?:e|[-_. ]e){1,2}(?<episode>\d{1,3}(?!\d+)))+){2,}`, ic),
		// Multi-episode Repeated (1x05 - 1x06)
		c.re(`^(?<title>.+?)(?:(?:[-_\W](?<![()\[!]))+(?<season>(?<!\d+)(?:\d{1,2}|\d{4})(?!\d+))(?:x{1,2}(?<episode>\d{1,3}(?!\d+)))+){2,}`, ic),
		// Single episodes with a title (S01E05, 1x05, etc) followed by ".5 [SP]"
		c.re(`^(?<title>.+?)(?:(?:[-_\W](?<![()\[!]))+S?(?<season>(?<!\d+)(?:\d{1,2})(?!\d+))(?:[ex]|\W[ex]|_){1,2}(?<episode>\d{2,3}(?!\d+|(?:[ex]|\W[ex]|_|-){1,2}\d+))(?<special>\.5[ .]\[SP\]))`, ic),
		// Single episodes with a title (S01E05, 1x05, etc) and trailing info in slashes
		c.re(`^(?<title>.+?)(?:(?:[-_\W](?<![()\[!]))+S?(?<season>(?<!\d+)(?:\d{1,2})(?!\d+))(?:[ex]|\W[ex]|_){1,2}(?<episode>\d{2,3}(?!\d+|(?:[ex]|\W[ex]|_|-){1,2}\d+))).+?(?:\[.+?\])(?!\\)`, ic),
		// Anime - Title Season EpisodeNumber + Absolute Episode Number [SubGroup]
		c.re(`^(?<title>.+?)(?:[-_\W](?<![()\[!]))+(?:S?(?<season>(?<!\d+)\d{1,2}(?!\d+))(?:(?:[ex]|\W[ex]|-){1,2}(?<episode>(?<!\d+)\d{2}(?!\d+)))+)[-_. (]+?(?:[-_. ]?(?<absoluteepisode>(?<!\d+)\d{3}(\.\d{1,2})?(?!\d+|[pi])))+.+?\[(?<subgroup>.+?)\](?:$|\.mkv)`, ic),
		// Multi-Episode with a title (S01E05E06, S01E05-06, S01E05 E06, etc) and trailing info in slashes
		c.re(`^(?<title>.+?)(?:(?:[-_\W](?<![()\[!]))+S?(?<season>(?<!\d+)(?:\d{1,2})(?!\d+))(?:[ex]|\W[ex]|_){1,2}(?<episode>\d{2,3}(?!\d+))(?:(?:\-|[ex]|\W[ex]|_){1,2}(?<episode>\d{2,3}(?!\d+)))+).+?(?:\[.+?\])(?!\\)`, ic),
		// Anime - Title Absolute Episode Number [SubGroup] [Hash]? (Series Title Episode 99-100 [RlsGroup] [ABCD1234])
		c.re(`^(?<title>.+?)[-_. ]Episode(?:[-_. ]+(?<absoluteepisode>\d{2,3}(\.\d{1,2})?(?!\d+)))+(?:.+?)\[(?<subgroup>.+?)\].*?(?<hash>[(\[][0-9A-F]{8}[)\]])?$`, ic),
		// Anime - Title Absolute Episode Number [SubGroup] [Hash]
		c.re(`^(?<title>.+?)(?:(?:_|-|\s|\.)+(?<absoluteepisode>\d{3}(\.\d{1,2})(?!\d+)))+(?:.+?)\[(?<subgroup>.+?)\].*?(?<hash>[(\[][0-9A-F]{8}[)\]])?$`, ic),
		// Anime - Title Absolute Episode Number (Year) [SubGroup]
		c.re(`^(?<title>.+?)[-_. ]+(?<absoluteepisode>(?<!\d+)\d{2}(?!\d+))[-_. ](\(\d{4}\))[-_. ]\[(?<subgroup>.+?)\]`, ic),
		// Anime - Title with trailing number, Absolute Episode Number and hash
		c.re(`^(?<title>[^-]+?)(?:(?<![-_. ]|\b[0]\d+) - )(?:[-_. ]?(?<absoluteepisode>\d{2,3}(\.\d{1,2})?(?!\d+)))+(?:[-_. ]+(?<special>special|ova|ovd))?.*?(?<hash>[(\[][0-9A-F]{8}[)\]])(?:$|\.mkv)`, ic),
		// Anime - Title Absolute Episode Number [Hash]
		c.re(`^(?<title>.+?)(?:(?:_|-|\s|\.)+(?<absoluteepisode>\d{2,3}(\.\d{1,2})?(?!\d+)))+(?:[-_. ]+(?<special>special|ova|ovd))?[-_. ]+.*?(?<hash>[(\[][0-9A-F]{8}[)\]])$`, ic),
		// Episodes with airdate AND season/episode number, capture season/episode only
		c.re(`^(?<title>.+?)?\W*(?<airdate>\d{4}\W+[0-1][0-9]\W+[0-3][0-9])(?!\W+[0-3][0-9])[-_. ](?:s?(?<season>(?<!\d+)(?:\d{1,2})(?!\d+)))(?:[ex](?<episode>(?<!\d+)(?:\d{1,3})(?!\d+)))`, ic),
		// Episodes with airdate AND season/episode number
		c.re(`^(?<title>.+?)?\W*(?<airyear>\d{4})\W+(?<airmonth>[0-1][0-9])\W+(?<airday>[0-3][0-9])(?!\W+[0-3][0-9]).+?(?:s?(?<season>(?<!\d+)(?:\d{1,2})(?!\d+)))(?:[ex](?<episode>(?<!\d+)(?:\d{1,3})(?!\d+)))`, ic),
		// Episodes with absolute episode number AND airdate (TJET Wrestling)
		c.re(`^(?<title>.+?)?[-_. ](?:e\d{2,3}(?!\d+))[-_. ](?<airyear>\d{4})\W+(?<airmonth>[0-1][0-9])\W+(?<airday>[0-3][0-9])(?!\W+[0-3][0-9]).+?(?:\[[a-z]+\])`, ic),
		// Single or multi episode releases with multiple titles, then season and episode numbers after the last title. (Title1 / Title2 / ... / S1E1-2 of 6)
		c.re(`^((?<title>.*?)[ ._]\/[ ._])+\(?S(?<season>(?<!\d+)\d{1,2}(?!\d+))(?:\W|_)?E?[ ._]?(?<episode>(?<!\d+)\d{1,2}(?!\d+))(?:-(?<episode>(?<!\d+)\d{1,2}(?!\d+)))?(?:[ ._]of[ ._](?<episodecount>\d{1,2}))?\)?[ ._][\(\[]`, ic),
		// Multi-episode with title (S01E99-100, S01E05-06)
		c.re(`^(?<title>.+?)(?:[-_\W](?<![()\[!]))+S(?<season>(?<!\d+)(?:\d{1,2})(?!\d+))E(?<episode>\d{2,3}(?!\d+))(?:-(?<episode>\d{2,3}(?!\d+)))+(?:[-_. ]|$)`, ic),
		// Multi-episode with title (S01E05-06, S01E05-6)
		c.re(`^(?<title>.+?)(?:[-_\W](?<![()\[!]))+S(?<season>(?<!\d+)(?:\d{1,2})(?!\d+))E(?<episode>\d{1,2}(?!\d+))(?:-(?<episode>\d{1,2}(?!\d+)))+(?:[-_. ]|$)`, ic),
		// Episodes with a Chinese title and English title, Single episodes (S01E05, 1x05, etc) & Multi-episode (S01E05E06, S01E05-06, S01E05 E06, etc)
		c.re(`^(?<title>[\u4E00-\u9FCC]+)[_. ](?<title>.+?)(?:(?:[-_\W](?<![()\[!]))+S?(?<season>(?<!\d+)(?:\d{1,2})(?!\d+))(?:[ex]|\W[ex]){1,2}(?<episode>\d{2,3}(?!\d+))(?:(?:\-|[ex]|\W[ex]|_){1,2}(?<episode>\d{2,3}(?!\d+)))*)(?:[-_. ]|$)`, ic),
		// Episodes with a title, Single episodes (S01E05, 1x05, etc) & Multi-episode (S01E05E06, S01E05-06, S01E05 E06, etc)
		c.re(`^(?<title>.+?)(?:(?:[-_\W](?<![()\[!]))+S?(?<season>(?<!\d+)(?:\d{1,2})(?!\d+))(?:[ex]|\W[ex]){1,2}(?<episode>\d{2,3}(?!\d+))(?:(?:\-|[ex]|\W[ex]|_){1,2}(?<episode>\d{2,3}(?!\d+)))*)(?:[-_. ]|$)`, ic),
		// Episodes with a title, 4 digit season number, Single episodes (S2016E05, etc) & Multi-episode (S2016E05E06, S2016E05-06, S2016E05 E06, etc)
		c.re(`^(?<title>.+?)(?:(?:[-_\W](?<![()\[!]))+S(?<season>(?<!\d+)(?:\d{4})(?!\d+))(?:e|\We|_){1,2}(?<episode>\d{2,4}(?!\d+))(?:(?:\-|e|\We|_){1,2}(?<episode>\d{2,3}(?!\d+)))*)\W?(?!\\)`, ic),
		// Episodes with a title, 4 digit season number, Single episodes (2016x05, etc) & Multi-episode (2016x05x06, 2016x05-06, 2016x05 x06, etc)
		c.re(`^(?<title>.+?)(?:(?:[-_\W](?<![()\[!]))+(?<season>(?<!\d+)(?:\d{4})(?!\d+))(?:x|\Wx){1,2}(?<episode>\d{2,4}(?!\d+))(?:(?:\-|x|\Wx|_){1,2}(?<episode>\d{2,3}(?!\d+)))*)\W?(?!\\)`, ic),
		// Episodes with a title, Single episodes (s01.05)
		c.re(`^(?<title>.+?)(?:[-_\W](?<![()\[!]))+S(?<season>(?<!\d+)(?:\d{2})(?!\d+))(?:\.)(?<episode>\d{2,3}(?!\d+))(?:[-_. ]|$)`, ic),
		// Multi-season pack
		c.re(`^(?<title>.+?)(Complete Series)?[-_. ]+(?:S|(?:Season|Saison|Series|Stagione)[_. ])(?<season>(?<!\d+)(?:\d{1,2})(?!\d+))(?:(?:[-_. ]{1}|[-_. ]{3})(?:S|(?:Season|Saison|Series|Stagione)[_. ])?(?<season>(?<!\d+)(?:\d{1,2})(?!\d+))){1,}`, ic),
		// Partial season pack
		c.re(`^(?<title>.+?)(?:\W+S(?<season>(?<!\d+)(?:\d{1,2})(?!\d+))\W+(?:(?:(?:Part|Vol)\W?|(?<!\d+\W+)e|p)(?<seasonpart>\d{1,2}(?!\d+)))+)`, ic),
		// Anime - 4 digit absolute episode number in batch (1017-1088) or 1017-1088
		c.re(`^(?:\[(?<subgroup>.+?)\][-_. ]?)(?<title>.+?)[-_. ]+?\(?(?<absoluteepisode>\d{4}(\.\d{1,2})?(?!\d+))[-](?<absoluteepisode>\d{4}(\.\d{1,2})?(?!\d+))\)?`, ic),
		// Anime - 4 digit absolute episode number
		c.re(`^(?:\[(?<subgroup>.+?)\][-_. ]?)(?<title>.+?)[-_. ]+?(?<absoluteepisode>\d{4}(\.\d{1,2})?(?!\d+))`, ic),
		// Mini-Series, treated as season 1, multi episodes are labelled as E1-E2
		c.re(`^(?<title>.+?)(?:[-._ ][e])(?<episode>\d{2,3}(?!\d+))(?:(?:\-?[e])(?<episode>\d{2,3}(?!\d+)))+`, ic),
		// 5 digit episode number with a title
		c.re(`^(?:(?<title>.+?)(?:_|-|\s|\.)+)(?:S?(?<season>(?<!\d+)\d{1,2}(?!\d+)))(?:(?:\-|[ex]|\W[ex]|_){1,2}(?<episode>(?<!\d+)\d{5}(?!\d+)))`, ic),
		// 5 digit multi-episode with a title
		c.re(`^(?:(?<title>.+?)(?:_|-|\s|\.)+)(?:S?(?<season>(?<!\d+)\d{1,2}(?!\d+)))(?:(?:[-_. ]{1,3}ep){1,2}(?<episode>(?<!\d+)\d{5}(?!\d+)))+`, ic),
		// Episodes with airdate and part (2018.04.28.Part.2)
		c.re(`^(?<title>.+?)?\W*(?<airyear>\d{4})[-_. ]+(?<airmonth>[0-1][0-9])[-_. ]+(?<airday>[0-3][0-9])(?![-_. ]+[0-3][0-9])[-_. ]+Part[-_. ]?(?<part>[1-9])`, ic),
		// Episodes with airdate (2018.04.28)
		c.re(`^(?<title>.+?)?\W*(?<airyear>\d{4})[-_. ]+(?<airmonth>[0-1][0-9])[-_. ]+(?<airday>[0-3][0-9])(?![-_. ]+[0-3][0-9])`, ic),
		// Anime - Title 4-digit Absolute Episode Number [SubGroup]
		c.re(`^(?<title>.+?)[-_. ]+(?<absoluteepisode>(?<!\d+)\d{4}(?!\d+))[-_. ]\[(?<subgroup>.+?)\]`, ic),
		// Mini-Series with year in title, treated as season 1, episodes are labelled as Part01, Part 01, Part.1
		c.re(`^(?<title>.+?\d{4})(?:\W+(?:(?:Part\W?|e)(?<episode>\d{1,2}(?!\d+)))+)`, ic),
		// 3-digit season number
		// Single episode: Title.S001E01
		// Multi episode: Title.S001E01-E02
		c.re(`^(?<title>.+?)[-_. ]+S(?<season>(?<!\d+)\d{3}(?!\d+))E(?<episode>(?<!\d+)\d{2}(?!\d+))(?:-[ex](?<episode>(?<!\d+)\d{2}(?!\d+)))?([_. ]|$)(?!\\)`, ic),
		// Mini-Series, treated as season 1, episodes are labelled as XofY
		c.re(`^(?<title>.+?)(?:\W+(?:(?<episode>(?<!\d+)\d{1,2}(?!\d+))of\d+)+)`, ic),
		// Supports Season 01 Episode 03
		c.re(`(?:.*(?:\"|^))(?<title>.*?)(?:[-_\W](?<![()\[]))+(?:\W?Season\W?)(?<season>(?<!\d+)\d{1,2}(?!\d+))(?:\W|_)+(?:Episode\W)(?:[-_. ]?(?<episode>(?<!\d+)\d{1,2}(?!\d+)))+`, ic),
		// Multi-episode with episodes in square brackets (Series Title [S01E11E12] or Series Title [S01E11-12])
		c.re(`(?:.*(?:^))(?<title>.*?)[-._ ]+\[S(?<season>(?<!\d+)\d{2}(?!\d+))(?:[E-]{1,2}(?<episode>(?<!\d+)\d{2}(?!\d+)))+\]`, ic),
		// Multi-episode with episodes in brackets (Series Title (S01E11E12) or Series Title (S01E11-12) or Series Title (S01E1-3 of 12))
		c.re(`(?:.*(?:^))(?<title>.*?)[-._ ]+\(S(?<season>(?<!\d+)\d{2}(?!\d+))(?:[E-]{1,2}(?<episode>(?<!\d+)\d{1,2}(?!\d+)))+([._ ]+of[._ ]+[\dx]+)?\)`, ic),
		// Multi-episode release with no space between series title and season (S01E11E12)
		c.re(`(?:.*(?:^))(?<title>.*?)S(?<season>(?<!\d+)\d{2}(?!\d+))(?:E(?<episode>(?<!\d+)\d{2}(?!\d+)))+`, ic),
		// Multi-episode with single episode numbers (S6.E1-E2, S6.E1E2, S6E1E2, etc)
		c.re(`^(?<title>.+?)[-_. ]S(?<season>(?<!\d+)(?:\d{1,2}|\d{4})(?!\d+))(?:[-_. ]?[ex]?(?<episode>(?<!\d+)\d{1,2}(?!\d+)))+`, ic),
		// Single or multi episode releases with multiple titles, each followed by season and episode numbers in brackets
		c.re(`^(?<title>.*?)[ ._]\(S(?<season>(?<!\d+)\d{1,2}(?!\d+))(?:\W|_)?E?[ ._]?(?<episode>(?<!\d+)\d{1,2}(?!\d+))(?:-(?<episode>(?<!\d+)\d{1,2}(?!\d+)))?\)(?:[ ._]\/[ ._])(?<title>.*?)[ ._]\(`, ic),
		// Multi-episode with episodes in brackets (Series Title (S01E11E12) or Series Title (S1E11-12) or Series Title (S1E1-3 of 12))
		c.re(`(?:.*(?:^))(?<title>.*?)[-._ ]+\(S(?<season>(?<!\d+)\d{1}(?!\d+))(?:[E-]{1,2}(?<episode>(?<!\d+)\d{1,2}(?!\d+)))+([._ ]+of[._ ]+[\dx]+)?\)`, ic),
		// Single episode season or episode S1E1 or S1-E1 or S1.Ep1 or S01.Ep.01
		c.re(`(?:.*(?:\"|^))(?<title>.*?)(?:\W?|_)S(?<season>(?<!\d+)\d{1,2}(?!\d+))(?:\W|_)?Ep?[ ._]?(?<episode>(?<!\d+)\d{1,2}(?!\d+))`, ic),
		// 3 digit season S010E05
		c.re(`(?:.*(?:\"|^))(?<title>.*?)(?:\W?|_)S(?<season>(?<!\d+)\d{3}(?!\d+))(?:\W|_)?E(?<episode>(?<!\d+)\d{1,2}(?!\d+))`, ic),
		// Separated season and episode numbers S01 - E01
		c.re(`^(?<title>.+?)(?:_|-|\s|\.)+S(?<season>\d{2}(?!\d+))(\W-\W)E(?<episode>(?<!\d+)\d{2}(?!\d+))(?!\\)`, ic),
		// Season and episode numbers in square brackets (single and multi-episode)
		// Series Title - [02x01] - Episode 1
		// Series Title - [02x01x02] - Episode 1
		c.re(`^(?<title>.+?)?(?:[-_\W](?<![()\[!]))+\[(?:s)?(?<season>(?<!\d+)\d{1,2})(?:(?:[ex])(?<episode>\d{2}))(?:(?:[-ex]){1,2}(?<episode>\d{2}))*\].+?(?:\.|$)`, ic),
		// Anime - Title with season number - Absolute Episode Number (Title S01 - EP14)
		c.re(`^(?<title>.+?S\d{1,2})[-_. ]{3,}(?:EP)?(?<absoluteepisode>\d{2,3}(\.\d{1,2})?(?!\d+|[-]))`, ic),
		// Anime - French titles with single episode numbers, with or without leading sub group ([RlsGroup] Title - Episode 1)
		c.re(`^(?:\[(?<subgroup>.+?)\][-_. ]?)?(?<title>.+?)[-_. ]+?(?:Episode[-_. ]+?)(?<absoluteepisode>\d{1}(\.\d{1,2})?(?!\d+))`, ic),
		// Anime - Absolute episode number in square brackets
		c.re(`^(?:\[(?<subgroup>.+?)\][-_. ]?)(?<title>.+?)[-_. ]+?\[(?<absoluteepisode>\d{2,3}(\.\d{1,2})?(?!\d+))\]`, ic),
		// Japanese variety shows with leading date
		c.re(`^(?<airyear>\d{2})(?<airmonth>[0-1][0-9])(?<airday>[0-3][0-9])(?![-_. ]+[0-3][0-9])[-_. ](?<title>.+?)[-_. ](?:Season[-_. ]?(?<season>\d{1,2})[-_. ])?(?:ep|#)(?<episode>\d{2,3})`, ic),
		// Season only releases followed by year
		c.re(`^(?<title>.+?)[-_. ]+?(?:S|Season|Saison|Series|Stagione)[-_. ]?(?<season>\d{1,2}(?=[-_. ]\d{4}[-_. ]+))(?<extras>EXTRAS|SUBPACK)?(?!\\)`, ic),
		// Season only releases
		c.re(`^(?<title>.+?)[-_. ]+?(?:S|Season|Saison|Series|Stagione)[-_. ]?(?<season>\d{1,2}(?![-_. ]?\d+))(?:[-_. ]|$)+(?<extras>EXTRAS|SUBPACK)?(?!\\)`, ic),
		// 4 digit season only releases
		c.re(`^(?<title>.+?)[-_. ]+?(?:S|Season|Saison|Series|Stagione)[-_. ]?(?<season>\d{4}(?![-_. ]?\d+))(\W+|_|$)(?<extras>EXTRAS|SUBPACK)?(?!\\)`, ic),
		// Spanish tracker releases
		c.re(`^(?<title>.+?)(?:(?:[-_. ]+?Temporada.+?|\[.+?\])\[Cap)(?:[-_. ]+(?<season>(?<!\d+)\d{1,2})(?<episode>(?<!e|x)(?:[1-9][0-9]|[0][1-9])))+(?:\])`, ic),
		// Supports 103/113 naming
		c.re(`^(?<title>.+?)?(?:(?:[_.-](?<![()\[!]))+(?<season>(?<!\d+)[1-9])(?<episode>[1-9][0-9]|[0][1-9])(?![a-z]|\d+))+(?:[_.]|$)`, ic),
		// 4 digit episode number
		// Episodes without a title, Single (S01E05, 1x05) AND Multi (S01E04E05, 1x04x05, etc)
		c.re(`^(?:S?(?<season>(?<!\d+)\d{1,2}(?!\d+))(?:(?:\-|[ex]|\W[ex]|_){1,2}(?<episode>\d{4}(?!\d+|i|p)))+)(\W+|_|$)(?!\\)`, ic),
		// 4 digit episode number
		// Episodes with a title, Single episodes (S01E05, 1x05, etc) & Multi-episode (S01E05E06, S01E05-06, S01E05 E06, etc)
		c.re(`^(?<title>.+?)(?:(?:[-_\W](?<![()\[!]|\d{1,2}-))+S?(?<season>(?<!\d+)\d{1,2}(?!\d+))(?:(?:\-|[ex]|\W[ex]|_){1,2}(?<episode>\d{4}(?!\d+|i|p)))+)\W?(?!\\)`, ic),
		// Turkish tracker releases (01 BLM, 3. Blm, 04.Bolum, etc)
		c.re(`^(?<title>.+?)[_. ](?<absoluteepisode>\d{1,4})(?:[_. ]+)(?:BLM|B[oö]l[uü]m)`, ic),
		// Episodes with airdate (04.28.2018)
		c.re(`^(?<title>.+?)?\W*(?<ambiguousairmonth>[0-1][0-9])[-_. ]+(?<ambiguousairday>[0-3][0-9])[-_. ]+(?<airyear>\d{4})(?!\d+)`, ic),
		// Episodes with airdate (28.04.2018)
		c.re(`^(?<title>.+?)?\W*(?<ambiguousairday>[0-3][0-9])[-_. ]+(?<ambiguousairmonth>[0-1][0-9])[-_. ]+(?<airyear>\d{4})(?!\d+)`, ic),
		// Episodes with airdate (20180428)
		c.re(`^(?<title>.+?)?\W*(?<!\d+)(?<airyear>\d{4})(?<airmonth>[0-1][0-9])(?<airday>[0-3][0-9])(?!\d+)`, ic),
		// Anime - Title Absolute Episode Number (E195 or E1206)
		c.re(`^(?:\[(?<subgroup>.+?)\][-_. ]?)?(?<title>.+?)(?:(?:_|-|\s|\.)+(?:e|ep)(?<absoluteepisode>(\d{3}|\d{4})(\.\d{1,2})?))+[-_. ].*?(?<hash>[(\[][0-9A-F]{8}[)\]])?$`, ic),
		// Daily episodes that use short month format instead of number
		c.re(`^(?<title>.+?)[-_. ]+(?<airday>[1-2]\d|3[01]|[1-9])(?:th|st|rd)[-_. ](?<shortairmonth>jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[-_. ](?<airyear>19[4-9]\d|20\d\d)`, ic),
		// Supports 1103/1113 naming
		c.re(`^(?<title>.+?)?(?:(?:[-_. ](?<![()\[!]))*(?<!\d{1,2}-)(?<season>(?<!\d+|\(|\[|e|x)\d{2})(?<episode>(?<!e|x)(?:[1-9][0-9]|[0][1-9])(?!p|i|\d+|\)|\]|\W\d+|\W(?:e|ep|x)\d+)))+([-_. ]+|$)(?!\\)`, ic),
		// Dutch/Flemish release titles
		c.re(`^(?<title>.+?)[-_. ](?:Se\.(?<season>(?<!\d+)(?:\d{1,2}|\d{4})(?!\d+))(?:(?:[-_ ]?afl\.)(?<episode>\d{1,3}(?!\d+))(?:(?:[-]|[-_ ]en[-_ ])(?<episode>\d{1,3}(?!\d+)))*))`, ic),
		// Episodes with single digit episode number (S01E1, S01E5E6, etc)
		c.re(`^(?<title>.*?)(?:(?:[-_\W](?<![()\[!]))+S?(?<season>(?<!\d+)\d{1,2}(?!\d+))(?:(?:\-|[ex]){1,2}(?<episode>\d{1}))+)+(\W+|_|$)(?!\\)`, ic),
		// iTunes Season 1\05 Title (Quality).ext
		c.re(`^(?:Season(?:_|-|\s|\.)(?<season>(?<!\d+)\d{1,2}(?!\d+)))(?:_|-|\s|\.)(?<episode>(?<!\d+)\d{1,2}(?!\d+))`, ic),
		// iTunes 1-05 Title (Quality).ext
		c.re(`^(?:(?<season>(?<!\d+)(?:\d{1,2})(?!\d+))(?:-(?<episode>\d{2,3}(?!\d+))))`, ic),
		// Mini-Series, treated as season 1, episodes are labelled as Part01, Part 01, Part.1
		c.re(`^(?<title>.+?)(?:\W+(?:(?:(?<!\()Part\W?|(?<!\d+\W+)e)(?<episode>\d{1,2}(?!\d+|\))))+)`, ic),
		// Mini-Series, treated as season 1, episodes are labelled as Part One/Two/Three/...Nine, Part.One, Part_One
		c.re(`^(?<title>.+?)(?:\W+(?:Part[-._ ](?<episode>One|Two|Three|Four|Five|Six|Seven|Eight|Nine)(?>[-._ ])))`, ic),
		// Anime Range - Title Absolute Episode Number (ep01-12)
		c.re(`^(?:\[(?<subgroup>.+?)\][-_. ]?)?(?<title>.+?)(?:_|\s|\.)+(?:e|ep)(?<absoluteepisode>\d{2,3}(\.\d{1,2})?)-(?<absoluteepisode>(?<!\d+)\d{1,2}(\.\d{1,2})?(?!\d+|-)).*?(?<hash>[(\[][0-9A-F]{8}[)\]])?$`, ic),
		// Anime - Title Absolute Episode Number (e66)
		c.re(`^(?:\[(?<subgroup>.+?)\][-_. ]?)?(?<title>.+?)(?:(?:_|-|\s|\.)+(?:e|ep)(?<absoluteepisode>\d{2,4}(\.\d{1,2})?))+[-_. ].*?(?<hash>[(\[][0-9A-F]{8}[)\]])?$`, ic),
		// Anime - Title Episode Absolute Episode Number (Series Title Episode 01)
		c.re(`^(?<title>.+?)[-_. ](?:Episode)(?:[-_. ]+(?<absoluteepisode>(?<!\d+)\d{2,3}(\.\d{1,2})?(?!\d+)))+.*?(?<hash>[(\[][0-9A-F]{8}[)\]])?$`, ic),
		// Anime Range - Title Absolute Episode Number (1 or 2 digit absolute episode numbers in a range, 1-10)
		c.re(`^(?:\[(?<subgroup>.+?)\][-_. ]?)?(?<title>.+?)[_. ]+(?<absoluteepisode>(?<!\d+)\d{1,2}(\.\d{1,2})?(?!\d+))-(?<absoluteepisode>(?<!\d+)\d{1,2}(\.\d{1,2})?(?!\d+|-)).*?(?<hash>[(\[][0-9A-F]{8}[)\]])?$`, ic),
		// Anime - Title Episode/Episodio Absolute Episode Number
		c.re(`^(?:\[(?<subgroup>.+?)\][-_. ]?)?(?<title>.+?)[-_. ]+(?:Episode|Episodio)(?:[-_. ]+(?<absoluteepisode>(?<!\d+)\d{2,4}(\.\d{1,2})?(?!\d+|[ip])))+.*?(?<hash>[(\[][0-9A-F]{8}[)\]])?$`, ic),
		// Anime - Title [Absolute Episode Number] from AniLibriaTV
		c.re(`^(?:\[(?<subgroup>.+?)\][-_. ]?)?(?<title>.+?)(?:[-_. ]\[)(?:(?:-?)(?<absoluteepisode>(?<!\d+)\d{2,3}(\.\d{1,2})?(?!\d+|[ip])))+(?:\][-_. ]).*?(?<hash>[(\[][0-9A-F]{8}[)\]])?$`, ic),
		// Simple season releases in the format 'Series (year) - Temporada season'
		c.re(`^(?<title>.+?)[_. ]-[_. ](?:Temporada)[-_. ]?(?<season>\d{1,2}(?![_. ]?\d+))(?:[_. ]|$)+(?<extras>EXTRAS|SUBPACK)?(?!\\)`, ic),
		// Anime - Title Absolute Episode Number
		c.re(`^(?:\[(?<subgroup>.+?)\][-_. ]?)?(?<title>.+?)(?:[-_. ]+(?<absoluteepisode>(?<!\d+)\d{2,4}(\.\d{1,2})?(?!\d+|[ip])))+.*?(?<hash>[(\[][0-9A-F]{8}[)\]])?$`, ic),
		// Anime - Title {Absolute Episode Number}
		c.re(`^(?:\[(?<subgroup>.+?)\][-_. ]?)?(?<title>.+?)(?:(?:[-_\W](?<![()\[!]))+(?<absoluteepisode>(?<!\d+)\d{2,3}(\.\d{1,2})?(?!\d+|[ip])))+.*?(?<hash>[(\[][0-9A-F]{8}[)\]])?$`, ic),
		// Extant, terrible multi-episode naming (extant.10708.hdtv-lol.mp4)
		c.re(`^(?<title>.+?)[-_. ](?<season>[0]?\d?)(?:(?<episode>\d{2}){2}(?!\d+))[-_. ]`, ic),
		// Episodes without a title, Single episode numbers (S1E1, 1x1)
		c.re(`^(?:S?(?<season>(?<!\d+)(?:\d{1,2}|\d{4})(?!\d+))(?:(?:[-_ ]?[ex])(?<episode>\d{1}(?!\d+))))`, ic),
		// Anime OVA special
		c.re(`^\[(?<subgroup>.+?)\][-_. ]?(?<title>.+?)(?:[-_. ]+(?<special>special|ova|ovd|ncop|nced)).*?(?<hash>[(\[][0-9A-F]{8}[)\]])?(?:$|\.mkv)`, ic),
		// Episodes with a title, 3 digit season number, Single episodes (S001E05, 1x05, etc) & Multi-episode (S001E05E06, S001E05-06, S001E05 E06, etc)
		c.re(`^(?<title>.+?)(?:(?:[-_\W](?<![()\[!]))+S?(?<season>(?<!\d+)(?:\d{3})(?!\d+))(?:[ex]|\W[ex]){1,2}(?<episode>\d{2,3}(?!\d+))(?:(?:\-|[ex]|\W[ex]|_){1,2}(?<episode>\d{2,3}(?!\d+)))*)(?:[-_. ]|$)`, ic),
	}
	t.rejectHashed = []*regexp2.Regexp{
		// Generic match for md5 and mixed-case hashes.
		c.re(`^[0-9a-zA-Z]{32}`, regexp2.None),
		// Generic match for shorter lower-case hashes.
		c.re(`^[a-z0-9]{24}$`, regexp2.None),
		// Format seen on some NZBGeek releases
		// Be very strict with these coz they are very close to the valid 101 ep numbering.
		c.re(`^[A-Z]{11}\d{3}$`, regexp2.None),
		c.re(`^[a-z]{12}\d{3}$`, regexp2.None),
		// Backup filename (Unknown origins)
		c.re(`^Backup_\d{5,}S\d{2}-\d{2}$`, regexp2.None),
		// 123 - Started appearing December 2014
		c.re(`^123$`, regexp2.None),
		// abc - Started appearing January 2015
		c.re(`^abc$`, ic),
		// abc - Started appearing 2020
		c.re(`^abc[-_. ]xyz`, ic),
		// b00bs - Started appearing January 2015
		c.re(`^b00bs$`, ic),
		// 170424_26 - Started appearing August 2018
		c.re(`^\d{6}_\d{2}$`, regexp2.None),
		// additional Generic match for mixed-case hashes. - Started appearing Dec 2020
		c.re(`^[0-9a-zA-Z]{30}`, regexp2.None),
		// additional Generic match for mixed-case hashes. - Started appearing Jan 2021
		c.re(`^[0-9a-zA-Z]{26}`, regexp2.None),
		// additional Generic match for mixed-case hashes. - Started appearing Jan 2021
		c.re(`^[0-9a-zA-Z]{39}`, regexp2.None),
		// additional Generic match for mixed-case hashes. - Started appearing Jan 2021
		c.re(`^[0-9a-zA-Z]{24}`, regexp2.None),
	}
	t.seasonFolder = c.re(`^(Season[ ._-]*\d+|Specials)$`, regexp2.None)
	// Detects whether the title was reversed.
	t.reversed = c.re(`(?:^|[-._ ])(p027|p0801|\d{2,3}E-?\d{2}S)[-._ ]`, regexp2.None)
	t.simpleTitle = c.re(`(?:(480|540|576|720|1080|1440|2160)[ip]|[xh][\W_]?26[456]|DD\W?5\W1|[<>?*]|848x480|1280x720|1920x1080|3840x2160|4096x2160|(?<![a-f0-9])(8|10)[ -]?(b(?![a-z0-9])|bit))\s*?`, ic)
	t.sixDigitAirDate = c.re(`(?<=[_.-])(?<airdate>(?<!\d)(?<airyear>[1-9]\d{1})(?<airmonth>[0-1][0-9])(?<airday>[0-3][0-9]))(?=[_.-])`, ic)
	t.cleanBrackets = c.re(`\[[a-z0-9 ._-]+\]$`, ic)
	t.yearInTitle = c.re(`^(?<title>.+?)[-_. ]+?[\(\[]?(?<year>\d{4})[\]\)]?`, ic)
	t.titleComponents = c.re(`^(?:(?<title>.+?) \((?<title>.+?)\)|(?<title>.+?) \| (?<title>.+?)|(?<title>.+?) AKA (?<title>.+?)|(?<title>.+?) / (?<title>.+?))$`, ic)
	t.requestInfo = c.re(`^(?:\[.+?\])+`, regexp2.None)
	t.fileExtension = c.re(`\.[a-z0-9]{2,4}$`, ic)
	t.common = newCommonTable(c)
	if c.err != nil {
		return nil, c.err
	}
	var err error
	if t.lang, err = newSeriesLanguageTable(); err != nil {
		return nil, err
	}
	if t.group, err = newSeriesGroupTable(); err != nil {
		return nil, err
	}
	return t, nil
}

// all is Regex.Matches: every successive match of re in s.
func (b *budget) all(re *regexp2.Regexp, s string) ([]*regexp2.Match, error) {
	var out []*regexp2.Match
	m, err := b.first(re, s)
	for m != nil && err == nil {
		out = append(out, m)
		m, err = b.next(re, s, m)
	}
	return out, err
}

// removeFileExtension is FileExtensions.RemoveFileExtension: a trailing dot
// and two to four letters or digits are dropped only when they are a known
// media file extension (or .par2 / .nzb); any other tail is part of the name.
func (t *seriesTitleTable) removeFileExtension(b *budget, title string) (string, error) {
	var evalErr error
	return b.replaceFunc(t.fileExtension, title, &evalErr, func(m regexp2.Match) string {
		ext := strings.ToLower(m.String())
		if seriesExtensionQuality(ext) != sqUnknown || ext == ".par2" || ext == ".nzb" {
			return ""
		}
		return m.String()
	})
}

// reverseUTF16 is Array.Reverse over the UTF-16 code units of s. A reversed
// surrogate pair has no text form and decodes to the replacement character.
func reverseUTF16(s string) string {
	units := utf16.Encode([]rune(s))
	slices.Reverse(units)
	return string(utf16.Decode(units))
}

// shortMonths is the source's ShortMonths dictionary.
var shortMonths = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

// numberWords is the source's Numbers array: the word of each digit.
var numberWords = [...]string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine"}

// parse is ParseTitle: nil with a nil error is the source's null (the name was
// rejected or no pattern produced a result). loc is the local calendar of the
// air date limit.
func (t *seriesTitleTable) parse(b *budget, loc *time.Location, title string) (*EpisodeInfo, error) {
	valid, err := t.validateBeforeParsing(b, title)
	if err != nil || !valid {
		return nil, err
	}

	reversed, err := b.isMatch(t.reversed, title)
	if err != nil {
		return nil, err
	}
	if reversed {
		withoutExtension, err := t.removeFileExtension(b, title)
		if err != nil {
			return nil, err
		}
		title = reverseUTF16(withoutExtension) + utf16Suffix(title, utf16Len(withoutExtension))
	}

	releaseTitle, err := t.removeFileExtension(b, title)
	if err != nil {
		return nil, err
	}
	releaseTitle = strings.ReplaceAll(strings.ReplaceAll(releaseTitle, "【", "["), "】", "]")
	for _, s := range t.preSubstitutions {
		if releaseTitle, err = b.replace(s.re, releaseTitle, s.replacement); err != nil {
			return nil, err
		}
	}

	simpleTitle, err := b.replace(t.simpleTitle, releaseTitle, "")
	if err != nil {
		return nil, err
	}
	// Quick fix stripping [url] - prefixes and postfixes.
	if simpleTitle, err = b.replace(t.common.websitePrefix, simpleTitle, ""); err != nil {
		return nil, err
	}
	if simpleTitle, err = b.replace(t.common.websitePostfix, simpleTitle, ""); err != nil {
		return nil, err
	}
	if simpleTitle, err = b.replace(t.common.torrentSuffix, simpleTitle, ""); err != nil {
		return nil, err
	}
	var evalErr error
	if simpleTitle, err = b.replaceFunc(t.cleanBrackets, simpleTitle, &evalErr, func(m regexp2.Match) string {
		q, err := t.quality.parseQualityName(b, m.String())
		if err != nil {
			evalErr = err
			return m.String()
		}
		if q.Code != t.quality.defs[sqUnknown].code {
			return ""
		}
		return m.String()
	}); err != nil {
		return nil, err
	}

	if simpleTitle, err = t.fixSixDigitAirDate(b, simpleTitle); err != nil {
		return nil, err
	}

	for _, re := range t.titles {
		matches, err := b.all(re, simpleTitle)
		if err != nil {
			return nil, err
		}
		if len(matches) == 0 {
			continue
		}
		result, err := t.parseMatches(b, loc, simpleTitle, matches, releaseTitle)
		switch {
		case errors.Is(err, errUnparseable):
			return nil, nil
		case err != nil:
			return nil, err
		case result == nil:
			continue
		}
		return t.complete(b, result, matches[0], title, releaseTitle)
	}
	return nil, nil
}

// fixSixDigitAirDate turns a six-digit air date (yymmdd between separators)
// into the dotted four-digit-year form the daily patterns read.
func (t *seriesTitleTable) fixSixDigitAirDate(b *budget, simpleTitle string) (string, error) {
	m, err := b.first(t.sixDigitAirDate, simpleTitle)
	if err != nil || m == nil {
		return simpleTitle, err
	}
	airMonth, airDay := groupText(m, "airmonth"), groupText(m, "airday")
	if airMonth == "00" && airDay == "00" {
		return simpleTitle, nil
	}
	fixed := "20" + groupText(m, "airyear") + "." + airMonth + "." + airDay
	return strings.ReplaceAll(simpleTitle, groupText(m, "airdate"), fixed), nil
}

// validateBeforeParsing is the source's ValidateBeforeParsing.
func (t *seriesTitleTable) validateBeforeParsing(b *budget, title string) (bool, error) {
	lower := strings.ToLower(title)
	if strings.Contains(lower, "password") && strings.Contains(lower, "yenc") {
		return false, nil
	}
	if !hasLetterOrDigit(title) {
		return false, nil
	}
	withoutExtension, err := t.removeFileExtension(b, title)
	if err != nil {
		return false, err
	}
	for _, re := range t.rejectHashed {
		hashed, err := b.isMatch(re, withoutExtension)
		if err != nil || hashed {
			return false, err
		}
	}
	folder, err := b.isMatch(t.seasonFolder, withoutExtension)
	return !folder && err == nil, err
}

// complete runs the part of ParseTitle after a pattern produced a result: the
// special-season fix, languages, quality, release group and hash.
func (t *seriesTitleTable) complete(b *budget, result *EpisodeInfo, match *regexp2.Match, title, releaseTitle string) (*EpisodeInfo, error) {
	if result.FullSeason && containsFold(result.ReleaseTokens, "special") {
		result.FullSeason = false
		result.Special = true
	}

	var err error
	if result.Languages, err = t.lang.parse(b, result.ReleaseTokens); err != nil {
		return nil, err
	}
	if result.Quality, err = t.quality.parseQuality(b, title); err != nil {
		return nil, err
	}
	if result.ReleaseGroup, err = t.group.parse(b, t, releaseTitle); err != nil {
		return nil, err
	}
	if sub := groupText(match, "subgroup"); !isBlank(sub) {
		result.ReleaseGroup = sub
	}
	if hash := groupText(match, "hash"); groupOK(match, "hash") {
		if hash = strings.Trim(hash, "[]()"); hash != "1280x720" {
			result.ReleaseHash = hash
		}
	}
	return result, nil
}

// endIndex is Group.EndIndex(): the end, in UTF-16 units of s, of the group's
// last capture, 0 for a group that took no part.
func endIndex(s string, m *regexp2.Match, name string) int {
	if !groupOK(m, name) {
		return 0
	}
	return captureEnd(s, m.GroupByName(name).Capture)
}

// captureEnd is Capture.EndIndex(): the end of c, in UTF-16 units of s.
func captureEnd(s string, c regexp2.Capture) int {
	at, n := c.ByteRange()
	return utf16Len(s[:at+n])
}

// captures are the values of every capture of the named group, in order, and
// their ends in UTF-16 units of s.
func captures(s string, m *regexp2.Match, name string) (values []string, ends []int) {
	if !groupOK(m, name) {
		return nil, nil
	}
	for _, c := range m.GroupByName(name).Captures {
		values = append(values, c.String())
		ends = append(ends, captureEnd(s, c))
	}
	return values, ends
}

// numerals is ConvertToNumerals(value.Normalize(FormKC)) over the characters a
// number pattern can capture (decimal digits): the compatibility digits fold to
// ASCII, then every decimal digit of the Basic Multilingual Plane becomes its
// ASCII digit. A digit outside it is two UTF-16 units to the source, which are
// not numbers, so it stays and the number does not parse.
func numerals(value string) string {
	var sb strings.Builder
	for _, r := range value {
		switch {
		case r >= 0xFF10 && r <= 0xFF19: // fullwidth digits
			r = '0' + (r - 0xFF10)
		case r >= 0x1D7CE && r <= 0x1D7FF: // mathematical digits
			r = '0' + (r-0x1D7CE)%10
		case r < 0x10000 && unicode.IsDigit(r) && (r < '0' || r > '9'):
			for _, rg := range unicode.Nd.R16 {
				if uint16(r) >= rg.Lo && uint16(r) <= rg.Hi {
					r = '0' + rune(uint16(r)-rg.Lo)%10
					break
				}
			}
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

// parseNumber is the source's ParseNumber: an integer, or the word of a digit.
func parseNumber(value string) (int, error) {
	if n, ok := tryParseInt(numerals(value)); ok {
		return n, nil
	}
	if i := slices.Index(numberWords[:], strings.ToLower(value)); i >= 0 {
		return i, nil
	}
	return 0, errUnparseable
}

// decimal is an exact non-negative decimal number: its integer digits and its
// fraction digits as written.
type decimal struct {
	whole int64
	frac  string
}

// parseDecimal is the source's ParseDecimal for the numbers a pattern captures:
// digits with an optional dot and fraction digits.
func parseDecimal(value string) (decimal, error) {
	whole, frac, hasFrac := strings.Cut(numerals(value), ".")
	digits := func(s string) bool {
		return s != "" && strings.Trim(s, "0123456789") == ""
	}
	n, err := strconv.ParseInt(whole, 10, 64)
	if !digits(whole) || err != nil || (hasFrac && !digits(frac)) {
		return decimal{}, errUnparseable
	}
	return decimal{n, frac}, nil
}

// compare orders two decimals numerically.
func (d decimal) compare(o decimal) int {
	if d.whole != o.whole {
		return int(d.whole - o.whole)
	}
	n := max(len(d.frac), len(o.frac))
	return strings.Compare(d.frac+strings.Repeat("0", n-len(d.frac)), o.frac+strings.Repeat("0", n-len(o.frac)))
}

// integral reports whether the decimal has no fractional part (d % 1 == 0).
func (d decimal) integral() bool { return strings.Trim(d.frac, "0") == "" }

// text is the decimal as text: its integer digits, the fraction as written.
func (d decimal) text() string {
	if d.frac == "" {
		return strconv.FormatInt(d.whole, 10)
	}
	return strconv.FormatInt(d.whole, 10) + "." + d.frac
}

// intRange is Enumerable.Range(start, count).ToArray().
func intRange(start, count int) []int {
	out := make([]int, count)
	for i := range out {
		out[i] = start + i
	}
	return out
}

// parseMatches is ParseMatchCollection: nil with a nil error is the source's
// null (the matches name no usable result).
func (t *seriesTitleTable) parseMatches(b *budget, loc *time.Location, simple string, matches []*regexp2.Match, releaseTitle string) (*EpisodeInfo, error) {
	first := matches[0]
	seriesName := strings.ReplaceAll(strings.ReplaceAll(groupText(first, "title"), ".", " "), "_", " ")
	seriesName, err := b.replace(t.requestInfo, seriesName, "")
	if err != nil {
		return nil, err
	}
	seriesName = strings.Trim(seriesName, " ")

	airYear, _ := tryParseInt(groupText(first, "airyear"))
	lastIndex := endIndex(simple, first, "title")

	var result *EpisodeInfo
	if airYear < 1900 {
		result, err = t.episodic(simple, matches, releaseTitle, &lastIndex)
	} else {
		result, err = t.daily(b, loc, simple, first, airYear, releaseTitle, &lastIndex)
	}
	if err != nil || result == nil {
		return nil, err
	}

	if lastIndex < utf16Len(releaseTitle) {
		result.ReleaseTokens = utf16Suffix(releaseTitle, lastIndex)
	} else {
		result.ReleaseTokens = releaseTitle
	}
	result.SeriesTitle = seriesName
	if result.SeriesTitleInfo, err = t.seriesTitleInfo(b, seriesName, first); err != nil {
		return nil, err
	}
	return result, nil
}

// episodic is the part of ParseMatchCollection for a result without an air date:
// season, episode and absolute episode numbers and the release type flags.
func (t *seriesTitleTable) episodic(simple string, matches []*regexp2.Match, releaseTitle string, lastIndex *int) (*EpisodeInfo, error) {
	first := matches[0]
	result := &EpisodeInfo{
		ReleaseTitle:           releaseTitle,
		SeasonNumbers:          []int{0},
		EpisodeNumbers:         []int{},
		AbsoluteEpisodeNumbers: []int{},
	}

	for _, m := range matches {
		episodes, episodeEnds := captures(simple, m, "episode")
		absolutes, absoluteEnds := captures(simple, m, "absoluteepisode")

		// A list of no episodes is a full season release.
		if len(episodes) > 0 {
			firstNumber, err := parseNumber(episodes[0])
			if err != nil {
				return nil, err
			}
			lastNumber, err := parseNumber(episodes[len(episodes)-1])
			if err != nil {
				return nil, err
			}
			if firstNumber > lastNumber {
				return nil, nil
			}
			result.EpisodeNumbers = intRange(firstNumber, lastNumber-firstNumber+1)
			*lastIndex = max(*lastIndex, episodeEnds[len(episodeEnds)-1])
			if groupOK(m, "special") {
				result.Special = true
			}
			if groupOK(m, "splitepisode") {
				result.IsSplitEpisode = true
			}
		}

		if len(absolutes) > 0 {
			firstNumber, err := parseDecimal(absolutes[0])
			if err != nil {
				return nil, err
			}
			lastNumber, err := parseDecimal(absolutes[len(absolutes)-1])
			if err != nil {
				return nil, err
			}
			if firstNumber.compare(lastNumber) > 0 {
				return nil, nil
			}
			if !firstNumber.integral() || !lastNumber.integral() {
				if len(absolutes) != 1 {
					return nil, nil // Multiple matches not allowed for specials
				}
				result.SpecialAbsoluteEpisodeNumbers = []string{firstNumber.text()}
				result.Special = true
				*lastIndex = max(*lastIndex, absoluteEnds[0])
			} else {
				result.AbsoluteEpisodeNumbers = intRange(int(firstNumber.whole), int(lastNumber.whole-firstNumber.whole+1))
				if groupOK(m, "special") {
					result.Special = true
				}
				*lastIndex = max(*lastIndex, absoluteEnds[len(absoluteEnds)-1])
			}
		}

		if len(episodes) == 0 && len(absolutes) == 0 {
			// An "Extras" or "SUBPACK" release is flagged so it can be filtered out.
			if !isBlank(groupText(first, "extras")) {
				result.IsSeasonExtra = true
			}
			// Partial season packs have a seasonpart group, which tells them from a
			// full season or single episode release.
			if seasonPart := groupText(first, "seasonpart"); !isBlank(seasonPart) {
				n, ok := tryParseInt(seasonPart)
				if !ok {
					return nil, errUnparseable
				}
				result.SeasonPart = n
				result.IsPartialSeason = true
			} else if groupOK(first, "special") {
				result.Special = true
			} else {
				result.FullSeason = true
			}
		}

		if len(episodes) == 2 && groupOK(first, "episodecount") && episodes[1] == groupText(first, "episodecount") {
			result.EpisodeNumbers = []int{}
			result.FullSeason = true
		}
	}

	var seasons []int
	seasonValues, seasonEnds := captures(simple, first, "season")
	for i, v := range seasonValues {
		if n, ok := tryParseInt(v); ok {
			seasons = append(seasons, n)
			*lastIndex = max(*lastIndex, seasonEnds[i])
		}
	}
	slices.Sort(seasons)
	seasons = slices.Compact(seasons)

	switch {
	case len(seasons) == 1 || len(seasons) > 2:
		result.SeasonNumbers = seasons
	case len(seasons) == 2:
		result.SeasonNumbers = intRange(seasons[0], seasons[1]-seasons[0]+1)
	case len(result.AbsoluteEpisodeNumbers) == 0 && len(result.EpisodeNumbers) > 0:
		// No season and not an absolute only release: a mini series, season 1.
		result.SeasonNumbers = []int{1}
		result.IsMiniSeries = true
	}
	return result, nil
}

// daily is the part of ParseMatchCollection for a result with an air date. An
// ambiguous, impossible, too distant or too old date is errUnparseable.
func (t *seriesTitleTable) daily(b *budget, loc *time.Location, simple string, m *regexp2.Match, airYear int, releaseTitle string, lastIndex *int) (*EpisodeInfo, error) {
	number := func(name string) (int, error) {
		n, ok := tryParseInt(groupText(m, name))
		if !ok {
			return 0, errUnparseable
		}
		return n, nil
	}
	var airMonth, airDay int
	var err error
	switch {
	case groupOK(m, "ambiguousairmonth") && groupOK(m, "ambiguousairday"):
		if airMonth, err = number("ambiguousairmonth"); err != nil {
			return nil, err
		}
		if airDay, err = number("ambiguousairday"); err != nil {
			return nil, err
		}
		if airDay <= 12 && airMonth <= 12 {
			return nil, errUnparseable // Ambiguous Date, cannot validate month and day
		}
	case groupOK(m, "shortairmonth"):
		month, ok := shortMonths[strings.ToLower(groupText(m, "shortairmonth"))]
		if !ok {
			return nil, errUnparseable
		}
		airMonth = month
		if airDay, err = number("airday"); err != nil {
			return nil, err
		}
	default:
		if airMonth, err = number("airmonth"); err != nil {
			return nil, err
		}
		if airDay, err = number("airday"); err != nil {
			return nil, err
		}
	}
	// Swap day and month if month is bigger than 12 (scene fail).
	if airMonth > 12 {
		airDay, airMonth = airMonth, airDay
	}

	airDate := time.Date(airYear, time.Month(airMonth), airDay, 0, 0, 0, 0, time.UTC)
	if airDate.Year() != airYear || int(airDate.Month()) != airMonth || airDate.Day() != airDay {
		return nil, errUnparseable // Invalid date found
	}
	// Allow early releases, but reject dates too far in the future (most likely a
	// parse error): the limit is the local date 14 days after now.
	y, month, d := b.now().In(loc).Date()
	if airDate.After(time.Date(y, month, d+14, 0, 0, 0, 0, time.UTC)) {
		return nil, errUnparseable
	}
	// A date before 1970 is only valid with a title year (the recorded-TV format).
	if airDate.Before(time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)) && isBlank(groupText(m, "titleyear")) {
		return nil, errUnparseable
	}

	for _, name := range [...]string{"airyear", "airmonth", "airday"} {
		*lastIndex = max(*lastIndex, endIndex(simple, m, name))
	}
	result := &EpisodeInfo{
		ReleaseTitle:           releaseTitle,
		SeasonNumbers:          []int{0},
		EpisodeNumbers:         []int{},
		AbsoluteEpisodeNumbers: []int{},
		AirDate:                airDate.Format("2006-01-02"),
	}
	if groupOK(m, "part") {
		part, err := number("part")
		if err != nil {
			return nil, err
		}
		result.DailyPart = &part
	}
	return result, nil
}

// seriesTitleInfo is GetSeriesTitleInfo: the title, the title without its year,
// the year and every alternative title.
func (t *seriesTitleTable) seriesTitleInfo(b *budget, title string, first *regexp2.Match) (SeriesTitleInfo, error) {
	info := SeriesTitleInfo{Title: title}
	m, err := b.first(t.yearInTitle, title)
	if err != nil {
		return SeriesTitleInfo{}, err
	}
	if m == nil {
		info.TitleWithoutYear = title
	} else {
		info.TitleWithoutYear = groupText(m, "title")
		var ok bool
		if info.Year, ok = tryParseInt(groupText(m, "year")); !ok {
			return SeriesTitleInfo{}, errUnparseable
		}
	}

	components, err := b.first(t.titleComponents, info.TitleWithoutYear)
	if err != nil {
		return SeriesTitleInfo{}, err
	}
	switch {
	case components != nil:
		info.AllTitles, _ = captures(info.TitleWithoutYear, components, "title")
	case groupOK(first, "title") && len(first.GroupByName("title").Captures) > 1:
		for _, c := range first.GroupByName("title").Captures {
			info.AllTitles = append(info.AllTitles, strings.ReplaceAll(strings.ReplaceAll(c.String(), ".", " "), "_", " "))
		}
	}
	return info, nil
}
