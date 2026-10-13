package engine

import (
	"errors"
	"time"

	"easbridge/internal/store"
	"easbridge/internal/upstream"
	"easbridge/internal/vcard"
)

// Contacts folders (docs/DESIGN-contacts.md section 3). The folders of type 9 and 14
// are synced by the engine's single worker, after the mail work, one Sync round trip
// at a time, each when it is due: at start, then every ContactsInterval, and at once
// while a download is under way. They are not part of Ping.

// contactState is the schedule of one Contacts folder.
type contactState struct {
	due      time.Time // when the next sync is due
	failures int
	retryAt  time.Time // not before (back-off after a failure)
}

// wakeAt is the earliest time the folder can be synced.
func (c *contactState) wakeAt() time.Time {
	if c.retryAt.After(c.due) {
		return c.retryAt
	}
	return c.due
}

// resumeContacts schedules every stored Contacts folder once at start-up (contacts
// may have changed while the bridge was down).
func (a *Account) resumeContacts() {
	if !a.cfg.ContactsEnabled {
		return
	}
	fs, err := a.st.ContactFolders()
	if err != nil {
		a.log.Error("reading the contact folders failed", "err", err)
		return
	}
	ids := make([]int64, 0, len(fs))
	for _, f := range fs {
		ids = append(ids, f.ID)
	}
	a.markContactsDue(ids...)
}

// markContactsDue makes the folders due now.
func (a *Account) markContactsDue(ids ...int64) {
	now := a.cfg.Clock()
	a.wmu.Lock()
	for _, id := range ids {
		cs := a.cstate[id]
		if cs == nil {
			cs = &contactState{}
			a.cstate[id] = cs
		}
		cs.due = now
	}
	a.wmu.Unlock()
	a.poke()
}

// contactFoldersChanged reacts to the result of a hierarchy sync.
func (a *Account) contactFoldersChanged(d store.ContactFolderDiff) {
	if d.Empty() {
		return
	}
	a.wmu.Lock()
	for _, f := range d.Removed {
		delete(a.cstate, f.ID)
	}
	a.wmu.Unlock()
	ids := make([]int64, 0, len(d.Added))
	for _, f := range d.Added {
		ids = append(ids, f.ID)
	}
	a.markContactsDue(ids...)
	a.log.Info("contact folders updated", "added", len(d.Added), "removed", len(d.Removed), "renamed", len(d.Renamed))
}

// contactsStep syncs one Contacts folder that is due and reports whether it did.
func (a *Account) contactsStep(now time.Time) bool {
	if !a.cfg.ContactsEnabled {
		return false
	}
	a.wmu.Lock()
	var pick int64
	var pickDue time.Time
	for id, cs := range a.cstate {
		if now.Before(cs.due) || now.Before(cs.retryAt) {
			continue
		}
		if pick == 0 || cs.due.Before(pickDue) || (cs.due.Equal(pickDue) && id < pick) {
			pick, pickDue = id, cs.due
		}
	}
	a.wmu.Unlock()
	if pick == 0 {
		return false
	}
	a.syncContactFolder(pick)
	return true
}

// syncContactFolder performs one Sync round trip for a Contacts folder and commits
// the result with the new key.
func (a *Account) syncContactFolder(id int64) {
	f, err := a.st.ContactFolderByID(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			a.wmu.Lock()
			delete(a.cstate, id) // the folder is gone
			a.wmu.Unlock()
			return
		}
		a.log.Warn("reading a contact folder failed; its sync waits", "folder", id, "err", err)
		a.contactsBackoff(id)
		return
	}
	ctx, cancel := a.callCtx()
	delta, err := a.upstream().SyncContacts(ctx, f.ServerID, f.SyncKey)
	cancel()
	if err != nil {
		a.contactsFailed(f, err)
		return
	}
	batch := store.ContactBatch{CallKey: f.SyncKey, NewKey: delta.NewKey, More: delta.MoreAvailable, Reset: delta.Reset, Deleted: delta.Deleted}
	for _, group := range [][]upstream.Contact{delta.Added, delta.Changed} {
		for _, c := range group {
			if c.ServerID == "" {
				continue
			}
			card := vcard.FromContact(a.user, f.ServerID, c)
			batch.Upserts = append(batch.Upserts, store.ContactCardIn{ServerID: c.ServerID, UID: card.UID, VCard: card.Marshal()})
		}
	}
	var res store.ContactBatchResult
	if _, err := a.commit(nil, func(tx *store.Tx) error {
		var err error
		res, err = tx.ApplyContactBatch(f.ID, batch)
		return err
	}); err != nil {
		if errors.Is(err, store.ErrStaleKey) {
			a.markContactsDue(id)
			return
		}
		a.log.Error("storing a contacts sync batch failed", "folder", id, "err", err)
		a.contactsBackoff(id)
		return
	}
	if delta.Reset {
		a.log.Warn("server reset the contacts sync key; downloading the folder again (the cards stay until the new download is complete)", "folder", id)
	}
	a.log.Debug("contacts sync batch", "folder", id, "adds", len(delta.Added), "changes", len(delta.Changed),
		"deletes", len(delta.Deleted), "more", delta.MoreAvailable, "reset", delta.Reset)
	if res.Completed {
		n := 0
		if cs, err := a.st.ContactStats(); err == nil {
			for _, s := range cs {
				n += s.Cards
			}
		}
		a.log.Info("contact book downloaded", "folder", id, "cards_in_all_books", n)
	}
	// Under way (a download, or more to fetch): continue at once. Otherwise wait for the interval.
	a.wmu.Lock()
	cs := a.cstate[id]
	if cs == nil {
		cs = &contactState{}
		a.cstate[id] = cs
	}
	cs.failures, cs.retryAt = 0, time.Time{}
	now := a.cfg.Clock()
	if res.Downloading || delta.MoreAvailable || delta.Reset || batch.CallKey == "0" {
		cs.due = now
	} else {
		cs.due = now.Add(a.cfg.ContactsInterval)
	}
	a.wmu.Unlock()
}

// contactsFailed reacts to a failed contacts Sync.
func (a *Account) contactsFailed(f *store.ContactFolder, err error) {
	if isPaused(err) {
		return // the breaker opened: nothing was sent; the folder stays due
	}
	if a.handleUpstreamError(err) {
		return
	}
	var se *upstream.StatusError
	switch {
	case errors.Is(err, upstream.ErrHierarchyChanged), errors.As(err, &se) && (se.Code == 12 || se.Code == 8):
		a.log.Info("contacts sync needs a folder refresh first", "folder", f.ID, "err", err)
		a.requestFolderSync()
	default:
		a.log.Warn("contacts Sync failed", "folder", f.ID, "err", err)
	}
	a.contactsBackoff(f.ID)
}

func (a *Account) contactsBackoff(id int64) {
	a.wmu.Lock()
	cs := a.cstate[id]
	if cs == nil {
		cs = &contactState{}
		a.cstate[id] = cs
	}
	cs.failures++
	cs.retryAt = a.backoff(cs.failures)
	a.wmu.Unlock()
}

// contactsStatus describes the contacts feature of the account for /healthz.
func (a *Account) contactsStatus() *ContactsStatus {
	cs := &ContactsStatus{Books: []BookStatus{}, DirectoryEnabled: a.cfg.Directory.Enabled()}
	if stats, err := a.st.ContactStats(); err == nil {
		for _, s := range stats {
			cs.Books = append(cs.Books, BookStatus{Complete: s.Complete, Downloading: s.Downloading, Cards: s.Cards})
		}
	}
	if !cs.DirectoryEnabled {
		return cs
	}
	cs.DirectoryCards, _ = a.st.DirectoryCardCount()
	now := a.cfg.Clock()
	cs.DirectoryQueue, _ = a.st.CandidateQueue(now, a.cfg.Directory.Domains, a.user)
	if st, err := a.st.DirectoryState(); err == nil {
		cs.DirectoryLookupsToday = st.LookupsOn(now.UTC().Format("2006-01-02"))
		if st.PausedUntil > now.Unix() {
			cs.DirectoryPausedUntil = st.PausedUntil
		}
	}
	if a.cfg.Directory.Full {
		cs.Sweep = a.sweepStatus()
	}
	return cs
}
