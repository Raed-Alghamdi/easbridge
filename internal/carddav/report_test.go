package carddav_test

import (
	"fmt"
	"strings"
	"testing"

	"easbridge/internal/store"
	"easbridge/internal/vcard"
)

const queryAll = `<?xml version="1.0" encoding="utf-8"?>
<C:addressbook-query xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><prop>
  <getetag/><C:address-data/></prop></C:addressbook-query>`

func (h *harness) report(path, depth, body string) response {
	h.t.Helper()
	opts := []reqOpt{hdr("Content-Type", "application/xml")}
	if depth != "" {
		opts = append(opts, hdr("Depth", depth))
	}
	return h.do("REPORT", path, body, opts...)
}

func TestAddressbookQueryReturnsEveryCardWithItsETagAndData(t *testing.T) {
	h := newHarness(t)
	f := h.seedBook("c1", "Contacts", store.TypeContacts, map[string]string{"1": "Ada Lovelace", "2": "عبدالله القحطاني", "3": "Mixed & <odd> ]]> name"})
	r := h.report("/dav/addressbooks/me/contacts/", "1", queryAll)
	if r.code != 207 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	ms := parseMS(t, r.body)
	cs, _ := h.acct.ContactCards(f.ID)
	if len(ms.Responses) != 3 {
		t.Fatalf("%d responses", len(ms.Responses))
	}
	for _, c := range cs {
		resp, ok := ms.byHref("/dav/addressbooks/me/contacts/" + c.UID + ".vcf")
		if !ok {
			t.Errorf("card %s missing", c.UID)
			continue
		}
		p := resp.props("200")
		if strings.TrimSpace(p["getetag"].Inner) != c.ETag {
			t.Errorf("etag %q vs %q", p["getetag"].Inner, c.ETag)
		}
		// The data survives the XML round trip byte for byte (CRLF included).
		var got struct {
			V string `xml:",chardata"`
		}
		if err := decodeInner(p["address-data"].Inner, &got); err != nil {
			t.Fatal(err)
		}
		if got.V != string(c.VCard) {
			t.Errorf("address-data differs:\n got %q\nwant %q", got.V, c.VCard)
		}
	}
	if !strings.Contains(r.body, "&#13;") {
		t.Errorf("the carriage returns of the vCards are sent as references (XML would normalize a bare one away)")
	}
	if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/xml") {
		t.Errorf("content type %q", ct)
	}
}

func TestQueryOfAnEmptyOrIncompleteBook(t *testing.T) {
	h := newHarness(t)
	if _, err := h.acct.Update(nil, func(tx *store.Tx) error {
		_, err := tx.ApplyContactFolders([]store.FolderInput{{ServerID: "c1", DisplayName: "Contacts", EASType: 9}}, nil, nil, false)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r := h.report("/dav/addressbooks/me/contacts/", "1", queryAll)
	if r.code != 207 || len(parseMS(t, r.body).Responses) != 0 {
		t.Errorf("%d %s", r.code, r.body)
	}
}

func TestQueryFiltersAreRefusedNotIgnored(t *testing.T) {
	h := newHarness(t)
	h.seedBook("c1", "Contacts", store.TypeContacts, map[string]string{"1": "Ada"})
	withFilter := func(filter string) string {
		return `<C:addressbook-query xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><prop><getetag/></prop>` + filter + `</C:addressbook-query>`
	}
	for name, f := range map[string]string{
		"a prop-filter": `<C:filter><C:prop-filter name="FN"><C:text-match collation="i;unicode-casemap">ada</C:text-match></C:prop-filter></C:filter>`,
		"anyof":         `<C:filter test="anyof"><C:prop-filter name="EMAIL"/></C:filter>`,
		"hostile name":  `<C:filter><C:prop-filter name="x&quot;&gt;&lt;script"/></C:filter>`,
	} {
		r := h.report("/dav/addressbooks/me/contacts/", "1", withFilter(f))
		if r.code != 403 || !strings.Contains(r.body, "supported-filter") {
			t.Errorf("%s: %d %s", name, r.code, r.body)
		}
		if strings.Contains(r.body, "script") {
			t.Errorf("%s: the filter name was echoed", name)
		}
	}
	// No filter and an empty filter match everything.
	for _, f := range []string{"", `<C:filter/>`, `<C:filter test="anyof"></C:filter>`} {
		r := h.report("/dav/addressbooks/me/contacts/", "1", withFilter(f))
		if r.code != 207 || len(parseMS(t, r.body).Responses) != 1 {
			t.Errorf("filter %q: %d", f, r.code)
		}
	}
}

func TestQueryLimit(t *testing.T) {
	h := newHarness(t)
	h.seedBook("c1", "Contacts", store.TypeContacts, map[string]string{"1": "A", "2": "B", "3": "C"})
	body := `<C:addressbook-query xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><prop><getetag/></prop><C:limit><C:nresults>2</C:nresults></C:limit></C:addressbook-query>`
	r := h.report("/dav/addressbooks/me/contacts/", "1", body)
	ms := parseMS(t, r.body)
	if r.code != 207 || len(ms.Responses) != 3 || !strings.Contains(r.body, "507") {
		t.Errorf("a limited query is cut and says so with a 507: %d %s", r.code, r.body)
	}
}

func TestMultiget(t *testing.T) {
	h := newHarness(t)
	f := h.seedBook("c1", "Contacts", store.TypeContacts, map[string]string{"1": "Ada", "2": "Bob"})
	h.seedBook("c2", "Clients", store.TypeUserContacts, map[string]string{"9": "Other book"})
	cs, _ := h.acct.ContactCards(f.ID)
	other := h.mustCards(h.mustFolderByServerID("c2"))
	href := func(uid string) string { return "/dav/addressbooks/me/contacts/" + uid + ".vcf" }
	body := `<?xml version="1.0" encoding="utf-8"?><C:addressbook-multiget xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><prop><getetag/><C:address-data/></prop>` +
		"<href>" + href(cs[0].UID) + "</href>" +
		"<href>" + href("eas-nosuchcard") + "</href>" +
		"<href>/dav/addressbooks/me/clients-or-not/" + cs[1].UID + ".vcf</href>" +
		"<href>/dav/addressbooks/me/contacts/" + other[0].UID + ".vcf</href>" + // a card of another book
		"<href>https://easbridge.example:8443" + href(cs[1].UID) + "</href>" +
		"<href>/etc/passwd</href><href>/dav/addressbooks/me/contacts/../contacts/x.vcf</href><href></href>" +
		"</C:addressbook-multiget>"
	r := h.report("/dav/addressbooks/me/contacts/", "", body) // MailFlow sends no Depth with a multiget
	if r.code != 207 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	ms := parseMS(t, r.body)
	got, notFound := 0, 0
	for _, resp := range ms.Responses {
		if strings.Contains(resp.Status, "404") {
			notFound++
			continue
		}
		if len(resp.props("200")) == 2 {
			got++
		}
	}
	if got != 2 || notFound != 6 {
		t.Errorf("%d cards, %d not found:\n%s", got, notFound, r.body)
	}
	for _, resp := range ms.Responses {
		if strings.Contains(resp.Href, other[0].UID) && !strings.Contains(resp.Status, "404") {
			t.Errorf("a card of another book was returned by this one")
		}
	}
}

func (h *harness) mustFolderByServerID(sid string) *store.ContactFolder {
	h.t.Helper()
	fs, _ := h.acct.ContactFolders()
	for i := range fs {
		if fs[i].ServerID == sid {
			return &fs[i]
		}
	}
	h.t.Fatalf("no folder %s", sid)
	return nil
}

func (h *harness) mustCards(f *store.ContactFolder) []store.Card {
	cs, err := h.acct.ContactCards(f.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return cs
}

func TestMultigetTooManyHrefs(t *testing.T) {
	h := newHarness(t)
	h.seedBook("c1", "Contacts", store.TypeContacts, map[string]string{"1": "A"})
	var b strings.Builder
	b.WriteString(`<C:addressbook-multiget xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><prop><getetag/></prop>`)
	for i := 0; i < 1001; i++ {
		b.WriteString("<href>/a</href>")
	}
	b.WriteString("</C:addressbook-multiget>")
	if r := h.report("/dav/addressbooks/me/contacts/", "", b.String()); r.code != 413 {
		t.Errorf("%d", r.code)
	}
}

func TestMultigetOf100IsTheBatchMailFlowSends(t *testing.T) {
	h := newHarness(t)
	cards := map[string]string{}
	for i := 0; i < 120; i++ {
		cards[fmt.Sprint(i)] = fmt.Sprintf("Person %03d", i)
	}
	f := h.seedBook("c1", "Contacts", store.TypeContacts, cards)
	cs := h.mustCards(f)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n" + `<C:addressbook-multiget xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><prop>` + "\n  <getetag/><C:address-data/></prop>")
	for _, c := range cs[:100] {
		b.WriteString("<href>/dav/addressbooks/me/contacts/" + c.UID + ".vcf</href>")
	}
	b.WriteString("</C:addressbook-multiget>")
	r := h.report("/dav/addressbooks/me/contacts/", "", b.String())
	if r.code != 207 || len(parseMS(t, r.body).Responses) != 100 {
		t.Errorf("%d (%d bytes)", r.code, len(r.body))
	}
}

func TestOtherReportsAndWrongTargets(t *testing.T) {
	h := newHarness(t)
	f := h.seedBook("c1", "Contacts", store.TypeContacts, map[string]string{"1": "A"})
	cs := h.mustCards(f)
	// sync-collection is not implemented and not advertised.
	r := h.report("/dav/addressbooks/me/contacts/", "0", `<sync-collection xmlns="DAV:"><sync-token/><sync-level>1</sync-level><prop><getetag/></prop></sync-collection>`)
	if r.code != 403 || !strings.Contains(r.body, "supported-report") {
		t.Errorf("sync-collection: %d %s", r.code, r.body)
	}
	for _, p := range []string{"/dav/addressbooks/me/", "/dav/addressbooks/me/contacts/" + cs[0].UID + ".vcf", "/", "/dav/principals/me/"} {
		if r := h.report(p, "1", queryAll); r.code != 405 || r.header.Get("Allow") == "" {
			t.Errorf("REPORT on %s: %d", p, r.code)
		}
	}
	if r := h.report("/dav/addressbooks/me/nobook/", "1", queryAll); r.code != 404 {
		t.Errorf("REPORT on nothing: %d", r.code)
	}
	if r := h.report("/dav/addressbooks/me/contacts/", "infinity", queryAll); r.code != 400 {
		t.Errorf("Depth infinity: %d", r.code)
	}
	if r := h.report("/dav/addressbooks/me/contacts/", "1", ""); r.code != 400 {
		t.Errorf("empty body: %d", r.code)
	}
	if r := h.report("/dav/addressbooks/me/contacts/", "1", "<not xml"); r.code != 400 {
		t.Errorf("broken XML: %d", r.code)
	}
	// Properties other than the ones the cards have are reported missing, per card.
	body := `<C:addressbook-query xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><prop><getetag/><getlastmodified/></prop></C:addressbook-query>`
	r = h.report("/dav/addressbooks/me/contacts/", "1", body)
	resp := parseMS(t, r.body).Responses[0]
	if _, ok := resp.props("404")["getlastmodified"]; !ok || len(resp.props("200")) != 1 {
		t.Errorf("%s", r.body)
	}
}

func TestDirectoryBookReports(t *testing.T) {
	h := newHarness(t)
	c := h.putDirectory("pat@corp.example", "Pat Smith")
	r := h.report("/dav/addressbooks/me/directory/", "1", queryAll)
	ms := parseMS(t, r.body)
	if r.code != 207 || len(ms.Responses) != 1 || ms.Responses[0].Href != "/dav/addressbooks/me/directory/"+vcard.DirectoryUID("pat@corp.example")+".vcf" {
		t.Fatalf("%d %s", r.code, r.body)
	}
	g := h.do("GET", ms.Responses[0].Href, "")
	if g.code != 200 || g.body != string(c.VCard) {
		t.Errorf("%d %q", g.code, g.body)
	}
}
