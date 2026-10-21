package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/ondryadev/ws-ping/probe/internal/config"
)

// Healthcheck is the `probe healthcheck` subcommand used by the Docker
// HEALTHCHECK (the image has no shell or curl). It asks the running process's
// own listener for /healthz, verifying the served certificate against the
// certificate file, and returns nil only for HTTP 200.
func Healthcheck(cfg config.Config) error {
	raw, err := os.ReadFile(cfg.TLSCert)
	if err != nil {
		return fmt.Errorf("read certificate: %w", err)
	}
	pool := x509.NewCertPool()
	for rest := raw; ; {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if c, err := x509.ParseCertificate(blk.Bytes); err == nil && blk.Type == "CERTIFICATE" {
			pool.AddCert(c)
		}
	}
	addr := cfg.LocalDialAddr()
	client := &http.Client{
		Timeout: 4 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, addr)
			},
			TLSClientConfig:   &tls.Config{ServerName: cfg.Hostname, RootCAs: pool, MinVersion: tls.VersionTLS12},
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+net.JoinHostPort(cfg.Hostname, cfg.ListenPort())+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %d", resp.StatusCode)
	}
	return nil
}
