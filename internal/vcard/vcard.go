// Package vcard writes the vCard 3.0 cards the CardDAV server hands out. It only
// writes: EASBridge is read-only, so no card is ever parsed.
//
// The rules are those of RFC 2426 with the care MailFlow's reader needs (see
// docs/DESIGN-contacts.md section 3.4): CRLF line ends, the values escaped (backslash,
// comma, semicolon, new line), lines folded at 75 octets without ever cutting a UTF-8
// sequence, and one TYPE parameter per type, because MailFlow reads up to the next
// semicolon.
package vcard

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
	"unicode/utf8"
)

// Card is a contact as the CardDAV server serves it.
type Card struct {
	UID  string
	Name string // FN; if empty the first of the other names, the organization or an e-mail address is used

	Family, Given, Additional, Prefix, Suffix string // N
	Nickname                                  string

	Emails    []Email
	Phones    []Phone
	Org       []string // organization, then its units
	Title     string
	Addresses []Address
	// Birthday and Anniversary are written as dates (UTC); the zero time is left out.
	Birthday, Anniversary time.Time
	URL                   string
	Categories            []string
	Note                  string
}

// Email is one e-mail address with its TYPE values.
type Email struct {
	Value string
	Types []string
}

// Phone is one telephone number with its TYPE values.
type Phone struct {
	Value string
	Types []string
}

// Address is one postal address (ADR: post box and extended address are not used).
type Address struct {
	Types                                 []string
	Street, City, Region, Postal, Country string
}

func (a Address) empty() bool {
	return a.Street == "" && a.City == "" && a.Region == "" && a.Postal == "" && a.Country == ""
}

// Marshal renders c as a vCard 3.0 with CRLF line ends and folded lines.
func (c Card) Marshal() []byte {
	var lines []string
	add := func(name string, params []string, value string) {
		var b strings.Builder
		b.WriteString(name)
		for _, p := range params {
			b.WriteString(";TYPE=")
			b.WriteString(paramValue(p))
		}
		b.WriteByte(':')
		b.WriteString(value)
		lines = append(lines, b.String())
	}
	lines = append(lines, "BEGIN:VCARD", "VERSION:3.0")
	add("UID", nil, Escape(c.UID))
	add("FN", nil, Escape(c.fn()))
	if c.Family != "" || c.Given != "" || c.Additional != "" || c.Prefix != "" || c.Suffix != "" {
		add("N", nil, strings.Join([]string{Escape(c.Family), Escape(c.Given), Escape(c.Additional), Escape(c.Prefix), Escape(c.Suffix)}, ";"))
	}
	if c.Nickname != "" {
		add("NICKNAME", nil, Escape(c.Nickname))
	}
	for _, e := range c.Emails {
		if e.Value != "" {
			add("EMAIL", e.Types, Escape(e.Value))
		}
	}
	for _, p := range c.Phones {
		if p.Value != "" {
			add("TEL", p.Types, Escape(p.Value))
		}
	}
	if len(c.Org) > 0 && c.Org[0] != "" || len(c.Org) > 1 {
		parts := make([]string, len(c.Org))
		for i, o := range c.Org {
			parts[i] = Escape(o)
		}
		add("ORG", nil, strings.Join(parts, ";"))
	}
	if c.Title != "" {
		add("TITLE", nil, Escape(c.Title))
	}
	for _, a := range c.Addresses {
		if !a.empty() {
			add("ADR", a.Types, strings.Join([]string{"", "", Escape(a.Street), Escape(a.City), Escape(a.Region), Escape(a.Postal), Escape(a.Country)}, ";"))
		}
	}
	if !c.Birthday.IsZero() {
		add("BDAY", nil, c.Birthday.UTC().Format("2006-01-02"))
	}
	if !c.Anniversary.IsZero() {
		add("X-ANNIVERSARY", nil, c.Anniversary.UTC().Format("2006-01-02"))
	}
	if c.URL != "" {
		add("URL", nil, Escape(c.URL))
	}
	if len(c.Categories) > 0 {
		cats := make([]string, 0, len(c.Categories))
		for _, x := range c.Categories {
			if x != "" {
				cats = append(cats, Escape(x))
			}
		}
		if len(cats) > 0 {
			add("CATEGORIES", nil, strings.Join(cats, ","))
		}
	}
	if c.Note != "" {
		add("NOTE", nil, Escape(c.Note))
	}
	lines = append(lines, "END:VCARD")

	var out strings.Builder
	for _, l := range lines {
		out.WriteString(Fold(l))
	}
	return []byte(out.String())
}

// fn is the formatted name: FN is required by vCard 3.0, so it falls back.
func (c Card) fn() string {
	if s := strings.TrimSpace(c.Name); s != "" {
		return s
	}
	var parts []string
	for _, p := range []string{c.Prefix, c.Given, c.Additional, c.Family, c.Suffix} {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, " ")
	}
	if c.Nickname != "" {
		return c.Nickname
	}
	if len(c.Org) > 0 && strings.TrimSpace(c.Org[0]) != "" {
		return strings.TrimSpace(c.Org[0])
	}
	for _, e := range c.Emails {
		if e.Value != "" {
			return e.Value
		}
	}
	return c.UID
}

// Escape escapes a text value: backslash, comma, semicolon and new line (a carriage
// return is dropped, a bare one too), and cleans it up: invalid UTF-8 becomes U+FFFD
// and the other control characters (which have no place in a card and could break its
// lines) become spaces.
func Escape(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		i += n
		if r == utf8.RuneError && n == 1 {
			b.WriteRune(utf8.RuneError)
			continue
		}
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == ';':
			b.WriteString(`\;`)
		case r == ',':
			b.WriteString(`\,`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			// dropped: a CRLF pair becomes the one escaped line feed above
		case r < 0x20 || r == 0x7f || r == 0x2028 || r == 0x2029 || r == 0x85:
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// paramValue makes a TYPE value safe: letters, digits and hyphens only, upper case.
func paramValue(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "OTHER"
	}
	return b.String()
}

// Fold folds one content line at 75 octets and ends it with CRLF. A continuation
// line starts with one space and holds at most 74 further octets. A UTF-8 sequence is
// never split between two lines.
func Fold(line string) string {
	if len(line) <= 75 {
		return line + "\r\n"
	}
	var b strings.Builder
	limit := 75
	for len(line) > 0 {
		if len(line) <= limit {
			b.WriteString(line)
			b.WriteString("\r\n")
			break
		}
		cut := limit
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		if cut == 0 { // cannot happen with valid text (a rune is at most 4 octets); never loop for ever
			cut = limit
		}
		b.WriteString(line[:cut])
		b.WriteString("\r\n ")
		line = line[cut:]
		limit = 74
	}
	return b.String()
}

// ETag is the entity tag of a card: the quoted SHA-256 of its bytes.
func ETag(card []byte) string {
	sum := sha256.Sum256(card)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// PersonalUID is the stable UID of a card that comes from a Contacts folder: "eas-"
// and the first 32 hex digits of the SHA-256 of the account, the folder's ServerId and
// the item's ServerId.
func PersonalUID(account, folderServerID, itemServerID string) string {
	return "eas-" + hash32(account, folderServerID, itemServerID)
}

// DirectoryUID is the stable UID of a directory card: "dir-" and the first 32 hex
// digits of the SHA-256 of the lower-case address.
func DirectoryUID(email string) string {
	return "dir-" + hash32(strings.ToLower(strings.TrimSpace(email)))
}

// hash32 hashes the parts, separated by a zero byte so that ("ab","c") and ("a","bc")
// differ (one part is hashed as it is), and returns 32 hex digits.
func hash32(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])[:32]
}
