package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// First sync, all or nothing (v0.1.1).
//
// MailFlow raises new-mail alerts for every UID above the highest UID it has
// cached, and skips them only for a folder it has nothing cached for. A folder
// that revealed its messages batch by batch, newest first, would therefore
// make MailFlow alert for every older message that arrives later. So a folder
// shows nothing of its server messages until its first sync has caught up, and
// then shows all of them at once with UIDs in date order.
//
// While the folder is closed (folders.initial_sync_complete = 0) its server
// messages are stored as "staged" rows: ordinary rows of the messages table
// (with MIME, header facts and reference tokens, found by ServerId like any
// other) whose uid is a provisional negative number from folders.stage_next.
// They never touch uid_next, so UIDNEXT does not move while the folder is
// closed, and every read that feeds IMAP filters on uid > 0 (see message.go).
// Local messages (Sent placeholders, local drafts) and messages moved into the
// folder by a client keep getting real UIDs at once, because APPENDUID and
// COPYUID need them; the staged messages are numbered above those later.

// AddStagedMessage stores a server message of a closed folder. It is invisible
// to IMAP and records no event.
func (t *Tx) AddStagedMessage(folderID int64, nm NewMessage) error {
	if len(nm.MIME) == 0 {
		return errors.New("store: empty message")
	}
	if nm.ServerID == "" {
		return errors.New("store: a staged message needs a ServerId")
	}
	var uid int64
	err := t.tx.QueryRow(`UPDATE folders SET stage_next = stage_next - 1 WHERE id = ? AND account_id = ? RETURNING stage_next + 1`,
		folderID, t.a.ID).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: stage message: folder: %w", ErrNotFound)
	}
	if err != nil {
		return err
	}
	_, err = t.insertMessage(folderID, uid, nm)
	return err
}

// SetFlagsPK is SetFlags for a message found by primary key (staged messages
// have no UID). The change is announced only if the message is visible.
func (t *Tx) SetFlagsPK(pk int64, flags Flags, keywords string) (bool, error) {
	folderID, uid, err := t.locate(pk)
	if err != nil {
		return false, err
	}
	res, err := t.tx.Exec(`UPDATE messages SET flags = ?, keywords = ? WHERE id = ? AND (flags != ? OR keywords != ?)`,
		int(flags), keywords, pk, int(flags), keywords)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if uid > 0 {
		t.emit(Event{Kind: EvFlags, FolderID: folderID, UID: uint32(uid), Flags: flags, Keywords: keywords})
	}
	return true, nil
}

// DeleteByPK is DeleteMessage for a message found by primary key. Removing a
// staged message is silent: nobody ever heard of it.
func (t *Tx) DeleteByPK(pk int64) (bool, error) {
	folderID, uid, err := t.locate(pk)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if err := t.dropSentCopyOf(`m.id = ?`, pk); err != nil {
		return false, err
	}
	res, err := t.tx.Exec(`DELETE FROM messages WHERE id = ?`, pk)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if uid > 0 {
		t.emit(Event{Kind: EvExpunge, FolderID: folderID, UID: uint32(uid)})
	}
	return true, nil
}

// locate returns the folder and uid of one of this account's messages.
func (t *Tx) locate(pk int64) (folderID, uid int64, err error) {
	err = t.tx.QueryRow(`SELECT m.folder_id, m.uid FROM messages m JOIN folders f ON f.id = m.folder_id
		WHERE m.id = ? AND f.account_id = ?`, pk, t.a.ID).Scan(&folderID, &uid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, ErrNotFound
	}
	return folderID, uid, err
}

// StagedByMessageID returns the staged messages of a folder with this
// Message-ID (no brackets).
func (t *Tx) StagedByMessageID(folderID int64, id string) ([]Message, error) {
	if id == "" {
		return nil, nil
	}
	return queryMessages(t.tx, `folder_id = ? AND uid < 0 AND message_id = ? ORDER BY id`, folderID, id)
}

// StagedDated returns the staged messages of a folder whose Date header lies in
// [from, to].
func (t *Tx) StagedDated(folderID int64, from, to time.Time) ([]Message, error) {
	return queryMessages(t.tx, `folder_id = ? AND uid < 0 AND date_hdr BETWEEN ? AND ? ORDER BY id`,
		folderID, from.Unix(), to.Unix())
}

// SetInitialChainDone records whether the last data Sync of a closed folder
// reported nothing more to fetch.
func (t *Tx) SetInitialChainDone(folderID int64, done bool) error {
	_, err := t.tx.Exec(`UPDATE folders SET initial_chain_done = ? WHERE id = ? AND account_id = ?`, b2i(done), folderID, t.a.ID)
	return err
}

// RestartInitial throws away what the first sync of a closed folder has
// collected (staged messages, messages waiting for their MIME) so that it can
// start over. Nothing was ever visible, so there is nothing to reconcile.
// Messages that already have a UID (moved in, drafts, placeholders) stay.
func (t *Tx) RestartInitial(folderID int64) error {
	var complete int
	err := t.tx.QueryRow(`SELECT initial_sync_complete FROM folders WHERE id = ? AND account_id = ?`, folderID, t.a.ID).Scan(&complete)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: restart initial sync: %w", ErrNotFound)
	}
	if err != nil {
		return err
	}
	if complete != 0 {
		return nil // an open folder keeps what it has; resets are reconciled instead
	}
	if _, err := t.tx.Exec(`DELETE FROM messages WHERE folder_id = ? AND uid < 0`, folderID); err != nil {
		return err
	}
	if _, err := t.tx.Exec(`DELETE FROM pending_fetch WHERE folder_id = ?`, folderID); err != nil {
		return err
	}
	_, err = t.tx.Exec(`UPDATE folders SET initial_chain_done = 0, stage_next = -1 WHERE id = ?`, folderID)
	return err
}

// PendingBlocking counts the messages of a folder that still hold up its first
// sync: announced, not downloaded, and not yet given up on. An item with
// maxItemFailures failures of its own (see PendingFetch.ItemFailures) no longer
// counts, so one undownloadable message cannot keep a folder closed forever.
func (t *Tx) PendingBlocking(folderID int64, maxItemFailures int) (int, error) {
	var n int
	err := t.tx.QueryRow(`SELECT COUNT(*) FROM pending_fetch WHERE folder_id = ? AND item_failures < ?`,
		folderID, maxItemFailures).Scan(&n)
	return n, err
}

// PendingBlockingCount is PendingBlocking on the read pool.
func (a *Account) PendingBlockingCount(folderID int64, maxItemFailures int) (int, error) {
	var n int
	err := a.db.r.QueryRow(`SELECT COUNT(*) FROM pending_fetch WHERE folder_id = ? AND item_failures < ?`,
		folderID, maxItemFailures).Scan(&n)
	return n, err
}

// CompleteInitialSync opens a closed folder: in one transaction every staged
// message gets a UID, in ascending order of its received date (ties: ServerId),
// above every UID handed out so far, and the folder is marked complete. One
// EvBulkAdd event announces all of them. It returns how many messages became
// visible.
func (t *Tx) CompleteInitialSync(folderID int64) (int, error) {
	var base int64
	err := t.tx.QueryRow(`SELECT uid_next FROM folders WHERE id = ? AND account_id = ?`, folderID, t.a.ID).Scan(&base)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("store: complete initial sync: %w", ErrNotFound)
	}
	if err != nil {
		return 0, err
	}
	// One set-based statement. The window function makes SQLite compute the
	// whole numbering before the first row is updated, so renumbering cannot
	// collide with the rows it has not reached yet (new uids are all above
	// uid_next, old ones are negative).
	res, err := t.tx.Exec(`UPDATE messages SET uid = r.new_uid
		FROM (SELECT id, ? + ROW_NUMBER() OVER (ORDER BY internal_date, server_id) - 1 AS new_uid
		      FROM messages WHERE folder_id = ? AND uid < 0) AS r
		WHERE messages.id = r.id`, base, folderID)
	if err != nil {
		return 0, fmt.Errorf("store: number staged messages: %w", err)
	}
	n, _ := res.RowsAffected()
	if _, err := t.tx.Exec(`UPDATE folders SET uid_next = uid_next + ?, stage_next = -1,
		initial_sync_complete = 1, initial_chain_done = 1 WHERE id = ?`, n, folderID); err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	metas, err := queryMetas(t.tx, `folder_id = ? AND uid >= ?`, folderID, base)
	if err != nil {
		return 0, err
	}
	if int64(len(metas)) != n {
		return 0, fmt.Errorf("store: numbered %d staged messages but found %d", n, len(metas))
	}
	t.emit(Event{Kind: EvBulkAdd, FolderID: folderID, Bulk: metas})
	return int(n), nil
}

// InitialProgress reports, per folder in creation order, whether its first sync
// has finished and how much is still hidden or waiting for its body.
func (a *Account) InitialProgress() ([]FolderProgress, error) {
	rows, err := a.db.r.Query(`SELECT f.id, f.initial_sync_complete,
		(SELECT COUNT(*) FROM messages m WHERE m.folder_id = f.id AND m.uid < 0),
		(SELECT COUNT(*) FROM pending_fetch p WHERE p.folder_id = f.id)
		FROM folders f WHERE f.account_id = ? ORDER BY f.id`, a.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FolderProgress
	for rows.Next() {
		var p FolderProgress
		var done int
		if err := rows.Scan(&p.FolderID, &done, &p.Staged, &p.PendingFetch); err != nil {
			return nil, err
		}
		p.Complete = done != 0
		out = append(out, p)
	}
	return out, rows.Err()
}
