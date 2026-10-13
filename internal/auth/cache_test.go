package auth

import (
	"fmt"
	"testing"
	"time"
)

func TestVerifiedCache(t *testing.T) {
	c := New()
	if c.IsVerified("a@x.test", "pw") {
		t.Fatal("empty cache must not verify anything")
	}
	c.MarkVerified("A@X.test", "pw-1")
	if !c.IsVerified("a@x.test", "pw-1") {
		t.Error("verified credential not recognised (user is case-insensitive)")
	}
	if c.IsVerified("a@x.test", "pw-2") || c.IsVerified("b@x.test", "pw-1") {
		t.Error("different password or user must not verify")
	}
	// Re-verifying with a new password replaces the old one.
	c.MarkVerified("a@x.test", "pw-2")
	if c.IsVerified("a@x.test", "pw-1") || !c.IsVerified("a@x.test", "pw-2") {
		t.Error("new credential should replace the old one")
	}
	c.ForgetVerified("a@x.test")
	if c.IsVerified("a@x.test", "pw-2") {
		t.Error("forgotten credential still verifies")
	}
}

func TestSaltedDigestsDifferPerCacheAndHideThePassword(t *testing.T) {
	a, b := New(), New()
	if a.Digest("u", "p") == b.Digest("u", "p") {
		t.Error("digests must depend on the per-process salt")
	}
	if a.Digest("u", "p") == a.Digest("u", "q") || a.Digest("u", "p") == a.Digest("v", "p") {
		t.Error("digest collision across inputs")
	}
	// "u" NUL "p" must not collide with "u\x00p" split elsewhere.
	if a.Digest("ab", "c") == a.Digest("a", "bc") {
		t.Error("digest does not separate user and password")
	}
}

func TestFailureCooldown(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := New()
	c.SetClock(func() time.Time { return now })
	c.MarkFailed("a@x.test", "bad", 15*time.Minute)
	until, ok := c.FailedUntil("a@x.test", "bad")
	if !ok || !until.Equal(now.Add(15*time.Minute)) {
		t.Fatalf("FailedUntil = %v %v", until, ok)
	}
	if _, ok := c.FailedUntil("a@x.test", "other"); ok {
		t.Error("a different password must not be blocked")
	}
	now = now.Add(14 * time.Minute)
	if _, ok := c.FailedUntil("a@x.test", "bad"); !ok {
		t.Error("cooldown ended early")
	}
	now = now.Add(2 * time.Minute)
	if _, ok := c.FailedUntil("a@x.test", "bad"); ok {
		t.Error("cooldown did not end")
	}
	if _, nfail := c.Sizes(); nfail != 0 {
		t.Errorf("expired entry not collected: %d", nfail)
	}
}

func TestVerifyClearsFailure(t *testing.T) {
	c := New()
	c.MarkFailed("a@x.test", "pw", time.Hour)
	c.MarkVerified("a@x.test", "pw")
	if _, ok := c.FailedUntil("a@x.test", "pw"); ok {
		t.Error("a credential Exchange accepted must not stay blocked")
	}
}

func TestForgetIfSumKeepsNewerCredential(t *testing.T) {
	c := New()
	old := c.MarkVerified("a@x.test", "old")
	c.MarkVerified("a@x.test", "new")
	c.ForgetIfSum("a@x.test", old) // late report about the old password
	if !c.IsVerified("a@x.test", "new") {
		t.Error("late failure of the old password invalidated the new one")
	}
	c.ForgetIfSum("a@x.test", c.Digest("a@x.test", "new"))
	if c.IsVerified("a@x.test", "new") {
		t.Error("current credential should have been forgotten")
	}
}

func TestAllowAttemptSlidingWindow(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := New()
	c.SetClock(func() time.Time { return now })
	const window = 30 * time.Minute
	if ok, _ := c.AllowAttempt("a@x.test", 2, window); !ok {
		t.Fatal("a fresh user has a budget")
	}
	d1, d2, d3 := c.Digest("a@x.test", "1"), c.Digest("a@x.test", "2"), c.Digest("a@x.test", "3")
	c.RecordFailure("a@x.test", d1, FailRejected, time.Minute)
	now = now.Add(10 * time.Minute)
	if ok, _ := c.AllowAttempt("a@x.test", 2, window); !ok {
		t.Fatal("one failure leaves one attempt")
	}
	c.RecordFailure("a@x.test", d2, FailUnverified, time.Minute)
	ok, retry := c.AllowAttempt("a@x.test", 2, window)
	if ok || !retry.Equal(time.Unix(1_800_000_000, 0).Add(window)) {
		t.Fatalf("budget spent: ok=%v retry=%v (the oldest attempt leaves the window at +30 min)", ok, retry)
	}
	if ok, _ := c.AllowAttempt("b@x.test", 2, window); !ok {
		t.Error("budgets are per user")
	}
	now = now.Add(20*time.Minute + time.Second) // the first attempt is out of the window
	if ok, _ := c.AllowAttempt("a@x.test", 2, window); !ok {
		t.Fatal("window did not slide")
	}
	c.RecordFailure("a@x.test", d3, FailRejected, time.Minute)
	if ok, retry := c.AllowAttempt("a@x.test", 2, window); ok || !retry.Equal(time.Unix(1_800_000_000, 0).Add(10*time.Minute+window)) {
		t.Fatalf("second window: ok=%v retry=%v", ok, retry)
	}
	// A success proves the password right and clears the history.
	c.MarkVerified("a@x.test", "good")
	if ok, _ := c.AllowAttempt("a@x.test", 2, window); !ok {
		t.Error("a success must clear the budget")
	}
}

func TestRecordFailureEscalatesAndCaps(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := New()
	c.SetClock(func() time.Time { return now })
	d := c.Digest("a@x.test", "bad")
	want := []time.Duration{15 * time.Minute, time.Hour, 4 * time.Hour, 24 * time.Hour, 24 * time.Hour, 24 * time.Hour}
	for i, w := range want {
		got, streak := c.RecordFailure("a@x.test", d, FailRejected, 15*time.Minute)
		if got != w || streak != i+1 {
			t.Fatalf("failure %d: cooldown %v streak %d, want %v streak %d", i+1, got, streak, w, i+1)
		}
		if until, kind, ok := c.FailedUntilKind("a@x.test", "bad"); !ok || kind != FailRejected || !until.Equal(now.Add(w)) {
			t.Fatalf("failure %d: FailedUntilKind = %v %v %v", i+1, until, kind, ok)
		}
	}
	// A cooldown never exceeds a day, whatever the base.
	c2 := New()
	c2.SetClock(func() time.Time { return now })
	d2 := c2.Digest("u", "p")
	var got time.Duration
	for i := 0; i < 6; i++ {
		got, _ = c2.RecordFailure("u", d2, FailRejected, 12*time.Hour)
	}
	if got != MaxCooldown {
		t.Errorf("cap: %v", got)
	}
}

func TestEscalationRestartsForAnotherCredentialOrAfterASuccess(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := New()
	c.SetClock(func() time.Time { return now })
	a, b := c.Digest("u", "a"), c.Digest("u", "b")
	c.RecordFailure("u", a, FailRejected, time.Minute)
	if cd, _ := c.RecordFailure("u", a, FailRejected, time.Minute); cd != 4*time.Minute {
		t.Fatalf("second failure of the same password: %v", cd)
	}
	if cd, streak := c.RecordFailure("u", b, FailUnverified, time.Minute); cd != time.Minute || streak != 1 {
		t.Fatalf("another password starts again: %v %d", cd, streak)
	}
	if cd, _ := c.RecordFailure("u", b, FailUnverified, time.Minute); cd != 4*time.Minute {
		t.Fatalf("and escalates from there: %v", cd)
	}
	c.MarkVerified("u", "good")
	if cd, streak := c.RecordFailure("u", b, FailRejected, time.Minute); cd != time.Minute || streak != 1 {
		t.Fatalf("a success clears the escalation: %v %d", cd, streak)
	}
	// The kind is remembered per credential.
	if _, kind, _ := c.FailedUntilKind("u", "a"); kind != FailRejected {
		t.Errorf("kind of a = %v", kind)
	}
}

// A client that invents names must not grow the maps for as long as the cooldowns last.
func TestTheBookkeepingIsBoundedAndExpires(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	c := New()
	c.SetClock(func() time.Time { return now })
	c.SetLimits(50, 50, 30*time.Minute)
	for i := 0; i < 300; i++ {
		u := fmt.Sprintf("invented%03d@example.test", i)
		c.RecordFailure(u, c.Digest(u, "pw"), FailRejected, 15*time.Minute)
		if _, failed := c.Sizes(); failed > 50 || c.UserEntries() > 50 {
			t.Fatalf("after %d names: %d failures and %d users, the bounds are 50", i+1, failed, c.UserEntries())
		}
		c.CountAttempt(u + "x")
		if c.UserEntries() > 50 {
			t.Fatalf("CountAttempt grew the map to %d", c.UserEntries())
		}
	}
	// The cooldown and the window are over: the next write sweeps them away.
	now = now.Add(25 * time.Hour)
	c.RecordTransient(c.Digest("late@example.test", "pw"), time.Minute)
	if _, failed := c.Sizes(); failed != 1 || c.UserEntries() != 0 {
		t.Errorf("a day later: %d failures, %d users, want 1 and 0", failed, c.UserEntries())
	}
}

// A user's record lives as long as its cooldown or the window, whichever is longer: the budget is not forgotten early.
func TestAUsersAttemptsStayForTheWindow(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	c := New()
	c.SetClock(func() time.Time { return now })
	c.SetLimits(0, 0, 30*time.Minute)
	c.CountAttempt("a@example.test")
	c.CountAttempt("a@example.test")
	now = now.Add(20 * time.Minute)
	c.RecordTransient(c.Digest("b@example.test", "x"), time.Minute) // any write runs the sweep
	if ok, _ := c.AllowAttempt("a@example.test", 2, 30*time.Minute); ok {
		t.Error("the two attempts were forgotten after 20 minutes")
	}
	now = now.Add(15 * time.Minute)
	c.RecordTransient(c.Digest("c@example.test", "x"), time.Minute)
	if c.UserEntries() != 0 {
		t.Errorf("the record outlived the window: %d", c.UserEntries())
	}
}

func TestATransientPauseIsShortFixedAndCostsNoBudget(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	c := New()
	c.SetClock(func() time.Time { return now })
	d := c.Digest("a@example.test", "pw")
	for i := 0; i < 5; i++ {
		c.RecordTransient(d, time.Minute)
		until, kind, ok := c.FailedUntilKind("a@example.test", "pw")
		if !ok || kind != FailTransient || until.Sub(now) != time.Minute {
			t.Fatalf("round %d: %v %v %v", i, until.Sub(now), kind, ok)
		}
		now = now.Add(61 * time.Second)
	}
	if ok, _ := c.AllowAttempt("a@example.test", 1, time.Hour); !ok {
		t.Error("a transient pause used the budget")
	}
	if c.UserEntries() != 0 {
		t.Error("a transient pause made a per-user record")
	}
}
