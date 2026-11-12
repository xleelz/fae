package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xleelz/fae/daemon"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/workflow"
)

// putIssuesWorkflow is the default workflow with a Claude minimal route on
// the QA role, a binding the write rule refuses (workflow.RouteEffortIssues)
// but Parse, which every stored revision passes, accepts.
func putIssuesWorkflow(t *testing.T, change func(*workflow.Document), extra string) string {
	t.Helper()
	doc := defaults.Workflow()
	doc.ID = "put-issues"
	qa := doc.Roles["qa"]
	qa.Routes = append([]workflow.Binding{{Provider: "claude", Model: "claude-opus-5-5", Effort: "minimal"}}, qa.Routes[1:]...)
	doc.Roles["qa"] = qa
	if change != nil {
		change(&doc)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if extra != "" {
		raw = append(append(raw[:len(raw)-1], ',', ' '), []byte(extra+"}")...)
	}
	file := filepath.Join(t.TempDir(), "wf.json")
	if err = os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return file
}

// FAE-WF-003 (with FAE-WF-041 and FAE-CLI-020): fae workflow-put refuses a
// document with every write issue listed at once, as the daemon's install
// does. Before, it returned workflow.Parse's error, which since wave 10 no
// longer holds the route-effort write rule, so a document with any other
// issue was refused naming that issue alone, and a document whose only issue
// was the route went to the daemon. The offline validators list them all too,
// a structural issue (an unknown field) among them.
func TestWorkflowPutListsEveryWriteIssue(t *testing.T) {
	home := shortHome(t)
	route := "roles.qa.routes[0]"
	minimal := "effort minimal is not supported by provider claude"
	for _, tt := range []struct {
		name string
		file string
		want []string
	}{
		{"legacy write plus route issue", putIssuesWorkflow(t, func(d *workflow.Document) { d.SchemaVersion = 1 }, ""), []string{route, minimal, "schema_version"}},
		{"route issue alone", putIssuesWorkflow(t, nil, ""), []string{route, minimal}},
		{"route and validation issues", putIssuesWorkflow(t, func(d *workflow.Document) { d.Limits.Attempts = 0 }, ""), []string{route, minimal, "limits.attempts"}},
		{"route and structural issues", putIssuesWorkflow(t, nil, `"bogus_field": 1`), []string{route, minimal, "bogus_field"}},
	} {
		// No daemon runs: a document that reached the RPC would exit 3.
		err := execute(context.Background(), []string{"template", "workflow-put", "--content", tt.file, "--authorizing-message", "m", "--home", home, "--instance", "none"}, &bytes.Buffer{})
		if exitCode(err) != 1 {
			t.Fatalf("%s: workflow-put exit %d (%v); want 1, refused before the daemon", tt.name, exitCode(err), err)
		}
		for _, w := range tt.want {
			if !strings.Contains(fmt.Sprint(err), w) {
				t.Errorf("%s: workflow-put refusal does not list %q: %v", tt.name, w, err)
			}
		}
		for _, args := range [][]string{{"workflow", "validate", tt.file}, {"template", "workflow-validate", "--content", tt.file}} {
			var out bytes.Buffer
			err := execute(context.Background(), args, &out)
			if exitCode(err) != 1 {
				t.Fatalf("%s: %v exit %d (%v)", tt.name, args, exitCode(err), err)
			}
			var v daemon.WorkflowValidation
			if e := json.Unmarshal(out.Bytes(), &v); e != nil || v.Valid {
				t.Fatalf("%s: %v: %s %v", tt.name, args, out.String(), e)
			}
			for _, w := range tt.want {
				if !strings.Contains(fmt.Sprint(err), w) {
					t.Errorf("%s: %v does not list %q: %v", tt.name, args, w, err)
				}
			}
		}
	}
	// A document with no issue still goes to the daemon, which is not running.
	doc := defaults.Workflow()
	doc.ID = "put-clean"
	raw, _ := json.Marshal(doc)
	file := filepath.Join(t.TempDir(), "clean.json")
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := execute(context.Background(), []string{"template", "workflow-put", "--content", file, "--authorizing-message", "m", "--home", home, "--instance", "none"}, &bytes.Buffer{}); exitCode(err) != 3 {
		t.Fatalf("a valid document did not reach the RPC: exit %d (%v)", exitCode(err), err)
	}
}
