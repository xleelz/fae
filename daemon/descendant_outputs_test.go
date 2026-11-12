package daemon

import (
	"encoding/json"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"os"
	"path/filepath"
	"testing"
)

func TestDescendantOutputPathsIncludeCompletedDetachedButNotSibling(t *testing.T) {
	root := t.TempDir()
	path := func(name string) string {
		p := filepath.Join(root, "artifacts", name, "out")
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	parentOut := path("parent")
	childOut := path("child")
	grandOut := path("detached")
	siblingOut := path("sibling")
	jobs := []engine.Job{{TaskID: "parent", OutputDir: parentOut}, {TaskID: "child", Ancestors: []string{"parent"}, OutputDir: childOut, State: domain.Done}, {TaskID: "detached", Ancestors: []string{"child", "parent"}, Detached: true, OutputDir: grandOut, State: domain.Done}, {TaskID: "sibling", Ancestors: []string{"parent"}, OutputDir: siblingOut}}
	rows := []controlplane.JobSnapshot{}
	for _, j := range jobs {
		raw, _ := json.Marshal(envelope{Job: j})
		rows = append(rows, controlplane.JobSnapshot{ID: j.TaskID, Payload: raw})
	}
	got, err := descendantOutputPaths(root, "child", rows)
	if err != nil || len(got) != 1 || got[0] != grandOut {
		t.Fatal(got, err)
	}
	got, err = descendantOutputPaths(root, "parent", rows)
	if err != nil || len(got) != 3 {
		t.Fatal(got, err)
	}
	rows = append(rows, controlplane.JobSnapshot{ID: "bad", Payload: []byte(`{"job":{"task_id":"bad","ancestors":["parent"],"output_dir":"/tmp/foreign"}}`)})
	if _, err = descendantOutputPaths(root, "parent", rows); err == nil {
		t.Fatal("foreign output directory accepted")
	}
}
