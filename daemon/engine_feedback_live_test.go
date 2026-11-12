package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	agentruntime "github.com/xleelz/fae/runtime"
)

// TestLiveFeedbackBridgeFixture wraps the live protocol fixture: it logs
// every brief, and its first two implement turns leave the tree failing (a
// compile error, then a formatting fault) so later turns must see why.
func TestLiveFeedbackBridgeFixture(t *testing.T) {
	if os.Getenv("LIVE_FEEDBACK_BRIDGE") != "1" {
		return
	}
	var c agentruntime.BridgeConfig
	raw, err := os.ReadFile(os.Args[len(os.Args)-1])
	if err != nil || json.Unmarshal(raw, &c) != nil {
		os.Exit(31)
	}
	var blocks []struct{ Kind, Stage string }
	if json.Unmarshal(c.Brief, &blocks) != nil {
		os.Exit(32)
	}
	stage := ""
	for _, b := range blocks {
		if b.Kind == "stage" {
			stage = b.Stage
		}
	}
	log := os.Getenv("LIVE_FEEDBACK_LOG")
	prior, _ := filepath.Glob(filepath.Join(log, stage+"-*.json"))
	visit := len(prior) + 1
	if os.WriteFile(filepath.Join(log, fmt.Sprintf("%s-%d.json", stage, visit)), c.Brief, 0600) != nil {
		os.Exit(33)
	}
	faults := map[int]string{1: "package sample\n\nfunc answer() int { return undefinedAnswer }\n", 2: "package sample\n\nfunc answer() int {   return 42 }\n"}
	if stage != "implement" || faults[visit] == "" {
		TestLiveDaemonBridgeFixture(t) // exits
	}
	var fields map[string]any
	if json.Unmarshal(defaults.HappyScript()["implement"][0].Output, &fields) != nil {
		os.Exit(34)
	}
	fields["source"] = liveTestSource
	output, _ := json.Marshal(fields)
	if os.WriteFile(filepath.Join(c.Worktree, "answer.go"), []byte(faults[visit]), 0600) != nil {
		os.Exit(35)
	}
	out, _ := json.Marshal(agentruntime.Handoff{DispatchID: c.DispatchID, SessionID: c.SessionID, TaskID: c.TaskID, Output: output})
	if os.WriteFile(c.Handoff, out, 0600) != nil {
		os.Exit(36)
	}
	os.Exit(0)
}

// Engine feedback (Appendix C steps 5, 9 and 16): a failed run's excerpt is
// stored by reference before its result is recorded, and the rework turn the
// failure routes back to gets it in an engine_feedback block: the compiler
// error after a failed green run, the listed file after failed formatting.
// Turns whose input runs all passed get no block.
func TestLiveEngineFeedbackReachesReworkTurn(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	log := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Live.Command = []string{exe, "-test.run=^TestLiveFeedbackBridgeFixture$", "--", "{config}"}
	cfg.Live.Environment = map[string]string{"LIVE_DAEMON_BRIDGE": "1", "LIVE_FEEDBACK_BRIDGE": "1", "LIVE_FEEDBACK_LOG": log}
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "feedback-one", AuthorizingMessage: "message"}, "feedback-one"); err != nil {
		t.Fatal(err)
	}
	snapshot := awaitState(t, d, "feedback-one", "Done")
	var e envelope
	if err = decodeEnvelope(snapshot.Payload, &e); err != nil {
		t.Fatal(err)
	}

	// The three failed runs (the RED, the broken green run, the bad
	// formatting) each stored one excerpt by reference, bound to its
	// artifact; the passing runs stored none.
	var names []string
	err = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "inspect feedback", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT name FROM daemon_markers WHERE name LIKE 'engine-feedback/%' ORDER BY name`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err = rows.Scan(&name); err != nil {
				return err
			}
			names = append(names, name)
		}
		return rows.Err()
	})
	if err != nil || len(names) != 3 {
		t.Fatal("feedback records", names, err)
	}
	stored := map[string]string{}
	for _, name := range names {
		raw, err := d.store.GetMarker(ctx, name)
		var record struct {
			Artifact, Stage string
			DispatchID      string               `json:"dispatch_id"`
			Excerpt         controlplane.BlobRef `json:"excerpt"`
		}
		if err != nil || json.Unmarshal(raw, &record) != nil || record.Excerpt.Size < 1 || record.Excerpt.Size > 16<<10 {
			t.Fatal("feedback record", string(raw), err)
		}
		bound := false
		for _, a := range e.Job.Evidence {
			bound = bound || a.ID == record.Artifact && a.Stage == record.Stage && strings.HasPrefix(a.Session, record.DispatchID+"/session/")
		}
		text, err := d.store.GetBlob(ctx, record.Excerpt)
		if err != nil || !bound || stored[record.Stage] != "" {
			t.Fatal("feedback record not bound to one failed run", string(raw), err)
		}
		stored[record.Stage] = string(text)
	}
	if !strings.Contains(stored["run-red"], "got 0 want 42") || !strings.Contains(stored["run-tests"], "undefined: undefinedAnswer") || !strings.Contains(stored["format"], "answer.go") {
		t.Fatalf("stored excerpts %q", stored)
	}

	// Only the two rework implement turns carry engine_feedback.
	logged, err := filepath.Glob(filepath.Join(log, "*.json"))
	if err != nil || len(logged) < 10 {
		t.Fatal("briefs not logged", logged, err)
	}
	type feedbackRun struct {
		Stage            string   `json:"stage"`
		DispatchID       string   `json:"dispatch_id"`
		Command          []string `json:"command"`
		ExitCode         int      `json:"exit_code"`
		ExecutedCount    int      `json:"executed_count"`
		MatchedCount     int      `json:"matched_count"`
		Excerpt          string   `json:"excerpt"`
		ExcerptTruncated bool     `json:"excerpt_truncated"`
	}
	with := map[string][]feedbackRun{}
	for _, path := range logged {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var blocks []json.RawMessage
		if err = json.Unmarshal(raw, &blocks); err != nil {
			t.Fatal(err)
		}
		kinds := []string{}
		for _, b := range blocks {
			var block struct {
				Kind string        `json:"kind"`
				Runs []feedbackRun `json:"runs"`
			}
			if err = json.Unmarshal(b, &block); err != nil {
				t.Fatal(err)
			}
			kinds = append(kinds, block.Kind)
			if block.Kind == "engine_feedback" {
				with[filepath.Base(path)] = block.Runs
			}
		}
		if _, ok := with[filepath.Base(path)]; ok && strings.Join(kinds, ",") != "role_instructions,stage,task_scope,engine_feedback,operator_context,kernel_contract" {
			t.Fatal("engine_feedback out of order", kinds)
		}
	}
	if len(with) != 2 || len(with["implement-2.json"]) != 1 || len(with["implement-3.json"]) != 1 {
		t.Fatalf("engine_feedback in %v", with)
	}
	green, format := with["implement-2.json"][0], with["implement-3.json"][0]
	if green.Stage != "run-tests" || green.ExitCode == 0 || green.ExecutedCount != 0 || green.Excerpt != stored["run-tests"] || green.ExcerptTruncated || green.Command[0] != "go" || !strings.HasPrefix(green.DispatchID, "feedback-one/") {
		t.Fatalf("green feedback %+v", green)
	}
	for _, want := range []string{"# example.com/live", "answer.go:3:28: undefined: undefinedAnswer", "FAIL\texample.com/live [build failed]", "exit code 1, executed 0, matched 0"} {
		if !strings.Contains(green.Excerpt, want) {
			t.Fatalf("green excerpt lacks %q:\n%s", want, green.Excerpt)
		}
	}
	if strings.Contains(green.Excerpt, "WORK=") || strings.Contains(green.Excerpt, "/compile ") {
		t.Fatal("green excerpt kept the -x trace", green.Excerpt)
	}
	if format.Stage != "format" || format.Excerpt != "gofmt -l lists files whose formatting differs:\nanswer.go\n\nexit code 0, 1 file listed\n" || format.Command[0] != "gofmt" || format.DispatchID == green.DispatchID {
		t.Fatalf("format feedback %+v", format)
	}
}
