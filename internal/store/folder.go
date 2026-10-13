package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// EAS folder types (MS-ASCMD 2.2.3.170.3) that EASBridge cares about.
const (
	TypeUserGeneric  = 1
	TypeInbox        = 2
	TypeDrafts       = 3
	TypeDeletedItems = 4
	TypeSentItems    = 5
	TypeOutbox       = 6
	TypeUserMail     = 12
)

// IsMailType reports whether an EAS folder type is exposed over IMAP. The
// Outbox (6) is hidden; calendars, contacts, tasks and notes are out of scope.
func IsMailType(t int) bool {
	switch t {
	case TypeUserGeneric, TypeInbox, TypeDrafts, TypeDeletedItems, TypeSentItems, TypeUserMail:
		return true
	}
	return false
}

// FolderInput is a folder as reported by FolderSync.
type FolderInput struct {
	ServerID       string
	ParentServerID string
	DisplayName    string
	EASType        int
}

// FolderDiff reports what ApplyFolders did.
type FolderDiff struct {
	Added   []Folder
	Removed []Folder
	// Changed lists folders whose name, parent, type, path or special-use
	// changed (UID validity is kept).
	Changed []Folder
}

// Empty reports whether nothing changed.
func (d FolderDiff) Empty() bool { return len(d.Added)+len(d.Removed)+len(d.Changed) == 0 }

const folderCols = `id, server_id, parent_server_id, display_name, eas_type, path, special_use,
  uid_validity, uid_next, sync_key, recon_active, recon_epoch, recon_verify_at,
  initial_sync_complete, initial_chain_done`

func scanFolder(sc interface{ Scan(...any) error }) (*Folder, error) {
	var f Folder
	var uv, un, active, complete, chain int64
	err := sc.Scan(&f.ID, &f.ServerID, &f.ParentServerID, &f.DisplayName, &f.EASType, &f.Path, &f.SpecialUse,
		&uv, &un, &f.SyncKey, &active, &f.ReconEpoch, &f.ReconVerifyAt, &complete, &chain)
	if err != nil {
		return nil, err
	}
	f.UIDValidity, f.UIDNext, f.ReconActive = uint32(uv), uint32(un), active != 0
	f.InitialComplete, f.InitialChainDone = complete != 0, chain != 0
	return &f, nil
}

func listFolders(q querier, accountID int64) ([]Folder, error) {
	rows, err := q.Query(`SELECT `+folderCols+` FROM folders WHERE account_id = ? ORDER BY path`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Folder
	for rows.Next() {
		f, err := scanFolder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

func oneFolder(q querier, where string, args ...any) (*Folder, error) {
	f, err := scanFolder(q.QueryRow(`SELECT `+folderCols+` FROM folders WHERE `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return f, err
}

// Folders returns every folder of the account ordered by path.
func (a *Account) Folders() ([]Folder, error) { return listFolders(a.db.r, a.ID) }

// FolderByID looks a folder up by local id.
func (a *Account) FolderByID(id int64) (*Folder, error) {
	return oneFolder(a.db.r, `account_id = ? AND id = ?`, a.ID, id)
}

// FolderByServerID looks a folder up by its EAS ServerId.
func (a *Account) FolderByServerID(sid string) (*Folder, error) {
	return oneFolder(a.db.r, `account_id = ? AND server_id = ?`, a.ID, sid)
}

// FolderByPath looks a folder up by IMAP name (INBOX is case-insensitive).
func (a *Account) FolderByPath(path string) (*Folder, error) {
	if strings.EqualFold(path, "INBOX") {
		return oneFolder(a.db.r, `account_id = ? AND eas_type = ?`, a.ID, TypeInbox)
	}
	return oneFolder(a.db.r, `account_id = ? AND path = ?`, a.ID, path)
}

// SpecialFolder returns the folder carrying special-use attribute use
// (for example `\Sent`).
func (a *Account) SpecialFolder(use string) (*Folder, error) {
	return oneFolder(a.db.r, `account_id = ? AND special_use = ?`, a.ID, use)
}

// Transaction variants.

func (t *Tx) Folders() ([]Folder, error) { return listFolders(t.tx, t.a.ID) }
func (t *Tx) FolderByID(id int64) (*Folder, error) {
	return oneFolder(t.tx, `account_id = ? AND id = ?`, t.a.ID, id)
}
func (t *Tx) FolderByServerID(sid string) (*Folder, error) {
	return oneFolder(t.tx, `account_id = ? AND server_id = ?`, t.a.ID, sid)
}
func (t *Tx) FolderByPath(path string) (*Folder, error) {
	if strings.EqualFold(path, "INBOX") {
		return oneFolder(t.tx, `account_id = ? AND eas_type = ?`, t.a.ID, TypeInbox)
	}
	return oneFolder(t.tx, `account_id = ? AND path = ?`, t.a.ID, path)
}

// ApplyFolders applies a FolderSync result. adds and updates are upserted;
// deleted ServerIds are removed (with their messages). If full is true the
// input is the complete hierarchy (key "0"), and every stored folder it does
// not mention is removed too. Non-mail folders are ignored. Paths and
// special-use attributes are recomputed afterwards.
func (t *Tx) ApplyFolders(adds, updates []FolderInput, deleted []string, full bool) (FolderDiff, error) {
	var diff FolderDiff
	existing, err := t.Folders()
	if err != nil {
		return diff, err
	}
	byServer := map[string]Folder{}
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
			if in, ok := incoming[sid]; !ok || !IsMailType(in.EASType) {
				remove[sid] = true
			}
		}
	}
	for sid, in := range incoming {
		if !IsMailType(in.EASType) {
			remove[sid] = true // a folder that stopped being mail
		}
	}
	for sid := range remove {
		f, ok := byServer[sid]
		if !ok {
			continue
		}
		if _, err := t.tx.Exec(`DELETE FROM folders WHERE id = ?`, f.ID); err != nil {
			return diff, err
		}
		diff.Removed = append(diff.Removed, f)
		delete(byServer, sid)
	}
	sort.Slice(diff.Removed, func(i, j int) bool { return diff.Removed[i].ID < diff.Removed[j].ID })

	for _, sid := range order {
		in := incoming[sid]
		if !IsMailType(in.EASType) {
			continue
		}
		if f, ok := byServer[sid]; ok {
			if f.ParentServerID != in.ParentServerID || f.DisplayName != in.DisplayName || f.EASType != in.EASType {
				if _, err := t.tx.Exec(`UPDATE folders SET parent_server_id = ?, display_name = ?, eas_type = ? WHERE id = ?`,
					in.ParentServerID, in.DisplayName, in.EASType, f.ID); err != nil {
					return diff, err
				}
				f.ParentServerID, f.DisplayName, f.EASType = in.ParentServerID, in.DisplayName, in.EASType
				byServer[sid] = f
				diff.Changed = append(diff.Changed, f)
			}
			continue
		}
		uv, err := t.nextUIDValidity()
		if err != nil {
			return diff, err
		}
		res, err := t.tx.Exec(`INSERT INTO folders(account_id, server_id, parent_server_id, display_name, eas_type, path, uid_validity)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, t.a.ID, in.ServerID, in.ParentServerID, in.DisplayName, in.EASType, "\x00pending:"+in.ServerID, uv)
		if err != nil {
			return diff, err
		}
		id, _ := res.LastInsertId()
		f, err := t.FolderByID(id)
		if err != nil {
			return diff, err
		}
		byServer[sid] = *f
		diff.Added = append(diff.Added, *f)
	}

	// Recompute paths and special-use for the whole tree.
	all := make([]Folder, 0, len(byServer))
	for _, f := range byServer {
		all = append(all, f)
	}
	layout := ComputeLayout(all)
	changedIDs := map[int64]bool{}
	for _, f := range diff.Changed {
		changedIDs[f.ID] = true
	}
	addedIDs := map[int64]bool{}
	for _, f := range diff.Added {
		addedIDs[f.ID] = true
	}
	for _, f := range all {
		l := layout[f.ID]
		if l.Path == f.Path && l.SpecialUse == f.SpecialUse {
			continue
		}
		if _, err := t.tx.Exec(`UPDATE folders SET path = ?, special_use = ? WHERE id = ?`, l.Path, l.SpecialUse, f.ID); err != nil {
			return diff, err
		}
		if !addedIDs[f.ID] && !changedIDs[f.ID] {
			f.Path, f.SpecialUse = l.Path, l.SpecialUse
			diff.Changed = append(diff.Changed, f)
			changedIDs[f.ID] = true
		}
	}
	// Report final values.
	for i := range diff.Added {
		f, _ := t.FolderByID(diff.Added[i].ID)
		if f != nil {
			diff.Added[i] = *f
		}
	}
	for i := range diff.Changed {
		f, _ := t.FolderByID(diff.Changed[i].ID)
		if f != nil {
			diff.Changed[i] = *f
		}
	}
	return diff, nil
}

// CreateFolderLocal records a folder that FolderCreate just made upstream.
func (t *Tx) CreateFolderLocal(in FolderInput) (*Folder, error) {
	diff, err := t.ApplyFolders([]FolderInput{in}, nil, nil, false)
	if err != nil {
		return nil, err
	}
	if len(diff.Added) != 1 {
		return nil, fmt.Errorf("store: folder %q was not added", in.ServerID)
	}
	return &diff.Added[0], nil
}

// Layout is the computed IMAP name and special-use attribute of a folder.
type Layout struct {
	Path       string
	SpecialUse string
}

// ComputeLayout derives IMAP names and special-use attributes for a folder
// tree. It is deterministic: the same input always gives the same output.
func ComputeLayout(folders []Folder) map[int64]Layout {
	byServer := make(map[string]*Folder, len(folders))
	for i := range folders {
		byServer[folders[i].ServerID] = &folders[i]
	}
	component := func(f *Folder) string {
		if f.EASType == TypeInbox {
			return "INBOX"
		}
		return SanitizeName(f.DisplayName)
	}
	fullPath := func(f *Folder) (string, int) {
		parts := []string{component(f)}
		depth := 0
		seen := map[string]bool{f.ServerID: true}
		for p := f.ParentServerID; depth < 32; depth++ {
			pf, ok := byServer[p]
			if !ok || seen[p] {
				break
			}
			seen[p] = true
			parts = append([]string{component(pf)}, parts...)
			p = pf.ParentServerID
		}
		return strings.Join(parts, "/"), depth
	}

	type item struct {
		f     *Folder
		path  string
		depth int
	}
	items := make([]item, 0, len(folders))
	for i := range folders {
		p, d := fullPath(&folders[i])
		items = append(items, item{&folders[i], p, d})
	}
	// Inbox first, then shallow before deep, then by ServerId, so that the
	// real INBOX keeps its name and duplicates get a suffix deterministically.
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if (a.f.EASType == TypeInbox) != (b.f.EASType == TypeInbox) {
			return a.f.EASType == TypeInbox
		}
		if a.depth != b.depth {
			return a.depth < b.depth
		}
		return a.f.ServerID < b.f.ServerID
	})

	out := make(map[int64]Layout, len(folders))
	used := map[string]bool{}
	for _, it := range items {
		path := it.path
		if it.f.EASType != TypeInbox && strings.EqualFold(path, "INBOX") {
			path = "INBOX (2)"
		}
		base := path
		for n := 2; used[strings.ToLower(path)]; n++ {
			path = fmt.Sprintf("%s (%d)", base, n)
		}
		used[strings.ToLower(path)] = true
		out[it.f.ID] = Layout{Path: path}
	}

	// Special-use: by EAS type, then Junk and Archive by name (top level only).
	junk, archive := (*Folder)(nil), (*Folder)(nil)
	junkRank, archiveRank := 1<<30, 1<<30
	for i := range folders {
		f := &folders[i]
		l := out[f.ID]
		switch f.EASType {
		case TypeDrafts:
			l.SpecialUse = `\Drafts`
		case TypeDeletedItems:
			l.SpecialUse = `\Trash`
		case TypeSentItems:
			l.SpecialUse = `\Sent`
		case TypeInbox:
		default:
			if _, hasParent := byServer[f.ParentServerID]; !hasParent {
				if r, ok := junkNameRank(f.DisplayName); ok && (r < junkRank || (r == junkRank && f.ServerID < junk.ServerID)) {
					junk, junkRank = f, r
				}
				if r, ok := archiveNameRank(f.DisplayName); ok && (r < archiveRank || (r == archiveRank && f.ServerID < archive.ServerID)) {
					archive, archiveRank = f, r
				}
			}
		}
		out[f.ID] = l
	}
	if junk != nil {
		l := out[junk.ID]
		l.SpecialUse = `\Junk`
		out[junk.ID] = l
	}
	if archive != nil && archive != junk {
		l := out[archive.ID]
		l.SpecialUse = `\Archive`
		out[archive.ID] = l
	}
	return out
}

// SanitizeName makes a display name safe as one IMAP path component.
func SanitizeName(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\':
			return '_'
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if s == "" || s == "." || s == ".." {
		return "_"
	}
	return s
}

// Name tables for folders Exchange does not mark by type. Matching is done on
// normalised names (lower case, Arabic diacritics and tatweel removed, alef and
// ya variants unified).
var (
	junkNames = []string{
		"junk email", "junk e-mail", "junk", "junk mail", "spam", "bulk mail", "bulk email",
		"البريد العشوائي", "البريد الاكتروني العشوائي", "البريد الالكتروني العشوائي",
		"البريد غير الهام", "البريد الالكتروني غير الهام", "الرسائل غير الهامه", "رسائل غير هامه",
		"الرسائل غير المرغوب فيها", "رسائل غير مرغوب فيها", "غير المرغوب فيه", "الرسائل المزعجه", "الرسائل العشوائيه",
	}
	archiveNames = []string{"archive", "archives", "الارشيف", "ارشيف", "الأرشيف"}
)

// NormalizeName lower-cases and strips marks that vary in Arabic spelling.
func NormalizeName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r == 0x0640 || (r >= 0x064B && r <= 0x065F) || r == 0x0670: // tatweel, harakat
			continue
		case r == 0x0623 || r == 0x0625 || r == 0x0622 || r == 0x0671: // alef variants
			b.WriteRune(0x0627)
		case r == 0x0649: // alef maksura -> ya
			b.WriteRune(0x064A)
		case r == 0x0629: // ta marbuta -> ha
			b.WriteRune(0x0647)
		default:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func nameRank(list []string, display string) (int, bool) {
	n := NormalizeName(display)
	for i, cand := range list {
		if n == NormalizeName(cand) {
			return i, true
		}
	}
	return 0, false
}

func junkNameRank(display string) (int, bool)    { return nameRank(junkNames, display) }
func archiveNameRank(display string) (int, bool) { return nameRank(archiveNames, display) }
