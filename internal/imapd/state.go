package imapd

import (
	"log/slog"
	"sort"
	"sync"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"easbridge/internal/engine"
	"easbridge/internal/store"
)

// mailboxState is the shared in-memory listing of one folder: the UIDs and
// flags of its messages in UID order, plus the go-imap MailboxTracker that turns
// changes into per-session EXISTS/EXPUNGE/FETCH updates.
//
// It is fed by engine change events (delivered in commit order under the
// engine's lock) after being initialised from an atomic snapshot, so it always
// mirrors the database. Sessions never mutate it directly; they go through the
// engine and the resulting events come back here.
type mailboxState struct {
	h        *hub
	folderID int64
	log      *slog.Logger
	cancel   func()
	refs     int // guarded by hub.mu

	mu      sync.RWMutex
	ready   bool
	pending []store.Event // events that arrived before the snapshot was installed
	entries []store.MsgMeta
	uidNext uint32
	tracker *imapserver.MailboxTracker
	views   map[*session]*imapserver.SessionTracker
}

// hub holds the mailbox states of one account.
type hub struct {
	acct   *engine.Account
	log    *slog.Logger
	mu     sync.Mutex
	states map[int64]*mailboxState
}

func newHub(acct *engine.Account, log *slog.Logger) *hub {
	return &hub{acct: acct, log: log, states: map[int64]*mailboxState{}}
}

// acquire returns the shared state of a folder, creating it on first use.
func (h *hub) acquire(folderID int64, uidNext uint32) (*mailboxState, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if st := h.states[folderID]; st != nil {
		st.refs++
		return st, nil
	}
	st := &mailboxState{h: h, folderID: folderID, log: h.log, views: map[*session]*imapserver.SessionTracker{}}
	snap, cancel, err := h.acct.Watch(folderID, st.dispatch)
	if err != nil {
		return nil, err
	}
	st.cancel = cancel
	st.mu.Lock()
	st.entries = snap
	st.uidNext = uidNext
	if n := len(snap); n > 0 && snap[n-1].UID >= st.uidNext {
		st.uidNext = snap[n-1].UID + 1
	}
	st.tracker = imapserver.NewMailboxTracker(uint32(len(snap)))
	st.ready = true
	for _, ev := range st.pending {
		st.applyLocked(ev)
	}
	st.pending = nil
	st.mu.Unlock()
	st.refs = 1
	h.states[folderID] = st
	return st, nil
}

// release drops one reference; the last one stops listening.
func (h *hub) release(st *mailboxState) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st.refs--
	if st.refs > 0 {
		return
	}
	delete(h.states, st.folderID)
	st.cancel()
}

// dispatch receives engine events. It runs with the engine lock held and must
// not call back into the engine.
func (st *mailboxState) dispatch(ev store.Event) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.ready {
		st.pending = append(st.pending, ev)
		return
	}
	st.applyLocked(ev)
}

func (st *mailboxState) indexOf(uid uint32) int {
	i := sort.Search(len(st.entries), func(i int) bool { return st.entries[i].UID >= uid })
	if i < len(st.entries) && st.entries[i].UID == uid {
		return i
	}
	return -1
}

func (st *mailboxState) applyLocked(ev store.Event) {
	switch ev.Kind {
	case store.EvAdd:
		if n := len(st.entries); n > 0 && ev.UID <= st.entries[n-1].UID {
			return // already part of the snapshot
		}
		st.entries = append(st.entries, store.MsgMeta{UID: ev.UID, Flags: ev.Flags, Keywords: ev.Keywords,
			InternalDate: ev.Date, Size: ev.Size, Kind: ev.MsgKind})
		if ev.UID >= st.uidNext {
			st.uidNext = ev.UID + 1
		}
		st.tracker.QueueNumMessages(uint32(len(st.entries)))
	case store.EvBulkAdd:
		// The folder finished its first sync: all of its messages appear at once
		// (UIDs ascending, above every UID listed so far). Sessions hear one
		// EXISTS with the new total, not one per message.
		added := false
		for _, m := range ev.Bulk {
			if n := len(st.entries); n > 0 && m.UID <= st.entries[n-1].UID {
				continue // already part of the snapshot
			}
			st.entries = append(st.entries, m)
			added = true
		}
		if !added {
			return
		}
		if last := st.entries[len(st.entries)-1].UID; last >= st.uidNext {
			st.uidNext = last + 1
		}
		st.tracker.QueueNumMessages(uint32(len(st.entries)))
	case store.EvResize:
		// The bytes of the message were replaced by another rendering of the same
		// message (the Sent copy the bridge submitted, with its Bcc header). The
		// UID, the flags and the position are the same, so no session is told
		// anything; only the size that RFC822.SIZE reports is corrected.
		if i := st.indexOf(ev.UID); i >= 0 {
			st.entries[i].Size = ev.Size
		}
	case store.EvExpunge:
		i := st.indexOf(ev.UID)
		if i < 0 {
			return
		}
		st.tracker.QueueExpunge(uint32(i + 1))
		st.entries = append(st.entries[:i], st.entries[i+1:]...)
	case store.EvFlags:
		i := st.indexOf(ev.UID)
		if i < 0 {
			return
		}
		st.entries[i].Flags, st.entries[i].Keywords = ev.Flags, ev.Keywords
		var src *imapserver.SessionTracker
		if s, ok := ev.Origin.(*session); ok {
			src = st.views[s]
		}
		st.tracker.QueueMessageFlags(uint32(i+1), imap.UID(ev.UID), imapFlags(ev.Flags, ev.Keywords), src)
	}
}

// view is one session's selected mailbox.
type view struct {
	s        *session
	st       *mailboxState
	tracker  *imapserver.SessionTracker
	folder   *store.Folder
	readOnly bool
}

// newView registers a session on the state. The returned view must be closed.
func (st *mailboxState) newView(s *session, f *store.Folder, readOnly bool) *view {
	st.mu.Lock()
	defer st.mu.Unlock()
	t := st.tracker.NewSession()
	st.views[s] = t
	return &view{s: s, st: st, tracker: t, folder: f, readOnly: readOnly}
}

// close releases the session tracker (always) and the shared state.
func (v *view) close() {
	v.st.mu.Lock()
	delete(v.st.views, v.s)
	v.st.mu.Unlock()
	v.tracker.Close()
	v.st.h.release(v.st)
}

// target is one message selected by a sequence set or UID set.
type target struct {
	meta store.MsgMeta
	seq  uint32 // sequence number in this session's view
}

// resolve expands a message set against this session's view. Sequence numbers
// are the client's (they lag behind the server until pending updates are
// delivered); "*" is the largest number the client knows. Messages the client
// has not been told about yet are not addressable.
func (v *view) resolve(set imap.NumSet) []target {
	st := v.st
	st.mu.RLock()
	defer st.mu.RUnlock()
	n := len(st.entries)
	if n == 0 {
		return nil
	}
	var idxs []int
	switch ns := set.(type) {
	case imap.SeqSet:
		var max uint32
		for i := n; i >= 1; i-- {
			if c := v.tracker.EncodeSeqNum(uint32(i)); c != 0 {
				max = c
				break
			}
		}
		for _, r := range ns {
			start, stop := r.Start, r.Stop
			if start == 0 {
				start = max
			}
			if stop == 0 || stop > max {
				stop = max
			}
			if start > stop {
				if r.Stop != 0 {
					continue // a static range beyond the end
				}
				start = stop // "n:*" with n beyond the end still names the last message
			}
			for c := start; c <= stop && c != 0; c++ {
				if s := v.tracker.DecodeSeqNum(c); s != 0 && int(s) <= n {
					idxs = append(idxs, int(s)-1)
				}
				if c == ^uint32(0) {
					break
				}
			}
		}
	case imap.UIDSet:
		maxUID := st.entries[n-1].UID
		for _, r := range ns {
			start, stop := uint32(r.Start), uint32(r.Stop)
			dynamic := start == 0 || stop == 0
			if start == 0 {
				start = maxUID
			}
			if stop == 0 {
				stop = maxUID
			}
			if start > stop && dynamic {
				start, stop = stop, start // RFC 3501: "n:*" always includes the last message
			}
			i := sort.Search(n, func(i int) bool { return st.entries[i].UID >= start })
			for ; i < n && st.entries[i].UID <= stop; i++ {
				idxs = append(idxs, i)
			}
		}
	default:
		return nil
	}
	sort.Ints(idxs)
	out := make([]target, 0, len(idxs))
	last := -1
	for _, i := range idxs {
		if i == last {
			continue
		}
		last = i
		seq := v.tracker.EncodeSeqNum(uint32(i + 1))
		if seq == 0 {
			continue
		}
		out = append(out, target{meta: st.entries[i], seq: seq})
	}
	return out
}

// snapshot returns a copy of the entries with their client sequence numbers.
func (v *view) snapshot() []target {
	st := v.st
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make([]target, 0, len(st.entries))
	for i, e := range st.entries {
		seq := v.tracker.EncodeSeqNum(uint32(i + 1))
		if seq == 0 {
			continue
		}
		out = append(out, target{meta: e, seq: seq})
	}
	return out
}

// seqOf returns the client sequence number of a UID (0 if unknown).
func (v *view) seqOf(uid uint32) uint32 {
	st := v.st
	st.mu.RLock()
	defer st.mu.RUnlock()
	i := st.indexOf(uid)
	if i < 0 {
		return 0
	}
	return v.tracker.EncodeSeqNum(uint32(i + 1))
}
