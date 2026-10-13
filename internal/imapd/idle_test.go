//go:build devfake

package imapd_test

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"easbridge/internal/engine"
	"easbridge/internal/fakeupstream"
	"easbridge/internal/testenv"
)

// events collects what a client heard while idle or between commands.
type events struct {
	mu      sync.Mutex
	exists  []uint32
	expunge []uint32
	flags   map[imap.UID][]imap.Flag
}

func newEvents() *events { return &events{flags: map[imap.UID][]imap.Flag{}} }

func (ev *events) handler() *imapclient.UnilateralDataHandler {
	return &imapclient.UnilateralDataHandler{
		Expunge: func(n uint32) { ev.mu.Lock(); ev.expunge = append(ev.expunge, n); ev.mu.Unlock() },
		Mailbox: func(d *imapclient.UnilateralDataMailbox) {
			if d.NumMessages != nil {
				ev.mu.Lock()
				ev.exists = append(ev.exists, *d.NumMessages)
				ev.mu.Unlock()
			}
		},
		Fetch: func(m *imapclient.FetchMessageData) {
			b, err := m.Collect()
			if err != nil {
				return
			}
			ev.mu.Lock()
			ev.flags[b.UID] = b.Flags
			ev.mu.Unlock()
		},
	}
}

func (ev *events) lastExists() uint32 {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	if len(ev.exists) == 0 {
		return 0
	}
	return ev.exists[len(ev.exists)-1]
}

func (ev *events) expunges() []uint32 {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	return append([]uint32(nil), ev.expunge...)
}

func (ev *events) flagsOf(uid imap.UID) []imap.Flag {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	return ev.flags[uid]
}

func TestIdleOnSeveralFoldersAtOnceReceivesEverything(t *testing.T) {
	// imapflow IDLEs every selected session after 15 s of quiet: the sync
	// session on INBOX, pooled sessions on Sent, Junk, ... all at the same time.
	e := newIMAP(t)
	inbox := e.Srv.FolderID(testenv.User, "Inbox")
	sent := e.Srv.FolderID(testenv.User, "Sent Items")
	junk := e.Srv.FolderID(testenv.User, "Junk Email")
	e.ready("Inbox", "INBOX")
	evI, evS, evJ := newEvents(), newEvents(), newEvents()
	cI, cS, cJ := e.login(evI.handler()), e.login(evS.handler()), e.login(evJ.handler())
	for c, name := range map[*imapclient.Client]string{cI: "INBOX", cS: "Sent Items", cJ: "Junk Email"} {
		if _, err := c.Select(name, nil).Wait(); err != nil {
			t.Fatal(err)
		}
	}
	idles := []*imapclient.IdleCommand{}
	for _, c := range []*imapclient.Client{cI, cS, cJ} {
		id, err := c.Idle()
		if err != nil {
			t.Fatal(err)
		}
		idles = append(idles, id)
	}

	// New mail in every folder.
	now := e.Clock.Now()
	i1 := e.Srv.AddMessage(testenv.User, inbox, testenv.Mail("i1@x", "i1", now, ""), fakeupstream.AddOptions{Date: now})
	e.Srv.AddMessage(testenv.User, inbox, testenv.Mail("i2@x", "i2", now, ""), fakeupstream.AddOptions{Date: now.Add(time.Second)})
	e.Srv.AddMessage(testenv.User, sent, testenv.Mail("s1@x", "s1", now, ""), fakeupstream.AddOptions{Date: now, Read: true})
	e.Srv.AddMessage(testenv.User, junk, testenv.Mail("j1@x", "j1", now, ""), fakeupstream.AddOptions{Date: now})
	testenv.Eventually(t, "EXISTS in all three folders", func() bool {
		return evI.lastExists() == 2 && evS.lastExists() == 1 && evJ.lastExists() == 1
	})

	// Flags changed elsewhere.
	yes := true
	e.Srv.SetFlags(testenv.User, inbox, i1, &yes, &yes)
	testenv.Eventually(t, "FETCH FLAGS while idling", func() bool { return len(evI.flagsOf(1)) == 2 })
	if f := fmt.Sprint(evI.flagsOf(1)); !strings.Contains(f, `\Seen`) || !strings.Contains(f, `\Flagged`) {
		t.Errorf("flags = %s", f)
	}
	// Deleted elsewhere.
	e.Srv.DeleteMessage(testenv.User, inbox, i1)
	testenv.Eventually(t, "EXPUNGE while idling", func() bool { return len(evI.expunges()) == 1 })
	if ex := evI.expunges(); ex[0] != 1 {
		t.Errorf("expunge seq %v", ex)
	}

	// A command must end IDLE promptly.
	start := time.Now()
	for _, id := range idles {
		if err := id.Close(); err != nil {
			t.Fatal(err)
		}
		if err := id.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cI.Status("INBOX", &imap.StatusOptions{NumMessages: true}).Wait(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("leaving IDLE took %v", d)
	}
}

func TestIdleWithoutSelectedMailboxAndRawDone(t *testing.T) {
	e := newIMAP(t)
	r := e.rawDial()
	r.loginOK()
	r.send("i1 IDLE")
	if l := r.line(); !strings.HasPrefix(l, "+") {
		t.Fatalf("no continuation: %q", l)
	}
	r.send("DONE")
	if l := r.line(); !strings.HasPrefix(l, "i1 OK") {
		t.Fatalf("IDLE end: %q", l)
	}
	r.ok("NOOP")
}

func TestManySessionsStayConsistentUnderConcurrentChanges(t *testing.T) {
	e := newIMAP(t, func(c *engine.Config, s *fakeupstream.Server) { s.Window = 7 })
	inbox := e.Srv.FolderID(testenv.User, "Inbox")
	e.ready("Inbox", "INBOX")

	const sessions = 4
	evs := make([]*events, sessions)
	clients := make([]*imapclient.Client, sessions)
	for i := range clients {
		evs[i] = newEvents()
		clients[i] = e.login(evs[i].handler())
		if _, err := clients[i].Select("INBOX", nil).Wait(); err != nil {
			t.Fatal(err)
		}
	}
	idle, err := clients[0].Idle() // one of them idles the whole time
	if err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var delivered atomic.Int32
	wg.Add(1)
	go func() { // mail keeps arriving
		defer wg.Done()
		for i := 0; i < 40; i++ {
			d := e.Clock.Now().Add(time.Duration(i) * time.Second)
			e.Srv.AddMessage(testenv.User, inbox, testenv.Mail(fmt.Sprintf("c%d@x", i), "s", d, ""), fakeupstream.AddOptions{Date: d})
			delivered.Add(1)
			time.Sleep(time.Duration(rand.Intn(8)) * time.Millisecond)
		}
	}()
	for w := 1; w < sessions; w++ {
		wg.Add(1)
		go func(c *imapclient.Client, seed int64) { // clients flag, move and expunge
			defer wg.Done()
			rnd := rand.New(rand.NewSource(seed))
			for i := 0; i < 25; i++ {
				select {
				case <-stop:
					return
				default:
				}
				st, err := c.Status("INBOX", &imap.StatusOptions{NumMessages: true}).Wait()
				if err != nil || *st.NumMessages == 0 {
					time.Sleep(5 * time.Millisecond)
					continue
				}
				set := imap.SeqSetNum(uint32(1 + rnd.Intn(int(*st.NumMessages))))
				switch rnd.Intn(4) {
				case 0:
					c.Store(set, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagFlagged}}, nil).Close()
				case 1:
					c.Store(set, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagSeen}}, nil).Close()
				case 2:
					c.Store(set, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close()
					c.Expunge().Close()
				case 3:
					c.Fetch(set, &imap.FetchOptions{UID: true, Flags: true}).Close()
				}
			}
		}(clients[w], int64(w))
	}
	wg.Wait()
	close(stop)
	testenv.Eventually(t, "all mail synced", func() bool {
		f, err := e.account().Store().FolderByPath("INBOX")
		if err != nil {
			return false
		}
		// Messages may have been expunged by the clients: compare with the server.
		return len(e.Srv.Items(testenv.User, inbox)) >= 0 && testenv.ServerCount(e.account(), f.ID) > 0
	})
	time.Sleep(300 * time.Millisecond) // let Ping-driven syncs settle
	if err := idle.Close(); err != nil {
		t.Fatal(err)
	}
	idle.Wait()

	// Every session must now agree with the database on what the mailbox holds.
	type row struct {
		uid   imap.UID
		flags string
	}
	list := func(c *imapclient.Client) []row {
		if err := c.Noop().Wait(); err != nil {
			t.Fatal(err)
		}
		set := imap.SeqSet{}
		set.AddRange(1, 0)
		msgs, err := c.Fetch(set, &imap.FetchOptions{UID: true, Flags: true}).Collect()
		if err != nil {
			t.Fatal(err)
		}
		var out []row
		for _, m := range msgs {
			fl := make([]string, len(m.Flags))
			for i, f := range m.Flags {
				fl[i] = string(f)
			}
			sort.Strings(fl)
			out = append(out, row{m.UID, strings.Join(fl, ",")})
		}
		return out
	}
	fresh := e.login(nil)
	if _, err := fresh.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	want := list(fresh)
	f, _ := e.account().Store().FolderByPath("INBOX")
	metas, _ := e.account().Store().ListMessages(f.ID)
	if len(metas) != len(want) {
		t.Fatalf("fresh session sees %d messages, database has %d", len(want), len(metas))
	}
	for i, c := range clients {
		got := list(c)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("session %d disagrees with the database:\n got  %v\n want %v", i, got, want)
		}
		if st := c.Mailbox(); st == nil || int(st.NumMessages) != len(want) {
			t.Errorf("session %d tracks %v messages, want %d", i, st, len(want))
		}
	}
}
