#!/bin/sh
# Mutation check for the random test: builds the bridge with a fault built in
# on purpose (build tag faultinject: removals are reported to the connections
# in the wrong order, see internal/mirror/fault_on.go) and expects
# TestRandomConcurrentChanges to find it. If the test does NOT fail, it is
# worthless, and this script ends with an error.
set -eu
cd "$(dirname "$0")/.."
mkdir -p test/out
set +e
docker run --rm --name mail-bridge-mutation-check \
  -v "$PWD":/src -v mail-bridge-gomod:/go/pkg/mod -v mail-bridge-gocache:/root/.cache/go-build \
  -w /src golang:1.24 \
  go test -tags faultinject -count=1 -run TestRandomConcurrentChanges ./internal/bridge/ >test/out/mutation-check.log 2>&1
rc=$?
set -e
if [ "$rc" -eq 0 ]; then
  echo "ERROR: The random test did not find the built-in fault."
  exit 1
fi
if ! grep -q "disagree" test/out/mutation-check.log; then
  echo "ERROR: The test failed, but not on a disagreement (see test/out/mutation-check.log)."
  exit 1
fi
echo "Mutation check passed: the random test finds the built-in fault."
grep -E "^\s+conn[0-9]+: " test/out/mutation-check.log | head -4
grep -E "^(--- FAIL|FAIL|ok)" test/out/mutation-check.log | head -8
