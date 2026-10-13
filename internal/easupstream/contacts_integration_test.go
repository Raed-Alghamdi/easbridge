package easupstream_test

// The contacts feature end to end over the real library: engine -> adapter ->
// patched go-activesync -> WBXML over TLS -> the wire-level model of Exchange, which
// refuses what Exchange refuses (a first-SyncKey request with extra elements is Sync
// status 4; a Search that is not well formed is Search status 2). Every test also fails
// if the model refused a request.

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hstern/go-activesync/wbxml"

	"easbridge/internal/easupstream"
	"easbridge/internal/engine"
	"easbridge/internal/logx"
	"easbridge/internal/store"
	"easbridge/internal/testgal"
	"easbridge/internal/upstream"
	"easbridge/internal/vcard"
)

// cmodel is the contacts part of the model: items with their fields, Sync keys that
// name a snapshot of what the client was told, a window, and key invalidation.
type cmodel struct {
	mu      sync.Mutex
	items   map[string]map[string]string
	keys    map[string]map[string]string // key -> id -> signature
	serial  int
	window  int
	invalid int
	syncs   int
}

func newCModel(window int) *cmodel {
	return &cmodel{items: map[string]map[string]string{}, keys: map[string]map[string]string{}, window: window}
}

func sigOf(fields map[string]string) string {
	names := make([]string, 0, len(fields))
	for k := range fields {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, k := range names {
		b.WriteString(k + "=" + fields[k] + ";")
	}
	return b.String()
}

func (c *cmodel) set(id string, fields map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[id] = fields
}

func (c *cmodel) remove(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, id)
}

func (c *cmodel) forgetKeys() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.invalid = c.serial + 1
}

func (c *cmodel) issue(folder string, known map[string]string) string {
	c.serial++
	key := fmt.Sprintf("C%d", c.serial)
	cp := map[string]string{}
	for k, v := range known {
		cp[k] = v
	}
	c.keys[key] = cp
	return key
}

func (c *cmodel) handle(fid string, in *wbxml.Element) (int, *wbxml.Element) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.syncs++
	key, _ := text(in, "SyncKey")
	if key == "0" {
		return 0, syncResp(fid, c.issue(fid, nil), "1")
	}
	known, ok := c.keys[key]
	var serial int
	fmt.Sscanf(key, "C%d", &serial)
	if !ok || serial < c.invalid {
		return 0, syncResp(fid, key, "3")
	}
	var adds, chg, del []string
	for id, f := range c.items {
		if old, tracked := known[id]; !tracked {
			adds = append(adds, id)
		} else if old != sigOf(f) {
			chg = append(chg, id)
		}
	}
	for id := range known {
		if _, ok := c.items[id]; !ok {
			del = append(del, id)
		}
	}
	sort.Strings(adds)
	sort.Strings(chg)
	sort.Strings(del)
	next := map[string]string{}
	for k, v := range known {
		next[k] = v
	}
	win := c.window
	if w, ok := text(in, "WindowSize"); ok {
		n := 0
		fmt.Sscanf(w, "%d", &n)
		if n > 0 && n < win {
			win = n
		}
	}
	var cmds []*wbxml.Element
	more := false
	n := 0
	take := func() bool {
		if n >= win {
			more = true
			return false
		}
		n++
		return true
	}
	for _, id := range adds {
		if !take() {
			break
		}
		cmds = append(cmds, contactAdd(id, c.items[id]))
		next[id] = sigOf(c.items[id])
	}
	for _, id := range chg {
		if !take() {
			break
		}
		app := e(wbxml.PageAirSync, "ApplicationData")
		for k, v := range c.items[id] {
			app.Children = append(app.Children, contactEl(k, v))
		}
		cmds = append(cmds, e(wbxml.PageAirSync, "Change", e(wbxml.PageAirSync, "ServerId", txt(id)), app))
		next[id] = sigOf(c.items[id])
	}
	for _, id := range del {
		if !take() {
			break
		}
		cmds = append(cmds, e(wbxml.PageAirSync, "Delete", e(wbxml.PageAirSync, "ServerId", txt(id))))
		delete(next, id)
	}
	nk := c.issue(fid, next)
	var kidsOf []*wbxml.Element
	if len(cmds) > 0 {
		kidsOf = append(kidsOf, commands(cmds...))
	}
	if more {
		kidsOf = append(kidsOf, e(wbxml.PageAirSync, "MoreAvailable"))
	}
	return 0, syncResp(fid, nk, "1", kidsOf...)
}

type cstack struct {
	*stack
	cm      *cmodel
	people  *[]galPerson
	queries *[]string
	qmu     *sync.Mutex
	// what a restart needs
	fac upstream.Factory
	cfg engine.Config
	log *slog.Logger
}

// restart stops the engine and starts a new manager over the same database and model, with the
// contacts on or off, and logs in again.
func (c *cstack) restart(contacts bool) {
	c.t.Helper()
	c.mgr.Close()
	c.cfg.ContactsEnabled = contacts
	mgr := engine.NewManager(c.cfg, c.log, c.mgr.DB(), c.fac, func(u string) bool { return u == user })
	c.t.Cleanup(mgr.Close) // before the database is closed (cleanups run last in, first out)
	c.mgr = mgr
	a, err := c.mgr.Login(ctx, user, pass)
	if err != nil {
		c.t.Fatalf("login: %v", err)
	}
	c.acct = a
}

func (c *cstack) zeroKeys() (zeros, total int) {
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	for _, k := range c.m.hierSeen {
		if k == "0" {
			zeros++
		}
	}
	return zeros, len(c.m.hierSeen)
}

// buildContactsStack wires the engine, with contacts on, to the real adapter against the
// model with a Contacts folder (type 9) and a user contacts folder (type 14).
func buildContactsStack(t *testing.T, domains ...string) *cstack {
	return buildContactsStackWith(t, domains, nil)
}

// buildContactsStackWith is buildContactsStack with a change to the engine settings and to the
// model before the engine logs in (the Search handler of the model can be replaced there).
func buildContactsStackWith(t *testing.T, domains []string, mod func(*engine.Config, *model)) *cstack {
	logx.ResetSecretsForTest()
	m := newModel(t)
	cm := newCModel(4)
	def := m.addFolder("Contacts", 9)
	usr := m.addFolder("Clients", 14)
	userModel := newCModel(4)
	origSync := m.sync
	m.f.on("Sync", func(r *http.Request, in *wbxml.Element) (int, *wbxml.Element) {
		fid, _ := text(in, "CollectionId")
		switch fid {
		case def.id:
			return cm.handle(fid, in)
		case usr.id:
			return userModel.handle(fid, in)
		}
		return origSync(r, in)
	})
	people := &[]galPerson{}
	queries := &[]string{}
	qmu := &sync.Mutex{}
	m.f.on("Search", func(r *http.Request, in *wbxml.Element) (int, *wbxml.Element) {
		qmu.Lock()
		defer qmu.Unlock()
		return galSearchHandler(*people, false, queries)(r, in)
	})
	buf := &bytes.Buffer{}
	sw := &syncWriter{w: buf}
	log := logx.New(logx.Options{Level: slog.LevelDebug, Writer: sw})
	fac, err := easupstream.NewFactory(easupstream.Config{
		URL: m.f.url(), HTTPClient: m.f.srv.Client(), Version: "16.1", DeviceType: "easbridge", UserAgent: "easbridge/it",
		MIMEInlineMax: 2000, WindowSize: 5, MaxHeartbeat: time.Second, Logger: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := engine.Config{
		Grace: time.Hour, PingHeartbeat: time.Second, PingMinHeartbeat: 100 * time.Millisecond, AuthFailCooldown: time.Minute,
		MaxMessageBytes: 1 << 20, PlaceholderTTL: time.Hour, PlaceholderWindow: 10 * time.Minute,
		ReconVerifyDelay: 150 * time.Millisecond, FetchWorkers: 2, Housekeeping: 30 * time.Millisecond,
		Backoff: func(int) time.Duration { return 20 * time.Millisecond }, CallTimeout: 10 * time.Second,
		ContactsEnabled: true, ContactsInterval: 60 * time.Millisecond,
		Directory: engine.DirectoryConfig{Domains: domains, Spacing: 10 * time.Millisecond, Poll: 5 * time.Millisecond, SeedBatch: 5},
	}
	if mod != nil {
		mod(&cfg, m)
	}
	mgr := engine.NewManager(cfg, log, db, fac, func(u string) bool { return u == user })
	s := &stack{t: t, m: m, mgr: mgr, logs: sw, buf: buf}
	t.Cleanup(func() {
		mgr.Close()
		mgr.DB().Close()
		if t.Failed() {
			t.Logf("log:\n%s", s.logText())
		}
	})
	a, err := mgr.Login(ctx, user, pass)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	s.acct = a
	return &cstack{stack: s, cm: cm, people: people, queries: queries, qmu: qmu, fac: fac, cfg: cfg, log: log}
}

func (s *cstack) book(id string) *store.ContactFolder {
	s.t.Helper()
	var f *store.ContactFolder
	eventually(s.t, "the book "+id, func() bool {
		var err error
		f, err = s.acct.Store().ContactFolderByBook(id)
		return err == nil
	})
	return f
}

func (s *cstack) cards(f *store.ContactFolder) []store.Card {
	cs, _ := s.acct.Store().ContactCards(f.ID)
	return cs
}

func TestContactsOverTheRealLibraryAgainstTheWireLevelModel(t *testing.T) {
	s := buildContactsStack(t)
	for i := 0; i < 9; i++ {
		s.cm.set(fmt.Sprintf("c%d", i), map[string]string{"FirstName": fmt.Sprintf("First%d", i), "LastName": "Last", "Email1Address": fmt.Sprintf("p%d@example.test", i), "MobilePhoneNumber": "+1 555 50 000 000" + fmt.Sprint(i)})
	}
	s.cm.set("arabic", map[string]string{"FirstName": "عبدالرحمن", "LastName": "السلمي", "CompanyName": "شركة الأمثلة المحدودة", "BusinessPhoneNumber": "+1 555 011 0100",
		"Email1Address": "Abdulrahman <ar@example.test>", "OfficeLocation": "برج ب"})
	f := s.book(store.DefaultBookID)
	eventually(t, "the first download (10 contacts in windows of 4)", func() bool { return len(s.cards(f)) == 10 })
	var arabic string
	for _, c := range s.cards(f) {
		if c.UID == vcard.PersonalUID(user, f.ServerID, "arabic") {
			arabic = strings.ReplaceAll(strings.ReplaceAll(string(c.VCard), "\r\n ", ""), "\r\n", "\n")
		}
	}
	for _, want := range []string{"N:السلمي;عبدالرحمن;;;", "ORG:شركة الأمثلة المحدودة", "EMAIL;TYPE=OTHER:ar@example.test", "TEL;TYPE=WORK;TYPE=VOICE:+1 555 011 0100", "NOTE:Office: برج ب"} {
		if !strings.Contains(arabic, want) {
			t.Errorf("the Arabic contact lacks %q:\n%s", want, arabic)
		}
	}
	// The user folder is a book of its own and is synced too.
	b2 := s.acct.Store()
	fs, _ := b2.ContactFolders()
	if len(fs) != 2 || fs[1].DisplayName != "Clients" {
		t.Fatalf("books %+v", fs)
	}
	// An edit and a delete come through.
	s.cm.set("c1", map[string]string{"FirstName": "Edited", "LastName": "Last"})
	s.cm.remove("c2")
	eventually(t, "the edit and the delete", func() bool {
		cs := s.cards(f)
		if len(cs) != 9 {
			return false
		}
		for _, c := range cs {
			if strings.Contains(string(c.VCard), "Edited") {
				return true
			}
		}
		return false
	})
	// The server forgets every key: the library answers status 3, the adapter restarts the chain, the engine swaps.
	ctag := mustBook(t, s, f).CTag
	s.cm.remove("c3")
	s.cm.set("new", map[string]string{"FirstName": "Brand", "LastName": "New"})
	s.cm.forgetKeys()
	eventually(t, "the restarted download replaces the cards", func() bool {
		cs := s.cards(f)
		hasNew, hasC3 := false, false
		for _, c := range cs {
			hasNew = hasNew || strings.Contains(string(c.VCard), "Brand")
			hasC3 = hasC3 || strings.Contains(string(c.VCard), "First3")
		}
		return len(cs) == 9 && hasNew && !hasC3 && mustBook(t, s, f).CTag != ctag
	})
	if !strings.Contains(s.logText(), "server reset the contacts sync key") {
		t.Errorf("no line about the reset")
	}
	// Pings never name a contacts folder.
	for _, r := range s.m.f.requests("Ping") {
		for _, id := range s.m.f.pingFolders(r) {
			if id == f.ServerID {
				t.Errorf("a Ping names the contacts folder")
			}
		}
	}
	// Nothing about the contacts reached the log.
	for _, secret := range []string{"عبدالرحمن", "السلمي", "ar@example.test", "First1", "+1 555", "BEGIN:VCARD"} {
		if strings.Contains(s.logText(), secret) {
			t.Errorf("the log contains %q", secret)
		}
	}
}

func mustBook(t *testing.T, s *cstack, f *store.ContactFolder) *store.ContactFolder {
	t.Helper()
	nf, err := s.acct.Store().ContactFolderByID(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	return nf
}

func TestDirectoryOverTheRealLibraryKeepsOnlyExactMatchesAndSendsWellFormedSearches(t *testing.T) {
	s := buildContactsStack(t, "corp.example")
	*s.people = []galPerson{
		{display: "Pat Smith", first: "Pat", last: "Smith", email: "pat@corp.example", alias: "psmith", office: "B2", title: "Engineer", company: "Example Co", phone: "222", home: "000-HOME", mobile: "333"},
		{display: "Patricia Jones", first: "Patricia", last: "Jones", email: "patricia@corp.example", alias: "pjones", home: "111-OTHERHOME"},
		{display: "Pat Lee", first: "Pat", last: "Lee", email: "pat.lee@corp.example"},
	}
	now := time.Now().UTC()
	s.m.deliver("Inbox", []byte("Message-ID: <d1@x>\r\nFrom: Bob <bob@elsewhere.example>\r\nTo: alice@example.test\r\nCc: Pat Smith <pat@corp.example>, Gone Person <gone@corp.example>\r\nSubject: s\r\nDate: Fri, 09 Oct 2026 12:00:00 +0000\r\n\r\nbody\r\n"), true, now)
	eventually(t, "the exact match is kept", func() bool {
		n, _ := s.acct.Store().DirectoryCardCount()
		return n == 1
	})
	eventually(t, "the other candidate is looked up and not found", func() bool {
		c, err := s.acct.Store().Candidate("gone@corp.example")
		return err == nil && c.State == store.CandNotFound
	})
	c, err := s.acct.Store().DirectoryCardByEmail("pat@corp.example")
	if err != nil {
		t.Fatal(err)
	}
	card := string(c.VCard)
	for _, want := range []string{"FN:Pat Smith", "EMAIL;TYPE=WORK:pat@corp.example", "NOTE:Office: B2"} {
		if !strings.Contains(card, want) {
			t.Errorf("card lacks %q:\n%s", want, card)
		}
	}
	for _, never := range []string{"Patricia", "Lee", "psmith", "HOME", "pjones"} {
		if strings.Contains(card, never) {
			t.Errorf("the card holds %q", never)
		}
	}
	s.qmu.Lock()
	qs := append([]string(nil), *s.queries...)
	s.qmu.Unlock()
	// The ladder: the address, the local part, the header name (when the header has one).
	want := map[string]bool{"pat@corp.example": true, "pat": true, "Pat Smith": false, "gone@corp.example": true, "gone": true, "Gone Person": true}
	for _, q := range qs {
		if _, ok := want[q]; !ok {
			t.Errorf("unexpected search %q", q)
		}
	}
	for _, r := range s.m.f.requests("Search") {
		if rg, _ := text(r.Root, "Range"); rg != "0-99" {
			t.Errorf("Range = %q", rg)
		}
	}
	for _, secret := range []string{"pat@corp.example", "Pat Smith", "gone@corp.example", "Gone Person", "corp.example"} {
		if strings.Contains(s.logText(), secret) {
			t.Errorf("the log contains %q", secret)
		}
	}
}

// The whole opt-in sweep over the real library against the wire-level model: a generated GAL of
// a couple of thousand entries (names many people share, Arabic display names with Latin aliases,
// contacts of another domain), ambiguous name resolution, a window of 100 and a Range cut to 0-99.
// The model also refuses every Search request that is not well formed (the checks of
// searchViolation), so a sweep that sent a bad one would fail here.
func TestFullSweepOverTheRealLibraryFindsEveryEntryOfTheDirectoryDomain(t *testing.T) {
	people := testgal.Generate(2000, 11, []string{"corp.example"}, "outside.example")
	people = append(people, testgal.Person{Display: "Alice Own", First: "Alice", Last: "Own", Alias: "alice", Email: "alice@corp.example"})
	var queries []string
	var qmu sync.Mutex
	s := buildContactsStackWith(t, []string{"corp.example"}, func(c *engine.Config, m *model) {
		c.Directory.Full = true
		c.Directory.Spacing = time.Millisecond
		c.Directory.DailyCap = 100000
		m.f.on("Search", galANRHandler(people, 2, &queries, &qmu)) // the server answers from 2 characters
	})
	want := testgal.InDomain(people, "corp.example")
	deadline := time.Now().Add(60 * time.Second)
	for {
		sw, _ := s.acct.Store().Sweep()
		if sw.ID == 1 && sw.Phase == store.SweepIdle && sw.FinishedAt != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sweep did not finish: %+v", sw)
		}
		time.Sleep(20 * time.Millisecond)
	}
	sw, _ := s.acct.Store().Sweep()
	cards, _ := s.acct.Store().DirectoryCards()
	if len(cards) != len(want) || sw.LastCapped || sw.CalibLen != 2 {
		t.Fatalf("%d cards for %d entries; sweep %+v", len(cards), len(want), sw)
	}
	for _, p := range want {
		if _, err := s.acct.Store().DirectoryCardByEmail(p.Email); err != nil {
			t.Fatalf("no card for an entry of the directory domain: %v", err)
		}
	}
	for _, c := range cards {
		if strings.Contains(string(c.VCard), "outside.example") || strings.Contains(string(c.VCard), "HOMEPHONE-NOT-KEPT") {
			t.Fatalf("a card holds what must not be kept:\n%s", c.VCard)
		}
	}
	qmu.Lock()
	defer qmu.Unlock()
	if qs := strings.Join(queries[:3], "|"); qs != "a|al|aa" {
		t.Errorf("the first queries are %q: the calibration with the own local part (a is refused, al answered), then the walk of two characters", qs)
	}
	for _, r := range s.m.f.requests("Search") {
		if rg, _ := text(r.Root, "Range"); rg != "0-99" {
			t.Fatalf("Range = %q", rg)
		}
	}
	for _, bad := range []string{"alsulami", "outside.example", "Alice Own", "alice@corp.example"} {
		if strings.Contains(s.logText(), bad) {
			t.Errorf("the log contains %q", bad)
		}
	}
	t.Logf("%d entries (%d in the domain): %d searches", len(people), len(want), sw.Searches)
}

// The upgrade from 0.1.8 over the real library: a hierarchy key committed with the contacts off
// is read again from key "0", once, and the mail is where it was whether or not Exchange keeps
// the collections' Sync keys across a FolderSync from "0".
func TestUpgradedInstallReadsTheHierarchyAgainOverTheRealLibrary(t *testing.T) {
	for _, resets := range []bool{false, true} {
		t.Run(fmt.Sprintf("Exchange invalidates the Sync keys: %v", resets), func(t *testing.T) {
			s := buildContactsStackWith(t, nil, func(c *engine.Config, m *model) {
				c.ContactsEnabled = false // 0.1.8, or 0.2.x without CardDAV
				m.hierZeroResets = resets
				for i := 0; i < 7; i++ {
					m.deliver("Inbox", realMail(fmt.Sprintf("up%d@x", i), "s", 0), i%2 == 0, time.Now().Add(-time.Duration(10-i)*time.Hour))
				}
			})
			inbox := func() (*store.Folder, []uint32) {
				f, err := s.acct.Store().FolderByPath("INBOX")
				if err != nil {
					return nil, nil
				}
				ms, _ := s.acct.Store().ListMessages(f.ID)
				var uids []uint32
				for _, m := range ms {
					uids = append(uids, m.UID)
				}
				return f, uids
			}
			var f0 *store.Folder
			var uids0 []uint32
			eventually(t, "seven messages", func() bool { f0, uids0 = inbox(); return f0 != nil && len(uids0) == 7 })
			if fs, _ := s.acct.Store().ContactFolders(); len(fs) != 0 {
				t.Fatalf("contact folders with the feature off: %v", fs)
			}
			if z, _ := s.zeroKeys(); z != 1 {
				t.Fatalf("FolderSync keys %v", s.m.hierSeen)
			}

			for i := 0; i < 6; i++ {
				s.cm.set(fmt.Sprintf("c%d", i), map[string]string{"FirstName": fmt.Sprintf("First%d", i), "LastName": "Last", "Email1Address": fmt.Sprintf("p%d@example.test", i)})
			}
			s.restart(true)
			def := s.book(store.DefaultBookID)
			eventually(t, "the first download (6 contacts in windows of 4)", func() bool { return len(s.cards(def)) == 6 })
			if z, _ := s.zeroKeys(); z != 2 {
				t.Errorf("%d FolderSyncs from key 0, want 2 (the first one and the upgrade): %v", z, s.m.hierSeen)
			}
			if kept, _ := s.acct.Store().ContactFoldersKept(); !kept {
				t.Errorf("the marker is not set")
			}
			if resets {
				eventually(t, "the silent key reset of the mail", func() bool {
					return strings.Contains(s.logText(), "server reset the sync key")
				})
			}
			holds(t, "the mail stays as it was", 400*time.Millisecond, func() bool {
				f, uids := inbox()
				return f != nil && fmt.Sprint(uids) == fmt.Sprint(uids0) && f.UIDValidity == f0.UIDValidity
			})
			if c, _ := s.acct.Store().Counts(f0.ID); c.Messages != 7 || c.UIDNext != 8 {
				t.Errorf("counts %+v", c)
			}

			// A restart does not read it again; off and on does, once.
			z0, n0 := s.zeroKeys()
			s.restart(true)
			s.book(store.DefaultBookID)
			eventually(t, "a FolderSync after the restart", func() bool { _, n := s.zeroKeys(); return n > n0 })
			if z, _ := s.zeroKeys(); z != z0 || z != 2 {
				t.Errorf("a restart repeated the full FolderSync: %v", s.m.hierSeen)
			}
			s.restart(false)
			eventually(t, "a hierarchy with the contacts off", func() bool { kept, _ := s.acct.Store().ContactFoldersKept(); return !kept })
			s.restart(true)
			eventually(t, "one more full FolderSync", func() bool { z, _ := s.zeroKeys(); return z == 3 })
			holds(t, "and no other", 300*time.Millisecond, func() bool { z, _ := s.zeroKeys(); return z == 3 })
			if _, uids := inbox(); fmt.Sprint(uids) != fmt.Sprint(uids0) {
				t.Errorf("UIDs %v, were %v", uids, uids0)
			}
		})
	}
}

// holds asserts that cond stays true for d.
func holds(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if !cond() {
			t.Fatalf("did not hold: %s", what)
		}
	}
}
