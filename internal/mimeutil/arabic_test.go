package mimeutil

import (
	"fmt"
	"strings"
	"testing"

	"easbridge/internal/mailsamples"
)

// Arabic mail comes in windows-1256, ISO-8859-6 and UTF-8, RFC 2047 B- or Q-encoded
// (real-Exchange finding 3: the mailbox has items with InternetCPID 1256, 28596 and
// 65001). The samples were produced with Python's codecs.

func TestDecodeHeaderArabicInEveryCharsetAndEncoding(t *testing.T) {
	for _, set := range [][]mailsamples.Word{mailsamples.Subjects, mailsamples.Names} {
		for _, w := range set {
			for kind, word := range map[string]string{"B": w.B, "Q": w.Q} {
				t.Run(w.Charset+" "+kind, func(t *testing.T) {
					if got := DecodeHeader(word); got != w.Text {
						t.Errorf("DecodeHeader(%q) = %q, want %q", word, got, w.Text)
					}
					// The charset in any case, with white space around the word and text next to it.
					upper := strings.ToUpper(w.Charset)
					variant := strings.Replace(word, w.Charset, upper, 1)
					if got := DecodeHeader("  Re: " + variant + " (draft)"); got != "  Re: "+w.Text+" (draft)" {
						t.Errorf("with the charset in capitals and text around: %q", got)
					}
				})
			}
		}
	}
}

func TestDecodeHeaderOfAHeaderOfSeveralWords(t *testing.T) {
	for _, w := range mailsamples.LongSubjects {
		word := w.B + w.Q // one of the two is set
		if got := DecodeHeader(word); got != w.Text {
			t.Errorf("%s %q...: got %q, want %q", w.Charset, word[:40], got, w.Text)
		}
	}
	// A UTF-8 character cut between two words is whole again.
	if got := DecodeHeader(mailsamples.SplitUTF8); got != mailsamples.Subject {
		t.Errorf("a character cut between two words: %q, want %q", got, mailsamples.Subject)
	}
	// The white space between two encoded-words is not text, the white space around text is.
	b := mailsamples.Subjects[0].B
	if got := DecodeHeader(b + "  \t " + b); got != mailsamples.Subject+mailsamples.Subject {
		t.Errorf("two words: %q", got)
	}
	if got := DecodeHeader(b + " and " + b); got != mailsamples.Subject+" and "+mailsamples.Subject {
		t.Errorf("two words with text between: %q", got)
	}
}

func TestDecodeHeaderReadsWhatRealMailDoes(t *testing.T) {
	arabic := "اللغة"
	for in, want := range map[string]string{
		// A language after the charset (RFC 2231 section 5): the standard library gives up on the whole value.
		"=?UTF-8*ar?B?2KfZhNmE2LrYqQ==?=":                  arabic,
		"=?utf-8*ar-SA?Q?=D8=A7=D9=84=D9=84=D8=BA=D8=A9?=": arabic,
		// Base64 without its padding, or with too much.
		"=?utf-8?B?2KfZhNmE2LrYqQ?=":     arabic,
		"=?utf-8?B?2KfZhNmE2LrYqQ====?=": arabic,
		// Q text: "_" is a space, a "=" that starts no byte stays, lower-case hex works.
		"=?utf-8?q?a_b_=d8=a7?=":             "a b ا",
		"=?utf-8?Q?100%=_off?=":              "100%= off",
		"=?windows-1256?q?=c7=e1=da=d1=c8?=": "العرب",
		// Encoded-words are found anywhere in the value; a word that is not one is left alone.
		"x=?utf-8?q?y?=z": "xyz",
		"=?utf-8?q?y":     "=?utf-8?q?y",
		"=?utf-8?x?y?=":   "=?utf-8?x?y?=",
		"a =? b ?= c":     "a =? b ?= c",
		"":                "",
		"plain text":      "plain text",
	} {
		if got := DecodeHeader(in); got != want {
			t.Errorf("DecodeHeader(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDecodeHeaderLeavesAWordItCannotDecodeAndStillDecodesTheOthers(t *testing.T) {
	good := mailsamples.Subjects[0].B
	for name, bad := range map[string]string{
		"an unknown charset": "=?x-no-such-charset?B?aGVsbG8=?=",
		"broken base64":      "=?utf-8?B?!!!!?=",
		"an empty charset":   "=??B?aGVsbG8=?=",
	} {
		got := DecodeHeader(good + " " + bad + " " + good)
		if want := mailsamples.Subject + " " + bad + " " + mailsamples.Subject; got != want {
			t.Errorf("%s: got %q, want %q", name, got, want)
		}
	}
	// What is not UTF-8 is cleaned, in an encoded word and outside.
	if got := DecodeHeader("caf\xe9 =?utf-8?Q?=E9?="); strings.ContainsRune(got, 0xe9) || !strings.HasPrefix(got, "caf\uFFFD") || !strings.HasSuffix(got, "\uFFFD") {
		t.Errorf("invalid UTF-8: %q", got)
	}
	if got := DecodeHeader("caf\xe9"); got != "caf\uFFFD" {
		t.Errorf("invalid UTF-8 without a word: %q", got)
	}
}

func TestDecodeHeaderIsLinear(t *testing.T) {
	// A header of 128 KiB full of encoded-words, and one full of unfinished ones.
	w := mailsamples.Subjects[0].B
	if got := DecodeHeader(strings.Repeat(w+" ", 128<<10/len(w))); !strings.HasPrefix(got, mailsamples.Subject) {
		t.Errorf("many words: %.40q", got)
	}
	DecodeHeader(strings.Repeat("=?utf-8?B?", 128<<10/10))
	DecodeHeader(strings.Repeat("=?", 64<<10))
}

func TestParseAddrListKeepsTheNamesAsWritten(t *testing.T) {
	q := mailsamples.Names[0].Q // windows-1256, a charset net/mail has no reader for
	b := mailsamples.Names[2].B
	for _, tc := range []struct {
		name, in string
		want     []Addr
	}{
		{"a Q word in windows-1256", q + " <a@example.test>", []Addr{{q, "a@example.test"}}},
		{"a B word in UTF-8", b + " <a@example.test>", []Addr{{b, "a@example.test"}}},
		{"several", q + " <a@example.test>, \"Smith, John\" <j@example.test>, plain@example.test, " + b + " <b@example.test>",
			[]Addr{{q, "a@example.test"}, {"Smith, John", "j@example.test"}, {"", "plain@example.test"}, {b, "b@example.test"}}},
		{"two words in a phrase keep both", q + " " + b + " <a@example.test>", []Addr{{q + " " + b, "a@example.test"}}},
		{"a word in a quoted string", "\"" + q + "\" <a@example.test>", []Addr{{q, "a@example.test"}}},
		{"text and a word", "Dr. " + q + " <a@example.test>", []Addr{{"Dr. " + q, "a@example.test"}}},
		{"a word in a comment", "a@example.test (" + q + ")", []Addr{{q, "a@example.test"}}},
		{"UTF-8 written as it is", "محمد <a@example.test>", []Addr{{"محمد", "a@example.test"}}},
		{"an unknown charset does not fail the list", "=?x-no-such-charset?B?aGVsbG8=?= <a@example.test>, b@example.test",
			[]Addr{{"=?x-no-such-charset?B?aGVsbG8=?=", "a@example.test"}, {"", "b@example.test"}}},
		{"a group", "undisclosed-recipients:;", []Addr{}},
		{"empty", "  ", nil},
	} {
		got, err := ParseAddrList(tc.in)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%s:\n got  %v\n want %v", tc.name, got, tc.want)
		}
	}
	// Bytes that are not UTF-8 (a header in a legacy charset) do not lose the address.
	got, err := ParseAddrList("\xc7\xe1\xda\xd1\xc8 <a@example.test>")
	if err != nil || len(got) != 1 || got[0].Address != "a@example.test" || !strings.Contains(got[0].Name, "\uFFFD") {
		t.Errorf("8-bit name: %v %v", got, err)
	}
	// A value that is not an address list is net/mail's error, as before.
	if _, err := ParseAddrList("not an address <"); err == nil {
		t.Error("a broken list was accepted")
	}
}

func TestParseAddrListCannotBeFooledByAPlaceholder(t *testing.T) {
	// The words are swapped for atoms while the list is parsed; text that looks like such an atom must
	// stay what it is.
	q := mailsamples.Names[0].Q
	got, err := ParseAddrList("ewph0z " + q + " <a@example.test>, ewph0z <b@example.test>")
	if err != nil || len(got) != 2 || got[0].Name != "ewph0z "+q || got[1].Name != "ewph0z" {
		t.Errorf("%v %v", got, err)
	}
	// Thousands of words.
	var parts []string
	for i := 0; i < 3000; i++ {
		parts = append(parts, fmt.Sprintf("%s <u%d@example.test>", q, i))
	}
	got, err = ParseAddrList(strings.Join(parts, ", "))
	if err != nil || len(got) != 3000 || got[2999].Name != q || got[2999].Address != "u2999@example.test" {
		t.Errorf("3000 addresses: %d %v", len(got), err)
	}
}

func TestNormalizeAddrListSurvivesAnyCharset(t *testing.T) {
	// net/mail refuses a display name in windows-1256 and the whole value became its lower-cased text: two
	// copies of one message then differed in "To" and were never bound (Sent de-duplication).
	for _, in := range []string{
		mailsamples.Names[0].Q + " <B@Example.test>, " + mailsamples.Names[1].B + " <a@example.test>",
		"<a@example.test>, \"" + mailsamples.Names[2].B + "\" <b@EXAMPLE.test>",
		"=?x-no-such-charset?B?aGVsbG8=?= <a@example.test>, b@example.test",
	} {
		if got := NormalizeAddrList(in); got != "a@example.test,b@example.test" {
			t.Errorf("NormalizeAddrList(%q) = %q", in, got)
		}
	}
}

func TestParseMetaDecodesArabicSubjectsInEveryCharsetAndEncoding(t *testing.T) {
	for _, w := range append(append([]mailsamples.Word{}, mailsamples.Subjects...), mailsamples.LongSubjects...) {
		for _, enc := range []string{w.B, w.Q} {
			if enc == "" {
				continue
			}
			msg := mailsamples.Mail("m@x", enc, mailsamples.Names[0].Q, mailsamples.Bodies[0], "8bit")
			m := ParseMeta(msg)
			if m.Subject != w.Text {
				t.Errorf("%s: Subject = %q, want %q", w.Charset, m.Subject, w.Text)
			}
			if m.To != "alice@example.test" || m.MessageID != "m@x" {
				t.Errorf("%s: To %q, Message-ID %q", w.Charset, m.To, m.MessageID)
			}
		}
	}
	// The same subject written in all of them is one subject, with or without a prefix.
	var norm []string
	for _, w := range mailsamples.Subjects {
		norm = append(norm, NormalizeSubject(w.B), NormalizeSubject("Re: "+w.Q), NormalizeSubject("FWD:  Re: "+w.Text))
	}
	for _, n := range norm {
		if n != norm[0] {
			t.Errorf("normalised subjects differ: %q and %q", norm[0], n)
		}
	}
	if norm[0] != mailsamples.Subject {
		t.Errorf("normalised subject = %q", norm[0])
	}
}

func TestAddEnvelopeRecipientsReadsArabicNamesInAnyCharset(t *testing.T) {
	// net/mail could not read a list with a name in windows-1256 and the code fell back to scanning the
	// text for "@": a display name that looks like an address then counted as a recipient, and the real
	// Bcc recipient with that address was not added to the headers (EAS sends to the headers only).
	msg := []byte("From: a@example.test\r\nTo: " + mailsamples.Names[0].Q + " <Bob@Example.test>, \"dave@example.test\" <eve@example.test>\r\n" +
		"Cc: " + mailsamples.Names[1].B + " <carol@example.test>\r\nSubject: s\r\n\r\nbody")
	out, added := AddEnvelopeRecipients(msg, []string{"bob@example.test", "CAROL@example.test", "dave@example.test", "eve@example.test"})
	if len(added) != 1 || added[0] != "dave@example.test" || !strings.Contains(string(out), "Bcc: dave@example.test\r\n") {
		t.Errorf("added %v\n%s", added, out)
	}
}
