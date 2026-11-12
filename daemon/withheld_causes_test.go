package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/hostguard"
)

// withheldNow reads the latest round's withheld tasks from status.
func withheldNow(t *testing.T, d *Daemon) (map[string]int, map[string]string) {
	t.Helper()
	s := d.status.Load().(statusSnapshot)
	counts, _ := s.Scheduler["withheld"].(map[string]int)
	tasks, _ := s.Scheduler["withheld_tasks"].(map[string]string)
	return counts, tasks
}

// FAE-SCH-010: a checkpoint left unsettled after the round began (here by a
// housekeeping step) holds every task the round has not yet decided, and each
// is withheld as checkpoint_unsettled rather than skipped without a reason.
func TestMidRoundUnsettledCheckpointWithholdsRemainingTasks(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	stopDrive(t, d)
	for _, id := range []string{"held-one", "held-two"} {
		if _, err = call(t, d, "Admit", AdmitParams{ID: id, Tree: "tree", AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
	}
	d.mu.Lock()
	ghost := d.jobs["held-one"]
	ghost.ID = "ghost"
	d.testFault = func(point, id string) error {
		if point == "step" && id == "sub-workflows" {
			// Runs under the writer lock, after the round settled its start.
			d.pendingCheckpoint = &jobCheckpoint{Before: ghost, Proposed: ghost}
		}
		return nil
	}
	d.mu.Unlock()
	err = d.dispatch()
	d.mu.Lock()
	d.testFault = nil
	d.pendingCheckpoint = nil
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	counts, tasks := withheldNow(t, d)
	if tasks["held-one"] != withheldCheckpointUnsettled || tasks["held-two"] != withheldCheckpointUnsettled || counts[withheldCheckpointUnsettled] != 2 || len(tasks) != 2 {
		t.Fatalf("tasks skipped behind a mid-round checkpoint: %v (counts %v)", tasks, counts)
	}
	raw, err := os.ReadFile(d.layout.Log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "held-one:checkpoint_unsettled,held-two:checkpoint_unsettled") {
		t.Fatalf("withheld trace lacks the mid-round checkpoint:\n%s", raw)
	}
}

// FAE-SCH-010, FAE-CP-006: after a plane restart the drive acts on no
// checkpoint the plane has not re-confirmed. While the plane is unreachable
// the round ends early, and every task it would have decided is withheld as
// plane_unconfirmed.
func TestPlaneUnconfirmedRoundIsWithheld(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	stopDrive(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "unconfirmed", Tree: "tree", AuthorizingMessage: "message"}, "unconfirmed"); err != nil {
		t.Fatal(err)
	}
	child := d.currentChild()
	if err = child.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-child.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("Dolt child survived its kill")
	}
	// A plane restart asks the drive to re-confirm every checkpoint; the
	// plane monitor waits a probe period before restarting it itself.
	d.revalidate.Store(true)
	err = d.dispatch()
	revalidating := d.revalidate.Load()
	counts, tasks := withheldNow(t, d)
	if restartErr := d.restartPlane(); restartErr != nil {
		t.Fatal(restartErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if tasks["unconfirmed"] != withheldPlaneUnconfirmed || counts[withheldPlaneUnconfirmed] != 1 || len(tasks) != 1 || !revalidating {
		t.Fatalf("unreachable plane round: withheld %v (counts %v), re-validation still pending %v", tasks, counts, revalidating)
	}
	raw, err := os.ReadFile(d.layout.Log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "unconfirmed:plane_unconfirmed") {
		t.Fatalf("withheld trace lacks plane_unconfirmed:\n%s", raw)
	}
}

// FAE-SCH-010, FAE-OBS-004: a resident pane reservation refused at the pane
// limit withholds its task as pane_limit, the cause its admission line names
// (reason=PaneLimit), not host_guard; a refusal on host pressure is counted
// as host_guard. The task is read from the dispatch's launch context.
func TestResidentPaneRefusalWithheldByCause(t *testing.T) {
	a, _, logPath := pressureFixture(t, 1)
	paneFixture(t, a, 3)
	a.view.Store(&scheduleView{molecule: map[string]string{"resident-task": "m"}})
	pressured := false
	c := hostguard.DefaultConfig()
	a.observe = func(context.Context) (hostguard.Sample, error) {
		if pressured {
			return hostSample(1, c.Load+1), nil
		}
		return hostSample(1, 0.5), nil
	}
	a.inventory = func(context.Context) (map[string]int, error) { return map[string]int{"/foreign": 3}, nil }
	socket := filepath.Join(a.daemon.layout.Root, "resident.sock")
	launch := withWithheldTask(context.Background(), "resident-task")
	parked := engine.Job{TaskID: "resident-task", Parked: true, Reason: "infrastructure-deferred"}
	if e := a.ReserveResidentPane(launch, "resident-1", socket); refusalReason(t, e) != "PaneLimit" {
		t.Fatal(e)
	}
	if r := parkedReason(parked, a); r != withheldPaneLimit {
		t.Fatalf("resident pane-limit refusal withheld as %q", r)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil || !strings.Contains(string(raw), "reason=PaneLimit") {
		t.Fatalf("admission line does not name the pane limit: %s %v", raw, err)
	}
	pressured = true
	if e := a.ReserveResidentPane(launch, "resident-1", socket); refusalReason(t, e) != "HostPressure" {
		t.Fatal(e)
	}
	if r := parkedReason(parked, a); r != withheldHostGuard {
		t.Fatalf("resident pressure refusal withheld as %q", r)
	}
}
