//go:build devfake

package engine_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"easbridge/internal/engine"
	"easbridge/internal/fakeupstream"
	"easbridge/internal/logx"
	"easbridge/internal/store"
)

const (
	testUser = "alice@example.test"
	testPass = "pw-one-correct-horse-battery"
)

// fakeClock runs at real speed from a fixed start and can be pushed forward.
type fakeClock struct {
	mu     sync.Mutex
	start  time.Time
	real   time.Time
	offset time.Duration
}

func newFakeClock(start time.Time) *fakeClock { return &fakeClock{start: start, real: time.Now()} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.start.Add(time.Since(c.real) + c.offset)
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.offset += d
	c.mu.Unlock()
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type env struct {
	t     *testing.T
	srv   *fakeupstream.Server
	db    *store.DB
	mgr   *engine.Manager
	clock *fakeClock
	logs  *syncBuffer
	dir   string
}

// newEnv builds a manager on a fake Exchange with fast timers. Only
// allow-listed users can log in (testUser).
func newEnv(t *testing.T, mods ...func(*engine.Config, *fakeupstream.Server)) *env {
	t.Helper()
	logx.ResetSecretsForTest()
	e := &env{t: t, srv: fakeupstream.NewServer(), clock: newFakeClock(time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)),
		logs: &syncBuffer{}}
	e.srv.AddUser(testUser, testPass)
	e.srv.SetClock(e.clock.Now) // one clock for the server and the engine: nothing depends on the time of day
	cfg := engine.Config{
		Grace:             time.Hour,
		PingHeartbeat:     300 * time.Millisecond,
		PingMinHeartbeat:  40 * time.Millisecond,
		AuthFailCooldown:  15 * time.Minute,
		MaxMessageBytes:   1 << 20,
		PlaceholderTTL:    24 * time.Hour,
		PlaceholderWindow: 10 * time.Minute,
		ReconVerifyDelay:  200 * time.Millisecond,
		FetchWorkers:      2,
		OutboxBatch:       100,
		Housekeeping:      30 * time.Millisecond,
		Backoff:           func(int) time.Duration { return 15 * time.Millisecond },
		Clock:             e.clock.Now,
		CallTimeout:       10 * time.Second,
		// The circuit breaker pauses for microseconds (in effect not at all), so
		// that the many tests that make Exchange fail on purpose do not wait or
		// race with a pause; breaker_test.go sets real values.
		BreakerBase: time.Microsecond,
		BreakerMax:  4 * time.Microsecond,
	}
	inline := 4096
	for _, m := range mods {
		m(&cfg, e.srv)
	}
	_ = inline
	e.dir = t.TempDir()
	db, err := store.Open(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	db.SetClock(e.clock.Now)
	e.db = db
	log := logx.New(logx.Options{Level: slog.LevelDebug, Writer: e.logs})
	e.mgr = engine.NewManager(cfg, log, db, e.srv.Factory(inline), func(u string) bool { return u == testUser })
	t.Cleanup(func() {
		e.mgr.Close()
		e.mgr.DB().Close()
		if t.Failed() {
			t.Logf("engine log:\n%s", e.logs.String())
		}
	})
	t.Cleanup(e.srv.ReleaseAll) // runs first: no call may stay held while the manager shuts down
	return e
}

func (e *env) login() *engine.Account {
	e.t.Helper()
	a, err := e.mgr.Login(context.Background(), testUser, testPass)
	if err != nil {
		e.t.Fatalf("login: %v", err)
	}
	return a
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(3 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// consistently asserts cond stays true for a short while.
func consistently(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if !cond() {
			t.Fatalf("%s did not hold", what)
		}
		time.Sleep(3 * time.Millisecond)
	}
}

func (e *env) folder(a *engine.Account, path string) *store.Folder {
	e.t.Helper()
	f, err := a.Store().FolderByPath(path)
	if err != nil {
		e.t.Fatalf("folder %q: %v", path, err)
	}
	return f
}

// serverCount is the number of server-bound messages stored for a folder.
func serverCount(a *engine.Account, folderID int64) int {
	ms, err := a.Store().ListMessages(folderID)
	if err != nil {
		return -1
	}
	n := 0
	for _, m := range ms {
		if m.Kind == store.KindServer {
			n++
		}
	}
	return n
}

// mirrored waits until the local copy of a folder holds every server item.
func (e *env) mirrored(a *engine.Account, serverName, path string) *store.Folder {
	e.t.Helper()
	sid := e.srv.FolderID(testUser, serverName)
	if sid == "" {
		e.t.Fatalf("no server folder %q", serverName)
	}
	var f *store.Folder
	eventually(e.t, fmt.Sprintf("folder %s mirrored", path), func() bool {
		var err error
		f, err = a.Store().FolderByPath(path)
		if err != nil {
			return false
		}
		return f.InitialComplete && serverCount(a, f.ID) == len(e.srv.Items(testUser, sid)) && f.SyncKey != "0"
	})
	return f
}

// waitBulk waits until the bulk announcement of a folder's first sync has been
// delivered to the recorder: the database shows the folder as open from the
// moment the transaction commits, a hair before the listeners hear about it.
func waitBulk(t *testing.T, rec *recorder) {
	t.Helper()
	eventually(t, "the bulk announcement is delivered", func() bool { return rec.count(store.EvBulkAdd) >= 1 })
}

// progress returns the first-sync state of a folder.
func (e *env) progress(a *engine.Account, folderID int64) store.FolderProgress {
	e.t.Helper()
	ps, err := a.Store().InitialProgress()
	if err != nil {
		e.t.Fatal(err)
	}
	for _, p := range ps {
		if p.FolderID == folderID {
			return p
		}
	}
	e.t.Fatalf("no progress for folder %d", folderID)
	return store.FolderProgress{}
}

// mailBytes builds a small RFC 5322 message.
func mailBytes(id, subject string, date time.Time, extra string) []byte {
	return []byte("Message-ID: <" + id + ">\r\nFrom: Bob <bob@example.test>\r\nTo: alice@example.test\r\nSubject: " + subject +
		"\r\nDate: " + date.Format("Mon, 02 Jan 2006 15:04:05 -0700") + "\r\n" + extra +
		"MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nbody of " + id + "\r\n")
}

// recorder collects the events of one folder.
type recorder struct {
	mu     sync.Mutex
	events []store.Event
}

func (r *recorder) add(ev store.Event) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
}

func (r *recorder) all() []store.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]store.Event(nil), r.events...)
}

func (r *recorder) count(k store.EventKind) int {
	n := 0
	for _, ev := range r.all() {
		if ev.Kind == k {
			n++
		}
	}
	return n
}

func (r *recorder) uids(k store.EventKind) []uint32 {
	var out []uint32
	for _, ev := range r.all() {
		if ev.Kind == k {
			out = append(out, ev.UID)
		}
	}
	return out
}

func (e *env) watch(a *engine.Account, f *store.Folder) (*recorder, []store.MsgMeta) {
	e.t.Helper()
	r := &recorder{}
	snap, cancel, err := a.Watch(f.ID, r.add)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(cancel)
	return r, snap
}

func uidsOf(ms []store.MsgMeta) []uint32 {
	var out []uint32
	for _, m := range ms {
		out = append(out, m.UID)
	}
	return out
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
