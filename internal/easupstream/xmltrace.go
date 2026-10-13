package easupstream

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/hstern/go-activesync/wbxml"
)

// renderXML writes the decoded WBXML message root as indented XML for
// `easbridge probe --trace`. What it shows is decided by an allow-list: the value
// of an element is printed only if the element is listed in shown below (protocol
// structure: keys, ids of folders, statuses, sizes, options, the device description
// and the policy). The value of every other element, and every binary block, is
// replaced by its length and its kind (an inline string, "text", or an OPAQUE block,
// "binary": the first real Fetch failed because a MIME body came as the one when the
// library expected the other, and a trace that could not tell them apart hid the
// cause), so that a message body, a subject, an address, a folder name, a user name or
// a password cannot appear in a trace, whatever the server sends or a later version of
// the protocol adds. The structure (the element names) is always shown: that is what a
// protocol error is about. Credentials are not in the messages at all (they travel in
// an HTTP header that is never traced).
func renderXML(root *wbxml.Element, reg *wbxml.Registry) string {
	var b strings.Builder
	renderElement(&b, root, root.Codepage, 0, reg)
	return b.String()
}

// shown lists, per code page, the elements whose values are printed. A page marked
// all shows every value.
var shown = map[byte]pageRule{
	wbxml.PageAirSync: names("SyncKey", "CollectionId", "Status", "GetChanges", "DeletesAsMoves", "WindowSize", "FilterType", "Truncation",
		"Conflict", "MIMESupport", "MIMETruncation", "MaxItems", "Class", "MoreAvailable", "Wait", "HeartbeatInterval", "Limit", "Partial",
		"ConversationMode"),
	wbxml.PageAirSyncBase:     names("Type", "TruncationSize", "AllOrNone", "EstimatedDataSize", "Truncated", "NativeBodyType", "Status", "Method", "IsInline"),
	wbxml.PageEmail:           names("Read", "Importance", "MessageClass", "FlagStatus", "InternetCPID", "AllDayEvent", "BusyStatus", "InstanceType", "Sensitivity", "ResponseRequested", "DisallowNewTimeProposal"),
	wbxml.PageEmail2:          names("LastVerbExecuted", "ReceivedAsBcc", "MeetingMessageType", "IsDraft"),
	wbxml.PageFolderHierarchy: names("SyncKey", "Status", "Count", "Type"), // not DisplayName, ServerId, ParentId
	wbxml.PagePing:            allValues,
	wbxml.PageProvision:       allValues, // the policy is the point of Provision, and holds nothing personal
	wbxml.PageSettings:        names("Model", "FriendlyName", "OS", "OSLanguage", "UserAgent", "EnableOutboundSMS", "Status"),
	wbxml.PageItemOperations:  names("Store", "Status", "Range", "Total", "DeleteSubFolders", "MoveAlways", "Version"), // not Data, UserName, Password
	wbxml.PageComposeMail:     names("SaveInSentItems", "ReplaceMime", "Status"),                                       // not Mime, ClientId
	wbxml.PageMove:            names("Status"),
	// Contacts and the Global Address List (v0.2.0). A contact is all personal data, so
	// none of the elements of the Contacts and Contacts2 pages shows a value (the entry is
	// listed so that the decision is written down, not left to the default). A Search shows
	// its store ("GAL"), its range, its total and its statuses, and never the Query (a
	// name or an address) or the Properties of a Result (the GAL page: names, numbers,
	// the address itself); of the GAL page only the status of a property is shown.
	wbxml.PageContacts:  names(),
	wbxml.PageContacts2: names(),
	wbxml.PageSearch:    names("Store", "Name", "Range", "Total", "Status", "Response"),
	wbxml.PageGAL:       names("Status"),
}

type pageRule func(name string) bool

func names(list ...string) pageRule {
	set := make(map[string]bool, len(list))
	for _, n := range list {
		set[n] = true
	}
	return func(name string) bool { return set[name] }
}

func allValues(string) bool { return true }

// maxShown is the longest value printed; the allowed elements hold short values.
const maxShown = 200

func renderElement(b *strings.Builder, el *wbxml.Element, basePage byte, depth int, reg *wbxml.Registry) {
	pad := strings.Repeat("  ", depth)
	name := el.Name
	if el.Codepage != basePage {
		if p := reg.PageName(el.Codepage); p != "" {
			name = strings.ToLower(p) + ":" + name
		} else {
			name = fmt.Sprintf("page%d:%s", el.Codepage, name)
		}
	}
	var kids []*wbxml.Element
	var text strings.Builder
	hiddenText, hiddenBinary := 0, 0 // bytes of inline strings that are not shown, and of OPAQUE blocks
	allowed := false
	if rule, ok := shown[el.Codepage]; ok && rule(el.Name) {
		allowed = true
	}
	for _, n := range el.Children {
		switch c := n.(type) {
		case *wbxml.Element:
			kids = append(kids, c)
		case wbxml.Text:
			if allowed {
				text.WriteString(string(c))
			} else {
				hiddenText += len(c)
			}
		case wbxml.Opaque:
			hiddenBinary += len(c)
		}
	}
	switch {
	case len(kids) == 0 && text.Len() == 0 && hiddenText+hiddenBinary == 0:
		fmt.Fprintf(b, "%s<%s/>\n", pad, name)
	case len(kids) == 0:
		fmt.Fprintf(b, "%s<%s>%s</%s>\n", pad, name, value(text.String(), hiddenText, hiddenBinary), name)
	default:
		fmt.Fprintf(b, "%s<%s>\n", pad, name)
		if text.Len() > 0 || hiddenText+hiddenBinary > 0 {
			fmt.Fprintf(b, "%s  %s\n", pad, value(text.String(), hiddenText, hiddenBinary))
		}
		for _, k := range kids {
			renderElement(b, k, basePage, depth+1, reg)
		}
		fmt.Fprintf(b, "%s</%s>\n", pad, name)
	}
}

// value prints what may be shown of an element's text, or says how much there is and
// of which kind: "[72024 text bytes not shown]" for an inline string (WBXML STR_I),
// "[16 binary bytes not shown]" for an OPAQUE block, "[3 text + 16 binary bytes not
// shown]" for both.
func value(shownText string, hiddenText, hiddenBinary int) string {
	if shownText != "" || hiddenText+hiddenBinary == 0 {
		return clean(shownText)
	}
	switch {
	case hiddenBinary == 0:
		return fmt.Sprintf("[%d text bytes not shown]", hiddenText)
	case hiddenText == 0:
		return fmt.Sprintf("[%d binary bytes not shown]", hiddenBinary)
	}
	return fmt.Sprintf("[%d text + %d binary bytes not shown]", hiddenText, hiddenBinary)
}

// clean makes a value safe to print: no control characters, escaped markup, bounded.
func clean(s string) string { return cleanN(s, maxShown) }

// cleanN is clean with a bound of n bytes.
func cleanN(s string, n int) string {
	if len(s) > n {
		cut := n
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + fmt.Sprintf("[%d more bytes not shown]", len(s)-cut)
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '<':
			b.WriteString("&lt;")
		case r == '>':
			b.WriteString("&gt;")
		case r == '&':
			b.WriteString("&amp;")
		case r < 0x20 || r == 0x7f || r == utf8.RuneError:
			b.WriteByte('?')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
