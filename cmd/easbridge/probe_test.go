package main

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"easbridge/internal/config"
	"easbridge/internal/version"
)

func pipeWith(t *testing.T, content string) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w.WriteString(content)
	w.Close()
	t.Cleanup(func() { r.Close() })
	return r
}

func TestReadPasswordFromStdin(t *testing.T) {
	var stderr bytes.Buffer
	pw, err := readPassword(true, pipeWith(t, "s3cret pass phrase\r\nsecond line\n"), &stderr, "alice@example.test")
	if err != nil || pw != "s3cret pass phrase" {
		t.Fatalf("password = %q, err = %v", pw, err)
	}
	if stderr.Len() != 0 {
		t.Errorf("nothing may be echoed: %q", stderr.String())
	}
	if pw, err := readPassword(true, pipeWith(t, "no-newline"), &stderr, "u"); err != nil || pw != "no-newline" {
		t.Errorf("last line without newline: %q %v", pw, err)
	}
	if _, err := readPassword(true, pipeWith(t, ""), &stderr, "u"); err == nil {
		t.Error("empty stdin accepted")
	}
}

func TestReadPasswordRefusesANonTerminalWithoutTheFlag(t *testing.T) {
	var stderr bytes.Buffer
	_, err := readPassword(false, pipeWith(t, "pw\n"), &stderr, "u")
	if err == nil || !strings.Contains(err.Error(), "--password-stdin") {
		t.Fatalf("err = %v", err)
	}
}

func TestProbeNeedsURLAndUser(t *testing.T) {
	t.Setenv("EASBRIDGE_EAS_URL", "")
	t.Setenv("EASBRIDGE_ALLOWED_USERS", "")
	var out, errb bytes.Buffer
	if err := runProbe(nil, pipeWith(t, "pw\n"), &out, &errb); err == nil || !strings.Contains(err.Error(), "--url") {
		t.Fatalf("err = %v", err)
	}
}

func TestProbeRefusesPlainHTTP(t *testing.T) {
	var out, errb bytes.Buffer
	dir := t.TempDir()
	err := runProbe([]string{"--url", "http://mail.example.test/Microsoft-Server-ActiveSync", "--user", "a@example.test",
		"--password-stdin", "--data-dir", dir, "--ping-seconds", "0"}, pipeWith(t, "pw\n"), &out, &errb)
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("err = %v", err)
	}
}

// `easbridge probe` must present the device the service presents (v0.1.5): same
// variables, same defaults, same checks, and one function that builds the part of
// the Exchange configuration they share.
func TestTheProbeAndTheServiceDescribeTheSameDevice(t *testing.T) {
	envs := []map[string]string{
		nil,
		{"EASBRIDGE_DEVICE_MODEL": "mail-gateway", "EASBRIDGE_DEVICE_FRIENDLY_NAME": "Office gateway", "EASBRIDGE_DEVICE_OS": "Alpine",
			"EASBRIDGE_DEVICE_OS_LANGUAGE": "ar-sa", "EASBRIDGE_DEVICE_TYPE": "gateway", "EASBRIDGE_USER_AGENT": "easbridge/pinned"},
	}
	for _, extra := range envs {
		m := map[string]string{
			"EASBRIDGE_EAS_URL": "https://mail.example.test/Microsoft-Server-ActiveSync", "EASBRIDGE_ALLOWED_USERS": "alice@example.test",
			"EASBRIDGE_IMAP_ADDR": "10.0.0.20:993", "EASBRIDGE_SMTP_ADDR": "10.0.0.20:465",
		}
		for k, v := range extra {
			m[k] = v
		}
		getenv := func(k string) string { return m[k] }
		cfg, err := config.Load(getenv)
		if err != nil {
			t.Fatal(err)
		}
		o, err := parseProbe(nil, getenv, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		service := easConfig(cfg.EASURL, cfg.EASVersion, cfg.EASCAFile, cfg.Device())
		probe := easConfig(o.url, o.version, o.caFile, o.device)
		if service != probe {
			t.Errorf("the probe describes another device than the service:\n service %+v\n probe   %+v", service, probe)
		}
		if o.user != "alice@example.test" || o.pingSeconds != 60 {
			t.Errorf("probe options: %+v", o)
		}
	}
}

func TestProbeFlagsOverrideTheDevice(t *testing.T) {
	m := map[string]string{"EASBRIDGE_DEVICE_MODEL": "from-env", "EASBRIDGE_EAS_URL": "https://x.test/Microsoft-Server-ActiveSync", "EASBRIDGE_ALLOWED_USERS": "a@x.test"}
	getenv := func(k string) string { return m[k] }
	o, err := parseProbe([]string{"--device-model", "from-flag", "--device-friendly-name", "ff", "--device-os", "oo", "--device-os-language", "ll",
		"--device-type", "tt", "--user-agent", "uu", "--ping-seconds", "0"}, getenv, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := config.Device{Type: "tt", Model: "from-flag", FriendlyName: "ff", OS: "oo", OSLanguage: "ll", UserAgent: "uu"}
	if o.device != want || o.pingSeconds != 0 {
		t.Errorf("device %+v ping %d", o.device, o.pingSeconds)
	}
	// The same checks as for the service.
	if _, err := parseProbe([]string{"--device-model", "bad\nmodel"}, getenv, io.Discard); err == nil || !strings.Contains(err.Error(), "control characters") {
		t.Errorf("a control character in a flag: %v", err)
	}
	m["EASBRIDGE_DEVICE_OS"] = strings.Repeat("x", 200)
	if _, err := parseProbe(nil, getenv, io.Discard); err == nil || !strings.Contains(err.Error(), "EASBRIDGE_DEVICE_OS") {
		t.Errorf("an invalid variable: %v", err)
	}
}

// What the bridge says when nobody configures anything: the existing identity
// (DeviceType, User-Agent) and an honest description.
func TestProbeDefaultsAreTheServiceDefaults(t *testing.T) {
	m := map[string]string{"EASBRIDGE_EAS_URL": "https://x.test/Microsoft-Server-ActiveSync", "EASBRIDGE_ALLOWED_USERS": "a@x.test"}
	o, err := parseProbe(nil, func(k string) string { return m[k] }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := config.Device{Type: "easbridge", Model: "easbridge", FriendlyName: "easbridge (MailFlow bridge)", OS: "Linux", OSLanguage: "en-us",
		UserAgent: version.UserAgent()}
	if o.device != want {
		t.Errorf("device %+v, want %+v", o.device, want)
	}
	if o.version != "16.1" || o.dataDir != "/data" {
		t.Errorf("options %+v", o)
	}
}

// The service says once, at start-up, which device it presents to Exchange (nothing
// secret: the organisation approves devices and may want to compare).
func TestTheServiceLogsTheDeviceItPresents(t *testing.T) {
	m := map[string]string{
		"EASBRIDGE_EAS_URL": "https://mail.example.test/Microsoft-Server-ActiveSync", "EASBRIDGE_ALLOWED_USERS": "alice@example.test",
		"EASBRIDGE_IMAP_ADDR": "10.0.0.20:993", "EASBRIDGE_SMTP_ADDR": "10.0.0.20:465",
		"EASBRIDGE_DEVICE_MODEL": "mail-gateway",
	}
	cfg, err := config.Load(func(k string) string { return m[k] })
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	if _, _, err := newUpstream(cfg, log); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"device presented to Exchange", "device_type=easbridge", "model=mail-gateway", `friendly_name="easbridge (MailFlow bridge)"`,
		"os=Linux", "os_language=en-us", "user_agent=easbridge/" + version.Version} {
		if !strings.Contains(out, want) {
			t.Errorf("start-up log lacks %q:\n%s", want, out)
		}
	}
}

// --trace is off unless asked for (it prints what the server sends, redacted).
func TestProbeTraceIsOffByDefault(t *testing.T) {
	m := map[string]string{"EASBRIDGE_EAS_URL": "https://x.test/Microsoft-Server-ActiveSync", "EASBRIDGE_ALLOWED_USERS": "a@x.test"}
	getenv := func(k string) string { return m[k] }
	o, err := parseProbe(nil, getenv, io.Discard)
	if err != nil || o.trace {
		t.Fatalf("default: %+v %v", o, err)
	}
	o, err = parseProbe([]string{"--trace"}, getenv, io.Discard)
	if err != nil || !o.trace {
		t.Fatalf("--trace: %+v %v", o, err)
	}
	if !strings.Contains(usage, "--trace") {
		t.Errorf("the usage text does not mention --trace:\n%s", usage)
	}
}

// The probe gained a contacts step and a directory step in v0.2.0: on by default, off
// with --skip-contacts.
func TestProbeSkipContactsFlag(t *testing.T) {
	getenv := func(k string) string {
		return map[string]string{"EASBRIDGE_EAS_URL": "https://mail.example.test/Microsoft-Server-ActiveSync", "EASBRIDGE_ALLOWED_USERS": "alice@example.test"}[k]
	}
	o, err := parseProbe(nil, getenv, io.Discard)
	if err != nil || o.skipContacts {
		t.Fatalf("by default the contacts and directory steps run: %+v %v", o, err)
	}
	o, err = parseProbe([]string{"--skip-contacts"}, getenv, io.Discard)
	if err != nil || !o.skipContacts {
		t.Fatalf("--skip-contacts: %+v %v", o, err)
	}
	var usage bytes.Buffer
	parseProbe([]string{"-h"}, getenv, &usage)
	if !strings.Contains(usage.String(), "-skip-contacts") {
		t.Errorf("the flag is not in the usage text:\n%s", usage.String())
	}
}
