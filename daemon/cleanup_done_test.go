package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"github.com/xleelz/fae/worktree"
)

// FAE-CTL-042, FAE-CP-013 for finished work (Dolt, git): `fae cleanup done`
// archives and removes a Done task's worktree and branch once its own
// landing is in the integration ref, with the guarantees cleanup of Retired
// tasks has: dry run by default, a verified plane backup holding the task,
// an archive ref keeping the branch tip, verified before anything goes.
// Before, cleanup covered Retired tasks only and only Escalated tasks
// retire, so a Done task's worktree and branch stayed forever (86 MB on the
// wave-8 self-host instance). A Done task whose landing is not in the ref is
// listed as skipped and kept.
func TestCleanupDoneRemovesAnIntegratedTasksWorktreeAndBranch(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { d.Close(context.Background()) }()
	authorize(t, d)
	m := &worktree.Manager{Repository: cfg.Workspace, Root: d.layout.Worktrees}
	admitDone := func(task string, land bool) (worktree.Binding, string) {
		return admitLandedDoneTask(t, ctx, d, cfg, m, task, land)
	}
	landed, sha := admitDone("landed-task", true)
	unlanded, _ := admitDone("unlanded-task", false)
	cleanup := func(params map[string]any) ([]map[string]any, error) {
		params["done"] = true
		reply, err := call(t, d, "CleanupRetired", params, "")
		if err != nil {
			return nil, err
		}
		var items []map[string]any
		if err = json.Unmarshal(reply.Result, &items); err != nil {
			t.Fatal(err)
		}
		return items, nil
	}
	byTask := func(items []map[string]any) map[string]map[string]any {
		out := map[string]map[string]any{}
		for _, item := range items {
			out[item["task"].(string)] = item
		}
		return out
	}
	items, err := cleanup(map[string]any{})
	if err != nil {
		t.Fatal("dry run:", err)
	}
	plan := byTask(items)
	if len(items) != 2 || plan["landed-task"]["skipped"] != nil || plan["landed-task"]["removed"] != false || plan["landed-task"]["landing"] != sha {
		t.Fatalf("dry run %v", items)
	}
	if skipped, _ := plan["unlanded-task"]["skipped"].(string); !strings.Contains(skipped, "is not in "+cfg.Live.IntegrationRef) {
		t.Fatalf("an unlanded Done task is not skipped: %v", plan["unlanded-task"])
	}
	if _, err = cleanup(map[string]any{"apply": true, "authorizing_message": "message"}); err == nil {
		t.Fatal("apply without a verified plane backup accepted")
	}
	if _, err = os.Stat(landed.Path); err != nil {
		t.Fatal("worktree removed without a backup", err)
	}
	reply, err := call(t, d, "PlaneBackup", map[string]any{"authorizing_message": "message"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var backup struct{ Path string }
	if err = json.Unmarshal(reply.Result, &backup); err != nil {
		t.Fatal(err)
	}
	if items, err = cleanup(map[string]any{"apply": true, "backup": backup.Path, "authorizing_message": "message"}); err != nil {
		t.Fatal(err)
	}
	if done := byTask(items); done["landed-task"]["removed"] != true || done["unlanded-task"]["removed"] != false {
		t.Fatalf("apply %v", items)
	}
	if _, err = os.Lstat(landed.Path); !os.IsNotExist(err) {
		t.Fatal("the integrated Done task's worktree stayed", err)
	}
	if err = exec.Command("git", "-C", cfg.Workspace, "rev-parse", "--verify", "--quiet", "refs/heads/"+landed.Branch).Run(); err == nil {
		t.Fatal("the integrated Done task's branch stayed")
	}
	if archive := liveGit(t, cfg.Workspace, "for-each-ref", "--format=%(objectname)", "refs/fae/archive/"+worktree.Identity("done-run", "landed-task")+"/"); archive != sha {
		t.Fatalf("archive ref %q, want the branch tip %s", archive, sha)
	}
	if _, err = os.Stat(unlanded.Path); err != nil {
		t.Fatal("the unlanded Done task's worktree was removed", err)
	}
	liveGit(t, cfg.Workspace, "rev-parse", "--verify", "refs/heads/"+unlanded.Branch)
}

// admitLandedDoneTask admits a Done live task with its own worktree, branch
// and one commit, and an Integrate artifact naming that commit as its
// landing; with land, the commit is made on the integration ref's tip and
// becomes it, so successive landings chain.
func admitLandedDoneTask(t *testing.T, ctx context.Context, d *Daemon, cfg Config, m *worktree.Manager, task string, land bool) (worktree.Binding, string) {
	t.Helper()
	base := "HEAD"
	if land {
		base = cfg.Live.IntegrationRef
	}
	binding, err := m.Ensure(ctx, "done-run", task, base)
	if err != nil {
		t.Fatal(err)
	}
	liveGit(t, binding.Path, "commit", "--allow-empty", "-m", task+" work")
	sha := liveGit(t, binding.Path, "rev-parse", "HEAD")
	if land {
		liveGit(t, cfg.Workspace, "update-ref", cfg.Live.IntegrationRef, sha)
	}
	job, err := engine.NewLive(task, 1, defaults.Workflow(), liveGit(t, binding.Path, "rev-parse", "HEAD^{tree}"))
	if err != nil {
		t.Fatal(err)
	}
	job.State = domain.Done
	fields := map[string]any{"landed": true, "sha": sha}
	raw, _ := json.Marshal(fields)
	identity, _ := domain.ContentHash(raw)
	job.Evidence = append(job.Evidence, workflow.Evidence{ID: identity, Type: "Integration", Version: job.Document.Artifacts["Integration"].Version, Stage: "integrate", TaskID: task, Tree: job.Tree, Session: "fixture", Role: "host", Provenance: true, Fields: fields})
	job.Provenance = append(job.Provenance, engine.Provenance{ArtifactID: identity, ExecutionKind: "live", TaskID: task, Primitive: workflow.Integrate, Stage: "integrate", Tree: job.Tree, Session: "fixture", Role: "host", DispatchID: "fixture", ProducedAt: time.Now().UTC()})
	payload, _ := json.Marshal(envelope{Job: job, Admission: AdmitParams{ID: task, RunID: "done-run"}})
	if _, err = d.store.AdmitJob(ctx, controlplane.Audit{Actor: "operator", Action: "test done", Outcome: "ok"}, "cleanup-"+task, controlplane.JobSnapshot{ID: task, WorkflowID: defaults.Workflow().ID, WorkflowRevision: 1, TargetID: cfg.Workspace, TargetRevision: 1, State: "Done", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	return binding, sha
}

// plantedFinishedRows is how many finished rows the paging test plants: 20
// pages of the test's page size. (Planting 100,001, past the old listing
// bound, took 55 s on this host; the bound's removal is the paging itself.)
const plantedFinishedRows = 2000

// plantFinishedRows writes n Done job and task rows of a non-live execution
// kind (and every tenth row with a NULL execution_kind, as a pre-schema-11 or
// un-backfilled row with no git/worktree artifacts has) with no snapshot
// behind them (an unknown payload digest), so any read of their snapshots
// fails: the rows of finished tasks cleanup must pass over without reading
// their snapshots.
func plantFinishedRows(t *testing.T, d *Daemon, n int) {
	t.Helper()
	ctx := context.Background()
	digest := strings.Repeat("0", 64)
	for start := 0; start < n; start += 500 {
		end := min(start+500, n)
		err := d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "plant finished rows", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
			now := time.Now().UTC()
			jobs, jobArgs, tasks, taskArgs := []string{}, []any{}, []string{}, []any{}
			for i := start; i < end; i++ {
				id := fmt.Sprintf("planted-%06d", i)
				var kind any = "scripted"
				if i%10 == 0 {
					kind = nil
				}
				jobs, jobArgs = append(jobs, "(?,?,?,?,?,?,?,?,?,?)"), append(jobArgs, id, 3, "wf", 1, "target", 1, "Done", digest, 1, "")
				tasks, taskArgs = append(tasks, "(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)"), append(taskArgs, id, "planted-run", "", "target", 1, "Done", 1, 20, 10, "", "", kind, "normal", now, now)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO jobs (job_id,version,workflow_id,workflow_revision,target_id,target_revision,state,payload_digest,payload_size,molecule_id) VALUES `+strings.Join(jobs, ","), jobArgs...); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO tasks (task_id,run_id,molecule_id,target_id,target_revision,state,attempt,attempt_limit,priority,disposition,escalation_reason,execution_kind,mode,created_at,updated_at) VALUES `+strings.Join(tasks, ","), taskArgs...)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// FAE-CP-009, FAE-CTL-042 for `fae cleanup done` (Dolt, git): Done tasks are
// never removed from the plane, so the candidate scan pages their rows
// through the state index and checks git/worktree state before reading a
// snapshot: a finished task that was not live (or has NULL execution_kind and
// no git/worktree artifacts), or one already cleaned (worktree and branch
// removed with verified archive ref in git), is passed over without reading
// its snapshot. Skipped Done tasks (unintegrated or dirty worktree) do not
// count toward `--limit`, so they never starve later cleanable tasks, and
// cleanup writes nothing to the plane (no events, audit rows or commits).
func TestCleanupDonePagesItsCandidatesAndSkipsWhatItCleaned(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { d.Close(context.Background()) }()
	authorize(t, d)
	page := cleanupScanPage
	cleanupScanPage = 100
	t.Cleanup(func() { cleanupScanPage = page })
	m := &worktree.Manager{Repository: cfg.Workspace, Root: d.layout.Worktrees}
	// Two permanently skipped Done tasks early in id order (one unlanded, one
	// with a dirty worktree), followed by three integrated cleanable live
	// tasks spread among the planted rows so the scan crosses pages.
	admitLandedDoneTask(t, ctx, d, cfg, m, "a-00-unlanded", false)
	dirtyBinding, _ := admitLandedDoneTask(t, ctx, d, cfg, m, "a-01-dirty", true)
	if err = os.WriteFile(dirtyBinding.Path+"/untracked.txt", []byte("dirty"), 0600); err != nil {
		t.Fatal(err)
	}
	live := []string{"a-first", "planted-001000-live", "zz-last"}
	for _, task := range live {
		admitLandedDoneTask(t, ctx, d, cfg, m, task, true)
	}
	plantFinishedRows(t, d, plantedFinishedRows)
	cleanup := func(params map[string]any) []map[string]any {
		t.Helper()
		params["done"] = true
		reply, err := call(t, d, "CleanupRetired", params, "")
		if err != nil {
			t.Fatal(err)
		}
		var items []map[string]any
		if err = json.Unmarshal(reply.Result, &items); err != nil {
			t.Fatal(err)
		}
		return items
	}
	tasks := func(items []map[string]any) (out []string) {
		for _, item := range items {
			out = append(out, item["task"].(string))
		}
		return out
	}
	all := append([]string{"a-00-unlanded", "a-01-dirty"}, live...)
	if got := tasks(cleanup(map[string]any{})); strings.Join(got, ",") != strings.Join(all, ",") {
		t.Fatalf("dry run over %d planted rows planned %v, want %v", plantedFinishedRows, got, all)
	}
	items := cleanup(map[string]any{"limit": 2})
	if got := tasks(items); strings.Join(got, ",") != "a-00-unlanded,a-01-dirty,a-first,planted-001000-live," || !strings.Contains(items[4]["skipped"].(string), "remain beyond this run's limit of 2") {
		t.Fatalf("limit 2: %v", items)
	}
	reply, err := call(t, d, "PlaneBackup", map[string]any{"authorizing_message": "message"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var backup struct{ Path string }
	if err = json.Unmarshal(reply.Result, &backup); err != nil {
		t.Fatal(err)
	}
	eventsBefore, auditsBefore, commitsBefore := planeCounts(t, d)
	apply := map[string]any{"apply": true, "backup": backup.Path, "authorizing_message": "message", "limit": 2}
	items = cleanup(apply)
	if got := tasks(items); strings.Join(got, ",") != "a-00-unlanded,a-01-dirty,a-first,planted-001000-live," || items[0]["removed"] != false || items[1]["removed"] != false || items[2]["removed"] != true || items[3]["removed"] != true {
		t.Fatalf("first apply: %v", items)
	}
	// The next run passes over what the first cleaned via git archive state
	// and finishes the remaining cleanable task; the run after that reports
	// only the two skipped tasks, with no "remain beyond this run's limit".
	items = cleanup(apply)
	if got := tasks(items); strings.Join(got, ",") != "a-00-unlanded,a-01-dirty,zz-last" || items[2]["removed"] != true {
		t.Fatalf("second apply: %v", items)
	}
	if items = cleanup(apply); strings.Join(tasks(items), ",") != "a-00-unlanded,a-01-dirty" {
		t.Fatalf("third apply planned cleaned tasks again or reported more remaining: %v", items)
	}
	if gotEvents, gotAudits, gotCommits := planeCounts(t, d); gotEvents != eventsBefore || gotAudits != auditsBefore || gotCommits != commitsBefore {
		t.Fatalf("cleanup done wrote to the plane: events %d->%d, audits %d->%d, commits %d->%d", eventsBefore, gotEvents, auditsBefore, gotAudits, commitsBefore, gotCommits)
	}
}
