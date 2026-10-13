//go:build devfake

package engine_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"easbridge/internal/engine"
	"easbridge/internal/fakeupstream"
	"easbridge/internal/store"
	"easbridge/internal/upstream"
)

// The Sent copy shows the Bcc recipients (v0.1.8, real-Exchange finding 4).
//
// Exchange's Sent Items copy of a message that went out through SMTP has no Bcc
// header, so the bridge keeps the bytes it submitted (with the Bcc header it wrote
// for the envelope-only recipients) and serves them. The fake Exchange strips the
// Bcc header from the copy it saves, as the real one does.

const hiddenRcpt = "hidden-bcc@example.test"

// submittedMIME is a message as the SMTP front end hands it to SendMail: the
// recipient that exists only in the SMTP envelope is in a Bcc header.
func submittedMIME(id, subject string, date time.Time) []byte {
	return []byte("Message-ID: <" + id + ">\r\nFrom: alice@example.test\r\nTo: bob@example.test\r\nSubject: " + subject +
		"\r\nDate: " + date.Format("Mon, 02 Jan 2006 15:04:05 -0700") + "\r\nBcc: " + hiddenRcpt +
		"\r\nMIME-Version: 1.0\r\nContent-Type: text/plain\r\n\r\nhello\r\n")
}

// clientCopyOf is the same message as MailFlow APPENDs it: it builds it without
// the Bcc header.
func clientCopyOf(submitted []byte) []byte {
	return bytes.Replace(submitted, []byte("Bcc: "+hiddenRcpt+"\r\n"), nil, 1)
}

func storedMIME(t *testing.T, a *engine.Account, folderID int64, uid uint32) []byte {
	t.Helper()
	b, err := a.Store().MIME(folderID, uid)
	if err != nil {
		t.Fatalf("MIME of UID %d: %v", uid, err)
	}
	return b
}

func keptCopies(t *testing.T, a *engine.Account) int {
	t.Helper()
	n, _, err := a.Store().SentCopies()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// serves checks that a message serves exactly these bytes: the MIME, the header
// block, the size and the envelope all describe them.
func serves(t *testing.T, a *engine.Account, folderID int64, uid uint32, want []byte, what string) {
	t.Helper()
	if got := storedMIME(t, a, folderID, uid); !bytes.Equal(got, want) {
		t.Errorf("%s: UID %d serves\n%q\nwant\n%q", what, uid, got, want)
	}
	m, err := a.Store().Message(folderID, uid)
	if err != nil {
		t.Fatal(err)
	}
	if m.Size != int64(len(want)) {
		t.Errorf("%s: UID %d has size %d, want %d", what, uid, m.Size, len(want))
	}
	if hasBcc := bytes.Contains(want, []byte("\r\nBcc: ")); hasBcc != bytes.Contains(m.Header, []byte("\r\nBcc: ")) {
		t.Errorf("%s: the stored header block does not match the bytes: %q", what, m.Header)
	}
	blob, err := a.Store().Structure(folderID, uid)
	if err != nil {
		t.Fatal(err)
	}
	if hasBcc := bytes.Contains(want, []byte("\r\nBcc: ")); hasBcc != strings.Contains(string(blob), `"b":[`) {
		t.Errorf("%s: the stored envelope does not match the bytes: %s", what, blob)
	}
}

func TestTheSentCopyShowsTheBccRecipientsWhenTheClientAppendsFirst(t *testing.T) {
	e, a, _, _ := seeded(t, 0)
	sent := e.mirrored(a, "Sent Items", "Sent Items") // open: its events are counted below
	rec, _ := e.watch(a, sent)
	e.srv.HoldSentCopies(testUser, true)

	submitted := submittedMIME("bcc-1@mailflow.test", "Budget", e.clock.Now())
	// 1. SMTP submission: the bytes are kept.
	if err := a.SendMail(context.Background(), submitted); err != nil {
		t.Fatal(err)
	}
	if n := keptCopies(t, a); n != 1 {
		t.Fatalf("%d submitted messages kept, want 1", n)
	}
	// 2. MailFlow APPENDs its own copy, which has no Bcc header. The placeholder
	// serves the submitted bytes from the start: MailFlow looks at the new UID at
	// once and does not look again.
	res, err := a.Append("Sent Items", clientCopyOf(submitted), store.FlagSeen, "", time.Time{}, "s")
	if err != nil || res.UID != 1 || res.Duplicate {
		t.Fatalf("APPEND: %+v %v", res, err)
	}
	serves(t, a, sent.ID, 1, submitted, "the placeholder")
	if m, _ := a.Store().Message(sent.ID, 1); m.Kind != store.KindPlaceholder || m.ServerID != "" {
		t.Fatalf("the APPENDed message is not a placeholder: %+v", m)
	}

	// 3. Exchange's copy arrives without Bcc and binds to the placeholder.
	e.srv.ReleaseSentCopies(testUser)
	eventually(t, "Exchange's copy is bound", func() bool {
		m, err := a.Store().Message(sent.ID, 1)
		return err == nil && m.Kind == store.KindServer && m.ServerID != ""
	})
	sf := e.srv.FolderID(testUser, "Sent Items")
	items := e.srv.Items(testUser, sf)
	if len(items) != 1 || bytes.Contains(items[0].MIME, []byte(hiddenRcpt)) {
		t.Fatalf("setup: the fake's Sent copy should exist and have no Bcc: %d items", len(items))
	}
	time.Sleep(100 * time.Millisecond)

	// Still the submitted bytes, same UID, bound to the real item, nothing new.
	serves(t, a, sent.ID, 1, submitted, "after binding")
	if m, _ := a.Store().Message(sent.ID, 1); m.ServerID != items[0].ID {
		t.Errorf("bound to %q, want Exchange's item %q (flags, moves and deletes must act on it)", m.ServerID, items[0].ID)
	}
	if c, _ := a.Store().Counts(sent.ID); c.Messages != 1 || c.UIDNext != 2 {
		t.Errorf("counts %+v: the Sent copy was duplicated or a UID was used", c)
	}
	if rec.count(store.EvAdd) != 1 || rec.count(store.EvResize) != 0 || rec.count(store.EvExpunge) != 0 || rec.count(store.EvFlags) != 0 {
		t.Errorf("events %+v: want the placeholder's EXISTS and nothing else", rec.all())
	}
	if n := keptCopies(t, a); n != 0 {
		t.Errorf("%d submitted messages still kept after the copy was bound", n)
	}

	// The binding is to Exchange's real item: flags and deletes act on it.
	if _, err := a.SetFlags(sent.ID, []uint32{1}, engine.FlagAdd, store.FlagFlagged, nil, "s"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the flag reaches Exchange's item", func() bool {
		it, ok := serverItem(e, "Sent Items", items[0].ID)
		return ok && it.Flagged
	})
	if _, err := a.SetFlags(sent.ID, []uint32{1}, engine.FlagAdd, store.FlagDeleted, nil, "s"); err != nil {
		t.Fatal(err)
	}
	if gone, err := a.Expunge(sent.ID, nil, "s"); err != nil || len(gone) != 1 {
		t.Fatalf("expunge: %v %v", gone, err)
	}
	eventually(t, "Exchange moves its item to Deleted Items", func() bool {
		_, still := serverItem(e, "Sent Items", items[0].ID)
		return !still && len(e.srv.Items(testUser, e.srv.FolderID(testUser, "Deleted Items"))) == 1
	})
}

func TestTheSentCopyShowsTheBccRecipientsWhenExchangesCopyComesFirst(t *testing.T) {
	// The copy is in Sent Items before the client APPENDs: it is stored with the
	// submitted bytes, and the APPEND finds it.
	e, a, _, _ := seeded(t, 0)
	sent := e.mirrored(a, "Sent Items", "Sent Items") // open: its events are counted below
	rec, _ := e.watch(a, sent)

	submitted := submittedMIME("bcc-2@mailflow.test", "Re: lunch", e.clock.Now())
	if err := a.SendMail(context.Background(), submitted); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Exchange's copy is synced", func() bool { return serverCount(a, sent.ID) == 1 })
	serves(t, a, sent.ID, 1, submitted, "Exchange's copy")
	sf := e.srv.FolderID(testUser, "Sent Items")
	if items := e.srv.Items(testUser, sf); len(items) != 1 || bytes.Contains(items[0].MIME, []byte(hiddenRcpt)) {
		t.Fatalf("setup: the fake's Sent copy should have no Bcc")
	}

	res, err := a.Append("Sent Items", clientCopyOf(submitted), store.FlagSeen, "", time.Time{}, "s")
	if err != nil || res.UID != 1 || !res.Duplicate {
		t.Fatalf("APPEND after Exchange's copy: %+v %v", res, err)
	}
	serves(t, a, sent.ID, 1, submitted, "after the APPEND")
	if c, _ := a.Store().Counts(sent.ID); c.Messages != 1 || c.UIDNext != 2 {
		t.Errorf("counts %+v", c)
	}
	if rec.count(store.EvAdd) != 1 || rec.count(store.EvResize) != 0 {
		t.Errorf("events %+v: want one EXISTS", rec.all())
	}
	if n := keptCopies(t, a); n != 0 {
		t.Errorf("%d submitted messages still kept", n)
	}
}

func TestExchangesCopyPickedUpWhileTheSubmissionIsStillInFlightFindsTheBytes(t *testing.T) {
	// Sending takes seconds through a gateway, and Exchange saves its copy as part of
	// it: Ping can have the Sent folder synced before SendMail has returned. The bytes
	// are registered before the request leaves, so that copy already finds them. (A
	// registration after the reply would store Exchange's rendering, which MailFlow
	// would fetch without the Bcc recipient and never look at again.)
	e, a, _, _ := seeded(t, 0)
	sent := e.mirrored(a, "Sent Items", "Sent Items") // open: its events are counted below
	e.srv.HoldAfter("SendMail", testUser, "", 0)      // the call waits after Exchange saved its copy
	submitted := submittedMIME("inflight-1@mailflow.test", "In flight", e.clock.Now())
	done := make(chan error, 1)
	go func() { done <- a.SendMail(context.Background(), submitted) }()

	eventually(t, "Exchange's copy is synced while SendMail has not returned", func() bool { return serverCount(a, sent.ID) == 1 })
	select {
	case err := <-done:
		t.Fatalf("SendMail returned before it was released: %v", err)
	default:
	}
	serves(t, a, sent.ID, 1, submitted, "the copy picked up while the submission was in flight")

	e.srv.Release("SendMail", testUser, "")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	res, err := a.Append("Sent Items", clientCopyOf(submitted), store.FlagSeen, "", time.Time{}, "s")
	if err != nil || res.UID != 1 || !res.Duplicate {
		t.Fatalf("APPEND after the reply: %+v %v", res, err)
	}
	serves(t, a, sent.ID, 1, submitted, "after the reply and the APPEND")
	if n := keptCopies(t, a); n != 0 {
		t.Errorf("%d submitted messages still kept", n)
	}
}

func TestTheSentCopyShowsTheBccRecipientsWhileTheSentFolderIsStillClosed(t *testing.T) {
	// The Sent folder is doing its first sync, so Exchange's copy is staged, out of
	// sight, until the folder opens. The client's placeholder gets its UID at once.
	setup := func(t *testing.T) (*env, *engine.Account, *store.Folder, string) {
		e := newEnv(t)
		sentSID := e.srv.FolderID(testUser, "Sent Items")
		old := e.clock.Now().Add(-5 * time.Hour)
		e.srv.AddMessage(testUser, sentSID, mailBytes("old1@x", "first", old, ""), fakeupstream.AddOptions{Date: old, Read: true})
		e.srv.AddMessage(testUser, sentSID, bigMail("old2@x", "second", old.Add(time.Hour)), fakeupstream.AddOptions{Date: old.Add(time.Hour), Read: true})
		e.srv.HoldAfter("FetchMIME", testUser, sentSID, 0) // keeps the folder closed
		a := e.login()
		f := e.waitFolder(a, "Sent Items")
		eventually(t, "the old messages are staged or waiting", func() bool {
			p := e.progress(a, f.ID)
			return p.Staged == 1 && p.PendingFetch == 1
		})
		return e, a, f, sentSID
	}
	open := func(t *testing.T, e *env, a *engine.Account, f *store.Folder, sentSID string) {
		e.srv.Release("FetchMIME", testUser, sentSID)
		eventually(t, "the folder opens", func() bool {
			cur, _ := a.Store().FolderByID(f.ID)
			return cur.InitialComplete
		})
	}

	t.Run("Exchange's copy is staged before the APPEND", func(t *testing.T) {
		e, a, f, sentSID := setup(t)
		submitted := submittedMIME("bcc-3@mailflow.test", "Closed 1", e.clock.Now())
		if err := a.SendMail(context.Background(), submitted); err != nil {
			t.Fatal(err)
		}
		eventually(t, "Exchange's copy is staged", func() bool { return e.progress(a, f.ID).Staged == 2 })
		res, err := a.Append("Sent Items", clientCopyOf(submitted), store.FlagSeen, "", time.Time{}, "s")
		if err != nil || res.Duplicate || res.UID != 1 {
			t.Fatalf("APPEND while the folder is closed: %+v %v", res, err)
		}
		serves(t, a, f.ID, 1, submitted, "the placeholder")
		open(t, e, a, f, sentSID)
		serves(t, a, f.ID, 1, submitted, "after the folder opened")
		if m, _ := a.Store().Message(f.ID, 1); m.Kind != store.KindServer || m.ServerID == "" {
			t.Errorf("not bound to Exchange's copy: %+v", m)
		}
		if got, _ := a.Store().UIDsByMessageID(f.ID, "bcc-3@mailflow.test"); len(got) != 1 {
			t.Errorf("the message has UIDs %v, want exactly one", got)
		}
		if n := keptCopies(t, a); n != 0 {
			t.Errorf("%d submitted messages still kept", n)
		}
	})
	t.Run("the APPEND comes before Exchange's copy", func(t *testing.T) {
		e, a, f, sentSID := setup(t)
		e.srv.HoldSentCopies(testUser, true)
		submitted := submittedMIME("bcc-4@mailflow.test", "Closed 2", e.clock.Now())
		if err := a.SendMail(context.Background(), submitted); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Append("Sent Items", clientCopyOf(submitted), store.FlagSeen, "", time.Time{}, "s"); err != nil {
			t.Fatal(err)
		}
		e.srv.ReleaseSentCopies(testUser)
		// A copy that finds a placeholder waiting binds to it at once, also while
		// the folder is closed; it is not staged.
		eventually(t, "Exchange's copy is bound to the placeholder", func() bool {
			m, err := a.Store().Message(f.ID, 1)
			return err == nil && m.Kind == store.KindServer && m.ServerID != ""
		})
		serves(t, a, f.ID, 1, submitted, "after binding, the folder still closed")
		if p := e.progress(a, f.ID); p.Staged != 1 {
			t.Errorf("%d staged messages, want only the old one", p.Staged)
		}
		open(t, e, a, f, sentSID)
		serves(t, a, f.ID, 1, submitted, "after the folder opened")
		if got, _ := a.Store().UIDsByMessageID(f.ID, "bcc-4@mailflow.test"); len(got) != 1 || got[0] != 1 {
			t.Errorf("the message has UIDs %v, want [1]", got)
		}
		if n := keptCopies(t, a); n != 0 {
			t.Errorf("%d submitted messages still kept", n)
		}
	})
	t.Run("the client never APPENDs", func(t *testing.T) {
		e, a, f, sentSID := setup(t)
		submitted := submittedMIME("bcc-5@mailflow.test", "Closed 3", e.clock.Now())
		if err := a.SendMail(context.Background(), submitted); err != nil {
			t.Fatal(err)
		}
		eventually(t, "Exchange's copy is staged", func() bool { return e.progress(a, f.ID).Staged == 2 })
		if n := keptCopies(t, a); n != 1 {
			t.Fatalf("%d submitted messages kept while the folder is closed, want 1", n)
		}
		open(t, e, a, f, sentSID)
		got, _ := a.Store().UIDsByMessageID(f.ID, "bcc-5@mailflow.test")
		if len(got) != 1 {
			t.Fatalf("the message has UIDs %v", got)
		}
		serves(t, a, f.ID, got[0], submitted, "Exchange's copy, opened with the folder")
		if n := keptCopies(t, a); n != 0 {
			t.Errorf("%d submitted messages still kept after the folder opened", n)
		}
	})
}

func TestTheSentCopyOfALargeMessageShowsTheBccRecipientsToo(t *testing.T) {
	// Above the inline limit Exchange's copy is announced by Sync and downloaded
	// with ItemOperations; it goes through the same door when it is stored.
	e, a, _, _ := seeded(t, 0)
	sent := e.mirrored(a, "Sent Items", "Sent Items") // open: its events are counted below
	big := append(submittedMIME("bcc-6@mailflow.test", "Big", e.clock.Now()), bytes.Repeat([]byte("0123456789abcdef"), 600)...)
	if err := a.SendMail(context.Background(), big); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Exchange's copy is downloaded and stored", func() bool { return serverCount(a, sent.ID) == 1 })
	serves(t, a, sent.ID, 1, big, "the downloaded copy")
}

func TestAPlaceholderThatBindsWithoutTheSubmittedBytesGetsThemThen(t *testing.T) {
	// A client that APPENDs before it submits: the placeholder was made from the
	// client's copy, because nothing had been submitted yet. When Exchange's copy
	// binds to it, the placeholder is given the submitted bytes. It is the one case
	// in which a visible message changes its bytes: the UID and the ServerId stay,
	// clients hear nothing, and the size that RFC822.SIZE reports follows.
	e, a, _, _ := seeded(t, 0)
	sent := e.mirrored(a, "Sent Items", "Sent Items") // open: its events are counted below
	rec, _ := e.watch(a, sent)
	e.srv.HoldSentCopies(testUser, true)

	submitted := submittedMIME("bcc-7@mailflow.test", "Backwards", e.clock.Now())
	client := clientCopyOf(submitted)
	if _, err := a.Append("Sent Items", client, store.FlagSeen, "", time.Time{}, "s"); err != nil {
		t.Fatal(err)
	}
	serves(t, a, sent.ID, 1, client, "the placeholder before the submission")
	if err := a.SendMail(context.Background(), submitted); err != nil {
		t.Fatal(err)
	}
	e.srv.ReleaseSentCopies(testUser)
	eventually(t, "Exchange's copy is bound", func() bool {
		m, err := a.Store().Message(sent.ID, 1)
		return err == nil && m.Kind == store.KindServer && m.ServerID != ""
	})
	serves(t, a, sent.ID, 1, submitted, "after binding")
	if rec.count(store.EvAdd) != 1 || rec.count(store.EvResize) != 1 || rec.count(store.EvExpunge) != 0 || rec.count(store.EvFlags) != 0 {
		t.Fatalf("events %+v: want one EXISTS (the placeholder) and one silent size correction", rec.all())
	}
	for _, ev := range rec.all() {
		if ev.Kind == store.EvResize && (ev.UID != 1 || ev.Size != int64(len(submitted))) {
			t.Errorf("resize event %+v, want UID 1 and size %d", ev, len(submitted))
		}
	}
	if c, _ := a.Store().Counts(sent.ID); c.Messages != 1 || c.UIDNext != 2 {
		t.Errorf("counts %+v", c)
	}
	if n := keptCopies(t, a); n != 0 {
		t.Errorf("%d submitted messages still kept", n)
	}
}

func TestTheKeptCopyOutlivesTheEngine(t *testing.T) {
	// The engine stops when nobody is connected for the grace period. The bytes are
	// in the database, so the engine that starts at the next login still finds them
	// when Exchange's copy turns up.
	// The grace period runs on the (real-time) fake clock from the engine's creation, and
	// nothing is attached: a short one (it was 50 ms) can end before the test has sent its
	// first request when the machine is slow (-race, a bigger schema). The test moves the
	// clock itself to end the grace, so a long one costs only the reaper's tick (a tenth).
	e := newEnv(t, func(c *engine.Config, _ *fakeupstream.Server) { c.Grace = time.Second })
	a := e.login()
	sent := e.mirrored(a, "Sent Items", "Sent Items")
	e.srv.HoldSentCopies(testUser, true)
	submitted := submittedMIME("outlive-1@mailflow.test", "Outlives the engine", e.clock.Now())
	if err := a.SendMail(context.Background(), submitted); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Hour) // far inside SubmittedTTL
	eventually(t, "the engine stops", func() bool { return a.Stopped() })

	b := e.login()
	if b == a {
		t.Fatal("expected a fresh engine")
	}
	if n := keptCopies(t, b); n != 1 {
		t.Fatalf("%d kept after the restart, want 1", n)
	}
	e.srv.ReleaseSentCopies(testUser)
	eventually(t, "Exchange's copy is synced", func() bool { return serverCount(b, sent.ID) == 1 })
	serves(t, b, sent.ID, 1, submitted, "Exchange's copy after the engine restarted")
	if n := keptCopies(t, b); n != 0 {
		t.Errorf("%d kept after the copy was stored", n)
	}
}

func TestADeletedSentMessageTakesItsKeptCopyWithIt(t *testing.T) {
	e, a, _, _ := seeded(t, 0)
	sent := e.mirrored(a, "Sent Items", "Sent Items") // open: its events are counted below
	e.srv.HoldSentCopies(testUser, true)
	submitted := submittedMIME("bcc-8@mailflow.test", "Deleted early", e.clock.Now())
	if err := a.SendMail(context.Background(), submitted); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Append("Sent Items", clientCopyOf(submitted), store.FlagSeen, "", time.Time{}, "s"); err != nil {
		t.Fatal(err)
	}
	if n := keptCopies(t, a); n != 1 {
		t.Fatalf("%d kept, want 1 (the copy is waiting for Exchange's)", n)
	}
	// The user deletes the placeholder before Exchange's copy arrives.
	if _, err := a.SetFlags(sent.ID, []uint32{1}, engine.FlagAdd, store.FlagDeleted, nil, "s"); err != nil {
		t.Fatal(err)
	}
	if gone, err := a.Expunge(sent.ID, nil, "s"); err != nil || len(gone) != 1 {
		t.Fatalf("expunge: %v %v", gone, err)
	}
	if n := keptCopies(t, a); n != 0 {
		t.Fatalf("%d submitted messages still kept after the Sent message was expunged", n)
	}
}

func TestAMovedSentMessageKeepsServingTheSubmittedBytesInItsNewFolder(t *testing.T) {
	// IMAP MOVE copies the stored message, so the bytes go along. (Deleting with
	// \Deleted and EXPUNGE lets Exchange move the item to Deleted Items instead, and
	// what Exchange renders there has no Bcc header.)
	e, a, _, _ := seeded(t, 0)
	sent := e.mirrored(a, "Sent Items", "Sent Items") // open: its events are counted below
	submitted := submittedMIME("bcc-9@mailflow.test", "Moved", e.clock.Now())
	if err := a.SendMail(context.Background(), submitted); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Exchange's copy is synced", func() bool { return serverCount(a, sent.ID) == 1 })
	junk := e.mirrored(a, "Junk Email", "Junk Email")
	out, err := a.Move(context.Background(), sent.ID, []uint32{1}, junk.ID, "s")
	if err != nil || len(out.Moved) != 1 {
		t.Fatalf("move: %+v %v", out, err)
	}
	serves(t, a, junk.ID, out.Moved[0].Dst, submitted, "the moved message")
	if n := keptCopies(t, a); n != 0 {
		t.Errorf("%d submitted messages kept", n)
	}
	// Exchange's own view of the moved item is the copy without Bcc; the bridge's
	// is the one it submitted, and it stays so when the folder is synced again.
	e.srv.ResetFolder(testUser, e.srv.FolderID(testUser, "Junk Email"))
	eventually(t, "the folder is resynced", func() bool { return strings.Contains(e.logs.String(), "reconciliation finished") })
	serves(t, a, junk.ID, out.Moved[0].Dst, submitted, "after another sync of the folder")
}

func TestKeptCopiesExpireAndAreBounded(t *testing.T) {
	e := newEnv(t, func(c *engine.Config, _ *fakeupstream.Server) {
		c.SubmittedTTL = time.Hour
		c.SubmittedMax = 2
	})
	a := e.login()
	e.mirrored(a, "Inbox", "INBOX")
	e.srv.HoldSentCopies(testUser, true) // Exchange never delivers: nobody claims the copies
	for i, id := range []string{"b1@mailflow.test", "b2@mailflow.test", "b3@mailflow.test"} {
		if err := a.SendMail(context.Background(), submittedMIME(id, "Subject", e.clock.Now())); err != nil {
			t.Fatal(err)
		}
		e.clock.Advance(time.Second)
		if want := min(i+1, 2); keptCopies(t, a) != want {
			t.Fatalf("after %d submissions %d are kept, want %d (at most SubmittedMax)", i+1, keptCopies(t, a), want)
		}
	}
	e.clock.Advance(2 * time.Hour)
	eventually(t, "housekeeping drops the copies nobody claimed", func() bool { return keptCopies(t, a) == 0 })
	if !strings.Contains(e.logs.String(), "never claimed were dropped") {
		t.Errorf("no log line about the dropped copies")
	}
}

func TestAFailedSubmissionKeepsItsCopyOnlyIfTheMessageMayHaveGoneOut(t *testing.T) {
	e, a, _, _ := seeded(t, 0)
	ctx := context.Background()
	// Exchange refuses the message: it did not go out, nothing is kept.
	e.srv.FailNext("SendMail", &upstream.StatusError{Op: "SendMail", Code: 116, Perm: true}, 1)
	if err := a.SendMail(ctx, submittedMIME("f1@mailflow.test", "Refused", e.clock.Now())); err == nil {
		t.Fatal("the refused message was accepted")
	}
	if n := keptCopies(t, a); n != 0 {
		t.Errorf("%d kept after Exchange refused the message", n)
	}
	// The request never left: it did not go out either.
	e.srv.FailNext("SendMail", upstream.ErrNotSent, 1)
	if err := a.SendMail(ctx, submittedMIME("f2@mailflow.test", "Not sent", e.clock.Now())); err == nil {
		t.Fatal("expected an error")
	}
	if n := keptCopies(t, a); n != 0 {
		t.Errorf("%d kept after a request that never left", n)
	}
	// A timeout may have happened after Exchange took the message, and its Sent
	// copy may still turn up: the copy waits for it.
	e.srv.FailNext("SendMail", upstream.ErrUnavailable, 1)
	failed := submittedMIME("f3@mailflow.test", "Timed out", e.clock.Now())
	if err := a.SendMail(ctx, failed); err == nil {
		t.Fatal("expected an error")
	}
	if n := keptCopies(t, a); n != 1 {
		t.Errorf("%d kept after an ambiguous failure, want 1", n)
	}
	// MailFlow submits it again; the retry replaces the registration, it does not add one.
	if err := a.SendMail(ctx, failed); err != nil {
		t.Fatal(err)
	}
	if n := keptCopies(t, a); n != 1 {
		t.Errorf("%d kept after the retry, want 1", n)
	}
}

func TestOnlyAMessageWithABccHeaderAndAMessageIDIsKept(t *testing.T) {
	e, a, _, _ := seeded(t, 0)
	sent := e.mirrored(a, "Sent Items", "Sent Items") // open: its events are counted below
	ctx := context.Background()
	// No Bcc header: Exchange's copy lists the same recipients, nothing to keep.
	plain := clientCopyOf(submittedMIME("p1@mailflow.test", "Plain", e.clock.Now()))
	if err := a.SendMail(ctx, plain); err != nil {
		t.Fatal(err)
	}
	// A Bcc header but no Message-ID: Exchange's copy cannot be recognised.
	noID := bytes.Replace(submittedMIME("x", "No id", e.clock.Now()), []byte("Message-ID: <x>\r\n"), nil, 1)
	if err := a.SendMail(ctx, noID); err != nil {
		t.Fatal(err)
	}
	if n := keptCopies(t, a); n != 0 {
		t.Fatalf("%d kept, want none", n)
	}
	eventually(t, "both copies are synced", func() bool { return serverCount(a, sent.ID) == 2 })
	// A plain message is served as Exchange rendered it, which here is the same bytes.
	uids, _ := a.Store().UIDsByMessageID(sent.ID, "p1@mailflow.test")
	if len(uids) != 1 {
		t.Fatalf("UIDs %v", uids)
	}
	serves(t, a, sent.ID, uids[0], plain, "a message without a Bcc header")
}

func TestRegistrationFailingDoesNotFailTheSubmission(t *testing.T) {
	// The message goes out; only its Sent copy cannot show the Bcc recipients.
	e, a, _, _ := seeded(t, 0)
	var failing atomic.Bool
	e.db.SetWriteHook(func() error {
		if failing.Load() {
			return store.ErrInjected
		}
		return nil
	})
	t.Cleanup(func() { e.db.SetWriteHook(nil) })
	failing.Store(true) // the registration is the one write SendMail makes, and it happens inside the call
	err := a.SendMail(context.Background(), submittedMIME("w1@mailflow.test", "Database trouble", e.clock.Now()))
	failing.Store(false)
	if err != nil {
		t.Fatalf("SendMail failed because the copy could not be kept: %v", err)
	}
	if n := len(e.srv.Sent(testUser)); n != 1 {
		t.Fatalf("Exchange received %d submissions, want 1", n)
	}
	if n := keptCopies(t, a); n != 0 {
		t.Errorf("%d kept", n)
	}
	if !strings.Contains(e.logs.String(), "could not keep the submitted message") {
		t.Errorf("the failure was not logged")
	}
	for _, leak := range []string{hiddenRcpt, "w1@mailflow.test", "Database trouble"} {
		if strings.Contains(e.logs.String(), leak) {
			t.Errorf("the log contains %q", leak)
		}
	}
}

func fileBytesIn(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, de := range entries {
		if de.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, de.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[de.Name()] = b
	}
	return out
}

func TestRemoteWipeClearsTheKeptCopies(t *testing.T) {
	e, a, _, _ := seeded(t, 1)
	e.srv.HoldSentCopies(testUser, true) // the copy waits; nobody claims it
	marker := "wipe-bcc-recipient-" + strings.Repeat("z", 12) + "@example.test"
	msg := bytes.ReplaceAll(submittedMIME("wipe-1@mailflow.test", "Wipe me", e.clock.Now()), []byte(hiddenRcpt), []byte(marker))
	if err := a.SendMail(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if n := keptCopies(t, a); n != 1 {
		t.Fatalf("%d kept, want 1", n)
	}
	// The copy is in the state database (the main file or its write-ahead log).
	found := false
	for _, b := range fileBytesIn(t, e.dir) {
		found = found || bytes.Contains(b, []byte(marker))
	}
	if !found {
		t.Fatal("the kept copy is not in the state database: the check below proves nothing")
	}

	e.srv.RequestWipe(testUser)
	eventually(t, "WIPED marker written", func() bool { return store.WipeMarkerExists(e.dir) })
	eventually(t, "wipe acknowledged to Exchange", func() bool { return e.srv.WipeAcknowledged(testUser) })
	eventually(t, "the old database is gone and a fresh one is open", func() bool {
		acct, err := e.mgr.DB().Account(testUser)
		if err != nil {
			return false
		}
		fs, _ := acct.Folders()
		return len(fs) == 0
	})
	for name, b := range fileBytesIn(t, e.dir) {
		if bytes.Contains(b, []byte(marker)) {
			t.Errorf("%s still holds the kept copy after the wipe", name)
		}
	}
	acct, _ := e.mgr.DB().Account(testUser)
	if n, _, err := acct.SentCopies(); err != nil || n != 0 {
		t.Errorf("the fresh database holds %d kept copies (%v)", n, err)
	}
}

// ---- Bcc recipients of messages sent from elsewhere (the email2:Bcc property) ----
//
// Exchange's MIME rendering of a message in Sent Items has no Bcc header, whoever
// sent it. A message sent from a phone or from OWA does not pass through the
// bridge, so there are no submitted bytes; if Exchange lists the Bcc recipients of
// the item (MS-ASEMAIL 2.2.2.9 allows it from 16.0 on, 3.2.5.4 lets a Sync response
// carry any element of the E-mail class), the bridge writes them into the copy it
// serves, and asks for nothing new.

func TestAMessageSentFromElsewhereShowsTheBccRecipientsExchangeListsForIt(t *testing.T) {
	e := newEnv(t)
	sentSID := e.srv.FolderID(testUser, "Sent Items")
	inboxSID := e.srv.FolderID(testUser, "Inbox")
	now := e.clock.Now()
	plain := mailBytes("phone-1@x", "From my phone", now.Add(-3*time.Hour), "")
	withHeader := mailBytes("phone-2@x", "Has a header already", now.Add(-2*time.Hour), "Bcc: already@example.test\r\n")
	big := bigMail("phone-3@x", "Big, from OWA", now.Add(-time.Hour))
	e.srv.AddMessage(testUser, sentSID, plain, fakeupstream.AddOptions{Read: true, Date: now.Add(-3 * time.Hour), Bcc: `"Eve Example" <eve@example.test>, frank@example.test`})
	e.srv.AddMessage(testUser, sentSID, withHeader, fakeupstream.AddOptions{Read: true, Date: now.Add(-2 * time.Hour), Bcc: "other@example.test"})
	e.srv.AddMessage(testUser, sentSID, big, fakeupstream.AddOptions{Read: true, Date: now.Add(-time.Hour), Bcc: "gina@example.test"})
	e.srv.AddMessage(testUser, sentSID, mailBytes("phone-4@x", "No property", now.Add(-30*time.Minute), ""), fakeupstream.AddOptions{Read: true, Date: now.Add(-30 * time.Minute)})
	// A property on a message that is not in Sent Items is not for the bridge to use.
	e.srv.AddMessage(testUser, inboxSID, mailBytes("in-1@x", "Received", now.Add(-time.Hour), ""), fakeupstream.AddOptions{Bcc: "someone@example.test"})

	a := e.login()
	sent := e.mirrored(a, "Sent Items", "Sent Items")
	inbox := e.mirrored(a, "Inbox", "INBOX")
	uidOf := func(f *store.Folder, id string) uint32 {
		t.Helper()
		u, _ := a.Store().UIDsByMessageID(f.ID, id)
		if len(u) != 1 {
			t.Fatalf("%s has UIDs %v", id, u)
		}
		return u[0]
	}

	// Written into the copy that is served, with the names Exchange gave.
	u1 := uidOf(sent, "phone-1@x")
	got := string(storedMIME(t, a, sent.ID, u1))
	if !strings.Contains(got, "\r\nBcc: \"Eve Example\" <eve@example.test>, frank@example.test\r\n") {
		t.Errorf("the Bcc header is missing or wrong:\n%s", got)
	}
	// Otherwise the message is exactly what Exchange rendered: the header goes in
	// front of the blank line that ends the header block.
	cut := bytes.Index(plain, []byte("\r\n\r\n")) + 2
	want := string(plain[:cut]) + "Bcc: \"Eve Example\" <eve@example.test>, frank@example.test\r\n" + string(plain[cut:])
	if got != want {
		t.Errorf("the message changed in other ways than the Bcc header:\n%s\nwant\n%s", got, want)
	}
	serves(t, a, sent.ID, u1, []byte(got), "a message sent from a phone")
	// Not twice, and not over a header the message has.
	u2 := uidOf(sent, "phone-2@x")
	serves(t, a, sent.ID, u2, withHeader, "a message that has a Bcc header already")
	// A message that had to be downloaded, too.
	u3 := uidOf(sent, "phone-3@x")
	if got := string(storedMIME(t, a, sent.ID, u3)); !strings.Contains(got, "\r\nBcc: gina@example.test\r\n") || !strings.HasSuffix(got, string(big[bytes.Index(big, []byte("\r\n\r\n")):])) {
		t.Errorf("the downloaded message lost its Bcc property or its body:\n%.300s", got)
	}
	// Without the property, nothing changes.
	serves(t, a, sent.ID, uidOf(sent, "phone-4@x"), mailBytes("phone-4@x", "No property", now.Add(-30*time.Minute), ""), "a message without the property")
	// Only Sent Items.
	serves(t, a, inbox.ID, uidOf(inbox, "in-1@x"), mailBytes("in-1@x", "Received", now.Add(-time.Hour), ""), "a message of the Inbox")

	if n := strings.Count(e.logs.String(), "Exchange lists the Bcc recipients of a message in Sent Items"); n != 1 {
		t.Errorf("the log says it %d times, want once", n)
	}
	// A message that arrives later, while the folder is open, is treated the same.
	e.srv.AddMessage(testUser, sentSID, mailBytes("phone-5@x", "Later", now, ""), fakeupstream.AddOptions{Read: true, Date: now, Bcc: "late@example.test"})
	eventually(t, "the later message is synced", func() bool {
		u, _ := a.Store().UIDsByMessageID(sent.ID, "phone-5@x")
		return len(u) == 1
	})
	if got := string(storedMIME(t, a, sent.ID, uidOf(sent, "phone-5@x"))); !strings.Contains(got, "\r\nBcc: late@example.test\r\n") {
		t.Errorf("a later message has no Bcc header:\n%s", got)
	}
}

func TestTheSubmittedBytesWinOverTheBccProperty(t *testing.T) {
	// A message the bridge submitted is served with the bytes it submitted, whatever
	// the item is listed with.
	e, a, _, _ := seeded(t, 0)
	sent := e.mirrored(a, "Sent Items", "Sent Items") // open: its events are counted below
	sf := e.srv.FolderID(testUser, "Sent Items")
	submitted := submittedMIME("both-1@mailflow.test", "Both", e.clock.Now())
	e.srv.HoldSentCopies(testUser, true)
	if err := a.SendMail(context.Background(), submitted); err != nil {
		t.Fatal(err)
	}
	// Exchange's copy (without Bcc) is listed with a property that differs.
	e.srv.AddMessage(testUser, sf, clientCopyOf(submitted), fakeupstream.AddOptions{Read: true, Date: e.clock.Now(), Bcc: "someone-else@example.test"})
	eventually(t, "the copy is synced", func() bool { return serverCount(a, sent.ID) == 1 })
	serves(t, a, sent.ID, 1, submitted, "the submitted message")
	if strings.Contains(string(storedMIME(t, a, sent.ID, 1)), "someone-else") {
		t.Error("the property was used over the submitted bytes")
	}
}

func TestSendMailPassesTheSameBytesToExchangeAsBefore(t *testing.T) {
	// Regression: keeping a copy must not change what is handed to Exchange, and the
	// delivered copy (what the To recipient gets) has no Bcc header.
	e, a, _, _ := seeded(t, 0)
	submitted := submittedMIME("same-1@mailflow.test", "Unchanged", e.clock.Now())
	if err := a.SendMail(context.Background(), submitted); err != nil {
		t.Fatal(err)
	}
	sentMsgs := e.srv.Sent(testUser)
	if len(sentMsgs) != 1 || !bytes.Equal(sentMsgs[0].MIME, submitted) || !sentMsgs[0].SaveInSent {
		t.Fatalf("Exchange received %d submissions, the first %q", len(sentMsgs), sentMsgs[0].MIME)
	}
	if !bytes.Equal(sentMsgs[0].Delivered, clientCopyOf(submitted)) || bytes.Contains(sentMsgs[0].Delivered, []byte(hiddenRcpt)) || bytes.Contains(sentMsgs[0].Delivered, []byte("Bcc:")) {
		t.Fatalf("the delivered copy has a Bcc header or the hidden recipient:\n%s", sentMsgs[0].Delivered)
	}
}
