//go:build devfake

package engine_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"easbridge/internal/engine"
	"easbridge/internal/fakeupstream"
	"easbridge/internal/store"
	"easbridge/internal/upstream"
	"easbridge/internal/vcard"
)

// withContacts switches the contacts feature on with fast timers, three contacts
// per Sync response (so that a download takes several round trips).
func withContacts(mods ...func(*engine.Config, *fakeupstream.Server)) func(*engine.Config, *fakeupstream.Server) {
	return func(c *engine.Config, s *fakeupstream.Server) {
		c.ContactsEnabled = true
		c.ContactsInterval = 60 * time.Millisecond
		s.Window = 3
		for _, m := range mods {
			m(c, s)
		}
	}
}

func person(first, last, email string) upstream.Contact {
	return upstream.Contact{FirstName: first, LastName: last, Email1: email, BusinessPhone: "+1 555 011 0100", Company: "Example Co"}
}

func (e *env) contactFolder(a *engine.Account, book string) *store.ContactFolder {
	e.t.Helper()
	var f *store.ContactFolder
	eventually(e.t, "contact folder "+book, func() bool {
		var err error
		f, err = a.Store().ContactFolderByBook(book)
		return err == nil
	})
	return f
}

func (e *env) cards(a *engine.Account, folderID int64) []store.Card {
	cs, err := a.Store().ContactCards(folderID)
	if err != nil {
		e.t.Fatal(err)
	}
	return cs
}

func (e *env) waitCards(a *engine.Account, folderID int64, n int) []store.Card {
	e.t.Helper()
	var cs []store.Card
	eventually(e.t, fmt.Sprintf("%d cards in the book", n), func() bool {
		cs = e.cards(a, folderID)
		return len(cs) == n
	})
	return cs
}

func TestContactsAreOffUnlessEnabled(t *testing.T) {
	e := newEnv(t)
	fid := e.srv.AddContactFolder(testUser, "Contacts", fakeupstream.TypeContacts)
	e.srv.AddContact(testUser, fid, person("Ada", "Lovelace", "ada@corp.example"))
	a := e.login()
	e.mirrored(a, "Inbox", "INBOX")
	consistently(t, "no contacts activity", 200*time.Millisecond, func() bool {
		fs, _ := a.Store().ContactFolders()
		return len(fs) == 0 && e.srv.Calls("SyncContacts") == 0 && e.srv.Calls("SearchDirectory") == 0
	})
	if e.mgr.Status().Accounts[0].Contacts != nil {
		t.Errorf("the health report mentions contacts although the feature is off")
	}
}

func TestContactBooksFollowTheHierarchy(t *testing.T) {
	e := newEnv(t, withContacts())
	def := e.srv.AddContactFolder(testUser, "Contacts", fakeupstream.TypeContacts)
	user := e.srv.AddContactFolder(testUser, "Clients", fakeupstream.TypeUserContacts)
	a := e.login()
	f1 := e.contactFolder(a, store.DefaultBookID)
	if f1.ServerID != def || f1.EASType != 9 {
		t.Errorf("default book: %+v", f1)
	}
	fs, _ := a.Store().ContactFolders()
	if len(fs) != 2 || fs[1].ServerID != user || fs[1].DisplayName != "Clients" || !strings.HasPrefix(fs[1].BookID, "f-") {
		t.Fatalf("books: %+v", fs)
	}
	e.srv.RenameFolder(testUser, user, "Customers")
	eventually(t, "rename", func() bool {
		fs, _ := a.Store().ContactFolders()
		return len(fs) == 2 && fs[1].DisplayName == "Customers"
	})
	e.srv.RemoveFolder(testUser, user)
	eventually(t, "delete", func() bool {
		fs, _ := a.Store().ContactFolders()
		return len(fs) == 1
	})
	// A new folder appears and is synced.
	n := e.srv.AddContactFolder(testUser, "Family", fakeupstream.TypeUserContacts)
	e.srv.AddContact(testUser, n, person("Zed", "Z", "zed@example.test"))
	eventually(t, "new folder", func() bool {
		fs, _ := a.Store().ContactFolders()
		if len(fs) != 2 {
			return false
		}
		return fs[1].Complete && len(e.cards(a, fs[1].ID)) == 1
	})
	// Mail is unaffected: no contact folder in the mail list.
	for _, f := range mustFolders(t, a) {
		if f.EASType == 9 || f.EASType == 14 {
			t.Errorf("a contact folder among the mail folders: %+v", f)
		}
	}
}

func mustFolders(t *testing.T, a *engine.Account) []store.Folder {
	t.Helper()
	fs, err := a.Store().Folders()
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func TestContactsAreNotPartOfPing(t *testing.T) {
	e := newEnv(t, withContacts())
	cid := e.srv.AddContactFolder(testUser, "Contacts", fakeupstream.TypeContacts)
	e.srv.AddContact(testUser, cid, person("Ada", "L", "ada@example.test"))
	a := e.login()
	e.mirrored(a, "Inbox", "INBOX")
	f := e.contactFolder(a, store.DefaultBookID)
	e.waitCards(a, f.ID, 1)
	eventually(t, "a Ping has been made", func() bool { return e.srv.Calls("Ping") >= 1 })
	if e.srv.PingedFolders()[cid] {
		t.Errorf("the contacts folder is in a Ping")
	}
}

func TestFirstContactDownloadIsAllOrNothing(t *testing.T) {
	e := newEnv(t, withContacts())
	cid := e.srv.AddContactFolder(testUser, "Contacts", fakeupstream.TypeContacts)
	for i := 0; i < 8; i++ {
		e.srv.AddContact(testUser, cid, person(fmt.Sprintf("First%d", i), "Last", fmt.Sprintf("p%d@example.test", i)))
	}
	// The bootstrap and the first data call go through, then the download freezes.
	e.srv.HoldAfter("SyncContacts", testUser, cid, 2)
	a := e.login()
	f := e.contactFolder(a, store.DefaultBookID)
	eventually(t, "the download is under way", func() bool {
		st, _ := a.Store().ContactStats()
		return len(st) == 1 && st[0].Downloading && st[0].Staged == 3
	})
	consistently(t, "an unfinished book lists nothing", 150*time.Millisecond, func() bool {
		st, _ := a.Store().ContactStats()
		return len(e.cards(a, f.ID)) == 0 && !st[0].Complete && st[0].Cards == 0
	})
	e.srv.Release("SyncContacts", testUser, cid)
	cs := e.waitCards(a, f.ID, 8)
	if nf, _ := a.Store().ContactFolderByID(f.ID); !nf.Complete || nf.Downloading {
		t.Errorf("folder: %+v", nf)
	}
	for _, c := range cs {
		if !strings.HasPrefix(c.UID, "eas-") || c.ETag != vcard.ETag(c.VCard) || !strings.Contains(string(c.VCard), "VERSION:3.0") {
			t.Errorf("card: %+v", c)
		}
	}
}

func cardOf(t *testing.T, cs []store.Card, uid string) string {
	t.Helper()
	for _, c := range cs {
		if c.UID == uid {
			return string(c.VCard)
		}
	}
	return ""
}

func TestContactAddsChangesAndDeletesAreApplied(t *testing.T) {
	e := newEnv(t, withContacts())
	cid := e.srv.AddContactFolder(testUser, "Contacts", fakeupstream.TypeContacts)
	id1 := e.srv.AddContact(testUser, cid, person("Ada", "Lovelace", "ada@example.test"))
	id2 := e.srv.AddContact(testUser, cid, person("Bob", "Builder", "bob@example.test"))
	a := e.login()
	f := e.contactFolder(a, store.DefaultBookID)
	e.waitCards(a, f.ID, 2)
	ctag0 := mustContactFolder(t, a, f.ID).CTag
	uid1 := vcard.PersonalUID(testUser, cid, id1)

	c := person("Ada", "King", "ada@example.test")
	e.srv.UpdateContact(testUser, cid, id1, c)
	eventually(t, "the change", func() bool { return strings.Contains(cardOf(t, e.cards(a, f.ID), uid1), "N:King;Ada") })
	ctag1 := mustContactFolder(t, a, f.ID).CTag
	if ctag1 == ctag0 {
		t.Errorf("a change must move the ctag")
	}

	id3 := e.srv.AddContact(testUser, cid, person("Cy", "Young", "cy@example.test"))
	e.waitCards(a, f.ID, 3)
	if mustContactFolder(t, a, f.ID).CTag == ctag1 {
		t.Errorf("an add must move the ctag")
	}
	ctag2 := mustContactFolder(t, a, f.ID).CTag

	e.srv.DeleteContact(testUser, cid, id2)
	cs := e.waitCards(a, f.ID, 2)
	if mustContactFolder(t, a, f.ID).CTag == ctag2 {
		t.Errorf("a delete must move the ctag")
	}
	uids := []string{cs[0].UID, cs[1].UID}
	if !(contains(strings.Join(uids, ","), uid1) && contains(strings.Join(uids, ","), vcard.PersonalUID(testUser, cid, id3))) {
		t.Errorf("the wrong cards remain: %v", uids)
	}
	// Quiet: the ctag stays while nothing changes, though the folder is polled.
	ctag3 := mustContactFolder(t, a, f.ID).CTag
	calls := e.srv.Calls("SyncContacts")
	eventually(t, "polling", func() bool { return e.srv.Calls("SyncContacts") >= calls+3 })
	if mustContactFolder(t, a, f.ID).CTag != ctag3 {
		t.Errorf("the ctag moved without a change")
	}
}

func mustContactFolder(t *testing.T, a *engine.Account, id int64) *store.ContactFolder {
	t.Helper()
	f, err := a.Store().ContactFolderByID(id)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestInvalidSyncKeyRestartsTheChainAndReplacesTheCardsAtomically(t *testing.T) {
	e := newEnv(t, withContacts())
	cid := e.srv.AddContactFolder(testUser, "Contacts", fakeupstream.TypeContacts)
	ids := map[string]string{}
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		ids[n] = e.srv.AddContact(testUser, cid, person(strings.ToUpper(n), "Person", n+"@example.test"))
	}
	a := e.login()
	f := e.contactFolder(a, store.DefaultBookID)
	e.waitCards(a, f.ID, 7)
	ctag0 := mustContactFolder(t, a, f.ID).CTag

	// While the bridge is not looking: one contact is deleted, one changed, one added; the key is lost.
	e.srv.HoldAfter("SyncContacts", testUser, cid, 0) // freeze before the restart can start
	e.srv.DeleteContact(testUser, cid, ids["a"])
	e.srv.UpdateContact(testUser, cid, ids["b"], person("B", "Changed", "b@example.test"))
	nid := e.srv.AddContact(testUser, cid, person("H", "New", "h@example.test"))
	e.srv.ResetContactFolder(testUser, cid)
	// Let the restart's bootstrap and first data call through, then freeze again.
	e.srv.Release("SyncContacts", testUser, cid)
	e.srv.HoldAfter("SyncContacts", testUser, cid, 2)
	eventually(t, "the new download is under way", func() bool {
		st, _ := a.Store().ContactStats()
		return len(st) == 1 && st[0].Downloading && st[0].Staged > 0
	})
	consistently(t, "the old cards stay while the new download runs", 150*time.Millisecond, func() bool {
		return len(e.cards(a, f.ID)) == 7 && mustContactFolder(t, a, f.ID).CTag == ctag0
	})
	e.srv.Release("SyncContacts", testUser, cid)
	eventually(t, "the swap", func() bool { return mustContactFolder(t, a, f.ID).CTag != ctag0 })
	cs := e.cards(a, f.ID)
	if len(cs) != 7 { // 7 - a + h
		t.Fatalf("%d cards after the swap", len(cs))
	}
	if cardOf(t, cs, vcard.PersonalUID(testUser, cid, ids["a"])) != "" {
		t.Errorf("a contact deleted in the gap is still served")
	}
	if !strings.Contains(cardOf(t, cs, vcard.PersonalUID(testUser, cid, ids["b"])), "Changed") {
		t.Errorf("a contact changed in the gap is stale")
	}
	if cardOf(t, cs, vcard.PersonalUID(testUser, cid, nid)) == "" {
		t.Errorf("a contact added in the gap is missing")
	}
	if !contains(e.logs.String(), "server reset the contacts sync key") {
		t.Errorf("no log line about the reset")
	}
}

func TestInvalidSyncKeyDuringTheFirstDownloadStartsOver(t *testing.T) {
	e := newEnv(t, withContacts())
	cid := e.srv.AddContactFolder(testUser, "Contacts", fakeupstream.TypeContacts)
	for i := 0; i < 8; i++ {
		e.srv.AddContact(testUser, cid, person(fmt.Sprintf("F%d", i), "L", fmt.Sprintf("p%d@example.test", i)))
	}
	e.srv.HoldAfter("SyncContacts", testUser, cid, 2)
	a := e.login()
	f := e.contactFolder(a, store.DefaultBookID)
	eventually(t, "half way", func() bool {
		st, _ := a.Store().ContactStats()
		return len(st) == 1 && st[0].Staged == 3
	})
	e.srv.ResetContactFolder(testUser, cid)
	e.srv.Release("SyncContacts", testUser, cid)
	e.waitCards(a, f.ID, 8)
}

func TestContactSyncRetriesAfterFailuresAndStaysOffWhileTheBreakerIsOpen(t *testing.T) {
	e := newEnv(t, withContacts(func(c *engine.Config, _ *fakeupstream.Server) {
		c.BreakerBase, c.BreakerMax = 300*time.Millisecond, time.Second
	}))
	cid := e.srv.AddContactFolder(testUser, "Contacts", fakeupstream.TypeContacts)
	e.srv.AddContact(testUser, cid, person("Ada", "L", "ada@example.test"))
	e.srv.FailNext("SyncContacts", &upstream.ThrottleError{HTTP: 429, Explicit: true, RetryAfter: 400 * time.Millisecond}, 1)
	a := e.login()
	eventually(t, "the throttled call", func() bool { return e.srv.Calls("SyncContacts") == 1 })
	eventually(t, "the breaker is open", func() bool { return e.mgr.Status().Accounts[0].Paused })
	n := e.srv.Calls("SyncContacts")
	consistently(t, "nothing leaves while the breaker is open", 250*time.Millisecond, func() bool { return e.srv.Calls("SyncContacts") == n })
	f := e.contactFolder(a, store.DefaultBookID)
	e.waitCards(a, f.ID, 1)
}

func TestContactSyncBacksOffAfterAnError(t *testing.T) {
	e := newEnv(t, withContacts())
	cid := e.srv.AddContactFolder(testUser, "Contacts", fakeupstream.TypeContacts)
	e.srv.AddContact(testUser, cid, person("Ada", "L", "ada@example.test"))
	e.srv.FailNext("SyncContacts", errors.New("boom"), 3)
	a := e.login()
	f := e.contactFolder(a, store.DefaultBookID)
	e.waitCards(a, f.ID, 1)
	if e.srv.Calls("SyncContacts") < 4 {
		t.Errorf("calls = %d", e.srv.Calls("SyncContacts"))
	}
}

func TestPersonalCardsHaveStableUIDsAndAreScopedToTheirFolder(t *testing.T) {
	e := newEnv(t, withContacts())
	c1 := e.srv.AddContactFolder(testUser, "Contacts", fakeupstream.TypeContacts)
	c2 := e.srv.AddContactFolder(testUser, "Clients", fakeupstream.TypeUserContacts)
	same := person("Same", "Item", "same@example.test")
	same.ServerID = "shared-id"
	e.srv.AddContact(testUser, c1, same)
	e.srv.AddContact(testUser, c2, same)
	a := e.login()
	b1, b2 := e.contactFolder(a, store.DefaultBookID), (*store.ContactFolder)(nil)
	fs, _ := a.Store().ContactFolders()
	b2 = &fs[1]
	x := e.waitCards(a, b1.ID, 1)
	y := e.waitCards(a, b2.ID, 1)
	if x[0].UID == y[0].UID {
		t.Errorf("the same item ServerId in two folders must give two UIDs")
	}
	if x[0].UID != vcard.PersonalUID(testUser, c1, "shared-id") || y[0].UID != vcard.PersonalUID(testUser, c2, "shared-id") {
		t.Errorf("uids %s %s", x[0].UID, y[0].UID)
	}
}

func TestHealthCountsContactBooks(t *testing.T) {
	e := newEnv(t, withContacts())
	cid := e.srv.AddContactFolder(testUser, "Contacts", fakeupstream.TypeContacts)
	e.srv.AddContact(testUser, cid, person("Ada", "L", "ada@example.test"))
	e.srv.AddContact(testUser, cid, person("Bob", "L", "bob@example.test"))
	a := e.login()
	f := e.contactFolder(a, store.DefaultBookID)
	e.waitCards(a, f.ID, 2)
	st := e.mgr.Status().Accounts[0].Contacts
	if st == nil || len(st.Books) != 1 || !st.Books[0].Complete || st.Books[0].Cards != 2 || st.DirectoryEnabled {
		t.Errorf("status: %+v", st)
	}
}

func TestNothingAboutContactsReachesTheLog(t *testing.T) {
	e := newEnv(t, withContacts())
	cid := e.srv.AddContactFolder(testUser, "Secret Clients", fakeupstream.TypeContacts)
	c := person("Noorhan", "Alsulami", "noorhan.private@example.test")
	c.HomePhone, c.MobilePhone, c.Office = "+1 555 055 4567", "+1 555 050 4321", "Tower B"
	c.HomeAddress = upstream.Address{Street: "12 Hidden Street", City: "Jeddah"}
	e.srv.AddContact(testUser, cid, c)
	e.srv.AddContact(testUser, cid, upstream.Contact{FirstName: "عبدالله", LastName: "القحطاني", Email1: "abdullah@example.test"})
	a := e.login()
	f := e.contactFolder(a, store.DefaultBookID)
	e.waitCards(a, f.ID, 2)
	// Provoke the failure paths too.
	e.srv.FailNext("SyncContacts", errors.New("boom"), 1)
	e.srv.ResetContactFolder(testUser, cid)
	time.Sleep(300 * time.Millisecond)
	logs := e.logs.String()
	for _, secret := range []string{"Noorhan", "Alsulami", "noorhan.private", "+1 555", "Hidden Street", "Jeddah", "Tower B", "Secret Clients", "عبدالله", "القحطاني", "abdullah@", "BEGIN:VCARD", "VCARD"} {
		if contains(logs, secret) {
			t.Errorf("the log contains %q", secret)
		}
	}
}

func TestWipeDeletesContactsDirectoryCardsAndCandidatesEvenIfTheDatabaseSurvives(t *testing.T) {
	e := newEnv(t, withContacts(func(c *engine.Config, _ *fakeupstream.Server) {
		c.Directory = engine.DirectoryConfig{Domains: []string{"corp.example"}, Spacing: time.Millisecond, Poll: 5 * time.Millisecond}
	}))
	cid := e.srv.AddContactFolder(testUser, "Contacts", fakeupstream.TypeContacts)
	e.srv.AddContact(testUser, cid, person("Ada", "L", "ada@example.test"))
	e.srv.AddGALPerson(fakeupstream.GALPerson{DirectoryEntry: upstream.DirectoryEntry{DisplayName: "Pat Smith", EmailAddress: "pat@corp.example"}})
	inbox := e.srv.FolderID(testUser, "Inbox")
	e.srv.AddMessage(testUser, inbox, mailBytes("m1", "hi", e.clock.Now(), "Cc: pat@corp.example\r\n"), fakeupstream.AddOptions{})
	// The wipe must not rely on destroying the database to remove personal data.
	e.mgr.SetDestroyHook(func(*store.DB) error { return errors.New("injected: the files cannot be deleted") })
	a := e.login()
	f := e.contactFolder(a, store.DefaultBookID)
	e.waitCards(a, f.ID, 1)
	eventually(t, "a directory card", func() bool {
		n, _ := a.Store().DirectoryCardCount()
		return n == 1
	})
	db := e.mgr.DB()
	e.srv.RequestWipe(testUser)
	eventually(t, "WIPED marker written", func() bool { return store.WipeMarkerExists(e.dir) })
	eventually(t, "wipe acknowledged", func() bool { return e.srv.WipeAcknowledged(testUser) })
	eventually(t, "contact folders, cards, directory cards, candidates and bookkeeping are gone", func() bool {
		acct, err := db.Account(testUser)
		if err != nil {
			return false
		}
		fs, _ := acct.ContactFolders()
		cards, _ := acct.DirectoryCards()
		cands, _ := acct.Candidates()
		st, _ := acct.DirectoryState()
		return len(fs)+len(cards)+len(cands) == 0 && st.LastSearchMs == 0 && st.CTag == "0-0"
	})
}

func TestAccountOnlyWipeDeletesContactsToo(t *testing.T) {
	e := newEnv(t, withContacts())
	cid := e.srv.AddContactFolder(testUser, "Contacts", fakeupstream.TypeContacts)
	e.srv.AddContact(testUser, cid, person("Ada", "L", "ada@example.test"))
	e.mgr.SetDestroyHook(func(*store.DB) error { return errors.New("injected") })
	a := e.login()
	f := e.contactFolder(a, store.DefaultBookID)
	e.waitCards(a, f.ID, 1)
	db := e.mgr.DB()
	e.srv.RequestAccountOnlyWipe(testUser)
	eventually(t, "wipe acknowledged", func() bool { return e.srv.WipeAcknowledged(testUser) })
	eventually(t, "contacts gone", func() bool {
		acct, _ := db.Account(testUser)
		fs, _ := acct.ContactFolders()
		return len(fs) == 0
	})
}
