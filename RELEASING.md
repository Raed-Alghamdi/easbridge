# Releasing

A release is a tag `vX.Y.Z` on `main`. The tag starts `.github/workflows/release.yml`, which refuses to go on unless the tag fits the
code, runs every check, and then builds and pushes the image. Everything before the tag is yours to do by hand.

## Checklist

1. **`main` is green.** The CI workflow passes on the commit you want to release (gofmt, vet, tests with and without `devfake`, the
   vendored library, `check-prod`, the compose merge test, `govulncheck`, `compat`, `e2e`).
2. **Version.** `internal/version/version.go` (`Version`), `Makefile` (`VERSION ?=`) and `Dockerfile` (`ARG VERSION=`) say the new version;
   `cmd/easbridge/dockerfile_test.go` fails if they differ. The image reference in the README and INSTALL.md (`IMAGE=...:X.Y.Z`) says it
   too; the other documents write `<version>`.
3. **`CHANGELOG.md`** has a `## [X.Y.Z]` entry that says what changed for someone who upgrades, including anything that needs a manual step
   (a new environment variable that changes a default, a schema change, a changed default port).
4. **A copy of the data directory restores.** If the schema changed, upgrade a copy of a real data volume once and read the log.
5. **Tag and push.**

   ```sh
   git switch main && git pull
   git tag -s vX.Y.Z -m "EASBridge X.Y.Z"      # or -a if you do not sign
   git push origin vX.Y.Z
   ```

   The workflow fails, before it builds anything, if the tag without its `v` differs from `internal/version.Version`, if `CHANGELOG.md` has no
   `## [X.Y.Z]` entry, or if the tagged commit is not on `main`. It then runs `make check`, `make vulncheck`, `make compat` and `make e2e`
   (with `EASBRIDGE_E2E_REQUIRED=1`, so that an unusable Docker fails instead of skipping), and only then builds `linux/amd64` and
   `linux/arm64`, pushes `ghcr.io/<owner>/easbridge:X.Y.Z` and `:X.Y`, with an SBOM and a provenance attestation. Only the image job has
   `packages: write`.
6. **First release only: make the package public and link it to the repository.** A package that GHCR creates on its first push is private.
   Open the repository's **Packages** (or your profile's Packages), choose `easbridge`, then **Package settings**:
   - under **Manage Actions access**, check that the repository is listed with the Write role (it is linked by the
     `org.opencontainers.image.source` label the metadata action sets);
   - under **Danger Zone**, **Change visibility**, choose **Public**.

   Then check from a machine that is not logged in: `docker pull ghcr.io/<owner>/easbridge:X.Y.Z`.
7. **GitHub release.** Create a release from the tag with the CHANGELOG entry as its text.
8. **Check the image.** `docker run --rm ghcr.io/<owner>/easbridge:X.Y.Z version`, and one `init` run as INSTALL.md shows.

## If a release goes wrong

Do not move a tag. Fix on `main`, bump the patch version and tag again. A bad image is deleted from the package's page, but anyone who has
pulled it keeps it, so say so in the CHANGELOG.

## Dependencies

Dependabot proposes updates to Go modules, GitHub Actions (pinned by commit SHA, with the version in a comment) and the Docker base images
(pinned by digest). Read each one; run `make check`, `make compat` and `make e2e` before merging a base image or action update.
