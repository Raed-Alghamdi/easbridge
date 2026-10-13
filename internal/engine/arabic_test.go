//go:build devfake

package engine_test

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"easbridge/internal/fakeupstream"
	"easbridge/internal/mailsamples"
	"easbridge/internal/store"
)

// The Sent-copy fallback compares the normalised subject and the recipients of the
// client's copy with Exchange's (real-Exchange finding 3: the mailbox holds mail in
// windows-1256, ISO-8859-6 and UTF-8). The two copies rarely use the same charset or
// the same encoding for the same text.

func TestSentFallbackBindsAnArabicCopyWhateverTheCharsetAndEncoding(t *testing.T) {
	// The client's copy: UTF-8, base64 (what nodemailer writes). Exchange's copy of the same message has
	// no Message-ID that matches, and its Subject and recipient name are in each charset and encoding in turn.
	client := mailsamples.Subjects[2]
	clientName := mailsamples.Names[2]
	for i := range mailsamples.Subjects {
		for _, enc := range []string{"B", "Q"} {
			subj, name := mailsamples.Subjects[i], mailsamples.Names[i]
			subjWord, nameWord := subj.B, name.Q
			if enc == "Q" {
				subjWord, nameWord = subj.Q, name.B
			}
			t.Run(subj.Charset+" "+enc, func(t *testing.T) {
				e, a, _, _ := seeded(t, 0)
				sent := e.folder(a, "Sent Items")
				now := e.clock.Now()
				ph := sentMessage("p@mailflow.test", client.B, clientName.B+" <bob@example.test>", now)
				if _, err := a.Append("Sent Items", ph, store.FlagSeen, "", time.Time{}, "s"); err != nil {
					t.Fatal(err)
				}
				// Exchange's copy: no Message-ID, the same Arabic text in another charset.
				copyWithoutID := bytes.Replace(sentMessage("x", subjWord, nameWord+" <BOB@example.test>", now), []byte("Message-ID: <x>\r\n"), nil, 1)
				e.srv.AddMessage(testUser, e.srv.FolderID(testUser, "Sent Items"), copyWithoutID, fakeupstream.AddOptions{Read: true})
				eventually(t, "the placeholder is bound to Exchange's copy", func() bool {
					m, err := a.Store().Message(sent.ID, 1)
					return err == nil && m.Kind == store.KindServer
				})
				time.Sleep(100 * time.Millisecond)
				if c, _ := a.Store().Counts(sent.ID); c.Messages != 1 {
					t.Fatalf("counts = %+v: the Sent copy was duplicated", c)
				}
			})
		}
	}
}

func TestSentFallbackDoesNotBindADifferentArabicSubject(t *testing.T) {
	e, a, _, _ := seeded(t, 0)
	sent := e.folder(a, "Sent Items")
	now := e.clock.Now()
	ph := sentMessage("p@mailflow.test", mailsamples.Subjects[0].B, mailsamples.Names[0].Q+" <bob@example.test>", now)
	if _, err := a.Append("Sent Items", ph, store.FlagSeen, "", time.Time{}, "s"); err != nil {
		t.Fatal(err)
	}
	// The same recipient and time, another subject (the long one), and the same subject for another
	// recipient: neither is the client's message.
	sf := e.srv.FolderID(testUser, "Sent Items")
	noID := func(subject, to string) []byte {
		return bytes.Replace(sentMessage("x", subject, to, now), []byte("Message-ID: <x>\r\n"), nil, 1)
	}
	e.srv.AddMessage(testUser, sf, noID(mailsamples.LongSubjects[0].B, mailsamples.Names[0].Q+" <bob@example.test>"), fakeupstream.AddOptions{Read: true})
	e.srv.AddMessage(testUser, sf, noID(mailsamples.Subjects[1].Q, mailsamples.Names[1].B+" <eve@example.test>"), fakeupstream.AddOptions{Read: true})
	eventually(t, "the two other messages arrive", func() bool { return serverCount(a, sent.ID) == 2 })
	if c, _ := a.Store().Counts(sent.ID); c.Messages != 3 {
		t.Fatalf("counts = %+v", c)
	}
	if m, _ := a.Store().Message(sent.ID, 1); m.Kind != store.KindPlaceholder {
		t.Fatalf("the placeholder was bound to a different message: %+v", m)
	}
}

func TestStoredSubjectOfArabicMailIsTheDecodedText(t *testing.T) {
	// What the fallback and SEARCH SUBJECT compare is the subject stored with the message.
	e, a, f, _ := seeded(t, 0)
	inbox := e.srv.FolderID(testUser, "Inbox")
	n := 0
	for _, w := range append(append([]mailsamples.Word{}, mailsamples.Subjects...), mailsamples.LongSubjects...) {
		for _, word := range []string{w.B, w.Q} {
			if word == "" {
				continue
			}
			n++
			id := fmt.Sprintf("subj%d@x", n)
			e.srv.AddMessage(testUser, inbox, mailsamples.Mail(id, word, mailsamples.Names[0].Q, mailsamples.Bodies[0], "8bit"),
				fakeupstream.AddOptions{Date: e.clock.Now().Add(time.Duration(n) * time.Minute)})
		}
	}
	eventually(t, "the messages arrive", func() bool { return serverCount(a, f.ID) == n })
	ms, err := a.Store().ListMessages(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, mm := range ms {
		m, err := a.Store().Message(f.ID, mm.UID)
		if err != nil {
			t.Fatal(err)
		}
		if m.Subject != mailsamples.Subject && m.Subject != mailsamples.LongSubject {
			t.Errorf("UID %d: stored subject %q", mm.UID, m.Subject)
		}
	}
}
