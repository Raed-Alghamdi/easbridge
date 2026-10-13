package engine

import (
	"errors"
	"time"

	"easbridge/internal/mimeutil"
	"easbridge/internal/store"
)

// Sent-copy de-duplication (DESIGN section 9).
//
// MailFlow submits a message over SMTP and then APPENDs its own copy to the
// \Sent folder. With SaveInSentItems=true Exchange saves a copy too. To show one
// message instead of two:
//
//   - APPEND finds a Sent message with the same Message-ID: return that UID,
//     store nothing.
//   - Otherwise APPEND creates a placeholder with a fresh UID (the client needs
//     an APPENDUID). When Exchange's copy later arrives through Sync, it is
//     bound to the placeholder: same UID, no EXISTS, no new message. The match
//     is by Message-ID. Only when one side has no Message-ID at all is the
//     fallback used: normalised Subject, Date within PlaceholderWindow and the
//     same recipients. When both sides have one and they differ, these are two
//     different messages however alike they look (two replies "Thanks" to the
//     same person within minutes), and merging them would lose one.
//   - A placeholder nobody claimed within PlaceholderTTL becomes a local-only
//     message (housekeeping).

// bindPlaceholder binds an incoming Sent item to a waiting placeholder.
func (a *Account) bindPlaceholder(tx *store.Tx, f *store.Folder, serverID string, mime []byte, mark int64) (bool, error) {
	phs, err := tx.ByKind(f.ID, store.KindPlaceholder)
	if err != nil || len(phs) == 0 {
		return false, err
	}
	meta := mimeutil.ParseMeta(mime)
	var match *store.Message
	if meta.MessageID != "" {
		for i := range phs {
			if phs[i].MessageID == meta.MessageID {
				match = &phs[i]
				break
			}
		}
	}
	if match == nil && !meta.Date.IsZero() {
		subj := mimeutil.NormalizeSubject(meta.Subject)
		best := a.cfg.PlaceholderWindow + 1
		for i := range phs {
			p := &phs[i]
			if meta.MessageID != "" && p.MessageID != "" {
				continue // both have a Message-ID and it is not the same one
			}
			if p.DateHdr.IsZero() || mimeutil.NormalizeSubject(p.Subject) != subj || p.To != meta.To {
				continue
			}
			delta := p.DateHdr.Sub(meta.Date)
			if delta < 0 {
				delta = -delta
			}
			if delta <= a.cfg.PlaceholderWindow && delta < best {
				best, match = delta, p
			}
		}
	}
	if match == nil {
		return false, nil
	}
	if err := tx.BindServerIDMarked(f.ID, match.UID, serverID, mark); err != nil {
		return false, err
	}
	// The client's copy keeps its bytes (Exchange's rendering is dropped), and if
	// the bridge submitted this message they are the submitted ones (submitted.go).
	if err := a.adoptSubmitted(tx, match, meta.MessageID); err != nil {
		return false, err
	}
	a.log.Debug("Exchange's Sent copy bound to the client's copy", "folder", f.ID, "uid", match.UID)
	return true, nil
}

// bindStagedPlaceholders applies the rule above at the moment a Sent folder
// opens after its first sync: Exchange's copy of a message the client appended
// in the meantime is among the staged messages, not behind a UID, so it is
// matched here. The staged copy is dropped and the placeholder takes over its
// ServerId and keeps its UID.
func (a *Account) bindStagedPlaceholders(tx *store.Tx, f *store.Folder) error {
	phs, err := tx.ByKind(f.ID, store.KindPlaceholder)
	if err != nil || len(phs) == 0 {
		return err
	}
	for i := range phs {
		p := &phs[i]
		match, err := a.stagedCopyOf(tx, f, p)
		if err != nil {
			return err
		}
		if match == nil {
			continue
		}
		// Before the staged copy goes: deleting a Sent message ends the registration
		// of the bytes the bridge submitted, and the placeholder still needs them.
		if err := a.adoptSubmitted(tx, p, match.MessageID); err != nil {
			return err
		}
		if _, err := tx.DeleteByPK(match.PK); err != nil {
			return err
		}
		if err := tx.BindServerID(f.ID, p.UID, match.ServerID); err != nil {
			return err
		}
		a.log.Debug("Exchange's Sent copy bound to the client's copy while the folder opened", "folder", f.ID, "uid", p.UID)
	}
	return nil
}

// stagedCopyOf finds the staged message that is Exchange's copy of placeholder
// p: same Message-ID, else (only if the placeholder or the candidate has no
// Message-ID) same normalised subject and recipients with a Date within
// PlaceholderWindow (the nearest wins).
func (a *Account) stagedCopyOf(tx *store.Tx, f *store.Folder, p *store.Message) (*store.Message, error) {
	if p.MessageID != "" {
		ms, err := tx.StagedByMessageID(f.ID, p.MessageID)
		if err != nil {
			return nil, err
		}
		if len(ms) > 0 {
			return &ms[0], nil
		}
	}
	if p.DateHdr.IsZero() {
		return nil, nil
	}
	w := a.cfg.PlaceholderWindow
	cands, err := tx.StagedDated(f.ID, p.DateHdr.Add(-w), p.DateHdr.Add(w))
	if err != nil {
		return nil, err
	}
	subj := mimeutil.NormalizeSubject(p.Subject)
	var best *store.Message
	bestDelta := w + 1
	for i := range cands {
		c := &cands[i]
		if p.MessageID != "" && c.MessageID != "" {
			continue // both have a Message-ID and it is not the same one
		}
		if c.DateHdr.IsZero() || mimeutil.NormalizeSubject(c.Subject) != subj || c.To != p.To {
			continue
		}
		delta := c.DateHdr.Sub(p.DateHdr)
		if delta < 0 {
			delta = -delta
		}
		if delta <= w && delta < bestDelta {
			best, bestDelta = c, delta
		}
	}
	return best, nil
}

// AppendResult is what APPEND reports back (UIDPLUS APPENDUID).
type AppendResult struct {
	UID         uint32
	UIDValidity uint32
	// Duplicate is true when an existing message was reused (Sent de-duplication).
	Duplicate bool
}

// Append stores a message the IMAP client uploaded.
//
//   - \Sent: de-duplicated against Exchange's copy, see above;
//   - \Drafts: kept in the bridge only (DESIGN section 9, v0.1);
//   - any other folder: ErrCannot.
func (a *Account) Append(path string, data []byte, flags store.Flags, keywords string, when time.Time, origin any) (AppendResult, error) {
	var res AppendResult
	if int64(len(data)) > a.cfg.MaxMessageBytes {
		return res, ErrTooLarge
	}
	if len(data) == 0 {
		return res, cannot("empty message")
	}
	f, err := a.st.FolderByPath(path)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return res, ErrNoMailbox
		}
		return res, err
	}
	switch f.EASType {
	case store.TypeSentItems:
	case store.TypeDrafts:
	default:
		return res, cannot("messages can only be appended to Sent Items and Drafts")
	}
	_, err = a.commit(origin, func(tx *store.Tx) error {
		cur, err := tx.FolderByID(f.ID)
		if err != nil {
			return err
		}
		res.UIDValidity = cur.UIDValidity
		if cur.EASType == store.TypeDrafts {
			m, err := tx.AddMessage(cur.ID, store.NewMessage{Kind: store.KindLocalDraft, Flags: flags | store.FlagDraft,
				Keywords: keywords, InternalDate: when, MIME: data})
			if err != nil {
				return err
			}
			res.UID = m.UID
			return nil
		}
		meta := mimeutil.ParseMeta(data)
		if meta.MessageID != "" {
			existing, err := tx.ByMessageID(cur.ID, meta.MessageID)
			if err != nil {
				return err
			}
			if len(existing) > 0 {
				res.UID, res.Duplicate = existing[0].UID, true
				return nil
			}
		}
		// The placeholder serves the bytes the bridge submitted, if it submitted this
		// message: the client's copy has no Bcc header (submitted.go).
		stored := data
		if ours, err := submittedCopy(tx, meta.MessageID); err != nil {
			return err
		} else if ours != nil {
			stored = ours
		}
		m, err := tx.AddMessage(cur.ID, store.NewMessage{Kind: store.KindPlaceholder, Flags: flags, Keywords: keywords,
			InternalDate: when, MIME: stored, PlaceholderExpires: tx.Now().Add(a.cfg.PlaceholderTTL)})
		if err != nil {
			return err
		}
		res.UID = m.UID
		return nil
	})
	return res, err
}
