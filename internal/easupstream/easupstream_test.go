package easupstream_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hstern/go-activesync/wbxml"

	"easbridge/internal/easupstream"
	"easbridge/internal/logx"
	"easbridge/internal/upstream"
)

type fixture struct {
	t    *testing.T
	fake *fakeEAS
	fac  *easupstream.Factory
	logs *bytes.Buffer
}

func newFixture(t *testing.T, mod ...func(*easupstream.Config)) *fixture {
	t.Helper()
	logx.ResetSecretsForTest()
	f := &fixture{t: t, fake: newFake(t), logs: &bytes.Buffer{}}
	cfg := easupstream.Config{
		URL: f.fake.url(), HTTPClient: f.fake.srv.Client(), Version: "16.1", DeviceType: "easbridge", UserAgent: "easbridge/test",
		MIMEInlineMax: 4096, WindowSize: 20, MaxHeartbeat: 480 * time.Second,
		Logger: logx.New(logx.Options{Level: slog.LevelDebug, Writer: &syncWriter{w: f.logs}}),
	}
	for _, m := range mod {
		m(&cfg)
	}
	fac, err := easupstream.NewFactory(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.fac = fac
	return f
}

type syncWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func (f *fixture) mailbox(policy string) *easupstream.Mailbox {
	f.t.Helper()
	m, err := f.fac.New(upstream.Config{User: user, Password: pass, DeviceID: dev, PolicyKey: policy})
	if err != nil {
		f.t.Fatal(err)
	}
	return m.(*easupstream.Mailbox)
}

func (f *fixture) open(policy string) *easupstream.Mailbox {
	f.t.Helper()
	m := f.mailbox(policy)
	if err := m.Open(context.Background()); err != nil {
		f.t.Fatalf("Open: %v", err)
	}
	return m
}

var ctx = context.Background()

func TestFactoryRejectsHTTP(t *testing.T) {
	for _, u := range []string{"http://mail.example.test/Microsoft-Server-ActiveSync", "ftp://x", "mail.example.test/x", ""} {
		if _, err := easupstream.NewFactory(easupstream.Config{URL: u}); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
	if _, err := easupstream.NewFactory(easupstream.Config{URL: "https://mail.example.test/Microsoft-Server-ActiveSync"}); err != nil {
		t.Errorf("https rejected: %v", err)
	}
}

func TestHTTPClientTimeoutExceedsTheHeartbeat(t *testing.T) {
	// DESIGN section 12: HTTP timeout > heartbeat + 60 s. Checked on the client
	// the factory builds itself (no override).
	fac, err := easupstream.NewFactory(easupstream.Config{URL: "https://mail.example.test/x", MaxHeartbeat: 480 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if to := fac.HTTPTimeout(); to <= 540*time.Second {
		t.Errorf("HTTP timeout %v does not exceed heartbeat + 60 s", to)
	}
}

func TestEachMailboxKeepsItsOwnSessionCookies(t *testing.T) {
	// A gateway such as an access-policy gateway issues a session cookie; dropping it would make
	// the gateway open a new session for every request. Cookies must also never
	// leak from one mailbox to another.
	const cookie = "MRHSession=gateway-session"
	f := newFixture(t)
	f.fake.cookie = cookie
	f.open("")
	first := f.fake.all()
	if len(first) < 2 || first[0].Cookie != "" {
		t.Fatalf("first mailbox: %d requests, first cookie %q (want a cookie-less first request)", len(first), first[0].Cookie)
	}
	for i, r := range first[1:] {
		if r.Cookie != cookie {
			t.Errorf("request %d (%s) carried cookie %q, want %q", i+1, r.Cmd, r.Cookie, cookie)
		}
	}
	f.open("")
	second := f.fake.all()[len(first):]
	if len(second) < 2 {
		t.Fatalf("second mailbox sent %d requests", len(second))
	}
	if second[0].Cookie != "" {
		t.Errorf("a new mailbox started with another mailbox's cookie: %q", second[0].Cookie)
	}
	if second[1].Cookie != cookie {
		t.Errorf("the second mailbox did not keep its own cookie: %q", second[1].Cookie)
	}
}

func TestOpenNegotiatesOnceAndProvisions(t *testing.T) {
	f := newFixture(t)
	m := f.open("")
	if m.Version() != "16.1" {
		t.Fatalf("version = %q", m.Version())
	}
	if n := len(f.fake.requests("")); n != 1 { // the single OPTIONS has no Cmd
		t.Errorf("%d OPTIONS requests, want exactly 1", n)
	}
	if f.fake.count("Provision") != 2 {
		t.Errorf("Provision round trips = %d, want 2", f.fake.count("Provision"))
	}
	for _, r := range f.fake.requests("Provision") {
		if r.Version != "16.1" {
			t.Errorf("Provision sent with protocol %q", r.Version)
		}
	}
	if m.PolicyKey() != "FINALKEY" {
		t.Errorf("policy key = %q", m.PolicyKey())
	}
	// Honest identification in the query string.
	if q := f.fake.last("Provision").Query; !strings.Contains(q, "DeviceType=easbridge") || !strings.Contains(q, "DeviceId="+dev) {
		t.Errorf("query = %s", q)
	}
	// The device is described inside the Provision request (TestDeviceInformation...),
	// not by a Settings command of its own: nothing but OPTIONS and the two phases.
	if f.fake.count("Settings") != 0 {
		t.Errorf("Settings requests = %d, want none", f.fake.count("Settings"))
	}
	if n := len(f.fake.all()); n != 3 {
		t.Errorf("%d requests for a successful Open, want 3 (OPTIONS and the two Provision phases)", n)
	}
}

func TestOpenFallsBackToVersion14_1(t *testing.T) {
	f := newFixture(t)
	f.fake.versions = "2.5,12.0,12.1,14.0,14.1" // no 16.x
	m := f.open("")
	if m.Version() != "14.1" {
		t.Fatalf("version = %q, want 14.1", m.Version())
	}
	if f.fake.last("Provision").Version != "14.1" {
		t.Errorf("requests did not use 14.1")
	}
	// 14.1 preferred explicitly.
	f2 := newFixture(t, func(c *easupstream.Config) { c.Version = "14.1" })
	if v := f2.open("").Version(); v != "14.1" {
		t.Errorf("version = %q", v)
	}
	// Nothing in common: a permanent, explicit error.
	f3 := newFixture(t)
	f3.fake.versions = "2.5,12.0,12.1"
	err := f3.mailbox("").Open(ctx)
	var se *upstream.StatusError
	if !errors.As(err, &se) || !se.Perm {
		t.Errorf("err = %v", err)
	}
}

func TestAccountOnlyRemoteWipeOfEAS161(t *testing.T) {
	// The order arrives in the Provision response ...
	f := newFixture(t)
	f.fake.on("Provision", always(0, e(wbxml.PageProvision, "Provision",
		e(wbxml.PageProvision, "Status", txt("1")), e(wbxml.PageProvision, "AccountOnlyRemoteWipe"))))
	m := f.mailbox("")
	err := m.Open(ctx)
	if !errors.Is(err, upstream.ErrAccountOnlyWipe) {
		t.Fatalf("Open: %v", err)
	}
	if !errors.Is(err, upstream.ErrRemoteWipe) {
		t.Errorf("an account-only wipe is a wipe for code that does not care which: %v", err)
	}
	// ... and is answered with its own element, not with RemoteWipe.
	f.fake.on("Provision", always(0, e(wbxml.PageProvision, "Provision", e(wbxml.PageProvision, "Status", txt("1")))))
	if err := m.AcknowledgeWipe(ctx, true); err != nil {
		t.Fatalf("AcknowledgeWipe: %v", err)
	}
	ack := f.fake.last("Provision").Root
	if st, _ := text(ack.Find("AccountOnlyRemoteWipe"), "Status"); st != "1" || ack.Find("RemoteWipe") != nil {
		t.Errorf("account-only acknowledgement = %v", ack)
	}
	// A full wipe stays a full wipe.
	f = newFixture(t)
	f.fake.on("Provision", always(0, e(wbxml.PageProvision, "Provision",
		e(wbxml.PageProvision, "Status", txt("1")), e(wbxml.PageProvision, "RemoteWipe"))))
	if err := f.mailbox("").Open(ctx); !errors.Is(err, upstream.ErrRemoteWipe) || errors.Is(err, upstream.ErrAccountOnlyWipe) {
		t.Errorf("full wipe: %v", err)
	}
	// Through the 449 re-provision path of any other command.
	f = newFixture(t)
	m = f.open("")
	f.fake.on("Provision", always(0, e(wbxml.PageProvision, "Provision",
		e(wbxml.PageProvision, "Status", txt("1")), e(wbxml.PageProvision, "AccountOnlyRemoteWipe"))))
	f.fake.on("Sync", always(449, nil))
	if _, err := m.Sync(ctx, "5", "k1", upstream.SyncOptions{}); !errors.Is(err, upstream.ErrAccountOnlyWipe) {
		t.Errorf("account-only wipe through 449: %v", err)
	}
}

func TestOpenAuthAndWipeAndNoPolicy(t *testing.T) {
	// 401 on OPTIONS.
	f := newFixture(t)
	f.fake.status = http.StatusUnauthorized
	if err := f.mailbox("").Open(ctx); !errors.Is(err, upstream.ErrAuthFailed) {
		t.Errorf("OPTIONS 401: %v", err)
	}
	// 401 on Provision (OPTIONS passes: some proxies do not check it).
	f = newFixture(t)
	f.fake.on("Provision", always(http.StatusUnauthorized, nil))
	f.fake.on("Settings", always(http.StatusUnauthorized, nil))
	if err := f.mailbox("").Open(ctx); !errors.Is(err, upstream.ErrAuthFailed) {
		t.Errorf("Provision 401: %v", err)
	}
	// Remote wipe order in the Provision response, acknowledged afterwards even
	// though Open failed (that is exactly the flow the manager runs).
	f = newFixture(t)
	f.fake.on("Provision", always(0, e(wbxml.PageProvision, "Provision",
		e(wbxml.PageProvision, "Status", txt("1")), e(wbxml.PageProvision, "RemoteWipe"))))
	m := f.mailbox("")
	if err := m.Open(ctx); !errors.Is(err, upstream.ErrRemoteWipe) {
		t.Fatalf("wipe: %v", err)
	}
	f.fake.on("Provision", always(0, e(wbxml.PageProvision, "Provision", e(wbxml.PageProvision, "Status", txt("1")))))
	if err := m.AcknowledgeWipe(ctx, false); err != nil {
		t.Fatalf("AcknowledgeWipe: %v", err)
	}
	ack := f.fake.last("Provision").Root
	if st, _ := text(ack.Find("RemoteWipe"), "Status"); st != "1" || ack.Find("AccountOnlyRemoteWipe") != nil {
		t.Errorf("wipe acknowledgement = %v", ack)
	}
	// Before any Open there is nothing to acknowledge on.
	if err := f.mailbox("").AcknowledgeWipe(ctx, false); err == nil {
		t.Errorf("acknowledging on a never-opened mailbox succeeded")
	}
	// Policy status 2: nothing to provision.
	f = newFixture(t)
	f.fake.on("Provision", always(0, e(wbxml.PageProvision, "Provision",
		e(wbxml.PageProvision, "Status", txt("1")),
		e(wbxml.PageProvision, "Policies", e(wbxml.PageProvision, "Policy",
			e(wbxml.PageProvision, "PolicyType", txt("MS-EAS-Provisioning-WBXML")), e(wbxml.PageProvision, "Status", txt("2")))))))
	m = f.open("OLDKEY")
	if m.PolicyKey() != "" || f.fake.count("Provision") != 1 {
		t.Errorf("no-policy handling: key %q after %d round trips", m.PolicyKey(), f.fake.count("Provision"))
	}
}

// Protocol 14.0 must not carry the device information in Provision (MS-ASPROV
// 2.2.2.53); a Settings command after provisioning is how it gets there.
func TestDeviceInformationOnProtocol14_0GoesInASettingsCommandAfterProvisioning(t *testing.T) {
	f := newFixture(t, func(c *easupstream.Config) { c.Version = "14.0" })
	f.fake.versions = "2.5,12.0,12.1,14.0"
	m := f.open("")
	if m.Version() != "14.0" {
		t.Fatalf("version %q", m.Version())
	}
	var seq []string
	for _, r := range f.fake.all() {
		seq = append(seq, r.Method+" "+r.Cmd)
	}
	if got, want := strings.Join(seq, ","), "OPTIONS ,POST Provision,POST Provision,POST Settings"; got != want {
		t.Errorf("requests = %s, want %s", got, want)
	}
	for i, r := range f.fake.requests("Provision") {
		if r.Root.Find("DeviceInformation") != nil {
			t.Errorf("Provision request %d carries device information on protocol 14.0", i+1)
		}
	}
	di := f.fake.last("Settings").Root.Find("DeviceInformation")
	if di == nil {
		t.Fatalf("the Settings command has no DeviceInformation")
	}
	if v, _ := text(di, "Model"); v != "easbridge" {
		t.Errorf("Model = %q", v)
	}
	// Only once per device: with a policy key on file the device is known.
	f = newFixture(t, func(c *easupstream.Config) { c.Version = "14.0" })
	f.fake.versions = "2.5,12.0,12.1,14.0"
	f.open("HAVEKEY")
	if f.fake.count("Settings") != 0 {
		t.Errorf("Settings sent although a policy key exists")
	}
	// A refusal does not fail Open; a rejected login does.
	f = newFixture(t, func(c *easupstream.Config) { c.Version = "14.0" })
	f.fake.versions = "2.5,12.0,12.1,14.0"
	f.fake.on("Settings", always(0, e(wbxml.PageSettings, "Settings", e(wbxml.PageSettings, "Status", txt("2")))))
	if err := f.mailbox("").Open(ctx); err != nil {
		t.Errorf("a refused DeviceInformation must not fail Open: %v", err)
	}
	f.fake.on("Settings", always(http.StatusUnauthorized, nil))
	if err := f.mailbox("").Open(ctx); !errors.Is(err, upstream.ErrAuthFailed) {
		t.Errorf("a 401 on the Settings command: %v", err)
	}
}

func TestFolderSyncStatuses(t *testing.T) {
	f := newFixture(t)
	m := f.open("")
	calls := 0
	f.fake.on("FolderSync", func(r *http.Request, in *wbxml.Element) (int, *wbxml.Element) {
		calls++
		key, _ := text(in, "SyncKey")
		switch key {
		case "0":
			return 0, folderSyncResp("h1", folderAdd("5", "0", "Inbox", 2), folderAdd("6", "0", "Sent Items", 5), folderAdd("9", "5", "Sub", 12))
		case "h1":
			return 0, folderSyncResp("h2",
				e(wbxml.PageFolderHierarchy, "Delete", e(wbxml.PageFolderHierarchy, "ServerId", txt("9"))))
		}
		return 0, e(wbxml.PageFolderHierarchy, "FolderSync", e(wbxml.PageFolderHierarchy, "Status", txt("9")))
	})
	d, err := m.FolderSync(ctx, "0")
	if err != nil || !d.Full || d.NewKey != "h1" || len(d.Added) != 3 || d.Added[0].Type != 2 || d.Added[2].ParentID != "5" || d.Added[1].DisplayName != "Sent Items" {
		t.Fatalf("initial: %+v %v", d, err)
	}
	d, err = m.FolderSync(ctx, "h1")
	if err != nil || d.Full || d.NewKey != "h2" || len(d.Deleted) != 1 || d.Deleted[0] != "9" {
		t.Fatalf("delta: %+v %v", d, err)
	}
	// Status 9 (the key is unknown): reset to "0" and retry once; the result is a full listing.
	before := calls
	d, err = m.FolderSync(ctx, "stale-key")
	if err != nil || !d.Full || d.NewKey != "h1" || calls-before != 2 {
		t.Fatalf("after status 9: %+v %v (calls %d)", d, err, calls-before)
	}
	if k, _ := text(f.fake.requests("FolderSync")[calls-1].Root, "SyncKey"); k != "0" {
		t.Errorf("the retry did not restart from key 0 (%q)", k)
	}
	// Status 3 is recovered by the library; the delta is marked Full.
	f.fake.on("FolderSync", func(r *http.Request, in *wbxml.Element) (int, *wbxml.Element) {
		key, _ := text(in, "SyncKey")
		if key != "0" {
			return 0, e(wbxml.PageFolderHierarchy, "FolderSync", e(wbxml.PageFolderHierarchy, "Status", txt("3")))
		}
		return 0, folderSyncResp("h7", folderAdd("5", "0", "Inbox", 2))
	})
	d, err = m.FolderSync(ctx, "bad")
	if err != nil || !d.Full || d.NewKey != "h7" {
		t.Fatalf("after status 3: %+v %v", d, err)
	}
}

func mime(id string) string {
	return "Message-ID: <" + id + ">\r\nFrom: b@example.test\r\nTo: alice@example.test\r\nSubject: s\r\n\r\nbody " + id + "\r\n"
}

func TestSyncBootstrapItemsAndBodyHandling(t *testing.T) {
	f := newFixture(t, func(c *easupstream.Config) { c.Filter = 0; c.WindowSize = 7 })
	m := f.open("")
	big := mime("big@x") // pretend it was cut
	f.fake.on("Sync", func(r *http.Request, in *wbxml.Element) (int, *wbxml.Element) {
		key, _ := text(in, "SyncKey")
		switch key {
		case "0":
			return 0, syncResp("5", "k1", "1")
		case "k1":
			return 0, syncResp("5", "k2", "1", commands(
				addItem("5:1", mime("full@x"), true, 2, "2026-10-01T10:00:00.000Z", false, len(mime("full@x"))),
				addItem("5:2", big[:20], false, 0, "2026-10-02T10:00:00.000Z", true, 9000),
				addItem("5:3", big[:20], false, 0, "2026-10-03T10:00:00.000Z", false, 9000), // cut without saying so
				e(wbxml.PageAirSync, "Change", e(wbxml.PageAirSync, "ServerId", txt("5:9")),
					e(wbxml.PageAirSync, "ApplicationData", e(wbxml.PageEmail, "Read", txt("1")))),
				e(wbxml.PageAirSync, "Delete", e(wbxml.PageAirSync, "ServerId", txt("5:8"))),
			), e(wbxml.PageAirSync, "MoreAvailable"))
		}
		return 0, syncResp("5", key, "3")
	})
	// 1. bootstrap: one request, a key, no items.
	d, err := m.Sync(ctx, "5", "0", upstream.SyncOptions{})
	if err != nil || d.NewKey != "k1" || len(d.Added)+len(d.Changed)+len(d.Deleted) != 0 || d.Reset || d.MoreAvailable {
		t.Fatalf("bootstrap: %+v %v", d, err)
	}
	if f.fake.count("Sync") != 1 {
		t.Fatalf("bootstrap made %d requests, want exactly 1 (NoBootstrap)", f.fake.count("Sync"))
	}
	// 2. data call.
	d, err = m.Sync(ctx, "5", "k1", upstream.SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if d.NewKey != "k2" || !d.MoreAvailable || len(d.Added) != 3 || len(d.Changed) != 1 || len(d.Deleted) != 1 || d.Deleted[0] != "5:8" {
		t.Fatalf("delta: %+v", d)
	}
	a := d.Added
	if !a[0].MIMEComplete || !a[0].Read || !a[0].ReadSet || !a[0].Flagged || !a[0].FlagSet || a[0].ServerID != "5:1" ||
		!a[0].DateReceived.Equal(time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)) || string(a[0].MIME) != mime("full@x") {
		t.Errorf("complete item: %+v", a[0])
	}
	if a[1].MIMEComplete || a[1].EstimatedSize != 9000 {
		t.Errorf("truncated item must not be complete: %+v", a[1])
	}
	if a[2].MIMEComplete {
		t.Errorf("an item shorter than its announced size must not be complete: %+v", a[2])
	}
	ch := d.Changed[0]
	if ch.ServerID != "5:9" || !ch.ReadSet || !ch.Read || ch.FlagSet {
		t.Errorf("change must report only what the server sent: %+v", ch)
	}
	// Request shape: MIME, MIMESupport 2, truncation = inline max, window, no date filter.
	req := f.fake.requests("Sync")[1].Root
	if v, _ := text(req, "TruncationSize"); v != "4096" {
		t.Errorf("TruncationSize = %q", v)
	}
	if v, _ := text(req, "MIMESupport"); v != "2" {
		t.Errorf("MIMESupport = %q", v)
	}
	if v, _ := text(req, "WindowSize"); v != "7" {
		t.Errorf("WindowSize = %q", v)
	}
	if _, ok := text(req, "FilterType"); ok {
		t.Errorf("FilterType sent although the filter is 'all'")
	}
	if v, _ := text(req, "Type"); v != "4" {
		t.Errorf("BodyPreference Type = %q", v)
	}
	for _, long := range []string{"Wait", "HeartbeatInterval"} {
		if _, ok := text(req, long); ok {
			t.Errorf("Sync must not carry %s (long polls are Ping's job)", long)
		}
	}
	// The key the caller gave is the key that was sent, whatever happened before.
	if k, _ := text(req, "SyncKey"); k != "k1" {
		t.Errorf("SyncKey sent = %q", k)
	}
	// 3. status 3 -> silent reset, flagged for the engine.
	d, err = m.Sync(ctx, "5", "dead-key", upstream.SyncOptions{})
	if err != nil || !d.Reset || d.NewKey != "k1" || len(d.Added) != 0 {
		t.Fatalf("after status 3: %+v %v", d, err)
	}
}

func TestSyncDateFilterAndHeadersOnly(t *testing.T) {
	f := newFixture(t, func(c *easupstream.Config) { c.Filter = 4 })
	m := f.open("")
	f.fake.on("Sync", always(0, syncResp("5", "k2", "1")))
	if _, err := m.Sync(ctx, "5", "k1", upstream.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if v, _ := text(f.fake.last("Sync").Root, "FilterType"); v != "4" {
		t.Errorf("FilterType = %q, want 4 (two weeks)", v)
	}
	if _, err := m.Sync(ctx, "5", "k1", upstream.SyncOptions{HeadersOnly: true}); err != nil {
		t.Fatal(err)
	}
	r := f.fake.last("Sync").Root
	if v, _ := text(r, "Type"); v != "1" {
		t.Errorf("headers-only body type = %q, want 1 (plain)", v)
	}
	if v, _ := text(r, "TruncationSize"); v != "1" {
		t.Errorf("headers-only truncation = %q, want 1", v)
	}
	if _, ok := text(r, "MIMESupport"); ok {
		t.Errorf("MIMESupport sent for a headers-only sync")
	}
}

func TestSyncErrorMapping(t *testing.T) {
	f := newFixture(t)
	m := f.open("")
	try := func(h handler) error {
		f.fake.on("Sync", h)
		_, err := m.Sync(ctx, "5", "k1", upstream.SyncOptions{})
		return err
	}
	if err := try(always(0, syncResp("5", "k", "12"))); !errors.Is(err, upstream.ErrHierarchyChanged) {
		t.Errorf("status 12: %v", err)
	}
	var se *upstream.StatusError
	if err := try(always(0, syncResp("5", "k", "8"))); !errors.As(err, &se) || se.Code != 8 || !se.Perm {
		t.Errorf("status 8: %v", err)
	}
	if err := try(always(0, syncResp("5", "k", "5"))); !errors.As(err, &se) || se.Code != 5 || se.Perm {
		t.Errorf("status 5 must be temporary: %v", err)
	}
	for _, code := range []int{500, 502, 503, 504} {
		if err := try(always(code, nil)); !errors.Is(err, upstream.ErrUnavailable) {
			t.Errorf("HTTP %d: %v", code, err)
		}
	}
	if err := try(always(http.StatusUnauthorized, nil)); !errors.Is(err, upstream.ErrAuthFailed) {
		t.Errorf("HTTP 401: %v", err)
	}
	// 449 (Retry With): the library re-provisions once; a still-failing 449 is temporary.
	if err := try(always(449, nil)); !errors.Is(err, upstream.ErrUnavailable) {
		t.Errorf("HTTP 449: %v", err)
	}
	// A wipe order arriving through the 449 re-provision path.
	f.fake.on("Provision", always(0, e(wbxml.PageProvision, "Provision", e(wbxml.PageProvision, "Status", txt("1")), e(wbxml.PageProvision, "RemoteWipe"))))
	if err := try(always(449, nil)); !errors.Is(err, upstream.ErrRemoteWipe) {
		t.Errorf("wipe through 449: %v", err)
	}
	// Dropped connection.
	f.fake.on("Sync", func(w *http.Request, in *wbxml.Element) (int, *wbxml.Element) { panic(http.ErrAbortHandler) })
	if _, err := m.Sync(ctx, "5", "k1", upstream.SyncOptions{}); !errors.Is(err, upstream.ErrUnavailable) {
		t.Errorf("dropped connection: %v", err)
	}
}

func TestFetchMIMEAndConcurrencyLimit(t *testing.T) {
	f := newFixture(t)
	m := f.open("")
	var mu sync.Mutex
	release := make(chan struct{})
	started := 0
	f.fake.on("ItemOperations", func(r *http.Request, in *wbxml.Element) (int, *wbxml.Element) {
		mu.Lock()
		started++
		mu.Unlock()
		<-release
		id, _ := text(in, "ServerId")
		return 0, fetchMIMEResponse(mimeData, "5", id, mime(id), false, 0, "2026-10-01T10:00:00.000Z")
	})
	var wg sync.WaitGroup
	results := make([][]byte, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b, err := m.FetchMIME(ctx, "5", "5:"+itoa(i))
			if err != nil {
				t.Errorf("fetch %d: %v", i, err)
			}
			results[i] = b
		}(i)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := started
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // a third request would have arrived by now
	if got := f.fake.maxInflight("ItemOperations"); got != 2 {
		t.Errorf("%d ItemOperations in flight, want exactly 2 (the limit)", got)
	}
	close(release)
	wg.Wait()
	for i, b := range results {
		if string(b) != mime("5:"+itoa(i)) {
			t.Errorf("result %d = %q", i, b)
		}
	}
	if v, _ := text(f.fake.last("ItemOperations").Root, "MIMESupport"); v != "2" {
		t.Errorf("MIMESupport in Fetch = %q (patch 5)", v)
	}
	// No MIME in the answer.
	f.fake.on("ItemOperations", always(0, e(wbxml.PageItemOperations, "ItemOperations",
		e(wbxml.PageItemOperations, "Status", txt("1")),
		e(wbxml.PageItemOperations, "Response", e(wbxml.PageItemOperations, "Fetch",
			e(wbxml.PageItemOperations, "Status", txt("1")), e(wbxml.PageItemOperations, "Properties"))))))
	close2 := make(chan struct{})
	_ = close2
	if _, err := m.FetchMIME(ctx, "5", "5:1"); !errors.Is(err, upstream.ErrNoMIME) {
		t.Errorf("answer without MIME: %v", err)
	}
	// Context cancellation while waiting for a slot.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := m.FetchMIME(cctx, "5", "5:1"); err == nil {
		t.Errorf("canceled fetch succeeded")
	}
}

func TestApplyChanges(t *testing.T) {
	f := newFixture(t)
	m := f.open("")
	f.fake.on("Sync", always(0, syncResp("5", "k3", "1",
		e(wbxml.PageAirSync, "Responses",
			e(wbxml.PageAirSync, "Change", e(wbxml.PageAirSync, "ServerId", txt("5:2")), e(wbxml.PageAirSync, "Status", txt("8")))),
		commands(
			addItem("5:70", mime("late@x"), false, 0, "2026-10-04T10:00:00.000Z", false, 100),
			e(wbxml.PageAirSync, "Delete", e(wbxml.PageAirSync, "ServerId", txt("5:3")))),
		e(wbxml.PageAirSync, "MoreAvailable"))))
	yes, no := true, false
	res, err := m.ApplyChanges(ctx, "5", "k2", []upstream.Change{
		{ServerID: "5:1", Read: &yes, Flagged: &no}, {ServerID: "5:2", Read: &yes}, {ServerID: "5:4", Delete: true},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.NewKey != "k3" || len(res.Results) != 1 || res.Results[0].ServerID != "5:2" || !res.Results[0].Gone || res.Results[0].OK {
		t.Fatalf("result: %+v", res)
	}
	sc := res.ServerChanges
	if sc == nil || sc.NewKey != "k3" || !sc.MoreAvailable || len(sc.Added) != 1 || sc.Added[0].ServerID != "5:70" || len(sc.Deleted) != 1 {
		t.Fatalf("server changes were lost: %+v", sc)
	}
	r := f.fake.last("Sync").Root
	if v, _ := text(r, "GetChanges"); v != "0" {
		t.Errorf("GetChanges = %q", v)
	}
	if v, _ := text(r, "DeletesAsMoves"); v != "1" {
		t.Errorf("DeletesAsMoves = %q for a normal delete", v)
	}
	if k, _ := text(r, "SyncKey"); k != "k2" {
		t.Errorf("SyncKey sent = %q", k)
	}
	if _, err := m.ApplyChanges(ctx, "5", "k3", []upstream.Change{{ServerID: "5:9", Delete: true}}, true); err != nil {
		t.Fatal(err)
	}
	if v, _ := text(f.fake.last("Sync").Root, "DeletesAsMoves"); v != "0" {
		t.Errorf("hard delete sent DeletesAsMoves = %q", v)
	}
	// The key is wrong: the caller hears status 3 and decides (no silent reset).
	f.fake.on("Sync", always(0, syncResp("5", "x", "3")))
	var se *upstream.StatusError
	if _, err := m.ApplyChanges(ctx, "5", "zz", []upstream.Change{{ServerID: "5:9", Delete: true}}, false); !errors.As(err, &se) || se.Code != 3 {
		t.Errorf("status 3: %v", err)
	}
	if _, err := m.ApplyChanges(ctx, "5", "0", []upstream.Change{{ServerID: "5:9", Delete: true}}, false); err == nil {
		t.Errorf("key 0 accepted")
	}
}

func TestMoveItemsAndFolderOperations(t *testing.T) {
	f := newFixture(t)
	m := f.open("")
	f.fake.on("MoveItems", always(0, e(wbxml.PageMove, "MoveItems",
		e(wbxml.PageMove, "Response", e(wbxml.PageMove, "SrcMsgId", txt("5:1")), e(wbxml.PageMove, "Status", txt("3")), e(wbxml.PageMove, "DstMsgId", txt("6:11"))),
		e(wbxml.PageMove, "Response", e(wbxml.PageMove, "SrcMsgId", txt("5:2")), e(wbxml.PageMove, "Status", txt("5"))))))
	res, err := m.MoveItems(ctx, "5", "6", []string{"5:1", "5:2"})
	if err != nil || len(res) != 2 || !res[0].OK || res[0].DstServerID != "6:11" || res[1].OK || res[1].Status != 5 {
		t.Fatalf("MoveItems: %+v %v", res, err)
	}

	// Folder operations use the hierarchy key the caller passes.
	f.fake.on("FolderCreate", always(0, e(wbxml.PageFolderHierarchy, "FolderCreate",
		e(wbxml.PageFolderHierarchy, "Status", txt("1")), e(wbxml.PageFolderHierarchy, "SyncKey", txt("h9")),
		e(wbxml.PageFolderHierarchy, "ServerId", txt("77")))))
	r, err := m.CreateFolder(ctx, "h8", "0", "Snoozed")
	if err != nil || r.ServerID != "77" || r.NewKey != "h9" {
		t.Fatalf("CreateFolder: %+v %v", r, err)
	}
	req := f.fake.last("FolderCreate").Root
	if k, _ := text(req, "SyncKey"); k != "h8" {
		t.Errorf("hierarchy key sent = %q", k)
	}
	if n, _ := text(req, "DisplayName"); n != "Snoozed" {
		t.Errorf("name sent = %q", n)
	}
	if ty, _ := text(req, "Type"); ty != "12" {
		t.Errorf("type sent = %q, want 12 (user mail folder)", ty)
	}
	f.fake.on("FolderUpdate", always(0, e(wbxml.PageFolderHierarchy, "FolderUpdate",
		e(wbxml.PageFolderHierarchy, "Status", txt("1")), e(wbxml.PageFolderHierarchy, "SyncKey", txt("h10")))))
	r, err = m.RenameFolder(ctx, "h9", "77", "0", "Later")
	if err != nil || r.NewKey != "h10" || r.ServerID != "77" {
		t.Fatalf("RenameFolder: %+v %v", r, err)
	}
	f.fake.on("FolderDelete", always(0, e(wbxml.PageFolderHierarchy, "FolderDelete",
		e(wbxml.PageFolderHierarchy, "Status", txt("1")), e(wbxml.PageFolderHierarchy, "SyncKey", txt("h11")))))
	if r, err = m.DeleteFolder(ctx, "h10", "77"); err != nil || r.NewKey != "h11" {
		t.Fatalf("DeleteFolder: %+v %v", r, err)
	}
	// Status 9 on the hierarchy key reaches the engine as a StatusError.
	f.fake.on("FolderCreate", always(0, e(wbxml.PageFolderHierarchy, "FolderCreate", e(wbxml.PageFolderHierarchy, "Status", txt("9")))))
	var se *upstream.StatusError
	if _, err := m.CreateFolder(ctx, "stale", "0", "X"); !errors.As(err, &se) || se.Code != 9 {
		t.Errorf("status 9: %v", err)
	}
	f.fake.on("FolderCreate", always(0, e(wbxml.PageFolderHierarchy, "FolderCreate", e(wbxml.PageFolderHierarchy, "Status", txt("2")))))
	if _, err := m.CreateFolder(ctx, "h", "0", "Exists"); !errors.As(err, &se) || se.Code != 2 {
		t.Errorf("status 2: %v", err)
	}
}

func TestSendMail(t *testing.T) {
	f := newFixture(t)
	m := f.open("")
	f.fake.on("SendMail", always(0, nil)) // an empty 200 is success
	msg := []byte(mime("send@x"))
	if err := m.SendMail(ctx, msg, true); err != nil {
		t.Fatal(err)
	}
	req := f.fake.last("SendMail").Root
	if req.Find("SaveInSentItems") == nil {
		t.Errorf("SaveInSentItems missing")
	}
	if mimeEl := req.Find("Mime"); mimeEl == nil || len(mimeEl.Children) != 1 {
		t.Errorf("Mime element missing")
	} else if op, ok := mimeEl.Children[0].(wbxml.Opaque); !ok || !bytes.Equal(op, msg) {
		t.Errorf("MIME was altered on the way: %v", mimeEl.Children[0])
	}
	if err := m.SendMail(ctx, msg, false); err != nil {
		t.Fatal(err)
	}
	if f.fake.last("SendMail").Root.Find("SaveInSentItems") != nil {
		t.Errorf("SaveInSentItems sent although not requested")
	}
	cases := []struct {
		name   string
		h      handler
		target error
		code   int
	}{
		{"status 119", always(0, e(wbxml.PageComposeMail, "SendMail", e(wbxml.PageComposeMail, "Status", txt("119")))), nil, 119},
		{"status 111", always(0, e(wbxml.PageComposeMail, "SendMail", e(wbxml.PageComposeMail, "Status", txt("111")))), nil, 111},
		{"413", always(413, nil), upstream.ErrTooLarge, 0},
		{"503", always(503, nil), upstream.ErrUnavailable, 0},
		{"401", always(401, nil), upstream.ErrAuthFailed, 0},
	}
	for _, tc := range cases {
		f.fake.on("SendMail", tc.h)
		err := m.SendMail(ctx, msg, true)
		var se *upstream.StatusError
		switch {
		case tc.code != 0:
			if !errors.As(err, &se) || se.Code != tc.code || se.Op != "SendMail" {
				t.Errorf("%s: %v", tc.name, err)
			}
			if tc.code == 111 && se.Perm {
				t.Errorf("%s must be temporary", tc.name)
			}
			if tc.code == 119 && !se.Perm {
				t.Errorf("%s must be permanent", tc.name)
			}
		case !errors.Is(err, tc.target):
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

// sentClientID sends mime and returns the ClientId that went on the wire.
func sentClientID(t *testing.T, f *fixture, m upstream.Mailbox, mimeBytes string) string {
	t.Helper()
	if err := m.SendMail(ctx, []byte(mimeBytes), true); err != nil {
		t.Fatal(err)
	}
	id, ok := text(f.fake.last("SendMail").Root, "ClientId")
	if !ok {
		t.Fatal("no ClientId in SendMail")
	}
	return id
}

func TestSendMailClientIDIsTheSameForARetriedSubmission(t *testing.T) {
	// A random ClientId made every retry of a submission (a timeout, a dropped
	// connection) a new message. It is derived from the Message-ID now, so
	// Exchange recognises the repeat (MS-ASCMD: the server uses the ClientId to
	// identify duplicate messages).
	f := newFixture(t)
	m := f.open("")
	f.fake.on("SendMail", always(0, nil))
	a := sentClientID(t, f, m, mime("retry@x"))
	if b := sentClientID(t, f, m, mime("retry@x")); a != b {
		t.Errorf("the same message got two ClientIds: %s and %s", a, b)
	}
	// The same Message-ID with another Date header, as a client rebuilding the
	// message for a retry would produce: still the same submission.
	rebuilt := "Date: Fri, 09 Oct 2026 12:00:05 +0000\r\n" + mime("retry@x")
	if b := sentClientID(t, f, m, rebuilt); a != b {
		t.Errorf("a rebuilt message with the same Message-ID got another ClientId: %s and %s", a, b)
	}
	// Another message is another submission, and angle brackets do not matter.
	if b := sentClientID(t, f, m, mime("other@x")); a == b {
		t.Errorf("two messages share the ClientId %s", a)
	}
	if b := sentClientID(t, f, m, strings.Replace(mime("retry@x"), "<retry@x>", "  <retry@x>  ", 1)); a != b {
		t.Errorf("spacing around the Message-ID changed the ClientId: %s and %s", a, b)
	}
	// Within the 40 characters EAS allows, hex, and nothing of the message in it.
	if len(a) != 32 || strings.Trim(a, "0123456789abcdef") != "" || strings.Contains(a, "retry") {
		t.Errorf("ClientId = %q", a)
	}
	// Without a Message-ID the whole message decides.
	noID := "From: b@example.test\r\nTo: alice@example.test\r\nSubject: s\r\n\r\nbody one\r\n"
	x := sentClientID(t, f, m, noID)
	if y := sentClientID(t, f, m, noID); x != y {
		t.Errorf("an identical message without Message-ID got two ClientIds: %s and %s", x, y)
	}
	if y := sentClientID(t, f, m, strings.Replace(noID, "one", "two", 1)); x == y {
		t.Errorf("different messages without Message-ID share the ClientId %s", x)
	}
}

func TestSendMailPreviouslySentIsSuccess(t *testing.T) {
	// Exchange answers a ClientId it has seen with status 118. The earlier attempt
	// delivered the message, so the retry has nothing left to do.
	f := newFixture(t)
	m := f.open("")
	f.fake.on("SendMail", always(0, e(wbxml.PageComposeMail, "SendMail", e(wbxml.PageComposeMail, "Status", txt("118")))))
	if err := m.SendMail(ctx, []byte(mime("again@x")), true); err != nil {
		t.Fatalf("status 118 must count as sent: %v", err)
	}
	// Any other refusal still is one.
	f.fake.on("SendMail", always(0, e(wbxml.PageComposeMail, "SendMail", e(wbxml.PageComposeMail, "Status", txt("120")))))
	var se *upstream.StatusError
	if err := m.SendMail(ctx, []byte(mime("again@x")), true); !errors.As(err, &se) || se.Code != 120 {
		t.Fatalf("status 120: %v", err)
	}
}

func TestPingStatusesAreReadEvenWhenTheLibraryReturnsAnError(t *testing.T) {
	f := newFixture(t)
	m := f.open("")
	for _, tc := range []struct {
		resp   *wbxml.Element
		status int
		hb     time.Duration
		ids    []string
	}{
		{pingResp("1"), 1, 0, nil},
		{pingResp("2", "5", "9"), 2, 0, []string{"5", "9"}},
		{e(wbxml.PagePing, "Ping", e(wbxml.PagePing, "Status", txt("5")), e(wbxml.PagePing, "HeartbeatInterval", txt("300"))), 5, 300 * time.Second, nil},
		{pingResp("6"), 6, 0, nil},
		{pingResp("7"), 7, 0, nil},
		{pingResp("8"), 8, 0, nil},
	} {
		f.fake.on("Ping", always(0, tc.resp))
		res, err := m.Ping(ctx, 480*time.Second, []string{"5", "9"})
		if err != nil || res == nil || res.Status != tc.status || res.Heartbeat != tc.hb || strings.Join(res.Changed, ",") != strings.Join(tc.ids, ",") {
			t.Errorf("status %d: %+v %v", tc.status, res, err)
		}
	}
	req := f.fake.last("Ping").Root
	if hb, _ := text(req, "HeartbeatInterval"); hb != "480" {
		t.Errorf("HeartbeatInterval = %q", hb)
	}
	if n := len(req.FindAll("Folder")); n != 2 {
		t.Errorf("%d folders in the Ping request", n)
	}
	if c, _ := text(req, "Class"); c != "Email" {
		t.Errorf("Class = %q", c)
	}
	// Transport trouble is an error without a status.
	f.fake.on("Ping", always(504, nil))
	if res, err := m.Ping(ctx, time.Minute, []string{"5"}); res != nil || !errors.Is(err, upstream.ErrUnavailable) {
		t.Errorf("504: %+v %v", res, err)
	}
	f.fake.on("Ping", always(401, nil))
	if _, err := m.Ping(ctx, time.Minute, []string{"5"}); !errors.Is(err, upstream.ErrAuthFailed) {
		t.Errorf("401: %v", err)
	}
}

func TestLoggingNeverShowsTheMailboxNameOrPassword(t *testing.T) {
	f := newFixture(t)
	m := f.open("")
	f.fake.on("FolderSync", always(0, folderSyncResp("h1", folderAdd("5", "0", "Inbox", 2))))
	m.FolderSync(ctx, "0")
	f.fake.on("Sync", always(http.StatusUnauthorized, nil))
	m.Sync(ctx, "5", "k1", upstream.SyncOptions{})
	logs := f.logs.String()
	if !strings.Contains(logs, "eas request") {
		t.Fatalf("no debug output captured:\n%s", logs)
	}
	for _, bad := range []string{"alice@example.test", "alice%40example.test", pass, "YWxpY2U"} {
		if strings.Contains(logs, bad) {
			t.Errorf("log contains %q:\n%s", bad, logs)
		}
	}
	if !strings.Contains(logs, "DeviceId=") {
		t.Errorf("device id missing from the request log")
	}
}

func TestProbePrintsOnlyStatusesCountsSizesAndTimings(t *testing.T) {
	f := newFixture(t)
	secretSubject := "TOP SECRET SUBJECT"
	f.fake.on("FolderSync", always(0, folderSyncResp("h1",
		folderAdd("5", "0", "Inbox", 2), folderAdd("6", "0", "Sent Items", 5), folderAdd("7", "0", "Divorce lawyer", 12))))
	f.fake.on("Sync", func(r *http.Request, in *wbxml.Element) (int, *wbxml.Element) {
		if key, _ := text(in, "SyncKey"); key == "0" {
			return 0, syncResp("5", "k1", "1")
		}
		return 0, syncResp("5", "k2", "1", commands(
			addItem("5:7771", "stub", true, 0, "2026-10-01T10:00:00.000Z", true, 51234),
			addItem("5:7772", "stub", false, 0, "2026-10-02T10:00:00.000Z", true, 700)), e(wbxml.PageAirSync, "MoreAvailable"))
	})
	f.fake.on("ItemOperations", always(0, fetchMIMEResponse(mimeData, "5", "5:7771",
		"Subject: "+secretSubject+"\r\nFrom: secret@example.test\r\n\r\nbody", true, 0, "2026-10-01T10:00:00.000Z")))
	f.fake.on("Ping", always(0, pingResp("1")))
	var out bytes.Buffer
	cfg := f.fac // Probe builds its own factory from the same config
	_ = cfg
	err := easupstream.Probe(ctx, easupstream.ProbeConfig{
		Config: easupstream.Config{URL: f.fake.url(), HTTPClient: f.fake.srv.Client(), Version: "16.1", UserAgent: "easbridge/probe-test"},
		User:   user, Password: pass, DeviceID: dev, PingSeconds: 1, Out: &out, SkipContacts: true,
	})
	if err != nil {
		t.Fatalf("probe: %v\n%s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{"protocol=16.1", "folders=3 by_type=2:1,5:1,12:1", "items=2 unread=1 more=true", "est_sizes_bytes=[ 51234 700 ]", "bytes=", "status=1", "probe finished"} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{secretSubject, "secret@example.test", "Divorce", "Inbox", "Sent Items", user, pass, "5:777"} {
		if strings.Contains(got, bad) {
			t.Errorf("report leaks %q:\n%s", bad, got)
		}
	}
	// Headers only, five items.
	if w, _ := text(f.fake.requests("Sync")[1].Root, "WindowSize"); w != "5" {
		t.Errorf("window = %q", w)
	}
	// A failing step is reported without identifying data and stops the probe.
	f.fake.on("FolderSync", always(http.StatusUnauthorized, nil))
	out.Reset()
	err = easupstream.Probe(ctx, easupstream.ProbeConfig{
		Config: easupstream.Config{URL: f.fake.url(), HTTPClient: f.fake.srv.Client()},
		User:   user, Password: pass, DeviceID: dev, Out: &out, SkipContacts: true,
	})
	if !errors.Is(err, upstream.ErrAuthFailed) || !strings.Contains(out.String(), "authentication failed") || strings.Contains(out.String(), user) {
		t.Errorf("failure report: %v\n%s", err, out.String())
	}
}

// A failure before any request leaves the machine (no connection, DNS, TLS
// handshake) means the password was not presented to anyone: the bridge must not
// count it as a failed verification. Everything the server answers, a 403 for
// example, must still count, because a gateway may answer a wrong password that way.
func TestNeverSentIsToldApartFromAnswersThatMightHaveCounted(t *testing.T) {
	// 1. Nothing listens: connection refused.
	f := newFixture(t)
	f.fake.srv.Close()
	m := f.mailbox("")
	err := m.Open(ctx)
	if !errors.Is(err, upstream.ErrNotSent) || !errors.Is(err, upstream.ErrUnavailable) {
		t.Errorf("connection refused: %v (want ErrNotSent, which is also ErrUnavailable)", err)
	}

	// 2. TLS handshake fails: the client does not trust the server's certificate.
	f2 := newFixture(t, func(c *easupstream.Config) { c.HTTPClient = nil })
	m2 := f2.mailbox("")
	if err := m2.Open(ctx); !errors.Is(err, upstream.ErrNotSent) {
		t.Errorf("untrusted certificate: %v", err)
	}

	// 3. The server answers: 403, a redirect page, a server error. Those may
	// have been verdicts on the password and are NOT "not sent".
	f3 := newFixture(t)
	m3 := f3.mailbox("")
	f3.fake.status = http.StatusForbidden
	if err := m3.Open(ctx); err == nil || errors.Is(err, upstream.ErrNotSent) || !errors.Is(err, upstream.ErrUnavailable) {
		t.Errorf("HTTP 403: %v (want ErrUnavailable but not ErrNotSent)", err)
	}
}

// Throttling answers are typed so that the engine can pause: HTTP 429 and 503
// (with the server's Retry-After) ask the client to slow down at once, other 5xx
// count when they repeat (security review of v0.1.0, runaway loops).
func TestThrottlingAnswersAreTypedAndCarryRetryAfter(t *testing.T) {
	f := newFixture(t)
	m := f.open("")
	try := func(status int, retryAfter string) error {
		f.fake.on("Sync", func(r *http.Request, in *wbxml.Element) (int, *wbxml.Element) { return status, nil })
		f.fake.retryAfter = retryAfter
		_, err := m.Sync(ctx, "5", "k1", upstream.SyncOptions{})
		return err
	}
	var te *upstream.ThrottleError
	for _, tc := range []struct {
		status   int
		header   string
		explicit bool
		after    time.Duration
	}{
		{429, "7", true, 7 * time.Second},
		{503, "120", true, 2 * time.Minute},
		{503, "", true, 0},
		{503, "not a number", true, 0},
		{503, "-5", true, 0},
		{503, "99999999", true, 24 * time.Hour},
		{500, "", false, 0},
		{502, "30", false, 30 * time.Second},
		{504, "", false, 0},
	} {
		err := try(tc.status, tc.header)
		if !errors.As(err, &te) || te.HTTP != tc.status || te.Explicit != tc.explicit || te.RetryAfter != tc.after || !errors.Is(err, upstream.ErrUnavailable) {
			t.Errorf("HTTP %d Retry-After %q: %v (%+v), want explicit=%v after=%v", tc.status, tc.header, err, te, tc.explicit, tc.after)
		}
	}
	// An HTTP date in the future is converted to a delay.
	err := try(503, time.Now().Add(90*time.Second).UTC().Format(http.TimeFormat))
	if !errors.As(err, &te) || te.RetryAfter < 80*time.Second || te.RetryAfter > 91*time.Second {
		t.Errorf("Retry-After as a date: %v", err)
	}
	// Other answers are not throttling.
	for _, status := range []int{403, 404, 408} {
		if err := try(status, ""); errors.As(err, &te) || !errors.Is(err, upstream.ErrUnavailable) {
			t.Errorf("HTTP %d: %v", status, err)
		}
	}
	// OPTIONS carries it as well.
	f2 := newFixture(t)
	f2.fake.status = http.StatusServiceUnavailable
	f2.fake.retryAfter = "45"
	if err := f2.mailbox("").Open(ctx); !errors.As(err, &te) || te.RetryAfter != 45*time.Second {
		t.Errorf("OPTIONS: %v", err)
	}
}
