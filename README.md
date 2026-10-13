<picture>
  <source media="(prefers-color-scheme: dark)" srcset="brand/readme-header-dark.svg">
  <img alt="EASBridge" src="brand/readme-header-light.svg" width="450">
</picture>

**Exchange ActiveSync in. IMAP, SMTP and CardDAV out.**

[![CI](https://github.com/raed-alghamdi/easbridge/actions/workflows/ci.yml/badge.svg)](https://github.com/raed-alghamdi/easbridge/actions/workflows/ci.yml)
[![Licence: MIT](https://img.shields.io/badge/licence-MIT-blue.svg)](LICENSE)

EASBridge lets an ordinary mail client read and send the mail of an on-premises Microsoft Exchange mailbox that
only allows **Exchange ActiveSync (EAS)**. The client talks IMAP and SMTP submission to EASBridge, and EASBridge talks
EAS to Exchange. It also serves the mailbox contacts and the company address book to CardDAV clients, read only.
It does for EAS what DavMail does for EWS.

It was written for [MailFlow](https://github.com/maathimself/mailflow) and is tested against MailFlow's own IMAP, SMTP and
CardDAV client code (MailFlow 3.8.3 and 3.9.0), but any client that speaks IMAP and SMTP submission over TLS should work.
It is one static Go binary (no cgo) in a distroless `nonroot` image.

```
MailFlow ──IMAPS 993──▶ EASBridge ──EAS over HTTPS (Basic)──▶ Exchange (behind a reverse proxy / load balancer)
         ──SMTPS 465──▶    │
         ──CardDAV 8443─▶  └── /data/state.db  (SQLite: folders, UID map, flags, cached MIME, drafts, outbox)
```

## Quick start

Already running MailFlow with Docker Compose? Two commands and two forms in MailFlow. Run them in the folder that holds MailFlow's
compose file:

```sh
IMAGE=ghcr.io/raed-alghamdi/easbridge:0.3.0     # the published image; the other examples in this file use $IMAGE
docker run --rm -it --user "$(id -u):$(id -g)" -v "$PWD:/out" "$IMAGE" init
docker compose -f docker-compose.yml -f docker-compose.easbridge.yml up -d
```

`init` writes the compose override, and `up` starts EASBridge. **[INSTALL.md](INSTALL.md)** has everything: what you need, the
MailFlow screens, upgrading, uninstalling and troubleshooting.

## Status

Version 0.3.0 is the first public release. What is covered, and what is not:

- **Tested against fakes and models, and against real client code.** An in-memory fake Exchange, a wire-level (WBXML) model that refuses
  what Exchange refuses, imapflow and nodemailer (the libraries MailFlow uses), MailFlow 3.9.0's own CardDAV client, and a Docker
  end-to-end run. [docs/TESTING.md](docs/TESTING.md) describes the strategy and the mutation checks.
- **Seen on a real Exchange.** An on-premises Exchange 2019 behind a load balancer produced five findings that the fakes could not
  (listed in [docs/TESTING.md](docs/TESTING.md)); each is fixed and has a test. The mail side has had the most real use. The contacts
  and company address book code has had less: run `easbridge probe` first and read its output (see "First contact with Exchange").
- **Not covered:** Exchange Online (it has no Basic authentication), calendar, tasks and notes, and writing contacts back.
- **Known limits** are under "Limits".

## What it does

- **IMAP** (implicit TLS, port 993): LOGIN and AUTHENTICATE PLAIN, LIST with SPECIAL-USE, NAMESPACE, STATUS, SELECT/EXAMINE,
  FETCH (ENVELOPE, BODYSTRUCTURE, sections), SEARCH, STORE, MOVE with COPYUID, EXPUNGE/UID EXPUNGE, APPEND, CREATE/DELETE/RENAME,
  IDLE on any folder in several sessions at once. Everything is served from the local database; Exchange is only asked for changes.
  ENVELOPE carries the subject and the names as the message wrote them (RFC 2047 encoded-words are not decoded: the client does that,
  RFC 3501 section 7.4.2), sections are the original bytes, and SEARCH reads the decoded text of headers and bodies in the charsets
  go-message knows (windows-1256, ISO-8859-6 and UTF-8 are tested, B and Q encoded).
- **SMTP submission** (implicit TLS, port 465): AUTH PLAIN/LOGIN, hands the raw message to EAS `SendMail` with `SaveInSentItems`.
  Recipients that exist only in the SMTP envelope (Bcc) are written into a `Bcc:` header first, because EAS reads recipients
  from the headers. The bytes are kept (see "Sent copies").
- **Sync**: FolderSync, Sync per folder with full MIME (big messages are completed with ItemOperations before they become
  visible, so BODYSTRUCTURE and sizes are always right), Ping for push, a durable outbox for flag changes and deletes.
- **First sync, all or nothing**: a folder shows none of its messages while its first download runs (LIST shows it, STATUS
  and SELECT report 0 messages and a steady UIDNEXT). When the download has caught up, all messages appear at once, as
  one `EXISTS`, with UIDs in the order of their received date. MailFlow then ingests the folder as a fresh backfill
  without new-mail alerts instead of alerting for every older message that would otherwise arrive late.
- **Sent copies**: MailFlow APPENDs every sent message to Sent, and Exchange saves one too. They are de-duplicated (same
  Message-ID, else subject + date + recipients) so the folder shows one copy with one UID. Exchange's copy has no Bcc header
  (it comes back through EAS without one, although the message was delivered with the recipient), so a message submitted with
  a Bcc header is served as the bridge submitted it, from the moment MailFlow APPENDs it: the Sent folder shows the Bcc
  recipients, on the same UID, with nothing new announced when Exchange's copy arrives. Details under "Sent copies and Bcc".
- **Drafts** APPENDed by the client stay in the bridge (they are not created on Exchange).
- **Contacts** (on by default, read-only): a CardDAV address book (TLS, its own port) that MailFlow 3.9.0 pulls: one book per Contacts
  folder of the mailbox, and a "Directory" book that grows one exact match at a time from the Global Address List. See "Contacts and
  directory (read-only)".

## Running it

**The minimal form.** A container on the network MailFlow is on, with the two values every install needs: the ActiveSync URL and the
login. EASBridge makes its own certificate (see "TLS"); hand its CA to MailFlow as described there.

```sh
docker run -d --name easbridge --restart unless-stopped \
  --network <the network MailFlow is on> \
  -e EASBRIDGE_EAS_URL=https://mail.example.com/Microsoft-Server-ActiveSync \
  -e EASBRIDGE_ALLOWED_USERS=you@example.com \
  -v easbridge-data:/data \
  "$IMAGE"
```

EASBridge listens for IMAP (993), SMTP submission (465) and CardDAV (8443) on all of the container's addresses, and MailFlow reaches
it by the container's name (`easbridge`) on that network. The contacts feature is on. The container needs a route to Exchange; if
MailFlow's network is an internal one (no route out), also attach an egress network, as in the strict form. The bridge refuses to
start with this default if its network namespace does not look like a container's (host networking, for example; see the bullets under "Running it").

**The strict form.** The hardened shape for a host that also faces other networks: read-only root, no capabilities, resource limits, a
fixed address on a private network, and the listeners bound to that address only.

```sh
docker create --name easbridge --read-only --cap-drop=ALL --security-opt no-new-privileges \
  --log-opt max-size=50m --log-opt max-file=3 \
  --memory=1g --pids-limit=256 --ulimit core=0 \
  --network mail-internal --ip 10.0.0.5 \
  -e EASBRIDGE_EAS_URL=https://mail.example.com/Microsoft-Server-ActiveSync \
  -e EASBRIDGE_ALLOWED_USERS=you@example.com \
  -e EASBRIDGE_LISTEN_IP=10.0.0.5 \
  -v easbridge-data:/data \
  "$IMAGE"
docker network connect mail-egress easbridge
docker start easbridge
```

**Never publish these ports** (`-p 993:993` and the like). IMAP, SMTP and CardDAV have no per-connection limits and no protection against a
flood of connections or of password guesses beyond the per-login lockout rules; they are meant for a private Docker network that only MailFlow's
backend and EASBridge share. The image declares no `EXPOSE` on purpose.

(Any Docker host works. The container templates of NAS and panel systems map the same variables, paths and options.)

- Build: `docker build -t local/easbridge:0.3.0 .` (multi-stage; `golang:1.27` builder, `CGO_ENABLED=0`, `-trimpath`,
  final image `gcr.io/distroless/static-debian12:nonroot`, uid 65532). The base images and the BuildKit frontend named by
  `# syntax=` are pinned by digest in the `Dockerfile`; a test fails if one is not. The builder is `golang:1.27.2`, the
  Go release of the project-local toolchain.
- **Resource limits.** `--memory=1g` and `--pids-limit=256` bound what a hostile message or a bug can take from the host
  (the bridge parses mail from the outside world, with hard limits of its own; this is the second wall), and
  `--ulimit core=0` keeps a crash from writing a core dump, which would contain passwords and mail from the process memory.
- **Where EASBridge listens.** Each of the three listeners (IMAP, SMTP, CardDAV) takes its address, in this order, from its own
  variable (`EASBRIDGE_IMAP_ADDR`, `EASBRIDGE_SMTP_ADDR`, `EASBRIDGE_CARDDAV_ADDR`: `ip:port`, validated as before), else from
  `EASBRIDGE_LISTEN_IP` (one IP literal, v4 or v6, on the default port: 993, 465, 8443), else from the default, which is **all of the
  container's addresses**. Setting `EASBRIDGE_LISTEN_IP` is the strict choice: the bridge is attached to two networks (below), and a
  listener on every address answers on both, so anything on the egress network could reach the IMAP and SMTP ports. Give the
  container a fixed address on `mail-internal` (`--ip 10.0.0.5`; the network must be a user-defined one whose subnet contains it,
  so pick a free address of yours) and name it in `EASBRIDGE_LISTEN_IP`. It must be an address of the container: otherwise the bridge
  does not start and says which addresses exist. The health endpoint stays on `127.0.0.1:8081`. The start log names the mode
  (`listening on all container addresses (set EASBRIDGE_LISTEN_IP to restrict)` or `listening on 10.0.0.5 only`). Addresses in the
  three variables keep working exactly as before (`:993`, `0.0.0.0:993` and `[::]:993` in a variable are still refused unless
  `EASBRIDGE_ALLOW_ANY_BIND=true` is set).
- **Host-network guard.** With the wildcard default (no `EASBRIDGE_LISTEN_IP`) the bridge refuses to start in a network namespace
  that does not look like a container's, because a wildcard bind on a host is open to every network of the host. The rule: leave out
  the loopback; **every other interface must be named `eth` and digits** (`eth0`, `eth1`). A container has `lo` and `eth0` (or
  `eth0` and `eth1` on two networks); a host has `enp*`, `eno*`, `wlan0`, `wlp*`, `docker0`, `br-*`, `veth*`, `tailscale0` or `wg0`, and
  any one of them is enough to refuse. The message says to set `EASBRIDGE_LISTEN_IP` or the three listener variables, or
  `EASBRIDGE_ALLOW_ANY_BIND=true` if listening on every address there is what you want. Limits of the guess: host networking on a
  machine whose only interface is called `eth0` passes it, and a container with a VPN interface or Podman's `pasta` is refused (set
  `EASBRIDGE_LISTEN_IP` for those). A configuration that gives all three listener variables is never checked.
- No host ports. The bridge needs two networks: the private one MailFlow's backend uses (`mail-internal`, where MailFlow
  reaches it as `easbridge:993` / `easbridge:465`) and an egress network that can reach Exchange (`mail-egress`).
  An internal Docker network has no route out, so with only the first one the bridge could never reach Exchange. Older
  Docker versions take a single `--network` at creation, hence create, `network connect`, start.
- **`/data`.** The image has `/data` owned by uid 65532 with mode 0700 and declares it a volume, so a named Docker volume
  (`-v easbridge-data:/data`) is created writable for the bridge with no `chown`. A host directory mounted over it
  (`-v /srv/easbridge/data:/data`) must be writable by uid 65532 (`chown 65532:65532 /srv/easbridge/data`). The state database
  lives there.
- **Contacts are on by default** (`EASBRIDGE_CONTACTS=on`), with the CardDAV listener on port 8443 by the rule above. Set
  `EASBRIDGE_CONTACTS=off` for no CardDAV listener, no contacts sync and no GAL lookups at all (mail only). `off` together with `EASBRIDGE_CARDDAV_ADDR` is refused at start. An address in `EASBRIDGE_CARDDAV_ADDR` must differ from the
  IMAP, SMTP and health addresses; like them it uses the bridge's TLS certificate (see "TLS").
- Binding ports 993, 465 and 8443 as uid 65532 with `--cap-drop=ALL` relies on Docker's default
  `net.ipv4.ip_unprivileged_port_start=0` inside containers (Docker 20.10 and later). If your runtime differs, pass
  `--sysctl net.ipv4.ip_unprivileged_port_start=0` or put the ports of the listener variables above 1023.
- The root file system can be read-only (`--read-only`): the bridge writes only under `/data`, and SQLite keeps its
  temporary data in memory.
- `HEALTHCHECK` runs `/easbridge healthcheck` (the image has no shell or curl). It asks `http://127.0.0.1:8081/healthz`.
  To read the report, run `docker exec easbridge /easbridge healthcheck -v`: it prints the JSON (states and counts, no mail content) and
  exits 0 when the bridge is healthy.

### TLS

EASBridge serves IMAP, SMTP and CardDAV over **implicit TLS only**; there is no plaintext listener and no STARTTLS. The certificate
comes in one of two ways, and the health report says which (`tls_mode`, with `tls_server_expires_in_days`;
`docker exec easbridge /easbridge healthcheck -v`).

**Automatic (the default).** If you provide no certificate files, EASBridge makes a private CA and a server certificate at its first
start, in `/data/tls/` (`ca.pem`, `ca-key.pem`, `server.pem`, `server-key.pem`; the directory is `0700`, the keys `0600`, all owned
by the user the bridge runs as). ECDSA P-256. The CA is valid 20 years and has a unique name (`EASBridge local CA` and a random id);
the server certificate is valid 2 years and is **renewed from the same CA 60 days before it ends, while the bridge runs** (listeners
pick it up without a restart). The CA never changes, so MailFlow needs to trust it once. Later starts reuse the files; a valid set is
never regenerated. The server certificate names:

- DNS `easbridge`, the container's host name, and every name in `EASBRIDGE_TLS_NAMES` (comma-separated; an entry that is an IP
  address becomes an IP name);
- IP names for `EASBRIDGE_LISTEN_IP` (if set) and the container's own non-loopback addresses.

**The CA is name-constrained.** MailFlow's backend trusts it through `NODE_EXTRA_CA_CERTS`, and Node applies that to *every* outbound TLS
connection the backend makes, not only to EASBridge. A CA that could certify any name would be a skeleton key for whoever reads the data
volume, so the CA is made with critical name constraints: it may certify only the DNS names it was created for (`easbridge`, the host name
at that time, the entries of `EASBRIDGE_TLS_NAMES`) and IP addresses in the loopback, RFC 1918, unique-local (`fc00::/7`) and link-local
ranges. Node and OpenSSL refuse a certificate for any other name, whoever signed it.

If the addresses change (a new `EASBRIDGE_LISTEN_IP`, an `EASBRIDGE_TLS_NAMES` address, a new container address) a new server
certificate is issued from the same CA at start. The host name and the container's own addresses are best effort: a recreated container
has a new host name, which the CA was not made for, so it is simply left out. A *required* name outside the constraints (a new DNS
name in `EASBRIDGE_TLS_NAMES`, a public address in `EASBRIDGE_LISTEN_IP`) stops the start with one line that says how to start over:
stop EASBridge, remove the directory `/data/tls` in the data volume (a new CA is made at the next start) and **restart MailFlow's backend**,
because Node reads the CA only when it starts. A set whose CA is incomplete, does not match its key or has expired is **not**
replaced behind your back: the bridge stops with one line, the files stay as they are, and you decide (remove `/data/tls` to start
over, and trust the new CA).

*Giving MailFlow the CA, so that it can verify the certificate.* Two ways:

1. **Export to a shared volume.** Set `EASBRIDGE_CA_EXPORT_DIR` (for compose: `/ca`) and mount a volume there. EASBridge writes
   `ca.pem` (`0644`) at every start and again whenever the file is missing or differs. Mount the same volume read-only in MailFlow's
   backend (for example at `/easbridge-ca`) and set `NODE_EXTRA_CA_CERTS=/easbridge-ca/ca.pem` there. **Node reads that variable only
   when it starts**, so the backend must be started after EASBridge has written the file the first time (in compose: `depends_on` with
   `condition: service_healthy`, and restart the backend if you recreate the CA volume).
2. **Print it.** `docker exec easbridge /easbridge ca` prints the CA certificate (PEM) on standard output; save it where MailFlow's
   backend can read it. The binary is `/easbridge` in the image (the image has no shell). In provided mode the command says the CA is
   not managed by EASBridge and exits with an error.

**Provided.** If `EASBRIDGE_TLS_CERT` and `EASBRIDGE_TLS_KEY` are both set, or neither is set and both `/tls/server.pem` and
`/tls/server-key.pem` exist, those files are used exactly as before and never written to or "repaired". The leaf certificate's names must
include the host MailFlow connects to (for example DNS `easbridge`). A renewed certificate is picked up without a restart (checked at
most once a minute, on a handshake). For a private CA, make MailFlow trust it (for example `NODE_EXTRA_CA_CERTS`) instead of relaxing
certificate checks. If only one of the two variables is set, if only one of the two default files exists, or if the files cannot be read
or do not match each other, the bridge refuses to start with one line and changes nothing. `EASBRIDGE_TLS_NAMES` and
`EASBRIDGE_CA_EXPORT_DIR` do nothing in this mode (a warning says so).

### Configuration

All configuration is environment variables. Required: `EASBRIDGE_EAS_URL` and `EASBRIDGE_ALLOWED_USERS`.

| Variable | Default | Meaning |
|---|---|---|
| `EASBRIDGE_EAS_URL` | (required) | EAS endpoint. **https only**; `http://` is refused |
| `EASBRIDGE_ALLOWED_USERS` | (required) | Comma-separated login names. Anyone else is rejected locally, without contacting Exchange. An entry that starts with `@` (`@example.com`) allows every login of exactly that domain (any case, no subdomains); see "Security model" for what that means |
| `EASBRIDGE_EAS_VERSION` | `16.1` | Preferred protocol version (`16.1`, `16.0`, `14.1`, `14.0`). One OPTIONS at engine start; falls back to `14.1` |
| `EASBRIDGE_DEVICE_TYPE` | `easbridge` | EAS DeviceType, part of the identity Exchange approves (honest on purpose; do not impersonate Outlook). Changing it makes the bridge a new device |
| `EASBRIDGE_DEVICE_MODEL` | `easbridge` | Model in the device information (see "Device identity"); must not be empty |
| `EASBRIDGE_DEVICE_FRIENDLY_NAME` | `easbridge (MailFlow bridge)` | FriendlyName in the device information, what Exchange shows in its device list |
| `EASBRIDGE_DEVICE_OS` | `Linux` | OS in the device information |
| `EASBRIDGE_DEVICE_OS_LANGUAGE` | `en-us` | OSLanguage in the device information |
| `EASBRIDGE_USER_AGENT` | `easbridge/<version>` | HTTP User-Agent, also sent as the UserAgent of the device information. It follows the version; if a rule on the Exchange side matches the exact string, pin it with this variable |
| `EASBRIDGE_FILTER` | `all` | Sync window: `all`, `1d`, `3d`, `1w`, `2w`, `1m`. With a window, mail older than it is not synced and may disappear from IMAP as it ages out |
| `EASBRIDGE_MIME_INLINE_MAX` | `1MiB` | Largest message body accepted inline in a Sync response; bigger ones are fetched with ItemOperations |
| `EASBRIDGE_PING_HEARTBEAT` | `480` | Ping heartbeat in seconds (or a duration like `8m`). Steps down towards 120 s if long requests are cut early |
| `EASBRIDGE_LISTEN_IP` | (unset) | One IP literal (v4 or v6) that IMAP, SMTP and CardDAV bind (ports 993, 465, 8443) unless their own variable is set. It must be an address of the container, or the bridge does not start. Unset: all of the container's addresses, with the host-network guard ("Running it") |
| `EASBRIDGE_IMAP_ADDR` / `EASBRIDGE_SMTP_ADDR` | (from `EASBRIDGE_LISTEN_IP` or all container addresses, 993 / 465) | `IP:port` to listen on, for example `10.0.0.5:993`. Wins over `EASBRIDGE_LISTEN_IP`. An IP address is required; an address for every interface (`:993`, `0.0.0.0`, `::`) is refused unless `EASBRIDGE_ALLOW_ANY_BIND=true` |
| `EASBRIDGE_HEALTH_ADDR` | `127.0.0.1:8081` | Health endpoint (`/healthz`), content-free. The same rule about every interface applies |
| `EASBRIDGE_ALLOW_ANY_BIND` | (unset) | `true` (or `1`) allows listen addresses without an IP in a variable, and turns the host-network guard of the wildcard default off. Not needed in a container |
| `EASBRIDGE_TLS_CERT` / `EASBRIDGE_TLS_KEY` | (unset; files at `/tls/server.pem` and `/tls/server-key.pem` count as given) | Your own certificate and key (provided mode, used as they are). Both or neither. With neither and no files at the default paths EASBridge makes its own ("TLS") |
| `EASBRIDGE_TLS_NAMES` | (unset) | Comma-separated extra names of the automatic server certificate: DNS names, or IP addresses. Always included: `easbridge`, the host name, `EASBRIDGE_LISTEN_IP`, the container's addresses |
| `EASBRIDGE_CA_EXPORT_DIR` | (unset) | Directory (a volume) that receives a copy of the automatic CA certificate as `ca.pem`, for MailFlow's `NODE_EXTRA_CA_CERTS`. Compose: `/ca` |
| `EASBRIDGE_DATA_DIR` | `/data` | State directory (`state.db`, and the `WIPED` marker) |
| `EASBRIDGE_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `EASBRIDGE_LOG_FORMAT` | `text` | `text` or `json` |
| `EASBRIDGE_AUTH_FAIL_COOLDOWN` | `15m` | How long a password Exchange rejected is refused locally |
| `EASBRIDGE_ENGINE_GRACE` | `30m` | How long a mailbox engine outlives its last client session |
| `EASBRIDGE_MAX_MESSAGE_BYTES` | `35MiB` | Size limit for SMTP and APPEND |
| `EASBRIDGE_EAS_CA_FILE` | (none) | Extra CA certificates (PEM) to trust when talking to Exchange |
| `EASBRIDGE_CONTACTS` | `on` | `on` or `off`. Off: no contacts sync, no GAL lookups, no CardDAV listener. Off together with `EASBRIDGE_CARDDAV_ADDR` is a configuration error |
| `EASBRIDGE_CARDDAV_ADDR` | (from `EASBRIDGE_LISTEN_IP` or all container addresses, 8443) | `IP:port` of the read-only CardDAV listener (TLS, the certificate of IMAP and SMTP). The same bind rules as IMAP and SMTP; it must differ from their addresses and from the health address |
| `EASBRIDGE_CONTACTS_INTERVAL` | `30m` | How often each Contacts folder is synced (5 minutes to 24 hours). Polling: contacts folders are not part of Ping |
| `EASBRIDGE_DIRECTORY_DOMAINS` | the domains of `EASBRIDGE_ALLOWED_USERS` | Comma-separated domains whose addresses are looked up in the Global Address List; `none` turns the directory off |
| `EASBRIDGE_DIRECTORY_LOOKUPS_PER_HOUR` | `60` | 1 to 180. One GAL search at a time, consecutive searches at least `max(20 s, 1 h / value)` apart (that spacing is what caps the value at 180), and at most ten times the value per UTC day |
| `EASBRIDGE_DIRECTORY_REFRESH` | `720h` | How long a found directory card is kept before it is looked up again (24 hours to 365 days) |
| `EASBRIDGE_DIRECTORY_FULL` | `off` | `off` or `on`: the opt-in, **experimental** full sweep of the Global Address List (see "The full directory sweep"). `on` needs the contacts (`EASBRIDGE_CONTACTS` not `off`) and a directory (`EASBRIDGE_DIRECTORY_DOMAINS` not `none`); the combination is refused at start otherwise |
| `EASBRIDGE_DIRECTORY_FULL_INTERVAL` | `720h` | From the end of one finished sweep to the start of the next (168 hours to 365 days) |
| `EASBRIDGE_CONTACTS_BOOK_NAME` | `Exchange contacts` | Name of the address book of the default Contacts folder (a user-created folder keeps its own name) |
| `EASBRIDGE_DIRECTORY_BOOK_NAME` | `Directory` | Name of the directory address book; must differ from the other |

### Device identity

Exchange knows the bridge as a device. Two things identify it, and both stay the same for the life of an install: the **DeviceId** (32 hex
characters, generated once and kept in `state.db`) and the **DeviceType** (`easbridge`). An organisation that approves devices
approves that pair, so neither may change; if `/data` is lost the bridge becomes a new device and has to be approved again.

From protocol 14.1 on (the default is 16.1) Exchange also wants the device described **inside the initial Provision
request**, the one that downloads the policy: MS-ASPROV 2.2.2.53 makes `settings:DeviceInformation` mandatory there, and
Exchange 2019 answers Provision status 165 (DeviceInformationRequired) without it. A separate Settings command does not count,
and the acknowledgement that follows must not carry it. The bridge sends:

| Element | Value |
|---|---|
| `Model` | `EASBRIDGE_DEVICE_MODEL` (`easbridge`) |
| `FriendlyName` | `EASBRIDGE_DEVICE_FRIENDLY_NAME` (`easbridge (MailFlow bridge)`) |
| `OS` | `EASBRIDGE_DEVICE_OS` (`Linux`) |
| `OSLanguage` | `EASBRIDGE_DEVICE_OS_LANGUAGE` (`en-us`) |
| `UserAgent` | `EASBRIDGE_USER_AGENT` (`easbridge/<version>`), the same string as the HTTP header |
| `EnableOutboundSMS` | `0` |

It never sends an IMEI, a phone number or a mobile operator, and none of the values may name another product: the bridge is
not Outlook, an iPhone or anything else, and the defaults say what it is. Each value is at most 128 bytes without control
characters. On protocol 14.0 the specification forbids the element in Provision; there it goes in a Settings command right
after provisioning. The service logs the values once at start-up, and `easbridge probe` prints them and reads the same
variables (its `--device-*` flags override them).

## First contact with Exchange: `easbridge probe`

Before trusting any behaviour described here, run the probe once from the machine that will host the bridge. After a
successful probe you can start the service and add the account to MailFlow straight away (see "First sync" below). It reads the
password **without echo** (or one line from stdin with `--password-stdin`), then does: OPTIONS and Provision, FolderSync,
a 5-item header-only Sync of the Inbox, one full fetch (size only) and a 60 s Ping. It prints only status codes, counts, sizes
and timings, never addresses, subjects, folder names or bodies. It uses the same DeviceId as the service when it shares
`/data`, so it does not create an extra device partnership, and the same device description as the service (the
`EASBRIDGE_DEVICE_*` and `EASBRIDGE_USER_AGENT` variables). Nothing else is written locally.

Under every step it lists the HTTP requests of that step with their command, status and where the time went: `new
connection` (DNS, TCP and TLS) or `connection reused`, and how long the server, or a gateway in front of it, took to
answer. A slow step can therefore be told apart: more requests than needed, a slow connection, or a slow answer. A refusal
is explained in one line (for example `Provision status 165 permanent=true: server requires device information in
Provision`).

**`--trace`** (off by default) also prints, under each request, the decoded request and response as XML, so that the next
protocol error can be diagnosed from one run: the headers that matter (`MS-ASProtocolVersion`, `X-MS-PolicyKey`; for OPTIONS
the versions and commands the server lists) and every element of the message. The values are shown only for protocol
structure (keys, statuses, ids of folders, sizes, options, the device description, the policy). The value of every other
element, every message body (`Data`), subject, address, folder name, item id, user name and password, and every binary block
is replaced by its length and its kind (`[72024 text bytes not shown]` for an inline string, `[16 binary bytes not shown]` for
an OPAQUE block: the first real Fetch failed on exactly that difference), an element the tables do not know is named `Unknown_<page>_0x<token>` with its value hidden, and
the answer of a gateway that is not WBXML is described by its size. Credentials, cookies and the URL are not in the output.

Run it **before the first start of the service, or with the service stopped.** It provisions the device again and starts
fresh sync chains for the folder list and the Inbox on Exchange, which makes any keys a running service already holds
invalid. The service recovers by itself (status 9 and 3 handling, re-provisioning, then a full resync with UIDs kept), but
it costs a complete resync of the folder list and the Inbox on its next start.

```sh
docker run --rm -it --network mail-egress -v /srv/easbridge/data:/data "$IMAGE" \
  probe --url https://mail.example.com/Microsoft-Server-ActiveSync --user you@example.com
```

Use the same egress network as the service, so the probe tests the same path (gateway, load balancer) the service will use.

Flags: `--url`, `--user`, `--version`, `--data-dir`, `--ping-seconds` (0 skips the Ping), `--ca-file`, `--device-type`,
`--device-model`, `--device-friendly-name`, `--device-os`, `--device-os-language`, `--user-agent`, `--trace`, `--password-stdin` and
`--skip-contacts`.

**Every probe run is a real logon attempt**, and a wrong password counts toward Active Directory account lockout like any other.
Do not run it in a loop, and do not script it with a password you are not sure of.

After the mail steps the probe has two more, on by default: **contacts** (the folders by type, then one Sync of the default
Contacts folder: status, number of items, the *names* of the fields present) and **directory** (a GAL search for the account's own full
address, then, 20 seconds later like any two searches of the service, for its local part: status, Total, entries returned, whether an
entry's `EmailAddress` matched exactly, the names of the fields present), and `gal prefix 1` and `gal prefix 2`: a search for the
first one and the first two characters of the same local part (also 20 s apart), with status, total, entries returned and whether the
account's own entry was among them: these tell whether the directory answers the short queries the sweep needs. No value is ever printed, with or without `--trace`. A failing
step is reported and the probe goes on with the next, then ends with an error. `--skip-contacts` leaves both out. Like the Inbox sync,
the contacts Sync starts a chain of its own on Exchange, which the service recovers from (see above).

### If the probe stops

Run it again with `--trace` and read the request that was refused against MS-ASCMD. The statuses met so far:

| Step | Status | Meaning | What helps |
|---|---|---|---|
| sync boot | Sync 4 (protocol error) | The Sync request breaks the specification. Typical cause: the request for a first SyncKey carried `GetChanges` (MS-ASCMD 2.2.3.84); EASBridge sends the SyncKey and the CollectionId only, so a report of this is a bug | `--trace`, and the request in the report |
| fetch 1 mime | "holds no MIME body the bridge can read" | Exchange answered, but the `airsyncbase:Body` of the answer had no `Data` the library reads as MIME. Typical cause: the MIME came as an inline string (WBXML STR_I) instead of an OPAQUE block; EASBridge takes both forms, so a report of this is a bug | `--trace`: the `Body` shows its `Type` (4 is MIME) and whether `Data` is `text` or `binary` bytes |

### If the probe stops at Provision

Every status below is permanent unless noted: the bridge asks once and does not repeat the request, and a failed login is
remembered like any failed verification (the same password is refused locally for the cooldown, and at most two unsuccessful
verifications per user reach Exchange in 30 minutes).

| Status | Meaning | What helps |
|---|---|---|
| 165 DeviceInformationRequired | The Provision request had no device information (EASBridge sends it; see "Device identity") | Report it as a bug, with `--trace` |
| 139 DeviceNotFullyProvisionable, 141 LegacyDeviceOnStrictPolicy, 145 ExternallyManagedDevicesNotAllowed | A strict device policy refuses a device that cannot apply it, or that sent no policy key | The Exchange administrator must allow the device |
| 129 DeviceIsBlockedForThisUser | The device is blocked or not approved yet | Approve it in the organisation's portal |
| 126, 130, 131 | ActiveSync is disabled for the user, access is denied, the account is disabled | The Exchange administrator |
| 177 MaximumDevicesReached | The user has too many device partnerships | Remove old devices in Exchange |
| 142, 143, 144 (temporary) | The device must provision again | From another command the bridge provisions again itself, once per call, and repeats the call once; as the answer to Provision it is reported |
| 140 or a RemoteWipe element | The server ordered a wipe | The service honours it (see "Security model"); the probe only reports it |
| 2, 3 (Provision), 3, 4, 5 (Policy) | Protocol error, server error, unknown or corrupt policy, out-of-date policy key | See the line the probe prints; 3 and 5 are temporary |

## First-run checks

Some behaviour cannot be proven without the real server. Check it once, with your own mailboxes, before relying on the bridge
for real correspondence.

1. **`easbridge probe`** (above): provisioning, the folder list, one Sync, one fetch, Ping.
2. **Bcc must not leak.** EAS `SendMail` reads its recipients from the message headers, so for a recipient that exists only
   in the SMTP envelope (a Bcc) the bridge writes a `Bcc:` header into the message it hands to Exchange. Exchange is expected
   to strip that header from the copies it delivers, as it does for mail composed in Outlook. (It does on the real Exchange 2019
   it was tested against: the To recipient's copy had no Bcc header, finding 4 in `docs/TESTING.md`.) Check it once more on your
   own server: from MailFlow send one message with **To: your first mailbox** and **Bcc: your second mailbox** (both yours).
   Open each received copy and view its source: the copy in the To mailbox must contain no `Bcc:` header and not the Bcc
   address anywhere, and the Bcc mailbox must have received the message. If the To copy shows the header, stop sending through
   the bridge and report it. Then open the message in MailFlow's Sent folder: it shows the Bcc address (that copy is the
   bridge's own, see "Sent copies and Bcc").

A message submitted twice (MailFlow retries a submission whose answer it never saw) is delivered once: the `ClientId` of the
`SendMail` request is derived from the message's `Message-ID`, so Exchange recognises the repeat (status 118) and the bridge
counts it as sent.

## Sent copies and Bcc

EAS `SendMail` takes its recipients from the message headers, so the bridge writes the recipients that exist only in the SMTP
envelope into a `Bcc:` header before it hands the message to Exchange. Exchange delivers the message without that header and
saves a Sent Items copy without it too, and that copy comes back through EAS as MIME with no Bcc header, while Outlook shows the
Bcc recipients of a sent message. MailFlow looks at a new UID once and does not look again, so the Bcc recipient has to be there
from the first FETCH.

- A message submitted with a Bcc header (and a Message-ID) is kept in `state.db`, as submitted, **before** the request leaves for
  Exchange (sending takes seconds, and Exchange's copy can be picked up before the reply). The Sent copy, whichever comes
  first, is served with those bytes: the placeholder MailFlow's APPEND creates, Exchange's own copy if it was there first (also
  while the Sent folder is still doing its first sync, also for a message that had to be downloaded), and a placeholder that
  binds to Exchange's copy without having them. The UID, the flags and the ServerId binding are the same as ever, so flags,
  moves and deletes act on Exchange's item, and no client is told anything: no new EXISTS, no new UID.
- The kept copy is a way station. It is dropped once the message that serves it is bound to Exchange's item, when that message is
  deleted or moved out of Sent Items, when the Sent folder opens with the message staged, after 24 hours, and beyond 100 messages
  or 128 MiB (oldest first). It is part of the state database: overwritten when deleted (`secure_delete`), gone with a remote wipe.
- IMAP MOVE of the Sent message to another folder keeps serving the bytes there (MOVE copies the stored message). `\Deleted` +
  EXPUNGE does not: Exchange moves the item to Deleted Items itself and renders it without a Bcc header. Messages that did not pass through the bridge, messages without a Bcc header (Exchange's copy lists the same recipients) and messages without a Message-ID show what
  Exchange renders.
- **Messages sent from a phone or OWA** do not pass through the bridge. MS-ASEMAIL 2.2.2.9 defines an `email2:Bcc` element
  (protocol 16.0 and 16.1); the specification uses it for drafts (3.1.5.4) and lets any Sync (3.2.5.4) or ItemOperations
  (3.2.5.2) response carry any element of the E-mail class, but it does not say that Exchange lists the Bcc recipients of a sent
  message, and none has been seen yet. If it does, the bridge writes them into a `Bcc:` header of the copy it serves (Sent Items
  only, never over a Bcc header the message has, never for a message it submitted itself) and logs one line the first time
  (`Exchange lists the Bcc recipients of a message in Sent Items`). Nothing is requested for it. If that line never appears, a
  message sent elsewhere shows Exchange's rendering, without Bcc recipients.

## Contacts and directory (read-only)

On by default (`EASBRIDGE_CONTACTS=off` turns it off). MailFlow 3.9.0 has a read-only CardDAV client, so nothing changes in
MailFlow: the bridge serves a CardDAV address book and MailFlow pulls it (every 15 to 1440 minutes, its own setting) and re-serves the
contacts to phones through its own CardDAV server. Two sources feed it:

- **The mailbox's Contacts folders**, one address book per folder (EAS folder types 9 and 14), synced by polling every
  `EASBRIDGE_CONTACTS_INTERVAL`. The first download of a book is all or nothing: it lists no cards until the whole folder is in, then
  they all appear at once. If Exchange forgets the sync state (a status 3, which `easbridge probe` also causes, because it starts its
  own chain on the shared DeviceId) the folder is downloaded again and its cards are replaced in one step when that is done. Names,
  e-mail addresses, phones by type, company, department, job title, addresses, birthday, anniversary, web page, categories and
  nickname are mapped; the office location is shown as a note, because MailFlow shows only the name, the organization, the e-mail
  addresses, the phones and the note of a card. No photos, no notes (the library does not ask Exchange for the body of a contact), and
  never the government id, the customer id, the instant messaging addresses or the manager's name.
- **A directory built up as you go** from the organization's Global Address List. EAS can only *search* the GAL (at most about 100
  results per query); it cannot list it. So the bridge looks up, **one at a time and rate-limited**, the addresses in the directory
  domains that appear in the From, To and Cc of the mail it holds (the stored messages are read once in the background after start,
  new mail adds candidates as it arrives; the most recent addresses first, the more frequent among equals), and **keeps a card only for
  an entry whose `EmailAddress` equals the address looked for**. For each address it searches the full address; if no entry matches
  exactly, the local part; then the display name from the message header if there is one: at most three searches per address and
  attempt. Every other person a search returns is dropped unread. The GAL is never enumerated or harvested: no wildcard, no
  letter-by-letter search. An address with no match is not asked again for 30 days; a found card is looked up again after
  `EASBRIDGE_DIRECTORY_REFRESH` and removed when two such lookups at least 7 days apart both find no exact match (the person left).
  HomePhone and Alias are never stored. Whether Exchange's search finds an entry by its full SMTP address is not known until the
  probe has run against the real server (`easbridge probe` searches the account's own address and then its local part).

The lookups pause while the circuit breaker is open, honour Retry-After, back off on throttling, time-outs and server errors, run
only while the bridge holds the account's credentials (they are part of the mailbox engine, which exists while a client is
connected plus the grace period), and stop for 24 hours (one log line) when Exchange answers access denied or access blocked.

**Turning the contacts on for an install that already ran.** The bridge remembers whether the hierarchy it stored (the
FolderSync key) was taken with the Contacts folders kept. If the contacts were off when it was stored, Exchange no longer mentions
those folders in an incremental FolderSync, so the first start with the contacts on reads the whole hierarchy once from key 0; the log says `contacts are on and the stored folder hierarchy was taken without the Contacts
folders: reading the hierarchy again in full, once`, and the next `folder sync done` line shows `full=true` and `contact_folders_added`.
The mail is not touched (no new UIDs, nothing announced to IMAP clients); if Exchange also restarts its per-folder sync state, the
mail folders are re-read and reconciled quietly. Turning the contacts off and on again repeats this once.

### The full directory sweep (opt in, experimental)

> **Experimental.** It works only where the directory answers short queries (one or two characters). Where the shortest query it answers
> is three characters or more, walking every string of that length is not feasible (59,319 searches before any division), so the bridge
> stops the sweep for that run, says why in one log line and in `/healthz` (`sweep_infeasible`), and tries again at its next time.
> It is off by default.

By default the Directory book only holds people whose address appeared in your mail. With `EASBRIDGE_DIRECTORY_FULL=on` the bridge
also copies **the whole Global Address List** into the Directory book (the entries of `EASBRIDGE_DIRECTORY_DOMAINS` only: the external
mail contacts a GAL lists are left out). Read the risk first, and then decide:

> **Risk, stated plainly.** Exchange can search its directory but not list it, so the sweep asks for it piece by piece: it searches
> one letter or digit, and for every answer that is cut off at about 100 entries, again with each possible next character. For a
> directory of a few thousand people that is some hundreds to a few thousand searches (about 1,300 for 3,000 entries in the tests;
> 1,600 when the server answers only from two characters), at most 5,000 per sweep, repeated every 30 days by default. To the
> organization's servers this looks like a program enumerating the directory. They may notice it, throttle it, or block the
> device, and the device is the same one that carries your mail, so a block would stop the mail as well. The bridge keeps the pace
> low (the budget below) and stops for 24 hours if Exchange answers access denied, but it cannot make the sweep invisible. Leave it
> off if you are not sure that you may do this.

How it works, in short (details: `docs/DESIGN-contacts.md` section 9):

- **Calibration.** Exchange matches the start of the display name, the first name, the last name and the alias (ambiguous name
  resolution); every mailbox has an ASCII alias, so a walk over alias prefixes reaches everybody, Arabic display names included. First
  the bridge finds the shortest query Exchange answers: it searches the first 1, 2, 3 and 4 characters of the account's own local
  part (the account's own entry is the known positive) and uses the first length that finds something. If none does, there is no sweep
  this time: one log line and `calibration_failed` in `/healthz`. A length of 3 or 4 is answered but not walked: the sweep stops itself
  for the run (`sweep_infeasible` and `sweep_infeasible_length` in `/healthz`, one log line), and removes nothing.
- **Walk.** Every string of that length over `a-z 0-9 . _ -`; a prefix that matches more than a search returns (Total larger than the
  entries returned, or 100 entries and no Total) is divided into the prefix plus each character, and so on. A query Exchange refuses as
  too short or too complex is divided as well. At most 20 characters and 5,000 searches per sweep; reaching either ends the sweep early
  (one log line, `last_sweep_capped` in `/healthz`).
- **Pace and priority.** The sweep spends the budget of the lookups (`EASBRIDGE_DIRECTORY_LOOKUPS_PER_HOUR`: searches at least 20 s
  apart, a daily cap of ten times the hourly value, one search at a time, the circuit breaker, Retry-After, back-off, the 24-hour pause on
  access denied) and only what the lookups of addresses from mail leave: a new address in mail is looked up before the next prefix. With
  the defaults (one search a minute, 600 a day) a sweep of 1,300 searches takes about two and a half days.
- **Restart.** The queue of prefixes is kept in the database, so a restart goes on where the sweep stopped.
- **Removal.** At the end of a sweep that was not cut short, a card that only sweeps found is removed if neither this sweep nor the
  previous finished one saw it, unless a lookup of its exact address confirmed it since the previous sweep started. Nothing is removed
  after a sweep that ended early. Turning the option off drops the sweep's state and removes the cards only it had found at the next
  start; cards that a lookup of the exact address also found stay.
- **Known limit.** An entry whose alias is exactly a prefix that more than 100 other entries share (they all start with that alias) is
  beyond the window of the search for that prefix and is not returned. It is found by the as-you-go lookup of its address if it shows up
  in your mail.
- `/healthz`, per mailbox, `directory_sweep`: `full`, `calibrated_length`, `calibration_failed`, `state` (`idle`, `running`, `waiting`
  with `waiting_until_unix`), `prefixes_done`, `prefixes_queued`, `searches_this_sweep`, `entries_found_this_sweep`, `last_sweep_unix`,
  `last_sweep_capped`: counts only.
- Whether Exchange answers short queries (one or two characters) differs between servers; the probe's `gal prefix` steps tell. How a
  server reacts to a sweep only a run tells.

### Setting it up in MailFlow 3.9.0

`easbridge init` prints these steps with your values, and [INSTALL.md](INSTALL.md) has them in full. In short:

1. An administrator of MailFlow turns on **Settings > Security & Integrations > Security > Mail Server Connection Policy > Allow
   private / local hosts** (it is needed for the mail account and for CardDAV alike).
2. In MailFlow open **Settings > Security & Integrations > Integrations > Apps** and find the **CardDAV Contacts** card. Fill in
   - *Server URL*: `https://easbridge:8443/`,
   - *Username* and *App password*: the mailbox login and password, the same as for IMAP (the bridge checks them with the very same
     rules, allow-list and lockout protection included),
   - *When a contact already exists* and *Sync interval (minutes)* as you like,

   and press **Connect**. MailFlow checks the login, finds the address books and starts a first sync.
3. The contacts appear on MailFlow's Contacts page, one address book per Contacts folder plus the Directory, marked "Synced from
   CardDAV". They are read-only there. If the books are empty at first, the first download is still running: look at the health
   report (`docker exec easbridge /easbridge healthcheck -v`, `contacts.books[].complete`).

MailFlow shows the first part of the organization, the name, e-mail addresses with their type, phones by type and the note;
everything else of a card (job title, addresses, birthday ...) is stored by MailFlow with the card and reaches phones through its
own CardDAV server.

### The CardDAV server

Hand-written on `net/http`, no dependency. Paths: `/.well-known/carddav` (301 to `/dav/`), `/dav/principals/me/`,
`/dav/addressbooks/me/` (the books), `/dav/addressbooks/me/<book>/` (`contacts`, `f-<hash>` for a user folder, `directory`),
`<book>/<uid>.vcf`; `/` answers like `/dav/` so the bare origin works as the URL. Methods: OPTIONS (`DAV: 1, 3, addressbook`),
PROPFIND (Depth 0 and 1; Depth infinity is 403 `propfind-finite-depth`), REPORT (`addressbook-query`, `addressbook-multiget`; a
filter with a `prop-filter` is 403 `supported-filter`, never silently ignored), GET and HEAD. **Everything that writes gets 405** with an
`Allow` header; privileges say read only; `sync-collection` is not offered (MailFlow does not use it). Cards are vCard 3.0 with CRLF,
escaped values and lines folded at 75 octets without splitting a UTF-8 sequence; the UID is stable (`eas-` and 32 hex digits of a hash
of account, folder and item; `dir-` and 32 hex digits of the hash of the lower-case address), the ETag the quoted SHA-256 of the
card, the ctag changes whenever a card of the book changes. TLS 1.2 or later over HTTP/1.1, 10 s header and 30 s read timeouts, 16
KiB of headers, 64 KiB of body, at most 8 requests at once, a DOCTYPE or ENTITY in a request is refused. HTTP Basic goes through the
**same login as IMAP LOGIN**: a wrong password is `401` (`WWW-Authenticate: Basic realm="easbridge", charset="UTF-8"`) and counts
exactly as an IMAP failure does; a login refused locally for any other reason, or Exchange being unreachable, is `503` with
`Retry-After`, never a 401. Design and decisions: `docs/DESIGN-contacts.md`.

## Security model

- **Credentials are pass-through and never persisted.** The IMAP/SMTP username and password are used as the EAS Basic
  credentials. They live in process memory only (inside the engine for that mailbox) and are never written to disk or logs.
  A log scrubber removes any registered secret (and its Basic/SASL encodings) from every log line, and tests scan full
  sessions for the test password.
- **Allow-list.** Only `EASBRIDGE_ALLOWED_USERS` can authenticate; other names are rejected without touching Exchange, so
  the bridge cannot be used as a password oracle for names outside it. **A domain entry (`@example.com`) widens this on purpose:**
  anyone who can reach the bridge and has a login in that domain can try a password through it. Exchange still decides, and the
  lockout protection below applies per login (a few unsuccessful verifications per login in a window, then refusal). Because any name of
  the domain gets that far, one source address may also present at most 10 names that have not been verified in 10 minutes; further names
  from it are refused locally and no database row, lock or cache entry is made for a name that was never verified. Use plain addresses
  where you can name the people, and keep the listeners on a network you trust.
- **Lockout protection** (every failed logon counts against the Active Directory account lockout, so the bridge asks
  Exchange as rarely as it can). All of this is in memory only; restarting the bridge clears it.
  - A password Exchange rejects (HTTP 401) is remembered as a salted hash and refused locally with `AUTHENTICATIONFAILED`.
    So is a verification that failed for any other reason (a gateway's 403, a redirect, an HTML logon page, a timeout), but
    that one is answered `NO [UNAVAILABLE]` (SMTP 454), because the password may well be right.
  - A hash that fails again is refused for longer each time: 15 minutes, 1 hour, 4 hours, then 24 hours. A different password
    or a success starts again.
  - At most 2 unsuccessful verifications reach Exchange for a user in any 30 minutes; beyond that every login that is not
    already verified is refused locally with `NO [UNAVAILABLE]`, whatever password it carries.
  - A password that has been verified keeps the fast path: later logins (MailFlow reconnects often) never reach Exchange.
    Concurrent logins share one verification.
  - A failure that cannot have counted at Active Directory costs nothing: no connection, a DNS or TLS handshake failure, or a
    client that went away. An answer that says the server is busy or silent (HTTP 429, a 5xx, a time-out) is no refusal either: the
    same password waits a fixed 60 seconds and uses no budget. A 401, a 403, a redirect that is not followed and an HTML logon page
    count and escalate.
  - **What stops traffic during a session.** A 401 is never retried: all Exchange traffic for that mailbox stops until a client logs in
    with a working password. A 403, a redirect that is not followed or an HTML page in answer to an EAS request (how a gateway refuses a
    credential) is counted against the same per-user budget, and the second one in a row stops the mailbox the same way and drops
    the cached verification, so the next login goes through the guarded path above. A 404 and genuine network errors are retried with
    the back-off. A late 401 for a password that has since been replaced is ignored.
- **No per-connection limits.** The IMAP, SMTP and CardDAV servers do not limit connections or request rates per client (the
  lockout rules above protect the mailbox password, not the listeners). Keep the ports on a private network; never publish them.
- **Gateway cookies** that Exchange or a gateway in front of it sets (for example an access-policy session cookie) are kept per
  mailbox, in memory only, so the gateway does not open a new session for every request.
- **TLS everywhere.** Listeners are implicit TLS; AUTH is impossible without it; the Exchange URL must be https. Redirects
  from Exchange are followed only if they stay on https and on the same host and port (at most 5): the Basic credentials are
  never taken to another host or onto a plain connection. A refused redirect shows up as an HTTP 3xx error, logged with the
  status only.
- **Hostile mail is bounded.** Message structure (BODYSTRUCTURE, ENVELOPE, `BODY[n.m]`, SEARCH) is computed once, when the
  message is stored, with hard limits: 32 levels of nesting, 1,000 parts, 128 KiB of header per part, a scan budget
  proportional to the size. A message beyond a limit is shown as one `application/octet-stream` part: it stays visible and its
  bytes stay downloadable. A deliberately hostile message that would need gigabytes to describe takes about 200 KB.
- **Circuit breaker.** Every mailbox has one. It pauses all its Exchange traffic after 3 failed writes to the local database
  in a row, after 3 server errors (HTTP 5xx, transient EAS statuses) in a row, and at once on HTTP 429/503 or EAS status 111.
  The pause starts at 5 s, doubles with every consecutive trip up to 15 minutes and is never shorter than the server's
  `Retry-After`. It is visible in `/healthz`.
- **Exposure.** The listeners bind the one IP you name (`EASBRIDGE_LISTEN_IP` or the listener variables), or, by default, all
  addresses of a container, which the bridge allows only in a namespace that looks like a container's (see "Running it"); `0.0.0.0`,
  `::` and an empty host in a variable are refused unless `EASBRIDGE_ALLOW_ANY_BIND=true`.
- **Honest device identity** (see "Device identity": a fixed DeviceId and DeviceType, a device description that says what the
  bridge is and nothing a phone would have) and **remote wipe is honoured**: on a wipe order the bridge writes `/data/WIPED`, deletes the
  state database (the cache of your mail), acknowledges the wipe, closes all sessions, logs one loud line and refuses logins
  until you delete the marker. Exchange 2016/2019 can also order an *account only* wipe (EAS 16.1, `AccountOnlyRemoteWipe`);
  the bridge keeps nothing but the cache of the mailbox, so it does the same and acknowledges with that order's own element.
- **Logs** never contain credentials, subjects, addresses or bodies, and with contacts on no names, phone numbers, search queries or
  vCards either: statuses and counts only. The `User` query value is removed from request URLs.
- **The automatic CA's key lives in the data volume** (`/data/tls/ca-key.pem`, `0600`). Anyone who can read that volume can make certificates for any name that MailFlow, once it trusts the CA, will accept. Protect the volume like the database, and do not share the exported `ca.pem` file's directory writable with anything else (it holds only the public certificate, but a writable shared directory lets another container replace it).
- **At rest.** `state.db` holds a cache of your mail (the bridge-only drafts, and for a day the messages just sent with a Bcc
  recipient, see "Sent copies and Bcc"), with contacts on of your contacts and the directory cards too, and is not encrypted by easbridge: protect
  the data directory (disk or volume encryption, `0700` directory; the database file is created `0600`). SQLite runs with
  `secure_delete`: a deleted message is overwritten in the file instead of lingering in free pages (the write-ahead log can
  still hold older page images until the next checkpoint overwrites them).

## First sync

While a folder downloads for the first time, MailFlow sees it as an empty folder; when the download ends the whole folder
appears in one step. There is nothing to wait for before adding the account, but a large folder stays empty until its
last message is in (the Inbox is synced first, then the other folders one after another). Progress is in `/healthz`
(`initial_sync_complete`, `initial_sync_pending`, `staged_items`, and one entry per folder in `folder_progress`).

Details worth knowing:

- A folder opens when its sync has caught up and no message is still waiting for its body. Mail that arrives meanwhile is
  part of the first batch. Afterwards new mail gets the next UID, as usual.
- A message that cannot be downloaded for reasons of its own (Exchange answers with a permanent error status for it, or sends
  no body; five times) stops holding its folder back; the folder opens without it and the message follows as an ordinary new
  message once it can be fetched. Connection trouble and transient statuses such as "server busy" do not count: however long
  an outage lasts, the folder stays closed. After ten such failures the message is replaced by a small stand-in with the
  subject, sender, recipients and date that Exchange lists for it and a plain note as its body (header
  `X-Easbridge-Stand-In`), so a message Exchange can never render does not stay invisible. It is final.
- If Exchange throws the folder's sync state away during the first download, the download starts again from scratch. After the
  folder has opened, such a reset keeps UIDs and never hides the folder again.
- Messages that need a UID at once get one even in a folder that is still downloading, so they sit below the folder's
  other messages: Sent copies appended by the client (APPENDUID), drafts appended by the client, and messages moved into
  a folder by MOVE (COPYUID; clients such as MailFlow track the moved message by it). The one exception is the Inbox: a
  message moved into an Inbox that is still downloading waits with the rest and the MOVE answers without COPYUID, because
  a known UID in the Inbox would make MailFlow alert for everything that follows.
- A Sent message that Exchange delivers during the first download is matched with the client's own copy when the folder opens
  (same rules as later), so it does not appear twice.
- The very first login on a fresh database waits (up to 15 s) until the folder list has been fetched, so that the client's
  first LIST or SELECT does not find an account without an Inbox.
- A folder that appears later, whether created in OWA or by a client's CREATE, starts closed like any other and opens when
  its first download ends, usually at once. A MOVE into a freshly created folder still gets its COPYUID right away (see
  above), which is what MailFlow's Snoozed folder relies on.

## Limits

- No calendar, tasks or notes. Contacts are **read-only** (no write-back, no photos, no contact notes), and exist only while
  `EASBRIDGE_CONTACTS` is `on`. Mail folders over IMAP only (Outbox is hidden).
- Servers differ in what their company address book answers: whether a search finds an entry by its full SMTP address (the
  three-search ladder exists because some do not), and whether it answers short queries (the full sweep depends on that). The probe
  tells for your server. How long the download of a large Contacts folder takes through a gateway also depends on it (windows of
  100 items, photos included).
- Without the sweep the directory only knows people whose address appeared in mail the bridge holds, one search at a time: a new correspondent is in
  the directory after the next lookup, which with the default 60 lookups an hour can be a while when many addresses are new.
- A database migrated by a newer version cannot be opened by an older one. Rolling back needs the copy of the data directory taken before the upgrade.
- **Drafts are local**: drafts APPENDed by the client live only in the bridge, not on Exchange, and are lost with the data
  directory. Drafts created elsewhere (OWA, phone) do sync in.
- **COPY is refused** (`NO [CANNOT]`); EAS has no copy. MOVE works and returns COPYUID.
- No CONDSTORE, QRESYNC, COMPRESS, ID, ESEARCH, SORT, THREAD or server-side search; SEARCH is evaluated locally over the
  cache. MailFlow treats the missing CONDSTORE as "no baseline" and does full flag scans, which are answered from the local
  database.
- `\Answered`, `$Forwarded` and other keywords are stored locally only (go-activesync discards Exchange's
  LastVerbExecuted). `\Deleted` is local until EXPUNGE.
- One Exchange server per bridge.
- Widening `EASBRIDGE_FILTER` on an installation that has already synced brings older mail in as new messages with higher
  UIDs, which MailFlow can treat as new mail. Choose the window before the first start.
- Unmatched Sent placeholders (Exchange never delivered its copy) stay as local-only messages after 24 hours.
- A Sent copy is matched with the client's by Message-ID. The subject + date + recipients fallback is used only when one of the
  two has no Message-ID: two different messages that look alike are never merged. If Exchange rewrites the Message-ID of its
  copy, the Sent folder shows both copies.
- A delete that Exchange refuses brings the message back, as a new message with a new UID (the received date and the
  read/flagged state are kept).
- The Sent folder shows the Bcc recipients of messages that were sent through the bridge (see "Sent copies and
  Bcc"). Messages sent earlier, messages sent from a phone or OWA (unless Exchange lists their Bcc recipients, which has not been
  seen) and the copy that Exchange moves to Deleted Items when a Sent message is expunged show Exchange's rendering, without them.

## Operating notes

- `GET /healthz` (loopback only by default) returns JSON without mailbox content: `version`, `status`, `imap_listening`,
  `smtp_listening`, `wiped` and, per mailbox (identified by a short tag, not the address), `folders`, `pending_fetch`,
  `outbox`, `syncing` (folders still waiting for their sync), `sessions`, `last_sync_unix`, `ping_heartbeat_s` and
  `auth_failed`, the circuit breaker (`paused`, `paused_until_unix` and `pause_reason` while it is open, `pause_trips`), plus
  the first-sync progress (counts only, no folder names): `initial_sync_complete` and
  `initial_sync_pending` (numbers of folders), `staged_items` (messages downloaded but not visible yet) and `folder_progress`
  (one `{initial_sync_complete, staged, pending_fetch}` entry per folder). With the contacts feature on, `carddav_listening` and, per
  mailbox, `contacts`: `books` (per book `complete`, `downloading`, `cards`), `directory_enabled`, `directory_cards`, `directory_queue`
  (candidates waiting for a lookup), `directory_lookups_today` and, while paused, `directory_paused_until_unix` (counts only, no names).
  HTTP 503 when a listener is down or the bridge was wiped.
- A message that cannot be downloaded stays invisible and is retried with backoff; it is logged at error level after five
  attempts, once more if it stops holding its folder's first sync back, and a last time when it is replaced by a stand-in
  (see "First sync").
- While the circuit breaker is open nothing leaves for Exchange; clients get `NO [UNAVAILABLE]` for commands that need
  Exchange and keep reading the local cache. The log says why and for how long (one line per trip).
- If Ping keeps failing (for example a proxy that cuts every held request even at the shortest heartbeat) the bridge logs a
  warning and syncs every mail folder every 5 minutes instead, so new mail still arrives, only later. It returns to push as
  soon as a Ping succeeds. The same sweep covers folders that a server-side limit keeps out of the Ping set.
- If Exchange answers 401 mid-session the engine stops all Exchange traffic until a client logs in with a working password.
- After a silent sync-key reset by Exchange the folder is resynced and messages deleted in the gap are removed only after
  two complete resyncs agree (a verification resync runs 5 minutes after the first).

## Development

```sh
source env.sh                    # project-local Go toolchain and caches
make check                       # vet, tests (plain and -tags devfake), vendored go-activesync tests, production-binary check
make e2e                         # the Docker end-to-end run (skips without Docker)
make compat                      # imapflow 1.7.8 / nodemailer 10.0.13 and MailFlow 3.9.0's CardDAV client against a local devfake bridge (needs node, npm and, once, the network)
make build                       # bin/easbridge (production, no fake upstream)
make vulncheck                   # known vulnerabilities in the dependencies: scripts/govulncheck.sh, pinned govulncheck
```

- `scripts/govulncheck.sh` runs the pinned `govulncheck` with the project-local toolchain and caches (only the Go module
  proxy and `vuln.go.dev` are contacted). It must report that your code is affected by 0 vulnerabilities. It also lists
  vulnerabilities in required modules that the code does not call. Run it before every release; to move to a newer tool,
  change `GOVULNCHECK_VERSION` in the script. CI runs `make check`, `make compat`, `make e2e` and `make vulncheck` on every push; they
  must stay green on `main`.
- Build tag `devfake` compiles an in-memory fake Exchange (`internal/fakeupstream`) and a loopback control API into the
  binary. It is for tests and the compatibility harness only; the production build and the image do not contain it
  (`make check-prod` verifies this).
- Layout: `cmd/easbridge`, `internal/{config,logx,store,engine,upstream,easupstream,fakeupstream,imapd,smtpd,carddav,vcard,mimeutil,mimestruct,auth}`,
  `third_party/go-activesync` (vendored v1.1.0 with fifteen documented patches, see `PATCHES.md` there), `compat/` (Node harness; it
  also runs MailFlow 3.9.0's CardDAV client, fetched at test time into a git-ignored cache and checked against pinned SHA-256 sums).
- `docs/DESIGN.md` is the specification (`docs/DESIGN-contacts.md` for contacts); `docs/TESTING.md` records status, deviations, known
  gaps and how to resume.
