package ingest

import (
	"compress/gzip"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Body reads a body through its Content-Encoding and returns its decoded bytes,
// refusing more than max decoded bytes and never more than maxBodyBytes, the
// class cap: a larger caller bound is clamped, a smaller one is honoured. It
// takes no declared length, so a transport's Content-Length cannot widen the
// read. identity, gzip and deflate (zlib, RFC 7230) are supported; any other
// encoding is refused instead of being returned undecoded, a compressed body
// that ends early is body_partial rather than a shorter success, and a bound
// below one is a caller error.
func Body(r io.Reader, enc string, max int64) ([]byte, error) {
	if max < 1 {
		return nil, fmt.Errorf("ingest: the body bound %d is not positive", max)
	}
	if max > maxBodyBytes {
		max = maxBodyBytes
	}
	encoding := strings.ToLower(strings.TrimSpace(enc))
	decoded, closer, err := bodyReader(r, encoding)
	if err != nil {
		return nil, err
	}
	if closer != nil {
		defer func() { _ = closer.Close() }()
	}
	data, err := readLimited(decoded, max, reasonBodyTooLarge)
	if err != nil {
		var refused *Error
		if errors.As(err, &refused) {
			return nil, err
		}
		return nil, &Error{Reason: reasonBodyPartial, Params: map[string]string{"encoding": encoding}, cause: err}
	}
	return data, nil
}

// bodyReader returns r under its Content-Encoding and the decoder to close.
func bodyReader(r io.Reader, encoding string) (io.Reader, io.Closer, error) {
	switch encoding {
	case "", "identity":
		return r, nil, nil
	case "gzip":
		decoded, err := gzip.NewReader(r)
		if err != nil {
			return nil, nil, &Error{Reason: reasonBodyPartial, Params: map[string]string{"encoding": encoding}, cause: err}
		}
		return decoded, decoded, nil
	case "deflate":
		decoded, err := zlib.NewReader(r)
		if err != nil {
			return nil, nil, &Error{Reason: reasonBodyPartial, Params: map[string]string{"encoding": encoding}, cause: err}
		}
		return decoded, decoded, nil
	}
	return nil, nil, &Error{Reason: reasonBodyEncodingUnsupported, Params: map[string]string{"encoding": encoding}}
}
