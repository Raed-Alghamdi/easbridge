package store

import (
	"database/sql"
	"errors"
	"strconv"
	"time"
)

// The message as the bridge submitted it (v0.1.8).
//
// A message that goes out through SMTP carries a Bcc header the bridge wrote for
// the recipients that exist only in the SMTP envelope, because EAS reads the
// recipients from the headers. Exchange delivers it without that header and
// saves the Sent Items copy without it too, so Exchange's own copy shows the
// message without its Bcc recipients, while Outlook, which reads them from the
// sender's mailbox, shows them. The bridge keeps the bytes it submitted in
// sent_copy, keyed by Message-ID, from before the submission until Exchange's
// copy of the message has appeared in Sent Items and the message there serves
// these bytes. The row is a way station: the permanent home of the bytes is the
// message itself (messages and mime), which is what FETCH reads and what an
// expunge, a move or a remote wipe takes care of.

// KeepSentCopy stores the bytes of a message the bridge submits to Exchange, under
// its Message-ID (no angle brackets), replacing an earlier copy under the same
// key. It then prunes the table with the given limits (PruneSentCopies).
func (t *Tx) KeepSentCopy(messageID string, mime []byte, ttl time.Duration, maxEntries int, maxBytes int64) error {
	if messageID == "" || len(mime) == 0 {
		return errors.New("store: a kept copy needs a Message-ID and bytes")
	}
	if _, err := t.tx.Exec(`INSERT INTO sent_copy(account_id, message_id, created_at, data) VALUES (?, ?, ?, ?)
		ON CONFLICT(account_id, message_id) DO UPDATE SET created_at = excluded.created_at, data = excluded.data`,
		t.a.ID, messageID, t.now.Unix(), mime); err != nil {
		return err
	}
	_, err := t.PruneSentCopies(ttl, maxEntries, maxBytes)
	return err
}

// SentCopy returns the bytes the bridge submitted for the message with this
// Message-ID, or ErrNotFound.
func (t *Tx) SentCopy(messageID string) ([]byte, error) {
	if messageID == "" {
		return nil, ErrNotFound
	}
	var b []byte
	err := t.tx.QueryRow(`SELECT data FROM sent_copy WHERE account_id = ? AND message_id = ?`, t.a.ID, messageID).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}

// DropSentCopy forgets the kept copy of the message with this Message-ID (nothing
// happens when there is none).
func (t *Tx) DropSentCopy(messageID string) error {
	if messageID == "" {
		return nil
	}
	_, err := t.tx.Exec(`DELETE FROM sent_copy WHERE account_id = ? AND message_id = ?`, t.a.ID, messageID)
	return err
}

// PruneSentCopies bounds the kept copies, like everything else the bridge stores.
// Copies older than ttl go first; then, newest first, copies are kept while at
// most maxEntries are kept and their bytes stay within maxBytes, and every older
// one goes. The newest copy always stays, however big it is (a limit of 0 or less
// switches the limit off). It returns how many rows it removed.
func (t *Tx) PruneSentCopies(ttl time.Duration, maxEntries int, maxBytes int64) (int, error) {
	removed := 0
	if ttl > 0 {
		res, err := t.tx.Exec(`DELETE FROM sent_copy WHERE account_id = ? AND created_at <= ?`, t.a.ID, t.now.Add(-ttl).Unix())
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		removed += int(n)
	}
	if maxEntries <= 0 && maxBytes <= 0 {
		return removed, nil
	}
	rows, err := t.tx.Query(`SELECT id, length(data) FROM sent_copy WHERE account_id = ? ORDER BY created_at DESC, id DESC`, t.a.ID)
	if err != nil {
		return removed, err
	}
	var drop []int64
	kept, total, dropping := 0, int64(0), false
	for rows.Next() {
		var id, size int64
		if err := rows.Scan(&id, &size); err != nil {
			rows.Close()
			return removed, err
		}
		if !dropping && kept > 0 && ((maxEntries > 0 && kept >= maxEntries) || (maxBytes > 0 && total+size > maxBytes)) {
			dropping = true
		}
		if dropping {
			drop = append(drop, id)
			continue
		}
		kept++
		total += size
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return removed, err
	}
	for _, id := range drop {
		if _, err := t.tx.Exec(`DELETE FROM sent_copy WHERE id = ?`, id); err != nil {
			return removed, err
		}
	}
	return removed + len(drop), nil
}

// dropSentCopyOf forgets the kept copy of the message that matches where (a
// condition on messages m) when that message sits in a Sent Items folder, just
// before the message itself is deleted: a Sent item that is deleted, or moved out
// of Sent Items, takes the way station with it.
func (t *Tx) dropSentCopyOf(where string, args ...any) error {
	_, err := t.tx.Exec(`DELETE FROM sent_copy WHERE account_id = ? AND message_id != '' AND message_id IN (
		SELECT m.message_id FROM messages m JOIN folders f ON f.id = m.folder_id
		WHERE f.eas_type = `+strconv.Itoa(TypeSentItems)+` AND `+where+`)`, append([]any{t.a.ID}, args...)...)
	return err
}

// DropSentCopiesOfStaged forgets the kept copies of the messages a Sent Items folder
// has staged, at the moment the folder opens: they were stored with the submitted
// bytes (a placeholder that claimed one has taken it and dropped it already), so the
// way station has nothing left to do.
func (t *Tx) DropSentCopiesOfStaged(folderID int64) error {
	_, err := t.tx.Exec(`DELETE FROM sent_copy WHERE account_id = ? AND message_id != '' AND message_id IN (
		SELECT message_id FROM messages WHERE folder_id = ? AND uid < 0)`, t.a.ID, folderID)
	return err
}

// SentCopies reports how many submitted messages are kept and how many bytes they
// take (health checks and tests).
func (a *Account) SentCopies() (n int, bytes int64, err error) {
	err = a.db.r.QueryRow(`SELECT COUNT(*), COALESCE(SUM(length(data)), 0) FROM sent_copy WHERE account_id = ?`, a.ID).Scan(&n, &bytes)
	return n, bytes, err
}
