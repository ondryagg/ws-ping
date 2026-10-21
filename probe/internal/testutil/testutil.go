// Package testutil starts the whole service in-process for integration tests.
package testutil

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ondryadev/ws-ping/probe/internal/app"
	"github.com/ondryadev/ws-ping/probe/internal/config"
	"github.com/ondryadev/ws-ping/probe/internal/proto"
	"github.com/ondryadev/ws-ping/probe/internal/testcert"
)

const (
	Hostname = "probe.test"
	Origin   = "https://ondrya.com"
)

// Config returns a valid test configuration on ephemeral loopback ports with
// short, test-friendly values where the spec allows configuration.
func Config(t testing.TB) config.Config {
	t.Helper()
	dir := t.TempDir()
	certFile, keyFile, _ := testcert.Write(t, dir, Hostname, time.Now().Add(24*time.Hour))
	c := config.Defaults()
	c.Hostname = Hostname
	c.Listen = "127.0.0.1:0"
	c.MetricsListen = "127.0.0.1:0"
	c.AllowedOrigins = []string{Origin}
	c.NodeID = "test-01"
	c.Region = "test"
	c.TLSCert, c.TLSKey = certFile, keyFile
	c.StateDir = filepath.Join(dir, "state")
	c.DisableFile = ""
	c.SelftestInterval = 200 * time.Millisecond
	c.MaxConnsPerIP = 50
	c.NewConnsPerMin = 100000
	c.MaxRawConnsPerIP = 100
	c.MaxRawConns = 10000
	c.MaxConns = 5000
	return c
}

// Running is an in-process service.
type Running struct {
	T       testing.TB
	Cfg     config.Config
	Public  string
	Metrics string
	Pool    *x509.CertPool
	Exits   chan int
	Stderr  *SafeBuffer
	Logs    *SafeBuffer

	cancel context.CancelFunc
	done   chan int
	code   int
	once   sync.Once
}

// SafeBuffer is a bytes.Buffer safe for concurrent use.
type SafeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *SafeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *SafeBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// Start runs the service and waits until the first self-test passes, unless
// waitHealthy is false.
func Start(t testing.TB, cfg config.Config, mutate func(*app.Options)) *Running {
	return start(t, cfg, mutate, true)
}

func StartUnchecked(t testing.TB, cfg config.Config, mutate func(*app.Options)) *Running {
	return start(t, cfg, mutate, false)
}

func start(t testing.TB, cfg config.Config, mutate func(*app.Options), waitHealthy bool) *Running {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &Running{T: t, Cfg: cfg, cancel: cancel, done: make(chan int, 1), Exits: make(chan int, 8),
		Stderr: &SafeBuffer{}, Logs: &SafeBuffer{}}
	ready := make(chan app.Addrs, 1)
	opts := app.Options{
		Cfg: cfg, Version: "test", Commit: "test", LogOut: r.Logs, Stderr: r.Stderr,
		Exit:    func(c int) { r.Exits <- c },
		OnReady: func(a app.Addrs) { ready <- a },
	}
	if mutate != nil {
		mutate(&opts)
	}
	go func() { r.done <- app.Run(ctx, opts) }()
	select {
	case a := <-ready:
		r.Public, r.Metrics = a.Public, a.Metrics
	case code := <-r.done:
		t.Fatalf("service exited early with %d: %s", code, r.Logs.String())
	case <-time.After(10 * time.Second):
		t.Fatal("service did not start")
	}
	pem, _ := os.ReadFile(cfg.TLSCert)
	r.Pool = x509.NewCertPool()
	r.Pool.AppendCertsFromPEM(pem)
	t.Cleanup(func() { r.Stop() })
	if waitHealthy {
		r.WaitHealthy(10 * time.Second)
	}
	return r
}

// Stop cancels the service and returns its exit code.
func (r *Running) Stop() int {
	r.once.Do(func() {
		r.cancel()
		select {
		case r.code = <-r.done:
		case <-time.After(30 * time.Second):
			r.T.Error("service did not stop within 30s")
			r.code = -1
		}
	})
	return r.code
}

// Wait returns the exit code once the service has stopped by itself.
func (r *Running) Wait(d time.Duration) (int, bool) {
	select {
	case c := <-r.done:
		r.once.Do(func() { r.code = c })
		return c, true
	case <-time.After(d):
		return 0, false
	}
}

func (r *Running) URL(path string) string {
	_, port, _ := net.SplitHostPort(r.Public)
	return "https://" + net.JoinHostPort(Hostname, port) + path
}

// Client returns an HTTP client that reaches the service at Hostname, bound
// to a source IP if srcIP is not empty.
func (r *Running) Client(srcIP string) *http.Client {
	d := &net.Dialer{Timeout: 5 * time.Second}
	if srcIP != "" {
		d.LocalAddr = &net.TCPAddr{IP: net.ParseIP(srcIP)}
	}
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return d.DialContext(ctx, network, r.Public)
			},
			TLSClientConfig:   &tls.Config{ServerName: Hostname, RootCAs: r.Pool, MinVersion: tls.VersionTLS12},
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// DialOpts tweak a WebSocket dial. The zero value is a well-formed request.
type DialOpts struct {
	Origin       *string // nil means the allowed origin; pointer to "" omits the header
	Subprotocols []string
	NoProtocol   bool
	SrcIP        string
	Client       *http.Client
}

func Str(s string) *string { return &s }

func (r *Running) Dial(ctx context.Context, o DialOpts) (*websocket.Conn, *http.Response, error) {
	hdr := http.Header{}
	switch {
	case o.Origin == nil:
		hdr.Set("Origin", Origin)
	case *o.Origin != "":
		hdr.Set("Origin", *o.Origin)
	}
	sp := o.Subprotocols
	if sp == nil && !o.NoProtocol {
		sp = []string{proto.Subprotocol}
	}
	hc := o.Client
	if hc == nil {
		hc = r.Client(o.SrcIP)
	}
	return websocket.Dial(ctx, r.URL("/ws"), &websocket.DialOptions{
		HTTPClient: hc, HTTPHeader: hdr, Subprotocols: sp, CompressionMode: websocket.CompressionDisabled,
	})
}

// MustDial dials with defaults and reads the hello frame.
func (r *Running) MustDial(t testing.TB, srcIP string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, resp, err := r.Dial(ctx, DialOpts{SrcIP: srcIP})
	if err != nil {
		st := 0
		if resp != nil {
			st = resp.StatusCode
		}
		t.Fatalf("dial: %v (status %d)", err, st)
	}
	_, hello, err := c.Read(ctx)
	if err != nil || !strings.Contains(string(hello), `"t":"hello"`) {
		t.Fatalf("hello: %q %v", hello, err)
	}
	return c
}

// Ping sends one ping and returns the reply text.
func Ping(ctx context.Context, c *websocket.Conn, seq uint32) (string, error) {
	if err := c.Write(ctx, websocket.MessageText, fmt.Appendf(nil, `{"t":"ping","seq":%d}`, seq)); err != nil {
		return "", err
	}
	_, b, err := c.Read(ctx)
	return string(b), err
}

func (r *Running) WaitHealthy(d time.Duration) {
	r.T.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if r.Status("/healthz", "") == 200 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	r.T.Fatalf("service not healthy within %v; logs: %s", d, r.Logs.String())
}

// Status performs GET path and returns the HTTP status (0 on error).
func (r *Running) Status(path, srcIP string) int {
	resp, err := r.Client(srcIP).Get(r.URL(path))
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// Metric returns the value of the first series whose line starts with prefix
// (name or name{labels}), or -1 if absent.
func (r *Running) Metric(prefix string) float64 {
	resp, err := http.Get("http://" + r.Metrics + "/metrics")
	if err != nil {
		r.T.Fatalf("metrics: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, prefix+" ") {
			v, _ := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, prefix)), 64)
			return v
		}
	}
	return -1
}

// Eventually polls f until it returns true.
func Eventually(t testing.TB, d time.Duration, msg string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %v: %s", d, msg)
}
