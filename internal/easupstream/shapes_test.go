package easupstream_test

// The shapes of the first real Exchange 2019 answers (real-Exchange finding 3, v0.1.7):
// an ItemOperations Fetch whose Properties hold the MIME of a Type 4 body as an inline
// string, and Sync Add items with attachments, a plain-text body cut to one byte, binary
// conversation ids and empty Flag and Categories elements. The values are synthetic.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hstern/go-activesync/wbxml"

	"easbridge/internal/upstream"
)

// arabicMIME is a message with an Arabic subject and body in windows-1256, raw 8-bit
// (the bytes are not UTF-8), padded to size bytes.
func arabicMIME(size int) string {
	head := "Message-ID: <arabic-cp1256@example.test>\r\nFrom: =?windows-1256?Q?=C7=E1=CD=D3=C7=C8?= <sender@example.test>\r\n" +
		"To: alice@example.test\r\nSubject: =?windows-1256?B?x+Ha0cjtyQ==?=\r\nDate: Thu, 01 Oct 2026 10:00:00 +0000\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: text/plain; charset=windows-1256\r\nContent-Transfer-Encoding: 8bit\r\n\r\n"
	body := "\xc7\xe1\xda\xd1\xc8\xed\xc9 \xc7\xe1\xe3\xca\xca\xc7\xc8\xda\xc9\r\n"
	if size <= len(head)+len(body) {
		return head + body
	}
	pad := size - len(head) - len(body)
	line := strings.Repeat("\xe3", 70) + "\r\n"
	return head + body + strings.Repeat(line, pad/len(line)) + strings.Repeat("\xe3", pad%len(line))
}

func TestFetchMIMEReadsTheMIMEOfTheRealExchangeAnswer(t *testing.T) {
	for _, tc := range []struct {
		name string
		data func(string) *wbxml.Element
		size int
	}{
		{"an inline string (Exchange 2019)", mimeData, 600},
		{"an inline string of 72,024 bytes, the size in the trace", mimeData, 72024},
		{"OPAQUE", opaqueData, 600},
		{"OPAQUE of 72,024 bytes", opaqueData, 72024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			m := f.open("")
			raw := arabicMIME(tc.size)
			if tc.size == 72024 && len(raw) != 72024 {
				t.Fatalf("test message is %d bytes", len(raw))
			}
			sid := "7:" + strings.Repeat("0", 58) // the trace had 60 bytes
			f.fake.on("ItemOperations", always(0, fetchMIMEResponse(tc.data, "7", sid, raw, false, 0, "2026-10-01T10:00:00.000Z")))
			got, err := m.FetchMIME(ctx, "7", sid)
			if err != nil {
				t.Fatalf("FetchMIME: %v", err)
			}
			if string(got) != raw {
				t.Fatalf("FetchMIME returned %d bytes that differ from the %d bytes the server sent", len(got), len(raw))
			}
			// The request of the trace: Store=Mailbox, CollectionId, ServerId, Options{MIMESupport 2, BodyPreference{Type 4}}.
			req := f.fake.last("ItemOperations").Root
			for name, want := range map[string]string{"Store": "Mailbox", "CollectionId": "7", "ServerId": sid, "MIMESupport": "2"} {
				if v, _ := text(req, name); v != want {
					t.Errorf("%s = %q, want %q", name, v, want)
				}
			}
			if v, _ := text(req.Find("BodyPreference"), "Type"); v != "4" {
				t.Errorf("BodyPreference/Type = %q", v)
			}
		})
	}
}

func TestFetchMIMEWithoutAMIMEBodyIsErrNoMIME(t *testing.T) {
	f := newFixture(t)
	m := f.open("")
	raw := arabicMIME(300)
	for name, body := range map[string]*wbxml.Element{
		"Type 4 with an empty Data":        mimeBody(e(wbxml.PageAirSyncBase, "Data"), false, 0),
		"Type 4 without Data":              e(wbxml.PageAirSyncBase, "Body", e(wbxml.PageAirSyncBase, "Type", txt("4"))),
		"a plain-text body of 1 byte":      e(wbxml.PageAirSyncBase, "Body", e(wbxml.PageAirSyncBase, "Type", txt("1")), e(wbxml.PageAirSyncBase, "EstimatedDataSize", txt("300")), mimeData("h")),
		"an HTML body instead of the MIME": e(wbxml.PageAirSyncBase, "Body", e(wbxml.PageAirSyncBase, "Type", txt("2")), mimeData("<p>x</p>")),
	} {
		f.fake.on("ItemOperations", always(0, fetchResponse("7", "7:1", "1", messageProperties(raw, false, 0, "2026-10-01T10:00:00.000Z", body, false))))
		if _, err := m.FetchMIME(ctx, "7", "7:1"); !errors.Is(err, upstream.ErrNoMIME) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestSyncReadsTheItemsOfTheRealTrace(t *testing.T) {
	f := newFixture(t)
	m := f.open("")
	raw := arabicMIME(500)
	date := "2026-10-01T10:00:00.000Z"
	f.fake.on("Sync", always(0, syncResp("5", "k2", "1", commands(
		// A full-body Add with an inline string, as Exchange 2019 sends it.
		addItem("5:1", raw, false, 0, date, false, len(raw)),
		// The same cut to 100 bytes by the truncation size.
		addItemData(mimeData, "5:2", raw, raw[:100], true, 2, date, true, len(raw)),
		// The Add of a headers-only sync, with the attachment, Cc and ReplyTo of the trace.
		addItemHeadersOnly("5:3", raw, false, 0, date, len(raw))))))
	d, err := m.Sync(ctx, "5", "k1", upstream.SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Added) != 3 {
		t.Fatalf("%d items", len(d.Added))
	}
	a := d.Added
	want := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	if string(a[0].MIME) != raw || !a[0].MIMEComplete || a[0].EstimatedSize != len(raw) || a[0].Read || !a[0].ReadSet || a[0].Flagged || !a[0].FlagSet || !a[0].DateReceived.Equal(want) {
		t.Errorf("full item: %+v", a[0])
	}
	if string(a[1].MIME) != raw[:100] || a[1].MIMEComplete || !a[1].Read || !a[1].Flagged {
		t.Errorf("truncated item: complete=%v, %d bytes, read=%v, flagged=%v", a[1].MIMEComplete, len(a[1].MIME), a[1].Read, a[1].Flagged)
	}
	for i, it := range a {
		// The headers come as the server decoded them; the Arabic subject must arrive as UTF-8.
		if it.Subject != "العربية" || !strings.HasSuffix(it.From, "<sender@example.test>") || it.To != "alice@example.test" {
			t.Errorf("item %d headers: subject %q, from %q, to %q", i, it.Subject, it.From, it.To)
		}
	}

	// A headers-only sync: the body is a plain-text byte or two, never MIME.
	d, err = m.Sync(ctx, "5", "k1", upstream.SyncOptions{HeadersOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Added) != 3 || d.Added[2].MIME != nil || d.Added[2].EstimatedSize != len(raw) || d.Added[2].Subject != "العربية" {
		t.Errorf("headers-only items: %+v", d.Added)
	}
}
