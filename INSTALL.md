# Installing EASBridge next to MailFlow

EASBridge lets MailFlow read and send the mail of a Microsoft Exchange mailbox that only allows Exchange
ActiveSync (EAS), and gives it the mailbox contacts and the company address book (read only). MailFlow
sees an ordinary IMAP, SMTP and CardDAV server called `easbridge`.

This page is the whole install for the usual case: you already run MailFlow with Docker Compose. One
`init` run, one `docker compose up`, two forms in MailFlow.

## What you need

- **Exchange**, on premises (written against 2019; 2016 speaks the same protocol), with ActiveSync
  switched on for the mailbox and **Basic authentication allowed for ActiveSync**. EASBridge signs in as the
  mailbox with its login and password, like a phone does.
- The ActiveSync address of your Exchange server, usually
  `https://mail.example.com/Microsoft-Server-ActiveSync`, reachable from the machine that runs Docker.
- Your **organisation may need to approve the device**. Exchange sees a new device called `easbridge` the
  first time; depending on its mobile device policy it is allowed at once, quarantined until an administrator
  approves it, or refused. If it is quarantined, ask your Exchange administrator to approve the device
  (see "Troubleshooting").
- **MailFlow 3.9.0 or newer** (its CardDAV client is what reads the contacts) running with **Docker Compose**,
  **Docker 24 or newer with the compose v2 plugin** (`docker compose version`).
- A mailbox login and its password. The password is typed into MailFlow and, if you run it, into the probe.
  It is never written to a file by EASBridge, and the files `init` writes hold none.

## The install

Run these in the folder that holds MailFlow's compose file (`docker-compose.yml`, or
`docker-compose.ghcr.yml` if you installed the pre-built images).

**Which compose file.** The commands below say `docker-compose.yml`. If you run MailFlow from `docker-compose.ghcr.yml` (the
pre-built images), put that name in every command instead. To type less, tell Compose once per shell, with your file's name:

```sh
export COMPOSE_FILE=docker-compose.yml:docker-compose.easbridge.yml     # or docker-compose.ghcr.yml:docker-compose.easbridge.yml
```

and then plain `docker compose up -d`, `docker compose logs easbridge` and so on work (`init` prints this line with the name it found).
Steps 4 to 6 happen in MailFlow's own web address, in your browser, not in a terminal. **Never publish EASBridge's ports** (`-p 993:993`
and the like): the mail ports are for the private network MailFlow's backend and EASBridge share, and have no connection limits of their own.

**1. Write the two files.**

```sh
IMAGE=ghcr.io/raed-alghamdi/easbridge:0.3.0     # the published image
docker run --rm -it --user "$(id -u):$(id -g)" -v "$PWD:/out" "$IMAGE" init
```

`--user "$(id -u):$(id -g)"` matters: the image runs as the unprivileged user 65532, which cannot write to your
folder; with your own user and group the files come out yours. `init` asks for the ActiveSync URL and for the
logins that may use the bridge: your address, several separated by commas, or `@example.com` to allow every
login of that domain (anyone who can reach the bridge and has a login in that domain can then try a password
through it; Exchange still checks it, and each login is rate limited, but prefer named addresses).
It checks them, checks MailFlow's compose file as text for the `backend` service and the `mailflow` network, and
writes:

- `docker-compose.easbridge.yml`, an override that adds the `easbridge` service and three lines to MailFlow's
  backend (the CA to trust, a read-only volume for it, and a wait for the bridge to be healthy);
- `easbridge.env`, the URL and the logins. Nothing secret.

The prompts need a terminal, which is what `-it` gives. Without one (a script, CI), give the answers as flags:

```sh
docker run --rm --user "$(id -u):$(id -g)" -v "$PWD:/out" "$IMAGE" init \
  --eas-url https://mail.example.com/Microsoft-Server-ActiveSync --users you@example.com
```

and `init` stops with a message naming `--eas-url` and `--users` if one is missing. Other flags: `--out` (default `/out`),
`--image`, `--force` (replace the two files; without it `init` never overwrites), `--backend-service` (default
`backend`) and `--network` (default `mailflow`); `init` warns, naming the flag, when MailFlow's file has no such
service or network. Later you can read the files, change them, or run `init --force` again.

**2. Optional: a first contact with Exchange**, before anything runs. It asks for the password without echoing it
and reports at once whether Exchange accepts the device, the login and the URL. Run it before step 3: the probe and
the service use the same data volume.

```sh
docker compose -f docker-compose.yml -f docker-compose.easbridge.yml run --rm easbridge probe --user you@example.com
```

**Each probe run is a real logon attempt.** A wrong password counts toward the Active Directory account lockout like any other, so
do not run it in a loop. For a script, feed the password on standard input (`-T` turns off the terminal that `run` normally allocates):

```sh
printf '%s\n' "$PASSWORD" | docker compose -f docker-compose.yml -f docker-compose.easbridge.yml run --rm -T easbridge probe --user you@example.com --password-stdin
```

**3. Start.**

```sh
docker compose -f docker-compose.yml -f docker-compose.easbridge.yml up -d
```

EASBridge makes its own certificate authority and certificate on its first start and writes the authority's
certificate to a volume that MailFlow's backend mounts read-only; the backend starts after the bridge is healthy,
which is what lets it trust the certificate (Node reads that setting only when it starts).

**4. In MailFlow, once, as an administrator:** Settings > Security & Integrations > Security > Mail Server
Connection Policy > turn on **Allow private / local hosts**. (MailFlow refuses a mail server at a private
address until you do, and `easbridge` is one. The same switch also covers the CardDAV address on port 8443 in step 6.)

**5. Add the mail account:** Settings > Account & Mail > Accounts > **Add account** > **Custom**:

| Field | Value |
|---|---|
| Display name | any name you like (MailFlow requires it) |
| Email address | your mailbox address |
| Username (IMAP login) | the login you gave `init` (usually the same address) |
| Password / App password | the mailbox password |
| IMAP Settings: IMAP Host, Port | `easbridge`, `993` |
| SMTP Settings: SMTP Host, Port | `easbridge`, `465` |

There is no TLS selector in MailFlow's form: ports 993 and 465 select TLS, and MailFlow verifies the certificate with the authority it
was given. Leave "Skip TLS certificate verification" off (it is only shown when the administrator allows insecure TLS). The first sync of a big Inbox takes a while; the folder shows its messages
all at once when the download has caught up.

**6. Contacts (optional):** Settings > Security & Integrations > Integrations > **Apps** > **CardDAV Contacts**:
Server URL `https://easbridge:8443/`, Username the same login, App password the same password, **Connect**. You get
one address book for each Contacts folder of the mailbox and a "Directory" book with the people from your company
address book whose addresses appear in your mail.

(The labels above are those of MailFlow 3.9.0's interface.)

## Without Docker Compose

Run the container yourself on the network MailFlow is on. The minimal form (and the strict one) are in the
[README](README.md#running-it); give the container the two values and a data volume, and start it **before**
MailFlow's backend. MailFlow's backend must trust the authority that EASBridge made:

```sh
docker exec easbridge /easbridge ca > easbridge-ca.pem
```

prints the certificate (the binary is `/easbridge` in the image; the image has no shell). Mount that file into
MailFlow's backend and set `NODE_EXTRA_CA_CERTS` to its path there, then start (or restart) the backend: Node
reads the variable only at start. Or set `EASBRIDGE_CA_EXPORT_DIR=/ca` on the container, mount a volume there, and
mount the same volume read-only in the backend, as the compose file does.

## First-run checks

1. `easbridge probe` (step 2 above) is the first check; add `--trace` to see the structure of every request
   and answer, without content, if it stops.
2. `docker exec easbridge /easbridge healthcheck -v` prints the health report and exits 0 when the bridge is healthy (that is also
   what Docker runs, without `-v`). It shows `tls_mode` (`automatic`), `tls_server_expires_in_days`, and the folders' first sync under
   `accounts`. It holds states and counts, never mail or names.
3. The Bcc check: send a message from MailFlow with a Bcc recipient to yourself and check that the Bcc recipient
   received it and the To recipient's copy has no Bcc header (the README's "First-run checks" says how).
   MailFlow 3.9.0 does not show Bcc recipients of messages it sent itself; that is MailFlow's display, not the bridge.

## Upgrading

Take a copy of the data volume first (the database is migrated in place and an older image cannot open a newer
database):

```sh
docker volume ls | grep easbridge-data        # the name starts with your compose project's name
docker run --rm -v <that-volume>:/from:ro -v "$PWD:/to" alpine tar czf /to/easbridge-data.tgz -C /from .
```

Then run `init --force` with the new image (it rewrites the two files with the new tag; your settings are asked
again) or edit the tag in `docker-compose.easbridge.yml`, and:

```sh
docker compose -f docker-compose.yml -f docker-compose.easbridge.yml pull easbridge
docker compose -f docker-compose.yml -f docker-compose.easbridge.yml up -d
```

The certificate authority lives in the data volume and does not change, so MailFlow keeps trusting it. Read
`CHANGELOG.md` for the version you move to.

## Uninstalling

```sh
docker compose -f docker-compose.yml -f docker-compose.easbridge.yml stop easbridge
docker compose -f docker-compose.yml -f docker-compose.easbridge.yml rm -f easbridge
docker compose -f docker-compose.yml up -d            # MailFlow alone: the backend loses the three lines
docker volume ls | grep easbridge                      # <project>_easbridge-data and <project>_easbridge-ca
docker volume rm <project>_easbridge-data <project>_easbridge-ca
rm docker-compose.easbridge.yml easbridge.env
```

Do not use `docker compose down -v` for this: it would remove MailFlow's volumes (its database) as well. The
data volume holds the cache of your mail, the contacts and the certificate authority's key; removing it deletes
them. In MailFlow, remove the account and the CardDAV connection.

## Troubleshooting

**MailFlow says the certificate is not trusted or the connection fails with a certificate error.**
MailFlow's backend started before EASBridge had written its certificate authority, or was started before the
override was in place. Node reads `NODE_EXTRA_CA_CERTS` only when it starts: restart the backend
(`docker compose -f ... -f ... restart backend`). Check that the file is there:
`docker compose -f ... -f ... exec backend ls -l /easbridge-ca/ca.pem`. If you recreated the `easbridge-data` volume, the
authority is a new one: restart the backend after the bridge is up. Never switch certificate checking off to get
past this.

**The log says the certificate is signed by an unknown authority, or the probe fails with "unknown authority".** Your Exchange
server (or the gateway in front of it) uses a certificate from your organisation's own CA, which EASBridge does not know. Give it the CA
certificate (PEM) in a second override file, next to the generated one, so that `init --force` never overwrites it:

```yaml
# docker-compose.local.yml
services:
  easbridge:
    environment:
      EASBRIDGE_EAS_CA_FILE: /exchange-ca/ca.pem
    volumes:
      - ./exchange-ca.pem:/exchange-ca/ca.pem:ro
```

Add it to the commands (`-f docker-compose.local.yml`) or to `COMPOSE_FILE`, and recreate the container. Do not look for a way to switch
certificate checking off: EASBridge sends the mailbox password to that server.

**EASBridge cannot reach Exchange although the URL is right (the log shows connection errors).** MailFlow's compose file may define its
network with `internal: true`, which has no route out. Give EASBridge a second network that has one, in the same
`docker-compose.local.yml`:

```yaml
services:
  easbridge:
    networks:
      - mailflow          # or the name MailFlow's network has
      - egress
networks:
  egress:
    driver: bridge
```

MailFlow's backend stays on its internal network and only EASBridge can reach out.

**EASBridge does not start; the log says a name is outside what the CA may certify.** The automatic CA is name-constrained to the
names it was made for (see the README, "TLS"). A new DNS name in `EASBRIDGE_TLS_NAMES`, or a public address in
`EASBRIDGE_LISTEN_IP`, needs a new CA: stop EASBridge, remove the `tls` directory inside its data volume, start it, and **restart
MailFlow's backend** so that Node reads the new CA. A new container host name is not a problem: the CA was not made for it and it is left out.

**EASBridge does not start; the log says the network namespace does not look like a container's.**
You ran it with host networking or outside a container, and it refused to listen on every address of a host.
Set `EASBRIDGE_LISTEN_IP` to the one address it should use, or give the three listener variables, or (if listening
on every address there is what you want) `EASBRIDGE_ALLOW_ANY_BIND=true`. Under Compose the default is right and
this message does not appear.

**The log or the probe says the device is quarantined, blocked or needs approval** (an HTTP 403 or 449, or a
provisioning status). Exchange has not approved the device `easbridge` yet. Ask your Exchange administrator to
approve it (in Exchange: the mobile devices of the mailbox, "allow" the device with the id shown in the probe
output), or to change the mobile device mailbox policy. **Then restart the bridge** (`docker compose restart easbridge`): while the
device was refused, the bridge remembers locally that this password failed and does not ask Exchange again for a while (up to 15
minutes the first time, longer if it failed repeatedly); a restart clears that. EASBridge presents itself honestly as a device of type
`easbridge`; it never pretends to be another product.

**Exchange answers 401 (the login or password is rejected).** Check the login (some servers want
`DOMAIN\user` or the user principal name, not the mail address) and the password. A wrong password is
remembered and refused locally for a while so that your account is not locked out; fix the password, then restart the
bridge. Several wrong attempts in a row are rate limited on purpose. A 403, a redirect to a logon page or an HTML page where
Exchange's answer should be (how a gateway in front of Exchange refuses a credential) count the same way: two in a row during a
session stop all traffic of that mailbox until MailFlow logs in again with a working password.

**The contacts book is empty, or the "Directory" book has few people.** The mailbox Contacts books fill after
their first download, all at once. The Directory book grows one person at a time as addresses from your mail are
looked up in the company address book (at most 60 a hour by default). That is by design.

**The full directory sweep** (`EASBRIDGE_DIRECTORY_FULL=on`, off by default and experimental) copies the whole
company address book by searching it piece by piece. To the Exchange servers it looks like a program
enumerating the directory, and it may be noticed, throttled or blocked, which would stop your mail too. It stops
itself where the directory only answers queries of three characters or more. Leave it off unless you have
asked whoever runs Exchange.

**Something else.** `docker compose -f ... -f ... logs easbridge` has the story in plain sentences (statuses and
counts, never mail or passwords). `easbridge probe --trace` shows the structure of the traffic.
