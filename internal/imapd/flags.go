package imapd

import (
	"strings"

	"github.com/emersion/go-imap/v2"

	"easbridge/internal/store"
)

// systemFlags maps IMAP system flags to the store's bit mask.
var systemFlags = map[string]store.Flags{
	`\seen`:     store.FlagSeen,
	`\answered`: store.FlagAnswered,
	`\flagged`:  store.FlagFlagged,
	`\deleted`:  store.FlagDeleted,
	`\draft`:    store.FlagDraft,
}

// permanentFlags are what the bridge stores (DESIGN section 9: no keywords are
// advertised; MailFlow only writes these).
var permanentFlags = []imap.Flag{imap.FlagSeen, imap.FlagFlagged, imap.FlagDeleted, imap.FlagDraft, imap.FlagAnswered}

// parseFlags splits client-supplied flags into system flags and keywords.
// \Recent and \* are ignored; unknown backslash flags are ignored; anything
// else is a keyword (kept locally only).
func parseFlags(in []imap.Flag) (sys store.Flags, keywords []string) {
	for _, f := range in {
		s := strings.TrimSpace(string(f))
		low := strings.ToLower(s)
		if bit, ok := systemFlags[low]; ok {
			sys |= bit
			continue
		}
		if s == "" || strings.HasPrefix(s, `\`) {
			continue
		}
		keywords = append(keywords, s)
	}
	return sys, keywords
}

// imapFlags renders stored flags for FLAGS responses.
func imapFlags(f store.Flags, keywords string) []imap.Flag {
	var out []imap.Flag
	if f.Has(store.FlagSeen) {
		out = append(out, imap.FlagSeen)
	}
	if f.Has(store.FlagAnswered) {
		out = append(out, imap.FlagAnswered)
	}
	if f.Has(store.FlagFlagged) {
		out = append(out, imap.FlagFlagged)
	}
	if f.Has(store.FlagDeleted) {
		out = append(out, imap.FlagDeleted)
	}
	if f.Has(store.FlagDraft) {
		out = append(out, imap.FlagDraft)
	}
	for _, k := range store.SplitKeywords(keywords) {
		out = append(out, imap.Flag(k))
	}
	return out
}

// hasFlag reports whether a message carries the given IMAP flag (system flags
// and keywords, case-insensitive like the protocol).
func hasFlag(m store.MsgMeta, flag imap.Flag) bool {
	low := strings.ToLower(string(flag))
	if bit, ok := systemFlags[low]; ok {
		return m.Flags.Has(bit)
	}
	for _, k := range store.SplitKeywords(m.Keywords) {
		if strings.EqualFold(k, string(flag)) {
			return true
		}
	}
	return false
}
