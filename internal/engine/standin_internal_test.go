package engine

import (
	"bytes"
	"errors"
	"fmt"
	"mime"
	"net/mail"
	"strings"
	"testing"
	"time"

	"easbridge/internal/store"
	"easbridge/internal/upstream"
)

func parseStandIn(t *testing.T, p *store.PendingFetch) (*mail.Message, []byte) {
	t.Helper()
	raw := standInMIME(p)
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("not a message: %v\n%s", err, raw)
	}
	return msg, raw
}

func TestStandInShowsWhatExchangeListed(t *testing.T) {
	date := time.Date(2026, 10, 9, 8, 30, 15, 0, time.UTC)
	msg, raw := parseStandIn(t, &store.PendingFetch{
		ServerID: "5:77", InternalDate: date, Subject: "Quarterly numbers",
		From: "Carol Smith <carol@example.test>", To: "alice@example.test; Dave <dave@example.test>",
	})
	if got := msg.Header.Get("Subject"); got != "Quarterly numbers" {
		t.Errorf("subject = %q", got)
	}
	if from, _ := msg.Header.AddressList("From"); len(from) != 1 || from[0].Address != "carol@example.test" || from[0].Name != "Carol Smith" {
		t.Errorf("from = %v", from)
	}
	to, _ := msg.Header.AddressList("To")
	if len(to) != 2 || to[0].Address != "alice@example.test" || to[1].Address != "dave@example.test" {
		t.Errorf("to = %v", to)
	}
	if d, err := msg.Header.Date(); err != nil || !d.Equal(date) {
		t.Errorf("date = %v %v", d, err)
	}
	id := msg.Header.Get("Message-Id")
	if !strings.HasPrefix(id, "<standin-") || !strings.HasSuffix(id, "@easbridge.invalid>") {
		t.Errorf("message-id = %q", id)
	}
	other, _ := parseStandIn(t, &store.PendingFetch{ServerID: "5:78", InternalDate: date})
	if other.Header.Get("Message-Id") == id {
		t.Error("two items share a Message-ID")
	}
	again, _ := parseStandIn(t, &store.PendingFetch{ServerID: "5:77", InternalDate: date})
	if again.Header.Get("Message-Id") != id {
		t.Error("the Message-ID of an item must be stable")
	}
	if len(raw) > 1500 {
		t.Errorf("a stand-in is small, this one has %d bytes", len(raw))
	}
	if !bytes.Contains(raw, []byte("\r\n\r\nThis message could not be downloaded from Exchange.")) {
		t.Errorf("body:\n%s", raw)
	}
	// 7-bit, all of it.
	for _, c := range raw {
		if c > 0x7e || (c < 0x20 && c != '\r' && c != '\n') {
			t.Fatalf("byte 0x%02x in the stand-in", c)
		}
	}
}

func TestStandInIsSafeAgainstWhatExchangeSends(t *testing.T) {
	date := time.Date(2026, 10, 9, 8, 30, 15, 0, time.UTC)
	long := strings.Repeat("x", 1000)
	for _, tc := range []struct {
		name string
		p    store.PendingFetch
	}{
		{"line breaks in the subject", store.PendingFetch{Subject: "Hi\r\nBcc: attacker@example.test\r\nX-Evil: 1"}},
		{"line breaks in the sender", store.PendingFetch{From: "Eve <eve@example.test>\r\nBcc: attacker@example.test"}},
		{"line breaks in the recipients", store.PendingFetch{To: "a@example.test;\r\nBcc: attacker@example.test"}},
		{"control characters and NULs", store.PendingFetch{Subject: "a\x00b\x01c\x1bd\u2028e\ufeff", From: "\x00\x00", To: "\x7f"}},
		{"invalid UTF-8", store.PendingFetch{Subject: "caf\xe9 \xff\xfe", From: "\xc3\x28 <a@example.test>"}},
		{"very long fields", store.PendingFetch{Subject: long, From: long + " <a@example.test>", To: strings.Repeat("a@example.test;", 500)}},
		{"nothing at all", store.PendingFetch{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.p.ServerID, tc.p.InternalDate = "5:1", date
			msg, raw := parseStandIn(t, &tc.p)
			for k := range msg.Header {
				switch k {
				case "Message-Id", "Date", "From", "To", "Subject", "Mime-Version", "Content-Type", "Content-Transfer-Encoding", "X-Easbridge-Stand-In":
				default:
					t.Errorf("header %q injected", k)
				}
			}
			if len(msg.Header["Bcc"]) != 0 || bytes.Contains(raw, []byte("attacker")) && !strings.Contains(tc.name, "line breaks") {
				t.Errorf("injected content:\n%s", raw)
			}
			if _, err := msg.Header.AddressList("From"); err != nil {
				t.Errorf("From is not an address list: %v", err)
			}
			if to := msg.Header.Get("To"); to != "" {
				if _, err := msg.Header.AddressList("To"); err != nil {
					t.Errorf("To is not an address list: %v", err)
				}
			}
			subj, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
			if err != nil || subj == "" || len([]rune(subj)) > 200 {
				t.Errorf("subject %q (%d runes) %v", subj, len([]rune(subj)), err)
			}
			for _, line := range bytes.Split(raw, []byte("\r\n")) {
				if len(line) > 998 {
					t.Errorf("a line of %d bytes", len(line))
				}
			}
		})
	}
	// Nothing known: a placeholder subject and an address that cannot exist.
	msg, _ := parseStandIn(t, &store.PendingFetch{ServerID: "5:1", InternalDate: date})
	if msg.Header.Get("Subject") != "(no subject)" {
		t.Errorf("subject = %q", msg.Header.Get("Subject"))
	}
	if from, _ := msg.Header.AddressList("From"); len(from) != 1 || from[0].Address != "unknown@easbridge.invalid" {
		t.Errorf("from = %v", from)
	}
	if msg.Header.Get("To") != "" {
		t.Errorf("a To header without recipients: %q", msg.Header.Get("To"))
	}
}

func TestStandInKeepsNonASCIIText(t *testing.T) {
	msg, _ := parseStandIn(t, &store.PendingFetch{ServerID: "5:1", InternalDate: time.Now(), Subject: "تقرير ربع سنوي", From: "علي <ali@example.test>"})
	subj, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil || subj != "تقرير ربع سنوي" {
		t.Errorf("subject = %q %v (raw %q)", subj, err, msg.Header.Get("Subject"))
	}
	from, err := msg.Header.AddressList("From")
	if err != nil || len(from) != 1 || from[0].Name != "علي" || from[0].Address != "ali@example.test" {
		t.Errorf("from = %v %v", from, err)
	}
}

// Which failed downloads count against the message (five of them stop its folder
// from waiting for it, ten make a final stand-in).
func TestOnlyFailuresAboutTheItemCountAgainstIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"no body", upstream.ErrNoMIME, true},
		{"a permanent status of the item", &upstream.StatusError{Op: "Fetch", Code: 8, Perm: true}, true},
		{"wrapped", fmt.Errorf("fetch: %w", &upstream.StatusError{Op: "Fetch", Code: 6, Perm: true}), true},
		{"a transient status", &upstream.StatusError{Op: "Fetch", Code: 110}, false},
		{"a permanent status about the device (129 blocked)", &upstream.StatusError{Op: "Fetch", Code: 129, Perm: true, Global: true}, false},
		{"a permanent status about the policy (141)", &upstream.StatusError{Op: "Fetch", Code: 141, Perm: true, Global: true}, false},
		{"a disabled account (131)", fmt.Errorf("fetch: %w", &upstream.StatusError{Op: "Fetch", Code: 131, Perm: true, Global: true}), false},
		{"the network", upstream.ErrUnavailable, false},
		{"something else", errors.New("boom"), false},
	} {
		if got := itemSpecific(tc.err); got != tc.want {
			t.Errorf("%s: itemSpecific = %v, want %v", tc.name, got, tc.want)
		}
	}
}
