package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"math/rand/v2"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// The contract constants are literal on purpose: the tests prove the accepted
// 2,097,152-byte cap and the numeric statuses, never the production constants.
const (
	testChallenge = "a5c3f0e1927b64d8"
	pngSignature  = "\x89PNG\r\n\x1a\n"

	capBytes = 2097152

	stInternal   = 1
	stUsage      = 2
	stMissing    = 10
	stOversize   = 11
	stNotPNG     = 12
	stNotStatic  = 13
	stDimensions = 14
	stMismatch   = 15
)

type chunk struct {
	typ  string
	body []byte
}

// stripPattern derives the 66 strip bits straight from the documented
// protocol: challenge nibbles MSB first, then mode, then viewport.
func stripPattern(challenge string, preview, mobile bool) [66]bool {
	var bits [66]bool
	for i, ch := range challenge {
		v, err := strconv.ParseUint(string(ch), 16, 8)
		if err != nil {
			panic(err)
		}
		for j := range 4 {
			bits[4*i+j] = v>>uint(3-j)&1 == 1
		}
	}
	bits[64] = preview
	bits[65] = mobile
	return bits
}

func shade(v uint8) color.NRGBA { return color.NRGBA{R: v, G: v, B: v, A: 255} }

func newCanvas(w, h int, bits [66]bool) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	for k, one := range bits {
		r, c := k/33, k%33
		v := uint8(255)
		if one {
			v = 0
		}
		for y := h - 16 + 8*r; y < h-8+8*r; y++ {
			for x := 8 * c; x < 8*c+8; x++ {
				img.SetNRGBA(x, y, shade(v))
			}
		}
	}
	return img
}

// newCanvas16 is newCanvas at 16 bits per channel.
func newCanvas16(w, h int, bits [66]bool) *image.NRGBA64 {
	img := image.NewNRGBA64(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	for k, one := range bits {
		r, c := k/33, k%33
		v := uint16(0xffff)
		if one {
			v = 0
		}
		for y := h - 16 + 8*r; y < h-8+8*r; y++ {
			for x := 8 * c; x < 8*c+8; x++ {
				img.SetNRGBA64(x, y, color.NRGBA64{R: v, G: v, B: v, A: 0xffff})
			}
		}
	}
	return img
}

func encode(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func split(t *testing.T, data []byte) []chunk {
	t.Helper()
	var chunks []chunk
	for pos := 8; pos < len(data); {
		length := int(binary.BigEndian.Uint32(data[pos:]))
		chunks = append(chunks, chunk{
			typ:  string(data[pos+4 : pos+8]),
			body: append([]byte(nil), data[pos+8:pos+8+length]...),
		})
		pos += 12 + length
	}
	return chunks
}

func join(chunks []chunk) []byte {
	out := []byte(pngSignature)
	for _, c := range chunks {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(c.body)))
		out = append(out, n[:]...)
		typed := append([]byte(c.typ), c.body...)
		out = append(out, typed...)
		var sum [4]byte
		binary.BigEndian.PutUint32(sum[:], crc32.ChecksumIEEE(typed))
		out = append(out, sum[:]...)
	}
	return out
}

func insertAt(chunks []chunk, i int, extra ...chunk) []chunk {
	out := append([]chunk(nil), chunks[:i]...)
	out = append(out, extra...)
	return append(out, chunks[i:]...)
}

func types(chunks []chunk) []string {
	var out []string
	for _, c := range chunks {
		out = append(out, c.typ)
	}
	return out
}

func check(name, challenge string, data []byte) (status int, out []byte, reason string) {
	var stdout, stderr bytes.Buffer
	status = run([]string{"-name", name, "-challenge", challenge}, bytes.NewReader(data), &stdout, &stderr)
	return status, stdout.Bytes(), strings.TrimSpace(stderr.String())
}

func ihdr(w, h uint32, colourType byte) []byte {
	b := make([]byte, 13)
	binary.BigEndian.PutUint32(b[0:], w)
	binary.BigEndian.PutUint32(b[4:], h)
	b[8], b[9] = 8, colourType
	return b
}

func TestAcceptsEveryMappedCapture(t *testing.T) {
	cases := []struct {
		name            string
		w, h            int
		preview, mobile bool
	}{
		{"dev-desktop", 1440, 900, false, false},
		{"dev-mobile", 390, 844, false, true},
		{"preview-desktop", 1440, 900, true, false},
		{"preview-mobile", 390, 844, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := newCanvas(tc.w, tc.h, stripPattern(testChallenge, tc.preview, tc.mobile))
			status, out, reason := check(tc.name, testChallenge, encode(t, in))
			if status != 0 {
				t.Fatalf("status %d (%s), want 0", status, reason)
			}
			got, err := png.Decode(bytes.NewReader(out))
			if err != nil {
				t.Fatal(err)
			}
			if got.Bounds().Dx() != tc.w || got.Bounds().Dy() != tc.h {
				t.Fatalf("output %v, want %dx%d", got.Bounds(), tc.w, tc.h)
			}
			for y := range tc.h {
				for x := range tc.w {
					wr, wg, wb, wa := in.At(x, y).RGBA()
					gr, gg, gb, ga := got.At(x, y).RGBA()
					if wr != gr || wg != gg || wb != gb || wa != ga {
						t.Fatalf("pixel %d,%d changed by re-encoding", x, y)
					}
				}
			}
			chunks := split(t, out)
			if chunks[0].typ != "IHDR" || chunks[len(chunks)-1].typ != "IEND" {
				t.Fatalf("chunks %v", types(chunks))
			}
			for _, c := range chunks[1 : len(chunks)-1] {
				if c.typ != "IDAT" {
					t.Fatalf("canonical output has %s chunk", c.typ)
				}
			}
			if chunks[0].body[9] != 2 {
				t.Fatalf("colour type %d, want truecolour 2", chunks[0].body[9])
			}
		})
	}
}

func TestSizeCapsThroughTheProductionEntryPoint(t *testing.T) {
	good := encode(t, newCanvas(1440, 900, stripPattern(testChallenge, false, false)))
	chunks := split(t, good)
	padded := func(total int) []byte {
		bodyLen := total - len(good) - 12
		body := append([]byte("pad\x00"), bytes.Repeat([]byte{'a'}, bodyLen-4)...)
		return join(insertAt(chunks, len(chunks)-1, chunk{"tEXt", body}))
	}
	exact := padded(capBytes)
	if len(exact) != capBytes {
		t.Fatalf("fixture is %d bytes, want %d", len(exact), capBytes)
	}
	status, out, reason := check("dev-desktop", testChallenge, exact)
	if status != 0 {
		t.Fatalf("2097152 bytes: status %d (%s), want 0", status, reason)
	}
	for _, c := range split(t, out) {
		if c.typ != "IHDR" && c.typ != "IDAT" && c.typ != "IEND" {
			t.Fatalf("canonical output kept %s chunk", c.typ)
		}
	}
	over := padded(capBytes + 1)
	if len(over) != capBytes+1 {
		t.Fatalf("fixture is %d bytes, want %d", len(over), capBytes+1)
	}
	if status, out, reason := check("dev-desktop", testChallenge, over); status != stOversize || reason != "capture_oversize" || len(out) != 0 {
		t.Fatalf("2097153 bytes: status %d (%s) with %d output bytes, want 11 capture_oversize and none", status, reason, len(out))
	}
}

// noisyPaletted is an under-cap paletted capture whose truecolour canonical
// re-encoding is far above the cap: deterministic palette noise.
func noisyPaletted(t *testing.T) []byte {
	t.Helper()
	rng := rand.New(rand.NewPCG(1, 2))
	pal := make(color.Palette, 256)
	pal[0], pal[1] = shade(255), shade(0)
	for i := 2; i < 256; i++ {
		pal[i] = color.NRGBA{R: uint8(rng.UintN(256)), G: uint8(rng.UintN(256)), B: uint8(rng.UintN(256)), A: 255}
	}
	p := image.NewPaletted(image.Rect(0, 0, 1440, 900), pal)
	for i := range p.Pix {
		p.Pix[i] = uint8(2 + rng.UintN(254))
	}
	for k, one := range stripPattern(testChallenge, false, false) {
		r, c := k/33, k%33
		idx := uint8(0)
		if one {
			idx = 1
		}
		for y := 900 - 16 + 8*r; y < 900-8+8*r; y++ {
			for x := 8 * c; x < 8*c+8; x++ {
				p.SetColorIndex(x, y, idx)
			}
		}
	}
	return encode(t, p)
}

func TestCanonicalOutputOverTheProductionCapIsRejected(t *testing.T) {
	in := noisyPaletted(t)
	if len(in) > capBytes {
		t.Fatalf("fixture premise: input is %d bytes, must stay within %d", len(in), capBytes)
	}
	img, err := png.Decode(bytes.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	canon := image.NewNRGBA(img.Bounds())
	draw.Draw(canon, canon.Bounds(), img, img.Bounds().Min, draw.Src)
	if got := len(encode(t, canon)); got <= capBytes {
		t.Fatalf("fixture premise: canonical re-encoding is %d bytes, must exceed %d", got, capBytes)
	}
	status, out, reason := check("dev-desktop", testChallenge, in)
	if status != stOversize || reason != "capture_oversize" || len(out) != 0 {
		t.Fatalf("status %d (%s) with %d output bytes, want 11 capture_oversize and none", status, reason, len(out))
	}
}

func TestRejectsMalformedAndAnimated(t *testing.T) {
	good := encode(t, newCanvas(1440, 900, stripPattern(testChallenge, false, false)))
	chunks := split(t, good)
	idat := 0
	offset := 8
	for i, c := range chunks {
		if c.typ == "IDAT" {
			idat = i
			break
		}
		offset += 12 + len(c.body)
	}
	badCRC := append([]byte(nil), good...)
	badCRC[offset+8] ^= 0xff
	text := chunk{"tEXt", []byte("k\x00v")}
	cases := []struct {
		name   string
		input  []byte
		status int
		reason string
	}{
		{"empty", nil, stMissing, "capture_missing"},
		{"not a png", []byte("definitely not a png"), stNotPNG, "capture_not_png"},
		{"truncated", good[:len(good)-5], stNotPNG, "capture_not_png"},
		{"bad crc", badCRC, stNotPNG, "capture_not_png"},
		{"trailing byte", append(append([]byte(nil), good...), 0), stNotPNG, "capture_not_png"},
		{"ihdr not first", join(insertAt(chunks, 0, text)), stNotPNG, "capture_not_png"},
		{"missing iend", join(chunks[:len(chunks)-1]), stNotPNG, "capture_not_png"},
		{"acTL", join(insertAt(chunks, idat, chunk{"acTL", make([]byte, 8)})), stNotStatic, "capture_not_static"},
		{"fcTL", join(insertAt(chunks, idat, chunk{"fcTL", make([]byte, 26)})), stNotStatic, "capture_not_static"},
		{"fdAT", join(insertAt(chunks, len(chunks)-1, chunk{"fdAT", make([]byte, 8)})), stNotStatic, "capture_not_static"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, out, reason := check("dev-desktop", testChallenge, tc.input)
			if status != tc.status || reason != tc.reason || len(out) != 0 {
				t.Fatalf("status %d (%s) with %d output bytes, want %d %s and none", status, reason, len(out), tc.status, tc.reason)
			}
		})
	}
}

// TestDecoderErrorsBehindValidFraming covers inputs whose chunk structure and
// CRCs are all valid, so only IHDR semantics or the zlib stream are wrong.
func TestDecoderErrorsBehindValidFraming(t *testing.T) {
	good := encode(t, newCanvas(1440, 900, stripPattern(testChallenge, false, false)))
	chunks := split(t, good)
	var zdata []byte
	for _, c := range chunks {
		if c.typ == "IDAT" {
			zdata = append(zdata, c.body...)
		}
	}
	badDepth := ihdr(1440, 900, 2)
	badDepth[8] = 3
	cases := []struct {
		name   string
		chunks []chunk
	}{
		{"invalid bit depth", []chunk{{"IHDR", badDepth}, {"IDAT", zdata}, {"IEND", nil}}},
		{"corrupt zlib stream", []chunk{{"IHDR", ihdr(1440, 900, 2)}, {"IDAT", bytes.Repeat([]byte{0x5a}, 200)}, {"IEND", nil}}},
		{"truncated zlib stream", []chunk{{"IHDR", ihdr(1440, 900, 2)}, {"IDAT", zdata[:len(zdata)/2]}, {"IEND", nil}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, out, reason := check("dev-desktop", testChallenge, join(tc.chunks))
			if status != stNotPNG || reason != "capture_not_png" || len(out) != 0 {
				t.Fatalf("status %d (%s) with %d output bytes, want 12 capture_not_png and none", status, reason, len(out))
			}
		})
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestInternalIOFailures(t *testing.T) {
	args := []string{"-name", "dev-desktop", "-challenge", testChallenge}
	var stdout, stderr bytes.Buffer
	if status := run(args, failingReader{}, &stdout, &stderr); status != stInternal || strings.TrimSpace(stderr.String()) != "capture_read_failed" || stdout.Len() != 0 {
		t.Fatalf("failing reader: status %d (%s) with %d output bytes, want 1 capture_read_failed and none", status, strings.TrimSpace(stderr.String()), stdout.Len())
	}
	good := encode(t, newCanvas(1440, 900, stripPattern(testChallenge, false, false)))
	stderr.Reset()
	if status := run(args, bytes.NewReader(good), failingWriter{}, &stderr); status != stInternal || strings.TrimSpace(stderr.String()) != "capture_write_failed" {
		t.Fatalf("failing writer: status %d (%s), want 1 capture_write_failed", status, strings.TrimSpace(stderr.String()))
	}
	// A reader that fails after valid bytes is still a read failure.
	stderr.Reset()
	partial := io.MultiReader(bytes.NewReader(good[:100]), failingReader{})
	if status := run(args, partial, &stdout, &stderr); status != stInternal {
		t.Fatalf("partial then failing reader: status %d, want 1", status)
	}
}

func TestDimensionsAreCheckedBeforeAllocation(t *testing.T) {
	wrongHeight := encode(t, newCanvas(1440, 901, stripPattern(testChallenge, false, false)))
	desktop := encode(t, newCanvas(1440, 900, stripPattern(testChallenge, false, false)))
	if status, _, reason := check("dev-desktop", testChallenge, wrongHeight); status != stDimensions || reason != "capture_dimensions" {
		t.Fatalf("1440x901: status %d (%s), want 14", status, reason)
	}
	if status, _, reason := check("dev-mobile", testChallenge, desktop); status != stDimensions || reason != "capture_dimensions" {
		t.Fatalf("desktop image under mobile name: status %d (%s), want 14", status, reason)
	}
	hostile := join([]chunk{{typ: "IHDR", body: ihdr(100000, 100000, 2)}, {typ: "IEND"}})
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	status, _, reason := check("dev-desktop", testChallenge, hostile)
	runtime.ReadMemStats(&after)
	if status != stDimensions || reason != "capture_dimensions" {
		t.Fatalf("100000x100000: status %d (%s), want 14", status, reason)
	}
	if grown := after.TotalAlloc - before.TotalAlloc; grown > 16<<20 {
		t.Fatalf("oversized IHDR allocated %d bytes before rejection", grown)
	}
}

func TestChallengeBinding(t *testing.T) {
	for _, k := range []int{0, 17, 63, 64, 65} {
		bits := stripPattern(testChallenge, false, false)
		bits[k] = !bits[k]
		status, _, reason := check("dev-desktop", testChallenge, encode(t, newCanvas(1440, 900, bits)))
		if status != stMismatch || reason != "capture_challenge_mismatch" {
			t.Fatalf("flipped bit %d: status %d (%s), want 15", k, status, reason)
		}
	}
	replay := encode(t, newCanvas(1440, 900, stripPattern("0000000000000001", false, false)))
	if status, _, _ := check("dev-desktop", testChallenge, replay); status != stMismatch {
		t.Fatalf("replayed capture: status %d, want 15", status)
	}
}

func TestStripCentreThresholds(t *testing.T) {
	// testChallenge starts with 1010: cell 0 is black (1), cell 1 is white (0).
	cases := []struct {
		name   string
		x      int
		value  color.NRGBA
		status int
	}{
		{"black edge 63", 4, shade(63), 0},
		{"black edge 64", 4, shade(64), stMismatch},
		{"white edge 192", 12, shade(192), 0},
		{"white edge 191", 12, shade(191), stMismatch},
		{"grey", 4, shade(128), stMismatch},
		{"nonopaque", 4, color.NRGBA{A: 128}, stMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			img := newCanvas(1440, 900, stripPattern(testChallenge, false, false))
			img.SetNRGBA(tc.x, 900-12, tc.value)
			if status, _, reason := check("dev-desktop", testChallenge, encode(t, img)); status != tc.status {
				t.Fatalf("status %d (%s), want %d", status, reason, tc.status)
			}
		})
	}
	// Pixels away from the cell centres carry no information.
	img := newCanvas(1440, 900, stripPattern(testChallenge, false, false))
	img.SetNRGBA(0, 900-16, shade(128))
	if status, _, reason := check("dev-desktop", testChallenge, encode(t, img)); status != 0 {
		t.Fatalf("off-centre grey: status %d (%s), want 0", status, reason)
	}
}

// TestSixteenBitAlphaIsJudgedBeforeNormalization: a centre alpha of 0xfffe is
// nonopaque, but an 8-bit conversion rounds it to opaque, so the strip must be
// read on the decoded image first.
func TestSixteenBitAlphaIsJudgedBeforeNormalization(t *testing.T) {
	for _, tc := range []struct {
		alpha  uint16
		status int
	}{{0xffff, 0}, {0xfffe, stMismatch}} {
		img := newCanvas16(1440, 900, stripPattern(testChallenge, false, false))
		img.SetNRGBA64(4, 900-12, color.NRGBA64{A: tc.alpha}) // cell 0 is black
		in := encode(t, img)
		if got := split(t, in)[0].body[8]; got != 16 {
			t.Fatalf("fixture bit depth %d, want 16", got)
		}
		if status, _, reason := check("dev-desktop", testChallenge, in); status != tc.status {
			t.Fatalf("alpha %#04x: status %d (%s), want %d", tc.alpha, status, reason, tc.status)
		}
	}
}

func TestPalettedInputBecomesTruecolour(t *testing.T) {
	bits := stripPattern(testChallenge, false, false)
	build := func(background uint8) []byte {
		pal := color.Palette{shade(255), shade(0), color.NRGBA{}}
		p := image.NewPaletted(image.Rect(0, 0, 1440, 900), pal)
		for i := range p.Pix {
			p.Pix[i] = background
		}
		for k, one := range bits {
			r, c := k/33, k%33
			for y := 900 - 16 + 8*r; y < 900-8+8*r; y++ {
				for x := 8 * c; x < 8*c+8; x++ {
					if one {
						p.SetColorIndex(x, y, 1)
					} else {
						p.SetColorIndex(x, y, 0)
					}
				}
			}
		}
		return encode(t, p)
	}
	for _, tc := range []struct {
		name       string
		background uint8
		colourType byte
	}{{"opaque palette", 0, 2}, {"transparent background", 2, 6}} {
		t.Run(tc.name, func(t *testing.T) {
			in := build(tc.background)
			if got := split(t, in)[0].body[9]; got != 3 {
				t.Fatalf("fixture colour type %d, want paletted 3", got)
			}
			status, out, reason := check("dev-desktop", testChallenge, in)
			if status != 0 {
				t.Fatalf("status %d (%s), want 0", status, reason)
			}
			chunks := split(t, out)
			for _, c := range chunks {
				if c.typ == "PLTE" || c.typ == "tRNS" {
					t.Fatalf("canonical output kept %s", c.typ)
				}
			}
			if got := chunks[0].body[9]; got != tc.colourType {
				t.Fatalf("output colour type %d, want %d", got, tc.colourType)
			}
		})
	}
}

func TestMetadataChunksAreDropped(t *testing.T) {
	good := encode(t, newCanvas(1440, 900, stripPattern(testChallenge, false, false)))
	chunks := split(t, good)
	withText := join(insertAt(chunks, 1,
		chunk{"tEXt", []byte("k\x00v")},
		chunk{"zTXt", []byte("k\x00\x00xyz")},
		chunk{"iTXt", []byte("k\x00\x00\x00\x00\x00v")},
	))
	status, out, reason := check("dev-desktop", testChallenge, withText)
	if status != 0 {
		t.Fatalf("status %d (%s), want 0", status, reason)
	}
	for _, c := range split(t, out) {
		if c.typ != "IHDR" && c.typ != "IDAT" && c.typ != "IEND" {
			t.Fatalf("canonical output kept %s chunk", c.typ)
		}
	}
}

func TestUsageErrors(t *testing.T) {
	good := encode(t, newCanvas(1440, 900, stripPattern(testChallenge, false, false)))
	for _, tc := range []struct{ name, challenge string }{
		{"unknown-name", testChallenge},
		{"dev-desktop", "A5C3F0E1927B64D8"},
		{"dev-desktop", "a5c3"},
		{"dev-desktop", ""},
	} {
		if status, _, _ := check(tc.name, tc.challenge, good); status != stUsage {
			t.Fatalf("%q/%q: status %d, want usage %d", tc.name, tc.challenge, status, stUsage)
		}
	}
	var out, errOut bytes.Buffer
	if status := run([]string{"-name", "dev-desktop", "-challenge", testChallenge, "extra"}, bytes.NewReader(good), &out, &errOut); status != stUsage {
		t.Fatalf("stray argument: status %d, want usage %d", status, stUsage)
	}
}
