package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"easbridge/internal/mimestruct"
	"easbridge/internal/mimeutil"
)

const msgCols = `id, folder_id, uid, COALESCE(server_id, ''), kind, flags, keywords, internal_date, size,
  message_id, in_reply_to, refs, subject, date_hdr, to_hdr, placeholder_expires, recon_mark, recon_strikes`

func scanMessage(sc interface{ Scan(...any) error }, withHeader bool) (*Message, error) {
	var m Message
	var uid, kind, flags, idate, dhdr, pexp int64
	dest := []any{&m.PK, &m.FolderID, &uid, &m.ServerID, &kind, &flags, &m.Keywords, &idate, &m.Size,
		&m.MessageID, &m.InReplyTo, &m.References, &m.Subject, &dhdr, &m.To, &pexp, &m.ReconMark, &m.ReconStrikes}
	if withHeader {
		dest = append(dest, &m.Header)
	}
	if err := sc.Scan(dest...); err != nil {
		return nil, err
	}
	m.Kind, m.Flags = Kind(kind), Flags(flags)
	if uid < 0 {
		m.Staged = true // provisional uid of a message that is not visible yet; UID stays 0
	} else {
		m.UID = uint32(uid)
	}
	m.InternalDate = time.Unix(idate, 0).UTC()
	if dhdr != 0 {
		m.DateHdr = time.Unix(dhdr, 0).UTC()
	}
	if pexp != 0 {
		m.PlaceholderExpires = time.Unix(pexp, 0).UTC()
	}
	return &m, nil
}

func getMessage(q querier, withHeader bool, where string, args ...any) (*Message, error) {
	cols := msgCols
	if withHeader {
		cols += ", header"
	}
	m, err := scanMessage(q.QueryRow(`SELECT `+cols+` FROM messages WHERE `+where, args...), withHeader)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}

func queryMessages(q querier, where string, args ...any) ([]Message, error) {
	rows, err := q.Query(`SELECT `+msgCols+` FROM messages WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func listMeta(q querier, folderID int64) ([]MsgMeta, error) {
	return queryMetas(q, `folder_id = ? AND uid > 0`, folderID)
}

// queryMetas lists the visible-message records matching where, in UID order.
func queryMetas(q querier, where string, args ...any) ([]MsgMeta, error) {
	rows, err := q.Query(`SELECT uid, flags, keywords, internal_date, size, kind FROM messages WHERE `+where+` ORDER BY uid`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MsgMeta
	for rows.Next() {
		var m MsgMeta
		var uid, flags, kind int64
		if err := rows.Scan(&uid, &flags, &m.Keywords, &m.InternalDate, &m.Size, &kind); err != nil {
			return nil, err
		}
		m.UID, m.Flags, m.Kind = uint32(uid), Flags(flags), Kind(kind)
		out = append(out, m)
	}
	return out, rows.Err()
}

func counts(q querier, folderID int64) (Counts, error) {
	var c Counts
	var uv, un int64
	err := q.QueryRow(`SELECT f.uid_validity, f.uid_next,
		(SELECT COUNT(*) FROM messages m WHERE m.folder_id = f.id AND m.uid > 0),
		(SELECT COUNT(*) FROM messages m WHERE m.folder_id = f.id AND m.uid > 0 AND (m.flags & 1) = 0)
		FROM folders f WHERE f.id = ?`, folderID).Scan(&uv, &un, &c.Messages, &c.Unseen)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	c.UIDValidity, c.UIDNext = uint32(uv), uint32(un)
	return c, err
}

func mimeOf(q querier, folderID int64, uid uint32) ([]byte, error) {
	var b []byte
	err := q.QueryRow(`SELECT mime.data FROM mime JOIN messages m ON m.id = mime.message_pk
		WHERE m.folder_id = ? AND m.uid = ?`, folderID, uid).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}

// Reads on the read pool ----------------------------------------------------

// ListMessages returns the folder's messages in UID order.
func (a *Account) ListMessages(folderID int64) ([]MsgMeta, error) { return listMeta(a.db.r, folderID) }

// Counts returns what STATUS needs, straight from the database.
func (a *Account) Counts(folderID int64) (Counts, error) { return counts(a.db.r, folderID) }

// Message returns one message with its header block (no MIME body).
func (a *Account) Message(folderID int64, uid uint32) (*Message, error) {
	return getMessage(a.db.r, true, `folder_id = ? AND uid = ?`, folderID, uid)
}

// MessageByServerID returns the message bound to an Exchange ServerId.
func (a *Account) MessageByServerID(folderID int64, serverID string) (*Message, error) {
	return getMessage(a.db.r, false, `folder_id = ? AND server_id = ?`, folderID, serverID)
}

// MIME returns the cached full message.
func (a *Account) MIME(folderID int64, uid uint32) ([]byte, error) {
	return mimeOf(a.db.r, folderID, uid)
}

// Structure returns the stored MIME structure index of a message (see
// internal/mimestruct), nil for a message stored before the index existed.
func (a *Account) Structure(folderID int64, uid uint32) ([]byte, error) {
	var b []byte
	err := a.db.r.QueryRow(`SELECT structure FROM messages WHERE folder_id = ? AND uid = ?`, folderID, uid).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}

// UIDsByMessageID returns the UIDs whose Message-ID equals id (no brackets).
func (a *Account) UIDsByMessageID(folderID int64, id string) ([]uint32, error) {
	return uidQuery(a.db.r, `SELECT uid FROM messages WHERE folder_id = ? AND uid > 0 AND message_id = ? ORDER BY uid`, folderID, id)
}

// UIDsByRef returns the UIDs of messages whose In-Reply-To (kind OpRefInReplyTo)
// or References (OpRefReferences) header contains the token id (no brackets).
func (a *Account) UIDsByRef(folderID int64, kind int, id string) ([]uint32, error) {
	return uidQuery(a.db.r, `SELECT DISTINCT m.uid FROM message_refs r JOIN messages m ON m.id = r.message_pk
		WHERE m.folder_id = ? AND m.uid > 0 AND r.kind = ? AND r.ref = ? ORDER BY m.uid`, folderID, kind, id)
}

// Header reference kinds for UIDsByRef.
const (
	RefInReplyTo  = 1
	RefReferences = 2
)

func uidQuery(q querier, query string, args ...any) ([]uint32, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uint32
	for rows.Next() {
		var u int64
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, uint32(u))
	}
	return out, rows.Err()
}

// SearchRow is the per-message data SEARCH evaluates header criteria on.
type SearchRow struct {
	UID        uint32
	MessageID  string
	InReplyTo  string
	References string
	Subject    string
	To         string
	DateHdr    int64
	Size       int64
}

// SearchRows loads the indexed header facts of every message in a folder.
func (a *Account) SearchRows(folderID int64) (map[uint32]SearchRow, error) {
	rows, err := a.db.r.Query(`SELECT uid, message_id, in_reply_to, refs, subject, to_hdr, date_hdr, size
		FROM messages WHERE folder_id = ? AND uid > 0`, folderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uint32]SearchRow{}
	for rows.Next() {
		var r SearchRow
		var u int64
		if err := rows.Scan(&u, &r.MessageID, &r.InReplyTo, &r.References, &r.Subject, &r.To, &r.DateHdr, &r.Size); err != nil {
			return nil, err
		}
		r.UID = uint32(u)
		out[r.UID] = r
	}
	return out, rows.Err()
}

// Reads inside a transaction ---------------------------------------------

func (t *Tx) ListMessages(folderID int64) ([]MsgMeta, error) { return listMeta(t.tx, folderID) }
func (t *Tx) Counts(folderID int64) (Counts, error)          { return counts(t.tx, folderID) }
func (t *Tx) Message(folderID int64, uid uint32) (*Message, error) {
	return getMessage(t.tx, true, `folder_id = ? AND uid = ?`, folderID, uid)
}
func (t *Tx) MessageByServerID(folderID int64, serverID string) (*Message, error) {
	return getMessage(t.tx, false, `folder_id = ? AND server_id = ?`, folderID, serverID)
}
func (t *Tx) MIME(folderID int64, uid uint32) ([]byte, error) { return mimeOf(t.tx, folderID, uid) }

// ByMessageID returns messages of the folder with this Message-ID (no brackets).
func (t *Tx) ByMessageID(folderID int64, id string) ([]Message, error) {
	if id == "" {
		return nil, nil
	}
	return queryMessages(t.tx, `folder_id = ? AND uid > 0 AND message_id = ? ORDER BY uid`, folderID, id)
}

// ByKind returns the folder's messages of one kind (placeholders, drafts).
func (t *Tx) ByKind(folderID int64, kind Kind) ([]Message, error) {
	return queryMessages(t.tx, `folder_id = ? AND uid > 0 AND kind = ? ORDER BY uid`, folderID, int(kind))
}

// ServerMessages returns every message of the folder bound to a ServerId.
func (t *Tx) ServerMessages(folderID int64) ([]Message, error) {
	return queryMessages(t.tx, `folder_id = ? AND uid > 0 AND server_id IS NOT NULL ORDER BY uid`, folderID)
}

// Writes ------------------------------------------------------------------

// AddMessage stores a message and its MIME and assigns the next UID of the
// folder (strictly increasing, never reused). An EvAdd event is recorded.
func (t *Tx) AddMessage(folderID int64, nm NewMessage) (*Message, error) {
	if len(nm.MIME) == 0 {
		return nil, errors.New("store: empty message")
	}
	var uid int64
	err := t.tx.QueryRow(`UPDATE folders SET uid_next = uid_next + 1 WHERE id = ? AND account_id = ? RETURNING uid_next - 1`,
		folderID, t.a.ID).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("store: add message: folder: %w", ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	when, err := t.insertMessage(folderID, uid, nm)
	if err != nil {
		return nil, err
	}
	t.emit(Event{Kind: EvAdd, FolderID: folderID, UID: uint32(uid), Flags: nm.Flags, Keywords: nm.Keywords,
		Date: when.Unix(), Size: int64(len(nm.MIME)), MsgKind: nm.Kind})
	return t.Message(folderID, uint32(uid))
}

// insertMessage writes the rows of a new message (message, MIME, reference
// tokens) under the given uid and returns its INTERNALDATE.
func (t *Tx) insertMessage(folderID, uid int64, nm NewMessage) (time.Time, error) {
	meta := mimeutil.ParseMeta(nm.MIME)
	when := nm.InternalDate
	if when.IsZero() {
		when = t.now
	}
	var sid any
	if nm.ServerID != "" {
		sid = nm.ServerID
	}
	var dateHdr, pexp int64
	if !meta.Date.IsZero() {
		dateHdr = meta.Date.Unix()
	}
	if !nm.PlaceholderExpires.IsZero() {
		pexp = nm.PlaceholderExpires.Unix()
	}
	// The structure and envelope are computed once, here, with hard limits (see
	// internal/mimestruct): nothing that FETCH does later parses the message.
	structure := mimestruct.Build(nm.MIME).Encode()
	res, err := t.tx.Exec(`INSERT INTO messages(folder_id, uid, server_id, kind, flags, keywords, internal_date, size,
		message_id, in_reply_to, refs, subject, date_hdr, to_hdr, placeholder_expires, header, structure, recon_mark)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		folderID, uid, sid, int(nm.Kind), int(nm.Flags), nm.Keywords, when.Unix(), len(nm.MIME),
		meta.MessageID, meta.InReplyTo, meta.References, meta.Subject, dateHdr, meta.To, pexp, meta.Header, structure, nm.ReconMark)
	if err != nil {
		return when, fmt.Errorf("store: add message: %w", err)
	}
	pk, _ := res.LastInsertId()
	if _, err := t.tx.Exec(`INSERT INTO mime(message_pk, data) VALUES (?, ?)`, pk, nm.MIME); err != nil {
		return when, err
	}
	// The directory: addresses in its domains that this message mentions.
	if err := t.noteHeader(meta.Header, when); err != nil {
		return when, err
	}
	return when, t.insertRefs(pk, meta)
}

// insertRefs writes the In-Reply-To and References tokens of a message.
func (t *Tx) insertRefs(pk int64, meta mimeutil.Meta) error {
	for _, id := range meta.InReplyToIDs {
		if _, err := t.tx.Exec(`INSERT INTO message_refs(message_pk, kind, ref) VALUES (?, ?, ?)`, pk, RefInReplyTo, id); err != nil {
			return err
		}
	}
	for _, id := range meta.ReferenceIDs {
		if _, err := t.tx.Exec(`INSERT INTO message_refs(message_pk, kind, ref) VALUES (?, ?, ?)`, pk, RefReferences, id); err != nil {
			return err
		}
	}
	return nil
}

// ReplaceMIME replaces the stored bytes of a message with another rendering of
// the same message: the MIME, the header block, the structure index and
// everything indexed from the header (Message-ID, subject, recipients, reference
// tokens). The UID, the ServerId, the flags, the keywords and the dates do not
// change. A visible message is announced with EvResize, which carries the new
// size and tells IMAP clients nothing (the message is not new and its flags are
// the same); a staged message is replaced silently.
//
// The engine uses it to give a Sent Items message the bytes the bridge submitted
// instead of Exchange's rendering, which has no Bcc header.
func (t *Tx) ReplaceMIME(pk int64, mime []byte) error {
	if len(mime) == 0 {
		return errors.New("store: empty message")
	}
	folderID, uid, err := t.locate(pk)
	if err != nil {
		return err
	}
	meta := mimeutil.ParseMeta(mime)
	var dateHdr int64
	if !meta.Date.IsZero() {
		dateHdr = meta.Date.Unix()
	}
	structure := mimestruct.Build(mime).Encode()
	if _, err := t.tx.Exec(`UPDATE messages SET size = ?, message_id = ?, in_reply_to = ?, refs = ?, subject = ?,
		date_hdr = ?, to_hdr = ?, header = ?, structure = ? WHERE id = ?`,
		len(mime), meta.MessageID, meta.InReplyTo, meta.References, meta.Subject, dateHdr, meta.To, meta.Header, structure, pk); err != nil {
		return fmt.Errorf("store: replace message: %w", err)
	}
	if _, err := t.tx.Exec(`INSERT INTO mime(message_pk, data) VALUES (?, ?)
		ON CONFLICT(message_pk) DO UPDATE SET data = excluded.data`, pk, mime); err != nil {
		return err
	}
	if _, err := t.tx.Exec(`DELETE FROM message_refs WHERE message_pk = ?`, pk); err != nil {
		return err
	}
	if err := t.insertRefs(pk, meta); err != nil {
		return err
	}
	if uid > 0 {
		t.emit(Event{Kind: EvResize, FolderID: folderID, UID: uint32(uid), Size: int64(len(mime))})
	}
	return nil
}

// SetStructure replaces the stored MIME structure index of a message (nil: none,
// so that the next reader builds it on the fly). Maintenance and tests; normal
// messages get theirs when they are stored.
func (a *Account) SetStructure(folderID int64, uid uint32, blob []byte) error {
	_, err := a.Update(nil, func(t *Tx) error {
		res, err := t.tx.Exec(`UPDATE messages SET structure = ? WHERE folder_id = ? AND uid = ?`, blob, folderID, uid)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
	return err
}

// SetFlags stores new flags and keywords for a message and records EvFlags if
// they differ from what was stored. It reports whether anything changed.
func (t *Tx) SetFlags(folderID int64, uid uint32, flags Flags, keywords string) (bool, error) {
	res, err := t.tx.Exec(`UPDATE messages SET flags = ?, keywords = ?
		WHERE folder_id = ? AND uid = ? AND (flags != ? OR keywords != ?)`,
		int(flags), keywords, folderID, uid, int(flags), keywords)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}
	t.emit(Event{Kind: EvFlags, FolderID: folderID, UID: uid, Flags: flags, Keywords: keywords})
	return true, nil
}

// DeleteMessage removes a message and its MIME and records EvExpunge. Deleting a
// message that does not exist is not an error (it reports false). A message of a
// Sent Items folder takes the copy the bridge kept of its submission with it.
func (t *Tx) DeleteMessage(folderID int64, uid uint32) (bool, error) {
	if err := t.dropSentCopyOf(`m.folder_id = ? AND m.uid = ?`, folderID, uid); err != nil {
		return false, err
	}
	res, err := t.tx.Exec(`DELETE FROM messages WHERE folder_id = ? AND uid = ?`, folderID, uid)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}
	t.emit(Event{Kind: EvExpunge, FolderID: folderID, UID: uid})
	return true, nil
}

// BindServerID attaches an Exchange ServerId to a bridge-only message (a Sent
// placeholder whose server copy arrived) and makes it a normal server message.
// The UID and the cached MIME do not change, so no event is emitted.
func (t *Tx) BindServerID(folderID int64, uid uint32, serverID string) error {
	return t.BindServerIDMarked(folderID, uid, serverID, 0)
}

// BindServerIDMarked is BindServerID for a folder whose full resync is being
// collected: the message is marked as listed in epoch mark (0: leave the mark).
func (t *Tx) BindServerIDMarked(folderID int64, uid uint32, serverID string, mark int64) error {
	res, err := t.tx.Exec(`UPDATE messages SET server_id = ?, kind = 0, placeholder_expires = 0,
		recon_mark = CASE WHEN ? != 0 THEN ? ELSE recon_mark END
		WHERE folder_id = ? AND uid = ? AND server_id IS NULL`, serverID, mark, mark, folderID, uid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: bind %s: %w", "server id", ErrNotFound)
	}
	return nil
}

// ExpirePlaceholders turns Sent placeholders older than their deadline into
// local-only messages and returns how many there were.
func (t *Tx) ExpirePlaceholders() (int, error) {
	res, err := t.tx.Exec(`UPDATE messages SET kind = ?, placeholder_expires = 0
		WHERE kind = ? AND placeholder_expires != 0 AND placeholder_expires <= ?
		AND folder_id IN (SELECT id FROM folders WHERE account_id = ?)`,
		int(KindLocalOnly), int(KindPlaceholder), t.now.Unix(), t.a.ID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// Reconciliation marks -------------------------------------------------------

// MarkSeen records that the given ServerIds were listed by the current full
// resync (epoch) of the folder.
func (t *Tx) MarkSeen(folderID int64, epoch int64, serverIDs []string) error {
	for _, sid := range serverIDs {
		if _, err := t.tx.Exec(`UPDATE messages SET recon_mark = ? WHERE folder_id = ? AND server_id = ?`, epoch, folderID, sid); err != nil {
			return err
		}
	}
	return nil
}

// ReconAbsent returns the server-bound messages the resync of this epoch did
// not list.
func (t *Tx) ReconAbsent(folderID, epoch int64) ([]Message, error) {
	return queryMessages(t.tx, `folder_id = ? AND uid > 0 AND server_id IS NOT NULL AND recon_mark != ? ORDER BY uid`, folderID, epoch)
}

// SetStrikes stores how many consecutive full resyncs missed a message.
func (t *Tx) SetStrikes(folderID int64, uid uint32, strikes int) error {
	_, err := t.tx.Exec(`UPDATE messages SET recon_strikes = ? WHERE folder_id = ? AND uid = ?`, strikes, folderID, uid)
	return err
}

// ClearStrikes forgets the strikes of messages that were listed again.
func (t *Tx) ClearStrikes(folderID, epoch int64) error {
	_, err := t.tx.Exec(`UPDATE messages SET recon_strikes = 0 WHERE folder_id = ? AND server_id IS NOT NULL AND recon_mark = ? AND recon_strikes != 0`, folderID, epoch)
	return err
}

// SetRecon updates a folder's reconciliation bookkeeping.
func (t *Tx) SetRecon(folderID int64, active bool, epoch, verifyAt int64) error {
	a := 0
	if active {
		a = 1
	}
	_, err := t.tx.Exec(`UPDATE folders SET recon_active = ?, recon_epoch = ?, recon_verify_at = ? WHERE id = ?`, a, epoch, verifyAt, folderID)
	return err
}
