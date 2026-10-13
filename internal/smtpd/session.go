package smtpd

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"easbridge/internal/engine"
	"easbridge/internal/mimeutil"
)

type session struct {
	srv    *Server
	conn   *smtp.Conn
	log    *slog.Logger
	acct   *engine.Account
	detach func()

	from  string
	rcpts []string
}

var _ smtp.AuthSession = (*session)(nil)

// AuthMechanisms implements smtp.AuthSession. go-smtp offers AUTH only on TLS
// connections (AllowInsecureAuth is off).
func (s *session) AuthMechanisms() []string { return []string{sasl.Plain, sasl.Login} }

// Auth implements smtp.AuthSession. PLAIN and LOGIN end in the same credential
// check as IMAP: allow-list, verified fast path, lockout cache.
func (s *session) Auth(mech string) (sasl.Server, error) {
	switch mech {
	case sasl.Plain:
		return sasl.NewPlainServer(func(identity, username, password string) error {
			if identity != "" && !strings.EqualFold(identity, username) {
				return smtp.ErrAuthFailed
			}
			return s.login(username, password)
		}), nil
	case sasl.Login:
		return newLoginServer(s.login), nil
	}
	return nil, smtp.ErrAuthUnknownMechanism
}

func (s *session) login(username, password string) error {
	if s.acct != nil {
		return smtp.ErrAuthFailed
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	if s.conn != nil && s.conn.Conn() != nil {
		ctx = engine.WithSource(ctx, engine.SourceFromAddr(s.conn.Conn().RemoteAddr()))
	}
	acct, err := s.srv.mgr.Login(ctx, username, password)
	if err != nil {
		switch {
		case errors.Is(err, engine.ErrBadCredentials):
			s.log.Info("SMTP login refused")
			return errBadAuth
		case errors.Is(err, engine.ErrWiped):
			s.log.Warn("SMTP login refused: mailbox wiped")
			return smtpErr(454, smtp.EnhancedCode{4, 7, 0}, "The mailbox was wiped on Exchange's request; an administrator must re-enable access")
		default:
			s.log.Warn("SMTP login failed", "err", err)
			return errAuthTemp
		}
	}
	s.acct = acct
	s.detach = acct.Attach(func() { s.conn.Close() })
	return nil
}

func (s *session) needAuth() error {
	if s.acct == nil {
		return smtp.ErrAuthRequired
	}
	return nil
}

// Mail implements smtp.Session.
func (s *session) Mail(from string, opts *smtp.MailOptions) error {
	if err := s.needAuth(); err != nil {
		return err
	}
	s.from, s.rcpts = from, nil
	return nil
}

// Rcpt implements smtp.Session.
func (s *session) Rcpt(to string, opts *smtp.RcptOptions) error {
	if err := s.needAuth(); err != nil {
		return err
	}
	s.rcpts = append(s.rcpts, to)
	return nil
}

// Data implements smtp.Session: read the message, add Bcc-only recipients to the
// headers, hand it to Exchange.
func (s *session) Data(r io.Reader) error {
	if err := s.needAuth(); err != nil {
		return err
	}
	if len(s.rcpts) == 0 {
		return errNoRecipients
	}
	max := s.acct.Config().MaxMessageBytes
	raw, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return err // includes smtp.ErrDataTooLarge from the library
	}
	if int64(len(raw)) > max {
		return errTooLarge
	}
	raw, added := mimeutil.AddEnvelopeRecipients(raw, s.rcpts)
	if int64(len(raw)) > max {
		return errTooLarge
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := s.acct.SendMail(ctx, raw); err != nil {
		mapped := MapSendError(err)
		s.log.Warn("SendMail failed", "bytes", len(raw), "recipients", len(s.rcpts), "err", err)
		return mapped
	}
	s.log.Info("message submitted", "bytes", len(raw), "recipients", len(s.rcpts), "bcc_added", len(added))
	return nil
}

// Reset implements smtp.Session.
func (s *session) Reset() { s.from, s.rcpts = "", nil }

// Logout implements smtp.Session.
func (s *session) Logout() error {
	if s.detach != nil {
		s.detach()
		s.detach = nil
	}
	return nil
}
