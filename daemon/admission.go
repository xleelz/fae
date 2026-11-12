package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"
)

// admit is the shared operator/controller/schedule admission boundary. The
// serialized daemon writer must hold mu; SQL idempotency precedes host effects.
func (d *Daemon) admit(ctx context.Context, key string, rawParams json.RawMessage, audit controlplane.Audit) (any, error) {
	// A retried keyed admission returns its original result even while
	// draining (FAE-API-012).
	if original, found, err := d.store.LookupAdmission(ctx, key, rawParams); err != nil {
		return nil, err
	} else if found {
		return admissionResult(original), nil
	}
	if d.draining {
		return nil, errors.New("draining: admissions paused until restart")
	}
	snapshot, err := d.prepareAdmission(ctx, rawParams)
	if err != nil {
		return nil, err
	}
	saved, err := d.writeAdmissionRow(ctx, audit, key, snapshot)
	if err != nil {
		return nil, err
	}
	// Replays return the original response; never rewind the live checkpoint
	// or resurrect a terminal task startup left on the plane.
	if _, exists := d.jobs[snapshot.ID]; !exists && !saved.Replayed() {
		d.jobs[snapshot.ID] = saved
	}
	d.publishStatus()
	d.signal()
	d.notifyEvents()
	return admissionResult(saved), nil
}

// prepareAdmission validates and pins a task without writing admission state.
func (d *Daemon) prepareAdmission(ctx context.Context, rawParams json.RawMessage) (controlplane.JobSnapshot, error) {
	var p AdmitParams
	if err := decode(rawParams, &p); err != nil {
		return controlplane.JobSnapshot{}, err
	}
	var given map[string]json.RawMessage
	if json.Unmarshal(rawParams, &given) == nil {
		_, p.maxSessionsSet = given["max_sessions"]
	}
	if p.ID == "" && p.ParentTask != "" {
		var err error
		p.ID, err = d.nextDetachedID(ctx, p.ParentTask)
		if err != nil {
			return controlplane.JobSnapshot{}, err
		}
	}
	if p.ID == "" {
		return controlplane.JobSnapshot{}, errors.New("task id required")
	}
	if len(p.ID) > 2048 || strings.ContainsAny(p.ID, "/\\\x00\n\r") {
		return controlplane.JobSnapshot{}, errors.New("task id must be a safe hierarchical identity of at most 2048 bytes")
	}

	// A bound task is never bound again (FAE-WF-005): its identity is read,
	// in memory and on the plane, before any write or fork touches its tree.
	if err := d.refuseExistingTask(ctx, p.ID); err != nil {
		return controlplane.JobSnapshot{}, err
	}
	var priority struct {
		Priority *int `json:"priority"`
	}
	if json.Unmarshal(rawParams, &priority) == nil && priority.Priority == nil {
		p.Priority = 10
	}
	if p.Priority < 10 {
		return controlplane.JobSnapshot{}, errors.New("priority must be at least 10")
	}
	if err := validateStructuredFields(p); err != nil {
		return controlplane.JobSnapshot{}, err
	}
	if p.Mode == "" && (p.DryRun || p.CommitExternal) {
		p.Mode = "Normal"
	}
	if p.Mode != "" && p.Mode != "Local" && p.Mode != "Normal" {
		return controlplane.JobSnapshot{}, errors.New("unknown disposition")
	}
	if !SubmissionModeComplete(p.Mode == "Normal", p.DryRun, p.CommitExternal) {
		return controlplane.JobSnapshot{}, errors.New("a Normal submission requires exactly one of dry_run (--dry-run) or commit_external (--commit-external); Local takes neither")
	}
	if p.Mode == "" {
		p.Mode = d.config.DefaultMode
	}
	if p.Mode == "" {
		p.Mode = "Local"
	}
	if p.Mode == "Local" && (p.CommitExternal || p.TicketRoom != "" || p.Destination != "" || p.PushNamespace != "") {
		return controlplane.JobSnapshot{}, errors.New("Local disposition cannot grant external effects")
	}
	if p.Mode == "Normal" && d.live == nil {
		return controlplane.JobSnapshot{}, errors.New("Normal disposition requires live runtime")
	}
	if p.CommitExternal {
		if missing := missingLiveValues(p, false); len(missing) > 0 {
			return controlplane.JobSnapshot{}, missingLiveError(missing)
		}
		if e := d.store.RequireExternalAuthority(ctx, p.AuthorizingMessage); e != nil {
			return controlplane.JobSnapshot{}, e
		}
	}
	if p.RunID == "" {
		p.RunID = d.config.RunID
	}
	if p.WorkflowID == "" {
		p.WorkflowID = defaults.Workflow().ID
	}
	rev, raw, err := d.storedWorkflow(ctx, p.WorkflowID, p.Revision)
	if err != nil {
		return controlplane.JobSnapshot{}, err
	}
	// A retired workflow refuses every new binding; bound tasks keep running.
	if err = d.store.WorkflowActive(ctx, p.WorkflowID); err != nil {
		return controlplane.JobSnapshot{}, err
	}
	p.Revision = rev.Number
	doc, err := d.parseWorkflow(raw)
	if err != nil {
		return controlplane.JobSnapshot{}, err
	}
	treeInfo, err := d.prepareTreeAdmission(ctx, &p, doc)
	if err != nil {
		return controlplane.JobSnapshot{}, err
	}
	if p.Tree == "" {
		cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD^{tree}")
		cmd.Dir = d.config.Workspace
		cmd.Env = cleanGitEnv()
		out, err := cmd.Output()
		if err != nil {
			return controlplane.JobSnapshot{}, fmt.Errorf("resolve workspace tree: %w", err)
		}
		p.Tree = strings.TrimSpace(string(out))
	}
	if err = validateAdmissionReads(doc, p.Reads); err != nil {
		return controlplane.JobSnapshot{}, err
	}
	if err = d.checkSubWorkflows(ctx, doc, nil); err != nil {
		return controlplane.JobSnapshot{}, err
	}
	if p.TargetConfig != "" {
		_, targetRaw, e := d.store.GetTarget(ctx, p.TargetConfig, p.TargetRevision)
		if e != nil {
			return controlplane.JobSnapshot{}, e
		}
		target, e := parseTarget(targetRaw)
		if e != nil {
			return controlplane.JobSnapshot{}, e
		}
		if target.Normal != nil && target.Normal.Adapter != nil {
			if e = d.store.AdapterActive(ctx, target.Normal.Adapter.ID); e != nil {
				return controlplane.JobSnapshot{}, e
			}
		}
	}
	tools, err := d.admissionTools(ctx, p)
	if err != nil {
		return controlplane.JobSnapshot{}, err
	}
	if doc, err = normalContent(doc, p.Mode, tools); err != nil {
		return controlplane.JobSnapshot{}, err
	}
	// Validate versioned policy before any review baseline adapter read.
	if err = d.pinTarget(ctx, &p, doc); err != nil {
		return controlplane.JobSnapshot{}, err
	}
	if _, ok := doc.TaskClasses[p.TaskClass]; p.TaskClass != "" && !ok {
		return controlplane.JobSnapshot{}, errors.New("unknown task class " + p.TaskClass)
	}
	admissionLive := d.live
	if d.live != nil {
		admissionLive, err = d.liveForAdmission(ctx, p)
		if err != nil {
			return controlplane.JobSnapshot{}, err
		}
	}

	if d.live != nil {
		doc, err = reviewDocument(admissionLive.config, p.ID, doc)
		if err != nil {
			return controlplane.JobSnapshot{}, err
		}
		hasReview := false
		for _, stage := range doc.Stages {
			if stage.Primitive == workflow.ExternalEffect && reviewOperation(admissionLive.config.reviewOperation(p.ID, stage.Subject)) {
				hasReview = true
			}
		}
		if hasReview && p.CommitExternal {
			if _, err = admissionLive.checkReviewTargetEvidence(ctx, p.ID); err != nil {
				if ctx.Value(backlogPreparationKey{}) != nil {
					return controlplane.JobSnapshot{}, err
				}
				raw, _ := json.Marshal(map[string]string{"task": p.ID, "defect": err.Error()})
				if save := d.store.PutMarker(ctx, controlplane.Audit{Actor: "daemon", Action: "review target health", Outcome: "blocked", Inputs: raw}, "review-target/"+p.ID, raw); save != nil {
					return controlplane.JobSnapshot{}, save
				}
				return controlplane.JobSnapshot{}, err
			}
		}
	}
	if d.live != nil {
		if e := validateExternalStages(admissionLive.config, doc, p); e != nil {
			return controlplane.JobSnapshot{}, e
		}
	}
	if d.live != nil && d.live.guard.panes != nil {
		if err := validatePaneFloor(doc, d.live.guard.effectivePaneLimit()); err != nil {
			return controlplane.JobSnapshot{}, err
		}
	}
	if p.TargetConfig != "" {
		if err = d.store.TargetActive(ctx, p.TargetConfig); err != nil {
			return controlplane.JobSnapshot{}, err
		}
	}
	if err = d.pinTarget(ctx, &p, doc); err != nil {
		return controlplane.JobSnapshot{}, err
	}
	if err = workflow.CheckDocumentWritesDisjoint(doc, p.DeclaredFiles); err != nil {
		return controlplane.JobSnapshot{}, err
	}
	pinnedRoots, pinnedDenyPaths, err := d.verifyAdmissionTargetRoots(ctx, p)
	if err != nil {
		return controlplane.JobSnapshot{}, err
	}
	if err = d.validateWorkflowIsolation(ctx, p, doc); err != nil {
		return controlplane.JobSnapshot{}, err
	}
	for _, ro := range p.ReadOnlyFiles {
		if idx := workflow.MatchDenyPathIndex(pinnedDenyPaths, ro); idx >= 0 {
			return controlplane.JobSnapshot{}, fmt.Errorf("read_only_files path %q refused by deny_paths[%d]", ro, idx)
		}
	}
	var job engine.Job
	doc.ValidationRecursionCeiling = d.recursionCeiling()
	if d.live != nil {
		// Every task, a carry-forward re-admission included, forks from the
		// current integration tip (FAE-VCS-001); the exact base is recorded
		// durably with the job.
		base, e := admissionLive.integrationTip(ctx)
		if e == nil && treeInfo.Parent != nil {
			visit := treeInfo.Parent.Job
			if internal, ok := ctx.Value(childAdmissionKey{}).(childAdmissionContext); ok && internal.VisitView != nil {
				visit = *internal.VisitView
			}
			base, e = admissionLive.childVisitBase(ctx, visit)
		}
		if e != nil {
			return controlplane.JobSnapshot{}, e
		}
		_, actual, e := admissionLive.forkTask(ctx, p.RunID, p.ID, base, "")
		if e != nil {
			return controlplane.JobSnapshot{}, e
		}
		// A live admission takes its tree from the owned task worktree.
		p.Tree = actual
		job, err = engine.NewLive(p.ID, int(rev.Number), doc, actual)
		job.Base = base
	} else {
		job, err = engine.New(p.ID, int(rev.Number), doc, p.Tree)
	}
	if err == nil {
		job, err = engine.SetTaskClass(job, p.TaskClass)
	}
	if err == nil && p.CarryFrom != "" {
		job, err = d.carryForward(ctx, p, job)
	}
	if err != nil {
		return controlplane.JobSnapshot{}, err
	}
	applyTreeAdmission(&job, p, treeInfo, ctx)
	if err = d.deriveJobFootprint(ctx, &job); err != nil {
		return controlplane.JobSnapshot{}, err
	}
	job, err = engine.BindTaskInputs(job, treeInfo.Inputs, treeInfo.Sources)
	if err != nil {
		return controlplane.JobSnapshot{}, err
	}
	job.History = append(job.History, admissionTreeEvents(job)...)
	job.ReadRoots = pinnedRoots
	job.DenyPaths = pinnedDenyPaths
	job.Reads = p.Reads
	job.DeclaredFiles = slices.Clone(p.DeclaredFiles)
	if p.TargetConfig != "" {
		if pinnedTgt, tErr := d.pinnedTarget(ctx, p); tErr == nil && pinnedTgt != nil {
			docGlobs, _ := effectiveTargetDocumentPolicy(*pinnedTgt)
			if len(docGlobs) > 0 {
				job.DocumentGlobs = slices.Clone(docGlobs)
			}
		}
	}
	job.Disposition = domain.Disposition{Mode: domain.Mode(p.Mode), Live: p.CommitExternal, TicketRoom: p.TicketRoom, Destination: p.Destination, PushNamespace: p.PushNamespace}
	job.FollowUpOf = p.FollowUpOf
	job.TurnBudget = p.TurnBudget
	if len(p.ReadOnlyFiles) > 0 {
		job.ReadOnlyFiles = append([]string(nil), p.ReadOnlyFiles...)
	}
	if d.live != nil {
		if err = configureReviewJob(admissionLive.config, &job); err != nil {
			return controlplane.JobSnapshot{}, err
		}
	}
	if err = d.prepareTaskDirectories(ctx, p, &job); err != nil {
		return controlplane.JobSnapshot{}, err
	}
	payload, _ := json.Marshal(envelope{Job: job, Admission: p, AdmittedAt: time.Now().UTC()})
	snapshot := controlplane.JobSnapshot{ID: p.ID, WorkflowID: p.WorkflowID, WorkflowRevision: rev.Number, TargetID: d.config.Workspace, TargetRevision: 1, State: string(job.State), Payload: payload, Events: admissionEvents(job)}
	if p.TargetConfig != "" {
		snapshot.TargetID = p.TargetConfig
		snapshot.TargetRevision = p.TargetRevision
	}
	return snapshot, nil
}

// refuseExistingTask refuses an identity the drive holds or quarantines, or
// the plane stores in any state. The caller holds d.mu. An Admit RPC read the
// plane before it took the lock (admitReads); every task bound since is in
// the drive's state checked here, or its admission row write returned an
// error and the plane is read again (existingBeforeLock), so the refusal
// still precedes any fork. Cron, backlog and sub-workflow admissions read
// the plane here.
func (d *Daemon) refuseExistingTask(ctx context.Context, id string) error {
	if _, held := d.jobs[id]; !held && !d.quarantineHeld(id) {
		stored, read := d.existingBeforeLock(ctx, id)
		if !read {
			found, err := d.store.ExistingJobs(ctx, []string{id})
			if err != nil {
				return err
			}
			stored = found[id]
		}
		if !stored {
			return nil
		}
	}
	return fmt.Errorf("task %s already exists: a bound task is never bound again; admit under a new identity", id)
}

// admissionWriteBook records, under d.mu, the admission row writes since
// startup (FAE-WF-005, FAE-ROB-021). writes counts every write, committed or
// not, so a read made before the writer lock can tell whether an admission
// may have changed the plane since. outcomeUnknown names the identities a
// write returned an error for: its transaction may have committed with the
// acknowledgement lost, leaving a stored task the drive does not hold, so an
// absence read before the lock is not trusted for them.
type admissionWriteBook struct {
	writes         uint64
	outcomeUnknown map[string]bool
}

// noteAdmissionWrite records one admission row write of ids, which returned
// err. The caller holds d.mu.
func (d *Daemon) noteAdmissionWrite(err error, ids ...string) {
	d.admissionWrites.writes++
	if err == nil {
		return
	}
	if d.admissionWrites.outcomeUnknown == nil {
		d.admissionWrites.outcomeUnknown = map[string]bool{}
	}
	for _, id := range ids {
		d.admissionWrites.outcomeUnknown[id] = true
	}
}

// writeAdmissionRow writes one task's admission row and records the write.
// The caller holds d.mu.
func (d *Daemon) writeAdmissionRow(ctx context.Context, a controlplane.Audit, key string, snapshot controlplane.JobSnapshot) (controlplane.JobSnapshot, error) {
	unlock, lockErr := d.lockOutputOwnership(ctx)
	if lockErr != nil {
		return controlplane.JobSnapshot{}, lockErr
	}
	defer unlock()
	saved, err := d.store.AdmitJob(ctx, a, key, snapshot)
	if err == nil {
		err = d.publishOutputOwners([]controlplane.JobSnapshot{saved})
	}
	if err == nil {
		// Test seam: the row committed and its acknowledgement was lost.
		err = d.faultAt("admit-acknowledgement", snapshot.ID)
	}
	d.noteAdmissionWrite(err, snapshot.ID)
	return saved, err
}

// existingBeforeLock is whether the plane stored id when the RPC read it
// before taking d.mu, and whether that read stands. An absence does not
// stand for an identity whose admission outcome is unknown: the caller,
// which holds d.mu, reads the plane for it again.
func (d *Daemon) existingBeforeLock(ctx context.Context, id string) (stored, read bool) {
	stored, read = readsBefore(ctx).existingTask(id)
	if read && !stored && d.admissionWrites.outcomeUnknown[id] {
		return false, false
	}
	return stored, read
}

// admitReads reads, before Admit takes the writer lock, whether the plane
// stores the task's identity (FAE-ROB-020/-021).
func (d *Daemon) admitReads(ctx context.Context, raw json.RawMessage, r *writerReads) {
	var p AdmitParams
	if decode(raw, &p) != nil || p.ID == "" {
		return
	}
	r.readExisting(ctx, d, []string{p.ID})
}

// missingLiveValues names, one by one, each pinned value a live Normal
// submission lacks (FAE-MODE-004); tickets adds the ticket room.
func missingLiveValues(p AdmitParams, tickets bool) []string {
	missing := []string{}
	if tickets && p.TicketRoom == "" {
		missing = append(missing, "ticket room (--ticket-room)")
	}
	if p.Destination == "" {
		missing = append(missing, "review destination branch (--review-destination-branch)")
	}
	if p.PushNamespace == "" {
		missing = append(missing, "push namespace (--push-namespace)")
	}
	return missing
}
func missingLiveError(missing []string) error {
	return fmt.Errorf("live Normal submission refused: missing %s", strings.Join(missing, "; missing "))
}

// validateStructuredFields checks an item's optional structured fields
// (FAE-CLI-014): unique declared files under the one declared-path rule
// (DeclaredPathSafe, so never in .git at any depth), a compilable bounded
// test filter and a declared test shape.
func validateStructuredFields(p AdmitParams) error {
	if len(p.DeclaredFiles) > 1000 {
		return errors.New("at most 1000 declared files")
	}
	seen := map[string]bool{}
	for _, f := range p.DeclaredFiles {
		if !DeclaredPathSafe(f) || seen[f] {
			return fmt.Errorf("declared file %q must be a unique clean relative path inside the worktree, outside .git", f)
		}
		seen[f] = true
	}
	if len(p.ReadOnlyFiles) > 1000 {
		return errors.New("at most 1000 read-only files")
	}
	seenRO := map[string]bool{}
	for _, f := range p.ReadOnlyFiles {
		if !DeclaredPathSafe(f) || seenRO[f] {
			return fmt.Errorf("read-only file %q must be a unique clean relative path inside the worktree, outside .git", f)
		}
		seenRO[f] = true
	}
	if len(p.TestFilter) > 1024 {
		return errors.New("test filter exceeds 1024 bytes")
	}
	if _, err := regexp.Compile(p.TestFilter); p.TestFilter != "" && err != nil {
		return fmt.Errorf("test filter: %w", err)
	}
	if _, err := controller.CompileTestFilter(p.TestFilter); p.TestFilter != "" && err != nil {
		return err
	}
	if p.TestShape != "" && !slices.Contains(workflow.TestShapes, p.TestShape) && !slices.Contains(LandingTestShapes, p.TestShape) {
		return fmt.Errorf("test shape %q is not one of %v", p.TestShape, workflow.TestShapes)
	}
	if err := validateLandingTestShape(p.TestShape, p.TestFilter, p.DeclaredFiles); err != nil {
		return err
	}
	if err := workflow.ValidateReadPolicy(p.Reads); err != nil {
		return err
	}
	return nil
}
