#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
root=$PWD
export GOCACHE="$root/.cache/go-build" TMPDIR="$root/.cache/tmp"
export PATH="$root/.tools/spin/Src:$PATH"
if [[ -d /opt/homebrew/opt/openjdk@17 ]]; then
 export PATH="/opt/homebrew/opt/openjdk@17/bin:/opt/homebrew/opt/coreutils/libexec/gnubin:$PATH"
fi
export Z3_EXE=${Z3_EXE:-$(command -v z3)}
mkdir -p "$GOCACHE" "$TMPDIR" verification/logs .tools/projects
jar="$root/.tools/gobra/target/scala-2.13/gobra.jar"
[[ -f $jar && -x .tools/gomela-pinned-bin ]] || { echo 'Run scripts/install-verifiers.sh first' >&2; exit 1; }
java -Xss128m -jar "$jar" -i scheduler/capacity.go > verification/logs/gobra-capacity.log 2>&1
grep -q 'Gobra found 0 errors' verification/logs/gobra-capacity.log
# Derive the firing control directly from the verified production source.
sed 's/return weight > 0 \&\& weight <= capacity-used/return weight > 0/' scheduler/capacity.go > .cache/capacity-negative.go
if java -Xss128m -jar "$jar" -i .cache/capacity-negative.go > verification/logs/gobra-negative.log 2>&1; then
 echo 'Gobra failed to reject the weakened capacity predicate' >&2; exit 1
fi
grep -q 'Postcondition might not hold' verification/logs/gobra-negative.log
# Verify the actual durable-engine completion guard and reject stale-token bypass.
java -Xss128m -jar "$jar" -i kernel/completion.go > verification/logs/gobra-completion.log 2>&1
grep -q 'Gobra found 0 errors' verification/logs/gobra-completion.log
sed 's/return !terminal \&\& pending \&\& tokenMatches \&\& evidenceValid/return !terminal \&\& pending \&\& evidenceValid/' kernel/completion.go > .cache/completion-negative.go
if java -Xss128m -jar "$jar" -i .cache/completion-negative.go > verification/logs/gobra-completion-negative.log 2>&1; then
 echo 'Gobra failed to reject stale completion token bypass' >&2; exit 1
fi
grep -q 'Postcondition might not hold' verification/logs/gobra-completion-negative.log
# Verify the actual production owner identity guard and a PID-reuse bypass.
java -Xss128m -jar "$jar" -i scheduler/release.go > verification/logs/gobra-release.log 2>&1
grep -q 'Gobra found 0 errors' verification/logs/gobra-release.log
sed 's/return pid > 0 \&\& started > 0 \&\& pid == holderPID \&\& started == holderStarted/return pid > 0 \&\& started > 0 \&\& pid == holderPID/' scheduler/release.go > .cache/release-negative.go
if java -Xss128m -jar "$jar" -i .cache/release-negative.go > verification/logs/gobra-release-negative.log 2>&1; then
 echo 'Gobra failed to reject PID reuse release bypass' >&2; exit 1
fi
grep -q 'Postcondition might not hold' verification/logs/gobra-release-negative.log
# Verify exact ownership conjunction used before orphan signals.
java -Xss128m -jar "$jar" -i processowner/guard.go > verification/logs/gobra-orphan.log 2>&1
grep -q 'Gobra found 0 errors' verification/logs/gobra-orphan.log
sed 's/return ownerMatches \&\& identityMatches \&\& groupMatches \&\& privateRecord/return ownerMatches \&\& groupMatches \&\& privateRecord/' processowner/guard.go > .cache/orphan-negative.go
if java -Xss128m -jar "$jar" -i .cache/orphan-negative.go > verification/logs/gobra-orphan-negative.log 2>&1; then
 echo 'Gobra failed to reject reused process identity' >&2; exit 1
fi
grep -q 'Postcondition might not hold' verification/logs/gobra-orphan-negative.log
java -Xss128m -jar "$jar" -i kernel/reconcile.go > verification/logs/gobra-reconcile.log 2>&1
grep -q 'Gobra found 0 errors' verification/logs/gobra-reconcile.log
sed 's/return tipMatches \&\& knownHead \&\& knownTree \&\& evidenceValid/return tipMatches \&\& knownHead \&\& evidenceValid/' kernel/reconcile.go > .cache/reconcile-negative.go
if java -Xss128m -jar "$jar" -i .cache/reconcile-negative.go > verification/logs/gobra-reconcile-negative.log 2>&1; then
 echo 'Gobra failed to reject unchecked reconcile tree' >&2; exit 1
fi
grep -q 'Postcondition might not hold' verification/logs/gobra-reconcile-negative.log
# Drain must account for durable pending dispatches, not only running workers.
java -Xss128m -jar "$jar" -i kernel/drain.go > verification/logs/gobra-drain.log 2>&1
grep -q 'Gobra found 0 errors' verification/logs/gobra-drain.log
sed 's/return draining \&\& active == 0 \&\& pending == 0/return draining \&\& active == 0/' kernel/drain.go > .cache/drain-negative.go
if java -Xss128m -jar "$jar" -i .cache/drain-negative.go > verification/logs/gobra-drain-negative.log 2>&1; then
 echo 'Gobra failed to reject restart before durable collection' >&2; exit 1
fi
grep -q 'Postcondition might not hold' verification/logs/gobra-drain-negative.log
java -Xss128m -jar "$jar" -i kernel/conflict.go > verification/logs/gobra-conflict.log 2>&1
grep -q 'Gobra found 0 errors' verification/logs/gobra-conflict.log
sed 's/return additionsRetained \&\& deletionsRetained/return deletionsRetained/' kernel/conflict.go > .cache/conflict-negative.go
if java -Xss128m -jar "$jar" -i .cache/conflict-negative.go > verification/logs/gobra-conflict-negative.log 2>&1; then
 echo 'Gobra failed to reject weakened conflict predicate' >&2; exit 1
fi
grep -q 'Postcondition might not hold' verification/logs/gobra-conflict-negative.log
java -Xss128m -jar "$jar" -i scheduler/recovery.go > verification/logs/gobra-recovery.log 2>&1
grep -q 'Gobra found 0 errors' verification/logs/gobra-recovery.log
sed 's/return ownerDead \&\& linkageMatches \&\& (neverLaunched || retired)/return linkageMatches \&\& (neverLaunched || retired)/' scheduler/recovery.go > .cache/recovery-negative.go
if java -Xss128m -jar "$jar" -i .cache/recovery-negative.go > verification/logs/gobra-recovery-negative.log 2>&1; then
 echo 'Gobra failed to reject weakened recovery predicate' >&2; exit 1
fi
grep -q 'Postcondition might not hold' verification/logs/gobra-recovery-negative.log
java -Xss128m -jar "$jar" -i kernel/integration.go > verification/logs/gobra-integration.log 2>&1
grep -q 'Gobra found 0 errors' verification/logs/gobra-integration.log
sed 's/return noWrite \&\& tipAdvanced \&\& ownCommitKnown \&\& boundedRoute/return tipAdvanced \&\& ownCommitKnown \&\& boundedRoute/' kernel/integration.go > .cache/integration-negative.go
if java -Xss128m -jar "$jar" -i .cache/integration-negative.go > verification/logs/gobra-integration-negative.log 2>&1; then
 echo 'Gobra failed to reject retry with unknown write outcome' >&2; exit 1
fi
grep -q 'Postcondition might not hold' verification/logs/gobra-integration-negative.log
# Additional production guards: exact source mutations must match once.
verify_guard() {
 local name=$1 source=$2 original=$3 weakened=$4
 java -Xss128m -jar "$jar" -i "$source" > "verification/logs/gobra-$name.log" 2>&1
 grep -q 'Gobra found 0 errors' "verification/logs/gobra-$name.log"
 python3 - "$source" ".cache/$name-negative.go" "$original" "$weakened" <<'PYMUTATE'
import pathlib,sys
source,target,original,weakened=sys.argv[1:]
text=pathlib.Path(source).read_text()
assert text.count(original)==1, (source,original)
pathlib.Path(target).write_text(text.replace(original,weakened))
PYMUTATE
 if java -Xss128m -jar "$jar" -i ".cache/$name-negative.go" > "verification/logs/gobra-$name-negative.log" 2>&1; then
  echo "Gobra failed to reject weakened $name guard" >&2; exit 1
 fi
 grep -q 'Postcondition might not hold' "verification/logs/gobra-$name-negative.log"
}
verify_guard provider providerhealth/eligibility.go 'return score >= 50000 && score <= 100000' 'return score <= 100000'
verify_guard shim commandshim/guard.go 'return roleAllowed && scopeValid && outputSafe && leaseHeld && !nested' 'return roleAllowed && scopeValid && outputSafe && !nested'
verify_guard pane scheduler/pane_guard.go 'return live < limit && pending < limit-live' 'return live < limit'
verify_guard dynamic-pane scheduler/dynamic_panes.go 'return current' 'return requested'
verify_guard idle kernel/idle.go 'return global && preCode && !protected && !active && inflight == 0' 'return global && preCode && !protected && inflight == 0'
verify_guard stall kernel/stall.go 'return restrictedContract && retired && unchanged && firstSession' 'return restrictedContract && unchanged && firstSession'
verify_guard provider-session kernel/provider_session.go 'return enabled && scopedPolicy && !fresh' 'return enabled && scopedPolicy'
verify_guard dynamic-host scheduler/dynamic_capacity.go 'return used <= limit && weight > 0 && weight <= limit-used' 'return weight > 0'
verify_guard startup kernel/startup.go 'return notExecuted && retired && firstSession' 'return notExecuted && firstSession'
verify_guard loop loops/admission.go 'return active && !inFlight && beforeExpiry && due && belowMaximum' 'return active && beforeExpiry && due && belowMaximum'
verify_guard uncertain kernel/uncertain.go 'return authorized && uncertain && matching && retired && unchanged' 'return authorized && uncertain && matching && unchanged'
verify_guard resident runtime/resident_guard.go 'return identityMatches && live && !fresh' 'return identityMatches && !fresh'
verify_guard cron cron/admission.go 'return active && due && beforeEnd && pinned' 'return active && due && pinned'
verify_guard controller controller/guard.go 'return clearEffects && (!whenClear || clearQA) && (clearOverlap || acknowledged)' 'return (!whenClear || clearQA) && (clearOverlap || acknowledged)'
verify_guard effect kernel/effect.go 'return authorized && retired && exactPostcondition && trustedReceipt' 'return authorized && retired && exactPostcondition'
verify_guard deployment controller/deployment_guard.go 'return acknowledged || (expired && !criticalTask && !topNearLimit && !queryFailed)' 'return acknowledged || (expired && !topNearLimit && !queryFailed)'
verify_guard drive kernel/drive_rearm.go 'return !alive && !pending && budget && running' 'return !alive && budget && running'
verify_guard external external/adoption.go 'return identity && pins && named && performed' 'return identity && pins && named'
verify_guard schema external/schema_guard.go 'return declared' 'return true'
verify_guard review external/review_guard.go 'return requiredPass && commentsClear && knownStatus' 'return requiredPass && knownStatus'
verify_guard ticket external/ticket_guard.go 'return !started && !emitted && ordered && live' 'return !emitted && ordered && live'
verify_guard authority daemon/authority_guard.go 'return authenticated && known && allowed' 'return known && allowed'
verify_guard control engine/control_guard.go 'return !terminal && (!inFlight || allowsInFlight)' 'return !inFlight || allowsInFlight'
verify_guard readstream daemon/read_guard.go 'return after >= floor' 'return true'
verify_guard prophet daemon/prophet_guard.go 'return directed && supported' 'return supported'
verify_guard backlog daemon/backlog_guard.go 'return !blocking || prerequisiteDone' 'return true'
verify_guard restart daemon/restart_guard.go 'return headKnown && treeKnown' 'return headKnown'
verify_guard retirement daemon/retirement_guard.go 'return resolves && owned' 'return resolves'
verify_guard target daemon/target_guard.go 'return bound && allowed' 'return bound'
verify_guard prophet-route daemon/prophet_route_guard.go 'return observed && retired && withinBudget' 'return retired && withinBudget'
verify_guard normal-target daemon/normal_target_guard.go 'return installed && matches' 'return installed'
verify_guard census daemon/census_guard.go 'return versionMatches && digestMatches' 'return versionMatches'
verify_guard cleanup worktree/cleanup_guard.go 'return archiveMatches && backupVerified' 'return archiveMatches'
verify_guard adapter daemon/adapter_guard.go 'return revisionMatches && hashMatches' 'return revisionMatches'
verify_guard review-workspace daemon/review_workspace_guard.go 'return approvedPackage && exactBinding' 'return approvedPackage'
verify_guard scratch daemon/scratch_guard.go 'return marked && backedUp' 'return marked'
verify_guard index effects/index_guard.go 'return trustedIndex && expectedHead' 'return expectedHead'
verify_guard adapter-retirement controlplane/adapter_retirement_guard.go 'return !retired' 'return true'
verify_guard fleet-provision daemon/fleet_provision_guard.go 'return attemptRecorded && receiptMatched && baselineGreen' 'return attemptRecorded && baselineGreen'
verify_guard backup-retention daemon/backup_retention_guard.go 'return verified && !protected && planMatches' 'return verified && planMatches'
verify_guard backup-replication daemon/backup_replication_guard.go 'return identityMatched && contentMatched' 'return identityMatched'
verify_guard retention-schedule daemon/retention_schedule_guard.go 'return active && due && authorized' 'return active && due'
verify_guard scratch-archive daemon/scratch_archive_guard.go 'return completedJournal && exactSurvivor && unreferencedSource' 'return completedJournal && exactSurvivor'
verify_guard backup-schedule daemon/backup_schedule_guard.go 'return active && due && authorized' 'return active && due'
verify_guard replication-schedule daemon/replication_schedule_guard.go 'return active && due && authorized' 'return active && due'
verify_guard backup-quota daemon/backup_quota_guard.go 'return bytesFit && countFit' 'return bytesFit'
verify_guard replication-unpin daemon/replication_unpin_guard.go 'return allConfirmed && remoteVerified && noPendingClaim' 'return allConfirmed && remoteVerified'
# Isolated fresh work directory prevents stale models from satisfying checks.
run_dir=$(mktemp -d "$root/.tools/gomela-pinned/fae-run.XXXXXX")
trap 'rm -rf "$run_dir"' EXIT
mkdir -p "$run_dir/source" "$run_dir/negative" "$run_dir/positive-result" "$run_dir/negative-result"
for model in provider shim panes dynamic_panes provider_session dynamic_host startup loop uncertain cron effect deployment drive external schema review ticket authority control readstream prophet backlog restart retirement target prophet_route normal_target census cleanup adapter review_workspace scratch index adapter_retirement fleet_provision backup_retention backup_replication retention_schedule scratch_archive backup_schedule replication_schedule backup_unpin backup_quota; do
 cp "verification/gomela/$model.go" "$run_dir/source/$model.go"
 cp "verification/negative/$model.go.txt" "$run_dir/negative/$model.go"
done
cp verification/gomela/controller.go "$run_dir/source/controller.go"
cp verification/negative/controller_missing_acceptance.go.txt "$run_dir/negative/controller.go"
cp verification/gomela/resident.go "$run_dir/source/resident.go"
cp verification/negative/resident_missing_retirement.go.txt "$run_dir/negative/resident.go"
cp verification/gomela/idle.go "$run_dir/source/idle.go"
cp verification/negative/idle_missing_handoff.go.txt "$run_dir/negative/idle.go"
cp verification/gomela/stall.go "$run_dir/source/stall.go"
cp verification/negative/stall.go.txt "$run_dir/negative/stall.go"
cp verification/gomela/recovery.go "$run_dir/source/recovery.go"
cp verification/negative/recovery.go.txt "$run_dir/negative/recovery.go"
cp verification/gomela/conflict.go "$run_dir/source/conflict.go"
cp verification/negative/conflict.go.txt "$run_dir/negative/conflict.go"
cp verification/gomela/drain.go "$run_dir/source/drain.go"
cp verification/negative/drain.go.txt "$run_dir/negative/drain.go"
cp verification/gomela/completion.go "$run_dir/source/completion.go"
cp verification/gomela/daemon.go "$run_dir/source/daemon.go"
cp verification/gomela/lease.go "$run_dir/source/lease.go"
cp verification/gomela/orphan.go "$run_dir/source/orphan.go"
cp verification/gomela/multiplexer.go "$run_dir/source/multiplexer.go"
cp verification/negative/deadlock.go.txt "$run_dir/negative/deadlock.go"
cp verification/negative/daemon.go.txt "$run_dir/negative/daemon.go"
cp verification/negative/lease.go.txt "$run_dir/negative/lease.go"
cp verification/negative/orphan.go.txt "$run_dir/negative/orphan.go"
cp verification/negative/multiplexer.go.txt "$run_dir/negative/multiplexer.go"
cp .tools/gomela-pinned/config.yaml "$run_dir/config.yaml"
# The pinned upstream tool expects a projects directory above its cwd.
mkdir -p .tools/gomela-pinned/projects
(cd "$run_dir"; "$root/.tools/gomela-pinned-bin" -result_folder positive-result fs source) > verification/logs/gomela-positive.log 2>&1
(cd "$run_dir"; "$root/.tools/gomela-pinned-bin" -result_folder negative-result fs negative) > verification/logs/gomela-negative.log 2>&1
mkdir -p verification/results/models
cp "$run_dir"/positive-result/source/*.pml verification/results/models/
cp "$run_dir"/negative-result/negative/*.pml verification/results/models/
cp "$run_dir/positive-result/verification.csv" verification/results/gomela-positive.csv
cp "$run_dir/negative-result/verification.csv" verification/results/gomela-negative.csv
python3 - <<'PY'
import csv
positive=list(csv.reader(open('verification/results/gomela-positive.csv')))
negative=list(csv.reader(open('verification/results/gomela-negative.csv')))
assert len(positive)==57, positive
assert all(int(r[2])>0 and r[4:10]==['false']*6 and not r[10] for r in positive),positive
assert len(negative)==55 and all(int(r[2])>0 and r[8]=='true' for r in negative),negative
PY
python3 - <<'PYHASH'
import hashlib,json,pathlib
files=['daemon/replication_unpin_guard.go','daemon/backup_quota_guard.go','verification/gomela/backup_unpin.go','verification/negative/backup_unpin.go.txt','verification/gomela/backup_quota.go','verification/negative/backup_quota.go.txt','daemon/backup_schedule_guard.go','daemon/replication_schedule_guard.go','verification/gomela/backup_schedule.go','verification/negative/backup_schedule.go.txt','verification/gomela/replication_schedule.go','verification/negative/replication_schedule.go.txt','daemon/retention_schedule_guard.go','daemon/scratch_archive_guard.go','verification/gomela/retention_schedule.go','verification/negative/retention_schedule.go.txt','verification/gomela/scratch_archive.go','verification/negative/scratch_archive.go.txt','daemon/backup_retention_guard.go','daemon/backup_replication_guard.go','verification/gomela/backup_retention.go','verification/negative/backup_retention.go.txt','verification/gomela/backup_replication.go','verification/negative/backup_replication.go.txt','controlplane/adapter_retirement_guard.go','daemon/fleet_provision_guard.go','verification/gomela/adapter_retirement.go','verification/negative/adapter_retirement.go.txt','verification/gomela/fleet_provision.go','verification/negative/fleet_provision.go.txt','effects/index_guard.go','verification/gomela/index.go','verification/negative/index.go.txt','daemon/adapter_guard.go','daemon/review_workspace_guard.go','daemon/scratch_guard.go','verification/gomela/adapter.go','verification/negative/adapter.go.txt','verification/gomela/review_workspace.go','verification/negative/review_workspace.go.txt','verification/gomela/scratch.go','verification/negative/scratch.go.txt','daemon/normal_target_guard.go','daemon/census_guard.go','worktree/cleanup_guard.go','verification/gomela/normal_target.go','verification/negative/normal_target.go.txt','verification/gomela/census.go','verification/negative/census.go.txt','verification/gomela/cleanup.go','verification/negative/cleanup.go.txt','daemon/retirement_guard.go','daemon/target_guard.go','daemon/prophet_route_guard.go','verification/gomela/retirement.go','verification/negative/retirement.go.txt','verification/gomela/target.go','verification/negative/target.go.txt','verification/gomela/prophet_route.go','verification/negative/prophet_route.go.txt','daemon/prophet_guard.go','daemon/backlog_guard.go','daemon/restart_guard.go','verification/gomela/prophet.go','verification/negative/prophet.go.txt','verification/gomela/backlog.go','verification/negative/backlog.go.txt','verification/gomela/restart.go','verification/negative/restart.go.txt','daemon/authority_guard.go','engine/control_guard.go','daemon/read_guard.go','verification/gomela/authority.go','verification/negative/authority.go.txt','verification/gomela/control.go','verification/negative/control.go.txt','verification/gomela/readstream.go','verification/negative/readstream.go.txt','external/schema_guard.go','external/review_guard.go','external/ticket_guard.go','verification/gomela/schema.go','verification/negative/schema.go.txt','verification/gomela/review.go','verification/negative/review.go.txt','verification/gomela/ticket.go','verification/negative/ticket.go.txt','controller/deployment_guard.go','kernel/drive_rearm.go','external/adoption.go','verification/gomela/deployment.go','verification/negative/deployment.go.txt','verification/gomela/drive.go','verification/negative/drive.go.txt','verification/gomela/external.go','verification/negative/external.go.txt','controller/guard.go','verification/gomela/controller.go','verification/negative/controller_missing_acceptance.go.txt','kernel/effect.go','verification/gomela/effect.go','verification/negative/effect.go.txt','cron/admission.go','verification/gomela/cron.go','verification/negative/cron.go.txt','loops/admission.go','kernel/uncertain.go','runtime/resident_guard.go','verification/gomela/loop.go','verification/negative/loop.go.txt','verification/gomela/uncertain.go','verification/negative/uncertain.go.txt','verification/gomela/resident.go','verification/negative/resident_missing_retirement.go.txt','kernel/provider_session.go','scheduler/dynamic_capacity.go','kernel/startup.go','verification/gomela/provider_session.go','verification/negative/provider_session.go.txt','verification/gomela/dynamic_host.go','verification/negative/dynamic_host.go.txt','verification/gomela/startup.go','verification/negative/startup.go.txt','scheduler/dynamic_panes.go','kernel/idle.go','kernel/stall.go','verification/gomela/dynamic_panes.go','verification/negative/dynamic_panes.go.txt','verification/gomela/idle.go','verification/negative/idle_missing_handoff.go.txt','verification/gomela/stall.go','verification/negative/stall.go.txt','providerhealth/eligibility.go','commandshim/guard.go','scheduler/pane_guard.go','verification/gomela/provider.go','verification/negative/provider.go.txt','verification/gomela/shim.go','verification/negative/shim.go.txt','verification/gomela/panes.go','verification/negative/panes.go.txt','kernel/integration.go','verification/gomela/recovery.go','verification/negative/recovery.go.txt','verification/gomela/conflict.go','verification/negative/conflict.go.txt','kernel/conflict.go','scheduler/recovery.go','kernel/drain.go','verification/gomela/drain.go','verification/negative/drain.go.txt','verification/gomela/multiplexer.go','verification/negative/multiplexer.go.txt','processowner/guard.go','kernel/reconcile.go','verification/gomela/orphan.go','verification/negative/orphan.go.txt','scheduler/release.go','verification/gomela/lease.go','verification/negative/lease.go.txt','kernel/completion.go','scheduler/capacity.go','verification/gomela/completion.go','verification/gomela/daemon.go','verification/negative/deadlock.go.txt','verification/negative/daemon.go.txt']
pathlib.Path('verification/results/source-hashes.json').write_text(json.dumps({p:hashlib.sha256(pathlib.Path(p).read_bytes()).hexdigest() for p in files},indent=2)+'\n')
PYHASH
printf 'Gobra fifty-six production guards and Gomela fifty-seven positive/fifty-five negative bounded models passed; all firing controls rejected.\n'
