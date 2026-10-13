# Security policy

## Reporting a vulnerability

Report it privately through GitHub: open the repository's **Security** tab and choose **Report a vulnerability** (GitHub's private
vulnerability reporting). Do not open a public issue or pull request for it.

Say what you found, how to reproduce it, and which version (`easbridge version`) and configuration it affects. Remove passwords,
tokens and any mail content from logs before you attach them. You will get an answer within a few days; a fix and an advisory follow
as soon as the fix is ready, and you are credited if you want to be.

## Supported versions

Only the **latest minor release** (0.3.x at the time of writing) gets security fixes. Upgrade before you report something that an
older version has.

## How EASBridge protects the mailbox password

The allow-list, the lockout protection, what stops traffic during a session, the name-constrained CA and the network assumptions are
described in [docs/SECURITY-MODEL.md](docs/SECURITY-MODEL.md). Read it to judge whether a finding is in scope.

## What counts as a vulnerability (in scope)

- The `easbridge` binary and the container image built from this repository: the IMAP, SMTP submission and CardDAV servers, the
  handling of mail and contact data, the certificate and key handling, the configuration and `easbridge init`, the generated compose
  files.
- Anything that lets someone without a valid mailbox login read mail or contacts, send mail, or make EASBridge send requests to
  Exchange for a login that is not on its allow-list; credentials reaching a log, a file or an error message; a way around the lockout
  protection that would lock a mailbox out of Active Directory; unsafe defaults.
- Denial of service by a small request that a remote client can send before authenticating.

## What does not count (out of scope)

- Bugs in Exchange, MailFlow or Docker themselves.
- Problems that need an attacker who already has root on the host, the data volume, or the container's environment (the data volume
  holds the mail cache and the CA's key by design; protect it as you would the mail).
- A domain entry in `EASBRIDGE_ALLOWED_USERS` (`@example.com`) letting anyone with a login in that domain try a password through EASBridge: that
  is what the entry means, and the [security model](docs/SECURITY-MODEL.md#who-can-sign-in-through-easbridge) says so.
- The experimental full directory sweep being noticed or blocked by an Exchange server: the documentation warns about it.
- Findings in the test-only code under `compat/` and `e2e/`, unless they affect what is shipped.

Dependencies are checked with `govulncheck` in CI and updated through Dependabot.
