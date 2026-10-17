// SPDX-License-Identifier: GPL-3.0

package parse

// Movie title parsing, ported from the movie reference parser (parser/Parser.cs
// ParseMovieTitle and the parser/ParserCommon.cs and parser/RegexReplace.cs
// members it uses, located by BOXSET_REF_MOVIE) at commit
// df2faa64c75e6081556d11232dc46523e7264ea1. Control flow, pattern order and
// pattern text follow the source; the parity corpus testdata/parser/movie.jsonl
// is extracted from its tests.
//
// Faithful-adapter notes:
//   - Every pattern is compiled once with the table and all matching goes
//     through the shared budget of the public operation (regex.go): the
//     quality, language and release group parsing ParseMovieTitle runs share
//     that one budget and never get a fresh one.
//   - The source's exception handler turns any failure into "unable to parse";
//     here only the one reachable case (a replacement range outside the
//     string) is that, while a budget, timeout or cancellation failure is an
//     error and never a quiet no-match.
//   - String indexes of the source are UTF-16 code units: the title group's
//     index is converted from bytes and applied in UTF-16 units.

import (
	"strings"

	"github.com/dlclark/regexp2/v2"
)

// movieEditionPattern is the source's EditionRegex; several title patterns
// embed it by concatenation.
const movieEditionPattern = `\(?\b(?<edition>(((Recut.|Extended.|Ultimate.)?(Director.?s|Collector.?s|Theatrical|Ultimate|Extended|Despecialized|(Special|Rouge|Final|Assembly|Imperial|Diamond|Signature|Hunter|Rekall)(?=(.(Cut|Edition|Version)))|\d{2,3}(th)?.Anniversary)(?:.(Cut|Edition|Version))?(.(Extended|Uncensored|Remastered|Unrated|Uncut|Open.?Matte|IMAX|Fan.?Edit))?|((Uncensored|Remastered|Unrated|Uncut|Open?.Matte|IMAX|Fan.?Edit|Restored|((2|3|4)in1))))))\b\)?`

// movieTitleTable is the immutable set of compiled patterns of the movie title
// parser, with the language and release group tables it runs.
type movieTitleTable struct {
	quality *movieTable
	lang    *languageTable
	group   *releaseGroupTable

	edition       *regexp2.Regexp // EditionRegex
	reportEdition *regexp2.Regexp // ReportEditionRegex
	hardcodedSubs *regexp2.Regexp
	titles        []*regexp2.Regexp // ReportMovieTitleRegex, in source order
	folder        *regexp2.Regexp   // ReportMovieTitleFolderRegex
	rejectHashed  []*regexp2.Regexp
	reversed      *regexp2.Regexp
	alternative   *regexp2.Regexp
	bracketedAlt  *regexp2.Regexp
	normalizeAlt  *regexp2.Regexp
	imdbID        *regexp2.Regexp
	tmdbID        *regexp2.Regexp
	simpleTitle   *regexp2.Regexp
	simpleRelease *regexp2.Regexp
	cleanBrackets *regexp2.Regexp
	requestInfo   *regexp2.Regexp
	fileExtension *regexp2.Regexp
	common        commonTable
}

// commonTable is ParserCommon: the website and torrent suffix cleanup shared by
// the title and release group parsers.
type commonTable struct {
	websitePrefix  *regexp2.Regexp
	websitePostfix *regexp2.Regexp
	torrentSuffix  *regexp2.Regexp
}

func newCommonTable(c *compiler) commonTable {
	return commonTable{
		websitePrefix:  c.re(`^(?:(?:\[|\()\s*)?(?:www\.)?[-a-z0-9-]{1,256}\.(?<!Naruto-Kun\.)(?:[a-z]{2,6}\.[a-z]{2,6}|xn--[a-z0-9-]{4,}|[a-z]{2,})\b(?:\s*(?:\]|\))|[ -]{2,})[ -]*`, ic),
		websitePostfix: c.re(`(?:\[\s*)?(?:www\.)?[-a-z0-9-]{1,256}\.(?:xn--[a-z0-9-]{4,}|[a-z]{2,6})\b(?:\s*\])$`, ic),
		torrentSuffix:  c.re(`\[(?:ettv|rartv|rarbg|cttv|publichd)\]$`, ic),
	}
}

func newMovieTitleTable(q *movieTable) (*movieTitleTable, error) {
	c := &compiler{}
	t := &movieTitleTable{quality: q}
	t.edition = c.re(movieEditionPattern, ic)
	t.reportEdition = c.re(`^.+?`+movieEditionPattern, ic)
	t.hardcodedSubs = c.re(`\b((?<hcsub>(\w+(?<!SOFT|MULTI|HORRIBLE)SUBS?))|(?<hc>(HC|SUBBED)))\b`, ic|regexp2.IgnorePatternWhitespace)
	t.titles = []*regexp2.Regexp{
		// Anime [Subgroup] with year explicitly in parentheses, e.g. [Sub] Title 1993 (1993) [VHS]
		c.re(`^(?:\[(?<subgroup>.+?)\][-_. ]?)(?<title>(?![(\[]).+?)\s*\((?<year>(1(8|9)|20)\d{2})\).*?(?<hash>\[\w{8}\])?(?:$|\.)`, ic),
		// Anime [Subgroup] and Year
		c.re(`^(?:\[(?<subgroup>.+?)\][-_. ]?)(?<title>(?![(\[]).+?)?(?:(?:[-_\W](?<![)\[!]))*(?<year>(1(8|9)|20)\d{2}(?!p|i|x|\d+|\]|\W\d+)))+.*?(?<hash>\[\w{8}\])?(?:$|\.)`, ic),
		// Anime [Subgroup] no year, versioned title, hash
		c.re(`^(?:\[(?<subgroup>.+?)\][-_. ]?)(?<title>(?![(\[]).+?)((v)(?:\d{1,2})(?:([-_. ])))(\[.*)?(?:[\[(][^])])?.*?(?<hash>\[\w{8}\])(?:$|\.)`, ic),
		// Anime [Subgroup] no year, info in double sets of brackets, hash
		c.re(`^(?:\[(?<subgroup>.+?)\][-_. ]?)(?<title>(?![(\[]).+?)(\[.*).*?(?<hash>\[\w{8}\])(?:$|\.)`, ic),
		// Anime [Subgroup] no year, info in parentheses or brackets, hash
		c.re(`^(?:\[(?<subgroup>.+?)\][-_. ]?)(?<title>(?![(\[]).+)(?:[\[(][^])]).*?(?<hash>\[\w{8}\])(?:$|\.)`, ic),
		// Some german or french tracker formats (missing year, ...); french removed as it broke all movies with french titles
		c.re(`^(?<title>(?![(\[]).+?)((\W|_))(`+movieEditionPattern+`.{1,3})?(?:(?<!(19|20)\d{2}.*?)(?<!(?:Good|The)[_ .-])(German|TrueFrench))(.+?)(?=((19|20)\d{2}|$))(?<year>(19|20)\d{2}(?!p|i|\d+|\]|\W\d+))?(\W+|_|$)(?!\\)`, ic),
		// Special, Despecialized, etc. Edition Movies, e.g: Mission.Impossible.3.Special.Edition.2011
		c.re(`^(?<title>(?![(\[]).+?)?(?:(?:[-_\W](?<![)\[!]))*`+movieEditionPattern+`.{1,3}(?<year>(1(8|9)|20)\d{2}(?!p|i|\d+|\]|\W\d+)))+(\W+|_|$)(?!\\)`, ic),
		// Normal movie format, e.g: Mission.Impossible.3.2011
		c.re(`^(?<title>(?![(\[]).+?)?(?:(?:[-_\W](?<![)\[!]))*(?<year>(1(8|9)|20)\d{2}(?!p|i|(1(8|9)|20)\d{2}|\]|\W(1(8|9)|20)\d{2})))+(\W+|_|$)(?!\\)`, ic),
		// PassThePopcorn Torrent names: Star.Wars[PassThePopcorn]
		c.re(`^(?<title>.+?)?(?:(?:[-_\W](?<![()\[!]))*(?<year>(\[\w *\])))+(\W+|_|$)(?!\\)`, ic),
		// That did not work? Maybe some tool uses [] for years.
		c.re(`^(?<title>(?![(\[]).+?)?(?:(?:[-_\W](?<![)!]))*(?<year>(1(8|9)|20)\d{2}(?!p|i|\d+|\W\d+)))+(\W+|_|$)(?!\\)`, ic),
		// As a last resort for movies that have ( or [ in their title.
		c.re(`^(?<title>.+?)?(?:(?:[-_\W](?<![)\[!]))*(?<year>(1(8|9)|20)\d{2}(?!p|i|\d+|\]|\W\d+)))+(\W+|_|$)(?!\\)`, ic),
	}
	// When year comes first.
	t.folder = c.re(`^(?:(?:[-_\W](?<![)!]))*(?<year>(19|20)\d{2}(?!p|i|\d+|\W\d+)))+(\W+|_|$)(?<title>.+?)?$`, regexp2.None)
	t.rejectHashed = []*regexp2.Regexp{
		// Generic match for md5 and mixed-case hashes.
		c.re(`^[0-9a-zA-Z]{32}`, regexp2.None),
		// Generic match for shorter lower-case hashes.
		c.re(`^[a-z0-9]{24}$`, regexp2.None),
		// Format seen on some NZBGeek releases; strict, they are close to valid 101 numbering.
		c.re(`^[A-Z]{11}\d{3}$`, regexp2.None),
		c.re(`^[a-z]{12}\d{3}$`, regexp2.None),
		// Backup filename (unknown origins)
		c.re(`^Backup_\d{5,}S\d{2}-\d{2}$`, regexp2.None),
		c.re(`^123$`, regexp2.None),
		c.re(`^abc$`, ic),
		c.re(`^abc[-_. ]xyz`, ic),
		c.re(`^b00bs$`, ic),
	}
	// Detects whether the title was reversed.
	t.reversed = c.re(`(?:^|[-._ ])(p027|p0801)[-._ ]`, regexp2.None)
	// Splits movie titles that contain AKA.
	t.alternative = c.re(`[ ]+(?:AKA|\/)[ ]+`, ic)
	// Unbrackets alternative titles.
	t.bracketedAlt = c.re(`(.*) \([ ]*AKA[ ]+(.*)\)`, ic)
	t.normalizeAlt = c.re(`[ ]+(?:A\.K\.A\.)[ ]+`, ic)
	t.imdbID = c.re(`(?<imdbid>tt\d{7,8})`, ic)
	t.tmdbID = c.re(`tmdb(id)?-(?<tmdbid>\d+)`, ic)
	t.simpleTitle = c.re(`(?:(480|540|576|720|1080|2160)[ip]|[xh][\W_]?26[45]|DD\W?5\W1|[<>?*]|848x480|1280x720|1920x1080|3840x2160|4096x2160|(8|10)b(it)?|10-bit)\s*?(?![a-b0-9])`, ic)
	t.simpleRelease = c.re(`\s*(?:[<>?*|])`, ic)
	t.cleanBrackets = c.re(`\[[a-z0-9 ._-]+\]$`, ic)
	t.requestInfo = c.re(`^(?:\[.+?\])+`, regexp2.None)
	t.fileExtension = c.re(`\.[a-z0-9]{2,4}$`, ic)
	t.common = newCommonTable(c)
	if c.err != nil {
		return nil, c.err
	}
	var err error
	if t.lang, err = newLanguageTable(); err != nil {
		return nil, err
	}
	if t.group, err = newReleaseGroupTable(); err != nil {
		return nil, err
	}
	return t, nil
}

// removeFileExtension is FileExtensions.RemoveFileExtension: a trailing dot
// and two to four letters or digits are dropped only when they are a known
// media file extension (or .par2 / .nzb); any other tail is part of the name.
func (t *movieTitleTable) removeFileExtension(b *budget, title string) (string, error) {
	var evalErr error
	return b.replaceFunc(t.fileExtension, title, &evalErr, func(m regexp2.Match) string {
		ext := strings.ToLower(m.String())
		if movieExtensionQuality(ext) != rqUnknown || ext == ".par2" || ext == ".nzb" {
			return ""
		}
		return m.String()
	})
}

// parse is ParseMovieTitle: nil with a nil error is the source's null (the name
// was rejected or no pattern produced a title).
func (t *movieTitleTable) parse(b *budget, title string, isDir bool) (*MovieInfo, error) {
	originalTitle := title
	if !t.validateBeforeParsing(title) {
		return nil, nil
	}
	rejected, err := t.rejectedHashed(b, title)
	if err != nil || rejected {
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
		// The source interpolates the reversed character array into the title,
		// which formats as the array's type name; the untouched tail follows.
		title = "System.Char[]" + utf16Suffix(title, utf16Len(withoutExtension))
	}

	releaseTitle, err := t.removeFileExtension(b, title)
	if err != nil {
		return nil, err
	}
	// Trim dashes from end.
	releaseTitle = strings.Trim(releaseTitle, "-_")
	releaseTitle = strings.ReplaceAll(strings.ReplaceAll(releaseTitle, "【", "["), "】", "]")

	simpleTitle, err := b.replace(t.simpleTitle, releaseTitle, "")
	if err != nil {
		return nil, err
	}
	// Quick fix stripping [url] - prefixes.
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
		if q.Code != t.quality.defs[rqUnknown].code {
			return ""
		}
		return m.String()
	}); err != nil {
		return nil, err
	}

	regexes := t.titles
	if isDir {
		regexes = append(append([]*regexp2.Regexp{}, regexes...), t.folder)
	}
	for _, re := range regexes {
		match, err := b.first(re, simpleTitle)
		if err != nil {
			return nil, err
		}
		if match == nil {
			continue
		}
		result, err := t.parseMatch(b, match)
		if err != nil {
			return nil, err
		}
		if result == nil {
			continue
		}
		return t.complete(b, result, match, originalTitle, title, releaseTitle, simpleTitle)
	}
	return nil, nil
}

// complete runs the part of ParseMovieTitle after a pattern produced a title:
// the simple release title, release group, hardcoded subs, languages, quality,
// edition, hash and IDs. A nil result is the source's exception on an
// out-of-range replacement, which it reports as unable to parse.
func (t *movieTitleTable) complete(b *budget, result *MovieInfo, match *regexp2.Match, originalTitle, title, releaseTitle, simpleTitle string) (*MovieInfo, error) {
	simpleReleaseTitle, err := b.replace(t.simpleRelease, releaseTitle, "")
	if err != nil {
		return nil, err
	}

	replaceString := result.MovieTitles[0]
	if groupOK(match, "title") {
		replaceString = groupText(match, "title")
	}
	if !isBlank(replaceString) {
		movie := "A Movie"
		if strings.Contains(replaceString, ".") {
			movie = "A.Movie"
		}
		if groupOK(match, "title") {
			at, n := match.GroupByName("title").ByteRange()
			var ok bool
			if simpleReleaseTitle, ok = replaceUTF16(simpleReleaseTitle, utf16Len(simpleTitle[:at]), utf16Len(simpleTitle[at:at+n]), movie); !ok {
				return nil, nil
			}
		} else {
			simpleReleaseTitle = strings.ReplaceAll(simpleReleaseTitle, replaceString, movie)
		}
	}

	if result.ReleaseGroup, err = t.group.parse(b, t, simpleReleaseTitle); err != nil {
		return nil, err
	}
	if sub := groupText(match, "subgroup"); !isBlank(sub) {
		result.ReleaseGroup = sub
	}
	if result.HardcodedSubs, err = t.parseHardcodedSubs(b, title); err != nil {
		return nil, err
	}

	languageTitle := simpleReleaseTitle
	if !isBlank(result.ReleaseGroup) {
		languageTitle = strings.ReplaceAll(simpleReleaseTitle, result.ReleaseGroup, "RlsGrp")
	}
	if result.Languages, err = t.lang.parse(b, languageTitle); err != nil {
		return nil, err
	}

	if result.Quality, err = t.quality.parseQuality(b, title); err != nil {
		return nil, err
	}
	if isBlank(result.Edition) {
		if result.Edition, err = t.parseEdition(b, simpleReleaseTitle); err != nil {
			return nil, err
		}
	}

	if result.ReleaseHash = groupText(match, "hash"); result.ReleaseHash != "" {
		result.ReleaseHash = strings.Trim(result.ReleaseHash, "[]")
		if result.ReleaseHash == "1280x720" {
			result.ReleaseHash = ""
		}
	}

	result.OriginalTitle = originalTitle
	result.ReleaseTitle = releaseTitle
	result.SimpleReleaseTitle = simpleReleaseTitle

	if result.ImdbID, err = t.parseImdbID(b, simpleReleaseTitle); err != nil {
		return nil, err
	}
	if result.TmdbID, err = t.parseTmdbID(b, simpleReleaseTitle); err != nil {
		return nil, err
	}
	return result, nil
}

// validateBeforeParsing is the source's ValidateBeforeParsing without the
// hashed-release rejection, which needs the budget (rejectedHashed).
func (t *movieTitleTable) validateBeforeParsing(title string) bool {
	lower := strings.ToLower(title)
	if strings.Contains(lower, "password") && strings.Contains(lower, "yenc") {
		return false
	}
	return hasLetterOrDigit(title)
}

// rejectedHashed is the hashed-release rejection of ValidateBeforeParsing.
func (t *movieTitleTable) rejectedHashed(b *budget, title string) (bool, error) {
	withoutExtension, err := t.removeFileExtension(b, title)
	if err != nil {
		return false, err
	}
	for _, re := range t.rejectHashed {
		hashed, err := b.isMatch(re, withoutExtension)
		if err != nil || hashed {
			return hashed, err
		}
	}
	return false, nil
}

// parseMatch is ParseMovieMatchCollection for the first match of a title
// pattern: nil when the match has no usable title.
func (t *movieTitleTable) parseMatch(b *budget, match *regexp2.Match) (*MovieInfo, error) {
	if !groupOK(match, "title") || groupText(match, "title") == "(" {
		return nil, nil
	}
	movieName := strings.ReplaceAll(groupText(match, "title"), "_", " ")
	movieName, err := b.replace(t.normalizeAlt, movieName, " AKA ")
	if err != nil {
		return nil, err
	}
	if movieName, err = b.replace(t.requestInfo, movieName, ""); err != nil {
		return nil, err
	}
	movieName = strings.Trim(movieName, " ")

	parts := strings.Split(movieName, ".")
	var sb strings.Builder
	previousAcronym := false
	for n, part := range parts {
		nextPart := ""
		if len(parts) >= n+2 {
			nextPart = parts[n+1]
		}
		lower := strings.ToLower(part)
		_, partInt := tryParseInt(part)
		_, nextInt := tryParseInt(nextPart)
		switch {
		case utf16Len(part) == 1 && lower != "a" && !partInt &&
			(previousAcronym || n < len(parts)-1) &&
			(previousAcronym || utf16Len(nextPart) != 1 || !nextInt):
			sb.WriteString(part + ".")
			previousAcronym = true
		case lower == "a" && (previousAcronym || utf16Len(nextPart) == 1):
			sb.WriteString(part + ".")
			previousAcronym = true
		case lower == "dr":
			sb.WriteString(part + ".")
			previousAcronym = true
		default:
			if previousAcronym {
				sb.WriteString(" ")
				previousAcronym = false
			}
			sb.WriteString(part + " ")
		}
	}
	movieName = strings.Trim(sb.String(), " ")

	result := &MovieInfo{}
	result.Year, _ = tryParseInt(groupText(match, "year"))
	if groupOK(match, "edition") {
		result.Edition = strings.ReplaceAll(groupText(match, "edition"), ".", " ")
	}

	result.MovieTitles = []string{movieName}

	// Delete parentheses of the form (aka ...).
	unbracketed, err := b.replace(t.bracketedAlt, movieName, "$1 AKA $2")
	if err != nil {
		return nil, err
	}
	// Split by AKA and filter out empty and duplicate names.
	alternatives, err := b.split(t.alternative, unbracketed)
	if err != nil {
		return nil, err
	}
	for _, alternative := range alternatives {
		if !isBlank(alternative) && alternative != movieName {
			result.MovieTitles = append(result.MovieTitles, alternative)
		}
	}
	return result, nil
}

// parseHardcodedSubs is ParseHardcodeSubs: the last hardcoded-subtitle token of
// the title, or empty.
func (t *movieTitleTable) parseHardcodedSubs(b *budget, title string) (string, error) {
	m, err := b.last(t.hardcodedSubs, title)
	if err != nil || m == nil {
		return "", err
	}
	if groupOK(m, "hcsub") {
		return groupText(m, "hcsub"), nil
	}
	if groupOK(m, "hc") {
		return "Generic Hardcoded Subs", nil
	}
	return "", nil
}

// parseEdition is ParseEdition: the edition named after the first characters of
// the language title, dots read as spaces.
func (t *movieTitleTable) parseEdition(b *budget, languageTitle string) (string, error) {
	m, err := b.first(t.reportEdition, languageTitle)
	if err != nil || m == nil {
		return "", err
	}
	if edition := groupText(m, "edition"); !isBlank(edition) {
		return strings.ReplaceAll(edition, ".", " "), nil
	}
	return "", nil
}

// parseImdbID is ParseImdbId: the first tt-number of nine or ten UTF-16 units.
func (t *movieTitleTable) parseImdbID(b *budget, title string) (string, error) {
	m, err := b.first(t.imdbID, title)
	if err != nil || m == nil {
		return "", err
	}
	id := groupText(m, "imdbid")
	if n := utf16Len(id); n == 9 || n == 10 {
		return id, nil
	}
	return "", nil
}

// parseTmdbID is ParseTmdbId: the first tmdb(id)-number, 0 when it is not an int.
func (t *movieTitleTable) parseTmdbID(b *budget, title string) (int, error) {
	m, err := b.first(t.tmdbID, title)
	if err != nil || m == nil {
		return 0, err
	}
	id, _ := tryParseInt(groupText(m, "tmdbid"))
	return id, nil
}
