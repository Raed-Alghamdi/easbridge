//go:build devfake

package engine_test

import (
	"testing"
	"time"

	"easbridge/internal/engine"
	"easbridge/internal/fakeupstream"
	"easbridge/internal/upstream"
)

// Ping status 7 means "the folder hierarchy changed". The Ping loop used to ask
// for a FolderSync and Ping again a second later, whether or not the FolderSync
// had run (or could run), so a hierarchy that could not be refreshed meant one
// request a second for ever.

func TestPingStatus7WaitsForTheFolderSyncInsteadOfPolling(t *testing.T) {
	e := newEnv(t, func(c *engine.Config, _ *fakeupstream.Server) { c.HierarchyBackoff = 40 * time.Millisecond })
	a := e.login()
	e.mirrored(a, "Inbox", "INBOX")

	e.srv.FailNext("FolderSync", upstream.ErrUnavailable, -1) // the hierarchy cannot be refreshed
	e.srv.AddFolder(testUser, "0", "NewFolder", fakeupstream.TypeMail)
	eventually(t, "the hierarchy refresh is attempted and fails", func() bool { return e.srv.Calls("FolderSync") >= 3 })

	time.Sleep(100 * time.Millisecond) // a Ping that was already on its way
	before := e.srv.Calls("Ping")
	time.Sleep(2500 * time.Millisecond)
	if n := e.srv.Calls("Ping") - before; n > 1 {
		t.Fatalf("%d Pings in 2.5 s while the hierarchy could not be refreshed", n)
	}

	// Once FolderSync works the folder appears and push carries on.
	e.srv.ClearFailures()
	eventually(t, "the new folder is known", func() bool {
		_, err := a.Store().FolderByPath("NewFolder")
		return err == nil
	})
	f := e.folder(a, "INBOX")
	e.srv.AddMessage(testUser, e.srv.FolderID(testUser, "Inbox"), mailBytes("after7@x", "after", e.clock.Now(), ""), fakeupstream.AddOptions{})
	eventually(t, "push works again", func() bool { return serverCount(a, f.ID) == 1 })
}

func TestPingStatus7BacksOffWhenExchangeKeepsAnsweringIt(t *testing.T) {
	e := newEnv(t, func(c *engine.Config, s *fakeupstream.Server) {
		c.HierarchyBackoff = 40 * time.Millisecond
		s.PingStatus7 = true
	})
	a := e.login()
	e.waitFolder(a, "INBOX")
	eventually(t, "the first Pings", func() bool { return e.srv.Calls("Ping") >= 1 })
	start := e.srv.Calls("Ping")
	time.Sleep(1500 * time.Millisecond)
	// 40, 80, 160, 320, 640 ms apart: five or six more, not 37.
	if n := e.srv.Calls("Ping") - start; n > 8 {
		t.Fatalf("%d Pings in 1.5 s although every answer was status 7", n)
	}
	if e.srv.Calls("Ping")-start < 2 {
		t.Errorf("the loop stopped pinging altogether")
	}
}
