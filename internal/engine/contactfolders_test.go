//go:build devfake

package engine_test

import (
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"easbridge/internal/engine"
	"easbridge/internal/fakeupstream"
	"easbridge/internal/logx"
	"easbridge/internal/store"
)

// The upgrade from 0.1.8: the stored FolderSync key is older than the contacts feature, so an
// incremental FolderSync never mentions the Contacts folders. The engine reads the hierarchy
// again from key "0", once (schema 8, accounts.contact_folders_kept).

// downgradeTo rewinds a closed state database to an older schema, the way 0.1.8 (5), 0.2.0
// (6) and 0.2.1 (7) left it: the tables and columns of the later migrations are dropped.
func downgradeTo(t *testing.T, dir string, version int) {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, store.DBFileName))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var qs []string
	qs = append(qs, `ALTER TABLE accounts DROP COLUMN contact_folders_kept`)
	if version <= 6 {
		qs = append(qs, `DROP TABLE directory_sweep_queue`, `DROP TABLE directory_sweep`,
			`ALTER TABLE directory_cards DROP COLUMN by_lookup`, `ALTER TABLE directory_cards DROP COLUMN by_sweep`,
			`ALTER TABLE directory_cards DROP COLUMN sweep_seen`, `ALTER TABLE directory_cards DROP COLUMN confirmed_at`)
	}
	if version <= 5 {
		qs = append(qs, `DROP TABLE contact_cards`, `DROP TABLE contact_folders`, `DROP TABLE directory_cards`,
			`DROP TABLE directory_candidates`, `DROP TABLE directory_state`)
	}
	qs = append(qs, fmt.Sprintf(`PRAGMA user_version = %d`, version))
	for _, q := range qs {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// reopen stops the engines, optionally rewinds the database to an older schema, and starts a
// new manager over it with the contacts feature on or off (the settings of restart).
func (e *env) reopen(schema int, contacts bool) {
	e.t.Helper()
	e.mgr.Close()
	if schema > 0 {
		if err := e.mgr.DB().Close(); err != nil {
			e.t.Fatal(err)
		}
		downgradeTo(e.t, e.dir, schema)
		db, err := store.Open(e.dir)
		if err != nil {
			e.t.Fatal(err)
		}
		db.SetClock(e.clock.Now)
		e.db = db
	}
	cfg := engine.Config{
		Grace: time.Hour, PingHeartbeat: 300 * time.Millisecond, PingMinHeartbeat: 40 * time.Millisecond,
		AuthFailCooldown: 15 * time.Minute, MaxMessageBytes: 1 << 20, PlaceholderTTL: 24 * time.Hour, PlaceholderWindow: 10 * time.Minute,
		ReconVerifyDelay: 200 * time.Millisecond, FetchWorkers: 2, OutboxBatch: 100, Housekeeping: 30 * time.Millisecond,
		Backoff: func(int) time.Duration { return 15 * time.Millisecond }, Clock: e.clock.Now, CallTimeout: 10 * time.Second,
		BreakerBase: time.Microsecond, BreakerMax: 4 * time.Microsecond,
		ContactsEnabled: contacts, ContactsInterval: 60 * time.Millisecond,
	}
	log := logx.New(logx.Options{Level: slog.LevelDebug, Writer: e.logs})
	e.mgr = engine.NewManager(cfg, log, e.db, e.srv.Factory(4096), func(u string) bool { return u == testUser })
}

func (e *env) zeroKeys() int {
	n := 0
	for _, k := range e.srv.FolderSyncKeys(testUser) {
		if k == "0" {
			n++
		}
	}
	return n
}

func (e *env) keptMarker(a *engine.Account) bool {
	e.t.Helper()
	k, err := a.Store().ContactFoldersKept()
	if err != nil {
		e.t.Fatal(err)
	}
	return k
}

// upgradeFixture is a mailbox as 0.1.8 left it: mail synced, a committed FolderSync key, no
// contact folders although Exchange has two, and (when kept is set) the messages and counts
// to compare with afterwards.
type upgradeFixture struct {
	e       *env
	inbox   *store.Folder
	uids    []uint32
	counts  store.Counts
	hierKey string
}

func newUpgradeFixture(t *testing.T, mod func(*engine.Config, *fakeupstream.Server)) *upgradeFixture {
	e := newEnv(t, func(c *engine.Config, s *fakeupstream.Server) {
		s.Window = 3
		if mod != nil {
			mod(c, s)
		}
	})
	inboxID := e.srv.FolderID(testUser, "Inbox")
	for i := 0; i < 5; i++ {
		e.srv.AddMessage(testUser, inboxID, mailBytes(fmt.Sprintf("u%d@x", i), "s", e.clock.Now().Add(time.Duration(i-10)*time.Hour), ""), fakeupstream.AddOptions{})
	}
	def := e.srv.AddContactFolder(testUser, "Contacts", fakeupstream.TypeContacts)
	e.srv.AddContact(testUser, def, person("Ada", "Lovelace", "ada@corp.example"))
	usr := e.srv.AddContactFolder(testUser, "Clients", fakeupstream.TypeUserContacts)
	e.srv.AddContact(testUser, usr, person("Bob", "Client", "bob@corp.example"))

	// The contacts are off: this is 0.1.8 (or 0.2.x without EASBRIDGE_CARDDAV_ADDR).
	a := e.login()
	f := e.mirrored(a, "Inbox", "INBOX")
	eventually(t, "five messages", func() bool { return serverCount(a, f.ID) == 5 })
	if fs, _ := a.Store().ContactFolders(); len(fs) != 0 {
		t.Fatalf("contact folders with the feature off: %+v", fs)
	}
	key, _ := a.Store().FolderSyncKey()
	if key == "0" || e.keptMarker(a) {
		t.Fatalf("key %q, marker %v: want a committed key and no marker", key, e.keptMarker(a))
	}
	ms, _ := a.Store().ListMessages(f.ID)
	counts, _ := a.Store().Counts(f.ID)
	return &upgradeFixture{e: e, inbox: f, uids: uidsOf(ms), counts: counts, hierKey: key}
}

// mailUnchanged checks what the upgrade must not touch: the messages and their UIDs, the
// counts, UIDVALIDITY and UIDNEXT of the Inbox, and no announcement of new mail.
func (u *upgradeFixture) mailUnchanged(a *engine.Account, rec *recorder) {
	t := u.e.t
	t.Helper()
	cur, err := a.Store().FolderByID(u.inbox.ID)
	if err != nil || cur.UIDValidity != u.inbox.UIDValidity {
		t.Errorf("UIDVALIDITY changed: %+v %v", cur, err)
	}
	ms, _ := a.Store().ListMessages(u.inbox.ID)
	if fmt.Sprint(uidsOf(ms)) != fmt.Sprint(u.uids) {
		t.Errorf("UIDs %v, were %v", uidsOf(ms), u.uids)
	}
	if c, _ := a.Store().Counts(u.inbox.ID); c.Messages != u.counts.Messages || c.UIDNext != u.counts.UIDNext {
		t.Errorf("counts %+v, were %+v", c, u.counts)
	}
	if rec != nil {
		for _, kind := range []store.EventKind{store.EvAdd, store.EvBulkAdd, store.EvExpunge} {
			if n := rec.count(kind); n != 0 {
				t.Errorf("%d events of kind %v announced to an IMAP session: %+v", n, kind, rec.all())
			}
		}
	}
}

func TestAnUpgradedInstallDiscoversTheContactFoldersWithOneFullFolderSync(t *testing.T) {
	for _, resets := range []bool{false, true} {
		name := "Exchange keeps the collections' Sync keys"
		if resets {
			name = "Exchange invalidates the collections' Sync keys"
		}
		t.Run(name, func(t *testing.T) {
			u := newUpgradeFixture(t, func(c *engine.Config, s *fakeupstream.Server) { s.FolderSyncZeroResetsKeys = resets })
			e := u.e
			if e.zeroKeys() != 1 {
				t.Fatalf("keys %v", e.srv.FolderSyncKeys(testUser))
			}
			// The upgrade: 0.1.8's database (schema 5) opened by this build with the contacts on.
			e.reopen(5, true)
			a := e.login()
			rec, _ := e.watch(a, u.inbox)
			def := e.contactFolder(a, store.DefaultBookID)
			usr := e.contactFolderByType(a, store.TypeUserContacts)
			e.waitCards(a, def.ID, 1)
			e.waitCards(a, usr.ID, 1)
			if e.zeroKeys() != 2 {
				t.Errorf("FolderSync keys %v: want exactly one more full FolderSync", e.srv.FolderSyncKeys(testUser))
			}
			if !e.keptMarker(a) {
				t.Errorf("the marker is not set after the full hierarchy")
			}
			if !strings.Contains(e.logs.String(), "reading the hierarchy again in full, once") {
				t.Errorf("no log line about the one full FolderSync")
			}
			if !strings.Contains(e.logs.String(), "full=true contact_folders_added=2 contact_folders_removed=0") {
				t.Errorf("the folder sync line does not show the two contact folders:\n%s", e.logs.String())
			}
			// Mail: the same messages with the same UIDs, found again if Exchange dropped the keys.
			if resets {
				eventually(t, "the Inbox was synced from scratch and reconciled", func() bool {
					f, _ := a.Store().FolderByID(u.inbox.ID)
					return f != nil && !f.ReconActive && f.ReconVerifyAt == 0 && strings.Contains(e.logs.String(), "server reset the sync key")
				})
			}
			consistently(t, "nothing else happens to the mail", 300*time.Millisecond, func() bool { return serverCount(a, u.inbox.ID) == 5 })
			u.mailUnchanged(a, rec)

			// A restart does not repeat it.
			zeros, before := e.zeroKeys(), len(e.srv.FolderSyncKeys(testUser))
			e.reopen(0, true)
			a = e.login()
			e.contactFolder(a, store.DefaultBookID)
			eventually(t, "a FolderSync after the restart", func() bool { return len(e.srv.FolderSyncKeys(testUser)) > before })
			consistently(t, "no second full FolderSync", 200*time.Millisecond, func() bool { return e.zeroKeys() == zeros })
			u.mailUnchanged(a, nil)

			// Off and on again: one more.
			e.reopen(0, false)
			a = e.login()
			eventually(t, "the marker is cleared by a hierarchy taken with the contacts off", func() bool { return !e.keptMarker(a) })
			if fs, _ := a.Store().ContactFolders(); len(fs) != 0 {
				// The feature is off in the engine; the rows go at the next start of the service
				// (removeStaleContactData), which is tested in cmd/easbridge.
				_ = fs
			}
			e.reopen(0, true)
			a = e.login()
			eventually(t, "one more full FolderSync", func() bool { return e.zeroKeys() == zeros+1 && e.keptMarker(a) })
			consistently(t, "and not another", 200*time.Millisecond, func() bool { return e.zeroKeys() == zeros+1 })
			e.contactFolder(a, store.DefaultBookID)
			u.mailUnchanged(a, nil)
		})
	}
}

func (e *env) contactFolderByType(a *engine.Account, typ int) *store.ContactFolder {
	e.t.Helper()
	var f *store.ContactFolder
	eventually(e.t, fmt.Sprintf("contact folder of type %d", typ), func() bool {
		fs, _ := a.Store().ContactFolders()
		for i := range fs {
			if fs[i].EASType == typ {
				f = &fs[i]
				return true
			}
		}
		return false
	})
	return f
}

func TestTheFullFolderSyncOfAnUpgradeAlsoRemovesAContactFolderExchangeNoLongerHas(t *testing.T) {
	// A database from 0.2.1 (schema 7) that holds a contact folder Exchange dropped meanwhile:
	// the full hierarchy applies the contact folders with Full set, so the stale one goes.
	e := newEnv(t, withContacts())
	gone := e.srv.AddContactFolder(testUser, "Old", fakeupstream.TypeUserContacts)
	e.srv.AddContact(testUser, gone, person("Old", "Friend", "old@corp.example"))
	keep := e.srv.AddContactFolder(testUser, "Contacts", fakeupstream.TypeContacts)
	e.srv.AddContact(testUser, keep, person("Ada", "Lovelace", "ada@corp.example"))
	a := e.login()
	e.waitCards(a, e.contactFolderByType(a, store.TypeUserContacts).ID, 1)
	e.contactFolder(a, store.DefaultBookID)
	if !e.keptMarker(a) {
		t.Fatal("the marker is not set after a first hierarchy with the contacts on")
	}
	e.mgr.Close()
	e.srv.RemoveFolder(testUser, gone)
	e.reopen(7, true) // the marker column is gone: as 0.2.1 left it
	a = e.login()
	eventually(t, "the stale contact folder is removed", func() bool {
		fs, _ := a.Store().ContactFolders()
		return len(fs) == 1 && fs[0].EASType == store.TypeContacts
	})
}

func TestEverySchemaFromFiveToSevenGetsTheOneFullFolderSync(t *testing.T) {
	for _, schema := range []int{5, 6, 7} {
		t.Run(fmt.Sprintf("schema %d", schema), func(t *testing.T) {
			u := newUpgradeFixture(t, nil)
			u.e.reopen(schema, true)
			a := u.e.login()
			u.e.contactFolder(a, store.DefaultBookID)
			if n := u.e.zeroKeys(); n != 2 {
				t.Errorf("%d full FolderSyncs, want the first one and one for the upgrade: %v", n, u.e.srv.FolderSyncKeys(testUser))
			}
			u.mailUnchanged(a, nil)
		})
	}
}

func TestWithTheContactsOffTheHierarchyIsNeverReadAgainInFull(t *testing.T) {
	u := newUpgradeFixture(t, nil) // the contacts are off: one FolderSync from key 0, the first
	e := u.e
	before := len(e.srv.FolderSyncKeys(testUser))
	for i := 0; i < 2; i++ {
		e.reopen(0, false)
		a := e.login()
		e.mirrored(a, "Inbox", "INBOX")
		n := before + i + 1
		eventually(t, "a FolderSync after the restart", func() bool { return len(e.srv.FolderSyncKeys(testUser)) >= n })
	}
	if e.zeroKeys() != 1 {
		t.Errorf("FolderSync keys %v: the marker must not force a full hierarchy while the contacts are off", e.srv.FolderSyncKeys(testUser))
	}
}
