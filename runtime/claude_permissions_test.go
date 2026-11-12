package runtime

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/workflow"
)

func TestClaudeTaskPermissionsIntersectLayersAndArtifacts(t *testing.T) {
	file := filepath.Join(t.TempDir(), "settings.json")
	launch := Launch{Worktree: "/task", WriteScopes: [][]string{{"pkg/**"}, {"pkg/allowed/**", "outside/**"}}, OutputDir: "/output"}
	check := func() []string {
		t.Helper()
		if err := os.WriteFile(file, []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := ConfigureClaudeTaskSettings(file, launch); err != nil {
			t.Fatal(err)
		}
		raw, _ := os.ReadFile(file)
		var doc struct {
			Permissions struct {
				Allow []string `json:"allow"`
			} `json:"permissions"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		return doc.Permissions.Allow
	}
	rules := check()
	if !slices.Contains(rules, "Write(//task/pkg/allowed/**)") || slices.Contains(rules, "Write(//task/outside/**)") {
		t.Fatal(rules)
	}
	launch.ArtifactsOnly = true
	launch.ChangeProducer = true
	rules = check()
	for _, rule := range rules {
		if strings.HasPrefix(rule, "Write(//task/") || strings.HasPrefix(rule, "Edit(//task/") {
			t.Fatal("artifacts-only worktree write granted", rules)
		}
	}
	if !slices.Contains(rules, "Write(//output/pkg/allowed/**)") {
		t.Fatal(rules)
	}
}

// Installed CLI conformance is deliberately offline: --help parses the actual
// executable and confirms launch switches without starting a model session.
func TestInstalledClaudeLaunchConformance(t *testing.T) {
	binary, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, binary, "--help").CombinedOutput()
	if err != nil {
		t.Fatal(err, string(raw))
	}
	help := string(raw)
	for _, flag := range []string{"--restricted", "--safe-mode", "--add-dir", "--tools", "--settings", "--permission-mode"} {
		if !strings.Contains(help, flag) {
			t.Errorf("installed CLI lacks %s", flag)
		}
	}
	tools := ClaudeNativeTools(workflow.Profile{Allowed: []string{"read", "list", "search_glob", "search_regex", "edit", "write", "shell", "handoff"}}, true)
	for _, tool := range tools {
		if !slices.Contains([]string{"Read", "Glob", "Grep", "Edit", "Write", "Bash", "StructuredOutput"}, tool) {
			t.Errorf("non-native tool name %q", tool)
		}
	}
}
