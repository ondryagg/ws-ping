//go:build fixture

package main

import (
	"context"
	"flag"
	"net"
	"sync"
	"time"

	"github.com/ondryadev/ws-ping/probe/internal/app"
)

// fixtureSetup adds failure-injection flags. They exist only in binaries built
// with -tags fixture and are used by the soak and watchdog tests.
func fixtureSetup(fs *flag.FlagSet) func(*app.Options) {
	var delay, hangAfter, hangUpgradeAfter, interval time.Duration
	var maxPings int
	fs.DurationVar(&delay, "test-delay", 0, "TEST ONLY: delay every pong by this long")
	fs.DurationVar(&hangAfter, "test-hang-accept-after", 0, "TEST ONLY: after this long the accept loop blocks forever")
	fs.DurationVar(&hangUpgradeAfter, "test-hang-upgrade-after", 0, "TEST ONLY: after this long every upgrade blocks forever")
	fs.DurationVar(&interval, "test-selftest-interval", 0, "TEST ONLY: self-test period")
	fs.IntVar(&maxPings, "test-max-pings", 0, "TEST ONLY: pings per session, beyond the production limit of 1000")
	return func(o *app.Options) {
		// Make an accidentally deployed fixture image obvious in the startup
		// log, in /info and in `probe version`.
		o.Version += "+fixture"
		if maxPings > 0 {
			o.Cfg.MaxPings = maxPings
		}
		if interval > 0 {
			o.Cfg.SelftestInterval = interval
		}
		if hangUpgradeAfter > 0 {
			at := time.Now().Add(hangUpgradeAfter)
			o.Hooks.OnUpgrade = func(ctx context.Context) {
				if time.Now().Before(at) {
					return
				}
				<-ctx.Done()
			}
		}
		if delay > 0 {
			o.Hooks.OnPing = func(ctx context.Context, _ uint32) {
				t := time.NewTimer(delay)
				defer t.Stop()
				select {
				case <-t.C:
				case <-ctx.Done():
				}
			}
		}
		if hangAfter > 0 {
			deadline := time.Now().Add(hangAfter)
			o.WrapListener = func(l net.Listener) net.Listener {
				return &hangListener{Listener: l, at: deadline, closed: make(chan struct{})}
			}
		}
	}
}

// hangListener stops handing out connections after a point in time while the
// kernel keeps completing TCP handshakes into the backlog, which is exactly
// what a stuck accept loop looks like from outside.
type hangListener struct {
	net.Listener
	at     time.Time
	once   sync.Once
	closed chan struct{}
}

func (h *hangListener) Accept() (net.Conn, error) {
	if time.Now().After(h.at) {
		<-h.closed
		return nil, net.ErrClosed
	}
	c, err := h.Listener.Accept()
	if err == nil && time.Now().After(h.at) {
		_ = c.Close()
		<-h.closed
		return nil, net.ErrClosed
	}
	return c, err
}

func (h *hangListener) Close() error {
	h.once.Do(func() { close(h.closed) })
	return h.Listener.Close()
}
