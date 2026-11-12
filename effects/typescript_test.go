package effects

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/commandshim"
)

func nativeNode(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("installed Node.js required; no package downloads")
	}
	return node
}
func runNodeEvidence(t *testing.T, node, source, testID, expected string) (RunEvidence, error) {
	t.Helper()
	root := t.TempDir()
	file := filepath.Join(root, "fixture.test.cjs")
	if err := os.WriteFile(file, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	argv := []string{node, "--test", file}
	if testID != "" {
		var err error
		argv, err = ShapeNodeTest(argv, testID)
		if err != nil {
			t.Fatal(err)
		}
	}
	policy := commandshim.Policy{Worktree: root, CWD: root, CodeRole: true, LeaseHeld: true}
	prepared, err := policy.PrepareTypeScript(argv, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, prepared[0], prepared[1:]...)
	cmd.Dir = root
	output, runErr := cmd.CombinedOutput()
	r := RunEvidence{Output: string(output)}
	if exit, ok := runErr.(*exec.ExitError); ok {
		r.ExitCode = exit.ExitCode()
	} else if runErr != nil {
		t.Fatal(runErr, string(output))
	}
	err = ParseNodeTestEvidence(&r, Command{Argv: prepared, TestID: testID, ExpectedFailure: expected, Shape: "node-test"})
	return r, err
}
func TestNodeTestEvidenceNativeExactPassFailSkipAndNoTests(t *testing.T) {
	node := nativeNode(t)
	for _, tc := range []struct {
		name, source   string
		wantError, red bool
	}{
		{name: "pass-exact", source: `const {test}=require('node:test');test('exact',()=>{});test('other',()=>{throw Error('UNSELECTED_RAN')});`},
		{name: "expected-red", source: `const {test}=require('node:test');test('exact',()=>{throw Error('EXPECTED_RED')});`, red: true},
		{name: "skip", source: `const {test}=require('node:test');test.skip('exact',()=>{});`, wantError: true},
		{name: "todo", source: `const {test}=require('node:test');test.todo('exact');`, wantError: true},
		{name: "missing-selector", source: `const {test}=require('node:test');test('other',()=>{});`, wantError: true},
		{name: "empty-file", source: `const value=1;`, wantError: true},
		{name: "module-error", source: `throw Error('module could not load');`, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := runNodeEvidence(t, node, tc.source, "exact", "EXPECTED_RED")
			if tc.wantError {
				if err == nil {
					t.Fatalf("unexecuted/error case accepted: %+v", r)
				}
				return
			}
			if err != nil {
				t.Fatal(err, r.Output)
			}
			if r.ExecutedCount != 1 || r.MatchedCount != 1 || !r.FreshBuild || r.ExpectedFailure != tc.red || r.RedPassed == tc.red {
				t.Fatalf("incorrect exact evidence: %+v", r)
			}
		})
	}
}
func TestNodeTestEvidenceNativeNestedWholeSuite(t *testing.T) {
	node := nativeNode(t)
	r, err := runNodeEvidence(t, node, `const {describe,it}=require('node:test');describe('suite',()=>{it('first child',()=>{});it('second child',()=>{});});`, "", "")
	if err != nil || r.ExecutedCount != 2 || r.MatchedCount != 0 || r.ExpectedFailure {
		t.Fatalf("nested whole-suite evidence %+v %v\n%s", r, err, r.Output)
	}
}
func TestNodeEvidenceRejectsTruncatedContradictoryOrExtraCases(t *testing.T) {
	node := nativeNode(t)
	r, err := runNodeEvidence(t, node, `const {test}=require('node:test');test('exact',()=>{});`, "exact", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*RunEvidence)
	}{{"truncated", func(r *RunEvidence) { r.Truncated = true }}, {"exit-disagrees", func(r *RunEvidence) { r.ExitCode = 1 }}, {"summary-disagrees", func(r *RunEvidence) { r.Output = strings.Replace(r.Output, "# tests 1", "# tests 9", 1) }}, {"missing-summary", func(r *RunEvidence) { r.Output = strings.Split(r.Output, "# tests")[0] }}} {
		t.Run(tc.name, func(t *testing.T) {
			bad := r
			tc.change(&bad)
			if err := ParseNodeTestEvidence(&bad, Command{Argv: []string{node, "--test", "fixture.test.cjs"}, TestID: "exact"}); err == nil {
				t.Fatal("false evidence accepted")
			}
		})
	}
	r, err = runNodeEvidence(t, node, `const {test}=require('node:test');test('exact',()=>{});test('other',()=>{});`, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = ParseNodeTestEvidence(&r, Command{TestID: "exact"}); err == nil {
		t.Fatal("extra executed test accepted by exact shape")
	}
}
func TestNodeNativeTypeScriptThroughBroker(t *testing.T) {
	node := nativeNode(t)
	version, err := exec.Command(node, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^v(?:24\.(?:1[2-9]|[2-9][0-9])\.|2[5-9]\.|[3-9][0-9]\.)`).MatchString(strings.TrimSpace(string(version))) {
		t.Skip("stable native TypeScript stripping requires Node >=24.12")
	}
	b, w, _ := setup(t)
	file := filepath.Join(w.Path, "arithmetic.test.ts")
	source := `import {test} from 'node:test';import assert from 'node:assert/strict';const result:number=3;test('exact_ts',()=>assert.equal(result,3));test('other',()=>{throw Error('UNSELECTED_RAN')});`
	if err = os.WriteFile(file, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	argv := []string{node, "--test", file}
	b.Allowed = [][]string{argv}
	run, err := b.Run(context.Background(), w, Command{Argv: argv, Shape: "node-test", TestID: "exact_ts", Timeout: 10 * time.Second})
	if err != nil || run.ExecutedCount != 1 || run.MatchedCount != 1 || !run.RedPassed {
		t.Fatalf("native TS broker evidence %+v %v", run, err)
	}
	source = strings.Replace(source, "assert.equal(result,3)", "assert.equal(result,99,'EXPECTED_TS_RED')", 1)
	if err = os.WriteFile(file, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	run, err = b.Run(context.Background(), w, Command{Argv: argv, Shape: "node-test", TestID: "exact_ts", ExpectedFailure: "EXPECTED_TS_RED", Timeout: 10 * time.Second})
	if err != nil || run.ExitCode == 0 || !run.ExpectedFailure || run.RedPassed {
		t.Fatalf("changed source not observed through broker: %+v %v", run, err)
	}
}
