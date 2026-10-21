package netx

import (
	"net"
	"sync"
	"syscall"
	"testing"
)

type fakeConn struct {
	net.Conn
	addr  *net.TCPAddr
	local *net.TCPAddr
}

func (f fakeConn) RemoteAddr() net.Addr { return f.addr }
func (f fakeConn) LocalAddr() net.Addr {
	if f.local != nil {
		return f.local
	}
	return f.Conn.LocalAddr()
}

type fakeListener struct {
	mu    sync.Mutex
	queue []net.Addr
	err   error
	peers []net.Conn
	local *net.TCPAddr
}

func (f *fakeListener) Accept() (net.Conn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queue) == 0 {
		return nil, f.err
	}
	a := f.queue[0]
	f.queue = f.queue[1:]
	c1, c2 := net.Pipe()
	f.peers = append(f.peers, c2)
	fc := fakeConn{Conn: c1, addr: a.(*net.TCPAddr)}
	if f.local != nil {
		fc.local = f.local
	}
	return fc, nil
}
func (f *fakeListener) Close() error   { return nil }
func (f *fakeListener) Addr() net.Addr { return &net.TCPAddr{} }

func tcp(ip string) net.Addr { return &net.TCPAddr{IP: net.ParseIP(ip), Port: 40000} }

func TestCapsApplyBeforeTLSAndReleaseOnClose(t *testing.T) {
	fl := &fakeListener{err: net.ErrClosed, queue: []net.Addr{
		tcp("198.51.100.1"), tcp("198.51.100.1"), tcp("198.51.100.1"), // third is over the per-IP cap
		tcp("198.51.100.2"), tcp("198.51.100.3"), // fifth overall pushes past the global cap of 3
	}}
	var rejects []string
	l := Wrap(fl, Options{MaxConns: 3, MaxPerIP: 2, OnReject: func(r string) { rejects = append(rejects, r) }})

	var open []net.Conn
	for i := 0; i < 3; i++ {
		c, err := l.Accept()
		if err != nil {
			t.Fatal(err)
		}
		open = append(open, c)
	}
	if _, err := l.Accept(); err == nil {
		t.Fatal("queue should be drained with the closed error")
	}
	if len(rejects) != 2 || rejects[0] != "raw_per_ip" || rejects[1] != "raw_capacity" {
		t.Fatalf("rejects %v", rejects)
	}
	if l.Open() != 3 {
		t.Fatalf("open %d", l.Open())
	}
	for _, c := range open {
		c.Close()
		c.Close() // idempotent
	}
	if l.Open() != 0 || l.perIP.Len() != 0 {
		t.Fatalf("not released: open=%d keys=%d", l.Open(), l.perIP.Len())
	}
}

func TestLoopbackIsExemptSoTheWatchdogIsNotStarved(t *testing.T) {
	var q []net.Addr
	for i := 0; i < 10; i++ {
		q = append(q, tcp("127.0.0.1"))
	}
	l := Wrap(&fakeListener{err: net.ErrClosed, queue: q}, Options{MaxConns: 1, MaxPerIP: 1})
	for i := 0; i < 10; i++ {
		if _, err := l.Accept(); err != nil {
			t.Fatalf("loopback connection %d refused: %v", i, err)
		}
	}
	if l.Open() != 0 {
		t.Fatal("loopback must not be tracked")
	}
}

func TestIPv6SharesOneSlotPer64(t *testing.T) {
	fl := &fakeListener{err: net.ErrClosed, queue: []net.Addr{
		tcp("2001:db8:1:1::1"), tcp("2001:db8:1:1::2"), tcp("2001:db8:1:2::1"),
	}}
	n := 0
	l := Wrap(fl, Options{MaxConns: 10, MaxPerIP: 1, OnReject: func(string) { n++ }})
	var got int
	for {
		if _, err := l.Accept(); err != nil {
			break
		}
		got++
	}
	if got != 2 || n != 1 {
		t.Fatalf("accepted %d rejected %d", got, n)
	}
}

func TestAcceptErrorsAreReportedAndClassified(t *testing.T) {
	var classes []string
	fl := &fakeListener{err: &net.OpError{Op: "accept", Err: syscall.EMFILE}}
	l := Wrap(fl, Options{MaxConns: 1, MaxPerIP: 1, OnAcceptError: func(c string, _ error) { classes = append(classes, c) }})
	if _, err := l.Accept(); err == nil {
		t.Fatal("error must be returned unchanged so net/http backs off")
	}
	if len(classes) != 1 || classes[0] != "emfile" {
		t.Fatalf("%v", classes)
	}
	for err, want := range map[error]string{syscall.ENFILE: "enfile", net.ErrClosed: "closed", syscall.ECONNABORTED: "aborted"} {
		if got := ClassifyAcceptError(err); got != want {
			t.Errorf("%v: %s want %s", err, got, want)
		}
	}
}

func TestAConnectionFromTheHostsOwnAddressIsExempt(t *testing.T) {
	// A listener bound to one IP is reached by the self-test from that same IP.
	own := &net.TCPAddr{IP: net.ParseIP("203.0.113.5"), Port: 8443}
	var q []net.Addr
	for i := 0; i < 5; i++ {
		q = append(q, tcp("203.0.113.5"))
	}
	l := Wrap(&fakeListener{err: net.ErrClosed, queue: q, local: own}, Options{MaxConns: 1, MaxPerIP: 1})
	for i := 0; i < 5; i++ {
		if _, err := l.Accept(); err != nil {
			t.Fatalf("self connection %d refused: %v", i, err)
		}
	}
	// The same remote address is not exempt when the listener is elsewhere.
	other := &net.TCPAddr{IP: net.ParseIP("203.0.113.9"), Port: 8443}
	l2 := Wrap(&fakeListener{err: net.ErrClosed, queue: []net.Addr{tcp("203.0.113.5"), tcp("203.0.113.5")}, local: other},
		Options{MaxConns: 10, MaxPerIP: 1})
	if _, err := l2.Accept(); err != nil {
		t.Fatal(err)
	}
	var rejected int
	l2.o.OnReject = func(string) { rejected++ }
	if _, err := l2.Accept(); err == nil || rejected != 1 {
		t.Fatalf("a remote client must still be capped (err=%v rejected=%d)", err, rejected)
	}
}
