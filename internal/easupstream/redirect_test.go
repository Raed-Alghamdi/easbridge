package easupstream_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"easbridge/internal/easupstream"
	"easbridge/internal/logx"
	"easbridge/internal/upstream"
)

// An Exchange front end (or a hijacked path to it) that redirects must not be
// able to take the Basic credentials to another host or onto a plain connection.
func TestARedirectToAnotherHostOrToHTTPNeverReceivesTheCredentials(t *testing.T) {
	var hits atomic.Int32
	var auth atomic.Value
	record := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		auth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	})
	otherTLS := httptest.NewTLSServer(record) // another host:port, https
	defer otherTLS.Close()
	plain := httptest.NewServer(record) // another host:port, plain http
	defer plain.Close()

	for _, tc := range []struct{ name, to string }{
		{"another https host", otherTLS.URL + "/Microsoft-Server-ActiveSync"},
		{"plain http", plain.URL + "/Microsoft-Server-ActiveSync"},
	} {
		for _, code := range []int{301, 302, 303, 307, 308} {
			t.Run(tc.name+" "+strconv.Itoa(code), func(t *testing.T) {
				front := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Location", tc.to+"?token=secret-session-token")
					w.WriteHeader(code)
				}))
				defer front.Close()
				var logs bytes.Buffer
				fac, err := easupstream.NewFactory(easupstream.Config{
					URL: front.URL + "/Microsoft-Server-ActiveSync", HTTPClient: front.Client(), Version: "16.1",
					Logger: logx.New(logx.Options{Level: slog.LevelDebug, Writer: &syncWriter{w: &logs}}),
				})
				if err != nil {
					t.Fatal(err)
				}
				mb, err := fac.New(upstream.Config{User: user, Password: pass, DeviceID: dev})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				err = mb.Open(ctx)
				if err == nil || !errors.Is(err, upstream.ErrUnavailable) || !strings.Contains(err.Error(), "HTTP "+strconv.Itoa(code)) {
					t.Fatalf("Open = %v, want an unavailable error that names the HTTP status", err)
				}
				if n := hits.Load(); n != 0 {
					t.Fatalf("the redirect target received %d request(s), Authorization %q", n, auth.Load())
				}
				// Nothing of the Location (a token, a host name) in the error or the log.
				if strings.Contains(err.Error(), "secret-session-token") || strings.Contains(logs.String(), "secret-session-token") {
					t.Errorf("the Location leaked:\nerror: %v\nlog: %s", err, logs.String())
				}
				if !strings.Contains(logs.String(), "not followed") {
					t.Errorf("the refusal was not logged: %s", logs.String())
				}
			})
		}
	}
}

// A redirect that stays on the same https host (a gateway's own path, say) is
// followed, and the credentials go with it only to that host.
func TestARedirectWithinTheSameHTTPSHostIsFollowed(t *testing.T) {
	f := newFixture(t)
	f.fake.redirectOptions = "/Microsoft-Server-ActiveSync/real"
	f.open("") // Open succeeds: OPTIONS went through the redirect
	var first, second *req
	for _, r := range f.fake.all() {
		r := r
		if r.Method != http.MethodOptions {
			continue
		}
		if first == nil {
			first = &r
		} else if second == nil {
			second = &r
		}
	}
	if first == nil || second == nil || first.Path == second.Path || second.Path != "/Microsoft-Server-ActiveSync/real" {
		t.Fatalf("OPTIONS requests: %+v and %+v", first, second)
	}
	if first.Auth == "" || second.Auth != first.Auth {
		t.Errorf("the credentials must go to the same host's redirect target: %q then %q", first.Auth, second.Auth)
	}
}
