// Package netx wraps the raw TCP listener. It enforces connection caps before
// any TLS work is spent, bounds per-socket kernel buffers, and reports accept
// errors so a stuck accept loop leaves evidence.
package netx

import (
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/ondryadev/ws-ping/probe/internal/clientip"
	"github.com/ondryadev/ws-ping/probe/internal/limiter"
	"github.com/ondryadev/ws-ping/probe/internal/metrics"
)

// Options configure the wrapper.
type Options struct {
	MaxConns int // open sockets, all clients
	MaxPerIP int // open sockets per client IP (/64 for IPv6)
	SockBuf  int // SO_SNDBUF and SO_RCVBUF, 0 for kernel default

	OnReject      func(reason string)
	OnAcceptError func(class string, err error)
}

// Listener is the accounting listener.
type Listener struct {
	net.Listener
	o     Options
	total atomic.Int64
	perIP *limiter.Counter
}

func Wrap(l net.Listener, o Options) *Listener {
	return &Listener{Listener: l, o: o, perIP: limiter.NewCounter(o.MaxPerIP)}
}

// Open is the number of tracked (non-loopback) sockets.
func (l *Listener) Open() int64 { return l.total.Load() }

// Accept returns the next admitted connection. Refused sockets are closed
// immediately and the loop continues; only listener errors are returned.
// Local peers (loopback, or a connection from this host's own address: the
// self-test and health check) are exempt from the caps so
// the watchdog is not starved by a flood.
func (l *Listener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			if l.o.OnAcceptError != nil {
				l.o.OnAcceptError(ClassifyAcceptError(err), err)
			}
			return nil, err
		}
		ta, ok := c.RemoteAddr().(*net.TCPAddr)
		if !ok {
			return c, nil
		}
		addr, _ := netip.AddrFromSlice(ta.IP)
		addr = addr.Unmap()
		var local netip.Addr
		if la, ok := c.LocalAddr().(*net.TCPAddr); ok {
			local, _ = netip.AddrFromSlice(la.IP)
		}
		if clientip.IsLocal(addr, local) {
			l.tune(c)
			return c, nil
		}
		key := clientip.Key(addr)
		if l.total.Load() >= int64(l.o.MaxConns) {
			l.refuse(c, metrics.ReasonRawCapacity)
			continue
		}
		if !l.perIP.TryAcquire(key) {
			l.refuse(c, metrics.ReasonRawPerIP)
			continue
		}
		l.total.Add(1)
		l.tune(c)
		return &trackedConn{Conn: c, l: l, key: key}, nil
	}
}

func (l *Listener) refuse(c net.Conn, reason string) {
	_ = c.Close()
	if l.o.OnReject != nil {
		l.o.OnReject(reason)
	}
}

func (l *Listener) tune(c net.Conn) {
	if l.o.SockBuf <= 0 {
		return
	}
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(l.o.SockBuf)
		_ = tc.SetWriteBuffer(l.o.SockBuf)
	}
}

type trackedConn struct {
	net.Conn
	l    *Listener
	key  netip.Addr
	once sync.Once
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		c.l.perIP.Release(c.key)
		c.l.total.Add(-1)
	})
	return err
}

// ClassifyAcceptError maps an accept error to a small fixed label set.
func ClassifyAcceptError(err error) string {
	switch {
	case errors.Is(err, syscall.EMFILE):
		return "emfile"
	case errors.Is(err, syscall.ENFILE):
		return "enfile"
	case errors.Is(err, net.ErrClosed):
		return "closed"
	case errors.Is(err, syscall.ENOBUFS), errors.Is(err, syscall.ENOMEM):
		return "nomem"
	case errors.Is(err, syscall.ECONNABORTED), errors.Is(err, syscall.ECONNRESET):
		return "aborted"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	return "other"
}
