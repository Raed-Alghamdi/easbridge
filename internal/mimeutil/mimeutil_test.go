package mimeutil

import (
	"strings"
	"testing"
	"time"
)

func TestSplitHeader(t *testing.T) {
	cases := []struct {
		name, in, wantHdr, wantBody string
	}{
		{"crlf", "A: 1\r\nB: 2\r\n\r\nbody", "A: 1\r\nB: 2\r\n\r\n", "body"},
		{"lf", "A: 1\nB: 2\n\nbody", "A: 1\nB: 2\n\n", "body"},
		{"no body", "A: 1\r\n\r\n", "A: 1\r\n\r\n", ""},
		{"no blank line", "A: 1\r\nB: 2", "A: 1\r\nB: 2", ""},
		{"leading blank", "\r\nbody", "\r\n", "body"},
		{"folded", "A: 1\r\n 2\r\n\r\nx", "A: 1\r\n 2\r\n\r\n", "x"},
		{"empty", "", "", ""},
	}
	for _, tc := range cases {
		h, b := SplitHeader([]byte(tc.in))
		if string(h) != tc.wantHdr || string(b) != tc.wantBody {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", tc.name, h, b, tc.wantHdr, tc.wantBody)
		}
	}
}

func TestParseMeta(t *testing.T) {
	raw := "Message-ID: <abc.123@mail.example.test>\r\n" +
		"In-Reply-To: <parent@example.test>\r\n" +
		"References: <root@example.test>\r\n <parent@example.test>\r\n" +
		"Subject: =?UTF-8?B?2LTZg9ix2KfZiw==?= Re: test\r\n" +
		"Date: Tue, 06 Oct 2026 10:11:12 +0300\r\n" +
		"To: Zed <ZED@Example.test>, \"A B\" <a@example.test>\r\n" +
		"\r\nbody here"
	m := ParseMeta([]byte(raw))
	if m.MessageID != "abc.123@mail.example.test" {
		t.Errorf("MessageID = %q", m.MessageID)
	}
	if m.InReplyTo != "<parent@example.test>" || len(m.InReplyToIDs) != 1 || m.InReplyToIDs[0] != "parent@example.test" {
		t.Errorf("InReplyTo = %q %v", m.InReplyTo, m.InReplyToIDs)
	}
	if got := strings.Join(m.ReferenceIDs, "|"); got != "root@example.test|parent@example.test" {
		t.Errorf("ReferenceIDs = %q", got)
	}
	if m.References != "<root@example.test> <parent@example.test>" {
		t.Errorf("References = %q", m.References)
	}
	if !strings.HasPrefix(m.Subject, "شكرا") || !strings.HasSuffix(m.Subject, "Re: test") {
		t.Errorf("Subject = %q", m.Subject)
	}
	want := time.Date(2026, 10, 6, 7, 11, 12, 0, time.UTC)
	if !m.Date.Equal(want) {
		t.Errorf("Date = %v want %v", m.Date, want)
	}
	if m.To != "a@example.test,zed@example.test" {
		t.Errorf("To = %q", m.To)
	}
	if strings.Contains(string(m.Header), "body here") || !strings.HasSuffix(string(m.Header), "\r\n\r\n") {
		t.Errorf("Header = %q", m.Header)
	}
}

func TestParseMetaTolerant(t *testing.T) {
	m := ParseMeta([]byte("garbage line without colon\r\nMessage-ID: no-brackets@x\r\n\r\n"))
	if m.MessageID != "no-brackets@x" {
		t.Errorf("MessageID = %q", m.MessageID)
	}
	if m := ParseMeta(nil); m.MessageID != "" || len(m.Header) != 0 {
		t.Errorf("empty message: %+v", m)
	}
	if m := ParseMeta([]byte("Date: not a date\r\n\r\n")); !m.Date.IsZero() {
		t.Errorf("bad date should be zero")
	}
}

func TestNormalizeSubject(t *testing.T) {
	for in, want := range map[string]string{
		"Re: Re:  Hello   World": "hello world",
		"FWD: [x] hi":            "[x] hi",
		"Re[2]: topic":           "topic",
		"plain":                  "plain",
		"":                       "",
	} {
		if got := NormalizeSubject(in); got != want {
			t.Errorf("NormalizeSubject(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIds(t *testing.T) {
	got := Ids("<a@b> junk <c@d>  <bad id>")
	if len(got) != 2 || got[0] != "a@b" || got[1] != "c@d" {
		t.Errorf("Ids = %v", got)
	}
}

func TestAddEnvelopeRecipients(t *testing.T) {
	base := "From: a@x.test\r\nTo: Bob <bob@x.test>, carol@x.test\r\nCc: dan@x.test\r\nSubject: s\r\n\r\nbody\r\n"
	t.Run("nothing missing", func(t *testing.T) {
		out, added := AddEnvelopeRecipients([]byte(base), []string{"BOB@x.test", "carol@x.test", "dan@x.test"})
		if string(out) != base || len(added) != 0 {
			t.Fatalf("changed a complete message: %q %v", out, added)
		}
	})
	t.Run("bcc only recipients get a Bcc header", func(t *testing.T) {
		out, added := AddEnvelopeRecipients([]byte(base), []string{"bob@x.test", "eve@x.test", "Frank@x.test", "eve@x.test"})
		want := "From: a@x.test\r\nTo: Bob <bob@x.test>, carol@x.test\r\nCc: dan@x.test\r\nSubject: s\r\nBcc: eve@x.test, Frank@x.test\r\n\r\nbody\r\n"
		if string(out) != want || len(added) != 2 {
			t.Fatalf("got %q added %v", out, added)
		}
	})
	t.Run("existing Bcc is extended in place", func(t *testing.T) {
		msg := "From: a@x.test\r\nTo: bob@x.test\r\nBcc: old@x.test,\r\n  older@x.test\r\nSubject: s\r\n\r\nbody"
		out, added := AddEnvelopeRecipients([]byte(msg), []string{"bob@x.test", "old@x.test", "new@x.test"})
		want := "From: a@x.test\r\nTo: bob@x.test\r\nBcc: old@x.test, older@x.test, new@x.test\r\nSubject: s\r\n\r\nbody"
		if string(out) != want || len(added) != 1 || added[0] != "new@x.test" {
			t.Fatalf("got %q added %v", out, added)
		}
	})
	t.Run("empty Bcc field", func(t *testing.T) {
		msg := "To: bob@x.test\r\nBcc:\r\nSubject: s\r\n\r\nbody"
		out, _ := AddEnvelopeRecipients([]byte(msg), []string{"zed@x.test"})
		if want := "To: bob@x.test\r\nBcc: zed@x.test\r\nSubject: s\r\n\r\nbody"; string(out) != want {
			t.Fatalf("got %q", out)
		}
	})
	t.Run("LF line endings stay LF", func(t *testing.T) {
		out, _ := AddEnvelopeRecipients([]byte("To: bob@x.test\nSubject: s\n\nbody\n"), []string{"zed@x.test"})
		if want := "To: bob@x.test\nSubject: s\nBcc: zed@x.test\n\nbody\n"; string(out) != want {
			t.Fatalf("got %q", out)
		}
	})
	t.Run("body is never touched", func(t *testing.T) {
		body := "Bcc: not-a-header@x.test\r\n\r\n--\r\n"
		out, _ := AddEnvelopeRecipients([]byte("To: a@x.test\r\n\r\n"+body), []string{"z@x.test"})
		if !strings.HasSuffix(string(out), "\r\n\r\n"+body) {
			t.Fatalf("body changed: %q", out)
		}
	})
	t.Run("headers only message", func(t *testing.T) {
		out, _ := AddEnvelopeRecipients([]byte("To: a@x.test"), []string{"z@x.test"})
		if want := "To: a@x.test\r\nBcc: z@x.test\r\n\r\n"; string(out) != want {
			t.Fatalf("got %q", out)
		}
	})
	t.Run("unparsable To still counts the addresses it contains", func(t *testing.T) {
		msg := "To: undisclosed-recipients:;, bob@@x.test, ok@x.test\r\n\r\nbody"
		_, added := AddEnvelopeRecipients([]byte(msg), []string{"ok@x.test", "other@x.test"})
		if len(added) != 1 || added[0] != "other@x.test" {
			t.Fatalf("added %v", added)
		}
	})
}

// Mail is hostile input: a header of tens of megabytes or a References field of
// millions of ids must not become millions of database rows or seconds of CPU
// (security review of v0.1.0, "header parsing").
func TestParseMetaIsBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString("Message-ID: <real@example.test>\r\nSubject: kept\r\nReferences:")
	for line := 0; b.Len() < 6<<20; line++ { // a References field of 6 MB (about a million ids), folded as real ones are
		b.WriteString("\r\n")
		for k := 0; k < 100; k++ {
			b.WriteString(" <rxxx>")
		}
	}
	b.WriteString("\r\nIn-Reply-To:")
	for i := 0; i < 50000; i++ {
		b.WriteString(" <i>")
	}
	b.WriteString("\r\nX-Late: 1\r\n\r\nbody\r\n")
	raw := []byte(b.String())
	start := time.Now()
	m := ParseMeta(raw)
	took := time.Since(start)
	if took > 2*time.Second {
		t.Errorf("ParseMeta took %v for a %d MB header", took, len(raw)>>20)
	}
	if len(m.ReferenceIDs) > MaxReferenceIDs || len(m.InReplyToIDs) > MaxInReplyToIDs {
		t.Errorf("indexed %d references and %d in-reply-to ids (limits %d and %d)",
			len(m.ReferenceIDs), len(m.InReplyToIDs), MaxReferenceIDs, MaxInReplyToIDs)
	}
	if len(m.ReferenceIDs) != MaxReferenceIDs || m.MessageID != "real@example.test" || m.Subject != "kept" {
		t.Errorf("the beginning of the header must still be indexed: %d references, %q, %q", len(m.ReferenceIDs), m.MessageID, m.Subject)
	}
	if len(m.References) > MaxHeaderBytes {
		t.Errorf("References value has %d bytes", len(m.References))
	}
	// The whole header block is kept for BODY[HEADER].
	if len(m.Header) != len(raw)-len("body\r\n") {
		t.Errorf("stored header block is %d bytes of %d", len(m.Header), len(raw))
	}

	// A single line of megabytes (no folding at all) is not a real field: it is
	// dropped together with whatever follows the cut.
	huge := []byte("Message-ID: <a@x>\r\nReferences: " + strings.Repeat("<z> ", 2<<20) + "\r\n\r\n")
	start = time.Now()
	m = ParseMeta(huge)
	if time.Since(start) > 2*time.Second || len(m.ReferenceIDs) != 0 || m.MessageID != "a@x" {
		t.Errorf("single huge line: %v, %d ids, %q", time.Since(start), len(m.ReferenceIDs), m.MessageID)
	}
}
