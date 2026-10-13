// Package mimeutil extracts the few header facts EASBridge indexes (Message-ID,
// In-Reply-To, References, Subject, Date, To) from raw RFC 5322 messages, and
// holds small helpers shared by the IMAP and SMTP front ends.
package mimeutil

import (
	"bytes"
	"net/mail"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Limits on what is parsed out of a message's header block. Mail is hostile
// input: a header of tens of megabytes (or a References field of millions of
// ids) must not turn into millions of database rows or seconds of CPU. The raw
// header block itself is always stored whole; only the parsing is bounded.
const (
	// MaxHeaderBytes is how much of a header block is parsed.
	MaxHeaderBytes = 128 << 10
	// MaxInReplyToIDs and MaxReferenceIDs cap the reference tokens indexed.
	MaxInReplyToIDs = 200
	MaxReferenceIDs = 2000
)

// capHeader returns at most MaxHeaderBytes of h, cut at a line boundary.
func capHeader(h []byte) []byte {
	if len(h) <= MaxHeaderBytes {
		return h
	}
	h = h[:MaxHeaderBytes]
	if i := bytes.LastIndexByte(h, '\n'); i >= 0 {
		h = h[:i+1]
	}
	return h
}

// Meta is what EASBridge remembers about a message besides its bytes.
type Meta struct {
	// Header is the raw header block, including the blank line that ends it.
	Header []byte
	// MessageID is the Message-ID without angle brackets ("" if absent).
	MessageID string
	// InReplyTo and References are the unfolded header values.
	InReplyTo  string
	References string
	// InReplyToIDs and ReferenceIDs are the <...> tokens without brackets.
	InReplyToIDs []string
	ReferenceIDs []string
	// Subject is RFC 2047 decoded.
	Subject string
	// Date is the Date header (zero if absent or unparsable).
	Date time.Time
	// To is a normalised, sorted, lower-case list of the recipient addr-specs,
	// joined with commas.
	To string
}

// SplitHeader splits raw at the first blank line. header includes the blank
// line; body is what follows. A message without a blank line is all header.
func SplitHeader(raw []byte) (header, body []byte) {
	// A message that begins with a blank line has an empty header.
	if bytes.HasPrefix(raw, []byte("\r\n")) {
		return raw[:2], raw[2:]
	}
	if bytes.HasPrefix(raw, []byte("\n")) {
		return raw[:1], raw[1:]
	}
	i := 0
	for i < len(raw) {
		nl := bytes.IndexByte(raw[i:], '\n')
		if nl < 0 {
			return raw, nil
		}
		end := i + nl + 1 // just past this line's LF
		if end < len(raw) {
			if raw[end] == '\n' {
				return raw[:end+1], raw[end+1:]
			}
			if raw[end] == '\r' && end+1 < len(raw) && raw[end+1] == '\n' {
				return raw[:end+2], raw[end+2:]
			}
		}
		i = end
	}
	return raw, nil
}

// ParseMeta reads the header of raw. It never fails: unparsable parts are left
// empty.
func ParseMeta(raw []byte) Meta {
	header, _ := SplitHeader(raw)
	m := Meta{Header: header}
	hdr := parseFields(capHeader(header))
	if ids := Ids(hdr.get("Message-Id")); len(ids) > 0 {
		m.MessageID = ids[0]
	} else {
		m.MessageID = strings.Trim(strings.TrimSpace(hdr.get("Message-Id")), "<>")
	}
	m.InReplyTo = collapse(hdr.get("In-Reply-To"))
	m.References = collapse(hdr.get("References"))
	m.InReplyToIDs = firstN(Ids(m.InReplyTo), MaxInReplyToIDs)
	m.ReferenceIDs = firstN(Ids(m.References), MaxReferenceIDs)
	m.Subject = DecodeHeader(collapse(hdr.get("Subject")))
	if d := strings.TrimSpace(hdr.get("Date")); d != "" {
		if t, err := mail.ParseDate(d); err == nil {
			m.Date = t
		}
	}
	m.To = NormalizeAddrList(hdr.get("To"))
	return m
}

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

func firstN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

var idRe = regexp.MustCompile(`<([^<>\s]+)>`)

// Ids returns the <id> tokens of s without brackets, in order.
func Ids(s string) []string {
	var out []string
	for _, m := range idRe.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

// NormalizeAddrList returns the bare, lower-case addresses of a header value,
// sorted and comma-joined. Unparsable input is lower-cased and trimmed.
func NormalizeAddrList(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	list, err := ParseAddrList(s)
	if err != nil {
		return strings.ToLower(collapse(s))
	}
	addrs := make([]string, 0, len(list))
	for _, a := range list {
		addrs = append(addrs, strings.ToLower(a.Address))
	}
	sort.Strings(addrs)
	return strings.Join(addrs, ",")
}

var subjectPrefix = regexp.MustCompile(`(?i)^\s*((re|fwd?|aw|sv)\s*(\[\d+\])?\s*:\s*)+`)

// NormalizeSubject lower-cases a subject, collapses white space and drops
// leading Re:/Fwd: style prefixes. Used by the Sent de-duplication fallback.
func NormalizeSubject(s string) string {
	s = subjectPrefix.ReplaceAllString(DecodeHeader(s), "")
	return strings.ToLower(collapse(s))
}

// fields is a tolerant header parser: lines without a colon are skipped
// instead of aborting the parse, which matters for slightly malformed mail.
type fields map[string][]string

func (f fields) get(name string) string {
	if v := f[strings.ToLower(name)]; len(v) > 0 {
		return v[0]
	}
	return ""
}

func parseFields(header []byte) fields {
	f := fields{}
	var key string
	var val strings.Builder
	flush := func() {
		if key != "" {
			f[key] = append(f[key], strings.TrimSpace(val.String()))
		}
		key = ""
		val.Reset()
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(header), "\r\n", "\n"), "\n") {
		if line == "" {
			break // end of header
		}
		if line[0] == ' ' || line[0] == '\t' {
			if key != "" {
				val.WriteByte(' ')
				val.WriteString(strings.TrimSpace(line))
			}
			continue
		}
		flush()
		i := strings.IndexByte(line, ':')
		if i <= 0 {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(line[:i]))
		val.WriteString(strings.TrimSpace(line[i+1:]))
	}
	flush()
	return f
}

// AddEnvelopeRecipients makes sure every SMTP envelope recipient appears in the
// message's headers. EAS SendMail takes its recipients from the headers only,
// while SMTP clients put Bcc recipients solely in RCPT TO. Recipients missing
// from To, Cc and Bcc are added to a Bcc header (Exchange strips Bcc on
// delivery): an existing Bcc field is extended in place, otherwise one is added
// at the end of the header block. The message is otherwise untouched. It
// returns the new message and the addresses that were added.
func AddEnvelopeRecipients(raw []byte, envelope []string) (out []byte, added []string) {
	header, body := SplitHeader(raw)
	have := map[string]bool{}
	for _, f := range []string{"to", "cc", "bcc"} {
		for _, v := range parseFields(header)[f] {
			if list, err := ParseAddrList(v); err == nil {
				for _, a := range list {
					have[strings.ToLower(a.Address)] = true
				}
			} else {
				// Unparsable list: fall back to scanning for addr-specs.
				for _, tok := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ';' || r == '<' || r == '>' || r == ' ' || r == '"' }) {
					if strings.Contains(tok, "@") {
						have[strings.ToLower(tok)] = true
					}
				}
			}
		}
	}
	seen := map[string]bool{}
	for _, r := range envelope {
		r = strings.TrimSpace(r)
		low := strings.ToLower(r)
		if r == "" || have[low] || seen[low] {
			continue
		}
		seen[low] = true
		added = append(added, r)
	}
	if len(added) == 0 {
		return raw, nil
	}
	eol := eolOf(header)
	extra := strings.Join(added, ", ")
	if start, end, ok := findField(header, "bcc"); ok {
		// Extend the existing field: "Bcc: <old value>, <extra>" on one line.
		oldVal := strings.TrimSpace(string(header[start:end]))
		oldVal = strings.TrimSpace(strings.TrimPrefix(oldVal[strings.IndexByte(oldVal, ':')+1:], " "))
		oldVal = strings.Join(strings.Fields(oldVal), " ")
		newField := "Bcc: " + extra
		if oldVal != "" {
			newField = "Bcc: " + oldVal + ", " + extra
		}
		var b bytes.Buffer
		b.Write(header[:start])
		b.WriteString(newField + eol)
		b.Write(header[end:])
		b.Write(body)
		return b.Bytes(), added
	}
	return appendField(header, body, eol, "Bcc: "+extra), added
}

// eolOf is the line ending a header block uses: CRLF, or LF when the block has
// only bare line feeds.
func eolOf(header []byte) string {
	if !bytes.Contains(header, []byte("\r\n")) && bytes.Contains(header, []byte("\n")) {
		return "\n"
	}
	return "\r\n"
}

// appendField returns the message with one more header field (field is its text
// without the final line ending, folded lines included) placed in front of the
// blank line that ends the header block, which is added when the block has none.
// The rest of the message is untouched.
func appendField(header, body []byte, eol, field string) []byte {
	cut := len(header)
	switch {
	case bytes.HasSuffix(header, []byte("\r\n\r\n")):
		cut -= 2
	case bytes.HasSuffix(header, []byte("\n\n")):
		cut--
	default:
		// No terminating blank line (headers only): add one.
		var b bytes.Buffer
		b.Write(header)
		if !bytes.HasSuffix(header, []byte("\n")) {
			b.WriteString(eol)
		}
		b.WriteString(field + eol + eol)
		return b.Bytes()
	}
	var b bytes.Buffer
	b.Write(header[:cut])
	b.WriteString(field + eol)
	b.Write(header[cut:])
	b.Write(body)
	return b.Bytes()
}

// HasField reports whether the header block of raw has a field called name
// (case-insensitive, folded lines included).
func HasField(raw []byte, name string) bool {
	header, _ := SplitHeader(raw)
	_, _, ok := findField(header, name)
	return ok
}

// findField locates the first header field called name (case-insensitive) and
// returns the byte range of the field including its folded continuation lines
// and its line ending.
func findField(header []byte, name string) (start, end int, ok bool) {
	pos := 0
	for pos < len(header) {
		nl := bytes.IndexByte(header[pos:], '\n')
		lineEnd := len(header)
		if nl >= 0 {
			lineEnd = pos + nl + 1
		}
		line := header[pos:lineEnd]
		trim := bytes.TrimRight(line, "\r\n")
		if len(trim) == 0 {
			return 0, 0, false // blank line: end of the header
		}
		if trim[0] != ' ' && trim[0] != '\t' {
			if i := bytes.IndexByte(trim, ':'); i > 0 && strings.EqualFold(strings.TrimSpace(string(trim[:i])), name) {
				end := lineEnd
				for end < len(header) && (header[end] == ' ' || header[end] == '\t') {
					n := bytes.IndexByte(header[end:], '\n')
					if n < 0 {
						end = len(header)
						break
					}
					end += n + 1
				}
				return pos, end, true
			}
		}
		pos = lineEnd
	}
	return 0, 0, false
}
