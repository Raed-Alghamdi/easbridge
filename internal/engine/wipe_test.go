//go:build devfake

package engine_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"easbridge/internal/engine"
	"easbridge/internal/fakeupstream"
	"easbridge/internal/store"
)

func TestRemoteWipeDeletesStateRefusesLoginsAndAcknowledges(t *testing.T) {
	e, a, f, _ := seeded(t, 3)
	var closed atomic.Int32
	a.Attach(func() { closed.Add(1) })
	a.Attach(func() { closed.Add(1) })
	oldDevice, _ := e.mgr.DB().DeviceID()
	if serverCount(a, f.ID) != 3 {
		t.Fatal("setup")
	}

	e.srv.RequestWipe(testUser) // the next Ping/Sync/FolderSync sees it
	eventually(t, "WIPED marker written", func() bool { return store.WipeMarkerExists(e.dir) })
	eventually(t, "wipe acknowledged to Exchange", func() bool { return e.srv.WipeAcknowledged(testUser) })
	eventually(t, "sessions closed", func() bool { return closed.Load() == 2 })

	// The old database is gone; the fresh one is empty and has a new device id.
	eventually(t, "state deleted", func() bool {
		acct, err := e.mgr.DB().Account(testUser)
		if err != nil {
			return false
		}
		fs, _ := acct.Folders()
		return len(fs) == 0
	})
	if newDevice, _ := e.mgr.DB().DeviceID(); newDevice == oldDevice {
		t.Errorf("the device id survived the wipe")
	}
	// Exactly one loud line.
	if n := strings.Count(e.logs.String(), "REMOTE WIPE REQUESTED"); n != 1 {
		t.Errorf("%d loud wipe lines, want 1", n)
	}
	if !strings.Contains(e.logs.String(), "level=ERROR msg=\"REMOTE WIPE") {
		t.Errorf("the wipe line is not at ERROR level")
	}
	// Logins are refused, with or without a fast-path credential.
	for i := 0; i < 3; i++ {
		if _, err := e.mgr.Login(context.Background(), testUser, testPass); !errors.Is(err, engine.ErrWiped) {
			t.Fatalf("login after wipe: %v", err)
		}
	}
	if !e.mgr.Status().Wiped {
		t.Errorf("status does not report the wipe")
	}
	before := e.srv.AuthAttempts()
	e.mgr.Login(context.Background(), testUser, testPass)
	if e.srv.AuthAttempts() != before {
		t.Errorf("Exchange was contacted while wiped")
	}

	// The operator deletes the marker: the bridge starts over as a new device.
	if err := os.Remove(store.WipeMarkerPath(e.dir)); err != nil {
		t.Fatal(err)
	}
	b, err := e.mgr.Login(context.Background(), testUser, testPass)
	if err != nil {
		t.Fatalf("login after the marker was removed: %v", err)
	}
	if b == a {
		t.Fatal("the wiped engine came back")
	}
	nf := e.mirrored(b, "Inbox", "INBOX")
	if serverCount(b, nf.ID) != 3 {
		t.Errorf("fresh sync after wipe found %d messages", serverCount(b, nf.ID))
	}
}

func TestAccountOnlyRemoteWipeIsHonouredAndAnsweredWithItsOwnElement(t *testing.T) {
	// The EAS 16.1 order "remove this account's data" used to be decoded as a tag
	// called Hash and never noticed. The fake refuses the acknowledgement of the
	// other kind, as Exchange would keep asking, so WipeAcknowledged is only true
	// if the engine answered the right way.
	e, a, f, _ := seeded(t, 3)
	var closed atomic.Int32
	a.Attach(func() { closed.Add(1) })
	oldDevice, _ := e.mgr.DB().DeviceID()
	if serverCount(a, f.ID) != 3 {
		t.Fatal("setup")
	}

	e.srv.RequestAccountOnlyWipe(testUser)
	eventually(t, "WIPED marker written", func() bool { return store.WipeMarkerExists(e.dir) })
	eventually(t, "wipe acknowledged with AccountOnlyRemoteWipe", func() bool { return e.srv.WipeAcknowledged(testUser) })
	eventually(t, "sessions closed", func() bool { return closed.Load() == 1 })
	eventually(t, "state deleted", func() bool {
		acct, err := e.mgr.DB().Account(testUser)
		if err != nil {
			return false
		}
		fs, _ := acct.Folders()
		return len(fs) == 0
	})
	if newDevice, _ := e.mgr.DB().DeviceID(); newDevice == oldDevice {
		t.Errorf("the device id survived the wipe")
	}
	if n := strings.Count(e.logs.String(), "REMOTE WIPE (ACCOUNT ONLY) REQUESTED"); n != 1 {
		t.Errorf("%d loud wipe lines, want 1", n)
	}
	if !strings.Contains(e.logs.String(), "level=ERROR msg=\"REMOTE WIPE (ACCOUNT ONLY)") {
		t.Errorf("the wipe line is not at ERROR level")
	}
	for i := 0; i < 2; i++ {
		if _, err := e.mgr.Login(context.Background(), testUser, testPass); !errors.Is(err, engine.ErrWiped) {
			t.Fatalf("login after wipe: %v", err)
		}
	}
	if !e.mgr.Status().Wiped {
		t.Errorf("status does not report the wipe")
	}
}

func TestAccountOnlyRemoteWipeRequestedWhileLoggingIn(t *testing.T) {
	e := newEnv(t)
	e.srv.RequestAccountOnlyWipe(testUser)
	if _, err := e.mgr.Login(context.Background(), testUser, testPass); !errors.Is(err, engine.ErrWiped) {
		t.Fatalf("err = %v", err)
	}
	eventually(t, "marker", func() bool { return store.WipeMarkerExists(e.dir) })
	eventually(t, "ack of the right kind", func() bool { return e.srv.WipeAcknowledged(testUser) })
}

func TestAFullWipeIsNotAnsweredAsAnAccountOnlyOne(t *testing.T) {
	// The other direction of the same rule: the fake refuses an account-only
	// acknowledgement of a full wipe.
	e, _, _, _ := seeded(t, 1)
	e.srv.RequestWipe(testUser)
	eventually(t, "wipe acknowledged", func() bool { return e.srv.WipeAcknowledged(testUser) })
	if strings.Contains(e.logs.String(), "ACCOUNT ONLY") {
		t.Errorf("a full wipe was handled as an account-only one")
	}
}

func TestRemoteWipeRequestedWhileLoggingIn(t *testing.T) {
	e := newEnv(t)
	e.srv.RequestWipe(testUser)
	_, err := e.mgr.Login(context.Background(), testUser, testPass)
	if !errors.Is(err, engine.ErrWiped) {
		t.Fatalf("err = %v", err)
	}
	eventually(t, "marker", func() bool { return store.WipeMarkerExists(e.dir) })
	eventually(t, "ack", func() bool { return e.srv.WipeAcknowledged(testUser) })
	if _, err := e.mgr.Login(context.Background(), testUser, testPass); !errors.Is(err, engine.ErrWiped) {
		t.Fatalf("second login: %v", err)
	}
}

func TestPingHeartbeatStepsDownWhenLongRequestsAreCut(t *testing.T) {
	e := newEnv(t, func(c *engine.Config, s *fakeupstream.Server) {
		c.PingHeartbeat = 800 * time.Millisecond
		c.PingMinHeartbeat = 40 * time.Millisecond
		s.PingDropAfter = 150 * time.Millisecond // a proxy that kills requests held longer
	})
	a := e.login()
	f := e.mirrored(a, "Inbox", "INBOX")
	eventually(t, "heartbeat adapted below the proxy's limit", func() bool { return a.PingHeartbeat() < 150*time.Millisecond })
	if a.PingHeartbeat() < 40*time.Millisecond {
		t.Fatalf("heartbeat went below the floor: %v", a.PingHeartbeat())
	}
	// Push still works with the shorter heartbeat.
	rec, _ := e.watch(a, f)
	e.srv.AddMessage(testUser, e.srv.FolderID(testUser, "Inbox"), mailBytes("p@x", "push", e.clock.Now(), ""), fakeupstream.AddOptions{})
	eventually(t, "new mail via Ping", func() bool { return rec.count(store.EvAdd) == 1 })
	if !strings.Contains(e.logs.String(), "shorter heartbeat") {
		t.Errorf("the adaptation was not logged")
	}
}

func TestPingStatus5AdoptsTheServersHeartbeat(t *testing.T) {
	e := newEnv(t, func(c *engine.Config, s *fakeupstream.Server) {
		c.PingHeartbeat = 800 * time.Millisecond
		c.PingMinHeartbeat = 40 * time.Millisecond
		s.MaxPingHeartbeat = 120 * time.Millisecond
	})
	a := e.login()
	e.mirrored(a, "Inbox", "INBOX")
	eventually(t, "server's heartbeat adopted", func() bool { return a.PingHeartbeat() == 120*time.Millisecond })
}

func TestPingErrorsDoNotKillTheEngine(t *testing.T) {
	e := newEnv(t)
	a := e.login()
	f := e.mirrored(a, "Inbox", "INBOX")
	e.srv.FailNext("Ping", errors.New("boom"), 5)
	rec, _ := e.watch(a, f)
	e.srv.AddMessage(testUser, e.srv.FolderID(testUser, "Inbox"), mailBytes("p2@x", "after errors", e.clock.Now(), ""), fakeupstream.AddOptions{})
	eventually(t, "mail arrives once Ping recovers", func() bool { return rec.count(store.EvAdd) == 1 })
}
