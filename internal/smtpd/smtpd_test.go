//go:build devfake

package smtpd_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"easbridge/internal/engine"
	"easbridge/internal/fakeupstream"
	"easbridge/internal/imapd"
	"easbridge/internal/smtpd"
	"easbridge/internal/testenv"
	"easbridge/internal/testpki"
	"easbridge/internal/upstream"
)

type senv struct {
	*testenv.Env
	pki      *testpki.PKI
	smtp     *smtpd.Server
	addr     string
	imap     *imapd.Server
	imapAddr string
}

func newEnv(t *testing.T, mods ...func(*engine.Config, *fakeupstream.Server)) *senv {
	t.Helper()
	te := testenv.New(t, 4096, mods...)
	pki, err := testpki.New()
	if err != nil {
		t.Fatal(err)
	}
	e := &senv{Env: te, pki: pki}
	e.smtp = smtpd.NewServer(te.Mgr, te.Log, te.Cfg.MaxMessageBytes, "easbridge")
	ln, err := tls.Listen("tcp", "127.0.0.1:0", pki.ServerConfig())
	if err != nil {
		t.Fatal(err)
	}
	go e.smtp.Serve(ln)
	e.addr = ln.Addr().String()
	e.imap = imapd.NewServer(te.Mgr, te.Log)
	iln, err := tls.Listen("tcp", "127.0.0.1:0", pki.ServerConfig())
	if err != nil {
		t.Fatal(err)
	}
	go e.imap.Serve(iln)
	e.imapAddr = iln.Addr().String()
	t.Cleanup(func() { e.smtp.Close(); e.imap.Close() })
	return e
}

func (e *senv) dial() *smtp.Client {
	e.T.Helper()
	c, err := smtp.DialTLS(e.addr, e.pki.ClientConfig("easbridge"))
	if err != nil {
		e.T.Fatal(err)
	}
	e.T.Cleanup(func() { c.Close() })
	return c
}

func (e *senv) authed() *smtp.Client {
	e.T.Helper()
	c := e.dial()
	if err := c.Auth(sasl.NewPlainClient("", testenv.User, testenv.Pass)); err != nil {
		e.T.Fatalf("AUTH PLAIN: %v", err)
	}
	return c
}

func smtpCode(err error) (int, string) {
	var se *smtp.SMTPError
	if errors.As(err, &se) {
		return se.Code, fmt.Sprintf("%d.%d.%d", se.EnhancedCode[0], se.EnhancedCode[1], se.EnhancedCode[2])
	}
	return 0, ""
}

func message(id, subject string) []byte {
	return []byte("Message-ID: <" + id + ">\r\nFrom: Alice <alice@example.test>\r\nTo: Bob <bob@example.test>\r\nCc: carol@example.test\r\n" +
		"Subject: " + subject + "\r\nDate: Fri, 09 Oct 2026 12:00:00 +0000\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nhello there\r\n")
}

func TestAuthIsRequiredAndWorksWithPlainAndLogin(t *testing.T) {
	e := newEnv(t)
	c := e.dial()
	if ok, _ := c.Extension("AUTH"); !ok {
		t.Fatal("AUTH not offered inside TLS")
	}
	if ok, _ := c.Extension("STARTTLS"); ok {
		t.Error("STARTTLS must not be offered: the listener is already TLS")
	}
	if !c.SupportsAuth("PLAIN") || !c.SupportsAuth("LOGIN") || c.SupportsAuth("CRAM-MD5") {
		t.Error("AUTH mechanisms must be PLAIN and LOGIN only")
	}
	if size, ok := c.MaxMessageSize(); !ok || size != int(e.Cfg.MaxMessageBytes) {
		t.Errorf("SIZE = %d %v", size, ok)
	}
	// Anything before AUTH is refused.
	if err := c.Mail("alice@example.test", nil); err == nil {
		t.Fatal("MAIL FROM accepted before AUTH")
	} else if code, _ := smtpCode(err); code != 502 && code != 530 {
		t.Errorf("MAIL before AUTH: %v", err)
	}
	if err := c.Auth(sasl.NewPlainClient("", testenv.User, testenv.Pass)); err != nil {
		t.Fatalf("PLAIN: %v", err)
	}
	c2 := e.dial()
	if err := c2.Auth(sasl.NewLoginClient(testenv.User, testenv.Pass)); err != nil {
		t.Fatalf("LOGIN: %v", err)
	}
	if err := c2.Mail("alice@example.test", nil); err != nil {
		t.Fatalf("MAIL after LOGIN: %v", err)
	}
	// Both used the same engine and one verification.
	if n := e.Srv.AuthAttempts(); n != 1 {
		t.Errorf("%d Exchange verifications, want 1", n)
	}
}

func TestAuthFailuresMatchTheIMAPPolicy(t *testing.T) {
	e := newEnv(t)
	e.Srv.AddUser("mallory@example.test", "mallory-pw")
	for _, tc := range []struct{ name, user, pass string }{
		{"wrong password", testenv.User, "wrong-password-xyz"},
		{"outside the allow-list", "mallory@example.test", "mallory-pw"},
	} {
		c := e.dial()
		err := c.Auth(sasl.NewPlainClient("", tc.user, tc.pass))
		if code, enh := smtpCode(err); code != 535 || enh != "5.7.8" {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	for i := 0; i < 4; i++ { // identical wrong password: refused locally
		e.dial().Auth(sasl.NewLoginClient(testenv.User, "wrong-password-xyz"))
	}
	if n := e.Srv.AuthAttempts(); n != 1 {
		t.Fatalf("Exchange saw %d attempts, want 1", n)
	}
	// Exchange down while verifying: a temporary failure, not a bad password.
	e2 := newEnv(t)
	e2.Srv.FailNext("Open", upstream.ErrNotSent, 1) // no connection: the password was not presented to anyone
	err := e2.dial().Auth(sasl.NewPlainClient("", testenv.User, testenv.Pass))
	if code, _ := smtpCode(err); code != 454 {
		t.Errorf("Exchange down during AUTH: %v", err)
	}
	if err := e2.dial().Auth(sasl.NewPlainClient("", testenv.User, testenv.Pass)); err != nil {
		t.Errorf("AUTH after recovery: %v", err)
	}
}

func TestAuthRefusedOnPlaintextConnections(t *testing.T) {
	e := newEnv(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go e.smtp.Serve(ln) // a plain listener, which the product never opens
	c, err := smtp.Dial(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if ok, _ := c.Extension("AUTH"); ok {
		t.Error("AUTH advertised on a plaintext connection")
	}
	err = c.Auth(sasl.NewPlainClient("", testenv.User, testenv.Pass))
	if err == nil {
		t.Fatal("AUTH worked without TLS")
	}
	if n := e.Srv.AuthAttempts(); n != 0 {
		t.Errorf("credentials from a plaintext connection reached Exchange (%d)", n)
	}
}

func TestBccOnlyRecipientsAreAddedToTheHeaders(t *testing.T) {
	e := newEnv(t)
	c := e.authed()
	msg := message("bcc-test@mailflow", "With Bcc")
	// Envelope: To + Cc as in the headers, plus two Bcc-only recipients (one repeated).
	rcpts := []string{"bob@example.test", "carol@example.test", "dave@example.test", "Eve@Example.test", "dave@example.test"}
	if err := c.SendMail("alice@example.test", rcpts, bytes.NewReader(msg)); err != nil {
		t.Fatalf("SendMail: %v", err)
	}
	sent := e.Srv.Sent(testenv.User)
	if len(sent) != 1 {
		t.Fatalf("Exchange received %d submissions", len(sent))
	}
	got := string(sent[0].MIME)
	if !strings.Contains(got, "Bcc: dave@example.test, Eve@Example.test\r\n") {
		t.Errorf("Bcc header missing or wrong:\n%s", got)
	}
	// Otherwise the message is exactly what the client sent.
	want := strings.Replace(string(msg), "Subject: With Bcc\r\n", "Subject: With Bcc\r\n", 1)
	stripped := strings.Replace(got, "Bcc: dave@example.test, Eve@Example.test\r\n", "", 1)
	if stripped != want {
		t.Errorf("the message was altered beyond the Bcc header:\n%q\nvs\n%q", stripped, want)
	}
	if !sent[0].SaveInSent {
		t.Error("SaveInSentItems must be requested")
	}
	rc := strings.Join(sent[0].Recipients, ",")
	for _, a := range []string{"bob@", "carol@", "dave@", "eve@"} {
		if !strings.Contains(rc, a) {
			t.Errorf("recipient %s missing from %s", a, rc)
		}
	}
	// A message that needs no help is passed through byte for byte.
	if err := c.SendMail("alice@example.test", []string{"bob@example.test", "carol@example.test"}, bytes.NewReader(message("plain@x", "Plain"))); err != nil {
		t.Fatal(err)
	}
	if got := e.Srv.Sent(testenv.User)[1].MIME; !bytes.Equal(got, message("plain@x", "Plain")) {
		t.Errorf("an unchanged message was modified")
	}
}

func TestSizeLimitAndRecipientRules(t *testing.T) {
	e := newEnv(t)
	c := e.authed()
	big := append(message("big@x", "big"), bytes.Repeat([]byte(strings.Repeat("x", 76)+"\r\n"), 2<<20/78)...)
	err := c.SendMail("alice@example.test", []string{"bob@example.test"}, bytes.NewReader(big))
	if code, enh := smtpCode(err); code != 552 || enh != "5.3.4" {
		t.Fatalf("oversized message: %v", err)
	}
	// The connection is still usable after the refusal.
	if err := c.Reset(); err != nil {
		t.Errorf("RSET after a refused DATA: %v", err)
	}
	if err := c.SendMail("alice@example.test", []string{"bob@example.test"}, bytes.NewReader(message("ok@x", "ok"))); err != nil {
		t.Errorf("next message: %v", err)
	}
	// DATA without any RCPT is refused by the protocol layer.
	c2 := e.authed()
	if err := c2.Mail("alice@example.test", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Data(); err == nil {
		t.Error("DATA accepted without a recipient")
	}
}

func TestARetriedSubmissionIsDeliveredOnce(t *testing.T) {
	// MailFlow submits again when it never saw the answer to the first attempt
	// (the connection dropped after Exchange had taken the message). Exchange
	// recognises the ClientId, derived from the Message-ID, and sends only once.
	e := newEnv(t)
	msg := message("retry-1@mailflow", "Retried")
	for i := 0; i < 3; i++ {
		if err := e.authed().SendMail("alice@example.test", []string{"bob@example.test"}, bytes.NewReader(msg)); err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	if n := len(e.Srv.Sent(testenv.User)); n != 1 {
		t.Fatalf("Exchange sent %d copies of one message", n)
	}
	if n := e.Srv.Calls("SendMail duplicate"); n != 2 {
		t.Errorf("Exchange saw %d repeats, want 2", n)
	}
	// The client rebuilt the message for the retry: new Date, same Message-ID.
	rebuilt := bytes.Replace(msg, []byte("12:00:00"), []byte("12:00:31"), 1)
	if err := e.authed().SendMail("alice@example.test", []string{"bob@example.test"}, bytes.NewReader(rebuilt)); err != nil {
		t.Fatal(err)
	}
	if n := len(e.Srv.Sent(testenv.User)); n != 1 {
		t.Errorf("a rebuilt retry was sent again: %d copies", n)
	}
	// Another message goes out as usual.
	if err := e.authed().SendMail("alice@example.test", []string{"bob@example.test"}, bytes.NewReader(message("retry-2@mailflow", "Next"))); err != nil {
		t.Fatal(err)
	}
	if n := len(e.Srv.Sent(testenv.User)); n != 2 {
		t.Errorf("%d messages sent, want 2", n)
	}
}

func TestUpstreamErrorsBecomeTheRightSMTPReplies(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name string
		err  error
		code int
		enh  string
	}{
		{"no recipients 119", &upstream.StatusError{Op: "SendMail", Code: 119, Perm: true}, 554, "5.5.3"},
		{"recipient unresolved 116", &upstream.StatusError{Op: "SendMail", Code: 116, Perm: true}, 550, "5.1.1"},
		{"submission failed 120", &upstream.StatusError{Op: "SendMail", Code: 120, Perm: true}, 554, "5.3.0"},
		{"invalid mime 107", &upstream.StatusError{Op: "SendMail", Code: 107, Perm: true}, 554, "5.6.0"},
		{"server error retry 111", &upstream.StatusError{Op: "SendMail", Code: 111}, 451, "4.3.0"},
		{"mailbox full 113", &upstream.StatusError{Op: "SendMail", Code: 113, Perm: true}, 552, "5.2.2"},
		{"send quota 115", &upstream.StatusError{Op: "SendMail", Code: 115}, 452, "4.5.3"},
		{"attachment too large 122", &upstream.StatusError{Op: "SendMail", Code: 122, Perm: true}, 552, "5.3.4"},
		{"access denied 130", &upstream.StatusError{Op: "SendMail", Code: 130, Perm: true}, 550, "5.7.1"},
		{"account disabled 131", &upstream.StatusError{Op: "SendMail", Code: 131, Perm: true}, 550, "5.7.1"},
		{"unknown permanent", &upstream.StatusError{Op: "SendMail", Code: 999, Perm: true}, 554, "5.3.0"},
		{"unknown temporary", &upstream.StatusError{Op: "SendMail", Code: 998}, 451, "4.3.0"},
		{"too large", upstream.ErrTooLarge, 552, "5.3.4"},
		{"unavailable", upstream.ErrUnavailable, 451, "4.4.1"},
		{"wrapped unavailable", fmt.Errorf("send: %w", upstream.ErrUnavailable), 451, "4.4.1"},
		{"something else", errors.New("boom"), 451, "4.3.0"},
	}
	for _, tc := range cases {
		e.Srv.FailNext("SendMail", tc.err, 1)
		c := e.authed()
		err := c.SendMail("alice@example.test", []string{"bob@example.test"}, bytes.NewReader(message("e@x", tc.name)))
		if code, enh := smtpCode(err); code != tc.code || enh != tc.enh {
			t.Errorf("%s: got %v, want %d %s", tc.name, err, tc.code, tc.enh)
		}
	}
	// Pure mapping of engine-level errors.
	for err, code := range map[error]int{engine.ErrTooLarge: 552, engine.ErrUnavailable: 451, engine.ErrAuthFailed: 454, engine.ErrWiped: 451} {
		if got, _ := smtpCode(smtpd.MapSendError(err)); got != code {
			t.Errorf("MapSendError(%v) = %d, want %d", err, got, code)
		}
	}
	if smtpd.MapSendError(nil) != nil {
		t.Error("nil must map to nil")
	}
}

// The whole MailFlow send flow across both protocols: SMTP submission, then the
// IMAP APPEND of the sent copy, then Exchange's own copy arriving by sync. One
// message in Sent, same UID throughout.
func TestSendThenAppendToSentLeavesExactlyOneCopy(t *testing.T) {
	e := newEnv(t)
	e.Srv.HoldSentCopies(testenv.User, true)
	msg := message("flow-1@mailflow", "Flow")
	if err := e.authed().SendMail("alice@example.test", []string{"bob@example.test", "carol@example.test", "zed@example.test"}, bytes.NewReader(msg)); err != nil {
		t.Fatal(err)
	}
	ic, err := imapclient.DialTLS(e.imapAddr, &imapclient.Options{TLSConfig: e.pki.ClientConfig("easbridge")})
	if err != nil {
		t.Fatal(err)
	}
	defer ic.Close()
	if err := ic.Login(testenv.User, testenv.Pass).Wait(); err != nil {
		t.Fatal(err)
	}
	acct, _ := e.Mgr.Login(nil, testenv.User, testenv.Pass)
	testenv.Eventually(t, "folders", func() bool { fs, _ := acct.Store().Folders(); return len(fs) == 5 })
	cmd := ic.Append("Sent Items", int64(len(msg)), &imap.AppendOptions{Flags: []imap.Flag{imap.FlagSeen}})
	cmd.Write(msg)
	cmd.Close()
	ad, err := cmd.Wait()
	if err != nil || ad.UID != 1 {
		t.Fatalf("APPEND: %+v %v", ad, err)
	}
	e.Srv.ReleaseSentCopies(testenv.User)
	sentID := e.Srv.FolderID(testenv.User, "Sent Items")
	testenv.Eventually(t, "Exchange's copy synced and bound", func() bool {
		f, _ := acct.Store().FolderByPath("Sent Items")
		m, err := acct.Store().Message(f.ID, 1)
		return err == nil && m.ServerID != "" && len(e.Srv.Items(testenv.User, sentID)) == 1
	})
	time.Sleep(150 * time.Millisecond)
	st, err := ic.Status("Sent Items", &imap.StatusOptions{NumMessages: true, UIDNext: true}).Wait()
	if err != nil || *st.NumMessages != 1 || st.UIDNext != 2 {
		t.Fatalf("Sent status = %+v %v: the copy was duplicated", st, err)
	}
}

func TestSMTPLogsContainNoCredentialsOrMailContent(t *testing.T) {
	e := newEnv(t)
	bad := e.dial()
	bad.Auth(sasl.NewLoginClient(testenv.User, "smtp-wrong-PW-5521"))
	ok := e.dial()
	if err := ok.Auth(sasl.NewLoginClient(testenv.User, testenv.Pass)); err != nil {
		t.Fatal(err)
	}
	ok.SendMail("alice@example.test", []string{"bob@example.test", "hidden-bcc@example.test"},
		bytes.NewReader(message("log-1@x", "Highly confidential subject")))
	e.Srv.FailNext("SendMail", errors.New("boom"), 1)
	ok2 := e.authed()
	ok2.SendMail("alice@example.test", []string{"bob@example.test"}, bytes.NewReader(message("log-2@x", "Second confidential")))
	time.Sleep(200 * time.Millisecond)
	logs := e.Logs.String()
	if len(logs) < 100 {
		t.Fatalf("too little log output to check: %q", logs)
	}
	for _, f := range []string{testenv.Pass, "smtp-wrong-PW-5521", "Highly confidential", "Second confidential", "hello there",
		"hidden-bcc@example.test", "bob@example.test", "log-1@x", testenv.User} {
		if strings.Contains(logs, f) {
			t.Errorf("log output contains %q", f)
		}
	}
	if t.Failed() {
		t.Logf("log:\n%s", logs)
	}
	_ = fakeupstream.TypeMail
}

func TestRepeatedWrongPasswordsGetATemporaryFailure(t *testing.T) {
	e := newEnv(t)
	var codes []int
	for i := 0; i < 4; i++ {
		err := e.dial().Auth(sasl.NewPlainClient("", testenv.User, fmt.Sprintf("guess-%d", i)))
		code, _ := smtpCode(err)
		codes = append(codes, code)
	}
	if want := []int{535, 535, 454, 454}; fmt.Sprint(codes) != fmt.Sprint(want) {
		t.Errorf("replies %v, want %v (two verifications, then a temporary failure)", codes, want)
	}
	if n := e.Srv.AuthAttempts(); n != 2 {
		t.Errorf("Exchange was asked %d times, want 2", n)
	}
}

// While the circuit breaker is open nothing is sent: the submission is refused
// with a 4xx, so that the client keeps the message and retries, and nothing
// reaches Exchange.
func TestSubmissionWhileTheBreakerIsOpenGetsA4xx(t *testing.T) {
	e := newEnv(t, func(c *engine.Config, _ *fakeupstream.Server) {
		c.BreakerBase = 5 * time.Second
		c.BreakerMax = 15 * time.Minute
	})
	msg := message("paused-1@mailflow", "While paused")
	// Exchange throttles the send itself: this attempt fails and opens the breaker.
	e.Srv.FailNext("SendMail", &upstream.ThrottleError{HTTP: 429, RetryAfter: 10 * time.Minute, Explicit: true}, 1)
	err := e.authed().SendMail("alice@example.test", []string{"bob@example.test"}, bytes.NewReader(msg))
	if code, _ := smtpCode(err); code != 451 {
		t.Fatalf("the throttled attempt: %v, want 451", err)
	}
	calls := e.Srv.Calls("SendMail")
	// The retry arrives during the pause: refused at once, Exchange is not asked.
	err = e.authed().SendMail("alice@example.test", []string{"bob@example.test"}, bytes.NewReader(msg))
	if code, enh := smtpCode(err); code != 451 || enh != "4.4.1" {
		t.Fatalf("during the pause: %v, want 451 4.4.1", err)
	}
	if e.Srv.Calls("SendMail") != calls || len(e.Srv.Sent(testenv.User)) != 0 {
		t.Fatal("a message reached Exchange while the breaker was open")
	}
	// After the pause the same submission goes through, once.
	e.Clock.Advance(10*time.Minute + time.Second)
	if err := e.authed().SendMail("alice@example.test", []string{"bob@example.test"}, bytes.NewReader(msg)); err != nil {
		t.Fatalf("after the pause: %v", err)
	}
	if n := len(e.Srv.Sent(testenv.User)); n != 1 {
		t.Fatalf("%d messages sent", n)
	}
}

// ---- The Sent copy shows the Bcc recipients (v0.1.8, real-Exchange finding 4) ----
//
// The whole MailFlow flow across both protocols, with a recipient that exists only
// in the SMTP envelope. The bridge writes it into a Bcc header for Exchange, which
// delivers the message without that header and saves a Sent Items copy without it
// (the fake does the same). MailFlow's Sent folder must nevertheless show the
// recipient: on the UID MailFlow got from its APPEND, with nothing new announced
// when Exchange's copy arrives.

const bccRcpt = "hidden-bcc@example.test"

// bccFlow is what one test of the flow sees.
type bccFlow struct {
	e         *senv
	ic        *imapclient.Client
	ev        *clientEvents
	submitted []byte // what the bridge handed to Exchange
	client    []byte // the copy MailFlow APPENDs: no Bcc header
}

type clientEvents struct {
	mu     sync.Mutex
	exists []uint32
	other  []string // expunges and flag updates
}

func (ev *clientEvents) handler() *imapclient.UnilateralDataHandler {
	return &imapclient.UnilateralDataHandler{
		Expunge: func(n uint32) {
			ev.mu.Lock()
			ev.other = append(ev.other, fmt.Sprintf("expunge %d", n))
			ev.mu.Unlock()
		},
		Mailbox: func(d *imapclient.UnilateralDataMailbox) {
			if d.NumMessages != nil {
				ev.mu.Lock()
				ev.exists = append(ev.exists, *d.NumMessages)
				ev.mu.Unlock()
			}
		},
		Fetch: func(m *imapclient.FetchMessageData) {
			b, err := m.Collect()
			ev.mu.Lock()
			ev.other = append(ev.other, fmt.Sprintf("fetch uid %d %v", b.UID, err))
			ev.mu.Unlock()
		},
	}
}

func (ev *clientEvents) counts() (exists int, other []string) {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	return len(ev.exists), append([]string(nil), ev.other...)
}

func newBccFlow(t *testing.T, id string) *bccFlow {
	t.Helper()
	e := newEnv(t)
	f := &bccFlow{e: e, ev: &clientEvents{}}
	ic, err := imapclient.DialTLS(e.imapAddr, &imapclient.Options{TLSConfig: e.pki.ClientConfig("easbridge"), UnilateralDataHandler: f.ev.handler()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ic.Close() })
	if err := ic.Login(testenv.User, testenv.Pass).Wait(); err != nil {
		t.Fatal(err)
	}
	f.ic = ic
	acct, err := e.Mgr.Login(context.Background(), testenv.User, testenv.Pass)
	if err != nil {
		t.Fatal(err)
	}
	testenv.Eventually(t, "the Sent folder is known and open", func() bool {
		fo, err := acct.Store().FolderByPath("Sent Items")
		return err == nil && fo.InitialComplete
	})
	return f
}

// send submits a message over SMTP: To bob, Cc carol, and bccRcpt in the envelope only.
func (f *bccFlow) send(t *testing.T, id, subject string) {
	t.Helper()
	msg := message(id, subject)
	rcpts := []string{"bob@example.test", "carol@example.test", bccRcpt}
	if err := f.e.authed().SendMail("alice@example.test", rcpts, bytes.NewReader(msg)); err != nil {
		t.Fatalf("SendMail: %v", err)
	}
	sent := f.e.Srv.Sent(testenv.User)
	got := sent[len(sent)-1]
	// What the bridge hands to Exchange is what it handed to Exchange before: the
	// message with one Bcc header for the recipient that was in the envelope only.
	if want := strings.Replace(string(msg), "\r\n\r\n", "\r\nBcc: "+bccRcpt+"\r\n\r\n", 1); string(got.MIME) != want {
		t.Fatalf("the submission changed:\n%q\nwant\n%q", got.MIME, want)
	}
	// And what Exchange delivers to the To recipient has no Bcc header and does not
	// name the hidden recipient anywhere.
	if bytes.Contains(got.Delivered, []byte("\r\nBcc:")) || bytes.Contains(got.Delivered, []byte(bccRcpt)) || !bytes.Contains(got.Delivered, []byte("To: Bob <bob@example.test>")) {
		t.Fatalf("the copy delivered to the To recipient:\n%s", got.Delivered)
	}
	f.submitted, f.client = got.MIME, msg
}

func (f *bccFlow) appendClientCopy(t *testing.T) imap.UID {
	t.Helper()
	cmd := f.ic.Append("Sent Items", int64(len(f.client)), &imap.AppendOptions{Flags: []imap.Flag{imap.FlagSeen}})
	cmd.Write(f.client)
	cmd.Close()
	ad, err := cmd.Wait()
	if err != nil {
		t.Fatalf("APPEND: %v", err)
	}
	return ad.UID
}

// fetch is MailFlow's FETCH of a new message: UID FLAGS BODYSTRUCTURE ENVELOPE
// INTERNALDATE RFC822.SIZE BODY.PEEK[HEADER], and the whole message.
func (f *bccFlow) fetch(t *testing.T, uid imap.UID) *imapclient.FetchMessageBuffer {
	t.Helper()
	msgs, err := f.ic.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{
		UID: true, Flags: true, InternalDate: true, RFC822Size: true, Envelope: true,
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
		BodySection: []*imap.FetchItemBodySection{
			{Specifier: imap.PartSpecifierHeader, Peek: true},
			{Peek: true},
		},
	}).Collect()
	if err != nil || len(msgs) != 1 {
		t.Fatalf("FETCH of UID %d: %d messages, %v", uid, len(msgs), err)
	}
	return msgs[0]
}

// expectBcc checks that FETCH shows the hidden recipient in every way MailFlow can
// ask for it, and that what it shows is the submitted message.
func (f *bccFlow) expectBcc(t *testing.T, uid imap.UID, when string) {
	t.Helper()
	m := f.fetch(t, uid)
	if m.Envelope == nil || len(m.Envelope.Bcc) != 1 || m.Envelope.Bcc[0].Mailbox+"@"+m.Envelope.Bcc[0].Host != bccRcpt {
		t.Errorf("%s: ENVELOPE Bcc = %+v", when, m.Envelope)
	}
	hdr := m.FindBodySection(&imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, Peek: true})
	if !bytes.Contains(hdr, []byte("\r\nBcc: "+bccRcpt+"\r\n")) {
		t.Errorf("%s: BODY[HEADER] has no Bcc header:\n%s", when, hdr)
	}
	whole := m.FindBodySection(&imap.FetchItemBodySection{Peek: true})
	if !bytes.Equal(whole, f.submitted) {
		t.Errorf("%s: BODY[] is not the submitted message:\n%q", when, whole)
	}
	if m.RFC822Size != int64(len(f.submitted)) || int64(len(whole)) != m.RFC822Size {
		t.Errorf("%s: RFC822.SIZE = %d, BODY[] has %d bytes, the submitted message %d", when, m.RFC822Size, len(whole), len(f.submitted))
	}
	if m.UID != uid {
		t.Errorf("%s: UID %d, want %d", when, m.UID, uid)
	}
}

func (f *bccFlow) expectNothingNew(t *testing.T, wantExists int, when string) {
	t.Helper()
	if err := f.ic.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	if n, other := f.ev.counts(); n != wantExists || len(other) != 0 {
		t.Errorf("%s: the client heard %d EXISTS (want %d) and %v", when, n, wantExists, other)
	}
	st, err := f.ic.Status("Sent Items", &imap.StatusOptions{NumMessages: true, UIDNext: true}).Wait()
	if err != nil || *st.NumMessages != 1 || st.UIDNext != 2 {
		t.Errorf("%s: Sent status = %+v %v: the Sent copy was duplicated or a UID was used", when, st, err)
	}
}

func TestTheSentCopyShowsTheBccRecipientAndNothingNewIsAnnouncedWhenExchangesCopyArrives(t *testing.T) {
	f := newBccFlow(t, "bcc-flow-1@mailflow")
	f.e.Srv.HoldSentCopies(testenv.User, true)
	f.send(t, "bcc-flow-1@mailflow", "Bcc flow, APPEND first")
	if _, err := f.ic.Select("Sent Items", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	uid := f.appendClientCopy(t)
	if uid != 1 {
		t.Fatalf("APPENDUID %d", uid)
	}
	// MailFlow looks at the new message at once: the Bcc recipient is there.
	f.expectBcc(t, uid, "before Exchange's copy")
	if err := f.ic.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	existsBefore, _ := f.ev.counts()
	if existsBefore != 1 {
		t.Fatalf("%d EXISTS for the APPEND, want 1", existsBefore)
	}

	// Exchange's copy arrives without Bcc and binds to the same UID.
	f.e.Srv.ReleaseSentCopies(testenv.User)
	sentID := f.e.Srv.FolderID(testenv.User, "Sent Items")
	acct, _ := f.e.Mgr.Login(context.Background(), testenv.User, testenv.Pass)
	testenv.Eventually(t, "Exchange's copy is synced and bound", func() bool {
		fo, _ := acct.Store().FolderByPath("Sent Items")
		m, err := acct.Store().Message(fo.ID, 1)
		return err == nil && m.ServerID != "" && len(f.e.Srv.Items(testenv.User, sentID)) == 1
	})
	if items := f.e.Srv.Items(testenv.User, sentID); bytes.Contains(items[0].MIME, []byte(bccRcpt)) {
		t.Fatalf("setup: Exchange's copy should have no Bcc recipient")
	}
	time.Sleep(150 * time.Millisecond)
	f.expectNothingNew(t, 1, "after Exchange's copy arrived")
	f.expectBcc(t, uid, "after Exchange's copy arrived")
}

func TestTheSentCopyShowsTheBccRecipientWhenExchangesCopyComesFirst(t *testing.T) {
	f := newBccFlow(t, "bcc-flow-2@mailflow")
	if _, err := f.ic.Select("Sent Items", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	f.send(t, "bcc-flow-2@mailflow", "Bcc flow, Exchange first")
	acct, _ := f.e.Mgr.Login(context.Background(), testenv.User, testenv.Pass)
	testenv.Eventually(t, "Exchange's copy is synced", func() bool {
		fo, _ := acct.Store().FolderByPath("Sent Items")
		c, err := acct.Store().Counts(fo.ID)
		return err == nil && c.Messages == 1
	})
	// MailFlow hears about the new message and looks at it, before it APPENDs anything.
	testenv.Eventually(t, "the client hears EXISTS", func() bool {
		f.ic.Noop().Wait()
		n, _ := f.ev.counts()
		return n == 1
	})
	f.expectBcc(t, 1, "Exchange's copy, fetched before the APPEND")
	uid := f.appendClientCopy(t)
	if uid != 1 {
		t.Fatalf("APPENDUID %d, want the UID of Exchange's copy", uid)
	}
	f.expectBcc(t, uid, "after the APPEND")
	f.expectNothingNew(t, 1, "after the APPEND")
}

func TestAClientThatAppendsBeforeItSubmitsGetsTheBccRecipientWhenTheCopyBinds(t *testing.T) {
	// The placeholder was made from the client's copy, which has no Bcc header. When
	// Exchange's copy binds to it, it serves the submitted message. That is the one
	// case where a visible message changes its bytes: the UID is the same, nothing
	// is announced, and RFC822.SIZE follows.
	f := newBccFlow(t, "bcc-flow-3@mailflow")
	f.e.Srv.HoldSentCopies(testenv.User, true)
	f.client = message("bcc-flow-3@mailflow", "Bcc flow, backwards")
	if _, err := f.ic.Select("Sent Items", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	uid := f.appendClientCopy(t)
	before := f.fetch(t, uid)
	if before.RFC822Size != int64(len(f.client)) || bytes.Contains(before.FindBodySection(&imap.FetchItemBodySection{Peek: true}), []byte("\r\nBcc:")) {
		t.Fatalf("the placeholder before the submission: size %d", before.RFC822Size)
	}
	f.send(t, "bcc-flow-3@mailflow", "Bcc flow, backwards")
	existsBefore, _ := f.ev.counts()
	f.e.Srv.ReleaseSentCopies(testenv.User)
	acct, _ := f.e.Mgr.Login(context.Background(), testenv.User, testenv.Pass)
	testenv.Eventually(t, "Exchange's copy is bound", func() bool {
		fo, _ := acct.Store().FolderByPath("Sent Items")
		m, err := acct.Store().Message(fo.ID, 1)
		return err == nil && m.ServerID != ""
	})
	time.Sleep(150 * time.Millisecond)
	f.expectNothingNew(t, existsBefore, "after the swap")
	f.expectBcc(t, uid, "after the swap")
}
