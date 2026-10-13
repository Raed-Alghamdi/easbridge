# Contacts and the company directory

EASBridge serves your contacts to MailFlow as a **read-only** CardDAV address book. Nothing is written back to Exchange. This page
explains where the cards come from, how to connect MailFlow, the optional full sweep of the company directory, and what the CardDAV
server does. The design and its reasons are in [DESIGN-contacts.md](DESIGN-contacts.md).

- [Where do the contacts come from?](#where-do-the-contacts-come-from)
- [Connecting MailFlow 3.9.0](#connecting-mailflow-390)
- [The full directory sweep (opt in, experimental)](#the-full-directory-sweep-opt-in-experimental)
- [The CardDAV server](#the-carddav-server)

## Where do the contacts come from?

Contacts are on by default (`EASBRIDGE_CONTACTS=off` turns them off). MailFlow 3.9.0 has a read-only CardDAV client, so nothing
changes in MailFlow: the bridge serves a CardDAV address book, MailFlow pulls it (every 15 to 1440 minutes, its own setting) and
re-serves the contacts to phones through its own CardDAV server. Two sources feed it.

### The mailbox's Contacts folders

One address book per Contacts folder (EAS folder types 9 and 14), synced by polling every `EASBRIDGE_CONTACTS_INTERVAL`.

- The first download of a book is all or nothing: it lists no cards until the whole folder is in, then they all appear at once.
- If Exchange forgets the sync state (a status 3, which `easbridge probe` also causes, because it starts its own chain on the shared
  DeviceId), the folder is downloaded again and its cards are replaced in one step when that is done.
- Mapped fields: names, email addresses, phones by type, company, department, job title, addresses, birthday, anniversary, web page,
  categories and nickname. The office location is shown as a note, because MailFlow shows only the name, the organisation, the email
  addresses, the phones and the note of a card.
- Not mapped: photos, notes (the library does not ask Exchange for the body of a contact), the government id, the customer id, the
  instant messaging addresses and the manager's name.

### A directory built up as you go

Exchange lets EAS *search* the company directory (the Global Address List, GAL), at most about 100 results per query, but not list
it. So the bridge looks up the addresses that appear in the From, To and Cc of the mail it holds, **one at a time and
rate-limited**, and only for the directory domains. The stored messages are read once in the background after start, and new mail
adds candidates as it arrives (the most recent addresses first, the more frequent among equals).

- **A card is kept only for an entry whose `EmailAddress` equals the address looked for.** Every other person a search returns is
  dropped unread.
- For each address the bridge searches the full address. If no entry matches exactly, it searches the local part, then the display
  name from the message header if there is one: at most three searches per address and attempt.
- The directory is never enumerated or harvested: no wildcard, no letter-by-letter search (unless you turn on the
  [full sweep](#the-full-directory-sweep-opt-in-experimental)).
- An address with no match is not asked again for 30 days. A found card is looked up again after `EASBRIDGE_DIRECTORY_REFRESH` and
  removed when two such lookups at least 7 days apart both find no exact match (the person left).
- HomePhone and Alias are never stored.
- Whether Exchange's search finds an entry by its full SMTP address is not known until the probe has run against the real server.
  `easbridge probe` searches the account's own address and then its local part.

The lookups pause while the circuit breaker is open, honour Retry-After, and back off on throttling, timeouts and server errors. They
run only while the bridge holds the account's credentials (they are part of the mailbox engine, which exists while a client is
connected plus the grace period). They stop for 24 hours (one log line) when Exchange answers access denied or access blocked.

### Turning the contacts on for an install that already ran

The bridge remembers whether the hierarchy it stored (the FolderSync key) was taken with the Contacts folders kept. If the contacts
were off when it was stored, Exchange no longer mentions those folders in an incremental FolderSync. So the first start with the
contacts on reads the whole hierarchy once from key 0. The log says `contacts are on and the stored folder hierarchy was taken
without the Contacts folders: reading the hierarchy again in full, once`, and the next `folder sync done` line shows `full=true` and
`contact_folders_added`.

The mail is not touched: no new UIDs, nothing announced to IMAP clients. If Exchange also restarts its per-folder sync state, the mail
folders are re-read and reconciled quietly. Turning the contacts off and on again repeats this once.

## Connecting MailFlow 3.9.0

`easbridge init` prints these steps with your values, and [INSTALL.md](../INSTALL.md#the-install) has them in full. In short:

1. An administrator of MailFlow turns on **Settings > Security & Integrations > Security > Mail Server Connection Policy > Allow
   private / local hosts**. It is needed for the mail account and for CardDAV alike.
2. In MailFlow open **Settings > Security & Integrations > Integrations > Apps** and find the **CardDAV Contacts** card. Fill in:
   - *Server URL*: `https://easbridge:8443/`;
   - *Username* and *App password*: the mailbox login and password, the same as for IMAP (the bridge checks them with the very same
     rules, allow-list and lockout protection included);
   - *When a contact already exists* and *Sync interval (minutes)* as you like;

   then press **Connect**. MailFlow checks the login, finds the address books and starts a first sync.
3. The contacts appear on MailFlow's Contacts page, one address book per Contacts folder plus the Directory, marked "Synced from
   CardDAV". They are read-only there. If the books are empty at first, the first download is still running: look at the health
   report (`docker exec easbridge /easbridge healthcheck -v`, `contacts.books[].complete`).

MailFlow shows the first part of the organisation, the name, the email addresses with their type, the phones by type and the note.
Everything else on a card (job title, addresses, birthday and so on) is stored by MailFlow with the card and reaches phones through its
own CardDAV server.

## The full directory sweep (opt in, experimental)

> **Experimental.** It works only where the directory answers short queries (one or two characters). Where the shortest query it
> answers is three characters or more, walking every string of that length is not feasible (59,319 searches before any division). The
> bridge then stops the sweep for that run, says why in one log line and in `/healthz` (`sweep_infeasible`), and tries again at its
> next time. It is off by default.

By default the Directory book only holds people whose address appeared in your mail. With `EASBRIDGE_DIRECTORY_FULL=on` the bridge
also copies **the whole company directory** into the Directory book. Only the entries of `EASBRIDGE_DIRECTORY_DOMAINS` are copied: the
external mail contacts a directory lists are left out. Read the risk first, and then decide.

> **Risk, stated plainly.** Exchange can search its directory but not list it, so the sweep asks for it piece by piece: it searches
> one letter or digit, and for every answer that is cut off at about 100 entries, again with each possible next character. For a
> directory of a few thousand people that is some hundreds to a few thousand searches (about 1,300 for 3,000 entries in the tests;
> 1,600 when the server answers only from two characters), at most 5,000 per sweep, repeated every 30 days by default. To the
> organisation's servers this looks like a program enumerating the directory. They may notice it, throttle it, or block the device,
> and the device is the same one that carries your mail, so a block would stop the mail as well. The bridge keeps the pace low (the
> budget below) and stops for 24 hours if Exchange answers access denied, but it cannot make the sweep invisible. Leave it off if you
> are not sure that you may do this.

How it works, in short (details: [DESIGN-contacts.md](DESIGN-contacts.md), section 9):

- **Calibration.** Exchange matches the start of the display name, the first name, the last name and the alias (ambiguous name
  resolution). Every mailbox has an ASCII alias, so a walk over alias prefixes reaches everybody, Arabic display names included.
  First the bridge finds the shortest query Exchange answers: it searches the first 1, 2, 3 and 4 characters of the account's own
  local part (the account's own entry is the known positive) and uses the first length that finds something.
  - If none does, there is no sweep this time: one log line and `calibration_failed` in `/healthz`.
  - A length of 3 or 4 is answered but not walked: the sweep stops itself for the run (`sweep_infeasible` and
    `sweep_infeasible_length` in `/healthz`, one log line), and removes nothing.
- **Walk.** Every string of that length over `a-z 0-9 . _ -`. A prefix that matches more than a search returns (Total larger than the
  entries returned, or 100 entries and no Total) is divided into the prefix plus each character, and so on. A query Exchange refuses
  as too short or too complex is divided as well. At most 20 characters and 5,000 searches per sweep; reaching either ends the sweep
  early (one log line, `last_sweep_capped` in `/healthz`).
- **Pace and priority.** The sweep spends the budget of the lookups, and only what the lookups of addresses from mail leave. A new
  address in mail is looked up before the next prefix. With the defaults (one search a minute, 600 a day) a sweep of 1,300 searches
  takes about two and a half days. The budget is `EASBRIDGE_DIRECTORY_LOOKUPS_PER_HOUR`: searches at least 20 s apart, a daily cap of
  ten times the hourly value, one search at a time, the circuit breaker, Retry-After, back-off, and the 24-hour pause on access
  denied.
- **Restart.** The queue of prefixes is kept in the database, so a restart goes on where the sweep stopped.
- **Removal.** At the end of a sweep that was not cut short, a card that only sweeps found is removed if neither this sweep nor the
  previous finished one saw it, unless a lookup of its exact address confirmed it since the previous sweep started. Nothing is
  removed after a sweep that ended early. Turning the option off drops the sweep's state and removes the cards only it had found at
  the next start; cards that a lookup of the exact address also found stay.
- **Known limit.** An entry whose alias is exactly a prefix that more than 100 other entries share (they all start with that alias)
  is beyond the window of the search for that prefix and is not returned. It is found by the as-you-go lookup of its address if it
  shows up in your mail.
- **Health.** `/healthz` has, per mailbox, `directory_sweep`: `full`, `calibrated_length`, `calibration_failed`, `state` (`idle`,
  `running`, `waiting` with `waiting_until_unix`), `prefixes_done`, `prefixes_queued`, `searches_this_sweep`,
  `entries_found_this_sweep`, `last_sweep_unix`, `last_sweep_capped`. Counts only.
- **Servers differ.** Whether Exchange answers short queries (one or two characters) differs between servers; the probe's `gal prefix`
  steps tell. How a server reacts to a sweep only a run tells.

## The CardDAV server

Hand-written on `net/http`, no dependency.

- **Paths.** `/.well-known/carddav` (301 to `/dav/`), `/dav/principals/me/`, `/dav/addressbooks/me/` (the books),
  `/dav/addressbooks/me/<book>/` (`contacts`, `f-<hash>` for a user folder, `directory`) and `<book>/<uid>.vcf`. `/` answers like
  `/dav/` so the bare origin works as the URL.
- **Methods.** OPTIONS (`DAV: 1, 3, addressbook`), PROPFIND (Depth 0 and 1; Depth infinity is 403 `propfind-finite-depth`), REPORT
  (`addressbook-query`, `addressbook-multiget`; a filter with a `prop-filter` is 403 `supported-filter`, never silently ignored), GET
  and HEAD. **Everything that writes gets 405** with an `Allow` header. Privileges say read only. `sync-collection` is not offered
  (MailFlow does not use it).
- **Cards.** vCard 3.0 with CRLF, escaped values, and lines folded at 75 octets without splitting a UTF-8 sequence. The UID is stable:
  `eas-` and 32 hex digits of a hash of account, folder and item, or `dir-` and 32 hex digits of the hash of the lower-case address.
  The ETag is the quoted SHA-256 of the card. The ctag changes whenever a card of the book changes.
- **Limits.** TLS 1.2 or later over HTTP/1.1, 10 s header and 30 s read timeouts, 16 KiB of headers, 64 KiB of body, at most 8
  requests at once. A DOCTYPE or ENTITY in a request is refused.
- **Login.** HTTP Basic goes through the **same login as IMAP LOGIN**. A wrong password is `401`
  (`WWW-Authenticate: Basic realm="easbridge", charset="UTF-8"`) and counts exactly as an IMAP failure does. A login refused locally for
  any other reason, or Exchange being unreachable, is `503` with `Retry-After`, never a 401.
