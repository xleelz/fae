package daemon

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

func governanceCall[T any](t *testing.T, d *Daemon, method string, p any) T {
	t.Helper()
	r, err := call(t, d, method, p, "")
	if err != nil {
		t.Fatal(method, err)
	}
	var out T
	if err = json.Unmarshal(r.Result, &out); err != nil {
		t.Fatal(method, err)
	}
	return out
}
func refused(t *testing.T, d *Daemon, method string, p any, key, want string) {
	t.Helper()
	if _, err := call(t, d, method, p, key); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("%s: got %v, want refusal containing %q", method, err, want)
	}
}

// FAE-WF-002 / FAE-CLI-020: append-only numbering, parent linkage, no caller-chosen
// revision, retirement that refuses new bindings while a bound task finishes.
func TestWorkflowGovernanceNativeRevisionsRetirementAndReads(t *testing.T) {
	example, err := os.ReadFile("../examples/workflow.json")
	if err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile("../examples/script.json")
	if err != nil {
		t.Fatal(err)
	}
	cfg, ctx := fixture(t)
	gated := &drainExecutor{started: make(chan string, 1000), release: make(chan struct{}), delegate: engine.NewScripted(defaults.HappyScript())}
	cfg.Executor = gated
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			close(gated.release)
		}
		stop(t, d)
	}()
	authorize(t, d)
	doc := defaults.Workflow()
	doc.ID = "governed"
	first := governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"})
	if first.Number != 1 || first.Parent != 0 || first.Name != "governed" {
		t.Fatalf("first revision %+v", first)
	}
	coder := doc.Roles["coder"]
	coder.Cap = 3
	doc.Roles["coder"] = coder
	second := governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Name: "Governed delivery", Document: doc, AuthorizingMessage: "message"})
	if second.Number != 2 || second.Parent != 1 || second.Name != "Governed delivery" || second.Hash == first.Hash {
		t.Fatalf("second revision %+v", second)
	}
	// A caller cannot choose (or overwrite) a revision number.
	refused(t, d, "WorkflowInstall", map[string]any{"id": doc.ID, "revision": 1, "document": doc, "authorizing_message": "message"}, "", "unknown field")
	listed := governanceCall[[]controlplane.WorkflowListing](t, d, "WorkflowList", WorkflowParams{ID: doc.ID})
	if len(listed) != 2 || listed[0].Revision != 1 || listed[0].Parent != 0 || listed[1].Parent != 1 || listed[0].Hash != first.Hash || listed[1].Retired {
		t.Fatalf("listing %+v", listed)
	}
	if all := governanceCall[[]controlplane.WorkflowListing](t, d, "WorkflowList", WorkflowParams{}); len(all) != 3 {
		t.Fatalf("instance listing %+v", all)
	}
	got := governanceCall[WorkflowRead](t, d, "Workflows", WorkflowParams{ID: doc.ID, Revision: 1})
	stored, err := workflow.Parse(got.Document)
	if err != nil || got.Revision.Number != 1 || got.Retirement != nil || stored.Roles["coder"].Cap != 4 {
		t.Fatalf("get %+v %v", got.Revision, err)
	}
	if latest := governanceCall[WorkflowRead](t, d, "Workflows", WorkflowParams{ID: doc.ID}); latest.Revision.Number != 2 {
		t.Fatalf("latest %+v", latest.Revision)
	}
	refused(t, d, "Workflows", WorkflowParams{ID: doc.ID, Revision: 9}, "", "unknown workflow")
	diff := governanceCall[WorkflowDiffResult](t, d, "WorkflowDiff", WorkflowParams{ID: doc.ID, From: 1, To: 2})
	if len(diff.Changes) != 1 || diff.Changes[0].Path != "roles.coder.cap" || !diff.From.Valid || !diff.To.Valid {
		t.Fatalf("diff %+v", diff)
	}
	refused(t, d, "WorkflowDiff", WorkflowParams{ID: doc.ID, From: 1, To: 3}, "", "unknown workflow")
	raw, _ := json.Marshal(doc)
	if v := governanceCall[WorkflowValidation](t, d, "WorkflowValidate", WorkflowParams{Document: raw}); !v.Valid || v.ContentHash == "" || v.Workflow != "governed" {
		t.Fatalf("validate %+v", v)
	}
	broken := defaults.Workflow()
	broken.Start = "missing"
	broken.Limits.Attempts = 0
	raw, _ = json.Marshal(broken)
	if v := governanceCall[WorkflowValidation](t, d, "WorkflowValidate", WorkflowParams{Document: raw}); v.Valid || len(v.Issues) < 2 {
		t.Fatalf("broken validate %+v", v)
	}
	if s := governanceCall[workflow.Simulation](t, d, "WorkflowSimulate", WorkflowParams{Document: example, Scenario: script, Task: "demo"}); s.State != "Done" {
		t.Fatalf("simulation %+v", s)
	}
	refused(t, d, "WorkflowSimulate", WorkflowParams{Document: example, ID: "demo", Scenario: script, Task: "demo"}, "", "exactly one")

	// Bind a task, then retire: the bound task finishes, new bindings are refused.
	if _, err = call(t, d, "Admit", AdmitParams{ID: "bound", WorkflowID: doc.ID, Tree: "tree", AuthorizingMessage: "message"}, "bound"); err != nil {
		t.Fatal(err)
	}
	<-gated.started
	retirement := governanceCall[controlplane.WorkflowRetirement](t, d, "WorkflowRetire", WorkflowParams{ID: doc.ID, Reason: "superseded by v3 design", AuthorizingMessage: "message"})
	if retirement.Reason != "superseded by v3 design" || retirement.RetiredAt.IsZero() {
		t.Fatalf("retirement %+v", retirement)
	}
	again := governanceCall[controlplane.WorkflowRetirement](t, d, "WorkflowRetire", WorkflowParams{ID: doc.ID, Reason: "second", AuthorizingMessage: "message"})
	if !again.RetiredAt.Equal(retirement.RetiredAt) || again.Reason != retirement.Reason {
		t.Fatalf("first retirement not kept %+v", again)
	}
	refused(t, d, "WorkflowRetire", WorkflowParams{ID: "never-installed", Reason: "x", AuthorizingMessage: "message"}, "", "unknown workflow")
	refused(t, d, "Admit", AdmitParams{ID: "late", WorkflowID: doc.ID, Tree: "tree", AuthorizingMessage: "message"}, "late", "workflow retired")
	refused(t, d, "Admit", AdmitParams{ID: "pinned", WorkflowID: doc.ID, Revision: 1, Tree: "tree", AuthorizingMessage: "message"}, "pinned", "workflow retired")
	refused(t, d, "Admit", AdmitParams{ID: "unknown", WorkflowID: "no-such-workflow", Tree: "tree", AuthorizingMessage: "message"}, "unknown", "unknown workflow")
	refused(t, d, "BacklogAdmit", BacklogParams{Items: []BacklogItemParams{{AdmitParams: AdmitParams{ID: "queued", WorkflowID: doc.ID, Tree: "tree", Priority: 10, Content: "x"}}}, AuthorizingMessage: "message"}, "", "workflow retired")
	refused(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, "", "workflow retired")
	if after := governanceCall[WorkflowRead](t, d, "Workflows", WorkflowParams{ID: doc.ID}); after.Retirement == nil || after.Revision.Number != 2 {
		t.Fatalf("retired read %+v", after)
	}
	if listed = governanceCall[[]controlplane.WorkflowListing](t, d, "WorkflowList", WorkflowParams{ID: doc.ID}); len(listed) != 2 || !listed[0].Retired || !listed[1].Retired {
		t.Fatalf("retired listing %+v", listed)
	}
	for _, id := range []string{"late", "pinned", "unknown", "queued"} {
		if _, err = d.store.GetJob(ctx, id); err == nil {
			t.Fatalf("refused admission %s was written", id)
		}
	}
	close(gated.release)
	released = true
	if done := awaitState(t, d, "bound", "Done"); done.WorkflowID != doc.ID || done.WorkflowRevision != 2 {
		t.Fatalf("bound task %+v", done)
	}
}
