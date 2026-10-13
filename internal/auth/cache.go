// Package auth keeps the in-memory credential state that protects the Active
// Directory account behind the bridge (DESIGN section 3):
//
//   - a verified-credential cache, so the many cheap LOGINs MailFlow makes never
//     reach Exchange (salted HMAC, constant-time compare, memory only);
//   - a failure cache, so a password whose verification failed is refused locally
//     for a cooldown instead of being retried (account lockout protection). Every
//     kind of failure is cached, not only a 401: a gateway in front of Exchange
//     (an access-policy gateway) answers a wrong password with a 403, a redirect or a logon page,
//     and each of those is still a failed logon in Active Directory. Repeating
//     the same failing password escalates the cooldown: 15 minutes, 1 hour,
//     4 hours, 24 hours;
//   - a per-user budget of unsuccessful verifications in a sliding window, so
//     that guessing a password cannot make Exchange count more than a couple of
//     failures per window.
//
// Passwords are never stored: only HMAC-SHA256(salt, user NUL password) with a
// salt generated at process start. Nothing here is persisted.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"strings"
	"sync"
	"time"
)

// Sum is a salted credential digest.
type Sum [sha256.Size]byte

// FailKind says why a credential is refused locally.
type FailKind int

const (
	// FailRejected: Exchange answered 401, the password is wrong.
	FailRejected FailKind = iota + 1
	// FailUnverified: the verification failed in another way (a gateway's 403 or
	// logon page, a timeout, a server error): the password may or may not be
	// wrong, and the attempt may still have counted as a failed logon.
	FailUnverified
	// FailTransient: Exchange did not answer (429, a 5xx, a time-out): it cannot have been a refusal
	// of the credential, so the pause is short and fixed (RecordTransient) and costs no budget.
	FailTransient
)

// Limits of the bookkeeping. Without them a client that invents login names (a domain entry in the
// allow-list lets it) would grow the maps for as long as the cooldowns last.
const (
	// DefaultMaxFailed and DefaultMaxUsers bound the failure cache and the per-user bookkeeping;
	// when full, the entries that end soonest are dropped first.
	DefaultMaxFailed = 4096
	DefaultMaxUsers  = 4096
	// DefaultWindow is how long an unsuccessful verification stays on a user's budget when the cache
	// is not told the window of the configuration.
	DefaultWindow = 30 * time.Minute
)

// escalation multiplies the base cooldown on each further failure with the same
// password: with the default 15 minutes it gives 15 minutes, 1 hour, 4 hours and
// 24 hours.
var escalation = [...]time.Duration{1, 4, 16, 96}

// MaxCooldown caps every cooldown.
const MaxCooldown = 24 * time.Hour

type failure struct {
	until time.Time
	kind  FailKind
}

// userState is what the cache remembers about unsuccessful logins of one user.
type userState struct {
	keepUntil time.Time   // the bookkeeping is dropped after this: the cooldown and the window are over
	attempts  []time.Time // unsuccessful verifications inside the window
	last      Sum         // the credential of the latest failure ...
	hasLast   bool
	level     int // ... and how many failures in a row it had, minus one
}

// Cache holds verified and rejected credential digests.
type Cache struct {
	salt []byte
	now  func() time.Time

	maxFailed, maxUsers int
	window              time.Duration

	mu       sync.Mutex
	verified map[string]Sum        // user -> digest of the credential Exchange accepted
	failed   map[Sum]failure       // digest of a credential whose verification failed -> end of cooldown
	users    map[string]*userState // user -> failure bookkeeping
}

// New returns an empty cache with a fresh random salt.
func New() *Cache {
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		panic("auth: no randomness: " + err.Error())
	}
	return &Cache{salt: salt, now: time.Now, maxFailed: DefaultMaxFailed, maxUsers: DefaultMaxUsers, window: DefaultWindow,
		verified: map[string]Sum{}, failed: map[Sum]failure{}, users: map[string]*userState{}}
}

// SetLimits changes the bounds of the failure cache and of the per-user bookkeeping, and the window
// the per-user budget is counted in (the manager passes its configuration; tests pass small bounds).
func (c *Cache) SetLimits(maxFailed, maxUsers int, window time.Duration) {
	c.mu.Lock()
	if maxFailed > 0 {
		c.maxFailed = maxFailed
	}
	if maxUsers > 0 {
		c.maxUsers = maxUsers
	}
	if window > 0 {
		c.window = window
	}
	c.mu.Unlock()
}

// SetClock replaces the time source (tests).
func (c *Cache) SetClock(now func() time.Time) { c.now = now }

func norm(user string) string { return strings.ToLower(strings.TrimSpace(user)) }

// Digest returns the salted digest of a credential pair.
func (c *Cache) Digest(user, pass string) Sum {
	mac := hmac.New(sha256.New, c.salt)
	mac.Write([]byte(norm(user)))
	mac.Write([]byte{0})
	mac.Write([]byte(pass))
	var s Sum
	copy(s[:], mac.Sum(nil))
	return s
}

// IsVerified reports whether (user, pass) is the credential Exchange accepted.
// The comparison is constant-time.
func (c *Cache) IsVerified(user, pass string) bool {
	want := c.Digest(user, pass)
	c.mu.Lock()
	have, ok := c.verified[norm(user)]
	c.mu.Unlock()
	if !ok {
		// Compare anyway so the timing does not reveal whether a user is cached.
		have = Sum{}
	}
	return subtle.ConstantTimeCompare(have[:], want[:]) == 1 && ok
}

// MarkVerified records the credential Exchange just accepted and forgets any
// earlier rejection of the same credential and the user's failure history: a
// success proves the password right.
func (c *Cache) MarkVerified(user, pass string) Sum {
	d := c.Digest(user, pass)
	c.mu.Lock()
	c.verified[norm(user)] = d
	delete(c.failed, d)
	delete(c.users, norm(user))
	c.mu.Unlock()
	return d
}

// ForgetVerified drops the verified credential of user (engine stopped or
// credentials rejected).
func (c *Cache) ForgetVerified(user string) {
	c.mu.Lock()
	delete(c.verified, norm(user))
	c.mu.Unlock()
}

// ForgetIfSum drops the verified credential only if it still is d, so a late
// failure report for an old password cannot invalidate a newer one.
func (c *Cache) ForgetIfSum(user string, d Sum) {
	c.mu.Lock()
	if have, ok := c.verified[norm(user)]; ok && subtle.ConstantTimeCompare(have[:], d[:]) == 1 {
		delete(c.verified, norm(user))
	}
	c.mu.Unlock()
}

// MarkFailedSum records that Exchange rejected the credential with digest d;
// identical attempts are refused locally until the cooldown ends. It does not
// escalate and does not use the user's budget (RecordFailure does).
func (c *Cache) MarkFailedSum(d Sum, cooldown time.Duration) {
	c.mu.Lock()
	c.failed[d] = failure{until: c.now().Add(cooldown), kind: FailRejected}
	c.gc()
	c.mu.Unlock()
}

// MarkFailed is MarkFailedSum for a plain credential pair.
func (c *Cache) MarkFailed(user, pass string, cooldown time.Duration) {
	c.MarkFailedSum(c.Digest(user, pass), cooldown)
}

// RecordFailure notes one unsuccessful verification of the credential with
// digest d for user (of any kind: see FailKind). It counts against the user's
// budget, caches the digest for a cooldown that grows each time the same
// credential fails again (base, 4x, 16x, 96x; at most MaxCooldown), and returns
// that cooldown and how many failures in a row the credential has had. A failure
// with a different credential starts again at the base cooldown; only a success
// (MarkVerified) clears the history.
func (c *Cache) RecordFailure(user string, d Sum, kind FailKind, base time.Duration) (cooldown time.Duration, streak int) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	u := c.users[norm(user)]
	if u == nil {
		u = &userState{}
		c.users[norm(user)] = u
	}
	u.attempts = append(u.attempts, now)
	if u.hasLast && subtle.ConstantTimeCompare(u.last[:], d[:]) == 1 {
		u.level++
	} else {
		u.last, u.hasLast, u.level = d, true, 0
	}
	idx := u.level
	if idx >= len(escalation) {
		idx = len(escalation) - 1
	}
	cooldown = base * escalation[idx]
	if cooldown > MaxCooldown {
		cooldown = MaxCooldown
	}
	c.failed[d] = failure{until: now.Add(cooldown), kind: kind}
	u.keepUntil = now.Add(maxDur(cooldown, c.window))
	c.gc()
	return cooldown, u.level + 1
}

// RecordTransient notes that Exchange did not answer a verification (429, a 5xx, a time-out): the same
// credential is refused locally for pause, a short fixed time, and nothing else is remembered: no budget
// is used and the cooldown does not escalate.
func (c *Cache) RecordTransient(d Sum, pause time.Duration) {
	c.mu.Lock()
	c.failed[d] = failure{until: c.now().Add(pause), kind: FailTransient}
	c.gc()
	c.mu.Unlock()
}

func maxDur(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// CountAttempt counts one unsuccessful verification against the user's budget without caching the
// credential as failed: for the first of an in-session answer that only looks like a refusal (a
// gateway's 403), which a recovering gateway may never repeat.
func (c *Cache) CountAttempt(user string) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	u := c.users[norm(user)]
	if u == nil {
		u = &userState{}
		c.users[norm(user)] = u
	}
	u.attempts = append(u.attempts, now)
	if k := now.Add(c.window); k.After(u.keepUntil) {
		u.keepUntil = k
	}
	c.gc()
}

// AllowAttempt reports whether the user may cause another Exchange verification:
// fewer than max unsuccessful ones happened in the last window. If not, it says
// when the oldest of them leaves the window.
func (c *Cache) AllowAttempt(user string, max int, window time.Duration) (ok bool, retryAt time.Time) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	u := c.users[norm(user)]
	if u == nil {
		return true, time.Time{}
	}
	keep := u.attempts[:0]
	for _, t := range u.attempts {
		if now.Sub(t) < window {
			keep = append(keep, t)
		}
	}
	u.attempts = keep
	if len(u.attempts) < max {
		return true, time.Time{}
	}
	return false, u.attempts[len(u.attempts)-max].Add(window)
}

// FailedUntil reports whether (user, pass) was refused recently and, if so,
// when the cooldown ends.
func (c *Cache) FailedUntil(user, pass string) (time.Time, bool) {
	until, _, ok := c.FailedUntilKind(user, pass)
	return until, ok
}

// FailedUntilKind is FailedUntil that also says why.
func (c *Cache) FailedUntilKind(user, pass string) (time.Time, FailKind, bool) {
	d := c.Digest(user, pass)
	c.mu.Lock()
	defer c.mu.Unlock()
	f, ok := c.failed[d]
	if !ok {
		return time.Time{}, 0, false
	}
	if !c.now().Before(f.until) {
		delete(c.failed, d)
		return time.Time{}, 0, false
	}
	return f.until, f.kind, true
}

// gc drops expired failures and the bookkeeping of users whose cooldown and window are over, then,
// if a map is still over its bound, the entries that end soonest; the caller holds the lock.
func (c *Cache) gc() {
	now := c.now()
	for d, f := range c.failed {
		if !now.Before(f.until) {
			delete(c.failed, d)
		}
	}
	for n, u := range c.users {
		if !now.Before(u.keepUntil) {
			delete(c.users, n)
		}
	}
	for len(c.failed) > c.maxFailed {
		var oldest Sum
		var at time.Time
		first := true
		for d, f := range c.failed {
			if first || f.until.Before(at) {
				oldest, at, first = d, f.until, false
			}
		}
		delete(c.failed, oldest)
	}
	for len(c.users) > c.maxUsers {
		var oldest string
		var at time.Time
		first := true
		for n, u := range c.users {
			if first || u.keepUntil.Before(at) {
				oldest, at, first = n, u.keepUntil, false
			}
		}
		delete(c.users, oldest)
	}
}

// UserEntries is the number of users with failure bookkeeping, for tests and diagnostics.
func (c *Cache) UserEntries() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.users)
}

// Sizes returns (verified, failed) entry counts, for tests and diagnostics.
func (c *Cache) Sizes() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.verified), len(c.failed)
}

// Reset forgets every verified and rejected credential (after a remote wipe).
func (c *Cache) Reset() {
	c.mu.Lock()
	c.verified = map[string]Sum{}
	c.failed = map[Sum]failure{}
	c.users = map[string]*userState{}
	c.mu.Unlock()
}
