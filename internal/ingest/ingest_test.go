package ingest

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"image"
	"image/png"
	"io"
	"runtime"
	"strings"
	"testing"
)

// Body takes no declared length: its parameters are the reader, the encoding and
// the cap, so no transport metadata can widen the read. This fails to build if
// that changes.
var _ func(io.Reader, string, int64) ([]byte, error) = Body

// TestInputLimits requires every ADR 0004 §6 class to admit an input of exactly
// its cap and to refuse cap+1 with that class's structured reason, hostile
// shapes included: nesting, invalid UTF-8, trailing data, CRLF and LF record
// terminators, a truncated body, a forged declared length inside a compressed
// member (TestBodyGzipBomb) and a hostile image header. Each refusal is an
// *Error with the documented reason and parameters, never a shortened value
// presented as success; a body with no declared length is bounded at the HTTP
// boundary in cmd/boxset/ingest_integration_test.go. Image bounds are decided
// from the header; Image's post-decode re-check is not exercised here because
// the three registered decoders cannot reach it: PNG and JPEG report the
// header's dimensions, and the standard library gif decoder rejects a frame
// larger than the logical screen.
func TestInputLimits(t *testing.T) {
	t.Run("json accepts its byte cap", func(t *testing.T) {
		var value struct{ Pad string }
		document := jsonOfSize(int(MaxJSONBytes))
		if err := JSON(document, &value); err != nil {
			t.Fatalf("JSON at the byte cap: %v", err)
		}
		// The whole document decoded: the pad fills every byte between the two
		// literals, so a short or truncated read shows up here.
		const literals = len(`{"pad":""}`)
		if len(value.Pad) != len(document)-literals {
			t.Fatalf("decoded pad = %d bytes, want %d", len(value.Pad), len(document)-literals)
		}
	})
	t.Run("json refuses one byte over its byte cap", func(t *testing.T) {
		var value struct{ Pad string }
		rejected := wantReason(t, JSON(jsonOfSize(int(MaxJSONBytes)+1), &value), reasonJSONTooLarge)
		if rejected["limit_bytes"] != "1048576" || rejected["observed_bytes"] != "1048577" {
			t.Fatalf("params = %v, want limit_bytes=1048576 observed_bytes=1048577", rejected)
		}
	})
	t.Run("json accepts the depth cap", func(t *testing.T) {
		var value any
		if err := JSON(nestedJSON(maxJSONDepth), &value); err != nil {
			t.Fatalf("JSON at the depth cap: %v", err)
		}
	})
	t.Run("json refuses one level over the depth cap", func(t *testing.T) {
		var value any
		rejected := wantReason(t, JSON(nestedJSON(maxJSONDepth+1), &value), reasonJSONDepthExceeded)
		if rejected["limit_depth"] != "64" || rejected["observed_depth"] != "65" {
			t.Fatalf("params = %v, want limit_depth=64 observed_depth=65", rejected)
		}
	})
	t.Run("json counts nesting outside strings only", func(t *testing.T) {
		brackets := strings.Repeat("[", 4*maxJSONDepth) + strings.Repeat("]", 4*maxJSONDepth)
		var value struct{ Pad string }
		if err := JSON([]byte(`{"pad":"`+brackets+`"}`), &value); err != nil || value.Pad != brackets {
			t.Fatalf("a string of %d brackets: err=%v", len(brackets), err)
		}
		// An escaped quote does not close the string, so the brackets behind it are
		// still text and still do not nest.
		var escaped struct{ Pad string }
		if err := JSON([]byte(`{"pad":"quote \" then `+brackets+`"}`), &escaped); err != nil || !strings.HasSuffix(escaped.Pad, brackets) {
			t.Fatalf("an escaped quote before %d brackets: err=%v", len(brackets), err)
		}
	})
	t.Run("json refuses invalid utf8", func(t *testing.T) {
		var value struct{ Pad string }
		rejected := wantReason(t, JSON([]byte("{\"pad\":\"\xff\"}"), &value), reasonUTF8Invalid)
		if rejected["offset"] != "8" {
			t.Fatalf("offset = %q, want 8", rejected["offset"])
		}
	})
	t.Run("json refuses trailing data", func(t *testing.T) {
		var value any
		err := JSON([]byte(`{"pad":"x"}x`), &value)
		wantReason(t, err, reasonJSONMalformed)
	})
	t.Run("json refuses malformed documents", func(t *testing.T) {
		var value any
		err := JSON([]byte(`{"pad":`), &value)
		wantReason(t, err, reasonJSONMalformed)
	})
	t.Run("json refuses a value its own shape rejects", func(t *testing.T) {
		var value shapeValue
		err := JSON([]byte(`{"n":2}`), &value)
		wantReason(t, err, reasonJSONInvalidShape)
		if !errors.Is(err, errShapeIncomplete) {
			t.Fatalf("the validator error is not reachable through Unwrap: %v", err)
		}
	})

	t.Run("body accepts its byte cap", func(t *testing.T) {
		plain := identical(int(maxBodyBytes))
		data, err := Body(bytes.NewReader(plain), "", maxBodyBytes)
		if err != nil || !bytes.Equal(data, plain) {
			t.Fatalf("identity body at the cap: %d bytes, %v", len(data), err)
		}
	})
	t.Run("body refuses one byte over its byte cap", func(t *testing.T) {
		_, err := Body(bytes.NewReader(identical(int(maxBodyBytes)+1)), "", maxBodyBytes)
		rejected := wantReason(t, err, reasonBodyTooLarge)
		if rejected["limit_bytes"] != "16777216" || rejected["observed_bytes"] != "16777217" {
			t.Fatalf("params = %v, want limit_bytes=16777216 observed_bytes=16777217", rejected)
		}
	})
	t.Run("body accepts gzip at its byte cap", func(t *testing.T) {
		plain := identical(int(maxBodyBytes))
		data, err := Body(bytes.NewReader(gzipBytes(t, plain, flate.BestSpeed)), "gzip", maxBodyBytes)
		if err != nil || !bytes.Equal(data, plain) {
			t.Fatalf("gzip body at the cap: %d bytes, %v", len(data), err)
		}
	})
	t.Run("body refuses gzip one byte over its byte cap", func(t *testing.T) {
		compressed := gzipBytes(t, identical(int(maxBodyBytes)+1), flate.BestSpeed)
		_, err := Body(bytes.NewReader(compressed), "gzip", maxBodyBytes)
		wantReason(t, err, reasonBodyTooLarge)
	})
	t.Run("body accepts deflate at its byte cap", func(t *testing.T) {
		plain := identical(int(maxBodyBytes))
		data, err := Body(bytes.NewReader(zlibBytes(t, plain)), "deflate", maxBodyBytes)
		if err != nil || !bytes.Equal(data, plain) {
			t.Fatalf("deflate body at the cap: %d bytes, %v", len(data), err)
		}
	})
	t.Run("body refuses deflate one byte over its byte cap", func(t *testing.T) {
		compressed := zlibBytes(t, identical(int(maxBodyBytes)+1))
		_, err := Body(bytes.NewReader(compressed), "deflate", maxBodyBytes)
		wantReason(t, err, reasonBodyTooLarge)
	})
	t.Run("body refuses an unsupported encoding", func(t *testing.T) {
		_, err := Body(strings.NewReader(`{"pad":"x"}`), "br", maxBodyBytes)
		rejected := wantReason(t, err, reasonBodyEncodingUnsupported)
		if rejected["encoding"] != "br" {
			t.Fatalf("encoding = %q, want br", rejected["encoding"])
		}
	})
	t.Run("body refuses a truncated compression stream", func(t *testing.T) {
		compressed := gzipBytes(t, identical(1<<20), flate.BestSpeed)
		_, err := Body(bytes.NewReader(compressed[:len(compressed)/2]), "gzip", maxBodyBytes)
		rejected := wantReason(t, err, reasonBodyPartial)
		if rejected["encoding"] != "gzip" {
			t.Fatalf("encoding = %q, want gzip", rejected["encoding"])
		}
	})
	t.Run("body refuses a truncated identity body", func(t *testing.T) {
		_, err := Body(io.MultiReader(strings.NewReader("synthetic"), failingReader{}), "", maxBodyBytes)
		wantReason(t, err, reasonBodyPartial)
	})
	t.Run("body clamps a caller cap above the class cap", func(t *testing.T) {
		// A caller passing 1 GiB must still be bounded by the body class cap.
		_, err := Body(bytes.NewReader(identical(int(maxBodyBytes)+1)), "", 1<<30)
		rejected := wantReason(t, err, reasonBodyTooLarge)
		if rejected["limit_bytes"] != "16777216" {
			t.Fatalf("params = %v, want limit_bytes=16777216", rejected)
		}
	})
	t.Run("body honours a caller cap below the class cap", func(t *testing.T) {
		data, err := Body(bytes.NewReader(identical(1024)), "", 1024)
		if err != nil || len(data) != 1024 {
			t.Fatalf("a body at a lower caller cap: %d bytes, %v", len(data), err)
		}
		_, err = Body(bytes.NewReader(identical(1025)), "", 1024)
		rejected := wantReason(t, err, reasonBodyTooLarge)
		if rejected["limit_bytes"] != "1024" || rejected["observed_bytes"] != "1025" {
			t.Fatalf("params = %v, want limit_bytes=1024 observed_bytes=1025", rejected)
		}
	})
	t.Run("body refuses a non-positive caller cap", func(t *testing.T) {
		for _, bound := range []int64{0, -1} {
			_, err := Body(strings.NewReader("synthetic"), "", bound)
			var rejected *Error
			if err == nil || errors.As(err, &rejected) {
				t.Fatalf("bound %d: error = %v, want a programmer error", bound, err)
			}
			if !strings.Contains(err.Error(), "not positive") {
				t.Fatalf("bound %d: error = %v, want a non-positive bound message", bound, err)
			}
		}
	})

	t.Run("feed accepts the entry cap", func(t *testing.T) {
		document := feedOf(maxFeedEntries)
		count, err := Feed(document)
		if err != nil || count != maxFeedEntries {
			t.Fatalf("feed at the entry cap: %d entries, %v", count, err)
		}
		// The closing bracket was reached, so the whole array was consumed: the
		// same document without its last byte is refused as malformed.
		_, err = Feed(document[:len(document)-1])
		wantReason(t, err, reasonFeedMalformed)
	})
	t.Run("feed refuses one entry over the entry cap", func(t *testing.T) {
		_, err := Feed(feedOf(maxFeedEntries + 1))
		rejected := wantReason(t, err, reasonFeedEntriesExceeded)
		if rejected["limit_entries"] != "10000" || rejected["observed_entries"] != "10001" {
			t.Fatalf("params = %v, want limit_entries=10000 observed_entries=10001", rejected)
		}
	})
	t.Run("feed refuses malformed documents", func(t *testing.T) {
		for _, document := range []string{`[`, `[{}`, `{}`, `[] x`, `[{}] [{}]`, ``} {
			if _, err := Feed([]byte(document)); err != nil {
				wantReason(t, err, reasonFeedMalformed)
				continue
			}
			t.Fatalf("feed accepted malformed document %q", document)
		}
	})
	t.Run("feed refuses invalid utf8", func(t *testing.T) {
		_, err := Feed([]byte("[{\"pad\":\"\xff\"}]"))
		wantReason(t, err, reasonUTF8Invalid)
	})

	t.Run("dataset accepts the record cap", func(t *testing.T) {
		compressed, records := datasetGZIP(t, int64(maxDatasetRecords)*16, 16, flate.BestSpeed)
		count, err := Dataset(bytes.NewReader(compressed), func(json.RawMessage) error { return nil })
		if err != nil || count != maxDatasetRecords || records != maxDatasetRecords {
			t.Fatalf("generation at the record cap: %d records of %d, %v", count, records, err)
		}
	})
	t.Run("dataset refuses one record over the record cap", func(t *testing.T) {
		compressed, _ := datasetGZIP(t, int64(maxDatasetRecords+1)*16, 16, flate.BestSpeed)
		_, err := Dataset(bytes.NewReader(compressed), func(json.RawMessage) error { return nil })
		rejected := wantReason(t, err, reasonDatasetRecordsExceeded)
		if rejected["limit_records"] != "1000000" || rejected["observed_records"] != "1000001" {
			t.Fatalf("params = %v, want limit_records=1000000 observed_records=1000001", rejected)
		}
	})
	t.Run("dataset accepts a record at its byte cap", func(t *testing.T) {
		// The generator counts the line including its newline, so a record of
		// exactly maxRecordBytes content is a line of maxRecordBytes+1 bytes.
		compressed, _ := datasetGZIP(t, 2*(maxRecordBytes+1), maxRecordBytes+1, flate.BestSpeed)
		count, err := Dataset(bytes.NewReader(compressed), func(json.RawMessage) error { return nil })
		if err != nil || count != 2 {
			t.Fatalf("records at the byte cap: %d records, %v", count, err)
		}
	})
	t.Run("dataset refuses a record one byte over its byte cap", func(t *testing.T) {
		compressed, _ := datasetGZIP(t, 2*(maxRecordBytes+2), maxRecordBytes+2, flate.BestSpeed)
		_, err := Dataset(bytes.NewReader(compressed), func(json.RawMessage) error { return nil })
		rejected := wantReason(t, err, reasonDatasetRecordTooLarge)
		if rejected["limit_bytes"] != "65536" {
			t.Fatalf("params = %v, want limit_bytes=65536", rejected)
		}
	})
	t.Run("dataset accepts a record at its byte cap ending in CRLF", func(t *testing.T) {
		// A CRLF terminator costs one buffer byte more than LF, so the scanner bound
		// must carry a full record plus its longest terminator.
		plain := strings.Repeat(string(ndjsonRecord(maxRecordBytes))+"\r\n", 2)
		count, err := Dataset(bytes.NewReader(gzipBytes(t, []byte(plain), flate.BestSpeed)), func(json.RawMessage) error { return nil })
		if err != nil || count != 2 {
			t.Fatalf("CRLF records at the byte cap: %d records, %v", count, err)
		}
	})
	t.Run("dataset refuses a CRLF record one byte over its byte cap", func(t *testing.T) {
		plain := strings.Repeat(string(ndjsonRecord(maxRecordBytes+1))+"\r\n", 2)
		_, err := Dataset(bytes.NewReader(gzipBytes(t, []byte(plain), flate.BestSpeed)), func(json.RawMessage) error { return nil })
		rejected := wantReason(t, err, reasonDatasetRecordTooLarge)
		if rejected["limit_bytes"] != "65536" {
			t.Fatalf("params = %v, want limit_bytes=65536", rejected)
		}
	})
	t.Run("dataset accepts the decoded byte cap", func(t *testing.T) {
		compressed, records := datasetGZIP(t, maxDatasetDecodedBytes, 4<<10, flate.BestSpeed)
		count, err := Dataset(bytes.NewReader(compressed), func(json.RawMessage) error { return nil })
		if err != nil || count != records || count != int(maxDatasetDecodedBytes)/(4<<10) {
			t.Fatalf("generation at the decoded cap: %d records of %d, %v", count, records, err)
		}
	})
	t.Run("dataset refuses one decoded byte over the decoded cap", func(t *testing.T) {
		compressed, _ := datasetGZIP(t, maxDatasetDecodedBytes+1, 4<<10, flate.BestSpeed)
		_, err := Dataset(bytes.NewReader(compressed), func(json.RawMessage) error { return nil })
		rejected := wantReason(t, err, reasonDatasetDecodedTooLarge)
		if rejected["limit_bytes"] != "268435456" {
			t.Fatalf("params = %v, want limit_bytes=268435456", rejected)
		}
	})
	t.Run("dataset accepts the compressed byte cap", func(t *testing.T) {
		compressed, records := storedGZIP(t, maxDatasetCompressedBytes)
		count, err := Dataset(bytes.NewReader(compressed), func(json.RawMessage) error { return nil })
		if err != nil || count != records || count == 0 {
			t.Fatalf("generation at the compressed cap: %d records of %d, %v", count, records, err)
		}
	})
	t.Run("dataset refuses one compressed byte over the compressed cap", func(t *testing.T) {
		compressed, _ := storedGZIP(t, maxDatasetCompressedBytes+1)
		_, err := Dataset(bytes.NewReader(compressed), func(json.RawMessage) error { return nil })
		rejected := wantReason(t, err, reasonDatasetCompressedTooLarge)
		if rejected["limit_bytes"] != "67108864" {
			t.Fatalf("params = %v, want limit_bytes=67108864", rejected)
		}
	})
	t.Run("dataset refuses malformed records", func(t *testing.T) {
		for _, plain := range []string{"{\"a\":1}\n{\"b\":\n", "{\"a\":1}\n\n{\"a\":2}\n", "{\"a\":1}x\n", "not json\n"} {
			_, err := Dataset(bytes.NewReader(gzipBytes(t, []byte(plain), flate.BestSpeed)), func(json.RawMessage) error { return nil })
			wantReason(t, err, reasonDatasetMalformed)
		}
	})
	t.Run("dataset refuses a truncated generation", func(t *testing.T) {
		compressed := gzipBytes(t, []byte("{\"a\":1}\n{\"a\":2}\n"), flate.BestSpeed)
		_, err := Dataset(bytes.NewReader(compressed[:len(compressed)-6]), func(json.RawMessage) error { return nil })
		wantReason(t, err, reasonDatasetMalformed)
	})
	t.Run("dataset refuses invalid utf8", func(t *testing.T) {
		_, err := Dataset(bytes.NewReader(gzipBytes(t, []byte("{\"a\":\"\xff\"}\n"), flate.BestSpeed)), func(json.RawMessage) error { return nil })
		wantReason(t, err, reasonUTF8Invalid)
	})
	t.Run("dataset refuses a callback error and reports no success", func(t *testing.T) {
		compressed := gzipBytes(t, []byte("{\"a\":1}\n{\"a\":2}\n{\"a\":3}\n"), flate.BestSpeed)
		calls := 0
		count, err := Dataset(bytes.NewReader(compressed), func(json.RawMessage) error {
			calls++
			if calls == 2 {
				return errCallback
			}
			return nil
		})
		if !errors.Is(err, errCallback) {
			t.Fatalf("error = %v, want the callback's own error", err)
		}
		if count != 0 {
			t.Fatalf("count = %d after a callback error, want 0", count)
		}
		if calls != 2 {
			t.Fatalf("the walk called the callback %d times, want 2", calls)
		}
	})

	t.Run("image accepts its encoded byte cap", func(t *testing.T) {
		img, format, err := Image(bytes.NewReader(pngPadded(t, 1, 1, int(maxImageBytes))))
		if err != nil || format != "png" || img.Bounds().Dx() != 1 || img.Bounds().Dy() != 1 {
			t.Fatalf("png at the encoded cap: %v %q %v", img, format, err)
		}
	})
	t.Run("image refuses one byte over its encoded cap", func(t *testing.T) {
		_, _, err := Image(bytes.NewReader(pngPadded(t, 1, 1, int(maxImageBytes)+1)))
		rejected := wantReason(t, err, reasonImageTooLarge)
		if rejected["limit_bytes"] != "10485760" || rejected["observed_bytes"] != "10485761" {
			t.Fatalf("params = %v, want limit_bytes=10485760 observed_bytes=10485761", rejected)
		}
	})
	t.Run("image accepts the pixel cap", func(t *testing.T) {
		img, _, err := Image(bytes.NewReader(pngImage(t, 4000, 4000)))
		if err != nil || img.Bounds().Dx()*img.Bounds().Dy() != maxImagePixels {
			t.Fatalf("image at the pixel cap: %v %v", img.Bounds(), err)
		}
	})
	t.Run("image refuses one pixel over the pixel cap", func(t *testing.T) {
		_, _, err := Image(bytes.NewReader(pngHeaderOnly(8192, 1954)))
		rejected := wantReason(t, err, reasonImagePixelsExceeded)
		if rejected["pixels"] != "16007168" || rejected["limit_pixels"] != "16000000" {
			t.Fatalf("params = %v, want pixels=16007168 limit_pixels=16000000", rejected)
		}
	})
	t.Run("image accepts a side at its cap", func(t *testing.T) {
		img, _, err := Image(bytes.NewReader(pngImage(t, maxImageSide, 1)))
		if err != nil || img.Bounds().Dx() != maxImageSide {
			t.Fatalf("image with an %d-pixel side: %v %v", maxImageSide, img.Bounds(), err)
		}
	})
	t.Run("image refuses a side over its cap", func(t *testing.T) {
		_, _, err := Image(bytes.NewReader(pngHeaderOnly(maxImageSide+1, 1)))
		rejected := wantReason(t, err, reasonImageDimensionsInvalid)
		if rejected["width"] != "8193" || rejected["limit_side"] != "8192" {
			t.Fatalf("params = %v, want width=8193 limit_side=8192", rejected)
		}
	})
	t.Run("image refuses data it cannot decode", func(t *testing.T) {
		_, _, err := Image(bytes.NewReader([]byte("not an image at all")))
		wantReason(t, err, reasonImageMalformed)
	})
}

// TestBodyGzipBomb requires a decompression bomb to be refused at the decoded
// cap: a small gzip member expanding past 16 MiB and the same member with a
// forged ISIZE trailer claiming 1,024 bytes. Neither may be decoded whole,
// truncated or accepted, and the member at the cap still decodes completely.
func TestBodyGzipBomb(t *testing.T) {
	plain := identical(int(maxBodyBytes) + 1)
	bomb := gzipBytes(t, plain, flate.BestSpeed)
	if len(bomb) > int(maxBodyBytes)/64 {
		t.Fatalf("the member is %d compressed bytes with no bomb shape", len(bomb))
	}

	_, err := Body(bytes.NewReader(bomb), "gzip", maxBodyBytes)
	wantReason(t, err, reasonBodyTooLarge)

	forged := bytes.Clone(bomb)
	binary.LittleEndian.PutUint32(forged[len(forged)-4:], 1024)
	_, err = Body(bytes.NewReader(forged), "gzip", maxBodyBytes)
	wantReason(t, err, reasonBodyTooLarge)

	// The same member at the cap still decodes completely, so the refusal above
	// is the cap and not a broken member.
	data, err := Body(bytes.NewReader(gzipBytes(t, identical(int(maxBodyBytes)), flate.BestSpeed)), "gzip", maxBodyBytes)
	if err != nil || len(data) != int(maxBodyBytes) {
		t.Fatalf("the member at the cap: %d bytes, %v", len(data), err)
	}
}

// TestRejectHeapBounded requires a refusal to peak inside one cap of heap instead
// of buffering the input. The body case feeds 64 MiB (4x the cap) and allows
// cap+1 MiB; the dataset case feeds a generation decoding to 512 MiB (2x the
// decoded cap), so a read-everything-then-refuse path peaks far past the
// allowance while an incremental refusal does not. Peak samples are taken inside
// the read and inside the record callback, so the check does not depend on when a
// collection happens to run, and the retained heap is compared after a collection.
func TestRejectHeapBounded(t *testing.T) {
	t.Run("body", func(t *testing.T) {
		before := settledHeap(t)
		sampler := &peakHeap{every: 1, peak: before}
		_, err := Body(samplingReader{r: &repeatReader{b: 'x', n: 4 * maxBodyBytes}, sampler: sampler}, "", maxBodyBytes)
		wantReason(t, err, reasonBodyTooLarge)
		assertHeapGrowth(t, before, sampler.peak, maxBodyBytes+mib)
	})
	t.Run("dataset", func(t *testing.T) {
		compressed, _ := datasetGZIP(t, 2*maxDatasetDecodedBytes, 4<<10, flate.BestSpeed)
		before := settledHeap(t)
		sampler := &peakHeap{every: 1024, peak: before}
		_, err := Dataset(bytes.NewReader(compressed), func(json.RawMessage) error {
			sampler.sample()
			return nil
		})
		wantReason(t, err, reasonDatasetDecodedTooLarge)
		assertHeapGrowth(t, before, sampler.peak, maxDatasetDecodedBytes+mib)
	})
}

// peakHeap tracks the highest live heap it samples.
type peakHeap struct {
	every int
	count int
	peak  uint64
}

// sample reads the live heap on every every-th call and keeps the highest reading.
func (p *peakHeap) sample() {
	p.count++
	if p.every > 1 && p.count%p.every != 0 {
		return
	}
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	if stats.HeapAlloc > p.peak {
		p.peak = stats.HeapAlloc
	}
}

// samplingReader reports every read to a peak sampler, so the peak of a bounded
// read is observable before the refusal returns.
type samplingReader struct {
	r       io.Reader
	sampler *peakHeap
}

func (s samplingReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	s.sampler.sample()
	return n, err
}

// settledHeap returns the live heap after a collection, so a growth check starts
// from a settled baseline.
func settledHeap(t *testing.T) uint64 {
	t.Helper()
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.HeapAlloc
}

// assertHeapGrowth requires the sampled peak and the retained heap after a
// collection to both stay inside allowance.
func assertHeapGrowth(t *testing.T, before, peak uint64, allowance int64) {
	t.Helper()
	retained := settledHeap(t)
	if grew := int64(peak) - int64(before); grew >= allowance {
		t.Fatalf("the refusal peaked %d bytes above the baseline, allowance %d", grew, allowance)
	}
	if grew := int64(retained) - int64(before); grew >= allowance {
		t.Fatalf("the refusal retained %d bytes above the baseline, allowance %d", grew, allowance)
	}
}

// shapeValue is a decoded document that decides its own shape.
type shapeValue struct {
	N int `json:"n"`
}

var errShapeIncomplete = errors.New("synthetic incomplete value")

// Validate reports the value incomplete unless N is 1.
func (s *shapeValue) Validate() error {
	if s.N != 1 {
		return errShapeIncomplete
	}
	return nil
}

var errCallback = errors.New("synthetic callback refusal")

// failingReader ends a body with an error instead of EOF.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// repeatReader yields n copies of one byte without holding them.
type repeatReader struct {
	b byte
	n int64
}

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.n == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.n {
		p = p[:r.n]
	}
	for i := range p {
		p[i] = r.b
	}
	r.n -= int64(len(p))
	return len(p), nil
}

// wantReason requires err to be an *Error whose reason is want and returns its
// parameters, so a caller can check the limit and the observation it recorded.
func wantReason(t *testing.T, err error, want string) map[string]string {
	t.Helper()
	var rejected *Error
	if !errors.As(err, &rejected) {
		t.Fatalf("error = %v, want *ingest.Error with reason %s", err, want)
	}
	if rejected.Reason != want {
		t.Fatalf("reason = %s, want %s (%v)", rejected.Reason, want, err)
	}
	return rejected.Params
}

// jsonOfSize returns a JSON document of exactly size bytes.
func jsonOfSize(size int) []byte {
	const head, tail = `{"pad":"`, `"}`
	shift := len(head) + len(tail)
	if size < shift {
		return nil
	}
	return []byte(head + strings.Repeat("x", size-shift) + tail)
}

// nestedJSON returns a document of exactly depth nested arrays.
func nestedJSON(depth int) []byte {
	return []byte(strings.Repeat("[", depth) + strings.Repeat("]", depth))
}

// feedOf returns a metadata feed with n entries.
func feedOf(n int) []byte {
	var document bytes.Buffer
	document.WriteByte('[')
	for i := range n {
		if i > 0 {
			document.WriteByte(',')
		}
		document.WriteString(`{"pad":"x"}`)
	}
	document.WriteByte(']')
	return document.Bytes()
}

// identical returns n copies of one byte.
func identical(n int) []byte { return bytes.Repeat([]byte{'x'}, n) }

// gzipBytes returns the gzip encoding of data at level.
func gzipBytes(t *testing.T, data []byte, level int) []byte {
	t.Helper()
	var out bytes.Buffer
	writer, err := gzip.NewWriterLevel(&out, level)
	if err != nil {
		t.Fatalf("gzip level %d: %v", level, err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// zlibBytes returns the RFC 7230 deflate (zlib) encoding of data.
func zlibBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	writer := zlib.NewWriter(&out)
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

const (
	ndjsonHead = `{"pad":"`
	ndjsonTail = `"}`

	// ndjsonMinLine is the shortest line the helpers build, its newline included.
	ndjsonMinLine = len(ndjsonHead) + len(ndjsonTail) + 1
)

// ndjsonRecord returns one valid JSON record of exactly content bytes, its
// terminator excluded.
func ndjsonRecord(content int) []byte {
	record := make([]byte, 0, content)
	record = append(record, ndjsonHead...)
	record = append(record, bytes.Repeat([]byte{'x'}, content-len(ndjsonHead)-len(ndjsonTail))...)
	return append(record, ndjsonTail...)
}

// ndjsonLine returns a valid JSON line of exactly size bytes, newline included.
func ndjsonLine(size int) []byte { return append(ndjsonRecord(size-1), '\n') }

// datasetGZIP returns a gzip NDJSON generation of exactly decoded bytes at
// level, built from whole JSON lines of record bytes (the last one absorbing a
// short remainder), and its record count.
func datasetGZIP(t *testing.T, decoded int64, record, level int) ([]byte, int) {
	t.Helper()
	if record < ndjsonMinLine || decoded < int64(record) {
		t.Fatalf("generation of %d bytes in %d-byte records", decoded, record)
	}
	whole := decoded / int64(record)
	rest := decoded % int64(record)
	if rest > 0 && rest < int64(ndjsonMinLine) {
		if whole < 2 {
			t.Fatalf("generation of %d bytes cannot end in a line of %d", decoded, rest)
		}
		whole--
		rest += int64(record)
	}
	var out bytes.Buffer
	writer, err := gzip.NewWriterLevel(&out, level)
	if err != nil {
		t.Fatalf("gzip level %d: %v", level, err)
	}
	line := ndjsonLine(record)
	records := 0
	for range whole {
		if _, err := writer.Write(line); err != nil {
			t.Fatal(err)
		}
		records++
	}
	if rest > 0 {
		if _, err := writer.Write(ndjsonLine(int(rest))); err != nil {
			t.Fatal(err)
		}
		records++
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), records
}

// storedGZIP returns a gzip NDJSON generation of exactly size compressed bytes,
// written with stored deflate blocks so the compressed size follows the decoded
// content, and its record count.
func storedGZIP(t *testing.T, size int64) ([]byte, int) {
	t.Helper()
	decoded := size - 18 // gzip header and trailer
	for range 8 {
		compressed, records := datasetGZIP(t, decoded, 32<<10, flate.NoCompression)
		if int64(len(compressed)) == size {
			return compressed, records
		}
		decoded += size - int64(len(compressed))
	}
	t.Fatalf("no %d-byte generation converged", size)
	return nil, 0
}

// pngChunk returns one PNG chunk.
func pngChunk(kind string, data []byte) []byte {
	chunk := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
	chunk = append(chunk, kind...)
	chunk = append(chunk, data...)
	return binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(append([]byte(kind), data...)))
}

// pngImage returns a valid encoding of a width x height gray image.
func pngImage(t *testing.T, width, height int) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, image.NewGray(image.Rect(0, 0, width, height))); err != nil {
		t.Fatalf("encode a %dx%d png: %v", width, height, err)
	}
	return out.Bytes()
}

// pngPadded returns a valid width x height PNG of exactly size bytes: the real
// image plus one ancillary chunk filling the difference before IEND.
func pngPadded(t *testing.T, width, height, size int) []byte {
	t.Helper()
	data := pngImage(t, width, height)
	end := len(data) - 12 // the IEND chunk closes the stream
	pad := size - len(data) - 12
	if pad < 0 {
		t.Fatalf("a %dx%d png is %d bytes, above %d", width, height, len(data), size)
	}
	padded := make([]byte, 0, size)
	padded = append(padded, data[:end]...)
	padded = append(padded, pngChunk("prVt", make([]byte, pad))...)
	return append(padded, data[end:]...)
}

// pngHeaderOnly returns a PNG declaring width x height with no pixel data, so
// the header caps are decided without decoding an image.
func pngHeaderOnly(width, height uint32) []byte {
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], width)
	binary.BigEndian.PutUint32(ihdr[4:], height)
	ihdr[8] = 8 // bit depth
	ihdr[9] = 0 // gray
	data := []byte("\x89PNG\r\n\x1a\n")
	data = append(data, pngChunk("IHDR", ihdr)...)
	return append(data, pngChunk("IEND", nil)...)
}
