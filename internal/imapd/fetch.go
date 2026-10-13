package imapd

import (
	"errors"
	"io"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"easbridge/internal/engine"
	"easbridge/internal/mimestruct"
	"easbridge/internal/store"
)

// Fetch implements imapserver.Session. Everything is served from the local
// database: FLAGS, INTERNALDATE and RFC822.SIZE from the in-memory listing,
// BODYSTRUCTURE and ENVELOPE from the structure index stored with the message
// (computed once, when the message was stored, with hard limits: see
// internal/mimestruct), the header block for BODY[HEADER...], and the cached
// complete MIME for the other body sections. Sections that do not exist are
// answered with an empty literal (go-imap cannot write NIL), never with an
// error.
func (s *session) Fetch(w *imapserver.FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) error {
	v, err := s.needView()
	if err != nil {
		return err
	}
	targets := v.resolve(numSet)
	if len(targets) == 0 {
		return nil
	}
	markSeen := false
	if !v.readOnly {
		for _, bs := range options.BodySection {
			if !bs.Peek {
				markSeen = true
			}
		}
	}
	need := fetchNeeds(options)

	for _, t := range targets {
		uid := t.meta.UID
		meta := t.meta
		opts := options
		if markSeen && !meta.Flags.Has(store.FlagSeen) {
			// A non-PEEK body fetch sets \Seen, and the response must say so.
			ups, err := s.acct.SetFlags(v.folder.ID, []uint32{uid}, engine.FlagAdd, store.FlagSeen, nil, s)
			if err != nil {
				return mapError(s.log, err)
			}
			if len(ups) == 1 {
				meta.Flags, meta.Keywords = ups[0].Flags, ups[0].Keywords
				o := *options
				o.Flags = true
				opts = &o
			}
		}
		d, err := s.loadMessage(v.folder.ID, uid, need)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue // expunged while we were serving the command
			}
			return mapError(s.log, err)
		}
		if err := s.writeFetch(w, t.seq, meta, opts, d); err != nil {
			return err
		}
	}
	return nil
}

// needs says which stored pieces of a message a FETCH needs.
type needs struct {
	header   bool // the header block (BODY[HEADER...] of the message itself)
	envelope bool // ENVELOPE
	index    bool // the part tree (BODYSTRUCTURE, numbered sections, BINARY)
	mime     bool // the complete message
}

func fetchNeeds(o *imap.FetchOptions) needs {
	n := needs{envelope: o.Envelope, index: o.BodyStructure != nil}
	if len(o.BinarySection) > 0 || len(o.BinarySectionSize) > 0 {
		n.index, n.mime = true, true
	}
	for _, bs := range o.BodySection {
		switch {
		case headerOnly(bs):
			n.header = true
		case len(bs.Part) > 0:
			n.index, n.mime = true, true
		default:
			n.mime = true
		}
	}
	return n
}

// loaded is what loadMessage read for one message.
type loaded struct {
	header []byte
	mime   []byte
	ix     *mimestruct.Index
}

// loadMessage reads the pieces of one message that a FETCH needs. The structure
// index comes from the database; messages stored before it existed (and indexes
// that do not match the message) get one built on the spot, within the same
// limits.
func (s *session) loadMessage(folderID int64, uid uint32, need needs) (*loaded, error) {
	st := s.acct.Store()
	d := &loaded{}
	if need.index || need.envelope {
		blob, err := st.Structure(folderID, uid)
		if err != nil {
			return nil, err
		}
		if ix, derr := mimestruct.Decode(blob); derr == nil {
			d.ix = ix
		}
	}
	if need.mime || (need.index && d.ix == nil) {
		mime, err := st.MIME(folderID, uid)
		if err != nil {
			return nil, err
		}
		d.mime = mime
		if d.ix == nil || !d.ix.Check(len(mime)) {
			d.ix = mimestruct.Build(mime)
		}
	}
	if need.header || (need.envelope && d.ix == nil) {
		m, err := st.Message(folderID, uid)
		if err != nil {
			return nil, err
		}
		d.header = m.Header
	}
	return d, nil
}

// headerOnly reports whether a body section can be answered from the header
// block alone.
func headerOnly(bs *imap.FetchItemBodySection) bool {
	return len(bs.Part) == 0 && bs.Specifier == imap.PartSpecifierHeader
}

func (s *session) writeFetch(w *imapserver.FetchWriter, seq uint32, meta store.MsgMeta, o *imap.FetchOptions, d *loaded) error {
	rw := w.CreateMessage(seq)
	// UID is always sent: imapflow keys everything on it.
	rw.WriteUID(imap.UID(meta.UID))
	if o.Flags {
		rw.WriteFlags(imapFlags(meta.Flags, meta.Keywords))
	}
	if o.InternalDate {
		rw.WriteInternalDate(time.Unix(meta.InternalDate, 0).UTC())
	}
	if o.RFC822Size {
		rw.WriteRFC822Size(meta.Size)
	}
	if o.Envelope {
		if d.ix != nil {
			rw.WriteEnvelope(d.ix.Env.IMAP())
		} else {
			rw.WriteEnvelope(mimestruct.EnvelopeOf(d.header).IMAP())
		}
	}
	if o.BodyStructure != nil {
		rw.WriteBodyStructure(d.ix.BodyStructure())
	}
	for _, bs := range o.BodySection {
		var buf []byte
		if headerOnly(bs) && d.header != nil {
			buf = mimestruct.Section(d.header, nil, bs)
		} else {
			src := d.mime
			if src == nil {
				src = d.header
			}
			buf = mimestruct.Section(src, d.ix, bs)
		}
		if err := writeLiteral(rw.WriteBodySection(bs, int64(len(buf))), buf); err != nil {
			return err
		}
	}
	for _, bs := range o.BinarySection {
		buf := mimestruct.BinarySection(d.mime, d.ix, bs)
		if err := writeLiteral(rw.WriteBinarySection(bs, int64(len(buf))), buf); err != nil {
			return err
		}
	}
	for _, bss := range o.BinarySectionSize {
		rw.WriteBinarySectionSize(bss, mimestruct.BinarySectionSize(d.mime, d.ix, bss))
	}
	return rw.Close()
}

func writeLiteral(wc io.WriteCloser, b []byte) error {
	_, werr := wc.Write(b)
	cerr := wc.Close()
	if werr != nil {
		return werr
	}
	return cerr
}
