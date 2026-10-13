// Package carddav serves the contacts of EASBridge as a read-only CardDAV address book
// (RFC 6352) on net/http and encoding/xml, with no dependency beyond the standard
// library. It implements what MailFlow's client sends and nothing that writes: OPTIONS,
// PROPFIND, REPORT (addressbook-query and addressbook-multiget), GET and HEAD. See
// docs/DESIGN-contacts.md section 5.
package carddav

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"easbridge/internal/engine"
	"easbridge/internal/store"
)

// Paths of the resources.
const (
	pathWellKnown = "/.well-known/carddav"
	pathDAV       = "/dav/"
	pathPrincipal = "/dav/principals/me/"
	pathHome      = "/dav/addressbooks/me/"
)

// Limits (section 5.5).
const (
	maxBody       = 64 << 10
	maxConcurrent = 8
	maxHrefs      = 1000
)

// Options configures a Server.
type Options struct {
	// Login authenticates a Basic credential through the same path as IMAP LOGIN and
	// returns the canonical (lower-case) user. Its errors are those of
	// engine.Manager.Login.
	Login func(ctx context.Context, user, pass string) (string, error)
	// Ready, if set, is called after a login and before a request is answered (not for
	// OPTIONS): it may wait a little until the mailbox's folder hierarchy is known, so that
	// the first request after a start does not find a mailbox without address books.
	Ready func(ctx context.Context, user string)
	// DB returns the current state database (it is replaced after a remote wipe).
	DB func() *store.DB
	// ContactsBookName is the display name of the book of the default Contacts folder,
	// DirectoryBookName the one of the directory; the directory book is listed only if
	// DirectoryEnabled.
	ContactsBookName  string
	DirectoryBookName string
	DirectoryEnabled  bool
	Log               *slog.Logger
}

// Server is the CardDAV server.
type Server struct {
	o   Options
	sem chan struct{}
	srv *http.Server
	log *slog.Logger
}

// New builds a Server.
func New(o Options) *Server {
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	s := &Server{o: o, sem: make(chan struct{}, maxConcurrent), log: o.Log}
	s.srv = &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
		TLSNextProto:      map[string]func(*http.Server, *tls.Conn, http.Handler){}, // HTTP/1.1 only
	}
	return s
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// Serve serves on a listener until Close. The listener should speak TLS (ServeTLS does
// that); tests serve plain TCP.
func (s *Server) Serve(ln net.Listener) error { return s.srv.Serve(ln) }

// ServeTLS serves HTTPS on a plain listener with the given TLS configuration (a copy of
// it): the certificate and the version floor are the caller's, the protocol is
// HTTP/1.1 whatever a client offers.
func (s *Server) ServeTLS(ln net.Listener, cfg *tls.Config) error {
	c := cfg.Clone()
	c.NextProtos = []string{"http/1.1"}
	return s.srv.Serve(tls.NewListener(ln, c))
}

// Close stops the server.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.srv.Shutdown(ctx); err != nil {
		return s.srv.Close()
	}
	return nil
}

// Handler returns the handler (tests serve it with httptest).
func (s *Server) Handler() http.Handler { return s }

// resource kinds
type kind int

const (
	kRoot kind = iota
	kDAV
	kPrincipal
	kHome
	kBook
	kCard
)

func (k kind) String() string {
	return [...]string{"root", "dav", "principal", "home", "book", "card"}[k]
}

// allow is the Allow header of a resource.
func (k kind) allow() string {
	switch k {
	case kBook:
		return "OPTIONS, PROPFIND, REPORT"
	case kCard:
		return "OPTIONS, GET, HEAD, PROPFIND"
	}
	return "OPTIONS, PROPFIND"
}

const allAllowed = "OPTIONS, PROPFIND, REPORT, GET, HEAD"

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		w.Header().Set("Retry-After", "5")
		http.Error(w, "too many requests at once", http.StatusServiceUnavailable)
		return
	}
	start := time.Now()
	sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
	res := "-"
	defer func() {
		s.log.Debug("carddav request", "method", r.Method, "status", sw.code, "resource", res, "took", time.Since(start).Round(time.Millisecond).String())
	}()
	w = sw
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)

	// The discovery redirect reveals nothing, so it needs no login.
	if r.URL.Path == pathWellKnown {
		res = "well-known"
		http.Redirect(w, r, pathDAV, http.StatusMovedPermanently)
		return
	}

	user, ok := s.authenticate(w, r)
	if !ok {
		res = "auth"
		return
	}
	if s.o.Ready != nil && r.Method != http.MethodOptions {
		s.o.Ready(r.Context(), user)
	}
	k, book, cardUID, found := s.route(user, r.URL.Path)
	if found {
		res = k.String()
	}
	switch r.Method {
	case http.MethodOptions:
		if !found {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("DAV", "1, 3, addressbook")
		w.Header().Set("Allow", k.allow())
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
	case "PROPFIND":
		if !found {
			http.NotFound(w, r)
			return
		}
		s.propfind(w, r, user, k, book, cardUID)
	case "REPORT":
		if !found {
			http.NotFound(w, r)
			return
		}
		if k != kBook {
			w.Header().Set("Allow", k.allow())
			http.Error(w, "REPORT is only answered on an address book", http.StatusMethodNotAllowed)
			return
		}
		s.report(w, r, user, book)
	case http.MethodGet, http.MethodHead:
		if !found {
			http.NotFound(w, r)
			return
		}
		if k != kCard {
			w.Header().Set("Allow", k.allow())
			http.Error(w, "this is a collection; PROPFIND lists it", http.StatusMethodNotAllowed)
			return
		}
		s.getCard(w, r, user, book, cardUID)
	default:
		// Everything that writes (PUT, DELETE, PROPPATCH, MKCOL, COPY, MOVE, LOCK, POST ...)
		// and everything unknown: the server is read-only.
		w.Header().Set("Allow", allAllowed)
		w.Header().Set("DAV", "1, 3, addressbook")
		http.Error(w, "this address book is read-only", http.StatusMethodNotAllowed)
	}
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

// authenticate checks the Basic credential through the shared login path. On failure it
// has answered and returns false.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (string, bool) {
	challenge := func() {
		w.Header().Set("WWW-Authenticate", `Basic realm="easbridge", charset="UTF-8"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
	}
	user, pass, ok := parseBasic(r.Header.Get("Authorization"))
	if !ok || user == "" || pass == "" {
		challenge()
		return "", false
	}
	u, err := s.o.Login(engine.WithSource(r.Context(), engine.SourceFromString(r.RemoteAddr)), user, pass)
	switch {
	case err == nil:
		return u, true
	case errors.Is(err, engine.ErrBadCredentials):
		challenge()
	default:
		// Refused locally (the lockout budget or a cooldown after a failure that was not a
		// 401), Exchange unreachable, or a remote wipe: the password may well be right, so
		// this is not a 401 (MailFlow reports a 401 as "check the username and password").
		wait := 60 * time.Second
		var te *engine.ThrottledError
		if errors.As(err, &te) {
			if d := time.Until(te.Until); d > 0 {
				wait = d
			}
		}
		if wait > 24*time.Hour {
			wait = 24 * time.Hour
		}
		w.Header().Set("Retry-After", strconv.Itoa(int(wait/time.Second)+1))
		http.Error(w, "the service is not available for this login now", http.StatusServiceUnavailable)
	}
	return "", false
}

// parseBasic decodes an Authorization header of the Basic scheme.
func parseBasic(h string) (user, pass string, ok bool) {
	const prefix = "basic "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(h[len(prefix):]))
	if err != nil {
		return "", "", false
	}
	u, p, found := strings.Cut(string(raw), ":")
	if !found {
		return "", "", false
	}
	return u, p, true
}

// bookInfo is an address book as the server lists it.
type bookInfo struct {
	id        string
	name      string
	ctag      string
	directory bool
	folderID  int64
}

func (s *Server) account(user string) (*store.Account, error) {
	db := s.o.DB()
	if db == nil {
		return nil, errors.New("no database")
	}
	return db.Account(user)
}

// books lists the address books of a user: the Contacts folders (the default one under
// the configured name), then the directory.
func (s *Server) books(user string) ([]bookInfo, error) {
	a, err := s.account(user)
	if err != nil {
		return nil, err
	}
	fs, err := a.ContactFolders()
	if err != nil {
		return nil, err
	}
	var out []bookInfo
	for _, f := range fs {
		name := f.DisplayName
		if f.BookID == store.DefaultBookID && s.o.ContactsBookName != "" {
			name = s.o.ContactsBookName
		}
		if strings.TrimSpace(name) == "" {
			name = "Contacts"
		}
		out = append(out, bookInfo{id: f.BookID, name: name, ctag: f.CTag, folderID: f.ID})
	}
	if s.o.DirectoryEnabled {
		st, err := a.DirectoryState()
		if err != nil {
			return nil, err
		}
		out = append(out, bookInfo{id: store.DirectoryBookID, name: s.o.DirectoryBookName, ctag: st.CTag, directory: true})
	}
	return out, nil
}

func (s *Server) cardsOf(user string, b *bookInfo) ([]store.Card, error) {
	a, err := s.account(user)
	if err != nil {
		return nil, err
	}
	if b.directory {
		return a.DirectoryCards()
	}
	return a.ContactCards(b.folderID)
}

func (s *Server) cardOf(user string, b *bookInfo, uid string) (*store.Card, error) {
	a, err := s.account(user)
	if err != nil {
		return nil, err
	}
	if b.directory {
		return a.DirectoryCard(uid)
	}
	return a.ContactCard(b.folderID, uid)
}

// route maps a path to a resource. For a book or a card it looks the book up; found is
// false for a path that names nothing. The card itself is not looked up here.
func (s *Server) route(user, p string) (k kind, b *bookInfo, uid string, found bool) {
	switch p {
	case "/":
		return kRoot, nil, "", true
	case "/dav", pathDAV:
		return kDAV, nil, "", true
	case strings.TrimSuffix(pathPrincipal, "/"), pathPrincipal:
		return kPrincipal, nil, "", true
	case strings.TrimSuffix(pathHome, "/"), pathHome:
		return kHome, nil, "", true
	}
	rest, ok := strings.CutPrefix(p, pathHome)
	if !ok || rest == "" {
		return 0, nil, "", false
	}
	id, tail, hasSlash := strings.Cut(rest, "/")
	if id == "" || strings.ContainsAny(id, "\x00") {
		return 0, nil, "", false
	}
	books, err := s.books(user)
	if err != nil {
		return 0, nil, "", false
	}
	var bk *bookInfo
	for i := range books {
		if books[i].id == id {
			bk = &books[i]
		}
	}
	if bk == nil {
		return 0, nil, "", false
	}
	if !hasSlash || tail == "" {
		return kBook, bk, "", true
	}
	name, isCard := strings.CutSuffix(tail, ".vcf")
	if !isCard || name == "" || strings.ContainsAny(name, "/") {
		return 0, nil, "", false
	}
	return kCard, bk, name, true
}

func bookHref(id string) string      { return pathHome + id + "/" }
func cardHref(id, uid string) string { return pathHome + id + "/" + uid + ".vcf" }

// getCard serves one card (GET and HEAD).
func (s *Server) getCard(w http.ResponseWriter, r *http.Request, user string, b *bookInfo, uid string) {
	c, err := s.cardOf(user, b, uid)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/vcard; charset=utf-8")
	h.Set("ETag", c.ETag)
	if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, c.ETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Length", strconv.Itoa(len(c.VCard)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write(c.VCard)
	}
}

func etagMatches(header, etag string) bool {
	for _, t := range strings.Split(header, ",") {
		t = strings.TrimSpace(t)
		if t == "*" || subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(t, "W/")), []byte(etag)) == 1 {
			return true
		}
	}
	return false
}

// fail answers an internal error without detail.
func (s *Server) fail(w http.ResponseWriter, err error) {
	s.log.Error("carddav: reading the contacts failed", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
