# Security policy

## Reporting a vulnerability

Please report it privately through GitHub: open the repository's **Security** tab and choose **Report a vulnerability**
(GitHub's private vulnerability reporting). Do not open a public issue or pull request for it.

Say what you found, how to reproduce it, and which version (`easbridge version`) and configuration it affects. Remove passwords,
tokens and any mail content from logs before you attach them. You will get an answer within a few days; a fix and an advisory follow
as soon as the fix is ready, and you are credited if you want to be.

## Supported versions

Only the **latest minor release** (0.3.x at the time of writing) gets security fixes. Upgrade before you report something that an
older version has.

## What protects the mailbox password, and what stops traffic

A mailbox password is sent to Exchange on every verification, and every wrong one counts toward the Active Directory lockout. So:

- Only logins on the allow-list reach Exchange. A domain entry (`@example.com`) lets any name of that domain through the allow-list;
  then a source address may present at most 10 names that have not been verified in 10 minutes, and a name that was never verified leaves no
  trace in the database.
- A verified password is cached in memory and later logins never reach Exchange. A wrong one is refused locally for 15 minutes, 1 hour,
  4 hours, then 24 hours, and a user may cause at most 2 unsuccessful verifications in 30 minutes.
- During a session a 401 stops all Exchange traffic of the mailbox at once. A 403, a redirect that is not followed, or an HTML page in
  answer to an EAS request (how a gateway refuses a credential) counts against the same budget, and the second in a row stops the mailbox
  the same way. Throttling (429, 5xx) and time-outs are retried with a back-off and cost no budget.
- The IMAP, SMTP and CardDAV servers have no per-connection limits. Keep their ports on a private network and never publish them.
- The automatic CA is name-constrained (the names it was made for, and private addresses), because MailFlow's backend trusts it for every
  outbound connection it makes. Its key lives in the data volume: whoever reads that volume can mint certificates for those names.

## What is in scope

- The `easbridge` binary and the container image built from this repository: the IMAP, SMTP submission and CardDAV servers, the
  handling of mail and contact data, the certificate and key handling, the configuration and `easbridge init`, the generated compose
  files.
- Anything that lets someone without a valid mailbox login read mail or contacts, send mail, or make EASBridge send requests to
  Exchange for a login that is not on its allow-list; credentials reaching a log, a file or an error message; a way around the lockout
  protection that would lock a mailbox out of Active Directory; unsafe defaults.
- Denial of service by a small request that a remote client can send before authenticating.

## What is not

- Bugs in Exchange, MailFlow or Docker themselves.
- Problems that need an attacker who already has root on the host, the data volume, or the container's environment (the data volume
  holds the mail cache and the certificate authority's key by design; protect it as you would the mail).
- A domain entry in `EASBRIDGE_ALLOWED_USERS` (`@example.com`) letting anyone with a login in that domain try a password through the
  bridge: that is what the entry means, and the README says so.
- The experimental full directory sweep being noticed or blocked by an Exchange server: the documentation warns about it.
- Findings in the test-only code under `compat/` and `e2e/`, unless they affect what is shipped.

Dependencies are checked with `govulncheck` in CI and updated through Dependabot.
