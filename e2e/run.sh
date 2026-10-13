#!/usr/bin/env bash
# `make e2e`: the whole install path in Docker. Skips (exit 0, one line) when Docker is not usable.
#
#  1. builds a devfake image of the bridge (a fake Exchange inside; never publish it);
#  2. runs `easbridge init` from that image, non-interactively, next to a stand-in MailFlow compose file
#     (a `backend` service on a `mailflow` network), with --image pointing at the devfake image;
#  3. `docker compose up`: the backend (a Node 22 container) must start after the bridge is healthy and
#     connect with imapflow, nodemailer and HTTPS to CardDAV, certificate verification on, trusting only
#     NODE_EXTRA_CA_CERTS=/easbridge-ca/ca.pem;
#  4. checks the output and the order of events, and removes everything, volumes included.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
[ -n "${NODE_BIN_DIR:-}" ] && export PATH="$NODE_BIN_DIR:$PATH"

# Without a usable Docker the run is skipped (exit 0), unless EASBRIDGE_E2E_REQUIRED is set (CI): then it fails.
skip() { echo "e2e: SKIPPED ($1)"; if [ -n "${EASBRIDGE_E2E_REQUIRED:-}" ]; then echo "e2e: but EASBRIDGE_E2E_REQUIRED is set"; exit 1; fi; exit 0; }
command -v docker >/dev/null 2>&1 || skip "docker is not installed"
why="$(docker info 2>&1 >/dev/null)" || skip "Docker is not usable here: $(echo "$why" | tail -1)"
docker compose version >/dev/null 2>&1 || skip "docker compose is not available"

TMP="$(mktemp -d "${TMPDIR:-/tmp}/easbridge-e2e.XXXXXX")"
PROJECT="easbridgee2e$$"
IMG="easbridge-e2e:devfake-$$"
COMPOSE=(docker compose -p "$PROJECT" -f docker-compose.yml -f docker-compose.easbridge.yml -f e2e.override.yml)
cleanup() {
  (cd "$TMP" && "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1) || true
  docker rmi "$IMG" >/dev/null 2>&1 || true
  rm -rf "$TMP"
}
trap cleanup EXIT

cd "$ROOT/compat"
if [ ! -d node_modules/imapflow ] || [ ! -d node_modules/nodemailer ]; then npm ci --no-audit --no-fund; fi
cd "$ROOT"

echo "e2e: building the devfake image"
docker build -q -t "$IMG" --build-arg GOTAGS=devfake --build-arg VERSION=e2e . >/dev/null

cp e2e/docker-compose.yml e2e/e2e.override.yml e2e/backend.js "$TMP/"
cp -r compat/node_modules "$TMP/node_modules"

echo "e2e: easbridge init"
docker run --rm --user "$(id -u):$(id -g)" -v "$TMP:/out" "$IMG" init \
  --eas-url https://exchange.invalid/Microsoft-Server-ActiveSync --users e2e@example.test --image "$IMG" >"$TMP/init.out"
test -f "$TMP/docker-compose.easbridge.yml" && test -f "$TMP/easbridge.env"
grep -q "up -d" "$TMP/init.out"

echo "e2e: docker compose up"
cd "$TMP"
set +e
"${COMPOSE[@]}" up --abort-on-container-exit --exit-code-from backend >"$TMP/up.log" 2>&1
rc=$?
set -e
cat "$TMP/up.log" | sed 's/^/  | /' | tail -40

fail() { echo "e2e: FAILED: $*"; exit 1; }
[ "$rc" = 0 ] || fail "the backend exited with $rc"
for want in "E2E ca.pem present at start" "E2E imap ok" "E2E smtp ok" "E2E carddav ok" "E2E OK imap smtp carddav"; do
  grep -q "$want" "$TMP/up.log" || fail "the backend log has no \"$want\""
done

# The backend started after the bridge's first successful health check.
python3 - "$(docker inspect easbridge-e2e)" "$(docker inspect easbridge-e2e-backend)" <<'PY' || fail "the backend did not wait for a healthy bridge"
import json, sys
from datetime import datetime
def t(s):
    s = s.replace("Z", "+00:00")
    head, _, rest = s.partition(".")
    if rest:
        frac, _, tz = rest.partition("+")
        s = head + "." + frac[:6] + ("+" + tz if tz else "")
    return datetime.fromisoformat(s)
bridge = json.loads(sys.argv[1])[0]
backend = json.loads(sys.argv[2])[0]
ok = [e for e in bridge["State"]["Health"]["Log"] if e["ExitCode"] == 0]
first_ok = t(ok[0]["End"])
started = t(backend["State"]["StartedAt"])
print("bridge first healthy:", first_ok, " backend started:", started)
sys.exit(0 if started >= first_ok else 1)
PY

echo "e2e: PASSED (init, compose up, the backend waited for the healthy bridge and reached IMAP, SMTP and CardDAV with verification on)"
