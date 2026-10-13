package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Account is a handle on one mailbox's rows.
type Account struct {
	db   *DB
	ID   int64
	User string
}

// Account returns the handle for user (lower-cased), creating the row on first
// use.
func (db *DB) Account(user string) (*Account, error) {
	user = strings.ToLower(strings.TrimSpace(user))
	if user == "" {
		return nil, errors.New("store: empty user")
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	var id int64
	err := db.w.QueryRow(`SELECT id FROM accounts WHERE user = ?`, user).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		var res sql.Result
		res, err = db.w.Exec(`INSERT INTO accounts(user, created_at) VALUES (?, ?)`, user, db.now().Unix())
		if err == nil {
			id, err = res.LastInsertId()
		}
	}
	if err != nil {
		return nil, err
	}
	return &Account{db: db, ID: id, User: user}, nil
}

// PolicyKeyOf returns the stored Provision policy key of user without creating the account's row ("" when
// there is none): a login that has not been verified yet must leave no trace in the database.
func (db *DB) PolicyKeyOf(user string) (string, error) {
	var k string
	err := db.r.QueryRow(`SELECT policy_key FROM accounts WHERE user = ?`, strings.ToLower(strings.TrimSpace(user))).Scan(&k)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return k, err
}

// AccountUsers lists the logins that have a row (tests and diagnostics).
func (db *DB) AccountUsers() ([]string, error) {
	rows, err := db.r.Query(`SELECT user FROM accounts ORDER BY user`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// DB returns the database the account lives in.
func (a *Account) DB() *DB { return a.db }

// Tx is a write transaction scoped to one account. Obtain it from
// Account.Update. Change events are collected and returned after commit.
type Tx struct {
	tx     *sql.Tx
	a      *Account
	origin any
	now    time.Time
	events []Event
}

// Update runs fn in one immediate write transaction. If fn returns nil the
// transaction commits and the collected events are returned (each stamped with
// origin). Otherwise everything is rolled back and no events are returned.
//
// If fn panics the transaction is rolled back before the panic goes on: the
// database has a single writer connection, and a transaction left open on it
// would block every later write for ever.
func (a *Account) Update(origin any, fn func(*Tx) error) ([]Event, error) {
	a.db.mu.Lock()
	defer a.db.mu.Unlock()
	if h := a.db.hook.Load(); h != nil {
		if err := (*h)(); err != nil {
			return nil, err
		}
	}
	sqlTx, err := a.db.w.BeginTx(context.Background(), nil)
	if err != nil {
		return nil, err
	}
	finished := false
	defer func() {
		if !finished { // fn panicked (or ended the goroutine)
			sqlTx.Rollback()
		}
	}()
	t := &Tx{tx: sqlTx, a: a, origin: origin, now: a.db.now()}
	if err := fn(t); err != nil {
		finished = true
		sqlTx.Rollback()
		return nil, err
	}
	finished = true
	if err := sqlTx.Commit(); err != nil {
		return nil, err
	}
	return t.events, nil
}

func (t *Tx) emit(e Event) {
	e.Origin = t.origin
	t.events = append(t.events, e)
}

// Now returns the transaction's timestamp.
func (t *Tx) Now() time.Time { return t.now }

// Account returns the account the transaction belongs to.
func (t *Tx) Account() *Account { return t.a }

// Keys ------------------------------------------------------------------------

// FolderSyncKey returns the committed FolderSync key ("0" if none).
func (a *Account) FolderSyncKey() (string, error) {
	var k string
	err := a.db.r.QueryRow(`SELECT folder_sync_key FROM accounts WHERE id = ?`, a.ID).Scan(&k)
	return k, err
}

// PolicyKey returns the committed Provision policy key ("" if none).
func (a *Account) PolicyKey() (string, error) {
	var k string
	err := a.db.r.QueryRow(`SELECT policy_key FROM accounts WHERE id = ?`, a.ID).Scan(&k)
	return k, err
}

// ContactFoldersKept reports whether the stored hierarchy (the FolderSync key) was taken
// with the Contacts folders kept, so that an incremental FolderSync can be trusted to
// mention them. False for every account of a database from before schema 8, and again
// after the contact data was removed or a hierarchy was taken with the feature off.
func (a *Account) ContactFoldersKept() (bool, error) {
	var n int
	err := a.db.r.QueryRow(`SELECT contact_folders_kept FROM accounts WHERE id = ?`, a.ID).Scan(&n)
	return n != 0, err
}

// SetContactFoldersKept records, in the transaction that commits a hierarchy, whether that
// hierarchy was applied with the contact folders kept.
func (t *Tx) SetContactFoldersKept(kept bool) error {
	n := 0
	if kept {
		n = 1
	}
	_, err := t.tx.Exec(`UPDATE accounts SET contact_folders_kept = ? WHERE id = ?`, n, t.a.ID)
	return err
}

// SetFolderSyncKey commits the FolderSync key.
func (t *Tx) SetFolderSyncKey(key string) error {
	_, err := t.tx.Exec(`UPDATE accounts SET folder_sync_key = ? WHERE id = ?`, key, t.a.ID)
	return err
}

// FolderSyncKey reads the key inside the transaction (used to verify a write).
func (t *Tx) FolderSyncKey() (string, error) {
	var k string
	err := t.tx.QueryRow(`SELECT folder_sync_key FROM accounts WHERE id = ?`, t.a.ID).Scan(&k)
	return k, err
}

// SetPolicyKey commits the policy key.
func (t *Tx) SetPolicyKey(key string) error {
	_, err := t.tx.Exec(`UPDATE accounts SET policy_key = ? WHERE id = ?`, key, t.a.ID)
	return err
}

// SetSyncKey commits a folder's Sync key.
func (t *Tx) SetSyncKey(folderID int64, key string) error {
	res, err := t.tx.Exec(`UPDATE folders SET sync_key = ? WHERE id = ? AND account_id = ?`, key, folderID, t.a.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: set sync key: %w", ErrNotFound)
	}
	return nil
}

// nextUIDValidity hands out a UIDVALIDITY that is greater than any given out
// before for this account (so a re-created folder never repeats one).
func (t *Tx) nextUIDValidity() (uint32, error) {
	var floor int64
	if err := t.tx.QueryRow(`SELECT uid_validity_floor FROM accounts WHERE id = ?`, t.a.ID).Scan(&floor); err != nil {
		return 0, err
	}
	v := t.now.Unix()
	if v <= floor {
		v = floor + 1
	}
	if v > 0xFFFFFFF0 {
		return 0, errors.New("store: UIDVALIDITY space exhausted")
	}
	if _, err := t.tx.Exec(`UPDATE accounts SET uid_validity_floor = ? WHERE id = ?`, v, t.a.ID); err != nil {
		return 0, err
	}
	return uint32(v), nil
}
