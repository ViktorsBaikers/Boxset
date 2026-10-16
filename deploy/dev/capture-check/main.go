// Command capture-check validates one untrusted browser screenshot read from
// standard input and writes its canonical static re-encoding to standard
// output. It is stdlib-only and is installed in the work image; the host
// runner feeds each exported capture to a fresh container running it.
//
// Usage: capture-check -name NAME -challenge HEX16 < raw.png > canonical.png
//
// Exit status: 0 success; 10 capture_missing; 11 capture_oversize;
// 12 capture_not_png; 13 capture_not_static; 14 capture_dimensions;
// 15 capture_challenge_mismatch; 2 usage; 1 internal I/O failure. The reason
// word is written to standard error.
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"hash/crc32"
	"image"
	"image/draw"
	"image/png"
	"io"
	"os"
	"strconv"
)

// maxPNGBytes caps both the raw input and the canonical output.
const maxPNGBytes = 2 * 1024 * 1024

const (
	exitOK         = 0
	exitInternal   = 1
	exitUsage      = 2
	exitMissing    = 10
	exitOversize   = 11
	exitNotPNG     = 12
	exitNotStatic  = 13
	exitDimensions = 14
	exitMismatch   = 15
)

// Challenge strip geometry: 2 rows x 33 cells, 8x8 opaque pixels each, at the
// bottom-left of the viewport. Bit 33*r+c is black (1) or white (0); bits 0-63
// are the challenge MSB first, bit 64 the mode (preview) and bit 65 the
// viewport (mobile).
const (
	stripRows  = 2
	stripCols  = 33
	stripCell  = 8
	stripBits  = stripRows * stripCols
	stripBlack = 64  // luma below this reads as bit 1
	stripWhite = 191 // luma above this reads as bit 0
)

type target struct {
	width, height   int
	preview, mobile bool
}

// lookup maps a fixed capture name to its target without package-level state.
func lookup(name string) (target, bool) {
	switch name {
	case "dev-desktop":
		return target{1440, 900, false, false}, true
	case "dev-mobile":
		return target{390, 844, false, true}, true
	case "preview-desktop":
		return target{1440, 900, true, false}, true
	case "preview-mobile":
		return target{390, 844, true, true}, true
	}
	return target{}, false
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("capture-check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	name := fs.String("name", "", "capture name")
	challengeText := fs.String("challenge", "", "16 lowercase hex digits")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return fail(stderr, exitUsage, "capture_usage")
	}
	t, ok := lookup(*name)
	if !ok {
		return fail(stderr, exitUsage, "capture_usage")
	}
	challenge, ok := parseChallenge(*challengeText)
	if !ok {
		return fail(stderr, exitUsage, "capture_usage")
	}
	data, err := io.ReadAll(io.LimitReader(stdin, maxPNGBytes+1))
	if err != nil {
		return fail(stderr, exitInternal, "capture_read_failed")
	}
	out, status, reason := validate(data, t, challenge, maxPNGBytes, maxPNGBytes)
	if status != exitOK {
		return fail(stderr, status, reason)
	}
	if _, err := stdout.Write(out); err != nil {
		return fail(stderr, exitInternal, "capture_write_failed")
	}
	return exitOK
}

func fail(stderr io.Writer, status int, reason string) int {
	_, _ = fmt.Fprintln(stderr, reason)
	return status
}

func parseChallenge(s string) (uint64, bool) {
	if len(s) != 16 {
		return 0, false
	}
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return 0, false
		}
	}
	v, err := strconv.ParseUint(s, 16, 64)
	return v, err == nil
}

// validate checks data against target t and the challenge and returns the
// canonical re-encoding. inLimit bounds the raw input, outLimit the output.
func validate(data []byte, t target, challenge uint64, inLimit, outLimit int) (out []byte, status int, reason string) {
	if len(data) == 0 {
		return nil, exitMissing, "capture_missing"
	}
	if len(data) > inLimit {
		return nil, exitOversize, "capture_oversize"
	}
	animated, ok := scanChunks(data)
	if !ok {
		return nil, exitNotPNG, "capture_not_png"
	}
	if animated {
		return nil, exitNotStatic, "capture_not_static"
	}
	// Dimensions are checked from the IHDR before any pixel allocation.
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, exitNotPNG, "capture_not_png"
	}
	if cfg.Width != t.width || cfg.Height != t.height {
		return nil, exitDimensions, "capture_dimensions"
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, exitNotPNG, "capture_not_png"
	}
	// The strip is read on the decoded image exactly as the browser wrote it
	// (full 16-bit alpha); normalizing to 8 bits first could round a
	// nonopaque centre up to opaque.
	if !stripMatches(img, t, challenge) {
		return nil, exitMismatch, "capture_challenge_mismatch"
	}
	canon := truecolour(img)
	var buf bytes.Buffer
	if err := png.Encode(&buf, canon); err != nil {
		return nil, exitInternal, "capture_encode_failed"
	}
	if buf.Len() > outLimit {
		return nil, exitOversize, "capture_oversize"
	}
	return buf.Bytes(), exitOK, ""
}

// scanChunks walks the PNG chunk structure: signature, bounds, CRC, IHDR
// first, IEND last and no trailing byte. animated reports acTL/fcTL/fdAT.
func scanChunks(data []byte) (animated, ok bool) {
	if len(data) < 8 || string(data[:8]) != "\x89PNG\r\n\x1a\n" {
		return false, false
	}
	pos := 8
	first := true
	for pos < len(data) {
		if len(data)-pos < 12 {
			return false, false
		}
		length := int64(binary.BigEndian.Uint32(data[pos:]))
		end := int64(pos) + 12 + length
		if length > 1<<31-1 || end > int64(len(data)) {
			return false, false
		}
		bodyEnd := pos + 8 + int(length)
		if crc32.ChecksumIEEE(data[pos+4:bodyEnd]) != binary.BigEndian.Uint32(data[bodyEnd:]) {
			return false, false
		}
		typ := string(data[pos+4 : pos+8])
		if first {
			if typ != "IHDR" || length != 13 {
				return false, false
			}
			first = false
		}
		switch typ {
		case "acTL", "fcTL", "fdAT":
			animated = true
		case "IEND":
			return animated, length == 0 && int(end) == len(data)
		}
		pos = int(end)
	}
	return false, false // no IEND
}

// truecolour returns img as an 8-bit RGBA/NRGBA image so the encoder emits a
// plain truecolour IHDR/IDAT/IEND stream and never a palette or tRNS chunk.
func truecolour(img image.Image) image.Image {
	switch m := img.(type) {
	case *image.RGBA, *image.NRGBA:
		return m
	}
	b := img.Bounds()
	n := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(n, n.Bounds(), img, b.Min, draw.Src)
	return n
}

// stripMatches reads the 66 strip cell centres at (8c+4, H-12+8r).
func stripMatches(img image.Image, t target, challenge uint64) bool {
	b := img.Bounds()
	for k := range stripBits {
		r, c := k/stripCols, k%stripCols
		x := b.Min.X + stripCell*c + stripCell/2
		y := b.Min.Y + t.height - 12 + stripCell*r
		bit, ok := pixelBit(img, x, y)
		if !ok || bit != expectedBit(challenge, t, k) {
			return false
		}
	}
	return true
}

func expectedBit(challenge uint64, t target, k int) int {
	switch {
	case k < 64:
		return int(challenge >> uint(63-k) & 1)
	case k == 64:
		return b2i(t.preview)
	default:
		return b2i(t.mobile)
	}
}

func b2i(v bool) int {
	if v {
		return 1
	}
	return 0
}

// pixelBit decodes one opaque cell centre: black is 1, white is 0; a
// nonopaque or intermediate pixel is not a valid bit.
func pixelBit(img image.Image, x, y int) (int, bool) {
	r, g, b, a := img.At(x, y).RGBA()
	if a != 0xffff {
		return 0, false
	}
	luma := (299*(r>>8) + 587*(g>>8) + 114*(b>>8)) / 1000
	switch {
	case luma < stripBlack:
		return 1, true
	case luma > stripWhite:
		return 0, true
	}
	return 0, false
}
