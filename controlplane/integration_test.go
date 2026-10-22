package controlplane

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/scheduler"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestDoltIntegration starts its own disposable database; it never accepts a DSN.
// Set FAE_TEST_DOLT_BINARY to an installed Dolt executable to enable it.
func TestDoltIntegration(t *testing.T) {
	binary := os.Getenv("FAE_TEST_DOLT_BINARY")
	if binary == "" {
		t.Skip("set FAE_TEST_DOLT_BINARY to enable isolated Dolt integration")
	}
	root := t.TempDir()
	data := filepath.Join(root, "plane")
	if e := os.Mkdir(data, 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("DOLT_ROOT_PATH", filepath.Join(root, "config"))
	t.Setenv("DOLT_DISABLE_EVENT_FLUSH", "1")
	init := exec.Command(binary, "init", "--name", "Fae Tests", "--email", "tests@localhost")
	init.Dir = data
	if output, e := init.CombinedOutput(); e != nil {
		t.Fatalf("initialize isolated Dolt: %v: %s", e, output)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	server, e := StartServer(ctx, ServerConfig{Binary: binary, DataDir: data, StopWait: 2 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	defer server.Stop()
	dsn := fmt.Sprintf("root@tcp(127.0.0.1:%d)/plane?parseTime=true", server.Port())
	var ready *Store
	for i := 0; i < 100; i++ {
		ready, e = Open(ctx, dsn, Options{})
		if e == nil {
			ready.Close()
			break
		}
		select {
		case <-server.Done():
			t.Fatalf("Dolt exited: %v", server.Err())
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	if e != nil {
		t.Fatalf("Dolt readiness: %v", e)
	}
	s, e := Open(ctx, dsn, Options{VersionCommits: true})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	// Build a genuine v1 database, then migrate it forward without losing audit data.
	if _, e = s.db.ExecContext(ctx, `CREATE TABLE migrations (version BIGINT PRIMARY KEY,applied_at DATETIME(6) NOT NULL)`); e != nil {
		t.Fatal(e)
	}
	for _, statement := range SchemaStatements {
		if _, e = s.db.ExecContext(ctx, statement); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = s.db.ExecContext(ctx, `INSERT INTO migrations VALUES (1,UTC_TIMESTAMP(6))`); e != nil {
		t.Fatal(e)
	}
	if e = s.Mutate(ctx, Audit{Actor: "migration-test", Action: "v1-preserved", Outcome: "ok"}, func(context.Context, *sql.Tx) error { return nil }); e != nil {
		t.Fatal(e)
	}
	if e = s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	if e = s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	var preserved int
	if e = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit WHERE action='v1-preserved'`).Scan(&preserved); e != nil || preserved != 1 {
		t.Fatal("migration lost v1 audit", e)
	}
	exerciseDurableJobs(t, ctx, s)
	checkProviderDolt(t, ctx, s)
	if e = s.EnsureLeasePool(ctx, audit(), LeasePoolConfig{Key: "durable-role", Kind: "role", Capacity: 2}); e != nil {
		t.Fatal(e)
	}
	if e = s.EnsureLeasePool(ctx, audit(), LeasePoolConfig{Key: "restart-lease", Kind: "role", Capacity: 1}); e != nil {
		t.Fatal(e)
	}
	if e = s.EnqueueLease(ctx, audit(), "restart-lease", scheduler.Request{ID: "held", Owner: scheduler.Owner{PID: 1, Started: 1}, Weight: 1, Queued: time.Now()}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.AdvanceLeases(ctx, audit(), "restart-lease", time.Now(), map[scheduler.Owner]scheduler.Liveness{}); e != nil {
		t.Fatal(e)
	}

	// Abruptly lose the child, reconnect to a new owned child, and recover the
	// committed snapshot without manufacturing another event or admission.
	if e = server.cmd.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	<-server.Done()
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	server, e = StartServer(ctx, ServerConfig{Binary: binary, DataDir: data, StopWait: 2 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	defer server.Stop()
	for i := 0; i < 100; i++ {
		s, e = Open(ctx, dsn, Options{VersionCommits: true})
		if e == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	recovered, e := s.GetJob(ctx, "job")
	if e != nil || recovered.Version != 2 || recovered.State != "Doing" {
		t.Fatal("child crash recovery", recovered, e)
	}
	events, e := s.JobEvents(ctx, "job", 0, 10)
	if e != nil || len(events) != 2 {
		t.Fatal("child recovery duplicated events", events, e)
	}
	if p, e := s.LeaseSnapshot(ctx, "durable-role"); e != nil || p.Capacity != 2 {
		t.Fatal("lease restart", p, e)
	}
	if p, e := s.LeaseSnapshot(ctx, "restart-lease"); e != nil || len(p.Holders) != 1 || p.Holders[0].ID != "held" {
		t.Fatal("held lease lost at crash", p, e)
	}
	exerciseDurableLeases(t, ctx, s, dsn)
	suffix := time.Now().UTC().Format("150405.000000000")
	a := Audit{Actor: "integration-test", Action: "round-" + suffix, Outcome: "ok"}
	content := bytes.Repeat([]byte("large-remediation"), 65536)
	ref, e := s.PutBlob(ctx, a, content)
	if e != nil {
		t.Fatal(e)
	}
	actual, e := s.GetBlob(ctx, ref)
	if e != nil || !bytes.Equal(actual, content) {
		t.Fatalf("blob %v", e)
	}
	validate := func([]byte) error { return nil }
	r, e := s.WriteWorkflow(ctx, a, "workflow-"+suffix, "test", json.RawMessage(`{"schema_version":1,"name":"`+suffix+`"}`), validate)
	if e != nil {
		t.Fatal(e)
	}
	m, e := s.BindMolecule(ctx, a, r.WorkflowID, r.Number)
	if e != nil {
		t.Fatal(e)
	}
	task := Task{ID: "task-" + suffix, RunID: suffix, MoleculeID: m, TargetID: "local", TargetRevision: 1, State: "Pending", AttemptLimit: 20, Priority: 10, Disposition: json.RawMessage(`{"mode":"Local"}`)}
	if e = s.CreateTask(ctx, a, task); e != nil {
		t.Fatal(e)
	}
	digest, e := s.RecordArtifact(ctx, a, task.ID, "Test", "test", 1, json.RawMessage(`{"passed":true}`), json.RawMessage(`{"producer":"test"}`), validate)
	if e != nil {
		t.Fatal(e)
	}
	transition := Transition{TaskID: task.ID, From: "Pending", To: "Done", Owner: "test", EvidenceDigest: digest}
	if e = s.Transition(ctx, a, transition, func(Task, Transition) error { return nil }); e != nil {
		t.Fatal(e)
	}
	got, e := s.GetTask(ctx, task.ID)
	if e != nil || got.State != "Done" {
		t.Fatalf("task %+v %v", got, e)
	}
	if e = s.Transition(ctx, a, transition, func(Task, Transition) error { return nil }); e == nil {
		t.Fatal("stale transition accepted")
	}
	var before int
	if e = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit`).Scan(&before); e != nil {
		t.Fatal(e)
	}
	sentinel := errors.New("injected failure")
	e = s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) error {
		if _, e := tx.ExecContext(ctx, `UPDATE tasks SET state='Broken' WHERE task_id=?`, task.ID); e != nil {
			return e
		}
		return sentinel
	})
	if !errors.Is(e, sentinel) {
		t.Fatal(e)
	}
	got, e = s.GetTask(ctx, task.ID)
	if e != nil || got.State != "Done" {
		t.Fatal("rollback failed", e)
	}
	var after int
	if e = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit`).Scan(&after); e != nil || before != after {
		t.Fatal("audit rollback", before, after, e)
	}
	var commits int
	if e = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dolt_log WHERE message=?`, a.Action+" by "+a.Actor).Scan(&commits); e != nil || commits < 6 {
		t.Fatalf("version commits=%d err=%v", commits, e)
	}
}

func exerciseDurableJobs(t *testing.T, ctx context.Context, s *Store) {
	t.Helper()
	j := jobFixture()
	a := audit()
	a.Inputs = json.RawMessage(`{"id":"job"}`)
	admitted, e := s.AdmitJob(ctx, a, "admission-key", j)
	if e != nil {
		t.Fatal(e)
	}
	admitted.State = "Doing"
	admitted.Payload = json.RawMessage(`{"stage":1}`)
	saved, e := s.SaveJob(ctx, a, admitted, 1)
	if e != nil || saved.Version != 2 {
		t.Fatal(saved, e)
	}
	replay, e := s.AdmitJob(ctx, a, "admission-key", j)
	if e != nil || replay.Version != 1 || replay.State != "Pending" {
		t.Fatal("replay changed original response", replay, e)
	}
	if _, e = s.SaveJob(ctx, a, admitted, 1); !errors.Is(e, ErrConflict) {
		t.Fatal("stale save", e)
	}
	j.Payload = json.RawMessage(`{"stage":2}`)
	a.Inputs = json.RawMessage(`{"id":"different"}`)
	if _, e = s.AdmitJob(ctx, a, "admission-key", j); !errors.Is(e, ErrIdempotencyConflict) {
		t.Fatal("key conflict", e)
	}
	// Fail after the snapshot UPDATE and blob write but before event/audit commit.
	broken := saved
	broken.State = "Broken"
	broken.Payload = json.RawMessage(`{"stage":99}`)
	broken.Events = []Event{{Kind: "", Payload: json.RawMessage(`{}`)}}
	if _, e = s.SaveJob(ctx, a, broken, 2); e == nil {
		t.Fatal("invalid event committed")
	}
	replay, found, e := s.LookupAdmission(ctx, "admission-key", json.RawMessage(`{ "id": "job" }`))
	if e != nil || !found || replay.Version != 1 {
		t.Fatal("canonical RPC replay", replay, found, e)
	}
	got, e := s.GetJob(ctx, j.ID)
	if e != nil || got.Version != 2 || got.State != "Doing" {
		t.Fatal(got, e)
	}
	events, e := s.Events(ctx, 0, 10)
	if e != nil || len(events) != 2 || events[0].Sequence != 1 || events[1].Sequence != 2 {
		t.Fatal(events, e)
	}
	resumed, e := s.Events(ctx, 1, 10)
	if e != nil || len(resumed) != 1 || resumed[0].Sequence != 2 {
		t.Fatal(resumed, e)
	}
	if e = s.PutMarker(ctx, a, "graceful-stop", json.RawMessage(`{"inflight":["job"]}`)); e != nil {
		t.Fatal(e)
	}
	marker, e := s.GetMarker(ctx, "graceful-stop")
	if e != nil || string(marker) != `{"inflight":["job"]}` {
		t.Fatal(string(marker), e)
	}
	if e = s.RecordMessage(ctx, "operator-message", "approve local run"); e != nil {
		t.Fatal(e)
	}
	if e = s.RequireMessage(ctx, "operator-message"); e != nil {
		t.Fatal(e)
	}
	if e = s.RequireMessage(ctx, "missing"); e == nil {
		t.Fatal("missing authority accepted")
	}
}
