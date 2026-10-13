package easupstream

import (
	"context"
	"errors"
	"sort"

	"github.com/hstern/go-activesync/eas"

	"easbridge/internal/upstream"
)

// galRange is the number of entries asked of one Global Address List search. The
// server answers at most about 100 whatever the range (MS-ASCMD 2.2.3.133.2: a range
// that ends beyond 99 is cut); EASBridge never pages.
const galRange = 100

// SyncContacts implements upstream.Mailbox: one Sync round trip for a Contacts folder
// from the committed key. The first request of a chain (key "0") is the key and the
// folder only (vendored patch 15). On an invalid key (status 3) the chain starts again
// from "0" inside this call, like the library does for mail, and the delta says so
// (Reset) with the bootstrap key and no items.
func (m *Mailbox) SyncContacts(ctx context.Context, folderID, key string) (*upstream.ContactDelta, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}
	if key == "" {
		key = "0"
	}
	m.st.prime(folderID, key)
	res, err := m.cl.SyncContacts(ctx, folderID)
	reset := false
	if err != nil && key != "0" && eas.IsStatusCode(err, eas.StatusInvalidSyncKey) {
		reset = true
		m.st.prime(folderID, "0")
		res, err = m.cl.SyncContacts(ctx, folderID)
	}
	if err != nil {
		return nil, mapError("Sync", err)
	}
	newKey, _ := m.st.take(folderID)
	d := &upstream.ContactDelta{NewKey: newKey, MoreAvailable: res.MoreAvailable, Reset: reset, Deleted: res.Deleted}
	if reset {
		// The restarted call is the key-"0" request: it carries no items worth keeping.
		d.Added, d.Changed, d.Deleted = nil, nil, nil
	} else {
		for i := range res.Added {
			d.Added = append(d.Added, toContact(&res.Added[i]))
		}
		for i := range res.Changed {
			d.Changed = append(d.Changed, toContact(&res.Changed[i]))
		}
	}
	d.Fields = contactFieldNames(res.Added, res.Changed)
	return d, nil
}

func toContact(c *eas.ContactItem) upstream.Contact {
	addr := func(a eas.ContactAddress) upstream.Address {
		return upstream.Address{Street: a.Street, City: a.City, State: a.State, PostalCode: a.PostalCode, Country: a.Country}
	}
	return upstream.Contact{
		ServerID:  c.ServerID,
		FirstName: c.FirstName, LastName: c.LastName, MiddleName: c.MiddleName, Title: c.Title, Suffix: c.Suffix,
		FileAs: c.FileAs, NickName: c.NickName,
		Company: c.CompanyName, Department: c.Department, JobTitle: c.JobTitle, Office: c.OfficeLocation,
		Email1: c.Email1Address, Email2: c.Email2Address, Email3: c.Email3Address,
		HomePhone: c.HomePhone, BusinessPhone: c.BusinessPhone, MobilePhone: c.MobilePhone,
		HomeAddress: addr(c.HomeAddress), BusinessAddress: addr(c.BusinessAddress),
		Birthday: c.Birthday, Anniversary: c.Anniversary, WebPage: c.WebPage, Categories: c.Categories,
	}
}

// contactFieldNames lists the names (never the values) of the fields at least one of
// the items carried, in a fixed order, for `easbridge probe`.
func contactFieldNames(lists ...[]eas.ContactItem) []string {
	present := map[string]bool{}
	for _, l := range lists {
		for _, c := range l {
			for _, f := range []struct {
				name string
				set  bool
			}{
				{"FirstName", c.FirstName != ""}, {"LastName", c.LastName != ""}, {"MiddleName", c.MiddleName != ""},
				{"Title", c.Title != ""}, {"Suffix", c.Suffix != ""}, {"FileAs", c.FileAs != ""}, {"NickName", c.NickName != ""},
				{"CompanyName", c.CompanyName != ""}, {"Department", c.Department != ""}, {"JobTitle", c.JobTitle != ""},
				{"OfficeLocation", c.OfficeLocation != ""},
				{"Email1Address", c.Email1Address != ""}, {"Email2Address", c.Email2Address != ""}, {"Email3Address", c.Email3Address != ""},
				{"HomePhoneNumber", c.HomePhone != ""}, {"BusinessPhoneNumber", c.BusinessPhone != ""}, {"MobilePhoneNumber", c.MobilePhone != ""},
				{"HomeAddress", c.HomeAddress != (eas.ContactAddress{})}, {"BusinessAddress", c.BusinessAddress != (eas.ContactAddress{})},
				{"Birthday", !c.Birthday.IsZero()}, {"Anniversary", !c.Anniversary.IsZero()}, {"WebPage", c.WebPage != ""},
				{"Picture", len(c.Picture) > 0}, {"Categories", len(c.Categories) > 0},
				{"IMAddress", c.IMAddress != "" || c.IMAddress2 != "" || c.IMAddress3 != ""}, {"ManagerName", c.ManagerName != ""},
				{"GovernmentId", c.GovernmentID != ""}, {"CustomerId", c.CustomerID != ""},
			} {
				if f.set {
					present[f.name] = true
				}
			}
		}
	}
	return sortedKeys(present)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SearchDirectory implements upstream.Mailbox: one Search (store GAL) for query. The
// entries are cut down to the fields EASBridge keeps: the home phone and the alias
// are dropped here, so nothing further up can store them.
func (m *Mailbox) SearchDirectory(ctx context.Context, query string) (*upstream.DirectoryResult, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}
	res, err := m.cl.GALSearch(ctx, query, galRange)
	if err != nil {
		var se *eas.StatusError
		if errors.As(err, &se) && se.Command == "Search/GAL" && (se.Code == 6 || se.Code == 12) {
			return &upstream.DirectoryResult{}, nil // nothing found / the range starts after the last match
		}
		return nil, mapError("Search", err)
	}
	out := &upstream.DirectoryResult{Total: res.Total}
	present := map[string]bool{}
	for _, e := range res.Entries {
		for _, f := range []struct {
			name string
			set  bool
		}{
			{"DisplayName", e.DisplayName != ""}, {"FirstName", e.FirstName != ""}, {"LastName", e.LastName != ""},
			{"Title", e.Title != ""}, {"Office", e.Office != ""}, {"Company", e.Company != ""}, {"Alias", e.Alias != ""},
			{"EmailAddress", e.EmailAddress != ""}, {"Phone", e.Phone != ""}, {"HomePhone", e.HomePhone != ""}, {"MobilePhone", e.MobilePhone != ""},
		} {
			if f.set {
				present[f.name] = true
			}
		}
		out.Entries = append(out.Entries, upstream.DirectoryEntry{
			DisplayName: e.DisplayName, FirstName: e.FirstName, LastName: e.LastName,
			Title: e.Title, Office: e.Office, Company: e.Company,
			EmailAddress: e.EmailAddress, Phone: e.Phone, MobilePhone: e.MobilePhone,
		})
	}
	out.Fields = sortedKeys(present)
	return out, nil
}
