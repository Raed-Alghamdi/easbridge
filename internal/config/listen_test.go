package config

import (
	"net/netip"
	"strings"
	"testing"
)

func minimal() map[string]string {
	return map[string]string{
		EnvEASURL:       "https://mail.example.test/Microsoft-Server-ActiveSync",
		EnvAllowedUsers: "alice@example.test",
	}
}

func mustLoad(t *testing.T, m map[string]string) *Config {
	t.Helper()
	c, err := Load(env(m))
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	return c
}

func ifaces(spec ...string) InterfaceLister {
	// "eth0=10.0.0.20" or "lo=127.0.0.1,::1" or "wlan0"
	return func() ([]Iface, error) {
		var out []Iface
		for _, s := range spec {
			name, addrs, _ := strings.Cut(s, "=")
			f := Iface{Name: name, Loopback: name == "lo"}
			for _, a := range strings.Split(addrs, ",") {
				if a != "" {
					f.Addrs = append(f.Addrs, netip.MustParseAddr(a))
				}
			}
			out = append(out, f)
		}
		return out, nil
	}
}

func TestTheInstallDefaultsAreTwoValues(t *testing.T) {
	c := mustLoad(t, minimal())
	if c.IMAPAddr != ":993" || c.SMTPAddr != ":465" || c.CardDAVAddr != ":8443" || c.HealthAddr != "127.0.0.1:8081" {
		t.Errorf("addresses %q %q %q health %q", c.IMAPAddr, c.SMTPAddr, c.CardDAVAddr, c.HealthAddr)
	}
	if !c.Contacts || !c.ContactsEnabled() || !c.DirectoryEnabled() || strings.Join(c.DirectoryDomains, ",") != "example.test" {
		t.Errorf("contacts and directory should be on by default: %+v", c)
	}
	if !c.WildcardListeners() || c.ListensOnAnyInterface() {
		t.Errorf("the wildcard is the default, not an explicit any-bind")
	}
	if !strings.Contains(c.ListenMode(), "listening on all container addresses (set EASBRIDGE_LISTEN_IP to restrict)") {
		t.Errorf("mode: %s", c.ListenMode())
	}
}

func TestListenIPBindsAllThreeListeners(t *testing.T) {
	for ip, want := range map[string][3]string{
		"10.0.0.20":       {"10.0.0.20:993", "10.0.0.20:465", "10.0.0.20:8443"},
		"192.0.2.7":       {"192.0.2.7:993", "192.0.2.7:465", "192.0.2.7:8443"},
		"2001:db8::5":     {"[2001:db8::5]:993", "[2001:db8::5]:465", "[2001:db8::5]:8443"},
		"::ffff:10.1.1.1": {"10.1.1.1:993", "10.1.1.1:465", "10.1.1.1:8443"},
	} {
		c := mustLoad(t, with(minimal(), EnvListenIP, ip))
		if c.IMAPAddr != want[0] || c.SMTPAddr != want[1] || c.CardDAVAddr != want[2] || c.HealthAddr != "127.0.0.1:8081" {
			t.Errorf("%s: %q %q %q health %q", ip, c.IMAPAddr, c.SMTPAddr, c.CardDAVAddr, c.HealthAddr)
		}
		if c.WildcardListeners() || !strings.Contains(c.ListenMode(), "only") {
			t.Errorf("%s: %v %s", ip, c.WildcardListeners(), c.ListenMode())
		}
	}
	c := mustLoad(t, with(minimal(), EnvListenIP, "10.0.0.5", EnvContacts, "off"))
	if c.CardDAVAddr != "" || c.IMAPAddr != "10.0.0.5:993" {
		t.Errorf("contacts off: %q %q", c.CardDAVAddr, c.IMAPAddr)
	}
}

func TestListenIPMustBeAnIPLiteral(t *testing.T) {
	for _, bad := range []string{"mailflow", "localhost", "10.0.0.5:993", "[::1]:993", "10.0.0.256", "0.0.0.0", "::", "fe80::1%eth0", "10.0.0.5/24", "10.0.0.5,10.0.0.6"} {
		_, err := Load(env(with(minimal(), EnvListenIP, bad)))
		if err == nil || !strings.Contains(err.Error(), EnvListenIP) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

// An address in a variable wins for its listener, with the validation of 0.2.2.
func TestTheListenerVariablesWinOverListenIP(t *testing.T) {
	c := mustLoad(t, with(minimal(), EnvListenIP, "10.0.0.5", EnvIMAPAddr, "10.0.0.20:1993"))
	if c.IMAPAddr != "10.0.0.20:1993" || c.SMTPAddr != "10.0.0.5:465" || c.CardDAVAddr != "10.0.0.5:8443" {
		t.Errorf("IMAP wins: %q %q %q", c.IMAPAddr, c.SMTPAddr, c.CardDAVAddr)
	}
	c = mustLoad(t, with(minimal(), EnvListenIP, "10.0.0.5", EnvSMTPAddr, "127.0.0.1:1465", EnvCardDAVAddr, "127.0.0.1:1443"))
	if c.IMAPAddr != "10.0.0.5:993" || c.SMTPAddr != "127.0.0.1:1465" || c.CardDAVAddr != "127.0.0.1:1443" {
		t.Errorf("SMTP and CardDAV win: %q %q %q", c.IMAPAddr, c.SMTPAddr, c.CardDAVAddr)
	}
	// The existing validation still applies to what a variable says.
	for _, kv := range [][]string{{EnvIMAPAddr, ":993"}, {EnvSMTPAddr, "0.0.0.0:465"}, {EnvCardDAVAddr, "mailflow:8443"}, {EnvIMAPAddr, "easbridge:993"}} {
		if _, err := Load(env(with(minimal(), append([]string{EnvListenIP, "10.0.0.5"}, kv...)...))); err == nil {
			t.Errorf("%v accepted", kv)
		}
	}
	// A variable that collides with a default is caught.
	if _, err := Load(env(with(minimal(), EnvListenIP, "10.0.0.5", EnvIMAPAddr, "10.0.0.5:8443"))); err == nil {
		t.Error("IMAP on the CardDAV default port of the same IP was accepted")
	}
}

// The configuration of 0.2.2 behaves as it did: nothing is derived, no interface is looked at.
func TestTheExistingLiveConfigurationIsUnchanged(t *testing.T) {
	c := mustLoad(t, map[string]string{
		EnvEASURL: "https://mail.example.test/Microsoft-Server-ActiveSync", EnvAllowedUsers: "alice@example.test",
		EnvIMAPAddr: "10.0.0.20:993", EnvSMTPAddr: "10.0.0.20:465", EnvCardDAVAddr: "10.0.0.20:8443",
	})
	if c.IMAPAddr != "10.0.0.20:993" || c.SMTPAddr != "10.0.0.20:465" || c.CardDAVAddr != "10.0.0.20:8443" || c.HealthAddr != "127.0.0.1:8081" {
		t.Errorf("addresses changed: %q %q %q %q", c.IMAPAddr, c.SMTPAddr, c.CardDAVAddr, c.HealthAddr)
	}
	if !c.Contacts || !c.ContactsEnabled() || c.WildcardListeners() || c.ListensOnAnyInterface() {
		t.Errorf("contacts stay on, nothing is a wildcard: %+v", c)
	}
	boom := func() ([]Iface, error) {
		t.Fatal("the interfaces were listed for a configuration with explicit addresses")
		return nil, nil
	}
	if err := c.CheckListenNetwork(boom); err != nil {
		t.Errorf("checked: %v", err)
	}
	// Not even on a host (the addresses are the operator's responsibility, as before).
	if err := c.CheckListenNetwork(ifaces("lo=127.0.0.1", "wlan0=192.0.2.9")); err != nil {
		t.Errorf("host: %v", err)
	}
	// And the listener check of 0.2.2 is the same: the wildcard in a variable still needs the flag.
	if _, err := Load(env(with(minimal(), EnvIMAPAddr, ":993", EnvSMTPAddr, "10.0.0.20:465"))); err == nil {
		t.Error("a wildcard in a variable needs EASBRIDGE_ALLOW_ANY_BIND")
	}
}

func TestListenIPMustBeAnAddressOfTheContainer(t *testing.T) {
	c := mustLoad(t, with(minimal(), EnvListenIP, "10.0.0.20"))
	plain := ifaces("lo=127.0.0.1,::1", "eth0=10.0.0.20,fe80::42")
	if err := c.CheckListenNetwork(plain); err != nil {
		t.Errorf("an address of eth0: %v", err)
	}
	if err := c.CheckListenNetwork(ifaces("lo=127.0.0.1", "eth0=10.0.0.9", "eth1=10.1.0.3")); err == nil ||
		!strings.Contains(err.Error(), "EASBRIDGE_LISTEN_IP=10.0.0.20 is not an address of this container") ||
		!strings.Contains(err.Error(), "eth0 10.0.0.9") || !strings.Contains(err.Error(), "eth1 10.1.0.3") {
		t.Errorf("a wrong address must be named with the existing ones: %v", err)
	}
	// An address on a second network is fine; the loopback is an interface too.
	c = mustLoad(t, with(minimal(), EnvListenIP, "10.1.0.3"))
	if err := c.CheckListenNetwork(ifaces("lo=127.0.0.1", "eth0=10.0.0.9", "eth1=10.1.0.3")); err != nil {
		t.Errorf("second network: %v", err)
	}
	c = mustLoad(t, with(minimal(), EnvListenIP, "::1"))
	if err := c.CheckListenNetwork(plain); err != nil {
		t.Errorf("the IPv6 loopback: %v", err)
	}
	// With LISTEN_IP set the host guard is not applied: the address is the choice.
	c = mustLoad(t, with(minimal(), EnvListenIP, "192.0.2.9"))
	if err := c.CheckListenNetwork(ifaces("lo=127.0.0.1", "wlan0=192.0.2.9", "docker0=172.17.0.1")); err != nil {
		t.Errorf("a host with the address named: %v", err)
	}
	// Only used when a listener uses it.
	c = mustLoad(t, with(minimal(), EnvListenIP, "192.0.2.77", EnvIMAPAddr, "127.0.0.1:1993", EnvSMTPAddr, "127.0.0.1:1465", EnvCardDAVAddr, "127.0.0.1:1443"))
	if err := c.CheckListenNetwork(plain); err != nil {
		t.Errorf("all three listeners have their own: %v", err)
	}
	// The lister failing is an error, not a pass.
	if err := mustLoad(t, minimal()).CheckListenNetwork(func() ([]Iface, error) { return nil, errTest }); err == nil {
		t.Error("a failing lister passed the check")
	}
}

var errTest = &testErr{}

type testErr struct{}

func (*testErr) Error() string { return "no netlink" }

func TestTheWildcardNeedsANamespaceThatLooksLikeAContainer(t *testing.T) {
	c := mustLoad(t, minimal())
	for name, ok := range map[string]InterfaceLister{
		"plain container":           ifaces("lo=127.0.0.1", "eth0=172.17.0.2"),
		"two-network container":     ifaces("lo=127.0.0.1", "eth0=172.17.0.2", "eth1=10.1.0.3"),
		"eth10 and more":            ifaces("lo=127.0.0.1", "eth0=172.17.0.2", "eth10=10.1.0.3"),
		"no network at all":         ifaces("lo=127.0.0.1"),
		"IPv6 only container":       ifaces("lo=::1", "eth0=fd00::5"),
		"container with no address": ifaces("lo=127.0.0.1", "eth0"),
	} {
		if err := c.CheckListenNetwork(ok); err != nil {
			t.Errorf("%s refused: %v", name, err)
		}
	}
	for name, spec := range map[string][]string{
		"host networking":           {"lo=127.0.0.1", "enp3s0=192.0.2.9", "docker0=172.17.0.1", "br-1a2b3c=172.20.0.1", "veth9f8e7d"},
		"laptop with wlan0":         {"lo=127.0.0.1", "wlan0=192.0.2.9"},
		"laptop with enp and wlan":  {"lo=127.0.0.1", "enp0s31f6", "wlp2s0=192.0.2.9"},
		"only docker0":              {"lo=127.0.0.1", "docker0=172.17.0.1"},
		"a bridge":                  {"lo=127.0.0.1", "br-1a2b3c=172.20.0.1"},
		"veth pairs":                {"lo=127.0.0.1", "veth1234"},
		"a VPN next to eth0":        {"lo=127.0.0.1", "eth0=172.17.0.2", "tailscale0=100.64.0.2"},
		"eth0 with a suffix":        {"lo=127.0.0.1", "eth0.100=172.17.0.2"},
		"names that only start eth": {"lo=127.0.0.1", "ethernet0=172.17.0.2"},
	} {
		err := c.CheckListenNetwork(ifaces(spec...))
		if err == nil {
			t.Errorf("%s was accepted", name)
			continue
		}
		for _, want := range []string{EnvListenIP, EnvIMAPAddr, EnvSMTPAddr, EnvCardDAVAddr, EnvAllowAnyBind, "does not look like a container"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: the message lacks %q: %v", name, want, err)
			}
		}
	}
	// The override.
	c = mustLoad(t, with(minimal(), EnvAllowAnyBind, "true"))
	if err := c.CheckListenNetwork(ifaces("lo=127.0.0.1", "wlan0=192.0.2.9")); err != nil {
		t.Errorf("EASBRIDGE_ALLOW_ANY_BIND=true did not override the guard: %v", err)
	}
	if !strings.Contains(c.ListenMode(), "not checked") {
		t.Errorf("mode: %s", c.ListenMode())
	}
	// A partly derived configuration is a wildcard too.
	c = mustLoad(t, with(minimal(), EnvIMAPAddr, "10.0.0.20:993"))
	if !c.WildcardListeners() || c.CheckListenNetwork(ifaces("lo=127.0.0.1", "wlan0=192.0.2.9")) == nil {
		t.Error("SMTP and CardDAV still use the wildcard; the host must be refused")
	}
}

func TestContactsAreOnByDefaultAndOffByName(t *testing.T) {
	if c := mustLoad(t, minimal()); !c.Contacts || !c.ContactsEnabled() || c.CardDAVAddr != ":8443" {
		t.Errorf("default: %+v", c)
	}
	for _, v := range []string{"on", "ON", " on "} {
		if c := mustLoad(t, with(minimal(), EnvContacts, v)); !c.ContactsEnabled() {
			t.Errorf("%q", v)
		}
	}
	c := mustLoad(t, with(minimal(), EnvContacts, "off"))
	if c.Contacts || c.ContactsEnabled() || c.DirectoryEnabled() || c.CardDAVAddr != "" {
		t.Errorf("off: %+v", c)
	}
	_, err := Load(env(with(minimal(), EnvContacts, "off", EnvCardDAVAddr, "10.0.0.20:8443")))
	if err == nil || !strings.Contains(err.Error(), EnvContacts) || !strings.Contains(err.Error(), EnvCardDAVAddr) {
		t.Errorf("off with an address: %v", err)
	}
	for _, v := range []string{"true", "1", "yes", "maybe"} {
		if _, err := Load(env(with(minimal(), EnvContacts, v))); err == nil || !strings.Contains(err.Error(), EnvContacts) {
			t.Errorf("%q: %v", v, err)
		}
	}
	// 0.2.2's configuration: the address set, the variable unset.
	c = mustLoad(t, with(minimal(), EnvIMAPAddr, "10.0.0.20:993", EnvSMTPAddr, "10.0.0.20:465", EnvCardDAVAddr, "10.0.0.20:8443"))
	if !c.ContactsEnabled() || c.CardDAVAddr != "10.0.0.20:8443" {
		t.Errorf("existing configuration: %+v", c)
	}
}

func TestAllowedUsersAcceptDomainEntries(t *testing.T) {
	c := mustLoad(t, with(minimal(), EnvAllowedUsers, " @Example.COM , bob@other.test, @example.com, carol"))
	if got := strings.Join(c.AllowedUsers, ","); got != "@example.com,bob@other.test,carol" {
		t.Errorf("entries %q", got)
	}
	// The directory domains default to the domains of every entry, the domain entries included.
	if got := strings.Join(c.DirectoryDomains, ","); got != "example.com,other.test" {
		t.Errorf("directory domains %q", got)
	}
	for in, want := range map[string]bool{
		"anyone@example.com": true, "ANYONE@Example.Com": true, "a.b+c@example.com": true,
		"bob@other.test": true, "carol": true,
		"bob@sub.example.com":        false, // no subdomains
		"bob@notexample.com":         false,
		"bob@example.com.evil.test":  false,
		"bob@evilexample.com":        false,
		"bob@other.test.example.org": false,
		"alice@other.test":           false, // only bob is named in that domain
		"@example.com":               false, // no local part
		"x@y@example.com":            false,
		"example.com":                false,
		"":                           false,
	} {
		if got := c.UserAllowed(in); got != want {
			t.Errorf("UserAllowed(%q) = %v, want %v", in, got, want)
		}
	}
	for _, bad := range []string{"@", "@bad domain", "@.com", "@example.com.", "@-x.com", "@exa_mple.com"} {
		if _, err := Load(env(with(minimal(), EnvAllowedUsers, bad))); err == nil || !strings.Contains(err.Error(), EnvAllowedUsers) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	// The variable stays required.
	for _, none := range []string{"", " , ,"} {
		if _, err := Load(env(with(minimal(), EnvAllowedUsers, none))); err == nil || !strings.Contains(err.Error(), EnvAllowedUsers) {
			t.Errorf("%q: %v", none, err)
		}
	}
	// A domain entry alone gives the directory its domain; "none" still turns it off.
	c = mustLoad(t, with(minimal(), EnvAllowedUsers, "@example.com", EnvDirectoryDomains, "none"))
	if c.DirectoryEnabled() {
		t.Error("none must turn the directory off")
	}
}

func TestTLSSettings(t *testing.T) {
	c := mustLoad(t, minimal())
	if c.TLSCertSet || c.TLSKeySet || c.TLSCert != "/tls/server.pem" || c.TLSKey != "/tls/server-key.pem" || len(c.TLSNames) != 0 || c.CAExportDir != "" {
		t.Errorf("defaults: %+v", c)
	}
	c = mustLoad(t, with(minimal(), EnvTLSCert, "/x/c.pem", EnvTLSKey, "/x/k.pem", EnvCAExportDir, "/ca"))
	if !c.TLSCertSet || !c.TLSKeySet || c.TLSCert != "/x/c.pem" || c.CAExportDir != "/ca" {
		t.Errorf("given: %+v", c)
	}
	// An empty variable is no variable.
	if c := mustLoad(t, with(minimal(), EnvTLSCert, " ")); c.TLSCertSet {
		t.Error("a blank variable counts as set")
	}
	c = mustLoad(t, with(minimal(), EnvTLSNames, " Mail.Internal, 192.0.2.44 ,mail.internal, ::ffff:10.0.0.7,,easbridge"))
	if got := strings.Join(c.TLSNames, ","); got != "mail.internal,192.0.2.44,10.0.0.7,easbridge" {
		t.Errorf("names %q", got)
	}
	for _, bad := range []string{"has space", "under_score", "host:993", "0.0.0.0", "::", "fe80::1%eth0", "-x.test", "a..b"} {
		if _, err := Load(env(with(minimal(), EnvTLSNames, bad))); err == nil || !strings.Contains(err.Error(), EnvTLSNames) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestAllowListEntriesAreValidated(t *testing.T) {
	for _, ok := range []string{"alice@example.com", "Alice.B+tag@Example.COM", "@example.com", "DOMAIN\\alice", "alice", "jörg@example.com", "عمر@example.com"} {
		if _, err := Load(env(with(minimal(), EnvAllowedUsers, ok))); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"alice smith@example.com", "alice@@example.com", "a@b@example.com", "alice@", "alice@exa mple.com", "alice@-bad.com", `"alice"@example.com`,
		"<alice@example.com>", "alice@example.com;bob@example.com", "alice(x)@example.com", "alice@example..com", "@bad domain", "a\tb@example.com"} {
		_, err := Load(env(with(minimal(), EnvAllowedUsers, bad)))
		if err == nil || !strings.Contains(err.Error(), EnvAllowedUsers) {
			t.Errorf("%q accepted or the error does not name the variable: %v", bad, err)
			continue
		}
		if !strings.Contains(err.Error(), "name@example.com") && !strings.Contains(err.Error(), "@example.com") {
			t.Errorf("%q: the error does not say what to write: %v", bad, err)
		}
	}
}

func TestTheWildcardMessageSaysWhatToDoInstead(t *testing.T) {
	_, err := Load(env(with(minimal(), EnvIMAPAddr, ":993")))
	if err == nil {
		t.Fatal("accepted")
	}
	for _, want := range []string{"Leave the variable out", EnvListenIP, EnvAllowAnyBind + "=true", "10.0.0.5:993"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message lacks %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "=1") {
		t.Error("the message still advises =1")
	}
}
