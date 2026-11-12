package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/processowner"
	"github.com/xleelz/fae/workflow"
)

// Resident Herdr sessions on the production drive: the shipped default's
// non-review turns of one role and scope reuse one resident pane
// (runtime.ResidentMultiplexer.Run -> CanReuseResident), so a full Local run
// keeps fewer resident panes than it ran resident turns.
func TestDaemonResidentSessionsReusePanes(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	herdr := testHerdr(t)
	cfg.Live.Adapter = "multiplexer"
	cfg.Live.HerdrBinary = herdr
	cfg.Live.ResidentSessions = true
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "resident-one", AuthorizingMessage: "message"}, "resident-one"); err != nil {
		t.Fatal(err)
	}
	snapshot := awaitMultiplexerState(t, d, "resident-one", "Done")
	var env envelope
	if err = json.Unmarshal(snapshot.Payload, &env); err != nil {
		t.Fatal(err)
	}
	turns := 0
	for _, e := range env.Job.History {
		if s, ok := env.Job.Document.Stages[e.Stage]; e.Kind == "dispatch" && ok && s.Primitive == workflow.AgentTurn {
			turns++
		}
	}
	panes := d.idleSessions.Count()
	if env.Job.State != domain.Done || panes < 1 || panes >= turns {
		t.Fatalf("resident turns did not reuse panes: %d panes for %d resident turns", panes, turns)
	}
	// FAE-RT-011: every task-bound transport retired with its collected turn;
	// only workflow-global residents keep live owned processes.
	sessions := filepath.Join(d.layout.Root, "sessions")
	audits, _ := filepath.Glob(filepath.Join(sessions, "session-*", "resident-session.json"))
	taskBound := map[string]bool{}
	for _, path := range audits {
		var audit struct {
			Resident  string `json:"resident_id"`
			TaskBound bool   `json:"task_bound"`
		}
		if raw, err := os.ReadFile(path); err != nil || json.Unmarshal(raw, &audit) != nil {
			t.Fatal(path, err)
		}
		if audit.TaskBound {
			taskBound[audit.Resident] = true
		}
	}
	live, err := processowner.Inventory(filepath.Join(sessions, ".processes"))
	if err != nil || len(taskBound) == 0 {
		t.Fatal("no task-bound resident turn audited", err)
	}
	for _, record := range live {
		if taskBound[record.Session] {
			t.Fatalf("task-bound resident %s outlived its collected turn: %+v", record.Session, record)
		}
	}
}
