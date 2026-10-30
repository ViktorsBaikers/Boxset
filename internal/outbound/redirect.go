package outbound

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/ViktorsBaikers/Boxset/internal/redact"
)

// noRedirect stops the transport-level client from following a redirect: every hop
// is decided here instead, so one response is one wire request and no hop escapes
// validation.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// isRedirect reports a status that asks for another request.
func isRedirect(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// nextHop validates a redirect target before it is requested. A hop is a new
// destination: the dial resolves and validates it again, and the credential is
// attached only when the hop's origin is the approved one. A target that carries a
// credential in its query — one of the names the redactor refuses or the one this
// op attaches — that downgrades https, or that is not http at all is refused. The
// refusal names the target's host, never its URL.
func nextHop(from *url.URL, header http.Header, sent int, credentialQuery string) (*url.URL, *Error) {
	location := header.Get("Location")
	if location == "" {
		return nil, denyHop(canonicalHost(from.Hostname()), sent)
	}
	next, err := from.Parse(location)
	if err != nil {
		return nil, denyHop(canonicalHost(from.Hostname()), sent)
	}
	scheme := strings.ToLower(next.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, denyHop(canonicalHost(next.Hostname()), sent)
	}
	// The previous hop's own scheme is compared canonically: a caller-built URL
	// may spell it in any case and is still the TLS origin the hop must not leave.
	if strings.EqualFold(from.Scheme, "https") && scheme != "https" {
		return nil, denyHop(canonicalHost(next.Hostname()), sent)
	}
	// A credential this client did not attach is not carried onward: a target that
	// already names one in its query is refused rather than followed.
	if queryNamesCredential(next.RawQuery, credentialQuery) {
		return nil, denyHop(canonicalHost(next.Hostname()), sent)
	}
	next.Scheme = scheme
	return next, nil
}

// queryNamesCredential reports whether raw names a credential: one of the names
// the redactor refuses, or the query name of the credential this op attaches,
// which is the peer's own convention and so cannot be known to the redactor.
func queryNamesCredential(raw, credentialQuery string) bool {
	if redact.QueryHasSecret(raw) {
		return true
	}
	if credentialQuery == "" {
		return false
	}
	for part := range strings.SplitSeq(raw, "&") {
		name, _, _ := strings.Cut(part, "=")
		if decoded, err := url.QueryUnescape(name); err == nil {
			name = decoded
		}
		if strings.EqualFold(strings.TrimSpace(name), credentialQuery) {
			return true
		}
	}
	return false
}

// denyHop is the refusal of one redirect target.
func denyHop(host string, sent int) *Error {
	return &Error{Reason: ReasonRedirectDenied, Params: map[string]string{"host": host}, Attempts: sent}
}
