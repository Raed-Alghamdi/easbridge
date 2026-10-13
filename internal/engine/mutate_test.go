//go:build devfake

package engine_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"easbridge/internal/engine"
	"easbridge/internal/fakeupstream"
	"easbridge/internal/store"
)

func serverItem(e *env, folder, id string) (fakeupstream.ItemInfo, bool) {
	for _, it := range e.srv.Items(testUser, e.srv.FolderID(testUser, folder)) {
		if it.ID == id {
			return it, true
		}
	}
	return fakeupstream.ItemInfo{}, false
}

// seeded builds an env with n Inbox messages already mirrored.
func seeded(t *testing.T, n int) (*env, *engine.Account, *store.Folder, []string) {
	e := newEnv(t)
	inbox := e.srv.FolderID(testUser, "Inbox")
	var ids []string
	for i := 0; i < n; i++ {
		ids = append(ids, e.srv.AddMessage(testUser, inbox,
			mailBytes(fmt.Sprintf("s%d@x", i), fmt.Sprintf("subject %d", i), e.clock.Now().Add(time.Duration(i)*time.Minute), ""),
			fakeupstream.AddOptions{Date: e.clock.Now().Add(time.Duration(i) * time.Minute)}))
	}
	a := e.login()
	f := e.mirrored(a, "Inbox", "INBOX")
	return e, a, f, ids
}

func TestStoreFlagsSyncSeenAndFlaggedUpstreamAndKeepTheRestLocal(t *testing.T) {
	e, a, f, ids := seeded(t, 2)
	rec, _ := e.watch(a, f)

	ups, err := a.SetFlags(f.ID, []uint32{1}, engine.FlagAdd, store.FlagSeen|store.FlagFlagged|store.FlagDeleted|store.FlagAnswered, []string{"$Forwarded"}, "sess-A")
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 1 || ups[0].Flags != store.FlagSeen|store.FlagFlagged|store.FlagDeleted|store.FlagAnswered || ups[0].Keywords != "$Forwarded" {
		t.Fatalf("updates = %+v", ups)
	}
	// Local immediately; upstream shortly.
	m, _ := a.Store().Message(f.ID, 1)
	if !m.Flags.Has(store.FlagSeen) || !m.Flags.Has(store.FlagDeleted) {
		t.Fatalf("local flags = %v", m.Flags)
	}
	eventually(t, "\\Seen and \\Flagged reach Exchange", func() bool {
		it, ok := serverItem(e, "Inbox", ids[0])
		return ok && it.Read && it.Flagged
	})
	// \Deleted, \Answered and keywords are never sent: the other message is untouched.
	if it, _ := serverItem(e, "Inbox", ids[1]); it.Read || it.Flagged {
		t.Errorf("unrelated message changed upstream: %+v", it)
	}
	// The event carries the originating session, so IMAP can skip echoing it.
	evs := rec.all()
	if len(evs) != 1 || evs[0].Kind != store.EvFlags || evs[0].Origin != "sess-A" {
		t.Fatalf("events = %+v", evs)
	}
	// Removing \Seen later flows upstream too.
	if _, err := a.SetFlags(f.ID, []uint32{1}, engine.FlagRemove, store.FlagSeen, nil, "sess-A"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "\\Seen removal reaches Exchange", func() bool {
		it, _ := serverItem(e, "Inbox", ids[0])
		return !it.Read && it.Flagged
	})
	// FLAGS replace.
	if ups, _ := a.SetFlags(f.ID, []uint32{2}, engine.FlagReplace, store.FlagFlagged, []string{"b", "a"}, nil); ups[0].Keywords != "a b" {
		t.Errorf("keywords not normalised: %q", ups[0].Keywords)
	}
	// Unknown UIDs are skipped, not an error.
	if ups, err := a.SetFlags(f.ID, []uint32{99}, engine.FlagAdd, store.FlagSeen, nil, nil); err != nil || len(ups) != 0 {
		t.Errorf("unknown uid: %v %v", ups, err)
	}
}

func TestRejectedFlagChangeIsRolledBackWithAFlagsEvent(t *testing.T) {
	e, a, f, _ := seeded(t, 1)
	rec, _ := e.watch(a, f)
	e.srv.RejectChangesStatus = 6 // permanent: error in client/server conversion
	if _, err := a.SetFlags(f.ID, []uint32{1}, engine.FlagAdd, store.FlagSeen, nil, "s"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "rollback", func() bool {
		m, _ := a.Store().Message(f.ID, 1)
		return !m.Flags.Has(store.FlagSeen)
	})
	if rec.count(store.EvFlags) != 2 {
		t.Fatalf("want the optimistic change and the rollback, got %+v", rec.all())
	}
	if n, _ := a.Store().OutboxLen(); n != 0 {
		t.Fatalf("rejected change still queued: %d", n)
	}
}

func TestExpungeSoftDeletesEverywhereButHardDeletesInTrash(t *testing.T) {
	e, a, f, ids := seeded(t, 3)
	trash := e.srv.FolderID(testUser, "Deleted Items")

	// Inbox: \Deleted + EXPUNGE of one UID only.
	if _, err := a.SetFlags(f.ID, []uint32{1, 3}, engine.FlagAdd, store.FlagDeleted, nil, "s"); err != nil {
		t.Fatal(err)
	}
	gone, err := a.Expunge(f.ID, func(u uint32) bool { return u == 1 }, "s") // UID EXPUNGE 1
	if err != nil || len(gone) != 1 || gone[0] != 1 {
		t.Fatalf("expunged %v %v", gone, err)
	}
	eventually(t, "the item moved to Deleted Items (DeletesAsMoves=1)", func() bool {
		_, still := serverItem(e, "Inbox", ids[0])
		return !still && len(e.srv.Items(testUser, trash)) == 1
	})
	if c, _ := a.Store().Counts(f.ID); c.Messages != 2 {
		t.Fatalf("counts = %+v", c)
	}
	// Plain EXPUNGE takes the rest of the \Deleted ones.
	if gone, _ := a.Expunge(f.ID, nil, "s"); len(gone) != 1 || gone[0] != 3 {
		t.Fatalf("second expunge %v", gone)
	}

	// The copy in Deleted Items syncs in; expunging there is permanent.
	tf := e.mirrored(a, "Deleted Items", "Deleted Items")
	eventually(t, "trash mirrored", func() bool { return serverCount(a, tf.ID) == 2 })
	metas, _ := a.Store().ListMessages(tf.ID)
	var uids []uint32
	for _, m := range metas {
		uids = append(uids, m.UID)
	}
	if _, err := a.SetFlags(tf.ID, uids, engine.FlagAdd, store.FlagDeleted, nil, "s"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Expunge(tf.ID, nil, "s"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "permanent delete in Deleted Items", func() bool { return len(e.srv.Items(testUser, trash)) == 0 })
}

func TestMoveReportsNewUIDsKeepsFlagsAndFlushesPendingFlagsFirst(t *testing.T) {
	e, a, f, ids := seeded(t, 3)
	junk := e.folder(a, "Junk Email")
	rec, _ := e.watch(a, f)

	// Flag change immediately followed by a MOVE: the server must see the flag
	// before the item gets its new id.
	if _, err := a.SetFlags(f.ID, []uint32{2}, engine.FlagAdd, store.FlagSeen|store.FlagAnswered, nil, "s"); err != nil {
		t.Fatal(err)
	}
	out, err := a.Move(context.Background(), f.ID, []uint32{2, 3}, junk.ID, "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Moved) != 2 || out.Moved[0].Src != 2 || out.Moved[0].Dst != 1 || out.Moved[1].Src != 3 || out.Moved[1].Dst != 2 || len(out.Failed) != 0 {
		t.Fatalf("outcome = %+v", out)
	}
	if out.DstUIDValidity != junk.UIDValidity {
		t.Errorf("UIDVALIDITY = %d, want %d", out.DstUIDValidity, junk.UIDValidity)
	}
	// Source: expunged for the originating session (events) and gone from the DB.
	if got := rec.uids(store.EvExpunge); len(got) != 2 {
		t.Fatalf("expunge events %v", got)
	}
	if c, _ := a.Store().Counts(f.ID); c.Messages != 1 {
		t.Fatalf("source counts %+v", c)
	}
	// Destination: new UIDs, flags carried over, MIME copied, Exchange agrees.
	m, err := a.Store().Message(junk.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Flags.Has(store.FlagSeen) || !m.Flags.Has(store.FlagAnswered) || m.MessageID != "s1@x" || m.ServerID == "" {
		t.Fatalf("moved message = %+v", m)
	}
	if mime, _ := a.Store().MIME(junk.ID, 1); !bytes.Contains(mime, []byte("body of s1@x")) {
		t.Errorf("MIME not copied")
	}
	jid := e.srv.FolderID(testUser, "Junk Email")
	items := e.srv.Items(testUser, jid)
	if len(items) != 2 {
		t.Fatalf("server junk items: %d", len(items))
	}
	var movedSeen bool
	for _, it := range items {
		if strings.Contains(string(it.MIME), "s1@x") {
			movedSeen = it.Read
		}
	}
	if !movedSeen {
		t.Errorf("the \\Seen flag did not reach Exchange before the move")
	}
	if _, still := serverItem(e, "Inbox", ids[1]); still {
		t.Errorf("source item still on the server")
	}
	// The Sync of the destination later reports the same ServerIds: no duplicates.
	eventually(t, "junk synced", func() bool { return serverCount(a, junk.ID) == 2 })
	consistently(t, "no duplicate after the follow-up Sync", 150*time.Millisecond, func() bool { return serverCount(a, junk.ID) == 2 })
	if metas, _ := a.Store().ListMessages(junk.ID); len(metas) != 2 {
		t.Fatalf("junk has %d messages", len(metas))
	}
}

func TestMoveRefusesLocalOnlyMessagesAndSameFolder(t *testing.T) {
	e, a, f, _ := seeded(t, 1)
	drafts := e.folder(a, "Drafts")
	if _, err := a.Append("Drafts", mailBytes("d1@x", "draft", e.clock.Now(), ""), store.FlagSeen, "", time.Time{}, "s"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Move(context.Background(), drafts.ID, []uint32{1}, f.ID, "s"); !errors.Is(err, engine.ErrCannot) {
		t.Fatalf("moving a local draft: %v", err)
	}
	if _, err := a.Move(context.Background(), f.ID, []uint32{1}, f.ID, "s"); !errors.Is(err, engine.ErrCannot) {
		t.Fatalf("moving into the same folder: %v", err)
	}
	// Already-expunged UIDs are reported, not fatal.
	out, err := a.Move(context.Background(), f.ID, []uint32{77}, e.folder(a, "Junk Email").ID, "s")
	if err != nil || len(out.Failed) != 1 {
		t.Fatalf("outcome %+v err %v", out, err)
	}
}

func TestAppendDraftsAreLocalAndOtherFoldersAreRefused(t *testing.T) {
	e, a, _, _ := seeded(t, 0)
	drafts := e.folder(a, "Drafts")
	rec, _ := e.watch(a, drafts)
	d1 := mailBytes("draft1@x", "my draft", e.clock.Now(), "")
	res, err := a.Append("Drafts", d1, store.FlagSeen|store.FlagDraft, "", time.Time{}, "s")
	if err != nil || res.UID != 1 || res.UIDValidity != drafts.UIDValidity {
		t.Fatalf("append: %+v %v", res, err)
	}
	// A newer revision gets a new UID; deleting the old one is purely local.
	res2, _ := a.Append("Drafts", mailBytes("draft1@x", "my draft v2", e.clock.Now(), ""), store.FlagSeen, "", time.Time{}, "s")
	if res2.UID != 2 {
		t.Fatalf("second draft uid %d", res2.UID)
	}
	if uids, _ := a.Store().UIDsByMessageID(drafts.ID, "draft1@x"); len(uids) != 2 {
		t.Fatalf("message-id search: %v", uids)
	}
	if _, err := a.SetFlags(drafts.ID, []uint32{1}, engine.FlagAdd, store.FlagDeleted, nil, "s"); err != nil {
		t.Fatal(err)
	}
	if gone, _ := a.Expunge(drafts.ID, nil, "s"); len(gone) != 1 || gone[0] != 1 {
		t.Fatalf("expunge %v", gone)
	}
	if n, _ := a.Store().OutboxLen(); n != 0 {
		t.Fatalf("a local draft produced an upstream delete (%d queued)", n)
	}
	if rec.count(store.EvAdd) != 2 || rec.count(store.EvExpunge) != 1 {
		t.Fatalf("events: %+v", rec.all())
	}
	if len(e.srv.Items(testUser, e.srv.FolderID(testUser, "Drafts"))) != 0 {
		t.Fatalf("a local draft leaked to Exchange")
	}
	// \Draft is always set.
	if m, _ := a.Store().Message(drafts.ID, 2); !m.Flags.Has(store.FlagDraft) || m.Kind != store.KindLocalDraft {
		t.Errorf("draft flags/kind: %+v", m)
	}

	if _, err := a.Append("INBOX", d1, 0, "", time.Time{}, "s"); !errors.Is(err, engine.ErrCannot) {
		t.Errorf("APPEND to INBOX: %v", err)
	}
	if _, err := a.Append("Nope", d1, 0, "", time.Time{}, "s"); !errors.Is(err, engine.ErrNoMailbox) {
		t.Errorf("APPEND to a missing folder: %v", err)
	}
	if _, err := a.Append("Sent Items", bytes.Repeat([]byte("x"), 2<<20), 0, "", time.Time{}, "s"); !errors.Is(err, engine.ErrTooLarge) {
		t.Errorf("oversized APPEND: %v", err)
	}
}

// ---- Sent-copy de-duplication ----------------------------------------------

func sentMessage(id, subject, to string, date time.Time) []byte {
	return []byte("Message-ID: <" + id + ">\r\nFrom: alice@example.test\r\nTo: " + to + "\r\nSubject: " + subject +
		"\r\nDate: " + date.Format("Mon, 02 Jan 2006 15:04:05 -0700") + "\r\nMIME-Version: 1.0\r\nContent-Type: text/plain\r\n\r\nhello\r\n")
}

func TestSentAppendBeforeServerCopyBindsToTheSameUID(t *testing.T) {
	e, a, _, _ := seeded(t, 0)
	sent := e.folder(a, "Sent Items")
	rec, _ := e.watch(a, sent)
	e.srv.HoldSentCopies(testUser, true)

	mime := sentMessage("out1@mailflow.test", "Quarterly numbers", "bob@example.test", e.clock.Now())
	// 1. SMTP submission.
	if err := a.SendMail(context.Background(), mime); err != nil {
		t.Fatal(err)
	}
	// 2. MailFlow appends its copy: a placeholder with a fresh UID.
	res, err := a.Append("Sent Items", mime, store.FlagSeen, "", time.Time{}, "s")
	if err != nil || res.UID != 1 || res.Duplicate {
		t.Fatalf("append: %+v %v", res, err)
	}
	if rec.count(store.EvAdd) != 1 {
		t.Fatalf("events %+v", rec.all())
	}
	// 3. Exchange's own copy arrives via Sync.
	e.srv.ReleaseSentCopies(testUser)
	eventually(t, "server copy bound", func() bool {
		m, err := a.Store().Message(sent.ID, 1)
		return err == nil && m.ServerID != "" && m.Kind == store.KindServer
	})
	time.Sleep(100 * time.Millisecond)
	// No second message, no new EXISTS, same UID.
	if c, _ := a.Store().Counts(sent.ID); c.Messages != 1 || c.UIDNext != 2 {
		t.Fatalf("counts = %+v (the Sent copy was duplicated)", c)
	}
	if rec.count(store.EvAdd) != 1 {
		t.Fatalf("a second EXISTS was announced: %+v", rec.all())
	}
	// A repeated APPEND of the same message returns the same UID.
	res, _ = a.Append("Sent Items", mime, store.FlagSeen, "", time.Time{}, "s")
	if res.UID != 1 || !res.Duplicate {
		t.Fatalf("repeated append: %+v", res)
	}
}

func TestSentServerCopyBeforeAppendIsReused(t *testing.T) {
	e, a, _, _ := seeded(t, 0)
	sent := e.folder(a, "Sent Items")
	mime := sentMessage("out2@mailflow.test", "Re: lunch", "bob@example.test", e.clock.Now())
	if err := a.SendMail(context.Background(), mime); err != nil {
		t.Fatal(err)
	}
	eventually(t, "server copy synced", func() bool { return serverCount(a, sent.ID) == 1 })
	res, err := a.Append("Sent Items", mime, store.FlagSeen, "", time.Time{}, "s")
	if err != nil || res.UID != 1 || !res.Duplicate {
		t.Fatalf("append after the server copy: %+v %v", res, err)
	}
	if c, _ := a.Store().Counts(sent.ID); c.Messages != 1 {
		t.Fatalf("counts = %+v", c)
	}
}

func TestSentCopyWithAnotherMessageIDIsNeverMergedByLookAlike(t *testing.T) {
	// The subject + date + recipients fallback used to merge a placeholder with a
	// server copy even when both had a Message-ID and the IDs differed. Two
	// different messages that look alike (two "Thanks" to the same person within
	// minutes) were then merged into one and one of them was lost. Now two
	// different Message-IDs mean two different messages. (If Exchange rewrote the
	// Message-ID of its copy, the folder shows both: a duplicate, not a loss.)
	e, a, _, _ := seeded(t, 0)
	e.srv.RewriteMessageID = true
	e.srv.HoldSentCopies(testUser, true)
	sent := e.folder(a, "Sent Items")
	now := e.clock.Now()

	one := sentMessage("one@mailflow.test", "Thanks", "bob@example.test", now)
	two := sentMessage("two@mailflow.test", "Thanks", "bob@example.test", now.Add(time.Minute))
	for _, m := range [][]byte{one, two} {
		if err := a.SendMail(context.Background(), m); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Append("Sent Items", m, store.FlagSeen, "", time.Time{}, "s"); err != nil {
			t.Fatal(err)
		}
	}
	e.srv.ReleaseSentCopies(testUser)
	eventually(t, "Exchange's copies arrive as messages of their own", func() bool { return serverCount(a, sent.ID) == 2 })
	time.Sleep(100 * time.Millisecond)
	if c, _ := a.Store().Counts(sent.ID); c.Messages != 4 {
		t.Fatalf("counts = %+v, want the two placeholders and the two server copies", c)
	}
	for uid := uint32(1); uid <= 2; uid++ {
		if m, _ := a.Store().Message(sent.ID, uid); m.Kind != store.KindPlaceholder {
			t.Errorf("UID %d was bound to a look-alike: %+v", uid, m)
		}
	}
}

func TestSentFallbackStillBindsAnExchangeCopyThatHasNoMessageID(t *testing.T) {
	// The fallback is for the case where one side has no Message-ID to compare.
	e, a, _, _ := seeded(t, 0)
	sent := e.folder(a, "Sent Items")
	now := e.clock.Now()
	ph := sentMessage("p@mailflow.test", "Report", "bob@example.test", now)
	if _, err := a.Append("Sent Items", ph, store.FlagSeen, "", time.Time{}, "s"); err != nil {
		t.Fatal(err)
	}
	copyWithoutID := bytes.Replace(sentMessage("x", "Report", "bob@example.test", now), []byte("Message-ID: <x>\r\n"), nil, 1)
	sf := e.srv.FolderID(testUser, "Sent Items")
	e.srv.AddMessage(testUser, sf, copyWithoutID, fakeupstream.AddOptions{Read: true})
	eventually(t, "the placeholder is bound", func() bool {
		m, err := a.Store().Message(sent.ID, 1)
		return err == nil && m.Kind == store.KindServer
	})
	time.Sleep(100 * time.Millisecond)
	if c, _ := a.Store().Counts(sent.ID); c.Messages != 1 {
		t.Fatalf("counts = %+v", c)
	}
}

func TestSentFallbackDoesNotMatchDifferentRecipientsOrFarDates(t *testing.T) {
	e, a, _, _ := seeded(t, 0)
	sent := e.folder(a, "Sent Items")
	now := e.clock.Now()
	// Placeholder from the client: to bob, 1 hour ago.
	ph := sentMessage("p@mailflow.test", "Report", "bob@example.test", now.Add(-time.Hour))
	if _, err := a.Append("Sent Items", ph, store.FlagSeen, "", time.Time{}, "s"); err != nil {
		t.Fatal(err)
	}
	// Server items with the same subject but another recipient, and with the
	// right recipient but 1 hour away: neither may be taken for the placeholder.
	sf := e.srv.FolderID(testUser, "Sent Items")
	e.srv.AddMessage(testUser, sf, sentMessage("srv1@x", "Report", "eve@example.test", now.Add(-time.Hour)), fakeupstream.AddOptions{Read: true})
	e.srv.AddMessage(testUser, sf, sentMessage("srv2@x", "Report", "bob@example.test", now), fakeupstream.AddOptions{Read: true})
	eventually(t, "two more messages", func() bool { return serverCount(a, sent.ID) == 2 })
	if c, _ := a.Store().Counts(sent.ID); c.Messages != 3 {
		t.Fatalf("counts = %+v", c)
	}
	if m, _ := a.Store().Message(sent.ID, 1); m.Kind != store.KindPlaceholder {
		t.Fatalf("the placeholder was bound wrongly: %+v", m)
	}
}

func TestUnmatchedSentPlaceholderBecomesLocalOnlyAfter24Hours(t *testing.T) {
	e, a, _, _ := seeded(t, 0)
	sent := e.folder(a, "Sent Items")
	if _, err := a.Append("Sent Items", sentMessage("lonely@x", "x", "bob@example.test", e.clock.Now()), store.FlagSeen, "", time.Time{}, "s"); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(23 * time.Hour)
	time.Sleep(120 * time.Millisecond)
	if m, _ := a.Store().Message(sent.ID, 1); m.Kind != store.KindPlaceholder {
		t.Fatalf("expired too early: kind %d", m.Kind)
	}
	e.clock.Advance(2 * time.Hour)
	eventually(t, "placeholder becomes local-only", func() bool {
		m, _ := a.Store().Message(sent.ID, 1)
		return m != nil && m.Kind == store.KindLocalOnly
	})
	if c, _ := a.Store().Counts(sent.ID); c.Messages != 1 {
		t.Fatalf("the message disappeared: %+v", c)
	}
}

func TestSendMailPassesTheMessageUnchangedAndErrorsThrough(t *testing.T) {
	e, a, _, _ := seeded(t, 0)
	mime := sentMessage("m@x", "hi", "bob@example.test", e.clock.Now())
	if err := a.SendMail(context.Background(), mime); err != nil {
		t.Fatal(err)
	}
	sent := e.srv.Sent(testUser)
	if len(sent) != 1 || !bytes.Equal(sent[0].MIME, mime) || !sent[0].SaveInSent || sent[0].Recipients[0] != "bob@example.test" {
		t.Fatalf("sent = %+v", sent)
	}
	if err := a.SendMail(context.Background(), bytes.Repeat([]byte("x"), 2<<20)); !errors.Is(err, engine.ErrTooLarge) {
		t.Errorf("oversized: %v", err)
	}
	// No recipients: Exchange's refusal comes back for the SMTP layer to map.
	if err := a.SendMail(context.Background(), []byte("Subject: x\r\n\r\nbody")); err == nil {
		t.Errorf("a message without recipients was accepted")
	}
}

// ---- Folder operations --------------------------------------------------------

func TestCreateRenameDeleteFolders(t *testing.T) {
	e, a, _, _ := seeded(t, 0)
	ctx := context.Background()
	if err := a.CreateFolder(ctx, "Snoozed"); err != nil {
		t.Fatal(err)
	}
	if err := a.CreateFolder(ctx, "Snoozed"); !errors.Is(err, engine.ErrExists) {
		t.Fatalf("second CREATE: %v", err)
	}
	if err := a.CreateFolder(ctx, "INBOX"); !errors.Is(err, engine.ErrExists) {
		t.Fatalf("CREATE INBOX: %v", err)
	}
	f, err := a.Store().FolderByPath("Snoozed")
	if err != nil || f.EASType != store.TypeUserMail || f.UIDNext != 1 {
		t.Fatalf("folder = %+v %v", f, err)
	}
	if e.srv.FolderID(testUser, "Snoozed") != f.ServerID {
		t.Fatalf("Exchange has a different id")
	}
	// Parents are created on demand.
	if err := a.CreateFolder(ctx, "Clients/ACME/2026"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store().FolderByPath("Clients/ACME/2026"); err != nil {
		t.Fatal(err)
	}
	// The new folder is synced like any other: mail dropped into it arrives.
	e.srv.AddMessage(testUser, f.ServerID, mailBytes("sn@x", "snoozed mail", e.clock.Now(), ""), fakeupstream.AddOptions{})
	eventually(t, "mail in the new folder", func() bool { return serverCount(a, f.ID) == 1 })

	if err := a.RenameFolder(ctx, "Snoozed", "Later"); err != nil {
		t.Fatal(err)
	}
	g, err := a.Store().FolderByPath("Later")
	if err != nil || g.ID != f.ID || g.UIDValidity != f.UIDValidity {
		t.Fatalf("rename lost identity: %+v %v", g, err)
	}
	if err := a.RenameFolder(ctx, "Clients/ACME", "Clients/ACME2"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store().FolderByPath("Clients/ACME2/2026"); err != nil {
		t.Fatalf("children did not follow the rename: %v", err)
	}
	// System folders are protected.
	for _, name := range []string{"INBOX", "Drafts", "Sent Items", "Deleted Items", "Junk Email"} {
		if err := a.RenameFolder(ctx, name, "X"); !errors.Is(err, engine.ErrCannot) {
			t.Errorf("rename %s: %v", name, err)
		}
		if err := a.DeleteFolder(ctx, name); !errors.Is(err, engine.ErrCannot) {
			t.Errorf("delete %s: %v", name, err)
		}
	}
	if err := a.DeleteFolder(ctx, "Clients"); !errors.Is(err, engine.ErrCannot) {
		t.Errorf("delete of a folder with children: %v", err)
	}
	if err := a.DeleteFolder(ctx, "Nope"); !errors.Is(err, engine.ErrNoMailbox) {
		t.Errorf("delete of a missing folder: %v", err)
	}
	if err := a.DeleteFolder(ctx, "Later"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store().FolderByPath("Later"); err == nil {
		t.Fatal("folder survived DELETE")
	}
	if e.srv.FolderID(testUser, "Snoozed") != "" {
		t.Fatal("Exchange still has the folder")
	}
	// The hierarchy key is persisted: after a restart-like FolderSync nothing is lost.
	if k, _ := a.Store().FolderSyncKey(); k == "0" || k == "" {
		t.Fatalf("hierarchy key not stored: %q", k)
	}
}

func TestHierarchyKeyResetIsRecoveredByARefreshAndRetry(t *testing.T) {
	// FolderSync status 9 / invalid hierarchy key on CREATE: the engine refreshes
	// the hierarchy and retries once.
	e, a, _, _ := seeded(t, 0)
	e.srv.ResetHierarchy(testUser)
	if err := a.CreateFolder(context.Background(), "AfterReset"); err != nil {
		t.Fatalf("create after a hierarchy reset: %v", err)
	}
	if _, err := a.Store().FolderByPath("AfterReset"); err != nil {
		t.Fatal(err)
	}
}
