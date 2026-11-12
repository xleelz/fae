package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	r "github.com/xleelz/fae/runtime"
)

// A bridge's pinned scope (--files, --test-file, a --task-scopes entry) and
// the brief's task_scope block follow the one declared-path rule: a path
// inside .git at any depth, in any letter case, is a local refusal before the
// provider runs, recorded so the runtime types it, and no file is read or
// changed. Files named like .git are still files.
func TestBridgeScopeRefusesNestedGitMetadata(t *testing.T) {
	for name, tc := range map[string]struct {
		files  []string
		test   string
		scopes string
		block  []any
	}{
		"pinned file":      {files: []string{"answer.go", "sub/.git/config"}},
		"pinned file case": {files: []string{"answer.go", "sub/.GIT/config"}},
		"pinned test file": {files: []string{"answer.go", "answer_test.go"}, test: "sub/.git/answer_test.go"},
		"task scopes":      {scopes: `{"t":{"files":["answer.go","vendor/x/.git/HEAD","answer_test.go"],"test_file":"answer_test.go"}}`},
		"block file":       {block: []any{taskScope([]string{"answer.go", "sub/.git/config"}, "")}},
		"block test file":  {block: []any{taskScope([]string{"answer.go"}, "a/.Git/answer_test.go")}},
	} {
		t.Run(name, func(t *testing.T) {
			c, config, l := turnFixture(t, "implement", tc.block, acceptedNoEdits)
			c.Files, c.TestFile, c.TaskScopesJSON = tc.files, tc.test, tc.scopes
			if tc.block != nil {
				c.Files = nil
			}
			e := Run(context.Background(), c, config)
			var refusal *Refusal
			if !errors.As(e, &refusal) || refusal.Class != r.RefusedBeforeProvider || !strings.Contains(e.Error(), "unsafe") {
				t.Fatalf("want a local refusal naming the unsafe path, got %v", e)
			}
			if launched(l) {
				t.Fatal("provider launched despite a scope path in .git")
			}
			if _, e := os.Stat(filepath.Join(l.LogDirectory, r.BridgeRefusalFile)); e != nil {
				t.Fatal("refusal not recorded:", e)
			}
		})
	}
	c, config, l := turnFixture(t, "implement", nil, acceptedNoEdits)
	c.Files = []string{"answer.go", ".gitignore", "sub/.gitattributes", "x.git/y.go"}
	if e := Run(context.Background(), c, config); e != nil || !launched(l) {
		t.Fatal("ordinary dot files refused", e)
	}
}
