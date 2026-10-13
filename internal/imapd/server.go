// Package imapd is the IMAP front end: a go-imap server whose backend is the
// local state database and the engine.
//
// Advertised capabilities (DESIGN sections 9 and 12): IMAP4rev1, UIDPLUS, MOVE,
// SPECIAL-USE, NAMESPACE, LITERAL+ and CHILDREN, AUTH=PLAIN (TLS only), plus the
// ones go-imap adds on its own for IMAP4rev1 (IDLE, UNSELECT, ENABLE,
// UTF8=ACCEPT, SASL-IR, LITERAL-). Not advertised: ID, CONDSTORE, QRESYNC,
// COMPRESS, IMAP4rev2, LIST-STATUS, ESEARCH, SORT, THREAD, STARTTLS.
package imapd

import (
	"crypto/tls"
	"log/slog"
	"net"
	"sync"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"easbridge/internal/engine"
	"easbridge/internal/logx"
)

// Server is the IMAP listener and session factory.
type Server struct {
	mgr  *engine.Manager
	log  *slog.Logger
	imap *imapserver.Server

	mu   sync.Mutex
	hubs map[*engine.Account]*hub
}

// Caps is the exact capability set handed to go-imap.
func Caps() imap.CapSet {
	return imap.CapSet{
		imap.CapIMAP4rev1:   {},
		imap.CapUIDPlus:     {},
		imap.CapMove:        {},
		imap.CapSpecialUse:  {},
		imap.CapNamespace:   {},
		imap.CapLiteralPlus: {},
		imap.CapChildren:    {},
	}
}

// NewServer builds the IMAP server. Sessions authenticate through mgr.
func NewServer(mgr *engine.Manager, log *slog.Logger) *Server {
	s := &Server{mgr: mgr, log: log.With("component", "imap"), hubs: map[*engine.Account]*hub{}}
	s.imap = imapserver.New(&imapserver.Options{
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &session{srv: s, conn: conn, log: s.log}, nil, nil
		},
		Caps: Caps(),
		// Never DebugWriter: it would carry credentials. Never TLSConfig either:
		// the listener already speaks TLS (implicit), so STARTTLS has no place.
		Logger: logx.Adapter(log, "imap", slog.LevelWarn),
	})
	return s
}

// hubFor returns the mailbox-state hub of an account's engine.
func (s *Server) hubFor(a *engine.Account) *hub {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.hubs[a]
	if h == nil {
		// Forget hubs of engines that have been stopped.
		for acct := range s.hubs {
			if acct.Stopped() {
				delete(s.hubs, acct)
			}
		}
		h = newHub(a, s.log)
		s.hubs[a] = h
	}
	return h
}

// Serve accepts connections on ln, which must already be a TLS listener.
func (s *Server) Serve(ln net.Listener) error { return s.imap.Serve(ln) }

// ListenAndServeTLS listens with implicit TLS (port 993 style).
func (s *Server) ListenAndServeTLS(addr string, cfg *tls.Config) error {
	ln, err := tls.Listen("tcp", addr, cfg)
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

// Close stops accepting and closes all connections.
func (s *Server) Close() error { return s.imap.Close() }
