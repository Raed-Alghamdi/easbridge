package carddav

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strconv"

	"easbridge/internal/store"
)

// A multistatus response under construction. Elements are written by hand so that the
// prefixes are the usual ones (D for DAV:, C for CardDAV, CS for the calendarserver.org
// namespace, which getctag lives in) and every text is escaped.
type multistatus struct{ b bytes.Buffer }

const multistatusOpen = `<?xml version="1.0" encoding="utf-8"?>` + "\n" +
	`<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav" xmlns:CS="http://calendarserver.org/ns/">` + "\n"

func newMultistatus() *multistatus {
	m := &multistatus{}
	m.b.WriteString(multistatusOpen)
	return m
}

// response writes one <D:response> with the properties found (status 200) and the ones
// not found (status 404).
func (m *multistatus) response(href string, found []string, missing []xml.Name) {
	m.b.WriteString("<D:response><D:href>" + escapeText(href) + "</D:href>\n")
	if len(found) > 0 {
		m.b.WriteString("<D:propstat><D:prop>")
		for _, p := range found {
			m.b.WriteString(p)
		}
		m.b.WriteString("</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat>\n")
	}
	if len(missing) > 0 {
		m.b.WriteString("<D:propstat><D:prop>")
		for i, n := range missing {
			m.b.WriteString(emptyElement(n, i))
		}
		m.b.WriteString("</D:prop><D:status>HTTP/1.1 404 Not Found</D:status></D:propstat>\n")
	}
	m.b.WriteString("</D:response>\n")
}

// statusResponse writes a <D:response> that only has a status (a href that names
// nothing in a multiget; the truncation marker of a limited query).
func (m *multistatus) statusResponse(href, status string) {
	m.b.WriteString("<D:response><D:href>" + escapeText(href) + "</D:href><D:status>" + status + "</D:status></D:response>\n")
}

func (m *multistatus) bytes() []byte {
	return append(append([]byte(nil), m.b.Bytes()...), []byte("</D:multistatus>\n")...)
}

// emptyElement renders a property that is named but not found, with its own namespace.
func emptyElement(n xml.Name, i int) string {
	switch n.Space {
	case nsDAV:
		return "<D:" + n.Local + "/>"
	case nsCard:
		return "<C:" + n.Local + "/>"
	case nsCS:
		return "<CS:" + n.Local + "/>"
	}
	if !safeLocal(n.Local) || !safeNS(n.Space) {
		return ""
	}
	p := "X" + strconv.Itoa(i)
	return "<" + p + ":" + n.Local + ` xmlns:` + p + `="` + n.Space + `"/>`
}

// writeMultistatus sends a 207.
func writeMultistatus(w http.ResponseWriter, m *multistatus) {
	b := m.bytes()
	h := w.Header()
	h.Set("Content-Type", "application/xml; charset=utf-8")
	h.Set("DAV", "1, 3, addressbook")
	h.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(http.StatusMultiStatus)
	w.Write(b)
}

// writeError sends a DAV:error body with a precondition element (403 and the like).
func writeError(w http.ResponseWriter, code int, precondition string) {
	body := `<?xml version="1.0" encoding="utf-8"?>` + "\n" +
		`<D:error xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">` + precondition + "</D:error>\n"
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(code)
	io.WriteString(w, body)
}

// readBody reads the request body (at most maxBody) and answers 413 itself when it is
// longer. A DOCTYPE or ENTITY declaration is answered 400.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if asMaxBytes(err, &mbe) {
			http.Error(w, "the request body is too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "cannot read the request", http.StatusBadRequest)
		}
		return nil, false
	}
	return b, true
}

// propRequest is what a PROPFIND asks for.
type propRequest struct {
	all      bool
	propname bool
	names    []xml.Name
}

// parsePropfind reads a PROPFIND body. An empty body is allprop (RFC 4918 9.1).
func parsePropfind(b []byte) (propRequest, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return propRequest{all: true}, nil
	}
	root, err := parseXML(b)
	if err != nil {
		return propRequest{}, err
	}
	if root.name.Space != nsDAV || root.name.Local != "propfind" {
		return propRequest{}, errBadRoot
	}
	var pr propRequest
	for _, c := range root.children {
		if c.name.Space != nsDAV {
			continue
		}
		switch c.name.Local {
		case "allprop":
			pr.all = true
		case "propname":
			pr.propname = true
		case "prop":
			for _, p := range c.children {
				pr.names = append(pr.names, p.name)
			}
		}
	}
	if !pr.all && !pr.propname && pr.names == nil {
		return propRequest{}, errBadRoot
	}
	return pr, nil
}

func (s *Server) propfind(w http.ResponseWriter, r *http.Request, user string, k kind, b *bookInfo, uid string) {
	// RFC 4918 9.1: a missing Depth header means infinity, and infinity is refused.
	depth := r.Header.Get("Depth")
	switch depth {
	case "0", "1":
	default:
		writeError(w, http.StatusForbidden, "<D:propfind-finite-depth/>")
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	req, err := parsePropfind(body)
	if err != nil {
		http.Error(w, "bad PROPFIND request: "+err.Error(), http.StatusBadRequest)
		return
	}
	ms := newMultistatus()
	switch k {
	case kCard:
		c, err := s.cardOf(user, b, uid)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			s.fail(w, err)
			return
		}
		s.writeProps(ms, req, resource{k: kCard, book: b, card: c})
	case kBook:
		s.writeProps(ms, req, resource{k: kBook, book: b})
		if depth == "1" {
			cards, err := s.cardsOf(user, b)
			if err != nil {
				s.fail(w, err)
				return
			}
			for i := range cards {
				s.writeProps(ms, req, resource{k: kCard, book: b, card: &cards[i]})
			}
		}
	case kHome:
		s.writeProps(ms, req, resource{k: kHome})
		if depth == "1" {
			books, err := s.books(user)
			if err != nil {
				s.fail(w, err)
				return
			}
			for i := range books {
				s.writeProps(ms, req, resource{k: kBook, book: &books[i]})
			}
		}
	default:
		s.writeProps(ms, req, resource{k: k})
	}
	writeMultistatus(w, ms)
}

// resource is a resource whose properties are being written.
type resource struct {
	k    kind
	book *bookInfo
	card *store.Card
}

func (rs resource) href() string {
	switch rs.k {
	case kRoot:
		return "/"
	case kDAV:
		return pathDAV
	case kPrincipal:
		return pathPrincipal
	case kHome:
		return pathHome
	case kBook:
		return bookHref(rs.book.id)
	}
	return cardHref(rs.book.id, rs.card.UID)
}

// propNames lists the properties a resource has, in a fixed order (propname and allprop).
func (rs resource) propNames() []xml.Name {
	d := func(l string) xml.Name { return xml.Name{Space: nsDAV, Local: l} }
	switch rs.k {
	case kCard:
		return []xml.Name{d("resourcetype"), d("getetag"), d("getcontenttype"), d("getcontentlength"),
			d("current-user-principal"), d("current-user-privilege-set")}
	case kBook:
		return []xml.Name{d("resourcetype"), d("displayname"), {Space: nsCS, Local: "getctag"}, d("supported-report-set"),
			{Space: nsCard, Local: "supported-address-data"}, d("current-user-principal"), d("current-user-privilege-set")}
	case kHome:
		return []xml.Name{d("resourcetype"), d("displayname"), d("current-user-principal"), d("current-user-privilege-set")}
	case kPrincipal:
		return []xml.Name{d("resourcetype"), d("displayname"), d("principal-URL"), {Space: nsCard, Local: "addressbook-home-set"},
			d("current-user-principal"), d("current-user-privilege-set")}
	}
	return []xml.Name{d("resourcetype"), d("current-user-principal"), d("current-user-privilege-set")}
}

// prop renders one property of a resource ("", false when the resource does not have it).
func (rs resource) prop(n xml.Name) (string, bool) {
	href := func(h string) string { return "<D:href>" + escapeText(h) + "</D:href>" }
	switch {
	case n.Space == nsDAV && n.Local == "resourcetype":
		switch rs.k {
		case kPrincipal:
			return "<D:resourcetype><D:collection/><D:principal/></D:resourcetype>", true
		case kBook:
			return "<D:resourcetype><D:collection/><C:addressbook/></D:resourcetype>", true
		case kCard:
			return "<D:resourcetype/>", true
		}
		return "<D:resourcetype><D:collection/></D:resourcetype>", true
	case n.Space == nsDAV && n.Local == "displayname":
		switch rs.k {
		case kBook:
			return "<D:displayname>" + escapeText(rs.book.name) + "</D:displayname>", true
		case kHome:
			return "<D:displayname>Address books</D:displayname>", true
		case kPrincipal:
			return "<D:displayname>Mailbox</D:displayname>", true
		}
	case n.Space == nsCS && n.Local == "getctag":
		if rs.k == kBook {
			return "<CS:getctag>" + escapeText(rs.book.ctag) + "</CS:getctag>", true
		}
	case n.Space == nsDAV && n.Local == "getetag":
		if rs.k == kCard {
			return "<D:getetag>" + escapeText(rs.card.ETag) + "</D:getetag>", true
		}
	case n.Space == nsDAV && n.Local == "getcontenttype":
		if rs.k == kCard {
			return "<D:getcontenttype>text/vcard; charset=utf-8</D:getcontenttype>", true
		}
	case n.Space == nsDAV && n.Local == "getcontentlength":
		if rs.k == kCard {
			return "<D:getcontentlength>" + strconv.Itoa(len(rs.card.VCard)) + "</D:getcontentlength>", true
		}
	case n.Space == nsDAV && n.Local == "current-user-principal":
		return "<D:current-user-principal>" + href(pathPrincipal) + "</D:current-user-principal>", true
	case n.Space == nsDAV && n.Local == "principal-URL":
		if rs.k == kPrincipal {
			return "<D:principal-URL>" + href(pathPrincipal) + "</D:principal-URL>", true
		}
	case n.Space == nsCard && n.Local == "addressbook-home-set":
		if rs.k == kPrincipal {
			return "<C:addressbook-home-set>" + href(pathHome) + "</C:addressbook-home-set>", true
		}
	case n.Space == nsDAV && n.Local == "supported-report-set":
		if rs.k == kBook {
			return "<D:supported-report-set>" +
				"<D:supported-report><D:report><C:addressbook-query/></D:report></D:supported-report>" +
				"<D:supported-report><D:report><C:addressbook-multiget/></D:report></D:supported-report>" +
				"</D:supported-report-set>", true
		}
	case n.Space == nsCard && n.Local == "supported-address-data":
		if rs.k == kBook {
			return `<C:supported-address-data><C:address-data-type content-type="text/vcard" version="3.0"/></C:supported-address-data>`, true
		}
	case n.Space == nsDAV && n.Local == "current-user-privilege-set":
		// Read only: nothing the bridge serves can be written.
		return "<D:current-user-privilege-set><D:privilege><D:read/></D:privilege><D:privilege><D:read-current-user-privilege-set/></D:privilege></D:current-user-privilege-set>", true
	}
	return "", false
}

// writeProps writes the <D:response> for rs answering req.
func (s *Server) writeProps(ms *multistatus, req propRequest, rs resource) {
	var found []string
	var missing []xml.Name
	switch {
	case req.propname:
		for _, n := range rs.propNames() {
			switch n.Space {
			case nsDAV:
				found = append(found, "<D:"+n.Local+"/>")
			case nsCard:
				found = append(found, "<C:"+n.Local+"/>")
			case nsCS:
				found = append(found, "<CS:"+n.Local+"/>")
			}
		}
	case req.all:
		for _, n := range rs.propNames() {
			if n.Local == "current-user-principal" || n.Local == "current-user-privilege-set" || n.Local == "supported-address-data" {
				continue // not part of allprop
			}
			if v, ok := rs.prop(n); ok {
				found = append(found, v)
			}
		}
	default:
		seen := map[xml.Name]bool{}
		for _, n := range req.names {
			if seen[n] {
				continue
			}
			seen[n] = true
			if v, ok := rs.prop(n); ok {
				found = append(found, v)
			} else {
				missing = append(missing, n)
			}
		}
	}
	ms.response(rs.href(), found, missing)
}

var errBadRoot = &badRequest{"the request has an unexpected root element"}

type badRequest struct{ msg string }

func (e *badRequest) Error() string { return e.msg }

func asMaxBytes(err error, target **http.MaxBytesError) bool {
	for err != nil {
		if m, ok := err.(*http.MaxBytesError); ok {
			*target = m
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
