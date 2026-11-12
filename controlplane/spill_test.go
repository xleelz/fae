package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
)

// FAE-CP-012: no row column holds more than the inline limit. An oversized
// audit input, event payload, transition note, escalation reason and
// decision rationale are stored as content-addressed blobs and the column
// holds the reference, which resolves to the exact content.
func TestOversizedPayloadsSpillToReferences(t *testing.T) {
	s := testPlane(t, SchemaVersion)
	ctx := context.Background()
	big := strings.Repeat("remediation ", 100000) // ~1.2 MB
	inputs, _ := json.Marshal(map[string]string{"remediation": big})
	if e := s.Mutate(ctx, Audit{Actor: "operator", Action: "large-input", Inputs: inputs, Outcome: "ok"}, func(context.Context, *sql.Tx) error { return nil }); e != nil {
		t.Fatal(e)
	}
	resolveColumn := func(query string) []byte {
		t.Helper()
		var text string
		if e := s.db.QueryRowContext(ctx, query).Scan(&text); e != nil {
			t.Fatal(query, e)
		}
		if len(text) > 65535 {
			t.Fatalf("column holds %d bytes inline", len(text))
		}
		raw, e := s.ResolveText(ctx, text)
		if e != nil {
			t.Fatal(e)
		}
		return raw
	}
	canonical, _ := CanonicalJSON(inputs)
	if got := resolveColumn(`SELECT inputs FROM audit WHERE action='large-input'`); string(got) != string(canonical) {
		t.Fatal("audit input did not round-trip")
	}
	doc := defaults.Workflow()
	installWorkflow(t, s, doc)
	job, e := engine.New("spill", 1, doc, "tree")
	if e != nil {
		t.Fatal(e)
	}
	saved, e := s.AdmitJob(ctx, audit(), "", envelopeSnapshot(t, job))
	if e != nil {
		t.Fatal(e)
	}
	// An operator escalation whose cause carries the whole remediation.
	next := job
	next.History = append(append([]engine.Event{}, job.History...), engine.Event{Kind: "transition", From: domain.Pending, To: domain.Escalated, Cause: "OperatorEscalate: " + big, EscalationKind: domain.HumanDecisionRequired})
	next.State, next.Reason, next.EscalationKind = domain.Escalated, big, domain.HumanDecisionRequired
	proposal := envelopeSnapshot(t, next)
	event, _ := json.Marshal(map[string]string{"cause": big})
	proposal.Events = []Event{{Kind: "large-event", Payload: event}}
	if _, e = s.SaveJob(ctx, audit(), proposal, saved.Version); e != nil {
		t.Fatal(e)
	}
	eventCanonical, _ := CanonicalJSON(event)
	var wrapped storedTaskEvent
	if err := json.Unmarshal(resolveColumn(`SELECT payload FROM events WHERE kind='large-event'`), &wrapped); err != nil || wrapped.Version != 1 {
		t.Fatal("event metadata wrapper missing", err)
	}
	public, err := s.LastJobEvent(ctx, "spill", "large-event")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := blobRef(string(public.Payload)); !ok || len(public.Payload) > s.options.InlineLimit {
		t.Fatal("oversized public event expanded", len(public.Payload))
	}
	if got, err := s.ResolveText(ctx, string(public.Payload)); err != nil || string(got) != string(eventCanonical) {
		t.Fatal("event did not round-trip", err)
	}
	if got := resolveColumn(`SELECT note FROM transitions WHERE task_id='spill' AND to_state='Escalated'`); string(got) != "OperatorEscalate: "+big {
		t.Fatal("transition note did not round-trip")
	}
	if got := resolveColumn(`SELECT escalation_reason FROM tasks WHERE task_id='spill'`); string(got) != big {
		t.Fatal("escalation reason did not round-trip")
	}
	if got := resolveColumn(`SELECT rationale FROM decisions WHERE task_id='spill' AND kind='escalation'`); string(got) != "OperatorEscalate: "+big {
		t.Fatal("decision rationale did not round-trip")
	}
}
