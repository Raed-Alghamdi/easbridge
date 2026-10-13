# EASBridge configuration reference

All configuration is environment variables. Two are required: `EASBRIDGE_EAS_URL` and `EASBRIDGE_ALLOWED_USERS`. With
[Docker Compose](../INSTALL.md), `easbridge init` writes them for you into `easbridge.env`. This page lists every setting, then
explains how EASBridge listens, how it gets its certificate, and how it presents itself to Exchange.

- [All settings](#all-settings)
- [Running the container by hand](#running-the-container-by-hand)
- [Where does it listen?](#where-does-it-listen)
- [TLS: which certificate do the listeners use?](#tls-which-certificate-do-the-listeners-use)
- [Device identity](#device-identity)

## All settings

### Required

| Variable | Meaning |
|---|---|
| `EASBRIDGE_EAS_URL` | The ActiveSync endpoint. **https only**; `http://` is refused |
| `EASBRIDGE_ALLOWED_USERS` | Comma-separated login names. Anyone else is rejected locally, without contacting Exchange. An entry that starts with `@` (`@example.com`) allows every login of exactly that domain (any case, no subdomains); read [what that means](SECURITY-MODEL.md#who-can-sign-in-through-easbridge) first |

### Exchange connection and device

| Variable | Default | Meaning |
|---|---|---|
| `EASBRIDGE_EAS_VERSION` | `16.1` | Preferred protocol version (`16.1`, `16.0`, `14.1`, `14.0`). One OPTIONS at engine start; falls back to `14.1` |
| `EASBRIDGE_EAS_CA_FILE` | (none) | Extra CA certificates (PEM) to trust when talking to Exchange |
| `EASBRIDGE_DEVICE_TYPE` | `easbridge` | EAS DeviceType, part of the identity Exchange approves (honest on purpose; do not impersonate Outlook). Changing it makes the bridge a new device |
| `EASBRIDGE_DEVICE_MODEL` | `easbridge` | Model in the device information (see [Device identity](#device-identity)); must not be empty |
| `EASBRIDGE_DEVICE_FRIENDLY_NAME` | `easbridge (MailFlow bridge)` | FriendlyName in the device information, what Exchange shows in its device list |
| `EASBRIDGE_DEVICE_OS` | `Linux` | OS in the device information |
| `EASBRIDGE_DEVICE_OS_LANGUAGE` | `en-us` | OSLanguage in the device information |
| `EASBRIDGE_USER_AGENT` | `easbridge/<version>` | HTTP User-Agent, also sent as the UserAgent of the device information. It follows the version; if a rule on the Exchange side matches the exact string, pin it with this variable |
| `EASBRIDGE_FILTER` | `all` | Sync window: `all`, `1d`, `3d`, `1w`, `2w`, `1m`. With a window, mail older than it is not synced and may disappear from IMAP as it ages out |
| `EASBRIDGE_MIME_INLINE_MAX` | `1MiB` | Largest message body accepted inline in a Sync response; bigger ones are fetched with ItemOperations |
| `EASBRIDGE_PING_HEARTBEAT` | `480` | Ping heartbeat in seconds (or a duration like `8m`). Steps down towards 120 s if long requests are cut early |

### Listeners

How these combine is explained in [Where does it listen?](#where-does-it-listen).

| Variable | Default | Meaning |
|---|---|---|
| `EASBRIDGE_LISTEN_IP` | (unset) | One IP literal (v4 or v6) that IMAP, SMTP and CardDAV bind (ports 993, 465, 8443) unless their own variable is set. It must be an address of the container, or the bridge does not start. Unset: all of the container's addresses, with the host-network guard |
| `EASBRIDGE_IMAP_ADDR` / `EASBRIDGE_SMTP_ADDR` | (from `EASBRIDGE_LISTEN_IP` or all container addresses; 993 / 465) | `IP:port` to listen on, for example `10.0.0.5:993`. Wins over `EASBRIDGE_LISTEN_IP`. An IP address is required; an address for every interface (`:993`, `0.0.0.0`, `::`) is refused unless `EASBRIDGE_ALLOW_ANY_BIND=true` |
| `EASBRIDGE_CARDDAV_ADDR` | (from `EASBRIDGE_LISTEN_IP` or all container addresses; 8443) | `IP:port` of the read-only CardDAV listener (TLS, the certificate of IMAP and SMTP). The same bind rules as IMAP and SMTP; it must differ from their addresses and from the health address |
| `EASBRIDGE_HEALTH_ADDR` | `127.0.0.1:8081` | Health endpoint (`/healthz`), content-free. The same rule about every interface applies |
| `EASBRIDGE_ALLOW_ANY_BIND` | (unset) | `true` (or `1`) allows listen addresses without an IP in a variable, and turns the host-network guard of the wildcard default off. Not needed in a container |

### TLS

How these combine is explained in [TLS](#tls-which-certificate-do-the-listeners-use).

| Variable | Default | Meaning |
|---|---|---|
| `EASBRIDGE_TLS_CERT` / `EASBRIDGE_TLS_KEY` | (unset; files at `/tls/server.pem` and `/tls/server-key.pem` count as given) | Your own certificate and key (provided mode, used as they are). Both or neither. With neither and no files at the default paths EASBridge makes its own |
| `EASBRIDGE_TLS_NAMES` | (unset) | Comma-separated extra names of the automatic server certificate: DNS names, or IP addresses. Always included: `easbridge`, the host name, `EASBRIDGE_LISTEN_IP`, the container's addresses |
| `EASBRIDGE_CA_EXPORT_DIR` | (unset) | Directory (a volume) that receives a copy of the automatic CA certificate as `ca.pem`, for MailFlow's `NODE_EXTRA_CA_CERTS`. Compose: `/ca` |

### State, logs and limits

| Variable | Default | Meaning |
|---|---|---|
| `EASBRIDGE_DATA_DIR` | `/data` | State directory (`state.db`, and the `WIPED` marker) |
| `EASBRIDGE_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `EASBRIDGE_LOG_FORMAT` | `text` | `text` or `json` |
| `EASBRIDGE_AUTH_FAIL_COOLDOWN` | `15m` | How long a password Exchange rejected is refused locally |
| `EASBRIDGE_ENGINE_GRACE` | `30m` | How long a mailbox engine outlives its last client session |
| `EASBRIDGE_MAX_MESSAGE_BYTES` | `35MiB` | Size limit for SMTP and APPEND |

### Contacts and company directory

What these do is explained in [CONTACTS.md](CONTACTS.md).

| Variable | Default | Meaning |
|---|---|---|
| `EASBRIDGE_CONTACTS` | `on` | `on` or `off`. Off: no contacts sync, no company directory lookups, no CardDAV listener. Off together with `EASBRIDGE_CARDDAV_ADDR` is a configuration error |
| `EASBRIDGE_CONTACTS_INTERVAL` | `30m` | How often each Contacts folder is synced (5 minutes to 24 hours). Polling: contacts folders are not part of Ping |
| `EASBRIDGE_DIRECTORY_DOMAINS` | the domains of `EASBRIDGE_ALLOWED_USERS` | Comma-separated domains whose addresses are looked up in the company directory; `none` turns the directory off |
| `EASBRIDGE_DIRECTORY_LOOKUPS_PER_HOUR` | `60` | 1 to 180. One directory search at a time, consecutive searches at least `max(20 s, 1 h / value)` apart (that spacing is what caps the value at 180), and at most ten times the value per UTC day |
| `EASBRIDGE_DIRECTORY_REFRESH` | `720h` | How long a found directory card is kept before it is looked up again (24 hours to 365 days) |
| `EASBRIDGE_DIRECTORY_FULL` | `off` | `off` or `on`: the opt-in, **experimental** [full sweep](CONTACTS.md#the-full-directory-sweep-opt-in-experimental) of the company directory. `on` needs the contacts (`EASBRIDGE_CONTACTS` not `off`) and a directory (`EASBRIDGE_DIRECTORY_DOMAINS` not `none`); the combination is refused at start otherwise |
| `EASBRIDGE_DIRECTORY_FULL_INTERVAL` | `720h` | From the end of one finished sweep to the start of the next (168 hours to 365 days) |
| `EASBRIDGE_CONTACTS_BOOK_NAME` | `Exchange contacts` | Name of the address book of the default Contacts folder (a user-created folder keeps its own name) |
| `EASBRIDGE_DIRECTORY_BOOK_NAME` | `Directory` | Name of the directory address book; must differ from the other |

## Running the container by hand

[INSTALL.md](../INSTALL.md) uses Docker Compose. If you start the container yourself, put it on the network MailFlow's backend uses,
give it the two required values and a data volume, and start it **before** MailFlow's backend. In the examples, `$IMAGE` is the
image, for example `ghcr.io/raed-alghamdi/easbridge:<version>`.

**The minimal form.** The container needs a route to Exchange. If MailFlow's network is an internal one (no route out), also attach
an egress network, as in the strict form.

```sh
docker run -d --name easbridge --restart unless-stopped \
  --network <the network MailFlow is on> \
  -e EASBRIDGE_EAS_URL=https://mail.example.com/Microsoft-Server-ActiveSync \
  -e EASBRIDGE_ALLOWED_USERS=you@example.com \
  -v easbridge-data:/data \
  "$IMAGE"
```

EASBridge listens for IMAP (993), SMTP submission (465) and CardDAV (8443) on all of the container's addresses, and MailFlow reaches
it by the container's name (`easbridge`) on that network. The contacts feature is on. The bridge refuses to start with this default
if its network namespace does not look like a container's (host networking, for example; see
[The host-network guard](#the-host-network-guard)). EASBridge makes its own certificate; hand its CA to MailFlow as described under
[Giving MailFlow the CA](#giving-mailflow-the-ca).

**The strict form.** The hardened shape for a host that also faces other networks: read-only root, no capabilities, resource limits,
a fixed address on a private network, and the listeners bound to that address only.

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

**Never publish these ports** (`-p 993:993` and the like). IMAP, SMTP and CardDAV have no per-connection limits and no protection
against a flood of connections or of password guesses beyond the per-login lockout rules. They are meant for a private Docker network
that only MailFlow's backend and EASBridge share. The image declares no `EXPOSE` on purpose.

Any Docker host works. The container templates of NAS and panel systems map the same variables, paths and options.

### Notes on the container

- **Two networks.** There are no host ports. The bridge needs the private network MailFlow's backend uses (`mail-internal`, where
  MailFlow reaches it as `easbridge:993` and `easbridge:465`) and an egress network that can reach Exchange (`mail-egress`). An
  internal Docker network has no route out, so with only the first one the bridge could never reach Exchange. Older Docker versions
  take a single `--network` at creation, hence create, `network connect`, start.
- **Resource limits.** `--memory=1g` and `--pids-limit=256` bound what a hostile message or a bug can take from the host (the bridge
  parses mail from the outside world, with hard limits of its own; this is the second wall). `--ulimit core=0` keeps a crash from
  writing a core dump, which would contain passwords and mail from the process memory.
- **`/data`.** The image has `/data` owned by uid 65532 with mode 0700 and declares it a volume, so a named Docker volume
  (`-v easbridge-data:/data`) is created writable for the bridge with no `chown`. A host directory mounted over it
  (`-v /srv/easbridge/data:/data`) must be writable by uid 65532 (`chown 65532:65532 /srv/easbridge/data`). The state database lives
  there.
- **Read-only root.** The root file system can be read-only (`--read-only`): the bridge writes only under `/data`, and SQLite keeps
  its temporary data in memory.
- **Ports below 1024.** Binding ports 993, 465 and 8443 as uid 65532 with `--cap-drop=ALL` relies on Docker's default
  `net.ipv4.ip_unprivileged_port_start=0` inside containers (Docker 20.10 and later). If your runtime differs, pass
  `--sysctl net.ipv4.ip_unprivileged_port_start=0` or put the ports of the listener variables above 1023.
- **Contacts are on by default** (`EASBRIDGE_CONTACTS=on`), with the CardDAV listener on port 8443 by the rule in
  [Where does it listen?](#where-does-it-listen). Set `EASBRIDGE_CONTACTS=off` for no CardDAV listener, no contacts sync and no
  company directory lookups at all (mail only). `off` together with `EASBRIDGE_CARDDAV_ADDR` is refused at start. An address in
  `EASBRIDGE_CARDDAV_ADDR` must differ from the IMAP, SMTP and health addresses; like them it uses the bridge's TLS certificate.
- **Health check.** `HEALTHCHECK` runs `/easbridge healthcheck` (the image has no shell or curl). It asks
  `http://127.0.0.1:8081/healthz`. See [OPERATIONS.md](OPERATIONS.md#health) for reading the report.
- **Building the image yourself.** `docker build -t local/easbridge:<version> .` is a multi-stage build: a `golang:1.27.2` builder with
  `CGO_ENABLED=0` and `-trimpath`, and a final image `gcr.io/distroless/static-debian12:nonroot` (uid 65532). The base images and the
  BuildKit frontend named by `# syntax=` are pinned by digest in the `Dockerfile`; a test fails if one is not. The builder is the Go
  release of the project-local toolchain.

## Where does it listen?

Each of the three listeners (IMAP, SMTP, CardDAV) takes its address from the first of these that is set:

1. its own variable (`EASBRIDGE_IMAP_ADDR`, `EASBRIDGE_SMTP_ADDR`, `EASBRIDGE_CARDDAV_ADDR`: `ip:port`);
2. `EASBRIDGE_LISTEN_IP` (one IP literal, v4 or v6, on the default port: 993, 465, 8443);
3. the default, which is **all of the container's addresses**.

Setting `EASBRIDGE_LISTEN_IP` is the strict choice. The bridge is attached to two networks, and a listener on every address answers
on both, so anything on the egress network could reach the IMAP and SMTP ports. Give the container a fixed address on `mail-internal`
(`--ip 10.0.0.5`; the network must be a user-defined one whose subnet contains it, so pick a free address of yours) and name it in
`EASBRIDGE_LISTEN_IP`. It must be an address of the container: otherwise the bridge does not start and says which addresses exist.

- The health endpoint stays on `127.0.0.1:8081`.
- The start log names the mode: `listening on all container addresses (set EASBRIDGE_LISTEN_IP to restrict)` or
  `listening on 10.0.0.5 only`.
- `:993`, `0.0.0.0:993` and `[::]:993` in a listener variable are refused unless `EASBRIDGE_ALLOW_ANY_BIND=true` is set.

### The host-network guard

With the wildcard default (no `EASBRIDGE_LISTEN_IP`) the bridge refuses to start in a network namespace that does not look like a
container's, because a wildcard bind on a host is open to every network of the host. The rule: leave out the loopback; **every other
interface must be named `eth` and digits** (`eth0`, `eth1`). A container has `lo` and `eth0` (or `eth0` and `eth1` on two networks).
A host has `enp*`, `eno*`, `wlan0`, `wlp*`, `docker0`, `br-*`, `veth*`, `tailscale0` or `wg0`, and any one of them is enough to refuse.

The message says to set `EASBRIDGE_LISTEN_IP` or the three listener variables, or `EASBRIDGE_ALLOW_ANY_BIND=true` if listening on
every address there is what you want. A configuration that gives all three listener variables is never checked.

The guard is a guess, and it can be wrong in two ways:

- Host networking on a machine whose only interface is called `eth0` passes it.
- A container with a VPN interface, or Podman's `pasta`, is refused. Set `EASBRIDGE_LISTEN_IP` for those.

## TLS: which certificate do the listeners use?

EASBridge serves IMAP, SMTP and CardDAV over **implicit TLS only**; there is no plaintext listener and no STARTTLS. The certificate
comes in one of two ways, and the health report says which (`tls_mode`, with `tls_server_expires_in_days`; see
[OPERATIONS.md](OPERATIONS.md#health)).

### Automatic certificate (the default)

If you provide no certificate files, EASBridge makes a private CA (certificate authority) and a server certificate at its first start,
in `/data/tls/` (`ca.pem`, `ca-key.pem`, `server.pem`, `server-key.pem`). The directory is `0700`, the keys are `0600`, and all are
owned by the user the bridge runs as.

- The keys are ECDSA P-256.
- The CA is valid 20 years and has a unique name (`EASBridge local CA` and a random id). It never changes, so MailFlow needs to
  trust it once.
- The server certificate is valid 2 years and is **renewed from the same CA 60 days before it ends, while the bridge runs**. The
  listeners pick it up without a restart.
- Later starts reuse the files; a valid set is never regenerated.

The server certificate names:

- DNS `easbridge`, the container's host name, and every name in `EASBRIDGE_TLS_NAMES` (comma-separated; an entry that is an IP
  address becomes an IP name);
- IP names for `EASBRIDGE_LISTEN_IP` (if set) and the container's own non-loopback addresses.

**The CA is name-constrained.** MailFlow's backend trusts it through `NODE_EXTRA_CA_CERTS`, and Node applies that to *every* outbound
TLS connection the backend makes, not only to EASBridge. A CA that could certify any name would be a skeleton key for whoever reads
the data volume. So the CA is made with critical name constraints: it may certify only the DNS names it was created for (`easbridge`,
the host name at that time, the entries of `EASBRIDGE_TLS_NAMES`) and IP addresses in the loopback, RFC 1918, unique-local
(`fc00::/7`) and link-local ranges. Node and OpenSSL refuse a certificate for any other name, whoever signed it.

**When the addresses change** (a new `EASBRIDGE_LISTEN_IP`, an `EASBRIDGE_TLS_NAMES` address, a new container address), a new server
certificate is issued from the same CA at start. The host name and the container's own addresses are best effort: a recreated
container has a new host name, which the CA was not made for, so it is simply left out.

**When a required name is outside the constraints** (a new DNS name in `EASBRIDGE_TLS_NAMES`, a public address in
`EASBRIDGE_LISTEN_IP`), the start stops with one line that says how to start over:

1. Stop EASBridge.
2. Remove the directory `/data/tls` in the data volume. A new CA is made at the next start.
3. Start EASBridge, then **restart MailFlow's backend**, because Node reads the CA only when it starts.

A set whose CA is incomplete, does not match its key or has expired is **not** replaced behind your back. The bridge stops with one
line and leaves the files as they are, and you decide: remove `/data/tls` to start over, and trust the new CA.

### Giving MailFlow the CA

MailFlow must trust the CA to verify the certificate. There are two ways.

1. **Export to a shared volume.** Set `EASBRIDGE_CA_EXPORT_DIR` (for compose: `/ca`) and mount a volume there. EASBridge writes
   `ca.pem` (`0644`) at every start and again whenever the file is missing or differs. Mount the same volume read-only in MailFlow's
   backend (for example at `/easbridge-ca`) and set `NODE_EXTRA_CA_CERTS=/easbridge-ca/ca.pem` there. **Node reads that variable only
   when it starts**, so start the backend after EASBridge has written the file the first time. In compose, use `depends_on` with
   `condition: service_healthy`, and restart the backend if you recreate the CA volume.
2. **Print it.** `docker exec easbridge /easbridge ca` prints the CA certificate (PEM) on standard output; save it where MailFlow's
   backend can read it. The binary is `/easbridge` in the image (the image has no shell). In provided mode the command says the CA is
   not managed by EASBridge and exits with an error.

### Your own certificate (provided mode)

If `EASBRIDGE_TLS_CERT` and `EASBRIDGE_TLS_KEY` are both set, or neither is set and both `/tls/server.pem` and `/tls/server-key.pem`
exist, EASBridge uses those files as they are and never writes to or "repairs" them.

- The leaf certificate's names must include the host MailFlow connects to (for example DNS `easbridge`).
- A renewed certificate is picked up without a restart (checked at most once a minute, on a handshake).
- For a private CA, make MailFlow trust it (for example with `NODE_EXTRA_CA_CERTS`) instead of relaxing certificate checks.
- If only one of the two variables is set, if only one of the two default files exists, or if the files cannot be read or do not match
  each other, the bridge refuses to start with one line and changes nothing.
- `EASBRIDGE_TLS_NAMES` and `EASBRIDGE_CA_EXPORT_DIR` do nothing in this mode (a warning says so).

## Device identity

Exchange knows the bridge as a device. Two things identify it, and both stay the same for the life of an install: the **DeviceId** (32
hex characters, generated once and kept in `state.db`) and the **DeviceType** (`easbridge`). An organisation that approves devices
approves that pair, so neither may change. If `/data` is lost, the bridge becomes a new device and has to be approved again.

From protocol 14.1 on (the default is 16.1), Exchange also wants the device described **inside the initial Provision request**, the one
that downloads the policy. MS-ASPROV 2.2.2.53 makes `settings:DeviceInformation` mandatory there, and Exchange 2019 answers Provision
status 165 (DeviceInformationRequired) without it. A separate Settings command does not count, and the acknowledgement that follows
must not carry it. On protocol 14.0 the specification forbids the element in Provision; there it goes in a Settings command right
after provisioning. The bridge sends:

| Element | Value |
|---|---|
| `Model` | `EASBRIDGE_DEVICE_MODEL` (`easbridge`) |
| `FriendlyName` | `EASBRIDGE_DEVICE_FRIENDLY_NAME` (`easbridge (MailFlow bridge)`) |
| `OS` | `EASBRIDGE_DEVICE_OS` (`Linux`) |
| `OSLanguage` | `EASBRIDGE_DEVICE_OS_LANGUAGE` (`en-us`) |
| `UserAgent` | `EASBRIDGE_USER_AGENT` (`easbridge/<version>`), the same string as the HTTP header |
| `EnableOutboundSMS` | `0` |

It never sends an IMEI, a phone number or a mobile operator. None of the values may name another product: the bridge is not Outlook,
an iPhone or anything else, and the defaults say what it is. Each value is at most 128 bytes without control characters.

The service logs the values once at start-up. `easbridge probe` prints them and reads the same variables (its `--device-*` flags
override them).
