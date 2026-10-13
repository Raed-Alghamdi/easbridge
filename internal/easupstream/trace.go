package easupstream

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"time"

	"github.com/hstern/go-activesync/wbxml"
)

// requestTrace records the HTTP requests `easbridge probe` makes: which command, the
// HTTP status and where the time went. It keeps no URL, header, body or error text,
// so nothing that identifies the user or mail can end up in the report.
//
// It exists to answer one question the first real probe raised: of a slow step,
// how much is the bridge (more requests than needed), how much is getting a
// connection (DNS, TCP, TLS) and how much is the server or a gateway in front of it.
type requestTrace struct {
	mu   sync.Mutex
	list []*tracedRequest
	// messages: also keep the decoded request and response of every call
	// (`easbridge probe --trace`), redacted by renderXML.
	messages bool
	reg      *wbxml.Registry
}

type tracedRequest struct {
	Method string
	Cmd    string // the EAS command of a POST, "" for OPTIONS
	Status int    // HTTP status, 0 if the request failed before an answer
	// Connect is the time to get a connection: DNS, TCP and TLS for a new one, next
	// to nothing for a reused one. Wait runs from the end of the request to the first
	// byte of the answer: the server, and any gateway in front of it. Total is
	// everything until the response headers arrived.
	Connect, Wait, Total time.Duration
	Reused               bool
	// Detail holds the decoded request and response, line by line, when messages are
	// kept.
	Detail []string
}

func (t tracedRequest) String() string {
	what := t.Method
	if t.Cmd != "" {
		what += " " + t.Cmd
	}
	if t.Status == 0 {
		return fmt.Sprintf("%s -> no answer after %s", what, rounded(t.Total))
	}
	conn := "new connection " + rounded(t.Connect)
	if t.Reused {
		conn = "connection reused"
	}
	return fmt.Sprintf("%s -> HTTP %d in %s (%s, answer after %s)", what, t.Status, rounded(t.Total), conn, rounded(t.Wait))
}

// rounded prints a duration with a resolution that suits its size.
func rounded(d time.Duration) string {
	switch {
	case d >= 10*time.Second:
		return d.Round(100 * time.Millisecond).String()
	case d >= time.Second:
		return d.Round(10 * time.Millisecond).String()
	}
	return d.Round(time.Millisecond).String()
}

func (r *requestTrace) add(t *tracedRequest) {
	r.mu.Lock()
	r.list = append(r.list, t)
	r.mu.Unlock()
}

// mark returns a position; since returns what was recorded after it.
func (r *requestTrace) mark() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.list)
}

func (r *requestTrace) since(mark int) []tracedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]tracedRequest, 0, len(r.list)-mark)
	for _, t := range r.list[mark:] {
		c := *t
		c.Detail = append([]string(nil), t.Detail...)
		out = append(out, c)
	}
	return out
}

// tracingTransport records every round trip of base in rec.
type tracingTransport struct {
	base http.RoundTripper
	rec  *requestTrace
}

func (t *tracingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var mu sync.Mutex // the hooks run on the transport's goroutines
	var gotConn, wrote, first time.Time
	reused := false
	start := time.Now()
	hooks := &httptrace.ClientTrace{
		GotConn: func(i httptrace.GotConnInfo) {
			mu.Lock()
			gotConn, reused = time.Now(), i.Reused
			mu.Unlock()
		},
		WroteRequest: func(httptrace.WroteRequestInfo) {
			mu.Lock()
			wrote = time.Now()
			mu.Unlock()
		},
		GotFirstResponseByte: func() {
			mu.Lock()
			first = time.Now()
			mu.Unlock()
		},
	}
	var requestDetail []string
	if t.rec.messages {
		requestDetail = describeRequest(req, t.rec.reg)
	}
	resp, err := t.base.RoundTrip(req.WithContext(httptrace.WithClientTrace(req.Context(), hooks)))
	end := time.Now()

	mu.Lock()
	defer mu.Unlock()
	tr := tracedRequest{Method: req.Method, Cmd: req.URL.Query().Get("Cmd"), Total: end.Sub(start), Reused: reused}
	if !gotConn.IsZero() {
		tr.Connect = gotConn.Sub(start)
	}
	if !first.IsZero() {
		from := wrote
		if from.IsZero() {
			from = gotConn
		}
		if !from.IsZero() && first.After(from) {
			tr.Wait = first.Sub(from)
		}
	}
	if resp != nil {
		tr.Status = resp.StatusCode
	}
	tr.Detail = requestDetail
	entry := &tr
	t.rec.add(entry)
	if t.rec.messages && resp != nil {
		// The body is read by the caller; the response is described when it is closed.
		resp.Body = &captureBody{rc: resp.Body, max: maxCapture, done: func(b []byte, total int) {
			lines := describeResponse(resp, b, total, t.rec.reg)
			t.rec.mu.Lock()
			entry.Detail = append(entry.Detail, lines...)
			t.rec.mu.Unlock()
		}}
	}
	return resp, err
}

// maxCapture bounds what is kept of a message to describe it.
const maxCapture = 4 << 20

// maxHeaderShown bounds a header value in a trace; the list of commands a server
// supports is the longest of them.
const maxHeaderShown = 1000

// captureBody keeps the first max bytes that pass through it and tells done, once,
// when the body is closed.
type captureBody struct {
	rc    io.ReadCloser
	max   int
	buf   bytes.Buffer
	total int
	once  sync.Once
	done  func(captured []byte, total int)
}

func (c *captureBody) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	if n > 0 {
		c.total += n
		if room := c.max - c.buf.Len(); room > 0 {
			if n < room {
				room = n
			}
			c.buf.Write(p[:room])
		}
	}
	return n, err
}

func (c *captureBody) Close() error {
	err := c.rc.Close()
	c.once.Do(func() { c.done(c.buf.Bytes(), c.total) })
	return err
}

// describeRequest renders what the client sent: the two headers that matter for the
// protocol (never Authorization, Cookie or the URL, which holds the user name) and
// the decoded, redacted message.
func describeRequest(req *http.Request, reg *wbxml.Registry) []string {
	head := "request  " + req.Method
	var parts []string
	for _, h := range []string{"MS-ASProtocolVersion", "X-MS-PolicyKey"} {
		if v := req.Header.Get(h); v != "" {
			parts = append(parts, h+"="+cleanN(v, maxHeaderShown))
		}
	}
	if len(parts) > 0 {
		head += " " + strings.Join(parts, " ")
	}
	if req.GetBody == nil || req.Method == http.MethodOptions {
		return []string{head}
	}
	rc, err := req.GetBody()
	if err != nil {
		return []string{head}
	}
	defer rc.Close()
	body, _ := io.ReadAll(io.LimitReader(rc, maxCapture+1))
	return describeMessage(head, req.Header.Get("Content-Encoding"), body, len(body) > maxCapture, len(body), reg)
}

// describeResponse renders what the server answered. For OPTIONS the headers that
// list what the server speaks are the answer.
func describeResponse(resp *http.Response, body []byte, total int, reg *wbxml.Registry) []string {
	head := fmt.Sprintf("response HTTP %d", resp.StatusCode)
	var parts []string
	for _, h := range []string{"MS-Server-ActiveSync", "MS-ASProtocolVersions", "MS-ASProtocolCommands", "Retry-After", "Content-Type"} {
		if v := resp.Header.Get(h); v != "" {
			parts = append(parts, h+"="+cleanN(v, maxHeaderShown))
		}
	}
	if len(parts) > 0 {
		head += " " + strings.Join(parts, " ")
	}
	if total == 0 && resp.Request != nil && resp.Request.Method == http.MethodOptions {
		return []string{head} // the answer to OPTIONS is in its headers
	}
	return describeMessage(head, resp.Header.Get("Content-Encoding"), body, total > len(body), total, reg)
}

// describeMessage appends the redacted XML of a WBXML body to a heading line. A body
// that is not WBXML (an HTML page of a gateway, say) is described by its size alone:
// its text can hold names and session data.
func describeMessage(head, encoding string, body []byte, tooBig bool, total int, reg *wbxml.Registry) []string {
	lines := []string{head}
	switch {
	case total == 0:
		return append(lines, "  (empty body)")
	case tooBig:
		return append(lines, fmt.Sprintf("  (body of %d bytes, too large to show)", total))
	}
	raw := body
	if strings.EqualFold(encoding, "gzip") {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err == nil {
			raw, err = io.ReadAll(io.LimitReader(zr, maxCapture+1))
		}
		if err != nil || len(raw) > maxCapture {
			return append(lines, fmt.Sprintf("  (gzip body of %d bytes, not readable)", total))
		}
	}
	doc, err := wbxml.Unmarshal(raw, reg)
	if err != nil || doc.Root == nil {
		return append(lines, fmt.Sprintf("  (%d bytes, not a WBXML message)", total))
	}
	for _, ln := range strings.Split(strings.TrimRight(renderXML(doc.Root, reg), "\n"), "\n") {
		lines = append(lines, "  "+ln)
	}
	return lines
}
