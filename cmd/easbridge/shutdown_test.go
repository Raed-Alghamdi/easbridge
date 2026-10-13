//go:build devfake

package main

import (
	"context"
	"log/slog"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"easbridge/internal/config"
	"easbridge/internal/logx"
	"easbridge/internal/testpki"
)

// A stop that arrives at any step of the start must end serve promptly with every listener closed.
// The hooks make it exact: the stop is the cancellation of the context at the named step, and every
// server's goroutine is held back until the servers have been told to close, which is the order that
// left go-smtp accepting on a listener nobody closed (the SMTP goroutine reached Serve after Close).
func TestAStopAtAnyStepOfTheStartEndsServeWithEveryListenerClosed(t *testing.T) {
	pki, err := testpki.New()
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []string{"tls", "imap listener", "smtp listener", "health listener", "carddav listener", "servers", "started"} {
		t.Run(step, func(t *testing.T) {
			dir := t.TempDir()
			_, certFile, keyFile, err := pki.WriteFiles(filepath.Join(dir, "tls"))
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("EASBRIDGE_FAKE_PASSWORD", "pw-shutdown-test-battery")
			env := map[string]string{
				config.EnvUpstream: "fake", config.EnvAllowedUsers: "alice@example.test", config.EnvDataDir: filepath.Join(dir, "data"),
				config.EnvTLSCert: certFile, config.EnvTLSKey: keyFile,
				config.EnvIMAPAddr: "127.0.0.1:0", config.EnvSMTPAddr: "127.0.0.1:0", config.EnvHealthAddr: "127.0.0.1:0", config.EnvCardDAVAddr: "127.0.0.1:0",
			}
			cfg, err := config.Load(func(k string) string { return env[k] })
			if err != nil {
				t.Fatal(err)
			}
			log := logx.New(logx.Options{Level: slog.LevelDebug, Writer: &lockedBuf{}})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var mu sync.Mutex
			var addrs []string
			closed := make(chan struct{})
			var once sync.Once
			hooks := &serveHooks{
				step: func(name, addr string) {
					mu.Lock()
					if addr != "" {
						addrs = append(addrs, addr)
					}
					mu.Unlock()
					if name == step {
						cancel()
					}
				},
				beforeServe: func(string) {
					select {
					case <-closed:
					case <-time.After(10 * time.Second):
					}
				},
				afterClose: func() { once.Do(func() { close(closed) }) },
			}
			done := make(chan error, 1)
			go func() { done <- serve(ctx, cfg, log, hooks) }()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("serve: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("serve did not return within 5 s of a stop at the step %q", step)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(addrs) == 0 && step != "tls" {
				t.Fatalf("no listener was recorded for the step %q", step)
			}
			for _, a := range addrs {
				if c, err := net.DialTimeout("tcp", a, time.Second); err == nil {
					c.Close()
					t.Errorf("%s still accepts connections after serve returned", a)
				}
			}
		})
	}
}
