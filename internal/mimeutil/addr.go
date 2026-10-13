package mimeutil

import (
	"net/mail"
	"strconv"
	"strings"
)

// Addr is one mailbox of an address list: the display name exactly as the header
// has it, RFC 2047 encoded-words and all, and the addr-spec.
type Addr struct {
	Name    string
	Address string
}

// ParseAddrList parses the value of an address header (From, To, Cc ...) with the
// grammar of net/mail and decodes nothing. net/mail decodes the encoded-words of a
// display name while it parses, and fails the whole list when it meets a charset it
// has no reader for; go-message's AddressList does the same with its charset table.
// Both are wrong for what EASBridge needs: an IMAP ENVELOPE carries the names as the
// message wrote them (RFC 3501 section 7.4.2) and a list must not vanish because one
// name is in a charset nobody listed. So every encoded-word is swapped for a plain
// atom before the parse and put back afterwards. Text that is not valid UTF-8 (a
// header in a legacy 8-bit charset) is cleaned first, as net/mail refuses it.
//
// An empty value gives no addresses and no error. Errors are net/mail's.
func ParseAddrList(s string) ([]Addr, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	s = strings.ToValidUTF8(s, "�")
	var restore *strings.Replacer
	if locs := encodedWord.FindAllStringIndex(s, -1); locs != nil {
		// A prefix that is not in the value, so that nothing in it can be taken for a placeholder.
		prefix := "ewph"
		for strings.Contains(s, prefix) {
			prefix += "x"
		}
		var b strings.Builder
		pairs := make([]string, 0, 2*len(locs))
		last := 0
		for i, loc := range locs {
			tok := prefix + strconv.Itoa(i) + "z" // "z" ends the number: no token is the start of another
			b.WriteString(s[last:loc[0]])
			b.WriteString(tok)
			pairs = append(pairs, tok, s[loc[0]:loc[1]])
			last = loc[1]
		}
		b.WriteString(s[last:])
		s = b.String()
		restore = strings.NewReplacer(pairs...)
	}
	list, err := mail.ParseAddressList(s)
	if err != nil {
		return nil, err
	}
	out := make([]Addr, 0, len(list))
	for _, a := range list {
		name, addr := a.Name, a.Address
		if restore != nil {
			name, addr = restore.Replace(name), restore.Replace(addr)
		}
		out = append(out, Addr{Name: name, Address: addr})
	}
	return out, nil
}

// maxCandidateAddrs bounds the addresses taken from one message: a header with
// thousands of recipients is not a person's mail.
const maxCandidateAddrs = 200

// HeaderAddresses returns the mailboxes of the From, To and Cc fields of a message
// header (every field of that name), in that order, with the display name decoded and
// the address lower-cased. A field that does not parse gives nothing; at most 200 are
// returned. The directory (internal/engine) uses it to find the addresses that appear
// in the user's mail.
func HeaderAddresses(header []byte) []Addr {
	f := parseFields(capHeader(header))
	var out []Addr
	for _, name := range []string{"from", "to", "cc"} {
		for _, v := range f[name] {
			list, err := ParseAddrList(v)
			if err != nil {
				continue
			}
			for _, a := range list {
				addr := strings.ToLower(strings.TrimSpace(a.Address))
				if addr == "" || !strings.Contains(addr, "@") {
					continue
				}
				out = append(out, Addr{Name: collapse(DecodeHeader(a.Name)), Address: addr})
				if len(out) >= maxCandidateAddrs {
					return out
				}
			}
		}
	}
	return out
}
