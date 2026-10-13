package tlsauto

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	crand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io/fs"
	"log/slog"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"easbridge/internal/config"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

var t0 = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

func names(dns []string, ips ...string) Names {
	n := Names{DNS: dns}
	for _, s := range ips {
		n.IPs = append(n.IPs, netip.MustParseAddr(s))
	}
	return n
}

func open(t *testing.T, dir string, n Names, c *clock, log *slog.Logger) *Manager {
	t.Helper()
	m, err := Open(Options{Dir: dir, Names: n, Now: c.Now, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func leafOf(t *testing.T, dir string) *x509.Certificate {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, ServerFile))
	if err != nil {
		t.Fatal(err)
	}
	c, err := parseCert(b)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func read(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFirstStartMakesACAAndAServerCertificate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	c := &clock{t: t0}
	m := open(t, dir, names([]string{"abc123", "easbridge"}, "10.0.0.20"), c, nil)

	// Directory and files: owner, permissions.
	st, err := os.Stat(dir)
	if err != nil || st.Mode().Perm() != 0o700 || !st.IsDir() {
		t.Fatalf("directory %v %v", st, err)
	}
	for f, want := range map[string]fs.FileMode{CAFile: 0o644, CAKeyFile: 0o600, ServerFile: 0o644, ServerKeyFile: 0o600} {
		fi, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s has mode %v, want %v", f, fi.Mode().Perm(), want)
		}
		if uid := fi.Sys().(*syscall.Stat_t).Uid; int(uid) != os.Getuid() {
			t.Errorf("%s is owned by %d, want the running user %d", f, uid, os.Getuid())
		}
	}
	if uid := st.Sys().(*syscall.Stat_t).Uid; int(uid) != os.Getuid() {
		t.Errorf("the directory is owned by %d", uid)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 4 {
		t.Errorf("%d entries in the directory (temporary files left?)", len(ents))
	}

	ca, err := parseCert(read(t, filepath.Join(dir, CAFile)))
	if err != nil {
		t.Fatal(err)
	}
	leaf := leafOf(t, dir)
	if !ca.IsCA || !ca.BasicConstraintsValid || ca.KeyUsage&x509.KeyUsageCertSign == 0 || !ca.MaxPathLenZero {
		t.Errorf("CA flags: %+v", ca)
	}
	if !strings.HasPrefix(ca.Subject.CommonName, "EASBridge local CA ") || len(strings.TrimPrefix(ca.Subject.CommonName, "EASBridge local CA ")) != 8 {
		t.Errorf("CA subject %q", ca.Subject.CommonName)
	}
	if m.CASubject() != ca.Subject.CommonName || m.CAFingerprint() != Fingerprint(ca) || strings.Count(m.CAFingerprint(), ":") != 31 {
		t.Errorf("subject %q fingerprint %q", m.CASubject(), m.CAFingerprint())
	}
	if want := c.Now().AddDate(20, 0, 0); !ca.NotAfter.Equal(want.Truncate(time.Second)) && ca.NotAfter.Sub(want).Abs() > 2*time.Second {
		t.Errorf("CA valid until %v, want about %v", ca.NotAfter, want)
	}
	if want := c.Now().AddDate(2, 0, 0); leaf.NotAfter.Sub(want).Abs() > 2*time.Second {
		t.Errorf("server certificate valid until %v, want about %v", leaf.NotAfter, want)
	}
	for _, k := range []any{ca.PublicKey, leaf.PublicKey} {
		pk, ok := k.(*ecdsa.PublicKey)
		if !ok || pk.Curve != elliptic.P256() {
			t.Errorf("key %T is not ECDSA P-256", k)
		}
	}
	if strings.Join(leaf.DNSNames, ",") != "abc123,easbridge" || len(leaf.IPAddresses) != 1 || !leaf.IPAddresses[0].Equal(net.ParseIP("10.0.0.20")) {
		t.Errorf("SANs %v %v", leaf.DNSNames, leaf.IPAddresses)
	}
	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth || leaf.IsCA {
		t.Errorf("leaf usage %+v", leaf.ExtKeyUsage)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	for _, name := range []string{"easbridge", "abc123"} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: name, CurrentTime: c.Now()}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "elsewhere", CurrentTime: c.Now()}); err == nil {
		t.Error("a name that is not in the certificate verified")
	}
	if !bytes.Equal(m.CAPEM(), read(t, filepath.Join(dir, CAFile))) || bytes.Contains(m.CAPEM(), []byte("PRIVATE")) {
		t.Error("CAPEM")
	}
	// Two installs never share a CA.
	m2 := open(t, filepath.Join(t.TempDir(), "tls"), names([]string{"easbridge"}), c, nil)
	if m2.CASubject() == m.CASubject() || bytes.Equal(m2.CAPEM(), m.CAPEM()) {
		t.Error("two fresh installs have the same CA")
	}
}

func TestALaterStartReusesTheFilesAndNeverRegeneratesAValidSet(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	c := &clock{t: t0}
	n := names([]string{"easbridge", "h1"}, "10.0.0.20")
	m := open(t, dir, n, c, nil)
	before := map[string][]byte{}
	mod := map[string]time.Time{}
	for _, f := range []string{CAFile, CAKeyFile, ServerFile, ServerKeyFile} {
		before[f] = read(t, filepath.Join(dir, f))
		fi, _ := os.Stat(filepath.Join(dir, f))
		mod[f] = fi.ModTime()
	}
	c.Add(400 * 24 * time.Hour)
	m2 := open(t, dir, n, c, nil)
	for f, b := range before {
		if !bytes.Equal(b, read(t, filepath.Join(dir, f))) {
			t.Errorf("%s was rewritten", f)
		}
		fi, _ := os.Stat(filepath.Join(dir, f))
		if !fi.ModTime().Equal(mod[f]) {
			t.Errorf("%s was touched", f)
		}
	}
	if m2.CAFingerprint() != m.CAFingerprint() || !m2.ServerNotAfter().Equal(m.ServerNotAfter()) {
		t.Error("another CA or certificate after a restart")
	}
	if renewed, err := m2.Renew(); renewed || err != nil {
		t.Errorf("Renew on a valid set: %v %v", renewed, err)
	}
}

func TestOtherNamesGetANewServerCertificateFromTheSameCA(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	c := &clock{t: t0}
	first := Names{DNS: []string{"easbridge"}, HostDNS: []string{"h1"}, IPs: []netip.Addr{netip.MustParseAddr("10.0.0.20")}}
	m := open(t, dir, first, c, nil)
	ca, caKey := read(t, filepath.Join(dir, CAFile)), read(t, filepath.Join(dir, CAKeyFile))
	step := func(label string, n Names, wantNew bool) {
		t.Helper()
		serial0 := leafOf(t, dir).SerialNumber
		m2 := open(t, dir, n, c, nil)
		if got := leafOf(t, dir).SerialNumber.Cmp(serial0) != 0; got != wantNew {
			t.Errorf("%s: new certificate = %v, want %v", label, got, wantNew)
		}
		if !bytes.Equal(ca, read(t, filepath.Join(dir, CAFile))) || !bytes.Equal(caKey, read(t, filepath.Join(dir, CAKeyFile))) || m2.CAFingerprint() != m.CAFingerprint() {
			t.Errorf("%s: the CA changed", label)
		}
	}
	step("same names", first, false)
	// A recreated container has another host name: the CA was not made for it, so it is left out of the
	// certificate (best effort) and the certificate that named the old host name is replaced once.
	step("another host name", Names{DNS: []string{"easbridge"}, HostDNS: []string{"h2"}, IPs: first.IPs}, true)
	step("and the same again", Names{DNS: []string{"easbridge"}, HostDNS: []string{"h3"}, IPs: first.IPs}, false)
	// Addresses: a new required address (EASBRIDGE_LISTEN_IP) reissues, one that goes away does not, an
	// interface address outside the private ranges is left out.
	step("a new address", Names{DNS: []string{"easbridge"}, IPs: []netip.Addr{netip.MustParseAddr("10.0.0.20"), netip.MustParseAddr("10.1.0.5")}}, true)
	step("an address gone", Names{DNS: []string{"easbridge"}, IPs: []netip.Addr{netip.MustParseAddr("10.1.0.5")}}, false)
	step("an interface address in the private ranges", Names{DNS: []string{"easbridge"}, IPs: []netip.Addr{netip.MustParseAddr("10.1.0.5")}, AutoIPs: []netip.Addr{netip.MustParseAddr("192.168.1.9")}}, true)
	step("a public interface address is left out", Names{DNS: []string{"easbridge"}, IPs: []netip.Addr{netip.MustParseAddr("10.1.0.5")}, AutoIPs: []netip.Addr{netip.MustParseAddr("192.168.1.9"), netip.MustParseAddr("203.0.113.9")}}, false)
	// A name below a permitted domain is permitted (X.509 reads a constraint "easbridge" so).
	step("a subdomain of a permitted name", Names{DNS: []string{"easbridge", "api.easbridge"}, IPs: []netip.Addr{netip.MustParseAddr("10.1.0.5")}}, true)
	pool := x509.NewCertPool()
	pool.AddCert(m.caCert)
	if _, err := leafOf(t, dir).Verify(x509.VerifyOptions{Roots: pool, CurrentTime: c.Now(), DNSName: "api.easbridge"}); err != nil {
		t.Errorf("the reissued certificate does not verify with the first CA: %v", err)
	}
}

func TestTheServerCertificateIsRenewedSixtyDaysBeforeItEndsFromTheSameCA(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	c := &clock{t: t0}
	n := names([]string{"easbridge"})
	m := open(t, dir, n, c, nil)
	ca := read(t, filepath.Join(dir, CAFile))
	first := leafOf(t, dir)
	c.Add(2*365*24*time.Hour - 70*24*time.Hour) // 70 days left (leap days make two years a day or so longer)
	if renewed, err := m.Renew(); renewed || err != nil {
		t.Fatalf("renewed with 70 days left: %v %v", renewed, err)
	}
	c.Add(15 * 24 * time.Hour) // 55 days left
	renewed, err := m.Renew()
	if err != nil || !renewed {
		t.Fatalf("not renewed with 55 days left: %v %v", renewed, err)
	}
	second := leafOf(t, dir)
	if second.SerialNumber.Cmp(first.SerialNumber) == 0 || !second.NotAfter.After(first.NotAfter.Add(300*24*time.Hour)) {
		t.Errorf("the renewed certificate: %v until %v (was %v)", second.SerialNumber, second.NotAfter, first.NotAfter)
	}
	if !bytes.Equal(ca, read(t, filepath.Join(dir, CAFile))) || m.ServerNotAfter().Before(second.NotAfter) {
		t.Error("the CA changed, or the manager does not know the new certificate")
	}
	if m.ServerNotAfter().Sub(c.Now()) < 700*24*time.Hour {
		t.Errorf("only %v left on the new certificate", m.ServerNotAfter().Sub(c.Now()))
	}
	// A start long after: the container was off for the whole last stretch.
	c.Add(700 * 24 * time.Hour)
	m3 := open(t, dir, n, c, nil)
	if m3.CAFingerprint() != m.CAFingerprint() || m3.ServerNotAfter().Sub(c.Now()) < 700*24*time.Hour {
		t.Error("a start with an expired server certificate did not renew it from the same CA")
	}
	if renewed, _ := m3.Renew(); renewed {
		t.Error("renewed twice")
	}
}

func TestACAThatCannotBeUsedIsNeverReplaced(t *testing.T) {
	c := &clock{t: t0}
	n := names([]string{"easbridge"})
	snapshot := func(dir string) map[string][]byte {
		out := map[string][]byte{}
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			out[e.Name()] = read(t, filepath.Join(dir, e.Name()))
		}
		return out
	}
	for name, damage := range map[string]func(dir string){
		"CA key missing":      func(d string) { os.Remove(filepath.Join(d, CAKeyFile)) },
		"CA certificate gone": func(d string) { os.Remove(filepath.Join(d, CAFile)) },
		"CA gone, server stays": func(d string) {
			os.Remove(filepath.Join(d, CAFile))
			os.Remove(filepath.Join(d, CAKeyFile))
		},
		"CA certificate garbled": func(d string) { os.WriteFile(filepath.Join(d, CAFile), []byte("not a certificate"), 0o644) },
		"CA key of another CA": func(d string) {
			other := t.TempDir()
			open(t, other, n, c, nil)
			b, _ := os.ReadFile(filepath.Join(other, CAKeyFile))
			os.WriteFile(filepath.Join(d, CAKeyFile), b, 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "tls")
			open(t, dir, n, c, nil)
			damage(dir)
			before := snapshot(dir)
			_, err := Open(Options{Dir: dir, Names: n, Now: c.Now})
			if err == nil {
				t.Fatal("started with a broken CA")
			}
			after := snapshot(dir)
			if len(before) != len(after) {
				t.Errorf("files appeared or went: %v -> %v", keys(before), keys(after))
			}
			for f, b := range before {
				if !bytes.Equal(b, after[f]) {
					t.Errorf("%s was changed", f)
				}
			}
		})
	}
	t.Run("expired CA", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "tls")
		open(t, dir, n, c, nil)
		late := &clock{t: t0.AddDate(21, 0, 0)}
		if _, err := Open(Options{Dir: dir, Names: n, Now: late.Now}); err == nil || !strings.Contains(err.Error(), "expired") {
			t.Errorf("%v", err)
		}
	})
}

func keys(m map[string][]byte) []string {
	var k []string
	for n := range m {
		k = append(k, n)
	}
	return k
}

func TestAServerCertificateThatDoesNotMatchItsKeyIsReplacedFromTheCA(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	c := &clock{t: t0}
	n := names([]string{"easbridge"})
	m := open(t, dir, n, c, nil)
	other := t.TempDir()
	open(t, other, n, c, nil)
	os.WriteFile(filepath.Join(dir, ServerKeyFile), read(t, filepath.Join(other, ServerKeyFile)), 0o600)
	m2 := open(t, dir, n, c, nil)
	if m2.CAFingerprint() != m.CAFingerprint() {
		t.Error("the CA changed")
	}
	if _, err := tls.LoadX509KeyPair(filepath.Join(dir, ServerFile), filepath.Join(dir, ServerKeyFile)); err != nil {
		t.Errorf("the pair is still broken: %v", err)
	}
}

func TestTheCAIsExportedAndKeptUpToDate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	m := open(t, dir, names([]string{"easbridge"}), &clock{t: t0}, nil)
	exp := filepath.Join(t.TempDir(), "ca-out")
	changed, err := m.ExportCA(exp)
	if err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	dst := filepath.Join(exp, CAFile)
	fi, _ := os.Stat(dst)
	if !bytes.Equal(read(t, dst), m.CAPEM()) || fi.Mode().Perm() != 0o644 {
		t.Errorf("export %v", fi.Mode())
	}
	if changed, err := m.ExportCA(exp); changed || err != nil {
		t.Errorf("unchanged export reported %v %v", changed, err)
	}
	os.WriteFile(dst, []byte("stale"), 0o644)
	if changed, err := m.ExportCA(exp); !changed || err != nil || !bytes.Equal(read(t, dst), m.CAPEM()) {
		t.Errorf("a stale export was not replaced: %v %v", changed, err)
	}
	os.Chmod(dst, 0o600)
	if changed, _ := m.ExportCA(exp); !changed {
		t.Error("the mode was not repaired")
	}
	os.Remove(dst)
	if changed, _ := m.ExportCA(exp); !changed {
		t.Error("a deleted export was not written again")
	}
	// A directory that cannot be written is an error, not silence.
	if _, err := m.ExportCA(filepath.Join(dst, "nope")); err == nil {
		t.Error("export into a file path succeeded")
	}
}

func TestNothingSecretReachesTheLog(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	c := &clock{t: t0}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m := open(t, dir, names([]string{"easbridge"}), c, log)
	c.Add(2*365*24*time.Hour - 10*24*time.Hour)
	m.Renew()
	out := buf.String()
	keyPEM := string(read(t, filepath.Join(dir, CAKeyFile)))
	srvKey := string(read(t, filepath.Join(dir, ServerKeyFile)))
	for _, bad := range []string{"PRIVATE KEY", "BEGIN", strings.Split(keyPEM, "\n")[1], strings.Split(srvKey, "\n")[1]} {
		if strings.Contains(out, bad) {
			t.Errorf("the log contains %q", bad)
		}
	}
	if !strings.Contains(out, "issued from the existing CA") {
		t.Errorf("no log line about the renewal: %s", out)
	}
}

func TestResolveNames(t *testing.T) {
	ifs := []config.Iface{
		{Name: "lo", Loopback: true, Addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("::1")}},
		{Name: "eth0", Addrs: []netip.Addr{netip.MustParseAddr("10.0.0.20"), netip.MustParseAddr("fe80::42"), netip.MustParseAddr("fd00::5")}},
		{Name: "eth1", Addrs: []netip.Addr{netip.MustParseAddr("::ffff:10.1.0.3")}},
	}
	n := ResolveNames("Abc123", []string{"mail.internal", "192.0.2.9", "easbridge"}, "10.0.0.20", ifs)
	if got := strings.Join(n.DNS, ","); got != "easbridge,mail.internal" || strings.Join(n.HostDNS, ",") != "abc123" {
		t.Errorf("DNS %s, host %v: the host name is best effort, the others are required", got, n.HostDNS)
	}
	str := func(ips []netip.Addr) string {
		var out []string
		for _, ip := range ips {
			out = append(out, ip.String())
		}
		return strings.Join(out, ",")
	}
	if got := str(n.IPs); got != "10.0.0.20,192.0.2.9" {
		t.Errorf("required IPs %s", got)
	}
	if got := str(n.AutoIPs); got != "10.1.0.3,fd00::5" {
		t.Errorf("interface IPs %s (loopback, link-local and the required ones are left out)", got)
	}
	if n := ResolveNames("", nil, "", nil); strings.Join(n.DNS, ",") != "easbridge" || len(n.IPs) != 0 || len(n.HostDNS) != 0 {
		t.Errorf("bare: %+v", n)
	}
}

func TestDecideTheMode(t *testing.T) {
	has := func(files ...string) func(string) bool {
		return func(p string) bool {
			for _, f := range files {
				if f == p {
					return true
				}
			}
			return false
		}
	}
	for _, tc := range []struct {
		name         string
		certSet, key bool
		exists       func(string) bool
		want         Mode
		err          string
	}{
		{"nothing at all", false, false, has(), Automatic, ""},
		{"both variables", true, true, has(), Provided, ""},
		{"both variables, files there", true, true, has("/c", "/k"), Provided, ""},
		{"only the certificate variable", true, false, has("/c", "/k"), "", "only one of EASBRIDGE_TLS_CERT and EASBRIDGE_TLS_KEY"},
		{"only the key variable", false, true, has(), "", "only one of"},
		{"both default files", false, false, has("/c", "/k"), Provided, ""},
		{"only the default certificate", false, false, has("/c"), "", "only /c exists, not /k"},
		{"only the default key", false, false, has("/k"), "", "only /k exists, not /c"},
	} {
		got, err := Decide(tc.certSet, tc.key, "/c", "/k", tc.exists)
		switch {
		case tc.err == "" && (err != nil || got != tc.want):
			t.Errorf("%s: %v %v", tc.name, got, err)
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

// A client of the listener sees the renewed certificate without a restart of the listener or the
// bridge, while it is being served, and verifies it with the CA it trusted the first time.
func TestARenewalIsServedWithoutARestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	c := &clock{t: t0}
	n := names([]string{"easbridge"})
	m := open(t, dir, n, c, nil)
	certFile, keyFile := m.ServerFiles()
	r, err := NewReloader(certFile, keyFile, time.Hour) // the long recheck: only Invalidate gets the new one in
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", r.TLSConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { conn.(*tls.Conn).Handshake(); conn.Close() }()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx, 5*time.Millisecond, func(renewed bool, err error) {
		if renewed {
			r.Invalidate()
		}
	})
	pool := x509.NewCertPool()
	pool.AddCert(m.caCert)
	serial := func() string {
		conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{RootCAs: pool, ServerName: "easbridge", Time: c.Now})
		if err != nil {
			t.Fatalf("a client that trusts the CA: %v", err)
		}
		defer conn.Close()
		return conn.ConnectionState().PeerCertificates[0].SerialNumber.String()
	}
	first := serial()
	time.Sleep(30 * time.Millisecond)
	if serial() != first {
		t.Fatal("the certificate changed with 2 years left")
	}
	c.Add(2*365*24*time.Hour - 59*24*time.Hour)
	deadline := time.Now().Add(5 * time.Second)
	var now string
	for time.Now().Before(deadline) {
		if now = serial(); now != first {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if now == first {
		t.Fatal("the renewed certificate was never served")
	}
	// An untrusting client (no CA) still refuses: the verification is real.
	if _, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{ServerName: "easbridge"}); err == nil {
		t.Error("a client without the CA accepted the certificate")
	}
}

func TestTheReloaderKeepsServingWhenAPairIsBroken(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	c := &clock{t: t0}
	m := open(t, dir, names([]string{"easbridge"}), c, nil)
	certFile, keyFile := m.ServerFiles()
	r, err := NewReloader(certFile, keyFile, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	first := r.Leaf().SerialNumber.String()
	os.WriteFile(keyFile, []byte("garbage"), 0o600)
	time.Sleep(5 * time.Millisecond)
	cert, err := r.Get()
	if err != nil || cert == nil || r.Leaf().SerialNumber.String() != first {
		t.Errorf("the old pair is not kept: %v", err)
	}
	if _, err := NewReloader(certFile, keyFile, time.Minute); err == nil || !strings.Contains(err.Error(), keyFile) {
		t.Errorf("a broken pair at start: %v", err)
	}
}

// ---- name constraints ----------------------------------------------------------------------------

func TestTheCAHasCriticalNameConstraints(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	open(t, dir, Names{DNS: []string{"easbridge", "mail.internal"}, HostDNS: []string{"abc123"}, IPs: []netip.Addr{netip.MustParseAddr("10.0.0.20")}}, &clock{t: t0}, nil)
	ca, err := parseCert(read(t, filepath.Join(dir, CAFile)))
	if err != nil {
		t.Fatal(err)
	}
	if !ca.PermittedDNSDomainsCritical {
		t.Error("the constraints are not critical: a client that does not know them would ignore them")
	}
	if got := strings.Join(ca.PermittedDNSDomains, ","); got != "easbridge,mail.internal,abc123" {
		t.Errorf("permitted DNS domains %q: the names the CA was made for", got)
	}
	var ranges []string
	for _, n := range ca.PermittedIPRanges {
		ranges = append(ranges, n.String())
	}
	want := "127.0.0.0/8,::1/128,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,fc00::/7,169.254.0.0/16,fe80::/10"
	if got := strings.Join(ranges, ","); got != want {
		t.Errorf("permitted IP ranges %q, want %q", got, want)
	}
	if len(ca.ExcludedDNSDomains) != 0 {
		t.Error("excluded domains set")
	}
}

// Whoever holds the CA key (it lives in the data volume) can sign anything; the clients must refuse a
// name outside the constraints. Go enforces them like OpenSSL does; compat/ checks it with Node.
func TestALeafForAnotherNameFromTheSameCAIsRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	c := &clock{t: t0}
	m := open(t, dir, Names{DNS: []string{"easbridge"}, HostDNS: []string{"abc123"}, IPs: []netip.Addr{netip.MustParseAddr("10.0.0.20")}}, c, nil)
	mint := func(dns []string, ips ...string) *x509.Certificate {
		t.Helper()
		key, _ := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(77), Subject: pkix.Name{CommonName: "x"}, DNSNames: dns,
			NotBefore: t0.Add(-time.Hour), NotAfter: t0.AddDate(1, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		for _, ip := range ips {
			tmpl.IPAddresses = append(tmpl.IPAddresses, net.ParseIP(ip))
		}
		der, err := x509.CreateCertificate(crand.Reader, tmpl, m.caCert, &key.PublicKey, m.caKey)
		if err != nil {
			t.Fatal(err)
		}
		leaf, _ := x509.ParseCertificate(der)
		return leaf
	}
	pool := x509.NewCertPool()
	pool.AddCert(m.caCert)
	verify := func(leaf *x509.Certificate, name string) error {
		_, err := leaf.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: t0, DNSName: name, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
		return err
	}
	if err := verify(mint([]string{"easbridge"}, "10.0.0.5"), "easbridge"); err != nil {
		t.Errorf("a leaf for the permitted name: %v", err)
	}
	for _, name := range []string{"mail.example.com", "evil.example.test", "easbridge.example.com", "xeasbridge"} {
		if err := verify(mint([]string{name}), name); err == nil {
			t.Errorf("a leaf for %q verified", name)
		}
	}
	if err := verify(mint([]string{"easbridge"}, "8.8.8.8"), "easbridge"); err == nil {
		t.Error("a leaf with a public address verified")
	}
	if err := verify(mint(nil, "203.0.113.7"), ""); err == nil {
		t.Error("a leaf for a public address verified")
	}
}

func TestANameOutsideTheConstraintsStopsTheStartWithTheWayOut(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	c := &clock{t: t0}
	base := Names{DNS: []string{"easbridge"}, IPs: []netip.Addr{netip.MustParseAddr("10.0.0.20")}}
	open(t, dir, base, c, nil)
	before := map[string][]byte{}
	for _, f := range []string{CAFile, CAKeyFile, ServerFile, ServerKeyFile} {
		before[f] = read(t, filepath.Join(dir, f))
	}
	for name, n := range map[string]Names{
		"a new DNS name (EASBRIDGE_TLS_NAMES)": {DNS: []string{"easbridge", "mail.example.com"}, IPs: base.IPs},
		"a public address (LISTEN_IP)":         {DNS: []string{"easbridge"}, IPs: []netip.Addr{netip.MustParseAddr("203.0.113.7")}},
	} {
		_, err := Open(Options{Dir: dir, Names: n, Now: c.Now})
		if err == nil {
			t.Errorf("%s: the start was not refused", name)
			continue
		}
		msg := err.Error()
		for _, want := range []string{"may not certify", "remove the directory " + dir, "restart MailFlow's backend", "easbridge"} {
			if !strings.Contains(msg, want) {
				t.Errorf("%s: the message lacks %q: %s", name, want, msg)
			}
		}
		if strings.Contains(msg, "\n") {
			t.Errorf("%s: more than one line", name)
		}
		for f, b := range before {
			if !bytes.Equal(b, read(t, filepath.Join(dir, f))) {
				t.Errorf("%s: %s was changed", name, f)
			}
		}
	}
}

func TestAFirstStartWithAPublicAddressIsRefusedBeforeAnyFileIsMade(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	_, err := Open(Options{Dir: dir, Names: Names{DNS: []string{"easbridge"}, IPs: []netip.Addr{netip.MustParseAddr("198.51.100.4")}}, Now: (&clock{t: t0}).Now})
	if err == nil || !strings.Contains(err.Error(), "private addresses only") || !strings.Contains(err.Error(), "198.51.100.4") {
		t.Fatalf("%v", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("files made: %v", ents)
	}
}

// A CA made by an early build has no constraints. It is not replaced (that would change what MailFlow trusts)
// but the log says so.
func TestACAWithoutConstraintsStillWorksAndIsReported(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	os.MkdirAll(dir, 0o700)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(5), Subject: pkix.Name{CommonName: "EASBridge local CA 00000000"}, NotBefore: t0.Add(-time.Hour), NotAfter: t0.AddDate(20, 0, 0),
		IsCA: true, BasicConstraintsValid: true, MaxPathLen: 0, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, _ := x509.CreateCertificate(crand.Reader, tmpl, tmpl, &key.PublicKey, key)
	kd, _ := x509.MarshalPKCS8PrivateKey(key)
	os.WriteFile(filepath.Join(dir, CAFile), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	os.WriteFile(filepath.Join(dir, CAKeyFile), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}), 0o600)
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	open(t, dir, Names{DNS: []string{"easbridge", "anything.example.com"}, IPs: []netip.Addr{netip.MustParseAddr("203.0.113.7")}}, &clock{t: t0}, log)
	if !strings.Contains(buf.String(), "without name constraints") {
		t.Errorf("no warning: %s", buf.String())
	}
}
