package store

import (
	"strings"
	"time"
)

// Flags is the bit mask of system flags kept per message.
type Flags uint8

// System flags. Everything else is a keyword (stored as text, local only).
const (
	FlagSeen Flags = 1 << iota
	FlagAnswered
	FlagFlagged
	FlagDeleted
	FlagDraft
)

// Has reports whether all bits of f2 are set.
func (f Flags) Has(f2 Flags) bool { return f&f2 == f2 }

// Kind says where a message comes from.
type Kind uint8

const (
	// KindServer is an Exchange item (has a ServerId).
	KindServer Kind = 0
	// KindLocalDraft is a draft APPENDed by the IMAP client; it lives only in
	// the bridge (DESIGN section 9).
	KindLocalDraft Kind = 1
	// KindPlaceholder is a Sent copy APPENDed by the client that is waiting for
	// Exchange's own copy to bind to it (24 h).
	KindPlaceholder Kind = 2
	// KindLocalOnly is a placeholder whose time ran out; it stays local.
	KindLocalOnly Kind = 3
)

// Local reports whether the message has no Exchange counterpart (yet).
func (k Kind) Local() bool { return k != KindServer }

// Folder is one mail folder.
type Folder struct {
	ID             int64
	ServerID       string
	ParentServerID string
	DisplayName    string
	EASType        int
	Path           string // IMAP mailbox name
	SpecialUse     string // `\Sent` etc., or ""
	UIDValidity    uint32
	UIDNext        uint32
	SyncKey        string
	ReconActive    bool
	ReconEpoch     int64
	ReconVerifyAt  int64
	// InitialComplete is false while the folder's first sync is running: its
	// server messages are staged (invisible to IMAP) until the folder opens.
	InitialComplete bool
	// InitialChainDone is set when the last data Sync of an unopened folder
	// reported nothing more to fetch; the folder then opens as soon as no
	// message is left waiting for its MIME.
	InitialChainDone bool
}

// IsInbox reports whether this is the Inbox.
func (f *Folder) IsInbox() bool { return f.EASType == 2 }

// MsgMeta is the lightweight per-message record the IMAP layer keeps in memory.
type MsgMeta struct {
	UID          uint32
	Flags        Flags
	Keywords     string
	InternalDate int64
	Size         int64
	Kind         Kind
}

// Message is a stored message without its MIME body.
type Message struct {
	PK       int64
	FolderID int64
	// UID is the IMAP UID, or 0 for a staged message (one that belongs to a
	// folder whose first sync has not finished and is not visible yet).
	UID                uint32
	Staged             bool
	ServerID           string // "" for local messages
	Kind               Kind
	Flags              Flags
	Keywords           string
	InternalDate       time.Time
	Size               int64
	MessageID          string
	InReplyTo          string
	References         string
	Subject            string
	DateHdr            time.Time
	To                 string
	PlaceholderExpires time.Time
	ReconMark          int64
	ReconStrikes       int
	Header             []byte
}

// Meta returns the in-memory listing record for m.
func (m *Message) Meta() MsgMeta {
	return MsgMeta{UID: m.UID, Flags: m.Flags, Keywords: m.Keywords, InternalDate: m.InternalDate.Unix(), Size: m.Size, Kind: m.Kind}
}

// NewMessage is the input to Tx.AddMessage.
type NewMessage struct {
	ServerID           string // "" for local messages
	Kind               Kind
	Flags              Flags
	Keywords           string
	InternalDate       time.Time // zero means now
	MIME               []byte
	PlaceholderExpires time.Time
	// ReconMark is the reconciliation epoch the message is born in when a full
	// resync of its folder is being collected (0 otherwise): a message that
	// arrives during the epoch has been seen by definition and must not be
	// suspected of having been deleted when the epoch ends.
	ReconMark int64
}

// Counts are the numbers STATUS needs.
type Counts struct {
	Messages    int
	Unseen      int
	UIDNext     uint32
	UIDValidity uint32
}

// EventKind classifies a change to a folder's message list.
type EventKind uint8

const (
	// EvAdd: a message with this UID appeared (always the highest UID so far).
	EvAdd EventKind = iota + 1
	// EvExpunge: the message with this UID is gone.
	EvExpunge
	// EvFlags: the flags of this UID changed.
	EvFlags
	// EvBulkAdd: a folder finished its first sync and every staged message
	// became visible at once (Event.Bulk, ascending UIDs, all above any UID
	// listeners knew). Listeners announce it as a single EXISTS.
	EvBulkAdd
	// EvResize: the stored bytes of this UID were replaced by another rendering of
	// the same message (Event.Size is the new size), see Tx.ReplaceMIME. Nothing is
	// announced to IMAP clients, because the message is not new and its flags did
	// not change; listeners only correct the size they keep for RFC822.SIZE.
	EvResize
)

// Event describes one change made by a transaction. Events are returned by
// Account.Update after commit, in the order the changes were made.
type Event struct {
	Kind     EventKind
	FolderID int64
	UID      uint32
	Flags    Flags  // new flags (EvAdd, EvFlags)
	Keywords string // new keywords (EvAdd, EvFlags)
	Origin   any    // whoever caused the change (opaque to the store)
	// For EvAdd: the message's INTERNALDATE (unix seconds), size and kind, so
	// listeners can keep a complete in-memory listing without a database read.
	Date    int64
	Size    int64
	MsgKind Kind
	// Bulk lists the newly visible messages of an EvBulkAdd.
	Bulk []MsgMeta
}

// Outbox operations.
const (
	OpRead    = 1
	OpFlagged = 2
	OpDelete  = 3
)

// OutboxOp is one queued upstream change.
type OutboxOp struct {
	ID          int64
	FolderID    int64
	ServerID    string
	Op          int
	Value       bool
	Orig        int // value before the first queued change: 0, 1 or -1 (unknown)
	Hard        bool
	Version     int
	Attempts    int
	NextAttempt int64 // unix milliseconds
	// RestoreFlags and RestoreDate are what a delete needs to bring the message
	// back if Exchange refuses it (RestoreDate is zero when not recorded).
	RestoreFlags Flags
	RestoreDate  time.Time
}

// PendingFetch is an item waiting for its full MIME.
type PendingFetch struct {
	ID           int64
	FolderID     int64
	ServerID     string
	Flags        Flags
	InternalDate time.Time
	EstSize      int64
	Attempts     int
	// ItemFailures counts the failed attempts that were about this item (the
	// server answered with a status for it, or sent no body), as opposed to
	// transport trouble that says nothing about the item.
	ItemFailures int
	NextAttempt  int64
	// Subject, From and To are the metadata the Sync response carried (raw, as
	// Exchange sent them), kept for the placeholder shown if the body never comes.
	Subject, From, To string
	// Bcc is the email2:Bcc property the Sync response carried ("" when it did
	// not), kept until the body is there: a message of Sent Items that Exchange
	// lists with its Bcc recipients is served with a Bcc header (see the engine).
	Bcc string
}

// FolderProgress is the first-sync state of one folder, for /healthz.
type FolderProgress struct {
	FolderID     int64
	Complete     bool
	Staged       int // messages fetched but still hidden
	PendingFetch int // messages announced but not yet downloaded
}

// SplitKeywords turns the stored keyword text into a slice.
func SplitKeywords(s string) []string { return strings.Fields(s) }
