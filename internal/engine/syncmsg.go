package engine

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"easbridge/internal/store"
	"easbridge/internal/upstream"
)

var errStaleKey = errors.New("engine: folder sync key changed underneath a sync batch")

// syncBatch performs one Sync round trip for a folder and commits the result.
func (a *Account) syncBatch(id int64) {
	f, err := a.st.FolderByID(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			a.wmu.Lock()
			delete(a.fs, id) // the folder is gone
			a.wmu.Unlock()
			a.signalClean()
			return
		}
		a.log.Warn("reading a folder failed; its sync waits", "folder", id, "err", err)
		a.folderBackoff(id)
		return
	}
	a.wmu.Lock()
	gen := 0
	if fs := a.fs[id]; fs != nil {
		gen = fs.gen
	}
	a.wmu.Unlock()

	ctx, cancel := a.callCtx()
	delta, err := a.upstream().Sync(ctx, f.ServerID, f.SyncKey, upstream.SyncOptions{HeadersOnly: f.ReconActive})
	cancel()
	if err != nil {
		a.syncFailed(f, err)
		return
	}
	more, err := a.applyDelta(f, f.SyncKey, delta)
	if err != nil {
		if errors.Is(err, errStaleKey) {
			a.markDirty(id)
			return
		}
		a.log.Error("storing a sync batch failed", "folder", id, "err", err)
		a.folderBackoff(id)
		return
	}
	a.lastSync.Store(a.cfg.Clock().Unix())
	a.clearDirty(id, gen, more || delta.Reset || f.SyncKey == "0")
}

// clearDirty ends the dirty state of a folder unless more work remains or it
// was marked dirty again while the batch ran.
func (a *Account) clearDirty(id int64, gen int, keep bool) {
	a.wmu.Lock()
	if fs := a.fs[id]; fs != nil {
		fs.failures = 0
		fs.retryAt = time.Time{}
		if !keep && fs.gen == gen {
			fs.dirty = false
		}
	}
	a.wmu.Unlock()
	if !keep {
		a.signalClean()
	}
}

func (a *Account) folderBackoff(id int64) {
	a.wmu.Lock()
	fs := a.fs[id]
	if fs == nil {
		fs = &folderState{prio: 2, dirty: true}
		a.fs[id] = fs
	}
	fs.failures++
	fs.retryAt = a.backoff(fs.failures)
	a.wmu.Unlock()
}

// syncFailed reacts to a failed Sync call.
func (a *Account) syncFailed(f *store.Folder, err error) {
	if isPaused(err) {
		return // the breaker opened just now: nothing was sent, and the folder stays dirty
	}
	if a.handleUpstreamError(err) {
		return
	}
	var se *upstream.StatusError
	switch {
	case errors.Is(err, upstream.ErrHierarchyChanged), errors.As(err, &se) && (se.Code == 12 || se.Code == 8):
		// Status 12: the hierarchy changed. Status 8: the folder is gone. Both
		// are answered by refreshing the hierarchy first.
		a.log.Info("sync needs a folder refresh first", "folder", f.ID, "err", err)
		a.requestFolderSync()
		a.folderBackoff(f.ID)
	default:
		a.log.Warn("Sync failed", "folder", f.ID, "err", err)
		a.folderBackoff(f.ID)
	}
}

// applyDelta commits one Sync result: items, flag changes, deletions and the
// new sync key, all in one transaction. It reports whether the server has more.
func (a *Account) applyDelta(f *store.Folder, callKey string, d *upstream.SyncDelta) (more bool, err error) {
	var newPending bool
	_, err = a.commit(nil, func(tx *store.Tx) error {
		cur, err := tx.FolderByID(f.ID)
		if err != nil {
			return err
		}
		if cur.SyncKey != callKey {
			return errStaleKey
		}
		if err := tx.SetSyncKey(f.ID, d.NewKey); err != nil {
			return err
		}
		active, epoch := cur.ReconActive, cur.ReconEpoch
		switch {
		case d.Reset && !cur.InitialComplete:
			// Status 3 during the folder's first sync. Nothing was ever visible,
			// so there is nothing to reconcile and no UID to keep: download the
			// folder again from scratch.
			if err := tx.RestartInitial(f.ID); err != nil {
				return err
			}
			a.log.Warn("server reset the sync key during the folder's first sync; downloading it again", "folder", f.ID)
		case d.Reset:
			// Status 3: the server forgot our state. It will re-list everything
			// but cannot say what was deleted in the meantime, so remember which
			// items the complete resync mentions and reconcile at the end. The
			// folder stays open and keeps its UIDs.
			epoch++
			active = true
			if err := tx.SetRecon(f.ID, true, epoch, 0); err != nil {
				return err
			}
			a.log.Warn("server reset the sync key; resyncing the folder and reconciling", "folder", f.ID)
		}
		np, err := a.applyItems(tx, cur, d.Added, d.Changed, d.Deleted, active, epoch)
		if err != nil {
			return err
		}
		newPending = np
		if active && !d.MoreAvailable && !d.Reset && callKey != "0" {
			if err := a.finishRecon(tx, cur, epoch); err != nil {
				return err
			}
		}
		if !cur.InitialComplete {
			// A data call (not the key-"0" bootstrap, not a reset) that left
			// nothing more to fetch ends the first sync's chain; the folder
			// opens once no message is still waiting for its body.
			if !d.Reset && callKey != "0" {
				if err := tx.SetInitialChainDone(f.ID, !d.MoreAvailable); err != nil {
					return err
				}
			}
			return a.completeInitialIfReady(tx, f.ID)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	if newPending {
		a.wakeFetch()
	}
	return d.MoreAvailable, nil
}

func flagsFromServer(cur store.Flags, it upstream.Item, protectSeen, protectFlagged bool) store.Flags {
	f := cur
	if it.ReadSet && !protectSeen {
		if it.Read {
			f |= store.FlagSeen
		} else {
			f &^= store.FlagSeen
		}
	}
	if it.FlagSet && !protectFlagged {
		if it.Flagged {
			f |= store.FlagFlagged
		} else {
			f &^= store.FlagFlagged
		}
	}
	return f
}

// applyItems applies server-side changes inside tx. It is shared by Sync
// batches and by the server changes that come back with ApplyChanges.
func (a *Account) applyItems(tx *store.Tx, f *store.Folder, added, changed []upstream.Item, deleted []string,
	reconActive bool, epoch int64) (newPending bool, err error) {

	for _, sid := range deleted {
		m, err := tx.MessageByServerID(f.ID, sid)
		switch {
		case err == nil:
			// By primary key: a staged message has no UID, and its removal is
			// silent because nobody ever heard of it.
			if _, err := tx.DeleteByPK(m.PK); err != nil {
				return false, err
			}
		case !errors.Is(err, store.ErrNotFound):
			return false, err
		}
		if err := tx.DeletePending(f.ID, sid); err != nil {
			return false, err
		}
		if err := tx.DropOutbox(f.ID, sid); err != nil {
			return false, err
		}
	}

	// Later entries for the same ServerId win.
	byID := map[string]upstream.Item{}
	var order []string
	for _, it := range append(append([]upstream.Item(nil), added...), changed...) {
		if it.ServerID == "" {
			continue
		}
		if _, dup := byID[it.ServerID]; !dup {
			order = append(order, it.ServerID)
		}
		byID[it.ServerID] = it
	}
	if reconActive && len(order) > 0 {
		if err := tx.MarkSeen(f.ID, epoch, order); err != nil {
			return false, err
		}
	}

	var fresh []upstream.Item
	for _, sid := range order {
		it := byID[sid]
		m, err := tx.MessageByServerID(f.ID, sid)
		switch {
		case err == nil:
			if err := a.mergeFlags(tx, f, m, it); err != nil {
				return false, err
			}
		case errors.Is(err, store.ErrNotFound):
			// An item the user deleted whose delete has not reached Exchange yet
			// is still listed by the server. It must not come back as a new
			// message: the delete is on its way (and if Exchange refuses it, the
			// rollback brings the message back).
			queued, err := deleteQueued(tx, f.ID, sid)
			if err != nil {
				return false, err
			}
			if queued {
				continue
			}
			fresh = append(fresh, it)
		default:
			return false, err
		}
	}
	mark := reconMarkOf(reconActive, epoch)

	// New messages get their UIDs in date order, so UIDs grow with age.
	sort.SliceStable(fresh, func(i, j int) bool {
		if !fresh[i].DateReceived.Equal(fresh[j].DateReceived) {
			return fresh[i].DateReceived.Before(fresh[j].DateReceived)
		}
		return fresh[i].ServerID < fresh[j].ServerID
	})
	for _, it := range fresh {
		pend, err := a.addNew(tx, f, it, mark)
		if err != nil {
			return false, err
		}
		newPending = newPending || pend
	}
	return newPending, nil
}

// reconMarkOf is the recon_mark a message gets when it is stored while a full
// resync of its folder is being collected: a message that arrives during the
// epoch has been seen by definition, so the end of the epoch must not take it
// for one the server forgot (0 outside a resync).
func reconMarkOf(active bool, epoch int64) int64 {
	if active {
		return epoch
	}
	return 0
}

// deleteQueued reports whether a delete of the item waits in the outbox.
func deleteQueued(tx *store.Tx, folderID int64, serverID string) (bool, error) {
	ops, err := tx.OutboxFor(folderID, serverID)
	if err != nil {
		return false, err
	}
	for _, o := range ops {
		if o.Op == store.OpDelete {
			return true, nil
		}
	}
	return false, nil
}

// mergeFlags applies the server's flags to a message we already have, except
// for attributes with a local change still waiting in the outbox.
func (a *Account) mergeFlags(tx *store.Tx, f *store.Folder, m *store.Message, it upstream.Item) error {
	if !it.ReadSet && !it.FlagSet {
		return nil
	}
	ops, err := tx.OutboxFor(f.ID, m.ServerID)
	if err != nil {
		return err
	}
	var protectSeen, protectFlagged bool
	for _, o := range ops {
		switch o.Op {
		case store.OpRead:
			protectSeen = true
		case store.OpFlagged:
			protectFlagged = true
		case store.OpDelete:
			return nil
		}
	}
	nf := flagsFromServer(m.Flags, it, protectSeen, protectFlagged)
	if nf == m.Flags {
		return nil
	}
	_, err = tx.SetFlagsPK(m.PK, nf, m.Keywords)
	return err
}

// addNew handles an item we have never stored: it becomes a message if its MIME
// is complete, otherwise a pending fetch. It reports whether a fetch was queued.
func (a *Account) addNew(tx *store.Tx, f *store.Folder, it upstream.Item, mark int64) (pending bool, err error) {
	flags := flagsFromServer(initialFlags(f), it, false, false)
	date := it.DateReceived
	if date.IsZero() {
		date = tx.Now()
	}
	if p, err := tx.PendingByServerID(f.ID, it.ServerID); err == nil {
		// Already waiting for its MIME. Keep the freshest flags.
		pf := flagsFromServer(p.Flags, it, false, false)
		if !(it.MIMEComplete && len(it.MIME) > 0) {
			return false, tx.UpdatePendingFlags(f.ID, it.ServerID, pf)
		}
		flags = pf
		if err := tx.DeletePending(f.ID, it.ServerID); err != nil {
			return false, err
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	if !(it.MIMEComplete && len(it.MIME) > 0) {
		return true, tx.UpsertPending(store.PendingFetch{
			FolderID: f.ID, ServerID: it.ServerID, Flags: flags, InternalDate: date, EstSize: int64(it.EstimatedSize),
			Subject: it.Subject, From: it.From, To: it.To, Bcc: it.Bcc,
		})
	}
	return false, a.insertServerMessage(tx, f, it.ServerID, it.MIME, flags, date, mark, it.Bcc)
}

// insertServerMessage stores a complete server message, binding it to a Sent
// placeholder when it is Exchange's copy of something the client appended. mark
// is the reconciliation epoch the message is born in (see reconMarkOf). bcc is
// the email2:Bcc property Exchange listed with the item ("" when none): a message
// of Sent Items is stored with the bytes the bridge submitted if it submitted it,
// else with its Bcc recipients written back into a Bcc header (submitted.go).
func (a *Account) insertServerMessage(tx *store.Tx, f *store.Folder, serverID string, mime []byte, flags store.Flags, date time.Time, mark int64, bcc string) error {
	claimed := ""
	if f.EASType == store.TypeSentItems {
		bound, err := a.bindPlaceholder(tx, f, serverID, mime, mark)
		if err != nil {
			return err
		}
		if bound {
			return nil
		}
		if mime, claimed, err = a.servedSentMIME(tx, mime, bcc); err != nil {
			return err
		}
	}
	nm := store.NewMessage{ServerID: serverID, Flags: flags, InternalDate: date, MIME: mime, ReconMark: mark}
	if !f.InitialComplete {
		// The folder is still doing its first sync: keep the message out of
		// sight until the folder opens (see completeInitialIfReady).
		if err := tx.AddStagedMessage(f.ID, nm); err != nil {
			return fmt.Errorf("staging %s: %w", "message", err)
		}
		return nil
	}
	if _, err := tx.AddMessage(f.ID, nm); err != nil {
		return fmt.Errorf("storing %s: %w", "message", err)
	}
	if claimed != "" {
		// Visible and bound to Exchange's item, with the submitted bytes: the way
		// station has done its work. (A message that is still staged leaves it to
		// the folder opening, which may still bind a placeholder to it.)
		return tx.DropSentCopy(claimed)
	}
	return nil
}

// completeInitialIfReady opens a folder whose first sync has caught up: the
// last data Sync reported nothing more to fetch and no message is still
// waiting for its body (messages that keep failing on their own account stop
// counting, see maxItemFailures). In one transaction, with the caller's
// changes, every staged message gets its UID in date order and the folder
// becomes visible; listeners hear a single bulk event.
func (a *Account) completeInitialIfReady(tx *store.Tx, folderID int64) error {
	f, err := tx.FolderByID(folderID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if f.InitialComplete || !f.InitialChainDone {
		return nil
	}
	waiting, err := tx.PendingBlocking(folderID, maxItemFailures)
	if err != nil || waiting > 0 {
		return err
	}
	if f.EASType == store.TypeSentItems {
		if err := a.bindStagedPlaceholders(tx, f); err != nil {
			return err
		}
		// What is left staged is stored with the submitted bytes where there are any;
		// the registrations have done their work (submitted.go).
		if err := tx.DropSentCopiesOfStaged(folderID); err != nil {
			return err
		}
	}
	n, err := tx.CompleteInitialSync(folderID)
	if err != nil {
		return err
	}
	a.log.Info("folder's first sync is complete; its messages are visible now", "folder", folderID, "messages", n)
	return nil
}

// openReadyFolders is the safety net for folders that wait for message bodies
// (or for a message to give up): it opens any whose conditions are met.
func (a *Account) openReadyFolders(folders []store.Folder) {
	for i := range folders {
		f := &folders[i]
		if f.InitialComplete || !f.InitialChainDone {
			continue
		}
		if n, err := a.st.PendingBlockingCount(f.ID, maxItemFailures); err != nil || n > 0 {
			continue
		}
		if _, err := a.commit(nil, func(tx *store.Tx) error { return a.completeInitialIfReady(tx, f.ID) }); err != nil {
			a.log.Error("opening a folder after its first sync failed", "folder", f.ID, "err", err)
		}
	}
}

func initialFlags(f *store.Folder) store.Flags {
	if f.EASType == store.TypeDrafts {
		return store.FlagDraft
	}
	return 0
}

// finishRecon ends a reconciliation epoch: server-bound messages the complete
// resync did not list are suspects. A suspect is removed only if the previous
// complete resync missed it as well (two consecutive confirmations); otherwise
// it gets a strike and a verification resync is scheduled.
func (a *Account) finishRecon(tx *store.Tx, f *store.Folder, epoch int64) error {
	absent, err := tx.ReconAbsent(f.ID, epoch)
	if err != nil {
		return err
	}
	removed, struck := 0, 0
	for _, m := range absent {
		if m.ReconStrikes >= 1 {
			if _, err := tx.DeleteMessage(f.ID, m.UID); err != nil {
				return err
			}
			if err := tx.DropOutbox(f.ID, m.ServerID); err != nil {
				return err
			}
			removed++
			continue
		}
		if err := tx.SetStrikes(f.ID, m.UID, 1); err != nil {
			return err
		}
		struck++
	}
	if err := tx.ClearStrikes(f.ID, epoch); err != nil {
		return err
	}
	var verifyAt int64
	if struck > 0 {
		verifyAt = tx.Now().Add(a.cfg.ReconVerifyDelay).Unix()
	}
	if err := tx.SetRecon(f.ID, false, epoch, verifyAt); err != nil {
		return err
	}
	a.log.Info("reconciliation finished", "folder", f.ID, "removed", removed, "suspected", struck)
	return nil
}

// startVerification begins the second complete resync that confirms suspected
// deletions. It restarts the folder from key "0".
func (a *Account) startVerification(f *store.Folder) {
	_, err := a.commit(nil, func(tx *store.Tx) error {
		cur, err := tx.FolderByID(f.ID)
		if err != nil {
			return err
		}
		if cur.ReconActive || cur.ReconVerifyAt == 0 {
			return nil
		}
		if err := tx.SetSyncKey(f.ID, "0"); err != nil {
			return err
		}
		return tx.SetRecon(f.ID, true, cur.ReconEpoch+1, 0)
	})
	if err != nil {
		a.log.Error("cannot start verification resync", "folder", f.ID, "err", err)
		return
	}
	a.markDirty(f.ID)
}

// housekeeping expires Sent placeholders and starts due verification resyncs.
func (a *Account) housekeeping(now time.Time) {
	a.wmu.Lock()
	a.nextHousekeep = now.Add(a.cfg.Housekeeping)
	a.wmu.Unlock()
	var expired, pruned int
	if _, err := a.commit(nil, func(tx *store.Tx) error {
		var err error
		if expired, err = tx.ExpirePlaceholders(); err != nil {
			return err
		}
		pruned, err = tx.PruneSentCopies(a.cfg.SubmittedTTL, a.cfg.SubmittedMax, a.cfg.SubmittedMaxBytes)
		return err
	}); err != nil {
		a.log.Error("housekeeping failed", "err", err)
		return
	}
	if expired > 0 {
		a.log.Info("Sent placeholders gave up waiting for Exchange's copy; they stay as local-only messages", "count", expired)
	}
	if pruned > 0 {
		a.log.Info("copies of submitted messages that Exchange's Sent copy never claimed were dropped", "count", pruned)
	}
	folders, err := a.st.Folders()
	if err != nil {
		return
	}
	if (a.pingLimited.Load() || a.pingDegraded.Load()) && !now.Before(a.nextSweep) {
		a.nextSweep = now.Add(a.cfg.SweepInterval)
		a.markAllDirty()
	}
	a.openReadyFolders(folders)
	for i := range folders {
		f := &folders[i]
		if f.ReconVerifyAt != 0 && !f.ReconActive && now.Unix() >= f.ReconVerifyAt {
			a.startVerification(f)
		}
	}
}
