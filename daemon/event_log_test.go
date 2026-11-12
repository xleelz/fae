package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
)

// FAE-OBS-006 acceptance: over a fault-injected scripted run, every park and
// escalation event row has exactly one structured log line and every such line
// has its row. Covers the drive checkpoint (including a failed, uncommitted
// save) and an operator escalation of an in-flight task.
func TestParkAndEscalationLogLinesPairWithEvents(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.Executor = engine.NewScripted(map[string][]engine.ScriptStep{
		"esc/work": {{Output: json.RawMessage(`{"ok":false}`)}},
		"unc/work": {{Output: json.RawMessage(`{"ok":true}`), Class: "uncertain"}},
		"op/work":  {{Output: json.RawMessage(`{"ok":true}`), Delay: 3 * time.Second}},
	})
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			stop(t, d)
		}
	}()
	failed := false
	d.mu.Lock()
	d.testFault = func(point, id string) error {
		if point == "save" && id == "esc" && !failed {
			failed = true
			return &controlplane.RetryableError{Attempts: 5, Cause: errors.New("injected uncommitted save")}
		}
		return nil
	}
	d.mu.Unlock()
	authorize(t, d)
	doc := shortWorkflow()
	if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"esc", "unc", "op"} {
		if _, err = call(t, d, "Admit", AdmitParams{ID: id, WorkflowID: doc.ID, Tree: "tree", AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
	}
	awaitInFlight(t, d, "op", "work")
	if _, err = call(t, d, "Escalate", TaskControlParams{ID: "op", Reason: "operator hold", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "esc", "Escalated")
	deadline := time.Now().Add(time.Minute)
	for {
		s, _ := d.store.GetJob(ctx, "unc")
		var e envelope
		_ = json.Unmarshal(s.Payload, &e)
		if e.Job.Parked && e.Job.Reason == "UncertainDispatch" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("uncertain task not parked", string(s.Payload))
		}
		time.Sleep(30 * time.Millisecond)
	}
	for {
		s, _ := d.store.GetJob(ctx, "op")
		var e envelope
		_ = json.Unmarshal(s.Payload, &e)
		if e.Job.InFlight == nil && e.Job.State == domain.Escalated {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("held task completion not collected", string(s.Payload))
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !failed {
		t.Fatal("fault not injected")
	}
	events, err := d.store.Events(context.Background(), 0, 100000)
	if err != nil {
		t.Fatal(err)
	}
	var want, got []string
	kinds := map[string]bool{}
	for _, e := range events {
		var ev engine.Event
		_ = json.Unmarshal(e.Payload, &ev)
		if ev.Kind == "park" || ev.Kind == "escalation" || ev.Kind == "transition" && ev.To == domain.Escalated {
			line, ok := pairedEventLine(e)
			if !ok || !strings.Contains(line, `task="`+e.JobID+`"`) {
				t.Fatalf("event %s has no paired line", e.Payload)
			}
			want = append(want, strings.TrimSuffix(line, "\n"))
			kinds[ev.Kind+"/"+string(ev.EscalationKind)+"/"+ev.Cause] = true
		}
	}
	raw, err := os.ReadFile(d.layout.Log)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(raw), "\n") {
		if _, text, ok := strings.Cut(l, " component="+pairedEventComponent+" "); ok {
			got = append(got, text)
		}
	}
	stop(t, d)
	stopped = true
	sort.Strings(want)
	sort.Strings(got)
	if strings.Join(want, "\n") != strings.Join(got, "\n") {
		t.Fatalf("log lines and event rows differ\nrows:\n%s\nlines:\n%s", strings.Join(want, "\n"), strings.Join(got, "\n"))
	}
	if !kinds["park//UncertainDispatch"] {
		t.Fatalf("scripted run lacks a park: %v", kinds)
	}
	escalations := 0
	for _, w := range want {
		if strings.HasPrefix(w, "escalation kind=HumanDecisionRequired") || strings.HasPrefix(w, "escalation kind=OperatorEscalate") {
			escalations++
		}
	}
	if escalations < 2 {
		t.Fatalf("expected drive and operator escalations: %v", want)
	}
}
