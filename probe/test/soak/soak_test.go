//go:build soak

package soak

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestMain(m *testing.M) {
	if os.Getenv("SOAK_HELPER") == "halfopen" {
		helperHalfOpen()
		return
	}
	os.Exit(m.Run())
}

// helperHalfOpen opens connections, announces readiness, then idles so the
// parent can freeze it with SIGSTOP.
func helperHalfOpen() {
	tg := &target{public: os.Getenv("SOAK_PUBLIC")}
	pemPath := os.Getenv("SOAK_CERT")
	_ = pemPath
	n := 0
	fmt.Sscan(os.Getenv("SOAK_COUNT"), &n)
	pool, err := loadPool(pemPath)
	if err != nil {
		fmt.Println("error", err)
		os.Exit(1)
	}
	tg.pool = pool
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	var ok atomic.Int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, _, err := tg.dial(ctx, ipFor(5000+i/4))
			if err != nil {
				return
			}
			if _, _, err := c.Read(ctx); err == nil {
				ok.Add(1)
			}
		}(i)
	}
	wg.Wait()
	fmt.Printf("ready %d\n", ok.Load())
	time.Sleep(10 * time.Minute)
}

// 1. Idle clients: 1000 sockets that never speak.
func TestIdleClientsAreClosedWithin6Seconds(t *testing.T) {
	tg := start(t, opts{})
	base := tg.sample()
	const n = 1000
	type result struct {
		elapsed time.Duration
		status  websocket.StatusCode
		err     error
	}
	results := make([]result, n)
	sem := make(chan struct{}, 100)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			c, _, err := tg.dial(ctx, ipFor(i/4))
			if err != nil {
				results[i].err = err
				return
			}
			defer c.CloseNow()
			if _, _, err := c.Read(ctx); err != nil { // hello
				results[i].err = err
				return
			}
			t0 := time.Now()
			_, _, err = c.Read(ctx)
			results[i] = result{elapsed: time.Since(t0), status: websocket.CloseStatus(err), err: err}
		}(i)
	}
	wg.Wait()
	var worst time.Duration
	bad := 0
	for _, r := range results {
		if r.status != websocket.StatusGoingAway {
			bad++
			if bad <= 3 {
				t.Errorf("client not closed with 1001: %v", r.err)
			}
		}
		worst = max(worst, r.elapsed)
	}
	if bad > 0 {
		t.Fatalf("%d of %d clients were not closed by the server", bad, n)
	}
	if worst > 6*time.Second {
		t.Fatalf("slowest idle close took %v, limit 6s", worst)
	}
	end := tg.settle(base, 30*time.Second, 10)
	report(t, "idle_clients", map[string]any{"clients": n, "slowest_close_s": worst.Seconds(),
		"goroutines_before": base.Goroutines, "goroutines_after": end.Goroutines, "fds_before": base.FDs, "fds_after": end.FDs})
}

// 2. Half-open clients: the peer process is frozen with SIGSTOP.
func TestHalfOpenClientsAreFreedByTheDeadlines(t *testing.T) {
	tg := start(t, opts{})
	base := tg.sample()
	const n = 200
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "SOAK_HELPER=halfopen", "SOAK_PUBLIC="+tg.public, "SOAK_COUNT="+fmt.Sprint(n),
		"SOAK_CERT="+filepath.Join(tg.certDir, "tls.crt"))
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "ready") {
		t.Fatalf("helper: %q %v", line, err)
	}
	if err := cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	frozen := time.Now()
	if a := tg.sample().Active; a < n/2 {
		t.Fatalf("only %v sessions were open when the client froze", a)
	}
	var freed time.Duration
	for time.Since(frozen) < 30*time.Second {
		if tg.sample().Active == 0 {
			freed = time.Since(frozen)
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if freed == 0 {
		t.Fatalf("frozen clients still held after 30 s: %+v", tg.sample())
	}
	// 5 s idle read deadline plus the 2 s close handshake, plus scheduling.
	if freed > 9*time.Second {
		t.Fatalf("frozen clients freed after %v, want under 9s", freed)
	}
	end := tg.settle(base, 30*time.Second, 10)
	report(t, "half_open_clients", map[string]any{"clients": n, "freed_after_s": freed.Seconds(),
		"goroutines_after": end.Goroutines, "fds_after": end.FDs})
}

// 3. Clients that send pings and never read the pongs.
func TestClientsThatNeverReadAreCutAtTheWriteDeadline(t *testing.T) {
	// With the production limit of at most 1000 pings the pongs can never fill
	// the socket buffers, so this test needs the fixture build's override.
	tg := start(t, opts{fixture: true, env: map[string]string{"PROBE_SOCK_BUF": "4096"}, args: []string{"--test-max-pings=1000000"}})
	base := tg.sample()
	rcvBuf = 2048
	stuckClient := tg.client("127.50.0.1")
	rcvBuf = 0
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	stuck, _, err := websocket.Dial(ctx, tg.url("/ws"), &websocket.DialOptions{
		HTTPClient: stuckClient, HTTPHeader: map[string][]string{"Origin": {origin}},
		Subprotocols: []string{"ondrya-latency.v1"}, CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stuck.CloseNow()
	var closedAt atomic.Int64
	var sent atomic.Int64
	go func() {
		for i := 0; ; i++ {
			sent.Store(int64(i))
			// No per-write timeout: the library hard-closes (RST) when a write
			// context expires, which would hide the server's own deadline.
			err := stuck.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf(`{"t":"ping","seq":%d}`, i)))
			if err != nil {
				closedAt.Store(time.Now().UnixNano())
				return
			}
			if ctx.Err() != nil {
				return
			}
		}
	}()

	good, _, err := tg.dial(ctx, "127.50.0.2")
	if err != nil {
		t.Fatal(err)
	}
	defer good.CloseNow()
	if _, _, err := good.Read(ctx); err != nil {
		t.Fatal(err)
	}
	var lat []time.Duration
	start := time.Now()
	for tg.metric("probe_write_timeouts_total") < 1 {
		if time.Since(start) > 12*time.Second {
			t.Fatalf("write deadline never fired: sent=%d stuckClosed=%v sample=%+v\n%s", sent.Load(), closedAt.Load() != 0, tg.sample(), tail(tg.logs(), 1500))
		}
		s := time.Now()
		if err := good.Write(ctx, websocket.MessageText, []byte(`{"t":"ping","seq":1}`)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := good.Read(ctx); err != nil {
			t.Fatal(err)
		}
		lat = append(lat, time.Since(s))
		time.Sleep(40 * time.Millisecond)
	}
	fired := time.Since(start)
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	p99 := lat[len(lat)*99/100]
	if p99 > 250*time.Millisecond {
		t.Fatalf("healthy session p99 latency %v while another was blocked", p99)
	}
	end := tg.settle(base, 40*time.Second, 10)
	report(t, "never_read", map[string]any{"write_deadline_fired_after_s": fired.Seconds(),
		"other_session_p99_ms": float64(p99.Microseconds()) / 1000, "other_session_samples": len(lat),
		"goroutines_after": end.Goroutines, "fds_after": end.FDs})
}

// 4. Connection flood from a few IPs, then recovery without a restart.
func TestConnectionFloodThenRecoversWithoutRestart(t *testing.T) {
	tg := start(t, opts{})
	base := tg.sample()
	const attempts, ips = 5000, 5
	var ok, rejected429, rejected503, other atomic.Int64
	var held sync.Map
	sem := make(chan struct{}, 250)
	var wg sync.WaitGroup
	t0 := time.Now()
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			c, resp, err := tg.dial(ctx, fmt.Sprintf("127.60.0.%d", i%ips+1))
			switch {
			case err == nil:
				ok.Add(1)
				held.Store(i, c)
			case resp != nil && resp.StatusCode == 429:
				rejected429.Add(1)
			case resp != nil && resp.StatusCode == 503:
				rejected503.Add(1)
			default:
				other.Add(1)
			}
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(t0)
	mid := tg.sample()
	held.Range(func(_, v any) bool { v.(*websocket.Conn).CloseNow(); return true })
	if ok.Load() > ips*(10+int64(elapsed.Seconds()/6)+2) {
		t.Fatalf("%d upgrades succeeded; the per-IP rate limit should have capped this", ok.Load())
	}
	if got := ok.Load() + rejected429.Load() + rejected503.Load() + other.Load(); got != attempts {
		t.Fatalf("accounting: %d", got)
	}
	if mid.MemBytes-base.MemBytes > 64<<20 {
		t.Fatalf("memory grew by %.1f MiB during the flood", (mid.MemBytes-base.MemBytes)/(1<<20))
	}
	// Normal service within 10 s of the flood ending, from an unaffected IP.
	deadline := time.Now().Add(10 * time.Second)
	recovered := false
	var recoveredAfter time.Duration
	flood := time.Now()
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, _, err := tg.dial(ctx, "127.61.0.1")
		if err == nil {
			_, _, _ = c.Read(ctx)
			if err := c.Write(ctx, websocket.MessageText, []byte(`{"t":"ping","seq":9}`)); err == nil {
				if _, b, err := c.Read(ctx); err == nil && string(b) == `{"t":"pong","seq":9}` {
					recovered, recoveredAfter = true, time.Since(flood)
				}
			}
			c.CloseNow()
		}
		cancel()
		if recovered {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !recovered {
		t.Fatal("no normal service within 10s of the flood ending")
	}
	end := tg.settle(base, 30*time.Second, 10)
	if end.StartTime != base.StartTime {
		t.Fatal("the process restarted")
	}
	report(t, "connection_flood", map[string]any{"attempts": attempts, "source_ips": ips, "upgrades_ok": ok.Load(),
		"rejected_429": rejected429.Load(), "rejected_503": rejected503.Load(), "other_errors": other.Load(),
		"flood_seconds": elapsed.Seconds(), "memory_growth_mib": (mid.MemBytes - base.MemBytes) / (1 << 20),
		"recovered_after_s": recoveredAfter.Seconds(), "goroutines_after": end.Goroutines, "fds_after": end.FDs})
}

// 5. Churn soak. SOAK_CHURN_DURATION=30m runs the full spec version.
func TestChurnSoakStaysFlat(t *testing.T) {
	dur := 20 * time.Second
	if v := os.Getenv("SOAK_CHURN_DURATION"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatal(err)
		}
		dur = d
	}
	tg := start(t, opts{})
	base := tg.sample()
	const rate = 50
	var started, completed, failed atomic.Int64
	var latMu sync.Mutex
	var lats []time.Duration
	var seq atomic.Int64
	inflight := make(chan struct{}, 1000)
	var wg sync.WaitGroup
	stop := time.Now().Add(dur)

	type sample struct{ g, f, m, a float64 }
	var samples []sample
	var sampMu sync.Mutex
	sdone := make(chan struct{})
	go func() {
		defer close(sdone)
		for time.Now().Before(stop) {
			time.Sleep(5 * time.Second)
			s := tg.sample()
			sampMu.Lock()
			samples = append(samples, sample{s.Goroutines, s.FDs, s.MemBytes, s.Active})
			sampMu.Unlock()
		}
	}()

	tick := time.NewTicker(time.Second / rate)
	defer tick.Stop()
	for time.Now().Before(stop) {
		<-tick.C
		select {
		case inflight <- struct{}{}:
		default:
			continue
		}
		wg.Add(1)
		started.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-inflight }()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			c, _, err := tg.dial(ctx, ipFor(int(seq.Add(1))))
			if err != nil {
				failed.Add(1)
				return
			}
			defer c.CloseNow()
			if _, _, err := c.Read(ctx); err != nil {
				failed.Add(1)
				return
			}
			for i := 0; i < 30; i++ {
				s := time.Now()
				if err := c.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf(`{"t":"ping","seq":%d}`, i))); err != nil {
					failed.Add(1)
					return
				}
				if _, _, err := c.Read(ctx); err != nil {
					failed.Add(1)
					return
				}
				latMu.Lock()
				if len(lats) < 200000 {
					lats = append(lats, time.Since(s))
				}
				latMu.Unlock()
			}
			if _, _, err := c.Read(ctx); websocket.CloseStatus(err) == websocket.StatusNormalClosure {
				completed.Add(1)
			} else {
				failed.Add(1)
			}
		}()
	}
	wg.Wait()
	<-sdone

	sampMu.Lock()
	defer sampMu.Unlock()
	if len(samples) < 4 {
		t.Skipf("run longer than %v to compare windows (SOAK_CHURN_DURATION)", dur)
	}
	win := len(samples) / 4
	if w := int((5 * time.Minute) / (5 * time.Second)); win > w {
		win = w
	}
	mean := func(ss []sample, f func(sample) float64) float64 {
		var sum float64
		for _, s := range ss {
			sum += f(s)
		}
		return sum / float64(len(ss))
	}
	first, last := samples[1:1+win], samples[len(samples)-win:]
	check := func(name string, f func(sample) float64, slack float64) (float64, float64) {
		a, b := mean(first, f), mean(last, f)
		if math.Abs(b-a) > math.Max(0.10*a, slack) {
			t.Errorf("%s drifted: first window %.1f, last window %.1f", name, a, b)
		}
		return a, b
	}
	g1, g2 := check("goroutines", func(s sample) float64 { return s.g }, 25)
	f1, f2 := check("open fds", func(s sample) float64 { return s.f }, 25)
	m1, m2 := check("memory", func(s sample) float64 { return s.m }, 16<<20)

	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	pct := func(p float64) float64 {
		if len(lats) == 0 {
			return 0
		}
		return float64(lats[int(float64(len(lats)-1)*p)].Microseconds()) / 1000
	}
	end := tg.settle(base, 30*time.Second, 10)
	report(t, "churn_soak", map[string]any{"duration_s": dur.Seconds(), "rate_per_s": rate, "started": started.Load(),
		"completed": completed.Load(), "failed": failed.Load(),
		"goroutines_first": g1, "goroutines_last": g2, "fds_first": f1, "fds_last": f2,
		"mem_mib_first": m1 / (1 << 20), "mem_mib_last": m2 / (1 << 20),
		"pong_rtt_p50_ms": pct(0.5), "pong_rtt_p99_ms": pct(0.99), "goroutines_after": end.Goroutines, "fds_after": end.FDs})
	if float64(failed.Load()) > 0.01*float64(started.Load()) {
		t.Fatalf("%d of %d sessions failed", failed.Load(), started.Load())
	}
}

// 6. Slowloris, in two forms.
func TestSlowlorisIsDroppedByTheHeaderAndHandshakeDeadlines(t *testing.T) {
	tg := start(t, opts{})
	base := tg.sample()
	const n = 20
	measure := func(name string, prep func(i int) (net.Conn, func(), error)) time.Duration {
		var worst atomic.Int64
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				c, drip, err := prep(i)
				if err != nil {
					t.Errorf("%s: %v", name, err)
					return
				}
				defer c.Close()
				t0 := time.Now()
				stop := make(chan struct{})
				if drip != nil {
					go func() {
						tk := time.NewTicker(time.Second)
						defer tk.Stop()
						for {
							select {
							case <-stop:
								return
							case <-tk.C:
								drip()
							}
						}
					}()
				}
				_ = c.SetReadDeadline(time.Now().Add(15 * time.Second))
				buf := make([]byte, 1)
				_, _ = c.Read(buf) // returns on server close
				close(stop)
				d := time.Since(t0)
				for {
					w := worst.Load()
					if int64(d) <= w || worst.CompareAndSwap(w, int64(d)) {
						break
					}
				}
			}(i)
		}
		wg.Wait()
		return time.Duration(worst.Load())
	}
	silent := measure("tcp-only", func(i int) (net.Conn, func(), error) {
		c, err := net.Dial("tcp", tg.public)
		return c, nil, err
	})
	drip := measure("tls-then-drip", func(i int) (net.Conn, func(), error) {
		raw, err := net.Dial("tcp", tg.public)
		if err != nil {
			return nil, nil, err
		}
		tc := tls.Client(raw, &tls.Config{ServerName: hostname, RootCAs: tg.pool, MinVersion: tls.VersionTLS12})
		if err := tc.Handshake(); err != nil {
			return nil, nil, err
		}
		_, _ = tc.Write([]byte("GET /ws HTTP/1.1\r\nHost: probe.test\r\nX-Slow: "))
		return tc, func() { _, _ = tc.Write([]byte("a")) }, nil
	})
	if silent > 7*time.Second || drip > 7*time.Second {
		t.Fatalf("slow connections were held too long: tcp-only %v, drip %v (limit 5s + scheduling)", silent, drip)
	}
	end := tg.settle(base, 30*time.Second, 10)
	report(t, "slowloris", map[string]any{"connections_each": n, "tcp_only_worst_s": silent.Seconds(),
		"tls_then_drip_worst_s": drip.Seconds(), "goroutines_after": end.Goroutines, "fds_after": end.FDs})
}

// 7. File descriptor exhaustion: reject cleanly, then exit by itself.
func TestFDExhaustionRejectsCleanlyThenSelfRestarts(t *testing.T) {
	tg := start(t, opts{nofile: 64})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	for w := 0; w < 200; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			d := net.Dialer{Timeout: 2 * time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP(ipFor(7000 + w))}}
			for ctx.Err() == nil {
				c, err := d.DialContext(ctx, "tcp", tg.public)
				if err != nil {
					time.Sleep(50 * time.Millisecond)
					continue
				}
				_ = c.SetReadDeadline(time.Now().Add(8 * time.Second))
				buf := make([]byte, 1)
				_, _ = c.Read(buf)
				c.Close()
			}
		}(w)
	}
	code, exited := tg.waitExit(150 * time.Second)
	cancel()
	wg.Wait()
	logs := tg.logs()
	if !exited {
		t.Fatalf("did not exit by itself under descriptor exhaustion:\n%s", tail(logs, 4000))
	}
	if code != 70 {
		t.Fatalf("exit code %d, want 70:\n%s", code, tail(logs, 4000))
	}
	for _, bad := range []string{"panic:", "fatal error:", "goroutine 1 [running]"} {
		if strings.Contains(strings.Split(logs, "ondrya-probe diagnostic bundle")[0], bad) {
			t.Fatalf("crashed uncleanly (%q):\n%s", bad, tail(logs, 4000))
		}
	}
	accept := strings.Count(logs, `"event":"accept:emfile"`)
	if !strings.Contains(logs, "ondrya-probe diagnostic bundle") {
		t.Fatalf("no diagnostic bundle:\n%s", tail(logs, 3000))
	}
	files, _ := filepath.Glob(filepath.Join(tg.stateDir, "diag-*.txt"))
	report(t, "fd_exhaustion", map[string]any{"nofile": 64, "exit_code": code,
		"accept_emfile_log_lines": accept, "diag_files": len(files)})
	if len(files) == 0 {
		t.Fatal("no diag file in the state dir")
	}
}

// 8. Watchdog: a hung upgrade path.
func TestWatchdogHungUpgradeGoes503BundlesAndExits70(t *testing.T) {
	tg := start(t, opts{fixture: true, args: []string{"--test-hang-upgrade-after=8s"}})
	t0 := time.Now()
	var unhealthyAfter time.Duration
	for time.Since(t0) < 60*time.Second {
		if st := tg.status("/healthz"); st != 200 {
			if st != 503 && st != 0 {
				t.Fatalf("unexpected /healthz status %d", st)
			}
			unhealthyAfter = time.Since(t0)
			break
		}
		select {
		case <-tg.exited:
			t.Fatal("exited before /healthz went unhealthy")
		case <-time.After(200 * time.Millisecond):
		}
	}
	if unhealthyAfter == 0 || unhealthyAfter > 45*time.Second {
		t.Fatalf("/healthz stayed healthy for %v", time.Since(t0))
	}
	code, exited := tg.waitExit(90 * time.Second)
	if !exited || code != 70 {
		t.Fatalf("exit %v code %d, want 70:\n%s", exited, code, tail(tg.logs(), 3000))
	}
	files, _ := filepath.Glob(filepath.Join(tg.stateDir, "diag-*.txt"))
	if len(files) != 1 {
		t.Fatalf("diag files: %v", files)
	}
	b, _ := os.ReadFile(files[0])
	if !strings.Contains(string(b), "goroutine dump") || !strings.Contains(string(b), "handleWS") {
		t.Fatalf("bundle lacks the goroutine dump showing the stuck handlers:\n%s", tail(string(b), 2000))
	}
	report(t, "watchdog_hung_upgrade", map[string]any{"healthz_unhealthy_after_s": unhealthyAfter.Seconds(),
		"exit_after_s": time.Since(t0).Seconds(), "exit_code": code})
}

func TestWatchdogHungAcceptExits70(t *testing.T) {
	tg := start(t, opts{fixture: true, args: []string{"--test-hang-accept-after=8s"}})
	t0 := time.Now()
	code, exited := tg.waitExit(90 * time.Second)
	if !exited || code != 70 {
		t.Fatalf("exit %v code %d, want 70:\n%s", exited, code, tail(tg.logs(), 3000))
	}
	if !strings.Contains(tg.logs(), "ondrya-probe diagnostic bundle") {
		t.Fatal("no bundle")
	}
	report(t, "watchdog_hung_accept", map[string]any{"exit_after_s": time.Since(t0).Seconds(), "exit_code": code})
}

// 9. Crash-loop guard.
func TestCrashLoopGuardStaysUpDegradedAfterFiveRestarts(t *testing.T) {
	state, certs := writableDir(t), writableDir(t)
	for i := 0; i < 6; i++ {
		tg := start(t, opts{fixture: true, stateDir: state, certDir: ensureCerts(t, certs),
			args: []string{"--test-hang-upgrade-after=1s", "--test-selftest-interval=300ms"}, noHealth: true})
		code, exited := tg.waitExit(90 * time.Second)
		if !exited || code != 70 {
			t.Fatalf("run %d: exit %v code %d:\n%s", i+1, exited, code, tail(tg.logs(), 2000))
		}
		tg.kill()
	}
	tg := start(t, opts{fixture: true, stateDir: state, certDir: certs,
		args: []string{"--test-hang-upgrade-after=1s", "--test-selftest-interval=300ms"}, noHealth: true})
	time.Sleep(20 * time.Second)
	if _, exited := tg.waitExit(time.Millisecond); exited {
		t.Fatalf("must stay up in degraded mode:\n%s", tail(tg.logs(), 2000))
	}
	if tg.status("/healthz") != 503 || tg.status("/ping") != 503 {
		t.Fatal("degraded node must answer 503 on /healthz and /ping")
	}
	if _, resp, err := tg.dial(context.Background(), ""); err == nil || resp == nil || resp.StatusCode != 503 {
		t.Fatalf("degraded /ws must be 503, got %v %v", resp, err)
	}
	if tg.metric("probe_crashloop") != 1 {
		t.Fatal("probe_crashloop must be 1")
	}
	report(t, "crashloop_guard", map[string]any{"restarts_before_degraded": 6, "degraded_stayed_up_s": 20})
}

func ensureCerts(t *testing.T, dir string) string {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, "tls.crt")); err != nil {
		tgCerts(t, dir)
	}
	return dir
}

// 10. Graceful shutdown with active sessions.
func TestGracefulShutdownCutsSessionsAt10sAndExits0(t *testing.T) {
	tg := start(t, opts{})
	const n = 20
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	var ended atomic.Int64
	var lastEnd atomic.Int64
	for i := 0; i < n; i++ {
		c, _, err := tg.dial(ctx, ipFor(9000+i))
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(i int, c *websocket.Conn) {
			defer wg.Done()
			defer c.CloseNow()
			_, _, _ = c.Read(ctx)
			for j := 0; ctx.Err() == nil; j++ { // a ping every 500 ms keeps it alive
				if err := c.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf(`{"t":"ping","seq":%d}`, j))); err != nil {
					break
				}
				if _, _, err := c.Read(ctx); err != nil {
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
			ended.Add(1)
			lastEnd.Store(time.Now().UnixNano())
		}(i, c)
	}
	time.Sleep(2 * time.Second)
	termAt := time.Now()
	tg.signal(syscall.SIGTERM)
	code, exited := tg.waitExit(20 * time.Second)
	exitAfter := time.Since(termAt)
	wg.Wait()
	if !exited || code != 0 {
		t.Fatalf("exit %v code %d:\n%s", exited, code, tail(tg.logs(), 2000))
	}
	if exitAfter > 14*time.Second {
		t.Fatalf("shutdown took %v, want about 10s grace", exitAfter)
	}
	if ended.Load() != n {
		t.Fatalf("%d of %d sessions ended", ended.Load(), n)
	}
	report(t, "graceful_shutdown", map[string]any{"sessions": n, "exit_after_s": exitAfter.Seconds(), "exit_code": code})
}

// 11. The test-only flag exists only in the fixture build.
func TestProductionBinaryRejectsTestFlags(t *testing.T) {
	tg := start(t, opts{args: []string{"--test-delay=1s"}, noHealth: true})
	code, exited := tg.waitExit(20 * time.Second)
	if !exited || code != 2 || !strings.Contains(tg.logs(), "flag provided but not defined") {
		t.Fatalf("production build must refuse --test-delay: exited=%v code=%d\n%s", exited, code, tg.logs())
	}

	fx := start(t, opts{fixture: true, args: []string{"--test-delay=300ms"}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := fx.dial(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	_, _, _ = c.Read(ctx)
	s := time.Now()
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"t":"ping","seq":1}`))
	if _, _, err := c.Read(ctx); err != nil || time.Since(s) < 290*time.Millisecond {
		t.Fatalf("fixture --test-delay not applied: %v %v", err, time.Since(s))
	}
	report(t, "test_flag_isolation", map[string]any{"production_exit_code": code})
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
