//go:build devfake

package fakeupstream

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"easbridge/internal/testgal"
	"easbridge/internal/upstream"
)

// ControlHandler is a small HTTP API for driving a Server from outside the
// process (the imapflow compatibility harness runs the bridge as a separate
// binary). It is only compiled with the devfake tag and must listen on loopback.
//
//	POST /inject?user=&folder=&read=0|1&flagged=0|1&date=RFC3339&bcc=   body: raw MIME      -> {"id": "..."}
//	                                           (bcc: the email2:Bcc property the item is listed with)
//	POST /flags?user=&folder=&id=&read=0|1&flagged=0|1
//	POST /delete?user=&folder=&id=
//	POST /reset?user=&folder=                  forget the folder's sync keys
//	POST /password?user=                       body: the new password
//	POST /hold-sent?user=&hold=0|1             delay the Sent Items copy of submissions
//	POST /release-sent?user=
//	POST /wipe?user=                           request a remote wipe
//	POST /fail?op=&n=&kind=unavailable|boom    make the next n calls of op fail
//	POST /hold?user=&folder=&op=Sync|FetchMIME&after=N   let N calls through, then make later ones wait
//	POST /release?user=&folder=&op=            let the waiting calls continue
//	GET  /sent?user=                           -> submissions with recipients, MIME and delivered MIME (base64)
//	GET  /items?user=&folder=                  -> items (id, read, flagged, size, message_id)
//	GET  /folders?user=                        -> {id: name}
//	GET  /calls?op=                            -> {"count": n}
//	POST /contact-folder?user=&name=&type=9|14 -> {"id": "..."}   a Contacts folder
//	POST /contact?user=&folder=&id=            body: JSON upstream.Contact; with id, replaces that contact  -> {"id": "..."}
//	POST /contact-delete?user=&folder=&id=
//	POST /contact-reset?user=&folder=          forget the folder's contact sync keys
//	POST /gal-add                              body: JSON GALPerson (the organization's directory)
//	POST /gal-remove?email=
//	POST /gal-address-search?on=0|1            whether a search by full address finds the entry
//	POST /gal-generate?n=&seed=&domains=a,b&external=   add n generated entries (internal/testgal) to the directory -> {"in_domain": k}
//	POST /gal-anr?on=0|1&min=N                  strict ambiguous name resolution; refuse queries shorter than N characters
//	GET  /gal-queries                          -> {"queries": [...], "violations": [...], "max_in_flight": n}
func (s *Server) ControlHandler() http.Handler {
	mux := http.NewServeMux()
	bad := func(w http.ResponseWriter, msg string) { http.Error(w, msg, http.StatusBadRequest) }
	flag := func(r *http.Request, name string) *bool {
		v := r.URL.Query().Get(name)
		if v == "" {
			return nil
		}
		b := v == "1" || v == "true"
		return &b
	}
	folder := func(w http.ResponseWriter, r *http.Request) (string, bool) {
		user, name := r.URL.Query().Get("user"), r.URL.Query().Get("folder")
		if s.acct(user) == nil {
			bad(w, "unknown user")
			return "", false
		}
		id := s.FolderID(user, name)
		if id == "" {
			bad(w, "unknown folder")
			return "", false
		}
		return id, true
	}
	reply := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}

	mux.HandleFunc("/inject", func(w http.ResponseWriter, r *http.Request) {
		fid, ok := folder(w, r)
		if !ok {
			return
		}
		mime, _ := io.ReadAll(r.Body)
		opts := AddOptions{}
		if f := flag(r, "read"); f != nil {
			opts.Read = *f
		}
		if f := flag(r, "flagged"); f != nil {
			opts.Flagged = *f
		}
		opts.Bcc = r.URL.Query().Get("bcc")
		if d := r.URL.Query().Get("date"); d != "" {
			t, err := time.Parse(time.RFC3339, d)
			if err != nil {
				bad(w, "bad date")
				return
			}
			opts.Date = t
		}
		reply(w, map[string]string{"id": s.AddMessage(r.URL.Query().Get("user"), fid, mime, opts)})
	})
	mux.HandleFunc("/flags", func(w http.ResponseWriter, r *http.Request) {
		fid, ok := folder(w, r)
		if !ok {
			return
		}
		s.SetFlags(r.URL.Query().Get("user"), fid, r.URL.Query().Get("id"), flag(r, "read"), flag(r, "flagged"))
		reply(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/delete", func(w http.ResponseWriter, r *http.Request) {
		fid, ok := folder(w, r)
		if !ok {
			return
		}
		s.DeleteMessage(r.URL.Query().Get("user"), fid, r.URL.Query().Get("id"))
		reply(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/reset", func(w http.ResponseWriter, r *http.Request) {
		fid, ok := folder(w, r)
		if !ok {
			return
		}
		s.ResetFolder(r.URL.Query().Get("user"), fid)
		reply(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/password", func(w http.ResponseWriter, r *http.Request) {
		user := r.URL.Query().Get("user")
		if s.acct(user) == nil {
			bad(w, "unknown user")
			return
		}
		pw, _ := io.ReadAll(r.Body)
		s.SetPassword(user, string(pw))
		reply(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/hold-sent", func(w http.ResponseWriter, r *http.Request) {
		user := r.URL.Query().Get("user")
		if s.acct(user) == nil {
			bad(w, "unknown user")
			return
		}
		f := flag(r, "hold")
		s.HoldSentCopies(user, f != nil && *f)
		reply(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/release-sent", func(w http.ResponseWriter, r *http.Request) {
		user := r.URL.Query().Get("user")
		if s.acct(user) == nil {
			bad(w, "unknown user")
			return
		}
		s.ReleaseSentCopies(user)
		reply(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/wipe", func(w http.ResponseWriter, r *http.Request) {
		user := r.URL.Query().Get("user")
		if s.acct(user) == nil {
			bad(w, "unknown user")
			return
		}
		s.RequestWipe(user)
		reply(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/fail", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		if n == 0 {
			n = 1
		}
		var err error = errors.New("injected failure")
		if r.URL.Query().Get("kind") == "unavailable" {
			err = upstream.ErrUnavailable
		}
		s.FailNext(r.URL.Query().Get("op"), err, n)
		reply(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/hold", func(w http.ResponseWriter, r *http.Request) {
		fid, ok := folder(w, r)
		if !ok {
			return
		}
		op := r.URL.Query().Get("op")
		if op != "Sync" && op != "FetchMIME" {
			bad(w, "op must be Sync or FetchMIME")
			return
		}
		after, _ := strconv.Atoi(r.URL.Query().Get("after"))
		s.HoldAfter(op, r.URL.Query().Get("user"), fid, after)
		reply(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/release", func(w http.ResponseWriter, r *http.Request) {
		fid, ok := folder(w, r)
		if !ok {
			return
		}
		s.Release(r.URL.Query().Get("op"), r.URL.Query().Get("user"), fid)
		reply(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/sent", func(w http.ResponseWriter, r *http.Request) {
		user := r.URL.Query().Get("user")
		if s.acct(user) == nil {
			bad(w, "unknown user")
			return
		}
		type item struct {
			Recipients []string `json:"recipients"`
			Save       bool     `json:"save_in_sent"`
			MIME       string   `json:"mime_b64"`
			Delivered  string   `json:"delivered_b64"`
		}
		out := []item{}
		for _, m := range s.Sent(user) {
			out = append(out, item{m.Recipients, m.SaveInSent, base64.StdEncoding.EncodeToString(m.MIME), base64.StdEncoding.EncodeToString(m.Delivered)})
		}
		reply(w, out)
	})
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		fid, ok := folder(w, r)
		if !ok {
			return
		}
		type item struct {
			ID      string `json:"id"`
			Read    bool   `json:"read"`
			Flagged bool   `json:"flagged"`
			Size    int    `json:"size"`
			MIME    string `json:"mime_b64,omitempty"`
		}
		out := []item{}
		for _, it := range s.Items(r.URL.Query().Get("user"), fid) {
			i := item{ID: it.ID, Read: it.Read, Flagged: it.Flagged, Size: len(it.MIME)}
			if r.URL.Query().Get("mime") == "1" {
				i.MIME = base64.StdEncoding.EncodeToString(it.MIME)
			}
			out = append(out, i)
		}
		reply(w, out)
	})
	mux.HandleFunc("/folders", func(w http.ResponseWriter, r *http.Request) {
		user := r.URL.Query().Get("user")
		if s.acct(user) == nil {
			bad(w, "unknown user")
			return
		}
		reply(w, s.Folders(user))
	})
	mux.HandleFunc("/contact-folder", func(w http.ResponseWriter, r *http.Request) {
		user := r.URL.Query().Get("user")
		if s.acct(user) == nil {
			bad(w, "unknown user")
			return
		}
		typ, _ := strconv.Atoi(r.URL.Query().Get("type"))
		if typ == 0 {
			typ = TypeContacts
		}
		if typ != TypeContacts && typ != TypeUserContacts {
			bad(w, "type must be 9 or 14")
			return
		}
		reply(w, map[string]string{"id": s.AddContactFolder(user, r.URL.Query().Get("name"), typ)})
	})
	mux.HandleFunc("/contact", func(w http.ResponseWriter, r *http.Request) {
		fid, ok := folder(w, r)
		if !ok {
			return
		}
		var c upstream.Contact
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			bad(w, "bad contact: "+err.Error())
			return
		}
		user, id := r.URL.Query().Get("user"), r.URL.Query().Get("id")
		if id != "" {
			s.UpdateContact(user, fid, id, c)
			reply(w, map[string]string{"id": id})
			return
		}
		reply(w, map[string]string{"id": s.AddContact(user, fid, c)})
	})
	mux.HandleFunc("/contact-delete", func(w http.ResponseWriter, r *http.Request) {
		fid, ok := folder(w, r)
		if !ok {
			return
		}
		s.DeleteContact(r.URL.Query().Get("user"), fid, r.URL.Query().Get("id"))
		reply(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/contact-reset", func(w http.ResponseWriter, r *http.Request) {
		fid, ok := folder(w, r)
		if !ok {
			return
		}
		s.ResetContactFolder(r.URL.Query().Get("user"), fid)
		reply(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/gal-add", func(w http.ResponseWriter, r *http.Request) {
		var p GALPerson
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			bad(w, "bad entry: "+err.Error())
			return
		}
		s.AddGALPerson(p)
		reply(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/gal-remove", func(w http.ResponseWriter, r *http.Request) {
		s.RemoveGALPerson(r.URL.Query().Get("email"))
		reply(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/gal-address-search", func(w http.ResponseWriter, r *http.Request) {
		f := flag(r, "on")
		s.mu.Lock()
		s.GALAddressSearch = f != nil && *f
		s.mu.Unlock()
		reply(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/gal-generate", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		n, _ := strconv.Atoi(q.Get("n"))
		seed, _ := strconv.ParseInt(q.Get("seed"), 10, 64)
		domains := strings.Split(q.Get("domains"), ",")
		if n <= 0 || n > 20000 || domains[0] == "" {
			bad(w, "n and domains are required")
			return
		}
		ext := q.Get("external")
		if ext == "" {
			ext = "outside.example"
		}
		people := testgal.Generate(n, seed, domains, ext)
		s.AddGALPeople(GALFromTestgal(people))
		reply(w, map[string]int{"in_domain": len(testgal.InDomain(people, domains...))})
	})
	mux.HandleFunc("/gal-anr", func(w http.ResponseWriter, r *http.Request) {
		on := flag(r, "on")
		min, _ := strconv.Atoi(r.URL.Query().Get("min"))
		s.mu.Lock()
		s.GALANR, s.GALAllowEnumeration = on != nil && *on, on != nil && *on
		s.GALMinQuery = min
		s.mu.Unlock()
		reply(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/gal-queries", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]any{"queries": s.GALQueries(), "violations": s.GALViolations(), "max_in_flight": s.GALMaxInFlight()})
	})
	mux.HandleFunc("/calls", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]int{"count": s.Calls(r.URL.Query().Get("op"))})
	})
	return mux
}
