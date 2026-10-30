package outbound

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/ViktorsBaikers/Boxset/internal/redact"
)

// TestRedirectHopsAreValidated proves AC-031's redirect clauses: a hop is a new
// destination, so it is resolved and validated again, a downgrade or a target that
// carries a credential in its query is refused before it is requested, a hop past
// the wire bound is refused, and a hop to a private address that is not the approved
// origin never reaches its listener.
func TestRedirectHopsAreValidated(t *testing.T) {
	t.Run("a relative location is followed as one more wire request", func(t *testing.T) {
		var served int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			served++
			if r.URL.Path == "/start" {
				http.Redirect(w, r, "/next", http.StatusTemporaryRedirect)
				return
			}
			fmt.Fprint(w, "moved")
		}))
		defer srv.Close()
		client, slot := newTestClient(loopbackResolver(), nil)

		response, err := client.Do(context.Background(), Op{
			Target: approvedTarget(t, srv),
			Method: http.MethodGet,
			URL:    mustURL(t, srv.URL+"/start"),
			Safe:   true,
		}, nil)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		if response.Body != "moved" {
			t.Fatalf("body = %q, want moved", response.Body)
		}
		if served != 2 {
			t.Fatalf("server served %d requests, want the request and its hop", served)
		}
		if taken, released := slot.counts(); taken != 2 || released != 2 {
			t.Fatalf("slot taken/released = %d/%d, want one permit per hop", taken, released)
		}
	})

	t.Run("a downgrade from https is refused", func(t *testing.T) {
		var plain int
		insecure := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { plain++ }))
		defer insecure.Close()
		secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, insecure.URL+"/next", http.StatusFound)
		}))
		defer secure.Close()
		secure.Config.ErrorLog = quietLog

		port, err := strconv.Atoi(mustURL(t, secure.URL).Port())
		if err != nil {
			t.Fatalf("secure port: %v", err)
		}
		resolver := newResolver(map[string][]netip.Addr{"example.com": addrs("127.0.0.1")})
		client, _ := newTestClient(resolver, trustServer(secure))
		target := &Target{Origin: Origin{Scheme: "https", Host: "example.com", Port: port}, Private: true}

		_, err = client.Do(context.Background(), Op{
			Target: target,
			Method: http.MethodGet,
			URL:    mustURL(t, "https://example.com:"+strconv.Itoa(port)+"/start"),
			Safe:   true,
		}, nil)
		wantError(t, err, ReasonRedirectDenied)
		if plain != 0 {
			t.Fatalf("the downgraded origin served %d requests, want none", plain)
		}
	})

	t.Run("a hand-built uppercase scheme cannot downgrade", func(t *testing.T) {
		var plain int
		insecure := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { plain++ }))
		defer insecure.Close()
		secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, insecure.URL+"/next", http.StatusFound)
		}))
		defer secure.Close()
		secure.Config.ErrorLog = quietLog

		port, err := strconv.Atoi(mustURL(t, secure.URL).Port())
		if err != nil {
			t.Fatalf("secure port: %v", err)
		}
		resolver := newResolver(map[string][]netip.Addr{"example.com": addrs("127.0.0.1")})
		client, _ := newTestClient(resolver, trustServer(secure))
		target := &Target{Origin: Origin{Scheme: "https", Host: "example.com", Port: port}, Private: true}

		_, err = client.Do(context.Background(), Op{
			Target: target,
			Method: http.MethodGet,
			// The scheme is written by hand, not parsed: url.Parse would lower it,
			// and the origin comparisons already treat it canonically.
			URL:  &url.URL{Scheme: "HTTPS", Host: "example.com:" + strconv.Itoa(port), Path: "/start"},
			Safe: true,
		}, nil)
		wantError(t, err, ReasonRedirectDenied)
		if plain != 0 {
			t.Fatalf("the downgraded origin served %d requests, want none", plain)
		}
	})

	t.Run("a credential in the target's query is refused", func(t *testing.T) {
		const sentinel = "bxk_location0123456789"
		var served int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			served++
			// The sentinel travels in the target's host as well as its query: the
			// refusal names that host, so a refusal that rendered it verbatim would
			// carry the credential's own text into a log.
			http.Redirect(w, r, "http://"+sentinel+".hop.test/next?apikey="+sentinel, http.StatusFound)
		}))
		defer srv.Close()
		client, _ := newTestClient(loopbackResolver(), nil)

		_, err := client.Do(context.Background(), Op{
			Target: approvedTarget(t, srv),
			Method: http.MethodGet,
			URL:    mustURL(t, srv.URL+"/start"),
			Safe:   true,
		}, nil)
		refused := wantError(t, err, ReasonRedirectDenied)
		if served != 1 {
			t.Fatalf("server served %d requests, want one: the hop was refused", served)
		}
		if refused.Params["host"] != sentinel+".hop.test" {
			t.Fatalf("params = %v, want the refused target's host named", refused.Params)
		}
		rendered := refused.Error()
		if strings.Contains(rendered, sentinel) {
			t.Fatalf("rendered refusal = %q, want no target credential", rendered)
		}
		if !strings.Contains(rendered, redact.Marker) {
			t.Fatalf("rendered refusal = %q, want a redaction marker", rendered)
		}
	})

	t.Run("a credential the target itself names in its query is refused", func(t *testing.T) {
		const secret = "bxk_signed0123456789"
		var served int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			served++
			// "sig" is this peer's own convention, a name the redactor cannot know:
			// the hop names the credential this target attaches.
			http.Redirect(w, r, "/next?sig="+secret, http.StatusFound)
		}))
		defer srv.Close()
		client, _ := newTestClient(loopbackResolver(), nil)
		target := approvedTarget(t, srv)
		target.Credential = &Credential{
			Header: "Authorization",
			Query:  "sig",
			Value:  func(context.Context) ([]byte, error) { return []byte(secret), nil },
		}

		_, err := client.Do(context.Background(), Op{
			Target: target,
			Method: http.MethodGet,
			URL:    mustURL(t, srv.URL+"/start"),
			Safe:   true,
		}, nil)
		refused := wantError(t, err, ReasonRedirectDenied)
		if served != 1 {
			t.Fatalf("server served %d requests, want one: the hop was refused", served)
		}
		if refused.Attempts != 1 {
			t.Fatalf("attempts = %d, want 1", refused.Attempts)
		}
		if strings.Contains(refused.Error(), secret) {
			t.Fatalf("rendered refusal = %q, want no credential", refused.Error())
		}
	})

	t.Run("a hop's userinfo is never sent", func(t *testing.T) {
		const sentinel = "bxk_hopuserinfo0123456789"
		var served int
		var authorization string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			served++
			if r.URL.Path == "/start" {
				http.Redirect(w, r, "http://"+sentinel+":"+sentinel+"@"+r.Host+"/next", http.StatusFound)
				return
			}
			authorization = r.Header.Get("Authorization")
			fmt.Fprint(w, "ok")
		}))
		defer srv.Close()
		client, _ := newTestClient(loopbackResolver(), nil)

		if _, err := client.Do(context.Background(), Op{
			Target: approvedTarget(t, srv),
			Method: http.MethodGet,
			URL:    mustURL(t, srv.URL+"/start"),
			Safe:   true,
		}, nil); err != nil {
			t.Fatalf("Do: %v", err)
		}
		if served != 2 {
			t.Fatalf("server served %d requests, want the request and its hop", served)
		}
		if authorization != "" {
			t.Fatalf("hop authorization = %q, want none: a Location's userinfo is not a scoped credential", authorization)
		}
	})

	t.Run("a hop past the wire bound is refused", func(t *testing.T) {
		var served int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			served++
			http.Redirect(w, r, "/"+strconv.Itoa(served), http.StatusFound)
		}))
		defer srv.Close()
		client, _ := newTestClient(loopbackResolver(), nil)

		_, err := client.Do(context.Background(), Op{
			Target: approvedTarget(t, srv),
			Method: http.MethodGet,
			URL:    mustURL(t, srv.URL+"/0"),
			Safe:   true,
		}, nil)
		refused := wantError(t, err, ReasonBudgetExhausted)
		if served != maxWireRequests {
			t.Fatalf("server served %d requests, want %d", served, maxWireRequests)
		}
		if refused.Attempts != maxWireRequests {
			t.Fatalf("attempts = %d, want %d", refused.Attempts, maxWireRequests)
		}
	})

	t.Run("a hop to an unapproved private origin never connects", func(t *testing.T) {
		var hop, approved int
		unapproved := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hop++ }))
		defer unapproved.Close()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			approved++
			http.Redirect(w, r, unapproved.URL+"/next", http.StatusFound)
		}))
		defer srv.Close()
		client, _ := newTestClient(loopbackResolver(), nil)

		_, err := client.Do(context.Background(), Op{
			Target: approvedTarget(t, srv),
			Method: http.MethodGet,
			URL:    mustURL(t, srv.URL+"/start"),
			Safe:   true,
		}, nil)
		refused := wantError(t, err, ReasonDialDenied)
		if refused.Attempts != 1 {
			t.Fatalf("attempts = %d, want 1: the request was sent before the hop was refused", refused.Attempts)
		}
		if approved != 1 {
			t.Fatalf("the approved origin served %d requests, want 1", approved)
		}
		if hop != 0 {
			t.Fatalf("the unapproved origin served %d requests, want 0: the hop was refused before it connected", hop)
		}
	})

	t.Run("a hop to a forbidden digest is refused before resolution", func(t *testing.T) {
		digest := sha256.Sum256([]byte("forbidden.test"))
		var served int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			served++
			http.Redirect(w, r, "http://sub.forbidden.test/next", http.StatusFound)
		}))
		defer srv.Close()
		resolver := loopbackResolver()
		client := New(Config{
			Resolver:       resolver,
			Slot:           newSlot(4).take,
			Version:        "1.2.3",
			ForbiddenHosts: []string{hex.EncodeToString(digest[:])},
		})

		_, err := client.Do(context.Background(), Op{
			Target: approvedTarget(t, srv),
			Method: http.MethodGet,
			URL:    mustURL(t, srv.URL+"/start"),
			Safe:   true,
		}, nil)
		wantError(t, err, ReasonHostForbidden)
		if served != 1 {
			t.Fatalf("server served %d requests, want one: the hop was refused", served)
		}
		if asked := resolver.lookups("sub.forbidden.test"); asked != 0 {
			t.Fatalf("%d lookups of the forbidden name, want none", asked)
		}
	})
}
