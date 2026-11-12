package daemon

import (
	"encoding/json"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/workflow"
	"os"
	"path/filepath"
	"testing"
)

func targetFixture(workspace string) TargetConfig {
	c := TargetConfig{SchemaVersion: 1, Workspace: workspace, IntegrationRef: "refs/heads/main", ProtectedBranches: []string{"refs/heads/main"}}
	for _, s := range defaults.Workflow().Stages {
		if s.Primitive == workflow.HostCommand {
			c.AllowedCommands = append(c.AllowedCommands, s.Command)
		}
	}
	return c
}
func TestTargetSchemaAndExactCommandPolicy(t *testing.T) {
	c := targetFixture("/repo")
	b, _ := json.Marshal(c)
	if _, e := parseTarget(b); e != nil {
		t.Fatal(e)
	}
	for _, mutate := range []func(*TargetConfig){func(c *TargetConfig) { c.SchemaVersion = 2 }, func(c *TargetConfig) { c.Workspace = "relative" }, func(c *TargetConfig) { c.ProtectedBranches = nil }, func(c *TargetConfig) { c.AllowedCommands = nil }, func(c *TargetConfig) { c.AllowedCommands = [][]string{{"sh", "{{script}}"}} }, func(c *TargetConfig) { c.ProtectedBranches = append(c.ProtectedBranches, "refs/heads/../bad") }} {
		bad := targetFixture("/repo")
		mutate(&bad)
		raw, _ := json.Marshal(bad)
		if _, e := parseTarget(raw); e == nil {
			t.Fatal("invalid target accepted", string(raw))
		}
	}
	if _, e := parseTarget([]byte(`{"schema_version":1,"unknown":true}`)); e == nil {
		t.Fatal("unknown policy accepted")
	}
	if !targetAllows(c, c.AllowedCommands[0]) || targetAllows(c, []string{"sh", "-c", "go test ./..."}) {
		t.Fatal("command shape")
	}
	for _, bound := range []bool{false, true} {
		for _, allowed := range []bool{false, true} {
			if TargetCommandAuthorized(bound, allowed) != (bound && allowed) {
				t.Fatal("guard")
			}
		}
	}
}
func TestTargetWorkspaceBinding(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := &Daemon{config: Config{Workspace: root}}
	c := targetFixture(root)
	if err = d.validateTargetBinding(c); err != nil {
		t.Fatal(err)
	}
	c.Workspace = filepath.Join(root, "missing")
	if d.validateTargetBinding(c) == nil {
		t.Fatal("missing binding accepted")
	}
	link := filepath.Join(root, "alias")
	if err = os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	c.Workspace = link
	if d.validateTargetBinding(c) == nil {
		t.Fatal("symlink accepted")
	}
}
func TestTargetNativeGovernancePinningRetirement(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if d != nil {
			stop(t, d)
		}
	}()
	authorize(t, d)
	awaitDrive(t, d, true, 1)
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)
	doc := targetFixture(cfg.Workspace)
	put := func(c TargetConfig) error {
		_, e := call(t, d, "TargetConfigPut", TargetPutParams{ID: "local", Document: c, AuthorizingMessage: "message"}, "")
		return e
	}
	if _, err = call(t, d, "TargetConfigPut", TargetPutParams{ID: "unauthorized", Document: doc}, ""); err == nil {
		t.Fatal("missing authority accepted")
	}
	if err = put(doc); err != nil {
		t.Fatal(err)
	}
	cronSpec := cronSpec("target-cron")
	cronSpec.Template.TargetConfig = "local"
	if _, err = call(t, d, "Schedule", ScheduleParams{Action: "create", Cron: &cronSpec, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if got := cronRead(t, d, cronSpec.ID); got.Spec.Template.TargetRevision != 1 {
		t.Fatal("cron target not pinned", got)
	}
	r, _, err := d.store.GetTarget(ctx, "local", 0)
	if err != nil || r.Revision != 1 {
		t.Fatal(r, err)
	}
	p := AdmitParams{ID: "pinned", Tree: "tree", Content: "work", TargetConfig: "local", AuthorizingMessage: "message"}
	if _, err = call(t, d, "Admit", p, "pinned"); err != nil {
		t.Fatal(err)
	}
	var env envelope
	d.mu.Lock()
	snap := d.jobs["pinned"]
	d.mu.Unlock()
	if err = decodeEnvelope(snap.Payload, &env); err != nil {
		t.Fatal(err)
	}
	if snap.TargetID != "local" || snap.TargetRevision != 1 || env.Admission.TargetHash != r.Hash {
		t.Fatal("not pinned", snap)
	}
	item := BacklogItemParams{AdmitParams: AdmitParams{ID: "target-backlog", Tree: "tree", Content: "backlog", Priority: 10, TargetConfig: "local", TargetRevision: 1}}
	if _, err = call(t, d, "BacklogAdmit", BacklogParams{Items: []BacklogItemParams{item}, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	item.TargetRevision = 2
	if _, err = call(t, d, "BacklogAdmit", BacklogParams{Items: []BacklogItemParams{item}, AuthorizingMessage: "message"}, ""); err == nil {
		t.Fatal("backlog retarget accepted")
	}
	restrictive := doc
	restrictive.AllowedCommands = [][]string{{"go", "version"}}
	if err = put(restrictive); err != nil {
		t.Fatal(err)
	}
	if err = d.checkPinnedTarget(ctx, env.Admission, env.Job.Document); err != nil {
		t.Fatal("pinned revision changed", err)
	}
	p.ID = "latest"
	if _, err = call(t, d, "Admit", p, "latest"); err == nil {
		t.Fatal("new restrictive target ignored")
	}
	if _, err = call(t, d, "TargetConfigGet", map[string]any{"id": "local", "revision": 1}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "TargetConfigList", map[string]any{}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "TargetConfigRetire", map[string]any{"id": "local", "reason": "superseded", "authorizing_message": "message"}, ""); err != nil {
		t.Fatal(err)
	}
	p.ID = "retired"
	p.TargetRevision = 1
	if _, err = call(t, d, "Admit", p, "retired"); err == nil {
		t.Fatal("retired admission accepted")
	}
	before, _, e := d.store.GetTarget(ctx, "local", 1)
	if e != nil || !before.Retired {
		t.Fatal("retired metadata absent", before, e)
	}
	listed, e := d.store.ListTargets(ctx)
	if e != nil || len(listed) != 2 || !listed[0].Retired {
		t.Fatal(listed, e)
	}
	if _, e = call(t, d, "TargetConfigRetire", map[string]any{"id": "local", "reason": "replacement reason", "authorizing_message": "message"}, ""); e != nil {
		t.Fatal(e)
	}
	if err = put(doc); err == nil {
		t.Fatal("retired target revised")
	}
	if err = d.checkPinnedTarget(ctx, env.Admission, env.Job.Document); err != nil {
		t.Fatal("retirement recalled active task", err)
	}
	env.Admission.TargetHash = "tampered"
	if err = d.checkPinnedTarget(ctx, env.Admission, env.Job.Document); err == nil {
		t.Fatal("hash ignored")
	}
	stop(t, d)
	d = nil
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.store.TargetActive(ctx, "local"); err == nil {
		t.Fatal("retirement lost on restart")
	}
	d.mu.Lock()
	snap = d.jobs["pinned"]
	d.mu.Unlock()
	if snap.TargetRevision != 1 {
		t.Fatal("binding lost on restart")
	}
}

func targetV4(workspace string) TargetConfig {
	c := targetFixture(workspace)
	c.SchemaVersion = 4
	build := []string{"go", "build", "./..."}
	c.AllowedCommands = append(c.AllowedCommands, build, []string{"runner", "--case", "{{test-id}}", "--", "{{text}}"})
	c.RequiredTests = []RequiredTest{{Name: "suite", Argv: defaults.TestCommand(), TimeoutSeconds: 900}, {Name: "daemon", Argv: defaults.TestCommand(), Paths: []string{"daemon/"}, TimeoutSeconds: 900}}
	c.CompileCheck = &TargetCommand{Argv: build, TimeoutSeconds: 600}
	c.DiffClassifier = &DiffClassifier{Code: []string{"**/*.go"}, NotCode: []string{"**/*_test.go", "**/testdata/**"}}
	return c
}

// Schema 4 adds typed shapes and QA/commit policy; earlier schemas refuse it,
// so stored v1-v3 revisions keep their exact meaning (FAE-WF-035, -085).
func TestTargetSchemaFourPolicy(t *testing.T) {
	good := targetV4("/repo")
	raw, _ := json.Marshal(good)
	if _, e := parseTarget(raw); e != nil {
		t.Fatal(e)
	}
	for name, mutate := range map[string]func(*TargetConfig){
		"v1 required tests": func(c *TargetConfig) {
			c.SchemaVersion = 1
			c.AllowedCommands = c.AllowedCommands[:len(c.AllowedCommands)-1]
			c.CompileCheck, c.DiffClassifier = nil, nil
		},
		"v1 slot": func(c *TargetConfig) {
			c.SchemaVersion = 1
			c.RequiredTests, c.CompileCheck, c.DiffClassifier = nil, nil, nil
		},
		"v5":                func(c *TargetConfig) { c.SchemaVersion = 5 },
		"program slot":      func(c *TargetConfig) { c.AllowedCommands = append(c.AllowedCommands, []string{"{{text}}"}) },
		"unknown slot type": func(c *TargetConfig) { c.AllowedCommands = append(c.AllowedCommands, []string{"runner", "{{shell}}"}) },
		"partial slot": func(c *TargetConfig) {
			c.AllowedCommands = append(c.AllowedCommands, []string{"runner", "-run={{text}}"})
		},
		"unlisted required test": func(c *TargetConfig) { c.RequiredTests[0].Argv = []string{"go", "test", "./..."} },
		"duplicate test name":    func(c *TargetConfig) { c.RequiredTests[1].Name = "suite" },
		"no timeout":             func(c *TargetConfig) { c.RequiredTests[0].TimeoutSeconds = 0 },
		"escaping scope":         func(c *TargetConfig) { c.RequiredTests[1].Paths = []string{"../x"} },
		"unlisted compile":       func(c *TargetConfig) { c.CompileCheck.Argv = []string{"go", "vet", "./..."} },
		"empty classifier":       func(c *TargetConfig) { c.DiffClassifier.Code = nil },
		"bad glob":               func(c *TargetConfig) { c.DiffClassifier.NotCode = []string{"/abs/*.go"} },
	} {
		c := targetV4("/repo")
		c.RequiredTests = append([]RequiredTest(nil), c.RequiredTests...)
		mutate(&c)
		if name == "v1 slot" {
			c.AllowedCommands = append(c.AllowedCommands, []string{"runner", "{{text}}"})
		}
		raw, _ := json.Marshal(c)
		if _, e := parseTarget(raw); e == nil {
			t.Error(name, "accepted")
		}
	}
	stage := workflow.Stage{Primitive: workflow.HostCommand, Command: []string{"runner", "--case", "{{case}}", "--", "{{why}}"}, Bindings: map[string]workflow.ArgBinding{"case": {Type: "test-id"}, "why": {Type: "text"}}}
	if !targetAllowsStage(good, stage) || targetAllowsStage(targetFixture("/repo"), stage) {
		t.Fatal("template admission")
	}
	stage.Bindings["case"] = workflow.ArgBinding{Type: "text"}
	if targetAllowsStage(good, stage) {
		t.Fatal("slot type ignored")
	}
	if !targetAllowsVector(good, []string{"runner", "--case", "a::b", "--", "x; rm -rf / $(id)"}) || targetAllowsVector(good, []string{"runner", "--case", "a b", "--", "x"}) || targetAllowsVector(good, []string{"sh", "-c", "x"}) || !targetAllowsVector(good, defaults.TestCommand()) {
		t.Fatal("vector validation")
	}
}

func TestRequiredTestScopeAndDiffClassifier(t *testing.T) {
	c := targetV4("/repo")
	names := func(ts []RequiredTest) (out []string) {
		for _, t := range ts {
			out = append(out, t.Name)
		}
		return
	}
	if got := names(requiredFor(c, []string{"README.md"})); len(got) != 1 || got[0] != "suite" {
		t.Fatal(got)
	}
	if got := names(requiredFor(c, []string{"README.md", "daemon/live.go"})); len(got) != 2 {
		t.Fatal(got)
	}
	k := *c.DiffClassifier
	got := classifyDiff(k, []string{"main.go", "pkg/a/b.go", "pkg/a/b_test.go", "pkg/testdata/x.go", "docs/x.md", "go.mod"})
	if len(got) != 2 || got[0] != "main.go" || got[1] != "pkg/a/b.go" {
		t.Fatal(got)
	}
	for _, c := range []struct {
		p, n string
		ok   bool
	}{{"**", "a/b/c", true}, {"docs/**", "docs/a/b.md", true}, {"docs/**", "src/docs/a", false}, {"*.md", "a/b.md", false}, {"**/*.md", "b.md", true}, {"a/?.go", "a/x.go", true}} {
		if matchGlob(c.p, c.n) != c.ok {
			t.Error(c)
		}
	}
}

func TestCommitPolicyGuards(t *testing.T) {
	for _, b := range [][4]bool{{true, true, true, true}, {false, false, false, false}, {false, true, false, false}, {false, true, true, false}, {false, false, true, true}} {
		want := b[0] || b[2] && !b[3] || !b[2] && !b[1]
		if CanCommitWithoutRed(b[0], b[1], b[2], b[3]) != want {
			t.Error(b)
		}
	}
	if CanCommitTree(true, false) || !CanCommitTree(true, true) || !CanCommitTree(false, false) {
		t.Fatal("tree guard")
	}
}
