package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Schema 4 (v0.1.2): restore data on queued deletes, metadata on waiting items.

func TestMigration4KeepsOlderRowsAndDefaultsTheNewColumns(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	// A database as v0.1.1 left it: schema version 3, an outbox row and a waiting item.
	if err := func() error {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		raw, err := sql.Open(driverName, "file:"+filepath.Join(dir, DBFileName))
		if err != nil {
			return err
		}
		defer raw.Close()
		for i := 0; i < 3; i++ {
			if _, err := raw.Exec(migrations[i]); err != nil {
				return err
			}
		}
		if _, err := raw.Exec(`PRAGMA user_version = 3`); err != nil {
			return err
		}
		for _, q := range []string{
			`INSERT INTO accounts(id, user, created_at) VALUES (1, 'a@x.test', 0)`,
			`INSERT INTO folders(id, account_id, server_id, display_name, eas_type, path, uid_validity) VALUES (1, 1, '5', 'Inbox', 2, 'INBOX', 1)`,
			`INSERT INTO outbox(folder_id, server_id, op, value, hard, created_at) VALUES (1, '5:1', 3, 1, 0, 0)`,
			`INSERT INTO pending_fetch(folder_id, server_id, flags, internal_date, est_size, created_at) VALUES (1, '5:2', 1, 1000, 0, 0)`,
		} {
			if _, err := raw.Exec(q); err != nil {
				return err
			}
		}
		return nil
	}(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if v, _ := db.SchemaVersion(); v != len(migrations) || v < 4 {
		t.Fatalf("schema version %d, want %d", v, len(migrations))
	}
	acct, err := db.Account("a@x.test")
	if err != nil {
		t.Fatal(err)
	}
	ops, err := acct.OutboxDue(0, time.Now(), 10)
	if err != nil || len(ops) != 1 {
		t.Fatalf("outbox after the migration: %v %v", ops, err)
	}
	if o := ops[0]; o.Op != OpDelete || o.RestoreFlags != 0 || !o.RestoreDate.IsZero() {
		t.Errorf("an older delete has nothing to restore from: %+v", o)
	}
	pend, err := acct.PendingDue(time.Now().Add(time.Hour), 10)
	if err != nil || len(pend) != 1 || pend[0].Subject != "" || pend[0].From != "" || pend[0].To != "" || pend[0].Flags != 1 {
		t.Fatalf("waiting item after the migration: %+v %v", pend, err)
	}
}

func TestDeleteKeepsWhatIsNeededToBringTheMessageBack(t *testing.T) {
	_, acct := openTemp(t)
	inbox := applyStandard(t, acct)["1"]
	date := time.Date(2026, 9, 1, 8, 30, 0, 0, time.UTC)
	if _, err := acct.Update(nil, func(tx *Tx) error {
		return tx.EnqueueDelete(inbox.ID, "1:9", true, FlagSeen|FlagFlagged|FlagDeleted, date)
	}); err != nil {
		t.Fatal(err)
	}
	ops, err := acct.OutboxFor(inbox.ID, "1:9")
	if err != nil || len(ops) != 1 {
		t.Fatalf("%v %v", ops, err)
	}
	o := ops[0]
	if o.Op != OpDelete || !o.Hard || o.RestoreFlags != FlagSeen|FlagFlagged|FlagDeleted || !o.RestoreDate.Equal(date) {
		t.Errorf("queued delete: %+v", o)
	}
	// Queued again (the item was restored and deleted once more): the new record wins.
	later := date.Add(time.Hour)
	if _, err := acct.Update(nil, func(tx *Tx) error { return tx.EnqueueDelete(inbox.ID, "1:9", false, FlagSeen, later) }); err != nil {
		t.Fatal(err)
	}
	ops, _ = acct.OutboxFor(inbox.ID, "1:9")
	if len(ops) != 1 || ops[0].Hard || ops[0].RestoreFlags != FlagSeen || !ops[0].RestoreDate.Equal(later) {
		t.Errorf("requeued delete: %+v", ops)
	}
}

func TestWaitingItemsKeepTheirMetadata(t *testing.T) {
	_, acct := openTemp(t)
	inbox := applyStandard(t, acct)["1"]
	date := time.Unix(5000, 0)
	up := func(p PendingFetch) {
		t.Helper()
		if _, err := acct.Update(nil, func(tx *Tx) error { return tx.UpsertPending(p) }); err != nil {
			t.Fatal(err)
		}
	}
	up(PendingFetch{FolderID: inbox.ID, ServerID: "1:3", InternalDate: date, Subject: "Budget", From: "Carol <c@x.test>", To: "a@x.test; b@x.test"})
	// A refresh that carries no metadata (a Sync Change) does not erase it.
	up(PendingFetch{FolderID: inbox.ID, ServerID: "1:3", InternalDate: date, Flags: FlagSeen})
	got, err := acct.PendingDue(time.Now().Add(time.Hour), 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
	if p := got[0]; p.Subject != "Budget" || p.From != "Carol <c@x.test>" || p.To != "a@x.test; b@x.test" || p.Flags != FlagSeen {
		t.Errorf("waiting item: %+v", p)
	}
}

func TestMessagesAndBindingsCanBeBornInAnEpoch(t *testing.T) {
	_, acct := openTemp(t)
	inbox := applyStandard(t, acct)["1"]
	if _, err := acct.Update(nil, func(tx *Tx) error {
		if err := tx.SetInitialChainDone(inbox.ID, true); err != nil {
			return err
		}
		if _, err := tx.CompleteInitialSync(inbox.ID); err != nil {
			return err
		}
		if _, err := tx.AddMessage(inbox.ID, NewMessage{ServerID: "1:1", MIME: rawMsg("e1@x", "one", ""), ReconMark: 4}); err != nil {
			return err
		}
		if _, err := tx.AddMessage(inbox.ID, NewMessage{ServerID: "1:2", MIME: rawMsg("e2@x", "two", "")}); err != nil {
			return err
		}
		// A Sent placeholder claimed during the epoch.
		ph, err := tx.AddMessage(inbox.ID, NewMessage{Kind: KindPlaceholder, MIME: rawMsg("e3@x", "three", "")})
		if err != nil {
			return err
		}
		return tx.BindServerIDMarked(inbox.ID, ph.UID, "1:3", 4)
	}); err != nil {
		t.Fatal(err)
	}
	var absent []Message
	if _, err := acct.Update(nil, func(tx *Tx) error {
		var err error
		absent, err = tx.ReconAbsent(inbox.ID, 4)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(absent) != 1 || absent[0].ServerID != "1:2" {
		t.Fatalf("suspects at the end of epoch 4: %+v (only the message that was not marked)", absent)
	}
}

// A deleted message must not stay readable in the free pages of the database
// file (a copy of the data directory, a backup).
func TestDeletedMessagesAreOverwrittenInTheFile(t *testing.T) {
	marker := "EASBRIDGE-SECURE-DELETE-MARKER-0123456789-abcdefghij"
	run := func(t *testing.T, secure bool) (inFile bool) {
		db, acct := openTemp(t)
		if !secure {
			if _, err := db.w.Exec(`PRAGMA secure_delete = OFF`); err != nil { // the single writer connection
				t.Fatal(err)
			}
		}
		var on int
		if err := db.w.QueryRow(`PRAGMA secure_delete`).Scan(&on); err != nil || (on == 1) != secure {
			t.Fatalf("secure_delete = %d (%v), want %v", on, err, secure)
		}
		inbox := applyStandard(t, acct)["1"]
		body := strings.Repeat(marker+" ", 200) // a few pages
		raw := append(rawMsg("sd@x", "secret", ""), []byte(body)...)
		var uid uint32
		if _, err := acct.Update(nil, func(tx *Tx) error {
			m, err := tx.AddMessage(inbox.ID, NewMessage{ServerID: "1:1", MIME: raw})
			if err != nil {
				return err
			}
			uid = m.UID
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		checkpoint := func() {
			if _, err := db.w.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
				t.Fatal(err)
			}
		}
		checkpoint() // the message is in the main file now
		if !fileHas(t, db.path, marker) {
			t.Fatal("the test message never reached the database file")
		}
		if _, err := acct.Update(nil, func(tx *Tx) error { _, err := tx.DeleteMessage(inbox.ID, uid); return err }); err != nil {
			t.Fatal(err)
		}
		checkpoint()
		return fileHas(t, db.path, marker) || fileHas(t, db.path+"-wal", marker)
	}
	// The control: with secure_delete off the deleted message does stay behind,
	// so that the check below can fail.
	if !run(t, false) {
		t.Fatal("without secure_delete the deleted message did not stay in the file: the test proves nothing")
	}
	if run(t, true) {
		t.Fatal("a deleted message is still readable in the database file")
	}
}

func fileHas(t *testing.T, path, s string) bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false
		}
		t.Fatal(err)
	}
	return strings.Contains(string(b), s)
}
