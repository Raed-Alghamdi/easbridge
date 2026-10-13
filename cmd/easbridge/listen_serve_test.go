//go:build devfake

package main

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"easbridge/internal/config"
	"easbridge/internal/engine"
	"easbridge/internal/fakeupstream"
	"easbridge/internal/logx"
	"easbridge/internal/store"
	"easbridge/internal/testpki"
)

func listenEnv(t *testing.T, kv ...string) (*config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("EASBRIDGE_FAKE_PASSWORD", "pw-listen-test-battery")
	env := map[string]string{
		config.EnvUpstream: "fake", config.EnvAllowedUsers: "alice@example.test", config.EnvDataDir: filepath.Join(dir, "data"),
		config.EnvTLSCert: filepath.Join(dir, "none.pem"), config.EnvTLSKey: filepath.Join(dir, "none-key.pem"),
	}
	for i := 0; i+1 < len(kv); i += 2 {
		env[kv[i]] = kv[i+1]
	}
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	return cfg, dir
}

func lister(spec map[string]string) config.InterfaceLister {
	return func() ([]config.Iface, error) {
		var out []config.Iface
		for name, a := range spec {
			f := config.Iface{Name: name, Loopback: name == "lo"}
			if a != "" {
				f.Addrs = []netip.Addr{netip.MustParseAddr(a)}
			}
			out = append(out, f)
		}
		return out, nil
	}
}

func TestServeRefusesToListenEverywhereOnAHostAndBindsNothing(t *testing.T) {
	cfg, dir := listenEnv(t)
	logs := &lockedBuf{}
	log := logx.New(logx.Options{Level: slog.LevelDebug, Writer: logs})
	err := serve(context.Background(), cfg, log, &serveHooks{interfaces: lister(map[string]string{"lo": "127.0.0.1", "wlan0": "192.0.2.9", "docker0": "172.17.0.1"})})
	if err == nil {
		t.Fatal("serve started on a host with the wildcard default")
	}
	for _, want := range []string{"does not look like a container", config.EnvListenIP, config.EnvAllowAnyBind, "wlan0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error lacks %q: %v", want, err)
		}
	}
	if _, statErr := os.Stat(filepath.Join(dir, "data", store.DBFileName)); statErr == nil {
		t.Error("the state database was opened before the check")
	}
}

func TestServeRefusesAListenIPThatIsNotAnAddressOfTheContainer(t *testing.T) {
	cfg, _ := listenEnv(t, config.EnvListenIP, "10.9.9.9")
	log := logx.New(logx.Options{Level: slog.LevelDebug, Writer: &lockedBuf{}})
	err := serve(context.Background(), cfg, log, &serveHooks{interfaces: lister(map[string]string{"lo": "127.0.0.1", "eth0": "10.0.0.20"})})
	if err == nil || !strings.Contains(err.Error(), "EASBRIDGE_LISTEN_IP=10.9.9.9 is not an address of this container") || !strings.Contains(err.Error(), "eth0 10.0.0.20") {
		t.Errorf("%v", err)
	}
}

// A person of an allowed domain is let in (and checked by Exchange), another domain never reaches it.
func TestADomainEntryLetsInTheLoginsOfThatDomainOnly(t *testing.T) {
	cfg, _ := listenEnv(t, config.EnvAllowedUsers, "@example.test")
	srv := fakeupstream.NewServer()
	srv.AddUser("bob@example.test", "pw-bob-correct-horse")
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	log := logx.New(logx.Options{Level: slog.LevelDebug, Writer: &lockedBuf{}})
	ec := engine.DefaultConfig()
	mgr := engine.NewManager(ec, log, db, srv.Factory(4096), cfg.UserAllowed)
	defer func() { mgr.Close(); mgr.DB().Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := mgr.Login(ctx, "Bob@Example.TEST", "pw-bob-correct-horse"); err != nil {
		t.Fatalf("a login of the allowed domain: %v", err)
	}
	before := srv.AuthAttempts()
	for _, u := range []string{"bob@sub.example.test", "mallory@other.test", "bob@example.test.evil.test"} {
		_, err := mgr.Login(ctx, u, "pw-anything-else-entirely")
		if !errors.Is(err, engine.ErrBadCredentials) {
			t.Errorf("%s: %v", u, err)
		}
	}
	if srv.AuthAttempts() != before {
		t.Errorf("a login outside the domain reached Exchange (%d attempts)", srv.AuthAttempts()-before)
	}
}

// The configuration that 0.2.2 ran with (every listener has its own address) starts as before, even in a
// namespace that looks like a host, and the log says where it listens.
func TestExplicitListenerAddressesStartAsBeforeOnAHost(t *testing.T) {
	pki, err := testpki.New()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	_, certFile, keyFile, err := pki.WriteFiles(filepath.Join(dir, "tls"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := listenEnv(t, config.EnvTLSCert, certFile, config.EnvTLSKey, keyFile, config.EnvDataDir, filepath.Join(dir, "data"),
		config.EnvIMAPAddr, "127.0.0.1:0", config.EnvSMTPAddr, "127.0.0.1:0", config.EnvCardDAVAddr, "127.0.0.1:0", config.EnvHealthAddr, "127.0.0.1:0")
	logs := &lockedBuf{}
	log := logx.New(logx.Options{Level: slog.LevelDebug, Writer: logs})
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, cfg, log, &serveHooks{
			interfaces:  lister(map[string]string{"lo": "127.0.0.1", "wlan0": "192.0.2.9"}),
			onListening: func(_, _, _ string) { ready <- struct{}{} },
		})
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("serve returned: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not start")
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("serve: %v", err)
	}
	if !strings.Contains(logs.String(), "listening on the addresses given in EASBRIDGE_IMAP_ADDR") {
		t.Errorf("the start log does not name the mode:\n%s", logs.String())
	}
}
