package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"easbridge/internal/auth"
	"easbridge/internal/logx"
	"easbridge/internal/store"
	"easbridge/internal/upstream"
)

// Manager owns the engines (one per mailbox) and decides who may log in.
//
// Login policy (DESIGN section 3):
//
//  1. Only allow-listed users are ever forwarded to Exchange.
//  2. A credential Exchange already accepted in this process is accepted again
//     without contacting Exchange (salted digest, constant-time compare).
//  3. A credential whose verification failed, in any way (a 401, a gateway's
//     403 or logon page, a timeout), is refused locally for a cooldown that
//     grows when the same credential fails again: 15 minutes, 1 hour, 4 hours,
//     24 hours. A refusal after a 401 answers like a wrong password; after any
//     other failure it answers NO [UNAVAILABLE], because the password may be
//     right.
//  4. Anything else costs exactly one verification (OPTIONS + Provision), and
//     concurrent logins for the same user share it. Every user has a budget of
//     two unsuccessful verifications per 30 minutes; beyond it unverified logins
//     are refused locally (NO [UNAVAILABLE]) until the window moves on, so that
//     guessing passwords cannot lock the Active Directory account.
//  5. Exchange being unreachable never counts as a wrong password.
type Manager struct {
	cfg     Config
	log     *slog.Logger
	dbp     atomic.Pointer[store.DB]
	factory upstream.Factory
	allowed func(string) bool
	auth    *auth.Cache
	dataDir string

	mu       sync.Mutex
	accounts map[string]*Account
	locks    map[string]*userLock
	sources  *sourceLimiter

	destroyHook atomic.Pointer[func(*store.DB) error]

	wiping   atomic.Bool
	wipeMu   sync.Mutex // held while a wipe runs; Close waits for it
	lastWipe time.Time  // when the last wipe finished (guarded by wipeMu)
	closed   atomic.Bool
	stopReap chan struct{}
	reapDone chan struct{}
}

// NewManager creates a manager. allowed reports whether a (lower-cased) login
// name is on the allow-list.
func NewManager(cfg Config, log *slog.Logger, db *store.DB, f upstream.Factory, allowed func(string) bool) *Manager {
	cfg = cfg.withDefaults()
	m := &Manager{
		cfg: cfg, log: log, factory: f, allowed: allowed,
		auth:     auth.New(),
		dataDir:  db.Dir(),
		accounts: map[string]*Account{},
		locks:    map[string]*userLock{},
		sources:  newSourceLimiter(cfg.SourceNames, cfg.SourceWindow),
		stopReap: make(chan struct{}),
		reapDone: make(chan struct{}),
	}
	m.auth.SetClock(cfg.Clock)
	m.auth.SetLimits(0, 0, cfg.LoginWindow)
	m.dbp.Store(db)
	if cfg.Directory.Enabled() {
		db.SetDirectoryDomains(cfg.Directory.Domains)
	}
	go m.reaper()
	return m
}

// SetDestroyHook replaces what a remote wipe does to the state database (Destroy by
// default). Tests use it to see what the wipe has removed by itself when destroying the
// database fails. nil restores the default.
func (m *Manager) SetDestroyHook(f func(*store.DB) error) {
	if f == nil {
		m.destroyHook.Store(nil)
		return
	}
	m.destroyHook.Store(&f)
}

// DB returns the current state database (it is replaced by an empty one after a
// remote wipe).
func (m *Manager) DB() *store.DB { return m.dbp.Load() }

func (m *Manager) acctTag(user string) string {
	h := sha256.Sum256([]byte(strings.ToLower(user)))
	return hex.EncodeToString(h[:4])
}

// userLock serialises the logins of one user. The lock exists only while somebody holds or waits for it,
// so a client that invents names cannot grow the map.
type userLock struct {
	sync.Mutex
	refs int
}

// lockUser returns with the user's lock held; the returned function unlocks it and forgets it when
// nobody else wants it.
func (m *Manager) lockUser(user string) (unlock func()) {
	m.mu.Lock()
	l := m.locks[user]
	if l == nil {
		l = &userLock{}
		m.locks[user] = l
	}
	l.refs++
	m.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		m.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(m.locks, user)
		}
		m.mu.Unlock()
	}
}

func (m *Manager) running(user string) *Account {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a := m.accounts[user]; a != nil && !a.Stopped() && !a.Wiped() {
		return a
	}
	return nil
}

// BookkeepingSizes is the size of the in-memory login bookkeeping, for tests and diagnostics: the failure cache,
// the per-user records, the per-user locks and the sources with unverified names. All are bounded.
func (m *Manager) BookkeepingSizes() (failed, users, locks, sources int) {
	_, failed = m.auth.Sizes()
	users = m.auth.UserEntries()
	m.mu.Lock()
	locks = len(m.locks)
	m.mu.Unlock()
	return failed, users, locks, m.sources.size()
}

// Wiped reports whether logins are refused because of a remote wipe: a wipe is
// in progress or the WIPED marker file still exists.
func (m *Manager) Wiped() bool {
	return m.wiping.Load() || store.WipeMarkerExists(m.dataDir)
}

// WaitHierarchy waits, up to timeout, until the running engine of user knows the
// mailbox's folder hierarchy (the folder list, which the contact folders come with). It
// reports whether it is known; without a running engine it returns false at once.
func (m *Manager) WaitHierarchy(ctx context.Context, user string, timeout time.Duration) bool {
	a := m.running(strings.ToLower(strings.TrimSpace(user)))
	if a == nil {
		return false
	}
	return a.WaitHierarchy(ctx, timeout)
}

// Login authenticates a client-supplied credential and returns the user's
// engine. The password is used for the Exchange connection and held in memory
// by the upstream session; it is never stored.
func (m *Manager) Login(ctx context.Context, user, pass string) (*Account, error) {
	if m.closed.Load() {
		return nil, ErrUnavailable
	}
	if m.Wiped() {
		return nil, ErrWiped
	}
	user = strings.ToLower(strings.TrimSpace(user))
	if user == "" || pass == "" || !m.allowed(user) {
		return nil, ErrBadCredentials // never forwarded: the bridge is no password oracle
	}
	// Whatever a client sends for an allowed name must never reach a log line. (A name that is not allowed is
	// refused above and never logged or processed, so its password needs no place in the scrubber.)
	logx.RegisterCredential(user, pass)
	if a := m.running(user); a != nil && m.auth.IsVerified(user, pass) {
		return a, nil
	}
	if until, kind, bad := m.auth.FailedUntilKind(user, pass); bad {
		return nil, m.refuseFailed(user, until, kind)
	}

	unlock := m.lockUser(user)
	defer unlock()
	// Another login for the same user may have finished while we waited.
	if m.Wiped() {
		return nil, ErrWiped
	}
	if a := m.running(user); a != nil && m.auth.IsVerified(user, pass) {
		return a, nil
	}
	if until, kind, bad := m.auth.FailedUntilKind(user, pass); bad {
		return nil, m.refuseFailed(user, until, kind)
	}
	if ok, retryAt := m.auth.AllowAttempt(user, m.cfg.LoginAttempts, m.cfg.LoginWindow); !ok {
		m.log.Warn("login refused locally: too many unsuccessful verifications; Exchange is not asked again until the window moves on",
			"acct", m.acctTag(user), "reason", "budget", "attempts", m.cfg.LoginAttempts,
			"window", m.cfg.LoginWindow.String(), "retry_at", retryAt.UTC().Format(time.RFC3339))
		return nil, &ThrottledError{Until: retryAt, Reason: "too many unsuccessful logins"}
	}
	// With a domain entry in the allow-list any name of the domain gets this far: one source may try only a few
	// distinct names that have not been verified.
	src := sourceOf(ctx)
	if m.cfg.DomainEntries {
		if ok, retryAt := m.sources.allow(src, user, m.cfg.Clock()); !ok {
			m.log.Warn("login refused locally: this source has tried too many names that were not verified; Exchange is not asked",
				"reason", "source", "names", m.sources.names, "window", m.sources.window.String(), "retry_at", retryAt.UTC().Format(time.RFC3339))
			return nil, &ThrottledError{Until: retryAt, Reason: "too many unverified names from one address"}
		}
	}
	a, err := m.verify(ctx, user, pass)
	if err == nil && m.cfg.DomainEntries {
		m.sources.verified(src, user)
	}
	return a, err
}

// isTransient reports whether a failed verification was no answer from the server at all (see
// upstream.ErrTransient) or a request to slow down (429, 5xx): not a refusal of the credential.
func isTransient(err error) bool {
	var te *upstream.ThrottleError
	return errors.As(err, &te) || errors.Is(err, upstream.ErrTransient)
}

// refuseFailed answers a login whose credential failed a verification recently
// (one log line per refusal, never the password).
func (m *Manager) refuseFailed(user string, until time.Time, kind auth.FailKind) error {
	reason := "the credential was rejected by Exchange"
	if kind == auth.FailUnverified {
		reason = "an earlier verification with this credential failed"
	}
	m.log.Warn("login refused locally: "+reason, "acct", m.acctTag(user), "reason", "cooldown", "until", until.UTC().Format(time.RFC3339))
	if kind == auth.FailRejected {
		return ErrBadCredentials
	}
	return &ThrottledError{Until: until, Reason: reason}
}

func (m *Manager) verify(ctx context.Context, user, pass string) (*Account, error) {
	devID, err := m.DB().DeviceID()
	if err != nil {
		return nil, fmt.Errorf("%w: state database: %v", ErrUnavailable, err)
	}
	// The account's row is made only after Exchange accepted the login: a name that was never verified
	// leaves nothing in the database.
	policy, _ := m.DB().PolicyKeyOf(user)
	up, err := m.factory.New(upstream.Config{User: user, Password: pass, DeviceID: devID, PolicyKey: policy})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	octx, cancel := context.WithTimeout(ctx, 90*time.Second)
	opened := time.Now()
	err = up.Open(octx)
	took := time.Since(opened)
	cancel()
	switch {
	case err == nil:
	case errors.Is(err, upstream.ErrAuthFailed):
		up.Close()
		cooldown, streak := m.auth.RecordFailure(user, m.auth.Digest(user, pass), auth.FailRejected, m.cfg.AuthFailCooldown)
		m.log.Warn("Exchange rejected a login; the same credential is refused locally for the cooldown",
			"acct", m.acctTag(user), "cooldown", cooldown.String(), "failures_in_a_row", streak)
		return nil, ErrBadCredentials
	case errors.Is(err, upstream.ErrRemoteWipe):
		go m.wipeCore(nil, up, errors.Is(err, upstream.ErrAccountOnlyWipe))
		return nil, ErrWiped
	case errors.Is(err, context.Canceled), errors.Is(err, upstream.ErrNotSent):
		// The client went away while Exchange was being asked, or the request
		// never left the machine (no connection, DNS, TLS handshake): the
		// credential was not presented to anyone, so this is no failed
		// verification and costs neither budget nor cooldown.
		up.Close()
		m.log.Warn("cannot reach Exchange to verify a login", "acct", m.acctTag(user), "err", err)
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	case isTransient(err):
		// Exchange (or a gateway) did not answer: 429, a 5xx, a time-out or a dropped connection. That cannot
		// have been a refusal of the password, so no budget is used and the cooldown does not escalate; the same
		// credential waits a short fixed time so that a struggling server is not hammered.
		up.Close()
		m.auth.RecordTransient(m.auth.Digest(user, pass), m.cfg.TransientPause)
		m.log.Warn("Exchange did not answer a login verification (throttled, a server error or a time-out); the same credential waits briefly",
			"acct", m.acctTag(user), "pause", m.cfg.TransientPause.String(), "err", err)
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	default:
		up.Close()
		// A 403, a redirect or a logon page: a gateway answers a wrong password that way, and the attempt may
		// still have counted as a failed logon in Active Directory, so it is remembered like a rejection.
		cooldown, streak := m.auth.RecordFailure(user, m.auth.Digest(user, pass), auth.FailUnverified, m.cfg.AuthFailCooldown)
		m.log.Warn("cannot verify a login with Exchange; the same credential is refused locally for the cooldown",
			"acct", m.acctTag(user), "cooldown", cooldown.String(), "failures_in_a_row", streak, "err", err)
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}

	acct, err := m.DB().Account(user)
	if err != nil {
		up.Close()
		return nil, fmt.Errorf("%w: state database: %v", ErrUnavailable, err)
	}
	if pk := up.PolicyKey(); pk != policy {
		if _, err := acct.Update(nil, func(tx *store.Tx) error { return tx.SetPolicyKey(pk) }); err != nil {
			m.log.Error("storing the policy key failed", "err", err)
		}
	}
	logx.PinCredential(user, pass)
	sum := m.auth.MarkVerified(user, pass)

	m.mu.Lock()
	existing := m.accounts[user]
	if existing != nil && (existing.Stopped() || existing.Wiped()) {
		existing = nil
	}
	if existing != nil {
		m.mu.Unlock()
		existing.swapUpstream(up, sum)
		return existing, nil
	}
	a := newAccount(m, acct, up, sum)
	m.accounts[user] = a
	m.mu.Unlock()
	a.start()
	// How long Exchange took to verify the login is worth a field: the first real
	// Provision took 50 s, and a login that waits that long is otherwise a mystery.
	m.log.Info("mailbox engine started", "acct", m.acctTag(user), "verified_in", took.Round(time.Millisecond).String())
	return a, nil
}

// credentialRejected is called by an engine when Exchange answered 401 during a
// session: the credential is forgotten and refused for the cooldown.
func (m *Manager) credentialRejected(user string, sum auth.Sum) {
	m.auth.ForgetIfSum(user, sum)
	cooldown, streak := m.auth.RecordFailure(user, sum, auth.FailRejected, m.cfg.AuthFailCooldown)
	m.log.Warn("Exchange rejected a credential during a session; it is refused locally for the cooldown",
		"acct", m.acctTag(user), "cooldown", cooldown.String(), "failures_in_a_row", streak)
}

// authLikeFailure counts one answer that looks like a refused credential (403, a redirect that was not
// followed, a logon page) during a session against the user's budget of unsuccessful verifications, the
// same one an unverified login uses, and starts the escalating cooldown of that credential.
func (m *Manager) authLikeFailure(user string, sum auth.Sum, inARow int) {
	if inARow < authLikeLimit {
		m.auth.CountAttempt(user)
		m.log.Warn("Exchange, or a gateway in front of it, answered a request in a way that looks like a refused credential (403, redirect or logon page); counted, retried with the back-off",
			"acct", m.acctTag(user), "in_a_row", inARow)
		return
	}
	cooldown, streak := m.auth.RecordFailure(user, sum, auth.FailUnverified, m.cfg.AuthFailCooldown)
	m.log.Warn("Exchange, or a gateway in front of it, answered again in a way that looks like a refused credential; the credential is refused locally for the cooldown",
		"acct", m.acctTag(user), "in_a_row", inARow, "cooldown", cooldown.String(), "failures_in_a_row", streak)
}

// stopAccount stops an engine and forgets its verified credential.
func (m *Manager) stopAccount(a *Account) {
	m.mu.Lock()
	if m.accounts[a.user] == a {
		delete(m.accounts, a.user)
	}
	m.mu.Unlock()
	m.auth.ForgetVerified(a.user)
	a.closeSessions()
	a.Stop()
	m.log.Info("mailbox engine stopped", "acct", m.acctTag(a.user))
}

// reaper stops engines that have been without sessions for the grace period.
func (m *Manager) reaper() {
	defer close(m.reapDone)
	every := m.cfg.Grace / 10
	if every > time.Minute {
		every = time.Minute
	}
	if every < 5*time.Millisecond {
		every = 5 * time.Millisecond
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-m.stopReap:
			return
		case <-t.C:
		}
		now := m.cfg.Clock()
		var idle []*Account
		m.mu.Lock()
		for _, a := range m.accounts {
			if since := a.idleSince(); !since.IsZero() && now.Sub(since) >= m.cfg.Grace {
				idle = append(idle, a)
			}
		}
		m.mu.Unlock()
		for _, a := range idle {
			m.log.Info("no client for the grace period; stopping the mailbox engine", "acct", m.acctTag(a.user))
			m.stopAccount(a)
		}
	}
}

// Close stops every engine and the reaper. The database stays open.
func (m *Manager) Close() {
	if !m.closed.CompareAndSwap(false, true) {
		return
	}
	// A wipe in progress must finish first (it replaces the database).
	m.wipeMu.Lock()
	m.wipeMu.Unlock() //nolint:staticcheck // used as a barrier
	close(m.stopReap)
	<-m.reapDone
	m.mu.Lock()
	accts := make([]*Account, 0, len(m.accounts))
	for _, a := range m.accounts {
		accts = append(accts, a)
	}
	m.accounts = map[string]*Account{}
	m.mu.Unlock()
	for _, a := range accts {
		a.closeSessions()
		a.Stop()
	}
}

// AccountStatus is a content-free snapshot of one engine, for /healthz.
type AccountStatus struct {
	Tag        string `json:"acct"`
	AuthFailed bool   `json:"auth_failed"`
	Folders    int    `json:"folders"`
	Pending    int    `json:"pending_fetch"`
	Outbox     int    `json:"outbox"`
	Syncing    int    `json:"syncing"`
	Sessions   int    `json:"sessions"`
	LastSync   int64  `json:"last_sync_unix"`
	PingSecs   int    `json:"ping_heartbeat_s"`
	// The circuit breaker (breaker.go): whether all Exchange traffic of this
	// mailbox is paused right now, until when and why (no mailbox content), and
	// how many times it has opened since the engine started.
	Paused      bool   `json:"paused"`
	PausedUntil int64  `json:"paused_until_unix,omitempty"`
	PauseReason string `json:"pause_reason,omitempty"`
	PauseTrips  int    `json:"pause_trips"`
	// First-sync progress (counts only, no folder names): folders that have
	// finished and folders still downloading, and messages fetched but hidden
	// until their folder opens.
	InitialDone    int            `json:"initial_sync_complete"`
	InitialPending int            `json:"initial_sync_pending"`
	Staged         int            `json:"staged_items"`
	FolderProgress []FolderStatus `json:"folder_progress"`
	// Contacts is present when the contacts feature is on: counts only.
	Contacts *ContactsStatus `json:"contacts,omitempty"`
}

// ContactsStatus is the content-free state of the contacts feature for /healthz: whether
// the first download of each book is complete and how many cards it lists, the number of
// directory cards, the number of candidates waiting for a lookup, the searches sent today
// (UTC) and, while the directory is paused, until when.
type ContactsStatus struct {
	Books                 []BookStatus `json:"books"`
	DirectoryEnabled      bool         `json:"directory_enabled"`
	DirectoryCards        int          `json:"directory_cards"`
	DirectoryQueue        int          `json:"directory_queue"`
	DirectoryLookupsToday int          `json:"directory_lookups_today"`
	DirectoryPausedUntil  int64        `json:"directory_paused_until_unix,omitempty"`
	// Sweep is present when the opt-in full sweep is on.
	Sweep *SweepStatus `json:"directory_sweep,omitempty"`
}

// SweepStatus is the content-free state of the full sweep for /healthz: whether it is on, the
// calibrated query length, idle, running or waiting until a time, the prefixes done and
// queued, the searches and the entries of the sweep under way, and how the last one ended.
type SweepStatus struct {
	Full                  bool   `json:"full"`
	CalibratedLength      int    `json:"calibrated_length"`
	CalibrationFailed     bool   `json:"calibration_failed"`
	State                 string `json:"state"`
	WaitingUntilUnix      int64  `json:"waiting_until_unix,omitempty"`
	PrefixesDone          int    `json:"prefixes_done"`
	PrefixesQueued        int    `json:"prefixes_queued"`
	SearchesThisSweep     int    `json:"searches_this_sweep"`
	EntriesFoundThisSweep int    `json:"entries_found_this_sweep"`
	LastSweepUnix         int64  `json:"last_sweep_unix,omitempty"`
	LastSweepCapped       bool   `json:"last_sweep_capped"`
	// SweepInfeasible is set while the last sweep stopped itself because the directory answers
	// no query shorter than SweepInfeasibleLength (3 or more) characters.
	SweepInfeasible       bool `json:"sweep_infeasible"`
	SweepInfeasibleLength int  `json:"sweep_infeasible_length,omitempty"`
}

// BookStatus is the state of one contact book, in the order of the books.
type BookStatus struct {
	Complete    bool `json:"complete"`
	Downloading bool `json:"downloading"`
	Cards       int  `json:"cards"`
}

// FolderStatus is the first-sync state of one folder, in creation order.
type FolderStatus struct {
	InitialSyncComplete bool `json:"initial_sync_complete"`
	Staged              int  `json:"staged"`
	PendingFetch        int  `json:"pending_fetch"`
}

// Status describes the manager without revealing mailbox content.
type Status struct {
	Wiped    bool            `json:"wiped"`
	Accounts []AccountStatus `json:"accounts"`
}

// Status returns the current state for the health endpoint.
func (m *Manager) Status() Status {
	st := Status{Wiped: m.Wiped()}
	m.mu.Lock()
	accts := make([]*Account, 0, len(m.accounts))
	for _, a := range m.accounts {
		accts = append(accts, a)
	}
	m.mu.Unlock()
	if st.Wiped {
		return st
	}
	for _, a := range accts {
		as := AccountStatus{Tag: m.acctTag(a.user), AuthFailed: a.AuthFailed(), LastSync: a.lastSync.Load(),
			PingSecs: int(a.PingHeartbeat() / time.Second)}
		if until, reason, trips := a.brk.state(); !until.IsZero() {
			as.Paused, as.PausedUntil, as.PauseReason, as.PauseTrips = true, until.Unix(), reason, trips
		} else {
			as.PauseTrips = trips
		}
		if fs, err := a.st.Folders(); err == nil {
			as.Folders = len(fs)
		}
		as.Pending, _ = a.st.PendingCount(0)
		as.Outbox, _ = a.st.OutboxLen()
		as.Syncing = a.Syncing()
		if prog, err := a.st.InitialProgress(); err == nil {
			for _, p := range prog {
				if p.Complete {
					as.InitialDone++
				} else {
					as.InitialPending++
				}
				as.Staged += p.Staged
				as.FolderProgress = append(as.FolderProgress, FolderStatus{InitialSyncComplete: p.Complete, Staged: p.Staged, PendingFetch: p.PendingFetch})
			}
		}
		a.smu.Lock()
		as.Sessions = len(a.sessions)
		a.smu.Unlock()
		if m.cfg.ContactsEnabled {
			as.Contacts = a.contactsStatus()
		}
		st.Accounts = append(st.Accounts, as)
	}
	return st
}

// Wipe ------------------------------------------------------------------------

// wipe carries out an Exchange remote wipe request that arrived on account a.
func (m *Manager) wipe(a *Account, accountOnly bool) { m.wipeCore(a, nil, accountOnly) }

// wipeCore deletes the local mail cache, refuses further logins and
// acknowledges the wipe (DESIGN section 3). from is the account that saw the
// request (nil if it arrived while verifying a login, then ack is the session
// to acknowledge on).
//
// accountOnly is the Exchange 16.1 variant that asks for this account's data to
// go and nothing else on the device. The bridge keeps nothing but the mailbox
// caches, so it does the same as for a full wipe (the whole state database, the
// WIPED marker) and only the acknowledgement differs.
func (m *Manager) wipeCore(from *Account, ack upstream.Mailbox, accountOnly bool) {
	reqAt := time.Now()
	m.wipeMu.Lock()
	defer m.wipeMu.Unlock()
	if !m.lastWipe.IsZero() && reqAt.Before(m.lastWipe) {
		return // a duplicate report that arrived while the wipe ran
	}
	if from != nil && from.wiped.Load() {
		return
	}
	m.wiping.Store(true)
	defer func() {
		m.wiping.Store(false)
		m.lastWipe = time.Now()
	}()
	what := "REMOTE WIPE REQUESTED BY EXCHANGE"
	if accountOnly {
		what = "REMOTE WIPE (ACCOUNT ONLY) REQUESTED BY EXCHANGE"
	}
	m.log.Error(what + ": deleting the local mail cache and refusing all logins until the WIPED marker file in the data directory is removed")
	// Marker first: even if we crash halfway, logins stay refused.
	if err := store.WriteWipeMarker(m.dataDir, m.cfg.Clock()); err != nil {
		m.log.Error("cannot write the WIPED marker", "err", err)
	}
	m.mu.Lock()
	accts := make([]*Account, 0, len(m.accounts))
	for _, a := range m.accounts {
		accts = append(accts, a)
	}
	m.accounts = map[string]*Account{}
	m.mu.Unlock()
	for _, a := range accts {
		a.wiped.Store(true)
		a.closeSessions()
		a.stopWorkers()
	}
	// The contacts, the directory cards and the candidates go first, explicitly, with the
	// secure delete of the database: they are the account's personal data, and they must
	// be gone even if destroying the database files below fails.
	for _, a := range accts {
		if _, err := a.st.Update(nil, func(tx *store.Tx) error { return tx.DeleteContactData() }); err != nil {
			m.log.Error("deleting the contacts of a wiped mailbox failed", "err", err)
		}
	}
	if from != nil {
		ack = from.upstream()
	}
	if ack != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := ack.AcknowledgeWipe(ctx, accountOnly); err != nil {
			m.log.Error("could not acknowledge the wipe to Exchange", "err", err)
		}
		cancel()
		ack.Close()
	}
	for _, a := range accts {
		if up := a.upstream(); up != ack {
			up.Close()
		}
		a.stopped.Store(true)
	}
	destroy := (*store.DB).Destroy
	if h := m.destroyHook.Load(); h != nil {
		destroy = *h
	}
	if err := destroy(m.DB()); err != nil {
		m.log.Error("deleting the state database failed", "err", err)
	}
	m.auth.Reset()
	// Start over with an empty database (and a new DeviceId); logins stay
	// refused until the operator removes the WIPED marker.
	ndb, err := store.Open(m.dataDir)
	if err != nil {
		m.log.Error("cannot create a fresh state database after the wipe", "err", err)
		return
	}
	if m.cfg.Directory.Enabled() {
		ndb.SetDirectoryDomains(m.cfg.Directory.Domains)
	}
	m.dbp.Store(ndb)
}
