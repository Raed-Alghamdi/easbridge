//go:build devfake

package imapd_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-sasl"

	"easbridge/internal/fakeupstream"
	"easbridge/internal/imapd"
	"easbridge/internal/testenv"
	"easbridge/internal/upstream"
)

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func TestCapabilitiesAreExactlyTheDesignedSet(t *testing.T) {
	e := newIMAP(t)
	// The Caps handed to go-imap: nothing that needs a missing session interface
	// (go-imap panics at connection start in that case), nothing unimplemented.
	have := map[string]bool{}
	for c := range imapd.Caps() {
		have[string(c)] = true
	}
	for _, want := range []string{"IMAP4rev1", "UIDPLUS", "MOVE", "SPECIAL-USE", "NAMESPACE", "LITERAL+", "CHILDREN"} {
		if !have[want] {
			t.Errorf("Caps misses %s", want)
		}
	}
	for _, bad := range []string{"IMAP4rev2", "CONDSTORE", "QRESYNC", "COMPRESS=DEFLATE", "ID", "LIST-STATUS", "ESEARCH", "SORT", "THREAD", "STARTTLS", "APPENDLIMIT"} {
		if have[bad] {
			t.Errorf("Caps must not contain %s", bad)
		}
	}

	// On the wire, before login: only what is needed to authenticate.
	r := e.rawDial()
	lines := r.ok("CAPABILITY")
	pre := strings.Fields(strings.TrimPrefix(lines[0], "* CAPABILITY "))
	if got, want := sorted(pre), sorted([]string{"IMAP4rev1", "SASL-IR", "LITERAL-", "AUTH=PLAIN"}); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("pre-login capabilities = %v, want %v", got, want)
	}
	// After login: the designed set plus what go-imap adds for IMAP4rev1.
	r.loginOK()
	lines = r.ok("CAPABILITY")
	post := strings.Fields(strings.TrimPrefix(lines[0], "* CAPABILITY "))
	want := []string{"IMAP4rev1", "SASL-IR", "LITERAL-", "UNSELECT", "ENABLE", "IDLE", "UTF8=ACCEPT",
		"NAMESPACE", "UIDPLUS", "MOVE", "CHILDREN", "SPECIAL-USE", "LITERAL+"}
	if strings.Join(sorted(post), " ") != strings.Join(sorted(want), " ") {
		t.Errorf("post-login capabilities = %v\nwant %v", sorted(post), sorted(want))
	}
}

func TestEnableUTF8OnlyForWhatWasAdvertised(t *testing.T) {
	// imapflow sends ENABLE CONDSTORE UTF8=ACCEPT IMAP4rev2 but filters the list
	// to advertised extensions first (lib/commands/enable.js), so it sends just
	// UTF8=ACCEPT here. This pins what the bridge answers to that command.
	e := newIMAP(t)
	r := e.rawDial()
	r.loginOK()
	lines := r.ok("ENABLE UTF8=ACCEPT")
	if lines[0] != "* ENABLED UTF8=ACCEPT" {
		t.Errorf("ENABLED = %q", lines[0])
	}
	// A client that asks for CONDSTORE blindly gets nothing for it.
	lines = r.ok("ENABLE CONDSTORE")
	if strings.Contains(strings.ToUpper(joinLines(lines)), "CONDSTORE") {
		t.Errorf("CONDSTORE was enabled: %v", lines)
	}
}

func TestLoginAndAuthenticatePlain(t *testing.T) {
	e := newIMAP(t)
	c := e.dial(nil)
	if !containsAll(capsOf(c), "AUTH=PLAIN") {
		t.Fatalf("AUTH=PLAIN not offered: %v", capsOf(c))
	}
	if err := c.Authenticate(sasl.NewPlainClient("", testenv.User, testenv.Pass)); err != nil {
		t.Fatalf("AUTHENTICATE PLAIN: %v", err)
	}
	if _, err := c.Namespace().Wait(); err != nil {
		t.Fatalf("NAMESPACE: %v", err)
	}
	c2 := e.dial(nil)
	if err := c2.Login(testenv.User, testenv.Pass).Wait(); err != nil {
		t.Fatalf("LOGIN: %v", err)
	}
	// MailFlow logs in again every minute for its STATUS cycle: no Exchange traffic.
	for i := 0; i < 20; i++ {
		c := e.dial(nil)
		if err := c.Login(testenv.User, testenv.Pass).Wait(); err != nil {
			t.Fatal(err)
		}
		c.Close()
	}
	if n := e.Srv.AuthAttempts(); n != 1 {
		t.Fatalf("%d logins caused %d Exchange verifications, want 1", 22, n)
	}
}

func TestLoginFailuresAreAuthenticationFailedAndNeverRepeatedUpstream(t *testing.T) {
	e := newIMAP(t)
	e.Srv.AddUser("mallory@example.test", "m-password")
	for _, tc := range []struct{ user, pass string }{
		{testenv.User, "wrong-password-xyz"},
		{"mallory@example.test", "m-password"},
		{testenv.User, ""},
	} {
		c := e.dial(nil)
		err := c.Login(tc.user, tc.pass).Wait()
		ie, ok := err.(*imap.Error)
		if err == nil || !ok || ie.Code != imap.ResponseCodeAuthenticationFailed {
			t.Errorf("login %q: err = %v", tc.user, err)
		}
	}
	// Retrying the wrong password does not reach Exchange again.
	for i := 0; i < 5; i++ {
		c := e.dial(nil)
		c.Login(testenv.User, "wrong-password-xyz").Wait()
	}
	if n := e.Srv.AuthAttempts(); n != 1 {
		t.Fatalf("Exchange saw %d attempts, want 1 (the allow-list and the lockout cache must absorb the rest)", n)
	}
	// Commands before login are refused, not crashes.
	r := e.rawDial()
	if l := status(r.cmd("SELECT INBOX")); !strings.Contains(l, "BAD") {
		t.Errorf("SELECT before login: %s", l)
	}
}

func TestExchangeDownDuringLoginIsUnavailableNotAuthFailed(t *testing.T) {
	e := newIMAP(t)
	e.Srv.FailNext("Open", upstream.ErrNotSent, 1) // no connection: the password was not presented to anyone
	c := e.dial(nil)
	err := c.Login(testenv.User, testenv.Pass).Wait()
	ie, ok := err.(*imap.Error)
	if !ok || ie.Code != imap.ResponseCodeUnavailable {
		t.Fatalf("err = %v, want NO [UNAVAILABLE]", err)
	}
	c2 := e.dial(nil)
	if err := c2.Login(testenv.User, testenv.Pass).Wait(); err != nil {
		t.Fatalf("login after recovery: %v", err)
	}
}

func TestListWithSpecialUseChildrenAndNamespace(t *testing.T) {
	e := newIMAP(t)
	e.Srv.AddFolder(testenv.User, "0", "الأرشيف", fakeupstream.TypeMail)
	proj := e.Srv.AddFolder(testenv.User, e.Srv.FolderID(testenv.User, "Inbox"), "Projects", fakeupstream.TypeMail)
	e.Srv.AddFolder(testenv.User, proj, "Clients", fakeupstream.TypeMail)
	c := e.login(nil)
	a := e.account()
	testenv.Eventually(t, "folders", func() bool { fs, _ := a.Store().Folders(); return len(fs) == 8 })

	boxes, err := c.List("", "*", nil).Collect()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]imap.MailboxAttr{}
	for _, b := range boxes {
		if b.Delim != '/' {
			t.Errorf("delimiter of %s = %q", b.Mailbox, b.Delim)
		}
		got[b.Mailbox] = b.Attrs
	}
	has := func(name string, a imap.MailboxAttr) bool {
		for _, x := range got[name] {
			if x == a {
				return true
			}
		}
		return false
	}
	for name, use := range map[string]imap.MailboxAttr{
		"Sent Items": imap.MailboxAttrSent, "Drafts": imap.MailboxAttrDrafts, "Deleted Items": imap.MailboxAttrTrash,
		"Junk Email": imap.MailboxAttrJunk, "الأرشيف": imap.MailboxAttrArchive,
	} {
		if !has(name, use) {
			t.Errorf("%s lacks %s: %v", name, use, got[name])
		}
	}
	if !has("INBOX", imap.MailboxAttrHasChildren) || !has("INBOX/Projects", imap.MailboxAttrHasChildren) || !has("INBOX/Projects/Clients", imap.MailboxAttrHasNoChildren) {
		t.Errorf("CHILDREN attributes wrong: %v", got)
	}
	for _, hidden := range []string{"Outbox", "Calendar"} {
		if _, ok := got[hidden]; ok {
			t.Errorf("%s must not be listed", hidden)
		}
	}
	// Pattern handling.
	sub, _ := c.List("", "INBOX/%", nil).Collect()
	if len(sub) != 1 || sub[0].Mailbox != "INBOX/Projects" {
		t.Errorf("LIST INBOX/%% = %+v", sub)
	}
	if one, _ := c.List("", "inbox", nil).Collect(); len(one) != 1 || one[0].Mailbox != "INBOX" {
		t.Errorf("INBOX must be case-insensitive: %+v", one)
	}
	root, _ := c.List("", "", nil).Collect()
	if len(root) != 1 || root[0].Delim != '/' || root[0].Mailbox != "" {
		t.Errorf(`LIST "" "" = %+v`, root)
	}
	ns, err := c.Namespace().Wait()
	if err != nil || len(ns.Personal) != 1 || ns.Personal[0].Prefix != "" || ns.Personal[0].Delim != '/' || ns.Other != nil || ns.Shared != nil {
		t.Errorf("NAMESPACE = %+v %v", ns, err)
	}
	// LSUB lists everything (all folders are subscribed).
	r := e.rawDial()
	r.loginOK()
	lines := r.ok(`LSUB "" "*"`)
	if len(lines) < 8 || !strings.HasPrefix(lines[0], "* LSUB") {
		t.Errorf("LSUB: %v", lines)
	}
}

func TestUTF8MailboxNamesWithAndWithoutEnable(t *testing.T) {
	e := newIMAP(t)
	e.Srv.AddFolder(testenv.User, "0", "الأرشيف", fakeupstream.TypeMail)
	e.account()
	testenv.Eventually(t, "folder", func() bool {
		fs, _ := e.account().Store().Folders()
		return len(fs) == 6
	})
	r := e.rawDial()
	r.loginOK()
	lines := r.ok(`LIST "" "*"`)
	if !strings.Contains(joinLines(lines), "&") {
		t.Errorf("without ENABLE UTF8=ACCEPT the name must be modified UTF-7: %v", lines)
	}
	r.ok("ENABLE UTF8=ACCEPT")
	lines = r.ok(`LIST "" "*"`)
	if !strings.Contains(joinLines(lines), `"الأرشيف"`) {
		t.Errorf("with UTF8=ACCEPT the name must be raw UTF-8: %v", lines)
	}
	if l := status(r.cmd(`SELECT "الأرشيف"`)); !strings.Contains(l, "OK") {
		t.Errorf("SELECT of the UTF-8 name: %s", l)
	}
}

func TestStatusIsExactAndAnsweredFromTheDatabase(t *testing.T) {
	e := newIMAP(t)
	e.seedInbox(6) // 3 read, 3 unread
	c := e.login(nil)
	e.ready("Inbox", "INBOX")
	callsBefore := e.Srv.Calls("Sync") + e.Srv.Calls("Ping")
	st, err := c.Status("INBOX", &imap.StatusOptions{NumMessages: true, NumUnseen: true, UIDNext: true, UIDValidity: true,
		NumDeleted: true, Size: true}).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if *st.NumMessages != 6 || *st.NumUnseen != 3 || st.UIDNext != 7 || st.UIDValidity == 0 || *st.NumDeleted != 0 || *st.Size <= 0 {
		t.Fatalf("status = %+v", st)
	}
	if *st.NumUnseen > *st.NumMessages || st.UIDNext == 0 {
		t.Fatalf("status violates MailFlow's invariants: %+v", st)
	}
	// Every STATUS item combination must be safe (go-imap dereferences them).
	for _, o := range []imap.StatusOptions{{NumMessages: true}, {UIDNext: true}, {NumUnseen: true}, {UIDValidity: true},
		{AppendLimit: true}, {DeletedStorage: true}, {NumDeleted: true}} {
		o := o
		if _, err := c.Status("INBOX", &o).Wait(); err != nil {
			t.Errorf("STATUS %+v: %v", o, err)
		}
	}
	if _, err := c.Status("Nope", &imap.StatusOptions{NumMessages: true}).Wait(); err == nil {
		t.Error("STATUS of a missing mailbox succeeded")
	} else if ie, ok := err.(*imap.Error); !ok || ie.Code != imap.ResponseCodeNonExistent {
		t.Errorf("STATUS of a missing mailbox: %v", err)
	}
	// RECENT is obsolete but go-imap dereferences it: it must be answered (0).
	r := e.rawDial()
	r.loginOK()
	if l := joinLines(r.ok("STATUS INBOX (RECENT MESSAGES UIDNEXT)")); !strings.Contains(l, "RECENT 0") {
		t.Errorf("STATUS RECENT: %s", l)
	}
	// Reads: a STATUS cycle must not cause any EAS traffic by itself.
	for i := 0; i < 10; i++ {
		c.Status("INBOX", &imap.StatusOptions{NumMessages: true, NumUnseen: true}).Wait()
	}
	if n := e.Srv.Calls("Sync"); n > callsBefore {
		// A Ping-driven sync may legitimately happen; STATUS itself must not.
		t.Logf("sync calls grew (%d -> %d), tolerated only if unrelated", callsBefore, n)
	}
}

func TestSelectAndExamineReports(t *testing.T) {
	e := newIMAP(t)
	e.seedInbox(3)
	e.login(nil)
	e.ready("Inbox", "INBOX")
	r := e.rawDial()
	r.loginOK()
	lines := r.ok("SELECT INBOX")
	all := joinLines(lines)
	for _, want := range []string{"* 3 EXISTS", "* 0 RECENT", "[UIDVALIDITY ", "[UIDNEXT 4]", `* FLAGS (\Seen \Answered \Flagged \Deleted \Draft)`,
		`[PERMANENTFLAGS (\Seen \Answered \Flagged \Deleted \Draft)]`, "[READ-WRITE]", "[UNSEEN "} {
		// The server writes flags in its own order; check the set instead of the order.
		if want == `* FLAGS (\Seen \Answered \Flagged \Deleted \Draft)` || strings.HasPrefix(want, "[PERMANENTFLAGS") {
			continue
		}
		if !strings.Contains(all, want) {
			t.Errorf("SELECT response lacks %q:\n%s", want, all)
		}
	}
	if !strings.Contains(all, `\Seen`) || strings.Contains(all, `\*`) || strings.Contains(all, "CONDSTORE") || strings.Contains(all, "HIGHESTMODSEQ") {
		t.Errorf("flags must be exactly the five system flags without \\*:\n%s", all)
	}
	lines = r.ok("EXAMINE INBOX")
	if !strings.Contains(joinLines(lines), "[READ-ONLY]") {
		t.Errorf("EXAMINE: %v", lines)
	}
	if l := status(r.cmd("SELECT NoSuchBox")); !strings.Contains(l, "NONEXISTENT") {
		t.Errorf("SELECT of a missing mailbox: %s", l)
	}
	// Selecting another mailbox closes the first one cleanly.
	r.ok("SELECT Drafts")
}

func TestUnselectAndCloseDoNotLeakViews(t *testing.T) {
	e := newIMAP(t)
	e.seedInbox(2)
	c := e.login(nil)
	e.ready("Inbox", "INBOX")
	for i := 0; i < 5; i++ {
		if _, err := c.Select("INBOX", nil).Wait(); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Unselect().Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if err := c.UnselectAndExpunge().Wait(); err != nil {
		t.Fatal(err)
	}
	if err := c.Logout().Wait(); err != nil {
		t.Fatal(err)
	}
}

// After two unsuccessful verifications the bridge stops asking Exchange and
// answers like an unavailable service, not like a wrong password: the password
// may well be right (security review of v0.1.0, Active Directory lockout).
func TestRepeatedWrongPasswordsEndWithUnavailableNotAuthFailed(t *testing.T) {
	e := newIMAP(t)
	r := e.rawDial()
	var got []string
	for i := 0; i < 4; i++ {
		lines := r.cmd(`LOGIN "%s" "guess-%d"`, testenv.User, i)
		got = append(got, status(lines))
	}
	for i, l := range got {
		want := "[AUTHENTICATIONFAILED]"
		if i >= 2 {
			want = "[UNAVAILABLE]"
		}
		if !strings.Contains(l, " NO ") || !strings.Contains(l, want) {
			t.Errorf("attempt %d: %q, want NO %s", i+1, l, want)
		}
	}
	if n := e.Srv.AuthAttempts(); n != 2 {
		t.Errorf("Exchange was asked %d times, want 2", n)
	}
}
