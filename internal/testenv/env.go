//go:build devfake

package testenv

import (
	"bytes"
	"log/slog"
	"sync"
	"testing"
	"time"

	"easbridge/internal/engine"
	"easbridge/internal/fakeupstream"
	"easbridge/internal/logx"
	"easbridge/internal/store"
)

// Credentials of the one allow-listed test user.
const (
	User = "alice@example.test"
	Pass = "pw-one-correct-horse-battery"
)

// Clock runs at real speed from a fixed start and can be pushed forward.
type Clock struct {
	mu     sync.Mutex
	start  time.Time
	real   time.Time
	offset time.Duration
}

// NewClock starts a clock at start.
func NewClock(start time.Time) *Clock { return &Clock{start: start, real: time.Now()} }

// Now returns the current fake time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.start.Add(time.Since(c.real) + c.offset)
}

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.offset += d
	c.mu.Unlock()
}

// Buffer is a goroutine-safe log sink.
type Buffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *Buffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

// String returns everything logged so far.
func (s *Buffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// Env is the backend under test.
type Env struct {
	T     *testing.T
	Srv   *fakeupstream.Server
	DB    *store.DB
	Mgr   *engine.Manager
	Clock *Clock
	Logs  *Buffer
	Log   *slog.Logger
	Dir   string
	Cfg   engine.Config
}

// New builds a manager on a fake Exchange with fast timers. Only User can log
// in. inlineMax is the MIME size above which the fake truncates Sync bodies.
func New(t *testing.T, inlineMax int, mods ...func(*engine.Config, *fakeupstream.Server)) *Env {
	t.Helper()
	logx.ResetSecretsForTest()
	e := &Env{T: t, Srv: fakeupstream.NewServer(), Clock: NewClock(time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)), Logs: &Buffer{}}
	e.Srv.AddUser(User, Pass)
	e.Srv.SetClock(e.Clock.Now) // one clock for the server and the engine: nothing depends on the time of day
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
		Clock:             e.Clock.Now,
		CallTimeout:       10 * time.Second,
		// The circuit breaker pauses for microseconds (in effect not at all), so
		// that the many tests that make Exchange fail on purpose do not wait or
		// race with a pause; the tests of the breaker itself set real values.
		BreakerBase: time.Microsecond,
		BreakerMax:  4 * time.Microsecond,
	}
	for _, m := range mods {
		m(&cfg, e.Srv)
	}
	e.Cfg = cfg
	e.Dir = t.TempDir()
	db, err := store.Open(e.Dir)
	if err != nil {
		t.Fatal(err)
	}
	db.SetClock(e.Clock.Now)
	e.DB = db
	e.Log = logx.New(logx.Options{Level: slog.LevelDebug, Writer: e.Logs})
	e.Mgr = engine.NewManager(cfg, e.Log, db, e.Srv.Factory(inlineMax), func(u string) bool { return u == User })
	t.Cleanup(func() {
		e.Mgr.Close()
		e.Mgr.DB().Close()
		if t.Failed() {
			t.Logf("engine log:\n%s", e.Logs.String())
		}
	})
	t.Cleanup(e.Srv.ReleaseAll) // runs first: no call may stay held while the manager shuts down
	return e
}

// Eventually polls cond for up to five seconds.
func Eventually(t *testing.T, what string, cond func() bool) {
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

// Mail builds a small RFC 5322 message.
func Mail(id, subject string, date time.Time, extra string) []byte {
	return []byte("Message-ID: <" + id + ">\r\nFrom: Bob <bob@example.test>\r\nTo: alice@example.test\r\nSubject: " + subject +
		"\r\nDate: " + date.Format("Mon, 02 Jan 2006 15:04:05 -0700") + "\r\n" + extra +
		"MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nbody of " + id + "\r\n")
}

// ServerCount is the number of server-bound messages stored for a folder.
func ServerCount(a *engine.Account, folderID int64) int {
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

// Mirrored waits until the local copy of a folder holds every item the fake
// Exchange has in the named server folder.
func (e *Env) Mirrored(a *engine.Account, serverName, path string) *store.Folder {
	e.T.Helper()
	sid := e.Srv.FolderID(User, serverName)
	if sid == "" {
		e.T.Fatalf("no server folder %q", serverName)
	}
	var f *store.Folder
	Eventually(e.T, "folder "+path+" mirrored", func() bool {
		var err error
		f, err = a.Store().FolderByPath(path)
		if err != nil {
			return false
		}
		return f.InitialComplete && ServerCount(a, f.ID) == len(e.Srv.Items(User, sid)) && f.SyncKey != "0"
	})
	return f
}
