package watchdog

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ondryadev/ws-ping/probe/internal/metrics"
)

func newWD(t *testing.T, now *time.Time) *Watchdog {
	t.Helper()
	m := metrics.New(prometheus.NewRegistry(), metrics.Deps{})
	cfg := Defaults(Config{})
	return New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), m, func() time.Time { return *now })
}

func TestFatalAfterThreeConsecutiveFailuresExactlyOnce(t *testing.T) {
	now := time.Unix(1000, 0)
	w := newWD(t, &now)
	fired := 0
	var recent []string
	w.OnFatal = func(_ string, fails int, r []string) { fired++; recent = r; _ = fails }
	bad := errors.New("dial: timeout")
	w.record(bad)
	w.record(bad)
	w.record(nil) // a pass resets the streak
	w.record(bad)
	w.record(bad)
	if fired != 0 {
		t.Fatal("must not fire before 3 in a row")
	}
	w.record(bad)
	w.record(bad)
	if fired != 1 || len(recent) == 0 {
		t.Fatalf("fired %d times", fired)
	}
}

func TestHealthyOnlyWithinThirtySecondsOfAPass(t *testing.T) {
	now := time.Unix(1000, 0)
	w := newWD(t, &now)
	if w.Healthy() {
		t.Fatal("unhealthy before the first pass")
	}
	w.record(nil)
	if !w.Healthy() {
		t.Fatal("healthy right after a pass")
	}
	now = now.Add(29 * time.Second)
	if !w.Healthy() {
		t.Fatal("still healthy at 29 s")
	}
	now = now.Add(2 * time.Second)
	if w.Healthy() {
		t.Fatal("unhealthy after 30 s without a pass")
	}
}

func TestDegradedNeverExitsAndRecoversAfterSixPasses(t *testing.T) {
	now := time.Unix(1000, 0)
	w := newWD(t, &now)
	degraded := true
	recovered := false
	w.Degraded = func() bool { return degraded }
	w.OnFatal = func(string, int, []string) { t.Fatal("must not exit while degraded") }
	w.OnRecovered = func() { degraded = false; recovered = true }
	for i := 0; i < 10; i++ {
		w.record(errors.New("x"))
	}
	for i := 0; i < 5; i++ {
		w.record(nil)
	}
	if recovered {
		t.Fatal("five passes are not enough")
	}
	w.record(nil)
	if !recovered {
		t.Fatal("six consecutive passes clear degraded mode")
	}
}
