#!/bin/sh
# Runs mbsync (isync), a strict real IMAP client, against the bridge and the
# test edge: first sync, second run without fetches, changes on both sides,
# stable follow-up run. All in one Docker container, nothing is opened to the
# outside. Output is also written to test/out/mbsync.log.
set -eu
cd "$(dirname "$0")/.."
IMAGE=mail-bridge-mbsync
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
  docker build -q -t "$IMAGE" - <<'DOCKER'
FROM golang:1.24
RUN apt-get update && apt-get install -y --no-install-recommends isync && rm -rf /var/lib/apt/lists/*
DOCKER
fi
mkdir -p test/out
docker run --rm --name mail-bridge-mbsync-run \
  -v "$PWD":/src -v mail-bridge-gomod:/go/pkg/mod -v mail-bridge-gocache:/root/.cache/go-build \
  -w /src "$IMAGE" \
  go test -tags mbsync -count=1 -v ./test/mbsync/ 2>&1 | grep -v '^{"t":' | tee test/out/mbsync.log
grep -q '^ok ' test/out/mbsync.log
