package easupstream

// What an EAS status code means for the bridge. The numbers come from MS-ASCMD
// 2.2.2 (common status codes, valid for every command) and MS-ASPROV 2.2.2.54 and
// 3.1.5.2 (the statuses of Provision and of its Policy element), which are small
// numbers that mean something else than the same number in other commands.
//
// go-activesync names some of these wrongly (141 to 145 and 177 are one code off,
// see third_party/go-activesync/PATCHES.md), so the bridge does not use its names:
// every message below is written here, from the specification.

// statusFacts is what the bridge knows about one status.
type statusFacts struct {
	// msg is one line for the operator (log and probe): what the server means and,
	// where somebody has to act, who. No trailing period.
	msg string
	// perm: the same request would fail again, so retrying it unchanged is pointless.
	perm bool
	// global: the status is about the device or the account, not about one item or
	// folder. A global status must never be counted against a message.
	global bool
}

// Shorthands for the table.
func permanent(msg string) statusFacts { return statusFacts{msg: msg, perm: true, global: true} }
func transient(msg string) statusFacts { return statusFacts{msg: msg, perm: false, global: true} }
func malformed(what string) statusFacts {
	return permanent("the server rejected the request as malformed (" + what + ")")
}

// commonStatus holds the statuses that any command can answer with (all are 101
// or more). Codes that are not listed here keep the classification of
// permanentStatus and the library's own text.
var commonStatus = map[int]statusFacts{
	101: malformed("invalid content"),
	102: malformed("invalid WBXML"),
	103: malformed("invalid XML"),
	104: malformed("invalid date or time"),
	105: malformed("invalid combination of ids"),
	106: malformed("invalid ids"),
	107: malformed("invalid MIME"),
	108: permanent("the server does not accept the device id"),
	109: permanent("the server does not accept the device type"),
	110: transient("the server reports an unspecified error"),
	111: transient("the server asks the client to retry later"),
	112: permanent("the server cannot access the directory service (Active Directory access denied)"),
	113: permanent("the mailbox is over its size quota"),
	114: transient("the mailbox server is offline"),
	115: transient("the send quota is used up"),
	123: permanent("the user has no mailbox"),
	124: permanent("the server refused a request without credentials"),
	125: permanent("the user was not found in the directory service"),
	126: permanent("ActiveSync is disabled for this user"),
	127: permanent("the server is configured to keep this user from syncing"),
	128: permanent("the server keeps users on legacy mailbox servers from syncing"),
	129: permanent("the server blocks this device for this user; the Exchange administrator must allow it"),
	130: permanent("the server denied access to this request"),
	131: permanent("the user's account is disabled"),

	139: permanent("the server refuses a device that cannot fully apply its device policy; the Exchange administrator must allow this device"),
	140: transient("the server ordered a remote wipe"),
	141: permanent("the server saw no policy key and treats this device as not provisionable under its strict policy; a gateway may drop X-MS-PolicyKey, or the Exchange administrator must allow the device"),
	142: transient("the server wants the device to provision first"),
	143: transient("the server's device policy has to be refreshed, the device must provision again"),
	144: transient("the server no longer accepts the device's policy key, the device must provision again"),
	145: permanent("the server does not allow externally managed devices; the Exchange administrator must allow this device"),
	165: permanent("server requires device information in Provision"),
	177: permanent("the user has too many device partnerships; old devices must be removed in Exchange"),
}

// provisionStatus holds the statuses of the Provision element itself
// (MS-ASPROV 2.2.2.54.2); the common ones above can come with it as well.
var provisionStatus = map[int]statusFacts{
	2: {msg: "the server rejected the Provision request as a protocol error", perm: true, global: true},
	3: {msg: "the server reported an error while provisioning (usually temporary)", global: true},
}

// syncStatus holds the statuses of Sync itself (MS-ASCMD 2.2.3.177.17) that need a
// line of their own; the others keep the library's text. Their meaning is that of
// Sync: the same numbers mean something else in other commands.
var syncStatus = map[int]statusFacts{
	4:  {msg: "the server rejected the Sync request as a protocol error: it breaks the specification (easbridge probe --trace shows the request)", perm: true},
	5:  {msg: "the server reported an error while syncing (usually temporary)"},
	13: {msg: "the server says the Sync request was incomplete and wants it sent again"},
}

// searchStatus holds the statuses of a Search request and of its Store element
// (MS-ASCMD 2.2.3.162.2 and 2.2.3.177.10), used for the Global Address List. The
// small numbers mean something else here than in other commands. 5 and 13 are the ones
// that say the directory must not be searched: EASBridge then leaves it alone for a day.
var searchStatus = map[int]statusFacts{
	2:  {msg: "the server rejected the Search request as invalid", perm: true},
	3:  {msg: "the server reported an error while searching (usually temporary)"},
	4:  {msg: "the server rejected a link in the Search request", perm: true},
	5:  {msg: "the server denied access to the directory search (access denied)", perm: true},
	7:  {msg: "the server could not connect to the directory while searching (usually temporary)"},
	8:  {msg: "the server found the Search query too complex", perm: true},
	10: {msg: "the directory search timed out on the server (usually temporary)"},
	11: {msg: "the server wants a folder sync before it searches (usually temporary)"},
	13: {msg: "the server blocks the directory search for this user (access blocked)", perm: true},
	14: {msg: "the server wants credentials for the directory search", perm: true},
}

// policyStatus holds the statuses of Policy inside Provision (MS-ASPROV 2.2.2.54.1).
// Status 2 ("no policy defined") never reaches the bridge: the library takes it as
// success (patch 1).
var policyStatus = map[int]statusFacts{
	3: {msg: "the server does not know the policy type the bridge asked for", perm: true, global: true},
	4: {msg: "the device policy on the server is corrupt; the Exchange administrator must fix it", perm: true, global: true},
	5: {msg: "the server says the policy key of the acknowledgement is out of date, provisioning starts again next time", global: true},
}

// lookupStatus returns what is known about a status that command answered with.
// command is the library's name for it: "Provision" for the status of the Provision
// element, "Provision/Policy" for the one of a policy, anything else for the rest.
func lookupStatus(command string, code int) (statusFacts, bool) {
	var f statusFacts
	var ok bool
	switch command {
	case "Provision":
		if f, ok = provisionStatus[code]; ok {
			return f, true
		}
	case "Provision/Policy":
		f, ok = policyStatus[code]
		return f, ok
	case "Sync":
		if f, ok = syncStatus[code]; ok {
			return f, true
		}
	case "Search/GAL":
		if f, ok = searchStatus[code]; ok {
			return f, true
		}
	}
	f, ok = commonStatus[code]
	return f, ok
}

// permanentStatus reports whether a status that statuses.go does not describe
// means "do not retry the same request": everything except the server-side
// family. (The common statuses above carry their own classification.)
func permanentStatus(code int) bool {
	switch code {
	case 5, 9, 13:
		return false
	}
	return true
}
