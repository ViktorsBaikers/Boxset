// Package ingest decodes untrusted input under the fixed ADR 0004 §6 limits.
// Every cap is enforced while reading, so an oversized or hostile input is
// refused with a structured reason instead of being buffered whole, truncated
// or reported as a partial success.
package ingest

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The initial ADR 0004 §6 limits. mib is a binary megabyte, the unit those rows
// are written in, and every cap is the admitted maximum: cap+1 is refused.
const (
	mib = 1 << 20

	// MaxJSONBytes bounds an ordinary native JSON request. It is exported for
	// the HTTP boundary, which mirrors the same cap onto the transport.
	MaxJSONBytes = 1 * mib
	// maxJSONDepth bounds nesting, the deepest admitted document holding
	// maxJSONDepth objects or arrays.
	maxJSONDepth = 64
	// maxBodyBytes bounds a decoded body: identity, gzip or deflate.
	maxBodyBytes = 16 * mib
	// maxFeedEntries bounds the entries of one metadata feed.
	maxFeedEntries = 10_000
	// maxDatasetCompressedBytes bounds a gzip NDJSON generation on disk.
	maxDatasetCompressedBytes = 64 * mib
	// maxDatasetDecodedBytes bounds the same generation after decompression.
	maxDatasetDecodedBytes = 256 * mib
	// maxDatasetRecords bounds the records of one generation.
	maxDatasetRecords = 1_000_000
	// maxRecordBytes bounds one record, its newline excluded.
	maxRecordBytes = 64 << 10
	// maxImageBytes bounds an encoded image.
	maxImageBytes = 10 * mib
	// maxImageSide bounds each decoded image dimension.
	maxImageSide = 8_192
	// maxImagePixels bounds the decoded image area.
	maxImagePixels = 16_000_000

	// readChunk is the first buffer a bounded read asks for: an ordinary body
	// costs one chunk, and only an input past it pays for the whole limit.
	readChunk = 32 << 10
	// maxEmptyReads bounds a reader that keeps returning no bytes and no error,
	// so a stalled input is refused instead of spun on.
	maxEmptyReads = 100
)

// Reason tokens. Each names one class of refused input; the reason and its
// parameters are the stable contract callers switch on.
const (
	reasonJSONTooLarge            = "json_too_large"
	reasonJSONDepthExceeded       = "json_depth_exceeded"
	reasonJSONMalformed           = "json_malformed"
	reasonUTF8Invalid             = "utf8_invalid"
	reasonJSONInvalidShape        = "json_invalid_shape"
	reasonBodyTooLarge            = "body_too_large"
	reasonBodyPartial             = "body_partial"
	reasonBodyEncodingUnsupported = "body_encoding_unsupported"

	reasonFeedEntriesExceeded = "feed_entries_exceeded"
	reasonFeedMalformed       = "feed_malformed"

	reasonDatasetCompressedTooLarge = "dataset_compressed_too_large"
	reasonDatasetDecodedTooLarge    = "dataset_decoded_too_large"
	reasonDatasetRecordsExceeded    = "dataset_records_exceeded"
	reasonDatasetRecordTooLarge     = "dataset_record_too_large"
	reasonDatasetMalformed          = "dataset_malformed"

	reasonImageTooLarge          = "image_too_large"
	reasonImageDimensionsInvalid = "image_dimensions_invalid"
	reasonImagePixelsExceeded    = "image_pixels_exceeded"
	reasonImageMalformed         = "image_malformed"
)

// Error is a refused input. Reason is one of the tokens above and Params holds
// the limit and the observation that decided it, never input bytes or
// credential material. cause keeps the decoder or validator error reachable for
// errors.Is and errors.As without rendering it into the message.
type Error struct {
	Reason string
	Params map[string]string

	cause error
}

// Error renders the reason and its parameters in a stable order.
func (e *Error) Error() string {
	var message strings.Builder
	message.WriteString("ingest: ")
	message.WriteString(e.Reason)
	keys := make([]string, 0, len(e.Params))
	for key := range e.Params {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		fmt.Fprintf(&message, " %s=%s", key, e.Params[key])
	}
	return message.String()
}

// Unwrap returns the decoder or validator error behind a refusal, if any.
func (e *Error) Unwrap() error { return e.cause }

// count renders a count as a parameter value.
func count(n int64) string { return strconv.FormatInt(n, 10) }

// readLimited reads r to its end and refuses more than max bytes. It grows once
// from one chunk to the whole limit, so a refused input costs a single max+1
// buffer instead of a doubling series that would overshoot the cap. A read error
// is returned as it came; a refusal is reason's *Error. A bound below one is a
// caller error, never a stream of zero length.
func readLimited(r io.Reader, max int64, reason string) ([]byte, error) {
	if max < 1 {
		return nil, fmt.Errorf("ingest: the byte bound %d is not positive", max)
	}
	limit := max + 1
	buffer := make([]byte, 0, min(limit, int64(readChunk)))
	empty := 0
	for {
		if len(buffer) == cap(buffer) {
			grown := make([]byte, len(buffer), limit)
			copy(grown, buffer)
			buffer = grown
		}
		n, err := r.Read(buffer[len(buffer):cap(buffer)])
		buffer = buffer[:len(buffer)+n]
		if int64(len(buffer)) > max {
			return nil, &Error{Reason: reason, Params: map[string]string{
				"limit_bytes":    count(max),
				"observed_bytes": count(int64(len(buffer))),
			}}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return buffer, nil
			}
			return nil, err
		}
		if n == 0 {
			empty++
			if empty >= maxEmptyReads {
				return nil, io.ErrNoProgress
			}
			continue
		}
		empty = 0
	}
}

// firstInvalidUTF8 returns the byte offset of the first invalid UTF-8 sequence,
// or -1 for valid input.
func firstInvalidUTF8(data []byte) int {
	for offset := 0; offset < len(data); {
		r, size := utf8.DecodeRune(data[offset:])
		if r == utf8.RuneError && size == 1 {
			return offset
		}
		offset += size
	}
	return -1
}
