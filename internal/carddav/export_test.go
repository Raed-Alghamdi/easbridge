package carddav

import (
	"context"
	"net/http"
)

// HTTPServerForTest exposes the http.Server for the test of its limits.
func HTTPServerForTest(s *Server) *http.Server { return s.srv }

// LoginForTest returns the login function the server was built with.
func (s *Server) LoginForTest() func(ctx context.Context, user, pass string) (string, error) {
	return s.o.Login
}
