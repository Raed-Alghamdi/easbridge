# EASBridge design (v0.1 MVP)

This is the design of the first version (v0.1), kept for the reasoning behind it. Later versions are described in
DESIGN-contacts.md and DESIGN-v0.3.md and in the CHANGELOG; where this document and the code differ, the code and the later
documents are right.

## 1. Goal

A small, self-hosted gateway that lets an ordinary IMAP/SMTP client (MailFlow) use one Exchange mailbox that only allows
Exchange ActiveSync (EAS). Target: a mailbox on an on-premises Exchange (2016 or 2019) that only allows ActiveSync, usually with Basic
authentication and often behind a reverse proxy or load balancer (`https://mail.example.com/Microsoft-Server-ActiveSync`; IMAP, SMTP and MAPI
closed, EWS filtered). Like DavMail, but speaking EAS upstream.

Non-goals for v0.1: calendar, contacts, tasks, notes, S/MIME, IMAP COPY of server data, server-side search, multiple Exchange servers.

## 2. Architecture

```
MailFlow backend ──IMAP──▶ easbridge ──EAS (HTTPS, Basic)──▶ mail.example.com (load balancer ▶ Exchange 2019)
                 ──SMTP──▶          │
                                    └── /data/state.db (SQLite: folder map, UID map, flags, envelopes, MIME cache)
```

Single Go binary, three parts:

1. **EAS account engine** (one per authenticated mailbox, created on first successful login, kept alive while any IMAP/SMTP session
   for it is open plus a grace period, default 30 min). Owns all EAS traffic for that mailbox and serialises it (one worker goroutine;
   EAS forbids concurrent Sync on a collection with the same SyncKey). Runs: Provision → FolderSync → Sync per folder → Ping loop.
2. **IMAP server** (`github.com/emersion/go-imap/v2/imapserver`). Reads from the local state DB; mutations are queued to the engine.
   Unilateral updates (EXISTS/EXPUNGE/FETCH FLAGS) are pushed to selected/IDLE sessions via the library's trackers.
3. **SMTP submission server** (`github.com/emersion/go-smtp`). Accepts a message and hands raw MIME to EAS `SendMail` with `SaveInSentItems`.

Libraries: `github.com/hstern/go-activesync/eas` (MIT; young, tested only against Z-Push/SOGo, so treat as a dependency we may need to patch or vendor),
`github.com/emersion/go-imap/v2`, `github.com/emersion/go-smtp`, `github.com/emersion/go-message` (MIME parsing for ENVELOPE/BODYSTRUCTURE),
`modernc.org/sqlite` (CGO-free). Static build, distroless/nonroot image.

## 3. Authentication and credentials (privacy-critical)

- **Pass-through, never persisted.** IMAP `LOGIN`/`AUTHENTICATE PLAIN` and SMTP `AUTH PLAIN/LOGIN` username+password are used as the EAS Basic
  credentials. They live only in process memory inside the account engine. They are never written to disk, logs, metrics, or error strings.
- **Allow-list.** Only usernames listed in `EASBRIDGE_ALLOWED_USERS` (comma-separated; v0.1: the one mailbox address) are accepted; anything else is
  rejected locally without contacting Exchange (prevents the bridge being used as a password oracle against other accounts).
- **Lockout protection (important).** An AD account can be locked by repeated bad passwords. On an EAS 401 for a (user, password-hash) pair,
  cache the failure (salted hash in memory only) and reject identical retries locally for `EASBRIDGE_AUTH_FAIL_COOLDOWN` (default 15 min).
  Never retry a 401 automatically. Successful credentials are verified once per engine start (Provision/FolderSync), not per IMAP command.
- **Device identity.** Persist a random `DeviceId` (32 hex) and the Provision `PolicyKey` in the state DB (these are device tokens, not user
  secrets). `DeviceType`/`User-Agent` are honest and configurable (defaults `easbridge` / `easbridge/<version>`); do not impersonate Outlook.
- **Policies.** EAS Provision policies are acknowledged so the server will sync (the bridge cannot enforce PIN/encryption on a server).
  **Remote wipe is honoured**: on a wipe command, acknowledge, delete the state DB and MIME cache, refuse further logins until the operator
  deletes `/data/WIPED`, and log one loud line.

## 4. Mailbox model

### Folders
- From EAS `FolderSync`. Mail folder types only (1 user-created generic? 2 Inbox, 3 Drafts, 4 Deleted Items, 5 Sent Items, 6 Outbox, 12 user mail).
- IMAP names: Inbox → `INBOX`; others by display path with `/` delimiter; SPECIAL-USE: Drafts `\Drafts`, Deleted Items `\Trash`,
  Sent Items `\Sent`, Junk Email (detect by well-known name/type if exposed) `\Junk`, Archive `\Archive` if present. Outbox hidden.
- Folder create/rename/delete from IMAP map to EAS `FolderCreate/FolderUpdate/FolderDelete` (v0.1 may return `NO [CANNOT]` if risky).

### Messages and UIDs
- Each (folder, EAS ServerId) gets an IMAP UID from a per-folder counter; `UIDVALIDITY` is created per folder and only changes if the mapping
  is lost or the folder is recreated. On EAS `InvalidSyncKey` (Status 3) resync the folder and re-match by ServerId, keeping UIDs where the
  ServerId is unchanged; only bump UIDVALIDITY if matching is impossible.
- EAS `MoveItems` returns a new ServerId → new UID in the destination (report COPYUID/MOVE responses accordingly).
- Sync window: `EASBRIDGE_FILTER` (`all` default, or `1d|3d|1w|2w|1m`, the email FilterType values).
- Bodies: request MIME (`BodyPreference Type=4`, `MIMESupport=2`) with a truncation threshold (`EASBRIDGE_MIME_INLINE_MAX`, default 512 KiB).
  Complete MIME is cached; truncated items record `EstimatedDataSize` and are fetched in full with `ItemOperations Fetch` on demand
  (when a client asks for BODY[], BODYSTRUCTURE, or RFC822.SIZE accuracy matters).
- ENVELOPE/BODYSTRUCTURE are computed from the cached MIME with go-message; INTERNALDATE = EAS `DateReceived`.

### Flags
| IMAP | EAS | Direction |
|---|---|---|
| `\Seen` | `Read` | both |
| `\Flagged` | `Flag/Status=2` (clear → status 0) | both |
| `\Answered` / `$Forwarded` | `LastVerbExecuted` (if exposed) | server → client only |
| `\Draft` | items in Drafts | server → client only |
| `\Deleted` | local only until EXPUNGE | client |
| keywords (`$Junk`, custom) | none | stored locally in state DB only (PERMANENTFLAGS advertises them only if MailFlow needs them) |

### Mutations
- STORE flags → queued EAS `Sync` client `Change`; optimistic local update, rollback + FETCH FLAGS if EAS rejects.
- MOVE (RFC 6851) → EAS `MoveItems`. COPY → `NO [CANNOT]` unless the scout report shows MailFlow needs it (EAS has no copy).
- EXPUNGE / UID EXPUNGE of `\Deleted` → EAS `Sync` `Delete` with `DeletesAsMoves=0` when in Trash, `1` elsewhere (configurable).
- APPEND: Drafts → (v0.1) store locally as a bridge-only draft or EAS 16.x draft Add if the library supports it; Sent → accepted and
  de-duplicated against the copy Exchange saves via `SaveInSentItems` (exact behaviour depends on whether MailFlow APPENDs after sending);
  other folders → `NO [CANNOT]`.
- SEARCH → evaluated locally over the state DB (UID/sequence sets, flags, dates, header fields, TEXT/BODY over cached MIME).

### Push
- EAS `Ping` over all synced mail folders (or Inbox + a configurable list). `HeartbeatInterval` default 480 s, adaptive down to 120 s if the
  the load balancer or Exchange drops held requests early. On Ping status 2 → Sync the changed folders → update DB → notify IMAP sessions.
- If no IMAP session is IDLE/selected, the engine still keeps Ping alive during the grace period so new mail is ready immediately.

## 5. SMTP submission

- `AUTH PLAIN`/`LOGIN` only, required before `MAIL FROM`. Same allow-list and lockout protection as IMAP.
- **Bcc handling:** EAS `SendMail` takes recipients from the MIME headers. Recipients given only as `RCPT TO` (Bcc) must be added as a
  `Bcc:` header before upload (Exchange strips it on delivery). Envelope-only recipients that already appear in To/Cc are not duplicated.
- `SaveInSentItems=true`. Max size `EASBRIDGE_MAX_MESSAGE_BYTES` (default 35 MiB, match MailFlow/Exchange limits).
- On EAS errors return a permanent (5xx) or temporary (4xx) SMTP code that matches the EAS status so MailFlow reports it correctly.

## 6. Network, TLS, logging

- Listens only on the private Docker network shared with MailFlow's backend (no host ports). IMAP 143/993 and SMTP 587/465 according to the
  TLS decision (to be fixed after the scout reports: plaintext on an isolated internal network, or implicit TLS with a mounted certificate
  whose name matches a Docker network alias, so MailFlow need not relax certificate checks globally).
- Logs: structured, one line per EAS command with status code and duration; never credentials, never subjects/addresses/bodies at INFO.
  DEBUG may include EAS status XML but must redact `Data`, `Subject`, `From`, `To`, `Cc`, `Bcc`, and Basic auth headers.
- Health: `/healthz` on a separate loopback/internal port reporting engine state (no mailbox content).

## 7. Configuration (env)

`EASBRIDGE_EAS_URL`, `EASBRIDGE_ALLOWED_USERS`, `EASBRIDGE_EAS_VERSION` (prefer `16.1`, fall back to `14.1`),
`EASBRIDGE_DEVICE_TYPE`, `EASBRIDGE_USER_AGENT`, `EASBRIDGE_FILTER`, `EASBRIDGE_MIME_INLINE_MAX`, `EASBRIDGE_PING_HEARTBEAT`,
`EASBRIDGE_IMAP_ADDR`, `EASBRIDGE_SMTP_ADDR`, `EASBRIDGE_TLS_CERT`, `EASBRIDGE_TLS_KEY`, `EASBRIDGE_DATA_DIR` (`/data`),
`EASBRIDGE_LOG_LEVEL`, `EASBRIDGE_AUTH_FAIL_COOLDOWN`, `EASBRIDGE_ENGINE_GRACE`, `EASBRIDGE_MAX_MESSAGE_BYTES`.

## 8. Verification plan

1. Unit tests: UID/UIDVALIDITY mapping and resync matching, flag mapping, Bcc injection, auth lockout cache, log redaction, SEARCH evaluator.
2. Integration: run the go-activesync test stack (Z-Push or SOGo) in an isolated Docker network on a test host, drive the bridge with a
   scripted IMAP/SMTP client (go-imap client): login, LIST, SELECT, FETCH, STORE \Seen/\Flagged, MOVE, EXPUNGE, IDLE wake-up on new mail,
   send with Bcc. Test credentials only (generated for the test server), never a real person's.
3. MailFlow end-to-end against the same test stack: add a custom IMAP/SMTP account pointing at the bridge, confirm sync, flags, move, delete,
   send, live notification.
4. Production: the operator adds the account in MailFlow with their own password. Check bridge logs for EAS status codes,
   device approval/quarantine, Ping stability through the load balancer, and first full sync time.

## 9. MailFlow 3.8.3 compatibility requirements (from code analysis; these override earlier sections where they differ)

Client stack: MailFlow backend uses `imapflow` 1.7.8 (IMAP) and `nodemailer` 10 (SMTP). References are to the MailFlow repo.

### Transport and auth
- **IMAP on port 993, implicit TLS. SMTP on port 465, implicit TLS.** MailFlow picks TLS mode from the port; plaintext/143 would require
  MailFlow's instance-wide `allow_insecure_tls`, which also weakens SMTP STARTTLS protection for every account, so we do not use it.
- **Certificate:** leaf `DNS:easbridge` signed by a private CA. Server files mounted into the bridge: `/tls/server.pem`, `/tls/server-key.pem`
  (from a host directory). MailFlow trusts the CA via `NODE_EXTRA_CA_CERTS`. MailFlow connects to host `easbridge`
  on the private Docker network `mail-internal`; its admin setting `allow_private_hosts` must be on (an administrator turns it on).
- Auth: imapflow prefers `AUTHENTICATE PLAIN`, then `AUTH=LOGIN`, then `LOGIN`. Support `AUTHENTICATE PLAIN` and `LOGIN`.
  SMTP: nodemailer picks PLAIN, then LOGIN. Same username/password for IMAP and SMTP.
- **LOGIN volume:** one persistent sync session + up to 4 pooled sessions per account, plus a fresh LOGIN every ~60 s for the STATUS cycle.
  LOGIN must be cheap: if an engine for that user exists and the password matches its in-memory verified credential (compare salted hashes,
  constant time), accept immediately without contacting Exchange. Only a new/different password triggers one EAS verification
  (subject to the lockout cache in section 3).

### Capabilities to advertise (advertise ONLY what is implemented)
`IMAP4rev1 IDLE UIDPLUS MOVE SPECIAL-USE NAMESPACE ID LITERAL+ AUTH=PLAIN` (plus `CHILDREN` if trivial).
Do **not** advertise `ENABLE`, `CONDSTORE`, `QRESYNC`, `COMPRESS=DEFLATE`, `IMAP4rev2`, `UTF8=ACCEPT`, `LIST-STATUS`, `ESEARCH`, `SORT`, `THREAD`
(imapflow sends `ENABLE CONDSTORE UTF8=ACCEPT IMAP4rev2` and COMPRESS when offered; an `ENABLED IMAP4REV2` reply switches it to rev2 semantics).
BAD replies to ID/NAMESPACE/LSUB are tolerated, but implementing ID and NAMESPACE is better.

### Behaviour that must hold
- **IDLE on any folder, several sessions at once.** imapflow auto-IDLEs every SELECTED session after 15 s of quiet, so pooled sessions IDLE on
  Sent, Junk, etc. Every IDLE session must receive EXISTS/EXPUNGE/FETCH(FLAGS) for its mailbox, and any command must end IDLE promptly.
  The sync session re-IDLEs every 25 min.
- **STATUS must be exact and fast** (answer from the DB): `MESSAGES`, `UNSEEN` (≤ MESSAGES), `UIDNEXT` (> 0), numeric `UIDVALIDITY`.
  MailFlow runs a STATUS cycle every 60 s over ~6 folders; count changes trigger a folder pass, and UNSEEN changes are how it notices
  read-state changes made elsewhere (phone/OWA) when there is no CONDSTORE.
- **UIDs strictly increase per folder.** MailFlow treats `UID > maxKnownUid` as new mail. A UIDVALIDITY change makes it purge and re-download
  the folder, so avoid changing it.
- **New-mail FETCH** is `UID FLAGS BODYSTRUCTURE ENVELOPE INTERNALDATE RFC822.SIZE BODY.PEEK[HEADER]`, then bodies via `BODY.PEEK[n]`
  (a background indexer asks for up to 8 parts per message). So full MIME must be available for BODYSTRUCTURE at sync time: request full,
  untruncated MIME in EAS Sync (small WindowSize to bound response size); fall back to ItemOperations Fetch for anything truncated.
  Absent sections must return NIL, never BAD. Commands time out after 30 s in MailFlow: SELECT/STATUS/SEARCH/FETCH from the local DB.
- **SEARCH criteria used:** `ALL`, UID sets, `UID n:*`, `HEADER Message-ID <id>`, `DELETED`, nested `OR` of `HEADER In-Reply-To`/`HEADER References`.
  Store Message-ID, In-Reply-To and References per message (indexed) and evaluate locally.
- **Flags written by MailFlow:** `\Seen`, `\Flagged`, `\Deleted`, `\Draft`. No keywords: PERMANENTFLAGS = `\Seen \Flagged \Deleted \Draft \Answered`.
- **MOVE** is used when advertised (else COPY+delete). Return COPYUID in MOVE responses (UIDPLUS). COPY may return `NO [CANNOT]`.
- **Special folders:** advertise SPECIAL-USE flags on LIST (`\Sent`, `\Drafts`, `\Trash`, `\Junk`, `\Archive`); detect Exchange's Junk/Archive
  by well-known names (English and Arabic) since EAS types don't mark them. Spam actions MOVE to `\Junk`.
- **CREATE** is used for `Snoozed` (snooze aborts if CREATE fails) and user folders → EAS FolderCreate (type 12 under the root or parent).
  DELETE/RENAME → FolderDelete/FolderUpdate.
- **Sent copies:** MailFlow APPENDs every sent message to `\Sent` (not configurable) after SMTP. With `SaveInSentItems=true` Exchange also saves
  one. Dedupe: on APPEND to Sent, if a Sent item with the same Message-ID exists, return APPENDUID of that item and store nothing; otherwise
  create a local placeholder with a new UID, and when Exchange's copy arrives via Sync with the same Message-ID (fallback: same Subject + Date
  within 10 min + same To), bind the server item to the placeholder's UID (no new UID, no EXISTS). Unmatched placeholders become local-only
  after 24 h. Unit-test this.
- **Drafts:** MailFlow APPENDs drafts with `\Draft \Seen`, then deletes the previous draft UID after a Message-ID check. A missing Drafts
  folder breaks drafting (HTTP 422). v0.1: drafts APPENDed by MailFlow are stored locally (bridge-only items in the Drafts mailbox, sharing
  the folder's UID sequence with server drafts); server drafts from Exchange still sync in. Deleting a local draft is local.
- Reconnect behaviour: auth failures make MailFlow back off 5 min → 6 h; connect refusals 30 s → 15 min. Integrity checks compare server
  listings and only delete rows absent in two consecutive views.

## 10. Deployment shape

- A Docker container (`local/easbridge:<version>` built from the repository, or the published image), created from a compose file or a `docker run`
  command (the container-panel templates of NAS systems map the same variables, paths and options).
- Networks: a private one (internal, a fixed subnet; the MailFlow backend reaches `easbridge`) and an egress network that can reach the Exchange
  server. No host ports.
- Data: a volume or host directory at `/data` (state DB, MIME cache). TLS: files at `/tls` (read only), or, since 0.3, a certificate the bridge
  makes for itself in `/data/tls`.
- Run as uid 65532 (distroless nonroot), `--read-only`, `--cap-drop=ALL`, `no-new-privileges`, log opts `max-size=50m max-file=3`.

## 11. Open questions (resolve from library notes)

- Does go-activesync expose full-MIME Sync, ItemOperations Fetch, MoveItems, SendMail raw MIME, Ping, FolderCreate/Update/Delete and
  persistable state (SyncKeys, PolicyKey) as needed? Gaps are filled by patching a vendored copy, with tests.
- Does go-imap v2's server support IDLE, MOVE, UIDPLUS (APPENDUID/COPYUID), SPECIAL-USE, AUTHENTICATE PLAIN and multi-session update
  delivery adequately, and can capability advertisement be restricted as required above?

## 12. Implementation decisions (from reading the library sources; these override earlier sections where they differ)

### Libraries and patches
- **go-activesync v1.1.0 is vendored** under `third_party/go-activesync/` (MIT, keep LICENSE and a `PATCHES.md` listing every change) and
  wired with a `replace` directive. Patches, each with its own unit test:
  1. Provision: treat Policy `Status=2` ("no policy defined") as success with no policy (upstream issue #21).
  2. Provision: surface RemoteWipe requests to the caller (typed error or callback) so the bridge can honour them (section 3).
  3. Sync: allow `DeletesAsMoves=0` per call (hard delete only when expunging inside Trash; `1` elsewhere).
  4. Sync: allow omitting FilterType entirely (EASBRIDGE_FILTER=`all` → no filter) and allow disabling/raising the fixed 32 KiB truncation
     so MIME (Type 4, MIMESupport=2) can come inline up to `EASBRIDGE_MIME_INLINE_MAX` (default 1 MiB).
  5. ItemOperations Fetch: send `MIMESupport=2` when MIME is requested (harmless if Exchange ignores it).
  Prefer small, clearly marked patches; record upstream-able diffs in PATCHES.md.
- **go-imap pinned to `v2.0.0-beta.8`**, go-smtp v0.25.0, go-sasl, go-message, modernc.org/sqlite v1.60.1 (exact versions in go.mod).
- go-imap advertises `IDLE UNSELECT ENABLE UTF8=ACCEPT SASL-IR LITERAL-` automatically with IMAP4rev1. That is acceptable **only if** an
  `ENABLE CONDSTORE UTF8=ACCEPT IMAP4rev2` from imapflow never yields `CONDSTORE` or `IMAP4REV2` in `ENABLED` (Caps must not contain
  IMAP4rev2). Verify with the imapflow harness; patch go-imap only if needed.
- **ID:** go-imap has no ID handler and an unknown pre-login command gets `BYE` + disconnect. Check imapflow 1.7.8 (extracted for reading) to
  confirm it only sends ID when the server advertises ID. If it does, do not advertise ID and add no handler. If it sends ID regardless,
  vendor go-imap and add a minimal ID handler (respond `* ID NIL`).
- Capability mismatches in go-imap panic at connection start: cover the exact Caps set in a test.

### EAS engine rules
- State store implements go-activesync's `StateStore` with **staged keys**: `SetSyncKey`/`SetPolicyKey` are staged in memory; the engine
  commits staged keys in the same SQLite transaction as the items/changes they belong to (at-least-once; dedupe by ServerId on replay).
  When a folder's key is reset to "0" (silent status 3 reset), mark the folder for reconciliation and remove local items the completed full
  resync no longer lists (two consecutive confirmations, mirroring MailFlow's own caution).
- FolderSync status 9 → reset folder hierarchy key to "0" and retry once. Sync status 12 → FolderSync then Sync. Ping/Sync status 14 →
  adopt the server's limit. Ping: read `res.Status` even when err is set; status 7 → FolderSync; status 5 → adjust HeartbeatInterval;
  HTTP client timeout > heartbeat + 60 s.
- Protocol version: prefer `EASBRIDGE_EAS_VERSION` (default 16.1). Run one OPTIONS (NegotiateVersion) at engine start before any
  concurrent use; fall back to 14.1 if 16.1 is not offered. Reject `http://` server URLs.
- One sync goroutine per folder at most; one Ping goroutine; ItemOperations fetches on a separate goroutine with concurrency 2.
- Root folder-key writes must be reliable (the library ignores their errors): verify after write; stop the engine loudly on failure.
- New messages become visible to IMAP (UID assigned, EXISTS sent) **only after their full MIME is cached**, so BODYSTRUCTURE is always
  correct. Initial backfill fetches newest first.

### Architecture for testability
- Define an internal interface (e.g. `upstream.Mailbox`) between the bridge core (store, IMAP, SMTP) and EAS. Implement `easupstream`
  (real, go-activesync) and `fakeupstream` (in-memory, deterministic, can inject new mail, flag changes, deletions, moves, failures).
  Core tests and the imapflow harness use the fake; it is compiled only with build tag `devfake` (never in the production image).
- `easbridge probe` subcommand for the first real-Exchange check (the operator runs it): reads the password from the terminal without echo
  (or `--password-stdin`), runs OPTIONS, Provision, FolderSync, a 5-item header-only Sync of Inbox, one FetchEmail (reports size only), and
  a 60 s Ping; prints only status codes, counts, sizes and timings, never addresses, subjects or bodies. Uses the same DeviceId as the
  service (shared `/data`), so it does not create an extra device partnership.

### Logging
- Never set go-imap `DebugWriter` or go-smtp `Debug` in production. Wrap go-activesync's logger to drop the `User` query parameter.
