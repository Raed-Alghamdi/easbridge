//go:build devfake

package engine_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"easbridge/internal/engine"
	"easbridge/internal/upstream"
)

// Regression tests for the Active Directory lockout gaps found by the security
// review of v0.1.0: eight distinct wrong passwords made eight Exchange attempts,
// eight logins with one password while Exchange answered something other than a
// 401 (a gateway's 403, a redirect, a logon page, a timeout) made eight attempts,
// and a known-bad password was retried every 15 minutes for ever.

func isThrottled(err error) bool {
	var te *engine.ThrottledError
	return errors.As(err, &te) && errors.Is(err, engine.ErrUnavailable) && !errors.Is(err, engine.ErrBadCredentials)
}

func TestVerificationBudget(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var results []string
	for i := 0; i < 8; i++ {
		_, err := e.mgr.Login(ctx, testUser, fmt.Sprintf("guess-%d", i))
		switch {
		case errors.Is(err, engine.ErrBadCredentials):
			results = append(results, "bad")
		case isThrottled(err):
			results = append(results, "paused")
		default:
			results = append(results, fmt.Sprint(err))
		}
	}
	if want := "bad bad paused paused paused paused paused paused"; strings.Join(results, " ") != want {
		t.Fatalf("results = %v, want %s", results, want)
	}
	if n := e.srv.AuthAttempts(); n != 2 {
		t.Fatalf("eight distinct wrong passwords made %d Exchange attempts, want 2", n)
	}

	// Even the right password waits: it cannot be told from a guess.
	if _, err := e.mgr.Login(ctx, testUser, testPass); !isThrottled(err) {
		t.Fatalf("right password with the budget spent: %v", err)
	}
	if n := e.srv.AuthAttempts(); n != 2 {
		t.Fatalf("a refused login reached Exchange (%d attempts)", n)
	}

	// The window slides: after 30 minutes both attempts are out of it.
	e.clock.Advance(29 * time.Minute)
	if _, err := e.mgr.Login(ctx, testUser, testPass); !isThrottled(err) {
		t.Fatalf("29 minutes in: %v", err)
	}
	e.clock.Advance(2 * time.Minute)
	if _, err := e.mgr.Login(ctx, testUser, testPass); err != nil {
		t.Fatalf("after the window: %v", err)
	}
	if n := e.srv.AuthAttempts(); n != 3 {
		t.Fatalf("attempts = %d, want 3", n)
	}
	// A success clears the history: the next guess is an ordinary attempt that
	// reaches Exchange (and is rejected there), not a local refusal.
	if _, err := e.mgr.Login(ctx, testUser, "another-guess"); !errors.Is(err, engine.ErrBadCredentials) {
		t.Fatalf("guess after a success: %v", err)
	}
	if n := e.srv.AuthAttempts(); n != 4 {
		t.Fatalf("attempts = %d, want 4", n)
	}
}

func TestVerifiedCredentialIgnoresTheBudget(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.mgr.Login(ctx, testUser, testPass); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		e.mgr.Login(ctx, testUser, fmt.Sprintf("guess-%d", i))
	}
	if n := e.srv.AuthAttempts(); n != 3 { // the login, then two guesses
		t.Fatalf("attempts = %d, want 3", n)
	}
	// A guess spends the budget, but the credential Exchange accepted keeps its fast path.
	for i := 0; i < 10; i++ {
		if _, err := e.mgr.Login(ctx, testUser, testPass); err != nil {
			t.Fatalf("verified login %d with the budget spent: %v", i, err)
		}
	}
	if n := e.srv.AuthAttempts(); n != 3 {
		t.Fatalf("fast-path logins reached Exchange (%d)", n)
	}
}

func TestEveryKindOfFailedVerificationIsRemembered(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// Exchange (or a gateway) answers, but not with a 401: a 403, a redirect, an
	// HTML logon page. The password may be wrong, and the attempt may have counted.
	e.srv.FailNext("Open", fmt.Errorf("HTTP 403: %w", upstream.ErrAuthLike), -1)
	for i := 0; i < 8; i++ {
		_, err := e.mgr.Login(ctx, testUser, testPass)
		if i == 0 && !errors.Is(err, engine.ErrUnavailable) {
			t.Fatalf("first login: %v", err)
		}
		if i > 0 && !isThrottled(err) {
			t.Fatalf("login %d: %v, want a local refusal that is not a wrong-password answer", i, err)
		}
	}
	if n := e.srv.AuthAttempts(); n != 1 {
		t.Fatalf("eight logins with one password reached Exchange %d times, want 1", n)
	}
	// After the cooldown it is asked once more (and fails once more).
	e.clock.Advance(16 * time.Minute)
	e.mgr.Login(ctx, testUser, testPass)
	if n := e.srv.AuthAttempts(); n != 2 {
		t.Fatalf("attempts after the cooldown = %d, want 2", n)
	}
	// Once Exchange works, the next window of the (longer) cooldown ends in a success.
	e.srv.ClearFailures()
	e.clock.Advance(61 * time.Minute) // 1 h after the second failure
	if _, err := e.mgr.Login(ctx, testUser, testPass); err != nil {
		t.Fatalf("login after the escalated cooldown: %v", err)
	}
}

func TestNoConnectionCostsNeitherBudgetNorCooldown(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.srv.FailNext("Open", upstream.ErrNotSent, 6)
	for i := 0; i < 6; i++ {
		if _, err := e.mgr.Login(ctx, testUser, testPass); !errors.Is(err, engine.ErrUnavailable) || isThrottled(err) {
			t.Fatalf("login %d while Exchange cannot be reached: %v", i, err)
		}
	}
	if _, err := e.mgr.Login(ctx, testUser, testPass); err != nil {
		t.Fatalf("login once Exchange is reachable: %v", err)
	}
	if n := e.srv.AuthAttempts(); n != 7 {
		t.Fatalf("attempts = %d", n)
	}
}

func TestKnownBadPasswordBacksOffExponentially(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// Cooldown after the nth failure with the same password: 15 min, 1 h, 4 h, 24 h, 24 h.
	cooldowns := []time.Duration{15 * time.Minute, time.Hour, 4 * time.Hour, 24 * time.Hour, 24 * time.Hour}
	attempts := 0
	for n, cd := range cooldowns {
		if _, err := e.mgr.Login(ctx, testUser, "forgotten-old-password"); !errors.Is(err, engine.ErrBadCredentials) {
			t.Fatalf("failure %d: %v", n+1, err)
		}
		attempts++
		if got := e.srv.AuthAttempts(); got != attempts {
			t.Fatalf("failure %d: attempts = %d, want %d", n+1, got, attempts)
		}
		// Still refused locally just before the cooldown ends ...
		e.clock.Advance(cd - time.Minute)
		if _, err := e.mgr.Login(ctx, testUser, "forgotten-old-password"); !errors.Is(err, engine.ErrBadCredentials) || e.srv.AuthAttempts() != attempts {
			t.Fatalf("failure %d: %v, attempts %d: refused too late", n+1, err, e.srv.AuthAttempts())
		}
		// ... and tried again just after.
		e.clock.Advance(2 * time.Minute)
	}
}

func TestAnotherPasswordOrASuccessStartsTheEscalationOver(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	fail := func(pw string) {
		t.Helper()
		if _, err := e.mgr.Login(ctx, testUser, pw); !errors.Is(err, engine.ErrBadCredentials) {
			t.Fatalf("%s: %v", pw, err)
		}
	}
	fail("old-1") // level 0: 15 min
	e.clock.Advance(16 * time.Minute)
	fail("old-1")                     // level 1: 1 h
	e.clock.Advance(31 * time.Minute) // the first attempt left the window, the cooldown of old-1 (1 h) has not ended
	fail("old-2")                     // another password: back to 15 min
	e.clock.Advance(16 * time.Minute)
	before := e.srv.AuthAttempts()
	fail("old-2") // 15 min later it is tried again, not an hour later
	if e.srv.AuthAttempts() != before+1 {
		t.Fatal("a different password did not restart at the base cooldown")
	}
	// A success clears the history.
	e.clock.Advance(2 * time.Hour)
	if _, err := e.mgr.Login(ctx, testUser, testPass); err != nil {
		t.Fatal(err)
	}
	e.mgr.Close() // forget the engine so that the next login must verify again
}

func TestRefusalsAreLoggedWithoutThePassword(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		e.mgr.Login(ctx, testUser, fmt.Sprintf("secret-guess-%d", i))
	}
	for i := 0; i < 3; i++ {
		e.mgr.Login(ctx, testUser, "secret-guess-0") // a cooldown refusal
	}
	logs := e.logs.String()
	if n := strings.Count(logs, "login refused locally"); n != 4+3 {
		t.Errorf("%d refusal lines for 4 budget refusals and 3 cooldown refusals:\n%s", n, logs)
	}
	if strings.Contains(logs, "secret-guess") {
		t.Errorf("a password reached the log:\n%s", logs)
	}
}
