#!/usr/bin/env bash
# deploy/renew-r2.sh — refresh press's R2 credentials before they expire.
#
# press's artifacts bucket credentials are R2 temporary credentials minted
# from a parent account API token, with a TTL of at most seven days. The
# PARENT is long-lived and lives only on the box (never in this file); the
# child expires. Renewing is therefore: mint a new child, update the
# `press-store` secret, restart the two deployments. Nothing else changes —
# the bucket, the layout and the tenant prefixes are unaffected.
#
# Run it on the box (where the parent token file lives), or anywhere the cf
# CLI is authenticated and the file is present:
#
#   ./deploy/renew-r2.sh                 # renew with a 7-day TTL
#   ./deploy/renew-r2.sh --ttl 86400     # renew for a day
#
# The parent token file: PRESS_R2_PARENT_TOKEN (default
# /etc/anywaye/r2-parent-token on the box). It holds the long-lived
# `cfat_...` account token whose only permission is Workers R2 Storage
# Read+Write on the artifacts bucket. If the parent is lost, the
# credentials cannot be renewed — the dashboard can mint another.
set -euo pipefail

# Two halves, because the two tools live in two places: `cf` is authenticated
# on an operator's machine (the parent token is a dashboard credential), and
# kubectl lives on the box. The script mints where it runs and applies over
# ssh, so a renewal is one command from a laptop.
TTL="604800"
while [ $# -gt 0 ]; do
  case "$1" in
    --ttl) TTL="$2"; shift 2 ;;
    --box) BOX="$2"; shift 2 ;;
    *) echo "renew-r2: unknown argument: $1" >&2; exit 2 ;;
  esac
done
BOX="${BOX:-contabo}"

ACCOUNT_ID="1e83e8ee4d1b6e51c2f4527ea005804d"
BUCKET="anywaye-docs-artifacts"
PARENT_TOKEN_FILE="${PRESS_R2_PARENT_TOKEN:-/etc/anywaye/r2-parent-token}"

if [ ! -r "$PARENT_TOKEN_FILE" ]; then
  echo "renew-r2: no parent token at $PARENT_TOKEN_FILE." >&2
  echo "  It is the long-lived cfat_... token created 2026-10-16 for this bucket." >&2
  exit 1
fi
PARENT_ID="$(cat "$PARENT_TOKEN_FILE-parent-id" 2>/dev/null || echo '')"
if [ -z "$PARENT_ID" ]; then
  echo "renew-r2: $PARENT_TOKEN_FILE-parent-id is missing — it names the parent token's id." >&2
  exit 1
fi
# The parent travels to the box only inside this command's memory: the box
# holds the id (for future parented mints) and never the value, which is why
# the id file is the one written there at setup and the value stays here.

# cf lives in different places on different machines (a global npm install,
# a mise-managed one) and a non-interactive ssh shell does not inherit the
# user's PATH. Find it, or fall back to npx, which is the same package.
CF_BIN="$(command -v cf || true)"
if [ -z "$CF_BIN" ]; then
  for candidate in \
    /usr/local/bin/cf \
    /opt/homebrew/bin/cf \
    "$HOME"/.local/share/mise/installs/node/*/bin/cf; do
    [ -x "$candidate" ] && CF_BIN="$candidate" && break
  done
fi
if [ -z "$CF_BIN" ]; then
  echo "renew-r2: the cf CLI is required (npm i -g cf)." >&2
  exit 1
fi
echo "renew-r2: cf at $CF_BIN"

echo "renew-r2: minting a ${TTL}s child of $PARENT_ID for $BUCKET"
CREDS="$("$CF_BIN" r2 temporary-credentials create \
  --bucket "$BUCKET" \
  --permission object-read-write \
  --ttl-seconds "$TTL" \
  --parent-access-key-id "$PARENT_ID")"

# Extract with node rather than jq: the CLI prints bare JSON and the box has
# node on PATH wherever press runs.
read -r KEY_ID SECRET TOKEN <<<"$(node -e "
const c = JSON.parse(process.argv[1]);
console.log(c.accessKeyId, c.secretAccessKey, c.sessionToken);
" "$CREDS")"

# The apply runs on the box: cluster credentials live there and nowhere else.
ssh "$BOX" sudo k3s kubectl create secret generic press-store -n press \
  --from-literal="PRESS_STORE_ENDPOINT=https://${ACCOUNT_ID}.r2.cloudflarestorage.com" \
  --from-literal="PRESS_STORE_BUCKET=$BUCKET" \
  --from-literal="PRESS_STORE_ACCESS_KEY_ID=$KEY_ID" \
  --from-literal="PRESS_STORE_SECRET_ACCESS_KEY=$SECRET" \
  --from-literal="PRESS_STORE_SESSION_TOKEN=$TOKEN" \
  --dry-run=client -o yaml '|' sudo k3s kubectl apply -f - >/dev/null

ssh "$BOX" sudo k3s kubectl rollout restart deployment/press deployment/press-claim -n press >/dev/null

echo "renew-r2: renewed for ${TTL}s (until $(date -u -v+${TTL}S +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d @$(( $(date +%s) + TTL )) +%Y-%m-%dT%H:%M:%SZ))"
