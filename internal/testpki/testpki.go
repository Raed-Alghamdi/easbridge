// Package testpki makes a throwaway certificate authority and server
// certificate for tests and for the compatibility harness. Nothing here is
// used by the production binary.
package testpki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// PKI is a CA plus one server certificate for DNS names easbridge and
// localhost and the loopback addresses.
type PKI struct {
	CAPEM   []byte
	CertPEM []byte // leaf certificate
	KeyPEM  []byte // leaf private key
	caPool  *x509.CertPool
}

// New generates a fresh PKI valid for 48 hours.
func New() (*PKI, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "easbridge test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(48 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, err
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "easbridge"},
		DNSNames:     []string{"easbridge", "localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(48 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	return &PKI{
		CAPEM:   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		caPool:  pool,
	}, nil
}

// ServerConfig returns a TLS config presenting the leaf certificate.
func (p *PKI) ServerConfig() *tls.Config {
	cert, err := tls.X509KeyPair(p.CertPEM, p.KeyPEM)
	if err != nil {
		panic(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
}

// ClientConfig returns a TLS config that trusts only this PKI's CA.
func (p *PKI) ClientConfig(serverName string) *tls.Config {
	return &tls.Config{RootCAs: p.caPool, ServerName: serverName, MinVersion: tls.VersionTLS12}
}

// Pool returns the CA pool.
func (p *PKI) Pool() *x509.CertPool { return p.caPool }

// WriteFiles writes ca.pem, server.pem and server-key.pem into dir and returns
// their paths.
func (p *PKI) WriteFiles(dir string) (ca, cert, key string, err error) {
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	ca, cert, key = filepath.Join(dir, "ca.pem"), filepath.Join(dir, "server.pem"), filepath.Join(dir, "server-key.pem")
	if err = os.WriteFile(ca, p.CAPEM, 0o644); err != nil {
		return
	}
	if err = os.WriteFile(cert, p.CertPEM, 0o644); err != nil {
		return
	}
	err = os.WriteFile(key, p.KeyPEM, 0o600)
	return
}
