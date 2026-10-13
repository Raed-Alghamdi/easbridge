package imapd

import (
	"errors"
	"log/slog"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"easbridge/internal/engine"
)

func no(code imap.ResponseCode, text string) *imap.Error {
	return &imap.Error{Type: imap.StatusResponseTypeNo, Code: code, Text: text}
}

func bad(text string) *imap.Error {
	return &imap.Error{Type: imap.StatusResponseTypeBad, Text: text}
}

// mapError turns engine errors into IMAP responses. Unknown errors become
// "internal server error" (the library logs them).
func mapError(log *slog.Logger, err error) error {
	var ie *imap.Error
	var ce *engine.CannotError
	var te *engine.ThrottledError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ie):
		return err
	case errors.As(err, &ce):
		return no(imap.ResponseCodeCannot, ce.Reason)
	case errors.Is(err, engine.ErrNoMailbox):
		return no(imap.ResponseCodeNonExistent, "Mailbox does not exist")
	case errors.Is(err, engine.ErrExists):
		return no(imap.ResponseCodeAlreadyExists, "Mailbox already exists")
	case errors.Is(err, engine.ErrCannot):
		return no(imap.ResponseCodeCannot, err.Error())
	case errors.Is(err, engine.ErrTooLarge):
		return no(imap.ResponseCodeTooBig, "Message too large")
	case errors.Is(err, engine.ErrBadCredentials):
		return imapserver.ErrAuthFailed
	case errors.Is(err, engine.ErrWiped):
		return no(imap.ResponseCodeUnavailable, "The mailbox was wiped on Exchange's request; an administrator must re-enable access")
	case errors.Is(err, engine.ErrAuthFailed):
		return no(imap.ResponseCodeUnavailable, "Exchange rejected the stored credentials; log in again with the current password")
	case errors.As(err, &te):
		return no(imap.ResponseCodeUnavailable, "Logins are paused after repeated failures; try again later")
	case errors.Is(err, engine.ErrUnavailable):
		return no(imap.ResponseCodeUnavailable, "Exchange is temporarily unavailable")
	}
	log.Error("IMAP command failed", "err", err)
	return err
}
