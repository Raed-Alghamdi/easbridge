package imapd

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"easbridge/internal/engine"
	"easbridge/internal/store"
)

// session is one IMAP connection. go-imap runs a connection's commands one at a
// time on one goroutine; IDLE is the only place another goroutine appears (the
// library's, calling Idle), and it only touches the view's SessionTracker.
type session struct {
	srv    *Server
	conn   *imapserver.Conn
	log    *slog.Logger
	acct   *engine.Account
	hub    *hub
	detach func()
	view   *view
}

var (
	_ imapserver.Session          = (*session)(nil)
	_ imapserver.SessionMove      = (*session)(nil)
	_ imapserver.SessionNamespace = (*session)(nil)
)

func (s *session) needAuth() error {
	if s.acct == nil {
		return bad("Not authenticated")
	}
	return nil
}

func (s *session) needView() (*view, error) {
	if s.view == nil {
		return nil, bad("No mailbox selected")
	}
	return s.view, nil
}

func (s *session) closeView() {
	if s.view != nil {
		s.view.close()
		s.view = nil
	}
}

// Close implements imapserver.Session.
func (s *session) Close() error {
	s.closeView()
	if s.detach != nil {
		s.detach()
		s.detach = nil
	}
	return nil
}

// Login implements imapserver.Session. LOGIN and AUTHENTICATE PLAIN both end
// here (go-imap's built-in PLAIN calls Login). The manager decides whether the
// credential is accepted without bothering Exchange.
func (s *session) Login(username, password string) error {
	if s.acct != nil {
		return bad("Already authenticated")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	if nc := s.conn.NetConn(); nc != nil {
		ctx = engine.WithSource(ctx, engine.SourceFromAddr(nc.RemoteAddr()))
	}
	acct, err := s.srv.mgr.Login(ctx, username, password)
	if err != nil {
		if errors.Is(err, engine.ErrBadCredentials) {
			s.log.Info("IMAP login refused")
		} else {
			s.log.Warn("IMAP login failed", "err", err)
		}
		return mapError(s.log, err)
	}
	s.acct = acct
	s.hub = s.srv.hubFor(acct)
	s.detach = acct.Attach(func() { s.conn.Bye("Mailbox closed by the bridge") })
	// The very first login on a fresh database finds no folders yet: the engine
	// is still fetching the hierarchy. Wait for it (bounded) so that the client's
	// first LIST or SELECT does not see an account without an Inbox.
	if !acct.WaitHierarchy(ctx, hierarchyWait) {
		s.log.Warn("the folder list is not known yet; the client may see an empty account")
	}
	return nil
}

// hierarchyWait bounds how long the first login waits for the folder list.
const hierarchyWait = 15 * time.Second

// Select implements imapserver.Session.
func (s *session) Select(mailbox string, options *imap.SelectOptions) (*imap.SelectData, error) {
	if err := s.needAuth(); err != nil {
		return nil, err
	}
	f, err := s.acct.Store().FolderByPath(mailbox)
	if err != nil {
		return nil, no(imap.ResponseCodeNonExistent, "Mailbox does not exist")
	}
	s.closeView()
	st, err := s.hub.acquire(f.ID, f.UIDNext)
	if err != nil {
		return nil, mapError(s.log, err)
	}
	v := st.newView(s, f, options != nil && options.ReadOnly)
	s.view = v

	st.mu.RLock()
	defer st.mu.RUnlock()
	data := &imap.SelectData{
		Flags:          append([]imap.Flag(nil), permanentFlags...),
		PermanentFlags: append([]imap.Flag(nil), permanentFlags...),
		NumMessages:    uint32(len(st.entries)),
		UIDNext:        imap.UID(st.uidNext),
		UIDValidity:    f.UIDValidity,
	}
	for i, e := range st.entries {
		if !e.Flags.Has(store.FlagSeen) {
			data.FirstUnseenSeqNum = uint32(i + 1)
			break
		}
	}
	return data, nil
}

// Unselect implements imapserver.Session.
func (s *session) Unselect() error {
	s.closeView()
	return nil
}

// Poll implements imapserver.Session: it delivers queued EXISTS, EXPUNGE and
// FETCH FLAGS updates after a command.
func (s *session) Poll(w *imapserver.UpdateWriter, allowExpunge bool) error {
	if s.view == nil {
		return nil
	}
	return s.view.tracker.Poll(w, allowExpunge)
}

// Idle implements imapserver.Session. Any session can IDLE, in any folder, and
// all of them are woken by the engine's events.
func (s *session) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	v := s.view
	if v == nil {
		<-stop
		return nil
	}
	return v.tracker.Idle(w, stop)
}

// Subscribe and Unsubscribe are accepted but change nothing: every folder is
// always subscribed.
func (s *session) Subscribe(mailbox string) error   { return s.needAuth() }
func (s *session) Unsubscribe(mailbox string) error { return s.needAuth() }

// Create implements imapserver.Session (CREATE -> EAS FolderCreate).
func (s *session) Create(mailbox string, options *imap.CreateOptions) error {
	if err := s.needAuth(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return mapError(s.log, s.acct.CreateFolder(ctx, mailbox))
}

// Delete implements imapserver.Session.
func (s *session) Delete(mailbox string) error {
	if err := s.needAuth(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return mapError(s.log, s.acct.DeleteFolder(ctx, mailbox))
}

// Rename implements imapserver.Session.
func (s *session) Rename(mailbox, newName string, options *imap.RenameOptions) error {
	if err := s.needAuth(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return mapError(s.log, s.acct.RenameFolder(ctx, mailbox, newName))
}

// Copy implements imapserver.Session. EAS has no copy; MOVE is supported.
func (s *session) Copy(numSet imap.NumSet, dest string) (*imap.CopyData, error) {
	return nil, no(imap.ResponseCodeCannot, "COPY is not supported by this server (use MOVE)")
}

// Append implements imapserver.Session.
func (s *session) Append(mailbox string, r imap.LiteralReader, options *imap.AppendOptions) (*imap.AppendData, error) {
	if err := s.needAuth(); err != nil {
		return nil, err
	}
	max := s.acct.Config().MaxMessageBytes
	if r.Size() > max {
		return nil, no(imap.ResponseCodeTooBig, "Message too large")
	}
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	var flags []imap.Flag
	var when time.Time
	if options != nil {
		flags, when = options.Flags, options.Time
	}
	sys, kw := parseFlags(flags)
	res, err := s.acct.Append(mailbox, data, sys, joinKeywords(kw), when, s)
	if err != nil {
		if errors.Is(err, engine.ErrNoMailbox) {
			return nil, no(imap.ResponseCodeTryCreate, "Mailbox does not exist")
		}
		return nil, mapError(s.log, err)
	}
	return &imap.AppendData{UID: imap.UID(res.UID), UIDValidity: res.UIDValidity}, nil
}

func joinKeywords(kw []string) string {
	out := ""
	for i, k := range kw {
		if i > 0 {
			out += " "
		}
		out += k
	}
	return out
}

// Expunge implements imapserver.Session. The EXPUNGE responses reach this
// session like everyone else's, through its tracker, when the command ends.
func (s *session) Expunge(w *imapserver.ExpungeWriter, uids *imap.UIDSet) error {
	v, err := s.needView()
	if err != nil {
		return err
	}
	if v.readOnly {
		return no(imap.ResponseCodeNoPerm, "Mailbox is read-only")
	}
	var only func(uint32) bool
	if uids != nil {
		allowed := map[uint32]bool{}
		for _, t := range v.resolve(*uids) {
			allowed[t.meta.UID] = true
		}
		only = func(u uint32) bool { return allowed[u] }
	}
	_, err = s.acct.Expunge(v.folder.ID, only, s)
	return mapError(s.log, err)
}

// Store implements imapserver.Session.
func (s *session) Store(w *imapserver.FetchWriter, numSet imap.NumSet, flags *imap.StoreFlags, options *imap.StoreOptions) error {
	v, err := s.needView()
	if err != nil {
		return err
	}
	if v.readOnly {
		return no(imap.ResponseCodeNoPerm, "Mailbox is read-only")
	}
	targets := v.resolve(numSet)
	if len(targets) == 0 {
		return nil
	}
	uids := make([]uint32, len(targets))
	for i, t := range targets {
		uids[i] = t.meta.UID
	}
	var op engine.FlagOp
	switch flags.Op {
	case imap.StoreFlagsSet:
		op = engine.FlagReplace
	case imap.StoreFlagsAdd:
		op = engine.FlagAdd
	case imap.StoreFlagsDel:
		op = engine.FlagRemove
	default:
		return bad("Unknown STORE operation")
	}
	sys, kw := parseFlags(flags.Flags)
	updates, err := s.acct.SetFlags(v.folder.ID, uids, op, sys, kw, s)
	if err != nil {
		return mapError(s.log, err)
	}
	if flags.Silent {
		return nil
	}
	// Answer with the new flags. The originating session is excluded from the
	// tracker's FETCH update, so this is the only report it gets.
	for _, u := range updates {
		seq := v.seqOf(u.UID)
		if seq == 0 {
			continue
		}
		rw := w.CreateMessage(seq)
		rw.WriteUID(imap.UID(u.UID))
		rw.WriteFlags(imapFlags(u.Flags, u.Keywords))
		if err := rw.Close(); err != nil {
			return err
		}
	}
	return nil
}

// Move implements imapserver.SessionMove (RFC 6851): EAS MoveItems, then
// COPYUID, then the source EXPUNGEs (delivered through the tracker).
func (s *session) Move(w *imapserver.MoveWriter, numSet imap.NumSet, dest string) error {
	v, err := s.needView()
	if err != nil {
		return err
	}
	if v.readOnly {
		return no(imap.ResponseCodeNoPerm, "Mailbox is read-only")
	}
	df, err := s.acct.Store().FolderByPath(dest)
	if err != nil {
		return no(imap.ResponseCodeTryCreate, "Destination mailbox does not exist")
	}
	targets := v.resolve(numSet)
	if len(targets) == 0 {
		return nil
	}
	uids := make([]uint32, len(targets))
	for i, t := range targets {
		uids[i] = t.meta.UID
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := s.acct.Move(ctx, v.folder.ID, uids, df.ID, s)
	if err != nil {
		return mapError(s.log, err)
	}
	if len(out.Moved) > 0 {
		var src, dst imap.UIDSet
		for _, m := range out.Moved {
			src.AddNum(imap.UID(m.Src))
			dst.AddNum(imap.UID(m.Dst))
		}
		if err := w.WriteCopyData(&imap.CopyData{UIDValidity: out.DstUIDValidity, SourceUIDs: src, DestUIDs: dst}); err != nil {
			return err
		}
	}
	if len(out.Failed) > 0 {
		return no(imap.ResponseCodeUnavailable, "Some messages could not be moved")
	}
	return nil
}

// Namespace implements imapserver.SessionNamespace.
func (s *session) Namespace() (*imap.NamespaceData, error) {
	if err := s.needAuth(); err != nil {
		return nil, err
	}
	return &imap.NamespaceData{Personal: []imap.NamespaceDescriptor{{Prefix: "", Delim: '/'}}}, nil
}
