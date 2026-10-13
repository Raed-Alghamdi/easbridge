<picture>
  <source media="(prefers-color-scheme: dark)" srcset="brand/readme-header-dark.svg">
  <img alt="EASBridge" src="brand/readme-header-light.svg" width="450">
</picture>

**Exchange ActiveSync in. IMAP, SMTP and CardDAV out.**

[![CI](https://github.com/raed-alghamdi/easbridge/actions/workflows/ci.yml/badge.svg)](https://github.com/raed-alghamdi/easbridge/actions/workflows/ci.yml)
[![Licence: MIT](https://img.shields.io/badge/licence-MIT-blue.svg)](LICENSE)

EASBridge lets an ordinary mail client read and send the mail of an on-premises Microsoft Exchange mailbox that only allows
**Exchange ActiveSync (EAS)**. The client talks IMAP and SMTP submission to EASBridge, and EASBridge talks EAS to Exchange. It also
serves the mailbox contacts and the company directory (the Exchange Global Address List, GAL) to CardDAV clients, read only. It does
for EAS what DavMail does for EWS.

It was written for [MailFlow](https://github.com/maathimself/mailflow) and is tested against MailFlow's own IMAP, SMTP and CardDAV
client code (MailFlow 3.8.3 and 3.9.0), but any client that speaks IMAP and SMTP submission over TLS should work. It is one static Go
binary (no cgo) in a distroless `nonroot` image.

```
MailFlow ──IMAPS 993──▶ EASBridge ──EAS over HTTPS (Basic)──▶ Exchange (behind a reverse proxy / load balancer)
         ──SMTPS 465──▶    │
         ──CardDAV 8443─▶  └── /data/state.db  (SQLite: folders, UID map, flags, cached MIME, drafts, outbox)
```

## Is it for you?

You need:

- **On-premises Exchange** with ActiveSync switched on for the mailbox and **Basic authentication allowed for ActiveSync**. It was
  written against Exchange 2019; 2016 speaks the same protocol.
- **MailFlow 3.9.0 or newer** for the guided install and the contacts, or another client that speaks IMAP and SMTP submission over
  implicit TLS.
- **Docker.** The guided install uses Docker 24 or newer with the Compose v2 plugin.

It does **not** support Exchange Online (it has no Basic authentication), calendar, tasks, notes, or writing contacts back to
Exchange. Your organisation may also need to approve the device: Exchange sees a new device called `easbridge` the first time.

Not sure whether your server qualifies? Run `easbridge probe` before you install anything else. It tries your server with the same
device and the same path the service will use, asks for the password without echo, and prints only statuses, counts and timings
([First contact with Exchange](docs/OPERATIONS.md#first-contact-with-exchange-easbridge-probe)). Each run is a real login attempt,
so a wrong password counts toward your Active Directory lockout.

## Quick start

Already running MailFlow with Docker Compose? Run these in the folder that holds MailFlow's compose file, then fill in two forms in
MailFlow:

```sh
IMAGE=ghcr.io/raed-alghamdi/easbridge:0.3.0     # the published image
docker run --rm -it --user "$(id -u):$(id -g)" -v "$PWD:/out" "$IMAGE" init
docker compose -f docker-compose.yml -f docker-compose.easbridge.yml up -d
```

`init` asks for the ActiveSync URL and the logins that may use EASBridge, then writes the compose override. `up` starts EASBridge and
then MailFlow's backend, which waits until EASBridge is healthy. **[INSTALL.md](INSTALL.md)** has everything else: the MailFlow
screens, upgrading, uninstalling and troubleshooting.

## What works

- **Mail over IMAP** (port 993, implicit TLS): folders, flags, move, delete, append, search and push (IDLE) on any folder. Everything
  is served from a local database; Exchange is only asked for changes ([the full list](docs/OPERATIONS.md#what-do-the-mail-listeners-support)).
- **Sending over SMTP submission** (port 465, implicit TLS). Bcc recipients are kept in the Sent folder even though Exchange's own
  copy has none ([Sent copies and Bcc](docs/OPERATIONS.md#sent-copies-and-bcc)).
- **First sync is all or nothing.** A folder shows none of its messages while it downloads for the first time, then all of them at
  once ([First sync](docs/OPERATIONS.md#first-sync)).
- **Contacts and the company directory, read only,** over CardDAV (port 8443): one address book per Contacts folder, and a Directory
  book that grows from the company directory ([CONTACTS.md](docs/CONTACTS.md)).

## Security in brief

- **Credentials pass through and are never stored.** The mailbox password lives in memory only and is scrubbed from logs.
- **An allow-list** (`EASBRIDGE_ALLOWED_USERS`) decides who may sign in. Other names never reach Exchange.
- **Lockout protection.** Wrong passwords are remembered and refused locally, and few verifications reach Exchange, so your Active
  Directory account is not locked out by retries. A refusal during a session stops traffic.
- **CardDAV is read only.** Everything that writes gets `405`.
- **The automatic CA is name-constrained**, because MailFlow's backend trusts it for every outbound connection it makes.
- **Never publish the ports.** IMAP, SMTP and CardDAV have no per-connection limits and belong on a private Docker network.

The full model, with the reasons, is in [docs/SECURITY-MODEL.md](docs/SECURITY-MODEL.md). To report a vulnerability, see
[SECURITY.md](SECURITY.md).

## Configuration

Configuration is environment variables. With the guided install, `init` writes the required two for you.

| Variable | Default | Meaning |
|---|---|---|
| `EASBRIDGE_EAS_URL` | (required) | The ActiveSync endpoint. https only |
| `EASBRIDGE_ALLOWED_USERS` | (required) | Comma-separated logins; `@example.com` allows a whole domain |
| `EASBRIDGE_EAS_CA_FILE` | (none) | Extra CA certificates (PEM) to trust when talking to Exchange |
| `EASBRIDGE_FILTER` | `all` | Sync window: `all`, `1d`, `3d`, `1w`, `2w`, `1m` |
| `EASBRIDGE_LISTEN_IP` | (unset) | The one address the listeners bind; unset means all of the container's addresses |
| `EASBRIDGE_TLS_NAMES` | (unset) | Extra names for the automatic certificate |
| `EASBRIDGE_CONTACTS` | `on` | `off` turns off contacts, company directory lookups and the CardDAV listener |

Every setting, the TLS modes, how to hand the CA to MailFlow and how to run the container by hand are in
[docs/CONFIGURATION.md](docs/CONFIGURATION.md).

## Limits

- No calendar, tasks or notes, and contacts are read only.
- Drafts that the client saves live only in EASBridge, not on Exchange.
- COPY is refused (EAS has no copy); MOVE works. There is no server-side search; SEARCH runs over the local cache.
- Widening `EASBRIDGE_FILTER` after the first sync brings older mail in as new messages. Choose the window before the first start.
- A database migrated by a newer version cannot be opened by an older one. Copy the data directory before you upgrade.

The full list is in [docs/OPERATIONS.md](docs/OPERATIONS.md#limits).

## Status and testing

Version 0.3.0 is the first public release. It is tested against an in-memory fake Exchange, a wire-level model that refuses what
Exchange refuses, the client libraries MailFlow uses, MailFlow 3.9.0's own CardDAV client, and a Docker end-to-end run. It has also
been seen on a real on-premises Exchange 2019 behind a load balancer. The mail side has had the most real use; the contacts and
company directory code has had less, so run `easbridge probe` first ([First contact with
Exchange](docs/OPERATIONS.md#first-contact-with-exchange-easbridge-probe)). [docs/TESTING.md](docs/TESTING.md) describes the
strategy, the mutation checks and what the real server taught.

## Documentation

| Read this | To |
|---|---|
| [INSTALL.md](INSTALL.md) | Install next to MailFlow, upgrade, uninstall, troubleshoot |
| [docs/CONFIGURATION.md](docs/CONFIGURATION.md) | Every setting, TLS, listening addresses, device identity, running the container by hand |
| [docs/OPERATIONS.md](docs/OPERATIONS.md) | The probe, first-run checks, first sync, Bcc, health, all limits |
| [docs/CONTACTS.md](docs/CONTACTS.md) | Contacts, the company directory, the full sweep, the CardDAV server |
| [docs/SECURITY-MODEL.md](docs/SECURITY-MODEL.md) | What protects the password, the mailbox and the network |
| [SECURITY.md](SECURITY.md) | Reporting a vulnerability, supported versions, scope |
| [docs/TESTING.md](docs/TESTING.md) | The test strategy and what a real Exchange taught |
| [docs/DESIGN.md](docs/DESIGN.md), [docs/DESIGN-contacts.md](docs/DESIGN-contacts.md), [docs/DESIGN-v0.3.md](docs/DESIGN-v0.3.md) | The design, by version |
| [CONTRIBUTING.md](CONTRIBUTING.md), [RELEASING.md](RELEASING.md) | Contributing and releasing |
| [CHANGELOG.md](CHANGELOG.md) | What changed in each version |
| [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md), [third_party/go-activesync/PATCHES.md](third_party/go-activesync/PATCHES.md) | Dependencies and the patches to the vendored ActiveSync library |

## Development

```sh
source env.sh                    # picks the Go to use (EASBRIDGE_GOROOT, .toolchain/go or the go on PATH); caches stay in .cache
make check                       # vet, tests (plain and -tags devfake), vendored go-activesync tests, production-binary check
make e2e                         # the Docker end-to-end run (skips without Docker)
make compat                      # imapflow 1.7.8 / nodemailer 10.0.13 and MailFlow 3.9.0's CardDAV client against a local devfake bridge (needs node, npm and, once, the network)
make build                       # bin/easbridge (production, no fake upstream)
make vulncheck                   # known vulnerabilities in the dependencies: scripts/govulncheck.sh, pinned govulncheck
```

- `scripts/govulncheck.sh` runs the pinned `govulncheck` with the Go that env.sh picks and the caches in `.cache` (only the Go module proxy and
  `vuln.go.dev` are contacted). It must report that your code is affected by 0 vulnerabilities. It also lists vulnerabilities in
  required modules that the code does not call. Run it before every release; to move to a newer tool, change `GOVULNCHECK_VERSION` in
  the script. CI runs `make check`, `make compat`, `make e2e` and `make vulncheck` on every push; they must stay green on `main`.
- Build tag `devfake` compiles an in-memory fake Exchange (`internal/fakeupstream`) and a loopback control API into the binary. It is
  for tests and the compatibility harness only; the production build and the image do not contain it (`make check-prod` verifies this).
- Layout: `cmd/easbridge`, `internal/{config,logx,store,engine,upstream,easupstream,fakeupstream,imapd,smtpd,carddav,vcard,tlsauto,install,mimeutil,mimestruct,auth}`,
  `third_party/go-activesync` (vendored v1.1.0 with fifteen documented patches), `compat/` (Node harness; it also runs MailFlow
  3.9.0's CardDAV client, fetched at test time into a git-ignored cache and checked against pinned SHA-256 sums).
- [docs/DESIGN.md](docs/DESIGN.md) is the specification ([docs/DESIGN-contacts.md](docs/DESIGN-contacts.md) for contacts);
  [docs/TESTING.md](docs/TESTING.md) describes how EASBridge is tested and what a real Exchange taught it. See [CONTRIBUTING.md](CONTRIBUTING.md)
  before you open a pull request.

## Licence

MIT. See [LICENSE](LICENSE). The licences of the dependencies are in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
