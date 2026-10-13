package imapd

import (
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"easbridge/internal/store"
)

const delim = '/'

func useAttr(use string) imap.MailboxAttr {
	switch use {
	case `\Sent`:
		return imap.MailboxAttrSent
	case `\Drafts`:
		return imap.MailboxAttrDrafts
	case `\Trash`:
		return imap.MailboxAttrTrash
	case `\Junk`:
		return imap.MailboxAttrJunk
	case `\Archive`:
		return imap.MailboxAttrArchive
	}
	return ""
}

// List implements imapserver.Session (LIST, LSUB and the LIST-EXTENDED forms
// go-imap parses). Special-use attributes are always returned; every folder
// counts as subscribed.
func (s *session) List(w *imapserver.ListWriter, ref string, patterns []string, options *imap.ListOptions) error {
	if err := s.needAuth(); err != nil {
		return err
	}
	// LIST "" "": report the hierarchy delimiter and the (non-selectable) root.
	if len(patterns) == 0 {
		if options.SelectSubscribed || options.SelectSpecialUse {
			return nil
		}
		return w.WriteList(&imap.ListData{Attrs: []imap.MailboxAttr{imap.MailboxAttrNoSelect}, Delim: delim, Mailbox: ""})
	}
	folders, err := s.acct.Store().Folders()
	if err != nil {
		return mapError(s.log, err)
	}
	for _, f := range folders {
		if options.SelectSpecialUse && f.SpecialUse == "" {
			continue
		}
		match := false
		for _, p := range patterns {
			if imapserver.MatchList(f.Path, delim, normalizeInbox(ref), normalizeInbox(p)) {
				match = true
				break
			}
		}
		if !match {
			continue
		}
		data := &imap.ListData{Mailbox: f.Path, Delim: delim}
		if hasChildren(folders, f.Path) {
			data.Attrs = append(data.Attrs, imap.MailboxAttrHasChildren)
		} else {
			data.Attrs = append(data.Attrs, imap.MailboxAttrHasNoChildren)
		}
		if a := useAttr(f.SpecialUse); a != "" {
			data.Attrs = append(data.Attrs, a)
		}
		if options.ReturnSubscribed || options.SelectSubscribed {
			data.Attrs = append(data.Attrs, imap.MailboxAttrSubscribed)
		}
		if options.ReturnStatus != nil {
			if sd, err := s.statusFor(&f, options.ReturnStatus); err == nil {
				data.Status = sd
			}
		}
		if err := w.WriteList(data); err != nil {
			return err
		}
	}
	return nil
}

// normalizeInbox upper-cases a leading "inbox" path component (INBOX is the one
// case-insensitive mailbox name).
func normalizeInbox(p string) string {
	if len(p) >= 5 && strings.EqualFold(p[:5], "INBOX") && (len(p) == 5 || p[5] == '/') {
		return "INBOX" + p[5:]
	}
	return p
}

func hasChildren(folders []store.Folder, path string) bool {
	prefix := path + string(delim)
	for _, f := range folders {
		if strings.HasPrefix(f.Path, prefix) {
			return true
		}
	}
	return false
}

// Status implements imapserver.Session. It answers from the database, never
// from Exchange, so MailFlow's 60-second STATUS cycle is cheap and exact.
func (s *session) Status(mailbox string, options *imap.StatusOptions) (*imap.StatusData, error) {
	if err := s.needAuth(); err != nil {
		return nil, err
	}
	f, err := s.acct.Store().FolderByPath(mailbox)
	if err != nil {
		return nil, no(imap.ResponseCodeNonExistent, "Mailbox does not exist")
	}
	return s.statusFor(f, options)
}

func (s *session) statusFor(f *store.Folder, options *imap.StatusOptions) (*imap.StatusData, error) {
	c, err := s.acct.Store().Counts(f.ID)
	if err != nil {
		return nil, mapError(s.log, err)
	}
	// Every requested number is filled in: go-imap dereferences them.
	data := &imap.StatusData{Mailbox: f.Path}
	if options.NumMessages {
		n := uint32(c.Messages)
		data.NumMessages = &n
	}
	if options.UIDNext {
		data.UIDNext = imap.UID(c.UIDNext)
	}
	if options.UIDValidity {
		data.UIDValidity = c.UIDValidity
	}
	if options.NumUnseen {
		n := uint32(c.Unseen)
		data.NumUnseen = &n
	}
	if options.NumRecent {
		var n uint32
		data.NumRecent = &n
	}
	if options.NumDeleted || options.Size || options.DeletedStorage {
		metas, err := s.acct.Store().ListMessages(f.ID)
		if err != nil {
			return nil, mapError(s.log, err)
		}
		var deleted uint32
		var size, delSize int64
		for _, m := range metas {
			size += m.Size
			if m.Flags.Has(store.FlagDeleted) {
				deleted++
				delSize += m.Size
			}
		}
		if options.NumDeleted {
			data.NumDeleted = &deleted
		}
		if options.Size {
			data.Size = &size
		}
		if options.DeletedStorage {
			data.DeletedStorage = &delSize
		}
	}
	return data, nil
}
