// Package app wires the listeners, servers and background loops together and
// owns startup and shutdown order.
package app

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ondryadev/ws-ping/probe/internal/config"
	"github.com/ondryadev/ws-ping/probe/internal/crashloop"
	"github.com/ondryadev/ws-ping/probe/internal/diag"
	"github.com/ondryadev/ws-ping/probe/internal/logsum"
	"github.com/ondryadev/ws-ping/probe/internal/metrics"
	"github.com/ondryadev/ws-ping/probe/internal/netx"
	"github.com/ondryadev/ws-ping/probe/internal/server"
	"github.com/ondryadev/ws-ping/probe/internal/tlsutil"
	"github.com/ondryadev/ws-ping/probe/internal/watchdog"
)

// ExitWatchdog is the exit code after a failed self-test.
const ExitWatchdog = 70

// Addrs are the bound listener addresses, reported to Options.OnReady.
type Addrs struct{ Public, Metrics string }

// Options configure Run. Everything except Cfg is optional.
type Options struct {
	Cfg     config.Config
	Version string
	Commit  string
	LogOut  io.Writer // JSON log destination, default os.Stdout
	Stderr  io.Writer // diagnostic bundles, default os.Stderr
	Exit    func(int) // called on a watchdog fatal, default os.Exit
	Hooks   server.Hooks

	WrapListener func(net.Listener) net.Listener // fixture builds only
	OnReady      func(Addrs)
}

var errSNI = errors.New("sni mismatch")

// Run serves until ctx is cancelled, then shuts down gracefully and returns
// the process exit code.
func Run(ctx context.Context, o Options) int {
	cfg := o.Cfg
	if o.LogOut == nil {
		o.LogOut = os.Stdout
	}
	if o.Stderr == nil {
		o.Stderr = os.Stderr
	}
	if o.Exit == nil {
		o.Exit = os.Exit
	}
	logger := slog.New(slog.NewJSONHandler(o.LogOut, &slog.HandlerOptions{Level: cfg.LogLevel}))
	logger.Info("starting", append(cfg.Summary(),
		"version", o.Version, "commit", o.Commit, "memory_limit", debug.SetMemoryLimit(-1))...)

	var m *metrics.Metrics
	rel, err := tlsutil.New(cfg.TLSCert, cfg.TLSKey, cfg.Hostname, logger, func() {
		if m != nil {
			m.CertReloadFailures.Inc()
		}
	})
	if err != nil {
		logger.Error("cannot load TLS certificate", "err", err)
		return 1
	}
	reg := prometheus.NewRegistry()
	m = metrics.New(reg, metrics.Deps{Version: o.Version, Commit: o.Commit, CertExpiry: rel.NotAfter})

	ring := diag.NewRing(50)
	spare := diag.NewSpare(3)
	sum := logsum.New()
	tok := make([]byte, 32)
	if _, err := rand.Read(tok); err != nil {
		logger.Error("cannot read random bytes", "err", err)
		return 1
	}
	token := hex.EncodeToString(tok)

	srv, err := server.New(server.Deps{
		Cfg: cfg, Log: logger, Metrics: m, Now: time.Now, Errors: ring, Summary: sum,
		Token: token, Hooks: o.Hooks, Version: o.Version,
	})
	if err != nil {
		logger.Error("cannot build server", "err", err)
		return 1
	}

	tlsCfg := &tls.Config{
		MinVersion:     tls.VersionTLS12,
		NextProtos:     []string{"http/1.1"},
		GetCertificate: rel.GetCertificate,
		// SNI must be the configured hostname. Returning an error aborts the
		// handshake before any certificate is sent.
		GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
			if !strings.EqualFold(h.ServerName, cfg.Hostname) {
				m.Rejected.WithLabelValues(metrics.ReasonSNI).Inc()
				sum.Inc("reject:" + metrics.ReasonSNI)
				return nil, errSNI
			}
			return nil, nil
		},
	}

	var lc net.ListenConfig
	raw, err := lc.Listen(ctx, "tcp", cfg.Listen)
	if err != nil {
		logger.Error("cannot listen", "addr", cfg.Listen, "err", err)
		return 1
	}
	nl := netx.Wrap(raw, netx.Options{
		MaxConns: cfg.MaxRawConns, MaxPerIP: cfg.MaxRawConnsPerIP, SockBuf: cfg.SockBuf,
		OnReject: func(reason string) {
			m.Rejected.WithLabelValues(reason).Inc()
			sum.Inc("reject:" + reason)
		},
		OnAcceptError: func(class string, err error) {
			if class == "closed" {
				return
			}
			m.AcceptErrors.WithLabelValues(class).Inc()
			sum.Inc("accept:" + class)
			ring.Add(time.Now(), "accept", err.Error())
		},
	})
	var public net.Listener = nl
	if o.WrapListener != nil {
		public = o.WrapListener(public)
	}
	tlsLn := tls.NewListener(public, tlsCfg)

	mln, err := lc.Listen(ctx, "tcp", cfg.MetricsListen)
	if err != nil {
		_ = raw.Close()
		logger.Error("cannot listen for metrics", "addr", cfg.MetricsListen, "err", err)
		return 1
	}

	var http1 http.Protocols
	http1.SetHTTP1(true)
	httpSrv := &http.Server{
		Handler:           srv,
		Protocols:         &http1,
		ReadHeaderTimeout: cfg.HeaderTimeout, // bounds header read; net/http takes the TLS handshake bound from the smaller of this and ReadTimeout
		ReadTimeout:       cfg.IdleTimeout,
		WriteTimeout:      cfg.IdleTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    8 << 10,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelDebug),
	}
	metricsSrv := newMetricsServer(reg, mln.Addr().String(), cfg)

	// Crash-loop guard: too many recent watchdog exits means stay up, degraded.
	if err := os.MkdirAll(cfg.StateDir, 0o750); err != nil {
		logger.Warn("state dir not writable; diagnostic files and the crash-loop guard are limited", "dir", cfg.StateDir, "err", err)
	}
	guard := crashloop.New(cfg.StateDir, time.Now)
	if guard.Tripped() {
		srv.SetDegraded(true)
		m.Crashloop.Set(1)
		logger.Error("crash loop detected: serving 503 until the self-test passes repeatedly",
			"exits_in_window", guard.Count(), "window", crashloop.Window.String())
	}

	dialAddr := localDial(cfg, raw.Addr())
	wcfg := watchdog.Defaults(watchdog.Config{
		Hostname: cfg.Hostname, DialAddr: dialAddr, Origin: cfg.AllowedOrigins[0], Token: token, Roots: rel.Pool,
	})
	wcfg.Interval = cfg.SelftestInterval
	wd := watchdog.New(wcfg, logger, m, time.Now)
	wd.Degraded = srv.Degraded
	wd.OnRecovered = func() {
		srv.SetDegraded(false)
		m.Crashloop.Set(0)
		if err := guard.Reset(); err != nil {
			logger.Warn("resetting crash-loop guard", "err", err)
		}
	}
	// dumpAndStop frees what the dump needs even if the process is out of file
	// descriptors: it stops accepting (which releases the listener's descriptor
	// and stops new sockets from taking freed ones) and hands back the reserve.
	var fatalStarted atomic.Bool
	dumpAndStop := func() {
		_ = raw.Close()
		spare.Release()
	}
	wd.OnFatal = func(reason string, fails int, recent []string) {
		fatalStarted.Store(true)
		dumpAndStop()
		path := diag.Emit(cfg.StateDir, o.Stderr, diag.Info{
			Reason: reason, Time: time.Now(), Version: o.Version, Commit: o.Commit,
			ActiveConns: srv.ActiveSessions(), SelftestFails: fails, SelftestErrors: recent, Errors: ring,
		}, logger)
		if err := guard.Record(); err != nil {
			logger.Warn("recording watchdog exit", "err", err)
		}
		logger.Error("self-test failing, exiting so the container restarts", "diag_file", path, "exit_code", ExitWatchdog)
		o.Exit(ExitWatchdog)
	}
	srv.SetHealth(wd.Healthy)

	if o.OnReady != nil {
		o.OnReady(Addrs{Public: raw.Addr().String(), Metrics: mln.Addr().String()})
	}

	bgCtx, bgCancel := context.WithCancel(context.Background())
	wdCtx, wdCancel := context.WithCancel(context.Background())
	var bg sync.WaitGroup
	start := func(f func()) { bg.Add(1); go func() { defer bg.Done(); f() }() }
	serveErr := make(chan error, 2)

	// Owners below: Run. Exits: bgCtx/wdCtx cancel or the listener closing.
	start(func() { srv.Run(bgCtx) })
	start(func() { sum.Run(bgCtx, logger, 10*time.Second) })
	start(func() { rel.Run(bgCtx, 10*time.Second) })
	start(func() { wd.Run(wdCtx) })
	start(func() { serveErr <- fmt.Errorf("public listener: %w", httpSrv.Serve(tlsLn)) })
	start(func() { serveErr <- fmt.Errorf("metrics listener: %w", metricsSrv.Serve(mln)) })

	code := 0
	select {
	case <-ctx.Done():
		logger.Info("shutdown requested")
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			// The accept loop died. Leave evidence and let Docker restart us.
			// Closing the listener in the watchdog path also lands here; that
			// path writes its own bundle and exits.
			if !fatalStarted.Load() {
				logger.Error("listener stopped unexpectedly", "err", err)
				dumpAndStop()
				diag.Emit(cfg.StateDir, o.Stderr, diag.Info{
					Reason: err.Error(), Time: time.Now(), Version: o.Version, Commit: o.Commit,
					ActiveConns: srv.ActiveSessions(), Errors: ring,
				}, logger)
			}
			code = ExitWatchdog
		}
	}

	// Stop the watchdog first: a self-test against a closing listener would
	// otherwise look like a failure and trigger a restart mid-shutdown.
	wdCancel()
	shCtx, shCancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer shCancel()
	bg.Add(1)
	go func() { defer bg.Done(); _ = httpSrv.Shutdown(shCtx) }() // closes listeners at once
	srv.Shutdown(cfg.ShutdownGrace)
	_ = httpSrv.Close()
	_ = metricsSrv.Close()
	bgCancel()
	bg.Wait()
	return code
}

// localDial is the address the watchdog dials, using the bound port.
func localDial(cfg config.Config, bound net.Addr) string {
	host, _, _ := net.SplitHostPort(cfg.LocalDialAddr())
	_, port, _ := net.SplitHostPort(bound.String())
	return net.JoinHostPort(host, port)
}
