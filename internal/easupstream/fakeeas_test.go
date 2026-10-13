package easupstream_test

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"io"
	stdmime "mime"
	"net/http"
	"net/http/httptest"
	stdmail "net/mail"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-message/charset"
	"github.com/hstern/go-activesync/wbxml"

	"easbridge/internal/testgal"
)

const (
	user = "alice@example.test"
	pass = "pw-eas-test-correct-horse"
	dev  = "0123456789abcdef0123456789abcdef"
)

// req is one request the fake Exchange saw.
type req struct {
	Path    string
	Method  string
	Cmd     string
	Query   string
	Version string // MS-ASProtocolVersion
	Policy  string // X-MS-PolicyKey
	Auth    string
	Cookie  string
	Agent   string         // User-Agent
	Root    *wbxml.Element // decoded body, nil for OPTIONS or an empty body
}

// handler answers one command. Return a nil element for an empty 200 body.
type handler func(r *http.Request, in *wbxml.Element) (status int, out *wbxml.Element)

// fakeEAS is a WBXML-speaking stand-in for an Exchange ActiveSync endpoint,
// good enough to drive the adapter's Provision, FolderSync, Sync, Fetch, Ping,
// SendMail, MoveItems and folder paths. Tests install handlers per command.
type fakeEAS struct {
	t               *testing.T
	srv             *httptest.Server
	mu              sync.Mutex
	handlers        map[string]handler
	reqs            []req
	versions        string          // MS-ASProtocolVersions header of OPTIONS
	status          int             // when non-zero, OPTIONS answers with this HTTP status
	cookie          string          // when set ("name=value"), every response sets this session cookie
	retryAfter      string          // when set, every response carries this Retry-After header
	redirectOptions string          // when set, OPTIONS of any other path is redirected (307) to this path
	delay           time.Duration   // when set, every request waits this long before it is answered
	violations      []string        // requests the model refused as protocol errors (see requestViolation)
	html            map[string]bool // commands answered with an HTML page and status 200 (a gateway's logon page)
	// expectViolations: the test sends requests that break the rules on purpose.
	expectViolations bool
	inflight         map[string]int
	maxFly           map[string]int
}

func newFake(t *testing.T) *fakeEAS {
	f := &fakeEAS{t: t, handlers: map[string]handler{}, versions: "2.5,12.0,12.1,14.0,14.1,16.0,16.1",
		inflight: map[string]int{}, maxFly: map[string]int{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	t.Cleanup(func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.violations) > 0 && !f.expectViolations {
			t.Errorf("the model refused %d request(s) as protocol errors: %v", len(f.violations), f.violations)
		}
	})
	// Sensible defaults: an account without trouble.
	f.on("Provision", provisionOK("TEMPKEY", "FINALKEY"))
	f.on("Settings", always(0, e(wbxml.PageSettings, "Settings", e(wbxml.PageSettings, "Status", txt("1")))))
	return f
}

// answerHTML makes the model answer cmd with an HTML page (status 200), like a gateway that wants a logon.
func (f *fakeEAS) answerHTML(cmd string, on bool) {
	f.mu.Lock()
	if f.html == nil {
		f.html = map[string]bool{}
	}
	f.html[cmd] = on
	f.mu.Unlock()
}

func (f *fakeEAS) url() string { return f.srv.URL + "/Microsoft-Server-ActiveSync" }

func (f *fakeEAS) on(cmd string, h handler) {
	f.mu.Lock()
	f.handlers[cmd] = h
	f.mu.Unlock()
}

func (f *fakeEAS) requests(cmd string) []req {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []req
	for _, r := range f.reqs {
		if r.Cmd == cmd {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeEAS) count(cmd string) int { return len(f.requests(cmd)) }

func (f *fakeEAS) all() []req {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]req(nil), f.reqs...)
}

func (f *fakeEAS) last(cmd string) req {
	rs := f.requests(cmd)
	if len(rs) == 0 {
		f.t.Fatalf("no %s request was made", cmd)
	}
	return rs[len(rs)-1]
}

func (f *fakeEAS) maxInflight(cmd string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxFly[cmd]
}

func (f *fakeEAS) serve(w http.ResponseWriter, r *http.Request) {
	rec := req{Path: r.URL.Path, Method: r.Method, Cmd: r.URL.Query().Get("Cmd"), Query: r.URL.RawQuery,
		Version: r.Header.Get("MS-ASProtocolVersion"), Policy: r.Header.Get("X-MS-PolicyKey"), Auth: r.Header.Get("Authorization"),
		Cookie: r.Header.Get("Cookie"), Agent: r.Header.Get("User-Agent")}
	body, _ := io.ReadAll(r.Body)
	if len(body) > 0 {
		if doc, err := wbxml.Unmarshal(body, wbxml.DefaultRegistry()); err == nil {
			rec.Root = doc.Root
		}
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, rec)
	h := f.handlers[rec.Cmd]
	if rec.Cmd != "" {
		f.inflight[rec.Cmd]++
		if f.inflight[rec.Cmd] > f.maxFly[rec.Cmd] {
			f.maxFly[rec.Cmd] = f.inflight[rec.Cmd]
		}
	}
	f.mu.Unlock()
	defer func() {
		if rec.Cmd != "" {
			f.mu.Lock()
			f.inflight[rec.Cmd]--
			f.mu.Unlock()
		}
	}()

	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.retryAfter != "" {
		w.Header().Set("Retry-After", f.retryAfter)
	}
	if f.cookie != "" {
		name, value, _ := strings.Cut(f.cookie, "=")
		http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", Secure: true, HttpOnly: true})
	}
	if r.Method == http.MethodOptions {
		if f.redirectOptions != "" && r.URL.Path != f.redirectOptions {
			w.Header().Set("Location", f.redirectOptions)
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		if f.status != 0 {
			w.WriteHeader(f.status)
			return
		}
		w.Header().Set("MS-ASProtocolVersions", f.versions)
		w.Header().Set("MS-ASProtocolCommands", "Sync,SendMail,SmartForward,SmartReply,GetAttachment,GetHierarchy,CreateCollection,DeleteCollection,MoveCollection,FolderSync,FolderCreate,FolderDelete,FolderUpdate,MoveItems,GetItemEstimate,MeetingResponse,Search,Settings,Ping,ItemOperations,Provision,ResolveRecipients,ValidateCert")
		w.WriteHeader(http.StatusOK)
		return
	}
	if !strings.HasPrefix(rec.Auth, "Basic ") {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if h == nil {
		f.t.Errorf("fake EAS: no handler for %q", rec.Cmd)
		w.WriteHeader(http.StatusNotImplemented)
		return
	}
	// Exchange checks the request before it does anything with it.
	if rec.Root != nil {
		if why := requestViolation(rec.Cmd, rec.Version, rec.Root); why != "" {
			f.t.Logf("fake EAS: %s request refused (%s)", rec.Cmd, why)
			f.mu.Lock()
			f.violations = append(f.violations, rec.Cmd+": "+why)
			f.mu.Unlock()
			b, _ := wbxml.Marshal(&wbxml.Document{Root: violationResponse(rec.Cmd)}, wbxml.DefaultRegistry())
			w.Header().Set("Content-Type", "application/vnd.ms-sync.wbxml")
			w.WriteHeader(http.StatusOK)
			w.Write(b)
			return
		}
	}
	f.mu.Lock()
	page := f.html[rec.Cmd]
	f.mu.Unlock()
	if page {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("<html><body><form action=\"/logon\">Sign in</form></body></html>"))
		return
	}
	status, out := h(r, rec.Root)
	if status == 0 {
		status = http.StatusOK
	}
	if out == nil {
		w.WriteHeader(status)
		return
	}
	b, err := wbxml.Marshal(&wbxml.Document{Root: out}, wbxml.DefaultRegistry())
	if err != nil {
		f.t.Errorf("fake EAS: cannot encode response: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.ms-sync.wbxml")
	w.WriteHeader(status)
	w.Write(b)
}

// ---- response builders -----------------------------------------------------

func e(page byte, name string, kids ...wbxml.Node) *wbxml.Element {
	return wbxml.E(page, name, kids...)
}
func txt(s string) wbxml.Node { return wbxml.Text(s) }

func status(page byte, name, code string) *wbxml.Element {
	return e(page, name, e(page, "Status", txt(code)))
}

// provisionOK answers the two-phase handshake the way Exchange 2019 does, which
// includes the rule that made the first real probe fail: from protocol 14.1 on
// the policy download (the request without a PolicyKey) must carry
// settings:DeviceInformation with a Set that holds at least a Model
// (MS-ASPROV 2.2.2.53), and a request that does not gets Provision status 165,
// DeviceInformationRequired. A separate Settings command does not help. Every test
// that provisions through the default handler therefore fails against code that
// leaves the device information out of the Provision request.
//
// The acknowledgement needs nothing of the sort; the model does not mind a
// Settings command either (it answers it like a server that does not care).
func provisionOK(temp, final string) handler {
	return func(r *http.Request, in *wbxml.Element) (int, *wbxml.Element) {
		initial := in != nil && in.Find("PolicyKey") == nil
		key := final
		if initial {
			key = temp
		}
		var answer []wbxml.Node
		if initial && deviceInformationRequired(r.Header.Get("MS-ASProtocolVersion")) {
			if !hasDeviceModel(in) {
				return 0, e(wbxml.PageProvision, "Provision", e(wbxml.PageProvision, "Status", txt("165")))
			}
			answer = append(answer, e(wbxml.PageSettings, "DeviceInformation", e(wbxml.PageSettings, "Status", txt("1"))))
		}
		return 0, e(wbxml.PageProvision, "Provision", append(answer,
			e(wbxml.PageProvision, "Status", txt("1")),
			e(wbxml.PageProvision, "Policies",
				e(wbxml.PageProvision, "Policy",
					e(wbxml.PageProvision, "PolicyType", txt("MS-EAS-Provisioning-WBXML")),
					e(wbxml.PageProvision, "Status", txt("1")),
					e(wbxml.PageProvision, "PolicyKey", txt(key)))))...)
	}
}

// deviceInformationRequired reports whether protocol version v (14.1, 16.0, 16.1)
// makes the device information mandatory in the initial Provision request. It is
// written out here on purpose: the model must not share code with the client.
func deviceInformationRequired(v string) bool {
	maj, min, ok := strings.Cut(strings.TrimSpace(v), ".")
	if !ok {
		return false
	}
	major, err1 := strconv.Atoi(maj)
	minor, err2 := strconv.Atoi(min)
	return err1 == nil && err2 == nil && (major > 14 || (major == 14 && minor >= 1))
}

// hasDeviceModel reports whether the request carries settings:DeviceInformation
// as a child of Provision, in front of Policies, with a Set that holds a Model.
func hasDeviceModel(in *wbxml.Element) bool {
	if in == nil || in.Name != "Provision" {
		return false
	}
	var seenPolicies bool
	for _, k := range in.Children {
		el, ok := k.(*wbxml.Element)
		if !ok {
			continue
		}
		switch {
		case el.Name == "Policies":
			seenPolicies = true
		case el.Name == "DeviceInformation" && el.Codepage == wbxml.PageSettings:
			if seenPolicies {
				return false // behind Policies: not where the specification puts it
			}
			set := el.Find("Set")
			model := set.Find("Model")
			return set != nil && model != nil && strings.TrimSpace(model.TextContent()) != ""
		}
	}
	return false
}

func always(status int, out *wbxml.Element) handler {
	return func(*http.Request, *wbxml.Element) (int, *wbxml.Element) { return status, out }
}

func folderSyncResp(key string, folders ...*wbxml.Element) *wbxml.Element {
	changes := e(wbxml.PageFolderHierarchy, "Changes", e(wbxml.PageFolderHierarchy, "Count", txt(itoa(len(folders)))))
	for _, f := range folders {
		changes.Children = append(changes.Children, f)
	}
	return e(wbxml.PageFolderHierarchy, "FolderSync",
		e(wbxml.PageFolderHierarchy, "Status", txt("1")),
		e(wbxml.PageFolderHierarchy, "SyncKey", txt(key)), changes)
}

func folderAdd(id, parent, name string, typ int) *wbxml.Element {
	return e(wbxml.PageFolderHierarchy, "Add",
		e(wbxml.PageFolderHierarchy, "ServerId", txt(id)),
		e(wbxml.PageFolderHierarchy, "ParentId", txt(parent)),
		e(wbxml.PageFolderHierarchy, "DisplayName", txt(name)),
		e(wbxml.PageFolderHierarchy, "Type", txt(itoa(typ))))
}

func itoa(n int) string {
	var b bytes.Buffer
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	b.Write(d)
	return b.String()
}

// syncResp builds a Sync response for one collection.
func syncResp(folder, key string, status string, kids ...*wbxml.Element) *wbxml.Element {
	coll := e(wbxml.PageAirSync, "Collection",
		e(wbxml.PageAirSync, "SyncKey", txt(key)),
		e(wbxml.PageAirSync, "CollectionId", txt(folder)),
		e(wbxml.PageAirSync, "Status", txt(status)))
	for _, k := range kids {
		coll.Children = append(coll.Children, k)
	}
	return e(wbxml.PageAirSync, "Sync", e(wbxml.PageAirSync, "Collections", coll))
}

func commands(kids ...*wbxml.Element) *wbxml.Element {
	c := e(wbxml.PageAirSync, "Commands")
	for _, k := range kids {
		c.Children = append(c.Children, k)
	}
	return c
}

// ---- what Exchange 2019 sends for a message (v0.1.7, real-Exchange finding 3) ----
//
// The first real Fetch failed with "item has no MIME body" although Exchange had
// answered with the message: it sends the MIME of a Type 4 body as an inline string
// (WBXML STR_I), and the library took only an OPAQUE chunk for MIME. The model used
// OPAQUE everywhere, so no test could notice. It now answers like Exchange 2019, with
// the elements in the order of the trace: the headers, Body, MessageClass,
// InternetCPID, an empty Flag, ContentClass, NativeBodyType, ConversationId and
// ConversationIndex as binary, an empty Categories, IsDraft; in an ItemOperations
// Fetch the Class element comes in front of Properties.

// mimeData is airsyncbase:Data as Exchange 2019 sends a Type 4 body: an inline string.
func mimeData(raw string) *wbxml.Element { return e(wbxml.PageAirSyncBase, "Data", txt(raw)) }

// opaqueData is the other form a server may use (and the one the library used to
// expect): the same bytes as OPAQUE.
func opaqueData(raw string) *wbxml.Element {
	return e(wbxml.PageAirSyncBase, "Data", wbxml.Opaque(raw))
}

// nodes converts elements to the variadic argument of e.
func nodes(els []*wbxml.Element) []wbxml.Node {
	out := make([]wbxml.Node, len(els))
	for i, el := range els {
		out[i] = el
	}
	return out
}

var wordDecoder = &stdmime.WordDecoder{CharsetReader: charset.Reader}

// fieldOf returns the unfolded, RFC 2047 decoded value of the first header field
// called name in raw ("" if there is none). Messages that are cut off or are not
// messages at all ("stub") give "".
func fieldOf(raw, name string) string {
	head, _, _ := strings.Cut(raw, "\r\n\r\n")
	var val string
	found := false
	for _, line := range strings.Split(head, "\r\n") {
		if found {
			if line != "" && (line[0] == ' ' || line[0] == '\t') {
				val += " " + strings.TrimSpace(line)
				continue
			}
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(k), name) {
			found, val = true, strings.TrimSpace(v)
		}
	}
	if d, err := wordDecoder.DecodeHeader(val); err == nil {
		return d
	}
	return val
}

var charsetParam = regexp.MustCompile(`(?i)charset="?([A-Za-z0-9_.:-]+)`)

// internetCPID is the code page Exchange reports for a message: from the charset of
// the top level Content-Type.
func internetCPID(raw string) string {
	m := charsetParam.FindStringSubmatch(fieldOf(raw, "Content-Type"))
	if m == nil {
		return "65001"
	}
	switch strings.ToLower(m[1]) {
	case "windows-1256":
		return "1256"
	case "iso-8859-6":
		return "28596"
	case "us-ascii":
		return "20127"
	case "iso-8859-1":
		return "28591"
	}
	return "65001"
}

// displayTo is what Exchange puts in DisplayTo: the names of the recipients.
func displayTo(to string) string {
	list, err := (&stdmail.AddressParser{WordDecoder: wordDecoder}).ParseList(to)
	if err != nil || len(list) == 0 {
		return to
	}
	var names []string
	for _, a := range list {
		if a.Name != "" {
			names = append(names, a.Name)
		} else {
			names = append(names, a.Address)
		}
	}
	return strings.Join(names, "; ")
}

// messageProperties are the children of ApplicationData (a Sync Add) or of
// Properties (an ItemOperations Fetch) for one message, in the order of the trace.
// body is the finished airsyncbase:Body. withAttachment adds an inline attachment
// (Attachments/Attachment with DisplayName, FileReference, Method, EstimatedDataSize,
// ContentId and IsInline), and with it the Cc and ReplyTo of the Sync Add in the trace.
func messageProperties(raw string, read bool, flag int, date string, body *wbxml.Element, withAttachment bool) []*wbxml.Element {
	em := func(name, v string) *wbxml.Element {
		if v == "" {
			return e(wbxml.PageEmail, name)
		}
		return e(wbxml.PageEmail, name, txt(v))
	}
	to, subject := fieldOf(raw, "To"), fieldOf(raw, "Subject")
	readV := "0"
	if read {
		readV = "1"
	}
	out := []*wbxml.Element{em("To", to)}
	if withAttachment {
		out = append(out, em("Cc", fieldOf(raw, "Cc")))
	}
	out = append(out, em("From", fieldOf(raw, "From")))
	if withAttachment {
		out = append(out, em("ReplyTo", fieldOf(raw, "Reply-To")))
	}
	out = append(out, em("Subject", subject), em("DateReceived", date), em("DisplayTo", displayTo(to)),
		em("ThreadTopic", subject), em("Importance", "1"), em("Read", readV))
	if withAttachment {
		out = append(out, e(wbxml.PageAirSyncBase, "Attachments",
			e(wbxml.PageAirSyncBase, "Attachment",
				e(wbxml.PageAirSyncBase, "DisplayName", txt("logo.png")),
				e(wbxml.PageAirSyncBase, "FileReference", txt("5%3a42%3a0")),
				e(wbxml.PageAirSyncBase, "Method", txt("1")),
				e(wbxml.PageAirSyncBase, "EstimatedDataSize", txt("2048")),
				e(wbxml.PageAirSyncBase, "ContentId", txt("logo@example.test")),
				e(wbxml.PageAirSyncBase, "IsInline", txt("1")))))
	}
	flagEl := e(wbxml.PageEmail, "Flag") // an empty Flag: not flagged
	if flag != 0 {
		flagEl = e(wbxml.PageEmail, "Flag", e(wbxml.PageEmail, "FlagStatus", txt(itoa(flag))))
	}
	sum := sha1.Sum([]byte(raw))
	index := append(append([]byte(nil), sum[:]...), sum[:2]...) // 22 bytes, like a ConversationIndex
	return append(out, body,
		em("MessageClass", "IPM.Note"), em("InternetCPID", internetCPID(raw)), flagEl,
		em("ContentClass", "urn:content-classes:message"),
		e(wbxml.PageAirSyncBase, "NativeBodyType", txt("2")),
		e(wbxml.PageEmail2, "ConversationId", wbxml.Opaque(sum[:16])),
		e(wbxml.PageEmail2, "ConversationIndex", wbxml.Opaque(index)),
		e(wbxml.PageEmail, "Categories"),
		e(wbxml.PageEmail2, "IsDraft", txt("0")))
}

// mimeBody is airsyncbase:Body for a Type 4 body: Type, EstimatedDataSize, Truncated
// (when the server cut the message) and Data.
func mimeBody(data *wbxml.Element, truncated bool, est int) *wbxml.Element {
	body := e(wbxml.PageAirSyncBase, "Body",
		e(wbxml.PageAirSyncBase, "Type", txt("4")),
		e(wbxml.PageAirSyncBase, "EstimatedDataSize", txt(itoa(est))))
	if truncated {
		body.Children = append(body.Children, e(wbxml.PageAirSyncBase, "Truncated", txt("1")))
	}
	body.Children = append(body.Children, data)
	return body
}

// addItem builds a Sync <Add> with a MIME body as Exchange 2019 sends it.
func addItem(id string, raw string, read bool, flag int, date string, truncated bool, est int) *wbxml.Element {
	return addItemData(mimeData, id, raw, raw, read, flag, date, truncated, est)
}

// addItemData is addItem with the form of the Data element chosen by the caller, and
// with the part of the message that is sent (sent: the whole message or its first
// bytes) apart from the message (full) that the header properties come from, as on a
// server, whose Subject and From do not depend on how much of the body fits.
func addItemData(data func(string) *wbxml.Element, id string, full, sent string, read bool, flag int, date string, truncated bool, est int) *wbxml.Element {
	return e(wbxml.PageAirSync, "Add",
		e(wbxml.PageAirSync, "ServerId", txt(id)),
		e(wbxml.PageAirSync, "ApplicationData",
			nodes(messageProperties(full, read, flag, date, mimeBody(data(sent), truncated, est), false))...))
}

// addItemHeadersOnly builds a Sync <Add> for a request that asked for a plain-text
// body cut to one byte (the 5-header sync of the probe): Body Type 1, EstimatedDataSize,
// Truncated and the first byte of the text.
func addItemHeadersOnly(id string, raw string, read bool, flag int, date string, est int) *wbxml.Element {
	_, text, _ := strings.Cut(raw, "\r\n\r\n")
	if len(text) > 1 {
		text = text[:1]
	}
	body := e(wbxml.PageAirSyncBase, "Body",
		e(wbxml.PageAirSyncBase, "Type", txt("1")),
		e(wbxml.PageAirSyncBase, "EstimatedDataSize", txt(itoa(est))),
		e(wbxml.PageAirSyncBase, "Truncated", txt("1")),
		e(wbxml.PageAirSyncBase, "Data", txt(text)))
	return e(wbxml.PageAirSync, "Add",
		e(wbxml.PageAirSync, "ServerId", txt(id)),
		e(wbxml.PageAirSync, "ApplicationData", nodes(messageProperties(raw, read, flag, date, body, true))...))
}

// fetchResponse is the answer to one ItemOperations Fetch: Status, CollectionId, ServerId
// and, when the fetch succeeded, Class in front of Properties (the order of the trace).
// props are the children of Properties.
func fetchResponse(folder, sid, status string, props []*wbxml.Element) *wbxml.Element {
	f := e(wbxml.PageItemOperations, "Fetch",
		e(wbxml.PageItemOperations, "Status", txt(status)),
		e(wbxml.PageAirSync, "CollectionId", txt(folder)),
		e(wbxml.PageAirSync, "ServerId", txt(sid)))
	if status == "1" {
		f.Children = append(f.Children, e(wbxml.PageAirSync, "Class", txt("Email")),
			e(wbxml.PageItemOperations, "Properties", nodes(props)...))
	}
	return e(wbxml.PageItemOperations, "ItemOperations",
		e(wbxml.PageItemOperations, "Status", txt("1")),
		e(wbxml.PageItemOperations, "Response", f))
}

// fetchMIMEResponse is the Fetch answer for a message with the MIME in the form data
// builds (mimeData: an inline string, as Exchange 2019 sends it).
func fetchMIMEResponse(data func(string) *wbxml.Element, folder, sid, raw string, read bool, flag int, date string) *wbxml.Element {
	return fetchResponse(folder, sid, "1", messageProperties(raw, read, flag, date, mimeBody(data(raw), false, len(raw)), false))
}

func pingResp(status string, folders ...string) *wbxml.Element {
	out := e(wbxml.PagePing, "Ping", e(wbxml.PagePing, "Status", txt(status)))
	if len(folders) > 0 {
		fs := e(wbxml.PagePing, "Folders")
		for _, f := range folders {
			fs.Children = append(fs.Children, e(wbxml.PagePing, "Folder", txt(f)))
		}
		out.Children = append(out.Children, fs)
	}
	return out
}

// text finds the first element called name in the request and returns its text.
func text(root *wbxml.Element, name string) (string, bool) {
	if root == nil {
		return "", false
	}
	if el := root.Find(name); el != nil {
		return el.TextContent(), true
	}
	return "", false
}

// ---- what Exchange insists on in a request (v0.1.6, real-Exchange finding 2) ----
//
// The first real Sync failed with status 4, a protocol error: the request that
// asks for a first SyncKey carried GetChanges (MS-ASCMD 2.2.3.84). The model used
// to accept it. Now every Sync, ItemOperations and Ping request is checked against
// the rules of MS-ASCMD (the schemas of section 6, the element pages of 2.2.3 and
// the ordering rules stated there) before the handler of a test sees it, and a
// request that breaks one is answered the way Exchange answers: Sync status 4,
// ItemOperations status 2, Ping status 4. The checks are written out here and share
// no code with the client.

// requestViolation returns what is wrong with a Sync, ItemOperations or Ping
// request, or "".
func requestViolation(cmd, version string, root *wbxml.Element) string {
	switch cmd {
	case "Sync":
		return syncViolation(version, root)
	case "ItemOperations":
		return itemOperationsViolation(version, root)
	case "Ping":
		return pingViolation(root)
	case "Search":
		return searchViolation(root)
	}
	return ""
}

// violationResponse is Exchange's answer to a request it cannot accept.
func violationResponse(cmd string) *wbxml.Element {
	switch cmd {
	case "Sync":
		return e(wbxml.PageAirSync, "Sync", e(wbxml.PageAirSync, "Status", txt("4")))
	case "ItemOperations":
		return e(wbxml.PageItemOperations, "ItemOperations", e(wbxml.PageItemOperations, "Status", txt("2")))
	case "Search":
		return e(wbxml.PageSearch, "Search", e(wbxml.PageSearch, "Status", txt("2")))
	}
	return e(wbxml.PagePing, "Ping", e(wbxml.PagePing, "Status", txt("4")))
}

// kids returns the element children of el.
func kids(el *wbxml.Element) []*wbxml.Element {
	var out []*wbxml.Element
	if el == nil {
		return nil
	}
	for _, n := range el.Children {
		if c, ok := n.(*wbxml.Element); ok {
			out = append(out, c)
		}
	}
	return out
}

func oneOf(s string, list ...string) bool {
	for _, v := range list {
		if s == v {
			return true
		}
	}
	return false
}

// ordered checks that the children of parent appear in the order of rule, each at
// most as often as its entry says (max 0 means once), and returns what is wrong.
func ordered(what string, parent *wbxml.Element, page byte, rule []childRule) string {
	pos := -1
	count := map[string]int{}
	for _, c := range kids(parent) {
		idx := -1
		for i, r := range rule {
			if r.name == c.Name {
				idx = i
			}
		}
		switch {
		case idx < 0:
			return what + ": " + c.Name + " is not allowed here"
		case c.Codepage != rule[idx].page(page):
			return fmt.Sprintf("%s: %s is on code page %d, not %d", what, c.Name, c.Codepage, rule[idx].page(page))
		case idx < pos:
			return what + ": " + c.Name + " is out of order (strict ordering)"
		}
		pos = idx
		count[c.Name]++
		if count[c.Name] > rule[idx].limit() {
			return what + ": " + c.Name + " appears too often"
		}
	}
	for _, r := range rule {
		if r.required && count[r.name] == 0 {
			return what + ": " + r.name + " is required"
		}
	}
	return ""
}

type childRule struct {
	name     string
	pg       *byte // code page of the element, the parent's if nil
	max      int
	required bool
}

func (r childRule) page(parent byte) byte {
	if r.pg != nil {
		return *r.pg
	}
	return parent
}

func (r childRule) limit() int {
	if r.max == 0 {
		return 1
	}
	return r.max
}

func pg(p byte) *byte { return &p }

func syncViolation(version string, root *wbxml.Element) string {
	if root == nil || root.Name != "Sync" {
		return "the request is not a Sync"
	}
	// 6.45 Sync Request Schema: Collections, Wait, HeartbeatInterval, WindowSize, Partial.
	if m := ordered("Sync", root, wbxml.PageAirSync, []childRule{{name: "Collections"}, {name: "Wait"}, {name: "HeartbeatInterval"}, {name: "WindowSize"}, {name: "Partial"}}); m != "" {
		return m
	}
	for _, colls := range kids(root) {
		if colls.Name != "Collections" {
			continue
		}
		if len(kids(colls)) == 0 {
			return "Collections: at least one Collection is required"
		}
		for _, c := range kids(colls) {
			if c.Name != "Collection" {
				return "Collections: " + c.Name + " is not allowed here"
			}
			if m := collectionViolation(version, c); m != "" {
				return m
			}
		}
	}
	return ""
}

func collectionViolation(version string, c *wbxml.Element) string {
	rule := []childRule{
		{name: "SyncKey", required: true}, {name: "CollectionId", required: true}, {name: "Supported"}, {name: "DeletesAsMoves"},
		{name: "GetChanges"}, {name: "WindowSize"}, {name: "ConversationMode"}, {name: "Options", max: 2}, {name: "Commands"},
	}
	// 2.2.3.29.2: "a strict ordering of the XML elements within a Collection"; Class
	// is supported "only when protocol version 2.5, 12.0, or 12.1 is used".
	if !deviceInformationRequired(version) && !oneOf(version, "14.0") {
		rule = append([]childRule{{name: "Class"}}, rule...)
	}
	if m := ordered("Collection", c, wbxml.PageAirSync, rule); m != "" {
		return m
	}
	byName := map[string]*wbxml.Element{}
	for _, k := range kids(c) {
		byName[k.Name] = k
	}
	boolean := func(name string) string {
		if el := byName[name]; el != nil && !oneOf(el.TextContent(), "", "0", "1") {
			return name + " is a boolean, got " + el.TextContent()
		}
		return ""
	}
	for _, n := range []string{"DeletesAsMoves", "GetChanges", "ConversationMode"} {
		if m := boolean(n); m != "" {
			return m
		}
	}
	if len(byName["SyncKey"].TextContent()) > 64 || len(byName["CollectionId"].TextContent()) > 64 {
		return "SyncKey and CollectionId hold at most 64 characters"
	}
	if w := byName["WindowSize"]; w != nil {
		n, err := strconv.Atoi(w.TextContent())
		if err != nil || n < 0 || n > 512 {
			return "WindowSize must be 0 to 512, got " + w.TextContent()
		}
	}
	// 2.2.3.84: status 4 for GetChanges present and empty or 1 with SyncKey 0.
	if byName["SyncKey"].TextContent() == "0" {
		if g := byName["GetChanges"]; g != nil && oneOf(g.TextContent(), "", "1") {
			return "GetChanges is present and empty or 1 with SyncKey 0 (MS-ASCMD 2.2.3.84)"
		}
		// An initial request has nothing to apply: 3.1.5.4 describes it as the SyncKey
		// and the CollectionId.
		if byName["Commands"] != nil {
			return "Commands with SyncKey 0 (MS-ASCMD 3.1.5.4)"
		}
	}
	if cm := byName["Commands"]; cm != nil && len(kids(cm)) == 0 {
		return "Commands holds no operation (2.2.3.32)"
	}
	for _, o := range kids(c) {
		if o.Name == "Options" {
			if m := optionsViolation("Options", o, version, true); m != "" {
				return m
			}
		}
	}
	return ""
}

// optionsViolation checks an Options element of Sync (sync) or of an ItemOperations
// Fetch: the children the schemas allow (6.1 AirSync.xsd, 6.23), how often, their
// values, and the BodyPreference rules of MS-ASAIRS 2.2.2.12.
func optionsViolation(what string, o *wbxml.Element, version string, sync bool) string {
	allowed := map[string]byte{"BodyPreference": wbxml.PageAirSyncBase, "BodyPartPreference": wbxml.PageAirSyncBase, "MIMESupport": wbxml.PageAirSync}
	if sync {
		for _, n := range []string{"FilterType", "Truncation", "Class", "Conflict", "MIMETruncation", "MaxItems"} {
			allowed[n] = wbxml.PageAirSync
		}
		allowed["RightsManagementSupport"] = wbxml.PageRightsManagement
	} else {
		for _, n := range []string{"Schema", "Range", "UserName", "Password"} {
			allowed[n] = wbxml.PageItemOperations
		}
		allowed["RightsManagementSupport"] = wbxml.PageRightsManagement
	}
	if len(kids(o)) == 0 {
		return what + " holds no child (it should hold at least one)"
	}
	count := map[string]int{}
	bodyTypes := map[string]bool{}
	for _, k := range kids(o) {
		page, ok := allowed[k.Name]
		if !ok {
			return what + ": " + k.Name + " is not allowed here"
		}
		if k.Codepage != page {
			return fmt.Sprintf("%s: %s is on code page %d, not %d", what, k.Name, k.Codepage, page)
		}
		count[k.Name]++
		if k.Name == "BodyPartPreference" && !deviceInformationRequired(version) {
			return what + ": BodyPartPreference needs protocol 14.1 or later"
		}
		if k.Name != "BodyPreference" && k.Name != "Schema" && count[k.Name] > 1 {
			return what + ": " + k.Name + " appears more than once"
		}
		num := func(lo, hi int) bool {
			n, err := strconv.Atoi(k.TextContent())
			return err == nil && n >= lo && n <= hi
		}
		switch k.Name {
		case "FilterType":
			// 0 to 5 are the email windows; 8 (incomplete tasks) is status 4 for email.
			if !num(0, 5) {
				return what + ": FilterType " + k.TextContent() + " is not valid for email"
			}
		case "MIMESupport":
			if !num(0, 2) {
				return what + ": MIMESupport must be 0, 1 or 2"
			}
		case "MIMETruncation":
			if !num(0, 8) {
				return what + ": MIMETruncation must be 0 to 8"
			}
		case "BodyPreference":
			if m := bodyPreferenceViolation(k, bodyTypes); m != "" {
				return what + ": " + m
			}
		}
	}
	// MS-ASCMD 2.2.3.110: MIME comes with MIMESupport and a BodyPreference of Type 4.
	if m := count["MIMESupport"]; m > 0 {
		for _, k := range kids(o) {
			if k.Name == "MIMESupport" && k.TextContent() != "0" && !bodyTypes["4"] {
				return what + ": MIMESupport without a BodyPreference of Type 4"
			}
		}
	}
	return ""
}

// bodyPreferenceViolation: Type, TruncationSize, AllOrNone, Preview in this order,
// Type required, one BodyPreference per Type (MS-ASAIRS 2.2.2.12).
func bodyPreferenceViolation(bp *wbxml.Element, seen map[string]bool) string {
	ab := wbxml.PageAirSyncBase
	if m := ordered("BodyPreference", bp, ab, []childRule{{name: "Type", required: true}, {name: "TruncationSize"}, {name: "AllOrNone"}, {name: "Preview"}}); m != "" {
		return m
	}
	typ := kids(bp)[0].TextContent()
	if !oneOf(typ, "1", "2", "3", "4") {
		return "BodyPreference Type must be 1 to 4, got " + typ
	}
	if seen[typ] {
		return "two BodyPreference elements of Type " + typ
	}
	seen[typ] = true
	for _, k := range kids(bp) {
		if k.Name == "TruncationSize" {
			if n, err := strconv.Atoi(k.TextContent()); err != nil || n < 0 {
				return "TruncationSize must be a number"
			}
		}
	}
	return ""
}

func itemOperationsViolation(version string, root *wbxml.Element) string {
	if root == nil || root.Name != "ItemOperations" {
		return "the request is not an ItemOperations"
	}
	if len(kids(root)) == 0 {
		return "ItemOperations holds no operation"
	}
	for _, op := range kids(root) {
		if op.Name != "Fetch" || op.Codepage != wbxml.PageItemOperations {
			return "ItemOperations: only Fetch is checked here, got " + op.Name
		}
		// 6.23: Fetch is an xs:all, so any order; the elements and their namespaces matter.
		allowed := map[string]byte{"Store": wbxml.PageItemOperations, "ServerId": wbxml.PageAirSync, "CollectionId": wbxml.PageAirSync,
			"LinkId": wbxml.PageDocumentLibrary, "LongId": wbxml.PageSearch, "FileReference": wbxml.PageAirSyncBase,
			"Options": wbxml.PageItemOperations, "RemoveRightsManagementProtection": wbxml.PageRightsManagement}
		seen := map[string]*wbxml.Element{}
		for _, k := range kids(op) {
			page, ok := allowed[k.Name]
			if !ok {
				return "Fetch: " + k.Name + " is not allowed here"
			}
			if k.Codepage != page {
				return fmt.Sprintf("Fetch: %s is on code page %d, not %d", k.Name, k.Codepage, page)
			}
			if seen[k.Name] != nil {
				return "Fetch: " + k.Name + " appears twice"
			}
			seen[k.Name] = k
		}
		store := seen["Store"]
		if store == nil || store.TextContent() != "Mailbox" {
			return "Fetch: Store must be Mailbox for an email"
		}
		if seen["ServerId"] == nil || seen["CollectionId"] == nil {
			return "Fetch: an email is fetched by CollectionId and ServerId"
		}
		if o := seen["Options"]; o != nil {
			if m := optionsViolation("Options", o, version, false); m != "" {
				return m
			}
		}
	}
	return ""
}

func pingViolation(root *wbxml.Element) string {
	if root == nil || root.Name != "Ping" {
		return "the request is not a Ping"
	}
	// 6.29: an xs:all of HeartbeatInterval and Folders.
	seen := map[string]bool{}
	for _, k := range kids(root) {
		if !oneOf(k.Name, "HeartbeatInterval", "Folders") || k.Codepage != wbxml.PagePing {
			return "Ping: " + k.Name + " is not allowed here"
		}
		if seen[k.Name] {
			return "Ping: " + k.Name + " appears twice"
		}
		seen[k.Name] = true
		switch k.Name {
		case "HeartbeatInterval":
			if _, err := strconv.Atoi(k.TextContent()); err != nil {
				return "HeartbeatInterval must be an integer"
			}
		case "Folders":
			if len(kids(k)) == 0 {
				return "Folders holds no Folder"
			}
			for _, f := range kids(k) {
				if f.Name != "Folder" {
					return "Folders: " + f.Name + " is not allowed here"
				}
				var id, class string
				for _, x := range kids(f) {
					switch x.Name {
					case "Id":
						id = x.TextContent()
					case "Class":
						class = x.TextContent()
					default:
						return "Folder: " + x.Name + " is not allowed here"
					}
				}
				if id == "" || len(id) > 64 || !oneOf(class, "Email", "Calendar", "Contacts", "Tasks", "Notes") {
					return "Folder needs an Id and a Class of Email, Calendar, Contacts, Tasks or Notes"
				}
			}
		}
	}
	return ""
}

// searchViolation checks a Search request for the Global Address List against
// MS-ASCMD 2.2.3.152 and the schema of 6.37: a Search holds Store elements; a Store
// holds Name, Query and Options in this order (Name and Query are required); a GAL
// query is a text, not a tree; the options of a GAL search are Range, UserName,
// Password and Picture, and a Range is "start-end" with start not above end.
func searchViolation(root *wbxml.Element) string {
	if root == nil || root.Name != "Search" || root.Codepage != wbxml.PageSearch {
		return "the request is not a Search"
	}
	stores := kids(root)
	if len(stores) == 0 {
		return "Search: a Store is required"
	}
	for _, st := range stores {
		if st.Name != "Store" || st.Codepage != wbxml.PageSearch {
			return "Search: " + st.Name + " is not allowed here"
		}
		if m := ordered("Store", st, wbxml.PageSearch, []childRule{{name: "Name", required: true}, {name: "Query", required: true}, {name: "Options"}}); m != "" {
			return m
		}
		var name, query *wbxml.Element
		var options *wbxml.Element
		for _, k := range kids(st) {
			switch k.Name {
			case "Name":
				name = k
			case "Query":
				query = k
			case "Options":
				options = k
			}
		}
		if name.TextContent() != "GAL" {
			return "Store: this model answers the GAL only, got " + name.TextContent()
		}
		if len(kids(query)) != 0 {
			return "Query: a GAL query is text"
		}
		if strings.TrimSpace(query.TextContent()) == "" {
			return "Query: empty"
		}
		if options != nil {
			rangeSeen := false
			for _, o := range kids(options) {
				if !oneOf(o.Name, "Range", "UserName", "Password", "Picture") || o.Codepage != wbxml.PageSearch {
					return "Options: " + o.Name + " is not allowed here"
				}
				if o.Name == "Range" {
					if rangeSeen {
						return "Options: Range twice"
					}
					rangeSeen = true
					if _, _, ok := parseRange(o.TextContent()); !ok {
						return "Range must be start-end with start not above end, got " + o.TextContent()
					}
				}
			}
		}
	}
	return ""
}

var rangeRe = regexp.MustCompile(`^(\d+)-(\d+)$`)

func parseRange(s string) (start, end int, ok bool) {
	m := rangeRe.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, false
	}
	start, _ = strconv.Atoi(m[1])
	end, _ = strconv.Atoi(m[2])
	return start, end, start <= end
}

// ---- Contacts and the Global Address List ------------------------------------------

// contactEl is one element of the Contacts page.
func contactEl(name, v string) *wbxml.Element { return e(wbxml.PageContacts, name, txt(v)) }

// contactAdd is a Sync <Add> of a contact the way Exchange 2019 sends one: fields of
// the Contacts page, the Contacts2 extras, and a Picture as a base64 text.
func contactAdd(id string, fields map[string]string, extras ...*wbxml.Element) *wbxml.Element {
	names := make([]string, 0, len(fields))
	for k := range fields {
		names = append(names, k)
	}
	sort.Strings(names)
	app := e(wbxml.PageAirSync, "ApplicationData")
	for _, k := range names {
		app.Children = append(app.Children, contactEl(k, fields[k]))
	}
	for _, x := range extras {
		app.Children = append(app.Children, x)
	}
	return e(wbxml.PageAirSync, "Add", e(wbxml.PageAirSync, "ServerId", txt(id)), app)
}

// galPerson is an entry of the model's directory.
type galPerson struct {
	display, first, last, email, alias, office, title, company, phone, home, mobile string
}

func (p galPerson) element() *wbxml.Element {
	props := e(wbxml.PageSearch, "Properties")
	add := func(name, v string) {
		if v != "" {
			props.Children = append(props.Children, e(wbxml.PageGAL, name, txt(v)))
		}
	}
	add("DisplayName", p.display)
	add("Phone", p.phone)
	add("Office", p.office)
	add("Title", p.title)
	add("Company", p.company)
	add("Alias", p.alias)
	add("FirstName", p.first)
	add("LastName", p.last)
	add("HomePhone", p.home)
	add("MobilePhone", p.mobile)
	add("EmailAddress", p.email)
	return e(wbxml.PageSearch, "Result", props)
}

// galSearchHandler answers Search for the GAL from people: a prefix match on the
// names, the alias and the local part, and on the whole address when byAddress is
// set; at most 100 entries, the range cut to 0-99 as Exchange does; no Range element
// when nothing matches. queries collects the Query texts.
func galSearchHandler(people []galPerson, byAddress bool, queries *[]string) handler {
	return func(r *http.Request, in *wbxml.Element) (int, *wbxml.Element) {
		q, _ := text(in, "Query")
		if queries != nil {
			*queries = append(*queries, q)
		}
		ql := strings.ToLower(strings.TrimSpace(q))
		start, end := 0, 99
		if rg, ok := text(in, "Range"); ok {
			start, end, _ = parseRange(rg)
		}
		if end > 99 {
			end = 99 // "ranges beyond 0-99 are clamped"
		}
		var hits []galPerson
		for _, p := range people {
			email := strings.ToLower(p.email)
			match := false
			if strings.Contains(ql, "@") {
				match = byAddress && strings.HasPrefix(email, ql)
			} else {
				for _, f := range []string{p.display, p.first, p.last, p.alias, strings.SplitN(email, "@", 2)[0]} {
					f = strings.ToLower(f)
					if strings.HasPrefix(f, ql) {
						match = true
					}
					for _, w := range strings.Fields(f) {
						if strings.HasPrefix(w, ql) {
							match = true
						}
					}
				}
			}
			if match {
				hits = append(hits, p)
			}
		}
		store := e(wbxml.PageSearch, "Store", e(wbxml.PageSearch, "Status", txt("1")))
		if len(hits) > 0 && start < len(hits) {
			last := end
			if last >= len(hits) {
				last = len(hits) - 1
			}
			for _, p := range hits[start : last+1] {
				store.Children = append(store.Children, p.element())
			}
			store.Children = append(store.Children, e(wbxml.PageSearch, "Range", txt(fmt.Sprintf("%d-%d", start, last))))
		}
		store.Children = append(store.Children, e(wbxml.PageSearch, "Total", txt(itoa(len(hits)))))
		return 0, e(wbxml.PageSearch, "Search", e(wbxml.PageSearch, "Status", txt("1")), e(wbxml.PageSearch, "Response", store))
	}
}

// searchStatusResp is a Search answered with a status instead of results: at the top
// (top true) or in the Store.
func searchStatusResp(code string, top bool) *wbxml.Element {
	if top {
		return e(wbxml.PageSearch, "Search", e(wbxml.PageSearch, "Status", txt(code)))
	}
	return e(wbxml.PageSearch, "Search", e(wbxml.PageSearch, "Status", txt("1")),
		e(wbxml.PageSearch, "Response", e(wbxml.PageSearch, "Store", e(wbxml.PageSearch, "Status", txt(code)))))
}

// violationsSeen is the number of requests the model refused as protocol errors.
func (f *fakeEAS) violationsSeen() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.violations)
}

// pingFolders returns the folder ids a recorded Ping request names.
func (f *fakeEAS) pingFolders(r req) []string {
	var out []string
	if r.Root == nil {
		return nil
	}
	for _, el := range r.Root.FindAll("Id") {
		out = append(out, el.TextContent())
	}
	return out
}

// galANRHandler answers Search for the GAL like the sweep needs it: strict ambiguous name
// resolution (the start of the display name, first name, last name or alias), the window the
// first 100 matches by address, a Range cut to 0-99, a Total, and a refusal (Search status 2)
// of a query shorter than minQuery characters. queries collects the Query texts.
func galANRHandler(people []testgal.Person, minQuery int, queries *[]string, mu *sync.Mutex) handler {
	return func(r *http.Request, in *wbxml.Element) (int, *wbxml.Element) {
		q, _ := text(in, "Query")
		if mu != nil {
			mu.Lock()
			*queries = append(*queries, q)
			mu.Unlock()
		}
		if minQuery > 0 && len([]rune(q)) < minQuery {
			return 0, searchStatusResp("2", true)
		}
		ql := strings.ToLower(strings.TrimSpace(q))
		start, end := 0, 99
		if rg, ok := text(in, "Range"); ok {
			start, end, _ = parseRange(rg)
		}
		if end > 99 {
			end = 99
		}
		var hits []testgal.Person
		for _, p := range people {
			if p.MatchesANR(ql) {
				hits = append(hits, p)
			}
		}
		sort.SliceStable(hits, func(i, j int) bool { return hits[i].Email < hits[j].Email })
		store := e(wbxml.PageSearch, "Store", e(wbxml.PageSearch, "Status", txt("1")))
		if len(hits) > 0 && start < len(hits) {
			last := end
			if last >= len(hits) {
				last = len(hits) - 1
			}
			for _, p := range hits[start : last+1] {
				store.Children = append(store.Children, galPerson{display: p.Display, first: p.First, last: p.Last, email: p.Email, alias: p.Alias,
					office: p.Office, title: p.Title, company: p.Company, phone: p.Phone, mobile: p.Mobile, home: "HOMEPHONE-NOT-KEPT"}.element())
			}
			store.Children = append(store.Children, e(wbxml.PageSearch, "Range", txt(fmt.Sprintf("%d-%d", start, last))))
		}
		store.Children = append(store.Children, e(wbxml.PageSearch, "Total", txt(itoa(len(hits)))))
		return 0, e(wbxml.PageSearch, "Search", e(wbxml.PageSearch, "Status", txt("1")), e(wbxml.PageSearch, "Response", store))
	}
}
