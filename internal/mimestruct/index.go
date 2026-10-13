// Package mimestruct builds a bounded, zero-copy index of a message's MIME
// structure and serves BODYSTRUCTURE, ENVELOPE, body sections and text search
// from it.
//
// Mail is hostile input. go-imap's own helpers (ExtractBodyStructure,
// ExtractBodySection) and go-message's multipart reader recurse without a depth
// limit, copy every leaf (and, for message/rfc822, the whole remainder again at
// every nesting level) and have no cap on the number of parts. A single message
// of a few hundred kilobytes with thousands of nested message/rfc822 parts made
// them allocate gigabytes, and a few megabytes of nested multiparts kept a CPU
// busy for minutes. Here the structure is scanned once, over the message's own
// bytes (nothing is copied), with hard limits:
//
//   - at most MaxDepth levels of nesting,
//   - at most MaxParts parts,
//   - at most MaxHeaderBytes of header per part,
//   - a scan budget proportional to the message size.
//
// A message that exceeds a limit is presented as one opaque
// application/octet-stream part ("flat"): it stays visible and its bytes stay
// downloadable. The index is computed once, when the message is stored, and kept
// with it (Encode/Decode); FETCH, SEARCH and section extraction only read it.
//
// go-imap's helpers are still the specification for the output of normal
// messages: the differential tests compare this package with them.
package mimestruct

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	gomessage "github.com/emersion/go-message"
	"github.com/emersion/go-message/textproto"

	"easbridge/internal/mimeutil"
)

// Limits. They are generous for real mail (a deeply forwarded thread has a
// handful of levels, a newsletter a few dozen parts) and tiny compared with an
// attack.
const (
	// Version is the format of the stored index. 2: the envelope holds the header
	// text as written (encoded-words are not decoded); an index of version 1 is
	// rebuilt from the message when it is read.
	Version = 2
	// MaxDepth is the deepest nesting of multipart and message/rfc822 parts.
	MaxDepth = 32
	// MaxParts is the largest number of parts (every node of the tree).
	MaxParts = 1000
	// MaxHeaderBytes is the largest header block parsed per part.
	MaxHeaderBytes = mimeutil.MaxHeaderBytes

	// scanFactor times the message size (plus scanSlack) is the number of bytes
	// the builder may scan before it gives up. Normal mail is scanned a handful
	// of times; the budget only matters for adversarial nesting.
	scanFactor = 32
	scanSlack  = 1 << 20
)

// Index is the structure of one message.
type Index struct {
	V    int   `json:"v"`
	Flat bool  `json:"flat,omitempty"` // a limit was exceeded; Root is one opaque part
	Env  *Env  `json:"env"`
	Root *Node `json:"root"`
}

// Node is one MIME part. Offsets are into the raw message.
type Node struct {
	// HS..HE is the part's header block (HE is just past the blank line that
	// ends it, which is also where the body starts); HE..BE is its body.
	HS int `json:"hs"`
	HE int `json:"he"`
	BE int `json:"be"`

	Type   string            `json:"t,omitempty"`
	Sub    string            `json:"s,omitempty"`
	Params map[string]string `json:"p,omitempty"`
	ID     string            `json:"id,omitempty"`
	Desc   string            `json:"d,omitempty"`
	Enc    string            `json:"e,omitempty"`

	Disp       string            `json:"dp,omitempty"`
	DispParams map[string]string `json:"dpp,omitempty"`
	Lang       []string          `json:"l,omitempty"`
	Loc        string            `json:"loc,omitempty"`

	// Lines counts the lines of a text part, or of a whole message/rfc822 part.
	Lines int64 `json:"n,omitempty"`

	// Kids are the parts of a multipart; Msg is the embedded message of a
	// message/rfc822 part.
	Kids []*Node   `json:"k,omitempty"`
	Msg  *Embedded `json:"m,omitempty"`
}

// Embedded is the message inside a message/rfc822 part.
type Embedded struct {
	Env  *Env  `json:"env"`
	Root *Node `json:"root"`
}

// IsMultipart reports whether the node is a multipart container.
func (n *Node) IsMultipart() bool { return n.Type == "multipart" }

// MediaType returns "type/subtype".
func (n *Node) MediaType() string { return n.Type + "/" + n.Sub }

// Encode serialises the index for storage.
func (ix *Index) Encode() []byte {
	b, err := json.Marshal(ix)
	if err != nil {
		return nil
	}
	return b
}

// Decode reads a stored index. It returns an error for anything that is not a
// valid index of the current version, in which case the caller rebuilds it.
func Decode(b []byte) (*Index, error) {
	if len(b) == 0 {
		return nil, errors.New("mimestruct: no stored index")
	}
	var ix Index
	if err := json.Unmarshal(b, &ix); err != nil {
		return nil, err
	}
	if ix.V != Version || ix.Root == nil || ix.Env == nil {
		return nil, errors.New("mimestruct: unknown index version")
	}
	return &ix, nil
}

// Check verifies that every offset lies inside a message of rawLen bytes and
// that the tree respects the limits, so that a damaged stored index can never
// make a slice expression panic.
func (ix *Index) Check(rawLen int) bool {
	parts := 0
	var walk func(n *Node, depth, lo, hi int) bool
	walk = func(n *Node, depth, lo, hi int) bool {
		parts++
		if n == nil || parts > MaxParts || depth > MaxDepth {
			return false
		}
		if n.HS < lo || n.HS > n.HE || n.HE > n.BE || n.BE > hi || n.BE > rawLen {
			return false
		}
		for _, k := range n.Kids {
			if !walk(k, depth+1, n.HE, n.BE) {
				return false
			}
		}
		if n.Msg != nil {
			if n.Msg.Root == nil || n.Msg.Env == nil || !walk(n.Msg.Root, depth+1, n.HE, n.BE) {
				return false
			}
		}
		return true
	}
	return walk(ix.Root, 0, 0, rawLen)
}

// builder holds the state of one Build.
type builder struct {
	raw    []byte
	parts  int
	budget int
	over   bool
}

// charge accounts for scanning n bytes and gives up when the budget is spent.
func (b *builder) charge(n int) {
	b.budget -= n
	if b.budget < 0 {
		b.over = true
	}
}

// Build indexes a raw message. It never fails and never takes more than a
// bounded amount of work: a message beyond the limits is returned as a flat
// index.
func Build(raw []byte) *Index {
	b := &builder{raw: raw, budget: scanFactor*len(raw) + scanSlack}
	he := headerLen(raw)
	env := EnvelopeOf(raw[:he])
	root := b.node(0, he, len(raw), 0, "")
	if b.over || root == nil {
		return flat(raw, he, env)
	}
	return &Index{V: Version, Env: env, Root: root}
}

// flat presents the whole body as one opaque part.
func flat(raw []byte, he int, env *Env) *Index {
	return &Index{V: Version, Flat: true, Env: env, Root: &Node{
		HS: 0, HE: he, BE: len(raw),
		Type: "application", Sub: "octet-stream", Enc: "7bit",
	}}
}

// headerLen returns the length of the header block at the start of b: up to and
// including the blank line that ends it, or all of b if there is none.
func headerLen(b []byte) int {
	h, _ := mimeutil.SplitHeader(b)
	return len(h)
}

func parseHeader(hdr []byte) gomessage.Header {
	h, _ := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(hdr))) // a malformed line ends the header early, as in go-imap
	return gomessage.Header{Header: h}
}

// node indexes the part whose header is raw[hs:he] and whose body is raw[he:be].
// parent is the media type of the enclosing multipart ("" at the top and inside
// message/rfc822).
func (b *builder) node(hs, he, be, depth int, parent string) *Node {
	if b.over {
		return nil
	}
	b.parts++
	if b.parts > MaxParts || depth > MaxDepth || he-hs > MaxHeaderBytes || hs > he || he > be {
		b.over = true
		return nil
	}
	b.charge(he - hs)
	h := parseHeader(b.raw[hs:he])
	mt, params, _ := h.ContentType()
	if parent == "multipart/digest" && !h.Has("Content-Type") {
		// RFC 2046 5.1.5: the default type inside a digest is message/rfc822.
		mt, params = "message/rfc822", nil
	}
	primary, sub, _ := strings.Cut(mt, "/")
	n := &Node{HS: hs, HE: he, BE: be, Type: primary, Sub: sub, Params: params}
	n.ID = h.Get("Content-Id")
	n.Desc = h.Get("Content-Description")
	n.Enc = h.Get("Content-Transfer-Encoding")
	if disp, dparams, _ := h.ContentDisposition(); disp != "" {
		n.Disp, n.DispParams = disp, dparams
	}
	if v := h.Get("Content-Language"); v != "" {
		// TODO in go-imap as well: CFWS inside the list is not handled.
		for _, l := range strings.Split(v, ",") {
			n.Lang = append(n.Lang, strings.TrimSpace(l))
		}
	}
	n.Loc = h.Get("Content-Location")

	if primary == "multipart" {
		for _, sp := range b.split(he, be, params["boundary"]) {
			if b.over {
				return nil
			}
			ke := sp.s + headerLen(b.raw[sp.s:sp.e])
			b.charge(ke - sp.s)
			kid := b.node(sp.s, ke, sp.e, depth+1, mt)
			if kid == nil {
				return nil
			}
			n.Kids = append(n.Kids, kid)
		}
		return n
	}

	if primary == "text" || mt == "message/rfc822" || mt == "message/global" {
		b.charge(be - he)
		n.Lines = int64(bytes.Count(b.raw[he:be], []byte("\n")))
	}
	if mt == "message/rfc822" || mt == "message/global" {
		eh := he + headerLen(b.raw[he:be])
		b.charge(eh - he)
		root := b.node(he, eh, be, depth+1, "")
		if root == nil {
			return nil
		}
		n.Msg = &Embedded{Env: EnvelopeOf(b.raw[he:eh]), Root: root}
	}
	return n
}
