//go:build soak

package soak

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ondryadev/ws-ping/probe/internal/testcert"
)

func loadPool(path string) (*x509.CertPool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p := x509.NewCertPool()
	p.AppendCertsFromPEM(b)
	return p, nil
}

func tgCerts(t *testing.T, dir string) {
	t.Helper()
	testcert.Write(t, dir, hostname, time.Now().Add(24*time.Hour))
	for _, f := range []string{"tls.crt", "tls.key"} {
		_ = os.Chmod(filepath.Join(dir, f), 0o644)
	}
}
