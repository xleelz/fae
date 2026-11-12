package execution

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// Engine feedback: a turn with failed runs among its inputs gets an
// engine_feedback block after the stage block and before operator_context;
// a turn without one gets no block.
func TestEngineFeedbackBlock(t *testing.T) {
	x, d, a := fixture(t)
	rec := &recorder{inner: x.Runtime}
	x.Runtime = rec
	var feedback []FeedbackRun
	var seen engine.Dispatch
	var seenDoc workflow.Document
	x.Feedback = func(_ context.Context, d engine.Dispatch, doc workflow.Document) ([]FeedbackRun, error) {
		seen, seenDoc = d, doc
		return feedback, nil
	}
	if _, err := x.Execute(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	kinds := func(raw json.RawMessage) []string {
		var blocks []struct{ Kind string }
		if err := json.Unmarshal(raw, &blocks); err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, b := range blocks {
			out = append(out, b.Kind)
		}
		return out
	}
	if got := strings.Join(kinds(rec.briefs[0]), ","); got != "role_instructions,stage,task_scope,operator_context,kernel_contract" || seen.ID != d.ID || seenDoc.Roles["coder"].Instructions != "instructions" {
		t.Fatal("brief without failed runs", got, string(rec.briefs[0]))
	}
	d.ID, d.Sessions = "dispatch-2", []engine.Session{{ID: "two", Role: "coder"}}
	feedback = []FeedbackRun{{Stage: "run-tests", DispatchID: "task/hash/7", Command: []string{"go", "test", "-json", "-count=1", "./..."}, ExitCode: 1, ExecutedCount: 3, MatchedCount: 1, Excerpt: "answer.go:3:9: undefined: x\n\nexit code 1, executed 3, matched 1\n"}}
	if _, err := x.Execute(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(kinds(rec.briefs[1]), ","); got != "role_instructions,stage,task_scope,engine_feedback,operator_context,kernel_contract" {
		t.Fatal("brief with a failed run", got)
	}
	var blocks []json.RawMessage
	_ = json.Unmarshal(rec.briefs[1], &blocks)
	var block struct {
		Kind string        `json:"kind"`
		Runs []FeedbackRun `json:"runs"`
	}
	if err := json.Unmarshal(blocks[3], &block); err != nil || len(block.Runs) != 1 || block.Runs[0].Excerpt != feedback[0].Excerpt || block.Runs[0].DispatchID != "task/hash/7" || block.Runs[0].ExitCode != 1 || block.Runs[0].MatchedCount != 1 || block.Runs[0].ExcerptTruncated {
		t.Fatal(err, string(blocks[3]))
	}
	for _, field := range []string{`"stage":"run-tests"`, `"dispatch_id":"task/hash/7"`, `"command":["go","test","-json","-count=1","./..."]`, `"exit_code":1`, `"executed_count":3`, `"matched_count":1`, `"excerpt":"answer.go:3:9: undefined: x\n\nexit code 1, executed 3, matched 1\n"`, `"excerpt_truncated":false`} {
		if !strings.Contains(string(blocks[3]), field) {
			t.Fatal("engine_feedback lacks", field, string(blocks[3]))
		}
	}
	// A failed store read fails the dispatch before launch.
	x.Feedback = func(context.Context, engine.Dispatch, workflow.Document) ([]FeedbackRun, error) {
		return nil, errors.New("store unavailable")
	}
	d.ID, d.Sessions = "dispatch-3", []engine.Session{{ID: "three", Role: "coder"}}
	acquired := a.acquired
	if _, err := x.Execute(context.Background(), d); err == nil || a.acquired != acquired || len(rec.briefs) != 2 {
		t.Fatal("launched without its feedback", err)
	}
}

// Engine feedback bounds: 16 KiB per run and 48 KiB per brief, cuts flagged,
// short excerpts kept whole.
func TestEngineFeedbackBounds(t *testing.T) {
	long := strings.Repeat("ü line of output\n", 2000)
	runs := []FeedbackRun{{Stage: "a", Excerpt: "short\n"}, {Stage: "b", Excerpt: long}, {Stage: "c", Excerpt: long}, {Stage: "d", Excerpt: long}, {Stage: "e", Excerpt: long}}
	out := boundFeedback(runs)
	total := 0
	for i, r := range out {
		total += len(r.Excerpt)
		if len(r.Excerpt) > 16<<10 || !utf8.ValidString(r.Excerpt) || r.Stage != runs[i].Stage {
			t.Fatal("run over its bound", i, len(r.Excerpt))
		}
		if i > 0 && (!r.ExcerptTruncated || !strings.Contains(r.Excerpt, "bytes cut ...]") || len(r.Excerpt) < 11<<10) {
			t.Fatal("long run not cut fairly", i, len(r.Excerpt), r.ExcerptTruncated)
		}
	}
	if total > FeedbackTotalLimit || out[0].Excerpt != "short\n" || out[0].ExcerptTruncated || runs[1].Excerpt != long {
		t.Fatal("brief over its bound", total)
	}
	one := boundFeedback([]FeedbackRun{{Excerpt: long}})
	if len(one[0].Excerpt) > 16<<10 || !one[0].ExcerptTruncated {
		t.Fatal("per-run bound", len(one[0].Excerpt))
	}
	if engineFeedbackBlock(nil) != nil {
		t.Fatal("empty feedback produced a block")
	}
}
