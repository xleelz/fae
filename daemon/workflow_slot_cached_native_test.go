package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

func TestWorkflowSlotFlockProcessHelper(t *testing.T) {
	path := os.Getenv("FAE_TEST_WORKFLOW_FLOCK")
	if path == "" {
		t.Skip("subprocess helper")
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stdout.Write([]byte("locked")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Second)
	os.Exit(0)
}

type heldStageExecutor struct {
	started       chan string
	first, second chan struct{}
	delegate      *engine.Scripted
}

func (x *heldStageExecutor) Execute(ctx context.Context, d engine.Dispatch) (engine.Result, error) {
	x.started <- d.Stage
	release := x.first
	if d.Stage == "second" {
		release = x.second
	}
	select {
	case <-ctx.Done():
		return engine.Result{}, ctx.Err()
	case <-release:
	}
	return x.delegate.Execute(ctx, d)
}

func TestWorkflowSlotsNativeHeldNextStageIgnoresLockedLedgerAndIdleRoundsDoNotWrite(t *testing.T) {
	cfg, ctx := fixture(t)
	x := &heldStageExecutor{started: make(chan string, 2), first: make(chan struct{}), second: make(chan struct{}), delegate: engine.NewScripted(map[string][]engine.ScriptStep{"first": {{Output: json.RawMessage(`{"text":"first"}`)}}, "second": {{Output: json.RawMessage(`{"text":"second"}`)}}})}
	cfg.Executor = x
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	doc := slotWorkflow("held-next-stage", "tree")
	stage := doc.Stages["make"]
	doc.Start = "first"
	doc.Stages = map[string]workflow.Stage{"first": stage, "second": stage}
	doc.Edges = []workflow.Edge{{From: "first", To: "second"}, {From: "second", To: "Done"}}
	governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"})
	if _, err = call(t, d, "Admit", AdmitParams{ID: "root", WorkflowID: doc.ID, Tree: "tree", AuthorizingMessage: "message"}, "root"); err != nil {
		t.Fatal(err)
	}
	select {
	case stage := <-x.started:
		if stage != "first" {
			t.Fatal(stage)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("first stage did not start")
	}
	d.mu.Lock()
	d.workflowSlots.lastUsage = time.Now()
	if d.workflowSlots.usageTimer != nil {
		d.workflowSlots.usageTimer.Stop()
	}
	d.mu.Unlock()
	statePath := filepath.Join(cfg.WorkflowLedgerPath, "state.json")
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	locker := exec.Command(os.Args[0], "-test.run=^TestWorkflowSlotFlockProcessHelper$")
	locker.Env = append(os.Environ(), "FAE_TEST_WORKFLOW_FLOCK="+filepath.Join(cfg.WorkflowLedgerPath, "lock"))
	stdout, err := locker.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	locker.Stderr = os.Stderr
	if err = locker.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if locker.ProcessState == nil {
			_ = locker.Process.Kill()
			_ = locker.Wait()
		}
	}()
	ready := make([]byte, 6)
	if _, err = io.ReadFull(stdout, ready); err != nil || string(ready) != "locked" {
		t.Fatal("ledger lock helper", string(ready), err)
	}
	close(x.first)
	select {
	case stage := <-x.started:
		if stage != "second" {
			t.Fatal(stage)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("held member's next stage waited for the locked ledger")
	}
	for range 3 {
		round := d.driveRounds.Load()
		d.signal()
		deadline := time.Now().Add(time.Second)
		for d.driveRounds.Load() <= round {
			if time.Now().After(deadline) {
				t.Fatal("unchanged held round blocked")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !info.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatal("held next stage or unchanged rounds rewrote the host ledger")
	}
	if err = locker.Wait(); err != nil {
		t.Fatal(err)
	}
	close(x.second)
	awaitState(t, d, "root", "Done")
}
