// Package logx builds the structured logger used everywhere in easbridge.
//
// Privacy rules (DESIGN section 3 and 6): credentials never reach a log line,
// and neither do subjects, addresses or bodies. The code avoids logging them in
// the first place; this package is the safety net. It
//
//   - redacts attributes whose key names sensitive data (see sensitiveKeys),
//   - scrubs every registered secret (passwords, Basic auth blobs) from the
//     message, from string attributes and from error text,
//   - strips the User query parameter from URLs logged by go-activesync.
package logx

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
)

const redacted = "[redacted]"

// sensitiveKeys are attribute names whose value is never printed.
var sensitiveKeys = map[string]bool{
	"password": true, "passwd": true, "pass": true, "pwd": true, "secret": true, "token": true,
	"authorization": true, "auth": true, "cookie": true, "set-cookie": true,
	"subject": true, "from": true, "to": true, "cc": true, "bcc": true, "replyto": true,
	"reply-to": true, "sender": true, "body": true, "data": true, "mime": true, "rcpt": true,
	"user": true, "username": true, "login": true, "email": true, "address": true,
}

// secret registry --------------------------------------------------------

// The registry has two tiers. Recent secrets (every password a client
// presented, right or wrong) live in a FIFO of maxRecent entries so that a
// flood of bad logins cannot grow memory. Pinned secrets (credentials Exchange
// accepted) are never evicted by that flood.
type registry struct {
	mu     sync.RWMutex
	recent []string
	pinned []string
	all    []string // pinned + recent, longest first
}

var global = &registry{}

const (
	maxRecent = 256
	maxPinned = 64
)

func (r *registry) add(s string, pin bool) {
	if len(s) < 3 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	has := func(l []string) bool {
		for _, v := range l {
			if v == s {
				return true
			}
		}
		return false
	}
	switch {
	case pin:
		if !has(r.pinned) {
			if len(r.pinned) >= maxPinned {
				r.pinned = r.pinned[1:]
			}
			r.pinned = append(r.pinned, s)
		}
	case !has(r.recent) && !has(r.pinned):
		if len(r.recent) >= maxRecent {
			r.recent = r.recent[1:]
		}
		r.recent = append(r.recent, s)
	default:
		return
	}
	r.all = append(append(r.all[:0:0], r.pinned...), r.recent...)
	sort.SliceStable(r.all, func(i, j int) bool { return len(r.all[i]) > len(r.all[j]) })
}

// RegisterSecret makes Scrub (and every logger built here) remove s from
// output. Secrets live in process memory only.
func RegisterSecret(s string) { global.add(s, false) }

// PinSecret is RegisterSecret for a secret that must outlive a flood of
// other registrations (a credential Exchange accepted).
func PinSecret(s string) { global.add(s, true) }

func registerCredential(user, pass string, pin bool) {
	if pass == "" {
		return
	}
	for _, s := range []string{
		pass,
		url.QueryEscape(pass),
		base64.StdEncoding.EncodeToString([]byte(user + ":" + pass)),             // HTTP Basic
		base64.StdEncoding.EncodeToString([]byte("\x00" + user + "\x00" + pass)), // SASL PLAIN
		base64.StdEncoding.EncodeToString([]byte(pass)),
	} {
		global.add(s, pin)
	}
}

// RegisterCredential registers a password together with the forms it takes on
// the wire: the Basic authorization blob, the SASL PLAIN blob and the
// URL-escaped form.
func RegisterCredential(user, pass string) { registerCredential(user, pass, false) }

// PinCredential is RegisterCredential for a credential that Exchange accepted.
func PinCredential(user, pass string) { registerCredential(user, pass, true) }

var userParam = regexp.MustCompile(`(?i)([?&]User=)[^&\s"']+`)

// RedactURLs replaces the value of any User= query parameter in s (the mailbox
// name go-activesync puts in every request URL, and which Go's net/http errors
// print) with "-".
func RedactURLs(s string) string {
	if !strings.Contains(s, "ser=") && !strings.Contains(s, "SER=") {
		return s
	}
	return userParam.ReplaceAllString(s, "${1}-")
}

// Scrub replaces every registered secret in s with "***" and removes mailbox
// names from URLs.
func Scrub(s string) string {
	s = RedactURLs(s)
	global.mu.RLock()
	defer global.mu.RUnlock()
	for _, sec := range global.all {
		if strings.Contains(s, sec) {
			s = strings.ReplaceAll(s, sec, "***")
		}
	}
	return s
}

// ResetSecretsForTest clears the registry.
func ResetSecretsForTest() {
	global.mu.Lock()
	global.recent, global.pinned, global.all = nil, nil, nil
	global.mu.Unlock()
}

// stripUser removes the value of the User query parameter from a URL string.
func stripUser(s string) string {
	i := strings.IndexByte(s, '?')
	if i < 0 {
		return s
	}
	q, err := url.ParseQuery(s[i+1:])
	if err != nil || !q.Has("User") {
		return s
	}
	q.Set("User", "-")
	return s[:i] + "?" + q.Encode()
}

func replaceAttr(_ []string, a slog.Attr) slog.Attr {
	key := strings.ToLower(a.Key)
	if sensitiveKeys[key] {
		return slog.String(a.Key, redacted)
	}
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		s := Scrub(v.String())
		if key == "url" {
			s = stripUser(s)
		}
		a.Value = slog.StringValue(s)
	case slog.KindAny:
		switch x := v.Any().(type) {
		case error:
			a.Value = slog.StringValue(Scrub(x.Error()))
		case nil:
		default:
			a.Value = slog.StringValue(Scrub(fmt.Sprintf("%+v", x)))
		}
	}
	return a
}

// Options configure New.
type Options struct {
	Level  slog.Leveler
	Writer io.Writer
	JSON   bool
}

// New returns a logger with redaction and secret scrubbing.
func New(o Options) *slog.Logger {
	ho := &slog.HandlerOptions{Level: o.Level, ReplaceAttr: replaceAttr}
	var h slog.Handler
	if o.JSON {
		h = slog.NewJSONHandler(o.Writer, ho)
	} else {
		h = slog.NewTextHandler(o.Writer, ho)
	}
	return slog.New(h)
}

// Discard returns a logger that drops everything.
func Discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.Level(100)}))
}

// EAS wraps base for use as go-activesync's Config.Logger. Whatever the base
// handler is, URLs it logs lose the User query value (the mailbox name).
func EAS(base *slog.Logger) *slog.Logger {
	return slog.New(&urlHandler{inner: base.Handler()})
}

type urlHandler struct{ inner slog.Handler }

func (h *urlHandler) Enabled(ctx context.Context, l slog.Level) bool { return h.inner.Enabled(ctx, l) }
func (h *urlHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &urlHandler{inner: h.inner.WithAttrs(cleanAttrs(as))}
}
func (h *urlHandler) WithGroup(n string) slog.Handler {
	return &urlHandler{inner: h.inner.WithGroup(n)}
}
func (h *urlHandler) Handle(ctx context.Context, r slog.Record) error {
	nr := slog.NewRecord(r.Time, r.Level, Scrub(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(cleanAttr(a))
		return true
	})
	return h.inner.Handle(ctx, nr)
}

func cleanAttrs(as []slog.Attr) []slog.Attr {
	out := make([]slog.Attr, len(as))
	for i, a := range as {
		out[i] = cleanAttr(a)
	}
	return out
}

func cleanAttr(a slog.Attr) slog.Attr {
	if strings.EqualFold(a.Key, "url") && a.Value.Kind() == slog.KindString {
		return slog.String(a.Key, stripUser(a.Value.String()))
	}
	return a
}

// Printf adapters for libraries with their own Logger interfaces ------------

// Printf is the logger shape used by go-imap (Printf) and go-smtp (Printf and
// Println). Output goes to l at the given level.
type Printf struct {
	l     *slog.Logger
	level slog.Level
}

// Adapter returns a Printf adapter that logs at level, tagged with component.
func Adapter(l *slog.Logger, component string, level slog.Level) *Printf {
	return &Printf{l: l.With("component", component), level: level}
}

func (p *Printf) Printf(format string, args ...any) {
	p.l.Log(context.Background(), p.level, fmt.Sprintf(strings.TrimRight(format, "\n"), args...))
}

func (p *Printf) Println(args ...any) {
	p.l.Log(context.Background(), p.level, strings.TrimRight(fmt.Sprintln(args...), "\n"))
}
