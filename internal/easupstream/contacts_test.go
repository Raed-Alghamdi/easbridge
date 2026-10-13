package easupstream_test

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hstern/go-activesync/wbxml"

	"easbridge/internal/upstream"
)

// ---- Contacts Sync -------------------------------------------------------------

func TestSyncContactsFirstRequestIsTheKeyAndTheCollectionAndNothingElse(t *testing.T) {
	f := newFixture(t)
	m := f.open("")
	f.fake.on("Sync", func(*http.Request, *wbxml.Element) (int, *wbxml.Element) { return 0, syncResp("9", "k1", "1") })
	d, err := m.SyncContacts(ctx, "9", "0")
	if err != nil || d.NewKey != "k1" || d.Reset || len(d.Added)+len(d.Changed)+len(d.Deleted) != 0 {
		t.Fatalf("%+v %v", d, err)
	}
	if f.fake.count("Sync") != 1 {
		t.Fatalf("%d requests", f.fake.count("Sync"))
	}
	var names []string
	for _, k := range kids(f.fake.last("Sync").Root.Find("Collection")) {
		names = append(names, k.Name)
	}
	if strings.Join(names, ",") != "SyncKey,CollectionId" {
		t.Errorf("the request for a first SyncKey holds %v (real-Exchange finding 2: anything else is Sync status 4)", names)
	}
	if len(f.fake.all()) > 0 && f.fake.violationsSeen() != 0 {
		t.Errorf("the model refused a request")
	}
}

func TestSyncContactsOldPatchlessRequestIsRefusedByTheModel(t *testing.T) {
	// The model refuses what Exchange refuses: GetChanges with SyncKey 0 is status 4.
	f := newFixture(t)
	f.fake.expectViolations = true
	f.fake.on("Provision", provisionOK("T", "F"))
	req := e(wbxml.PageAirSync, "Sync", e(wbxml.PageAirSync, "Collections", e(wbxml.PageAirSync, "Collection",
		e(wbxml.PageAirSync, "SyncKey", txt("0")), e(wbxml.PageAirSync, "CollectionId", txt("9")),
		e(wbxml.PageAirSync, "DeletesAsMoves", txt("1")), e(wbxml.PageAirSync, "GetChanges", txt("1")), e(wbxml.PageAirSync, "WindowSize", txt("100")))))
	if why := requestViolation("Sync", "16.1", req); why == "" || !strings.Contains(why, "GetChanges") {
		t.Errorf("the model accepts the request the vendored library used to send: %q", why)
	}
}

func TestSyncContactsReadsAddsChangesDeletesAndNamesTheFieldsNotTheValues(t *testing.T) {
	f := newFixture(t)
	m := f.open("")
	photo := base64.StdEncoding.EncodeToString([]byte("not really a jpeg"))
	f.fake.on("Sync", func(r *http.Request, in *wbxml.Element) (int, *wbxml.Element) {
		key, _ := text(in, "SyncKey")
		switch key {
		case "0":
			return 0, syncResp("9", "k1", "1")
		case "k1":
			return 0, syncResp("9", "k2", "1", commands(
				contactAdd("9:1", map[string]string{
					"FirstName": "عبدالله", "LastName": "القحطاني", "MiddleName": "م", "Title": "د.", "Suffix": "PhD", "FileAs": "القحطاني, عبدالله",
					"CompanyName": "الشركة", "Department": "الأنظمة", "JobTitle": "مدير", "OfficeLocation": "Tower B",
					"Email1Address": "abdullah@example.test", "Email2Address": "second@example.test",
					"HomePhoneNumber": "+1 555 011 1111", "BusinessPhoneNumber": "+1 555 011 2222", "MobilePhoneNumber": "+1 555 050 3333",
					"HomeAddressStreet": "1 Home St", "HomeAddressCity": "Jeddah", "BusinessAddressCountry": "SA",
					"Birthday": "1990-05-15T00:00:00.000Z", "Anniversary": "2015-01-02T00:00:00.000Z", "WebPage": "https://example.test",
					"Picture": photo,
				},
					e(wbxml.PageContacts, "Categories", e(wbxml.PageContacts, "Category", txt("VIP")), e(wbxml.PageContacts, "Category", txt("Family"))),
					e(wbxml.PageContacts2, "NickName", txt("Abu")),
					e(wbxml.PageContacts2, "GovernmentId", txt("1234567890")),
					e(wbxml.PageContacts2, "CustomerId", txt("C-77")),
					e(wbxml.PageContacts2, "IMAddress", txt("im@example.test")),
					e(wbxml.PageContacts2, "ManagerName", txt("Boss"))),
				e(wbxml.PageAirSync, "Change", e(wbxml.PageAirSync, "ServerId", txt("9:2")),
					e(wbxml.PageAirSync, "ApplicationData", contactEl("LastName", "Changed"))),
				e(wbxml.PageAirSync, "Delete", e(wbxml.PageAirSync, "ServerId", txt("9:3"))),
				e(wbxml.PageAirSync, "SoftDelete", e(wbxml.PageAirSync, "ServerId", txt("9:4"))),
			), e(wbxml.PageAirSync, "MoreAvailable"))
		}
		return 0, syncResp("9", key, "3")
	})
	if _, err := m.SyncContacts(ctx, "9", "0"); err != nil {
		t.Fatal(err)
	}
	d, err := m.SyncContacts(ctx, "9", "k1")
	if err != nil {
		t.Fatal(err)
	}
	if d.NewKey != "k2" || !d.MoreAvailable || d.Reset || len(d.Added) != 1 || len(d.Changed) != 1 || len(d.Deleted) != 2 {
		t.Fatalf("delta %+v", d)
	}
	c := d.Added[0]
	if c.ServerID != "9:1" || c.FirstName != "عبدالله" || c.LastName != "القحطاني" || c.MiddleName != "م" || c.Title != "د." || c.Suffix != "PhD" ||
		c.FileAs != "القحطاني, عبدالله" || c.Company != "الشركة" || c.Department != "الأنظمة" || c.JobTitle != "مدير" || c.Office != "Tower B" ||
		c.Email1 != "abdullah@example.test" || c.Email2 != "second@example.test" || c.HomePhone != "+1 555 011 1111" ||
		c.BusinessPhone != "+1 555 011 2222" || c.MobilePhone != "+1 555 050 3333" || c.HomeAddress.Street != "1 Home St" ||
		c.HomeAddress.City != "Jeddah" || c.BusinessAddress.Country != "SA" || c.WebPage != "https://example.test" || c.NickName != "Abu" ||
		!c.Birthday.Equal(time.Date(1990, 5, 15, 0, 0, 0, 0, time.UTC)) || !c.Anniversary.Equal(time.Date(2015, 1, 2, 0, 0, 0, 0, time.UTC)) ||
		strings.Join(c.Categories, ",") != "VIP,Family" {
		t.Errorf("contact: %+v", c)
	}
	if ch := d.Changed[0]; ch.ServerID != "9:2" || ch.LastName != "Changed" {
		t.Errorf("change: %+v", ch)
	}
	if strings.Join(d.Deleted, ",") != "9:3,9:4" {
		t.Errorf("deleted: %v", d.Deleted)
	}
	// The probe's list names fields and holds no value.
	fields := strings.Join(d.Fields, ",")
	for _, want := range []string{"FirstName", "Email1Address", "Picture", "GovernmentId", "CustomerId", "Birthday", "NickName"} {
		if !strings.Contains(fields, want) {
			t.Errorf("Fields lacks %s: %s", want, fields)
		}
	}
	for _, secret := range []string{"عبدالله", "example.test", "+1 555", "Jeddah", "1234567890", "C-77", "Tower B"} {
		if strings.Contains(fields, secret) {
			t.Errorf("Fields holds a value: %q", secret)
		}
	}
}

func TestSyncContactsInvalidKeyRestartsTheChain(t *testing.T) {
	f := newFixture(t)
	m := f.open("")
	keys := []string{}
	f.fake.on("Sync", func(r *http.Request, in *wbxml.Element) (int, *wbxml.Element) {
		key, _ := text(in, "SyncKey")
		keys = append(keys, key)
		switch key {
		case "0":
			return 0, syncResp("9", "boot", "1", commands(contactAdd("9:1", map[string]string{"FirstName": "ignored"})))
		case "old":
			return 0, syncResp("9", "", "3")
		}
		return 0, syncResp("9", key, "3")
	})
	d, err := m.SyncContacts(ctx, "9", "old")
	if err != nil {
		t.Fatal(err)
	}
	if !d.Reset || d.NewKey != "boot" || len(d.Added)+len(d.Changed)+len(d.Deleted) != 0 {
		t.Errorf("a reset carries the bootstrap key and no items: %+v", d)
	}
	if strings.Join(keys, ",") != "old,0" {
		t.Errorf("requests with keys %v", keys)
	}
}

func TestSyncContactsErrorMapping(t *testing.T) {
	f := newFixture(t)
	m := f.open("")
	var status string
	f.fake.on("Sync", func(*http.Request, *wbxml.Element) (int, *wbxml.Element) { return 0, syncResp("9", "k", status) })
	_, err := m.SyncContacts(ctx, "9", "k1")
	status = "12"
	if _, err = m.SyncContacts(ctx, "9", "k1"); !errors.Is(err, upstream.ErrHierarchyChanged) {
		t.Errorf("status 12: %v", err)
	}
	status = "8"
	var se *upstream.StatusError
	if _, err = m.SyncContacts(ctx, "9", "k1"); !errors.As(err, &se) || se.Code != 8 {
		t.Errorf("status 8: %v", err)
	}
	status = "5"
	if _, err = m.SyncContacts(ctx, "9", "k1"); !errors.As(err, &se) || se.Code != 5 || se.Perm {
		t.Errorf("status 5 is temporary: %v", err)
	}
	status = "3"
	f.fake.on("Sync", func(_ *http.Request, in *wbxml.Element) (int, *wbxml.Element) {
		return 0, syncResp("9", "k", "3") // even after the restart
	})
	if _, err = m.SyncContacts(ctx, "9", "k1"); err == nil {
		t.Errorf("a second status 3 is an error, not a loop")
	}
}

// ---- GAL Search ----------------------------------------------------------------

func TestSearchDirectoryAsksForTheFirstHundredAndReturnsEntriesWithoutHomePhoneOrAlias(t *testing.T) {
	f := newFixture(t)
	m := f.open("")
	var queries []string
	people := []galPerson{{display: "Pat Smith", first: "Pat", last: "Smith", email: "pat@corp.example", alias: "psmith", office: "B2", title: "Engineer",
		company: "Example Co", phone: "222", home: "000", mobile: "333"}}
	f.fake.on("Search", galSearchHandler(people, true, &queries))
	res, err := m.SearchDirectory(ctx, "pat@corp.example")
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 || len(res.Entries) != 1 {
		t.Fatalf("%+v", res)
	}
	en := res.Entries[0]
	if en.DisplayName != "Pat Smith" || en.EmailAddress != "pat@corp.example" || en.Office != "B2" || en.Title != "Engineer" || en.Company != "Example Co" ||
		en.Phone != "222" || en.MobilePhone != "333" || en.FirstName != "Pat" || en.LastName != "Smith" {
		t.Errorf("entry %+v", en)
	}
	if got := strings.Join(res.Fields, ","); !strings.Contains(got, "Alias") || !strings.Contains(got, "HomePhone") || strings.Contains(got, "psmith") {
		t.Errorf("the probe's field list names every field present, the unkept ones too, and no value: %s", got)
	}
	rq := f.fake.last("Search").Root
	if r, _ := text(rq, "Range"); r != "0-99" {
		t.Errorf("Range = %q: the first hundred, once", r)
	}
	if n, _ := text(rq, "Name"); n != "GAL" {
		t.Errorf("store %q", n)
	}
	if strings.Join(queries, "|") != "pat@corp.example" {
		t.Errorf("queries %v", queries)
	}
}

func TestSearchDirectoryReturnsAtMostAHundredAndSaysHowManyMatched(t *testing.T) {
	f := newFixture(t)
	m := f.open("")
	var people []galPerson
	for i := 0; i < 130; i++ {
		people = append(people, galPerson{display: "Sam " + itoa(i), email: "sam" + itoa(i) + "@corp.example"})
	}
	f.fake.on("Search", galSearchHandler(people, true, nil))
	res, err := m.SearchDirectory(ctx, "sam")
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 130 || len(res.Entries) != 100 {
		t.Errorf("total %d, %d entries: the range is clamped to 0-99", res.Total, len(res.Entries))
	}
}

func TestSearchDirectoryNothingFoundIsAnEmptyResult(t *testing.T) {
	f := newFixture(t)
	m := f.open("")
	f.fake.on("Search", galSearchHandler(nil, true, nil))
	res, err := m.SearchDirectory(ctx, "nobody@corp.example")
	if err != nil || res.Total != 0 || len(res.Entries) != 0 {
		t.Errorf("%+v %v", res, err)
	}
	// Status 6 and 12 of the store (not found; the range starts after the last match) are empty too.
	for _, code := range []string{"6", "12"} {
		f.fake.on("Search", always(0, searchStatusResp(code, false)))
		if res, err := m.SearchDirectory(ctx, "x@corp.example"); err != nil || len(res.Entries) != 0 {
			t.Errorf("store status %s: %+v %v", code, res, err)
		}
	}
}

func TestSearchDirectoryStatuses(t *testing.T) {
	f := newFixture(t)
	m := f.open("")
	for _, tc := range []struct {
		code   string
		top    bool
		perm   bool
		denied bool
	}{
		{"2", true, true, false}, {"3", true, false, false}, {"3", false, false, false}, {"5", true, true, true}, {"5", false, true, true},
		{"7", false, false, false}, {"8", false, true, false}, {"10", false, false, false}, {"13", true, true, true}, {"13", false, true, true},
		{"14", false, true, true},
	} {
		f.fake.on("Search", always(0, searchStatusResp(tc.code, tc.top)))
		_, err := m.SearchDirectory(ctx, "pat@corp.example")
		var se *upstream.StatusError
		if !errors.As(err, &se) || se.Op != "Search" || se.Code != atoiT(t, tc.code) || se.Perm != tc.perm || upstream.DirectoryDenied(err) != tc.denied {
			t.Errorf("status %s (top %v): %v", tc.code, tc.top, err)
		}
		if se != nil && se.Reason == "" {
			t.Errorf("status %s has no explanation", tc.code)
		}
	}
	// A common status: the account is blocked.
	f.fake.on("Search", always(0, searchStatusResp("129", true)))
	if _, err := m.SearchDirectory(ctx, "x@corp.example"); err == nil || upstream.DirectoryDenied(err) {
		t.Errorf("a blocked device is not a directory refusal: %v", err)
	}
	f.fake.on("Search", always(0, searchStatusResp("130", true)))
	if _, err := m.SearchDirectory(ctx, "x@corp.example"); !upstream.DirectoryDenied(err) {
		t.Errorf("common status 130 (denied) stops the directory: %v", err)
	}
}

func atoiT(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

func TestSearchDirectoryHTTPThrottlingIsTypedWithRetryAfter(t *testing.T) {
	f := newFixture(t)
	m := f.open("")
	f.fake.on("Search", always(http.StatusServiceUnavailable, nil))
	f.fake.retryAfter = "7"
	_, err := m.SearchDirectory(ctx, "pat@corp.example")
	var te *upstream.ThrottleError
	if !errors.As(err, &te) || te.HTTP != 503 || te.RetryAfter != 7*time.Second || !te.Explicit {
		t.Errorf("%v", err)
	}
}

func TestSearchDirectoryErrorsAndLogsNeverHoldTheQuery(t *testing.T) {
	f := newFixture(t)
	m := f.open("")
	f.fake.on("Search", always(0, searchStatusResp("3", true)))
	_, err := m.SearchDirectory(ctx, "private.person@corp.example")
	if err == nil {
		t.Fatal("no error")
	}
	if strings.Contains(err.Error(), "private") || strings.Contains(f.logs.String(), "private") {
		t.Errorf("the query leaked: %v\n%s", err, f.logs.String())
	}
}

func TestTheModelRefusesSearchRequestsExchangeWouldRefuse(t *testing.T) {
	good := e(wbxml.PageSearch, "Search", e(wbxml.PageSearch, "Store",
		e(wbxml.PageSearch, "Name", txt("GAL")), e(wbxml.PageSearch, "Query", txt("pat")),
		e(wbxml.PageSearch, "Options", e(wbxml.PageSearch, "Range", txt("0-99")))))
	if why := searchViolation(good); why != "" {
		t.Fatalf("the model refuses a good request: %s", why)
	}
	for name, bad := range map[string]*wbxml.Element{
		"options before the query": e(wbxml.PageSearch, "Search", e(wbxml.PageSearch, "Store", e(wbxml.PageSearch, "Name", txt("GAL")),
			e(wbxml.PageSearch, "Options", e(wbxml.PageSearch, "Range", txt("0-9"))), e(wbxml.PageSearch, "Query", txt("pat")))),
		"empty query": e(wbxml.PageSearch, "Search", e(wbxml.PageSearch, "Store", e(wbxml.PageSearch, "Name", txt("GAL")), e(wbxml.PageSearch, "Query", txt("")))),
		"no query":    e(wbxml.PageSearch, "Search", e(wbxml.PageSearch, "Store", e(wbxml.PageSearch, "Name", txt("GAL")))),
		"backwards range": e(wbxml.PageSearch, "Search", e(wbxml.PageSearch, "Store", e(wbxml.PageSearch, "Name", txt("GAL")), e(wbxml.PageSearch, "Query", txt("a")),
			e(wbxml.PageSearch, "Options", e(wbxml.PageSearch, "Range", txt("9-0"))))),
		"a range that is not one": e(wbxml.PageSearch, "Search", e(wbxml.PageSearch, "Store", e(wbxml.PageSearch, "Name", txt("GAL")), e(wbxml.PageSearch, "Query", txt("a")),
			e(wbxml.PageSearch, "Options", e(wbxml.PageSearch, "Range", txt("99"))))),
		"a tree as the query": e(wbxml.PageSearch, "Search", e(wbxml.PageSearch, "Store", e(wbxml.PageSearch, "Name", txt("GAL")),
			e(wbxml.PageSearch, "Query", e(wbxml.PageSearch, "And")))),
		"an option of the mailbox": e(wbxml.PageSearch, "Search", e(wbxml.PageSearch, "Store", e(wbxml.PageSearch, "Name", txt("GAL")), e(wbxml.PageSearch, "Query", txt("a")),
			e(wbxml.PageSearch, "Options", e(wbxml.PageSearch, "DeepTraversal")))),
	} {
		if why := searchViolation(bad); why == "" {
			t.Errorf("%s: the model accepts it", name)
		}
	}
}
