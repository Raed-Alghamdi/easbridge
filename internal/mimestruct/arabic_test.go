package mimestruct

import (
	"io"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	gomessage "github.com/emersion/go-message"
	"github.com/emersion/go-message/textproto"

	"easbridge/internal/mailsamples"
	"easbridge/internal/mimeutil"
)

// Arabic mail (real-Exchange finding 3): windows-1256, ISO-8859-6 and UTF-8 in
// RFC 2047 words, B and Q, and in bodies with every transfer encoding. The samples
// come from Python's codecs.

// unfold is what a header value is once the folds are gone.
func unfold(s string) string { return strings.ReplaceAll(s, "\r\n ", " ") }

func TestEnvelopeHoldsTheHeaderTextAsWritten(t *testing.T) {
	// RFC 3501 section 7.4.2: the fields of an ENVELOPE are the contents of the headers. The
	// encoded-words stay encoded, in the charset they were written in; the client decodes them.
	for i, subj := range mailsamples.Subjects {
		name := mailsamples.Names[i]
		for _, tc := range []struct{ subject, from string }{{subj.B, name.Q}, {subj.Q, name.B}} {
			raw := mailsamples.Mail("a@x", tc.subject, tc.from, mailsamples.Bodies[i], "8bit")
			ix := Build(raw)
			if ix.Env.Subject != tc.subject {
				t.Errorf("%s: Subject = %q, want it as written, %q", subj.Charset, ix.Env.Subject, tc.subject)
			}
			if len(ix.Env.From) != 1 || ix.Env.From[0].Name != tc.from || ix.Env.From[0].Mailbox != "sender" || ix.Env.From[0].Host != "example.test" {
				t.Errorf("%s: From = %+v, want the name as written, %q", subj.Charset, ix.Env.From, tc.from)
			}
			// What the client does with it: the Arabic text.
			if mimeutil.DecodeHeader(ix.Env.Subject) != subj.Text || mimeutil.DecodeHeader(ix.Env.From[0].Name) != name.Text {
				t.Errorf("%s: the client cannot read the envelope back", subj.Charset)
			}
			// And the same envelope comes out of the stored form.
			dec, err := Decode(ix.Encode())
			if err != nil || dec.Env.Subject != tc.subject || dec.Env.From[0].Name != tc.from {
				t.Errorf("%s: stored envelope: %+v %v", subj.Charset, dec.Env, err)
			}
			// go-imap decoded these; what the client makes of ours is what go-imap would have shown.
			th, _ := textproto.ReadHeader(bufioReader(raw))
			want := imapserver.ExtractEnvelope(th)
			if want.Subject != subj.Text || want.From[0].Name != name.Text {
				t.Errorf("%s: go-imap's own envelope: %q %q", subj.Charset, want.Subject, want.From[0].Name)
			}
		}
	}
}

func TestEnvelopeSubjectOfSeveralFoldedWordsIsUnfoldedOnly(t *testing.T) {
	for _, w := range mailsamples.LongSubjects {
		word := w.B + w.Q
		raw := mailsamples.Mail("a@x", word, mailsamples.Names[0].Q, mailsamples.Bodies[0], "8bit")
		ix := Build(raw)
		if want := unfold(word); ix.Env.Subject != want {
			t.Errorf("%s: Subject = %q, want %q", w.Charset, ix.Env.Subject, want)
		}
		if strings.ContainsAny(ix.Env.Subject, "\r\n") {
			t.Errorf("%s: a fold is left in the subject", w.Charset)
		}
		if got := mimeutil.DecodeHeader(ix.Env.Subject); got != w.Text {
			t.Errorf("%s: decoded = %q, want %q", w.Charset, got, w.Text)
		}
		// The ENVELOPE as it goes out: no CRLF, so a quoted string and not a literal.
		if got := ix.Env.IMAP().Subject; got != unfold(word) {
			t.Errorf("%s: IMAP() subject = %q", w.Charset, got)
		}
	}
}

func TestEnvelopeAddressesSurviveANameInACharsetNobodyListed(t *testing.T) {
	// go-message's AddressList fails the whole list for a charset it has no reader for, and the
	// ENVELOPE then had no From, To or Cc at all.
	raw := []byte("From: =?x-no-such-charset?B?aGVsbG8=?= <a@example.test>\r\nTo: =?windows-1256?Q?=C7=E1?= <b@example.test>, \"Smith, John\" <c@example.test>, d@example.test\r\n" +
		"Cc: =?utf-8*ar?B?2KfZhA==?= <e@example.test>\r\nReply-To: <f@example.test>\r\nSubject: s\r\n\r\nbody")
	env := Build(raw).Env
	if len(env.From) != 1 || env.From[0].Name != "=?x-no-such-charset?B?aGVsbG8=?=" || env.From[0].Mailbox != "a" {
		t.Errorf("From = %+v", env.From)
	}
	if len(env.To) != 3 || env.To[0].Name != "=?windows-1256?Q?=C7=E1?=" || env.To[1].Name != "Smith, John" || env.To[2].Name != "" || env.To[2].Mailbox != "d" {
		t.Errorf("To = %+v", env.To)
	}
	if len(env.Cc) != 1 || env.Cc[0].Name != "=?utf-8*ar?B?2KfZhA==?=" || len(env.ReplyTo) != 1 || env.ReplyTo[0].Mailbox != "f" {
		t.Errorf("Cc = %+v, ReplyTo = %+v", env.Cc, env.ReplyTo)
	}
}

func TestEnvelopeOfEmbeddedMessageIsRawToo(t *testing.T) {
	w := mailsamples.Subjects[0]
	inner := string(mailsamples.Mail("inner@x", w.Q, mailsamples.Names[1].B, mailsamples.Bodies[0], "8bit"))
	raw := []byte("Message-ID: <outer@x>\r\nSubject: " + mailsamples.Subjects[2].B + "\r\nContent-Type: message/rfc822\r\n\r\n" + inner)
	ix := Build(raw)
	bs := ix.BodyStructure().(*imap.BodyStructureSinglePart)
	if bs.MessageRFC822 == nil || bs.MessageRFC822.Envelope.Subject != w.Q || ix.Env.Subject != mailsamples.Subjects[2].B {
		t.Fatalf("outer %q, inner %+v", ix.Env.Subject, bs.MessageRFC822)
	}
	if got := bs.MessageRFC822.Envelope.From[0].Name; got != mailsamples.Names[1].B {
		t.Errorf("inner From name = %q", got)
	}
}

func TestBodyStructureAndSectionsOfArabicParts(t *testing.T) {
	for i, body := range mailsamples.Bodies {
		for _, cte := range []string{"8bit", "base64", "quoted-printable"} {
			raw := mailsamples.Mail("a@x", mailsamples.Subjects[i].B, mailsamples.Names[i].Q, body, cte)
			ix := Build(raw)
			bs, ok := ix.BodyStructure().(*imap.BodyStructureSinglePart)
			if !ok || bs.Type != "text" || bs.Subtype != "plain" || bs.Params["charset"] != body.Charset || bs.Encoding != cte {
				t.Fatalf("%s %s: %#v", body.Charset, cte, ix.BodyStructure())
			}
			wantBody := map[string]string{"8bit": body.Raw, "base64": body.Base64, "quoted-printable": body.QP}[cte]
			if int(bs.Size) != len(wantBody) || bs.Text == nil || bs.Text.NumLines != int64(strings.Count(wantBody, "\n")) {
				t.Errorf("%s %s: size %d (want %d), text %+v", body.Charset, cte, bs.Size, len(wantBody), bs.Text)
			}
			// BODY[1], BODY[TEXT]: the bytes of the message, whatever the charset.
			for _, it := range []*imap.FetchItemBodySection{{Part: []int{1}}, {Specifier: imap.PartSpecifierText}} {
				if got := Section(raw, ix, it); string(got) != wantBody {
					t.Errorf("%s %s: section %v = %q, want %q", body.Charset, cte, it.Specifier, got, wantBody)
				}
			}
			// BODY[HEADER.FIELDS (SUBJECT)]: the subject line as written.
			got := Section(raw, ix, &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"Subject"}})
			if want := "Subject: " + mailsamples.Subjects[i].B + "\r\n\r\n"; string(got) != want {
				t.Errorf("%s: BODY[HEADER.FIELDS (SUBJECT)] = %q, want %q", body.Charset, got, want)
			}
		}
	}
}

func TestMatchTextFindsArabicInEveryCharsetAndTransferEncoding(t *testing.T) {
	needle := "بالعالم"  // in the body
	other := "غير موجود" // not in it
	for i, body := range mailsamples.Bodies {
		for _, cte := range []string{"8bit", "base64", "quoted-printable"} {
			subj := mailsamples.Subjects[i]
			raw := mailsamples.Mail("a@x", subj.B, mailsamples.Names[i].Q, body, cte)
			ix := Build(raw)
			for _, header := range []bool{false, true} {
				if !MatchText(raw, ix, needle, header) {
					t.Errorf("%s %s (header=%v): the body text is not found", body.Charset, cte, header)
				}
				if MatchText(raw, ix, other, header) {
					t.Errorf("%s %s (header=%v): found text that is not there", body.Charset, cte, header)
				}
			}
			// TEXT also reads the decoded header values; BODY does not.
			if !MatchText(raw, ix, "الربع", true) || !MatchText(raw, ix, "العتيبي", true) {
				t.Errorf("%s %s: TEXT does not find the words of the subject and the name", body.Charset, cte)
			}
			if MatchText(raw, ix, "الربع", false) {
				t.Errorf("%s %s: BODY looked into the headers", body.Charset, cte)
			}
		}
	}
}

func TestCharsetTableIsRegistered(t *testing.T) {
	// MatchText reads text parts through go-message, which converts by its registered table
	// (package charset); the registration is a side effect of an import, so it is checked here.
	if gomessage.CharsetReader == nil {
		t.Fatal("go-message has no charset reader: the charset package is not imported")
	}
	for _, cs := range []string{"windows-1256", "WINDOWS-1256", "iso-8859-6", "ISO-8859-6", "cp1256", "arabic", "ASMO-708", "utf-8"} {
		r, err := gomessage.CharsetReader(cs, strings.NewReader("\xc7"))
		if err != nil || r == nil {
			t.Errorf("charset %q: %v", cs, err)
		}
	}
	// An entity in windows-1256 reads as UTF-8.
	b := mailsamples.Bodies[0]
	h := gomessage.Header{}
	h.Set("Content-Type", "text/plain; charset=windows-1256")
	e, err := gomessage.New(h, strings.NewReader(b.Raw))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(e.Body); string(got) != b.Text {
		t.Errorf("a windows-1256 entity reads as %q, want %q", got, b.Text)
	}
}

func TestSearchOverAnArabicMultipartMessage(t *testing.T) {
	// A multipart/alternative message whose text parts are in different charsets.
	u8, w, iso := mailsamples.Bodies[2], mailsamples.Bodies[0], mailsamples.Bodies[1]
	raw := []byte("Message-ID: <m@x>\r\nSubject: " + mailsamples.Subjects[1].Q + "\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain; charset=" + w.Charset + "\r\nContent-Transfer-Encoding: 8bit\r\n\r\n" + w.Raw + "\r\n" +
		"--B\r\nContent-Type: text/plain; charset=" + strings.ToUpper(iso.Charset) + "\r\nContent-Transfer-Encoding: base64\r\n\r\n" + iso.Base64 + "\r\n" +
		"--B\r\nContent-Type: text/html; charset=" + u8.Charset + "\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n<p>" + u8.QP + "</p>\r\n--B--\r\n")
	ix := Build(raw)
	if len(ix.Root.Kids) != 3 {
		t.Fatalf("%d parts", len(ix.Root.Kids))
	}
	if !MatchText(raw, ix, "اختبار", false) || MatchText(raw, ix, "ليس هنا", false) {
		t.Error("BODY over parts in three charsets")
	}
	if !MatchText(raw, ix, "تقرير", true) {
		t.Error("TEXT does not read the subject")
	}
}
