package easupstream_test

// The Bcc recipients of a message in Sent Items (v0.1.8, real-Exchange finding 4).
//
// MS-ASEMAIL 2.2.2.9 defines email2:Bcc for protocol 16.0 and 16.1, 3.1.5.4 uses it
// for drafts, and 3.2.5.4 and 3.2.5.2 let a Sync or ItemOperations response carry any
// element of the E-mail class. Nothing says that Exchange lists the Bcc recipients of
// a sent message, and nothing has been seen of it: the adapter passes the element on
// when it is there and requests nothing for it.

import (
	"testing"

	"github.com/hstern/go-activesync/wbxml"

	"easbridge/internal/upstream"
)

func TestSyncPassesOnTheBccPropertyOfAnItemAndRequestsNothingForIt(t *testing.T) {
	f := newFixture(t)
	m := f.open("")
	raw := arabicMIME(500)
	date := "2026-10-01T10:00:00.000Z"
	withBcc := addItem("5:1", raw, true, 0, date, false, len(raw))
	app := withBcc.Find("ApplicationData")
	// Where the element stands among the others is up to the server.
	app.Children = append(app.Children, e(wbxml.PageEmail2, "Bcc", txt(`"Eve Example" <eve@example.test>, frank@example.test`)))
	f.fake.on("Sync", always(0, syncResp("5", "k2", "1", commands(
		withBcc,
		addItem("5:2", raw, true, 0, date, false, len(raw))))))

	d, err := m.Sync(ctx, "5", "k1", upstream.SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Added) != 2 {
		t.Fatalf("%d items", len(d.Added))
	}
	if got := d.Added[0].Bcc; got != `"Eve Example" <eve@example.test>, frank@example.test` {
		t.Errorf("Bcc = %q", got)
	}
	if got := d.Added[1].Bcc; got != "" {
		t.Errorf("an item without the element has Bcc %q", got)
	}
	if string(d.Added[0].MIME) != raw || !d.Added[0].MIMEComplete {
		t.Errorf("the property disturbed the body: %d bytes, complete=%v", len(d.Added[0].MIME), d.Added[0].MIMEComplete)
	}
	// Nothing is asked for it: the request has no property list, no Schema, and
	// mentions no Bcc.
	req := f.fake.last("Sync").Root
	if req.Find("Schema") != nil || req.Find("Bcc") != nil {
		t.Errorf("the Sync request asks for properties: %+v", req)
	}
}
