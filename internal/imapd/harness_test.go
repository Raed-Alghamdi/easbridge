//go:build devfake

package imapd_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"

	"easbridge/internal/engine"
	"easbridge/internal/fakeupstream"
	"easbridge/internal/imapd"
	"easbridge/internal/testenv"
	"easbridge/internal/testpki"
)

const inlineMax = 4096

type ienv struct {
	*testenv.Env
	pki  *testpki.PKI
	srv  *imapd.Server
	addr string
}

func newIMAP(t *testing.T, mods ...func(*engine.Config, *fakeupstream.Server)) *ienv {
	t.Helper()
	te := testenv.New(t, inlineMax, mods...)
	pki, err := testpki.New()
	if err != nil {
		t.Fatal(err)
	}
	s := imapd.NewServer(te.Mgr, te.Log)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", pki.ServerConfig())
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(ln)
	t.Cleanup(func() { s.Close() })
	return &ienv{Env: te, pki: pki, srv: s, addr: ln.Addr().String()}
}

// dial opens a TLS connection with the go-imap client; handler may be nil.
func (e *ienv) dial(handler *imapclient.UnilateralDataHandler) *imapclient.Client {
	e.T.Helper()
	c, err := imapclient.DialTLS(e.addr, &imapclient.Options{
		TLSConfig:             e.pki.ClientConfig("easbridge"),
		UnilateralDataHandler: handler,
	})
	if err != nil {
		e.T.Fatalf("dial: %v", err)
	}
	e.T.Cleanup(func() { c.Close() })
	return c
}

func (e *ienv) login(handler *imapclient.UnilateralDataHandler) *imapclient.Client {
	e.T.Helper()
	c := e.dial(handler)
	if err := c.Login(testenv.User, testenv.Pass).Wait(); err != nil {
		e.T.Fatalf("login: %v", err)
	}
	return c
}

// seedInbox puts n messages into the fake Inbox and returns the ServerIds.
func (e *ienv) seedInbox(n int) []string {
	inbox := e.Srv.FolderID(testenv.User, "Inbox")
	var ids []string
	for i := 0; i < n; i++ {
		d := e.Clock.Now().Add(-time.Duration(n-i) * time.Hour)
		ids = append(ids, e.Srv.AddMessage(testenv.User, inbox,
			testenv.Mail(fmt.Sprintf("seed%d@x", i), fmt.Sprintf("seed %d", i), d, ""),
			fakeupstream.AddOptions{Date: d, Read: i%2 == 0}))
	}
	return ids
}

// account returns the engine behind the test user (logging in if needed).
func (e *ienv) account() *engine.Account {
	e.T.Helper()
	a, err := e.Mgr.Login(context.Background(), testenv.User, testenv.Pass)
	if err != nil {
		e.T.Fatalf("manager login: %v", err)
	}
	return a
}

// ready waits until the local copy of a folder mirrors the fake Exchange.
func (e *ienv) ready(serverName, path string) {
	e.T.Helper()
	e.Mirrored(e.account(), serverName, path)
}

// raw is a minimal IMAP wire client for byte-exact checks.
type raw struct {
	t   *testing.T
	c   net.Conn
	r   *bufio.Reader
	tag atomic.Int32
}

func (e *ienv) rawDial() *raw {
	e.T.Helper()
	c, err := tls.Dial("tcp", e.addr, e.pki.ClientConfig("easbridge"))
	if err != nil {
		e.T.Fatal(err)
	}
	e.T.Cleanup(func() { c.Close() })
	r := &raw{t: e.T, c: c, r: bufio.NewReader(c)}
	r.line() // greeting
	return r
}

func (r *raw) line() string {
	r.t.Helper()
	r.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	s, err := r.r.ReadString('\n')
	if err != nil {
		r.t.Fatalf("read: %v (got %q)", err, s)
	}
	return strings.TrimRight(s, "\r\n")
}

// cmd sends a tagged command and returns all response lines up to and
// including the tagged status line. Literals are inlined as "{N}<bytes>".
func (r *raw) cmd(format string, a ...any) []string {
	r.t.Helper()
	tag := fmt.Sprintf("t%d", r.tag.Add(1))
	r.send(tag + " " + fmt.Sprintf(format, a...))
	return r.until(tag)
}

func (r *raw) send(line string) {
	r.t.Helper()
	r.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(r.c, line+"\r\n"); err != nil {
		r.t.Fatal(err)
	}
}

func (r *raw) until(tag string) []string {
	r.t.Helper()
	var out []string
	for {
		l := r.line()
		for strings.HasSuffix(l, "}") && strings.Contains(l, "{") {
			i := strings.LastIndex(l, "{")
			n, err := strconv.Atoi(l[i+1 : len(l)-1])
			if err != nil {
				break
			}
			buf := make([]byte, n)
			r.c.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := io.ReadFull(r.r, buf); err != nil {
				r.t.Fatal(err)
			}
			l = l + string(buf) + r.line()
		}
		out = append(out, l)
		if strings.HasPrefix(l, tag+" ") {
			return out
		}
	}
}

func status(lines []string) string { return lines[len(lines)-1] }

func (r *raw) ok(format string, a ...any) []string {
	r.t.Helper()
	lines := r.cmd(format, a...)
	if !strings.Contains(strings.SplitN(status(lines), " ", 3)[1], "OK") {
		r.t.Fatalf("%q failed: %v", fmt.Sprintf(format, a...), lines)
	}
	return lines
}

func (r *raw) loginOK() {
	r.t.Helper()
	r.ok(`LOGIN "%s" "%s"`, "alice@example.test", "pw-one-correct-horse-battery")
}

func joinLines(lines []string) string { return strings.Join(lines, "\n") }

func capsOf(c *imapclient.Client) []string {
	var out []string
	for k := range c.Caps() {
		out = append(out, string(k))
	}
	return out
}

func containsAll(have []string, want ...string) bool {
	set := map[string]bool{}
	for _, h := range have {
		set[h] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}
