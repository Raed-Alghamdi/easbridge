package engine

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"easbridge/internal/store"
	"easbridge/internal/upstream"
	"easbridge/internal/vcard"
)

// The full sweep of the Global Address List (docs/DESIGN-contacts.md section 9), opt in.
//
// A GAL can be searched but not listed, so the sweep walks prefixes: it finds the shortest
// query Exchange answers (calibration, with the account's own entry as the known positive),
// then searches every string of that length over the alphabet of an Exchange alias; a prefix
// that matches more than a search returns (more than 100) is divided into the prefix and each
// character of the alphabet, and so on until no prefix is saturated. Ambiguous name resolution
// matches the start of the display name, the first and the last name and the alias; every
// mail-enabled entry has an alias, so the walk reaches everyone, whatever script the name is
// in. It keeps the entries of the directory domains and nothing else.
//
// It uses the budget of the lookups (spacing, the day's cap, the breaker, Retry-After, the
// back-off, the pause after access denied) and only the part the lookups of addresses from
// mail leave: directoryLoop asks the lookups first.

// sweepStep does one unit of the sweep: it starts one, runs one calibration search or one
// prefix search. It reports whether it did something, and otherwise how long to wait.
func (a *Account) sweepStep() (did bool, wait time.Duration) {
	cfg := a.cfg.Directory
	now := a.cfg.Clock()
	sw, err := a.st.Sweep()
	if err != nil {
		a.log.Error("reading the sweep state failed", "err", err)
		return false, cfg.Poll
	}
	switch sw.Phase {
	case store.SweepIdle:
		if until := time.Unix(sw.NextStartAt, 0); sw.NextStartAt != 0 && now.Before(until) {
			w := until.Sub(now)
			if w > cfg.Poll {
				w = cfg.Poll
			}
			return false, w
		}
		if _, err := a.commit(nil, func(tx *store.Tx) error { return tx.SweepStart(now) }); err != nil {
			a.log.Error("starting the directory sweep failed", "err", err)
			return false, cfg.Poll
		}
		a.log.Info("directory sweep started", "sweep", sw.ID+1)
		return true, 0
	case store.SweepCalibrating:
		a.sweepCalibrate(sw)
		return true, 0
	default:
		a.sweepPrefix()
		return true, 0
	}
}

// ownLocalPart is the lower-case local part of the account's login.
func (a *Account) ownLocalPart() string {
	l, _, _ := strings.Cut(a.user, "@")
	return strings.ToLower(l)
}

// refusedQuery reports whether err is Exchange refusing the query itself (invalid, too
// complex, too short ...): a permanent Search status about this query alone.
func refusedQuery(err error) bool {
	var se *upstream.StatusError
	return errors.As(err, &se) && se.Op == "Search" && se.Perm && !se.Global
}

// sweepCalibrate searches the first L characters of the account's own local part for the
// next L. The first length whose search finds at least one entry is the shortest query the
// sweep may use; if none up to 4 does, there is no sweep this time.
func (a *Account) sweepCalibrate(sw store.Sweep) {
	cfg := a.cfg.Directory
	now := a.cfg.Clock()
	local := a.ownLocalPart()
	maxLen := store.MaxCalibrationLength
	if len(local) < maxLen {
		maxLen = len(local)
	}
	L := sw.CalibNext
	if L > maxLen {
		a.sweepGiveUp(now)
		return
	}
	res, err := a.search(local[:L])
	ok := false
	switch {
	case err == nil:
		ok = res.Total >= 1 || len(res.Entries) >= 1
	case a.directoryFailure(err):
		// The query itself was refused (too short, too complex): this length does not work.
	default:
		return // not the query's fault: nothing is recorded, the same length is tried again
	}
	var calibrated, failed bool
	if _, err := a.commit(nil, func(tx *store.Tx) error {
		var err error
		calibrated, failed, err = tx.SweepCalibrationResult(L, maxLen, ok, now, cfg.FullInterval)
		return err
	}); err != nil {
		a.log.Error("storing the sweep calibration failed", "err", err)
		return
	}
	switch {
	case calibrated && L >= store.InfeasibleCalibrationLength:
		a.log.Warn("directory sweep stopped for this run: the directory answers no query shorter than "+strconv.Itoa(L)+" characters, and walking every string of that length is not feasible; "+
			"the as-you-go lookups go on and the sweep tries again at its next time", "shortest_query_length", L, "retry_after", cfg.FullInterval.String())
	case calibrated:
		a.log.Info("directory sweep calibrated", "shortest_query_length", L)
	case failed:
		a.logCalibrationFailed()
	}
}

func (a *Account) sweepGiveUp(now time.Time) {
	if _, err := a.commit(nil, func(tx *store.Tx) error { return tx.SweepGiveUp(now, a.cfg.Directory.FullInterval) }); err != nil {
		a.log.Error("storing the sweep calibration failed", "err", err)
		return
	}
	a.logCalibrationFailed()
}

func (a *Account) logCalibrationFailed() {
	a.log.Warn("directory sweep not possible: the directory answers no query of up to 4 characters taken from this account's own address; "+
		"the as-you-go lookups go on and the sweep tries again at its next time", "retry_after", a.cfg.Directory.FullInterval.String())
}

// sweepPrefix searches the next prefix of the queue and stores what came back.
func (a *Account) sweepPrefix() {
	cfg := a.cfg.Directory
	now := a.cfg.Clock()
	var rowID int64
	var prefix string
	var have bool
	if _, err := a.commit(nil, func(tx *store.Tx) error {
		var err error
		rowID, prefix, have, err = tx.SweepNext()
		return err
	}); err != nil {
		a.log.Error("reading the sweep queue failed", "err", err)
		return
	}
	if !have {
		var out store.SweepStepOut
		if _, err := a.commit(nil, func(tx *store.Tx) error {
			var err error
			out, err = tx.SweepEndEmpty(now, cfg.FullInterval)
			return err
		}); err != nil {
			a.log.Error("ending the directory sweep failed", "err", err)
			return
		}
		a.logSweepEnd(out)
		return
	}
	in := store.SweepStepIn{RowID: rowID, MaxSearches: cfg.SweepMaxSearches, Now: now, Interval: cfg.FullInterval}
	res, err := a.search(prefix)
	switch {
	case err == nil:
		in.Entries = a.sweepKeep(res.Entries)
		saturated := res.Total > len(res.Entries) || (res.Total == 0 && len(res.Entries) >= 100)
		if saturated {
			a.sweepDivide(prefix, &in)
		}
	case a.directoryFailure(err):
		// Exchange refuses this query (too complex, too short): the prefix is divided.
		a.sweepDivide(prefix, &in)
	default:
		return // throttled, timed out, paused ...: the prefix stays at the head of the queue
	}
	var out store.SweepStepOut
	if _, err := a.commit(nil, func(tx *store.Tx) error {
		var err error
		out, err = tx.SweepApply(in)
		return err
	}); err != nil {
		a.log.Error("storing a sweep step failed", "err", err)
		return
	}
	if out.Finished {
		a.logSweepEnd(out)
	}
}

// sweepDivide queues the children of a prefix, or marks the sweep capped when the prefix is
// as long as a prefix may be.
func (a *Account) sweepDivide(prefix string, in *store.SweepStepIn) {
	if len(prefix) >= a.cfg.Directory.SweepMaxPrefix {
		in.Capped = true
		return
	}
	for _, c := range store.SweepAlphabet {
		in.Children = append(in.Children, prefix+string(c))
	}
}

// sweepKeep turns the entries of a search into cards: the ones in a directory domain, once
// each. Entries of other domains (mail contacts the GAL lists) are dropped, unread.
func (a *Account) sweepKeep(entries []upstream.DirectoryEntry) []store.SweepEntry {
	domains := map[string]bool{}
	for _, d := range a.cfg.Directory.Domains {
		domains[strings.ToLower(d)] = true
	}
	seen := map[string]bool{}
	var out []store.SweepEntry
	for _, e := range entries {
		addr := strings.ToLower(strings.TrimSpace(e.EmailAddress))
		i := strings.LastIndexByte(addr, '@')
		if i < 1 || !domains[addr[i+1:]] || seen[addr] {
			continue
		}
		seen[addr] = true
		out = append(out, store.SweepEntry{Email: addr, Card: vcard.FromDirectory(e).Marshal()})
	}
	return out
}

func (a *Account) logSweepEnd(out store.SweepStepOut) {
	sw, _ := a.st.Sweep()
	if out.Capped {
		a.log.Warn("directory sweep finished early: it reached a cap (5,000 searches or a prefix of 20 characters); nothing is removed after a capped sweep",
			"searches", sw.Searches, "entries_found", sw.Found)
		return
	}
	a.log.Info("directory sweep finished", "searches", sw.Searches, "entries_found", sw.Found, "cards_removed", out.Removed)
}

// sweepStatus describes the sweep for /healthz.
func (a *Account) sweepStatus() *SweepStatus {
	sw, err := a.st.Sweep()
	if err != nil {
		return &SweepStatus{Full: true, State: "unknown"}
	}
	now := a.cfg.Clock()
	st := &SweepStatus{Full: true, CalibratedLength: sw.CalibLen, CalibrationFailed: sw.CalibFailed,
		PrefixesDone: sw.PrefixesDone, PrefixesQueued: sw.Queued, SearchesThisSweep: sw.Searches, EntriesFoundThisSweep: sw.Found,
		LastSweepCapped: sw.LastCapped, LastSweepUnix: sw.FinishedAt}
	if sw.Phase == store.SweepIdle && sw.CalibLen >= store.InfeasibleCalibrationLength {
		st.SweepInfeasible, st.SweepInfeasibleLength = true, sw.CalibLen
	}
	switch {
	case sw.Phase != store.SweepIdle:
		st.State = "running"
	case sw.NextStartAt > now.Unix():
		st.State, st.WaitingUntilUnix = "waiting", sw.NextStartAt
	default:
		st.State = "idle"
	}
	return st
}
