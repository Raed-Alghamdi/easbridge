package logx

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func capture(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	ResetSecretsForTest()
	t.Cleanup(ResetSecretsForTest)
	var buf bytes.Buffer
	return New(Options{Level: slog.LevelDebug, Writer: &buf}), &buf
}

func TestSecretsAreScrubbedEverywhere(t *testing.T) {
	log, buf := capture(t)
	RegisterCredential("alice@example.test", "hunter2-correct-horse")

	log.Info("connecting with hunter2-correct-horse now",
		"detail", "pw=hunter2-correct-horse",
		"err", errors.New("auth failed for hunter2-correct-horse"),
		slog.Group("g", slog.String("x", "prefix hunter2-correct-horse suffix")),
		"header", "Authorization: Basic YWxpY2VAZXhhbXBsZS50ZXN0Omh1bnRlcjItY29ycmVjdC1ob3JzZQ==",
	)
	out := buf.String()
	if strings.Contains(out, "hunter2-correct-horse") {
		t.Fatalf("password leaked: %s", out)
	}
	if strings.Contains(out, "YWxpY2VAZXhhbXBsZS50ZXN0Omh1bnRlcjItY29ycmVjdC1ob3JzZQ") {
		t.Fatalf("Basic blob leaked: %s", out)
	}
	if !strings.Contains(out, "***") {
		t.Fatalf("expected scrub marker in %s", out)
	}
}

func TestSensitiveKeysAreRedacted(t *testing.T) {
	log, buf := capture(t)
	log.Info("x", "subject", "Re: secret plans", "From", "bob@example.test", "body", "hello", "user", "alice@example.test", "folder", "5")
	out := buf.String()
	for _, bad := range []string{"secret plans", "bob@example.test", "hello", "alice@example.test"} {
		if strings.Contains(out, bad) {
			t.Errorf("%q leaked in %s", bad, out)
		}
	}
	if !strings.Contains(out, "folder=5") {
		t.Errorf("non-sensitive attribute lost: %s", out)
	}
}

func TestEASLoggerDropsUserQueryValue(t *testing.T) {
	var buf bytes.Buffer
	// A plain handler without ReplaceAttr: the wrapper itself must do the work.
	base := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	l := EAS(base).With("url", "https://h/x?User=alice%40example.test&Cmd=Sync")
	l.Debug("eas request", "cmd", "Sync", "url", "https://h/Microsoft-Server-ActiveSync?Cmd=Sync&DeviceId=ab&User=alice%40example.test&DeviceType=easbridge")
	out := buf.String()
	if strings.Contains(out, "alice") {
		t.Fatalf("mailbox name leaked: %s", out)
	}
	if !strings.Contains(out, "DeviceId=ab") || !strings.Contains(out, "Cmd=Sync") {
		t.Fatalf("other query parameters should survive: %s", out)
	}
}

func TestStripUser(t *testing.T) {
	if got := stripUser("https://h/p"); got != "https://h/p" {
		t.Error(got)
	}
	if got := stripUser("https://h/p?a=1"); got != "https://h/p?a=1" {
		t.Error(got)
	}
	if got := stripUser("https://h/p?User=x&a=1"); strings.Contains(got, "x&") || !strings.Contains(got, "a=1") {
		t.Error(got)
	}
}

func TestAdapter(t *testing.T) {
	log, buf := capture(t)
	RegisterSecret("topsecret")
	a := Adapter(log, "imap", slog.LevelWarn)
	a.Printf("failed to read command: %v\n", errors.New("x topsecret y"))
	a.Println("a", "b")
	out := buf.String()
	if strings.Contains(out, "topsecret") || !strings.Contains(out, "component=imap") || !strings.Contains(out, "a b") {
		t.Fatalf("adapter output wrong: %s", out)
	}
}

func TestShortSecretsIgnored(t *testing.T) {
	ResetSecretsForTest()
	RegisterSecret("ab")
	if Scrub("abab") != "abab" {
		t.Error("secrets shorter than 3 bytes must be ignored")
	}
}

func TestScrubRemovesMailboxNamesFromURLsInErrors(t *testing.T) {
	ResetSecretsForTest()
	err := `Post "https://mail.example.test/Microsoft-Server-ActiveSync?Cmd=Sync&DeviceId=ab12&User=alice%40example.test&DeviceType=easbridge": dial tcp: i/o timeout`
	got := Scrub(err)
	if strings.Contains(got, "alice") || !strings.Contains(got, "User=-&DeviceType=easbridge") || !strings.Contains(got, "DeviceId=ab12") {
		t.Fatalf("Scrub = %s", got)
	}
	if RedactURLs("no query here") != "no query here" {
		t.Error("plain text changed")
	}
	// It also applies to every logged message and attribute.
	log, buf := capture(t)
	log.Warn("failed: "+err, "err", errors.New(err))
	if strings.Contains(buf.String(), "alice") {
		t.Fatalf("logger leaked the mailbox name: %s", buf.String())
	}
}
