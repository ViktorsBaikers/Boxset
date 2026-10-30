package ingest

import (
	"encoding/json"
	"errors"
	"unicode/utf8"
)

// JSON decodes one JSON document under the native request limits: at most
// MaxJSONBytes, at most maxJSONDepth levels of nesting, valid UTF-8 and no
// trailing data. encoding/json replaces invalid UTF-8 silently and its decoder
// has no depth limit, so both are decided here before the document is decoded.
// A value implementing Validate decides its own shape; a refusal there is
// json_invalid_shape with the validator's error reachable through Unwrap.
func JSON(data []byte, v any) error {
	if int64(len(data)) > MaxJSONBytes {
		return &Error{Reason: reasonJSONTooLarge, Params: map[string]string{
			"limit_bytes":    count(MaxJSONBytes),
			"observed_bytes": count(int64(len(data))),
		}}
	}
	if !utf8.Valid(data) {
		return invalidUTF8(data, 0)
	}
	if depth := jsonDepth(data); depth > maxJSONDepth {
		return &Error{Reason: reasonJSONDepthExceeded, Params: map[string]string{
			"limit_depth":    count(maxJSONDepth),
			"observed_depth": count(int64(depth)),
		}}
	}
	if err := json.Unmarshal(data, v); err != nil {
		return malformed(err, 0)
	}
	if validator, ok := v.(interface{ Validate() error }); ok {
		if err := validator.Validate(); err != nil {
			return &Error{Reason: reasonJSONInvalidShape, cause: err}
		}
	}
	return nil
}

// jsonDepth reports the deepest object or array nesting in data. Brackets
// inside strings, escaped or not, are text and do not nest.
func jsonDepth(data []byte) int {
	depth, deepest := 0, 0
	inString, escaped := false, false
	for _, b := range data {
		if inString {
			switch {
			case escaped:
				escaped = false
			case b == '\\':
				escaped = true
			case b == '"':
				inString = false
			}
			continue
		}
		switch b {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > deepest {
				deepest = depth
			}
		case '}', ']':
			if depth > 0 {
				depth--
			}
		}
	}
	return deepest
}

// invalidUTF8 returns the utf8_invalid refusal for data at the given offset.
func invalidUTF8(data []byte, offset int) *Error {
	return &Error{Reason: reasonUTF8Invalid, Params: map[string]string{
		"offset": count(int64(offset + firstInvalidUTF8(data))),
	}}
}

// malformed returns the json_malformed refusal for a decoding error, naming the
// byte offset when the decoder reported one.
func malformed(err error, offset int) *Error {
	params := map[string]string{"offset": count(int64(offset))}
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		params["offset"] = count(int64(offset) + syntax.Offset)
	}
	return &Error{Reason: reasonJSONMalformed, Params: params, cause: err}
}
