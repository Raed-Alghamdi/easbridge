// Command mintleaf signs a server certificate for one name with the automatic CA found in DIR (the
// files ca.pem and ca-key.pem), for the compatibility harness: it plays someone who got hold of the CA
// key and mints a certificate for a name the CA was not made for, and for one it was.
//
//	go run ./compat/mintleaf CA_DIR NAME OUT_DIR   ->  OUT_DIR/leaf.pem  OUT_DIR/leaf-key.pem
//
// Development tool only; it is not part of the production binary.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: mintleaf CA_DIR NAME OUT_DIR")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2], os.Args[3]); err != nil {
		fmt.Fprintln(os.Stderr, "mintleaf:", err)
		os.Exit(1)
	}
}

func run(caDir, name, out string) error {
	caPEM, err := os.ReadFile(filepath.Join(caDir, "ca.pem"))
	if err != nil {
		return err
	}
	keyPEM, err := os.ReadFile(filepath.Join(caDir, "ca-key.pem"))
	if err != nil {
		return err
	}
	cb, _ := pem.Decode(caPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return fmt.Errorf("no PEM in %s", caDir)
	}
	ca, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return err
	}
	k, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return err
	}
	caKey := k.(*ecdsa.PrivateKey)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano()), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	kd, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "leaf.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, "leaf-key.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}), 0o600)
}
