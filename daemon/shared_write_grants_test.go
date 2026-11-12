package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/execution"
	"github.com/xleelz/fae/provider"
	"github.com/xleelz/fae/workflow"
)

func TestSharedWriteRegistryDisjointCollectorsRetainCompletedPeerHistory(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(root, "out")
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal(err)
	}
	registry := filepath.Join(root, "shared-write-grants.json")
	guard := &liveAdmission{}
	bind := func(pattern string) execution.Binding {
		role := workflow.Role{}
		if pattern != "" {
			role.Writes = []string{pattern}
		}
		return execution.Binding{SharedWriteRegistry: registry, ArtifactsOnly: true, OutputDir: out, Worktree: filepath.Join(root, "worktree"), Document: workflow.Document{Roles: map[string]workflow.Role{"author": role}}}
	}
	acquire := func(id, pattern string) func() error {
		t.Helper()
		release, err := guard.acquireSharedWrite(engine.Dispatch{ID: id, Config: workflow.Stage{Primitive: workflow.AgentTurn, Role: "author"}}, bind(pattern), engine.Session{ID: id + "/session", Role: "author"})
		if err != nil {
			t.Fatal(err)
		}
		return release
	}
	observerRelease := acquire("observer", "")
	observer, err := provider.NewNativeDirectoryCollector(out, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	observer.SharedWrites(registry, "observer/session")
	aRelease := acquire("a", "a/**")
	a, err := provider.NewNativeDirectoryCollector(out, root, func(rel string) bool { return workflow.MatchGlob("a/**", rel) })
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.SharedWrites(registry, "a/session")
	bRelease := acquire("b", "b/**")
	b, err := provider.NewNativeDirectoryCollector(out, root, func(rel string) bool { return workflow.MatchGlob("b/**", rel) })
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	b.SharedWrites(registry, "b/session")
	_, err = guard.acquireSharedWrite(engine.Dispatch{ID: "overlap", Config: workflow.Stage{Primitive: workflow.AgentTurn, Role: "author"}}, bind("a/**"), engine.Session{ID: "overlap/session", Role: "author"})
	if err == nil || !strings.Contains(err.Error(), "SharedWriteScope") {
		t.Fatal("overlap admitted", err)
	}
	for _, name := range []string{"a/file", "b/file"} {
		if err = os.MkdirAll(filepath.Dir(filepath.Join(out, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(out, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err = a.Verify(); err != nil {
		t.Fatal("first collector reverted sibling", err)
	}
	if err = aRelease(); err != nil {
		t.Fatal(err)
	}
	if err = b.Verify(); err != nil {
		t.Fatal("slower collector lost completed sibling history", err)
	}
	if err = bRelease(); err != nil {
		t.Fatal(err)
	}
	if err = observer.Verify(); err != nil {
		t.Fatal("read-only observer reverted legitimate peer writes", err)
	}
	raw, err := os.ReadFile(registry)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot workflow.SharedWriteRegistry
	if err = json.Unmarshal(raw, &snapshot); err != nil || len(snapshot.Grants) != 3 {
		t.Fatal(snapshot, err)
	}
	for _, name := range []string{"a/file", "b/file"} {
		if raw, err = os.ReadFile(filepath.Join(out, name)); err != nil || string(raw) != name {
			t.Fatal(name, string(raw), err)
		}
	}
	if err = observerRelease(); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(registry)
	_ = json.Unmarshal(raw, &snapshot)
	if len(snapshot.Grants) != 0 {
		t.Fatal("finished epoch retained grants", snapshot)
	}
}
func TestSharedWriteRegistryUsesAbsoluteNestedRootsAndIntersection(t *testing.T) {
	root := t.TempDir()
	registry := filepath.Join(root, "registry.json")
	guard := &liveAdmission{}
	doc := workflow.Document{Roles: map[string]workflow.Role{"author": {Writes: []string{"**"}}}}
	binding := execution.Binding{SharedWriteRegistry: registry, ArtifactsOnly: true, OutputDir: root, Document: doc}
	d := engine.Dispatch{ID: "parent", Config: workflow.Stage{Role: "author", Primitive: workflow.AgentTurn, Writes: []string{"child/**"}}}
	release, err := guard.acquireSharedWrite(d, binding, engine.Session{ID: "parent/session", Role: "author"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	binding.OutputDir = filepath.Join(root, "child")
	d.ID = "child"
	d.Config.Writes = []string{"**"}
	if _, err = guard.acquireSharedWrite(d, binding, engine.Session{ID: "child/session", Role: "author"}); err == nil {
		t.Fatal("nested physical write roots overlap")
	}
	binding.OutputDir = root
	d.Config.Writes = []string{"unrelated/**"}
	releaseOther, err := guard.acquireSharedWrite(d, binding, engine.Session{ID: "other/session", Role: "author"})
	if err != nil {
		t.Fatal("intersection could not prove disjointness", err)
	}
	defer releaseOther()
}
