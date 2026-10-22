package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/external"
	"github.com/xleelz/fae/workflow"
	"github.com/xleelz/fae/worktree"
	"path/filepath"
	"strconv"
	"strings"
)

type reviewCheckpoint struct {
	ID        string `json:"id"`
	Revision  int    `json:"revision"`
	Published bool   `json:"published"`
	Merged    bool   `json:"merged"`
}

func reviewOperation(name string) bool {
	switch name {
	case "review_create", "review_read", "review_revision", "review_publish", "review_merge":
		return true
	}
	return false
}
func reviewKey(task string) string {
	raw, _ := json.Marshal(task)
	h, _ := domain.ContentHash(raw)
	return "review-lifecycle/" + h
}
func (d *Daemon) reviewCheckpoint(ctx context.Context, task string) (reviewCheckpoint, error) {
	var s reviewCheckpoint
	raw, e := d.store.GetMarker(ctx, reviewKey(task))
	if errors.Is(e, sql.ErrNoRows) {
		return s, nil
	}
	if e != nil {
		return s, e
	}
	e = json.Unmarshal(raw, &s)
	return s, e
}
func (d *Daemon) saveReview(ctx context.Context, task, operation string, s reviewCheckpoint) error {
	raw, _ := json.Marshal(s)
	return d.store.PutMarker(ctx, controlplane.Audit{Actor: "daemon", Action: "review " + operation, Inputs: raw, Outcome: "performed"}, reviewKey(task), raw)
}

func (c LiveConfig) reviewOperation(task, subject string) string {
	t := c.ExternalTools[subject]
	p := c.ReviewPolicies[task]
	return p.Operation(subject, t.Name)
}
func configureReviewJob(c LiveConfig, j *engine.Job) error {
	stages := map[string]string{}
	for name, s := range j.Document.Stages {
		if s.Primitive == workflow.ExternalEffect {
			if _, ok := c.ExternalTools[s.Subject]; ok && reviewOperation(c.reviewOperation(j.TaskID, s.Subject)) {
				stages[name] = c.reviewOperation(j.TaskID, s.Subject)
			}
		}
	}
	if len(stages) == 0 {
		return nil
	}
	p, ok := c.ReviewPolicies[j.TaskID]
	if !ok {
		return fmt.Errorf("missing pre-provisioned review workspace binding for task %s", j.TaskID)
	}
	statusGranted := false
	for _, stage := range j.Document.Stages {
		if stage.Primitive == workflow.ExternalEffect && stage.Subject == p.StatusTool && stage.Role == c.ExternalTools[p.StatusTool].Role {
			statusGranted = true
		}
	}
	for _, op := range stages {
		if (op == "review_read" || op == "review_publish" || op == "review_merge") && !statusGranted {
			return errors.New("review status tool requires declared external read stage and role")
		}
	}
	j.ReviewStages = stages
	j.ReviewDisabled = c.DisableReviewPublishMerge || p.DisablePublishMerge
	j.ReviewRemediation = p.RemediationStage
	j.ReviewPollSeconds = p.PollSeconds
	if j.ReviewPollSeconds == 0 {
		j.ReviewPollSeconds = engine.DefaultReviewPollSeconds
	}
	j.ReviewMaxPolls = p.MaxPolls
	if j.ReviewMaxPolls == 0 {
		j.ReviewMaxPolls = engine.DefaultReviewMaxPolls
	}
	if j.ReviewPollSeconds < 1 || j.ReviewPollSeconds > 3600 || j.ReviewMaxPolls < 1 || j.ReviewMaxPolls > 3600 {
		return errors.New("review poll_seconds and max_polls must each be within 1..3600")
	}
	j.ReviewMaxRevisions = p.MaxRevisions
	if j.ReviewMaxRevisions == 0 {
		j.ReviewMaxRevisions = 5
	}
	if j.ReviewMaxRevisions < 1 || j.ReviewMaxRevisions > 100 {
		return errors.New("review revision bound must be 1..100")
	}
	for stage, op := range stages {
		if op == "review_revision" {
			if j.ReviewRevisionStage != "" {
				return errors.New("only one review revision stage permitted")
			}
			j.ReviewRevisionStage = stage
		}
	}
	if j.ReviewRevisionStage != "" {
		for stage, op := range stages {
			if op == "review_create" && j.Document.Stages[stage].Failure != j.ReviewRevisionStage {
				return errors.New("review create alternate edge must target same-review revision stage")
			}
		}
	}
	for _, op := range stages {
		if op == "review_merge" && !j.ReviewDisabled && j.Disposition.Live {
			j.ReviewMergeRequired = true
		}
	}
	if p.RemediationStage != "" {
		s, ok := j.Document.Stages[p.RemediationStage]
		if !ok || s.Primitive != workflow.Loop {
			return errors.New("review remediation must name bounded implementation loop")
		}
		implementing := false
		for _, edge := range j.Document.Edges {
			if edge.From == p.RemediationStage {
				target := j.Document.Stages[edge.To]
				if target.Primitive == workflow.AgentTurn && target.ChangeProducer {
					implementing = true
				}
			}
		}
		if !implementing {
			return errors.New("review remediation loop must enter implementing change-producing agent")
		}
	}
	for stage, op := range stages {
		if op == "review_read" && p.RemediationStage != "" && j.Document.Stages[stage].Failure != p.RemediationStage {
			return errors.New("review read failure edge must target configured implementation loop")
		}
	}
	return nil
}

// bindReview validates the physical workspace before any external request and
// substitutes only daemon-owned identity tokens in installed argument pins.
func (x *liveExecutor) bindReview(ctx context.Context, dispatch engine.Dispatch, t external.Tool, s reviewCheckpoint) (external.Tool, external.Intent, external.ReviewPolicy, error) {
	var zero external.Intent
	op := x.config.reviewOperation(dispatch.TaskID, dispatch.Config.Subject)
	p, ok := x.config.ReviewPolicies[dispatch.TaskID]
	if !ok {
		return t, zero, p, fmt.Errorf("missing pre-provisioned review workspace binding for task %s", dispatch.TaskID)
	}
	if p.Package == "" || !filepath.IsAbs(p.Workspace) || p.Branch == "" || p.IntegrationTip == "" {
		return t, zero, p, errors.New("invalid pre-provisioned review package workspace binding")
	}
	if op == "review_create" || op == "review_revision" {
		real, e := filepath.EvalSymlinks(p.Workspace)
		if e != nil || real != filepath.Clean(p.Workspace) {
			return t, zero, p, errors.New("review workspace missing or symlinked")
		}
		w := worktree.Binding{Path: p.Workspace, Branch: p.Branch}
		branch, e := effectGit(ctx, w, "symbolic-ref", "--short", "HEAD")
		if e != nil || branch != p.Branch {
			return t, zero, p, fmt.Errorf("review workspace %s is not pinned to task branch %s", p.Workspace, p.Branch)
		}
		tree, e := effectGit(ctx, w, "rev-parse", "HEAD^{tree}")
		if e != nil || tree != dispatch.Tree {
			return t, zero, p, errors.New("review workspace tree differs from reviewed task tree")
		}
		if x.manager != nil && x.manager.Verify(ctx, w) == nil {
			measured, err := x.broker.Tree(ctx, w)
			if err != nil || measured != dispatch.Tree {
				return t, zero, p, errors.New("review workspace working tree differs from reviewed tree")
			}
		} else {
			dirty, err := effectGit(ctx, w, "status", "--porcelain")
			if err != nil || dirty != "" {
				return t, zero, p, errors.New("review workspace is dirty")
			}
		}
	}
	args := map[string]string{}
	for k, v := range t.Arguments {
		switch v {
		case "$review":
			if s.ID == "" {
				return t, zero, p, errors.New("review identity not yet recorded")
			}
			v = s.ID
		case "$workspace":
			v = p.Workspace
		case "$branch":
			v = p.Branch
		case "$package":
			v = p.Package
		case "$integration_tip":
			v = p.IntegrationTip
		case "$revision":
			v = strconv.Itoa(s.Revision + 1)
		}
		args[k] = v
	}
	if (op == "review_create" || op == "review_revision") && (args[p.ArgumentKey("workspace")] != p.Workspace || args[p.ArgumentKey("branch")] != p.Branch || args[p.ArgumentKey("package")] != p.Package) {
		return t, zero, p, errors.New("review creation/revision lacks pinned workspace, branch or package arguments")
	}
	if op != "review_create" && op != "review_baseline" && args[p.ArgumentKey("review_id")] != s.ID {
		return t, zero, p, errors.New("review operation lacks existing review identity")
	}
	if op == "review_create" {
		if args[p.ArgumentKey("draft")] != "true" {
			return t, zero, p, errors.New("review creation requires draft=true")
		}
		for _, semantic := range []string{"auto_publish", "auto_merge", "auto_merge_with_rerun"} {
			v := args[p.ArgumentKey(semantic)]
			if v != "" && v != "false" {
				return t, zero, p, errors.New("review creation implicit publication forbidden")
			}
		}
	}
	t.Arguments = args
	key := dispatch.Config.IdempotenceKey
	if op == "review_revision" {
		key += "/" + strconv.Itoa(s.Revision+1) + "/" + dispatch.Tree
	}
	stage := dispatch.Stage
	if op == "review_create" {
		stage = "review-create"
		key = p.Branch
	}
	i, e := external.NewIntent(t, dispatch.TaskID, stage, key, dispatch.Config.Role, dispatch.Tree, dispatch.Sessions[0].ID, args)
	if e == nil && op == "review_create" {
		identity, _ := json.Marshal([]string{dispatch.TaskID, t.Server, t.Name, p.Branch})
		i.Key, _ = domain.ContentHash(identity)
	}
	return t, i, p, e
}

func (x *liveExecutor) reviewRead(ctx context.Context, dispatch engine.Dispatch, alias string, s reviewCheckpoint) (external.Receipt, error) {
	return x.reviewReadWithEvidence(ctx, dispatch, alias, s, true)
}
func (x *liveExecutor) reviewReadWithEvidence(ctx context.Context, dispatch engine.Dispatch, alias string, s reviewCheckpoint, record bool) (external.Receipt, error) {
	t, ok := x.config.ExternalTools[alias]
	if !ok {
		return external.Receipt{}, fmt.Errorf("missing installed review read tool %s", alias)
	}
	if len(dispatch.Sessions) != 1 || dispatch.Sessions[0].Role != t.Role || dispatch.Config.Role != t.Role {
		return external.Receipt{}, errors.New("review read role is not the current declared role grant")
	}
	dispatch.Config.Subject = alias
	t, i, _, e := x.bindReview(ctx, dispatch, t, s)
	if e != nil {
		return external.Receipt{}, e
	}
	r, e := external.Read(ctx, t, i)
	if record {
		if save := x.daemon.recordExternalObservation(ctx, dispatch, i, r); save != nil {
			return r, save
		}
		if e != nil {
			raw, _ := json.Marshal(map[string]any{"intent": i, "error": e.Error()})
			if save := x.daemon.store.PutMarker(ctx, controlplane.Audit{Actor: "daemon", Action: "review read refused", Outcome: "error", Inputs: raw}, effectKey(dispatch)+"/read-error", raw); save != nil {
				return r, save
			}
		}
	}
	return r, e
}

type reviewRefusal struct {
	cause error
	wait  bool
}

func (e *reviewRefusal) Error() string { return e.cause.Error() }
func (e *reviewRefusal) Unwrap() error { return e.cause }
func (x *liveExecutor) executeReview(ctx context.Context, dispatch engine.Dispatch, env envelope, t external.Tool) (output any, err error) {
	queryStarted := false
	defer func() {
		if err != nil && !queryStarted {
			text := err.Error()
			wait := strings.Contains(text, "publish requirements not all Pass") || strings.Contains(text, "merge requirements/comments/publication incomplete")
			err = &reviewRefusal{cause: err, wait: wait}
		}
	}()

	op := x.config.reviewOperation(dispatch.TaskID, dispatch.Config.Subject)
	// Dry runs do not query status, baseline or effect endpoints.
	if !env.Job.Disposition.Live {
		return externalOutput(external.Intent{Tool: t.Name}, external.Receipt{}, "dry-run"), nil
	}
	s, e := x.daemon.reviewCheckpoint(ctx, dispatch.TaskID)
	if e != nil {
		return nil, e
	}
	p, ok := x.config.ReviewPolicies[dispatch.TaskID]
	if !ok {
		return nil, fmt.Errorf("missing pre-provisioned review workspace binding for task %s", dispatch.TaskID)
	}
	if (op == "review_publish" || op == "review_merge") && (x.config.DisableReviewPublishMerge || p.DisablePublishMerge) {
		return reviewOutput(external.Intent{Tool: t.Name}, external.Receipt{ExternalID: s.ID}, "disabled", s, "disabled"), nil
	}
	t, i, p, e := x.bindReview(ctx, dispatch, t, s)
	if e != nil {
		return nil, e
	}
	if op == "review_create" {
		if _, e = x.checkReviewTargetEvidence(ctx, dispatch.TaskID); e != nil {
			return nil, e
		}
	}

	if op == "review_read" {
		if dispatch.Config.Subject != p.StatusTool {
			return nil, errors.New("review read stage must grant configured status tool")
		}
		raw, _ := json.Marshal(i)
		if e = x.daemon.store.RecordEffect(ctx, effectKey(dispatch)+"/external-intent", dispatch.TaskID, "intent", raw); e != nil {
			return nil, e
		}
	}
	if op == "review_read" || op == "review_publish" || op == "review_merge" {
		receipt, e := x.reviewRead(ctx, dispatch, p.StatusTool, s)
		if e != nil {
			return nil, e
		}
		status, e := external.DecodeReviewStatus(receipt.Data)
		if e != nil {
			return nil, e
		}
		if status.ReviewID != s.ID || status.Tree != dispatch.Tree {
			return nil, errors.New("ReviewGateFailed: status identity/tree differs")
		}
		decision, e := external.EvaluateReview(status)
		if e != nil {
			return nil, e
		}
		if op == "review_read" {
			kind := "waiting"
			if (!s.Published && decision.Publish) || (s.Published && decision.Merge) {
				kind = "ready"
			}
			if decision.Remediate {
				kind = "remediation"
			}
			out := reviewOutput(i, receipt, "read", s, kind)
			if kind == "remediation" {
				w := worktree.Binding{Path: p.Workspace, Branch: p.Branch}
				committedTree, err := effectGit(ctx, w, "rev-parse", "HEAD^{tree}")
				if err != nil || committedTree != dispatch.Tree {
					return nil, errors.New("review remediation workspace commit does not match reviewed tree")
				}
				head, err := effectGit(ctx, w, "rev-parse", "HEAD")
				if err != nil {
					return nil, err
				}
				out["review_commit"] = head
			}
			return out, nil
		}
		if op == "review_publish" && !decision.Publish {
			return nil, errors.New("ReviewGateFailed: publish requirements not all Pass")
		}
		if op == "review_merge" && (!decision.Merge || !s.Published) {
			return nil, errors.New("ReviewGateFailed: merge requirements/comments/publication incomplete")
		}
	}
	max := p.MaxRevisions
	if max == 0 {
		max = 5
	}
	if op == "review_revision" && s.Revision >= max {
		return nil, errors.New("ReviewRevisionCap: implementing remediation exhausted")
	}
	raw, _ := json.Marshal(i)
	if e = x.daemon.store.RecordEffect(ctx, effectKey(dispatch)+"/external-intent", dispatch.TaskID, "intent", raw); e != nil {
		return nil, e
	}
	queryStarted = true
	receipt, found, e := external.Query(ctx, t, i)
	if save := x.daemon.recordExternalObservation(ctx, dispatch, i, receipt); save != nil {
		return nil, save
	}
	if e != nil {
		return nil, e
	}
	if !found {
		receipt, e = external.Perform(ctx, t, i)
		if save := x.daemon.recordExternalObservation(ctx, dispatch, i, receipt); save != nil {
			return nil, save
		}
		if e != nil {
			return nil, e
		}
	}
	if e = external.Match(t, i, receipt); e != nil {
		return nil, e
	}
	return x.finishReview(ctx, dispatch, i, receipt, s)
}
func reviewOutput(i external.Intent, r external.Receipt, state string, s reviewCheckpoint, decision string) map[string]any {
	o := externalOutput(i, r, state)
	o["review_id"] = s.ID
	o["review_revision"] = s.Revision
	o["review_merged"] = s.Merged
	o["review_decision"] = decision
	return o
}
func (x *liveExecutor) finishReview(ctx context.Context, dispatch engine.Dispatch, i external.Intent, r external.Receipt, s reviewCheckpoint) (any, error) {
	op := x.config.reviewOperation(dispatch.TaskID, dispatch.Config.Subject)
	if op != "review_create" && r.ExternalID != s.ID {
		return nil, errors.New("review mutation returned different review identity")
	}
	switch op {
	case "review_create":
		if s.ID != "" && s.ID != r.ExternalID {
			return nil, errors.New("second review identity refused")
		}
		s.ID = r.ExternalID
		if s.Revision == 0 {
			s.Revision = 1
		}
	case "review_revision":
		n, e := strconv.Atoi(i.Arguments[x.config.ReviewPolicies[dispatch.TaskID].ArgumentKey("revision")])
		if e != nil || n < s.Revision || n > s.Revision+1 {
			return nil, errors.New("invalid review revision progression")
		}
		s.Revision = n
		s.Published = false
		s.Merged = false
	case "review_publish":
		s.Published = true
	case "review_merge":
		s.Merged = true
	}
	if e := x.daemon.saveReview(ctx, dispatch.TaskID, i.Tool, s); e != nil {
		return nil, e
	}
	return reviewOutput(i, r, "reconciled", s, "performed"), nil
}

func (x *liveExecutor) reviewRecoveryIntent(ctx context.Context, dispatch engine.Dispatch, t external.Tool) (external.Tool, external.Intent, error) {
	var i external.Intent
	op := x.config.reviewOperation(dispatch.TaskID, dispatch.Config.Subject)
	p, ok := x.config.ReviewPolicies[dispatch.TaskID]
	if !ok {
		return t, i, errors.New("review workspace policy missing during recovery")
	}
	if (op == "review_publish" || op == "review_merge") && (x.config.DisableReviewPublishMerge || p.DisablePublishMerge) {
		return t, i, errors.New("review publish/merge disabled; recovery cannot adopt")
	}
	raw, e := x.daemon.store.GetMarker(ctx, effectKey(dispatch)+"/external-intent")
	if e != nil {
		return t, i, e
	}
	if e = decodeEffect(raw, &i); e != nil {
		return t, i, e
	}
	s, e := x.daemon.reviewCheckpoint(ctx, dispatch.TaskID)
	if e != nil {
		return t, i, e
	}
	// A receipt can have been durably applied immediately before the daemon died.
	// Reconstruct its original revision identity, never allocate another revision.
	if op == "review_revision" {
		n, err := strconv.Atoi(i.Arguments[x.config.ReviewPolicies[dispatch.TaskID].ArgumentKey("revision")])
		if err != nil || n < 2 || n > s.Revision+1 {
			return t, i, errors.New("invalid recovered revision")
		}
		s.Revision = n - 1
	}
	bound, want, _, e := x.bindReview(ctx, dispatch, t, s)
	if e != nil {
		return t, i, e
	}
	a, _ := json.Marshal(i)
	b, _ := json.Marshal(want)
	if string(a) != string(b) {
		return t, i, errors.New("review intent changed during recovery")
	}
	return bound, i, nil
}

func (x *liveExecutor) reviewIsLive(task string) bool {
	x.daemon.mu.Lock()
	snapshot, ok := x.daemon.jobs[task]
	x.daemon.mu.Unlock()
	var env envelope
	return ok && decodeEnvelope(snapshot.Payload, &env) == nil && env.Job.Disposition.AllowsExternalEffects()
}
