# Running EASBridge: first contact, first sync, health and limits

This page is for the days after the install: trying Exchange with `easbridge probe`, checking that EASBridge behaves on your server,
knowing what a first sync looks like, reading the health report, and knowing the limits. For the install itself, see
[INSTALL.md](../INSTALL.md).

- [First contact with Exchange: `easbridge probe`](#first-contact-with-exchange-easbridge-probe)
- [First-run checks](#first-run-checks)
- [First sync](#first-sync)
- [What do the mail listeners support?](#what-do-the-mail-listeners-support)
- [Sent copies and Bcc](#sent-copies-and-bcc)
- [Health](#health)
- [What to expect when something goes wrong](#what-to-expect-when-something-goes-wrong)
- [Limits](#limits)

## First contact with Exchange: `easbridge probe`

The probe is a one-off command that tries your Exchange server with the same device and the same path the service will use. After a
successful probe you can start the service and add the account to MailFlow straight away (see [First sync](#first-sync)).

### What it does

It reads the password **without echo** (or one line from stdin with `--password-stdin`), then runs these steps:

1. OPTIONS and Provision;
2. FolderSync;
3. a 5-item, header-only Sync of the Inbox;
4. one full fetch (size only);
5. a 60 second Ping;
6. contacts, then directory (both on by default; see [below](#the-contacts-and-directory-steps)).

It prints only status codes, counts, sizes and timings, never addresses, subjects, folder names or bodies. It uses the same DeviceId as
the service when it shares `/data`, so it does not create an extra device partnership. It uses the same device description as the
service (the `EASBRIDGE_DEVICE_*` and `EASBRIDGE_USER_AGENT` variables). Nothing else is written locally.

Under every step it lists the HTTP requests of that step with their command, status and where the time went: `new connection` (DNS,
TCP and TLS) or `connection reused`, and how long the server, or a gateway in front of it, took to answer. A slow step can therefore
be told apart: more requests than needed, a slow connection, or a slow answer. A refusal is explained in one line, for example
`Provision status 165 permanent=true: server requires device information in Provision`.

### Running it

Run the probe **before the first start of the service, or with the service stopped.** It provisions the device again and starts fresh
sync chains for the folder list and the Inbox on Exchange, which makes any keys a running service already holds invalid. The service
recovers by itself (status 9 and 3 handling, re-provisioning, then a full resync with UIDs kept), but that costs a complete resync of
the folder list and the Inbox on its next start.

With Docker Compose, the command is in [INSTALL.md](../INSTALL.md#the-install), step 2. Without it:

```sh
docker run --rm -it --network mail-egress -v /srv/easbridge/data:/data "$IMAGE" \
  probe --url https://mail.example.com/Microsoft-Server-ActiveSync --user you@example.com
```

Use the same egress network as the service, so the probe tests the same path (gateway, load balancer) the service will use.

**Every probe run is a real login attempt**, and a wrong password counts toward Active Directory account lockout like any other. Do
not run it in a loop, and do not script it with a password you are not sure of.

Flags: `--url`, `--user`, `--version`, `--data-dir`, `--ping-seconds` (0 skips the Ping), `--ca-file`, `--device-type`,
`--device-model`, `--device-friendly-name`, `--device-os`, `--device-os-language`, `--user-agent`, `--trace`, `--password-stdin` and
`--skip-contacts`.

### Seeing more: `--trace`

`--trace` (off by default) also prints, under each request, the decoded request and response as XML, so that the next protocol error
can be diagnosed from one run. It shows the headers that matter (`MS-ASProtocolVersion`, `X-MS-PolicyKey`; for OPTIONS the versions
and commands the server lists) and every element of the message.

- Values are shown only for protocol structure: keys, statuses, ids of folders, sizes, options, the device description, the policy.
- The value of every other element is replaced by its length and its kind. That covers every message body (`Data`), subject, address,
  folder name, item id, user name and password, and every binary block. For example `[72024 text bytes not shown]` is an inline
  string, and `[16 binary bytes not shown]` is an OPAQUE block (the first real Fetch failed on exactly that difference).
- An element the tables do not know is named `Unknown_<page>_0x<token>` with its value hidden.
- The answer of a gateway that is not WBXML is described by its size.
- Credentials, cookies and the URL are not in the output.

### The contacts and directory steps

After the mail steps the probe has two more, on by default. `--skip-contacts` leaves both out. A failing step is reported and the
probe goes on with the next, then ends with an error. No value is ever printed, with or without `--trace`.

- **contacts:** the folders by type, then one Sync of the default Contacts folder: status, number of items, the *names* of the fields
  present. Like the Inbox sync, this starts a chain of its own on Exchange, which the service recovers from (see above).
- **directory:** a company directory search for the account's own full address, then, 20 seconds later like any two searches of the
  service, for its local part. It prints the status, Total, the entries returned, whether an entry's `EmailAddress` matched exactly,
  and the names of the fields present.
- **`gal prefix 1` and `gal prefix 2`:** a search for the first one and the first two characters of the same local part (also 20
  seconds apart), with status, total, entries returned and whether the account's own entry was among them. These tell whether the
  directory answers the short queries the [full sweep](CONTACTS.md#the-full-directory-sweep-opt-in-experimental) needs.

### If the probe stops

Run it again with `--trace` and read the request that was refused against MS-ASCMD. The statuses met so far:

| Step | Status | Meaning | What helps |
|---|---|---|---|
| sync boot | Sync 4 (protocol error) | The Sync request breaks the specification. Typical cause: the request for a first SyncKey carried `GetChanges` (MS-ASCMD 2.2.3.84); EASBridge sends the SyncKey and the CollectionId only, so a report of this is a bug | `--trace`, and the request in the report |
| fetch 1 mime | "holds no MIME body the bridge can read" | Exchange answered, but the `airsyncbase:Body` of the answer had no `Data` the library reads as MIME. Typical cause: the MIME came as an inline string (WBXML STR_I) instead of an OPAQUE block; EASBridge takes both forms, so a report of this is a bug | `--trace`: the `Body` shows its `Type` (4 is MIME) and whether `Data` is `text` or `binary` bytes |

### If the probe stops at Provision

Every status below is permanent unless noted: the bridge asks once and does not repeat the request. A failed login is remembered like
any failed verification: the same password is refused locally for the cooldown, and at most two unsuccessful verifications per user
reach Exchange in 30 minutes.

| Status | Meaning | What helps |
|---|---|---|
| 165 DeviceInformationRequired | The Provision request had no device information (EASBridge sends it; see [Device identity](CONFIGURATION.md#device-identity)) | Report it as a bug, with `--trace` |
| 139 DeviceNotFullyProvisionable, 141 LegacyDeviceOnStrictPolicy, 145 ExternallyManagedDevicesNotAllowed | A strict device policy refuses a device that cannot apply it, or that sent no policy key | The Exchange administrator must allow the device |
| 129 DeviceIsBlockedForThisUser | The device is blocked or not approved yet | Approve it in the organisation's portal |
| 126, 130, 131 | ActiveSync is disabled for the user, access is denied, the account is disabled | The Exchange administrator |
| 177 MaximumDevicesReached | The user has too many device partnerships | Remove old devices in Exchange |
| 142, 143, 144 (temporary) | The device must provision again | From another command the bridge provisions again itself, once per call, and repeats the call once; as the answer to Provision it is reported |
| 140 or a RemoteWipe element | The server ordered a wipe | The service honours it (see [the security model](SECURITY-MODEL.md#what-does-easbridge-look-like-to-exchange-and-what-does-a-remote-wipe-do)); the probe only reports it |
| 2, 3 (Provision), 3, 4, 5 (Policy) | Protocol error, server error, unknown or corrupt policy, out-of-date policy key | See the line the probe prints; 3 and 5 are temporary |

## First-run checks

Some behaviour cannot be proven without the real server. Check it once, with your own mailboxes, before relying on the bridge for real
correspondence.

1. **Run `easbridge probe`** (above). It covers provisioning, the folder list, one Sync, one fetch and Ping.
2. **Check that Bcc does not leak.** EAS `SendMail` reads its recipients from the message headers, so for a recipient that exists
   only in the SMTP envelope (a Bcc) the bridge writes a `Bcc:` header into the message it hands to Exchange. Exchange is expected to
   strip that header from the copies it delivers, as it does for mail composed in Outlook. It does on the real Exchange 2019 it was
   tested against: the To recipient's copy had no Bcc header (finding 4 in [TESTING.md](TESTING.md#what-a-real-exchange-taught-it)).
   Check it once more on your own server:
   1. From MailFlow, send one message with **To: your first mailbox** and **Bcc: your second mailbox** (both yours).
   2. Open each received copy and view its source. The copy in the To mailbox must contain no `Bcc:` header and not the Bcc address
      anywhere, and the Bcc mailbox must have received the message.
   3. If the To copy shows the header, stop sending through the bridge and report it.
   4. Open the message in MailFlow's Sent folder: it shows the Bcc address, because that copy is the bridge's own (see
      [Sent copies and Bcc](#sent-copies-and-bcc)).

A message submitted twice (MailFlow retries a submission whose answer it never saw) is delivered once: the `ClientId` of the
`SendMail` request is derived from the message's `Message-ID`, so Exchange recognises the repeat (status 118) and the bridge counts it
as sent.

## First sync

While a folder downloads for the first time, MailFlow sees it as an empty folder (LIST shows it; STATUS and SELECT report 0 messages
and a steady UIDNEXT). When the download ends, the whole folder appears in one step, as one `EXISTS`, with UIDs in the order of the
received date. MailFlow then ingests the folder as a fresh backfill, without new-mail alerts for every older message.

There is nothing to wait for before adding the account, but a large folder stays empty until its last message is in. The Inbox is
synced first, then the other folders one after another. Progress is in `/healthz` (`initial_sync_complete`, `initial_sync_pending`,
`staged_items`, and one entry per folder in `folder_progress`).

Details worth knowing:

- A folder opens when its sync has caught up and no message is still waiting for its body. Mail that arrives meanwhile is part of the
  first batch. Afterwards new mail gets the next UID, as usual.
- A message that cannot be downloaded for reasons of its own (Exchange answers with a permanent error status for it, or sends no body;
  five times) stops holding its folder back. The folder opens without it, and the message follows as an ordinary new message once it
  can be fetched. Connection trouble and transient statuses such as "server busy" do not count: however long an outage lasts, the
  folder stays closed. After ten such failures the message is replaced by a small stand-in with the subject, sender, recipients and
  date that Exchange lists for it and a plain note as its body (header `X-Easbridge-Stand-In`), so a message Exchange can never render
  does not stay invisible. It is final.
- If Exchange throws the folder's sync state away during the first download, the download starts again from scratch. After the folder
  has opened, such a reset keeps UIDs and never hides the folder again.
- Some messages need a UID at once, so they get one even in a folder that is still downloading. They sit below the folder's other
  messages. They are Sent copies appended by the client (APPENDUID), drafts appended by the client, and messages moved into a folder
  by MOVE (COPYUID; clients such as MailFlow track the moved message by it).
- The one exception is the Inbox. A message moved into an Inbox that is still downloading waits with the rest, and the MOVE answers
  without COPYUID, because a known UID in the Inbox would make MailFlow alert for everything that follows.
- A Sent message that Exchange delivers during the first download is matched with the client's own copy when the folder opens (same
  rules as later), so it does not appear twice.
- The very first login on a fresh database waits (up to 15 s) until the folder list has been fetched, so that the client's first LIST
  or SELECT does not find an account without an Inbox.
- A folder that appears later, whether created in OWA or by a client's CREATE, starts closed like any other and opens when its first
  download ends, usually at once. A MOVE into a freshly created folder still gets its COPYUID right away, which is what MailFlow's
  Snoozed folder relies on.

## What do the mail listeners support?

- **IMAP** (implicit TLS, port 993): LOGIN and AUTHENTICATE PLAIN, LIST with SPECIAL-USE, NAMESPACE, STATUS, SELECT and EXAMINE, FETCH
  (ENVELOPE, BODYSTRUCTURE, sections), SEARCH, STORE, MOVE with COPYUID, EXPUNGE and UID EXPUNGE, APPEND, CREATE, DELETE and RENAME, and
  IDLE on any folder in several sessions at once. Everything is served from the local database; Exchange is only asked for changes.
  - ENVELOPE carries the subject and the names as the message wrote them. RFC 2047 encoded-words are not decoded: the client does
    that (RFC 3501 section 7.4.2).
  - Sections are the original bytes.
  - SEARCH reads the decoded text of headers and bodies in the charsets go-message knows (windows-1256, ISO-8859-6 and UTF-8 are
    tested, B and Q encoded).
- **SMTP submission** (implicit TLS, port 465): AUTH PLAIN and LOGIN. EASBridge hands the raw message to EAS `SendMail` with
  `SaveInSentItems`. Recipients that exist only in the SMTP envelope (Bcc) are written into a `Bcc:` header first, because EAS reads
  recipients from the headers. The bytes are kept ([Sent copies and Bcc](#sent-copies-and-bcc)).
- **Sync.** FolderSync, and Sync per folder with full MIME. Big messages are completed with ItemOperations before they become visible,
  so BODYSTRUCTURE and sizes are always right. Ping gives push, and a durable outbox carries flag changes and deletes.
- **Drafts** APPENDed by the client stay in the bridge. They are not created on Exchange.

## Sent copies and Bcc

MailFlow APPENDs every message it sends to Sent, and Exchange saves a Sent Items copy too. EASBridge de-duplicates them (same
Message-ID, else subject, date and recipients), so the folder shows one copy with one UID.

The difficulty is Bcc. EAS `SendMail` takes its recipients from the message headers, so the bridge writes the recipients that exist
only in the SMTP envelope into a `Bcc:` header before it hands the message to Exchange. Exchange delivers the message without that
header and saves its Sent Items copy without it too. That copy comes back through EAS as MIME with no Bcc header, while Outlook shows
the Bcc recipients of a sent message. MailFlow looks at a new UID once and does not look again, so the Bcc recipient has to be there
from the first FETCH. So a message submitted with a Bcc header is served as the bridge submitted it, from the moment MailFlow APPENDs
it: the Sent folder shows the Bcc recipients on the same UID, and nothing new is announced when Exchange's copy arrives.

- **What is kept.** A message submitted with a Bcc header (and a Message-ID) is kept in `state.db`, as submitted, **before** the
  request leaves for Exchange (sending takes seconds, and Exchange's copy can be picked up before the reply). The Sent copy, whichever
  comes first, is served with those bytes. That covers the placeholder MailFlow's APPEND creates, Exchange's own copy if it was there
  first (also while the Sent folder is still doing its first sync, also for a message that had to be downloaded), and a placeholder
  that binds to Exchange's copy without having them. The UID, the flags and the ServerId binding are the same as ever, so flags, moves
  and deletes act on Exchange's item, and no client is told anything: no new EXISTS, no new UID.
- **When it is dropped.** The kept copy is a way station. It is dropped once the message that serves it is bound to Exchange's item,
  when that message is deleted or moved out of Sent Items, when the Sent folder opens with the message staged, after 24 hours, and
  beyond 100 messages or 128 MiB (oldest first). It is part of the state database: overwritten when deleted (`secure_delete`), gone
  with a remote wipe.
- **Move and delete.** IMAP MOVE of the Sent message to another folder keeps serving the bytes there (MOVE copies the stored message).
  `\Deleted` plus EXPUNGE does not: Exchange moves the item to Deleted Items itself and renders it without a Bcc header. Messages that
  did not pass through the bridge, messages without a Bcc header (Exchange's copy lists the same recipients) and messages without a
  Message-ID show what Exchange renders.
- **Messages sent from a phone or OWA** do not pass through the bridge. MS-ASEMAIL 2.2.2.9 defines an `email2:Bcc` element (protocol
  16.0 and 16.1). The specification uses it for drafts (3.1.5.4) and lets any Sync (3.2.5.4) or ItemOperations (3.2.5.2) response
  carry any element of the Email class. It does not say that Exchange lists the Bcc recipients of a sent message, and none has been
  seen yet. If Exchange does, the bridge writes them into a `Bcc:` header of the copy it serves (Sent Items only, never over a Bcc
  header the message has, never for a message it submitted itself) and logs one line the first time (`Exchange lists the Bcc
  recipients of a message in Sent Items`). Nothing is requested for it. If that line never appears, a message sent elsewhere shows
  Exchange's rendering, without Bcc recipients.

## Health

The image's `HEALTHCHECK` runs `/easbridge healthcheck` (the image has no shell or curl). It asks `http://127.0.0.1:8081/healthz`.
To read the report yourself, run:

```sh
docker exec easbridge /easbridge healthcheck -v
```

It prints the JSON (states and counts, no mail content) and exits 0 when the bridge is healthy. Docker runs the same command without
`-v`. `GET /healthz` is loopback only by default and answers HTTP 503 when a listener is down or the bridge was wiped.

What the report holds:

- **The bridge:** `version`, `status`, `imap_listening`, `smtp_listening`, `wiped`, and with contacts on `carddav_listening`. The
  certificate: `tls_mode` and `tls_server_expires_in_days`.
- **Per mailbox**, identified by a short tag, not the address:
  - `folders`, `pending_fetch`, `outbox`, `syncing` (folders still waiting for their sync), `sessions`, `last_sync_unix`,
    `ping_heartbeat_s` and `auth_failed`;
  - the circuit breaker: `paused`, `paused_until_unix` and `pause_reason` while it is open, and `pause_trips`;
  - the first-sync progress (counts only, no folder names): `initial_sync_complete` and `initial_sync_pending` (numbers of folders),
    `staged_items` (messages downloaded but not visible yet) and `folder_progress` (one `{initial_sync_complete, staged,
    pending_fetch}` entry per folder).
- **Contacts, when on**, per mailbox under `contacts`: `books` (per book `complete`, `downloading`, `cards`), `directory_enabled`,
  `directory_cards`, `directory_queue` (candidates waiting for a lookup), `directory_lookups_today` and, while paused,
  `directory_paused_until_unix`. The full sweep adds `directory_sweep` ([fields](CONTACTS.md#the-full-directory-sweep-opt-in-experimental)).
  Counts only, no names.

## What to expect when something goes wrong

- **A message that cannot be downloaded** stays invisible and is retried with a back-off. It is logged at error level after five
  attempts, once more if it stops holding its folder's first sync back, and a last time when it is replaced by a stand-in (see
  [First sync](#first-sync)).
- **The circuit breaker is open.** Nothing leaves for Exchange. Clients get `NO [UNAVAILABLE]` for commands that need Exchange and
  keep reading the local cache. The log says why and for how long (one line per trip). See
  [the security model](SECURITY-MODEL.md#what-happens-when-exchange-is-slow-or-failing).
- **Exchange answers 401 mid-session.** All Exchange traffic stops until a client logs in with a working password; see
  [what stops traffic](SECURITY-MODEL.md#what-stops-traffic-during-a-session).
- **Ping keeps failing**, for example a proxy that cuts every held request even at the shortest heartbeat. The bridge logs a warning
  and syncs every mail folder every 5 minutes instead, so new mail still arrives, only later. It returns to push as soon as a Ping
  succeeds. The same sweep covers folders that a server-side limit keeps out of the Ping set.
- **Exchange silently resets a sync key.** The folder is resynced, and messages deleted in the gap are removed only after two complete
  resyncs agree (a verification resync runs 5 minutes after the first).

## Limits

The ones that matter most are in the [README](../README.md#limits). The full list:

**What is not supported**

- No calendar, tasks or notes. Contacts are **read-only** (no write-back, no photos, no contact notes), and exist only while
  `EASBRIDGE_CONTACTS` is `on`.
- Mail folders are served over IMAP only (Outbox is hidden).
- One Exchange server per bridge.

**Mail behaviour**

- **COPY is refused** (`NO [CANNOT]`); EAS has no copy. MOVE works and returns COPYUID.
- No CONDSTORE, QRESYNC, COMPRESS, ID, ESEARCH, SORT, THREAD or server-side search. SEARCH is evaluated locally over the cache.
  MailFlow treats the missing CONDSTORE as "no baseline" and does full flag scans, which are answered from the local database.
- `\Answered`, `$Forwarded` and other keywords are stored locally only (go-activesync discards Exchange's LastVerbExecuted).
  `\Deleted` is local until EXPUNGE.
- A delete that Exchange refuses brings the message back, as a new message with a new UID (the received date and the read and flagged
  state are kept).
- Widening `EASBRIDGE_FILTER` on an installation that has already synced brings older mail in as new messages with higher UIDs, which
  MailFlow can treat as new mail. Choose the window before the first start.

**Drafts and the Sent folder**

- **Drafts are local.** Drafts APPENDed by the client live only in the bridge, not on Exchange, and are lost with the data directory.
  Drafts created elsewhere (OWA, phone) do sync in.
- Unmatched Sent placeholders (Exchange never delivered its copy) stay as local-only messages after 24 hours.
- A Sent copy is matched with the client's by Message-ID. The fallback of subject, date and recipients is used only when one of the two
  has no Message-ID, so two different messages that look alike are never merged. If Exchange rewrites the Message-ID of its copy, the
  Sent folder shows both copies.
- The Sent folder shows the Bcc recipients of messages that were sent through the bridge (see
  [Sent copies and Bcc](#sent-copies-and-bcc)). Messages sent earlier, messages sent from a phone or OWA (unless Exchange lists their
  Bcc recipients, which has not been seen) and the copy that Exchange moves to Deleted Items when a Sent message is expunged show
  Exchange's rendering, without them.

**Contacts and the company directory**

- Servers differ in what their company directory answers: whether a search finds an entry by its full SMTP address (the three-search
  ladder exists because some do not), and whether it answers short queries (the full sweep depends on that). The probe tells for your
  server. How long the download of a large Contacts folder takes through a gateway also depends on it (windows of 100 items, photos
  included).
- Without the sweep, the directory only knows people whose address appeared in mail the bridge holds, one search at a time. A new
  correspondent is in the directory after the next lookup, which with the default 60 lookups an hour can be a while when many
  addresses are new.

**Upgrades**

- A database migrated by a newer version cannot be opened by an older one. Rolling back needs the copy of the data directory taken
  before the upgrade.
