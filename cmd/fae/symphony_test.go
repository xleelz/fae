package main

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/daemon"
	"github.com/xleelz/fae/symphony"
	"github.com/xleelz/fae/tracker"
)

func TestSymphonyCLIWorkflowPathDefaultAndErrors(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "test-linear-token")

	tmpDir := t.TempDir()
	validWorkflow := `---
tracker:
  kind: linear
  project_slug: fae-symphony
  _mock: true
  active_states: [Todo, In Progress]
  terminal_states: [Done, Cancelled]
workspace:
  root: ` + filepath.Join(tmpDir, "ws") + `
polling:
  interval_ms: 50
agent:
  max_concurrent_agents: 4
codex:
  command: codex app-server
---
You are working on {{ issue.identifier }}: {{ issue.title }}
`
	wfPath := filepath.Join(tmpDir, "WORKFLOW.md")
	if err := os.WriteFile(wfPath, []byte(validWorkflow), 0o600); err != nil {
		t.Fatal(err)
	}

	// 1. Explicit positional path with --once exits 0.
	var stdout, stderr bytes.Buffer
	code := RunSymphonyCLI(context.Background(), []string{"--once", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0 for explicit path, got %d (stderr=%s)", code, stderr.String())
	}

	// 2. Default ./WORKFLOW.md when positional path is omitted.
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.Chdir(origWD)
	}()

	stdout.Reset()
	stderr.Reset()
	code = RunSymphonyCLI(context.Background(), []string{"--once"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0 for default ./WORKFLOW.md, got %d (stderr=%s)", code, stderr.String())
	}

	// 3. Missing workflow file exits nonzero with missing_workflow_file message.
	stdout.Reset()
	stderr.Reset()
	code = RunSymphonyCLI(context.Background(), []string{"--once", filepath.Join(tmpDir, "DOES_NOT_EXIST.md")}, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "missing_workflow_file") {
		t.Fatalf("expected nonzero exit with missing_workflow_file, got code=%d stderr=%q", code, stderr.String())
	}

	// 4. Non-map YAML front matter exits nonzero with workflow_front_matter_not_a_map.
	badFrontMatterPath := filepath.Join(tmpDir, "BAD_MAP.md")
	if err := os.WriteFile(badFrontMatterPath, []byte("---\n- item1\n- item2\n---\nPrompt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	code = RunSymphonyCLI(context.Background(), []string{"--once", badFrontMatterPath}, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "workflow_front_matter_not_a_map") {
		t.Fatalf("expected nonzero exit with workflow_front_matter_not_a_map, got code=%d stderr=%q", code, stderr.String())
	}

	// 5. Missing Linear secret fails startup validation with nonzero exit.
	t.Setenv("LINEAR_API_KEY", "")
	stdout.Reset()
	stderr.Reset()
	code = RunSymphonyCLI(context.Background(), []string{"--once", wfPath}, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "missing_tracker_secret") {
		t.Fatalf("expected nonzero exit with missing_tracker_secret, got code=%d stderr=%q", code, stderr.String())
	}
}

func TestSymphonyCLIPortOverrideAndCleanShutdown(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "test-linear-token")

	tmpDir := t.TempDir()
	wfContent := `---
tracker:
  kind: linear
  project_slug: fae-symphony
  _mock: true
workspace:
  root: ` + filepath.Join(tmpDir, "ws") + `
polling:
  interval_ms: 50
server:
  port: 59999
---
Prompt body
`
	wfPath := filepath.Join(tmpDir, "WORKFLOW.md")
	if err := os.WriteFile(wfPath, []byte(wfContent), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	readyCh := make(chan string, 1)
	doneCh := make(chan int, 1)
	var stdout, stderr bytes.Buffer

	go func() {
		code := runSymphonyInternal(ctx, []string{"--port", "0", wfPath}, &stdout, &stderr, nil, func(_ *daemon.LinearOrchestrator, srv *daemon.SymphonyHTTPServer) {
			if srv != nil {
				readyCh <- srv.URL()
			}
		})
		doneCh <- code
	}()

	var baseURL string
	select {
	case baseURL = <-readyCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for HTTP server startup (stderr=%s)", stderr.String())
	}

	if strings.HasSuffix(baseURL, ":59999") {
		t.Fatalf("expected --port 0 to override server.port 59999, got %s", baseURL)
	}

	resp, err := http.Get(baseURL + "/api/v1/state")
	if err != nil {
		t.Fatalf("GET /api/v1/state failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from CLI HTTP server, got %d", resp.StatusCode)
	}

	cancel()
	select {
	case code := <-doneCh:
		if code != 0 {
			t.Fatalf("expected clean exit code 0 on context cancel, got %d (stderr=%s)", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for CLI shutdown")
	}
}

func TestSymphonyCLIServeSubcommandFlagsAndRealLinearAdapter(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "test-linear-token")

	// Verify buildCLIAdapter returns a real *tracker.LinearAdapter even when endpoint is omitted
	adapter, err := buildCLIAdapter(symphony.ServiceConfig{
		Tracker: symphony.TrackerConfig{
			Kind: "linear",
			Provider: map[string]any{
				"api_key":      "lin_test_token",
				"project_slug": "fae-symphony",
			},
		},
	})
	if err != nil {
		t.Fatalf("buildCLIAdapter failed: %v", err)
	}
	if _, ok := adapter.(*tracker.LinearAdapter); !ok {
		t.Fatalf("expected *tracker.LinearAdapter when endpoint is omitted, got %T", adapter)
	}

	tmpDir := t.TempDir()
	wfPath := filepath.Join(tmpDir, "WORKFLOW.md")
	wfContent := `---
tracker:
  kind: linear
  project_slug: fae-symphony
  _mock: true
workspace:
  root: ` + filepath.Join(tmpDir, "ws") + `
---
Work on {{ issue.identifier }}
`
	if err := os.WriteFile(wfPath, []byte(wfContent), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	// 1. Optional "serve" subcommand + --workflow flag
	code := RunSymphonyCLI(context.Background(), []string{"serve", "--once", "--workflow", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0 for 'serve --once --workflow', got %d (stderr=%s)", code, stderr.String())
	}

	// 2. --workflow-md flag
	stdout.Reset()
	stderr.Reset()
	code = RunSymphonyCLI(context.Background(), []string{"--once", "--workflow-md", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0 for '--once --workflow-md', got %d (stderr=%s)", code, stderr.String())
	}
}
