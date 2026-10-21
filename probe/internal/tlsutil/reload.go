// Package tlsutil serves a certificate from files and reloads it when the
// files change, without a restart.
package tlsutil

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

type loaded struct {
	cert     *tls.Certificate
	pool     *x509.CertPool
	notAfter time.Time
	sig      fileSig
}

type fileSig struct {
	certMod, keyMod   time.Time
	certSize, keySize int64
	// certbot keeps live/ as symlinks into archive/ and repoints them on
	// renewal, so the resolved target is part of the identity.
	certReal, keyReal string
}

// Reloader hands out the current certificate. A failed reload keeps the
// previous certificate in service.
type Reloader struct {
	certFile, keyFile string
	hostname          string
	log               *slog.Logger
	onFail            func()
	cur               atomic.Pointer[loaded]
}

// New loads the pair once and refuses to continue if it is unusable or does
// not cover hostname.
func New(certFile, keyFile, hostname string, log *slog.Logger, onFail func()) (*Reloader, error) {
	r := &Reloader{certFile: certFile, keyFile: keyFile, hostname: hostname, log: log, onFail: onFail}
	l, err := r.load()
	if err != nil {
		return nil, err
	}
	r.cur.Store(l)
	return r, nil
}

func (r *Reloader) stat() (fileSig, error) {
	c, err := os.Stat(r.certFile)
	if err != nil {
		return fileSig{}, err
	}
	k, err := os.Stat(r.keyFile)
	if err != nil {
		return fileSig{}, err
	}
	cr, err := filepath.EvalSymlinks(r.certFile)
	if err != nil {
		return fileSig{}, err
	}
	kr, err := filepath.EvalSymlinks(r.keyFile)
	if err != nil {
		return fileSig{}, err
	}
	return fileSig{c.ModTime(), k.ModTime(), c.Size(), k.Size(), cr, kr}, nil
}

func (r *Reloader) load() (*loaded, error) {
	sig, err := r.stat()
	if err != nil {
		return nil, fmt.Errorf("tls files: %w", err)
	}
	pair, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return nil, fmt.Errorf("tls key pair: %w", err)
	}
	if pair.Leaf == nil {
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return nil, fmt.Errorf("tls leaf: %w", err)
		}
		pair.Leaf = leaf
	}
	if err := pair.Leaf.VerifyHostname(r.hostname); err != nil {
		return nil, fmt.Errorf("certificate does not cover %q: %w", r.hostname, err)
	}
	raw, err := os.ReadFile(r.certFile)
	if err != nil {
		return nil, fmt.Errorf("tls chain: %w", err)
	}
	pool := x509.NewCertPool()
	for rest := raw; ; {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return nil, fmt.Errorf("tls chain: %w", err)
		}
		pool.AddCert(c)
	}
	return &loaded{cert: &pair, pool: pool, notAfter: pair.Leaf.NotAfter, sig: sig}, nil
}

// GetCertificate implements tls.Config.GetCertificate.
func (r *Reloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return r.cur.Load().cert, nil
}

// Pool contains the served chain; the self-test uses it as its trust anchor so
// it verifies the real certificate without depending on system roots.
func (r *Reloader) Pool() *x509.CertPool { return r.cur.Load().pool }

// NotAfter is the expiry of the certificate currently served.
func (r *Reloader) NotAfter() time.Time { return r.cur.Load().notAfter }

// Poll reloads if the files changed. It reports whether a new certificate is
// now in service.
func (r *Reloader) Poll() (bool, error) {
	sig, err := r.stat()
	if err != nil {
		r.fail()
		return false, err
	}
	if sig == r.cur.Load().sig {
		return false, nil
	}
	l, err := r.load()
	if err != nil {
		r.fail()
		return false, err
	}
	r.cur.Store(l)
	return true, nil
}

func (r *Reloader) fail() {
	if r.onFail != nil {
		r.onFail()
	}
}

// Run polls until ctx ends. Owner: the caller; exit: ctx.Done.
func (r *Reloader) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			switch changed, err := r.Poll(); {
			case err != nil && !errors.Is(err, os.ErrNotExist):
				r.log.Error("certificate reload failed, keeping the previous one", "err", err)
			case err != nil:
				r.log.Error("certificate files missing, keeping the previous one", "err", err)
			case changed:
				r.log.Info("certificate reloaded", "not_after", r.NotAfter().UTC().Format(time.RFC3339))
			}
		}
	}
}
