package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"easbridge/internal/vcard"
)

// The full sweep of the Global Address List (docs/DESIGN-contacts.md section 9): the state
// of the walk over the prefixes, kept in the database so that a restart resumes it, and the
// rules that decide which cards a finished sweep removes.

// SweepAlphabet is the alphabet of the prefix walk: the characters of an Exchange alias.
const SweepAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789._-"

// Sweep phases.
const (
	SweepIdle        = 0
	SweepCalibrating = 1
	SweepSweeping    = 2
)

// MaxCalibrationLength is the longest query tried while calibrating.
const MaxCalibrationLength = 4

// InfeasibleCalibrationLength is the shortest calibrated query length at which the sweep stops
// itself for the run (0.3): with 39 characters in the alphabet, every string of 3 characters is
// 59,319 searches before any prefix is divided, which at the rate the directory is limited to
// is weeks and would look like what it is.
const InfeasibleCalibrationLength = 3

// Sweep is the state of the sweep of an account.
type Sweep struct {
	ID                 int64
	Phase              int
	CalibNext          int
	CalibLen           int
	CalibFailed        bool
	StartedAt          int64
	NextStartAt        int64
	Searches           int
	Found              int
	PrefixesDone       int
	Capped             bool
	FinishedAt         int64
	LastCapped         bool
	CompletedID        int64
	CompletedStartedAt int64
	// Queued is the number of prefixes still to be searched, those written and those the
	// generator has yet to write.
	Queued int
}

const sweepCols = `sweep_id, phase, calib_next, calib_len, calib_failed, started_at, next_start_at, searches, found, prefixes_done,
  capped, gen_pos, gen_total, finished_at, last_capped, completed_id, completed_started_at`

func scanSweep(sc interface{ Scan(...any) error }) (Sweep, int64, int64, error) {
	var s Sweep
	var failed, capped, last, genPos, genTotal int64
	err := sc.Scan(&s.ID, &s.Phase, &s.CalibNext, &s.CalibLen, &failed, &s.StartedAt, &s.NextStartAt, &s.Searches, &s.Found, &s.PrefixesDone,
		&capped, &genPos, &genTotal, &s.FinishedAt, &last, &s.CompletedID, &s.CompletedStartedAt)
	s.CalibFailed, s.Capped, s.LastCapped = failed != 0, capped != 0, last != 0
	return s, genPos, genTotal, err
}

// Sweep returns the state of the account's sweep (zero values before it ever ran).
func (a *Account) Sweep() (Sweep, error) {
	s, pos, total, err := scanSweep(a.db.r.QueryRow(`SELECT `+sweepCols+` FROM directory_sweep WHERE account_id = ?`, a.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return Sweep{CalibNext: 1}, nil
	}
	if err != nil {
		return s, err
	}
	var n int
	if err := a.db.r.QueryRow(`SELECT COUNT(*) FROM directory_sweep_queue WHERE account_id = ?`, a.ID).Scan(&n); err != nil {
		return s, err
	}
	s.Queued = n + int(total-pos)
	return s, nil
}

// Sweep is the transactional variant (without the queue length).
func (t *Tx) Sweep() (Sweep, int64, int64, error) {
	s, pos, total, err := scanSweep(t.tx.QueryRow(`SELECT `+sweepCols+` FROM directory_sweep WHERE account_id = ?`, t.a.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return Sweep{CalibNext: 1}, 0, 0, nil
	}
	return s, pos, total, err
}

func (t *Tx) ensureSweep() error {
	_, err := t.tx.Exec(`INSERT OR IGNORE INTO directory_sweep(account_id) VALUES (?)`, t.a.ID)
	return err
}

// SweepStart begins a new sweep: a new id, the counters at zero, the queue empty, and the
// calibration of the shortest query length to do first.
func (t *Tx) SweepStart(now time.Time) error {
	if err := t.ensureSweep(); err != nil {
		return err
	}
	if _, err := t.tx.Exec(`DELETE FROM directory_sweep_queue WHERE account_id = ?`, t.a.ID); err != nil {
		return err
	}
	_, err := t.tx.Exec(`UPDATE directory_sweep SET sweep_id = sweep_id + 1, phase = ?, calib_next = 1, calib_len = 0, calib_failed = 0,
		started_at = ?, searches = 0, found = 0, prefixes_done = 0, capped = 0, gen_pos = 0, gen_total = 0 WHERE account_id = ?`,
		SweepCalibrating, now.Unix(), t.a.ID)
	return err
}

// SweepCalibrationResult records the outcome of one calibration search with a query of
// length length: ok means the search was answered with at least one match. The first length
// that is answered ends the calibration and the sweep begins with every string of that
// length; when no length up to MaxCalibrationLength (or up to maxLen, the length of the
// string the queries are cut from) is answered, the sweep is given up until the next time.
func (t *Tx) SweepCalibrationResult(length, maxLen int, ok bool, now time.Time, interval time.Duration) (calibrated, failed bool, err error) {
	if _, err := t.tx.Exec(`UPDATE directory_sweep SET searches = searches + 1 WHERE account_id = ?`, t.a.ID); err != nil {
		return false, false, err
	}
	if ok && length >= InfeasibleCalibrationLength {
		// The directory answers no query shorter than this: walking every string of this length
		// is not feasible. The sweep ends here for this run, as a finished one that completed
		// nothing (so that it removes nothing), and tries again at its next time.
		_, err := t.tx.Exec(`UPDATE directory_sweep SET calib_len = ?, phase = ?, gen_pos = 0, gen_total = 0, next_start_at = ?, finished_at = ?, last_capped = 0 WHERE account_id = ?`,
			length, SweepIdle, now.Add(interval).Unix(), now.Unix(), t.a.ID)
		return true, false, err
	}
	if ok {
		total := int64(1)
		for i := 0; i < length; i++ {
			total *= int64(len(SweepAlphabet))
		}
		_, err := t.tx.Exec(`UPDATE directory_sweep SET calib_len = ?, phase = ?, gen_pos = 0, gen_total = ? WHERE account_id = ?`,
			length, SweepSweeping, total, t.a.ID)
		return true, false, err
	}
	next := length + 1
	if next > MaxCalibrationLength || next > maxLen {
		_, err := t.tx.Exec(`UPDATE directory_sweep SET calib_next = ?, calib_failed = 1, phase = ?, next_start_at = ?, finished_at = ?, last_capped = 0 WHERE account_id = ?`,
			next, SweepIdle, now.Add(interval).Unix(), now.Unix(), t.a.ID)
		return false, true, err
	}
	_, err = t.tx.Exec(`UPDATE directory_sweep SET calib_next = ? WHERE account_id = ?`, next, t.a.ID)
	return false, false, err
}

// genBatch is how many prefixes of the starting level are written at a time.
const genBatch = 50

// SweepNext returns the next prefix to search without removing it (its row stays until
// SweepApply). When the queue is empty and the starting level has strings left, the next
// batch of them is written first. ok is false when nothing is left.
func (t *Tx) SweepNext() (rowID int64, prefix string, ok bool, err error) {
	err = t.tx.QueryRow(`SELECT id, prefix FROM directory_sweep_queue WHERE account_id = ? ORDER BY id LIMIT 1`, t.a.ID).Scan(&rowID, &prefix)
	if err == nil {
		return rowID, prefix, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, "", false, err
	}
	s, pos, total, err := t.Sweep()
	if err != nil || s.Phase != SweepSweeping || pos >= total {
		return 0, "", false, err
	}
	end := pos + genBatch
	if end > total {
		end = total
	}
	for i := pos; i < end; i++ {
		if _, err := t.tx.Exec(`INSERT INTO directory_sweep_queue(account_id, prefix) VALUES (?, ?)`, t.a.ID, genPrefix(i, s.CalibLen)); err != nil {
			return 0, "", false, err
		}
	}
	if _, err := t.tx.Exec(`UPDATE directory_sweep SET gen_pos = ? WHERE account_id = ?`, end, t.a.ID); err != nil {
		return 0, "", false, err
	}
	return t.SweepNext()
}

// genPrefix is the i-th string of the given length over the alphabet, in alphabet order.
func genPrefix(i int64, length int) string {
	b := make([]byte, length)
	n := int64(len(SweepAlphabet))
	for k := length - 1; k >= 0; k-- {
		b[k] = SweepAlphabet[i%n]
		i /= n
	}
	return string(b)
}

// SweepEntry is an entry a search returned that the sweep keeps.
type SweepEntry struct {
	Email string // lower case
	Card  []byte
}

// SweepStepIn is the outcome of the search for one prefix.
type SweepStepIn struct {
	RowID    int64
	Entries  []SweepEntry
	Children []string // the prefixes to search next (the prefix was saturated or refused)
	// Capped: the prefix was saturated but is as long as a prefix may be.
	Capped      bool
	MaxSearches int
	Now         time.Time
	Interval    time.Duration
}

// SweepStepOut says what a step did.
type SweepStepOut struct {
	Finished bool
	Capped   bool // the sweep that finished was capped
	Removed  int  // cards a finished, uncapped sweep removed
	// Changed: the directory book is not the same as before.
	Changed bool
}

// SweepApply stores the outcome of one search: the entries kept, the children queued, the
// counters, and, when nothing is left (or the search cap is reached), the end of the sweep
// with the removal of the cards that two sweeps in a row did not see.
func (t *Tx) SweepApply(in SweepStepIn) (SweepStepOut, error) {
	var out SweepStepOut
	s, pos, total, err := t.Sweep()
	if err != nil {
		return out, err
	}
	if s.Phase != SweepSweeping {
		return out, errors.New("store: no sweep is running")
	}
	if _, err := t.tx.Exec(`DELETE FROM directory_sweep_queue WHERE id = ? AND account_id = ?`, in.RowID, t.a.ID); err != nil {
		return out, err
	}
	newly := 0
	for _, e := range in.Entries {
		changed, seen, err := t.putSweepCard(e.Email, e.Card, s.ID)
		if err != nil {
			return out, err
		}
		out.Changed = out.Changed || changed
		if seen {
			newly++
		}
	}
	for _, c := range in.Children {
		if _, err := t.tx.Exec(`INSERT INTO directory_sweep_queue(account_id, prefix) VALUES (?, ?)`, t.a.ID, c); err != nil {
			return out, err
		}
	}
	searches := s.Searches + 1
	capped := s.Capped || in.Capped
	var left int
	if err := t.tx.QueryRow(`SELECT COUNT(*) FROM directory_sweep_queue WHERE account_id = ?`, t.a.ID).Scan(&left); err != nil {
		return out, err
	}
	remaining := left + int(total-pos)
	if in.MaxSearches > 0 && searches >= in.MaxSearches && remaining > 0 {
		capped = true
		if _, err := t.tx.Exec(`DELETE FROM directory_sweep_queue WHERE account_id = ?`, t.a.ID); err != nil {
			return out, err
		}
		pos, remaining = total, 0
	}
	if _, err := t.tx.Exec(`UPDATE directory_sweep SET searches = ?, prefixes_done = prefixes_done + 1, found = found + ?, capped = ?, gen_pos = ? WHERE account_id = ?`,
		searches, newly, boolInt(capped), pos, t.a.ID); err != nil {
		return out, err
	}
	if remaining > 0 {
		return out, nil
	}
	removed, err := t.sweepFinish(s, capped, in.Now, in.Interval)
	if err != nil {
		return out, err
	}
	out.Finished, out.Capped, out.Removed = true, capped, removed
	out.Changed = out.Changed || removed > 0
	return out, nil
}

// sweepFinish ends the running sweep. An uncapped sweep removes the cards that only
// sweeps found and that neither this sweep nor the previous uncapped one saw, unless a
// lookup of the exact address confirmed them since that previous sweep started. A capped
// sweep removes nothing. In both cases the next sweep is scheduled interval after now.
func (t *Tx) sweepFinish(s Sweep, capped bool, now time.Time, interval time.Duration) (int, error) {
	removed := 0
	if !capped && s.CompletedID > 0 {
		res, err := t.tx.Exec(`DELETE FROM directory_cards WHERE account_id = ? AND by_sweep = 1 AND sweep_seen < ? AND confirmed_at < ?`,
			t.a.ID, s.CompletedID, s.CompletedStartedAt)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		removed = int(n)
		if removed > 0 {
			if err := t.bumpDirectoryCTag(); err != nil {
				return 0, err
			}
		}
	}
	if !capped {
		if _, err := t.tx.Exec(`UPDATE directory_sweep SET completed_id = sweep_id, completed_started_at = started_at WHERE account_id = ?`, t.a.ID); err != nil {
			return 0, err
		}
	}
	_, err := t.tx.Exec(`UPDATE directory_sweep SET phase = ?, finished_at = ?, last_capped = ?, next_start_at = ? WHERE account_id = ?`,
		SweepIdle, now.Unix(), boolInt(capped), now.Add(interval).Unix(), t.a.ID)
	return removed, err
}

// putSweepCard stores or replaces the card of an address found by sweep sweepID, and says
// whether the book changed and whether this sweep saw the address for the first time.
func (t *Tx) putSweepCard(email string, card []byte, sweepID int64) (changed, firstSeen bool, err error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || len(card) == 0 {
		return false, false, errors.New("store: a directory card needs an address and bytes")
	}
	etag := vcard.ETag(card)
	var old string
	var seen int64
	err = t.tx.QueryRow(`SELECT etag, sweep_seen FROM directory_cards WHERE account_id = ? AND email = ?`, t.a.ID, email).Scan(&old, &seen)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := t.tx.Exec(`INSERT INTO directory_cards(account_id, email, uid, etag, vcard, updated_at, by_lookup, by_sweep, sweep_seen) VALUES (?, ?, ?, ?, ?, ?, 0, 1, ?)`,
			t.a.ID, email, vcard.DirectoryUID(email), etag, card, t.now.Unix(), sweepID); err != nil {
			return false, false, err
		}
		return true, true, t.bumpDirectoryCTag()
	case err != nil:
		return false, false, err
	}
	if old == etag {
		_, err = t.tx.Exec(`UPDATE directory_cards SET by_sweep = 1, sweep_seen = ? WHERE account_id = ? AND email = ?`, sweepID, t.a.ID, email)
		return false, seen != sweepID, err
	}
	if _, err := t.tx.Exec(`UPDATE directory_cards SET etag = ?, vcard = ?, updated_at = ?, by_sweep = 1, sweep_seen = ? WHERE account_id = ? AND email = ?`,
		etag, card, t.now.Unix(), sweepID, t.a.ID, email); err != nil {
		return false, false, err
	}
	return true, seen != sweepID, t.bumpDirectoryCTag()
}

// SweepGiveUp ends a calibration that cannot be carried out (the account's own local part is
// too short for the next length): no sweep until the next time.
func (t *Tx) SweepGiveUp(now time.Time, interval time.Duration) error {
	if err := t.ensureSweep(); err != nil {
		return err
	}
	_, err := t.tx.Exec(`UPDATE directory_sweep SET calib_failed = 1, phase = ?, next_start_at = ?, finished_at = ?, last_capped = 0 WHERE account_id = ?`,
		SweepIdle, now.Add(interval).Unix(), now.Unix(), t.a.ID)
	return err
}

// SweepEndEmpty finishes a sweep whose queue turned out to be empty (an interrupted state).
func (t *Tx) SweepEndEmpty(now time.Time, interval time.Duration) (SweepStepOut, error) {
	s, _, _, err := t.Sweep()
	if err != nil {
		return SweepStepOut{}, err
	}
	removed, err := t.sweepFinish(s, s.Capped, now, interval)
	return SweepStepOut{Finished: true, Capped: s.Capped, Removed: removed, Changed: removed > 0}, err
}

// SweepCardCount is the number of cards the sweep found.
func (a *Account) SweepCardCount() (int, error) {
	var n int
	err := a.db.r.QueryRow(`SELECT COUNT(*) FROM directory_cards WHERE account_id = ? AND by_sweep = 1`, a.ID).Scan(&n)
	return n, err
}

// DropSweep removes the state of every account's sweep and the cards only the sweep found;
// cards that a lookup of the exact address found as well stay, as lookup cards. The service
// calls it at start when the sweep is switched off. It returns the number of cards removed.
func (db *DB) DropSweep() (int, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.w.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT DISTINCT account_id FROM directory_cards WHERE by_sweep = 1 AND by_lookup = 0`)
	if err != nil {
		return 0, err
	}
	var accts []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		accts = append(accts, id)
	}
	rows.Close()
	res, err := tx.Exec(`DELETE FROM directory_cards WHERE by_sweep = 1 AND by_lookup = 0`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	for _, id := range accts {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO directory_state(account_id, created_ns) VALUES (?, ?)`, id, db.now().UnixNano()); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`UPDATE directory_state SET ctag_seq = ctag_seq + 1 WHERE account_id = ?`, id); err != nil {
			return 0, err
		}
	}
	for _, q := range []string{`UPDATE directory_cards SET by_sweep = 0, sweep_seen = 0`, `DELETE FROM directory_sweep_queue`, `DELETE FROM directory_sweep`} {
		if _, err := tx.Exec(q); err != nil {
			return 0, err
		}
	}
	return int(n), tx.Commit()
}

// DirectoryCardFlags tells how a directory card was found: by a lookup of its exact address, by
// the sweep (and the last sweep that saw it), and when a lookup last confirmed it.
func (a *Account) DirectoryCardFlags(email string) (byLookup, bySweep bool, sweepSeen, confirmedAt int64, err error) {
	var l, w int
	err = a.db.r.QueryRow(`SELECT by_lookup, by_sweep, sweep_seen, confirmed_at FROM directory_cards WHERE account_id = ? AND email = ?`,
		a.ID, strings.ToLower(email)).Scan(&l, &w, &sweepSeen, &confirmedAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return l != 0, w != 0, sweepSeen, confirmedAt, err
}
