// Copyright (C) 2026 Henry Stern
// SPDX-License-Identifier: MIT
//
// Tests for the easbridge patches (see ../PATCHES.md). One test group per
// patch; the numbers match PATCHES.md.

package eas

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hstern/go-activesync/wbxml"
)

func wbxmlBytes(t *testing.T, root *wbxml.Element) []byte {
	t.Helper()
	b, err := wbxml.Marshal(&wbxml.Document{Root: root}, wbxml.DefaultRegistry())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func parseRequest(t *testing.T, body []byte) *wbxml.Element {
	t.Helper()
	doc, err := wbxml.Unmarshal(body, wbxml.DefaultRegistry())
	if err != nil {
		t.Fatalf("request is not valid WBXML: %v", err)
	}
	return doc.Root
}

// ---- patch 1: Policy Status 2 ("no policy defined") is success -------------

func TestPatch1_provisionPolicyStatus2IsSuccess(t *testing.T) {
	var calls atomic.Int32
	c, cap, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/vnd.ms-sync.wbxml")
		w.Write(wbxmlBytes(t, wbxml.E(wbxml.PageProvision, "Provision",
			wbxml.E(wbxml.PageProvision, "Status", wbxml.Text("1")),
			wbxml.E(wbxml.PageProvision, "Policies",
				wbxml.E(wbxml.PageProvision, "Policy",
					wbxml.E(wbxml.PageProvision, "PolicyType", wbxml.Text("MS-EAS-Provisioning-WBXML")),
					wbxml.E(wbxml.PageProvision, "Status", wbxml.Text("2")),
				),
			),
		)))
	})
	ctx := context.Background()
	// A stale key from an earlier policy must be cleared.
	_ = c.cfg.State.SetPolicyKey(ctx, "STALE")
	if err := c.Provision(ctx); err != nil {
		t.Fatalf("Provision with policy status 2 must succeed, got %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("round trips = %d, want 1 (nothing to acknowledge)", got)
	}
	if pk, _ := c.cfg.State.PolicyKey(ctx); pk != "" {
		t.Errorf("policy key = %q, want cleared", pk)
	}
	if c.LastPolicy() != nil {
		t.Errorf("LastPolicy should be nil when no policy is defined")
	}
	_ = cap
}

func TestPatch1_policyStatus3StillFails(t *testing.T) {
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write(wbxmlBytes(t, wbxml.E(wbxml.PageProvision, "Provision",
			wbxml.E(wbxml.PageProvision, "Status", wbxml.Text("1")),
			wbxml.E(wbxml.PageProvision, "Policies",
				wbxml.E(wbxml.PageProvision, "Policy",
					wbxml.E(wbxml.PageProvision, "Status", wbxml.Text("3")),
				),
			),
		)))
	})
	err := c.Provision(context.Background())
	if !IsStatusCode(err, 3) {
		t.Fatalf("policy status 3 must remain an error, got %v", err)
	}
}

// ---- patch 2: RemoteWipe is surfaced ---------------------------------------

func wipeResponse(t *testing.T) []byte {
	return wbxmlBytes(t, wbxml.E(wbxml.PageProvision, "Provision",
		wbxml.E(wbxml.PageProvision, "Status", wbxml.Text("1")),
		wbxml.E(wbxml.PageProvision, "RemoteWipe"),
	))
}

func TestPatch2_provisionSurfacesRemoteWipe(t *testing.T) {
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write(wipeResponse(t))
	})
	err := c.Provision(context.Background())
	if !errors.Is(err, ErrRemoteWipe) {
		t.Fatalf("want ErrRemoteWipe, got %v", err)
	}
}

func TestPatch2_wipeAfter449ReprovisionIsSurfaced(t *testing.T) {
	// A command answered with 449 makes the client re-provision. If that
	// Provision carries RemoteWipe the original command must fail with it.
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "Cmd=Provision") {
			w.Write(wipeResponse(t))
			return
		}
		w.WriteHeader(449)
	})
	_ = c.cfg.State.SetSyncKey(context.Background(), "inbox", "S1")
	_, err := c.SyncEmail(context.Background(), "inbox", EmailSyncOptions{NoBootstrap: true})
	if !errors.Is(err, ErrRemoteWipe) {
		t.Fatalf("want ErrRemoteWipe through the 449 path, got %v", err)
	}
}

// ---- patch 3: DeletesAsMoves=0 per call ------------------------------------

func syncAck(t *testing.T, key string, extra ...*wbxml.Element) []byte {
	coll := wbxml.E(wbxml.PageAirSync, "Collection",
		wbxml.E(wbxml.PageAirSync, "SyncKey", wbxml.Text(key)),
		wbxml.E(wbxml.PageAirSync, "CollectionId", wbxml.Text("inbox")),
		wbxml.E(wbxml.PageAirSync, "Status", wbxml.Text("1")),
	)
	for _, e := range extra {
		coll.Children = append(coll.Children, e)
	}
	return wbxmlBytes(t, wbxml.E(wbxml.PageAirSync, "Sync",
		wbxml.E(wbxml.PageAirSync, "Collections", coll)))
}

func findText(root *wbxml.Element, name string) (string, bool) {
	if e := root.Find(name); e != nil {
		return e.TextContent(), true
	}
	return "", false
}

func TestPatch3_hardDeleteSendsDeletesAsMovesZero(t *testing.T) {
	for _, tc := range []struct {
		hard bool
		want string
	}{{false, "1"}, {true, "0"}} {
		c, cap, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Write(syncAck(t, "S2"))
		})
		ctx := context.Background()
		_ = c.cfg.State.SetSyncKey(ctx, "inbox", "S1")
		res, err := ApplyEmailChangesWith(ctx, c, "inbox",
			[]EmailChange{{ServerID: "inbox:9", Delete: true}}, ApplyOptions{HardDelete: tc.hard})
		if err != nil {
			t.Fatal(err)
		}
		if res.SyncKey != "S2" {
			t.Errorf("SyncKey = %q", res.SyncKey)
		}
		req := parseRequest(t, cap.body)
		if got, _ := findText(req, "DeletesAsMoves"); got != tc.want {
			t.Errorf("hard=%v: DeletesAsMoves = %q, want %q", tc.hard, got, tc.want)
		}
		if req.Find("Delete") == nil {
			t.Errorf("request has no Delete command")
		}
		if k, _ := c.cfg.State.SyncKey(ctx, "inbox"); k != "S2" {
			t.Errorf("state key = %q, want S2", k)
		}
	}
}

func TestPatch3_noSyncKeyIsRefusedAndStatus3IsNotSwallowed(t *testing.T) {
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write(wbxmlBytes(t, wbxml.E(wbxml.PageAirSync, "Sync",
			wbxml.E(wbxml.PageAirSync, "Status", wbxml.Text("3")))))
	})
	ctx := context.Background()
	if _, err := ApplyEmailChangesWith(ctx, c, "inbox",
		[]EmailChange{{ServerID: "x", Delete: true}}, ApplyOptions{}); err == nil {
		t.Fatal("expected an error for a folder without a sync key")
	}
	_ = c.cfg.State.SetSyncKey(ctx, "inbox", "S1")
	_, err := ApplyEmailChangesWith(ctx, c, "inbox",
		[]EmailChange{{ServerID: "x", Delete: true}}, ApplyOptions{})
	if !IsStatusCode(err, StatusInvalidSyncKey) {
		t.Fatalf("status 3 must be returned to the caller, got %v", err)
	}
	// The key must NOT have been reset behind the caller's back.
	if k, _ := c.cfg.State.SyncKey(ctx, "inbox"); k != "S1" {
		t.Errorf("key was changed to %q", k)
	}
}

func TestPatch3_unsupportedClient(t *testing.T) {
	if _, err := ApplyEmailChangesWith(context.Background(), nil, "f", nil, ApplyOptions{}); !errors.Is(err, ErrUnsupportedClient) {
		t.Fatalf("want ErrUnsupportedClient, got %v", err)
	}
}

// ---- patch 4: FilterType omission and truncation control -------------------

func TestPatch4_noDateFilterAndTruncation(t *testing.T) {
	run := func(opts EmailSyncOptions) *wbxml.Element {
		c, cap, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Write(syncAck(t, "S2"))
		})
		_ = c.cfg.State.SetSyncKey(context.Background(), "inbox", "S1")
		opts.NoBootstrap = true
		if _, err := c.SyncEmail(context.Background(), "inbox", opts); err != nil {
			t.Fatal(err)
		}
		return parseRequest(t, cap.body)
	}

	req := run(EmailSyncOptions{BodyType: BodyTypeMIME, MIMESupport: 2, NoDateFilter: true, BodyTruncationSize: 1 << 20})
	if _, ok := findText(req, "FilterType"); ok {
		t.Errorf("FilterType must be omitted with NoDateFilter")
	}
	if got, _ := findText(req, "TruncationSize"); got != "1048576" {
		t.Errorf("TruncationSize = %q, want 1048576 (raised from the 32 KiB default)", got)
	}

	req = run(EmailSyncOptions{BodyType: BodyTypeMIME, MIMESupport: 2, NoDateFilter: true, NoBodyTruncation: true})
	if _, ok := findText(req, "TruncationSize"); ok {
		t.Errorf("TruncationSize must be omitted with NoBodyTruncation")
	}

	// Defaults are unchanged: FilterTwoWeek and 32 KiB.
	req = run(EmailSyncOptions{})
	if got, _ := findText(req, "FilterType"); got != "4" {
		t.Errorf("default FilterType = %q, want 4", got)
	}
	if got, _ := findText(req, "TruncationSize"); got != "32768" {
		t.Errorf("default TruncationSize = %q, want 32768", got)
	}
}

// ---- patch 5: ItemOperations Fetch sends MIMESupport=2 for MIME ------------

func fetchResp(t *testing.T) []byte {
	return wbxmlBytes(t, wbxml.E(wbxml.PageItemOperations, "ItemOperations",
		wbxml.E(wbxml.PageItemOperations, "Status", wbxml.Text("1")),
		wbxml.E(wbxml.PageItemOperations, "Response",
			wbxml.E(wbxml.PageItemOperations, "Fetch",
				wbxml.E(wbxml.PageItemOperations, "Status", wbxml.Text("1")),
				wbxml.E(wbxml.PageAirSync, "ServerId", wbxml.Text("inbox:1")),
				wbxml.E(wbxml.PageItemOperations, "Properties",
					wbxml.E(wbxml.PageAirSyncBase, "Body",
						wbxml.E(wbxml.PageAirSyncBase, "Type", wbxml.Text("4")),
						wbxml.E(wbxml.PageAirSyncBase, "Data", wbxml.Opaque("Subject: x\r\n\r\nbody")),
					),
				),
			),
		),
	))
}

func TestPatch5_fetchEmailSendsMIMESupport(t *testing.T) {
	c, cap, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { w.Write(fetchResp(t)) })
	item, err := c.FetchEmail(context.Background(), "inbox", "inbox:1", FetchEmailOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(item.BodyMIME) != "Subject: x\r\n\r\nbody" {
		t.Errorf("BodyMIME = %q", item.BodyMIME)
	}
	req := parseRequest(t, cap.body)
	if got, _ := findText(req, "MIMESupport"); got != "2" {
		t.Errorf("MIMESupport = %q, want 2", got)
	}

	// Not asked for MIME -> not sent.
	c2, cap2, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { w.Write(fetchResp(t)) })
	if _, err := c2.FetchEmail(context.Background(), "inbox", "inbox:1", FetchEmailOptions{BodyType: BodyTypePlain}); err != nil {
		t.Fatal(err)
	}
	if _, ok := findText(parseRequest(t, cap2.body), "MIMESupport"); ok {
		t.Errorf("MIMESupport must not be sent for a Plain body request")
	}
}

// ---- patch 6: Read / Flag presence -----------------------------------------

func TestPatch6_presenceOfReadAndFlag(t *testing.T) {
	app := func(children ...*wbxml.Element) *wbxml.Element {
		e := wbxml.E(wbxml.PageAirSync, "ApplicationData")
		for _, c := range children {
			e.Children = append(e.Children, c)
		}
		return e
	}
	onlyRead := parseEmailItem("1", app(wbxml.E(wbxml.PageEmail, "Read", wbxml.Text("0"))))
	if !onlyRead.ReadSet || onlyRead.Read || onlyRead.FlagSet {
		t.Errorf("only Read=0: %+v", onlyRead)
	}
	emptyFlag := parseEmailItem("2", app(wbxml.E(wbxml.PageEmail, "Flag")))
	if emptyFlag.ReadSet || !emptyFlag.FlagSet || emptyFlag.FlagStatus != 0 {
		t.Errorf("empty <Flag/> (cleared): %+v", emptyFlag)
	}
	active := parseEmailItem("3", app(wbxml.E(wbxml.PageEmail, "Flag",
		wbxml.E(wbxml.PageEmail, "FlagStatus", wbxml.Text("2")))))
	if !active.FlagSet || active.FlagStatus != 2 || active.ReadSet {
		t.Errorf("active flag: %+v", active)
	}
	none := parseEmailItem("4", app(wbxml.E(wbxml.PageEmail, "Subject", wbxml.Text("s"))))
	if none.ReadSet || none.FlagSet {
		t.Errorf("neither mentioned: %+v", none)
	}
}

// ---- patch 7: ApplyEmailChangesWith keeps server changes and sends GetChanges=0 ----

func TestPatch7_serverChangesInApplyResponseAreReturned(t *testing.T) {
	c, cap, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write(syncAck(t, "S2",
			wbxml.E(wbxml.PageAirSync, "Commands",
				wbxml.E(wbxml.PageAirSync, "Add",
					wbxml.E(wbxml.PageAirSync, "ServerId", wbxml.Text("inbox:77")),
					wbxml.E(wbxml.PageAirSync, "ApplicationData",
						wbxml.E(wbxml.PageEmail, "Subject", wbxml.Text("hello")),
						wbxml.E(wbxml.PageEmail, "Read", wbxml.Text("0")),
					),
				),
				wbxml.E(wbxml.PageAirSync, "Delete",
					wbxml.E(wbxml.PageAirSync, "ServerId", wbxml.Text("inbox:5")),
				),
			),
			wbxml.E(wbxml.PageAirSync, "MoreAvailable"),
			wbxml.E(wbxml.PageAirSync, "Responses",
				wbxml.E(wbxml.PageAirSync, "Change",
					wbxml.E(wbxml.PageAirSync, "ServerId", wbxml.Text("inbox:1")),
					wbxml.E(wbxml.PageAirSync, "Status", wbxml.Text("8")),
				),
			),
		))
	})
	ctx := context.Background()
	_ = c.cfg.State.SetSyncKey(ctx, "inbox", "S1")
	read := true
	res, err := ApplyEmailChangesWith(ctx, c, "inbox",
		[]EmailChange{{ServerID: "inbox:1", Read: &read}}, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	req := parseRequest(t, cap.body)
	if got, _ := findText(req, "GetChanges"); got != "0" {
		t.Errorf("GetChanges = %q, want 0", got)
	}
	if len(res.Results) != 1 || res.Results[0].ServerID != "inbox:1" || res.Results[0].Status != 8 {
		t.Errorf("Results = %+v", res.Results)
	}
	sc := res.ServerChanges
	if sc == nil || len(sc.Added) != 1 || sc.Added[0].ServerID != "inbox:77" || sc.Added[0].Subject != "hello" ||
		len(sc.Deleted) != 1 || sc.Deleted[0] != "inbox:5" || !sc.MoreAvailable {
		t.Fatalf("ServerChanges = %+v", sc)
	}
	if !sc.Added[0].ReadSet {
		t.Errorf("patch 6 presence flag missing on the added item")
	}
}

func TestPatch7_noServerChangesGivesNil(t *testing.T) {
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { w.Write(syncAck(t, "S2")) })
	ctx := context.Background()
	_ = c.cfg.State.SetSyncKey(ctx, "inbox", "S1")
	read := false
	res, err := ApplyEmailChangesWith(ctx, c, "inbox", []EmailChange{{ServerID: "inbox:1", Read: &read}}, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.ServerChanges != nil {
		t.Errorf("ServerChanges = %+v, want nil", res.ServerChanges)
	}
}

// ---- patch 8: Provision code page, AccountOnlyRemoteWipe -------------------

// wbxmlHeader is WBXML 1.3, public id 1, UTF-8, empty string table; the body
// bytes below switch to code page 14 (Provision) and use its tokens directly, so
// these tests do not depend on the library's own name table.
var wbxmlHeader = []byte{0x03, 0x01, 0x6A, 0x00, 0x00, 0x0E}

// rawAccountOnlyWipe is <Provision><Status>1</Status><AccountOnlyRemoteWipe/></Provision>
// as a server sends it: Provision 0x45, Status 0x4B "1", AccountOnlyRemoteWipe 0x3B.
func rawAccountOnlyWipe() []byte {
	return append(append([]byte{}, wbxmlHeader...),
		0x45, 0x4B, 0x03, '1', 0x00, 0x01, 0x3B, 0x01)
}

func TestPatch8_provisionSurfacesAccountOnlyRemoteWipe(t *testing.T) {
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { w.Write(rawAccountOnlyWipe()) })
	err := c.Provision(context.Background())
	if !errors.Is(err, ErrAccountOnlyRemoteWipe) {
		t.Fatalf("want ErrAccountOnlyRemoteWipe, got %v", err)
	}
	// An account-only wipe is a wipe for callers that do not care which kind.
	if !errors.Is(err, ErrRemoteWipe) {
		t.Fatalf("an account-only wipe must also match ErrRemoteWipe, got %v", err)
	}
	// But a full wipe is not an account-only one.
	if errors.Is(ErrRemoteWipe, ErrAccountOnlyRemoteWipe) {
		t.Fatal("ErrRemoteWipe must not match ErrAccountOnlyRemoteWipe")
	}
}

func TestPatch8_accountOnlyWipeAfter449ReprovisionIsSurfaced(t *testing.T) {
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "Cmd=Provision") {
			w.Write(rawAccountOnlyWipe())
			return
		}
		w.WriteHeader(449)
	})
	_ = c.cfg.State.SetSyncKey(context.Background(), "inbox", "S1")
	_, err := c.SyncEmail(context.Background(), "inbox", EmailSyncOptions{NoBootstrap: true})
	if !errors.Is(err, ErrAccountOnlyRemoteWipe) {
		t.Fatalf("want ErrAccountOnlyRemoteWipe through the 449 path, got %v", err)
	}
}

func TestPatch8_aFullWipeBeatsAnAccountOnlyOne(t *testing.T) {
	both := append(append([]byte{}, wbxmlHeader...),
		0x45, 0x4B, 0x03, '1', 0x00, 0x01, 0x3B, 0x0C, 0x01) // AccountOnlyRemoteWipe then RemoteWipe
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { w.Write(both) })
	err := c.Provision(context.Background())
	if !errors.Is(err, ErrRemoteWipe) || errors.Is(err, ErrAccountOnlyRemoteWipe) {
		t.Fatalf("want the full wipe, got %v", err)
	}
}

func TestPatch8_acknowledgeAccountOnlyRemoteWipe(t *testing.T) {
	c, cap, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write(wbxmlBytes(t, wbxml.E(wbxml.PageProvision, "Provision", wbxml.E(wbxml.PageProvision, "Status", wbxml.Text("1")))))
	})
	if err := c.AcknowledgeAccountOnlyRemoteWipe(context.Background(), 0); err != nil { // 0 means success
		t.Fatal(err)
	}
	root := parseRequest(t, cap.body)
	if root.Name != "Provision" || len(root.Children) != 1 {
		t.Fatalf("request root: %s with %d children", root.Name, len(root.Children))
	}
	wipe := root.Find("AccountOnlyRemoteWipe")
	if wipe == nil || root.Find("RemoteWipe") != nil {
		t.Fatalf("the acknowledgement must be AccountOnlyRemoteWipe and not RemoteWipe: %+v", root)
	}
	if st, ok := findText(wipe, "Status"); !ok || st != "1" {
		t.Fatalf("Status = %q (%v), want 1", st, ok)
	}
	// On the wire it is token 0x3B (with content: 0x7B) of code page 14.
	if !bytes.Contains(cap.body, []byte{0x00, 0x0E, 0x45, 0x7B}) {
		t.Errorf("request bytes % X do not contain Provision followed by AccountOnlyRemoteWipe", cap.body)
	}
	if err := c.AcknowledgeAccountOnlyRemoteWipe(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	if st, _ := findText(parseRequest(t, cap.body).Find("AccountOnlyRemoteWipe"), "Status"); st != "2" {
		t.Errorf("failure status = %q, want 2", st)
	}
}

func TestPatch8_acknowledgementRefusedByTheServerIsAnError(t *testing.T) {
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write(wbxmlBytes(t, wbxml.E(wbxml.PageProvision, "Provision", wbxml.E(wbxml.PageProvision, "Status", wbxml.Text("2")))))
	})
	if err := c.AcknowledgeAccountOnlyRemoteWipe(context.Background(), 1); !IsStatusCode(err, 2) {
		t.Fatalf("want a status 2 error, got %v", err)
	}
}

func TestPatch8_policyDocumentIsReadUnderTheSpecNames(t *testing.T) {
	// EASProvisionDoc with DevicePasswordEnabled (0x0E) = 1 and
	// MinDevicePasswordLength (0x14) = 6. Under the old table 0x0E was
	// "AllowBluetooth" and 0x14 "DevicePasswordHistory": the policy came out wrong.
	doc := append(append([]byte{}, wbxmlHeader...),
		0x45,                        // Provision
		0x4B, 0x03, '1', 0x00, 0x01, // Status
		0x46, 0x47, // Policies, Policy
		0x48, 0x03, 'M', 'S', '-', 'E', 'A', 'S', '-', 'P', 'r', 'o', 'v', 'i', 's', 'i', 'o', 'n', 'i', 'n', 'g', '-', 'W', 'B', 'X', 'M', 'L', 0x00, 0x01, // PolicyType
		0x49, 0x03, '1', '2', '3', 0x00, 0x01, // PolicyKey
		0x4B, 0x03, '1', 0x00, 0x01, // Status
		0x4A, 0x4D, // Data, EASProvisionDoc
		0x4E, 0x03, '1', 0x00, 0x01, // DevicePasswordEnabled
		0x54, 0x03, '6', 0x00, 0x01, // MinDevicePasswordLength
		0x01, 0x01, 0x01, 0x01, 0x01) // end of doc, Data, Policy, Policies, Provision
	var calls atomic.Int32
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Write(doc)
			return
		}
		w.Write(wbxmlBytes(t, wbxml.E(wbxml.PageProvision, "Provision",
			wbxml.E(wbxml.PageProvision, "Status", wbxml.Text("1")),
			wbxml.E(wbxml.PageProvision, "Policies", wbxml.E(wbxml.PageProvision, "Policy",
				wbxml.E(wbxml.PageProvision, "PolicyKey", wbxml.Text("456")),
				wbxml.E(wbxml.PageProvision, "Status", wbxml.Text("1")))))))
	})
	if err := c.Provision(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := c.LastPolicy()
	if p == nil || !p.DevicePasswordEnabled || p.MinDevicePasswordLength != 6 {
		t.Fatalf("policy read under the wrong names: %+v", p)
	}
}

// ---- patch 9: HTTPError carries Retry-After ----------------------------------

func TestPatch9_httpErrorCarriesRetryAfter(t *testing.T) {
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	_ = c.cfg.State.SetSyncKey(context.Background(), "inbox", "S1")
	_, err := c.SyncEmail(context.Background(), "inbox", EmailSyncOptions{NoBootstrap: true})
	var he *HTTPError
	if !errors.As(err, &he) || he.StatusCode != http.StatusServiceUnavailable || he.RetryAfter != "120" {
		t.Fatalf("want HTTPError 503 with RetryAfter 120, got %v (%+v)", err, he)
	}
	// An answer without the header leaves the field empty.
	c2, _, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	_ = c2.cfg.State.SetSyncKey(context.Background(), "inbox", "S1")
	_, err = c2.SyncEmail(context.Background(), "inbox", EmailSyncOptions{NoBootstrap: true})
	if !errors.As(err, &he) || he.StatusCode != http.StatusBadGateway || he.RetryAfter != "" {
		t.Fatalf("want HTTPError 502 without RetryAfter, got %v (%+v)", err, he)
	}
}

func TestPatch9_optionsErrorCarriesRetryAfter(t *testing.T) {
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "Fri, 09 Oct 2026 12:00:00 GMT")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := c.Options(context.Background())
	var he *HTTPError
	if !errors.As(err, &he) || he.StatusCode != http.StatusTooManyRequests || he.RetryAfter != "Fri, 09 Oct 2026 12:00:00 GMT" {
		t.Fatalf("OPTIONS error: %v (%+v)", err, he)
	}
}

// ---- patch 10: device information inside the initial Provision request ------

// seen is one request the test server received.
type seen struct {
	cmd, policyKey string
	body           []byte
}

// provServer is a small server for the Provision handshake (policy download with
// the temporary key TKEY, acknowledgement with the final key FKEY). Other
// commands go to other.
type provServer struct {
	mu    sync.Mutex
	reqs  []seen
	other func(w http.ResponseWriter, r *http.Request, n int) // n counts the requests of that command, from 1
	// provResp, when set, replaces the answer to the policy download.
	provResp func() *wbxml.Element
}

func (p *provServer) count(cmd string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, r := range p.reqs {
		if r.cmd == cmd {
			n++
		}
	}
	return n
}

func (p *provServer) of(cmd string) []seen {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []seen
	for _, r := range p.reqs {
		if r.cmd == cmd {
			out = append(out, r)
		}
	}
	return out
}

func newProvClient(t *testing.T, version string, di *DeviceInformation, ps *provServer) *httpClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cmd := r.URL.Query().Get("Cmd")
		ps.mu.Lock()
		ps.reqs = append(ps.reqs, seen{cmd: cmd, policyKey: r.Header.Get("X-MS-PolicyKey"), body: body})
		n := 0
		for _, q := range ps.reqs {
			if q.cmd == cmd {
				n++
			}
		}
		ps.mu.Unlock()
		w.Header().Set("Content-Type", "application/vnd.ms-sync.wbxml")
		if cmd != "Provision" {
			if ps.other == nil {
				w.WriteHeader(http.StatusNotImplemented)
				return
			}
			ps.other(w, r, n)
			return
		}
		doc, err := wbxml.Unmarshal(body, wbxml.DefaultRegistry())
		if err != nil {
			t.Errorf("the Provision request is not valid WBXML: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		ack := doc.Root.Find("PolicyKey") != nil
		if !ack && ps.provResp != nil {
			w.Write(wbxmlBytes(t, ps.provResp()))
			return
		}
		key := "TKEY"
		if ack {
			key = "FKEY"
		}
		resp := wbxml.E(wbxml.PageProvision, "Provision")
		if !ack && doc.Root.Find("DeviceInformation") != nil {
			resp.Children = append(resp.Children, wbxml.E(wbxml.PageSettings, "DeviceInformation",
				wbxml.E(wbxml.PageSettings, "Status", wbxml.Text("1"))))
		}
		resp.Children = append(resp.Children,
			wbxml.E(wbxml.PageProvision, "Status", wbxml.Text("1")),
			wbxml.E(wbxml.PageProvision, "Policies", wbxml.E(wbxml.PageProvision, "Policy",
				wbxml.E(wbxml.PageProvision, "PolicyType", wbxml.Text("MS-EAS-Provisioning-WBXML")),
				wbxml.E(wbxml.PageProvision, "Status", wbxml.Text("1")),
				wbxml.E(wbxml.PageProvision, "PolicyKey", wbxml.Text(key)))))
		w.Write(wbxmlBytes(t, resp))
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(Config{
		ServerURL: srv.URL + "/Microsoft-Server-ActiveSync", Username: "henry", Password: "hunter2",
		DeviceID: "abcdef0123456789abcdef0123456789", State: NewMemoryState(),
		ASVersion: version, DeviceInformation: di,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c.(*httpClient)
}

var testDevice = DeviceInformation{
	Model: "easbridge", FriendlyName: "easbridge (MailFlow bridge)", OS: "Linux", OSLanguage: "en-us", UserAgent: "easbridge/test",
}

// wbxmlStr and wbxmlEl build the bytes of an inline string and of a leaf element
// that carries one (tag token | 0x40 because it has content).
func wbxmlStr(s string) []byte { return append(append([]byte{0x03}, s...), 0x00) }
func wbxmlEl(tok byte, s string) []byte {
	return append(append([]byte{tok | 0x40}, wbxmlStr(s)...), 0x01)
}
func wbxmlCat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// wantInitialProvision is the policy download of protocol 14.1 and later with
// testDevice, byte for byte. Tokens are those of MS-ASWBXML: code page 14
// (Provision) 0x05 Provision, 0x06 Policies, 0x07 Policy, 0x08 PolicyType; code
// page 18 (Settings) 0x16 DeviceInformation, 0x08 Set, 0x17 Model, 0x19
// FriendlyName, 0x1A OS, 0x1B OSLanguage, 0x20 UserAgent, 0x21 EnableOutboundSMS.
var wantInitialProvision = wbxmlCat(
	[]byte{0x03, 0x01, 0x6A, 0x00}, // WBXML 1.3, unknown public id, UTF-8, empty string table
	[]byte{0x00, 0x0E, 0x45},       // SWITCH_PAGE 14, Provision
	[]byte{0x00, 0x12, 0x56, 0x48}, // SWITCH_PAGE 18, DeviceInformation, Set
	wbxmlEl(0x17, "easbridge"),
	wbxmlEl(0x19, "easbridge (MailFlow bridge)"),
	wbxmlEl(0x1A, "Linux"),
	wbxmlEl(0x1B, "en-us"),
	wbxmlEl(0x20, "easbridge/test"),
	wbxmlEl(0x21, "0"),
	[]byte{0x01, 0x01},                   // END Set, END DeviceInformation
	[]byte{0x00, 0x0E, 0x46, 0x47, 0x48}, // SWITCH_PAGE 14, Policies, Policy, PolicyType
	wbxmlStr("MS-EAS-Provisioning-WBXML"),
	[]byte{0x01, 0x01, 0x01, 0x01}, // END PolicyType, Policy, Policies, Provision
)

// wantAcknowledgement is the second request: the temporary key and Status 1, no
// device information.
var wantAcknowledgement = wbxmlCat(
	[]byte{0x03, 0x01, 0x6A, 0x00},
	[]byte{0x00, 0x0E, 0x45, 0x46, 0x47, 0x48}, // Provision, Policies, Policy, PolicyType
	wbxmlStr("MS-EAS-Provisioning-WBXML"),
	[]byte{0x01},
	[]byte{0x49}, wbxmlStr("TKEY"), []byte{0x01}, // PolicyKey
	[]byte{0x4B}, wbxmlStr("1"), []byte{0x01}, // Status
	[]byte{0x01, 0x01, 0x01}, // END Policy, Policies, Provision
)

func TestPatch10_initialProvisionCarriesDeviceInformationBeforePolicies(t *testing.T) {
	for _, version := range []string{"14.1", "16.0", "16.1"} {
		t.Run(version, func(t *testing.T) {
			ps := &provServer{}
			di := testDevice
			c := newProvClient(t, version, &di, ps)
			if err := c.Provision(context.Background()); err != nil {
				t.Fatal(err)
			}
			reqs := ps.of("Provision")
			if len(reqs) != 2 {
				t.Fatalf("%d Provision requests, want the two phases", len(reqs))
			}
			if !bytes.Equal(reqs[0].body, wantInitialProvision) {
				t.Errorf("initial request:\n got % X\nwant % X", reqs[0].body, wantInitialProvision)
			}
			// MS-ASPROV 2.2.2.53: "but not on subsequent requests".
			if !bytes.Equal(reqs[1].body, wantAcknowledgement) {
				t.Errorf("acknowledgement:\n got % X\nwant % X", reqs[1].body, wantAcknowledgement)
			}
			if bytes.Contains(reqs[1].body, []byte{0x56}) {
				t.Errorf("the acknowledgement carries device information")
			}
			if pk, _ := c.cfg.State.PolicyKey(context.Background()); pk != "FKEY" {
				t.Errorf("policy key = %q", pk)
			}
			// The decoded shape, as a second opinion: DeviceInformation is the
			// first child of Provision, ahead of Policies, and its Set has Model.
			doc, err := wbxml.Unmarshal(reqs[0].body, wbxml.DefaultRegistry())
			if err != nil {
				t.Fatal(err)
			}
			var kids []string
			for _, k := range doc.Root.Children {
				if e, ok := k.(*wbxml.Element); ok {
					kids = append(kids, e.Name)
				}
			}
			if strings.Join(kids, ",") != "DeviceInformation,Policies" {
				t.Errorf("children of Provision = %v", kids)
			}
			if m := doc.Root.Find("DeviceInformation").Find("Set").Find("Model"); m == nil || m.TextContent() != "easbridge" {
				t.Errorf("Set has no Model")
			}
			for _, absent := range []string{"IMEI", "PhoneNumber", "MobileOperator"} {
				if doc.Root.Find(absent) != nil {
					t.Errorf("%s is sent although it was not set", absent)
				}
			}
		})
	}
}

func TestPatch10_olderProtocolsAndNoConfigurationSendNoDeviceInformationInProvision(t *testing.T) {
	// MS-ASPROV 2.2.2.53: on 14.0, 12.1 and 12.0 the client MUST NOT send it in a
	// Provision request. Without a configuration (upstream behaviour) nothing is sent.
	cases := []struct {
		version string
		di      *DeviceInformation
	}{{"14.0", &DeviceInformation{Model: "m"}}, {"12.1", &DeviceInformation{Model: "m"}}, {"12.0", &DeviceInformation{Model: "m"}},
		{"2.5", &DeviceInformation{Model: "m"}}, {"16.1", nil}, {"14.1", nil}}
	for _, tc := range cases {
		ps := &provServer{}
		c := newProvClient(t, tc.version, tc.di, ps)
		if err := c.Provision(context.Background()); err != nil {
			t.Fatalf("%s: %v", tc.version, err)
		}
		for i, r := range ps.of("Provision") {
			if bytes.Contains(r.body, []byte{0x00, 0x12}) {
				t.Errorf("%s (configured: %v): request %d switches to the Settings page: % X", tc.version, tc.di != nil, i+1, r.body)
			}
		}
		if len(ps.of("Provision")) != 2 {
			t.Errorf("%s: %d Provision requests", tc.version, len(ps.of("Provision")))
		}
	}
}

func TestPatch10_deviceInformationInProvisionRule(t *testing.T) {
	for v, want := range map[string]bool{
		"16.1": true, "16.0": true, "14.1": true, " 14.1 ": true, "17.0": true,
		"14.0": false, "12.1": false, "12.0": false, "2.5": false, "": false, "14": false, "x.y": false, "14.x": false, "-1.1": false,
	} {
		if got := DeviceInformationInProvision(v); got != want {
			t.Errorf("DeviceInformationInProvision(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestPatch10_modelIsRequired(t *testing.T) {
	for _, di := range []DeviceInformation{{}, {Model: "  "}, {FriendlyName: "no model"}} {
		di := di
		_, err := NewClient(Config{ServerURL: "https://x/Microsoft-Server-ActiveSync", Username: "u", Password: "p",
			DeviceID: "d", State: NewMemoryState(), DeviceInformation: &di})
		if err == nil || !strings.Contains(err.Error(), "Model") {
			t.Errorf("%+v: err = %v", di, err)
		}
	}
}

func TestPatch10_theConfigurationIsCopied(t *testing.T) {
	ps := &provServer{}
	di := testDevice
	c := newProvClient(t, "16.1", &di, ps)
	di.Model = "changed afterwards"
	if err := c.Provision(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ps.of("Provision")[0].body, wantInitialProvision) {
		t.Errorf("a later change of the caller's struct altered the request")
	}
}

func TestPatch10_theReprovisionAfter449CarriesItToo(t *testing.T) {
	ps := &provServer{other: func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			w.WriteHeader(449)
			return
		}
		w.Write(wbxmlBytes(t, wbxml.E(wbxml.PageFolderHierarchy, "FolderSync",
			wbxml.E(wbxml.PageFolderHierarchy, "Status", wbxml.Text("1")),
			wbxml.E(wbxml.PageFolderHierarchy, "SyncKey", wbxml.Text("H1")))))
	}}
	di := testDevice
	c := newProvClient(t, "16.1", &di, ps)
	if _, err := c.FolderSync(context.Background()); err != nil {
		t.Fatal(err)
	}
	reqs := ps.of("Provision")
	if len(reqs) != 2 || !bytes.Equal(reqs[0].body, wantInitialProvision) || !bytes.Equal(reqs[1].body, wantAcknowledgement) {
		t.Fatalf("re-provision after 449: %d requests, first % X", len(reqs), reqs[0].body)
	}
}

func TestPatch10_aRefusedDeviceInformationDoesNotFailTheProvisioning(t *testing.T) {
	ps := &provServer{provResp: func() *wbxml.Element {
		return wbxml.E(wbxml.PageProvision, "Provision",
			wbxml.E(wbxml.PageSettings, "DeviceInformation", wbxml.E(wbxml.PageSettings, "Status", wbxml.Text("2"))),
			wbxml.E(wbxml.PageProvision, "Status", wbxml.Text("1")),
			wbxml.E(wbxml.PageProvision, "Policies", wbxml.E(wbxml.PageProvision, "Policy",
				wbxml.E(wbxml.PageProvision, "PolicyType", wbxml.Text("MS-EAS-Provisioning-WBXML")),
				wbxml.E(wbxml.PageProvision, "Status", wbxml.Text("1")),
				wbxml.E(wbxml.PageProvision, "PolicyKey", wbxml.Text("TKEY")))))
	}}
	di := testDevice
	c := newProvClient(t, "16.1", &di, ps)
	if err := c.Provision(context.Background()); err != nil {
		t.Fatalf("a refused device description must not fail Provision: %v", err)
	}
	// The nested Status is not mistaken for the Provision status either way round.
	ps2 := &provServer{provResp: func() *wbxml.Element {
		return wbxml.E(wbxml.PageProvision, "Provision",
			wbxml.E(wbxml.PageSettings, "DeviceInformation", wbxml.E(wbxml.PageSettings, "Status", wbxml.Text("1"))),
			wbxml.E(wbxml.PageProvision, "Status", wbxml.Text("165")))
	}}
	c2 := newProvClient(t, "16.1", &di, ps2)
	if err := c2.Provision(context.Background()); !IsStatusCode(err, 165) {
		t.Fatalf("Provision status 165 must be reported as such, got %v", err)
	}
	if n := ps2.count("Provision"); n != 1 {
		t.Errorf("a refused Provision was repeated: %d requests", n)
	}
}

func TestPatch10_theSettingsCommandIsEncodedAsBefore(t *testing.T) {
	var body []byte
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Write(wbxmlBytes(t, wbxml.E(wbxml.PageSettings, "Settings", wbxml.E(wbxml.PageSettings, "Status", wbxml.Text("1")))))
	})
	if err := c.SettingsDeviceInformation(context.Background(), testDevice); err != nil {
		t.Fatal(err)
	}
	want := wbxmlCat(
		[]byte{0x03, 0x01, 0x6A, 0x00, 0x00, 0x12, 0x45, 0x56, 0x48}, // page 18, Settings, DeviceInformation, Set
		wbxmlEl(0x17, "easbridge"), wbxmlEl(0x19, "easbridge (MailFlow bridge)"), wbxmlEl(0x1A, "Linux"),
		wbxmlEl(0x1B, "en-us"), wbxmlEl(0x20, "easbridge/test"), wbxmlEl(0x21, "0"),
		[]byte{0x01, 0x01, 0x01})
	if !bytes.Equal(body, want) {
		t.Errorf("Settings request:\n got % X\nwant % X", body, want)
	}
}

// ---- patch 11: a status that asks for provisioning is answered like a 449 ----

func folderSyncStatus(t *testing.T, code string) []byte {
	return wbxmlBytes(t, wbxml.E(wbxml.PageFolderHierarchy, "FolderSync", wbxml.E(wbxml.PageFolderHierarchy, "Status", wbxml.Text(code))))
}

func folderSyncOK(t *testing.T) []byte {
	return wbxmlBytes(t, wbxml.E(wbxml.PageFolderHierarchy, "FolderSync",
		wbxml.E(wbxml.PageFolderHierarchy, "Status", wbxml.Text("1")),
		wbxml.E(wbxml.PageFolderHierarchy, "SyncKey", wbxml.Text("H1"))))
}

func TestPatch11_aStatusThatAsksForProvisioningIsAnsweredLikeA449(t *testing.T) {
	for _, code := range []string{"142", "143", "144"} {
		t.Run(code, func(t *testing.T) {
			ps := &provServer{other: func(w http.ResponseWriter, r *http.Request, n int) {
				if n == 1 {
					w.Write(folderSyncStatus(t, code))
					return
				}
				w.Write(folderSyncOK(t))
			}}
			di := testDevice
			c := newProvClient(t, "16.1", &di, ps)
			res, err := c.FolderSync(context.Background())
			if err != nil || res.SyncKey != "H1" {
				t.Fatalf("FolderSync: %+v %v", res, err)
			}
			if ps.count("FolderSync") != 2 || ps.count("Provision") != 2 {
				t.Errorf("FolderSync %d, Provision %d requests; want 2 and the two phases", ps.count("FolderSync"), ps.count("Provision"))
			}
			// The repeat is sent with the new policy key.
			if k := ps.of("FolderSync")[1].policyKey; k != "FKEY" {
				t.Errorf("the repeat used policy key %q", k)
			}
			// The re-provisioning is a normal one, device information included.
			if !bytes.Equal(ps.of("Provision")[0].body, wantInitialProvision) {
				t.Errorf("re-provisioning request: % X", ps.of("Provision")[0].body)
			}
		})
	}
}

func TestPatch11_aPersistingStatusIsNeverRetriedAgain(t *testing.T) {
	ps := &provServer{other: func(w http.ResponseWriter, r *http.Request, n int) { w.Write(folderSyncStatus(t, "143")) }}
	di := testDevice
	c := newProvClient(t, "16.1", &di, ps)
	_, err := c.FolderSync(context.Background())
	if !IsStatusCode(err, 143) {
		t.Fatalf("err = %v, want status 143", err)
	}
	if ps.count("FolderSync") != 2 || ps.count("Provision") != 2 {
		t.Errorf("FolderSync %d, Provision %d requests: one re-provisioning and one repeat, nothing more", ps.count("FolderSync"), ps.count("Provision"))
	}
}

func TestPatch11_status140LeadsToTheWipeOrder(t *testing.T) {
	ps := &provServer{
		other:    func(w http.ResponseWriter, r *http.Request, n int) { w.Write(folderSyncStatus(t, "140")) },
		provResp: func() *wbxml.Element { return wipeElement() },
	}
	di := testDevice
	c := newProvClient(t, "16.1", &di, ps)
	_, err := c.FolderSync(context.Background())
	if !errors.Is(err, ErrRemoteWipe) {
		t.Fatalf("err = %v, want ErrRemoteWipe", err)
	}
	if ps.count("FolderSync") != 1 || ps.count("Provision") != 1 {
		t.Errorf("FolderSync %d, Provision %d requests", ps.count("FolderSync"), ps.count("Provision"))
	}
}

func wipeElement() *wbxml.Element {
	return wbxml.E(wbxml.PageProvision, "Provision",
		wbxml.E(wbxml.PageProvision, "Status", wbxml.Text("1")),
		wbxml.E(wbxml.PageProvision, "RemoteWipe"))
}

func TestPatch11_provisionItselfIsNeverReprovisioned(t *testing.T) {
	for _, code := range []string{"140", "142", "143", "144"} {
		ps := &provServer{provResp: func() *wbxml.Element {
			return wbxml.E(wbxml.PageProvision, "Provision", wbxml.E(wbxml.PageProvision, "Status", wbxml.Text(code)))
		}}
		di := testDevice
		c := newProvClient(t, "16.1", &di, ps)
		// A deadline, so that a regression (Provision calling itself) fails instead of hanging.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := c.Provision(ctx)
		cancel()
		if err == nil || ps.count("Provision") != 1 {
			t.Errorf("status %s: err %v after %d Provision requests", code, err, ps.count("Provision"))
		}
	}
}

func TestPatch11_otherStatusesAreLeftAlone(t *testing.T) {
	// 141, 145 and 139 are refusals only an administrator can lift, 129 and 165
	// are not about the policy key; none of them is a reason to provision again.
	for _, code := range []string{"3", "129", "139", "141", "145", "165", "177"} {
		ps := &provServer{other: func(w http.ResponseWriter, r *http.Request, n int) { w.Write(folderSyncStatus(t, code)) }}
		di := testDevice
		c := newProvClient(t, "16.1", &di, ps)
		_, err := c.FolderSync(context.Background())
		var se *StatusError
		if !errors.As(err, &se) || ps.count("Provision") != 0 {
			t.Errorf("status %s: err %v, %d Provision requests", code, err, ps.count("Provision"))
		}
	}
}

func TestPatch11_anEmptyAnswerIsNotAProvisioningRequest(t *testing.T) {
	// SendMail answers success with an empty body.
	ps := &provServer{other: func(w http.ResponseWriter, r *http.Request, n int) { w.WriteHeader(http.StatusOK) }}
	di := testDevice
	c := newProvClient(t, "16.1", &di, ps)
	if err := c.SendMail(context.Background(), SendMailOptions{MIME: []byte("Subject: x\r\n\r\nbody\r\n"), ClientID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if ps.count("Provision") != 0 || ps.count("SendMail") != 1 {
		t.Errorf("SendMail %d, Provision %d requests", ps.count("SendMail"), ps.count("Provision"))
	}
}

// ---- patch 12: the priming Sync request, and the order inside Options ------

// syncRequestOf runs one SyncEmail with the stored key and returns the request
// body that went out.
func syncRequestOf(t *testing.T, storedKey string, opts EmailSyncOptions) []byte {
	t.Helper()
	c, cap, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write(syncAck(t, "NEWKEY"))
	})
	if storedKey != "" {
		_ = c.cfg.State.SetSyncKey(context.Background(), "5", storedKey)
	}
	opts.NoBootstrap = true
	if _, err := c.SyncEmail(context.Background(), "5", opts); err != nil {
		t.Fatal(err)
	}
	return cap.body
}

func TestPatch12_theRequestForAFirstSyncKeyHoldsTheKeyAndTheCollectionAndNothingElse(t *testing.T) {
	// MS-ASCMD 3.1.5.4: SyncKey 0 and CollectionId. Exchange answers status 4 to
	// GetChanges (set to 1 or empty) with SyncKey 0 (MS-ASCMD 2.2.3.84).
	want := wbxmlCat(
		[]byte{0x03, 0x01, 0x6A, 0x00},            // WBXML 1.3, unknown public id, UTF-8, empty string table
		[]byte{0x45, 0x5C, 0x4F},                  // AirSync page: Sync (0x05), Collections (0x1C), Collection (0x0F), all with content
		[]byte{0x4B}, wbxmlStr("0"), []byte{0x01}, // SyncKey (0x0B)
		[]byte{0x52}, wbxmlStr("5"), []byte{0x01}, // CollectionId (0x12)
		[]byte{0x01, 0x01, 0x01}, // END Collection, Collections, Sync
	)
	// Whatever the caller asks for, the first request is the same.
	for name, opts := range map[string]EmailSyncOptions{
		"defaults":     {},
		"the service":  {WindowSize: 20, BodyType: BodyTypeMIME, MIMESupport: 2, BodyTruncationSize: 1 << 20, NoDateFilter: true},
		"the probe":    {WindowSize: 5, BodyType: BodyTypePlain, BodyTruncationSize: 1, NoDateFilter: true},
		"every option": {WindowSize: 100, DateFilter: FilterOneMonth, MIMESupport: 1, MIMETruncation: 4, ConversationMode: true, RightsManagementSupport: true, BodyPartPreviewBytes: 512},
	} {
		for _, stored := range []string{"", "0"} {
			got := syncRequestOf(t, stored, opts)
			if !bytes.Equal(got, want) {
				t.Errorf("%s (stored key %q):\n got % X\nwant % X", name, stored, got, want)
			}
		}
	}
	// The bootstrap that SyncEmail runs by itself (no NoBootstrap) sends that
	// request first and the full one with the key it got.
	var bodies [][]byte
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
		w.Write(syncAck(t, "K"+strconv.Itoa(len(bodies))))
	})
	if _, err := c.SyncEmail(context.Background(), "5", EmailSyncOptions{WindowSize: 7}); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || !bytes.Equal(bodies[0], want) {
		t.Fatalf("%d requests; first % X", len(bodies), bodies[0])
	}
	second := parseRequest(t, bodies[1])
	if k, _ := findText(second, "SyncKey"); k != "K1" {
		t.Errorf("second request uses key %q", k)
	}
	if w, _ := findText(second, "WindowSize"); w != "7" {
		t.Errorf("WindowSize = %q", w)
	}
}

func TestPatch12_theRequestWithTheIssuedKeyCarriesTheRestInTheOrderOfTheSpecification(t *testing.T) {
	req := parseRequest(t, syncRequestOf(t, "K1", EmailSyncOptions{
		WindowSize: 5, BodyType: BodyTypeMIME, MIMESupport: 2, MIMETruncation: 3, BodyTruncationSize: 4096, DateFilter: FilterTwoWeek,
		ConversationMode: true, BodyPartPreviewBytes: 256, RightsManagementSupport: true,
	}))
	coll := req.Find("Collection")
	var kids []string
	for _, k := range coll.Children {
		if el, ok := k.(*wbxml.Element); ok {
			kids = append(kids, el.Name)
		}
	}
	// MS-ASCMD 2.2.3.29.2: "a strict ordering of the XML elements within a Collection".
	if got, want := strings.Join(kids, ","), "SyncKey,CollectionId,DeletesAsMoves,GetChanges,WindowSize,ConversationMode,Options"; got != want {
		t.Errorf("Collection children = %s, want %s", got, want)
	}
	if v, _ := findText(coll, "GetChanges"); v != "1" {
		t.Errorf("GetChanges = %q: the data request asks for the changes", v)
	}
	var opts []string
	for _, k := range coll.Find("Options").Children {
		if el, ok := k.(*wbxml.Element); ok {
			opts = append(opts, el.Name)
		}
	}
	// FilterType, then what the examples of MS-ASCMD 4.5.7 show: BodyPreference before MIMESupport.
	if got, want := strings.Join(opts, ","), "FilterType,BodyPreference,BodyPartPreference,MIMESupport,MIMETruncation,RightsManagementSupport"; got != want {
		t.Errorf("Options children = %s, want %s", got, want)
	}
	bp := coll.Find("BodyPreference")
	var bpk []string
	for _, k := range bp.Children {
		if el, ok := k.(*wbxml.Element); ok {
			bpk = append(bpk, el.Name)
		}
	}
	// MS-ASAIRS 2.2.2.12: Type, then TruncationSize.
	if strings.Join(bpk, ",") != "Type,TruncationSize" {
		t.Errorf("BodyPreference children = %v", bpk)
	}
}

// ---- patch 13: a tag the tables do not know does not fail the response ----------

func TestPatch13_aSyncResponseWithATagTheTablesDoNotKnowIsStillRead(t *testing.T) {
	// <Sync><Collections><Collection><SyncKey>S2</SyncKey><CollectionId>5</CollectionId><Status>1</Status>
	//  <Commands><Add><ServerId>5:1</ServerId><ApplicationData>
	//    <email:Read>1</email:Read>
	//    [page 17, token 0x3E: not in any table, with a child]
	//    [page 99, token 0x08: not a registered page]
	//    <email:Importance>2</email:Importance>
	//  </ApplicationData></Add></Commands></Collection></Collections></Sync>
	raw := wbxmlCat(
		[]byte{0x03, 0x01, 0x6A, 0x00},
		[]byte{0x45, 0x5C, 0x4F},                   // Sync, Collections, Collection
		[]byte{0x4B}, wbxmlStr("S2"), []byte{0x01}, // SyncKey
		[]byte{0x52}, wbxmlStr("5"), []byte{0x01}, // CollectionId
		[]byte{0x4E}, wbxmlStr("1"), []byte{0x01}, // Status
		[]byte{0x56, 0x47},                          // Commands, Add
		[]byte{0x4D}, wbxmlStr("5:1"), []byte{0x01}, // ServerId
		[]byte{0x5D},                                          // ApplicationData
		[]byte{0x00, 0x02, 0x55}, wbxmlStr("1"), []byte{0x01}, // page 2: Read (0x15)
		[]byte{0x00, 0x11, 0x7E}, wbxmlStr("unknown"), []byte{0x46}, wbxmlStr("x"), []byte{0x01, 0x01}, // page 17: unknown 0x3E with a Type child
		[]byte{0x00, 0x63, 0x08},                              // page 99: unknown, empty
		[]byte{0x00, 0x02, 0x52}, wbxmlStr("2"), []byte{0x01}, // page 2: Importance (0x12)
		[]byte{0x01, 0x01, 0x01, 0x01, 0x01, 0x01}, // END ApplicationData, Add, Commands, Collection, Collections, Sync
	)
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.ms-sync.wbxml")
		w.Write(raw)
	})
	_ = c.cfg.State.SetSyncKey(context.Background(), "5", "S1")
	res, err := c.SyncEmail(context.Background(), "5", EmailSyncOptions{NoBootstrap: true})
	if err != nil {
		t.Fatalf("the whole response failed for one unknown tag: %v", err)
	}
	if res.SyncKey != "S2" || len(res.Added) != 1 || !res.Added[0].Read || res.Added[0].Importance != 2 || res.Added[0].ServerID != "5:1" {
		t.Errorf("the known elements were not read: %+v", res)
	}
	// A client that is handed a strict registry keeps the old behaviour.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(raw) }))
	defer srv.Close()
	strict, err := NewClient(Config{ServerURL: srv.URL + "/x", Username: "u", Password: "p", DeviceID: "d", State: NewMemoryState(), Registry: wbxml.DefaultRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	_ = strict.(*httpClient).cfg.State.SetSyncKey(context.Background(), "5", "S1")
	if _, err := strict.SyncEmail(context.Background(), "5", EmailSyncOptions{NoBootstrap: true}); err == nil || !strings.Contains(err.Error(), "unknown tag") {
		t.Errorf("a strict registry must still fail on an unknown tag: %v", err)
	}
}

// ---- patch 14: a MIME body that arrives as an inline string ------------------

// A message with raw windows-1256 bytes in its body: byte transparency matters,
// the inline string is not UTF-8.
const patch14MIME = "Subject: =?windows-1256?B?x+Ha0cjtyQ==?=\r\nContent-Type: text/plain; charset=windows-1256\r\n" +
	"Content-Transfer-Encoding: 8bit\r\n\r\n\xc7\xe1\xda\xd1\xc8\xed\xc9\r\n"

// realBodyElement builds airsyncbase:Body the way the trace of real-Exchange
// finding 3 had it: Type, EstimatedDataSize, Data.
func realBodyElement(typ string, truncated bool, data wbxml.Node, size int) *wbxml.Element {
	b := wbxml.E(wbxml.PageAirSyncBase, "Body",
		wbxml.E(wbxml.PageAirSyncBase, "Type", wbxml.Text(typ)),
		wbxml.E(wbxml.PageAirSyncBase, "EstimatedDataSize", wbxml.Text(strconv.Itoa(size))))
	if truncated {
		b.Children = append(b.Children, wbxml.E(wbxml.PageAirSyncBase, "Truncated", wbxml.Text("1")))
	}
	if data != nil {
		b.Children = append(b.Children, wbxml.E(wbxml.PageAirSyncBase, "Data", data))
	} else {
		b.Children = append(b.Children, wbxml.E(wbxml.PageAirSyncBase, "Data"))
	}
	return b
}

// realProperties is the property list of one message in the order Exchange 2019
// used in the trace (synthetic values): the headers, Body, then MessageClass,
// InternetCPID, an empty Flag, ContentClass, NativeBodyType, ConversationId and
// ConversationIndex as binary, an empty Categories and IsDraft. withHeaders adds
// the Cc, ReplyTo and Attachments that the Sync Add of the trace had.
func realProperties(body *wbxml.Element, withExtras bool) []*wbxml.Element {
	em := func(name, v string) *wbxml.Element { return wbxml.E(wbxml.PageEmail, name, wbxml.Text(v)) }
	var out []*wbxml.Element
	out = append(out, em("To", `"Alice Example" <alice@example.test>`))
	if withExtras {
		out = append(out, em("Cc", `carol@example.test`))
	}
	out = append(out, em("From", `"Bob Builder" <bob@example.test>`))
	if withExtras {
		out = append(out, em("ReplyTo", `bob@example.test`))
	}
	out = append(out, em("Subject", "Quarterly report"), em("DateReceived", "2026-10-01T10:00:00.000Z"),
		em("DisplayTo", "Alice Example"), em("ThreadTopic", "Quarterly report"), em("Importance", "1"), em("Read", "0"))
	if withExtras {
		out = append(out, wbxml.E(wbxml.PageAirSyncBase, "Attachments",
			wbxml.E(wbxml.PageAirSyncBase, "Attachment",
				wbxml.E(wbxml.PageAirSyncBase, "DisplayName", wbxml.Text("logo.png")),
				wbxml.E(wbxml.PageAirSyncBase, "FileReference", wbxml.Text("5%3a42%3a0")),
				wbxml.E(wbxml.PageAirSyncBase, "Method", wbxml.Text("1")),
				wbxml.E(wbxml.PageAirSyncBase, "EstimatedDataSize", wbxml.Text("2048")),
				wbxml.E(wbxml.PageAirSyncBase, "ContentId", wbxml.Text("logo@example.test")),
				wbxml.E(wbxml.PageAirSyncBase, "IsInline", wbxml.Text("1")))))
	}
	out = append(out, body,
		em("MessageClass", "IPM.Note"), em("InternetCPID", "65001"), wbxml.E(wbxml.PageEmail, "Flag"),
		em("ContentClass", "urn:content-classes:message"),
		wbxml.E(wbxml.PageAirSyncBase, "NativeBodyType", wbxml.Text("2")),
		wbxml.E(wbxml.PageEmail2, "ConversationId", wbxml.Opaque("0123456789abcdef")),
		wbxml.E(wbxml.PageEmail2, "ConversationIndex", wbxml.Opaque("\x01\xd6\x9a\x01\x02\x03\x04\x05\x06\x07\x08")),
		wbxml.E(wbxml.PageEmail, "Categories"),
		wbxml.E(wbxml.PageEmail2, "IsDraft", wbxml.Text("0")))
	return out
}

// realFetchResponse is the ItemOperations response of the trace: Class in front of
// Properties, a 60 byte ServerId.
func realFetchResponse(t *testing.T, serverID string, props []*wbxml.Element) []byte {
	t.Helper()
	p := wbxml.E(wbxml.PageItemOperations, "Properties")
	for _, k := range props {
		p.Children = append(p.Children, k)
	}
	return wbxmlBytes(t, wbxml.E(wbxml.PageItemOperations, "ItemOperations",
		wbxml.E(wbxml.PageItemOperations, "Status", wbxml.Text("1")),
		wbxml.E(wbxml.PageItemOperations, "Response",
			wbxml.E(wbxml.PageItemOperations, "Fetch",
				wbxml.E(wbxml.PageItemOperations, "Status", wbxml.Text("1")),
				wbxml.E(wbxml.PageAirSync, "CollectionId", wbxml.Text("7")),
				wbxml.E(wbxml.PageAirSync, "ServerId", wbxml.Text(serverID)),
				wbxml.E(wbxml.PageAirSync, "Class", wbxml.Text("Email")),
				p))))
}

func TestPatch14_aFetchedMIMEBodyThatArrivesAsAnInlineStringIsTheMIME(t *testing.T) {
	// Exchange 2019 sends the MIME of a Type 4 body as an inline string (STR_I,
	// 0x03), not as OPAQUE (0xC3). The library took only an OPAQUE chunk for MIME,
	// put the text in Body and left BodyMIME empty: "item has no MIME body".
	sid := "7:" + strings.Repeat("A", 58)
	wire := realFetchResponse(t, sid, realProperties(realBodyElement("4", false, wbxml.Text(patch14MIME), len(patch14MIME)), false))
	if !bytes.Contains(wire, append(append([]byte{0x03}, patch14MIME...), 0x00)) {
		t.Fatal("the test response does not carry the MIME as an inline string")
	}
	c, cap, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.ms-sync.wbxml")
		w.Write(wire)
	})
	item, err := c.FetchEmail(context.Background(), "7", sid, FetchEmailOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if item.BodyType != BodyTypeMIME || string(item.BodyMIME) != patch14MIME {
		t.Fatalf("BodyType %v, BodyMIME %q (len %d), want the %d bytes of the message", item.BodyType, item.BodyMIME, len(item.BodyMIME), len(patch14MIME))
	}
	if item.Body != "" {
		t.Errorf("the MIME must not also be in Body: %q", item.Body)
	}
	// Everything else in Properties is read as well.
	if item.ServerID != sid || item.Subject != "Quarterly report" || item.From != `"Bob Builder" <bob@example.test>` ||
		item.To != `"Alice Example" <alice@example.test>` || item.DisplayTo != "Alice Example" || item.ThreadTopic != "Quarterly report" ||
		item.Importance != 1 || !item.ReadSet || item.Read || item.MessageClass != "IPM.Note" ||
		!item.FlagSet || item.FlagStatus != 0 || len(item.Categories) != 0 || string(item.ConversationID) != "0123456789abcdef" ||
		item.DateReceived.IsZero() || item.BodyEstimatedSize != len(patch14MIME) || item.HasAttachments {
		t.Errorf("properties: %+v", item)
	}
	// The request was the one the trace says: Store=Mailbox, CollectionId, ServerId,
	// Options{MIMESupport=2, BodyPreference{Type=4}}.
	req := parseRequest(t, cap.body)
	if v, _ := findText(req, "Store"); v != "Mailbox" {
		t.Errorf("Store = %q", v)
	}
	if v, _ := findText(req, "MIMESupport"); v != "2" {
		t.Errorf("MIMESupport = %q", v)
	}
	if v, _ := findText(req.Find("BodyPreference"), "Type"); v != "4" {
		t.Errorf("BodyPreference/Type = %q", v)
	}
}

func TestPatch14_theMIMEBodyAsOpaqueIsStillTheMIME(t *testing.T) {
	sid := "7:1"
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write(realFetchResponse(t, sid, realProperties(realBodyElement("4", false, wbxml.Opaque(patch14MIME), len(patch14MIME)), false)))
	})
	item, err := c.FetchEmail(context.Background(), "7", sid, FetchEmailOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if item.BodyType != BodyTypeMIME || string(item.BodyMIME) != patch14MIME || item.Body != "" {
		t.Errorf("BodyType %v, BodyMIME %q, Body %q", item.BodyType, item.BodyMIME, item.Body)
	}
}

func TestPatch14_bodyContentIsJoinedInOrderAndTheOrderOfTheElementsDoesNotMatter(t *testing.T) {
	data := func(kids ...wbxml.Node) *wbxml.Element { return wbxml.E(wbxml.PageAirSyncBase, "Data", kids...) }
	body := func(typ string, parts ...*wbxml.Element) *wbxml.Element {
		b := wbxml.E(wbxml.PageAirSyncBase, "Body")
		for _, p := range parts {
			b.Children = append(b.Children, p)
		}
		_ = typ
		return b
	}
	typeEl := wbxml.E(wbxml.PageAirSyncBase, "Type", wbxml.Text("4"))
	app := func(b *wbxml.Element) *wbxml.Element {
		return wbxml.E(wbxml.PageAirSync, "ApplicationData", b)
	}
	cases := []struct {
		name string
		b    *wbxml.Element
		want string
	}{
		{"two inline strings", body("4", typeEl, data(wbxml.Text("Subject: x\r\n"), wbxml.Text("\r\nbody"))), "Subject: x\r\n\r\nbody"},
		{"inline string, then OPAQUE", body("4", typeEl, data(wbxml.Text("Subject: x\r\n"), wbxml.Opaque("\r\nbody"))), "Subject: x\r\n\r\nbody"},
		{"two OPAQUE chunks (upstream took the first one only)", body("4", typeEl, data(wbxml.Opaque("Subject: x\r\n"), wbxml.Opaque("\r\nbody"))), "Subject: x\r\n\r\nbody"},
		{"Data in front of Type", body("4", data(wbxml.Text("Subject: x\r\n\r\nbody")), typeEl), "Subject: x\r\n\r\nbody"},
		{"OPAQUE without a Type", body("", data(wbxml.Opaque("Subject: x\r\n\r\nbody"))), "Subject: x\r\n\r\nbody"},
	}
	for _, tc := range cases {
		got := parseEmailItem("1", app(tc.b))
		if string(got.BodyMIME) != tc.want || got.Body != "" {
			t.Errorf("%s: BodyMIME %q, Body %q, want BodyMIME %q", tc.name, got.BodyMIME, got.Body, tc.want)
		}
	}
	// The slice belongs to the item: changing it must not change the tree it came from.
	d := data(wbxml.Text("Subject: x\r\n\r\nbody"))
	item := parseEmailItem("1", app(body("4", typeEl, d)))
	if len(item.BodyMIME) == 0 {
		t.Fatal("no MIME")
	}
	item.BodyMIME[0] = 'X'
	if string(d.Children[0].(wbxml.Text)) != "Subject: x\r\n\r\nbody" {
		t.Error("BodyMIME shares memory with the decoded tree")
	}
	// An empty Data element, with or without content, gives no MIME.
	for _, d := range []*wbxml.Element{data(), data(wbxml.Text("")), data(wbxml.Opaque(nil))} {
		if got := parseEmailItem("1", app(body("4", typeEl, d))); len(got.BodyMIME) != 0 || got.Body != "" {
			t.Errorf("empty Data: %+v", got)
		}
	}
}

func TestPatch14_otherBodyTypesKeepTheirTextInBody(t *testing.T) {
	for typ, name := range map[string]string{"1": "plain", "2": "HTML", "3": "RTF"} {
		app := wbxml.E(wbxml.PageAirSync, "ApplicationData", realBodyElement(typ, true, wbxml.Text("hello <b>world</b>\r\n"), 40))
		got := parseEmailItem("1", app)
		if got.Body != "hello <b>world</b>\r\n" || got.BodyMIME != nil || got.BodyType != BodyType(atoi(typ)) || !got.BodyTruncated {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

func TestPatch14_aSyncAddWithAttachmentsAndAPlainTextBodyIsRead(t *testing.T) {
	// The Sync Add of the real trace: a message with an inline attachment, a
	// plain-text body cut to the size asked for (Type 1, Truncated 1), binary
	// ConversationId and ConversationIndex, empty Flag and Categories, IsDraft.
	add := wbxml.E(wbxml.PageAirSync, "Add", wbxml.E(wbxml.PageAirSync, "ServerId", wbxml.Text("5:77")))
	app := wbxml.E(wbxml.PageAirSync, "ApplicationData")
	for _, k := range realProperties(realBodyElement("1", true, wbxml.Text("h"), 4096), true) {
		app.Children = append(app.Children, k)
	}
	add.Children = append(add.Children, app)
	raw := wbxmlBytes(t, wbxml.E(wbxml.PageAirSync, "Sync", wbxml.E(wbxml.PageAirSync, "Collections",
		wbxml.E(wbxml.PageAirSync, "Collection",
			wbxml.E(wbxml.PageAirSync, "SyncKey", wbxml.Text("S2")),
			wbxml.E(wbxml.PageAirSync, "CollectionId", wbxml.Text("5")),
			wbxml.E(wbxml.PageAirSync, "Status", wbxml.Text("1")),
			wbxml.E(wbxml.PageAirSync, "Commands", add)))))
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.ms-sync.wbxml")
		w.Write(raw)
	})
	_ = c.cfg.State.SetSyncKey(context.Background(), "5", "S1")
	res, err := c.SyncEmail(context.Background(), "5", EmailSyncOptions{NoBootstrap: true, BodyType: BodyTypePlain, BodyTruncationSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.SyncKey != "S2" || len(res.Added) != 1 {
		t.Fatalf("result: %+v", res)
	}
	it := res.Added[0]
	if it.ServerID != "5:77" || it.Subject != "Quarterly report" || it.To != `"Alice Example" <alice@example.test>` ||
		it.Cc != "carol@example.test" || it.ReplyTo != "bob@example.test" || it.From != `"Bob Builder" <bob@example.test>` ||
		it.DisplayTo != "Alice Example" || it.ThreadTopic != "Quarterly report" || it.Importance != 1 ||
		!it.ReadSet || it.Read || it.DateReceived.IsZero() || it.MessageClass != "IPM.Note" {
		t.Errorf("headers: %+v", it)
	}
	if !it.HasAttachments {
		t.Error("the Attachments element was not noticed")
	}
	if it.BodyType != BodyTypePlain || it.Body != "h" || !it.BodyTruncated || it.BodyEstimatedSize != 4096 || it.BodyMIME != nil {
		t.Errorf("body: type %v, %q, truncated %v, size %d, mime %q", it.BodyType, it.Body, it.BodyTruncated, it.BodyEstimatedSize, it.BodyMIME)
	}
	if !it.FlagSet || it.FlagStatus != 0 || len(it.Categories) != 0 || string(it.ConversationID) != "0123456789abcdef" {
		t.Errorf("flag %v/%d, categories %v, conversation %q", it.FlagSet, it.FlagStatus, it.Categories, it.ConversationID)
	}
}

func TestPatch14_aSyncAddWhoseMIMEBodyIsAnInlineStringIsTheMIME(t *testing.T) {
	// The same defect on the Sync path: with Type 4 and MIMESupport 2 the Add
	// items carry the (possibly truncated) message.
	add := wbxml.E(wbxml.PageAirSync, "Add", wbxml.E(wbxml.PageAirSync, "ServerId", wbxml.Text("5:78")))
	app := wbxml.E(wbxml.PageAirSync, "ApplicationData")
	for _, k := range realProperties(realBodyElement("4", true, wbxml.Text(patch14MIME[:40]), len(patch14MIME)), true) {
		app.Children = append(app.Children, k)
	}
	add.Children = append(add.Children, app)
	raw := wbxmlBytes(t, wbxml.E(wbxml.PageAirSync, "Sync", wbxml.E(wbxml.PageAirSync, "Collections",
		wbxml.E(wbxml.PageAirSync, "Collection",
			wbxml.E(wbxml.PageAirSync, "SyncKey", wbxml.Text("S2")),
			wbxml.E(wbxml.PageAirSync, "CollectionId", wbxml.Text("5")),
			wbxml.E(wbxml.PageAirSync, "Status", wbxml.Text("1")),
			wbxml.E(wbxml.PageAirSync, "Commands", add)))))
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { w.Write(raw) })
	_ = c.cfg.State.SetSyncKey(context.Background(), "5", "S1")
	res, err := c.SyncEmail(context.Background(), "5", EmailSyncOptions{NoBootstrap: true, BodyType: BodyTypeMIME, MIMESupport: 2, BodyTruncationSize: 40})
	if err != nil {
		t.Fatal(err)
	}
	it := res.Added[0]
	if it.BodyType != BodyTypeMIME || string(it.BodyMIME) != patch14MIME[:40] || !it.BodyTruncated || it.BodyEstimatedSize != len(patch14MIME) {
		t.Errorf("type %v, MIME %q, truncated %v, size %d", it.BodyType, it.BodyMIME, it.BodyTruncated, it.BodyEstimatedSize)
	}
}

// ---- patch 15: the first request of the Contacts, Tasks and Notes sync -----------

func genericRequestOf(t *testing.T, storedKey string) []byte {
	t.Helper()
	c, cap, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write(syncAck(t, "NEWKEY"))
	})
	if storedKey != "" {
		_ = c.cfg.State.SetSyncKey(context.Background(), "9", storedKey)
	}
	if _, err := c.SyncContacts(context.Background(), "9"); err != nil {
		t.Fatal(err)
	}
	return cap.body
}

func TestPatch15_theFirstContactsRequestHoldsTheKeyAndTheCollectionAndNothingElse(t *testing.T) {
	want := wbxmlCat(
		[]byte{0x03, 0x01, 0x6A, 0x00},
		[]byte{0x45, 0x5C, 0x4F},
		[]byte{0x4B}, wbxmlStr("0"), []byte{0x01}, // SyncKey
		[]byte{0x52}, wbxmlStr("9"), []byte{0x01}, // CollectionId
		[]byte{0x01, 0x01, 0x01},
	)
	for _, stored := range []string{"", "0"} {
		if got := genericRequestOf(t, stored); !bytes.Equal(got, want) {
			t.Errorf("stored key %q:\n got % X\nwant % X", stored, got, want)
		}
	}
}

func TestPatch15_theRequestWithTheIssuedKeyStillCarriesTheRest(t *testing.T) {
	req := parseRequest(t, genericRequestOf(t, "K1"))
	coll := req.Find("Collection")
	var kids []string
	for _, k := range coll.Children {
		if el, ok := k.(*wbxml.Element); ok {
			kids = append(kids, el.Name)
		}
	}
	if got, want := strings.Join(kids, ","), "SyncKey,CollectionId,DeletesAsMoves,GetChanges,WindowSize"; got != want {
		t.Errorf("Collection children = %s, want %s", got, want)
	}
	if v, _ := findText(coll, "GetChanges"); v != "1" {
		t.Errorf("GetChanges = %q", v)
	}
	if v, _ := findText(coll, "WindowSize"); v != "100" {
		t.Errorf("WindowSize = %q", v)
	}
}

func TestPatch15_tasksAndNotesShareTheFix(t *testing.T) {
	for name, call := range map[string]func(c *httpClient) error{
		"tasks": func(c *httpClient) error { _, err := c.SyncTasks(context.Background(), "9"); return err },
		"notes": func(c *httpClient) error { _, err := c.SyncNotes(context.Background(), "9"); return err },
	} {
		c, cap, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { w.Write(syncAck(t, "NEWKEY")) })
		if err := call(c); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, ok := findText(parseRequest(t, cap.body), "GetChanges"); ok {
			t.Errorf("%s: GetChanges in the request for a first key", name)
		}
	}
}
