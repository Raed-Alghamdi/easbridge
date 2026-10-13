package store

import (
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"

	"easbridge/internal/mimeutil"
	"easbridge/internal/vcard"
)

// The directory (docs/DESIGN-contacts.md section 4): cards for addresses that were
// found, one exact match at a time, in the Global Address List; and the candidates,
// the addresses in the directory domains that appear in stored mail.

// Candidate states.
const (
	CandNew      = 0 // not looked up yet
	CandFound    = 1 // a card exists
	CandNotFound = 2 // looked up, no exact match (the negative cache)
)

// Candidate is an address that may be looked up.
type Candidate struct {
	Email     string
	Name      string // the display name it was last seen with ("" if none)
	SeenCount int
	FirstSeen int64
	LastSeen  int64
	State     int
	CheckedAt int64
	NextCheck int64
	MissFirst int64
}

const candidateCols = `email, name, seen_count, first_seen, last_seen, state, checked_at, next_check, miss_first`

func scanCandidate(sc interface{ Scan(...any) error }) (*Candidate, error) {
	var c Candidate
	err := sc.Scan(&c.Email, &c.Name, &c.SeenCount, &c.FirstSeen, &c.LastSeen, &c.State, &c.CheckedAt, &c.NextCheck, &c.MissFirst)
	return &c, err
}

// candidateDomains is the set of domains whose addresses become candidates; nil means
// the directory is off and the store notes nothing.
type candidateDomains map[string]bool

// SetDirectoryDomains tells the store which domains' addresses to note as candidates
// when a message is stored. nil or empty switches noting off.
func (db *DB) SetDirectoryDomains(domains []string) {
	if len(domains) == 0 {
		db.candDomains.Store(nil)
		return
	}
	m := candidateDomains{}
	for _, d := range domains {
		m[strings.ToLower(strings.TrimSpace(d))] = true
	}
	db.candDomains.Store(&m)
}

// domainOf returns the part of an address after the last @, lower case.
func domainOf(email string) string {
	i := strings.LastIndexByte(email, '@')
	if i < 0 {
		return ""
	}
	return strings.ToLower(email[i+1:])
}

// SetCandidateNoting turns the noting of candidates at ingest on or off for one account
// (it is on unless turned off). The engine turns it off while it seeds the candidates
// from the stored mail, which counts every stored message once, and on when the
// seeding is done.
func (db *DB) SetCandidateNoting(accountID int64, on bool) {
	if on {
		db.noteOff.Delete(accountID)
	} else {
		db.noteOff.Store(accountID, true)
	}
}

// noteHeader notes the From, To and Cc addresses of a stored message that are in a
// directory domain. seen is the message's date.
func (t *Tx) noteHeader(header []byte, seen time.Time) error {
	doms := t.a.db.candDomains.Load()
	if doms == nil {
		return nil
	}
	if _, off := t.a.db.noteOff.Load(t.a.ID); off {
		return nil
	}
	return t.noteAddrs(*doms, mimeutil.HeaderAddresses(header), seen)
}

func (t *Tx) noteAddrs(doms candidateDomains, addrs []mimeutil.Addr, seen time.Time) error {
	done := map[string]bool{} // an address counts once per message
	for _, a := range addrs {
		if done[a.Address] || !doms[domainOf(a.Address)] {
			continue
		}
		done[a.Address] = true
		if err := t.NoteCandidate(a.Address, a.Name, seen); err != nil {
			return err
		}
	}
	return nil
}

// NoteCandidate records that email (lower case) appeared in a message dated seen, with
// the display name the header gave it ("" if none).
func (t *Tx) NoteCandidate(email, name string, seen time.Time) error {
	if len(name) > 200 {
		name = ""
	}
	ts := seen.Unix()
	_, err := t.tx.Exec(`INSERT INTO directory_candidates(account_id, email, name, seen_count, first_seen, last_seen) VALUES (?, ?, ?, 1, ?, ?)
		ON CONFLICT(account_id, email) DO UPDATE SET seen_count = seen_count + 1,
		  first_seen = MIN(first_seen, excluded.first_seen),
		  name = CASE WHEN excluded.name != '' AND (name = '' OR excluded.last_seen >= last_seen) THEN excluded.name ELSE name END,
		  last_seen = MAX(last_seen, excluded.last_seen)`, t.a.ID, email, name, ts, ts)
	return err
}

// ResetCandidateCounts sets every candidate's seen count to zero. The engine calls it
// before it seeds again (the domain list changed), so that the seeding counts each
// stored message once.
func (t *Tx) ResetCandidateCounts() error {
	_, err := t.tx.Exec(`UPDATE directory_candidates SET seen_count = 0 WHERE account_id = ?`, t.a.ID)
	return err
}

// SeedCandidates reads the headers of up to limit stored messages of the account whose
// primary key is above after, in key order, and notes their addresses in the given
// domains (the date of the message is the time they were seen). It returns the last
// key it read and whether there is nothing left. It runs in the caller's transaction;
// the engine calls it in small batches.
func (t *Tx) SeedCandidates(after int64, limit int, domains []string) (last int64, done bool, err error) {
	doms := candidateDomains{}
	for _, d := range domains {
		doms[strings.ToLower(strings.TrimSpace(d))] = true
	}
	rows, err := t.tx.Query(`SELECT m.id, m.internal_date, m.header FROM messages m JOIN folders f ON f.id = m.folder_id
		WHERE f.account_id = ? AND m.id > ? ORDER BY m.id LIMIT ?`, t.a.ID, after, limit)
	if err != nil {
		return after, false, err
	}
	type row struct {
		id, date int64
		header   []byte
	}
	var batch []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.date, &r.header); err != nil {
			rows.Close()
			return after, false, err
		}
		batch = append(batch, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return after, false, err
	}
	last = after
	for _, r := range batch {
		last = r.id
		if err := t.noteAddrs(doms, mimeutil.HeaderAddresses(r.header), time.Unix(r.date, 0)); err != nil {
			return after, false, err
		}
	}
	return last, len(batch) < limit, nil
}

// domainClause builds `substr(email, instr(email, '@') + 1) IN (?, ...)`. The part
// after the first @ is the domain: addresses have exactly one.
func domainClause(domains []string) (string, []any) {
	if len(domains) == 0 {
		return "0", nil
	}
	ph := make([]string, len(domains))
	args := make([]any, len(domains))
	for i, d := range domains {
		ph[i], args[i] = "?", strings.ToLower(d)
	}
	return `substr(email, instr(email, '@') + 1) IN (` + strings.Join(ph, ",") + `)`, args
}

// NextCandidate returns the candidate to look up next: one in a given domain, other than
// exclude, that is due (never looked up, or its next_check has passed), the one seen
// most recently first and, among those, the one seen most often. nil when none is due.
func (a *Account) NextCandidate(now time.Time, domains []string, exclude string) (*Candidate, error) {
	dc, args := domainClause(domains)
	q := `SELECT ` + candidateCols + ` FROM directory_candidates WHERE account_id = ? AND next_check <= ? AND email != ? AND ` + dc +
		` ORDER BY last_seen DESC, seen_count DESC, email LIMIT 1`
	c, err := scanCandidate(a.db.r.QueryRow(q, append([]any{a.ID, now.Unix(), strings.ToLower(exclude)}, args...)...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

// CandidateQueue counts the candidates that are due.
func (a *Account) CandidateQueue(now time.Time, domains []string, exclude string) (int, error) {
	dc, args := domainClause(domains)
	var n int
	err := a.db.r.QueryRow(`SELECT COUNT(*) FROM directory_candidates WHERE account_id = ? AND next_check <= ? AND email != ? AND `+dc,
		append([]any{a.ID, now.Unix(), strings.ToLower(exclude)}, args...)...).Scan(&n)
	return n, err
}

// Candidate returns one candidate.
func (a *Account) Candidate(email string) (*Candidate, error) {
	c, err := scanCandidate(a.db.r.QueryRow(`SELECT `+candidateCols+` FROM directory_candidates WHERE account_id = ? AND email = ?`, a.ID, strings.ToLower(email)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// Candidate is the transactional variant.
func (t *Tx) Candidate(email string) (*Candidate, error) {
	c, err := scanCandidate(t.tx.QueryRow(`SELECT `+candidateCols+` FROM directory_candidates WHERE account_id = ? AND email = ?`, t.a.ID, strings.ToLower(email)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// Candidates lists every candidate, most recently seen first.
func (a *Account) Candidates() ([]Candidate, error) {
	rows, err := a.db.r.Query(`SELECT `+candidateCols+` FROM directory_candidates WHERE account_id = ? ORDER BY last_seen DESC, seen_count DESC, email`, a.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		c, err := scanCandidate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// CandidateUpdate is the outcome of looking a candidate up.
type CandidateUpdate struct {
	State     int
	CheckedAt time.Time
	NextCheck time.Time
	MissFirst time.Time // zero: no miss recorded
}

// UpdateCandidate records where a lookup stands.
func (t *Tx) UpdateCandidate(email string, u CandidateUpdate) error {
	var miss int64
	if !u.MissFirst.IsZero() {
		miss = u.MissFirst.Unix()
	}
	_, err := t.tx.Exec(`UPDATE directory_candidates SET state = ?, checked_at = ?, next_check = ?, miss_first = ? WHERE account_id = ? AND email = ?`,
		u.State, u.CheckedAt.Unix(), u.NextCheck.Unix(), miss, t.a.ID, strings.ToLower(email))
	return err
}

// Directory cards -----------------------------------------------------------

// DirectoryCards lists the directory cards.
func (a *Account) DirectoryCards() ([]Card, error) {
	rows, err := a.db.r.Query(`SELECT uid, etag, vcard FROM directory_cards WHERE account_id = ? ORDER BY uid`, a.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Card
	for rows.Next() {
		var c Card
		if err := rows.Scan(&c.UID, &c.ETag, &c.VCard); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DirectoryCard returns one directory card by UID.
func (a *Account) DirectoryCard(uid string) (*Card, error) {
	var c Card
	err := a.db.r.QueryRow(`SELECT uid, etag, vcard FROM directory_cards WHERE account_id = ? AND uid = ?`, a.ID, uid).Scan(&c.UID, &c.ETag, &c.VCard)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// DirectoryCardByEmail returns the card kept for an address (tests, probe-free).
func (a *Account) DirectoryCardByEmail(email string) (*Card, error) {
	var c Card
	err := a.db.r.QueryRow(`SELECT uid, etag, vcard FROM directory_cards WHERE account_id = ? AND email = ?`, a.ID, strings.ToLower(email)).Scan(&c.UID, &c.ETag, &c.VCard)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// DirectoryCardCount is the number of directory cards.
func (a *Account) DirectoryCardCount() (int, error) {
	var n int
	err := a.db.r.QueryRow(`SELECT COUNT(*) FROM directory_cards WHERE account_id = ?`, a.ID).Scan(&n)
	return n, err
}

// DirectoryState is the per account bookkeeping of the directory.
type DirectoryState struct {
	CTag         string
	SeededFor    string
	SeedAfter    int64
	SeedDone     bool
	PausedUntil  int64
	LookupsDay   string
	LookupsToday int
	LastSearchMs int64
}

// LookupsOn is the number of searches sent on the UTC date day.
func (s DirectoryState) LookupsOn(day string) int {
	if s.LookupsDay != day {
		return 0
	}
	return s.LookupsToday
}

// DirectoryState returns the bookkeeping (zero values before anything was stored).
func (a *Account) DirectoryState() (DirectoryState, error) {
	var s DirectoryState
	var created, seq, done int64
	err := a.db.r.QueryRow(`SELECT created_ns, ctag_seq, seeded_for, seed_after, seed_done, paused_until, lookups_day, lookups_today, last_search_ms
		FROM directory_state WHERE account_id = ?`, a.ID).Scan(&created, &seq, &s.SeededFor, &s.SeedAfter, &done, &s.PausedUntil, &s.LookupsDay, &s.LookupsToday, &s.LastSearchMs)
	if errors.Is(err, sql.ErrNoRows) {
		return DirectoryState{CTag: ctagOf(0, 0)}, nil
	}
	if err != nil {
		return s, err
	}
	s.SeedDone = done != 0
	s.CTag = ctagOf(created, seq)
	return s, nil
}

func (t *Tx) ensureDirectoryState() error {
	_, err := t.tx.Exec(`INSERT OR IGNORE INTO directory_state(account_id, created_ns) VALUES (?, ?)`, t.a.ID, t.now.UnixNano())
	return err
}

func (t *Tx) bumpDirectoryCTag() error {
	if err := t.ensureDirectoryState(); err != nil {
		return err
	}
	_, err := t.tx.Exec(`UPDATE directory_state SET ctag_seq = ctag_seq + 1 WHERE account_id = ?`, t.a.ID)
	return err
}

// PutDirectoryCard stores or replaces the card kept for an address and reports whether
// the directory book changed (a new card, or other bytes than before).
func (t *Tx) PutDirectoryCard(email string, card []byte) (bool, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || len(card) == 0 {
		return false, errors.New("store: a directory card needs an address and bytes")
	}
	etag := vcard.ETag(card)
	var old string
	err := t.tx.QueryRow(`SELECT etag FROM directory_cards WHERE account_id = ? AND email = ?`, t.a.ID, email).Scan(&old)
	if err == nil && old == etag {
		// Found by a lookup of the exact address as well: it stays on a sweep's removal list no longer.
		_, err = t.tx.Exec(`UPDATE directory_cards SET by_lookup = 1, confirmed_at = ? WHERE account_id = ? AND email = ?`, t.now.Unix(), t.a.ID, email)
		return false, err
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if _, err := t.tx.Exec(`INSERT INTO directory_cards(account_id, email, uid, etag, vcard, updated_at, by_lookup, confirmed_at) VALUES (?, ?, ?, ?, ?, ?, 1, ?)
		ON CONFLICT(account_id, email) DO UPDATE SET uid = excluded.uid, etag = excluded.etag, vcard = excluded.vcard, updated_at = excluded.updated_at,
		  by_lookup = 1, confirmed_at = excluded.confirmed_at`,
		t.a.ID, email, vcard.DirectoryUID(email), etag, card, t.now.Unix(), t.now.Unix()); err != nil {
		return false, err
	}
	return true, t.bumpDirectoryCTag()
}

// TouchDirectoryCard notes that the card of an address was looked up again and is
// unchanged.
func (t *Tx) TouchDirectoryCard(email string) error {
	_, err := t.tx.Exec(`UPDATE directory_cards SET updated_at = ?, by_lookup = 1, confirmed_at = ? WHERE account_id = ? AND email = ?`, t.now.Unix(), t.now.Unix(), t.a.ID, strings.ToLower(email))
	return err
}

// DeleteDirectoryCard removes the card kept for an address and reports whether there was one.
func (t *Tx) DeleteDirectoryCard(email string) (bool, error) {
	// A card that a sweep has seen lately is not removed because lookups no longer find it
	// (they find it by its address; the sweep found it by its alias): it stays as a sweep
	// card, and the sweep decides.
	var bySweep int
	var seen, completed int64
	err := t.tx.QueryRow(`SELECT c.by_sweep, c.sweep_seen, COALESCE((SELECT completed_id FROM directory_sweep WHERE account_id = c.account_id), 0)
		FROM directory_cards c WHERE c.account_id = ? AND c.email = ?`, t.a.ID, strings.ToLower(strings.TrimSpace(email))).Scan(&bySweep, &seen, &completed)
	if err == nil && bySweep == 1 && seen >= completed {
		_, err := t.tx.Exec(`UPDATE directory_cards SET by_lookup = 0 WHERE account_id = ? AND email = ?`, t.a.ID, strings.ToLower(strings.TrimSpace(email)))
		return false, err
	}
	res, err := t.tx.Exec(`DELETE FROM directory_cards WHERE account_id = ? AND email = ?`, t.a.ID, strings.ToLower(strings.TrimSpace(email)))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}
	return true, t.bumpDirectoryCTag()
}

// SetSeedProgress records how far the seeding of the candidates has come, and the
// domain list it runs for.
func (t *Tx) SetSeedProgress(domainsKey string, after int64, done bool) error {
	if err := t.ensureDirectoryState(); err != nil {
		return err
	}
	_, err := t.tx.Exec(`UPDATE directory_state SET seeded_for = ?, seed_after = ?, seed_done = ? WHERE account_id = ?`, domainsKey, after, boolInt(done), t.a.ID)
	return err
}

// SetDirectoryPause stops the directory lookups until the given time (zero: no pause).
func (t *Tx) SetDirectoryPause(until time.Time) error {
	if err := t.ensureDirectoryState(); err != nil {
		return err
	}
	var u int64
	if !until.IsZero() {
		u = until.Unix()
	}
	_, err := t.tx.Exec(`UPDATE directory_state SET paused_until = ? WHERE account_id = ?`, u, t.a.ID)
	return err
}

// RecordSearch counts one GAL search sent at now: the time of the last search (kept
// across restarts so the spacing holds) and the count of the UTC day.
func (t *Tx) RecordSearch(now time.Time) error {
	if err := t.ensureDirectoryState(); err != nil {
		return err
	}
	day := now.UTC().Format("2006-01-02")
	_, err := t.tx.Exec(`UPDATE directory_state SET last_search_ms = ?,
		lookups_today = CASE WHEN lookups_day = ? THEN lookups_today + 1 ELSE 1 END, lookups_day = ? WHERE account_id = ?`,
		now.UnixMilli(), day, day, t.a.ID)
	return err
}

// DomainsKey is the canonical text of a domain list, to tell whether the seeding ran
// for the current one.
func DomainsKey(domains []string) string {
	d := append([]string(nil), domains...)
	for i := range d {
		d[i] = strings.ToLower(strings.TrimSpace(d[i]))
	}
	sort.Strings(d)
	return strings.Join(d, ",")
}
