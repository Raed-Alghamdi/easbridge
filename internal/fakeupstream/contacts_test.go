//go:build devfake

package fakeupstream

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"easbridge/internal/upstream"
)

func contactsMailbox(t *testing.T) (*Server, *Mailbox, string) {
	t.Helper()
	s := NewServer()
	s.AddUser("a@x.test", "pw")
	fid := s.AddContactFolder("a@x.test", "Contacts", TypeContacts)
	mb, err := s.Factory(0).New(upstream.Config{User: "a@x.test", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	return s, mb.(*Mailbox), fid
}

func TestContactsSyncWindowsAndKeys(t *testing.T) {
	s, m, fid := contactsMailbox(t)
	s.Window = 2
	for i := 0; i < 5; i++ {
		s.AddContact("a@x.test", fid, upstream.Contact{FirstName: fmt.Sprint("N", i)})
	}
	ctx := context.Background()
	d, err := m.SyncContacts(ctx, fid, "0")
	if err != nil || len(d.Added) != 0 || d.NewKey == "" || d.Reset {
		t.Fatalf("bootstrap %+v %v", d, err)
	}
	seen := 0
	key := d.NewKey
	for i := 0; i < 5; i++ {
		d, err = m.SyncContacts(ctx, fid, key)
		if err != nil {
			t.Fatal(err)
		}
		seen += len(d.Added)
		key = d.NewKey
		if !d.MoreAvailable {
			break
		}
	}
	if seen != 5 {
		t.Errorf("saw %d of 5 contacts", seen)
	}
	// Nothing new, then a change and a delete.
	d, _ = m.SyncContacts(ctx, fid, key)
	if len(d.Added)+len(d.Changed)+len(d.Deleted) != 0 {
		t.Errorf("a quiet folder: %+v", d)
	}
	key = d.NewKey
	cs := s.Contacts("a@x.test", fid)
	s.UpdateContact("a@x.test", fid, cs[0].ServerID, upstream.Contact{FirstName: "Changed"})
	s.DeleteContact("a@x.test", fid, cs[1].ServerID)
	d, _ = m.SyncContacts(ctx, fid, key)
	if len(d.Changed) != 1 || d.Changed[0].FirstName != "Changed" || len(d.Deleted) != 1 || d.Deleted[0] != cs[1].ServerID {
		t.Errorf("%+v", d)
	}
	// An unknown or invalidated key restarts from 0.
	if d, _ = m.SyncContacts(ctx, fid, "nonsense"); !d.Reset || len(d.Added) != 0 {
		t.Errorf("unknown key: %+v", d)
	}
	key = d.NewKey
	s.ResetContactFolder("a@x.test", fid)
	if d, _ = m.SyncContacts(ctx, fid, key); !d.Reset {
		t.Errorf("a reset folder rejects the keys it issued: %+v", d)
	}
	// Injected failures and counting.
	s.FailNext("SyncContacts", errors.New("boom"), 1)
	if _, err := m.SyncContacts(ctx, fid, "0"); err == nil {
		t.Errorf("no failure")
	}
	if s.Calls("SyncContacts") < 8 {
		t.Errorf("calls %d", s.Calls("SyncContacts"))
	}
}

func TestGALSearchMatchesPrefixesAndRecordsEverything(t *testing.T) {
	s, m, _ := contactsMailbox(t)
	for _, p := range []GALPerson{
		{DirectoryEntry: upstream.DirectoryEntry{DisplayName: "Pat Smith", EmailAddress: "pat@corp.example", FirstName: "Pat", LastName: "Smith"}, Alias: "psmith"},
		{DirectoryEntry: upstream.DirectoryEntry{DisplayName: "Patricia Jones", EmailAddress: "patricia@corp.example"}},
		{DirectoryEntry: upstream.DirectoryEntry{DisplayName: "Lee Wong", EmailAddress: "lee@corp.example"}},
	} {
		s.AddGALPerson(p)
	}
	ctx := context.Background()
	search := func(q string) []string {
		res, err := m.SearchDirectory(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range res.Entries {
			out = append(out, e.EmailAddress)
		}
		return out
	}
	if got := strings.Join(search("pat@corp.example"), ","); got != "pat@corp.example" {
		t.Errorf("full address: %s", got)
	}
	if got := strings.Join(search("pat"), ","); got != "pat@corp.example,patricia@corp.example" {
		t.Errorf("local part and names: %s", got)
	}
	if got := search("Wong"); len(got) != 1 {
		t.Errorf("last word of the display name: %v", got)
	}
	s.GALAddressSearch = false
	if got := search("pat@corp.example"); len(got) != 0 {
		t.Errorf("address search off: %v", got)
	}
	s.GALAddressSearch, s.GALNoLocalPartSearch = true, true
	if got := search("lee"); len(got) != 1 { // still found by first word of the display name
		t.Errorf("%v", got)
	}
	if got := search("psm"); len(got) != 1 {
		t.Errorf("alias: %v", got)
	}
	if got := strings.Join(s.GALQueries(), "|"); got != "pat@corp.example|pat|Wong|pat@corp.example|lee|psm" {
		t.Errorf("queries %s", got)
	}
	if len(s.GALViolations()) != 0 || len(s.GALSearchTimes()) != 6 {
		t.Errorf("violations %v", s.GALViolations())
	}
	// Enumeration attempts are noticed.
	for _, q := range []string{"*", "a", "pa%", ""} {
		m.SearchDirectory(ctx, q)
	}
	if len(s.GALViolations()) != 4 {
		t.Errorf("violations %q", s.GALViolations())
	}
	// A hundred entries at most, and Total says how many matched.
	for i := 0; i < 130; i++ {
		s.AddGALPerson(GALPerson{DirectoryEntry: upstream.DirectoryEntry{DisplayName: fmt.Sprint("Zed ", i), EmailAddress: fmt.Sprintf("z%d@corp.example", i)}})
	}
	res, _ := m.SearchDirectory(ctx, "zed")
	if res.Total != 130 || len(res.Entries) != 100 {
		t.Errorf("total %d, entries %d", res.Total, len(res.Entries))
	}
	s.RemoveGALPerson("PAT@corp.example")
	if got := search("psm"); len(got) != 0 {
		t.Errorf("removed: %v", got)
	}
	// Failures are injected by operation and count as attempts.
	s.FailNext("SearchDirectory", &upstream.StatusError{Op: "Search", Code: 5, Perm: true}, 1)
	if _, err := m.SearchDirectory(ctx, "lee"); !upstream.DirectoryDenied(err) {
		t.Errorf("%v", err)
	}
}

func TestPingedFoldersAreRecorded(t *testing.T) {
	s, m, fid := contactsMailbox(t)
	inbox := s.FolderID("a@x.test", "Inbox")
	if _, err := m.FolderSync(context.Background(), "0"); err != nil {
		t.Fatal(err)
	}
	if res, err := m.Ping(context.Background(), 10_000_000, []string{inbox}); err != nil || res.Status != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if p := s.PingedFolders(); !p[inbox] || p[fid] {
		t.Errorf("pinged %v", p)
	}
}
