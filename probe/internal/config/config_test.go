package config

import (
	"flag"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func validEnv() map[string]string {
	return map[string]string{
		"PROBE_HOSTNAME":            "lg-dal.example.com",
		"PROBE_LISTEN":              ":8443",
		"PROBE_ALLOWED_ORIGINS":     "https://ondrya.com",
		"PROBE_NODE_ID":             "dal-01",
		"PROBE_TLS_LETSENCRYPT_DIR": "/etc/letsencrypt",
	}
}

func TestLetsEncryptPathsFollowTheHostname(t *testing.T) {
	c, err := Load(nil, env(validEnv()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.TLSCert != "/etc/letsencrypt/live/lg-dal.example.com/fullchain.pem" ||
		c.TLSKey != "/etc/letsencrypt/live/lg-dal.example.com/privkey.pem" {
		t.Fatalf("paths: %s %s", c.TLSCert, c.TLSKey)
	}
	e := validEnv()
	e["PROBE_TLS_CERT_NAME"] = "lg-dal.example.com-0001"
	c, err = Load(nil, env(e), nil)
	if err != nil || !strings.HasSuffix(c.TLSCert, "/live/lg-dal.example.com-0001/fullchain.pem") {
		t.Fatalf("cert name: %v %s", err, c.TLSCert)
	}
}

func TestExplicitFilesStillWork(t *testing.T) {
	e := validEnv()
	delete(e, "PROBE_TLS_LETSENCRYPT_DIR")
	e["PROBE_TLS_CERT"], e["PROBE_TLS_KEY"] = "/tls/a.crt", "/tls/a.key"
	c, err := Load(nil, env(e), nil)
	if err != nil || c.TLSCert != "/tls/a.crt" || c.TLSKey != "/tls/a.key" {
		t.Fatalf("%v %+v", err, c)
	}
}

func TestLoadFromEnvironmentWithSpecDefaults(t *testing.T) {
	c, err := Load(nil, env(validEnv()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxConns != 2000 || c.MaxConnsPerIP != 4 || c.NewConnsPerMin != 10 || c.PingBurst != 20 || c.PingRate != 5 ||
		c.MaxPings != 30 || c.SessionLifetime != 20*time.Second || c.FrameMax != 64 ||
		c.ReadTimeout != 5*time.Second || c.WriteTimeout != 2*time.Second || c.CloseTimeout != 2*time.Second ||
		c.HeaderTimeout != 5*time.Second || c.IdleTimeout != 10*time.Second || c.ShutdownGrace != 10*time.Second ||
		c.SelftestInterval != 10*time.Second || len(c.TrustedProxies) != 0 {
		t.Fatalf("defaults drifted from the spec: %+v", c)
	}
	if c.MetricsListen != "127.0.0.1:9109" || c.LocalDialAddr() != "127.0.0.1:8443" || c.ListenPort() != "8443" {
		t.Fatalf("addresses: %s %s", c.MetricsListen, c.LocalDialAddr())
	}
}

func TestFlagsOverrideEnvironment(t *testing.T) {
	e := validEnv()
	e["PROBE_MAX_CONNS"] = "100"
	c, err := Load([]string{"--max-conns=200", "--hostname", "Other.Example.com"}, env(e), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxConns != 200 || c.Hostname != "other.example.com" {
		t.Fatalf("%d %s", c.MaxConns, c.Hostname)
	}
}

func TestRefusesBadConfiguration(t *testing.T) {
	cases := map[string]struct {
		mut  func(map[string]string)
		args []string
		want string
	}{
		"no hostname":         {func(m map[string]string) { delete(m, "PROBE_HOSTNAME") }, nil, "hostname is required"},
		"hostname with port":  {func(m map[string]string) { m["PROBE_HOSTNAME"] = "a.example.com:8443" }, nil, "not a valid"},
		"hostname with slash": {func(m map[string]string) { m["PROBE_HOSTNAME"] = "a.example.com/x" }, nil, "not a valid"},
		"no origins":          {func(m map[string]string) { delete(m, "PROBE_ALLOWED_ORIGINS") }, nil, "allowed-origins is required"},
		"wildcard origin":     {func(m map[string]string) { m["PROBE_ALLOWED_ORIGINS"] = "https://*.ondrya.com" }, nil, "wildcards"},
		"star origin":         {func(m map[string]string) { m["PROBE_ALLOWED_ORIGINS"] = "*" }, nil, "origins"},
		"origin with path":    {func(m map[string]string) { m["PROBE_ALLOWED_ORIGINS"] = "https://ondrya.com/x" }, nil, "nothing else"},
		"origin trailing /":   {func(m map[string]string) { m["PROBE_ALLOWED_ORIGINS"] = "https://ondrya.com/" }, nil, "nothing else"},
		"origin upper":        {func(m map[string]string) { m["PROBE_ALLOWED_ORIGINS"] = "https://Ondrya.com" }, nil, "lowercase"},
		"origin plain http":   {func(m map[string]string) { m["PROBE_ALLOWED_ORIGINS"] = "http://ondrya.com" }, nil, "only allowed for localhost"},
		"bad node id":         {func(m map[string]string) { m["PROBE_NODE_ID"] = `a"b` }, nil, "node-id"},
		"metrics not loop":    {func(m map[string]string) { m["PROBE_METRICS_LISTEN"] = "0.0.0.0:9109" }, nil, "loopback"},
		"metrics hostname":    {func(m map[string]string) { m["PROBE_METRICS_LISTEN"] = "localhost:9109" }, nil, "loopback"},
		"same ports":          {func(m map[string]string) { m["PROBE_METRICS_LISTEN"] = "127.0.0.1:8443" }, nil, "different ports"},
		"bad port":            {func(m map[string]string) { m["PROBE_LISTEN"] = ":99999" }, nil, "listen"},
		"per ip over global":  {func(m map[string]string) { m["PROBE_MAX_CONNS_PER_IP"] = "5000" }, nil, "max-conns-per-ip"},
		"not an int":          {func(m map[string]string) { m["PROBE_MAX_CONNS"] = "many" }, nil, "PROBE_MAX_CONNS"},
		"zero rate":           {func(m map[string]string) { m["PROBE_PING_RATE"] = "0" }, nil, "ping-rate"},
		"lifetime too long":   {func(m map[string]string) { m["PROBE_SESSION_LIFETIME"] = "10m" }, nil, "session-lifetime"},
		"bad log level":       {func(m map[string]string) { m["PROBE_LOG_LEVEL"] = "loud" }, nil, "log-level"},
		"bad proxy":           {func(m map[string]string) { m["PROBE_TRUSTED_PROXIES"] = "not-an-ip" }, nil, "trusted-proxies"},
		"relative disable":    {func(m map[string]string) { m["PROBE_DISABLE_FILE"] = "run/disabled" }, nil, "absolute"},
		"no tls source":       {func(m map[string]string) { delete(m, "PROBE_TLS_LETSENCRYPT_DIR") }, nil, "tls-letsencrypt-dir"},
		"both tls sources":    {func(m map[string]string) { m["PROBE_TLS_CERT"] = "/x.crt" }, nil, "alternatives"},
		"cert name alone": {func(m map[string]string) {
			delete(m, "PROBE_TLS_LETSENCRYPT_DIR")
			m["PROBE_TLS_CERT_NAME"] = "x"
			m["PROBE_TLS_CERT"] = "/a"
			m["PROBE_TLS_KEY"] = "/b"
		}, nil, "needs tls-letsencrypt-dir"},
		"cert name traversal": {func(m map[string]string) { m["PROBE_TLS_CERT_NAME"] = "../../etc" }, nil, "tls-cert-name"},
		"cert name slash":     {func(m map[string]string) { m["PROBE_TLS_CERT_NAME"] = "a/b" }, nil, "tls-cert-name"},
		"relative le dir":     {func(m map[string]string) { m["PROBE_TLS_LETSENCRYPT_DIR"] = "letsencrypt" }, nil, "absolute"},
		"unknown flag":        {nil, []string{"--test-delay=1s"}, "flag provided but not defined"},
		"stray argument":      {nil, []string{"serve"}, "unexpected argument"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := validEnv()
			if tc.mut != nil {
				tc.mut(e)
			}
			_, err := Load(tc.args, env(e), nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestExtraFlagsOnlyWhenRegistered(t *testing.T) {
	var d time.Duration
	_, err := Load([]string{"--test-delay=1s"}, env(validEnv()), func(fs *flag.FlagSet) { fs.DurationVar(&d, "test-delay", 0, "") })
	if err != nil || d != time.Second {
		t.Fatalf("%v %v", err, d)
	}
}

func TestTrustedProxiesAndLocalDial(t *testing.T) {
	e := validEnv()
	e["PROBE_TRUSTED_PROXIES"] = "10.0.0.0/8, 192.0.2.7"
	e["PROBE_LISTEN"] = "203.0.113.5:8443"
	c, err := Load(nil, env(e), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.TrustedProxies) != 2 || c.LocalDialAddr() != "203.0.113.5:8443" {
		t.Fatalf("%v %s", c.TrustedProxies, c.LocalDialAddr())
	}
}

func TestRefusesTrustedProxiesThatCollapseTheBoundary(t *testing.T) {
	for _, v := range []string{"0.0.0.0/0", "::/0", "127.0.0.1", "127.0.0.0/8", "::1", "0.0.0.0"} {
		e := validEnv()
		e["PROBE_TRUSTED_PROXIES"] = v
		if _, err := Load(nil, env(e), nil); err == nil || !strings.Contains(err.Error(), "trusted-proxies") {
			t.Errorf("%q must be refused, got %v", v, err)
		}
	}
	e := validEnv()
	e["PROBE_TRUSTED_PROXIES"] = "10.0.0.0/8"
	if _, err := Load(nil, env(e), nil); err != nil {
		t.Fatal(err)
	}
}
