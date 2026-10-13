package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"easbridge/internal/version"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(map[string]string{
		EnvEASURL:       "https://mail.example.test/Microsoft-Server-ActiveSync",
		EnvAllowedUsers: " Alice@Example.test , other@example.test,alice@example.test",
		EnvIMAPAddr:     "10.0.0.20:993", EnvSMTPAddr: "10.0.0.20:465",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.AllowedUsers, ","); got != "alice@example.test,other@example.test" {
		t.Errorf("AllowedUsers = %q (want lower-cased, de-duplicated)", got)
	}
	d := Defaults()
	if d.IMAPAddr != "" || d.SMTPAddr != "" || d.AllowAnyBind {
		t.Errorf("the listen addresses must have no default: %q %q", d.IMAPAddr, d.SMTPAddr)
	}
	if c.EASVersion != "16.1" || c.DeviceType != "easbridge" || c.Filter != "all" ||
		c.MIMEInlineMax != 1<<20 || c.PingHeartbeat != 480*time.Second ||
		c.IMAPAddr != "10.0.0.20:993" || c.SMTPAddr != "10.0.0.20:465" || c.HealthAddr != "127.0.0.1:8081" || c.DataDir != "/data" ||
		c.TLSCert != "/tls/server.pem" || c.TLSKey != "/tls/server-key.pem" ||
		c.AuthFailCooldown != 15*time.Minute || c.EngineGrace != 30*time.Minute ||
		c.MaxMessageBytes != 35<<20 || c.LogLevel != slog.LevelInfo || c.Upstream != "eas" {
		t.Errorf("unexpected defaults: %+v", c)
	}
	if !strings.HasPrefix(c.UserAgent, "easbridge/") || c.UserAgent != d.UserAgent {
		t.Errorf("user agent = %q", c.UserAgent)
	}
	if c.FilterType() != 0 {
		t.Errorf("filter all must map to 0")
	}
	if !c.UserAllowed("ALICE@example.TEST") || c.UserAllowed("mallory@example.test") {
		t.Errorf("allow-list check is wrong")
	}
}

func TestLoadOverrides(t *testing.T) {
	c, err := Load(env(map[string]string{
		EnvEASURL:           "https://x.test/Microsoft-Server-ActiveSync",
		EnvAllowedUsers:     "a@b.test",
		EnvEASVersion:       "14.1",
		EnvFilter:           "2w",
		EnvMIMEInlineMax:    "512KiB",
		EnvPingHeartbeat:    "300",
		EnvAuthFailCooldown: "20m",
		EnvEngineGrace:      "1h",
		EnvMaxMessageBytes:  "10MB",
		EnvLogLevel:         "debug",
		EnvIMAPAddr:         "127.0.0.1:9993",
		EnvSMTPAddr:         "127.0.0.1:9465",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.FilterType() != 4 || c.MIMEInlineMax != 512<<10 || c.PingHeartbeat != 300*time.Second ||
		c.AuthFailCooldown != 20*time.Minute || c.EngineGrace != time.Hour ||
		c.MaxMessageBytes != 10<<20 || c.LogLevel != slog.LevelDebug || c.EASVersion != "14.1" ||
		c.IMAPAddr != "127.0.0.1:9993" {
		t.Errorf("overrides not applied: %+v", c)
	}
}

func TestLoadRejectsBadInput(t *testing.T) {
	base := map[string]string{
		EnvEASURL:       "https://x.test/Microsoft-Server-ActiveSync",
		EnvAllowedUsers: "a@b.test",
		EnvIMAPAddr:     "127.0.0.1:993", EnvSMTPAddr: "127.0.0.1:465",
	}
	with := func(k, v string) map[string]string {
		m := map[string]string{}
		for kk, vv := range base {
			m[kk] = vv
		}
		m[k] = v
		return m
	}
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"http url", with(EnvEASURL, "http://x.test/Microsoft-Server-ActiveSync"), "https"},
		{"no url", map[string]string{EnvAllowedUsers: "a@b.test", EnvIMAPAddr: "127.0.0.1:993", EnvSMTPAddr: "127.0.0.1:465"}, EnvEASURL},
		{"no users", map[string]string{EnvEASURL: base[EnvEASURL], EnvIMAPAddr: "127.0.0.1:993", EnvSMTPAddr: "127.0.0.1:465"}, EnvAllowedUsers},
		{"bad version", with(EnvEASVersion, "12.0"), EnvEASVersion},
		{"bad filter", with(EnvFilter, "5y"), EnvFilter},
		{"tiny inline", with(EnvMIMEInlineMax, "10"), EnvMIMEInlineMax},
		{"heartbeat too short", with(EnvPingHeartbeat, "5"), EnvPingHeartbeat},
		{"bad level", with(EnvLogLevel, "loud"), EnvLogLevel},
		{"bad addr", with(EnvIMAPAddr, "nonsense"), EnvIMAPAddr},
		{"unknown upstream", with(EnvUpstream, "pigeon"), EnvUpstream},
		{"bad log format", with(EnvLogFormat, "xml"), EnvLogFormat},
		{"public fake control", with(EnvFakeControl, "0.0.0.0:9"), "loopback"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(env(tc.env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want mention of %q", err, tc.want)
			}
		})
	}
}

func TestFakeUpstreamNeedsNoURL(t *testing.T) {
	if _, err := Load(env(map[string]string{EnvUpstream: "fake", EnvAllowedUsers: "a@b.test",
		EnvIMAPAddr: "127.0.0.1:0", EnvSMTPAddr: "127.0.0.1:0"})); err != nil {
		t.Fatal(err)
	}
}

// The security review found the bridge listening on ":993" and ":465", that is on
// every interface of the host, by default. An address given in a variable must name one IP
// address, and "everywhere" has to be asked for by name. (Since 0.3 a listener without a
// variable takes the container default, which listen_test.go covers; the host is guarded by
// CheckListenNetwork.)
func TestBindAddressesFailClosed(t *testing.T) {
	load := func(imap, smtp string, extra ...string) error {
		m := map[string]string{
			EnvEASURL:       "https://x.test/Microsoft-Server-ActiveSync",
			EnvAllowedUsers: "a@b.test",
		}
		if imap != "-" {
			m[EnvIMAPAddr] = imap
		}
		if smtp != "-" {
			m[EnvSMTPAddr] = smtp
		}
		for i := 0; i+1 < len(extra); i += 2 {
			m[extra[i]] = extra[i+1]
		}
		_, err := Load(env(m))
		return err
	}
	good := "10.0.0.20:993"
	for _, tc := range []struct {
		name       string
		imap, smtp string
		extra      []string
		want       string // "" accepted; else a substring of the error
	}{
		{"explicit addresses", good, "10.0.0.20:465", nil, ""},
		{"loopback and an ephemeral port", "127.0.0.1:0", "127.0.0.1:0", nil, ""},
		{"IPv6 loopback", "[::1]:993", "[::1]:465", nil, ""},
		{"IPv6 with a zone", "[fe80::1%eth0]:993", "[fe80::1%eth0]:465", nil, ""},

		{"no IMAP address: the default (v0.3)", "-", "10.0.0.20:465", nil, ""},
		{"no SMTP address: the default (v0.3)", good, "-", nil, ""},
		{"empty host", ":993", "10.0.0.20:465", nil, "every network interface"},
		{"0.0.0.0", "0.0.0.0:993", "10.0.0.20:465", nil, "every network interface"},
		{"IPv6 any", good, "[::]:465", nil, "every network interface"},
		{"IPv4-mapped any", "[::ffff:0.0.0.0]:993", "10.0.0.20:465", nil, "every network interface"},
		{"host name", "easbridge:993", "10.0.0.20:465", nil, "IP address"},
		{"localhost is a name too", "localhost:993", "10.0.0.20:465", nil, "IP address"},
		{"port out of range", "10.0.0.20:99999", "10.0.0.20:465", nil, "valid port"},
		{"port not a number", "10.0.0.20:imaps", "10.0.0.20:465", nil, "valid port"},
		{"no port", "10.0.0.20", "10.0.0.20:465", nil, "host:port"},
		{"health on every interface", good, "10.0.0.20:465", []string{EnvHealthAddr, ":8081"}, EnvHealthAddr},

		{"any bind allowed by name", ":993", "0.0.0.0:465", []string{EnvAllowAnyBind, "1"}, ""},
		{"any bind for the health endpoint too", good, "10.0.0.20:465", []string{EnvHealthAddr, "0.0.0.0:8081", EnvAllowAnyBind, "1"}, ""},
		{"allow flag 0 is off", ":993", "10.0.0.20:465", []string{EnvAllowAnyBind, "0"}, "every network interface"},
		{"allow flag true", ":993", "10.0.0.20:465", []string{EnvAllowAnyBind, "true"}, ""},
		{"allow flag must be true or 1", ":993", "10.0.0.20:465", []string{EnvAllowAnyBind, "yes"}, EnvAllowAnyBind},
		{"the flag does not excuse a host name", "easbridge:993", "10.0.0.20:465", []string{EnvAllowAnyBind, "1"}, "IP address"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := load(tc.imap, tc.smtp, tc.extra...)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("err = %v, want mention of %q", err, tc.want)
			}
		})
	}
}

func TestListensOnAnyInterface(t *testing.T) {
	mk := func(imap, smtp, health string) *Config {
		return &Config{IMAPAddr: imap, SMTPAddr: smtp, HealthAddr: health}
	}
	if mk("10.0.0.20:993", "10.0.0.20:465", "127.0.0.1:8081").ListensOnAnyInterface() {
		t.Error("explicit addresses listen on one interface each")
	}
	for _, c := range []*Config{
		mk(":993", "10.0.0.20:465", "127.0.0.1:8081"),
		mk("10.0.0.20:993", "0.0.0.0:465", "127.0.0.1:8081"),
		mk("10.0.0.20:993", "10.0.0.20:465", "[::]:8081"),
	} {
		if !c.ListensOnAnyInterface() {
			t.Errorf("%+v must be reported", c)
		}
	}
}

func TestParseSizeAndDuration(t *testing.T) {
	for in, want := range map[string]int64{"1": 1, "2k": 2048, "2KiB": 2048, "3M": 3 << 20, "35MiB": 35 << 20, "1g": 1 << 30} {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := ParseSize("-1"); err == nil {
		t.Error("negative size accepted")
	}
	if d, err := ParseDuration("90"); err != nil || d != 90*time.Second {
		t.Errorf("ParseDuration(90) = %v, %v", d, err)
	}
	if d, err := ParseDuration("2m"); err != nil || d != 2*time.Minute {
		t.Errorf("ParseDuration(2m) = %v, %v", d, err)
	}
}

// The device the bridge presents to Exchange (v0.1.5): honest defaults, each part
// configurable, the identity that was approved untouched.
func TestDeviceDefaults(t *testing.T) {
	c, err := Load(env(map[string]string{
		EnvEASURL: "https://mail.example.test/Microsoft-Server-ActiveSync", EnvAllowedUsers: "a@b.test",
		EnvIMAPAddr: "10.0.0.20:993", EnvSMTPAddr: "10.0.0.20:465",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := Device{
		Type: "easbridge", Model: "easbridge", FriendlyName: "easbridge (MailFlow bridge)", OS: "Linux", OSLanguage: "en-us",
		UserAgent: "easbridge/" + version.Version,
	}
	if got := c.Device(); got != want {
		t.Errorf("device = %+v, want %+v", got, want)
	}
	// What the organisation approved: DeviceType and User-Agent keep their form.
	if c.DeviceType != "easbridge" || c.UserAgent != version.UserAgent() || version.UserAgent() != "easbridge/"+version.Version {
		t.Errorf("identity changed: type %q, user agent %q", c.DeviceType, c.UserAgent)
	}
	// Never another product.
	for _, v := range []string{c.DeviceType, c.DeviceModel, c.DeviceFriendlyName, c.DeviceOS, c.UserAgent} {
		for _, other := range []string{"outlook", "iphone", "ipad", "android", "samsung", "microsoft", "apple"} {
			if strings.Contains(strings.ToLower(v), other) {
				t.Errorf("default %q names another product (%s)", v, other)
			}
		}
	}
}

func TestDeviceOverridesAndEmptyValues(t *testing.T) {
	base := map[string]string{
		EnvEASURL: "https://mail.example.test/Microsoft-Server-ActiveSync", EnvAllowedUsers: "a@b.test",
		EnvIMAPAddr: "10.0.0.20:993", EnvSMTPAddr: "10.0.0.20:465",
		EnvDeviceModel: "  mail-gateway ", EnvDeviceFriendly: "Office mail gateway", EnvDeviceOS: "Alpine Linux",
		EnvDeviceOSLanguage: "ar-sa", EnvDeviceType: "gateway", EnvUserAgent: "easbridge/pinned",
	}
	c, err := Load(env(base))
	if err != nil {
		t.Fatal(err)
	}
	want := Device{Type: "gateway", Model: "mail-gateway", FriendlyName: "Office mail gateway", OS: "Alpine Linux", OSLanguage: "ar-sa", UserAgent: "easbridge/pinned"}
	if got := c.Device(); got != want {
		t.Errorf("device = %+v, want %+v", got, want)
	}
	// An empty variable (a template that leaves the field blank) keeps the default.
	base[EnvDeviceModel], base[EnvDeviceOS] = "", "   "
	c, err = Load(env(base))
	if err != nil || c.DeviceModel != DefaultDeviceModel || c.DeviceOS != DefaultDeviceOS {
		t.Errorf("empty values: %+v %v", c.Device(), err)
	}
}

func TestDeviceSettingsAreChecked(t *testing.T) {
	base := map[string]string{
		EnvEASURL: "https://mail.example.test/Microsoft-Server-ActiveSync", EnvAllowedUsers: "a@b.test",
		EnvIMAPAddr: "10.0.0.20:993", EnvSMTPAddr: "10.0.0.20:465",
	}
	for _, tc := range []struct{ name, val, want string }{
		{EnvDeviceModel, strings.Repeat("m", 129), "at most 128"},
		{EnvDeviceFriendly, "two\nlines", "control characters"},
		{EnvDeviceOS, "nul\x00byte", "control characters"},
		{EnvDeviceOSLanguage, "tab\there", "control characters"},
		{EnvDeviceType, "bell\x07", "control characters"},
		{EnvUserAgent, "carriage\rreturn", "control characters"},
	} {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		m[tc.name] = tc.val
		_, err := Load(env(m))
		if err == nil || !strings.Contains(err.Error(), tc.name) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s=%q: err = %v, want a complaint about %s", tc.name, tc.val, err, tc.want)
		}
		if _, err := LoadDevice(env(m)); err == nil {
			t.Errorf("LoadDevice accepted %s=%q", tc.name, tc.val)
		}
	}
	// 128 bytes are fine.
	m := map[string]string{EnvDeviceModel: strings.Repeat("m", 128)}
	if d, err := LoadDevice(env(m)); err != nil || len(d.Model) != 128 {
		t.Errorf("128 bytes: %v", err)
	}
}

// The probe reads the device through LoadDevice; it must be the device Load gives.
func TestLoadDeviceAgreesWithLoad(t *testing.T) {
	for _, extra := range []map[string]string{
		nil,
		{EnvDeviceModel: "m"},
		{EnvDeviceFriendly: "f", EnvDeviceOS: "o", EnvDeviceOSLanguage: "l", EnvDeviceType: "t", EnvUserAgent: "u"},
		{EnvDeviceModel: "", EnvDeviceOS: " "},
	} {
		m := map[string]string{
			EnvEASURL: "https://mail.example.test/Microsoft-Server-ActiveSync", EnvAllowedUsers: "a@b.test",
			EnvIMAPAddr: "10.0.0.20:993", EnvSMTPAddr: "10.0.0.20:465",
		}
		for k, v := range extra {
			m[k] = v
		}
		c, err := Load(env(m))
		if err != nil {
			t.Fatal(err)
		}
		d, err := LoadDevice(env(m))
		if err != nil || d != c.Device() {
			t.Errorf("%v: LoadDevice = %+v (%v), Load = %+v", extra, d, err, c.Device())
		}
	}
}
