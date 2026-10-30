package fixture

import (
	"bytes"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// A Secret is one value a saved fixture must not hold, with the fixed
// placeholder that takes its place. The recorder knows its own secrets; nothing
// derives one from the bytes it reads.
type Secret struct {
	Value       string
	Placeholder string
}

// Scrub rewrites the fields a peer controls: the request URL, every response header
// value and every body. A value is replaced with its placeholder in every spelling it
// can travel in, because a credential that rode in a URL is the same secret as one in
// a body.
func Scrub(doc *Document, secrets []Secret) {
	for i := range doc.Exchanges {
		exchange := &doc.Exchanges[i]
		exchange.Request.URL = scrubText(exchange.Request.URL, secrets)
		if exchange.Response == nil {
			continue
		}
		for name, values := range exchange.Response.Header {
			scrubbed := make([]string, len(values))
			for j, value := range values {
				scrubbed[j] = scrubText(value, secrets)
			}
			exchange.Response.Header[name] = scrubbed
		}
		exchange.Response.Body = []byte(scrubText(string(exchange.Response.Body), secrets))
		// The header described the bytes before the scrub rewrote them, and a client
		// reads it as the body's size. A partial answer keeps the length its peer
		// declared: that mismatch is the fault being recorded.
		if exchange.Fault != FaultPartial {
			declaredLength(exchange.Response)
		}
	}
}

func scrubText(text string, secrets []Secret) string {
	for _, secret := range secrets {
		for _, spelling := range spellings(secret) {
			text = strings.ReplaceAll(text, spelling, secret.Placeholder)
		}
	}
	return text
}

// spellings is every form of a secret the scrub rewrites: the value as written and
// the form it takes percent-encoded inside a query.
func spellings(secret Secret) []string {
	if secret.Value == "" {
		return nil
	}
	if encoded := url.QueryEscape(secret.Value); encoded != secret.Value {
		return []string{secret.Value, encoded}
	}
	return []string{secret.Value}
}

// declaredLength rewrites the Content-Length the peer declared to the length of the
// body that is saved, and leaves a header that declares none alone.
func declaredLength(response *Response) {
	for name := range response.Header {
		if strings.EqualFold(name, "Content-Length") {
			response.Header[name] = []string{strconv.Itoa(len(response.Body))}
			return
		}
	}
}

// Check re-reads the encoded document and refuses it while any secret value
// survives, in every spelling the scrub rewrites. The rules rewrite the fields they
// know, so the check is what catches a value in a field they do not (a header name,
// say): a recording that fails it is never published. A saved body is base64, so its
// bytes are read back out of the file rather than trusted as text.
func Check(data []byte, secrets []Secret) error {
	doc, err := Decode(data)
	if err != nil {
		return err
	}
	for _, secret := range secrets {
		for _, spelling := range spellings(secret) {
			value := []byte(spelling)
			if bytes.Contains(data, value) || bodiesHold(doc, value) {
				return fmt.Errorf("%w: %s", ErrScrubIncomplete, secret.Placeholder)
			}
		}
	}
	return nil
}

// bodiesHold reports whether a recorded body holds value.
func bodiesHold(doc Document, value []byte) bool {
	for _, exchange := range doc.Exchanges {
		if exchange.Response != nil && bytes.Contains(exchange.Response.Body, value) {
			return true
		}
	}
	return false
}
