package server

import (
	"net/http"
	"net/netip"
	"strings"
)

// PeerMatches uses the socket peer, including IPv4-mapped IPv6 peers.
func PeerMatches(remote string, prefixes []netip.Prefix) bool {
	peer, err := netip.ParseAddrPort(remote)
	if err != nil {
		return false
	}
	addr := peer.Addr().Unmap()
	for _, prefix := range prefixes {
		if prefix.Addr().Is4In6() && prefix.Bits() >= 96 {
			prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
		}
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func secureRequest(r *http.Request, prefixes []netip.Prefix) bool {
	if r.TLS != nil {
		return true
	}
	if !PeerMatches(r.RemoteAddr, prefixes) {
		return false
	}
	values := r.Header.Values("X-Forwarded-Proto")
	return len(values) == 1 && strings.TrimSpace(values[0]) == "https"
}
