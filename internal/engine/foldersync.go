package engine

import (
	"errors"
	"fmt"

	"easbridge/internal/store"
	"easbridge/internal/upstream"
)

// resumeFromStore schedules the work a previous run left behind: every folder
// is synced once at start-up (mail may have arrived while the bridge was down)
// and the folder hierarchy is refreshed.
func (a *Account) resumeFromStore() {
	a.requestFolderSync()
	a.markAllDirty()
	a.wakeFetch()
	a.resumeContacts()
}

// doFolderSync runs FolderSync and commits the hierarchy together with its key.
func (a *Account) doFolderSync() {
	key, err := a.st.FolderSyncKey()
	if err != nil {
		a.log.Error("read hierarchy key", "err", err)
		a.folderFailed()
		return
	}
	if a.cfg.ContactsEnabled && key != "0" {
		// A hierarchy key committed while the contacts were off (every key of 0.1.8) stands
		// for a hierarchy without the Contacts folders: an incremental FolderSync never
		// mentions them. Read the hierarchy again in full, once; the commit below records it.
		kept, err := a.st.ContactFoldersKept()
		if err != nil {
			a.log.Error("read the contact folder marker", "err", err)
			a.folderFailed()
			return
		}
		if !kept {
			a.log.Info("contacts are on and the stored folder hierarchy was taken without the Contacts folders: reading the hierarchy again in full, once")
			key = "0"
		}
	}
	ctx, cancel := a.callCtx()
	delta, err := a.upstream().FolderSync(ctx, key)
	cancel()
	if err != nil {
		if isPaused(err) || a.handleUpstreamError(err) {
			return
		}
		a.log.Warn("FolderSync failed", "err", err)
		a.folderFailed()
		return
	}
	var diff store.FolderDiff
	var cdiff store.ContactFolderDiff
	_, err = a.commit(nil, func(tx *store.Tx) error {
		var adds, ups []store.FolderInput
		for _, f := range delta.Added {
			adds = append(adds, toInput(f))
		}
		for _, f := range delta.Updated {
			ups = append(ups, toInput(f))
		}
		var err error
		diff, err = tx.ApplyFolders(adds, ups, delta.Deleted, delta.Full)
		if err != nil {
			return err
		}
		if a.cfg.ContactsEnabled {
			// The Contacts folders (types 9 and 14) are kept next to the mail folders.
			if cdiff, err = tx.ApplyContactFolders(adds, ups, delta.Deleted, delta.Full); err != nil {
				return err
			}
		}
		// Recorded with the key it belongs to: with the feature off the next hierarchy
		// drops the Contacts folders again, and turning it on later reads it once more.
		if err := tx.SetContactFoldersKept(a.cfg.ContactsEnabled); err != nil {
			return err
		}
		if err := tx.SetFolderSyncKey(delta.NewKey); err != nil {
			return err
		}
		// The library ignores write errors for the hierarchy key, so verify.
		if got, err := tx.FolderSyncKey(); err != nil || got != delta.NewKey {
			return fmt.Errorf("hierarchy key was not stored (got %q, err %v)", got, err)
		}
		return nil
	})
	if err != nil {
		// Without the key the next FolderSync would fail (status 9) and the
		// state would drift; stop loudly instead of limping on.
		a.log.Error("cannot store the folder hierarchy; stopping this mailbox's engine", "err", err)
		go a.m.stopAccount(a)
		return
	}
	a.wmu.Lock()
	a.wantFolderSync = false
	a.folderFailures = 0
	a.wmu.Unlock()
	if a.cfg.ContactsEnabled {
		a.log.Info("folder sync done", "folders_added", len(diff.Added), "folders_removed", len(diff.Removed),
			"folders_changed", len(diff.Changed), "full", delta.Full, "contact_folders_added", len(cdiff.Added), "contact_folders_removed", len(cdiff.Removed))
	} else {
		a.log.Info("folder sync done", "folders_added", len(diff.Added), "folders_removed", len(diff.Removed),
			"folders_changed", len(diff.Changed), "full", delta.Full)
	}
	for _, f := range diff.Removed {
		a.wmu.Lock()
		delete(a.fs, f.ID)
		a.wmu.Unlock()
	}
	for _, f := range diff.Added {
		a.markDirty(f.ID)
	}
	a.contactFoldersChanged(cdiff)
	a.signalClean()
}

func toInput(f upstream.Folder) store.FolderInput {
	return store.FolderInput{ServerID: f.ServerID, ParentServerID: f.ParentID, DisplayName: f.DisplayName, EASType: f.Type}
}

func (a *Account) folderFailed() {
	a.wmu.Lock()
	a.folderFailures++
	a.folderRetryAt = a.backoff(a.folderFailures)
	a.wmu.Unlock()
}

// errNoHierarchyKey is returned by the folder operations when the stored
// hierarchy key is unusable and a FolderSync is needed first.
var errNoHierarchyKey = errors.New("engine: hierarchy key needs a FolderSync first")
