package easupstream_test

// An end-to-end test of the real stack: engine -> easupstream adapter ->
// patched go-activesync -> WBXML over TLS -> a small stateful model of an
// Exchange server. It is the closest thing to Exchange that can run offline, and
// it exercises the staged-key plumbing, silent key resets, truncated bodies,
// Ping, client changes, MoveItems and SendMail through the real library.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hstern/go-activesync/wbxml"

	"easbridge/internal/easupstream"
	"easbridge/internal/engine"
	"easbridge/internal/logx"
	"easbridge/internal/store"
)

type mItem struct {
	id      string
	mime    []byte
	read    bool
	flagged bool
	date    time.Time
}

type mSnap struct {
	serial int
	known  map[string][2]bool
}

type mFolder struct {
	id, name string
	typ      int
	items    map[string]*mItem
	keys     map[string]*mSnap
	serial   int
	invalid  int
	latest   *mSnap
}

// model is a tiny Exchange: folders, items, sync keys, Ping.
type model struct {
	mu      sync.Mutex
	f       *fakeEAS
	folders map[string]*mFolder
	order   []string
	next    int
	hier    int
	changed chan struct{}
	sent    [][]byte
	window  int
	hierN   int
	hierKey map[string]int
	// hierSeen lists the SyncKey of every FolderSync. hierZeroResets makes a FolderSync from
	// "0" that is not the first invalidate every mail collection's Sync key (status 3 on the
	// next Sync): MS-ASCMD does not say what Exchange does, the bridge must survive both.
	hierSeen       []string
	hierZeroResets bool
	// data builds airsyncbase:Data: mimeData (an inline string, as Exchange 2019
	// sends it) unless a test wants the other form.
	data func(string) *wbxml.Element
}

func newModel(t *testing.T) *model {
	m := &model{f: newFake(t), folders: map[string]*mFolder{}, changed: make(chan struct{}), window: 5, hierKey: map[string]int{}, data: mimeData}
	for _, d := range []struct {
		name string
		typ  int
	}{{"Inbox", 2}, {"Drafts", 3}, {"Deleted Items", 4}, {"Sent Items", 5}, {"Junk Email", 12}} {
		m.addFolder(d.name, d.typ)
	}
	m.f.on("FolderSync", m.folderSync)
	m.f.on("Sync", m.sync)
	m.f.on("ItemOperations", m.fetch)
	m.f.on("Ping", m.ping)
	m.f.on("MoveItems", m.move)
	m.f.on("SendMail", m.sendMail)
	return m
}

func (m *model) notify() {
	close(m.changed)
	m.changed = make(chan struct{})
}

func (m *model) addFolder(name string, typ int) *mFolder {
	m.next++
	f := &mFolder{id: strconv.Itoa(m.next), name: name, typ: typ, items: map[string]*mItem{}, keys: map[string]*mSnap{}}
	m.folders[f.id] = f
	m.order = append(m.order, f.id)
	return f
}

func (m *model) byName(name string) *mFolder {
	for _, f := range m.folders {
		if f.name == name {
			return f
		}
	}
	panic("no folder " + name)
}

func (m *model) deliver(folder string, mime []byte, read bool, date time.Time) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	f := m.byName(folder)
	m.next++
	id := f.id + ":" + strconv.Itoa(m.next)
	f.items[id] = &mItem{id: id, mime: mime, read: read, date: date.UTC().Truncate(time.Second)}
	m.notify()
	return id
}

func (m *model) remove(folder, id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.byName(folder).items, id)
	m.notify()
}

func (m *model) setRead(folder, id string, read bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byName(folder).items[id].read = read
	m.notify()
}

func (m *model) forgetKeys(folder string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f := m.byName(folder)
	f.invalid = f.serial + 1
	f.latest = nil
	m.notify()
}

func (m *model) snapshotOf(f *mFolder) map[string][2]bool {
	out := map[string][2]bool{}
	for id, it := range f.items {
		out[id] = [2]bool{it.read, it.flagged}
	}
	return out
}

func (f *mFolder) issue(known map[string][2]bool) string {
	f.serial++
	key := fmt.Sprintf("S%s.%d", f.id, f.serial)
	cp := map[string][2]bool{}
	for k, v := range known {
		cp[k] = v
	}
	s := &mSnap{serial: f.serial, known: cp}
	f.keys[key] = s
	f.latest = s
	return key
}

func (m *model) folderSync(r *http.Request, in *wbxml.Element) (int, *wbxml.Element) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key, _ := text(in, "SyncKey")
	if key == "0" && len(m.hierSeen) > 0 && m.hierZeroResets {
		for _, f := range m.folders {
			f.invalid = f.serial + 1
			f.latest = nil
		}
	}
	m.hierSeen = append(m.hierSeen, key)
	var adds []*wbxml.Element
	if key == "0" {
		for _, id := range m.order {
			f := m.folders[id]
			adds = append(adds, folderAdd(f.id, "0", f.name, f.typ))
		}
	} else if _, ok := m.hierKey[key]; !ok {
		return 0, e(wbxml.PageFolderHierarchy, "FolderSync", e(wbxml.PageFolderHierarchy, "Status", txt("9")))
	}
	m.hierN++
	nk := fmt.Sprintf("H%d", m.hierN)
	m.hierKey[nk] = m.hierN
	return 0, folderSyncResp(nk, adds...)
}

func (m *model) sync(r *http.Request, in *wbxml.Element) (int, *wbxml.Element) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fid, _ := text(in, "CollectionId")
	key, _ := text(in, "SyncKey")
	f := m.folders[fid]
	if f == nil {
		return 0, syncResp(fid, key, "8")
	}
	if key == "0" {
		return 0, syncResp(fid, f.issue(map[string][2]bool{}), "1")
	}
	snap := f.keys[key]
	if snap == nil || snap.serial < f.invalid {
		return 0, syncResp(fid, key, "3")
	}
	known := map[string][2]bool{}
	for k, v := range snap.known {
		known[k] = v
	}
	var responses []*wbxml.Element
	// Client commands.
	if cmds := in.Find("Commands"); cmds != nil {
		for _, c := range cmds.Children {
			el, ok := c.(*wbxml.Element)
			if !ok {
				continue
			}
			sid, _ := text(el, "ServerId")
			it := f.items[sid]
			if it == nil {
				responses = append(responses, e(wbxml.PageAirSync, el.Name, e(wbxml.PageAirSync, "ServerId", txt(sid)), e(wbxml.PageAirSync, "Status", txt("8"))))
				continue
			}
			switch el.Name {
			case "Delete":
				delete(f.items, sid)
				delete(known, sid)
				if dam, _ := text(in, "DeletesAsMoves"); dam != "0" && f.typ != 4 {
					d := m.byName("Deleted Items")
					m.next++
					nid := d.id + ":" + strconv.Itoa(m.next)
					d.items[nid] = &mItem{id: nid, mime: it.mime, read: it.read, flagged: it.flagged, date: it.date}
				}
			case "Change":
				if v, ok := text(el, "Read"); ok {
					it.read = v == "1"
				}
				if v, ok := text(el, "FlagStatus"); ok {
					it.flagged = v == "2"
				}
				if _, tracked := known[sid]; tracked {
					known[sid] = [2]bool{it.read, it.flagged}
				}
			}
		}
		m.notify()
	}
	var kids []*wbxml.Element
	more := false
	if gc, _ := text(in, "GetChanges"); gc != "0" {
		trunc := 1 << 30
		if v, ok := text(in, "TruncationSize"); ok {
			trunc, _ = strconv.Atoi(v)
		}
		wantMIME := true
		if v, ok := text(in, "Type"); ok && v != "4" {
			wantMIME = false
		}
		win := m.window
		if v, ok := text(in, "WindowSize"); ok {
			win, _ = strconv.Atoi(v)
		}
		var adds, chg, del []string
		for id, it := range f.items {
			k, tracked := known[id]
			switch {
			case !tracked:
				adds = append(adds, id)
			case k != [2]bool{it.read, it.flagged}:
				chg = append(chg, id)
			}
		}
		for id := range known {
			if f.items[id] == nil {
				del = append(del, id)
			}
		}
		sort.Slice(adds, func(i, j int) bool {
			a, b := f.items[adds[i]], f.items[adds[j]]
			if !a.date.Equal(b.date) {
				return a.date.After(b.date)
			}
			return a.id > b.id
		})
		sort.Strings(chg)
		sort.Strings(del)
		var cmdEls []*wbxml.Element
		n := 0
		take := func() bool {
			if n >= win {
				more = true
				return false
			}
			n++
			return true
		}
		for _, id := range adds {
			if !take() {
				break
			}
			it := f.items[id]
			date := it.date.Format("2006-01-02T15:04:05.000Z")
			if !wantMIME {
				// A plain-text body cut to the size asked for: the headers-only sync.
				cmdEls = append(cmdEls, addItemHeadersOnly(id, string(it.mime), it.read, flagStatus(it.flagged), date, len(it.mime)))
				known[id] = [2]bool{it.read, it.flagged}
				continue
			}
			data := it.mime
			cut := false
			if len(data) > trunc {
				data, cut = data[:trunc], true
			}
			cmdEls = append(cmdEls, addItemData(m.data, id, string(it.mime), string(data), it.read, flagStatus(it.flagged), date, cut, len(it.mime)))
			known[id] = [2]bool{it.read, it.flagged}
		}
		for _, id := range chg {
			if !take() {
				break
			}
			it := f.items[id]
			readV := "0"
			if it.read {
				readV = "1"
			}
			cmdEls = append(cmdEls, e(wbxml.PageAirSync, "Change", e(wbxml.PageAirSync, "ServerId", txt(id)),
				e(wbxml.PageAirSync, "ApplicationData",
					e(wbxml.PageEmail, "Read", txt(readV)),
					e(wbxml.PageEmail, "Flag", e(wbxml.PageEmail, "FlagStatus", txt(strconv.Itoa(flagStatus(it.flagged))))))))
			known[id] = [2]bool{it.read, it.flagged}
		}
		for _, id := range del {
			if !take() {
				break
			}
			cmdEls = append(cmdEls, e(wbxml.PageAirSync, "Delete", e(wbxml.PageAirSync, "ServerId", txt(id))))
			delete(known, id)
		}
		if len(cmdEls) > 0 {
			kids = append(kids, commands(cmdEls...))
		}
	}
	if len(responses) > 0 {
		rs := e(wbxml.PageAirSync, "Responses")
		for _, r := range responses {
			rs.Children = append(rs.Children, r)
		}
		kids = append(kids, rs)
	}
	if more {
		kids = append(kids, e(wbxml.PageAirSync, "MoreAvailable"))
	}
	return 0, syncResp(fid, f.issue(known), "1", kids...)
}

func flagStatus(b bool) int {
	if b {
		return 2
	}
	return 0
}

func (m *model) fetch(r *http.Request, in *wbxml.Element) (int, *wbxml.Element) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fid, _ := text(in, "CollectionId")
	sid, _ := text(in, "ServerId")
	f := m.folders[fid]
	var it *mItem
	if f != nil {
		it = f.items[sid]
	}
	if it == nil {
		return 0, fetchResponse(fid, sid, "6", nil)
	}
	return 0, fetchMIMEResponse(m.data, fid, sid, string(it.mime), it.read, flagStatus(it.flagged), it.date.Format("2006-01-02T15:04:05.000Z"))
}

func (m *model) ping(r *http.Request, in *wbxml.Element) (int, *wbxml.Element) {
	hb := 1
	if v, ok := text(in, "HeartbeatInterval"); ok {
		hb, _ = strconv.Atoi(v)
	}
	var ids []string
	for _, f := range in.FindAll("Folder") {
		if id, ok := text(f, "Id"); ok {
			ids = append(ids, id)
		}
	}
	deadline := time.NewTimer(time.Duration(hb) * time.Second)
	defer deadline.Stop()
	for {
		m.mu.Lock()
		var changed []string
		for _, id := range ids {
			f := m.folders[id]
			if f == nil {
				continue
			}
			base := map[string][2]bool{}
			if f.latest != nil {
				base = f.latest.known
			}
			cur := m.snapshotOf(f)
			dirty := len(cur) != len(base)
			for k, v := range cur {
				if b, ok := base[k]; !ok || b != v {
					dirty = true
				}
			}
			if dirty && (f.latest != nil || len(cur) > 0) {
				changed = append(changed, id)
			}
		}
		ch := m.changed
		m.mu.Unlock()
		if len(changed) > 0 {
			return 0, pingResp("2", changed...)
		}
		select {
		case <-ch:
		case <-deadline.C:
			return 0, pingResp("1")
		case <-r.Context().Done():
			return 0, pingResp("1")
		}
	}
}

func (m *model) move(r *http.Request, in *wbxml.Element) (int, *wbxml.Element) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := e(wbxml.PageMove, "MoveItems")
	for _, mv := range in.FindAll("Move") {
		src, _ := text(mv, "SrcMsgId")
		sf, _ := text(mv, "SrcFldId")
		df, _ := text(mv, "DstFldId")
		s, d := m.folders[sf], m.folders[df]
		it := s.items[src]
		if it == nil {
			out.Children = append(out.Children, e(wbxml.PageMove, "Response", e(wbxml.PageMove, "SrcMsgId", txt(src)), e(wbxml.PageMove, "Status", txt("5"))))
			continue
		}
		m.next++
		nid := d.id + ":" + strconv.Itoa(m.next)
		d.items[nid] = &mItem{id: nid, mime: it.mime, read: it.read, flagged: it.flagged, date: it.date}
		delete(s.items, src)
		out.Children = append(out.Children, e(wbxml.PageMove, "Response", e(wbxml.PageMove, "SrcMsgId", txt(src)),
			e(wbxml.PageMove, "Status", txt("3")), e(wbxml.PageMove, "DstMsgId", txt(nid))))
	}
	m.notify()
	return 0, out
}

func (m *model) sendMail(r *http.Request, in *wbxml.Element) (int, *wbxml.Element) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mimeEl := in.Find("Mime"); mimeEl != nil && len(mimeEl.Children) == 1 {
		if op, ok := mimeEl.Children[0].(wbxml.Opaque); ok {
			m.sent = append(m.sent, append([]byte(nil), op...))
			if in.Find("SaveInSentItems") != nil {
				f := m.byName("Sent Items")
				m.next++
				id := f.id + ":" + strconv.Itoa(m.next)
				f.items[id] = &mItem{id: id, mime: append([]byte(nil), op...), read: true, date: time.Now().UTC().Truncate(time.Second)}
				m.notify()
			}
		}
	}
	return 0, nil
}

// ---- the test -------------------------------------------------------------

type stack struct {
	t    *testing.T
	m    *model
	mgr  *engine.Manager
	acct *engine.Account
	logs *syncWriter
	buf  *bytes.Buffer
}

// newStack is buildStack plus a first login through the real stack.
func newStack(t *testing.T) *stack {
	s := buildStack(t)
	a, err := s.mgr.Login(ctx, user, pass)
	if err != nil {
		t.Fatalf("login through the real stack: %v", err)
	}
	s.acct = a
	return s
}

// buildStack wires the engine to the real adapter and library against the model,
// and logs nobody in.
func buildStack(t *testing.T) *stack {
	logx.ResetSecretsForTest()
	m := newModel(t)
	buf := &bytes.Buffer{}
	sw := &syncWriter{w: buf}
	log := logx.New(logx.Options{Level: slog.LevelDebug, Writer: sw})
	fac, err := easupstream.NewFactory(easupstream.Config{
		URL: m.f.url(), HTTPClient: m.f.srv.Client(), Version: "16.1", DeviceType: "easbridge", UserAgent: "easbridge/it",
		MIMEInlineMax: 2000, WindowSize: 5, MaxHeartbeat: time.Second, Logger: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := engine.Config{
		Grace: time.Hour, PingHeartbeat: time.Second, PingMinHeartbeat: 100 * time.Millisecond, AuthFailCooldown: time.Minute,
		MaxMessageBytes: 1 << 20, PlaceholderTTL: time.Hour, PlaceholderWindow: 10 * time.Minute,
		ReconVerifyDelay: 150 * time.Millisecond, FetchWorkers: 2, Housekeeping: 30 * time.Millisecond,
		Backoff: func(int) time.Duration { return 20 * time.Millisecond }, CallTimeout: 10 * time.Second,
	}
	mgr := engine.NewManager(cfg, log, db, fac, func(u string) bool { return u == user })
	s := &stack{t: t, m: m, mgr: mgr, logs: sw, buf: buf}
	t.Cleanup(func() {
		mgr.Close()
		mgr.DB().Close()
		if t.Failed() {
			t.Logf("log:\n%s", s.logText())
		}
	})
	return s
}

func (s *stack) logText() string {
	s.logs.mu.Lock()
	defer s.logs.mu.Unlock()
	return s.buf.String()
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func (s *stack) count(path string) int {
	f, err := s.acct.Store().FolderByPath(path)
	if err != nil {
		return -1
	}
	ms, _ := s.acct.Store().ListMessages(f.ID)
	n := 0
	for _, m := range ms {
		if m.Kind == store.KindServer {
			n++
		}
	}
	return n
}

func realMail(id, subject string, extra int) []byte {
	b := []byte("Message-ID: <" + id + ">\r\nFrom: b@example.test\r\nTo: alice@example.test\r\nSubject: " + subject +
		"\r\nDate: Fri, 09 Oct 2026 12:00:00 +0000\r\nMIME-Version: 1.0\r\nContent-Type: text/plain\r\n\r\nbody of " + id + "\r\n")
	if extra > 0 {
		b = append(b, bytes.Repeat([]byte(strings.Repeat("x", 76)+"\r\n"), extra/78+1)...)
	}
	return b
}

func TestEngineOverTheRealLibraryAgainstAWireLevelExchangeModel(t *testing.T) {
	s := newStack(t)
	m := s.m
	now := time.Now().UTC()
	// Initial state: 12 messages, two of them bigger than the inline limit.
	for i := 0; i < 12; i++ {
		extra := 0
		if i == 3 || i == 8 {
			extra = 6000
		}
		m.deliver("Inbox", realMail(fmt.Sprintf("m%02d@x", i), "s", extra), i%2 == 0, now.Add(-time.Duration(12-i)*time.Hour))
	}
	eventually(t, "initial sync across windows incl. the truncated ones", func() bool { return s.count("INBOX") == 12 })
	f, _ := s.acct.Store().FolderByPath("INBOX")
	for _, id := range []string{"m03@x", "m08@x"} {
		uids, _ := s.acct.Store().UIDsByMessageID(f.ID, id)
		if len(uids) != 1 {
			t.Fatalf("%s: %v", id, uids)
		}
		mime, _ := s.acct.Store().MIME(f.ID, uids[0])
		if !bytes.Equal(mime, realMail(id, "s", 6000)) {
			t.Fatalf("%s was stored truncated (%d bytes)", id, len(mime))
		}
	}
	// Folder mapping through the real FolderSync path.
	for path, use := range map[string]string{"Sent Items": `\Sent`, "Drafts": `\Drafts`, "Deleted Items": `\Trash`, "Junk Email": `\Junk`} {
		ff, err := s.acct.Store().FolderByPath(path)
		if err != nil || ff.SpecialUse != use {
			t.Errorf("%s: %+v %v", path, ff, err)
		}
	}

	// Push through Ping.
	var evMu sync.Mutex
	var adds []uint32
	var flagEvents int
	_, cancel, _ := s.acct.Watch(f.ID, func(ev store.Event) {
		evMu.Lock()
		defer evMu.Unlock()
		switch ev.Kind {
		case store.EvAdd:
			adds = append(adds, ev.UID)
		case store.EvFlags:
			flagEvents++
		}
	})
	defer cancel()
	newID := m.deliver("Inbox", realMail("push@x", "pushed", 0), false, now)
	eventually(t, "new mail through Ping", func() bool { evMu.Lock(); defer evMu.Unlock(); return len(adds) == 1 })
	m.setRead("Inbox", newID, true)
	eventually(t, "flag change from elsewhere", func() bool { evMu.Lock(); defer evMu.Unlock(); return flagEvents == 1 })

	// Client flag change travels with GetChanges=0 and reaches the model.
	m00, _ := s.acct.Store().UIDsByMessageID(f.ID, "m00@x")
	if len(m00) != 1 {
		t.Fatalf("m00@x: %v", m00)
	}
	if _, err := s.acct.SetFlags(f.ID, m00, engine.FlagAdd, store.FlagFlagged|store.FlagSeen, nil, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "STORE reaches the server", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		n := 0
		for _, it := range m.byName("Inbox").items {
			if it.flagged && it.read && strings.Contains(string(it.mime), "m00@x") {
				n++
			}
		}
		return n == 1
	})

	// Expunge -> DeletesAsMoves=1: the item lands in Deleted Items.
	if _, err := s.acct.SetFlags(f.ID, []uint32{2}, engine.FlagAdd, store.FlagDeleted, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.acct.Expunge(f.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "soft delete", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return len(m.byName("Deleted Items").items) == 1
	})

	// MOVE with the new destination UID.
	junk, _ := s.acct.Store().FolderByPath("Junk Email")
	out, err := s.acct.Move(ctx, f.ID, []uint32{4, 5}, junk.ID, nil)
	if err != nil || len(out.Moved) != 2 || out.Moved[0].Dst != 1 || out.Moved[1].Dst != 2 {
		t.Fatalf("move: %+v %v", out, err)
	}

	// Silent key reset (status 3) with a deletion hidden in the gap.
	gone := m.deliver("Inbox", realMail("gone@x", "gone", 0), true, now)
	eventually(t, "gone@x synced", func() bool { return s.count("INBOX") >= 11 })
	rec := &struct {
		mu  sync.Mutex
		exp []uint32
	}{}
	_, cancel2, _ := s.acct.Watch(f.ID, func(ev store.Event) {
		if ev.Kind == store.EvExpunge {
			rec.mu.Lock()
			rec.exp = append(rec.exp, ev.UID)
			rec.mu.Unlock()
		}
	})
	defer cancel2()
	goneUID, _ := s.acct.Store().UIDsByMessageID(f.ID, "gone@x")
	m.remove("Inbox", gone)
	m.forgetKeys("Inbox")
	eventually(t, "reconciliation removes the message deleted during the gap", func() bool {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return len(rec.exp) == 1 && rec.exp[0] == goneUID[0]
	})
	cur, _ := s.acct.Store().FolderByID(f.ID)
	if cur.UIDValidity != f.UIDValidity {
		t.Errorf("UIDVALIDITY changed by the reset")
	}

	// SendMail with the Sent copy arriving through Sync binds to the APPENDed copy.
	msg := []byte("Message-ID: <sent1@mailflow>\r\nFrom: alice@example.test\r\nTo: bob@example.test\r\nSubject: out\r\nDate: Fri, 09 Oct 2026 12:00:00 +0000\r\n\r\nhello\r\n")
	if err := s.acct.SendMail(ctx, msg); err != nil {
		t.Fatal(err)
	}
	res, err := s.acct.Append("Sent Items", msg, store.FlagSeen, "", time.Time{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "Exchange's Sent copy bound to the placeholder", func() bool {
		sf, _ := s.acct.Store().FolderByPath("Sent Items")
		mm, err := s.acct.Store().Message(sf.ID, res.UID)
		return err == nil && mm.ServerID != ""
	})
	time.Sleep(100 * time.Millisecond)
	sf, _ := s.acct.Store().FolderByPath("Sent Items")
	if c, _ := s.acct.Store().Counts(sf.ID); c.Messages != 1 {
		t.Errorf("Sent has %d messages: duplicated", c.Messages)
	}

	// The keys the library used were all committed by the engine.
	st := s.mgr.Status()
	if len(st.Accounts) != 1 || st.Accounts[0].AuthFailed || st.Accounts[0].Pending != 0 {
		t.Errorf("status = %+v", st)
	}
	// Privacy of the whole run.
	logs := s.logText()
	for _, bad := range []string{pass, user, "alice%40", "pushed", "m03@x", "b@example.test"} {
		if strings.Contains(logs, bad) {
			t.Errorf("log contains %q", bad)
		}
	}
}

func TestRemoteWipeOverTheRealLibrary(t *testing.T) {
	s := newStack(t)
	s.m.deliver("Inbox", realMail("w1@x", "s", 0), false, time.Now())
	eventually(t, "synced", func() bool { return s.count("INBOX") == 1 })
	// From now on every command is answered with 449 and the re-provision carries
	// the wipe order: the engine must honour it.
	var mu sync.Mutex
	wiped := false
	acks := 0
	s.m.f.on("Provision", func(r *http.Request, in *wbxml.Element) (int, *wbxml.Element) {
		mu.Lock()
		defer mu.Unlock()
		if in != nil && in.Find("RemoteWipe") != nil {
			acks++
			wiped = false
			return 0, e(wbxml.PageProvision, "Provision", e(wbxml.PageProvision, "Status", txt("1")))
		}
		if wiped {
			return 0, e(wbxml.PageProvision, "Provision", e(wbxml.PageProvision, "Status", txt("1")), e(wbxml.PageProvision, "RemoteWipe"))
		}
		return provisionOK("T", "F")(r, in)
	})
	pingH := s.m.ping
	s.m.f.on("Ping", func(r *http.Request, in *wbxml.Element) (int, *wbxml.Element) {
		mu.Lock()
		w := wiped
		mu.Unlock()
		if w {
			return 449, nil
		}
		return pingH(r, in)
	})
	mu.Lock()
	wiped = true
	mu.Unlock()
	eventually(t, "the wipe is acknowledged", func() bool { mu.Lock(); defer mu.Unlock(); return acks == 1 })
	eventually(t, "marker written", func() bool { return store.WipeMarkerExists(s.mgr.DB().Dir()) })
	if _, err := s.mgr.Login(ctx, user, pass); err == nil {
		t.Errorf("login accepted after a wipe")
	}
	if n := strings.Count(s.logText(), "REMOTE WIPE REQUESTED"); n != 1 {
		t.Errorf("%d loud wipe lines", n)
	}
}

func TestEngineWithRealLibraryAuthFailureStopsTraffic(t *testing.T) {
	s := newStack(t)
	s.m.deliver("Inbox", realMail("a1@x", "s", 0), false, time.Now())
	eventually(t, "synced", func() bool { return s.count("INBOX") == 1 })
	// Exchange starts answering 401 (password changed).
	for _, cmd := range []string{"Sync", "Ping", "FolderSync", "ItemOperations", "Provision"} {
		s.m.f.on(cmd, always(http.StatusUnauthorized, nil))
	}
	s.m.deliver("Inbox", realMail("a2@x", "s", 0), false, time.Now()) // wakes the Ping
	eventually(t, "401 noticed", func() bool { return s.acct.AuthFailed() })
	before := s.m.f.count("Sync") + s.m.f.count("Ping") + s.m.f.count("Provision")
	time.Sleep(300 * time.Millisecond)
	if after := s.m.f.count("Sync") + s.m.f.count("Ping") + s.m.f.count("Provision"); after != before {
		t.Errorf("traffic continued after the 401: %d -> %d requests", before, after)
	}
	if _, err := s.mgr.Login(ctx, user, pass); err == nil {
		t.Errorf("the rejected password was accepted again")
	}
	if s.m.f.count("Provision") != s.m.f.count("Provision") { // keep vet quiet about unused helpers
		t.Log("unreachable")
	}
	_ = strconv.Itoa
	_ = context.Background
}

// v0.1.5, finding 1: a Provision status that is not a rejected password (here 165,
// DeviceInformationRequired; the same holds for 139, 141 and 145) fails the login,
// is explained in the log in one line, and is remembered like every failed
// verification: the same credential is refused locally for the cooldown, nothing
// goes to Exchange again, and the per-user budget of unsuccessful verifications
// still holds. Nothing is retried on the way.
func TestAProvisionStatusAtLoginIsExplainedAndKeepsTheLockoutProtections(t *testing.T) {
	s := buildStack(t)
	s.m.f.on("Provision", always(0, provStatus("165")))
	requests := func() int { return len(s.m.f.all()) }

	_, err := s.mgr.Login(ctx, user, pass)
	if !errors.Is(err, engine.ErrUnavailable) || errors.Is(err, engine.ErrBadCredentials) {
		t.Fatalf("login: %v (the password was fine: this must not look like a wrong password)", err)
	}
	if !strings.Contains(err.Error(), "server requires device information in Provision") {
		t.Errorf("the refusal does not say why: %v", err)
	}
	// OPTIONS and one Provision: no second route, no retry.
	if n := requests(); n != 2 || s.m.f.count("Provision") != 1 || s.m.f.count("Settings") != 0 {
		t.Fatalf("%d requests (Provision %d, Settings %d), want OPTIONS and one Provision", n, s.m.f.count("Provision"), s.m.f.count("Settings"))
	}
	logs := s.logText()
	line := ""
	for _, l := range strings.Split(logs, "\n") {
		if strings.Contains(l, "cannot verify a login with Exchange") {
			line = l
		}
	}
	if line == "" || !strings.Contains(line, "server requires device information in Provision") || !strings.Contains(line, "status 165") {
		t.Errorf("no clear log line about the refusal:\n%s", logs)
	}

	// The same credential again: refused locally, Exchange hears nothing.
	_, err = s.mgr.Login(ctx, user, pass)
	var te *engine.ThrottledError
	if !errors.As(err, &te) || errors.Is(err, engine.ErrBadCredentials) {
		t.Fatalf("second login: %v, want a local refusal that is not a wrong-password answer", err)
	}
	if n := requests(); n != 2 {
		t.Errorf("a refused login reached Exchange (%d requests)", n)
	}
	// Another password may be tried once more (two unsuccessful verifications per
	// window), after that the budget holds.
	if _, err := s.mgr.Login(ctx, user, pass+"-2"); !errors.Is(err, engine.ErrUnavailable) {
		t.Fatalf("second password: %v", err)
	}
	if n := requests(); n != 4 {
		t.Errorf("%d requests after the second verification, want 4", n)
	}
	if _, err := s.mgr.Login(ctx, user, pass+"-3"); !errors.As(err, &te) {
		t.Fatalf("third password: %v, want the budget to hold", err)
	}
	if n := requests(); n != 4 {
		t.Errorf("the budget did not hold: %d requests", n)
	}
	// Privacy: neither password is anywhere in the log.
	for _, bad := range []string{pass, pass + "-2", pass + "-3"} {
		if strings.Contains(s.logText(), bad) {
			t.Errorf("log contains a password")
		}
	}
}

// Statuses 139, 141 and 145 of a strict policy: the same treatment as 165.
func TestStrictPolicyStatusesAtLoginAreRememberedToo(t *testing.T) {
	for _, code := range []string{"139", "141", "145"} {
		s := buildStack(t)
		s.m.f.on("Provision", always(0, provStatus(code)))
		_, err := s.mgr.Login(ctx, user, pass)
		if !errors.Is(err, engine.ErrUnavailable) || !strings.Contains(err.Error(), "status "+code) || !strings.Contains(err.Error(), "Exchange administrator") {
			t.Errorf("%s: %v", code, err)
		}
		if _, err := s.mgr.Login(ctx, user, pass); err == nil || len(s.m.f.all()) != 2 {
			t.Errorf("%s: second login %v after %d requests", code, err, len(s.m.f.all()))
		}
	}
}

// The log says how long Exchange took to verify a login: the first real Provision
// took 50 s, and a login that waits that long is otherwise a mystery.
func TestTheLogSaysHowLongTheVerificationTook(t *testing.T) {
	s := buildStack(t)
	s.m.f.delay = 60 * time.Millisecond // OPTIONS and the two Provision phases
	a, err := s.mgr.Login(ctx, user, pass)
	if err != nil {
		t.Fatal(err)
	}
	s.acct = a
	var line string
	for _, l := range strings.Split(s.logText(), "\n") {
		if strings.Contains(l, "mailbox engine started") {
			line = l
		}
	}
	m := regexp.MustCompile(`verified_in=(\S+)`).FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("no verified_in in %q", line)
	}
	d, err := time.ParseDuration(m[1])
	if err != nil || d < 150*time.Millisecond || d > 5*time.Second {
		t.Errorf("verified_in = %q (%v, %v), want about 180ms", m[1], d, err)
	}
}
