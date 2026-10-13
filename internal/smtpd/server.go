// Package smtpd is the SMTP submission front end (implicit TLS, AUTH PLAIN and
// LOGIN). A message accepted here goes to Exchange through EAS SendMail with
// SaveInSentItems; Bcc-only recipients are written into a Bcc header first
// because EAS takes its recipients from the headers.
package smtpd

import (
	"crypto/tls"
	"log/slog"
	"net"
	"time"

	"github.com/emersion/go-smtp"

	"easbridge/internal/engine"
	"easbridge/internal/logx"
)

// Server is the submission listener.
type Server struct {
	mgr  *engine.Manager
	log  *slog.Logger
	smtp *smtp.Server
}

// NewServer builds the SMTP server. maxBytes limits message size (the engine
// enforces the same limit); hostname is announced in the greeting.
func NewServer(mgr *engine.Manager, log *slog.Logger, maxBytes int64, hostname string) *Server {
	s := &Server{mgr: mgr, log: log.With("component", "smtp")}
	be := smtp.BackendFunc(func(c *smtp.Conn) (smtp.Session, error) {
		return &session{srv: s, conn: c, log: s.log}, nil
	})
	s.smtp = smtp.NewServer(be)
	s.smtp.Domain = hostname
	s.smtp.MaxMessageBytes = maxBytes
	s.smtp.MaxRecipients = 500
	s.smtp.ReadTimeout = 5 * time.Minute
	s.smtp.WriteTimeout = 5 * time.Minute
	s.smtp.AllowInsecureAuth = false // AUTH only inside TLS
	s.smtp.EnableSMTPUTF8 = true
	// Never set Debug: it would show AUTH exchanges.
	s.smtp.ErrorLog = logx.Adapter(log, "smtp", slog.LevelWarn)
	return s
}

// Serve accepts connections on ln, which must be a TLS listener.
func (s *Server) Serve(ln net.Listener) error { return s.smtp.Serve(ln) }

// ListenAndServeTLS listens with implicit TLS (port 465 style).
func (s *Server) ListenAndServeTLS(addr string, cfg *tls.Config) error {
	ln, err := tls.Listen("tcp", addr, cfg)
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

// Close stops the server and closes connections.
func (s *Server) Close() error { return s.smtp.Close() }
