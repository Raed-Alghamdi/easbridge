//go:build devfake

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"easbridge/internal/config"
	"easbridge/internal/logx"
	"easbridge/internal/testpki"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func TestServeWithTheFakeUpstreamEndToEnd(t *testing.T) {
	pki, err := testpki.New()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	_, certFile, keyFile, err := pki.WriteFiles(filepath.Join(dir, "tls"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("EASBRIDGE_FAKE_PASSWORD", "pw-serve-test-battery")
	env := map[string]string{
		config.EnvUpstream: "fake", config.EnvAllowedUsers: "alice@example.test",
		config.EnvIMAPAddr: "127.0.0.1:0", config.EnvSMTPAddr: "127.0.0.1:0", config.EnvHealthAddr: "127.0.0.1:0",
		config.EnvFakeControl: "127.0.0.1:0", config.EnvTLSCert: certFile, config.EnvTLSKey: keyFile,
		config.EnvDataDir: filepath.Join(dir, "data"), config.EnvLogLevel: "debug", config.EnvContacts: "off",
	}
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuf{}
	log := logx.New(logx.Options{Level: slog.LevelDebug, Writer: logs})
	ctx, cancel := context.WithCancel(context.Background())
	type addrs struct{ imap, smtp, health string }
	ready := make(chan addrs, 1)
	done := make(chan error, 1)
	stopped := make(chan struct{})
	go func() {
		done <- serve(ctx, cfg, log, &serveHooks{onListening: func(i, s, h string) { ready <- addrs{i, s, h} }})
		close(stopped)
	}()
	var a addrs
	select {
	case a = <-ready:
	case err := <-done:
		t.Fatalf("serve returned early: %v\n%s", err, logs.String())
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not start")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
		}
	})

	// Control API address, from the startup log.
	m := regexp.MustCompile(`control API listening" addr=(\S+)`).FindStringSubmatch(logs.String())
	if m == nil {
		t.Fatalf("no control address in the log:\n%s", logs.String())
	}
	control := "http://" + m[1]

	// Health: 200 and no mailbox content.
	resp, err := http.Get("http://" + a.health + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	var h map[string]any
	json.NewDecoder(resp.Body).Decode(&h)
	resp.Body.Close()
	if resp.StatusCode != 200 || h["status"] != "ok" || h["imap_listening"] != true || h["smtp_listening"] != true {
		t.Fatalf("health = %d %v", resp.StatusCode, h)
	}
	t.Setenv(config.EnvHealthAddr, a.health)
	if err := runHealthcheck(nil); err != nil {
		t.Errorf("healthcheck: %v", err)
	}

	// IMAP over implicit TLS.
	var gotExists uint32
	var mu sync.Mutex
	c, err := imapclient.DialTLS(a.imap, &imapclient.Options{TLSConfig: pki.ClientConfig("easbridge"),
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{Mailbox: func(d *imapclient.UnilateralDataMailbox) {
			if d.NumMessages != nil {
				mu.Lock()
				gotExists = *d.NumMessages
				mu.Unlock()
			}
		}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Login("alice@example.test", "pw-serve-test-battery").Wait(); err != nil {
		t.Fatal(err)
	}
	var sel *imap.SelectData
	for i := 0; i < 100; i++ {
		if sel, err = c.Select("INBOX", nil).Wait(); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("SELECT INBOX: %v", err)
	}
	if sel.NumMessages != 0 {
		t.Fatalf("EXISTS = %d", sel.NumMessages)
	}
	mime := "Message-ID: <inject@x>\r\nFrom: b@example.test\r\nTo: alice@example.test\r\nSubject: injected\r\nDate: Fri, 09 Oct 2026 12:00:00 +0000\r\n\r\nhi\r\n"
	r, err := http.Post(control+"/inject?user=alice@example.test&folder=Inbox", "message/rfc822", strings.NewReader(mime))
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("inject: %v %v", r, err)
	}
	r.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.Noop().Wait()
		mu.Lock()
		n := gotExists
		mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no EXISTS after injecting mail\n%s", logs.String())
		}
		time.Sleep(30 * time.Millisecond)
	}

	// Health after the first sync: first-sync progress as counts only, no folder
	// names, every folder open and nothing hidden.
	var acct map[string]any
	for i := 0; i < 100 && acct == nil; i++ {
		resp, err := http.Get("http://" + a.health + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		var hh struct {
			Accounts []map[string]any `json:"accounts"`
		}
		json.NewDecoder(resp.Body).Decode(&hh)
		resp.Body.Close()
		if len(hh.Accounts) == 1 && hh.Accounts[0]["initial_sync_pending"] == float64(0) && hh.Accounts[0]["syncing"] == float64(0) {
			acct = hh.Accounts[0]
		} else {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if acct == nil {
		t.Fatal("the first sync never finished according to /healthz")
	}
	if acct["initial_sync_complete"] != acct["folders"] || acct["staged_items"] != float64(0) {
		t.Errorf("first-sync counts: %v", acct)
	}
	// The circuit breaker is part of the health picture: closed, never tripped.
	if acct["paused"] != false || acct["pause_trips"] != float64(0) || acct["paused_until_unix"] != nil || acct["pause_reason"] != nil {
		t.Errorf("circuit breaker fields: %v", acct)
	}
	prog, _ := acct["folder_progress"].([]any)
	if len(prog) == 0 || float64(len(prog)) != acct["folders"] {
		t.Fatalf("folder_progress = %v", acct["folder_progress"])
	}
	for _, p := range prog {
		fp := p.(map[string]any)
		if fp["initial_sync_complete"] != true || fp["staged"] != float64(0) || fp["pending_fetch"] != float64(0) || len(fp) != 3 {
			t.Errorf("folder progress entry: %v (counts only, no names)", fp)
		}
	}

	// SMTP submission with a Bcc-only recipient, then ask the control API what Exchange got.
	sc, err := smtp.DialTLS(a.smtp, pki.ClientConfig("easbridge"))
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	if err := sc.Auth(sasl.NewPlainClient("", "alice@example.test", "pw-serve-test-battery")); err != nil {
		t.Fatal(err)
	}
	if err := sc.SendMail("alice@example.test", []string{"bob@example.test", "hidden@example.test"},
		strings.NewReader("Message-ID: <out@x>\r\nFrom: alice@example.test\r\nTo: bob@example.test\r\nSubject: out\r\n\r\nbody\r\n")); err != nil {
		t.Fatal(err)
	}
	r, err = http.Get(control + "/sent?user=alice@example.test")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if !strings.Contains(string(body), "hidden@example.test") {
		t.Fatalf("Exchange did not receive the Bcc recipient: %s", body)
	}

	// Password and mail never reach the log.
	if strings.Contains(logs.String(), "pw-serve-test-battery") || strings.Contains(logs.String(), "injected") {
		t.Errorf("log leaks:\n%s", logs.String())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}
	if _, err := os.Stat(filepath.Join(dir, "data", "state.db")); err != nil {
		t.Errorf("state database missing: %v", err)
	}
	_ = fmt.Sprint
}

func TestHealthcheckFailsWhenNothingListens(t *testing.T) {
	t.Setenv(config.EnvHealthAddr, "127.0.0.1:1")
	if err := runHealthcheck(nil); err == nil {
		t.Fatal("healthcheck succeeded without a bridge")
	}
}
