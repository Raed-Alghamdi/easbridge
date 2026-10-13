package vcard

import (
	"net/mail"
	"strings"

	"easbridge/internal/upstream"
)

// FromContact maps an item of a Contacts folder to a card (docs/DESIGN-contacts.md
// section 3.4). account is the lower-case login name and folderServerID the ServerId
// of the folder; with the item's ServerId they make the UID.
func FromContact(account, folderServerID string, c upstream.Contact) Card {
	card := Card{
		UID:    PersonalUID(account, folderServerID, c.ServerID),
		Name:   strings.TrimSpace(c.FileAs),
		Family: strings.TrimSpace(c.LastName), Given: strings.TrimSpace(c.FirstName), Additional: strings.TrimSpace(c.MiddleName),
		Prefix: strings.TrimSpace(c.Title), Suffix: strings.TrimSpace(c.Suffix),
		Nickname: strings.TrimSpace(c.NickName),
		Title:    strings.TrimSpace(c.JobTitle),
		URL:      strings.TrimSpace(c.WebPage),
		Birthday: c.Birthday, Anniversary: c.Anniversary,
	}
	// The type of an address is not known (Exchange has Email1 to Email3 and no
	// types). MailFlow labels anything but work and home "Other", which is honest.
	for _, e := range []string{c.Email1, c.Email2, c.Email3} {
		if a := BareAddress(e); a != "" {
			card.Emails = append(card.Emails, Email{Value: a, Types: []string{"OTHER"}})
		}
	}
	// One TYPE parameter each: MailFlow reads up to the next semicolon, so a combined
	// "TYPE=WORK,VOICE" would show as "Other".
	for _, p := range []struct {
		v     string
		types []string
	}{
		{c.BusinessPhone, []string{"WORK", "VOICE"}},
		{c.MobilePhone, []string{"CELL"}},
		{c.HomePhone, []string{"HOME", "VOICE"}},
	} {
		if v := strings.TrimSpace(p.v); v != "" {
			card.Phones = append(card.Phones, Phone{Value: v, Types: p.types})
		}
	}
	if company, dept := strings.TrimSpace(c.Company), strings.TrimSpace(c.Department); company != "" || dept != "" {
		card.Org = []string{company}
		if dept != "" {
			card.Org = append(card.Org, dept)
		}
	}
	card.Addresses = []Address{
		{Types: []string{"HOME"}, Street: c.HomeAddress.Street, City: c.HomeAddress.City, Region: c.HomeAddress.State, Postal: c.HomeAddress.PostalCode, Country: c.HomeAddress.Country},
		{Types: []string{"WORK"}, Street: c.BusinessAddress.Street, City: c.BusinessAddress.City, Region: c.BusinessAddress.State, Postal: c.BusinessAddress.PostalCode, Country: c.BusinessAddress.Country},
	}
	for _, cat := range c.Categories {
		if cat = strings.TrimSpace(cat); cat != "" {
			card.Categories = append(card.Categories, cat)
		}
	}
	card.Note = officeNote(c.Office)
	return card
}

// FromDirectory maps a GAL entry to a card. Only the fields of section 4.6 are used;
// the entry type does not even carry the home phone and the alias.
func FromDirectory(e upstream.DirectoryEntry) Card {
	addr := strings.ToLower(strings.TrimSpace(e.EmailAddress))
	card := Card{
		UID:    DirectoryUID(addr),
		Name:   strings.TrimSpace(e.DisplayName),
		Family: strings.TrimSpace(e.LastName), Given: strings.TrimSpace(e.FirstName),
		Title:  strings.TrimSpace(e.Title),
		Emails: []Email{{Value: addr, Types: []string{"WORK"}}},
		Note:   officeNote(e.Office),
	}
	if card.Name == "" {
		card.Name = addr
	}
	if v := strings.TrimSpace(e.Phone); v != "" {
		card.Phones = append(card.Phones, Phone{Value: v, Types: []string{"WORK", "VOICE"}})
	}
	if v := strings.TrimSpace(e.MobilePhone); v != "" {
		card.Phones = append(card.Phones, Phone{Value: v, Types: []string{"CELL"}})
	}
	if v := strings.TrimSpace(e.Company); v != "" {
		card.Org = []string{v}
	}
	return card
}

// officeNote is how an office location is shown: MailFlow shows nothing of a card but
// its name, organization, addresses, phones and note.
func officeNote(office string) string {
	if office = strings.TrimSpace(office); office != "" {
		return "Office: " + office
	}
	return ""
}

// BareAddress reduces what Exchange stores in an e-mail field ("addr", "Name <addr>",
// "SMTP:addr") to the address, or "" when there is no address in it.
func BareAddress(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.IndexByte(s, '<'); i >= 0 {
		if j := strings.IndexByte(s[i:], '>'); j > 0 {
			s = s[i+1 : i+j]
		}
	} else if len(s) > 5 && strings.EqualFold(s[:5], "smtp:") {
		s = s[5:]
	}
	s = strings.TrimSpace(s)
	if a, err := mail.ParseAddress(s); err == nil {
		s = a.Address
	}
	if !strings.Contains(s, "@") {
		return ""
	}
	return s
}
