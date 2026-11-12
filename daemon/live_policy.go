package daemon

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/effects"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/execution"
	"github.com/xleelz/fae/workflow"
	"github.com/xleelz/fae/worktree"
)

// taskEnvelope reads the durable job and admission behind a dispatch.
func (x *liveExecutor) taskEnvelope(id string) (envelope, error) {
	x.daemon.mu.Lock()
	snapshot, ok := x.daemon.jobs[id]
	x.daemon.mu.Unlock()
	var e envelope
	if !ok {
		return e, errors.New("unknown live task")
	}
	return e, decodeEnvelope(snapshot.Payload, &e)
}

// pinnedTarget returns the task's immutable target revision, or nil when the
// task was admitted without one.
func (d *Daemon) pinnedTarget(ctx context.Context, p AdmitParams) (*TargetConfig, error) {
	if p.TargetConfig == "" {
		return nil, nil
	}
	r, raw, err := d.store.GetTarget(ctx, p.TargetConfig, p.TargetRevision)
	if err != nil {
		return nil, err
	}
	if r.Hash != p.TargetHash {
		return nil, errors.New("target content identity mismatch")
	}
	c, err := parseTarget(raw)
	return &c, err
}

// authorizeCommand returns the exact vector a HostCommand dispatch may run.
// The instance allowlist authorizes the template; a pinned target authorizes
// the bound vector (FAE-WF-035). Nothing has started when this refuses.
func (x *liveExecutor) authorizeCommand(ctx context.Context, d engine.Dispatch, target *TargetConfig) ([]string, *effects.Broker, error) {
	argv, broker, _, _, err := x.authorizeCommandWithTask(ctx, d, envelope{}, target)
	return argv, broker, err
}

func (x *liveExecutor) authorizeCommandWithTask(ctx context.Context, d engine.Dispatch, task envelope, target *TargetConfig) ([]string, *effects.Broker, map[string]string, []string, error) {
	if !slices.ContainsFunc(x.config.AllowedCommands, func(v []string) bool {
		return slices.Equal(v, d.Config.Command) || workflow.MatchTemplate(v, d.Config)
	}) {
		return nil, nil, nil, nil, errors.New("command template is not in the instance allowlist")
	}
	stageRoots := workflow.StageReadRoots(task.Job.Document, task.Job.TaskClass, d.Config)
	var cmdOnlyRoots []string
	for k, b := range d.Config.Bindings {
		_ = k
		if b.IsReadRoot() {
			cmdOnlyRoots = append(cmdOnlyRoots, b.ReadRootKey())
		}
	}
	for _, t := range d.Config.ArgTemplates {
		if t.Kind == "read-root" {
			cmdOnlyRoots = append(cmdOnlyRoots, t.Key)
		}
	}
	var allRoots []string
	for _, r := range append(stageRoots, cmdOnlyRoots...) {
		if r != "" && !slices.Contains(allRoots, r) {
			allRoots = append(allRoots, r)
		}
	}
	slices.Sort(allRoots)
	var verifiedRoots map[string]string
	var rootPathsMap map[string]string
	var rootPathsSlice []string
	if len(allRoots) > 0 {
		if target == nil {
			return nil, nil, nil, nil, fmt.Errorf("read-root-changed: %s", allRoots[0])
		}
		verifiedRoots = make(map[string]string, len(allRoots))
		rootPathsMap = make(map[string]string, len(allRoots))
		for _, rName := range allRoots {
			rc, ok := target.ReadRoots[rName]
			if !ok {
				return nil, nil, nil, nil, fmt.Errorf("read-root-changed: %s", rName)
			}
			actual, err := VerifyReadRoot(ctx, rName, rc, target.DenyPaths)
			if err != nil {
				return nil, nil, nil, nil, fmt.Errorf("read-root-changed: %s", rName)
			}
			if pinned := task.Job.ReadRoots[rName]; pinned != "" && actual != pinned {
				return nil, nil, nil, nil, fmt.Errorf("read-root-changed: %s", rName)
			}
			verifiedRoots[rName] = actual
			rootPathsMap[rName] = rc.Path
			rootPathsSlice = append(rootPathsSlice, rc.Path)
		}
	}
	argv := d.Vector()
	if workflow.HasRuntimeReferences(d.Config.Command) {
		bound, err := engine.BindStageCommand(task.Job, d.Config)
		if err != nil || !slices.Equal(bound, argv) {
			return nil, nil, nil, nil, errors.New("dispatch runtime references differ from task bindings")
		}
	}
	if len(d.Config.Bindings) > 0 || len(d.Config.ArgTemplates) > 0 {
		if len(cmdOnlyRoots) == 0 {
			bound, err := workflow.BindCommand(d.Config, d.Inputs)
			if err != nil || !slices.Equal(bound, argv) {
				return nil, nil, nil, nil, errors.New("dispatch vector differs from its bound inputs")
			}
		} else {
			partial, err := workflow.BindCommand(d.Config, d.Inputs)
			if err != nil || !slices.Equal(partial, argv) {
				return nil, nil, nil, nil, errors.New("dispatch vector differs from its bound inputs")
			}
			bound, err := workflow.BindCommandWithReadRoots(d.Config, d.Inputs, rootPathsMap)
			if err != nil {
				return nil, nil, nil, nil, err
			}
			argv = bound
		}
	}
	if target != nil && !targetAllowsVector(*target, argv) {
		return nil, nil, nil, nil, errors.New("bound command vector is outside the target allowlist")
	}
	broker := x.broker
	if len(d.Argv) > 0 || len(cmdOnlyRoots) > 0 {
		copied := *x.broker
		copied.Allowed = [][]string{argv}
		broker = &copied
	}
	return argv, broker, verifiedRoots, rootPathsSlice, nil
}

// changedPaths lists the task's own changes between its base and tree.
func changedPaths(ctx context.Context, worktree, base, tree string) ([]string, error) {
	if base == "" || tree == "" {
		return nil, errors.New("diff requires the task base and tree")
	}
	cmd := exec.CommandContext(ctx, "git", "diff", "--name-only", "--no-renames", base, tree)
	cmd.Dir = worktree
	cmd.Env = cleanGitEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

func requiredProofKey(task, tree string, target AdmitParams) string {
	sum := sha256.Sum256([]byte(task + "\x00" + tree + "\x00" + target.TargetConfig + "\x00" + target.TargetHash))
	return "required-tests/" + hex.EncodeToString(sum[:])
}

// targetChecks lists the target's checks a QA run must execute for this diff:
// the selected always-required tests, then the compile check (FAE-WF-085).
func targetChecks(target TargetConfig, changed []string) []TargetCommand {
	var out []TargetCommand
	for _, t := range requiredFor(target, changed) {
		out = append(out, TargetCommand{Argv: t.Argv, TimeoutSeconds: t.TimeoutSeconds})
	}
	docOnly := len(changed) > 0 && len(target.DocumentGlobs) > 0 && slices.IndexFunc(changed, func(f string) bool { return !workflow.MatchesAnyGlob(f, target.DocumentGlobs) }) < 0
	if target.CompileCheck != nil && !docOnly {
		out = append(out, *target.CompileCheck)
	}
	return out
}

// runRequired runs the QA checks under the QA stage's heavy lease and records
// a durable proof for the exact tree when every one passed.
func (x *liveExecutor) runRequired(ctx context.Context, d engine.Dispatch, w worktree.Binding, target TargetConfig, admission AdmitParams) (runs []effects.RunEvidence, err error) {
	changed, err := changedPaths(ctx, w.Path, d.Base, d.Tree)
	if err != nil {
		return nil, err
	}
	passed := true
	for _, c := range targetChecks(target, changed) {
		started := time.Now().UTC()
		run, e := x.broker.Run(ctx, w, effects.Command{Argv: c.Argv, Timeout: time.Duration(c.TimeoutSeconds) * time.Second})
		if e != nil {
			return nil, fmt.Errorf("required check %v: %w", c.Argv, e)
		}
		if e = x.recordEngineRun(ctx, d, c.Argv, nil, run, started); e != nil {
			return nil, e
		}
		passed = passed && run.ExitCode == 0
		runs = append(runs, run)
	}
	if passed && len(runs) > 0 {
		raw, _ := json.Marshal(map[string]any{"task": d.TaskID, "tree": d.Tree, "target": admission.TargetConfig, "target_hash": admission.TargetHash, "runs": len(runs)})
		err = x.daemon.store.PutMarker(ctx, controlplane.Audit{Actor: "daemon", Action: "required tests passed", Outcome: "ok", Inputs: raw}, requiredProofKey(d.TaskID, d.Tree, admission), raw)
	}
	return runs, err
}

// foldRequired joins the required runs into the QA stage's engine run: any
// failure fails it, and executed counts and digests accumulate.
func foldRequired(run effects.RunEvidence, extra []effects.RunEvidence) effects.RunEvidence {
	for _, e := range extra {
		if run.ExitCode == 0 && e.ExitCode != 0 {
			run.ExitCode = e.ExitCode
		}
		run.ExecutedCount += e.ExecutedCount
		sum := sha256.Sum256([]byte(run.OutputDigest + "\x00" + e.OutputDigest))
		run.OutputDigest = hex.EncodeToString(sum[:])
	}
	return run
}

// runDocumentChecks executes a target's configured DocumentChecks against w
// and returns their structured receipts (FAE-CTL-052).
func runDocumentChecks(ctx context.Context, broker *effects.Broker, w worktree.Binding, target TargetConfig, declaredFiles []string) ([]engine.DocumentCheckReceipt, error) {
	_, docChecks := effectiveTargetDocumentPolicy(target)
	if len(docChecks) == 0 {
		return nil, nil
	}
	receipts := make([]engine.DocumentCheckReceipt, 0, len(docChecks))
	for _, dc := range docChecks {
		spec := workflow.CheckSpec{
			Format:     dc.Format,
			Output:     dc.Output,
			CountField: dc.CountField,
			Writes:     slices.Clone(dc.Writes),
		}
		rec := engine.DocumentCheckReceipt{
			ID:         dc.ID,
			CountField: dc.CountField,
		}
		if err := workflow.CheckWritesDisjoint(spec.Writes, declaredFiles); err != nil {
			rec.ParseError = err.Error()
			receipts = append(receipts, rec)
			return receipts, fmt.Errorf("document_check %s: %w", dc.ID, err)
		}
		ev, err := broker.Run(ctx, w, effects.Command{
			Argv:          slices.Clone(dc.Command),
			Check:         &spec,
			DeclaredFiles: slices.Clone(declaredFiles),
		})
		rec.ExitCode = ev.ExitCode
		if err != nil {
			rec.ParseError = err.Error()
			receipts = append(receipts, rec)
			return receipts, fmt.Errorf("document_check %s: %w", dc.ID, err)
		}
		rec.Count = int(ev.CheckCount)
		if ev.ExitCode != 0 || rec.Count < 1 {
			receipts = append(receipts, rec)
			return receipts, fmt.Errorf("document_check %s exited %d with %s=%d", dc.ID, ev.ExitCode, dc.CountField, rec.Count)
		}
		receipts = append(receipts, rec)
	}
	return receipts, nil
}

// prepareDocumentChecksForJob ensures e.Job.DeclaredFiles and e.Job.DocumentGlobs
// are populated from e.Admission and the pinned target, and runs DocumentChecks
// when a document-only task reaches a protected stage (FAE-CTL-052).
func (d *Daemon) prepareDocumentChecksForJob(ctx context.Context, e *envelope) {
	if d == nil || e == nil {
		return
	}
	if len(e.Job.DeclaredFiles) == 0 && len(e.Admission.DeclaredFiles) > 0 {
		e.Job.DeclaredFiles = slices.Clone(e.Admission.DeclaredFiles)
	}
	if e.Admission.TargetConfig == "" {
		return
	}
	pinnedTgt, err := d.pinnedTarget(ctx, e.Admission)
	if err != nil || pinnedTgt == nil {
		return
	}
	docGlobs, docChecks := effectiveTargetDocumentPolicy(*pinnedTgt)
	if len(docGlobs) > 0 && len(e.Job.DocumentGlobs) == 0 {
		e.Job.DocumentGlobs = slices.Clone(docGlobs)
	}
	if len(e.Job.DeclaredFiles) == 0 || len(e.Job.DocumentGlobs) == 0 || len(docChecks) == 0 {
		return
	}
	for _, f := range e.Job.DeclaredFiles {
		if !workflow.MatchesAnyGlob(f, e.Job.DocumentGlobs) {
			return
		}
	}
	stage, ok := e.Job.Document.Stages[string(e.Job.State)]
	if !ok || (stage.Primitive != workflow.VcsCommit && stage.Primitive != workflow.Integrate) {
		return
	}
	if d.live != nil && d.live.broker != nil && d.live.manager != nil {
		runID := e.Admission.RunID
		if runID == "" {
			runID = d.config.RunID
		}
		if w, wErr := d.live.manager.Ensure(ctx, runID, e.Job.TaskID, e.Job.Base); wErr == nil {
			receipts, _ := runDocumentChecks(ctx, d.live.broker, w, *pinnedTgt, e.Job.DeclaredFiles)
			if len(receipts) > 0 {
				e.Job.DocumentChecks = receipts
			}
		}
	}
}

// commitRefusal is a typed no-write refusal of a VcsCommit.
type commitRefusal struct{ class, detail string }

// commitPolicy is the target's commit floor, checked on the exact reviewed
// tree before any commit object exists: the no-red classifier (FAE-WF-073),
// the compile check and the always-required tests (FAE-WF-085).
func (x *liveExecutor) commitPolicy(ctx context.Context, d engine.Dispatch, w worktree.Binding, e envelope, target *TargetConfig) (refusal *commitRefusal, err error) {
	var classifier *DiffClassifier
	var changed []string
	var docGlobs []string
	var docChecks []DocumentCheck
	if target != nil {
		classifier = target.DiffClassifier
		docGlobs, docChecks = effectiveTargetDocumentPolicy(*target)
		if classifier != nil || target.CompileCheck != nil || len(target.RequiredTests) > 0 || len(docGlobs) > 0 {
			if changed, err = changedPaths(ctx, w.Path, d.Base, d.Tree); err != nil {
				return nil, err
			}
		}
	}
	docOnly := target != nil && len(changed) > 0 && len(docGlobs) > 0 && slices.IndexFunc(changed, func(f string) bool { return !workflow.MatchesAnyGlob(f, docGlobs) }) < 0
	var code []string
	if classifier != nil {
		code = classifyDiff(*classifier, changed)
	}
	hasClassifier := classifier != nil || docOnly
	if docOnly {
		code = nil
	}
	if !CanCommitWithoutRed(engine.HasFrozenRed(e.Job), engine.NoRedDeclared(e.Job), hasClassifier, len(code) > 0) {
		detail := "no-red task without a target diff classifier"
		if classifier != nil {
			detail = "production code without RED: " + strings.Join(code, ", ")
		}
		return &commitRefusal{engine.ClassNoRedViolation, detail}, nil
	}
	if target == nil {
		return nil, nil
	}
	if docOnly && len(docChecks) > 0 {
		declared := e.Admission.DeclaredFiles
		if len(declared) == 0 {
			declared = changed
		}
		if _, dcErr := runDocumentChecks(ctx, x.broker, w, *target, declared); dcErr != nil {
			return &commitRefusal{engine.ClassRequiredCheckFailed, dcErr.Error()}, nil
		}
	}
	// The commit runs nothing heavy itself: a QA run must have proved this
	// exact tree under the pinned target, or the commit is refused.
	declared := len(targetChecks(*target, changed)) > 0
	if declared {
		_, err = x.daemon.store.GetMarker(ctx, requiredProofKey(d.TaskID, d.Tree, e.Admission))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	if !CanCommitTree(declared, declared && err == nil) {
		return &commitRefusal{engine.ClassRequiredCheckFailed, "always-required tests and compile check not proven on the committed tree"}, nil
	}
	return nil, nil
}

// dispatchPolicy is the pinned policy a host or VCS effect executes under.
type dispatchPolicy struct {
	task      envelope
	target    *TargetConfig
	argv      []string
	broker    *effects.Broker
	readRoots map[string]string
	rootPaths []string
}

// policyFor resolves the task's pinned target and, for a HostCommand, the
// authorized vector and broker. It refuses before any effect starts.
func (x *liveExecutor) policyFor(ctx context.Context, d engine.Dispatch) (p dispatchPolicy, err error) {
	p.broker = x.broker
	if d.Config.Primitive != workflow.HostCommand && d.Config.Primitive != workflow.VcsCommit && d.Config.Primitive != workflow.Integrate && !d.Config.IsGitPush() {
		return p, nil
	}
	if p.task, err = x.taskEnvelope(d.TaskID); err != nil {
		return p, err
	}
	if p.target, err = x.daemon.pinnedTarget(ctx, p.task.Admission); err != nil {
		return p, err
	}
	if p.target != nil {
		copied := *x.broker
		copied.CommitIdentity = p.target.EffectsCommitIdentity()
		copied.ContentPolicy = p.target.EffectsContentPolicy()
		p.broker = &copied
		docGlobs, _ := effectiveTargetDocumentPolicy(*p.target)
		if len(docGlobs) > 0 && len(p.task.Job.DocumentGlobs) == 0 {
			p.task.Job.DocumentGlobs = slices.Clone(docGlobs)
		}
		if len(p.task.Job.DeclaredFiles) == 0 && len(p.task.Admission.DeclaredFiles) > 0 {
			p.task.Job.DeclaredFiles = slices.Clone(p.task.Admission.DeclaredFiles)
		}
	}
	if d.Config.Primitive == workflow.HostCommand {
		p.argv, p.broker, p.readRoots, p.rootPaths, err = x.authorizeCommandWithTask(ctx, d, p.task, p.target)
		if err == nil && p.target != nil {
			p.broker.CommitIdentity = p.target.EffectsCommitIdentity()
			p.broker.ContentPolicy = p.target.EffectsContentPolicy()
		}
	}
	return p, err
}

// redIdentity is the frozen RED identity (FAE-WF-030, FAE-WF-076): the
// normalised declared Go test function, or for other stacks the token stream
// of the declaration naming the test identifier.
func redIdentity(testFile string, source []byte, id string) (string, error) {
	if strings.HasSuffix(testFile, ".go") {
		return workflow.GoTestIdentity(source, []string{id})
	}
	return workflow.TokenTestIdentity(source, workflow.TestDeclarationName(testFile, id))
}

// checkFrozenRed refuses an implementation change to the frozen RED: Go files
// keep everything the declared test relies on, other files its declaration.
func checkFrozenRed(testFile string, frozen, current []byte, id string) error {
	if strings.HasSuffix(testFile, ".go") {
		return workflow.CheckFrozenFile(frozen, current, []string{id})
	}
	return workflow.CheckFrozenTokens(frozen, current, workflow.TestDeclarationName(testFile, id))
}

// noRedExempt holds for an implementation turn of a task its intake or spec
// declared no-red that has no RED evidence: the commit-time classifier, not
// a frozen RED, governs it (FAE-WF-073). Valid RED evidence always wins.
func (x *liveExecutor) noRedExempt(d engine.Dispatch) bool {
	for _, a := range d.Inputs {
		if a.Type == "FrozenIdentity" {
			return false
		}
	}
	e, err := x.taskEnvelope(d.TaskID)
	return err == nil && engine.NoRedDeclared(e.Job) && !engine.HasFrozenRed(e.Job)
}

// redRunWorktree selects where a RED run executes. A replacement RED,
// authored after the task already changed production files, must fail
// against the task's base revision (FAE-WF-077): it runs in a scratch
// worktree at the base with only the declared test file overlaid. A first
// RED, whose tree differs from the base only in that file, runs in place.
func (x *liveExecutor) redRunWorktree(ctx context.Context, d engine.Dispatch, b execution.Binding, w worktree.Binding, task envelope, testFile string) (worktree.Binding, func(), error) {
	none := func() {}
	if !d.Config.TestRun || d.Config.TestShape == "" || !engine.RedPhase(task.Job) || d.Base == "" {
		return w, none, nil
	}
	changed, err := changedPaths(ctx, w.Path, d.Base, d.Tree)
	if err != nil {
		return w, none, err
	}
	if !slices.ContainsFunc(changed, func(name string) bool { return name != testFile }) {
		return w, none, nil
	}
	source, err := x.testSource(b, testFile)
	if err != nil {
		return w, none, err
	}
	run, scratchTask := task.Admission.RunID, d.TaskID+".red-base"
	scratch, err := x.manager.Ensure(ctx, run, scratchTask, d.Base)
	if err != nil {
		return w, none, err
	}
	scrub := func() error {
		// Adopted scratch state from an interrupted run is discarded first.
		if _, err := effectGit(ctx, scratch, "reset", "--hard", "--quiet", d.Base); err != nil {
			return err
		}
		_, err := effectGit(ctx, scratch, "clean", "-fdxq")
		return err
	}
	release := func() {
		if scrub() == nil && x.manager.Remove(context.WithoutCancel(ctx), run, scratchTask) == nil {
			_, _ = effectGit(context.WithoutCancel(ctx), worktree.Binding{Path: x.manager.Repository}, "branch", "-D", "--quiet", scratch.Branch)
		}
	}
	path, err := safeWorktreeFile(scratch.Path, testFile)
	if err == nil {
		err = scrub()
	}
	if err == nil {
		err = os.MkdirAll(filepath.Dir(path), 0700)
	}
	if err == nil {
		err = os.WriteFile(path, source, 0600)
	}
	if err != nil {
		release()
		return w, none, err
	}
	return scratch, release, nil
}

// safeWorktreeFile joins a declared relative file under a worktree root.
func safeWorktreeFile(root, name string) (string, error) {
	if workflow.CheckArg("relative-path", name) != nil {
		return "", errors.New("invalid pinned test file")
	}
	return filepath.Join(root, name), nil
}
