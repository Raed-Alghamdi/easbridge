# How EASBridge is tested, and what a real Exchange taught it

EASBridge speaks a protocol (Exchange ActiveSync) to a server that most contributors cannot reach, and an old one (IMAP) to clients
whose behaviour matters more than the specification. So the test strategy is a stack of stand-ins, each as strict as the thing it
replaces, plus a rule that every surprise from the real server becomes a test.

## The layers

1. **Unit tests** for the pure parts: UID and UIDVALIDITY mapping, flag mapping, the SQLite store and its migrations (each migration is
   run on a copy of a database of the previous schema), MIME structure and ENVELOPE, address and header decoding in several charsets, vCard
   writing, configuration parsing and validation, the certificate authority code, `init` and its generated files (golden files).
2. **A fake upstream** (`internal/fakeupstream`, compiled only with the `devfake` build tag, so the production binary cannot contain
   it; `make check-prod` greps the binary for it). An in-memory Exchange with folders, items, sync keys, Ping, SendMail, contacts and a
   searchable address list, that can inject new mail, flag changes, deletions, moves and failures (HTTP errors, throttling, status
   codes), and be switched between behaviours the specification leaves open. The engine, the IMAP and SMTP servers and the CardDAV
   server are tested against it through real network connections.
3. **A wire-level model of Exchange** (`internal/easupstream/*_test.go`). The real adapter and the vendored ActiveSync library talk WBXML
   over HTTP to a model that **refuses what the specification forbids**, with the answers Exchange gives (a Sync request with elements it
   must not have, a Provision without device information, a Search that is not well formed). A test that provokes a refusal by accident
   fails. This is where the real server's behaviour is kept: when it surprised us, the model learned the same behaviour first, the
   test failed, and then the code was fixed.
4. **Compatibility with the client code MailFlow actually runs** (`compat/`, `make compat`). A Node harness starts the devfake bridge as
   a child process and drives it with imapflow and nodemailer (the libraries MailFlow's backend uses), replaying MailFlow's access
   patterns (the 60 second STATUS cycle with fresh logins, IDLE, MOVE, APPEND of a Sent copy, Arabic subjects and bodies in windows-1256,
   ISO-8859-6 and UTF-8). For contacts it fetches MailFlow 3.9.0's `carddavClient.js` and `vcard.js` unchanged from the release tag, checks
   each file against a pinned SHA-256, and runs them against the bridge. For the automatic certificate, a separate Node process trusts only
   the exported CA, with verification on.
5. **End to end in Docker** (`e2e/`, `make e2e`, and the compose merge test). The generated compose override is merged with MailFlow
   3.9.0's two compose files by `docker compose config` and the result is checked; `make e2e` builds a test image, runs `init`, runs
   `docker compose up`, and checks that a Node 22 container started after the bridge was healthy and reached IMAP, SMTP and CardDAV with
   certificate verification on. It skips cleanly where Docker is not usable.
6. **Race detector and hostile input.** The important packages run with `-race`, repeatedly, under load. The CardDAV server and the MIME
   code are tested with hostile input (oversized structures, nesting, malformed XML, unusual methods).
7. **Dependency checks.** `make vulncheck` runs a pinned `govulncheck` with a project-local cache.

## Mutation checks

A passing test suite says little about whether it would notice a bug. For each feature, a short list of deliberate bugs is applied one
at a time (the line that enforces a rule is removed, inverted or weakened), the tests that should notice are run, and the change is
undone. Examples: the rate limit removed, a card removed after one miss instead of two, a read-only method allowed, the CA volume
mounted writable in the backend, a key file written world-readable, the renewal of the certificate never happening, files overwritten
without `--force`. A mutation that survives means a missing test: one is written, and the mutation is applied again. Several times a
mutation survived at first, and each got its test. The lists are not kept as code; they are applied by a small script
during development.

## Mutation checks of the lockout and certificate rules

The lockout rules (what counts against a user's budget, what stops a session, what is bounded before a login is verified) and the
certificate rules (the CA's name constraints, what is refused at start) are the parts where a silent weakening would not show in normal
use, so each rule has a test that fails when the rule is removed: the constraints dropped or made non-critical, a name outside them
accepted, two refusal look-alikes not stopping the engine, the first one not counted, the streak not reset, the account row made before
verification, the source limit off, the maps unbounded, a throttled login counting as a failure. Node and OpenSSL enforce the CA's
constraints too; `compat/` has Node refuse a leaf for another host name signed with the real CA key.

## What a real Exchange taught it

The findings below come from an **on-premises Exchange 2019 behind a load balancer**, met through `easbridge probe` and then through
ordinary use. Each was invisible to the layers above until the real server answered, and each now has a regression test in the model.

1. **Provision refused with status 165 (DeviceInformationRequired).** From protocol 14.1 on, the initial Provision request must itself
   carry the device information (`settings:DeviceInformation` with at least a model); a separate Settings command before it does not
   count. The ActiveSync library of the time documented the opposite. Fixed by sending it inside the first Provision request. The model
   now answers 165 without it, so every provisioning test guards this. The same pass corrected a few status names that were one code off, and
   added recovery for the statuses that ask the client to provision again.
2. **Sync status 4 (protocol error) on the request for a first SyncKey.** The request with SyncKey 0 must contain nothing but the
   SyncKey and the CollectionId; `GetChanges` is rejected there. The code page tables were also incomplete for protocol 16.x (a meeting
   invitation carried a token no table knew, which failed the decoding of the whole response), so they were completed and made to
   survive an unknown token. `easbridge probe --trace` was added: it prints the decoded request and answer of every call, redacted by an
   allow-list, so the next protocol error needs one run and one report.
3. **"Item has no MIME body" although the MIME was in the answer.** Exchange 2019 sends the MIME as an inline string, the library only
   recognised an opaque block. Fixed in the vendored library; the model now answers the way Exchange does (and can be switched to the
   other form). The same work made ENVELOPE follow RFC 3501 (header text with encoded words kept as written) and made header decoding
   tolerant, with tests for Arabic mail in windows-1256, ISO-8859-6 and UTF-8.
4. **The Sent Items copy has no Bcc header.** The message is delivered correctly and the To recipient's copy carries no Bcc, but
   Exchange's own copy of the sent message, which comes back through ActiveSync, lists no Bcc recipients either, so the Sent folder lost
   them. The bridge now keeps the submitted bytes and serves them until Exchange's copy is bound to them. Whether Exchange lists Bcc for
   items sent from other clients is not promised by the specification and is unknown. (MailFlow 3.9.0 does not store Bcc recipients for
   the messages it sends, so it does not show them for those; that is MailFlow's behaviour.)
5. **The Contacts folders were never found on an install that had run before.** An incremental FolderSync lists only what changed since
   its key, and the key stored by a version that dropped the Contacts folders stands for a hierarchy without them, so Exchange never
   mentioned them again. The same happens when contacts are switched on later. The bridge now records whether the stored hierarchy kept
   the contact folders and reads it again from key 0, once, when it did not. MS-ASCMD does not say what Exchange does to the
   per-folder sync keys when the hierarchy is read from 0; both answers (it keeps them, it drops them) are handled with the existing
   silent key-reset path and are tested against both variants of the fake and the model.

Things the specification leaves open and that are therefore **not known** until someone runs it against their server: whether the
company address book answers one- and two-character queries (the full sweep depends on it, and stops itself if the shortest query it
gets answered is three characters or more), how a given server reacts to hundreds of directory searches, and how a given organisation's
device policy treats a device called `easbridge`.

## Running everything

```
make check      # vet (both tag sets), tests (both tag sets), vendored library tests, the production-binary check
make compat     # imapflow, nodemailer and MailFlow's CardDAV client against a devfake bridge (needs Node 22)
make e2e        # the Docker end-to-end run (skips without Docker)
make vulncheck  # known vulnerabilities in the dependencies
```
