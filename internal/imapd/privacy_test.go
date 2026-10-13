//go:build devfake

package imapd_test

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-sasl"

	"easbridge/internal/fakeupstream"
	"easbridge/internal/testenv"
)

// DESIGN section 3: credentials never reach a log, and neither do subjects,
// addresses or bodies. This runs a full session (good and bad logins, PLAIN and
// LOGIN, FETCH/SEARCH/STORE/APPEND/MOVE) at DEBUG level and scans everything the
// bridge logged.
func TestLogsContainNeitherPasswordsNorMailContent(t *testing.T) {
	e := newIMAP(t)
	inbox := e.Srv.FolderID(testenv.User, "Inbox")
	d := e.Clock.Now().Add(-time.Hour)
	e.Srv.AddMessage(testenv.User, inbox, multipartMessage("secret-id@x", d), fakeupstream.AddOptions{Date: d})
	e.ready("Inbox", "INBOX") // starts the engine through a manager login

	// Wrong passwords (one allowed user, one not), LOGIN and AUTHENTICATE PLAIN.
	wrong := e.dial(nil)
	wrong.Login(testenv.User, "totally-wrong-PW-9981").Wait()
	wrong2 := e.dial(nil)
	wrong2.Login("stranger@example.test", "stranger-PW-4417").Wait()
	plain := e.dial(nil)
	if err := plain.Authenticate(sasl.NewPlainClient("", testenv.User, testenv.Pass)); err != nil {
		t.Fatal(err)
	}
	c := e.login(nil)
	c.Select("INBOX", nil).Wait()
	c.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{Envelope: true, BodySection: []*imap.FetchItemBodySection{{}}}).Collect()
	c.UIDSearch(&imap.SearchCriteria{Text: []string{"html part"}}, nil).Wait()
	c.Store(imap.UIDSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagFlagged}}, nil).Collect()
	c.Move(imap.UIDSetNum(1), "Junk Email").Wait()
	msg := testenv.Mail("appended-id@x", "Appended confidential subject", d, "")
	cmd := c.Append("Drafts", int64(len(msg)), nil)
	cmd.Write(msg)
	cmd.Close()
	cmd.Wait()
	time.Sleep(300 * time.Millisecond) // let the engine log what it wants to

	logs := e.Logs.String()
	if len(logs) < 200 {
		t.Fatalf("suspiciously little log output to check:\n%s", logs)
	}
	forbidden := []string{
		testenv.Pass, "totally-wrong-PW-9981", "stranger-PW-4417",
		base64.StdEncoding.EncodeToString([]byte(testenv.User + ":" + testenv.Pass)),
		base64.StdEncoding.EncodeToString([]byte("\x00" + testenv.User + "\x00" + testenv.Pass)),
		base64.StdEncoding.EncodeToString([]byte(testenv.Pass)),
		"Quarterly report", "Appended confidential", "plain part text", "html part", "secret-id@x", "appended-id@x",
		"bob@example.test", "carol@example.test", "stranger@example.test", testenv.User,
	}
	for _, f := range forbidden {
		if strings.Contains(logs, f) {
			t.Errorf("log output contains %q", f)
		}
	}
	if t.Failed() {
		t.Logf("full log:\n%s", logs)
	}
}
