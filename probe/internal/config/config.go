// Package config loads and validates the probe configuration from flags and
// environment variables. The service refuses to start on any invalid value.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Config holds every tunable. Fields without a flag are fixed by the spec and
// only change in tests.
type Config struct {
	Hostname       string
	Listen         string
	MetricsListen  string
	AllowedOrigins []string
	NodeID         string
	Region         string
	TLSCert        string
	TLSKey         string
	// TLSLetsEncryptDir is a certbot config directory (normally
	// /etc/letsencrypt). When set, TLSCert and TLSKey are derived from it.
	TLSLetsEncryptDir string
	TLSCertName       string

	MaxConns         int
	MaxConnsPerIP    int
	NewConnsPerMin   int
	PingRate         float64
	PingBurst        int
	MaxRawConns      int
	MaxRawConnsPerIP int
	SockBuf          int
	TrustedProxies   []netip.Prefix

	Maintenance bool
	DisableFile string
	LogLevel    slog.Level
	StateDir    string

	MaxPings        int
	SessionLifetime time.Duration

	FrameMax         int
	ReadTimeout      time.Duration
	WriteTimeout     time.Duration
	CloseTimeout     time.Duration
	HeaderTimeout    time.Duration
	IdleTimeout      time.Duration
	ShutdownGrace    time.Duration
	SelftestInterval time.Duration
}

// HelpError is returned by Load when -h or --help was given.
type HelpError struct{ Text string }

func (e *HelpError) Error() string { return "help requested" }

// Defaults returns the spec defaults. Required fields are left empty.
func Defaults() Config {
	return Config{
		Listen:           ":8443",
		MetricsListen:    "127.0.0.1:9109",
		MaxConns:         2000,
		MaxConnsPerIP:    4,
		NewConnsPerMin:   10,
		PingRate:         5,
		PingBurst:        20,
		MaxRawConns:      4096,
		MaxRawConnsPerIP: 16,
		SockBuf:          32 * 1024,
		DisableFile:      "/run/probe/disabled",
		LogLevel:         slog.LevelInfo,
		StateDir:         "/var/log/ondrya-probe",
		MaxPings:         30,
		SessionLifetime:  20 * time.Second,
		FrameMax:         64,
		ReadTimeout:      5 * time.Second,
		WriteTimeout:     2 * time.Second,
		CloseTimeout:     2 * time.Second,
		HeaderTimeout:    5 * time.Second,
		IdleTimeout:      10 * time.Second,
		ShutdownGrace:    10 * time.Second,
		SelftestInterval: 10 * time.Second,
	}
}

var (
	hostnameRE = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	// certNameRE admits certbot lineage names such as "host.example.com" or
	// "host.example.com-0001" and nothing that could walk out of live/.
	certNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`)
	nodeIDRE   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	regionRE   = regexp.MustCompile(`^[A-Za-z0-9._ -]{0,64}$`)
)

type loader struct {
	fs     *flag.FlagSet
	getenv func(string) string
	errs   []error
}

func envName(flagName string) string {
	return "PROBE_" + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// fromEnv returns the environment value for a flag, if set and non-empty.
func (l *loader) fromEnv(name string) (string, string, bool) {
	env := envName(name)
	v := l.getenv(env)
	return env, v, v != ""
}

func (l *loader) str(p *string, name, usage string) {
	if _, v, ok := l.fromEnv(name); ok {
		*p = v
	}
	l.fs.StringVar(p, name, *p, usage)
}

func (l *loader) integer(p *int, name, usage string) {
	if env, v, ok := l.fromEnv(name); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			l.errs = append(l.errs, fmt.Errorf("%s: %q is not an integer", env, v))
		} else {
			*p = n
		}
	}
	l.fs.IntVar(p, name, *p, usage)
}

func (l *loader) float(p *float64, name, usage string) {
	if env, v, ok := l.fromEnv(name); ok {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			l.errs = append(l.errs, fmt.Errorf("%s: %q is not a number", env, v))
		} else {
			*p = f
		}
	}
	l.fs.Float64Var(p, name, *p, usage)
}

func (l *loader) boolean(p *bool, name, usage string) {
	if env, v, ok := l.fromEnv(name); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			l.errs = append(l.errs, fmt.Errorf("%s: %q is not a boolean", env, v))
		} else {
			*p = b
		}
	}
	l.fs.BoolVar(p, name, *p, usage)
}

func (l *loader) dur(p *time.Duration, name, usage string) {
	if env, v, ok := l.fromEnv(name); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			l.errs = append(l.errs, fmt.Errorf("%s: %q is not a duration", env, v))
		} else {
			*p = d
		}
	}
	l.fs.DurationVar(p, name, *p, usage)
}

// Load parses flags (which override environment variables), then validates.
// extra lets a build-tagged file register additional flags; production builds
// pass nil, so unknown flags are rejected.
func Load(args []string, getenv func(string) string, extra func(*flag.FlagSet)) (Config, error) {
	c := Defaults()
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	l := &loader{fs: fs, getenv: getenv}

	var origins, proxies, level string
	level = c.LogLevel.String()
	if v := getenv(envName("allowed-origins")); v != "" {
		origins = v
	}
	if v := getenv(envName("trusted-proxies")); v != "" {
		proxies = v
	}

	l.str(&c.Hostname, "hostname", "public DNS name served by this node (required)")
	l.str(&c.Listen, "listen", "public listen address, host:port")
	l.str(&c.MetricsListen, "metrics-listen", "loopback address for /metrics and /debug/pprof/")
	fs.StringVar(&origins, "allowed-origins", origins, "comma separated exact Origin values (required)")
	l.str(&c.NodeID, "node-id", "node identifier sent in hello and /info (required)")
	l.str(&c.Region, "region", "region label for /info")
	l.str(&c.TLSLetsEncryptDir, "tls-letsencrypt-dir", "certbot config directory, normally /etc/letsencrypt; serves live/<cert name>/fullchain.pem and privkey.pem and follows renewals")
	l.str(&c.TLSCertName, "tls-cert-name", "certbot certificate name under live/ (default: the hostname)")
	l.str(&c.TLSCert, "tls-cert", "TLS certificate chain, PEM (alternative to --tls-letsencrypt-dir)")
	l.str(&c.TLSKey, "tls-key", "TLS private key, PEM (alternative to --tls-letsencrypt-dir)")
	l.integer(&c.MaxConns, "max-conns", "global concurrent WebSocket sessions")
	l.integer(&c.MaxConnsPerIP, "max-conns-per-ip", "concurrent WebSocket sessions per client IP (/64 for IPv6)")
	l.integer(&c.NewConnsPerMin, "new-conns-per-min", "new WebSocket sessions per client IP per minute")
	l.float(&c.PingRate, "ping-rate", "sustained /ping and /info requests per second per client IP")
	l.integer(&c.PingBurst, "ping-burst", "burst of /ping and /info requests per client IP")
	l.integer(&c.MaxRawConns, "max-raw-conns", "open TCP connections before TLS, all clients")
	l.integer(&c.MaxRawConnsPerIP, "max-raw-conns-per-ip", "open TCP connections before TLS per client IP")
	l.integer(&c.SockBuf, "sock-buf", "SO_SNDBUF and SO_RCVBUF per accepted socket in bytes, 0 for kernel default")
	fs.StringVar(&proxies, "trusted-proxies", proxies, "comma separated CIDRs allowed to set X-Forwarded-For (default none)")
	l.boolean(&c.Maintenance, "maintenance", "answer 503 on /ws and /ping")
	l.str(&c.DisableFile, "disable-file", "kill switch file; if it exists, behave as --maintenance")
	fs.StringVar(&level, "log-level", envOr(getenv, "log-level", level), "debug, info, warn or error")
	l.str(&c.StateDir, "state-dir", "directory for diagnostic bundles and the crash-loop guard")
	l.integer(&c.MaxPings, "max-pings", "pings answered per session")
	l.dur(&c.SessionLifetime, "session-lifetime", "total session lifetime")
	if extra != nil {
		extra(fs)
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			var b strings.Builder
			b.WriteString("usage: probe [serve|healthcheck|version] [flags]\n\nEvery flag also reads the environment variable PROBE_<FLAG>, with dashes as underscores\n(for example --max-conns is PROBE_MAX_CONNS). Flags win over the environment.\n\n")
			fs.SetOutput(&b)
			fs.PrintDefaults()
			return Config{}, &HelpError{Text: b.String()}
		}
		return Config{}, fmt.Errorf("flags: %w", err)
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if len(l.errs) > 0 {
		return Config{}, errors.Join(l.errs...)
	}

	var errs []error
	if err := c.LogLevel.UnmarshalText([]byte(level)); err != nil {
		errs = append(errs, fmt.Errorf("log-level: %w", err))
	}
	c.AllowedOrigins = splitList(origins)
	for _, p := range splitList(proxies) {
		pre, err := parsePrefix(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("trusted-proxies: %w", err))
			continue
		}
		if pre.Bits() == 0 || pre.Addr().IsLoopback() || pre.Addr().IsUnspecified() {
			errs = append(errs, fmt.Errorf("trusted-proxies: %s would trust every client or the loopback self-test path", p))
			continue
		}
		c.TrustedProxies = append(c.TrustedProxies, pre)
	}
	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	c.Hostname = strings.ToLower(strings.TrimSpace(c.Hostname))
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	if c.TLSLetsEncryptDir != "" {
		live := filepath.Join(c.TLSLetsEncryptDir, "live", c.certName())
		c.TLSCert = filepath.Join(live, "fullchain.pem")
		c.TLSKey = filepath.Join(live, "privkey.pem")
	}
	return c, nil
}

// certName is the certbot lineage under live/. certbot names it after the
// first domain unless it had to add a numeric suffix.
func (c Config) certName() string {
	if c.TLSCertName != "" {
		return c.TLSCertName
	}
	return c.Hostname
}

func envOr(getenv func(string) string, flagName, def string) string {
	if v := getenv(envName(flagName)); v != "" {
		return v
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func parsePrefix(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, err
		}
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// Validate checks every field and returns all problems at once.
func (c Config) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	switch {
	case c.Hostname == "":
		bad("hostname is required")
	case len(c.Hostname) > 253 || !hostnameRE.MatchString(c.Hostname):
		bad("hostname %q is not a valid lowercase DNS name (no port, no trailing dot)", c.Hostname)
	}
	if _, ok := parseHostPort(c.Listen, true); !ok {
		bad("listen %q must be host:port with an IP literal or empty host", c.Listen)
	}
	if ap, ok := parseHostPort(c.MetricsListen, false); !ok || !ap.Addr().IsLoopback() {
		bad("metrics-listen %q must be a loopback IP literal with a port", c.MetricsListen)
	} else if lp, ok := parseHostPort(c.Listen, true); ok && lp.Port() == ap.Port() {
		bad("metrics-listen and listen must use different ports")
	}
	if len(c.AllowedOrigins) == 0 {
		bad("allowed-origins is required")
	}
	for _, o := range c.AllowedOrigins {
		if err := validOrigin(o); err != nil {
			bad("allowed-origins: %v", err)
		}
	}
	if !nodeIDRE.MatchString(c.NodeID) {
		bad("node-id must match %s", nodeIDRE)
	}
	if !regionRE.MatchString(c.Region) {
		bad("region must match %s", regionRE)
	}
	switch {
	case c.TLSLetsEncryptDir != "":
		if c.TLSCert != "" || c.TLSKey != "" {
			bad("tls-letsencrypt-dir and tls-cert/tls-key are alternatives; set one")
		}
		if !certNameRE.MatchString(c.certName()) {
			bad("tls-cert-name must match %s", certNameRE)
		}
		if !filepath.IsAbs(c.TLSLetsEncryptDir) {
			bad("tls-letsencrypt-dir must be an absolute path")
		}
	case c.TLSCertName != "":
		bad("tls-cert-name needs tls-letsencrypt-dir")
	case c.TLSCert == "" || c.TLSKey == "":
		bad("set tls-letsencrypt-dir, or both tls-cert and tls-key")
	}
	rng := func(name string, v, lo, hi int) {
		if v < lo || v > hi {
			bad("%s=%d out of range [%d, %d]", name, v, lo, hi)
		}
	}
	rng("max-conns", c.MaxConns, 1, 100000)
	rng("max-conns-per-ip", c.MaxConnsPerIP, 1, c.MaxConns)
	rng("new-conns-per-min", c.NewConnsPerMin, 1, 100000)
	rng("ping-burst", c.PingBurst, 1, 100000)
	rng("max-raw-conns", c.MaxRawConns, c.MaxConns, 1000000)
	rng("max-raw-conns-per-ip", c.MaxRawConnsPerIP, c.MaxConnsPerIP, c.MaxRawConns)
	rng("sock-buf", c.SockBuf, 0, 4<<20)
	rng("max-pings", c.MaxPings, 1, 1000)
	if c.PingRate <= 0 || c.PingRate > 100000 {
		bad("ping-rate=%v out of range (0, 100000]", c.PingRate)
	}
	if c.SessionLifetime < time.Second || c.SessionLifetime > time.Minute {
		bad("session-lifetime=%v out of range [1s, 1m]", c.SessionLifetime)
	}
	if c.StateDir == "" {
		bad("state-dir is required")
	}
	if c.DisableFile != "" && !strings.HasPrefix(c.DisableFile, "/") {
		bad("disable-file must be an absolute path or empty")
	}
	if c.FrameMax < 1 || c.ReadTimeout <= 0 || c.WriteTimeout <= 0 || c.CloseTimeout <= 0 ||
		c.HeaderTimeout <= 0 || c.IdleTimeout <= 0 || c.ShutdownGrace <= 0 || c.SelftestInterval <= 0 {
		bad("internal timeouts and frame size must be positive")
	}
	return errors.Join(errs...)
}

func parseHostPort(s string, allowEmptyHost bool) (netip.AddrPort, bool) {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return netip.AddrPort{}, false
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil || p == 0 {
		return netip.AddrPort{}, false
	}
	if host == "" {
		if !allowEmptyHost {
			return netip.AddrPort{}, false
		}
		return netip.AddrPortFrom(netip.IPv6Unspecified(), uint16(p)), true
	}
	a, err := netip.ParseAddr(host)
	if err != nil || a.Zone() != "" {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(a, uint16(p)), true
}

func validOrigin(o string) error {
	u, err := url.Parse(o)
	if err != nil {
		return fmt.Errorf("%q: %w", o, err)
	}
	if o != strings.ToLower(o) {
		return fmt.Errorf("%q must be lowercase", o)
	}
	if u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return fmt.Errorf("%q must be scheme://host[:port] with nothing else", o)
	}
	if strings.ContainsAny(u.Host, "*") {
		return fmt.Errorf("%q: wildcards are not allowed", o)
	}
	switch u.Scheme {
	case "https":
	case "http":
		switch u.Hostname() {
		case "localhost", "127.0.0.1", "::1":
		default:
			return fmt.Errorf("%q: http origins are only allowed for localhost", o)
		}
	default:
		return fmt.Errorf("%q: scheme must be https", o)
	}
	if o != u.Scheme+"://"+u.Host {
		return fmt.Errorf("%q is not in canonical form", o)
	}
	return nil
}

// ListenPort returns the numeric port of the public listener.
func (c Config) ListenPort() string {
	_, port, _ := net.SplitHostPort(c.Listen)
	return port
}

// LocalDialAddr is the address the self-test and health check dial to reach
// the public listener from inside the process.
func (c Config) LocalDialAddr() string {
	host, port, _ := net.SplitHostPort(c.Listen)
	if a, err := netip.ParseAddr(host); err != nil || a.IsUnspecified() {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// Summary returns slog attributes describing the config. There are no
// secrets in it: certificate paths only, never contents.
func (c Config) Summary() []any {
	return []any{
		"hostname", c.Hostname, "listen", c.Listen, "metrics_listen", c.MetricsListen,
		"node", c.NodeID, "region", c.Region, "origins", c.AllowedOrigins,
		"max_conns", c.MaxConns, "max_conns_per_ip", c.MaxConnsPerIP,
		"new_conns_per_min", c.NewConnsPerMin, "ping_rate", c.PingRate, "ping_burst", c.PingBurst,
		"max_raw_conns", c.MaxRawConns, "max_raw_conns_per_ip", c.MaxRawConnsPerIP,
		"max_pings", c.MaxPings, "sock_buf", c.SockBuf, "session_lifetime", c.SessionLifetime.String(),
		"trusted_proxies", len(c.TrustedProxies), "maintenance", c.Maintenance,
		"disable_file", c.DisableFile, "state_dir", c.StateDir,
		"tls_cert", c.TLSCert, "tls_key", c.TLSKey,
	}
}
