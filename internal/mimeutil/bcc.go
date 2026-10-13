package mimeutil

import (
	"net/mail"
	"regexp"
	"strings"
	"unicode"
)

// Limits of the Bcc header the bridge writes from a property.
const (
	// maxBccAddresses is the most recipients written into one Bcc header.
	maxBccAddresses = 2000
	// bccFoldAt is where a long Bcc field is folded: well inside the 998 characters
	// per line that RFC 5322 allows.
	bccFoldAt = 900
	// maxBccNameLen is the longest display name written; a longer one is dropped
	// and its address written bare.
	maxBccNameLen = 200
)

// addrSpecRe finds something that looks like an addr-spec in text that could not
// be read as an address list.
var addrSpecRe = regexp.MustCompile(`[^\s<>(),;:"\\]+@[^\s<>(),;:"\\]+`)

// AddBccFromProperty writes the Bcc recipients that Exchange lists with a message
// of Sent Items (the email2:Bcc property, a list of addresses that may carry
// display names) as a Bcc header of the copy the bridge serves: Exchange's own
// MIME rendering of a sent message has no Bcc header. It reports whether it did.
// A message that already has a Bcc header, an empty property and a property that
// holds no address that could be read leave the message as it was.
//
// The value comes from the server and is cleaned like the rest of what the
// bridge writes into a header: control characters are dropped (a line break cannot
// start another header), every address is rendered by net/mail (a name with
// special characters is quoted, one with non-ASCII characters becomes an
// RFC 2047 encoded-word), a repeated address is written once, at most
// maxBccAddresses addresses are kept and a long field is folded.
func AddBccFromProperty(raw []byte, prop string) ([]byte, bool) {
	if HasField(raw, "bcc") {
		return raw, false
	}
	header, body := SplitHeader(raw)
	eol := eolOf(header)
	value := bccFieldValue(prop, eol)
	if value == "" {
		return raw, false
	}
	return appendField(header, body, eol, "Bcc: "+value), true
}

// bccFieldValue renders the property as the value of a Bcc field ("" when it holds
// no address), folded with eol.
func bccFieldValue(prop string, eol string) string {
	prop = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(prop, ""))
	if strings.TrimSpace(prop) == "" {
		return ""
	}
	// Exchange separates recipients with commas (MS-ASEMAIL 2.2.2.79, the To element
	// that Bcc shares its form with); the vendored library's documentation says
	// semicolons, so both are read.
	list, err := mail.ParseAddressList(prop)
	if err != nil {
		list, err = mail.ParseAddressList(strings.ReplaceAll(prop, ";", ","))
	}
	var entries []string
	seen := map[string]bool{}
	add := func(display, addr string) {
		low := strings.ToLower(addr)
		if addr == "" || seen[low] || len(entries) >= maxBccAddresses {
			return
		}
		seen[low] = true
		if len(display) > maxBccNameLen {
			display = "" // a name that long is not a name; the address is what matters
		}
		if display == "" {
			entries = append(entries, addr)
			return
		}
		entries = append(entries, (&mail.Address{Name: display, Address: addr}).String())
	}
	if err == nil {
		for _, a := range list {
			add(a.Name, a.Address)
		}
	} else {
		// Not a list net/mail can read: take the addresses and leave the names.
		for _, tok := range addrSpecRe.FindAllString(prop, -1) {
			add("", tok)
		}
	}
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	lineLen := len("Bcc: ")
	for i, e := range entries {
		sep := ""
		if i > 0 {
			sep = ","
			if lineLen+len(sep)+1+len(e) > bccFoldAt {
				b.WriteString(sep + eol + " " + e)
				lineLen = 1 + len(e)
				continue
			}
			sep = ", "
		}
		b.WriteString(sep + e)
		lineLen += len(sep) + len(e)
	}
	return b.String()
}
