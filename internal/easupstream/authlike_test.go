package easupstream_test

import (
	"time"

	"easbridge/internal/easupstream"
	"errors"
	"net/http"
	"testing"

	"easbridge/internal/upstream"
)

// What the engine does with a failure depends on what kind it is (see engine/authlike_test.go): a 403, a redirect
// that is not followed and an HTML page in answer to an EAS request look like a gateway refusing a credential
// (upstream.ErrAuthLike); a 429 or a 5xx is the server being busy (ThrottleError); a time-out or a dropped
// connection got no answer (ErrTransient); a 404 and the rest stay plain ErrUnavailable.
func TestFailuresAreClassifiedForTheLockoutRules(t *testing.T) {
	f := newFixture(t)
	m := f.open("")
	try := func(h handler) error {
		f.fake.on("Sync", h)
		_, err := m.Sync(ctx, "5", "k1", upstream.SyncOptions{})
		return err
	}
	for _, code := range []int{http.StatusForbidden, http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect} {
		err := try(always(code, nil))
		if !errors.Is(err, upstream.ErrAuthLike) || !errors.Is(err, upstream.ErrUnavailable) || errors.Is(err, upstream.ErrAuthFailed) {
			t.Errorf("HTTP %d: %v, want an auth-like failure (also unavailable, not a 401)", code, err)
		}
	}
	f.fake.answerHTML("Sync", true)
	if _, err := m.Sync(ctx, "5", "k1", upstream.SyncOptions{}); !errors.Is(err, upstream.ErrAuthLike) {
		t.Errorf("an HTML page with status 200: %v", err)
	}
	f.fake.answerHTML("Sync", false)

	for _, code := range []int{http.StatusNotFound, http.StatusRequestTimeout} {
		err := try(always(code, nil))
		if !errors.Is(err, upstream.ErrUnavailable) || errors.Is(err, upstream.ErrAuthLike) || errors.Is(err, upstream.ErrTransient) {
			t.Errorf("HTTP %d: %v, want a plain unavailable", code, err)
		}
	}
	for _, code := range []int{429, 500, 502, 503, 504} {
		err := try(always(code, nil))
		var te *upstream.ThrottleError
		if !errors.As(err, &te) || errors.Is(err, upstream.ErrAuthLike) {
			t.Errorf("HTTP %d: %v, want a throttle error", code, err)
		}
	}
	if err := try(always(http.StatusUnauthorized, nil)); !errors.Is(err, upstream.ErrAuthFailed) || errors.Is(err, upstream.ErrAuthLike) {
		t.Errorf("HTTP 401: %v", err)
	}
}

// The same at the login: the verification (OPTIONS, then Provision) classifies the way the session does.
func TestAnHTMLLogonPageAtLoginIsAuthLike(t *testing.T) {
	f := newFixture(t)
	f.fake.answerHTML("Provision", true)
	m := f.mailbox("")
	if err := m.Open(ctx); !errors.Is(err, upstream.ErrAuthLike) {
		t.Fatalf("Open: %v", err)
	}
}

// No answer within the time the client waits is a time-out, which at a login costs only a short pause.
func TestATimeOutIsTransient(t *testing.T) {
	f := newFixture(t, func(c *easupstream.Config) {
		hc := *c.HTTPClient
		hc.Timeout = 150 * time.Millisecond
		c.HTTPClient = &hc
	})
	m := f.open("")
	f.fake.delay = 600 * time.Millisecond
	f.fake.on("Sync", always(0, syncResp("5", "k2", "1")))
	_, err := m.Sync(ctx, "5", "k1", upstream.SyncOptions{})
	if !errors.Is(err, upstream.ErrTransient) || !errors.Is(err, upstream.ErrUnavailable) || errors.Is(err, upstream.ErrAuthLike) {
		t.Errorf("a request that timed out: %v", err)
	}
}
