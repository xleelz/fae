#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
# Only this explicitly selected integration fixture variable is permitted.
while IFS='=' read -r name _; do
 case "$name" in FAE_TEST_DOLT_BINARY) ;; FAE_*) unset "$name" ;; esac
done < <(env)
export GOCACHE="$PWD/.cache/go-build"
mkdir -p "$GOCACHE"
# Scratch lives on the repository filesystem, never an implicit RAM-backed /tmp.
export TMPDIR
TMPDIR=$(mktemp -d "$PWD/.cache/t.XXXXXX")
trap 'rm -rf "$TMPDIR"' EXIT
python3 scripts/count-tests.py --self-test
go vet ./...
if ! go test -timeout=20m -json -race -count=1 -coverprofile=.cache/adapter-fleet-final-coverage.out "$@" ./... > .cache/adapter-fleet-final-test.json; then
 cat .cache/adapter-fleet-final-test.json
 exit 1
fi
python3 scripts/count-tests.py .cache/adapter-fleet-final-test.json
go tool cover -func=.cache/adapter-fleet-final-coverage.out | tail -1
# A direct import census supplements Go's built-in dependency-cycle rejection.
for pkg in ./domain ./workflow; do
 imports=$(go list -f '{{join .Imports "\n"}}' "$pkg")
 if printf '%s\n' "$imports" | grep -Eq '^(os|os/exec|net|net/.*|syscall)$'; then
  printf 'forbidden I/O import in %s\n' "$pkg" >&2
  exit 1
 fi
done
python3 - <<'PYCENSUS'
import os
# Apple's compiler driver may create its disposable xcrun cache here.
residue = [name for name in os.listdir(os.environ['TMPDIR']) if name != 'xcrun_db']
assert not residue, f'test scratch residue: {residue}'
print('Temporary-directory census passed')
PYCENSUS
