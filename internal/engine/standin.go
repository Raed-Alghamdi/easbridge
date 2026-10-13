package engine

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"mime"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"easbridge/internal/store"
)

// A stand-in is the message shown for an item Exchange keeps failing to give as
// MIME (an item class without a MIME form, a corrupt item, a body its converter
// chokes on). Without it the item is announced by Sync, never fetched, and stays
// invisible for ever: the user does not even learn that a message exists. After
// standInAfter failures that were about the item itself, a small message built
// from the metadata of the Sync response (subject, sender, recipients, date) takes
// its place, with a plain explanation as the body. It is a normal server-bound
// message: flag changes and deletes reach Exchange, and the item's later changes
// from Exchange are merged like any other. It is final: the item is not fetched
// again.
const standInAfter = 10

// standInMIME builds the stand-in for the waiting item p. Everything it contains
// comes from Exchange and is cleaned: no control characters, no line breaks
// inside a header value, bounded lengths, non-ASCII text encoded.
func standInMIME(p *store.PendingFetch) []byte {
	sum := sha256.Sum256([]byte(p.ServerID))
	var b bytes.Buffer
	line := func(name, value string) {
		b.WriteString(name + ": " + value + "\r\n")
	}
	line("Message-ID", "<standin-"+hex.EncodeToString(sum[:12])+"@easbridge.invalid>")
	line("Date", p.InternalDate.UTC().Format(time.RFC1123Z))
	line("From", standInFrom(p.From))
	if to := standInTo(p.To); to != "" {
		line("To", to)
	}
	subject := cleanText(p.Subject, 200)
	if subject == "" {
		subject = "(no subject)"
	}
	line("Subject", mime.QEncoding.Encode("utf-8", subject))
	line("MIME-Version", "1.0")
	line("Content-Type", "text/plain; charset=us-ascii")
	line("Content-Transfer-Encoding", "7bit")
	line("X-Easbridge-Stand-In", "Exchange could not provide this message")
	b.WriteString("\r\n")
	b.WriteString("This message could not be downloaded from Exchange.\r\n\r\n")
	b.WriteString("easbridge asked Exchange for the text of this message several times and Exchange could not\r\n")
	b.WriteString("provide it, so this stand-in shows what Exchange lists for it: the subject, the sender, the\r\n")
	b.WriteString("recipients and the date. Open the message in Outlook or in the Exchange web mail to read it.\r\n")
	return b.Bytes()
}

// cleanText removes control characters and collapses white space, so that the
// text is safe inside a header, and cuts it to at most max runes.
func cleanText(s string, max int) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\r' || r == '\n':
			return ' '
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == 0x2028 || r == 0x2029 || r == 0xfeff:
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > max {
		s = string([]rune(s)[:max])
	}
	return s
}

// standInFrom renders the sender Exchange listed ("Name <addr>") as a header
// value; what cannot be read as an address becomes a display name on an address
// that cannot exist.
func standInFrom(raw string) string {
	raw = cleanText(raw, 300)
	if a, err := mail.ParseAddress(raw); err == nil {
		return a.String()
	}
	name := raw
	if name == "" {
		name = "Unknown sender"
	}
	return (&mail.Address{Name: name, Address: "unknown@easbridge.invalid"}).String()
}

// standInTo renders Exchange's recipient list (semicolon separated) as a header
// value, "" if it cannot be read.
func standInTo(raw string) string {
	raw = cleanText(strings.ReplaceAll(raw, ";", ","), 1000)
	if raw == "" {
		return ""
	}
	list, err := mail.ParseAddressList(raw)
	if err != nil || len(list) == 0 {
		return ""
	}
	var parts []string
	for i, a := range list {
		if i == 50 {
			break
		}
		parts = append(parts, a.String())
	}
	return strings.Join(parts, ", ")
}
