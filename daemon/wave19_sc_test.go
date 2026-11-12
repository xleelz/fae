package daemon

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

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/effects"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/execution"
	agentruntime "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
	"github.com/xleelz/fae/worktree"
)

func TestTargetConfigReadRootsContentPolicyIdentityAndPushValidation(t *testing.T) {
	ws := t.TempDir()
	extDir := t.TempDir()

	baseTarget := func() TargetConfig {
		return TargetConfig{
			SchemaVersion:     4,
			Workspace:         ws,
			IntegrationRef:    "refs/heads/fae/main",
			ProtectedBranches: []string{"refs/heads/fae/main"},
			AllowedCommands:   [][]string{{"go", "test", "./..."}, {"diff", "-u", "{{read-root}}", "local.txt"}},
		}
	}

	// 1. ReadRoots: commit XOR digest, and path must be outside workspace
	c := baseTarget()
	c.ReadRoots = map[string]ReadRootConfig{
		"both": {Path: extDir, Commit: "0123456789abcdef0123456789abcdef01234567", Digest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
	}
	raw, _ := json.Marshal(c)
	if _, err := parseTarget(raw); err == nil || !strings.Contains(err.Error(), "exactly one of commit or digest") {
		t.Fatalf("expected commit XOR digest error, got: %v", err)
	}

	c = baseTarget()
	c.ReadRoots = map[string]ReadRootConfig{
		"neither": {Path: extDir},
	}
	raw, _ = json.Marshal(c)
	if _, err := parseTarget(raw); err == nil || !strings.Contains(err.Error(), "exactly one of commit or digest") {
		t.Fatalf("expected commit XOR digest error when neither set, got: %v", err)
	}

	c = baseTarget()
	c.ReadRoots = map[string]ReadRootConfig{
		"inside_ws": {Path: filepath.Join(ws, "sub"), Digest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
	}
	raw, _ = json.Marshal(c)
	if _, err := parseTarget(raw); err == nil || !strings.Contains(err.Error(), "outside the target workspace") {
		t.Fatalf("expected read_root inside workspace to be refused, got: %v", err)
	}

	// 2. ContentPolicy: invalid regex refused
	c = baseTarget()
	c.ContentPolicy = ContentPolicyConfig{
		DenyPatterns: []string{"[unclosed"},
	}
	raw, _ = json.Marshal(c)
	if _, err := parseTarget(raw); err == nil || !strings.Contains(err.Error(), "content_policy.deny_patterns[0]") {
		t.Fatalf("expected invalid deny_patterns regex to be refused, got: %v", err)
	}

	// 3. PushConfig: mode must be fast-forward-only
	c = baseTarget()
	c.Push = &PushConfig{Remote: "origin", Branches: []string{"main"}, Mode: "force"}
	raw, _ = json.Marshal(c)
	if _, err := parseTarget(raw); err == nil || !strings.Contains(err.Error(), "fast-forward-only") {
		t.Fatalf("expected non-fast-forward push mode to be refused, got: %v", err)
	}

	// 4. Valid configuration with ReadRoots, ContentPolicy, DenyPaths, CommitIdentity, and Push
	c = baseTarget()
	c.ReadRoots = map[string]ReadRootConfig{
		"ext": {Path: extDir, Digest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Deny: []string{"secret/**"}},
	}
	c.ContentPolicy = ContentPolicyConfig{
		DenyLiterals: []string{"TOP_SECRET_VALUE"},
		DenyPatterns: []string{`token_[0-9]+`},
	}
	c.DenyPaths = []string{"private/**", "*.pem"}
	c.CommitIdentity = &CommitIdentityConfig{
		Name:     "Fae Bot",
		Email:    "bot@fae.local",
		CoAuthor: "Operator <op@fae.local>",
	}
	c.Push = &PushConfig{
		Remote:   "origin",
		Branches: []string{"main"},
		Mode:     "fast-forward-only",
	}
	raw, _ = json.Marshal(c)
	if _, err := parseTarget(raw); err != nil {
		t.Fatalf("expected valid TargetConfig to parse, got: %v", err)
	}
}

func TestReadRootVerificationMutationRefusalAndHostCommandBinding(t *testing.T) {
	ctx := context.Background()
	gitRoot := t.TempDir()
	digestRoot := t.TempDir()

	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}

	// Setup git-pinned read root
	runGit(gitRoot, "init", "-q")
	if err := os.WriteFile(filepath.Join(gitRoot, "lib.go"), []byte("package lib\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(gitRoot, "add", "lib.go")
	runGit(gitRoot, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-q", "-m", "v1")
	pinnedSHA := runGit(gitRoot, "rev-parse", "HEAD")

	// Setup digest-pinned read root
	if err := os.WriteFile(filepath.Join(digestRoot, "data.txt"), []byte("immutable content\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(digestRoot, "ignored.secret"), []byte("ignored by deny\n"), 0644); err != nil {
		t.Fatal(err)
	}
	pinnedDigest, err := ComputeReadRootDigest(digestRoot, []string{"*.secret"}, nil)
	if err != nil {
		t.Fatalf("ComputeReadRootDigest: %v", err)
	}

	// Verify both pass initially
	gotSHA, err := VerifyReadRoot(ctx, "git_ext", ReadRootConfig{Path: gitRoot, Commit: pinnedSHA}, nil)
	if err != nil || gotSHA != pinnedSHA {
		t.Fatalf("VerifyReadRoot git_ext: got %q, err=%v", gotSHA, err)
	}
	gotDigest, err := VerifyReadRoot(ctx, "dig_ext", ReadRootConfig{Path: digestRoot, Digest: pinnedDigest, Deny: []string{"*.secret"}}, nil)
	if err != nil || gotDigest != pinnedDigest {
		t.Fatalf("VerifyReadRoot dig_ext: got %q, err=%v", gotDigest, err)
	}

	// Mutating an ignored file in digestRoot does not change digest
	if err := os.WriteFile(filepath.Join(digestRoot, "ignored.secret"), []byte("changed but denied\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyReadRoot(ctx, "dig_ext", ReadRootConfig{Path: digestRoot, Digest: pinnedDigest, Deny: []string{"*.secret"}}, nil); err != nil {
		t.Fatalf("expected denied file change in digestRoot to be ignored, got: %v", err)
	}

	// Mutating a non-denied file in digestRoot triggers read-root-changed: dig_ext
	if err := os.WriteFile(filepath.Join(digestRoot, "data.txt"), []byte("tampered!\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyReadRoot(ctx, "dig_ext", ReadRootConfig{Path: digestRoot, Digest: pinnedDigest, Deny: []string{"*.secret"}}, nil); err == nil || !strings.Contains(err.Error(), "read-root-changed: dig_ext") {
		t.Fatalf("expected read-root-changed: dig_ext, got: %v", err)
	}
	// Restore data.txt
	if err := os.WriteFile(filepath.Join(digestRoot, "data.txt"), []byte("immutable content\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Symlink escaping the read root is refused
	outsideFile := filepath.Join(t.TempDir(), "outside.txt")
	_ = os.WriteFile(outsideFile, []byte("outside"), 0644)
	symlinkPath := filepath.Join(digestRoot, "escape_link")
	if err := os.Symlink(outsideFile, symlinkPath); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyReadRoot(ctx, "dig_ext", ReadRootConfig{Path: digestRoot, Digest: pinnedDigest, Deny: []string{"*.secret"}}, nil); err == nil || !strings.Contains(err.Error(), "points outside root") {
		t.Fatalf("expected escaping symlink in read root to be refused, got: %v", err)
	}
	_ = os.Remove(symlinkPath)

	// Verify HostCommand authorizeCommandWithTask binds {{ext}} to verified read root path and refuses when root changes
	stage := workflow.Stage{
		Primitive: workflow.HostCommand,
		Role:      "coder",
		Command:   []string{"diff", "-u", "{{ext}}", "local.txt"},
		ArgTemplates: map[string]workflow.ArgTemplate{
			"ext": {Kind: "read-root", Key: "git_ext"},
		},
	}
	doc := defaults.Workflow()
	doc.Roles["coder"] = workflow.Role{
		ReadRoots: []string{"git_ext"},
		Reads:     &workflow.ReadPolicy{Allow: []string{"src/**"}},
	}
	target := &TargetConfig{
		AllowedCommands: [][]string{
			{"diff", "-u", "{{read-root}}", "local.txt"},
		},
		ReadRoots: map[string]ReadRootConfig{
			"git_ext": {Path: gitRoot, Commit: pinnedSHA},
		},
	}
	x := &liveExecutor{
		config: LiveConfig{
			AllowedCommands: [][]string{
				{"diff", "-u", "{{ext}}", "local.txt"},
			},
		},
		broker: &effects.Broker{},
	}
	env := envelope{
		Job: engine.Job{
			Document:  doc,
			ReadRoots: map[string]string{"git_ext": pinnedSHA},
		},
	}
	d := engine.Dispatch{
		TaskID: "task-1",
		Stage:  "check",
		Config: stage,
		Argv:   []string{"diff", "-u", "{{ext}}", "local.txt"},
	}
	argv, _, verifiedRoots, _, err := x.authorizeCommandWithTask(ctx, d, env, target)
	if err != nil {
		t.Fatalf("authorizeCommandWithTask valid: %v", err)
	}
	if !slices.Equal(argv, []string{"diff", "-u", gitRoot, "local.txt"}) {
		t.Fatalf("expected bound read-root argv, got %v", argv)
	}
	if verifiedRoots["git_ext"] != pinnedSHA {
		t.Fatalf("expected verifiedRoots[git_ext]=%s, got %v", pinnedSHA, verifiedRoots)
	}

	// Dirty the git read root and verify authorizeCommandWithTask refuses with read-root-changed: git_ext
	if err := os.WriteFile(filepath.Join(gitRoot, "dirty.txt"), []byte("uncommitted"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := x.authorizeCommandWithTask(ctx, d, env, target); err == nil || !strings.Contains(err.Error(), "read-root-changed: git_ext") {
		t.Fatalf("expected read-root-changed: git_ext when git root dirtied, got: %v", err)
	}
}

func TestExecutorEnforcesReadRootReverificationStageInputReadsAndProvenance(t *testing.T) {
	ctx := context.Background()
	doc := defaults.Workflow()
	plannerRole := doc.Roles["planner"]
	plannerRole.ReadRoots = []string{"sdk"}
	plannerRole.Reads = &workflow.ReadPolicy{Allow: []string{"src/**", "docs/**"}}
	doc.Roles["planner"] = plannerRole

	coderRole := doc.Roles["coder"]
	coderRole.Briefs = map[string]string{"default": "implement"}
	coderRole.Reads = &workflow.ReadPolicy{Allow: []string{"src/**"}, Deny: []string{"src/secret/**"}}
	doc.Roles["coder"] = coderRole
	doc.Stages["plan"] = workflow.Stage{
		Primitive: workflow.AgentTurn,
		Role:      "planner",
		Output:    "Spec",
	}
	doc.Stages["code"] = workflow.Stage{
		Primitive: workflow.AgentTurn,
		Role:      "coder",
		Inputs:    []workflow.Input{{Name: "spec", Type: "Spec", Stage: "plan", Version: 1}},
		Output:    "Spec",
	}

	var auditedRefusals []string
	rootMutated := false
	binding := execution.Binding{
		Worktree:  t.TempDir(),
		Document:  doc,
		Mode:      "local",
		Scope:     execution.TaskScope{Files: []string{"src/main.go", "src/secret/key.go", "private/cert.pem"}},
		TaskReads: &workflow.ReadPolicy{Deny: []string{"src/internal/**"}},
		DenyPaths: []string{"private/**"},
		ReadRoots: map[string]string{"sdk": "commit-abc123"},
		OnRefusedRead: func(task, role, path string) {
			auditedRefusals = append(auditedRefusals, task+":"+role+":"+path)
		},
		VerifyReadRoot: func(ctx context.Context, name string) (string, error) {
			if rootMutated {
				return "", os.ErrInvalid
			}
			return "commit-abc123", nil
		},
	}

	exec := &execution.Executor{
		Resolve: func(context.Context, engine.Dispatch) (execution.Binding, error) {
			return binding, nil
		},
		Admission: nopExecutionAdmission{},
		Tree: func(context.Context, execution.Binding) (string, error) {
			return "tree-1", nil
		},
		Runtime: fakeRuntimeRunner{
			out: agentruntime.Collected{
				Output:           defaults.HappyScript()["spec"][0].Output,
				ProcessesRetired: true,
			},
		},
	}

	// 1. Run planner turn: verifies read root "sdk" and records ReadRoots and Reads in Result
	planDispatch := engine.Dispatch{
		ID:            "d-plan",
		TaskID:        "task-1",
		ExecutionKind: "live",
		Stage:         "plan",
		Tree:          "tree-1",
		Config:        doc.Stages["plan"],
		Sessions:      []engine.Session{{ID: "s-plan", Role: "planner", Route: doc.Roles["planner"].Routes[0]}},
	}
	res, err := exec.Execute(ctx, planDispatch)
	if err != nil || res.Class != "" {
		t.Fatalf("Execute plan turn failed: res=%+v, err=%v", res, err)
	}
	if res.ReadRoots["sdk"] != "commit-abc123" {
		t.Fatalf("expected res.ReadRoots[sdk]=commit-abc123, got %v", res.ReadRoots)
	}
	if res.Reads == nil || !res.Reads.Allows("src/main.go") || res.Reads.Allows("private/cert.pem") {
		t.Fatalf("unexpected res.Reads on plan turn: %+v", res.Reads)
	}

	// 2. Mutate read root before turn: refused with read-root-changed: sdk
	rootMutated = true
	resMutated, err := exec.Execute(ctx, planDispatch)
	if err == nil || resMutated.Class != engine.ClassRefusedBeforeLaunch || !strings.Contains(resMutated.Detail, "read-root-changed: sdk") {
		t.Fatalf("expected ClassRefusedBeforeLaunch with read-root-changed: sdk, got res=%+v err=%v", resMutated, err)
	}
	rootMutated = false

	// 3. Stage input binding restriction: coder (Allow: src/**) cannot bind output from planner (Allow: src/**, docs/**)
	binding.Provenance = []engine.Provenance{
		{
			DispatchID: "d-plan",
			Stage:      "plan",
			Role:       "planner",
			Reads:      res.Reads,
		},
	}
	codeDispatch := engine.Dispatch{
		ID:            "d-code",
		TaskID:        "task-1",
		ExecutionKind: "live",
		Stage:         "code",
		Tree:          "tree-1",
		Config:        doc.Stages["code"],
		Inputs: []workflow.Evidence{
			{ID: "art-1", Type: "Spec", Version: 1, Stage: "plan", Role: "planner"},
		},
		Sessions: []engine.Session{{ID: "s-code", Role: "coder", Route: doc.Roles["coder"].Routes[0]}},
	}
	resRefusedBind, err := exec.Execute(ctx, codeDispatch)
	if err == nil || resRefusedBind.Class != engine.ClassRefusedBeforeLaunch || !strings.Contains(resRefusedBind.Detail, "read-policy") {
		t.Fatalf("expected stage input binding from broader read policy to be refused, got res=%+v err=%v", resRefusedBind, err)
	}

	// 4. When producer had narrower or equal read policy, coder turn succeeds and filters inline scope files with audit
	binding.Provenance[0].Reads = &workflow.ReadPolicy{Allow: []string{"src/**"}, Deny: []string{"src/secret/**", "src/internal/**", "private/**"}}
	auditedRefusals = nil
	resCode, err := exec.Execute(ctx, codeDispatch)
	if err != nil || resCode.Class != "" {
		t.Fatalf("expected coder turn with compatible producer read policy to succeed, got res=%+v err=%v", resCode, err)
	}
	if !slices.Contains(auditedRefusals, "task-1:coder:src/secret/key.go") || !slices.Contains(auditedRefusals, "task-1:coder:private/cert.pem") {
		t.Fatalf("expected refused inline scope files to be audited, got %v", auditedRefusals)
	}
}

func TestSecretRedactionIncludesContentPolicyAndDenyPaths(t *testing.T) {
	runDir := t.TempDir()
	credFile := filepath.Join(runDir, "cred")
	planeCred := filepath.Join(runDir, "plane-credential")
	if err := os.WriteFile(credFile, []byte("0123456789abcdef0123456789abcdef"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planeCred, []byte("fedcba9876543210fedcba9876543210"), 0600); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{layout: Layout{Credential: credFile, Run: runDir}}
	d.RegisterContentPolicy(ContentPolicyConfig{
		DenyLiterals: []string{"MY_CUSTOM_ORG_SECRET_TOKEN"},
		DenyPatterns: []string{`INTERNAL_KEY_[0-9]{4}`},
	})
	input := "error connecting with MY_CUSTOM_ORG_SECRET_TOKEN and INTERNAL_KEY_9876 in output"
	secrets, err := d.failureSecrets(nil)
	if err != nil {
		t.Fatalf("failureSecrets: %v", err)
	}
	redacted := secrets.redactString(input)
	if strings.Contains(redacted, "MY_CUSTOM_ORG_SECRET_TOKEN") || strings.Contains(redacted, "INTERNAL_KEY_9876") {
		t.Fatalf("expected content_policy literals and patterns to be redacted, got: %s", redacted)
	}
	if !strings.Contains(strings.ToUpper(redacted), "[REDACTED]") {
		t.Fatalf("expected [REDACTED] marker in output, got: %s", redacted)
	}

	// Also verify checkScopeDenyPaths refuses operator/derived scope files matching deny_paths
	if err := checkScopeDenyPaths([]string{"src/ok.go", "secrets/prod.env"}, "", []string{"secrets/**"}); err == nil || !strings.Contains(err.Error(), "deny_paths[0]") {
		t.Fatalf("expected checkScopeDenyPaths to refuse with deny_paths[0], got: %v", err)
	}
}

func TestLocalGitPushFastForwardContentScanAndIdempotence(t *testing.T) {
	ctx := context.Background()
	remoteBare := t.TempDir()
	ws := t.TempDir()

	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}

	runGit(remoteBare, "init", "--bare", "-q", "-b", "main")
	runGit(ws, "init", "-q", "-b", "main")
	runGit(ws, "remote", "add", "origin", remoteBare)
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(ws, "add", "main.go")
	runGit(ws, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-q", "-m", "initial")
	baseSHA := runGit(ws, "rev-parse", "HEAD")
	runGit(ws, "push", "-q", "origin", "main")

	// Create a fast-forward commit on ws
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\nfunc V2() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(ws, "add", "main.go")
	runGit(ws, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-q", "-m", "v2 commit")
	v2SHA := runGit(ws, "rev-parse", "HEAD")

	doc := defaults.Workflow()
	pushStage := workflow.Stage{
		Primitive:  workflow.ExternalEffect,
		EffectKind: "git-push",
		Subject:    "git-push",
		Pins:       map[string]string{"remote": "origin", "branch": "main"},
		Output:     "Receipt",
	}
	env := envelope{
		Job: engine.Job{
			TaskID:      "task-push",
			Base:        baseSHA,
			Document:    doc,
			Disposition: domain.Disposition{Mode: domain.Local},
			Evidence: []workflow.Evidence{
				{ID: "ev-1", Type: "Commit", Fields: map[string]any{"sha": v2SHA}},
			},
		},
	}
	dispatch := engine.Dispatch{
		ID:     "task-push/1/push",
		TaskID: "task-push",
		Stage:  "push",
		Tree:   runGit(ws, "rev-parse", "HEAD^{tree}"),
		Config: pushStage,
		Inputs: env.Job.Evidence,
	}
	w := worktree.Binding{Path: ws, Branch: "main"}
	x := &liveExecutor{}

	// 1. Refuse when target.Push is nil or branch is not in target.Push.Branches
	if _, err := x.executeGitPush(ctx, dispatch, w, env, &TargetConfig{}); err == nil || !strings.Contains(err.Error(), "target.push is not configured") {
		t.Fatalf("expected refusal when target.Push is nil, got: %v", err)
	}
	badBranchTarget := &TargetConfig{
		Push: &PushConfig{Remote: "origin", Branches: []string{"release"}, Mode: "fast-forward-only"},
	}
	if _, err := x.executeGitPush(ctx, dispatch, w, env, badBranchTarget); err == nil || !strings.Contains(err.Error(), "not in target.push.branches") {
		t.Fatalf("expected refusal when branch not in push.branches, got: %v", err)
	}

	// 2. Refuse when commit range <remote_sha>..<sha> violates ContentPolicy (reporting index only!)
	policyTarget := &TargetConfig{
		Push: &PushConfig{Remote: "origin", Branches: []string{"main"}, Mode: "fast-forward-only"},
		ContentPolicy: ContentPolicyConfig{
			DenyLiterals: []string{"func V2"},
		},
	}
	if _, err := x.executeGitPush(ctx, dispatch, w, env, policyTarget); err == nil || !strings.Contains(err.Error(), "content_policy.deny_literals[0]") || strings.Contains(err.Error(), "func V2") {
		t.Fatalf("expected content policy refusal with index only on git-push, got: %v", err)
	}

	// 3. Clean fast-forward push succeeds and advances remote ref
	cleanTarget := &TargetConfig{
		Push: &PushConfig{Remote: "origin", Branches: []string{"main"}, Mode: "fast-forward-only"},
	}
	receipt, err := x.executeGitPush(ctx, dispatch, w, env, cleanTarget)
	if err != nil {
		t.Fatalf("executeGitPush clean failed: %v", err)
	}
	if receipt["reconciliation_state"] != "reconciled" || receipt["sha"] != v2SHA {
		t.Fatalf("unexpected git-push receipt: %+v", receipt)
	}
	remoteHead := runGit(remoteBare, "rev-parse", "refs/heads/main")
	if remoteHead != v2SHA {
		t.Fatalf("expected remote refs/heads/main at %s, got %s", v2SHA, remoteHead)
	}

	// 4. Idempotent retry on same (remote, branch, sha) adopts without error
	receiptRetry, err := x.executeGitPush(ctx, dispatch, w, env, cleanTarget)
	if err != nil || receiptRetry["reconciliation_state"] != "adopted" {
		t.Fatalf("expected idempotent retry to return adopted, got %+v, err=%v", receiptRetry, err)
	}

	// 5. Non-fast-forward push (attempting to push baseSHA when remote is at v2SHA) is refused
	envNonFF := env
	envNonFF.Job.Evidence = []workflow.Evidence{{ID: "ev-0", Type: "Commit", Fields: map[string]any{"sha": baseSHA}}}
	dispatchNonFF := dispatch
	dispatchNonFF.Inputs = envNonFF.Job.Evidence
	if _, err := x.executeGitPush(ctx, dispatchNonFF, w, envNonFF, cleanTarget); err == nil || !strings.Contains(err.Error(), "non-fast-forward") {
		t.Fatalf("expected non-fast-forward push to be refused, got: %v", err)
	}

	// 6. Refuse when workflow has Gate/Review stages in Document.Stages but Job.Evidence has NOT recorded passing evidence yet
	envNoGateEvidence := env
	envNoGateEvidence.Job.Evidence = []workflow.Evidence{
		{ID: "ev-spec", Type: "Spec", Stage: "spec", Fields: map[string]any{"summary": "not a gate"}},
	}
	envNoGateEvidence.Job.Provenance = nil
	dispatchNoGate := dispatch
	dispatchNoGate.Inputs = envNoGateEvidence.Job.Evidence
	if _, err := x.executeGitPush(ctx, dispatchNoGate, w, envNoGateEvidence, cleanTarget); err == nil || !strings.Contains(err.Error(), "not preceded by passing protected-commit gates") {
		t.Fatalf("expected refusal when Gate stage exists in Document.Stages without recorded evidence in Job.Evidence, got: %v", err)
	}
}

func TestAdmissionRefusesReadOnlyFilesMatchingTargetDenyPaths(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)

	tc := targetV4(cfg.Workspace)
	tc.DenyPaths = []string{"secrets/**", "**/*.pem"}
	if _, err := call(t, d, "TargetConfigPut", TargetPutParams{ID: "sec-target", Document: tc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	targetMeta, _, err := d.store.GetTarget(ctx, "sec-target", 1)
	if err != nil {
		t.Fatal(err)
	}

	// verifyAdmissionTargetRoots refuses when ReadOnlyFiles matches target.DenyPaths
	_, _, err = d.verifyAdmissionTargetRoots(ctx, AdmitParams{
		ID:             "ro-deny",
		TargetConfig:   "sec-target",
		TargetRevision: 1,
		TargetHash:     targetMeta.Hash,
		DeclaredFiles:  []string{"src/main.go"},
		ReadOnlyFiles:  []string{"secrets/api.key"},
	})
	if err == nil || !strings.Contains(err.Error(), "matches target deny_paths (FAE-SEC-009)") {
		t.Fatalf("expected ReadOnlyFiles matching target.DenyPaths to be refused, got: %v", err)
	}

	// Full Admit RPC also refuses when read_only_files matches target.DenyPaths
	if _, err := call(t, d, "Admit", AdmitParams{
		ID:                 "ro-deny-rpc",
		Tree:               "tree",
		Content:            "work",
		TargetConfig:       "sec-target",
		TargetRevision:     1,
		DeclaredFiles:      []string{"src/main.go"},
		ReadOnlyFiles:      []string{"certs/prod.pem"},
		AuthorizingMessage: "message",
	}, "ro-deny-rpc"); err == nil || !strings.Contains(err.Error(), "deny_paths") {
		t.Fatalf("expected Admit with denied read_only_files to fail, got: %v", err)
	}
}

type enrichingExecutor struct {
	base  engine.Executor
	roots map[string]string
	reads *workflow.ReadPolicy
}

func (e enrichingExecutor) Execute(ctx context.Context, disp engine.Dispatch) (engine.Result, error) {
	res, err := e.base.Execute(ctx, disp)
	if err == nil {
		res.ReadRoots = e.roots
		res.Reads = e.reads
	}
	return res, err
}

func TestCompletedTaskProvenanceAndHistoryPreserveReadRootsAndReadsAcrossRestart(t *testing.T) {
	cfg, ctx := fixture(t)
	script := defaults.HappyScript()
	baseExec := engine.NewScripted(script)
	wantRoots := map[string]string{"ext_sdk": "0123456789abcdef0123456789abcdef01234567"}
	wantReads := &workflow.ReadPolicy{Allow: []string{"src/**"}, Deny: []string{"src/secret/**"}}
	cfg.Executor = enrichingExecutor{
		base:  baseExec,
		roots: wantRoots,
		reads: wantReads,
	}

	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)

	if _, err := call(t, d, "Admit", AdmitParams{
		ID:                 "done-prov",
		Tree:               "tree",
		Reads:              wantReads,
		AuthorizingMessage: "message",
	}, "done-prov"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "done-prov", "Done")

	// Restart daemon so d.jobs does not hold terminal task "done-prov" (FAE-CP-009).
	stop(t, d)
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	_, inMem := d.jobs["done-prov"]
	d.mu.Unlock()
	if inMem {
		t.Fatalf("expected terminal task done-prov not to be in d.jobs after restart")
	}

	// 1. TaskProvenance from controlplane preserves ReadRoots and Reads via artifacts.provenance
	provRows, err := d.store.TaskProvenance(ctx, "done-prov", time.Time{}, 100)
	if err != nil || len(provRows) == 0 {
		t.Fatalf("TaskProvenance: len=%d, err=%v", len(provRows), err)
	}
	for _, row := range provRows {
		if row.ReadRoots["ext_sdk"] != wantRoots["ext_sdk"] {
			t.Fatalf("TaskProvenance row %s missing ReadRoots: %+v", row.Stage, row.ReadRoots)
		}
		if row.Reads == nil || !slices.Equal(row.Reads.Allow, wantReads.Allow) || !slices.Equal(row.Reads.Deny, wantReads.Deny) {
			t.Fatalf("TaskProvenance row %s missing Reads: %+v", row.Stage, row.Reads)
		}
	}

	// 2. ProvenanceChain preserves ReadRoots and Reads
	hops, err := d.store.ProvenanceChain(ctx, "scripted-commit", 100)
	if err != nil || len(hops) == 0 {
		t.Fatalf("ProvenanceChain: len=%d, err=%v", len(hops), err)
	}
	for _, hop := range hops {
		if hop.ReadRoots["ext_sdk"] != wantRoots["ext_sdk"] || hop.Reads == nil {
			t.Fatalf("ProvenanceChain hop %s missing ReadRoots/Reads: %+v", hop.Stage, hop.ProvenanceRecord)
		}
	}

	// 3. History (Summary: true) preserves ReadRoots and Reads on ArtifactReferences after restart
	rSum, err := call(t, d, "History", HistoryParams{ID: "done-prov", Summary: true, Limit: 100}, "")
	if err != nil {
		t.Fatalf("History Summary=true: %v", err)
	}
	var sumPage HistoryPage
	decodeResult(t, rSum, nil, &sumPage)
	if len(sumPage.ArtifactReferences) == 0 {
		t.Fatalf("expected ArtifactReferences in History Summary=true")
	}
	for _, ref := range sumPage.ArtifactReferences {
		if ref.ReadRoots["ext_sdk"] != wantRoots["ext_sdk"] || ref.Reads == nil {
			t.Fatalf("History Summary=true ref %s missing ReadRoots/Reads: %+v", ref.Stage, ref)
		}
	}

	// 4. History (Records: true) preserves ReadRoots and Reads on Artifacts after restart
	rRec, err := call(t, d, "History", HistoryParams{ID: "done-prov", Records: true, Limit: 100}, "")
	if err != nil {
		t.Fatalf("History Records=true: %v", err)
	}
	var recPage RecordsPage
	decodeResult(t, rRec, nil, &recPage)
	if len(recPage.Artifacts) == 0 {
		t.Fatalf("expected Artifacts in History Records=true")
	}
	for _, art := range recPage.Artifacts {
		if art.ReadRoots["ext_sdk"] != wantRoots["ext_sdk"] || art.Reads == nil {
			t.Fatalf("History Records=true artifact %s missing ReadRoots/Reads: %+v", art.Stage, art)
		}
	}
}

func TestGitPushProjectsPerformedIntoExternalEffects(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)

	remoteBare := t.TempDir()
	ws := t.TempDir()
	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}

	runGit(remoteBare, "init", "--bare", "-q", "-b", "main")
	runGit(ws, "init", "-q", "-b", "main")
	runGit(ws, "remote", "add", "origin", remoteBare)
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(ws, "add", "main.go")
	runGit(ws, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-q", "-m", "initial")
	baseSHA := runGit(ws, "rev-parse", "HEAD")
	runGit(ws, "push", "-q", "origin", "main")

	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\nfunc Pushed() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(ws, "add", "main.go")
	runGit(ws, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-q", "-m", "pushed commit")
	pushSHA := runGit(ws, "rev-parse", "HEAD")

	doc := defaults.Workflow()
	pushStage := workflow.Stage{
		Primitive:  workflow.ExternalEffect,
		EffectKind: "git-push",
		Subject:    "git-push",
		Pins:       map[string]string{"remote": "origin", "branch": "main"},
		Output:     "Receipt",
	}
	env := envelope{
		Job: engine.Job{
			TaskID:      "task-push-plane",
			Base:        baseSHA,
			Document:    doc,
			Disposition: domain.Disposition{Mode: domain.Local},
			Evidence: []workflow.Evidence{
				{ID: "ev-commit", Type: "Commit", Stage: "commit", Fields: map[string]any{"sha": pushSHA}},
			},
		},
	}
	dispatch := engine.Dispatch{
		ID:     "task-push-plane/1/push",
		TaskID: "task-push-plane",
		Stage:  "push",
		Tree:   runGit(ws, "rev-parse", "HEAD^{tree}"),
		Config: pushStage,
		Inputs: env.Job.Evidence,
	}
	w := worktree.Binding{Path: ws, Branch: "main"}
	x := &liveExecutor{daemon: d}
	target := &TargetConfig{
		Push: &PushConfig{Remote: "origin", Branches: []string{"main"}, Mode: "fast-forward-only"},
	}

	receipt, err := x.executeGitPush(ctx, dispatch, w, env, target)
	if err != nil {
		t.Fatalf("executeGitPush with plane store failed: %v", err)
	}
	idempKey, _ := receipt["idempotency_key"].(string)
	if idempKey == "" {
		t.Fatalf("expected non-empty idempotency_key in receipt: %+v", receipt)
	}
	if n := countRows(t, d, `SELECT COUNT(*) FROM external_effects WHERE effect_id='`+idempKey+`' AND task_id='task-push-plane' AND tool='git-push' AND outcome='performed'`); n != 1 {
		t.Fatalf("expected 1 performed git-push row in external_effects, got %d", n)
	}
}

type nopExecutionAdmission struct{}

func (nopExecutionAdmission) Acquire(context.Context, engine.Dispatch, execution.Binding, engine.Session) (func() error, error) {
	return func() error { return nil }, nil
}

type fakeRuntimeRunner struct {
	out agentruntime.Collected
	err error
}

func (f fakeRuntimeRunner) Run(context.Context, agentruntime.Launch) (agentruntime.Collected, error) {
	return f.out, f.err
}
