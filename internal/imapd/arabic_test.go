//go:build devfake

package imapd_test

import (
	"fmt"
	"mime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message/charset"

	"easbridge/internal/fakeupstream"
	"easbridge/internal/mailsamples"
	"easbridge/internal/mimestruct"
	"easbridge/internal/testenv"
)

// Arabic mail through the IMAP front end (real-Exchange finding 3): windows-1256,
// ISO-8859-6 and UTF-8, RFC 2047 B and Q, bodies in 8bit, base64 and
// quoted-printable. ENVELOPE carries the header text as written, SEARCH finds the
// decoded Arabic, sections are the original bytes.

type arabicMail struct {
	id           string
	subject      mailsamples.Word
	name         mailsamples.Word
	subjectWord  string // as written in the Subject header
	nameWord     string // as written in the display name of From
	body         mailsamples.BodyText
	cte          string
	raw          []byte
	uid          uint32
	wantBodyWire string // the body as it is on the wire
}

func arabicCorpus() []*arabicMail {
	var out []*arabicMail
	for i := range mailsamples.Subjects {
		for k, enc := range []string{"B", "Q"} {
			for _, cte := range []string{"8bit", "base64", "quoted-printable"} {
				subj, name, body := mailsamples.Subjects[i], mailsamples.Names[i], mailsamples.Bodies[i]
				m := &arabicMail{id: fmt.Sprintf("ar-%s-%s-%s@x", subj.Charset, enc, cte), subject: subj, name: name, body: body, cte: cte}
				m.subjectWord, m.nameWord = subj.B, name.Q
				if k == 1 {
					m.subjectWord, m.nameWord = subj.Q, name.B
				}
				m.raw = mailsamples.Mail(m.id, m.subjectWord, m.nameWord, body, cte)
				m.wantBodyWire = map[string]string{"8bit": body.Raw, "base64": body.Base64, "quoted-printable": body.QP}[cte]
				out = append(out, m)
			}
		}
	}
	return out
}

// dialArabic is dial with the client decoding envelope strings through go-message's charset table.
func (e *ienv) dialArabic() *imapclient.Client {
	e.T.Helper()
	c, err := imapclient.DialTLS(e.addr, &imapclient.Options{
		TLSConfig:   e.pki.ClientConfig("easbridge"),
		WordDecoder: &mime.WordDecoder{CharsetReader: charset.Reader},
	})
	if err != nil {
		e.T.Fatalf("dial: %v", err)
	}
	e.T.Cleanup(func() { c.Close() })
	if err := c.Login(testenv.User, testenv.Pass).Wait(); err != nil {
		e.T.Fatalf("login: %v", err)
	}
	return c
}

func arabicInbox(t *testing.T) (*ienv, []*arabicMail, *imapclient.Client) {
	e := newIMAP(t)
	inbox := e.Srv.FolderID(testenv.User, "Inbox")
	corpus := arabicCorpus()
	base := e.Clock.Now().Add(-3 * time.Hour)
	for i, m := range corpus {
		d := base.Add(time.Duration(i) * time.Minute)
		e.Srv.AddMessage(testenv.User, inbox, m.raw, fakeupstream.AddOptions{Date: d})
	}
	e.ready("Inbox", "INBOX")
	c := e.dialArabic()
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	// UIDs by Message-ID.
	for _, m := range corpus {
		d, err := c.UIDSearch(&imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Message-ID", Value: "<" + m.id + ">"}}}, nil).Wait()
		if err != nil || len(uidsOf(d)) != 1 {
			t.Fatalf("%s: %v %v", m.id, err, d)
		}
		m.uid = uidsOf(d)[0]
	}
	return e, corpus, c
}

func TestArabicEnvelopeIsTheHeaderTextAsWritten(t *testing.T) {
	e, corpus, c := arabicInbox(t)

	// The client decodes the words it is given: the Arabic text, in every charset and encoding.
	byUID := map[uint32]*arabicMail{}
	set := imap.UIDSet{}
	for _, m := range corpus {
		byUID[m.uid] = m
		set.AddNum(imap.UID(m.uid))
	}
	got, err := c.Fetch(set, &imap.FetchOptions{UID: true, Envelope: true}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(corpus) {
		t.Fatalf("%d envelopes for %d messages", len(got), len(corpus))
	}
	for _, g := range got {
		m := byUID[uint32(g.UID)]
		if g.Envelope.Subject != mailsamples.Subject || len(g.Envelope.From) != 1 || g.Envelope.From[0].Name != mailsamples.DisplayName {
			t.Errorf("%s: client sees subject %q, from %+v", m.id, g.Envelope.Subject, g.Envelope.From)
		}
	}

	// On the wire: the words as written, in the charset they were written in, as quoted strings.
	r := e.rawDial()
	r.loginOK()
	r.ok(`SELECT INBOX`)
	for _, m := range corpus {
		line := joinLines(r.ok(`UID FETCH %d (ENVELOPE)`, m.uid))
		if !strings.Contains(line, ` "`+m.subjectWord+`" `) {
			t.Errorf("%s: the subject is not on the wire as written:\n%s", m.id, line)
		}
		if !strings.Contains(line, `(("`+m.nameWord+`" NIL "sender" "example.test")`) {
			t.Errorf("%s: the display name is not on the wire as written:\n%s", m.id, line)
		}
		if strings.Contains(line, "{") && strings.Contains(strings.SplitN(line, "ENVELOPE", 2)[1], "{") {
			t.Errorf("%s: a literal in the ENVELOPE:\n%s", m.id, line)
		}
		if strings.Contains(line, "utf-8?q?") {
			t.Errorf("%s: the envelope was decoded and encoded again:\n%s", m.id, line)
		}
	}
}

func TestArabicSearch(t *testing.T) {
	_, corpus, c := arabicInbox(t)
	all := make([]uint32, 0, len(corpus))
	for _, m := range corpus {
		all = append(all, m.uid)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	search := func(crit *imap.SearchCriteria) []uint32 {
		t.Helper()
		d, err := c.UIDSearch(crit, nil).Wait()
		if err != nil {
			t.Fatalf("search %+v: %v", crit, err)
		}
		return uidsOf(d)
	}
	hdr := func(k, v string) *imap.SearchCriteria {
		return &imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: k, Value: v}}}
	}
	for name, tc := range map[string]struct {
		crit *imap.SearchCriteria
		want []uint32
	}{
		"SUBJECT words (windows-1256, ISO-8859-6 and UTF-8 messages alike)": {hdr("Subject", "الربع"), all},
		"SUBJECT the whole subject":                                         {hdr("Subject", mailsamples.Subject), all},
		"SUBJECT a word that is not there":                                  {hdr("Subject", "الخريف"), nil},
		"SUBJECT the encoded form is not the text":                          {hdr("Subject", "=?windows-1256?B?"), nil},
		"FROM the display name (header search)":                             {hdr("From", "العتيبي"), all},
		"FROM another name":                                                 {hdr("From", "الشمري"), nil},
		"FROM the address":                                                  {hdr("From", "sender@example"), all},
		"BODY a word of the body":                                           {&imap.SearchCriteria{Body: []string{"بالعالم"}}, all},
		"BODY a word that is not there":                                     {&imap.SearchCriteria{Body: []string{"غائب"}}, nil},
		"BODY does not read the headers":                                    {&imap.SearchCriteria{Body: []string{"الربع"}}, nil},
		"TEXT reads body and headers":                                       {&imap.SearchCriteria{Text: []string{"الربع"}}, all},
		"TEXT a word of the body":                                           {&imap.SearchCriteria{Text: []string{"اختبار"}}, all},
		"NOT SUBJECT":                                                       {&imap.SearchCriteria{Not: []imap.SearchCriteria{*hdr("Subject", "الربع")}}, nil},
	} {
		if got := search(tc.crit); !eq(got, tc.want) {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
	// One message among them, found by what only it has.
	one := corpus[7]
	if got := search(hdr("Message-ID", "<"+one.id+">")); !eq(got, []uint32{one.uid}) {
		t.Errorf("Message-ID: %v", got)
	}
}

func TestArabicBodyStructureAndSectionsAreTheOriginalBytes(t *testing.T) {
	_, corpus, c := arabicInbox(t)
	for _, m := range corpus {
		sec1 := &imap.FetchItemBodySection{Part: []int{1}, Peek: true}
		secText := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierText, Peek: true}
		secHdr := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"Subject"}, Peek: true}
		msgs, err := c.Fetch(imap.UIDSetNum(imap.UID(m.uid)), &imap.FetchOptions{
			UID: true, RFC822Size: true, BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
			BodySection: []*imap.FetchItemBodySection{sec1, secText, secHdr, {Peek: true}},
		}).Collect()
		if err != nil || len(msgs) != 1 {
			t.Fatalf("%s: %v", m.id, err)
		}
		g := msgs[0]
		bs, ok := g.BodyStructure.(*imap.BodyStructureSinglePart)
		if !ok || bs.Type != "text" || bs.Params["charset"] != m.body.Charset || bs.Encoding != m.cte {
			t.Errorf("%s: BODYSTRUCTURE %#v", m.id, g.BodyStructure)
		}
		if got := string(g.FindBodySection(sec1)); got != m.wantBodyWire {
			t.Errorf("%s: BODY[1] = %q, want %q", m.id, got, m.wantBodyWire)
		}
		if got := string(g.FindBodySection(secText)); got != m.wantBodyWire {
			t.Errorf("%s: BODY[TEXT] = %q", m.id, got)
		}
		if got, want := string(g.FindBodySection(secHdr)), "Subject: "+m.subjectWord+"\r\n\r\n"; got != want {
			t.Errorf("%s: BODY[HEADER.FIELDS (SUBJECT)] = %q, want %q", m.id, got, want)
		}
		if got := g.FindBodySection(&imap.FetchItemBodySection{Peek: true}); string(got) != string(m.raw) || int(g.RFC822Size) != len(m.raw) {
			t.Errorf("%s: BODY[] is not the message (%d bytes, size %d, want %d)", m.id, len(got), g.RFC822Size, len(m.raw))
		}
	}
}

func TestArabicLongSubjectInSeveralWordsAndHeadersInLegacyCharsets(t *testing.T) {
	e := newIMAP(t)
	inbox := e.Srv.FolderID(testenv.User, "Inbox")
	base := e.Clock.Now().Add(-3 * time.Hour)
	var long []*arabicMail
	for i, w := range mailsamples.LongSubjects {
		word := w.B + w.Q
		m := &arabicMail{id: fmt.Sprintf("long%d@x", i), subjectWord: word, nameWord: mailsamples.Names[0].Q, body: mailsamples.Bodies[0], cte: "8bit"}
		m.raw = mailsamples.Mail(m.id, word, m.nameWord, m.body, "8bit")
		e.Srv.AddMessage(testenv.User, inbox, m.raw, fakeupstream.AddOptions{Date: base.Add(time.Duration(i) * time.Minute)})
		long = append(long, m)
	}
	// Raw 8-bit headers in windows-1256 (no encoded-words), as old mailers wrote them: not valid UTF-8.
	legacy := []byte("Message-ID: <legacy@x>\r\nFrom: " + mailsamples.Names[0].Raw + " <sender@example.test>\r\nTo: alice@example.test\r\nSubject: " + mailsamples.Subjects[0].Raw +
		"\r\nDate: Thu, 01 Oct 2026 10:00:00 +0000\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=windows-1256\r\nContent-Transfer-Encoding: 8bit\r\n\r\n" + mailsamples.Bodies[0].Raw)
	e.Srv.AddMessage(testenv.User, inbox, legacy, fakeupstream.AddOptions{Date: base.Add(time.Hour)})
	e.ready("Inbox", "INBOX")

	c := e.dialArabic()
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	r := e.rawDial()
	r.loginOK()
	r.ok(`SELECT INBOX`)
	for _, m := range long {
		d, _ := c.UIDSearch(&imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Message-ID", Value: "<" + m.id + ">"}}}, nil).Wait()
		if len(uidsOf(d)) != 1 {
			t.Fatalf("%s not found", m.id)
		}
		uid := uidsOf(d)[0]
		line := joinLines(r.ok(`UID FETCH %d (ENVELOPE)`, uid))
		// The words, separated by one space, in a quoted string: no fold, no literal.
		if want := ` "` + strings.ReplaceAll(m.subjectWord, "\r\n ", " ") + `" `; !strings.Contains(line, want) {
			t.Errorf("%s: ENVELOPE lacks the subject as written:\n%s", m.id, line)
		}
		msgs, err := c.Fetch(imap.UIDSetNum(imap.UID(uid)), &imap.FetchOptions{Envelope: true}).Collect()
		if err != nil || msgs[0].Envelope.Subject != mailsamples.LongSubject {
			t.Errorf("%s: client sees %q (%v)", m.id, msgs[0].Envelope.Subject, err)
		}
		// SUBJECT finds a word from the middle of a subject that is cut over three encoded-words.
		if d, err := c.UIDSearch(&imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: "خطة الربع القادم"}}}, nil).Wait(); err != nil || len(uidsOf(d)) != len(long) {
			t.Errorf("SUBJECT over folded words: %v %v", d, err)
		}
	}

	// The legacy message is served and searchable by its body and its address; nothing breaks.
	d, _ := c.UIDSearch(&imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Message-ID", Value: "<legacy@x>"}}}, nil).Wait()
	if len(uidsOf(d)) != 1 {
		t.Fatal("legacy message missing")
	}
	uid := uidsOf(d)[0]
	line := joinLines(r.ok(`UID FETCH %d (ENVELOPE BODY.PEEK[1])`, uid))
	if !strings.Contains(line, `"sender" "example.test"`) {
		t.Errorf("the sender of a message with 8-bit headers is lost:\n%s", line)
	}
	if !strings.Contains(line, mailsamples.Bodies[0].Raw) {
		t.Errorf("BODY[1] of the legacy message differs:\n%q", line)
	}
	if d, err := c.UIDSearch(&imap.SearchCriteria{Body: []string{"بالعالم"}}, nil).Wait(); err != nil || len(uidsOf(d)) != len(long)+1 {
		t.Errorf("BODY over the windows-1256 bodies: %v %v", d, err)
	}
}

func TestAnIndexStoredByAnEarlierVersionIsRebuiltAndServesTheEnvelopeAsWritten(t *testing.T) {
	// Up to 0.1.6 the index held a decoded envelope. Such an index is of version 1; the version is 2 now, and a
	// message with the old one is served from a fresh index built from its bytes, not from the stored envelope.
	e, corpus, _ := arabicInbox(t)
	acct := e.account()
	f, err := acct.Store().FolderByPath("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	m := corpus[0]
	old := mimestruct.Build(m.raw)
	old.V = 1
	old.Env.Subject = "DECODED SUBJECT OF AN OLD INDEX"
	if err := acct.Store().SetStructure(f.ID, m.uid, old.Encode()); err != nil {
		t.Fatal(err)
	}
	r := e.rawDial()
	r.loginOK()
	r.ok(`SELECT INBOX`)
	for _, items := range []string{"ENVELOPE", "ENVELOPE BODYSTRUCTURE"} {
		line := joinLines(r.ok(`UID FETCH %d (%s)`, m.uid, items))
		if strings.Contains(line, "DECODED SUBJECT") || !strings.Contains(line, ` "`+m.subjectWord+`" `) {
			t.Errorf("FETCH %s served the stored envelope of an old index:\n%s", items, line)
		}
	}
}
