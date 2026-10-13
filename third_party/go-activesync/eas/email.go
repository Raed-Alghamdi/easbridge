// Copyright (C) 2026 Henry Stern
// SPDX-License-Identifier: MIT

package eas

import (
	"encoding/hex"
	"strings"
	"time"

	"github.com/hstern/go-activesync/wbxml"
)

// BodyType maps the AirSyncBase body Type element values.
type BodyType int

const (
	BodyTypeNone  BodyType = 0
	BodyTypePlain BodyType = 1
	BodyTypeHTML  BodyType = 2
	BodyTypeRTF   BodyType = 3
	BodyTypeMIME  BodyType = 4
)

// FilterType is the EAS Sync FilterType element. It limits the date
// window of items returned in a Sync response.
type FilterType int

// Standard EAS FilterType values for email.
const (
	FilterNone       FilterType = 0
	FilterOneDay     FilterType = 1
	FilterThreeDay   FilterType = 2
	FilterOneWeek    FilterType = 3
	FilterTwoWeek    FilterType = 4
	FilterOneMonth   FilterType = 5
	FilterThreeMonth FilterType = 6
	FilterSixMonth   FilterType = 7
)

// EmailItem is a parsed email returned by Sync or ItemOperations Fetch.
//
// Fields are populated from whatever the server included; absent fields
// take their zero value. Servers vary; do not assume any single field is
// always present.
type EmailItem struct {
	ServerID string

	Subject      string
	From         string // raw "Name <addr>" form
	To           string // semicolon-separated, server's exact text
	Cc           string
	Bcc          string
	ReplyTo      string
	Sender       string
	DisplayTo    string
	DateReceived time.Time // zero if not parseable

	Read       bool
	FlagStatus int // 0=clear, 1=complete, 2=active
	// ReadSet and FlagSet report whether the server actually sent the Read
	// and Flag elements. A Sync <Change> carries only the properties that
	// changed, so Read==false alone cannot tell "marked unread" from "not
	// mentioned". easbridge patch 6.
	ReadSet        bool
	FlagSet        bool
	Importance     int // 0=low, 1=normal, 2=high
	HasAttachments bool
	ThreadTopic    string
	ConversationID []byte
	MessageClass   string

	BodyType          BodyType
	BodyEstimatedSize int
	BodyTruncated     bool
	Body              string // populated for BodyTypePlain / BodyTypeHTML / BodyTypeRTF
	BodyMIME          []byte // populated when BodyTypeMIME was requested
	BodyPreview       string // server-generated short preview, if any

	// Categories is the user's freeform tag list ("Work", "Family").
	Categories []string
	// VotingResponse is the recipient's vote on a voting-button mail
	// (Email2 codepage, 14.0+). Empty when the message has no buttons.
	VotingResponse string
	// VotingResponseOptions is the comma-separated list of available
	// vote choices the sender configured.
	VotingResponseOptions string
}

// Flagged is a convenience for FlagStatus != 0.
func (e *EmailItem) Flagged() bool { return e.FlagStatus != 0 }

// parseEmailItem converts an <ApplicationData> element into an EmailItem.
// It walks children by name + codepage, since the same tag name ("Type")
// can mean different things in Email vs AirSyncBase.
func parseEmailItem(serverID string, app *wbxml.Element) EmailItem {
	out := EmailItem{ServerID: serverID}
	if app == nil {
		return out
	}
	for _, c := range app.Children {
		el, ok := c.(*wbxml.Element)
		if !ok {
			continue
		}
		switch el.Codepage {
		case wbxml.PageEmail:
			parseEmailFieldEmailPage(&out, el)
		case wbxml.PageEmail2:
			parseEmailFieldEmail2Page(&out, el)
		case wbxml.PageAirSyncBase:
			parseEmailFieldAirSyncBase(&out, el)
		}
	}
	return out
}

func parseEmailFieldEmailPage(out *EmailItem, el *wbxml.Element) {
	switch el.Name {
	case "Subject":
		out.Subject = el.TextContent()
	case "From":
		out.From = el.TextContent()
	case "To":
		out.To = el.TextContent()
	case "Cc":
		out.Cc = el.TextContent()
	case "ReplyTo":
		out.ReplyTo = el.TextContent()
	case "DisplayTo":
		out.DisplayTo = el.TextContent()
	case "DateReceived":
		out.DateReceived, _ = parseEASTime(el.TextContent())
	case "Read":
		out.Read = el.TextContent() == "1"
		out.ReadSet = true
	case "Importance":
		out.Importance = atoi(el.TextContent())
	case "ThreadTopic":
		out.ThreadTopic = el.TextContent()
	case "MessageClass":
		out.MessageClass = el.TextContent()
	case "Flag":
		// FlagStatus lives in the Flag subtree. An empty <Flag/> means the
		// flag was cleared.
		out.FlagSet = true
		if st := el.Find("FlagStatus"); st != nil {
			out.FlagStatus = atoi(st.TextContent())
		}
	case "Categories":
		for _, c := range el.Children {
			if ce, ok := c.(*wbxml.Element); ok && ce.Name == "Category" {
				out.Categories = append(out.Categories, ce.TextContent())
			}
		}
	}
}

func parseEmailFieldEmail2Page(out *EmailItem, el *wbxml.Element) {
	switch el.Name {
	case "ConversationId":
		// Server sends raw hex bytes; both Sync and Fetch may use either
		// hex-encoded text or opaque bytes. Handle both.
		if t := el.TextContent(); t != "" {
			if b, err := hex.DecodeString(t); err == nil {
				out.ConversationID = b
			}
		}
		for _, c := range el.Children {
			if op, ok := c.(wbxml.Opaque); ok {
				out.ConversationID = []byte(op)
				return
			}
		}
	case "Bcc":
		out.Bcc = el.TextContent()
	case "Sender":
		out.Sender = el.TextContent()
	case "LastVerbExecuted":
		// 0=unknown, 1=ReplyToSender, 2=ReplyAll, 3=Forward.
		// Stored as a string for caller introspection only; not parsed
		// further to keep the EmailItem flat.
	}
}

func parseEmailFieldAirSyncBase(out *EmailItem, el *wbxml.Element) {
	switch el.Name {
	case "Body":
		parseBody(out, el)
	case "Attachments":
		out.HasAttachments = len(el.Children) > 0
	case "Preview":
		// Top-level Preview (some servers); Body's Preview wins if both present.
		if out.BodyPreview == "" {
			out.BodyPreview = el.TextContent()
		}
	case "NativeBodyType":
		// Informational; we already know what we asked for.
	}
}

func parseBody(out *EmailItem, body *wbxml.Element) {
	var data []byte
	var opaque bool
	for _, c := range body.Children {
		el, ok := c.(*wbxml.Element)
		if !ok {
			continue
		}
		if el.Codepage != wbxml.PageAirSyncBase {
			continue
		}
		switch el.Name {
		case "Type":
			out.BodyType = BodyType(atoi(el.TextContent()))
		case "EstimatedDataSize":
			out.BodyEstimatedSize = atoi(el.TextContent())
		case "Truncated":
			out.BodyTruncated = el.TextContent() == "1"
		case "Data":
			// easbridge patch 14: collect the content and decide below, once the
			// Type is known whatever the order of the elements. Upstream took
			// only the first OPAQUE chunk as MIME and read everything else as
			// text, so a Type 4 body that arrives as an inline string (what
			// Exchange 2019 sends) ended up in Body and BodyMIME stayed empty.
			d, op := bodyData(el)
			data = append(data, d...)
			opaque = opaque || op
		case "Preview":
			out.BodyPreview = el.TextContent()
		}
	}
	// easbridge patch 14: a MIME body (Type 4) lands in BodyMIME, as OPAQUE or as
	// an inline string, in one chunk or several. An OPAQUE chunk is taken for MIME
	// whatever the Type says, as before.
	if out.BodyType == BodyTypeMIME || opaque {
		if len(data) > 0 {
			out.BodyMIME = data
		}
		return
	}
	out.Body = string(data)
}

// bodyData returns the content of an airsyncbase:Data element: every chunk in
// document order, inline strings (STR_I) and OPAQUE alike, as a fresh slice, and
// whether any chunk was OPAQUE. easbridge patch 14.
func bodyData(e *wbxml.Element) (data []byte, opaque bool) {
	for _, c := range e.Children {
		switch v := c.(type) {
		case wbxml.Text:
			data = append(data, string(v)...)
		case wbxml.Opaque:
			data = append(data, v...)
			opaque = true
		}
	}
	return data, opaque
}

func firstOpaque(e *wbxml.Element) []byte {
	for _, c := range e.Children {
		if op, ok := c.(wbxml.Opaque); ok {
			out := make([]byte, len(op))
			copy(out, op)
			return out
		}
	}
	return nil
}

// parseEASTime parses the ISO-8601 timestamp formats EAS uses. EAS
// varies between fractional-second and integer-second precision, and
// Z-Push's CalDAV/CardDAV backends pass through the iCalendar basic
// form (YYYYMMDDTHHMMSSZ, no separators).
//
// Returns ok=false if the value is empty or in an unrecognised format,
// so callers can distinguish "field absent" from "field present and
// successfully parsed". The zero time.Time is returned on failure so
// the value is always safe to use even when ok is false.
func parseEASTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05.000Z",
		"2006-01-02T15:04:05Z",
		time.RFC3339Nano,
		time.RFC3339,
		"20060102T150405Z", // iCalendar UTC basic form
		"20060102T150405",  // iCalendar floating basic form
		"20060102",         // iCalendar all-day date
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
