package engine

import (
	"errors"
	"fmt"
	"time"
)

// Login errors (see Manager.Login).
var (
	// ErrBadCredentials: the user is not on the allow-list, the password was
	// rejected by Exchange, or an identical wrong password was rejected
	// recently and is refused locally.
	ErrBadCredentials = errors.New("authentication failed")
	// ErrWiped: Exchange ordered a remote wipe; logins are refused until the
	// operator deletes the WIPED marker.
	ErrWiped = errors.New("mailbox was wiped on Exchange's request")
	// ErrUnavailable: Exchange (or the bridge) cannot serve the request now.
	ErrUnavailable = errors.New("Exchange is temporarily unavailable")
	// ErrAuthFailed: Exchange rejected the stored credentials during a session.
	ErrAuthFailed = errors.New("Exchange rejected the credentials")
)

// ThrottledError is the login refusal that protects the Active Directory
// account: after repeated unsuccessful verifications (or because this very
// credential just failed) the bridge does not ask Exchange again until Until.
// It answers like an unavailable service (NO [UNAVAILABLE], SMTP 454), never
// like a wrong password: the password may well be right.
type ThrottledError struct {
	Until  time.Time
	Reason string
}

func (e *ThrottledError) Error() string {
	return "logins are refused locally until " + e.Until.UTC().Format(time.RFC3339) + ": " + e.Reason
}

// Is makes a ThrottledError an ErrUnavailable.
func (e *ThrottledError) Is(target error) bool { return target == ErrUnavailable }

// Mailbox operation errors the IMAP and SMTP layers map to protocol replies.
var (
	// ErrCannot: the operation is not supported by the bridge (IMAP NO [CANNOT]).
	ErrCannot = errors.New("operation not supported")
	// ErrNoMailbox: no such folder (NO [NONEXISTENT]).
	ErrNoMailbox = errors.New("no such mailbox")
	// ErrExists: the folder already exists (NO [ALREADYEXISTS]).
	ErrExists = errors.New("mailbox already exists")
	// ErrTooLarge: the message exceeds the size limit (NO [TOOBIG]).
	ErrTooLarge = errors.New("message too large")
)

// CannotError carries the reason an operation is not supported.
type CannotError struct{ Reason string }

func (e *CannotError) Error() string        { return e.Reason }
func (e *CannotError) Is(target error) bool { return target == ErrCannot }

func cannot(format string, a ...any) error { return &CannotError{Reason: fmt.Sprintf(format, a...)} }
