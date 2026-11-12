package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/daemon"
	"github.com/xleelz/fae/defaults"
)

func TestWorkflowTemplateVerbsMapToDaemonRPCs(t *testing.T) {
	home := shortHome(t)
	layout, err := daemon.Paths(home, "wf")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(layout.Run, 0700); err != nil {
		t.Fatal(err)
	}
	token, err := api.Credential(layout.Credential)
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan api.Request, 1)
	server, err := api.New(token, func(_ context.Context, r api.Request) (any, error) {
		requests <- r
		if r.Method == "WorkflowSimulate" {
			return map[string]any{"state": "Done"}, nil
		}
		return map[string]bool{"ok": true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = server.Listen(layout.Socket); err != nil {
		t.Fatal(err)
	}
	go server.Serve()
	defer server.Shutdown(context.Background())
	doc := defaults.Workflow()
	doc.ID = "custom"
	raw, _ := json.Marshal(doc)
	file := filepath.Join(t.TempDir(), "wf.json")
	if err = os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	scenario := filepath.Join(t.TempDir(), "s.json")
	if err = os.WriteFile(scenario, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		args   []string
		method string
		check  func(daemon.WorkflowParams, map[string]any) bool
	}{
		{[]string{"workflow-put", "--content", file, "--name", "Custom flow", "--authorizing-message", "m"}, "WorkflowInstall", func(_ daemon.WorkflowParams, raw map[string]any) bool {
			return raw["id"] == "custom" && raw["name"] == "Custom flow" && raw["authorizing_message"] == "m"
		}},
		{[]string{"workflow-get", "--workflow", "custom", "--revision", "2"}, "Workflows", func(p daemon.WorkflowParams, _ map[string]any) bool { return p.ID == "custom" && p.Revision == 2 }},
		{[]string{"workflow-latest", "--workflow", "custom"}, "Workflows", func(p daemon.WorkflowParams, _ map[string]any) bool { return p.ID == "custom" && p.Revision == 0 }},
		{[]string{"workflow-list", "--workflow", "custom"}, "WorkflowList", func(p daemon.WorkflowParams, _ map[string]any) bool { return p.ID == "custom" }},
		{[]string{"workflow-retire", "--workflow", "custom", "--reason", "superseded", "--authorizing-message", "m"}, "WorkflowRetire", func(p daemon.WorkflowParams, _ map[string]any) bool {
			return p.ID == "custom" && p.Reason == "superseded" && p.AuthorizingMessage == "m"
		}},
		{[]string{"workflow-diff", "--workflow", "custom", "--from", "1", "--to", "3"}, "WorkflowDiff", func(p daemon.WorkflowParams, _ map[string]any) bool { return p.From == 1 && p.To == 3 }},
		{[]string{"workflow-simulate", "--workflow", "custom", "--revision", "2", "--scenario", scenario, "--task", "t"}, "WorkflowSimulate", func(p daemon.WorkflowParams, _ map[string]any) bool {
			return p.ID == "custom" && p.Revision == 2 && p.Task == "t" && string(p.Scenario) == "{}" && len(p.Document) == 0
		}},
		{[]string{"workflow-simulate", "--content", file, "--scenario", scenario}, "WorkflowSimulate", func(p daemon.WorkflowParams, _ map[string]any) bool { return p.ID == "" && len(p.Document) > 0 }},
	} {
		args := append([]string{"template"}, tt.args...)
		if err = execute(context.Background(), append(args, "--home", home, "--instance", "wf"), &bytes.Buffer{}); err != nil {
			t.Fatal(tt.args, err)
		}
		r := <-requests
		var p daemon.WorkflowParams
		var m map[string]any
		_ = json.Unmarshal(r.Params, &p)
		_ = json.Unmarshal(r.Params, &m)
		if r.Method != tt.method || !tt.check(p, m) {
			t.Fatalf("%v: %s %s", tt.args, r.Method, r.Params)
		}
	}
	for _, args := range [][]string{
		{"template", "workflow-get", "--workflow", "custom"}, {"template", "workflow-latest"}, {"template", "workflow-diff", "--workflow", "w", "--from", "1"},
		{"template", "workflow-simulate", "--scenario", scenario}, {"template", "workflow-simulate", "--content", file, "--workflow", "w", "--scenario", scenario},
		{"template", "workflow-retire", "--workflow", "w"}, {"template", "workflow-get", "--workflow", "w", "--revision", "1", "--reason", "x"},
		{"template", "workflow-validate"}, {"template", "workflow-put"},
	} {
		if err = execute(context.Background(), append(args, "--home", home, "--instance", "wf"), &bytes.Buffer{}); exitCode(err) != 2 {
			t.Fatalf("%v: %v", args, err)
		}
	}
}

// FAE-WF-090 / FAE-CLI-020: offline validation lists every error and refuses with exit 1.
func TestWorkflowValidateOfflineListsAllErrors(t *testing.T) {
	doc := defaults.Workflow()
	doc.Start = "missing"
	doc.Limits.Attempts = 0
	doc.Limits.TestRuns = 0
	raw, _ := json.Marshal(doc)
	file := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := execute(context.Background(), []string{"template", "workflow-validate", "--content", file, "--json", "--home", t.TempDir()}, &out)
	if exitCode(err) != 1 || !strings.Contains(err.Error(), "limits.attempts") || !strings.Contains(err.Error(), "limits.test_runs") || !strings.Contains(err.Error(), "start") {
		t.Fatal(err)
	}
	var v daemon.WorkflowValidation
	if e := json.Unmarshal(out.Bytes(), &v); e != nil || v.Valid || len(v.Issues) < 3 {
		t.Fatalf("%s %v", out.String(), e)
	}
	out.Reset()
	if err = execute(context.Background(), []string{"workflow", "validate", file}, &out); exitCode(err) != 1 {
		t.Fatal(err)
	}
}
