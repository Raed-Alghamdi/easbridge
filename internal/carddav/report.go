package carddav

import (
	"encoding/xml"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"easbridge/internal/store"
)

// report answers a REPORT on an address book: addressbook-query and
// addressbook-multiget (RFC 6352 sections 8.6 and 8.7).
func (s *Server) report(w http.ResponseWriter, r *http.Request, user string, b *bookInfo) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	root, err := parseXML(body)
	if err != nil {
		http.Error(w, "bad REPORT request: "+err.Error(), http.StatusBadRequest)
		return
	}
	// A Depth other than 0 and 1 is not something to answer for a whole book either.
	if d := r.Header.Get("Depth"); d != "" && d != "0" && d != "1" {
		http.Error(w, "Depth must be 0 or 1", http.StatusBadRequest)
		return
	}
	switch {
	case root.name.Space == nsCard && root.name.Local == "addressbook-query":
		s.query(w, user, b, root)
	case root.name.Space == nsCard && root.name.Local == "addressbook-multiget":
		s.multiget(w, user, b, root)
	default:
		writeError(w, http.StatusForbidden, "<D:supported-report/>")
	}
}

// reportProps are the properties a REPORT asks for.
type reportProps struct {
	etag, data, ctype bool
	other             []xml.Name
}

func readReportProps(root *node) reportProps {
	var rp reportProps
	if p := root.child(nsDAV, "prop"); p != nil {
		for _, c := range p.children {
			switch {
			case c.name.Space == nsDAV && c.name.Local == "getetag":
				rp.etag = true
			case c.name.Space == nsDAV && c.name.Local == "getcontenttype":
				rp.ctype = true
			case c.name.Space == nsCard && c.name.Local == "address-data":
				rp.data = true // a selection of properties inside is ignored: the whole card is returned
			default:
				rp.other = append(rp.other, c.name)
			}
		}
	}
	return rp
}

func (rp reportProps) render(c store.Card) []string {
	var out []string
	if rp.etag {
		out = append(out, "<D:getetag>"+escapeText(c.ETag)+"</D:getetag>")
	}
	if rp.ctype {
		out = append(out, "<D:getcontenttype>text/vcard; charset=utf-8</D:getcontenttype>")
	}
	if rp.data {
		out = append(out, "<C:address-data>"+escapeText(string(c.VCard))+"</C:address-data>")
	}
	return out
}

// query answers an addressbook-query. A filter with a prop-filter is not applied by
// this server, and is answered 403 supported-filter instead of returning everything.
func (s *Server) query(w http.ResponseWriter, user string, b *bookInfo, root *node) {
	if f := root.child(nsCard, "filter"); f != nil {
		for _, c := range f.children {
			if c.name.Space == nsCard && c.name.Local == "prop-filter" {
				name := c.attrs["name"]
				pre := "<C:supported-filter>"
				if safeLocal(strings.ReplaceAll(name, "-", "_")) {
					pre += `<C:prop-filter name="` + name + `"/>`
				}
				writeError(w, http.StatusForbidden, pre+"</C:supported-filter>")
				return
			}
		}
	}
	limit := 0
	if l := root.child(nsCard, "limit"); l != nil {
		if n := l.child(nsCard, "nresults"); n != nil {
			limit, _ = strconv.Atoi(strings.TrimSpace(n.text))
		}
	}
	rp := readReportProps(root)
	cards, err := s.cardsOf(user, b)
	if err != nil {
		s.fail(w, err)
		return
	}
	ms := newMultistatus()
	truncated := false
	for i, c := range cards {
		if limit > 0 && i >= limit {
			truncated = true
			break
		}
		found := rp.render(c)
		var missing []xml.Name
		missing = append(missing, rp.other...)
		ms.response(cardHref(b.id, c.UID), found, missing)
	}
	if truncated {
		ms.statusResponse(bookHref(b.id), "HTTP/1.1 507 Insufficient Storage")
	}
	writeMultistatus(w, ms)
}

// multiget answers an addressbook-multiget: the cards of the hrefs, 404 for a href that
// names no card of this book.
func (s *Server) multiget(w http.ResponseWriter, user string, b *bookInfo, root *node) {
	rp := readReportProps(root)
	var hrefs []string
	for _, c := range root.children {
		if c.name.Space == nsDAV && c.name.Local == "href" {
			hrefs = append(hrefs, strings.TrimSpace(c.text))
		}
	}
	if len(hrefs) > maxHrefs {
		http.Error(w, "too many hrefs", http.StatusRequestEntityTooLarge)
		return
	}
	ms := newMultistatus()
	prefix := bookHref(b.id)
	for _, h := range hrefs {
		p := h
		if u, err := url.Parse(h); err == nil {
			p = u.Path // an absolute URL names the same path
		}
		name, ok := strings.CutPrefix(p, prefix)
		uid, isCard := strings.CutSuffix(name, ".vcf")
		if !ok || !isCard || uid == "" || strings.Contains(uid, "/") {
			ms.statusResponse(h, "HTTP/1.1 404 Not Found")
			continue
		}
		c, err := s.cardOf(user, b, uid)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				ms.statusResponse(h, "HTTP/1.1 404 Not Found")
				continue
			}
			s.fail(w, err)
			return
		}
		ms.response(cardHref(b.id, c.UID), rp.render(*c), rp.other)
	}
	writeMultistatus(w, ms)
}
