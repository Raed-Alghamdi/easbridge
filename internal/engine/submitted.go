package engine

import (
	"bytes"
	"errors"

	"easbridge/internal/mimeutil"
	"easbridge/internal/store"
	"easbridge/internal/upstream"
)

// The Sent copy shows the Bcc recipients (v0.1.8, real-Exchange finding 4).
//
// EAS SendMail reads its recipients from the message headers, so a recipient that
// exists only in the SMTP envelope (a Bcc) is written into a Bcc header before the
// message goes to Exchange. Exchange delivers it without that header, and the Sent
// Items copy it saves, which comes back through EAS as MIME (Type 4), has no Bcc
// header either, while Outlook shows the Bcc recipients of a sent message. Left
// alone, MailFlow's Sent folder would show the message without them.
//
// So the bridge keeps the bytes it submitted when they carry a Bcc header (a
// message without one loses nothing: Exchange's copy lists the same recipients)
// and serves them as the Sent copy of that message:
//
//   - SendMail registers the bytes in sent_copy, keyed by Message-ID, BEFORE the
//     request leaves. Sending takes seconds through a gateway, and Exchange saves
//     its copy as part of it: a Ping can have a Sync of Sent Items pick the copy up
//     before SendMail returns, and that copy must already find its bytes waiting.
//   - The placeholder that a client's APPEND to Sent creates is given the bytes
//     instead of the client's copy. MailFlow builds its own copy without the Bcc
//     header and APPENDs it right after the SMTP reply; it looks at the new UID at
//     once and does not look again, so the bytes have to be right from the first
//     FETCH.
//   - A copy that comes from Exchange before any APPEND is stored with the bytes
//     (also while the folder is still doing its first sync, and also when the
//     message had to be downloaded with ItemOperations).
//   - A placeholder that binds to Exchange's copy is checked once more and given
//     the bytes if it does not have them yet (a client that APPENDs before it
//     submits). That is the only case in which a visible message changes its bytes
//     under its UID: the UID, the flags and the ServerId stay, and clients are told
//     nothing (store.EvResize only corrects the size kept for RFC822.SIZE).
//
// Once the message that serves the bytes is bound to Exchange's item, the way
// station is not needed any more and the row goes; so does the row of a message that
// a Sent folder opens with (store.Tx.DropSentCopiesOfStaged). A row nobody claims
// (Exchange never saved a copy, the client never APPENDed) is pruned by age and by
// count and size; one whose Sent item is deleted or moved out of Sent Items goes with
// it (store.Tx.DropSentCopy, store.dropSentCopyOf).
//
// What this does not do:
//
//   - A message moved out of Sent Items with IMAP MOVE keeps serving the bytes in
//     its new folder: MOVE copies the stored message. Deleting it with \Deleted and
//     EXPUNGE lets Exchange move the item to Deleted Items, and what Exchange
//     renders there has no Bcc header.
//   - A message submitted without a Message-ID cannot be recognised in Exchange's
//     copy and keeps nothing (nodemailer always writes one).
//   - Messages sent before v0.1.8 keep what they have.

// keepSubmitted registers the bytes about to be submitted. It returns the
// Message-ID they are kept under, "" when nothing was kept: the message has no
// Bcc header, or no Message-ID, or the database refused the write. None of these
// stops the submission, which only costs the Sent copy its Bcc header.
func (a *Account) keepSubmitted(mime []byte) string {
	if !mimeutil.HasField(mime, "bcc") {
		return ""
	}
	id := mimeutil.ParseMeta(mime).MessageID
	if id == "" {
		a.log.Debug("a submitted message has a Bcc header but no Message-ID; its Sent copy will not show the Bcc recipients")
		return ""
	}
	if _, err := a.commit(nil, func(tx *store.Tx) error {
		return tx.KeepSentCopy(id, mime, a.cfg.SubmittedTTL, a.cfg.SubmittedMax, a.cfg.SubmittedMaxBytes)
	}); err != nil {
		a.log.Warn("could not keep the submitted message; its Sent copy will not show the Bcc recipients", "err", err)
		return ""
	}
	return id
}

// forgetSubmitted drops a registration that a failed submission leaves behind.
func (a *Account) forgetSubmitted(id string) {
	if _, err := a.commit(nil, func(tx *store.Tx) error { return tx.DropSentCopy(id) }); err != nil {
		a.log.Warn("could not forget the copy of a message that was not submitted", "err", err)
	}
}

// notSubmitted reports whether a failed SendMail certainly did not hand the
// message to Exchange: the request never left, the circuit breaker was open, or
// Exchange answered with a refusal. After any other failure (a timeout, a
// connection cut while the answer was awaited) the message may have gone out, and
// its Sent copy may still turn up, so the registration stays until it expires.
func notSubmitted(err error) bool {
	var se *upstream.StatusError
	return isPaused(err) || errors.As(err, &se) ||
		errors.Is(err, upstream.ErrNotSent) || errors.Is(err, upstream.ErrAuthFailed) ||
		errors.Is(err, upstream.ErrRemoteWipe) || errors.Is(err, upstream.ErrTooLarge)
}

// submittedCopy returns the bytes the bridge submitted for the message with this
// Message-ID, nil when it has none.
func submittedCopy(tx *store.Tx, messageID string) ([]byte, error) {
	b, err := tx.SentCopy(messageID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	return b, err
}

// adoptSubmitted makes the visible Sent Items message m, which has just been
// bound to Exchange's copy, serve the bytes the bridge submitted, and ends the
// registration. fallbackID is the Message-ID of Exchange's copy, for a message
// that has none of its own.
func (a *Account) adoptSubmitted(tx *store.Tx, m *store.Message, fallbackID string) error {
	id := m.MessageID
	if id == "" {
		id = fallbackID
	}
	ours, err := submittedCopy(tx, id)
	if err != nil || ours == nil {
		return err
	}
	have, err := tx.MIME(m.FolderID, m.UID)
	if err != nil {
		return err
	}
	if !bytes.Equal(have, ours) {
		if err := tx.ReplaceMIME(m.PK, ours); err != nil {
			return err
		}
		a.log.Debug("a Sent message that was bound to Exchange's copy now serves the bytes the bridge submitted", "folder", m.FolderID, "uid", m.UID)
	}
	return tx.DropSentCopy(id)
}

// servedSentMIME decides which bytes a message of Sent Items that is not bound to
// a placeholder is stored (and so served) with:
//
//   - the bytes the bridge submitted, when it submitted this message; claimed is
//     then their Message-ID, which the caller ends the registration of once the
//     message is visible;
//   - otherwise Exchange's rendering with the Bcc recipients Exchange lists with
//     the item (the email2:Bcc property), when it lists any and the rendering has
//     no Bcc header; that is how a message sent from a phone or from OWA shows
//     them, if Exchange gives them out (MS-ASEMAIL 3.2.5.4 lets it);
//   - otherwise Exchange's rendering as it is.
func (a *Account) servedSentMIME(tx *store.Tx, mime []byte, bcc string) (served []byte, claimed string, err error) {
	id := mimeutil.ParseMeta(mime).MessageID
	ours, err := submittedCopy(tx, id)
	if err != nil {
		return nil, "", err
	}
	if ours != nil {
		return ours, id, nil
	}
	if bcc != "" {
		if withBcc, ok := mimeutil.AddBccFromProperty(mime, bcc); ok {
			if a.bccSeen.CompareAndSwap(false, true) {
				a.log.Info("Exchange lists the Bcc recipients of a message in Sent Items (email2:Bcc); they are written into the copy the bridge serves")
			}
			return withBcc, "", nil
		}
	}
	return mime, "", nil
}
