package effects

import (
	"context"
	"github.com/xleelz/fae/workflow"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGeneratorKeepsDeclaredChangesAndRevertsOthers(t *testing.T) {
	b, w, _ := setup(t)
	command := []string{"python3", "-c", `open("ontology.json","w").write('{"ok":true}')`}
	b.Allowed = [][]string{command}
	run, err := b.Run(context.Background(), w, Command{Argv: command, Timeout: 10 * time.Second, DeclaredFiles: []string{"ontology.json"}, Generate: &workflow.GenerateSpec{Writes: []string{"ontology.json"}}, EffectiveWriteScopes: [][]string{{"*.json"}}})
	if err != nil || run.ChangedCount != 1 || len(run.Changed) != 1 || run.Changed[0] != "ontology.json" {
		t.Fatalf("generator evidence %+v %v", run, err)
	}
	snap, err := b.Snapshot(context.Background(), w, []string{"ontology.json"})
	if err != nil {
		t.Fatal(err)
	}
	commit, err := b.Commit(context.Background(), w, snap, "generated")
	if err != nil {
		t.Fatal(err)
	}
	if got := gitTest(t, w.Path, "show", commit.SHA+":ontology.json"); got != "{\"ok\":true}" {
		t.Fatal(got)
	}
	command = []string{"python3", "-c", `open("outside.txt","w").write("bad")`}
	b.Allowed = [][]string{command}
	_, err = b.Run(context.Background(), w, Command{Argv: command, Timeout: 10 * time.Second, Generate: &workflow.GenerateSpec{Writes: []string{"ontology.json"}}})
	if err == nil || !strings.Contains(err.Error(), "outside.txt") {
		t.Fatal("unscoped change accepted", err)
	}
	if _, err = os.Stat(filepath.Join(w.Path, "outside.txt")); !os.IsNotExist(err) {
		t.Fatal("outside change not reverted")
	}
}
func TestCheckRetainsScratchCapturesAndNonzeroExit(t *testing.T) {
	b, w, _ := setup(t)
	command := []string{"python3", "-c", `import os,json,sys;os.makedirs("scratch",exist_ok=True);open("scratch/accepted","w").write("ok");print(json.dumps({"checked":1,"body":"x"*300000}));sys.stderr.write("finding");sys.exit(1)`}
	b.Allowed = [][]string{command}
	b.Scratch = []string{"scratch/**"}
	run, err := b.Run(context.Background(), w, Command{Argv: command, Timeout: 10 * time.Second, Check: &workflow.CheckSpec{Format: "json", CountField: "checked", Writes: []string{"scratch/**"}}, Capture: []string{"stdout", "stderr"}})
	if err != nil || run.ExitCode != 1 || run.CheckCount != 1 || len(run.Captured["stdout"]) != 256<<10 || run.Captured["stderr"] != "finding" || !run.Truncated {
		t.Fatalf("check capture %+v %v", run, err)
	}
	if got, err := os.ReadFile(filepath.Join(w.Path, "scratch/accepted")); err != nil || string(got) != "ok" {
		t.Fatal("scratch not retained", err)
	}
	if names := gitTest(t, w.Path, "ls-tree", "--name-only", run.Tree); strings.Contains(names, "scratch") {
		t.Fatal("scratch in task tree", names)
	}
}

func TestArtifactGeneratorRunsWithoutNamespaceAndCollectsEvidence(t *testing.T) {
	b, w, _ := setup(t)
	output := t.TempDir()
	command := []string{"python3", "-c", `open("ontology.json","w").write('{"ok":true}')`}
	b.Allowed = [][]string{command}
	run, err := b.Run(context.Background(), w, Command{Argv: command, Timeout: 10 * time.Second, ArtifactsOnly: true, OutputDir: output, Generate: &workflow.GenerateSpec{Writes: []string{"ontology.json"}}, EffectiveWriteScopes: [][]string{{"*.json"}}})
	if err != nil {
		t.Fatal("output-only generator required namespace or failed", err)
	}
	if run.ChangedCount != 1 || len(run.Changed) != 1 || run.Changed[0] != "ontology.json" {
		t.Fatalf("native output evidence missing: %+v", run)
	}
	data, err := os.ReadFile(filepath.Join(output, "ontology.json"))
	if err != nil || string(data) != "{\"ok\":true}" {
		t.Fatal("generated artifact missing", string(data), err)
	}
	if run.IsolationPolicy != "cli-and-collector" {
		t.Fatal("unnecessary namespace required", run.IsolationPolicy)
	}
	if _, err = os.Stat(filepath.Join(w.Path, "ontology.json")); !os.IsNotExist(err) {
		t.Fatal("artifact generator wrote worktree", err)
	}
	command = []string{"python3", "-c", `open("outside.txt","w").write("bad")`}
	b.Allowed = [][]string{command}
	_, err = b.Run(context.Background(), w, Command{Argv: command, Timeout: 10 * time.Second, ArtifactsOnly: true, OutputDir: output, Generate: &workflow.GenerateSpec{Writes: []string{"ontology.json"}}})
	if err == nil {
		t.Fatal("out-of-scope artifact write accepted")
	}
	if _, err = os.Stat(filepath.Join(output, "outside.txt")); !os.IsNotExist(err) {
		t.Fatal("out-of-scope artifact not removed", err)
	}
}
