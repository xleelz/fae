// Package engine implements repository-neutral durable workflow decisions. The
// caller must persist Prepare's returned job before executing its dispatch and
// atomically persist Complete's returned job and events before dispatching again.
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/kernel"
	"github.com/xleelz/fae/workflow"
)

type Event struct {
	Kind       string               `json:"kind"`
	Stage      string               `json:"stage"`
	From       domain.State         `json:"from,omitempty"`
	To         domain.State         `json:"to,omitempty"`
	Attempt    int                  `json:"attempt"`
	Cause      string               `json:"cause"`
	DispatchID string               `json:"dispatch_id,omitempty"`
	Gate       *workflow.GateResult `json:"gate,omitempty"`
}
type Dispatch struct {
	ExecutionHead   string              `json:"execution_head,omitempty"`
	OperatorContext string              `json:"operator_context,omitempty"`
	AttemptCharged  bool                `json:"attempt_charged,omitempty"`
	Base            string              `json:"base,omitempty"`
	ExecutionKind   string              `json:"execution_kind,omitempty"`
	ID              string              `json:"id"`
	TaskID          string              `json:"task_id"`
	Stage           string              `json:"stage"`
	Tree            string              `json:"tree"`
	Attempt         int                 `json:"attempt"`
	Visit           int                 `json:"visit"`
	Config          workflow.Stage      `json:"config"`
	Inputs          []workflow.Evidence `json:"inputs"`
	Sessions        []Session           `json:"sessions"`
	CreatedAt       time.Time           `json:"created_at"`
}
type Session struct {
	Route workflow.Binding `json:"route,omitempty"`
	ID    string           `json:"id"`
	Role  string           `json:"role"`
}
type Result struct {
	Detail        string `json:"detail,omitempty"`
	NewBase       string `json:"new_base,omitempty"`
	ExecutionKind string `json:"execution_kind,omitempty"`
	// NewTree is measured by the trusted live executor after an allowed change turn.
	NewTree    string          `json:"new_tree,omitempty"`
	DispatchID string          `json:"dispatch_id"`
	TaskID     string          `json:"task_id"`
	Tree       string          `json:"tree"`
	Session    string          `json:"session"`
	Output     json.RawMessage `json:"output"`
	Reviews    []ReviewResult  `json:"reviews,omitempty"`
	Class      string          `json:"class,omitempty"`
	ProducedAt time.Time       `json:"produced_at"`
}
type ReviewResult struct {
	Session string          `json:"session"`
	Role    string          `json:"role"`
	Output  json.RawMessage `json:"output"`
}
type Executor interface {
	Execute(context.Context, Dispatch) (Result, error)
}
type Provenance struct {
	ExecutionKind    string             `json:"execution_kind"`
	ArtifactID       string             `json:"artifact_id"`
	DispatchID       string             `json:"dispatch_id"`
	TaskID           string             `json:"task_id"`
	Stage            string             `json:"stage"`
	Primitive        workflow.Primitive `json:"primitive"`
	Role             string             `json:"role"`
	Session          string             `json:"session"`
	Tree             string             `json:"tree"`
	Inputs           []string           `json:"inputs"`
	ProducedAt       time.Time          `json:"produced_at"`
	WorkflowHash     string             `json:"workflow_hash"`
	WorkflowRevision int                `json:"workflow_revision"`
	Provider         string             `json:"provider,omitempty"`
	Model            string             `json:"model,omitempty"`
}
type Job struct {
	// ExecutionHead is broker-proved branch HEAD; Base remains the reconciliation origin.
	ExecutionHead     string         `json:"execution_head,omitempty"`
	AttemptCapStage   string         `json:"attempt_cap_stage,omitempty"`
	NewAttempt        bool           `json:"new_attempt,omitempty"`
	BlockedFrom       string         `json:"blocked_from,omitempty"`
	TaskAttemptLimit  int            `json:"task_attempt_limit,omitempty"`
	ContextEntries    []ContextEntry `json:"context_entries,omitempty"`
	Deferred          bool           `json:"deferred,omitempty"`
	DeferredUntil     time.Time      `json:"deferred_until,omitempty"`
	BlockedRoute      string         `json:"blocked_route,omitempty"`
	OperatorStage     string         `json:"operator_stage,omitempty"`
	OperatorReason    string         `json:"operator_reason,omitempty"`
	ReviewPollCount   int            `json:"review_poll_count,omitempty"`
	ReviewPollSeconds int            `json:"review_poll_seconds,omitempty"`
	ReviewMaxPolls    int            `json:"review_max_polls,omitempty"`

	ReviewRevisionStage string            `json:"review_revision_stage,omitempty"`
	ReviewStages        map[string]string `json:"review_stages,omitempty"`
	ReviewMergeRequired bool              `json:"review_merge_required,omitempty"`
	ReviewMerged        bool              `json:"review_merged,omitempty"`
	ReviewDisabled      bool              `json:"review_disabled,omitempty"`
	ReviewID            string            `json:"review_id,omitempty"`
	ReviewRevision      int               `json:"review_revision,omitempty"`
	ReviewMaxRevisions  int               `json:"review_max_revisions,omitempty"`
	ReviewRemediation   string            `json:"review_remediation,omitempty"`

	Disposition         domain.Disposition `json:"disposition,omitempty"`
	AdmissionRetryAfter time.Time          `json:"admission_retry_after,omitempty"`
	Base                string             `json:"base,omitempty"`
	// ExecutionKind pins the executor trust boundary for the job lifetime.
	ExecutionKind      string              `json:"execution_kind"`
	TaskID             string              `json:"task_id"`
	Revision           int                 `json:"revision"`
	Document           workflow.Document   `json:"document"`
	WorkflowHash       string              `json:"workflow_hash"`
	State              domain.State        `json:"state"`
	Tree               string              `json:"tree"`
	Attempts           int                 `json:"attempts"`
	TestRuns           int                 `json:"test_runs"`
	Counts             map[string]int      `json:"counts"`
	Failures           map[string]int      `json:"failures"`
	Sequence           int                 `json:"sequence"`
	ResolvedDispatches []Dispatch          `json:"resolved_dispatches,omitempty"`
	InFlight           *Dispatch           `json:"in_flight,omitempty"`
	Evidence           []workflow.Evidence `json:"evidence"`
	Provenance         []Provenance        `json:"provenance"`
	History            []Event             `json:"history"`
	Reason             string              `json:"reason,omitempty"`
	// Parked is a derived wait reason; State remains the durable stage.
	Parked bool `json:"parked"`
}

func New(taskID string, revision int, doc workflow.Document, tree string) (Job, error) {
	j := Job{ExecutionKind: "scripted", TaskID: taskID, Revision: revision, Document: doc, State: domain.Pending, Tree: tree, Counts: map[string]int{}, Failures: map[string]int{}}
	if taskID == "" || revision < 1 || tree == "" {
		return j, errors.New("task, revision and tree required")
	}
	if issues := workflow.Validate(doc); len(issues) > 0 {
		return j, fmt.Errorf("invalid workflow: %v", issues)
	}
	var err error
	j.WorkflowHash, err = workflow.Hash(doc)
	return clone(j), err
}

// NewLive creates a job for the trusted live executor. Scripted records cannot
// be relabeled: every artifact and dispatch must retain its execution kind.
func NewLive(taskID string, revision int, doc workflow.Document, tree string) (Job, error) {
	j, err := New(taskID, revision, doc, tree)
	if err == nil {
		j.ExecutionKind = "live"
	}
	return j, err
}
func clone(j Job) Job {
	b, _ := json.Marshal(j)
	var out Job
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	_ = dec.Decode(&out)
	return out
}

// Validate checks immutable workflow binding and stored artifact identities. It
// does not authenticate arbitrary JSON: snapshots must come from the trusted store.
func Validate(j Job) error {
	if j.TaskAttemptLimit < 0 {
		return errors.New("invalid task attempt limit")
	}
	for _, entry := range j.ContextEntries {
		if entry.SHA256 != ContextHash(entry.Text) {
			return errors.New("invalid task context hash")
		}
	}
	if j.OperatorStage != "" && (j.InFlight == nil || j.OperatorStage != j.InFlight.Stage || (j.State != domain.Blocked && j.State != domain.Escalated) || j.OperatorReason == "") {
		return errors.New("invalid operator hold")
	}
	if j.Disposition.Mode != "" && j.Disposition.Mode != domain.Local && j.Disposition.Mode != domain.Normal {
		return errors.New("invalid job disposition")
	}
	if j.Disposition.Live && (j.Disposition.Mode != domain.Normal || j.Disposition.Destination == "" || j.Disposition.PushNamespace == "") {
		return errors.New("invalid live external disposition")
	}
	if j.Disposition.Mode == domain.Normal && j.ExecutionKind != "live" {
		return errors.New("Normal disposition requires trusted runtime")
	}

	if j.ReviewPollCount < 0 || j.ReviewMaxPolls < 0 || j.ReviewMaxPolls > 3600 || j.ReviewPollSeconds < 0 || j.ReviewPollSeconds > 3600 {
		return errors.New("invalid review poll bounds")
	}
	if j.ExecutionKind != "scripted" && j.ExecutionKind != "live" {
		return errors.New("unsupported or missing execution kind")
	}
	h, e := workflow.Hash(j.Document)
	if e != nil || h != j.WorkflowHash {
		return errors.New("workflow binding hash mismatch")
	}
	if j.TaskID == "" || j.Revision < 1 || j.Tree == "" || j.Counts == nil || j.Failures == nil {
		return errors.New("incomplete job")
	}
	if j.Attempts < 0 || j.TestRuns < 0 || j.Sequence < 0 {
		return errors.New("negative counters")
	}
	for _, counts := range []map[string]int{j.Counts, j.Failures} {
		for _, n := range counts {
			if n < 0 {
				return errors.New("negative stage counter")
			}
		}
	}
	for _, a := range j.Evidence {
		stage, exists := j.Document.Stages[a.Stage]
		if !exists || stage.Output != a.Type {
			return errors.New("artifact does not match producer stage output")
		}
		b, e := json.Marshal(a.Fields)
		if e != nil {
			return e
		}
		id, e := domain.ContentHash(b)
		if e != nil || a.ID != id || a.TaskID != j.TaskID || !a.Provenance {
			return errors.New("invalid stored evidence identity")
		}
		typ, ok := j.Document.Artifacts[a.Type]
		if !ok || typ.Version != a.Version {
			return errors.New("unknown stored artifact type")
		}
		if e = workflow.ValidateArtifact(typ.Schema, b); e != nil {
			return e
		}
		found := false
		for _, p := range j.Provenance {
			if p.ArtifactID == a.ID && p.TaskID == a.TaskID && p.Stage == a.Stage && p.Tree == a.Tree && p.Session == a.Session && p.Role == a.Role && p.DispatchID != "" && p.Primitive == stage.Primitive && !p.ProducedAt.IsZero() && p.ExecutionKind == j.ExecutionKind {
				found = true
			}
		}
		if !found {
			return errors.New("missing stored evidence provenance")
		}
	}
	if j.ExecutionHead != "" && j.ExecutionHead != j.Base {
		known := j.LatestCommitHead() == j.ExecutionHead
		if !known {
			return errors.New("execution HEAD lacks latest broker commit provenance")
		}
	}
	if j.InFlight != nil && j.InFlight.ExecutionHead != "" && j.InFlight.ExecutionHead != j.ExpectedHead() {
		return errors.New("in-flight execution HEAD mismatch")
	}
	if j.InFlight != nil && (j.InFlight.TaskID != j.TaskID || (j.InFlight.Stage != string(j.State) && j.InFlight.Stage != j.OperatorStage) || j.InFlight.Tree != j.Tree || j.InFlight.Base != j.Base) {
		return errors.New("invalid in-flight dispatch")
	}
	if d := j.InFlight; d != nil {
		if d.ExecutionKind != j.ExecutionKind && !(j.ExecutionKind == "scripted" && d.ExecutionKind == "") {
			return errors.New("dispatch execution kind mismatch")
		}
		stored, _ := json.Marshal(d.Config)
		declared, _ := json.Marshal(j.Document.Stages[d.Stage])
		if !bytes.Equal(stored, declared) || len(d.Sessions) == 0 || d.ID == "" || d.CreatedAt.IsZero() {
			return errors.New("invalid dispatch configuration")
		}
		if d.ID != fmt.Sprintf("%s/%s/%d", j.TaskID, j.WorkflowHash, j.Sequence) || d.Attempt != j.Attempts || d.Attempt < 1 || d.Visit != j.Counts[d.Stage] || d.Visit < 1 {
			return errors.New("invalid dispatch identity or counters")
		}
		roles := []string{d.Config.Role}
		count := 1
		if d.Config.Primitive == workflow.Review {
			if len(d.Config.Reviewers) > 0 {
				roles = d.Config.Reviewers
			}
			if d.Config.Count > 0 {
				count = d.Config.Count
			}
		}
		if len(d.Sessions) != len(roles)*count {
			return errors.New("invalid dispatch session count")
		}
		for i, session := range d.Sessions {
			if session.Route.Provider != "" {
				declared := false
				for _, route := range j.Document.Roles[session.Role].Routes {
					if route == session.Route {
						declared = true
					}
				}
				if !declared {
					return errors.New("dispatch route not declared for role")
				}
			}
			if session.ID != fmt.Sprintf("%s/session/%d", d.ID, i) || session.Role != roles[i/count] {
				return errors.New("invalid dispatch session identity")
			}
		}
	}
	return nil
}
func event(j *Job, e Event, events *[]Event) {
	j.History = append(j.History, e)
	*events = append(*events, e)
}
func move(j *Job, to, cause string, events *[]Event) error {
	if to == string(domain.Done) && j.ReviewMergeRequired && !j.ReviewMerged {
		stop(j, "ReviewMergeRequired", events)
		return nil
	}

	if to == string(domain.Done) && j.Disposition.Mode == domain.Normal && !j.Disposition.Live {
		to = string(domain.Escalated)
		cause = "DryRunComplete"
	}
	if !workflow.CanTransition(j.Document, j.State, domain.State(to)) {
		return fmt.Errorf("illegal transition %s -> %s", j.State, to)
	}
	attempt := j.Attempts
	if attempt < 1 {
		attempt = 1
	}
	e := Event{Kind: "transition", Stage: string(j.State), From: j.State, To: domain.State(to), Attempt: attempt, Cause: cause}
	event(j, e, events)
	j.State = domain.State(to)
	j.Reason = ""
	if cause == "DryRunComplete" {
		j.Reason = cause
	}
	j.Parked = false
	return nil
}
func stop(j *Job, reason string, events *[]Event) {
	if reason == "AttemptCap" {
		j.AttemptCapStage = string(j.State)
	}
	_ = move(j, string(domain.Escalated), reason, events)
	j.Reason = reason
	j.InFlight = nil
}
func route(j Job, s workflow.Stage) (string, error) {
	next := ""
	for _, e := range j.Document.Edges {
		if e.From == string(j.State) && !(j.ReviewStages[string(j.State)] != "" && e.To == s.Failure) && (s.Primitive != workflow.Gate || e.To != s.Failure) && e.To != s.Reverify {
			if next != "" {
				return "", errors.New("ambiguous success route")
			}
			next = e.To
		}
	}
	if next == "" {
		return "", errors.New("missing success route")
	}
	return next, nil
}
func current(j Job) []workflow.Evidence {
	latest := map[string]string{}
	for _, p := range j.Provenance {
		if p.Tree == j.Tree {
			latest[p.Stage] = p.DispatchID
		}
	}
	out := []workflow.Evidence{}
	for _, a := range j.Evidence {
		if a.Tree != j.Tree {
			continue
		}
		for _, p := range j.Provenance {
			if p.ArtifactID == a.ID && p.Stage == a.Stage && p.Session == a.Session && p.DispatchID == latest[a.Stage] {
				out = append(out, a)
				break
			}
		}
	}
	return out
}

// Informational inputs and frozen identities survive tree changes. Gates and
// protected effects continue to use current-tree evidence exclusively.
func latestInputs(j Job) []workflow.Evidence {
	if j.ExecutionKind != "live" {
		return current(j)
	}
	latest := map[string]string{}
	for _, p := range j.Provenance {
		latest[p.Stage] = p.DispatchID
	}
	var out []workflow.Evidence
	for _, a := range j.Evidence {
		for _, p := range j.Provenance {
			if p.ArtifactID == a.ID && p.Stage == a.Stage && p.Session == a.Session && p.DispatchID == latest[a.Stage] {
				out = append(out, a)
				break
			}
		}
	}
	return out
}
func boundInputs(j Job, s workflow.Stage) []workflow.Evidence {
	out := []workflow.Evidence{}
	seen := map[string]bool{}
	for _, a := range latestInputs(j) {
		for _, in := range s.Inputs {
			if a.Type == in.Type && a.Version == in.Version && (in.Stage == "" || a.Stage == in.Stage) {
				key := a.ID + "/" + a.Session
				if !seen[key] {
					out = append(out, a)
					seen[key] = true
				}
				break
			}
		}
	}
	return out
}

// Prepare makes one durable decision. An existing in-flight dispatch is never
// silently re-issued: after an unplanned restart the caller must park or recover
// that token explicitly. A completed stage is never executed again.
func Prepare(input Job) (Job, *Dispatch, []Event, error) { return PrepareWithRoutes(input, nil) }

// PrepareWithRoutes pins selected bindings before launch. A nil selector retains
// the first declared binding for scripted and legacy callers.
func PrepareWithRoutes(input Job, selectRoute func([]workflow.Binding) (workflow.Binding, bool)) (Job, *Dispatch, []Event, error) {
	j := clone(input)
	events := []Event{}
	if e := Validate(j); e != nil {
		return input, nil, nil, e
	}
	if j.Deferred {
		if j.DeferredUntil.IsZero() || time.Now().Before(j.DeferredUntil) {
			return j, nil, events, nil
		}
		j.Deferred = false
		j.DeferredUntil = time.Time{}
		event(&j, Event{Kind: "undefer", Cause: "defer deadline elapsed"}, &events)
	}
	if j.Parked && j.Reason == "infrastructure-deferred" && time.Now().Before(j.AdmissionRetryAfter) {
		return j, nil, events, nil
	}
	if selectRoute != nil && j.InFlight == nil && j.Parked && (j.Reason == "all-routes-unavailable" || j.Reason == "infrastructure-deferred") {
		j.Parked = false
		j.Reason = ""
	}
	if j.Parked && j.Reason == "ReviewPending" && !time.Now().Before(j.AdmissionRetryAfter) {
		j.Parked = false
		j.Reason = ""
	}
	if j.State.Terminal() || j.State == domain.Escalated || j.InFlight != nil || j.Parked {
		return j, nil, events, nil
	}
	if j.State == domain.Pending {
		if e := move(&j, j.Document.Start, "admitted", &events); e != nil {
			return input, nil, nil, e
		}
		return j, nil, events, nil
	}
	s, ok := j.Document.Stages[string(j.State)]
	if !ok {
		return input, nil, nil, errors.New("unknown durable stage")
	}
	if j.ReviewStages[string(j.State)] == "review_create" && j.ReviewID != "" {
		if j.ReviewRevisionStage == "" || j.ReviewRevision >= j.ReviewMaxRevisions {
			stop(&j, "ReviewRevisionCap: same-review revision route unavailable or exhausted", &events)
			return j, nil, events, nil
		}
		if e := move(&j, j.ReviewRevisionStage, "existing review requires revision", &events); e != nil {
			return input, nil, nil, e
		}
		return j, nil, events, nil
	}
	if missing := workflow.CheckInputs(s, j.TaskID, latestInputs(j)); len(missing) > 0 {
		j.Parked = true
		j.Reason = fmt.Sprintf("MissingInput: %v", missing)
		event(&j, Event{Kind: "park", Stage: string(j.State), Attempt: max(1, j.Attempts), Cause: j.Reason}, &events)
		return j, nil, events, nil
	}
	if s.Primitive == workflow.Escalate {
		stop(&j, "HumanDecisionRequired", &events)
		return j, nil, events, nil
	}
	if s.Primitive == workflow.ExternalEffect && (j.ExecutionKind != "live" || j.Disposition.Mode != domain.Normal) {
		stop(&j, "UnsupportedExternalEffect", &events)
		return j, nil, events, nil
	}
	if s.Primitive == workflow.HumanDecision {
		j.Parked = true
		j.Reason = "HumanDecisionRequired"
		event(&j, Event{Kind: "park", Stage: string(j.State), Attempt: max(1, j.Attempts), Cause: j.Reason}, &events)
		return j, nil, events, nil
	}
	var next string
	var err error
	switch s.Primitive {
	case workflow.Gate:
		g := workflow.Eval(*s.Expr, current(j))
		event(&j, Event{Kind: "gate", Stage: string(j.State), Attempt: max(1, j.Attempts), Cause: "evaluated", Gate: &g}, &events)
		if !g.Passed {
			next = s.Failure
		} else {
			next, err = route(j, s)
		}
	case workflow.Branch:
		next = s.Default
		for _, arm := range s.Cases {
			g := workflow.Eval(arm.When, current(j))
			if g.Value == workflow.Unknown {
				stop(&j, "UnknownDecision", &events)
				return j, nil, events, nil
			}
			if g.Passed {
				next = arm.Next
				break
			}
		}
	case workflow.Loop:
		if j.Counts[string(j.State)] >= s.MaxIterations {
			stop(&j, "LoopCap", &events)
			return j, nil, events, nil
		}
		j.Counts[string(j.State)]++
		next, err = route(j, s)
	default:
		selected := map[string]workflow.Binding{}
		if s.Primitive == workflow.AgentTurn || s.Primitive == workflow.Review {
			roles := []string{s.Role}
			if s.Primitive == workflow.Review && len(s.Reviewers) > 0 {
				roles = s.Reviewers
			}
			for _, name := range roles {
				routes := j.Document.Roles[name].Routes
				var route workflow.Binding
				ok := len(routes) > 0
				if ok {
					route = routes[0]
				}
				if selectRoute != nil {
					route, ok = selectRoute(routes)
				}
				if !ok {
					j.Parked = true
					j.Reason = "all-routes-unavailable"
					if !input.Parked || input.Reason != j.Reason {
						event(&j, Event{Kind: "park", Stage: string(j.State), Attempt: j.Attempts, Cause: j.Reason}, &events)
					}
					return j, nil, events, nil
				}
				selected[name] = route
			}
		}
		attemptBoundary := j.NewAttempt || j.Attempts == 0 || j.Counts[string(j.State)] > 0
		// Readiness observations are not new implementation attempts. A
		// separate durable observation budget bounds their waiting loop.
		operation := j.ReviewStages[string(j.State)]
		if j.Attempts > 0 && (operation == "review_read" || operation == "review_publish" || operation == "review_merge") {
			attemptBoundary = false
		}
		if attemptBoundary && j.Attempts >= j.AttemptLimit() {
			stop(&j, "AttemptCap", &events)
			return j, nil, events, nil
		}
		if s.TestRun && j.TestRuns >= j.Document.Limits.TestRuns {
			stop(&j, "TestRunCap", &events)
			return j, nil, events, nil
		}
		if s.Primitive == workflow.VcsCommit || s.Primitive == workflow.Integrate || s.Primitive == workflow.ExternalEffect {
			if e := protected(j); e != nil {
				stop(&j, "ProtectedEvidence: "+e.Error(), &events)
				return j, nil, events, nil
			}
		}
		// One initial attempt covers first visits to the graph. Each repeated
		// execution consumes another attempt; Sequence counts every dispatch.
		if attemptBoundary {
			j.NewAttempt = false
			j.Attempts++
		}
		j.Sequence++
		j.Counts[string(j.State)]++
		if s.TestRun {
			j.TestRuns++
		}
		d := Dispatch{ExecutionHead: j.ExpectedHead(), OperatorContext: j.LaterContext(), AttemptCharged: attemptBoundary, Base: j.Base, ExecutionKind: j.ExecutionKind, ID: fmt.Sprintf("%s/%s/%d", j.TaskID, j.WorkflowHash, j.Sequence), TaskID: j.TaskID, Stage: string(j.State), Tree: j.Tree, Attempt: j.Attempts, Visit: j.Counts[string(j.State)], Config: s, Inputs: boundInputs(j, s), CreatedAt: time.Now().UTC()}
		roles := []string{s.Role}
		if s.Primitive == workflow.Review && len(s.Reviewers) > 0 {
			roles = s.Reviewers
		}
		count := 1
		if s.Primitive == workflow.Review && s.Count > 0 {
			count = s.Count
		}
		for _, r := range roles {
			for i := 0; i < count; i++ {
				d.Sessions = append(d.Sessions, Session{ID: fmt.Sprintf("%s/session/%d", d.ID, len(d.Sessions)), Role: r, Route: selected[r]})
			}
		}
		j.InFlight = &d
		event(&j, Event{Kind: "dispatch", Stage: d.Stage, Attempt: d.Attempt, Cause: "inputs-resolved", DispatchID: d.ID}, &events)
		return j, &d, events, nil
	}
	if err != nil {
		return input, nil, nil, err
	}
	if err = move(&j, next, "stage-complete", &events); err != nil {
		return input, nil, nil, err
	}
	return j, nil, events, nil
}
func protected(j Job) error {
	var runs, reviews, changes []workflow.Evidence
	order := map[string]int{}
	for i, event := range j.History {
		if event.Kind == "dispatch" {
			order[event.DispatchID] = i + 1
		}
	}
	position := func(a workflow.Evidence) int {
		n := 0
		for _, p := range j.Provenance {
			if p.Stage == a.Stage && p.ArtifactID == a.ID && p.Session == a.Session && order[p.DispatchID] > n {
				n = order[p.DispatchID]
			}
		}
		return n
	}
	lastChange := 0
	for _, a := range latestInputs(j) {
		if j.Document.Stages[a.Stage].ChangeProducer {
			lastChange = max(lastChange, position(a))
			changes = append(changes, a)
		}
	}
	for _, e := range j.History {
		if e.Kind == "tree-reconciled" || e.Kind == "integration-target-moved" {
			lastChange = max(lastChange, order[e.DispatchID])
		}
	}
	for _, a := range current(j) {
		s := j.Document.Stages[a.Stage]
		if s.Primitive == workflow.HostCommand && s.TestRun && position(a) > lastChange {
			runs = append(runs, a)
		}
		if s.Primitive == workflow.Review && position(a) > lastChange {
			reviews = append(reviews, a)
		}
	}
	if e := workflow.CheckProtectedEvidence(j.Tree, runs, reviews, changes); e != nil {
		return e
	}
	// A passing gate must actually cite the current-tree run used as evidence.
	for i := len(j.History) - 1; i >= 0; i-- {
		e := j.History[i]
		if e.Gate == nil || !e.Gate.Passed {
			continue
		}
		stage := j.Document.Stages[e.Stage]
		if stage.TestStage == "" {
			continue
		}
		for _, r := range runs {
			if r.Stage != stage.TestStage {
				continue
			}
			for _, id := range e.Gate.EvidenceIDs {
				if r.ID == id {
					return nil
				}
			}
		}
	}
	return errors.New("no recorded passing test gate")
}

// Complete only accepts the currently persisted dispatch and its assigned
// sessions. Envelope provenance is created by the executor, never role JSON.
func complete(input Job, dispatchID string, r Result) (Job, []Event, error) {
	j := clone(input)
	events := []Event{}
	if e := Validate(j); e != nil {
		return input, nil, e
	}
	match := j.InFlight != nil && j.InFlight.ID == dispatchID && r.DispatchID == dispatchID
	validEnvelope := j.InFlight != nil && len(j.InFlight.Sessions) > 0 && r.TaskID == j.TaskID && r.Tree == j.InFlight.Tree && !r.ProducedAt.Before(j.InFlight.CreatedAt) && r.Session == j.InFlight.Sessions[0].ID
	if !kernel.CanComplete(j.State.Terminal() || j.State == domain.Escalated, j.InFlight != nil, match, validEnvelope) {
		return input, nil, errors.New("stale or terminal completion")
	}
	d := *j.InFlight
	if r.TaskID != j.TaskID || r.Tree != d.Tree || r.ProducedAt.Before(d.CreatedAt) || r.Session != d.Sessions[0].ID {
		return input, nil, errors.New("completion provenance mismatch")
	}
	if r.Detail != "" {
		detail := r.Detail
		if len(detail) > 2048 {
			detail = detail[:2048]
		}
		event(&j, Event{Kind: "execution-diagnostic", Stage: d.Stage, DispatchID: d.ID, Attempt: d.Attempt, Cause: detail}, &events)
	}
	if r.Class == "uncertain" || r.Class == "interrupted" {
		j.Parked = true
		j.Reason = "UncertainDispatch"
		event(&j, Event{Kind: "park", Stage: d.Stage, Attempt: d.Attempt, Cause: j.Reason, DispatchID: d.ID}, &events)
		return j, events, nil
	}
	if r.Class == "startup-retry" {
		return completeStartupRetry(input, j, d, r, events)
	}
	if r.Class == "stall-retry" {
		if j.ExecutionKind != "live" || r.ExecutionKind != "live" || (d.Config.Primitive != workflow.AgentTurn && d.Config.Primitive != workflow.Review) || r.NewTree != "" || r.NewBase != "" {
			return input, nil, errors.New("invalid stall recovery")
		}
		j.Failures["stall-recovery"]++
		j.InFlight = nil
		if j.Failures["stall-recovery"] > 2 {
			stop(&j, "StallExhausted", &events)
			return j, events, nil
		}
		event(&j, Event{Kind: "stall-retry", Stage: d.Stage, DispatchID: d.ID, Attempt: d.Attempt, Cause: "retired trusted bridge; unchanged tree"}, &events)
		return j, events, nil
	}
	if r.Class == "target-moved" {
		if j.ExecutionKind != "live" || r.ExecutionKind != "live" || d.Config.Primitive != workflow.Integrate || d.Config.Reverify == "" || r.NewTree != "" || r.NewBase != "" {
			return input, nil, errors.New("integration retry outside pinned live bounded route")
		}
		j.InFlight = nil
		event(&j, Event{Kind: "integration-target-moved", Stage: d.Stage, DispatchID: d.ID, Attempt: d.Attempt, Cause: "known no-write refusal; full reconciliation required"}, &events)
		if err := move(&j, d.Config.Reverify, "integration target moved", &events); err != nil {
			return input, nil, err
		}
		return j, events, nil
	}
	if r.Class == "infrastructure-deferred" && j.ExecutionKind == "live" {
		if d.AttemptCharged {
			j.Attempts--
		}
		j.Counts[d.Stage]--
		if d.Config.TestRun {
			j.TestRuns--
		}
		j.InFlight = nil
		j.Parked = true
		j.Reason = "infrastructure-deferred"
		j.AdmissionRetryAfter = time.Now().Add(5 * time.Second)
		event(&j, Event{Kind: "park", Stage: d.Stage, Attempt: j.Attempts, Cause: j.Reason, DispatchID: d.ID}, &events)
		return j, events, nil
	}
	if (r.Class == "review-wait" || r.Class == "review-gate-failed") && j.ExecutionKind == "live" && j.ReviewStages[d.Stage] != "" {
		if r.Class == "review-gate-failed" {
			stop(&j, "ReviewGateFailed: "+r.Detail, &events)
			return j, events, nil
		}
		parkReviewPoll(&j, d, r.Detail, &events)
		return j, events, nil
	}
	if r.Class != "" && r.Class != "success" {
		return failed(j, "ExecutionFailure: "+r.Class)
	}
	// A completed execution ends this startup sequence; later stage visits get a fresh bound.
	delete(j.Failures, "startup-recovery:"+d.Stage)
	s := d.Config
	if j.ExecutionKind == "live" && r.ExecutionKind != "live" {
		return input, nil, errors.New("live completion kind required")
	}
	if j.ExecutionKind == "scripted" && (r.ExecutionKind == "live" || r.NewTree != "") {
		return input, nil, errors.New("scripted completion cannot claim live tree")
	}
	if r.NewBase != "" && (j.ExecutionKind != "live" || s.Primitive != workflow.Merge || s.Reverify == "") {
		return input, nil, errors.New("base mutation outside reconciliation")
	}
	resultTree := d.Tree
	if r.NewTree != "" {
		if j.ExecutionKind != "live" || !((s.Primitive == workflow.AgentTurn && s.ChangeProducer) || (s.Primitive == workflow.Merge && s.Reverify != "")) {
			return input, nil, errors.New("tree mutation outside change-producing agent turn")
		}
		resultTree = r.NewTree
	}
	// A successful merge proves only the reviewed tree. Any subsequent source
	// edit or base reconciliation invalidates that proof before Done can close
	// the task (and its ticket) using stale delivery evidence.
	if resultTree != j.Tree {
		j.ReviewMerged = false
	}
	outputs := []ReviewResult{{Session: r.Session, Role: s.Role, Output: r.Output}}
	if s.Primitive == workflow.Review {
		if len(r.Reviews) != len(d.Sessions) {
			return failed(j, "MalformedOutput: review count")
		}
		outputs = r.Reviews
		for i, o := range outputs {
			if o.Session != d.Sessions[i].ID || o.Role != d.Sessions[i].Role {
				return input, nil, errors.New("review session mismatch")
			}
		}
	}
	// Validate all outputs before recording any, so partial reviewer collections
	// cannot survive as an aggregate approval.
	records := []workflow.Evidence{}
	for _, o := range outputs {
		if s.Output == "" {
			return failed(j, "MalformedOutput: stage has no output type")
		}
		typ := j.Document.Artifacts[s.Output]
		if e := workflow.ValidateArtifact(typ.Schema, o.Output); e != nil {
			return failed(j, "MalformedOutput: "+e.Error())
		}
		var fields map[string]any
		dec := json.NewDecoder(bytes.NewReader(o.Output))
		dec.UseNumber()
		if e := dec.Decode(&fields); e != nil || fields == nil {
			return failed(j, "MalformedOutput: object required")
		}
		if s.Primitive == workflow.HostCommand && s.TestRun {
			if fields["fresh_build"] != true {
				return failed(j, "MalformedOutput: fresh build required")
			}
		}
		id, e := domain.ContentHash(o.Output)
		if e != nil {
			return failed(j, "MalformedOutput: "+e.Error())
		}
		records = append(records, workflow.Evidence{ID: id, TaskID: j.TaskID, Type: s.Output, Version: typ.Version, Stage: d.Stage, Tree: resultTree, Role: o.Role, Session: o.Session, Provenance: true, Fields: fields})
	}

	if operation := j.ReviewStages[d.Stage]; operation != "" && j.Disposition.Live {
		if len(records) != 1 {
			return failed(j, "ReviewGateFailed: missing lifecycle receipt")
		}
		f := records[0].Fields
		if operation != "review_read" {
			resetReviewPoll(&j)
		}
		if operation == "review_create" || operation == "review_revision" || operation == "review_merge" {
			id, _ := f["review_id"].(string)
			if id == "" || (j.ReviewID != "" && id != j.ReviewID) {
				return failed(j, "ReviewGateFailed: changed review identity")
			}
			j.ReviewID = id
		}
		if operation == "review_create" {
			j.ReviewRevision = 1
		}
		if operation == "review_revision" {
			j.ReviewRevision++
			j.ReviewMerged = false
		}
		if operation == "review_merge" && !j.ReviewDisabled {
			if f["review_merged"] != true || f["reconciliation_state"] != "reconciled" {
				return failed(j, "ReviewGateFailed: merge receipt absent")
			}
			j.ReviewMerged = true
			event(&j, Event{Kind: "review-merged", Stage: d.Stage, DispatchID: d.ID, Cause: j.ReviewID, Attempt: d.Attempt}, &events)
		}
	}
	for _, a := range records {
		j.Evidence = append(j.Evidence, a)
		p := Provenance{ArtifactID: a.ID, DispatchID: d.ID, TaskID: j.TaskID, Stage: d.Stage, Primitive: s.Primitive, Role: a.Role, Session: a.Session, Tree: resultTree, ProducedAt: r.ProducedAt}
		p.ExecutionKind = j.ExecutionKind
		p.WorkflowHash = j.WorkflowHash
		p.WorkflowRevision = j.Revision
		if role, ok := j.Document.Roles[a.Role]; ok && len(role.Routes) > 0 {
			p.Provider = role.Routes[0].Provider
			p.Model = role.Routes[0].Model
			for _, session := range d.Sessions {
				if session.ID == a.Session && session.Route.Provider != "" {
					p.Provider = session.Route.Provider
					p.Model = session.Route.Model
				}
			}
		}
		for _, in := range d.Inputs {
			p.Inputs = append(p.Inputs, in.ID)
		}
		j.Provenance = append(j.Provenance, p)
	}
	if s.Primitive == workflow.Merge && (resultTree != d.Tree || (r.NewBase != "" && r.NewBase != d.Base)) {
		event(&j, Event{Kind: "tree-reconciled", Stage: d.Stage, DispatchID: d.ID, Cause: "current-tree tests and reviews required", Attempt: d.Attempt}, &events)
	}
	j.Tree = resultTree
	if r.NewBase != "" {
		j.Base = r.NewBase
		j.ExecutionHead = r.NewBase
	}
	if j.ExecutionKind == "live" && s.Primitive == workflow.VcsCommit && len(records) == 1 {
		if sha, ok := records[0].Fields["sha"].(string); ok && sha != "" {
			j.ExecutionHead = sha
		}
	}
	j.InFlight = nil
	next, e := route(j, s)
	if j.Disposition.Live && j.ReviewStages[d.Stage] == "review_read" && len(records) == 1 {
		decision, _ := records[0].Fields["review_decision"].(string)
		if decision == "remediation" {
			if j.ReviewRevision >= j.ReviewMaxRevisions || j.ReviewRemediation == "" {
				stop(&j, "ReviewRevisionCap: remediation unavailable or exhausted", &events)
				return j, events, nil
			}
			commit, _ := records[0].Fields["review_commit"].(string)
			if commit == "" {
				stop(&j, "ReviewGateFailed: remediation commit proof missing", &events)
				return j, events, nil
			}
			j.Base = commit
			j.ExecutionHead = commit
			next = j.ReviewRemediation
			event(&j, Event{Kind: "review-remediation", Stage: d.Stage, DispatchID: d.ID, Cause: j.ReviewID, Attempt: d.Attempt}, &events)
		} else if decision != "ready" {
			parkReviewPoll(&j, d, "review status pending", &events)
			return j, events, nil
		}
		resetReviewPoll(&j)
	}
	if s.Primitive == workflow.Merge && (resultTree != d.Tree || (r.NewBase != "" && r.NewBase != d.Base)) {
		// Conflicted marker trees must enter implementation remediation, not
		// the clean revalidation route. The typed report follows normal routing.
		conflicted := len(records) == 1 && records[0].Fields["result"] == "conflicted"
		if !conflicted {
			next = s.Reverify
		}
	}
	if e != nil {
		return input, nil, e
	}
	if e = move(&j, next, "completed "+d.ID, &events); e != nil {
		return input, nil, e
	}
	return j, events, nil
}
func failed(j Job, reason string) (Job, []Event, error) {
	events := []Event{}
	stage := string(j.State)
	j.Failures[stage]++
	j.InFlight = nil
	j.Reason = reason
	event(&j, Event{Kind: "failure", Stage: stage, Attempt: j.Attempts, Cause: reason}, &events)
	if j.Failures[stage] >= 3 {
		stop(&j, reason, &events)
	}
	return j, events, nil
}

// Recover distinguishes an interrupted safe scripted run from unknown execution.
// Only the explicit safeReplay capability permits reissuing the original token.
func Recover(input Job, graceful, safeReplay bool) (Job, *Dispatch, error) {
	j := clone(input)
	if e := Validate(j); e != nil {
		return input, nil, e
	}
	if j.InFlight == nil {
		return j, nil, nil
	}
	if !safeReplay || j.ExecutionKind == "live" || j.OperatorStage != "" {
		if !graceful && !(j.Parked && j.Reason == "UncertainDispatch") {
			j.Failures["crash-recovery"]++
			events := []Event{}
			event(&j, Event{Kind: "crash-recovery", Stage: j.InFlight.Stage, DispatchID: j.InFlight.ID, Cause: "execution outcome unknown; explicit resolution required"}, &events)
		}
		j.Parked = true
		j.Reason = "UncertainDispatch"
		return j, nil, nil
	}
	if !graceful {
		j.Failures["dispatch-recovery"]++
		if j.Failures["dispatch-recovery"] > 2 {
			events := []Event{}
			stop(&j, "DispatchRecoveryCap", &events)
			return j, nil, nil
		}
	}
	j.Parked = false
	j.Reason = ""
	return j, j.InFlight, nil
}

// ExpectedHead preserves compatibility for jobs admitted before execution-head tracking.
func (j Job) ExpectedHead() string {
	if j.ExecutionHead != "" {
		return j.ExecutionHead
	}
	return j.Base
}

// LatestCommitHead returns the latest broker commit receipt. Validate verifies
// evidence identity and provenance before callers may use this for recovery.
func (j Job) LatestCommitHead() string {
	for i := len(j.Provenance) - 1; i >= 0; i-- {
		p := j.Provenance[i]
		if p.Primitive != workflow.VcsCommit {
			continue
		}
		for _, a := range j.Evidence {
			if a.ID == p.ArtifactID && a.Session == p.Session {
				sha, _ := a.Fields["sha"].(string)
				return sha
			}
		}
		return ""
	}
	return ""
}
