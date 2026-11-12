package controlplane

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/scheduler"
)

// FAE-CP-011: every record family table has a production writer, not just a
// CREATE. The SQL string literals of the package's non-test sources must
// insert into or update each family table.
func TestEveryRecordFamilyHasProductionWriter(t *testing.T) {
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var literals strings.Builder
	fset := token.NewFileSet()
	for _, path := range sources {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if v, err := strconv.Unquote(lit.Value); err == nil {
					literals.WriteString(v + "\n")
				}
			}
			return true
		})
	}
	create := regexp.MustCompile(`CREATE TABLE IF NOT EXISTS (\w+)`)
	families := 0
	for _, statement := range SchemaStatements {
		m := create.FindStringSubmatch(statement)
		if m == nil {
			t.Fatalf("schema statement creates no table: %s", statement)
		}
		families++
		writer := regexp.MustCompile(`(?:INSERT(?: IGNORE)? INTO|REPLACE INTO|UPDATE) ` + m[1] + `\b`)
		if !writer.MatchString(literals.String()) {
			t.Errorf("record family table %s has no production writer", m[1])
		}
	}
	if families != 23 {
		t.Fatalf("schema families %d", families)
	}
}

func familyCount(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if e := s.db.QueryRowContext(context.Background(), query, args...).Scan(&n); e != nil {
		t.Fatal(query, e)
	}
	return n
}

// Each derived family is written in the transaction of its authoritative
// write: leases with the lease ledger, the ticket outbox with its emissions,
// external calls with their effect checkpoints, schedules and reviews with
// their markers, sessions with the checkpoint's provenance; engine runs are
// recorded with their sanitized-environment delta (FAE-CP-011, FAE-EXT-006,
// FAE-ROB-030).
func TestRecordFamiliesWrittenByProductionPaths(t *testing.T) {
	s := testPlane(t, SchemaVersion)
	ctx := context.Background()
	a := Audit{Actor: "daemon", Action: "family-test", Outcome: "ok"}

	// Leases: a grant opens a row, a release closes it with its reason.
	if e := s.EnsureLeasePool(ctx, a, LeasePoolConfig{Key: "role/coder", Kind: "role", Capacity: 1}); e != nil {
		t.Fatal(e)
	}
	owner := scheduler.Owner{PID: 4242, Started: 7}
	now := time.Now().UTC()
	for i, id := range []string{"first", "second"} {
		if e := s.EnqueueLease(ctx, a, "role/coder", scheduler.Request{ID: id, Owner: owner, Weight: 1, Queued: now.Add(time.Duration(i) * time.Millisecond)}); e != nil {
			t.Fatal(e)
		}
	}
	if familyCount(t, s, `SELECT COUNT(*) FROM leases`) != 0 {
		t.Fatal("a queued request is not a lease")
	}
	if _, e := s.AdvanceLeases(ctx, a, "role/coder", now, map[scheduler.Owner]scheduler.Liveness{}); e != nil {
		t.Fatal(e)
	}
	if n := familyCount(t, s, `SELECT COUNT(*) FROM leases WHERE scope_key='role/coder' AND kind='role' AND owner='pid:4242/start:7' AND released_at IS NULL`); n != 1 {
		t.Fatal("granted lease not projected", n)
	}
	if e := s.ReleaseLease(ctx, a, "role/coder", "first", owner, "turn finished"); e != nil {
		t.Fatal(e)
	}
	if _, e := s.AdvanceLeases(ctx, a, "role/coder", now.Add(time.Second), map[scheduler.Owner]scheduler.Liveness{}); e != nil {
		t.Fatal(e)
	}
	if n := familyCount(t, s, `SELECT COUNT(*) FROM leases WHERE release_reason='turn finished' AND released_at IS NOT NULL`); n != 1 {
		t.Fatal("released lease not closed with its reason", n)
	}
	if n := familyCount(t, s, `SELECT COUNT(*) FROM leases WHERE released_at IS NULL`); n != 1 {
		t.Fatal("second grant not projected", n)
	}

	// Ticket outbox, and the ticket call as one external effect.
	ticket := TicketEmission{Key: strings.Repeat("c", 64), Task: "task-t", Room: "room", Step: "InProgress"}
	if e := s.EnqueueTicket(ctx, ticket); e != nil {
		t.Fatal(e)
	}
	if n := familyCount(t, s, `SELECT COUNT(*) FROM ticket_outbox WHERE task_id='task-t' AND desired_step='InProgress' AND NOT emitted`); n != 1 {
		t.Fatal("queued ticket not projected", n)
	}
	started := ticket
	started.Started = true
	started.Intent, _ = json.Marshal(map[string]any{"intent": map[string]any{"key": "ticket-call", "task": "task-t", "tool": "tickets.update", "arguments": map[string]string{"room": "room"}, "digest": strings.Repeat("d", 64), "session": "outbox/" + ticket.Key}})
	if e := s.SaveTicket(ctx, ticket, started); e != nil {
		t.Fatal(e)
	}
	emitted := started
	emitted.Emitted, emitted.TicketID = true, "TICKET-9"
	if e := s.SaveTicket(ctx, started, emitted); e != nil {
		t.Fatal(e)
	}
	if n := familyCount(t, s, `SELECT COUNT(*) FROM ticket_outbox WHERE ticket_id='TICKET-9' AND emitted AND started`); n != 1 {
		t.Fatal("emitted ticket not projected", n)
	}
	if n := familyCount(t, s, `SELECT COUNT(*) FROM external_effects WHERE effect_id='ticket-call' AND tool='tickets.update' AND external_id='TICKET-9' AND reconciliation='validated' AND outcome='emitted' AND session_id=?`, "outbox/"+ticket.Key); n != 1 {
		t.Fatal("ticket call not audited as one effect", n)
	}

	// A generic external call: intent, then the reported identity; one row.
	intent := map[string]any{"key": "review-call", "task": "task-e", "stage": "s", "tool": "reviews.create", "arguments": map[string]string{"draft": "true"}, "digest": strings.Repeat("e", 64), "session": "session-reviewer", "endpoint": "x", "server": "y", "tree": "t"}
	raw, _ := json.Marshal(intent)
	if e := s.RecordEffect(ctx, "effect/review/external-intent", "task-e", "intent", raw); e != nil {
		t.Fatal(e)
	}
	raw, _ = json.Marshal(map[string]any{"intent": intent, "reported": map[string]any{"key": "review-call", "external_id": "R-1", "state": "open"}, "reconciliation_state": "unreconciled"})
	for i := 0; i < 2; i++ { // an identical observation is one checkpoint
		if e := s.RecordEffect(ctx, "effect/review/observed/1", "task-e", "observed", raw); e != nil {
			t.Fatal(e)
		}
	}
	if n := familyCount(t, s, `SELECT COUNT(*) FROM external_effects WHERE task_id='task-e'`); n != 1 {
		t.Fatal("one external call must be exactly one effect row", n)
	}
	var pinned, session, external, reconciliation string
	if e := s.db.QueryRowContext(ctx, `SELECT pinned_values,session_id,external_id,reconciliation FROM external_effects WHERE effect_id='review-call'`).Scan(&pinned, &session, &external, &reconciliation); e != nil {
		t.Fatal(e)
	}
	if pinned != `{"draft":"true"}` || session != "session-reviewer" || external != "R-1" || reconciliation != "reported" {
		t.Fatal(pinned, session, external, reconciliation)
	}

	// Schedules follow their markers, including removal.
	cron := map[string]any{"nightly": map[string]any{"spec": map[string]any{"id": "nightly", "expression": "0 3 * * *"}, "state": "active", "authorizing_message": "m-1"}}
	raw, _ = json.Marshal(cron)
	if e := s.PutMarker(ctx, a, "cron-schedules", raw); e != nil {
		t.Fatal(e)
	}
	backup, _ := json.Marshal(map[string]any{"state": "enabled", "interval_seconds": 3600, "authorizing_message": "m-2", "run_count": 4})
	if e := s.PutMarker(ctx, a, "backup-creation-schedule", backup); e != nil {
		t.Fatal(e)
	}
	if n := familyCount(t, s, `SELECT COUNT(*) FROM schedules WHERE schedule_id='cron-schedules/nightly' AND kind='cron' AND state='active' AND provenance LIKE '%m-1%'`); n != 1 {
		t.Fatal("cron schedule not projected", n)
	}
	if n := familyCount(t, s, `SELECT COUNT(*) FROM schedules WHERE schedule_id='backup-creation-schedule' AND kind='interval' AND specification='{"interval_seconds":3600,"keep":null}'`); n != 1 {
		t.Fatal("interval schedule not projected", n)
	}
	if e := s.PutMarker(ctx, a, "cron-schedules", json.RawMessage(`{}`)); e != nil {
		t.Fatal(e)
	}
	if n := familyCount(t, s, `SELECT COUNT(*) FROM schedules WHERE kind='cron'`); n != 0 {
		t.Fatal("removed schedule still projected", n)
	}

	// A review lifecycle marker projects its review.
	review, _ := json.Marshal(map[string]any{"id": "R-1", "revision": 2, "published": true, "merged": false})
	if e := s.PutMarker(ctx, a, ReviewMarker("task-e"), review, CodeReview{ID: "R-1", Task: "task-e", Branch: "fae/task/x", Tree: "tree-1", Revisions: 2, Published: true}); e != nil {
		t.Fatal(e)
	}
	if n := familyCount(t, s, `SELECT COUNT(*) FROM code_reviews WHERE review_id='R-1' AND task_id='task-e' AND branch='fae/task/x' AND revisions=2 AND publish_state='published' AND merge_state='open'`); n != 1 {
		t.Fatal("review not projected", n)
	}
	// A later operation that names no branch keeps the recorded one.
	merged, _ := json.Marshal(map[string]any{"id": "R-1", "revision": 2, "published": true, "merged": true})
	if e := s.PutMarker(ctx, a, ReviewMarker("task-e"), merged, CodeReview{ID: "R-1", Task: "task-e", Revisions: 2, Published: true, Merged: true}); e != nil {
		t.Fatal(e)
	}
	if n := familyCount(t, s, `SELECT COUNT(*) FROM code_reviews WHERE review_id='R-1' AND branch='fae/task/x' AND tree_hash='tree-1' AND merge_state='merged'`); n != 1 {
		t.Fatal("merged review lost its branch", n)
	}

	// An engine run records names only; a value is refused.
	run := EngineRun{TaskID: "task-e", DispatchID: "d-1", Command: []string{"go", "test", "./..."}, Tree: "tree-1", Environment: EnvironmentDelta{Removed: []string{"FAE_HOME", "DOLT_ROOT_PATH"}, Set: []string{"GOFLAGS"}}, ExitCode: 0, ExecutedCount: 3, OutputDigest: strings.Repeat("f", 64), Started: now, Finished: now.Add(time.Second)}
	if _, e := s.RecordEngineRun(ctx, run); e != nil {
		t.Fatal(e)
	}
	bad := run
	bad.Environment.Set = []string{"TOKEN=secret"}
	if _, e := s.RecordEngineRun(ctx, bad); e == nil {
		t.Fatal("an environment value was accepted into the delta")
	}
	runs, e := s.EngineRuns(ctx, "task-e", 10)
	if e != nil || len(runs) != 1 || string(runs[0].Environment) != `{"removed":["DOLT_ROOT_PATH","FAE_HOME"],"set":["GOFLAGS"],"replaced":[]}` || runs[0].ExecutedCount != 3 {
		t.Fatal(runs, e)
	}

	// Sessions are projected from the checkpoint's provenance.
	s.options.OwnerPID, s.options.OwnerStart = 777, "boot:1"
	doc := defaults.Workflow()
	installWorkflow(t, s, doc)
	job, e := engine.New("task-s", 1, doc, "fixture-tree")
	if e != nil {
		t.Fatal(e)
	}
	saved, e := s.AdmitJob(ctx, audit(), "", envelopeSnapshot(t, job))
	if e != nil {
		t.Fatal(e)
	}
	executor := engine.NewScripted(defaults.HappyScript())
	for i := 0; i < 200 && !job.State.Terminal(); i++ {
		next, d, _, e := engine.Prepare(job)
		if e != nil {
			t.Fatal(e)
		}
		if saved, e = s.SaveJob(ctx, audit(), envelopeSnapshot(t, next), saved.Version); e != nil {
			t.Fatal(e)
		}
		job = next
		if d != nil {
			r, e := executor.Execute(ctx, *d)
			if e != nil {
				t.Fatal(e)
			}
			r.Transcript = &engine.Transcript{Pane: "pane-" + d.Stage, StartedAt: now}
			for n := range r.Reviews {
				r.Reviews[n].Transcript = r.Transcript
			}
			if next, _, e = engine.Complete(job, d.ID, r); e != nil {
				t.Fatal(e)
			}
			if saved, e = s.SaveJob(ctx, audit(), envelopeSnapshot(t, next), saved.Version); e != nil {
				t.Fatal(e)
			}
			job = next
		}
	}
	if job.State != domain.Done {
		t.Fatal(job.State, job.Reason)
	}
	named := map[string]bool{}
	for _, p := range job.Provenance {
		if p.Session != "" {
			named[p.Session] = true
		}
	}
	if n := familyCount(t, s, `SELECT COUNT(*) FROM sessions WHERE task_id='task-s' AND owner_pid=777 AND owner_start='boot:1' AND pane_id LIKE 'pane-%'`); n == 0 || n != len(named) {
		t.Fatalf("sessions %d, provenance names %d", n, len(named))
	}
	for _, table := range []string{"leases", "sessions", "external_effects", "code_reviews", "ticket_outbox", "schedules", "engine_runs"} {
		if familyCount(t, s, `SELECT COUNT(*) FROM `+table) == 0 {
			t.Errorf("family %s unwritten", table)
		}
	}
}

// Migration 9 projects the state that existed before it: open leases,
// emissions, schedule markers, effect checkpoints and provenance sessions.
func TestMigration9BackfillsFamilies(t *testing.T) {
	s := testPlane(t, 8)
	ctx := context.Background()
	a := Audit{Actor: "daemon", Action: "pre-9", Outcome: "ok"}
	if e := s.EnsureLeasePool(ctx, a, LeasePoolConfig{Key: "heavy", Kind: "heavy-op", Capacity: 2}); e != nil {
		t.Fatal(e)
	}
	// Written as a v8 binary would: the ledger, emissions and markers only.
	pool, _ := scheduler.NewPool(2)
	pool.Holders = []scheduler.Lease{{Request: scheduler.Request{ID: "h", Owner: scheduler.Owner{PID: 9, Started: 1}, Weight: 1, Queued: time.Now().UTC()}, Expires: time.Now().Add(time.Hour).UTC()}}
	raw, _ := json.Marshal(pool)
	if _, e := s.db.ExecContext(ctx, `UPDATE resource_pools SET payload=? WHERE pool_key='heavy'`, string(raw)); e != nil {
		t.Fatal(e)
	}
	emission, _ := json.Marshal(TicketEmission{Key: strings.Repeat("a", 64), Task: "t", Room: "r", Step: "Open", Attempts: 1})
	if _, e := s.db.ExecContext(ctx, `INSERT INTO ticket_emissions (outbox_key,task_id,room,position,payload) VALUES (?,?,?,?,?)`, strings.Repeat("a", 64), "t", "r", 0, string(emission)); e != nil {
		t.Fatal(e)
	}
	loops, _ := json.Marshal(map[string]any{"watch": map[string]any{"spec": map[string]any{"id": "watch"}, "state": "running", "session_id": "loop-session"}})
	canonical, _ := CanonicalJSON(loops)
	if _, e := s.PutBlob(ctx, a, canonical); e != nil {
		t.Fatal(e)
	}
	if _, e := s.db.ExecContext(ctx, `INSERT INTO daemon_markers (name,payload_digest,payload_size) VALUES ('provider-loops',?,?)`, Digest(canonical), len(canonical)); e != nil {
		t.Fatal(e)
	}
	if e := s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	for query, want := range map[string]int{
		`SELECT COUNT(*) FROM leases WHERE scope_key='heavy' AND kind='heavy-op' AND released_at IS NULL`: 1,
		`SELECT COUNT(*) FROM ticket_outbox WHERE task_id='t' AND attempts=1`:                             1,
		`SELECT COUNT(*) FROM schedules WHERE kind='loop' AND state='running'`:                            1,
		`SELECT COUNT(*) FROM migrations WHERE version=9`:                                                 1,
	} {
		if n := familyCount(t, s, query); n != want {
			t.Fatal(query, n)
		}
	}
	// Re-running is idempotent.
	if e := s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
}
