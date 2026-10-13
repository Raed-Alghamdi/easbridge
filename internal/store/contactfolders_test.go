package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// rawDBAt builds a database at the given schema (5, 6 or 7) with one account whose FolderSync
// key was committed, as 0.1.8, 0.2.0 and 0.2.1 left them.
func rawDBAt(t *testing.T, version int) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open(driverName, "file:"+filepath.Join(dir, DBFileName))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for i := 0; i < version; i++ {
		if _, err := raw.Exec(migrations[i]); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`PRAGMA user_version = ` + string(rune('0'+version)),
		`INSERT INTO accounts(id, user, created_at, folder_sync_key) VALUES (1, 'a@corp.example', 0, 'hk9')`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return dir
}

func TestMigration8AddsTheMarkerClearedToSchemas5To7AndKeepsTheHierarchyKey(t *testing.T) {
	for _, from := range []int{5, 6, 7} {
		db, err := Open(rawDBAt(t, from))
		if err != nil {
			t.Fatalf("from %d: %v", from, err)
		}
		if v, _ := db.SchemaVersion(); v != 8 {
			t.Errorf("from %d: schema %d", from, v)
		}
		acct, err := db.Account("a@corp.example")
		if err != nil || acct.ID != 1 {
			t.Fatalf("from %d: %+v %v", from, acct, err)
		}
		if k, _ := acct.FolderSyncKey(); k != "hk9" {
			t.Errorf("from %d: the hierarchy key is %q", from, k)
		}
		// Every existing account reads its hierarchy again once the contacts are on.
		if kept, err := acct.ContactFoldersKept(); err != nil || kept {
			t.Errorf("from %d: marker %v %v, want it cleared", from, kept, err)
		}
		db.Close()
	}
}

func TestTheMarkerIsSetWithTheHierarchyAndClearedWhenTheContactDataGoes(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	acct, err := db.Account("a@corp.example")
	if err != nil {
		t.Fatal(err)
	}
	kept := func() bool {
		k, err := acct.ContactFoldersKept()
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	set := func(v bool) {
		if _, err := acct.Update(nil, func(tx *Tx) error { return tx.SetContactFoldersKept(v) }); err != nil {
			t.Fatal(err)
		}
	}
	if kept() {
		t.Fatal("a new account has the marker set")
	}
	set(true)
	if !kept() {
		t.Fatal("the marker did not stick")
	}
	// The feature off at the start of the service: with contact rows, and with none at all.
	addContactFolders(t, acct)
	if _, _, err := db.PurgeContactData(); err != nil || kept() {
		t.Errorf("PurgeContactData left the marker: %v %v", kept(), err)
	}
	set(true)
	if _, _, err := db.PurgeContactData(); err != nil || kept() {
		t.Errorf("PurgeContactData with nothing to remove left the marker: %v %v", kept(), err)
	}
	// A remote wipe and the removal of the contact data of one account.
	set(true)
	if _, err := acct.Update(nil, func(tx *Tx) error { return tx.DeleteContactData() }); err != nil || kept() {
		t.Errorf("DeleteContactData left the marker: %v %v", kept(), err)
	}
	// Turning only the directory off keeps it.
	set(true)
	if _, err := db.PurgeDirectoryData(); err != nil || !kept() {
		t.Errorf("PurgeDirectoryData cleared the marker: %v %v", kept(), err)
	}
}
