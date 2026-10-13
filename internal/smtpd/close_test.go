package smtpd

import (
	"net"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
)

// What serve relies on, pinned at the library: go-smtp's Serve, called after Close, registers the
// listener and accepts until somebody closes the listener (its done channel is already closed, but
// only an Accept error makes it look). A stop that arrives before the SMTP goroutine reaches Serve
// therefore hangs unless the listener is closed by the caller too (cmd/easbridge/serve.go does, and
// TestAStopAtAnyStepOfTheStartEndsServeWithEveryListenerClosed holds it to that).
func TestServeAfterCloseEndsOnlyWhenTheListenerIsClosed(t *testing.T) {
	srv := smtp.NewServer(smtp.BackendFunc(func(*smtp.Conn) (smtp.Session, error) { return nil, nil }))
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	select {
	case err := <-done:
		t.Skipf("go-smtp now returns from Serve after Close (%v): the explicit listener close in serve is then belt and braces", err)
	case <-time.After(300 * time.Millisecond):
	}
	ln.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after the listener was closed")
	}
}
