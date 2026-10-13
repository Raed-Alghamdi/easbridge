package easupstream

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/hstern/go-activesync/wbxml"
)

func el(page byte, name string, kids ...wbxml.Node) *wbxml.Element {
	return wbxml.E(page, name, kids...)
}
func tx(s string) wbxml.Node { return wbxml.Text(s) }

func render(root *wbxml.Element) string {
	return renderXML(root, wbxml.DefaultRegistry().TolerateUnknownTags())
}

func TestRenderShowsStructureAndOnlyTheValuesOnTheList(t *testing.T) {
	root := el(wbxml.PageAirSync, "Sync", el(wbxml.PageAirSync, "Collections", el(wbxml.PageAirSync, "Collection",
		el(wbxml.PageAirSync, "SyncKey", tx("{GUID}1")),
		el(wbxml.PageAirSync, "GetChanges"),
		el(wbxml.PageAirSync, "Commands", el(wbxml.PageAirSync, "Add",
			el(wbxml.PageAirSync, "ServerId", tx("5:42")),
			el(wbxml.PageAirSync, "ApplicationData",
				el(wbxml.PageEmail, "Subject", tx("Quarterly <numbers> & more")),
				el(wbxml.PageEmail, "Read", tx("1")),
				el(wbxml.PageAirSyncBase, "Body",
					el(wbxml.PageAirSyncBase, "Type", tx("4")),
					el(wbxml.PageAirSyncBase, "Data", wbxml.Opaque("secret body")))))))))
	got := render(root)
	want := `<Sync>
  <Collections>
    <Collection>
      <SyncKey>{GUID}1</SyncKey>
      <GetChanges/>
      <Commands>
        <Add>
          <ServerId>[4 text bytes not shown]</ServerId>
          <ApplicationData>
            <email:Subject>[26 text bytes not shown]</email:Subject>
            <email:Read>1</email:Read>
            <airsyncbase:Body>
              <airsyncbase:Type>4</airsyncbase:Type>
              <airsyncbase:Data>[11 binary bytes not shown]</airsyncbase:Data>
            </airsyncbase:Body>
          </ApplicationData>
        </Add>
      </Commands>
    </Collection>
  </Collections>
</Sync>
`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	for _, bad := range []string{"Quarterly", "numbers", "secret", "5:42"} {
		if strings.Contains(got, bad) {
			t.Errorf("shows %q", bad)
		}
	}
}

// The first real Fetch failed because the MIME of a Type 4 body came as an inline
// string where the library expected an OPAQUE block, and the trace said only
// "72024 bytes not shown" for both. It now says which it is, still without a byte of
// the content.
func TestRenderSaysWhetherAHiddenValueIsTextOrBinary(t *testing.T) {
	body := "Subject: private\r\n\r\nthe text of the message"
	data := func(kids ...wbxml.Node) string {
		return render(el(wbxml.PageAirSync, "ApplicationData", el(wbxml.PageAirSyncBase, "Body", el(wbxml.PageAirSyncBase, "Data", kids...))))
	}
	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{"an inline string", data(tx(body)), fmt.Sprintf("<airsyncbase:Data>[%d text bytes not shown]</airsyncbase:Data>", len(body))},
		{"an OPAQUE block", data(wbxml.Opaque(body)), fmt.Sprintf("<airsyncbase:Data>[%d binary bytes not shown]</airsyncbase:Data>", len(body))},
		{"both, in chunks", data(tx("abc"), wbxml.Opaque("defgh")), "<airsyncbase:Data>[3 text + 5 binary bytes not shown]</airsyncbase:Data>"},
		{"two inline strings", data(tx("abc"), tx("de")), "<airsyncbase:Data>[5 text bytes not shown]</airsyncbase:Data>"},
		{"an empty block", data(), "<airsyncbase:Data/>"},
	} {
		if !strings.Contains(tc.got, tc.want) {
			t.Errorf("%s: want %s in\n%s", tc.name, tc.want, tc.got)
		}
		for _, bad := range []string{"private", "the text", "defgh"} {
			if strings.Contains(tc.got, bad) {
				t.Errorf("%s: shows %q", tc.name, bad)
			}
		}
	}
	// A binary block in an element that is on the list is still not shown.
	if got := render(el(wbxml.PageAirSync, "ApplicationData", el(wbxml.PageEmail, "Read", wbxml.Opaque("1")))); !strings.Contains(got, "<email:Read>[1 binary bytes not shown]</email:Read>") {
		t.Errorf("a listed element holding a binary block: %s", got)
	}
}

// Nothing is shown just because it is in a place where a value is expected: the
// list decides, per page and name, so a new element of a later version, an unknown
// tag or an element on a page that is not listed stays hidden.
func TestRenderHidesWhatIsNotOnTheList(t *testing.T) {
	for name, root := range map[string]*wbxml.Element{
		"a folder name":               el(wbxml.PageFolderHierarchy, "FolderSync", el(wbxml.PageFolderHierarchy, "DisplayName", tx("Divorce lawyer"))),
		"a user name in a Fetch":      el(wbxml.PageItemOperations, "ItemOperations", el(wbxml.PageItemOperations, "UserName", tx("alice@example.test"))),
		"a password in a Fetch":       el(wbxml.PageItemOperations, "ItemOperations", el(wbxml.PageItemOperations, "Password", tx("hunter2"))),
		"a message in SendMail":       el(wbxml.PageComposeMail, "SendMail", el(wbxml.PageComposeMail, "Mime", tx("Subject: private"))),
		"an OOF message":              el(wbxml.PageSettings, "Settings", el(wbxml.PageSettings, "ReplyMessage", tx("I am away"))),
		"an address of the user":      el(wbxml.PageSettings, "Settings", el(wbxml.PageSettings, "SmtpAddress", tx("alice@example.test"))),
		"a phone number":              el(wbxml.PageSettings, "DeviceInformation", el(wbxml.PageSettings, "PhoneNumber", tx("+1 555500000000"))),
		"an IMEI":                     el(wbxml.PageSettings, "DeviceInformation", el(wbxml.PageSettings, "IMEI", tx("490154203237518"))),
		"a contact":                   el(wbxml.PageContacts, "FirstName", tx("Carol")),
		"a calendar subject":          el(wbxml.PageCalendar, "Subject", tx("Merger talks")),
		"an element nobody knows":     el(wbxml.PageAirSync, "Unknown_0_0x3E", tx("surprise")),
		"a page nobody registered":    el(99, "Unknown_99_0x08", tx("surprise")),
		"a preview of a body":         el(wbxml.PageAirSyncBase, "Preview", tx("Dear Bob, the numbers")),
		"a categories list":           el(wbxml.PageEmail, "Category", tx("Private")),
		"an attachment name":          el(wbxml.PageAirSyncBase, "DisplayName", tx("salary.xlsx")),
		"the client id of a message":  el(wbxml.PageComposeMail, "ClientId", tx("abc")),
		"the id of a moved message":   el(wbxml.PageMove, "SrcMsgId", tx("5:1")),
		"the name in a GAL result":    el(wbxml.PageGAL, "DisplayName", tx("Carol Smith")),
		"a find query":                el(wbxml.PageFind, "FreeText", tx("divorce")),
		"a thread topic":              el(wbxml.PageEmail, "ThreadTopic", tx("Re: merger")),
		"the date a message arrived":  el(wbxml.PageEmail, "DateReceived", tx("2026-10-01T10:00:00.000Z")),
		"the conversation id":         el(wbxml.PageEmail2, "ConversationId", tx("abcdef")),
		"the folder id of a folder":   el(wbxml.PageFolderHierarchy, "ServerId", tx("5")),
		"the parent id of a folder":   el(wbxml.PageFolderHierarchy, "ParentId", tx("0")),
		"the display-to of a message": el(wbxml.PageEmail, "DisplayTo", tx("Carol")),
		"the Bcc recipients":          el(wbxml.PageEmail2, "Bcc", tx(`"Eve" <hidden-recipient@example.test>`)),
	} {
		got := render(root)
		if !strings.Contains(got, "bytes not shown]") {
			t.Errorf("%s: not hidden:\n%s", name, got)
		}
		for _, v := range []string{"Divorce", "alice", "hunter2", "private", "away", "+1 555", "490154", "Carol", "Merger", "surprise", "Dear Bob", "Private", "salary", "abc", "5:1", "divorce", "merger", "2026-10", "abcdef", "hidden-recipient", "Eve"} {
			if strings.Contains(got, v) {
				t.Errorf("%s: %q is shown:\n%s", name, v, got)
			}
		}
	}
}

func TestRenderIsSafeToPrint(t *testing.T) {
	// A shown value is bounded and has no control characters or markup of its own.
	long := strings.Repeat("é", 300)
	got := render(el(wbxml.PageSettings, "DeviceInformation", el(wbxml.PageSettings, "Set",
		el(wbxml.PageSettings, "Model", tx("a<b>&c\x00d\x1be")),
		el(wbxml.PageSettings, "FriendlyName", tx(long)))))
	if !strings.Contains(got, "<settings:Model>") && !strings.Contains(got, "<Model>") {
		t.Fatalf("model missing:\n%s", got)
	}
	if strings.ContainsAny(got, "\x00\x1b") || !strings.Contains(got, "a&lt;b&gt;&amp;c?d?e") {
		t.Errorf("unsafe text:\n%q", got)
	}
	if !strings.Contains(got, "more bytes not shown]") || strings.Count(got, "é") > maxShown {
		t.Errorf("a long value is not bounded:\n%s", got)
	}
	// Bounding must not cut a character in two.
	if strings.ContainsRune(got, 0xFFFD) || strings.Contains(got, "?[") && strings.Contains(got, "\xc3[") {
		t.Errorf("a character was cut:\n%s", got)
	}
}

func TestDescribeMessageHandlesWhatAServerMaySend(t *testing.T) {
	reg := wbxml.DefaultRegistry().TolerateUnknownTags()
	wb, err := wbxml.Marshal(&wbxml.Document{Root: el(wbxml.PageAirSync, "Sync", el(wbxml.PageAirSync, "Status", tx("4")))}, wbxml.DefaultRegistry())
	if err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(wb)
	zw.Close()

	lines := func(enc string, body []byte, tooBig bool, total int) string {
		return strings.Join(describeMessage("head", enc, body, tooBig, total, reg), "\n")
	}
	if got := lines("", wb, false, len(wb)); !strings.Contains(got, "<Status>4</Status>") {
		t.Errorf("plain: %s", got)
	}
	if got := lines("gzip", gz.Bytes(), false, gz.Len()); !strings.Contains(got, "<Status>4</Status>") {
		t.Errorf("gzip: %s", got)
	}
	if got := lines("gzip", []byte("not gzip"), false, 8); !strings.Contains(got, "not readable") {
		t.Errorf("broken gzip: %s", got)
	}
	// An HTML page of a gateway: its size, not its text (it can hold names and session data).
	html := []byte("<html><body>Hello alice@example.test, your session abc123 expired</body></html>")
	if got := lines("", html, false, len(html)); !strings.Contains(got, "not a WBXML message") || strings.Contains(got, "alice") || strings.Contains(got, "abc123") {
		t.Errorf("html: %s", got)
	}
	if got := lines("", nil, false, 0); !strings.Contains(got, "(empty body)") {
		t.Errorf("empty: %s", got)
	}
	if got := lines("", wb, true, 9<<20); !strings.Contains(got, "too large to show") {
		t.Errorf("large: %s", got)
	}
	// An unknown tag inside a message: the rest is still shown, the unknown one by name.
	raw := []byte{0x03, 0x01, 0x6A, 0x00, 0x45, 0x4E, 0x03, '4', 0x00, 0x01, 0x00, 0x11, 0x7E, 0x03, 'x', 0x00, 0x01, 0x01}
	if got := lines("", raw, false, len(raw)); !strings.Contains(got, "<Status>4</Status>") || !strings.Contains(got, "Unknown_17_0x3E") || strings.Contains(got, ">x<") {
		t.Errorf("unknown tag: %s", got)
	}
}

func TestDescribeResponseShowsTheHeadersThatNameWhatTheServerSpeaksAndNoCookie(t *testing.T) {
	resp := &http.Response{StatusCode: 200, Header: http.Header{}, Request: &http.Request{Method: http.MethodOptions}}
	resp.Header.Set("MS-ASProtocolVersions", "14.1,16.0,16.1")
	resp.Header.Set("MS-ASProtocolCommands", strings.Repeat("Sync,", 100)+"Ping") // longer than an ordinary value
	resp.Header.Set("MS-Server-ActiveSync", "15.2")
	resp.Header.Set("Set-Cookie", "GatewaySession=secret-session; Path=/")
	resp.Header.Set("X-Forwarded-For", "10.1.2.3")
	got := strings.Join(describeResponse(resp, nil, 0, wbxml.DefaultRegistry()), "\n")
	for _, want := range []string{"HTTP 200", "MS-ASProtocolVersions=14.1,16.0,16.1", "MS-Server-ActiveSync=15.2", ",Ping"} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %q: %s", want, got)
		}
	}
	for _, bad := range []string{"secret-session", "GatewaySession", "10.1.2.3", "Cookie"} {
		if strings.Contains(got, bad) {
			t.Errorf("shows %q: %s", bad, got)
		}
	}
	if strings.Contains(got, "(empty body)") {
		t.Errorf("OPTIONS has no body: %s", got)
	}
}

// Contacts and the Global Address List (v0.2.0): a contact is all personal data, and a
// GAL search names a person. None of their values may appear in a trace; the structure,
// the statuses, the store, the range and the totals do.
func TestRenderHidesContactsAndTheGlobalAddressList(t *testing.T) {
	contact := el(wbxml.PageAirSync, "Sync", el(wbxml.PageAirSync, "Collections", el(wbxml.PageAirSync, "Collection",
		el(wbxml.PageAirSync, "SyncKey", tx("7")),
		el(wbxml.PageAirSync, "Status", tx("1")),
		el(wbxml.PageAirSync, "Commands", el(wbxml.PageAirSync, "Add",
			el(wbxml.PageAirSync, "ServerId", tx("9:77")),
			el(wbxml.PageAirSync, "ApplicationData",
				el(wbxml.PageContacts, "FirstName", tx("Noorhan")),
				el(wbxml.PageContacts, "Email1Address", tx("noorhan@example.test")),
				el(wbxml.PageContacts, "MobilePhoneNumber", tx("+1 555 050 4567")),
				el(wbxml.PageContacts, "Birthday", tx("1990-05-15T00:00:00.000Z")),
				el(wbxml.PageContacts, "Picture", tx("QUJD")),
				el(wbxml.PageContacts, "Categories", el(wbxml.PageContacts, "Category", tx("Secret"))),
				el(wbxml.PageContacts2, "GovernmentId", tx("1234567890")),
				el(wbxml.PageContacts2, "NickName", tx("Zuzu"))))))))
	got := render(contact)
	for _, bad := range []string{"Noorhan", "noorhan", "+1 555", "1990", "QUJD", "Secret", "1234567890", "Zuzu", "9:77"} {
		if strings.Contains(got, bad) {
			t.Errorf("the contact trace shows %q:\n%s", bad, got)
		}
	}
	for _, want := range []string{"<SyncKey>7</SyncKey>", "<contacts:FirstName>[7 text bytes not shown]</contacts:FirstName>", "contacts2:GovernmentId", "<Status>1</Status>"} {
		if !strings.Contains(got, want) {
			t.Errorf("the contact trace lacks %q:\n%s", want, got)
		}
	}

	query := el(wbxml.PageSearch, "Search", el(wbxml.PageSearch, "Store",
		el(wbxml.PageSearch, "Name", tx("GAL")), el(wbxml.PageSearch, "Query", tx("pat.private@corp.example")),
		el(wbxml.PageSearch, "Options", el(wbxml.PageSearch, "Range", tx("0-99")))))
	got = render(query)
	if strings.Contains(got, "pat.private") || !strings.Contains(got, "<Name>GAL</Name>") || !strings.Contains(got, "<Range>0-99</Range>") || !strings.Contains(got, "<Query>[") {
		t.Errorf("the search request:\n%s", got)
	}
	answer := el(wbxml.PageSearch, "Search", el(wbxml.PageSearch, "Status", tx("1")),
		el(wbxml.PageSearch, "Response", el(wbxml.PageSearch, "Store",
			el(wbxml.PageSearch, "Status", tx("1")),
			el(wbxml.PageSearch, "Result", el(wbxml.PageSearch, "Properties",
				el(wbxml.PageGAL, "DisplayName", tx("Pat Private")),
				el(wbxml.PageGAL, "EmailAddress", tx("pat.private@corp.example")),
				el(wbxml.PageGAL, "HomePhone", tx("+1 555 055 0000")),
				el(wbxml.PageGAL, "Alias", tx("pprivate")))),
			el(wbxml.PageSearch, "Range", tx("0-0")), el(wbxml.PageSearch, "Total", tx("1")))))
	got = render(answer)
	for _, bad := range []string{"Pat Private", "pat.private", "+1 555", "pprivate", "corp.example"} {
		if strings.Contains(got, bad) {
			t.Errorf("the search answer shows %q:\n%s", bad, got)
		}
	}
	for _, want := range []string{"<Range>0-0</Range>", "<Total>1</Total>", "<gal:EmailAddress>[24 text bytes not shown]</gal:EmailAddress>"} {
		if !strings.Contains(got, want) {
			t.Errorf("the search answer lacks %q:\n%s", want, got)
		}
	}
}
