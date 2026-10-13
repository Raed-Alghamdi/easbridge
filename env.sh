# EASBridge Go environment.
# Usage: source env.sh   (from any checkout: the paths follow the location of this file)
#
# Which Go it uses, in this order:
#   1. EASBRIDGE_GOROOT, if set (the CI image sets it to its own Go);
#   2. the project-local toolchain in .toolchain/go, if the checkout has one;
#   3. otherwise the Go that is on your PATH (it must satisfy the `go` line of go.mod).
# It says which one on stderr, once per shell, unless EASBRIDGE_ENV_QUIET is set (the Makefile sets it and
# prints the line itself in `make go-version`). The Go caches live in .cache inside the checkout in all cases,
# so a build never writes to your home directory.
_eb_root="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)"
if [ -n "${EASBRIDGE_GOROOT:-}" ]; then
  export GOROOT="$EASBRIDGE_GOROOT"
  export PATH="$GOROOT/bin:$PATH"
  _eb_which="the toolchain named by EASBRIDGE_GOROOT ($GOROOT)"
elif [ -x "$_eb_root/.toolchain/go/bin/go" ]; then
  export GOROOT="$_eb_root/.toolchain/go"
  export PATH="$GOROOT/bin:$PATH"
  _eb_which="the project-local toolchain (.toolchain/go)"
elif command -v go >/dev/null 2>&1; then
  unset GOROOT # the go on PATH knows its own root
  _eb_which="the Go on your PATH ($(command -v go))"
else
  echo "env.sh: no Go found: there is no .toolchain/go in this checkout and no go on your PATH (install Go, see go.mod for the version)" >&2
  unset _eb_root _eb_which
  return 1 2>/dev/null || exit 1
fi
export GOPATH="$_eb_root/.cache/gopath"
export GOMODCACHE="$_eb_root/.cache/gomod"
export GOCACHE="$_eb_root/.cache/gobuild"
export GOTOOLCHAIN="${GOTOOLCHAIN:-local}"
export GOFLAGS=-modcacherw
if [ -z "${EASBRIDGE_ENV_QUIET:-}" ] && [ -z "${_EASBRIDGE_ENV_SAID:-}" ]; then
  echo "env.sh: using $_eb_which: $(go version)" >&2
  export _EASBRIDGE_ENV_SAID=1
fi
unset _eb_root _eb_which
