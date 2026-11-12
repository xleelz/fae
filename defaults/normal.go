package defaults

import (
	"encoding/json"
	"sort"

	"github.com/xleelz/fae/workflow"
)

// Generation orders shipped default contents. Bump it whenever Shipped()
// changes, so a daemon upgrades instances that hold an older builtin
// revision; TestShippedGenerationPinsContent fails until it is bumped and the
// new content hash is appended to testdata/shipped-generation.json and pinned
// in shippedGenerations (normal_test.go). A recorded hash is never edited.
// Generation 3 adds the request judge and the no-red route (schema 2).
// Generation 4 accepts Rust, Python and Node test shapes in RED reports.
const Generation = 4

// Subjects of the Normal code-review sub-workflow. A Normal task binds each
// to the target configuration's installed tool of that alias (TargetPins);
// the review subjects are also the review lifecycle operations.
const (
	TicketCreate   = "ticket_create"
	TicketUpdate   = "ticket_update"
	ReviewCreate   = "review_create"
	ReviewRevision = "review_revision"
	ReviewRead     = "review_read"
	ReviewPublish  = "review_publish"
	ReviewMerge    = "review_merge"
)

// NormalSubjects lists every subject a Normal default task binds.
func NormalSubjects() []string {
	return []string{TicketCreate, TicketUpdate, ReviewCreate, ReviewRevision, ReviewRead, ReviewPublish, ReviewMerge}
}

// NormalStages is the Normal sub-workflow's stage order in the normal case.
var NormalStages = []string{"ticket-open", "ticket-in-review", "review-create", "review-read", "review-publish", "review-merge", "ticket-closed"}

// Shipped is the default workflow content installed at first start
// (FAE-WF-080): Workflow() followed, in Normal mode only, by the code-review
// sub-workflow of Appendix C step 20. After integration a Normal task confirms
// its ticket and moves it to review, creates the review as a draft from the
// pre-provisioned workspace binding (FAE-EXT-010, -015; the adapter pushes the
// pinned branch to the push namespace), polls its status (the bounded
// ReviewPending observation), remediates on the same review through a bounded
// loop into the implementing role (FAE-EXT-012), publishes and merges as
// separate effects (FAE-EXT-011) and closes the ticket before Done
// (FAE-EXT-013). Every added stage is Normal-only: Local tasks pin
// workflow.LocalProjection(Shipped()), which is Workflow() byte for byte.
func Shipped() workflow.Document {
	d := Workflow()
	receipt := map[string]any{"tool": typ("string"), "argument_digest": typ("string"), "external_id": typ("string"), "reconciliation_state": typ("string"), "idempotency_key": typ("string")}
	r := artifact(receipt)
	var s map[string]any
	_ = json.Unmarshal(r.Schema, &s)
	props := s["properties"].(map[string]any)
	for key, t := range map[string]string{"review_id": "string", "review_revision": "integer", "review_merged": "boolean", "review_decision": "string", "review_commit": "string", "review_status": "object"} {
		props[key] = typ(t)
	}
	r.Schema, _ = json.Marshal(s)
	d.Artifacts["ExternalReceipt"] = r
	planner := d.Roles["planner"]
	planner.Outputs = append(planner.Outputs, "ExternalReceipt")
	sort.Strings(planner.Outputs)
	// The planner holds the effect grants the Normal stages act under; its
	// agent sessions still have no external-effect tool (FAE-WF-042).
	planner.Profile.Effects = []string{ReviewCreate, ReviewMerge, ReviewPublish, ReviewRead, ReviewRevision, TicketCreate, TicketUpdate}
	d.Roles["planner"] = planner
	effect := func(subject, failure string) workflow.Stage {
		return workflow.Stage{Primitive: workflow.ExternalEffect, Role: "planner", NormalOnly: true, Subject: subject, Pins: workflow.TargetPins(), Failure: failure, Output: "ExternalReceipt"}
	}
	stages := []struct {
		id      string
		stage   workflow.Stage
		success string
	}{
		{"ticket-open", effect(TicketCreate, ""), "ticket-in-review"},
		{"ticket-in-review", effect(TicketUpdate, ""), "review-create"},
		// An existing review is never recreated: the engine routes it to a
		// new revision on the same review identity.
		{"review-create", effect(ReviewCreate, "review-revision"), "review-read"},
		{"review-revision", effect(ReviewRevision, ""), "review-read"},
		{"review-read", effect(ReviewRead, "review-remediation-loop"), "review-publish"},
		{"review-remediation-loop", workflow.Stage{Primitive: workflow.Loop, MaxIterations: 5, NormalOnly: true}, "implement"},
		{"review-publish", effect(ReviewPublish, ""), "review-merge"},
		{"review-merge", effect(ReviewMerge, ""), "ticket-closed"},
		{"ticket-closed", effect(TicketUpdate, ""), "Done"},
	}
	for _, x := range stages {
		if x.stage.Primitive == workflow.ExternalEffect {
			x.stage.IdempotenceKey = x.id
		}
		d.Stages[x.id] = x.stage
		d.Edges = append(d.Edges, workflow.Edge{From: x.id, To: x.success})
		if x.stage.Failure != "" {
			d.Edges = append(d.Edges, workflow.Edge{From: x.id, To: x.stage.Failure})
		}
	}
	for i, e := range d.Edges {
		if e.From == "integration-gate" && e.To == "Done" {
			d.Edges[i].To = "ticket-open"
		}
	}
	// Remediation reaches the implementer with the review's status and comments.
	implement := d.Stages["implement"]
	implement.Inputs = append(implement.Inputs, optional("review", "ExternalReceipt", "review-read"))
	d.Stages["implement"] = implement
	return d
}
