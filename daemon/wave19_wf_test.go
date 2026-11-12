package daemon

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/effects"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"github.com/xleelz/fae/worktree"
)

type funcExecutor func(context.Context, engine.Dispatch) (engine.Result, error)

func (f funcExecutor) Execute(ctx context.Context, d engine.Dispatch) (engine.Result, error) {
	return f(ctx, d)
}

func TestApplyLandingTestShapeAndTargetPolicyDocumentChecks(t *testing.T) {
	// 1. Verify applyLandingTestShape shapes LandRequest.QA for pkg test_shape (Wave 18 residual in daemon/land.go).
	env := envelope{
		Job: engine.Job{TaskID: "task-shape-1"},
		Admission: AdmitParams{
			DeclaredFiles: []string{"workflow/fan.go"},
			TestFilter:    "TestFanOut",
			TestShape:     LandingTestShapePkg,
		},
	}
	scope := liveTaskScope{
		Files:  []string{"workflow/fan.go"},
		TestID: "TestFanOut",
	}
	req := controller.LandRequest{
		ID:   "land-1",
		Task: "task-shape-1",
		QA:   [][]string{{"go", "test", "-json", "-count=1", "./..."}},
	}
	receipt, err := applyLandingTestShape(&req, env, scope, []string{"go", "test", "./..."})
	if err != nil {
		t.Fatalf("applyLandingTestShape failed: %v", err)
	}
	if receipt.ResolvedShape != LandingTestShapePkg {
		t.Fatalf("expected ResolvedShape=%q, got %q", LandingTestShapePkg, receipt.ResolvedShape)
	}
	foundShapedQA := slices.ContainsFunc(req.QA, func(argv []string) bool {
		return slices.Equal(argv, receipt.EffectiveArgs)
	})
	if !foundShapedQA {
		t.Fatalf("expected req.QA to contain shaped args %v, got %v", receipt.EffectiveArgs, req.QA)
	}

	// 2. Verify validateTargetPolicy accepts valid DocumentGlobs and DocumentChecks (FAE-CTL-052).
	validTarget := TargetConfig{
		SchemaVersion:  4,
		IntegrationRef: "refs/heads/main",
		DocumentGlobs:  []string{"docs/**", "models/*.yaml"},
		DocumentChecks: []DocumentCheck{
			{
				ID:         "yaml-schema",
				Command:    []string{"python3", "scripts/check_yaml.py"},
				Format:     "json",
				Output:     "stdout",
				CountField: "valid_files",
				Writes:     []string{".cache/**"},
			},
			{
				ID:         "md-links",
				Command:    []string{"python3", "scripts/check_links.py"},
				Format:     "lines",
				CountField: "checked_docs",
			},
		},
	}
	if err := validateTargetPolicy(validTarget); err != nil {
		t.Fatalf("expected validTarget with DocumentGlobs + DocumentChecks to pass validateTargetPolicy, got: %v", err)
	}

	// 3. Verify validateTargetPolicy rejects DocumentChecks without DocumentGlobs, empty CountField, or bad Format.
	noGlobs := validTarget
	noGlobs.DocumentGlobs = nil
	if err := validateTargetPolicy(noGlobs); err == nil || !strings.Contains(err.Error(), "document_globs") {
		t.Fatalf("expected error when DocumentChecks declared without DocumentGlobs, got: %v", err)
	}

	noCountField := validTarget
	noCountField.DocumentChecks = []DocumentCheck{{
		ID:      "bad-check",
		Command: []string{"true"},
		Format:  "json",
	}}
	if err := validateTargetPolicy(noCountField); err == nil || !strings.Contains(err.Error(), "count_field") {
		t.Fatalf("expected error when DocumentCheck is missing count_field, got: %v", err)
	}
}

func TestDriveParallelFanOutAndBranchWorktrees(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=fae-test",
			"GIT_AUTHOR_EMAIL=fae@example.com",
			"GIT_COMMITTER_NAME=fae-test",
			"GIT_COMMITTER_EMAIL=fae@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	gitRun(repo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/test\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"pkg/alpha/a.go", "pkg/beta/b.go"} {
		abs := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte("package p\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(repo, "add", ".")
	gitRun(repo, "commit", "-m", "initial")
	baseSHA := gitRun(repo, "rev-parse", "HEAD")
	baseTree := gitRun(repo, "rev-parse", "HEAD^{tree}")

	broker := &effects.Broker{
		Manager: &worktree.Manager{
			Repository: repo,
			Root:       filepath.Join(tmp, "worktrees"),
		},
	}

	role := func() workflow.Role {
		return workflow.Role{
			Instructions: "instructions",
			Briefs:       map[string]string{"default": "brief"},
			Routes:       []workflow.Binding{{Provider: "script", Model: "recorded"}},
			Scope:        "task-bound",
			Cap:          1,
			Outputs:      []string{"report"},
		}
	}
	passExpr := workflow.Expr{
		Op: "eq",
		Args: []workflow.Expr{
			{Op: "field", Stage: "branch_review", Path: []string{"approved"}},
			{Op: "literal", Value: true},
		},
	}
	doc := currentWorkflowFixture(workflow.Document{
		SchemaVersion: 1,
		ID:            "parallel-change-fanout",
		Start:         "discover",
		Limits:        workflow.Limits{Attempts: 20, TestRuns: 8, ScopeWidens: 3},
		Roles:         map[string]workflow.Role{"coder": role(), "reviewer": role()},
		Artifacts:     map[string]workflow.ArtifactType{"report": {Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}},
		Stages: map[string]workflow.Stage{
			"discover": {
				Primitive: workflow.AgentTurn,
				Role:      "coder",
				Output:    "report",
			},
			"fan": {
				Primitive: workflow.FanOut,
				Join:      "join",
				Parallel:  true,
				BranchesFrom: &workflow.BranchesFromSpec{
					Stage: "discover",
					Field: "items",
					Min:   1,
					Max:   8,
				},
				BranchTemplate: &workflow.FanBranch{
					ID:         "worker",
					Start:      "edit",
					WriteScope: []string{"pkg/{{item.pkg}}/**"},
					Pass:       &passExpr,
				},
			},
			"edit": {
				Primitive:      workflow.AgentTurn,
				Role:           "coder",
				Output:         "report",
				ChangeProducer: true,
			},
			"branch_review": {
				Primitive: workflow.Review,
				Role:      "reviewer",
				Output:    "report",
			},
			"join": {
				Primitive: workflow.FanIn,
				Rule:      workflow.JoinAll,
				Failure:   "stop",
			},
			"stop": {
				Primitive: workflow.Escalate,
			},
		},
		Edges: []workflow.Edge{
			{From: "discover", To: "fan"},
			{From: "fan", To: "edit"},
			{From: "edit", To: "branch_review"},
			{From: "branch_review", To: "join"},
			{From: "join", To: "Done"},
			{From: "join", To: "stop"},
		},
	})
	if issues := workflow.Validate(doc); len(issues) > 0 {
		t.Fatalf("Validate parallel change fanout workflow: %v", issues)
	}

	j, err := engine.New("task-par-fan", 1, doc, baseTree)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	j.Base = baseSHA
	j.DeclaredFiles = []string{"pkg/alpha/a.go", "pkg/beta/b.go"}

	parentBinding, err := broker.Manager.Ensure(ctx, "run-1", "task-par-fan", baseSHA)
	if err != nil {
		t.Fatalf("Manager.Ensure: %v", err)
	}
	var mu sync.Mutex
	var branchWts []effects.BranchWorktree

	// Drive the job from Pending -> discover -> parallel FanOut (edit + branch_review) -> FanIn -> Done.
	j, _, err = DriveParallelFanOut(ctx, j, funcExecutor(func(ctx context.Context, d engine.Dispatch) (engine.Result, error) {
		switch d.Stage {
		case "discover":
			planOut, _ := json.Marshal(map[string]any{
				"items": []any{
					map[string]any{"pkg": "alpha", "file": "pkg/alpha/a.go", "body": "package p\nconst Alpha = 1\n"},
					map[string]any{"pkg": "beta", "file": "pkg/beta/b.go", "body": "package p\nconst Beta = 2\n"},
				},
			})
			return engine.Result{
				DispatchID: d.ID,
				TaskID:     d.TaskID,
				Tree:       d.Tree,
				Session:    d.Sessions[0].ID,
				Output:     planOut,
				ProducedAt: time.Now().UTC(),
			}, nil
		case "edit":
			itemMap, _ := d.BranchItem.(map[string]any)
			relFile, _ := itemMap["file"].(string)
			body, _ := itemMap["body"].(string)
			bw, err := ExecuteBranchWorktree(ctx, broker, parentBinding, d, func(branchBinding worktree.Binding) error {
				return os.WriteFile(filepath.Join(branchBinding.Path, relFile), []byte(body), 0o644)
			})
			if err != nil {
				return engine.Result{}, err
			}
			mu.Lock()
			branchWts = append(branchWts, bw)
			mu.Unlock()
			patchOut, _ := json.Marshal(map[string]any{"summary": "edited " + relFile})
			return engine.Result{
				DispatchID:   d.ID,
				TaskID:       d.TaskID,
				Tree:         d.Tree,
				BranchCommit: bw.CommitSHA,
				BranchRef:    bw.Ref,
				Session:      d.Sessions[0].ID,
				Output:       patchOut,
				ProducedAt:   time.Now().UTC(),
			}, nil
		case "branch_review":
			verdictOut, _ := json.Marshal(map[string]any{"approved": true})
			return engine.Result{
				DispatchID: d.ID,
				TaskID:     d.TaskID,
				Tree:       d.Tree,
				Session:    d.Sessions[0].ID,
				Output:     verdictOut,
				Reviews:    []engine.ReviewResult{{Session: d.Sessions[0].ID, Role: d.Sessions[0].Role, Output: verdictOut}},
				ProducedAt: time.Now().UTC(),
			}, nil
		default:
			t.Fatalf("unexpected stage %s", d.Stage)
			return engine.Result{}, nil
		}
	}), engine.Options{})
	if err != nil {
		t.Fatalf("DriveParallelFanOut failed: %v", err)
	}
	if j.State != domain.Done {
		t.Fatalf("expected job to reach Done after DriveParallelFanOut, got state=%s reason=%s detail=%s", j.State, j.Reason, j.EscalationDetail)
	}

	// Sort branchWts in deterministic branch order (branch-0, branch-1) and integrate into parentBinding.
	slices.SortFunc(branchWts, func(a, b effects.BranchWorktree) int {
		return strings.Compare(a.BranchID, b.BranchID)
	})
	if _, err := broker.IntegrateBranchWorktrees(ctx, parentBinding, branchWts); err != nil {
		t.Fatalf("IntegrateBranchWorktrees failed: %v", err)
	}

	// Verify both branch edits were integrated into parentBinding.Path in branch order.
	alphaBytes, err := os.ReadFile(filepath.Join(parentBinding.Path, "pkg/alpha/a.go"))
	if err != nil || !strings.Contains(string(alphaBytes), "Alpha = 1") {
		t.Fatalf("expected pkg/alpha/a.go to contain integrated branch-0 edit, got %q (err=%v)", string(alphaBytes), err)
	}
	betaBytes, err := os.ReadFile(filepath.Join(parentBinding.Path, "pkg/beta/b.go"))
	if err != nil || !strings.Contains(string(betaBytes), "Beta = 2") {
		t.Fatalf("expected pkg/beta/b.go to contain integrated branch-1 edit, got %q (err=%v)", string(betaBytes), err)
	}
}

func initFanOutTestRepo(t *testing.T) (string, string) {
	t.Helper()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=fae-test",
			"GIT_AUTHOR_EMAIL=fae@example.com",
			"GIT_COMMITTER_NAME=fae-test",
			"GIT_COMMITTER_EMAIL=fae@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	gitRun(repo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/test\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"pkg/alpha/a.go", "pkg/beta/b.go"} {
		abs := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte("package p\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(repo, "add", ".")
	gitRun(repo, "commit", "-m", "initial")
	return repo, gitRun(repo, "rev-parse", "HEAD")
}

func TestWave20Fixes_BrokerGoTestCheckAndCommitIdentity(t *testing.T) {
	ctx := context.Background()
	repo, baseSHA := initFanOutTestRepo(t)
	mgr := &worktree.Manager{
		Repository: repo,
		Root:       filepath.Join(t.TempDir(), "worktrees"),
	}
	broker := &effects.Broker{
		Manager: mgr,
		Allowed: [][]string{{"go", "test", "./..."}},
		CommitIdentity: &workflow.CommitIdentity{
			Name:  "Custom Author",
			Email: "custom@example.com",
		},
	}
	w, err := mgr.Ensure(ctx, "run-w20", "task-w20", baseSHA)
	if err != nil {
		t.Fatalf("mgr.Ensure: %v", err)
	}

	// 1. Bug 1B: HostCommand with Check != nil invoking `go test` without `-json -count=1` must NOT be rejected.
	spec := workflow.CheckSpec{
		Format:     "lines",
		CountField: "lines_checked",
	}
	ev, err := broker.Run(ctx, w, effects.Command{
		Argv:    []string{"go", "test", "./..."},
		Timeout: 30 * time.Second,
		Check:   &spec,
	})
	if err != nil {
		t.Fatalf("expected Check command invoking 'go test -list .' without -json -count=1 to succeed, got: %v (ev=%+v)", err, ev)
	}

	// 2. R4 Gap 7: ForkBranchWorktree with dirty parent working tree uses broker.CommitIdentity.GitEnv().
	if err := os.WriteFile(filepath.Join(w.Path, "pkg/alpha/a.go"), []byte("package p\n// dirty parent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bw, err := broker.ForkBranchWorktree(ctx, w, "fan", "b0", []string{"pkg/alpha/**"})
	if err != nil {
		t.Fatalf("ForkBranchWorktree: %v", err)
	}
	defer func() { _ = broker.CleanupBranchWorktree(ctx, bw) }()
	cmd := exec.CommandContext(ctx, "git", "-C", bw.Binding.Path, "log", "-1", "--format=%an <%ae>", bw.BaseCommit)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git log base commit: %v (%s)", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "Custom Author <custom@example.com>" {
		t.Fatalf("expected synthetic fanout base commit author 'Custom Author <custom@example.com>', got %q", got)
	}
}

func TestWave20Fixes_HostRunFailedNonVacuousCheckAndScopes(t *testing.T) {
	// 1. Bug 1D: hostRunFailed checks stage.Check.CountField < 1.
	checkStage := workflow.Stage{
		Primitive: workflow.HostCommand,
		Check: &workflow.CheckSpec{
			Format:     "json",
			CountField: "summary.checked",
		},
	}
	if !hostRunFailed(checkStage, map[string]any{"exit_code": 0, "summary": map[string]any{"checked": 0}}) {
		t.Fatal("expected hostRunFailed=true when Check.CountField is 0")
	}
	if hostRunFailed(checkStage, map[string]any{"exit_code": 0, "summary": map[string]any{"checked": 3}}) {
		t.Fatal("expected hostRunFailed=false when Check.CountField is 3 and exit_code is 0")
	}

	// 2. R4 Gap 3 & 4: editTask and resolveLiveTaskScope enforce DenyPaths and CheckDocumentWritesDisjoint,
	// and dispatchLiveScope narrows scope to d.WriteScope.
	doc := defaults.Workflow()
	doc.Stages["check_stage"] = workflow.Stage{
		Primitive:      workflow.HostCommand,
		Role:           "qa",
		Command:        []string{"./scripts/check.sh"},
		Weight:         1,
		TimeoutSeconds: 30,
		Check: &workflow.CheckSpec{
			Format:     "json",
			CountField: "checked",
			Writes:     []string{"reports/**"},
		},
	}
	job := engine.Job{
		TaskID:        "task-edit-check",
		Revision:      1,
		Document:      doc,
		Tree:          "tree-1",
		ExecutionKind: "live",
		DenyPaths:     []string{"secrets/**", "internal/secret.go"},
	}
	env := envelope{
		Job:       job,
		Admission: AdmitParams{ID: "task-edit-check", Title: "t", Content: "c", Priority: 20, DeclaredFiles: []string{"pkg/a.go"}},
	}
	snap := controlplane.JobSnapshot{ID: "task-edit-check", State: "Pending"}
	row := controlplane.BacklogItem{ID: "task-edit-check", Priority: 20}

	// Editing DeclaredFiles to match DenyPaths must be refused.
	deniedFiles := []string{"secrets/token.txt"}
	if _, err := editTask(snap, env, row, taskEdit{DeclaredFiles: &deniedFiles}, controlplane.Audit{}, "test"); err == nil {
		t.Fatal("expected editTask to refuse DeclaredFiles matching DenyPaths")
	}
	// Editing ReadOnlyFiles to match DenyPaths must be refused.
	deniedRO := []string{"internal/secret.go"}
	if _, err := editTask(snap, env, row, taskEdit{ReadOnlyFiles: &deniedRO}, controlplane.Audit{}, "test"); err == nil {
		t.Fatal("expected editTask to refuse ReadOnlyFiles matching DenyPaths")
	}
	// Editing DeclaredFiles to overlap Check.Writes must be refused.
	overlapWrites := []string{"reports/out.json"}
	if _, err := editTask(snap, env, row, taskEdit{DeclaredFiles: &overlapWrites}, controlplane.Audit{}, "test"); err == nil {
		t.Fatal("expected editTask to refuse DeclaredFiles overlapping stage Check.Writes")
	}

	// Pinned resolveLiveTaskScope enforces e.Job.DenyPaths on Files, TestFile, and ReadOnlyFiles.
	lx := &liveExecutor{
		config: LiveConfig{
			Tasks: map[string]LiveTaskConfig{
				"task-pinned-deny": {
					Files:    []string{"pkg/a.go"},
					TestFile: "secrets/secret_test.go",
					TestID:   "TestSecret",
				},
			},
		},
	}
	if _, err := lx.resolveLiveTaskScope("task-pinned-deny", nil, workflow.Stage{}, func() (envelope, error) {
		return env, nil
	}); err == nil {
		t.Fatal("expected pinned resolveLiveTaskScope to refuse TestFile matching DenyPaths")
	}

	// narrowScopeToWriteScope intersects task scope with branch WriteScope.
	narrowed := narrowScopeToWriteScope(liveTaskScope{
		Files:    []string{"pkg/alpha/a.go", "pkg/beta/b.go"},
		TestFile: "pkg/beta/b_test.go",
	}, []string{"pkg/alpha/**"})
	if !slices.Equal(narrowed.Files, []string{"pkg/alpha/a.go"}) || narrowed.TestFile != "" {
		t.Fatalf("expected narrowScopeToWriteScope to narrow Files to pkg/alpha/a.go and demote out-of-scope TestFile, got %+v", narrowed)
	}

	// parseTargetPolicy falls back to top-level DocumentGlobs and DocumentChecks.
	lp, err := parseTargetPolicy(TargetConfig{
		SchemaVersion:  4,
		DocumentGlobs:  []string{"docs/**"},
		DocumentChecks: []DocumentCheck{{ID: "doc-lint", Command: []string{"./lint.sh"}, Format: "lines", CountField: "checked"}},
	})
	if err != nil || len(lp.DocumentGlobs) != 1 || len(lp.DocumentChecks) != 1 {
		t.Fatalf("expected parseTargetPolicy to populate DocumentGlobs and DocumentChecks from top-level TargetConfig, got lp=%+v err=%v", lp, err)
	}
}
