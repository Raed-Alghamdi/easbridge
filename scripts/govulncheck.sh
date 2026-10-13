#!/usr/bin/env bash
# One command to check the dependencies for known vulnerabilities:
#
#   scripts/govulncheck.sh              # govulncheck ./...
#   scripts/govulncheck.sh -show verbose ./...
#
# It runs the pinned govulncheck with the project-local Go toolchain and caches
# from env.sh, so nothing is installed system-wide: the tool itself comes from the
# module cache in .cache/gomod, its build from .cache/gobuild, and its copy of the
# vulnerability database (https://vuln.go.dev, the only network access besides the
# module proxy on the first run) from .cache/xdg. Without arguments it checks
# every package of the module.
#
# govulncheck reports two things: vulnerabilities in code the bridge actually
# calls (these fail the check, exit status 3) and vulnerabilities in the modules it
# requires but does not call (listed for information). To update the tool, change
# the version below, run this script and read the result before committing.
set -euo pipefail

GOVULNCHECK_VERSION=v1.8.0

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=../env.sh
source "$here/env.sh"
# Keep the vulnerability database and the tool's own settings inside the project.
export XDG_CACHE_HOME="$here/.cache/xdg"
export XDG_CONFIG_HOME="$here/.cache/xdg-config"
mkdir -p "$XDG_CACHE_HOME" "$XDG_CONFIG_HOME"

cd "$here"
if [ "$#" -eq 0 ]; then
  set -- ./...
fi
exec go run "golang.org/x/vuln/cmd/govulncheck@${GOVULNCHECK_VERSION}" "$@"
