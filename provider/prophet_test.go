package provider

import (
	"context"
	"encoding/json"
	"github.com/xleelz/fae/workflow"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProphetProviderIsolation(t *testing.T) {
	for _, tc := range []string{"valid", "read", "edit", "exec", "not-fresh", "wrong-role", "host-command"} {
		t.Run(tc, func(t *testing.T) {
			c, path, l := providerFixture(t, "")
			l.WorkflowID = "fae-prophet"
			l.Role = "prophet"
			l.FreshContext = true
			l.TaskID = "global"
			l.Profile = workflow.Profile{Allowed: []string{"handoff"}}
			c.TaskScopesJSON = `{"unrelated":{"files":["answer.go"],"test_file":"test.go"}}`
			c.Files = []string{"../must-not-read"}
			c.SessionRoot = filepath.Join(l.Worktree, "sessions")
			switch tc {
			case "read", "edit", "exec":
				l.Profile.Allowed = append(l.Profile.Allowed, tc)
			case "not-fresh":
				l.FreshContext = false
			case "wrong-role":
				l.Role = "coder"
			case "host-command":
				l.Profile.Build = true
			}
			// Fake provider must produce no edits; providerFixture otherwise proposes an edit.
			script := `#!/bin/sh
out=''
while [ "$#" -gt 0 ]; do
 if [ "$1" = '--output-last-message' ]; then shift; out="$1"; fi
 shift
done
input=$(cat)
case "$input" in *'"files":{}'*) ;; *) exit 44;; esac
printf '%s' '{"output":{"accepted":true},"edits":[]}' > "$out"
`
			if e := os.WriteFile(c.Binary, []byte(script), 0700); e != nil {
				t.Fatal(e)
			}
			raw, _ := json.Marshal(l)
			if e := os.WriteFile(path, raw, 0600); e != nil {
				t.Fatal(e)
			}
			err := Run(context.Background(), c, path)
			if tc == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				if _, e := os.Stat(c.SessionRoot); !os.IsNotExist(e) {
					t.Fatal("Prophet persisted provider session", e)
				}
			} else if err == nil {
				t.Fatal("unsafe Prophet launch accepted")
			}
			data, e := os.ReadFile(filepath.Join(l.Worktree, "answer.go"))
			if e != nil || strings.TrimSpace(string(data)) != "original" {
				t.Fatal(string(data), e)
			}
		})
	}
}
