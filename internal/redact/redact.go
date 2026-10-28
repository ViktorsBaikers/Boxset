// Package redact is the shared redactor. Every string that can carry a URL, a
// credential or an error from the network passes through String before it is
// logged, stored or shown; NewHandler applies the same rule to every slog
// record. Masking is deliberately broad: a value is lost from a log line
// rather than a secret kept.
package redact

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
)

// Marker replaces every masked value.
const Marker = "[REDACTED]"

// queryNames are the query parameter names that carry a credential. A request
// that puts one in its URL is refused (QueryHasSecret); everywhere else its
// value is masked.
var queryNames = []string{"apikey", "api_key", "token", "access_token", "password"}

var (
	// Match names before consuming values: a nonsecret pair must not hide a
	// later credential in the same text. Decode names before classification.
	pairRE = regexp.MustCompile(`(?i)([a-z0-9_.%+-]+?)(=|%3d)`)
	// "name": "value" in JSON-shaped text.
	jsonRE = regexp.MustCompile(`(?i)("[a-z0-9_.-]*(?:apikey|api_key|api-key|token|password|passwd|secret|authorization|cookie)"\s*:\s*)"(?:[^"\\]|\\.)*"`)
	keyRE  = regexp.MustCompile(`\bbxk_[A-Za-z0-9_-]+`)
	// Authorization schemes and the API key header written as text.
	authRE = regexp.MustCompile(`(?i)\b(bearer|basic|x-api-key:?)(\s+)([^\s"',;]+)`)
	// Userinfo can itself be a credential, even without a password separator.
	userinfoRE = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)([^/\s@]+)@`)
)

// String masks every credential String can recognize in s: secret query or
// form values, JSON members, authorization values and URL credentials.
func String(s string) string {
	s = userinfoRE.ReplaceAllString(s, "${1}"+Marker+"@")
	var out strings.Builder
	end := 0
	for _, pair := range pairRE.FindAllStringSubmatchIndex(s, -1) {
		if pair[0] < end {
			continue // already inside a masked value
		}
		name, err := url.QueryUnescape(s[pair[2]:pair[3]])
		if err == nil && !SecretKey(name) {
			continue
		}
		out.WriteString(s[end:pair[1]])
		out.WriteString(Marker)
		// Raw query values may contain quotes or whitespace. Only actual
		// query/fragment delimiters end them; ambiguous text is masked too.
		end = pair[1]
		if n := strings.IndexAny(s[end:], "&#"); n >= 0 {
			end += n
		} else {
			end = len(s)
		}
	}
	if end != 0 {
		out.WriteString(s[end:])
		s = out.String()
	}
	s = jsonRE.ReplaceAllString(s, `${1}"`+Marker+`"`)
	s = authRE.ReplaceAllString(s, "${1}${2}"+Marker)
	return keyRE.ReplaceAllString(s, Marker)
}

// URL is the redacted form of u.
func URL(u *url.URL) string {
	if u == nil {
		return ""
	}
	return String(u.String())
}

// QueryHasSecret reports whether a raw query string names a credential
// parameter (apikey, api_key, token, access_token or password, in any letter
// case). Such a request is refused before routing: a credential never travels
// in a URL.
func QueryHasSecret(rawQuery string) bool {
	for part := range strings.SplitSeq(rawQuery, "&") {
		name, _, _ := strings.Cut(part, "=")
		if decoded, err := url.QueryUnescape(name); err == nil {
			name = decoded
		}
		for _, q := range queryNames {
			if strings.EqualFold(strings.TrimSpace(name), q) {
				return true
			}
		}
	}
	return false
}

// SecretKey reports whether an attribute or field name holds a credential:
// its value is masked whole, whatever it looks like.
func SecretKey(key string) bool {
	k := strings.ToLower(key)
	for _, w := range []string{"password", "passwd", "token", "secret", "apikey", "api_key", "api-key", "authorization", "cookie"} {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}

// NewHandler wraps next so every record it receives has its message and
// attribute values redacted: an attribute whose key is a credential name is
// masked whole, every other value is passed through String after it is
// resolved. Errors and Stringers use redacted text; opaque values are masked whole.
func NewHandler(next slog.Handler) slog.Handler { return &handler{next: next} }

type handler struct{ next slog.Handler }

func (h *handler) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, String(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(attr(a))
		return true
	})
	return h.next.Handle(ctx, out)
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		clean[i] = attr(a)
	}
	return &handler{next: h.next.WithAttrs(clean)}
}

func (h *handler) WithGroup(name string) slog.Handler {
	return &handler{next: h.next.WithGroup(name)}
}

// attr is a with its value redacted.
func attr(a slog.Attr) slog.Attr {
	if SecretKey(a.Key) {
		return slog.String(a.Key, Marker)
	}
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, String(v.String()))
	case slog.KindGroup:
		group := v.Group()
		clean := make([]any, len(group))
		for i, g := range group {
			clean[i] = attr(g)
		}
		return slog.Group(a.Key, clean...)
	case slog.KindAny:
		switch value := v.Any().(type) {
		case error:
			return slog.String(a.Key, String(value.Error()))
		case fmt.Stringer:
			return slog.String(a.Key, String(value.String()))
		default:
			return slog.String(a.Key, Marker)
		}
	default:
		return slog.Attr{Key: a.Key, Value: v}
	}
}
