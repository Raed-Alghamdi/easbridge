//go:build devfake

package engine_test

import (
	"bytes"
	"fmt"
	"net/mail"
	"strings"
	"testing"
	"time"

	"easbridge/internal/engine"
	"easbridge/internal/fakeupstream"
	"easbridge/internal/store"
	"easbridge/internal/upstream"
)

// Data-integrity findings of the security review of v0.1.0 (LOW 11).

// (a) A message stored while a full resync of its folder is being collected has
// been seen by definition. It used to keep recon_mark 0 and, when the epoch
// ended, count as a message the server had forgotten: a strike, then a second
// complete resync of the folder to confirm it.
func TestAMessageStoredDuringAResyncIsNotSuspected(t *testing.T) {
	e := newEnv(t, func(c *engine.Config, s *fakeupstream.Server) { s.Window = 2 })
	inbox := e.srv.FolderID(testUser, "Inbox")
	for i := 0; i < 4; i++ {
		d := e.clock.Now().Add(-time.Duration(10-i) * time.Hour)
		e.srv.AddMessage(testUser, inbox, mailBytes(fmt.Sprintf("old%d@x", i), "old", d, ""), fakeupstream.AddOptions{Date: d})
	}
	a := e.login()
	f := e.mirrored(a, "Inbox", "INBOX")

	// A large message: announced by Sync, its body fetched separately. Its fetch
	// is held, so that it completes in the middle of the resync below.
	e.srv.HoldAfter("FetchMIME", testUser, inbox, 0)
	e.srv.AddMessage(testUser, inbox, bigMail("big@x", "big", e.clock.Now()), fakeupstream.AddOptions{Date: e.clock.Now()})
	eventually(t, "the large message waits for its body", func() bool {
		n, _ := a.Store().PendingCount(f.ID)
		return n == 1 && e.srv.Calls("FetchMIME held") >= 1
	})

	// The server forgets its sync state. The resync lists the newest item (the
	// large one) first; its second window is held.
	e.srv.HoldAfter("Sync", testUser, inbox, 2)
	e.srv.ResetFolder(testUser, inbox)
	eventually(t, "the resync is under way and held", func() bool { return e.srv.Calls("Sync held") >= 1 })
	cur, _ := a.Store().FolderByID(f.ID)
	if !cur.ReconActive {
		t.Fatalf("no resync in progress: %+v", cur)
	}

	// The body arrives now: the message is stored in the middle of the resync.
	e.srv.Release("FetchMIME", testUser, inbox)
	eventually(t, "the large message is stored", func() bool { return serverCount(a, f.ID) == 5 })

	e.srv.Release("Sync", testUser, inbox)
	eventually(t, "the resync ends", func() bool {
		c, _ := a.Store().FolderByID(f.ID)
		return c != nil && !c.ReconActive
	})
	c, _ := a.Store().FolderByID(f.ID)
	if c.ReconVerifyAt != 0 {
		t.Errorf("a verification resync was scheduled for a message that arrived during the resync")
	}
	if !strings.Contains(e.logs.String(), "removed=0 suspected=0") {
		t.Errorf("the resync suspected a message:\n%s", e.logs.String())
	}
	if n := serverCount(a, f.ID); n != 5 {
		t.Errorf("%d messages, want 5", n)
	}
}

// (b) A delete Exchange refuses used to be forgotten: the message had left the
// local folder and never came back, although it was still on the server.
func TestRefusedDeleteBringsTheMessageBack(t *testing.T) {
	e, a, f, _ := seeded(t, 3)
	rec, _ := e.watch(a, f)
	if _, err := a.SetFlags(f.ID, []uint32{2}, engine.FlagAdd, store.FlagSeen|store.FlagFlagged, nil, "s"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the flags reach Exchange", func() bool {
		n, _ := a.Store().OutboxLen()
		return n == 0
	})
	before, _ := a.Store().Message(f.ID, 2)

	e.srv.RejectDeletesStatus = 6 // Exchange refuses every delete
	if _, err := a.SetFlags(f.ID, []uint32{2}, engine.FlagAdd, store.FlagDeleted, nil, "s"); err != nil {
		t.Fatal(err)
	}
	if gone, err := a.Expunge(f.ID, nil, "s"); err != nil || len(gone) != 1 || gone[0] != 2 {
		t.Fatalf("expunge: %v %v", gone, err)
	}
	// It comes back, as a new arrival with a new UID, with the flags it had.
	eventually(t, "the message is back", func() bool {
		uids, _ := a.Store().UIDsByMessageID(f.ID, "s1@x")
		return len(uids) == 1
	})
	uids, _ := a.Store().UIDsByMessageID(f.ID, "s1@x")
	if uids[0] == 2 {
		t.Errorf("the UID %d was reused", uids[0])
	}
	back, _ := a.Store().Message(f.ID, uids[0])
	if back.Subject != before.Subject || !back.Flags.Has(store.FlagSeen) || !back.Flags.Has(store.FlagFlagged) || back.Flags.Has(store.FlagDeleted) {
		t.Errorf("restored message: subject %q flags %v (was %q %v)", back.Subject, back.Flags, before.Subject, before.Flags)
	}
	if !back.InternalDate.Equal(before.InternalDate) {
		t.Errorf("received date %v, was %v", back.InternalDate, before.InternalDate)
	}
	if n, _ := a.Store().OutboxLen(); n != 0 {
		t.Errorf("the refused delete is still queued: %d", n)
	}
	if rec.count(store.EvExpunge) != 1 || rec.count(store.EvAdd) != 1 {
		t.Errorf("events: %d expunge, %d add; want 1 and 1", rec.count(store.EvExpunge), rec.count(store.EvAdd))
	}
	if n, _ := a.Store().PendingCount(0); n != 0 {
		t.Errorf("%d downloads left over", n)
	}
	if serverCount(a, f.ID) != 3 {
		t.Errorf("the folder has %d messages, want 3", serverCount(a, f.ID))
	}
}

func TestADeleteQueuedByAnOlderVersionIsDroppedWhenRefused(t *testing.T) {
	// No record of flags and date was kept for it (restore_date 0): nothing to
	// restore from, so the refused delete is simply forgotten, as before.
	e, a, f, ids := seeded(t, 2)
	e.srv.RejectDeletesStatus = 6
	if _, err := a.Store().Update(nil, func(tx *store.Tx) error {
		m, err := tx.Message(f.ID, 1)
		if err != nil {
			return err
		}
		if err := tx.EnqueueOp(f.ID, m.ServerID, store.OpDelete, true, false, -1); err != nil {
			return err
		}
		_, err = tx.DeleteMessage(f.ID, 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the refused delete leaves the outbox", func() bool {
		n, _ := a.Store().OutboxLen()
		return n == 0
	})
	time.Sleep(100 * time.Millisecond)
	if n, _ := a.Store().PendingCount(0); n != 0 {
		t.Errorf("a download was queued without a record to restore from")
	}
	_ = ids
}

// (c) An item with a delete waiting in the outbox is still listed by the server.
// A resync, or a replayed batch, used to bring it back as a new message.
func TestAServerAddForAnItemWithAQueuedDeleteDoesNotResurrectIt(t *testing.T) {
	e, a, f, _ := seeded(t, 3)
	inbox := e.srv.FolderID(testUser, "Inbox")
	rec, _ := e.watch(a, f)
	e.srv.FailNext("ApplyChanges", upstream.ErrUnavailable, -1) // the delete cannot reach Exchange
	if _, err := a.SetFlags(f.ID, []uint32{2}, engine.FlagAdd, store.FlagDeleted, nil, "s"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Expunge(f.ID, nil, "s"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the delete is tried", func() bool { return e.srv.Calls("ApplyChanges") >= 1 })

	// The server forgets its sync state and lists everything again, the item
	// included (it is still there).
	e.srv.ResetFolder(testUser, inbox)
	eventually(t, "the resync ends", func() bool {
		cur, _ := a.Store().FolderByID(f.ID)
		return cur != nil && !cur.ReconActive && cur.ReconEpoch > 0
	})
	consistently(t, "the deleted message stays gone", 300*time.Millisecond, func() bool {
		n, _ := a.Store().PendingCount(0)
		return serverCount(a, f.ID) == 2 && n == 0
	})
	if rec.count(store.EvAdd) != 0 {
		t.Errorf("the message came back as a new arrival: %+v", rec.all())
	}

	// Once Exchange takes the delete, the item leaves the Inbox for good.
	e.srv.ClearFailures()
	eventually(t, "the delete reaches Exchange", func() bool {
		n, _ := a.Store().OutboxLen()
		return n == 0
	})
	if len(e.srv.Items(testUser, inbox)) != 2 {
		t.Errorf("the server still has %d Inbox items", len(e.srv.Items(testUser, inbox)))
	}
	consistently(t, "and stays gone", 200*time.Millisecond, func() bool { return serverCount(a, f.ID) == 2 })
}

// (d) An item Exchange keeps failing to give as MIME used to stay invisible for
// ever. After the retry budget a small stand-in with the listed subject, sender
// and date takes its place.
func TestAnItemExchangeCannotRenderBecomesAStandIn(t *testing.T) {
	e := newEnv(t)
	inbox := e.srv.FolderID(testUser, "Inbox")
	d := e.clock.Now().Add(-3 * time.Hour)
	e.srv.AddMessage(testUser, inbox, mailBytes("fine@x", "fine", d, ""), fakeupstream.AddOptions{Date: d})
	bad := append([]byte("Message-ID: <bad@x>\r\nFrom: Carol Smith <carol@example.test>\r\nTo: alice@example.test, dave@example.test\r\nSubject: Quarterly numbers\r\n"+
		"Date: Fri, 09 Oct 2026 08:30:00 +0000\r\nMIME-Version: 1.0\r\nContent-Type: text/plain\r\n\r\n"), bytes.Repeat([]byte("0123456789abcdef"), 400)...)
	d2 := e.clock.Now().Add(-2 * time.Hour)
	e.srv.AddMessage(testUser, inbox, bad, fakeupstream.AddOptions{Date: d2, Read: true})
	// Every download fails for a reason that is about the item.
	e.srv.FailNext("FetchMIME", &upstream.StatusError{Op: "Fetch", Code: 8, Perm: true, Reason: "item not convertible"}, -1)
	a := e.login()
	f := e.waitFolder(a, "INBOX")

	eventually(t, "the stand-in is stored", func() bool { return serverCount(a, f.ID) == 2 })
	if n, _ := a.Store().PendingCount(0); n != 0 {
		t.Errorf("%d downloads still pending", n)
	}
	uids, _ := a.Store().UIDsByMessageID(f.ID, "fine@x")
	if len(uids) != 1 {
		t.Fatalf("the ordinary message: %v", uids)
	}
	var stand *store.Message
	ms, _ := a.Store().ListMessages(f.ID)
	for _, m := range ms {
		if m.UID != uids[0] {
			stand, _ = a.Store().Message(f.ID, m.UID)
		}
	}
	if stand == nil || stand.ServerID == "" || stand.Kind != store.KindServer || !stand.Flags.Has(store.FlagSeen) {
		t.Fatalf("stand-in: %+v", stand)
	}
	raw, err := a.Store().MIME(f.ID, stand.UID)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("the stand-in is not a message: %v\n%s", err, raw)
	}
	if got := msg.Header.Get("Subject"); got != "Quarterly numbers" {
		t.Errorf("subject = %q", got)
	}
	if from, err := msg.Header.AddressList("From"); err != nil || len(from) != 1 || from[0].Address != "carol@example.test" || from[0].Name != "Carol Smith" {
		t.Errorf("from = %v %v", from, err)
	}
	if to, err := msg.Header.AddressList("To"); err != nil || len(to) != 2 {
		t.Errorf("to = %v %v", to, err)
	}
	if dt, err := msg.Header.Date(); err != nil || !dt.Equal(d2.Truncate(time.Second)) {
		t.Errorf("date = %v %v, want the received date %v", dt, err, d2)
	}
	if msg.Header.Get("X-Easbridge-Stand-In") == "" || !strings.Contains(string(raw), "could not be downloaded") {
		t.Errorf("the stand-in does not say what it is:\n%s", raw)
	}
	if !strings.Contains(e.logs.String(), "showing a stand-in") {
		t.Errorf("the stand-in was not logged")
	}

	// It is final: nothing is fetched again and nothing changes when Exchange
	// recovers.
	calls := e.srv.Calls("FetchMIME")
	e.srv.ClearFailures()
	consistently(t, "no more downloads", 300*time.Millisecond, func() bool {
		return e.srv.Calls("FetchMIME") == calls && serverCount(a, f.ID) == 2
	})
}

// (e) is covered by TestSentCopyWithAnotherMessageIDIsNeverMergedByLookAlike and
// TestSentFallbackStillBindsAnExchangeCopyThatHasNoMessageID in mutate_test.go;
// this is the same rule while the Sent folder is still doing its first sync.
func TestStagedSentCopiesFollowTheSameMessageIDRule(t *testing.T) {
	e := newEnv(t)
	sentSID := e.srv.FolderID(testUser, "Sent Items")
	old1 := e.clock.Now().Add(-5 * time.Hour)
	old2 := e.clock.Now().Add(-4 * time.Hour)
	now := e.clock.Now()
	e.srv.AddMessage(testUser, sentSID, mailBytes("old1@x", "first", old1, ""), fakeupstream.AddOptions{Date: old1, Read: true})
	e.srv.AddMessage(testUser, sentSID, bigMail("old2@x", "second", old2), fakeupstream.AddOptions{Date: old2, Read: true})
	// Exchange's copies: one under another Message-ID than the client's copy, one
	// without a Message-ID. Subject, date and recipients are those of the client's.
	e.srv.AddMessage(testUser, sentSID, mailBytes("exchange-id@x", "just sent", now, ""), fakeupstream.AddOptions{Date: now, Read: true})
	noID := bytes.Replace(mailBytes("noid", "second sent", now, ""), []byte("Message-ID: <noid>\r\n"), nil, 1)
	e.srv.AddMessage(testUser, sentSID, noID, fakeupstream.AddOptions{Date: now, Read: true})
	e.srv.HoldAfter("FetchMIME", testUser, sentSID, 0) // keeps the folder closed

	a := e.login()
	f := e.waitFolder(a, "Sent Items")
	eventually(t, "Exchange's copies are staged", func() bool {
		p := e.progress(a, f.ID)
		return p.Staged == 3 && p.PendingFetch == 1
	})
	for _, id := range []struct{ id, subject string }{{"client-a@x", "just sent"}, {"client-b@x", "second sent"}} {
		if _, err := a.Append("Sent Items", mailBytes(id.id, id.subject, now, ""), store.FlagSeen, "", time.Time{}, "s"); err != nil {
			t.Fatal(err)
		}
	}
	e.srv.Release("FetchMIME", testUser, sentSID)
	eventually(t, "the folder opens", func() bool {
		cur, _ := a.Store().FolderByID(f.ID)
		return cur.InitialComplete
	})
	// UID 1 (message-ID client-a) keeps Exchange's copy with another ID apart; UID 2
	// (client-b) takes over the copy that has no Message-ID.
	m1, _ := a.Store().Message(f.ID, 1)
	m2, _ := a.Store().Message(f.ID, 2)
	if m1.Kind != store.KindPlaceholder {
		t.Errorf("UID 1 was bound to a message with another Message-ID: %+v", m1)
	}
	if m2.Kind != store.KindServer || m2.ServerID == "" {
		t.Errorf("UID 2 was not bound to the copy without a Message-ID: %+v", m2)
	}
	if c, _ := a.Store().Counts(f.ID); c.Messages != 5 { // 2 placeholders (one bound), old1, old2, exchange-id
		t.Errorf("counts = %+v, want 5 messages", c)
	}
}
