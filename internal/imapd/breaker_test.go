//go:build devfake

package imapd_test

import (
	"strings"
	"testing"
	"time"

	"easbridge/internal/engine"
	"easbridge/internal/fakeupstream"
	"easbridge/internal/testenv"
	"easbridge/internal/upstream"
)

// While the circuit breaker is open nothing leaves for Exchange. Commands that
// need Exchange answer NO [UNAVAILABLE], the clean "try later" of IMAP, and what
// the local cache can answer keeps working.
func TestWhileTheBreakerIsOpenCommandsAnswerNOUnavailable(t *testing.T) {
	e := newIMAP(t, func(c *engine.Config, _ *fakeupstream.Server) {
		c.BreakerBase = 5 * time.Second
		c.BreakerMax = 15 * time.Minute
	})
	inbox := e.Srv.FolderID(testenv.User, "Inbox")
	e.Srv.AddMessage(testenv.User, inbox, []byte("Message-ID: <b@x>\r\nFrom: b@example.test\r\nTo: alice@example.test\r\nSubject: cached\r\n"+
		"Date: Fri, 09 Oct 2026 12:00:00 +0000\r\n\r\nbody\r\n"), fakeupstream.AddOptions{Date: e.Clock.Now().Add(-time.Hour)})
	e.ready("Inbox", "INBOX")

	// Exchange asks to slow down: the breaker opens at once.
	e.Srv.FailNext("Sync", &upstream.ThrottleError{HTTP: 503, RetryAfter: 10 * time.Minute, Explicit: true}, 1)
	e.Srv.AddMessage(testenv.User, inbox, []byte("Message-ID: <c@x>\r\nFrom: c@example.test\r\nTo: alice@example.test\r\nSubject: later\r\n"+
		"Date: Fri, 09 Oct 2026 12:30:00 +0000\r\n\r\nbody\r\n"), fakeupstream.AddOptions{Date: e.Clock.Now()})
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := e.Mgr.Status()
		if len(st.Accounts) == 1 && st.Accounts[0].Paused {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the breaker did not open")
		}
		time.Sleep(5 * time.Millisecond)
	}

	r := e.rawDial()
	r.loginOK()
	for _, cmd := range []string{`CREATE "While-paused"`, `RENAME "INBOX/nothing" "x"`, `DELETE "nothing-here"`} {
		l := status(r.cmd("%s", cmd))
		if !strings.Contains(l, "NO [UNAVAILABLE]") {
			t.Errorf("%s: %s, want NO [UNAVAILABLE]", cmd, l)
		}
	}
	// A flag change and an EXPUNGE are local at once and queued for later: they
	// work, nothing is sent while paused.
	r.ok(`SELECT INBOX`)
	if got := joinLines(r.ok(`UID FETCH 1 (UID FLAGS ENVELOPE)`)); !strings.Contains(got, "cached") {
		t.Errorf("the cache stopped answering: %s", got)
	}
	calls := e.Srv.Calls("ApplyChanges")
	r.ok(`UID STORE 1 +FLAGS (\Seen)`)
	time.Sleep(100 * time.Millisecond)
	if e.Srv.Calls("ApplyChanges") != calls {
		t.Error("a change was sent to Exchange while the breaker was open")
	}
	// Log lines never carry mail; the pause is explained once.
	if n := strings.Count(e.Logs.String(), "pausing all Exchange traffic"); n != 1 {
		t.Errorf("%d pause lines in the log, want 1", n)
	}
}

// An Exchange that cannot be reached is "try again later", not a server bug.
func TestCreateWhileExchangeIsUnreachableIsUnavailableNotAServerBug(t *testing.T) {
	e := newIMAP(t)
	e.ready("Inbox", "INBOX")
	r := e.rawDial()
	r.loginOK()
	e.Srv.FailNext("CreateFolder", upstream.ErrUnavailable, 1)
	if l := status(r.cmd(`CREATE "Archive2026"`)); !strings.Contains(l, "NO [UNAVAILABLE]") {
		t.Errorf("CREATE with Exchange down: %s, want NO [UNAVAILABLE]", l)
	}
	if l := status(r.cmd(`CREATE "Archive2026"`)); !strings.Contains(l, " OK") {
		t.Errorf("CREATE after recovery: %s", l)
	}
}
