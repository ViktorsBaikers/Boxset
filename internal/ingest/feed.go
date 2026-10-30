package ingest

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

// Feed counts the entries of one metadata feed: a single JSON array whose
// elements are the entries. The array is walked without decoding its entries, so
// the maxFeedEntries+1st entry is refused before the rest of the feed is read,
// and a document that is not one complete array, trailing data included, is
// feed_malformed.
func Feed(data []byte) (int, error) {
	if !utf8.Valid(data) {
		return 0, invalidUTF8(data, 0)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	first, err := decoder.Token()
	if err != nil {
		return 0, feedMalformed(decoder, err)
	}
	if first != json.Delim('[') {
		return 0, feedMalformed(decoder, nil)
	}
	entries := 0
	for decoder.More() {
		var entry json.RawMessage
		if err := decoder.Decode(&entry); err != nil {
			return 0, feedMalformed(decoder, err)
		}
		entries++
		if entries > maxFeedEntries {
			return 0, &Error{Reason: reasonFeedEntriesExceeded, Params: map[string]string{
				"limit_entries":    count(maxFeedEntries),
				"observed_entries": count(int64(entries)),
			}}
		}
	}
	if _, err := decoder.Token(); err != nil {
		return 0, feedMalformed(decoder, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return 0, feedMalformed(decoder, nil)
	}
	return entries, nil
}

// feedMalformed returns the feed_malformed refusal for a document that is not
// one complete array, naming the byte offset the decoder reached.
func feedMalformed(decoder *json.Decoder, err error) *Error {
	return &Error{Reason: reasonFeedMalformed, Params: map[string]string{
		"offset": count(decoder.InputOffset()),
	}, cause: err}
}
