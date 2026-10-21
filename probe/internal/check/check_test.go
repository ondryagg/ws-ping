package check

import (
	"strings"
	"testing"
)

func TestHostMatches(t *testing.T) {
	for host, want := range map[string]bool{
		"lg.example.com":        true,
		"LG.Example.COM":        true,
		"lg.example.com:8443":   true,
		"lg.example.com:1":      true,
		"lg.example.com:":       false,
		"lg.example.com:80x":    false,
		"lg.example.com:123456": false,
		"lg.example.com.":       false,
		"evil.example":          false,
		"lg.example.com.evil":   false,
		"":                      false,
		":8443":                 false,
		"lg.example.com:8443@x": false,
		"[::1]:8443":            false,
		" lg.example.com":       false,
	} {
		if got := HostMatches(host, "lg.example.com"); got != want {
			t.Errorf("%q: got %v want %v", host, got, want)
		}
	}
}

func TestOriginSetIsExact(t *testing.T) {
	s := NewOriginSet([]string{"https://ondrya.com", "https://www.ondrya.com"})
	for o, want := range map[string]bool{
		"https://ondrya.com":      true,
		"https://www.ondrya.com":  true,
		"":                        false,
		"null":                    false,
		"https://ondrya.com/":     false,
		"https://ONDRYA.com":      false,
		"http://ondrya.com":       false,
		"https://ondrya.com:443":  false,
		"https://x.ondrya.com":    false,
		"https://ondrya.com.evil": false,
		" https://ondrya.com":     false,
	} {
		if got := s.Allowed(o); got != want {
			t.Errorf("%q: got %v want %v", o, got, want)
		}
	}
}

func TestOffersSubprotocol(t *testing.T) {
	const p = "ondrya-latency.v1"
	if !OffersSubprotocol([]string{"chat, " + p}, p) || !OffersSubprotocol([]string{"chat", p}, p) {
		t.Fatal("should be found")
	}
	if OffersSubprotocol([]string{"ondrya-latency.v2", p + "x", "x" + p}, p) || OffersSubprotocol(nil, p) {
		t.Fatal("must not match partial tokens")
	}
}

func FuzzOriginAllowed(f *testing.F) {
	allowed := []string{"https://ondrya.com", "https://www.ondrya.com"}
	s := NewOriginSet(allowed)
	for _, a := range allowed {
		f.Add(a)
	}
	f.Add("https://ondrya.com\x00")
	f.Fuzz(func(t *testing.T, o string) {
		want := o == allowed[0] || o == allowed[1]
		if s.Allowed(o) != want {
			t.Fatalf("%q: Allowed=%v, exact=%v", o, s.Allowed(o), want)
		}
	})
}

func FuzzHostMatches(f *testing.F) {
	f.Add("lg.example.com:8443")
	f.Add("[::1]:80")
	f.Add("")
	f.Fuzz(func(t *testing.T, h string) {
		if HostMatches(h, "lg.example.com") {
			base := h
			if i := strings.LastIndexByte(h, ':'); i >= 0 {
				base = h[:i]
			}
			if !strings.EqualFold(base, "lg.example.com") {
				t.Fatalf("%q matched but is not the host", h)
			}
		}
	})
}
