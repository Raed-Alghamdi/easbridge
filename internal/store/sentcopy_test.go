package store

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The copies the bridge keeps of the messages it submitted (v0.1.8, schema 5).

func keptCount(t *testing.T, acct *Account) (int, int64) {
	t.Helper()
	n, bytes, err := acct.SentCopies()
	if err != nil {
		t.Fatal(err)
	}
	return n, bytes
}

func TestSentCopyIsKeptReplacedReadAndDropped(t *testing.T) {
	_, acct := openTemp(t)
	raw1 := rawMsg("kept1@x", "first", "Bcc: eve@example.test\r\n")
	raw2 := rawMsg("kept1@x", "first, rebuilt", "Bcc: eve@example.test\r\n")
	if _, err := acct.Update(nil, func(tx *Tx) error {
		if err := tx.KeepSentCopy("kept1@x", raw1, time.Hour, 10, 1<<20); err != nil {
			return err
		}
		got, err := tx.SentCopy("kept1@x")
		if err != nil || !bytes.Equal(got, raw1) {
			t.Fatalf("read back: %q %v", got, err)
		}
		// The same Message-ID again replaces the bytes (a retried submission).
		if err := tx.KeepSentCopy("kept1@x", raw2, time.Hour, 10, 1<<20); err != nil {
			return err
		}
		got, _ = tx.SentCopy("kept1@x")
		if !bytes.Equal(got, raw2) {
			t.Fatalf("not replaced: %q", got)
		}
		if _, err := tx.SentCopy("other@x"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown Message-ID: %v", err)
		}
		if _, err := tx.SentCopy(""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("empty Message-ID: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n, size := keptCount(t, acct); n != 1 || size != int64(len(raw2)) {
		t.Fatalf("kept %d copies of %d bytes", n, size)
	}
	if _, err := acct.Update(nil, func(tx *Tx) error {
		if err := tx.KeepSentCopy("", raw1, time.Hour, 10, 1<<20); err == nil {
			t.Error("a copy without a Message-ID was accepted")
		}
		if err := tx.KeepSentCopy("x@x", nil, time.Hour, 10, 1<<20); err == nil {
			t.Error("an empty copy was accepted")
		}
		if err := tx.DropSentCopy("nothing@x"); err != nil {
			t.Errorf("dropping what is not there: %v", err)
		}
		return tx.DropSentCopy("kept1@x")
	}); err != nil {
		t.Fatal(err)
	}
	if n, _ := keptCount(t, acct); n != 0 {
		t.Fatalf("%d copies left after the drop", n)
	}
}

func TestSentCopiesAreBoundedByAgeCountAndSize(t *testing.T) {
	db, acct := openTemp(t)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(now.Unix())
	db.SetClock(func() time.Time { return time.Unix(clock.Load(), 0) })
	keep := func(id string, size int, ttl time.Duration, maxN int, maxBytes int64) {
		t.Helper()
		raw := append(rawMsg(id, "s", "Bcc: x@example.test\r\n"), bytes.Repeat([]byte("x"), size)...)
		if _, err := acct.Update(nil, func(tx *Tx) error { return tx.KeepSentCopy(id, raw, ttl, maxN, maxBytes) }); err != nil {
			t.Fatal(err)
		}
	}
	has := func(id string) bool {
		t.Helper()
		var ok bool
		if _, err := acct.Update(nil, func(tx *Tx) error {
			_, err := tx.SentCopy(id)
			ok = err == nil
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	ids := func() string {
		var out []string
		for _, id := range []string{"a@x", "b@x", "c@x", "d@x", "e@x"} {
			if has(id) {
				out = append(out, id[:1])
			}
		}
		return strings.Join(out, "")
	}

	t.Run("age", func(t *testing.T) {
		keep("a@x", 10, time.Hour, 10, 1<<20)
		clock.Add(40 * 60)
		keep("b@x", 10, time.Hour, 10, 1<<20)
		clock.Add(30 * 60) // a is 70 minutes old, b is 30
		keep("c@x", 10, time.Hour, 10, 1<<20)
		if got := ids(); got != "bc" {
			t.Fatalf("after the age limit: %q, want bc", got)
		}
		var n int
		clock.Add(2 * 3600) // housekeeping, later
		if _, err := acct.Update(nil, func(tx *Tx) error {
			var err error
			n, err = tx.PruneSentCopies(time.Hour, 10, 1<<20)
			return err
		}); err != nil || n != 2 {
			t.Fatalf("housekeeping pruned %d (%v), want 2", n, err)
		}
		if got := ids(); got != "" {
			t.Fatalf("copies left: %q", got)
		}
	})
	t.Run("count: the oldest go first", func(t *testing.T) {
		for _, id := range []string{"a@x", "b@x", "c@x", "d@x"} {
			keep(id, 10, time.Hour, 3, 1<<20)
			clock.Add(1)
		}
		if got := ids(); got != "bcd" {
			t.Fatalf("with at most 3: %q, want bcd", got)
		}
	})
	t.Run("size: the oldest go first and the newest always stays", func(t *testing.T) {
		if _, err := acct.Update(nil, func(tx *Tx) error {
			_, err := tx.PruneSentCopies(0, 0, 0) // limits off: nothing goes
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if got := ids(); got != "bcd" {
			t.Fatalf("limits off: %q", got)
		}
		sizeOf := func(id string, pad int) int64 {
			return int64(len(rawMsg(id, "s", "Bcc: x@example.test\r\n")) + pad)
		}
		eSize, aSize, bSize, cSize := sizeOf("e@x", 5000), sizeOf("a@x", 1000), sizeOf("b@x", 100), sizeOf("c@x", 100)
		keep("e@x", 5000, time.Hour, 10, eSize-1) // alone bigger than the limit
		if got := ids(); got != "e" {
			t.Fatalf("one copy bigger than the byte limit: %q, want e alone (the newest always stays)", got)
		}
		clock.Add(1)
		keep("a@x", 1000, time.Hour, 10, eSize+aSize)
		clock.Add(1)
		keep("b@x", 100, time.Hour, 10, eSize+aSize+bSize)
		if got := ids(); got != "abe" { // ids lists in alphabetical order
			t.Fatalf("within the byte limit: %q, want abe", got)
		}
		clock.Add(1)
		// c fits with b and a, but not with e as well: e, the oldest, goes.
		keep("c@x", 100, time.Hour, 10, aSize+bSize+cSize)
		if got := ids(); got != "abc" {
			t.Fatalf("after the byte limit: %q, want abc", got)
		}
		clock.Add(1)
		// Once something is dropped everything older goes with it, even a small copy
		// that would still fit: the oldest go first.
		keep("d@x", 100, time.Hour, 10, sizeOf("d@x", 100)+cSize+bSize-1)
		if got := ids(); got != "cd" {
			t.Fatalf("after the next byte limit: %q, want cd (b and everything older goes)", got)
		}
		if n, size := keptCount(t, acct); n < 1 || size > sizeOf("d@x", 100)+cSize+bSize {
			t.Fatalf("%d copies of %d bytes are over the limit", n, size)
		}
	})
}

func TestDeletingASentMessageForgetsItsKeptCopy(t *testing.T) {
	_, acct := openTemp(t)
	folders := applyStandard(t, acct)
	sent, inbox := folders["4"], folders["1"]
	keep := func(ids ...string) {
		t.Helper()
		if _, err := acct.Update(nil, func(tx *Tx) error {
			for _, id := range ids {
				if err := tx.KeepSentCopy(id, rawMsg(id, "s", "Bcc: x@example.test\r\n"), time.Hour, 10, 1<<20); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	add := func(folder *Folder, id string, staged bool) *Message {
		t.Helper()
		var m *Message
		if _, err := acct.Update(nil, func(tx *Tx) error {
			nm := NewMessage{ServerID: "srv:" + id, MIME: rawMsg(id, "s", "")}
			if staged {
				return tx.AddStagedMessage(folder.ID, nm)
			}
			var err error
			m, err = tx.AddMessage(folder.ID, nm)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return m
	}
	keep("p@x", "q@x", "r@x", "s@x")
	pm := add(sent, "p@x", false)
	add(sent, "q@x", false)
	add(sent, "r@x", true)
	im := add(inbox, "s@x", false) // the same Message-ID in the Inbox is not a Sent item

	del := func(f func(tx *Tx) error) {
		t.Helper()
		if _, err := acct.Update(nil, f); err != nil {
			t.Fatal(err)
		}
	}
	// An expunge, or the removal of a message that moved out of Sent Items.
	del(func(tx *Tx) error { _, err := tx.DeleteMessage(sent.ID, pm.UID); return err })
	if n, _ := keptCount(t, acct); n != 3 {
		t.Fatalf("%d copies kept after one delete, want 3", n)
	}
	// A deletion on the server, found by primary key; a staged message too.
	del(func(tx *Tx) error {
		m, err := tx.MessageByServerID(sent.ID, "srv:q@x")
		if err != nil {
			return err
		}
		_, err = tx.DeleteByPK(m.PK)
		return err
	})
	del(func(tx *Tx) error {
		m, err := tx.MessageByServerID(sent.ID, "srv:r@x")
		if err != nil {
			return err
		}
		_, err = tx.DeleteByPK(m.PK)
		return err
	})
	if n, _ := keptCount(t, acct); n != 1 {
		t.Fatalf("%d copies kept, want only the one whose message is in the Inbox", n)
	}
	// The Inbox message with that Message-ID leaving does not touch it.
	del(func(tx *Tx) error { _, err := tx.DeleteMessage(inbox.ID, im.UID); return err })
	if n, _ := keptCount(t, acct); n != 1 {
		t.Fatalf("an Inbox message took a copy with it: %d kept", n)
	}
	// Deleting something that is not there is not an error and changes nothing.
	del(func(tx *Tx) error {
		ok, err := tx.DeleteMessage(sent.ID, 999)
		if ok || err != nil {
			t.Errorf("DeleteMessage of nothing: %v %v", ok, err)
		}
		ok, err = tx.DeleteByPK(123456)
		if ok || err != nil {
			t.Errorf("DeleteByPK of nothing: %v %v", ok, err)
		}
		return nil
	})
	if n, _ := keptCount(t, acct); n != 1 {
		t.Fatalf("%d copies kept", n)
	}
}

func TestReplaceMIMERefreshesWhatIsIndexedAndAnnouncesOnlyTheSize(t *testing.T) {
	_, acct := openTemp(t)
	sent := applyStandard(t, acct)["4"]
	old := rawMsg("rep@x", "Old subject", "In-Reply-To: <parent@x>\r\nReferences: <root@x> <parent@x>\r\n")
	renewed := []byte("Message-ID: <rep@x>\r\nSubject: New subject\r\nDate: Tue, 06 Oct 2026 10:00:00 +0000\r\nTo: bob@example.test\r\n" +
		"Bcc: eve@example.test\r\nIn-Reply-To: <other@x>\r\n\r\nthe same message, with its Bcc recipients\r\n")
	var m *Message
	if _, err := acct.Update(nil, func(tx *Tx) error {
		var err error
		m, err = tx.AddMessage(sent.ID, NewMessage{ServerID: "5:1", Flags: FlagSeen | FlagFlagged, Keywords: "$k", MIME: old})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := acct.UIDsByRef(sent.ID, RefReferences, "root"+"@x"); len(got) != 1 {
		t.Fatalf("setup: References index %v", got)
	}
	evs, err := acct.Update("sess", func(tx *Tx) error { return tx.ReplaceMIME(m.PK, renewed) })
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Kind != EvResize || evs[0].UID != m.UID || evs[0].Size != int64(len(renewed)) || evs[0].FolderID != sent.ID {
		t.Fatalf("events %+v: want one EvResize for UID %d with the new size", evs, m.UID)
	}
	got, err := acct.Message(sent.ID, m.UID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ServerID != "5:1" || got.Flags != FlagSeen|FlagFlagged || got.Keywords != "$k" || got.UID != m.UID || got.Kind != KindServer {
		t.Errorf("identity changed: %+v", got)
	}
	if got.Size != int64(len(renewed)) || got.Subject != "New subject" || !bytes.Contains(got.Header, []byte("Bcc: eve@example.test")) {
		t.Errorf("not refreshed: size %d subject %q header %q", got.Size, got.Subject, got.Header)
	}
	if mime, _ := acct.MIME(sent.ID, m.UID); !bytes.Equal(mime, renewed) {
		t.Errorf("MIME not replaced")
	}
	if blob, _ := acct.Structure(sent.ID, m.UID); !strings.Contains(string(blob), `"s":"New subject"`) || !strings.Contains(string(blob), `"m":"eve"`) {
		t.Errorf("structure index not rebuilt: %s", blob)
	}
	if u, _ := acct.UIDsByRef(sent.ID, RefReferences, "root@x"); len(u) != 0 {
		t.Errorf("the old References token is still indexed: %v", u)
	}
	if u, _ := acct.UIDsByRef(sent.ID, RefInReplyTo, "other@x"); len(u) != 1 {
		t.Errorf("the new In-Reply-To token is not indexed: %v", u)
	}
	if c, _ := acct.Counts(sent.ID); c.Messages != 1 || c.UIDNext != m.UID+1 {
		t.Errorf("counts %+v: replacing must not add a message or move UIDNEXT", c)
	}

	// A staged message is replaced silently: nobody has heard of it.
	var stagedPK int64
	if _, err := acct.Update(nil, func(tx *Tx) error {
		if err := tx.AddStagedMessage(sent.ID, NewMessage{ServerID: "5:2", MIME: rawMsg("st@x", "staged", "")}); err != nil {
			return err
		}
		sm, err := tx.MessageByServerID(sent.ID, "5:2")
		stagedPK = sm.PK
		return err
	}); err != nil {
		t.Fatal(err)
	}
	evs, err = acct.Update(nil, func(tx *Tx) error {
		return tx.ReplaceMIME(stagedPK, rawMsg("st@x", "staged again", "Bcc: z@example.test\r\n"))
	})
	if err != nil || len(evs) != 0 {
		t.Fatalf("a staged message: events %+v, err %v", evs, err)
	}
	// Refusals.
	if _, err := acct.Update(nil, func(tx *Tx) error { return tx.ReplaceMIME(m.PK, nil) }); err == nil {
		t.Error("an empty message was accepted")
	}
	if _, err := acct.Update(nil, func(tx *Tx) error { return tx.ReplaceMIME(987654, renewed) }); !errors.Is(err, ErrNotFound) {
		t.Errorf("a message that does not exist: %v", err)
	}
}

func TestMigration5AddsTheTableAndKeepsOlderRows(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open(driverName, "file:"+filepath.Join(dir, DBFileName))
	if err != nil {
		t.Fatal(err)
	}
	// A database as v0.1.7 left it: schema version 4, a waiting item with its metadata.
	for i := 0; i < 4; i++ {
		if _, err := raw.Exec(migrations[i]); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`PRAGMA user_version = 4`,
		`INSERT INTO accounts(id, user, created_at) VALUES (1, 'a@x.test', 0)`,
		`INSERT INTO folders(id, account_id, server_id, display_name, eas_type, path, uid_validity) VALUES (1, 1, '5', 'Sent Items', 5, 'Sent Items', 1)`,
		`INSERT INTO pending_fetch(folder_id, server_id, flags, internal_date, est_size, created_at, subject, from_addr, to_addr)
		   VALUES (1, '5:2', 1, 1000, 77, 0, 'kept subject', 'me@x.test', 'you@x.test')`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	raw.Close()

	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if v, _ := db.SchemaVersion(); v != len(migrations) || v < 5 {
		t.Fatalf("schema version %d, want %d", v, len(migrations))
	}
	acct, err := db.Account("a@x.test")
	if err != nil {
		t.Fatal(err)
	}
	pend, err := acct.PendingDue(time.Now().Add(time.Hour), 10)
	if err != nil || len(pend) != 1 {
		t.Fatalf("waiting items after the migration: %v %v", pend, err)
	}
	if p := pend[0]; p.Subject != "kept subject" || p.To != "you@x.test" || p.EstSize != 77 || p.Bcc != "" {
		t.Errorf("the waiting item changed or has no empty Bcc: %+v", p)
	}
	if n, size := keptCount(t, acct); n != 0 || size != 0 {
		t.Errorf("a migrated database holds %d kept copies", n)
	}
	if _, err := acct.Update(nil, func(tx *Tx) error {
		return tx.KeepSentCopy("m@x", rawMsg("m@x", "s", "Bcc: a@x.test\r\n"), time.Hour, 5, 1<<20)
	}); err != nil {
		t.Fatalf("the new table cannot be written: %v", err)
	}
	// An account's rows go with the account (ON DELETE CASCADE).
	if _, err := db.w.Exec(`DELETE FROM accounts WHERE id = ?`, acct.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.w.QueryRow(`SELECT COUNT(*) FROM sent_copy`).Scan(&n); err != nil || n != 0 {
		t.Errorf("kept copies of a deleted account: %d %v", n, err)
	}
}

func TestPendingFetchKeepsTheBccProperty(t *testing.T) {
	_, acct := openTemp(t)
	sent := applyStandard(t, acct)["4"]
	date := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	up := func(bcc string) {
		t.Helper()
		if _, err := acct.Update(nil, func(tx *Tx) error {
			return tx.UpsertPending(PendingFetch{FolderID: sent.ID, ServerID: "4:1", InternalDate: date, EstSize: 9, Subject: "s", Bcc: bcc})
		}); err != nil {
			t.Fatal(err)
		}
	}
	read := func() PendingFetch {
		t.Helper()
		var p PendingFetch
		if _, err := acct.Update(nil, func(tx *Tx) error {
			got, err := tx.PendingByServerID(sent.ID, "4:1")
			if err == nil {
				p = *got
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return p
	}
	up(`"Eve" <eve@example.test>`)
	if p := read(); p.Bcc != `"Eve" <eve@example.test>` {
		t.Fatalf("Bcc = %q", p.Bcc)
	}
	up("") // a later announcement without the property does not erase it
	if p := read(); p.Bcc != `"Eve" <eve@example.test>` {
		t.Fatalf("an announcement without the property erased it: %q", p.Bcc)
	}
	up("frank@example.test")
	if p := read(); p.Bcc != "frank@example.test" {
		t.Fatalf("a newer value did not replace it: %q", p.Bcc)
	}
	due, err := acct.PendingDue(time.Now().Add(time.Hour), 10)
	if err != nil || len(due) != 1 || due[0].Bcc != "frank@example.test" {
		t.Fatalf("PendingDue: %+v %v", due, err)
	}
}

// A kept copy that is dropped must not stay readable in the free pages of the
// database file (a copy of the data directory, a backup): the same promise as for
// every deleted message.
func TestDroppedKeptCopiesAreOverwrittenInTheFile(t *testing.T) {
	marker := "EASBRIDGE-KEPT-COPY-MARKER-0123456789-abcdefghij"
	run := func(t *testing.T, secure bool) (inFile bool) {
		db, acct := openTemp(t)
		if !secure {
			if _, err := db.w.Exec(`PRAGMA secure_delete = OFF`); err != nil {
				t.Fatal(err)
			}
		}
		raw := append(rawMsg("kc@x", "s", "Bcc: hidden@example.test\r\n"), []byte(strings.Repeat(marker+" ", 200))...)
		if _, err := acct.Update(nil, func(tx *Tx) error { return tx.KeepSentCopy("kc@x", raw, time.Hour, 5, 1<<20) }); err != nil {
			t.Fatal(err)
		}
		checkpoint := func() {
			if _, err := db.w.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
				t.Fatal(err)
			}
		}
		checkpoint()
		if !fileHas(t, db.path, marker) {
			t.Fatal("the kept copy never reached the database file")
		}
		if _, err := acct.Update(nil, func(tx *Tx) error { return tx.DropSentCopy("kc@x") }); err != nil {
			t.Fatal(err)
		}
		checkpoint()
		return fileHas(t, db.path, marker) || fileHas(t, db.path+"-wal", marker)
	}
	if !run(t, false) {
		t.Fatal("without secure_delete the dropped copy did not stay in the file: the test proves nothing")
	}
	if run(t, true) {
		t.Fatal("a dropped kept copy is still readable in the database file")
	}
}

// Pruning goes through the same DELETE, so the same promise holds for a copy that
// ages out or is pushed out by newer ones.
func TestPrunedKeptCopiesAreOverwrittenInTheFile(t *testing.T) {
	marker := "EASBRIDGE-PRUNED-COPY-MARKER-0123456789-klmnopqrst"
	db, acct := openTemp(t)
	big := func(id string) []byte {
		return append(rawMsg(id, "s", "Bcc: hidden@example.test\r\n"), []byte(strings.Repeat(marker+" ", 200))...)
	}
	if _, err := acct.Update(nil, func(tx *Tx) error {
		if err := tx.KeepSentCopy("old@x", big("old@x"), time.Hour, 1, 1<<20); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.w.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	if !fileHas(t, db.path, marker) {
		t.Fatal("the kept copy never reached the database file")
	}
	// A second copy pushes the first one out (at most one is kept).
	if _, err := acct.Update(nil, func(tx *Tx) error {
		return tx.KeepSentCopy("new@x", []byte(fmt.Sprintf("Message-ID: <new@x>\r\nBcc: a@x.test\r\n\r\nshort\r\n")), time.Hour, 1, 1<<20)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.w.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	if fileHas(t, db.path, marker) || fileHas(t, db.path+"-wal", marker) {
		t.Fatal("a pruned kept copy is still readable in the database file")
	}
	if n, _ := keptCount(t, acct); n != 1 {
		t.Fatalf("%d copies kept, want 1", n)
	}
}

func TestOpeningASentFolderForgetsTheCopiesOfItsStagedMessages(t *testing.T) {
	_, acct := openTemp(t)
	folders := applyStandard(t, acct)
	sent, inbox := folders["4"], folders["1"]
	if _, err := acct.Update(nil, func(tx *Tx) error {
		for _, id := range []string{"staged@x", "visible@x", "waiting@x", "inbox@x"} {
			if err := tx.KeepSentCopy(id, rawMsg(id, "s", "Bcc: x@example.test\r\n"), time.Hour, 10, 1<<20); err != nil {
				return err
			}
		}
		if err := tx.AddStagedMessage(sent.ID, NewMessage{ServerID: "4:1", MIME: rawMsg("staged@x", "s", "")}); err != nil {
			return err
		}
		if _, err := tx.AddMessage(sent.ID, NewMessage{ServerID: "4:2", MIME: rawMsg("visible@x", "s", "")}); err != nil {
			return err
		}
		if err := tx.AddStagedMessage(inbox.ID, NewMessage{ServerID: "1:1", MIME: rawMsg("inbox@x", "s", "")}); err != nil {
			return err
		}
		return tx.DropSentCopiesOfStaged(sent.ID)
	}); err != nil {
		t.Fatal(err)
	}
	var left []string
	if _, err := acct.Update(nil, func(tx *Tx) error {
		for _, id := range []string{"staged@x", "visible@x", "waiting@x", "inbox@x"} {
			if _, err := tx.SentCopy(id); err == nil {
				left = append(left, id)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Only the copy of a message staged in that Sent folder goes; the others are
	// still waiting for their message.
	if strings.Join(left, " ") != "visible@x waiting@x inbox@x" {
		t.Fatalf("copies left: %v", left)
	}
}
