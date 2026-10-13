package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"easbridge/internal/vcard"
)

func TestMigration7AddsTheSweepToASchema6DatabaseAndKeepsItsDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open(driverName, "file:"+filepath.Join(dir, DBFileName))
	if err != nil {
		t.Fatal(err)
	}
	// A database as 0.2.0 left it: schema 6 with a directory card, a candidate and bookkeeping.
	for i := 0; i < 6; i++ {
		if _, err := raw.Exec(migrations[i]); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`PRAGMA user_version = 6`,
		`INSERT INTO accounts(id, user, created_at) VALUES (1, 'a@corp.example', 0)`,
		`INSERT INTO directory_cards(account_id, email, uid, etag, vcard, updated_at) VALUES (1, 'pat@corp.example', 'dir-x', '"e"', X'4243', 5)`,
		`INSERT INTO directory_candidates(account_id, email, first_seen, last_seen, state) VALUES (1, 'pat@corp.example', 1, 2, 1)`,
		`INSERT INTO directory_state(account_id, created_ns, ctag_seq, lookups_today) VALUES (1, 77, 4, 9)`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	raw.Close()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if v, _ := db.SchemaVersion(); v != 8 {
		t.Fatalf("schema %d", v)
	}
	acct, _ := db.Account("a@corp.example")
	c, err := acct.DirectoryCardByEmail("pat@corp.example")
	if err != nil || string(c.VCard) != "BC" {
		t.Fatalf("the card: %+v %v", c, err)
	}
	by, sweep, seen, conf, err := acct.DirectoryCardFlags("pat@corp.example")
	if err != nil || !by || sweep || seen != 0 || conf != 0 {
		t.Errorf("an existing card was found by a lookup and by no sweep: %v %v %d %d %v", by, sweep, seen, conf, err)
	}
	if st, _ := acct.DirectoryState(); st.LookupsToday != 9 || st.CTag != ctagOf(77, 4) {
		t.Errorf("state %+v", st)
	}
	if cand, err := acct.Candidate("pat@corp.example"); err != nil || cand.State != CandFound {
		t.Errorf("candidate %+v %v", cand, err)
	}
	if sw, err := acct.Sweep(); err != nil || sw.ID != 0 || sw.Phase != SweepIdle || sw.Queued != 0 {
		t.Errorf("a fresh sweep state: %+v %v", sw, err)
	}
	if _, err := acct.Update(nil, func(tx *Tx) error { return tx.SweepStart(time.Now()) }); err != nil {
		t.Errorf("the new tables cannot be written: %v", err)
	}
}

func sweepAcct(t *testing.T) (*DB, *Account) {
	db, a := openTemp(t)
	return db, a
}

func dcard(email string) []byte {
	return vcard.Card{UID: vcard.DirectoryUID(email), Name: email}.Marshal()
}

func startSweep(t *testing.T, a *Account, now time.Time, calibrated int) {
	t.Helper()
	if _, err := a.Update(nil, func(tx *Tx) error {
		if err := tx.SweepStart(now); err != nil {
			return err
		}
		_, _, err := tx.SweepCalibrationResult(calibrated, 4, true, now, time.Hour)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// runSweep applies steps until the sweep finishes, with entries[prefix] returned for each prefix.
func runSweep(t *testing.T, a *Account, now time.Time, max int, seen func(prefix string) []string, divide func(prefix string) bool) SweepStepOut {
	t.Helper()
	for i := 0; i < 100000; i++ {
		var out SweepStepOut
		var done bool
		if _, err := a.Update(nil, func(tx *Tx) error {
			id, prefix, ok, err := tx.SweepNext()
			if err != nil || !ok {
				done = !ok
				return err
			}
			in := SweepStepIn{RowID: id, MaxSearches: max, Now: now, Interval: 24 * time.Hour}
			for _, e := range seen(prefix) {
				in.Entries = append(in.Entries, SweepEntry{Email: e, Card: dcard(e)})
			}
			if divide != nil && divide(prefix) {
				for _, c := range SweepAlphabet {
					in.Children = append(in.Children, prefix+string(c))
				}
			}
			out, err = tx.SweepApply(in)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if done {
			t.Fatal("the queue ran dry before the sweep finished")
		}
		if out.Finished {
			return out
		}
	}
	t.Fatal("the sweep does not end")
	return SweepStepOut{}
}

func TestGeneratedPrefixesAreEveryStringOfTheLengthInOrder(t *testing.T) {
	if len(SweepAlphabet) != 39 {
		t.Fatal(len(SweepAlphabet))
	}
	if genPrefix(0, 1) != "a" || genPrefix(38, 1) != "-" || genPrefix(0, 3) != "aaa" || genPrefix(1, 3) != "aab" || genPrefix(39, 2) != "ba" || genPrefix(39*39-1, 2) != "--" {
		t.Errorf("%q %q %q %q %q %q", genPrefix(0, 1), genPrefix(38, 1), genPrefix(0, 3), genPrefix(1, 3), genPrefix(39, 2), genPrefix(39*39-1, 2))
	}
	seen := map[string]bool{}
	for i := int64(0); i < 39*39; i++ {
		p := genPrefix(i, 2)
		if len(p) != 2 || seen[p] {
			t.Fatalf("%q twice or wrong", p)
		}
		seen[p] = true
	}
}

func TestSweepQueueIsWrittenInBatchesAndWalkedInOrder(t *testing.T) {
	_, a := sweepAcct(t)
	now := time.Now()
	startSweep(t, a, now, 2)
	sw, _ := a.Sweep()
	if sw.Phase != SweepSweeping || sw.CalibLen != 2 || sw.Queued != 39*39 {
		t.Fatalf("%+v", sw)
	}
	if n := countRows(t, a, `SELECT COUNT(*) FROM directory_sweep_queue`); n != 0 {
		t.Errorf("%d rows written at once", n)
	}
	var got []string
	out := runSweep(t, a, now, 100000, func(p string) []string { got = append(got, p); return nil }, nil)
	if !out.Finished || out.Capped || len(got) != 39*39 || got[0] != "aa" || got[1] != "ab" || got[len(got)-1] != "--" {
		t.Errorf("%d prefixes, first %q, finished %+v", len(got), got[0], out)
	}
	sw, _ = a.Sweep()
	if sw.Phase != SweepIdle || sw.CompletedID != 1 || sw.PrefixesDone != 39*39 || sw.Searches != 39*39+1 || sw.Queued != 0 || sw.LastCapped {
		t.Errorf("%+v", sw)
	}
	if sw.NextStartAt != now.Add(24*time.Hour).Unix() || sw.FinishedAt != now.Unix() {
		t.Errorf("times %+v", sw)
	}
}

func TestSweepAppliesOneStepAtomicallyWithChildrenCardsAndCounters(t *testing.T) {
	_, a := sweepAcct(t)
	now := time.Now()
	startSweep(t, a, now, 1)
	var id int64
	a.Update(nil, func(tx *Tx) error { var err error; id, _, _, err = tx.SweepNext(); return err })
	in := SweepStepIn{RowID: id, MaxSearches: 100, Now: now, Interval: time.Hour, Children: []string{"aa", "ab"},
		Entries: []SweepEntry{{Email: "x@corp.example", Card: dcard("x@corp.example")}, {Email: "y@corp.example", Card: dcard("y@corp.example")}}}
	var out SweepStepOut
	a.Update(nil, func(tx *Tx) error { var err error; out, err = tx.SweepApply(in); return err })
	if out.Finished || !out.Changed {
		t.Errorf("%+v", out)
	}
	sw, _ := a.Sweep()
	if sw.Searches != 2 || sw.Found != 2 || sw.PrefixesDone != 1 || sw.Queued != 2+38 {
		t.Errorf("%+v", sw)
	}
	// The same entries again (another prefix): not counted twice, no change of the book.
	a.Update(nil, func(tx *Tx) error { id, _, _, _ = tx.SweepNext(); return nil })
	in.RowID, in.Children = id, nil
	a.Update(nil, func(tx *Tx) error { var err error; out, err = tx.SweepApply(in); return err })
	sw, _ = a.Sweep()
	if sw.Found != 2 || out.Changed || sw.PrefixesDone != 2 {
		t.Errorf("%+v %+v", sw, out)
	}
	if n, _ := a.DirectoryCardCount(); n != 2 {
		t.Errorf("%d cards", n)
	}
	// A failed transaction changes nothing.
	_, err := a.Update(nil, func(tx *Tx) error {
		id, _, _, _ = tx.SweepNext()
		_, err := tx.SweepApply(SweepStepIn{RowID: id, Entries: []SweepEntry{{Email: "z@corp.example", Card: dcard("z")}, {Email: "", Card: nil}}, Now: now})
		return err
	})
	if err == nil {
		t.Fatal("an empty entry was accepted")
	}
	if n, _ := a.DirectoryCardCount(); n != 2 {
		t.Errorf("a failed step left %d cards", n)
	}
	if s2, _ := a.Sweep(); s2.Searches != sw.Searches {
		t.Errorf("a failed step moved the counters")
	}
}

func TestSweepSearchCapEndsItAndClearsTheQueue(t *testing.T) {
	_, a := sweepAcct(t)
	now := time.Now()
	startSweep(t, a, now, 2)
	out := runSweep(t, a, now, 50, func(string) []string { return nil }, nil)
	sw, _ := a.Sweep()
	if !out.Finished || !out.Capped || sw.Searches != 50 || sw.Queued != 0 || !sw.LastCapped || sw.CompletedID != 0 || sw.Phase != SweepIdle {
		t.Errorf("%+v %+v", out, sw)
	}
}

func TestSweepRemovalRules(t *testing.T) {
	_, a := sweepAcct(t)
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	one := func(email string) func(string) []string {
		return func(p string) []string {
			if p == "a" {
				return []string{email}
			}
			return nil
		}
	}
	both := func(emails ...string) func(string) []string {
		return func(p string) []string {
			if p == "a" {
				return emails
			}
			return nil
		}
	}
	has := func(e string) bool { _, err := a.DirectoryCardByEmail(e); return err == nil }
	// Sweep 1 sees everybody (nothing is removed: there is no previous sweep).
	startSweep(t, a, t0, 1)
	out := runSweep(t, a, t0, 5000, both("keep@corp.example", "gone@corp.example", "confirmed@corp.example"), nil)
	if out.Removed != 0 || out.Capped {
		t.Fatalf("%+v", out)
	}
	// Sweep 2 (a week later) sees only keep: nothing is removed yet, gone and confirmed were seen in the previous sweep.
	t1 := t0.Add(7 * 24 * time.Hour)
	startSweep(t, a, t1, 1)
	out = runSweep(t, a, t1, 5000, one("keep@corp.example"), nil)
	if out.Removed != 0 || !has("gone@corp.example") {
		t.Fatalf("%+v", out)
	}
	// A lookup of the exact address confirms one of them after sweep 2 started.
	a.Update(nil, func(tx *Tx) error {
		tx.now = t1.Add(time.Hour)
		_, err := tx.PutDirectoryCard("confirmed@corp.example", dcard("confirmed@corp.example"))
		return err
	})
	byLookup, bySweep, _, _, _ := a.DirectoryCardFlags("confirmed@corp.example")
	if !byLookup || !bySweep {
		t.Errorf("flags %v %v", byLookup, bySweep)
	}
	// A capped sweep 3: nothing is removed.
	t2 := t1.Add(7 * 24 * time.Hour)
	startSweep(t, a, t2, 2)
	out = runSweep(t, a, t2, 10, one("keep@corp.example"), nil)
	if !out.Capped || out.Removed != 0 || !has("gone@corp.example") {
		t.Fatalf("a capped sweep removes nothing: %+v", out)
	}
	// Sweep 4 finishes: gone was last seen in sweep 1, the previous uncapped sweep is 2: removed. confirmed stays.
	t3 := t2.Add(7 * 24 * time.Hour)
	startSweep(t, a, t3, 1)
	out = runSweep(t, a, t3, 5000, one("keep@corp.example"), nil)
	if out.Capped || out.Removed != 1 || has("gone@corp.example") || !has("confirmed@corp.example") || !has("keep@corp.example") {
		t.Errorf("%+v gone %v confirmed %v keep %v", out, has("gone@corp.example"), has("confirmed@corp.example"), has("keep@corp.example"))
	}
	if st, _ := a.DirectoryState(); st.CTag == "" {
		t.Error("no ctag")
	}
}

func TestACardOnlyALookupFoundIsNotTheSweepsToRemove(t *testing.T) {
	_, a := sweepAcct(t)
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	a.Update(nil, func(tx *Tx) error {
		_, err := tx.PutDirectoryCard("mail@corp.example", dcard("mail@corp.example"))
		return err
	})
	for i, at := range []time.Time{t0, t0.Add(8 * 24 * time.Hour), t0.Add(16 * 24 * time.Hour)} {
		startSweep(t, a, at, 1)
		out := runSweep(t, a, at, 5000, func(string) []string { return nil }, nil)
		if out.Removed != 0 {
			t.Fatalf("sweep %d removed a lookup card", i+1)
		}
	}
	if _, err := a.DirectoryCardByEmail("mail@corp.example"); err != nil {
		t.Error(err)
	}
}

func TestALookupRemovalLeavesACardTheSweepSawRecently(t *testing.T) {
	_, a := sweepAcct(t)
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	a.Update(nil, func(tx *Tx) error {
		_, err := tx.PutDirectoryCard("p@corp.example", dcard("p@corp.example"))
		return err
	})
	startSweep(t, a, t0, 1)
	runSweep(t, a, t0, 5000, func(p string) []string {
		if p == "a" {
			return []string{"p@corp.example"}
		}
		return nil
	}, nil)
	var gone bool
	a.Update(nil, func(tx *Tx) (err error) { gone, err = tx.DeleteDirectoryCard("p@corp.example"); return })
	if gone {
		t.Error("the lookup rule removed a card that the sweep saw in the sweep that just finished")
	}
	by, sw, _, _, err := a.DirectoryCardFlags("p@corp.example")
	if err != nil || by || !sw {
		t.Errorf("it stays as a sweep card: %v %v %v", by, sw, err)
	}
	// A card no sweep saw is removed as before.
	a.Update(nil, func(tx *Tx) error { _, err := tx.PutDirectoryCard("q@corp.example", dcard("q")); return err })
	a.Update(nil, func(tx *Tx) (err error) { gone, err = tx.DeleteDirectoryCard("q@corp.example"); return })
	if !gone {
		t.Error("a lookup card is removed by the lookup rule")
	}
}

func TestSweepCalibrationResults(t *testing.T) {
	_, a := sweepAcct(t)
	now := time.Now()
	res := func(l, max int, ok bool) (bool, bool) {
		var c, f bool
		a.Update(nil, func(tx *Tx) (err error) { c, f, err = tx.SweepCalibrationResult(l, max, ok, now, 48*time.Hour); return })
		return c, f
	}
	a.Update(nil, func(tx *Tx) error { return tx.SweepStart(now) })
	if c, f := res(1, 4, false); c || f {
		t.Error("length 1 failed, 2 is next")
	}
	if sw, _ := a.Sweep(); sw.CalibNext != 2 || sw.Phase != SweepCalibrating || sw.Searches != 1 {
		t.Errorf("%+v", sw)
	}
	if c, f := res(2, 4, true); !c || f {
		t.Error("length 2 answered")
	}
	if sw, _ := a.Sweep(); sw.CalibLen != 2 || sw.Phase != SweepSweeping || sw.Queued != 39*39 {
		t.Errorf("%+v", sw)
	}
	a.Update(nil, func(tx *Tx) error { return tx.SweepStart(now) })
	if sw, _ := a.Sweep(); sw.ID != 2 || sw.CalibLen != 0 || sw.Searches != 0 {
		t.Errorf("a new sweep calibrates again: %+v", sw)
	}
	res(1, 4, false)
	res(2, 4, false)
	res(3, 4, false)
	if c, f := res(4, 4, false); c || !f {
		t.Error("none answered")
	}
	sw, _ := a.Sweep()
	if !sw.CalibFailed || sw.Phase != SweepIdle || sw.NextStartAt != now.Add(48*time.Hour).Unix() {
		t.Errorf("%+v", sw)
	}
	// A local part of two characters: the third length is not tried.
	a.Update(nil, func(tx *Tx) error { return tx.SweepStart(now) })
	res(1, 2, false)
	if c, f := res(2, 2, false); c || !f {
		t.Error("a local part of two characters can give no third length")
	}
	a.Update(nil, func(tx *Tx) error { return tx.SweepStart(now) })
	a.Update(nil, func(tx *Tx) error { return tx.SweepGiveUp(now, time.Hour) })
	if sw, _ := a.Sweep(); !sw.CalibFailed || sw.Phase != SweepIdle {
		t.Errorf("%+v", sw)
	}
}

func TestACalibrationOfThreeCharactersOrMoreEndsTheSweepForTheRun(t *testing.T) {
	_, a := sweepAcct(t)
	now := time.Now()
	for _, l := range []int{3, 4} {
		a.Update(nil, func(tx *Tx) error { return tx.SweepStart(now) })
		var c, f bool
		a.Update(nil, func(tx *Tx) (err error) { c, f, err = tx.SweepCalibrationResult(l, 4, true, now, 48*time.Hour); return })
		if !c || f {
			t.Errorf("length %d: calibrated %v failed %v", l, c, f)
		}
		sw, _ := a.Sweep()
		if sw.Phase != SweepIdle || sw.CalibLen != l || sw.Queued != 0 || sw.NextStartAt != now.Add(48*time.Hour).Unix() || sw.FinishedAt != now.Unix() || sw.CompletedID != 0 || sw.CalibFailed {
			t.Errorf("length %d: %+v", l, sw)
		}
		if _, _, ok, _ := nextPrefix(t, a); ok {
			t.Errorf("length %d: a prefix is queued", l)
		}
	}
	// 1 and 2 still sweep.
	a.Update(nil, func(tx *Tx) error { return tx.SweepStart(now) })
	a.Update(nil, func(tx *Tx) (err error) { _, _, err = tx.SweepCalibrationResult(2, 4, true, now, 48*time.Hour); return })
	if sw, _ := a.Sweep(); sw.Phase != SweepSweeping || sw.Queued != 39*39 {
		t.Errorf("%+v", sw)
	}
}

func nextPrefix(t *testing.T, a *Account) (int64, string, bool, error) {
	t.Helper()
	var id int64
	var p string
	var ok bool
	_, err := a.Update(nil, func(tx *Tx) (err error) { id, p, ok, err = tx.SweepNext(); return })
	return id, p, ok, err
}

func TestDropSweepKeepsLookupCardsAndRemovesTheRest(t *testing.T) {
	db, a := sweepAcct(t)
	now := time.Now()
	a.Update(nil, func(tx *Tx) error {
		_, err := tx.PutDirectoryCard("both@corp.example", dcard("both@corp.example"))
		return err
	})
	startSweep(t, a, now, 1)
	runSweep(t, a, now, 5000, func(p string) []string {
		if p == "a" {
			return []string{"both@corp.example", "sweep@corp.example"}
		}
		return nil
	}, nil)
	startSweep(t, a, now.Add(time.Hour), 1)
	ctag, _ := a.DirectoryState()
	n, err := db.DropSweep()
	if err != nil || n != 1 {
		t.Fatalf("removed %d: %v", n, err)
	}
	if _, err := a.DirectoryCardByEmail("sweep@corp.example"); err == nil {
		t.Error("a card only the sweep found stays")
	}
	by, sw, seen, _, err := a.DirectoryCardFlags("both@corp.example")
	if err != nil || !by || sw || seen != 0 {
		t.Errorf("both: %v %v %d %v", by, sw, seen, err)
	}
	if s, _ := a.Sweep(); s.ID != 0 || s.Phase != SweepIdle || s.Queued != 0 {
		t.Errorf("state %+v", s)
	}
	if st, _ := a.DirectoryState(); st.CTag == ctag.CTag {
		t.Errorf("the book changed: the ctag must too")
	}
	if n := countRows(t, a, `SELECT COUNT(*) FROM directory_sweep_queue`); n != 0 {
		t.Errorf("queue %d", n)
	}
	if n, err := db.DropSweep(); err != nil || n != 0 {
		t.Errorf("a second drop: %d %v", n, err)
	}
}

func TestWipeAndPurgeRemoveTheSweepToo(t *testing.T) {
	db, a := sweepAcct(t)
	startSweep(t, a, time.Now(), 2)
	a.Update(nil, func(tx *Tx) error { _, _, _, err := tx.SweepNext(); return err })
	a.Update(nil, func(tx *Tx) error { _, err := tx.PutDirectoryCard("x@corp.example", dcard("x")); return err })
	if _, err := a.Update(nil, func(tx *Tx) error { return tx.DeleteContactData() }); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`directory_sweep`, `directory_sweep_queue`, `directory_cards`} {
		if n := countRows(t, a, `SELECT COUNT(*) FROM `+q); n != 0 {
			t.Errorf("%s: %d rows after the wipe", q, n)
		}
	}
	startSweep(t, a, time.Now(), 2)
	if _, _, err := db.PurgeContactData(); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, a, `SELECT COUNT(*) FROM directory_sweep`); n != 0 {
		t.Errorf("purge left %d", n)
	}
	startSweep(t, a, time.Now(), 2)
	if _, err := db.PurgeDirectoryData(); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, a, `SELECT COUNT(*) FROM directory_sweep`); n != 0 {
		t.Errorf("directory purge left %d", n)
	}
	_ = strings.TrimSpace
}
