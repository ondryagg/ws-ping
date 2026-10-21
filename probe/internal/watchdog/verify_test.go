package watchdog

import (
	"crypto/tls"
	"crypto/x509"
	"testing"
	"time"

	"github.com/ondryadev/ws-ping/probe/internal/testcert"
)

// The roots are consulted when the handshake finishes, so a certificate that
// replaces the one the dial started with is judged against the current roots.
func TestVerifyServedUsesTheRootsOfTheMomentOfTheHandshake(t *testing.T) {
	_, _, oldCert := testcert.Write(t, t.TempDir(), "probe.test", time.Now().Add(time.Hour))
	_, _, newCert := testcert.Write(t, t.TempDir(), "probe.test", time.Now().Add(48*time.Hour))
	pool := x509.NewCertPool()
	pool.AddCert(oldCert)
	w := &Watchdog{cfg: Config{Hostname: "probe.test", Roots: func() *x509.CertPool { return pool }}}
	state := func(c *x509.Certificate) tls.ConnectionState {
		return tls.ConnectionState{PeerCertificates: []*x509.Certificate{c}}
	}
	if err := w.verifyServed(state(oldCert)); err != nil {
		t.Fatalf("served chain in the pool: %v", err)
	}
	if err := w.verifyServed(state(newCert)); err == nil {
		t.Fatal("a certificate outside the pool must be refused")
	}
	pool = x509.NewCertPool() // the reload happens
	pool.AddCert(newCert)
	if err := w.verifyServed(state(newCert)); err != nil {
		t.Fatalf("renewed chain after the pool changed: %v", err)
	}
	other := &Watchdog{cfg: Config{Hostname: "other.test", Roots: func() *x509.CertPool { return pool }}}
	if err := other.verifyServed(state(newCert)); err == nil {
		t.Fatal("a certificate for another name must be refused")
	}
	if err := w.verifyServed(tls.ConnectionState{}); err == nil {
		t.Fatal("no certificate must be refused")
	}
}
