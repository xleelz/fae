package daemon

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/execution"
	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/processowner"
	"github.com/xleelz/fae/scheduler"
	"github.com/xleelz/fae/workflow"
)

func TestLeaseRecoveryDaemonHelper(t *testing.T) {
	path := os.Getenv("FAE_LEASE_DAEMON_FIXTURE")
	if path == "" {
		return
	}
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var cfg Config
	if e = json.Unmarshal(b, &cfg); e != nil {
		t.Fatal(e)
	}
	if e = os.Chdir(cfg.Workspace); e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	a, e := newLiveAdmission(d, LiveConfig{HostLedger: filepath.Join(filepath.Dir(path), "host-ledger"), HeavyCapacity: 2})
	if e != nil {
		t.Fatal(e)
	}
	a.observe = func(context.Context) (hostguard.Sample, error) {
		return hostguard.Sample{Memory: hostguard.Reading{Value: float64(32 * hostguard.GiB), Known: true}}, nil
	}
	session := engine.Session{ID: "crash-session", Role: "worker"}
	dispatch := engine.Dispatch{TaskID: "task", Config: workflow.Stage{}}
	_ = dispatch
	// Use the exact production admission path and its two durable pools.
	dispatch.Config.Primitive = workflow.HostCommand
	binding := execution.Binding{Document: workflow.Document{ID: "wf", Roles: map[string]workflow.Role{"worker": {Cap: 1, Scope: "workflow-global"}}}}
	if _, e = a.Acquire(ctx, dispatch, binding, session); e != nil {
		t.Fatal(e)
	}
	if e = a.Launching(ctx, session.ID); e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command("/bin/sh", "-c", "sleep 300 & wait")
	record, e := processowner.StartLinked(cmd, filepath.Join(d.layout.Run, "effect-processes"), d.layout.Worktrees, session.ID)
	if e != nil {
		t.Fatal(e)
	}
	if os.Getenv("FAE_LEASE_RETIRE_BEFORE_CRASH") == "1" {
		if e = processowner.Terminate(ctx, record, d.layout.Worktrees); e != nil {
			t.Fatal(e)
		}
		cmd.Wait()
		if e = processowner.Retire(record); e != nil {
			t.Fatal(e)
		}
		// Deliberately leave the operation journal launching. A daemon SIGKILL
		// here must recover from the terminal receipt, never infer nonexecution.
	}
	proof, e := a.journal.Read(a.proofID(session.ID))
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(proof)
	if e = os.WriteFile(path+".ready", raw, 0600); e != nil {
		t.Fatal(e)
	}
	select {}
}
func TestSIGKILLLinkedWorkloadRecoversBothLeasePools(t *testing.T) {
	testSIGKILLLinkedWorkload(t, false)
}
func TestSIGKILLAfterProcessRetirementBeforeJournalAck(t *testing.T) {
	testSIGKILLLinkedWorkload(t, true)
}
func testSIGKILLLinkedWorkload(t *testing.T, retiredBeforeCrash bool) {
	cfg, ctx := fixture(t)
	root := filepath.Dir(cfg.Workspace)
	hostPath := filepath.Join(root, "host-ledger")
	host, e := scheduler.OpenLedger(hostPath, 2)
	if e != nil {
		t.Fatal(e)
	}
	owner, e := scheduler.CurrentOwner()
	if e != nil {
		t.Fatal(e)
	}
	if e = host.Enqueue(ctx, scheduler.Request{ID: "foreign", Owner: owner, Weight: 1, Queued: time.Now()}); e != nil {
		t.Fatal(e)
	}
	if _, _, e = host.Advance(ctx, time.Now(), map[scheduler.Owner]scheduler.Liveness{owner: scheduler.Alive}); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(root, "lease-crash.json")
	b, marshalErr := json.Marshal(cfg)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	os.WriteFile(path, b, 0600)
	exe, _ := os.Executable()
	cmd := exec.Command(exe, "-test.run=^TestLeaseRecoveryDaemonHelper$")
	cmd.Env = append(os.Environ(), "FAE_LEASE_DAEMON_FIXTURE="+path)
	if retiredBeforeCrash {
		cmd.Env = append(cmd.Env, "FAE_LEASE_RETIRE_BEFORE_CRASH=1")
	}
	log, e := os.Create(path + ".log")
	if e != nil {
		t.Fatal(e)
	}
	defer log.Close()
	cmd.Stdout = log
	cmd.Stderr = log
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	var proof scheduler.OperationProof
	deadline := time.Now().Add(20 * time.Second)
	for {
		raw, e := os.ReadFile(path + ".ready")
		if e == nil {
			if e = json.Unmarshal(raw, &proof); e != nil {
				t.Fatal(e)
			}
			break
		}
		if time.Now().After(deadline) {
			log.Sync()
			out, _ := os.ReadFile(path + ".log")
			t.Fatalf("helper timeout %s", out)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if proof.State != "launching" || proof.Expected != 1 {
		t.Fatal(proof)
	}
	cmd.Process.Kill()
	cmd.Wait()
	// Startup watchdog may race the restart; await its durable proof, not a timeout
	// assumption that the process is gone.
	deadline = time.Now().Add(8 * time.Second)
	for {
		complete, e := processowner.LinkedRetirement(proof.Registry, proof.ProcessOwner, proof.Session, proof.Owner.PID, proof.Birth, 1)
		if e != nil {
			t.Fatal(e)
		}
		if complete {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("watchdog never supplied complete retirement proof")
		}
		time.Sleep(20 * time.Millisecond)
	}
	var d *Daemon
	deadline = time.Now().Add(8 * time.Second)
	for {
		d, e = Start(ctx, cfg)
		if e == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(e)
		}
		time.Sleep(50 * time.Millisecond)
	}
	defer stop(t, d)
	if _, e = processowner.Recover(ctx, proof.Registry, proof.ProcessOwner); e != nil {
		t.Fatal(e)
	}
	a, e := newLiveAdmission(d, LiveConfig{HostLedger: hostPath, HeavyCapacity: 2})
	if e != nil {
		t.Fatal(e)
	}
	p, e := a.host.Snapshot(ctx)
	if e != nil || len(p.Holders) != 1 || p.Holders[0].ID != "foreign" {
		t.Fatal(p, e)
	}
	role, e := d.store.LeaseSnapshot(ctx, proof.RolePool)
	if e != nil || len(role.Holders) != 0 || len(role.Waiting) != 0 {
		t.Fatal(role, e)
	}
	entries, e := a.journal.Entries()
	if e != nil || len(entries) != 0 {
		t.Fatal(entries, e)
	}
	if _, e = newLiveAdmission(d, LiveConfig{HostLedger: hostPath, HeavyCapacity: 2}); e != nil {
		t.Fatal("restart replay", e)
	}
	audit, e := host.Audit(ctx)
	if e != nil {
		t.Fatal(e)
	}
	recovered := 0
	for _, item := range audit {
		if item.Action == "recovered" {
			recovered++
		}
	}
	if recovered != 1 {
		t.Fatal("recovery audit count", recovered)
	}
}
