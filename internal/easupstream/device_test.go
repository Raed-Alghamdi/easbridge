package easupstream_test

// Tests for the first failure against a real Exchange 2019 (v0.1.5, docs/TESTING.md,
// real-Exchange finding 1): the initial Provision request of protocol 14.1 and later
// must carry settings:DeviceInformation, or Exchange answers status 165. The model in
// fakeeas_test.go (provisionOK) now insists on it, so every test that provisions
// through it is a regression test as well; the tests here pin the details.

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hstern/go-activesync/wbxml"

	"easbridge/internal/easupstream"
	"easbridge/internal/upstream"
)

// childNames lists the element children of el in order.
func childNames(el *wbxml.Element) []string {
	var out []string
	for _, k := range el.Children {
		if c, ok := k.(*wbxml.Element); ok {
			out = append(out, c.Name)
		}
	}
	return out
}

// deviceInfo returns Provision/DeviceInformation/Set as name -> text.
func deviceInfo(t *testing.T, root *wbxml.Element) map[string]string {
	t.Helper()
	di := root.Find("DeviceInformation")
	if di == nil {
		t.Fatalf("no DeviceInformation in %v", childNames(root))
	}
	set := di.Find("Set")
	if set == nil {
		t.Fatalf("DeviceInformation has no Set")
	}
	out := map[string]string{}
	for _, k := range set.Children {
		if c, ok := k.(*wbxml.Element); ok {
			if c.Codepage != wbxml.PageSettings {
				t.Errorf("%s is on code page %d, not the Settings page", c.Name, c.Codepage)
			}
			out[c.Name] = c.TextContent()
		}
	}
	return out
}

func provStatus(code string) *wbxml.Element {
	return e(wbxml.PageProvision, "Provision", e(wbxml.PageProvision, "Status", txt(code)))
}

func policyStatus(code string) *wbxml.Element {
	return e(wbxml.PageProvision, "Provision", e(wbxml.PageProvision, "Status", txt("1")),
		e(wbxml.PageProvision, "Policies", e(wbxml.PageProvision, "Policy",
			e(wbxml.PageProvision, "PolicyType", txt("MS-EAS-Provisioning-WBXML")), e(wbxml.PageProvision, "Status", txt(code)))))
}

// The default configuration: the honest device, in the right place, and nothing else.
func TestDeviceInformationIsSentInsideTheInitialProvisionRequest(t *testing.T) {
	for _, version := range []string{"16.1", "16.0", "14.1"} {
		t.Run(version, func(t *testing.T) {
			f := newFixture(t, func(c *easupstream.Config) { c.Version = version })
			m := f.open("")
			if m.Version() != version {
				t.Fatalf("protocol %q", m.Version())
			}
			reqs := f.fake.requests("Provision")
			if len(reqs) != 2 {
				t.Fatalf("%d Provision requests", len(reqs))
			}
			first, second := reqs[0], reqs[1]

			// Where: the first child of Provision, in front of Policies.
			if got := strings.Join(childNames(first.Root), ","); got != "DeviceInformation,Policies" {
				t.Errorf("children of Provision = %s", got)
			}
			// What: the bridge as it is. Exactly these, so no IMEI, phone number or operator.
			want := map[string]string{
				"Model": "easbridge", "FriendlyName": "easbridge (MailFlow bridge)", "OS": "Linux", "OSLanguage": "en-us",
				"UserAgent": "easbridge/test", "EnableOutboundSMS": "0",
			}
			got := deviceInfo(t, first.Root)
			if len(got) != len(want) {
				t.Errorf("device information = %v", got)
			}
			for k, v := range want {
				if got[k] != v {
					t.Errorf("%s = %q, want %q", k, got[k], v)
				}
			}
			// Not on the acknowledgement (MS-ASPROV 2.2.2.53).
			if second.Root.Find("DeviceInformation") != nil {
				t.Errorf("the acknowledgement carries device information")
			}
			if second.Root.Find("PolicyKey") == nil {
				t.Errorf("the second request is not the acknowledgement")
			}
			// And no separate Settings command.
			if f.fake.count("Settings") != 0 {
				t.Errorf("a Settings command was sent")
			}
		})
	}
}

// The identity the organisation approved does not change: the device id, the
// DeviceType and the User-Agent are what they were before the device information
// existed.
func TestTheDeviceIdentityIsUnchanged(t *testing.T) {
	f := newFixture(t)
	f.open("")
	for _, r := range f.fake.all() {
		if r.Method != http.MethodPost {
			continue
		}
		if !strings.Contains(r.Query, "DeviceType=easbridge") || !strings.Contains(r.Query, "DeviceId="+dev) {
			t.Errorf("%s: query %q", r.Cmd, r.Query)
		}
		if r.Agent != "easbridge/test" {
			t.Errorf("%s: User-Agent %q", r.Cmd, r.Agent)
		}
	}
}

func TestTheDeviceSettingsAreWhatIsSent(t *testing.T) {
	f := newFixture(t, func(c *easupstream.Config) {
		c.DeviceModel, c.DeviceFriendlyName, c.DeviceOS, c.DeviceOSLanguage = "mail-gateway", "Mail gateway of the office", "Alpine Linux", "ar-sa"
	})
	f.open("")
	got := deviceInfo(t, f.fake.requests("Provision")[0].Root)
	for k, v := range map[string]string{"Model": "mail-gateway", "FriendlyName": "Mail gateway of the office", "OS": "Alpine Linux", "OSLanguage": "ar-sa",
		"UserAgent": "easbridge/test"} {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	// DeviceType is the one thing in the URL; it is not the model.
	if q := f.fake.last("Provision").Query; !strings.Contains(q, "DeviceType=easbridge") {
		t.Errorf("query %s", q)
	}
}

// post sends one raw WBXML request straight to the model.
func post(t *testing.T, f *fixture, cmd, version string, body *wbxml.Element) *wbxml.Element {
	t.Helper()
	b, err := wbxml.Marshal(&wbxml.Document{Root: body}, wbxml.DefaultRegistry())
	if err != nil {
		t.Fatal(err)
	}
	u := f.fake.url() + "?Cmd=" + cmd + "&User=" + url.QueryEscape(user) + "&DeviceId=" + dev + "&DeviceType=easbridge"
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Basic dGVzdDp0ZXN0")
	req.Header.Set("MS-ASProtocolVersion", version)
	req.Header.Set("Content-Type", "application/vnd.ms-sync.wbxml")
	resp, err := f.fake.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if len(out) == 0 {
		return nil
	}
	doc, err := wbxml.Unmarshal(out, wbxml.DefaultRegistry())
	if err != nil {
		t.Fatal(err)
	}
	return doc.Root
}

// The model is what makes the regression test bite, so it is tested itself.
func TestTheExchangeModelAnswers165WithoutDeviceInformation(t *testing.T) {
	f := newFixture(t)
	policies := func() *wbxml.Element {
		return e(wbxml.PageProvision, "Policies", e(wbxml.PageProvision, "Policy", e(wbxml.PageProvision, "PolicyType", txt("MS-EAS-Provisioning-WBXML"))))
	}
	deviceInformation := func(kids ...wbxml.Node) *wbxml.Element {
		return e(wbxml.PageSettings, "DeviceInformation", e(wbxml.PageSettings, "Set", kids...))
	}
	model := e(wbxml.PageSettings, "Model", txt("m"))
	status := func(r *wbxml.Element) string { // the Status of Provision itself, not the one inside DeviceInformation
		for _, k := range r.Children {
			if c, ok := k.(*wbxml.Element); ok && c.Name == "Status" {
				return c.TextContent()
			}
		}
		return ""
	}
	for _, tc := range []struct {
		name    string
		version string
		body    *wbxml.Element
		want    string
	}{
		{"no device information", "16.1", e(wbxml.PageProvision, "Provision", policies()), "165"},
		{"no device information on 14.1", "14.1", e(wbxml.PageProvision, "Provision", policies()), "165"},
		{"no device information on 16.0", "16.0", e(wbxml.PageProvision, "Provision", policies()), "165"},
		{"behind Policies", "16.1", e(wbxml.PageProvision, "Provision", policies(), deviceInformation(model)), "165"},
		{"an empty Set", "16.1", e(wbxml.PageProvision, "Provision", deviceInformation(), policies()), "165"},
		{"a Set without Model", "16.1", e(wbxml.PageProvision, "Provision", deviceInformation(e(wbxml.PageSettings, "OS", txt("Linux"))), policies()), "165"},
		{"in the right place", "16.1", e(wbxml.PageProvision, "Provision", deviceInformation(model), policies()), "1"},
		{"14.0 does not need it", "14.0", e(wbxml.PageProvision, "Provision", policies()), "1"},
		{"12.1 does not need it", "12.1", e(wbxml.PageProvision, "Provision", policies()), "1"},
	} {
		if got := status(post(t, f, "Provision", tc.version, tc.body)); got != tc.want {
			t.Errorf("%s: Provision status %s, want %s", tc.name, got, tc.want)
		}
	}
	// The acknowledgement is not asked for device information.
	ack := e(wbxml.PageProvision, "Provision", e(wbxml.PageProvision, "Policies", e(wbxml.PageProvision, "Policy",
		e(wbxml.PageProvision, "PolicyType", txt("MS-EAS-Provisioning-WBXML")), e(wbxml.PageProvision, "PolicyKey", txt("TEMPKEY")),
		e(wbxml.PageProvision, "Status", txt("1")))))
	if got := status(post(t, f, "Provision", "16.1", ack)); got != "1" {
		t.Errorf("acknowledgement: status %s", got)
	}
	// A Settings command beforehand (what v0.1.4 did) does not satisfy the rule.
	settings := e(wbxml.PageSettings, "Settings", deviceInformation(model))
	post(t, f, "Settings", "16.1", settings)
	if got := status(post(t, f, "Provision", "16.1", e(wbxml.PageProvision, "Provision", policies()))); got != "165" {
		t.Errorf("after a Settings command: Provision status %s, want 165", got)
	}
}

// The statuses the specification lists for Provision and for the Policy element,
// and the common ones that can come with it (MS-ASCMD 2.2.2, MS-ASPROV 3.1.5.2),
// each give a clear line, are classified right and are never repeated. 165 is the
// one the first real probe ran into.
func TestProvisionStatusesGiveOneClearLineAndAreNeverRetried(t *testing.T) {
	for _, tc := range []struct {
		name    string
		resp    *wbxml.Element
		code    int
		perm    bool
		must    string   // part of the message
		mustNot []string // names the vendored library gets wrong
	}{
		{"165 DeviceInformationRequired", provStatus("165"), 165, true, "server requires device information in Provision", nil},
		{"139 DeviceNotFullyProvisionable", provStatus("139"), 139, true, "cannot fully apply its device policy", nil},
		{"141 LegacyDeviceOnStrictPolicy", provStatus("141"), 141, true, "no policy key", []string{"DeviceNotProvisioned"}},
		{"145 ExternallyManagedDevicesNotAllowed", provStatus("145"), 145, true, "externally managed", []string{"NoRecurrence"}},
		{"142 DeviceNotProvisioned", provStatus("142"), 142, false, "provision first", []string{"PolicyRefresh"}},
		{"143 PolicyRefresh", provStatus("143"), 143, false, "policy has to be refreshed", []string{"InvalidPolicyKey"}},
		{"144 InvalidPolicyKey", provStatus("144"), 144, false, "no longer accepts the device's policy key", []string{"ExternallyManaged"}},
		{"129 DeviceIsBlockedForThisUser", provStatus("129"), 129, true, "blocks this device", nil},
		{"177 MaximumDevicesReached", provStatus("177"), 177, true, "too many device partnerships", []string{"ItemNotFound"}},
		{"Provision status 2", provStatus("2"), 2, true, "protocol error", nil},
		{"Provision status 3", provStatus("3"), 3, false, "usually temporary", nil},
		{"Policy status 3", policyStatus("3"), 3, true, "policy type", nil},
		{"Policy status 4", policyStatus("4"), 4, true, "corrupt", nil},
		{"Policy status 5", policyStatus("5"), 5, false, "out of date", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.fake.on("Provision", always(0, tc.resp))
			err := f.mailbox("").Open(ctx)
			var se *upstream.StatusError
			if !errors.As(err, &se) {
				t.Fatalf("err = %v", err)
			}
			if se.Op != "Provision" || se.Code != tc.code || se.Perm != tc.perm || !se.Global {
				t.Errorf("%+v, want Provision status %d permanent=%v global", se, tc.code, tc.perm)
			}
			if !strings.Contains(se.Reason, tc.must) {
				t.Errorf("message %q lacks %q", se.Reason, tc.must)
			}
			for _, bad := range tc.mustNot {
				if strings.Contains(err.Error(), bad) {
					t.Errorf("message %q uses the library's wrong name %q", err.Error(), bad)
				}
			}
			if strings.ContainsAny(err.Error(), "\r\n") || strings.Count(err.Error(), "eas:") != 0 {
				t.Errorf("not one clean line: %q", err.Error())
			}
			if tc.perm != !upstream.IsTemporary(err) {
				t.Errorf("IsTemporary = %v for permanent=%v", upstream.IsTemporary(err), tc.perm)
			}
			// Never repeated, never on another route: OPTIONS and one Provision.
			if n := len(f.fake.all()); n != 2 || f.fake.count("Provision") != 1 || f.fake.count("Settings") != 0 {
				t.Errorf("%d requests (Provision %d, Settings %d), want OPTIONS and one Provision", n, f.fake.count("Provision"), f.fake.count("Settings"))
			}
		})
	}
}

func TestStatus165IsTheMessageTheOperatorSees(t *testing.T) {
	f := newFixture(t)
	f.fake.on("Provision", always(0, provStatus("165")))
	err := f.mailbox("").Open(ctx)
	if err == nil || err.Error() != "upstream: Provision: status 165 (server requires device information in Provision)" {
		t.Errorf("err = %v", err)
	}
}

// A refusal in the acknowledgement (the second phase) is explained like any other.
func TestAStatusInTheAcknowledgementIsExplainedToo(t *testing.T) {
	f := newFixture(t)
	calls := 0
	f.fake.on("Provision", func(r *http.Request, in *wbxml.Element) (int, *wbxml.Element) {
		calls++
		if calls == 2 {
			return 0, policyStatus("5")
		}
		return provisionOK("T", "F")(r, in)
	})
	err := f.mailbox("").Open(ctx)
	var se *upstream.StatusError
	if !errors.As(err, &se) || se.Code != 5 || se.Perm || !strings.Contains(se.Reason, "out of date") {
		t.Errorf("err = %v", err)
	}
	if f.fake.count("Provision") != 2 {
		t.Errorf("%d Provision requests, want the two phases and no third", f.fake.count("Provision"))
	}
}

func TestAWipeOrderInTheInitialProvisionIsHonouredWithoutRetries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		el    string
		match error
	}{{"RemoteWipe", "RemoteWipe", upstream.ErrRemoteWipe}, {"AccountOnlyRemoteWipe", "AccountOnlyRemoteWipe", upstream.ErrAccountOnlyWipe}} {
		f := newFixture(t)
		f.fake.on("Provision", func(r *http.Request, in *wbxml.Element) (int, *wbxml.Element) {
			if in != nil && in.Find("DeviceInformation") == nil {
				return 0, provStatus("165")
			}
			return 0, e(wbxml.PageProvision, "Provision", e(wbxml.PageProvision, "Status", txt("1")), e(wbxml.PageProvision, tc.el))
		})
		m := f.mailbox("")
		if err := m.Open(ctx); !errors.Is(err, tc.match) || !errors.Is(err, upstream.ErrRemoteWipe) {
			t.Errorf("%s: Open: %v", tc.name, err)
		}
		if n := len(f.fake.all()); n != 2 || f.fake.count("Provision") != 1 {
			t.Errorf("%s: %d requests, want OPTIONS and one Provision", tc.name, n)
		}
	}
}

// MS-ASPROV 3.1.5.1.1: an initial request starts from policy key 0; MS-ASPROV 3.1.5.2
// lists status 141 for a request without a policy key.
func TestTheFirstProvisionStartsFromPolicyKeyZero(t *testing.T) {
	for _, stored := range []string{"", "STALEKEY"} {
		f := newFixture(t)
		m := f.open(stored)
		reqs := f.fake.requests("Provision")
		if reqs[0].Policy != "0" || reqs[1].Policy != "TEMPKEY" {
			t.Errorf("stored %q: X-MS-PolicyKey %q then %q, want 0 then TEMPKEY", stored, reqs[0].Policy, reqs[1].Policy)
		}
		if m.PolicyKey() != "FINALKEY" {
			t.Errorf("stored %q: policy key %q", stored, m.PolicyKey())
		}
		if r := f.fake.requests(""); len(r) != 1 || r[0].Policy != "" {
			t.Errorf("OPTIONS carried a policy key: %+v", r)
		}
	}
	// The 0 is a starting point, never a key: not after a refusal ...
	f := newFixture(t)
	f.fake.on("Provision", always(0, provStatus("165")))
	m := f.mailbox("STALEKEY")
	if err := m.Open(ctx); err == nil {
		t.Fatal("Open succeeded")
	}
	if k := m.PolicyKey(); k != "" {
		t.Errorf("policy key after a refused Provision = %q", k)
	}
	// ... and not when the server defines no policy at all.
	f = newFixture(t)
	f.fake.on("Provision", always(0, policyStatus("2")))
	if k := f.open("OLDKEY").PolicyKey(); k != "" {
		t.Errorf("policy key without a policy = %q", k)
	}
	// Status 2 of the policy is success, said in one line (not silently).
	if got := f.logs.String(); !strings.Contains(got, "Exchange defines no device policy for this device (Provision policy status 2)") {
		t.Errorf("no line about the missing policy in the log:\n%s", got)
	}
	if n := f.fake.count("Provision"); n != 1 {
		t.Errorf("%d Provision requests for a server without a policy, want 1", n)
	}
}

// Statuses that ask for provisioning in the middle of a session: once per call,
// and the call is repeated once.
func TestSessionStatusesThatAskForProvisioningAreAnsweredOncePerCall(t *testing.T) {
	syncStatus := func(code string) *wbxml.Element {
		return e(wbxml.PageAirSync, "Sync", e(wbxml.PageAirSync, "Status", txt(code)))
	}
	for _, code := range []string{"142", "143", "144"} {
		t.Run(code, func(t *testing.T) {
			f := newFixture(t)
			m := f.open("")
			base := len(f.fake.all())
			n := 0
			f.fake.on("Sync", func(r *http.Request, in *wbxml.Element) (int, *wbxml.Element) {
				n++
				if n == 1 {
					return 0, syncStatus(code)
				}
				return 0, syncResp("5", "k2", "1")
			})
			f.fake.on("Provision", provisionOK("T2", "F2"))
			d, err := m.Sync(ctx, "5", "k1", upstream.SyncOptions{})
			if err != nil || d.NewKey != "k2" {
				t.Fatalf("Sync: %+v %v", d, err)
			}
			if got := len(f.fake.all()) - base; got != 4 { // Sync, Provision x2, Sync
				t.Errorf("%d requests after the status, want 4", got)
			}
			if k := f.fake.requests("Sync")[1].Policy; k != "F2" || m.PolicyKey() != "F2" {
				t.Errorf("the repeat used policy key %q (mailbox %q), want F2", k, m.PolicyKey())
			}
			// The re-provisioning describes the device again.
			if got := deviceInfo(t, f.fake.requests("Provision")[2].Root)["Model"]; got != "easbridge" {
				t.Errorf("Model in the re-provisioning = %q", got)
			}
		})
	}

	// A status that does not go away: one re-provisioning and one repeat per
	// call, however often the engine calls. Nothing loops inside the call.
	f := newFixture(t)
	m := f.open("")
	base := len(f.fake.all())
	f.fake.on("Sync", always(0, syncStatus("143")))
	for call := 1; call <= 3; call++ {
		_, err := m.Sync(ctx, "5", "k1", upstream.SyncOptions{})
		var se *upstream.StatusError
		if !errors.As(err, &se) || se.Code != 143 || se.Perm || !strings.Contains(se.Reason, "policy has to be refreshed") {
			t.Fatalf("call %d: %v", call, err)
		}
		if got := len(f.fake.all()) - base; got != 4*call {
			t.Fatalf("after call %d: %d requests, want %d", call, got, 4*call)
		}
	}
}

func TestADeviceLevelStatusInASessionIsExplainedAndNeitherRetriedNorHeldAgainstTheMessage(t *testing.T) {
	for _, tc := range []struct {
		code, must string
		perm       bool
	}{{"129", "blocks this device", true}, {"139", "cannot fully apply", true}, {"141", "not provisionable", true},
		{"145", "externally managed", true}, {"165", "device information", true}, {"126", "disabled", true}, {"131", "disabled", true}} {
		f := newFixture(t)
		m := f.open("")
		base := len(f.fake.all())
		f.fake.on("ItemOperations", always(0, e(wbxml.PageItemOperations, "ItemOperations", e(wbxml.PageItemOperations, "Status", txt(tc.code)))))
		_, err := m.FetchMIME(ctx, "5", "5:1")
		var se *upstream.StatusError
		if !errors.As(err, &se) || se.Op != "Fetch" || se.Perm != tc.perm || !se.Global || !strings.Contains(se.Reason, tc.must) {
			t.Errorf("%s: %v (%+v)", tc.code, err, se)
		}
		if got := len(f.fake.all()) - base; got != 1 {
			t.Errorf("%s: %d requests, want exactly the one that was asked for", tc.code, got)
		}
	}
}

// A status 140 in the middle of a session leads to the wipe order and honours it.
func TestStatus140InASessionLeadsToTheWipeOrder(t *testing.T) {
	f := newFixture(t)
	m := f.open("")
	f.fake.on("Sync", always(0, e(wbxml.PageAirSync, "Sync", e(wbxml.PageAirSync, "Status", txt("140")))))
	f.fake.on("Provision", always(0, e(wbxml.PageProvision, "Provision", e(wbxml.PageProvision, "Status", txt("1")), e(wbxml.PageProvision, "RemoteWipe"))))
	base := len(f.fake.all())
	if _, err := m.Sync(ctx, "5", "k1", upstream.SyncOptions{}); !errors.Is(err, upstream.ErrRemoteWipe) {
		t.Fatalf("err = %v", err)
	}
	if got := len(f.fake.all()) - base; got != 2 { // Sync, Provision
		t.Errorf("%d requests, want 2", got)
	}
}

// Where the time of Open goes (the first real probe took 21 s): into the requests
// and nowhere else. The bridge adds no waits, no retries and no fallbacks of its
// own, so a failing Open costs two round trips and a successful one three.
func TestOpenSpendsItsTimeOnTheRequestsAndNowhereElse(t *testing.T) {
	const d = 120 * time.Millisecond
	// The request count is the proof; the time only has to rule out a wait of
	// seconds (a back-off, a fallback), so the upper bound is generous enough for a
	// busy machine that runs many packages at once.
	slack := 4 * time.Second

	f := newFixture(t)
	f.fake.delay = d
	start := time.Now()
	f.open("")
	if el, n := time.Since(start), len(f.fake.all()); n != 3 || el < 3*d || el > 3*d+slack {
		t.Errorf("successful Open: %d requests in %v, want 3 requests in about %v", n, el, 3*d)
	}

	f = newFixture(t)
	f.fake.delay = d
	f.fake.on("Provision", always(0, provStatus("165")))
	start = time.Now()
	err := f.mailbox("").Open(ctx)
	if el, n := time.Since(start), len(f.fake.all()); err == nil || n != 2 || el < 2*d || el > 2*d+slack {
		t.Errorf("failing Open: %d requests in %v (%v), want 2 requests in about %v", n, el, err, 2*d)
	}
}

// ---- the probe -----------------------------------------------------------------

func TestTheProbeShowsTheDeviceTheRequestsAndTheReasonForARefusal(t *testing.T) {
	f := newFixture(t)
	f.fake.delay = 20 * time.Millisecond
	f.fake.on("Provision", always(0, provStatus("165")))
	var out bytes.Buffer
	err := easupstream.Probe(ctx, easupstream.ProbeConfig{SkipContacts: true,
		Config: easupstream.Config{URL: f.fake.url(), HTTPClient: f.fake.srv.Client(), Version: "16.1", UserAgent: "easbridge/probe-test",
			DeviceModel: "probe-model"},
		User: user, Password: pass, DeviceID: dev, Out: &out,
	})
	var se *upstream.StatusError
	if !errors.As(err, &se) || se.Code != 165 {
		t.Fatalf("err = %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"options+prov FAILED  Provision status 165 permanent=true: server requires device information in Provision",
		`model="probe-model"`, `friendly_name="easbridge (MailFlow bridge)"`, `os="Linux"`, `os_language="en-us"`, `user_agent="easbridge/probe-test"`,
		"no IMEI, phone number or mobile operator",
		"http: OPTIONS -> HTTP 200 in ", "http: POST Provision -> HTTP 200 in ", "answer after ", "new connection ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "http: POST Provision") != 1 {
		t.Errorf("the failing Provision was repeated:\n%s", got)
	}
	// The report names no user, password, URL or query.
	for _, bad := range []string{user, pass, "alice", "Cmd=", "User=", "https://", "127.0.0.1"} {
		if strings.Contains(got, bad) {
			t.Errorf("report contains %q:\n%s", bad, got)
		}
	}
}

// The probe must describe the same device as the service, so the part of the
// configuration both build from is one value (cmd/easbridge shares it as well).
func TestTheProbeSendsTheDeviceItWasGiven(t *testing.T) {
	f := newFixture(t)
	f.fake.on("FolderSync", always(http.StatusUnauthorized, nil)) // stops the probe after Open
	var out bytes.Buffer
	easupstream.Probe(ctx, easupstream.ProbeConfig{SkipContacts: true,
		Config: easupstream.Config{URL: f.fake.url(), HTTPClient: f.fake.srv.Client(), Version: "16.1", UserAgent: "ua/1", DeviceType: "type1",
			DeviceModel: "m1", DeviceFriendlyName: "f1", DeviceOS: "o1", DeviceOSLanguage: "l1"},
		User: user, Password: pass, DeviceID: dev, Out: &out,
	})
	got := deviceInfo(t, f.fake.requests("Provision")[0].Root)
	for k, v := range map[string]string{"Model": "m1", "FriendlyName": "f1", "OS": "o1", "OSLanguage": "l1", "UserAgent": "ua/1"} {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if q := f.fake.last("Provision").Query; !strings.Contains(q, "DeviceType=type1") {
		t.Errorf("query %s", q)
	}
}

func TestTheProbeReportsARequestThatGotNoAnswer(t *testing.T) {
	f := newFixture(t)
	f.fake.srv.Close()
	var out bytes.Buffer
	err := easupstream.Probe(ctx, easupstream.ProbeConfig{SkipContacts: true,
		Config: easupstream.Config{URL: f.fake.url(), HTTPClient: f.fake.srv.Client(), Version: "16.1"},
		User:   user, Password: pass, DeviceID: dev, Out: &out,
	})
	if !errors.Is(err, upstream.ErrNotSent) {
		t.Fatalf("err = %v", err)
	}
	var lines []string
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.Contains(l, "http: ") {
			lines = append(lines, strings.TrimSpace(l))
		}
	}
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "http: OPTIONS -> no answer after ") {
		t.Errorf("request lines: %q\n%s", lines, out.String())
	}
	if strings.Contains(lines[0], "127.0.0.1") || strings.Contains(lines[0], "dial") {
		t.Errorf("the request line names the host or the error: %q", lines[0])
	}
}

// A status the bridge has no text for keeps the library's: permanent unless it is
// one of the server-side family, and global from 101 on (the small numbers are the
// statuses of one command or one item).
func TestAnUndescribedStatusKeepsTheLibrarysTextAndIsGlobalFrom101(t *testing.T) {
	for _, tc := range []struct {
		code   string
		global bool
	}{{"164", true}, {"150", true}, {"6", false}, {"8", false}} {
		f := newFixture(t)
		m := f.open("")
		f.fake.on("ItemOperations", always(0, e(wbxml.PageItemOperations, "ItemOperations", e(wbxml.PageItemOperations, "Status", txt(tc.code)))))
		_, err := m.FetchMIME(ctx, "5", "5:1")
		var se *upstream.StatusError
		if !errors.As(err, &se) || !se.Perm || se.Global != tc.global || !strings.Contains(se.Reason, "status "+tc.code) {
			t.Errorf("status %s: %v (%+v)", tc.code, err, se)
		}
	}
}
