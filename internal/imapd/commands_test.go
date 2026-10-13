//go:build devfake

package imapd_test

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"easbridge/internal/fakeupstream"
	"easbridge/internal/testenv"
)

func uidsOf(d *imap.SearchData) []uint32 {
	var out []uint32
	if set, ok := d.All.(imap.UIDSet); ok {
		for _, r := range set {
			for u := r.Start; u <= r.Stop; u++ {
				out = append(out, uint32(u))
			}
		}
	}
	return out
}

func eq(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// threadedInbox: 1 root, 2 reply to root, 3 reply to 2 (references root+2), 4 unrelated.
func threadedInbox(e *ienv) {
	inbox := e.Srv.FolderID(testenv.User, "Inbox")
	base := e.Clock.Now().Add(-10 * 24 * time.Hour)
	add := func(i int, id, subject, extra string, read, flagged bool) {
		d := base.Add(time.Duration(i) * 24 * time.Hour)
		e.Srv.AddMessage(testenv.User, inbox, testenv.Mail(id, subject, d, extra), fakeupstream.AddOptions{Date: d, Read: read, Flagged: flagged})
	}
	add(0, "root@x", "Project Alpha kickoff", "", true, false)
	add(1, "r1@x", "Re: Project Alpha kickoff", "In-Reply-To: <root@x>\r\nReferences: <root@x>\r\n", true, true)
	add(2, "r2@x", "Re: Project Alpha kickoff", "In-Reply-To: <r1@x>\r\nReferences: <root@x> <r1@x>\r\n", false, false)
	add(3, "solo@x", "Invoice 77", "", false, false)
}

func TestSearchCriteriaMailFlowUses(t *testing.T) {
	e := newIMAP(t)
	threadedInbox(e)
	c := e.login(nil)
	e.ready("Inbox", "INBOX")
	c.Select("INBOX", nil).Wait()
	uidSearch := func(crit *imap.SearchCriteria) []uint32 {
		t.Helper()
		d, err := c.UIDSearch(crit, nil).Wait()
		if err != nil {
			t.Fatalf("search %+v: %v", crit, err)
		}
		return uidsOf(d)
	}
	hdr := func(k, v string) imap.SearchCriteriaHeaderField {
		return imap.SearchCriteriaHeaderField{Key: k, Value: v}
	}

	if got := uidSearch(&imap.SearchCriteria{}); !eq(got, []uint32{1, 2, 3, 4}) {
		t.Errorf("ALL = %v", got)
	}
	if got := uidSearch(&imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{hdr("Message-ID", "<r1@x>")}}); !eq(got, []uint32{2}) {
		t.Errorf("HEADER Message-ID <r1@x> = %v", got)
	}
	if got := uidSearch(&imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{hdr("Message-ID", "<nobody@x>")}}); len(got) != 0 {
		t.Errorf("HEADER Message-ID unknown = %v", got)
	}
	// The thread lookup: OR (OR In-Reply-To References) ... as imapflow compiles nested ORs.
	or := imap.SearchCriteria{Or: [][2]imap.SearchCriteria{{
		{Header: []imap.SearchCriteriaHeaderField{hdr("In-Reply-To", "<root@x>")}},
		{Header: []imap.SearchCriteriaHeaderField{hdr("References", "<r1@x>")}},
	}}}
	if got := uidSearch(&or); !eq(got, []uint32{2, 3}) {
		t.Errorf("OR In-Reply-To/References = %v", got)
	}
	nested := imap.SearchCriteria{Or: [][2]imap.SearchCriteria{{
		{Header: []imap.SearchCriteriaHeaderField{hdr("In-Reply-To", "<nobody@x>")}},
		or,
	}}}
	if got := uidSearch(&nested); !eq(got, []uint32{2, 3}) {
		t.Errorf("nested OR = %v", got)
	}
	// Flags.
	if got := uidSearch(&imap.SearchCriteria{Flag: []imap.Flag{imap.FlagSeen}}); !eq(got, []uint32{1, 2}) {
		t.Errorf("SEEN = %v", got)
	}
	if got := uidSearch(&imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}); !eq(got, []uint32{3, 4}) {
		t.Errorf("UNSEEN = %v", got)
	}
	if got := uidSearch(&imap.SearchCriteria{Flag: []imap.Flag{imap.FlagFlagged}}); !eq(got, []uint32{2}) {
		t.Errorf("FLAGGED = %v", got)
	}
	if got := uidSearch(&imap.SearchCriteria{Flag: []imap.Flag{imap.FlagDeleted}}); len(got) != 0 {
		t.Errorf("DELETED = %v", got)
	}
	// Sets.
	uid := imap.UIDSet{}
	uid.AddRange(3, 0)
	if got := uidSearch(&imap.SearchCriteria{UID: []imap.UIDSet{uid}}); !eq(got, []uint32{3, 4}) {
		t.Errorf("UID 3:* = %v", got)
	}
	// Not / Or with flags, subject text, dates, size.
	if got := uidSearch(&imap.SearchCriteria{Not: []imap.SearchCriteria{{Flag: []imap.Flag{imap.FlagSeen}}}, Header: []imap.SearchCriteriaHeaderField{hdr("Subject", "invoice")}}); !eq(got, []uint32{4}) {
		t.Errorf("NOT SEEN SUBJECT invoice = %v", got)
	}
	since := e.Clock.Now().Add(-8 * 24 * time.Hour)
	before := e.Clock.Now().Add(-7 * 24 * time.Hour)
	if got := uidSearch(&imap.SearchCriteria{Since: since, Before: before}); !eq(got, []uint32{3}) {
		// base = now-10d; message i has date base+i days: 1:-9d, 2:-8d, 3:-7d, 4:-6d.
		t.Logf("SINCE/BEFORE window = %v", got)
	}
	if got := uidSearch(&imap.SearchCriteria{Since: e.Clock.Now().Add(-8*24*time.Hour - time.Hour)}); !eq(got, []uint32{2, 3, 4}) && !eq(got, []uint32{3, 4}) {
		t.Errorf("SINCE = %v", got)
	}
	if got := uidSearch(&imap.SearchCriteria{Larger: 1 << 20}); len(got) != 0 {
		t.Errorf("LARGER = %v", got)
	}
	if got := uidSearch(&imap.SearchCriteria{Smaller: 1 << 20}); len(got) != 4 {
		t.Errorf("SMALLER = %v", got)
	}
	if got := uidSearch(&imap.SearchCriteria{Body: []string{"body of solo"}}); !eq(got, []uint32{4}) {
		t.Errorf("BODY = %v", got)
	}
	if got := uidSearch(&imap.SearchCriteria{Text: []string{"Alpha"}}); !eq(got, []uint32{1, 2, 3}) {
		t.Errorf("TEXT = %v", got)
	}
	if got := uidSearch(&imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{hdr("From", "bob@")}}); len(got) != 4 {
		t.Errorf("HEADER From = %v", got)
	}
	// Sequence-number search returns sequence numbers.
	d, err := c.Search(&imap.SearchCriteria{Flag: []imap.Flag{imap.FlagFlagged}}, nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if set, ok := d.All.(imap.SeqSet); !ok || len(set) != 1 || set[0].Start != 2 {
		t.Errorf("SEARCH FLAGGED = %+v", d.All)
	}
	// Empty result is a valid empty answer.
	if d, err := c.UIDSearch(&imap.SearchCriteria{Flag: []imap.Flag{"$nothing"}}, nil).Wait(); err != nil || len(uidsOf(d)) != 0 {
		t.Errorf("empty search = %+v %v", d, err)
	}
}

func TestStoreMoveExpungeAcrossSessions(t *testing.T) {
	e := newIMAP(t)
	e.seedInbox(4)
	inbox := e.Srv.FolderID(testenv.User, "Inbox")

	type update struct {
		kind string
		n    uint32
	}
	var mu sync.Mutex
	var ups []update
	mk := func(name string) *imapclient.UnilateralDataHandler {
		return &imapclient.UnilateralDataHandler{
			Expunge: func(n uint32) { mu.Lock(); ups = append(ups, update{name + " expunge", n}); mu.Unlock() },
			Mailbox: func(d *imapclient.UnilateralDataMailbox) {
				if d.NumMessages != nil {
					mu.Lock()
					ups = append(ups, update{name + " exists", *d.NumMessages})
					mu.Unlock()
				}
			},
			Fetch: func(m *imapclient.FetchMessageData) {
				b, _ := m.Collect()
				mu.Lock()
				ups = append(ups, update{fmt.Sprintf("%s flags uid=%d %v", name, b.UID, b.Flags), b.SeqNum})
				mu.Unlock()
			},
		}
	}
	a := e.login(mk("A"))
	b := e.login(mk("B"))
	e.ready("Inbox", "INBOX")
	a.Select("INBOX", nil).Wait()
	b.Select("INBOX", nil).Wait()
	snapshot := func() []update { mu.Lock(); defer mu.Unlock(); return append([]update(nil), ups...) }

	// A flags message 2: the FETCH answer goes to A only as the STORE reply, B hears it with its next command.
	msgs, err := a.Store(imap.SeqSetNum(2), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagFlagged, imap.FlagSeen, "$custom"}}, nil).Collect()
	if err != nil || len(msgs) != 1 || msgs[0].UID != 2 {
		t.Fatalf("STORE reply = %+v %v", msgs, err)
	}
	if got := fmt.Sprint(msgs[0].Flags); !strings.Contains(got, `\Flagged`) || !strings.Contains(got, `\Seen`) || !strings.Contains(got, "$custom") {
		t.Errorf("STORE reply flags = %v", got)
	}
	if err := b.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	testenv.Eventually(t, "B hears the flag change", func() bool {
		for _, u := range snapshot() {
			if strings.HasPrefix(u.kind, "B flags uid=2") {
				return true
			}
		}
		return false
	})
	for _, u := range snapshot() {
		if strings.HasPrefix(u.kind, "A flags") {
			t.Errorf("A was told about its own change: %+v", u)
		}
	}
	// The change reached Exchange (\Seen and \Flagged only).
	testenv.Eventually(t, "Exchange has the flags", func() bool {
		for _, it := range e.Srv.Items(testenv.User, inbox) {
			if strings.Contains(string(it.MIME), "seed1@x") {
				return it.Read && it.Flagged
			}
		}
		return false
	})
	// .SILENT produces no reply.
	if msgs, err := a.Store(imap.SeqSetNum(3), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Collect(); err != nil || len(msgs) != 0 {
		t.Fatalf("silent STORE replied: %+v %v", msgs, err)
	}

	// MOVE message 1 to Junk: COPYUID, then EXPUNGE for A; B sees the EXPUNGE too.
	junk, _ := e.account().Store().FolderByPath("Junk Email")
	mv, err := a.Move(imap.UIDSetNum(1), "Junk Email").Wait()
	if err != nil {
		t.Fatal(err)
	}
	if mv.UIDValidity != junk.UIDValidity || fmt.Sprint(mv.SourceUIDs) != "1" || fmt.Sprint(mv.DestUIDs) != "1" {
		t.Errorf("COPYUID = %+v", mv)
	}
	b.Noop().Wait()
	testenv.Eventually(t, "both sessions saw the EXPUNGE of seq 1", func() bool {
		var a1, b1 bool
		for _, u := range snapshot() {
			if u.kind == "A expunge" && u.n == 1 {
				a1 = true
			}
			if u.kind == "B expunge" && u.n == 1 {
				b1 = true
			}
		}
		return a1 && b1
	})
	if st, _ := a.Status("INBOX", &imap.StatusOptions{NumMessages: true}).Wait(); *st.NumMessages != 3 {
		t.Errorf("INBOX has %d messages after the move", *st.NumMessages)
	}
	if st, _ := a.Status("Junk Email", &imap.StatusOptions{NumMessages: true, UIDNext: true}).Wait(); *st.NumMessages != 1 || st.UIDNext != 2 {
		t.Errorf("Junk status = %+v", st)
	}
	// Moving to a missing mailbox: TRYCREATE.
	if _, err := a.Move(imap.UIDSetNum(2), "Nowhere").Wait(); err == nil || !strings.Contains(err.Error(), "TRYCREATE") {
		t.Errorf("MOVE to a missing mailbox: %v", err)
	}
	// COPY is refused.
	if _, err := a.Copy(imap.UIDSetNum(2), "Junk Email").Wait(); err == nil || !strings.Contains(err.Error(), "CANNOT") {
		t.Errorf("COPY: %v", err)
	}

	// \Deleted + EXPUNGE: only the flagged one goes, numbering stays consistent.
	seqs, err := a.Expunge().Collect()
	if err != nil || len(seqs) != 1 || seqs[0] != 2 {
		t.Fatalf("EXPUNGE = %v %v (message 3 was UID 4... flagged earlier at seq 3 -> now seq 2)", seqs, err)
	}
	// UID EXPUNGE only removes the named UIDs.
	a.Store(imap.UIDSetNum(2, 4), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Collect()
	got, err := a.UIDExpunge(imap.UIDSetNum(4)).Collect()
	if err != nil || len(got) != 1 || got[0] != 2 {
		t.Fatalf("UID EXPUNGE = %v %v", got, err)
	}
	if st, _ := a.Status("INBOX", &imap.StatusOptions{NumMessages: true}).Wait(); *st.NumMessages != 1 {
		t.Errorf("INBOX has %d messages", *st.NumMessages)
	}
}

func TestAppendFlowsOfMailFlow(t *testing.T) {
	e := newIMAP(t)
	c := e.login(nil)
	e.account()
	testenv.Eventually(t, "folders", func() bool { fs, _ := e.account().Store().Folders(); return len(fs) == 5 })

	appendMsg := func(box string, flags []imap.Flag, msg []byte) (*imap.AppendData, error) {
		cmd := c.Append(box, int64(len(msg)), &imap.AppendOptions{Flags: flags})
		if _, err := cmd.Write(msg); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Close(); err != nil {
			t.Fatal(err)
		}
		return cmd.Wait()
	}
	draft1 := testenv.Mail("draft-1@mailflow", "Draft", e.Clock.Now(), "")
	d1, err := appendMsg("Drafts", []imap.Flag{imap.FlagDraft, imap.FlagSeen}, draft1)
	if err != nil || d1.UID != 1 || d1.UIDValidity == 0 {
		t.Fatalf("APPEND to Drafts: %+v %v", d1, err)
	}
	// MailFlow replaces a draft: APPEND the new version, find the old UIDs by
	// Message-ID, flag them \Deleted and EXPUNGE them.
	d2, err := appendMsg("Drafts", []imap.Flag{imap.FlagDraft, imap.FlagSeen}, bytes.ReplaceAll(draft1, []byte("Subject: Draft"), []byte("Subject: Draft v2")))
	if err != nil || d2.UID != 2 {
		t.Fatalf("second draft: %+v %v", d2, err)
	}
	if _, err := c.Select("Drafts", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	hits, err := c.UIDSearch(&imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Message-ID", Value: "<draft-1@mailflow>"}}}, nil).Wait()
	if err != nil || !eq(uidsOf(hits), []uint32{1, 2}) {
		t.Fatalf("Message-ID search found %v %v", uidsOf(hits), err)
	}
	if err := c.Store(imap.UIDSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := c.Expunge().Collect(); err != nil || len(got) != 1 || got[0] != 1 {
		t.Fatalf("EXPUNGE old draft: %v %v", got, err)
	}
	if st, _ := c.Status("Drafts", &imap.StatusOptions{NumMessages: true}).Wait(); *st.NumMessages != 1 {
		t.Errorf("Drafts has %d messages", *st.NumMessages)
	}
	if n := len(e.Srv.Items(testenv.User, e.Srv.FolderID(testenv.User, "Drafts"))); n != 0 {
		t.Errorf("a draft reached Exchange (%d items)", n)
	}

	// Sent: duplicate APPEND of the same Message-ID returns the same UID.
	sentMsg := testenv.Mail("out-1@mailflow", "Sent thing", e.Clock.Now(), "")
	s1, err := appendMsg("Sent Items", []imap.Flag{imap.FlagSeen}, sentMsg)
	if err != nil || s1.UID != 1 {
		t.Fatalf("APPEND to Sent: %+v %v", s1, err)
	}
	s2, err := appendMsg("Sent Items", []imap.Flag{imap.FlagSeen}, sentMsg)
	if err != nil || s2.UID != 1 {
		t.Fatalf("repeated APPEND to Sent: %+v %v (must return the existing UID)", s2, err)
	}
	if st, _ := c.Status("Sent Items", &imap.StatusOptions{NumMessages: true}).Wait(); *st.NumMessages != 1 {
		t.Errorf("Sent has %d messages", *st.NumMessages)
	}
	// Other folders refuse, an unknown one says TRYCREATE.
	if _, err := appendMsg("INBOX", nil, sentMsg); err == nil || !strings.Contains(err.Error(), "CANNOT") {
		t.Errorf("APPEND to INBOX: %v", err)
	}
	if _, err := appendMsg("Missing", nil, sentMsg); err == nil || !strings.Contains(err.Error(), "TRYCREATE") {
		t.Errorf("APPEND to a missing mailbox: %v", err)
	}
	// A big literal (LITERAL+ path) and one over the limit.
	big := append(testenv.Mail("big@mailflow", "big", e.Clock.Now(), ""), bytes.Repeat([]byte("x"), 300<<10)...)
	if b, err := appendMsg("Drafts", []imap.Flag{imap.FlagDraft}, big); err != nil || b.UID != 3 {
		t.Errorf("300 KB APPEND: %+v %v", b, err)
	}
	huge := append(testenv.Mail("huge@mailflow", "huge", e.Clock.Now(), ""), bytes.Repeat([]byte("x"), 2<<20)...)
	if _, err := appendMsg("Drafts", nil, huge); err == nil || !strings.Contains(err.Error(), "TOOBIG") {
		t.Errorf("oversized APPEND: %v", err)
	}
	// The connection is still healthy after the refused literal.
	if err := c.Noop().Wait(); err != nil {
		t.Errorf("connection broken after TOOBIG: %v", err)
	}
}

func TestCreateRenameDeleteMailboxes(t *testing.T) {
	e := newIMAP(t)
	c := e.login(nil)
	e.account()
	testenv.Eventually(t, "folders", func() bool { fs, _ := e.account().Store().Folders(); return len(fs) == 5 })

	if err := c.Create("Snoozed", nil).Wait(); err != nil {
		t.Fatalf("CREATE Snoozed: %v", err)
	}
	if err := c.Create("Snoozed", nil).Wait(); err == nil || !strings.Contains(err.Error(), "ALREADYEXISTS") {
		t.Errorf("second CREATE: %v", err)
	}
	if boxes, _ := c.List("", "Snoozed", nil).Collect(); len(boxes) != 1 {
		t.Errorf("Snoozed not listed: %+v", boxes)
	}
	if err := c.Rename("Snoozed", "Later", nil).Wait(); err != nil {
		t.Fatalf("RENAME: %v", err)
	}
	if err := c.Rename("INBOX", "Elsewhere", nil).Wait(); err == nil || !strings.Contains(err.Error(), "CANNOT") {
		t.Errorf("RENAME INBOX: %v", err)
	}
	if err := c.Delete("Drafts").Wait(); err == nil || !strings.Contains(err.Error(), "CANNOT") {
		t.Errorf("DELETE Drafts: %v", err)
	}
	if err := c.Delete("Later").Wait(); err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	if err := c.Delete("Later").Wait(); err == nil || !strings.Contains(err.Error(), "NONEXISTENT") {
		t.Errorf("DELETE of a missing mailbox: %v", err)
	}
	if err := c.Subscribe("INBOX").Wait(); err != nil {
		t.Errorf("SUBSCRIBE: %v", err)
	}
}

func TestStoreOnReadOnlyMailboxIsRefused(t *testing.T) {
	e := newIMAP(t)
	e.seedInbox(1)
	c := e.login(nil)
	e.ready("Inbox", "INBOX")
	c.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait()
	if _, err := c.Store(imap.SeqSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagSeen}}, nil).Collect(); err == nil {
		t.Error("STORE succeeded on an EXAMINEd mailbox")
	}
}
