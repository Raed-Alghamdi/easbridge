package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"easbridge/internal/vcard"
)

func card(server, uid, name string) ContactCardIn {
	return ContactCardIn{ServerID: server, UID: uid, VCard: []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:" + uid + "\r\nFN:" + name + "\r\nEND:VCARD\r\n")}
}

func addContactFolders(t *testing.T, a *Account) []ContactFolder {
	t.Helper()
	if _, err := a.Update(nil, func(tx *Tx) error {
		_, err := tx.ApplyContactFolders([]FolderInput{
			{ServerID: "c1", ParentServerID: "0", DisplayName: "Contacts", EASType: TypeContacts},
			{ServerID: "c2", ParentServerID: "0", DisplayName: "Clients", EASType: TypeUserContacts},
			{ServerID: "m1", ParentServerID: "0", DisplayName: "Inbox", EASType: TypeInbox},
			{ServerID: "k1", ParentServerID: "0", DisplayName: "Calendar", EASType: 8},
		}, nil, nil, true)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	fs, err := a.ContactFolders()
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func apply(t *testing.T, a *Account, id int64, b ContactBatch) ContactBatchResult {
	t.Helper()
	var res ContactBatchResult
	if _, err := a.Update(nil, func(tx *Tx) error {
		var err error
		res, err = tx.ApplyContactBatch(id, b)
		return err
	}); err != nil {
		t.Fatalf("ApplyContactBatch(%+v): %v", b, err)
	}
	return res
}

func cardNames(t *testing.T, a *Account, id int64) string {
	t.Helper()
	cs, err := a.ContactCards(id)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range cs {
		names = append(names, strings.TrimSuffix(strings.SplitN(string(c.VCard), "FN:", 2)[1][:strings.Index(strings.SplitN(string(c.VCard), "FN:", 2)[1], "\r")], ""))
	}
	return strings.Join(names, ",")
}

func TestContactFoldersAreKeptNextToTheMailFolders(t *testing.T) {
	_, a := openTemp(t)
	fs := addContactFolders(t, a)
	if len(fs) != 2 {
		t.Fatalf("only the contacts folders belong here: %+v", fs)
	}
	if fs[0].BookID != DefaultBookID || fs[0].DisplayName != "Contacts" || fs[0].EASType != TypeContacts {
		t.Errorf("the default folder comes first with the fixed book id: %+v", fs[0])
	}
	if !strings.HasPrefix(fs[1].BookID, "f-") || len(fs[1].BookID) != 18 || fs[1].DisplayName != "Clients" {
		t.Errorf("a user folder gets a hashed book id: %+v", fs[1])
	}
	if fs[0].SyncKey != "0" || fs[0].Complete || fs[0].Downloading {
		t.Errorf("a new folder has not been synced: %+v", fs[0])
	}
	// The mail side does not see them.
	if mail, _ := a.Folders(); len(mail) != 0 {
		t.Errorf("contact folders leaked into the mail folders: %+v", mail)
	}
	applyStandard(t, a)
	if mail, _ := a.Folders(); len(mail) != 8 { // the Outbox and the Calendar of the tree are not exposed as mail folders
		t.Errorf("mail folders = %d", len(mail))
	}
}

func TestContactFolderRenameDeleteAndTypeChange(t *testing.T) {
	_, a := openTemp(t)
	fs := addContactFolders(t, a)
	book := fs[1].BookID
	var diff ContactFolderDiff
	a.Update(nil, func(tx *Tx) (err error) {
		diff, err = tx.ApplyContactFolders(nil, []FolderInput{{ServerID: "c2", DisplayName: "Customers", EASType: TypeUserContacts}}, nil, false)
		return
	})
	if len(diff.Renamed) != 1 || diff.Renamed[0].DisplayName != "Customers" {
		t.Fatalf("diff %+v", diff)
	}
	fs, _ = a.ContactFolders()
	if fs[1].DisplayName != "Customers" || fs[1].BookID != book {
		t.Errorf("a rename keeps the book id: %+v", fs[1])
	}
	// An unchanged folder is not a change.
	a.Update(nil, func(tx *Tx) (err error) {
		diff, err = tx.ApplyContactFolders(nil, []FolderInput{{ServerID: "c2", DisplayName: "Customers", EASType: TypeUserContacts}}, nil, false)
		return
	})
	if !diff.Empty() {
		t.Errorf("nothing changed: %+v", diff)
	}
	// A folder that stops being a contacts folder goes, with its cards.
	apply(t, a, fs[1].ID, ContactBatch{CallKey: "0", NewKey: "1"})
	apply(t, a, fs[1].ID, ContactBatch{CallKey: "1", NewKey: "2", Upserts: []ContactCardIn{card("1", "eas-1", "A")}})
	a.Update(nil, func(tx *Tx) (err error) {
		diff, err = tx.ApplyContactFolders(nil, []FolderInput{{ServerID: "c2", DisplayName: "Customers", EASType: TypeUserMail}}, nil, false)
		return
	})
	if len(diff.Removed) != 1 {
		t.Fatalf("diff %+v", diff)
	}
	if n := countRows(t, a, `SELECT COUNT(*) FROM contact_cards`); n != 0 {
		t.Errorf("the cards of a removed folder stay: %d", n)
	}
	// An explicit delete.
	a.Update(nil, func(tx *Tx) (err error) {
		diff, err = tx.ApplyContactFolders(nil, nil, []string{"c1"}, false)
		return
	})
	if len(diff.Removed) != 1 || diff.Removed[0].ServerID != "c1" {
		t.Errorf("diff %+v", diff)
	}
	if fs, _ = a.ContactFolders(); len(fs) != 0 {
		t.Errorf("folders left: %+v", fs)
	}
}

func TestAFullHierarchyRemovesContactFoldersItNoLongerLists(t *testing.T) {
	_, a := openTemp(t)
	addContactFolders(t, a)
	a.Update(nil, func(tx *Tx) error {
		_, err := tx.ApplyContactFolders([]FolderInput{{ServerID: "c1", DisplayName: "Contacts", EASType: TypeContacts}}, nil, nil, true)
		return err
	})
	fs, _ := a.ContactFolders()
	if len(fs) != 1 || fs[0].ServerID != "c1" {
		t.Errorf("c2 should be gone: %+v", fs)
	}
	// An incremental result that lists nothing removes nothing.
	a.Update(nil, func(tx *Tx) error { _, err := tx.ApplyContactFolders(nil, nil, nil, false); return err })
	if fs, _ = a.ContactFolders(); len(fs) != 1 {
		t.Errorf("an incremental result removed a folder")
	}
}

func TestOnlyOneFolderGetsTheDefaultBookID(t *testing.T) {
	_, a := openTemp(t)
	a.Update(nil, func(tx *Tx) error {
		_, err := tx.ApplyContactFolders([]FolderInput{
			{ServerID: "a", DisplayName: "One", EASType: TypeContacts}, {ServerID: "b", DisplayName: "Two", EASType: TypeContacts}}, nil, nil, true)
		return err
	})
	fs, _ := a.ContactFolders()
	if len(fs) != 2 || fs[0].BookID != DefaultBookID || fs[1].BookID == DefaultBookID {
		t.Errorf("book ids must be unique: %+v", fs)
	}
}

func TestFirstDownloadIsAllOrNothing(t *testing.T) {
	_, a := openTemp(t)
	f := addContactFolders(t, a)[0]
	apply(t, a, f.ID, ContactBatch{CallKey: "0", NewKey: "k1", More: false})
	r := apply(t, a, f.ID, ContactBatch{CallKey: "k1", NewKey: "k2", More: true, Upserts: []ContactCardIn{card("1", "eas-1", "Ada"), card("2", "eas-2", "Bob")}})
	if r.Completed || !r.Downloading {
		t.Errorf("more is coming: %+v", r)
	}
	if got := cardNames(t, a, f.ID); got != "" {
		t.Fatalf("a book lists nothing before the first download has finished, listed %q", got)
	}
	if st, _ := a.ContactStats(); len(st) != 2 || st[0].Complete || st[0].Cards != 0 || st[0].Staged != 2 {
		t.Errorf("stats %+v", st)
	}
	ctag := mustFolder(t, a, f.ID).CTag
	r = apply(t, a, f.ID, ContactBatch{CallKey: "k2", NewKey: "k3", Upserts: []ContactCardIn{card("3", "eas-3", "Cy")}})
	if !r.Completed || r.Downloading || !r.Changed {
		t.Errorf("the last batch completes the download: %+v", r)
	}
	if got := cardNames(t, a, f.ID); got != "Ada,Bob,Cy" {
		t.Errorf("all cards appear at once, got %q", got)
	}
	nf := mustFolder(t, a, f.ID)
	if !nf.Complete || nf.Downloading || nf.SyncKey != "k3" || nf.CTag == ctag {
		t.Errorf("folder after the first download: %+v (ctag was %s)", nf, ctag)
	}
	if st, _ := a.ContactStats(); st[0].Cards != 3 || !st[0].Complete || st[0].Staged != 0 {
		t.Errorf("stats %+v", st)
	}
}

func mustFolder(t *testing.T, a *Account, id int64) *ContactFolder {
	t.Helper()
	f, err := a.ContactFolderByID(id)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestAnEmptyBookCompletes(t *testing.T) {
	_, a := openTemp(t)
	f := addContactFolders(t, a)[0]
	apply(t, a, f.ID, ContactBatch{CallKey: "0", NewKey: "k1"})
	r := apply(t, a, f.ID, ContactBatch{CallKey: "k1", NewKey: "k2"})
	if !r.Completed || !mustFolder(t, a, f.ID).Complete {
		t.Errorf("an empty folder is complete after its data call: %+v", r)
	}
}

func TestIncrementalBatchesAddChangeDeleteAndMoveTheCTag(t *testing.T) {
	_, a := openTemp(t)
	f := addContactFolders(t, a)[0]
	apply(t, a, f.ID, ContactBatch{CallKey: "0", NewKey: "k1"})
	apply(t, a, f.ID, ContactBatch{CallKey: "k1", NewKey: "k2", Upserts: []ContactCardIn{card("1", "eas-1", "Ada"), card("2", "eas-2", "Bob")}})
	c0 := mustFolder(t, a, f.ID).CTag

	r := apply(t, a, f.ID, ContactBatch{CallKey: "k2", NewKey: "k3", Upserts: []ContactCardIn{card("3", "eas-3", "Cy")}})
	c1 := mustFolder(t, a, f.ID).CTag
	if !r.Changed || c1 == c0 {
		t.Errorf("an add changes the ctag: %+v %s %s", r, c0, c1)
	}
	r = apply(t, a, f.ID, ContactBatch{CallKey: "k3", NewKey: "k4", Upserts: []ContactCardIn{card("1", "eas-1", "Ada L")}})
	c2 := mustFolder(t, a, f.ID).CTag
	if !r.Changed || c2 == c1 || cardNames(t, a, f.ID) != "Ada L,Bob,Cy" {
		t.Errorf("a change changes the ctag and the card: %+v %q", r, cardNames(t, a, f.ID))
	}
	// A card that comes again unchanged (a replayed batch) changes nothing.
	r = apply(t, a, f.ID, ContactBatch{CallKey: "k4", NewKey: "k5", Upserts: []ContactCardIn{card("1", "eas-1", "Ada L")}})
	if r.Changed || mustFolder(t, a, f.ID).CTag != c2 {
		t.Errorf("an identical card is not a change: %+v", r)
	}
	r = apply(t, a, f.ID, ContactBatch{CallKey: "k5", NewKey: "k6", Deleted: []string{"2", "999"}})
	c3 := mustFolder(t, a, f.ID).CTag
	if !r.Changed || c3 == c2 || cardNames(t, a, f.ID) != "Ada L,Cy" {
		t.Errorf("a delete changes the ctag: %+v %q", r, cardNames(t, a, f.ID))
	}
	// Only the key moves: the ctag stays.
	apply(t, a, f.ID, ContactBatch{CallKey: "k6", NewKey: "k7"})
	if mustFolder(t, a, f.ID).CTag != c3 || mustFolder(t, a, f.ID).SyncKey != "k7" {
		t.Errorf("an empty batch must not move the ctag")
	}
	// The same UID and bytes in another folder's book do not interfere.
}

func TestAStaleKeyIsRefused(t *testing.T) {
	_, a := openTemp(t)
	f := addContactFolders(t, a)[0]
	apply(t, a, f.ID, ContactBatch{CallKey: "0", NewKey: "k1"})
	_, err := a.Update(nil, func(tx *Tx) error {
		_, err := tx.ApplyContactBatch(f.ID, ContactBatch{CallKey: "0", NewKey: "k9", Upserts: []ContactCardIn{card("1", "eas-1", "X")}})
		return err
	})
	if !errors.Is(err, ErrStaleKey) {
		t.Fatalf("err = %v", err)
	}
	if got := mustFolder(t, a, f.ID).SyncKey; got != "k1" {
		t.Errorf("the key moved to %q", got)
	}
	if n := countRows(t, a, `SELECT COUNT(*) FROM contact_cards`); n != 0 {
		t.Errorf("a refused batch stored %d cards", n)
	}
}

func TestResetRestartsTheDownloadAndSwapsAtomically(t *testing.T) {
	_, a := openTemp(t)
	f := addContactFolders(t, a)[0]
	apply(t, a, f.ID, ContactBatch{CallKey: "0", NewKey: "k1"})
	apply(t, a, f.ID, ContactBatch{CallKey: "k1", NewKey: "k2", Upserts: []ContactCardIn{card("1", "eas-1", "Ada"), card("2", "eas-2", "Bob")}})
	before := mustFolder(t, a, f.ID)

	// The server forgot the key: the call restarted from 0 and brought a new key.
	r := apply(t, a, f.ID, ContactBatch{CallKey: "k2", NewKey: "n1", Reset: true})
	if !r.Downloading || r.Completed || r.Changed {
		t.Errorf("a reset only starts the download: %+v", r)
	}
	if got := cardNames(t, a, f.ID); got != "Ada,Bob" {
		t.Errorf("the old cards stay visible while the new download runs, got %q", got)
	}
	apply(t, a, f.ID, ContactBatch{CallKey: "n1", NewKey: "n2", More: true, Upserts: []ContactCardIn{card("1", "eas-1", "Ada"), card("3", "eas-3", "Cy")}})
	if got := cardNames(t, a, f.ID); got != "Ada,Bob" {
		t.Errorf("a half-finished download is invisible, got %q", got)
	}
	if mustFolder(t, a, f.ID).CTag != before.CTag {
		t.Errorf("the ctag must not move before the swap")
	}
	r = apply(t, a, f.ID, ContactBatch{CallKey: "n2", NewKey: "n3"})
	if !r.Completed || !r.Changed {
		t.Errorf("the swap changed the list (Bob is gone, Cy is new): %+v", r)
	}
	if got := cardNames(t, a, f.ID); got != "Ada,Cy" {
		t.Errorf("the new download replaces the cards, got %q", got)
	}
	if mustFolder(t, a, f.ID).CTag == before.CTag {
		t.Errorf("the ctag moves with the swap")
	}
}

func TestAResetThatLeavesTheListAloneDoesNotMoveTheCTag(t *testing.T) {
	_, a := openTemp(t)
	f := addContactFolders(t, a)[0]
	apply(t, a, f.ID, ContactBatch{CallKey: "0", NewKey: "k1"})
	apply(t, a, f.ID, ContactBatch{CallKey: "k1", NewKey: "k2", Upserts: []ContactCardIn{card("1", "eas-1", "Ada")}})
	ctag := mustFolder(t, a, f.ID).CTag
	apply(t, a, f.ID, ContactBatch{CallKey: "k2", NewKey: "n1", Reset: true})
	r := apply(t, a, f.ID, ContactBatch{CallKey: "n1", NewKey: "n2", Upserts: []ContactCardIn{card("1", "eas-1", "Ada")}})
	if !r.Completed || r.Changed || mustFolder(t, a, f.ID).CTag != ctag {
		t.Errorf("the same cards again are not a change: %+v", r)
	}
}

func TestAResetDuringTheFirstDownloadDropsWhatWasStaged(t *testing.T) {
	_, a := openTemp(t)
	f := addContactFolders(t, a)[0]
	apply(t, a, f.ID, ContactBatch{CallKey: "0", NewKey: "k1"})
	apply(t, a, f.ID, ContactBatch{CallKey: "k1", NewKey: "k2", More: true, Upserts: []ContactCardIn{card("1", "eas-1", "Ada")}})
	apply(t, a, f.ID, ContactBatch{CallKey: "k2", NewKey: "n1", Reset: true})
	if n := countRows(t, a, `SELECT COUNT(*) FROM contact_cards`); n != 0 {
		t.Errorf("the staged cards of the abandoned download stay: %d", n)
	}
	apply(t, a, f.ID, ContactBatch{CallKey: "n1", NewKey: "n2", Upserts: []ContactCardIn{card("9", "eas-9", "Zed")}})
	if got := cardNames(t, a, f.ID); got != "Zed" {
		t.Errorf("got %q", got)
	}
}

func TestADeleteDuringADownloadRemovesTheStagedCard(t *testing.T) {
	_, a := openTemp(t)
	f := addContactFolders(t, a)[0]
	apply(t, a, f.ID, ContactBatch{CallKey: "0", NewKey: "k1"})
	apply(t, a, f.ID, ContactBatch{CallKey: "k1", NewKey: "k2", More: true, Upserts: []ContactCardIn{card("1", "eas-1", "Ada"), card("2", "eas-2", "Bob")}})
	apply(t, a, f.ID, ContactBatch{CallKey: "k2", NewKey: "k3", Deleted: []string{"1"}})
	if got := cardNames(t, a, f.ID); got != "Bob" {
		t.Errorf("got %q", got)
	}
}

func TestContactCardLookupByUIDOnlyServesVisibleCardsOfACompleteBook(t *testing.T) {
	_, a := openTemp(t)
	fs := addContactFolders(t, a)
	f := fs[0]
	apply(t, a, f.ID, ContactBatch{CallKey: "0", NewKey: "k1"})
	apply(t, a, f.ID, ContactBatch{CallKey: "k1", NewKey: "k2", More: true, Upserts: []ContactCardIn{card("1", "eas-1", "Ada")}})
	if _, err := a.ContactCard(f.ID, "eas-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a staged card is not served: %v", err)
	}
	apply(t, a, f.ID, ContactBatch{CallKey: "k2", NewKey: "k3"})
	c, err := a.ContactCard(f.ID, "eas-1")
	if err != nil || c.ETag != vcard.ETag(c.VCard) {
		t.Errorf("%+v %v", c, err)
	}
	if _, err := a.ContactCard(fs[1].ID, "eas-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a card of another book: %v", err)
	}
	// Another account cannot read it.
	db := a.DB()
	other, _ := db.Account("other@example.test")
	if cs, _ := other.ContactCards(f.ID); len(cs) != 0 {
		t.Errorf("another account read the cards: %v", cs)
	}
	if _, err := other.ContactCard(f.ID, "eas-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("another account read a card: %v", err)
	}
}

func TestBatchValidation(t *testing.T) {
	_, a := openTemp(t)
	f := addContactFolders(t, a)[0]
	for _, c := range []ContactCardIn{{UID: "u", VCard: []byte("x")}, {ServerID: "s", VCard: []byte("x")}, {ServerID: "s", UID: "u"}} {
		_, err := a.Update(nil, func(tx *Tx) error {
			_, err := tx.ApplyContactBatch(f.ID, ContactBatch{CallKey: "0", NewKey: "1", Upserts: []ContactCardIn{c}})
			return err
		})
		if err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
	_, err := a.Update(nil, func(tx *Tx) error {
		_, err := tx.ApplyContactBatch(9999, ContactBatch{CallKey: "0", NewKey: "1"})
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown folder: %v", err)
	}
}

func countRows(t *testing.T, a *Account, q string, args ...any) int {
	t.Helper()
	var n int
	if err := a.DB().r.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ---- the directory ----

func mailFrom(id, from, to, cc, date string) []byte {
	h := "Message-ID: <" + id + ">\r\nFrom: " + from + "\r\nTo: " + to + "\r\n"
	if cc != "" {
		h += "Cc: " + cc + "\r\n"
	}
	return []byte(h + "Subject: s\r\nDate: " + date + "\r\n\r\nbody\r\n")
}

func storeMail(t *testing.T, a *Account, folder *Folder, raw []byte, when time.Time) {
	t.Helper()
	if _, err := a.Update(nil, func(tx *Tx) error {
		_, err := tx.AddMessage(folder.ID, NewMessage{MIME: raw, InternalDate: when})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStoredMessagesAddCandidatesOnlyInTheDirectoryDomains(t *testing.T) {
	db, a := openTemp(t)
	inbox := applyStandard(t, a)["1"]
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	// Directory off: nothing is noted.
	storeMail(t, a, inbox, mailFrom("a", "x@corp.example", "y@corp.example", "", "Thu, 01 Oct 2026 12:00:00 +0000"), t0)
	if cs, _ := a.Candidates(); len(cs) != 0 {
		t.Fatalf("with the directory off nothing is noted: %+v", cs)
	}

	db.SetDirectoryDomains([]string{"Corp.Example"})
	storeMail(t, a, inbox, mailFrom("b", "=?UTF-8?B?2LnZhdixINin2YTYs9mE2YXZig==?= <Omar@Corp.Example>", "me@other.example, Pat <pat@corp.example>", "pat@corp.example, sub@mail.corp.example", ""), t0.Add(time.Hour))
	storeMail(t, a, inbox, mailFrom("c", "omar@corp.example", "me@other.example", "", ""), t0.Add(2*time.Hour))
	cs, err := a.Candidates()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Candidate{}
	for _, c := range cs {
		got[c.Email] = c
	}
	if len(got) != 2 {
		t.Fatalf("candidates: %+v (other.example and the sub domain are not directory domains)", cs)
	}
	o := got["omar@corp.example"]
	if o.SeenCount != 2 || o.LastSeen != t0.Add(2*time.Hour).Unix() || o.FirstSeen != t0.Add(time.Hour).Unix() || o.State != CandNew {
		t.Errorf("omar: %+v", o)
	}
	if o.Name != "عمر السلمي" {
		t.Errorf("the name of the latest message that had one is kept: %q (the later message had none)", o.Name)
	}
	if p := got["pat@corp.example"]; p.SeenCount != 1 || p.Name != "Pat" {
		t.Errorf("an address counts once per message even when it is in To and Cc: %+v", p)
	}
}

func TestStagedAndLocalMessagesCountToo(t *testing.T) {
	db, a := openTemp(t)
	db.SetDirectoryDomains([]string{"corp.example"})
	inbox := applyStandard(t, a)["1"]
	a.Update(nil, func(tx *Tx) error {
		return tx.AddStagedMessage(inbox.ID, NewMessage{ServerID: "s1", MIME: mailFrom("a", "x@corp.example", "me@o.example", "", "")})
	})
	a.Update(nil, func(tx *Tx) error {
		_, err := tx.AddMessage(inbox.ID, NewMessage{Kind: KindLocalDraft, MIME: mailFrom("b", "me@o.example", "y@corp.example", "", "")})
		return err
	})
	if cs, _ := a.Candidates(); len(cs) != 2 {
		t.Errorf("%+v", cs)
	}
}

func TestSeedingReadsStoredMessagesInBatchesAndIsAccurate(t *testing.T) {
	db, a := openTemp(t)
	inbox := applyStandard(t, a)["1"]
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 7; i++ {
		storeMail(t, a, inbox, mailFrom(fmt.Sprint("m", i), fmt.Sprintf("p%d@corp.example", i%3), "me@other.example", "", ""), t0.Add(time.Duration(i)*time.Hour))
	}
	var after int64
	batches := 0
	for {
		var done bool
		if _, err := a.Update(nil, func(tx *Tx) (err error) {
			after, done, err = tx.SeedCandidates(after, 3, []string{"corp.example"})
			return
		}); err != nil {
			t.Fatal(err)
		}
		batches++
		if done {
			break
		}
		if batches > 10 {
			t.Fatal("seeding does not end")
		}
	}
	if batches != 3 {
		t.Errorf("7 messages in batches of 3 take 3 batches, took %d", batches)
	}
	cs, _ := a.Candidates()
	counts := map[string]int{}
	for _, c := range cs {
		counts[c.Email] = c.SeenCount
	}
	if len(counts) != 3 || counts["p0@corp.example"] != 3 || counts["p1@corp.example"] != 2 || counts["p2@corp.example"] != 2 {
		t.Errorf("counts %v", counts)
	}
	// Recency comes from the date of the message, not from the time of the seeding.
	if cs[0].Email != "p0@corp.example" || cs[0].LastSeen != t0.Add(6*time.Hour).Unix() {
		t.Errorf("most recent first: %+v", cs[0])
	}
	// Other accounts' messages are not read.
	_ = db
	other, _ := a.DB().Account("other@example.test")
	var last int64
	other.Update(nil, func(tx *Tx) (err error) {
		last, _, err = tx.SeedCandidates(0, 100, []string{"corp.example"})
		return
	})
	if cs, _ := other.Candidates(); len(cs) != 0 || last != 0 {
		t.Errorf("another account's seeding read this account's mail: %+v", cs)
	}
}

func TestNextCandidateOrderDomainsAndDueness(t *testing.T) {
	_, a := openTemp(t)
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	note := func(email string, seen time.Time, times int) {
		for i := 0; i < times; i++ {
			a.Update(nil, func(tx *Tx) error { return tx.NoteCandidate(email, "", seen) })
		}
	}
	note("old-frequent@corp.example", now.Add(-72*time.Hour), 9)
	note("recent-once@corp.example", now.Add(-1*time.Hour), 1)
	note("recent-twice@corp.example", now.Add(-1*time.Hour), 2)
	note("me@corp.example", now, 5)
	note("elsewhere@other.example", now, 5)
	doms := []string{"corp.example"}
	var order []string
	for i := 0; i < 5; i++ {
		c, err := a.NextCandidate(now, doms, "ME@corp.example")
		if err != nil {
			t.Fatal(err)
		}
		if c == nil {
			break
		}
		order = append(order, c.Email)
		a.Update(nil, func(tx *Tx) error {
			return tx.UpdateCandidate(c.Email, CandidateUpdate{State: CandNotFound, CheckedAt: now, NextCheck: now.Add(time.Hour)})
		})
	}
	want := "recent-twice@corp.example,recent-once@corp.example,old-frequent@corp.example"
	if strings.Join(order, ",") != want {
		t.Errorf("order %v, want %s (the most recent first, the more frequent among equals; own address and other domains never)", order, want)
	}
	if n, _ := a.CandidateQueue(now, doms, "me@corp.example"); n != 0 {
		t.Errorf("queue %d", n)
	}
	if n, _ := a.CandidateQueue(now.Add(2*time.Hour), doms, "me@corp.example"); n != 3 {
		t.Errorf("after the negative cache expired the queue is %d", n)
	}
	if c, _ := a.NextCandidate(now, nil, ""); c != nil {
		t.Errorf("no domains, no candidate: %+v", c)
	}
}

func TestDirectoryCardsAndTheirCTag(t *testing.T) {
	_, a := openTemp(t)
	if st, _ := a.DirectoryState(); st.CTag != "0-0" {
		t.Errorf("fresh ctag %q", st.CTag)
	}
	put := func(email, name string) bool {
		var changed bool
		a.Update(nil, func(tx *Tx) (err error) {
			changed, err = tx.PutDirectoryCard(email, []byte("BEGIN:VCARD\r\nFN:"+name+"\r\nEND:VCARD\r\n"))
			return
		})
		return changed
	}
	if !put("Ada@Corp.example", "Ada") {
		t.Error("a new card is a change")
	}
	st1, _ := a.DirectoryState()
	if put("ada@corp.example", "Ada") {
		t.Error("the same bytes are not a change")
	}
	if st, _ := a.DirectoryState(); st.CTag != st1.CTag {
		t.Error("the ctag moved without a change")
	}
	if !put("ada@corp.example", "Ada Lovelace") {
		t.Error("other bytes are a change")
	}
	st2, _ := a.DirectoryState()
	if st2.CTag == st1.CTag {
		t.Error("the ctag must move")
	}
	c, err := a.DirectoryCardByEmail("ADA@corp.example")
	if err != nil || c.UID != vcard.DirectoryUID("ada@corp.example") {
		t.Fatalf("%+v %v", c, err)
	}
	if byUID, err := a.DirectoryCard(c.UID); err != nil || byUID.ETag != c.ETag {
		t.Errorf("%+v %v", byUID, err)
	}
	if n, _ := a.DirectoryCardCount(); n != 1 {
		t.Errorf("count %d", n)
	}
	var gone bool
	a.Update(nil, func(tx *Tx) (err error) { gone, err = tx.DeleteDirectoryCard("Ada@corp.example"); return })
	st3, _ := a.DirectoryState()
	if !gone || st3.CTag == st2.CTag {
		t.Errorf("delete: %v %s %s", gone, st2.CTag, st3.CTag)
	}
	a.Update(nil, func(tx *Tx) (err error) { gone, err = tx.DeleteDirectoryCard("ada@corp.example"); return })
	if gone {
		t.Error("deleting nothing is no change")
	}
	if cs, _ := a.DirectoryCards(); len(cs) != 0 {
		t.Errorf("%v", cs)
	}
	_, err = a.Update(nil, func(tx *Tx) error { _, err := tx.PutDirectoryCard("", []byte("x")); return err })
	if err == nil {
		t.Error("an empty address was accepted")
	}
}

func TestDirectoryStateSearchCountPauseAndSeedProgress(t *testing.T) {
	_, a := openTemp(t)
	d1 := time.Date(2026, 10, 12, 23, 59, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		a.Update(nil, func(tx *Tx) error { return tx.RecordSearch(d1.Add(time.Duration(i) * time.Second)) })
	}
	st, _ := a.DirectoryState()
	if st.LookupsOn("2026-10-12") != 3 || st.LookupsOn("2026-10-13") != 0 || st.LastSearchMs != d1.Add(2*time.Second).UnixMilli() {
		t.Errorf("%+v", st)
	}
	a.Update(nil, func(tx *Tx) error { return tx.RecordSearch(d1.Add(2 * time.Minute)) })
	if st, _ = a.DirectoryState(); st.LookupsOn("2026-10-13") != 1 {
		t.Errorf("a new UTC day starts again: %+v", st)
	}
	until := d1.Add(24 * time.Hour)
	a.Update(nil, func(tx *Tx) error { return tx.SetDirectoryPause(until) })
	a.Update(nil, func(tx *Tx) error { return tx.SetSeedProgress("a,b", 42, true) })
	st, _ = a.DirectoryState()
	if st.PausedUntil != until.Unix() || st.SeededFor != "a,b" || st.SeedAfter != 42 || !st.SeedDone {
		t.Errorf("%+v", st)
	}
	a.Update(nil, func(tx *Tx) error { return tx.SetDirectoryPause(time.Time{}) })
	if st, _ = a.DirectoryState(); st.PausedUntil != 0 {
		t.Errorf("%+v", st)
	}
	if DomainsKey([]string{"B.example", "a.example"}) != "a.example,b.example" {
		t.Error("DomainsKey is canonical")
	}
}

func TestDeleteContactDataRemovesEverythingOfTheAccountAndNothingElse(t *testing.T) {
	db, a := openTemp(t)
	b, _ := db.Account("other@example.test")
	for _, acct := range []*Account{a, b} {
		a2 := acct
		a2.Update(nil, func(tx *Tx) error {
			if _, err := tx.ApplyContactFolders([]FolderInput{{ServerID: "c1", DisplayName: "Contacts", EASType: TypeContacts}}, nil, nil, true); err != nil {
				return err
			}
			if _, err := tx.PutDirectoryCard("x@corp.example", []byte("v")); err != nil {
				return err
			}
			if err := tx.NoteCandidate("x@corp.example", "X", time.Now()); err != nil {
				return err
			}
			return tx.SetDirectoryPause(time.Now().Add(time.Hour))
		})
		fs, _ := a2.ContactFolders()
		apply(t, a2, fs[0].ID, ContactBatch{CallKey: "0", NewKey: "1"})
		apply(t, a2, fs[0].ID, ContactBatch{CallKey: "1", NewKey: "2", Upserts: []ContactCardIn{card("1", "u1", "N")}})
	}
	if _, err := a.Update(nil, func(tx *Tx) error { return tx.DeleteContactData() }); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`SELECT COUNT(*) FROM contact_folders`, `SELECT COUNT(*) FROM contact_cards`, `SELECT COUNT(*) FROM directory_cards`,
		`SELECT COUNT(*) FROM directory_candidates`, `SELECT COUNT(*) FROM directory_state`} {
		if n := countRows(t, a, q); n != 1 {
			t.Errorf("%s: %d rows left, want the other account's one", q, n)
		}
	}
	if fs, _ := a.ContactFolders(); len(fs) != 0 {
		t.Errorf("%+v", fs)
	}
	if fs, _ := b.ContactFolders(); len(fs) != 1 {
		t.Errorf("the other account lost its folders")
	}
}

func TestPurgeContactDataAndDirectoryData(t *testing.T) {
	db, a := openTemp(t)
	if f, d, err := db.PurgeContactData(); err != nil || f != 0 || d != 0 {
		t.Errorf("nothing to purge: %d %d %v", f, d, err)
	}
	a.Update(nil, func(tx *Tx) error {
		tx.ApplyContactFolders([]FolderInput{{ServerID: "c1", DisplayName: "Contacts", EASType: TypeContacts}}, nil, nil, true)
		tx.PutDirectoryCard("x@corp.example", []byte("v"))
		return tx.NoteCandidate("x@corp.example", "", time.Now())
	})
	n, err := db.PurgeDirectoryData()
	if err != nil || n < 2 {
		t.Errorf("directory purge: %d %v", n, err)
	}
	if fs, _ := a.ContactFolders(); len(fs) != 1 {
		t.Errorf("a directory purge leaves the contact folders")
	}
	f, d, err := db.PurgeContactData()
	if err != nil || f != 1 {
		t.Errorf("purge: %d %d %v", f, d, err)
	}
	if fs, _ := a.ContactFolders(); len(fs) != 0 {
		t.Errorf("folders left")
	}
}

func TestMigrationsAddTheTablesToACopyOfASchema5DatabaseAndGoStraightTo8(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open(driverName, "file:"+filepath.Join(dir, DBFileName))
	if err != nil {
		t.Fatal(err)
	}
	// A database as v0.1.8 left it: schema 5, an account, a folder, a message and a kept Sent copy.
	for i := 0; i < 5; i++ {
		if _, err := raw.Exec(migrations[i]); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`PRAGMA user_version = 5`,
		`INSERT INTO meta(key, value) VALUES ('device_id', '0123456789abcdef0123456789abcdef')`,
		`INSERT INTO accounts(id, user, created_at, folder_sync_key) VALUES (1, 'a@corp.example', 0, 'hk7')`,
		`INSERT INTO folders(id, account_id, server_id, display_name, eas_type, path, uid_validity, uid_next, sync_key, initial_sync_complete) VALUES (1, 1, '1', 'Inbox', 2, 'INBOX', 5, 2, 'k', 1)`,
		`INSERT INTO messages(id, folder_id, uid, internal_date, size, header, flags) VALUES (1, 1, 1, 1000, 5, X'46726f6d3a20782540636f72702e6578616d706c650d0a0d0a', 0)`,
		`INSERT INTO mime(message_pk, data) VALUES (1, X'46726f6d3a20782540636f72702e6578616d706c650d0a0d0a00')`,
		`INSERT INTO sent_copy(account_id, message_id, created_at, data) VALUES (1, 'm@x', 1, X'00')`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	raw.Close()
	// The copy is what a rollback would restore.
	snapshot, err := os.ReadFile(filepath.Join(dir, DBFileName))
	if err != nil {
		t.Fatal(err)
	}

	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if v, _ := db.SchemaVersion(); v != 8 || len(migrations) != 8 {
		t.Fatalf("schema version %d, %d migrations; want 8 (0.2.0 and 0.2.1 never ran against a schema 5 database: it goes to 8 directly)", v, len(migrations))
	}
	acct, err := db.Account("a@corp.example")
	if err != nil || acct.ID != 1 {
		t.Fatalf("account after the migration: %+v %v", acct, err)
	}
	if k, _ := acct.FolderSyncKey(); k != "hk7" {
		t.Errorf("the hierarchy key changed: %q", k)
	}
	if id, _ := db.DeviceID(); id != "0123456789abcdef0123456789abcdef" {
		t.Errorf("the DeviceId changed: %q", id)
	}
	fs, _ := acct.Folders()
	if len(fs) != 1 || fs[0].Path != "INBOX" || fs[0].UIDNext != 2 {
		t.Fatalf("mail folders after the migration: %+v", fs)
	}
	if m, err := acct.Message(fs[0].ID, 1); err != nil || m.InternalDate.Unix() != 1000 {
		t.Errorf("the stored message: %+v %v", m, err)
	}
	if n, _ := keptCount(t, acct); n != 1 {
		t.Errorf("the kept Sent copy: %d", n)
	}
	// The new tables are there, empty, and work.
	for _, tbl := range []string{"contact_folders", "contact_cards", "directory_cards", "directory_candidates", "directory_state"} {
		if n := countRows(t, acct, `SELECT COUNT(*) FROM `+tbl); n != 0 {
			t.Errorf("%s has %d rows after the migration", tbl, n)
		}
	}
	if folders := addContactFolders(t, acct); len(folders) != 2 {
		t.Errorf("the new tables cannot be written: %+v", folders)
	}
	// The migration itself fills nothing from the stored mail: seeding is a task of the engine.
	db.SetDirectoryDomains([]string{"corp.example"})
	if cs, _ := acct.Candidates(); len(cs) != 0 {
		t.Errorf("the migration must not seed candidates: %+v", cs)
	}
	db.Close()

	// v0.1.8 refuses a schema 6 database; the snapshot of schema 5 still opens with this build.
	older := migrations
	migrations = migrations[:5]
	_, err = Open(dir)
	migrations = older
	if err == nil || !strings.Contains(err.Error(), "newer than this build") {
		t.Errorf("a build that knows only schema 5 must refuse the migrated database, got %v", err)
	}
	rb := filepath.Join(t.TempDir(), "rollback")
	os.MkdirAll(rb, 0o700)
	os.WriteFile(filepath.Join(rb, DBFileName), snapshot, 0o600)
	migrations = migrations[:5]
	rdb, err := Open(rb)
	migrations = older
	if err != nil {
		t.Fatalf("the pre-upgrade snapshot opens with the old schema: %v", err)
	}
	rdb.Close()
}
