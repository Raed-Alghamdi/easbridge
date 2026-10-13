package store

import (
	"database/sql"
	"errors"
	"time"
)

// Outbox ----------------------------------------------------------------------

func scanOp(sc interface{ Scan(...any) error }) (OutboxOp, error) {
	var o OutboxOp
	var value, hard int
	var rflags, rdate int64
	err := sc.Scan(&o.ID, &o.FolderID, &o.ServerID, &o.Op, &value, &o.Orig, &hard, &o.Version, &o.Attempts, &o.NextAttempt, &rflags, &rdate)
	o.Value, o.Hard = value != 0, hard != 0
	o.RestoreFlags = Flags(rflags)
	if rdate != 0 {
		o.RestoreDate = time.Unix(rdate, 0).UTC()
	}
	return o, err
}

const opCols = `id, folder_id, server_id, op, value, orig, hard, version, attempts, next_attempt, restore_flags, restore_date`

func queryOps(q querier, where string, args ...any) ([]OutboxOp, error) {
	rows, err := q.Query(`SELECT `+opCols+` FROM outbox WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxOp
	for rows.Next() {
		o, err := scanOp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// EnqueueOp queues an upstream change for an item, replacing an older queued
// change of the same kind. A delete supersedes every other op for the item,
// and ops for an item with a queued delete are dropped. orig is the value the
// attribute had before this change (0, 1, or -1 if unknown); the first queued
// orig is kept so a rejected change can be rolled back.
func (t *Tx) EnqueueOp(folderID int64, serverID string, op int, value, hard bool, orig int) error {
	if op == OpDelete {
		if _, err := t.tx.Exec(`DELETE FROM outbox WHERE folder_id = ? AND server_id = ? AND op != ?`, folderID, serverID, OpDelete); err != nil {
			return err
		}
	} else {
		var n int
		if err := t.tx.QueryRow(`SELECT COUNT(*) FROM outbox WHERE folder_id = ? AND server_id = ? AND op = ?`, folderID, serverID, OpDelete).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
	}
	_, err := t.tx.Exec(`INSERT INTO outbox(folder_id, server_id, op, value, orig, hard, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(folder_id, server_id, op) DO UPDATE SET
		  value = excluded.value, hard = excluded.hard, version = version + 1, attempts = 0, next_attempt = 0`,
		folderID, serverID, op, b2i(value), orig, b2i(hard), t.now.Unix())
	return err
}

// EnqueueDelete queues the upstream delete of an item, like EnqueueOp with
// OpDelete, and remembers the flags and received date the message had, so that
// it can be brought back if Exchange refuses the delete.
func (t *Tx) EnqueueDelete(folderID int64, serverID string, hard bool, flags Flags, date time.Time) error {
	if err := t.EnqueueOp(folderID, serverID, OpDelete, true, hard, -1); err != nil {
		return err
	}
	_, err := t.tx.Exec(`UPDATE outbox SET restore_flags = ?, restore_date = ? WHERE folder_id = ? AND server_id = ? AND op = ?`,
		int(flags), date.Unix(), folderID, serverID, OpDelete)
	return err
}

// OutboxFor returns the queued ops of one item.
func (t *Tx) OutboxFor(folderID int64, serverID string) ([]OutboxOp, error) {
	return queryOps(t.tx, `folder_id = ? AND server_id = ?`, folderID, serverID)
}

// OutboxFor returns the queued ops of one item (read pool).
func (a *Account) OutboxFor(folderID int64, serverID string) ([]OutboxOp, error) {
	return queryOps(a.db.r, `folder_id = ? AND server_id = ?`, folderID, serverID)
}

// OutboxDue returns up to limit ops whose retry time has come, oldest first,
// for one folder or (folderID 0) all folders.
func (a *Account) OutboxDue(folderID int64, now time.Time, limit int) ([]OutboxOp, error) {
	if folderID != 0 {
		return queryOps(a.db.r, `folder_id = ? AND next_attempt <= ? ORDER BY id LIMIT ?`, folderID, now.UnixMilli(), limit)
	}
	return queryOps(a.db.r, `next_attempt <= ? AND folder_id IN (SELECT id FROM folders WHERE account_id = ?) ORDER BY id LIMIT ?`,
		now.UnixMilli(), a.ID, limit)
}

// OutboxLen counts queued ops.
func (a *Account) OutboxLen() (int, error) {
	var n int
	err := a.db.r.QueryRow(`SELECT COUNT(*) FROM outbox WHERE folder_id IN (SELECT id FROM folders WHERE account_id = ?)`, a.ID).Scan(&n)
	return n, err
}

// OutboxFolders lists folders that have queued ops.
func (a *Account) OutboxFolders() ([]int64, error) {
	rows, err := a.db.r.Query(`SELECT DISTINCT folder_id FROM outbox WHERE folder_id IN (SELECT id FROM folders WHERE account_id = ?) ORDER BY folder_id`, a.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// CompleteOps removes ops the server has applied. An op that was overwritten
// by a newer STORE while it was in flight (version changed) stays queued.
func (t *Tx) CompleteOps(ops []OutboxOp) error {
	for _, o := range ops {
		if _, err := t.tx.Exec(`DELETE FROM outbox WHERE id = ? AND version = ?`, o.ID, o.Version); err != nil {
			return err
		}
	}
	return nil
}

// FailOps schedules a retry for ops that could not be applied.
func (t *Tx) FailOps(ops []OutboxOp, next time.Time) error {
	for _, o := range ops {
		if _, err := t.tx.Exec(`UPDATE outbox SET attempts = attempts + 1, next_attempt = ? WHERE id = ? AND version = ?`,
			next.UnixMilli(), o.ID, o.Version); err != nil {
			return err
		}
	}
	return nil
}

// DropOutbox removes every queued op of an item.
func (t *Tx) DropOutbox(folderID int64, serverID string) error {
	_, err := t.tx.Exec(`DELETE FROM outbox WHERE folder_id = ? AND server_id = ?`, folderID, serverID)
	return err
}

// Pending fetches --------------------------------------------------------------

func scanPending(sc interface{ Scan(...any) error }) (PendingFetch, error) {
	var p PendingFetch
	var flags, date int64
	err := sc.Scan(&p.ID, &p.FolderID, &p.ServerID, &flags, &date, &p.EstSize, &p.Attempts, &p.ItemFailures, &p.NextAttempt,
		&p.Subject, &p.From, &p.To, &p.Bcc)
	p.Flags, p.InternalDate = Flags(flags), time.Unix(date, 0).UTC()
	return p, err
}

const pendCols = `id, folder_id, server_id, flags, internal_date, est_size, attempts, item_failures, next_attempt, subject, from_addr, to_addr, bcc`

// UpsertPending records (or refreshes) an item waiting for its MIME. Attempts
// are kept when it already exists.
func (t *Tx) UpsertPending(p PendingFetch) error {
	_, err := t.tx.Exec(`INSERT INTO pending_fetch(folder_id, server_id, flags, internal_date, est_size, created_at, subject, from_addr, to_addr, bcc)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(folder_id, server_id) DO UPDATE SET
		  flags = excluded.flags, internal_date = excluded.internal_date, est_size = excluded.est_size,
		  subject = CASE WHEN excluded.subject != '' THEN excluded.subject ELSE subject END,
		  from_addr = CASE WHEN excluded.from_addr != '' THEN excluded.from_addr ELSE from_addr END,
		  to_addr = CASE WHEN excluded.to_addr != '' THEN excluded.to_addr ELSE to_addr END,
		  bcc = CASE WHEN excluded.bcc != '' THEN excluded.bcc ELSE bcc END`,
		p.FolderID, p.ServerID, int(p.Flags), p.InternalDate.Unix(), p.EstSize, t.now.Unix(), p.Subject, p.From, p.To, p.Bcc)
	return err
}

// PendingByServerID returns the waiting item, or ErrNotFound.
func (t *Tx) PendingByServerID(folderID int64, serverID string) (*PendingFetch, error) {
	p, err := scanPending(t.tx.QueryRow(`SELECT `+pendCols+` FROM pending_fetch WHERE folder_id = ? AND server_id = ?`, folderID, serverID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &p, err
}

// UpdatePendingFlags changes the flags a waiting item will get.
func (t *Tx) UpdatePendingFlags(folderID int64, serverID string, flags Flags) error {
	_, err := t.tx.Exec(`UPDATE pending_fetch SET flags = ? WHERE folder_id = ? AND server_id = ?`, int(flags), folderID, serverID)
	return err
}

// DeletePending forgets a waiting item.
func (t *Tx) DeletePending(folderID int64, serverID string) error {
	_, err := t.tx.Exec(`DELETE FROM pending_fetch WHERE folder_id = ? AND server_id = ?`, folderID, serverID)
	return err
}

// BumpPending records a failed attempt and when to try again. itemSpecific says
// the failure was about this item (Exchange answered with a status for it, or
// sent no body) rather than about the connection; only those count towards
// giving the item up (PendingBlocking).
func (t *Tx) BumpPending(id int64, next time.Time, itemSpecific bool) error {
	_, err := t.tx.Exec(`UPDATE pending_fetch SET attempts = attempts + 1, item_failures = item_failures + ?, next_attempt = ? WHERE id = ?`,
		b2i(itemSpecific), next.UnixMilli(), id)
	return err
}

// PendingDue returns up to limit waiting items whose retry time has come,
// newest first (the initial backfill shows recent mail first).
func (a *Account) PendingDue(now time.Time, limit int) ([]PendingFetch, error) {
	rows, err := a.db.r.Query(`SELECT `+pendCols+` FROM pending_fetch
		WHERE next_attempt <= ? AND folder_id IN (SELECT id FROM folders WHERE account_id = ?)
		ORDER BY internal_date DESC, id LIMIT ?`, now.UnixMilli(), a.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingFetch
	for rows.Next() {
		p, err := scanPending(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PendingNext returns when the earliest waiting item is due (ok is false if none
// waits).
func (a *Account) PendingNext() (due time.Time, ok bool, err error) {
	var ms sql.NullInt64
	err = a.db.r.QueryRow(`SELECT MIN(next_attempt) FROM pending_fetch WHERE folder_id IN (SELECT id FROM folders WHERE account_id = ?)`, a.ID).Scan(&ms)
	if err != nil || !ms.Valid {
		return time.Time{}, false, err
	}
	return time.UnixMilli(ms.Int64), true, nil
}

// PendingCount counts waiting items (all folders when folderID is 0).
func (a *Account) PendingCount(folderID int64) (int, error) {
	var n int
	var err error
	if folderID == 0 {
		err = a.db.r.QueryRow(`SELECT COUNT(*) FROM pending_fetch WHERE folder_id IN (SELECT id FROM folders WHERE account_id = ?)`, a.ID).Scan(&n)
	} else {
		err = a.db.r.QueryRow(`SELECT COUNT(*) FROM pending_fetch WHERE folder_id = ?`, folderID).Scan(&n)
	}
	return n, err
}
