package smtpd

import (
	"errors"
	"net"

	"github.com/emersion/go-smtp"

	"easbridge/internal/engine"
	"easbridge/internal/upstream"
)

func smtpErr(code int, enh smtp.EnhancedCode, msg string) *smtp.SMTPError {
	return &smtp.SMTPError{Code: code, EnhancedCode: enh, Message: msg}
}

// Temporary and permanent replies the bridge produces on its own.
var (
	errTempUnavailable = smtpErr(451, smtp.EnhancedCode{4, 4, 1}, "Exchange is temporarily unavailable, try again later")
	errAuthTemp        = smtpErr(454, smtp.EnhancedCode{4, 7, 0}, "Temporary authentication failure")
	errTooLarge        = smtpErr(552, smtp.EnhancedCode{5, 3, 4}, "Message too large")
	errNoRecipients    = smtpErr(554, smtp.EnhancedCode{5, 5, 3}, "No valid recipients")
	errBadAuth         = smtp.ErrAuthFailed
)

// MapSendError converts what a failed send returned (engine or upstream error)
// into the SMTP reply MailFlow should see. Permanent problems get 5xx so the
// client stops retrying; anything that may pass gets 4xx.
//
// The numeric Status values are the MS-ASCMD common status codes Exchange
// returns for SendMail.
func MapSendError(err error) error {
	if err == nil {
		return nil
	}
	var se *smtp.SMTPError
	if errors.As(err, &se) {
		return err
	}
	switch {
	case errors.Is(err, engine.ErrTooLarge), errors.Is(err, upstream.ErrTooLarge):
		return errTooLarge
	case errors.Is(err, engine.ErrAuthFailed), errors.Is(err, upstream.ErrAuthFailed):
		return smtpErr(454, smtp.EnhancedCode{4, 7, 0}, "Exchange rejected the stored credentials; authenticate again with the current password")
	case errors.Is(err, engine.ErrWiped), errors.Is(err, upstream.ErrRemoteWipe):
		return smtpErr(451, smtp.EnhancedCode{4, 3, 2}, "The mailbox was wiped on Exchange's request")
	case errors.Is(err, engine.ErrUnavailable), errors.Is(err, upstream.ErrUnavailable):
		return errTempUnavailable
	}
	var st *upstream.StatusError
	if errors.As(err, &st) {
		return mapStatus(st)
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return errTempUnavailable
	}
	return smtpErr(451, smtp.EnhancedCode{4, 3, 0}, "Message could not be sent, try again later")
}

// mapStatus maps an EAS status of SendMail. The values are those of the common
// status table of MS-ASCMD 2.2.2. (118, MessagePreviouslySent, never gets here:
// the EAS adapter makes it success, the message having been sent before.)
func mapStatus(st *upstream.StatusError) error {
	switch st.Code {
	case 101, 102, 103, 104, 105, 106, 107: // malformed request or MIME
		return smtpErr(554, smtp.EnhancedCode{5, 6, 0}, "Exchange rejected the message content")
	case 110, 111: // server error, retry later
		return smtpErr(451, smtp.EnhancedCode{4, 3, 0}, "Exchange reported a server error, try again later")
	case 113: // MailboxQuotaExceeded
		return smtpErr(552, smtp.EnhancedCode{5, 2, 2}, "Mailbox quota exceeded")
	case 114: // MailboxServerOffline
		return errTempUnavailable
	case 115: // SendQuotaExceeded
		return smtpErr(452, smtp.EnhancedCode{4, 5, 3}, "Sending quota exceeded, try again later")
	case 116: // MessageRecipientUnresolved
		return smtpErr(550, smtp.EnhancedCode{5, 1, 1}, "Exchange could not resolve a recipient")
	case 119: // MessageHasNoRecipient
		return errNoRecipients
	case 120, 121: // MailSubmissionFailed, MessageReplyFailed
		return smtpErr(554, smtp.EnhancedCode{5, 3, 0}, "Exchange could not submit the message")
	case 122: // AttachmentIsTooLarge
		return errTooLarge
	case 123, 124, 125, 126, 127, 128, 129, 130, 131: // no mailbox, anonymous, disabled, blocked, access denied
		return smtpErr(550, smtp.EnhancedCode{5, 7, 1}, "Exchange refused to send for this account")
	case 140, 141, 142, 143: // provisioning / policy: the engine re-provisions
		return smtpErr(451, smtp.EnhancedCode{4, 7, 0}, "Exchange asked for re-provisioning, try again")
	}
	if st.Temporary() {
		return smtpErr(451, smtp.EnhancedCode{4, 3, 0}, "Message could not be sent, try again later")
	}
	return smtpErr(554, smtp.EnhancedCode{5, 3, 0}, "Exchange refused the message")
}
