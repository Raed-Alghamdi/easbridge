package imapd

import (
	"bufio"
	"bytes"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-message/textproto"

	"easbridge/internal/mimestruct"
	"easbridge/internal/mimeutil"
	"easbridge/internal/store"
)

// Search implements imapserver.Session. Everything is evaluated locally
// (DESIGN section 4): sequence and UID sets, flags, dates and sizes from the
// in-memory listing; Message-ID, In-Reply-To, References, Subject and sent dates
// from indexed columns; any other header from the stored header block; TEXT and
// BODY by scanning the cached MIME. Cheap criteria run first so the expensive
// ones only see the survivors.
func (s *session) Search(kind imapserver.NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions) (*imap.SearchData, error) {
	v, err := s.needView()
	if err != nil {
		return nil, err
	}
	all := v.snapshot()
	m := &matcher{s: s, v: v}
	for _, t := range all {
		if t.seq > m.maxSeq {
			m.maxSeq = t.seq
		}
		if t.meta.UID > m.maxUID {
			m.maxUID = t.meta.UID
		}
	}

	cands := all
	c := criteria
	// Fast path: HEADER Message-ID <exact id> goes through the index.
	if uids, rest, ok := s.indexedMessageID(v, criteria); ok {
		keep := map[uint32]bool{}
		for _, u := range uids {
			keep[u] = true
		}
		cands = cands[:0:0]
		for _, t := range all {
			if keep[t.meta.UID] {
				cands = append(cands, t)
			}
		}
		c = rest
	}

	var data imap.SearchData
	var seqSet imap.SeqSet
	var uidSet imap.UIDSet
	for _, t := range cands {
		if !m.match(t, c) {
			continue
		}
		seqSet.AddNum(t.seq)
		uidSet.AddNum(imap.UID(t.meta.UID))
		num := t.seq
		if kind == imapserver.NumKindUID {
			num = t.meta.UID
		}
		if data.Min == 0 || num < data.Min {
			data.Min = num
		}
		if num > data.Max {
			data.Max = num
		}
		data.Count++
	}
	if kind == imapserver.NumKindUID {
		data.All = uidSet
	} else {
		data.All = seqSet
	}
	return &data, nil
}

// indexedMessageID recognises a top-level HEADER Message-ID <id> criterion with
// a complete, bracketed id and answers it from the index. It returns the
// candidate UIDs and the criteria without that header.
func (s *session) indexedMessageID(v *view, c *imap.SearchCriteria) ([]uint32, *imap.SearchCriteria, bool) {
	for i, hf := range c.Header {
		if !strings.EqualFold(hf.Key, "Message-ID") {
			continue
		}
		val := strings.TrimSpace(hf.Value)
		if len(val) < 3 || val[0] != '<' || val[len(val)-1] != '>' {
			continue
		}
		uids, err := s.acct.Store().UIDsByMessageID(v.folder.ID, val[1:len(val)-1])
		if err != nil {
			return nil, nil, false
		}
		rest := *c
		rest.Header = append(append([]imap.SearchCriteriaHeaderField(nil), c.Header[:i]...), c.Header[i+1:]...)
		return uids, &rest, true
	}
	return nil, nil, false
}

type matcher struct {
	s      *session
	v      *view
	maxSeq uint32
	maxUID uint32

	rowsTried bool
	rows      map[uint32]store.SearchRow
}

func (m *matcher) row(uid uint32) (store.SearchRow, bool) {
	if !m.rowsTried {
		m.rowsTried = true
		m.rows, _ = m.s.acct.Store().SearchRows(m.v.folder.ID)
	}
	r, ok := m.rows[uid]
	return r, ok
}

func inSeqSet(set imap.SeqSet, n, max uint32) bool {
	for _, r := range set {
		start, stop := r.Start, r.Stop
		if start == 0 {
			start = max
		}
		if stop == 0 {
			stop = max
		}
		if start > stop {
			if r.Stop != 0 {
				continue
			}
			start = stop
		}
		if n >= start && n <= stop {
			return true
		}
	}
	return false
}

func inUIDSet(set imap.UIDSet, u, max uint32) bool {
	for _, r := range set {
		start, stop := uint32(r.Start), uint32(r.Stop)
		dynamic := start == 0 || stop == 0
		if start == 0 {
			start = max
		}
		if stop == 0 {
			stop = max
		}
		if start > stop && dynamic {
			start = stop
		}
		if u >= start && u <= stop {
			return true
		}
	}
	return false
}

// matchDate compares calendar dates only (RFC 3501: time and zone are ignored).
func matchDate(t, since, before time.Time) bool {
	t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	if !since.IsZero() {
		sd := time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, time.UTC)
		if t.Before(sd) {
			return false
		}
	}
	if !before.IsZero() {
		bd := time.Date(before.Year(), before.Month(), before.Day(), 0, 0, 0, 0, time.UTC)
		if !t.Before(bd) {
			return false
		}
	}
	return true
}

func (m *matcher) match(t target, c *imap.SearchCriteria) bool {
	for _, ss := range c.SeqNum {
		if !inSeqSet(ss, t.seq, m.maxSeq) {
			return false
		}
	}
	for _, us := range c.UID {
		if !inUIDSet(us, t.meta.UID, m.maxUID) {
			return false
		}
	}
	for _, f := range c.Flag {
		if !hasFlag(t.meta, f) {
			return false
		}
	}
	for _, f := range c.NotFlag {
		if hasFlag(t.meta, f) {
			return false
		}
	}
	if !c.Since.IsZero() || !c.Before.IsZero() {
		if !matchDate(time.Unix(t.meta.InternalDate, 0).UTC(), c.Since, c.Before) {
			return false
		}
	}
	if c.Larger != 0 && t.meta.Size <= c.Larger {
		return false
	}
	if c.Smaller != 0 && t.meta.Size >= c.Smaller {
		return false
	}
	for _, hf := range c.Header {
		if !m.matchHeader(t, hf) {
			return false
		}
	}
	if !c.SentSince.IsZero() || !c.SentBefore.IsZero() {
		r, ok := m.row(t.meta.UID)
		if !ok || r.DateHdr == 0 || !matchDate(time.Unix(r.DateHdr, 0).UTC(), c.SentSince, c.SentBefore) {
			return false
		}
	}
	for _, text := range c.Text {
		if !m.matchBody(t, text, true) {
			return false
		}
	}
	for _, body := range c.Body {
		if !m.matchBody(t, body, false) {
			return false
		}
	}
	for i := range c.Not {
		if m.match(t, &c.Not[i]) {
			return false
		}
	}
	for i := range c.Or {
		if !m.match(t, &c.Or[i][0]) && !m.match(t, &c.Or[i][1]) {
			return false
		}
	}
	return true
}

func containsFold(hay, needle string) bool {
	return strings.Contains(strings.ToLower(hay), strings.ToLower(needle))
}

// matchHeader evaluates HEADER <field> <substring>. An empty substring matches
// messages that have the field at all.
func (m *matcher) matchHeader(t target, hf imap.SearchCriteriaHeaderField) bool {
	key := strings.ToLower(hf.Key)
	switch key {
	case "message-id", "in-reply-to", "references", "subject":
		r, ok := m.row(t.meta.UID)
		if !ok {
			return false
		}
		var val string
		switch key {
		case "message-id":
			if r.MessageID != "" {
				val = "<" + r.MessageID + ">"
			}
		case "in-reply-to":
			val = r.InReplyTo
		case "references":
			val = r.References
		case "subject":
			val = r.Subject
		}
		if hf.Value == "" {
			return val != "" || (key == "subject")
		}
		return containsFold(val, hf.Value)
	}
	msg, err := m.s.acct.Store().Message(m.v.folder.ID, t.meta.UID)
	if err != nil {
		return false
	}
	// At most mimestruct.MaxHeaderBytes of the header are parsed: mail is hostile.
	h, _ := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(mimestruct.CapHeader(msg.Header))))
	fields := h.FieldsByKey(hf.Key)
	if hf.Value == "" {
		return fields.Len() > 0
	}
	for fields.Next() {
		if containsFold(mimeutil.DecodeHeader(fields.Value()), hf.Value) {
			return true
		}
	}
	return false
}

// matchBody evaluates BODY (text parts only) and TEXT (headers and text parts)
// over the message's structure index, which is bounded in depth, parts and
// examined text (see internal/mimestruct).
func (m *matcher) matchBody(t target, pattern string, includeHeader bool) bool {
	if pattern == "" {
		return true
	}
	st := m.s.acct.Store()
	mime, err := st.MIME(m.v.folder.ID, t.meta.UID)
	if err != nil {
		return false
	}
	var ix *mimestruct.Index
	if blob, err := st.Structure(m.v.folder.ID, t.meta.UID); err == nil {
		ix, _ = mimestruct.Decode(blob)
	}
	return mimestruct.MatchText(mime, ix, pattern, includeHeader)
}
