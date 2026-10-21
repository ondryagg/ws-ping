package app_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ondryadev/ws-ping/probe/internal/app"
	"github.com/ondryadev/ws-ping/probe/internal/testutil"
)

func ctx5(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestEchoSessionCompletesWithNormalClose(t *testing.T) {
	r := testutil.Start(t, testutil.Config(t), nil)
	ctx := ctx5(t)
	c, resp, err := r.Dial(ctx, testutil.DialOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if got := c.Subprotocol(); got != "ondrya-latency.v1" {
		t.Fatalf("subprotocol %q", got)
	}
	if resp.Header.Get("Sec-WebSocket-Extensions") != "" {
		t.Fatal("permessage-deflate must be off")
	}
	_, hello, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var h struct {
		T            string `json:"t"`
		Proto        int    `json:"proto"`
		Node         string `json:"node"`
		MaxPings     int    `json:"maxPings"`
		ServerTimeMs int64  `json:"serverTimeMs"`
	}
	if err := json.Unmarshal(hello, &h); err != nil {
		t.Fatalf("hello %q: %v", hello, err)
	}
	if h.T != "hello" || h.Proto != 1 || h.Node != "test-01" || h.MaxPings != 30 || abs(time.Now().UnixMilli()-h.ServerTimeMs) > 5000 {
		t.Fatalf("bad hello %+v", h)
	}
	seqs := []uint32{0, 7, 4294967295, 7, 1}
	for i := 0; i < 30; i++ {
		seq := seqs[i%len(seqs)]
		got, err := testutil.Ping(ctx, c, seq)
		if err != nil {
			t.Fatalf("ping %d: %v", i, err)
		}
		if want := fmt.Sprintf(`{"t":"pong","seq":%d}`, seq); got != want {
			t.Fatalf("ping %d: got %s want %s", i, got, want)
		}
	}
	_, _, err = c.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatalf("expected 1000 after the last pong, got %v", err)
	}
	testutil.Eventually(t, 3*time.Second, "completed metric", func() bool {
		return r.Metric(`probe_connections_total{result="completed"}`) == 1
	})
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

func TestPreUpgradeRejections(t *testing.T) {
	r := testutil.Start(t, testutil.Config(t), nil)
	ctx := ctx5(t)
	cases := []struct {
		name string
		opts testutil.DialOpts
		want int
	}{
		{"missing origin", testutil.DialOpts{Origin: testutil.Str("")}, 403},
		{"unknown origin", testutil.DialOpts{Origin: testutil.Str("https://evil.example")}, 403},
		{"null origin", testutil.DialOpts{Origin: testutil.Str("null")}, 403},
		{"origin prefix", testutil.DialOpts{Origin: testutil.Str("https://ondrya.com.evil.example")}, 403},
		{"origin trailing slash", testutil.DialOpts{Origin: testutil.Str("https://ondrya.com/")}, 403},
		{"origin case", testutil.DialOpts{Origin: testutil.Str("https://ONDRYA.com")}, 403},
		{"no subprotocol", testutil.DialOpts{NoProtocol: true}, 400},
		{"wrong subprotocol", testutil.DialOpts{Subprotocols: []string{"chat"}}, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, resp, err := r.Dial(ctx, tc.opts)
			if err == nil {
				c.CloseNow()
				t.Fatal("expected the upgrade to be refused")
			}
			if resp == nil || resp.StatusCode != tc.want {
				t.Fatalf("status %v, want %d (err %v)", resp, tc.want, err)
			}
		})
	}
	if v := r.Metric(`probe_rejected_total{reason="origin"}`); v != 6 {
		t.Fatalf("origin rejects counted %v, want 6", v)
	}
	if v := r.Metric(`probe_rejected_total{reason="subprotocol"}`); v != 2 {
		t.Fatalf("subprotocol rejects counted %v, want 2", v)
	}
}

func TestHostMethodPathAndSNI(t *testing.T) {
	r := testutil.Start(t, testutil.Config(t), nil)
	hc := r.Client("")
	do := func(method, path, host string) int {
		req, _ := http.NewRequest(method, r.URL(path), nil)
		if host != "" {
			req.Host = host
		}
		req.Header.Set("Origin", testutil.Origin)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := do("GET", "/ws", "evil.example"); got != 421 {
		t.Fatalf("wrong Host on /ws: %d", got)
	}
	if got := do("GET", "/ping", "evil.example"); got != 421 {
		t.Fatalf("wrong Host on /ping: %d", got)
	}
	if got := do("POST", "/ws", ""); got != 405 {
		t.Fatalf("POST /ws: %d", got)
	}
	if got := do("GET", "/nope", ""); got != 404 {
		t.Fatalf("GET /nope: %d", got)
	}
	if got := do("GET", "/ws", "PROBE.test:9999"); got != 400 && got != 426 {
		t.Fatalf("host with different case and port must pass the host check (then fail the upgrade): %d", got)
	}
	for _, sni := range []string{"other.test", ""} {
		conn, err := net.Dial("tcp", r.Public)
		if err != nil {
			t.Fatal(err)
		}
		tc := tls.Client(conn, &tls.Config{ServerName: sni, InsecureSkipVerify: true}) //nolint:gosec // probing the server's SNI check
		_ = tc.SetDeadline(time.Now().Add(3 * time.Second))
		if err := tc.Handshake(); err == nil {
			t.Fatalf("SNI %q: handshake must fail", sni)
		}
		conn.Close()
	}
	if v := r.Metric(`probe_rejected_total{reason="sni"}`); v < 2 {
		t.Fatalf("sni rejects %v", v)
	}
}

func TestBadFramesCloseWithSpecCodes(t *testing.T) {
	cfg := testutil.Config(t)
	r := testutil.Start(t, cfg, nil)
	ctx := ctx5(t)
	cases := []struct {
		name string
		typ  websocket.MessageType
		body string
		want websocket.StatusCode
	}{
		{"binary", websocket.MessageBinary, `{"t":"ping","seq":1}`, websocket.StatusMessageTooBig},
		{"oversized", websocket.MessageText, `{"t":"ping","seq":1,"x":"` + strings.Repeat("a", 60) + `"}`, websocket.StatusMessageTooBig},
		{"not json", websocket.MessageText, `hello`, websocket.StatusPolicyViolation},
		{"wrong type", websocket.MessageText, `{"t":"pong","seq":1}`, websocket.StatusPolicyViolation},
		{"missing seq", websocket.MessageText, `{"t":"ping"}`, websocket.StatusPolicyViolation},
		{"negative seq", websocket.MessageText, `{"t":"ping","seq":-1}`, websocket.StatusPolicyViolation},
		{"seq overflow", websocket.MessageText, `{"t":"ping","seq":4294967296}`, websocket.StatusPolicyViolation},
		{"float seq", websocket.MessageText, `{"t":"ping","seq":1.5}`, websocket.StatusPolicyViolation},
		{"extra field", websocket.MessageText, `{"t":"ping","seq":1,"a":2}`, websocket.StatusPolicyViolation},
		{"trailing data", websocket.MessageText, `{"t":"ping","seq":1}x`, websocket.StatusPolicyViolation},
		{"empty", websocket.MessageText, ``, websocket.StatusPolicyViolation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := r.MustDial(t, "")
			defer c.CloseNow()
			if err := c.Write(ctx, tc.typ, []byte(tc.body)); err != nil {
				t.Fatal(err)
			}
			_, _, err := c.Read(ctx)
			if got := websocket.CloseStatus(err); got != tc.want {
				t.Fatalf("close status %d (%v), want %d", got, err, tc.want)
			}
		})
	}
	// Exactly at the limit is accepted.
	c := r.MustDial(t, "")
	defer c.CloseNow()
	pad := `{"t":"ping","seq":1}`
	body := pad[:len(pad)-1] + strings.Repeat(" ", 64-len(pad)) + "}"
	if len(body) != 64 {
		t.Fatalf("test bug: %d", len(body))
	}
	if err := c.Write(ctx, websocket.MessageText, []byte(body)); err != nil {
		t.Fatal(err)
	}
	if _, b, err := c.Read(ctx); err != nil || string(b) != `{"t":"pong","seq":1}` {
		t.Fatalf("64-byte frame: %q %v", b, err)
	}
}

func TestIdleLifetimeAndMaxPings(t *testing.T) {
	cfg := testutil.Config(t)
	cfg.ReadTimeout = 400 * time.Millisecond
	cfg.SessionLifetime = 1500 * time.Millisecond
	cfg.MaxPings = 3
	r := testutil.Start(t, cfg, nil)
	ctx := ctx5(t)

	t.Run("idle closes 1001", func(t *testing.T) {
		c := r.MustDial(t, "")
		defer c.CloseNow()
		start := time.Now()
		_, _, err := c.Read(ctx)
		if websocket.CloseStatus(err) != websocket.StatusGoingAway {
			t.Fatalf("want 1001, got %v", err)
		}
		if d := time.Since(start); d < 300*time.Millisecond || d > 1500*time.Millisecond {
			t.Fatalf("idle close after %v", d)
		}
		if r.Metric("probe_read_timeouts_total") < 1 {
			t.Fatal("read timeout not counted")
		}
	})
	t.Run("max pings closes 1000", func(t *testing.T) {
		c := r.MustDial(t, "")
		defer c.CloseNow()
		for i := uint32(0); i < 3; i++ {
			if _, err := testutil.Ping(ctx, c, i); err != nil {
				t.Fatal(err)
			}
		}
		_, _, err := c.Read(ctx)
		if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
			t.Fatalf("want 1000, got %v", err)
		}
	})
	t.Run("lifetime closes 1000", func(t *testing.T) {
		cfg2 := testutil.Config(t)
		cfg2.SessionLifetime = time.Second
		cfg2.MaxPings = 1000
		r2 := testutil.Start(t, cfg2, nil)
		c := r2.MustDial(t, "")
		defer c.CloseNow()
		start := time.Now()
		var err error
		for i := uint32(0); err == nil; i++ {
			_, err = testutil.Ping(ctx, c, i)
			time.Sleep(100 * time.Millisecond)
		}
		if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
			t.Fatalf("want 1000, got %v", err)
		}
		if d := time.Since(start); d < 900*time.Millisecond || d > 2500*time.Millisecond {
			t.Fatalf("closed after %v, want about 1s", d)
		}
	})
}

func TestConnectionLimits(t *testing.T) {
	cfg := testutil.Config(t)
	cfg.MaxConns = 6
	cfg.MaxConnsPerIP = 2
	cfg.MaxRawConns = 100
	cfg.NewConnsPerMin = 100000
	r := testutil.Start(t, cfg, nil)
	ctx := ctx5(t)

	var held []*websocket.Conn
	t.Cleanup(func() {
		for _, c := range held {
			c.CloseNow()
		}
	})
	dial := func(src string) int {
		c, resp, err := r.Dial(ctx, testutil.DialOpts{SrcIP: src})
		if err == nil {
			held = append(held, c)
			return 101
		}
		if resp == nil {
			t.Fatalf("dial %s: %v", src, err)
		}
		return resp.StatusCode
	}
	if first, second := dial("127.0.0.2"), dial("127.0.0.2"); first != 101 || second != 101 {
		t.Fatal("two sessions per IP must be allowed")
	}
	if got := dial("127.0.0.2"); got != 429 {
		t.Fatalf("third session from one IP: %d, want 429", got)
	}
	for _, ip := range []string{"127.0.0.3", "127.0.0.3", "127.0.0.4", "127.0.0.4"} {
		if dial(ip) != 101 {
			t.Fatal("sessions up to the global cap must be allowed")
		}
	}
	if got := dial("127.0.0.5"); got != 503 {
		t.Fatalf("beyond the global cap: %d, want 503", got)
	}
	if r.Metric(`probe_rejected_total{reason="per_ip"}`) != 1 || r.Metric(`probe_rejected_total{reason="capacity"}`) != 1 {
		t.Fatal("reject counters wrong")
	}
	// Closing frees the slot.
	held[0].CloseNow()
	testutil.Eventually(t, 3*time.Second, "slot released", func() bool { return dial("127.0.0.2") == 101 })
}

func TestNewConnectionRateLimit(t *testing.T) {
	cfg := testutil.Config(t)
	cfg.NewConnsPerMin = 3
	r := testutil.Start(t, cfg, nil)
	ctx := ctx5(t)
	var codes []int
	for i := 0; i < 5; i++ {
		c, resp, err := r.Dial(ctx, testutil.DialOpts{SrcIP: "127.0.0.9"})
		if err == nil {
			c.CloseNow()
			codes = append(codes, 101)
		} else {
			codes = append(codes, resp.StatusCode)
		}
		time.Sleep(50 * time.Millisecond) // let the per-IP slot free
	}
	if fmt.Sprint(codes) != "[101 101 101 429 429]" {
		t.Fatalf("got %v", codes)
	}
	// A different IP has its own bucket.
	if c, _, err := r.Dial(ctx, testutil.DialOpts{SrcIP: "127.0.0.10"}); err != nil {
		t.Fatal(err)
	} else {
		c.CloseNow()
	}
}

func TestPingInfoHealthz(t *testing.T) {
	cfg := testutil.Config(t)
	cfg.PingBurst = 3
	cfg.PingRate = 0.001
	r := testutil.Start(t, cfg, nil)
	hc := r.Client("127.0.0.20")

	get := func(path, origin string) (*http.Response, string) {
		req, _ := http.NewRequest("GET", r.URL(path), nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	resp, _ := get("/ping", testutil.Origin)
	if resp.StatusCode != 204 || resp.Header.Get("Cache-Control") != "no-store" ||
		resp.Header.Get("Access-Control-Allow-Origin") != testutil.Origin ||
		resp.Header.Get("Timing-Allow-Origin") != testutil.Origin {
		t.Fatalf("bad /ping response %d %v", resp.StatusCode, resp.Header)
	}
	if resp, _ := get("/ping", "https://evil.example"); resp.StatusCode != 403 || resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("unknown origin on /ping: %d %v", resp.StatusCode, resp.Header)
	}
	resp, body := get("/info", "")
	if resp.StatusCode != 200 || body != `{"node":"test-01","region":"test","version":"test","proto":1}` {
		t.Fatalf("/info: %d %s", resp.StatusCode, body)
	}
	if resp, _ := get("/ping", ""); resp.StatusCode != 429 {
		t.Fatalf("burst of 3 exhausted, got %d", resp.StatusCode)
	}
	if resp, _ := get("/healthz", ""); resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("/healthz must not be rate limited or cached: %d", resp.StatusCode)
	}
}

func TestMaintenanceAndKillSwitchFile(t *testing.T) {
	t.Run("flag", func(t *testing.T) {
		cfg := testutil.Config(t)
		cfg.Maintenance = true
		r := testutil.Start(t, cfg, nil)
		if r.Status("/ping", "") != 503 {
			t.Fatal("/ping must be 503")
		}
		if _, resp, err := r.Dial(ctx5(t), testutil.DialOpts{}); err == nil || resp.StatusCode != 503 {
			t.Fatalf("/ws must be 503, got %v %v", resp, err)
		}
		if r.Status("/healthz", "") != 200 {
			t.Fatal("/healthz must keep answering (and stay healthy: the watchdog bypasses maintenance)")
		}
	})
	t.Run("file", func(t *testing.T) {
		cfg := testutil.Config(t)
		cfg.DisableFile = filepath.Join(t.TempDir(), "disabled")
		r := testutil.Start(t, cfg, nil)
		if r.Status("/ping", "") != 204 {
			t.Fatal("serving before the file exists")
		}
		if err := os.WriteFile(cfg.DisableFile, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		testutil.Eventually(t, 4*time.Second, "503 after file appears", func() bool { return r.Status("/ping", "") == 503 })
		_ = os.Remove(cfg.DisableFile)
		testutil.Eventually(t, 4*time.Second, "204 after file removed", func() bool { return r.Status("/ping", "") == 204 })
	})
}

func TestPanicInOneSessionDoesNotAffectOthers(t *testing.T) {
	cfg := testutil.Config(t)
	cfg.MaxConnsPerIP = 3
	r := testutil.Start(t, cfg, func(o *app.Options) {
		o.Hooks.OnPing = func(_ context.Context, seq uint32) {
			if seq == 666 {
				panic("boom")
			}
		}
	})
	ctx := ctx5(t)
	good := r.MustDial(t, "127.0.0.30")
	defer good.CloseNow()
	for round := 0; round < 3; round++ { // would exhaust the per-IP cap if a slot leaked
		bad := r.MustDial(t, "127.0.0.30")
		_ = bad.Write(ctx, websocket.MessageText, []byte(`{"t":"ping","seq":666}`))
		if _, _, err := bad.Read(ctx); err == nil {
			t.Fatal("the panicking session must be closed")
		}
		bad.CloseNow()
		if _, err := testutil.Ping(ctx, good, uint32(round)); err != nil {
			t.Fatalf("other session affected by panic: %v", err)
		}
	}
	if v := r.Metric("probe_panics_total"); v != 3 {
		t.Fatalf("panics counted %v", v)
	}
	testutil.Eventually(t, 3*time.Second, "slots released", func() bool {
		c, _, err := r.Dial(ctx, testutil.DialOpts{SrcIP: "127.0.0.30"})
		if err == nil {
			c.CloseNow()
		}
		return err == nil
	})
}

func TestWritesToClientsThatNeverReadAreCutAtTheDeadline(t *testing.T) {
	cfg := testutil.Config(t)
	cfg.SockBuf = 4096
	cfg.WriteTimeout = 300 * time.Millisecond
	cfg.MaxPings = 1_000_000
	cfg.SessionLifetime = 30 * time.Second
	cfg.ReadTimeout = 10 * time.Second
	r := testutil.Start(t, cfg, nil)
	ctx := ctx5(t)

	// A client with a tiny receive buffer that sends pings and never reads.
	d := &net.Dialer{Control: func(_, _ string, rc syscallConn) error { return setRcvBuf(rc, 2048) }}
	hc := r.Client("127.0.0.40")
	hc.Transport.(*http.Transport).DialContext = func(ctx context.Context, n, _ string) (net.Conn, error) {
		d.LocalAddr = &net.TCPAddr{IP: net.ParseIP("127.0.0.40")}
		return d.DialContext(ctx, n, r.Public)
	}
	stuck, _, err := r.Dial(ctx, testutil.DialOpts{Client: hc})
	if err != nil {
		t.Fatal(err)
	}
	defer stuck.CloseNow()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := uint32(0); ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			wctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			err := stuck.Write(wctx, websocket.MessageText, fmt.Appendf(nil, `{"t":"ping","seq":%d}`, i))
			cancel()
			if err != nil && wctx.Err() == nil {
				return
			}
		}
	}()
	defer func() { close(stop); wg.Wait() }()

	// While it is stuck, another session must stay fast.
	good := r.MustDial(t, "127.0.0.41")
	defer good.CloseNow()
	var worst time.Duration
	testutil.Eventually(t, 8*time.Second, "write timeout counted", func() bool {
		s := time.Now()
		if _, err := testutil.Ping(ctx, good, 1); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(s); d > worst {
			worst = d
		}
		return r.Metric("probe_write_timeouts_total") >= 1
	})
	if worst > 200*time.Millisecond {
		t.Fatalf("a healthy session saw %v latency while another was blocked", worst)
	}
	testutil.Eventually(t, 3*time.Second, "stuck connection released", func() bool {
		return r.Metric("probe_connections_active") == 1
	})
}

func TestNoGoroutineOrSlotLeakAfterMixedAbuse(t *testing.T) {
	cfg := testutil.Config(t)
	cfg.ReadTimeout = 300 * time.Millisecond
	cfg.MaxConnsPerIP = 100
	r := testutil.Start(t, cfg, nil)
	ctx := ctx5(t)
	time.Sleep(300 * time.Millisecond)
	base := runtime.NumGoroutine()

	var wg sync.WaitGroup
	for i := 0; i < 150; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, _, err := r.Dial(ctx, testutil.DialOpts{SrcIP: fmt.Sprintf("127.0.1.%d", i%250+1)})
			if err != nil {
				return
			}
			switch i % 4 {
			case 0: // vanish immediately
				c.CloseNow()
			case 1: // idle until the server closes
				_, _, _ = c.Read(ctx)
				c.CloseNow()
			case 2: // protocol violation
				_ = c.Write(ctx, websocket.MessageText, []byte("x"))
				_, _, _ = c.Read(ctx)
				c.CloseNow()
			default: // polite
				_, _ = testutil.Ping(ctx, c, 1)
				_ = c.Close(websocket.StatusNormalClosure, "")
			}
		}(i)
	}
	wg.Wait()
	testutil.Eventually(t, 5*time.Second, "sessions drained", func() bool { return r.Metric("probe_connections_active") == 0 })
	testutil.Eventually(t, 5*time.Second, "goroutines back to baseline", func() bool {
		runtime.GC()
		return runtime.NumGoroutine() <= base+8
	})
}

func TestMetricsAndPprofAreLoopbackOnlyAndRebindingSafe(t *testing.T) {
	r := testutil.Start(t, testutil.Config(t), nil)
	if r.Status("/metrics", "") != 404 || r.Status("/debug/pprof/", "") != 404 {
		t.Fatal("metrics and pprof must not exist on the public listener")
	}
	get := func(path, host string) int {
		req, _ := http.NewRequest("GET", "http://"+r.Metrics+path, nil)
		if host != "" {
			req.Host = host
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if get("/metrics", "") != 200 || get("/debug/pprof/goroutine?debug=2", "") != 200 {
		t.Fatal("metrics listener must serve /metrics and pprof")
	}
	if get("/metrics", "attacker.example:1234") != 403 {
		t.Fatal("non-loopback Host must be refused (DNS rebinding)")
	}
	host, _, _ := net.SplitHostPort(r.Metrics)
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		t.Fatalf("metrics bound to %s", r.Metrics)
	}
}

func TestStartupRefusesBadCertificate(t *testing.T) {
	cfg := testutil.Config(t)
	cfg.Hostname = "other.test" // certificate is for probe.test
	var logs testutil.SafeBuffer
	code := app.Run(context.Background(), app.Options{Cfg: cfg, LogOut: &logs, Stderr: &logs})
	if code != 1 || !strings.Contains(logs.String(), "does not cover") {
		t.Fatalf("code %d logs %s", code, logs.String())
	}
}

func TestGracefulShutdown(t *testing.T) {
	cfg := testutil.Config(t)
	cfg.ShutdownGrace = 700 * time.Millisecond
	cfg.ReadTimeout = 20 * time.Second
	r := testutil.Start(t, cfg, nil)
	ctx := ctx5(t)
	c := r.MustDial(t, "")
	defer c.CloseNow()
	start := time.Now()
	done := make(chan int, 1)
	go func() { done <- r.Stop() }()
	_, _, err := c.Read(ctx)
	if err == nil {
		t.Fatal("session must be closed on shutdown")
	}
	code := <-done
	if code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("shutdown took %v", d)
	}
	if _, err := net.DialTimeout("tcp", r.Public, time.Second); err == nil {
		t.Fatal("listener must be closed after shutdown")
	}
}

// listener that stops serving, as a stuck accept loop would.
type hangListener struct {
	net.Listener
	hang   atomic.Bool
	closed chan struct{}
	once   sync.Once
}

func (h *hangListener) Accept() (net.Conn, error) {
	c, err := h.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if !h.hang.Load() {
		return c, nil
	}
	_ = c.Close()
	<-h.closed
	return nil, net.ErrClosed
}

func (h *hangListener) Close() error {
	h.once.Do(func() { close(h.closed) })
	return h.Listener.Close()
}

func TestWatchdogDumpsEvidenceAndExits70(t *testing.T) {
	cfg := testutil.Config(t)
	var hl *hangListener
	r := testutil.Start(t, cfg, func(o *app.Options) {
		o.WrapListener = func(l net.Listener) net.Listener {
			hl = &hangListener{Listener: l, closed: make(chan struct{})}
			return hl
		}
	})
	if r.Status("/healthz", "") != 200 {
		t.Fatal("healthy before the fault")
	}
	hl.hang.Store(true)
	// /healthz can no longer be reached either; the point is the exit.
	select {
	case code := <-r.Exits:
		if code != app.ExitWatchdog {
			t.Fatalf("exit code %d, want 70", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("watchdog did not fire; logs: %s", r.Logs.String())
	}
	stderr := r.Stderr.String()
	for _, want := range []string{"diagnostic bundle", "goroutine dump", "open_fds:", "active_connections:", "last accept and upgrade errors"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("bundle on stderr lacks %q", want)
		}
	}
	files, _ := filepath.Glob(filepath.Join(cfg.StateDir, "diag-*.txt"))
	if len(files) != 1 {
		t.Fatalf("diag files: %v", files)
	}
	b, _ := os.ReadFile(filepath.Join(cfg.StateDir, "watchdog-exits"))
	if strings.Count(string(b), "\n") != 1 {
		t.Fatalf("crash-loop record: %q", b)
	}
}

func TestCrashLoopGuardDegradesThenRecovers(t *testing.T) {
	cfg := testutil.Config(t)
	cfg.SelftestInterval = 60 * time.Millisecond
	if err := os.MkdirAll(cfg.StateDir, 0o750); err != nil {
		t.Fatal(err)
	}
	var lines strings.Builder
	for i := 0; i < 6; i++ { // more than 5 exits in the window
		fmt.Fprintf(&lines, "%d\n", time.Now().Add(-time.Duration(i)*time.Minute).Unix())
	}
	if err := os.WriteFile(filepath.Join(cfg.StateDir, "watchdog-exits"), []byte(lines.String()), 0o640); err != nil {
		t.Fatal(err)
	}
	r := testutil.StartUnchecked(t, cfg, nil)
	if r.Metric("probe_crashloop") != 1 {
		t.Fatal("probe_crashloop must be 1 at start")
	}
	if got := r.Status("/healthz", ""); got != 503 {
		t.Fatalf("degraded /healthz: %d", got)
	}
	if got := r.Status("/ping", ""); got != 503 {
		t.Fatalf("degraded /ping: %d", got)
	}
	// The self-test bypasses degraded mode, passes six times, and clears it.
	testutil.Eventually(t, 5*time.Second, "recovery", func() bool { return r.Status("/healthz", "") == 200 })
	if r.Metric("probe_crashloop") != 0 {
		t.Fatal("probe_crashloop must clear")
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "watchdog-exits")); !os.IsNotExist(err) {
		t.Fatal("guard history must be reset after recovery")
	}
	select {
	case code := <-r.Exits:
		t.Fatalf("must not exit while degraded, got %d", code)
	default:
	}
}

func TestHealthcheckSubcommandLogic(t *testing.T) {
	cfg := testutil.Config(t)
	r := testutil.Start(t, cfg, nil)
	cfg.Listen = r.Public
	if err := app.Healthcheck(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.TLSCert = filepath.Join(t.TempDir(), "missing")
	if err := app.Healthcheck(cfg); err == nil {
		t.Fatal("missing certificate must fail")
	}
}

func TestMetricsServerRefusesCrossSiteBrowserRequests(t *testing.T) {
	r := testutil.Start(t, testutil.Config(t), nil)
	get := func(hdr map[string]string) int {
		req, _ := http.NewRequest("GET", "http://"+r.Metrics+"/debug/pprof/cmdline", nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if get(nil) != 200 || get(map[string]string{"Sec-Fetch-Site": "none"}) != 200 || get(map[string]string{"Sec-Fetch-Site": "same-origin"}) != 200 {
		t.Fatal("direct and same-origin requests must work")
	}
	if get(map[string]string{"Sec-Fetch-Site": "cross-site"}) != 403 || get(map[string]string{"Sec-Fetch-Site": "same-site"}) != 403 ||
		get(map[string]string{"Origin": "https://attacker.example"}) != 403 {
		t.Fatal("cross-site browser requests must be refused")
	}
}

// ownIP returns a non-loopback unicast address of this host, if any.
func ownIP(t *testing.T) string {
	t.Helper()
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() && ipn.IP.IsGlobalUnicast() {
			return ipn.IP.String()
		}
	}
	return ""
}

// Regression for watchdog-selftest-loses-loopback-exemption-with-specific-ip-listen.
func TestSelfTestStaysHealthyWhenListeningOnASpecificIPInMaintenance(t *testing.T) {
	ip := ownIP(t)
	if ip == "" {
		t.Skip("no non-loopback IPv4 address on this host")
	}
	cfg := testutil.Config(t)
	cfg.Listen = ip + ":0"
	cfg.Maintenance = true // visitors get 503; the self-test must not
	cfg.MaxConns = 1
	cfg.MaxConnsPerIP = 1
	cfg.MaxRawConns = 1
	cfg.MaxRawConnsPerIP = 1
	r := testutil.Start(t, cfg, nil)
	time.Sleep(6 * cfg.SelftestInterval) // well past three self-test periods
	select {
	case code := <-r.Exits:
		t.Fatalf("watchdog exited with %d although only visitors are being refused", code)
	default:
	}
	if r.Metric("probe_selftest_ok") != 1 {
		t.Fatal("self-test must keep passing")
	}
	if r.Status("/ping", "") != 503 {
		t.Fatal("visitors must still get 503 in maintenance")
	}
}

func TestPingCORSForSeveralOriginsAndPreflight(t *testing.T) {
	allowed := []string{"https://ondrya.com", "https://www.ondrya.com", "http://localhost:4321", "http://localhost:4322", "http://127.0.0.1:4321"}
	cfg := testutil.Config(t)
	cfg.AllowedOrigins = allowed
	cfg.PingBurst = 50
	r := testutil.Start(t, cfg, nil)
	hc := r.Client("127.0.0.21")
	do := func(method, origin string) *http.Response {
		req, _ := http.NewRequest(method, r.URL("/ping"), nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
			req.Header.Set("Access-Control-Request-Method", "GET")
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	for _, o := range allowed {
		g := do("GET", o)
		if g.StatusCode != 204 || g.Header.Get("Access-Control-Allow-Origin") != o || g.Header.Get("Timing-Allow-Origin") != o ||
			g.Header.Get("Vary") != "Origin" {
			t.Fatalf("GET from %s: %d %v", o, g.StatusCode, g.Header)
		}
		p := do("OPTIONS", o)
		if p.StatusCode != 204 || p.Header.Get("Access-Control-Allow-Origin") != o ||
			p.Header.Get("Access-Control-Allow-Methods") != "GET, OPTIONS" ||
			p.Header.Get("Access-Control-Max-Age") != "86400" || p.Header.Get("Vary") != "Origin" {
			t.Fatalf("OPTIONS from %s: %d %v", o, p.StatusCode, p.Header)
		}
	}
	for _, o := range []string{"https://example.org", "https://ondrya.com.evil.example", "http://localhost:4323", "https://sub.ondrya.com", "null"} {
		for _, m := range []string{"GET", "OPTIONS"} {
			resp := do(m, o)
			if resp.StatusCode != 403 || resp.Header.Get("Access-Control-Allow-Origin") != "" || resp.Header.Get("Timing-Allow-Origin") != "" ||
				resp.Header.Get("Access-Control-Allow-Methods") != "" {
				t.Fatalf("%s from %s must be refused without CORS headers: %d %v", m, o, resp.StatusCode, resp.Header)
			}
		}
	}
	if p := do("OPTIONS", ""); p.StatusCode != 204 || p.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("OPTIONS without Origin: %d %v", p.StatusCode, p.Header)
	}
	if resp := do("POST", ""); resp.StatusCode != 405 || resp.Header.Get("Allow") != "GET, OPTIONS" {
		t.Fatalf("POST: %d %v", resp.StatusCode, resp.Header)
	}
}

func TestPingRateLimitIsPerIPAndAnswers429(t *testing.T) {
	cfg := testutil.Config(t)
	cfg.PingBurst, cfg.PingRate = 20, 0.001
	r := testutil.Start(t, cfg, nil)
	count := func(ip string, n int) (ok, limited int) {
		hc := r.Client(ip)
		for i := 0; i < n; i++ {
			resp, err := hc.Get(r.URL("/ping"))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			switch resp.StatusCode {
			case 204:
				ok++
			case 429:
				limited++
			}
		}
		return
	}
	if ok, limited := count("127.0.0.30", 40); ok != 20 || limited != 20 {
		t.Fatalf("burst 20 then 429: ok=%d limited=%d", ok, limited)
	}
	if ok, _ := count("127.0.0.31", 1); ok != 1 {
		t.Fatal("another address must have its own budget")
	}
}
