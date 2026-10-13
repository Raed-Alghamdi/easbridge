//go:build devfake

package engine_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"easbridge/internal/engine"
	"easbridge/internal/fakeupstream"
	"easbridge/internal/upstream"
)

func TestLoginRejectsUsersOutsideTheAllowListWithoutContactingExchange(t *testing.T) {
	e := newEnv(t)
	e.srv.AddUser("mallory@example.test", "whatever-password")
	_, err := e.mgr.Login(context.Background(), "mallory@example.test", "whatever-password")
	if !errors.Is(err, engine.ErrBadCredentials) {
		t.Fatalf("err = %v", err)
	}
	if _, err := e.mgr.Login(context.Background(), "", ""); !errors.Is(err, engine.ErrBadCredentials) {
		t.Fatalf("empty credentials: %v", err)
	}
	if n := e.srv.AuthAttempts(); n != 0 {
		t.Fatalf("Exchange was contacted %d times for a user outside the allow-list", n)
	}
}

func TestLoginWrongPasswordIsRememberedAndNotRetried(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := e.mgr.Login(ctx, testUser, "wrong-password-1"); !errors.Is(err, engine.ErrBadCredentials) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if n := e.srv.AuthAttempts(); n != 1 {
		t.Fatalf("a wrong password reached Exchange %d times, want exactly 1 (lockout protection)", n)
	}
	// A different wrong password is a new credential: one more attempt.
	if _, err := e.mgr.Login(ctx, testUser, "wrong-password-2"); !errors.Is(err, engine.ErrBadCredentials) {
		t.Fatal(err)
	}
	if n := e.srv.AuthAttempts(); n != 2 {
		t.Fatalf("attempts = %d, want 2", n)
	}
	// Two unsuccessful verifications are the budget of the window: even the right
	// password is refused locally (as unavailable, not as a wrong password) until
	// the window has moved on. See TestVerificationBudget for the details.
	if _, err := e.mgr.Login(ctx, testUser, testPass); !errors.Is(err, engine.ErrUnavailable) || errors.Is(err, engine.ErrBadCredentials) {
		t.Fatalf("right password with the budget spent: %v", err)
	}
	if n := e.srv.AuthAttempts(); n != 2 {
		t.Fatalf("attempts = %d, want 2", n)
	}
	e.clock.Advance(31 * time.Minute)
	if _, err := e.mgr.Login(ctx, testUser, testPass); err != nil {
		t.Fatal(err)
	}
	// After the cooldown the wrong password is tried again.
	before := e.srv.AuthAttempts()
	if _, err := e.mgr.Login(ctx, testUser, "wrong-password-1"); !errors.Is(err, engine.ErrBadCredentials) {
		t.Fatal(err)
	}
	if e.srv.AuthAttempts() != before+1 {
		t.Fatalf("cooldown did not expire")
	}
}

func TestLoginFastPathAndSingleVerification(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	const n = 25
	accts := make([]*engine.Account, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a, err := e.mgr.Login(ctx, testUser, testPass)
			if err != nil {
				t.Errorf("login %d: %v", i, err)
				return
			}
			accts[i] = a
		}(i)
	}
	wg.Wait()
	if got := e.srv.AuthAttempts(); got != 1 {
		t.Fatalf("%d concurrent logins caused %d verifications, want 1", n, got)
	}
	for i := 1; i < n; i++ {
		if accts[i] != accts[0] {
			t.Fatalf("logins returned different engines")
		}
	}
	// Later logins (MailFlow's STATUS cycle) never touch Exchange.
	for i := 0; i < 50; i++ {
		if _, err := e.mgr.Login(ctx, testUser, testPass); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.srv.AuthAttempts(); got != 1 {
		t.Fatalf("fast-path logins reached Exchange (%d)", got)
	}
	// User names are case-insensitive.
	if _, err := e.mgr.Login(ctx, "ALICE@example.test", testPass); err != nil {
		t.Fatal(err)
	}
	if got := e.srv.AuthAttempts(); got != 1 {
		t.Fatalf("case-variant login reached Exchange (%d)", got)
	}
}

func TestLoginWhileExchangeIsDownIsNotABadPassword(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// Nothing was sent (no connection): the credential was not presented to
	// anyone, so the failure is neither remembered nor counted.
	e.srv.FailNext("Open", upstream.ErrNotSent, 1)
	if _, err := e.mgr.Login(ctx, testUser, testPass); !errors.Is(err, engine.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	// Not cached as a failure: the next try verifies again and succeeds.
	if _, err := e.mgr.Login(ctx, testUser, testPass); err != nil {
		t.Fatalf("second login: %v", err)
	}
	if e.srv.AuthAttempts() != 2 {
		t.Fatalf("attempts = %d", e.srv.AuthAttempts())
	}
}

func TestPasswordChangeSwapsCredentialsAndOldOneStopsWorking(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.login()
	e.mirrored(a, "Inbox", "INBOX")

	e.srv.SetPassword(testUser, "pw-two-new-password")
	// The old password still sits in the fast-path cache, but the engine's own
	// traffic will notice the 401 as soon as it talks to Exchange. Poke it.
	e.srv.AddMessage(testUser, e.srv.FolderID(testUser, "Inbox"), mailBytes("p1@x", "after change", e.clock.Now(), ""), fakeupstream.AddOptions{})
	eventually(t, "engine notices the 401", func() bool { return a.AuthFailed() })
	callsAfter := e.srv.Calls("Sync") + e.srv.Calls("Ping") + e.srv.Calls("FolderSync")
	time.Sleep(150 * time.Millisecond)
	if now := e.srv.Calls("Sync") + e.srv.Calls("Ping") + e.srv.Calls("FolderSync"); now != callsAfter {
		t.Fatalf("engine kept talking to Exchange after a 401 (%d -> %d calls)", callsAfter, now)
	}
	// The rejected credential is refused locally from now on.
	before := e.srv.AuthAttempts()
	if _, err := e.mgr.Login(ctx, testUser, testPass); !errors.Is(err, engine.ErrBadCredentials) {
		t.Fatalf("old password: %v", err)
	}
	if e.srv.AuthAttempts() != before {
		t.Fatalf("the rejected password was sent to Exchange again")
	}
	// The new password verifies once, resumes sync on the same engine.
	b, err := e.mgr.Login(ctx, testUser, "pw-two-new-password")
	if err != nil {
		t.Fatal(err)
	}
	if b != a {
		t.Fatalf("a new engine was created instead of renewing the credentials")
	}
	eventually(t, "sync resumes with the new password", func() bool {
		f, _ := a.Store().FolderByPath("INBOX")
		return f != nil && serverCount(a, f.ID) == 1
	})
	if a.AuthFailed() {
		t.Fatal("still marked as auth-failed")
	}
}

func TestEngineStopsAfterGraceAndRestartsOnLogin(t *testing.T) {
	e := newEnv(t, func(c *engine.Config, _ *fakeupstream.Server) { c.Grace = 60 * time.Millisecond })
	a := e.login()
	detach := a.Attach(func() {})
	e.clock.Advance(time.Hour)
	consistently(t, "engine survives while a session is attached", 150*time.Millisecond, func() bool { return !a.Stopped() })
	detach()
	e.clock.Advance(time.Hour)
	eventually(t, "engine stops after the grace period", func() bool { return a.Stopped() })
	// The verified credential is forgotten with the engine: a new login verifies again.
	before := e.srv.AuthAttempts()
	b := e.login()
	if b == a || e.srv.AuthAttempts() != before+1 {
		t.Fatalf("expected a fresh engine and one verification")
	}
}
