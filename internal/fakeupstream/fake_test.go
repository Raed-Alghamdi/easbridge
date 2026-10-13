//go:build devfake

package fakeupstream

import (
	"bytes"
	"testing"
)

// Exchange strips the Bcc header from every copy it delivers and from the Sent
// Items copy it saves; the fake does the same, and the tests of the Sent copy rely
// on it.
func TestStripBccRemovesTheWholeFieldAndNothingElse(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"in the middle", "To: a@x.test\r\nBcc: b@x.test\r\nSubject: s\r\n\r\nbody\r\n", "To: a@x.test\r\nSubject: s\r\n\r\nbody\r\n"},
		{"first", "Bcc: b@x.test\r\nTo: a@x.test\r\n\r\nbody", "To: a@x.test\r\n\r\nbody"},
		{"last", "To: a@x.test\r\nBcc: b@x.test\r\n\r\nbody", "To: a@x.test\r\n\r\nbody"},
		{"folded over several lines", "To: a@x.test\r\nBcc: b@x.test,\r\n c@x.test,\r\n\td@x.test\r\nSubject: s\r\n\r\nbody", "To: a@x.test\r\nSubject: s\r\n\r\nbody"},
		{"any case", "To: a@x.test\r\nBCC: b@x.test\r\nbcc: c@x.test\r\n\r\nbody", "To: a@x.test\r\n\r\nbody"},
		{"a field that only ends in bcc", "To: a@x.test\r\nX-Bcc: keep@x.test\r\nSubject: s\r\n\r\nbody", "To: a@x.test\r\nX-Bcc: keep@x.test\r\nSubject: s\r\n\r\nbody"},
		{"a continuation of another field that mentions Bcc", "To: a@x.test,\r\n Bcc: keep@x.test\r\n\r\nbody", "To: a@x.test,\r\n Bcc: keep@x.test\r\n\r\nbody"},
		{"the body is never touched", "To: a@x.test\r\n\r\nBcc: in-the-body@x.test\r\n continued\r\n", "To: a@x.test\r\n\r\nBcc: in-the-body@x.test\r\n continued\r\n"},
		{"LF line endings", "To: a@x.test\nBcc: b@x.test\nSubject: s\n\nbody\n", "To: a@x.test\nSubject: s\n\nbody\n"},
		{"no Bcc at all", "To: a@x.test\r\nSubject: s\r\n\r\nbody", "To: a@x.test\r\nSubject: s\r\n\r\nbody"},
	} {
		if got := stripBcc([]byte(tc.in)); !bytes.Equal(got, []byte(tc.want)) {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}
