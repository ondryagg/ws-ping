// Package server implements the public HTTP and WebSocket handlers.
//
// Concurrency model: one goroutine per WebSocket session owns its connection
// and is the only reader and writer. There is no hub, no write channel and no
// shared per-session state. Locks only guard counters and are never held
// across I/O. Every goroutine started here has an owner and an exit path
// noted next to it.
package server

import (
	"bufio"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ondryadev/ws-ping/probe/internal/check"
	"github.com/ondryadev/ws-ping/probe/internal/clientip"
	"github.com/ondryadev/ws-ping/probe/internal/config"
	"github.com/ondryadev/ws-ping/probe/internal/diag"
	"github.com/ondryadev/ws-ping/probe/internal/limiter"
	"github.com/ondryadev/ws-ping/probe/internal/logsum"
	"github.com/ondryadev/ws-ping/probe/internal/metrics"
	"github.com/ondryadev/ws-ping/probe/internal/proto"
)

// SelftestHeader carries the per-process secret that marks the watchdog's
// own loopback connections. The secret is random per process, never logged,
// and only honoured from a loopback peer.
const SelftestHeader = "X-Probe-Selftest"

// failsafeSlack is how far a context deadline trails the matching net.Conn
// deadline. The net deadline fires first and lets the session send a proper
// close frame; the context is the backstop.
const failsafeSlack = 500 * time.Millisecond

// Hooks are test seams. They are nil in production; only the fixture build
// or tests set them.
type Hooks struct {
	// OnPing runs after a ping is parsed and before the pong is written.
	OnPing func(ctx context.Context, seq uint32)
	// OnUpgrade runs after the pre-upgrade checks pass and before the upgrade.
	// The fixture build uses it to simulate a hung upgrade path.
	OnUpgrade func(ctx context.Context)
}

// Deps are the collaborators of a Server.
type Deps struct {
	Cfg     config.Config
	Log     *slog.Logger
	Metrics *metrics.Metrics
	Now     limiter.Clock // limiter and hello timestamps; deadlines use the real clock
	Errors  *diag.Ring
	Summary *logsum.Summarizer
	Token   string // self-test secret, empty disables the bypass
	Hooks   Hooks
	Version string
}

type Server struct {
	cfg     config.Config
	log     *slog.Logger
	m       *metrics.Metrics
	now     limiter.Clock
	errs    *diag.Ring
	sum     *logsum.Summarizer
	token   []byte
	hooks   Hooks
	resolve clientip.Resolver
	origins check.OriginSet
	hello   proto.HelloEncoder
	info    []byte

	sem     *limiter.Semaphore
	perIP   *limiter.Counter
	newConn *limiter.Buckets
	pingRL  *limiter.Buckets
	kill    *killSwitch

	health   atomic.Pointer[func() bool]
	degraded atomic.Bool

	base       context.Context
	cancelBase context.CancelFunc

	mu      sync.Mutex // guards closing and the Add side of wg only
	closing bool
	wg      sync.WaitGroup
	active  atomic.Int64

	lastPanicLog atomic.Int64
}

// maxBucketKeys bounds the per-IP rate tables (memory about 100 bytes each).
const maxBucketKeys = 100_000

func New(d Deps) (*Server, error) {
	cfg := d.Cfg
	infoJSON, err := json.Marshal(struct {
		Node    string `json:"node"`
		Region  string `json:"region"`
		Version string `json:"version"`
		Proto   int    `json:"proto"`
	}{cfg.NodeID, cfg.Region, d.Version, proto.Version})
	if err != nil {
		return nil, fmt.Errorf("encode /info: %w", err)
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	base, cancel := context.WithCancel(context.Background())
	s := &Server{
		cfg: cfg, log: d.Log, m: d.Metrics, now: d.Now, errs: d.Errors, sum: d.Summary,
		token: []byte(d.Token), hooks: d.Hooks,
		resolve: clientip.Resolver{Trusted: cfg.TrustedProxies},
		origins: check.NewOriginSet(cfg.AllowedOrigins),
		hello:   proto.NewHelloEncoder(cfg.NodeID, cfg.MaxPings),
		info:    infoJSON,
		sem:     limiter.NewSemaphore(cfg.MaxConns),
		perIP:   limiter.NewCounter(cfg.MaxConnsPerIP),
		newConn: limiter.NewBuckets(float64(cfg.NewConnsPerMin)/60, cfg.NewConnsPerMin, maxBucketKeys, d.Now),
		pingRL:  limiter.NewBuckets(cfg.PingRate, cfg.PingBurst, maxBucketKeys, d.Now),
		kill:    newKillSwitch(cfg.Maintenance, cfg.DisableFile, d.Now),
		base:    base, cancelBase: cancel,
	}
	return s, nil
}

// SetHealth installs the function /healthz consults (the self-test result).
func (s *Server) SetHealth(f func() bool) { s.health.Store(&f) }

// SetDegraded switches crash-loop degraded mode: /ws and /ping answer 503 and
// /healthz reports unhealthy.
func (s *Server) SetDegraded(v bool) { s.degraded.Store(v) }
func (s *Server) Degraded() bool     { return s.degraded.Load() }

// ActiveSessions is the number of in-flight WebSocket sessions, including
// the watchdog's own.
func (s *Server) ActiveSessions() int64 { return s.active.Load() }

// Run sweeps the rate tables until ctx ends. Owner: caller; exit: ctx.Done.
func (s *Server) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.newConn.Run(ctx, 10*time.Second) }()
	go func() { defer wg.Done(); s.pingRL.Run(ctx, 10*time.Second) }()
	wg.Wait()
}

// Shutdown stops admitting sessions, waits up to grace for the rest, then
// cancels them and waits for the close handshake bound.
func (s *Server) Shutdown(grace time.Duration) {
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()

	done := make(chan struct{})
	// Owner: Shutdown. Exit: wg.Wait returns, which every session guarantees
	// within its lifetime plus close timeout.
	go func() { s.wg.Wait(); close(done) }()

	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case <-done:
		return
	case <-t.C:
	}
	s.cancelBase()
	t2 := time.NewTimer(s.cfg.CloseTimeout + failsafeSlack)
	defer t2.Stop()
	select {
	case <-done:
	case <-t2.C:
		s.log.Warn("sessions still open after forced shutdown", "active", s.active.Load())
	}
}

// Close cancels everything immediately (tests, fatal exit).
func (s *Server) Close() { s.cancelBase() }

func (s *Server) beginSession() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return false
	}
	s.wg.Add(1)
	s.active.Add(1)
	return true
}

func (s *Server) endSession() {
	s.active.Add(-1)
	s.wg.Done()
}

// hijackWriter lets the server keep a handle on the raw connection that the
// WebSocket library takes over, so deadlines and a hard close stay possible.
type hijackWriter struct {
	http.ResponseWriter
	conn net.Conn
}

func (h *hijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := h.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	c, rw, err := hj.Hijack()
	if err == nil {
		if tc, ok := c.(*tls.Conn); ok {
			c = &hardCloseConn{Conn: tc}
		}
		h.conn = c
	}
	return c, rw, err
}

// hardCloseConn closes the TCP socket without the TLS close_notify alert.
// tls.Conn.Close resets the write deadline to 5 s to send that alert, which
// would let a peer that never reads keep a finished session's slot for up to 5
// s past the 2 s close bound. The WebSocket close handshake has already run at
// the application layer by the time the socket is closed.
type hardCloseConn struct{ *tls.Conn }

func (c *hardCloseConn) Close() error { return c.NetConn().Close() }

func (h *hijackWriter) Unwrap() http.ResponseWriter { return h.ResponseWriter }

// ServeHTTP routes. The cheap, ordered checks for /ws live in handleWS.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hw := &hijackWriter{ResponseWriter: w}
	defer s.recoverPanic(hw)

	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")

	if r.URL.Path == "/ws" {
		s.handleWS(hw, r)
		return
	}
	if !check.HostMatches(r.Host, s.cfg.Hostname) {
		s.reject(w, http.StatusMisdirectedRequest, metrics.ReasonHost)
		return
	}
	switch r.URL.Path {
	case "/ping":
		s.handlePing(w, r)
	case "/info":
		s.handleInfo(w, r)
	case "/healthz":
		s.handleHealthz(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) reject(w http.ResponseWriter, status int, reason string) {
	s.m.Rejected.WithLabelValues(reason).Inc()
	s.sum.Inc("reject:" + reason)
	h := w.Header()
	h.Set("Connection", "close")
	switch status {
	case http.StatusTooManyRequests:
		h.Set("Retry-After", "1")
	case http.StatusServiceUnavailable:
		h.Set("Retry-After", "30")
	}
	http.Error(w, http.StatusText(status), status)
}

// recoverPanic is deferred first in ServeHTTP, so every deferred cleanup in
// the handlers (slot release, counters) has already run when it executes.
func (s *Server) recoverPanic(hw *hijackWriter) {
	rec := recover()
	if rec == nil {
		return
	}
	s.m.Panics.Inc()
	s.sum.Inc("panic")
	s.errs.Add(s.now(), "panic", fmt.Sprint(rec))
	// Full stack at most once a minute; the counter and summary cover the rest.
	if now := time.Now().UnixNano(); now-s.lastPanicLog.Load() > int64(time.Minute) {
		s.lastPanicLog.Store(now)
		s.log.Error("recovered panic in handler", "panic", fmt.Sprint(rec), "stack", string(debug.Stack()))
	}
	if hw.conn != nil {
		_ = hw.conn.Close()
		return
	}
	func() {
		defer func() { _ = recover() }()
		http.Error(hw, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	}()
}

// client returns the peer, the limiting key, and whether the request came
// from the watchdog.
func (s *Server) client(r *http.Request) (key netip.Addr, internal, ok bool) {
	peer, cl, ok := s.resolve.Client(r.RemoteAddr, r.Header.Values("X-Forwarded-For"))
	if !ok {
		return netip.Addr{}, false, false
	}
	var local netip.Addr
	if la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		if ap, err := netip.ParseAddrPort(la.String()); err == nil {
			local = ap.Addr()
		}
	}
	internal = len(s.token) > 0 && clientip.IsLocal(peer, local) &&
		subtle.ConstantTimeCompare([]byte(r.Header.Get(SelftestHeader)), s.token) == 1
	return clientip.Key(cl), internal, true
}
