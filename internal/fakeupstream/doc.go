// Package fakeupstream is an in-memory, deterministic stand-in for Exchange. It
// implements upstream.Mailbox, supports injecting new mail, flag changes,
// deletions, moves, key resets, failures and remote-wipe requests, and is
// used by the engine tests and the imapflow compatibility harness.
//
// Everything except this file is compiled only with the devfake build tag, so
// the production binary cannot contain it.
package fakeupstream
