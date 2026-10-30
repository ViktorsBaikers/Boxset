package ingest

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

// Dataset decodes one gzip NDJSON generation and calls record once per record
// with its raw JSON. Every cap is enforced while reading: at most
// maxDatasetCompressedBytes of compressed input, maxDatasetDecodedBytes decoded,
// maxDatasetRecords records and maxRecordBytes per record. The walk stops at the
// first crossed cap, so a bomb is refused at the cap instead of being decoded
// whole. An error from record ends the walk and is returned unchanged: a refused
// import is never reported as a successful count.
func Dataset(r io.Reader, record func(json.RawMessage) error) (int, error) {
	compressed := &countingReader{r: r}
	decoded, err := gzip.NewReader(io.LimitReader(compressed, maxDatasetCompressedBytes+1))
	if err != nil {
		if refused := datasetCaps(compressed, nil); refused != nil {
			return 0, refused
		}
		return 0, datasetMalformed(0, err)
	}
	defer func() { _ = decoded.Close() }()

	stream := &countingReader{r: decoded}
	// The token bound carries the record cap plus its longest terminator: a record
	// of exactly maxRecordBytes ending in CRLF must still fit, and anything longer
	// is refused here rather than by the scanner.
	lines := bufio.NewScanner(io.LimitReader(stream, maxDatasetDecodedBytes+1))
	lines.Buffer(make([]byte, 0, readChunk), maxRecordBytes+2)

	records := 0
	for lines.Scan() {
		if refused := datasetCaps(compressed, stream); refused != nil {
			return 0, refused
		}
		line := lines.Bytes()
		if len(line) > maxRecordBytes {
			return 0, datasetRecordTooLarge(records + 1)
		}
		if len(line) == 0 || !json.Valid(line) {
			return 0, datasetMalformed(records+1, nil)
		}
		if !utf8.Valid(line) {
			return 0, &Error{Reason: reasonUTF8Invalid, Params: map[string]string{
				"record": count(int64(records) + 1),
				"offset": count(int64(firstInvalidUTF8(line))),
			}}
		}
		records++
		if records > maxDatasetRecords {
			return 0, &Error{Reason: reasonDatasetRecordsExceeded, Params: map[string]string{
				"limit_records":    count(maxDatasetRecords),
				"observed_records": count(int64(records)),
			}}
		}
		// The scanner reuses its buffer, so each record is handed over as its own
		// copy: a callback may retain what it is given.
		if err := record(append(json.RawMessage(nil), line...)); err != nil {
			return 0, err
		}
	}
	if err := lines.Err(); err != nil {
		if refused := datasetCaps(compressed, stream); refused != nil {
			return 0, refused
		}
		if errors.Is(err, bufio.ErrTooLong) {
			return 0, datasetRecordTooLarge(records + 1)
		}
		return 0, datasetMalformed(records+1, err)
	}
	if refused := datasetCaps(compressed, stream); refused != nil {
		return 0, refused
	}
	return records, nil
}

// datasetCaps reports the first dataset byte cap the walk has crossed, the
// compressed cap first because it is decided on the bytes actually read.
func datasetCaps(compressed, decoded *countingReader) *Error {
	if compressed.n > maxDatasetCompressedBytes {
		return &Error{Reason: reasonDatasetCompressedTooLarge, Params: map[string]string{
			"limit_bytes":      count(maxDatasetCompressedBytes),
			"compressed_bytes": count(compressed.n),
		}}
	}
	if decoded != nil && decoded.n > maxDatasetDecodedBytes {
		return &Error{Reason: reasonDatasetDecodedTooLarge, Params: map[string]string{
			"limit_bytes":   count(maxDatasetDecodedBytes),
			"decoded_bytes": count(decoded.n),
		}}
	}
	return nil
}

// datasetMalformed returns the dataset_malformed refusal for a record that is
// not one JSON value, or for a generation whose stream ended early.
func datasetMalformed(record int, err error) *Error {
	return &Error{Reason: reasonDatasetMalformed, Params: map[string]string{
		"record": count(int64(record)),
	}, cause: err}
}

// datasetRecordTooLarge returns the dataset_record_too_large refusal for one
// record past its byte cap.
func datasetRecordTooLarge(record int) *Error {
	return &Error{Reason: reasonDatasetRecordTooLarge, Params: map[string]string{
		"limit_bytes": count(maxRecordBytes),
		"record":      count(int64(record)),
	}}
}

// countingReader counts the bytes it has passed through, so a cap is decided on
// bytes already consumed even when a decoder stops on an error.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
