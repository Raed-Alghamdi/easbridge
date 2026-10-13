package engine

import (
	"errors"
	"strings"
	"time"

	"easbridge/internal/store"
	"easbridge/internal/upstream"
	"easbridge/internal/vcard"
)

// The directory (docs/DESIGN-contacts.md section 4): the Global Address List can be
// searched but not listed, so the engine looks up, one search at a time and
// rate-limited, the addresses in the directory domains that appear in the user's mail,
// and keeps a card only for an entry whose EmailAddress is exactly the address looked
// for. It never enumerates: no wildcard, no search for anything but a candidate's
// address, local part or display name, and every other entry a search returns is
// dropped unread. The loop is a goroutine of the engine, so it runs only while the
// engine holds the account's credentials.

// directoryLoop seeds the candidates from the stored mail, then looks them up.
func (a *Account) directoryLoop() {
	if !a.directorySeed() {
		return
	}
	for a.ctx.Err() == nil {
		did, wait, idle := a.directoryStep()
		if !did && idle && a.cfg.Directory.Full {
			// Nothing from the mail is due and a search may be sent: the sweep uses what is left.
			did, wait = a.sweepStep()
		}
		if did {
			continue
		}
		t := time.NewTimer(wait)
		select {
		case <-a.dirWake:
		case <-t.C:
		case <-a.ctx.Done():
		}
		t.Stop()
	}
}

// directorySeed counts the addresses in the stored messages once, in batches of
// transactions of their own, and starts the noting of new messages when it is done. It
// runs again, from the start, if the domain list changed since. It reports false when
// the engine stopped first.
//
// A message stored in the instant between the last batch and the switch to noting is
// not counted; the figures are approximations (they order the lookups) and a later
// message from the same address makes up for it.
func (a *Account) directorySeed() bool {
	db := a.st.DB()
	domains := a.cfg.Directory.Domains
	key := store.DomainsKey(domains)
	for a.ctx.Err() == nil {
		st, err := a.st.DirectoryState()
		if err != nil {
			a.log.Error("reading the directory state failed", "err", err)
			if !a.sleepCtx(a.cfg.Directory.Poll) {
				return false
			}
			continue
		}
		if st.SeededFor == key && st.SeedDone {
			db.SetCandidateNoting(a.st.ID, true)
			return true
		}
		after := st.SeedAfter
		recount := false
		if st.SeededFor != key {
			after, recount = 0, st.SeededFor != ""
		}
		for a.ctx.Err() == nil {
			var last int64
			var done bool
			if _, err := a.commit(nil, func(tx *store.Tx) error {
				if recount && after == 0 {
					if err := tx.ResetCandidateCounts(); err != nil {
						return err
					}
				}
				var err error
				if last, done, err = tx.SeedCandidates(after, a.cfg.Directory.SeedBatch, domains); err != nil {
					return err
				}
				return tx.SetSeedProgress(key, last, done)
			}); err != nil {
				a.log.Error("seeding the directory candidates failed; trying again", "err", err)
				if !a.sleepCtx(a.cfg.Directory.Poll) {
					return false
				}
				break // read the state again
			}
			recount, after = false, last
			if done {
				db.SetCandidateNoting(a.st.ID, true)
				n := 0
				if cs, err := a.st.Candidates(); err == nil {
					n = len(cs)
				}
				a.log.Info("directory candidates seeded from the stored mail", "candidates", n)
				return true
			}
		}
	}
	return false
}

// slot says whether a search may be sent now. wait is how long until it may; hard
// says the reason is not the plain spacing (the breaker is open, the directory is
// paused, the day's cap is reached, a back-off runs, the engine has no working
// credentials): a lookup in progress gives up then instead of waiting.
func (a *Account) slot(now time.Time) (wait time.Duration, hard bool) {
	cfg := a.cfg.Directory
	if a.authFailed.Load() || a.wiped.Load() {
		return cfg.Poll, true
	}
	if d := a.brk.remaining(); d > 0 {
		return d, true
	}
	st, err := a.st.DirectoryState()
	if err != nil {
		return cfg.Poll, true
	}
	if until := time.Unix(st.PausedUntil, 0); st.PausedUntil != 0 && now.Before(until) {
		return until.Sub(now), true
	}
	if !a.dirRetryAt.IsZero() && now.Before(a.dirRetryAt) {
		return a.dirRetryAt.Sub(now), true
	}
	day := now.UTC().Format("2006-01-02")
	if st.LookupsOn(day) >= cfg.DailyCap {
		next := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day()+1, 0, 0, 0, 0, time.UTC)
		return next.Sub(now), true
	}
	if st.LastSearchMs != 0 {
		if since := now.Sub(time.UnixMilli(st.LastSearchMs)); since < cfg.Spacing {
			return cfg.Spacing - since, false
		}
	}
	return 0, false
}

// directoryStep looks up one candidate if one is due and a search may be sent. It
// reports whether it searched (so the loop goes round again at once) and otherwise how
// long to wait before looking again: until the spacing allows the next search, at
// most the polling interval.
//
// idle is true when a search may be sent now but no candidate is due: the moment for the sweep.
func (a *Account) directoryStep() (did bool, wait time.Duration, idle bool) {
	cfg := a.cfg.Directory
	now := a.cfg.Clock()
	if w, _ := a.slot(now); w > 0 {
		if w > cfg.Poll {
			w = cfg.Poll
		}
		return false, w, false
	}
	c, err := a.st.NextCandidate(now, cfg.Domains, a.user)
	if err != nil {
		a.log.Error("reading the next directory candidate failed", "err", err)
		return false, cfg.Poll, false
	}
	if c == nil {
		return false, cfg.Poll, true
	}
	a.lookup(c)
	return true, 0, false
}

// ladder is the searches for a candidate, at most three: the full address, the local
// part, and the display name of a message header if there is one.
func ladder(c *store.Candidate) []string {
	qs := []string{c.Email}
	lp, _, _ := strings.Cut(c.Email, "@")
	if lp != "" {
		qs = append(qs, lp)
	}
	if n := strings.TrimSpace(c.Name); n != "" && !strings.EqualFold(n, c.Email) && !strings.EqualFold(n, lp) {
		qs = append(qs, n)
	}
	return qs
}

// lookup runs the ladder for one candidate and records the outcome. Only an entry
// whose EmailAddress equals the candidate is kept.
func (a *Account) lookup(c *store.Candidate) {
	var hit *upstream.DirectoryEntry
	for i, q := range ladder(c) {
		if i > 0 && !a.awaitSlot() {
			return // a pause, the breaker or the cap: the candidate stays due
		}
		res, err := a.search(q)
		if err != nil {
			a.directoryFailed(c, err)
			return
		}
		a.dirFailures = 0
		for j := range res.Entries {
			if strings.EqualFold(strings.TrimSpace(res.Entries[j].EmailAddress), c.Email) {
				e := res.Entries[j]
				hit = &e
				break
			}
		}
		if hit != nil {
			break
		}
	}
	a.applyLookup(c, hit)
}

// awaitSlot waits for the spacing between two searches of one ladder. It gives up
// (false) if the reason for waiting is something else.
func (a *Account) awaitSlot() bool {
	for {
		w, hard := a.slot(a.cfg.Clock())
		if w <= 0 {
			return true
		}
		if hard {
			return false
		}
		if w > a.cfg.Directory.Poll {
			w = a.cfg.Directory.Poll
		}
		if !a.sleepCtx(w) {
			return false
		}
	}
}

// search sends one GAL search. It is counted (and the time kept, for the spacing) before
// it leaves, so a crash cannot make a search free.
func (a *Account) search(query string) (*upstream.DirectoryResult, error) {
	if _, err := a.commit(nil, func(tx *store.Tx) error { return tx.RecordSearch(a.cfg.Clock()) }); err != nil {
		return nil, err
	}
	a.dirSearched.Add(1)
	ctx, cancel := a.callCtx()
	defer cancel()
	return a.upstream().SearchDirectory(ctx, query)
}

// applyLookup records the outcome of a lookup (hit is nil when no entry matched).
func (a *Account) applyLookup(c *store.Candidate, hit *upstream.DirectoryEntry) {
	cfg := a.cfg.Directory
	now := a.cfg.Clock()
	removed := false
	_, err := a.commit(nil, func(tx *store.Tx) error {
		cur, err := tx.Candidate(c.Email)
		if err != nil {
			return err
		}
		if hit != nil {
			card := vcard.FromDirectory(*hit)
			changed, err := tx.PutDirectoryCard(c.Email, card.Marshal())
			if err != nil {
				return err
			}
			if !changed {
				if err := tx.TouchDirectoryCard(c.Email); err != nil {
					return err
				}
			}
			return tx.UpdateCandidate(c.Email, store.CandidateUpdate{State: store.CandFound, CheckedAt: now, NextCheck: now.Add(cfg.Refresh)})
		}
		if cur.State == store.CandFound {
			// A card that was found before and is not found now: the person may have left.
			// One miss schedules another try after MissRecheck; a second miss at least that
			// long after the first removes the card.
			first := time.Unix(cur.MissFirst, 0)
			switch {
			case cur.MissFirst == 0:
				return tx.UpdateCandidate(c.Email, store.CandidateUpdate{State: store.CandFound, CheckedAt: now, NextCheck: now.Add(cfg.MissRecheck), MissFirst: now})
			case now.Sub(first) >= cfg.MissRecheck:
				if _, err := tx.DeleteDirectoryCard(c.Email); err != nil {
					return err
				}
				removed = true
				return tx.UpdateCandidate(c.Email, store.CandidateUpdate{State: store.CandNotFound, CheckedAt: now, NextCheck: now.Add(cfg.NotFoundRetry)})
			default:
				return tx.UpdateCandidate(c.Email, store.CandidateUpdate{State: store.CandFound, CheckedAt: now, NextCheck: first.Add(cfg.MissRecheck), MissFirst: first})
			}
		}
		return tx.UpdateCandidate(c.Email, store.CandidateUpdate{State: store.CandNotFound, CheckedAt: now, NextCheck: now.Add(cfg.NotFoundRetry)})
	})
	if err != nil {
		a.log.Error("storing the outcome of a directory lookup failed", "err", err)
		return
	}
	if removed {
		a.log.Info("a directory card was removed: two lookups at least a week apart found no exact match")
	}
}

// directoryFailed reacts to a failed search. Nothing is recorded about the candidate
// (it stays due), except that a refusal of the query itself postpones it a day.
func (a *Account) directoryFailed(c *store.Candidate, err error) {
	if !a.directoryFailure(err) {
		return
	}
	now := a.cfg.Clock()
	a.log.Warn("the server refused a directory search permanently; the candidate is postponed", "err", err)
	if _, e := a.commit(nil, func(tx *store.Tx) error {
		cur, err := tx.Candidate(c.Email)
		if err != nil {
			return err
		}
		return tx.UpdateCandidate(c.Email, store.CandidateUpdate{State: cur.State, CheckedAt: now, NextCheck: now.Add(24 * time.Hour), MissFirst: time.Unix(cur.MissFirst, 0)})
	}); e != nil {
		a.log.Error("postponing a directory candidate failed", "err", e)
	}
}

// directoryFailure handles what every failed search has in common, for a lookup and for the
// sweep: an open breaker, a wipe or a rejected password (nothing to do here), access denied (the
// directory stops for a day), and throttling, time-outs and server errors (a back-off that
// honours Retry-After). It returns true only for a permanent refusal of the query itself (an
// invalid or too complex query), which the caller has to deal with.
func (a *Account) directoryFailure(err error) (refused bool) {
	if isPaused(err) || a.handleUpstreamError(err) {
		return false
	}
	cfg := a.cfg.Directory
	now := a.cfg.Clock()
	if upstream.DirectoryDenied(err) {
		until := now.Add(cfg.DenyPause)
		if _, e := a.commit(nil, func(tx *store.Tx) error { return tx.SetDirectoryPause(until) }); e != nil {
			a.log.Error("storing the directory pause failed", "err", e)
		}
		a.log.Warn("the server denied the directory search; directory lookups are stopped for 24 hours", "until", until.UTC().Format(time.RFC3339))
		return
	}
	var se *upstream.StatusError
	if errors.As(err, &se) && se.Perm && !se.Global {
		return true
	}
	// Throttling, a time-out, a server error, the network: back off, doubling up to the maximum.
	a.dirFailures++
	d := cfg.BackoffBase
	for i := 1; i < a.dirFailures && d < cfg.BackoffMax; i++ {
		d *= 2
	}
	if d > cfg.BackoffMax {
		d = cfg.BackoffMax
	}
	var te *upstream.ThrottleError
	if errors.As(err, &te) && te.RetryAfter > d {
		d = te.RetryAfter // honour Retry-After
	}
	a.dirRetryAt = now.Add(d)
	a.log.Warn("a directory search failed; backing off", "for", d.String(), "failures_in_a_row", a.dirFailures, "err", err)
	return false
}
