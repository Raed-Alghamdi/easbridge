//go:build devfake

package engine_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"easbridge/internal/engine"
	"easbridge/internal/fakeupstream"
	"easbridge/internal/store"
	"easbridge/internal/upstream"
)

// First sync, all or nothing (v0.1.1): a folder shows none of its server
// messages until its first sync has caught up, then all of them at once, with
// UIDs in date order. These tests freeze the fake Exchange mid-chain with
// HoldAfter so that every phase can be observed deterministically.

// bigMail is larger than the 4 KB inline limit of the test setup, so Sync
// announces it truncated and it needs a separate fetch.
func bigMail(id, subject string, date time.Time) []byte {
	return append(mailBytes(id, subject, date, ""), bytes.Repeat([]byte("0123456789abcdef"), 400)...)
}

// waitFolder waits for the folder row of a mail folder to exist.
func (e *env) waitFolder(a *engine.Account, path string) *store.Folder {
	e.t.Helper()
	var f *store.Folder
	eventually(e.t, "folder "+path+" is known", func() bool {
		var err error
		f, err = a.Store().FolderByPath(path)
		return err == nil
	})
	return f
}

// expectDateOrder checks that the visible messages of a folder carry UIDs
// 1..n in ascending order of (received date, ServerId), exactly the server's
// items.
func (e *env) expectDateOrder(a *engine.Account, f *store.Folder, serverFolder string) {
	e.t.Helper()
	items := e.srv.Items(testUser, serverFolder) // oldest first, ties by ServerId
	metas, err := a.Store().ListMessages(f.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	if len(metas) != len(items) {
		e.t.Fatalf("%d visible messages, the server has %d", len(metas), len(items))
	}
	for i, it := range items {
		m, err := a.Store().MessageByServerID(f.ID, it.ID)
		if err != nil {
			e.t.Fatalf("item %s (rank %d): %v", it.ID, i+1, err)
		}
		if m.UID != uint32(i+1) {
			e.t.Errorf("item %s (date %s) has UID %d, want %d", it.ID, it.Date.Format(time.RFC3339), m.UID, i+1)
		}
	}
	for i := 1; i < len(metas); i++ {
		if metas[i].InternalDate < metas[i-1].InternalDate {
			e.t.Errorf("UID %d is older than UID %d", metas[i].UID, metas[i-1].UID)
		}
	}
}

func TestFirstSyncOpensTheFolderAllAtOnceWithUIDsInDateOrder(t *testing.T) {
	e := newEnv(t)
	e.srv.Window = 4
	inbox := e.srv.FolderID(testUser, "Inbox")
	const n = 17
	base := e.clock.Now().Add(-100 * time.Hour)
	for i := 0; i < n; i++ {
		// A scrambled date order (7 is coprime with 17), and every fourth message
		// is too large to come inline, so it also needs a separate fetch.
		d := base.Add(time.Duration((i*7)%n) * time.Hour)
		mime := mailBytes(fmt.Sprintf("m%d@x", i), fmt.Sprintf("m%d", i), d, "")
		if i%4 == 0 {
			mime = bigMail(fmt.Sprintf("m%d@x", i), fmt.Sprintf("m%d", i), d)
		}
		e.srv.AddMessage(testUser, inbox, mime, fakeupstream.AddOptions{Date: d})
	}
	// Two messages share a date: the ServerId breaks the tie.
	tie := base.Add(40 * time.Hour)
	e.srv.AddMessage(testUser, inbox, mailBytes("tie1@x", "tie", tie, ""), fakeupstream.AddOptions{Date: tie})
	e.srv.AddMessage(testUser, inbox, mailBytes("tie2@x", "tie", tie, ""), fakeupstream.AddOptions{Date: tie})
	const total = n + 2

	// Freeze the chain after the bootstrap and two batches, and keep every
	// large message from being fetched.
	e.srv.HoldAfter("Sync", testUser, inbox, 3)
	e.srv.HoldAfter("FetchMIME", testUser, inbox, 0)
	a := e.login()
	f := e.waitFolder(a, "INBOX")
	rec, snap := e.watch(a, f)
	if len(snap) != 0 {
		t.Fatalf("snapshot of a new folder: %+v", snap)
	}
	eventually(t, "two batches arrive", func() bool {
		p := e.progress(a, f.ID)
		return p.Staged+p.PendingFetch >= 8
	})

	// Phase 1: mid-chain. Nothing is visible, nothing is announced, and STATUS
	// and SELECT see an empty folder whose UIDNEXT does not move.
	consistently(t, "the folder looks empty while its first sync runs", 250*time.Millisecond, func() bool {
		c, err := a.Store().Counts(f.ID)
		ms, _ := a.Store().ListMessages(f.ID)
		cur, _ := a.Store().FolderByID(f.ID)
		return err == nil && c.Messages == 0 && c.Unseen == 0 && c.UIDNext == 1 && len(ms) == 0 && !cur.InitialComplete &&
			len(rec.all()) == 0
	})
	st := e.mgr.Status().Accounts[0]
	if st.InitialPending != 5 || st.InitialDone != 0 || st.Staged == 0 {
		t.Errorf("health during the first sync: %+v", st)
	}

	// Mail that arrives during the chain is part of the bulk.
	fresh := e.clock.Now()
	e.srv.AddMessage(testUser, inbox, mailBytes("fresh@x", "fresh", fresh, ""), fakeupstream.AddOptions{Date: fresh})

	// Phase 2: the chain ends, but large messages are still being fetched.
	e.srv.Release("Sync", testUser, inbox)
	eventually(t, "the chain is done", func() bool {
		cur, _ := a.Store().FolderByID(f.ID)
		return cur.InitialChainDone
	})
	consistently(t, "the folder stays closed until the last body is in", 200*time.Millisecond, func() bool {
		cur, _ := a.Store().FolderByID(f.ID)
		return !cur.InitialComplete && serverCount(a, f.ID) == 0 && len(rec.all()) == 0
	})
	if p := e.progress(a, f.ID); p.PendingFetch == 0 {
		t.Fatalf("expected large messages to be waiting: %+v", p)
	}

	// Phase 3: the last body arrives and the folder opens in one step.
	e.srv.Release("FetchMIME", testUser, inbox)
	eventually(t, "the folder opens", func() bool {
		cur, _ := a.Store().FolderByID(f.ID)
		return cur.InitialComplete
	})
	waitBulk(t, rec)
	if rec.count(store.EvBulkAdd) != 1 || rec.count(store.EvAdd) != 0 || len(rec.all()) != 1 {
		t.Fatalf("want a single bulk announcement, got %+v", rec.all())
	}
	if got := len(rec.all()[0].Bulk); got != total+1 {
		t.Fatalf("the bulk announced %d messages, want %d", got, total+1)
	}
	for i, m := range rec.all()[0].Bulk {
		if m.UID != uint32(i+1) {
			t.Fatalf("bulk[%d].UID = %d", i, m.UID)
		}
	}
	e.expectDateOrder(a, f, inbox)
	if tie1, _ := a.Store().UIDsByMessageID(f.ID, "tie1@x"); len(tie1) != 1 {
		t.Fatal("tie1 missing")
	}
	t1, _ := a.Store().UIDsByMessageID(f.ID, "tie1@x")
	t2, _ := a.Store().UIDsByMessageID(f.ID, "tie2@x")
	if t1[0]+1 != t2[0] {
		t.Errorf("messages with the same date are not numbered by ServerId: %d, %d", t1[0], t2[0])
	}
	if fr, _ := a.Store().UIDsByMessageID(f.ID, "fresh@x"); len(fr) != 1 || fr[0] != uint32(total+1) {
		t.Errorf("the message that arrived during the chain: %v, want it last (%d)", fr, total+1)
	}
	if c, _ := a.Store().Counts(f.ID); c.Messages != total+1 || c.UIDNext != uint32(total+2) {
		t.Errorf("counts after opening: %+v", c)
	}
	st = e.mgr.Status().Accounts[0]
	eventually(t, "every folder is open", func() bool {
		st = e.mgr.Status().Accounts[0]
		return st.InitialDone == 5 && st.InitialPending == 0 && st.Staged == 0
	})

	// Afterwards a new message is an ordinary addition above the highest UID.
	later := e.clock.Now().Add(time.Minute)
	e.srv.AddMessage(testUser, inbox, mailBytes("later@x", "later", later, ""), fakeupstream.AddOptions{Date: later})
	eventually(t, "new mail after the first sync", func() bool { return rec.count(store.EvAdd) == 1 })
	if got := rec.uids(store.EvAdd); len(got) != 1 || got[0] != uint32(total+2) {
		t.Errorf("new mail got UIDs %v, want [%d]", got, total+2)
	}
	if rec.count(store.EvBulkAdd) != 1 {
		t.Errorf("the folder was announced again: %d bulk events", rec.count(store.EvBulkAdd))
	}
}

func TestResetAfterTheFolderOpenedKeepsItOpenAndKeepsUIDs(t *testing.T) {
	e := newEnv(t)
	inbox := e.srv.FolderID(testUser, "Inbox")
	for i := 0; i < 6; i++ {
		d := e.clock.Now().Add(-time.Duration(6-i) * time.Hour)
		e.srv.AddMessage(testUser, inbox, mailBytes(fmt.Sprintf("r%d@x", i), "s", d, ""), fakeupstream.AddOptions{Date: d})
	}
	a := e.login()
	f := e.mirrored(a, "Inbox", "INBOX")
	rec, _ := e.watch(a, f)
	before := map[string]uint32{}
	for i := 0; i < 6; i++ {
		u, _ := a.Store().UIDsByMessageID(f.ID, fmt.Sprintf("r%d@x", i))
		if len(u) != 1 {
			t.Fatalf("r%d: %v", i, u)
		}
		before[fmt.Sprintf("r%d@x", i)] = u[0]
	}

	// Exchange forgets the folder's sync state: the engine resyncs from scratch
	// and reconciles. The folder must never close or renumber meanwhile.
	e.srv.ResetFolder(testUser, inbox)
	deadline := time.Now().Add(5 * time.Second)
	for {
		cur, _ := a.Store().FolderByID(f.ID)
		if !cur.InitialComplete || serverCount(a, f.ID) != 6 {
			t.Fatalf("the folder was hidden or emptied during a reset: %+v, %d messages", cur, serverCount(a, f.ID))
		}
		if cur.ReconEpoch > 0 && !cur.ReconActive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the reset was never reconciled")
		}
		time.Sleep(2 * time.Millisecond)
	}
	for id, uid := range before {
		if u, _ := a.Store().UIDsByMessageID(f.ID, id); len(u) != 1 || u[0] != uid {
			t.Errorf("%s: UIDs %v, want [%d]", id, u, uid)
		}
	}
	if len(rec.all()) != 0 {
		t.Errorf("a reset produced events: %+v", rec.all())
	}
}

func TestResetDuringTheFirstSyncStartsOverWithoutGhosts(t *testing.T) {
	e := newEnv(t)
	e.srv.Window = 3
	inbox := e.srv.FolderID(testUser, "Inbox")
	ids := map[string]string{}
	for i := 0; i < 9; i++ {
		d := e.clock.Now().Add(-time.Duration(9-i) * time.Hour)
		name := fmt.Sprintf("g%d@x", i)
		ids[name] = e.srv.AddMessage(testUser, inbox, mailBytes(name, "s", d, ""), fakeupstream.AddOptions{Date: d})
	}
	e.srv.HoldAfter("Sync", testUser, inbox, 3) // bootstrap + the three newest batches' first two = six staged
	a := e.login()
	f := e.waitFolder(a, "INBOX")
	rec, _ := e.watch(a, f)
	eventually(t, "six messages are staged", func() bool { return e.progress(a, f.ID).Staged == 6 })

	// Exchange forgets the state, and a message that is already staged goes away
	// in the gap (g8 is the newest, so it was in the first batch).
	e.srv.ResetFolder(testUser, inbox)
	e.srv.DeleteMessage(testUser, inbox, ids["g8@x"])
	e.srv.Release("Sync", testUser, inbox)

	eventually(t, "the folder opens", func() bool {
		cur, _ := a.Store().FolderByID(f.ID)
		return cur.InitialComplete
	})
	e.expectDateOrder(a, f, inbox) // exactly the server's eight survivors, no ghost of g8
	if u, _ := a.Store().UIDsByMessageID(f.ID, "g8@x"); len(u) != 0 {
		t.Errorf("a message deleted during the first sync is visible: %v", u)
	}
	waitBulk(t, rec)
	if rec.count(store.EvBulkAdd) != 1 || rec.count(store.EvAdd) != 0 || rec.count(store.EvExpunge) != 0 {
		t.Errorf("events: %+v", rec.all())
	}
	if !strings.Contains(e.logs.String(), "downloading it again") {
		t.Errorf("the restart was not logged")
	}
}

func TestMessageThatKeepsFailingOnItsOwnStopsHoldingTheFolderBack(t *testing.T) {
	e := newEnv(t)
	inbox := e.srv.FolderID(testUser, "Inbox")
	for i := 0; i < 3; i++ {
		d := e.clock.Now().Add(-time.Duration(10-i) * time.Hour)
		e.srv.AddMessage(testUser, inbox, mailBytes(fmt.Sprintf("ok%d@x", i), "s", d, ""), fakeupstream.AddOptions{Date: d})
	}
	d := e.clock.Now().Add(-time.Hour)
	e.srv.AddMessage(testUser, inbox, bigMail("broken@x", "broken", d), fakeupstream.AddOptions{Date: d})
	// Exchange answers every attempt to fetch the large message with an error
	// status of its own.
	e.srv.FailNext("FetchMIME", &upstream.StatusError{Op: "Fetch", Code: 6, Perm: true, Reason: "no such object"}, -1)
	a := e.login()
	f := e.waitFolder(a, "INBOX")
	rec, _ := e.watch(a, f)

	eventually(t, "the folder opens without the message that cannot be fetched", func() bool { return serverCount(a, f.ID) == 3 })
	// (The log line follows the commit that opened the folder by a hair.)
	eventually(t, "giving up on the message is logged", func() bool {
		return strings.Contains(e.logs.String(), "no longer waits for it")
	})
	if n, _ := a.Store().PendingCount(f.ID); n != 1 {
		t.Fatalf("the message must stay queued for later, pending = %d", n)
	}
	// Once it can be fetched it follows like any later arrival, above the others.
	e.srv.ClearFailures()
	eventually(t, "the message follows", func() bool { return serverCount(a, f.ID) == 4 })
	if u, _ := a.Store().UIDsByMessageID(f.ID, "broken@x"); len(u) != 1 || u[0] != 4 {
		t.Errorf("late message UIDs: %v, want [4]", u)
	}
	waitBulk(t, rec)
	if rec.count(store.EvBulkAdd) != 1 || rec.count(store.EvAdd) != 1 {
		t.Errorf("events: %d bulk, %d add", rec.count(store.EvBulkAdd), rec.count(store.EvAdd))
	}
}

func TestTransientStatusesNeverMakeAMessageGiveUp(t *testing.T) {
	// "Server busy" and its kin describe the server, not the message: however
	// long they last, the folder must stay closed and then open with everything,
	// instead of opening without its large messages and receiving them as late
	// (alerting) arrivals.
	e := newEnv(t)
	inbox := e.srv.FolderID(testUser, "Inbox")
	for i := 0; i < 2; i++ {
		d := e.clock.Now().Add(-time.Duration(10-i) * time.Hour)
		e.srv.AddMessage(testUser, inbox, mailBytes(fmt.Sprintf("ok%d@x", i), "s", d, ""), fakeupstream.AddOptions{Date: d})
	}
	d := e.clock.Now().Add(-time.Hour)
	e.srv.AddMessage(testUser, inbox, bigMail("busy@x", "busy", d), fakeupstream.AddOptions{Date: d})
	e.srv.FailNext("FetchMIME", &upstream.StatusError{Op: "Fetch", Code: 110, Perm: false, Reason: "server busy"}, -1)
	a := e.login()
	f := e.waitFolder(a, "INBOX")
	rec, _ := e.watch(a, f)

	// The engine gives up on a message after five failures of its own: wait for
	// far more attempts than that.
	eventually(t, "many attempts", func() bool { return e.srv.Calls("FetchMIME") >= 25 })
	consistently(t, "the folder stays closed", 150*time.Millisecond, func() bool {
		cur, _ := a.Store().FolderByID(f.ID)
		return !cur.InitialComplete && serverCount(a, f.ID) == 0
	})
	pend, _ := a.Store().PendingDue(e.clock.Now().Add(time.Hour), 10)
	if len(pend) != 1 || pend[0].Attempts < 5 || pend[0].ItemFailures != 0 {
		t.Fatalf("pending after transient failures: %+v", pend)
	}

	e.srv.ClearFailures()
	eventually(t, "the folder opens with every message", func() bool { return serverCount(a, f.ID) == 3 })
	waitBulk(t, rec)
	if rec.count(store.EvBulkAdd) != 1 || rec.count(store.EvAdd) != 0 {
		t.Errorf("events: %d bulk, %d add", rec.count(store.EvBulkAdd), rec.count(store.EvAdd))
	}
	e.expectDateOrder(a, f, inbox)
}

// A permanent status about the device or the account (a blocked device, a disabled
// account, a policy the server enforces) describes the server's verdict on the
// whole mailbox, not the message. Counted against the message it would, after ten
// tries, replace it for good by a stand-in although nothing is wrong with it.
func TestStatusesAboutTheDeviceNeverMakeAMessageGiveUp(t *testing.T) {
	e := newEnv(t)
	inbox := e.srv.FolderID(testUser, "Inbox")
	for i := 0; i < 2; i++ {
		d := e.clock.Now().Add(-time.Duration(10-i) * time.Hour)
		e.srv.AddMessage(testUser, inbox, mailBytes(fmt.Sprintf("ok%d@x", i), "s", d, ""), fakeupstream.AddOptions{Date: d})
	}
	d := e.clock.Now().Add(-time.Hour)
	e.srv.AddMessage(testUser, inbox, bigMail("blocked@x", "blocked", d), fakeupstream.AddOptions{Date: d})
	// The device was blocked for this user (status 129): permanent, and global.
	e.srv.FailNext("FetchMIME", &upstream.StatusError{Op: "Fetch", Code: 129, Perm: true, Global: true,
		Reason: "the server blocks this device for this user; the Exchange administrator must allow it"}, -1)
	a := e.login()
	f := e.waitFolder(a, "INBOX")

	// More attempts than a message may fail for reasons of its own (five to stop
	// holding the folder, ten for a stand-in).
	eventually(t, "many attempts", func() bool { return e.srv.Calls("FetchMIME") >= 30 })
	consistently(t, "the folder stays closed and the message stays what it is", 150*time.Millisecond, func() bool {
		cur, _ := a.Store().FolderByID(f.ID)
		return !cur.InitialComplete && serverCount(a, f.ID) == 0
	})
	pend, _ := a.Store().PendingDue(e.clock.Now().Add(time.Hour), 10)
	if len(pend) != 1 || pend[0].Attempts < 10 || pend[0].ItemFailures != 0 {
		t.Fatalf("pending after the device was refused: %+v", pend)
	}
	if strings.Contains(e.logs.String(), "stand-in") {
		t.Errorf("a stand-in was made for a message that is not at fault")
	}

	// The administrator allows the device: the real message arrives.
	e.srv.ClearFailures()
	eventually(t, "the folder opens with every message", func() bool { return serverCount(a, f.ID) == 3 })
	raw, err := a.Store().MIME(f.ID, 3)
	if err != nil || !bytes.Contains(raw, []byte("Message-ID: <blocked@x>")) || bytes.Contains(raw, []byte("X-Easbridge-Stand-In")) {
		t.Errorf("the message is not the real one: %v\n%.200s", err, raw)
	}
}

func TestSentCopyThatArrivesDuringTheFirstSyncBindsToTheClientsCopy(t *testing.T) {
	e := newEnv(t)
	sentSID := e.srv.FolderID(testUser, "Sent Items")
	old1 := e.clock.Now().Add(-5 * time.Hour)
	old2 := e.clock.Now().Add(-4 * time.Hour)
	now := e.clock.Now()
	e.srv.AddMessage(testUser, sentSID, mailBytes("old1@x", "first", old1, ""), fakeupstream.AddOptions{Date: old1, Read: true})
	e.srv.AddMessage(testUser, sentSID, bigMail("old2@x", "second", old2), fakeupstream.AddOptions{Date: old2, Read: true})
	// Exchange's own copy of a message the client is about to send.
	copyMIME := mailBytes("sent-now@x", "just sent", now, "")
	e.srv.AddMessage(testUser, sentSID, copyMIME, fakeupstream.AddOptions{Date: now, Read: true})
	e.srv.HoldAfter("FetchMIME", testUser, sentSID, 0) // keeps the folder closed

	a := e.login()
	f := e.waitFolder(a, "Sent Items")
	eventually(t, "Exchange's copy is staged", func() bool {
		p := e.progress(a, f.ID)
		return p.Staged == 2 && p.PendingFetch == 1
	})
	// The client appends its own copy (MailFlow does that after the SMTP send).
	res, err := a.Append("Sent Items", copyMIME, store.FlagSeen, "", time.Time{}, "s")
	if err != nil || res.Duplicate || res.UID != 1 {
		t.Fatalf("APPEND while the folder is closed: %+v, %v (the client needs a UID at once)", res, err)
	}

	e.srv.Release("FetchMIME", testUser, sentSID)
	eventually(t, "the folder opens", func() bool {
		cur, _ := a.Store().FolderByID(f.ID)
		return cur.InitialComplete
	})
	if n := serverCount(a, f.ID); n != 3 {
		t.Fatalf("%d messages, want 3 (no duplicate of the sent message)", n)
	}
	u, _ := a.Store().UIDsByMessageID(f.ID, "sent-now@x")
	if len(u) != 1 || u[0] != 1 {
		t.Fatalf("the sent message has UIDs %v, want [1] (the client's UID)", u)
	}
	m, _ := a.Store().Message(f.ID, 1)
	if m.Kind != store.KindServer || m.ServerID == "" {
		t.Errorf("the client's copy was not bound to Exchange's: %+v", m)
	}
	// The other two follow in date order above it.
	for uid, id := range map[uint32]string{2: "old1@x", 3: "old2@x"} {
		if got, _ := a.Store().UIDsByMessageID(f.ID, id); len(got) != 1 || got[0] != uid {
			t.Errorf("%s: %v, want [%d]", id, got, uid)
		}
	}
}

func TestMoveIntoAnInboxStillDownloadingIsStagedAndGivesNoCopyUID(t *testing.T) {
	// Scenario: the Inbox waits for a large message while small
	// folders have already opened, and a client moves a message into the Inbox.
	// A COPYUID would give MailFlow a known UID in the Inbox, and every message
	// of the later bulk would count as new mail.
	e := newEnv(t)
	inbox := e.srv.FolderID(testUser, "Inbox")
	junk := e.srv.FolderID(testUser, "Junk Email")
	di := e.clock.Now().Add(-8 * time.Hour)
	e.srv.AddMessage(testUser, inbox, mailBytes("in1@x", "s", di, ""), fakeupstream.AddOptions{Date: di})
	e.srv.AddMessage(testUser, inbox, bigMail("in2@x", "s", di.Add(time.Hour)), fakeupstream.AddOptions{Date: di.Add(time.Hour)})
	dj := e.clock.Now().Add(-3 * time.Hour)
	e.srv.AddMessage(testUser, junk, mailBytes("j1@x", "s", dj, ""), fakeupstream.AddOptions{Date: dj})
	e.srv.AddMessage(testUser, junk, mailBytes("j2@x", "s", dj.Add(time.Hour), ""), fakeupstream.AddOptions{Date: dj.Add(time.Hour)})
	e.srv.HoldAfter("FetchMIME", testUser, inbox, 0)

	a := e.login()
	fJunk := e.mirrored(a, "Junk Email", "Junk Email") // Junk opens while the Inbox waits
	fIn := e.waitFolder(a, "INBOX")
	eventually(t, "the Inbox is waiting for its large message", func() bool {
		p := e.progress(a, fIn.ID)
		return p.Staged == 1 && p.PendingFetch == 1
	})
	out, err := a.Move(context.Background(), fJunk.ID, []uint32{1}, fIn.ID, "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Moved) != 0 || out.Hidden != 1 || len(out.Failed) != 0 {
		t.Fatalf("MOVE into a closed Inbox: %+v (no UID may be handed out)", out)
	}
	if serverCount(a, fIn.ID) != 0 || serverCount(a, fJunk.ID) != 1 {
		t.Fatalf("Inbox %d, Junk %d visible messages", serverCount(a, fIn.ID), serverCount(a, fJunk.ID))
	}

	e.srv.Release("FetchMIME", testUser, inbox)
	eventually(t, "the Inbox opens", func() bool {
		cur, _ := a.Store().FolderByID(fIn.ID)
		return cur.InitialComplete
	})
	e.expectDateOrder(a, fIn, inbox) // three messages, the moved one exactly once
}

func TestMoveIntoAnotherFolderStillDownloadingGetsItsUIDAtOnce(t *testing.T) {
	// Outside the Inbox nothing alerts, and the client needs the COPYUID (the
	// Snoozed and Trash flows track the moved message by it). The moved message
	// keeps that UID; the folder's own messages are numbered above it when the
	// folder opens.
	e := newEnv(t)
	inbox := e.srv.FolderID(testUser, "Inbox")
	junk := e.srv.FolderID(testUser, "Junk Email")
	for i := 0; i < 2; i++ {
		d := e.clock.Now().Add(-time.Duration(2-i) * time.Hour)
		e.srv.AddMessage(testUser, inbox, mailBytes(fmt.Sprintf("in%d@x", i), "s", d, ""), fakeupstream.AddOptions{Date: d})
	}
	dj := e.clock.Now().Add(-9 * time.Hour)
	e.srv.AddMessage(testUser, junk, mailBytes("j1@x", "s", dj, ""), fakeupstream.AddOptions{Date: dj})
	e.srv.AddMessage(testUser, junk, bigMail("j2@x", "s", dj.Add(time.Hour)), fakeupstream.AddOptions{Date: dj.Add(time.Hour)})
	e.srv.HoldAfter("FetchMIME", testUser, junk, 0)

	a := e.login()
	fIn := e.mirrored(a, "Inbox", "INBOX")
	fJunk := e.waitFolder(a, "Junk Email")
	eventually(t, "Junk Email has staged and waiting messages", func() bool {
		p := e.progress(a, fJunk.ID)
		return p.Staged == 1 && p.PendingFetch == 1
	})
	out, err := a.Move(context.Background(), fIn.ID, []uint32{1}, fJunk.ID, "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Moved) != 1 || out.Moved[0].Dst != 1 || out.Hidden != 0 {
		t.Fatalf("MOVE into a closed folder: %+v", out)
	}
	if serverCount(a, fJunk.ID) != 1 {
		t.Fatalf("the moved message must be visible at once, %d visible", serverCount(a, fJunk.ID))
	}

	e.srv.Release("FetchMIME", testUser, junk)
	eventually(t, "Junk Email opens", func() bool {
		cur, _ := a.Store().FolderByID(fJunk.ID)
		return cur.InitialComplete
	})
	if serverCount(a, fJunk.ID) != 3 {
		t.Fatalf("%d messages, want 3 (the moved one exactly once)", serverCount(a, fJunk.ID))
	}
	moved, _ := a.Store().UIDsByMessageID(fJunk.ID, "in0@x")
	if len(moved) != 1 || moved[0] != 1 {
		t.Errorf("the moved message must keep UID 1: %v", moved)
	}
	for uid, id := range map[uint32]string{2: "j1@x", 3: "j2@x"} {
		if got, _ := a.Store().UIDsByMessageID(fJunk.ID, id); len(got) != 1 || got[0] != uid {
			t.Errorf("%s: %v, want [%d] (date order above the moved message)", id, got, uid)
		}
	}
}

func TestMoveIntoAFolderTheClientJustCreatedGivesACopyUID(t *testing.T) {
	// MailFlow creates its Snoozed folder and moves mail into it at once, and
	// tracks the message by the COPYUID. A new folder starts closed like any
	// other, but the UID is handed out at once.
	e := newEnv(t)
	inbox := e.srv.FolderID(testUser, "Inbox")
	e.srv.AddMessage(testUser, inbox, mailBytes("snooze@x", "s", e.clock.Now(), ""), fakeupstream.AddOptions{Date: e.clock.Now()})
	a := e.login()
	fIn := e.mirrored(a, "Inbox", "INBOX")
	ctx := context.Background()
	if err := a.CreateFolder(ctx, "Snoozed"); err != nil {
		t.Fatal(err)
	}
	fs, err := a.Store().FolderByPath("Snoozed")
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Move(ctx, fIn.ID, []uint32{1}, fs.ID, "s")
	if err != nil || len(out.Moved) != 1 || out.Moved[0].Dst != 1 || out.Hidden != 0 {
		t.Fatalf("MOVE into the new folder: %+v, %v", out, err)
	}
	// Its (empty) first sync finishes, lists the moved message once, and opens the folder.
	eventually(t, "the new folder opens", func() bool {
		cur, _ := a.Store().FolderByID(fs.ID)
		return cur.InitialComplete
	})
	if serverCount(a, fs.ID) != 1 {
		t.Errorf("%d messages in the new folder, want the moved one only", serverCount(a, fs.ID))
	}
	if u, _ := a.Store().UIDsByMessageID(fs.ID, "snooze@x"); len(u) != 1 || u[0] != 1 {
		t.Errorf("the moved message must keep UID 1: %v", u)
	}
}

func TestWaitHierarchyBlocksUntilTheFolderListIsKnown(t *testing.T) {
	e := newEnv(t)
	e.srv.HoldAfter("FolderSync", testUser, "", 0)
	a := e.login() // a fresh database: the engine is now waiting for Exchange's folder list
	ctx := context.Background()
	if a.WaitHierarchy(ctx, 60*time.Millisecond) {
		t.Fatal("the hierarchy cannot be known while FolderSync is held")
	}
	done := make(chan bool, 1)
	go func() { done <- a.WaitHierarchy(ctx, 5*time.Second) }()
	select {
	case ok := <-done:
		t.Fatalf("WaitHierarchy returned %v before the folder list arrived", ok)
	case <-time.After(100 * time.Millisecond):
	}
	e.srv.Release("FolderSync", testUser, "")
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("WaitHierarchy gave up although the folder list arrived")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WaitHierarchy did not return after the folder list arrived")
	}
	if _, err := a.Store().FolderByPath("INBOX"); err != nil {
		t.Errorf("no INBOX once the hierarchy is known: %v", err)
	}
	// Known hierarchy: no waiting at all.
	start := time.Now()
	if !a.WaitHierarchy(ctx, time.Second) || time.Since(start) > 100*time.Millisecond {
		t.Errorf("WaitHierarchy was slow or failed with a known hierarchy")
	}
	// A cancelled context ends the wait.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	e2 := newEnv(t)
	e2.srv.HoldAfter("FolderSync", testUser, "", 0)
	if e2.login().WaitHierarchy(cctx, 5*time.Second) {
		t.Error("a cancelled context must end the wait")
	}
}

func TestFirstSyncResumesWhereItStoppedAfterAnEngineRestart(t *testing.T) {
	// The grace period runs on the (real-time) fake clock from the engine's creation, and
	// nothing is attached: a short one (it was 50 ms) can end before the test has sent its
	// first request when the machine is slow (-race, a bigger schema). The test moves the
	// clock itself to end the grace, so a long one costs only the reaper's tick (a tenth).
	e := newEnv(t, func(c *engine.Config, _ *fakeupstream.Server) { c.Grace = time.Second })
	e.srv.Window = 3
	inbox := e.srv.FolderID(testUser, "Inbox")
	for i := 0; i < 10; i++ {
		d := e.clock.Now().Add(-time.Duration(10-i) * time.Hour)
		e.srv.AddMessage(testUser, inbox, mailBytes(fmt.Sprintf("p%d@x", i), "s", d, ""), fakeupstream.AddOptions{Date: d})
	}
	e.srv.HoldAfter("Sync", testUser, inbox, 3) // bootstrap + two batches
	a := e.login()
	f := e.waitFolder(a, "INBOX")
	eventually(t, "six messages are staged", func() bool { return e.progress(a, f.ID).Staged == 6 })

	// Nobody is connected: the engine stops after its grace period, in the
	// middle of the first sync.
	e.clock.Advance(time.Hour)
	eventually(t, "the engine stops", func() bool { return a.Stopped() })
	cur, _ := e.mgr.DB().Account(testUser)
	if p := func() store.FolderProgress {
		ps, _ := cur.InitialProgress()
		for _, p := range ps {
			if p.FolderID == f.ID {
				return p
			}
		}
		return store.FolderProgress{}
	}(); p.Staged != 6 || p.Complete {
		t.Fatalf("what was collected must survive the stop: %+v", p)
	}

	// A new engine carries on from the committed key and finishes the job.
	b := e.login()
	if b == a {
		t.Fatal("expected a fresh engine")
	}
	e.srv.Release("Sync", testUser, inbox)
	eventually(t, "the folder opens", func() bool {
		cf, _ := b.Store().FolderByID(f.ID)
		return cf.InitialComplete
	})
	e.expectDateOrder(b, f, inbox)
}
