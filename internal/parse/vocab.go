// SPDX-License-Identifier: GPL-3.0

package parse

import "unicode"

// resolution is the source Resolution enum of QualityParser.cs: the pixel
// height, 0 for Unknown.
type resolution int

const (
	resUnknown resolution = 0
	res360p    resolution = 360
	res480p    resolution = 480
	res540p    resolution = 540
	res576p    resolution = 576
	res720p    resolution = 720
	res1080p   resolution = 1080
	res2160p   resolution = 2160
)

// Explicit per-application code maps. Every source-native quality, source,
// modifier and detection member maps to a stable Boxset code here; nothing is
// derived from enum ordinals. The tables are values returned by functions so
// no package state is shared or mutable.

// Detection codes.
const (
	detectionUnknown   = "unknown"
	detectionName      = "name"
	detectionExtension = "extension"
)

// detectionCode maps a source QualityDetectionSource member.
func detectionCode(member string) (string, bool) {
	switch member {
	case "Unknown":
		return detectionUnknown, true
	case "Name":
		return detectionName, true
	case "Extension":
		return detectionExtension, true
	case "MediaInfo":
		return "mediainfo", true
	}
	return "", false
}

// qualityDef is one source Quality member: its Boxset code and the facts the
// source Quality object carries. sourceRank is the source member's ordinal in
// the application's own QualitySource enum, used only where the source
// compares sources (QualityFinder), never to derive a code.
type qualityDef struct {
	member     string
	code       string
	source     string
	sourceRank int
	modifier   string
	resolution int
}

func (d qualityDef) quality() Quality {
	return Quality{Code: d.code, Source: d.source, Resolution: d.resolution, Modifier: d.modifier}
}

// Series QualitySource ordinals.
const (
	ssUnknown = iota
	ssTelevision
	ssTelevisionRaw
	ssWeb
	ssWebRip
	ssDVD
	ssBluray
	ssBlurayRaw
)

// seriesSourceCode maps a series QualitySource member.
func seriesSourceCode(member string) (string, bool) {
	switch member {
	case "Unknown":
		return "unknown", true
	case "Television":
		return "tv", true
	case "TelevisionRaw":
		return "tv-raw", true
	case "Web":
		return "web-dl", true
	case "WebRip":
		return "web-rip", true
	case "DVD":
		return "dvd", true
	case "Bluray":
		return "bluray", true
	case "BlurayRaw":
		return "bluray-raw", true
	}
	return "", false
}

// Series Quality members in Quality.All order.
const (
	sqUnknown = iota
	sqSDTV
	sqDVD
	sqWEBRip480p
	sqWEBDL480p
	sqBluray480p
	sqBluray576p
	sqHDTV720p
	sqWEBRip720p
	sqWEBDL720p
	sqBluray720p
	sqBluray1080p
	sqHDTV1080p
	sqWEBRip1080p
	sqWEBDL1080p
	sqRAWHD
	sqHDTV2160p
	sqWEBRip2160p
	sqWEBDL2160p
	sqBluray2160p
	sqBluray1080pRemux
	sqBluray2160pRemux
	sqCount
)

// seriesDefs is the series Quality table (Quality.cs).
func seriesDefs() [sqCount]qualityDef {
	return [sqCount]qualityDef{
		sqUnknown:          {"Unknown", "unknown", "unknown", ssUnknown, "", 0},
		sqSDTV:             {"SDTV", "sdtv", "tv", ssTelevision, "", 480},
		sqDVD:              {"DVD", "dvd", "dvd", ssDVD, "", 480},
		sqWEBRip480p:       {"WEBRip480p", "webrip-480p", "web-rip", ssWebRip, "", 480},
		sqWEBDL480p:        {"WEBDL480p", "webdl-480p", "web-dl", ssWeb, "", 480},
		sqBluray480p:       {"Bluray480p", "bluray-480p", "bluray", ssBluray, "", 480},
		sqBluray576p:       {"Bluray576p", "bluray-576p", "bluray", ssBluray, "", 576},
		sqHDTV720p:         {"HDTV720p", "hdtv-720p", "tv", ssTelevision, "", 720},
		sqWEBRip720p:       {"WEBRip720p", "webrip-720p", "web-rip", ssWebRip, "", 720},
		sqWEBDL720p:        {"WEBDL720p", "webdl-720p", "web-dl", ssWeb, "", 720},
		sqBluray720p:       {"Bluray720p", "bluray-720p", "bluray", ssBluray, "", 720},
		sqBluray1080p:      {"Bluray1080p", "bluray-1080p", "bluray", ssBluray, "", 1080},
		sqHDTV1080p:        {"HDTV1080p", "hdtv-1080p", "tv", ssTelevision, "", 1080},
		sqWEBRip1080p:      {"WEBRip1080p", "webrip-1080p", "web-rip", ssWebRip, "", 1080},
		sqWEBDL1080p:       {"WEBDL1080p", "webdl-1080p", "web-dl", ssWeb, "", 1080},
		sqRAWHD:            {"RAWHD", "raw-hd", "tv-raw", ssTelevisionRaw, "", 1080},
		sqHDTV2160p:        {"HDTV2160p", "hdtv-2160p", "tv", ssTelevision, "", 2160},
		sqWEBRip2160p:      {"WEBRip2160p", "webrip-2160p", "web-rip", ssWebRip, "", 2160},
		sqWEBDL2160p:       {"WEBDL2160p", "webdl-2160p", "web-dl", ssWeb, "", 2160},
		sqBluray2160p:      {"Bluray2160p", "bluray-2160p", "bluray", ssBluray, "", 2160},
		sqBluray1080pRemux: {"Bluray1080pRemux", "remux-1080p", "bluray-raw", ssBlurayRaw, "", 1080},
		sqBluray2160pRemux: {"Bluray2160pRemux", "remux-2160p", "bluray-raw", ssBlurayRaw, "", 2160},
	}
}

// Movie QualitySource ordinals.
const (
	rsUnknown = iota
	rsCAM
	rsTelesync
	rsTelecine
	rsWorkprint
	rsDVD
	rsTV
	rsWebDL
	rsWebRip
	rsBluray
)

// movieSourceCode maps a movie QualitySource member.
func movieSourceCode(member string) (string, bool) {
	switch member {
	case "UNKNOWN":
		return "unknown", true
	case "CAM":
		return "cam", true
	case "TELESYNC":
		return "telesync", true
	case "TELECINE":
		return "telecine", true
	case "WORKPRINT":
		return "workprint", true
	case "DVD":
		return "dvd", true
	case "TV":
		return "tv", true
	case "WEBDL":
		return "web-dl", true
	case "WEBRIP":
		return "web-rip", true
	case "BLURAY":
		return "bluray", true
	}
	return "", false
}

// movieModifierCode maps a movie Modifier member. NONE is the empty code.
func movieModifierCode(member string) (string, bool) {
	switch member {
	case "NONE":
		return "", true
	case "REGIONAL":
		return "regional", true
	case "SCREENER":
		return "screener", true
	case "RAWHD":
		return "raw-hd", true
	case "BRDISK":
		return "br-disk", true
	case "REMUX":
		return "remux", true
	}
	return "", false
}

// Movie Quality members in Quality.All order.
const (
	rqUnknown = iota
	rqWORKPRINT
	rqCAM
	rqTELESYNC
	rqTELECINE
	rqDVDSCR
	rqREGIONAL
	rqSDTV
	rqDVD
	rqDVDR
	rqHDTV720p
	rqHDTV1080p
	rqHDTV2160p
	rqWEBDL480p
	rqWEBDL720p
	rqWEBDL1080p
	rqWEBDL2160p
	rqWEBRip480p
	rqWEBRip720p
	rqWEBRip1080p
	rqWEBRip2160p
	rqBluray480p
	rqBluray576p
	rqBluray720p
	rqBluray1080p
	rqBluray2160p
	rqRemux1080p
	rqRemux2160p
	rqBRDISK
	rqRAWHD
	rqCount
)

// movieDefs is the movie Quality table (Quality.cs).
func movieDefs() [rqCount]qualityDef {
	return [rqCount]qualityDef{
		rqUnknown:     {"Unknown", "unknown", "unknown", rsUnknown, "", 0},
		rqWORKPRINT:   {"WORKPRINT", "workprint", "workprint", rsWorkprint, "", 0},
		rqCAM:         {"CAM", "cam", "cam", rsCAM, "", 0},
		rqTELESYNC:    {"TELESYNC", "telesync", "telesync", rsTelesync, "", 0},
		rqTELECINE:    {"TELECINE", "telecine", "telecine", rsTelecine, "", 0},
		rqDVDSCR:      {"DVDSCR", "dvdscr", "dvd", rsDVD, "screener", 480},
		rqREGIONAL:    {"REGIONAL", "regional", "dvd", rsDVD, "regional", 480},
		rqSDTV:        {"SDTV", "sdtv", "tv", rsTV, "", 480},
		rqDVD:         {"DVD", "dvd", "dvd", rsDVD, "", 0},
		rqDVDR:        {"DVDR", "dvd-r", "dvd", rsDVD, "remux", 480},
		rqHDTV720p:    {"HDTV720p", "hdtv-720p", "tv", rsTV, "", 720},
		rqHDTV1080p:   {"HDTV1080p", "hdtv-1080p", "tv", rsTV, "", 1080},
		rqHDTV2160p:   {"HDTV2160p", "hdtv-2160p", "tv", rsTV, "", 2160},
		rqWEBDL480p:   {"WEBDL480p", "webdl-480p", "web-dl", rsWebDL, "", 480},
		rqWEBDL720p:   {"WEBDL720p", "webdl-720p", "web-dl", rsWebDL, "", 720},
		rqWEBDL1080p:  {"WEBDL1080p", "webdl-1080p", "web-dl", rsWebDL, "", 1080},
		rqWEBDL2160p:  {"WEBDL2160p", "webdl-2160p", "web-dl", rsWebDL, "", 2160},
		rqWEBRip480p:  {"WEBRip480p", "webrip-480p", "web-rip", rsWebRip, "", 480},
		rqWEBRip720p:  {"WEBRip720p", "webrip-720p", "web-rip", rsWebRip, "", 720},
		rqWEBRip1080p: {"WEBRip1080p", "webrip-1080p", "web-rip", rsWebRip, "", 1080},
		rqWEBRip2160p: {"WEBRip2160p", "webrip-2160p", "web-rip", rsWebRip, "", 2160},
		rqBluray480p:  {"Bluray480p", "bluray-480p", "bluray", rsBluray, "", 480},
		rqBluray576p:  {"Bluray576p", "bluray-576p", "bluray", rsBluray, "", 576},
		rqBluray720p:  {"Bluray720p", "bluray-720p", "bluray", rsBluray, "", 720},
		rqBluray1080p: {"Bluray1080p", "bluray-1080p", "bluray", rsBluray, "", 1080},
		rqBluray2160p: {"Bluray2160p", "bluray-2160p", "bluray", rsBluray, "", 2160},
		rqRemux1080p:  {"Remux1080p", "remux-1080p", "bluray", rsBluray, "remux", 1080},
		rqRemux2160p:  {"Remux2160p", "remux-2160p", "bluray", rsBluray, "remux", 2160},
		rqBRDISK:      {"BRDISK", "br-disk", "bluray", rsBluray, "br-disk", 1080},
		rqRAWHD:       {"RAWHD", "raw-hd", "tv", rsTV, "raw-hd", 1080},
	}
}

// qualityDefByMember finds a source Quality member by its name.
func qualityDefByMember(defs []qualityDef, member string) (qualityDef, bool) {
	for _, d := range defs {
		if d.member == member {
			return d, true
		}
	}
	return qualityDef{}, false
}

// seriesQualityDef and movieQualityDef resolve a source Quality member name
// (as it appears in the extracted corpora) to its explicit code definition.
func seriesQualityDef(member string) (qualityDef, bool) {
	defs := seriesDefs()
	return qualityDefByMember(defs[:], member)
}

func movieQualityDef(member string) (qualityDef, bool) {
	defs := movieDefs()
	return qualityDefByMember(defs[:], member)
}

// extensionOrdinalUpper upper-cases a file extension per rune, the comparison
// of the source's StringComparer.OrdinalIgnoreCase extension dictionary.
func extensionOrdinalUpper(ext string) string {
	out := make([]rune, 0, len(ext))
	for _, r := range ext {
		out = append(out, unicode.ToUpper(r))
	}
	return string(out)
}

// seriesExtensionQuality is MediaFileExtensions.GetQualityForExtension.
func seriesExtensionQuality(ext string) int {
	switch extensionOrdinalUpper(ext) {
	case ".M4V", ".3GP", ".NSV", ".TY", ".STRM", ".RM", ".RMVB", ".M3U", ".IFO", ".MOV", ".QT", ".DIVX", ".XVID",
		".BIVX", ".NRG", ".PVA", ".WMV", ".ASF", ".ASX", ".OGM", ".OGV", ".M2V", ".AVI", ".BIN", ".DAT", ".DVR-MS",
		".MPG", ".MPEG", ".MP4", ".AVC", ".VP3", ".SVQ3", ".NUV", ".VIV", ".DV", ".FLI", ".FLV", ".WPL":
		return sqSDTV
	case ".IMG", ".ISO", ".VOB":
		return sqDVD
	case ".MKV", ".TS", ".WTV":
		return sqHDTV720p
	case ".M2TS":
		return sqBluray720p
	}
	return sqUnknown
}

// movieExtensionQuality is the movie MediaFileExtensions.GetQualityForExtension.
func movieExtensionQuality(ext string) int {
	switch extensionOrdinalUpper(ext) {
	case ".M4V", ".3GP", ".NSV", ".TY", ".STRM", ".RM", ".RMVB", ".M3U", ".IFO", ".MOV", ".QT", ".DIVX", ".XVID",
		".BIVX", ".NRG", ".PVA", ".WMV", ".ASF", ".ASX", ".OGM", ".OGV", ".M2V", ".AVI", ".BIN", ".DAT", ".DVR-MS",
		".MPG", ".MPEG", ".MP4", ".AVC", ".VP3", ".SVQ3", ".NUV", ".VIV", ".DV", ".FLI", ".FLV", ".WPL",
		".TS", ".WTV":
		return rqSDTV
	case ".IMG", ".ISO", ".VOB":
		return rqDVD
	case ".MKV", ".MK3D":
		return rqWEBDL720p
	case ".M2TS":
		return rqBluray720p
	}
	return rqUnknown
}
