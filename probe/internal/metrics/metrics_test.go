package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestEverySpecSeriesIsRegistered(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg, Deps{Version: "v1", Commit: "abc", CertExpiry: func() time.Time { return time.Now().Add(time.Hour) }})
	m.ConnsTotal.WithLabelValues(ResultCompleted).Inc()
	m.AcceptErrors.WithLabelValues("emfile").Inc()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, f := range families {
		have[f.GetName()] = true
	}
	for _, name := range strings.Fields(`probe_connections_active probe_connections_total probe_rejected_total
		probe_session_duration_seconds probe_pings_total probe_read_timeouts_total probe_write_timeouts_total
		probe_panics_total probe_accept_errors_total probe_goroutines probe_open_fds probe_fd_limit probe_memory_bytes
		probe_selftest_ok probe_selftest_failures_consecutive probe_cert_expiry_seconds probe_build_info probe_crashloop`) {
		if !have[name] {
			t.Errorf("missing series %s", name)
		}
	}
	if got := testutil.ToFloat64(m.Rejected.WithLabelValues(ReasonOrigin)); got != 0 {
		t.Fatalf("pre-created reason series must start at 0, got %v", got)
	}
}
