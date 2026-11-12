package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
)

const bonusTestSource = "package sample\n\nimport \"testing\"\n\nfunc TestBonus(t *testing.T) {\n\tif got := answer() + bonus; got != 43 {\n\t\tt.Fatalf(\"got %d want 43\", got)\n\t}\n}\n"

func liveVariants(t *testing.T, cfg Config, variants map[string]map[string]string) {
	t.Helper()
	raw, err := json.Marshal(variants)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Live.Environment["LIVE_DAEMON_VARIANTS"] = string(raw)
}

func snapshotEnvelope(t *testing.T, s controlplane.JobSnapshot) envelope {
	t.Helper()
	var e envelope
	if err := json.Unmarshal(s.Payload, &e); err != nil {
		t.Fatal(err)
	}
	return e
}

// FAE-VCS-001/-008: a released dependent forks from the integration tip that
// holds its satisfied prerequisite's landed output, and records that exact
// base durably. Its RED fails for the declared reason only because the
// prerequisite's implementation is already in its tree.
func TestDaemonDependentForksFromPrerequisiteTip(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	if err := os.WriteFile(filepath.Join(cfg.Workspace, "bonus.go"), []byte("package sample\n\nvar bonus = 0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	liveGit(t, cfg.Workspace, "add", "bonus.go")
	liveGit(t, cfg.Workspace, "commit", "-m", "bonus seam")
	base := liveGit(t, cfg.Workspace, "rev-parse", "HEAD")
	liveGit(t, cfg.Workspace, "branch", "-f", "fae/integration", base)
	cfg.Live.Base = base
	cfg.Live.Tasks = map[string]LiveTaskConfig{"second": {Files: []string{"bonus.go", "bonus_test.go"}, TestFile: "bonus_test.go", TestID: "TestBonus", ExpectedFailure: "got 42 want 43"}}
	liveVariants(t, cfg, map[string]map[string]string{"second": {"TestFile": "bonus_test.go", "Test": bonusTestSource, "ImplFile": "bonus.go", "Impl": "package sample\n\nvar bonus = 1\n"}})
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	item := func(id string) BacklogItemParams {
		return BacklogItemParams{AdmitParams: AdmitParams{ID: id, Priority: 10, Content: "accept " + id}}
	}
	if _, err = call(t, d, "BacklogAdmit", BacklogParams{Items: []BacklogItemParams{item("first"), item("second")}, Edges: []controlplane.BacklogEdge{{From: "first", To: "second", Kind: "blocking"}}, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	first := snapshotEnvelope(t, awaitThroughDeferred(t, d, "first", "Done", 3*time.Minute))
	landed := liveGit(t, cfg.Workspace, "rev-parse", cfg.Live.IntegrationRef)
	if landed == base || first.Job.Base != base {
		t.Fatal("prerequisite did not land from the admission tip", first.Job.Base, base, landed)
	}
	second := snapshotEnvelope(t, awaitThroughDeferred(t, d, "second", "Done", 3*time.Minute))
	if second.Job.Base != landed {
		t.Fatalf("dependent base %s is not the prerequisite's landed tip %s", second.Job.Base, landed)
	}
	forked := false
	for _, e := range second.Job.History {
		forked = forked || e.Kind == "worktree-forked" && strings.Contains(e.Cause, landed)
		if e.Kind == "tree-reconciled" || e.Kind == "integration-target-moved" {
			t.Fatal("dependent met its prerequisite only at merge", e)
		}
	}
	if !forked {
		t.Fatal("durable fork record absent", second.Job.History)
	}
	tip := liveGit(t, cfg.Workspace, "rev-parse", cfg.Live.IntegrationRef)
	if liveGit(t, cfg.Workspace, "rev-parse", tip+"^") != landed {
		t.Fatal("dependent did not land directly on its prerequisite")
	}
	if !strings.Contains(liveGit(t, cfg.Workspace, "show", tip+":answer.go"), "return 42") || !strings.Contains(liveGit(t, cfg.Workspace, "show", tip+":bonus.go"), "bonus = 1") {
		t.Fatal("integrated tree lacks either task's output")
	}
}

// FAE-VCS-008: two Fae tasks editing the same lines integrate through the real
// path: the first lands, the second's merge conflicts and routes to
// implementation remediation, and it lands only after its gates re-run on
// the integrated result.
func TestDaemonTwoTasksSameLinesIntegrateThenRemediate(t *testing.T) {
	cfg, ctx, base := liveDaemonFixture(t)
	liveVariants(t, cfg, map[string]map[string]string{"right": {"ImplFile": "answer.go", "Impl": "package sample\n\nfunc answer() int { return 6 * 7 }\n"}})
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	for _, id := range []string{"left", "right"} {
		if _, err = call(t, d, "Admit", AdmitParams{ID: id, AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
	}
	jobs := map[string]envelope{}
	for _, id := range []string{"left", "right"} {
		jobs[id] = snapshotEnvelope(t, awaitThroughDeferred(t, d, id, "Done", 4*time.Minute))
	}
	tip := liveGit(t, cfg.Workspace, "rev-parse", cfg.Live.IntegrationRef)
	parent := liveGit(t, cfg.Workspace, "rev-parse", tip+"^")
	if liveGit(t, cfg.Workspace, "rev-parse", parent+"^") != base {
		t.Fatal("integration is not one serial landing per task")
	}
	second := ""
	for id, e := range jobs {
		// The integration queue serialises the window: Fae's own landing
		// never moves the target under the other task (FAE-VCS-008).
		for _, h := range e.Job.History {
			if h.Kind == "integration-target-moved" {
				t.Fatal(id, "integration window not serialised")
			}
		}
		conflict := -1
		for i, a := range e.Job.Evidence {
			if a.Type == "ReconcileReport" && a.Fields["result"] == "conflicted" {
				conflict = i
				if paths, _ := json.Marshal(a.Fields["paths"]); string(paths) != `["answer.go"]` {
					t.Fatal("conflict paths not recorded", string(paths))
				}
			}
		}
		if conflict < 0 {
			continue
		}
		if second != "" {
			t.Fatal("both tasks conflicted")
		}
		second = id
		// Every gate re-ran on the remediated, integrated result.
		after := map[string]bool{}
		for _, a := range e.Job.Evidence[conflict+1:] {
			if a.Tree == e.Job.Tree {
				after[a.Stage] = true
			}
		}
		for _, stage := range []string{"implement", "run-tests", "compare-red", "format", "reviews", "oracle", "qa"} {
			if !after[stage] {
				t.Fatalf("%s did not re-run after the conflict: %v", stage, after)
			}
		}
		if e.Job.Base != parent {
			t.Fatal("second task's base is not the first landing", e.Job.Base, parent)
		}
	}
	if second == "" {
		t.Fatal("same-line edits never conflicted")
	}
	want := map[string]string{"left": "return 42", "right": "return 6 * 7"}[second]
	if got := liveGit(t, cfg.Workspace, "show", tip+":answer.go"); !strings.Contains(got, want) {
		t.Fatal("second landing lacks its remediated edit", got)
	}
	if liveGit(t, cfg.Workspace, "rev-parse", "HEAD") != base || liveGit(t, cfg.Workspace, "status", "--porcelain") != "" {
		t.Fatal("primary checkout changed")
	}
}

// awaitReleased waits until a task has left Pending: its fork point is then
// fixed, so a test may move the integration target under it.
func awaitReleased(t *testing.T, d *Daemon, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if s, err := d.store.GetJob(context.Background(), id); err == nil && s.State != "Pending" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("task never released", id)
}

// FAE-VCS-007: two tasks making the identical change both finish; the second
// is contained by the first's landing, which its Integration record names.
func TestDaemonIdenticalChangeRecordsContainingSHA(t *testing.T) {
	cfg, ctx, base := liveDaemonFixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	for _, id := range []string{"same-a", "same-b"} {
		if _, err = call(t, d, "Admit", AdmitParams{ID: id, AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
	}
	jobs := map[string]envelope{}
	for _, id := range []string{"same-a", "same-b"} {
		jobs[id] = snapshotEnvelope(t, awaitThroughDeferred(t, d, id, "Done", 4*time.Minute))
	}
	tip := liveGit(t, cfg.Workspace, "rev-parse", cfg.Live.IntegrationRef)
	if liveGit(t, cfg.Workspace, "rev-parse", tip+"^") != base {
		t.Fatal("identical change landed twice")
	}
	contained := 0
	for id, e := range jobs {
		for _, a := range e.Job.Evidence {
			if a.Type != "Integration" {
				continue
			}
			if by, ok := a.Fields["contained_by"]; ok {
				contained++
				if by != tip || a.Fields["landed"] != true {
					t.Fatal(id, "containment does not name the containing tip", a.Fields)
				}
			}
		}
	}
	if contained != 1 {
		t.Fatal("exactly one task must be recorded as contained", contained)
	}
}

// FAE-VCS-003: a prior implementation commit planted in the task worktree
// after admission never reaches a RED run: the RED stage refuses it with
// StaleBranchResidue and nothing lands.
func TestDaemonPlantedCommitRefusedAtRED(t *testing.T) {
	cfg, ctx, base := liveDaemonFixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	var planted atomic.Bool
	watch, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		for watch.Err() == nil {
			if s, e := d.store.GetJob(watch, "residue"); e == nil && s.State == "test-first" {
				w := d.live.manager.Binding(d.config.RunID, "residue")
				if os.WriteFile(filepath.Join(w.Path, "answer.go"), []byte("package sample\n\nfunc answer() int { return 42 }\n"), 0600) == nil {
					cmd := exec.Command("git", "-C", w.Path, "-c", "user.name=Prior", "-c", "user.email=prior@localhost", "commit", "-qam", "prior attempt's implementation")
					cmd.Env = cleanGitEnv()
					planted.Store(cmd.Run() == nil)
				}
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	if _, err = call(t, d, "Admit", AdmitParams{ID: "residue", AuthorizingMessage: "message"}, "residue"); err != nil {
		t.Fatal(err)
	}
	e := snapshotEnvelope(t, awaitState(t, d, "residue", "Escalated"))
	if !planted.Load() || e.Job.EscalationKind != domain.StaleBranchResidue {
		t.Fatalf("planted commit not refused as residue: planted=%v kind=%s %s", planted.Load(), e.Job.EscalationKind, e.Job.EscalationDetail)
	}
	from := ""
	for _, h := range e.Job.History {
		if h.Kind == "transition" && h.To == domain.Escalated {
			from = string(h.From)
		}
		if h.Kind == "dispatch" && (h.Stage == "freeze-red" || h.Stage == "implement") {
			t.Fatal("RED accepted a tree holding the feature", h.Stage)
		}
	}
	if from != "test-first" && from != "run-red" {
		t.Fatal("residue refused outside the RED stage", from)
	}
	if liveGit(t, cfg.Workspace, "rev-parse", cfg.Live.IntegrationRef) != base {
		t.Fatal("residue landed")
	}
}

// independentTask gives task id its own declared files (bonus.go and its
// test) on a new fixture base, so its change never overlaps the default
// tasks' answer.go edits. It returns the new base.
func independentTask(t *testing.T, cfg Config, id string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cfg.Workspace, "bonus.go"), []byte("package sample\n\nvar bonus = 0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	liveGit(t, cfg.Workspace, "add", "bonus.go")
	liveGit(t, cfg.Workspace, "commit", "-m", "bonus seam")
	base := liveGit(t, cfg.Workspace, "rev-parse", "HEAD")
	liveGit(t, cfg.Workspace, "branch", "-f", "fae/integration", base)
	cfg.Live.Base = base
	cfg.Live.Tasks = map[string]LiveTaskConfig{id: {Files: []string{"bonus.go", "bonus_test.go"}, TestFile: "bonus_test.go", TestID: "TestBonus", ExpectedFailure: "got 0 want 1"}}
	test := "package sample\n\nimport \"testing\"\n\nfunc TestBonus(t *testing.T) {\n\tif bonus != 1 {\n\t\tt.Fatalf(\"got %d want 1\", bonus)\n\t}\n}\n"
	liveVariants(t, cfg, map[string]map[string]string{id: {"TestFile": "bonus_test.go", "Test": test, "ImplFile": "bonus.go", "Impl": "package sample\n\nvar bonus = 1\n"}})
	return base
}

// advanceIntegration commits name onto the integration target through a
// scratch worktree in the fixture root, leaving the primary checkout as it
// is, and returns the new tip.
func advanceIntegration(t *testing.T, cfg Config, name string) string {
	t.Helper()
	dir := filepath.Join(filepath.Dir(cfg.Workspace), "advance")
	old := liveGit(t, cfg.Workspace, "rev-parse", cfg.Live.IntegrationRef)
	liveGit(t, cfg.Workspace, "worktree", "add", "--detach", dir, old)
	defer liveGit(t, cfg.Workspace, "worktree", "remove", "--force", dir)
	if err := os.WriteFile(filepath.Join(dir, name), []byte("moved "+name+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	liveGit(t, dir, "add", name)
	liveGit(t, dir, "commit", "-qm", "integration moved: "+name)
	tip := liveGit(t, dir, "rev-parse", "HEAD")
	liveGit(t, cfg.Workspace, "update-ref", cfg.Live.IntegrationRef, tip, old)
	return tip
}

// slowRefork puts a git wrapper first on PATH that holds every refork tree
// move (read-tree -m -u) for delay. The wrapper creates started when a move
// begins and finished once it is done.
func slowRefork(t *testing.T, delay time.Duration) (started, finished string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	started, finished = filepath.Join(dir, "started"), filepath.Join(dir, "finished")
	script := fmt.Sprintf("#!/bin/sh\ncase \" $* \" in\n*' read-tree -m -u '*) : > '%s'; sleep %d; '%s' \"$@\"; status=$?; : > '%s'; exit $status;;\nesac\nexec '%s' \"$@\"\n", started, int(delay/time.Second), real, finished, real)
	if err = os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return started, finished
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// FAE-ROB-021 with FAE-VCS-008: the release-time refork runs its git commands
// and its checkpoint without the writer lock. An admission issued while a
// slowed refork moves a released task's tree returns in under 1 s, before the
// refork ends, and the refork still records the new tip as the task's base.
func TestAdmitDoesNotWaitForTheReleaseRefork(t *testing.T) {
	cfg, ctx, base := liveDaemonFixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	if _, err = controllerCall(t, d, controller.Params{Action: "stop-drive", AuthorizingMessage: "message"}); err != nil {
		t.Fatal(err)
	}
	awaitDrive(t, d, false, 1)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "released", AuthorizingMessage: "message"}, "released"); err != nil {
		t.Fatal(err)
	}
	if e := liveEnvelope(t, d, "released"); e.Job.Base != base || e.Job.State != domain.Pending {
		t.Fatal("task did not wait for release at its admission base", e.Job.Base, e.Job.State)
	}
	tip := advanceIntegration(t, cfg, "MOVED.md")
	started, finished := slowRefork(t, 6*time.Second)
	if _, err = controllerCall(t, d, controller.Params{Action: "rearm", AuthorizingMessage: "message"}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Minute); !exists(started); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("release refork never started")
		}
	}
	begun := time.Now()
	_, err = callOnce(t, d, "Admit", AdmitParams{ID: "admitted", AuthorizingMessage: "message"}, "admitted")
	elapsed, during := time.Since(begun), !exists(finished)
	if err != nil || elapsed >= time.Second || !during {
		t.Fatalf("admission waited for the release refork: %s (refork still running: %v): %v", elapsed, during, err)
	}
	t.Logf("admission returned in %s while the release refork ran", elapsed)
	_, e := awaitJob(t, d, "released", 3*time.Minute, func(_ controlplane.JobSnapshot, e envelope) bool { return e.Job.Base != base })
	if e.Job.Base != tip {
		t.Fatal("released task forked from", e.Job.Base, "not the tip", tip)
	}
	forked := false
	for _, h := range e.Job.History {
		forked = forked || h.Kind == "worktree-forked" && strings.Contains(h.Cause, tip)
	}
	w := d.live.manager.Binding(d.config.RunID, "released")
	if !forked || liveGit(t, w.Path, "rev-parse", "HEAD^{tree}") != liveGit(t, cfg.Workspace, "rev-parse", tip+"^{tree}") {
		t.Fatal("refork not recorded or worktree not at the tip", forked)
	}
	if a := liveEnvelope(t, d, "admitted"); a.Job.Base != tip {
		t.Fatal("admission did not fork from the current tip", a.Job.Base, tip)
	}
}

// FAE-WF-005: an Admit naming an existing task with another idempotency key
// is refused before any fork. The existing task's worktree, branch and
// recorded base stay as they were, though the integration tip has moved.
func TestAdmitExistingTaskRefusedBeforeFork(t *testing.T) {
	cfg, ctx, base := liveDaemonFixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	if _, err = controllerCall(t, d, controller.Params{Action: "stop-drive", AuthorizingMessage: "message"}); err != nil {
		t.Fatal(err)
	}
	awaitDrive(t, d, false, 1)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "bound", AuthorizingMessage: "message"}, "bound"); err != nil {
		t.Fatal(err)
	}
	advanceIntegration(t, cfg, "MOVED.md")
	w := d.live.manager.Binding(d.config.RunID, "bound")
	_, refused := call(t, d, "Admit", AdmitParams{ID: "bound", Content: "a second binding", AuthorizingMessage: "message"}, "bound-again")
	stored, err := d.store.GetJob(ctx, "bound")
	if err != nil {
		t.Fatal(err)
	}
	if e := snapshotEnvelope(t, stored); e.Job.Base != base || stored.Version != 1 {
		t.Fatal("existing binding changed", e.Job.Base, stored.Version)
	}
	if head := liveGit(t, w.Path, "rev-parse", "HEAD"); head != base || liveGit(t, cfg.Workspace, "rev-parse", "refs/heads/"+w.Branch) != base {
		t.Fatal("existing task's worktree re-forked to", head)
	}
	if status := liveGit(t, w.Path, "status", "--porcelain"); status != "" {
		t.Fatal("existing task's worktree changed", status)
	}
	if refused == nil || !strings.Contains(refused.Error(), "already exists") {
		t.Fatal("existing task not refused by identity", refused)
	}
}

// FAE-WF-086 with FAE-VCS-001/-008: a carry-forward re-admission forks from
// the current integration tip, not from its source's base, then waits for
// release like any other task and re-forks onto the tip of that moment. Its
// carried pre-code evidence is informational on the new base, where every
// late stage runs before it lands.
func TestDaemonCarryForwardForksFromTheMovedTip(t *testing.T) {
	cfg, ctx, base := liveDaemonFixture(t)
	// The source's RED turn writes an undeclared file: it escalates at
	// test-first with its pre-code evidence accepted.
	liveVariants(t, cfg, map[string]map[string]string{"first": {"TestFile": "notes.txt", "Test": "prior notes\n"}})
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	const content = "make answer 42"
	if _, err = call(t, d, "Admit", AdmitParams{ID: "first", Content: content, AuthorizingMessage: "message"}, "first"); err != nil {
		t.Fatal(err)
	}
	first := snapshotEnvelope(t, awaitState(t, d, "first", "Escalated"))
	if first.Job.Base != base || first.Job.EscalationKind != domain.StaleBranchResidue || first.Job.Counts["plan"] != 1 {
		t.Fatal("source did not escalate after its pre-code stages", first.Job.Base, first.Job.EscalationKind, first.Job.Counts)
	}
	moved := advanceIntegration(t, cfg, "MOVED.md")
	if _, err = controllerCall(t, d, controller.Params{Action: "stop-drive", AuthorizingMessage: "message"}); err != nil {
		t.Fatal(err)
	}
	awaitDrive(t, d, false, 1)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "second", Content: content, CarryFrom: "first", AuthorizingMessage: "message"}, "second"); err != nil {
		t.Fatal(err)
	}
	if e := liveEnvelope(t, d, "second"); e.Job.Base != moved || e.Job.State != domain.Pending {
		t.Fatal("carry-forward did not fork from the current tip and wait for release", e.Job.Base, e.Job.State)
	}
	later := advanceIntegration(t, cfg, "LATER.md")
	if _, err = controllerCall(t, d, controller.Params{Action: "rearm", AuthorizingMessage: "message"}); err != nil {
		t.Fatal(err)
	}
	second := snapshotEnvelope(t, awaitThroughDeferred(t, d, "second", "Done", 4*time.Minute))
	if second.Job.Base != later {
		t.Fatal("carried task not re-forked at release onto", later, "but", second.Job.Base)
	}
	carried, forked, entered := 0, false, false
	for _, p := range second.Job.Provenance {
		if p.CarriedFrom == "first" {
			carried++
		}
	}
	for _, h := range second.Job.History {
		forked = forked || h.Kind == "worktree-forked" && strings.Contains(h.Cause, later)
		entered = entered || h.Kind == "transition" && h.From == domain.Pending && strings.HasPrefix(h.Cause, "carry-forward")
	}
	if carried < 4 || !forked || !entered || second.Job.Counts["spec"] != 0 || second.Job.Counts["plan"] != 0 || second.Job.Counts["test-first"] != 1 {
		t.Fatal("carry-forward", carried, forked, entered, second.Job.Counts)
	}
	tip := liveGit(t, cfg.Workspace, "rev-parse", cfg.Live.IntegrationRef)
	if liveGit(t, cfg.Workspace, "rev-parse", tip+"^") != later || !strings.Contains(liveGit(t, cfg.Workspace, "show", tip+":answer.go"), "return 42") {
		t.Fatal("carried task did not land on the moved tip")
	}
}

// FAE-ROB-021: the release refork re-reads the task under the writer lock
// before recording the fork. A task that changed while its refork ran
// unlocked is not checkpointed over; the next release records the fork the
// worktree already holds, without a second tree move.
func TestReleaseReforkRechecksTheTask(t *testing.T) {
	cfg, ctx, base := liveDaemonFixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	if _, err = controllerCall(t, d, controller.Params{Action: "stop-drive", AuthorizingMessage: "message"}); err != nil {
		t.Fatal(err)
	}
	awaitDrive(t, d, false, 1)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "raced", AuthorizingMessage: "message"}, "raced"); err != nil {
		t.Fatal(err)
	}
	tip := advanceIntegration(t, cfg, "MOVED.md")
	started, finished := slowRefork(t, 2*time.Second)
	release := func() (bool, error) {
		d.mu.Lock()
		defer d.mu.Unlock()
		s := d.jobs["raced"]
		_, _, changed, err := d.forkAtRelease(s, snapshotEnvelope(t, s))
		return changed, err
	}
	d.mu.Lock()
	original := d.jobs["raced"]
	d.mu.Unlock()
	go func() {
		for !exists(started) {
			time.Sleep(5 * time.Millisecond)
		}
		// Another writer's checkpoint lands while the refork runs unlocked.
		d.mu.Lock()
		changed := d.jobs["raced"]
		changed.Version++
		d.jobs["raced"] = changed
		d.mu.Unlock()
	}()
	if changed, err := release(); err != nil || !changed {
		t.Fatal("a task changed during its refork was not reported", changed, err)
	}
	if e := liveEnvelope(t, d, "raced"); e.Job.Base != base {
		t.Fatal("fork checkpointed over a changed task", e.Job.Base)
	}
	if err = os.Remove(finished); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.jobs["raced"] = original
	d.mu.Unlock()
	if changed, err := release(); err != nil || changed {
		t.Fatal("unchanged task's refork not applied", changed, err)
	}
	e := liveEnvelope(t, d, "raced")
	w := d.live.manager.Binding(d.config.RunID, "raced")
	if e.Job.Base != tip || liveGit(t, w.Path, "rev-parse", "HEAD") != tip || exists(finished) {
		t.Fatal("refork not recorded once at the tip", e.Job.Base, exists(finished))
	}
}
