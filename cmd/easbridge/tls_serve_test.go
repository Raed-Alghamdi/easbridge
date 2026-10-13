//go:build devfake

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"easbridge/internal/config"
	"easbridge/internal/logx"
	"easbridge/internal/testpki"
	"easbridge/internal/tlsauto"
)

const auto = "-" // startBridge: no TLS variables

func dial(t *testing.T, addr string, ca []byte, name string) *x509.Certificate {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		t.Fatal("no CA in the PEM")
	}
	conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: pool, ServerName: name})
	if err != nil {
		t.Fatalf("a client that trusts the CA could not connect as %q: %v", name, err)
	}
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0]
}

func sum(t *testing.T, p string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

// Without any TLS variable and without files at /tls the bridge makes its own CA and certificate, and
// MailFlow's side verifies it with the exported CA.
func TestAnInstallWithNoTLSFilesGetsAnAutomaticCertificate(t *testing.T) {
	dir := t.TempDir()
	export := filepath.Join(dir, "ca-out")
	b := startBridge(t, dir, map[string]string{config.EnvTLSCert: auto, config.EnvCAExportDir: export, config.EnvTLSNames: "mail.internal, 10.0.0.44"})
	tlsDir := filepath.Join(dir, "data", "tls")
	for f, want := range map[string]os.FileMode{"ca.pem": 0o644, "ca-key.pem": 0o600, "server.pem": 0o644, "server-key.pem": 0o600} {
		fi, err := os.Stat(filepath.Join(tlsDir, f))
		if err != nil || fi.Mode().Perm() != want || int(fi.Sys().(*syscall.Stat_t).Uid) != os.Getuid() {
			t.Errorf("%s: %v %v", f, fi, err)
		}
	}
	if fi, err := os.Stat(tlsDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the directory: %v %v", fi, err)
	}
	ca, err := os.ReadFile(filepath.Join(export, "ca.pem"))
	if err != nil {
		t.Fatalf("no export: %v", err)
	}
	if fi, _ := os.Stat(filepath.Join(export, "ca.pem")); fi.Mode().Perm() != 0o644 {
		t.Errorf("export mode %v", fi.Mode())
	}
	inData, _ := os.ReadFile(filepath.Join(tlsDir, "ca.pem"))
	if !bytes.Equal(ca, inData) {
		t.Error("the export is not the CA")
	}
	for _, name := range []string{"easbridge", "mail.internal"} {
		leaf := dial(t, b.imap, ca, name)
		if leaf.NotAfter.Sub(time.Now()) < 700*24*time.Hour {
			t.Errorf("the server certificate ends %v", leaf.NotAfter)
		}
	}
	if len(dial(t, b.imap, ca, "easbridge").IPAddresses) < 1 {
		t.Error("the IP names were not added")
	}
	// A client that trusts only another CA refuses it.
	other, _ := testpki.New()
	if _, err := tls.Dial("tcp", b.imap, other.ClientConfig("easbridge")); err == nil {
		t.Error("a client with another CA accepted the certificate")
	}
	h := b.healthz()
	days, _ := h["tls_server_expires_in_days"].(float64)
	if h["tls_mode"] != "automatic" || days < 700 || days > 731 {
		t.Errorf("health tls_mode=%v days=%v", h["tls_mode"], h["tls_server_expires_in_days"])
	}
	logs := b.logs.String()
	block, _ := pemFirst(ca)
	cert, _ := x509.ParseCertificate(block)
	for _, want := range []string{"TLS: automatic certificate", cert.Subject.CommonName, tlsauto.Fingerprint(cert)} {
		if !strings.Contains(logs, want) {
			t.Errorf("the start log lacks %q", want)
		}
	}
	for _, key := range []string{"ca-key.pem", "server-key.pem"} {
		kb, _ := os.ReadFile(filepath.Join(tlsDir, key))
		for _, line := range strings.Split(string(kb), "\n")[1:2] {
			if line != "" && strings.Contains(logs, line) {
				t.Errorf("key material of %s in the log", key)
			}
		}
	}
	if strings.Contains(logs, "PRIVATE KEY") {
		t.Error("PRIVATE KEY in the log")
	}
	b.stop()

	// A restart reuses everything and does not regenerate.
	before := map[string][32]byte{}
	for _, f := range []string{"ca.pem", "ca-key.pem", "server.pem", "server-key.pem"} {
		before[f] = sum(t, filepath.Join(tlsDir, f))
	}
	b2 := startBridge(t, dir, map[string]string{config.EnvTLSCert: auto, config.EnvCAExportDir: export, config.EnvTLSNames: "mail.internal, 10.0.0.44"})
	for f, h := range before {
		if sum(t, filepath.Join(tlsDir, f)) != h {
			t.Errorf("%s was rewritten by a restart", f)
		}
	}
	dial(t, b2.imap, ca, "easbridge")
	b2.stop()

	// A damaged export is repaired at start.
	os.WriteFile(filepath.Join(export, "ca.pem"), []byte("stale"), 0o644)
	b3 := startBridge(t, dir, map[string]string{config.EnvTLSCert: auto, config.EnvCAExportDir: export, config.EnvTLSNames: "mail.internal, 10.0.0.44"})
	if got, _ := os.ReadFile(filepath.Join(export, "ca.pem")); !bytes.Equal(got, ca) {
		t.Error("a stale export was not replaced")
	}
	b3.stop()

	// A new address: a new server certificate from the same CA, MailFlow's trust unchanged.
	serverBefore := sum(t, filepath.Join(tlsDir, "server.pem"))
	b4 := startBridge(t, dir, map[string]string{config.EnvTLSCert: auto, config.EnvCAExportDir: export, config.EnvTLSNames: "mail.internal, 10.0.0.44, 10.0.0.45"})
	if sum(t, filepath.Join(tlsDir, "server.pem")) == serverBefore {
		t.Error("no new server certificate for a new address")
	}
	if sum(t, filepath.Join(tlsDir, "ca.pem")) != before["ca.pem"] || sum(t, filepath.Join(tlsDir, "ca-key.pem")) != before["ca-key.pem"] {
		t.Error("the CA changed")
	}
	dial(t, b4.imap, ca, "10.0.0.45")
	b4.stop()

	// A new DNS name is outside what this CA was made for: the start is refused with one line that says
	// how to start over, and nothing in the directory changes.
	snapshot := map[string][32]byte{}
	for _, f := range []string{"ca.pem", "ca-key.pem", "server.pem", "server-key.pem"} {
		snapshot[f] = sum(t, filepath.Join(tlsDir, f))
	}
	err = serveToError(t, dir, map[string]string{config.EnvTLSNames: "mail.internal, 10.0.0.44, 10.0.0.45, other.internal"})
	if err == nil || !strings.Contains(err.Error(), "other.internal") || !strings.Contains(err.Error(), "may not certify") ||
		!strings.Contains(err.Error(), "remove the directory") || !strings.Contains(err.Error(), "restart MailFlow's backend") || strings.Contains(err.Error(), "\n") {
		t.Errorf("a name outside the constraints: %v", err)
	}
	for f, h := range snapshot {
		if sum(t, filepath.Join(tlsDir, f)) != h {
			t.Errorf("%s changed", f)
		}
	}
}

// serveToError runs serve with the automatic certificate in dir (the data directory of an earlier run) and
// returns the error it stops with.
func serveToError(t *testing.T, dir string, extra map[string]string) error {
	t.Helper()
	t.Setenv("EASBRIDGE_FAKE_PASSWORD", "pw-tls-test-battery")
	m := map[string]string{
		config.EnvUpstream: "fake", config.EnvAllowedUsers: "alice@example.test", config.EnvDataDir: filepath.Join(dir, "data"),
		config.EnvIMAPAddr: "127.0.0.1:0", config.EnvSMTPAddr: "127.0.0.1:0", config.EnvHealthAddr: "127.0.0.1:0", config.EnvContacts: "off",
	}
	for k, v := range extra {
		m[k] = v
	}
	cfg, err := config.Load(func(k string) string { return m[k] })
	if err != nil {
		t.Fatal(err)
	}
	log := logx.New(logx.Options{Level: slog.LevelDebug, Writer: &lockedBuf{}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return serve(ctx, cfg, log, nil)
}

func pemFirst(b []byte) ([]byte, []byte) {
	blk, rest := pem.Decode(b)
	if blk == nil {
		return nil, nil
	}
	return blk.Bytes, rest
}

// Provided files are used as 0.2.2 did: the mode is reported, nothing is written next to them.
func TestProvidedFilesAreUsedAsBeforeAndNeverTouched(t *testing.T) {
	dir := t.TempDir()
	export := filepath.Join(dir, "ca-out")
	// startBridge writes the PKI to <dir>/tls and sets the two variables: the configuration of an install with its own certificate files.
	b := startBridge(t, dir, map[string]string{config.EnvCAExportDir: export, config.EnvTLSNames: "ignored.internal"})
	cert, key := filepath.Join(dir, "tls", "server.pem"), filepath.Join(dir, "tls", "server-key.pem")
	before := [2][32]byte{sum(t, cert), sum(t, key)}
	h := b.healthz()
	if h["tls_mode"] != "provided" || h["tls_server_expires_in_days"].(float64) < 1 {
		t.Errorf("health %v %v", h["tls_mode"], h["tls_server_expires_in_days"])
	}
	if _, err := os.Stat(filepath.Join(dir, "data", "tls")); err == nil {
		t.Error("an automatic directory was made next to provided files")
	}
	if _, err := os.Stat(export); err == nil {
		t.Error("something was exported although the certificate is provided")
	}
	if !strings.Contains(b.logs.String(), "using the certificate and key you provided") || !strings.Contains(b.logs.String(), "no CA of EASBridge's to export") {
		t.Errorf("log:\n%s", b.logs.String())
	}
	if leaf := dial(t, b.imap, b.pki.CAPEM, "easbridge"); leaf.Subject.CommonName != "easbridge" {
		t.Errorf("leaf %v", leaf.Subject)
	}
	b.stop()
	if [2][32]byte{sum(t, cert), sum(t, key)} != before {
		t.Error("provided files were changed")
	}
}

func TestPartialOrBrokenProvidedFilesStopTheStartWithOneLineAndChangeNothing(t *testing.T) {
	pki, _ := testpki.New()
	other, _ := testpki.New()
	mk := func(t *testing.T) (dir, cert, key string) {
		dir = t.TempDir()
		if _, _, _, err := pki.WriteFiles(filepath.Join(dir, "tls")); err != nil {
			t.Fatal(err)
		}
		return dir, filepath.Join(dir, "tls", "server.pem"), filepath.Join(dir, "tls", "server-key.pem")
	}
	run := func(t *testing.T, dir string, env map[string]string) error {
		t.Helper()
		t.Setenv("EASBRIDGE_FAKE_PASSWORD", "pw-tls-test-battery")
		m := map[string]string{
			config.EnvUpstream: "fake", config.EnvAllowedUsers: "alice@example.test", config.EnvDataDir: filepath.Join(dir, "data"),
			config.EnvIMAPAddr: "127.0.0.1:0", config.EnvSMTPAddr: "127.0.0.1:0", config.EnvHealthAddr: "127.0.0.1:0", config.EnvContacts: "off",
		}
		for k, v := range env {
			m[k] = v
		}
		cfg, err := config.Load(func(k string) string { return m[k] })
		if err != nil {
			t.Fatal(err)
		}
		log := logx.New(logx.Options{Level: slog.LevelDebug, Writer: &lockedBuf{}})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return serve(ctx, cfg, log, nil)
	}
	for name, tc := range map[string]struct {
		env  func(cert, key string, dir string) map[string]string
		want string
	}{
		"only the certificate": {func(c, k, d string) map[string]string { return map[string]string{config.EnvTLSCert: c} }, "only one of EASBRIDGE_TLS_CERT and EASBRIDGE_TLS_KEY"},
		"only the key":         {func(c, k, d string) map[string]string { return map[string]string{config.EnvTLSKey: k} }, "only one of"},
		"files that do not exist": {func(c, k, d string) map[string]string {
			return map[string]string{config.EnvTLSCert: filepath.Join(d, "none.pem"), config.EnvTLSKey: filepath.Join(d, "none-key.pem")}
		}, "cannot be used"},
		"a certificate and a key of two sets": {func(c, k, d string) map[string]string {
			ok, kk := filepath.Join(d, "o.pem"), filepath.Join(d, "o-key.pem")
			os.WriteFile(ok, other.CertPEM, 0o644)
			os.WriteFile(kk, other.KeyPEM, 0o600)
			return map[string]string{config.EnvTLSCert: c, config.EnvTLSKey: kk}
		}, "does not match"},
		"a garbled certificate": {func(c, k, d string) map[string]string {
			os.WriteFile(c, []byte("junk"), 0o644)
			return map[string]string{config.EnvTLSCert: c, config.EnvTLSKey: k}
		}, "cannot be used"},
	} {
		t.Run(name, func(t *testing.T) {
			dir, cert, key := mk(t)
			env := tc.env(cert, key, dir)
			sb := [2][32]byte{sum(t, cert), sum(t, key)}
			err := run(t, dir, env)
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(strings.TrimSpace(err.Error()), "\n") {
				t.Fatalf("want one line with %q, got %v", tc.want, err)
			}
			if [2][32]byte{sum(t, cert), sum(t, key)} != sb {
				t.Error("a provided file was changed")
			}
			if _, err := os.Stat(filepath.Join(dir, "data", "tls")); err == nil {
				t.Error("an automatic set was made next to the files that were refused")
			}
		})
	}
}

func TestTheCACommand(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.EnvDataDir, filepath.Join(dir, "data"))
	t.Setenv(config.EnvTLSCert, "")
	t.Setenv(config.EnvTLSKey, "")
	var out bytes.Buffer
	if err := runCA(nil, &out); err == nil || !strings.Contains(err.Error(), "start EASBridge once") {
		t.Errorf("before the first start: %v", err)
	}
	b := startBridge(t, dir, map[string]string{config.EnvTLSCert: auto})
	b.stop()
	out.Reset()
	if err := runCA(nil, &out); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(filepath.Join(dir, "data", "tls", "ca.pem"))
	if !bytes.Equal(out.Bytes(), want) || !strings.HasPrefix(out.String(), "-----BEGIN CERTIFICATE-----") || strings.Contains(out.String(), "PRIVATE") {
		t.Errorf("output %q", out.String())
	}
	if err := runCA([]string{"x"}, &out); err == nil {
		t.Error("arguments accepted")
	}
	// Provided: not managed.
	t.Setenv(config.EnvTLSCert, filepath.Join(dir, "tls", "server.pem"))
	t.Setenv(config.EnvTLSKey, filepath.Join(dir, "tls", "server-key.pem"))
	out.Reset()
	err := runCA(nil, &out)
	if err == nil || !strings.Contains(err.Error(), "not managed by EASBridge") || out.Len() != 0 {
		t.Errorf("provided: %v %q", err, out.String())
	}
}

// While the bridge runs, the server certificate is renewed 60 days before it ends, from the same CA, and the
// listeners serve the new one at once.
func TestTheRunningBridgeServesTheRenewedCertificate(t *testing.T) {
	var mu sync.Mutex
	offset := time.Duration(0)
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return time.Now().Add(offset) }
	testHooks = func(h *serveHooks) { h.tlsNow, h.tlsEvery = now, 10*time.Millisecond }
	t.Cleanup(func() { testHooks = nil })
	dir := t.TempDir()
	b := startBridge(t, dir, map[string]string{config.EnvTLSCert: auto})
	ca, _ := os.ReadFile(filepath.Join(dir, "data", "tls", "ca.pem"))
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	serial := func() string {
		conn, err := tls.Dial("tcp", b.imap, &tls.Config{RootCAs: pool, ServerName: "easbridge", Time: now})
		if err != nil {
			t.Fatalf("%v", err)
		}
		defer conn.Close()
		return conn.ConnectionState().PeerCertificates[0].SerialNumber.String()
	}
	first := serial()
	mu.Lock()
	offset = 2*365*24*time.Hour - 59*24*time.Hour
	mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		if got = serial(); got != first {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got == first {
		t.Fatalf("the bridge never served a renewed certificate:\n%s", b.logs.String())
	}
	if sum(t, filepath.Join(dir, "data", "tls", "ca.pem")) != sha256.Sum256(ca) {
		t.Error("the CA changed")
	}
	if !strings.Contains(b.logs.String(), "TLS server certificate renewed from the same CA") {
		t.Error("no log line about the renewal")
	}
}

// docker exec easbridge /easbridge healthcheck -v prints the health report; without -v it prints nothing.
func TestHealthcheckVerbosePrintsTheReport(t *testing.T) {
	b := startBridge(t, t.TempDir(), map[string]string{config.EnvTLSCert: auto})
	t.Setenv(config.EnvHealthAddr, b.health)
	var out bytes.Buffer
	if err := runHealthcheckTo([]string{"-v"}, &out); err != nil {
		t.Fatal(err)
	}
	var h map[string]any
	if err := json.Unmarshal(out.Bytes(), &h); err != nil || h["status"] != "ok" || h["tls_mode"] != "automatic" {
		t.Errorf("report %q: %v", out.String(), err)
	}
	if !strings.Contains(out.String(), "\n  \"") {
		t.Errorf("not indented: %q", out.String())
	}
	out.Reset()
	if err := runHealthcheckTo(nil, &out); err != nil || out.Len() != 0 {
		t.Errorf("plain healthcheck: %v %q", err, out.String())
	}
	if err := runHealthcheckTo([]string{"x"}, &out); err == nil {
		t.Error("an unknown argument was accepted")
	}
	b.stop()
	if err := runHealthcheckTo([]string{"-v"}, &out); err == nil {
		t.Error("a stopped bridge reported healthy")
	}
}
