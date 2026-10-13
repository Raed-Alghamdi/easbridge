package upstream

import (
	"crypto/sha256"
	"encoding/hex"

	"easbridge/internal/mimeutil"
)

// SubmissionID is the idempotency key of a message submission. EAS SendMail
// carries it as ClientId, which "MUST be unique for each message, as the server
// will use the ClientId element value to identify duplicate messages"
// (MS-ASCMD 2.2.3.28); Exchange then answers a repeat with status 118,
// MessagePreviouslySent, instead of sending again.
//
// A random ClientId makes every attempt a new message: when the client retries a
// submission whose outcome it never learned (a timeout, a dropped connection, a
// 4xx the message survived) the recipients get it twice. So the key is derived
// from the message itself: from its Message-ID header (a retry of one message
// keeps it even if the client rebuilds the rest) or, if there is none, from the
// whole message.
//
// It is 32 hex digits (the limit is 40 characters) of a SHA-256, so it reveals
// nothing about the message.
func SubmissionID(mime []byte) string {
	h := sha256.New()
	h.Write([]byte("easbridge-sendmail-v1\x00"))
	if id := mimeutil.ParseMeta(mime).MessageID; id != "" {
		h.Write([]byte("message-id\x00"))
		h.Write([]byte(id))
	} else {
		h.Write([]byte("message\x00"))
		h.Write(mime)
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}
