package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDocumentOnlyLandingAndRefusal(t *testing.T) {
	f := newLandFixture(t)
	// Add a passing test in base so RequiredTests can run it if requested.
	f.write("doc_test.go", "package land\n\nimport \"testing\"\n\nfunc TestSchemaDoc(t *testing.T) {}\n")
	f.git("add", "doc_test.go")
	f.git("commit", "-m", "add doc test")
	f.base = f.git("rev-parse", "HEAD")

	// Commit that only touches models/schema.yaml (matches DocumentGlobs).
	docCommit := f.commitOn(f.base, "doc-only", map[string]string{
		"models/schema.yaml": "version: 1\nentities:\n  - user\n",
	})

	// 1. Clean document-only landing with DocumentChecks (json + lines + writes cleanup) and RequiredTests.
	req := f.request("doc-clean", docCommit)
	req.Files = []string{"models/schema.yaml"}
	req.Filter = "" // no go test filter needed for document-only diff
	req.DocumentGlobs = []string{"models/*.yaml", "docs/**"}
	req.DocumentChecks = []DocumentCheck{
		{
			ID:         "validate-yaml-json",
			Command:    []string{"sh", "-c", `mkdir -p .cache && echo "cached" > .cache/yaml.tmp && printf '{"valid_files":2,"status":"ok"}\n'`},
			Format:     "json",
			Output:     "stdout",
			CountField: "valid_files",
			Writes:     []string{".cache/**"},
		},
		{
			ID:         "validate-yaml-lines",
			Command:    []string{"sh", "-c", "printf 'models/schema.yaml\\n'"},
			Format:     "lines",
			Output:     "stdout",
			CountField: "checked_lines",
		},
	}
	req.RequiredTests = [][]string{{"go", "test", "-run", "^TestSchemaDoc$", "./..."}}

	res, err := Land(context.Background(), req)
	if err != nil || res.Outcome != LandLanded {
		t.Fatalf("expected document-only landing to succeed, got err=%v outcome=%s reason=%s gates=%+v", err, res.Outcome, res.Reason, res.Gates)
	}
	for _, name := range []string{"own-hunk", "format", "compile", "filter", "qa", "suite"} {
		g := gate(res, name)
		if !g.Passed || g.Count < 1 {
			t.Fatalf("expected gate %s to pass with positive count on document-only landing, got %+v", name, g)
		}
	}
	// Verify that .cache/yaml.tmp written by the DocumentCheck was not committed or left in the target checkout.
	if _, statErr := os.Stat(filepath.Join(f.repo, ".cache", "yaml.tmp")); !os.IsNotExist(statErr) {
		t.Fatalf("expected DocumentCheck writes path .cache/yaml.tmp not to pollute target repo, statErr=%v", statErr)
	}

	// 2. Document-only landing where a DocumentCheck reports count == 0 MUST be refused (Held).
	docCommit2 := f.commitOn(res.Landed, "doc-zero-count", map[string]string{
		"models/schema.yaml": "version: 2\n",
	})
	reqZero := f.request("doc-zero", docCommit2)
	reqZero.Files = []string{"models/schema.yaml"}
	reqZero.Filter = ""
	reqZero.DocumentGlobs = []string{"models/*.yaml"}
	reqZero.DocumentChecks = []DocumentCheck{
		{
			ID:         "validate-yaml-empty",
			Command:    []string{"sh", "-c", `printf '{"valid_files":0}\n'`},
			Format:     "json",
			Output:     "stdout",
			CountField: "valid_files",
		},
	}
	resZero, err := Land(context.Background(), reqZero)
	if err != nil {
		t.Fatalf("unexpected transport error on zero-count document check: %v", err)
	}
	if resZero.Outcome == LandLanded {
		t.Fatalf("expected zero-count DocumentCheck to refuse landing, got LandLanded")
	}
	if g := gate(resZero, "suite"); g.Passed {
		t.Fatalf("expected suite gate to fail when DocumentCheck count is 0, got %+v", g)
	}

	// 3. Mixed diff touching both models/schema.yaml and answer.go (outside DocumentGlobs) is NOT document-only and fails without a code test filter.
	mixedCommit := f.commitOn(res.Landed, "mixed-diff", map[string]string{
		"models/schema.yaml": "version: 3\n",
		"answer.go":          "package land\n\nfunc answer() int { return 99 }\n",
	})
	reqMixed := f.request("doc-mixed", mixedCommit)
	reqMixed.Files = []string{"models/schema.yaml", "answer.go"}
	reqMixed.Filter = "TestNonExistentFilter"
	reqMixed.DocumentGlobs = []string{"models/*.yaml"}
	reqMixed.DocumentChecks = []DocumentCheck{
		{
			ID:         "validate-yaml-ok",
			Command:    []string{"sh", "-c", `printf '{"valid_files":1}\n'`},
			Format:     "json",
			Output:     "stdout",
			CountField: "valid_files",
		},
	}
	resMixed, err := Land(context.Background(), reqMixed)
	if err != nil {
		t.Fatalf("unexpected error on mixed diff: %v", err)
	}
	if resMixed.Outcome == LandLanded {
		t.Fatalf("expected mixed diff touching answer.go outside DocumentGlobs to require code test filter and refuse landing")
	}

	// 4. Unit test EvaluateLanding directly for DocumentOnlyInput.
	overall, gates := EvaluateLanding(LandingEvaluationInput{
		DiffFiles:     []string{"docs/guide.md", "models/schema.yaml"},
		DocumentGlobs: []string{"docs/**", "models/*.yaml"},
		DocumentChecks: []DocumentCheckRun{
			{ID: "doc-lint", Command: []string{"lint"}, Format: "json", CountField: "files", ExitCode: 0, Count: 3},
		},
	})
	if !overall.Passed || !LandingGate(gates).Passed {
		t.Fatalf("expected EvaluateLanding to pass for valid document-only input, got %+v", gates)
	}
	overallFail, gatesFail := EvaluateLanding(LandingEvaluationInput{
		DiffFiles:     []string{"docs/guide.md"},
		DocumentGlobs: []string{"docs/**"},
		DocumentChecks: []DocumentCheckRun{
			{ID: "doc-lint", Command: []string{"lint"}, Format: "json", CountField: "files", ExitCode: 0, Count: 0},
		},
	})
	if overallFail.Passed || LandingGate(gatesFail).Passed || !strings.Contains(overallFail.Reason, "suite") {
		t.Fatalf("expected EvaluateLanding to fail when DocumentCheckRun has Count=0, got %+v", gatesFail)
	}
}
