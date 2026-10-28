package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// maxBaseLen bounds BOXSET_URL_BASE; a longer value is a configuration slip, not a deployment.
const maxBaseLen = 128

// normalizeBase validates BOXSET_URL_BASE and returns the form the server uses: "" for the
// root, otherwise "/seg[/seg...]" without a trailing slash. A base must start with "/" and
// every segment is non-empty and made of RFC 3986 unreserved characters, so one value is
// safe inside a URL, an HTML attribute, a JavaScript string and a CSS url().
func normalizeBase(raw string) (string, error) {
	if len(raw) > maxBaseLen {
		return "", fmt.Errorf("%w: longer than %d bytes", ErrBaseInvalid, maxBaseLen)
	}
	if raw == "" || raw == "/" {
		return "", nil
	}
	if raw[0] != '/' {
		return "", fmt.Errorf("%w: %q must start with /", ErrBaseInvalid, raw)
	}
	candidate := strings.TrimRight(raw, "/")
	if candidate == "" {
		return "", fmt.Errorf("%w: %q has an empty path segment", ErrBaseInvalid, raw)
	}
	for seg := range strings.SplitSeq(candidate[1:], "/") {
		switch seg {
		case "":
			return "", fmt.Errorf("%w: %q has an empty path segment", ErrBaseInvalid, raw)
		case ".", "..":
			return "", fmt.Errorf("%w: %q has a dot segment", ErrBaseInvalid, raw)
		}
		for i := range len(seg) {
			if !unreserved(seg[i]) {
				return "", fmt.Errorf("%w: %q may use only letters, digits and - . _ ~ between slashes", ErrBaseInvalid, raw)
			}
		}
	}
	return candidate, nil
}

func unreserved(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '-' || c == '.' || c == '_' || c == '~'
}

// stripBase serves h for requests under base with the base removed from their path. The
// escaped path is judged before any decoding: an encoded slash or backslash and a dot
// segment are rejected (400), a path outside the base or with an empty segment is 404, so
// nothing but the literal base prefix reaches the router and no cleaned-up redirect can
// turn an odd path into a listed one.
func stripBase(base string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner, status := belowBase(base, r.URL.EscapedPath())
		if status != 0 {
			http.Error(w, http.StatusText(status), status)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path, r2.URL.RawPath = inner, ""
		h.ServeHTTP(w, r2)
	})
}

// belowBase returns the decoded path below base for an escaped request path, or the HTTP
// status that refuses it.
func belowBase(base, escaped string) (string, int) {
	lower := strings.ToLower(escaped)
	if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") || strings.Contains(escaped, `\`) {
		return "", http.StatusBadRequest
	}
	below := escaped
	if base != "" {
		switch {
		case escaped == base:
			below = "/"
		case strings.HasPrefix(escaped, base+"/"):
			below = escaped[len(base):]
		default:
			return "", http.StatusNotFound
		}
	}
	decoded, err := url.PathUnescape(below)
	if err != nil || strings.ContainsRune(decoded, 0) || strings.Contains(decoded, `\`) {
		return "", http.StatusBadRequest
	}
	for seg := range strings.SplitSeq(decoded, "/") {
		if seg == "." || seg == ".." {
			return "", http.StatusBadRequest
		}
	}
	if decoded == "" || strings.Contains(decoded, "//") {
		return "", http.StatusNotFound
	}
	return decoded, 0
}
