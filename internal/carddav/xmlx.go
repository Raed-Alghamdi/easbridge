package carddav

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

// Namespaces of the protocol.
const (
	nsDAV  = "DAV:"
	nsCard = "urn:ietf:params:xml:ns:carddav"
	nsCS   = "http://calendarserver.org/ns/"
)

// Limits of what is read from a request.
const (
	maxNodes = 5000
	maxDepth = 24
)

// node is a parsed XML element: just enough tree to read a PROPFIND or a REPORT.
type node struct {
	name     xml.Name
	attrs    map[string]string
	children []*node
	text     string
}

func (n *node) child(space, local string) *node {
	for _, c := range n.children {
		if c.name.Space == space && c.name.Local == local {
			return c
		}
	}
	return nil
}

// errDoctype is returned for a document with a document type declaration or an entity
// declaration: nobody sends one in a CardDAV request, and a hostile one is how entity
// expansion attacks are made.
var errDoctype = errors.New("a DOCTYPE or ENTITY declaration is not accepted")

// hasDoctype reports whether b contains a document type or entity declaration. The
// comparison ignores case and looks anywhere in the bytes, comments and CDATA
// included, so there is nothing to hide it in.
func hasDoctype(b []byte) bool {
	l := bytes.ToLower(b)
	return bytes.Contains(l, []byte("<!doctype")) || bytes.Contains(l, []byte("<!entity"))
}

// parseXML reads a document into a tree. It rejects a DOCTYPE, an encoding other than
// UTF-8, more than maxNodes elements and nesting deeper than maxDepth.
func parseXML(b []byte) (*node, error) {
	if hasDoctype(b) {
		return nil, errDoctype
	}
	if !utf8.Valid(b) {
		return nil, errors.New("the document is not UTF-8")
	}
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = true
	d.Entity = nil // the five predefined entities only
	d.CharsetReader = func(charset string, in io.Reader) (io.Reader, error) {
		if strings.EqualFold(charset, "utf-8") || strings.EqualFold(charset, "utf8") {
			return in, nil
		}
		return nil, errors.New("only UTF-8 is accepted")
	}
	var root *node
	var stack []*node
	count := 0
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			count++
			if count > maxNodes || len(stack) >= maxDepth {
				return nil, errors.New("the document is too large or too deeply nested")
			}
			n := &node{name: t.Name}
			if len(t.Attr) > 0 {
				n.attrs = map[string]string{}
				for _, a := range t.Attr {
					if a.Name.Space == "" || a.Name.Space == "xmlns" {
						n.attrs[a.Name.Local] = a.Value
					}
				}
			}
			if len(stack) > 0 {
				p := stack[len(stack)-1]
				p.children = append(p.children, n)
			} else if root == nil {
				root = n
			} else {
				return nil, errors.New("more than one root element")
			}
			stack = append(stack, n)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].text += string(t)
			}
		case xml.Directive:
			return nil, errDoctype
		}
	}
	if root == nil {
		return nil, errors.New("empty document")
	}
	return root, nil
}

// escapeText escapes character data for an XML element: & < > and the carriage
// return (as &#13;, the way SabreDAV does, so that the CRLF line ends of a vCard
// survive the end-of-line normalization of XML). Characters that XML 1.0 does not allow
// are dropped.
func escapeText(s string) string {
	var b strings.Builder
	b.Grow(len(s) + len(s)/8)
	for _, r := range s {
		switch {
		case r == '&':
			b.WriteString("&amp;")
		case r == '<':
			b.WriteString("&lt;")
		case r == '>':
			b.WriteString("&gt;")
		case r == '\r':
			b.WriteString("&#13;")
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0xFFFE || r == 0xFFFF || (r >= 0xD800 && r <= 0xDFFF):
			// not an XML character
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// safeLocal reports whether a property name can be echoed as an element name.
func safeLocal(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_' || (i > 0 && (r >= '0' && r <= '9' || r == '-' || r == '.'))
		if !ok {
			return false
		}
	}
	return true
}

// safeNS reports whether a namespace URI can be echoed in an attribute.
func safeNS(s string) bool {
	if len(s) > 256 {
		return false
	}
	for _, r := range s {
		if r < 0x21 || r > 0x7e || r == '"' || r == '<' || r == '>' || r == '&' || r == '\'' {
			return false
		}
	}
	return true
}
