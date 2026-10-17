// SPDX-License-Identifier: GPL-3.0

package parse

// Quality parsing for kind KindMovie, ported from the movie reference parser
// (parser/QualityParser.cs and the qualities/ and
// MediaFiles/MediaFileExtensions.cs members it uses, located by
// BOXSET_REF_MOVIE) at commit df2faa64c75e6081556d11232dc46523e7264ea1.
// Control flow, pattern order and pattern text follow the source; the parity
// corpus testdata/parser/movie.jsonl is extracted from its tests.
// The faithful-adapter notes of quality_series.go apply here too.
//
// Source quirk kept observable: for a TELESYNC source the quality's
// Resolution is the parsed resolution of that release (the source assigns it
// on the quality object of that result).

import (
	"github.com/dlclark/regexp2/v2"
)

type movieTable struct {
	defs          [rqCount]qualityDef
	source        *regexp2.Regexp
	rawHD         *regexp2.Regexp
	mpeg2         *regexp2.Regexp
	brDisk        *regexp2.Regexp
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
	germanRemux   *regexp2.Regexp
}

func newMovieTable() (*movieTable, error) {
	c := &compiler{}
	t := &movieTable{defs: movieDefs()}
	t.source = c.re(`\b(?:
		(?<bluray>M?Blu[-_. ]?Ray|HD[-_. ]?DVD|BD(?!$)|UHD2?BD|BDISO|BDMux|BD25|BD50|BR[-_. ]?DISK)|
		(?<webdl>WEB[-_. ]?DL(?:mux)?|AmazonHD|AmazonSD|iTunesHD|MaxdomeHD|NetflixU?HD|WebHD|HBOMaxHD|DisneyHD|[. ]WEB[. ](?:[xh][ .]?26[45]|AVC|HEVC|DDP?5[. ]1)|[. ](?-i:WEB)$|(?:\d{3,4}0p)[-. ](?:Hybrid[-_. ]?)?WEB[-. ]|[-. ]WEB[-. ]\d{3,4}0p|\b\s\/\sWEB\s\/\s\b|(?:AMZN|NF|DP)[. -]WEB[. -](?!Rip))|
		(?<webrip>WebRip|Web-Rip|WEBMux)|
		(?<hdtv>HDTV)|
		(?<bdrip>BDRip|BDLight|HD[-_. ]?DVDRip|UHDBDRip)|
		(?<brrip>BRRip)|
		(?<dvdr>\d?x?M?DVD-?[R59])|
		(?<dvd>DVD(?!-R)|DVDRip|xvidvd)|
		(?<dsr>WS[-_. ]DSR|DSR)|
		(?<regional>R[0-9]{1}|REGIONAL)|
		(?<scr>SCR|SCREENER|DVDSCR|DVDSCREENER)|
		(?<ts>TS[-_. ]|TELESYNCH?|HD-TS|HDTS|PDVD|TSRip|HDTSRip)|
		(?<tc>TC|TELECINE|HD-TC|HDTC)|
		(?<cam>CAMRIP|(?:NEW)?CAM|HD-?CAM(?:Rip)?|HQCAM)|
		(?<wp>WORKPRINT|WP)|
		(?<pdtv>PDTV)|
		(?<sdtv>SDTV)|
		(?<tvrip>TVRip)
		)(?:\b|$|[ .])`, ic|regexp2.IgnorePatternWhitespace)
	t.rawHD = c.re(`\b(?<rawhd>RawHD|Raw[-_. ]HD)\b`, ic)
	t.mpeg2 = c.re(`\b(?<mpeg2>MPEG[-_. ]?2)\b`, regexp2.None)
	t.brDisk = c.re(`^(?!.*\b((?<!HD[._ -]|HD)DVD|BDRip|720p|MKV|XviD|WMV|d3g|(BD)?REMUX|^(?=.*1080p)(?=.*HEVC)|[xh][-_. ]?26[45]|German.*[DM]L|((?<=\d{4}).*German.*([DM]L)?)(?=.*\b(AVC|HEVC|VC[-_. ]?1|MVC|MPEG[-_. ]?2)\b))\b)(((?=.*\b(Blu[-_. ]?ray|BD|HD[-_. ]?DVD)\b)(?=.*\b(AVC|HEVC|VC[-_. ]?1|MVC|MPEG[-_. ]?2|BDMV|ISO)\b))|^((?=.*\b(((?=.*\b((.*_)?COMPLETE.*|Dis[ck])\b)(?=.*(Blu[-_. ]?ray|HD[-_. ]?DVD)))|3D[-_. ]?BD|BR[-_. ]?DISK|Full[-_. ]?Blu[-_. ]?ray|^((?=.*((BD|UHD)[-_. ]?(25|50|66|100|ISO)))))))).*`, ic)
	t.proper = c.re(`\b(?<proper>proper)\b`, ic)
	t.repack = c.re(`\b(?<repack>repack\d?|rerip\d?)\b`, ic)
	t.version = c.re(`\d[-._ ]?v(?<version>\d)[-._ ]|\[v(?<version>\d)\]|repack(?<version>\d)|rerip(?<version>\d)`, ic)
	t.real = c.re(`\b(?<real>REAL)\b`, regexp2.None)
	t.resolution = c.re(`\b(?:(?<R360p>360p)|(?<R480p>480p|480i|640x480|848x480)|(?<R540p>540p)|(?<R576p>576p)|(?<R720p>720p|1280x720|960p)|(?<R1080p>1080p|1920x1080|1440p|FHD|1080i|4kto1080p)|(?<R2160p>2160p|3840x2160|4k[-_. ](?:UHD|HEVC|BD|H\.?265)|(?:UHD|HEVC|BD|H\.?265)[-_. ]4k))\b`, ic)
	t.altResolution = c.re(`\b(?<R2160p>UHD)\b|(?<R2160p>\[4K\])`, ic)
	t.codec = c.re(`\b(?:(?<x264>x264)|(?<h264>h264)|(?<xvidhd>XvidHD)|(?<xvid>X-?vid)|(?<divx>divx))\b`, ic)
	t.otherSource = c.re(`(?<hdtv>HD[-_. ]TV)|(?<sdtv>SD[-_. ]TV)`, ic)
	t.animeBluray = c.re(`bd(?:720|1080|2160)|(?<=[-_. (\[])bd(?=[-_. )\]])`, ic)
	t.animeWebDL = c.re(`\[WEB\]|[\[\(]WEB[ .]`, ic)
	t.highDefPdtv = c.re(`hr[-_. ]ws`, ic)
	t.remux = c.re(`(?:[_. \[]|\d{4}p-|\bHybrid-)(?<remux>(?:(BD|UHD)[-_. ]?)?Remux)\b|(?<remux>(?:(BD|UHD)[-_. ]?)?Remux[_. ]\d{4}p)`, ic)
	t.germanRemux = c.re(`((?<=\d{4}).*German.*([DM]L)?)(?=.*\b(AVC|HEVC|VC[_. -]?1|MVC|MPEG[_. -]?2))(?=.*Blu-?ray)`, ic)
	if c.err != nil {
		return nil, c.err
	}
	return t, nil
}

func (t *movieTable) newResult() Quality {
	q := t.defs[rqUnknown].quality()
	q.Revision = Revision{Version: 1}
	q.SourceDetection, q.ResolutionDetection, q.ModifierDetection, q.RevisionDetection =
		detectionUnknown, detectionUnknown, detectionUnknown, detectionUnknown
	return q
}

func (t *movieTable) set(q *Quality, idx int) {
	d := t.defs[idx]
	q.Code, q.Source, q.Resolution, q.Modifier = d.code, d.source, d.resolution, d.modifier
}

// parseQuality is QualityParser.ParseQuality.
func (t *movieTable) parseQuality(b *budget, name string) (Quality, error) {
	if isBlank(name) {
		return t.newResult(), nil
	}
	name = trimWhite(name)
	result, err := t.parseQualityName(b, name)
	if err != nil {
		return Quality{}, err
	}
	// Based on extension
	if result.Code == t.defs[rqUnknown].code && !containsInvalidPathChar(name) {
		t.set(&result, movieExtensionQuality(pathExtension(name)))
		result.SourceDetection = detectionExtension
		result.ResolutionDetection = detectionExtension
		result.ModifierDetection = detectionExtension
	}
	return result, nil
}

func (t *movieTable) parseQualityName(b *budget, name string) (Quality, error) {
	normalized := trimWhite(replaceUnderscores(name))
	result := t.newResult()
	if _, err := applyRevision(b, &result, name, normalized, t.version, t.proper, t.repack, t.real); err != nil {
		return Quality{}, err
	}
	finish := func(idx int) (Quality, error) {
		t.set(&result, idx)
		return result, nil
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
	if !remux {
		if remux, err = b.isMatch(t.germanRemux, normalized); err != nil {
			return Quality{}, err
		}
	}
	brDisk, err := b.isMatch(t.brDisk, normalized)
	if err != nil {
		return Quality{}, err
	}

	rawHD, err := b.isMatch(t.rawHD, normalized)
	if err != nil {
		return Quality{}, err
	}
	if rawHD && !brDisk {
		result.SourceDetection = detectionName
		result.ResolutionDetection = detectionName
		return finish(rqRAWHD)
	}

	if resolution != resUnknown {
		result.ResolutionDetection = detectionName
	}

	if sourceMatch != nil {
		result.SourceDetection = detectionName

		if groupOK(sourceMatch, "bluray") {
			switch {
			case brDisk:
				return finish(rqBRDISK)
			case groupOK(codec, "xvid") || groupOK(codec, "divx"):
				return finish(rqBluray480p)
			case resolution == res2160p:
				if remux {
					return finish(rqRemux2160p)
				}
				return finish(rqBluray2160p)
			case resolution == res1080p:
				if remux {
					return finish(rqRemux1080p)
				}
				return finish(rqBluray1080p)
			case resolution == res720p:
				return finish(rqBluray720p)
			case resolution == res576p:
				return finish(rqBluray576p)
			case resolution == res360p || resolution == res480p || resolution == res540p:
				return finish(rqBluray480p)
			// Treat a remux without a source as 1080p, not 720p.
			// 720p remux should fallback as 720p BluRay
			case remux && resolution != res720p:
				return finish(rqRemux1080p)
			}
			return finish(rqBluray720p)
		}

		if groupOK(sourceMatch, "webdl") {
			switch {
			case resolution == res2160p:
				return finish(rqWEBDL2160p)
			case resolution == res1080p:
				return finish(rqWEBDL1080p)
			case resolution == res720p:
				return finish(rqWEBDL720p)
			case containsOrdinal(name, "[WEBDL]"):
				return finish(rqWEBDL720p)
			}
			return finish(rqWEBDL480p)
		}

		if groupOK(sourceMatch, "webrip") {
			switch resolution {
			case res2160p:
				return finish(rqWEBRip2160p)
			case res1080p:
				return finish(rqWEBRip1080p)
			case res720p:
				return finish(rqWEBRip720p)
			}
			return finish(rqWEBRip480p)
		}

		switch {
		case groupOK(sourceMatch, "scr"):
			return finish(rqDVDSCR)
		case groupOK(sourceMatch, "cam"):
			return finish(rqCAM)
		case groupOK(sourceMatch, "ts"):
			t.set(&result, rqTELESYNC)
			result.Resolution = int(resolution)
			return result, nil
		case groupOK(sourceMatch, "tc"):
			return finish(rqTELECINE)
		case groupOK(sourceMatch, "wp"):
			return finish(rqWORKPRINT)
		case groupOK(sourceMatch, "regional"):
			return finish(rqREGIONAL)
		}

		if groupOK(sourceMatch, "hdtv") {
			mpeg2, err := b.isMatch(t.mpeg2, normalized)
			if err != nil {
				return Quality{}, err
			}
			switch {
			case mpeg2:
				return finish(rqRAWHD)
			case resolution == res2160p:
				return finish(rqHDTV2160p)
			case resolution == res1080p:
				return finish(rqHDTV1080p)
			case resolution == res720p:
				return finish(rqHDTV720p)
			case containsOrdinal(name, "[HDTV]"):
				return finish(rqHDTV720p)
			}
			return finish(rqSDTV)
		}

		if groupOK(sourceMatch, "bdrip") || groupOK(sourceMatch, "brrip") {
			switch resolution {
			case res720p:
				return finish(rqBluray720p)
			case res1080p:
				return finish(rqBluray1080p)
			case res2160p:
				return finish(rqBluray2160p)
			case res576p:
				return finish(rqBluray576p)
			}
			return finish(rqBluray480p)
		}

		if groupOK(sourceMatch, "dvdr") {
			return finish(rqDVDR)
		}

		if groupOK(sourceMatch, "dvd") {
			return finish(rqDVD)
		}

		if groupOK(sourceMatch, "pdtv") || groupOK(sourceMatch, "sdtv") || groupOK(sourceMatch, "dsr") || groupOK(sourceMatch, "tvrip") {
			switch {
			case resolution == res1080p || containsFold(normalized, "1080p"):
				return finish(rqHDTV1080p)
			case resolution == res720p || containsFold(normalized, "720p"):
				return finish(rqHDTV720p)
			}
			hr, err := b.isMatch(t.highDefPdtv, normalized)
			if err != nil {
				return Quality{}, err
			}
			if hr {
				result.ResolutionDetection = detectionName
				return finish(rqHDTV720p)
			}
			return finish(rqSDTV)
		}
	}

	if sourceMatch == nil && remux && resolution != resUnknown {
		result.SourceDetection = detectionUnknown
		switch resolution {
		case res480p:
			return finish(rqBluray480p)
		case res720p:
			return finish(rqBluray720p)
		case res2160p:
			return finish(rqRemux2160p)
		case res1080p:
			return finish(rqRemux1080p)
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
			return finish(rqDVD)
		case resolution == res1080p || containsFold(normalized, "1080p"):
			result.ResolutionDetection = detectionName
			if remux {
				return finish(rqRemux1080p)
			}
			return finish(rqBluray1080p)
		case resolution == res2160p || containsFold(normalized, "2160p"):
			result.ResolutionDetection = detectionName
			if remux {
				return finish(rqRemux2160p)
			}
			return finish(rqBluray2160p)
		// Treat a remux without a source as 1080p, not 720p.
		// 720p remux should fallback as 720p BluRay
		case remux && resolution != res720p:
			return finish(rqRemux1080p)
		}
		return finish(rqBluray720p)
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
			return finish(rqWEBDL480p)
		case resolution == res1080p || containsFold(normalized, "1080p"):
			result.ResolutionDetection = detectionName
			return finish(rqWEBDL1080p)
		case resolution == res2160p || containsFold(normalized, "2160p"):
			result.ResolutionDetection = detectionName
			return finish(rqWEBDL2160p)
		}
		return finish(rqWEBDL720p)
	}

	if resolution != resUnknown {
		source := rsUnknown
		modifier := ""
		if remux {
			result.SourceDetection = detectionName
			source = rsBluray
			modifier = t.defs[rqRemux1080p].modifier
		} else if ext := t.defs[movieExtensionQuality(pathExtensionAnywhere(name))]; ext.member != "Unknown" {
			result.SourceDetection = detectionExtension
			source = ext.sourceRank
		}

		switch resolution {
		case res2160p:
			result.ResolutionDetection = detectionName
			if source == rsUnknown {
				return finish(rqHDTV2160p)
			}
			return finish(t.find(source, 2160, modifier))
		case res1080p:
			result.ResolutionDetection = detectionName
			if source == rsUnknown {
				return finish(rqHDTV1080p)
			}
			return finish(t.find(source, 1080, modifier))
		case res720p:
			result.ResolutionDetection = detectionName
			if source == rsUnknown {
				return finish(rqHDTV720p)
			}
			return finish(t.find(source, 720, modifier))
		case res360p, res480p, res540p, res576p:
			result.ResolutionDetection = detectionName
			if source == rsUnknown {
				return finish(rqSDTV)
			}
			return finish(t.find(source, 480, modifier))
		}
	}

	if groupOK(codec, "x264") {
		return finish(rqSDTV)
	}

	if containsFold(normalized, "848x480") {
		result.ResolutionDetection = detectionName
		switch {
		case containsOrdinal(normalized, "dvd"):
			result.SourceDetection = detectionName
			return finish(rqDVD)
		case containsFold(normalized, "bluray"):
			result.SourceDetection = detectionName
			return finish(rqBluray480p)
		}
		return finish(rqSDTV)
	}

	if containsFold(normalized, "1280x720") {
		result.ResolutionDetection = detectionName
		if containsFold(normalized, "bluray") {
			result.SourceDetection = detectionName
			return finish(rqBluray720p)
		}
		return finish(rqHDTV720p)
	}

	if containsFold(normalized, "1920x1080") {
		result.ResolutionDetection = detectionName
		if containsFold(normalized, "bluray") {
			result.SourceDetection = detectionName
			return finish(rqBluray1080p)
		}
		return finish(rqHDTV1080p)
	}

	for _, s := range [...]struct {
		needle string
		idx    int
	}{{"bluray720p", rqBluray720p}, {"bluray1080p", rqBluray1080p}, {"bluray2160p", rqBluray2160p}} {
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
		return finish(rqSDTV)
	case groupOK(other, "hdtv"):
		result.SourceDetection = detectionName
		return finish(rqHDTV720p)
	}
	return result, nil
}

// parseResolution is QualityParser.ParseResolution.
func (t *movieTable) parseResolution(b *budget, name string) (resolution, error) {
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

// find is QualityFinder.FindBySourceAndResolution: an exact source,
// resolution and modifier match; else the unknown-resolution quality of that
// source and modifier; else the lowest source at or above the requested one
// among the qualities with that modifier and resolution (first in Quality.All
// order on ties); else Unknown.
func (t *movieTable) find(source, res int, modifier string) int {
	for i, d := range t.defs {
		if d.sourceRank == source && d.resolution == res && d.modifier == modifier {
			return i
		}
	}
	for i, d := range t.defs {
		if i != rqUnknown && d.sourceRank == source && d.resolution == 0 && d.modifier == modifier {
			return i
		}
	}
	nearest := rqUnknown
	best := -1
	for i, d := range t.defs {
		if d.modifier == modifier && d.resolution == res && d.sourceRank >= source && (best < 0 || d.sourceRank < best) {
			nearest, best = i, d.sourceRank
		}
	}
	return nearest
}
