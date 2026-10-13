package carddav_test

import (
	"strings"
	"testing"

	"easbridge/internal/store"
)

func TestEveryMethodThatWritesIsRefusedAndNothingChanges(t *testing.T) {
	h := newHarness(t)
	f := h.seedBook("c1", "Contacts", store.TypeContacts, map[string]string{"1": "Ada", "2": "Bob"})
	h.putDirectory("pat@corp.example", "Pat")
	cs := h.mustCards(f)
	before, _ := h.acct.ContactCards(f.ID)
	dirBefore, _ := h.acct.DirectoryCards()
	ctagBefore := mustFolder(t, h).CTag
	card := "/dav/addressbooks/me/contacts/" + cs[0].UID + ".vcf"
	newCard := "/dav/addressbooks/me/contacts/new-card.vcf"
	for _, method := range []string{"PUT", "DELETE", "POST", "PATCH", "PROPPATCH", "MKCOL", "MKCALENDAR", "MKADDRESSBOOK", "COPY", "MOVE", "LOCK", "UNLOCK", "ACL", "BIND", "UNBIND", "SEARCH", "FOO"} {
		for _, path := range []string{card, newCard, "/dav/addressbooks/me/contacts/", "/dav/addressbooks/me/", "/dav/addressbooks/me/directory/", "/dav/addressbooks/me/newbook/", "/", "/nowhere"} {
			r := h.do(method, path, "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:evil\r\nFN:Evil\r\nEND:VCARD\r\n", hdr("Destination", "/dav/addressbooks/me/contacts/x.vcf"), hdr("Content-Type", "text/vcard"))
			if r.code != 405 {
				t.Errorf("%s %s: %d (a write method is refused with 405)", method, path, r.code)
				continue
			}
			allow := r.header.Get("Allow")
			for _, m := range []string{"OPTIONS", "PROPFIND", "REPORT", "GET", "HEAD"} {
				if !strings.Contains(allow, m) {
					t.Errorf("%s %s: Allow %q lacks %s", method, path, allow, m)
				}
			}
			for _, m := range []string{"PUT", "DELETE", "POST", "PROPPATCH", "MKCOL", "COPY", "MOVE", "LOCK"} {
				if strings.Contains(allow, m) {
					t.Errorf("%s %s: Allow %q names %s", method, path, allow, m)
				}
			}
		}
	}
	after, _ := h.acct.ContactCards(f.ID)
	dirAfter, _ := h.acct.DirectoryCards()
	if len(after) != len(before) || len(dirAfter) != len(dirBefore) || mustFolder(t, h).CTag != ctagBefore {
		t.Errorf("a refused write changed something")
	}
	for i := range before {
		if before[i].ETag != after[i].ETag {
			t.Errorf("a card changed")
		}
	}
	if r := h.do("GET", newCard, ""); r.code != 404 {
		t.Errorf("a PUT created a card: %d", r.code)
	}
}

func TestRefusedWritesStillNeedALoginFirst(t *testing.T) {
	h := newHarness(t)
	if r := h.do("PUT", "/dav/addressbooks/me/contacts/x.vcf", "x", hdr("Authorization", "-")); r.code != 401 {
		t.Errorf("%d", r.code)
	}
}

func TestGetAndHeadServeTheExactBytesOfACard(t *testing.T) {
	h := newHarness(t)
	f := h.seedBook("c1", "Contacts", store.TypeContacts, map[string]string{"1": "عبدالله القحطاني"})
	c := h.mustCards(f)[0]
	href := "/dav/addressbooks/me/contacts/" + c.UID + ".vcf"
	r := h.do("GET", href, "")
	if r.code != 200 || r.body != string(c.VCard) {
		t.Fatalf("%d %q", r.code, r.body)
	}
	if r.header.Get("Content-Type") != "text/vcard; charset=utf-8" || r.header.Get("ETag") != c.ETag || r.header.Get("Content-Length") != itoaN(len(c.VCard)) {
		t.Errorf("headers %v", r.header)
	}
	if r.header.Get("Cache-Control") != "no-store" || r.header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("a private response must not be cached or sniffed: %v", r.header)
	}
	hd := h.do("HEAD", href, "")
	if hd.code != 200 || hd.body != "" || hd.header.Get("ETag") != c.ETag || hd.header.Get("Content-Length") != itoaN(len(c.VCard)) {
		t.Errorf("HEAD: %d %q %v", hd.code, hd.body, hd.header)
	}
	if n := h.do("GET", href, "", hdr("If-None-Match", c.ETag)); n.code != 304 || n.body != "" {
		t.Errorf("If-None-Match: %d", n.code)
	}
	if n := h.do("GET", href, "", hdr("If-None-Match", `"other", `+c.ETag)); n.code != 304 {
		t.Errorf("a list: %d", n.code)
	}
	if n := h.do("GET", href, "", hdr("If-None-Match", `"other"`)); n.code != 200 {
		t.Errorf("a different tag: %d", n.code)
	}
	// A collection is not a document.
	for _, p := range []string{"/dav/addressbooks/me/contacts/", "/dav/addressbooks/me/", "/dav/", "/"} {
		for _, m := range []string{"GET", "HEAD"} {
			if r := h.do(m, p, ""); r.code != 405 || r.header.Get("Allow") == "" {
				t.Errorf("%s %s: %d", m, p, r.code)
			}
		}
	}
	if r := h.do("GET", "/dav/addressbooks/me/contacts/eas-nope.vcf", ""); r.code != 404 {
		t.Errorf("%d", r.code)
	}
}

func itoaN(n int) string {
	var d []byte
	if n == 0 {
		return "0"
	}
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}
