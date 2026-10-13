package engine

import (
	"errors"
	"strconv"
	"time"

	"easbridge/internal/store"
	"easbridge/internal/upstream"
)

// Messages whose MIME was truncated in a Sync response wait in pending_fetch and
// are completed here with ItemOperations Fetch, FetchWorkers at a time. A
// message gets its UID (and IMAP clients hear about it) only once the complete
// MIME is stored, so BODYSTRUCTURE and RFC822.SIZE are always right. A failing
// fetch never blocks Sync: the key was committed together with the pending row.

const maxFetchBackoff = time.Hour

// maxItemFailures is how many times a message may fail to download for reasons
// of its own (Exchange answers with a permanent status for that item, or with no
// body) before it stops holding up the first sync of its folder. Connection
// trouble and transient statuses (server busy, retry later: the codes
// permanentStatus lists) do not count: an outage must not open a folder without
// its large messages.
// Such a message is still retried and shows up like any later arrival once it
// can be fetched. With the default backoff the fifth failure comes after about a
// minute and a half of consistent failure.
const maxItemFailures = 5

// itemSpecific says whether a failed fetch is about the item itself rather than
// about the connection or the server as a whole. Only a permanent status of the
// item is: the transient ones (busy, retry later, policy refresh) describe the
// server, and so do the permanent ones about the device or the account (a blocked
// device, a disabled account, a policy the server enforces). Counting those against
// a message would, after ten tries, replace it for good by a stand-in although
// nothing is wrong with it.
func itemSpecific(err error) bool {
	var se *upstream.StatusError
	return errors.Is(err, upstream.ErrNoMIME) || (errors.As(err, &se) && se.Perm && !se.Global)
}

func (a *Account) fetchLoop() {
	for {
		if a.ctx.Err() != nil {
			return
		}
		if a.authFailed.Load() || a.wiped.Load() {
			a.waitFetch(30 * time.Second)
			continue
		}
		if !a.pauseWait() { // the breaker is open: no request leaves until it closes
			return
		}
		p, key := a.claimPending()
		if p == nil {
			a.waitFetch(a.fetchIdle())
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					a.log.Error("fetch panic", "panic", r)
				}
				a.releasePending(key)
			}()
			a.fetchOne(p)
		}()
	}
}

// fetchIdle says how long to wait when nothing can be fetched right now: until
// the earliest backed-off item is due, or a long while if none waits (new items
// wake the loop).
func (a *Account) fetchIdle() time.Duration {
	due, ok, err := a.st.PendingNext()
	if err != nil || !ok {
		return time.Minute
	}
	now := a.cfg.Clock()
	d := due.Sub(now)
	switch {
	case d < 20*time.Millisecond:
		// Due, but in flight elsewhere or held back in memory (see holdFetch).
		// An item in flight wakes the loop when it ends; a hold ends at its time.
		d = 20 * time.Millisecond
		if t, held := a.earliestHold(now); held && t.Sub(now) > d {
			d = t.Sub(now)
		}
	case d > time.Minute:
		return time.Minute
	}
	if d > time.Minute {
		d = time.Minute
	}
	return d
}

// fetchHold is the in-memory back-off of one message.
type fetchHold struct {
	until time.Time
	fails int // failures the database could not record, for the growth of the wait
}

// holdFetch keeps a message out of the fetch loop without the database, for a
// time that grows with every failure that could not be recorded. It is the safety
// net for a database that cannot record the usual back-off (BumpPending): the
// item would be due again at once and be fetched from Exchange again and again.
func (a *Account) holdFetch(p *store.PendingFetch) {
	k := pendKey(p)
	a.wmu.Lock()
	defer a.wmu.Unlock()
	h := a.fetchRetry[k]
	h.fails++
	d := a.cfg.Backoff(p.Attempts + h.fails)
	if d > maxFetchBackoff {
		d = maxFetchBackoff
	}
	h.until = a.cfg.Clock().Add(d)
	a.fetchRetry[k] = h
}

func (a *Account) releaseHold(p *store.PendingFetch) {
	a.wmu.Lock()
	delete(a.fetchRetry, pendKey(p))
	a.wmu.Unlock()
}

// earliestHold returns when the first in-memory hold ends.
func (a *Account) earliestHold(now time.Time) (time.Time, bool) {
	a.wmu.Lock()
	defer a.wmu.Unlock()
	var first time.Time
	for _, h := range a.fetchRetry {
		if h.until.After(now) && (first.IsZero() || h.until.Before(first)) {
			first = h.until
		}
	}
	return first, !first.IsZero()
}

func (a *Account) waitFetch(d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-a.fetchWake:
	case <-t.C:
	case <-a.ctx.Done():
	}
}

func pendKey(p *store.PendingFetch) string {
	return strconv.FormatInt(p.FolderID, 10) + "|" + p.ServerID
}

func (a *Account) claimPending() (*store.PendingFetch, string) {
	now := a.cfg.Clock()
	due, err := a.st.PendingDue(now, 64)
	if err != nil {
		return nil, ""
	}
	a.wmu.Lock()
	defer a.wmu.Unlock()
	for k, h := range a.fetchRetry {
		if now.After(h.until.Add(maxFetchBackoff)) {
			delete(a.fetchRetry, k) // long forgotten (this keeps the map from growing)
		}
	}
	for i := range due {
		k := pendKey(&due[i])
		if h, held := a.fetchRetry[k]; held && now.Before(h.until) {
			continue
		}
		if !a.inflight[k] {
			a.inflight[k] = true
			return &due[i], k
		}
	}
	return nil, ""
}

func (a *Account) releasePending(key string) {
	a.wmu.Lock()
	delete(a.inflight, key)
	a.wmu.Unlock()
	// Another item may be waiting behind this one.
	a.wakeFetch()
}

func (a *Account) fetchOne(p *store.PendingFetch) {
	f, err := a.st.FolderByID(p.FolderID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			a.dropPending(p) // the folder is gone, and its pending rows with it
		} else {
			a.log.Warn("reading a folder failed; the message waits", "err", err)
			a.holdFetch(p)
		}
		return
	}
	ctx, cancel := a.callCtx()
	mime, err := a.upstream().FetchMIME(ctx, f.ServerID, p.ServerID)
	cancel()
	switch {
	case err == nil && len(mime) > 0:
		if err := a.completePending(p, f, mime); err != nil {
			a.log.Error("storing a fetched message failed", "folder", f.ID, "err", err)
			_, _, _, _ = a.failPending(p, false)
		}
	case errors.Is(err, upstream.ErrNotFound):
		a.dropPending(p) // deleted on the server before we got it
	case isPaused(err):
		// The breaker opened between the check and the call: nothing was sent, so
		// there is nothing to record against the message.
	case err != nil && a.handleUpstreamError(err):
	default:
		if err == nil {
			err = upstream.ErrNoMIME
		}
		own := itemSpecific(err)
		attempts, ownFailures, standIn, rerr := a.failPending(p, own)
		switch {
		case rerr != nil:
			a.log.Debug("fetch failed", "folder", f.ID, "err", err)
		case standIn:
			a.log.Error("Exchange cannot provide the body of a message after repeated tries; showing a stand-in with the subject, sender and date instead",
				"folder", f.ID, "attempts", attempts, "err", err)
		case own && ownFailures == maxItemFailures:
			a.log.Error("a message cannot be downloaded; its folder no longer waits for it and the message follows once it can be fetched",
				"folder", f.ID, "attempts", attempts, "err", err)
		case attempts == 5:
			a.log.Error("a message cannot be downloaded and stays invisible until it can", "folder", f.ID, "attempts", attempts, "err", err)
		default:
			a.log.Debug("fetch failed", "folder", f.ID, "err", err)
		}
	}
}

// fetchBackoff is the wait after the next failed attempt for a message.
func (a *Account) fetchBackoff(p *store.PendingFetch) time.Duration {
	d := a.cfg.Backoff(p.Attempts + 1)
	if d > maxFetchBackoff {
		d = maxFetchBackoff
	}
	return d
}

// dropPending forgets a message that cannot be fetched any more. If the
// database cannot record that, the message is held back in memory instead of
// being asked for again at once.
func (a *Account) dropPending(p *store.PendingFetch) {
	_, err := a.commit(nil, func(tx *store.Tx) error {
		if err := tx.DeletePending(p.FolderID, p.ServerID); err != nil {
			return err
		}
		// It may have been the last message its folder was waiting for.
		return a.completeInitialIfReady(tx, p.FolderID)
	})
	if err != nil {
		a.log.Warn("could not forget a message that is gone; holding it back", "err", err)
		a.holdFetch(p)
		return
	}
	a.releaseHold(p)
}

// failPending records a failed attempt and returns the item's totals afterwards,
// read inside the transaction (the caller's copy of the row may be stale: another
// worker can have finished an attempt on the same item since it was listed).
//
// When the database cannot record the attempt the message is held back in memory
// instead (holdFetch) and err is set: without that the very same item would be
// due again at once and every pass would be another request to Exchange.
func (a *Account) failPending(p *store.PendingFetch, itemSpecific bool) (attempts, ownFailures int, standIn bool, err error) {
	d := a.fetchBackoff(p)
	_, err = a.commit(nil, func(tx *store.Tx) error {
		standIn = false
		if err := tx.BumpPending(p.ID, a.cfg.Clock().Add(d), itemSpecific); err != nil {
			return err
		}
		if cur, err := tx.PendingByServerID(p.FolderID, p.ServerID); err == nil {
			attempts, ownFailures = cur.Attempts, cur.ItemFailures
			if itemSpecific && cur.ItemFailures >= standInAfter {
				if err := a.replaceWithStandIn(tx, cur); err != nil {
					return err
				}
				standIn = true
			}
		}
		// A message that has given up no longer holds its folder back.
		return a.completeInitialIfReady(tx, p.FolderID)
	})
	if err != nil {
		a.log.Warn("could not record a failed download; holding the message back", "err", err)
		a.holdFetch(p)
		return p.Attempts + 1, p.ItemFailures, false, err
	}
	a.releaseHold(p)
	return attempts, ownFailures, standIn, nil
}

// replaceWithStandIn gives up on an item whose body Exchange cannot provide: the
// stand-in (see standin.go) is stored in its place and the item is not fetched
// again.
func (a *Account) replaceWithStandIn(tx *store.Tx, p *store.PendingFetch) error {
	f, err := tx.FolderByID(p.FolderID)
	if err != nil {
		return err
	}
	if err := tx.DeletePending(p.FolderID, p.ServerID); err != nil {
		return err
	}
	if _, err := tx.MessageByServerID(p.FolderID, p.ServerID); err == nil {
		return nil // stored meanwhile (a replayed Sync): nothing to stand in for
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return a.insertServerMessage(tx, f, p.ServerID, standInMIME(p), p.Flags, p.InternalDate, reconMarkOf(f.ReconActive, f.ReconEpoch), "")
}

// completePending stores the fetched message. The pending row is re-read inside
// the transaction: if a Sync deleted the item meanwhile, the bytes are dropped.
func (a *Account) completePending(p *store.PendingFetch, f *store.Folder, mime []byte) error {
	_, err := a.commit(nil, func(tx *store.Tx) error {
		cur, err := tx.PendingByServerID(p.FolderID, p.ServerID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		fresh, err := tx.FolderByID(p.FolderID)
		if err != nil {
			return err
		}
		if _, err := tx.MessageByServerID(p.FolderID, p.ServerID); err == nil {
			// Already stored by a replayed Sync (or moved in by a client).
			if err := tx.DeletePending(p.FolderID, p.ServerID); err != nil {
				return err
			}
			return a.completeInitialIfReady(tx, p.FolderID)
		}
		if err := tx.DeletePending(p.FolderID, p.ServerID); err != nil {
			return err
		}
		if err := a.insertServerMessage(tx, fresh, p.ServerID, mime, cur.Flags, cur.InternalDate, reconMarkOf(fresh.ReconActive, fresh.ReconEpoch), cur.Bcc); err != nil {
			return err
		}
		// Stored (or staged): if it was the last message the folder waited for,
		// the folder opens in this very transaction.
		return a.completeInitialIfReady(tx, p.FolderID)
	})
	if err == nil {
		a.releaseHold(p)
	}
	return err
}
