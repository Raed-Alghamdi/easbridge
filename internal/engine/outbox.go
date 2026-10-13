package engine

import (
	"context"
	"errors"
	"time"

	"easbridge/internal/store"
	"easbridge/internal/upstream"
)

// The outbox holds local flag changes and expunges until Exchange has accepted
// them. IMAP commands change the local state at once (so every later command
// sees it) and queue the upstream change here; the worker sends queued changes
// folder by folder in batches.

// maxOpAttempts is how many times a change that fails with a transient status
// is retried before it is treated as rejected and rolled back.
const maxOpAttempts = 8

// outboxStall holds the whole outbox back for a growing time. It is the safety
// net for a failure that could not be recorded with the queued changes (the
// database cannot be written, or the folder cannot be read): the changes would be
// due again at once, and every pass would be another request to Exchange (the
// security review of v0.1.0 saw 12,800 identical Syncs in two seconds).
// Failures that are recorded back off per change, see deferOps.
func (a *Account) outboxStall() {
	a.wmu.Lock()
	a.outboxFailures++
	a.outboxRetryAt = a.backoff(a.outboxFailures)
	a.wmu.Unlock()
}

// outboxUnstall ends the stall: Exchange took the changes and the result is stored.
func (a *Account) outboxUnstall() {
	a.wmu.Lock()
	a.outboxFailures = 0
	a.outboxRetryAt = time.Time{}
	a.wmu.Unlock()
}

// flushOutboxStep sends the due changes of one folder. It reports whether it
// had anything to do.
func (a *Account) flushOutboxStep(now time.Time) bool {
	a.wmu.Lock()
	wait := now.Before(a.outboxRetryAt)
	a.wmu.Unlock()
	if wait {
		return false
	}
	ops, err := a.st.OutboxDue(0, now, a.cfg.OutboxBatch)
	if err != nil || len(ops) == 0 {
		return false
	}
	fid := ops[0].FolderID
	var group []store.OutboxOp
	for _, o := range ops {
		if o.FolderID == fid {
			group = append(group, o)
		}
	}
	a.flushFolderOps(fid, group)
	return true
}

// flushNow synchronously sends everything queued for one folder, retries
// included (used before a MOVE so the server sees the flags first). It runs on
// the worker and fails if Exchange does not accept the changes.
func (a *Account) flushNow(ctx context.Context, folderID int64) error {
	farFuture := a.cfg.Clock().Add(100 * 24 * time.Hour)
	for i := 0; i < 50; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		ops, err := a.st.OutboxDue(folderID, farFuture, a.cfg.OutboxBatch)
		if err != nil {
			return err
		}
		if len(ops) == 0 {
			return nil
		}
		a.flushFolderOps(folderID, ops)
		if a.authFailed.Load() {
			return ErrAuthFailed
		}
		after, err := a.st.OutboxDue(folderID, farFuture, a.cfg.OutboxBatch)
		if err != nil {
			return err
		}
		if sameOps(after, ops) {
			return ErrUnavailable // no progress: Exchange is not accepting the changes now
		}
	}
	return ErrUnavailable
}

func sameOps(a, b []store.OutboxOp) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Version != b[i].Version {
			return false
		}
	}
	return true
}

// flushFolderOps sends one batch of a folder's queued changes.
func (a *Account) flushFolderOps(folderID int64, ops []store.OutboxOp) {
	f, err := a.st.FolderByID(folderID)
	if err != nil {
		// A folder that is gone took its ops with it (cascade). Any other error is
		// the database: hold the outbox back so that this does not spin.
		if !errors.Is(err, store.ErrNotFound) {
			a.log.Warn("reading a folder failed; the outbox waits", "folder", folderID, "err", err)
			a.outboxStall()
		}
		return
	}
	if f.SyncKey == "0" || f.SyncKey == "" {
		// Not bootstrapped (new folder, or a verification resync restarted it).
		a.markDirty(f.ID)
		a.deferOps(ops, 2*time.Second)
		return
	}
	// Deletes inside Deleted Items are permanent (DeletesAsMoves=0); everywhere
	// else a delete moves the item to Deleted Items.
	var soft, hard, flags []store.OutboxOp
	for _, o := range ops {
		switch {
		case o.Op == store.OpDelete && o.Hard:
			hard = append(hard, o)
		case o.Op == store.OpDelete:
			soft = append(soft, o)
		default:
			flags = append(flags, o)
		}
	}
	for _, g := range []struct {
		ops  []store.OutboxOp
		hard bool
	}{{flags, false}, {soft, false}, {hard, true}} {
		if len(g.ops) == 0 {
			continue
		}
		if a.sendOps(f.ID, g.ops, g.hard) {
			return // stop on failure; the rest retries with the backoff
		}
	}
}

// deferOps records that ops failed and are not to be tried before now+d. If the
// database cannot record it, the whole outbox is held back in memory instead.
func (a *Account) deferOps(ops []store.OutboxOp, d time.Duration) {
	if _, err := a.commit(nil, func(tx *store.Tx) error { return tx.FailOps(ops, a.cfg.Clock().Add(d)) }); err != nil {
		a.log.Warn("could not record a failed upload; holding the outbox back", "err", err)
		a.outboxStall()
	}
}

// sendOps sends ops (all deletes of one kind, or all flag changes) and commits
// the outcome. It reports whether the caller should stop (failure).
func (a *Account) sendOps(folderID int64, ops []store.OutboxOp, hard bool) (failed bool) {
	f, err := a.st.FolderByID(folderID) // fresh key: an earlier group may have advanced it
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			a.log.Warn("reading a folder failed; the outbox waits", "folder", folderID, "err", err)
			a.outboxStall()
		}
		return true
	}
	// One Change per item: read and flagged ops of the same item are combined.
	byItem := map[string]*upstream.Change{}
	var order []string
	for _, o := range ops {
		c := byItem[o.ServerID]
		if c == nil {
			c = &upstream.Change{ServerID: o.ServerID}
			byItem[o.ServerID] = c
			order = append(order, o.ServerID)
		}
		v := o.Value
		switch o.Op {
		case store.OpRead:
			c.Read = &v
		case store.OpFlagged:
			c.Flagged = &v
		case store.OpDelete:
			c.Delete = true
		}
	}
	changes := make([]upstream.Change, 0, len(order))
	for _, sid := range order {
		changes = append(changes, *byItem[sid])
	}

	ctx, cancel := a.callCtx()
	res, err := a.upstream().ApplyChanges(ctx, f.ServerID, f.SyncKey, changes, hard)
	cancel()
	if err != nil {
		if isPaused(err) || a.handleUpstreamError(err) {
			return true // the breaker is open (nothing was sent), or the account is stopping
		}
		var se *upstream.StatusError
		switch {
		case errors.As(err, &se) && se.Code == 3:
			// The server forgot the folder's sync state: restart the folder from
			// scratch. The queued changes stay and are sent after the resync.
			a.log.Warn("sync key rejected while sending changes; resyncing the folder", "folder", f.ID)
			if a.resetFolderKey(f.ID) != nil {
				a.outboxStall()
			}
		case errors.Is(err, upstream.ErrHierarchyChanged), errors.As(err, &se) && se.Code == 12:
			a.requestFolderSync()
			a.deferOps(ops, 2*time.Second)
		default:
			a.log.Warn("sending changes failed", "folder", f.ID, "err", err)
			attempts := 0
			for _, o := range ops {
				if o.Attempts > attempts {
					attempts = o.Attempts
				}
			}
			a.deferOps(ops, a.cfg.Backoff(attempts+1))
		}
		return true
	}

	results := map[string]upstream.ChangeResult{}
	for _, r := range res.Results {
		results[r.ServerID] = r
	}
	var more, restored bool
	_, err = a.commit(nil, func(tx *store.Tx) error {
		restored = false
		cur, err := tx.FolderByID(f.ID)
		if err != nil {
			return err
		}
		if cur.SyncKey != f.SyncKey {
			return errStaleKey
		}
		if err := tx.SetSyncKey(f.ID, res.NewKey); err != nil {
			return err
		}
		var done, retry []store.OutboxOp
		for _, o := range ops {
			r, has := results[o.ServerID]
			switch {
			case !has || r.OK || r.Gone || r.Status == 1 || r.Status == 7:
				// Accepted, already gone, or lost a conflict (the server's version
				// arrives with the next Sync).
				done = append(done, o)
			case isTransientStatus(r.Status) && o.Attempts+1 < maxOpAttempts:
				retry = append(retry, o)
			default:
				a.log.Warn("Exchange rejected a change; undoing it locally", "folder", f.ID, "status", r.Status)
				done = append(done, o)
				back, err := a.rollbackOp(tx, f.ID, o)
				if err != nil {
					return err
				}
				restored = restored || back
			}
		}
		if err := tx.CompleteOps(done); err != nil {
			return err
		}
		if len(retry) > 0 {
			if err := tx.FailOps(retry, tx.Now().Add(a.cfg.Backoff(retry[0].Attempts+1))); err != nil {
				return err
			}
		}
		if sc := res.ServerChanges; sc != nil {
			cur2, err := tx.FolderByID(f.ID)
			if err != nil {
				return err
			}
			np, err := a.applyItems(tx, cur2, sc.Added, sc.Changed, sc.Deleted, cur2.ReconActive, cur2.ReconEpoch)
			if err != nil {
				return err
			}
			if np {
				a.wakeFetch()
			}
			more = sc.MoreAvailable
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errStaleKey) {
			a.markDirty(f.ID)
			return true
		}
		// Exchange has the changes but this side could not store the result: the
		// same changes would go out again at once.
		a.log.Error("recording sent changes failed; holding the outbox back", "folder", f.ID, "err", err)
		a.outboxStall()
		return true
	}
	a.outboxUnstall()
	if restored {
		a.wakeFetch() // a refused delete queued the message for download again
	}
	if more || res.ServerChanges != nil {
		a.markDirty(f.ID)
	}
	return false
}

func isTransientStatus(code int) bool {
	switch code {
	case 5, 9, 13, 110, 111, 114, 115:
		return true
	}
	return false
}

// rollbackOp undoes a change Exchange refused.
//
//   - A read/flagged change gets its previous value back and the clients hear
//     about it (an EvFlags event becomes FETCH FLAGS).
//   - A delete brought the message down locally at once, so the message must come
//     back: it is queued for download again (the fetch loop stores it under a new
//     UID, like a new arrival) with the flags and received date it had. restored
//     says a download was queued. A delete queued by an older version has no
//     record of those and cannot be undone.
func (a *Account) rollbackOp(tx *store.Tx, folderID int64, o store.OutboxOp) (restored bool, err error) {
	if o.Op == store.OpDelete {
		if o.RestoreDate.IsZero() {
			return false, nil
		}
		a.log.Warn("Exchange refused a delete; the message comes back", "folder", folderID)
		flags := o.RestoreFlags &^ store.FlagDeleted
		return true, tx.UpsertPending(store.PendingFetch{FolderID: folderID, ServerID: o.ServerID, Flags: flags, InternalDate: o.RestoreDate})
	}
	if o.Orig < 0 {
		return false, nil
	}
	m, err := tx.MessageByServerID(folderID, o.ServerID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	nf := m.Flags
	bit := store.FlagSeen
	if o.Op == store.OpFlagged {
		bit = store.FlagFlagged
	}
	if o.Orig == 1 {
		nf |= bit
	} else {
		nf &^= bit
	}
	_, err = tx.SetFlagsPK(m.PK, nf, m.Keywords)
	return false, err
}

// resetFolderKey restarts a folder from sync key "0" with reconciliation. A
// folder that is still doing its first sync has nothing visible to reconcile
// and simply starts its download again.
func (a *Account) resetFolderKey(folderID int64) error {
	_, err := a.commit(nil, func(tx *store.Tx) error {
		cur, err := tx.FolderByID(folderID)
		if err != nil {
			return err
		}
		if err := tx.SetSyncKey(folderID, "0"); err != nil {
			return err
		}
		if !cur.InitialComplete {
			return tx.RestartInitial(folderID)
		}
		return tx.SetRecon(folderID, true, cur.ReconEpoch+1, 0)
	})
	if err != nil {
		a.log.Error("resetting a folder failed", "folder", folderID, "err", err)
		return err
	}
	a.markDirty(folderID)
	return nil
}
