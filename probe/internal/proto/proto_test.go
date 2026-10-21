package proto

import (
	"strings"
	"testing"
)

func TestParsePing(t *testing.T) {
	ok := map[string]uint32{
		`{"t":"ping","seq":0}`:          0,
		`{"t":"ping","seq":4294967295}`: 4294967295,
		`{"seq":5,"t":"ping"}`:          5,
		` {"t":"ping", "seq": 12 } `:    12,
	}
	for in, want := range ok {
		got, err := ParsePing([]byte(in))
		if err != nil || got != want {
			t.Errorf("%q: %d %v", in, got, err)
		}
	}
	bad := []string{``, `{}`, `null`, `[]`, `{"t":"ping"}`, `{"seq":1}`, `{"t":"pong","seq":1}`, `{"t":"ping","seq":-1}`,
		`{"t":"ping","seq":4294967296}`, `{"t":"ping","seq":1.0}`, `{"t":"ping","seq":"1"}`, `{"t":"ping","seq":1,"x":1}`,
		`{"t":"ping","seq":1}{"t":"ping","seq":2}`, `{"t":"ping","seq":1}x`, `{"t":null,"seq":1}`, `{"t":"ping","seq":null}`}
	for _, in := range bad {
		if _, err := ParsePing([]byte(in)); err == nil {
			t.Errorf("%q must be rejected", in)
		}
	}
}

func TestPongAndHelloFormat(t *testing.T) {
	if got := string(AppendPong(nil, 4294967295)); got != `{"t":"pong","seq":4294967295}` {
		t.Fatal(got)
	}
	h := NewHelloEncoder(`we"ird`, 30).Append(nil, 1700000000123)
	want := `{"t":"hello","proto":1,"node":"we\"ird","maxPings":30,"serverTimeMs":1700000000123}`
	if string(h) != want {
		t.Fatalf("%s", h)
	}
}

func FuzzParsePing(f *testing.F) {
	for _, s := range []string{`{"t":"ping","seq":1}`, `{}`, ``, `{"t":"ping","seq":99999999999}`, strings.Repeat("{", 64)} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		seq, err := ParsePing(b)
		if err != nil {
			return
		}
		// An accepted message must echo back to something ParsePing-compatible.
		pong := AppendPong(nil, seq)
		if !strings.HasPrefix(string(pong), `{"t":"pong","seq":`) || len(pong) > 64 {
			t.Fatalf("bad pong %q", pong)
		}
	})
}
