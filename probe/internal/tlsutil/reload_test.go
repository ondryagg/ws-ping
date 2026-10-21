package tlsutil

import (
	"crypto/x509"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ondryadev/ws-ping/probe/internal/testcert"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestReloadsOnChangeAndKeepsOldOnFailure(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, first := testcert.Write(t, dir, "probe.test", time.Now().Add(time.Hour))
	fails := 0
	r, err := New(certFile, keyFile, "probe.test", quiet, func() { fails++ })
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := r.Poll(); changed || err != nil {
		t.Fatalf("unchanged files: %v %v", changed, err)
	}
	cur, _ := r.GetCertificate(nil)
	if cur.Leaf.SerialNumber.Cmp(first.SerialNumber) != 0 {
		t.Fatal("wrong initial certificate")
	}

	// Rotate: new pair written over the old files.
	time.Sleep(10 * time.Millisecond)
	_, _, second := testcert.Write(t, dir, "probe.test", time.Now().Add(48*time.Hour))
	if changed, err := r.Poll(); !changed || err != nil {
		t.Fatalf("rotation must be picked up: %v %v", changed, err)
	}
	cur, _ = r.GetCertificate(nil)
	if cur.Leaf.SerialNumber.Cmp(second.SerialNumber) != 0 || !r.NotAfter().Equal(second.NotAfter) {
		t.Fatal("new certificate not served")
	}

	// Half-written rotation (cert and key from different pairs) keeps the old one.
	time.Sleep(10 * time.Millisecond)
	other := t.TempDir()
	oc, _, _ := testcert.Write(t, other, "probe.test", time.Now().Add(time.Hour))
	b, _ := os.ReadFile(oc)
	if err := os.WriteFile(certFile, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if changed, err := r.Poll(); changed || err == nil {
		t.Fatalf("mismatched pair must fail: %v %v", changed, err)
	}
	cur, _ = r.GetCertificate(nil)
	if cur.Leaf.SerialNumber.Cmp(second.SerialNumber) != 0 || fails != 1 {
		t.Fatalf("old certificate must stay in service (fails=%d)", fails)
	}

	// Files removed.
	_ = os.Remove(filepath.Join(dir, "tls.key"))
	if _, err := r.Poll(); err == nil || fails != 2 {
		t.Fatalf("missing files must count as a failure: %v %d", err, fails)
	}
}

func TestRefusesCertificateForAnotherHostname(t *testing.T) {
	dir := t.TempDir()
	c, k, _ := testcert.Write(t, dir, "probe.test", time.Now().Add(time.Hour))
	if _, err := New(c, k, "lg.example.com", quiet, nil); err == nil {
		t.Fatal("must refuse a certificate that does not cover the hostname")
	}
	if _, err := New(filepath.Join(dir, "nope"), k, "probe.test", quiet, nil); err == nil {
		t.Fatal("must refuse missing files")
	}
}

// certbot lays out live/<name>/{fullchain,privkey}.pem as symlinks into
// archive/ and repoints them on renewal.
func TestFollowsCertbotSymlinkSwap(t *testing.T) {
	root := t.TempDir()
	live, archive := filepath.Join(root, "live", "probe.test"), filepath.Join(root, "archive", "probe.test")
	for _, d := range []string{live, archive} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	issue := func(n string, notAfter time.Time) *x509.Certificate {
		d := filepath.Join(archive, n)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		_, _, c := testcert.Write(t, d, "probe.test", notAfter)
		return c
	}
	point := func(n string) {
		for dst, src := range map[string]string{"fullchain.pem": "tls.crt", "privkey.pem": "tls.key"} {
			l := filepath.Join(live, dst)
			_ = os.Remove(l)
			if err := os.Symlink(filepath.Join("..", "..", "archive", "probe.test", n, src), l); err != nil {
				t.Fatal(err)
			}
		}
	}
	first := issue("1", time.Now().Add(time.Hour))
	point("1")
	r, err := New(filepath.Join(live, "fullchain.pem"), filepath.Join(live, "privkey.pem"), "probe.test", quiet, nil)
	if err != nil {
		t.Fatal(err)
	}
	second := issue("2", time.Now().Add(48*time.Hour))
	point("2")
	if changed, err := r.Poll(); !changed || err != nil {
		t.Fatalf("symlink swap must be picked up: %v %v", changed, err)
	}
	cur, _ := r.GetCertificate(nil)
	if cur.Leaf.SerialNumber.Cmp(second.SerialNumber) != 0 || cur.Leaf.SerialNumber.Cmp(first.SerialNumber) == 0 {
		t.Fatal("renewed certificate not served")
	}
}
