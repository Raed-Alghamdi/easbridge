// Package easupstream is the real upstream: it speaks Exchange ActiveSync to
// Exchange through the vendored, patched go-activesync, following the rules of
// DESIGN section 12 (staged keys, status 9/12 handling, Ping status handling,
// protocol version 16.1 with a 14.1 fallback after one OPTIONS, https only,
// URL redaction, ItemOperations concurrency 2).
package easupstream

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"easbridge/internal/logx"
	"easbridge/internal/upstream"
)

// Config configures the EAS upstream.
type Config struct {
	// URL is the Exchange ActiveSync endpoint; https only.
	URL string
	// Version is the preferred protocol version ("16.1"); 14.1 is the fallback.
	Version    string
	DeviceType string
	UserAgent  string
	// DeviceModel, DeviceFriendlyName, DeviceOS and DeviceOSLanguage, together with
	// UserAgent, are the device information Exchange gets in the initial Provision
	// request (protocol 14.1 and later; MS-ASPROV 2.2.2.53 makes it mandatory, and
	// Exchange answers status 165 without it), or in a Settings command after
	// provisioning on protocol 14.0. They say what the bridge is. They never name
	// another product, and no IMEI, phone number or mobile operator is sent.
	DeviceModel        string
	DeviceFriendlyName string
	DeviceOS           string
	DeviceOSLanguage   string
	// Filter is the EAS FilterType for Sync (0 = no filter, all mail).
	Filter int
	// MIMEInlineMax is the largest MIME body accepted inline in Sync.
	MIMEInlineMax int
	// WindowSize is the number of items per Sync response.
	WindowSize int
	// MaxHeartbeat is the longest Ping heartbeat that will be used; the HTTP
	// client timeout is this plus two minutes.
	MaxHeartbeat time.Duration
	// CAFile adds trusted CA certificates (PEM) to the system pool.
	CAFile string
	// HTTPClient overrides the HTTP client (tests). Its Timeout is respected.
	HTTPClient *http.Client
	// Logger receives debug events; the User query value is removed.
	Logger *slog.Logger
	// FetchConcurrency bounds simultaneous ItemOperations (default 2).
	FetchConcurrency int
}

func (c *Config) setDefaults() {
	if c.Version == "" {
		c.Version = "16.1"
	}
	if c.DeviceType == "" {
		c.DeviceType = "easbridge"
	}
	if c.UserAgent == "" {
		c.UserAgent = "easbridge"
	}
	if c.DeviceModel == "" {
		c.DeviceModel = "easbridge"
	}
	if c.DeviceFriendlyName == "" {
		c.DeviceFriendlyName = "easbridge (MailFlow bridge)"
	}
	if c.DeviceOS == "" {
		c.DeviceOS = "Linux"
	}
	if c.DeviceOSLanguage == "" {
		c.DeviceOSLanguage = "en-us"
	}
	if c.MIMEInlineMax <= 0 {
		c.MIMEInlineMax = 1 << 20
	}
	if c.WindowSize <= 0 {
		c.WindowSize = 20
	}
	if c.MaxHeartbeat <= 0 {
		c.MaxHeartbeat = 480 * time.Second
	}
	if c.FetchConcurrency <= 0 {
		c.FetchConcurrency = 2
	}
	if c.Logger == nil {
		c.Logger = logx.Discard()
	}
}

// Factory builds EAS mailboxes sharing one HTTP client.
type Factory struct {
	cfg  Config
	http *http.Client
}

var _ upstream.Factory = (*Factory)(nil)

// NewFactory validates the configuration (the URL must be https) and builds the
// HTTP client.
func NewFactory(cfg Config) (*Factory, error) {
	cfg.setDefaults()
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("easupstream: bad URL: %w", err)
	}
	if u.Scheme != "https" || u.Host == "" {
		// Basic credentials would travel in clear text.
		return nil, errors.New("easupstream: the Exchange URL must be https://")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc, err = newHTTPClient(cfg)
		if err != nil {
			return nil, err
		}
	}
	// Every client, a supplied one included, is given the HTML check.
	guarded := *hc
	base := guarded.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	guarded.Transport = htmlGuard{base}
	return &Factory{cfg: cfg, http: &guarded}, nil
}

func newHTTPClient(cfg Config) (*http.Client, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("easupstream: read CA file: %w", err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("easupstream: the CA file contains no certificates")
		}
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   30 * time.Second,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 0, // a Ping is held open for the whole heartbeat
		ForceAttemptHTTP2:     false,
	}
	// The HTTP timeout must exceed the longest held Ping by more than a minute
	// (DESIGN section 12).
	return &http.Client{Transport: tr, Timeout: cfg.MaxHeartbeat + 2*time.Minute}, nil
}

// errHTMLLogon is what htmlGuard returns for an HTML page that answers an EAS request.
var errHTMLLogon = errors.New("easupstream: an HTML page answered an EAS request")

// htmlGuard turns a 2xx HTML answer to a POST (every EAS command is one) into an error. Exchange answers
// EAS requests with WBXML or nothing; an HTML body is the logon page of a gateway that has not let the
// request through, and it must count as a refused credential, not as a decoding problem.
type htmlGuard struct{ http.RoundTripper }

func (g htmlGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := g.RoundTripper.RoundTrip(req)
	if err != nil || req.Method != http.MethodPost || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, err
	}
	if ct := strings.ToLower(resp.Header.Get("Content-Type")); strings.Contains(ct, "html") {
		resp.Body.Close()
		return nil, errHTMLLogon
	}
	return resp, nil
}

// maxRedirects is how many redirects of one request are followed.
const maxRedirects = 5

// redirectPolicy is the CheckRedirect of every Exchange session. The credentials
// travel with every request (HTTP Basic), so a redirect must not take them to a
// plain connection or to another host: Go's client strips the Authorization
// header only when the host changes, and a redirect to http:// on the same host
// would send it in clear text. Only redirects that stay on https and on the host
// (and port) of the original request are followed, a few of them; for anything
// else the redirect response itself is returned and the caller sees an HTTP
// 3xx. The log line carries the status and nothing of the Location, which can
// hold a session token or a user name.
func redirectPolicy(log *slog.Logger) func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		status := 0
		if req.Response != nil {
			status = req.Response.StatusCode
		}
		switch {
		case req.URL.Scheme != "https":
			log.Warn("Exchange answered with a redirect to a connection that is not https; not followed", "status", status)
		case len(via) == 0 || hostPort(req.URL) != hostPort(via[0].URL):
			log.Warn("Exchange answered with a redirect to another host; not followed", "status", status)
		case len(via) > maxRedirects:
			log.Warn("Exchange answered with too many redirects; not followed", "status", status)
		default:
			return nil
		}
		return http.ErrUseLastResponse
	}
}

// hostPort is the lower-cased host and the port (443 if none) of u.
func hostPort(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// New implements upstream.Factory. Nothing is sent until Open.
func (f *Factory) New(c upstream.Config) (upstream.Mailbox, error) {
	return newMailbox(f.cfg, f.http, c)
}

// HTTPTimeout returns the HTTP client timeout (tests check it exceeds the
// longest Ping heartbeat).
func (f *Factory) HTTPTimeout() time.Duration { return f.http.Timeout }
