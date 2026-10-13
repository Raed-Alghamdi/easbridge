package carddav_test

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"

	"easbridge/internal/store"
	"easbridge/internal/vcard"
)

func TestDiscoveryAsMailFlowDoesIt(t *testing.T) {
	h := newHarness(t)
	h.seedBook("c1", "Contacts", store.TypeContacts, map[string]string{"1": "Ada Lovelace", "2": "عبدالله القحطاني"})
	h.seedBook("c2", "Clients", store.TypeUserContacts, map[string]string{"3": "Zed"})
	h.putDirectory("pat@corp.example", "Pat Smith")

	// 1. current-user-principal at the URL given in MailFlow (the bare origin).
	r := h.propfind("/", "0", propBody("current-user-principal"))
	if r.code != 207 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	ms := parseMS(t, r.body)
	if p := ms.Responses[0].props("200")["current-user-principal"]; !strings.Contains(p.Inner, "<href>/dav/principals/me/</href>") && !strings.Contains(p.Inner, "/dav/principals/me/") {
		t.Fatalf("principal: %+v", p)
	}
	// ... and through the well-known redirect.
	if r := h.do("PROPFIND", "/.well-known/carddav", ""); r.code != 301 || r.header.Get("Location") != "/dav/" {
		t.Fatalf("well-known %d %q", r.code, r.header.Get("Location"))
	}
	r = h.propfind("/dav/", "0", propBody("current-user-principal"))
	if r.code != 207 || !strings.Contains(r.body, "/dav/principals/me/") {
		t.Fatalf("/dav/: %d %s", r.code, r.body)
	}

	// 2. addressbook-home-set on the principal.
	r = h.propfind("/dav/principals/me/", "0", propBody("C:addressbook-home-set"))
	ms = parseMS(t, r.body)
	if p := ms.Responses[0].props("200")["addressbook-home-set"]; !strings.Contains(p.Inner, "/dav/addressbooks/me/") {
		t.Fatalf("home set: %s", r.body)
	}

	// 3. The books: resourcetype, displayname, getctag at Depth 1.
	r = h.propfind("/dav/addressbooks/me/", "1", propBody("resourcetype", "displayname", "cs:getctag"))
	ms = parseMS(t, r.body)
	books := map[string]msResponse{}
	for _, resp := range ms.Responses {
		rt := resp.props("200")["resourcetype"]
		if strings.Contains(rt.Inner, "addressbook") {
			books[resp.Props200Name()] = resp
		}
	}
	if len(books) != 3 {
		t.Fatalf("books %v in\n%s", books, r.body)
	}
	for name, resp := range books {
		if resp.props("200")["getctag"].Inner == "" {
			t.Errorf("%s has no ctag", name)
		}
	}
	for _, want := range []string{"Exchange contacts", "Clients", "Directory"} {
		if _, ok := books[want]; !ok {
			t.Errorf("no book called %q: %v", want, keys(books))
		}
	}
	// The home itself is a collection and not an address book.
	home, _ := ms.byHref("/dav/addressbooks/me/")
	if strings.Contains(home.props("200")["resourcetype"].Inner, "addressbook") {
		t.Errorf("the home is not an address book")
	}
}

func (r msResponse) Props200Name() string {
	return strings.TrimSpace(r.props("200")["displayname"].Inner)
}

func keys(m map[string]msResponse) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestTheBareOriginAnswersLikeTheDAVRoot(t *testing.T) {
	h := newHarness(t)
	for _, p := range []string{"/", "/dav/", "/dav"} {
		r := h.propfind(p, "0", "")
		if r.code != 207 {
			t.Errorf("%s: %d", p, r.code)
		}
	}
}

func TestPropfindDepthRules(t *testing.T) {
	h := newHarness(t)
	h.seedBook("c1", "Contacts", store.TypeContacts, map[string]string{"1": "A", "2": "B", "3": "C"})
	book := "/dav/addressbooks/me/contacts/"
	for _, depth := range []string{"infinity", "", "Infinity", "2", "x"} {
		r := h.propfind(book, depth, "")
		if r.code != 403 || !strings.Contains(r.body, "propfind-finite-depth") {
			t.Errorf("Depth %q: %d %s", depth, r.code, r.body)
		}
		if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/xml") {
			t.Errorf("Depth %q: content type %q", depth, ct)
		}
	}
	if r := h.propfind(book, "0", ""); r.code != 207 || len(parseMS(t, r.body).Responses) != 1 {
		t.Errorf("Depth 0 lists the book only: %d", r.code)
	}
	if r := h.propfind(book, "1", ""); r.code != 207 || len(parseMS(t, r.body).Responses) != 4 {
		t.Errorf("Depth 1 lists the book and its cards: %d %d", r.code, len(parseMS(t, r.body).Responses))
	}
	// A card has no children.
	cs, _ := h.acct.ContactCards(mustFolder(t, h).ID)
	if r := h.propfind("/dav/addressbooks/me/contacts/"+cs[0].UID+".vcf", "1", ""); r.code != 207 || len(parseMS(t, r.body).Responses) != 1 {
		t.Errorf("a card at Depth 1: %d", r.code)
	}
	// The home lists the books at Depth 1 and only itself at Depth 0.
	if r := h.propfind("/dav/addressbooks/me/", "0", ""); len(parseMS(t, r.body).Responses) != 1 {
		t.Errorf("home depth 0")
	}
}

func mustFolder(t *testing.T, h *harness) *store.ContactFolder {
	t.Helper()
	fs, err := h.acct.ContactFolders()
	if err != nil || len(fs) == 0 {
		t.Fatal(err)
	}
	return &fs[0]
}

func TestPropertiesOfEveryResource(t *testing.T) {
	h := newHarness(t)
	h.seedBook("c1", "Contacts", store.TypeContacts, map[string]string{"1": "Ada"})
	cs, _ := h.acct.ContactCards(mustFolder(t, h).ID)
	uid := cs[0].UID
	all := []string{"resourcetype", "displayname", "cs:getctag", "getetag", "getcontenttype", "C:supported-address-data", "current-user-principal",
		"principal-URL", "C:addressbook-home-set", "supported-report-set", "current-user-privilege-set", "getcontentlength", "nothing-like-this", "getlastmodified"}
	type want struct{ found, missing []string }
	for path, w := range map[string]want{
		"/":                              {[]string{"resourcetype", "current-user-principal", "current-user-privilege-set"}, nil},
		"/dav/principals/me/":            {[]string{"resourcetype", "displayname", "principal-URL", "addressbook-home-set", "current-user-principal", "current-user-privilege-set"}, nil},
		"/dav/addressbooks/me/":          {[]string{"resourcetype", "displayname", "current-user-principal", "current-user-privilege-set"}, nil},
		"/dav/addressbooks/me/contacts/": {[]string{"resourcetype", "displayname", "getctag", "supported-report-set", "supported-address-data", "current-user-principal", "current-user-privilege-set"}, nil},
		"/dav/addressbooks/me/contacts/" + uid + ".vcf": {[]string{"resourcetype", "getetag", "getcontenttype", "getcontentlength", "current-user-principal", "current-user-privilege-set"}, nil},
	} {
		r := h.propfind(path, "0", propBody(all...))
		if r.code != 207 {
			t.Errorf("%s: %d", path, r.code)
			continue
		}
		resp := parseMS(t, r.body).Responses[0]
		got := resp.props("200")
		for _, f := range w.found {
			if _, ok := got[f]; !ok {
				t.Errorf("%s lacks %s: %s", path, f, r.body)
			}
		}
		miss := resp.props("404")
		for _, name := range []string{"nothing-like-this", "getlastmodified"} {
			if _, ok := miss[name]; !ok {
				t.Errorf("%s: %s should be in the 404 propstat: %s", path, name, r.body)
			}
		}
		if len(got)+len(miss) != len(all) {
			t.Errorf("%s: %d found and %d missing for %d asked", path, len(got), len(miss), len(all))
		}
		if p := got["current-user-privilege-set"]; strings.Contains(p.Inner, "write") || strings.Contains(p.Inner, "all") || strings.Contains(p.Inner, "bind") || strings.Contains(p.Inner, "unbind") || !strings.Contains(p.Inner, "read") {
			t.Errorf("%s: privileges %q (read only)", path, p.Inner)
		}
	}
}

func TestPropertyValues(t *testing.T) {
	h := newHarness(t)
	f := h.seedBook("c1", "Ignored name", store.TypeContacts, map[string]string{"1": "Ada"})
	cs, _ := h.acct.ContactCards(f.ID)
	uid := cs[0].UID
	r := h.propfind("/dav/addressbooks/me/contacts/", "0", propBody("displayname", "cs:getctag", "resourcetype", "supported-report-set"))
	p := parseMS(t, r.body).Responses[0].props("200")
	if strings.TrimSpace(p["displayname"].Inner) != "Exchange contacts" {
		t.Errorf("the default book is named by the setting: %q", p["displayname"].Inner)
	}
	if strings.TrimSpace(p["getctag"].Inner) != f.CTag {
		t.Errorf("ctag %q, want %q", p["getctag"].Inner, f.CTag)
	}
	for _, want := range []string{"collection", "addressbook"} {
		if !strings.Contains(p["resourcetype"].Inner, want) {
			t.Errorf("resourcetype %q lacks %s", p["resourcetype"].Inner, want)
		}
	}
	for _, rep := range []string{"addressbook-query", "addressbook-multiget"} {
		if !strings.Contains(p["supported-report-set"].Inner, rep) {
			t.Errorf("supported-report-set lacks %s", rep)
		}
	}
	if strings.Contains(p["supported-report-set"].Inner, "sync-collection") {
		t.Errorf("sync-collection must not be advertised")
	}
	r = h.propfind("/dav/addressbooks/me/contacts/"+uid+".vcf", "0", propBody("getetag", "getcontenttype", "getcontentlength", "resourcetype"))
	p = parseMS(t, r.body).Responses[0].props("200")
	card := cs[0]
	if strings.TrimSpace(p["getetag"].Inner) != card.ETag || !strings.HasPrefix(card.ETag, `"`) {
		t.Errorf("etag %q vs %q", p["getetag"].Inner, card.ETag)
	}
	if strings.TrimSpace(p["getcontenttype"].Inner) != "text/vcard; charset=utf-8" {
		t.Errorf("content type %q", p["getcontenttype"].Inner)
	}
	if strings.TrimSpace(p["getcontentlength"].Inner) != strconv.Itoa(len(card.VCard)) {
		t.Errorf("length %q", p["getcontentlength"].Inner)
	}
	if strings.Contains(p["resourcetype"].Inner, "collection") {
		t.Errorf("a card is not a collection")
	}
}

func TestAllpropAndPropname(t *testing.T) {
	h := newHarness(t)
	h.seedBook("c1", "Contacts", store.TypeContacts, map[string]string{"1": "Ada"})
	for _, body := range []string{"", `<propfind xmlns="DAV:"><allprop/></propfind>`} {
		r := h.propfind("/dav/addressbooks/me/contacts/", "0", body)
		p := parseMS(t, r.body).Responses[0].props("200")
		for _, want := range []string{"resourcetype", "displayname", "getctag"} {
			if _, ok := p[want]; !ok {
				t.Errorf("allprop (%q) lacks %s", body, want)
			}
		}
		if _, ok := p["current-user-principal"]; ok {
			t.Errorf("allprop does not carry current-user-principal")
		}
	}
	r := h.propfind("/dav/addressbooks/me/contacts/", "0", `<propfind xmlns="DAV:"><propname/></propfind>`)
	p := parseMS(t, r.body).Responses[0].props("200")
	if _, ok := p["getctag"]; !ok || strings.TrimSpace(p["getctag"].Inner) != "" {
		t.Errorf("propname gives names without values: %+v", p["getctag"])
	}
}

func TestUnknownPropertiesKeepTheirNamespaceInThe404Propstat(t *testing.T) {
	h := newHarness(t)
	r := h.propfind("/dav/addressbooks/me/", "0", `<propfind xmlns="DAV:" xmlns:x="http://example.test/ns"><prop><x:thing/><getlastmodified/><resourcetype/></prop></propfind>`)
	if r.code != 207 || !strings.Contains(r.body, `http://example.test/ns`) || !strings.Contains(r.body, "HTTP/1.1 404 Not Found") || !strings.Contains(r.body, "HTTP/1.1 200 OK") {
		t.Errorf("%d %s", r.code, r.body)
	}
	// A hostile property name is not echoed into the XML.
	r = h.propfind("/dav/addressbooks/me/", "0", `<propfind xmlns="DAV:"><prop><a-b.c/></prop></propfind>`)
	parseMS(t, r.body)
}

func TestNotFoundPaths(t *testing.T) {
	h := newHarness(t)
	h.seedBook("c1", "Contacts", store.TypeContacts, map[string]string{"1": "A"})
	for _, p := range []string{"/nothing", "/dav/addressbooks/me/nobook/", "/dav/addressbooks/me/contacts/nosuch.vcf", "/dav/addressbooks/me/contacts/a/b.vcf",
		"/dav/addressbooks/me/contacts/x.txt", "/dav/addressbooks/other/", "/dav/principals/", "/dav/addressbooks/", "/dav/addressbooks/me/../me/contacts/", "//dav/",
		"/dav/addressbooks/me/contacts/%2e%2e/", "/dav/addressbooks/me/contacts/.vcf"} {
		if r := h.propfind(p, "0", ""); r.code != 404 {
			t.Errorf("PROPFIND %s: %d", p, r.code)
		}
		if r := h.do("GET", p, ""); r.code != 404 && r.code != 405 {
			t.Errorf("GET %s: %d", p, r.code)
		}
	}
	// A book without its trailing slash is the same book.
	if r := h.propfind("/dav/addressbooks/me/contacts", "0", ""); r.code != 207 {
		t.Errorf("no trailing slash: %d", r.code)
	}
}

func TestOptions(t *testing.T) {
	h := newHarness(t)
	f := h.seedBook("c1", "Contacts", store.TypeContacts, map[string]string{"1": "A"})
	cs, _ := h.acct.ContactCards(f.ID)
	for path, allow := range map[string]string{
		"/": "OPTIONS, PROPFIND", "/dav/": "OPTIONS, PROPFIND", "/dav/addressbooks/me/": "OPTIONS, PROPFIND",
		"/dav/addressbooks/me/contacts/":                      "OPTIONS, PROPFIND, REPORT",
		"/dav/addressbooks/me/contacts/" + cs[0].UID + ".vcf": "OPTIONS, GET, HEAD, PROPFIND",
	} {
		r := h.do("OPTIONS", path, "")
		if r.code != 200 || r.header.Get("DAV") != "1, 3, addressbook" || r.header.Get("Allow") != allow {
			t.Errorf("OPTIONS %s: %d DAV=%q Allow=%q", path, r.code, r.header.Get("DAV"), r.header.Get("Allow"))
		}
	}
	if r := h.do("OPTIONS", "/nothing", ""); r.code != 404 {
		t.Errorf("OPTIONS on nothing: %d", r.code)
	}
}

func TestTheDirectoryBookIsListedOnlyWhenTheDirectoryIsOn(t *testing.T) {
	for _, on := range []bool{true, false} {
		h := newHarness(t)
		if !on {
			h = newHarness(t, func(o *carddavOptions) { o.DirectoryEnabled = false })
		}
		r := h.propfind("/dav/addressbooks/me/", "1", propBody("resourcetype"))
		listed := strings.Contains(r.body, "/dav/addressbooks/me/directory/")
		if listed != on {
			t.Errorf("directory enabled %v, listed %v", on, listed)
		}
		if r := h.propfind("/dav/addressbooks/me/directory/", "0", ""); (r.code == 207) != on {
			t.Errorf("directory enabled %v, PROPFIND %d", on, r.code)
		}
	}
}

func TestACompleteBookIsListedBeforeItsFirstDownloadWithoutCards(t *testing.T) {
	h := newHarness(t)
	if _, err := h.acct.Update(nil, func(tx *store.Tx) error {
		_, err := tx.ApplyContactFolders([]store.FolderInput{{ServerID: "c1", DisplayName: "Contacts", EASType: store.TypeContacts}}, nil, nil, false)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f := mustFolder(t, h)
	h.apply(f.ID, store.ContactBatch{CallKey: "0", NewKey: "1"})
	h.apply(f.ID, store.ContactBatch{CallKey: "1", NewKey: "2", More: true, Upserts: []store.ContactCardIn{{ServerID: "x", UID: "eas-x", VCard: vcard.Card{UID: "eas-x", Name: "Hidden"}.Marshal()}}})
	r := h.propfind("/dav/addressbooks/me/contacts/", "1", "")
	if ms := parseMS(t, r.body); len(ms.Responses) != 1 {
		t.Errorf("a book lists no cards before its first download has finished: %v", ms.hrefs())
	}
	if r := h.do("GET", "/dav/addressbooks/me/contacts/eas-x.vcf", ""); r.code != 404 {
		t.Errorf("a staged card is served: %d", r.code)
	}
	h.apply(f.ID, store.ContactBatch{CallKey: "2", NewKey: "3"})
	if ms := parseMS(t, h.propfind("/dav/addressbooks/me/contacts/", "1", "").body); len(ms.Responses) != 2 {
		t.Errorf("after the download: %v", ms.hrefs())
	}
}

func TestTheCTagMovesWithTheCards(t *testing.T) {
	h := newHarness(t)
	f := h.seedBook("c1", "Contacts", store.TypeContacts, map[string]string{"1": "A"})
	ctag := func() string {
		p := parseMS(t, h.propfind("/dav/addressbooks/me/contacts/", "0", propBody("cs:getctag")).body).Responses[0].props("200")
		return strings.TrimSpace(p["getctag"].Inner)
	}
	c0 := ctag()
	h.apply(f.ID, store.ContactBatch{CallKey: "2", NewKey: "3", Upserts: []store.ContactCardIn{{ServerID: "2", UID: "eas-2", VCard: vcard.Card{UID: "eas-2", Name: "B"}.Marshal()}}})
	c1 := ctag()
	h.apply(f.ID, store.ContactBatch{CallKey: "3", NewKey: "4"})
	must(t, c0 != c1, "an add must change the ctag")
	must(t, ctag() == c1, "a sync without changes must not")
	h.apply(f.ID, store.ContactBatch{CallKey: "4", NewKey: "5", Deleted: []string{"2"}})
	must(t, ctag() != c1, "a delete must change the ctag")
	// The directory book too.
	dtag := func() string {
		p := parseMS(t, h.propfind("/dav/addressbooks/me/directory/", "0", propBody("cs:getctag")).body).Responses[0].props("200")
		return strings.TrimSpace(p["getctag"].Inner)
	}
	d0 := dtag()
	h.putDirectory("pat@corp.example", "Pat")
	must(t, dtag() != d0, "a new directory card changes the ctag")
}

func TestTheServerWaitsForTheMailboxBeforeAnsweringAndNeverForOptions(t *testing.T) {
	var mu sync.Mutex
	var users []string
	h := newHarness(t, func(o *carddavOptions) {
		o.Ready = func(_ context.Context, user string) {
			mu.Lock()
			users = append(users, user)
			mu.Unlock()
		}
	})
	h.do("OPTIONS", "/", "")
	h.propfind("/dav/addressbooks/me/", "1", "")
	h.do("GET", "/dav/addressbooks/me/contacts/x.vcf", "")
	h.do("PUT", "/dav/addressbooks/me/contacts/x.vcf", "x")
	h.do("PROPFIND", "/", "", hdr("Authorization", "-"), hdr("Depth", "0")) // not logged in: nothing to wait for
	mu.Lock()
	defer mu.Unlock()
	if len(users) != 3 || users[0] != testUser {
		t.Errorf("Ready was called for %q", users)
	}
}
