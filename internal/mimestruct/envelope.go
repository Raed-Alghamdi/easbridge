package mimestruct

import (
	"bufio"
	"bytes"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	gomessage "github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"

	"easbridge/internal/mimeutil"
)

// Addr is one address of an envelope. Name is the display name as the header has it:
// RFC 2047 encoded-words are not decoded.
type Addr struct {
	Name    string `json:"n,omitempty"`
	Mailbox string `json:"m,omitempty"`
	Host    string `json:"h,omitempty"`
}

// Env is the IMAP ENVELOPE of a message, in a form that can be stored. Subject and
// the display names hold the header text as it was written (RFC 3501 section 7.4.2:
// "the contents of the header"): an RFC 2047 encoded-word stays an encoded-word, in
// the charset it was written in, and the client decodes it. They used to be decoded
// here, and go-imap's writer then encoded the UTF-8 again as Q-encoded words, so a
// windows-1256 subject reached the client in another charset than it was sent in,
// folded over several lines when it was long.
type Env struct {
	Date      time.Time `json:"d,omitzero"`
	Subject   string    `json:"s,omitempty"`
	From      []Addr    `json:"f,omitempty"`
	Sender    []Addr    `json:"sn,omitempty"`
	ReplyTo   []Addr    `json:"r,omitempty"`
	To        []Addr    `json:"t,omitempty"`
	Cc        []Addr    `json:"c,omitempty"`
	Bcc       []Addr    `json:"b,omitempty"`
	InReplyTo []string  `json:"i,omitempty"`
	MessageID string    `json:"id,omitempty"`
}

// CapHeader returns at most MaxHeaderBytes of a header block, cut at a line
// boundary, so that parsing it takes a bounded amount of work.
func CapHeader(h []byte) []byte {
	if len(h) <= MaxHeaderBytes {
		return h
	}
	h = h[:MaxHeaderBytes]
	if i := bytes.LastIndexByte(h, '\n'); i >= 0 {
		h = h[:i+1]
	}
	return h
}

// EnvelopeOf builds the envelope from the start of a message (only the header
// block is read, and at most MaxHeaderBytes of it). It is go-imap's
// ExtractEnvelope, minus the unbounded input and minus the decoding of
// encoded-words: the subject and the display names are the header text, unfolded
// and trimmed, nothing else (see Env).
func EnvelopeOf(raw []byte) *Env {
	h, _ := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(CapHeader(raw))))
	mh := mail.Header{Header: gomessage.Header{Header: h}}
	date, _ := mh.Date()
	inReplyTo, _ := mh.MsgIDList("In-Reply-To")
	messageID, _ := mh.MessageID()
	return &Env{
		Date:      date,
		Subject:   strings.TrimSpace(h.Get("Subject")),
		From:      addrList(h, "From"),
		Sender:    addrList(h, "Sender"),
		ReplyTo:   addrList(h, "Reply-To"),
		To:        addrList(h, "To"),
		Cc:        addrList(h, "Cc"),
		Bcc:       addrList(h, "Bcc"),
		InReplyTo: inReplyTo,
		MessageID: messageID,
	}
}

// addrList reads an address header with the display names as written. A header
// that is not an address list gives no addresses, as in go-imap.
func addrList(h textproto.Header, k string) []Addr {
	addrs, _ := mimeutil.ParseAddrList(h.Get(k))
	var l []Addr
	for _, a := range addrs {
		mailbox, host, ok := strings.Cut(a.Address, "@")
		if !ok {
			continue
		}
		l = append(l, Addr{Name: a.Name, Mailbox: mailbox, Host: host})
	}
	return l
}

// IMAP converts the envelope to go-imap's type.
func (e *Env) IMAP() *imap.Envelope {
	if e == nil {
		return &imap.Envelope{}
	}
	conv := func(in []Addr) []imap.Address {
		var out []imap.Address
		for _, a := range in {
			out = append(out, imap.Address{Name: a.Name, Mailbox: a.Mailbox, Host: a.Host})
		}
		return out
	}
	return &imap.Envelope{
		Date: e.Date, Subject: e.Subject,
		From: conv(e.From), Sender: conv(e.Sender), ReplyTo: conv(e.ReplyTo),
		To: conv(e.To), Cc: conv(e.Cc), Bcc: conv(e.Bcc),
		InReplyTo: e.InReplyTo, MessageID: e.MessageID,
	}
}
