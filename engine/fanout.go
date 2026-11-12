package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
)

// Branch statuses of a fan-out visit.
const (
	BranchPending   = "pending"
	BranchRunning   = "running"
	BranchPassed    = "passed"
	BranchFailed    = "failed"
	BranchSkipped   = "skipped"
	BranchCancelled = "cancelled"
)

// FanOutValidationError is a typed runtime refusal when a FanOut's dynamic
// branch count or branch write scopes violate workflow constraints (FAE-WF-096).
type FanOutValidationError struct {
	Code   string
	Stage  string
	Reason string
}

func (e *FanOutValidationError) Error() string {
	return fmt.Sprintf("fan-out %s validation failed: %s", e.Stage, e.Reason)
}

// FanState is the durable branch and join state of one FanOut visit.
type FanState struct {
	IntegratedTree string        `json:"integrated_tree,omitempty"`
	Baseline       *Job          `json:"baseline,omitempty"`
	Stage          string        `json:"stage"`
	Join           string        `json:"join"`
	Visit          int           `json:"visit"`
	Parallel       bool          `json:"parallel,omitempty"`
	Dynamic        bool          `json:"dynamic,omitempty"`
	ActiveBranchID string        `json:"active_branch_id,omitempty"`
	Branches       []BranchState `json:"branches"`
}

// FanBranchState is an alias for BranchState (FAE-WF-096).
type FanBranchState = BranchState

type BranchState struct {
	FailureReason string         `json:"failure_reason,omitempty"`
	Frame         *Job           `json:"frame,omitempty"`
	Error         string         `json:"error,omitempty"`
	Output        string         `json:"output,omitempty"`
	ID            string         `json:"id"`
	Start         string         `json:"start"`
	Status        string         `json:"status"`
	Stage         string         `json:"stage,omitempty"`
	Pass          *workflow.Expr `json:"pass,omitempty"`
	Item          any            `json:"item,omitempty"`
	WriteScope    []string       `json:"write_scope,omitempty"`
	InFlight      *Dispatch      `json:"in_flight,omitempty"`
	BaseTree      string         `json:"base_tree,omitempty"`
	Tree          string         `json:"tree,omitempty"`
	CommitSHA     string         `json:"commit_sha,omitempty"`
	BranchRef     string         `json:"branch_ref,omitempty"`
}

func validateFanState(j Job) error {
	f := j.Fan
	if f == nil {
		return nil
	}
	s, ok := j.Document.Stages[f.Stage]
	if !ok || s.Primitive != workflow.FanOut || s.Join != f.Join || f.Visit < 1 {
		return errors.New("invalid fan-out state")
	}
	if s.BranchesFrom == nil {
		if len(s.Branches) != len(f.Branches) {
			return errors.New("invalid fan-out state")
		}
	} else {
		maxB := s.BranchesFrom.Max
		if maxB <= 0 {
			maxB = 256
		}
		if len(f.Branches) == 0 || len(f.Branches) < s.BranchesFrom.Min || len(f.Branches) > maxB || len(f.Branches) > 256 {
			return errors.New("invalid fan-out state")
		}
	}
	tmpls := workflow.EffectiveBranches(s)
	running := 0
	for i, b := range f.Branches {
		if s.BranchesFrom == nil {
			if b.ID != s.Branches[i].ID || b.Start != s.Branches[i].Start {
				return errors.New("fan-out state differs from its pinned branches")
			}
		} else {
			if len(tmpls) != 1 || b.ID != fmt.Sprintf("branch-%d", i) || b.Start != tmpls[0].Start {
				return errors.New("fan-out state differs from its pinned branches")
			}
		}
		switch b.Status {
		case BranchRunning:
			running++
		case BranchPending, BranchPassed, BranchFailed, BranchSkipped, BranchCancelled:
		default:
			return errors.New("invalid fan-out branch status")
		}
	}
	if !s.Parallel && !f.Parallel && running > 1 {
		return errors.New("more than one running branch")
	}
	return nil
}

// running is the branch currently executing, if any.
func (f *FanState) running() int {
	if f == nil {
		return -1
	}
	if f.ActiveBranchID != "" {
		for i, b := range f.Branches {
			if b.ID == f.ActiveBranchID {
				return i
			}
		}
	}
	for i, b := range f.Branches {
		if b.Status == BranchRunning {
			return i
		}
	}
	return -1
}

// activeBranch returns the active BranchState and its index, if any.
func (f *FanState) activeBranch() (int, *BranchState) {
	i := f.running()
	if i < 0 || i >= len(f.Branches) {
		return -1, nil
	}
	return i, &f.Branches[i]
}

// inFan reports whether the task's stage belongs to its active fan-out.
func inFan(j Job) bool {
	if j.Fan == nil {
		return false
	}
	_, member := workflow.BranchOf(j.Document, j.Fan.Stage)[string(j.State)]
	return member || string(j.State) == j.Fan.Join || string(j.State) == j.Fan.Stage
}

func branchItemEvidence(j Job, b BranchState) []workflow.Evidence {
	if j.Fan == nil || b.Item == nil {
		return nil
	}
	fields := map[string]any{"item": b.Item}
	if m, ok := b.Item.(map[string]any); ok {
		for k, v := range m {
			if _, exists := fields[k]; !exists {
				fields[k] = v
			}
		}
	}
	raw, _ := json.Marshal(fields)
	id, _ := domain.ContentHash(raw)
	if id == "" {
		id = fmt.Sprintf("fan-item-%s-%s", j.Fan.Stage, b.ID)
	}
	s := j.Document.Stages[j.Fan.Stage]
	outType := s.Output
	if outType == "" {
		outType = "FanItem"
	}
	ver := 1
	if a, ok := j.Document.Artifacts[outType]; ok && a.Version > 0 {
		ver = a.Version
	}
	res := []workflow.Evidence{{
		ID:         id,
		TaskID:     j.TaskID,
		Type:       outType,
		Version:    ver,
		Stage:      j.Fan.Stage,
		Tree:       j.Tree,
		Provenance: true,
		Fields:     fields,
	}}
	// Also expose against any input of the current stage that binds from the FanOut stage or branches_from stage.
	curStage, ok := j.Document.Stages[string(j.State)]
	if ok {
		for _, in := range curStage.Inputs {
			if (in.Stage == j.Fan.Stage || (s.BranchesFrom != nil && in.Stage == s.BranchesFrom.Stage) || in.Stage == "") && (in.Type != outType || in.Version != ver || in.Stage != j.Fan.Stage) {
				st := in.Stage
				if st == "" {
					st = j.Fan.Stage
				}
				res = append(res, workflow.Evidence{
					ID:         id + "-" + in.Name,
					TaskID:     j.TaskID,
					Type:       in.Type,
					Version:    in.Version,
					Stage:      st,
					Tree:       j.Tree,
					Provenance: true,
					Fields:     fields,
				})
			}
		}
	}
	return res
}

// fanVisible hides a sibling branch's evidence from the running branch, so
// no branch (in particular no reviewer) sees another's output before the
// join (FAE-WF-033). After the join every branch's evidence is visible.
func fanVisible(j Job, evidence []workflow.Evidence) []workflow.Evidence {
	if j.Fan == nil {
		return evidence
	}
	i := j.Fan.running()
	if i < 0 {
		return evidence
	}
	activeID := j.Fan.Branches[i].ID
	owner := workflow.BranchOf(j.Document, j.Fan.Stage)
	provBranch := map[string]string{}
	for _, p := range j.Provenance {
		if p.BranchID != "" {
			provBranch[p.ArtifactID+"/"+p.Session] = p.BranchID
		}
	}
	out := []workflow.Evidence{}
	for _, a := range evidence {
		if pb := provBranch[a.ID+"/"+a.Session]; pb != "" && pb != activeID {
			continue
		}
		if !j.Fan.Dynamic {
			if o, ok := owner[a.Stage]; ok && o != activeID {
				continue
			}
		}
		out = append(out, a)
	}
	out = append(out, branchItemEvidence(j, j.Fan.Branches[i])...)
	return out
}

// fanArrive records the running branch's outcome as it reaches the join: a
// branch that arrives by a failed check's failure route, or whose declared
// pass verdict (a recorded gate evaluation) is not true, has failed.
func fanArrive(j *Job, to string, events *[]Event) {
	f := j.Fan
	if f == nil || to != f.Join {
		return
	}
	i := f.running()
	if i < 0 {
		return
	}
	fanArriveBranch(j, i, string(j.State), to, events)
}

func fanArriveBranch(j *Job, i int, fromStage, to string, events *[]Event) {
	f := j.Fan
	if f == nil || i < 0 || i >= len(f.Branches) || to != f.Join {
		return
	}
	from := j.Document.Stages[fromStage]
	status := BranchPassed
	if workflow.FailureRouted(from) && from.Failure == to {
		status = BranchFailed
	}
	var pass *workflow.Expr
	if f.Branches[i].Pass != nil {
		pass = f.Branches[i].Pass
	} else if s := j.Document.Stages[f.Stage]; i < len(s.Branches) && s.Branches[i].Pass != nil {
		pass = s.Branches[i].Pass
	} else if tmpls := workflow.EffectiveBranches(j.Document.Stages[f.Stage]); len(tmpls) == 1 && tmpls[0].Pass != nil {
		pass = tmpls[0].Pass
	}
	if pass != nil {
		prevActive := f.ActiveBranchID
		f.ActiveBranchID = f.Branches[i].ID
		g := record(j, workflow.RecordBranch, f.Join, f.Branches[i].ID, 0, *pass, current(*j), "branch-verdict", events)
		f.ActiveBranchID = prevActive
		if !g.Passed {
			status = BranchFailed
		}
	}
	f.Branches[i].Status = status
	if status == BranchFailed {
		f.Branches[i].Error = failureText(j.Reason)
		if f.Branches[i].Error == "" {
			f.Branches[i].Error = "branch failed at " + fromStage
		}
	}
	f.Branches[i].Stage = to
	f.Branches[i].InFlight = nil
	event(j, Event{Kind: "branch-joined", Stage: fromStage, Attempt: j.stagedAttempt(), Cause: f.Branches[i].ID + " " + status}, events)
}

func lookupNestedField(fields map[string]any, field string) (any, bool) {
	if fields == nil {
		return nil, false
	}
	if field == "" {
		if v, ok := fields["items"]; ok {
			return v, true
		}
		var single any
		count := 0
		for _, v := range fields {
			if _, ok := v.([]any); ok {
				single = v
				count++
			}
		}
		return single, count == 1
	}
	if v, ok := fields[field]; ok {
		return v, true
	}
	cur := any(fields)
	for _, part := range strings.Split(field, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func fanHasChangeProducer(d workflow.Document, fanID string) bool {
	for _, region := range workflow.BranchOf(d, fanID) {
		_ = region
	}
	for stageID := range workflow.BranchOf(d, fanID) {
		if workflow.ProducesChanges(d, d.Stages[stageID]) {
			return true
		}
	}
	return false
}

func verifyFanWriteScopes(fanStage string, branches []BranchState, declaredFiles, approvedScope []string, requireScope bool) error {
	allowedTaskFiles := append(append([]string(nil), declaredFiles...), approvedScope...)
	for i, b := range branches {
		if requireScope && len(b.WriteScope) == 0 {
			return &FanOutValidationError{Stage: fanStage, Reason: fmt.Sprintf("branch %s has no write_scope", b.ID)}
		}
		for _, g := range b.WriteScope {
			if !workflow.ValidWriteGlob(g) {
				return &FanOutValidationError{Stage: fanStage, Reason: fmt.Sprintf("branch %s has invalid write_scope %q", b.ID, g)}
			}
			if len(allowedTaskFiles) > 0 && !workflow.MatchesAnyGlob(g, allowedTaskFiles) {
				return &FanOutValidationError{Stage: fanStage, Reason: fmt.Sprintf("branch %s write_scope %q is outside task declared files", b.ID, g)}
			}
		}
		for k := 0; k < i; k++ {
			for _, g1 := range branches[k].WriteScope {
				for _, g2 := range b.WriteScope {
					if workflow.GlobsOverlap(g1, g2) {
						return &FanOutValidationError{Stage: fanStage, Reason: fmt.Sprintf("branch %s write_scope %q overlaps branch %s write_scope %q", b.ID, g2, branches[k].ID, g1)}
					}
				}
			}
		}
	}
	return nil
}

func expandFanBranches(j Job, fanStage string, s workflow.Stage) ([]BranchState, bool, error) {
	requireScope := fanHasChangeProducer(j.Document, fanStage)
	if s.BranchesFrom == nil {
		eff := workflow.EffectiveBranches(s)
		branches := make([]BranchState, 0, len(eff))
		for _, b := range eff {
			branches = append(branches, BranchState{
				ID:         b.ID,
				Start:      b.Start,
				Status:     BranchPending,
				Pass:       b.Pass,
				WriteScope: append([]string(nil), b.WriteScope...),
				BaseTree:   j.Tree,
			})
		}
		if requireScope || len(s.WriteScope) > 0 {
			if err := verifyFanWriteScopes(fanStage, branches, j.DeclaredFiles, j.ApprovedScope, requireScope); err != nil {
				return nil, false, err
			}
		}
		return branches, false, nil
	}
	bf := s.BranchesFrom
	if bf.Projection != nil {
		value, _, err := ResolveProjection(j, *bf.Projection)
		if err != nil {
			code := "width"
			if bf.Projection.Object != nil || bf.Projection.Source == "item" || strings.Contains(err.Error(), "filter") || strings.Contains(err.Error(), "bound exceeded") {
				code = "binding"
			}
			return nil, false, &FanOutValidationError{Code: code, Stage: fanStage, Reason: code + ": " + err.Error()}
		}
		copy := j
		fields := map[string]any{"items": value}
		raw, _ := json.Marshal(fields)
		id, _ := domain.ContentHash(raw)
		copy.Evidence = append(copy.Evidence, workflow.Evidence{ID: id, TaskID: j.TaskID, Stage: "projection-source", Type: "ProjectionSource", Version: 1, Tree: j.Tree, Provenance: true, Fields: fields})
		copy.Provenance = append(copy.Provenance, Provenance{ArtifactID: id, Stage: "projection-source", TaskID: j.TaskID, Tree: j.Tree, DispatchID: "projection", Session: "", ExecutionKind: j.ExecutionKind})
		next := s
		cloned := *bf
		cloned.Projection = nil
		cloned.Stage = "projection-source"
		cloned.Type = ""
		cloned.Input = ""
		cloned.Field = "items"
		next.BranchesFrom = &cloned
		return expandFanBranches(copy, fanStage, next)
	}
	var source *workflow.Evidence
	selectedStage := bf.Stage
	if bf.Input != "" {
		selectedStage = "input:" + bf.Input
	}
	for _, a := range latestInputs(j) {
		if (selectedStage == "" || a.Stage == selectedStage) && (bf.Type == "" || a.Type == bf.Type) {
			aCopy := a
			source = &aCopy
		}
	}
	if source == nil {
		for i := len(j.Evidence) - 1; i >= 0; i-- {
			if (selectedStage == "" || j.Evidence[i].Stage == selectedStage) && (bf.Type == "" || j.Evidence[i].Type == bf.Type) {
				aCopy := j.Evidence[i]
				source = &aCopy
				break
			}
		}
	}
	if source == nil {
		return nil, false, &FanOutValidationError{Stage: fanStage, Reason: fmt.Sprintf("missing output from branches_from stage %s", bf.Stage)}
	}
	rawItems, ok := lookupNestedField(source.Fields, bf.Field)
	if !ok {
		return nil, false, &FanOutValidationError{Stage: fanStage, Reason: fmt.Sprintf("missing branches_from field %q in stage %s", bf.Field, bf.Stage)}
	}
	var items []any
	switch v := rawItems.(type) {
	case []any:
		items = v
	case []string:
		for _, s := range v {
			items = append(items, s)
		}
	case []map[string]any:
		for _, m := range v {
			items = append(items, m)
		}
	default:
		return nil, false, &FanOutValidationError{Stage: fanStage, Reason: fmt.Sprintf("branches_from field %q in stage %s is not an array", bf.Field, bf.Stage)}
	}
	count := len(items)
	maxB := bf.Max
	if maxB <= 0 {
		maxB = 256
	}
	if count == 0 && bf.Min == 0 {
		return nil, true, nil
	}
	if count < bf.Min || count > maxB || count > 256 {
		return nil, false, &FanOutValidationError{Stage: fanStage, Reason: fmt.Sprintf("branches_from count %d is outside bounds [%d, %d]", count, bf.Min, maxB)}
	}
	tmpls := workflow.EffectiveBranches(s)
	if len(tmpls) != 1 {
		return nil, false, &FanOutValidationError{Stage: fanStage, Reason: "branches_from requires a single branch template"}
	}
	tmpl := tmpls[0]
	branches := make([]BranchState, 0, count)
	for i, item := range items {
		ws, err := workflow.ExpandBranchWriteScope(tmpl.WriteScope, item)
		if err != nil {
			return nil, false, &FanOutValidationError{Stage: fanStage, Reason: err.Error()}
		}
		branches = append(branches, BranchState{
			ID:         fmt.Sprintf("branch-%d", i),
			Start:      tmpl.Start,
			Status:     BranchPending,
			Pass:       tmpl.Pass,
			Item:       item,
			WriteScope: ws,
			BaseTree:   j.Tree,
		})
	}
	if requireScope || len(tmpl.WriteScope) > 0 {
		if err := verifyFanWriteScopes(fanStage, branches, j.DeclaredFiles, j.ApprovedScope, requireScope); err != nil {
			return nil, false, err
		}
	}
	return branches, false, nil
}

// fanStep runs a FanOut stage (start the visit and its first branch) or a
// FanIn stage (enter the next branch, or record the join verdict and route).
func fanStep(j *Job, s workflow.Stage, events *[]Event) error {
	stage := string(j.State)
	if s.Primitive == workflow.FanOut {
		j.Counts[stage]++
		visit := j.Counts[stage]
		branches, emptyRoute, err := expandFanBranches(*j, stage, s)
		if err != nil {
			return err
		}
		if s.BranchesFrom != nil && s.BranchesFrom.Projection != nil {
			if err := recordProjection(j, stage, "branches_from", *s.BranchesFrom.Projection, events); err != nil {
				return err
			}
		}
		if emptyRoute {
			if err := recordJoinArtifact(j, j.Document.Stages[s.Join], &FanState{Stage: stage, Join: s.Join, Visit: visit, Branches: []BranchState{}}, events); err != nil {
				return err
			}
			event(j, Event{Kind: "fan-out", Stage: stage, Attempt: j.stagedAttempt(), Cause: fmt.Sprintf("visit %d: 0 branches -> %s", visit, s.BranchesFrom.Empty)}, events)
			return move(j, s.BranchesFrom.Empty, "fan-out empty", events)
		}
		f := &FanState{
			Stage:    stage,
			Join:     s.Join,
			Visit:    visit,
			Parallel: s.Parallel,
			Dynamic:  s.BranchesFrom != nil,
			Branches: branches,
		}
		if true { // All new fan instances checkpoint isolated branch execution views.
			base := clone(*j)
			base.Fan = nil
			base.LastFan = nil
			base.InFlight = nil
			base.Child = nil
			f.Baseline = &base
		}
		for i := range f.Branches {
			if s.BranchesFrom == nil && i < len(s.Branches) {
				f.Branches[i].Output = s.Branches[i].Output
			} else if s.BranchTemplate != nil {
				f.Branches[i].Output = s.BranchTemplate.Output
			}
		}
		f.Branches[0].Status = BranchRunning
		f.Branches[0].Stage = f.Branches[0].Start
		j.Fan = f
		event(j, Event{Kind: "fan-out", Stage: stage, Attempt: j.stagedAttempt(), Cause: fmt.Sprintf("visit %d: %d branches joined at %s", f.Visit, len(f.Branches), s.Join)}, events)
		if f.Baseline != nil {
			return nil
		}
		return move(j, f.Branches[0].Start, "fan-branch "+f.Branches[0].ID, events)
	}
	f := j.Fan
	if f == nil || f.Join != stage {
		return fmt.Errorf("join %s reached without its fan-out", stage)
	}
	passed, failed := 0, 0
	for i := range f.Branches {
		if f.Branches[i].Status == BranchRunning {
			// In sequential mode, reaching the join without an engine route
			// means the branch did not pass. In parallel mode, if other
			// branches are still running and the join isn't settled yet, don't
			// clobber running sibling branches.
			if !f.Parallel && !s.Parallel {
				f.Branches[i].Status = BranchFailed
				event(j, Event{Kind: "branch-joined", Stage: stage, Attempt: j.stagedAttempt(), Cause: f.Branches[i].ID + " " + BranchFailed}, events)
			}
		}
		switch f.Branches[i].Status {
		case BranchPassed:
			passed++
		case BranchFailed:
			failed++
		}
	}
	verdict, decided := workflow.JoinVerdict(s.Rule, s.Quorum, passed, failed, len(f.Branches))
	if !decided {
		for i := range f.Branches {
			if f.Branches[i].Status == BranchPending {
				f.Branches[i].Status = BranchRunning
				f.Branches[i].Stage = f.Branches[i].Start
				f.ActiveBranchID = f.Branches[i].ID
				return move(j, f.Branches[i].Start, "fan-branch "+f.Branches[i].ID, events)
			}
		}
		if f.Parallel || s.Parallel {
			for i := range f.Branches {
				if f.Branches[i].Status == BranchRunning && f.Branches[i].Stage != stage && f.Branches[i].Stage != "" {
					f.ActiveBranchID = f.Branches[i].ID
					return enter(j, domain.State(f.Branches[i].Stage))
				}
			}
		}
		return errors.New("undecided join without a pending branch")
	}
	g := workflow.GateResult{Value: workflow.False, Passed: verdict, EvidenceIDs: []string{}, Leaves: []workflow.Leaf{}}
	if verdict {
		g.Value = workflow.True
	}
	for i := range f.Branches {
		if f.Branches[i].Status == BranchPending || f.Branches[i].Status == BranchRunning {
			// The rule is settled: the remaining branches are cancelled unrun.
			f.Branches[i].Status = BranchSkipped
			var cancelled []*Dispatch
			if f.Branches[i].Frame != nil {
				cancelled = BranchDispatches(*f.Branches[i].Frame)
			} else if f.Branches[i].InFlight != nil {
				cancelled = []*Dispatch{f.Branches[i].InFlight}
			}
			for _, d := range cancelled {
				f.Branches[i].Status = BranchCancelled
				event(j, Event{Kind: "branch-cancelled", Stage: stage, DispatchID: d.ID, Attempt: j.stagedAttempt(), Cause: d.BranchID}, events)
			}
			f.Branches[i].InFlight = nil
			event(j, Event{Kind: "branch-skipped", Stage: stage, Attempt: j.stagedAttempt(), Cause: f.Branches[i].ID}, events)
		}
		g.Leaves = append(g.Leaves, workflow.Leaf{Op: "branch", Path: []string{f.Branches[i].ID}, Value: f.Branches[i].Status})
	}
	cause := fmt.Sprintf("%s: %d passed, %d failed of %d", s.Rule, passed, failed, len(f.Branches))
	if !verdict {
		for _, b := range f.Branches {
			if b.Status == BranchFailed {
				cause += "; first failed branch " + b.ID + ": " + b.Error
				break
			}
		}
	}
	event(j, Event{Kind: "join", Stage: stage, Attempt: j.stagedAttempt(), Cause: cause, Gate: &g}, events)
	if err := recordJoinArtifact(j, s, f, events); err != nil {
		return err
	}
	j.LastFan = f
	j.Fan = nil
	if !verdict {
		return routeFailure(j, "gate", cause, "", events)
	}
	next := s.Failure
	if verdict {
		var err error
		if next, err = route(*j, s); err != nil {
			return err
		}
	}
	return move(j, next, "stage-complete", events)
}

// PrepareBranch prepares the next step or dispatch for a specific branch of an
// active (or starting) FanOut stage, allowing Parallel: true fans to have
// multiple branches in flight concurrently (FAE-WF-096).
func PrepareBranch(input Job, branchID string) (Job, *Dispatch, []Event, error) {
	return PrepareBranchWith(input, branchID, Options{})
}

// PrepareBranchWith is PrepareBranch with explicit route selection options.
func PrepareBranchWith(input Job, branchID string, o Options) (Job, *Dispatch, []Event, error) {
	if input.Fan != nil && input.Fan.Baseline != nil {
		j, ds, ev, err := prepareNestedFan(input, o, 1, branchID)
		if len(ds) > 0 {
			return j, ds[0], ev, err
		}
		return j, nil, ev, err
	}
	j := clone(input)
	events := []Event{}
	if err := Validate(j); err != nil {
		return input, nil, nil, err
	}
	if j.State == domain.Pending {
		var err error
		j, _, events, err = PrepareWith(j, o)
		if err != nil || j.Fan == nil && j.Document.Stages[string(j.State)].Primitive != workflow.FanOut {
			return j, nil, events, err
		}
	}
	if j.Fan == nil {
		s, ok := j.Document.Stages[string(j.State)]
		if !ok || s.Primitive != workflow.FanOut {
			return input, nil, nil, fmt.Errorf("job is not in a FanOut stage (state=%s)", j.State)
		}
		var stepEvents []Event
		if err := fanStep(&j, s, &stepEvents); err != nil {
			return input, nil, nil, err
		}
		events = append(events, stepEvents...)
		if j.Fan == nil {
			// Empty route taken.
			return j, nil, events, nil
		}
	}
	idx := -1
	for i, b := range j.Fan.Branches {
		if b.ID == branchID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return input, nil, nil, fmt.Errorf("unknown fan-out branch %q", branchID)
	}
	b := &j.Fan.Branches[idx]
	if b.Status == BranchPassed || b.Status == BranchFailed || b.Status == BranchSkipped {
		return j, nil, events, nil
	}
	if b.InFlight != nil {
		return j, b.InFlight, events, nil
	}
	if b.Status == BranchPending {
		if !j.Fan.Parallel && j.Fan.running() >= 0 && j.Fan.running() != idx {
			return input, nil, nil, errors.New("more than one running branch")
		}
		b.Status = BranchRunning
		b.Stage = b.Start
		event(&j, Event{Kind: "transition", Stage: j.Fan.Stage, From: domain.State(j.Fan.Stage), To: domain.State(b.Start), Attempt: j.stagedAttempt(), Cause: "fan-branch " + b.ID}, &events)
	}
	if b.Stage == "" {
		b.Stage = b.Start
	}
	for {
		b = &j.Fan.Branches[idx]
		if b.Status != BranchRunning || b.Stage == j.Fan.Join {
			break
		}
		savedState := j.State
		savedInFlight := j.InFlight
		if err := enter(&j, domain.State(b.Stage)); err != nil {
			return input, nil, nil, err
		}
		j.InFlight = nil
		j.Fan.ActiveBranchID = b.ID
		nextJ, d, stepEvents, err := PrepareWith(j, o)
		if err != nil {
			return input, nil, nil, err
		}
		events = append(events, stepEvents...)
		j = nextJ
		if j.Fan == nil || j.State == domain.Escalated || j.State.Terminal() {
			return j, nil, events, nil
		}
		b = &j.Fan.Branches[idx]
		if d != nil {
			branchIndex := idx
			d.BranchIndex = &branchIndex
			d.BranchID = b.ID
			d.FanStage = j.Fan.Stage
			d.BranchItem = b.Item
			d.WriteScope = append([]string(nil), b.WriteScope...)
			b.InFlight = d
			b.Stage = d.Stage
			j.InFlight = d
			_ = savedState
			_ = savedInFlight
			return j, d, events, nil
		}
		b.Stage = string(j.State)
		if b.Stage == j.Fan.Join || b.Status != BranchRunning {
			break
		}
	}
	// If join is now ready and no other branch is running, evaluate FanIn.
	if j.Fan != nil && j.State == domain.State(j.Fan.Join) {
		runningOther := false
		for i, br := range j.Fan.Branches {
			if i != idx && br.Status == BranchRunning {
				runningOther = true
				break
			}
		}
		joinStage := j.Document.Stages[j.Fan.Join]
		passed, failed := 0, 0
		for _, br := range j.Fan.Branches {
			if br.Status == BranchPassed {
				passed++
			} else if br.Status == BranchFailed {
				failed++
			}
		}
		_, decided := workflow.JoinVerdict(joinStage.Rule, joinStage.Quorum, passed, failed, len(j.Fan.Branches))
		if decided || !runningOther {
			var joinEvents []Event
			if err := fanStep(&j, joinStage, &joinEvents); err != nil {
				return input, nil, nil, err
			}
			events = append(events, joinEvents...)
		}
	}
	if j.Fan != nil && j.InFlight == nil {
		for _, br := range j.Fan.Branches {
			if br.InFlight != nil {
				j.InFlight = br.InFlight
				if br.InFlight.Tree != "" {
					j.Tree = br.InFlight.Tree
				}
				if err := enter(&j, domain.State(br.InFlight.Stage)); err != nil {
					return input, nil, nil, err
				}
				break
			}
		}
	}
	return j, nil, events, nil
}

// CompleteBranch completes an in-flight dispatch of a specific FanOut branch
// and advances the branch (and FanIn when settled) (FAE-WF-096).
func CompleteBranch(input Job, branchID string, dispatchID string, r Result) (Job, []Event, error) {
	if input.OperatorStage != "" {
		return Complete(input, dispatchID, r)
	}
	if input.Fan != nil && input.Fan.Baseline != nil {
		return completeNestedFan(input, branchID, dispatchID, r)
	}
	j := clone(input)
	if err := Validate(j); err != nil {
		return input, nil, err
	}
	if j.Fan == nil {
		return input, nil, errors.New("no active fan-out")
	}
	idx := -1
	for i, b := range j.Fan.Branches {
		if b.ID == branchID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return input, nil, fmt.Errorf("unknown fan-out branch %q", branchID)
	}
	b := j.Fan.Branches[idx]
	targetDispatch := b.InFlight
	if targetDispatch == nil && j.InFlight != nil && (j.InFlight.BranchID == branchID || j.InFlight.BranchID == "") {
		targetDispatch = j.InFlight
	}
	if targetDispatch == nil || targetDispatch.ID != dispatchID {
		return input, nil, errors.New("stale or mismatched branch dispatch")
	}
	var otherInFlight *Dispatch
	for i, br := range j.Fan.Branches {
		if i != idx && br.InFlight != nil {
			otherInFlight = br.InFlight
			break
		}
	}
	j.InFlight = targetDispatch
	if err := enter(&j, domain.State(targetDispatch.Stage)); err != nil {
		return input, nil, err
	}
	j.Fan.ActiveBranchID = branchID
	if b.Tree != "" && r.Tree == b.Tree {
		j.Tree = b.Tree
	}
	nextJ, events, err := complete(j, dispatchID, r)
	if err != nil {
		return input, nil, err
	}
	j = nextJ
	if j.Fan == nil {
		return j, events, nil
	}
	if j.Fan.Branches[idx].BaseTree == "" {
		if r.Tree != "" {
			j.Fan.Branches[idx].BaseTree = r.Tree
		} else {
			j.Fan.Branches[idx].BaseTree = input.Tree
		}
	}
	if r.NewTree != "" {
		j.Fan.Branches[idx].Tree = r.NewTree
	}
	if r.BranchCommit != "" {
		j.Fan.Branches[idx].CommitSHA = r.BranchCommit
	}
	if r.BranchRef != "" {
		j.Fan.Branches[idx].BranchRef = r.BranchRef
	}
	j.Fan.Branches[idx].InFlight = nil
	if j.Fan.Branches[idx].Status == BranchRunning {
		j.Fan.Branches[idx].Stage = string(j.State)
	}
	// Check if FanIn can now settle before restoring another branch's in-flight pointer.
	joinStage := j.Document.Stages[j.Fan.Join]
	passed, failed, running := 0, 0, 0
	for _, br := range j.Fan.Branches {
		switch br.Status {
		case BranchPassed:
			passed++
		case BranchFailed:
			failed++
		case BranchRunning:
			running++
		}
	}
	_, decided := workflow.JoinVerdict(joinStage.Rule, joinStage.Quorum, passed, failed, len(j.Fan.Branches))
	if decided || (j.State == domain.State(j.Fan.Join) && running == 0) {
		j.InFlight = nil
		if err := enter(&j, domain.State(j.Fan.Join)); err != nil {
			return input, nil, err
		}
		var joinEvents []Event
		if err := fanStep(&j, joinStage, &joinEvents); err != nil {
			return input, nil, err
		}
		events = append(events, joinEvents...)
		return j, events, nil
	}
	if j.InFlight == nil && otherInFlight != nil {
		j.InFlight = otherInFlight
		if otherInFlight.Tree != "" {
			j.Tree = otherInFlight.Tree
		}
		if err := enter(&j, domain.State(otherInFlight.Stage)); err != nil {
			return input, nil, err
		}
	} else if j.InFlight == nil && j.State == domain.State(j.Fan.Join) {
		for _, br := range j.Fan.Branches {
			if br.Status == BranchRunning && br.Stage != j.Fan.Join && br.Stage != "" {
				j.Fan.ActiveBranchID = br.ID
				if err := enter(&j, domain.State(br.Stage)); err != nil {
					return input, nil, err
				}
				break
			}
		}
	}
	return j, events, nil
}

// PrepareParallelFanOut prepares all ready branch dispatches for a parallel
// FanOut stage without disturbing already in-flight sibling branches (FAE-WF-096).
func PrepareParallelFanOut(input Job, o Options) (Job, []*Dispatch, []Event, error) {
	if input.Fan != nil && input.Fan.Baseline != nil {
		return prepareNestedFan(input, o, 256, "")
	}
	j := clone(input)
	var allEvents []Event
	var dispatches []*Dispatch
	if j.Fan == nil {
		s, ok := j.Document.Stages[string(j.State)]
		if !ok || s.Primitive != workflow.FanOut || !s.Parallel {
			return j, nil, nil, nil
		}
		var stepEvents []Event
		if err := fanStep(&j, s, &stepEvents); err != nil {
			return input, nil, nil, err
		}
		allEvents = append(allEvents, stepEvents...)
		if j.Fan == nil {
			return j, nil, allEvents, nil
		}
	}
	if !j.Fan.Parallel {
		return j, nil, allEvents, nil
	}
	for i := range j.Fan.Branches {
		br := j.Fan.Branches[i]
		if br.InFlight != nil || br.Status == BranchPassed || br.Status == BranchFailed || br.Status == BranchSkipped {
			continue
		}
		nextJ, d, evs, err := PrepareBranchWith(j, br.ID, o)
		if err != nil {
			return input, nil, nil, err
		}
		j = nextJ
		allEvents = append(allEvents, evs...)
		if d != nil {
			dispatches = append(dispatches, d)
		}
		if j.Fan == nil || j.State == domain.Escalated || j.State.Terminal() {
			break
		}
	}
	return j, dispatches, allEvents, nil
}
