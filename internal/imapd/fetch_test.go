//go:build devfake

package imapd_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"easbridge/internal/fakeupstream"
	"easbridge/internal/testenv"
)

func multipartMessage(id string, date time.Time) []byte {
	return []byte("Message-ID: <" + id + ">\r\n" +
		"From: Bob Builder <bob@example.test>\r\n" +
		"To: alice@example.test, Carol <carol@example.test>\r\n" +
		"Cc: dave@example.test\r\n" +
		"Subject: =?UTF-8?Q?Quarterly_report_=E2=9C=93?=\r\n" +
		"In-Reply-To: <parent@example.test>\r\n" +
		"Date: " + date.Format("Mon, 02 Jan 2006 15:04:05 -0700") + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"B1\"\r\n\r\n" +
		"--B1\r\nContent-Type: multipart/alternative; boundary=\"B2\"\r\n\r\n" +
		"--B2\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 7bit\r\n\r\nplain part text\r\n" +
		"--B2\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>html part</p>\r\n" +
		"--B2--\r\n" +
		"--B1\r\nContent-Type: application/pdf; name=\"r.pdf\"\r\nContent-Disposition: attachment; filename=\"r.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQK\r\n" +
		"--B1--\r\n")
}

func TestFetchPatternsMailFlowUses(t *testing.T) {
	e := newIMAP(t)
	inbox := e.Srv.FolderID(testenv.User, "Inbox")
	date := e.Clock.Now().Add(-26 * time.Hour).UTC().Truncate(time.Second)
	raw := multipartMessage("multi@x", date)
	e.Srv.AddMessage(testenv.User, inbox, raw, fakeupstream.AddOptions{Date: date, Flagged: true})
	c := e.login(nil)
	e.ready("Inbox", "INBOX")
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}

	// 1. The new-mail FETCH: UID FLAGS BODYSTRUCTURE ENVELOPE INTERNALDATE RFC822.SIZE BODY.PEEK[HEADER]
	set := imap.UIDSet{}
	set.AddRange(1, 0) // 1:*
	msgs, err := c.Fetch(set, &imap.FetchOptions{
		UID: true, Flags: true, InternalDate: true, RFC822Size: true, Envelope: true,
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
		BodySection:   []*imap.FetchItemBodySection{{Specifier: imap.PartSpecifierHeader, Peek: true}},
	}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("%d messages", len(msgs))
	}
	m := msgs[0]
	if m.UID != 1 || m.SeqNum != 1 || len(m.Flags) != 1 || m.Flags[0] != imap.FlagFlagged {
		t.Errorf("uid/seq/flags: %+v", m)
	}
	if !m.InternalDate.Equal(date) || m.RFC822Size != int64(len(raw)) {
		t.Errorf("internaldate %v size %d (want %v, %d)", m.InternalDate, m.RFC822Size, date, len(raw))
	}
	env := m.Envelope
	if env == nil || env.Subject != "Quarterly report ✓" || env.MessageID != "multi@x" || len(env.InReplyTo) != 1 || env.InReplyTo[0] != "parent@example.test" {
		t.Fatalf("envelope = %+v", env)
	}
	if len(env.From) != 1 || env.From[0].Name != "Bob Builder" || env.From[0].Mailbox != "bob" || env.From[0].Host != "example.test" ||
		len(env.To) != 2 || env.To[1].Name != "Carol" || len(env.Cc) != 1 || len(env.Bcc) != 0 {
		t.Errorf("addresses = %+v", env)
	}
	bs, ok := m.BodyStructure.(*imap.BodyStructureMultiPart)
	if !ok || bs.Subtype != "mixed" || len(bs.Children) != 2 {
		t.Fatalf("bodystructure = %#v", m.BodyStructure)
	}
	alt, ok := bs.Children[0].(*imap.BodyStructureMultiPart)
	if !ok || alt.Subtype != "alternative" || len(alt.Children) != 2 {
		t.Fatalf("alternative = %#v", bs.Children[0])
	}
	if pdf, ok := bs.Children[1].(*imap.BodyStructureSinglePart); !ok || pdf.Type != "application" || pdf.Subtype != "pdf" || pdf.Extended == nil || pdf.Extended.Disposition == nil {
		t.Errorf("attachment = %#v", bs.Children[1])
	}
	hdr := m.FindBodySection(&imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, Peek: true})
	if !bytes.HasPrefix(hdr, []byte("Message-ID: <multi@x>\r\n")) || !bytes.HasSuffix(hdr, []byte("\r\n\r\n")) || bytes.Contains(hdr, []byte("plain part")) {
		t.Errorf("BODY[HEADER] = %q", hdr)
	}

	// 2. The indexer: BODY.PEEK[n] for several parts at once.
	part := func(spec imap.PartSpecifier, p ...int) *imap.FetchItemBodySection {
		return &imap.FetchItemBodySection{Specifier: spec, Part: p, Peek: true}
	}
	secs := []*imap.FetchItemBodySection{
		part(imap.PartSpecifierNone, 1, 1), part(imap.PartSpecifierNone, 1, 2), part(imap.PartSpecifierNone, 2),
		part(imap.PartSpecifierMIME, 2), part(imap.PartSpecifierNone, 7), part(imap.PartSpecifierNone, 1, 9),
		{Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"Subject", "Date"}, Peek: true},
		{Specifier: imap.PartSpecifierText, Peek: true},
		{Peek: true},
		{Peek: true, Partial: &imap.SectionPartial{Offset: 4, Size: 10}},
	}
	msgs, err = c.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{UID: true, BodySection: secs}).Collect()
	if err != nil {
		t.Fatalf("part fetch: %v", err)
	}
	m = msgs[0]
	if got := string(m.FindBodySection(secs[0])); got != "plain part text" {
		t.Errorf("BODY[1.1] = %q", got)
	}
	if got := string(m.FindBodySection(secs[1])); got != "<p>html part</p>" {
		t.Errorf("BODY[1.2] = %q", got)
	}
	if got := strings.TrimSpace(string(m.FindBodySection(secs[2]))); got != "JVBERi0xLjQK" {
		t.Errorf("BODY[2] = %q", got)
	}
	if got := string(m.FindBodySection(secs[3])); !strings.Contains(got, "application/pdf") || strings.Contains(got, "JVBER") {
		t.Errorf("BODY[2.MIME] = %q", got)
	}
	// Absent parts are answered with an empty section, never an error.
	for _, i := range []int{4, 5} {
		if got := m.FindBodySection(secs[i]); len(got) != 0 {
			t.Errorf("absent part %d returned %q", i, got)
		}
	}
	if got := string(m.FindBodySection(secs[6])); !strings.HasPrefix(got, "Subject:") || !strings.Contains(got, "Date:") || strings.Contains(got, "Message-ID") {
		t.Errorf("HEADER.FIELDS = %q", got)
	}
	if got := m.FindBodySection(secs[7]); !bytes.HasPrefix(got, []byte("--B1")) {
		t.Errorf("BODY[TEXT] = %q", got)
	}
	if got := m.FindBodySection(secs[8]); !bytes.Equal(got, raw) {
		t.Errorf("BODY[] differs from the stored message (%d vs %d bytes)", len(got), len(raw))
	}
	if got := string(m.FindBodySection(secs[9])); got != string(raw[4:14]) {
		t.Errorf("BODY[]<4.10> = %q, want %q", got, raw[4:14])
	}
}

func TestUIDFetchOpenEndedRangeIncludesTheLastMessage(t *testing.T) {
	// RFC 3501: "UID n:*" always includes the highest UID, even when n is above
	// it. MailFlow asks for "maxKnown+1:*" after every notification and filters.
	e := newIMAP(t)
	e.seedInbox(3)
	c := e.login(nil)
	e.ready("Inbox", "INBOX")
	c.Select("INBOX", nil).Wait()
	// (Asked over the raw socket: the client library filters answers against the
	// requested set itself and cannot tell that "*" is 3.)
	r := e.rawDial()
	r.loginOK()
	r.ok("SELECT INBOX")
	if l := joinLines(r.ok("UID FETCH 10:* (UID)")); !strings.Contains(l, "* 3 FETCH (UID 3)") {
		t.Fatalf("UID FETCH 10:* = %s", l)
	}
	if l := joinLines(r.ok("UID FETCH 10:20 (UID)")); strings.Contains(l, "FETCH (") {
		t.Fatalf("a static range beyond the end matched: %s", l)
	}
	set := imap.UIDSet{}
	set.AddRange(2, 0)
	if msgs, _ := c.Fetch(set, &imap.FetchOptions{UID: true}).Collect(); len(msgs) != 2 {
		t.Fatalf("UID FETCH 2:* = %+v", msgs)
	}
	// Sequence sets and "*".
	seq := imap.SeqSet{}
	seq.AddRange(2, 0)
	if msgs, _ := c.Fetch(seq, &imap.FetchOptions{UID: true}).Collect(); len(msgs) != 2 || msgs[0].SeqNum != 2 {
		t.Fatalf("FETCH 2:* = %+v", msgs)
	}
	// Unknown ranges yield nothing, not an error.
	if msgs, err := c.Fetch(imap.UIDSetNum(99), &imap.FetchOptions{UID: true}).Collect(); err != nil || len(msgs) != 0 {
		t.Fatalf("UID FETCH 99 = %+v %v", msgs, err)
	}
	// FETCH with no mailbox selected is refused politely.
	c2 := e.login(nil)
	if _, err := c2.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{UID: true}).Collect(); err == nil {
		t.Error("FETCH without a selected mailbox succeeded")
	}
}

func TestFetchBodyWithoutPeekMarksSeenAndReportsFlags(t *testing.T) {
	e := newIMAP(t)
	inbox := e.Srv.FolderID(testenv.User, "Inbox")
	date := e.Clock.Now().Add(-time.Hour)
	e.Srv.AddMessage(testenv.User, inbox, testenv.Mail("unread@x", "u", date, ""), fakeupstream.AddOptions{Date: date})
	c := e.login(nil)
	e.ready("Inbox", "INBOX")
	c.Select("INBOX", nil).Wait()
	// PEEK leaves the message unread.
	c.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{Peek: true}}}).Collect()
	if st, _ := c.Status("INBOX", &imap.StatusOptions{NumUnseen: true}).Wait(); *st.NumUnseen != 1 {
		t.Fatalf("PEEK changed the flags")
	}
	// A plain BODY[] marks it \Seen and the response says so.
	msgs, err := c.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{}}}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs[0].Flags) != 1 || msgs[0].Flags[0] != imap.FlagSeen {
		t.Fatalf("implicit \\Seen not reported: %+v", msgs[0].Flags)
	}
	if st, _ := c.Status("INBOX", &imap.StatusOptions{NumUnseen: true}).Wait(); *st.NumUnseen != 0 {
		t.Fatalf("message still unseen")
	}
	// ... and it reaches Exchange.
	testenv.Eventually(t, "\\Seen on Exchange", func() bool {
		for _, it := range e.Srv.Items(testenv.User, inbox) {
			return it.Read
		}
		return false
	})
}

func TestLargeMessageIsFetchableOnlyWithRealSizeAndStructure(t *testing.T) {
	e := newIMAP(t)
	inbox := e.Srv.FolderID(testenv.User, "Inbox")
	date := e.Clock.Now().Add(-time.Hour)
	payload := strings.Repeat("0123456789abcdef", 6000) // 96 KB, far above the 4 KB inline limit
	raw := []byte("Message-ID: <big@x>\r\nFrom: b@example.test\r\nTo: alice@example.test\r\nSubject: big\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=X\r\n\r\n--X\r\nContent-Type: text/plain\r\n\r\nhi\r\n--X\r\nContent-Type: application/octet-stream\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" + payload + "\r\n--X--\r\n")
	e.Srv.AddMessage(testenv.User, inbox, raw, fakeupstream.AddOptions{Date: date})
	c := e.login(nil)
	e.ready("Inbox", "INBOX")
	c.Select("INBOX", nil).Wait()
	msgs, err := c.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{RFC822Size: true, BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
		BodySection: []*imap.FetchItemBodySection{{Part: []int{2}, Peek: true}}}).Collect()
	if err != nil || len(msgs) != 1 {
		t.Fatalf("%v %v", msgs, err)
	}
	if msgs[0].RFC822Size != int64(len(raw)) {
		t.Errorf("size = %d, want %d", msgs[0].RFC822Size, len(raw))
	}
	if bs, ok := msgs[0].BodyStructure.(*imap.BodyStructureMultiPart); !ok || len(bs.Children) != 2 {
		t.Errorf("structure = %#v", msgs[0].BodyStructure)
	}
	if got := msgs[0].FindBodySection(&imap.FetchItemBodySection{Part: []int{2}, Peek: true}); !strings.Contains(string(got), payload) {
		t.Errorf("attachment truncated: %d bytes", len(got))
	}
}

func TestWireFormatOfTheNewMailFetch(t *testing.T) {
	// The exact command imapflow sends (see DESIGN section 9), over a raw socket.
	e := newIMAP(t)
	inbox := e.Srv.FolderID(testenv.User, "Inbox")
	date := time.Date(2026, 10, 7, 9, 30, 0, 0, time.UTC)
	e.Srv.AddMessage(testenv.User, inbox, testenv.Mail("w@x", "wire", date, ""), fakeupstream.AddOptions{Date: date})
	e.login(nil)
	e.ready("Inbox", "INBOX")
	r := e.rawDial()
	r.loginOK()
	r.ok("SELECT INBOX")
	lines := r.ok("UID FETCH 1:* (UID FLAGS BODYSTRUCTURE ENVELOPE INTERNALDATE RFC822.SIZE BODY.PEEK[HEADER])")
	all := joinLines(lines)
	for _, want := range []string{"* 1 FETCH (", "UID 1", "FLAGS ()", `INTERNALDATE " 7-Oct-2026 09:30:00 +0000"`, "RFC822.SIZE ", "BODYSTRUCTURE ((", `ENVELOPE ("`, "BODY[HEADER] {"} {
		if want == "BODYSTRUCTURE ((" {
			want = `BODYSTRUCTURE ("text" "plain"`
		}
		if !strings.Contains(all, want) {
			t.Errorf("response lacks %q:\n%s", want, all)
		}
	}
	// A non-existent section on the wire.
	lines = r.ok("UID FETCH 1 (BODY.PEEK[9])")
	if !strings.Contains(joinLines(lines), "BODY[9] {0}") {
		t.Errorf("absent section: %v", lines)
	}
	_ = fmt.Sprint
}
