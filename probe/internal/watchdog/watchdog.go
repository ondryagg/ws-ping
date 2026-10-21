// Package watchdog is the self-healing mechanism. Every interval it runs a
// real round trip against the process's own public listener, through the same
// accept, TLS, upgrade and echo path visitors use. After enough consecutive
// failures it reports fatal so the process can dump evidence and exit, and
// Docker's restart policy brings it back.
package watchdog

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/ondryadev/ws-ping/probe/internal/metrics"
	"github.com/ondryadev/ws-ping/probe/internal/proto"
	"github.com/ondryadev/ws-ping/probe/internal/server"
)

const maxRecentErrors = 10

type Config struct {
	Hostname     string
	DialAddr     string // loopback address of the public listener
	Origin       string // one allowed origin, sent as the Origin header
	Token        string // server.SelftestHeader secret
	Roots        func() *x509.CertPool
	Interval     time.Duration // 10 s
	DialTimeout  time.Duration // TCP, TLS and upgrade
	PongTimeout  time.Duration // 2 s
	Threshold    int           // consecutive failures before fatal, 3
	MaxAge       time.Duration // /healthz is healthy while the last pass is newer, 30 s
	RecoverAfter int           // consecutive passes that clear degraded mode
}

// Defaults fills the spec values.
func Defaults(c Config) Config {
	c.Interval = 10 * time.Second
	c.DialTimeout = 3 * time.Second
	c.PongTimeout = 2 * time.Second
	c.Threshold = 3
	c.MaxAge = 30 * time.Second
	c.RecoverAfter = 6
	return c
}

type Watchdog struct {
	cfg Config
	log *slog.Logger
	m   *metrics.Metrics
	now func() time.Time

	// OnFatal runs once after Threshold consecutive failures unless the node is
	// degraded. Production writes the bundle and exits 70.
	OnFatal func(reason string, failures int, recent []string)
	// Degraded and OnRecovered implement crash-loop recovery.
	Degraded    func() bool
	OnRecovered func()

	lastOK atomic.Int64

	// Only the Run goroutine touches these.
	fails, oks int
	fired      bool

	mu     sync.Mutex // guards recent
	recent []string

	seq atomic.Uint32
}

func New(cfg Config, log *slog.Logger, m *metrics.Metrics, now func() time.Time) *Watchdog {
	return &Watchdog{cfg: cfg, log: log, m: m, now: now}
}

// Healthy backs /healthz: the last pass must be recent.
func (w *Watchdog) Healthy() bool {
	last := w.lastOK.Load()
	return last != 0 && w.now().Sub(time.Unix(0, last)) <= w.cfg.MaxAge
}

// verifyServed checks the served chain and hostname against the current roots.
func (w *Watchdog) verifyServed(cs tls.ConnectionState) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("server presented no certificate")
	}
	inter := x509.NewCertPool()
	for _, c := range cs.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
		DNSName: w.cfg.Hostname, Roots: w.cfg.Roots(), Intermediates: inter,
	})
	return err
}

// Once performs one self-test round trip.
func (w *Watchdog) Once(ctx context.Context) error {
	hc := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, w.cfg.DialAddr)
			},
			TLSClientConfig: &tls.Config{
				ServerName: w.cfg.Hostname,
				MinVersion: tls.VersionTLS12,
				// Verified by hand against the trust pool as it is when the handshake
				// finishes, not when the dial started: a renewal landing in between
				// would otherwise fail a perfectly good self-test.
				InsecureSkipVerify: true, //nolint:gosec // replaced by VerifyConnection below
				VerifyConnection:   w.verifyServed,
			},
			DisableKeepAlives: true,
		},
	}
	defer hc.CloseIdleConnections()

	_, port, _ := net.SplitHostPort(w.cfg.DialAddr)
	dctx, cancel := context.WithTimeout(ctx, w.cfg.DialTimeout)
	defer cancel()
	hdr := http.Header{}
	hdr.Set("Origin", w.cfg.Origin)
	hdr.Set(server.SelftestHeader, w.cfg.Token)
	c, _, err := websocket.Dial(dctx, //nolint:bodyclose // the library owns the body after a successful upgrade
		"wss://"+net.JoinHostPort(w.cfg.Hostname, port)+"/ws", &websocket.DialOptions{
			HTTPClient:      hc,
			HTTPHeader:      hdr,
			Subprotocols:    []string{proto.Subprotocol},
			CompressionMode: websocket.CompressionDisabled,
		})
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = c.CloseNow() }()
	c.SetReadLimit(256)

	rctx, rcancel := context.WithTimeout(ctx, w.cfg.PongTimeout)
	defer rcancel()
	_, helloMsg, err := c.Read(rctx)
	if err != nil {
		return fmt.Errorf("read hello: %w", err)
	}
	var hello struct {
		T     string `json:"t"`
		Proto int    `json:"proto"`
	}
	if err := json.Unmarshal(helloMsg, &hello); err != nil || hello.T != "hello" || hello.Proto != proto.Version {
		return errors.New("unexpected hello frame")
	}
	seq := w.seq.Add(1)
	ping := fmt.Appendf(nil, `{"t":"ping","seq":%d}`, seq)
	if err := c.Write(rctx, websocket.MessageText, ping); err != nil {
		return fmt.Errorf("write ping: %w", err)
	}
	_, pong, err := c.Read(rctx)
	if err != nil {
		return fmt.Errorf("read pong: %w", err)
	}
	if want := proto.AppendPong(nil, seq); string(pong) != string(want) {
		return errors.New("pong does not match ping")
	}
	return nil
}

// Run loops until ctx ends. Owner: the caller; exit: ctx.Done. A pass that
// was cut short by shutdown is not counted as a failure.
func (w *Watchdog) Run(ctx context.Context) {
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	for {
		err := w.Once(ctx)
		if ctx.Err() != nil {
			return
		}
		w.record(err)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (w *Watchdog) record(err error) {
	if err == nil {
		w.lastOK.Store(w.now().UnixNano())
		w.fails = 0
		w.oks++
		w.m.SelftestOK.Set(1)
		w.m.SelftestFailures.Set(0)
		if w.oks >= w.cfg.RecoverAfter && w.Degraded != nil && w.Degraded() && w.OnRecovered != nil {
			w.log.Info("self-test healthy again, leaving degraded mode", "passes", w.oks)
			w.OnRecovered()
		}
		return
	}
	w.oks = 0
	w.fails++
	w.m.SelftestOK.Set(0)
	w.m.SelftestFailures.Set(float64(w.fails))
	msg := w.now().UTC().Format(time.RFC3339) + " " + err.Error()
	w.mu.Lock()
	w.recent = append(w.recent, msg)
	if len(w.recent) > maxRecentErrors {
		w.recent = w.recent[1:]
	}
	recent := append([]string(nil), w.recent...)
	w.mu.Unlock()
	w.log.Warn("self-test failed", "consecutive", w.fails, "err", err)

	if w.fails >= w.cfg.Threshold && !w.fired && (w.Degraded == nil || !w.Degraded()) && w.OnFatal != nil {
		w.fired = true
		w.OnFatal(fmt.Sprintf("%d consecutive self-test failures: %v", w.fails, err), w.fails, recent)
	}
}
