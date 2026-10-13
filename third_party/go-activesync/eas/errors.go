// Copyright (C) 2026 Henry Stern
// SPDX-License-Identifier: MIT

package eas

import (
	"errors"
	"fmt"
)

// HTTPError is returned when an EAS request receives a non-2xx HTTP
// response from the server. The body is captured (truncated to 4 KiB) so
// callers can include it in diagnostics without re-issuing the request.
type HTTPError struct {
	StatusCode int
	Status     string
	URL        string
	Body       []byte // truncated to 4 KiB
	// RetryAfter is the raw Retry-After header of the response ("" if none):
	// a number of seconds or an HTTP date. easbridge patch 9: a throttled
	// server says how long to wait there, and callers had no way to see it.
	RetryAfter string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("eas: %s %s: HTTP %s (body=%dB)", "POST", e.URL, e.Status, len(e.Body))
}

// IsHTTPStatus reports whether err is an HTTPError with the given code.
// Useful for callers that want to distinguish 401 / 403 / 449 etc. without
// type-asserting at every site.
func IsHTTPStatus(err error, code int) bool {
	for ; err != nil; err = unwrap(err) {
		if h, ok := err.(*HTTPError); ok {
			return h.StatusCode == code
		}
	}
	return false
}

// unwrap is a tiny shim around the stdlib errors.Unwrap so we can avoid
// importing errors here just for one function.
func unwrap(err error) error {
	type wrapped interface{ Unwrap() error }
	if w, ok := err.(wrapped); ok {
		return w.Unwrap()
	}
	return nil
}

// StatusError is returned when an EAS command parses successfully but the
// embedded Status element reports a non-success code. The mapping of codes
// to human-readable names lives in status.go.
type StatusError struct {
	Command string // EAS command name ("FolderSync", "Provision", ...)
	Code    int
}

func (e *StatusError) Error() string {
	if name := statusName(e.Code); name != "" {
		return fmt.Sprintf("eas: %s: status %d (%s)", e.Command, e.Code, name)
	}
	return fmt.Sprintf("eas: %s: status %d", e.Command, e.Code)
}

// IsStatusCode reports whether err is a StatusError with the given EAS
// status code.
func IsStatusCode(err error, code int) bool {
	for ; err != nil; err = unwrap(err) {
		if s, ok := err.(*StatusError); ok {
			return s.Code == code
		}
	}
	return false
}

// ErrRemoteWipe is returned (wrapped) by Provision, and by any command that
// re-provisions after an HTTP 449, when the server's Provision response
// carries a <RemoteWipe> instruction. Test with errors.Is.
//
// easbridge patch 2: upstream go-activesync discards the RemoteWipe element,
// so a caller has no way to honour a wipe request.
var ErrRemoteWipe = errors.New("eas: server requested remote wipe")

// ErrAccountOnlyRemoteWipe is the EAS 16.1 variant: the Provision response
// carries <AccountOnlyRemoteWipe> (token 0x3B), asking the client to remove
// this account's data but nothing else on the device. errors.Is(err,
// ErrRemoteWipe) is true for it as well, so callers that honour every wipe need
// not know about the difference; the acknowledgement is another element
// (AcknowledgeAccountOnlyRemoteWipe).
//
// easbridge patch 8: the element was decoded as "Hash" and never noticed.
var ErrAccountOnlyRemoteWipe error = accountOnlyWipeError{}

type accountOnlyWipeError struct{}

func (accountOnlyWipeError) Error() string { return "eas: server requested account-only remote wipe" }

// Is makes the account-only wipe an ErrRemoteWipe.
func (accountOnlyWipeError) Is(target error) bool { return target == ErrRemoteWipe }
