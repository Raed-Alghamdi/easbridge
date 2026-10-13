//go:build devfake

package engine_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"easbridge/internal/engine"
	"easbridge/internal/fakeupstream"
	"easbridge/internal/store"
	"easbridge/internal/upstream"
)

func TestFolderMappingAndSpecialUse(t *testing.T) {
	e := newEnv(t)
	e.srv.AddFolder(testUser, "0", "الأرشيف", fakeupstream.TypeMail)
	proj := e.srv.AddFolder(testUser, e.srv.FolderID(testUser, "Inbox"), "Projects", fakeupstream.TypeMail)
	e.srv.AddFolder(testUser, proj, "Clients 2026", fakeupstream.TypeMail)
	a := e.login()
	eventually(t, "folders mapped", func() bool {
		fs, _ := a.Store().Folders()
		return len(fs) == 8
	})
	want := map[string]string{
		"INBOX": "", "Drafts": `\Drafts`, "Deleted Items": `\Trash`, "Sent Items": `\Sent`, "Junk Email": `\Junk`,
		"الأرشيف": `\Archive`, "INBOX/Projects": "", "INBOX/Projects/Clients 2026": "",
	}
	fs, _ := a.Store().Folders()
	got := map[string]string{}
	for _, f := range fs {
		got[f.Path] = f.SpecialUse
	}
	for p, use := range want {
		if g, ok := got[p]; !ok || g != use {
			t.Errorf("folder %q: special-use %q (present=%v), want %q; all=%v", p, g, ok, use, got)
		}
	}
	if _, ok := got["Outbox"]; ok {
		t.Error("Outbox must be hidden")
	}
	if _, ok := got["Calendar"]; ok {
		t.Error("Calendar must not be exposed")
	}
	// Device and policy keys are persisted for the next run (they are not secrets).
	if k, _ := a.Store().PolicyKey(); k != "fake-policy" {
		t.Errorf("policy key = %q", k)
	}
	if id, _ := e.mgr.DB().DeviceID(); len(id) != 32 {
		t.Errorf("device id = %q", id)
	}
}

func TestInitialSyncAcrossWindowsKeepsUIDsAndDates(t *testing.T) {
	e := newEnv(t)
	inbox := e.srv.FolderID(testUser, "Inbox")
	base := e.clock.Now().Add(-48 * time.Hour)
	const n = 13 // Window is 5: three round trips
	for i := 0; i < n; i++ {
		d := base.Add(time.Duration(i) * time.Hour)
		e.srv.AddMessage(testUser, inbox, mailBytes(fmt.Sprintf("m%02d@x", i), fmt.Sprintf("s%02d", i), d, ""),
			fakeupstream.AddOptions{Read: i%2 == 0, Flagged: i == 3, Date: d})
	}
	a := e.login()
	f := e.mirrored(a, "Inbox", "INBOX")
	metas, _ := a.Store().ListMessages(f.ID)
	if len(metas) != n {
		t.Fatalf("%d messages", len(metas))
	}
	for i, m := range metas {
		if m.UID != uint32(i+1) {
			t.Fatalf("UIDs must be dense and increasing: %v", uidsOf(metas))
		}
	}
	// Within one server window UIDs follow the date order (oldest first).
	last := int64(0)
	inversions := 0
	for _, m := range metas {
		if m.InternalDate < last {
			inversions++
		}
		last = m.InternalDate
	}
	// Exchange lists newest first, so the batches arrive newest first: there is
	// one inversion per window boundary at most (13 items, windows of 5).
	if inversions > 2 {
		t.Errorf("too many date inversions: %d", inversions)
	}
	// Flags and INTERNALDATE come from the server.
	byMsgID := func(id string) *store.Message {
		uids, _ := a.Store().UIDsByMessageID(f.ID, id)
		if len(uids) != 1 {
			t.Fatalf("message %s: uids %v", id, uids)
		}
		m, err := a.Store().Message(f.ID, uids[0])
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	m0 := byMsgID("m00@x")
	if !m0.Flags.Has(store.FlagSeen) || m0.Flags.Has(store.FlagFlagged) || !m0.InternalDate.Equal(base.Truncate(time.Second)) {
		t.Errorf("m00: %+v", m0)
	}
	m3 := byMsgID("m03@x")
	if m3.Flags.Has(store.FlagSeen) || !m3.Flags.Has(store.FlagFlagged) {
		t.Errorf("m03 flags = %v", m3.Flags)
	}
	mime, _ := a.Store().MIME(f.ID, m3.UID)
	if !bytes.Contains(mime, []byte("body of m03@x")) {
		t.Errorf("MIME not stored")
	}
	if c, _ := a.Store().Counts(f.ID); c.UIDNext != n+1 || c.Messages != n || c.Unseen != 6 {
		t.Errorf("counts = %+v", c)
	}
}

func TestNewMailArrivesThroughPingWithExists(t *testing.T) {
	e := newEnv(t)
	a := e.login()
	f := e.mirrored(a, "Inbox", "INBOX")
	rec, snap := e.watch(a, f)
	if len(snap) != 0 {
		t.Fatalf("snapshot = %v", snap)
	}
	inbox := e.srv.FolderID(testUser, "Inbox")
	id := e.srv.AddMessage(testUser, inbox, mailBytes("new1@x", "hello", e.clock.Now(), ""), fakeupstream.AddOptions{})
	eventually(t, "EXISTS for the new message", func() bool { return rec.count(store.EvAdd) == 1 })
	if got := rec.uids(store.EvAdd); len(got) != 1 || got[0] != 1 {
		t.Fatalf("add events %v", got)
	}
	// Flag change made elsewhere (OWA, phone) -> FETCH FLAGS.
	yes := true
	e.srv.SetFlags(testUser, inbox, id, &yes, &yes)
	eventually(t, "flags event", func() bool { return rec.count(store.EvFlags) == 1 })
	ev := rec.all()[1]
	if ev.UID != 1 || !ev.Flags.Has(store.FlagSeen) || !ev.Flags.Has(store.FlagFlagged) {
		t.Fatalf("flags event %+v", ev)
	}
	// Deleted elsewhere -> EXPUNGE.
	e.srv.DeleteMessage(testUser, inbox, id)
	eventually(t, "expunge event", func() bool { return rec.count(store.EvExpunge) == 1 })
	if c, _ := a.Store().Counts(f.ID); c.Messages != 0 || c.UIDNext != 2 {
		t.Fatalf("counts = %+v (UIDNEXT must keep growing)", c)
	}
}

func TestTruncatedMessagesHoldTheFolderBackUntilTheirFullMIMEArrives(t *testing.T) {
	e := newEnv(t)
	inbox := e.srv.FolderID(testUser, "Inbox")
	big := mailBytes("big@x", "big one", e.clock.Now().Add(-time.Hour), "")
	big = append(big, bytes.Repeat([]byte("0123456789abcdef"), 1000)...) // 16 KB > the 4 KB inline limit
	e.srv.AddMessage(testUser, inbox, mailBytes("small1@x", "small", e.clock.Now().Add(-3*time.Hour), ""),
		fakeupstream.AddOptions{Date: e.clock.Now().Add(-3 * time.Hour)})
	e.srv.AddMessage(testUser, inbox, big, fakeupstream.AddOptions{Date: e.clock.Now().Add(-time.Hour)})
	// The fetch fails for a while (an outage). The folder shows nothing meanwhile:
	// not the small message, and certainly not the big one without its body.
	e.srv.FailNext("FetchMIME", upstream.ErrUnavailable, -1)
	a := e.login()
	var f *store.Folder
	eventually(t, "the Inbox is known", func() bool {
		var err error
		f, err = a.Store().FolderByPath("INBOX")
		return err == nil
	})
	rec, _ := e.watch(a, f)
	eventually(t, "the small message is staged", func() bool { return e.progress(a, f.ID).Staged == 1 })
	consistently(t, "the folder stays closed", 200*time.Millisecond, func() bool {
		return serverCount(a, f.ID) == 0 && !e.progress(a, f.ID).Complete
	})
	if n, _ := a.Store().PendingCount(0); n != 1 {
		t.Fatalf("pending = %d, want 1", n)
	}
	// Meanwhile a later message flows in; it joins the others, still hidden.
	e.srv.AddMessage(testUser, inbox, mailBytes("small2@x", "later", e.clock.Now(), ""), fakeupstream.AddOptions{Date: e.clock.Now()})
	eventually(t, "the later message is staged too", func() bool { return e.progress(a, f.ID).Staged == 2 })
	if c, _ := a.Store().Counts(f.ID); c.Messages != 0 || c.UIDNext != 1 {
		t.Fatalf("STATUS numbers while closed: %+v", c)
	}

	e.srv.ClearFailures()
	eventually(t, "the folder opens once the big message is fetched", func() bool { return serverCount(a, f.ID) == 3 })
	// UIDs follow the received date, whatever the arrival order was.
	for uid, id := range map[uint32]string{1: "small1@x", 2: "big@x", 3: "small2@x"} {
		if got, _ := a.Store().UIDsByMessageID(f.ID, id); len(got) != 1 || got[0] != uid {
			t.Errorf("%s has UIDs %v, want [%d]", id, got, uid)
		}
	}
	mime, _ := a.Store().MIME(f.ID, 2)
	if !bytes.Equal(mime, big) {
		t.Fatalf("stored MIME differs from the server's (%d vs %d bytes)", len(mime), len(big))
	}
	m, _ := a.Store().Message(f.ID, 2)
	if m.Size != int64(len(big)) {
		t.Errorf("size = %d, want %d (RFC822.SIZE must be the real size)", m.Size, len(big))
	}
	// One announcement for the whole folder, no per-message additions.
	waitBulk(t, rec)
	if rec.count(store.EvBulkAdd) != 1 || rec.count(store.EvAdd) != 0 {
		t.Errorf("events: %d bulk, %d add", rec.count(store.EvBulkAdd), rec.count(store.EvAdd))
	}
	if n, _ := a.Store().PendingCount(0); n != 0 {
		t.Errorf("pending = %d", n)
	}
}

func TestItemDeletedBeforeItsFetchIsDropped(t *testing.T) {
	e := newEnv(t)
	inbox := e.srv.FolderID(testUser, "Inbox")
	big := append(mailBytes("gone@x", "big", e.clock.Now(), ""), bytes.Repeat([]byte("x"), 10000)...)
	id := e.srv.AddMessage(testUser, inbox, big, fakeupstream.AddOptions{})
	e.srv.FailNext("FetchMIME", upstream.ErrUnavailable, -1)
	a := e.login()
	eventually(t, "item parked as pending", func() bool { n, _ := a.Store().PendingCount(0); return n == 1 })
	e.srv.DeleteMessage(testUser, inbox, id)
	e.srv.ClearFailures()
	eventually(t, "pending item dropped", func() bool { n, _ := a.Store().PendingCount(0); return n == 0 })
	f := e.folder(a, "INBOX")
	consistently(t, "nothing was resurrected", 100*time.Millisecond, func() bool { return serverCount(a, f.ID) == 0 })
}

func TestReplayOfACommittedBatchDoesNotDuplicate(t *testing.T) {
	// At-least-once: if the committed key is older than the committed items
	// (crash between "items stored" and "key stored" cannot happen because both
	// are one transaction, but a restored backup or an old key can look like
	// that), the server replays the batch and ServerId de-duplication keeps the
	// UIDs stable.
	e := newEnv(t)
	inbox := e.srv.FolderID(testUser, "Inbox")
	for i := 0; i < 4; i++ {
		e.srv.AddMessage(testUser, inbox, mailBytes(fmt.Sprintf("r%d@x", i), "s", e.clock.Now().Add(time.Duration(i)*time.Minute), ""), fakeupstream.AddOptions{})
	}
	a := e.login()
	f := e.mirrored(a, "Inbox", "INBOX")
	before, _ := a.Store().ListMessages(f.ID)

	// Rewind the stored key to the bootstrap key of the folder (as if the last
	// commit had not happened), then let the server replay everything.
	e.srv.ResetFolder(testUser, inbox) // makes the next Sync a Reset + full resync
	e.srv.AddMessage(testUser, inbox, mailBytes("r9@x", "late", e.clock.Now().Add(time.Hour), ""), fakeupstream.AddOptions{})
	eventually(t, "resync completed", func() bool {
		cur, _ := a.Store().FolderByID(f.ID)
		return serverCount(a, f.ID) == 5 && cur != nil && !cur.ReconActive
	})
	after, _ := a.Store().ListMessages(f.ID)
	for i := range before {
		if after[i].UID != before[i].UID {
			t.Fatalf("UIDs changed by the resync: %v -> %v", uidsOf(before), uidsOf(after))
		}
	}
	if after[len(after)-1].UID != 5 {
		t.Fatalf("new message UID = %d, want 5", after[len(after)-1].UID)
	}
	cur, _ := a.Store().FolderByID(f.ID)
	if cur.UIDValidity != f.UIDValidity {
		t.Fatalf("UIDVALIDITY changed by a resync: %d -> %d", f.UIDValidity, cur.UIDValidity)
	}
}

func TestSilentKeyResetReconcilesDeletionsWithTwoConfirmations(t *testing.T) {
	// The verification resync starts at the first look after the delay (whole seconds of the
	// clock) and the suspicion is visible only until then; with the harness's 200 ms the
	// window can be shorter than the test's polling. An hour makes it last until the test
	// moves the clock.
	e := newEnv(t, func(c *engine.Config, _ *fakeupstream.Server) { c.ReconVerifyDelay = time.Hour })
	inbox := e.srv.FolderID(testUser, "Inbox")
	ids := map[string]string{}
	for _, n := range []string{"keep1", "keep2", "gone"} {
		ids[n] = e.srv.AddMessage(testUser, inbox, mailBytes(n+"@x", n, e.clock.Now(), ""), fakeupstream.AddOptions{})
	}
	a := e.login()
	f := e.mirrored(a, "Inbox", "INBOX")
	rec, _ := e.watch(a, f)
	goneUIDs, _ := a.Store().UIDsByMessageID(f.ID, "gone@x")

	// While the sync state is lost on the server, "gone" is deleted: no Delete
	// command can ever arrive for it.
	e.srv.DeleteMessage(testUser, inbox, ids["gone"])
	e.srv.ResetFolder(testUser, inbox)

	// First complete resync: suspected, still visible.
	eventually(t, "first resync finished with a suspicion", func() bool {
		cur, _ := a.Store().FolderByID(f.ID)
		if cur == nil || cur.ReconActive || cur.ReconVerifyAt == 0 {
			return false
		}
		return true
	})
	// The second, verifying resync removes it.
	e.clock.Advance(time.Hour + time.Second)
	eventually(t, "second confirmation removes the message", func() bool { return serverCount(a, f.ID) == 2 })
	if got := rec.uids(store.EvExpunge); len(got) != 1 || got[0] != goneUIDs[0] {
		t.Fatalf("expunge events %v, want [%d]", got, goneUIDs[0])
	}
	cur, _ := a.Store().FolderByID(f.ID)
	if cur.UIDValidity != f.UIDValidity {
		t.Fatalf("UIDVALIDITY changed (%d -> %d)", f.UIDValidity, cur.UIDValidity)
	}
	if cur.ReconVerifyAt != 0 || cur.ReconActive {
		t.Fatalf("reconciliation did not settle: %+v", cur)
	}
	// The survivors kept their UIDs.
	for _, n := range []string{"keep1", "keep2"} {
		if uids, _ := a.Store().UIDsByMessageID(f.ID, n+"@x"); len(uids) != 1 {
			t.Errorf("%s lost", n)
		}
	}
}

func TestMessageReappearingAfterResetIsNotDeleted(t *testing.T) {
	// Reset with nothing deleted: every message is listed again, nobody gets a
	// strike, no verification resync is needed.
	e := newEnv(t)
	inbox := e.srv.FolderID(testUser, "Inbox")
	for i := 0; i < 3; i++ {
		e.srv.AddMessage(testUser, inbox, mailBytes(fmt.Sprintf("a%d@x", i), "s", e.clock.Now(), ""), fakeupstream.AddOptions{})
	}
	a := e.login()
	f := e.mirrored(a, "Inbox", "INBOX")
	e.srv.ResetFolder(testUser, inbox)
	eventually(t, "resync finished", func() bool {
		cur, _ := a.Store().FolderByID(f.ID)
		return cur != nil && !cur.ReconActive && cur.ReconEpoch > 0
	})
	cur, _ := a.Store().FolderByID(f.ID)
	if cur.ReconVerifyAt != 0 {
		t.Errorf("a verification resync was scheduled although nothing is missing")
	}
	if serverCount(a, f.ID) != 3 {
		t.Errorf("messages lost")
	}
}

func TestServerFlagChangesDoNotOverrideQueuedLocalChanges(t *testing.T) {
	e := newEnv(t, func(c *engine.Config, s *fakeupstream.Server) {})
	inbox := e.srv.FolderID(testUser, "Inbox")
	id := e.srv.AddMessage(testUser, inbox, mailBytes("f1@x", "s", e.clock.Now(), ""), fakeupstream.AddOptions{})
	a := e.login()
	f := e.mirrored(a, "Inbox", "INBOX")
	// Make Exchange refuse our flag change for a moment (transient status), so
	// the local change stays queued while the server reports the old state.
	e.srv.RejectChangesStatus = 5
	if _, err := a.SetFlags(f.ID, []uint32{1}, engine.FlagAdd, store.FlagSeen, nil, "sess"); err != nil {
		t.Fatal(err)
	}
	no := false
	e.srv.SetFlags(testUser, inbox, id, &no, &no) // server-side change forces a Sync
	time.Sleep(100 * time.Millisecond)
	m, _ := a.Store().Message(f.ID, 1)
	if !m.Flags.Has(store.FlagSeen) {
		t.Fatalf("a queued local \\Seen was overwritten by the server's older state")
	}
	e.srv.RejectChangesStatus = 0
	eventually(t, "the queued change reaches Exchange", func() bool {
		for _, it := range e.srv.Items(testUser, inbox) {
			if it.ID == id && it.Read {
				return true
			}
		}
		return false
	})
}

func TestApplyChangesServerCommandsAreNotLost(t *testing.T) {
	// go-activesync patch 7: a client-change Sync can carry server changes in
	// its response; the engine must apply them (the key already moved on).
	e := newEnv(t, func(c *engine.Config, s *fakeupstream.Server) { s.ApplyReturnsServerChanges = true })
	inbox := e.srv.FolderID(testUser, "Inbox")
	e.srv.AddMessage(testUser, inbox, mailBytes("one@x", "one", e.clock.Now(), ""), fakeupstream.AddOptions{})
	a := e.login()
	f := e.mirrored(a, "Inbox", "INBOX")
	e.srv.FailNext("Ping", upstream.ErrUnavailable, -1) // only the apply response may deliver the new mail
	e.srv.AddMessage(testUser, inbox, mailBytes("two@x", "two", e.clock.Now(), ""), fakeupstream.AddOptions{})
	if _, err := a.SetFlags(f.ID, []uint32{1}, engine.FlagAdd, store.FlagSeen, nil, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "message delivered inside the ApplyChanges response", func() bool { return serverCount(a, f.ID) == 2 })
}

func TestFolderRemovalAndRenameFollowTheServer(t *testing.T) {
	e := newEnv(t)
	x := e.srv.AddFolder(testUser, "0", "Temp", fakeupstream.TypeMail)
	e.srv.AddMessage(testUser, x, mailBytes("t1@x", "s", e.clock.Now(), ""), fakeupstream.AddOptions{})
	a := e.login()
	e.mirrored(a, "Temp", "Temp")
	e.srv.RenameFolder(testUser, x, "Temp2")
	// The hierarchy is re-read when Ping reports an unknown folder, or at the
	// next explicit refresh; provoke it by a hierarchy reset.
	e.srv.ResetHierarchy(testUser)
	e.srv.AddMessage(testUser, e.srv.FolderID(testUser, "Inbox"), mailBytes("t2@x", "s", e.clock.Now(), ""), fakeupstream.AddOptions{})
	eventually(t, "rename picked up", func() bool {
		_, err := a.Store().FolderByPath("Temp2")
		return err == nil
	})
	f2, _ := a.Store().FolderByPath("Temp2")
	if serverCount(a, f2.ID) != 1 {
		t.Fatalf("renamed folder lost its messages")
	}
	e.srv.RemoveFolder(testUser, x)
	e.srv.ResetHierarchy(testUser)
	e.srv.AddMessage(testUser, e.srv.FolderID(testUser, "Inbox"), mailBytes("t3@x", "s", e.clock.Now(), ""), fakeupstream.AddOptions{})
	eventually(t, "folder removal picked up", func() bool {
		_, err := a.Store().FolderByPath("Temp2")
		return err != nil
	})
	if !strings.Contains(e.logs.String(), "folder sync done") {
		t.Errorf("no folder sync log line")
	}
}

func TestReplayFromAnOlderKeyKeepsUIDsStable(t *testing.T) {
	// The committed key is older than the committed items (what a restored
	// backup, or a crash with a non-atomic store, would leave). The server
	// replays the batch; ServerId de-duplication makes it harmless.
	e := newEnv(t, func(c *engine.Config, s *fakeupstream.Server) { s.Window = 100 })
	inbox := e.srv.FolderID(testUser, "Inbox")
	for i := 0; i < 4; i++ {
		e.srv.AddMessage(testUser, inbox, mailBytes(fmt.Sprintf("o%d@x", i), "s", e.clock.Now().Add(time.Duration(i)*time.Minute), ""), fakeupstream.AddOptions{})
	}
	a := e.login()
	f := e.mirrored(a, "Inbox", "INBOX")
	before, _ := a.Store().ListMessages(f.ID)
	cur, _ := a.Store().FolderByID(f.ID)
	if cur.SyncKey != inbox+"-k2" {
		t.Fatalf("unexpected key %q (the test assumes one bootstrap and one data batch)", cur.SyncKey)
	}
	rec, _ := e.watch(a, f)
	if _, err := a.Store().Update(nil, func(tx *store.Tx) error { return tx.SetSyncKey(f.ID, inbox+"-k1") }); err != nil {
		t.Fatal(err)
	}
	e.srv.AddMessage(testUser, inbox, mailBytes("o9@x", "fresh", e.clock.Now().Add(time.Hour), ""), fakeupstream.AddOptions{})
	eventually(t, "replayed batch plus the new message", func() bool { return serverCount(a, f.ID) == 5 })
	after, _ := a.Store().ListMessages(f.ID)
	for i := range before {
		if after[i].UID != before[i].UID {
			t.Fatalf("UIDs changed by the replay: %v -> %v", uidsOf(before), uidsOf(after))
		}
	}
	if got := rec.uids(store.EvAdd); len(got) != 1 || got[0] != 5 {
		t.Fatalf("only the new message may be announced, got %v", got)
	}
	if rec.count(store.EvExpunge) != 0 {
		t.Fatalf("the replay expunged something")
	}
}

func TestSyncStatus12RefreshesTheHierarchyThenSyncs(t *testing.T) {
	e := newEnv(t)
	inbox := e.srv.FolderID(testUser, "Inbox")
	e.srv.AddMessage(testUser, inbox, mailBytes("h1@x", "s", e.clock.Now(), ""), fakeupstream.AddOptions{})
	e.srv.FailNext("Sync", &upstream.StatusError{Op: "Sync", Code: 12, Reason: "hierarchy changed"}, 1)
	a := e.login()
	e.mirrored(a, "Inbox", "INBOX")
	if n := e.srv.Calls("FolderSync"); n < 2 {
		t.Fatalf("FolderSync ran %d times, want a refresh after status 12", n)
	}
}

func TestPingStatus6ShrinksTheWatchedSet(t *testing.T) {
	e := newEnv(t, func(c *engine.Config, s *fakeupstream.Server) { s.MaxPingFolders = 2 })
	a := e.login()
	f := e.mirrored(a, "Inbox", "INBOX")
	rec, _ := e.watch(a, f)
	// The engine learns of the limit when its first Ping over every mail folder
	// is answered with status 6; only then is the message added, so that it can
	// reach the engine through the limited Ping alone.
	eventually(t, "the server's Ping folder limit is noticed and logged", func() bool {
		return strings.Contains(e.logs.String(), "limits the number of Ping folders")
	})
	e.srv.AddMessage(testUser, e.srv.FolderID(testUser, "Inbox"), mailBytes("six@x", "s", e.clock.Now(), ""), fakeupstream.AddOptions{})
	eventually(t, "push still works with a limited folder set", func() bool { return rec.count(store.EvAdd) == 1 })
}

func TestStatusShowsWhetherFoldersStillWaitForTheirSync(t *testing.T) {
	// /healthz tells the operator when the initial backfill is done, so that the
	// account is added to MailFlow only then.
	e := newEnv(t)
	inbox := e.srv.FolderID(testUser, "Inbox")
	for i := 0; i < 5; i++ {
		e.srv.AddMessage(testUser, inbox, mailBytes(fmt.Sprintf("init%d@x", i), "s", e.clock.Now(), ""), fakeupstream.AddOptions{})
	}
	e.srv.FailNext("Sync", upstream.ErrUnavailable, -1)
	a := e.login()
	syncing := func() int {
		st := e.mgr.Status()
		if len(st.Accounts) != 1 {
			return -1
		}
		return st.Accounts[0].Syncing
	}
	eventually(t, "folders wait for a sync that keeps failing", func() bool { return syncing() > 0 })
	consistently(t, "the waiting folders are still reported", 150*time.Millisecond, func() bool { return syncing() > 0 })
	e.srv.ClearFailures()
	f := e.mirrored(a, "Inbox", "INBOX")
	eventually(t, "nothing waits any more", func() bool { return syncing() == 0 })
	if got := serverCount(a, f.ID); got != 5 {
		t.Errorf("inbox holds %d messages, want 5", got)
	}
}

func TestPingFailingFallsBackToPeriodicSweeps(t *testing.T) {
	// A proxy that cuts every held request, even at the shortest heartbeat,
	// leaves Ping useless: the engine must still find new mail, by syncing every
	// folder periodically, and must go back to push when Ping works again.
	e := newEnv(t)
	a := e.login()
	f := e.mirrored(a, "Inbox", "INBOX")
	rec, _ := e.watch(a, f)
	e.srv.FailNext("Ping", upstream.ErrUnavailable, -1)
	eventually(t, "the engine gives up on push", func() bool {
		return strings.Contains(e.logs.String(), "Ping keeps failing")
	})
	e.srv.AddMessage(testUser, e.srv.FolderID(testUser, "Inbox"), mailBytes("sweep@x", "found without Ping", e.clock.Now(), ""), fakeupstream.AddOptions{})
	e.clock.Advance(10 * time.Minute) // past the sweep interval
	eventually(t, "new mail found by a sweep", func() bool { return rec.count(store.EvAdd) == 1 })

	e.srv.ClearFailures()
	eventually(t, "push resumes", func() bool { return strings.Contains(e.logs.String(), "Ping works again") })
	e.srv.AddMessage(testUser, e.srv.FolderID(testUser, "Inbox"), mailBytes("push@x", "found by Ping", e.clock.Now(), ""), fakeupstream.AddOptions{})
	eventually(t, "new mail via Ping again", func() bool { return rec.count(store.EvAdd) == 2 })
}

// The fake Exchange dates a message without a date with the test clock, so that
// no test outcome depends on the time of day. (A test that relied on the wall
// clock being earlier than the fixed start of the test clock began to fail at
// midday, when the two orders swapped.)
func TestTheFakeExchangeDatesMessagesWithTheTestClock(t *testing.T) {
	e := newEnv(t)
	inbox := e.srv.FolderID(testUser, "Inbox")
	e.srv.AddMessage(testUser, inbox, mailBytes("nodate@x", "s", e.clock.Now(), ""), fakeupstream.AddOptions{})
	a := e.login()
	f := e.mirrored(a, "Inbox", "INBOX")
	m, err := a.Store().Message(f.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if d := m.InternalDate.Sub(e.clock.Now()); d > 30*time.Second || d < -30*time.Second {
		t.Fatalf("received %v, the test clock says %v: the fake server used another clock", m.InternalDate, e.clock.Now())
	}
}
