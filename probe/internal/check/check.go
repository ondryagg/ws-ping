// Package check holds the request header comparers used before an upgrade.
package check

import (
	"net"
	"strings"
)

// HostMatches reports whether a Host header names want (lowercase). The port
// is ignored; everything else must match case-insensitively.
func HostMatches(hostHeader, want string) bool {
	h := hostHeader
	if host, port, err := net.SplitHostPort(h); err == nil {
		if !validPort(port) {
			return false
		}
		h = host
	}
	return h != "" && strings.EqualFold(h, want)
}

// validPort accepts 1 to 5 ASCII digits. SplitHostPort does not check this,
// and a Host such as "name:80@x" must not pass.
func validPort(p string) bool {
	if len(p) == 0 || len(p) > 5 {
		return false
	}
	for i := 0; i < len(p); i++ {
		if p[i] < '0' || p[i] > '9' {
			return false
		}
	}
	return true
}

// OriginSet is an exact-match allow list.
type OriginSet map[string]struct{}

func NewOriginSet(origins []string) OriginSet {
	s := make(OriginSet, len(origins))
	for _, o := range origins {
		s[o] = struct{}{}
	}
	return s
}

// Allowed is an exact, byte-for-byte match. There is no normalisation, so
// "null", empty, mixed case or a trailing slash never match.
func (s OriginSet) Allowed(origin string) bool {
	if origin == "" {
		return false
	}
	_, ok := s[origin]
	return ok
}

// OffersSubprotocol reports whether any Sec-WebSocket-Protocol header value
// lists want as one of its comma separated tokens.
func OffersSubprotocol(values []string, want string) bool {
	for _, v := range values {
		for _, tok := range strings.Split(v, ",") {
			if strings.TrimSpace(tok) == want {
				return true
			}
		}
	}
	return false
}
