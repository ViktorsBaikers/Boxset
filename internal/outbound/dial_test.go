package outbound

import (
	"context"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// TestDialDeniesForbiddenAddresses proves AC-031's destination classes: every
// address the request's origin does not authorize is refused before a connection,
// a public origin that resolves to loopback never reaches the listener it would
// have connected to, an answer that mixes a public and a metadata address is
// refused whole, and a flip between a request and its hop is refused because the
// hop is resolved again.
func TestDialDeniesForbiddenAddresses(t *testing.T) {
	var served int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served++
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	port, err := strconv.Atoi(mustURL(t, srv.URL).Port())
	if err != nil {
		t.Fatalf("server port: %v", err)
	}

	t.Run("the exact approved private origin dials", func(t *testing.T) {
		served = 0
		client, _ := newTestClient(loopbackResolver(), nil)
		response, err := client.Do(context.Background(), Op{
			Target: approvedTarget(t, srv),
			Method: http.MethodGet,
			URL:    mustURL(t, srv.URL+"/items"),
			Safe:   true,
		}, nil)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		if response.Body != "ok" || served != 1 {
			t.Fatalf("response = %q after %d requests, want ok after one", response.Body, served)
		}
	})

	t.Run("every unauthorized address class is refused", func(t *testing.T) {
		served = 0
		for _, class := range []struct{ name, address string }{
			{"the unspecified address", "0.0.0.0"},
			{"the unspecified block", "0.0.0.1"},
			{"a multicast group", "224.0.0.1"},
			{"a link-local address", "169.254.1.1"},
			{"the metadata endpoint", "169.254.169.254"},
			{"an IPv4-mapped metadata endpoint", "::ffff:169.254.169.254"},
			{"another metadata endpoint", "100.100.100.200"},
			{"an IPv4-mapped carrier-grade NAT address", "::ffff:100.64.0.1"},
			{"an address inside the NAT64 prefix", "64:ff9b::7f00:1"},
			{"an address inside the local-use NAT64 prefix", "64:ff9b:1::7f00:1"},
			{"an address inside the 6to4 prefix", "2002:7f00:1::1"},
			{"an address in the reserved block", "240.0.0.1"},
			{"the limited broadcast address", "255.255.255.255"},
			{"an address in the benchmarking block", "198.18.0.1"},
			{"an IPv4-mapped benchmarking address", "::ffff:198.19.0.1"},
			{"a private address", "10.1.2.3"},
			{"a unique-local address", "fd00::1"},
			{"a loopback address", "127.0.0.1"},
			{"a zoned link-local literal", "fe80::1%eth0"},
			{"a zoned global literal", "2001:db8::1%eth0"},
		} {
			t.Run(class.name, func(t *testing.T) {
				resolver := newResolver(map[string][]netip.Addr{"denied.test": addrs(class.address)})
				client, _ := newTestClient(resolver, nil)
				_, err := client.Do(context.Background(), Op{
					Target: &Target{Origin: Origin{Scheme: "http", Host: "public.test", Port: 80}},
					Method: http.MethodGet,
					URL:    mustURL(t, "http://denied.test:"+strconv.Itoa(port)+"/items"),
					Safe:   true,
				}, nil)
				refused := wantError(t, err, ReasonDialDenied)
				if refused.Attempts != 0 {
					t.Fatalf("attempts = %d, want 0: the address was refused before a wire request", refused.Attempts)
				}
			})
		}
		// The loopback class above would have reached this server had it been
		// dialed: it saw no request.
		if served != 0 {
			t.Fatalf("the loopback origin served %d requests, want 0", served)
		}
	})

	t.Run("a metadata endpoint is refused for the approved private origin too", func(t *testing.T) {
		// Carrier-grade NAT and unique-local addresses are reachable as the approved
		// private origin, so the metadata endpoints inside those classes need their own
		// refusal.
		for _, class := range []struct {
			name    string
			address string
			mapped  bool
		}{
			{name: "the carrier-grade NAT endpoint", address: "100.100.100.200"},
			{name: "the IPv4-mapped carrier-grade NAT endpoint", address: "100.100.100.200", mapped: true},
			{name: "the unique-local endpoint", address: "fd00:ec2::254"},
		} {
			t.Run(class.name, func(t *testing.T) {
				answer := addrs(class.address)
				if class.mapped {
					// A resolver can answer with the IPv4-mapped IPv6 form itself,
					// which is not the form the parser normalises to.
					answer = []netip.Addr{netip.AddrFrom16(answer[0].As16())}
				}
				resolver := newResolver(map[string][]netip.Addr{"approved.test": answer})
				client, _ := newTestClient(resolver, nil)
				_, err := client.Do(context.Background(), Op{
					Target: &Target{Origin: Origin{Scheme: "http", Host: "approved.test", Port: port}, Private: true},
					Method: http.MethodGet,
					URL:    mustURL(t, "http://approved.test:"+strconv.Itoa(port)+"/items"),
					Safe:   true,
				}, nil)
				wantError(t, err, ReasonDialDenied)
			})
		}
	})

	t.Run("a public origin that resolves to loopback never connects", func(t *testing.T) {
		counter := countConnections(t)
		resolver := newResolver(map[string][]netip.Addr{"private.test": addrs("127.0.0.1")})
		client, _ := newTestClient(resolver, nil)
		_, err := client.Do(context.Background(), Op{
			Target: &Target{Origin: Origin{Scheme: "http", Host: "public.test", Port: 80}},
			Method: http.MethodGet,
			URL:    mustURL(t, "http://private.test:"+counter.port(t)+"/items"),
			Safe:   true,
		}, nil)
		wantError(t, err, ReasonDialDenied)
		if accepted := counter.count(); accepted != 0 {
			t.Fatalf("the listener accepted %d connections, want 0", accepted)
		}
	})

	t.Run("an answer mixing a public and a metadata address is refused whole", func(t *testing.T) {
		resolver := newResolver(map[string][]netip.Addr{
			"mixed.test": addrs("192.0.2.10", "169.254.169.254"),
		})
		client, _ := newTestClient(resolver, nil)
		_, err := client.Do(context.Background(), Op{
			Target: &Target{Origin: Origin{Scheme: "http", Host: "public.test", Port: 80}},
			Method: http.MethodGet,
			URL:    mustURL(t, "http://mixed.test/items"),
			Safe:   true,
		}, nil)
		wantError(t, err, ReasonDialDenied)
	})

	t.Run("a flip between a request and its hop is refused", func(t *testing.T) {
		var hops int
		resolver := loopbackResolver()
		flipped := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hops++
			if r.URL.Path == "/start" {
				resolver.answer("127.0.0.1", "169.254.169.254")
				http.Redirect(w, r, "/next", http.StatusTemporaryRedirect)
				return
			}
			fmt.Fprint(w, "rebound")
		}))
		defer flipped.Close()
		client, _ := newTestClient(resolver, nil)
		_, err := client.Do(context.Background(), Op{
			Target: approvedTarget(t, flipped),
			Method: http.MethodGet,
			URL:    mustURL(t, flipped.URL+"/start"),
			Safe:   true,
		}, nil)
		wantError(t, err, ReasonDialDenied)
		if hops != 1 {
			t.Fatalf("the origin served %d requests, want one: the hop was resolved again and refused", hops)
		}
	})
}

// TestDialVerifiesTLSHostname proves AC-031's TLS clause: the certificate is
// verified against the request URL's hostname even though the connection is made to
// the address the resolver returned, so a certificate for any other name is refused.
func TestDialVerifiesTLSHostname(t *testing.T) {
	var served int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served++
		fmt.Fprint(w, "secure")
	}))
	defer srv.Close()
	srv.Config.ErrorLog = quietLog
	port, err := strconv.Atoi(mustURL(t, srv.URL).Port())
	if err != nil {
		t.Fatalf("server port: %v", err)
	}
	resolver := newResolver(map[string][]netip.Addr{
		"example.com": addrs("127.0.0.1"),
		"other.test":  addrs("127.0.0.1"),
	})
	client, _ := newTestClient(resolver, trustServer(srv))
	url := "https://example.com:" + strconv.Itoa(port) + "/items"

	t.Run("the request hostname is the name that is verified", func(t *testing.T) {
		served = 0
		response, err := client.Do(context.Background(), Op{
			Target: &Target{Origin: Origin{Scheme: "https", Host: "example.com", Port: port}, Private: true},
			Method: http.MethodGet,
			URL:    mustURL(t, url),
			Safe:   true,
		}, nil)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		if response.Body != "secure" || served != 1 {
			t.Fatalf("response = %q after %d requests, want secure after one", response.Body, served)
		}
	})

	t.Run("a certificate for another name is refused", func(t *testing.T) {
		served = 0
		_, err := client.Do(context.Background(), Op{
			Target: &Target{Origin: Origin{Scheme: "https", Host: "other.test", Port: port}, Private: true},
			Method: http.MethodGet,
			URL:    mustURL(t, "https://other.test:"+strconv.Itoa(port)+"/items"),
			Safe:   true,
		}, nil)
		if err == nil {
			t.Fatal("Do succeeded, want the handshake refused")
		}
		if served != 0 {
			t.Fatalf("the peer served %d requests, want 0: the certificate was refused", served)
		}
	})
}

// TestAddressClasses proves the classification itself in both directions: each
// class that is never a destination is refused even for an approved private
// origin, the private classes are refused only without that approval, and the
// public neighbours of every refused prefix stay reachable — no prefix was drawn
// one bit too wide.
func TestAddressClasses(t *testing.T) {
	for _, class := range []struct {
		name     string
		address  string
		allowed  bool // a public address: reachable for either origin
		internal bool // reachable only as an exact approved Private origin
	}{
		{name: "the unspecified address", address: "0.0.0.1"},
		{name: "a multicast group", address: "224.0.0.1"},
		{name: "a link-local address", address: "169.254.1.1"},
		{name: "a metadata endpoint", address: "169.254.169.254"},
		{name: "the carrier-grade NAT metadata endpoint", address: "100.100.100.200"},
		{name: "the unique-local metadata endpoint", address: "fd00:ec2::254"},
		{name: "the NAT64 prefix", address: "64:ff9b::7f00:1"},
		{name: "the local-use NAT64 prefix", address: "64:ff9b:1::1"},
		{name: "the 6to4 prefix", address: "2002::1"},
		{name: "the reserved block", address: "240.0.0.1"},
		{name: "the limited broadcast address", address: "255.255.255.255"},
		{name: "the benchmarking block", address: "198.18.0.1"},
		{name: "an IPv4-mapped benchmarking address", address: "::ffff:198.18.0.1"},
		{name: "a loopback address", address: "127.0.0.1", internal: true},
		{name: "a private address", address: "10.1.2.3", internal: true},
		{name: "a carrier-grade NAT address", address: "100.64.0.1", internal: true},
		{name: "a unique-local address", address: "fd00::1", internal: true},
		{name: "the end of the carrier-grade NAT block", address: "100.127.255.255", internal: true},
		{name: "a public address below the carrier-grade NAT block", address: "100.63.255.255", allowed: true},
		{name: "a public address above the carrier-grade NAT block", address: "100.128.0.1", allowed: true},
		{name: "a public address below the benchmarking block", address: "198.17.255.255", allowed: true},
		{name: "a public address above the benchmarking block", address: "198.20.0.1", allowed: true},
		{name: "a public address below the reserved block", address: "223.255.255.255", allowed: true},
		{name: "the documentation address the tests call public", address: "192.0.2.10", allowed: true},
		{name: "a public address before the 6to4 prefix", address: "2001:4860:4860::8888", allowed: true},
		{name: "a public address after the 6to4 prefix", address: "2003::1", allowed: true},
		{name: "a public address beside the local-use NAT64 prefix", address: "64:ff9b:2::1", allowed: true},
	} {
		t.Run(class.name, func(t *testing.T) {
			resolved := netip.MustParseAddr(class.address)
			if class.allowed {
				for _, privateAllowed := range []bool{false, true} {
					if refusal := checkAddress(resolved, privateAllowed); refusal != nil {
						t.Fatalf("checkAddress(%s, %t) = %v, want a public address reached", class.address, privateAllowed, refusal)
					}
				}
				return
			}
			if class.internal {
				if refusal := checkAddress(resolved, false); refusal == nil {
					t.Fatalf("checkAddress(%s, false) = nil, want a refusal for a public origin", class.address)
				}
				if refusal := checkAddress(resolved, true); refusal != nil {
					t.Fatalf("checkAddress(%s, true) = %v, want the approved private origin to reach it", class.address, refusal)
				}
				return
			}
			for _, privateAllowed := range []bool{false, true} {
				if refusal := checkAddress(resolved, privateAllowed); refusal == nil {
					t.Fatalf("checkAddress(%s, %t) = nil, want a class that is never a destination", class.address, privateAllowed)
				}
			}
		})
	}
}

// TestTransportVerifiesCertificate proves AC-031's TLS clause on the production
// transport: a certificate is verified against the request URL's hostname even
// though the connection is made to the address the resolver returned. Nothing
// wraps this transport or replaces its TLSClientConfig, so newTransport's own
// fields decide — a pinned ServerName, a skipped verification or a trusted dialed
// address reddens it.
func TestTransportVerifiesCertificate(t *testing.T) {
	const child = "BOXSET_CERTIFICATE_PROOF"
	if os.Getenv(child) == "1" {
		verifyCertificateProof(t)
		return
	}
	// Go reads the system bundle once per process, so the proof runs in a child
	// that makes the test certificate its bundle from the first handshake. A
	// proof that cannot arm its own bundle fails the matching-name case loudly
	// instead of passing on the host's roots.
	command := exec.Command(os.Args[0], "-test.run=^TestTransportVerifiesCertificate$")
	command.Env = append(os.Environ(), child+"=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("the certificate proof failed: %v\n%s", err, output)
	}
}

// verifyCertificateProof is the child that arms SSL_CERT_FILE and runs the pair:
// the request's own hostname verifies, another name's certificate is refused.
func verifyCertificateProof(t *testing.T) {
	var served int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served++
		fmt.Fprint(w, "secure")
	}))
	defer srv.Close()
	srv.Config.ErrorLog = quietLog
	port, err := strconv.Atoi(mustURL(t, srv.URL).Port())
	if err != nil {
		t.Fatalf("server port: %v", err)
	}
	bundle := filepath.Join(t.TempDir(), "roots.pem")
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(bundle, certificate, 0o600); err != nil {
		t.Fatalf("write the certificate bundle: %v", err)
	}
	t.Setenv("SSL_CERT_FILE", bundle)
	resolver := newResolver(map[string][]netip.Addr{
		"example.com": addrs("127.0.0.1"),
		"other.test":  addrs("127.0.0.1"),
	})
	for _, proof := range []struct {
		name    string
		host    string
		refused string // the verification text a refused certificate must name
	}{
		{name: "the request hostname's certificate is accepted", host: "example.com"},
		{name: "a certificate for another name is refused", host: "other.test", refused: "not other.test"},
	} {
		t.Run(proof.name, func(t *testing.T) {
			served = 0
			transport := newTransport(Origin{Scheme: "https", Host: proof.host, Port: port}, true, resolver)
			request, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
				"https://"+proof.host+":"+strconv.Itoa(port)+"/items", nil)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			response, err := transport.RoundTrip(request)
			if proof.refused != "" {
				if err == nil {
					_ = response.Body.Close()
					t.Fatal("RoundTrip succeeded, want the certificate for another name refused")
				}
				if !strings.Contains(err.Error(), proof.refused) {
					t.Fatalf("error = %v, want the hostname verification refusal naming %q", err, proof.refused)
				}
				if served != 0 {
					t.Fatalf("the peer served %d requests, want 0: the handshake was refused", served)
				}
				return
			}
			if err != nil {
				t.Fatalf("RoundTrip: %v", err)
			}
			defer func() { _ = response.Body.Close() }()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if string(body) != "secure" || served != 1 {
				t.Fatalf("response = %q after %d requests, want secure after one", body, served)
			}
		})
	}
}

// countConnections is a loopback listener that counts the connections it accepts, so
// a refusal is provable as zero.
type connectionCounter struct {
	listener net.Listener
	mu       sync.Mutex
	accepted int
}

func countConnections(t *testing.T) *connectionCounter {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	counter := &connectionCounter{listener: listener}
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			counter.mu.Lock()
			counter.accepted++
			counter.mu.Unlock()
			_ = connection.Close()
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return counter
}

// port is the listener's port, so a request that must not dial would land here.
func (c *connectionCounter) port(t *testing.T) string {
	t.Helper()
	address, ok := c.listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address = %T, want *net.TCPAddr", c.listener.Addr())
	}
	return strconv.Itoa(address.Port)
}

func (c *connectionCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.accepted
}
