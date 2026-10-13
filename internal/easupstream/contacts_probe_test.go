package easupstream_test

import (
	"bytes"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hstern/go-activesync/wbxml"

	"easbridge/internal/easupstream"
	"easbridge/internal/testgal"
)

// contactsProbeFixture is a server with a mailbox, a default Contacts folder and a
// user contacts folder, and a directory that holds the account itself.
func contactsProbeFixture(t *testing.T, withDefault bool) (*fixture, *[]time.Time, *[]string) {
	f := newFixture(t)
	folders := []*wbxml.Element{folderAdd("5", "0", "Inbox", 2), folderAdd("8", "0", "Private Family Clients", 14)}
	if withDefault {
		folders = append(folders, folderAdd("9", "0", "Private Family Contacts", 9))
	}
	f.fake.on("FolderSync", always(0, folderSyncResp("h1", folders...)))
	f.fake.on("Sync", func(r *http.Request, in *wbxml.Element) (int, *wbxml.Element) {
		fid, _ := text(in, "CollectionId")
		key, _ := text(in, "SyncKey")
		if key == "0" {
			return 0, syncResp(fid, "k1", "1")
		}
		if fid == "9" {
			return 0, syncResp(fid, "k2", "1", commands(
				contactAdd("9:1", map[string]string{"FirstName": "عبدالله", "LastName": "القحطاني", "Email1Address": "private.person@example.test",
					"MobilePhoneNumber": "+1 555 050 4567", "CompanyName": "الشركة"}),
				contactAdd("9:2", map[string]string{"FirstName": "Zed", "BusinessPhoneNumber": "+1 555 011 0000"})))
		}
		return 0, syncResp(fid, "k2", "1")
	})
	f.fake.on("Ping", always(0, pingResp("1")))
	var mu sync.Mutex
	var times []time.Time
	var queries []string
	people := []galPerson{{display: "Alice Own", first: "Alice", last: "Own", email: user, alias: "aown", home: "+1 555 055 9999", office: "Tower B", phone: "+1 555 011 7777"},
		{display: "Alice Other", first: "Alice", last: "Other", email: "alice.other@example.test"}}
	inner := galSearchHandler(people, true, &queries)
	f.fake.on("Search", func(r *http.Request, in *wbxml.Element) (int, *wbxml.Element) {
		mu.Lock()
		times = append(times, time.Now())
		mu.Unlock()
		return inner(r, in)
	})
	return f, &times, &queries
}

func runContactsProbe(f *fixture, mod func(*easupstream.ProbeConfig)) (string, error) {
	var out bytes.Buffer
	pc := easupstream.ProbeConfig{
		Config: easupstream.Config{URL: f.fake.url(), HTTPClient: f.fake.srv.Client(), Version: "16.1", UserAgent: "easbridge/probe-test"},
		User:   user, Password: pass, DeviceID: dev, Out: &out, SearchSpacing: 120 * time.Millisecond,
	}
	if mod != nil {
		mod(&pc)
	}
	err := easupstream.Probe(ctx, pc)
	return out.String(), err
}

func TestProbeHasAContactsStepAndADirectoryStepThatPrintNoValues(t *testing.T) {
	f, times, queries := contactsProbeFixture(t, true)
	got, err := runContactsProbe(f, nil)
	if err != nil {
		t.Fatalf("probe: %v\n%s", err, got)
	}
	for _, want := range []string{
		"contacts     ok      contact folders: type9=1 type14=1",
		"ctc boot", "key issued", "ctc sync", "items=2 changed=0 deleted=0 more=false fields=[BusinessPhoneNumber,CompanyName,Email1Address,FirstName,LastName,MobilePhoneNumber]",
		"gal address", "total=1 entries=1 exact_match=true fields=[Alias,DisplayName,EmailAddress,FirstName,HomePhone,LastName,Office,Phone]",
		"gal local", "total=2 entries=2 exact_match=true", "probe finished",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{"عبدالله", "القحطاني", "الشركة", "private.person", "+1 555", "Zed", "Alice", "Own", user, "alice", "Tower B", pass, "Private Family", "aown"} {
		if strings.Contains(got, bad) {
			t.Errorf("the report shows %q:\n%s", bad, got)
		}
	}
	if strings.Join(*queries, "|") != user+"|alice|a|al" {
		t.Errorf("queries %q: the account's own address, its local part, then the first 1 and the first 2 characters of it", *queries)
	}
	if len(*times) == 4 {
		for i := 1; i < 4; i++ {
			if gap := (*times)[i].Sub((*times)[i-1]); gap < 110*time.Millisecond {
				t.Errorf("searches %d and %d are %v apart; the service keeps them apart too", i-1, i, gap)
			}
		}
	}
	for _, want := range []string{"gal prefix 1 ok      total=2 entries=2 own_entry=true", "gal prefix 2 ok      total=2 entries=2 own_entry=true"} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q:\n%s", want, got)
		}
	}
	// The contacts Sync is the first request with the key and the folder, then the data call.
	var contactSyncs []req
	for _, r := range f.fake.requests("Sync") {
		if c, _ := text(r.Root, "CollectionId"); c == "9" {
			contactSyncs = append(contactSyncs, r)
		}
	}
	if len(contactSyncs) != 2 {
		t.Fatalf("%d contact syncs", len(contactSyncs))
	}
	if n := len(kids(contactSyncs[0].Root.Find("Collection"))); n != 2 {
		t.Errorf("the first request holds %d children, want SyncKey and CollectionId", n)
	}
	// The model accepted everything (the cleanup checks that no request was refused), and nothing was written.
	if strings.Contains(got, "FAILED") {
		t.Errorf("a step failed:\n%s", got)
	}
}

func TestProbeSkipContactsLeavesBothStepsOut(t *testing.T) {
	f, _, queries := contactsProbeFixture(t, true)
	got, err := runContactsProbe(f, func(pc *easupstream.ProbeConfig) { pc.SkipContacts = true })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "contacts") || strings.Contains(got, "gal ") || len(*queries) != 0 || f.fake.count("Search") != 0 {
		t.Errorf("--skip-contacts:\n%s", got)
	}
	for _, r := range f.fake.requests("Sync") {
		if c, _ := text(r.Root, "CollectionId"); c == "9" {
			t.Errorf("the contacts folder was synced")
		}
	}
}

func TestProbeWithoutADefaultContactsFolderSaysSoAndStillTriesTheDirectory(t *testing.T) {
	f, _, queries := contactsProbeFixture(t, false)
	got, err := runContactsProbe(f, nil)
	if err != nil {
		t.Fatalf("%v\n%s", err, got)
	}
	if !strings.Contains(got, "contacts     skipped (no default Contacts folder, type 9, in the hierarchy; type 14 folders: 1)") || len(*queries) != 4 {
		t.Errorf("%d searches\n%s", len(*queries), got)
	}
}

func TestProbeReportsARefusedDirectorySearchAndStillEndsInAnError(t *testing.T) {
	f, _, _ := contactsProbeFixture(t, true)
	f.fake.on("Search", always(0, searchStatusResp("5", false)))
	got, err := runContactsProbe(f, nil)
	if err == nil {
		t.Fatalf("a failing step must make the probe fail:\n%s", got)
	}
	for _, want := range []string{"gal address  FAILED  Search status 5 permanent=true: the server denied access to the directory search (access denied)", "gal local    FAILED", "gal prefix 1 FAILED", "gal prefix 2 FAILED", "ctc sync", "probe finished"} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, user) {
		t.Errorf("the report names the user:\n%s", got)
	}
}

func TestProbeTraceOfTheContactsAndDirectoryStepsHidesTheValues(t *testing.T) {
	f, _, _ := contactsProbeFixture(t, true)
	got, err := runContactsProbe(f, func(pc *easupstream.ProbeConfig) { pc.Trace = true })
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"عبدالله", "القحطاني", "private.person", "+1 555", "Alice", "Tower B", "aown", "Private Family"} {
		if strings.Contains(got, bad) {
			t.Errorf("the trace shows %q", bad)
		}
	}
	for _, want := range []string{"<Name>GAL</Name>", "<Range>0-99</Range>", "<Total>1</Total>", "contacts:FirstName", "gal:EmailAddress", "<Query>[", "<SyncKey>k1</SyncKey>"} {
		if !strings.Contains(got, want) {
			t.Errorf("the trace lacks %q", want)
		}
	}
}

func TestProbePrefixStepsReportAShortQueryTheServerRefusesAndAnAnswerWithoutTheOwnEntry(t *testing.T) {
	f, _, _ := contactsProbeFixture(t, true)
	people := []testgal.Person{{Display: "Alice Other", First: "Alice", Last: "Other", Alias: "alice.other", Email: "alice.other@example.test"},
		{Display: "Alicia Keys", First: "Alicia", Last: "Keys", Alias: "akeys", Email: "akeys@example.test"}}
	var q []string
	f.fake.on("Search", galANRHandler(people, 2, &q, nil))
	got, err := runContactsProbe(f, nil)
	if err == nil {
		t.Fatalf("a refused step ends the probe with an error:\n%s", got)
	}
	for _, want := range []string{
		"gal prefix 1 FAILED  Search status 2 permanent=true: the server rejected the Search request as invalid",
		"gal prefix 2 ok      total=2 entries=2 own_entry=false", "probe finished",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{"Alicia", "Keys", "akeys", user} {
		if strings.Contains(got, bad) {
			t.Errorf("the report shows %q", bad)
		}
	}
}
