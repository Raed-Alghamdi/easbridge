package mimestruct

import (
	"bufio"
	"bytes"
	"io"
	"strings"

	"github.com/emersion/go-imap/v2"
	gomessage "github.com/emersion/go-message"
	"github.com/emersion/go-message/textproto"

	"easbridge/internal/mimeutil"
)

// open returns the embedded message of a message/rfc822 part, or the part
// itself (go-imap's openMessagePart).
func (n *Node) open() *Node {
	if n.Msg != nil {
		return n.Msg.Root
	}
	return n
}

// locate finds the part named by a section path (go-imap's findMessagePart),
// returning its header block and body as slices of raw. With no path it is the
// message itself. spec matters only for HEADER and TEXT of a numbered part: they
// address the embedded message when that part is a message/rfc822.
//
// ix may be nil when the path is empty. With a path and no index the message is
// indexed on the spot (bounded, like at ingest).
func locate(raw []byte, ix *Index, path []int, spec imap.PartSpecifier) (hdr, body []byte, ok bool) {
	if len(path) == 0 {
		he := headerLen(raw)
		return raw[:he], raw[he:], true
	}
	if ix == nil || !ix.Check(len(raw)) {
		ix = Build(raw)
	}
	n := ix.Root
	p := path
	// The first part of a non-multipart message is the message itself.
	if !n.IsMultipart() && p[0] == 1 {
		p = p[1:]
	}
	for _, num := range p {
		n = n.open()
		if !n.IsMultipart() {
			if num != 1 {
				return nil, nil, false
			}
			continue
		}
		if num < 1 || num > len(n.Kids) {
			return nil, nil, false
		}
		n = n.Kids[num-1]
	}
	if spec == imap.PartSpecifierHeader || spec == imap.PartSpecifierText {
		n = n.open()
	}
	return raw[n.HS:n.HE], raw[n.HE:n.BE], true
}

// Section extracts a body section (BODY[...]) of raw. It follows go-imap's
// ExtractBodySection, with the part tree taken from ix (nil: build it) and
// without ever copying a part. Unlike go-imap it returns the original bytes for
// BODY[] and for an unfiltered HEADER, MIME or TEXT (go-imap re-serialised the
// header), so the literal length always agrees with RFC822.SIZE.
func Section(raw []byte, ix *Index, item *imap.FetchItemBodySection) []byte {
	hdr, body, ok := locate(raw, ix, item.Part, item.Specifier)
	if !ok {
		return nil
	}
	var out []byte
	switch item.Specifier {
	case imap.PartSpecifierNone:
		if len(item.Part) == 0 {
			out = joinAdjacent(raw, hdr, body)
		} else {
			out = body
		}
	case imap.PartSpecifierText:
		out = body
	case imap.PartSpecifierHeader, imap.PartSpecifierMIME:
		out = filterHeader(hdr, item)
	}
	return extractPartial(out, item.Partial)
}

// joinAdjacent returns hdr followed by body. They are neighbours inside raw, so
// no copy is needed.
func joinAdjacent(raw, hdr, body []byte) []byte {
	if len(hdr) == 0 {
		return body
	}
	if len(body) == 0 {
		return hdr
	}
	// hdr and body are raw[:he] and raw[he:]: the whole message.
	if len(hdr)+len(body) == len(raw) {
		return raw
	}
	return append(append([]byte(nil), hdr...), body...)
}

// filterHeader applies HEADER.FIELDS and HEADER.FIELDS.NOT to a header block.
// Without filters the block is returned as it is.
func filterHeader(hdr []byte, item *imap.FetchItemBodySection) []byte {
	if len(item.HeaderFields) == 0 && len(item.HeaderFieldsNot) == 0 {
		return hdr
	}
	header, _ := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(hdr)))
	if len(item.HeaderFields) > 0 {
		keep := make(map[string]struct{}, len(item.HeaderFields))
		for _, k := range item.HeaderFields {
			keep[strings.ToLower(k)] = struct{}{}
		}
		for field := header.Fields(); field.Next(); {
			if _, ok := keep[strings.ToLower(field.Key())]; !ok {
				field.Del()
			}
		}
	}
	for _, k := range item.HeaderFieldsNot {
		header.Del(k)
	}
	var buf bytes.Buffer
	if err := textproto.WriteHeader(&buf, header); err != nil {
		return nil
	}
	return buf.Bytes()
}

func extractPartial(b []byte, partial *imap.SectionPartial) []byte {
	if partial == nil {
		return b
	}
	end := partial.Offset + partial.Size
	if partial.Offset > int64(len(b)) {
		return nil
	}
	if end > int64(len(b)) {
		end = int64(len(b))
	}
	return b[partial.Offset:end]
}

// BinarySection extracts BINARY[...]: the part with its transfer encoding
// decoded (go-imap's ExtractBinarySection). The server does not advertise
// BINARY, so clients do not ask for it; it is here so that a client that does
// still gets a bounded answer.
func BinarySection(raw []byte, ix *Index, item *imap.FetchItemBinarySection) []byte {
	hdr, body, ok := locate(raw, ix, item.Part, imap.PartSpecifierNone)
	if !ok {
		return nil
	}
	th, _ := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(hdr)))
	part, err := gomessage.New(gomessage.Header{Header: th}, bytes.NewReader(body))
	if part == nil {
		_ = err
		return nil
	}
	var buf bytes.Buffer
	if len(item.Part) == 0 {
		if err := textproto.WriteHeader(&buf, part.Header.Header); err != nil {
			return nil
		}
	}
	if _, err := io.Copy(&buf, io.LimitReader(part.Body, int64(len(raw))+1<<20)); err != nil {
		return nil
	}
	return extractPartial(buf.Bytes(), item.Partial)
}

// BinarySectionSize is the size of BINARY[...].
func BinarySectionSize(raw []byte, ix *Index, item *imap.FetchItemBinarySectionSize) uint32 {
	return uint32(len(BinarySection(raw, ix, &imap.FetchItemBinarySection{Part: item.Part})))
}

// maxSearchBytes bounds the decoded text examined per message by a text search.
const maxSearchBytes = 16 << 20

// MatchText implements SEARCH TEXT (includeHeader) and BODY: whether pattern,
// compared case-insensitively, occurs in the decoded text of the message's
// text parts and, for TEXT, in the decoded header values of every part. At
// most maxSearchBytes of decoded text are examined.
func MatchText(raw []byte, ix *Index, pattern string, includeHeader bool) bool {
	if pattern == "" {
		return true
	}
	if ix == nil || !ix.Check(len(raw)) {
		ix = Build(raw)
	}
	needle := strings.ToLower(pattern)
	budget := int64(maxSearchBytes)
	var walk func(n *Node) bool
	walk = func(n *Node) bool {
		hdr := raw[n.HS:n.HE]
		var th textproto.Header
		if includeHeader || !n.IsMultipart() {
			th, _ = textproto.ReadHeader(bufio.NewReader(bytes.NewReader(hdr)))
		}
		if includeHeader {
			for f := th.Fields(); f.Next(); {
				if strings.Contains(strings.ToLower(mimeutil.DecodeHeader(f.Value())), needle) {
					return true
				}
			}
		}
		if n.IsMultipart() {
			for _, k := range n.Kids {
				if walk(k) {
					return true
				}
			}
			return false
		}
		if n.Type != "text" && n.Type != "message" {
			return false
		}
		if budget <= 0 {
			return false
		}
		e, _ := gomessage.New(gomessage.Header{Header: th}, bytes.NewReader(raw[n.HE:n.BE]))
		if e == nil {
			return false
		}
		buf, _ := io.ReadAll(io.LimitReader(e.Body, budget))
		budget -= int64(len(buf))
		return bytes.Contains(bytes.ToLower(buf), []byte(needle))
	}
	return walk(ix.Root)
}
