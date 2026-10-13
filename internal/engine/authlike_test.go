//go:build devfake

package engine_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"easbridge/internal/engine"
	"easbridge/internal/fakeupstream"
	"easbridge/internal/logx"
	"easbridge/internal/upstream"
)

// A gateway in front of Exchange answers a credential it does not accept with a 403, a redirect to a logon page
// that is not followed, or an HTML page. Those are not 401s, but every one can count as a failed logon in
// Active Directory, so during a session they are counted against the user's budget and two in a row stop the
// engine, as a 401 does at once. A 404 or a transport error stays retryable with the back-off.

var authLikeKinds = map[string]error{
	"403":        fmt.Errorf("HTTP 403: %w", upstream.ErrAuthLike),
	"redirect":   fmt.Errorf("HTTP 302: %w", upstream.ErrAuthLike),
	"logon page": fmt.Errorf("an HTML page answered an EAS request (a logon page?): %w", upstream.ErrAuthLike),
}

func exchangeCalls(e *env) int {
	return e.srv.Calls("Sync") + e.srv.Calls("Ping") + e.srv.Calls("FolderSync") + e.srv.Calls("FetchMIME")
}

func TestTwoRefusalLookalikesInARowStopTheEngineAndExchangeSeesNothingMore(t *testing.T) {
	for name, kind := range authLikeKinds {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			a := e.login()
			f := e.mirrored(a, "Inbox", "INBOX")
			inbox := e.srv.FolderID(testUser, "Inbox")

			base := exchangeCalls(e)
			for _, op := range []string{"Sync", "Ping", "FolderSync", "FetchMIME"} {
				e.srv.FailNext(op, kind, -1)
			}
			e.srv.AddMessage(testUser, inbox, mailBytes("al1@x", "after the gateway turned", e.clock.Now(), ""), fakeupstream.AddOptions{})
			eventually(t, "the engine stops like after a 401", func() bool { return a.AuthFailed() })
			stopped := exchangeCalls(e)
			if stopped-base > 2 {
				t.Errorf("Exchange saw %d refused requests before the engine stopped, the budget is 2", stopped-base)
			}
			consistently(t, "nothing more reaches Exchange", 300*time.Millisecond, func() bool { return exchangeCalls(e) == stopped })

			// The cached verification is gone and the budget is spent: the next login does not reach Exchange,
			// whatever password it carries.
			attempts := e.srv.AuthAttempts()
			for _, pw := range []string{testPass, "another-password"} {
				if _, err := e.mgr.Login(ctx, testUser, pw); !isThrottled(err) {
					t.Fatalf("login with %q after the stop: %v", pw, err)
				}
			}
			if e.srv.AuthAttempts() != attempts {
				t.Fatal("a refused login reached Exchange")
			}

			// Once the window and the cooldown are over, a login is verified again (one request), on the same engine.
			for _, op := range []string{"Sync", "Ping", "FolderSync", "FetchMIME"} {
				e.srv.FailNext(op, nil, 0)
			}
			e.clock.Advance(31 * time.Minute)
			b, err := e.mgr.Login(ctx, testUser, testPass)
			if err != nil || b != a {
				t.Fatalf("login after the cooldown: %v (same engine: %v)", err, b == a)
			}
			if e.srv.AuthAttempts() != attempts+1 {
				t.Errorf("attempts = %d, want %d", e.srv.AuthAttempts(), attempts+1)
			}
			eventually(t, "sync resumes", func() bool { return serverCount(a, f.ID) == 1 })
			if a.AuthFailed() {
				t.Error("still marked as failed")
			}
		})
	}
}

func TestAnIsolatedRefusalLookalikeIsForgottenOnceExchangeAnswersAgain(t *testing.T) {
	e := newEnv(t)
	a := e.login()
	f := e.mirrored(a, "Inbox", "INBOX")
	inbox := e.srv.FolderID(testUser, "Inbox")
	for i := 0; i < 3; i++ {
		e.srv.FailNext("Sync", authLikeKinds["403"], 1)
		e.srv.AddMessage(testUser, inbox, mailBytes(fmt.Sprintf("iso%d@x", i), "s", e.clock.Now(), ""), fakeupstream.AddOptions{})
		want := i + 1
		eventually(t, "the mailbox carries on after a single 403", func() bool { return serverCount(a, f.ID) == want })
		if a.AuthFailed() {
			t.Fatalf("round %d: a single 403 stopped the engine", i)
		}
	}
}

func TestA404OrATransportErrorIsRetriedForeverWithTheBackoff(t *testing.T) {
	for name, kind := range map[string]error{
		"404":      fmt.Errorf("HTTP 404: %w", upstream.ErrUnavailable),
		"time-out": fmt.Errorf("Sync: i/o timeout: %w", upstream.ErrTransient),
		"dropped":  fmt.Errorf("Sync: unexpected EOF: %w", upstream.ErrTransient),
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			a := e.login()
			f := e.mirrored(a, "Inbox", "INBOX")
			inbox := e.srv.FolderID(testUser, "Inbox")
			e.srv.FailNext("Sync", kind, -1)
			base := e.srv.Calls("Sync")
			e.srv.AddMessage(testUser, inbox, mailBytes("tr1@x", "s", e.clock.Now(), ""), fakeupstream.AddOptions{})
			eventually(t, "the engine keeps trying", func() bool { return e.srv.Calls("Sync") >= base+5 })
			if a.AuthFailed() {
				t.Fatal("a failure that is not a refusal stopped the engine")
			}
			e.srv.FailNext("Sync", nil, 0)
			eventually(t, "and recovers", func() bool { return serverCount(a, f.ID) == 1 })
		})
	}
}

// A login whose verification got no answer (429, a 5xx, a time-out) was not refused by anyone: it costs no budget
// and no escalation, only a short fixed pause. A 403 or a logon page still counts and escalates.
func TestThrottlingAndTimeoutsAtLoginCostOnlyAShortPause(t *testing.T) {
	for name, kind := range map[string]error{
		"429":      &upstream.ThrottleError{HTTP: 429, Explicit: true},
		"503":      &upstream.ThrottleError{HTTP: 503, Explicit: true},
		"500":      &upstream.ThrottleError{HTTP: 500},
		"time-out": fmt.Errorf("Open: i/o timeout: %w", upstream.ErrTransient),
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			// Many rounds: with the budget of 2 and the escalation of 15 min, 1 h, 4 h, 24 h a counting failure
			// could not be repeated like this.
			for i := 0; i < 6; i++ {
				e.srv.FailNext("Open", kind, 1)
				if _, err := e.mgr.Login(ctx, testUser, testPass); !errors.Is(err, engine.ErrUnavailable) || isThrottled(err) {
					t.Fatalf("round %d: %v, want Exchange's trouble reported as it is", i, err)
				}
				before := e.srv.AuthAttempts()
				// Within the pause the same credential is not sent again ...
				var te *engine.ThrottledError
				if _, err := e.mgr.Login(ctx, testUser, testPass); !errors.As(err, &te) {
					t.Fatalf("round %d: the retry was not held back: %v", i, err)
				}
				if until := te.Until.Sub(e.clock.Now()); until > time.Minute+time.Second {
					t.Fatalf("round %d: the pause is %v: fixed and short, no escalation", i, until)
				}
				if e.srv.AuthAttempts() != before {
					t.Fatalf("round %d: the held-back login reached Exchange", i)
				}
				// ... and after about a minute it is.
				e.clock.Advance(61 * time.Second)
			}
			if _, err := e.mgr.Login(ctx, testUser, testPass); err != nil {
				t.Fatalf("login once Exchange answers: %v", err)
			}
		})
	}
}

func TestA403OrALogonPageAtLoginStillCountsAndEscalates(t *testing.T) {
	for name, kind := range authLikeKinds {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			e.srv.FailNext("Open", kind, -1)
			if _, err := e.mgr.Login(ctx, testUser, testPass); !errors.Is(err, engine.ErrUnavailable) {
				t.Fatal(err)
			}
			var te *engine.ThrottledError
			_, err := e.mgr.Login(ctx, testUser, testPass)
			if !errors.As(err, &te) || te.Until.Sub(e.clock.Now()) < 14*time.Minute {
				t.Fatalf("the cooldown after a %s: %v", name, err)
			}
		})
	}
}

// --- names invented by a client (a domain entry lets any name of the domain through the allow-list) -----------

func (e *env) withAllowList(allowed func(string) bool, mod func(*engine.Config)) {
	e.t.Helper()
	e.mgr.Close()
	cfg := engine.Config{
		Grace: time.Hour, PingHeartbeat: 300 * time.Millisecond, PingMinHeartbeat: 40 * time.Millisecond,
		AuthFailCooldown: 15 * time.Minute, MaxMessageBytes: 1 << 20, PlaceholderTTL: 24 * time.Hour, PlaceholderWindow: 10 * time.Minute,
		ReconVerifyDelay: 200 * time.Millisecond, FetchWorkers: 2, OutboxBatch: 100, Housekeeping: 30 * time.Millisecond,
		Backoff: func(int) time.Duration { return 15 * time.Millisecond }, Clock: e.clock.Now, CallTimeout: 10 * time.Second,
		BreakerBase: time.Microsecond, BreakerMax: 4 * time.Microsecond,
	}
	if mod != nil {
		mod(&cfg)
	}
	log := logx.New(logx.Options{Level: slog.LevelDebug, Writer: e.logs})
	e.mgr = engine.NewManager(cfg, log, e.db, e.srv.Factory(4096), allowed)
}

func domainEntry(u string) bool { return strings.HasSuffix(u, "@example.test") }

func TestAClientThatInventsNamesLeavesNoRowsAndReachesExchangeNoMoreThanTenTimes(t *testing.T) {
	e := newEnv(t)
	e.withAllowList(domainEntry, func(c *engine.Config) { c.DomainEntries = true })
	ctx := engine.WithSource(context.Background(), "192.0.2.50")
	for i := 0; i < 300; i++ {
		_, err := e.mgr.Login(ctx, fmt.Sprintf("invented%03d@example.test", i), "some-password")
		if err == nil {
			t.Fatalf("name %d was let in", i)
		}
		if i >= 10 && !isThrottled(err) {
			t.Fatalf("name %d: %v, want a local refusal", i, err)
		}
	}
	if n := e.srv.AuthAttempts(); n != 10 {
		t.Errorf("Exchange saw %d verifications of invented names from one source, want 10", n)
	}
	users, err := e.db.AccountUsers()
	if err != nil || len(users) != 0 {
		t.Errorf("rows for names that were never verified: %v %v", users, err)
	}
	failed, bk, locks, sources := e.mgr.BookkeepingSizes()
	if failed > 10 || bk > 10 || locks != 0 || sources != 1 {
		t.Errorf("bookkeeping after 300 names: failed %d users %d locks %d sources %d", failed, bk, locks, sources)
	}

	// Another source has its own allowance; a source without an address shares one.
	ctx2 := engine.WithSource(context.Background(), "192.0.2.51")
	for i := 0; i < 20; i++ {
		e.mgr.Login(ctx2, fmt.Sprintf("other%02d@example.test", i), "pw")
	}
	if n := e.srv.AuthAttempts(); n != 20 {
		t.Errorf("a second source: %d verifications in all, want 20", n)
	}
	for i := 0; i < 20; i++ {
		e.mgr.Login(context.Background(), fmt.Sprintf("anon%02d@example.test", i), "pw")
	}
	if n := e.srv.AuthAttempts(); n != 30 {
		t.Errorf("logins without a source address: %d verifications in all, want 30", n)
	}

	// The window moves on: the names are forgotten and the source may try again, and a real login works.
	e.clock.Advance(11 * time.Minute)
	if _, err := e.mgr.Login(ctx, testUser, testPass); err != nil {
		t.Fatalf("a real login after the window: %v", err)
	}
	users, _ = e.db.AccountUsers()
	if len(users) != 1 || users[0] != testUser {
		t.Errorf("rows after one real login: %v", users)
	}
	// The cooldowns end and the bookkeeping goes with them.
	e.clock.Advance(25 * time.Hour)
	e.mgr.Login(ctx, "one-more@example.test", "pw")
	failed, bk, _, sources = e.mgr.BookkeepingSizes()
	if failed > 2 || bk > 2 || sources > 2 {
		t.Errorf("bookkeeping a day later: failed %d users %d sources %d", failed, bk, sources)
	}
}

func TestWithoutADomainEntryNoSourceLimitApplies(t *testing.T) {
	e := newEnv(t)
	e.withAllowList(func(u string) bool { return u == testUser }, nil)
	ctx := engine.WithSource(context.Background(), "192.0.2.50")
	for i := 0; i < 15; i++ {
		e.mgr.Login(ctx, testUser, fmt.Sprintf("guess-%d", i))
	}
	_, _, _, sources := e.mgr.BookkeepingSizes()
	if sources != 0 {
		t.Errorf("the source limiter ran without a domain entry (%d sources)", sources)
	}
}
