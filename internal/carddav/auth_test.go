package carddav_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"easbridge/internal/engine"
)

func TestEveryRequestNeedsALoginExceptTheWellKnownRedirect(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct{ method, path string }{
		{"OPTIONS", "/"}, {"PROPFIND", "/"}, {"PROPFIND", "/dav/"}, {"PROPFIND", "/dav/principals/me/"}, {"PROPFIND", "/dav/addressbooks/me/"},
		{"REPORT", "/dav/addressbooks/me/directory/"}, {"GET", "/dav/addressbooks/me/directory/x.vcf"}, {"HEAD", "/dav/addressbooks/me/directory/x.vcf"},
		{"PUT", "/dav/addressbooks/me/directory/x.vcf"}, {"DELETE", "/dav/addressbooks/me/directory/x.vcf"}, {"GET", "/nothing"}, {"FOO", "/"},
	} {
		r := h.do(tc.method, tc.path, "", hdr("Authorization", "-"), hdr("Depth", "0"))
		if r.code != 401 {
			t.Errorf("%s %s without credentials: %d", tc.method, tc.path, r.code)
		}
		if got := r.header.Get("WWW-Authenticate"); got != `Basic realm="easbridge", charset="UTF-8"` {
			t.Errorf("%s %s: WWW-Authenticate = %q", tc.method, tc.path, got)
		}
	}
	if len(h.calls) != 0 {
		t.Errorf("the login was consulted %d times for requests without credentials", len(h.calls))
	}
	r := h.do("GET", "/.well-known/carddav", "", hdr("Authorization", "-"))
	if r.code != 301 || r.header.Get("Location") != "/dav/" {
		t.Errorf("well-known: %d %q", r.code, r.header.Get("Location"))
	}
	for _, m := range []string{"PROPFIND", "OPTIONS", "REPORT"} {
		if r := h.do(m, "/.well-known/carddav", "", hdr("Authorization", "-")); r.code != 301 {
			t.Errorf("%s well-known: %d", m, r.code)
		}
	}
}

func TestWrongAndMalformedCredentialsAre401(t *testing.T) {
	h := newHarness(t)
	for name, auth := range map[string]string{
		"wrong password":     basic(testUser, "nope"),
		"unknown user":       basic("mallory@example.test", testPass),
		"empty password":     basic(testUser, ""),
		"empty user":         basic("", testPass),
		"not basic":          "Bearer abc",
		"basic without data": "Basic ",
		"not base64":         "Basic !!!!",
		"no colon":           "Basic " + "bm9jb2xvbg==",
	} {
		r := h.do("PROPFIND", "/", "", hdr("Authorization", auth), hdr("Depth", "0"))
		if r.code != 401 || r.header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s: %d", name, r.code)
		}
		if strings.Contains(r.body, testPass) {
			t.Errorf("%s: the body repeats the password", name)
		}
	}
}

func TestTheLoginIsGivenWhatTheClientSent(t *testing.T) {
	h := newHarness(t)
	r := h.do("PROPFIND", "/", "", hdr("Authorization", "basic "+strings.TrimPrefix(basic(testUser, testPass), "Basic ")), hdr("Depth", "0"))
	if r.code != 207 {
		t.Fatalf("%d", r.code)
	}
	if len(h.calls) != 1 || h.calls[0] != [2]string{testUser, testPass} {
		t.Errorf("login calls: %q (the password has a colon and a non-ASCII letter and must arrive whole)", h.calls)
	}
}

func TestFailuresThatAreNotAWrongPasswordAreNever401(t *testing.T) {
	h := newHarness(t)
	until := time.Now().Add(90 * time.Second)
	for name, tc := range map[string]struct {
		err  error
		wait func(int) bool
	}{
		"throttled by the lockout budget": {&engine.ThrottledError{Until: until, Reason: "too many unsuccessful logins"}, func(n int) bool { return n >= 85 && n <= 95 }},
		"Exchange unavailable":            {fmt.Errorf("%w: down", engine.ErrUnavailable), func(n int) bool { return n >= 60 }},
		"wiped":                           {engine.ErrWiped, func(n int) bool { return n >= 60 }},
		"anything else":                   {errors.New("boom"), func(n int) bool { return n >= 60 }},
	} {
		h.login = func(context.Context, string, string) (string, error) { return "", tc.err }
		r := h.do("PROPFIND", "/", "", hdr("Depth", "0"))
		if r.code != 503 {
			t.Errorf("%s: %d", name, r.code)
			continue
		}
		n, err := strconv.Atoi(r.header.Get("Retry-After"))
		if err != nil || !tc.wait(n) {
			t.Errorf("%s: Retry-After %q", name, r.header.Get("Retry-After"))
		}
		if r.header.Get("WWW-Authenticate") != "" {
			t.Errorf("%s: a challenge was sent", name)
		}
	}
}

func TestTheSameLoginPathAsIMAPCountsFailuresTheSameWay(t *testing.T) {
	// The server hands every credential to the login function it is given, once per request
	// and before anything else, so the budget and the cooldown of engine.Manager.Login (shared
	// with IMAP and SMTP) apply to CardDAV requests exactly as they do to IMAP LOGINs.
	h := newHarness(t)
	for i := 0; i < 4; i++ {
		h.do("PROPFIND", "/", "", hdr("Authorization", basic(testUser, "wrong"+strconv.Itoa(i))), hdr("Depth", "0"))
	}
	h.do("PROPFIND", "/", "", hdr("Depth", "0"))
	if len(h.calls) != 5 {
		t.Fatalf("%d login calls for 5 requests", len(h.calls))
	}
	if h.calls[4] != [2]string{testUser, testPass} || h.calls[0][1] != "wrong0" {
		t.Errorf("calls %q", h.calls)
	}
}

func TestAnAuthenticatedUserOnlySeesHisOwnData(t *testing.T) {
	h := newHarness(t)
	h.seedBook("c1", "Contacts", 9, map[string]string{"a": "Alice Contact"})
	bob, _ := h.db.Account("bob@example.test")
	_ = bob
	h.login = func(_ context.Context, user, pass string) (string, error) {
		if pass == "bobpass" {
			return "bob@example.test", nil
		}
		return testUser, nil
	}
	r := h.do("PROPFIND", "/dav/addressbooks/me/", "", hdr("Depth", "1"), hdr("Authorization", basic("bob@example.test", "bobpass")))
	ms := parseMS(t, r.body)
	for _, href := range ms.hrefs() {
		if strings.Contains(href, "/contacts/") || href == "/dav/addressbooks/me/contacts/" {
			t.Errorf("bob sees alice's book: %v", ms.hrefs())
		}
	}
	// A card of alice's, asked for by bob, does not exist for him.
	alice := h.do("PROPFIND", "/dav/addressbooks/me/contacts/", "", hdr("Depth", "1"))
	am := parseMS(t, alice.body)
	if len(am.Responses) != 2 {
		t.Fatalf("alice: %v", am.hrefs())
	}
	cardHref := am.Responses[1].Href
	if r := h.do("GET", cardHref, "", hdr("Authorization", basic("bob@example.test", "bobpass"))); r.code != 404 {
		t.Errorf("bob reads alice's card: %d", r.code)
	}
	if r := h.do("GET", cardHref, ""); r.code != 200 {
		t.Errorf("alice cannot read her own card: %d", r.code)
	}
}
