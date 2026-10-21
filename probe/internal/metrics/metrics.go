// Package metrics defines every Prometheus series the probe exports. The
// registry is injected, never the global default.
package metrics

import (
	"runtime"
	rm "runtime/metrics"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ondryadev/ws-ping/probe/internal/sysinfo"
)

// Reject reasons. Fixed set, so label cardinality is bounded.
const (
	ReasonCapacity    = "capacity"
	ReasonPerIP       = "per_ip"
	ReasonHost        = "host"
	ReasonOrigin      = "origin"
	ReasonSubprotocol = "subprotocol"
	ReasonRateLimit   = "rate_limit"
	ReasonMaintenance = "maintenance"
	ReasonMethod      = "method"
	ReasonSNI         = "sni"
	ReasonDegraded    = "degraded"
	ReasonRawCapacity = "raw_capacity"
	ReasonRawPerIP    = "raw_per_ip"
	ReasonBadRequest  = "bad_request"
)

// Session results for probe_connections_total.
const (
	ResultCompleted    = "completed"
	ResultLifetime     = "lifetime"
	ResultIdle         = "idle"
	ResultProtocol     = "protocol_error"
	ResultTooBig       = "too_big"
	ResultClientClosed = "client_closed"
	ResultWriteTimeout = "write_timeout"
	ResultShutdown     = "shutdown"
	ResultError        = "error"
	ResultPanic        = "panic"
	ResultUpgradeFail  = "upgrade_failed"
)

type Metrics struct {
	Reg *prometheus.Registry

	ConnsActive        prometheus.Gauge
	ConnsTotal         *prometheus.CounterVec
	Rejected           *prometheus.CounterVec
	SessionDuration    prometheus.Histogram
	Pings              prometheus.Counter
	ReadTimeouts       prometheus.Counter
	WriteTimeouts      prometheus.Counter
	Panics             prometheus.Counter
	AcceptErrors       *prometheus.CounterVec
	SelftestOK         prometheus.Gauge
	SelftestFailures   prometheus.Gauge
	Crashloop          prometheus.Gauge
	CertReloadFailures prometheus.Counter
	StartTime          prometheus.Gauge
}

// Deps are values only the caller knows.
type Deps struct {
	Version, Commit string
	CertExpiry      func() time.Time
}

func New(reg *prometheus.Registry, d Deps) *Metrics {
	m := &Metrics{
		Reg:         reg,
		ConnsActive: prometheus.NewGauge(prometheus.GaugeOpts{Name: "probe_connections_active", Help: "WebSocket sessions currently open."}),
		ConnsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "probe_connections_total",
			Help: "Finished WebSocket sessions by result."}, []string{"result"}),
		Rejected: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "probe_rejected_total",
			Help: "Requests refused, by reason."}, []string{"reason"}),
		SessionDuration: prometheus.NewHistogram(prometheus.HistogramOpts{Name: "probe_session_duration_seconds",
			Help: "WebSocket session length.", Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 15, 20, 25}}),
		Pings:         prometheus.NewCounter(prometheus.CounterOpts{Name: "probe_pings_total", Help: "Pings answered."}),
		ReadTimeouts:  prometheus.NewCounter(prometheus.CounterOpts{Name: "probe_read_timeouts_total", Help: "Sessions closed for idling."}),
		WriteTimeouts: prometheus.NewCounter(prometheus.CounterOpts{Name: "probe_write_timeouts_total", Help: "Writes that hit the write deadline."}),
		Panics:        prometheus.NewCounter(prometheus.CounterOpts{Name: "probe_panics_total", Help: "Recovered handler panics."}),
		AcceptErrors: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "probe_accept_errors_total",
			Help: "Listener accept errors by class."}, []string{"error"}),
		SelftestOK:       prometheus.NewGauge(prometheus.GaugeOpts{Name: "probe_selftest_ok", Help: "1 if the last self-test passed."}),
		SelftestFailures: prometheus.NewGauge(prometheus.GaugeOpts{Name: "probe_selftest_failures_consecutive", Help: "Consecutive failed self-tests."}),
		Crashloop:        prometheus.NewGauge(prometheus.GaugeOpts{Name: "probe_crashloop", Help: "1 while degraded because of repeated watchdog restarts."}),
		StartTime:        prometheus.NewGauge(prometheus.GaugeOpts{Name: "probe_start_time_seconds", Help: "Unix time the process started; a change means a restart."}),
		CertReloadFailures: prometheus.NewCounter(prometheus.CounterOpts{Name: "probe_cert_reload_failures_total",
			Help: "Certificate reloads that failed and kept the old certificate."}),
	}
	reg.MustRegister(m.ConnsActive, m.ConnsTotal, m.Rejected, m.SessionDuration, m.Pings, m.ReadTimeouts,
		m.WriteTimeouts, m.Panics, m.AcceptErrors, m.SelftestOK, m.SelftestFailures, m.Crashloop, m.CertReloadFailures, m.StartTime)
	m.StartTime.Set(float64(time.Now().Unix()))

	reg.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "probe_goroutines", Help: "Live goroutines."},
			func() float64 { return float64(runtime.NumGoroutine()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "probe_open_fds", Help: "Open file descriptors."},
			func() float64 {
				n, err := sysinfo.OpenFDs()
				if err != nil {
					return -1
				}
				return float64(n)
			}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "probe_fd_limit", Help: "Soft file descriptor limit."},
			func() float64 {
				n, err := sysinfo.FDLimit()
				if err != nil {
					return -1
				}
				return float64(n)
			}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "probe_memory_bytes", Help: "Total memory mapped by the Go runtime."},
			func() float64 {
				s := []rm.Sample{{Name: "/memory/classes/total:bytes"}}
				rm.Read(s)
				if s[0].Value.Kind() != rm.KindUint64 {
					return -1
				}
				return float64(s[0].Value.Uint64())
			}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "probe_cert_expiry_seconds", Help: "Seconds until the served certificate expires."},
			func() float64 {
				if d.CertExpiry == nil {
					return -1
				}
				return time.Until(d.CertExpiry()).Seconds()
			}),
	)
	bi := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "probe_build_info", Help: "Build information."}, []string{"version", "commit"})
	bi.WithLabelValues(d.Version, d.Commit).Set(1)
	reg.MustRegister(bi)

	for _, r := range []string{ReasonCapacity, ReasonPerIP, ReasonHost, ReasonOrigin, ReasonSubprotocol, ReasonRateLimit,
		ReasonMaintenance, ReasonMethod, ReasonSNI, ReasonDegraded, ReasonRawCapacity, ReasonRawPerIP, ReasonBadRequest} {
		m.Rejected.WithLabelValues(r)
	}
	return m
}
