package easupstream_test

// Tests for `easbridge probe --trace` and for what the model refuses (v0.1.6,
// real-Exchange finding 2, docs/TESTING.md).

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hstern/go-activesync/wbxml"

	"easbridge/internal/easupstream"
	"easbridge/internal/upstream"
)

// secrets that must never reach a trace.
const (
	secretSubject = "TOP SECRET SUBJECT"
	secretSender  = "secret.sender@example.test"
	secretFolder  = "Divorce lawyer"
	secretBody    = "the body of the message: do not print"
	secretItemID  = "5:777"
)

// A probe whose server returns private data in every place the protocol has for it.
func traceFixture(t *testing.T) *fixture {
	f := newFixture(t)
	f.fake.on("FolderSync", always(0, folderSyncResp("h1",
		folderAdd("5", "0", "Inbox", 2), folderAdd("6", "0", "Sent Items", 5), folderAdd("7", "0", secretFolder, 12))))
	f.fake.on("Sync", func(r *http.Request, in *wbxml.Element) (int, *wbxml.Element) {
		if key, _ := text(in, "SyncKey"); key == "0" {
			return 0, syncResp("5", "k1", "1")
		}
		item := addItem(secretItemID+"1", "stub", true, 0, "2026-10-01T10:00:00.000Z", true, 51234)
		app := item.Find("ApplicationData")
		app.Children = append(app.Children,
			e(wbxml.PageEmail, "Subject", txt(secretSubject)),
			e(wbxml.PageEmail, "From", txt("Carol <"+secretSender+">")),
			e(wbxml.PageEmail, "To", txt(user)),
			e(wbxml.PageEmail, "DisplayTo", txt(user)),
			e(wbxml.PageEmail, "ThreadTopic", txt(secretSubject)),
			e(wbxml.PageEmail2, "ConversationId", wbxml.Opaque("conversation-id-bytes")),
			e(wbxml.PageEmail, "Importance", txt("1")),
			// A tag the tables do not know, with a value.
			e(wbxml.PageAirSyncBase, "Location", e(wbxml.PageAirSyncBase, "Street", txt("12 Private Street"))))
		return 0, syncResp("5", "k2", "1", commands(item), e(wbxml.PageAirSync, "MoreAvailable"))
	})
	// The Fetch answer as Exchange 2019 sends it: the MIME as an inline string.
	fetched := "Subject: " + secretSubject + "\r\nFrom: " + secretSender + "\r\n\r\n" + secretBody
	f.fake.on("ItemOperations", always(0, fetchResponse("5", secretItemID+"1", "1",
		messageProperties(fetched, true, 0, "2026-10-01T10:00:00.000Z", mimeBody(mimeData(fetched), false, 99), false))))
	f.fake.on("Ping", always(0, pingResp("1")))
	f.fake.cookie = "GatewaySession=gateway-session-token-123"
	return f
}

func runProbe(t *testing.T, f *fixture, trace bool) string {
	t.Helper()
	var out bytes.Buffer
	err := easupstream.Probe(ctx, easupstream.ProbeConfig{SkipContacts: true,
		Config: easupstream.Config{URL: f.fake.url(), HTTPClient: f.fake.srv.Client(), Version: "16.1", UserAgent: "easbridge/trace-test"},
		User:   user, Password: pass, DeviceID: dev, PingSeconds: 1, Out: &out, Trace: trace,
	})
	if err != nil {
		t.Fatalf("probe: %v\n%s", err, out.String())
	}
	return out.String()
}

func TestTraceIsOffByDefault(t *testing.T) {
	got := runProbe(t, traceFixture(t), false)
	for _, noise := range []string{"request  POST", "request  OPTIONS", "response HTTP", "<Sync>", "<Provision>"} {
		if strings.Contains(got, noise) {
			t.Errorf("%q in a report without --trace:\n%s", noise, got)
		}
	}
	if !strings.Contains(got, "http: OPTIONS ->") || !strings.Contains(got, "probe finished") {
		t.Errorf("the ordinary report is gone:\n%s", got)
	}
}

func TestTraceShowsTheStructureOfEveryCallAndNothingPrivate(t *testing.T) {
	f := traceFixture(t)
	got := runProbe(t, f, true)

	// The structure: what the next protocol error needs.
	for _, want := range []string{
		// OPTIONS: what the server speaks.
		"request  OPTIONS", "response HTTP 200 MS-ASProtocolVersions=2.5,12.0,12.1,14.0,14.1,16.0,16.1 MS-ASProtocolCommands=Sync,SendMail",
		// Provision, with the device information in front of Policies, and the acknowledgement.
		"request  POST MS-ASProtocolVersion=16.1 X-MS-PolicyKey=0", "<Provision>", "<settings:DeviceInformation>", "<settings:Model>easbridge</settings:Model>",
		"<settings:UserAgent>easbridge/trace-test</settings:UserAgent>", "<PolicyType>MS-EAS-Provisioning-WBXML</PolicyType>", "X-MS-PolicyKey=TEMPKEY", "<PolicyKey>TEMPKEY</PolicyKey>",
		"<PolicyKey>FINALKEY</PolicyKey>",
		// FolderSync.
		"<FolderSync>", "<SyncKey>0</SyncKey>", "<Count>3</Count>",
		// The boot Sync of finding 2: the key and the collection, nothing else.
		"<SyncKey>0</SyncKey>\n", "<CollectionId>5</CollectionId>",
		// The data Sync: the options in order.
		"<GetChanges>1</GetChanges>", "<WindowSize>5</WindowSize>", "<airsyncbase:BodyPreference>", "<airsyncbase:TruncationSize>1</airsyncbase:TruncationSize>",
		"<email:Importance>1</email:Importance>", "<MoreAvailable/>",
		// ItemOperations, with MIMESupport in its Options.
		"<ItemOperations>", "<Store>Mailbox</Store>", "<airsync:CollectionId>5</airsync:CollectionId>", "<airsync:MIMESupport>2</airsync:MIMESupport>", "<airsyncbase:EstimatedDataSize>99</airsyncbase:EstimatedDataSize>",
		// Ping: the status.
		"<Ping>", "<HeartbeatInterval>1</HeartbeatInterval>", "<Class>Email</Class>", "<Status>1</Status>",
		// The tag the tables do not know does not break the trace; its value is not shown.
		"<airsyncbase:Location>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("trace lacks %q:\n%s", want, got)
		}
	}
	// The data is there as structure only.
	for _, want := range []string{"<email:Subject>[", "<email:From>[", "<email:To>[", "<airsyncbase:Data>[", "<DisplayName>[", "<ServerId>[", "<email2:ConversationId>[", "<airsyncbase:Street>[", "bytes not shown]"} {
		if !strings.Contains(got, want) {
			t.Errorf("trace lacks the marker %q:\n%s", want, got)
		}
	}
	// Real-Exchange finding 3: the trace said "N bytes not shown" for the MIME of the
	// Fetch although it could not tell an inline string from an OPAQUE block, which is
	// what made the difference. It says which it was: the MIME of the Fetch came as text,
	// the ConversationId as binary.
	for _, want := range []string{"<airsyncbase:Data>[" /* the text of the Fetch */, " text bytes not shown]</airsyncbase:Data>", "<email2:ConversationId>[16 binary bytes not shown]", "<email2:ConversationIndex>[22 binary bytes not shown]"} {
		if !strings.Contains(got, want) {
			t.Errorf("trace lacks %q:\n%s", want, got)
		}
	}

	// Nothing private: no content, no names, no credentials, nothing of the connection.
	for _, bad := range []string{
		secretSubject, secretSender, secretFolder, secretBody, secretItemID, "12 Private Street", "conversation-id-bytes",
		user, pass, "alice", "Basic ", "Authorization", "Cookie", "GatewaySession", "gateway-session-token", "User=", "Cmd=", "DeviceId=",
		"Inbox", "Sent Items", "https://", "127.0.0.1", "localhost",
	} {
		if strings.Contains(got, bad) {
			t.Errorf("the trace shows %q:\n%s", bad, got)
		}
	}
}

func TestTraceDecodesAGzippedAnswerAndSaysLittleAboutWhatIsNotWBXML(t *testing.T) {
	f := newFixture(t)
	f.fake.on("FolderSync", func(r *http.Request, in *wbxml.Element) (int, *wbxml.Element) {
		return http.StatusForbidden, nil // a gateway's refusal: no WBXML at all
	})
	var out bytes.Buffer
	err := easupstream.Probe(ctx, easupstream.ProbeConfig{SkipContacts: true,
		Config: easupstream.Config{URL: f.fake.url(), HTTPClient: f.fake.srv.Client(), Version: "16.1"},
		User:   user, Password: pass, DeviceID: dev, Out: &out, Trace: true,
	})
	if !errors.Is(err, upstream.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "response HTTP 403") || !strings.Contains(got, "(empty body)") {
		t.Errorf("a refusal without a body:\n%s", got)
	}
}

// ---- the model's rules, tested through raw requests ---------------------------------

func syncRequest(collection ...wbxml.Node) *wbxml.Element {
	return e(wbxml.PageAirSync, "Sync", e(wbxml.PageAirSync, "Collections", e(wbxml.PageAirSync, "Collection", collection...)))
}

func air(name, value string) *wbxml.Element { return e(wbxml.PageAirSync, name, txt(value)) }

func bodyPref(kids ...wbxml.Node) *wbxml.Element {
	return e(wbxml.PageAirSyncBase, "BodyPreference", kids...)
}

func abase(name, value string) *wbxml.Element { return e(wbxml.PageAirSyncBase, name, txt(value)) }

func TestTheModelRefusesWhatExchangeRefusesInASyncRequest(t *testing.T) {
	f := newFixture(t)
	// "4" is the refusal: a top-level Status. An accepted request is answered by the
	// handler below, whose Status is the collection's.
	status := func(version string, req *wbxml.Element) string {
		root := post(t, f, "Sync", version, req)
		if root == nil {
			return "empty"
		}
		for _, k := range root.Children {
			if c, ok := k.(*wbxml.Element); ok && c.Name == "Status" {
				return c.TextContent()
			}
		}
		return "1"
	}
	// The model answers a request that passes with whatever the default handler is:
	// the point is the 4.
	f.fake.on("Sync", always(0, syncResp("5", "k1", "1")))
	f.fake.expectViolations = true

	ok := []struct {
		name string
		req  *wbxml.Element
	}{
		{"SyncKey 0 and CollectionId (MS-ASCMD 3.1.5.4)", syncRequest(air("SyncKey", "0"), air("CollectionId", "5"))},
		{"GetChanges 0 with SyncKey 0 (2.2.3.84: no error)", syncRequest(air("SyncKey", "0"), air("CollectionId", "5"), air("GetChanges", "0"))},
		{"the data request of the bridge", syncRequest(air("SyncKey", "k1"), air("CollectionId", "5"), air("DeletesAsMoves", "1"), air("GetChanges", "1"), air("WindowSize", "5"),
			e(wbxml.PageAirSync, "Options", air("FilterType", "4"), bodyPref(abase("Type", "4"), abase("TruncationSize", "1048576")), air("MIMESupport", "2")))},
		{"an empty GetChanges with a real key", syncRequest(air("SyncKey", "k1"), air("CollectionId", "5"), e(wbxml.PageAirSync, "GetChanges"))},
		{"two Options (the SMS class and the folder's)", syncRequest(air("SyncKey", "k1"), air("CollectionId", "5"), e(wbxml.PageAirSync, "Options", air("Class", "SMS")),
			e(wbxml.PageAirSync, "Options", air("FilterType", "3")))},
	}
	for _, tc := range ok {
		if got := status("16.1", tc.req); got != "1" {
			t.Errorf("%s: status %s, want it accepted", tc.name, got)
		}
	}
	bad := []struct {
		name string
		req  *wbxml.Element
	}{
		{"GetChanges 1 with SyncKey 0 (2.2.3.84: the finding)", syncRequest(air("SyncKey", "0"), air("CollectionId", "5"), air("GetChanges", "1"))},
		{"GetChanges empty with SyncKey 0 (2.2.3.84)", syncRequest(air("SyncKey", "0"), air("CollectionId", "5"), e(wbxml.PageAirSync, "GetChanges"))},
		{"the request of v0.1.5 for a first key", syncRequest(air("SyncKey", "0"), air("CollectionId", "5"), air("DeletesAsMoves", "1"), air("GetChanges", "1"), air("WindowSize", "5"),
			e(wbxml.PageAirSync, "Options", bodyPref(abase("Type", "1"), abase("TruncationSize", "1"))))},
		{"Commands with SyncKey 0", syncRequest(air("SyncKey", "0"), air("CollectionId", "5"),
			e(wbxml.PageAirSync, "Commands", e(wbxml.PageAirSync, "Delete", air("ServerId", "5:1"))))},
		{"Commands without an operation", syncRequest(air("SyncKey", "k1"), air("CollectionId", "5"), e(wbxml.PageAirSync, "Commands"))},
		{"Class on protocol 16.1 (2.2.3.29.2)", syncRequest(air("Class", "Email"), air("SyncKey", "k1"), air("CollectionId", "5"))},
		{"Options before WindowSize (strict ordering)", syncRequest(air("SyncKey", "k1"), air("CollectionId", "5"), e(wbxml.PageAirSync, "Options", air("FilterType", "3")), air("WindowSize", "5"))},
		{"GetChanges before DeletesAsMoves (strict ordering)", syncRequest(air("SyncKey", "k1"), air("CollectionId", "5"), air("GetChanges", "1"), air("DeletesAsMoves", "1"))},
		{"CollectionId before SyncKey (strict ordering)", syncRequest(air("CollectionId", "5"), air("SyncKey", "k1"))},
		{"no CollectionId", syncRequest(air("SyncKey", "k1"))},
		{"WindowSize over 512", syncRequest(air("SyncKey", "k1"), air("CollectionId", "5"), air("WindowSize", "513"))},
		{"GetChanges that is not a boolean", syncRequest(air("SyncKey", "k1"), air("CollectionId", "5"), air("GetChanges", "yes"))},
		{"a third Options", syncRequest(air("SyncKey", "k1"), air("CollectionId", "5"), e(wbxml.PageAirSync, "Options", air("FilterType", "3")),
			e(wbxml.PageAirSync, "Options", air("FilterType", "3")), e(wbxml.PageAirSync, "Options", air("FilterType", "3")))},
		{"FilterType 8 for email (2.2.3.68.2)", syncRequest(air("SyncKey", "k1"), air("CollectionId", "5"), e(wbxml.PageAirSync, "Options", air("FilterType", "8")))},
		{"two FilterType", syncRequest(air("SyncKey", "k1"), air("CollectionId", "5"), e(wbxml.PageAirSync, "Options", air("FilterType", "3"), air("FilterType", "4")))},
		{"two MIMESupport (2.2.3.110.3)", syncRequest(air("SyncKey", "k1"), air("CollectionId", "5"),
			e(wbxml.PageAirSync, "Options", bodyPref(abase("Type", "4")), air("MIMESupport", "2"), air("MIMESupport", "2")))},
		{"MIMESupport without a BodyPreference of Type 4", syncRequest(air("SyncKey", "k1"), air("CollectionId", "5"),
			e(wbxml.PageAirSync, "Options", bodyPref(abase("Type", "1")), air("MIMESupport", "2")))},
		{"MIMESupport 3", syncRequest(air("SyncKey", "k1"), air("CollectionId", "5"), e(wbxml.PageAirSync, "Options", bodyPref(abase("Type", "4")), air("MIMESupport", "3")))},
		{"two BodyPreference of one Type (MS-ASAIRS 2.2.2.12)", syncRequest(air("SyncKey", "k1"), air("CollectionId", "5"),
			e(wbxml.PageAirSync, "Options", bodyPref(abase("Type", "1")), bodyPref(abase("Type", "1"))))},
		{"TruncationSize before Type (MS-ASAIRS 2.2.2.12)", syncRequest(air("SyncKey", "k1"), air("CollectionId", "5"),
			e(wbxml.PageAirSync, "Options", bodyPref(abase("TruncationSize", "10"), abase("Type", "1"))))},
		{"MIMETruncation 9", syncRequest(air("SyncKey", "k1"), air("CollectionId", "5"),
			e(wbxml.PageAirSync, "Options", air("MIMETruncation", "9")))},
		{"an element that does not belong", syncRequest(air("SyncKey", "k1"), air("CollectionId", "5"), air("Status", "1"))},
	}
	for _, tc := range bad {
		if got := status("16.1", tc.req); got != "4" {
			t.Errorf("%s: status %s, want 4 (protocol error)", tc.name, got)
		}
	}
	// Class is allowed where the specification allows it (2.5, 12.0, 12.1).
	if got := status("12.1", syncRequest(air("Class", "Email"), air("SyncKey", "k1"), air("CollectionId", "5"))); got != "1" {
		t.Errorf("Class on 12.1: status %s", got)
	}
	if n := len(f.fake.violations); n != len(bad)+0 {
		t.Errorf("the model recorded %d refusals for %d bad requests: %v", n, len(bad), f.fake.violations)
	}
}

func TestTheModelRefusesWhatExchangeRefusesInAnItemOperationsFetchAndAPing(t *testing.T) {
	f := newFixture(t)
	f.fake.expectViolations = true
	f.fake.on("ItemOperations", always(0, e(wbxml.PageItemOperations, "ItemOperations", e(wbxml.PageItemOperations, "Status", txt("1")))))
	f.fake.on("Ping", always(0, pingResp("1")))
	io := func(name, value string) *wbxml.Element { return e(wbxml.PageItemOperations, name, txt(value)) }
	fetch := func(kids ...wbxml.Node) *wbxml.Element {
		return e(wbxml.PageItemOperations, "ItemOperations", e(wbxml.PageItemOperations, "Fetch", kids...))
	}
	opts := func(kids ...wbxml.Node) *wbxml.Element { return e(wbxml.PageItemOperations, "Options", kids...) }
	statusOf := func(cmd string, req *wbxml.Element) string {
		root := post(t, f, cmd, "16.1", req)
		for _, k := range root.Children {
			if c, ok := k.(*wbxml.Element); ok && c.Name == "Status" {
				return c.TextContent()
			}
		}
		return "no status"
	}
	// What the bridge sends: MIMESupport in the AirSync namespace, BodyPreference Type 4.
	good := fetch(io("Store", "Mailbox"), air("CollectionId", "5"), air("ServerId", "5:1"),
		opts(air("MIMESupport", "2"), bodyPref(abase("Type", "4"))))
	if got := statusOf("ItemOperations", good); got != "1" {
		t.Errorf("the bridge's Fetch: status %s", got)
	}
	for name, req := range map[string]*wbxml.Element{
		"MIMESupport without Type 4": fetch(io("Store", "Mailbox"), air("CollectionId", "5"), air("ServerId", "5:1"),
			opts(air("MIMESupport", "2"), bodyPref(abase("Type", "1")))),
		"no Store":                  fetch(air("CollectionId", "5"), air("ServerId", "5:1")),
		"no CollectionId":           fetch(io("Store", "Mailbox"), air("ServerId", "5:1")),
		"an empty ItemOperations":   e(wbxml.PageItemOperations, "ItemOperations"),
		"an Options of no child":    fetch(io("Store", "Mailbox"), air("CollectionId", "5"), air("ServerId", "5:1"), opts()),
		"two BodyPreference Type 4": fetch(io("Store", "Mailbox"), air("CollectionId", "5"), air("ServerId", "5:1"), opts(bodyPref(abase("Type", "4")), bodyPref(abase("Type", "4")))),
	} {
		if got := statusOf("ItemOperations", req); got != "2" {
			t.Errorf("%s: status %s, want 2 (protocol error)", name, got)
		}
	}

	pi := func(name, value string) *wbxml.Element { return e(wbxml.PagePing, name, txt(value)) }
	folder := func(id, class string) *wbxml.Element {
		return e(wbxml.PagePing, "Folder", pi("Id", id), pi("Class", class))
	}
	if got := statusOf("Ping", e(wbxml.PagePing, "Ping", pi("HeartbeatInterval", "480"), e(wbxml.PagePing, "Folders", folder("5", "Email")))); got != "1" {
		t.Errorf("the bridge's Ping: status %s", got)
	}
	for name, req := range map[string]*wbxml.Element{
		"a Class that does not exist":     e(wbxml.PagePing, "Ping", pi("HeartbeatInterval", "480"), e(wbxml.PagePing, "Folders", folder("5", "Mail"))),
		"no Folder":                       e(wbxml.PagePing, "Ping", pi("HeartbeatInterval", "480"), e(wbxml.PagePing, "Folders")),
		"a Folder without Class":          e(wbxml.PagePing, "Ping", e(wbxml.PagePing, "Folders", e(wbxml.PagePing, "Folder", pi("Id", "5")))),
		"a heartbeat that is no number":   e(wbxml.PagePing, "Ping", pi("HeartbeatInterval", "soon"), e(wbxml.PagePing, "Folders", folder("5", "Email"))),
		"an element that is no request's": e(wbxml.PagePing, "Ping", pi("MaxFolders", "1")),
	} {
		if got := statusOf("Ping", req); got != "4" {
			t.Errorf("%s: status %s, want 4 (syntax error)", name, got)
		}
	}
}

// The details of the boot Sync, seen from the adapter the service and the probe use.
func TestTheFirstSyncKeyIsAskedForWithTheKeyAndTheCollectionOnly(t *testing.T) {
	for name, headersOnly := range map[string]bool{"the service": false, "the probe": true} {
		f := newFixture(t, func(c *easupstream.Config) { c.WindowSize = 20 })
		m := f.open("")
		f.fake.on("Sync", always(0, syncResp("5", "k1", "1")))
		if _, err := m.Sync(ctx, "5", "0", upstream.SyncOptions{HeadersOnly: headersOnly}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		coll := f.fake.last("Sync").Root.Find("Collection")
		var names []string
		for _, k := range kids(coll) {
			names = append(names, k.Name)
		}
		if got := strings.Join(names, ","); got != "SyncKey,CollectionId" {
			t.Errorf("%s: the request for a first key holds %s, want SyncKey,CollectionId", name, got)
		}
		if len(f.fake.violations) != 0 {
			t.Errorf("%s: refused: %v", name, f.fake.violations)
		}
	}
}

func TestTheDataSyncKeepsTheOptionsInTheOrderOfTheSpecification(t *testing.T) {
	f := newFixture(t, func(c *easupstream.Config) { c.WindowSize = 20; c.Filter = 4; c.MIMEInlineMax = 1 << 20 })
	m := f.open("")
	f.fake.on("Sync", always(0, syncResp("5", "k2", "1")))
	if _, err := m.Sync(ctx, "5", "k1", upstream.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	coll := f.fake.last("Sync").Root.Find("Collection")
	var names, opts []string
	for _, k := range kids(coll) {
		names = append(names, k.Name)
	}
	for _, k := range kids(coll.Find("Options")) {
		opts = append(opts, k.Name)
	}
	if got, want := strings.Join(names, ","), "SyncKey,CollectionId,DeletesAsMoves,GetChanges,WindowSize,Options"; got != want {
		t.Errorf("Collection = %s, want %s", got, want)
	}
	if got, want := strings.Join(opts, ","), "FilterType,BodyPreference,MIMESupport"; got != want {
		t.Errorf("Options = %s, want %s", got, want)
	}
	if len(f.fake.violations) != 0 {
		t.Errorf("refused: %v", f.fake.violations)
	}
}

func TestSyncStatus4IsExplainedAndPermanent(t *testing.T) {
	f := newFixture(t)
	m := f.open("")
	f.fake.on("Sync", always(0, e(wbxml.PageAirSync, "Sync", e(wbxml.PageAirSync, "Status", txt("4")))))
	_, err := m.Sync(ctx, "5", "k1", upstream.SyncOptions{})
	var se *upstream.StatusError
	if !errors.As(err, &se) || se.Op != "Sync" || se.Code != 4 || !se.Perm || se.Global ||
		!strings.Contains(se.Reason, "protocol error") || !strings.Contains(se.Reason, "easbridge probe --trace") {
		t.Fatalf("err = %v (%+v)", err, se)
	}
	if strings.ContainsAny(err.Error(), "\r\n") || strings.Contains(err.Error(), "eas:") {
		t.Errorf("not one clean line: %q", err.Error())
	}
	// The same status as the answer of a collection.
	f.fake.on("Sync", always(0, syncResp("5", "k1", "4")))
	if _, err := m.Sync(ctx, "5", "k1", upstream.SyncOptions{}); !errors.As(err, &se) || se.Code != 4 || !strings.Contains(se.Reason, "protocol error") {
		t.Errorf("collection status 4: %v", err)
	}
}

func TestTheProbeNamesTheRequestThatBrokeTheRules(t *testing.T) {
	// A server that insists on a minimal first Sync: the probe says what it saw.
	f := newFixture(t)
	f.fake.on("FolderSync", always(0, folderSyncResp("h1", folderAdd("5", "0", "Inbox", 2))))
	f.fake.on("Sync", always(0, e(wbxml.PageAirSync, "Sync", e(wbxml.PageAirSync, "Status", txt("4")))))
	var out bytes.Buffer
	err := easupstream.Probe(ctx, easupstream.ProbeConfig{SkipContacts: true,
		Config: easupstream.Config{URL: f.fake.url(), HTTPClient: f.fake.srv.Client(), Version: "16.1"},
		User:   user, Password: pass, DeviceID: dev, Out: &out, Trace: true,
	})
	if err == nil {
		t.Fatal("probe succeeded")
	}
	got := out.String()
	for _, want := range []string{"sync boot    FAILED  Sync status 4 permanent=true: the server rejected the Sync request as a protocol error",
		"easbridge probe --trace", "<Sync>", "<SyncKey>0</SyncKey>", "<CollectionId>5</CollectionId>", "<Status>4</Status>"} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "<GetChanges>") {
		t.Errorf("the first Sync still asks for changes:\n%s", got)
	}
}

func TestTraceCostsNothingButTimeInTheNormalRun(t *testing.T) {
	// The answers are read through a wrapper; a slow, large answer must still arrive whole.
	f := traceFixture(t)
	big := bytes.Repeat([]byte("x"), 1<<20)
	f.fake.on("ItemOperations", always(0, fetchMIMEResponse(mimeData, "5", secretItemID+"1", string(big), true, 0, "2026-10-01T10:00:00.000Z")))
	f.fake.delay = 5 * time.Millisecond
	got := runProbe(t, f, true)
	if !strings.Contains(got, "bytes=1048576") {
		t.Errorf("the fetched message did not arrive whole:\n%s", got)
	}
}

func TestProbeExplainsAFetchThatHoldsNoMIMEAndTheTraceShowsWhatCame(t *testing.T) {
	// Real-Exchange finding 3: "Fetch: upstream: item has no MIME body" said nothing, and the trace could
	// not tell an inline string from a binary block. An answer with a plain-text body instead of the MIME:
	f := traceFixture(t)
	plain := "Subject: " + secretSubject + "\r\n\r\n" + secretBody
	f.fake.on("ItemOperations", always(0, fetchResponse("5", secretItemID+"1", "1", messageProperties(plain, true, 0, "2026-10-01T10:00:00.000Z",
		e(wbxml.PageAirSyncBase, "Body", e(wbxml.PageAirSyncBase, "Type", txt("1")), e(wbxml.PageAirSyncBase, "EstimatedDataSize", txt("55")), mimeData(plain)), false))))
	var out bytes.Buffer
	err := easupstream.Probe(ctx, easupstream.ProbeConfig{SkipContacts: true,
		Config: easupstream.Config{URL: f.fake.url(), HTTPClient: f.fake.srv.Client(), Version: "16.1"},
		User:   user, Password: pass, DeviceID: dev, Out: &out, Trace: true,
	})
	if err == nil {
		t.Fatal("probe succeeded")
	}
	got := out.String()
	for _, want := range []string{"fetch 1 mime FAILED", "holds no MIME body the bridge can read", "whether the Data came as text or as binary",
		"<airsyncbase:Type>1</airsyncbase:Type>", " text bytes not shown]"} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{secretSubject, secretBody} {
		if strings.Contains(got, bad) {
			t.Errorf("report leaks %q", bad)
		}
	}
}
