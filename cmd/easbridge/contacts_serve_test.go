//go:build devfake

package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"easbridge/internal/config"
	"easbridge/internal/logx"
	"easbridge/internal/store"
	"easbridge/internal/testpki"
)

// testHooks, when set, adjusts the hooks startBridge gives serve (a test sets it and resets it).
var testHooks func(*serveHooks)

type runningBridge struct {
	t       *testing.T
	pki     *testpki.PKI
	imap    string
	health  string
	carddav string
	control string
	logs    *lockedBuf
	stop    func()
}

// startBridge runs serve() with the fake upstream and the given extra settings.
func startBridge(t *testing.T, dir string, extra map[string]string) *runningBridge {
	t.Helper()
	pki, err := testpki.New()
	if err != nil {
		t.Fatal(err)
	}
	_, certFile, keyFile, err := pki.WriteFiles(filepath.Join(dir, "tls"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("EASBRIDGE_FAKE_PASSWORD", "pw-contacts-test-battery")
	env := map[string]string{
		config.EnvUpstream: "fake", config.EnvAllowedUsers: "alice@example.test",
		config.EnvIMAPAddr: "127.0.0.1:0", config.EnvSMTPAddr: "127.0.0.1:0", config.EnvHealthAddr: "127.0.0.1:0",
		config.EnvFakeControl: "127.0.0.1:0", config.EnvTLSCert: certFile, config.EnvTLSKey: keyFile,
		config.EnvDataDir: filepath.Join(dir, "data"), config.EnvLogLevel: "debug",
	}
	// The contacts are on by default since 0.3: these tests that do not ask for them keep them off
	// (the default CardDAV port is a fixed one).
	if _, ok := extra[config.EnvCardDAVAddr]; !ok {
		env[config.EnvContacts] = "off"
	}
	for k, v := range extra {
		env[k] = v
	}
	if env[config.EnvTLSCert] == "-" { // the automatic certificate: no TLS variables at all
		delete(env, config.EnvTLSCert)
		delete(env, config.EnvTLSKey)
	}
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuf{}
	log := logx.New(logx.Options{Level: slog.LevelDebug, Writer: logs})
	ctx, cancel := context.WithCancel(context.Background())
	b := &runningBridge{t: t, pki: pki, logs: logs}
	ready := make(chan struct{}, 1)
	davc := make(chan string, 1)
	done := make(chan error, 1)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		hooks := &serveHooks{
			onListening: func(i, s, h string) { b.imap, b.health = i, h; ready <- struct{}{} },
			onCardDAV:   func(a string) { davc <- a },
		}
		if testHooks != nil {
			testHooks(hooks)
		}
		done <- serve(ctx, cfg, log, hooks)
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("serve returned early: %v\n%s", err, logs.String())
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not start")
	}
	select {
	case b.carddav = <-davc:
	default:
	}
	m := regexp.MustCompile(`control API listening" addr=(\S+)`).FindStringSubmatch(logs.String())
	if m == nil {
		t.Fatalf("no control address in the log:\n%s", logs.String())
	}
	b.control = "http://" + m[1]
	b.stop = func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
			t.Error("serve did not stop")
		}
	}
	t.Cleanup(b.stop)
	return b
}

func (b *runningBridge) post(path, body string) {
	b.t.Helper()
	r, err := http.Post(b.control+path, "application/json", strings.NewReader(body))
	if err != nil || r.StatusCode != 200 {
		b.t.Fatalf("POST %s: %v %v", path, r, err)
	}
	r.Body.Close()
}

func (b *runningBridge) dav(method, path, user, pass, body string, hdrs ...string) (int, http.Header, string) {
	b.t.Helper()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: b.pki.ClientConfig("easbridge")}, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequest(method, "https://"+b.carddav+path, strings.NewReader(body))
	if err != nil {
		b.t.Fatal(err)
	}
	if user != "" {
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
	}
	for i := 0; i+1 < len(hdrs); i += 2 {
		req.Header.Set(hdrs[i], hdrs[i+1])
	}
	resp, err := client.Do(req)
	if err != nil {
		b.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(data)
}

func (b *runningBridge) healthz() map[string]any {
	b.t.Helper()
	resp, err := http.Get("http://" + b.health + "/healthz")
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	var h map[string]any
	json.NewDecoder(resp.Body).Decode(&h)
	h["_code"] = resp.StatusCode
	return h
}

func eventuallyT(t *testing.T, what string, cond func() bool) {
	t.Helper()
	eventuallyWithin(t, 10*time.Second, what, cond)
}

// eventuallyWithin is eventuallyT with its own limit (a sweep of a few hundred searches needs more under -race).
func eventuallyWithin(t *testing.T, limit time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

const bridgePass = "pw-contacts-test-battery"

func TestServeWithContactsAndTheDirectoryEndToEnd(t *testing.T) {
	dir := t.TempDir()
	b := startBridge(t, dir, map[string]string{
		config.EnvCardDAVAddr: "127.0.0.1:0", config.EnvDirectoryDomains: "corp.example", config.EnvDirectoryLookups: "180",
		config.EnvContactsInterval: "5m", config.EnvContactsBookName: "My Exchange", config.EnvDirectoryBook: "People",
	})
	if b.carddav == "" {
		t.Fatalf("no CardDAV listener\n%s", b.logs.String())
	}
	u := "alice@example.test"
	// Content on the fake Exchange: a Contacts folder with two contacts, a person in the directory, mail that names them.
	b.post("/contact-folder?user="+u+"&name=Contacts&type=9", "")
	b.post("/contact?user="+u+"&folder=Contacts", `{"FirstName":"عبدالله","LastName":"القحطاني","Email1":"abdullah@example.test","MobilePhone":"+1 555 050 4567"}`)
	b.post("/contact?user="+u+"&folder=Contacts", `{"FirstName":"Ada","LastName":"Lovelace","Email1":"ada@example.test"}`)
	b.post("/gal-add", `{"DisplayName":"Pat Smith","FirstName":"Pat","LastName":"Smith","EmailAddress":"pat@corp.example","Company":"Example Co","Alias":"psmith","HomePhone":"000"}`)
	mime := "Message-ID: <inject@x>\r\nFrom: b@example.test\r\nTo: alice@example.test\r\nCc: Pat Smith <pat@corp.example>\r\nSubject: s\r\nDate: Fri, 09 Oct 2026 12:00:00 +0000\r\n\r\nhi\r\n"
	r, err := http.Post(b.control+"/inject?user="+u+"&folder=Inbox", "message/rfc822", strings.NewReader(mime))
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("inject: %v %v", r, err)
	}
	r.Body.Close()
	// Logging in over IMAP starts the mailbox engine (the same login as CardDAV's).
	code, hd, _ := b.dav("PROPFIND", "/", u, bridgePass, propfindBody, "Depth", "0")
	if code != 207 {
		t.Fatalf("PROPFIND /: %d %v\n%s", code, hd, b.logs.String())
	}
	// Discovery as MailFlow does it, then the book list.
	var books string
	eventuallyT(t, "the books", func() bool {
		_, _, body := b.dav("PROPFIND", "/dav/addressbooks/me/", u, bridgePass, bookListBody, "Depth", "1")
		books = body
		return strings.Contains(body, "My Exchange") && strings.Contains(body, "People")
	})
	_ = books
	var query string
	eventuallyT(t, "both contacts in the book", func() bool {
		_, _, body := b.dav("REPORT", "/dav/addressbooks/me/contacts/", u, bridgePass, queryBody, "Depth", "1")
		query = body
		return strings.Count(body, "BEGIN:VCARD") == 2
	})
	for _, want := range []string{"N:القحطاني;عبدالله", "EMAIL;TYPE=OTHER:ada@example.test", "TEL;TYPE=CELL:+1 555 050 4567"} {
		if !strings.Contains(unfoldXML(query), want) {
			t.Errorf("the book lacks %q:\n%s", want, query)
		}
	}
	eventuallyT(t, "the directory card", func() bool {
		_, _, body := b.dav("REPORT", "/dav/addressbooks/me/directory/", u, bridgePass, queryBody, "Depth", "1")
		return strings.Contains(body, "FN:Pat Smith") && !strings.Contains(body, "psmith") && !strings.Contains(body, "000")
	})

	// Authentication through the shared login: a wrong password is 401, a write is 405.
	if code, hd, _ := b.dav("PROPFIND", "/", u, "wrong", propfindBody, "Depth", "0"); code != 401 || !strings.Contains(hd.Get("WWW-Authenticate"), "Basic") {
		t.Errorf("wrong password: %d", code)
	}
	if code, _, _ := b.dav("PUT", "/dav/addressbooks/me/contacts/x.vcf", u, bridgePass, "BEGIN:VCARD\r\nEND:VCARD\r\n"); code != 405 {
		t.Errorf("PUT: %d", code)
	}
	if code, _, _ := b.dav("PROPFIND", "/", "mallory@example.test", bridgePass, propfindBody, "Depth", "0"); code != 401 {
		t.Errorf("a user that is not on the allow-list: %d", code)
	}
	if code, hd, _ := b.dav("GET", "/.well-known/carddav", "", "", ""); code != 301 || hd.Get("Location") != "/dav/" {
		t.Errorf("well-known: %d %v", code, hd)
	}

	// /healthz: counts only.
	var acct map[string]any
	eventuallyT(t, "contacts in the health report", func() bool {
		h := b.healthz()
		accts, _ := h["accounts"].([]any)
		if len(accts) != 1 {
			return false
		}
		acct, _ = accts[0].(map[string]any)
		c, _ := acct["contacts"].(map[string]any)
		return c != nil && c["directory_cards"] == float64(1)
	})
	c := acct["contacts"].(map[string]any)
	books2, _ := c["books"].([]any)
	if len(books2) != 1 || books2[0].(map[string]any)["complete"] != true || books2[0].(map[string]any)["cards"] != float64(2) ||
		c["directory_enabled"] != true || c["directory_queue"] != float64(0) || c["directory_lookups_today"] != float64(1) {
		t.Errorf("health contacts: %v", c)
	}
	h := b.healthz()
	if h["carddav_listening"] != true || h["status"] != "ok" {
		t.Errorf("health: %v", h)
	}
	raw, _ := json.Marshal(h)
	for _, secret := range []string{"Pat", "Smith", "corp.example", "عبدالله", "Lovelace", "ada@", "My Exchange", "People"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("/healthz shows %q", secret)
		}
	}
	// Logs: no credentials, names, addresses or numbers.
	logs := b.logs.String()
	for _, secret := range []string{bridgePass, "wrong", "Pat Smith", "pat@corp.example", "ada@example.test", "abdullah@", "عبدالله", "Lovelace", "+1 555", "BEGIN:VCARD", "psmith"} {
		if strings.Contains(logs, secret) {
			t.Errorf("the log contains %q", secret)
		}
	}
	if !strings.Contains(logs, "contacts are on: read-only CardDAV") {
		t.Errorf("no start-up line about the contacts")
	}
}

const propfindBody = `<propfind xmlns="DAV:"><prop><current-user-principal/></prop></propfind>`
const bookListBody = `<propfind xmlns="DAV:" xmlns:cs="http://calendarserver.org/ns/"><prop><resourcetype/><displayname/><cs:getctag/></prop></propfind>`
const queryBody = `<C:addressbook-query xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><prop><getetag/><C:address-data/></prop></C:addressbook-query>`

func unfoldXML(s string) string {
	return strings.NewReplacer("&#13;", "", "\r\n ", "", "\n ", "").Replace(s)
}

func TestWithoutACardDAVAddressThereIsNoListenerAndNoContactData(t *testing.T) {
	dir := t.TempDir()
	// A database from an earlier run that had the feature on.
	db, err := store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	acct, _ := db.Account("alice@example.test")
	acct.Update(nil, func(tx *store.Tx) error {
		if _, err := tx.ApplyContactFolders([]store.FolderInput{{ServerID: "c1", DisplayName: "Contacts", EASType: 9}}, nil, nil, false); err != nil {
			return err
		}
		if _, err := tx.PutDirectoryCard("pat@corp.example", []byte("BEGIN:VCARD\r\nEND:VCARD\r\n")); err != nil {
			return err
		}
		return tx.NoteCandidate("pat@corp.example", "Pat", time.Now())
	})
	db.Close()

	b := startBridge(t, dir, nil)
	if b.carddav != "" {
		t.Errorf("a CardDAV listener without EASBRIDGE_CARDDAV_ADDR: %s", b.carddav)
	}
	h := b.healthz()
	if _, ok := h["carddav_listening"]; ok || h["_code"] != 200 {
		t.Errorf("health: %v", h)
	}
	if !strings.Contains(b.logs.String(), "the contact data of an earlier run was removed") {
		t.Errorf("no line about the removal:\n%s", b.logs.String())
	}
	b.stop()
	db, err = store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	acct, _ = db.Account("alice@example.test")
	fs, _ := acct.ContactFolders()
	cs, _ := acct.DirectoryCards()
	cands, _ := acct.Candidates()
	if len(fs)+len(cs)+len(cands) != 0 {
		t.Errorf("the contact data survived: %d %d %d", len(fs), len(cs), len(cands))
	}
	_ = fmt.Sprint
	_ = tls.VersionTLS12
}

func TestWithContactsButNoDirectoryTheDirectoryDataGoes(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	acct, _ := db.Account("alice@example.test")
	acct.Update(nil, func(tx *store.Tx) error {
		_, err := tx.PutDirectoryCard("pat@corp.example", []byte("BEGIN:VCARD\r\nEND:VCARD\r\n"))
		return err
	})
	db.Close()
	b := startBridge(t, dir, map[string]string{config.EnvCardDAVAddr: "127.0.0.1:0", config.EnvDirectoryDomains: "none"})
	if b.carddav == "" {
		t.Fatal("no listener")
	}
	b.post("/contact-folder?user=alice@example.test&name=Contacts&type=9", "")
	code, _, body := b.dav("PROPFIND", "/dav/addressbooks/me/", "alice@example.test", bridgePass, bookListBody, "Depth", "1")
	if code != 207 || strings.Contains(body, "directory") {
		t.Errorf("the directory book is listed although the directory is off: %d %s", code, body)
	}
	t0 := time.Now()
	b.stop()
	t.Logf("stop took %v", time.Since(t0))
	db, _ = store.Open(filepath.Join(dir, "data"))
	defer db.Close()
	acct, _ = db.Account("alice@example.test")
	if cs, _ := acct.DirectoryCards(); len(cs) != 0 {
		t.Errorf("directory cards survived")
	}
	eventuallyT(t, "the contact folder (synced from the server) is there", func() bool {
		fs, _ := acct.ContactFolders()
		return len(fs) == 1
	})
}

func TestServeWithTheFullDirectorySweepEndToEnd(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("EASBRIDGE_FAKE_SEARCH_SPACING", "1ms") // devfake only: the real spacing is at least 20 s
	b := startBridge(t, dir, map[string]string{
		config.EnvCardDAVAddr: "127.0.0.1:0", config.EnvDirectoryDomains: "corp.example", config.EnvDirectoryLookups: "180",
		config.EnvDirectoryFull: "on", config.EnvDirectoryFullInt: "168h",
	})
	u := "alice@example.test"
	b.post("/gal-anr?on=1&min=2", "") // the server answers from two characters
	res, err := http.Post(b.control+"/gal-generate?n=1500&seed=5&domains=corp.example&external=outside.example", "", nil)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("%v %v", res, err)
	}
	var gen struct {
		InDomain int `json:"in_domain"`
	}
	json.NewDecoder(res.Body).Decode(&gen)
	res.Body.Close()
	if gen.InDomain < 1000 {
		t.Fatalf("generated %d", gen.InDomain)
	}
	b.post("/gal-add", `{"DisplayName":"Alice Own","FirstName":"Alice","EmailAddress":"alice@corp.example"}`)
	if code, _, _ := b.dav("PROPFIND", "/", u, bridgePass, propfindBody, "Depth", "0"); code != 207 {
		t.Fatalf("login: %d", code)
	}
	var sweep map[string]any
	eventuallyWithin(t, 90*time.Second, "the sweep has finished", func() bool {
		h := b.healthz()
		accts, _ := h["accounts"].([]any)
		if len(accts) != 1 {
			return false
		}
		c, _ := accts[0].(map[string]any)["contacts"].(map[string]any)
		sweep, _ = c["directory_sweep"].(map[string]any)
		return sweep != nil && sweep["state"] == "waiting" && sweep["last_sweep_unix"] != nil
	})
	if sweep["full"] != true || sweep["calibrated_length"] != float64(2) || sweep["calibration_failed"] != false || sweep["last_sweep_capped"] != false ||
		sweep["prefixes_queued"] != float64(0) || sweep["entries_found_this_sweep"] != float64(gen.InDomain+1) || sweep["waiting_until_unix"] == nil {
		t.Errorf("sweep health: %v", sweep)
	}
	_, _, body := b.dav("REPORT", "/dav/addressbooks/me/directory/", u, bridgePass, queryBody, "Depth", "1")
	if n := strings.Count(body, "BEGIN:VCARD"); n != gen.InDomain+1 {
		t.Errorf("the Directory book has %d cards, want %d", n, gen.InDomain+1)
	}
	if strings.Contains(body, "outside.example") {
		t.Errorf("an external contact is in the book")
	}
	logs := b.logs.String()
	for _, want := range []string{"directory sweep started", "directory sweep calibrated", "directory sweep finished", "directory_full_sweep=true"} {
		if !strings.Contains(logs, want) {
			t.Errorf("no log line with %q", want)
		}
	}
	raw, _ := json.Marshal(b.healthz())
	for _, bad := range []string{"corp.example", "Alice", "outside.example"} {
		if strings.Contains(string(raw)+logs, bad) && bad != "corp.example" {
			t.Errorf("health or log shows %q", bad)
		}
	}
}

func TestTurningTheSweepOffRemovesWhatOnlyItFoundAtTheNextStart(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	acct, _ := db.Account("alice@example.test")
	acct.Update(nil, func(tx *store.Tx) error {
		if _, err := tx.PutDirectoryCard("lookup@corp.example", []byte("BEGIN:VCARD\r\nEND:VCARD\r\n")); err != nil {
			return err
		}
		if err := tx.SweepStart(time.Now()); err != nil {
			return err
		}
		_, _, err := tx.SweepCalibrationResult(1, 4, true, time.Now(), time.Hour)
		return err
	})
	acct.Update(nil, func(tx *store.Tx) error {
		id, _, _, err := tx.SweepNext()
		if err != nil {
			return err
		}
		_, err = tx.SweepApply(store.SweepStepIn{RowID: id, MaxSearches: 100, Now: time.Now(), Interval: time.Hour,
			Entries: []store.SweepEntry{{Email: "sweep@corp.example", Card: []byte("BEGIN:VCARD\r\nEND:VCARD\r\n")}, {Email: "lookup@corp.example", Card: []byte("BEGIN:VCARD\r\nFN:x\r\nEND:VCARD\r\n")}}})
		return err
	})
	db.Close()
	b := startBridge(t, dir, map[string]string{config.EnvCardDAVAddr: "127.0.0.1:0", config.EnvDirectoryDomains: "corp.example"}) // the sweep is off
	if !strings.Contains(b.logs.String(), "the full directory sweep is off: the cards only it had found were removed") {
		t.Errorf("no line about the removal:\n%s", b.logs.String())
	}
	b.stop()
	db, err = store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	acct, _ = db.Account("alice@example.test")
	if _, err := acct.DirectoryCardByEmail("sweep@corp.example"); err == nil {
		t.Error("a card only the sweep had found stays")
	}
	if _, err := acct.DirectoryCardByEmail("lookup@corp.example"); err != nil {
		t.Errorf("a card a lookup found goes: %v", err)
	}
	if sw, _ := acct.Sweep(); sw.ID != 0 {
		t.Errorf("the sweep state stays: %+v", sw)
	}
}

func TestStartingWithTheContactsOffClearsTheMarkerThatTheHierarchyKeptThem(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	acct, _ := db.Account("alice@example.test")
	if _, err := acct.Update(nil, func(tx *store.Tx) error { return tx.SetContactFoldersKept(true) }); err != nil {
		t.Fatal(err)
	}
	db.Close()
	b := startBridge(t, dir, nil) // EASBRIDGE_CARDDAV_ADDR is not set: the contacts are off
	b.stop()
	db, err = store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	acct, _ = db.Account("alice@example.test")
	if kept, err := acct.ContactFoldersKept(); err != nil || kept {
		t.Errorf("the marker is still set after a start with the contacts off: %v %v (the next start with them on must read the hierarchy again)", kept, err)
	}
}
