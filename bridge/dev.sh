#!/bin/sh
# Starts bridge and test edge locally in one container (mail-bridge-dev):
#   IMAP 127.0.0.1:3143 · SMTP 127.0.0.1:3465 · metrics 127.0.0.1:3190
# Without TLS, without the PROXY protocol, with two invented mailboxes. The
# credentials end up in .dev-credentials (mode 600, not in the repo).
#
#   ./dev.sh                              build and start (replaces a running container)
#   ./dev.sh mail <address> "<subject>"   inject a new mail into INBOX (to watch IDLE)
#   ./dev.sh stop                         stop and remove
set -eu
cd "$(dirname "$0")"
NAME=mail-bridge-dev
CRED=.dev-credentials

if [ "${1:-}" = "stop" ]; then
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  echo "$NAME stopped."
  exit 0
fi

if [ "${1:-}" = "mail" ]; then
  if [ $# -lt 3 ]; then
    echo "Usage: ./dev.sh mail <address> \"<subject>\" [text]" >&2
    exit 2
  fi
  shift
  exec docker exec "$NAME" /mail-bridge-dev mail "$@"
fi

if [ ! -f "$CRED" ]; then
  umask 077
  {
    echo "# Local development bridge: invented mailboxes in the test edge. Do not check in."
    echo "# IMAP 127.0.0.1:3143 · SMTP 127.0.0.1:3465 · no TLS · user name = address"
    echo "# One per line: address device password"
    echo "mara@workshop-one.test mb_$(openssl rand -hex 24)"
    echo "jonas@workshop-two.test mb_$(openssl rand -hex 24)"
  } >"$CRED"
fi
chmod 600 "$CRED"

docker build -q --target dev -t "$NAME" . >/dev/null
docker rm -f "$NAME" >/dev/null 2>&1 || true
# The credentials go into the container as a file (read-only), not as an
# environment variable.
docker run -d --name "$NAME" \
  -p 127.0.0.1:3143:1143 -p 127.0.0.1:3465:1465 -p 127.0.0.1:3190:8080 \
  -v "$PWD/$CRED":/run/dev-credentials:ro \
  -e ADMIN_ADDR=0.0.0.0:8080 \
  -e PROXY_PROTOCOL=off \
  -e DEV_TICK_SECONDS="${DEV_TICK_SECONDS:-0}" \
  "$NAME" >/dev/null

for i in 1 2 3 4 5 6 7 8 9 10; do
  if curl -fsS http://127.0.0.1:3190/healthz >/dev/null 2>&1; then
    echo "$NAME is running: IMAP 127.0.0.1:3143, SMTP 127.0.0.1:3465, metrics http://127.0.0.1:3190/metrics"
    echo "Credentials: $PWD/$CRED"
    exit 0
  fi
  sleep 0.5
done
echo "$NAME did not come up:" >&2
docker logs "$NAME" 2>&1 | tail -20 >&2
exit 1
