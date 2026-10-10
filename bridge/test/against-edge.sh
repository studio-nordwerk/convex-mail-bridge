#!/usr/bin/env bash
# Probe of the bridge against a given edge (a local Convex running the real code).
#
# Starts the bridge in Docker (container mail-bridge-edge) against EDGE_URL and checks it over IMAP.
# Nothing is assumed about the edge: what a mailbox contains is read over IMAP and compared with
# itself, with the counters of the edge (STATUS) and with EXPECT_INBOX/EXPECT_SENT.
# Printed are folders, UIDs, sizes and counters; never addresses, passwords, subjects, contents.
#
#   EDGE_URL=http://host.docker.internal:14211 MAIL_BRIDGE_SECRET=… \
#   MAILBOX_ADDRESS=… DEVICE_PASSWORD=… ./test/against-edge.sh
#
# Inputs (environment):
#   EDGE_URL            address of the edge as seen from the container (without /bridge/v1)
#   MAIL_BRIDGE_SECRET  shared secret (at least 32 characters)
#   MAILBOX_ADDRESS     address of the mailbox
#   DEVICE_PASSWORD     a device password for it
#   SECOND_ADDRESS, SECOND_PASSWORD   optional: second mailbox, checks the separation
#   EXPECT_INBOX, EXPECT_SENT         optional: expected number of mails at the start
#
# Switches:
#   SKIP_IDLE=1         skip the IDLE step (it waits for a mail from outside)
#   ALLOW_EXPUNGE=1     mark a mail as deleted and remove it with UID EXPUNGE
#                       (the mail from the IDLE step, otherwise the newest; EXPUNGE_UID=<n> picks one)
#   MBSYNC=1            then mbsync against the same bridge: first sync, second run without fetches
#   KEEP=0              remove the container at the end (default: it keeps running)
#   MAX_MAILS=200       this many mails per folder are read in full (the newest)
#   IDLE_WAIT_SECONDS=900, IDLE_GRACE_SECONDS=30   wait for the signal, grace period after it
#   IMAP_PORT=3243 SMTP_PORT=3565 ADMIN_PORT=3290  ports on 127.0.0.1
#   DOCKER_RUN_ARGS     further arguments for docker run (e.g. --network <network>)
#
# IDLE: The script reports "IDLE is running". Then deliver a mail from outside and give the
# signal afterwards: create the file /tmp/against-edge.trigger (touch) or, at the terminal, press Enter.
#
#   ./test/against-edge.sh stop      remove container mail-bridge-edge
#
# Exits with status 0 if every check passed; 1 on failures; 2 on wrong usage.
set -uo pipefail
trap '' PIPE
cd "$(dirname "$0")/.."

NAME=mail-bridge-edge
IMAGE=mail-bridge-edge
TRIGGER=/tmp/against-edge.trigger

if [ "${1:-}" = "stop" ]; then
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  echo "$NAME removed."
  exit 0
fi
if [ $# -gt 0 ]; then
  awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "$0"
  exit 2
fi

missing=0
for v in EDGE_URL MAIL_BRIDGE_SECRET MAILBOX_ADDRESS DEVICE_PASSWORD; do
  if [ -z "${!v:-}" ]; then
    echo "against-edge: $v is missing" >&2
    missing=1
  fi
done
if [ -n "${SECOND_ADDRESS:-}" ] && [ -z "${SECOND_PASSWORD:-}" ] || [ -z "${SECOND_ADDRESS:-}" ] && [ -n "${SECOND_PASSWORD:-}" ]; then
  echo "against-edge: SECOND_ADDRESS and SECOND_PASSWORD only together" >&2
  missing=1
fi
[ "$missing" = 0 ] || exit 2
if [ "${#MAIL_BRIDGE_SECRET}" -lt 32 ]; then
  echo "against-edge: MAIL_BRIDGE_SECRET has fewer than 32 characters; with it the edge is off (401)" >&2
  exit 2
fi
# docker passes these names through from the environment; the values appear on no command line.
export MAIL_BRIDGE_SECRET MAILBOX_ADDRESS DEVICE_PASSWORD
export SECOND_ADDRESS="${SECOND_ADDRESS:-}" SECOND_PASSWORD="${SECOND_PASSWORD:-}"
export EXPECT_INBOX="${EXPECT_INBOX:-}" EXPECT_SENT="${EXPECT_SENT:-}"
export SKIP_IDLE="${SKIP_IDLE:-}" ALLOW_EXPUNGE="${ALLOW_EXPUNGE:-}" EXPUNGE_UID="${EXPUNGE_UID:-}"
export MAX_MAILS="${MAX_MAILS:-200}" IDLE_WAIT_SECONDS="${IDLE_WAIT_SECONDS:-900}" IDLE_GRACE_SECONDS="${IDLE_GRACE_SECONDS:-30}"

IMAP_PORT="${IMAP_PORT:-3243}"
SMTP_PORT="${SMTP_PORT:-3565}"
ADMIN_PORT="${ADMIN_PORT:-3290}"
for p in "$IMAP_PORT" "$SMTP_PORT" "$ADMIN_PORT"; do
  case "$p" in
    ''|*[!0-9]*) echo "against-edge: port "$p" is not a number" >&2; exit 2 ;;
  esac
done

case "$EDGE_URL" in
  *://localhost*|*://127.*|*://\[::1\]*)
    echo "       Note: EDGE_URL points to localhost. From inside the container that is the container itself;" >&2
    echo "       an edge on this machine is called host.docker.internal there." >&2
    ;;
esac

# Credentials do not belong in the address (the bridge refuses them, and here they would show up in the output).
case "${EDGE_URL#*://}" in
  *@*)
    echo "against-edge: EDGE_URL must not carry credentials or an @" >&2
    exit 2
    ;;
esac
export CONVEX_SITE_URL="$EDGE_URL"
echo "== Bridge against a given edge =="
echo "       Edge: $EDGE_URL"
echo "       Bridge: container $NAME, IMAP 127.0.0.1:$IMAP_PORT, SMTP 127.0.0.1:$SMTP_PORT, metrics http://127.0.0.1:$ADMIN_PORT/metrics"

if ! docker build -q --target probe -t "$IMAGE" . >/dev/null; then
  echo "FAIL   the image $IMAGE cannot be built" >&2
  exit 1
fi
docker rm -f "$NAME" >/dev/null 2>&1 || true
# shellcheck disable=SC2086
if ! docker run -d --name "$NAME" \
  -p "127.0.0.1:$IMAP_PORT:1143" -p "127.0.0.1:$SMTP_PORT:1465" -p "127.0.0.1:$ADMIN_PORT:8080" \
  -e CONVEX_SITE_URL -e MAIL_BRIDGE_SECRET \
  -e PROXY_PROTOCOL=off -e ADMIN_ADDR=0.0.0.0:8080 \
  -e IMAP_MAX_CONNS_PER_IP=50 -e SMTP_MAX_CONNS_PER_IP=50 \
  ${DOCKER_RUN_ARGS:-} \
  "$IMAGE" >/dev/null; then
  echo "FAIL   the container $NAME does not start (ports in use?)" >&2
  exit 1
fi

up=0
for _ in $(seq 1 40); do
  if curl -fsS "http://127.0.0.1:$ADMIN_PORT/healthz" >/dev/null 2>&1; then
    up=1
    break
  fi
  sleep 0.25
done
if [ "$up" != 1 ]; then
  echo "FAIL   the bridge did not come up:" >&2
  docker logs "$NAME" 2>&1 | tail -20 >&2
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  exit 1
fi
echo "ok     the bridge is running"

# Clean up, also on abort (Ctrl-C): the FIFO always, the container only with KEEP=0.
fifo=""
probe_pid=""
cleanup() {
  [ -n "$probe_pid" ] && kill "$probe_pid" 2>/dev/null
  [ -n "$fifo" ] && rm -f "$fifo"
  if [ "${KEEP:-1}" = "0" ]; then
    docker rm -f "$NAME" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT
trap 'echo; echo "       aborted"; exit 130' INT TERM

finish() {
  if [ "${KEEP:-1}" = "0" ]; then
    echo "       Container $NAME is being removed (KEEP=0)."
  else
    echo "       Container $NAME keeps running (IMAP 127.0.0.1:$IMAP_PORT). Remove it with: ./test/against-edge.sh stop"
  fi
}

# ---- the probe ---------------------------------------------------------------
# The probe program runs in the container of the bridge. It receives the signal for the IDLE step
# (file or Enter) as a line on its standard input.
rm -f "$TRIGGER"
fifo="$(mktemp -u "${TMPDIR:-/tmp}/against-edge.XXXXXX")"
mkfifo -m 600 "$fifo"
docker exec -i \
  -e MAILBOX_ADDRESS -e DEVICE_PASSWORD -e SECOND_ADDRESS -e SECOND_PASSWORD \
  -e EXPECT_INBOX -e EXPECT_SENT -e SKIP_IDLE -e ALLOW_EXPUNGE -e EXPUNGE_UID \
  -e MAX_MAILS -e IDLE_WAIT_SECONDS -e IDLE_GRACE_SECONDS \
  -e PROBE_TRIGGER_HINT="touch $TRIGGER (or Enter)" \
  "$NAME" /edgeprobe <"$fifo" &
probe_pid=$!
exec 3>"$fifo"
# Every signal is passed on; the probe program discards one that arrives before the IDLE step.
while kill -0 "$probe_pid" 2>/dev/null; do
  if [ -e "$TRIGGER" ]; then
    rm -f "$TRIGGER"
    echo "go" >&3 2>/dev/null || true
  fi
  if [ -t 0 ]; then
    if IFS= read -r -t 1 _; then
      echo "go" >&3 2>/dev/null || true
    fi
  else
    sleep 0.3
  fi
done
wait "$probe_pid"
status=$?
probe_pid=""
exec 3>&-
rm -f "$fifo" "$TRIGGER"

# ---- mbsync ------------------------------------------------------------------
if [ "${MBSYNC:-}" = "1" ]; then
  echo
  echo "== mbsync against the same bridge =="
  MB_IMAGE=mail-bridge-mbsync
  if ! docker image inspect "$MB_IMAGE" >/dev/null 2>&1; then
    docker build -q -t "$MB_IMAGE" - >/dev/null <<'DOCKER'
FROM golang:1.24
RUN apt-get update && apt-get install -y --no-install-recommends isync && rm -rf /var/lib/apt/lists/*
DOCKER
  fi
  mkdir -p test/out
  # In the network of the bridge container: IMAP and metrics are on 127.0.0.1 there. The Maildir stays
  # in the container and disappears with it.
  docker run --rm --name mail-bridge-edge-mbsync --network "container:$NAME" \
    -v "$PWD":/src -v mail-bridge-gomod:/go/pkg/mod -v mail-bridge-gocache:/root/.cache/go-build \
    -e MAILBOX_ADDRESS -e DEVICE_PASSWORD \
    -e PROBE_IMAP_ADDR=127.0.0.1:1143 -e PROBE_METRICS_URL=http://127.0.0.1:8080/metrics \
    -w /src "$MB_IMAGE" \
    go test -tags mbsyncedge -count=1 -v -run TestMbsyncAgainstEdge ./test/mbsync/ 2>&1 |
    grep -v '^{"t":' | sed 's/^/       /' | tee test/out/mbsync-edge.log
  if grep -q '^       ok ' test/out/mbsync-edge.log; then
    echo "ok     mbsync: first sync byte for byte, second run without fetches"
  else
    echo "FAIL   mbsync run (see above)"
    status=1
  fi
fi

# ---- what the bridge logged meanwhile (event names and counts only) ---------------
echo
echo "== Log of the bridge (events) =="
docker logs "$NAME" 2>&1 | sed -n 's/.*"ev":"\([^"]*\)".*/\1/p' | sort | uniq -c | sed 's/^/       /'
faults="$(docker logs "$NAME" 2>&1 | grep -c '"ev":"edge_fault"' || true)"
if [ "${faults:-0}" != "0" ]; then
  echo "FAIL   edge_fault $faults time(s) in the log: the edge rejected requests of the bridge or failed internally"
  docker logs "$NAME" 2>&1 | grep '"ev":"edge_fault"' | head -10 | sed 's/^/       /'
  status=1
fi

echo
finish
if [ "$status" = 0 ]; then
  echo "== All checks passed =="
else
  echo "== There were failures (status $status) =="
fi
exit "$status"
