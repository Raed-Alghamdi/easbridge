package config

import (
	"strings"
	"testing"
	"time"
)

func contactsBase() map[string]string {
	return map[string]string{
		EnvEASURL:       "https://x.test/Microsoft-Server-ActiveSync",
		EnvAllowedUsers: "Owner@Example.com, other@mail.example.org",
		EnvIMAPAddr:     "192.0.2.20:993", EnvSMTPAddr: "192.0.2.20:465",
	}
}

func with(base map[string]string, kv ...string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

func TestContactsOffMeansNothingRuns(t *testing.T) {
	c, err := Load(env(with(contactsBase(), EnvContacts, "off")))
	if err != nil {
		t.Fatal(err)
	}
	if c.ContactsEnabled() || c.DirectoryEnabled() || c.CardDAVAddr != "" {
		t.Errorf("EASBRIDGE_CONTACTS=off must leave the feature off: %+v", c)
	}
	// The defaults are there, but nothing runs.
	if c.ContactsInterval != 30*time.Minute || c.DirectoryLookupsPerHour != 60 || c.DirectoryRefresh != 720*time.Hour ||
		c.ContactsBookName != "Exchange contacts" || c.DirectoryBookName != "Directory" {
		t.Errorf("defaults: %+v", c)
	}
}

func TestContactsDefaultsWhenEnabled(t *testing.T) {
	c, err := Load(env(with(contactsBase(), EnvCardDAVAddr, "192.0.2.20:8443")))
	if err != nil {
		t.Fatal(err)
	}
	if !c.ContactsEnabled() || !c.DirectoryEnabled() {
		t.Fatalf("not enabled: %+v", c)
	}
	if got := strings.Join(c.DirectoryDomains, ","); got != "example.com,mail.example.org" {
		t.Errorf("the default directory domains are those of the allowed users, got %q", got)
	}
	if c.SearchSpacing() != time.Minute {
		t.Errorf("spacing at 60 per hour = %v, want 1m", c.SearchSpacing())
	}
}

func TestDirectoryDomainsExplicitAndNone(t *testing.T) {
	c, err := Load(env(with(contactsBase(), EnvCardDAVAddr, "192.0.2.20:8443", EnvDirectoryDomains, " Corp.Example.com , example.com,EXAMPLE.com ")))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.DirectoryDomains, ","); got != "corp.example.com,example.com" {
		t.Errorf("domains = %q (lower-cased, de-duplicated)", got)
	}
	c, err = Load(env(with(contactsBase(), EnvCardDAVAddr, "192.0.2.20:8443", EnvDirectoryDomains, "NONE")))
	if err != nil {
		t.Fatal(err)
	}
	if !c.ContactsEnabled() || c.DirectoryEnabled() || len(c.DirectoryDomains) != 0 {
		t.Errorf("none turns the directory off and leaves contacts on: %+v", c)
	}
	// No allowed user has a domain to default to: the directory has nothing to look up.
	c, err = Load(env(map[string]string{EnvEASURL: "https://x.test/a", EnvAllowedUsers: "owner", EnvIMAPAddr: "127.0.0.1:1993",
		EnvSMTPAddr: "127.0.0.1:1465", EnvCardDAVAddr: "127.0.0.1:1443"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.DirectoryEnabled() {
		t.Errorf("a login name without a domain gives no default domain: %+v", c.DirectoryDomains)
	}
}

func TestContactsRejectBadInput(t *testing.T) {
	cases := []struct {
		name    string
		kv      []string
		wantErr string
	}{
		{"bind to every interface", []string{EnvCardDAVAddr, ":8443"}, EnvCardDAVAddr},
		{"host name instead of an IP", []string{EnvCardDAVAddr, "easbridge:8443"}, "IP address"},
		{"no port", []string{EnvCardDAVAddr, "192.0.2.20"}, "host:port"},
		{"same address as IMAP", []string{EnvCardDAVAddr, "192.0.2.20:993"}, "address of its own"},
		{"same address as SMTP", []string{EnvCardDAVAddr, "192.0.2.20:465"}, "address of its own"},
		{"same address as health", []string{EnvCardDAVAddr, "127.0.0.1:8081"}, "address of its own"},
		{"interval below the minimum", []string{EnvCardDAVAddr, "192.0.2.20:8443", EnvContactsInterval, "4m"}, EnvContactsInterval},
		{"interval nonsense", []string{EnvContactsInterval, "often"}, EnvContactsInterval},
		{"lookups zero", []string{EnvDirectoryLookups, "0"}, EnvDirectoryLookups},
		{"lookups above 180", []string{EnvDirectoryLookups, "181"}, EnvDirectoryLookups},
		{"lookups 600", []string{EnvDirectoryLookups, "600"}, "1 to 180"},
		{"lookups text", []string{EnvDirectoryLookups, "many"}, EnvDirectoryLookups},
		{"refresh below a day", []string{EnvDirectoryRefresh, "1h"}, EnvDirectoryRefresh},
		{"domain with at sign", []string{EnvDirectoryDomains, "@example.com"}, EnvDirectoryDomains},
		{"domain with a space", []string{EnvDirectoryDomains, "exa mple.com"}, "not a domain name"},
		{"domain with a hyphen edge", []string{EnvDirectoryDomains, "-example.com"}, "not a domain name"},
		{"none mixed with a domain", []string{EnvDirectoryDomains, "none,example.com"}, "cannot be combined"},
		{"only commas", []string{EnvDirectoryDomains, ", ,"}, "no domain"},
		{"control character in a book name", []string{EnvContactsBookName, "a\x01b"}, "control"},
		{"book name too long", []string{EnvDirectoryBook, strings.Repeat("x", 65)}, "at most 64"},
		{"book names equal", []string{EnvContactsBookName, "Same", EnvDirectoryBook, "same"}, "must differ"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(env(with(contactsBase(), tc.kv...)))
			if err == nil {
				t.Fatalf("accepted %v", tc.kv)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestContactsAcceptTheBoundaries(t *testing.T) {
	c, err := Load(env(with(contactsBase(), EnvCardDAVAddr, "192.0.2.20:8443", EnvContactsInterval, "5m",
		EnvDirectoryLookups, "180", EnvDirectoryRefresh, "24h", EnvContactsBookName, "Mine", EnvDirectoryBook, "Everyone")))
	if err != nil {
		t.Fatal(err)
	}
	if c.ContactsInterval != 5*time.Minute || c.DirectoryLookupsPerHour != 180 || c.DirectoryRefresh != 24*time.Hour ||
		c.ContactsBookName != "Mine" || c.DirectoryBookName != "Everyone" {
		t.Errorf("%+v", c)
	}
	if c.SearchSpacing() != 20*time.Second {
		t.Errorf("180 per hour is a search every 20 s, got %v", c.SearchSpacing())
	}
	c, err = Load(env(with(contactsBase(), EnvDirectoryLookups, "1")))
	if err != nil || c.SearchSpacing() != time.Hour {
		t.Errorf("1 per hour: %v %v", err, c.SearchSpacing())
	}
}

func TestSearchSpacingNeverGoesBelowTwentySeconds(t *testing.T) {
	for _, n := range []int{1, 20, 60, 120, 179, 180, 181, 600, 100000} {
		d := SpacingFor(n)
		if d < MinSearchSpacing {
			t.Errorf("SpacingFor(%d) = %v, below %v", n, d, MinSearchSpacing)
		}
	}
	if SpacingFor(0) != time.Hour {
		t.Errorf("a rate below 1 counts as 1")
	}
}

func TestCardDAVAddressMayBeAnyAddressWithAllowAnyBind(t *testing.T) {
	c, err := Load(env(with(contactsBase(), EnvCardDAVAddr, "0.0.0.0:8443", EnvAllowAnyBind, "1")))
	if err != nil {
		t.Fatal(err)
	}
	if !c.ListensOnAnyInterface() {
		t.Errorf("the CardDAV address must count for the warning about listening on every interface")
	}
}

func TestSameListener(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"1.2.3.4:443", "1.2.3.4:443", true},
		{"1.2.3.4:443", "1.2.3.5:443", false},
		{"1.2.3.4:443", "1.2.3.4:444", false},
		{"0.0.0.0:443", "1.2.3.4:443", true},
		{":443", "[::1]:443", true},
		{"127.0.0.1:0", "127.0.0.1:0", false},
		{"[::ffff:1.2.3.4]:443", "1.2.3.4:443", true},
	} {
		if got := sameListener(tc.a, tc.b); got != tc.want {
			t.Errorf("sameListener(%q, %q) = %v", tc.a, tc.b, got)
		}
	}
}

func TestFullDirectorySweepSetting(t *testing.T) {
	on := with(contactsBase(), EnvCardDAVAddr, "192.0.2.20:8443")
	c, err := Load(env(on))
	if err != nil || c.DirectoryFull || c.DirectoryFullInterval != 720*time.Hour {
		t.Fatalf("off by default, interval 720h: %+v %v", c, err)
	}
	for _, v := range []string{"on", "ON", " On "} {
		c, err = Load(env(with(on, EnvDirectoryFull, v)))
		if err != nil || !c.DirectoryFull {
			t.Errorf("%q: %v", v, err)
		}
	}
	c, err = Load(env(with(on, EnvDirectoryFull, "off")))
	if err != nil || c.DirectoryFull {
		t.Errorf("off: %v", err)
	}
	c, err = Load(env(with(on, EnvDirectoryFull, "on", EnvDirectoryFullInt, "168h")))
	if err != nil || c.DirectoryFullInterval != 168*time.Hour {
		t.Errorf("minimum interval: %v", err)
	}
	for name, tc := range map[string]struct {
		kv   []string
		want string
	}{
		"nonsense":                  {[]string{EnvDirectoryFull, "yes"}, EnvDirectoryFull},
		"on without contacts":       {[]string{EnvContacts, "off", EnvCardDAVAddr, "", EnvDirectoryFull, "on"}, EnvContacts},
		"on with the directory off": {[]string{EnvDirectoryFull, "on", EnvDirectoryDomains, "none"}, EnvDirectoryDomains},
		"interval below a week":     {[]string{EnvDirectoryFull, "on", EnvDirectoryFullInt, "167h"}, EnvDirectoryFullInt},
		"interval nonsense":         {[]string{EnvDirectoryFullInt, "weekly"}, EnvDirectoryFullInt},
	} {
		_, err := Load(env(with(on, tc.kv...)))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// No domain to default to: the directory is off, so the sweep is refused.
	_, err = Load(env(map[string]string{EnvEASURL: "https://x.test/a", EnvAllowedUsers: "owner", EnvIMAPAddr: "127.0.0.1:1993",
		EnvSMTPAddr: "127.0.0.1:1465", EnvCardDAVAddr: "127.0.0.1:1443", EnvDirectoryFull: "on"}))
	if err == nil {
		t.Errorf("the sweep without a directory domain was accepted")
	}
}
