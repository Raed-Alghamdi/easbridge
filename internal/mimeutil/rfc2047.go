package mimeutil

import (
	"bytes"
	"encoding/base64"
	"io"
	"regexp"
	"strings"

	"github.com/emersion/go-message/charset"
)

// encodedWord matches one RFC 2047 encoded-word, "=?charset?B?text?=" or "=?charset?Q?text?=".
// The charset may carry a language (RFC 2231 section 5: "utf-8*ar"), and the text holds
// neither "?" nor white space. The expression is linear (RE2).
var encodedWord = regexp.MustCompile(`=\?([^?\s]+)\?([bBqQ])\?([^?\s]*)\?=`)

// DecodeHeader decodes the RFC 2047 encoded-words of a header value into UTF-8 and
// leaves everything else as it is. It never fails: a word it cannot decode (an
// unknown charset, broken base64) stays as it was written, and the words around it
// are still decoded. The standard library gives up on a whole value for one unknown
// charset, and then a subject full of Arabic words stays encoded for SEARCH and for
// the Sent-copy comparison.
//
// It reads what real mail does: the charset in any case ("Windows-1256",
// "ISO-8859-6", "UTF-8"), a language after the charset ("UTF-8*ar"), base64 without
// its padding, "_" for a space and a stray "=" in Q text, a character of a
// multi-byte charset cut between two words (UTF-8 words are joined as bytes before
// the text is checked), and white space between two encoded-words, which is not part
// of the text (RFC 2047 section 6.2).
func DecodeHeader(s string) string {
	if !strings.Contains(s, "=?") {
		return strings.ToValidUTF8(s, "\uFFFD")
	}
	locs := encodedWord.FindAllStringSubmatchIndex(s, -1)
	if locs == nil {
		return strings.ToValidUTF8(s, "\uFFFD")
	}
	var out strings.Builder
	out.Grow(len(s))
	last := 0
	prevDecoded := false
	for _, loc := range locs {
		between := s[last:loc[0]]
		word, ok := decodeWord(s[loc[2]:loc[3]], s[loc[4]], s[loc[6]:loc[7]])
		if !ok {
			out.WriteString(between)
			out.WriteString(s[loc[0]:loc[1]])
			prevDecoded = false
			last = loc[1]
			continue
		}
		if !prevDecoded || strings.TrimSpace(between) != "" {
			out.WriteString(between)
		}
		out.WriteString(word)
		prevDecoded = true
		last = loc[1]
	}
	out.WriteString(s[last:])
	return strings.ToValidUTF8(out.String(), "�")
}

// decodeWord decodes the text of one encoded-word and converts it from its charset.
// The result is not checked for UTF-8 (the caller does it once, on the joined text).
func decodeWord(charsetName string, enc byte, text string) (string, bool) {
	if i := strings.IndexByte(charsetName, '*'); i >= 0 {
		charsetName = charsetName[:i] // the language of RFC 2231
	}
	if charsetName == "" {
		return "", false
	}
	var raw []byte
	if enc == 'b' || enc == 'B' {
		b, err := base64.StdEncoding.DecodeString(text)
		if err != nil {
			// Padding left out, or too much of it.
			if b, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(text, "=")); err != nil {
				return "", false
			}
		}
		raw = b
	} else {
		raw = qDecode(text)
	}
	switch strings.ToLower(charsetName) {
	case "utf-8", "utf8", "us-ascii", "ascii":
		return string(raw), true
	}
	r, err := charset.Reader(charsetName, bytes.NewReader(raw))
	if err != nil {
		return "", false
	}
	out, err := io.ReadAll(r)
	if err != nil {
		return "", false
	}
	return string(out), true
}

// qDecode decodes the "Q" encoding: "_" is a space, "=XX" is a byte, anything else
// stands for itself (a "=" that is not followed by two hex digits is kept).
func qDecode(s string) []byte {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '_':
			out = append(out, ' ')
		case c == '=' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]):
			out = append(out, unhex(s[i+1])<<4|unhex(s[i+2]))
			i += 2
		default:
			out = append(out, c)
		}
	}
	return out
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}
