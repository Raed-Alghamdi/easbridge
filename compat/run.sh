#!/usr/bin/env bash
# Builds the devfake bridge, makes a throwaway CA and runs the imapflow /
# nodemailer harness against it. Never talks to a real mail server.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck disable=SC1091
source "$ROOT/env.sh"
[ -n "${NODE_BIN_DIR:-}" ] && export PATH="$NODE_BIN_DIR:$PATH"

TMP="$(mktemp -d "${TMPDIR:-/tmp}/easbridge-compat.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

cd "$ROOT"
mkdir -p bin
CGO_ENABLED=0 go build -tags devfake -trimpath -o bin/easbridge-devfake ./cmd/easbridge
go run ./compat/genpki "$TMP/tls"

cd "$ROOT/compat"
if [ ! -d node_modules/imapflow ] || [ ! -d node_modules/fast-xml-parser ] || [ ! -d node_modules/undici ]; then
  npm ci --no-audit --no-fund
fi
# The CA must be trusted by Node from process start.
export NODE_EXTRA_CA_CERTS="$TMP/tls/ca.pem"
export EASBRIDGE_BIN="$ROOT/bin/easbridge-devfake"
export COMPAT_TMP="$TMP"
exec node "${COMPAT_SCRIPT:-run.js}" "$@"
