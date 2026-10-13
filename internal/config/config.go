// Package config reads easbridge's settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"easbridge/internal/version"
)

// Config is the validated runtime configuration.
type Config struct {
	// EASURL is the Exchange ActiveSync endpoint. https only.
	EASURL string
	// AllowedUsers is the allow-list of login names (lower case). Nobody else
	// is ever forwarded to Exchange. An entry that starts with "@" is a domain entry: it
	// allows every login of exactly that domain (no subdomains).
	AllowedUsers []string
	// EASVersion is the preferred protocol version ("16.1" falls back to "14.1").
	EASVersion string
	// DeviceType and UserAgent identify the bridge to Exchange (the DeviceType URL
	// parameter and the HTTP User-Agent). Together with DeviceModel,
	// DeviceFriendlyName, DeviceOS and DeviceOSLanguage they make up the device
	// information of the initial Provision request (see Device). The device is the
	// bridge, honestly: never the name of another product.
	DeviceType         string
	DeviceModel        string
	DeviceFriendlyName string
	DeviceOS           string
	DeviceOSLanguage   string
	UserAgent          string
	// Filter is the sync window: all, 1d, 3d, 1w, 2w or 1m.
	Filter string
	// MIMEInlineMax is the largest MIME body accepted inline in a Sync
	// response; larger ones are fetched with ItemOperations.
	MIMEInlineMax int
	// PingHeartbeat is the initial Ping heartbeat (adaptive downwards).
	PingHeartbeat time.Duration

	// IMAPAddr and SMTPAddr are where the listeners bind. Their own variables win. When a
	// variable is unset the address comes from the defaults (v0.3, docs/DESIGN-v0.3.md):
	// ListenIP (when set) or every address of the container, on port 993 (IMAP) and 465
	// (SMTP). An address given in a variable must name one IP address (a bridge that
	// listens on every interface of a host that also faces the outside is exposed): the
	// unspecified addresses (empty host, 0.0.0.0, ::) are refused unless AllowAnyBind is set.
	IMAPAddr   string
	SMTPAddr   string
	HealthAddr string
	// ListenIP (EASBRIDGE_LISTEN_IP) is the one IP address the listeners without an address
	// of their own bind. Empty: every address of the container, checked by
	// CheckListenNetwork.
	ListenIP string
	// AllowAnyBind (EASBRIDGE_ALLOW_ANY_BIND=true) permits the unspecified bind addresses
	// given in a variable, and turns the container check of the wildcard default off.
	AllowAnyBind bool
	// imapDerived, smtpDerived and carddavDerived say that the listener's address came from
	// the defaults and not from its own variable.
	imapDerived, smtpDerived, carddavDerived bool
	TLSCert                                  string
	TLSKey                                   string
	// TLSCertSet and TLSKeySet say that the variable was given (the defaults are paths a
	// provided certificate may be found at). Without either, and without files at the
	// default paths, the bridge makes its own certificate (internal/tlsauto).
	TLSCertSet, TLSKeySet bool
	// TLSNames are extra names (DNS names or IP literals) of the automatic server
	// certificate.
	TLSNames []string
	// CAExportDir, when set, receives a copy of the automatic CA certificate (ca.pem).
	CAExportDir string
	DataDir     string
	LogLevel    slog.Level
	// LogFormat is "text" (default) or "json".
	LogFormat string

	AuthFailCooldown time.Duration
	EngineGrace      time.Duration
	MaxMessageBytes  int64

	// EASCAFile optionally adds trusted CA certificates (PEM) for the
	// connection to Exchange, for servers with a private CA.
	EASCAFile string

	// Contacts (EASBRIDGE_CONTACTS=on|off, default on) switches the contacts feature.
	Contacts bool
	// CardDAVAddr is where the read-only CardDAV listener binds (TLS, the same
	// certificate as IMAP and SMTP). Empty: the whole contacts feature is off (no
	// contacts sync, no directory lookups, no listener). With Contacts on and no variable it
	// follows ListenIP on port 8443. The same bind rules as IMAP and SMTP apply, and it
	// must differ from their addresses.
	CardDAVAddr string
	// ContactsInterval is how often the Contacts folders are synced (polling; they
	// are not part of Ping).
	ContactsInterval time.Duration
	// DirectoryDomains are the domains whose addresses are looked up in the Global
	// Address List, one search at a time (docs/DESIGN-contacts.md). Empty turns the
	// directory off: EASBRIDGE_DIRECTORY_DOMAINS=none, or no allowed user has a
	// domain to default to.
	DirectoryDomains []string
	// DirectoryLookupsPerHour (1 to 180) sets the spacing of the GAL searches:
	// an hour divided by it, and never less than 20 seconds. The daily cap is ten
	// times this value.
	DirectoryLookupsPerHour int
	// DirectoryRefresh is how long a found card is kept before it is looked up again.
	DirectoryRefresh time.Duration
	// DirectoryFull (EASBRIDGE_DIRECTORY_FULL=on) turns on the opt-in full sweep of the
	// Global Address List (docs/DESIGN-contacts.md section 9). Off by default; it needs the
	// directory and CardDAV to be on.
	DirectoryFull bool
	// DirectoryFullInterval is the time from the end of one finished sweep to the start of
	// the next.
	DirectoryFullInterval time.Duration
	// ContactsBookName and DirectoryBookName are the display names of the address
	// book of the default Contacts folder and of the directory.
	ContactsBookName  string
	DirectoryBookName string

	// Upstream selects the mailbox backend: "eas" (default) or "fake" (only
	// in binaries built with the devfake tag).
	Upstream string
	// FakeControlAddr is the loopback address of the fake upstream's control
	// API (devfake builds only).
	FakeControlAddr string
}

// Env names.
const (
	EnvEASURL           = "EASBRIDGE_EAS_URL"
	EnvAllowedUsers     = "EASBRIDGE_ALLOWED_USERS"
	EnvEASVersion       = "EASBRIDGE_EAS_VERSION"
	EnvDeviceType       = "EASBRIDGE_DEVICE_TYPE"
	EnvDeviceModel      = "EASBRIDGE_DEVICE_MODEL"
	EnvDeviceFriendly   = "EASBRIDGE_DEVICE_FRIENDLY_NAME"
	EnvDeviceOS         = "EASBRIDGE_DEVICE_OS"
	EnvDeviceOSLanguage = "EASBRIDGE_DEVICE_OS_LANGUAGE"
	EnvUserAgent        = "EASBRIDGE_USER_AGENT"
	EnvFilter           = "EASBRIDGE_FILTER"
	EnvMIMEInlineMax    = "EASBRIDGE_MIME_INLINE_MAX"
	EnvPingHeartbeat    = "EASBRIDGE_PING_HEARTBEAT"
	EnvIMAPAddr         = "EASBRIDGE_IMAP_ADDR"
	EnvSMTPAddr         = "EASBRIDGE_SMTP_ADDR"
	EnvHealthAddr       = "EASBRIDGE_HEALTH_ADDR"
	EnvAllowAnyBind     = "EASBRIDGE_ALLOW_ANY_BIND"
	EnvListenIP         = "EASBRIDGE_LISTEN_IP"
	EnvContacts         = "EASBRIDGE_CONTACTS"
	EnvTLSNames         = "EASBRIDGE_TLS_NAMES"
	EnvCAExportDir      = "EASBRIDGE_CA_EXPORT_DIR"
	EnvTLSCert          = "EASBRIDGE_TLS_CERT"
	EnvTLSKey           = "EASBRIDGE_TLS_KEY"
	EnvDataDir          = "EASBRIDGE_DATA_DIR"
	EnvLogLevel         = "EASBRIDGE_LOG_LEVEL"
	EnvLogFormat        = "EASBRIDGE_LOG_FORMAT"
	EnvAuthFailCooldown = "EASBRIDGE_AUTH_FAIL_COOLDOWN"
	EnvEngineGrace      = "EASBRIDGE_ENGINE_GRACE"
	EnvMaxMessageBytes  = "EASBRIDGE_MAX_MESSAGE_BYTES"
	EnvEASCAFile        = "EASBRIDGE_EAS_CA_FILE"
	EnvCardDAVAddr      = "EASBRIDGE_CARDDAV_ADDR"
	EnvContactsInterval = "EASBRIDGE_CONTACTS_INTERVAL"
	EnvDirectoryDomains = "EASBRIDGE_DIRECTORY_DOMAINS"
	EnvDirectoryLookups = "EASBRIDGE_DIRECTORY_LOOKUPS_PER_HOUR"
	EnvDirectoryRefresh = "EASBRIDGE_DIRECTORY_REFRESH"
	EnvDirectoryFull    = "EASBRIDGE_DIRECTORY_FULL"
	EnvDirectoryFullInt = "EASBRIDGE_DIRECTORY_FULL_INTERVAL"
	EnvContactsBookName = "EASBRIDGE_CONTACTS_BOOK_NAME"
	EnvDirectoryBook    = "EASBRIDGE_DIRECTORY_BOOK_NAME"
	EnvUpstream         = "EASBRIDGE_UPSTREAM"
	EnvFakeControl      = "EASBRIDGE_FAKE_CONTROL_ADDR"
)

// Defaults returns the configuration with every default applied.
func Defaults() *Config {
	return &Config{
		EASVersion:         "16.1",
		DeviceType:         DefaultDeviceType,
		DeviceModel:        DefaultDeviceModel,
		DeviceFriendlyName: DefaultDeviceFriendlyName,
		DeviceOS:           DefaultDeviceOS,
		DeviceOSLanguage:   DefaultDeviceOSLanguage,
		UserAgent:          version.UserAgent(),
		Filter:             "all",
		MIMEInlineMax:      1 << 20,
		PingHeartbeat:      480 * time.Second,
		HealthAddr:         "127.0.0.1:8081",
		TLSCert:            "/tls/server.pem",
		TLSKey:             "/tls/server-key.pem",
		DataDir:            "/data",
		LogLevel:           slog.LevelInfo,
		LogFormat:          "text",
		AuthFailCooldown:   15 * time.Minute,
		EngineGrace:        30 * time.Minute,
		MaxMessageBytes:    35 << 20,
		Upstream:           "eas",
		Contacts:           true,

		ContactsInterval:        30 * time.Minute,
		DirectoryLookupsPerHour: 60,
		DirectoryRefresh:        30 * 24 * time.Hour,
		DirectoryFullInterval:   720 * time.Hour,
		ContactsBookName:        "Exchange contacts",
		DirectoryBookName:       "Directory",
	}
}

// Limits of the contacts settings.
const (
	MinContactsInterval = 5 * time.Minute
	MaxContactsInterval = 24 * time.Hour
	MinDirectoryRefresh = 24 * time.Hour
	MaxDirectoryRefresh = 365 * 24 * time.Hour
	// MinFullInterval is the least time between the end of a full sweep and the start of
	// the next.
	MinFullInterval = 168 * time.Hour
	MaxFullInterval = 365 * 24 * time.Hour
	// MaxDirectoryLookupsPerHour is what the 20 second spacing between two
	// searches allows: 3600 / 20.
	MaxDirectoryLookupsPerHour = 180
	// MinSearchSpacing is the least time between two GAL searches, whatever the
	// rate setting.
	MinSearchSpacing = 20 * time.Second
	maxBookName      = 64
)

// The device the bridge presents to Exchange. Honest on purpose: it says what it
// is and never names another product (not Outlook, not an iPhone). The
// organisation approves a device by its id and type, so changing DeviceType (or
// the id, which lives in the state database) makes the bridge a new device.
const (
	DefaultDeviceType         = "easbridge"
	DefaultDeviceModel        = "easbridge"
	DefaultDeviceFriendlyName = "easbridge (MailFlow bridge)"
	DefaultDeviceOS           = "Linux"
	DefaultDeviceOSLanguage   = "en-us"
)

// maxDeviceText is the longest device description accepted (bytes). Exchange
// shows these in its device list; the defaults are far shorter.
const maxDeviceText = 128

// Device is how the bridge describes itself to Exchange: the DeviceType of every
// request and, from protocol 14.1 on, the device information of the initial
// Provision request (MS-ASPROV 2.2.2.53: Model is mandatory there). No IMEI, phone
// number or mobile operator is ever sent.
type Device struct {
	Type         string // DeviceType, a URL parameter of every request
	Model        string
	FriendlyName string
	OS           string
	OSLanguage   string
	UserAgent    string // the HTTP User-Agent and the UserAgent of the device information
}

// Device returns the device settings of c.
func (c *Config) Device() Device {
	return Device{Type: c.DeviceType, Model: c.DeviceModel, FriendlyName: c.DeviceFriendlyName,
		OS: c.DeviceOS, OSLanguage: c.DeviceOSLanguage, UserAgent: c.UserAgent}
}

// Validate reports every problem with d: nothing may be empty, and no value may
// hold control characters (they cannot travel in a header or in WBXML) or exceed
// 128 bytes.
func (d Device) Validate() error {
	var errs []error
	for _, f := range []struct{ name, val string }{
		{EnvDeviceType, d.Type}, {EnvDeviceModel, d.Model}, {EnvDeviceFriendly, d.FriendlyName},
		{EnvDeviceOS, d.OS}, {EnvDeviceOSLanguage, d.OSLanguage}, {EnvUserAgent, d.UserAgent},
	} {
		switch {
		case strings.TrimSpace(f.val) == "":
			errs = append(errs, fmt.Errorf("%s must not be empty", f.name))
		case len(f.val) > maxDeviceText:
			errs = append(errs, fmt.Errorf("%s: at most %d bytes, got %d", f.name, maxDeviceText, len(f.val)))
		case strings.IndexFunc(f.val, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0:
			errs = append(errs, fmt.Errorf("%s must not contain control characters", f.name))
		}
	}
	return errors.Join(errs...)
}

// LoadDevice reads only the device settings from the environment, with the same
// names, defaults and checks as Load. `easbridge probe` uses it so that the probe
// presents exactly the device the service does.
func LoadDevice(getenv func(string) string) (Device, error) {
	c := Defaults()
	c.loadDevice(getenv)
	if err := c.Device().Validate(); err != nil {
		return Device{}, err
	}
	return c.Device(), nil
}

// loadDevice applies the device variables that are set (an empty one keeps the
// default). It does not validate; Load and LoadDevice do.
func (c *Config) loadDevice(getenv func(string) string) {
	for _, f := range []struct {
		name string
		dst  *string
	}{
		{EnvDeviceType, &c.DeviceType}, {EnvDeviceModel, &c.DeviceModel}, {EnvDeviceFriendly, &c.DeviceFriendlyName},
		{EnvDeviceOS, &c.DeviceOS}, {EnvDeviceOSLanguage, &c.DeviceOSLanguage}, {EnvUserAgent, &c.UserAgent},
	} {
		if v := strings.TrimSpace(getenv(f.name)); v != "" {
			*f.dst = v
		}
	}
}

// Load builds a Config from getenv (os.Getenv in production). All problems
// are reported together.
func Load(getenv func(string) string) (*Config, error) {
	c := Defaults()
	var errs []error
	str := func(name string, dst *string) {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			*dst = v
		}
	}
	bad := func(name string, err error) { errs = append(errs, fmt.Errorf("%s: %w", name, err)) }

	str(EnvEASURL, &c.EASURL)
	str(EnvEASVersion, &c.EASVersion)
	c.loadDevice(getenv)
	str(EnvFilter, &c.Filter)
	str(EnvIMAPAddr, &c.IMAPAddr)
	str(EnvSMTPAddr, &c.SMTPAddr)
	str(EnvHealthAddr, &c.HealthAddr)
	str(EnvTLSCert, &c.TLSCert)
	str(EnvTLSKey, &c.TLSKey)
	c.TLSCertSet = strings.TrimSpace(getenv(EnvTLSCert)) != ""
	c.TLSKeySet = strings.TrimSpace(getenv(EnvTLSKey)) != ""
	str(EnvCAExportDir, &c.CAExportDir)
	if err := c.loadTLSNames(getenv(EnvTLSNames)); err != nil {
		bad(EnvTLSNames, err)
	}
	str(EnvDataDir, &c.DataDir)
	str(EnvEASCAFile, &c.EASCAFile)
	str(EnvLogFormat, &c.LogFormat)
	str(EnvUpstream, &c.Upstream)
	str(EnvFakeControl, &c.FakeControlAddr)
	str(EnvCardDAVAddr, &c.CardDAVAddr)
	str(EnvContactsBookName, &c.ContactsBookName)
	str(EnvDirectoryBook, &c.DirectoryBookName)

	users, uerrs := ParseAllowedUsers(getenv(EnvAllowedUsers))
	c.AllowedUsers = users
	for _, err := range uerrs {
		bad(EnvAllowedUsers, err)
	}

	switch v := strings.ToLower(strings.TrimSpace(getenv(EnvAllowAnyBind))); v {
	case "", "0", "false":
	case "1", "true":
		c.AllowAnyBind = true
	default:
		bad(EnvAllowAnyBind, fmt.Errorf("want true (or leave it unset), got %q", getenv(EnvAllowAnyBind)))
	}
	if v := strings.TrimSpace(getenv(EnvListenIP)); v != "" {
		if ip, err := parseListenIP(v); err != nil {
			bad(EnvListenIP, err)
		} else {
			c.ListenIP = ip
		}
	}
	switch v := strings.ToLower(strings.TrimSpace(getenv(EnvContacts))); v {
	case "", "on":
	case "off":
		c.Contacts = false
	default:
		bad(EnvContacts, fmt.Errorf("want on or off, got %q", getenv(EnvContacts)))
	}
	if !c.Contacts && c.CardDAVAddr != "" {
		bad(EnvContacts, fmt.Errorf("off, but %s is set (%s): remove one of them", EnvCardDAVAddr, c.CardDAVAddr))
	}
	// The listeners without an address of their own take it from the defaults.
	derive := func(addr *string, port string, flag *bool) {
		if *addr != "" {
			return
		}
		*flag = true
		if c.ListenIP != "" {
			*addr = net.JoinHostPort(c.ListenIP, port)
		} else {
			*addr = ":" + port
		}
	}
	derive(&c.IMAPAddr, DefaultIMAPPort, &c.imapDerived)
	derive(&c.SMTPAddr, DefaultSMTPPort, &c.smtpDerived)
	if c.Contacts {
		derive(&c.CardDAVAddr, DefaultCardDAVPort, &c.carddavDerived)
	}
	if v := strings.TrimSpace(getenv(EnvLogLevel)); v != "" {
		if err := c.LogLevel.UnmarshalText([]byte(v)); err != nil {
			bad(EnvLogLevel, fmt.Errorf("want debug, info, warn or error, got %q", v))
		}
	}
	if v := strings.TrimSpace(getenv(EnvMIMEInlineMax)); v != "" {
		n, err := ParseSize(v)
		if err != nil || n < 1024 || n > 32<<20 {
			bad(EnvMIMEInlineMax, fmt.Errorf("want a size between 1KiB and 32MiB, got %q", v))
		} else {
			c.MIMEInlineMax = int(n)
		}
	}
	if v := strings.TrimSpace(getenv(EnvMaxMessageBytes)); v != "" {
		n, err := ParseSize(v)
		if err != nil || n < 1<<20 || n > 512<<20 {
			bad(EnvMaxMessageBytes, fmt.Errorf("want a size between 1MiB and 512MiB, got %q", v))
		} else {
			c.MaxMessageBytes = n
		}
	}
	for _, d := range []struct {
		name string
		dst  *time.Duration
		min  time.Duration
		max  time.Duration
	}{
		{EnvPingHeartbeat, &c.PingHeartbeat, 60 * time.Second, 59 * time.Minute},
		{EnvAuthFailCooldown, &c.AuthFailCooldown, time.Second, 24 * time.Hour},
		{EnvEngineGrace, &c.EngineGrace, 0, 7 * 24 * time.Hour},
		{EnvContactsInterval, &c.ContactsInterval, MinContactsInterval, MaxContactsInterval},
		{EnvDirectoryRefresh, &c.DirectoryRefresh, MinDirectoryRefresh, MaxDirectoryRefresh},
		{EnvDirectoryFullInt, &c.DirectoryFullInterval, MinFullInterval, MaxFullInterval},
	} {
		v := strings.TrimSpace(getenv(d.name))
		if v == "" {
			continue
		}
		dur, err := ParseDuration(v)
		if err != nil || dur < d.min || dur > d.max {
			bad(d.name, fmt.Errorf("want a duration between %v and %v (plain numbers are seconds), got %q", d.min, d.max, v))
			continue
		}
		*d.dst = dur
	}

	if v := strings.TrimSpace(getenv(EnvDirectoryLookups)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > MaxDirectoryLookupsPerHour {
			bad(EnvDirectoryLookups, fmt.Errorf("want a whole number from 1 to %d (consecutive searches are at least %v apart, which is what caps it), got %q",
				MaxDirectoryLookupsPerHour, MinSearchSpacing, v))
		} else {
			c.DirectoryLookupsPerHour = n
		}
	}
	if err := c.loadDirectoryDomains(getenv(EnvDirectoryDomains)); err != nil {
		bad(EnvDirectoryDomains, err)
	}
	switch v := strings.ToLower(strings.TrimSpace(getenv(EnvDirectoryFull))); v {
	case "", "off":
	case "on":
		c.DirectoryFull = true
	default:
		bad(EnvDirectoryFull, fmt.Errorf("want off or on, got %q", getenv(EnvDirectoryFull)))
	}

	if err := c.validate(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

// ParseAllowedUsers reads a comma-separated allow-list: lower case, without duplicates; an entry
// that starts with "@" is a domain entry and must be a valid domain. `easbridge init` uses it too, so
// that the file it writes is one the bridge accepts.
func ParseAllowedUsers(raw string) ([]string, []error) {
	var out []string
	var errs []error
	seen := map[string]bool{}
	for _, u := range strings.Split(raw, ",") {
		u = strings.ToLower(strings.TrimSpace(u))
		if u == "" || seen[u] {
			continue
		}
		if strings.HasPrefix(u, "@") && !validDomain(u[1:]) {
			errs = append(errs, fmt.Errorf("%q is not a domain entry (write @example.com: letters, digits, hyphens and dots)", u))
			continue
		}
		if !validLogin(u) {
			errs = append(errs, fmt.Errorf("%q is not a login name: no spaces, quotes or brackets; write name@example.com, DOMAIN\\name or @example.com for a whole domain", u))
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	return out, errs
}

// validLogin reports whether an allow-list entry (lower case, trimmed) can be a login: no space,
// control character, quote, bracket, comma or semicolon; with an @ it is name@domain with one @, a
// non-empty name and a valid domain; "@domain" entries were checked before. A login without @ (a
// user name, DOMAIN\\name) is accepted as it is: which form Exchange wants is its business.
func validLogin(u string) bool {
	for _, r := range u {
		if r <= 0x20 || r == 0x7f || strings.ContainsRune(`"'<>()[],;`, r) {
			return false
		}
	}
	if strings.HasPrefix(u, "@") {
		return true
	}
	if i := strings.Index(u, "@"); i >= 0 {
		return i > 0 && strings.Count(u, "@") == 1 && validDomain(u[i+1:])
	}
	return true
}

// ValidateEASURL applies the rules of EASBRIDGE_EAS_URL: an https URL with a host.
func ValidateEASURL(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return err
	case u.Scheme != "https":
		// Basic credentials would travel in clear text over http.
		return fmt.Errorf("must be an https:// URL (got scheme %q)", u.Scheme)
	case u.Host == "":
		return errors.New("has no host")
	}
	return nil
}

// loadTLSNames reads EASBRIDGE_TLS_NAMES: comma-separated DNS names or IP literals, lower case,
// without duplicates.
func (c *Config) loadTLSNames(raw string) error {
	seen := map[string]bool{}
	for _, n := range strings.Split(raw, ",") {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" {
			continue
		}
		if ip, err := netip.ParseAddr(n); err == nil {
			if ip.Zone() != "" || ip.IsUnspecified() {
				return fmt.Errorf("%q is not an address a certificate can name", n)
			}
			n = ip.Unmap().String()
		} else if !validDomain(n) {
			return fmt.Errorf("%q is not a DNS name or an IP address (letters, digits, hyphens and dots; no spaces, no port)", n)
		}
		if !seen[n] {
			seen[n] = true
			c.TLSNames = append(c.TLSNames, n)
		}
	}
	return nil
}

// loadDirectoryDomains sets DirectoryDomains from the variable (a comma-separated
// list, or "none"). Unset or empty means the domains of the allowed users; that is
// applied after the allow-list is read, so it must run after AllowedUsers is set.
func (c *Config) loadDirectoryDomains(raw string) error {
	raw = strings.ToLower(strings.TrimSpace(raw))
	switch raw {
	case "none":
		c.DirectoryDomains = nil
		return nil
	case "":
		seen := map[string]bool{}
		c.DirectoryDomains = nil
		for _, u := range c.AllowedUsers {
			if _, d, ok := strings.Cut(u, "@"); ok && d != "" && validDomain(d) && !seen[d] {
				seen[d] = true
				c.DirectoryDomains = append(c.DirectoryDomains, d)
			}
		}
		return nil
	}
	seen := map[string]bool{}
	c.DirectoryDomains = nil
	for _, d := range strings.Split(raw, ",") {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		if d == "none" {
			return errors.New(`"none" cannot be combined with domains`)
		}
		if !validDomain(d) {
			return fmt.Errorf("%q is not a domain name (letters, digits, hyphens and dots, for example example.com; no @, no spaces)", d)
		}
		if !seen[d] {
			seen[d] = true
			c.DirectoryDomains = append(c.DirectoryDomains, d)
		}
	}
	if len(c.DirectoryDomains) == 0 {
		return errors.New("no domain given (use none to turn the directory off)")
	}
	return nil
}

// validDomain reports whether d is a plausible lower-case ASCII host name:
// dot-separated labels of letters, digits and hyphens, none empty, none starting or
// ending with a hyphen, at most 253 bytes in all.
func validDomain(d string) bool {
	if d == "" || len(d) > 253 || strings.HasPrefix(d, ".") || strings.HasSuffix(d, ".") {
		return false
	}
	for _, label := range strings.Split(d, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

// ContactsEnabled reports whether the contacts feature is on: it is exactly when
// a CardDAV address is configured.
func (c *Config) ContactsEnabled() bool { return c.CardDAVAddr != "" }

// DirectoryEnabled reports whether directory lookups run: contacts are on and at
// least one domain is configured.
func (c *Config) DirectoryEnabled() bool { return c.ContactsEnabled() && len(c.DirectoryDomains) > 0 }

// SearchSpacing is the least time between two GAL searches: an hour divided by the
// lookups per hour, and never less than MinSearchSpacing.
func (c *Config) SearchSpacing() time.Duration { return SpacingFor(c.DirectoryLookupsPerHour) }

// SpacingFor is SearchSpacing for a rate.
func SpacingFor(perHour int) time.Duration {
	if perHour < 1 {
		perHour = 1
	}
	d := time.Hour / time.Duration(perHour)
	if d < MinSearchSpacing {
		d = MinSearchSpacing
	}
	return d
}

func (c *Config) validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	switch c.Upstream {
	case "eas":
		if c.EASURL == "" {
			add("%s is required", EnvEASURL)
		}
	case "fake":
	default:
		add("%s: want eas or fake, got %q", EnvUpstream, c.Upstream)
	}
	if c.EASURL != "" {
		if err := ValidateEASURL(c.EASURL); err != nil {
			add("%s %v", EnvEASURL, err)
		}
	}
	if len(c.AllowedUsers) == 0 {
		add("%s is required (comma-separated login names)", EnvAllowedUsers)
	}
	switch c.EASVersion {
	case "14.0", "14.1", "16.0", "16.1":
	default:
		add("%s: want 14.0, 14.1, 16.0 or 16.1, got %q", EnvEASVersion, c.EASVersion)
	}
	switch c.Filter {
	case "all", "1d", "3d", "1w", "2w", "1m":
	default:
		add("%s: want all, 1d, 3d, 1w, 2w or 1m, got %q", EnvFilter, c.Filter)
	}
	switch c.LogFormat {
	case "text", "json":
	default:
		add("%s: want text or json, got %q", EnvLogFormat, c.LogFormat)
	}
	for _, a := range []struct {
		name, val string
		derived   bool
	}{
		{EnvIMAPAddr, c.IMAPAddr, c.imapDerived}, {EnvSMTPAddr, c.SMTPAddr, c.smtpDerived}, {EnvHealthAddr, c.HealthAddr, false},
	} {
		if a.derived {
			continue // from the defaults: CheckListenNetwork looks at the interfaces
		}
		if err := c.checkBind(a.name, a.val); err != nil {
			errs = append(errs, err)
		}
	}
	if c.DirectoryFull {
		switch {
		case !c.ContactsEnabled():
			add("%s=on needs the contacts feature: set %s to on", EnvDirectoryFull, EnvContacts)
		case !c.DirectoryEnabled():
			add("%s=on needs the directory: %s is none, or empty and no allowed user has a domain", EnvDirectoryFull, EnvDirectoryDomains)
		}
	}
	if c.CardDAVAddr != "" {
		if err := c.checkBindUnless(c.carddavDerived, EnvCardDAVAddr, c.CardDAVAddr); err != nil {
			errs = append(errs, err)
		} else {
			for _, o := range []struct{ name, addr string }{{EnvIMAPAddr, c.IMAPAddr}, {EnvSMTPAddr, c.SMTPAddr}, {EnvHealthAddr, c.HealthAddr}} {
				if sameListener(c.CardDAVAddr, o.addr) {
					add("%s: %q is the address of %s; the CardDAV listener needs an address of its own", EnvCardDAVAddr, c.CardDAVAddr, o.name)
				}
			}
		}
	}
	for _, b := range []struct{ name, val string }{{EnvContactsBookName, c.ContactsBookName}, {EnvDirectoryBook, c.DirectoryBookName}} {
		switch {
		case strings.TrimSpace(b.val) == "":
			add("%s must not be empty", b.name)
		case len(b.val) > maxBookName:
			add("%s: at most %d bytes, got %d", b.name, maxBookName, len(b.val))
		case strings.IndexFunc(b.val, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0:
			add("%s must not contain control characters", b.name)
		}
	}
	if strings.EqualFold(strings.TrimSpace(c.ContactsBookName), strings.TrimSpace(c.DirectoryBookName)) {
		add("%s and %s must differ", EnvContactsBookName, EnvDirectoryBook)
	}
	if c.FakeControlAddr != "" {
		host, _, err := net.SplitHostPort(c.FakeControlAddr)
		if err != nil {
			add("%s: %q is not host:port", EnvFakeControl, c.FakeControlAddr)
		} else if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			add("%s must be a loopback address", EnvFakeControl)
		}
	}
	if err := c.Device().Validate(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// checkBind validates a listen address. It fails closed: there is no default, the
// host must be an IP address, and an address that means "every interface" is
// refused unless AllowAnyBind is set.
func (c *Config) checkBindUnless(derived bool, name, addr string) error {
	if derived {
		return nil
	}
	return c.checkBind(name, addr)
}

func (c *Config) checkBind(name, addr string) error {
	if addr == "" {
		return fmt.Errorf("%s is empty: give the IP address and port to listen on, for example 10.0.0.5:993", name)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s: %q is not host:port", name, addr)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("%s: %q has no valid port", name, addr)
	}
	unspecified := host == ""
	if !unspecified {
		ip, err := netip.ParseAddr(host)
		if err != nil {
			return fmt.Errorf("%s: %q must name an IP address, not a host name, so that it is certain where the bridge listens", name, addr)
		}
		unspecified = ip.Unmap().IsUnspecified()
	}
	if unspecified && !c.AllowAnyBind {
		return fmt.Errorf("%s: %q would listen on every network interface of the host. Leave the variable out to listen on all addresses of the container "+
			"(EASBridge checks that it runs in one), or set %s to the one IP address it should use, or give the IP address and port MailFlow reaches "+
			"EASBridge on (for example 10.0.0.5:993), or set %s=true if listening on every address here is what you want", name, addr, EnvListenIP, EnvAllowAnyBind)
	}
	return nil
}

// sameListener reports whether two listen addresses would collide: the same port
// (other than 0, which means "any free port") on the same IP, or on an unspecified
// one that covers it.
func sameListener(a, b string) bool {
	ha, pa, err1 := net.SplitHostPort(a)
	hb, pb, err2 := net.SplitHostPort(b)
	if err1 != nil || err2 != nil || pa != pb || pa == "0" {
		return false
	}
	ia, ea := netip.ParseAddr(ha)
	ib, eb := netip.ParseAddr(hb)
	if ha == "" || (ea == nil && ia.Unmap().IsUnspecified()) || hb == "" || (eb == nil && ib.Unmap().IsUnspecified()) {
		return true
	}
	return ea == nil && eb == nil && ia.Unmap() == ib.Unmap()
}

// ListensOnAnyInterface reports whether one of the listen addresses is an
// unspecified one (only possible with AllowAnyBind); the bridge says so loudly
// when it starts.
func (c *Config) ListensOnAnyInterface() bool {
	for i, a := range []string{c.IMAPAddr, c.SMTPAddr, c.HealthAddr, c.CardDAVAddr} {
		if (i == 0 && c.imapDerived) || (i == 1 && c.smtpDerived) || (i == 3 && c.carddavDerived) {
			continue // the wildcard default, which CheckListenNetwork vets
		}
		host, _, err := net.SplitHostPort(a)
		if err != nil {
			continue
		}
		if host == "" {
			return true
		}
		if ip, err := netip.ParseAddr(host); err == nil && ip.Unmap().IsUnspecified() {
			return true
		}
	}
	return false
}

// FilterType returns the EAS FilterType value for the configured window, or 0
// for "all" (meaning: omit the element).
func (c *Config) FilterType() int {
	switch c.Filter {
	case "1d":
		return 1
	case "3d":
		return 2
	case "1w":
		return 3
	case "2w":
		return 4
	case "1m":
		return 5
	}
	return 0
}

// UserAllowed reports whether login name u (any case) is on the allow-list.
func (c *Config) UserAllowed(u string) bool {
	u = strings.ToLower(strings.TrimSpace(u))
	for _, a := range c.AllowedUsers {
		if a == u && !strings.HasPrefix(a, "@") {
			return true
		}
		// A domain entry (@example.com) allows a login whose part after the last @ is exactly
		// that domain: no subdomain, a non-empty local part without another @.
		if strings.HasPrefix(a, "@") {
			if i := strings.LastIndex(u, "@"); i > 0 && u[i:] == a && !strings.Contains(u[:i], "@") {
				return true
			}
		}
	}
	return false
}

// ParseSize parses "1048576", "512K", "512KiB", "35M", "35MB", "1GiB".
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	mult := int64(1)
	for _, suf := range []struct {
		s string
		m int64
	}{{"kib", 1 << 10}, {"kb", 1 << 10}, {"k", 1 << 10}, {"mib", 1 << 20}, {"mb", 1 << 20}, {"m", 1 << 20},
		{"gib", 1 << 30}, {"gb", 1 << 30}, {"g", 1 << 30}, {"b", 1}} {
		if strings.HasSuffix(s, suf.s) {
			s = strings.TrimSpace(strings.TrimSuffix(s, suf.s))
			mult = suf.m
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size")
	}
	return n * mult, nil
}

// ParseDuration accepts Go durations ("15m") and plain seconds ("480").
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n < 0 {
			return 0, fmt.Errorf("negative duration")
		}
		return time.Duration(n) * time.Second, nil
	}
	return time.ParseDuration(s)
}
