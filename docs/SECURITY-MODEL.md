# The EASBridge security model

This page explains what EASBridge does to protect the mailbox password, the mailbox and the network around it, and what it leaves to
you. To report a vulnerability, see [SECURITY.md](../SECURITY.md). Settings that are named here are described in
[CONFIGURATION.md](CONFIGURATION.md).

## Where does the mailbox password go?

- **Credentials pass through and are never persisted.** The IMAP or SMTP login and password are used as the EAS Basic credentials.
  They live in process memory only (inside the engine, the part of the bridge that serves one mailbox) and are never written to disk
  or to a log. A log scrubber removes every registered secret, and its Basic and SASL encodings, from every log line. Tests scan whole
  sessions for the test password.
- **Gateway cookies** that Exchange or a gateway in front of it sets (for example an access-policy session cookie) are kept per
  mailbox, in memory only, so the gateway does not open a new session for every request.
- **TLS everywhere.** The listeners use implicit TLS, and AUTH is impossible without it. The Exchange URL must be https. EASBridge
  follows a redirect from Exchange only if it stays on https and on the same host and port (at most 5), so the Basic credentials never
  go to another host or onto a plain connection. A refused redirect shows up as an HTTP 3xx error, logged with the status only.

## Who can sign in through EASBridge?

Only the logins in `EASBRIDGE_ALLOWED_USERS`. Any other name is rejected without contacting Exchange, so EASBridge cannot be used as a
password oracle for names outside the list.

**A domain entry (`@example.com`) widens this on purpose.** Anyone who can reach the bridge and has a login in that domain can try a
password through it. Exchange still decides, and the lockout protection below applies to each login. Because every name of the domain
gets that far, one source address may also present at most 10 names that have not been verified in 10 minutes. Further names from it
are refused locally, and no database row, lock or cache entry is made for a name that was never verified. Use plain addresses where
you can name the people, and keep the listeners on a network you trust.

## How does EASBridge avoid locking out the Active Directory account?

Every failed login counts against the Active Directory account lockout, so EASBridge asks Exchange as rarely as it can. All of the
state below is in memory only; restarting the bridge clears it.

- A password Exchange rejects (HTTP 401) is remembered as a salted hash and refused locally with `AUTHENTICATIONFAILED`. So is a
  verification that failed for a refusal look-alike (a gateway's 403, a redirect, an HTML login page), but that one is answered
  `NO [UNAVAILABLE]` (SMTP 454), because the password may well be right.
- A hash that fails again is refused for longer each time: 15 minutes, 1 hour, 4 hours, then 24 hours. A different password or a
  success starts again.
- At most 2 unsuccessful verifications reach Exchange for a user in any 30 minutes. Beyond that, every login that is not already
  verified is refused locally with `NO [UNAVAILABLE]`, whatever password it carries.
- A password that has been verified keeps the fast path: later logins (MailFlow reconnects often) never reach Exchange. Concurrent
  logins share one verification.
- A failure that cannot have counted at Active Directory costs nothing: no connection, a DNS or TLS handshake failure, or a client that
  went away. An answer that says the server is busy or silent (HTTP 429, a 5xx, a timeout) is no refusal either: the same password
  waits a fixed 60 seconds and uses no budget. A 401, a 403, a redirect that is not followed and an HTML login page count and
  escalate.

## What stops traffic during a session?

- A 401 is never retried. All Exchange traffic for that mailbox stops until a client logs in with a working password. A late 401 for
  a password that has since been replaced is ignored.
- A 403, a redirect that is not followed, or an HTML page in answer to an EAS request (how a gateway refuses a credential) counts
  against the same per-user budget. The second one in a row stops the mailbox the same way and drops the cached verification, so the
  next login goes through the guarded path above.
- A 404 and genuine network errors are retried with a back-off.

## What happens when Exchange is slow or failing?

Every mailbox has a **circuit breaker**. It pauses all of that mailbox's Exchange traffic after 3 failed writes to the local database
in a row, after 3 server errors (HTTP 5xx, transient EAS statuses) in a row, and at once on HTTP 429 or 503 or EAS status 111. The
pause starts at 5 seconds, doubles with every consecutive trip up to 15 minutes, and is never shorter than the server's `Retry-After`.
It is visible in `/healthz` (see [OPERATIONS.md](OPERATIONS.md#health)).

## What does it assume about the network?

- **The listeners bind the one IP you name** (`EASBRIDGE_LISTEN_IP` or the listener variables) or, by default, all addresses of a
  container, which EASBridge allows only in a network namespace that looks like a container's. `0.0.0.0`, `::` and an empty host in
  a variable are refused unless `EASBRIDGE_ALLOW_ANY_BIND=true`. See [CONFIGURATION.md](CONFIGURATION.md#where-does-it-listen).
- **No per-connection limits.** The IMAP, SMTP and CardDAV listeners do not limit connections or request rates per client. The
  lockout rules above protect the mailbox password, not the listeners. Keep the ports on a private network and never publish them.

## What can the automatic CA certify?

The automatic CA is **name-constrained**. MailFlow's backend trusts it through `NODE_EXTRA_CA_CERTS`, and Node applies that to every
outbound TLS connection the backend makes. The CA is therefore made with critical name constraints: it may certify only the DNS
names it was created for and private IP addresses (loopback, RFC 1918, unique-local and link-local). Node and OpenSSL refuse a
certificate for any other name, whoever signed it. The names and the reset procedure are in
[CONFIGURATION.md](CONFIGURATION.md#automatic-certificate-the-default).

The CA's key lives in the data volume (`/data/tls/ca-key.pem`, `0600`). Anyone who can read that volume can make certificates that
MailFlow, once it trusts the CA, accepts for the names and private address ranges the CA permits (not for other public names). Protect
the volume like the database. Do not share the directory of the
exported `ca.pem` as writable with anything else: the file holds only the public certificate, but a writable shared directory lets
another container replace it.

## How is hostile mail contained?

Message structure (BODYSTRUCTURE, ENVELOPE, `BODY[n.m]`, SEARCH) is computed once, when the message is stored, with hard limits: 32
levels of nesting, 1,000 parts, 128 KiB of header per part, and a scan budget proportional to the size. A message beyond a limit is
shown as one `application/octet-stream` part: it stays visible and its bytes stay downloadable. A deliberately hostile message that
would need gigabytes to describe takes about 200 KB.

## What is written to disk and to logs?

- **Logs** never contain credentials, subjects, addresses or bodies. With contacts on, they contain no names, phone numbers, search
  queries or vCards either: statuses and counts only. The `User` query value is removed from request URLs.
- **At rest**, `state.db` holds a cache of your mail. It also holds the bridge-only drafts, for a day the messages just sent with a Bcc
  recipient (see [OPERATIONS.md](OPERATIONS.md#sent-copies-and-bcc)), and, with contacts on, your contacts and the directory cards.
  EASBridge does not encrypt it: protect the data directory (disk or volume encryption, a `0700` directory; the database file is
  created `0600`). SQLite runs with `secure_delete`, so a deleted message is overwritten in the file instead of lingering in free
  pages. The write-ahead log can still hold older page images until the next checkpoint overwrites them.

## What does EASBridge look like to Exchange, and what does a remote wipe do?

- **Honest device identity.** A fixed DeviceId and DeviceType, and a device description that says what the bridge is and nothing a
  phone would have (see [CONFIGURATION.md](CONFIGURATION.md#device-identity)).
- **Remote wipe is honoured.** On a wipe order the bridge writes `/data/WIPED`, deletes the state database (the cache of your mail),
  acknowledges the wipe, closes all sessions, logs one loud line and refuses logins until you delete the marker. Exchange 2016 and
  2019 can also order an account-only wipe (EAS 16.1, `AccountOnlyRemoteWipe`). The bridge keeps nothing but the cache of the
  mailbox, so it does the same and acknowledges with that order's own element.
