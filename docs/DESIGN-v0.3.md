# EASBridge v0.3: install defaults for a public release

EASBridge (EAS for Exchange ActiveSync, then Bridge; the binary, the image and the `EASBRIDGE_` variables keep the lower-case
identifier `easbridge`). Goal: a normal install needs two values, the ActiveSync URL (`EASBRIDGE_EAS_URL`) and the login (`EASBRIDGE_ALLOWED_USERS`), and
the certificate is made automatically (section 7). Slices 1 and 3 of the plan are (listen defaults, contacts on by default, domain
entries, the image's `/data`, a safer sweep), slice 2 is the automatic certificate. Existing installs keep working: their
variables keep their meaning (section 1.3).

## 1. Where the bridge listens

### 1.1 The rule

| Variable | Meaning |
|---|---|
| `EASBRIDGE_IMAP_ADDR`, `EASBRIDGE_SMTP_ADDR`, `EASBRIDGE_CARDDAV_ADDR` | Unchanged: `ip:port`, the IP a literal, validated exactly as in 0.2.2. When set, it decides that listener. |
| `EASBRIDGE_LISTEN_IP` (new, optional) | One IP literal (v4 or v6, no port, no zone, not `0.0.0.0` or `::`). Every listener without a variable of its own binds this IP on its default port. |
| (neither) | Every listener without a variable of its own binds all addresses of the container (`:993`, `:465`, `:8443`, a wildcard bind). MailFlow reaches the bridge by the container's name on the network they share. |

Default ports: IMAP 993, SMTP 465, CardDAV 8443. The health endpoint stays on `127.0.0.1:8081` (`EASBRIDGE_HEALTH_ADDR`, unchanged,
loopback only: the container's own healthcheck uses it).

CardDAV has an address only while the contacts are on (section 2). A listener whose address comes from the defaults is "derived".

### 1.2 The two checks (they need the interfaces, so they run at start, not in `Load`)

The interface lister is injectable (`config.InterfaceLister`; `serveHooks.interfaces` in the tests).

1. **`EASBRIDGE_LISTEN_IP` is an address of the container.** If at least one listener uses it, it must equal an address of one of the
   interfaces (the loopback counts). Otherwise the bridge does not start and says, in one line, what was given and which addresses exist:
   `EASBRIDGE_LISTEN_IP=10.9.9.9 is not an address of this container; the addresses here are: lo 127.0.0.1, ::1; eth0 10.0.0.5`.
2. **The wildcard needs a container.** If a listener is derived and `EASBRIDGE_LISTEN_IP` is unset, the namespace must look like a
   container's, or the bridge does not start (a wildcard bind on a host is open to every network of the host). The guard is skipped
   with `EASBRIDGE_ALLOW_ANY_BIND=true` (or `1`).

   **The heuristic.** Take the interfaces of the namespace, drop the loopback (flagged loopback, or named `lo`). The namespace looks
   like a container's when **every remaining interface is named `eth` followed by digits** (`^eth[0-9]+$`): `lo` and `eth0`, or
   `lo`, `eth0` and `eth1` for a container on two networks, or only `lo` for a container without a network. It looks like a host's
   as soon as one interface is named anything else: `docker0`, `br-*`, `veth*`, `wlan0`, `wlp*`, `enp*`, `eno*`, `tailscale0`, `wg0`,
   `eth0.100`. The refusal names the interfaces and tells the user to set `EASBRIDGE_LISTEN_IP`, or the three listener variables, or
   `EASBRIDGE_ALLOW_ANY_BIND=true`.

   What it does not catch: host networking on a machine whose only interface is called `eth0` (old VPS images), which looks like a
   container; there the wildcard is exactly what a published port would be, so the exposure is the host's own address. It also
   refuses some legitimate containers (Podman with `pasta` copies the host's interface names, a VPN sidecar adds `tun0` or `wg0`):
   those set `EASBRIDGE_LISTEN_IP` or the override. A false refusal costs one line in the log, a false acceptance exposes a mail
   gateway, so the heuristic errs on refusing.

The checks do not apply to a configuration in which all three listeners have their own variable (as 0.2.2 required): it starts as
before, on a host as well.

### 1.3 Precedence and compatibility

The listener variables win per listener, with their existing validation (`ip:port`, no host names, no unspecified address without
`EASBRIDGE_ALLOW_ANY_BIND`, no collision between the listeners). A variable and `EASBRIDGE_LISTEN_IP` can be mixed (IMAP given, SMTP
and CardDAV from the IP). The configuration of 0.2.2 that names all three listeners (`IMAP 10.0.0.5:993`, `SMTP 10.0.0.5:465`, `CARDDAV 10.0.0.5:8443`,
no `LISTEN_IP`, no `CONTACTS`) loads to exactly those addresses, with the contacts on, no interface looked at; a test uses that exact
environment.

`EASBRIDGE_ALLOW_ANY_BIND` accepts `true` and `1` (and `false`, `0`); it still allows an unspecified address in a variable, and now also
switches the wildcard guard off.

### 1.4 The start log

One line names the mode: `listening on all container addresses (set EASBRIDGE_LISTEN_IP to restrict)`, `listening on 10.0.0.5 only
(EASBRIDGE_LISTEN_IP)`, `listening on the addresses given in EASBRIDGE_IMAP_ADDR, EASBRIDGE_SMTP_ADDR and EASBRIDGE_CARDDAV_ADDR`, or,
with the override, `listening on all addresses of this namespace (...; EASBRIDGE_ALLOW_ANY_BIND is set, so the namespace was not checked)`,
followed by the bound addresses. The 0.2.2 warning for an unspecified address in a variable stays.

## 2. Contacts on by default

`EASBRIDGE_CONTACTS=on|off`, default `on`. On, with no `EASBRIDGE_CARDDAV_ADDR`: CardDAV follows section 1 (port 8443). `off` with
`EASBRIDGE_CARDDAV_ADDR` set is a configuration error (one line naming both). The configuration of 0.2.2 (address set, variable unset)
stays on. The directory keeps its defaults: the domains of the allowed users (domain entries included), lookups of the addresses seen in
mail. `EASBRIDGE_DIRECTORY_FULL=on` needs the contacts on.

Consequence: an install that did not set `EASBRIDGE_CARDDAV_ADDR` before now has the contacts on after the upgrade, with a CardDAV
listener on 8443 (same bind rules). Anything that must stay as it was sets `EASBRIDGE_CONTACTS=off`. The data of the feature is created
on the first run.

## 3. Domain entries in the allow-list

`EASBRIDGE_ALLOWED_USERS` entries beginning with `@` are domain entries: `@example.com` lets in every login `x@example.com`
(case-insensitive, **exact** domain match: not `x@sub.example.com`, not `x@notexample.com`, not `x@example.com.evil.test`; the local part
is non-empty and holds no second `@`). An entry must be a valid domain; plain addresses keep working and can be mixed; the variable stays
required. Domain entries are stored lower case; a domain entry is never treated as a login of its own (no fake mailbox, not the probe's
default login). The directory domains default to the domains of every entry, including `@domain` ones.

The password is still checked by Exchange and the lockout budget stays per login (only a few unsuccessful verifications per login
reach Exchange in a window, and a rejected password is refused locally for a while), so the exposure is: **anyone who can reach the bridge and has a login in that domain may try
passwords through it**, rate limited per login but not per source. The README says so plainly next to the example.

## 4. The image's `/data`

`/data` exists in the image, owned by 65532:65532, mode 0700, and `VOLUME ["/data"]` comes after the `COPY --chown`, so that Docker
initialises a new named volume from the image directory (content, owner and mode) and the bridge can write without a `chown`. A bind
mount of a host directory is still the operator's to make writable by 65532. A test reads the Dockerfile for the four lines and their order.

## 5. The full sweep stops itself when it cannot work

If calibration settles on a shortest query length of 3 or more, the sweep ends for that run: 39 characters in the alphabet make 59,319
strings of 3 characters (and 2.3 million of 4) before a single division, which at the rate the directory is limited to is weeks (3 characters at
one search a minute: 41 days) to years (4 characters) of searching and looks like what it is. The state is a sweep that finished without completing (nothing is removed), idle until
its next time (`EASBRIDGE_DIRECTORY_FULL_INTERVAL`), one log line (`directory sweep stopped for this run: the directory answers no query
shorter than N characters ...`), and `/healthz` shows `sweep_infeasible: true` and `sweep_infeasible_length: N` in `directory_sweep`. No
schema change: the condition is derived (idle with a calibrated length of 3 or more, which a sweep never leaves behind otherwise).
The option stays off by default and the README marks it experimental.

## 6. Deviations and decisions

- The brief says "refuse to start if the network namespace does not look like a container's (for example ...)"; the heuristic above
  is the exact rule. Interfaces that are down count (a host's down `wlan0` is still a host).
- `EASBRIDGE_ALLOW_ANY_BIND` previously accepted only `1`; it now also accepts `true`/`false`, as the brief names it.
- `Load` stays free of the network: the checks of section 1.2 are `Config.CheckListenNetwork`, called at the top of `serve`, before the
  state database is opened. `easbridge probe` and `healthcheck` do not run them.
- A domain entry gives the fake upstream (devfake builds) no mailbox; tests that need one name the login.
- Existing tests and the compatibility harness set `EASBRIDGE_CONTACTS=off` unless they ask for contacts, because the default CardDAV
  port is fixed.
- The sweep's "infeasible" state needs no column (section 5).

## 7. The automatic certificate (slice 2)

Goal: an install with no TLS files at all works, and MailFlow verifies the certificate. Package `internal/tlsauto`; wiring in
`cmd/easbridge/tls.go`.

### 7.1 Modes

- **Provided** (unchanged from 0.2.2): `EASBRIDGE_TLS_CERT` and `EASBRIDGE_TLS_KEY` both set, or neither set and both default files
  (`/tls/server.pem`, `/tls/server-key.pem`) present. The files are loaded with the same hot reload (at most once a minute, on a
  handshake) and **never written, repaired or replaced**. Exactly one of a pair (two variables, or two default files) is an error, as are
  files that cannot be read or whose certificate and key do not match: one line, nothing is changed, nothing is made next to them.
  `EASBRIDGE_TLS_NAMES` and `EASBRIDGE_CA_EXPORT_DIR` do nothing here (a warning says so).
- **Automatic** (new default, when no files are provided): the files live in `<data dir>/tls/`: `ca.pem`, `ca-key.pem`, `server.pem`,
  `server-key.pem`. The directory is `0700`, keys `0600`, certificates `0644`, everything owned by the running user (the files are
  written through a temporary file in the same directory and renamed, with the mode set explicitly, whatever the umask).
  ECDSA P-256. The CA: valid 20 years, subject `EASBridge local CA <8 hex digits>` (random per install), CA:true with path length 0.
  The server certificate: valid 2 years, CN `easbridge`, server-auth only.

### 7.2 Which files are reused

At start: no file at all makes a new CA and a server certificate. A valid CA (certificate and key match, not expired) is **never
replaced**; the server certificate is reissued from it when it is missing, unreadable, does not match its key, does not verify against
the CA now, ends within 60 days, or no longer fits the names. A CA that is incomplete, mismatched, garbled or expired, or a server
certificate without its CA, stops the start with one line and leaves the files alone: replacing the CA would silently change what
MailFlow trusts.

### 7.3 Names

DNS: `easbridge`, the container's host name, every `EASBRIDGE_TLS_NAMES` entry that is not an IP literal. IP: `EASBRIDGE_LISTEN_IP`,
the IP literals of `EASBRIDGE_TLS_NAMES`, and every non-loopback, non-link-local address of the interfaces at start (the lister of
section 1.2). The server certificate fits the names when its DNS names are exactly the wanted ones and every wanted address is among
its addresses: a name added, a name removed, or an address that is new (a container that came back with another address) issues a new
server certificate from the same CA; an address that went away does not.

### 7.4 Renewal

Every 12 hours while running (and at start), `Renew` looks at the server certificate; within 60 days of its end, it issues a new one
from the same CA, writes the key and then the certificate, and tells the reloader, so the next handshake serves it. A renewal that
fails is logged and retried at the next look; the old certificate keeps being served. The CA is never touched.

### 7.5 Handing the CA to MailFlow

1. `EASBRIDGE_CA_EXPORT_DIR`: `ca.pem` (`0644`) is written there at start (an error stops the start: the setting asked for a copy that
   cannot be made) and again at every renewal check if it is missing, differs or has another mode. MailFlow's backend mounts the
   volume read-only and sets `NODE_EXTRA_CA_CERTS=/easbridge-ca/ca.pem`; Node reads that variable only at start, so the backend must
   start after the bridge has written the file (compose: `depends_on` with `condition: service_healthy`; the next slice writes the
   compose file).
2. `easbridge ca` prints the CA certificate PEM on stdout. In the image: `docker exec easbridge /easbridge ca` (the binary is
   `/easbridge`; the exec runs as the container's user and the CA file is world readable). Provided mode: an error saying the CA is not
   managed by EASBridge. Before the first start: an error saying to start the bridge once.

### 7.6 Observability

`/healthz`: `tls_mode` (`automatic` or `provided`) and `tls_server_expires_in_days` (of the certificate being served, both modes). The
start log names the mode; in automatic mode with the CA subject, its SHA-256 fingerprint, the server certificate's end date, the DNS
names and the number of IP names. Never key material (tests look for it).

### 7.7 Deviations and decisions

- The brief says "a new `EASBRIDGE_TLS_NAMES`" only for DNS names; IP literals are accepted there too (they become IP names), because a
  client that connects by address needs it.
- A DNS name or address that disappears from the wanted set does not by itself reissue when only an address went away (section 7.3);
  this avoids a new certificate at every start of a container whose extra addresses come and go.
- A failing `EASBRIDGE_CA_EXPORT_DIR` write at start stops the start; later failures are logged.
- The server certificate file holds the leaf only (the CA is trusted separately).

## 8. `easbridge init`, the compose override and INSTALL.md (slice 4)

Goal: someone who already runs MailFlow with Docker Compose installs EASBridge with one `init` run, one `docker compose up` and two forms
in MailFlow. Code: `internal/install` (the generated text, the checks of the compose file as text), `cmd/easbridge/init.go`.

### 8.1 The image reference

One constant: `internal/version.Image` (default `ghcr.io/raed-alghamdi/easbridge`), the tag is the build's version (`version.ImageRef()`).
A build overrides it with `-ldflags -X easbridge/internal/version.Image=...` (`make IMAGE=...`, `docker build --build-arg IMAGE=...`), a
run with `init --image`. Slice 5 confirms the final location; nothing else names it in the code (the docs do, in INSTALL.md and the README).

### 8.2 `init`

Run as `docker run --rm -it --user "$(id -u):$(id -g)" -v "$PWD:/out" <image> init`. The `--user` matters: the image's user (65532) cannot write a
host folder. It asks, at a terminal, for the ActiveSync URL and the logins; without a terminal missing values are an error that names the flags
(`--eas-url`, `--users`). Other flags: `--out` (default `/out`), `--image`, `--force`, `--backend-service` (default `backend`), `--network`
(default `mailflow`). The URL is validated as the configuration does (https, a host; a different path only warns), the logins by
`config.ParseAllowedUsers` (lower case, de-duplicated, `@domain` entries valid), and the values must be safe in an env file and in YAML (no
spaces, quotes, `#`, `$`, backslashes). MailFlow's compose file (`docker-compose.yml`, else `docker-compose.ghcr.yml`) is read **as text**: a
service called like `--backend-service` under `services:` and a network called like `--network` under `networks:`; if either is missing
init warns and names the flag to use; a file it cannot read, or no file at all, is a warning too (the commands then name `docker-compose.yml`).
It writes `docker-compose.easbridge.yml` and `easbridge.env` (mode 0644), refuses to overwrite either without `--force` (checking both first, so
a refusal writes nothing), and prints the next steps with the values filled in. Nothing secret is written.

### 8.3 The override

The `easbridge` service: the image; `container_name: easbridge` (so that `docker exec easbridge /easbridge ca` works); `restart:
unless-stopped`; `read_only`; `cap_drop: [ALL]`; `no-new-privileges`; 1 GiB; 256 processes; core dumps off; json-file logs rotated at 50 MB
x 3; `env_file: easbridge.env`; `EASBRIDGE_CA_EXPORT_DIR=/ca`; the volumes `easbridge-data:/data` and `easbridge-ca:/ca`; the MailFlow network;
**no published ports**; a health check that runs the image's own command (`/easbridge healthcheck`) every 5 s instead of the image's 30 s, so
that the backend, which waits for it, starts seconds and not half a minute after the bridge (a deviation from "the image HEALTHCHECK":
the same command, a shorter interval, stated in the file). The backend gets `NODE_EXTRA_CA_CERTS=/easbridge-ca/ca.pem`, the volume
`easbridge-ca:/easbridge-ca:ro` and `depends_on: easbridge: condition: service_healthy`. Two named volumes. The image has `/ca` owned by 65532 so
that the volume the bridge writes to is writable without a chown (the same copy-up as for `/data`).

`cmd/easbridge/compose_merge_test.go` merges the output with MailFlow v3.9.0's two compose files, fetched at test time into
`compat/.mailflow-cache` and checked against SHA-256 pins, with `docker compose config --format json` (needs the docker CLI and the compose
plugin, not a daemon; skipped without them or offline): the backend keeps every environment variable and every `depends_on` entry it had,
gains the three additions, the CA volume is read only, the other services are unchanged, the bridge has the hardening and publishes nothing.

### 8.4 INSTALL.md and `make e2e`

INSTALL.md (repo root) is the user's page. The screen labels in it and in `init`'s steps were read from MailFlow 3.9.0's `en.json` and
`AdminPanel.jsx` at the tag (see TESTING.md). `make e2e` (`e2e/run.sh`) needs Docker (skips with one line otherwise): it builds a devfake image
(`--build-arg GOTAGS=devfake`; the real image has none), runs `init` from it into a temporary folder next to a stand-in compose file (a Node
22 `backend`, image pinned by digest, on a `mailflow` network) with `--image` set to the test image, runs `docker compose up
--abort-on-container-exit --exit-code-from backend`, and checks that the backend log has all five lines (the CA file was there at start,
IMAP, SMTP and CardDAV over verified TLS), that the backend started after the bridge's first successful health check, and removes everything,
volumes included. The backend program (`e2e/backend.js`) was also run against a local devfake bridge in automatic mode.

### 8.5 Found on the way

The Dockerfile of slice 2 still set `EASBRIDGE_TLS_CERT` and `EASBRIDGE_TLS_KEY`, so the automatic certificate could not be used in the image.
Removed; a test keeps the image free of `EASBRIDGE_TLS_` variables.

## 9. Hardening after review

### 9.1 The CA is name-constrained (changes 7.3)

MailFlow's backend trusts the CA through `NODE_EXTRA_CA_CERTS`, and Node applies that to every outbound TLS connection the backend makes. A
CA that could certify any name would let whoever reads the data volume (it holds the CA key) impersonate any site to the backend. The CA is
therefore made with **critical** name constraints (`internal/tlsauto/constraints.go`): permitted DNS domains are the names it was created for
(`easbridge`, the host name at that time, the entries of `EASBRIDGE_TLS_NAMES`), permitted IP ranges are loopback, RFC 1918, `fc00::/7` and
link-local, and nothing else. Both OpenSSL (Node) and Go enforce them.

Names are *required* (`easbridge`, `EASBRIDGE_TLS_NAMES`, `EASBRIDGE_LISTEN_IP`) or *best effort* (the host name, the container's interface
addresses). A best-effort name the CA may not certify is left out of the certificate without a word, so that a recreated container (new host
name) still starts. A required name outside the constraints stops the start with one line: what it is, that the CA was not made for it, and the
way out (stop EASBridge, remove the `tls` directory of the data volume, restart MailFlow's backend so that Node reads the new CA). A first start
with a public address among the required names is refused before any file is made. A CA made by an earlier build without constraints keeps
working and the log says it has none. Test: Go verifies that a leaf signed with the CA key for another name or a public address fails; Node
does the same in `compat/` (`compat/mintleaf` plays the holder of the key).

### 9.2 Answers that look like a refused credential

A 403, an HTTP redirect that is not followed, and a 2xx HTML page in answer to an EAS POST (the adapter's `htmlGuard` transport turns it into
an error) are `upstream.ErrAuthLike`: they are also an `ErrUnavailable`, so everything that backs off still does. During a session the first is
counted against the user's budget (`auth.Cache.CountAttempt`, no cooldown), the second in a row is recorded like an unverified login
(`RecordFailure`, escalating cooldown), stops the engine as a 401 does (`authFailed`) and drops the cached verification; any success resets the
count. 404 and genuine transport errors are not auth-like. At a login these still count and escalate, as an unverified failure always did.

### 9.3 Nothing grows before a login is verified

With a domain entry in the allow-list any name of the domain passes the allow-list. Before Exchange has accepted a login: no account row is
made (`PolicyKeyOf` reads without creating; `Account` runs after success), the per-user lock exists only while held, the failure cache and the
per-user records have bounds (4096 each; the entries that end soonest go first) and expire with their cooldown and window, and one source
address (the remote IP, passed with `engine.WithSource`) may present only 10 distinct unverified names in 10 minutes (EASBridge refuses
further ones locally, with no request to Exchange). The limiter runs only when the allow-list holds a domain entry.

### 9.4 Throttling and time-outs at a login

A verification that got no answer (HTTP 429, any 5xx, a time-out or a connection dropped on the way: `ThrottleError`, `upstream.ErrTransient`)
cannot have been a refusal of the password. The same credential waits a fixed 60 seconds (`RecordTransient`), with no budget used and no
escalation. 401, 403 and logon pages still count and escalate.

### 9.5 Smaller decisions

- `easbridge healthcheck -v` prints the health report; it is how the report is read from outside the container (the image has no curl).
- The password a client presents is registered with the log scrubber only after the allow-list check, so a name that is refused is not
  processed at all.
- Allow-list entries are validated (no spaces, quotes or brackets; at most one `@`, a valid domain; `DOMAIN\name` and a bare name stay valid).
- The Dockerfile builds on `$BUILDPLATFORM` and cross-compiles (`GOARCH=$TARGETARCH`), declares no `EXPOSE`, and the docs say never to publish
  the ports; the servers have no per-connection limits, so the ports must stay on a private network.
