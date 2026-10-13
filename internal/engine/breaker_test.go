//go:build devfake

package engine_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"easbridge/internal/engine"
	"easbridge/internal/fakeupstream"
	"easbridge/internal/store"
	"easbridge/internal/upstream"
)

// Regression tests for the runaway loops of the security review of v0.1.0: when
// the local database could not be written (or Exchange asked the client to slow
// down) the engine asked Exchange for the same thing again and again, 12,800
// identical requests in two seconds. Here the database is made unwritable on
// purpose, and the number of requests that reach the fake Exchange is bounded.

// exponential is a realistic back-off for tests that count requests: 50 ms
// doubling to 3 s (the harness default is a flat 15 ms).
func exponential(n int) time.Duration {
	d := 50 * time.Millisecond
	for i := 1; i < n && d < 3*time.Second; i++ {
		d *= 2
	}
	return d
}

// realBreaker gives the breaker the production timings (on the test clock).
func realBreaker(c *engine.Config, _ *fakeupstream.Server) {
	c.BreakerBase = 5 * time.Second
	c.BreakerMax = 15 * time.Minute
	c.BreakerThreshold = 3
}

// everyOp lists the operations of the fake Exchange that count as traffic.
var everyOp = []string{"FolderSync", "Sync", "FetchMIME", "ApplyChanges", "MoveItems", "CreateFolder",
	"RenameFolder", "DeleteFolder", "SendMail", "Ping"}

func totalCalls(e *env) int {
	n := 0
	for _, op := range everyOp {
		n += e.srv.Calls(op)
	}
	return n
}

func (e *env) acctStatus() engine.AccountStatus {
	e.t.Helper()
	st := e.mgr.Status()
	if len(st.Accounts) != 1 {
		e.t.Fatalf("want one running mailbox, got %d", len(st.Accounts))
	}
	return st.Accounts[0]
}

func failWrites(e *env) { e.db.SetWriteHook(func() error { return store.ErrInjected }) }

func TestThrottlingPausesAllTrafficUntilRetryAfter(t *testing.T) {
	e := newEnv(t, realBreaker)
	inbox := e.srv.FolderID(testUser, "Inbox")
	for i := 0; i < 3; i++ {
		d := e.clock.Now().Add(-time.Duration(3-i) * time.Hour)
		e.srv.AddMessage(testUser, inbox, mailBytes(fmt.Sprintf("t%d@x", i), "s", d, ""), fakeupstream.AddOptions{Date: d})
	}
	// Exchange answers the first Sync with 429 and asks for ten minutes.
	e.srv.FailNext("Sync", &upstream.ThrottleError{HTTP: 429, RetryAfter: 10 * time.Minute, Explicit: true}, 1)
	a := e.login()

	eventually(t, "the breaker opens", func() bool { return e.acctStatus().Paused })
	st := e.acctStatus()
	if left := time.Unix(st.PausedUntil, 0).Sub(e.clock.Now()); left < 9*time.Minute+30*time.Second || left > 10*time.Minute+time.Second {
		t.Errorf("the pause ends in %v, want the server's ten minutes", left)
	}
	if !strings.Contains(st.PauseReason, "429") || st.PauseTrips != 1 {
		t.Errorf("status: reason %q, trips %d", st.PauseReason, st.PauseTrips)
	}

	// Nothing leaves while it is open: no Sync, no Fetch, no Ping, no FolderSync.
	time.Sleep(60 * time.Millisecond) // calls that were already on their way
	before := totalCalls(e)
	consistently(t, "no request while Exchange is throttling", 500*time.Millisecond, func() bool { return totalCalls(e) == before })

	// Later requests made by clients are refused without a trip to Exchange.
	if _, err := a.Store().Folders(); err != nil {
		t.Fatal(err)
	}
	err := a.CreateFolder(context.Background(), "Archive/x")
	if err == nil || !errorsIsUnavailable(err) {
		t.Errorf("a client request during the pause: %v", err)
	}
	if totalCalls(e) != before {
		t.Error("the refused request reached Exchange")
	}

	// Ten minutes later everything resumes by itself.
	e.clock.Advance(10*time.Minute + 2*time.Second)
	e.mirrored(a, "Inbox", "INBOX")
	if st := e.acctStatus(); st.Paused {
		t.Errorf("still paused: %+v", st)
	}
}

func errorsIsUnavailable(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "paused") || strings.Contains(err.Error(), "unavailable"))
}

func TestARepeatedServerErrorOpensTheBreakerAndItEscalates(t *testing.T) {
	e := newEnv(t, realBreaker)
	inbox := e.srv.FolderID(testUser, "Inbox")
	d := e.clock.Now().Add(-time.Hour)
	e.srv.AddMessage(testUser, inbox, mailBytes("one@x", "s", d, ""), fakeupstream.AddOptions{Date: d})
	// HTTP 502 is not an explicit request to slow down: it takes three in a row.
	e.srv.FailNext("Sync", &upstream.ThrottleError{HTTP: 502}, -1)
	e.login()
	eventually(t, "the breaker opens", func() bool { return e.acctStatus().Paused })
	if n := e.srv.Calls("Sync"); n != 3 {
		t.Errorf("%d Syncs before the breaker opened, want 3", n)
	}
	st := e.acctStatus()
	if st.PausedUntil-e.clock.Now().Unix() > 6 || st.PauseTrips != 1 {
		t.Errorf("first pause: %+v", st)
	}
	// The pause ends, the next request fails again: the second pause is twice as long.
	e.clock.Advance(6 * time.Second)
	eventually(t, "the breaker opens again", func() bool { return e.acctStatus().PauseTrips == 2 })
	st = e.acctStatus()
	if left := st.PausedUntil - e.clock.Now().Unix(); left < 9 || left > 11 {
		t.Errorf("second pause lasts %d s, want about 10", left)
	}
	if n := e.srv.Calls("Sync"); n != 4 {
		t.Errorf("%d Syncs, want one probe after the first pause", n)
	}
}

func TestADatabaseThatCannotBeWrittenPausesExchangeTraffic(t *testing.T) {
	e := newEnv(t, realBreaker)
	inbox := e.srv.FolderID(testUser, "Inbox")
	d := e.clock.Now().Add(-2 * time.Hour)
	e.srv.AddMessage(testUser, inbox, mailBytes("first@x", "first", d, ""), fakeupstream.AddOptions{Date: d})
	a := e.login()
	f := e.mirrored(a, "Inbox", "INBOX")

	failWrites(e)
	d2 := e.clock.Now().Add(-time.Hour)
	e.srv.AddMessage(testUser, inbox, mailBytes("second@x", "second", d2, ""), fakeupstream.AddOptions{Date: d2})
	eventually(t, "the breaker opens", func() bool { return e.acctStatus().Paused })
	if r := e.acctStatus().PauseReason; !strings.Contains(r, "database") {
		t.Errorf("reason = %q", r)
	}
	time.Sleep(60 * time.Millisecond)
	before := totalCalls(e)
	consistently(t, "no request while the database is unwritable", 500*time.Millisecond, func() bool { return totalCalls(e) == before })
	if n := serverCount(a, f.ID); n != 1 {
		t.Fatalf("%d messages stored although nothing could be written", n)
	}

	// The disk is fine again; the pause ends; the message arrives.
	e.db.SetWriteHook(nil)
	e.clock.Advance(6 * time.Second)
	eventually(t, "the second message arrives", func() bool { return serverCount(a, f.ID) == 2 })
}

// layers runs a test with the circuit breaker and again without it (the failure
// threshold out of reach), so that the loop's own back-off is tested by itself.
func layers(t *testing.T, run func(t *testing.T, mods ...func(*engine.Config, *fakeupstream.Server))) {
	t.Run("with the breaker", func(t *testing.T) { run(t, realBreaker) })
	t.Run("loop back-off alone", func(t *testing.T) {
		run(t, realBreaker, func(c *engine.Config, _ *fakeupstream.Server) { c.BreakerThreshold = 1 << 30 })
	})
}

func TestOutboxDoesNotSpinWhenTheResultCannotBeStored(t *testing.T) {
	// Exchange takes the change but this side cannot record it: the same change
	// used to go out again at once, for ever (outboxRetryAt was never assigned).
	layers(t, func(t *testing.T, mods ...func(*engine.Config, *fakeupstream.Server)) {
		e := newEnv(t, append(mods, func(c *engine.Config, _ *fakeupstream.Server) { c.Backoff = exponential })...)
		inbox := e.srv.FolderID(testUser, "Inbox")
		for i := 0; i < 2; i++ {
			d := e.clock.Now().Add(-time.Duration(2-i) * time.Hour)
			e.srv.AddMessage(testUser, inbox, mailBytes(fmt.Sprintf("o%d@x", i), "s", d, ""), fakeupstream.AddOptions{Date: d})
		}
		a := e.login()
		f := e.mirrored(a, "Inbox", "INBOX")

		// Writes fail from the moment Exchange is asked to take the change: it
		// does, and the result cannot be recorded.
		e.db.SetWriteHook(func() error {
			if e.srv.Calls("ApplyChanges") >= 1 {
				return store.ErrInjected
			}
			return nil
		})
		if _, err := a.SetFlags(f.ID, []uint32{1}, engine.FlagAdd, store.FlagSeen, nil, "s"); err != nil {
			t.Fatal(err)
		}
		eventually(t, "Exchange accepts the change", func() bool {
			it, ok := serverItem(e, "Inbox", e.srv.Items(testUser, inbox)[0].ID)
			return ok && it.Read
		})
		start := e.srv.Calls("ApplyChanges")
		time.Sleep(1500 * time.Millisecond)
		if n := e.srv.Calls("ApplyChanges") - start; n > 8 {
			t.Fatalf("%d ApplyChanges in 1.5 s although the result cannot be stored: the outbox spins", n)
		}
		if n, _ := a.Store().OutboxLen(); n != 1 {
			t.Errorf("the change must stay queued while it cannot be recorded, outbox = %d", n)
		}

		// Once the database works the change completes.
		e.db.SetWriteHook(nil)
		e.clock.Advance(20 * time.Minute)
		eventually(t, "the outbox drains", func() bool { n, _ := a.Store().OutboxLen(); return n == 0 })
	})
}

func TestFetchDoesNotSpinWhenTheMessageCannotBeStored(t *testing.T) {
	// A message is downloaded but cannot be stored, and the failure cannot be
	// recorded either (BumpPending): it used to be due again at once.
	layers(t, func(t *testing.T, mods ...func(*engine.Config, *fakeupstream.Server)) {
		e := newEnv(t, append(mods, func(c *engine.Config, _ *fakeupstream.Server) { c.Backoff = exponential })...)
		inbox := e.srv.FolderID(testUser, "Inbox")
		d := e.clock.Now().Add(-time.Hour)
		e.srv.AddMessage(testUser, inbox, bigMail("big@x", "big", d), fakeupstream.AddOptions{Date: d})
		e.srv.HoldAfter("FetchMIME", testUser, inbox, 0) // the first download waits inside Exchange
		a := e.login()
		eventually(t, "a download is under way", func() bool { return e.srv.Calls("FetchMIME held") >= 1 })

		failWrites(e)
		e.srv.Release("FetchMIME", testUser, inbox)
		eventually(t, "the download completes", func() bool { return e.srv.Calls("FetchMIME") >= 1 })
		start := e.srv.Calls("FetchMIME")
		time.Sleep(1500 * time.Millisecond)
		if n := e.srv.Calls("FetchMIME") - start; n > 8 {
			t.Fatalf("%d downloads in 1.5 s although nothing can be stored: the fetch loop spins", n)
		}

		e.db.SetWriteHook(nil)
		e.clock.Advance(20 * time.Minute)
		f := e.waitFolder(a, "INBOX")
		eventually(t, "the message is stored once the database works", func() bool { return serverCount(a, f.ID) == 1 })
	})
}

func TestLate401ForAReplacedPasswordIsIgnored(t *testing.T) {
	// A request made with the old password is still on its way when a client logs
	// in with the new one. Its 401 says nothing about the new password; it used to
	// stop the engine and make the bridge forget the valid password.
	const newPass = "pw-two-correct-horse-battery-staple"
	e := newEnv(t)
	inbox := e.srv.FolderID(testUser, "Inbox")
	d := e.clock.Now().Add(-2 * time.Hour)
	e.srv.AddMessage(testUser, inbox, mailBytes("old@x", "old", d, ""), fakeupstream.AddOptions{Date: d})
	a := e.login()
	f := e.mirrored(a, "Inbox", "INBOX")

	e.srv.HoldAfter("Sync", testUser, inbox, 0)
	d2 := e.clock.Now().Add(-time.Hour)
	e.srv.AddMessage(testUser, inbox, mailBytes("new@x", "new", d2, ""), fakeupstream.AddOptions{Date: d2})
	eventually(t, "a Sync with the old password is under way", func() bool { return e.srv.Calls("Sync held") >= 1 })

	e.srv.SetPassword(testUser, newPass)
	b, err := e.mgr.Login(context.Background(), testUser, newPass)
	if err != nil || b != a {
		t.Fatalf("login with the new password: %v", err)
	}
	attempts := e.srv.AuthAttempts()
	e.srv.Release("Sync", testUser, inbox) // the old session's request is answered: 401

	eventually(t, "the late 401 is ignored and the mailbox carries on", func() bool { return serverCount(a, f.ID) == 2 })
	if a.AuthFailed() {
		t.Fatal("a 401 for the replaced password stopped the engine")
	}
	if !strings.Contains(e.logs.String(), "ignoring a late 401") {
		t.Errorf("the ignored 401 must be logged:\n%s", e.logs.String())
	}
	// The new password is still known to be good: no second verification.
	if _, err := e.mgr.Login(context.Background(), testUser, newPass); err != nil {
		t.Fatal(err)
	}
	if e.srv.AuthAttempts() != attempts {
		t.Error("the valid password was forgotten and had to be verified again")
	}
}

func TestAWipeOrderIsStillCarriedOutAfterTheBreakerCloses(t *testing.T) {
	// The breaker holds back requests, never the answer to a wipe order.
	e := newEnv(t, realBreaker)
	a := e.login()
	e.waitFolder(a, "INBOX")
	e.srv.FailNext("Sync", &upstream.ThrottleError{HTTP: 503, Explicit: true}, 1)
	e.srv.AddMessage(testUser, e.srv.FolderID(testUser, "Inbox"), mailBytes("x@x", "x", e.clock.Now().Add(-time.Hour), ""), fakeupstream.AddOptions{})
	eventually(t, "the breaker opens", func() bool { return e.acctStatus().Paused })
	e.srv.RequestWipe(testUser)
	// The wipe is noticed only when a request goes out, which the breaker prevents
	// until the pause ends. Then the order is carried out.
	e.clock.Advance(6 * time.Second)
	eventually(t, "the wipe is acknowledged", func() bool { return e.srv.WipeAcknowledged(testUser) })
}
