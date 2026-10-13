// Package store is easbridge's SQLite state: folders, messages with their
// ServerId to UID map, cached MIME, local drafts and Sent placeholders, the
// durable outbox, and the committed EAS sync/policy keys.
//
// Concurrency: one writer connection (every mutation runs through
// Account.Update, serialised) and a small pool of read-only connections. WAL
// mode lets readers continue while a Sync batch commits.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"modernc.org/sqlite" // CGO-free SQLite driver, registers "sqlite"
)

// File names inside the data directory.
const (
	DBFileName      = "state.db"
	WipeMarkerName  = "WIPED"
	metaDeviceID    = "device_id"
	readPoolSize    = 4
	busyTimeoutMs   = 5000
	driverName      = "sqlite"
	wipeMarkerPerms = 0o600
)

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("store: not found")

// DB is the open state database.
type DB struct {
	dir  string
	path string
	w    *sql.DB // single writer connection
	r    *sql.DB // read-only pool

	mu  sync.Mutex // serialises writers
	now func() time.Time

	// hook, when set, is consulted before every write transaction (tests).
	hook atomic.Pointer[func() error]

	// candDomains are the directory domains whose addresses a stored message makes
	// candidates for a lookup (nil: the directory is off).
	candDomains atomic.Pointer[candidateDomains]
	// noteOff holds the ids of the accounts whose ingest does not note candidates.
	noteOff sync.Map

	closeOnce sync.Once
}

// Open creates dir if needed, opens (or creates) state.db and migrates it.
func Open(dir string) (*DB, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("store: create data dir: %w", err)
	}
	path := filepath.Join(dir, DBFileName)
	// Create the file ourselves with private permissions: SQLite gives the
	// -wal and -shm files the permissions of the main file.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: create db file: %w", err)
	}
	f.Close()

	db := &DB{dir: dir, path: path, now: time.Now}
	base := (&url.URL{Path: path}).EscapedPath()
	// temp_store=MEMORY (2): the production container runs with a read-only
	// root file system, so SQLite must never need a temp file for sorts.
	// secure_delete=ON: a deleted message (an expunge, a wipe of a folder, a
	// removal by the server) is overwritten with zeros in the file instead of
	// lingering in free pages, where it would stay readable in a copy of the
	// database or a backup of the data directory.
	pragmas := fmt.Sprintf("_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_pragma=temp_store(2)&_pragma=secure_delete(1)", busyTimeoutMs)
	db.w, err = sql.Open(driverName, "file:"+base+"?"+pragmas+"&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.w.SetMaxOpenConns(1)
	db.w.SetConnMaxLifetime(0)
	if err := db.w.Ping(); err != nil {
		db.w.Close()
		return nil, fmt.Errorf("store: open %s: %w", DBFileName, err)
	}
	if err := db.migrate(); err != nil {
		db.w.Close()
		return nil, err
	}
	db.r, err = sql.Open(driverName, "file:"+base+"?"+pragmas+"&_pragma=query_only(1)")
	if err != nil {
		db.w.Close()
		return nil, err
	}
	db.r.SetMaxOpenConns(readPoolSize)
	return db, nil
}

func (db *DB) migrate() error {
	ctx := context.Background()
	var have int
	if err := db.w.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&have); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	if have > len(migrations) {
		return fmt.Errorf("store: database schema version %d is newer than this build (%d)", have, len(migrations))
	}
	for v := have; v < len(migrations); v++ {
		tx, err := db.w.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[v]); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migration %d: %w", v+1, err)
		}
		// PRAGMA does not accept bound parameters.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", v+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migration %d: set version: %w", v+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: migration %d: %w", v+1, err)
		}
	}
	return nil
}

// SchemaVersion returns the applied migration count.
func (db *DB) SchemaVersion() (int, error) {
	var v int
	err := db.r.QueryRow(`PRAGMA user_version`).Scan(&v)
	return v, err
}

// Dir returns the data directory.
func (db *DB) Dir() string { return db.dir }

// ErrInjected is what a write hook makes writes fail with.
var ErrInjected = errors.New("store: injected write failure")

// SetWriteHook installs a function that is consulted before every write
// transaction; if it returns an error the write fails with it. Tests use it to
// simulate a database that cannot be written. nil removes the hook.
func (db *DB) SetWriteHook(f func() error) {
	if f == nil {
		db.hook.Store(nil)
		return
	}
	db.hook.Store(&f)
}

// IsDBError reports whether err is a failure of the database itself (SQLite,
// the connection, an injected fault) rather than a refusal by the caller's own
// logic such as ErrNotFound.
func IsDBError(err error) bool {
	if err == nil {
		return false
	}
	var se *sqlite.Error
	return errors.Is(err, ErrInjected) || errors.As(err, &se) ||
		errors.Is(err, sql.ErrConnDone) || errors.Is(err, sql.ErrTxDone) || errors.Is(err, driver.ErrBadConn)
}

// SetClock replaces the time source (tests).
func (db *DB) SetClock(now func() time.Time) { db.now = now }

// Close closes both pools. It is safe to call more than once.
func (db *DB) Close() error {
	var err error
	db.closeOnce.Do(func() {
		db.mu.Lock()
		defer db.mu.Unlock()
		err = errors.Join(db.r.Close(), db.w.Close())
	})
	return err
}

// Destroy closes the database and deletes state.db, state.db-wal and
// state.db-shm. It is what a remote wipe does to the mail cache.
func (db *DB) Destroy() error {
	cerr := db.Close()
	var errs []error
	errs = append(errs, cerr)
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err := os.Remove(db.path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// DeviceID returns the install's EAS DeviceId (32 hex characters), creating it
// on first use. It is a device token, not a user secret.
func (db *DB) DeviceID() (string, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	var id string
	err := db.w.QueryRow(`SELECT value FROM meta WHERE key = ?`, metaDeviceID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	id = hex.EncodeToString(buf)
	// INSERT OR IGNORE then re-read: another process (the probe) may race us.
	if _, err := db.w.Exec(`INSERT OR IGNORE INTO meta(key, value) VALUES (?, ?)`, metaDeviceID, id); err != nil {
		return "", err
	}
	err = db.w.QueryRow(`SELECT value FROM meta WHERE key = ?`, metaDeviceID).Scan(&id)
	return id, err
}

// Wipe marker -----------------------------------------------------------------

// WipeMarkerPath is the file whose presence makes the bridge refuse logins.
func WipeMarkerPath(dir string) string { return filepath.Join(dir, WipeMarkerName) }

// WipeMarkerExists reports whether a remote wipe has happened and the operator
// has not yet removed the marker.
func WipeMarkerExists(dir string) bool {
	_, err := os.Stat(WipeMarkerPath(dir))
	return err == nil
}

// WriteWipeMarker records a remote wipe.
func WriteWipeMarker(dir string, when time.Time) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	msg := fmt.Sprintf("Exchange requested a remote wipe at %s.\nThe local mail cache was deleted. Delete this file to allow logins again.\n",
		when.UTC().Format(time.RFC3339))
	return os.WriteFile(WipeMarkerPath(dir), []byte(msg), wipeMarkerPerms)
}

// querier is satisfied by *sql.DB and *sql.Tx.
type querier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}
