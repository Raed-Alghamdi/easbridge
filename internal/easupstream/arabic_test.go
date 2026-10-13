package easupstream_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hstern/go-activesync/wbxml"

	"easbridge/internal/mailsamples"
)

// Arabic mail through the whole stack (engine, adapter, patched library, WBXML over TLS,
// the model of Exchange 2019), with the MIME in the form the server uses: an inline string
// (what Exchange 2019 sends, real-Exchange finding 3) and OPAQUE. Raw 8-bit bodies in
// windows-1256 and ISO-8859-6 are not UTF-8: the bytes must arrive as they were.

func TestArabicMailThroughTheRealStack(t *testing.T) {
	for name, data := range map[string]func(string) *wbxml.Element{"an inline string": mimeData, "OPAQUE": opaqueData} {
		t.Run(name, func(t *testing.T) {
			s := newStack(t)
			s.m.mu.Lock()
			s.m.data = data
			s.m.mu.Unlock()

			// Small messages (sent inline by Sync) and big ones (cut by the truncation size and fetched
			// with ItemOperations), in every charset, with the subject and the name in both encodings.
			now := time.Now().UTC()
			want := map[string][]byte{}
			n := 0
			for i, subj := range mailsamples.Subjects {
				for k, cte := range []string{"8bit", "base64", "quoted-printable"} {
					id := fmt.Sprintf("ar%d%d@x", i, k)
					subjWord, nameWord := subj.B, mailsamples.Names[i].Q
					if k == 1 {
						subjWord, nameWord = subj.Q, mailsamples.Names[i].B
					}
					raw := mailsamples.Mail(id, subjWord, nameWord, mailsamples.Bodies[i], cte)
					want[id] = raw
					s.m.deliver("Inbox", raw, k == 0, now.Add(-time.Duration(30-n)*time.Minute))
					n++
				}
				// A big message: more than the inline limit of 2000 bytes, raw 8-bit.
				id := fmt.Sprintf("big%d@x", i)
				raw := mailsamples.Mail(id, subj.B, mailsamples.Names[i].Q, mailsamples.Bodies[i], "8bit")
				raw = append(raw, bytes.Repeat([]byte(mailsamples.Bodies[i].Raw), 80)...)
				want[id] = raw
				s.m.deliver("Inbox", raw, false, now.Add(-time.Duration(30-n)*time.Minute))
				n++
			}
			eventually(t, "every message is stored", func() bool { return s.count("INBOX") == n })

			f, _ := s.acct.Store().FolderByPath("INBOX")
			for id, raw := range want {
				uids, _ := s.acct.Store().UIDsByMessageID(f.ID, id)
				if len(uids) != 1 {
					t.Errorf("%s: %v", id, uids)
					continue
				}
				got, err := s.acct.Store().MIME(f.ID, uids[0])
				if err != nil || !bytes.Equal(got, raw) {
					t.Errorf("%s: stored %d bytes (%v), the server sent %d; the bytes differ", id, len(got), err, len(raw))
				}
				m, err := s.acct.Store().Message(f.ID, uids[0])
				if err != nil || m.Subject != mailsamples.Subject {
					t.Errorf("%s: stored subject %q (%v)", id, m.Subject, err)
				}
			}
			// Some of the messages needed ItemOperations, and the library took the MIME from it.
			if s.m.f.count("ItemOperations") < 3 {
				t.Errorf("only %d ItemOperations: the big messages were not fetched", s.m.f.count("ItemOperations"))
			}
			if strings.Contains(s.logText(), "no MIME") {
				t.Errorf("the log reports a missing MIME body:\n%s", s.logText())
			}
		})
	}
}
