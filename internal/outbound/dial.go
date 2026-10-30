package outbound

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"syscall"
	"time"
)

// The transport bounds of one hop: no dial, handshake or header exchange outlives
// these, whatever the peer does.
const (
	dialTimeout            = 5 * time.Second
	tlsHandshakeTimeout    = 5 * time.Second
	responseHeaderTimeout  = 10 * time.Second
	maxResponseHeaderBytes = 64 << 10
)

// Address classes that are never a destination, whatever the request's origin
// approves. 0.0.0.0/8 is the unspecified block, which a Linux host routes to
// itself; the well-known and the local-use NAT64 prefixes embed an IPv4 address
// that would otherwise be reached as IPv6, and so does the 6to4 prefix. 240.0.0.0/4
// is reserved (255.255.255.255 included) and 198.18.0.0/15 is the benchmarking
// block: a resolver answer inside either names no peer this client should reach.
var (
	unspecified = netip.MustParsePrefix("0.0.0.0/8")
	nat64       = netip.MustParsePrefix("64:ff9b::/96")
	nat64Local  = netip.MustParsePrefix("64:ff9b:1::/48")
	sixToFour   = netip.MustParsePrefix("2002::/16")
	cgnat       = netip.MustParsePrefix("100.64.0.0/10")
	reserved    = netip.MustParsePrefix("240.0.0.0/4")
	benchmark   = netip.MustParsePrefix("198.18.0.0/15")
)

// metadataAddresses are the well-known instance metadata endpoints. Each is also
// link-local, carrier-grade NAT or unique-local; they are named here so the class
// that is denied on purpose stays visible.
var metadataAddresses = []netip.Addr{
	netip.MustParseAddr("169.254.169.254"),
	netip.MustParseAddr("169.254.170.2"),
	netip.MustParseAddr("100.100.100.200"),
	netip.MustParseAddr("fd00:ec2::254"),
}

// Resolver resolves one hostname to the addresses a dial may use. *net.Resolver
// satisfies it; a test injects a fixed table so every refusal is provable without
// the network.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// newTransport builds the transport of one hop. The proxy is explicitly absent, so
// no environment variable routes a request; keep-alives and HTTP/2 are off, one
// RoundTrip is one wire request; the handshake and the response header are
// bounded; and there is no DialTLSContext and no TLS ServerName, so the transport
// proves the certificate against the request URL's hostname even though the
// connection is made to a resolved address.
func newTransport(hop Origin, privateAllowed bool, resolver Resolver) *http.Transport {
	return &http.Transport{
		Proxy:                  nil,
		DialContext:            newDial(hop, privateAllowed, resolver),
		DisableKeepAlives:      true,
		ForceAttemptHTTP2:      false,
		TLSNextProto:           map[string]func(string, *tls.Conn) http.RoundTripper{},
		TLSHandshakeTimeout:    tlsHandshakeTimeout,
		ResponseHeaderTimeout:  responseHeaderTimeout,
		MaxResponseHeaderBytes: maxResponseHeaderBytes,
	}
}

// newDial dials hop and nothing else. The address must be the hop's own host and
// effective port, the name is resolved again for this attempt, every resolved
// address has to pass checkAddress, and the control hook repeats that check on the
// address the socket is about to reach, so a resolution that changed between the
// two cannot be dialed.
func newDial(hop Origin, privateAllowed bool, resolver Resolver) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if hop.Port < 1 || hop.Port > 65535 {
			return nil, denyAddress(netip.Addr{}, hop.Host)
		}
		host, port, err := net.SplitHostPort(address)
		if err != nil || canonicalHost(host) != hop.Host || port != strconv.Itoa(hop.Port) {
			return nil, denyAddress(netip.Addr{}, hop.Host)
		}
		ctx, cancel := context.WithTimeout(ctx, dialTimeout)
		defer cancel()

		addresses, err := resolver.LookupNetIP(ctx, "ip", hop.Host)
		if err != nil || len(addresses) == 0 {
			return nil, denyAddress(netip.Addr{}, hop.Host)
		}
		// One refused address refuses the whole answer: a resolver that mixes a
		// public and a metadata reply gets neither dialed.
		for _, resolved := range addresses {
			if refusal := checkAddress(resolved, privateAllowed); refusal != nil {
				return nil, refusal
			}
		}

		dialer := &net.Dialer{
			Timeout: dialTimeout,
			ControlContext: func(_ context.Context, _, target string, _ syscall.RawConn) error {
				connected, err := netip.ParseAddrPort(target)
				if err != nil {
					return denyAddress(netip.Addr{}, target)
				}
				if int(connected.Port()) != hop.Port {
					return denyAddress(connected.Addr(), hop.Host)
				}
				if refusal := checkAddress(connected.Addr(), privateAllowed); refusal != nil {
					return refusal
				}
				return nil
			},
		}
		var failure error
		for _, resolved := range addresses {
			target := netip.AddrPortFrom(resolved.Unmap(), uint16(hop.Port)).String()
			connection, err := dialer.DialContext(ctx, network, target)
			if err == nil {
				return connection, nil
			}
			failure = err
		}
		return nil, failure
	}
}

// checkAddress refuses every address class the hop's origin does not authorize.
// Loopback, private, unique-local and carrier-grade NAT are the classes an exact
// approved Private origin may reach.
func checkAddress(resolved netip.Addr, privateAllowed bool) *Error {
	refusal := denyAddress(resolved, "") // names the answer as the resolver gave it
	resolved = resolved.Unmap()
	switch {
	case !resolved.IsValid(), resolved.Zone() != "":
		return refusal
	case resolved.IsUnspecified(), unspecified.Contains(resolved):
		return refusal
	case resolved.IsMulticast(), resolved.IsLinkLocalUnicast(), resolved.IsLinkLocalMulticast():
		return refusal
	case slices.Contains(metadataAddresses, resolved),
		nat64.Contains(resolved), nat64Local.Contains(resolved), sixToFour.Contains(resolved),
		reserved.Contains(resolved), benchmark.Contains(resolved):
		return refusal
	case isInternal(resolved) && !privateAllowed:
		return refusal
	}
	return nil
}

// denyAddress is the refusal of one address, or of the name that produced none.
func denyAddress(resolved netip.Addr, host string) *Error {
	params := make(map[string]string, 1)
	switch {
	case resolved.IsValid():
		params["address"] = resolved.String()
	case host != "":
		params["host"] = canonicalHost(host)
	}
	return &Error{Reason: ReasonDialDenied, Params: params}
}

// isInternal reports the address classes only an exact approved Private origin
// may reach: loopback, private (RFC 1918 and RFC 4193) and carrier-grade NAT.
func isInternal(resolved netip.Addr) bool {
	return resolved.IsLoopback() || resolved.IsPrivate() || cgnat.Contains(resolved)
}
