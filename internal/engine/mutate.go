package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"easbridge/internal/store"
	"easbridge/internal/upstream"
)

// FlagOp is how STORE combines the given flags with the current ones.
type FlagOp int

const (
	FlagReplace FlagOp = iota // FLAGS
	FlagAdd                   // +FLAGS
	FlagRemove                // -FLAGS
)

// FlagUpdate reports the flags a message has after a STORE.
type FlagUpdate struct {
	UID      uint32
	Flags    store.Flags
	Keywords string
}

func combineFlags(cur store.Flags, curKw string, op FlagOp, sys store.Flags, kw []string) (store.Flags, string) {
	set := map[string]bool{}
	for _, k := range store.SplitKeywords(curKw) {
		set[k] = true
	}
	nf := cur
	switch op {
	case FlagReplace:
		nf = sys
		set = map[string]bool{}
		for _, k := range kw {
			set[k] = true
		}
	case FlagAdd:
		nf |= sys
		for _, k := range kw {
			set[k] = true
		}
	case FlagRemove:
		nf &^= sys
		for _, k := range kw {
			delete(set, k)
		}
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return nf, strings.Join(keys, " ")
}

// SetFlags changes flags locally and queues the upstream change for the two
// flags Exchange knows (\Seen, \Flagged). \Deleted, \Draft, \Answered and
// keywords stay local. The change events (FETCH FLAGS for other sessions) go
// to the watchers with the given origin, so the originating session can skip
// its own.
func (a *Account) SetFlags(folderID int64, uids []uint32, op FlagOp, sys store.Flags, kw []string, origin any) ([]FlagUpdate, error) {
	var out []FlagUpdate
	queued := false
	_, err := a.commit(origin, func(tx *store.Tx) error {
		for _, uid := range uids {
			m, err := tx.Message(folderID, uid)
			if errors.Is(err, store.ErrNotFound) {
				continue // expunged meanwhile
			}
			if err != nil {
				return err
			}
			nf, nkw := combineFlags(m.Flags, m.Keywords, op, sys, kw)
			changed, err := tx.SetFlags(folderID, uid, nf, nkw)
			if err != nil {
				return err
			}
			out = append(out, FlagUpdate{UID: uid, Flags: nf, Keywords: nkw})
			if !changed || m.ServerID == "" {
				continue
			}
			if (m.Flags^nf)&store.FlagSeen != 0 {
				if err := tx.EnqueueOp(folderID, m.ServerID, store.OpRead, nf.Has(store.FlagSeen), false, origFlag(m.Flags, store.FlagSeen)); err != nil {
					return err
				}
				queued = true
			}
			if (m.Flags^nf)&store.FlagFlagged != 0 {
				if err := tx.EnqueueOp(folderID, m.ServerID, store.OpFlagged, nf.Has(store.FlagFlagged), false, origFlag(m.Flags, store.FlagFlagged)); err != nil {
					return err
				}
				queued = true
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if queued {
		a.poke()
	}
	return out, nil
}

func origFlag(f, bit store.Flags) int {
	if f.Has(bit) {
		return 1
	}
	return 0
}

// Expunge removes the messages flagged \Deleted (all of them, or only those for
// which only returns true) and queues the upstream deletes. Deletes inside
// Deleted Items are permanent; elsewhere Exchange moves the item to Deleted
// Items. It returns the expunged UIDs.
func (a *Account) Expunge(folderID int64, only func(uint32) bool, origin any) ([]uint32, error) {
	var gone []uint32
	_, err := a.commit(origin, func(tx *store.Tx) error {
		f, err := tx.FolderByID(folderID)
		if err != nil {
			return err
		}
		metas, err := tx.ListMessages(folderID)
		if err != nil {
			return err
		}
		for _, m := range metas {
			if !m.Flags.Has(store.FlagDeleted) || (only != nil && !only(m.UID)) {
				continue
			}
			msg, err := tx.Message(folderID, m.UID)
			if err != nil {
				return err
			}
			if msg.ServerID != "" {
				// The delete remembers the flags and the date, to bring the message back
				// if Exchange refuses it.
				if err := tx.EnqueueDelete(folderID, msg.ServerID, f.EASType == store.TypeDeletedItems, msg.Flags, msg.InternalDate); err != nil {
					return err
				}
			}
			if _, err := tx.DeleteMessage(folderID, m.UID); err != nil {
				return err
			}
			gone = append(gone, m.UID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(gone) > 0 {
		a.poke()
	}
	return gone, nil
}

// MovedMessage pairs the source and destination UID of a moved message.
type MovedMessage struct{ Src, Dst uint32 }

// MoveOutcome is the result of Move.
type MoveOutcome struct {
	DstUIDValidity uint32
	Moved          []MovedMessage
	// Failed lists source UIDs that could not be moved.
	Failed []uint32
	// Hidden counts messages that were moved into an Inbox still doing its first
	// sync. They have no UID yet (they are numbered with the rest when the Inbox
	// opens), so they are not in Moved and the MOVE answers without COPYUID.
	Hidden int
}

// Move moves messages to another folder with EAS MoveItems and waits for the
// answer: the destination UIDs are only known once Exchange has assigned the
// new ServerIds. Local-only messages (drafts, placeholders) cannot be moved.
func (a *Account) Move(ctx context.Context, srcID int64, uids []uint32, dstID int64, origin any) (*MoveOutcome, error) {
	var out *MoveOutcome
	err := a.submit(ctx, func(ctx context.Context) error {
		var err error
		out, err = a.move(ctx, srcID, uids, dstID, origin)
		return err
	})
	return out, err
}

func (a *Account) move(ctx context.Context, srcID int64, uids []uint32, dstID int64, origin any) (*MoveOutcome, error) {
	if srcID == dstID {
		return nil, cannot("source and destination are the same folder")
	}
	src, err := a.st.FolderByID(srcID)
	if err != nil {
		return nil, ErrNoMailbox
	}
	dst, err := a.st.FolderByID(dstID)
	if err != nil {
		return nil, ErrNoMailbox
	}
	out := &MoveOutcome{DstUIDValidity: dst.UIDValidity}
	var serverIDs []string
	bySID := map[string]*store.Message{}
	for _, uid := range uids {
		m, err := a.st.Message(srcID, uid)
		if errors.Is(err, store.ErrNotFound) {
			out.Failed = append(out.Failed, uid)
			continue
		}
		if err != nil {
			return nil, err
		}
		if m.ServerID == "" {
			return nil, cannot("messages that exist only in the bridge (drafts, unconfirmed Sent copies) cannot be moved")
		}
		serverIDs = append(serverIDs, m.ServerID)
		bySID[m.ServerID] = m
	}
	if len(serverIDs) == 0 {
		return out, nil
	}
	// Flag changes made a moment ago must reach Exchange before the item gets a
	// new id.
	if err := a.flushNow(ctx, srcID); err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, a.cfg.CallTimeout)
	results, err := a.upstream().MoveItems(cctx, src.ServerID, dst.ServerID, serverIDs)
	cancel()
	if err != nil {
		a.handleUpstreamError(err)
		return nil, wrapUpstream(err)
	}
	moved := map[string]string{}
	for _, r := range results {
		if r.OK && r.DstServerID != "" {
			moved[r.SrcServerID] = r.DstServerID
		}
	}
	_, err = a.commit(origin, func(tx *store.Tx) error {
		dstNow, err := tx.FolderByID(dstID)
		if err != nil {
			return err
		}
		for _, sid := range serverIDs {
			m := bySID[sid]
			dstSID, ok := moved[sid]
			if !ok {
				out.Failed = append(out.Failed, m.UID)
				continue
			}
			mime, err := tx.MIME(srcID, m.UID)
			if err != nil {
				return err
			}
			if err := tx.DropOutbox(srcID, sid); err != nil {
				return err
			}
			if _, err := tx.DeleteMessage(srcID, m.UID); err != nil {
				return err
			}
			arrived := store.NewMessage{ServerID: dstSID, Flags: m.Flags, Keywords: m.Keywords,
				InternalDate: m.InternalDate, MIME: mime, ReconMark: reconMarkOf(dstNow.ReconActive, dstNow.ReconEpoch)}
			if !dstNow.InitialComplete && dstNow.IsInbox() {
				// The Inbox shows nothing yet. A UID handed out now would make
				// MailFlow believe it already knows the Inbox, and it would then
				// raise new-mail alerts for the whole folder once it opens. So the
				// message waits with the others and the MOVE has no COPYUID.
				// (Other folders hand the UID out at once, like Sent placeholders
				// and local drafts: their clients need it, and nothing alerts for
				// them.)
				if err := tx.AddStagedMessage(dstID, arrived); err != nil {
					return err
				}
				out.Hidden++
				continue
			}
			nm, err := tx.AddMessage(dstID, arrived)
			if err != nil {
				return err
			}
			out.Moved = append(out.Moved, MovedMessage{Src: m.UID, Dst: nm.UID})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out.Moved, func(i, j int) bool { return out.Moved[i].Src < out.Moved[j].Src })
	return out, nil
}

// wrapUpstream turns an upstream error into the engine's vocabulary.
func wrapUpstream(err error) error {
	switch {
	case errors.Is(err, upstream.ErrAuthFailed):
		return ErrAuthFailed
	case errors.Is(err, upstream.ErrRemoteWipe):
		return ErrWiped
	case errors.Is(err, upstream.ErrTooLarge):
		return ErrTooLarge
	case errors.Is(err, upstream.ErrUnavailable) && !errors.Is(err, ErrUnavailable):
		// Exchange (or the network) cannot be reached: the IMAP and SMTP layers
		// answer that as "try again later", not as a server bug.
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return err
}

// Folder operations ---------------------------------------------------------

func splitPath(path string) (parent, leaf string) {
	path = strings.Trim(path, "/")
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i], path[i+1:]
	}
	return "", path
}

// hierarchyKey returns a usable FolderSync key, running a FolderSync first if
// none exists yet.
func (a *Account) hierarchyKey() (string, error) {
	key, err := a.st.FolderSyncKey()
	if err != nil {
		return "", err
	}
	if key == "0" || key == "" {
		a.doFolderSync()
		if key, err = a.st.FolderSyncKey(); err != nil || key == "0" {
			return "", ErrUnavailable
		}
	}
	return key, nil
}

// folderCall runs a hierarchy-changing upstream call. On "invalid hierarchy key"
// (status 9) it refreshes the hierarchy once and retries.
func (a *Account) folderCall(ctx context.Context, call func(ctx context.Context, key string) (*upstream.FolderResult, error)) (*upstream.FolderResult, error) {
	for attempt := 0; ; attempt++ {
		key, err := a.hierarchyKey()
		if err != nil {
			return nil, err
		}
		cctx, cancel := context.WithTimeout(ctx, a.cfg.CallTimeout)
		res, err := call(cctx, key)
		cancel()
		if err == nil {
			return res, nil
		}
		var se *upstream.StatusError
		if attempt == 0 && errors.As(err, &se) && se.Code == 9 {
			if rerr := a.resetHierarchyKey(); rerr != nil {
				return nil, rerr
			}
			a.doFolderSync()
			continue
		}
		a.handleUpstreamError(err)
		return nil, err
	}
}

func (a *Account) resetHierarchyKey() error {
	_, err := a.commit(nil, func(tx *store.Tx) error { return tx.SetFolderSyncKey("0") })
	return err
}

// storeFolderResult records a folder change and the new hierarchy key in one
// transaction and verifies the key write (the library ignores such errors).
func (a *Account) storeFolderResult(key string, adds, ups []store.FolderInput, del []string) (store.FolderDiff, error) {
	var diff store.FolderDiff
	_, err := a.commit(nil, func(tx *store.Tx) error {
		var err error
		diff, err = tx.ApplyFolders(adds, ups, del, false)
		if err != nil {
			return err
		}
		if err := tx.SetFolderSyncKey(key); err != nil {
			return err
		}
		if got, err := tx.FolderSyncKey(); err != nil || got != key {
			return errors.New("hierarchy key was not stored")
		}
		return nil
	})
	if err != nil {
		a.log.Error("cannot store a folder change; stopping this mailbox's engine", "err", err)
		go a.m.stopAccount(a)
	}
	return diff, err
}

// CreateFolder creates a mail folder (and missing parents).
func (a *Account) CreateFolder(ctx context.Context, path string) error {
	return a.submit(ctx, func(ctx context.Context) error { return a.createFolder(ctx, path) })
}

func (a *Account) createFolder(ctx context.Context, path string) error {
	path = strings.Trim(path, "/")
	if path == "" || strings.EqualFold(path, "INBOX") {
		return ErrExists
	}
	if _, err := a.st.FolderByPath(path); err == nil {
		return ErrExists
	}
	// Create ancestors first.
	parts := strings.Split(path, "/")
	parentSID := "0"
	for i := range parts {
		cur := strings.Join(parts[:i+1], "/")
		if f, err := a.st.FolderByPath(cur); err == nil {
			parentSID = f.ServerID
			continue
		}
		name := parts[i]
		if name == "" {
			return cannot("empty folder name")
		}
		res, err := a.folderCall(ctx, func(ctx context.Context, key string) (*upstream.FolderResult, error) {
			return a.upstream().CreateFolder(ctx, key, parentSID, name)
		})
		if err != nil {
			var se *upstream.StatusError
			if errors.As(err, &se) && se.Code == 2 {
				return ErrExists
			}
			return wrapUpstream(err)
		}
		diff, err := a.storeFolderResult(res.NewKey, []store.FolderInput{{ServerID: res.ServerID, ParentServerID: parentSID, DisplayName: name, EASType: store.TypeUserMail}}, nil, nil)
		if err != nil {
			return err
		}
		for _, f := range diff.Added {
			a.markDirty(f.ID)
		}
		parentSID = res.ServerID
	}
	return nil
}

func systemFolder(f *store.Folder) bool {
	return f.EASType != store.TypeUserMail && f.EASType != store.TypeUserGeneric
}

// RenameFolder renames or moves a user folder.
func (a *Account) RenameFolder(ctx context.Context, from, to string) error {
	return a.submit(ctx, func(ctx context.Context) error {
		f, err := a.st.FolderByPath(strings.Trim(from, "/"))
		if err != nil {
			return ErrNoMailbox
		}
		if systemFolder(f) || f.SpecialUse != "" {
			return cannot("system folders cannot be renamed")
		}
		to = strings.Trim(to, "/")
		if _, err := a.st.FolderByPath(to); err == nil {
			return ErrExists
		}
		parentPath, leaf := splitPath(to)
		parentSID := "0"
		if parentPath != "" {
			p, err := a.st.FolderByPath(parentPath)
			if err != nil {
				return ErrNoMailbox
			}
			parentSID = p.ServerID
		}
		res, err := a.folderCall(ctx, func(ctx context.Context, key string) (*upstream.FolderResult, error) {
			return a.upstream().RenameFolder(ctx, key, f.ServerID, parentSID, leaf)
		})
		if err != nil {
			return wrapUpstream(err)
		}
		_, err = a.storeFolderResult(res.NewKey, nil, []store.FolderInput{{ServerID: f.ServerID, ParentServerID: parentSID, DisplayName: leaf, EASType: f.EASType}}, nil)
		return err
	})
}

// DeleteFolder deletes an empty-of-subfolders user folder and its messages.
func (a *Account) DeleteFolder(ctx context.Context, path string) error {
	return a.submit(ctx, func(ctx context.Context) error {
		f, err := a.st.FolderByPath(strings.Trim(path, "/"))
		if err != nil {
			return ErrNoMailbox
		}
		if systemFolder(f) || f.SpecialUse != "" {
			return cannot("system folders cannot be deleted")
		}
		all, err := a.st.Folders()
		if err != nil {
			return err
		}
		for _, c := range all {
			if c.ParentServerID == f.ServerID {
				return cannot("the folder has subfolders; delete them first")
			}
		}
		res, err := a.folderCall(ctx, func(ctx context.Context, key string) (*upstream.FolderResult, error) {
			return a.upstream().DeleteFolder(ctx, key, f.ServerID)
		})
		if err != nil {
			return wrapUpstream(err)
		}
		if _, err := a.storeFolderResult(res.NewKey, nil, nil, []string{f.ServerID}); err != nil {
			return err
		}
		a.wmu.Lock()
		delete(a.fs, f.ID)
		a.wmu.Unlock()
		return nil
	})
}

// SendMail hands a complete message to Exchange (saved in Sent Items). The
// caller has already added Bcc recipients to the headers. A message with a Bcc
// header is also kept as submitted, to be served as its Sent copy: Exchange's own
// copy has no Bcc header.
func (a *Account) SendMail(ctx context.Context, mime []byte) error {
	switch {
	case a.stopped.Load():
		return ErrUnavailable
	case a.wiped.Load():
		return ErrWiped
	case a.authFailed.Load():
		return ErrAuthFailed
	}
	if int64(len(mime)) > a.cfg.MaxMessageBytes {
		return ErrTooLarge
	}
	// Before the request leaves, so that the Sent copy Exchange saves finds the
	// bytes waiting however fast it is picked up (see submitted.go).
	kept := ""
	if a.brk.remaining() <= 0 {
		kept = a.keepSubmitted(mime)
	}
	cctx, cancel := context.WithTimeout(ctx, a.cfg.CallTimeout)
	defer cancel()
	if err := a.upstream().SendMail(cctx, mime, true); err != nil {
		a.handleUpstreamError(err)
		if kept != "" && notSubmitted(err) {
			a.forgetSubmitted(kept)
		}
		return err
	}
	a.afterSend()
	return nil
}

// afterSend makes sure Exchange's Sent Items copy is picked up promptly, so the
// client's APPEND de-duplication can find it (Ping would also report it).
func (a *Account) afterSend() {
	f, err := a.st.SpecialFolder(`\Sent`)
	if err != nil {
		return
	}
	for _, d := range []time.Duration{2 * time.Second, 10 * time.Second} {
		time.AfterFunc(d, func() {
			if a.ctx.Err() == nil {
				a.markDirty(f.ID)
			}
		})
	}
}
