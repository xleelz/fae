package daemon

import (
	"context"
	"encoding/json"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTaskOutputParentAndOwnedStagingLifecycle(t *testing.T) {
	root := t.TempDir()
	d := &Daemon{layout: Layout{Root: root}, jobs: map[string]controlplane.JobSnapshot{}}
	parent := engine.Job{TaskID: "parent", Document: workflow.Document{Produces: "artifacts", Staging: true}, State: domain.Pending}
	if err := d.prepareTaskDirectories(context.Background(), AdmitParams{}, &parent); err != nil {
		t.Fatal(err)
	}
	if parent.OutputDir != filepath.Join(root, "artifacts/parent/out") || parent.StagingDir != filepath.Join(root, "staging/parent") {
		t.Fatalf("task directories %+v", parent)
	}
	if err := os.WriteFile(filepath.Join(parent.OutputDir, "accepted.yaml"), []byte("accepted"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent.StagingDir, "temporary.yaml"), []byte("temporary"), 0600); err != nil {
		t.Fatal(err)
	}
	// A reconstructed daemon and completed parent still supply durable nesting.
	parent.State = domain.Done
	raw, _ := json.Marshal(envelope{Job: parent})
	d = &Daemon{layout: Layout{Root: root}, jobs: map[string]controlplane.JobSnapshot{"parent": {ID: "parent", State: "Done", Payload: raw}}}
	child := engine.Job{TaskID: "child", Document: workflow.Document{Produces: "artifacts", Staging: true}}
	if err := d.prepareTaskDirectories(context.Background(), AdmitParams{ParentTask: "parent"}, &child); err != nil {
		t.Fatal(err)
	}
	child.Ancestors = []string{"parent"}
	live := &liveExecutor{daemon: d}
	ancestors, ancestorErr := live.ancestorOutputDirectories(context.Background(), child)
	if ancestorErr != nil || len(ancestors) != 1 || ancestors[0] != parent.OutputDir {
		t.Fatal("ancestor output read grant lost", ancestors, ancestorErr)
	}
	if child.OutputDir != filepath.Join(parent.OutputDir, "child") {
		t.Fatalf("child output %s", child.OutputDir)
	}
	if data, err := os.ReadFile(filepath.Join(parent.StagingDir, "temporary.yaml")); err != nil || string(data) != "temporary" {
		t.Fatal("staging lost across restart", err)
	}
	d.cleanupTaskDirectories(parent)
	if _, err := os.Stat(parent.StagingDir); !os.IsNotExist(err) {
		t.Fatal("owned terminal staging not removed", err)
	}
	if data, err := os.ReadFile(filepath.Join(parent.OutputDir, "accepted.yaml")); err != nil || string(data) != "accepted" {
		t.Fatal("retained output removed", err)
	}
	// Foreign output/root symlinks cannot create children outside the instance.
	foreign := t.TempDir()
	if err := os.Symlink(foreign, filepath.Join(root, "artifacts/link")); err != nil {
		t.Fatal(err)
	}
	linked := engine.Job{TaskID: "link/escape", Document: workflow.Document{Produces: "artifacts"}}
	err := d.prepareTaskDirectories(context.Background(), AdmitParams{}, &linked)
	if err == nil || !strings.Contains(err.Error(), "physical") {
		t.Fatal("symlink output admitted", err)
	}
	if _, err := os.Stat(filepath.Join(foreign, "escape")); !os.IsNotExist(err) {
		t.Fatal("created outside instance", err)
	}
}

func TestAdmissionWritesCannotGrantReadOnlySupplyStageAuthority(t *testing.T) {
	root := t.TempDir()
	d := &Daemon{layout: Layout{Root: root}}
	job := engine.Job{TaskID: "readonly", Document: workflow.Document{Produces: "artifacts", Roles: map[string]workflow.Role{"reader": {}}, Stages: map[string]workflow.Stage{"read": {Primitive: workflow.AgentTurn, Role: "reader", OperatorMaySupply: true}}}}
	if err := d.prepareTaskDirectories(context.Background(), AdmitParams{Writes: []string{"judge/**"}}, &job); err == nil || !strings.Contains(err.Error(), "cannot grant authority") {
		t.Fatal("read-only admission widened", err)
	}
	if _, err := os.Stat(filepath.Join(root, "artifacts", "readonly")); !os.IsNotExist(err) {
		t.Fatal("rejected grant created task directories", err)
	}
}
