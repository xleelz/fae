package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/effects"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/external"
	"github.com/xleelz/fae/workflow"
	"github.com/xleelz/fae/worktree"
)

func externalIntent(c LiveConfig, disposition domain.Disposition, dispatch engine.Dispatch) (external.Tool, external.Intent, error) {
	if isFleetProvisionTool(c, dispatch.Config.Subject) {
		return external.Tool{}, external.Intent{}, errors.New("fleet provisioning is operator-only outside task execution")
	}
	t, ok := c.ExternalTools[dispatch.Config.Subject]
	if !ok {
		return t, external.Intent{}, errors.New("external stage requires installed subject tool")
	}
	if disposition.Mode != domain.Normal {
		session := "local"
		if len(dispatch.Sessions) > 0 && dispatch.Sessions[0].ID != "" {
			session = dispatch.Sessions[0].ID
		}
		stageKey := dispatch.Stage
		if parts := strings.Split(dispatch.ID, "/"); len(parts) >= 3 {
			stageKey = parts[len(parts)-2] + "/" + dispatch.Stage
		}
		tree := dispatch.Tree
		if tree == "" {
			tree = "local"
		}
		i, _ := external.NewIntent(t, dispatch.TaskID, stageKey, dispatch.Config.IdempotenceKey, t.Role, tree, session, dispatch.Config.Pins)
		return t, i, errors.New("external effect forbidden in Local mode")
	}
	if len(dispatch.Sessions) != 1 || dispatch.Sessions[0].Role != dispatch.Config.Role {
		return t, external.Intent{}, errors.New("external effect requires exact role session")
	}
	parts := strings.Split(dispatch.ID, "/")
	if len(parts) < 3 {
		return t, external.Intent{}, errors.New("invalid dispatch identity")
	}
	// Stable across retries, but isolated by workflow revision/hash and stage.
	i, e := external.NewIntent(t, dispatch.TaskID, parts[len(parts)-2]+"/"+dispatch.Stage, dispatch.Config.IdempotenceKey, dispatch.Config.Role, dispatch.Tree, dispatch.Sessions[0].ID, dispatch.Config.Pins)
	if disposition.Live {
		if destErr := validateExternalDestinations(t, dispatch.Config.Pins, disposition); destErr != nil {
			return t, i, destErr
		}
	}
	return t, i, e
}
func (x *liveExecutor) executeExternal(ctx context.Context, dispatch engine.Dispatch) (any, error) {
	x.daemon.mu.Lock()
	snapshot, ok := x.daemon.jobs[dispatch.TaskID]
	x.daemon.mu.Unlock()
	if !ok {
		return nil, errors.New("external task missing")
	}
	var env envelope
	if e := decodeEnvelope(snapshot.Payload, &env); e != nil {
		return nil, e
	}
	if err := x.daemon.checkPinnedTarget(ctx, env.Admission, env.Job.Document); err != nil {
		return nil, err
	}
	selected, err := x.daemon.liveForAdmission(ctx, env.Admission)
	if err != nil {
		return nil, err
	}
	// An external effect reads the selected target's tools and policies,
	// never the task's file scope.
	copyX := *x
	copyX.config = selected.config
	x = &copyX
	if isFleetProvisionTool(x.config, dispatch.Config.Subject) {
		return nil, errors.New("fleet provisioning is operator-only outside task execution")
	}
	if t, exists := x.config.ExternalTools[dispatch.Config.Subject]; exists && reviewOperation(x.config.reviewOperation(dispatch.TaskID, dispatch.Config.Subject)) {
		if env.Job.Disposition.Mode != domain.Normal || len(dispatch.Sessions) != 1 || dispatch.Sessions[0].Role != t.Role || dispatch.Config.Role != t.Role {
			return nil, errors.New("review effect requires Normal mode and exact role grant")
		}
		a, _ := json.Marshal(t.Arguments)
		b, _ := json.Marshal(dispatch.Config.Pins)
		if string(a) != string(b) {
			return nil, errors.New("review arguments differ from installed pins")
		}
		if env.Job.Disposition.Live {
			if e := validateExternalDestinations(t, dispatch.Config.Pins, env.Job.Disposition); e != nil {
				return nil, e
			}
		}
		return x.executeReview(ctx, dispatch, env, t)
	}
	if output, managed, err := x.executeManagedTicket(ctx, env.Job, dispatch); managed {
		return output, err
	}
	t, i, e := externalIntent(x.config, env.Job.Disposition, dispatch)
	if e != nil {
		if i.Key != "" {
			_ = x.daemon.recordExternalOutcome(ctx, dispatch, i, external.Receipt{}, "refused")
		}
		return nil, e
	}
	raw, _ := json.Marshal(i)
	if e = x.daemon.store.RecordEffect(ctx, effectKey(dispatch)+"/external-intent", dispatch.TaskID, "intent", raw); e != nil {
		return nil, e
	}
	if !env.Job.Disposition.Live {
		if save := x.daemon.recordExternalOutcome(ctx, dispatch, i, external.Receipt{}, "dry-run"); save != nil {
			return nil, save
		}
		return externalOutput(i, external.Receipt{}, "dry-run"), nil
	}
	receipt, found, e := external.Query(ctx, t, i)
	if save := x.daemon.recordExternalObservation(ctx, dispatch, i, receipt); save != nil {
		return nil, save
	}
	if e != nil {
		_ = x.daemon.recordExternalOutcome(ctx, dispatch, i, receipt, "failed")
		return nil, e
	}
	if !found {
		receipt, e = external.Perform(ctx, t, i)
		if save := x.daemon.recordExternalObservation(ctx, dispatch, i, receipt); save != nil {
			return nil, save
		}
		if e != nil {
			_ = x.daemon.recordExternalOutcome(ctx, dispatch, i, receipt, "failed")
			return nil, e
		}
	}
	if e = external.Match(t, i, receipt); e != nil {
		_ = x.daemon.recordExternalOutcome(ctx, dispatch, i, receipt, "failed")
		return nil, e
	}
	if save := x.daemon.recordExternalOutcome(ctx, dispatch, i, receipt, "reconciled"); save != nil {
		return nil, save
	}
	return externalOutput(i, receipt, "reconciled"), nil
}
func externalOutput(i external.Intent, r external.Receipt, state string) map[string]any {
	return map[string]any{"tool": i.Tool, "argument_digest": i.Digest, "external_id": r.ExternalID, "reconciliation_state": state, "idempotency_key": i.Key}
}

// Persist the reported identity before validating it. Even a mismatched response
// remains discoverable as unreconciled evidence; operators cannot inject it.
func (d *Daemon) recordExternalObservation(ctx context.Context, dispatch engine.Dispatch, i external.Intent, r external.Receipt) error {
	if r.ExternalID == "" {
		return nil
	}
	raw, _ := json.Marshal(map[string]any{"intent": i, "reported": r, "reconciliation_state": "unreconciled"})
	hash, _ := domain.ContentHash(raw)
	return d.store.RecordEffect(ctx, effectKey(dispatch)+"/observed/"+hash, dispatch.TaskID, "observed", raw)
}

func (d *Daemon) recordExternalOutcome(ctx context.Context, dispatch engine.Dispatch, i external.Intent, r external.Receipt, outcome string) error {
	if i.Key == "" {
		return nil
	}
	raw, _ := json.Marshal(map[string]any{"intent": i, "reported": r, "outcome": outcome})
	hash, _ := domain.ContentHash(raw)
	return d.store.RecordEffect(ctx, effectKey(dispatch)+"/outcome/"+hash, dispatch.TaskID, "observed", raw)
}
func (d *Daemon) reconcileExternal(ctx context.Context, dispatch engine.Dispatch, w worktree.Binding) (engine.Result, error) {
	var zero engine.Result
	snapshot, ok := d.jobs[dispatch.TaskID]
	if !ok {
		return zero, errors.New("external task missing")
	}
	var env envelope
	if e := decodeEnvelope(snapshot.Payload, &env); e != nil {
		return zero, e
	}
	if err := d.checkPinnedTarget(ctx, env.Admission, env.Job.Document); err != nil {
		return zero, err
	}
	selected, err := d.liveForAdmission(ctx, env.Admission)
	if err != nil {
		return zero, err
	}
	if isFleetProvisionTool(selected.config, dispatch.Config.Subject) {
		return zero, errors.New("fleet provisioning is operator-only outside task execution")
	}
	if dispatch.Config.IsGitPush() {
		target, err := d.pinnedTarget(ctx, env.Admission)
		if err != nil {
			return zero, err
		}
		output, err := selected.executeGitPush(ctx, dispatch, w, env, target)
		if err != nil {
			return zero, err
		}
		raw, _ := json.Marshal(output)
		return engine.Result{ExecutionKind: "live", DispatchID: dispatch.ID, TaskID: dispatch.TaskID, Tree: dispatch.Tree, Session: dispatch.Sessions[0].ID, ProducedAt: time.Now().UTC(), Output: raw}, nil
	}
	if managedTicket(selected.config, env.Job, dispatch.Config.Subject) {
		tree, err := d.live.broker.Tree(ctx, w)
		if err != nil {
			return zero, err
		}
		if tree != dispatch.Tree {
			return zero, errors.New("ticket recovery tree changed")
		}
		head, err := effectGit(ctx, w, "rev-parse", "HEAD")
		if err != nil {
			return zero, err
		}
		raw, err := d.store.GetMarker(ctx, effectKey(dispatch)+"/intent")
		if err != nil {
			return zero, err
		}
		var intent effectIntent
		if err = decodeEffect(raw, &intent); err != nil {
			return zero, err
		}
		if intent.Head != head || intent.Worktree != w {
			return zero, errors.New("ticket recovery worktree/head changed")
		}
		output, _, err := selected.executeManagedTicket(ctx, env.Job, dispatch)
		if err != nil {
			return zero, err
		}
		raw, _ = json.Marshal(output)
		return engine.Result{ExecutionKind: "live", DispatchID: dispatch.ID, TaskID: dispatch.TaskID, Tree: dispatch.Tree, Session: dispatch.Sessions[0].ID, ProducedAt: time.Now().UTC(), Output: raw}, nil
	}
	if !env.Job.Disposition.AllowsExternalEffects() {
		return zero, errors.New("external recovery requires live Normal disposition")
	}
	t, i, e := externalIntent(selected.config, env.Job.Disposition, dispatch)
	managedReview := reviewOperation(selected.config.reviewOperation(dispatch.TaskID, dispatch.Config.Subject))
	if managedReview {
		t, i, e = selected.reviewRecoveryIntent(ctx, dispatch, t)
	}
	if e != nil {
		return zero, e
	}
	raw, e := d.store.GetMarker(ctx, effectKey(dispatch)+"/external-intent")
	if e != nil {
		return zero, e
	}
	expected, _ := json.Marshal(i)
	var recorded external.Intent
	if e = decodeEffect(raw, &recorded); e != nil {
		return zero, e
	}
	actual, _ := json.Marshal(recorded)
	if string(expected) != string(actual) {
		return zero, errors.New("external intent changed")
	}
	tree, e := d.live.broker.Tree(ctx, w)
	if e != nil {
		return zero, e
	}
	if tree != dispatch.Tree {
		return zero, errors.New("external recovery tree changed")
	}
	head, e := effectGit(ctx, w, "rev-parse", "HEAD")
	if e != nil {
		return zero, e
	}
	raw, e = d.store.GetMarker(ctx, effectKey(dispatch)+"/intent")
	if e != nil {
		return zero, e
	}
	var local effectIntent
	if e = decodeEffect(raw, &local); e != nil {
		return zero, e
	}
	if local.Head != head || local.Worktree != w {
		return zero, errors.New("external recovery head/worktree changed")
	}
	if managedReview && selected.config.reviewOperation(dispatch.TaskID, dispatch.Config.Subject) == "review_read" {
		value, err := selected.executeReview(ctx, dispatch, env, t)
		if err != nil {
			return zero, err
		}
		output, _ := json.Marshal(value)
		return engine.Result{ExecutionKind: "live", DispatchID: dispatch.ID, TaskID: dispatch.TaskID, Tree: dispatch.Tree, Session: dispatch.Sessions[0].ID, ProducedAt: time.Now().UTC(), Output: output}, nil
	}
	receipt, found, e := external.Query(ctx, t, i)
	if save := d.recordExternalObservation(ctx, dispatch, i, receipt); save != nil {
		return zero, save
	}
	if e != nil {
		return zero, e
	}
	if !found {
		return zero, errors.New("external query absent; request may still complete, replay refused")
	}
	if e = external.Match(t, i, receipt); e != nil {
		return zero, e
	}
	var value any = externalOutput(i, receipt, "reconciled")
	if managedReview {
		s, err := d.reviewCheckpoint(ctx, dispatch.TaskID)
		if err != nil {
			return zero, err
		}
		value, e = selected.finishReview(ctx, dispatch, i, receipt, s)
		if e != nil {
			return zero, e
		}
	}
	output, _ := json.Marshal(value)
	r := engine.Result{ExecutionKind: "live", DispatchID: dispatch.ID, TaskID: dispatch.TaskID, Tree: dispatch.Tree, Session: dispatch.Sessions[0].ID, ProducedAt: time.Now().UTC(), Output: output}
	return r, nil
}

func validateExternalStages(c LiveConfig, doc workflow.Document, p AdmitParams) error {
	if err := validateTicketAdmission(c, doc, p); err != nil {
		return err
	}
	for name, s := range doc.Stages {
		if s.Primitive != workflow.ExternalEffect || s.IsGitPush() {
			continue
		}
		if isFleetProvisionTool(c, s.Subject) {
			return errors.New("fleet provisioning is operator-only outside task execution")
		}
		t, ok := c.ExternalTools[s.Subject]
		if !ok {
			return fmt.Errorf("stage %s external subject not installed", name)
		}
		probe := engine.Job{Document: doc, Disposition: domain.Disposition{Mode: domain.Normal, Live: true}}
		if managedTicket(c, probe, s.Subject) {
			a, _ := json.Marshal(t.Arguments)
			b, _ := json.Marshal(s.Pins)
			if s.Role != t.Role || string(a) != string(b) {
				return errors.New("ticket stage differs from exact installed role/static pins")
			}
			continue
		}
		validationTool := t
		if reviewOperation(c.reviewOperation(p.ID, s.Subject)) {
			validationTool.DiscoveredSchema = nil
		}
		_, e := external.NewIntent(validationTool, "admission", name, s.IdempotenceKey, s.Role, "tree", "session", s.Pins)
		if e != nil {
			return e
		}
		if p.CommitExternal {
			if e := validateExternalDestinations(t, s.Pins, domain.Disposition{TicketRoom: p.TicketRoom, Destination: p.Destination, PushNamespace: p.PushNamespace}); e != nil {
				return e
			}
		}

	}
	return nil
}

func validateExternalDestinations(t external.Tool, pins map[string]string, d domain.Disposition) error {
	required := map[string]string{"destination": d.Destination, "push_namespace": d.PushNamespace}
	_, hasTicketPin := pins["ticket_room"]
	if hasTicketPin || strings.HasPrefix(t.Name, "ticket_") {
		required["ticket_room"] = d.TicketRoom
	}
	for key, want := range required {
		if want == "" || pins[key] != want {
			return fmt.Errorf("external %s missing or differs from admission", key)
		}
	}
	return nil
}

// executeGitPush executes an engine-run Local-mode git-push stage (FAE-EXT-023):
//   - verifies TargetConfig.Push has Mode == "fast-forward-only", matching
//     Remote, and target branch in Push.Branches;
//   - verifies the stage is preceded by passing protected-commit gates;
//   - checks idempotence on (remote, branch, sha);
//   - verifies <remote_sha> is an ancestor of <sha> (fast-forward only);
//   - scans the commit range <remote_sha>..<sha> against ContentPolicy and DenyPaths;
//   - runs git push <remote> <sha>:refs/heads/<branch> and records durable
//     external-effect receipt and audit records (FAE-EXT-006).
func (x *liveExecutor) executeGitPush(ctx context.Context, dispatch engine.Dispatch, w worktree.Binding, env envelope, target *TargetConfig) (map[string]any, error) {
	if dispatch.Config.Role != "" {
		return nil, errors.New("git-push stage must be executed by the engine without a role")
	}
	if target == nil || target.Push == nil {
		return nil, errors.New("git-push refused: target.push is not configured")
	}
	if target.Push.Mode != "fast-forward-only" {
		return nil, fmt.Errorf("git-push refused: target.push.mode must be \"fast-forward-only\", got %q", target.Push.Mode)
	}
	remote := strings.TrimSpace(dispatch.Config.Pins["remote"])
	if remote != "" && remote != target.Push.Remote {
		return nil, fmt.Errorf("git-push refused: stage remote %q does not match target.push.remote %q", remote, target.Push.Remote)
	}
	if remote == "" {
		remote = target.Push.Remote
	}
	branch := strings.TrimSpace(dispatch.Config.Pins["branch"])
	if branch == "" {
		branch = strings.TrimSpace(dispatch.Config.Pins["destination"])
	}
	if branch == "" && len(target.Push.Branches) == 1 {
		branch = target.Push.Branches[0]
	}
	if branch == "" {
		branch = target.IntegrationRef
	}
	branch = strings.TrimPrefix(branch, "refs/heads/")
	allowedBranch := false
	for _, b := range target.Push.Branches {
		if strings.TrimPrefix(strings.TrimSpace(b), "refs/heads/") == branch {
			allowedBranch = true
			break
		}
	}
	if !allowedBranch {
		return nil, fmt.Errorf("git-push refused: branch %q is not in target.push.branches", branch)
	}
	// Verify protected-commit gates passed on the workflow and job (FAE-CTL-050 / FAE-CTL-052).
	if x.daemon != nil {
		env.Job.Document.ValidationRecursionCeiling = x.daemon.recursionCeiling()
	}
	if issues := workflow.Validate(env.Job.Document); len(issues) > 0 {
		return nil, fmt.Errorf("git-push refused: workflow failed protected-commit floor validation: %v", issues)
	}
	hasGateOrReview := false
	for _, ev := range env.Job.Evidence {
		if ev.Type == "EngineRun" || ev.Type == "OracleVerdict" || ev.Type == "ReviewVerdict" || ev.Type == "Integration" || ev.Type == "Commit" {
			hasGateOrReview = true
			break
		}
		if st, ok := env.Job.Document.Stages[ev.Stage]; ok && (st.Primitive == workflow.Gate || st.Primitive == workflow.Review || st.Primitive == workflow.Integrate || st.Primitive == workflow.VcsCommit) {
			hasGateOrReview = true
			break
		}
	}
	for _, p := range env.Job.Provenance {
		if p.Primitive == workflow.Gate || p.Primitive == workflow.Review || p.Primitive == workflow.Integrate || p.Primitive == workflow.VcsCommit {
			hasGateOrReview = true
			break
		}
	}
	if !hasGateOrReview {
		return nil, errors.New("git-push refused: stage is not preceded by passing protected-commit gates")
	}

	// Resolve <sha> to push.
	sha := ""
	for _, a := range dispatch.Inputs {
		if a.Type == "Integration" || a.Type == "Commit" {
			if s, _ := a.Fields["sha"].(string); s != "" {
				sha = s
			}
		}
	}
	if sha == "" {
		for _, a := range env.Job.Evidence {
			if a.Type == "Integration" || a.Type == "Commit" {
				if s, _ := a.Fields["sha"].(string); s != "" {
					sha = s
				}
			}
		}
	}
	if sha == "" {
		var err error
		sha, err = effectGit(ctx, w, "rev-parse", "--verify", "HEAD^{commit}")
		if err != nil {
			return nil, fmt.Errorf("git-push: resolve HEAD commit: %w", err)
		}
	}
	sha = strings.TrimSpace(sha)

	idempKey := fmt.Sprintf("git-push:%s:%s:%s", remote, branch, sha)
	idempHash, _ := domain.ContentHash([]byte(idempKey))
	intentPayload := map[string]any{
		"key":             idempKey,
		"task":            dispatch.TaskID,
		"tool":            "git-push",
		"effect_kind":     "git-push",
		"digest":          idempHash,
		"argument_digest": idempHash,
		"arguments": map[string]string{
			"remote": remote,
			"branch": branch,
			"sha":    sha,
		},
		"remote":          remote,
		"branch":          branch,
		"sha":             sha,
		"idempotency_key": idempKey,
	}
	intentRaw, _ := json.Marshal(intentPayload)
	if x.daemon != nil && x.daemon.store != nil {
		if err := x.daemon.store.RecordEffect(ctx, effectKey(dispatch)+"/external-intent", dispatch.TaskID, "intent", intentRaw); err != nil {
			return nil, err
		}
	}
	recordOutcome := func(outcome string, extID string, recState string) error {
		if x.daemon == nil || x.daemon.store == nil {
			return nil
		}
		obsRaw, _ := json.Marshal(map[string]any{
			"intent":               intentPayload,
			"external_id":          extID,
			"reconciliation_state": recState,
			"outcome":              outcome,
		})
		obsHash, _ := domain.ContentHash(obsRaw)
		if extID != "" {
			if err := x.daemon.store.RecordEffect(ctx, effectKey(dispatch)+"/observed/"+obsHash, dispatch.TaskID, "observed", obsRaw); err != nil {
				return err
			}
		}
		phase := "observed"
		if outcome == "performed" {
			phase = "performed"
		}
		return x.daemon.store.RecordEffect(ctx, effectKey(dispatch)+"/outcome/"+obsHash, dispatch.TaskID, phase, obsRaw)
	}

	// Resolve <remote_sha> on <remote> for refs/heads/<branch>.
	lsCmd := exec.CommandContext(ctx, "git", "-c", "core.hooksPath=/dev/null", "ls-remote", "--refs", remote, "refs/heads/"+branch)
	lsCmd.Dir = w.Path
	lsCmd.Env = cleanGitEnv()
	var lsOut, lsErr bytes.Buffer
	lsCmd.Stdout = &lsOut
	lsCmd.Stderr = &lsErr
	if err := lsCmd.Run(); err != nil {
		_ = recordOutcome("failed", "", "unreconciled")
		return nil, fmt.Errorf("git-push ls-remote %s refs/heads/%s: %w (%s)", remote, branch, err, strings.TrimSpace(lsErr.String()))
	}
	remoteSHA := ""
	if line := strings.TrimSpace(lsOut.String()); line != "" {
		fields := strings.Fields(line)
		if len(fields) > 0 {
			remoteSHA = fields[0]
		}
	}

	extID := fmt.Sprintf("%s:refs/heads/%s@%s", remote, branch, sha)
	// Idempotence check: if remote branch is already at sha (or sha is already an ancestor of remoteSHA
	// and we previously recorded this push), adopt without re-pushing.
	if remoteSHA == sha {
		if err := recordOutcome("reconciled", extID, "adopted"); err != nil {
			return nil, err
		}
		return map[string]any{
			"tool":                 "git-push",
			"effect_kind":          "git-push",
			"remote":               remote,
			"branch":               branch,
			"sha":                  sha,
			"remote_sha":           remoteSHA,
			"external_id":          extID,
			"idempotency_key":      idempKey,
			"reconciliation_state": "adopted",
		}, nil
	}

	// Fast-forward ancestry check: if remoteSHA != "", verify remoteSHA is an ancestor of sha.
	if remoteSHA != "" {
		// Ensure remoteSHA object is present locally before merge-base check.
		catCmd := exec.CommandContext(ctx, "git", "-c", "core.hooksPath=/dev/null", "cat-file", "-e", remoteSHA+"^{commit}")
		catCmd.Dir = w.Path
		catCmd.Env = cleanGitEnv()
		if catCmd.Run() != nil {
			fetchCmd := exec.CommandContext(ctx, "git", "-c", "core.hooksPath=/dev/null", "fetch", "--quiet", remote, "refs/heads/"+branch)
			fetchCmd.Dir = w.Path
			fetchCmd.Env = cleanGitEnv()
			_ = fetchCmd.Run()
		}
		ancCmd := exec.CommandContext(ctx, "git", "-c", "core.hooksPath=/dev/null", "merge-base", "--is-ancestor", remoteSHA, sha)
		ancCmd.Dir = w.Path
		ancCmd.Env = cleanGitEnv()
		if err := ancCmd.Run(); err != nil {
			// Check if sha is already an ancestor of remoteSHA and we previously recorded git-push/idempHash.
			revAncCmd := exec.CommandContext(ctx, "git", "-c", "core.hooksPath=/dev/null", "merge-base", "--is-ancestor", sha, remoteSHA)
			revAncCmd.Dir = w.Path
			revAncCmd.Env = cleanGitEnv()
			if revAncCmd.Run() == nil && x.daemon != nil && x.daemon.store != nil {
				if raw, mErr := x.daemon.store.GetMarker(ctx, "git-push/"+idempHash); mErr == nil && len(raw) > 0 {
					if err := recordOutcome("reconciled", extID, "adopted"); err != nil {
						return nil, err
					}
					return map[string]any{
						"tool":                 "git-push",
						"effect_kind":          "git-push",
						"remote":               remote,
						"branch":               branch,
						"sha":                  sha,
						"remote_sha":           remoteSHA,
						"external_id":          extID,
						"idempotency_key":      idempKey,
						"reconciliation_state": "adopted",
					}, nil
				}
			}
			_ = recordOutcome("refused", "", "refused")
			return nil, fmt.Errorf("git-push refused: non-fast-forward push of %s to %s/%s (remote at %s)", sha, remote, branch, remoteSHA)
		}
	}

	// Content & path policy scan across <remote_sha>..<sha> (FAE-SEC-009 & FAE-EXT-023).
	scanBase := remoteSHA
	if scanBase == "" {
		scanBase = env.Job.Base
	}
	if err := effects.ScanGitRange(ctx, w.Path, scanBase, sha, target.EffectsContentPolicy()); err != nil {
		_ = recordOutcome("refused", "", "refused")
		return nil, err
	}

	// Execute git push <remote> <sha>:refs/heads/<branch>.
	pushCmd := exec.CommandContext(ctx, "git", "-c", "core.hooksPath=/dev/null", "push", remote, sha+":refs/heads/"+branch)
	pushCmd.Dir = w.Path
	pushCmd.Env = cleanGitEnv()
	var pushOut, pushErr bytes.Buffer
	pushCmd.Stdout = &pushOut
	pushCmd.Stderr = &pushErr
	if err := pushCmd.Run(); err != nil {
		_ = recordOutcome("failed", "", "unreconciled")
		return nil, fmt.Errorf("git-push failed: %w (%s)", err, strings.TrimSpace(pushErr.String()))
	}

	receipt := map[string]any{
		"key":                  idempKey,
		"task":                 dispatch.TaskID,
		"tool":                 "git-push",
		"effect_kind":          "git-push",
		"digest":               idempHash,
		"argument_digest":      idempHash,
		"remote":               remote,
		"branch":               branch,
		"sha":                  sha,
		"remote_sha":           remoteSHA,
		"external_id":          extID,
		"idempotency_key":      idempKey,
		"reconciliation_state": "reconciled",
	}
	if x.daemon != nil && x.daemon.store != nil {
		receiptRaw, _ := json.Marshal(receipt)
		if err := x.daemon.store.RecordEffect(ctx, "git-push/"+idempHash, dispatch.TaskID, "performed", receiptRaw); err != nil {
			return nil, err
		}
	}
	if err := recordOutcome("performed", extID, "reconciled"); err != nil {
		return nil, err
	}
	return receipt, nil
}
