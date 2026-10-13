//go:build devfake

package imapd_test

import (
	"bytes"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"easbridge/internal/fakeupstream"
	"easbridge/internal/mimestruct"
	"easbridge/internal/testenv"
)

// Regression tests for the BODYSTRUCTURE denial of service found by the
// security review of v0.1.0 (see internal/mimestruct): one hostile message must
// not be able to exhaust memory or CPU through FETCH or SEARCH.

func nestedRFC822(levels int) []byte {
	var b bytes.Buffer
	b.WriteString("Message-ID: <hostile-rfc822@attacker.test>\r\nFrom: attacker@example.test\r\nTo: alice@example.test\r\nSubject: nested rfc822\r\nDate: Fri, 09 Oct 2026 12:00:00 +0000\r\n")
	for i := 0; i < levels; i++ {
		b.WriteString("Content-Type: message/rfc822\r\n\r\nSubject: level\r\n")
	}
	b.WriteString("Content-Type: text/plain\r\n\r\nneedle-in-the-innermost-body\r\n")
	return b.Bytes()
}

func nestedMultipart(levels int) []byte {
	var b bytes.Buffer
	b.WriteString("Message-ID: <hostile-multipart@attacker.test>\r\nFrom: attacker@example.test\r\nTo: alice@example.test\r\nSubject: nested multipart\r\nDate: Fri, 09 Oct 2026 13:00:00 +0000\r\n")
	for i := 0; i < levels; i++ {
		fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=\"b%d\"\r\n\r\n--b%d\r\n", i, i)
	}
	b.WriteString("Content-Type: text/plain\r\n\r\nneedle-in-the-innermost-body\r\n")
	return b.Bytes()
}

func TestHostileMessagesCannotExhaustFetchOrSearch(t *testing.T) {
	e := newIMAP(t)
	inbox := e.Srv.FolderID(testenv.User, "Inbox")
	hostile := [][]byte{nestedRFC822(8000), nestedMultipart(30000)}
	for i, raw := range hostile {
		d := e.Clock.Now().Add(-time.Duration(2-i) * time.Hour)
		e.Srv.AddMessage(testenv.User, inbox, raw, fakeupstream.AddOptions{Date: d})
	}
	e.ready("Inbox", "INBOX")
	r := e.rawDial()
	r.loginOK()
	r.ok(`SELECT INBOX`)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start := time.Now()
	lines := r.ok(`UID FETCH 1:2 (UID ENVELOPE BODYSTRUCTURE)`)
	all := joinLines(lines)
	// A message beyond the limits is one opaque part: still visible, still downloadable.
	if n := strings.Count(all, `("application" "octet-stream"`); n != 2 {
		t.Fatalf("want 2 flat structures, got %d in:\n%.2000s", n, all)
	}
	for _, subject := range []string{`"nested rfc822"`, `"nested multipart"`} {
		if !strings.Contains(all, subject) {
			t.Errorf("ENVELOPE lost %s", subject)
		}
	}
	// The body is downloadable as BODY[1]: it is the raw body after the header.
	for uid, raw := range hostile {
		got := r.ok(`UID FETCH %d (BODY.PEEK[1])`, uid+1)
		if body := joinLines(got); !strings.Contains(body, "needle-in-the-innermost-body") {
			t.Errorf("BODY[1] of message %d is empty or truncated (%d bytes of the response, message has %d)", uid+1, len(body), len(raw))
		}
	}
	// SEARCH TEXT and BODY walk the same bounded index.
	for _, crit := range []string{`SEARCH TEXT needle-in-the-innermost-body`, `SEARCH BODY needle-in-the-innermost-body`, `SEARCH HEADER Subject nested`} {
		r.ok("%s", crit)
	}
	runtime.ReadMemStats(&after)
	elapsed := time.Since(start)
	alloc := after.TotalAlloc - before.TotalAlloc
	t.Logf("hostile FETCH/SEARCH: %v, %d MB allocated", elapsed, alloc>>20)
	if elapsed > 5*time.Second {
		t.Errorf("took %v", elapsed)
	}
	if alloc > 256<<20 { // go-imap needed 2.8 GB for the first message alone
		t.Errorf("allocated %d MB", alloc>>20)
	}
}

func TestFetchRebuildsMissingOrDamagedStructure(t *testing.T) {
	// Messages stored before v0.1.2 have no stored index, and an index must never
	// be trusted blindly: FETCH builds a fresh one (within the same limits).
	e := newIMAP(t)
	inbox := e.Srv.FolderID(testenv.User, "Inbox")
	multi := "Message-ID: <multi@x>\r\nFrom: b@example.test\r\nTo: alice@example.test\r\nSubject: parts\r\nDate: Fri, 09 Oct 2026 12:00:00 +0000\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nContent-Type: text/plain\r\n\r\nhello\r\n--B\r\nContent-Type: application/pdf; name=\"a.pdf\"\r\n\r\n%PDF\r\n--B--\r\n"
	for i := 0; i < 4; i++ {
		d := e.Clock.Now().Add(-time.Duration(4-i) * time.Hour)
		e.Srv.AddMessage(testenv.User, inbox, []byte(multi), fakeupstream.AddOptions{Date: d})
	}
	e.ready("Inbox", "INBOX")
	acct := e.account()
	f, err := acct.Store().FolderByPath("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	// 1: no index. 2: garbage. 3: offsets beyond the message. 4: untouched.
	if err := acct.Store().SetStructure(f.ID, 1, nil); err != nil {
		t.Fatal(err)
	}
	if err := acct.Store().SetStructure(f.ID, 2, []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	if err := acct.Store().SetStructure(f.ID, 3, []byte(fmt.Sprintf(`{"v":%d,"env":{},"root":{"hs":0,"he":5,"be":99999999}}`, mimestruct.Version))); err != nil {
		t.Fatal(err)
	}
	r := e.rawDial()
	r.loginOK()
	r.ok(`SELECT INBOX`)
	for uid := 1; uid <= 4; uid++ {
		all := joinLines(r.ok(`UID FETCH %d (BODYSTRUCTURE BODY.PEEK[2] ENVELOPE)`, uid))
		if !strings.Contains(all, `("text" "plain"`) || !strings.Contains(all, `"mixed"`) || !strings.Contains(all, `"parts"`) {
			t.Errorf("UID %d: structure not rebuilt:\n%.1500s", uid, all)
		}
		if !strings.Contains(all, "%PDF") {
			t.Errorf("UID %d: BODY[2] not served", uid)
		}
	}
	if got := joinLines(r.ok(`SEARCH BODY hello`)); !strings.Contains(got, "* SEARCH 1 2 3 4") {
		t.Errorf("SEARCH BODY over rebuilt indexes: %s", got)
	}
}
