package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
)

func TestBoardBoundedSortedCachedPagination(t *testing.T) {
	d := &Daemon{jobs: map[string]controlplane.JobSnapshot{}, reads: make(chan struct{}, 1)}
	for i := 802; i >= 0; i-- {
		id := fmt.Sprintf("task-%04d", i)
		payload, _ := json.Marshal(envelope{Job: engine.Job{Reason: "waiting"}, Admission: AdmitParams{Priority: 10, Mode: "Local", RunID: "run", Content: strings.Repeat("not in board", 2000)}})
		d.jobs[id] = controlplane.JobSnapshot{ID: id, Version: 7, WorkflowID: "workflow", WorkflowRevision: 2, TargetID: "/workspace", TargetRevision: 1, State: "Pending", Payload: payload}
	}
	d.publishStatus()
	// A saturated SQL read pool and the writer lock cannot block cached Board.
	d.reads <- struct{}{}
	d.mu.Lock()
	defer d.mu.Unlock()
	after := ""
	seen := map[string]bool{}
	last := ""
	for {
		raw, _ := json.Marshal(BoardParams{After: after, Limit: 73})
		result, err := d.handle(context.Background(), api.Request{Method: "Board", Params: raw})
		if err != nil {
			t.Fatal(err)
		}
		page := result.(BoardPage)
		if len(page.Items) > 73 || !fits(page) {
			t.Fatal("page bound exceeded")
		}
		encoded, _ := json.Marshal(page)
		if strings.Contains(string(encoded), "not in board") || strings.Contains(string(encoded), "payload") {
			t.Fatal("board leaked full payload")
		}
		for _, item := range page.Items {
			if item.ID <= last || seen[item.ID] {
				t.Fatal("duplicate or unordered task", item.ID)
			}
			seen[item.ID] = true
			last = item.ID
			if item.Priority != 10 || item.WorkflowRevision != 2 {
				t.Fatal("lost metadata")
			}
		}
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor != last {
			t.Fatal("cursor differs from last item")
		}
		after = page.NextCursor
	}
	if len(seen) != 803 {
		t.Fatalf("lost tasks: %d", len(seen))
	}
	for _, limit := range []int{-1, 501} {
		raw, _ := json.Marshal(BoardParams{Limit: limit})
		if _, err := d.board(raw); err == nil {
			t.Fatal("bad limit accepted")
		}
	}
	// Published snapshots are immutable until the next writer publication.
	delete(d.jobs, "task-0000")
	page, err := d.board(nil)
	if err != nil || page.Items[0].ID != "task-0000" || len(page.Items) != 100 {
		t.Fatal("cache mutated or default bound incorrect")
	}
}
func TestBoardShrinksToEncodedBudget(t *testing.T) {
	d := &Daemon{}
	s := statusSnapshot{Updated: time.Now()}
	for i := 0; i < 20; i++ {
		s.Tasks = append(s.Tasks, BoardItem{JobIdentity: JobIdentity{ID: fmt.Sprintf("%02d", i)}, Reason: strings.Repeat("x", 100000)})
	}
	d.status.Store(s)
	page, err := d.board(json.RawMessage(`{"limit":20}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) >= 20 || len(page.Items) == 0 || page.NextCursor == "" || !fits(page) {
		t.Fatal("encoded bound not enforced")
	}
}
func TestHistoryPageReferencesFollowCompletionCursor(t *testing.T) {
	events := []controlplane.Event{}
	refs := []engine.Provenance{}
	for i := 1; i <= 11; i++ {
		token := fmt.Sprintf("dispatch-%d", i)
		payload, _ := json.Marshal(engine.Event{Kind: "transition", Cause: "completed " + token})
		events = append(events, controlplane.Event{Sequence: int64(i), Payload: payload})
		refs = append(refs, engine.Provenance{ArtifactID: fmt.Sprintf("artifact-%d", i), DispatchID: token, Stage: "Build", Tree: "tree"})
	}
	snapshot := controlplane.JobSnapshot{ID: "task", Payload: json.RawMessage(`{"document":"must not return"}`)}
	first, err := historyPage(snapshot, refs, events[:6], 5)
	if err != nil {
		t.Fatal(err)
	}
	if first.NextCursor != 5 || len(first.Events) != 5 || len(first.ArtifactReferences) != 5 {
		t.Fatal("incorrect first page")
	}
	second, err := historyPage(snapshot, refs, events[5:], 6)
	if err != nil {
		t.Fatal(err)
	}
	if second.NextCursor != 0 || len(second.ArtifactReferences) != 6 {
		t.Fatal("incorrect final page")
	}
	seen := map[string]bool{}
	for _, page := range []HistoryPage{first, second} {
		encoded, _ := json.Marshal(page)
		if strings.Contains(string(encoded), "must not return") {
			t.Fatal("returned workflow body")
		}
		for _, r := range page.ArtifactReferences {
			if seen[r.ArtifactID] {
				t.Fatal("duplicate reference")
			}
			seen[r.ArtifactID] = true
		}
	}
	if len(seen) != 11 {
		t.Fatal("missing reference")
	}
}
func TestHistoryShrinksOrRefusesOversizedEvent(t *testing.T) {
	payload, _ := json.Marshal(map[string]string{"data": strings.Repeat("x", 100000)})
	events := []controlplane.Event{}
	for i := 0; i < 20; i++ {
		events = append(events, controlplane.Event{Sequence: int64(i + 1), Payload: payload})
	}
	page, err := historyPage(controlplane.JobSnapshot{ID: "task"}, nil, events, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) >= 20 || page.NextCursor == 0 || !fits(page) {
		t.Fatal("history size bound missing")
	}
	huge, _ := json.Marshal(strings.Repeat("x", readPageBytes))
	_, err = historyPage(controlplane.JobSnapshot{ID: "task"}, nil, []controlplane.Event{{Payload: huge}}, 1)
	var typed *api.RPCError
	if !errors.As(err, &typed) || typed.Code != "response-too-large" {
		t.Fatalf("missing typed bound: %v", err)
	}
}

func TestLegacyHistoryRefusesOversizedPayload(t *testing.T) {
	small := controlplane.JobSnapshot{ID: "small", Payload: json.RawMessage(`{"job":{}}`)}
	result, err := legacyHistory(small)
	if err != nil || result.(controlplane.JobSnapshot).ID != "small" {
		t.Fatal("small legacy response changed")
	}
	large, _ := json.Marshal(strings.Repeat("x", readPageBytes))
	_, err = legacyHistory(controlplane.JobSnapshot{ID: "large", Payload: large})
	var typed *api.RPCError
	if !errors.As(err, &typed) || typed.Code != "response-too-large" || !strings.Contains(typed.Message, "summary=true") {
		t.Fatalf("missing actionable typed refusal: %v", err)
	}
}
