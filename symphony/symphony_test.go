package symphony

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }

// --- 1. WORKFLOW.md Discovery and Loader Tests (§17.1) ---

func TestResolveWorkflowPathPrecedence(t *testing.T) {
	explicit := "/custom/path/MY_WORKFLOW.md"
	if got := ResolveWorkflowPath(explicit); got != explicit {
		t.Fatalf("ResolveWorkflowPath(%q) = %q, want %q", explicit, got, explicit)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	wantDefault := filepath.Join(cwd, "WORKFLOW.md")
	if got := ResolveWorkflowPath(""); got != wantDefault {
		t.Fatalf("ResolveWorkflowPath(\"\") = %q, want %q", got, wantDefault)
	}
}

func TestLoadWorkflowMissingFile(t *testing.T) {
	missingPath := filepath.Join(t.TempDir(), "DOES_NOT_EXIST.md")
	_, err := LoadWorkflow(missingPath)
	if err == nil {
		t.Fatal("expected error for missing workflow file, got nil")
	}
	if !errors.Is(err, ErrMissingWorkflowFile) {
		t.Fatalf("expected errors.Is(err, ErrMissingWorkflowFile), got %v", err)
	}
}

func TestParseWorkflowBytesFrontMatterAndBody(t *testing.T) {
	raw := []byte(`---
# Tracker configuration
tracker:
  kind: fae
  provider:
    project_slug: "$PROJECT_SLUG"
    nested: { env: prod, retries: 3 }
  required_labels:
    - symphony
    - backend
  active_states: ["open", "in_progress"]
hooks:
  after_create: |
    git init
    echo "initialized # not a comment"
  before_run: >-
    ./scripts/prepare.sh
    --strict
  timeout_ms: 45000
agent:
  max_concurrent_agents: 6
  max_turns: 15
  max_concurrent_agents_by_state:
    "In Progress": 2
    Todo: 4
    InvalidZero: 0
    InvalidNeg: -1
    InvalidStr: "abc"
custom_extension:
  enabled: true
  ratio: 0.75
---

Work on {{ issue.identifier }}: {{ issue.title }}
`)

	def, err := ParseWorkflowBytes(raw, "/tmp/project/WORKFLOW.md")
	if err != nil {
		t.Fatalf("ParseWorkflowBytes failed: %v", err)
	}

	if def.Path != "/tmp/project/WORKFLOW.md" {
		t.Fatalf("def.Path = %q, want /tmp/project/WORKFLOW.md", def.Path)
	}
	if def.PromptTemplate != "Work on {{ issue.identifier }}: {{ issue.title }}" {
		t.Fatalf("unexpected PromptTemplate: %q", def.PromptTemplate)
	}

	// Unknown top-level keys must be preserved without error
	ext, ok := def.Config["custom_extension"].(map[string]any)
	if !ok || ext["enabled"] != true || ext["ratio"] != 0.75 {
		t.Fatalf("expected custom_extension preserved in Config, got %#v", def.Config["custom_extension"])
	}

	hooksMap, ok := def.Config["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("expected hooks map, got %#v", def.Config["hooks"])
	}
	afterCreate, _ := hooksMap["after_create"].(string)
	if !strings.Contains(afterCreate, "git init\necho \"initialized # not a comment\"") {
		t.Fatalf("unexpected literal block scalar after_create: %q", afterCreate)
	}
	beforeRun, _ := hooksMap["before_run"].(string)
	if beforeRun != "./scripts/prepare.sh --strict" {
		t.Fatalf("unexpected folded block scalar before_run: %q", beforeRun)
	}
}

func TestParseWorkflowBytesWithoutFrontMatter(t *testing.T) {
	raw := []byte("  \nYou are solving {{ issue.identifier }}.\n  ")
	def, err := ParseWorkflowBytes(raw, "WORKFLOW.md")
	if err != nil {
		t.Fatalf("ParseWorkflowBytes failed: %v", err)
	}
	if len(def.Config) != 0 {
		t.Fatalf("expected empty Config map, got %#v", def.Config)
	}
	if def.PromptTemplate != "You are solving {{ issue.identifier }}." {
		t.Fatalf("unexpected PromptTemplate: %q", def.PromptTemplate)
	}
}

func TestParseWorkflowBytesErrors(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantErr error
	}{
		{
			name:    "unclosed front matter delimiter",
			content: "---\ntracker:\n  kind: fae\nNo closing delimiter",
			wantErr: ErrWorkflowParseError,
		},
		{
			name:    "only opening delimiter",
			content: "---",
			wantErr: ErrWorkflowParseError,
		},
		{
			name:    "unclosed quote in yaml",
			content: "---\ntracker:\n  kind: \"unclosed\n---\nBody",
			wantErr: ErrWorkflowParseError,
		},
		{
			name:    "unclosed bracket in yaml",
			content: "---\ntracker:\n  active_states: [open, in_progress\n---\nBody",
			wantErr: ErrWorkflowParseError,
		},
		{
			name:    "top-level block sequence not a map",
			content: "---\n- item1\n- item2\n---\nBody",
			wantErr: ErrWorkflowFrontMatterNotMap,
		},
		{
			name:    "top-level flow sequence not a map",
			content: "---\n[1, 2, 3]\n---\nBody",
			wantErr: ErrWorkflowFrontMatterNotMap,
		},
		{
			name:    "top-level scalar not a map",
			content: "---\n\"just a string\"\n---\nBody",
			wantErr: ErrWorkflowFrontMatterNotMap,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseWorkflowBytes([]byte(tc.content), "WORKFLOW.md")
			if err == nil {
				t.Fatalf("expected error %v, got nil", tc.wantErr)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected errors.Is(err, %v), got %v", tc.wantErr, err)
			}
		})
	}
}

// --- 2. Configuration Resolution & Validation Tests (§17.1) ---

func TestResolveServiceConfigDefaults(t *testing.T) {
	def := &WorkflowDefinition{
		Path:           "/tmp/symphony/WORKFLOW.md",
		Config:         map[string]any{},
		PromptTemplate: "Prompt",
	}

	cfg, err := ResolveServiceConfig(def, nil)
	if err != nil {
		t.Fatalf("ResolveServiceConfig failed: %v", err)
	}

	if cfg.Tracker.Provider == nil || len(cfg.Tracker.Provider) != 0 {
		t.Fatalf("expected empty tracker.provider map, got %#v", cfg.Tracker.Provider)
	}
	if cfg.Tracker.RequiredLabels == nil || len(cfg.Tracker.RequiredLabels) != 0 {
		t.Fatalf("expected empty tracker.required_labels slice, got %#v", cfg.Tracker.RequiredLabels)
	}
	if cfg.Polling.IntervalMS != 30000 {
		t.Fatalf("polling.interval_ms = %d, want 30000", cfg.Polling.IntervalMS)
	}
	wantRoot, _ := filepath.Abs(filepath.Join(os.TempDir(), "symphony_workspaces"))
	if cfg.Workspace.Root != wantRoot {
		t.Fatalf("workspace.root = %q, want %q", cfg.Workspace.Root, wantRoot)
	}
	if cfg.Hooks.TimeoutMS != 60000 {
		t.Fatalf("hooks.timeout_ms = %d, want 60000", cfg.Hooks.TimeoutMS)
	}
	if cfg.Agent.MaxConcurrentAgents != 10 {
		t.Fatalf("agent.max_concurrent_agents = %d, want 10", cfg.Agent.MaxConcurrentAgents)
	}
	if cfg.Agent.MaxTurns != 20 {
		t.Fatalf("agent.max_turns = %d, want 20", cfg.Agent.MaxTurns)
	}
	if cfg.Agent.MaxRetryBackoffMS != 300000 {
		t.Fatalf("agent.max_retry_backoff_ms = %d, want 300000", cfg.Agent.MaxRetryBackoffMS)
	}
	if cfg.Agent.MaxConcurrentAgentsByState == nil || len(cfg.Agent.MaxConcurrentAgentsByState) != 0 {
		t.Fatalf("expected empty agent.max_concurrent_agents_by_state, got %#v", cfg.Agent.MaxConcurrentAgentsByState)
	}
	if cfg.Codex.Command != "codex app-server" {
		t.Fatalf("codex.command = %q, want 'codex app-server'", cfg.Codex.Command)
	}
	if cfg.Codex.TurnTimeoutMS != 3600000 {
		t.Fatalf("codex.turn_timeout_ms = %d, want 3600000", cfg.Codex.TurnTimeoutMS)
	}
	if cfg.Codex.ReadTimeoutMS != 5000 {
		t.Fatalf("codex.read_timeout_ms = %d, want 5000", cfg.Codex.ReadTimeoutMS)
	}
	if cfg.Codex.StallTimeoutMS != 300000 {
		t.Fatalf("codex.stall_timeout_ms = %d, want 300000", cfg.Codex.StallTimeoutMS)
	}
}

func TestResolveServiceConfigEnvTildeAndStateMapNormalization(t *testing.T) {
	raw := []byte(`---
tracker:
  kind: linear
  provider:
    api_key: $LINEAR_API_KEY
    endpoint: "${LINEAR_ENDPOINT}/graphql"
workspace:
  root: ~/my_workspaces/$PROJECT_ENV
agent:
  max_concurrent_agents_by_state:
    "  In Progress ": 3
    "TODO": "5"
    "blocked": 0
    "done": -2
    "broken": "not-a-number"
codex:
  command: "~/bin/codex app-server --URI=ws://localhost:9000"
---
Body`)

	def, err := ParseWorkflowBytes(raw, "/opt/project/WORKFLOW.md")
	if err != nil {
		t.Fatalf("ParseWorkflowBytes: %v", err)
	}

	envMap := map[string]string{
		"HOME":            "/home/tester",
		"LINEAR_API_KEY":  "lin_secret_123",
		"LINEAR_ENDPOINT": "https://api.linear.app",
		"PROJECT_ENV":     "staging",
	}
	cfg, err := ResolveServiceConfig(def, func(k string) string { return envMap[k] })
	if err != nil {
		t.Fatalf("ResolveServiceConfig: %v", err)
	}

	if got := cfg.Tracker.Provider["api_key"]; got != "lin_secret_123" {
		t.Fatalf("provider.api_key = %v, want lin_secret_123", got)
	}
	if got := cfg.Tracker.Provider["endpoint"]; got != "https://api.linear.app/graphql" {
		t.Fatalf("provider.endpoint = %v, want https://api.linear.app/graphql", got)
	}
	if cfg.Workspace.Root != "/home/tester/my_workspaces/staging" {
		t.Fatalf("workspace.root = %q, want /home/tester/my_workspaces/staging", cfg.Workspace.Root)
	}
	// codex.command must NOT expand ~ or rewrite URIs
	if cfg.Codex.Command != "~/bin/codex app-server --URI=ws://localhost:9000" {
		t.Fatalf("codex.command was unexpectedly modified: %q", cfg.Codex.Command)
	}

	// State keys normalized and invalid entries ignored
	wantByState := map[string]int{
		"in progress": 3,
		"todo":        5,
	}
	if len(cfg.Agent.MaxConcurrentAgentsByState) != len(wantByState) {
		t.Fatalf("by_state = %#v, want %#v", cfg.Agent.MaxConcurrentAgentsByState, wantByState)
	}
	for k, v := range wantByState {
		if cfg.Agent.MaxConcurrentAgentsByState[k] != v {
			t.Fatalf("by_state[%q] = %d, want %d", k, cfg.Agent.MaxConcurrentAgentsByState[k], v)
		}
	}

	// Also verify relative workspace.root resolves relative to filepath.Dir(def.Path)
	defRel, _ := ParseWorkflowBytes([]byte("---\nworkspace:\n  root: ./rel_ws\n---\n"), "/var/lib/symphony/WORKFLOW.md")
	cfgRel, err := ResolveServiceConfig(defRel, nil)
	if err != nil {
		t.Fatalf("ResolveServiceConfig relative root: %v", err)
	}
	if cfgRel.Workspace.Root != "/var/lib/symphony/rel_ws" {
		t.Fatalf("relative workspace.root = %q, want /var/lib/symphony/rel_ws", cfgRel.Workspace.Root)
	}
}

func TestValidateDispatchConfig(t *testing.T) {
	validDef, _ := ParseWorkflowBytes([]byte("---\ntracker:\n  kind: fae\n---\n"), "WORKFLOW.md")
	validCfg, err := ResolveServiceConfig(validDef, nil)
	if err != nil {
		t.Fatalf("ResolveServiceConfig: %v", err)
	}
	if err := ValidateDispatchConfig(validCfg, "fae", "linear"); err != nil {
		t.Fatalf("expected valid config to pass, got %v", err)
	}

	// Unsupported tracker kind
	if err := ValidateDispatchConfig(validCfg, "linear"); err == nil {
		t.Fatal("expected error for unsupported tracker.kind")
	}

	// Explicit non-positive hooks.timeout_ms must fail validation
	badHooksDef, _ := ParseWorkflowBytes([]byte("---\ntracker:\n  kind: fae\nhooks:\n  timeout_ms: 0\n---\n"), "WORKFLOW.md")
	badHooksCfg, err := ResolveServiceConfig(badHooksDef, nil)
	if err != nil {
		t.Fatalf("ResolveServiceConfig: %v", err)
	}
	if err := ValidateDispatchConfig(badHooksCfg); err == nil {
		t.Fatal("expected validation failure when hooks.timeout_ms <= 0")
	}

	// Explicit non-positive agent.max_turns must fail validation
	badTurnsDef, _ := ParseWorkflowBytes([]byte("---\ntracker:\n  kind: fae\nagent:\n  max_turns: -1\n---\n"), "WORKFLOW.md")
	badTurnsCfg, err := ResolveServiceConfig(badTurnsDef, nil)
	if err != nil {
		t.Fatalf("ResolveServiceConfig: %v", err)
	}
	if err := ValidateDispatchConfig(badTurnsCfg); err == nil {
		t.Fatal("expected validation failure when agent.max_turns <= 0")
	}

	// Empty codex.command must fail validation
	badCmdDef, _ := ParseWorkflowBytes([]byte("---\ntracker:\n  kind: fae\ncodex:\n  command: \"   \"\n---\n"), "WORKFLOW.md")
	badCmdCfg, err := ResolveServiceConfig(badCmdDef, nil)
	if err != nil {
		t.Fatalf("ResolveServiceConfig: %v", err)
	}
	if err := ValidateDispatchConfig(badCmdCfg); err == nil {
		t.Fatal("expected validation failure when codex.command is blank")
	}
}

// --- 3. Dynamic Reload Watcher Tests (§6.2 & §17.1) ---

func TestWatcherDynamicReloadAndFallbackOnInvalidReload(t *testing.T) {
	dir := t.TempDir()
	wfPath := filepath.Join(dir, "WORKFLOW.md")

	initialContent := `---
tracker:
  kind: fae
polling:
  interval_ms: 15000
---
Initial template for {{ issue.identifier }}`

	if err := os.WriteFile(wfPath, []byte(initialContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var reloadCount int
	var lastReloadErr error
	watcher, err := NewWatcher(wfPath,
		WithSupportedTrackers("fae", "linear"),
		WithOnReload(func(def *WorkflowDefinition, cfg *ServiceConfig) {
			reloadCount++
		}),
		WithOnError(func(err error) {
			lastReloadErr = err
		}),
	)
	if err != nil {
		t.Fatalf("NewWatcher failed: %v", err)
	}
	if reloadCount != 1 {
		t.Fatalf("expected initial reloadCount == 1, got %d", reloadCount)
	}
	if watcher.Config().Polling.IntervalMS != 15000 {
		t.Fatalf("expected IntervalMS == 15000, got %d", watcher.Config().Polling.IntervalMS)
	}

	// Unchanged file -> ReloadIfChanged returns false, nil
	changed, err := watcher.ReloadIfChanged()
	if err != nil || changed {
		t.Fatalf("ReloadIfChanged on unchanged file = (%v, %v), want (false, nil)", changed, err)
	}

	// Valid modification -> ReloadIfChanged updates effective config & template
	updatedContent := `---
tracker:
  kind: linear
  api_key: lin_test
  project_slug: fae
polling:
  interval_ms: 5000
---
Updated template for {{ issue.identifier }}`
	if err := os.WriteFile(wfPath, []byte(updatedContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	changed, err = watcher.ReloadIfChanged()
	if err != nil || !changed {
		t.Fatalf("ReloadIfChanged on valid update = (%v, %v), want (true, nil)", changed, err)
	}
	if watcher.Config().Tracker.Kind != "linear" || watcher.Config().Polling.IntervalMS != 5000 {
		t.Fatalf("unexpected reloaded config: %#v", watcher.Config())
	}
	if watcher.Workflow().PromptTemplate != "Updated template for {{ issue.identifier }}" {
		t.Fatalf("unexpected reloaded prompt template: %q", watcher.Workflow().PromptTemplate)
	}

	// Invalid modification (broken YAML front matter) -> must NOT overwrite last known good config
	brokenContent := `---
- not_a_map
---
Broken template`
	if err := os.WriteFile(wfPath, []byte(brokenContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	changed, err = watcher.ReloadIfChanged()
	if err == nil || changed {
		t.Fatalf("expected error and changed=false on invalid reload, got (%v, %v)", changed, err)
	}
	if watcher.LastError() == nil || lastReloadErr == nil {
		t.Fatal("expected LastError and OnError callback to be populated")
	}
	// Verify last known good state is still preserved
	if watcher.Config().Tracker.Kind != "linear" || watcher.Config().Polling.IntervalMS != 5000 {
		t.Fatalf("last known good config was overwritten: %#v", watcher.Config())
	}
	if watcher.Workflow().PromptTemplate != "Updated template for {{ issue.identifier }}" {
		t.Fatalf("last known good template was overwritten: %q", watcher.Workflow().PromptTemplate)
	}
}

// --- 4. Strict Prompt Template Renderer Tests (§5.4, §12, §17.1) ---

func TestRenderPromptEmptyFallback(t *testing.T) {
	got, err := RenderPrompt("   \n\t  ", Issue{Identifier: "FAE-1"}, nil)
	if err != nil {
		t.Fatalf("RenderPrompt failed: %v", err)
	}
	if got != "You are working on an issue from the configured tracker." {
		t.Fatalf("got %q, want default empty workflow prompt", got)
	}
}

func TestRenderPromptVariablesFiltersLoopsAndConditionals(t *testing.T) {
	created := time.Date(2026, 11, 3, 2, 0, 0, 0, time.UTC)
	issue := Issue{
		ID:          "id-42",
		Identifier:  "FAE-42",
		Title:       "  Fix Flaky Watcher  ",
		Description: nil,
		Priority:    intPtr(1),
		State:       "In Progress",
		BranchName:  strPtr("feat/fae-42"),
		URL:         strPtr("https://tracker.example/FAE-42"),
		AssigneeID:  strPtr("usr-9"),
		Labels:      []string{"Backend", "Symphony"},
		BlockedBy: []BlockerRef{
			{ID: strPtr("id-10"), Identifier: strPtr("FAE-10"), State: strPtr("open")},
		},
		NativeRef: map[string]any{
			"team_key": "CORE",
		},
		Dispatchable: true,
		CreatedAt:    &created,
	}

	tmpl := `Issue: {{ issue.identifier | downcase }} ({{ issue.title | strip | upcase }})
Team: {{ issue.native_ref.team_key }}
Description: {{ issue.description | default: "No description provided" }}
Labels ({{ issue.labels | size }}): {{ issue.labels | join: ", " }}
{% if attempt %}Retry attempt #{{ attempt }}{% else %}First attempt{% endif %}
{% if issue.priority == 1 and issue.state == "In Progress" %}Urgent active issue!{% endif %}
Blockers:
{% for b in issue.blocked_by %}- {{ forloop.index }}: {{ b.identifier }} [{{ b.state }}]{% endfor %}`

	// First run (attempt == nil)
	out1, err := RenderPrompt(tmpl, issue, nil)
	if err != nil {
		t.Fatalf("RenderPrompt (attempt=nil) failed: %v", err)
	}
	want1 := `Issue: fae-42 (FIX FLAKY WATCHER)
Team: CORE
Description: No description provided
Labels (2): Backend, Symphony
First attempt
Urgent active issue!
Blockers:
- 1: FAE-10 [open]`
	if out1 != want1 {
		t.Fatalf("RenderPrompt mismatch:\ngot:\n%s\nwant:\n%s", out1, want1)
	}

	// Retry run (attempt == 3)
	out2, err := RenderPrompt(tmpl, issue, intPtr(3))
	if err != nil {
		t.Fatalf("RenderPrompt (attempt=3) failed: %v", err)
	}
	if !strings.Contains(out2, "Retry attempt #3") {
		t.Fatalf("expected 'Retry attempt #3' in output:\n%s", out2)
	}
}

func TestRenderPromptStrictErrors(t *testing.T) {
	issue := Issue{
		ID:         "1",
		Identifier: "FAE-1",
		Title:      "Test",
		State:      "open",
		NativeRef:  map[string]any{"known": "yes"},
		BlockedBy:  []BlockerRef{{Identifier: strPtr("FAE-0")}},
	}

	cases := []struct {
		name    string
		tmpl    string
		wantErr error
	}{
		{
			name:    "unknown root variable",
			tmpl:    "Hello {{ unknown_var }}",
			wantErr: ErrTemplateRenderError,
		},
		{
			name:    "unknown property on issue",
			tmpl:    "Hello {{ issue.non_existent_field }}",
			wantErr: ErrTemplateRenderError,
		},
		{
			name:    "unknown property in conditional",
			tmpl:    "{% if issue.bogus %}yes{% endif %}",
			wantErr: ErrTemplateRenderError,
		},
		{
			name:    "unknown key on native_ref",
			tmpl:    "{{ issue.native_ref.missing_key }}",
			wantErr: ErrTemplateRenderError,
		},
		{
			name:    "unknown property on blocker item",
			tmpl:    "{% for b in issue.blocked_by %}{{ b.missing }}{% endfor %}",
			wantErr: ErrTemplateRenderError,
		},
		{
			name:    "unknown filter",
			tmpl:    "{{ issue.title | non_existent_filter }}",
			wantErr: ErrTemplateRenderError,
		},
		{
			name:    "unclosed output tag",
			tmpl:    "Hello {{ issue.title",
			wantErr: ErrTemplateParseError,
		},
		{
			name:    "unclosed control tag",
			tmpl:    "{% if issue.title %}Hello",
			wantErr: ErrTemplateParseError,
		},
		{
			name:    "unclosed for block",
			tmpl:    "{% for l in issue.labels %}{{ l }}",
			wantErr: ErrTemplateParseError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := RenderPrompt(tc.tmpl, issue, nil)
			if err == nil {
				t.Fatalf("expected error %v for template %q, got nil", tc.wantErr, tc.tmpl)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected errors.Is(err, %v), got %v", tc.wantErr, err)
			}
		})
	}
}

func TestRenderContinuationPrompt(t *testing.T) {
	issue := Issue{Identifier: "FAE-99", Title: "Add watcher", State: "in_progress"}
	got := RenderContinuationPrompt(issue, intPtr(2), 3, 20)
	if !strings.Contains(got, "FAE-99") || !strings.Contains(got, "turn 3 of 20") || !strings.Contains(got, "2") {
		t.Fatalf("unexpected continuation prompt: %q", got)
	}
}

// --- 5. Workspace Manager, Key Sanitization & Lifecycle Hooks Tests (§17.2) ---

func TestWorkspaceKeySanitizationAndCollisionResistance(t *testing.T) {
	validCharsRe := regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

	// Unchanged clean identifiers stay verbatim
	if got := WorkspaceKey("MT-123"); got != "MT-123" {
		t.Fatalf("WorkspaceKey(\"MT-123\") = %q, want \"MT-123\"", got)
	}
	if got := WorkspaceKey("project.issue_42-a"); got != "project.issue_42-a" {
		t.Fatalf("WorkspaceKey(\"project.issue_42-a\") = %q", got)
	}

	// Distinct identifiers that sanitize to the same prefix must NOT collide
	kSlash := WorkspaceKey("ABC/123")
	kColon := WorkspaceKey("ABC:123")
	kSpace := WorkspaceKey("ABC 123")
	kPlain := WorkspaceKey("ABC_123")

	for _, k := range []string{kSlash, kColon, kSpace, kPlain} {
		if !validCharsRe.MatchString(k) {
			t.Fatalf("WorkspaceKey produced invalid characters: %q", k)
		}
	}

	if kSlash == kColon || kSlash == kSpace || kColon == kSpace || kSlash == kPlain {
		t.Fatalf("collision detected among sanitized workspace keys: slash=%q colon=%q space=%q plain=%q",
			kSlash, kColon, kSpace, kPlain)
	}
	if !strings.HasPrefix(kSlash, "ABC_123-") || len(kSlash) != len("ABC_123-")+16 {
		t.Fatalf("expected 'ABC_123-<16 hex chars>', got %q", kSlash)
	}

	// Dot and double-dot safety
	kDot := WorkspaceKey(".")
	kDotDot := WorkspaceKey("..")
	if kDot == "." || kDotDot == ".." || kDot == kDotDot || !validCharsRe.MatchString(kDot) || !validCharsRe.MatchString(kDotDot) {
		t.Fatalf("unsafe dot workspace keys: %q, %q", kDot, kDotDot)
	}
}

func TestWorkspaceContainmentAndValidateAgentCwd(t *testing.T) {
	root := t.TempDir()
	wsPath := filepath.Join(root, "FAE-1")
	if err := os.MkdirAll(wsPath, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	if err := ValidateAgentCwd(root, wsPath, wsPath); err != nil {
		t.Fatalf("expected valid cwd to pass, got %v", err)
	}
	// Invariant 1: cwd != expectedWorkspacePath
	if err := ValidateAgentCwd(root, root, wsPath); err == nil {
		t.Fatal("expected error when cwd != expectedWorkspacePath")
	}
	// Invariant 2: expectedWorkspacePath outside workspaceRoot
	outside := filepath.Dir(root)
	if err := ValidateAgentCwd(root, outside, outside); err == nil {
		t.Fatal("expected error when workspacePath is outside workspaceRoot")
	}
	// Root directory itself is not a valid per-issue workspace
	if err := ValidateAgentCwd(root, root, root); err == nil {
		t.Fatal("expected error when expectedWorkspacePath equals workspaceRoot")
	}

	// Symlink escaping root must be rejected
	outsideTarget := t.TempDir()
	symlinkPath := filepath.Join(root, "escaped-link")
	if err := os.Symlink(outsideTarget, symlinkPath); err == nil {
		if err := ValidateAgentCwd(root, symlinkPath, symlinkPath); err == nil {
			t.Fatal("expected symlink escaping workspaceRoot to be rejected")
		}
	}
}

func TestWorkspaceLifecycleHooksAndTimeouts(t *testing.T) {
	root := t.TempDir()
	var loggedWarnings []string

	wm, err := NewWorkspaceManager(root, HooksConfig{
		AfterCreate:  strPtr(`echo "created" > after_create.txt`),
		BeforeRun:    strPtr(`echo "before" > before_run.txt`),
		AfterRun:     strPtr(`echo "after" > after_run.txt`),
		BeforeRemove: strPtr(`echo "removing" > ../before_remove_ran.txt`),
		TimeoutMS:    2000,
	})
	if err != nil {
		t.Fatalf("NewWorkspaceManager: %v", err)
	}
	wm.Logger = func(format string, args ...any) {
		loggedWarnings = append(loggedWarnings, format)
	}

	ctx := context.Background()

	// 1. First CreateForIssue -> CreatedNow == true, runs after_create
	ws1, err := wm.CreateForIssue(ctx, "FAE/101")
	if err != nil {
		t.Fatalf("CreateForIssue first call: %v", err)
	}
	if !ws1.CreatedNow {
		t.Fatal("expected CreatedNow == true on first creation")
	}
	markerPath := filepath.Join(ws1.Path, "after_create.txt")
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("expected after_create.txt to exist: %v", err)
	}

	// Remove marker and call CreateForIssue again -> CreatedNow == false, after_create does NOT re-run
	_ = os.Remove(markerPath)
	ws2, err := wm.CreateForIssue(ctx, "FAE/101")
	if err != nil {
		t.Fatalf("CreateForIssue second call: %v", err)
	}
	if ws2.CreatedNow {
		t.Fatal("expected CreatedNow == false on reused workspace")
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatal("expected after_create hook NOT to re-run on existing workspace")
	}

	// 2. RunBeforeRun and RunAfterRun
	if err := wm.RunBeforeRun(ctx, ws1.Path); err != nil {
		t.Fatalf("RunBeforeRun: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws1.Path, "before_run.txt")); err != nil {
		t.Fatalf("expected before_run.txt to exist: %v", err)
	}
	if err := wm.RunAfterRun(ctx, ws1.Path); err != nil {
		t.Fatalf("RunAfterRun: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws1.Path, "after_run.txt")); err != nil {
		t.Fatalf("expected after_run.txt to exist: %v", err)
	}

	// 3. after_run failure is logged and ignored (returns nil)
	wm.Hooks.AfterRun = strPtr(`exit 1`)
	if err := wm.RunAfterRun(ctx, ws1.Path); err != nil {
		t.Fatalf("expected RunAfterRun failure to be ignored, got %v", err)
	}
	if len(loggedWarnings) == 0 {
		t.Fatal("expected warning logged on after_run failure")
	}

	// 4. before_run failure returns error
	wm.Hooks.BeforeRun = strPtr(`echo "boom" >&2; exit 2`)
	if err := wm.RunBeforeRun(ctx, ws1.Path); err == nil {
		t.Fatal("expected RunBeforeRun failure to return error")
	}

	// 5. CleanupIssueWorkspace runs before_remove and deletes directory even if before_remove fails
	if err := wm.CleanupIssueWorkspace(ctx, "FAE/101"); err != nil {
		t.Fatalf("CleanupIssueWorkspace: %v", err)
	}
	if _, err := os.Stat(ws1.Path); !os.IsNotExist(err) {
		t.Fatalf("expected workspace directory %q to be removed", ws1.Path)
	}
	if _, err := os.Stat(filepath.Join(root, "before_remove_ran.txt")); err != nil {
		t.Fatalf("expected before_remove hook to have executed before removal: %v", err)
	}

	// 6. after_create failure or timeout cleans up the newly created directory
	wmTimeout, err := NewWorkspaceManager(root, HooksConfig{
		AfterCreate: strPtr(`sleep 5`),
		TimeoutMS:   80,
	})
	if err != nil {
		t.Fatalf("NewWorkspaceManager: %v", err)
	}
	_, err = wmTimeout.CreateForIssue(ctx, "FAE-TIMEOUT")
	if err == nil {
		t.Fatal("expected timeout error from after_create hook")
	}
	timedOutPath, _, _ := wmTimeout.PathForIssue("FAE-TIMEOUT")
	if _, statErr := os.Stat(timedOutPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected workspace directory %q to be cleaned up after failed after_create hook", timedOutPath)
	}
}

// --- 6. Types Helper Tests (NormalizeState, IssueRoutable, SortIssuesForDispatch) ---

func TestIssueRoutingAndDispatchSort(t *testing.T) {
	t1 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)

	issues := []Issue{
		{Identifier: "FAE-3", Priority: nil, CreatedAt: &t1, Dispatchable: true, Labels: []string{"Symphony", "Backend"}},
		{Identifier: "FAE-2", Priority: intPtr(2), CreatedAt: &t2, Dispatchable: true, Labels: []string{"symphony"}},
		{Identifier: "FAE-1", Priority: intPtr(1), CreatedAt: &t2, Dispatchable: true, Labels: []string{"SYMPHONY", "backend"}},
		{Identifier: "FAE-0", Priority: intPtr(2), CreatedAt: &t1, Dispatchable: false, Labels: []string{"symphony", "backend"}},
	}

	if !IssueRoutable(issues[0], []string{"symphony", "BACKEND"}) {
		t.Fatal("expected issues[0] to be routable")
	}
	if IssueRoutable(issues[1], []string{"symphony", "backend"}) {
		t.Fatal("expected issues[1] missing 'backend' label to be unroutable")
	}
	if IssueRoutable(issues[3], []string{"symphony"}) {
		t.Fatal("expected issues[3] with Dispatchable=false to be unroutable")
	}

	sorted := SortIssuesForDispatch(issues)
	wantOrder := []string{"FAE-1", "FAE-0", "FAE-2", "FAE-3"}
	for i, want := range wantOrder {
		if sorted[i].Identifier != want {
			t.Fatalf("sorted[%d].Identifier = %q, want %q", i, sorted[i].Identifier, want)
		}
	}
}

func TestResolveLinearDefaultsSecretEnvAndWorkerConfig(t *testing.T) {
	def, err := ParseWorkflowBytes([]byte(`---
tracker:
  kind: linear
  project_slug: my-proj
worker:
  ssh_hosts:
    - host-a
    - host-b
  max_concurrent_agents_per_host: 2
---
`), "WORKFLOW.md")
	if err != nil {
		t.Fatalf("ParseWorkflowBytes: %v", err)
	}
	cfg, err := ResolveServiceConfig(def, func(k string) string {
		if k == "LINEAR_API_KEY" {
			return "lin_default_secret"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("ResolveServiceConfig: %v", err)
	}
	if len(cfg.Tracker.ActiveStates) != 2 || cfg.Tracker.ActiveStates[0] != "Todo" || cfg.Tracker.ActiveStates[1] != "In Progress" {
		t.Fatalf("expected Linear default ActiveStates [Todo, In Progress], got %#v", cfg.Tracker.ActiveStates)
	}
	if len(cfg.Tracker.TerminalStates) != 5 {
		t.Fatalf("expected 5 Linear default TerminalStates, got %#v", cfg.Tracker.TerminalStates)
	}
	if cfg.Tracker.Provider["api_key"] != "lin_default_secret" || cfg.Tracker.Provider["_api_key_env"] != "LINEAR_API_KEY" {
		t.Fatalf("expected default LINEAR_API_KEY resolution, got %#v", cfg.Tracker.Provider)
	}
	if len(cfg.Worker.SSHHosts) != 2 || cfg.Worker.MaxConcurrentAgentsPerHost != 2 {
		t.Fatalf("unexpected WorkerConfig: %#v", cfg.Worker)
	}
	if err := ValidateDispatchConfig(cfg, "linear"); err != nil {
		t.Fatalf("ValidateDispatchConfig failed: %v", err)
	}

	// Explicit $CUSTOM_VAR when CUSTOM_VAR is empty must NOT fall back to LINEAR_API_KEY and must fail ValidateDispatchConfig
	customDef, _ := ParseWorkflowBytes([]byte(`---
tracker:
  kind: linear
  api_key: $CUSTOM_VAR
  project_slug: my-proj
---
`), "WORKFLOW.md")
	customCfg, err := ResolveServiceConfig(customDef, func(k string) string {
		if k == "LINEAR_API_KEY" {
			return "lin_fallback_should_not_be_used"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("ResolveServiceConfig custom: %v", err)
	}
	if customCfg.Tracker.Provider["_api_key_env"] != "CUSTOM_VAR" || customCfg.Tracker.Provider["_api_key_explicit"] != true {
		t.Fatalf("expected _api_key_env=CUSTOM_VAR and _api_key_explicit=true, got %#v", customCfg.Tracker.Provider)
	}
	if customCfg.Tracker.Provider["api_key"] != "" {
		t.Fatalf("expected empty api_key when CUSTOM_VAR is unset, got %v", customCfg.Tracker.Provider["api_key"])
	}
	if err := ValidateDispatchConfig(customCfg, "linear"); err == nil || !strings.Contains(err.Error(), "missing_tracker_secret") {
		t.Fatalf("expected missing_tracker_secret error, got %v", err)
	}

	// Missing project_slug / team_id / team_key / project_id must fail with invalid_tracker_config
	noScopeDef, _ := ParseWorkflowBytes([]byte(`---
tracker:
  kind: linear
  api_key: lin_key
---
`), "WORKFLOW.md")
	noScopeCfg, _ := ResolveServiceConfig(noScopeDef, nil)
	if err := ValidateDispatchConfig(noScopeCfg, "linear"); err == nil || !strings.Contains(err.Error(), "invalid_tracker_config") {
		t.Fatalf("expected invalid_tracker_config error, got %v", err)
	}

	// Non-positive worker.max_concurrent_agents_per_host must fail ValidateDispatchConfig
	badWorkerDef, _ := ParseWorkflowBytes([]byte(`---
tracker:
  kind: fae
worker:
  max_concurrent_agents_per_host: 0
---
`), "WORKFLOW.md")
	badWorkerCfg, _ := ResolveServiceConfig(badWorkerDef, nil)
	if err := ValidateDispatchConfig(badWorkerCfg, "fae"); err == nil || !strings.Contains(err.Error(), "max_concurrent_agents_per_host") {
		t.Fatalf("expected max_concurrent_agents_per_host validation error, got %v", err)
	}
}

func TestRenderPromptParentAndChildrenHierarchy(t *testing.T) {
	issue := Issue{
		ID:         "sub-1",
		Identifier: "FAE-101",
		Title:      "Sub-issue",
		State:      "In Progress",
		NativeRef: map[string]any{
			"parent": map[string]any{
				"id":         "par-1",
				"identifier": "FAE-100",
				"title":      "Parent Epic",
				"state":      "In Progress",
			},
			"children": []map[string]any{
				{
					"id":         "sub-2",
					"identifier": "FAE-102",
					"title":      "Child Task",
					"state":      "Todo",
				},
			},
		},
	}

	tmpl := `Parent: {{ issue.parent.identifier }} ({{ issue.parent.title }})
Children:{% for c in issue.children %} {{ c.identifier }}[{{ c.state }}]{% endfor %}`
	got, err := RenderPrompt(tmpl, issue, nil)
	if err != nil {
		t.Fatalf("RenderPrompt with parent/children failed: %v", err)
	}
	want := "Parent: FAE-100 (Parent Epic)\nChildren: FAE-102[Todo]"
	if got != want {
		t.Fatalf("RenderPrompt hierarchy mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

// --- 7. App-Server Client & Multi-Turn Runner Tests (§10 & §17.4) ---

func TestAppServerRunnerHandshakeMultiTurnToolsAndErrors(t *testing.T) {
	wsRoot := t.TempDir()
	wsPath := filepath.Join(wsRoot, "FAE-50")
	if err := os.MkdirAll(wsPath, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	scriptPath := filepath.Join(wsRoot, "mock_codex.py")
	scriptContent := `#!/usr/bin/env python3
import json, sys, os, time

mode = os.environ.get("TEST_MODE", "multiturn")
turn_count = 0

for raw in sys.stdin:
    line = raw.strip()
    if not line:
        continue
    msg = json.loads(line)
    method = msg.get("method", "")
    msg_id = msg.get("id")

    if method == "initialize":
        if mode == "timeout_handshake":
            time.sleep(2)
            continue
        sys.stdout.write(json.dumps({"id": msg_id, "result": {"serverInfo": {"name": "mock"}}}) + "\n")
        sys.stdout.flush()
    elif method == "initialized":
        pass
    elif method == "thread/start":
        sys.stdout.write(json.dumps({"id": msg_id, "result": {"thread": {"id": "th-1"}}}) + "\n")
        sys.stdout.flush()
    elif method == "turn/start":
        turn_count += 1
        turn_id = f"turn-{turn_count}"
        sys.stdout.write(json.dumps({"id": msg_id, "result": {"turn": {"id": turn_id}}}) + "\n")
        sys.stdout.flush()

        if mode == "input_required":
            sys.stdout.write(json.dumps({"id": 900, "method": "item/tool/requestUserInput", "params": {"prompt": "Need input"}}) + "\n")
            sys.stdout.flush()
            continue

        if mode == "turn_timeout":
            time.sleep(2)
            continue

        if turn_count == 1:
            # 1. Request command approval
            sys.stdout.write(json.dumps({"id": 501, "method": "item/commandExecution/requestApproval", "params": {"command": "ls"}}) + "\n")
            sys.stdout.flush()
            app_resp = json.loads(sys.stdin.readline())
            assert app_resp.get("result", {}).get("approved") is True

            # 2. Call linear_graphql tool
            sys.stdout.write(json.dumps({
                "id": 502,
                "method": "item/tool/call",
                "params": {
                    "name": "linear_graphql",
                    "arguments": {"query": "query { viewer { id } }"}
                }
            }) + "\n")
            sys.stdout.flush()
            tool_resp = json.loads(sys.stdin.readline())
            assert tool_resp.get("result", {}).get("success") is True

            # 3. Call unsupported tool -> must receive error response without stalling
            sys.stdout.write(json.dumps({
                "id": 503,
                "method": "item/tool/call",
                "params": {
                    "name": "unknown_magic_tool",
                    "arguments": {}
                }
            }) + "\n")
            sys.stdout.flush()
            unsup_resp = json.loads(sys.stdin.readline())
            assert unsup_resp.get("result", {}).get("success") is False

        # Emit token usage & rate limits and complete turn
        sys.stdout.write(json.dumps({
            "method": "turn/completed",
            "params": {
                "usage": {"input_tokens": 100 * turn_count, "output_tokens": 50 * turn_count, "total_tokens": 150 * turn_count},
                "rate_limits": {"remaining": 999}
            }
        }) + "\n")
        sys.stdout.flush()
`
	if err := os.WriteFile(scriptPath, []byte(scriptContent), 0o755); err != nil {
		t.Fatalf("WriteFile script: %v", err)
	}

	issue := Issue{
		ID:         "id-50",
		Identifier: "FAE-50",
		Title:      "Runner test",
		State:      "In Progress",
	}

	ws := Workspace{Path: wsPath, WorkspaceKey: "FAE-50"}

	// Subtest 1: Handshake + 2 turns + tool calls + approval + token/rate-limit events
	t.Run("multiturn_and_tools", func(t *testing.T) {
		var events []RunnerEvent
		var toolCalls []string
		activeChecks := 0

		runner := &AppServerRunner{
			Codex: CodexConfig{
				Command:       "TEST_MODE=multiturn python3 " + scriptPath,
				ReadTimeoutMS: 2000,
				TurnTimeoutMS: 5000,
			},
			MaxTurns:      3,
			WorkspaceRoot: wsRoot,
			DynamicTools: []RunnerToolSpec{
				{Name: "linear_graphql", Description: "Execute GraphQL"},
			},
			ExecuteTool: func(ctx context.Context, toolName string, arguments map[string]any) (RunnerToolResult, bool) {
				if toolName != "linear_graphql" {
					return RunnerToolResult{}, false
				}
				toolCalls = append(toolCalls, toolName)
				return RunnerToolResult{
					Success: true,
					Output:  map[string]any{"data": map[string]any{"viewer": map[string]any{"id": "usr-1"}}},
				}, true
			},
			CheckActive: func(ctx context.Context, current Issue) (Issue, bool, error) {
				activeChecks++
				// Keep active after turn 1 so turn 2 runs, then mark Done after turn 2
				if activeChecks == 1 {
					return Issue{ID: current.ID, Identifier: "FAE-50", Title: "Runner test", State: "In Progress"}, true, nil
				}
				return Issue{ID: current.ID, Identifier: "FAE-50", Title: "Runner test", State: "Done"}, false, nil
			},
			OnEvent: func(ev RunnerEvent) {
				events = append(events, ev)
			},
		}

		err := runner.Run(context.Background(), issue, ws, "Initial workflow prompt for FAE-50", nil)
		if err != nil {
			t.Fatalf("runner.Run failed: %v", err)
		}
		if activeChecks != 2 {
			t.Fatalf("expected 2 activeChecks (2 turns run), got %d", activeChecks)
		}
		if len(toolCalls) != 1 || toolCalls[0] != "linear_graphql" {
			t.Fatalf("expected 1 linear_graphql tool call, got %v", toolCalls)
		}
		hasApproval := false
		hasUnsupported := false
		hasCompleted := false
		lastSessionID := ""
		for _, ev := range events {
			if ev.SessionID != "" {
				lastSessionID = ev.SessionID
			}
			switch ev.Event {
			case "approval_auto_approved":
				hasApproval = true
			case "unsupported_tool_call":
				hasUnsupported = true
			case "turn_completed":
				hasCompleted = true
			}
		}
		if lastSessionID != "th-1-turn-2" {
			t.Fatalf("expected lastSessionID 'th-1-turn-2', got %q", lastSessionID)
		}
		if !hasApproval || !hasUnsupported || !hasCompleted {
			t.Fatalf("missing expected events in %+v", events)
		}
	})

	// Subtest 2: User input requested -> fails immediately with ErrTurnInputRequired
	t.Run("turn_input_required", func(t *testing.T) {
		runner := &AppServerRunner{
			Codex: CodexConfig{
				Command:       "TEST_MODE=input_required python3 " + scriptPath,
				ReadTimeoutMS: 2000,
				TurnTimeoutMS: 5000,
			},
			MaxTurns:      2,
			WorkspaceRoot: wsRoot,
		}
		err := runner.Run(context.Background(), issue, ws, "Prompt", nil)
		if err == nil || RunnerErrorCategory(err) != ErrTurnInputRequired {
			t.Fatalf("expected ErrTurnInputRequired, got %v", err)
		}
	})

	// Subtest 3: Handshake timeout -> fails with ErrResponseTimeout
	t.Run("response_timeout", func(t *testing.T) {
		runner := &AppServerRunner{
			Codex: CodexConfig{
				Command:       "TEST_MODE=timeout_handshake python3 " + scriptPath,
				ReadTimeoutMS: 120,
				TurnTimeoutMS: 1000,
			},
			MaxTurns:      1,
			WorkspaceRoot: wsRoot,
		}
		err := runner.Run(context.Background(), issue, ws, "Prompt", nil)
		if err == nil || RunnerErrorCategory(err) != ErrResponseTimeout {
			t.Fatalf("expected ErrResponseTimeout, got %v", err)
		}
	})

	// Subtest 4: Turn timeout -> fails with ErrTurnTimeout
	t.Run("turn_timeout", func(t *testing.T) {
		runner := &AppServerRunner{
			Codex: CodexConfig{
				Command:       "TEST_MODE=turn_timeout python3 " + scriptPath,
				ReadTimeoutMS: 1000,
				TurnTimeoutMS: 150,
			},
			MaxTurns:      1,
			WorkspaceRoot: wsRoot,
		}
		err := runner.Run(context.Background(), issue, ws, "Prompt", nil)
		if err == nil || RunnerErrorCategory(err) != ErrTurnTimeout {
			t.Fatalf("expected ErrTurnTimeout, got %v", err)
		}
	})
}

