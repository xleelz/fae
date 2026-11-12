package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/providerhealth"
	agentruntime "github.com/xleelz/fae/runtime"
)

// seedFinishedCopies writes n copies of a finished task's job and task rows
// under new ids, in one transaction, as tasks finished long ago.
func seedFinishedCopies(t *testing.T, d *Daemon, source string, n int) {
	t.Helper()
	ctx := context.Background()
	err := d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "seed finished tasks", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		job := make([]any, 9)
		if err := tx.QueryRowContext(ctx, `SELECT version,workflow_id,workflow_revision,target_id,target_revision,state,payload_digest,payload_size,molecule_id FROM jobs WHERE job_id=?`, source).Scan(&job[0], &job[1], &job[2], &job[3], &job[4], &job[5], &job[6], &job[7], &job[8]); err != nil {
			return err
		}
		task := make([]any, 12)
		if err := tx.QueryRowContext(ctx, `SELECT run_id,molecule_id,target_id,target_revision,state,attempt,attempt_limit,priority,disposition,escalation_reason,execution_kind,mode FROM tasks WHERE task_id=?`, source).Scan(&task[0], &task[1], &task[2], &task[3], &task[4], &task[5], &task[6], &task[7], &task[8], &task[9], &task[10], &task[11]); err != nil {
			return err
		}
		now := time.Now().UTC()
		jobs, jobArgs, tasks, taskArgs := []string{}, []any{}, []string{}, []any{}
		for i := 0; i < n; i++ {
			id := fmt.Sprintf("finished-%04d", i)
			jobs, jobArgs = append(jobs, "(?,?,?,?,?,?,?,?,?,?)"), append(append(jobArgs, id), job...)
			tasks, taskArgs = append(tasks, "(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)"), append(append(append(taskArgs, id), task...), now, now)
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

// globalProphetScope reads the global Prophet's session page and returns its
// task list as JSON, its total and whether the list was cut.
func globalProphetScope(t *testing.T, d *Daemon) (json.RawMessage, int, bool) {
	t.Helper()
	r, err := call(t, d, "ProphetSession", ProphetParams{}, "")
	if err != nil {
		t.Fatal("global Prophet session read: ", err)
	}
	var page struct {
		Scope struct {
			Tasks     json.RawMessage `json:"tasks"`
			Total     int             `json:"tasks_total"`
			Truncated bool            `json:"tasks_truncated"`
		} `json:"scope"`
	}
	if err = json.Unmarshal(r.Result, &page); err != nil {
		t.Fatal(err)
	}
	return page.Scope.Tasks, page.Scope.Total, page.Scope.Truncated
}

// FAE-CP-009, FAE-OPS-031: the global Prophet answers on an instance of more
// than 1,000 tasks. It reads the drive's tasks from its snapshot, unfinished
// ones first, and the tasks finished before the start from their job and task
// columns, only as far as its bound of 1,000 summaries (no whole-table read),
// then from the finished-task summary once built (no plane read). It states
// the total and that the list is cut, in reads and in a turn's brief.
func TestGlobalProphetScopedPastAThousandTasks(t *testing.T) {
	cfg, ctx := shortFixture(t)
	var mu sync.Mutex
	var brief string
	cfg.ProphetRunner = func(ctx context.Context, task string, raw json.RawMessage) (json.RawMessage, error) {
		mu.Lock()
		brief = string(raw)
		mu.Unlock()
		return json.Marshal(ProphetAnswer{Reply: "summarised", Proposals: []string{}})
	}
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	authorize(t, d)
	installShort(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "seed", WorkflowID: "short", Tree: "tree", AuthorizingMessage: "message"}, "seed"); err != nil {
		stop(t, d)
		t.Fatal(err)
	}
	awaitState(t, d, "seed", "Done")
	seedFinishedCopies(t, d, "seed", 1001)
	stop(t, d)
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	stopDrive(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "zz-active", WorkflowID: "short", Tree: "tree", AuthorizingMessage: "message"}, "zz-active"); err != nil {
		t.Fatal(err)
	}
	read := d.finished.read.Load()
	tasks, total, truncated := globalProphetScope(t, d)
	var rows []struct {
		Task     string `json:"task"`
		State    string `json:"state"`
		Workflow string `json:"workflow"`
		Finished bool   `json:"finished_before_start"`
		Excerpt  string `json:"content_excerpt"`
	}
	if err = json.Unmarshal(tasks, &rows); err != nil {
		t.Fatal(err)
	}
	if total != 1003 || !truncated || len(rows) != prophetGlobalTasks {
		t.Fatalf("global scope: %d of %d tasks, truncated %t", len(rows), total, truncated)
	}
	if rows[0].Task != "zz-active" || rows[0].Finished || rows[0].State != "Pending" {
		t.Fatalf("the drive's unfinished task is not listed first: %+v", rows[0])
	}
	for i, row := range rows[1:] {
		if want := fmt.Sprintf("finished-%04d", i); row.Task != want || !row.Finished || row.State != "Done" || row.Workflow != "short" {
			t.Fatalf("finished row %d: %+v, want %s", i+1, row, want)
		}
	}
	if n := d.finished.read.Load() - read; n > prophetGlobalTasks {
		t.Fatalf("the global scope read %d finished rows for %d summaries", n, prophetGlobalTasks)
	}
	if n := d.finished.snapshots.Load(); n != 0 {
		t.Fatalf("the global scope decoded %d finished snapshots", n)
	}
	// Once the finished-task summary is built, the scope reads no plane row.
	if _, err = call(t, d, "Board", BoardParams{IncludeRetired: true}, ""); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(30 * time.Second); d.finishedSummaryBuilt() == nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("finished-task summary never built")
		}
	}
	read = d.finished.read.Load()
	if again, _, _ := globalProphetScope(t, d); string(again) != string(tasks) {
		t.Fatal("the summary changed the global scope")
	}
	if n := d.finished.read.Load() - read; n != 0 {
		t.Fatalf("the global scope read %d plane rows with the summary built", n)
	}
	// A global turn starts, and its brief states the total and the cut.
	id := prophetSend(t, d, ProphetParams{Text: "summarise the instance"})
	if reply := prophetAwait(t, d, "", id); reply.Text != "summarised" {
		t.Fatal(reply)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(brief, `"tasks_total":1003`) || !strings.Contains(brief, `"tasks_truncated":true`) {
		t.Fatalf("the global brief lacks its total and cut: %.400s", brief)
	}
}

// FAE-ROB-020/-021: a Prophet turn's start reserves its scope under the
// writer lock and makes its plane reads and writes without it. While its
// first read is slowed, an admission and a task control return in under 1 s,
// a second turn of the same scope is refused as busy, and the slowed turn
// still starts and is answered.
func TestProphetSessionStartDoesNotHoldTheWriterLock(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.ProphetRunner = func(ctx context.Context, task string, raw json.RawMessage) (json.RawMessage, error) {
		return json.Marshal(ProphetAnswer{Reply: "answered", Proposals: []string{}})
	}
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	stopDrive(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "alpha", Content: "alpha", Tree: "tree", AuthorizingMessage: "message"}, "alpha"); err != nil {
		t.Fatal(err)
	}
	started := slowPlaneRead(d, "prophet/alpha", 3*time.Second)
	slowed := slowedCall(t, d, started, "ProphetChat", ProphetParams{Task: "alpha", Text: "describe"}, "")
	writesDuring(t, d, slowed, "fast")
	begun := time.Now()
	if _, err = callOnce(t, d, "ProphetChat", ProphetParams{Task: "alpha", Text: "again"}, ""); err == nil || !strings.Contains(err.Error(), "busy") || time.Since(begun) >= time.Second {
		t.Fatalf("a second turn of the scope was not refused at once: %s: %v", time.Since(begun), err)
	}
	var queued struct {
		ID string `json:"id"`
	}
	if err = json.Unmarshal(slowedResult(t, slowed), &queued); err != nil {
		t.Fatal(err)
	}
	if reply := prophetAwait(t, d, "alpha", queued.ID); reply.Text != "answered" {
		t.Fatal(reply)
	}
}

// FAE-ROB-021: a Prophet route's provider observation is one plane
// transaction made without the writer lock. While the test holds d.mu, as a
// writer stuck in I/O does, a Prophet run still records its route's outcome
// and returns.
func TestProphetRouteObservationTakesNoWriterLock(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	stopDrive(t, d)
	// The observation is under test, not host admission.
	d.live.guard.observe = func(context.Context) (hostguard.Sample, error) { return hostSample(1, 0.5), nil }
	d.live.runner = prophetRouteRunner(func(context.Context, agentruntime.Launch) (agentruntime.Collected, error) {
		return agentruntime.Collected{ProcessesRetired: true, Output: validProphetReply}, nil
	})
	done := make(chan error, 1)
	d.mu.Lock()
	go func() {
		_, err := d.runProphet(ctx, "", "prophet-unlocked-observation", json.RawMessage(`[]`))
		done <- err
	}()
	select {
	case err = <-done:
		d.mu.Unlock()
	case <-time.After(30 * time.Second):
		d.mu.Unlock()
		<-done
		t.Fatal("the Prophet route observation waited for the writer lock")
	}
	if err != nil {
		t.Fatal(err)
	}
	health, err := d.store.ProviderHealth(ctx)
	if err != nil || len(health) != 1 || health[0].Outcome != providerhealth.OK {
		t.Fatal("route outcome not recorded", health, err)
	}
}
