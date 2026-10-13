//go:build devfake

package fakeupstream

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"easbridge/internal/testgal"
	"easbridge/internal/upstream"
)

// Contacts folders and the Global Address List of the fake Exchange
// (docs/DESIGN-contacts.md). The contacts folders are ordinary folders of type 9 or
// 14 in the hierarchy; their items are contacts, synced with the same key discipline
// as mail (a key names a snapshot of what the client has been told, an unknown or
// invalidated key is answered with a restart from "0").

// EAS folder types of the Contacts class.
const (
	TypeContacts     = 9
	TypeUserContacts = 14
)

type cSnap struct {
	serial int
	known  map[string]string // item id -> signature
}

type contactFolder struct {
	items         map[string]*upstream.Contact
	keys          map[string]*cSnap
	serial        int
	invalidBefore int
}

func sig(c *upstream.Contact) string { return fmt.Sprintf("%#v", *c) }

func (s *Server) contactFolderLocked(user, folderID string) *contactFolder {
	a := s.acct(user)
	if a.contacts == nil {
		a.contacts = map[string]*contactFolder{}
	}
	cf := a.contacts[folderID]
	if cf == nil {
		panic("fakeupstream: no contacts folder " + folderID)
	}
	return cf
}

// AddContactFolder creates a Contacts folder (type 9 or 14) and returns its ServerId.
func (s *Server) AddContactFolder(user, name string, typ int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.acct(user)
	f := s.addFolderLocked(a, "0", name, typ)
	if a.contacts == nil {
		a.contacts = map[string]*contactFolder{}
	}
	a.contacts[f.id] = &contactFolder{items: map[string]*upstream.Contact{}, keys: map[string]*cSnap{}}
	return f.id
}

// AddContact puts a contact into a Contacts folder and returns its ServerId (the one
// in c if it has one).
func (s *Server) AddContact(user, folderID string, c upstream.Contact) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	cf := s.contactFolderLocked(user, folderID)
	a := s.acct(user)
	if c.ServerID == "" {
		a.nextItem++
		c.ServerID = fmt.Sprintf("%s:%d", folderID, a.nextItem)
	}
	cp := c
	cf.items[c.ServerID] = &cp
	s.notifyLocked()
	return c.ServerID
}

// UpdateContact replaces a contact (as if edited in OWA).
func (s *Server) UpdateContact(user, folderID, id string, c upstream.Contact) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cf := s.contactFolderLocked(user, folderID)
	c.ServerID = id
	cp := c
	cf.items[id] = &cp
	s.notifyLocked()
}

// DeleteContact removes a contact.
func (s *Server) DeleteContact(user, folderID, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.contactFolderLocked(user, folderID).items, id)
	s.notifyLocked()
}

// ResetContactFolder invalidates every Sync key of a Contacts folder: the next Sync
// with an old key is answered with a restart from "0".
func (s *Server) ResetContactFolder(user, folderID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cf := s.contactFolderLocked(user, folderID)
	cf.invalidBefore = cf.serial + 1
	s.notifyLocked()
}

// Contacts lists the contacts of a folder by ServerId.
func (s *Server) Contacts(user, folderID string) []upstream.Contact {
	s.mu.Lock()
	defer s.mu.Unlock()
	cf := s.contactFolderLocked(user, folderID)
	out := make([]upstream.Contact, 0, len(cf.items))
	for _, c := range cf.items {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ServerID < out[j].ServerID })
	return out
}

func (cf *contactFolder) issue(known map[string]string, folderID string) string {
	cf.serial++
	key := fmt.Sprintf("%s-c%d", folderID, cf.serial)
	cp := make(map[string]string, len(known))
	for k, v := range known {
		cp[k] = v
	}
	cf.keys[key] = &cSnap{serial: cf.serial, known: cp}
	return key
}

// SyncContacts implements upstream.Mailbox.
func (m *Mailbox) SyncContacts(ctx context.Context, folderID, key string) (*upstream.ContactDelta, error) {
	if err := m.gate(ctx, "SyncContacts", folderID); err != nil {
		return nil, err
	}
	a, err := m.enter("SyncContacts")
	if err != nil {
		return nil, err
	}
	defer m.srv.mu.Unlock()
	cf := a.contacts[folderID]
	if cf == nil {
		return nil, &upstream.StatusError{Op: "Sync", Code: 8, Perm: true, Reason: "folder not found"}
	}
	if key == "0" || key == "" {
		return &upstream.ContactDelta{NewKey: cf.issue(map[string]string{}, folderID)}, nil
	}
	snap := cf.keys[key]
	if snap == nil || snap.serial < cf.invalidBefore {
		// InvalidSyncKey: restart from "0" like the real adapter does.
		return &upstream.ContactDelta{NewKey: cf.issue(map[string]string{}, folderID), Reset: true}, nil
	}
	var adds, changes, deletes []string
	for id, c := range cf.items {
		old, ok := snap.known[id]
		switch {
		case !ok:
			adds = append(adds, id)
		case old != sig(c):
			changes = append(changes, id)
		}
	}
	for id := range snap.known {
		if _, ok := cf.items[id]; !ok {
			deletes = append(deletes, id)
		}
	}
	sort.Strings(adds)
	sort.Strings(changes)
	sort.Strings(deletes)
	w := m.srv.Window
	if w <= 0 {
		w = 5
	}
	next := make(map[string]string, len(snap.known))
	for k, v := range snap.known {
		next[k] = v
	}
	d := &upstream.ContactDelta{}
	n := 0
	take := func() bool {
		if n >= w {
			d.MoreAvailable = true
			return false
		}
		n++
		return true
	}
	for _, id := range adds {
		if !take() {
			break
		}
		d.Added = append(d.Added, *cf.items[id])
		next[id] = sig(cf.items[id])
	}
	for _, id := range changes {
		if !take() {
			break
		}
		d.Changed = append(d.Changed, *cf.items[id])
		next[id] = sig(cf.items[id])
	}
	for _, id := range deletes {
		if !take() {
			break
		}
		d.Deleted = append(d.Deleted, id)
		delete(next, id)
	}
	d.NewKey = cf.issue(next, folderID)
	return d, nil
}

// ---- the Global Address List ----

// GALPerson is an entry of the fake Global Address List, with the two fields that
// EASBridge must never keep.
type GALPerson struct {
	upstream.DirectoryEntry
	Alias     string
	HomePhone string
}

// AddGALPerson adds an entry to the organization's directory (shared by all users).
func (s *Server) AddGALPerson(p GALPerson) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gal = append(s.gal, p)
}

// RemoveGALPerson removes the entry with this address (the person left).
func (s *Server) RemoveGALPerson(email string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.gal[:0]
	for _, p := range s.gal {
		if !strings.EqualFold(p.EmailAddress, email) {
			out = append(out, p)
		}
	}
	s.gal = out
}

// GALQueries returns every query the GAL was searched with (attempts that failed
// included), in order.
func (s *Server) GALQueries() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.galQueries...)
}

// GALSearchTimes returns when each GAL search arrived (real time).
func (s *Server) GALSearchTimes() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.galTimes...)
}

// GALMaxInFlight is the most searches that were ever running at the same time.
func (s *Server) GALMaxInFlight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.galMaxFly
}

// GALViolations lists the queries that look like an attempt to enumerate the
// directory: empty, a single character, or with a wildcard. A correct client sends none.
func (s *Server) GALViolations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.galBad...)
}

func localPart(email string) string {
	l, _, _ := strings.Cut(email, "@")
	return l
}

func (p GALPerson) matches(q string, addressSearch, noLocalPart bool) bool {
	q = strings.ToLower(q)
	email := strings.ToLower(p.EmailAddress)
	if strings.Contains(q, "@") {
		return addressSearch && strings.HasPrefix(email, q)
	}
	names := []string{p.DisplayName, p.FirstName, p.LastName, p.Alias}
	if !noLocalPart {
		names = append(names, localPart(email))
	}
	for _, f := range names {
		f = strings.ToLower(f)
		if strings.HasPrefix(f, q) {
			return true
		}
		for _, w := range strings.Fields(f) {
			if strings.HasPrefix(w, q) {
				return true
			}
		}
	}
	return false
}

// SearchDirectory implements upstream.Mailbox: a prefix match on the names, the alias
// and the local part (and on the whole address when GALAddressSearch is set), at most
// 100 entries.
func (m *Mailbox) SearchDirectory(ctx context.Context, query string) (*upstream.DirectoryResult, error) {
	s := m.srv
	// Every attempt is recorded, also one that an injected failure refuses.
	s.mu.Lock()
	s.galQueries = append(s.galQueries, query)
	s.galTimes = append(s.galTimes, time.Now())
	q := strings.TrimSpace(query)
	if !s.GALAllowEnumeration && (q == "" || len([]rune(q)) < 2 || strings.ContainsAny(q, "*%?")) {
		s.galBad = append(s.galBad, q)
	}
	s.mu.Unlock()
	if err := m.gate(ctx, "SearchDirectory", ""); err != nil {
		return nil, err
	}
	a, err := m.enter("SearchDirectory")
	if err != nil {
		return nil, err
	}
	_ = a
	if min := s.GALMinQuery; min > 0 && len([]rune(q)) < min {
		s.mu.Unlock()
		return nil, &upstream.StatusError{Op: "Search", Code: 2, Perm: true, Reason: "the query is too short"}
	}
	if s.GALRefuse != nil && s.GALRefuse(q) {
		s.mu.Unlock()
		return nil, &upstream.StatusError{Op: "Search", Code: 8, Perm: true, Reason: "the query is too complex"}
	}
	s.galFly++
	if s.galFly > s.galMaxFly {
		s.galMaxFly = s.galFly
	}
	latency := s.GALLatency
	s.mu.Unlock()
	if latency > 0 {
		select {
		case <-time.After(latency):
		case <-ctx.Done():
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.galFly--
	res := &upstream.DirectoryResult{}
	var hits []upstream.DirectoryEntry
	for _, p := range s.gal {
		if (s.GALANR && p.matchesANR(q, s.GALAddressSearch)) || (!s.GALANR && p.matches(q, s.GALAddressSearch, s.GALNoLocalPartSearch)) {
			hits = append(hits, p.DirectoryEntry)
		}
	}
	// The window is the first 100 matches in address order.
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].EmailAddress < hits[j].EmailAddress })
	res.Total = len(hits)
	if len(hits) > 100 {
		hits = hits[:100]
	}
	res.Entries = hits
	if s.GALOmitTotal {
		res.Total = 0
	}
	return res, nil
}

// matchesANR is the strict ambiguous name resolution: the start of the display name, the first
// name, the last name or the alias (and, for a query with an @, of the address, when address
// search is on).
func (p GALPerson) matchesANR(q string, addressSearch bool) bool {
	q = strings.ToLower(q)
	if strings.Contains(q, "@") {
		return addressSearch && strings.HasPrefix(strings.ToLower(p.EmailAddress), q)
	}
	for _, f := range []string{p.DisplayName, p.FirstName, p.LastName, p.Alias} {
		if strings.HasPrefix(strings.ToLower(f), q) {
			return true
		}
	}
	return false
}

// GALFromTestgal converts generated entries; their aliases are kept (the fake knows them, the
// engine never receives them).
func GALFromTestgal(ps []testgal.Person) []GALPerson {
	out := make([]GALPerson, len(ps))
	for i, p := range ps {
		out[i] = GALPerson{DirectoryEntry: upstream.DirectoryEntry{DisplayName: p.Display, FirstName: p.First, LastName: p.Last, EmailAddress: p.Email,
			Title: p.Title, Office: p.Office, Company: p.Company, Phone: p.Phone, MobilePhone: p.Mobile}, Alias: p.Alias, HomePhone: "+1 555 055 0000"}
	}
	return out
}

// AddGALPeople adds many entries to the directory.
func (s *Server) AddGALPeople(ps []GALPerson) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gal = append(s.gal, ps...)
}
