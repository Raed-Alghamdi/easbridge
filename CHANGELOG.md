# Changelog

All notable changes to EASBridge. The format follows Keep a Changelog; versions follow semantic versioning.

## [0.3.0] - first public release

A normal install now needs two values, the ActiveSync URL and the login. The design is in `docs/DESIGN-v0.3.md`.

### Added

- **`easbridge init`** and a compose override for people who already run MailFlow with Docker Compose.
  `docker run --rm -it --user "$(id -u):$(id -g)" -v "$PWD:/out" <image> init` asks for the ActiveSync URL and the logins (or takes
  `--eas-url` and `--users`), checks them with the rules of the configuration, reads MailFlow's compose file as text for the backend
  service and the network, and writes `docker-compose.easbridge.yml` and `easbridge.env` (it never overwrites without `--force`, and it
  writes no secrets). The override adds the `easbridge` service (hardened, no published ports, two named volumes, a 5 second health
  check) and gives MailFlow's backend `NODE_EXTRA_CA_CERTS`, the CA volume read only, and `depends_on: easbridge: condition:
  service_healthy`. A test merges it with MailFlow 3.9.0's `docker-compose.yml` and `docker-compose.ghcr.yml` with `docker compose config`.
- `INSTALL.md`: requirements, the install step by step with MailFlow 3.9.0's own screen labels, the route without Compose, first-run
  checks, upgrading, uninstalling and troubleshooting by symptom.
- The README is now a short front page. The detail moved to `docs/CONFIGURATION.md` (settings, TLS, listening addresses, device
  identity, running the container by hand), `docs/OPERATIONS.md` (the probe, first sync, Bcc, health, limits), `docs/CONTACTS.md` and
  `docs/SECURITY-MODEL.md`; `SECURITY.md` keeps the reporting policy.
- **An automatic certificate.** With no certificate files provided, EASBridge makes a private CA (ECDSA P-256, valid 20 years, a unique
  name) and a server certificate (valid 2 years) in `/data/tls/` (directory `0700`, keys `0600`) at its first start, reuses them, and
  renews the server certificate from the same CA 60 days before it ends, while running. MailFlow trusts the CA once. The certificate names
  `easbridge`, the host name, `EASBRIDGE_TLS_NAMES` (DNS names or IP addresses), `EASBRIDGE_LISTEN_IP` and the container's addresses; a
  change of names issues a new one from the same CA. A damaged or mismatched CA is never replaced silently: the bridge stops with one line.
- `EASBRIDGE_CA_EXPORT_DIR` (compose: `/ca`): `ca.pem` is written there at start and whenever it differs, for MailFlow's backend
  (`NODE_EXTRA_CA_CERTS`, which Node reads only at start). `easbridge ca` prints the CA (`docker exec easbridge /easbridge ca`).
- `EASBRIDGE_LISTEN_IP` (optional): one IP address that IMAP, SMTP and CardDAV bind on their default ports (993, 465, 8443). It must be an
  address of the container, or the bridge does not start. Without it the listeners bind all addresses of the container, which is refused
  when the network namespace does not look like a container's (host networking, for example); `EASBRIDGE_ALLOW_ANY_BIND=true` overrides
  that. `EASBRIDGE_IMAP_ADDR`, `EASBRIDGE_SMTP_ADDR` and `EASBRIDGE_CARDDAV_ADDR` still work and win per listener.
- `EASBRIDGE_CONTACTS=on|off`, default `on`. `off` together with `EASBRIDGE_CARDDAV_ADDR` is a configuration error.
- `EASBRIDGE_ALLOWED_USERS` accepts domain entries: `@example.com` allows every login of exactly that domain (case-insensitive, no
  subdomains).
- `/healthz`: `tls_mode` and `tls_server_expires_in_days`; in `directory_sweep`, `sweep_infeasible` and `sweep_infeasible_length`.
- The published image name is one constant (`internal/version.Image`); `make IMAGE=...`, the Dockerfile's `IMAGE` argument and `init --image`
  change it.
- `make e2e`: builds a test image with a fake Exchange inside, runs `init` next to a stand-in compose file, runs `docker compose up`, and
  checks that a Node 22 backend waited for the healthy bridge and reached IMAP, SMTP and CardDAV with certificate verification on. It
  skips cleanly when Docker is not usable.
- The build works without the project-local Go toolchain: `env.sh` uses the Go on `PATH` when the checkout has no `.toolchain`.

### Changed

- **Contacts are on by default.** An install that did not set `EASBRIDGE_CARDDAV_ADDR` now has the contacts feature on after the upgrade,
  with a CardDAV listener on port 8443. Set `EASBRIDGE_CONTACTS=off` to keep it as it was.
- `EASBRIDGE_IMAP_ADDR` and `EASBRIDGE_SMTP_ADDR` are no longer required. `EASBRIDGE_ALLOW_ANY_BIND` accepts `true` as well as `1`.
- Provided certificate files (`EASBRIDGE_TLS_CERT` and `EASBRIDGE_TLS_KEY`, or files at `/tls/server.pem` and `/tls/server-key.pem`) are
  used exactly as before and never written to. One of the pair alone, unreadable files, or a certificate that does not match its key stop
  the start with one line.
- The image has `/data` and `/ca` owned by the runtime user (uid 65532) and declares `/data` a volume, so a named Docker volume needs no
  `chown`. The image sets no `EASBRIDGE_TLS_*` variable.
- The full directory sweep (`EASBRIDGE_DIRECTORY_FULL=on`, off by default) is marked experimental and stops itself for the run when
  the directory answers no query shorter than three characters: walking every string of that length is not feasible.

### Fixed

- **The automatic CA is name-constrained.** MailFlow's backend trusts it through `NODE_EXTRA_CA_CERTS` for every outbound connection it
  makes, so a CA that could certify any name would be a skeleton key for whoever reads the data volume. The CA now carries critical name
  constraints: the DNS names it was created for and private addresses only. A required name outside them stops the start with one line
  that says how to start over; the host name and interface addresses are best effort.
- **A 403, an unfollowed redirect or an HTML logon page during a session was retried for ever.** They count against the user's budget now,
  and two in a row stop the mailbox's traffic like a 401 (404 and network errors are still retried with the back-off).
- **Nothing grows before a login is verified.** With a domain entry in the allow-list, no account row is made for a name that was never
  verified, the lock, failure and user maps are bounded and expire, and one source address may present only 10 unverified names in 10 minutes.
- At a login, HTTP 429, 5xx and time-outs no longer count as failed verifications: the same password waits a fixed 60 seconds with no
  escalation. 401, 403 and logon pages still count.
- `easbridge healthcheck -v` prints the health report (the image has no curl). The wildcard bind message and the allow-list entries are
  checked and explained. The image builds on the build platform and cross-compiles (no emulation for arm64) and declares no `EXPOSE`.
- A stop that arrived while the bridge was still starting could leave the process waiting for ever (go-smtp's `Serve`, called after
  `Close`, accepts until its listener is closed), so a container could ignore SIGTERM until Docker killed it. Shutdown now closes the IMAP,
  SMTP, health and CardDAV listeners itself and waits for the servers at most 10 seconds.
- Test fixtures and documentation carry neutral example values.

### Upgrading

The database schema is 8, as in 0.2.2; there is no migration. Take a copy of the data directory before any upgrade: an older version
cannot open a database that a newer one migrated.

## Before 0.3.0

These versions were not published; they are summarised here.

- **0.2.2.** The Contacts folders were never found on an install whose stored FolderSync key predated the contacts feature (the key
  stood for a hierarchy from which Exchange no longer mentions them). Schema 8 records whether the stored hierarchy kept them; with
  contacts on and the marker unset, the next FolderSync starts from key 0, once. Mail is not touched.
- **0.2.1.** An opt-in full sweep of the Global Address List into the Directory book (`EASBRIDGE_DIRECTORY_FULL`, off by default):
  calibration of the shortest query Exchange answers, a prefix walk with division of saturated prefixes, caps of 5,000 searches and 20
  characters, a persisted queue (schema 7), the shared lookup budget with the as-you-go lookups first.
- **0.2.0.** Read-only contacts over CardDAV: one address book for each Contacts folder of the mailbox, and a Directory book built one
  exact Global Address List match at a time for the addresses seen in mail, rate limited. MailFlow 3.9.0's CardDAV client and vCard
  parser run unchanged in the test suite.
- **0.1.8.** The Sent copy shows the Bcc recipients (the submitted bytes are kept and served until Exchange's copy, which has no Bcc
  header, is bound to them).
- **0.1.5 to 0.1.7.** Fixes for what the first contacts with a real Exchange 2019 found: device information inside the initial Provision
  request; the request for a first SyncKey carries nothing else; a MIME body that arrives as an inline string is the MIME; ENVELOPE as
  RFC 3501 writes it; Arabic mail in windows-1256, ISO-8859-6 and UTF-8. See `docs/TESTING.md`.
- **0.1.0 to 0.1.4.** The first version: IMAP4rev1 and SMTP submission in front of Exchange ActiveSync, a SQLite cache, push through
  Ping, a durable outbox for flag changes and deletes, first sync all or nothing per folder, lockout protection for the mailbox
  password, a circuit breaker, bounded MIME structure parsing, and the fixes from an independent security review.
