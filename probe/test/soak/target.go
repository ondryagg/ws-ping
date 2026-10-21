//go:build soak

// Package soak holds the failure and soak tests from spec section 10. They run
// against the real binary: in a container when SOAK_IMAGE (and
// SOAK_FIXTURE_IMAGE) are set, otherwise as a local process from SOAK_BIN
// (and SOAK_FIXTURE_BIN). Build with: go test -tags soak ./test/soak
package soak

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ondryadev/ws-ping/probe/internal/testcert"
)

const (
	hostname = "probe.test"
	origin   = "https://ondrya.com"
)

type opts struct {
	fixture  bool
	env      map[string]string
	args     []string
	nofile   int    // 0 = leave at 65536
	stateDir string // reuse across runs
	certDir  string // reuse across runs
	noHealth bool   // do not wait for /healthz
}

type target struct {
	t         *testing.T
	name      string
	public    string // 127.0.0.1:port
	metrics   string
	pool      *x509.CertPool
	stateDir  string
	certDir   string
	container bool

	cmd    *exec.Cmd
	out    *syncBuf
	exited chan int
	code   int
	done   bool
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// freePort returns a port the service can bind on all addresses. It stays
// below the kernel's ephemeral range (32768 and up) so it cannot collide with
// a client socket left over from an earlier test, and probes the wildcard
// address because that is what the service binds.
func freePort(t *testing.T) int {
	t.Helper()
	for i := 0; i < 200; i++ {
		port := 20000 + rand.IntN(10000)
		l, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			continue
		}
		_ = l.Close()
		return port
	}
	t.Fatal("no free port found")
	return 0
}

func writableDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := os.Chmod(d, 0o777); err != nil {
		t.Fatal(err)
	}
	return d
}

func start(t *testing.T, o opts) *target {
	t.Helper()
	image := os.Getenv("SOAK_IMAGE")
	bin := os.Getenv("SOAK_BIN")
	if o.fixture {
		image, bin = os.Getenv("SOAK_FIXTURE_IMAGE"), os.Getenv("SOAK_FIXTURE_BIN")
	}
	if image == "" && bin == "" {
		t.Skip("set SOAK_IMAGE/SOAK_FIXTURE_IMAGE or SOAK_BIN/SOAK_FIXTURE_BIN")
	}
	tg := &target{t: t, container: image != "", out: &syncBuf{}, exited: make(chan int, 1)}
	tg.name = fmt.Sprintf("soak-%d-%d", os.Getpid(), time.Now().UnixNano()%1_000_000)

	tg.certDir = o.certDir
	if tg.certDir == "" {
		tg.certDir = writableDir(t)
		testcert.Write(t, tg.certDir, hostname, time.Now().Add(24*time.Hour))
		for _, f := range []string{"tls.crt", "tls.key"} {
			_ = os.Chmod(filepath.Join(tg.certDir, f), 0o644)
		}
	}
	tg.stateDir = o.stateDir
	if tg.stateDir == "" {
		tg.stateDir = writableDir(t)
	}
	pem, err := os.ReadFile(filepath.Join(tg.certDir, "tls.crt"))
	if err != nil {
		t.Fatal(err)
	}
	tg.pool = x509.NewCertPool()
	tg.pool.AppendCertsFromPEM(pem)

	port, mport := freePort(t), freePort(t)
	tg.public = fmt.Sprintf("127.0.0.1:%d", port)
	tg.metrics = fmt.Sprintf("127.0.0.1:%d", mport)
	env := map[string]string{
		"PROBE_HOSTNAME": hostname, "PROBE_LISTEN": fmt.Sprintf(":%d", port),
		"PROBE_ALLOWED_ORIGINS": origin, "PROBE_NODE_ID": "soak-01", "PROBE_REGION": "soak",
		"PROBE_METRICS_LISTEN": tg.metrics,
	}
	if tg.container {
		env["GOMEMLIMIT"] = "200MiB"
	} else {
		env["PROBE_TLS_CERT"] = filepath.Join(tg.certDir, "tls.crt")
		env["PROBE_TLS_KEY"] = filepath.Join(tg.certDir, "tls.key")
		env["PROBE_STATE_DIR"] = tg.stateDir
	}
	for k, v := range o.env {
		env[k] = v
	}

	if tg.container {
		nofile := 65536
		if o.nofile > 0 {
			nofile = o.nofile
		}
		args := []string{"run", "--name", tg.name, "--network", "host", "--read-only", "--user", "65532:65532",
			"--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--memory", "256m", "--pids-limit", "512",
			"--ulimit", fmt.Sprintf("nofile=%d:%d", nofile, nofile),
			"-v", tg.certDir + ":/tls:ro", "-v", tg.stateDir + ":/var/log/ondrya-probe"}
		for k, v := range env {
			args = append(args, "-e", k+"="+v)
		}
		args = append(args, image)
		args = append(args, o.args...)
		tg.cmd = exec.Command("docker", args...)
	} else {
		if o.nofile > 0 {
			shell := append([]string{"-c", fmt.Sprintf("ulimit -n %d; exec \"$0\" \"$@\"", o.nofile), bin}, o.args...)
			tg.cmd = exec.Command("sh", shell...)
		} else {
			tg.cmd = exec.Command(bin, o.args...)
		}
		tg.cmd.Env = os.Environ()
		for k, v := range env {
			tg.cmd.Env = append(tg.cmd.Env, k+"="+v)
		}
	}
	tg.cmd.Stdout, tg.cmd.Stderr = tg.out, tg.out
	if err := tg.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		err := tg.cmd.Wait()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			code = -1
		}
		tg.exited <- code
	}()
	t.Cleanup(tg.kill)
	if !o.noHealth {
		tg.waitHealthy(30 * time.Second)
	}
	return tg
}

func (tg *target) kill() {
	if tg.done {
		return
	}
	if tg.container {
		_ = exec.Command("docker", "rm", "-f", tg.name).Run()
	} else if tg.cmd.Process != nil {
		_ = tg.cmd.Process.Kill()
	}
	select {
	case <-tg.exited:
	case <-time.After(10 * time.Second):
	}
	tg.done = true
}

// waitExit waits for the process or container to end by itself.
func (tg *target) waitExit(d time.Duration) (int, bool) {
	select {
	case c := <-tg.exited:
		tg.code, tg.done = c, true
		return c, true
	case <-time.After(d):
		return 0, false
	}
}

func (tg *target) signal(sig syscall.Signal) {
	if tg.container {
		name := map[syscall.Signal]string{syscall.SIGTERM: "TERM", syscall.SIGKILL: "KILL"}[sig]
		_ = exec.Command("docker", "kill", "-s", name, tg.name).Run()
		return
	}
	_ = tg.cmd.Process.Signal(sig)
}

// logs returns everything the process printed. For containers the docker run
// client streams the container's output.
func (tg *target) logs() string { return tg.out.String() }

func (tg *target) client(src string) *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second}
	if rcvBuf > 0 {
		d.Control = rcvBufControl(rcvBuf)
	}
	if src != "" {
		d.LocalAddr = &net.TCPAddr{IP: net.ParseIP(src)}
	}
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, n, _ string) (net.Conn, error) {
				return d.DialContext(ctx, n, tg.public)
			},
			TLSClientConfig:   &tls.Config{ServerName: hostname, RootCAs: tg.pool, MinVersion: tls.VersionTLS12},
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// rcvBuf, when positive, shrinks the client's receive buffer (set by tests
// that need a client that cannot absorb pongs).
var rcvBuf int

func (tg *target) url(path string) string {
	_, port, _ := net.SplitHostPort(tg.public)
	return "https://" + net.JoinHostPort(hostname, port) + path
}

func (tg *target) dial(ctx context.Context, src string) (*websocket.Conn, *http.Response, error) {
	h := http.Header{"Origin": {origin}}
	return websocket.Dial(ctx, tg.url("/ws"), &websocket.DialOptions{
		HTTPClient: tg.client(src), HTTPHeader: h, Subprotocols: []string{"ondrya-latency.v1"},
		CompressionMode: websocket.CompressionDisabled,
	})
}

func (tg *target) status(path string) int {
	resp, err := tg.client("").Get(tg.url(path))
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func (tg *target) waitHealthy(d time.Duration) {
	tg.t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if tg.status("/healthz") == 200 {
			return
		}
		select {
		case c := <-tg.exited:
			tg.t.Fatalf("exited with %d before becoming healthy:\n%s", c, tg.logs())
		case <-time.After(100 * time.Millisecond):
		}
	}
	tg.t.Fatalf("not healthy within %v:\n%s", d, tg.logs())
}

type snapshot struct {
	Goroutines, FDs, Active float64
	MemBytes                float64
	StartTime               float64
}

func (tg *target) metric(name string) float64 {
	resp, err := http.Get("http://" + tg.metrics + "/metrics") //nolint:noctx // test helper
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, name+" ") {
			v, _ := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, name)), 64)
			return v
		}
	}
	return -1
}

func (tg *target) sample() snapshot {
	return snapshot{
		Goroutines: tg.metric("probe_goroutines"), FDs: tg.metric("probe_open_fds"),
		Active: tg.metric("probe_connections_active"), MemBytes: tg.metric("probe_memory_bytes"),
		StartTime: tg.metric("probe_start_time_seconds"),
	}
}

// settle waits until goroutines and descriptors are back near the baseline.
func (tg *target) settle(base snapshot, within time.Duration, slack float64) snapshot {
	tg.t.Helper()
	deadline := time.Now().Add(within)
	var s snapshot
	for time.Now().Before(deadline) {
		s = tg.sample()
		if s.Active == 0 && s.Goroutines <= base.Goroutines+slack && s.FDs <= base.FDs+slack {
			return s
		}
		time.Sleep(250 * time.Millisecond)
	}
	tg.t.Fatalf("did not return to baseline within %v: base=%+v now=%+v", within, base, s)
	return s
}

func ipFor(i int) string {
	return fmt.Sprintf("127.%d.%d.%d", 1+(i/62500)%200, (i/250)%250, i%250+1)
}

func report(t *testing.T, name string, v map[string]any) {
	t.Helper()
	v["test"] = name
	v["time"] = time.Now().UTC().Format(time.RFC3339)
	b, _ := json.Marshal(v)
	t.Logf("RESULT %s", b)
	path := os.Getenv("SOAK_REPORT")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s\n", b)
}
