package clientip

import (
	"net/netip"
	"testing"
)

func TestKeyCollapsesIPv6To64AndUnmapsIPv4(t *testing.T) {
	a := Key(netip.MustParseAddr("2001:db8:1:2:aaaa:bbbb:cccc:dddd"))
	b := Key(netip.MustParseAddr("2001:db8:1:2:1111:2222:3333:4444"))
	c := Key(netip.MustParseAddr("2001:db8:1:3::1"))
	if a != b {
		t.Fatalf("same /64 must share a key: %v %v", a, b)
	}
	if a == c {
		t.Fatal("different /64 must differ")
	}
	if Key(netip.MustParseAddr("::ffff:192.0.2.9")) != netip.MustParseAddr("192.0.2.9") {
		t.Fatal("v4-mapped must unmap")
	}
	if Key(netip.MustParseAddr("fe80::1%eth0")).Zone() != "" {
		t.Fatal("zone must be stripped")
	}
}

func TestForwardedForOnlyFromTrustedPeers(t *testing.T) {
	r := Resolver{}
	_, cl, ok := r.Client("198.51.100.1:5000", []string{"203.0.113.9"})
	if !ok || cl != netip.MustParseAddr("198.51.100.1") {
		t.Fatalf("default must ignore XFF, got %v", cl)
	}
	r = Resolver{Trusted: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}
	_, cl, _ = r.Client("198.51.100.1:5000", []string{"203.0.113.9"})
	if cl != netip.MustParseAddr("198.51.100.1") {
		t.Fatalf("untrusted peer must be used as is, got %v", cl)
	}
	_, cl, _ = r.Client("10.1.1.1:5000", []string{"1.2.3.4, 203.0.113.9, 10.2.2.2"})
	if cl != netip.MustParseAddr("203.0.113.9") {
		t.Fatalf("rightmost untrusted hop expected, got %v", cl)
	}
	_, cl, _ = r.Client("10.1.1.1:5000", []string{"garbage"})
	if cl != netip.MustParseAddr("10.1.1.1") {
		t.Fatalf("malformed XFF must fall back to the peer, got %v", cl)
	}
	if _, _, ok := r.Client("not-an-address", nil); ok {
		t.Fatal("bad remote address must fail")
	}
}

func TestIsLocalIncludesTheHostsOwnAddress(t *testing.T) {
	own := netip.MustParseAddr("203.0.113.5")
	for peer, want := range map[string]bool{
		"127.0.0.1": true, "::1": true, "203.0.113.5": true, "::ffff:203.0.113.5": true,
		"203.0.113.6": false, "198.51.100.1": false,
	} {
		if got := IsLocal(netip.MustParseAddr(peer), own); got != want {
			t.Errorf("%s: got %v want %v", peer, got, want)
		}
	}
	if IsLocal(netip.MustParseAddr("203.0.113.5"), netip.Addr{}) {
		t.Fatal("an unknown local address must not match anything")
	}
}
