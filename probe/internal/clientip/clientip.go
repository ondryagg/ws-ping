// Package clientip derives the rate-limiting key for a request.
package clientip

import (
	"net/netip"
	"strings"
)

// Key collapses an address to its limiting identity: IPv4 stays as is, IPv6
// is truncated to its /64 so one subscriber cannot rotate through addresses.
func Key(a netip.Addr) netip.Addr {
	a = a.Unmap().WithZone("")
	if a.Is6() {
		if p, err := a.Prefix(64); err == nil {
			return p.Addr()
		}
	}
	return a
}

// Parse extracts the address from a "host:port" RemoteAddr.
func Parse(remote string) (netip.Addr, bool) {
	ap, err := netip.ParseAddrPort(remote)
	if err != nil {
		return netip.Addr{}, false
	}
	return ap.Addr().Unmap().WithZone(""), true
}

// IsLocal reports whether peer is this host talking to itself: a loopback
// address, or the same address the connection arrived on. The latter is what
// the kernel uses as the source when a process dials its own non-loopback
// address, which is how the self-test reaches a listener bound to one IP.
func IsLocal(peer, local netip.Addr) bool {
	peer = peer.Unmap().WithZone("")
	return peer.IsLoopback() || (local.IsValid() && peer == local.Unmap().WithZone(""))
}

// Resolver picks the client address for a request. The TCP peer is the
// client unless the peer is in the trusted proxy list.
type Resolver struct {
	Trusted []netip.Prefix
}

func (r Resolver) trusted(a netip.Addr) bool {
	for _, p := range r.Trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Client returns the TCP peer and the address to rate-limit. xff holds the
// X-Forwarded-For header values; they are only read when the peer is trusted,
// and then from the right, skipping other trusted proxies.
func (r Resolver) Client(remote string, xff []string) (peer, client netip.Addr, ok bool) {
	peer, ok = Parse(remote)
	if !ok {
		return netip.Addr{}, netip.Addr{}, false
	}
	if len(r.Trusted) == 0 || !r.trusted(peer) || len(xff) == 0 {
		return peer, peer, true
	}
	var hops []string
	for _, v := range xff {
		hops = append(hops, strings.Split(v, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			return peer, peer, true // malformed header: fall back to the peer
		}
		a = a.Unmap().WithZone("")
		if !r.trusted(a) {
			return peer, a, true
		}
	}
	return peer, peer, true
}
