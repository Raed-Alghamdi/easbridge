package store

import (
	"errors"
	"testing"
	"time"
)

func TestAPanickingTransactionIsRolledBackAndDoesNotBlockLaterWrites(t *testing.T) {
	// The database has one writer connection. A panic in the callback of Update
	// used to leave its transaction open on it, and every later write then waited
	// for ever (a stuck mailbox until the process was restarted).
	_, acct := openTemp(t)
	applyStandard(t, acct)
	if _, err := acct.Update(nil, func(tx *Tx) error { return tx.SetFolderSyncKey("before") }); err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic must go on after the rollback")
			}
		}()
		_, _ = acct.Update(nil, func(tx *Tx) error {
			if err := tx.SetFolderSyncKey("written-by-the-transaction-that-panics"); err != nil {
				t.Error(err)
			}
			panic("boom")
		})
	}()
	if k, _ := acct.FolderSyncKey(); k != "before" {
		t.Fatalf("the half-done transaction is visible: key %q", k)
	}
	done := make(chan error, 1)
	go func() {
		_, err := acct.Update(nil, func(tx *Tx) error { return tx.SetFolderSyncKey("after") })
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the next write is blocked: the transaction of the panicking one is still open")
	}
	if k, _ := acct.FolderSyncKey(); k != "after" {
		t.Fatalf("key = %q", k)
	}
}

func TestWriteHookFailsWritesButNotReads(t *testing.T) {
	db, acct := openTemp(t)
	applyStandard(t, acct)
	db.SetWriteHook(func() error { return ErrInjected })
	called := false
	_, err := acct.Update(nil, func(tx *Tx) error { called = true; return nil })
	if !errors.Is(err, ErrInjected) || called || !IsDBError(err) {
		t.Fatalf("a write must fail before it starts: err=%v called=%v", err, called)
	}
	if _, err := acct.Folders(); err != nil {
		t.Fatalf("reads must keep working: %v", err)
	}
	db.SetWriteHook(nil)
	if _, err := acct.Update(nil, func(tx *Tx) error { return nil }); err != nil {
		t.Fatalf("writes work again once the hook is gone: %v", err)
	}
}

func TestIsDBErrorSeparatesTheDatabaseFromTheCallersLogic(t *testing.T) {
	_, acct := openTemp(t)
	if IsDBError(nil) || IsDBError(ErrNotFound) || IsDBError(errors.New("a refusal by the caller")) {
		t.Error("only failures of the database itself count")
	}
	if !IsDBError(ErrInjected) {
		t.Error("an injected fault is a database failure")
	}
	// A real SQLite error reached through a real statement.
	_, err := acct.Update(nil, func(tx *Tx) error {
		_, err := tx.tx.Exec(`INSERT INTO no_such_table VALUES (1)`)
		return err
	})
	if err == nil || !IsDBError(err) {
		t.Errorf("a SQLite error must be recognised: %v", err)
	}
}
