//go:build devfake

package imapd_test

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"easbridge/internal/testenv"
)

var existsLine = regexp.MustCompile(`^\* (\d+) EXISTS$`)

// existsLines returns the numbers of all "* n EXISTS" lines in a response.
func existsLines(lines []string) []string {
	var out []string
	for _, l := range lines {
		if m := existsLine.FindStringSubmatch(l); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

// During a folder's first sync STATUS and SELECT show an empty folder with a
// stable UIDNEXT; when the sync completes the messages appear with ONE EXISTS
// line, numbered in date order (v0.1.1).
func TestFolderIsEmptyDuringItsFirstSyncThenAppearsWithOneExists(t *testing.T) {
	e := newIMAP(t)
	e.Srv.Window = 3
	const n = 9
	inbox := e.Srv.FolderID(testenv.User, "Inbox")
	e.seedInbox(n)
	e.Srv.HoldAfter("Sync", testenv.User, inbox, 3) // bootstrap + two batches, then the chain stalls

	r := e.rawDial()
	r.loginOK()
	acct := e.account()
	testenv.Eventually(t, "part of the folder is downloaded", func() bool {
		ps, _ := acct.Store().InitialProgress()
		for _, p := range ps {
			if p.Staged > 0 {
				return true
			}
		}
		return false
	})

	// STATUS and SELECT while the first sync is running.
	for i := 0; i < 3; i++ {
		st := joinLines(r.ok(`STATUS INBOX (MESSAGES UNSEEN UIDNEXT)`))
		if !strings.Contains(st, "MESSAGES 0") || !strings.Contains(st, "UNSEEN 0") || !strings.Contains(st, "UIDNEXT 1") {
			t.Fatalf("STATUS during the first sync: %s", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
	sel := r.ok(`SELECT INBOX`)
	if got := existsLines(sel); len(got) != 1 || got[0] != "0" {
		t.Fatalf("SELECT during the first sync: %v", sel)
	}
	if !strings.Contains(joinLines(sel), "UIDNEXT 1") {
		t.Errorf("UIDNEXT must not move while the folder is closed: %v", sel)
	}
	if got := existsLines(r.ok(`NOOP`)); len(got) != 0 {
		t.Errorf("EXISTS lines while nothing changed: %v", got)
	}

	// The chain finishes: the folder opens, and the selected session hears one
	// EXISTS with the whole count.
	e.Srv.Release("Sync", testenv.User, inbox)
	testenv.Eventually(t, "the folder opens", func() bool {
		f, err := acct.Store().FolderByPath("INBOX")
		return err == nil && f.InitialComplete
	})
	noop := r.ok(`NOOP`)
	if got := existsLines(noop); len(got) != 1 || got[0] != fmt.Sprint(n) {
		t.Fatalf("want exactly one `* %d EXISTS`, got %v (all: %v)", n, got, noop)
	}
	if got := existsLines(r.ok(`NOOP`)); len(got) != 0 {
		t.Errorf("EXISTS announced twice: %v", got)
	}
	st := joinLines(r.ok(`STATUS INBOX (MESSAGES UIDNEXT)`))
	if !strings.Contains(st, fmt.Sprintf("MESSAGES %d", n)) || !strings.Contains(st, fmt.Sprintf("UIDNEXT %d", n+1)) {
		t.Errorf("STATUS after opening: %s", st)
	}

	// UIDs run with the dates: oldest message UID 1.
	fetch := r.ok(`UID FETCH 1:* (UID INTERNALDATE)`)
	var dates []time.Time
	re := regexp.MustCompile(`UID (\d+) INTERNALDATE "([^"]+)"|INTERNALDATE "([^"]+)" UID (\d+)`)
	for _, l := range fetch {
		m := re.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		ds := m[2]
		if ds == "" {
			ds = m[3]
		}
		d, err := time.Parse("_2-Jan-2006 15:04:05 -0700", strings.TrimSpace(ds))
		if err != nil {
			t.Fatalf("date %q: %v", ds, err)
		}
		dates = append(dates, d)
	}
	if len(dates) != n {
		t.Fatalf("fetched %d dates from %v", len(dates), fetch)
	}
	for i := 1; i < len(dates); i++ {
		if dates[i].Before(dates[i-1]) {
			t.Errorf("UID %d is older than UID %d: %v", i+1, i, dates)
		}
	}
}

// A session that selects a folder only after it opened sees the finished
// folder, not a half-built one.
func TestSelectAfterTheFirstSyncSeesEverything(t *testing.T) {
	e := newIMAP(t)
	e.Srv.Window = 3
	e.seedInbox(8)
	e.ready("Inbox", "INBOX")
	r := e.rawDial()
	r.loginOK()
	sel := r.ok(`SELECT INBOX`)
	if got := existsLines(sel); len(got) != 1 || got[0] != "8" {
		t.Fatalf("SELECT after the first sync: %v", sel)
	}
}

// The first login on a fresh database must not return before the folder list is
// known: MailFlow's first LIST or SELECT would otherwise find no INBOX.
func TestFirstLoginWaitsForTheFolderList(t *testing.T) {
	e := newIMAP(t)
	e.Srv.HoldAfter("FolderSync", testenv.User, "", 0)
	r := e.rawDial()
	done := make(chan []string, 1)
	go func() {
		done <- r.cmd(`LOGIN "%s" "%s"`, testenv.User, testenv.Pass)
	}()
	select {
	case lines := <-done:
		t.Fatalf("LOGIN completed before the folder list was known: %v", lines)
	case <-time.After(250 * time.Millisecond):
	}
	e.Srv.Release("FolderSync", testenv.User, "")
	var lines []string
	select {
	case lines = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("LOGIN did not complete after the folder list arrived")
	}
	if !strings.Contains(status(lines), "OK") {
		t.Fatalf("LOGIN: %v", lines)
	}
	if list := joinLines(r.ok(`LIST "" "*"`)); !strings.Contains(list, `"INBOX"`) && !strings.Contains(list, " INBOX") {
		t.Errorf("LIST right after LOGIN has no INBOX: %s", list)
	}
	if sel := r.ok(`SELECT INBOX`); len(existsLines(sel)) != 1 {
		t.Errorf("SELECT right after LOGIN: %v", sel)
	}
}
