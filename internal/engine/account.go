// Package engine owns everything that happens between the local state and
// Exchange for one mailbox: the sync loop, Ping, the durable outbox, folder
// mapping, Sent-copy de-duplication, remote wipe, and the credential/login
// policy. It knows nothing about IMAP or SMTP.
//
// Concurrency model (DESIGN section 2 and 12):
//
//   - one worker goroutine performs all sync traffic, in priority order:
//     requests from IMAP (MOVE, folder operations), FolderSync, the outbox,
//     then one Sync batch of one dirty folder at a time. EAS forbids two
//     concurrent Syncs with the same key, and a single worker makes that
//     impossible by construction;
//   - one Ping goroutine long-polls and only marks folders dirty;
//   - a small pool fetches truncated messages (ItemOperations) concurrently;
//   - SendMail runs in the caller's goroutine.
//
// Every database change goes through Account.commit, which holds the account
// lock from before the transaction until its change events were delivered, so
// a watcher that takes a snapshot under the same lock sees a consistent
// sequence of events.
package engine

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"easbridge/internal/auth"
	"easbridge/internal/store"
	"easbridge/internal/upstream"
)

// Config tunes the engine. Zero values are replaced by DefaultConfig.
type Config struct {
	// Grace is how long an engine outlives its last session.
	Grace time.Duration
	// PingHeartbeat is the initial Ping heartbeat; PingMinHeartbeat is the floor
	// of the adaptive step-down when long requests are dropped early.
	PingHeartbeat    time.Duration
	PingMinHeartbeat time.Duration
	// AuthFailCooldown is the base cooldown after a failed verification of a
	// credential: the same credential failing again is refused for 4, 16 and 96
	// times as long (15 min, 1 h, 4 h, 24 h by default).
	AuthFailCooldown time.Duration
	// LoginAttempts and LoginWindow are the per-user budget of unsuccessful
	// Exchange verifications: at most LoginAttempts in any LoginWindow; further
	// unverified logins are refused locally (NO [UNAVAILABLE]) until the oldest
	// attempt leaves the window. A credential Exchange already accepted is not
	// affected.
	LoginAttempts int
	LoginWindow   time.Duration
	// DomainEntries says that the allow-list holds a domain entry (@example.com): any name of the
	// domain passes the allow-list, so before a login is verified one source address may present
	// only SourceNames distinct names in SourceWindow (10 in 10 minutes by default); the rest are
	// refused locally.
	DomainEntries bool
	SourceNames   int
	SourceWindow  time.Duration
	// TransientPause is how long a credential whose verification got no answer (429, a 5xx, a
	// time-out) is refused locally: short, fixed, no budget (60 s by default).
	TransientPause time.Duration
	// MaxMessageBytes is the size limit for APPEND and SMTP.
	MaxMessageBytes int64
	// PlaceholderTTL is how long a Sent placeholder waits for Exchange's copy.
	PlaceholderTTL time.Duration
	// PlaceholderWindow is the Date tolerance of the Sent-copy fallback match.
	PlaceholderWindow time.Duration
	// SubmittedTTL, SubmittedMax and SubmittedMaxBytes bound the copies the
	// bridge keeps of the messages it submitted with a Bcc header while it waits
	// for Exchange's own copy in Sent Items (submitted.go): a copy older than
	// SubmittedTTL goes, and so do the oldest ones beyond SubmittedMax messages or
	// SubmittedMaxBytes bytes.
	SubmittedTTL      time.Duration
	SubmittedMax      int
	SubmittedMaxBytes int64
	// ReconVerifyDelay is the wait before a verification resync confirms
	// deletions suspected after a silent sync-key reset.
	ReconVerifyDelay time.Duration
	// FetchWorkers is the ItemOperations concurrency.
	FetchWorkers int
	// OutboxBatch is the largest number of changes sent in one Sync.
	OutboxBatch int
	// Housekeeping is the interval of the maintenance tick.
	Housekeeping time.Duration
	// Backoff returns the wait after the nth consecutive failure (n >= 1).
	Backoff func(n int) time.Duration
	// Clock is the time source.
	Clock func() time.Time
	// CallTimeout bounds non-Ping upstream calls.
	CallTimeout time.Duration
	// BreakerBase and BreakerMax bound the pause of the circuit breaker (see
	// breaker.go): it starts at BreakerBase and doubles with every consecutive
	// trip up to BreakerMax. BreakerThreshold is the number of consecutive
	// failures that trips it (throttling answers trip it at once).
	BreakerBase      time.Duration
	BreakerMax       time.Duration
	BreakerThreshold int
	// HierarchyBackoff is the wait after a Ping answered status 7 (the folder
	// hierarchy changed) and the hierarchy has been refreshed; it doubles with
	// every consecutive status 7, up to 60 times itself.
	HierarchyBackoff time.Duration
	// SweepInterval is how often all folders are synced when push is not
	// complete: the server limits the number of Ping folders (folders outside
	// the Ping set get no push) or Ping keeps failing.
	SweepInterval time.Duration

	// ContactsEnabled switches the contacts feature on (EASBRIDGE_CARDDAV_ADDR is
	// set): the Contacts folders are kept, and the directory if Directory.Domains is
	// not empty. Off, the engine does nothing about contacts.
	ContactsEnabled bool
	// ContactsInterval is how often each Contacts folder is synced.
	ContactsInterval time.Duration
	// Directory tunes the Global Address List lookups (directory.go).
	Directory DirectoryConfig
}

// DirectoryConfig tunes the directory (docs/DESIGN-contacts.md section 4).
type DirectoryConfig struct {
	// Domains whose addresses are looked up; empty turns the directory off.
	Domains []string
	// Spacing is the least time between two searches (the bridge's setting is an hour
	// divided by the lookups per hour, never below 20 s). DailyCap is the most
	// searches per UTC day (ten times the hourly rate).
	Spacing  time.Duration
	DailyCap int
	// Refresh is how long a found card is kept before it is looked up again.
	// NotFoundRetry is how long a candidate without a match waits for the next try,
	// MissRecheck how long after a first miss of a found card the second try comes
	// (and the least time between the two misses that remove a card), DenyPause how
	// long the lookups stop after an access denied answer.
	Refresh       time.Duration
	NotFoundRetry time.Duration
	MissRecheck   time.Duration
	DenyPause     time.Duration
	// BackoffBase and BackoffMax bound the wait after a failed search.
	BackoffBase, BackoffMax time.Duration
	// Poll is how often the lookup loop looks again when nothing is due.
	Poll time.Duration
	// SeedBatch is the number of stored messages the seeding reads per transaction.
	SeedBatch int

	// Full turns on the opt-in sweep of the whole Global Address List (sweep.go).
	// FullInterval is the time from the end of one sweep to the start of the next.
	// SweepMaxSearches (default 5,000) and SweepMaxPrefix (default 20) are the caps that
	// end a sweep.
	Full             bool
	FullInterval     time.Duration
	SweepMaxSearches int
	SweepMaxPrefix   int
}

// Enabled reports whether directory lookups run (given contacts are on).
func (d DirectoryConfig) Enabled() bool { return len(d.Domains) > 0 }

func (d DirectoryConfig) withDefaults() DirectoryConfig {
	if d.Spacing == 0 {
		d.Spacing = time.Minute
	}
	if d.DailyCap == 0 {
		d.DailyCap = 600
	}
	if d.Refresh == 0 {
		d.Refresh = 30 * 24 * time.Hour
	}
	if d.NotFoundRetry == 0 {
		d.NotFoundRetry = 30 * 24 * time.Hour
	}
	if d.MissRecheck == 0 {
		d.MissRecheck = 7 * 24 * time.Hour
	}
	if d.DenyPause == 0 {
		d.DenyPause = 24 * time.Hour
	}
	if d.BackoffBase == 0 {
		d.BackoffBase = time.Minute
	}
	if d.BackoffMax == 0 {
		d.BackoffMax = time.Hour
	}
	if d.Poll == 0 {
		d.Poll = time.Minute
	}
	if d.SeedBatch == 0 {
		d.SeedBatch = 200
	}
	if d.FullInterval == 0 {
		d.FullInterval = 720 * time.Hour
	}
	if d.SweepMaxSearches == 0 {
		d.SweepMaxSearches = 5000
	}
	if d.SweepMaxPrefix == 0 {
		d.SweepMaxPrefix = 20
	}
	return d
}

// DefaultConfig returns production defaults.
func DefaultConfig() Config {
	return Config{
		Grace:             30 * time.Minute,
		PingHeartbeat:     480 * time.Second,
		PingMinHeartbeat:  120 * time.Second,
		AuthFailCooldown:  15 * time.Minute,
		LoginAttempts:     2,
		LoginWindow:       30 * time.Minute,
		BreakerBase:       5 * time.Second,
		BreakerMax:        15 * time.Minute,
		BreakerThreshold:  3,
		HierarchyBackoff:  time.Second,
		MaxMessageBytes:   35 << 20,
		PlaceholderTTL:    24 * time.Hour,
		PlaceholderWindow: 10 * time.Minute,
		SubmittedTTL:      24 * time.Hour,
		SubmittedMax:      100,
		SubmittedMaxBytes: 128 << 20,
		ReconVerifyDelay:  5 * time.Minute,
		FetchWorkers:      2,
		OutboxBatch:       100,
		Housekeeping:      time.Minute,
		Backoff:           defaultBackoff,
		Clock:             time.Now,
		CallTimeout:       5 * time.Minute,
		SweepInterval:     5 * time.Minute,
	}
}

func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.Grace == 0 {
		c.Grace = d.Grace
	}
	if c.PingHeartbeat == 0 {
		c.PingHeartbeat = d.PingHeartbeat
	}
	if c.PingMinHeartbeat == 0 {
		c.PingMinHeartbeat = d.PingMinHeartbeat
	}
	if c.AuthFailCooldown == 0 {
		c.AuthFailCooldown = d.AuthFailCooldown
	}
	if c.LoginAttempts == 0 {
		c.LoginAttempts = d.LoginAttempts
	}
	if c.TransientPause == 0 {
		c.TransientPause = time.Minute
	}
	if c.LoginWindow == 0 {
		c.LoginWindow = d.LoginWindow
	}
	if c.BreakerBase == 0 {
		c.BreakerBase = d.BreakerBase
	}
	if c.BreakerMax == 0 {
		c.BreakerMax = d.BreakerMax
	}
	if c.BreakerThreshold == 0 {
		c.BreakerThreshold = d.BreakerThreshold
	}
	if c.HierarchyBackoff == 0 {
		c.HierarchyBackoff = d.HierarchyBackoff
	}
	if c.MaxMessageBytes == 0 {
		c.MaxMessageBytes = d.MaxMessageBytes
	}
	if c.PlaceholderTTL == 0 {
		c.PlaceholderTTL = d.PlaceholderTTL
	}
	if c.PlaceholderWindow == 0 {
		c.PlaceholderWindow = d.PlaceholderWindow
	}
	if c.SubmittedTTL == 0 {
		c.SubmittedTTL = d.SubmittedTTL
	}
	if c.SubmittedMax == 0 {
		c.SubmittedMax = d.SubmittedMax
	}
	if c.SubmittedMaxBytes == 0 {
		c.SubmittedMaxBytes = d.SubmittedMaxBytes
	}
	if c.ReconVerifyDelay == 0 {
		c.ReconVerifyDelay = d.ReconVerifyDelay
	}
	if c.FetchWorkers == 0 {
		c.FetchWorkers = d.FetchWorkers
	}
	if c.OutboxBatch == 0 {
		c.OutboxBatch = d.OutboxBatch
	}
	if c.Housekeeping == 0 {
		c.Housekeeping = d.Housekeeping
	}
	if c.Backoff == nil {
		c.Backoff = d.Backoff
	}
	if c.Clock == nil {
		c.Clock = d.Clock
	}
	if c.CallTimeout == 0 {
		c.CallTimeout = d.CallTimeout
	}
	if c.SweepInterval == 0 {
		c.SweepInterval = d.SweepInterval
	}
	if c.ContactsInterval == 0 {
		c.ContactsInterval = 30 * time.Minute
	}
	c.Directory = c.Directory.withDefaults()
	if !c.ContactsEnabled {
		c.Directory.Domains = nil // the directory needs the contacts feature
	}
	return c
}

// defaultBackoff is 5 s doubling to 5 min, with 20 % jitter.
func defaultBackoff(n int) time.Duration {
	d := 5 * time.Second
	for i := 1; i < n && d < 5*time.Minute; i++ {
		d *= 2
	}
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d + time.Duration(rand.Int64N(int64(d)/5+1))
}

// Listener receives change events for one folder. It runs with the account lock
// held: it must be quick and must not call back into the Account.
type Listener func(store.Event)

type watcher struct{ l Listener }

type request struct {
	ctx  context.Context
	run  func(ctx context.Context) error
	done chan error
}

type folderState struct {
	dirty    bool
	gen      int
	failures int
	retryAt  time.Time
	prio     int
}

// Account is the engine for one mailbox.
type Account struct {
	m    *Manager
	user string
	log  *slog.Logger
	st   *store.Account
	cfg  Config
	sum  auth.Sum // digest of the credential in use (for lockout reporting)

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex // commit + deliver; see package comment
	watches map[int64]map[*watcher]struct{}

	upMu sync.RWMutex
	up   upstream.Mailbox // always a *guard around the real session

	brk *breaker // pauses all Exchange traffic after repeated trouble

	// worker state
	wmu            sync.Mutex
	reqs           []*request
	fs             map[int64]*folderState
	wantFolderSync bool
	folderFailures int
	folderRetryAt  time.Time
	outboxRetryAt  time.Time
	outboxFailures int
	fetchRetry     map[string]fetchHold // in-memory back-off per message, for when the database cannot record it
	nextHousekeep  time.Time
	wake           chan struct{}
	cleanCh        chan struct{}

	fetchWake chan struct{}
	inflight  map[string]bool // folderID|serverID being fetched

	authFailed  atomic.Bool
	authLike    atomic.Int32 // 403s, unfollowed redirects and logon pages in a row (upstream.ErrAuthLike)
	pingLimited atomic.Bool  // the server limits the Ping folder set: sweep the rest periodically
	// pingDegraded is set while Ping keeps failing (for example a proxy that
	// cuts every held request): push is unreliable, so every folder is swept.
	pingDegraded atomic.Bool
	nextSweep    time.Time
	wiped        atomic.Bool
	stopped      atomic.Bool
	pingHB       atomic.Int64 // current heartbeat, ns

	smu      sync.Mutex
	sessions map[int]func() // attached sessions: id -> close function
	nextSess int
	idleAt   time.Time // when the last session detached (zero while any is attached)

	lastSync atomic.Int64 // unix time of the last successful Sync batch

	// contacts: the sync schedule of the Contacts folders (contacts.go), guarded by wmu.
	cstate map[int64]*contactState
	// dirWake wakes the directory loop when new candidates may exist (directory.go).
	dirWake chan struct{}
	// dirFailures counts failed searches in a row (the back-off), dirRetryAt is the
	// earliest time of the next one; both belong to the directory goroutine.
	dirFailures int
	dirRetryAt  time.Time
	// dirSearched counts the searches this engine sent (tests, health).
	dirSearched atomic.Int64

	// bccSeen is set when the first message of Sent Items that Exchange lists with
	// a Bcc property has been stored, so that the log says once that Exchange does
	// give those out (nothing in the specification promises it; see submitted.go).
	bccSeen atomic.Bool
}

func newAccount(m *Manager, st *store.Account, up upstream.Mailbox, sum auth.Sum) *Account {
	ctx, cancel := context.WithCancel(context.Background())
	a := &Account{
		m: m, user: st.User, st: st, cfg: m.cfg, sum: sum,
		log:    m.log.With("acct", m.acctTag(st.User)),
		ctx:    ctx,
		cancel: cancel,

		watches:        map[int64]map[*watcher]struct{}{},
		fs:             map[int64]*folderState{},
		wake:           make(chan struct{}, 1),
		cleanCh:        make(chan struct{}),
		fetchWake:      make(chan struct{}, 1),
		inflight:       map[string]bool{},
		fetchRetry:     map[string]fetchHold{},
		cstate:         map[int64]*contactState{},
		dirWake:        make(chan struct{}, 1),
		sessions:       map[int]func(){},
		wantFolderSync: true,
	}
	a.brk = newBreaker(m.cfg, a.log)
	a.up = &guard{Mailbox: up, a: a, sum: sum}
	a.pingHB.Store(int64(m.cfg.PingHeartbeat))
	a.idleAt = m.cfg.Clock()
	return a
}

// start launches the worker, the Ping loop and the fetch pool.
func (a *Account) start() {
	n := 2 + a.cfg.FetchWorkers
	if a.cfg.Directory.Enabled() {
		n++
		// The seeding counts every stored message once; until it is done the ingest
		// must not count them as well.
		a.st.DB().SetCandidateNoting(a.st.ID, false)
	}
	a.wg.Add(n)
	go func() { defer a.wg.Done(); a.workerLoop() }()
	go func() { defer a.wg.Done(); a.pingLoop() }()
	for i := 0; i < a.cfg.FetchWorkers; i++ {
		go func() { defer a.wg.Done(); a.fetchLoop() }()
	}
	if a.cfg.Directory.Enabled() {
		go func() { defer a.wg.Done(); a.directoryLoop() }()
	}
	// Anything left over from an earlier run.
	a.resumeFromStore()
}

// stopWorkers ends all goroutines without closing the upstream session.
func (a *Account) stopWorkers() {
	a.cancel()
	a.poke()
	a.wakeFetch()
	a.wg.Wait()
}

// Stop ends all goroutines and closes the upstream session.
func (a *Account) Stop() {
	if !a.stopped.CompareAndSwap(false, true) {
		return
	}
	a.stopWorkers()
	a.upstream().Close()
}

// Stopped reports whether the engine has been stopped.
func (a *Account) Stopped() bool { return a.stopped.Load() }

// User returns the login name (lower case).
func (a *Account) User() string { return a.user }

// Store gives the IMAP layer read access to the state.
func (a *Account) Store() *store.Account { return a.st }

// Config returns the engine configuration.
func (a *Account) Config() Config { return a.cfg }

func (a *Account) upstream() upstream.Mailbox {
	a.upMu.RLock()
	defer a.upMu.RUnlock()
	return a.up
}

// swapUpstream installs a freshly opened upstream session for a changed
// password. The old one is closed.
func (a *Account) swapUpstream(up upstream.Mailbox, sum auth.Sum) {
	a.upMu.Lock()
	old := a.up
	a.up = &guard{Mailbox: up, a: a, sum: sum}
	a.sum = sum
	a.upMu.Unlock()
	old.Close()
	a.authLike.Store(0)
	if a.authFailed.Swap(false) {
		a.log.Info("credentials renewed, resuming sync")
	}
	a.poke()
	a.wakeFetch()
}

// Attach registers a client session. closeFn is called if the mailbox is wiped.
// The returned function must be called when the session ends.
func (a *Account) Attach(closeFn func()) (detach func()) {
	a.smu.Lock()
	a.nextSess++
	id := a.nextSess
	a.sessions[id] = closeFn
	a.idleAt = time.Time{}
	a.smu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			a.smu.Lock()
			delete(a.sessions, id)
			if len(a.sessions) == 0 {
				a.idleAt = a.cfg.Clock()
			}
			a.smu.Unlock()
		})
	}
}

// idleSince returns when the last session detached (zero if one is attached).
func (a *Account) idleSince() time.Time {
	a.smu.Lock()
	defer a.smu.Unlock()
	return a.idleAt
}

func (a *Account) closeSessions() {
	a.smu.Lock()
	fns := make([]func(), 0, len(a.sessions))
	for _, f := range a.sessions {
		fns = append(fns, f)
	}
	a.smu.Unlock()
	for _, f := range fns {
		f()
	}
}

// Change delivery -------------------------------------------------------------

// commit runs fn in one database transaction and, if it succeeds, delivers the
// resulting events to the watchers before releasing the account lock.
func (a *Account) commit(origin any, fn func(*store.Tx) error) ([]store.Event, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	evs, err := a.st.Update(origin, fn)
	if err != nil {
		if dbFailure(err) {
			a.brk.localFailure(err) // repeated, it pauses all Exchange traffic
		}
		return nil, err
	}
	a.brk.localOK()
	a.deliverLocked(evs)
	if a.cfg.Directory.Enabled() {
		select {
		case a.dirWake <- struct{}{}:
		default:
		}
	}
	return evs, nil
}

func (a *Account) deliverLocked(evs []store.Event) {
	for _, ev := range evs {
		for w := range a.watches[ev.FolderID] {
			func() {
				defer func() {
					if r := recover(); r != nil {
						a.log.Error("change listener panicked", "panic", r)
					}
				}()
				w.l(ev)
			}()
		}
	}
}

// Watch registers l for the events of one folder and returns a snapshot of the
// folder's messages taken atomically with the registration: every later change
// reaches l exactly once, in commit order. cancel must be called when done.
func (a *Account) Watch(folderID int64, l Listener) (snapshot []store.MsgMeta, cancel func(), err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	snapshot, err = a.st.ListMessages(folderID)
	if err != nil {
		return nil, nil, err
	}
	w := &watcher{l: l}
	if a.watches[folderID] == nil {
		a.watches[folderID] = map[*watcher]struct{}{}
	}
	a.watches[folderID][w] = struct{}{}
	var once sync.Once
	return snapshot, func() {
		once.Do(func() {
			a.mu.Lock()
			delete(a.watches[folderID], w)
			if len(a.watches[folderID]) == 0 {
				delete(a.watches, folderID)
			}
			a.mu.Unlock()
		})
	}, nil
}

// Scheduling ---------------------------------------------------------------

func (a *Account) poke() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

func (a *Account) wakeFetch() {
	select {
	case a.fetchWake <- struct{}{}:
	default:
	}
}

func folderPrio(typ int) int {
	switch typ {
	case store.TypeInbox:
		return 0
	case store.TypeSentItems, store.TypeDrafts:
		return 1
	case store.TypeDeletedItems:
		return 3
	}
	return 2
}

// markDirty schedules a sync of the given local folder ids.
func (a *Account) markDirty(ids ...int64) {
	a.wmu.Lock()
	for _, id := range ids {
		fs := a.fs[id]
		if fs == nil {
			fs = &folderState{prio: 2}
			if f, err := a.st.FolderByID(id); err == nil {
				fs.prio = folderPrio(f.EASType)
			}
			a.fs[id] = fs
		}
		fs.dirty = true
		fs.gen++
	}
	a.wmu.Unlock()
	a.poke()
}

// WaitHierarchy blocks until the folder hierarchy has been fetched at least once
// (the first login on a fresh database finds no folders yet), the timeout passes
// or a context ends. It reports whether the hierarchy is known. Clients call it
// after logging in so that their first LIST or SELECT does not see an empty
// account.
func (a *Account) WaitHierarchy(ctx context.Context, timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		if key, err := a.st.FolderSyncKey(); err == nil && key != "0" {
			return true
		}
		a.wmu.Lock()
		clean := a.cleanCh
		a.wmu.Unlock()
		select {
		case <-clean:
		case <-time.After(250 * time.Millisecond):
		case <-deadline.C:
			return false
		case <-ctx.Done():
			return false
		case <-a.ctx.Done():
			return false
		}
	}
}

// Syncing returns how many folders (and a pending hierarchy sync) still wait for
// the worker. It is 0 once the initial sync has caught up.
func (a *Account) Syncing() int {
	a.wmu.Lock()
	defer a.wmu.Unlock()
	n := 0
	if a.wantFolderSync {
		n++
	}
	for _, fs := range a.fs {
		if fs.dirty {
			n++
		}
	}
	return n
}

// markAllDirty schedules every folder.
func (a *Account) markAllDirty() {
	fs, err := a.st.Folders()
	if err != nil {
		return
	}
	ids := make([]int64, 0, len(fs))
	for _, f := range fs {
		ids = append(ids, f.ID)
	}
	a.markDirty(ids...)
}

// markDirtyServerIDs is markDirty for EAS folder ids (from Ping).
func (a *Account) markDirtyServerIDs(sids []string) {
	var ids []int64
	for _, sid := range sids {
		if f, err := a.st.FolderByServerID(sid); err == nil {
			ids = append(ids, f.ID)
		}
	}
	a.markDirty(ids...)
}

func (a *Account) requestFolderSync() {
	a.wmu.Lock()
	a.wantFolderSync = true
	a.wmu.Unlock()
	a.poke()
}

// signalClean wakes goroutines waiting in waitSynced.
func (a *Account) signalClean() {
	a.wmu.Lock()
	close(a.cleanCh)
	a.cleanCh = make(chan struct{})
	a.wmu.Unlock()
}

// waitSynced blocks until none of ids is dirty, the timeout passes or the
// account stops.
func (a *Account) waitSynced(ids []int64, timeout time.Duration) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		a.wmu.Lock()
		busy := a.wantFolderSync
		for _, id := range ids {
			if fs := a.fs[id]; fs != nil && fs.dirty {
				busy = true
			}
		}
		ch := a.cleanCh
		a.wmu.Unlock()
		if !busy {
			return
		}
		select {
		case <-ch:
		case <-deadline.C:
			return
		case <-a.ctx.Done():
			return
		}
	}
}

// submit queues fn on the worker (ahead of sync work) and waits for it.
func (a *Account) submit(ctx context.Context, fn func(ctx context.Context) error) error {
	if a.stopped.Load() {
		return ErrUnavailable
	}
	if a.wiped.Load() {
		return ErrWiped
	}
	if a.authFailed.Load() {
		return ErrAuthFailed
	}
	if a.brk.remaining() > 0 {
		until, reason, _ := a.brk.state()
		return &pausedError{until: until, reason: reason}
	}
	r := &request{ctx: ctx, run: fn, done: make(chan error, 1)}
	a.wmu.Lock()
	a.reqs = append(a.reqs, r)
	a.wmu.Unlock()
	a.poke()
	select {
	case err := <-r.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-a.ctx.Done():
		return ErrUnavailable
	}
}

// Worker ---------------------------------------------------------------------

func (a *Account) workerLoop() {
	for {
		if a.ctx.Err() != nil {
			a.failRequests()
			return
		}
		if a.step() {
			continue
		}
		a.sleep()
	}
}

func (a *Account) failRequests() {
	a.wmu.Lock()
	reqs := a.reqs
	a.reqs = nil
	a.wmu.Unlock()
	for _, r := range reqs {
		r.done <- ErrUnavailable
	}
}

// step does one unit of work and reports whether it did any.
func (a *Account) step() (did bool) {
	defer func() {
		if r := recover(); r != nil {
			a.log.Error("worker panic", "panic", r)
			did = false
			time.Sleep(time.Second)
		}
	}()
	now := a.cfg.Clock()

	// 1. Requests from IMAP.
	a.wmu.Lock()
	var req *request
	if len(a.reqs) > 0 {
		req, a.reqs = a.reqs[0], a.reqs[1:]
	}
	a.wmu.Unlock()
	if req != nil {
		if err := req.ctx.Err(); err != nil {
			req.done <- err
		} else {
			req.done <- req.run(req.ctx)
		}
		return true
	}
	if a.authFailed.Load() || a.wiped.Load() {
		return false
	}
	if a.brk.remaining() > 0 {
		// The breaker is open: nothing may go to Exchange. Only local upkeep runs.
		a.wmu.Lock()
		due := !now.Before(a.nextHousekeep)
		a.wmu.Unlock()
		if due {
			a.housekeeping(now)
			return true
		}
		return false
	}

	// 2. Folder hierarchy.
	a.wmu.Lock()
	doFolders := a.wantFolderSync && !now.Before(a.folderRetryAt)
	a.wmu.Unlock()
	if doFolders {
		a.doFolderSync()
		return true
	}

	// 3. Local changes waiting to be sent.
	if a.flushOutboxStep(now) {
		return true
	}

	// 4. One Sync batch for the most urgent dirty folder.
	if id, ok := a.nextDirty(now); ok {
		a.syncBatch(id)
		return true
	}

	// 4b. One Sync batch for a Contacts folder that is due (after the mail).
	if a.contactsStep(now) {
		return true
	}

	// 5. Maintenance.
	a.wmu.Lock()
	due := !now.Before(a.nextHousekeep)
	a.wmu.Unlock()
	if due {
		a.housekeeping(now)
		return true
	}
	return false
}

// nextDirty picks the dirty folder to sync next.
func (a *Account) nextDirty(now time.Time) (int64, bool) {
	a.wmu.Lock()
	defer a.wmu.Unlock()
	type cand struct {
		id   int64
		prio int
	}
	var cs []cand
	for id, fs := range a.fs {
		if fs.dirty && !now.Before(fs.retryAt) {
			cs = append(cs, cand{id, fs.prio})
		}
	}
	if len(cs) == 0 {
		return 0, false
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].prio != cs[j].prio {
			return cs[i].prio < cs[j].prio
		}
		return cs[i].id < cs[j].id
	})
	return cs[0].id, true
}

// sleep waits until there is something to do.
func (a *Account) sleep() {
	a.wmu.Lock()
	next := a.nextHousekeep
	consider := func(t time.Time) {
		if !t.IsZero() && t.Before(next) {
			next = t
		}
	}
	if a.wantFolderSync {
		consider(a.folderRetryAt)
	}
	for _, fs := range a.fs {
		if fs.dirty {
			consider(fs.retryAt)
		}
	}
	consider(a.outboxRetryAt)
	for _, cs := range a.cstate {
		consider(cs.wakeAt())
	}
	if until, _, _ := a.brk.state(); !until.IsZero() {
		consider(until) // the breaker is open: wake when it closes
	}
	a.wmu.Unlock()
	d := next.Sub(a.cfg.Clock())
	if d < time.Millisecond {
		d = time.Millisecond
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-a.wake:
	case <-t.C:
	case <-a.ctx.Done():
	}
}

// callCtx returns a context for one bounded upstream call.
func (a *Account) callCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(a.ctx, a.cfg.CallTimeout)
}

// handleUpstreamError reacts to the errors every upstream call can return.
// It reports whether err was one of the account-wide conditions (auth failure,
// wipe, shutdown), in which case the caller must stop what it is doing.
func (a *Account) handleUpstreamError(err error) (fatal bool) {
	switch {
	case err == nil:
		return false
	case errors.Is(err, upstream.ErrAuthFailed):
		return a.onAuthFailed(err)
	case errors.Is(err, upstream.ErrAuthLike):
		return a.onAuthLike(err)
	case errors.Is(err, upstream.ErrRemoteWipe):
		go a.m.wipe(a, errors.Is(err, upstream.ErrAccountOnlyWipe)) // wipe stops this account; it must not run on its own worker
		return true
	case a.ctx.Err() != nil:
		return true
	}
	return false
}

// onAuthFailed stops all Exchange traffic after a 401. Nothing is retried
// automatically: repeated bad passwords lock the Active Directory account.
//
// The failure belongs to the credential the failing request used (the upstream
// session tags it, see sessionAuthError). If the password has been replaced
// since (a new login swapped the session), a late 401 from a request that still
// used the old one says nothing about the new one and is ignored: it must
// neither stop the engine nor make the bridge forget the valid password. It
// reports whether the caller must stop (the failure is about the current
// credential).
func (a *Account) onAuthFailed(err error) (fatal bool) {
	a.upMu.RLock()
	cur := a.sum
	sum := cur
	var sae *sessionAuthError
	if errors.As(err, &sae) {
		sum = sae.sum
	}
	if sum != cur {
		a.upMu.RUnlock()
		a.log.Info("ignoring a late 401 for a credential that has been replaced since")
		return false
	}
	already := a.authFailed.Swap(true) // under the lock: a swap of the session cannot slip in between
	a.upMu.RUnlock()
	if already {
		return true
	}
	a.log.Error("Exchange rejected the credentials (401); all upstream traffic stops until a client logs in with a working password")
	a.m.credentialRejected(a.user, sum)
	a.signalClean()
	return true
}

// authLikeLimit is how many answers that look like a refused credential (403, an unfollowed redirect,
// an HTML logon page) in a row stop the engine, as a 401 does at once. Each one is counted against the
// user's budget of unsuccessful verifications.
const authLikeLimit = 2

// onAuthLike handles an answer that looks like a refused credential during a session. The first is
// counted (budget and cooldown bookkeeping, as for an unverified login) and retried with the usual
// back-off; the second in a row stops all Exchange traffic of the mailbox like a 401 and drops the cached
// verification, so that the next login goes through the guarded login path. 404 and transport errors are
// not handled here: they stay retryable with the back-off.
func (a *Account) onAuthLike(err error) (fatal bool) {
	a.upMu.RLock()
	cur := a.sum
	sum := cur
	var sae *sessionAuthLikeError
	if errors.As(err, &sae) {
		sum = sae.sum
	}
	a.upMu.RUnlock()
	if sum != cur {
		a.log.Info("ignoring a late 403-like answer for a credential that has been replaced since")
		return false
	}
	if a.authFailed.Load() {
		return true
	}
	n := int(a.authLike.Add(1))
	a.m.authLikeFailure(a.user, sum, n)
	if n < authLikeLimit {
		return false
	}
	a.upMu.RLock()
	stale := a.sum != sum
	already := false
	if !stale {
		already = a.authFailed.Swap(true)
	}
	a.upMu.RUnlock()
	if stale || already {
		return !stale
	}
	a.log.Error("Exchange, or a gateway in front of it, keeps refusing the credential (403, a redirect or a logon page); all upstream traffic stops until a client logs in again")
	a.m.auth.ForgetIfSum(a.user, sum)
	a.signalClean()
	return true
}

// AuthFailed reports whether Exchange rejected the stored credentials.
func (a *Account) AuthFailed() bool { return a.authFailed.Load() }

// Wiped reports whether a remote wipe was carried out.
func (a *Account) Wiped() bool { return a.wiped.Load() }

// backoff registers a failure of the given kind and returns when to retry.
func (a *Account) backoff(n int) time.Time { return a.cfg.Clock().Add(a.cfg.Backoff(n)) }
