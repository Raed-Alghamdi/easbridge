# EASBridge v0.2 design: read-only contacts over CardDAV

Status: written before the code and kept up to date as decisions were made. Deviations from the brief that started this work
are listed in the last section. `DESIGN.md` is not edited; this file extends it for v0.2.

## 1. Goal and non-goals

MailFlow (3.9.0) has a **read-only CardDAV client**: it pulls an address book from a CardDAV URL every 15 to 1440
minutes and re-serves the contacts to phones through its own CardDAV server. So MailFlow needs no change: EASBridge serves a
read-only CardDAV address book and MailFlow pulls it. Two sources feed it:

1. **The mailbox's own Contacts folders**, through EAS Sync (class Contacts). One address book per folder.
2. **A directory built up as you go** from the organization's Global Address List (GAL). EAS can only *search* the GAL
   (`Search`, store `GAL`, at most about 100 results per query); it cannot list it. So the bridge looks up, one at a time and
   rate-limited, the addresses in the organization's domain(s) that appear in the user's mail, and keeps a card only for an
   **exact email match**.

**The GAL is never enumerated or harvested.** There are no letter-by-letter or wildcard searches, no search for anything that is
not a full address, a local part or a display name taken from a message header of a candidate, and the other people a search
returns are discarded unread. This is a deliberate privacy and policy decision; the tests pin it (section 7).

Non-goals of v0.2: writing contacts back (everything is read-only, the server refuses every write method), photos, calendars,
tasks, notes (the Notes class), the contact notes (see 3.4), `sync-collection` (MailFlow does not use it), several users per
bridge sharing contacts, and any change to MailFlow.

## 2. Enablement and configuration

All values are validated like the existing keys (one error list at start-up, `internal/config`).

| Variable | Default | Rule |
|---|---|---|
| `EASBRIDGE_CARDDAV_ADDR` | unset | `IP:port`, TLS with the same certificate and key as IMAP and SMTP. The same bind rules (an IP, not a host name; no unspecified address without `EASBRIDGE_ALLOW_ANY_BIND=1`). It must differ from the IMAP, SMTP and health addresses. **Unset means the whole feature is off**: no contacts sync, no GAL lookups, no listener, no extra requests to Exchange. A v0.1.8 configuration behaves exactly as before, apart from the schema migration. |
| `EASBRIDGE_CONTACTS_INTERVAL` | `30m` | How often each Contacts folder is synced (polling; contacts folders are not added to Ping). 5 minutes to 24 hours. |
| `EASBRIDGE_DIRECTORY_DOMAINS` | the domains of `EASBRIDGE_ALLOWED_USERS` | Comma-separated domains whose addresses are looked up. `none` turns the directory off. |
| `EASBRIDGE_DIRECTORY_LOOKUPS_PER_HOUR` | `60` | 1 to 180. One search in flight at a time; consecutive searches are at least `max(20 s, 1 h / value)` apart, which is what caps the value at 180; and a daily cap of ten times the value. |
| `EASBRIDGE_DIRECTORY_REFRESH` | `720h` (30 days) | How long a found card is kept before it is looked up again. 24 hours to 365 days. |
| `EASBRIDGE_CONTACTS_BOOK_NAME` | `Exchange contacts` | Display name of the address book of the default Contacts folder. |
| `EASBRIDGE_DIRECTORY_BOOK_NAME` | `Directory` | Display name of the directory address book. Must differ from the other. |

The variables are validated even when the feature is off. When the feature is off the bridge removes any contact data a
previous run stored (a cache that Exchange can rebuild), so that turning the feature off also removes the data.

## 3. Personal contacts (the Contacts folders)

### 3.1 Which folders

FolderSync already lists every folder; the bridge keeps the ones of type 9 (default Contacts) and 14 (user-created contact
folders) in `contact_folders`, next to the mail folders it already keeps. The default folder's book is called
`EASBRIDGE_CONTACTS_BOOK_NAME`, a user folder's book its folder name. A rename or a delete in FolderSync renames or removes the
book (and its cards) on the next hierarchy sync; a hierarchy sync that lists everything (key 0) removes the books it no longer
lists.

**The stored hierarchy must have kept them (0.2.2).** An incremental FolderSync lists only what changed since the key it is given.
The hierarchy key of an install that ran without the contacts (every key of 0.1.8, and of any run with `EASBRIDGE_CARDDAV_ADDR`
unset) stands for a hierarchy from which the Contacts folders were dropped, so Exchange never mentions them again and the books
stay empty for good. `accounts.contact_folders_kept` (schema 8, 0 for every existing account) records that the hierarchy behind
`folder_sync_key` was applied with the contact folders kept. When the contacts are on and it is 0, `doFolderSync` sends key `0`
instead of the stored key (one log line), applies the contact folders with Full set (a stale folder goes) and sets the marker in
the same transaction that commits the new key. Every committed hierarchy sets it to whether the contacts are on, so a run with them
off clears it; so does removing the contact data (the service starting with the contacts off, a wipe). Turning the contacts on
again therefore reads the hierarchy once more.

What a FolderSync from key 0 does to the collections' Sync keys is **not specified** (MS-ASCMD, "SyncKey (FolderSync)", says the
server returns every folder and must return status 9 for a key it does not know, and nothing about other collections). Two cases,
both handled with the existing paths: Exchange keeps the keys (the usual behaviour of devices that re-read their hierarchy), and
nothing else happens; or it drops them, the next Sync of each mail folder is answered with status 3, the library restarts from key
0 and the engine takes the silent key reset path (the folder is reconciled by ServerId, UIDs unchanged, nothing announced).
Mail folders are matched by ServerId: a full hierarchy changes no folder row, UIDVALIDITY or UID, and marks no folder dirty.

### 3.2 Sync

The engine's single worker syncs the contacts folders after the mail work (mail has priority), one Sync round trip at a time,
each folder when it is due: at start, then every `EASBRIDGE_CONTACTS_INTERVAL`, and at once while its first download is
running. The request for a first SyncKey is the SyncKey and the CollectionId only (real-Exchange finding 2; the vendored
`genericSyncFolder` sent `GetChanges` with SyncKey 0 and is patched, patch 15). Later requests carry `DeletesAsMoves`,
`GetChanges` and `WindowSize` 100.

**First sync, all or nothing.** A book lists no cards until its first full download has finished; then all of them appear at
once. The download is staged: its cards are stored with `staged = 1`, invisible to CardDAV, and one transaction swaps them in
when a data call (not the bootstrap) reports no `MoreAvailable`.

**Invalid SyncKey (Sync status 3).** This happens, for example, after `easbridge probe` started its own chain on the shared
DeviceId. The adapter restarts that folder's chain from key 0 (the same as the library does for mail), and reports the reset.
The engine drops any staged cards, starts a new staged download, keeps serving the old cards meanwhile, and replaces them
atomically when the new download completes. Cards that the new download no longer lists are gone after the swap, which is how
deletes made during the gap are noticed.

Adds, changes and deletes of a later sync apply directly to the visible cards in one transaction with the new key (the
at-least-once and replay rules of the mail path apply: the key is committed with the items).

### 3.3 Failures

The same rules as mail: the circuit breaker pauses everything (including contacts) while it is open, Retry-After is honoured
through the breaker, 401 stops all traffic, a wipe order is honoured. A failing folder backs off on its own (the engine's
back-off); status 8 and 12 ask for a hierarchy sync first.

### 3.4 Field mapping (vCard 3.0)

Everything `internal/easupstream` receives from the library's `ContactItem` that MailFlow's `vcard.js` shows or that a phone
shows is mapped. MailFlow's own client keeps the raw vCard and its CardDAV server serves it as it is, so what MailFlow does not
show still reaches phones.

| EAS | vCard | Notes |
|---|---|---|
| FirstName, LastName, MiddleName, Title (honorific), Suffix | `N:last;first;middle;title;suffix` | MailFlow reads the first two parts |
| FileAs, else the parts of the name, else company, else the first address | `FN` | always present (vCard 3.0 requires it) |
| NickName | `NICKNAME` | |
| Email1Address, Email2Address, Email3Address | `EMAIL;TYPE=OTHER` | The type is not known. MailFlow takes the first `TYPE=` it finds and labels `work`, `home`, anything else "Other". An address written as `Name <addr>` is reduced to `addr`. |
| BusinessPhoneNumber | `TEL;TYPE=WORK;TYPE=VOICE` | One `TYPE` parameter each, because MailFlow reads only up to the first `;`, and a combined `TYPE=WORK,VOICE` would show as "Other" |
| MobilePhoneNumber | `TEL;TYPE=CELL` | MailFlow shows `cell` as Mobile |
| HomePhoneNumber | `TEL;TYPE=HOME;TYPE=VOICE` | |
| CompanyName, Department | `ORG:company;department` | MailFlow shows the first part under the name |
| JobTitle | `TITLE` | |
| HomeAddress..., BusinessAddress... | `ADR;TYPE=HOME` / `ADR;TYPE=WORK` | `;;street;city;state;postal;country` |
| Birthday, Anniversary | `BDAY:YYYY-MM-DD`, `X-ANNIVERSARY:YYYY-MM-DD` | the UTC date |
| WebPage | `URL` | |
| Categories | `CATEGORIES` | |
| OfficeLocation | `NOTE:Office: ...` | MailFlow shows nothing but the organization and the notes; the office is shown as a note |

Not mapped, on purpose: the photo (no photos in v0.2), `GovernmentId`, `CustomerId` (identifiers nobody asked for), the instant
messaging addresses and the manager's name (no place in MailFlow or in vCard 3.0). The notes of a contact (the Body element) are
not requested by the library's contacts Sync and are therefore not available; the contact's `NOTE` is only the office line.

## 4. The directory

### 4.1 Candidates

A candidate is an address in a directory domain that appears in the From, To or Cc header of a stored message in any folder.
The bridge keeps, per address, how often (`seen_count`) and when (`first_seen`, `last_seen`) it was seen, and the last display
name it was seen with (taken from the header, used only for the third search of the ladder). Candidates come from:

- **Seeding**: once after start, a background task reads the headers of the messages already in `state.db`, in batches, in
  transactions of their own (never inside a migration). It records its progress and the domain list it ran for; it runs again if
  the domain list changed.
- **Ingest**: every message the store writes (a server message, staged or visible, a local draft, a Sent placeholder) adds its
  candidates in the same transaction, when the directory is on.

The account's own address is never looked up.

### 4.2 Order and limits

The next lookup is the due candidate that was seen most recently, ties broken by the higher `seen_count`. (Recent and frequent
first; a candidate that has not been looked up yet is due at once.) One search is in flight at a time, per account. The spacing
between two searches is `max(20 s, 1 h / EASBRIDGE_DIRECTORY_LOOKUPS_PER_HOUR)`, kept across restarts (the time of the last search
is stored), and at most ten times the hourly value are sent per UTC day (the count is stored; `/healthz` shows it).

### 4.3 The ladder

For a candidate the bridge searches the **full address**. If no entry's `EmailAddress` equals the candidate (case-insensitive,
exact), it searches the **local part**, then the **display name** from the message header if there is one and it differs. At most
three searches per candidate per attempt. **Only an entry whose `EmailAddress` equals the candidate is kept.** The other entries a
search returns are not stored, not logged and not counted beyond their number.

Whether Exchange's GAL search finds an entry by its full SMTP address is not known until the probe has run against the real
server (it matches by name and alias on most servers). The ladder is why the local-part and the name searches exist.

### 4.4 Outcomes and schedule

| Outcome | Effect |
|---|---|
| exact match | a card is stored or replaced; the candidate is `found`; checked again after `EASBRIDGE_DIRECTORY_REFRESH` |
| no match, never found before | `not found`; negative cache, checked again after 30 days |
| no match, found before | the first such miss is recorded and a re-check is scheduled 7 days later; a second miss at least 7 days after the first removes the card (the person left) and puts the candidate in the negative cache; a find in between clears the miss |
| failed search (network, throttling, timeout, server error) | nothing is recorded about the candidate; the loop backs off (1 minute doubling to 1 hour) |

### 4.5 When lookups stop

Lookups run only while the engine holds the account's credentials (they are goroutines of the engine). They pause while the
circuit breaker is open and honour Retry-After through it. On a throttling status (HTTP 429 or 503, Search status 3, 7 or 10, a
transient common status) the loop backs off. On access denied or blocked (Search status 5 or 13, 14 credentials required, common
status 130) the directory stops for 24 hours, stored in `directory_state.paused_until`, with one log line.

### 4.6 The card

| GAL | vCard |
|---|---|
| DisplayName | `FN` (the address if empty) |
| FirstName, LastName | `N:last;first;;;` |
| EmailAddress | `EMAIL;TYPE=WORK` |
| Phone | `TEL;TYPE=WORK;TYPE=VOICE` |
| MobilePhone | `TEL;TYPE=CELL` |
| Title | `TITLE` |
| Company | `ORG` |
| Office | `NOTE:Office: ...` (the same reasoning as in 3.4) |

`HomePhone` and `Alias` are **not stored** (data minimization): the adapter does not even hand them to the engine.

## 5. The CardDAV server (`internal/carddav`)

Hand-written on `net/http` and `encoding/xml`; no new Go dependency. Read-only. TLS 1.2 and later with the certificate of IMAP.

### 5.1 What MailFlow's client sends (read from 3.9.0, never copied)

Discovery: `PROPFIND` Depth 0 for `current-user-principal` at the URL given in MailFlow (`https://easbridge:<port>/`), then at
`/.well-known/carddav`; `PROPFIND` Depth 0 for `addressbook-home-set` on the principal; `PROPFIND` Depth 1 for `resourcetype`,
`displayname` and `getctag` on the home (the books are the entries with an `addressbook` resource type); then per book an
`addressbook-query` REPORT (Depth 1, `getetag` and `address-data`, no filter) or, when that is refused with 400, 403, 405, 415,
422 or 501, a `PROPFIND` Depth 1 for `getetag` and `resourcetype` plus `addressbook-multiget` REPORTs of 100 hrefs each (no Depth).
HTTP Basic on every request. MailFlow follows redirects and parses with the namespace prefixes removed. It decodes numeric
character references in `address-data` itself (a carriage return arrives as `&#13;`).

### 5.2 Paths

| Path | Resource |
|---|---|
| `/` | the root: answers `PROPFIND` like `/dav/`, so the URL given to MailFlow can be the bare origin |
| `/.well-known/carddav` | `301` to `/dav/` (no login needed; it reveals nothing) |
| `/dav/` | the DAV root with `current-user-principal` |
| `/dav/principals/me/` | the principal: `principal-URL`, `addressbook-home-set`, `current-user-principal` |
| `/dav/addressbooks/me/` | the home; Depth 1 lists the books |
| `/dav/addressbooks/me/<book>/` | an address book. `<book>` is `contacts` (the default Contacts folder), `f-<16 hex>` (a user contact folder, from a hash of its ServerId) or `directory` |
| `/dav/addressbooks/me/<book>/<uid>.vcf` | a card |

No login name appears in a path: the authenticated user decides whose data is served.

### 5.3 Methods

`OPTIONS` (`DAV: 1, 3, addressbook`), `PROPFIND` (Depth 0 and 1; Depth `infinity` is `403` with `propfind-finite-depth`),
`REPORT` (`addressbook-query`, `addressbook-multiget`), `GET`, `HEAD`. Anything else is `405` with an `Allow` header.
`current-user-privilege-set` says read only. `sync-collection` is neither implemented nor advertised (MailFlow does not use it).
Unknown properties go into a `404` propstat. An `addressbook-query` with a filter that has a `prop-filter` is answered `403` with
`supported-filter`: the bridge does not silently return everything for a filter it does not apply. No filter, or an empty one,
matches every card. Partial `address-data` selections are ignored (the whole card is returned, which a client must accept).

Properties: `resourcetype`, `displayname`, `getctag` (changes whenever any card of the book changes), `getetag`,
`getcontenttype` (`text/vcard; charset=utf-8`), `current-user-principal`, `principal-URL`, `addressbook-home-set`,
`supported-report-set`, `current-user-privilege-set`.

### 5.4 Cards

vCard 3.0, CRLF, values escaped (`\` `,` `;` newline), lines folded at 75 octets without splitting a UTF-8 sequence (tested with
Arabic names). UIDs are stable: personal `eas-` + the first 32 hex of sha256 over (account, folder ServerId, item ServerId),
directory `dir-` + the first 32 hex of sha256 of the lower-case address. The ETag is the quoted sha256 of the vCard bytes. The ctag
of a book is `<creation time in hex>-<counter>`: the counter goes up in every transaction that changes a card of the book, and the
creation time keeps a book that was deleted and made again from repeating a value.

### 5.5 Hardening

TLS 1.2+, `ReadHeaderTimeout` 10 s, `ReadTimeout` 30 s, `WriteTimeout` 60 s, `IdleTimeout` 120 s, `MaxHeaderBytes` 16 KiB, request
body limit 64 KiB, at most 8 requests at once (the ninth gets `503` with `Retry-After`), request XML with a `DOCTYPE` or an
`ENTITY` declaration is rejected. Responses are built by hand with escaped text, so nothing a card contains can break the XML.

### 5.6 Authentication

HTTP Basic through **the same login path as IMAP LOGIN** (`engine.Manager.Login`): the allow-list, the lockout budget, the cooldown
and the cached verification are shared, and a failure counts exactly as an IMAP failure does. `401` with
`WWW-Authenticate: Basic realm="easbridge", charset="UTF-8"` when the credentials are wrong or the user is not allowed. When the
login is refused locally because of the budget or a cooldown after a failure that was not a 401, or Exchange is unreachable, or a
wipe happened, the answer is `503` with `Retry-After`, never `401` (the password may be right; MailFlow reports a 401 as "check the
username and app password").

## 6. Everything else

- **Schema 6**, one appended migration: `contact_folders`, `contact_cards`, `directory_cards`, `directory_candidates`,
  `directory_state`. Version 0.1.8 cannot open a schema 6 database (it refuses a newer schema); rolling back needs the snapshot
  taken before the upgrade. CHANGELOG and [OPERATIONS.md](OPERATIONS.md#limits) say so.
- **Wipe.** A remote wipe and an account-only wipe delete the account's contact folders, cards, directory cards and candidates
  explicitly (secure delete), and then destroy the database as before.
- **`/healthz`** gets counts only: per mailbox, whether the first download of each book is complete and its number of cards, the
  number of directory cards, the length of the lookup queue, the lookups sent today, and until when the directory is paused.
- **Logs and probe output** never contain names, addresses, phone numbers, search queries or vCard data: statuses and counts only.
  The `--trace` redaction list covers the Contacts, Contacts2, GAL and Search pages (an allow-list, so a new element is hidden by
  default).
- **`easbridge probe`** gains a contacts step and a directory step after the mail steps, on by default, `--skip-contacts` to omit
  them. Contacts: the folders by type, one Sync of the default folder (status, number of items, the names of the fields present).
  Directory: a GAL search for the account's own full address, then for its local part (status, Total, entries returned, whether an
  entry's `EmailAddress` matched exactly, the names of the fields present). Never a value. Both start Exchange chains of their own
  and so invalidate the service's contacts SyncKey, which the service recovers from (section 3.2).
- **Version** 0.2.0 everywhere the version consistency test looks.

## 7. Tests

Unit tests for every piece (config, migration 5 to 6 on a copy of a schema 5 database, vCard generation, every CardDAV method,
path, depth and property, authentication and lockout through CardDAV, read-only refusals, limits and hostile XML, contacts sync,
the directory, wipe, health, log privacy, probe output, trace redaction); the wire-level Exchange model of
`internal/easupstream/fakeeas_test.go` and `internal/fakeupstream` gain Contacts Sync and GAL Search and refuse what Exchange
refuses; `compat/` runs MailFlow 3.9.0's real `carddavClient.js` and `vcard.js` (downloaded at test time from the `v3.9.0` tag
into a git-ignored cache, with sha256 pins, never committed) against a devfake bridge; mutation checks are recorded in
`TESTING.md`.

## 8. Decisions beyond the brief, and deviations

The brief's sentence about the search rate was garbled and was corrected by the coordinator during the work: the range of
`EASBRIDGE_DIRECTORY_LOOKUPS_PER_HOUR` is 1 to 180, consecutive searches are always at least 20 seconds apart (that is what caps the
range), and the daily cap is ten times the hourly value. The code follows the correction. Everything below is a decision the brief left
open, or a deviation, with the reason.

- **Authentication.** A failure that is not a wrong password answers `503` with `Retry-After`, not `401` (5.6). `Options.Ready` lets the
  server wait up to 15 seconds for the mailbox's folder hierarchy before it answers, so that the first request after a start does not
  find a mailbox without address books (MailFlow's connect step fails with "No address books found" otherwise).
- **PROPFIND without a Depth header** is treated as `infinity` (RFC 4918 9.1) and refused with 403 `propfind-finite-depth`.
- **Book ids** are `contacts`, `f-<hash>` and `directory` (5.2).
- **Field mapping** follows what MailFlow's `vcard.js` reads: the e-mail type of a personal contact is `OTHER`, a phone has one `TYPE`
  parameter per type (`TYPE=WORK;TYPE=VOICE`), the office is a note (3.4). The contact notes (Body) are not synced: the vendored
  library's contacts sync does not ask for a body and `ContactItem` has no field for it; adding it would change the request that
  Exchange has never answered for contacts.
- **Stale data.** With the feature off the stored contact data is removed at start; with contacts on and the directory off, the
  directory data is (section 2).
- **Directory loop details.** Search spacing and the day's count survive a restart (4.2). Candidates are not noted at ingest while the
  seeding runs (it counts every stored message itself); a changed domain list seeds again with exact counts. The account's own address
  is never looked up. A refusal of one query that is permanent (Search status 2, 4, 8) postpones the candidate a day. **Search statuses
  3, 7 and 10 (and the others of the Search command) feed the directory's own back-off, not the circuit breaker** (a directory that
  times out must not pause the mail); throttling at the HTTP level (429, 503, 5xx) and the common statuses (111 and the like) still
  go through the breaker, so Retry-After is honoured. The directory needs contacts to be on (`EASBRIDGE_DIRECTORY_DOMAINS` alone does
  nothing).
- **Probe.** The two GAL searches of the directory step are 20 seconds apart (the service's minimum, `ProbeConfig.SearchSpacing`
  in tests). A failing contacts or directory step does not stop the next one; the probe then ends with an error.
- **Wipe.** `engine.Manager.SetDestroyHook` exists so that a test can see that the wipe removed the contact data itself when destroying
  the database fails.
- **Test support.** devfake binaries read `EASBRIDGE_FAKE_CONTACTS_INTERVAL` and `EASBRIDGE_FAKE_SEARCH_SPACING` to shorten the two
  timers that the real configuration bounds from below; the production binary does not contain them (`make check-prod`). The
  compatibility harness runs the real `safeFetch.js` and `hostValidation.js` of MailFlow as well (nothing is shimmed) and replaces the
  global `fetch` with undici's, because MailFlow's Agent comes from undici 6.29 and refuses to work with the undici inside a newer Node.

## 9. The full directory sweep (v0.2.1, opt in)

A full copy of the Global Address List in the Directory book, not only the people who appear in the mail, was wanted by the first
user of this feature, who was told the risks: hundreds or thousands of searches against the organization's directory, and Exchange or its administrators
may notice or block a device that does it. So this is **off by default** and the as-you-go lookups of section 4 stay as they are and
keep priority. The decision overrides the sentence of section 1 that the GAL is never enumerated, for this option only: with
`EASBRIDGE_DIRECTORY_FULL=off` (the default) nothing in this section runs and nothing of it is stored.

### 9.1 Configuration

| Variable | Default | Rule |
|---|---|---|
| `EASBRIDGE_DIRECTORY_FULL` | `off` | `off` or `on`. `on` needs the contacts feature (`EASBRIDGE_CARDDAV_ADDR`) and the directory (`EASBRIDGE_DIRECTORY_DOMAINS` not `none`, and not empty); the combination is refused at start-up otherwise. |
| `EASBRIDGE_DIRECTORY_FULL_INTERVAL` | `720h` | From the end of one finished sweep to the start of the next. At least `168h`, at most 365 days. |

### 9.2 The walk

Exchange's ambiguous name resolution (ANR) matches the start of the display name, the first name, the last name and the alias, and
answers at most about 100 entries (Range 0-99) with a Total. Every mail-enabled entry has an alias, which is ASCII, so walking
prefixes of aliases reaches everybody, entries with Arabic display names included. The sweep:

1. **Calibrates** the shortest query Exchange answers, with the account's own entry as the known positive: for L = 1, 2, 3, 4 it
   searches the first L characters of the account's own local part; L is the first length whose search is answered with Total at least
   1 (an answer of nothing, and a refusal, both count as "not answered"). If none up to 4 is answered (or the local part is shorter
   than the next length), the sweep does not run this time: one log line, `calibration_failed` in `/healthz`, the next try after the
   interval. It calibrates again at the start of every sweep.
2. **Walks** every string of length L over the alphabet `a-z 0-9 . _ -` (39 characters), in order, and for each prefix searches once
   (Range 0-99). A prefix is **saturated** if Total is larger than the number of entries returned, or, when Total is missing (reported
   as 0), if 100 entries came back. A saturated prefix is divided: its children (the prefix and each character of the alphabet) are
   queued. A query that Exchange refuses (a permanent Search status about the query itself: invalid, bad link, too complex or too
   short) is divided as well. Prefixes are 20 characters at most, and a sweep makes 5,000 searches at most (calibration included);
   reaching either **ends the sweep early** (the sweep is finished, one log line, `last_sweep_capped` in `/healthz`). For L of 3 or more
   the walk of the starting level alone has more than 5,000 prefixes, so such a sweep is always capped; the strings of the starting level
   are written to the queue in batches of 50, not all at once.
3. **Keeps** every entry returned whose address is in `EASBRIDGE_DIRECTORY_DOMAINS` (exact domain, any case; this drops the external
   mail contacts a GAL lists), once per address (case-insensitive), with the same vCard mapping as section 4.6; HomePhone and Alias are
   never stored. The card is keyed by address in the same Directory book as the as-you-go cards: a person found both ways is one card.

**Known limit.** An entry whose alias is exactly a prefix that more than 100 other entries share (they all start with that alias, and
sort before it by address) is beyond the window of the search for that prefix and is not returned by the sweep; the children of the
prefix do not match it either. Such an entry is still found by the as-you-go lookup of its address if it appears in mail.

### 9.3 Budget and priority

The sweep spends the budget of the lookups: the spacing between two searches (`max(20 s, 1 h / EASBRIDGE_DIRECTORY_LOOKUPS_PER_HOUR)`),
the day's cap (ten times the hourly value), the one-search-in-flight rule, the circuit breaker, Retry-After, the back-off after errors
and the 24-hour pause after access denied or blocked are all the same code and the same counters. Every step of the directory loop
asks the lookups first: the sweep searches only when a search may be sent **and** no candidate from mail is due, so a new address in
mail is looked up before the next prefix. With the defaults (60 an hour, 600 a day) a sweep of 1,500 searches takes about 2.5 days
and one of 5,000 searches (the cap) about 8.5 days; the cap on searches, not the interval, is what bounds the load.

### 9.4 Persistence and removal

Schema 7 holds the state of the sweep (`directory_sweep`: id, phase, calibrated length, counters, times; `directory_sweep_queue`: the
prefixes still to be searched) and, per directory card, how it was found (`by_lookup`, `by_sweep`), the last sweep that saw it
(`sweep_seen`) and when a lookup of its exact address last confirmed it (`confirmed_at`). Every search and its outcome are committed
in one transaction, so a restart resumes at the next prefix instead of starting over (at most the search in flight at the stop is done
again). Schema 6 databases get the migration only; a database of schema 5 goes straight to 7 (0.2.0 was never published).

**Removal.** At the end of a sweep that finished **without** a cap, a card that only sweeps found (`by_sweep`) is removed if neither this
sweep nor the previous uncapped one saw it (`sweep_seen` older than the previous uncapped sweep), unless a lookup of its exact address
confirmed it since that previous sweep started. Nothing is removed after a capped sweep, nor while a sweep is running. (A card seen only
in a capped sweep in between counts as seen after the previous uncapped one, which is more careful than the letter of the brief.) The
as-you-go rule of section 4.4 (two misses a week apart) also stays; when it would remove a card that a recent sweep still sees, the
card stays as a sweep card instead.

**Turning the option off** drops the sweep state at the next start and removes the cards that only the sweep found; cards that a lookup
confirmed stay, as lookup cards.

### 9.5 Observability

`/healthz` (counts only): per mailbox `directory_sweep` with `full`, `calibrated_length`, `calibration_failed`, `state` (`idle`,
`running`, `waiting` with `waiting_until_unix`), `prefixes_done`, `prefixes_queued`, `searches_this_sweep`,
`entries_found_this_sweep`, `last_sweep_unix`, `last_sweep_capped`. Logs: a line when a sweep starts, when it is calibrated, when it
finishes (searches, entries, cards removed) and one when it ends early or cannot be calibrated; never a prefix, a name or an address.
`easbridge probe` gets a `gal prefix` step after `gal local` (skipped by `--skip-contacts`): the first 1 and then the first 2 characters of
the account's own local part, 20 seconds apart: status, Total, entries returned and whether the account's own entry was among
them. Counts only.

### 9.6 Deviations and decisions of 0.2.1

- The brief said a saturated prefix enqueues "prefix + each alphabet character"; the starting level is generated in batches and the
  queue is a table, so that the persisted state stays small for L of 3 or 4.
- A sweep that is capped is "finished" (the next one is scheduled after the interval) but is not a "completed" one: only uncapped sweeps
  count as the previous completed sweep for the removal rule.
- A permanent refusal of a prefix divides it whatever its status (2, 4, 8); other statuses back off like any failed search.
- The fake upstream sorts the matches of a search by address before it cuts the window at 100 (it used to cut first); `internal/testgal`
  generates the test GAL.
