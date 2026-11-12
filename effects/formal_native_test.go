package effects

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/proof"
)

// Normalization fixtures exercise actual configured subprocess transport and
// parsing, not the soundness or availability of any formal verifier.
func TestConfiguredFormalCommandNativeReceiptsAndExactAuthorization(t *testing.T) {
	b, w, _ := setup(t)
	source := filepath.Join(w.Path, "proof.go")
	if err := os.WriteFile(source, []byte("package test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config := &proof.Config{Tool: "fixture", Parser: "normalized-json", Kind: proof.Deductive, Sources: []string{"proof.go"}}
	for _, tc := range []struct {
		name, report string
		wantPass     bool
	}{
		{name: "proved", report: `{"outcome":"proved","obligations":1,"verified":1,"failed":0,"unknown":0,"skipped":0}`, wantPass: true},
		{name: "unknown", report: `{"outcome":"unknown","obligations":1,"verified":0,"failed":0,"unknown":1,"skipped":0}`},
		{name: "label-only", report: `{"passed":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argv := []string{"python3", "-c", "print(" + strconv.Quote(tc.report) + ")"}
			b.Allowed = [][]string{argv}
			run, err := b.Run(context.Background(), w, Command{Argv: argv, Formal: config, Timeout: 5 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			if run.Proof == nil || run.Proof.Passed != tc.wantPass || !strings.HasPrefix(run.Proof.SourceDigest, "sha256:") || run.ExecutableDigest == "" {
				t.Fatalf("configured transport receipt %+v", run)
			}
			if _, err = b.Run(context.Background(), w, Command{Argv: append(append([]string{}, argv...), "not-allowlisted"), Formal: config, Timeout: 5 * time.Second}); err == nil {
				t.Fatal("formal label bypassed exact allowlist")
			}
		})
	}
	argv := []string{"python3", "-c", `import json;open("proof.go","w").write("package changed\n");print(json.dumps({"outcome":"proved","obligations":1,"verified":1,"failed":0,"unknown":0,"skipped":0}))`}
	b.Allowed = [][]string{argv}
	run, err := b.Run(context.Background(), w, Command{Argv: argv, Formal: config, Scratch: []string{"proof.go"}, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if run.Proof == nil || run.Proof.Passed || run.Proof.Outcome != proof.Error || !strings.Contains(run.Proof.Detail, "source changed") {
		t.Fatalf("modified explicit proof source accepted: %+v", run.Proof)
	}
}
func TestConfiguredFormalGobraActualPositiveAndMutation(t *testing.T) {
	java, err := exec.LookPath("java")
	if err != nil {
		t.Skip("installed Java required for native Gobra verification")
	}
	jar := os.Getenv("FAE_TEST_GOBRA_JAR")
	if jar == "" {
		jar = filepath.Join("..", ".tools", "gobra", "target", "scala-2.13", "gobra.jar")
	}
	jar, err = filepath.Abs(jar)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(jar); err != nil {
		t.Skip("existing Gobra jar unavailable; no verifier installation performed")
	}
	z3, err := exec.LookPath("z3")
	if err != nil {
		t.Skip("installed Z3 required for native Gobra verification")
	}
	b, w, _ := setup(t)
	b.MaxOutput = 4 << 20
	source := filepath.Join(w.Path, "proof.go")
	writeSource := func(value string) {
		t.Helper()
		text := "package test\n// @ ensures result == 2\nfunc Two() (result int) { return " + value + " }\n"
		if err := os.WriteFile(source, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	argv := []string{java, "-Xmx768m", "-Xss32m", "-XX:ActiveProcessorCount=1", "-jar", jar, "--logLevel", "DEBUG", "-i", "proof.go"}
	b.Allowed = [][]string{argv}
	command := Command{Argv: argv, Formal: &proof.Config{Tool: "gobra", Parser: "native", Sources: []string{"proof.go"}}, Environment: []string{"Z3_EXE=" + z3}, Timeout: 60 * time.Second}
	writeSource("2")
	positive, err := b.Run(context.Background(), w, command)
	if err != nil || positive.Proof == nil || !positive.Proof.Passed || positive.Proof.Outcome != proof.Proved || positive.Proof.Verified != 1 || positive.Proof.Obligations != 1 || positive.ExecutableDigest == "" || !strings.HasPrefix(positive.Proof.SourceDigest, "sha256:") {
		t.Fatalf("actual Gobra positive receipt %+v error=%v\n%s\n%s", positive, err, positive.Output, positive.Stderr)
	}
	writeSource("3")
	negative, err := b.Run(context.Background(), w, command)
	if err != nil || negative.Proof == nil || negative.Proof.Passed || negative.Proof.Outcome != proof.Disproved || negative.Proof.Failed < 1 || negative.ExitCode == 0 || negative.Proof.SourceDigest == positive.Proof.SourceDigest {
		t.Fatalf("actual Gobra mutation receipt %+v error=%v\n%s\n%s", negative, err, negative.Output, negative.Stderr)
	}
}
