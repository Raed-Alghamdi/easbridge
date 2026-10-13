//go:build devfake

package main

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"easbridge/internal/config"
	"easbridge/internal/engine"
	"easbridge/internal/fakeupstream"
	"easbridge/internal/upstream"
)

// This file exists only in devfake builds (tests and the imapflow harness). The
// fake Exchange gets one mailbox per allowed user; the password comes from
// EASBRIDGE_FAKE_PASSWORD, and a loopback control API (EASBRIDGE_FAKE_CONTROL_ADDR)
// lets the harness inject mail and failures.
func newFakeFactory(cfg *config.Config, log *slog.Logger) (upstream.Factory, func(), error) {
	pw := os.Getenv("EASBRIDGE_FAKE_PASSWORD")
	if pw == "" {
		return nil, nil, errors.New("EASBRIDGE_FAKE_PASSWORD is required with the fake upstream")
	}
	srv := fakeupstream.NewServer()
	for _, u := range cfg.AllowedUsers {
		if strings.HasPrefix(u, "@") {
			continue // a domain entry: the fake mailbox of a person is created for a named login
		}
		srv.AddUser(u, pw)
	}
	closeFn := func() {}
	if cfg.FakeControlAddr != "" {
		ln, err := net.Listen("tcp", cfg.FakeControlAddr)
		if err != nil {
			return nil, nil, err
		}
		hs := &http.Server{Handler: srv.ControlHandler(), ReadHeaderTimeout: 5 * time.Second}
		go hs.Serve(ln)
		closeFn = func() { hs.Close() }
		log.Warn("FAKE UPSTREAM: no real Exchange is used; control API listening", "addr", ln.Addr().String())
	}
	return srv.Factory(cfg.MIMEInlineMax), closeFn, nil
}

// tuneEngineForTests lets the compatibility harness shorten the two timers that the
// real configuration bounds from below (a contacts sync every 5 minutes at least, a GAL
// search every 20 seconds at least), so that a check does not wait minutes. Only devfake
// binaries read these variables.
func tuneEngineForTests(ec *engine.Config, log *slog.Logger) {
	if v := os.Getenv("EASBRIDGE_FAKE_CONTACTS_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			ec.ContactsInterval = d
			log.Warn("FAKE UPSTREAM: the contacts interval is shortened for tests", "interval", d.String())
		}
	}
	if v := os.Getenv("EASBRIDGE_FAKE_SEARCH_SPACING"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			ec.Directory.Spacing = d
			log.Warn("FAKE UPSTREAM: the search spacing is shortened for tests", "spacing", d.String())
		}
	}
}
