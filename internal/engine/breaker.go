package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"easbridge/internal/auth"
	"easbridge/internal/store"
	"easbridge/internal/upstream"
)

// The circuit breaker.
//
// Each loop of the engine (Sync, outbox, fetch, Ping) has its own back-off, but a
// problem that is not about one item makes all of them hammer Exchange at the
// same time: a database that cannot be written leaves the same Sync, the same
// ItemOperations and the same flag change due again and again (12,800 identical
// Syncs in two seconds in the security review of v0.1.0), and a server that is
// shedding load is asked for more by every loop. So every mailbox has one breaker
// that pauses ALL its Exchange traffic:
//
//   - after Threshold failures in a row to write the local database;
//   - at once when Exchange or a gateway asks to slow down (HTTP 429 or 503, EAS
//     status 111), honouring Retry-After;
//   - after Threshold HTTP 5xx or transient EAS statuses in a row.
//
// The pause starts at Base and doubles with every consecutive trip, up to Max
// (15 minutes), and is never shorter than the server's Retry-After (capped at
// Max). A call that goes through afterwards and succeeds closes the breaker; one
// that fails trips it again at once with a longer pause. While it is open no
// request leaves the machine: the guard around the upstream session refuses
// them, and the loops sleep until the pause ends. /healthz shows the state.
type breaker struct {
	mu        sync.Mutex
	now       func() time.Time
	log       *slog.Logger
	base, max time.Duration
	threshold int

	until      time.Time
	reason     string
	level      int // consecutive trips; the pause is base << level
	upFails    int // consecutive upstream trouble
	localFails int // consecutive failed database writes
	trips      int // total, for /healthz
}

func newBreaker(cfg Config, log *slog.Logger) *breaker {
	return &breaker{now: cfg.Clock, log: log, base: cfg.BreakerBase, max: cfg.BreakerMax, threshold: cfg.BreakerThreshold}
}

// remaining returns how long the pause lasts (0: closed).
func (b *breaker) remaining() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if d := b.until.Sub(b.now()); d > 0 {
		return d
	}
	return 0
}

// state describes the breaker for /healthz.
func (b *breaker) state() (until time.Time, reason string, trips int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.until.After(b.now()) {
		return time.Time{}, "", b.trips
	}
	return b.until, b.reason, b.trips
}

// upstreamOK records an answer that shows Exchange is serving requests.
func (b *breaker) upstreamOK() {
	b.mu.Lock()
	b.upFails = 0
	if b.localFails == 0 {
		b.level = 0
	}
	b.mu.Unlock()
}

// localOK records a database write that worked.
func (b *breaker) localOK() {
	b.mu.Lock()
	b.localFails = 0
	if b.upFails == 0 {
		b.level = 0
	}
	b.mu.Unlock()
}

// upstreamTrouble records throttling or a server-side failure. explicit means
// the server asked to back off; retryAfter is its Retry-After (0: none).
func (b *breaker) upstreamTrouble(what string, explicit bool, retryAfter time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.upFails++
	if explicit || b.upFails >= b.threshold {
		b.tripLocked("Exchange is throttling or failing ("+what+")", retryAfter)
	}
}

// localFailure records a failed database write.
func (b *breaker) localFailure(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.localFails++
	if b.localFails >= b.threshold {
		b.tripLocked("the local database cannot be written", 0)
		b.log.Error("the local database cannot be written", "err", err)
	}
}

func (b *breaker) tripLocked(reason string, retryAfter time.Duration) {
	now := b.now()
	if b.until.After(now) {
		// Already open: this is a request that was in flight when it opened, failing
		// too. It belongs to the same incident, so it may lengthen the pause (the
		// server's Retry-After) but must not escalate it.
		if retryAfter > 0 {
			if retryAfter > b.max {
				retryAfter = b.max
			}
			if until := now.Add(retryAfter); until.After(b.until) {
				b.until, b.reason = until, reason
			}
		}
		return
	}
	d := b.base
	for i := 0; i < b.level && d < b.max; i++ {
		d *= 2
	}
	if retryAfter > d {
		d = retryAfter
	}
	if d > b.max || d <= 0 {
		d = b.max
	}
	b.until = now.Add(d)
	b.reason = reason
	b.level++
	b.trips++
	b.log.Warn("pausing all Exchange traffic of this mailbox", "reason", reason, "for", d.String(), "trips", b.trips)
}

// troubleOf classifies an upstream error: whether it is throttling or a
// server-side failure, whether the server asked to back off, and its Retry-After.
func troubleOf(err error) (what string, trouble, explicit bool, retryAfter time.Duration) {
	var te *upstream.ThrottleError
	if errors.As(err, &te) {
		what = fmt.Sprintf("HTTP %d", te.HTTP)
		if te.HTTP == 0 {
			what = fmt.Sprintf("EAS status %d", te.EAS)
		}
		return what, true, te.Explicit, te.RetryAfter
	}
	var se *upstream.StatusError
	if errors.As(err, &se) && !se.Perm {
		return fmt.Sprintf("EAS status %d", se.Code), true, se.Code == 111, 0
	}
	return "", false, false, 0
}

// pausedError is what the guard returns while the breaker is open. It is an
// ErrUnavailable (the upstream one and the engine's, which the IMAP and SMTP
// layers answer as NO [UNAVAILABLE] and a 4xx), so callers treat it like any
// transient failure; the loops do not call at all while paused.
type pausedError struct {
	until  time.Time
	reason string
}

func (e *pausedError) Error() string {
	return "Exchange traffic is paused until " + e.until.UTC().Format(time.RFC3339) + " (" + e.reason + ")"
}
func (e *pausedError) Is(target error) bool {
	return target == upstream.ErrUnavailable || target == ErrUnavailable
}

// isPaused reports whether err is the guard's refusal while the breaker is open.
func isPaused(err error) bool {
	var pe *pausedError
	return errors.As(err, &pe)
}

// pauseWait blocks while the breaker is open and returns true once it is closed.
// It returns false when the account is stopping. The wait is cut into slices of
// at most the housekeeping interval so that a clock that jumps is noticed.
func (a *Account) pauseWait() bool {
	for {
		d := a.brk.remaining()
		if d <= 0 {
			return true
		}
		if d > a.cfg.Housekeeping {
			d = a.cfg.Housekeeping
		}
		if !a.sleepCtx(d) {
			return false
		}
	}
}

// sessionAuthError remembers which credential the failing upstream session
// used, so that a late 401 for a password that has been replaced since cannot
// forget the new one.
type sessionAuthError struct {
	err error
	sum auth.Sum
}

func (e *sessionAuthError) Error() string { return e.err.Error() }
func (e *sessionAuthError) Unwrap() error { return e.err }

// sessionAuthLikeError is the same for a 403, an unfollowed redirect or an HTML logon page
// (upstream.ErrAuthLike). It stays an ErrUnavailable for everything that backs off.
type sessionAuthLikeError struct {
	err error
	sum auth.Sum
}

func (e *sessionAuthLikeError) Error() string { return e.err.Error() }
func (e *sessionAuthLikeError) Unwrap() error { return e.err }

// guard wraps an upstream session of an account: it refuses calls while the
// breaker is open, feeds the breaker with what comes back, and tags 401 errors
// with the credential of the session.
type guard struct {
	upstream.Mailbox
	a   *Account
	sum auth.Sum
}

func (g *guard) pre() error {
	if d := g.a.brk.remaining(); d > 0 {
		until, reason, _ := g.a.brk.state()
		return &pausedError{until: until, reason: reason}
	}
	return nil
}

func (g *guard) post(err error) error {
	switch {
	case err == nil:
		g.a.brk.upstreamOK()
		g.a.authLike.Store(0)
	case errors.Is(err, upstream.ErrAuthFailed):
		return &sessionAuthError{err: err, sum: g.sum}
	case errors.Is(err, upstream.ErrAuthLike):
		if what, trouble, explicit, after := troubleOf(err); trouble {
			g.a.brk.upstreamTrouble(what, explicit, after)
		}
		return &sessionAuthLikeError{err: err, sum: g.sum}
	default:
		if what, trouble, explicit, after := troubleOf(err); trouble {
			g.a.brk.upstreamTrouble(what, explicit, after)
		} else {
			var se *upstream.StatusError
			if errors.As(err, &se) {
				g.a.brk.upstreamOK() // a protocol-level answer: the server is serving
			}
		}
	}
	return err
}

func (g *guard) FolderSync(ctx context.Context, key string) (*upstream.FolderDelta, error) {
	if err := g.pre(); err != nil {
		return nil, err
	}
	r, err := g.Mailbox.FolderSync(ctx, key)
	return r, g.post(err)
}

func (g *guard) Sync(ctx context.Context, folderID, key string, o upstream.SyncOptions) (*upstream.SyncDelta, error) {
	if err := g.pre(); err != nil {
		return nil, err
	}
	r, err := g.Mailbox.Sync(ctx, folderID, key, o)
	return r, g.post(err)
}

func (g *guard) SyncContacts(ctx context.Context, folderID, key string) (*upstream.ContactDelta, error) {
	if err := g.pre(); err != nil {
		return nil, err
	}
	r, err := g.Mailbox.SyncContacts(ctx, folderID, key)
	return r, g.post(err)
}

// SearchDirectory differs from the other calls in one way: a Search status of the
// protocol (3, 7, 10: an error or a time-out on the server's side) is the directory's
// own trouble, which its loop answers with a back-off of its own; it must not pause the
// mail through the breaker. Throttling at the HTTP level (429, 503, 5xx) still does.
func (g *guard) SearchDirectory(ctx context.Context, query string) (*upstream.DirectoryResult, error) {
	if err := g.pre(); err != nil {
		return nil, err
	}
	r, err := g.Mailbox.SearchDirectory(ctx, query)
	var se *upstream.StatusError
	if errors.As(err, &se) && se.Op == "Search" && se.Code < 100 {
		// A status of the Search command itself (the common statuses, 101 and up, such as
		// 111 "retry later", still go through the breaker).
		g.a.brk.upstreamOK() // the server answered; what it said is the loop's business
		return r, err
	}
	return r, g.post(err)
}

func (g *guard) FetchMIME(ctx context.Context, folderID, serverID string) ([]byte, error) {
	if err := g.pre(); err != nil {
		return nil, err
	}
	r, err := g.Mailbox.FetchMIME(ctx, folderID, serverID)
	return r, g.post(err)
}

func (g *guard) ApplyChanges(ctx context.Context, folderID, key string, changes []upstream.Change, hardDelete bool) (*upstream.ApplyResult, error) {
	if err := g.pre(); err != nil {
		return nil, err
	}
	r, err := g.Mailbox.ApplyChanges(ctx, folderID, key, changes, hardDelete)
	return r, g.post(err)
}

func (g *guard) MoveItems(ctx context.Context, src, dst string, ids []string) ([]upstream.MoveResult, error) {
	if err := g.pre(); err != nil {
		return nil, err
	}
	r, err := g.Mailbox.MoveItems(ctx, src, dst, ids)
	return r, g.post(err)
}

func (g *guard) CreateFolder(ctx context.Context, key, parentID, name string) (*upstream.FolderResult, error) {
	if err := g.pre(); err != nil {
		return nil, err
	}
	r, err := g.Mailbox.CreateFolder(ctx, key, parentID, name)
	return r, g.post(err)
}

func (g *guard) RenameFolder(ctx context.Context, key, serverID, newParentID, newName string) (*upstream.FolderResult, error) {
	if err := g.pre(); err != nil {
		return nil, err
	}
	r, err := g.Mailbox.RenameFolder(ctx, key, serverID, newParentID, newName)
	return r, g.post(err)
}

func (g *guard) DeleteFolder(ctx context.Context, key, serverID string) (*upstream.FolderResult, error) {
	if err := g.pre(); err != nil {
		return nil, err
	}
	r, err := g.Mailbox.DeleteFolder(ctx, key, serverID)
	return r, g.post(err)
}

func (g *guard) SendMail(ctx context.Context, mime []byte, saveInSent bool) error {
	if err := g.pre(); err != nil {
		return err
	}
	return g.post(g.Mailbox.SendMail(ctx, mime, saveInSent))
}

func (g *guard) Ping(ctx context.Context, heartbeat time.Duration, folderIDs []string) (*upstream.PingResult, error) {
	if err := g.pre(); err != nil {
		return nil, err
	}
	r, err := g.Mailbox.Ping(ctx, heartbeat, folderIDs)
	return r, g.post(err)
}

// AcknowledgeWipe is never held back: a wipe order must be answered.
func (g *guard) AcknowledgeWipe(ctx context.Context, accountOnly bool) error {
	return g.Mailbox.AcknowledgeWipe(ctx, accountOnly)
}

// dbFailure reports whether err is a failure of the local database itself, as
// opposed to a refusal by the engine's own logic.
func dbFailure(err error) bool { return store.IsDBError(err) }
