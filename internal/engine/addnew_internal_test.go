package engine

import (
	"path/filepath"
	"testing"
	"time"

	"easbridge/internal/store"
	"easbridge/internal/upstream"
)

// A message born while a full resync of its folder is collected carries the epoch
// as its recon_mark, whichever way it is stored (a Sync batch or the server
// changes that come back with ApplyChanges go through addNew).
func TestNewMessagesBornDuringAResyncCarryTheEpoch(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	acct, err := db.Account("alice@example.test")
	if err != nil {
		t.Fatal(err)
	}
	var folderID int64
	if _, err := acct.Update(nil, func(tx *store.Tx) error {
		_, err := tx.ApplyFolders([]store.FolderInput{{ServerID: "1", ParentServerID: "0", DisplayName: "Inbox", EASType: store.TypeInbox}}, nil, nil, true)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	fs, _ := acct.Folders()
	folderID = fs[0].ID
	// A folder that has finished its first sync: its messages get UIDs at once.
	if _, err := acct.Update(nil, func(tx *store.Tx) error {
		if err := tx.SetInitialChainDone(folderID, true); err != nil {
			return err
		}
		_, err := tx.CompleteInitialSync(folderID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	const epoch = 7
	a := &Account{}
	mime := []byte("Message-ID: <born@x>\r\nSubject: s\r\nDate: Fri, 09 Oct 2026 12:00:00 +0000\r\n\r\nbody\r\n")
	item := func(id string) upstream.Item {
		return upstream.Item{ServerID: id, MIME: mime, MIMEComplete: true, DateReceived: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	}
	if _, err := acct.Update(nil, func(tx *store.Tx) error {
		f, err := tx.FolderByID(folderID)
		if err != nil {
			return err
		}
		if _, err := a.addNew(tx, f, item("1:1"), reconMarkOf(true, epoch)); err != nil {
			return err
		}
		_, err = a.addNew(tx, f, item("1:2"), reconMarkOf(false, epoch))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	m1, err := acct.MessageByServerID(folderID, "1:1")
	if err != nil || m1.ReconMark != epoch {
		t.Fatalf("born during the resync: %+v, %v (want recon_mark %d)", m1, err, epoch)
	}
	m2, err := acct.MessageByServerID(folderID, "1:2")
	if err != nil || m2.ReconMark != 0 {
		t.Fatalf("born outside a resync: %+v, %v (want recon_mark 0)", m2, err)
	}
	// At the end of the epoch only the message that was not marked is a suspect.
	var absent []store.Message
	if _, err := acct.Update(nil, func(tx *store.Tx) error {
		var err error
		absent, err = tx.ReconAbsent(folderID, epoch)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(absent) != 1 || absent[0].ServerID != "1:2" {
		t.Fatalf("suspects at the end of the epoch: %+v", absent)
	}
}
