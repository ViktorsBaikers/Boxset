// SPDX-License-Identifier: GPL-3.0

package parse

// Quality parsing for kind KindSeries. Control flow, pattern order and
// pattern text are semantic; the fixture table testdata/parser/series.jsonl
// asserts them.
//
// Implementation notes: white-space trimming and the ordinal-ignore-case
// extension table use the equivalent Unicode sets;
// the case-insensitive substring test is applied only to ASCII needles, for
// which ASCII case folding is equivalent; path handling follows Linux
// semantics (only NUL is an invalid path character, only '/'
// separates). regexp2 runs on runes while indexes are counted in UTF-16
// units; the two agree for the Basic Multilingual Plane.

import (
	"github.com/dlclark/regexp2/v2"
)

type seriesTable struct {
	defs          [sqCount]qualityDef
	source        *regexp2.Regexp
	rawHD         *regexp2.Regexp
	mpeg2         *regexp2.Regexp
	proper        *regexp2.Regexp
	repack        *regexp2.Regexp
	version       *regexp2.Regexp
	real          *regexp2.Regexp
	resolution    *regexp2.Regexp
	altResolution *regexp2.Regexp
	codec         *regexp2.Regexp
	otherSource   *regexp2.Regexp
	animeBluray   *regexp2.Regexp
	animeWebDL    *regexp2.Regexp
	highDefPdtv   *regexp2.Regexp
	remux         *regexp2.Regexp
}

const ic = regexp2.IgnoreCase

func newSeriesTable() (*seriesTable, error) {
	c := &compiler{}
	t := &seriesTable{defs: seriesDefs()}
	t.source = c.re(`\b(?:
		(?<bluray>BluRay|Blu-Ray|HD-?DVD|BDMux|BD(?!$))|
		(?<webdl>WEB[-_. ]DL(?:mux)?|WEBDL|AmazonHD|AmazonSD|iTunesHD|MaxdomeHD|NetflixU?HD|WebHD|HBOMaxHD|DisneyHD|[. ]WEB[. ](?:[xh][ .]?26[456]|AVC|HEVC|EAC3|DDP?[ .]?5[. ]1)|[. ](?-i:WEB)$|(?:720|1080|2160)p[-. ]WEB[-. ]|[-. ]WEB[-. ](?:720|1080|2160)p|\b\s\/\sWEB\s\/\s\b|(?:AMZN|ATVP|DP|NF)[. -]WEB[. -](?!Rip))|
		(?<webrip>WebRip|Web-Rip|WEBMux)|
		(?<hdtv>HDTV)|
		(?<bdrip>BDRip|BDLight)|
		(?<brrip>BRRip)|
		(?<dvd>DVD|DVDRip|NTSC|PAL|xvidvd)|
		(?<dsr>WS[-_. ]DSR|DSR)|
		(?<pdtv>PDTV)|
		(?<sdtv>SDTV)|
		(?<tvrip>TVRip)
		)(?:\b|$|[ .])`, ic|regexp2.IgnorePatternWhitespace)
	t.rawHD = c.re(`\b(?<rawhd>RawHD|Raw[-_. ]HD)\b`, ic)
	t.mpeg2 = c.re(`\b(?<mpeg2>MPEG[-_. ]?2)\b`, regexp2.None)
	t.proper = c.re(`\b(?<proper>proper)\b`, ic)
	t.repack = c.re(`\b(?<repack>repack\d?|rerip\d?)\b`, ic)
	t.version = c.re(`\d[-._ ]?v(?<version>\d)[-._ ]|\[v(?<version>\d)\]|repack(?<version>\d)|rerip(?<version>\d)|(?:480|576|720|1080|2160)p[._ ]v(?<version>\d)`, ic)
	t.real = c.re(`\b(?<real>REAL)\b`, regexp2.None)
	t.resolution = c.re(`\b(?:(?<R360p>360p)|(?<R480p>480p|480i|640x480|848x480)|(?<R540p>540p)|(?<R576p>576p)|(?<R720p>720p|1280x720|960p)|(?<R1080p>1080p|1920x1080|1440p|FHD|1080i|4kto1080p)|(?<R2160p>2160p|3840x2160|4k[-_. ](?:UHD|HEVC|BD|H26[56])|(?:UHD|HEVC|BD|H26[56])[-_. ]4k))\b`, ic)
	t.altResolution = c.re(`\b(?<R2160p>UHD)\b|(?<R2160p>\[4K\])`, ic)
	t.codec = c.re(`\b(?:(?<x264>x264)|(?<h264>h264)|(?<xvidhd>XvidHD)|(?<xvid>Xvid)|(?<divx>divx))\b`, ic)
	t.otherSource = c.re(`(?<hdtv>HD[-_. ]TV)|(?<sdtv>SD[-_. ]TV)`, ic)
	t.animeBluray = c.re(`bd(?:720|1080|2160)|(?<=[-_. (\[])bd(?=[-_. )\]])`, ic)
	t.animeWebDL = c.re(`\[WEB\]|[\[\(]WEB[ .]`, ic)
	t.highDefPdtv = c.re(`hr[-_. ]ws`, ic)
	t.remux = c.re(`(?:[_. ]|\d{4}p-|\bHybrid-)(?<remux>(?:(BD|UHD)[-_. ]?)?Remux)\b|(?<remux>(?:(BD|UHD)[-_. ]?)?Remux[_. ]\d{4}p)`, ic)
	if c.err != nil {
		return nil, c.err
	}
	return t, nil
}

func (t *seriesTable) newResult() Quality {
	q := t.defs[sqUnknown].quality()
	q.Revision = Revision{Version: 1}
	q.SourceDetection, q.ResolutionDetection, q.RevisionDetection = detectionUnknown, detectionUnknown, detectionUnknown
	return q
}

func (t *seriesTable) set(q *Quality, idx int) {
	d := t.defs[idx]
	q.Code, q.Source, q.Resolution, q.Modifier = d.code, d.source, d.resolution, d.modifier
}

// parseQuality parses the quality of one series release name.
func (t *seriesTable) parseQuality(b *budget, name string) (Quality, error) {
	if isBlank(name) {
		return t.newResult(), nil
	}
	name = trimWhite(name)
	result, err := t.parseQualityName(b, name)
	if err != nil {
		return Quality{}, err
	}
	// Based on extension
	if result.Code == t.defs[sqUnknown].code && !containsInvalidPathChar(name) {
		t.set(&result, seriesExtensionQuality(pathExtension(name)))
		result.SourceDetection = detectionExtension
		result.ResolutionDetection = detectionExtension
	}
	return result, nil
}

func (t *seriesTable) parseQualityName(b *budget, name string) (Quality, error) {
	normalized := trimWhite(replaceUnderscores(name))
	result, err := t.modifiers(b, name, normalized)
	if err != nil {
		return Quality{}, err
	}
	finish := func(idx int) (Quality, error) {
		t.set(&result, idx)
		return result, nil
	}

	rawHD, err := b.isMatch(t.rawHD, normalized)
	if err != nil {
		return Quality{}, err
	}
	if rawHD {
		result.SourceDetection = detectionName
		result.ResolutionDetection = detectionName
		return finish(sqRAWHD)
	}

	sourceMatch, err := b.last(t.source, normalized)
	if err != nil {
		return Quality{}, err
	}
	resolution, err := t.parseResolution(b, normalized)
	if err != nil {
		return Quality{}, err
	}
	codec, err := b.first(t.codec, normalized)
	if err != nil {
		return Quality{}, err
	}
	remux, err := b.isMatch(t.remux, normalized)
	if err != nil {
		return Quality{}, err
	}

	if resolution != resUnknown {
		result.ResolutionDetection = detectionName
	}

	if sourceMatch != nil {
		result.SourceDetection = detectionName

		if groupOK(sourceMatch, "bluray") {
			switch {
			case groupOK(codec, "xvid") || groupOK(codec, "divx"):
				return finish(sqBluray480p)
			case resolution == res2160p:
				if remux {
					return finish(sqBluray2160pRemux)
				}
				return finish(sqBluray2160p)
			case resolution == res1080p:
				if remux {
					return finish(sqBluray1080pRemux)
				}
				return finish(sqBluray1080p)
			case resolution == res576p:
				return finish(sqBluray576p)
			case resolution == res360p || resolution == res480p || resolution == res540p:
				return finish(sqBluray480p)
			// Treat a remux without a source as 1080p, not 720p.
			// 720p remux should fallback as 720p BluRay
			case remux && resolution != res720p:
				return finish(sqBluray1080pRemux)
			}
			return finish(sqBluray720p)
		}

		if groupOK(sourceMatch, "webdl") {
			switch {
			case resolution == res2160p:
				return finish(sqWEBDL2160p)
			case resolution == res1080p:
				return finish(sqWEBDL1080p)
			case resolution == res720p:
				return finish(sqWEBDL720p)
			case containsOrdinal(name, "[WEBDL]"):
				return finish(sqWEBDL720p)
			}
			return finish(sqWEBDL480p)
		}

		if groupOK(sourceMatch, "webrip") {
			switch resolution {
			case res2160p:
				return finish(sqWEBRip2160p)
			case res1080p:
				return finish(sqWEBRip1080p)
			case res720p:
				return finish(sqWEBRip720p)
			}
			return finish(sqWEBRip480p)
		}

		if groupOK(sourceMatch, "hdtv") {
			mpeg2, err := b.isMatch(t.mpeg2, normalized)
			if err != nil {
				return Quality{}, err
			}
			switch {
			case mpeg2:
				return finish(sqRAWHD)
			case resolution == res2160p:
				return finish(sqHDTV2160p)
			case resolution == res1080p:
				return finish(sqHDTV1080p)
			case resolution == res720p:
				return finish(sqHDTV720p)
			case containsOrdinal(name, "[HDTV]"):
				return finish(sqHDTV720p)
			}
			return finish(sqSDTV)
		}

		if groupOK(sourceMatch, "bdrip") || groupOK(sourceMatch, "brrip") {
			switch resolution {
			case res720p:
				return finish(sqBluray720p)
			case res1080p:
				return finish(sqBluray1080p)
			case res2160p:
				return finish(sqBluray2160p)
			}
			return finish(sqBluray480p)
		}

		if groupOK(sourceMatch, "dvd") {
			return finish(sqDVD)
		}

		if groupOK(sourceMatch, "pdtv") || groupOK(sourceMatch, "sdtv") || groupOK(sourceMatch, "dsr") || groupOK(sourceMatch, "tvrip") {
			switch {
			case resolution == res1080p || containsFold(normalized, "1080p"):
				return finish(sqHDTV1080p)
			case resolution == res720p || containsFold(normalized, "720p"):
				return finish(sqHDTV720p)
			}
			hr, err := b.isMatch(t.highDefPdtv, normalized)
			if err != nil {
				return Quality{}, err
			}
			if hr {
				result.ResolutionDetection = detectionName
				return finish(sqHDTV720p)
			}
			return finish(sqSDTV)
		}
	}

	if sourceMatch == nil && remux && resolution != resUnknown {
		result.SourceDetection = detectionUnknown
		switch resolution {
		case res480p:
			return finish(sqBluray480p)
		case res720p:
			return finish(sqBluray720p)
		case res2160p:
			return finish(sqBluray2160pRemux)
		case res1080p:
			return finish(sqBluray1080pRemux)
		}
	}

	// Anime Bluray matching
	animeBD, err := b.isMatch(t.animeBluray, normalized)
	if err != nil {
		return Quality{}, err
	}
	if animeBD {
		result.SourceDetection = detectionName
		switch {
		case resolution == res360p || resolution == res480p || resolution == res540p || resolution == res576p || containsFold(normalized, "480p"):
			result.ResolutionDetection = detectionName
			return finish(sqDVD)
		case resolution == res1080p || containsFold(normalized, "1080p"):
			result.ResolutionDetection = detectionName
			if remux {
				return finish(sqBluray1080pRemux)
			}
			return finish(sqBluray1080p)
		case resolution == res2160p || containsFold(normalized, "2160p"):
			result.ResolutionDetection = detectionName
			if remux {
				return finish(sqBluray2160pRemux)
			}
			return finish(sqBluray2160p)
		// Treat a remux without a source as 1080p, not 720p.
		// 720p remux should fallback as 720p BluRay
		case remux && resolution != res720p:
			return finish(sqBluray1080pRemux)
		}
		return finish(sqBluray720p)
	}

	animeWeb, err := b.isMatch(t.animeWebDL, normalized)
	if err != nil {
		return Quality{}, err
	}
	if animeWeb {
		result.SourceDetection = detectionName
		switch {
		case resolution == res360p || resolution == res480p || resolution == res540p || resolution == res576p || containsFold(normalized, "480p"):
			result.ResolutionDetection = detectionName
			return finish(sqWEBDL480p)
		case resolution == res1080p || containsFold(normalized, "1080p"):
			result.ResolutionDetection = detectionName
			return finish(sqWEBDL1080p)
		case resolution == res2160p || containsFold(normalized, "2160p"):
			result.ResolutionDetection = detectionName
			return finish(sqWEBDL2160p)
		}
		return finish(sqWEBDL720p)
	}

	if resolution != resUnknown {
		source := ssUnknown
		if remux {
			result.SourceDetection = detectionName
			source = ssBlurayRaw
		} else if ext := t.defs[seriesExtensionQuality(pathExtensionAnywhere(name))]; ext.member != "Unknown" {
			result.SourceDetection = detectionExtension
			source = ext.sourceRank
		}

		switch resolution {
		case res2160p:
			result.ResolutionDetection = detectionName
			if source == ssUnknown {
				return finish(sqHDTV2160p)
			}
			return finish(t.find(source, 2160))
		case res1080p:
			result.ResolutionDetection = detectionName
			if source == ssUnknown {
				return finish(sqHDTV1080p)
			}
			return finish(t.find(source, 1080))
		case res720p:
			result.ResolutionDetection = detectionName
			if source == ssUnknown {
				return finish(sqHDTV720p)
			}
			return finish(t.find(source, 720))
		case res360p, res480p, res540p, res576p:
			result.ResolutionDetection = detectionName
			if source == ssUnknown {
				return finish(sqSDTV)
			}
			return finish(t.find(source, 480))
		}
	}

	if groupOK(codec, "x264") {
		return finish(sqSDTV)
	}

	if containsOrdinal(normalized, "848x480") {
		result.ResolutionDetection = detectionName
		switch {
		case containsOrdinal(normalized, "dvd"):
			result.SourceDetection = detectionName
			return finish(sqDVD)
		case containsFold(normalized, "bluray"):
			result.SourceDetection = detectionName
			return finish(sqBluray480p)
		}
		return finish(sqSDTV)
	}

	if containsFold(normalized, "1280x720") {
		result.ResolutionDetection = detectionName
		if containsFold(normalized, "bluray") {
			result.SourceDetection = detectionName
			return finish(sqBluray720p)
		}
		return finish(sqHDTV720p)
	}

	if containsFold(normalized, "1920x1080") {
		result.ResolutionDetection = detectionName
		if containsFold(normalized, "bluray") {
			result.SourceDetection = detectionName
			return finish(sqBluray1080p)
		}
		return finish(sqHDTV1080p)
	}

	for _, s := range [...]struct {
		needle string
		idx    int
	}{{"bluray720p", sqBluray720p}, {"bluray1080p", sqBluray1080p}, {"bluray2160p", sqBluray2160p}} {
		if containsFold(normalized, s.needle) {
			result.SourceDetection = detectionName
			result.ResolutionDetection = detectionName
			return finish(s.idx)
		}
	}

	other, err := b.first(t.otherSource, normalized)
	if err != nil {
		return Quality{}, err
	}
	switch {
	case groupOK(other, "sdtv"):
		result.SourceDetection = detectionName
		return finish(sqSDTV)
	case groupOK(other, "hdtv"):
		result.SourceDetection = detectionName
		return finish(sqHDTV720p)
	}
	return result, nil
}

// parseResolution parses the resolution of a name.
func (t *seriesTable) parseResolution(b *budget, name string) (resolution, error) {
	m, err := b.first(t.resolution, name)
	if err != nil {
		return resUnknown, err
	}
	alt, err := b.first(t.altResolution, name)
	if err != nil {
		return resUnknown, err
	}
	switch {
	case groupOK(m, "R360p"):
		return res360p, nil
	case groupOK(m, "R480p"):
		return res480p, nil
	case groupOK(m, "R540p"):
		return res540p, nil
	case groupOK(m, "R576p"):
		return res576p, nil
	case groupOK(m, "R720p"):
		return res720p, nil
	case groupOK(m, "R1080p"):
		return res1080p, nil
	case groupOK(m, "R2160p"), groupOK(alt, "R2160p"):
		return res2160p, nil
	}
	return resUnknown, nil
}

// modifiers parses the revision (version, proper, repack, real) of a name.
func (t *seriesTable) modifiers(b *budget, name, normalized string) (Quality, error) {
	result := t.newResult()
	return applyRevision(b, &result, name, normalized, t.version, t.proper, t.repack, t.real)
}

// find is the lookup by source and resolution: the exact source and
// resolution match; Television/Web/WebRip below 720p roll to their 480p
// quality; otherwise the lowest source at or above the requested one among the
// qualities of that resolution (first in definition order on ties); otherwise
// Unknown.
func (t *seriesTable) find(source, res int) int {
	for i, d := range t.defs {
		if d.sourceRank == source && d.resolution == res {
			return i
		}
	}
	if res < 720 {
		switch source {
		case ssTelevision:
			return sqSDTV
		case ssWeb:
			return sqWEBDL480p
		case ssWebRip:
			return sqWEBRip480p
		}
	}
	nearest := sqUnknown
	best := -1
	for i, d := range t.defs {
		if d.resolution == res && d.sourceRank >= source && (best < 0 || d.sourceRank < best) {
			nearest, best = i, d.sourceRank
		}
	}
	return nearest
}
