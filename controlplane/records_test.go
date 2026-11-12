package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// testPlane starts a disposable Dolt plane at schema version, or skips
// without FAE_TEST_DOLT_BINARY.
func testPlane(t testing.TB, version int) *Store {
	t.Helper()
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
	// The server lives as long as its start context: until cleanup.
	ctx, cancel := context.WithCancel(context.Background())
	server, e := StartServer(ctx, ServerConfig{Binary: binary, DataDir: data, StopWait: 2 * time.Second})
	if e != nil {
		cancel()
		t.Fatal(e)
	}
	t.Cleanup(func() { server.Stop(); cancel() })
	dsn := fmt.Sprintf("root@tcp(127.0.0.1:%d)/plane?parseTime=true", server.Port())
	var s *Store
	for i := 0; i < 200; i++ {
		if s, e = Open(ctx, dsn, Options{}); e == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	if e = s.migrateTo(ctx, version); e != nil {
		t.Fatal(e)
	}
	return s
}

func installWorkflow(t testing.TB, s *Store, doc workflow.Document) Revision {
	t.Helper()
	raw, _ := json.Marshal(doc)
	r, e := s.WriteWorkflow(context.Background(), audit(), doc.ID, doc.ID, raw, func([]byte) error { return nil })
	if e != nil {
		t.Fatal(e)
	}
	return r
}

func envelopeSnapshot(t *testing.T, j engine.Job) JobSnapshot {
	t.Helper()
	raw, e := json.Marshal(map[string]any{"job": j, "admission": map[string]any{"priority": 10, "run_id": "run"}})
	if e != nil {
		t.Fatal(e)
	}
	return JobSnapshot{ID: j.TaskID, WorkflowID: j.Document.ID, WorkflowRevision: int64(j.Revision), TargetID: "target", TargetRevision: 1, State: string(j.State), Payload: raw}
}

// recordCounts is every relational record of one task, and its job version.
type recordCounts struct {
	Version                                      int64
	State                                        string
	Transitions, Decisions, Provenance, Artifact int
	Events                                       int
}

func countRecords(t *testing.T, s *Store, task string) recordCounts {
	t.Helper()
	ctx := context.Background()
	var c recordCounts
	if e := s.db.QueryRowContext(ctx, `SELECT version FROM jobs WHERE job_id=?`, task).Scan(&c.Version); e != nil {
		t.Fatal(e)
	}
	if e := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(state),'') FROM tasks WHERE task_id=?`, task).Scan(&c.State); e != nil {
		t.Fatal(e)
	}
	for _, q := range []struct {
		table string
		n     *int
	}{{"transitions", &c.Transitions}, {"decisions", &c.Decisions}, {"provenance", &c.Provenance}, {"artifacts", &c.Artifact}} {
		if e := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+q.table+` WHERE task_id=?`, task).Scan(q.n); e != nil {
			t.Fatal(e)
		}
	}
	if e := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE job_id=?`, task).Scan(&c.Events); e != nil {
		t.Fatal(e)
	}
	return c
}

// checkpoint saves one engine step with fault injection at every write of the
// checkpoint first: each interrupted save leaves every record unchanged, then
// the clean save writes them all (FAE-WF-022).
func checkpoint(t *testing.T, s *Store, saved JobSnapshot, next engine.Job) JobSnapshot {
	t.Helper()
	ctx := context.Background()
	proposal := envelopeSnapshot(t, next)
	before := countRecords(t, s, saved.ID)
	sentinel := errors.New("injected interruption")
	for fail := 1; ; fail++ {
		calls := 0
		s.fault = func(string) error {
			if calls++; calls == fail {
				return sentinel
			}
			return nil
		}
		got, e := s.SaveJob(ctx, audit(), proposal, saved.Version)
		s.fault = nil
		if calls < fail {
			// Every write passed the seam: this is the clean checkpoint.
			if e != nil {
				t.Fatal(e)
			}
			return got
		}
		if !errors.Is(e, sentinel) {
			t.Fatal(e)
		}
		if after := countRecords(t, s, saved.ID); after != before {
			t.Fatalf("interruption %d left partial records: %+v -> %+v", fail, before, after)
		}
	}
}

func TestCheckpointRecordsAtomicChainAndImmutability(t *testing.T) {
	s := testPlane(t, SchemaVersion)
	ctx := context.Background()
	doc := defaults.Workflow()
	installWorkflow(t, s, doc)
	job, e := engine.New("task-a", 1, doc, "fixture-tree")
	if e != nil {
		t.Fatal(e)
	}
	saved, e := s.AdmitJob(ctx, audit(), "", envelopeSnapshot(t, job))
	if e != nil {
		t.Fatal(e)
	}
	molecule := saved.MoleculeID
	if !validDigest(molecule) {
		t.Fatal("admission bound no molecule", saved)
	}
	executor := engine.NewScripted(defaults.HappyScript())
	transitions := 0
	for i := 0; i < 200 && !job.State.Terminal(); i++ {
		next, d, events, e := engine.Prepare(job)
		if e != nil {
			t.Fatal(e)
		}
		if d == nil && len(events) == 0 {
			t.Fatalf("stalled at %s", job.State)
		}
		saved, job = checkpoint(t, s, saved, next), next
		if d != nil {
			r, e := executor.Execute(ctx, *d)
			if e != nil {
				t.Fatal(e)
			}
			r.Transcript = &engine.Transcript{ProviderSession: "thread-" + d.Stage, LogDirectory: "/logs/" + d.Stage}
			for n := range r.Reviews {
				r.Reviews[n].Transcript = r.Transcript
			}
			if next, _, e = engine.Complete(job, d.ID, r); e != nil {
				t.Fatal(e)
			}
			saved, job = checkpoint(t, s, saved, next), next
		}
	}
	if job.State != domain.Done {
		t.Fatalf("state %s %s", job.State, job.Reason)
	}
	for _, ev := range job.History {
		if transitionEvent(ev) {
			transitions++
		}
	}
	c := countRecords(t, s, "task-a")
	if c.State != "Done" || c.Transitions != transitions || c.Provenance != len(job.Provenance) || c.Artifact == 0 || c.Decisions == 0 {
		t.Fatalf("records %+v, want %d transitions and %d provenance", c, transitions, len(job.Provenance))
	}
	m, e := s.GetMolecule(ctx, molecule)
	if e != nil || m.EndedAt == nil {
		t.Fatal("molecule did not end with its last task", m, e)
	}
	history, e := s.TaskTransitions(ctx, "task-a", 0, 1000)
	if e != nil || len(history) != transitions || history[0].From != "Pending" || history[len(history)-1].To != "Done" {
		t.Fatal(history, e)
	}
	for _, h := range history {
		if _, e = s.GetBlob(ctx, BlobRef{Digest: h.EvidenceDigest, Size: blobSize(t, s, h.EvidenceDigest)}); e != nil {
			t.Fatal("transition evidence unresolvable", e)
		}
	}

	// A chain query from the commit reaches the spec and its intake.
	chain, e := s.ProvenanceChain(ctx, "scripted-commit", 1000)
	if e != nil {
		t.Fatal(e)
	}
	stages := map[string]bool{}
	for _, hop := range chain {
		stages[hop.Stage] = true
		if hop.MoleculeID != molecule || hop.SessionID == "" || hop.Transcript == nil || hop.Transcript.ProviderSession != "thread-"+hop.Stage {
			t.Fatalf("hop lacks binding or transcript: %+v", hop)
		}
	}
	for _, stage := range []string{"commit", "implement", "spec", "intake"} {
		if !stages[stage] {
			t.Fatalf("chain misses %s: %v", stage, stages)
		}
	}

	// Provenance, artifacts, transitions and decisions refuse rewrites.
	for _, q := range []string{
		`UPDATE provenance SET role='forged' WHERE task_id='task-a'`,
		`DELETE FROM provenance WHERE task_id='task-a'`,
		`REPLACE INTO provenance (` + provenanceColumns + `,created_at) SELECT ` + provenanceColumns + `,created_at FROM provenance WHERE task_id='task-a' LIMIT 1`,
		`UPDATE artifacts SET kind='forged' WHERE task_id='task-a'`,
		`DELETE FROM artifacts WHERE task_id='task-a'`,
		`UPDATE transitions SET to_state='Forged' WHERE task_id='task-a'`,
		`DELETE FROM decisions WHERE task_id='task-a'`,
	} {
		if _, e = s.db.ExecContext(ctx, q); e == nil || !strings.Contains(e.Error(), "immutable") && !strings.Contains(e.Error(), "append-only") {
			t.Fatalf("plane accepted %s: %v", q, e)
		}
	}
	if after := countRecords(t, s, "task-a"); after != c {
		t.Fatal("refused rewrites changed records", c, after)
	}
}

func blobSize(t *testing.T, s *Store, digest string) int64 {
	t.Helper()
	var n int64
	if e := s.db.QueryRowContext(context.Background(), `SELECT size FROM blobs WHERE digest=?`, digest).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}

// FAE-WF-006/-007: tasks of one revision share its molecule; identical content
// in another revision is a distinct molecule; a planted content mismatch, an
// unknown revision and a retired workflow are refused at admission.
func TestMoleculeBindingAtAdmission(t *testing.T) {
	s := testPlane(t, SchemaVersion)
	ctx := context.Background()
	doc := defaults.Workflow()
	installWorkflow(t, s, doc)
	installWorkflow(t, s, doc)
	molecules := map[int]map[string]bool{1: {}, 2: {}}
	for i := 0; i < 10; i++ {
		revision := 1 + i%2
		job, e := engine.New(fmt.Sprintf("task-%d", i), revision, doc, "tree")
		if e != nil {
			t.Fatal(e)
		}
		saved, e := s.AdmitJob(ctx, audit(), "", envelopeSnapshot(t, job))
		if e != nil {
			t.Fatal(e)
		}
		molecules[revision][saved.MoleculeID] = true
	}
	if len(molecules[1]) != 1 || len(molecules[2]) != 1 {
		t.Fatal("tasks of one revision do not share one molecule", molecules)
	}
	for one := range molecules[1] {
		if molecules[2][one] {
			t.Fatal("revisions with identical content collided on one molecule id")
		}
	}
	var n int
	if e := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM molecules`).Scan(&n); e != nil || n != 2 {
		t.Fatal(n, e)
	}
	topology, e := s.Topology(ctx, "", "", 10, 100, false)
	if e != nil || len(topology.Molecules) != 2 || len(topology.Molecules[0].Tasks) != 5 || topology.Statements != 2 {
		t.Fatal(topology, e)
	}
	if _, e = s.db.ExecContext(ctx, `UPDATE molecules SET content_digest=? WHERE workflow_id=? AND revision=1`, strings.Repeat("c", 64), doc.ID); e != nil {
		t.Fatal(e)
	}
	job, _ := engine.New("mismatch", 1, doc, "tree")
	if _, e = s.AdmitJob(ctx, audit(), "", envelopeSnapshot(t, job)); !errors.Is(e, ErrMoleculeMismatch) {
		t.Fatal("planted molecule hash mismatch accepted", e)
	}
	job, _ = engine.New("unknown", 3, doc, "tree")
	if _, e = s.AdmitJob(ctx, audit(), "", envelopeSnapshot(t, job)); !errors.Is(e, ErrUnknownRevision) {
		t.Fatal("unknown revision accepted", e)
	}
	if e = s.RetireWorkflow(ctx, audit(), doc.ID, "superseded"); e != nil {
		t.Fatal(e)
	}
	job, _ = engine.New("retired", 2, doc, "tree")
	if _, e = s.AdmitJob(ctx, audit(), "", envelopeSnapshot(t, job)); !errors.Is(e, ErrWorkflowRetired) {
		t.Fatal("retired workflow accepted a new binding", e)
	}
}

// Migration 8 on an existing v7 plane binds every job, projects its records
// and rekeys a molecule bound under the colliding identity. An interruption
// leaves version 7 and no partial records; the replay completes once.
func TestMigration8BackfillInterruptedReplay(t *testing.T) {
	s := testPlane(t, 7)
	ctx := context.Background()
	doc := defaults.Workflow()
	r := installWorkflow(t, s, doc)
	job, e := engine.New("legacy", 1, doc, "tree")
	if e != nil {
		t.Fatal(e)
	}
	executor := engine.NewScripted(defaults.HappyScript())
	for i := 0; job.State != "plan"; i++ {
		if i > 100 {
			t.Fatal("fixture did not reach plan", job.State)
		}
		next, d, _, e := engine.Prepare(job)
		if e != nil {
			t.Fatal(e)
		}
		if job = next; d != nil {
			result, e := executor.Execute(ctx, *d)
			if e != nil {
				t.Fatal(e)
			}
			if job, _, e = engine.Complete(job, d.ID, result); e != nil {
				t.Fatal(e)
			}
		}
	}
	snapshot := envelopeSnapshot(t, job)
	payload, _ := CanonicalJSON(snapshot.Payload)
	if _, e = s.PutBlob(ctx, audit(), payload); e != nil {
		t.Fatal(e)
	}
	if _, e = s.db.ExecContext(ctx, `INSERT INTO jobs (job_id,version,workflow_id,workflow_revision,target_id,target_revision,state,payload_digest,payload_size) VALUES (?,?,?,?,?,?,?,?,?)`, "legacy", 3, doc.ID, 1, "target", 1, string(job.State), Digest(payload), len(payload)); e != nil {
		t.Fatal(e)
	}
	if _, e = s.db.ExecContext(ctx, `INSERT INTO molecules (molecule_id,workflow_id,revision,content_digest,created_at) VALUES (?,?,?,?,UTC_TIMESTAMP(6))`, r.Hash, doc.ID, 1, r.Hash); e != nil {
		t.Fatal(e)
	}
	// A legacy job whose history predates the record rules (a state no
	// transition reaches) keeps its snapshot history and records onward.
	legacy, _ := engine.New("unrecorded", 1, doc, "tree")
	legacy.State = "spec"
	unrecorded := envelopeSnapshot(t, legacy)
	raw, _ := CanonicalJSON(unrecorded.Payload)
	if _, e = s.PutBlob(ctx, audit(), raw); e != nil {
		t.Fatal(e)
	}
	if _, e = s.db.ExecContext(ctx, `INSERT INTO jobs (job_id,version,workflow_id,workflow_revision,target_id,target_revision,state,payload_digest,payload_size) VALUES (?,?,?,?,?,?,?,?,?)`, "unrecorded", 2, doc.ID, 1, "target", 1, "spec", Digest(raw), len(raw)); e != nil {
		t.Fatal(e)
	}
	s.fault = func(step string) error {
		if step == "artifact" {
			return errors.New("interrupted backfill")
		}
		return nil
	}
	if e = s.Migrate(ctx); e == nil {
		t.Fatal("interrupted migration reported success")
	}
	s.fault = nil
	var version, tasks int
	if e = s.db.QueryRowContext(ctx, `SELECT MAX(version) FROM migrations`).Scan(&version); e != nil || version != 7 {
		t.Fatal("interrupted migration recorded its version", version, e)
	}
	if e = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks`).Scan(&tasks); e != nil || tasks != 0 {
		t.Fatal("interrupted backfill left task rows", tasks, e)
	}
	for i := 0; i < 2; i++ {
		if e = s.Migrate(ctx); e != nil {
			t.Fatal(i, e)
		}
	}
	if e = s.db.QueryRowContext(ctx, `SELECT MAX(version) FROM migrations`).Scan(&version); e != nil || version != SchemaVersion {
		t.Fatal(version, e)
	}
	got, e := s.GetJob(ctx, "legacy")
	want, _ := MoleculeID(doc.ID, 1, r.Hash)
	if e != nil || got.MoleculeID != want {
		t.Fatal("legacy job not bound to the resolved molecule", got.MoleculeID, want, e)
	}
	if m, e := s.GetMolecule(ctx, want); e != nil || m.EndedAt != nil {
		t.Fatal("colliding molecule identity not rekeyed", m, e)
	}
	c := countRecords(t, s, "legacy")
	if c.State != "plan" || c.Provenance != len(job.Provenance) || c.Transitions == 0 {
		t.Fatalf("backfill records %+v", c)
	}
	if u := countRecords(t, s, "unrecorded"); u.State != "spec" || u.Transitions != 0 {
		t.Fatalf("legacy job records %+v", u)
	}
	// The next checkpoint appends from the backfilled cursors only.
	next, _, events, e := engine.Prepare(job)
	if e != nil || len(events) == 0 {
		t.Fatal(e)
	}
	if _, e = s.SaveJob(ctx, audit(), envelopeSnapshot(t, next), got.Version); e != nil {
		t.Fatal(e)
	}
	if after := countRecords(t, s, "legacy"); after.Provenance != c.Provenance || after.Version != c.Version+1 {
		t.Fatal("checkpoint after backfill re-recorded provenance", c, after)
	}
}
