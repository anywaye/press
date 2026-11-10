#!/usr/bin/env bash
# renew-r2.sh — refresh press's R2 credentials before they expire.
set -euo pipefail

TTL="604800"
while [ $# -gt 0 ]; do
  case "$1" in
    --ttl) TTL="$2"; shift 2 ;;
    *) echo "renew-r2: unknown argument: $1" >&2; exit 2 ;;
  esac
done

ACCOUNT_ID="1e83e8ee4d1b6e51c2f4527ea005804d"
BUCKET="cafaye-docs-artifacts"
PARENT_TOKEN_FILE="${PRESS_R2_PARENT_TOKEN:-/etc/anywaye/r2-parent-token}"
DEV_MF="${DEV_MF:-dev-mf}"

if [ ! -r "$PARENT_TOKEN_FILE" ]; then
  echo "renew-r2: no parent token at $PARENT_TOKEN_FILE." >&2
  exit 1
fi
PARENT_ID="$(cat "$PARENT_TOKEN_FILE-parent-id" 2>/dev/null || echo '')"
if [ -z "$PARENT_ID" ]; then
  echo "renew-r2: $PARENT_TOKEN_FILE-parent-id is missing." >&2
  exit 1
fi

CF_BIN="$(command -v cf || true)"
if [ -z "$CF_BIN" ]; then
  for candidate in /usr/local/bin/cf /opt/homebrew/bin/cf "$HOME"/.local/share/mise/installs/node/*/bin/cf; do
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

read -r KEY_ID SECRET TOKEN <<<"$(node -e "
const c = JSON.parse(process.argv[1]);
console.log(c.accessKeyId, c.secretAccessKey, c.sessionToken);
" "$CREDS")"

echo "renew-r2: updating SOPS-encrypted secret on dev-mf"

ssh "$DEV_MF" "KEY_ID='$KEY_ID' SECRET='$SECRET' TOKEN='$TOKEN' ACCOUNT_ID='$ACCOUNT_ID' BUCKET='$BUCKET' TTL='$TTL' bash -s" <<'REMOTE'
set -euo pipefail
cd /code/anywaye/press/deploy

cat > /tmp/secret-store-plain.yaml <<EOF
apiVersion: v1
kind: Secret
metadata:
    name: press-store
    namespace: press
type: Opaque
stringData:
    PRESS_STORE_ENDPOINT: https://${ACCOUNT_ID}.r2.cloudflarestorage.com
    PRESS_STORE_BUCKET: ${BUCKET}
    PRESS_STORE_ACCESS_KEY_ID: ${KEY_ID}
    PRESS_STORE_SECRET_ACCESS_KEY: ${SECRET}
    PRESS_STORE_SESSION_TOKEN: ${TOKEN}
EOF

sops --encrypt /tmp/secret-store-plain.yaml > secret-store.yaml
rm -f /tmp/secret-store-plain.yaml

cd /code/anywaye/press
git add deploy/secret-store.yaml
git commit -m "chore: renew R2 credentials (TTL ${TTL}s)"
git push origin master

echo "renew-r2: committed and pushed. Argo CD will sync automatically."
REMOTE

echo "renew-r2: renewed for ${TTL}s"
