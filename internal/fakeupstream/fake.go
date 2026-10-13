//go:build devfake

package fakeupstream

import (
	"bytes"
	"context"
	"fmt"
	"net/mail"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"easbridge/internal/mimeutil"
	"easbridge/internal/upstream"
)

// EAS folder types used by the fake hierarchy.
const (
	TypeInbox   = 2
	TypeDrafts  = 3
	TypeDeleted = 4
	TypeSent    = 5
	TypeOutbox  = 6
	TypeCal     = 8
	TypeMail    = 12
)

// Server is the shared in-memory Exchange. Mailboxes created from it see the
// same data.
type Server struct {
	mu    sync.Mutex
	users map[string]*account

	// Window is the number of changes returned per Sync (default 5).
	Window int
	// ApplyReturnsServerChanges makes ApplyChanges return pending server
	// changes in the same response (the case go-activesync patch 7 handles).
	ApplyReturnsServerChanges bool
	// PingDropAfter, when positive, makes any Ping with a longer heartbeat fail
	// with ErrUnavailable after that duration (a proxy cutting long requests).
	PingDropAfter time.Duration
	// MaxPingHeartbeat, when positive, makes longer heartbeats answer status 5.
	MaxPingHeartbeat time.Duration
	// RewriteMessageID makes the Sent Items copy of a submitted message get a
	// different Message-ID (exercises the de-duplication fallback).
	RewriteMessageID bool
	// MaxSend rejects submissions larger than this many bytes (0 = unlimited).
	MaxSend int
	// PingStatus7 makes every Ping answer status 7 (folder hierarchy sync
	// required), whatever the client has synced.
	PingStatus7 bool
	// MaxPingFolders, when positive, makes Ping answer status 6 (too many
	// folders) for longer folder lists.
	MaxPingFolders int
	// RejectChangesStatus, when non-zero, makes ApplyChanges answer every
	// flag change with this per-item status (deletes are unaffected).
	RejectChangesStatus int
	// RejectDeletesStatus, when non-zero, makes ApplyChanges answer every delete
	// with this per-item status and leave the item where it is.
	RejectDeletesStatus int

	// GALAddressSearch makes a GAL search find an entry by its full address (Exchange
	// may or may not; see docs/DESIGN-contacts.md section 4.3). Names, alias and local
	// part always match as prefixes.
	GALAddressSearch bool
	// GALNoLocalPartSearch makes a search for a local part find nothing (the entry
	// is then found by its name only).
	GALNoLocalPartSearch bool
	// GALANR switches the search to strict ambiguous name resolution: a query matches
	// the start of the display name, the first name, the last name or the alias, and
	// nothing else (no word inside a name, no local part). GALMinQuery, when positive, makes
	// a query shorter than that many characters fail with Search status 2. GALOmitTotal
	// leaves the Total out of the answer (it is reported as 0), as a server may.
	GALANR       bool
	GALMinQuery  int
	GALOmitTotal bool
	// FolderSyncZeroResetsKeys makes a FolderSync from key "0" that is not the account's
	// first invalidate every collection Sync key, mail and contacts: the next Sync with
	// an old key is answered with status 3 (a restart from "0"). MS-ASCMD does not say
	// what Exchange does; false (the default) is the model of an Exchange that keeps the
	// collections' keys, true the worst case the bridge must also survive.
	FolderSyncZeroResetsKeys bool
	// GALAllowEnumeration stops the fake from recording short or wildcard queries as
	// violations: the opt-in sweep sends one-character queries on purpose.
	GALAllowEnumeration bool
	// GALRefuse, when set, makes every query for which it returns true fail with Search
	// status 8 (too complex).
	GALRefuse func(query string) bool
	// GALLatency makes every GAL search take this long (outside the server lock).
	GALLatency time.Duration

	pinged map[string]bool // folder ids that were ever named in a Ping

	gal        []GALPerson
	galQueries []string
	galTimes   []time.Time
	galBad     []string
	galFly     int
	galMaxFly  int

	changed  chan struct{} // closed and replaced on every mutation
	gates    map[string]*gate
	failures map[string]*failure
	calls    map[string]int
	auth     int
	now      func() time.Time
}

type failure struct {
	err   error
	times int // <0: forever
}

// gate makes calls of one operation on one folder wait (see HoldAfter).
type gate struct {
	passes  int
	release chan struct{}
}

func gateKey(user, folderID, op string) string {
	return strings.ToLower(user) + "|" + folderID + "|" + op
}

// SetClock replaces the time source the server uses when nothing else says what
// time it is: the received date of a message delivered without one, the time of
// a submission. Tests hand the server their fake clock; otherwise the dates of
// the server and of the engine under test come from different clocks, and test
// outcomes depend on the time of day.
func (s *Server) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// NewServer creates an empty Exchange.
func NewServer() *Server {
	return &Server{
		users:            map[string]*account{},
		Window:           5,
		GALAddressSearch: true,
		changed:          make(chan struct{}),
		gates:            map[string]*gate{},
		failures:         map[string]*failure{},
		calls:            map[string]int{},
		now:              time.Now,
	}
}

type item struct {
	id      string
	mime    []byte
	read    bool
	flagged bool
	date    time.Time
	bcc     string // the email2:Bcc property the item is listed with ("" = none)
}

type itemState struct{ read, flagged bool }

type snapshot struct {
	serial int
	known  map[string]itemState
}

type folder struct {
	id, parent, name string
	typ              int
	items            map[string]*item
	keys             map[string]*snapshot
	serial           int
	invalidBefore    int
	latest           *snapshot
}

type hierSnap struct {
	serial int
	known  map[string]upstream.Folder
}

// SentMessage records one SendMail submission.
type SentMessage struct {
	// MIME is what Exchange received, the Bcc header the bridge wrote included.
	MIME       []byte
	Recipients []string // from To, Cc and Bcc headers, lower case
	SaveInSent bool
	At         time.Time
	// Delivered is the message as the recipients get it: Exchange strips the Bcc
	// header from every copy it delivers, and from the Sent Items copy it saves.
	Delivered []byte
}

type account struct {
	user, password string
	folders        map[string]*folder
	order          []string
	nextFolder     int
	nextItem       int
	hierSerial     int
	hierInvalid    int
	fsKeys         []string // the SyncKey of every FolderSync, in order
	hier           int      // bumped on every change of the folder hierarchy
	hierKeys       map[string]*hierSnap
	wipePending    bool
	wipeAccount    bool // the pending order is the 16.1 account-only one
	wipeAcked      bool
	sent           []SentMessage
	sentIDs        map[string]bool // ClientIds Exchange has seen
	holdCopies     bool
	held           []func()
	policySerial   int
	contacts       map[string]*contactFolder // Contacts folders by ServerId (contacts.go)
}

// AddUser creates a mailbox with the standard folders (Inbox, Drafts, Deleted
// Items, Sent Items, Outbox, Junk Email, Calendar).
func (s *Server) AddUser(user, password string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := &account{user: strings.ToLower(user), password: password,
		folders: map[string]*folder{}, hierKeys: map[string]*hierSnap{}}
	s.users[a.user] = a
	for _, f := range []struct {
		name string
		typ  int
	}{{"Inbox", TypeInbox}, {"Drafts", TypeDrafts}, {"Deleted Items", TypeDeleted}, {"Sent Items", TypeSent},
		{"Outbox", TypeOutbox}, {"Junk Email", TypeMail}, {"Calendar", TypeCal}} {
		s.addFolderLocked(a, "0", f.name, f.typ)
	}
}

func (s *Server) addFolderLocked(a *account, parent, name string, typ int) *folder {
	a.nextFolder++
	f := &folder{id: fmt.Sprintf("f%d", a.nextFolder), parent: parent, name: name, typ: typ,
		items: map[string]*item{}, keys: map[string]*snapshot{}}
	a.folders[f.id] = f
	a.order = append(a.order, f.id)
	a.hier++
	s.notifyLocked()
	return f
}

func (s *Server) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *Server) acct(user string) *account { return s.users[strings.ToLower(user)] }

// Test controls ------------------------------------------------------------

// SetPassword changes the account password; mailboxes opened with the old one
// get ErrAuthFailed on their next call.
func (s *Server) SetPassword(user, password string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acct(user).password = password
}

// AuthAttempts counts Open calls that reached the server.
func (s *Server) AuthAttempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.auth
}

// Calls returns how many times op (for example "Sync" or "Ping") was called.
func (s *Server) Calls(op string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[op]
}

// FailNext makes the next n calls of op return err (n < 0: forever).
func (s *Server) FailNext(op string, err error, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[op] = &failure{err: err, times: n}
}

// HoldAfter lets the next passes calls of op ("Sync" or "FetchMIME" for one
// folder, "FolderSync" with an empty folderID, "SendMail" with an empty folderID: the
// call waits after the message was taken) through and makes every later one wait, outside the server lock, until
// Release (or until the caller's context ends). It freezes a folder's first sync
// half way, deterministically: bootstrap, then some batches, then silence.
func (s *Server) HoldAfter(op, user, folderID string, passes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gates[gateKey(user, folderID, op)] = &gate{passes: passes, release: make(chan struct{})}
}

// Release lets the calls held by HoldAfter continue and removes the gate.
func (s *Server) Release(op, user, folderID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := gateKey(user, folderID, op)
	if g := s.gates[k]; g != nil {
		close(g.release)
		delete(s.gates, k)
	}
}

// ReleaseAll opens every gate (cleanup).
func (s *Server) ReleaseAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, g := range s.gates {
		close(g.release)
		delete(s.gates, k)
	}
}

// gate waits while a HoldAfter gate for this operation and folder is shut.
func (m *Mailbox) gate(ctx context.Context, op, folderID string) error {
	s := m.srv
	s.mu.Lock()
	g := s.gates[gateKey(m.user, folderID, op)]
	if g == nil {
		s.mu.Unlock()
		return nil
	}
	if g.passes > 0 {
		g.passes--
		s.mu.Unlock()
		return nil
	}
	ch := g.release
	s.calls[op+" held"]++
	s.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ClearFailures removes all injected failures.
func (s *Server) ClearFailures() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = map[string]*failure{}
}

// RequestWipe makes every further call of the account fail with ErrRemoteWipe
// until the wipe is acknowledged.
func (s *Server) RequestWipe(user string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.acct(user)
	a.wipePending, a.wipeAccount = true, false
	s.notifyLocked()
}

// RequestAccountOnlyWipe is RequestWipe for the EAS 16.1 order that asks for
// the account's data only: calls fail with ErrAccountOnlyWipe, and only the
// matching acknowledgement ends it (a RemoteWipe acknowledgement is refused, as
// Exchange would).
func (s *Server) RequestAccountOnlyWipe(user string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.acct(user)
	a.wipePending, a.wipeAccount = true, true
	s.notifyLocked()
}

// WipeAcknowledged reports whether the device confirmed the wipe.
func (s *Server) WipeAcknowledged(user string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acct(user).wipeAcked
}

// FolderID returns the ServerId of the folder with this display name.
func (s *Server) FolderID(user, name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.acct(user).order {
		if f := s.acct(user).folders[id]; f != nil && f.name == name {
			return id
		}
	}
	return ""
}

// AddFolder creates a folder and returns its ServerId.
func (s *Server) AddFolder(user, parent, name string, typ int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if parent == "" {
		parent = "0"
	}
	return s.addFolderLocked(s.acct(user), parent, name, typ).id
}

// RemoveFolder deletes a folder and everything in it.
func (s *Server) RemoveFolder(user, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.acct(user)
	delete(a.folders, id)
	for i, o := range a.order {
		if o == id {
			a.order = append(a.order[:i], a.order[i+1:]...)
			break
		}
	}
	a.hier++
	s.notifyLocked()
}

// RenameFolder changes a folder's display name.
func (s *Server) RenameFolder(user, id, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f := s.acct(user).folders[id]; f != nil {
		f.name = name
		s.acct(user).hier++
		s.notifyLocked()
	}
}

// AddOptions tune AddMessage.
type AddOptions struct {
	Read    bool
	Flagged bool
	Date    time.Time // zero: now
	// Bcc is the email2:Bcc property the item is listed with (what Exchange would
	// say for a message sent from a phone or OWA, if it gives the Bcc recipients
	// of a sent message out at all). "" lists none.
	Bcc string
}

// AddMessage delivers a message into a folder and returns its ServerId.
func (s *Server) AddMessage(user, folderID string, mime []byte, o AddOptions) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addMessageLocked(s.acct(user), folderID, mime, o)
}

func (s *Server) addMessageLocked(a *account, folderID string, mime []byte, o AddOptions) string {
	f := a.folders[folderID]
	if f == nil {
		panic("fakeupstream: no such folder " + folderID)
	}
	a.nextItem++
	id := fmt.Sprintf("%s:%d", folderID, a.nextItem)
	d := o.Date
	if d.IsZero() {
		d = s.now()
	}
	f.items[id] = &item{id: id, mime: append([]byte(nil), mime...), read: o.Read, flagged: o.Flagged, date: d.UTC().Truncate(time.Second), bcc: o.Bcc}
	s.notifyLocked()
	return id
}

// SetFlags changes the server-side flags of an item (nil = unchanged).
func (s *Server) SetFlags(user, folderID, id string, read, flagged *bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if it := s.acct(user).folders[folderID].items[id]; it != nil {
		if read != nil {
			it.read = *read
		}
		if flagged != nil {
			it.flagged = *flagged
		}
		s.notifyLocked()
	}
}

// DeleteMessage removes an item (as if deleted in OWA).
func (s *Server) DeleteMessage(user, folderID, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.acct(user).folders[folderID].items, id)
	s.notifyLocked()
}

// ResetFolder invalidates every Sync key of a folder (the server forgot the
// sync state: the next Sync answers InvalidSyncKey).
func (s *Server) ResetFolder(user, folderID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.acct(user).folders[folderID]
	f.invalidBefore = f.serial + 1
	f.latest = nil
	s.notifyLocked()
}

// FolderSyncKeys lists the SyncKey of every FolderSync the account has sent, in order.
func (s *Server) FolderSyncKeys(user string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.acct(user).fsKeys...)
}

// ResetHierarchy invalidates every FolderSync key.
func (s *Server) ResetHierarchy(user string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.acct(user)
	a.hierInvalid = a.hierSerial + 1
	a.hier++
	s.notifyLocked()
}

// HoldSentCopies delays saving submitted mail into Sent Items until
// ReleaseSentCopies is called.
func (s *Server) HoldSentCopies(user string, hold bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acct(user).holdCopies = hold
}

// ReleaseSentCopies saves all held Sent Items copies.
func (s *Server) ReleaseSentCopies(user string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.acct(user)
	for _, f := range a.held {
		f()
	}
	a.held = nil
}

// Sent returns what the account submitted with SendMail.
func (s *Server) Sent(user string) []SentMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SentMessage(nil), s.acct(user).sent...)
}

// ItemInfo is a read-only view of an item.
type ItemInfo struct {
	ID      string
	MIME    []byte
	Read    bool
	Flagged bool
	Date    time.Time
}

// Items lists a folder's items, oldest first.
func (s *Server) Items(user, folderID string) []ItemInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.acct(user).folders[folderID]
	var out []ItemInfo
	for _, it := range f.items {
		out = append(out, ItemInfo{it.id, append([]byte(nil), it.mime...), it.read, it.flagged, it.date})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Date.Equal(out[j].Date) {
			return out[i].Date.Before(out[j].Date)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// PingedFolders returns the ids of the folders that were ever part of a Ping.
func (s *Server) PingedFolders() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]bool{}
	for k := range s.pinged {
		out[k] = true
	}
	return out
}

// Folders lists folder names by id (for harness diagnostics).
func (s *Server) Folders(user string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for id, f := range s.acct(user).folders {
		out[id] = f.name
	}
	return out
}

// Mailbox implementation ---------------------------------------------------

// Factory returns an upstream.Factory whose mailboxes talk to this server.
// inlineMax is the MIME size above which Sync returns truncated bodies.
func (s *Server) Factory(inlineMax int) upstream.Factory {
	return upstream.FactoryFunc(func(cfg upstream.Config) (upstream.Mailbox, error) {
		return &Mailbox{srv: s, user: strings.ToLower(cfg.User), password: cfg.Password, inlineMax: inlineMax}, nil
	})
}

// Mailbox is a client session on a Server.
type Mailbox struct {
	srv       *Server
	user      string
	password  string
	inlineMax int
	opened    bool
	seenHier  int // hierarchy version at this session's last FolderSync
}

var _ upstream.Mailbox = (*Mailbox)(nil)

// enter takes the server lock and performs the checks every call shares.
// It returns the account with the lock held, or an error without the lock.
func (m *Mailbox) enter(op string) (*account, error) {
	s := m.srv
	s.mu.Lock()
	s.calls[op]++
	if f := s.failures[op]; f != nil && f.times != 0 {
		if f.times > 0 {
			f.times--
		}
		err := f.err
		s.mu.Unlock()
		return nil, err
	}
	a := s.acct(m.user)
	if a == nil || a.password != m.password {
		s.mu.Unlock()
		return nil, fmt.Errorf("%s: %w", op, upstream.ErrAuthFailed)
	}
	if a.wipePending && !a.wipeAcked && op != "AcknowledgeWipe" {
		s.mu.Unlock()
		if a.wipeAccount {
			return nil, fmt.Errorf("%s: %w", op, upstream.ErrAccountOnlyWipe)
		}
		return nil, fmt.Errorf("%s: %w", op, upstream.ErrRemoteWipe)
	}
	return a, nil
}

func (m *Mailbox) Open(ctx context.Context) error {
	m.srv.mu.Lock()
	m.srv.auth++
	m.srv.mu.Unlock()
	a, err := m.enter("Open")
	if err != nil {
		return err
	}
	defer m.srv.mu.Unlock()
	_ = a
	m.opened = true
	return nil
}

func (m *Mailbox) PolicyKey() string { return "fake-policy" }
func (m *Mailbox) Close()            {}

func (m *Mailbox) AcknowledgeWipe(ctx context.Context, accountOnly bool) error {
	a, err := m.enter("AcknowledgeWipe")
	if err != nil {
		return err
	}
	defer m.srv.mu.Unlock()
	if a.wipePending && accountOnly != a.wipeAccount {
		// The other element does not answer this order: Exchange would keep
		// asking (HTTP 449 and the same directive again).
		return &upstream.StatusError{Op: "Provision", Code: 2, Perm: true, Reason: "wrong kind of wipe acknowledgement"}
	}
	a.wipeAcked = true
	return nil
}

func (m *Mailbox) FolderSync(ctx context.Context, key string) (*upstream.FolderDelta, error) {
	if err := m.gate(ctx, "FolderSync", ""); err != nil {
		return nil, err
	}
	a, err := m.enter("FolderSync")
	if err != nil {
		return nil, err
	}
	defer m.srv.mu.Unlock()
	m.seenHier = a.hier
	if key == "0" && len(a.fsKeys) > 0 && m.srv.FolderSyncZeroResetsKeys {
		for _, f := range a.folders {
			f.invalidBefore = f.serial + 1
			f.latest = nil
		}
		for _, cf := range a.contacts {
			cf.invalidBefore = cf.serial + 1
		}
	}
	a.fsKeys = append(a.fsKeys, key)

	cur := map[string]upstream.Folder{}
	for _, id := range a.order {
		f := a.folders[id]
		cur[id] = upstream.Folder{ServerID: id, ParentID: f.parent, DisplayName: f.name, Type: f.typ}
	}
	var known map[string]upstream.Folder
	full := false
	if snap := a.hierKeys[key]; key != "0" && snap != nil && snap.serial >= a.hierInvalid {
		known = snap.known
	} else {
		full = true // "0", or a key the server no longer knows
	}
	a.hierSerial++
	newKey := fmt.Sprintf("h%d", a.hierSerial)
	a.hierKeys[newKey] = &hierSnap{serial: a.hierSerial, known: cur}
	d := &upstream.FolderDelta{NewKey: newKey, Full: full}
	for _, id := range a.order {
		f := cur[id]
		old, had := known[id]
		switch {
		case !had:
			d.Added = append(d.Added, f)
		case old != f:
			d.Updated = append(d.Updated, f)
		}
	}
	for id := range known {
		if _, ok := cur[id]; !ok {
			d.Deleted = append(d.Deleted, id)
		}
	}
	sort.Strings(d.Deleted)
	return d, nil
}

func (m *Mailbox) folderOf(a *account, op, id string) (*folder, error) {
	f := a.folders[id]
	if f == nil {
		return nil, &upstream.StatusError{Op: op, Code: 8, Perm: true, Reason: "folder not found"}
	}
	return f, nil
}

type pending struct {
	adds, changes []string
	deletes       []string
}

func (f *folder) diff(known map[string]itemState) pending {
	var p pending
	for id, it := range f.items {
		k, ok := known[id]
		switch {
		case !ok:
			p.adds = append(p.adds, id)
		case k != itemState{it.read, it.flagged}:
			p.changes = append(p.changes, id)
		}
	}
	for id := range known {
		if _, ok := f.items[id]; !ok {
			p.deletes = append(p.deletes, id)
		}
	}
	// Newest first, like Exchange's initial backfill.
	sort.Slice(p.adds, func(i, j int) bool {
		a, b := f.items[p.adds[i]], f.items[p.adds[j]]
		if !a.date.Equal(b.date) {
			return a.date.After(b.date)
		}
		return a.id > b.id
	})
	sort.Strings(p.changes)
	sort.Strings(p.deletes)
	return p
}

func (f *folder) issue(known map[string]itemState) string {
	f.serial++
	key := fmt.Sprintf("%s-k%d", f.id, f.serial)
	cp := make(map[string]itemState, len(known))
	for k, v := range known {
		cp[k] = v
	}
	snap := &snapshot{serial: f.serial, known: cp}
	f.keys[key] = snap
	f.latest = snap
	return key
}

func (m *Mailbox) toItem(it *item, withBody bool) upstream.Item {
	out := upstream.Item{ServerID: it.id, Read: it.read, ReadSet: true, Flagged: it.flagged, FlagSet: true,
		DateReceived: it.date, EstimatedSize: len(it.mime), Bcc: it.bcc}
	if hdr, _ := mimeutil.SplitHeader(it.mime); len(hdr) > 0 {
		if msg, err := mail.ReadMessage(bytes.NewReader(append(append([]byte(nil), hdr...), '\r', '\n'))); err == nil {
			out.Subject, out.From, out.To = msg.Header.Get("Subject"), msg.Header.Get("From"), msg.Header.Get("To")
		}
	}
	if withBody {
		if m.inlineMax > 0 && len(it.mime) > m.inlineMax {
			out.MIME = append([]byte(nil), it.mime[:m.inlineMax]...)
		} else {
			out.MIME = append([]byte(nil), it.mime...)
			out.MIMEComplete = true
		}
	}
	return out
}

// window builds a delta from the pending changes, at most Window entries, and
// returns the next known-state.
func (m *Mailbox) window(f *folder, known map[string]itemState, p pending, headersOnly bool) (*upstream.SyncDelta, map[string]itemState) {
	w := m.srv.Window
	if w <= 0 {
		w = 5
	}
	next := make(map[string]itemState, len(known))
	for k, v := range known {
		next[k] = v
	}
	d := &upstream.SyncDelta{}
	n := 0
	take := func() bool {
		if n >= w {
			d.MoreAvailable = true
			return false
		}
		n++
		return true
	}
	for _, id := range p.adds {
		if !take() {
			break
		}
		it := f.items[id]
		d.Added = append(d.Added, m.toItem(it, !headersOnly))
		next[id] = itemState{it.read, it.flagged}
	}
	for _, id := range p.changes {
		if !take() {
			break
		}
		it := f.items[id]
		d.Changed = append(d.Changed, m.toItem(it, false))
		next[id] = itemState{it.read, it.flagged}
	}
	for _, id := range p.deletes {
		if !take() {
			break
		}
		d.Deleted = append(d.Deleted, id)
		delete(next, id)
	}
	return d, next
}

func (m *Mailbox) Sync(ctx context.Context, folderID, key string, o upstream.SyncOptions) (*upstream.SyncDelta, error) {
	if err := m.gate(ctx, "Sync", folderID); err != nil {
		return nil, err
	}
	a, err := m.enter("Sync")
	if err != nil {
		return nil, err
	}
	defer m.srv.mu.Unlock()
	f, err := m.folderOf(a, "Sync", folderID)
	if err != nil {
		return nil, err
	}
	if key == "0" {
		return &upstream.SyncDelta{NewKey: f.issue(map[string]itemState{})}, nil
	}
	snap := f.keys[key]
	if snap == nil || snap.serial < f.invalidBefore {
		// InvalidSyncKey: restart from "0" like the real adapter does.
		return &upstream.SyncDelta{NewKey: f.issue(map[string]itemState{}), Reset: true}, nil
	}
	d, next := m.window(f, snap.known, f.diff(snap.known), o.HeadersOnly)
	d.NewKey = f.issue(next)
	return d, nil
}

func (m *Mailbox) FetchMIME(ctx context.Context, folderID, serverID string) ([]byte, error) {
	if err := m.gate(ctx, "FetchMIME", folderID); err != nil {
		return nil, err
	}
	a, err := m.enter("FetchMIME")
	if err != nil {
		return nil, err
	}
	defer m.srv.mu.Unlock()
	f, err := m.folderOf(a, "Fetch", folderID)
	if err != nil {
		return nil, err
	}
	it := f.items[serverID]
	if it == nil {
		return nil, fmt.Errorf("fetch %s: %w", serverID, upstream.ErrNotFound)
	}
	return append([]byte(nil), it.mime...), nil
}

func (m *Mailbox) ApplyChanges(ctx context.Context, folderID, key string, changes []upstream.Change, hard bool) (*upstream.ApplyResult, error) {
	a, err := m.enter("ApplyChanges")
	if err != nil {
		return nil, err
	}
	defer m.srv.mu.Unlock()
	f, err := m.folderOf(a, "Sync", folderID)
	if err != nil {
		return nil, err
	}
	snap := f.keys[key]
	if key == "0" || snap == nil || snap.serial < f.invalidBefore {
		return nil, &upstream.StatusError{Op: "Sync", Code: 3, Reason: "InvalidSyncKey"}
	}
	known := make(map[string]itemState, len(snap.known))
	for k, v := range snap.known {
		known[k] = v
	}
	res := &upstream.ApplyResult{}
	for _, c := range changes {
		it := f.items[c.ServerID]
		if it == nil {
			res.Results = append(res.Results, upstream.ChangeResult{ServerID: c.ServerID, Status: 8, Gone: true})
			continue
		}
		if !c.Delete && m.srv.RejectChangesStatus != 0 {
			res.Results = append(res.Results, upstream.ChangeResult{ServerID: c.ServerID, Status: m.srv.RejectChangesStatus})
			continue
		}
		if c.Delete && m.srv.RejectDeletesStatus != 0 {
			res.Results = append(res.Results, upstream.ChangeResult{ServerID: c.ServerID, Status: m.srv.RejectDeletesStatus})
			continue
		}
		if c.Delete {
			if hard || f.typ == TypeDeleted {
				delete(f.items, c.ServerID)
			} else {
				// DeletesAsMoves: the item lands in Deleted Items under a new id.
				for _, df := range a.folders {
					if df.typ == TypeDeleted {
						m.srv.addMessageLocked(a, df.id, it.mime, AddOptions{Read: it.read, Flagged: it.flagged, Date: it.date})
						break
					}
				}
				delete(f.items, c.ServerID)
			}
			delete(known, c.ServerID)
		} else {
			if c.Read != nil {
				it.read = *c.Read
			}
			if c.Flagged != nil {
				it.flagged = *c.Flagged
			}
			if _, ok := known[c.ServerID]; ok {
				known[c.ServerID] = itemState{it.read, it.flagged}
			}
		}
		res.Results = append(res.Results, upstream.ChangeResult{ServerID: c.ServerID, Status: 1, OK: true})
	}
	if m.srv.ApplyReturnsServerChanges {
		d, next := m.window(f, known, f.diff(known), false)
		known = next
		if len(d.Added)+len(d.Changed)+len(d.Deleted) > 0 {
			res.ServerChanges = d
		}
	}
	res.NewKey = f.issue(known)
	if res.ServerChanges != nil {
		res.ServerChanges.NewKey = res.NewKey
	}
	m.srv.notifyLocked()
	return res, nil
}

func (m *Mailbox) MoveItems(ctx context.Context, src, dst string, ids []string) ([]upstream.MoveResult, error) {
	a, err := m.enter("MoveItems")
	if err != nil {
		return nil, err
	}
	defer m.srv.mu.Unlock()
	sf, err := m.folderOf(a, "MoveItems", src)
	if err != nil {
		return nil, err
	}
	if _, err := m.folderOf(a, "MoveItems", dst); err != nil {
		return nil, err
	}
	var out []upstream.MoveResult
	for _, id := range ids {
		it := sf.items[id]
		if it == nil {
			out = append(out, upstream.MoveResult{SrcServerID: id, Status: 1})
			continue
		}
		newID := m.srv.addMessageLocked(a, dst, it.mime, AddOptions{Read: it.read, Flagged: it.flagged, Date: it.date})
		delete(sf.items, id)
		out = append(out, upstream.MoveResult{SrcServerID: id, DstServerID: newID, OK: true, Status: 3})
	}
	m.srv.notifyLocked()
	return out, nil
}

func (m *Mailbox) hierKeyOK(a *account, key string) error {
	snap := a.hierKeys[key]
	if key == "0" || snap == nil || snap.serial < a.hierInvalid {
		return &upstream.StatusError{Op: "FolderSync", Code: 9, Reason: "invalid hierarchy key"}
	}
	return nil
}

func (m *Mailbox) newHierKey(a *account) string {
	cur := map[string]upstream.Folder{}
	for _, id := range a.order {
		f := a.folders[id]
		cur[id] = upstream.Folder{ServerID: id, ParentID: f.parent, DisplayName: f.name, Type: f.typ}
	}
	a.hierSerial++
	k := fmt.Sprintf("h%d", a.hierSerial)
	a.hierKeys[k] = &hierSnap{serial: a.hierSerial, known: cur}
	return k
}

func (m *Mailbox) CreateFolder(ctx context.Context, key, parent, name string) (*upstream.FolderResult, error) {
	a, err := m.enter("CreateFolder")
	if err != nil {
		return nil, err
	}
	defer m.srv.mu.Unlock()
	if err := m.hierKeyOK(a, key); err != nil {
		return nil, err
	}
	for _, f := range a.folders {
		if f.parent == parent && strings.EqualFold(f.name, name) {
			return nil, &upstream.StatusError{Op: "FolderCreate", Code: 2, Perm: true, Reason: "folder exists"}
		}
	}
	f := m.srv.addFolderLocked(a, parent, name, TypeMail)
	m.seenHier = a.hier
	return &upstream.FolderResult{ServerID: f.id, NewKey: m.newHierKey(a)}, nil
}

func (m *Mailbox) RenameFolder(ctx context.Context, key, id, parent, name string) (*upstream.FolderResult, error) {
	a, err := m.enter("RenameFolder")
	if err != nil {
		return nil, err
	}
	defer m.srv.mu.Unlock()
	if err := m.hierKeyOK(a, key); err != nil {
		return nil, err
	}
	f := a.folders[id]
	if f == nil {
		return nil, &upstream.StatusError{Op: "FolderUpdate", Code: 4, Perm: true, Reason: "no such folder"}
	}
	f.parent, f.name = parent, name
	a.hier++
	m.seenHier = a.hier
	m.srv.notifyLocked()
	return &upstream.FolderResult{ServerID: id, NewKey: m.newHierKey(a)}, nil
}

func (m *Mailbox) DeleteFolder(ctx context.Context, key, id string) (*upstream.FolderResult, error) {
	a, err := m.enter("DeleteFolder")
	if err != nil {
		return nil, err
	}
	defer m.srv.mu.Unlock()
	if err := m.hierKeyOK(a, key); err != nil {
		return nil, err
	}
	if a.folders[id] == nil {
		return nil, &upstream.StatusError{Op: "FolderDelete", Code: 4, Perm: true, Reason: "no such folder"}
	}
	delete(a.folders, id)
	for i, o := range a.order {
		if o == id {
			a.order = append(a.order[:i], a.order[i+1:]...)
			break
		}
	}
	a.hier++
	m.seenHier = a.hier
	m.srv.notifyLocked()
	return &upstream.FolderResult{ServerID: id, NewKey: m.newHierKey(a)}, nil
}

var bccStrip = regexp.MustCompile(`(?i)^message-id:`)

// SendMail submits a message. HoldAfter("SendMail", user, "", 0) makes the call wait
// AFTER Exchange has taken the message and saved its Sent Items copy, outside the
// server lock: what a slow answer through a gateway looks like, with the copy
// already there to be synced while the submission is still in flight.
func (m *Mailbox) SendMail(ctx context.Context, mimeBytes []byte, save bool) error {
	if err := m.sendMail(ctx, mimeBytes, save); err != nil {
		return err
	}
	return m.gate(ctx, "SendMail", "")
}

func (m *Mailbox) sendMail(ctx context.Context, mimeBytes []byte, save bool) error {
	a, err := m.enter("SendMail")
	if err != nil {
		return err
	}
	defer m.srv.mu.Unlock()
	if m.srv.MaxSend > 0 && len(mimeBytes) > m.srv.MaxSend {
		return upstream.ErrTooLarge
	}
	hdr, _ := mimeutil.SplitHeader(mimeBytes)
	msg, perr := mail.ReadMessage(bytes.NewReader(append(append([]byte(nil), hdr...), '\r', '\n')))
	var rcpt []string
	if perr == nil {
		for _, h := range []string{"To", "Cc", "Bcc"} {
			if list, err := mail.ParseAddressList(msg.Header.Get(h)); err == nil {
				for _, ad := range list {
					rcpt = append(rcpt, strings.ToLower(ad.Address))
				}
			}
		}
	}
	if len(rcpt) == 0 {
		return &upstream.StatusError{Op: "SendMail", Code: 119, Perm: true, Reason: "MessageHasNoRecipient"}
	}
	// Exchange remembers the ClientId of recent messages and does not send one
	// twice (status 118, which the EAS adapter turns into success).
	id := upstream.SubmissionID(mimeBytes)
	if a.sentIDs[id] {
		m.srv.calls["SendMail duplicate"]++
		return nil
	}
	if a.sentIDs == nil {
		a.sentIDs = map[string]bool{}
	}
	a.sentIDs[id] = true
	// Exchange takes the recipients from the headers, Bcc included, and strips the
	// Bcc header from what it delivers and from the Sent Items copy it saves (real
	// Exchange 2019: the To recipient's copy has no Bcc, and neither has the Sent
	// copy that comes back through EAS).
	delivered := stripBcc(mimeBytes)
	a.sent = append(a.sent, SentMessage{MIME: append([]byte(nil), mimeBytes...), Recipients: rcpt, SaveInSent: save, At: m.srv.now(),
		Delivered: append([]byte(nil), delivered...)})
	if save {
		copyMIME := append([]byte(nil), delivered...)
		if m.srv.RewriteMessageID {
			copyMIME = rewriteMessageID(copyMIME)
		}
		deliver := func() {
			for _, f := range a.folders {
				if f.typ == TypeSent {
					m.srv.addMessageLocked(a, f.id, copyMIME, AddOptions{Read: true, Date: m.srv.now()})
					return
				}
			}
		}
		if a.holdCopies {
			a.held = append(a.held, deliver)
		} else {
			deliver()
		}
	}
	return nil
}

// stripBcc removes the Bcc header field (folded lines included) from the header
// block of a message.
func stripBcc(raw []byte) []byte {
	hdr, body := mimeutil.SplitHeader(raw)
	var out bytes.Buffer
	skipping := false
	for _, line := range bytes.SplitAfter(hdr, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		if skipping && (line[0] == ' ' || line[0] == '\t') {
			continue // a folded continuation of the field being dropped
		}
		skipping = false
		if line[0] != ' ' && line[0] != '\t' {
			if i := bytes.IndexByte(line, ':'); i > 0 && strings.EqualFold(strings.TrimSpace(string(line[:i])), "bcc") {
				skipping = true
				continue
			}
		}
		out.Write(line)
	}
	out.Write(body)
	return out.Bytes()
}

func rewriteMessageID(raw []byte) []byte {
	hdr, body := mimeutil.SplitHeader(raw)
	var out bytes.Buffer
	for _, line := range bytes.SplitAfter(hdr, []byte("\n")) {
		if bccStrip.Match(line) {
			out.WriteString("Message-ID: <rewritten-by-exchange@fake.test>\r\n")
			continue
		}
		out.Write(line)
	}
	out.Write(body)
	return out.Bytes()
}

func (m *Mailbox) Ping(ctx context.Context, heartbeat time.Duration, folderIDs []string) (*upstream.PingResult, error) {
	s := m.srv
	deadline := time.NewTimer(heartbeat)
	defer deadline.Stop()
	var drop <-chan time.Time
	if s.PingDropAfter > 0 && heartbeat > s.PingDropAfter {
		t := time.NewTimer(s.PingDropAfter)
		defer t.Stop()
		drop = t.C
	}
	for {
		a, err := m.enter("Ping")
		if err != nil {
			return nil, err
		}
		if s.PingStatus7 {
			s.mu.Unlock()
			return &upstream.PingResult{Status: 7}, nil
		}
		if s.MaxPingHeartbeat > 0 && heartbeat > s.MaxPingHeartbeat {
			s.mu.Unlock()
			return &upstream.PingResult{Status: 5, Heartbeat: s.MaxPingHeartbeat}, nil
		}
		if a.hier != m.seenHier {
			s.mu.Unlock()
			return &upstream.PingResult{Status: 7}, nil // folder hierarchy sync required
		}
		if s.MaxPingFolders > 0 && len(folderIDs) > s.MaxPingFolders {
			s.mu.Unlock()
			return &upstream.PingResult{Status: 6}, nil
		}
		var changed []string
		if s.pinged == nil {
			s.pinged = map[string]bool{}
		}
		for _, id := range folderIDs {
			s.pinged[id] = true
		}
		for _, id := range folderIDs {
			f := a.folders[id]
			if f == nil {
				s.mu.Unlock()
				return &upstream.PingResult{Status: 7}, nil
			}
			base := map[string]itemState{}
			if f.latest != nil {
				base = f.latest.known
			}
			p := f.diff(base)
			if f.latest == nil && len(f.items) == 0 {
				continue // never synced and empty: nothing to report
			}
			if len(p.adds)+len(p.changes)+len(p.deletes) > 0 || f.latest == nil {
				changed = append(changed, id)
			}
		}
		ch := s.changed
		s.mu.Unlock()
		if len(changed) > 0 {
			sort.Strings(changed)
			return &upstream.PingResult{Status: 2, Changed: changed}, nil
		}
		select {
		case <-ch:
		case <-deadline.C:
			return &upstream.PingResult{Status: 1}, nil
		case <-drop:
			return nil, fmt.Errorf("ping: connection dropped: %w", upstream.ErrUnavailable)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
