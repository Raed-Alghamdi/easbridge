package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTemp(t *testing.T) (*DB, *Account) {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "data dir"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	acct, err := db.Account("Alice@Example.test")
	if err != nil {
		t.Fatal(err)
	}
	return db, acct
}

func rawMsg(id, subject, extra string) []byte {
	return []byte("Message-ID: <" + id + ">\r\nSubject: " + subject + "\r\nDate: Tue, 06 Oct 2026 10:00:00 +0000\r\nTo: bob@example.test\r\n" + extra + "\r\nbody of " + id + "\r\n")
}

// standardTree is a typical Exchange hierarchy.
func standardTree() []FolderInput {
	return []FolderInput{
		{ServerID: "1", ParentServerID: "0", DisplayName: "Inbox", EASType: TypeInbox},
		{ServerID: "2", ParentServerID: "0", DisplayName: "Drafts", EASType: TypeDrafts},
		{ServerID: "3", ParentServerID: "0", DisplayName: "Deleted Items", EASType: TypeDeletedItems},
		{ServerID: "4", ParentServerID: "0", DisplayName: "Sent Items", EASType: TypeSentItems},
		{ServerID: "5", ParentServerID: "0", DisplayName: "Outbox", EASType: TypeOutbox},
		{ServerID: "6", ParentServerID: "0", DisplayName: "Junk Email", EASType: TypeUserMail},
		{ServerID: "7", ParentServerID: "0", DisplayName: "Archive", EASType: TypeUserMail},
		{ServerID: "8", ParentServerID: "0", DisplayName: "Calendar", EASType: 8},
		{ServerID: "9", ParentServerID: "1", DisplayName: "Projects", EASType: TypeUserMail},
		{ServerID: "10", ParentServerID: "9", DisplayName: "Clients/2026", EASType: TypeUserMail},
	}
}

func applyStandard(t *testing.T, a *Account) map[string]*Folder {
	t.Helper()
	if _, err := a.Update(nil, func(tx *Tx) error {
		_, err := tx.ApplyFolders(standardTree(), nil, nil, true)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	out := map[string]*Folder{}
	fs, err := a.Folders()
	if err != nil {
		t.Fatal(err)
	}
	for i := range fs {
		out[fs[i].ServerID] = &fs[i]
	}
	return out
}

func TestOpenMigratesAndReopens(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "with space")
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	v, err := db.SchemaVersion()
	if err != nil || v != len(migrations) {
		t.Fatalf("schema version = %d, %v; want %d", v, err, len(migrations))
	}
	st, err := os.Stat(filepath.Join(dir, DBFileName))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("db file mode = %v, want 0600", st.Mode().Perm())
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("second Close should be a no-op: %v", err)
	}
	db2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	if v, _ := db2.SchemaVersion(); v != len(migrations) {
		t.Errorf("schema version after reopen = %d", v)
	}
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.w.Exec(fmt.Sprintf("PRAGMA user_version = %d", len(migrations)+5)); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("want refusal of a newer schema, got %v", err)
	}
}

func TestDeviceIDStable(t *testing.T) {
	dir := t.TempDir()
	db1, _ := Open(dir)
	id1, err := db1.DeviceID()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id1) {
		t.Fatalf("device id %q is not 32 hex chars", id1)
	}
	// A second process (the probe) opens the same directory.
	db2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	id2, _ := db2.DeviceID()
	if id1 != id2 {
		t.Errorf("device id differs between handles: %s vs %s", id1, id2)
	}
	db1.Close()
	db2.Close()
	db3, _ := Open(dir)
	defer db3.Close()
	if id3, _ := db3.DeviceID(); id3 != id1 {
		t.Errorf("device id changed after reopen")
	}
}

func TestAccountIsCaseInsensitiveAndStable(t *testing.T) {
	db, a := openTemp(t)
	b, err := db.Account("  ALICE@example.TEST ")
	if err != nil || b.ID != a.ID || b.User != "alice@example.test" {
		t.Fatalf("account lookup: %+v %v", b, err)
	}
	if _, err := db.Account(" "); err == nil {
		t.Error("empty user must be rejected")
	}
}

func TestStagedKeysCommitWithItemsAtomically(t *testing.T) {
	_, a := openTemp(t)
	folders := applyStandard(t, a)
	inbox := folders["1"]

	// Items and the new sync key are committed together ...
	events, err := a.Update("sync", func(tx *Tx) error {
		if _, err := tx.AddMessage(inbox.ID, NewMessage{ServerID: "1:1", MIME: rawMsg("a@x", "one", "")}); err != nil {
			return err
		}
		if err := tx.SetSyncKey(inbox.ID, "key-2"); err != nil {
			return err
		}
		if err := tx.SetFolderSyncKey("hier-2"); err != nil {
			return err
		}
		return tx.SetPolicyKey("policy-9")
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != EvAdd || events[0].Origin != "sync" {
		t.Fatalf("events = %+v", events)
	}
	got, _ := a.FolderByID(inbox.ID)
	if got.SyncKey != "key-2" {
		t.Fatalf("sync key = %q", got.SyncKey)
	}
	if k, _ := a.FolderSyncKey(); k != "hier-2" {
		t.Errorf("folder sync key = %q", k)
	}
	if k, _ := a.PolicyKey(); k != "policy-9" {
		t.Errorf("policy key = %q", k)
	}

	// ... and a failure after the key was staged rolls both back (so the batch
	// is replayed from the old key after a crash, at-least-once).
	boom := errors.New("boom")
	events, err = a.Update(nil, func(tx *Tx) error {
		if _, err := tx.AddMessage(inbox.ID, NewMessage{ServerID: "1:2", MIME: rawMsg("b@x", "two", "")}); err != nil {
			return err
		}
		if err := tx.SetSyncKey(inbox.ID, "key-3"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) || len(events) != 0 {
		t.Fatalf("err=%v events=%v", err, events)
	}
	got, _ = a.FolderByID(inbox.ID)
	if got.SyncKey != "key-2" {
		t.Errorf("sync key advanced despite rollback: %q", got.SyncKey)
	}
	if c, _ := a.Counts(inbox.ID); c.Messages != 1 || c.UIDNext != 2 {
		t.Errorf("counts after rollback = %+v (UIDNEXT must not advance either)", c)
	}
}

func TestApplyFoldersLayoutAndSpecialUse(t *testing.T) {
	_, a := openTemp(t)
	f := applyStandard(t, a)

	want := map[string]struct{ path, use string }{
		"1":  {"INBOX", ""},
		"2":  {"Drafts", `\Drafts`},
		"3":  {"Deleted Items", `\Trash`},
		"4":  {"Sent Items", `\Sent`},
		"6":  {"Junk Email", `\Junk`},
		"7":  {"Archive", `\Archive`},
		"9":  {"INBOX/Projects", ""},
		"10": {"INBOX/Projects/Clients_2026", ""},
	}
	for sid, w := range want {
		got, ok := f[sid]
		if !ok {
			t.Errorf("folder %s missing", sid)
			continue
		}
		if got.Path != w.path || got.SpecialUse != w.use {
			t.Errorf("folder %s: path=%q use=%q, want %q %q", sid, got.Path, got.SpecialUse, w.path, w.use)
		}
		if got.UIDValidity == 0 || got.UIDNext != 1 || got.SyncKey != "0" {
			t.Errorf("folder %s: bad initial state %+v", sid, got)
		}
	}
	if _, ok := f["5"]; ok {
		t.Error("Outbox must be hidden")
	}
	if _, ok := f["8"]; ok {
		t.Error("Calendar must be ignored")
	}
	if got, err := a.FolderByPath("inbox"); err != nil || got.ServerID != "1" {
		t.Errorf("INBOX lookup is case-insensitive: %v %v", got, err)
	}
	if got, err := a.SpecialFolder(`\Sent`); err != nil || got.ServerID != "4" {
		t.Errorf("SpecialFolder: %v %v", got, err)
	}
	if _, err := a.FolderByPath("Nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown path: %v", err)
	}
}

func TestSpecialUseArabicNames(t *testing.T) {
	_, a := openTemp(t)
	_, err := a.Update(nil, func(tx *Tx) error {
		_, err := tx.ApplyFolders([]FolderInput{
			{ServerID: "1", ParentServerID: "0", DisplayName: "البريد الوارد", EASType: TypeInbox},
			{ServerID: "2", ParentServerID: "0", DisplayName: "البريد الإلكتروني غير الهام", EASType: TypeUserMail},
			{ServerID: "3", ParentServerID: "0", DisplayName: "الأرشيف", EASType: TypeUserMail},
			{ServerID: "4", ParentServerID: "0", DisplayName: "SPAM", EASType: TypeUserMail},
			{ServerID: "5", ParentServerID: "1", DisplayName: "Archive", EASType: TypeUserMail}, // nested: not the archive
		}, nil, nil, true)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	fs, _ := a.Folders()
	use := map[string]string{}
	for _, f := range fs {
		use[f.ServerID] = f.SpecialUse
	}
	// "البريد الإلكتروني غير الهام" ranks below the English "spam"? Only one
	// folder may carry \Junk, and it must be deterministic.
	junkCount := 0
	for _, u := range use {
		if u == `\Junk` {
			junkCount++
		}
	}
	if junkCount != 1 {
		t.Fatalf("exactly one \\Junk expected, got %d: %v", junkCount, use)
	}
	if use["3"] != `\Archive` {
		t.Errorf("Arabic archive folder not detected: %v", use)
	}
	if use["5"] != "" {
		t.Errorf("nested Archive must not be special: %v", use)
	}
	if use["1"] != "" {
		t.Errorf("Inbox has no special-use attribute: %v", use)
	}
}

func TestApplyFoldersKeepsUIDValidityAndHandlesRenameDeleteAndReset(t *testing.T) {
	_, a := openTemp(t)
	f := applyStandard(t, a)
	inbox := f["1"]
	proj := f["9"]

	// Rename "Projects" and reparent the child: the folder keeps its identity.
	var diff FolderDiff
	_, err := a.Update(nil, func(tx *Tx) error {
		var err error
		diff, err = tx.ApplyFolders(nil, []FolderInput{
			{ServerID: "9", ParentServerID: "0", DisplayName: "Work", EASType: TypeUserMail},
		}, nil, false)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	renamed, _ := a.FolderByServerID("9")
	if renamed.Path != "Work" || renamed.UIDValidity != proj.UIDValidity || renamed.ID != proj.ID {
		t.Errorf("rename lost identity: %+v vs %+v", renamed, proj)
	}
	child, _ := a.FolderByServerID("10")
	if child.Path != "Work/Clients_2026" {
		t.Errorf("child path = %q", child.Path)
	}
	if len(diff.Changed) < 2 {
		t.Errorf("diff.Changed should list the folder and its child: %+v", diff.Changed)
	}

	// Explicit delete.
	_, err = a.Update(nil, func(tx *Tx) error {
		diff, err = tx.ApplyFolders(nil, nil, []string{"10"}, false)
		return err
	})
	if err != nil || len(diff.Removed) != 1 || diff.Removed[0].ServerID != "10" {
		t.Fatalf("delete: %v %+v", err, diff)
	}

	// A full resync that no longer lists Archive and Junk removes them, keeps the rest.
	_, err = a.Update(nil, func(tx *Tx) error {
		var in []FolderInput
		for _, f := range standardTree() {
			if f.ServerID != "6" && f.ServerID != "7" && f.ServerID != "10" {
				in = append(in, f)
			}
		}
		diff, err = tx.ApplyFolders(in, nil, nil, true)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	removed := map[string]bool{}
	for _, r := range diff.Removed {
		removed[r.ServerID] = true
	}
	if !removed["6"] || !removed["7"] || len(removed) != 2 {
		t.Errorf("full resync removed %v", removed)
	}
	inbox2, _ := a.FolderByServerID("1")
	if inbox2.UIDValidity != inbox.UIDValidity {
		t.Errorf("a full folder resync must keep UIDVALIDITY (%d -> %d)", inbox.UIDValidity, inbox2.UIDValidity)
	}
}

func TestUIDValidityNeverRepeatsForRecreatedFolder(t *testing.T) {
	_, a := openTemp(t)
	var seen []uint32
	for i := 0; i < 3; i++ {
		_, err := a.Update(nil, func(tx *Tx) error {
			_, err := tx.ApplyFolders([]FolderInput{{ServerID: "7", ParentServerID: "0", DisplayName: "X", EASType: TypeUserMail}}, nil, nil, false)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		f, _ := a.FolderByServerID("7")
		seen = append(seen, f.UIDValidity)
		if _, err := a.Update(nil, func(tx *Tx) error {
			_, err := tx.ApplyFolders(nil, nil, []string{"7"}, false)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if !(seen[0] < seen[1] && seen[1] < seen[2]) {
		t.Fatalf("UIDVALIDITY must strictly increase for recreated folders: %v", seen)
	}
}

func TestDuplicateAndInboxNamesGetSuffix(t *testing.T) {
	_, a := openTemp(t)
	_, err := a.Update(nil, func(tx *Tx) error {
		_, err := tx.ApplyFolders([]FolderInput{
			{ServerID: "1", ParentServerID: "0", DisplayName: "Inbox", EASType: TypeInbox},
			{ServerID: "2", ParentServerID: "0", DisplayName: "inbox", EASType: TypeUserMail},
			{ServerID: "3", ParentServerID: "0", DisplayName: "Same", EASType: TypeUserMail},
			{ServerID: "4", ParentServerID: "0", DisplayName: "same", EASType: TypeUserMail},
		}, nil, nil, true)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	fs, _ := a.Folders()
	seen := map[string]bool{}
	for _, f := range fs {
		low := strings.ToLower(f.Path)
		if seen[low] {
			t.Errorf("duplicate path %q", f.Path)
		}
		seen[low] = true
	}
	if in, _ := a.FolderByServerID("1"); in.Path != "INBOX" {
		t.Errorf("real Inbox must keep its name, got %q", in.Path)
	}
}

func TestMessagesUIDsFlagsAndLookups(t *testing.T) {
	_, a := openTemp(t)
	inbox := applyStandard(t, a)["1"]

	add := func(sid, id string, flags Flags) *Message {
		var m *Message
		_, err := a.Update(nil, func(tx *Tx) error {
			var err error
			m, err = tx.AddMessage(inbox.ID, NewMessage{
				ServerID: sid, Flags: flags, InternalDate: time.Unix(1_700_000_000, 0),
				MIME: rawMsg(id, "subject "+id, "In-Reply-To: <p@x>\r\nReferences: <r1@x> <p@x>\r\n"),
			})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	m1 := add("1:1", "a@x", 0)
	m2 := add("1:2", "b@x", FlagSeen)
	m3 := add("1:3", "c@x", FlagSeen|FlagFlagged)
	if m1.UID != 1 || m2.UID != 2 || m3.UID != 3 {
		t.Fatalf("UIDs = %d %d %d", m1.UID, m2.UID, m3.UID)
	}
	if m1.MessageID != "a@x" || m1.Subject != "subject a@x" || m1.InReplyTo != "<p@x>" || m1.ServerID != "1:1" {
		t.Errorf("indexed fields wrong: %+v", m1)
	}
	if m1.Size != int64(len(rawMsg("a@x", "subject a@x", "In-Reply-To: <p@x>\r\nReferences: <r1@x> <p@x>\r\n"))) {
		t.Errorf("size = %d", m1.Size)
	}

	c, _ := a.Counts(inbox.ID)
	if c.Messages != 3 || c.Unseen != 1 || c.UIDNext != 4 || c.UIDValidity != inbox.UIDValidity {
		t.Errorf("counts = %+v", c)
	}

	// Delete the highest UID: the next one must not reuse it.
	if _, err := a.Update(nil, func(tx *Tx) error { _, err := tx.DeleteMessage(inbox.ID, 3); return err }); err != nil {
		t.Fatal(err)
	}
	m4 := add("1:4", "d@x", 0)
	if m4.UID != 4 {
		t.Errorf("UID reused: got %d, want 4", m4.UID)
	}

	// Lookups
	if uids, _ := a.UIDsByMessageID(inbox.ID, "b@x"); len(uids) != 1 || uids[0] != 2 {
		t.Errorf("UIDsByMessageID = %v", uids)
	}
	if uids, _ := a.UIDsByRef(inbox.ID, RefReferences, "r1@x"); len(uids) != 3 {
		t.Errorf("UIDsByRef(References) = %v (messages 1,2,4 share the reference)", uids)
	}
	if uids, _ := a.UIDsByRef(inbox.ID, RefInReplyTo, "p@x"); len(uids) != 3 {
		t.Errorf("UIDsByRef(In-Reply-To) = %v", uids)
	}
	if uids, _ := a.UIDsByRef(inbox.ID, RefInReplyTo, "nobody@x"); len(uids) != 0 {
		t.Errorf("UIDsByRef unknown = %v", uids)
	}
	mime, err := a.MIME(inbox.ID, 2)
	if err != nil || !strings.Contains(string(mime), "body of b@x") {
		t.Errorf("MIME = %q %v", mime, err)
	}
	full, err := a.Message(inbox.ID, 2)
	if err != nil || !strings.HasPrefix(string(full.Header), "Message-ID: <b@x>") || strings.Contains(string(full.Header), "body of") {
		t.Errorf("Header = %q %v", full.Header, err)
	}
	if bySID, err := a.MessageByServerID(inbox.ID, "1:2"); err != nil || bySID.UID != 2 {
		t.Errorf("MessageByServerID = %v %v", bySID, err)
	}
	metas, _ := a.ListMessages(inbox.ID)
	if len(metas) != 3 || metas[0].UID != 1 || metas[2].UID != 4 || metas[1].Flags != FlagSeen || metas[1].InternalDate != 1_700_000_000 {
		t.Errorf("ListMessages = %+v", metas)
	}
	rows, _ := a.SearchRows(inbox.ID)
	if r := rows[2]; r.MessageID != "b@x" || r.To != "bob@example.test" || r.DateHdr == 0 {
		t.Errorf("SearchRows[2] = %+v", r)
	}

	// Duplicate ServerId is refused by the unique index.
	_, err = a.Update(nil, func(tx *Tx) error {
		_, err := tx.AddMessage(inbox.ID, NewMessage{ServerID: "1:1", MIME: rawMsg("dup@x", "dup", "")})
		return err
	})
	if err == nil {
		t.Error("duplicate ServerId accepted")
	}

	// Flags: change emits an event, no-op does not.
	events, err := a.Update("sess", func(tx *Tx) error {
		if ch, err := tx.SetFlags(inbox.ID, 1, FlagSeen|FlagFlagged, "$x"); err != nil || !ch {
			return fmt.Errorf("first SetFlags: %v %v", ch, err)
		}
		if ch, err := tx.SetFlags(inbox.ID, 1, FlagSeen|FlagFlagged, "$x"); err != nil || ch {
			return fmt.Errorf("repeat SetFlags should be a no-op: %v %v", ch, err)
		}
		return nil
	})
	if err != nil || len(events) != 1 || events[0].Kind != EvFlags || events[0].Flags != FlagSeen|FlagFlagged || events[0].Keywords != "$x" {
		t.Fatalf("events=%+v err=%v", events, err)
	}

	// Deleting cascades to MIME and ref rows.
	events, err = a.Update(nil, func(tx *Tx) error {
		if ok, err := tx.DeleteMessage(inbox.ID, 1); err != nil || !ok {
			return fmt.Errorf("delete: %v %v", ok, err)
		}
		if ok, err := tx.DeleteMessage(inbox.ID, 1); err != nil || ok {
			return fmt.Errorf("second delete should report false: %v %v", ok, err)
		}
		return nil
	})
	if err != nil || len(events) != 1 || events[0].Kind != EvExpunge || events[0].UID != 1 {
		t.Fatalf("delete events=%+v err=%v", events, err)
	}
	if _, err := a.MIME(inbox.ID, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("MIME of deleted message: %v", err)
	}
	var n int
	if err := a.db.r.QueryRow(`SELECT COUNT(*) FROM message_refs WHERE message_pk NOT IN (SELECT id FROM messages)`).Scan(&n); err != nil || n != 0 {
		t.Errorf("orphaned refs: %d %v", n, err)
	}
}

func TestPlaceholderBindAndExpiry(t *testing.T) {
	db, a := openTemp(t)
	sent := applyStandard(t, a)["4"]
	now := time.Unix(1_800_000_000, 0)
	db.SetClock(func() time.Time { return now })

	var ph *Message
	_, err := a.Update(nil, func(tx *Tx) error {
		var err error
		ph, err = tx.AddMessage(sent.ID, NewMessage{Kind: KindPlaceholder, Flags: FlagSeen, MIME: rawMsg("sent1@x", "hi", ""),
			PlaceholderExpires: now.Add(24 * time.Hour)})
		if err != nil {
			return err
		}
		_, err = tx.AddMessage(sent.ID, NewMessage{Kind: KindPlaceholder, MIME: rawMsg("sent2@x", "hi2", ""),
			PlaceholderExpires: now.Add(time.Hour)})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// Bind the first one to a server item: UID stays, no event.
	events, err := a.Update(nil, func(tx *Tx) error { return tx.BindServerID(sent.ID, ph.UID, "4:77") })
	if err != nil || len(events) != 0 {
		t.Fatalf("bind: events=%v err=%v", events, err)
	}
	bound, _ := a.Message(sent.ID, ph.UID)
	if bound.ServerID != "4:77" || bound.Kind != KindServer || bound.UID != ph.UID || !bound.PlaceholderExpires.IsZero() {
		t.Errorf("bound = %+v", bound)
	}
	if _, err := a.Update(nil, func(tx *Tx) error { return tx.BindServerID(sent.ID, ph.UID, "4:78") }); err == nil {
		t.Error("binding an already bound message must fail")
	}

	// 30 minutes later nothing expires; two hours later the second does.
	now = now.Add(30 * time.Minute)
	n := 0
	a.Update(nil, func(tx *Tx) error { var err error; n, err = tx.ExpirePlaceholders(); return err })
	if n != 0 {
		t.Errorf("expired %d too early", n)
	}
	now = now.Add(90 * time.Minute)
	a.Update(nil, func(tx *Tx) error { var err error; n, err = tx.ExpirePlaceholders(); return err })
	if n != 1 {
		t.Errorf("expired %d, want 1", n)
	}
	m2, _ := a.Message(sent.ID, ph.UID+1)
	if m2.Kind != KindLocalOnly {
		t.Errorf("kind = %d, want local-only", m2.Kind)
	}
}

func TestOutboxCoalescingAndCompletion(t *testing.T) {
	_, a := openTemp(t)
	inbox := applyStandard(t, a)["1"]
	enq := func(op int, v, hard bool) {
		if _, err := a.Update(nil, func(tx *Tx) error { return tx.EnqueueOp(inbox.ID, "1:5", op, v, hard, 0) }); err != nil {
			t.Fatal(err)
		}
	}
	enq(OpRead, true, false)
	enq(OpRead, false, false) // overwrites, bumps version
	enq(OpFlagged, true, false)
	ops, _ := a.OutboxDue(0, time.Now(), 10)
	if len(ops) != 2 {
		t.Fatalf("ops = %+v", ops)
	}
	var read OutboxOp
	for _, o := range ops {
		if o.Op == OpRead {
			read = o
		}
	}
	if read.Value || read.Version != 2 {
		t.Errorf("read op = %+v (want value false, version 2)", read)
	}

	// A STORE that arrives while the old version is in flight must survive completion.
	enq(OpRead, true, false)
	if _, err := a.Update(nil, func(tx *Tx) error { return tx.CompleteOps([]OutboxOp{read}) }); err != nil {
		t.Fatal(err)
	}
	left, _ := a.OutboxFor(inbox.ID, "1:5")
	foundRead := false
	for _, o := range left {
		if o.Op == OpRead && o.Value && o.Version == 3 {
			foundRead = true
		}
	}
	if !foundRead {
		t.Errorf("newer read op was lost: %+v", left)
	}

	// Delete supersedes everything, and later ops for the item are ignored.
	enq(OpDelete, false, true)
	enq(OpRead, true, false)
	left, _ = a.OutboxFor(inbox.ID, "1:5")
	if len(left) != 1 || left[0].Op != OpDelete || !left[0].Hard {
		t.Errorf("after delete: %+v", left)
	}

	// Failure schedules a retry in the future.
	later := time.Now().Add(time.Hour)
	if _, err := a.Update(nil, func(tx *Tx) error { return tx.FailOps(left, later) }); err != nil {
		t.Fatal(err)
	}
	if due, _ := a.OutboxDue(0, time.Now(), 10); len(due) != 0 {
		t.Errorf("failed op is due immediately: %+v", due)
	}
	if due, _ := a.OutboxDue(0, later.Add(time.Second), 10); len(due) != 1 || due[0].Attempts != 1 {
		t.Errorf("failed op not retried later: %+v", due)
	}
	if n, _ := a.OutboxLen(); n != 1 {
		t.Errorf("OutboxLen = %d", n)
	}
	if fs, _ := a.OutboxFolders(); len(fs) != 1 || fs[0] != inbox.ID {
		t.Errorf("OutboxFolders = %v", fs)
	}
}

func TestPendingFetchOrderAndRetry(t *testing.T) {
	_, a := openTemp(t)
	inbox := applyStandard(t, a)["1"]
	mk := func(sid string, date int64) PendingFetch {
		return PendingFetch{FolderID: inbox.ID, ServerID: sid, Flags: FlagSeen, InternalDate: time.Unix(date, 0), EstSize: 5 << 20}
	}
	if _, err := a.Update(nil, func(tx *Tx) error {
		for _, p := range []PendingFetch{mk("old", 100), mk("new", 300), mk("mid", 200)} {
			if err := tx.UpsertPending(p); err != nil {
				return err
			}
		}
		// Upserting again keeps attempts and refreshes flags.
		return tx.UpsertPending(PendingFetch{FolderID: inbox.ID, ServerID: "mid", Flags: 0, InternalDate: time.Unix(200, 0)})
	}); err != nil {
		t.Fatal(err)
	}
	due, _ := a.PendingDue(time.Now(), 10)
	if len(due) != 3 || due[0].ServerID != "new" || due[1].ServerID != "mid" || due[2].ServerID != "old" {
		t.Fatalf("newest-first order broken: %+v", due)
	}
	if due[1].Flags != 0 {
		t.Errorf("upsert did not refresh flags: %+v", due[1])
	}
	later := time.Now().Add(time.Hour)
	a.Update(nil, func(tx *Tx) error { return tx.BumpPending(due[0].ID, later, false) })
	due, _ = a.PendingDue(time.Now(), 10)
	if len(due) != 2 {
		t.Errorf("backed-off item still due: %+v", due)
	}
	a.Update(nil, func(tx *Tx) error { return tx.DeletePending(inbox.ID, "mid") })
	if n, _ := a.PendingCount(0); n != 2 {
		t.Errorf("PendingCount = %d", n)
	}
}

func TestReconciliationPrimitives(t *testing.T) {
	_, a := openTemp(t)
	inbox := applyStandard(t, a)["1"]
	a.Update(nil, func(tx *Tx) error {
		for i := 1; i <= 4; i++ {
			if _, err := tx.AddMessage(inbox.ID, NewMessage{ServerID: fmt.Sprintf("1:%d", i), MIME: rawMsg(fmt.Sprintf("m%d@x", i), "s", "")}); err != nil {
				return err
			}
		}
		// A local draft is never subject to reconciliation.
		_, err := tx.AddMessage(inbox.ID, NewMessage{Kind: KindLocalDraft, MIME: rawMsg("draft@x", "d", "")})
		return err
	})
	var absent []Message
	_, err := a.Update(nil, func(tx *Tx) error {
		if err := tx.SetRecon(inbox.ID, true, 7, 0); err != nil {
			return err
		}
		if err := tx.MarkSeen(inbox.ID, 7, []string{"1:1", "1:2"}); err != nil {
			return err
		}
		var err error
		absent, err = tx.ReconAbsent(inbox.ID, 7)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(absent) != 2 || absent[0].ServerID != "1:3" || absent[1].ServerID != "1:4" {
		t.Fatalf("absent = %+v", absent)
	}
	a.Update(nil, func(tx *Tx) error {
		if err := tx.SetStrikes(inbox.ID, absent[0].UID, 1); err != nil {
			return err
		}
		return tx.ClearStrikes(inbox.ID, 7)
	})
	f, _ := a.FolderByID(inbox.ID)
	if !f.ReconActive || f.ReconEpoch != 7 {
		t.Errorf("recon state = %+v", f)
	}
	m3, _ := a.Message(inbox.ID, absent[0].UID)
	if m3.ReconStrikes != 1 {
		t.Errorf("strike lost: %+v", m3)
	}
	m1, _ := a.Message(inbox.ID, 1)
	if m1.ReconStrikes != 0 {
		t.Errorf("listed message keeps strikes: %+v", m1)
	}
}

func TestReadersDoNotBlockOnOpenWriteTransaction(t *testing.T) {
	_, a := openTemp(t)
	inbox := applyStandard(t, a)["1"]
	started, release := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		a.Update(nil, func(tx *Tx) error {
			tx.AddMessage(inbox.ID, NewMessage{ServerID: "1:1", MIME: rawMsg("w@x", "w", "")})
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	done := make(chan struct{})
	go func() {
		c, err := a.Counts(inbox.ID)
		if err != nil || c.Messages != 0 {
			t.Errorf("reader saw %+v %v (uncommitted data must be invisible)", c, err)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("reader blocked behind the write transaction")
	}
	close(release)
	wg.Wait()
	if c, _ := a.Counts(inbox.ID); c.Messages != 1 {
		t.Errorf("committed data missing: %+v", c)
	}
}

func TestDestroyAndWipeMarker(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := db.Account("x@y.test")
	a.Update(nil, func(tx *Tx) error { return tx.SetPolicyKey("k") })
	if WipeMarkerExists(dir) {
		t.Fatal("marker exists before a wipe")
	}
	if err := db.Destroy(); err != nil {
		t.Fatal(err)
	}
	if err := WriteWipeMarker(dir, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !WipeMarkerExists(dir) {
		t.Fatal("marker not written")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), DBFileName) {
			t.Errorf("%s survived Destroy", e.Name())
		}
	}
	// The operator removes the marker; a fresh database starts empty.
	os.Remove(WipeMarkerPath(dir))
	db2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	a2, _ := db2.Account("x@y.test")
	if k, _ := a2.PolicyKey(); k != "" {
		t.Errorf("state survived the wipe: policy key %q", k)
	}
}

func TestSchemaHasNoCredentialColumns(t *testing.T) {
	// DESIGN section 3: credentials are never persisted. Guard the schema
	// against a column that could hold one.
	for _, m := range migrations {
		low := strings.ToLower(m)
		for _, bad := range []string{"password", "passwd", "secret", "credential"} {
			if strings.Contains(low, bad) {
				t.Errorf("schema mentions %q", bad)
			}
		}
	}
}

// The production container has a read-only root file system, so SQLite must
// keep its temporary tables and sort files in memory on every connection.
func TestTempStoreIsInMemory(t *testing.T) {
	db, _ := openTemp(t)
	for name, pool := range map[string]interface {
		QueryRow(string, ...any) *sql.Row
	}{"writer": db.w, "reader": db.r} {
		var v int
		if err := pool.QueryRow(`PRAGMA temp_store`).Scan(&v); err != nil || v != 2 {
			t.Errorf("%s pool: temp_store = %d, %v (want 2 = MEMORY)", name, v, err)
		}
	}
}

// A login that was never verified must leave nothing in the database: PolicyKeyOf reads without creating the row.
func TestPolicyKeyOfReadsWithoutCreatingARow(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if k, err := db.PolicyKeyOf("Nobody@Example.test"); err != nil || k != "" {
		t.Fatalf("%q %v", k, err)
	}
	if users, _ := db.AccountUsers(); len(users) != 0 {
		t.Fatalf("a read made rows: %v", users)
	}
	acct, _ := db.Account("someone@example.test")
	acct.Update(nil, func(tx *Tx) error { return tx.SetPolicyKey("PK1") })
	if k, _ := db.PolicyKeyOf("  Someone@Example.test "); k != "PK1" {
		t.Errorf("policy key %q", k)
	}
}
