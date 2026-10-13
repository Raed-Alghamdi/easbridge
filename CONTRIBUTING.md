# Contributing

Thanks for looking. EASBridge is small on purpose, and it has to stay safe to run with a mailbox password, so changes are
reviewed closely.

- **Open an issue first** for anything bigger than a typo or a small fix, so that we agree on the shape before you write it. Features
  that need a real Exchange to check are especially worth a conversation: say what server you can test against.
- **Build and test.** You need Go (the version in `go.mod`; `env.sh` uses `.toolchain/go` if you have one, else the Go on your
  `PATH`). Run all three of these before you open a pull request. A new behaviour needs a test that fails without it.
  - `make check` runs vet and the tests with and without the `devfake` tag, the vendored library's tests and the production-binary
    check. It succeeds when it ends without a failure.
  - `make compat` needs Node 22. It runs imapflow, nodemailer and MailFlow's CardDAV client against a test bridge.
  - `make e2e` needs Docker. It runs the install end to end and skips where Docker is not usable.
- **Tests first for protocol bugs.** If a real server did something the fake did not, teach the model in
  `internal/easupstream` and `internal/fakeupstream` the same behaviour, watch the test fail, then fix the code (see [docs/TESTING.md](docs/TESTING.md)).
- **Style.** `gofmt`. Comments say why, not what. Messages and docs: plain sentences, no marketing words. The name is **EASBridge**
  in prose; `easbridge` only where it is an identifier (the binary, the module, the image, paths, the `EASBRIDGE_` variables).
- **Secrets.** Never put a password, token, address of a real server or a real person's mail in a test, a fixture, a log sample or an
  issue. Use `example.com` and `example.test`.
- **Commits.** One logical change per commit, a subject line in the imperative that says what changes (`Refuse a certificate that does
  not match its key`), and a body that says why when it is not obvious. No generated noise in the diff.
- **Licence.** By contributing you agree that your work is under the MIT licence of this repository.
- **Documentation.** The [README](README.md#documentation) lists every document. Say each fact once, in the document that owns it, and link to
  it from the others.
