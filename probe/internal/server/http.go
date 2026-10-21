package server

import (
	"net/http"

	"github.com/ondryadev/ws-ping/probe/internal/metrics"
)

// unavailable answers 503 while the kill switch or degraded mode is active.
func (s *Server) unavailable(w http.ResponseWriter) bool {
	switch {
	case s.degraded.Load():
		s.reject(w, http.StatusServiceUnavailable, metrics.ReasonDegraded)
	case s.kill.Active():
		s.reject(w, http.StatusServiceUnavailable, metrics.ReasonMaintenance)
	default:
		return false
	}
	return true
}

// cors applies the allowed-origin headers. A request without an Origin gets
// none. It returns false after writing a 403 for a present but unknown or
// duplicated Origin; that response carries no CORS headers. Vary: Origin is set
// on every answer so a shared cache never serves one origin's headers to another.
func (s *Server) cors(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Add("Vary", "Origin")
	vals := r.Header.Values("Origin")
	if len(vals) == 0 {
		return true
	}
	if len(vals) > 1 || !s.origins.Allowed(vals[0]) {
		s.reject(w, http.StatusForbidden, metrics.ReasonOrigin)
		return false
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", vals[0])
	h.Set("Timing-Allow-Origin", vals[0])
	return true
}

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	preflight := r.Method == http.MethodOptions
	if r.Method != http.MethodGet && !preflight {
		w.Header().Set("Allow", "GET, OPTIONS")
		s.reject(w, http.StatusMethodNotAllowed, metrics.ReasonMethod)
		return
	}
	key, internal, ok := s.client(r)
	if !ok {
		s.reject(w, http.StatusBadRequest, metrics.ReasonBadRequest)
		return
	}
	if !internal {
		if s.unavailable(w) {
			return
		}
		if !s.pingRL.Allow(key) {
			s.reject(w, http.StatusTooManyRequests, metrics.ReasonRateLimit)
			return
		}
	}
	if !s.cors(w, r) {
		return
	}
	if preflight {
		h := w.Header()
		h.Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		h.Set("Access-Control-Max-Age", "86400")
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		s.reject(w, http.StatusMethodNotAllowed, metrics.ReasonMethod)
		return
	}
	key, internal, ok := s.client(r)
	if !ok {
		s.reject(w, http.StatusBadRequest, metrics.ReasonBadRequest)
		return
	}
	if !internal && !s.pingRL.Allow(key) {
		s.reject(w, http.StatusTooManyRequests, metrics.ReasonRateLimit)
		return
	}
	if !s.cors(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(s.info)
}

// handleHealthz is 200 only while the self-test is passing and the node is
// not degraded. It stays available in maintenance mode.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		s.reject(w, http.StatusMethodNotAllowed, metrics.ReasonMethod)
		return
	}
	healthy := false
	if f := s.health.Load(); f != nil {
		healthy = (*f)()
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if !healthy || s.degraded.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("unhealthy\n"))
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}
