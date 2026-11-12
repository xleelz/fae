package daemon

import (
	"context"
	"encoding/json"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/provider"
	"github.com/xleelz/fae/workflow"
	"os"
	"testing"
	"time"
)

func TestOutputOwnershipPublishesOnlyCommittedRowsAndRebuildsNative(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.Executor = engine.NewScripted(map[string][]engine.ScriptStep{"draft": {{Output: json.RawMessage(`{"text":"owned"}`)}}})
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if d != nil {
			stop(t, d)
		}
	}()
	authorize(t, d)
	_, doc := explicitChildFixture("plain")
	doc.ID = "ownership-leaf"
	doc.Recursion = &workflow.Recursion{MaxDepth: 3, MaxDescendants: 5, MaxChildrenPerVisit: 3}
	governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"})
	admit := func(id, parent string, inputs map[string]any) error {
		_, e := call(t, d, "Admit", AdmitParams{ID: id, ParentTask: parent, WorkflowID: doc.ID, Inputs: inputs, Tree: "tree", AuthorizingMessage: "message"}, id)
		return e
	}
	if err = admit("owner-root", "", map[string]any{"value": "root"}); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "owner-root", "Done")
	read := func() outputOwnership {
		raw, e := os.ReadFile(d.outputOwnershipPath())
		if e != nil {
			t.Fatal(e)
		}
		var reg outputOwnership
		if e = json.Unmarshal(raw, &reg); e != nil {
			t.Fatal(e)
		}
		return reg
	}
	if reg := read(); reg.Version != 1 || len(reg.Owners) != 1 {
		t.Fatal(reg)
	}
	unlock, err := provider.LockOutputOwnership(d.outputOwnershipPath())
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- admit("owner-root.detached.1", "owner-root", map[string]any{"value": "child"}) }()
	select {
	case err := <-result:
		unlock()
		t.Fatal("admission passed an active collector ownership lock", err)
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	if err = <-result; err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "owner-root.detached.1", "Done")
	if err = admit("owner-root.detached.9", "owner-root", nil); err == nil {
		t.Fatal("invalid child admitted")
	}
	reg := read()
	if len(reg.Owners) != 2 {
		t.Fatal("refused candidate reserved output ownership", reg)
	}
	found := false
	for _, o := range reg.Owners {
		if o.Task == "owner-root.detached.1" {
			found = o.Parent == "owner-root" && len(o.Ancestors) == 1 && o.Ancestors[0] == "owner-root"
		}
	}
	if !found {
		t.Fatal(reg)
	}
	_, err = call(t, d, "BacklogAdmit", BacklogParams{Items: []BacklogItemParams{{AdmitParams: AdmitParams{ID: "owner-root.detached.2", Priority: 10, Content: "registry batch child", ParentTask: "owner-root", WorkflowID: doc.ID, Tree: "tree", Inputs: map[string]any{"value": "batch"}}}}, AuthorizingMessage: "message"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(read().Owners) != 3 {
		t.Fatal("backlog ownership not published")
	}
	path := d.outputOwnershipPath()
	stop(t, d)
	d = nil
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	d, err = Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(read().Owners) != 3 {
		t.Fatal("completed ownership did not rebuild")
	}
}
