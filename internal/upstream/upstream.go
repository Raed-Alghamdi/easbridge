// Package upstream defines the boundary between the bridge core (store,
// engine, IMAP, SMTP) and the thing that actually holds the mail. The real
// implementation speaks Exchange ActiveSync (internal/easupstream); the tests
// and the imapflow harness use an in-memory one (internal/fakeupstream, built
// only with the devfake tag).
//
// The interface is deliberately key-explicit: the engine hands in the sync key
// it has committed and gets the new key back together with the changes. The
// implementation never persists anything. The engine commits the new key in the
// same database transaction as the items it belongs to, so after a crash the old
// key is replayed and nothing is lost (at-least-once, de-duplicated by ServerId).
package upstream

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Errors. Implementations wrap these (use errors.Is).
var (
	// ErrAuthFailed means Exchange rejected the credentials (HTTP 401). The
	// engine never retries it; see DESIGN section 3.
	ErrAuthFailed = errors.New("upstream: authentication failed")
	// ErrRemoteWipe means Exchange asked the device to wipe its data.
	ErrRemoteWipe = errors.New("upstream: remote wipe requested")
	// ErrUnavailable covers network errors, timeouts and 5xx answers: retry later.
	ErrUnavailable = errors.New("upstream: temporarily unavailable")
	// ErrNotFound means the item or folder no longer exists upstream.
	ErrNotFound = errors.New("upstream: item not found")
	// ErrHierarchyChanged means the folder hierarchy changed (Sync status 12,
	// Ping status 7): run FolderSync first.
	ErrHierarchyChanged = errors.New("upstream: folder hierarchy changed")
	// ErrTooLarge means a message exceeds what Exchange accepts.
	ErrTooLarge = errors.New("upstream: message too large")
	// ErrNoMIME means the server returned no MIME body for an item.
	ErrNoMIME = errors.New("upstream: item has no MIME body")
)

// ErrAccountOnlyWipe is the Exchange 16.1 "account only" remote wipe order
// (Provision/AccountOnlyRemoteWipe): remove the data of this account and nothing
// else on the device. It is also an ErrRemoteWipe, so code that honours every
// wipe order need not tell the two apart; the acknowledgement is another element
// (AcknowledgeWipe).
var ErrAccountOnlyWipe error = &accountOnlyWipeError{}

type accountOnlyWipeError struct{}

func (*accountOnlyWipeError) Error() string { return "upstream: account-only remote wipe requested" }

// Is makes ErrAccountOnlyWipe an ErrRemoteWipe.
func (*accountOnlyWipeError) Is(target error) bool { return target == ErrRemoteWipe }

// ErrNotSent means the failure happened before any request reached a server:
// the connection could not be made, DNS failed or the TLS handshake failed. The
// credentials were not presented to anyone, so the attempt cannot have counted
// as a failed logon in Active Directory. It is also an ErrUnavailable.
var ErrNotSent error = &notSentError{}

type notSentError struct{}

func (*notSentError) Error() string {
	return "upstream: could not connect (the request was not sent)"
}

// Is makes ErrNotSent an ErrUnavailable.
func (*notSentError) Is(target error) bool { return target == ErrUnavailable }

// ErrAuthLike means the answer looks like the refusal of a credential even though it is not a
// 401: an HTTP 403, a redirect that was not followed, or an HTML page (a logon page) in answer to an
// EAS request. An access-policy gateway in front of Exchange answers a wrong or expired password
// that way, and every such attempt can still count as a failed logon in Active Directory. During a
// session the engine counts them against the user's budget and stops after two in a row, as it does for
// a 401; at a login they are remembered like a rejection. It is also an ErrUnavailable.
var ErrAuthLike error = &authLikeError{}

type authLikeError struct{}

func (*authLikeError) Error() string {
	return "upstream: the answer looks like a refused credential (403, redirect or logon page)"
}

// Is makes ErrAuthLike an ErrUnavailable.
func (*authLikeError) Is(target error) bool { return target == ErrUnavailable }

// ErrTransient means a timeout or a dropped connection after the request was sent: the server did not
// answer. It cannot have been a refusal of the credential, so at a login it costs a short fixed pause
// and no budget. It is also an ErrUnavailable.
var ErrTransient error = &transientError{}

type transientError struct{}

func (*transientError) Error() string { return "upstream: no answer (time-out or dropped connection)" }

// Is makes ErrTransient an ErrUnavailable.
func (*transientError) Is(target error) bool { return target == ErrUnavailable }

// ThrottleError means Exchange, or a gateway in front of it, is shedding load or
// failing on its own side: HTTP 429, 503 or another 5xx, or a transient EAS
// status. The engine pauses all traffic of the mailbox when this repeats (and at
// once when the server asked for it). It is also an ErrUnavailable.
type ThrottleError struct {
	HTTP int // HTTP status, 0 when the answer was an EAS status
	EAS  int // EAS status code, 0 when the answer was an HTTP status
	// RetryAfter is the delay the server asked for with a Retry-After header
	// (0 if none).
	RetryAfter time.Duration
	// Explicit says the server asked the client to back off (429, 503, or EAS
	// status 111 "retry later"): a single occurrence is enough to pause.
	Explicit bool
}

func (e *ThrottleError) Error() string {
	what := fmt.Sprintf("HTTP %d", e.HTTP)
	if e.HTTP == 0 {
		what = fmt.Sprintf("EAS status %d", e.EAS)
	}
	if e.RetryAfter > 0 {
		return fmt.Sprintf("upstream: throttled (%s, retry after %s)", what, e.RetryAfter)
	}
	return fmt.Sprintf("upstream: throttled (%s)", what)
}

// Is makes a ThrottleError an ErrUnavailable.
func (e *ThrottleError) Is(target error) bool { return target == ErrUnavailable }

// StatusError is a protocol-level refusal with the EAS status code attached.
type StatusError struct {
	Op   string
	Code int
	Perm bool // permanent (do not retry unchanged)
	// Global: the status is about the device or the account (a blocked device, a
	// disabled account, a policy the server enforces), not about the item or the
	// folder the call was about. Retrying the item is pointless while it holds, but
	// the item is not to blame: it must not be given up on.
	Global bool
	Reason string
}

func (e *StatusError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("upstream: %s: status %d (%s)", e.Op, e.Code, e.Reason)
	}
	return fmt.Sprintf("upstream: %s: status %d", e.Op, e.Code)
}

// Temporary reports whether retrying later may help.
func (e *StatusError) Temporary() bool { return !e.Perm }

// Folder is one entry of the Exchange folder hierarchy.
type Folder struct {
	ServerID    string
	ParentID    string
	DisplayName string
	Type        int // EAS folder type
}

// FolderDelta is the outcome of a FolderSync call.
type FolderDelta struct {
	NewKey  string
	Added   []Folder
	Updated []Folder
	Deleted []string
	// Full is true when the delta lists the complete hierarchy (the call started
	// from key "0", explicitly or after the server rejected the key). Folders
	// not listed are gone.
	Full bool
}

// Item is one email as reported by Sync.
type Item struct {
	ServerID string
	// Read and Flagged are meaningful only when ReadSet / FlagSet are true: a
	// Sync Change carries only the properties that changed.
	Read, ReadSet    bool
	Flagged, FlagSet bool
	DateReceived     time.Time
	MIME             []byte
	MIMEComplete     bool // false: MIME is truncated or absent, fetch it
	EstimatedSize    int  // size of the complete message when known
	// Subject, From and To are the metadata Exchange lists with the item, as it
	// sent them ("" when absent, always absent in a Change). The engine keeps them
	// for the placeholder it shows if the body can never be fetched.
	Subject, From, To string
	// Bcc is the email2:Bcc property (MS-ASEMAIL 2.2.2.9, protocol 16.0 and 16.1)
	// as Exchange sent it, "" when the item carries none. Exchange's MIME rendering
	// of a message in Sent Items has no Bcc header; if the item lists its Bcc
	// recipients here, the engine writes them back into the copy it serves.
	Bcc string
}

// SyncDelta is the outcome of one Sync round trip for one folder.
type SyncDelta struct {
	NewKey        string
	MoreAvailable bool
	// Reset is true when the server rejected the key (InvalidSyncKey) and the
	// call restarted from "0". The delta then only carries NewKey (a bootstrap
	// key) and no items; everything the folder holds will be re-listed by the
	// following calls. Anything not re-listed was deleted during the gap.
	Reset   bool
	Added   []Item
	Changed []Item
	Deleted []string
}

// SyncOptions tune one Sync call.
type SyncOptions struct {
	// HeadersOnly asks for no message bodies: every returned item then has
	// MIMEComplete false. The engine uses it for verification resyncs, where
	// bodies of known messages are not needed.
	HeadersOnly bool
}

// Change is a client-side modification to send upstream.
type Change struct {
	ServerID string
	Read     *bool
	Flagged  *bool
	Delete   bool
}

// ChangeResult is the per-item outcome of ApplyChanges.
type ChangeResult struct {
	ServerID string
	Status   int
	OK       bool
	// Gone: the item no longer exists upstream (the change is moot).
	Gone bool
}

// ApplyResult is the outcome of ApplyChanges.
type ApplyResult struct {
	NewKey  string
	Results []ChangeResult
	// ServerChanges holds server-side changes that came back in the same
	// response; they must be applied exactly like a Sync delta.
	ServerChanges *SyncDelta
}

// MoveResult is the per-item outcome of MoveItems.
type MoveResult struct {
	SrcServerID string
	DstServerID string
	OK          bool
	Status      int
}

// FolderResult is the outcome of folder create/update/delete.
type FolderResult struct {
	ServerID string // new folder (create only)
	NewKey   string // new hierarchy key
}

// PingResult is the outcome of one Ping.
type PingResult struct {
	// Status is the EAS Ping status (1 no changes, 2 changes, others errors).
	Status  int
	Changed []string
	// Heartbeat is the server's preferred interval when Status is 5.
	Heartbeat time.Duration
}

// Address is a postal address of a contact.
type Address struct {
	Street, City, State, PostalCode, Country string
}

// Contact is one item of a Contacts folder, with the fields EASBridge serves
// (docs/DESIGN-contacts.md section 3.4). The photo, the government and customer ids,
// the instant messaging addresses and the manager's name are not carried.
type Contact struct {
	ServerID string

	FirstName, LastName, MiddleName string
	Title                           string // honorific ("Dr.")
	Suffix                          string
	FileAs                          string
	NickName                        string

	Company, Department, JobTitle, Office string

	Email1, Email2, Email3 string

	HomePhone, BusinessPhone, MobilePhone string

	HomeAddress, BusinessAddress Address

	Birthday, Anniversary time.Time
	WebPage               string
	Categories            []string
}

// ContactDelta is the outcome of one Sync round trip for one Contacts folder.
type ContactDelta struct {
	NewKey        string
	MoreAvailable bool
	// Reset is true when the server rejected the key (InvalidSyncKey) and the call
	// restarted from "0": the delta then only carries the new bootstrap key and no
	// items, and the caller must start a new download of the folder.
	Reset   bool
	Added   []Contact
	Changed []Contact
	Deleted []string
	// Fields names the fields (not the values) that at least one item of the
	// response carried, for `easbridge probe`.
	Fields []string
}

// DirectoryEntry is one entry of the Global Address List as far as EASBridge keeps
// it. HomePhone and Alias are not carried (data minimization); the adapter drops
// them.
type DirectoryEntry struct {
	DisplayName, FirstName, LastName string
	Title, Office, Company           string
	EmailAddress                     string
	Phone, MobilePhone               string
}

// DirectoryResult is the answer to one GAL search.
type DirectoryResult struct {
	// Total is the number of matches the server reports, Entries the ones returned
	// (at most about 100).
	Total   int
	Entries []DirectoryEntry
	// Fields names the fields (not the values) that at least one entry carried,
	// HomePhone and Alias included, for `easbridge probe`.
	Fields []string
}

// Mailbox is one authenticated view of one Exchange mailbox.
type Mailbox interface {
	// Open checks the credentials and prepares the session: protocol version
	// negotiation and Provision. It must be called once, first. It returns
	// ErrAuthFailed for bad credentials and ErrRemoteWipe (or ErrAccountOnlyWipe)
	// if a wipe is pending.
	Open(ctx context.Context) error
	// PolicyKey returns the current Provision policy key for the engine to
	// persist (it is a device token, not a secret).
	PolicyKey() string

	FolderSync(ctx context.Context, key string) (*FolderDelta, error)
	Sync(ctx context.Context, folderID, key string, o SyncOptions) (*SyncDelta, error)
	// FetchMIME downloads the complete message of one item.
	FetchMIME(ctx context.Context, folderID, serverID string) ([]byte, error)
	// ApplyChanges sends flag changes and deletes for one folder. key must not be
	// "0". hardDelete makes deletes permanent (only inside Deleted Items).
	ApplyChanges(ctx context.Context, folderID, key string, changes []Change, hardDelete bool) (*ApplyResult, error)
	MoveItems(ctx context.Context, srcFolder, dstFolder string, serverIDs []string) ([]MoveResult, error)

	CreateFolder(ctx context.Context, key, parentID, name string) (*FolderResult, error)
	RenameFolder(ctx context.Context, key, serverID, newParentID, newName string) (*FolderResult, error)
	DeleteFolder(ctx context.Context, key, serverID string) (*FolderResult, error)

	// SyncContacts is one Sync round trip for a Contacts folder (types 9 and 14),
	// from key (the committed key, "0" to start a download). Like Sync, it hands the
	// new key back and persists nothing. A status 3 restarts the chain: Reset.
	SyncContacts(ctx context.Context, folderID, key string) (*ContactDelta, error)
	// SearchDirectory is one Global Address List search (store GAL) for query. The
	// server answers at most about 100 entries; EASBridge never pages.
	SearchDirectory(ctx context.Context, query string) (*DirectoryResult, error)

	// SendMail submits a complete RFC 5322 message (recipients come from its
	// headers, so Bcc recipients must already be in a Bcc header).
	SendMail(ctx context.Context, mime []byte, saveInSent bool) error

	// Ping blocks until a watched folder has changes, the heartbeat expires or
	// ctx ends. See PingResult for the status codes.
	Ping(ctx context.Context, heartbeat time.Duration, folderIDs []string) (*PingResult, error)

	// AcknowledgeWipe confirms a remote wipe to Exchange. accountOnly says the
	// order was an ErrAccountOnlyWipe, which has its own acknowledgement.
	AcknowledgeWipe(ctx context.Context, accountOnly bool) error
	// Close releases resources (idle connections).
	Close()
}

// Config is what a Factory needs to build a Mailbox.
type Config struct {
	User     string
	Password string
	DeviceID string
	// PolicyKey is the persisted Provision policy key ("" if none).
	PolicyKey string
}

// Factory builds Mailbox values. The password in Config lives in memory only.
type Factory interface {
	New(cfg Config) (Mailbox, error)
}

// FactoryFunc adapts a function to Factory.
type FactoryFunc func(cfg Config) (Mailbox, error)

// New implements Factory.
func (f FactoryFunc) New(cfg Config) (Mailbox, error) { return f(cfg) }

// IsTemporary reports whether err suggests trying again later.
func IsTemporary(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrUnavailable) {
		return true
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Temporary()
	}
	return false
}

// DirectoryDenied reports whether err says the directory may not be searched at all:
// the Search status 5 (access denied), 13 (access blocked) or 14 (credentials required),
// or the common status 130 (access denied to the request). The engine then leaves the
// directory alone for a day.
func DirectoryDenied(err error) bool {
	var se *StatusError
	if !errors.As(err, &se) || se.Op != "Search" {
		return false
	}
	switch se.Code {
	case 5, 13, 14, 130:
		return true
	}
	return false
}
