package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"easbridge/internal/vcard"
)

// Contacts folders (docs/DESIGN-contacts.md section 3). Everything here is empty
// unless the contacts feature is on.

// EAS folder types of the Contacts class.
const (
	TypeContacts     = 9  // the default Contacts folder
	TypeUserContacts = 14 // a user-created contacts folder
)

// IsContactsType reports whether an EAS folder type is a Contacts folder.
func IsContactsType(t int) bool { return t == TypeContacts || t == TypeUserContacts }

// ErrStaleKey is returned by ApplyContactBatch when the folder's committed Sync key is
// not the one the batch was fetched with (something else moved it): the batch is to be
// dropped and the folder synced again.
var ErrStaleKey = errors.New("store: the folder's sync key changed underneath the batch")

// DefaultBookID is the path segment of the book of the default Contacts folder.
const DefaultBookID = "contacts"

// DirectoryBookID is the path segment of the directory book.
const DirectoryBookID = "directory"

// ContactFolder is a Contacts folder and the state of its book.
type ContactFolder struct {
	ID          int64
	ServerID    string
	DisplayName string
	EASType     int
	BookID      string
	SyncKey     string
	// Complete: the first full download has finished and the book lists its cards.
	Complete bool
	// Downloading: a download from key 0 is under way; its cards are staged.
	Downloading bool
	// CTag changes whenever any card of the book changes.
	CTag       string
	LastSyncAt int64
}

func ctagOf(createdNs, seq int64) string { return fmt.Sprintf("%x-%d", createdNs, seq) }

const contactFolderCols = `id, server_id, display_name, eas_type, book_id, sync_key, complete, downloading, created_ns, ctag_seq, last_sync_at`

func scanContactFolder(sc interface{ Scan(...any) error }) (*ContactFolder, error) {
	var f ContactFolder
	var complete, downloading, created, seq int64
	if err := sc.Scan(&f.ID, &f.ServerID, &f.DisplayName, &f.EASType, &f.BookID, &f.SyncKey, &complete, &downloading, &created, &seq, &f.LastSyncAt); err != nil {
		return nil, err
	}
	f.Complete, f.Downloading = complete != 0, downloading != 0
	f.CTag = ctagOf(created, seq)
	return &f, nil
}

func listContactFolders(q querier, accountID int64) ([]ContactFolder, error) {
	rows, err := q.Query(`SELECT `+contactFolderCols+` FROM contact_folders WHERE account_id = ?
		ORDER BY (book_id != ?), display_name, book_id`, accountID, DefaultBookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContactFolder
	for rows.Next() {
		f, err := scanContactFolder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

func oneContactFolder(q querier, where string, args ...any) (*ContactFolder, error) {
	f, err := scanContactFolder(q.QueryRow(`SELECT `+contactFolderCols+` FROM contact_folders WHERE `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return f, err
}

// ContactFolders lists the account's contact folders, the default one first.
func (a *Account) ContactFolders() ([]ContactFolder, error) { return listContactFolders(a.db.r, a.ID) }

// ContactFolderByID looks a contact folder up by local id.
func (a *Account) ContactFolderByID(id int64) (*ContactFolder, error) {
	return oneContactFolder(a.db.r, `account_id = ? AND id = ?`, a.ID, id)
}

// ContactFolderByBook looks a contact folder up by the path segment of its book.
func (a *Account) ContactFolderByBook(book string) (*ContactFolder, error) {
	return oneContactFolder(a.db.r, `account_id = ? AND book_id = ?`, a.ID, book)
}

// ContactFolders is the transactional variant.
func (t *Tx) ContactFolders() ([]ContactFolder, error) { return listContactFolders(t.tx, t.a.ID) }

// ContactFolderByID is the transactional variant.
func (t *Tx) ContactFolderByID(id int64) (*ContactFolder, error) {
	return oneContactFolder(t.tx, `account_id = ? AND id = ?`, t.a.ID, id)
}

// ContactFolderDiff reports what ApplyContactFolders did.
type ContactFolderDiff struct {
	Added, Removed, Renamed []ContactFolder
}

// Empty reports whether nothing changed.
func (d ContactFolderDiff) Empty() bool { return len(d.Added)+len(d.Removed)+len(d.Renamed) == 0 }

func bookIDFor(serverID string) string {
	sum := sha256.Sum256([]byte("easbridge contact folder\x00" + serverID))
	return "f-" + hex.EncodeToString(sum[:])[:16]
}

// ApplyContactFolders applies a FolderSync result to the contact folders: the folders
// of type 9 and 14 among adds and updates are added or renamed, deleted ServerIds are
// removed with their cards, a folder that stopped being a contacts folder is removed,
// and with full (the result lists the whole hierarchy) every stored contact folder it
// does not list is removed too. Mail folders are not touched.
func (t *Tx) ApplyContactFolders(adds, updates []FolderInput, deleted []string, full bool) (ContactFolderDiff, error) {
	var diff ContactFolderDiff
	existing, err := t.ContactFolders()
	if err != nil {
		return diff, err
	}
	byServer := map[string]ContactFolder{}
	for _, f := range existing {
		byServer[f.ServerID] = f
	}
	incoming := map[string]FolderInput{}
	var order []string
	for _, in := range append(append([]FolderInput(nil), adds...), updates...) {
		if in.ServerID == "" {
			continue
		}
		if _, dup := incoming[in.ServerID]; !dup {
			order = append(order, in.ServerID)
		}
		incoming[in.ServerID] = in
	}
	remove := map[string]bool{}
	for _, sid := range deleted {
		remove[sid] = true
	}
	if full {
		for sid := range byServer {
			if in, ok := incoming[sid]; !ok || !IsContactsType(in.EASType) {
				remove[sid] = true
			}
		}
	}
	for sid, in := range incoming {
		if !IsContactsType(in.EASType) {
			remove[sid] = true // a folder that stopped being a contacts folder
		}
	}
	var removedIDs []int64
	for sid := range remove {
		if f, ok := byServer[sid]; ok {
			removedIDs = append(removedIDs, f.ID)
			diff.Removed = append(diff.Removed, f)
			delete(byServer, sid)
		}
	}
	sort.Slice(diff.Removed, func(i, j int) bool { return diff.Removed[i].ID < diff.Removed[j].ID })
	for _, id := range removedIDs {
		if _, err := t.tx.Exec(`DELETE FROM contact_folders WHERE id = ?`, id); err != nil {
			return diff, err
		}
	}
	taken := map[string]bool{}
	for _, f := range byServer {
		taken[f.BookID] = true
	}
	for _, sid := range order {
		in := incoming[sid]
		if !IsContactsType(in.EASType) {
			continue
		}
		if f, ok := byServer[sid]; ok {
			if f.DisplayName != in.DisplayName || f.EASType != in.EASType {
				if _, err := t.tx.Exec(`UPDATE contact_folders SET display_name = ?, eas_type = ? WHERE id = ?`, in.DisplayName, in.EASType, f.ID); err != nil {
					return diff, err
				}
				f.DisplayName, f.EASType = in.DisplayName, in.EASType
				diff.Renamed = append(diff.Renamed, f)
			}
			continue
		}
		book := bookIDFor(sid)
		if in.EASType == TypeContacts && !taken[DefaultBookID] {
			book = DefaultBookID
		}
		taken[book] = true
		res, err := t.tx.Exec(`INSERT INTO contact_folders(account_id, server_id, display_name, eas_type, book_id, created_ns) VALUES (?, ?, ?, ?, ?, ?)`,
			t.a.ID, sid, in.DisplayName, in.EASType, book, t.now.UnixNano())
		if err != nil {
			return diff, err
		}
		id, _ := res.LastInsertId()
		nf, err := t.ContactFolderByID(id)
		if err != nil {
			return diff, err
		}
		diff.Added = append(diff.Added, *nf)
	}
	return diff, nil
}

// ContactCardIn is one card of a Sync batch.
type ContactCardIn struct {
	ServerID string
	UID      string
	VCard    []byte
}

// ContactBatch is one Sync round trip of a contact folder, ready to be stored.
type ContactBatch struct {
	// CallKey is the key the call was made with, NewKey the key the server issued.
	CallKey, NewKey string
	More            bool
	// Reset: the server rejected the key and the call started again from "0"; the
	// folder starts a new download (the cards it shows stay until it completes).
	Reset   bool
	Upserts []ContactCardIn // Adds and Changes
	Deleted []string        // ServerIds
}

// ContactBatchResult says what ApplyContactBatch did.
type ContactBatchResult struct {
	// Completed: this batch finished a download and its cards were swapped in.
	Completed bool
	// Changed: the cards the book lists are not the same as before.
	Changed bool
	// Downloading: a download is still under way.
	Downloading bool
}

// ApplyContactBatch stores one Sync round trip of a contact folder in the caller's
// transaction, together with the new key.
//
// A call made with key "0", or one the server answered with a reset, starts a
// download: the staged cards of an earlier, unfinished one are dropped and the cards
// go in staged, invisible. While a download is under way the batches add to the staged
// cards. A data call (not the key "0" one) that reports nothing more to fetch ends the
// download: in this transaction the visible cards are replaced by the staged ones and
// the book becomes complete. Outside a download a batch applies to the visible cards.
// The book's ctag moves whenever the visible cards are not the same afterwards.
func (t *Tx) ApplyContactBatch(folderID int64, b ContactBatch) (ContactBatchResult, error) {
	var res ContactBatchResult
	f, err := t.ContactFolderByID(folderID)
	if err != nil {
		return res, err
	}
	if f.SyncKey != b.CallKey {
		return res, ErrStaleKey
	}
	changed := false
	downloading := f.Downloading
	if b.Reset || b.CallKey == "0" {
		if _, err := t.tx.Exec(`DELETE FROM contact_cards WHERE folder_id = ? AND staged = 1`, f.ID); err != nil {
			return res, err
		}
		downloading = true
	}
	staged := 0
	if downloading {
		staged = 1
	}
	for _, c := range b.Upserts {
		if c.ServerID == "" || c.UID == "" || len(c.VCard) == 0 {
			return res, errors.New("store: a contact card needs a ServerId, a UID and bytes")
		}
		etag := vcard.ETag(c.VCard)
		if !downloading {
			var old string
			err := t.tx.QueryRow(`SELECT etag FROM contact_cards WHERE folder_id = ? AND server_id = ? AND staged = 0`, f.ID, c.ServerID).Scan(&old)
			if err == nil && old == etag {
				continue
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return res, err
			}
			changed = true
		}
		if _, err := t.tx.Exec(`INSERT INTO contact_cards(folder_id, server_id, uid, etag, vcard, staged) VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(folder_id, server_id, staged) DO UPDATE SET uid = excluded.uid, etag = excluded.etag, vcard = excluded.vcard`,
			f.ID, c.ServerID, c.UID, etag, c.VCard, staged); err != nil {
			return res, err
		}
	}
	for _, sid := range b.Deleted {
		r, err := t.tx.Exec(`DELETE FROM contact_cards WHERE folder_id = ? AND server_id = ? AND staged = ?`, f.ID, sid, staged)
		if err != nil {
			return res, err
		}
		if n, _ := r.RowsAffected(); n > 0 && !downloading {
			changed = true
		}
	}
	complete := f.Complete
	if downloading && !b.Reset && b.CallKey != "0" && !b.More {
		before, err := t.cardSignature(f.ID, 0)
		if err != nil {
			return res, err
		}
		after, err := t.cardSignature(f.ID, 1)
		if err != nil {
			return res, err
		}
		if _, err := t.tx.Exec(`DELETE FROM contact_cards WHERE folder_id = ? AND staged = 0`, f.ID); err != nil {
			return res, err
		}
		if _, err := t.tx.Exec(`UPDATE contact_cards SET staged = 0 WHERE folder_id = ? AND staged = 1`, f.ID); err != nil {
			return res, err
		}
		downloading, res.Completed = false, true
		changed = !f.Complete || before != after
		complete = true
	}
	seq := ""
	if changed {
		seq = ", ctag_seq = ctag_seq + 1"
	}
	if _, err := t.tx.Exec(`UPDATE contact_folders SET sync_key = ?, complete = ?, downloading = ?, last_sync_at = ?`+seq+` WHERE id = ?`,
		b.NewKey, boolInt(complete), boolInt(downloading), t.now.Unix(), f.ID); err != nil {
		return res, err
	}
	res.Changed, res.Downloading = changed, downloading
	return res, nil
}

// cardSignature is a digest of the (UID, ETag) pairs of a book's visible or staged
// cards, to tell whether a finished download changes what the book lists.
func (t *Tx) cardSignature(folderID int64, staged int) (string, error) {
	rows, err := t.tx.Query(`SELECT uid, etag FROM contact_cards WHERE folder_id = ? AND staged = ? ORDER BY uid`, folderID, staged)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	h := sha256.New()
	for rows.Next() {
		var uid, etag string
		if err := rows.Scan(&uid, &etag); err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%s\x00", uid, etag)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Card is a card as the CardDAV server serves it.
type Card struct {
	UID   string
	ETag  string
	VCard []byte
}

// ContactCards lists the cards a book shows: the visible ones, and none until the
// book's first full download has finished.
func (a *Account) ContactCards(folderID int64) ([]Card, error) {
	rows, err := a.db.r.Query(`SELECT c.uid, c.etag, c.vcard FROM contact_cards c JOIN contact_folders f ON f.id = c.folder_id
		WHERE c.folder_id = ? AND f.account_id = ? AND f.complete = 1 AND c.staged = 0 ORDER BY c.uid`, folderID, a.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Card
	for rows.Next() {
		var c Card
		if err := rows.Scan(&c.UID, &c.ETag, &c.VCard); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ContactCard returns one visible card of a book by UID.
func (a *Account) ContactCard(folderID int64, uid string) (*Card, error) {
	var c Card
	err := a.db.r.QueryRow(`SELECT c.uid, c.etag, c.vcard FROM contact_cards c JOIN contact_folders f ON f.id = c.folder_id
		WHERE c.folder_id = ? AND f.account_id = ? AND f.complete = 1 AND c.staged = 0 AND c.uid = ?`, folderID, a.ID, uid).Scan(&c.UID, &c.ETag, &c.VCard)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ContactStat is the content-free state of one contact book, for /healthz.
type ContactStat struct {
	Complete    bool
	Downloading bool
	Cards       int
	Staged      int
}

// ContactStats lists the state of every contact book in the order of ContactFolders.
func (a *Account) ContactStats() ([]ContactStat, error) {
	rows, err := a.db.r.Query(`SELECT f.complete, f.downloading,
		COALESCE(SUM(c.staged = 0), 0), COALESCE(SUM(c.staged = 1), 0)
		FROM contact_folders f LEFT JOIN contact_cards c ON c.folder_id = f.id
		WHERE f.account_id = ? GROUP BY f.id ORDER BY (f.book_id != ?), f.display_name, f.book_id`, a.ID, DefaultBookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContactStat
	for rows.Next() {
		var s ContactStat
		var complete, downloading int
		if err := rows.Scan(&complete, &downloading, &s.Cards, &s.Staged); err != nil {
			return nil, err
		}
		s.Complete, s.Downloading = complete != 0, downloading != 0
		if !s.Complete {
			s.Cards = 0 // a book lists nothing before its first download has finished
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ResetContactFolder forgets a contact folder's sync key and starts a fresh download
// the next time it is synced (the cards it shows stay until the new download
// completes). The engine uses it when the committed key cannot be trusted.
func (t *Tx) ResetContactFolder(folderID int64) error {
	_, err := t.tx.Exec(`UPDATE contact_folders SET sync_key = '0' WHERE id = ? AND account_id = ?`, folderID, t.a.ID)
	return err
}

// DeleteContactData removes everything the contacts feature stores for the account:
// the contact folders with their cards, the directory cards, the candidates and the
// directory bookkeeping. It is what a remote wipe does explicitly before the database
// is destroyed, and what turning the feature off does.
func (t *Tx) DeleteContactData() error {
	for _, q := range []string{
		`DELETE FROM contact_cards WHERE folder_id IN (SELECT id FROM contact_folders WHERE account_id = ?)`,
		`DELETE FROM contact_folders WHERE account_id = ?`,
		`DELETE FROM directory_cards WHERE account_id = ?`,
		`DELETE FROM directory_candidates WHERE account_id = ?`,
		`DELETE FROM directory_state WHERE account_id = ?`,
		`DELETE FROM directory_sweep_queue WHERE account_id = ?`,
		`DELETE FROM directory_sweep WHERE account_id = ?`,
	} {
		if _, err := t.tx.Exec(q, t.a.ID); err != nil {
			return err
		}
	}
	// The contact folders are gone, so the hierarchy no longer stands for them: read it again.
	return t.SetContactFoldersKept(false)
}

// PurgeContactData removes the contacts data of every account, in one transaction. The
// service calls it at start when the contacts feature is off, so that turning the
// feature off removes what it stored. It reports how many contact folders and
// directory rows were removed.
func (db *DB) PurgeContactData() (folders, directory int, err error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.w.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	count := func(q string) int {
		var n int
		tx.QueryRow(q).Scan(&n)
		return n
	}
	folders = count(`SELECT COUNT(*) FROM contact_folders`)
	directory = count(`SELECT COUNT(*) FROM directory_cards`) + count(`SELECT COUNT(*) FROM directory_candidates`)
	// With the feature off the stored hierarchy does not stand for the contact folders any
	// more (schema 8): the next start with contacts on reads it again.
	if _, err := tx.Exec(`UPDATE accounts SET contact_folders_kept = 0 WHERE contact_folders_kept != 0`); err != nil {
		return 0, 0, err
	}
	if folders+directory+count(`SELECT COUNT(*) FROM directory_state`)+count(`SELECT COUNT(*) FROM directory_sweep`) == 0 {
		return 0, 0, tx.Commit()
	}
	for _, q := range []string{`DELETE FROM contact_cards`, `DELETE FROM contact_folders`, `DELETE FROM directory_cards`,
		`DELETE FROM directory_candidates`, `DELETE FROM directory_state`,
		`DELETE FROM directory_sweep_queue`, `DELETE FROM directory_sweep`} {
		if _, err := tx.Exec(q); err != nil {
			return 0, 0, err
		}
	}
	return folders, directory, tx.Commit()
}

// PurgeDirectoryData removes the directory rows of every account (used when the
// directory is switched off but contacts stay on).
func (db *DB) PurgeDirectoryData() (int, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.w.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n := 0
	for _, q := range []string{`DELETE FROM directory_cards`, `DELETE FROM directory_candidates`, `DELETE FROM directory_state`,
		`DELETE FROM directory_sweep_queue`, `DELETE FROM directory_sweep`} {
		r, err := tx.Exec(q)
		if err != nil {
			return 0, err
		}
		c, _ := r.RowsAffected()
		n += int(c)
	}
	return n, tx.Commit()
}
