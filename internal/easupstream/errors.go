package easupstream

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hstern/go-activesync/eas"

	"easbridge/internal/logx"
	"easbridge/internal/upstream"
)

// mapError translates go-activesync and transport errors into the upstream
// package's vocabulary. Credentials never appear in these errors; the User query
// value is stripped from URLs by the logger, and HTTPError.Error prints the URL,
// so errors from this package are logged through logx (which scrubs them).
func mapError(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	if errors.Is(err, upstream.ErrAuthFailed) || errors.Is(err, upstream.ErrRemoteWipe) ||
		errors.Is(err, upstream.ErrHierarchyChanged) || errors.Is(err, upstream.ErrNoMIME) {
		return err
	}
	switch {
	case errors.Is(err, eas.ErrAccountOnlyRemoteWipe): // before ErrRemoteWipe, which it also matches
		return fmt.Errorf("%s: %w", op, upstream.ErrAccountOnlyWipe)
	case errors.Is(err, eas.ErrRemoteWipe):
		return fmt.Errorf("%s: %w", op, upstream.ErrRemoteWipe)
	case eas.IsHTTPStatus(err, http.StatusUnauthorized):
		return fmt.Errorf("%s: HTTP 401: %w", op, upstream.ErrAuthFailed)
	case eas.IsHTTPStatus(err, http.StatusRequestEntityTooLarge):
		return fmt.Errorf("%s: HTTP 413: %w", op, upstream.ErrTooLarge)
	}
	if errors.Is(err, errHTMLLogon) {
		return fmt.Errorf("%s: an HTML page answered an EAS request (a logon page?): %w", op, upstream.ErrAuthLike)
	}
	if neverSent(err) {
		return fmt.Errorf("%s: %s: %w", op, logx.Scrub(err.Error()), upstream.ErrNotSent)
	}
	var he *eas.HTTPError
	if errors.As(err, &he) {
		switch {
		case he.StatusCode == http.StatusTooManyRequests || he.StatusCode == http.StatusServiceUnavailable:
			// The server (or its gateway) asks to slow down; Retry-After says how long.
			return fmt.Errorf("%s: %w", op, &upstream.ThrottleError{HTTP: he.StatusCode, RetryAfter: parseRetryAfter(he.RetryAfter), Explicit: true})
		case he.StatusCode >= 500:
			return fmt.Errorf("%s: %w", op, &upstream.ThrottleError{HTTP: he.StatusCode, RetryAfter: parseRetryAfter(he.RetryAfter)})
		}
		if he.StatusCode == http.StatusForbidden || (he.StatusCode >= 300 && he.StatusCode < 400) {
			// A gateway's way of refusing a credential (403, or a redirect to a logon page that was not
			// followed): see upstream.ErrAuthLike.
			return fmt.Errorf("%s: HTTP %d: %w", op, he.StatusCode, upstream.ErrAuthLike)
		}
		// 408, and also 404/449 after the library's own retry: nothing a retry with the same
		// credentials cannot try again later.
		return fmt.Errorf("%s: HTTP %d: %w", op, he.StatusCode, upstream.ErrUnavailable)
	}
	var se *eas.StatusError
	if errors.As(err, &se) {
		if se.Command == "Sync" && se.Code == 12 {
			return fmt.Errorf("%s: status 12: %w", op, upstream.ErrHierarchyChanged)
		}
		if f, ok := lookupStatus(se.Command, se.Code); ok {
			// A status the specification describes: one clear line, and the right
			// treatment (statuses.go), not the library's name for the number.
			return &upstream.StatusError{Op: op, Code: se.Code, Perm: f.perm, Global: f.global, Reason: f.msg}
		}
		// Not described in statuses.go. The common status codes (101 and up) say
		// something about the whole request, never about one item (those are the small
		// numbers of each command), so they are global whatever they are.
		return &upstream.StatusError{Op: op, Code: se.Code, Perm: permanentStatus(se.Code), Global: se.Code >= 100, Reason: logx.Scrub(se.Error())}
	}
	var ne net.Error
	var ue *url.Error
	// A time-out, or a connection dropped on the way (the HTTP client reports io.EOF inside its url.Error; a
	// WBXML decoder's own "unexpected EOF" is a damaged body, not a dropped connection).
	dropped := errors.As(err, &ue) && (errors.Is(ue.Err, io.EOF) || errors.Is(ue.Err, io.ErrUnexpectedEOF))
	if errors.Is(err, context.DeadlineExceeded) || dropped || (errors.As(err, &ne) && ne.Timeout()) {
		return fmt.Errorf("%s: %s: %w", op, logx.Scrub(err.Error()), upstream.ErrTransient)
	}
	if errors.As(err, &ne) || errors.As(err, &ue) {
		return fmt.Errorf("%s: %s: %w", op, logx.Scrub(err.Error()), upstream.ErrUnavailable)
	}
	return fmt.Errorf("%s: %s", op, logx.Scrub(err.Error()))
}

// parseRetryAfter reads a Retry-After header: a number of seconds or an HTTP
// date. Anything else, or a delay in the past, gives 0; a delay is capped at a day.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	var d time.Duration
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n < 0 {
			return 0
		}
		if n > 24*3600 {
			n = 24 * 3600
		}
		d = time.Duration(n) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = time.Until(t)
	}
	if d < 0 {
		return 0
	}
	if d > 24*time.Hour {
		d = 24 * time.Hour
	}
	return d
}

// neverSent reports whether err happened before any request reached a server: a
// failed connect, a DNS failure or a failed TLS handshake. The password was not
// presented to anyone.
func neverSent(err error) bool {
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Op == "dial" {
		return true
	}
	var de *net.DNSError
	if errors.As(err, &de) {
		return true
	}
	var rh tls.RecordHeaderError
	var uae x509.UnknownAuthorityError
	var he x509.HostnameError
	var cie x509.CertificateInvalidError
	var cve *tls.CertificateVerificationError
	return errors.As(err, &rh) || errors.As(err, &uae) || errors.As(err, &he) || errors.As(err, &cie) || errors.As(err, &cve)
}
