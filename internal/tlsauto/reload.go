package tlsauto

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// Mode decision ---------------------------------------------------------------------------------

// Decide says which mode applies and with which files. The files are provided when both variables
// are set, or neither is and both default files exist; neither variable and no default file is the
// automatic mode. Exactly one of a pair is an error: the bridge never guesses which one is meant.
func Decide(certSet, keySet bool, certFile, keyFile string, fileExists func(string) bool) (Mode, error) {
	switch {
	case certSet != keySet:
		return "", errors.New("only one of EASBRIDGE_TLS_CERT and EASBRIDGE_TLS_KEY is set; set both to use your own certificate, or neither for the automatic one")
	case certSet:
		return Provided, nil
	}
	c, k := fileExists(certFile), fileExists(keyFile)
	switch {
	case c && k:
		return Provided, nil
	case c || k:
		which, other := certFile, keyFile
		if k {
			which, other = keyFile, certFile
		}
		return "", fmt.Errorf("only %s exists, not %s; provide both files, or remove it to use the automatic certificate", which, other)
	}
	return Automatic, nil
}

// Reloader serves a certificate from two files and picks up a renewed one without a restart
// (checked at most once per Recheck, on a handshake; Invalidate forces the next handshake to look).
type Reloader struct {
	certFile, keyFile string
	recheck           time.Duration

	mu              sync.Mutex
	cert            *tls.Certificate
	leaf            *x509.Certificate
	loaded          time.Time
	certMod, keyMod time.Time
}

// NewReloader loads the pair once (an error if it cannot be used) and returns the reloader.
func NewReloader(certFile, keyFile string, recheck time.Duration) (*Reloader, error) {
	if recheck <= 0 {
		recheck = time.Minute
	}
	r := &Reloader{certFile: certFile, keyFile: keyFile, recheck: recheck}
	if _, err := r.Get(); err != nil {
		return nil, fmt.Errorf("the TLS certificate %s with the key %s cannot be used: %w", certFile, keyFile, err)
	}
	return r, nil
}

// Invalidate makes the next Get look at the files.
func (r *Reloader) Invalidate() {
	r.mu.Lock()
	r.loaded = time.Time{}
	r.mu.Unlock()
}

// Leaf is the certificate currently served.
func (r *Reloader) Leaf() *x509.Certificate {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.leaf
}

// TLSConfig is the server configuration of the listeners.
func (r *Reloader) TLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return r.Get() }}
}

// Get returns the certificate to serve.
func (r *Reloader) Get() (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cert != nil && time.Since(r.loaded) < r.recheck {
		return r.cert, nil
	}
	cs, err1 := os.Stat(r.certFile)
	ks, err2 := os.Stat(r.keyFile)
	if err1 != nil || err2 != nil {
		if r.cert != nil {
			return r.cert, nil // keep serving the old one
		}
		if err1 != nil {
			return nil, err1
		}
		return nil, err2
	}
	if r.cert != nil && cs.ModTime().Equal(r.certMod) && ks.ModTime().Equal(r.keyMod) {
		r.loaded = time.Now()
		return r.cert, nil
	}
	c, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		if r.cert != nil {
			return r.cert, nil
		}
		return nil, err
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		if r.cert != nil {
			return r.cert, nil
		}
		return nil, err
	}
	r.cert, r.leaf, r.loaded, r.certMod, r.keyMod = &c, leaf, time.Now(), cs.ModTime(), ks.ModTime()
	return r.cert, nil
}
