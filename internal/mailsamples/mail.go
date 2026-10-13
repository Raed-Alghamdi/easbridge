package mailsamples

import "strings"

// Mail returns a text/plain message from sender@example.test to alice@example.test.
// subject and name are written into the Subject header and the display name of From
// exactly as given: a Word's B or Q form, or its Raw bytes for a header in a legacy
// 8-bit charset. The body is body's text in its charset with the transfer encoding
// cte: "8bit" (the raw bytes), "base64" or "quoted-printable".
func Mail(id, subject, name string, body BodyText, cte string) []byte {
	var enc string
	switch cte {
	case "8bit":
		enc = body.Raw
	case "base64":
		enc = body.Base64
	case "quoted-printable":
		enc = body.QP
	default:
		panic("mailsamples: unknown transfer encoding " + cte)
	}
	return []byte(strings.Join([]string{
		"Message-ID: <" + id + ">",
		"From: " + name + " <sender@example.test>",
		"To: alice@example.test",
		"Subject: " + subject,
		"Date: Thu, 01 Oct 2026 10:00:00 +0000",
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=" + body.Charset,
		"Content-Transfer-Encoding: " + cte,
		"", enc,
	}, "\r\n"))
}
