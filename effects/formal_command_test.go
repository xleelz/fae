package effects

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xleelz/fae/proof"
)

func TestFormalSourceDigestBindsPhysicalSourceBytes(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{"a.go": "package sample\n", "b.go": "package sample\n// contract\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	first, err := formalSourceDigest(root, "tree", []string{"b.go", "a.go"})
	if err != nil {
		t.Fatal(err)
	}
	reordered, err := formalSourceDigest(root, "tree", []string{"a.go", "b.go"})
	if err != nil || first != reordered {
		t.Fatal("source digest depends on declaration order", err)
	}
	if err = os.WriteFile(filepath.Join(root, "a.go"), []byte("package changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	changed, err := formalSourceDigest(root, "tree", []string{"a.go", "b.go"})
	if err != nil || changed == first {
		t.Fatal("changed source reused proof identity", err)
	}
	fallback, err := formalSourceDigest(root, "actual-tree", nil)
	if err != nil || fallback != "git-tree:actual-tree" {
		t.Fatal(fallback, err)
	}
	for _, sources := range [][]string{{"a.go", "a.go"}, {"../outside.go"}, {"*.go"}, {"."}} {
		if _, err = formalSourceDigest(root, "tree", sources); err == nil {
			t.Fatal("invalid source list accepted", sources)
		}
	}
	if err = os.Symlink(filepath.Join(root, "a.go"), filepath.Join(root, "alias.go")); err != nil {
		t.Fatal(err)
	}
	if _, err = formalSourceDigest(root, "tree", []string{"alias.go"}); err == nil {
		t.Fatal("nonphysical source accepted")
	}
	file, err := os.Create(filepath.Join(root, "huge"))
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate((64 << 20) + 1); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if _, err = formalSourceDigest(root, "tree", []string{"huge"}); err == nil {
		t.Fatal("source bytes bound ignored")
	}
}
func TestFormalCommandLabelAndExitZeroAreNotProof(t *testing.T) {
	config := &proof.Config{Tool: "fixture", Parser: "normalized-json", Kind: proof.Deductive, Sources: []string{"source.go"}}
	for _, tc := range []struct {
		name, output                 string
		exit                         int
		timeout, truncated, wantPass bool
	}{
		{name: "proved", output: `{"outcome":"proved","obligations":2,"verified":2,"failed":0,"unknown":0,"skipped":0}`, wantPass: true},
		{name: "label-only", output: `{"passed":true}`},
		{name: "exit-zero", output: "success"},
		{name: "unknown", output: `{"outcome":"unknown","obligations":1,"verified":0,"failed":0,"unknown":1,"skipped":0}`},
		{name: "zero-obligations", output: `{"outcome":"proved","obligations":0,"verified":0,"failed":0,"unknown":0,"skipped":0}`},
		{name: "disproved", output: `{"outcome":"disproved","obligations":1,"verified":0,"failed":1,"unknown":0,"skipped":0}`, exit: 1},
		{name: "timeout", output: `{"outcome":"proved","obligations":1,"verified":1,"failed":0,"unknown":0,"skipped":0}`, timeout: true},
		{name: "truncated", output: `{"outcome":"proved","obligations":1,"verified":1,"failed":0,"unknown":0,"skipped":0}`, truncated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := RunEvidence{Output: tc.output, ExitCode: tc.exit, TimedOut: tc.timeout, Truncated: tc.truncated}
			formalResult(Command{Formal: config}, &run, "sha256:actual-source")
			if run.Proof == nil || run.Proof.Passed != tc.wantPass || run.Proof.SourceDigest != "sha256:actual-source" || run.Output != tc.output {
				t.Fatalf("incorrect formal receipt: %+v", run.Proof)
			}
		})
	}
}
func TestFormalConfiguredVersionIsNotObservedVersion(t *testing.T) {
	config := &proof.Config{Tool: "fixture", Parser: "normalized-json", Kind: proof.Deductive, ToolVersion: "expected-v1"}
	run := RunEvidence{Output: `{"outcome":"proved","obligations":1,"verified":1,"failed":0,"unknown":0,"skipped":0}`}
	formalResult(Command{Formal: config}, &run, "source")
	if run.Proof == nil || run.Proof.Passed || run.Proof.ToolVersion != "" || run.Proof.Kind != proof.Deductive || !strings.Contains(run.Proof.Detail, "version") {
		t.Fatalf("configured version pretended observed: %+v", run.Proof)
	}
	run = RunEvidence{Output: `{"outcome":"proved","obligations":1,"verified":1,"failed":0,"unknown":0,"skipped":0,"tool_version":"expected-v1"}`}
	formalResult(Command{Formal: config}, &run, "source")
	if run.Proof == nil || !run.Proof.Passed || run.Proof.ToolVersion != "expected-v1" {
		t.Fatal(run.Proof)
	}
}
