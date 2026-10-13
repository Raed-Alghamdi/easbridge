//go:build devfake

package engine_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"easbridge/internal/engine"
	"easbridge/internal/fakeupstream"
	"easbridge/internal/store"
	"easbridge/internal/testgal"
	"easbridge/internal/upstream"
)

const sweepDomain = "corp.example"

// sweepEnv is a directory environment with the full sweep on, a generated GAL and strict
// ANR matching. The one-character queries of the sweep are not violations here.
func sweepEnv(t *testing.T, n int, mods ...func(*engine.Config, *fakeupstream.Server)) (*env, []testgal.Person) {
	t.Helper()
	people := testgal.Generate(n, 7, []string{sweepDomain}, "outside.example")
	people = append(people, testgal.Person{Display: "Alice Own", First: "Alice", Last: "Own", Alias: "alice", Email: "alice@" + sweepDomain, Company: "Example Co"})
	e := dirEnv(t, []string{sweepDomain}, func(c *engine.Config, s *fakeupstream.Server) {
		c.Directory.Full = true
		c.Directory.Spacing = time.Millisecond
		c.Directory.Poll = 2 * time.Millisecond
		s.GALANR, s.GALAllowEnumeration = true, true
		s.AddGALPeople(fakeupstream.GALFromTestgal(people))
		for _, m := range mods {
			m(c, s)
		}
	})
	return e, people
}

func (e *env) sweep(a *engine.Account) store.Sweep {
	sw, err := a.Store().Sweep()
	if err != nil {
		e.t.Fatal(err)
	}
	return sw
}

func (e *env) waitSweepFinished(a *engine.Account, id int64) store.Sweep {
	e.t.Helper()
	var sw store.Sweep
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		sw = e.sweep(a)
		if sw.ID >= id && sw.Phase == store.SweepIdle && sw.FinishedAt != 0 && !(sw.ID > id) {
			return sw
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.t.Fatalf("the sweep %d did not finish: %+v", id, sw)
	return sw
}

// waitLog waits until the log holds sub (the engine writes the line after it has committed
// the state the tests wait for) and returns how often it is there then.
func (e *env) waitLog(sub string) int {
	e.t.Helper()
	eventually(e.t, "a log line with "+sub, func() bool { return strings.Contains(e.logs.String(), sub) })
	return strings.Count(e.logs.String(), sub)
}

func TestSweepFindsEveryEntryOfTheDirectoryDomainsOfAGeneratedGAL(t *testing.T) {
	e, people := sweepEnv(t, 3000)
	a := e.login()
	sw := e.waitSweepFinished(a, 1)
	want := testgal.InDomain(people, sweepDomain)
	cards := dirCards(t, a)
	if len(cards) != len(want) {
		t.Fatalf("%d cards, %d in-domain entries (%d in the GAL); sweep %+v", len(cards), len(want), len(people), sw)
	}
	have := map[string]bool{}
	for _, c := range cards {
		have[c.UID] = true
	}
	for _, p := range want {
		if c, err := a.Store().DirectoryCardByEmail(p.Email); err != nil || !have[c.UID] {
			t.Fatalf("no card for %s", p.Email)
		}
	}
	// No external contact, no alias and no home phone in any card.
	for _, c := range cards {
		v := string(c.VCard)
		if strings.Contains(v, "outside.example") || strings.Contains(v, "000 0000") {
			t.Errorf("a card holds what must not be kept:\n%s", v)
		}
	}
	if sw.Capped || sw.LastCapped || sw.CalibLen != 1 || sw.CalibFailed {
		t.Errorf("sweep %+v: the shortest query is 1 character here, and nothing was capped", sw)
	}
	if sw.Searches < 100 || sw.Searches > 5000 {
		t.Errorf("%d searches", sw.Searches)
	}
	t.Logf("%d GAL entries (%d in the domain): %d searches, %d prefixes", len(people), len(want), sw.Searches, sw.PrefixesDone)
	// The Arabic names are in, reached through their Latin aliases.
	arabic := 0
	for _, p := range want {
		if p.Arabic {
			arabic++
			c, _ := a.Store().DirectoryCardByEmail(p.Email)
			if !strings.Contains(string(c.VCard), "FN:"+p.Display) {
				t.Errorf("the Arabic display name of %s is not in its card", p.Email)
			}
		}
	}
	if arabic == 0 {
		t.Error("the generated GAL has no Arabic entry")
	}
	if n := e.srv.GALViolations(); len(n) != 0 {
		t.Errorf("%v", n)
	}
}

// sweepRestart restarts the manager over the same database with the sweep on (or off) and the
// given changes to the directory settings.
func (e *env) sweepRestart(full bool, mod func(*engine.DirectoryConfig)) *engine.Account {
	e.restart(func(c *engine.Config) {
		c.ContactsEnabled = true
		c.Directory = dirConfig(sweepDomain)
		c.Directory.Spacing = time.Millisecond
		c.Directory.Poll = 2 * time.Millisecond
		c.Directory.Full = full
		if mod != nil {
			mod(&c.Directory)
		}
	})
	a := e.login()
	a.Attach(func() {})
	return a
}

// queriesFrom returns the queries sent since index n.
func queriesFrom(e *env, n int) []string {
	qs := e.srv.GALQueries()
	if n > len(qs) {
		return nil
	}
	return qs[n:]
}

func TestSweepCalibratesTheShortestQueryExchangeAnswers(t *testing.T) {
	for min := 1; min <= 2; min++ {
		t.Run(fmt.Sprint("answers from ", min, " characters"), func(t *testing.T) {
			e, _ := sweepEnv(t, 300, func(c *engine.Config, s *fakeupstream.Server) {
				s.GALMinQuery = min
				c.Directory.SweepMaxSearches = 80
			})
			a := e.login()
			sw := e.waitSweepFinished(a, 1)
			if sw.CalibLen != min || sw.CalibFailed {
				t.Fatalf("sweep %+v", sw)
			}
			qs := e.srv.GALQueries()
			own := []string{"a", "al", "ali", "alic"}
			for i := 0; i < min; i++ {
				if qs[i] != own[i] {
					t.Fatalf("calibration query %d is %q, want %q (the first characters of the account's own local part): %q", i, qs[i], own[i], qs[:min+1])
				}
			}
			// After the calibration the sweep walks the strings of that length, from the first.
			first := strings.Repeat("a", min)
			if qs[min] != first {
				t.Errorf("the sweep starts with %q, want %q", qs[min], first)
			}
			for _, q := range qs[min:] {
				if len(q) < min {
					t.Errorf("a sweep query shorter than the calibrated length: %q", q)
				}
			}
		})
	}
}

// From 3 characters on, walking every string of that length is not feasible (59,319 searches
// before a single division): the sweep stops itself for the run, says so once, and shows it.
func TestSweepStopsItselfWhenTheShortestQueryIsThreeCharactersOrMore(t *testing.T) {
	for min := 3; min <= 4; min++ {
		t.Run(fmt.Sprint("answers from ", min, " characters"), func(t *testing.T) {
			e, _ := sweepEnv(t, 300, func(c *engine.Config, s *fakeupstream.Server) { s.GALMinQuery = min })
			a := e.login()
			a.Attach(func() {})
			sw := e.waitSweepFinished(a, 1)
			if sw.CalibLen != min || sw.CalibFailed || sw.Queued != 0 || sw.Searches != min || sw.CompletedID != 0 || sw.LastCapped {
				t.Fatalf("sweep %+v", sw)
			}
			own := []string{"a", "al", "ali", "alic"}
			if got := strings.Join(e.srv.GALQueries(), "|"); got != strings.Join(own[:min], "|") {
				t.Errorf("queries %q: only the calibration searches may be sent", got)
			}
			consistently(t, "no prefix is searched", 300*time.Millisecond, func() bool { return len(e.srv.GALQueries()) == min })
			if n := e.waitLog("directory sweep stopped for this run"); n != 1 {
				t.Errorf("%d log lines about it, want 1", n)
			}
			if !strings.Contains(e.logs.String(), fmt.Sprintf("no query shorter than %d characters", min)) {
				t.Errorf("the log line does not say why:\n%s", e.logs.String())
			}
			st := e.mgr.Status().Accounts[0].Contacts.Sweep
			if st == nil || !st.SweepInfeasible || st.SweepInfeasibleLength != min || st.CalibrationFailed || st.State != "waiting" || st.WaitingUntilUnix == 0 || st.PrefixesQueued != 0 {
				t.Errorf("health %+v", st)
			}
			if cards := dirCards(t, a); len(cards) != 0 {
				t.Errorf("%d cards from a sweep that did not run", len(cards))
			}
			// The next sweep, after the interval, calibrates again and stops again, once more.
			e.clock.Advance(721 * time.Hour)
			sw = e.waitSweepFinished(a, 2)
			if sw.CalibLen != min {
				t.Errorf("second sweep %+v", sw)
			}
		})
	}
}

func TestSweepIsGivenUpWhenNoQueryUpToFourCharactersIsAnswered(t *testing.T) {
	e, _ := sweepEnv(t, 300, func(c *engine.Config, s *fakeupstream.Server) { s.GALMinQuery = 5 })
	a := e.login()
	a.Attach(func() {})
	sw := e.waitSweepFinished(a, 1)
	if !sw.CalibFailed || sw.CalibLen != 0 || sw.Searches != 4 || sw.Queued != 0 {
		t.Fatalf("sweep %+v", sw)
	}
	if qs := e.srv.GALQueries(); strings.Join(qs, "|") != "a|al|ali|alic" {
		t.Errorf("queries %q", qs)
	}
	if n := e.waitLog("directory sweep not possible"); n != 1 {
		t.Errorf("%d log lines about it, want 1", n)
	}
	st := e.mgr.Status().Accounts[0].Contacts.Sweep
	if st == nil || !st.Full || !st.CalibrationFailed || st.State != "waiting" || st.WaitingUntilUnix == 0 || st.CalibratedLength != 0 {
		t.Errorf("health %+v", st)
	}
	// The lookups of addresses from mail go on.
	e.deliver("m1", "pat@corp.example", "alice@example.test", "", e.clock.Now())
	eventually(t, "a lookup", func() bool { return countQueries(e, "pat@corp.example") >= 1 })
	// And the next try comes after the interval, with a new calibration.
	n := len(e.srv.GALQueries())
	e.clock.Advance(721 * time.Hour)
	e.waitSweepFinished(a, 2)
	if got := strings.Join(queriesFrom(e, n), "|"); !strings.Contains(got, "a|al|ali|alic") {
		t.Errorf("the next sweep calibrates again: %q", got)
	}
}

func TestSweepAnswerWithNoMatchIsNotACalibration(t *testing.T) {
	e := dirEnv(t, []string{sweepDomain}, func(c *engine.Config, s *fakeupstream.Server) {
		c.Directory.Full = true
		c.Directory.Spacing = time.Millisecond
		c.Directory.Poll = 2 * time.Millisecond
		s.GALANR, s.GALAllowEnumeration = true, true // an empty directory: every query is answered, with nothing
	})
	a := e.login()
	sw := e.waitSweepFinished(a, 1)
	if !sw.CalibFailed || sw.Searches != 4 {
		t.Errorf("a length counts only if the search finds an entry: %+v", sw)
	}
}

func TestSweepDividesSaturatedPrefixesAndOnlyThose(t *testing.T) {
	for _, omitTotal := range []bool{false, true} {
		t.Run(fmt.Sprint("total omitted ", omitTotal), func(t *testing.T) {
			var people []testgal.Person
			people = append(people, testgal.Person{Display: "Alice Own", Alias: "alice", Email: "alice@" + sweepDomain})
			for i := 0; i < 130; i++ { // "k" matches 130 entries: more than one search returns
				al := fmt.Sprintf("k%03d", i)
				people = append(people, testgal.Person{Display: strings.ToUpper(al), Alias: al, Email: al + "@" + sweepDomain})
			}
			for i := 0; i < 5; i++ { // "m" matches 5: all of them are returned
				al := fmt.Sprintf("m%d", i)
				people = append(people, testgal.Person{Display: strings.ToUpper(al), Alias: al, Email: al + "@" + sweepDomain})
			}
			for i := 0; i < 99; i++ { // "q" matches exactly 99 (and 100 would fit the window)
				al := fmt.Sprintf("q%02d", i)
				people = append(people, testgal.Person{Display: strings.ToUpper(al), Alias: al, Email: al + "@" + sweepDomain})
			}
			e := dirEnv(t, []string{sweepDomain}, func(c *engine.Config, s *fakeupstream.Server) {
				c.Directory.Full = true
				c.Directory.Spacing = time.Millisecond
				c.Directory.Poll = 2 * time.Millisecond
				s.GALANR, s.GALAllowEnumeration, s.GALOmitTotal = true, true, omitTotal
				s.AddGALPeople(fakeupstream.GALFromTestgal(people))
			})
			a := e.login()
			sw := e.waitSweepFinished(a, 1)
			if n := len(dirCards(t, a)); n != len(people) {
				t.Fatalf("%d cards, want %d; sweep %+v", n, len(people), sw)
			}
			qs := map[string]bool{}
			for _, q := range e.srv.GALQueries() {
				qs[q] = true
			}
			for _, c := range store.SweepAlphabet {
				if !qs["k"+string(c)] {
					t.Errorf("k%c was not searched: the prefix k is saturated", c)
				}
				if qs["m"+string(c)] {
					t.Errorf("m%c was searched: the prefix m is not saturated", c)
				}
			}
			if !omitTotal && qs["qa"] {
				t.Errorf("the prefix q returned every match (Total equals the entries): it is not divided")
			}
		})
	}
}

func TestSweepDividesAPrefixExchangeRefuses(t *testing.T) {
	e, people := sweepEnv(t, 600, func(c *engine.Config, s *fakeupstream.Server) {
		s.GALRefuse = func(q string) bool { return q == "m" || q == "ma" }
	})
	a := e.login()
	e.waitSweepFinished(a, 1)
	qs := map[string]bool{}
	for _, q := range e.srv.GALQueries() {
		qs[q] = true
	}
	for _, c := range store.SweepAlphabet {
		if !qs["m"+string(c)] {
			t.Errorf("m%c: a refused prefix is divided", c)
		}
	}
	if !qs["maa"] {
		t.Errorf("ma was refused as well and is divided in turn")
	}
	want := testgal.InDomain(people, sweepDomain)
	if n := len(dirCards(t, a)); n != len(want) {
		t.Errorf("%d cards, %d in-domain entries: the refused prefixes lost nobody", n, len(want))
	}
}

func TestSweepEndsEarlyAtTheDepthCapAndAtTheSearchCap(t *testing.T) {
	t.Run("a prefix of 20 characters that is still saturated", func(t *testing.T) {
		var people []testgal.Person
		people = append(people, testgal.Person{Display: "Alice Own", Alias: "alice", Email: "alice@" + sweepDomain})
		for i := 0; i < 130; i++ {
			al := fmt.Sprintf("%s%03d", strings.Repeat("z", 20), i)
			people = append(people, testgal.Person{Display: al, Alias: al, Email: al + "@" + sweepDomain})
		}
		e := dirEnv(t, []string{sweepDomain}, func(c *engine.Config, s *fakeupstream.Server) {
			c.Directory.Full = true
			c.Directory.Spacing = time.Millisecond
			c.Directory.Poll = 2 * time.Millisecond
			s.GALANR, s.GALAllowEnumeration = true, true
			s.AddGALPeople(fakeupstream.GALFromTestgal(people))
		})
		a := e.login()
		sw := e.waitSweepFinished(a, 1)
		if !sw.LastCapped || sw.CompletedID != 0 {
			t.Errorf("sweep %+v: capped, and not a completed one", sw)
		}
		for _, q := range e.srv.GALQueries() {
			if len(q) > 20 {
				t.Errorf("a query of %d characters", len(q))
			}
		}
		if n := e.waitLog("directory sweep finished early"); n != 1 {
			t.Errorf("%d log lines about the cap, want 1", n)
		}
		st := e.mgr.Status().Accounts[0].Contacts.Sweep
		if st == nil || !st.LastSweepCapped {
			t.Errorf("health %+v", st)
		}
	})
	t.Run("5,000 searches (here 40)", func(t *testing.T) {
		e, _ := sweepEnv(t, 3000, func(c *engine.Config, _ *fakeupstream.Server) { c.Directory.SweepMaxSearches = 40 })
		a := e.login()
		sw := e.waitSweepFinished(a, 1)
		if !sw.LastCapped || sw.Searches != 40 || sw.Queued != 0 {
			t.Errorf("sweep %+v", sw)
		}
		if e.srv.Calls("SearchDirectory") != 40 {
			t.Errorf("%d searches", e.srv.Calls("SearchDirectory"))
		}
	})
}

func TestSweepKeepsOnlyTheDirectoryDomainsOnceEachAndMergesWithLookups(t *testing.T) {
	e, people := sweepEnv(t, 500)
	// An entry listed twice, an entry in a sub domain of the directory domain, and one found by mail first.
	dup := testgal.Person{Display: "Dora Twice", First: "Dora", Last: "Twice", Alias: "dora", Email: "dora@" + sweepDomain}
	e.srv.AddGALPeople(fakeupstream.GALFromTestgal([]testgal.Person{dup, dup,
		{Display: "Sub Domain", Alias: "subd", Email: "subd@sub." + sweepDomain}, {Display: "Case Mix", Alias: "casemix", Email: "CaseMix@" + strings.ToUpper(sweepDomain)}}))
	pat := people[10]
	e.deliver("m1", pat.Email, "alice@example.test", "", e.clock.Now().Add(-time.Hour))
	a := e.login()
	a.Attach(func() {})
	e.waitSweepFinished(a, 1)
	if _, err := a.Store().DirectoryCardByEmail("subd@sub." + sweepDomain); err == nil {
		t.Error("an entry of a sub domain was kept")
	}
	if _, err := a.Store().DirectoryCardByEmail("casemix@" + sweepDomain); err != nil {
		t.Errorf("an address in other case is the same address: %v", err)
	}
	want := len(testgal.InDomain(people, sweepDomain)) + 2 // dora and casemix
	if n := len(dirCards(t, a)); n != want {
		t.Errorf("%d cards, want %d (each address once)", n, want)
	}
	// The person from mail: one card, found both ways.
	byLookup, bySweep, seen, _, err := a.Store().DirectoryCardFlags(pat.Email)
	if err != nil || !byLookup || (pat.External && bySweep) || (!pat.External && (!bySweep || seen != 1)) {
		if !pat.External {
			t.Errorf("flags of the person from mail: lookup %v sweep %v seen %d %v", byLookup, bySweep, seen, err)
		}
	}
	uids := map[string]bool{}
	for _, c := range dirCards(t, a) {
		if uids[c.UID] {
			t.Errorf("two cards with the uid %s", c.UID)
		}
		uids[c.UID] = true
	}
}

func TestSweepResumesAfterARestartAndDoesNotStartOver(t *testing.T) {
	e, people := sweepEnv(t, 2000)
	a := e.login()
	a.Attach(func() {})
	eventually(t, "some prefixes done", func() bool { return e.sweep(a).PrefixesDone >= 150 })
	e.mgr.Close()
	before := e.sweep(a)
	if before.Phase != store.SweepSweeping || before.ID != 1 || before.Queued == 0 {
		t.Fatalf("state at the stop: %+v", before)
	}
	n := len(e.srv.GALQueries())
	a = e.sweepRestart(true, nil)
	sw := e.waitSweepFinished(a, 1)
	if sw.ID != 1 || sw.CompletedID != 1 || sw.LastCapped {
		t.Errorf("sweep %+v: the same sweep, finished", sw)
	}
	want := testgal.InDomain(people, sweepDomain)
	if got := len(dirCards(t, a)); got != len(want) {
		t.Errorf("%d cards, want %d", got, len(want))
	}
	// Nothing that was done before the stop is done again (the search in flight at the stop may be).
	done := map[string]bool{}
	for _, q := range e.srv.GALQueries()[:n] {
		done[q] = true
	}
	again := 0
	for _, q := range queriesFrom(e, n) {
		if done[q] {
			again++
		}
	}
	if again > 1 {
		t.Errorf("%d prefixes were searched again after the restart", again)
	}
	if got := queriesFrom(e, n); len(got) > 0 && got[0] == "a" && sw.CalibLen == 1 && len(got) > 1 && got[1] == "al" {
		t.Errorf("the restart calibrated again: %q", got[:3])
	}
}

func TestSweepRemovesACardOnlyWhenTwoCompletedSweepsDidNotSeeIt(t *testing.T) {
	e, _ := sweepEnv(t, 300)
	pat := testgal.Person{Display: "Pat Smith", First: "Pat", Last: "Smith", Alias: "pat", Email: "pat@" + sweepDomain}
	e.srv.AddGALPeople(fakeupstream.GALFromTestgal([]testgal.Person{pat}))
	a := e.login()
	a.Attach(func() {})
	e.waitSweepFinished(a, 1)
	if _, err := a.Store().DirectoryCardByEmail(pat.Email); err != nil {
		t.Fatalf("sweep 1 missed pat: %v", err)
	}
	total := len(dirCards(t, a))
	e.srv.RemoveGALPerson(pat.Email) // the person left
	e.clock.Advance(721 * time.Hour)
	sw := e.waitSweepFinished(a, 2)
	if _, err := a.Store().DirectoryCardByEmail(pat.Email); err != nil || len(dirCards(t, a)) != total || sw.CompletedID != 2 {
		t.Fatalf("one sweep without the card removes nothing (it was seen in the previous one): %v %+v", err, sw)
	}
	e.clock.Advance(721 * time.Hour)
	sw = e.waitSweepFinished(a, 3)
	if _, err := a.Store().DirectoryCardByEmail(pat.Email); err == nil || len(dirCards(t, a)) != total-1 {
		t.Errorf("two sweeps in a row without the card remove it")
	}
	if e.waitLog("cards_removed=1") < 1 {
		t.Errorf("the log says nothing about the removal")
	}
	st, _ := a.Store().DirectoryState()
	_ = st
}

// hide makes the entry with this alias unreachable for the prefix walk without taking it out of
// the GAL: 150 other entries share its alias as a prefix, and it sorts behind them, so it is
// beyond the 100 entries of the window of the prefix that is exactly its alias (the known limit
// of the sweep). A lookup of its exact address still finds it.
func hide(e *env, alias string) {
	var ps []testgal.Person
	for i := 0; i < 150; i++ {
		al := fmt.Sprintf("%s%03d", alias, i)
		ps = append(ps, testgal.Person{Display: al, Alias: al, Email: al + "@" + sweepDomain})
	}
	e.srv.AddGALPeople(fakeupstream.GALFromTestgal(ps))
}

func TestACardThatALookupConfirmedStaysAndOneThatNoLookupConfirmedGoes(t *testing.T) {
	e, _ := sweepEnv(t, 200, func(c *engine.Config, _ *fakeupstream.Server) {
		c.Directory.Refresh = 10 * 24 * time.Hour
		c.Directory.FullInterval = 7 * 24 * time.Hour
	})
	pat := testgal.Person{Display: "Zzzpat", Alias: "zzzpat", Email: "zzzpat@" + sweepDomain}
	quinn := testgal.Person{Display: "Qqqqquinn", Alias: "qqqqquinn", Email: "qqqqquinn@" + sweepDomain}
	e.srv.AddGALPeople(fakeupstream.GALFromTestgal([]testgal.Person{pat, quinn}))
	e.deliver("m1", pat.Email, "alice@example.test", "", e.clock.Now().Add(-time.Hour)) // pat is a correspondent
	a := e.login()
	a.Attach(func() {})
	e.waitState(a, pat.Email, store.CandFound)
	e.waitSweepFinished(a, 1)
	for _, p := range []testgal.Person{pat, quinn} {
		if _, bySweep, seen, _, err := a.Store().DirectoryCardFlags(p.Email); err != nil || !bySweep || seen != 1 {
			t.Fatalf("%s after sweep 1: %v %v %d", p.Email, err, bySweep, seen)
		}
	}
	if byLookup, _, _, _, _ := a.Store().DirectoryCardFlags(pat.Email); !byLookup {
		t.Fatal("pat was found by the lookup as well")
	}
	hide(e, "zzzpat")
	hide(e, "qqqqquinn")
	e.clock.Advance(7*24*time.Hour + time.Hour)
	e.waitSweepFinished(a, 2) // neither is seen, both are kept (seen in the previous sweep)
	for _, p := range []testgal.Person{pat, quinn} {
		if _, err := a.Store().DirectoryCardByEmail(p.Email); err != nil {
			t.Fatalf("%s was removed after one sweep without it", p.Email)
		}
	}
	// pat's lookup comes due (10 days after the first) and confirms the card, after sweep 2 began.
	e.clock.Advance(4 * 24 * time.Hour)
	eventually(t, "the lookup of pat is refreshed", func() bool {
		_, _, _, confirmed, err := a.Store().DirectoryCardFlags(pat.Email)
		return err == nil && confirmed >= e.clock.Now().Add(-time.Hour).Unix()
	})
	e.clock.Advance(4 * 24 * time.Hour)
	e.waitSweepFinished(a, 3)
	if _, err := a.Store().DirectoryCardByEmail(pat.Email); err != nil {
		t.Errorf("a lookup of the exact address confirmed pat since the previous sweep started: the card stays (%v)", err)
	}
	if _, err := a.Store().DirectoryCardByEmail(quinn.Email); err == nil {
		_, bs, seen, conf, _ := a.Store().DirectoryCardFlags(quinn.Email)
		sw := e.sweep(a)
		t.Logf("quinn: bySweep %v seen %d confirmed %d; sweep %+v; queries with qqqqq: %d", bs, seen, conf, sw, countQueries(e, "qqqqquinn"))
		t.Errorf("nobody confirmed quinn and two sweeps did not see him: the card goes")
	}
}

func TestNothingIsRemovedAfterACappedOrAnIncompleteSweep(t *testing.T) {
	e, _ := sweepEnv(t, 300)
	pat := testgal.Person{Display: "Zzzpat", Alias: "zzzpat", Email: "zzzpat@" + sweepDomain}
	e.srv.AddGALPeople(fakeupstream.GALFromTestgal([]testgal.Person{pat}))
	a := e.login()
	a.Attach(func() {})
	e.waitSweepFinished(a, 1)
	hide(e, "zzzpat")
	have := func() bool { _, err := a.Store().DirectoryCardByEmail(pat.Email); return err == nil }
	// Two capped sweeps in a row: nothing is removed.
	a = e.sweepRestart(true, func(d *engine.DirectoryConfig) { d.SweepMaxSearches = 40 })
	for id := int64(2); id <= 3; id++ {
		e.clock.Advance(721 * time.Hour)
		sw := e.waitSweepFinished(a, id)
		if !sw.LastCapped || sw.CompletedID != 1 || !have() {
			t.Fatalf("capped sweep %d: %+v, card kept %v", id, sw, have())
		}
	}
	// A sweep that finishes: the card was seen in the previous completed sweep (the first one): kept.
	a = e.sweepRestart(true, nil)
	e.clock.Advance(721 * time.Hour)
	sw := e.waitSweepFinished(a, 4)
	if sw.LastCapped || sw.CompletedID != 4 || !have() {
		t.Fatalf("sweep 4: %+v kept %v", sw, have())
	}
	// And now it has been missed by the completed sweeps 4 and 5? It was last seen in 1; the previous completed one is 4.
	e.clock.Advance(721 * time.Hour)
	e.waitSweepFinished(a, 5)
	if have() {
		t.Errorf("two completed sweeps in a row without the card remove it, even after capped ones in between")
	}
}

func TestNothingIsRemovedWhileASweepIsStillRunning(t *testing.T) {
	e, _ := sweepEnv(t, 300)
	pat := testgal.Person{Display: "Pat Smith", First: "Pat", Last: "Smith", Alias: "pat", Email: "pat@" + sweepDomain}
	e.srv.AddGALPeople(fakeupstream.GALFromTestgal([]testgal.Person{pat}))
	a := e.login()
	a.Attach(func() {})
	e.waitSweepFinished(a, 1)
	e.srv.RemoveGALPerson(pat.Email)
	e.clock.Advance(721 * time.Hour)
	e.waitSweepFinished(a, 2)
	e.clock.Advance(721 * time.Hour)
	e.srv.HoldAfter("SearchDirectory", testUser, "", 20)
	eventually(t, "sweep 3 under way", func() bool {
		sw := e.sweep(a)
		return sw.ID == 3 && sw.Phase == store.SweepSweeping && sw.Searches >= 10
	})
	consistently(t, "the card stays until the sweep has finished", 150*time.Millisecond, func() bool {
		_, err := a.Store().DirectoryCardByEmail(pat.Email)
		return err == nil
	})
	e.srv.Release("SearchDirectory", testUser, "")
	e.waitSweepFinished(a, 3)
	if _, err := a.Store().DirectoryCardByEmail(pat.Email); err == nil {
		t.Errorf("the finished sweep removes it")
	}
}

func TestLookupsOfAddressesFromMailGoBeforeTheSweep(t *testing.T) {
	e, _ := sweepEnv(t, 3000, func(c *engine.Config, _ *fakeupstream.Server) { c.Directory.Spacing = 30 * time.Millisecond })
	a := e.login()
	eventually(t, "the sweep is under way", func() bool { return e.sweep(a).PrefixesDone >= 5 })
	e.srv.AddGALPerson(gal("late@corp.example", "Late Person", "Late", "Person"))
	n := len(e.srv.GALQueries())
	e.deliver("m1", "late@corp.example", "alice@example.test", "", e.clock.Now())
	eventually(t, "the lookup", func() bool { return countQueries(e, "late@corp.example") == 1 })
	qs := e.srv.GALQueries()
	pos := -1
	for i, q := range qs {
		if q == "late@corp.example" {
			pos = i
		}
	}
	// A search of the sweep may have been in flight, one more may have been due; no further prefix goes first.
	if pos < 0 || pos > n+2 {
		t.Errorf("the lookup was query %d, the sweep had sent %d before the mail arrived", pos, n)
	}
	e.waitState(a, "late@corp.example", store.CandFound)
	if sw := e.sweep(a); sw.Phase != store.SweepSweeping {
		t.Errorf("the sweep goes on afterwards: %+v", sw)
	}
}

func TestLookupsAndTheSweepShareOneBudget(t *testing.T) {
	t.Run("spacing", func(t *testing.T) {
		const spacing = 50 * time.Millisecond
		e, _ := sweepEnv(t, 1000, func(c *engine.Config, s *fakeupstream.Server) {
			c.Directory.Spacing = spacing
			s.GALLatency = 5 * time.Millisecond
		})
		for i := 0; i < 4; i++ {
			e.deliver(fmt.Sprint("m", i), fmt.Sprintf("friend%d@corp.example", i), "alice@example.test", "", e.clock.Now().Add(-time.Duration(i+1)*time.Hour))
		}
		a := e.login()
		eventually(t, "lookups and sweep", func() bool { return e.sweep(a).PrefixesDone >= 8 && countQueries(e, "friend3@corp.example") >= 1 })
		ts := e.srv.GALSearchTimes()
		for i := 1; i < len(ts); i++ {
			if gap := ts[i].Sub(ts[i-1]); gap < spacing-8*time.Millisecond {
				t.Fatalf("searches %d and %d are %v apart, spacing %v: the sweep does not get a spacing of its own", i-1, i, gap, spacing)
			}
		}
		if e.srv.GALMaxInFlight() != 1 {
			t.Errorf("%d in flight", e.srv.GALMaxInFlight())
		}
	})
	t.Run("the day's cap", func(t *testing.T) {
		e, _ := sweepEnv(t, 1000, func(c *engine.Config, _ *fakeupstream.Server) { c.Directory.DailyCap = 30 })
		a := e.login()
		a.Attach(func() {})
		eventually(t, "30 searches", func() bool { return e.srv.Calls("SearchDirectory") == 30 })
		e.deliver("m1", "friend@corp.example", "alice@example.test", "", e.clock.Now())
		consistently(t, "neither the sweep nor a lookup goes past the cap", 300*time.Millisecond, func() bool { return e.srv.Calls("SearchDirectory") == 30 })
		st, _ := a.Store().DirectoryState()
		if st.LookupsOn(e.clock.Now().UTC().Format("2006-01-02")) != 30 {
			t.Errorf("state %+v", st)
		}
		e.clock.Advance(13 * time.Hour)
		eventually(t, "the next day", func() bool { return e.srv.Calls("SearchDirectory") > 30 && countQueries(e, "friend@corp.example") >= 1 })
	})
}

func TestSweepStopsForTheBreakerRetryAfterBackoffAndAccessDenied(t *testing.T) {
	t.Run("breaker", func(t *testing.T) {
		e, _ := sweepEnv(t, 1000, func(c *engine.Config, _ *fakeupstream.Server) {
			c.BreakerBase, c.BreakerMax = 100*time.Millisecond, 3*time.Second
			c.Directory.Spacing = 100 * time.Millisecond
		})
		a := e.login()
		eventually(t, "the sweep runs", func() bool { return e.sweep(a).PrefixesDone >= 2 })
		e.srv.FailNext("Sync", &upstream.ThrottleError{HTTP: 503, Explicit: true, RetryAfter: 1200 * time.Millisecond}, 1)
		e.deliver("m3", "bob@elsewhere.example", "alice@example.test", "", e.clock.Now())
		eventually(t, "the breaker is open", func() bool { return e.mgr.Status().Accounts[0].Paused })
		time.Sleep(150 * time.Millisecond)
		n := e.srv.Calls("SearchDirectory")
		st, _ := a.Store().DirectoryState()
		day := e.clock.Now().UTC().Format("2006-01-02")
		consistently(t, "no search is sent or counted while the breaker is open", 500*time.Millisecond, func() bool {
			s2, _ := a.Store().DirectoryState()
			return e.srv.Calls("SearchDirectory") == n && s2.LookupsOn(day) == st.LookupsOn(day)
		})
		eventually(t, "the sweep goes on", func() bool { return e.srv.Calls("SearchDirectory") > n })
	})
	t.Run("Retry-After", func(t *testing.T) {
		e, _ := sweepEnv(t, 300)
		a := e.login()
		eventually(t, "the sweep runs", func() bool { return e.sweep(a).PrefixesDone >= 3 })
		e.srv.FailNext("SearchDirectory", &upstream.ThrottleError{HTTP: 500, RetryAfter: 400 * time.Millisecond}, 1)
		n := e.srv.Calls("SearchDirectory")
		eventually(t, "the failing search", func() bool { return e.srv.Calls("SearchDirectory") > n })
		ts := e.srv.GALSearchTimes()
		last := len(ts) - 1
		eventually(t, "the next search", func() bool { ts = e.srv.GALSearchTimes(); return len(ts) > last+1 })
		if gap := ts[last+1].Sub(ts[last]); gap < 380*time.Millisecond {
			t.Errorf("the search after the failed one came %v later; Retry-After said 400 ms", gap)
		}
	})
	t.Run("search statuses back off and do not pause the mail", func(t *testing.T) {
		e, _ := sweepEnv(t, 300, func(c *engine.Config, _ *fakeupstream.Server) {
			c.Directory.BackoffBase, c.Directory.BackoffMax = 40*time.Millisecond, 160*time.Millisecond
		})
		a := e.login()
		eventually(t, "the sweep runs", func() bool { return e.sweep(a).PrefixesDone >= 3 })
		e.srv.FailNext("SearchDirectory", &upstream.StatusError{Op: "Search", Code: 3, Reason: "error"}, 3)
		n := e.srv.Calls("SearchDirectory")
		eventually(t, "three failures", func() bool { return e.srv.Calls("SearchDirectory") >= n+3 })
		ts := e.srv.GALSearchTimes()
		if gap := ts[n+1].Sub(ts[n]); gap < 35*time.Millisecond {
			t.Errorf("first back-off %v", gap)
		}
		if gap := ts[n+2].Sub(ts[n+1]); gap < 75*time.Millisecond {
			t.Errorf("second back-off %v", gap)
		}
		if e.mgr.Status().Accounts[0].PauseTrips != 0 {
			t.Errorf("a Search status tripped the breaker of the mailbox")
		}
		e.waitSweepFinished(a, 1)
	})
	t.Run("access denied stops it for 24 hours", func(t *testing.T) {
		e, _ := sweepEnv(t, 300)
		a := e.login()
		a.Attach(func() {})
		eventually(t, "the sweep runs", func() bool { return e.sweep(a).PrefixesDone >= 3 })
		e.srv.FailNext("SearchDirectory", &upstream.StatusError{Op: "Search", Code: 13, Perm: true, Reason: "blocked"}, 1)
		eventually(t, "the pause", func() bool { st, _ := a.Store().DirectoryState(); return st.PausedUntil != 0 })
		n := e.srv.Calls("SearchDirectory")
		consistently(t, "no search during the pause", 250*time.Millisecond, func() bool { return e.srv.Calls("SearchDirectory") == n })
		if c := strings.Count(e.logs.String(), "directory lookups are stopped for 24 hours"); c != 1 {
			t.Errorf("%d log lines", c)
		}
		e.clock.Advance(25 * time.Hour)
		e.waitSweepFinished(a, 1)
	})
}

func TestTurningTheSweepOffStopsItAndKeepsTheCardsLookupsConfirmed(t *testing.T) {
	e, people := sweepEnv(t, 300)
	known := people[5]
	if known.External {
		known = people[6]
	}
	e.deliver("m1", known.Email, "alice@example.test", "", e.clock.Now().Add(-time.Hour))
	a := e.login()
	e.waitSweepFinished(a, 1)
	e.waitState(a, known.Email, store.CandFound)
	all := len(dirCards(t, a))
	// The service drops the sweep state at start when the sweep is off (cmd/easbridge).
	e.mgr.Close()
	removed, err := e.db.DropSweep()
	if err != nil || removed != all-1 {
		t.Fatalf("removed %d of %d cards: %v (only the card a lookup confirmed stays)", removed, all, err)
	}
	n := e.srv.Calls("SearchDirectory")
	a = e.sweepRestart(false, nil)
	sw := e.sweep(a)
	if sw.ID != 0 || sw.Phase != store.SweepIdle {
		t.Errorf("sweep state survived: %+v", sw)
	}
	if cs := dirCards(t, a); len(cs) != 1 {
		t.Errorf("%d cards", len(cs))
	}
	if byLookup, bySweep, _, _, err := a.Store().DirectoryCardFlags(known.Email); err != nil || !byLookup || bySweep {
		t.Errorf("flags %v %v %v", byLookup, bySweep, err)
	}
	consistently(t, "no sweep searches", 200*time.Millisecond, func() bool { return e.srv.Calls("SearchDirectory") == n })
	if st := e.mgr.Status().Accounts[0].Contacts; st == nil || st.Sweep != nil {
		t.Errorf("health still shows a sweep: %+v", st)
	}
}

func TestSweepHealthShowsCountsOnlyAndTheLogsHoldNoPeople(t *testing.T) {
	e, people := sweepEnv(t, 600, func(c *engine.Config, s *fakeupstream.Server) { s.GALLatency = 3 * time.Millisecond })
	a := e.login()
	var st *engine.SweepStatus
	eventually(t, "running", func() bool {
		st = e.mgr.Status().Accounts[0].Contacts.Sweep
		return st != nil && st.State == "running" && st.PrefixesDone >= 5 && st.PrefixesQueued > 0 && st.EntriesFoundThisSweep > 0
	})
	if !st.Full || st.CalibratedLength != 1 || st.SearchesThisSweep < 5 || st.LastSweepUnix != 0 || st.LastSweepCapped {
		t.Errorf("running: %+v", st)
	}
	e.waitSweepFinished(a, 1)
	st = e.mgr.Status().Accounts[0].Contacts.Sweep
	if st.State != "waiting" || st.WaitingUntilUnix == 0 || st.LastSweepUnix == 0 || st.PrefixesQueued != 0 || st.EntriesFoundThisSweep != len(testgal.InDomain(people, sweepDomain)) {
		t.Errorf("finished: %+v", st)
	}
	e.waitLog("directory sweep finished")
	logs := e.logs.String()
	for _, p := range people[:40] {
		for _, bad := range []string{p.Email, p.Alias + "@", p.Display, p.Last, p.Phone} {
			if bad != "" && strings.Contains(logs, bad) {
				t.Fatalf("the log contains %q", bad)
			}
		}
	}
	for _, want := range []string{"directory sweep started", "directory sweep calibrated", "directory sweep finished"} {
		if !strings.Contains(logs, want) {
			t.Errorf("no log line %q", want)
		}
	}
	if strings.Contains(logs, "outside.example") || strings.Contains(logs, "عبد") {
		t.Errorf("the log has directory content")
	}
}
