//go:build devfake

package engine_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	"easbridge/internal/engine"
	"easbridge/internal/fakeupstream"
	"easbridge/internal/logx"
	"easbridge/internal/store"
	"easbridge/internal/upstream"
	"easbridge/internal/vcard"
)

const (
	day  = 24 * time.Hour
	spac = 25 * time.Millisecond
)

func dirConfig(domains ...string) engine.DirectoryConfig {
	return engine.DirectoryConfig{Domains: domains, Spacing: spac, Poll: 5 * time.Millisecond, DailyCap: 100000,
		BackoffBase: 30 * time.Millisecond, BackoffMax: 120 * time.Millisecond, SeedBatch: 3}
}

// dirEnv is an environment with contacts and the directory on for the given domains.
// It fails the test if the fake GAL saw a query that looks like an enumeration.
func dirEnv(t *testing.T, domains []string, mods ...func(*engine.Config, *fakeupstream.Server)) *env {
	t.Helper()
	e := newEnv(t, withContacts(func(c *engine.Config, s *fakeupstream.Server) {
		c.Directory = dirConfig(domains...)
		for _, m := range mods {
			m(c, s)
		}
	}))
	t.Cleanup(func() {
		if v := e.srv.GALViolations(); len(v) > 0 {
			t.Errorf("the bridge sent searches that look like an enumeration of the directory: %q", v)
		}
	})
	return e
}

func gal(email, display, first, last string) fakeupstream.GALPerson {
	return fakeupstream.GALPerson{
		DirectoryEntry: upstream.DirectoryEntry{DisplayName: display, FirstName: first, LastName: last, EmailAddress: email,
			Title: "Engineer", Company: "Example Co", Office: "B2", Phone: "+1 555 011 0101", MobilePhone: "+1 555 050 0102"},
		Alias: strings.Split(email, "@")[0] + "-alias", HomePhone: "+1 555 055 0000",
	}
}

func (e *env) deliver(id, from, to, cc string, when time.Time) {
	e.t.Helper()
	h := "Message-ID: <" + id + ">\r\nFrom: " + from + "\r\nTo: " + to + "\r\n"
	if cc != "" {
		h += "Cc: " + cc + "\r\n"
	}
	raw := []byte(h + "Subject: s " + id + "\r\nDate: " + when.Format("Mon, 02 Jan 2006 15:04:05 -0700") + "\r\n\r\nbody\r\n")
	e.srv.AddMessage(testUser, e.srv.FolderID(testUser, "Inbox"), raw, fakeupstream.AddOptions{Date: when, Read: true})
}

func (e *env) candidate(a *engine.Account, email string) *store.Candidate {
	c, err := a.Store().Candidate(email)
	if err != nil {
		return nil
	}
	return c
}

func (e *env) waitState(a *engine.Account, email string, state int) *store.Candidate {
	e.t.Helper()
	var c *store.Candidate
	eventually(e.t, fmt.Sprintf("%s reaches state %d", email, state), func() bool {
		c = e.candidate(a, email)
		return c != nil && c.State == state
	})
	return c
}

func dirCards(t *testing.T, a *engine.Account) []store.Card {
	t.Helper()
	cs, err := a.Store().DirectoryCards()
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func countQueries(e *env, q string) int {
	n := 0
	for _, x := range e.srv.GALQueries() {
		if x == q {
			n++
		}
	}
	return n
}

func TestOnlyAnEntryWhoseAddressMatchesExactlyIsKept(t *testing.T) {
	e := dirEnv(t, []string{"corp.example"})
	e.srv.GALAddressSearch = false // the full address finds nothing, the local part finds several people
	for _, p := range []fakeupstream.GALPerson{
		gal("pat@corp.example", "Pat Smith", "Pat", "Smith"), gal("patricia@corp.example", "Patricia Jones", "Patricia", "Jones"),
		gal("pat.lee@corp.example", "Pat Lee", "Pat", "Lee"), gal("paterson@corp.example", "Paul Paterson", "Paul", "Paterson"),
		gal("pat@other.example", "Pat Elsewhere", "Pat", "Elsewhere"),
	} {
		e.srv.AddGALPerson(p)
	}
	e.deliver("m1", "Bob <bob@other.example>", "alice@example.test", "pat@corp.example", e.clock.Now().Add(-time.Hour))
	a := e.login()
	e.waitState(a, "pat@corp.example", store.CandFound)
	cs := dirCards(t, a)
	if len(cs) != 1 {
		t.Fatalf("%d directory cards; only the exact match may be kept", len(cs))
	}
	card := string(cs[0].VCard)
	for _, want := range []string{"FN:Pat Smith", "EMAIL;TYPE=WORK:pat@corp.example", "TITLE:Engineer", "ORG:Example Co", "NOTE:Office: B2",
		"TEL;TYPE=WORK;TYPE=VOICE:+1 555 011 0101", "TEL;TYPE=CELL:+1 555 050 0102", "UID:" + vcard.DirectoryUID("pat@corp.example")} {
		if !strings.Contains(card, want) {
			t.Errorf("the card lacks %q:\n%s", want, card)
		}
	}
	for _, never := range []string{"Patricia", "Jones", "Lee", "Paterson", "Elsewhere", "pat-alias", "000 0000"} {
		if strings.Contains(card, never) {
			t.Errorf("the card holds %q: another person, the alias or the home phone", never)
		}
	}
	if got := e.srv.GALQueries(); strings.Join(got, "|") != "pat@corp.example|pat" {
		t.Errorf("queries %q: the full address, then the local part (no display name in the header)", got)
	}
	if byEmail, err := a.Store().DirectoryCardByEmail("pat@corp.example"); err != nil || byEmail.UID != cs[0].UID {
		t.Errorf("%v", err)
	}
	// Nothing else about the other people is anywhere in the database.
	if cands, _ := a.Store().Candidates(); len(cands) != 1 {
		t.Errorf("candidates: %+v", cands)
	}
}

func TestTheFullAddressIsSearchedFirstAndOneSearchSuffices(t *testing.T) {
	e := dirEnv(t, []string{"corp.example"})
	e.srv.AddGALPerson(gal("pat@corp.example", "Pat Smith", "Pat", "Smith"))
	e.deliver("m1", "pat@corp.example", "alice@example.test", "", e.clock.Now().Add(-time.Hour))
	a := e.login()
	e.waitState(a, "pat@corp.example", store.CandFound)
	if got := e.srv.GALQueries(); strings.Join(got, "|") != "pat@corp.example" {
		t.Errorf("queries %q", got)
	}
}

func TestTheLadderHasAtMostThreeSearchesAndTheThirdUsesTheHeaderName(t *testing.T) {
	e := dirEnv(t, []string{"corp.example"})
	e.srv.GALAddressSearch, e.srv.GALNoLocalPartSearch = false, true
	js := gal("j.smith@corp.example", "Jonathan Smith", "Jonathan", "Smith")
	js.Alias = "jsmith"
	e.srv.AddGALPerson(js)
	e.deliver("m1", "Jonathan Smith <j.smith@corp.example>", "ghost@corp.example, Nobody Known <nobody@corp.example>", "", e.clock.Now().Add(-time.Hour))
	a := e.login()
	e.waitState(a, "j.smith@corp.example", store.CandFound)
	e.waitState(a, "ghost@corp.example", store.CandNotFound)
	e.waitState(a, "nobody@corp.example", store.CandNotFound)
	byCand := map[string][]string{}
	// The queries come in candidate order; split them by the full address that starts each ladder.
	cur := ""
	for _, q := range e.srv.GALQueries() {
		if strings.Contains(q, "@") {
			cur = q
		}
		byCand[cur] = append(byCand[cur], q)
	}
	if got := strings.Join(byCand["j.smith@corp.example"], "|"); got != "j.smith@corp.example|j.smith|Jonathan Smith" {
		t.Errorf("ladder of the found one: %q", got)
	}
	if got := strings.Join(byCand["ghost@corp.example"], "|"); got != "ghost@corp.example|ghost" {
		t.Errorf("a candidate without a header name gets two searches: %q", got)
	}
	if got := strings.Join(byCand["nobody@corp.example"], "|"); got != "nobody@corp.example|nobody|Nobody Known" {
		t.Errorf("ladder of a missing one: %q", got)
	}
	for who, qs := range byCand {
		if len(qs) > 3 {
			t.Errorf("%s: %d searches in one attempt", who, len(qs))
		}
	}
}

func TestNotFoundIsRememberedFor30DaysAndThenTriedAgain(t *testing.T) {
	e := dirEnv(t, []string{"corp.example"})
	a := e.login()
	a.Attach(func() {})
	e.deliver("m1", "ghost@corp.example", "alice@example.test", "", e.clock.Now().Add(-time.Hour))
	c := e.waitState(a, "ghost@corp.example", store.CandNotFound)
	if got := time.Unix(c.NextCheck, 0).Sub(time.Unix(c.CheckedAt, 0)); got != 30*day {
		t.Errorf("next check in %v, want 30 days", got)
	}
	n := e.srv.Calls("SearchDirectory")
	e.clock.Advance(29 * day)
	consistently(t, "no search inside the 30 days", 150*time.Millisecond, func() bool { return e.srv.Calls("SearchDirectory") == n })
	e.srv.AddGALPerson(gal("ghost@corp.example", "Gus Host", "Gus", "Host"))
	e.clock.Advance(2 * day)
	e.waitState(a, "ghost@corp.example", store.CandFound)
	if len(dirCards(t, a)) != 1 {
		t.Errorf("the person who has appeared since gets a card")
	}
}

func TestAFoundCardIsRefreshedAfterTheRefreshInterval(t *testing.T) {
	e := dirEnv(t, []string{"corp.example"})
	p := gal("pat@corp.example", "Pat Smith", "Pat", "Smith")
	e.srv.AddGALPerson(p)
	e.deliver("m1", "pat@corp.example", "alice@example.test", "", e.clock.Now().Add(-time.Hour))
	a := e.login()
	a.Attach(func() {})
	c := e.waitState(a, "pat@corp.example", store.CandFound)
	if got := time.Unix(c.NextCheck, 0).Sub(time.Unix(c.CheckedAt, 0)); got != 30*day {
		t.Errorf("a found card is refreshed after %v, want 30 days", got)
	}
	st, _ := a.Store().DirectoryState()
	ctag := st.CTag
	n := e.srv.Calls("SearchDirectory")
	e.clock.Advance(20 * day)
	consistently(t, "no refresh before its time", 120*time.Millisecond, func() bool { return e.srv.Calls("SearchDirectory") == n })
	e.srv.RemoveGALPerson("pat@corp.example")
	p.Title = "Director"
	e.srv.AddGALPerson(p)
	e.clock.Advance(11 * day)
	eventually(t, "the refreshed card", func() bool {
		cs := dirCards(t, a)
		return len(cs) == 1 && strings.Contains(string(cs[0].VCard), "TITLE:Director")
	})
	if st, _ := a.Store().DirectoryState(); st.CTag == ctag {
		t.Errorf("a changed card moves the ctag")
	}
}

func TestACardIsRemovedAfterTwoMissesAtLeastAWeekApart(t *testing.T) {
	e := dirEnv(t, []string{"corp.example"})
	e.srv.AddGALPerson(gal("pat@corp.example", "Pat Smith", "Pat", "Smith"))
	e.deliver("m1", "pat@corp.example", "alice@example.test", "", e.clock.Now().Add(-time.Hour))
	a := e.login()
	a.Attach(func() {})
	e.waitState(a, "pat@corp.example", store.CandFound)
	e.srv.RemoveGALPerson("pat@corp.example") // the person left

	e.clock.Advance(31 * day) // the refresh: miss number one
	eventually(t, "the first miss is recorded", func() bool {
		c := e.candidate(a, "pat@corp.example")
		return c != nil && c.MissFirst != 0
	})
	c := e.candidate(a, "pat@corp.example")
	if c.State != store.CandFound || len(dirCards(t, a)) != 1 {
		t.Fatalf("one miss does not remove the card: %+v", c)
	}
	if got := time.Unix(c.NextCheck, 0).Sub(time.Unix(c.MissFirst, 0)); got != 7*day {
		t.Errorf("the second try is %v after the first, want 7 days", got)
	}
	n := e.srv.Calls("SearchDirectory")
	e.clock.Advance(3 * day)
	consistently(t, "no second try before the week is over", 150*time.Millisecond, func() bool { return e.srv.Calls("SearchDirectory") == n })
	if len(dirCards(t, a)) != 1 {
		t.Fatal("the card was removed too early")
	}
	e.clock.Advance(5 * day)
	eventually(t, "the card is removed", func() bool { return len(dirCards(t, a)) == 0 })
	c = e.waitState(a, "pat@corp.example", store.CandNotFound)
	if c.MissFirst != 0 || time.Unix(c.NextCheck, 0).Sub(time.Unix(c.CheckedAt, 0)) != 30*day {
		t.Errorf("after the removal the candidate is in the negative cache: %+v", c)
	}
	if n := strings.Count(e.logs.String(), "a directory card was removed"); n != 1 {
		t.Errorf("%d lines about the removal", n)
	}
}

func TestAFoundAgainCardClearsTheFirstMiss(t *testing.T) {
	e := dirEnv(t, []string{"corp.example"})
	p := gal("pat@corp.example", "Pat Smith", "Pat", "Smith")
	e.srv.AddGALPerson(p)
	e.deliver("m1", "pat@corp.example", "alice@example.test", "", e.clock.Now().Add(-time.Hour))
	a := e.login()
	a.Attach(func() {})
	e.waitState(a, "pat@corp.example", store.CandFound)
	e.srv.RemoveGALPerson("pat@corp.example")
	e.clock.Advance(31 * day)
	eventually(t, "the first miss", func() bool { c := e.candidate(a, "pat@corp.example"); return c != nil && c.MissFirst != 0 })
	e.srv.AddGALPerson(p) // a temporary hiccup of the directory
	e.clock.Advance(8 * day)
	eventually(t, "found again", func() bool {
		c := e.candidate(a, "pat@corp.example")
		return c != nil && c.MissFirst == 0 && c.State == store.CandFound
	})
	if len(dirCards(t, a)) != 1 {
		t.Errorf("the card must stay")
	}
	// A new first miss starts the count again: one miss later the card is still there.
	e.srv.RemoveGALPerson("pat@corp.example")
	e.clock.Advance(31 * day)
	eventually(t, "a new first miss", func() bool { c := e.candidate(a, "pat@corp.example"); return c != nil && c.MissFirst != 0 })
	if len(dirCards(t, a)) != 1 {
		t.Errorf("a single miss after a find removes nothing")
	}
}

func TestSearchesAreOneAtATimeAndSpacedApart(t *testing.T) {
	const spacing = 80 * time.Millisecond
	e := dirEnv(t, []string{"corp.example"}, func(c *engine.Config, s *fakeupstream.Server) {
		c.Directory.Spacing = spacing
		s.GALLatency = 15 * time.Millisecond
	})
	for i := 0; i < 6; i++ {
		e.srv.AddGALPerson(gal(fmt.Sprintf("p%d@corp.example", i), fmt.Sprintf("Person %d", i), "P", fmt.Sprint(i)))
		e.deliver(fmt.Sprint("m", i), fmt.Sprintf("p%d@corp.example", i), "alice@example.test", "", e.clock.Now().Add(-time.Duration(i+1)*time.Hour))
	}
	a := e.login()
	eventually(t, "six cards", func() bool { return len(dirCards(t, a)) == 6 })
	ts := e.srv.GALSearchTimes()
	if len(ts) != 6 {
		t.Fatalf("%d searches", len(ts))
	}
	for i := 1; i < len(ts); i++ {
		if gap := ts[i].Sub(ts[i-1]); gap < spacing-5*time.Millisecond {
			t.Errorf("searches %d and %d are %v apart, want at least %v", i-1, i, gap, spacing)
		}
	}
	if e.srv.GALMaxInFlight() != 1 {
		t.Errorf("%d searches in flight at once", e.srv.GALMaxInFlight())
	}
}

func TestTheSpacingAlsoHoldsInsideALadderAndAcrossARestart(t *testing.T) {
	const spacing = 120 * time.Millisecond
	e := dirEnv(t, []string{"corp.example"}, func(c *engine.Config, s *fakeupstream.Server) {
		c.Directory.Spacing = spacing
		s.GALAddressSearch = false
	})
	e.deliver("m1", "Ghost One <ghost@corp.example>", "alice@example.test", "", e.clock.Now().Add(-time.Hour))
	a := e.login()
	e.waitState(a, "ghost@corp.example", store.CandNotFound)
	ts := e.srv.GALSearchTimes()
	if len(ts) != 3 {
		t.Fatalf("%d searches", len(ts))
	}
	for i := 1; i < len(ts); i++ {
		if gap := ts[i].Sub(ts[i-1]); gap < spacing-5*time.Millisecond {
			t.Errorf("ladder searches %d and %d are %v apart, want at least %v", i-1, i, gap, spacing)
		}
	}
	// The time of the last search is stored, so a restart cannot shorten the spacing.
	st, _ := a.Store().DirectoryState()
	if st.LastSearchMs == 0 || st.LookupsOn(e.clock.Now().UTC().Format("2006-01-02")) != 3 {
		t.Errorf("state %+v", st)
	}
}

func TestTheDailyCapStopsLookupsUntilTheNextDay(t *testing.T) {
	e := dirEnv(t, []string{"corp.example"}, func(c *engine.Config, _ *fakeupstream.Server) {
		c.Directory.DailyCap = 3
		c.Directory.Spacing = time.Millisecond
	})
	for i := 0; i < 6; i++ {
		e.srv.AddGALPerson(gal(fmt.Sprintf("p%d@corp.example", i), fmt.Sprintf("Person %d", i), "P", fmt.Sprint(i)))
		e.deliver(fmt.Sprint("m", i), fmt.Sprintf("p%d@corp.example", i), "alice@example.test", "", e.clock.Now().Add(-time.Duration(i+1)*time.Hour))
	}
	a := e.login()
	a.Attach(func() {})
	eventually(t, "three searches", func() bool { return e.srv.Calls("SearchDirectory") == 3 })
	consistently(t, "no fourth search today", 200*time.Millisecond, func() bool { return e.srv.Calls("SearchDirectory") == 3 })
	if cs := e.mgr.Status().Accounts[0].Contacts; cs == nil || cs.DirectoryLookupsToday != 3 || cs.DirectoryCards != 3 || cs.DirectoryQueue != 3 {
		t.Errorf("status %+v", cs)
	}
	e.clock.Advance(13 * time.Hour) // past midnight UTC
	eventually(t, "the next day continues", func() bool { return e.srv.Calls("SearchDirectory") == 6 })
	eventually(t, "six cards", func() bool { return len(dirCards(t, a)) == 6 })
}

func TestNoSearchWhileTheBreakerIsOpenAndRetryAfterIsHonoured(t *testing.T) {
	e := dirEnv(t, []string{"corp.example"}, func(c *engine.Config, _ *fakeupstream.Server) {
		c.BreakerBase, c.BreakerMax = 100*time.Millisecond, 2*time.Second
		c.Directory.Spacing = time.Millisecond
	})
	e.srv.AddGALPerson(gal("pat@corp.example", "Pat Smith", "Pat", "Smith"))
	e.deliver("m1", "pat@corp.example", "alice@example.test", "", e.clock.Now().Add(-time.Hour))
	e.srv.FailNext("SearchDirectory", &upstream.ThrottleError{HTTP: 503, Explicit: true, RetryAfter: 600 * time.Millisecond}, 1)
	a := e.login()
	eventually(t, "the throttled search", func() bool { return e.srv.Calls("SearchDirectory") == 1 })
	started := time.Now()
	eventually(t, "the breaker is open", func() bool { return e.mgr.Status().Accounts[0].Paused })
	consistently(t, "nothing is sent, and no search is counted, while the breaker is open", 400*time.Millisecond, func() bool {
		st, _ := a.Store().DirectoryState()
		return e.srv.Calls("SearchDirectory") == 1 && st.LookupsOn(e.clock.Now().UTC().Format("2006-01-02")) == 1
	})
	e.waitState(a, "pat@corp.example", store.CandFound)
	if since := time.Since(started); since < 450*time.Millisecond {
		t.Errorf("the next search came %v after the throttled one; Retry-After said 600 ms", since)
	}
	if len(dirCards(t, a)) != 1 {
		t.Errorf("the lookup is retried and succeeds")
	}
}

func TestAccessDeniedStopsTheDirectoryFor24HoursWithOneLogLine(t *testing.T) {
	for _, code := range []int{5, 13, 14} {
		t.Run(fmt.Sprint("status ", code), func(t *testing.T) {
			e := dirEnv(t, []string{"corp.example"})
			e.srv.AddGALPerson(gal("pat@corp.example", "Pat Smith", "Pat", "Smith"))
			e.deliver("m1", "pat@corp.example", "alice@example.test", "", e.clock.Now().Add(-time.Hour))
			e.srv.FailNext("SearchDirectory", &upstream.StatusError{Op: "Search", Code: code, Perm: true, Reason: "denied"}, 1)
			a := e.login()
			a.Attach(func() {})
			eventually(t, "the denied search", func() bool { return e.srv.Calls("SearchDirectory") == 1 })
			eventually(t, "the pause is stored", func() bool {
				st, _ := a.Store().DirectoryState()
				return st.PausedUntil != 0
			})
			st, _ := a.Store().DirectoryState()
			if got := time.Unix(st.PausedUntil, 0).Sub(e.clock.Now()); got < 23*time.Hour || got > 24*time.Hour+time.Minute {
				t.Errorf("paused for %v", got)
			}
			consistently(t, "no search during the pause", 250*time.Millisecond, func() bool { return e.srv.Calls("SearchDirectory") == 1 })
			if cs := e.mgr.Status().Accounts[0].Contacts; cs == nil || cs.DirectoryPausedUntil != st.PausedUntil {
				t.Errorf("health does not show the pause: %+v", cs)
			}
			if n := strings.Count(e.logs.String(), "directory lookups are stopped for 24 hours"); n != 1 {
				t.Errorf("%d log lines about the pause, want 1", n)
			}
			if e.mgr.Status().Accounts[0].Paused {
				t.Errorf("a refusal of the directory must not pause the mail")
			}
			e.clock.Advance(25 * time.Hour)
			e.waitState(a, "pat@corp.example", store.CandFound)
			if len(dirCards(t, a)) != 1 {
				t.Errorf("the lookup resumes after the pause")
			}
		})
	}
}

func TestSearchErrorsBackOffWithoutPausingTheMail(t *testing.T) {
	e := dirEnv(t, []string{"corp.example"}, func(c *engine.Config, _ *fakeupstream.Server) {
		c.BreakerBase, c.BreakerMax = 400*time.Millisecond, time.Second
		c.Directory.Spacing = time.Millisecond
		c.Directory.BackoffBase, c.Directory.BackoffMax = 60*time.Millisecond, 200*time.Millisecond
	})
	e.srv.AddGALPerson(gal("pat@corp.example", "Pat Smith", "Pat", "Smith"))
	e.deliver("m1", "pat@corp.example", "alice@example.test", "", e.clock.Now().Add(-time.Hour))
	// Search statuses 3 (server error), 10 (time-out) and 7 (connection failed), four in a row.
	e.srv.FailNext("SearchDirectory", &upstream.StatusError{Op: "Search", Code: 3, Reason: "error"}, 4)
	a := e.login()
	eventually(t, "the failures", func() bool { return e.srv.Calls("SearchDirectory") >= 2 })
	var times []time.Time
	eventually(t, "a card in the end", func() bool { return len(dirCards(t, a)) == 1 })
	times = e.srv.GALSearchTimes()
	if len(times) != 5 {
		t.Fatalf("%d searches", len(times))
	}
	// The wait grows: 60, 120, 200 (capped), 200 ms.
	for i, min := range []time.Duration{55, 115, 195, 195} {
		if gap := times[i+1].Sub(times[i]); gap < min*time.Millisecond {
			t.Errorf("gap %d is %v, want at least %v", i, gap, min*time.Millisecond)
		}
	}
	if st := e.mgr.Status().Accounts[0]; st.PauseTrips != 0 {
		t.Errorf("Search statuses must not trip the circuit breaker of the mailbox: %+v", st)
	}
}

func TestAPermanentRefusalOfOneQueryPostponesTheCandidate(t *testing.T) {
	e := dirEnv(t, []string{"corp.example"})
	e.srv.AddGALPerson(gal("ok@corp.example", "Ok Person", "Ok", "Person"))
	e.deliver("m1", "bad@corp.example", "alice@example.test", "", e.clock.Now().Add(-2*time.Hour))
	e.deliver("m2", "ok@corp.example", "alice@example.test", "", e.clock.Now().Add(-3*time.Hour))
	e.srv.FailNext("SearchDirectory", &upstream.StatusError{Op: "Search", Code: 8, Perm: true, Reason: "too complex"}, 1)
	a := e.login()
	e.waitState(a, "ok@corp.example", store.CandFound)
	c := e.candidate(a, "bad@corp.example")
	if c == nil || c.State != store.CandNew || time.Unix(c.NextCheck, 0).Sub(e.clock.Now()) < 23*time.Hour {
		t.Errorf("the refused candidate waits a day: %+v", c)
	}
}

func TestSeedingReadsTheStoredMailOnceAndNewMailAddsCandidates(t *testing.T) {
	e := dirEnv(t, []string{"corp.example"}, func(c *engine.Config, _ *fakeupstream.Server) {
		c.Directory = engine.DirectoryConfig{} // first without the directory: mail only
		c.ContactsEnabled = false
	})
	now := e.clock.Now()
	e.deliver("m1", "Pat <pat@corp.example>", "alice@example.test, other@elsewhere.example", "", now.Add(-5*time.Hour))
	e.deliver("m2", "pat@corp.example", "alice@example.test", "lee@corp.example", now.Add(-4*time.Hour))
	e.deliver("m3", "bob@elsewhere.example", "alice@example.test, lee@corp.example", "", now.Add(-3*time.Hour))
	e.deliver("m4", "alice@example.test", "pat@corp.example", "", now.Add(-2*time.Hour))
	e.deliver("m5", "alice@example.test", "late@corp.example", "", now.Add(-1*time.Hour))
	a := e.login()
	e.mirrored(a, "Inbox", "INBOX")
	if cs, _ := a.Store().Candidates(); len(cs) != 0 {
		t.Fatalf("the directory is off: %+v", cs)
	}
	// Now the directory is switched on over the same database.
	e.restart(func(c *engine.Config) {
		c.ContactsEnabled = true
		c.Directory = dirConfig("corp.example")
		c.Directory.Spacing = time.Hour // no lookups: this test is about the candidates
	})
	a = e.login()
	eventually(t, "seeded", func() bool {
		st, _ := a.Store().DirectoryState()
		return st.SeedDone
	})
	cands, _ := a.Store().Candidates()
	got := map[string]int{}
	for _, c := range cands {
		got[c.Email] = c.SeenCount
	}
	if len(got) != 3 || got["pat@corp.example"] != 3 || got["lee@corp.example"] != 2 || got["late@corp.example"] != 1 {
		t.Errorf("candidates %v: addresses in the domain only, counted once per message", got)
	}
	if c := e.candidate(a, "pat@corp.example"); c.Name != "Pat" || c.LastSeen != now.Add(-2*time.Hour).Unix() {
		t.Errorf("pat: %+v", c)
	}
	// A new message adds candidates, once.
	e.deliver("m6", "new@corp.example", "alice@example.test", "pat@corp.example", now)
	eventually(t, "new candidates", func() bool {
		n, p := e.candidate(a, "new@corp.example"), e.candidate(a, "pat@corp.example")
		return n != nil && p != nil && p.SeenCount == 4
	})
	if c := e.candidate(a, "new@corp.example"); c.SeenCount != 1 {
		t.Errorf("new: %+v", c)
	}

	// Another start does not seed again (no double counting).
	e.restart(func(c *engine.Config) {
		c.ContactsEnabled = true
		c.Directory = dirConfig("corp.example")
		c.Directory.Spacing = time.Hour
	})
	a = e.login()
	time.Sleep(150 * time.Millisecond)
	if c := e.candidate(a, "pat@corp.example"); c.SeenCount != 4 {
		t.Errorf("a second start seeded again: %+v", c)
	}
}

func TestChangingTheDomainsSeedsAgainWithExactCounts(t *testing.T) {
	e := dirEnv(t, []string{"corp.example"}, func(c *engine.Config, _ *fakeupstream.Server) { c.Directory.Spacing = time.Hour })
	now := e.clock.Now()
	e.deliver("m1", "pat@corp.example", "alice@example.test", "z@other.example", now.Add(-3*time.Hour))
	e.deliver("m2", "pat@corp.example", "alice@example.test", "z@other.example", now.Add(-2*time.Hour))
	a := e.login()
	eventually(t, "seeded", func() bool { st, _ := a.Store().DirectoryState(); return st.SeedDone })
	e.mirrored(a, "Inbox", "INBOX")
	e.restart(func(c *engine.Config) {
		c.Directory = dirConfig("corp.example", "other.example")
		c.Directory.Spacing = time.Hour
	})
	a = e.login()
	eventually(t, "seeded for the new list", func() bool {
		st, _ := a.Store().DirectoryState()
		return st.SeedDone && st.SeededFor == "corp.example,other.example"
	})
	if p, z := e.candidate(a, "pat@corp.example"), e.candidate(a, "z@other.example"); p == nil || z == nil || p.SeenCount != 2 || z.SeenCount != 2 {
		t.Errorf("counts after the second seeding: %+v %+v", p, z)
	}
}

func (e *env) restart(mod func(*engine.Config)) {
	e.t.Helper()
	e.mgr.Close()
	cfg := engine.Config{
		Grace: time.Hour, PingHeartbeat: 300 * time.Millisecond, PingMinHeartbeat: 40 * time.Millisecond,
		AuthFailCooldown: 15 * time.Minute, MaxMessageBytes: 1 << 20, PlaceholderTTL: 24 * time.Hour, PlaceholderWindow: 10 * time.Minute,
		ReconVerifyDelay: 200 * time.Millisecond, FetchWorkers: 2, OutboxBatch: 100, Housekeeping: 30 * time.Millisecond,
		Backoff: func(int) time.Duration { return 15 * time.Millisecond }, Clock: e.clock.Now, CallTimeout: 10 * time.Second,
		BreakerBase: time.Microsecond, BreakerMax: 4 * time.Microsecond,
		ContactsEnabled: true, ContactsInterval: 60 * time.Millisecond,
	}
	mod(&cfg)
	e.srv.Window = 3
	log := logx.New(logx.Options{Level: slog.LevelDebug, Writer: e.logs})
	e.mgr = engine.NewManager(cfg, log, e.db, e.srv.Factory(4096), func(u string) bool { return u == testUser })
}

func TestOnlyDirectoryDomainsAreLookedUpAndNeverTheOwnAddress(t *testing.T) {
	e := dirEnv(t, []string{"example.test", "corp.example"}) // the account's own domain is a directory domain
	e.srv.AddGALPerson(gal("pat@corp.example", "Pat Smith", "Pat", "Smith"))
	e.srv.AddGALPerson(gal(testUser, "Alice Own", "Alice", "Own"))
	e.srv.AddGALPerson(gal("zed@other.example", "Zed Other", "Zed", "Other"))
	e.deliver("m1", testUser, testUser+", zed@other.example", "pat@corp.example", e.clock.Now().Add(-time.Hour))
	a := e.login()
	eventually(t, "done", func() bool { return len(dirCards(t, a)) == 1 })
	consistently(t, "no more searches", 150*time.Millisecond, func() bool { return e.srv.Calls("SearchDirectory") == 1 })
	if got := e.srv.GALQueries(); strings.Join(got, "|") != "pat@corp.example" {
		t.Errorf("queries %q: the other person in the domain, not the account's own address and not another domain", got)
	}
}

func TestTheMostRecentCandidatesAreLookedUpFirstAndTheMostFrequentAmongEquals(t *testing.T) {
	e := dirEnv(t, []string{"corp.example"})
	now := e.clock.Now()
	e.deliver("m1", "old@corp.example", "alice@example.test", "", now.Add(-72*time.Hour))
	e.deliver("m2", "old@corp.example", "alice@example.test", "", now.Add(-71*time.Hour))
	e.deliver("m3", "once@corp.example", "alice@example.test", "", now.Add(-1*time.Hour))
	e.deliver("m4", "twice@corp.example", "alice@example.test", "", now.Add(-1*time.Hour))
	e.deliver("m5", "twice@corp.example", "alice@example.test", "", now.Add(-2*time.Hour))
	e.deliver("m6", "mid@corp.example", "alice@example.test", "", now.Add(-30*time.Hour))
	a := e.login()
	eventually(t, "all looked up", func() bool { return e.srv.Calls("SearchDirectory") >= 4 })
	e.waitState(a, "old@corp.example", store.CandNotFound)
	var fulls []string
	for _, q := range e.srv.GALQueries() {
		if strings.Contains(q, "@") {
			fulls = append(fulls, q)
		}
	}
	if got := strings.Join(fulls, ","); got != "twice@corp.example,once@corp.example,mid@corp.example,old@corp.example" {
		t.Errorf("order %s", got)
	}
}

func TestNothingAboutThePeopleOfTheDirectoryReachesTheLog(t *testing.T) {
	e := dirEnv(t, []string{"corp.example"})
	e.srv.AddGALPerson(gal("noorhan.alsulami@corp.example", "Noorhan Alsulami", "Noorhan", "Alsulami"))
	e.srv.AddGALPerson(gal("abdullah@corp.example", "عبدالله القحطاني", "عبدالله", "القحطاني"))
	e.deliver("m1", "Noorhan Alsulami <noorhan.alsulami@corp.example>", "عبدالله القحطاني <abdullah@corp.example>, Unknown Person <unknown.person@corp.example>", "", e.clock.Now().Add(-time.Hour))
	e.srv.FailNext("SearchDirectory", &upstream.StatusError{Op: "Search", Code: 3, Reason: "error"}, 1)
	a := e.login()
	eventually(t, "two cards", func() bool { return len(dirCards(t, a)) == 2 })
	e.waitState(a, "unknown.person@corp.example", store.CandNotFound)
	// And the paths of the refusals.
	e.srv.FailNext("SearchDirectory", &upstream.StatusError{Op: "Search", Code: 5, Perm: true, Reason: "denied"}, 1)
	a.Attach(func() {})
	e.clock.Advance(40 * day)
	eventually(t, "the denial", func() bool { return strings.Contains(e.logs.String(), "stopped for 24 hours") })
	logs := e.logs.String()
	for _, secret := range []string{"noorhan", "Noorhan", "Alsulami", "alsulami", "abdullah", "عبدالله", "القحطاني", "unknown.person", "Unknown Person", "corp.example",
		"+1 555", "BEGIN:VCARD", "Engineer", "B2"} {
		if contains(logs, secret) {
			t.Errorf("the log contains %q", secret)
		}
	}
}

func TestDirectoryIsOffWithoutDomainsOrWithoutContacts(t *testing.T) {
	for name, mod := range map[string]func(*engine.Config, *fakeupstream.Server){
		"no domains":   func(c *engine.Config, _ *fakeupstream.Server) { c.Directory = engine.DirectoryConfig{} },
		"contacts off": func(c *engine.Config, _ *fakeupstream.Server) { c.ContactsEnabled = false },
	} {
		t.Run(name, func(t *testing.T) {
			e := dirEnv(t, []string{"corp.example"}, mod)
			e.srv.AddGALPerson(gal("pat@corp.example", "Pat Smith", "Pat", "Smith"))
			e.deliver("m1", "pat@corp.example", "alice@example.test", "", e.clock.Now().Add(-time.Hour))
			a := e.login()
			e.mirrored(a, "Inbox", "INBOX")
			consistently(t, "no lookups, no candidates", 200*time.Millisecond, func() bool {
				cs, _ := a.Store().Candidates()
				return e.srv.Calls("SearchDirectory") == 0 && len(cs) == 0
			})
			if st := e.mgr.Status().Accounts[0].Contacts; st != nil && st.DirectoryEnabled {
				t.Errorf("health claims the directory is on: %+v", st)
			}
		})
	}
}

func TestLookupsStopWhenTheEngineStopsAndWithoutCredentials(t *testing.T) {
	e := dirEnv(t, []string{"corp.example"}, func(c *engine.Config, _ *fakeupstream.Server) { c.Directory.Spacing = 40 * time.Millisecond })
	for i := 0; i < 20; i++ {
		e.deliver(fmt.Sprint("m", i), fmt.Sprintf("p%d@corp.example", i), "alice@example.test", "", e.clock.Now().Add(-time.Duration(i+1)*time.Hour))
	}
	a := e.login()
	eventually(t, "searching", func() bool { return e.srv.Calls("SearchDirectory") >= 2 })
	// A new password at Exchange: the next call is rejected with 401, all traffic stops.
	e.srv.SetPassword(testUser, "changed")
	eventually(t, "auth failed", func() bool { return a.AuthFailed() })
	time.Sleep(100 * time.Millisecond)
	n := e.srv.Calls("SearchDirectory")
	consistently(t, "no search without working credentials", 250*time.Millisecond, func() bool { return e.srv.Calls("SearchDirectory") <= n+1 })
	e.mgr.Close()
	n = e.srv.Calls("SearchDirectory")
	time.Sleep(150 * time.Millisecond)
	if e.srv.Calls("SearchDirectory") != n {
		t.Errorf("the lookups go on after the engine stopped")
	}
}

var _ = []any{context.Background, errors.New, sort.Strings}

func TestRetryAfterOfAServerErrorIsHonouredByTheDirectoryItself(t *testing.T) {
	// An HTTP 500 is not an explicit request to slow down, so the breaker does not open after one; the
	// Retry-After it carries still sets the earliest time of the next search.
	e := dirEnv(t, []string{"corp.example"}, func(c *engine.Config, _ *fakeupstream.Server) {
		c.Directory.Spacing = time.Millisecond
	})
	e.srv.AddGALPerson(gal("pat@corp.example", "Pat Smith", "Pat", "Smith"))
	e.deliver("m1", "pat@corp.example", "alice@example.test", "", e.clock.Now().Add(-time.Hour))
	e.srv.FailNext("SearchDirectory", &upstream.ThrottleError{HTTP: 500, RetryAfter: 400 * time.Millisecond}, 1)
	a := e.login()
	e.waitState(a, "pat@corp.example", store.CandFound)
	ts := e.srv.GALSearchTimes()
	if len(ts) != 2 || ts[1].Sub(ts[0]) < 380*time.Millisecond {
		t.Errorf("searches %v: the second must wait for the Retry-After of the first answer (the back-off alone is 30 ms)", ts)
	}
}

func TestTwoMissesLessThanAWeekApartDoNotRemoveTheCard(t *testing.T) {
	e := dirEnv(t, []string{"corp.example"})
	e.srv.AddGALPerson(gal("pat@corp.example", "Pat Smith", "Pat", "Smith"))
	e.deliver("m1", "pat@corp.example", "alice@example.test", "", e.clock.Now().Add(-time.Hour))
	a := e.login()
	a.Attach(func() {})
	e.waitState(a, "pat@corp.example", store.CandFound)
	e.srv.RemoveGALPerson("pat@corp.example")
	e.clock.Advance(31 * day)
	eventually(t, "the first miss", func() bool { c := e.candidate(a, "pat@corp.example"); return c != nil && c.MissFirst != 0 })
	first := e.candidate(a, "pat@corp.example")
	// Something makes the candidate due again early (here: a hand-made schedule); a miss one day after the first is not enough.
	a.Store().Update(nil, func(tx *store.Tx) error {
		return tx.UpdateCandidate("pat@corp.example", store.CandidateUpdate{State: store.CandFound, CheckedAt: e.clock.Now(), NextCheck: e.clock.Now().Add(day),
			MissFirst: time.Unix(first.MissFirst, 0)})
	})
	e.clock.Advance(day + time.Hour)
	eventually(t, "the second search", func() bool { return e.srv.Calls("SearchDirectory") >= 3 })
	time.Sleep(100 * time.Millisecond)
	c := e.candidate(a, "pat@corp.example")
	if len(dirCards(t, a)) != 1 || c.State != store.CandFound || c.MissFirst != first.MissFirst {
		t.Errorf("a second miss one day after the first removed or changed something: %+v", c)
	}
	if got := time.Unix(c.NextCheck, 0).Sub(time.Unix(first.MissFirst, 0)); got != 7*day {
		t.Errorf("the next try is %v after the first miss, want 7 days", got)
	}
}

func TestMailThatArrivesWhileTheSeedingRunsIsCountedOnce(t *testing.T) {
	e := dirEnv(t, []string{"corp.example"}, func(c *engine.Config, _ *fakeupstream.Server) {
		c.Directory = engine.DirectoryConfig{} // first without the directory
		c.ContactsEnabled = false
	})
	now := e.clock.Now()
	for i := 0; i < 40; i++ {
		e.deliver(fmt.Sprint("old", i), fmt.Sprintf("old%d@corp.example", i), "alice@example.test", "", now.Add(-time.Duration(100+i)*time.Hour))
	}
	a := e.login()
	e.mirrored(a, "Inbox", "INBOX")
	e.mgr.Close()
	// While the bridge is down, new mail arrives at Exchange; the seeding (one message per transaction) and the sync of that
	// mail run side by side when the bridge starts with the directory on.
	for i := 0; i < 30; i++ {
		e.deliver(fmt.Sprint("new", i), fmt.Sprintf("new%d@corp.example", i), "alice@example.test", "", now.Add(-time.Duration(i)*time.Minute))
	}
	e.restart(func(c *engine.Config) {
		c.ContactsEnabled = true
		c.Directory = dirConfig("corp.example")
		c.Directory.SeedBatch = 1
		c.Directory.Spacing = time.Hour
	})
	a = e.login()
	eventually(t, "seeded", func() bool { st, _ := a.Store().DirectoryState(); return st.SeedDone })
	eventually(t, "the new mail is stored", func() bool {
		return len(e.srv.Items(testUser, e.srv.FolderID(testUser, "Inbox"))) == 70 && e.candidate(a, "new29@corp.example") != nil
	})
	time.Sleep(200 * time.Millisecond)
	cands, _ := a.Store().Candidates()
	if len(cands) != 70 {
		t.Fatalf("%d candidates", len(cands))
	}
	for _, c := range cands {
		if c.SeenCount != 1 {
			t.Errorf("%s was counted %d times", c.Email, c.SeenCount)
		}
	}
}

func TestACandidateOfADomainThatIsNotConfiguredIsNotLookedUp(t *testing.T) {
	// A candidate can be in the table although its domain is not configured (the list was changed, the settings of an
	// earlier run): only the configured domains are searched.
	e := dirEnv(t, []string{"corp.example"})
	e.deliver("m1", "recent@corp.example", "alice@example.test", "", e.clock.Now().Add(-time.Hour))
	a := e.login()
	e.waitState(a, "recent@corp.example", store.CandNotFound)
	if _, err := a.Store().Update(nil, func(tx *store.Tx) error {
		return tx.NoteCandidate("older@other.example", "Older Person", e.clock.Now().Add(-time.Minute))
	}); err != nil {
		t.Fatal(err)
	}
	n := e.srv.Calls("SearchDirectory")
	consistently(t, "the candidate of the other domain is left alone", 300*time.Millisecond, func() bool { return e.srv.Calls("SearchDirectory") == n })
	for _, q := range e.srv.GALQueries() {
		if strings.Contains(q, "other") || strings.Contains(q, "older") || strings.Contains(q, "Older") {
			t.Errorf("searched for %q", q)
		}
	}
}

func TestTheDirectoryCountsNoSearchWhileTheBreakerIsOpenBecauseOfOtherTraffic(t *testing.T) {
	e := dirEnv(t, []string{"corp.example"}, func(c *engine.Config, _ *fakeupstream.Server) {
		c.BreakerBase, c.BreakerMax = 100*time.Millisecond, 3*time.Second
		c.Directory.Spacing = 400 * time.Millisecond
	})
	e.srv.AddGALPerson(gal("pat@corp.example", "Pat Smith", "Pat", "Smith"))
	e.srv.AddGALPerson(gal("lee@corp.example", "Lee Wong", "Lee", "Wong"))
	e.deliver("m1", "pat@corp.example", "alice@example.test", "", e.clock.Now().Add(-time.Hour))
	e.deliver("m2", "lee@corp.example", "alice@example.test", "", e.clock.Now().Add(-2*time.Hour))
	a := e.login()
	e.waitState(a, "pat@corp.example", store.CandFound)
	// Mail traffic is throttled: the breaker opens for 1.2 s, while the next search is due in 400 ms.
	e.srv.FailNext("Sync", &upstream.ThrottleError{HTTP: 503, Explicit: true, RetryAfter: 1200 * time.Millisecond}, 1)
	e.deliver("m3", "bob@elsewhere.example", "alice@example.test", "", e.clock.Now())
	eventually(t, "the breaker is open", func() bool { return e.mgr.Status().Accounts[0].Paused })
	time.Sleep(500 * time.Millisecond) // the search was due meanwhile
	if !e.mgr.Status().Accounts[0].Paused {
		t.Fatal("the breaker closed too early for this test")
	}
	consistently(t, "no search is sent or counted while the breaker is open", 300*time.Millisecond, func() bool {
		st, _ := a.Store().DirectoryState()
		return e.srv.Calls("SearchDirectory") == 1 && st.LookupsOn(e.clock.Now().UTC().Format("2006-01-02")) == 1
	})
	e.waitState(a, "lee@corp.example", store.CandFound)
}
