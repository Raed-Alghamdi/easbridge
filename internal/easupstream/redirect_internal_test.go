package easupstream

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestRedirectPolicy(t *testing.T) {
	orig := &http.Request{URL: mustURL(t, "https://mail.example.test/Microsoft-Server-ActiveSync?Cmd=Sync&User=alice%40example.test")}
	via := []*http.Request{orig}
	many := make([]*http.Request, maxRedirects+1)
	for i := range many {
		many[i] = orig
	}
	for _, tc := range []struct {
		name   string
		to     string
		via    []*http.Request
		follow bool
	}{
		{"same host, another path", "https://mail.example.test/my.policy", via, true},
		{"same host, upper case", "https://MAIL.example.test/owa/", via, true},
		{"explicit default port", "https://mail.example.test:443/owa/", via, true},

		{"downgrade to http on the same host", "http://mail.example.test/Microsoft-Server-ActiveSync", via, false},
		{"downgrade to http on the same host and port", "http://mail.example.test:443/x", via, false},
		{"another host", "https://evil.example.test/Microsoft-Server-ActiveSync", via, false},
		{"a subdomain is another host", "https://login.mail.example.test/", via, false},
		{"another port", "https://mail.example.test:8443/", via, false},
		{"another scheme", "ftp://mail.example.test/", via, false},
		{"too many", "https://mail.example.test/loop", many, false},
		{"no request chain", "https://mail.example.test/x", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			policy := redirectPolicy(slog.New(slog.NewTextHandler(&logs, nil)))
			next := &http.Request{URL: mustURL(t, tc.to), Response: &http.Response{StatusCode: 302}}
			err := policy(next, tc.via)
			switch {
			case tc.follow && err != nil:
				t.Fatalf("refused: %v", err)
			case !tc.follow && !errors.Is(err, http.ErrUseLastResponse):
				t.Fatalf("followed (err = %v)", err)
			}
			// The log line carries the status and nothing of the Location, which
			// can hold a session token or a user name.
			out := logs.String()
			if tc.follow && out != "" {
				t.Errorf("a followed redirect logged: %s", out)
			}
			if !tc.follow && (!strings.Contains(out, "status=302") || strings.Contains(out, "example.test") || strings.Contains(out, "alice")) {
				t.Errorf("log line: %s", out)
			}
		})
	}
}
