package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func stagedMsg(id, serverID string, date time.Time, extra string) NewMessage {
	return NewMessage{ServerID: serverID, InternalDate: date, MIME: rawMsg(id, "subject "+id, extra)}
}

// inboxOf applies the standard tree and returns the Inbox.
func inboxOf(t *testing.T, a *Account) *Folder {
	t.Helper()
	fs := applyStandard(t, a)
	f := fs["1"] // the Inbox of standardTree
	if f == nil {
		t.Fatalf("no Inbox in %v", fs)
	}
	if f.InitialComplete || f.InitialChainDone {
		t.Fatalf("a new folder must start closed: %+v", f)
	}
	return f
}

func TestStagedMessagesAreInvisibleToEveryRead(t *testing.T) {
	_, a := openTemp(t)
	inbox := inboxOf(t, a)
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if _, err := a.Update(nil, func(tx *Tx) error {
		for i := 0; i < 3; i++ {
			nm := stagedMsg(fmt.Sprintf("s%d@x", i), fmt.Sprintf("srv%d", i), base.Add(time.Duration(i)*time.Hour),
				"In-Reply-To: <parent@x>\r\nReferences: <root@x> <parent@x>\r\n")
			if err := tx.AddStagedMessage(inbox.ID, nm); err != nil {
				return err
			}
		}
		// One visible message (moved in by a client, say).
		_, err := tx.AddMessage(inbox.ID, stagedMsg("vis@x", "srvvis", base, ""))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	metas, _ := a.ListMessages(inbox.ID)
	if len(metas) != 1 || metas[0].UID != 1 {
		t.Fatalf("ListMessages = %+v, want only the visible message with UID 1", metas)
	}
	c, err := a.Counts(inbox.ID)
	if err != nil || c.Messages != 1 || c.Unseen != 1 || c.UIDNext != 2 {
		t.Fatalf("Counts = %+v, %v (staged messages must not count and UIDNEXT must not move)", c, err)
	}
	if u, _ := a.UIDsByMessageID(inbox.ID, "s0@x"); len(u) != 0 {
		t.Errorf("UIDsByMessageID found a staged message: %v", u)
	}
	if u, _ := a.UIDsByRef(inbox.ID, RefInReplyTo, "parent@x"); len(u) != 0 {
		t.Errorf("UIDsByRef(In-Reply-To) found staged messages: %v", u)
	}
	if u, _ := a.UIDsByRef(inbox.ID, RefReferences, "root@x"); len(u) != 0 {
		t.Errorf("UIDsByRef(References) found staged messages: %v", u)
	}
	rows, _ := a.SearchRows(inbox.ID)
	if len(rows) != 1 {
		t.Errorf("SearchRows = %d rows, want 1", len(rows))
	}
	for _, uid := range []uint32{0, 1, 2, 4294967295, 4294967294} {
		if uid == 1 {
			continue
		}
		if _, err := a.Message(inbox.ID, uid); err == nil {
			t.Errorf("Message(%d) returned a message", uid)
		}
		if _, err := a.MIME(inbox.ID, uid); err == nil {
			t.Errorf("MIME(%d) returned data", uid)
		}
	}
	if _, err := a.Update(nil, func(tx *Tx) error {
		if ms, _ := tx.ByMessageID(inbox.ID, "s1@x"); len(ms) != 0 {
			t.Errorf("ByMessageID found a staged message")
		}
		if ms, _ := tx.ServerMessages(inbox.ID); len(ms) != 1 {
			t.Errorf("ServerMessages = %d, want 1", len(ms))
		}
		if ms, _ := tx.ByKind(inbox.ID, KindServer); len(ms) != 1 {
			t.Errorf("ByKind = %d, want 1", len(ms))
		}
		if ms, _ := tx.ReconAbsent(inbox.ID, 99); len(ms) != 1 {
			t.Errorf("ReconAbsent = %d, want only the visible message", len(ms))
		}
		// The engine does find staged messages by ServerId, flagged as staged.
		m, err := tx.MessageByServerID(inbox.ID, "srv1")
		if err != nil || !m.Staged || m.UID != 0 {
			t.Errorf("MessageByServerID(staged) = %+v, %v", m, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	prog, _ := a.InitialProgress()
	var got *FolderProgress
	for i := range prog {
		if prog[i].FolderID == inbox.ID {
			got = &prog[i]
		}
	}
	if got == nil || got.Complete || got.Staged != 3 || got.PendingFetch != 0 {
		t.Errorf("InitialProgress = %+v", prog)
	}
}

func TestStagedFlagAndDeleteChangesAreSilent(t *testing.T) {
	_, a := openTemp(t)
	inbox := inboxOf(t, a)
	date := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var stagedPK, visiblePK int64
	if _, err := a.Update(nil, func(tx *Tx) error {
		if err := tx.AddStagedMessage(inbox.ID, stagedMsg("a@x", "a", date, "")); err != nil {
			return err
		}
		m, err := tx.AddMessage(inbox.ID, stagedMsg("b@x", "b", date, ""))
		if err != nil {
			return err
		}
		visiblePK = m.PK
		s, err := tx.MessageByServerID(inbox.ID, "a")
		stagedPK = s.PK
		return err
	}); err != nil {
		t.Fatal(err)
	}
	evs, err := a.Update(nil, func(tx *Tx) error {
		if ch, err := tx.SetFlagsPK(stagedPK, FlagSeen, ""); err != nil || !ch {
			t.Errorf("SetFlagsPK(staged) = %v, %v", ch, err)
		}
		if ch, err := tx.SetFlagsPK(stagedPK, FlagSeen, ""); err != nil || ch {
			t.Errorf("repeating the same flags reported a change: %v, %v", ch, err)
		}
		if ok, err := tx.DeleteByPK(stagedPK); err != nil || !ok {
			t.Errorf("DeleteByPK(staged) = %v, %v", ok, err)
		}
		return nil
	})
	if err != nil || len(evs) != 0 {
		t.Fatalf("staged changes must be silent: events %+v, err %v", evs, err)
	}
	evs, err = a.Update(nil, func(tx *Tx) error {
		if ch, err := tx.SetFlagsPK(visiblePK, FlagSeen|FlagFlagged, "kw"); err != nil || !ch {
			t.Errorf("SetFlagsPK(visible) = %v, %v", ch, err)
		}
		if ok, err := tx.DeleteByPK(visiblePK); err != nil || !ok {
			t.Errorf("DeleteByPK(visible) = %v, %v", ok, err)
		}
		return nil
	})
	if err != nil || len(evs) != 2 || evs[0].Kind != EvFlags || evs[0].UID != 1 || evs[1].Kind != EvExpunge || evs[1].UID != 1 {
		t.Fatalf("visible changes must be announced: %+v, %v", evs, err)
	}
	if ok, err := func() (bool, error) {
		var ok bool
		_, err := a.Update(nil, func(tx *Tx) error {
			var e error
			ok, e = tx.DeleteByPK(987654)
			return e
		})
		return ok, err
	}(); err != nil || ok {
		t.Errorf("deleting an unknown pk: %v, %v", ok, err)
	}
}

func TestCompleteInitialSyncNumbersByDateThenServerID(t *testing.T) {
	_, a := openTemp(t)
	inbox := inboxOf(t, a)
	d := func(h int) time.Time { return time.Date(2026, 10, 1, h, 0, 0, 0, time.UTC) }
	var draftUID, movedUID uint32
	if _, err := a.Update(nil, func(tx *Tx) error {
		// Arrival order is newest first, the way an initial backfill delivers.
		// "b" and "a" share a date: the ServerId breaks the tie.
		for _, s := range []struct {
			id, sid string
			h       int
			flags   Flags
		}{{"m9", "s9", 9, FlagSeen}, {"mb", "b", 5, 0}, {"ma", "a", 5, FlagFlagged}, {"m1", "s1", 1, 0}, {"m7", "s7", 7, FlagSeen | FlagFlagged}} {
			nm := stagedMsg(s.id+"@x", s.sid, d(s.h), "")
			nm.Flags, nm.Keywords = s.flags, "$kw"+s.sid
			if err := tx.AddStagedMessage(inbox.ID, nm); err != nil {
				return err
			}
		}
		// A local draft and a moved-in message get real UIDs at once.
		dr, err := tx.AddMessage(inbox.ID, NewMessage{Kind: KindLocalDraft, MIME: rawMsg("draft@x", "d", "")})
		if err != nil {
			return err
		}
		draftUID = dr.UID
		mv, err := tx.AddMessage(inbox.ID, stagedMsg("moved@x", "moved", d(12), ""))
		movedUID = mv.UID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if draftUID != 1 || movedUID != 2 {
		t.Fatalf("immediate UIDs = %d, %d, want 1, 2", draftUID, movedUID)
	}
	var n int
	evs, err := a.Update(nil, func(tx *Tx) error {
		var err error
		n, err = tx.CompleteInitialSync(inbox.ID)
		return err
	})
	if err != nil || n != 5 {
		t.Fatalf("CompleteInitialSync = %d, %v", n, err)
	}
	if len(evs) != 1 || evs[0].Kind != EvBulkAdd || evs[0].FolderID != inbox.ID || len(evs[0].Bulk) != 5 {
		t.Fatalf("want exactly one EvBulkAdd with 5 messages, got %+v", evs)
	}
	for i, m := range evs[0].Bulk {
		if m.UID != uint32(3+i) {
			t.Errorf("bulk[%d].UID = %d, want %d", i, m.UID, 3+i)
		}
	}
	wantOrder := []string{"s1", "a", "b", "s7", "s9"} // date, then ServerId
	for i, sid := range wantOrder {
		var m *Message
		a.Update(nil, func(tx *Tx) error { var e error; m, e = tx.MessageByServerID(inbox.ID, sid); return e })
		if m == nil || m.UID != uint32(3+i) || m.Staged {
			t.Errorf("%s: %+v, want UID %d", sid, m, 3+i)
		}
		if m != nil && m.Keywords != "$kw"+sid {
			t.Errorf("%s lost its keywords: %q", sid, m.Keywords)
		}
	}
	cur, _ := a.FolderByID(inbox.ID)
	if !cur.InitialComplete || !cur.InitialChainDone || cur.UIDNext != 8 {
		t.Errorf("folder after completion: %+v", cur)
	}
	if c, _ := a.Counts(inbox.ID); c.Messages != 7 || c.UIDNext != 8 {
		t.Errorf("Counts = %+v", c)
	}
	// Local messages kept their UIDs; the MIME of a renumbered message is intact.
	if m, err := a.Message(inbox.ID, draftUID); err != nil || m.Kind != KindLocalDraft {
		t.Errorf("draft: %+v, %v", m, err)
	}
	if mime, err := a.MIME(inbox.ID, 3); err != nil || len(mime) == 0 {
		t.Errorf("MIME of the renumbered message: %d bytes, %v", len(mime), err)
	}
	// A message arriving afterwards gets the next UID, as usual.
	if _, err := a.Update(nil, func(tx *Tx) error {
		m, err := tx.AddMessage(inbox.ID, stagedMsg("late@x", "late", d(2), ""))
		if err == nil && m.UID != 8 {
			t.Errorf("message after completion has UID %d, want 8", m.UID)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Completing again is harmless and silent.
	evs, err = a.Update(nil, func(tx *Tx) error { _, err := tx.CompleteInitialSync(inbox.ID); return err })
	if err != nil || len(evs) != 0 {
		t.Errorf("second completion: events %+v, err %v", evs, err)
	}
}

func TestCompleteInitialSyncOfAnEmptyFolderIsSilent(t *testing.T) {
	_, a := openTemp(t)
	inbox := inboxOf(t, a)
	var n int
	evs, err := a.Update(nil, func(tx *Tx) error {
		var err error
		n, err = tx.CompleteInitialSync(inbox.ID)
		return err
	})
	if err != nil || n != 0 || len(evs) != 0 {
		t.Fatalf("n=%d events=%+v err=%v", n, evs, err)
	}
	if cur, _ := a.FolderByID(inbox.ID); !cur.InitialComplete || cur.UIDNext != 1 {
		t.Errorf("folder: %+v", cur)
	}
}

func TestRestartInitialDropsWhatTheFirstSyncCollected(t *testing.T) {
	_, a := openTemp(t)
	inbox := inboxOf(t, a)
	date := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if _, err := a.Update(nil, func(tx *Tx) error {
		if err := tx.AddStagedMessage(inbox.ID, stagedMsg("a@x", "a", date, "")); err != nil {
			return err
		}
		if err := tx.AddStagedMessage(inbox.ID, stagedMsg("b@x", "b", date, "")); err != nil {
			return err
		}
		if _, err := tx.AddMessage(inbox.ID, stagedMsg("v@x", "v", date, "")); err != nil {
			return err
		}
		if err := tx.UpsertPending(PendingFetch{FolderID: inbox.ID, ServerID: "p", InternalDate: date}); err != nil {
			return err
		}
		return tx.SetInitialChainDone(inbox.ID, true)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Update(nil, func(tx *Tx) error { return tx.RestartInitial(inbox.ID) }); err != nil {
		t.Fatal(err)
	}
	prog, _ := a.InitialProgress()
	for _, p := range prog {
		if p.FolderID == inbox.ID && (p.Staged != 0 || p.PendingFetch != 0) {
			t.Errorf("restart left %+v", p)
		}
	}
	if c, _ := a.Counts(inbox.ID); c.Messages != 1 {
		t.Errorf("the visible message must survive a restart: %+v", c)
	}
	cur, _ := a.FolderByID(inbox.ID)
	if cur.InitialChainDone || cur.InitialComplete {
		t.Errorf("folder after restart: %+v", cur)
	}
	// The provisional counter starts over without clashing with anything.
	if _, err := a.Update(nil, func(tx *Tx) error {
		return tx.AddStagedMessage(inbox.ID, stagedMsg("c@x", "c", date, ""))
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRestartInitialDoesNothingToAnOpenFolder(t *testing.T) {
	_, a := openTemp(t)
	inbox := inboxOf(t, a)
	if _, err := a.Update(nil, func(tx *Tx) error {
		_, err := tx.CompleteInitialSync(inbox.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Update(nil, func(tx *Tx) error {
		return tx.UpsertPending(PendingFetch{FolderID: inbox.ID, ServerID: "late", InternalDate: time.Now()})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Update(nil, func(tx *Tx) error { return tx.RestartInitial(inbox.ID) }); err != nil {
		t.Fatal(err)
	}
	if cur, _ := a.FolderByID(inbox.ID); !cur.InitialComplete || !cur.InitialChainDone {
		t.Errorf("an open folder was reopened: %+v", cur)
	}
	if n, _ := a.PendingCount(inbox.ID); n != 1 {
		t.Errorf("an open folder lost its pending fetches: %d", n)
	}
}

func TestPendingBlockingCountsOnlyItemFailures(t *testing.T) {
	_, a := openTemp(t)
	inbox := inboxOf(t, a)
	date := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	later := time.Now().Add(time.Hour)
	if _, err := a.Update(nil, func(tx *Tx) error {
		for _, sid := range []string{"outage", "broken", "fine"} {
			if err := tx.UpsertPending(PendingFetch{FolderID: inbox.ID, ServerID: sid, InternalDate: date}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	due, _ := a.PendingDue(time.Now(), 10)
	ids := map[string]int64{}
	for _, p := range due {
		ids[p.ServerID] = p.ID
	}
	if _, err := a.Update(nil, func(tx *Tx) error {
		for i := 0; i < 20; i++ {
			// Twenty connection failures: nothing is learned about the item.
			if err := tx.BumpPending(ids["outage"], later, false); err != nil {
				return err
			}
		}
		for i := 0; i < 5; i++ {
			if err := tx.BumpPending(ids["broken"], later, true); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var blocking int
	a.Update(nil, func(tx *Tx) error {
		var err error
		blocking, err = tx.PendingBlocking(inbox.ID, 5)
		return err
	})
	if blocking != 2 { // "outage" and "fine" still hold the folder; "broken" has failed five times by itself
		t.Errorf("blocking = %d, want 2", blocking)
	}
	if n, _ := a.PendingCount(inbox.ID); n != 3 {
		t.Errorf("PendingCount still counts every waiting item: %d", n)
	}
}

func TestBulkNumberingOfAThousandsOfMessagesStaysFast(t *testing.T) {
	_, a := openTemp(t)
	inbox := inboxOf(t, a)
	total := 5000
	if raceEnabled {
		total = 500 // the race detector makes inserts an order of magnitude slower
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := a.Update(nil, func(tx *Tx) error {
		for i := 0; i < total; i++ {
			// Newest first, two messages per second to force ties.
			d := base.Add(time.Duration(total-i) / 2 * time.Second)
			if err := tx.AddStagedMessage(inbox.ID, stagedMsg(fmt.Sprintf("m%d@x", i), fmt.Sprintf("s%05d", i), d, "")); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	var n int
	evs, err := a.Update(nil, func(tx *Tx) error {
		var err error
		n, err = tx.CompleteInitialSync(inbox.ID)
		return err
	})
	took := time.Since(start)
	t.Logf("numbering %d staged messages took %v", n, took)
	if err != nil || n != total || len(evs) != 1 || len(evs[0].Bulk) != total {
		t.Fatalf("n=%d events=%d err=%v", n, len(evs), err)
	}
	if took > 30*time.Second { // generous: typically a fraction of a second
		t.Errorf("bulk numbering took %v", took)
	}
	metas, _ := a.ListMessages(inbox.ID)
	for i, m := range metas {
		if m.UID != uint32(i+1) {
			t.Fatalf("UIDs are not 1..%d at %d: %d", total, i, m.UID)
		}
		if i > 0 && m.InternalDate < metas[i-1].InternalDate {
			t.Fatalf("dates go backwards at UID %d", m.UID)
		}
	}
}

func TestMigrationFromTheFirstSchemaKeepsOpenFoldersOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DBFileName)
	raw, err := sql.Open(driverName, "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := raw.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(migrations[0])
	exec(`PRAGMA user_version = 1`)
	exec(`INSERT INTO accounts(id, user, created_at) VALUES (1, 'u@example.test', 1)`)
	folder := func(id int, key string, uidNext int) {
		exec(`INSERT INTO folders(id, account_id, server_id, display_name, eas_type, path, uid_validity, uid_next, sync_key)
			VALUES (?, 1, ?, ?, 12, ?, 100, ?, ?)`, id, fmt.Sprintf("s%d", id), fmt.Sprintf("F%d", id), fmt.Sprintf("F%d", id), uidNext, key)
	}
	folder(1, "0", 1)  // never synced
	folder(2, "k7", 1) // synced, nothing yet
	folder(3, "0", 1)  // key "0" again (verification resync) but holds a message
	folder(4, "0", 1)  // waiting for a body only
	folder(5, "0", 9)  // handed out UIDs before
	exec(`INSERT INTO messages(id, folder_id, uid, server_id, internal_date, size, header) VALUES (1, 3, 1, 'x', 1, 1, x'00')`)
	exec(`INSERT INTO pending_fetch(folder_id, server_id, internal_date, created_at) VALUES (4, 'y', 1, 1)`)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(dir)
	if err != nil {
		t.Fatalf("opening a v1 database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if v, _ := db.SchemaVersion(); v != len(migrations) {
		t.Errorf("user_version = %d, want %d", v, len(migrations))
	}
	acct, err := db.Account("u@example.test")
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]bool{1: false, 2: true, 3: true, 4: true, 5: true}
	for id, complete := range want {
		f, err := acct.FolderByID(id)
		if err != nil {
			t.Fatal(err)
		}
		if f.InitialComplete != complete || f.InitialChainDone != complete {
			t.Errorf("folder %d: complete=%v chain=%v, want %v", id, f.InitialComplete, f.InitialChainDone, complete)
		}
	}
	// The migrated database stages and numbers like a fresh one.
	if _, err := acct.Update(nil, func(tx *Tx) error {
		if err := tx.AddStagedMessage(1, stagedMsg("z@x", "z", time.Now(), "")); err != nil {
			return err
		}
		n, err := tx.CompleteInitialSync(1)
		if err == nil && n != 1 {
			t.Errorf("numbered %d", n)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
