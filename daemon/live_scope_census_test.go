package daemon

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/execution"
	"github.com/xleelz/fae/workflow"
)

// FAE-VER-001 for derived task scope (FAE-WF-036, FAE-WF-073, FAE-WF-078):
// every refusal the derived scope makes fires on an input it must refuse:
// each liveScopeRefusal(...) site in live_scope.go and live_red_subtests.go,
// as a typed scope refusal, and each refusal liveRedTurnChange returns (the
// files a test-first turn may change). The census at the end reads the
// sources: a site fired when one refusal's text holds every literal fragment
// of its message, so a refusal added to them fails this test until a case
// here fires it (scripts/check-ledger.py's check:derived-scope-refusals,
// check:red-subtest-refusal and check:red-turn-change-refusals claim each
// site by its source text).
func TestLiveScopeRefusalCensus(t *testing.T) {
	var seen []string
	fire := func(name string, err error) {
		t.Helper()
		if err == nil || !isScopeRefusal(err) {
			t.Errorf("%s: want a typed scope refusal, got %v", name, err)
			return
		}
		seen = append(seen, err.Error())
	}
	refused := func(name string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s: not refused", name)
			return
		}
		seen = append(seen, err.Error())
	}
	x := &liveExecutor{}
	resolve := func(approved []string, e envelope) (liveTaskScope, error) {
		return x.resolveLiveTaskScope("task", approved, workflow.Stage{}, func() (envelope, error) { return e, nil })
	}

	fire("a derived scope with no files", liveTaskScope{Derived: true}.needFiles())
	// An unsafe approved widen is left out, not refused (FAE-WF-078).
	if s, err := resolve([]string{"../escape.go"}, scopeEnvelope([]string{"a.go"})); err != nil || len(s.Files) != 1 || s.Files[0] != "a.go" {
		t.Errorf("an unsafe approved widen: want it left out, got %v %v", s.Files, err)
	}
	s, err := resolve(nil, scopeEnvelope(nil, specEvidence("a.go", "a_test.go", ".github/workflows/ci.yml")))
	if err != nil {
		t.Fatal(err)
	}
	fire("a Spec declaring a protected path", s.needFiles())
	_, err = resolve(nil, scopeEnvelope(nil, workflow.Evidence{Type: "Spec", Stage: "spec", Fields: map[string]any{"declared_files": "a.go"}}))
	fire("a Spec whose declared_files is not a list", err)
	_, err = resolve(nil, scopeEnvelope([]string{"a.go", "a.go"}))
	fire("a duplicate declared file", err)
	s, err = resolve(nil, scopeEnvelope([]string{"a.go", "a_test.go"}))
	if err != nil {
		t.Fatal(err)
	}
	fire("a derived scope with no RedReport", s.needRed())

	red := func(id, expected string, declared any) map[string]any {
		return map[string]any{"test_id": id, "expected_failure": expected, "declared_files": declared}
	}
	files := []string{"answer.go", "answer_test.go"}
	identity := func(name string, fields map[string]any) {
		t.Helper()
		_, _, _, err := liveRedIdentity(fields, files, true)
		fire(name, err)
	}
	fire("a language test id that is not literal", nonLiteralLanguageRedIdentity())
	identity("a test id that is not a Go test name", red("not a test", "got 0 want 42", []any{"answer_test.go"}))
	identity("no expected failure", red("TestAnswer", " ", []any{"answer_test.go"}))
	identity("a reason that is only the test's name", red("TestAnswer", "TestAnswer", []any{"answer_test.go"}))
	identity("declared_files that is not a list", red("TestAnswer", "got 0 want 42", 7))
	identity("an unsafe RedReport path", red("TestAnswer", "got 0 want 42", []any{"../answer_test.go"}))
	identity("no in-scope test file", red("TestAnswer", "got 0 want 42", []any{"answer.go"}))

	// A turn's RedReport is held to the same rule when the turn completes.
	daemon := &Daemon{jobs: map[string]controlplane.JobSnapshot{}}
	raw, err := json.Marshal(scopeEnvelope(files))
	if err != nil {
		t.Fatal(err)
	}
	daemon.jobs["task"] = controlplane.JobSnapshot{ID: "task", Payload: raw}
	turn, _ := json.Marshal(red("TestAnswer", "got 0 want 42", []any{"answer.go"}))
	fire("a turn's RedReport naming no test file", (&liveExecutor{daemon: daemon}).checkRedReport(t.Context(), engine.Dispatch{TaskID: "task", Stage: "red-report", Config: workflow.Stage{Output: "RedReport"}}, execution.Binding{}, turn))

	// A RED test that starts subtests (live_red_subtests.go).
	worktree, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	subtests := "package sample\n\nimport \"testing\"\n\nfunc TestAnswer(t *testing.T) {\n\tt.Run(\"one\", func(t *testing.T) {})\n}\n"
	if err = os.WriteFile(filepath.Join(worktree, "answer_test.go"), []byte(subtests), 0600); err != nil {
		t.Fatal(err)
	}
	fire("a RED test that starts subtests", x.liveRedSubtestRefusal(execution.Binding{Worktree: worktree}, "answer_test.go", "TestAnswer"))

	// The files a test-first turn may change (liveRedTurnChange).
	first := liveTaskScope{Derived: true, Files: files}
	refused("a first RED turn changing two files", liveRedTurnChange(first, []string{"answer.go", "answer_test.go"}))
	refused("a first RED turn changing a non-test file", liveRedTurnChange(first, []string{"answer.go"}))
	refused("a RED turn changing another file than its RED file", liveRedTurnChange(liveTaskScope{Derived: true, Files: files, TestFile: "answer_test.go"}, []string{"other_test.go"}))

	// The census: every refusal site fired. A site's fragments are the
	// literal text of its message: its string literals and the string
	// constants its file declares that it names, split at formatting verbs.
	literal := regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)
	verb := regexp.MustCompile(`%[-+# 0]*[0-9]*(?:\.[0-9]*)?[a-zA-Z%]`)
	read := func(file, function string) (string, map[string]string) {
		t.Helper()
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		source := string(raw)
		constants := map[string]string{}
		for _, c := range regexp.MustCompile(`(?m)^const (\w+) = ("(?:[^"\\]|\\.)*")$`).FindAllStringSubmatch(source, -1) {
			constants[c[1]] = c[2]
		}
		if function != "" {
			start := strings.Index(source, "\nfunc "+function+"(")
			end := strings.Index(source[max(start, 0):], "\n}\n")
			if start < 0 || end < 0 {
				t.Fatalf("func %s is not in %s", function, file)
			}
			source = source[start : start+end+3]
		}
		return source, constants
	}
	var sites [][]string
	constants := map[string]string{}
	for _, c := range []struct {
		file, function string
		pattern        *regexp.Regexp
		least          int
	}{
		{"live_scope.go", "", regexp.MustCompile(`(?m)liveScopeRefusal\((.*)\)\s*$`), 12},
		{"live_red_subtests.go", "", regexp.MustCompile(`(?m)liveScopeRefusal\((.*)\)\s*$`), 1},
		{"live_scope.go", "liveRedTurnChange", regexp.MustCompile(`(?m)return (?:fmt\.Errorf|errors\.New)\((.*)\)\s*$`), 3},
	} {
		source, declared := read(c.file, c.function)
		found := c.pattern.FindAllStringSubmatch(source, -1)
		if len(found) < c.least {
			t.Fatalf("%d refusal sites found in %s %s", len(found), c.file, c.function)
		}
		sites = append(sites, found...)
		maps.Copy(constants, declared)
	}
	var missing []string
	for _, site := range sites {
		quoted := literal.FindAllString(site[1], -1)
		for _, name := range regexp.MustCompile(`\w+`).FindAllString(literal.ReplaceAllString(site[1], ""), -1) {
			if c, ok := constants[name]; ok {
				quoted = append(quoted, c)
			}
		}
		var fragments []string
		for _, l := range quoted {
			text, err := strconv.Unquote(l)
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range verb.Split(text, -1) {
				if f != "" {
					fragments = append(fragments, f)
				}
			}
		}
		fired := false
		for _, text := range seen {
			all := true
			for _, f := range fragments {
				all = all && strings.Contains(text, f)
			}
			fired = fired || all
		}
		if !fired {
			missing = append(missing, site[1])
		}
	}
	sort.Strings(missing)
	if len(missing) != 0 {
		t.Errorf("refusals never fired:\n%s", strings.Join(missing, "\n"))
	}
}
