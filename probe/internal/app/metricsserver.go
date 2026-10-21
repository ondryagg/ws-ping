package app

import (
	"net"
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ondryadev/ws-ping/probe/internal/config"
)

// newMetricsServer serves /metrics and /debug/pprof/ on the loopback
// listener only. Requests whose Host is not a loopback name are refused, which
// stops a web page from reaching it through DNS rebinding.
func newMetricsServer(reg *prometheus.Registry, boundAddr string, cfg config.Config) *http.Server {
	_, port, _ := net.SplitHostPort(boundAddr)
	allowed := map[string]bool{
		net.JoinHostPort("127.0.0.1", port): true,
		net.JoinHostPort("::1", port):       true,
		net.JoinHostPort("localhost", port): true,
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{MaxRequestsInFlight: 4, Timeout: 10 * time.Second}))
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)

	guard := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A page in a browser on this host can send a simple GET with a
		// loopback Host. Browsers label such requests; refuse anything that is
		// not same-origin or a direct (non-browser) request.
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		if r.Header.Get("Origin") != "" {
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		if !allowed[r.Host] {
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
	return &http.Server{
		Handler:           guard,
		ReadHeaderTimeout: cfg.HeaderTimeout,
		ReadTimeout:       cfg.IdleTimeout,
		WriteTimeout:      2 * time.Minute, // pprof profiles run up to 30 s by default
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    8 << 10,
	}
}
