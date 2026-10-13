// Package tlsauto gives the bridge a certificate when none is provided: a private CA and a
// server certificate made from it, kept in a directory of the data volume, reused across
// starts and renewed from the same CA before the server certificate expires. The CA never
// changes, so a client that trusts it once keeps trusting every later server certificate.
//
// It also holds the certificate reloader the listeners use in both modes, and the rule that
// decides which mode applies.
package tlsauto

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/big"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"easbridge/internal/config"
)

// File names in the automatic directory (<data dir>/tls).
const (
	CAFile        = "ca.pem"
	CAKeyFile     = "ca-key.pem"
	ServerFile    = "server.pem"
	ServerKeyFile = "server-key.pem"
)

// Lifetimes. The server certificate is renewed RenewBefore its end.
const (
	CAYears     = 20
	ServerYears = 2
	RenewBefore = 60 * 24 * time.Hour
	// skew is how far before now a new certificate is valid.
	skew = time.Hour
)

// Mode says where the certificate comes from.
type Mode string

// The two modes.
const (
	Automatic Mode = "automatic"
	Provided  Mode = "provided"
)

// Names are the names of the server certificate.
//
// DNS and IPs are required: easbridge, the entries of EASBRIDGE_TLS_NAMES and EASBRIDGE_LISTEN_IP. A
// required name that the CA is not allowed to certify (section "name constraints" in the package
// comment of constraints.go) stops the start. HostDNS (the host name) and AutoIPs (the addresses
// of the interfaces) are best effort: they go into the certificate when the CA may certify them and are
// left out, silently, when it may not (a recreated container has a new host name).
type Names struct {
	DNS     []string
	IPs     []netip.Addr
	HostDNS []string
	AutoIPs []netip.Addr
}

// ResolveNames collects the names: DNS easbridge, the entries of EASBRIDGE_TLS_NAMES (DNS names or IP
// literals) and EASBRIDGE_LISTEN_IP as required names; the host name and every non-loopback,
// non-link-local address of the interfaces as best-effort names.
func ResolveNames(hostname string, extra []string, listenIP string, ifaces []config.Iface) Names {
	dns := map[string]bool{"easbridge": true}
	ips := map[netip.Addr]bool{}
	auto := map[netip.Addr]bool{}
	addIP := func(s string) {
		if ip, err := netip.ParseAddr(s); err == nil {
			ips[ip.Unmap()] = true
		}
	}
	for _, n := range extra {
		if _, err := netip.ParseAddr(n); err == nil {
			addIP(n)
		} else if n != "" {
			dns[n] = true
		}
	}
	if listenIP != "" {
		addIP(listenIP)
	}
	for _, f := range ifaces {
		if f.Loopback || f.Name == "lo" {
			continue
		}
		for _, a := range f.Addrs {
			a = a.Unmap()
			if a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsUnspecified() || ips[a] {
				continue
			}
			auto[a] = true
		}
	}
	var n Names
	for d := range dns {
		n.DNS = append(n.DNS, d)
	}
	sort.Strings(n.DNS)
	if h := strings.ToLower(strings.TrimSpace(hostname)); h != "" && !strings.ContainsAny(h, " /_") && !dns[h] {
		n.HostDNS = []string{h}
	}
	n.IPs = sortedAddrs(ips)
	n.AutoIPs = sortedAddrs(auto)
	return n
}

func sortedAddrs(m map[netip.Addr]bool) []netip.Addr {
	var out []netip.Addr
	for ip := range m {
		out = append(out, ip)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Less(out[j]) })
	return out
}

// Options are what Open needs.
type Options struct {
	// Dir is the directory of the automatic files (created 0700).
	Dir   string
	Names Names
	// Now replaces the clock (tests).
	Now func() time.Time
	Log *slog.Logger
}

// Manager owns the automatic CA and server certificate.
type Manager struct {
	o      Options
	mu     sync.Mutex
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
	caPEM  []byte
	leaf   *x509.Certificate
}

func (m *Manager) now() time.Time {
	if m.o.Now != nil {
		return m.o.Now()
	}
	return time.Now()
}

// Open loads the files of Dir, making what is missing: with no file at all a new CA and a
// server certificate; with a valid CA, a server certificate if there is none, or none that is
// valid for the names and the time. A CA that is incomplete, does not match its key or has
// expired is never replaced: that would change what the clients trust, and the files are left
// as they are.
func Open(o Options) (*Manager, error) {
	m := &Manager{o: o}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("the directory of the automatic certificate (%s): %w", o.Dir, err)
	}
	if err := os.Chmod(o.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("the directory of the automatic certificate (%s): %w", o.Dir, err)
	}
	caExists, keyExists := exists(m.path(CAFile)), exists(m.path(CAKeyFile))
	switch {
	case !caExists && !keyExists:
		if exists(m.path(ServerFile)) || exists(m.path(ServerKeyFile)) {
			// A server certificate without its CA cannot be reissued: never start over silently.
			return nil, fmt.Errorf("the automatic certificate files in %s are incomplete (%s and %s are missing, but a server certificate is there); "+
				"remove the directory to make a new set, and trust the new CA again", o.Dir, CAFile, CAKeyFile)
		}
		if err := checkNew(m.o.Names); err != nil {
			return nil, err
		}
		if err := m.newCA(); err != nil {
			return nil, err
		}
		if _, err := m.issue(); err != nil {
			return nil, err
		}
		return m, nil
	case !caExists || !keyExists:
		missing := CAFile
		if caExists {
			missing = CAKeyFile
		}
		return nil, fmt.Errorf("the automatic certificate files in %s are incomplete (%s is missing); remove the directory to make a new set, and trust the new CA again", o.Dir, missing)
	}
	if err := m.loadCA(); err != nil {
		return nil, err
	}
	if _, _, constrained := constraintsOf(m.caCert); !constrained && o.Log != nil {
		o.Log.Warn("the automatic CA in " + o.Dir + " was made without name constraints, so it could certify any name; remove the directory to get a constrained CA (then MailFlow's backend must be restarted to trust it)")
	}
	if _, err := m.Renew(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) path(f string) string { return filepath.Join(m.o.Dir, f) }

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func randSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

func (m *Manager) newCA() error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	id := make([]byte, 4)
	if _, err := rand.Read(id); err != nil {
		return err
	}
	serial, err := randSerial()
	if err != nil {
		return err
	}
	now := m.now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "EASBridge local CA " + hex.EncodeToString(id), Organization: []string{"EASBridge"}},
		NotBefore:             now.Add(-skew),
		NotAfter:              now.AddDate(CAYears, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		// Critical name constraints: see constraints.go.
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         append(append([]string(nil), m.o.Names.DNS...), m.o.Names.HostDNS...),
		PermittedIPRanges:           permittedNets(),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	// The key first: a certificate without a key is the state loadCA refuses, a key without a
	// certificate the same, but never a certificate whose key was not saved.
	if err := writeFile(m.path(CAKeyFile), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	if err := writeFile(m.path(CAFile), caPEM, 0o644); err != nil {
		return err
	}
	m.caCert, m.caKey, m.caPEM = cert, key, caPEM
	return nil
}

func (m *Manager) loadCA() error {
	caPEM, err := os.ReadFile(m.path(CAFile))
	if err != nil {
		return err
	}
	keyPEM, err := os.ReadFile(m.path(CAKeyFile))
	if err != nil {
		return err
	}
	cert, err := parseCert(caPEM)
	if err != nil {
		return fmt.Errorf("%s in %s: %w; the files are left as they are", CAFile, m.o.Dir, err)
	}
	key, err := parseKey(keyPEM)
	if err != nil {
		return fmt.Errorf("%s in %s: %w; the files are left as they are", CAKeyFile, m.o.Dir, err)
	}
	if !cert.IsCA || !samePublicKey(cert.PublicKey, &key.PublicKey) {
		return fmt.Errorf("%s and %s in %s do not belong together (or the certificate is not a CA); the files are left as they are, remove the directory to make a new set", CAFile, CAKeyFile, m.o.Dir)
	}
	if now := m.now(); now.After(cert.NotAfter) {
		return fmt.Errorf("the automatic CA in %s expired on %s; remove the directory to make a new set, and trust the new CA again", m.o.Dir, cert.NotAfter.Format("2006-01-02"))
	}
	m.caCert, m.caKey, m.caPEM = cert, key, caPEM
	return nil
}

func parseCert(b []byte) (*x509.Certificate, error) {
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, errors.New("is not a PEM certificate")
	}
	return x509.ParseCertificate(blk.Bytes)
}

func parseKey(b []byte) (*ecdsa.PrivateKey, error) {
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("is not a PEM key")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	ek, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("is not an ECDSA key")
	}
	return ek, nil
}

func samePublicKey(a any, b *ecdsa.PublicKey) bool {
	ak, ok := a.(*ecdsa.PublicKey)
	return ok && ak.Equal(b)
}

// Renew makes sure the server certificate in the directory is valid now, covers the names and has
// more than RenewBefore left; otherwise it issues a new one from the same CA and says so. The
// caller invalidates its reloader when it returns true.
func (m *Manager) Renew() (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	eff, err := effective(m.caCert, m.o.Names, m.o.Dir)
	if err != nil {
		return false, err
	}
	if reason := m.serverProblem(eff); reason != "" {
		leaf, err := m.issueLocked(eff)
		if err != nil {
			return false, err
		}
		if m.o.Log != nil {
			m.o.Log.Info("TLS server certificate issued from the existing CA", "reason", reason, "expires", leaf.NotAfter.Format("2006-01-02"))
		}
		return true, nil
	}
	return false, nil
}

// Run checks the server certificate every interval until ctx ends and calls after with the
// outcome (renewed, or the error). The listeners' reloader is told by after.
func (m *Manager) Run(ctx context.Context, interval time.Duration, after func(renewed bool, err error)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			renewed, err := m.Renew()
			if err != nil && m.o.Log != nil {
				m.o.Log.Error("renewing the TLS server certificate failed; the current one is served", "err", err)
			}
			if after != nil {
				after(renewed, err)
			}
		}
	}
}

// serverProblem returns why the files hold no usable server certificate ("" when they do), and
// loads the certificate when they do.
func (m *Manager) serverProblem(eff Names) string {
	certPEM, err1 := os.ReadFile(m.path(ServerFile))
	keyPEM, err2 := os.ReadFile(m.path(ServerKeyFile))
	if err1 != nil || err2 != nil {
		return "no server certificate"
	}
	leaf, err := parseCert(certPEM)
	if err != nil {
		return "the server certificate cannot be read"
	}
	key, err := parseKey(keyPEM)
	if err != nil || !samePublicKey(leaf.PublicKey, &key.PublicKey) {
		return "the server key does not match the server certificate"
	}
	now := m.now()
	pool := x509.NewCertPool()
	pool.AddCert(m.caCert)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return "the server certificate is not valid for this CA now"
	}
	if leaf.NotAfter.Sub(now) <= RenewBefore {
		return "the server certificate ends within 60 days"
	}
	if !coversNames(leaf, eff) {
		return "the names changed"
	}
	m.leaf = leaf
	return ""
}

// coversNames: the DNS names are exactly those wanted, every wanted address is there.
func coversNames(leaf *x509.Certificate, n Names) bool {
	have := append([]string(nil), leaf.DNSNames...)
	sort.Strings(have)
	if strings.Join(have, ",") != strings.Join(n.DNS, ",") {
		return false
	}
	ips := map[netip.Addr]bool{}
	for _, ip := range leaf.IPAddresses {
		if a, ok := netip.AddrFromSlice(ip); ok {
			ips[a.Unmap()] = true
		}
	}
	for _, want := range n.IPs {
		if !ips[want] {
			return false
		}
	}
	return true
}

func (m *Manager) issue() (*x509.Certificate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	eff, err := effective(m.caCert, m.o.Names, m.o.Dir)
	if err != nil {
		return nil, err
	}
	return m.issueLocked(eff)
}

func (m *Manager) issueLocked(names Names) (*x509.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randSerial()
	if err != nil {
		return nil, err
	}
	now := m.now()
	notAfter := now.AddDate(ServerYears, 0, 0)
	if notAfter.After(m.caCert.NotAfter) {
		notAfter = m.caCert.NotAfter
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "easbridge", Organization: []string{"EASBridge"}},
		DNSNames:     append([]string(nil), names.DNS...),
		NotBefore:    now.Add(-skew),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, ip := range names.IPs {
		tmpl.IPAddresses = append(tmpl.IPAddresses, ip.AsSlice())
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, m.caCert, &key.PublicKey, m.caKey)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	// The key first, then the certificate: a reader that sees the new key with the old
	// certificate keeps serving the old pair (they do not match) until the certificate lands.
	if err := writeFile(m.path(ServerKeyFile), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return nil, err
	}
	if err := writeFile(m.path(ServerFile), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return nil, err
	}
	m.leaf = leaf
	return leaf, nil
}

// writeFile writes through a temporary file in the same directory and renames it into place, with
// exactly the mode given whatever the umask says.
func writeFile(path string, data []byte, mode fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(name)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// CAPEM is the CA certificate in PEM form.
func (m *Manager) CAPEM() []byte { return append([]byte(nil), m.caPEM...) }

// CASubject is the subject of the CA.
func (m *Manager) CASubject() string { return m.caCert.Subject.CommonName }

// CAFingerprint is the SHA-256 fingerprint of the CA certificate, upper case hex with colons.
func (m *Manager) CAFingerprint() string { return Fingerprint(m.caCert) }

// Fingerprint is the SHA-256 fingerprint of a certificate.
func Fingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	var parts []string
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}

// ServerNotAfter is when the current server certificate ends.
func (m *Manager) ServerNotAfter() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.leaf.NotAfter
}

// ServerFiles are the paths of the server certificate and key.
func (m *Manager) ServerFiles() (cert, key string) { return m.path(ServerFile), m.path(ServerKeyFile) }

// ExportCA writes the CA certificate to dir/ca.pem (0644) when the file is missing or differs.
func (m *Manager) ExportCA(dir string) (changed bool, err error) {
	dst := filepath.Join(dir, CAFile)
	if cur, err := os.ReadFile(dst); err == nil && string(cur) == string(m.caPEM) {
		if st, err := os.Stat(dst); err == nil && st.Mode().Perm() == 0o644 {
			return false, nil
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	if err := writeFile(dst, m.caPEM, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// ReadCA returns the CA certificate of an automatic directory without opening it as a manager
// (for `easbridge ca`).
func ReadCA(dir string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(dir, CAFile))
	if err != nil {
		return nil, err
	}
	if _, err := parseCert(b); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, CAFile), err)
	}
	return b, nil
}
