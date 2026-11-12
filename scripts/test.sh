#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
# Only these explicitly selected test variables are permitted. Live tests use
# real host pressure unless FAE_TEST_QUIET_HOST_PRESSURE=1 is set deliberately.
while IFS='=' read -r name _; do
 case "$name" in FAE_TEST_DOLT_BINARY|FAE_TEST_HERDR_BINARY|FAE_TEST_QUIET_HOST_PRESSURE) ;; FAE_*) unset "$name" ;; esac
done < <(env)
export GOCACHE="$PWD/.cache/go-build"
mkdir -p "$GOCACHE"
# Scratch lives on the repository filesystem, never an implicit RAM-backed /tmp.
export TMPDIR
TMPDIR=$(mktemp -d "$PWD/.cache/t.XXXXXX")
trap 'rm -rf "$TMPDIR"' EXIT
python3 scripts/count-tests.py --self-test
python3 scripts/check-ledger.py --self-test
python3 scripts/test-hygiene.py --self-test
python3 scripts/self-host.py --self-test
python3 scripts/test-hygiene.py snapshot .cache/fixture-roots.json
python3 scripts/check-ledger.py
go vet ./...
# The daemon package runs alone, after the others: its live tests admit
# against real host pressure, which other packages' race-instrumented compiles
# and tests running beside it would raise.
others=$(go list ./... | grep -v '/daemon$')
status=0
go test -timeout=45m -json -race -count=1 -coverprofile=.cache/coverage-others.out "$@" $others > .cache/test.json || status=1
go test -timeout=150m -json -race -count=1 -coverprofile=.cache/coverage-daemon.out "$@" ./daemon >> .cache/test.json || status=1
if [ "$status" -ne 0 ]; then
 cat .cache/test.json
 exit 1
fi
# FAE-VER-003: the race-mode run above keeps a quarter of the full-library
# generated corpus; run it at its 10,000-case default without the detector.
go test -count=1 -run '^TestGeneratedWorkflowPropertiesFullLibrary$' ./engine
{ cat .cache/coverage-others.out; tail -n +2 .cache/coverage-daemon.out; } > .cache/coverage.out
python3 scripts/count-tests.py .cache/test.json
go tool cover -func=.cache/coverage.out | tail -1
# FAE-VER-006: a planted hostile environment (every Fae variable the sources
# name set to a wrong value) must not change outcomes or leave residue. The
# non-daemon packages run in full; the daemon subset covers its subprocess
# helpers, the scripted, live and UI daemon paths and sanitized gate runs.
hostile_daemon='^(TestCrashMatrixHelper|TestEffectCrashHelper|TestLeaseRecoveryDaemonHelper|TestDaemonOrphanSubprocess|TestCrashTurnHelper|TestExternalCrashHelper|TestDaemonScriptedLifecycle|TestDaemonLiveEffectsLifecycle|TestUIChildLifecycle|TestRunGatedReturnsOwnExitCodeWithSanitizedEnvironment|TestEffectSIGKILLBoundariesAndAuditedAdoption|TestLiveHostCommandShimPolicyRefusals|TestDaemonClaudeProviderPath|TestDaemonGeneratedWorkflows)$'
python3 scripts/test-hygiene.py hostile -- go test -timeout=30m -count=1 $others
python3 scripts/test-hygiene.py hostile -- go test -timeout=30m -count=1 -run "$hostile_daemon" ./daemon
# A direct import census supplements Go's built-in dependency-cycle rejection.
for pkg in ./domain ./workflow; do
 imports=$(go list -f '{{join .Imports "\n"}}' "$pkg")
 if printf '%s\n' "$imports" | grep -Eq '^(os|os/exec|net|net/.*|syscall)$'; then
  printf 'forbidden I/O import in %s\n' "$pkg" >&2
  exit 1
 fi
done
# The operator UI ships as the separate fae-ui binary, never in the core
# (FAE-GEN-010 non-goal 6).
if go list -deps ./cmd/fae | grep -qx 'github.com/xleelz/fae/ui/server'; then
 printf 'the core fae binary imports the operator UI server\n' >&2
 exit 1
fi
# $TMPDIR, the /var/tmp and /tmp fixture roots created during the run, and
# processes still carrying this run's TMPDIR must all be gone.
python3 scripts/test-hygiene.py check .cache/fixture-roots.json
