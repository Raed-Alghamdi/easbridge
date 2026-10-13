package carddav_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"easbridge/internal/carddav"
	"easbridge/internal/engine"
	"easbridge/internal/store"
	"easbridge/internal/vcard"
)

const (
	testUser = "alice@example.test"
	testPass = "pw:with:colons-ü"
)

// harness is a CardDAV server over a real state database and a stub login.
type harness struct {
	t     *testing.T
	db    *store.DB
	acct  *store.Account
	srv   *carddav.Server
	url   string
	login func(ctx context.Context, user, pass string) (string, error)
	mu    sync.Mutex
	calls [][2]string
	logs  *lockedBuf
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func newHarness(t *testing.T, mod ...func(*carddav.Options)) *harness {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	h := &harness{t: t, db: db, logs: &lockedBuf{}}
	h.acct, err = db.Account(testUser)
	if err != nil {
		t.Fatal(err)
	}
	h.login = func(_ context.Context, user, pass string) (string, error) {
		if strings.EqualFold(user, testUser) && pass == testPass {
			return strings.ToLower(user), nil
		}
		return "", engine.ErrBadCredentials
	}
	o := carddav.Options{
		Login: func(ctx context.Context, user, pass string) (string, error) {
			h.mu.Lock()
			h.calls = append(h.calls, [2]string{user, pass})
			h.mu.Unlock()
			return h.login(ctx, user, pass)
		},
		DB:                func() *store.DB { return db },
		ContactsBookName:  "Exchange contacts",
		DirectoryBookName: "Directory",
		DirectoryEnabled:  true,
		Log:               newLogger(h.logs),
	}
	for _, m := range mod {
		m(&o)
	}
	h.srv = carddav.New(o)
	// The real http.Server (its limits and timeouts) on a plain loopback listener; TLS has
	// a test of its own.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go h.srv.Serve(ln)
	h.url = "http://" + ln.Addr().String()
	t.Cleanup(func() { h.srv.Close() })
	return h
}

func basic(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// response is what a request returned.
type response struct {
	code   int
	header http.Header
	body   string
}

type reqOpt func(*http.Request)

func hdr(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }

// do sends a request as the test user unless an Authorization option overrides it.
func (h *harness) do(method, path string, body string, opts ...reqOpt) response {
	h.t.Helper()
	req, err := http.NewRequest(method, h.url+path, strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Authorization", basic(testUser, testPass))
	for _, o := range opts {
		o(req)
	}
	if req.Header.Get("Authorization") == "-" {
		req.Header.Del("Authorization")
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return response{resp.StatusCode, resp.Header, string(b)}
}

func (h *harness) propfind(path, depth, body string) response {
	h.t.Helper()
	opts := []reqOpt{hdr("Content-Type", "application/xml")}
	if depth != "" {
		opts = append(opts, hdr("Depth", depth))
	}
	return h.do("PROPFIND", path, body, opts...)
}

// seedBook adds a complete contact book with the given cards (name -> display name) and
// returns the folder.
func (h *harness) seedBook(serverID, name string, typ int, cards map[string]string) *store.ContactFolder {
	h.t.Helper()
	if _, err := h.acct.Update(nil, func(tx *store.Tx) error {
		_, err := tx.ApplyContactFolders([]store.FolderInput{{ServerID: serverID, DisplayName: name, EASType: typ}}, nil, nil, false)
		return err
	}); err != nil {
		h.t.Fatal(err)
	}
	fs, _ := h.acct.ContactFolders()
	var f *store.ContactFolder
	for i := range fs {
		if fs[i].ServerID == serverID {
			f = &fs[i]
		}
	}
	h.apply(f.ID, store.ContactBatch{CallKey: "0", NewKey: "1"})
	var ups []store.ContactCardIn
	for sid, fn := range cards {
		uid := vcard.PersonalUID(testUser, serverID, sid)
		ups = append(ups, store.ContactCardIn{ServerID: sid, UID: uid, VCard: vcard.Card{UID: uid, Name: fn}.Marshal()})
	}
	h.apply(f.ID, store.ContactBatch{CallKey: "1", NewKey: "2", Upserts: ups})
	f, _ = h.acct.ContactFolderByID(f.ID)
	return f
}

func (h *harness) apply(id int64, b store.ContactBatch) {
	h.t.Helper()
	if _, err := h.acct.Update(nil, func(tx *store.Tx) error {
		_, err := tx.ApplyContactBatch(id, b)
		return err
	}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) putDirectory(email, fn string) store.Card {
	h.t.Helper()
	card := vcard.Card{UID: vcard.DirectoryUID(email), Name: fn, Emails: []vcard.Email{{Value: email, Types: []string{"WORK"}}}}.Marshal()
	if _, err := h.acct.Update(nil, func(tx *store.Tx) error { _, err := tx.PutDirectoryCard(email, card); return err }); err != nil {
		h.t.Fatal(err)
	}
	c, err := h.acct.DirectoryCardByEmail(email)
	if err != nil {
		h.t.Fatal(err)
	}
	return *c
}

// ---- reading multistatus ----

type msProp struct {
	XMLName xml.Name
	Inner   string `xml:",innerxml"`
}

type msPropstat struct {
	Prop struct {
		Props []msProp `xml:",any"`
	} `xml:"prop"`
	Status string `xml:"status"`
}

type msResponse struct {
	Href     string       `xml:"href"`
	Status   string       `xml:"status"`
	Propstat []msPropstat `xml:"propstat"`
}

type multistatus struct {
	XMLName   xml.Name     `xml:"multistatus"`
	Responses []msResponse `xml:"response"`
}

func parseMS(t *testing.T, body string) multistatus {
	t.Helper()
	var ms multistatus
	if err := xml.Unmarshal([]byte(body), &ms); err != nil {
		t.Fatalf("the multistatus is not well-formed XML: %v\n%s", err, body)
	}
	return ms
}

// props returns the properties of a response with a given status line, by local name.
func (r msResponse) props(status string) map[string]msProp {
	out := map[string]msProp{}
	for _, ps := range r.Propstat {
		if strings.Contains(ps.Status, status) {
			for _, p := range ps.Prop.Props {
				out[p.XMLName.Local] = p
			}
		}
	}
	return out
}

func (m multistatus) byHref(href string) (msResponse, bool) {
	for _, r := range m.Responses {
		if r.Href == href {
			return r, true
		}
	}
	return msResponse{}, false
}

func (m multistatus) hrefs() []string {
	var out []string
	for _, r := range m.Responses {
		out = append(out, r.Href)
	}
	return out
}

func propBody(props ...string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?><propfind xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav" xmlns:cs="http://calendarserver.org/ns/"><prop>`)
	for _, p := range props {
		b.WriteString("<" + p + "/>")
	}
	b.WriteString("</prop></propfind>")
	return b.String()
}

func must(t *testing.T, cond bool, format string, a ...any) {
	t.Helper()
	if !cond {
		t.Errorf(format, a...)
	}
}

var _ = fmt.Sprint
