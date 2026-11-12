package daemon

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/workflow"
)

// LandParams starts a landing (FAE-CTL-030), a salvage (FAE-CTL-031) or a
// dependency-ordered batch (FAE-CTL-032) onto an operator branch.
type LandParams struct {
	Tasks     []string `json:"tasks"`
	Target    string   `json:"target"`
	Salvage   bool     `json:"salvage,omitempty"`
	SourceRef string   `json:"source_ref,omitempty"`
	// Flakes and Frozen are refused: the pinned target's landing policy
	// declares them (FAE-CTL-030). Kept so version-1 requests still decode.
	Flakes             []string   `json:"flakes,omitempty"`
	Frozen             [][]string `json:"frozen,omitempty"`
	AuthorizingMessage string     `json:"authorizing_message"`
}

// LandingRecord is the durable, audited state of one landing operation.
type LandingRecord struct {
	ID       string                  `json:"id"`
	State    string                  `json:"state"`
	Target   string                  `json:"target"`
	Gated    string                  `json:"gated"`
	Tasks    []string                `json:"tasks"`
	Salvage  bool                    `json:"salvage,omitempty"`
	Message  string                  `json:"message"`
	Actor    string                  `json:"actor"`
	Started  time.Time               `json:"started"`
	Finished time.Time               `json:"finished,omitzero"`
	Results  []controller.LandResult `json:"results"`
	Evidence string                  `json:"evidence,omitempty"`
	Retired  string                  `json:"retired,omitempty"`
	Error    string                  `json:"error,omitempty"`
}

type landingState struct {
	mu      sync.Mutex
	running sync.Map
}

var landingStates sync.Map

func (d *Daemon) landings() *landingState {
	s, _ := landingStates.LoadOrStore(d, &landingState{})
	return s.(*landingState)
}

// landingActive reports a landing in flight; it writes the operator's branch,
// so maintenance treats it like an integration window.
func (d *Daemon) landingActive() bool {
	active := false
	d.landings().running.Range(func(any, any) bool { active = true; return false })
	return active
}

func landingKey(id string) string { return "landing/" + id }
func salvageKey(task string) string {
	return fmt.Sprintf("landing-salvage/%x", sha256.Sum256([]byte(task)))
}

func (d *Daemon) landRPC(ctx context.Context, r api.Request, principal api.Principal) (any, error) {
	if r.Method == "LandStatus" {
		var p struct {
			ID string `json:"id"`
		}
		if e := decode(r.Params, &p); e != nil {
			return nil, e
		}
		// Sample the running set before the record: runLanding saves its final
		// record before leaving the set, so a landing that finishes between
		// the two reads is never reported interrupted.
		_, running := d.landings().running.Load(p.ID)
		raw, e := d.store.GetMarker(ctx, landingKey(p.ID))
		if errors.Is(e, sql.ErrNoRows) {
			return nil, errors.New("unknown landing")
		}
		if e != nil {
			return nil, e
		}
		var rec LandingRecord
		if e = json.Unmarshal(raw, &rec); e != nil {
			return nil, e
		}
		if rec.State == "running" && !running {
			rec.State, rec.Error = "interrupted", "daemon stopped during landing; inspect target, held and gated references"
		}
		return rec, nil
	}
	var p LandParams
	if e := decode(r.Params, &p); e != nil {
		return nil, e
	}
	if len(p.Flakes) > 0 || len(p.Frozen) > 0 {
		return nil, errors.New("flakes and frozen checks come from the task's pinned target configuration (landing policy), never from a landing request")
	}
	if d.ctx.Err() != nil {
		return nil, errors.New("shutting-down")
	}
	if held, e := d.maintenanceHeld(); held {
		return nil, errors.Join(e, errors.New("landing unavailable during maintenance"))
	}
	// The landing worker is reserved before any write; Close refuses it.
	if e := d.beginWorker(); e != nil {
		return nil, e
	}
	launched := false
	defer func() {
		if !launched {
			d.workers.Done()
		}
	}()
	authorized, e := d.store.AuthorizeAction(ctx, p.AuthorizingMessage, principal.Actor, "Land", "controller", false, actionClasses())
	if e != nil {
		return nil, e
	}
	if d.live == nil {
		return nil, errors.New("landing requires a live workspace runtime")
	}
	target := p.Target
	if !strings.HasPrefix(target, "refs/heads/") {
		target = "refs/heads/" + target
	}
	if len(p.Tasks) == 0 || len(p.Tasks) > 64 || p.Salvage != (p.SourceRef != "") || p.Salvage && len(p.Tasks) != 1 || target == d.live.config.IntegrationRef {
		return nil, errors.New("land requires 1..64 tasks, salvage with exactly one task and --source-ref, and a target other than the instance integration branch")
	}
	state := d.landings()
	if !state.mu.TryLock() {
		return nil, errors.New("another landing is running")
	}
	started := false
	defer func() {
		if !started {
			state.mu.Unlock()
		}
	}()
	graph, e := d.store.Backlog(ctx)
	if e != nil {
		return nil, e
	}
	edges := [][2]string{}
	for _, edge := range graph.Edges {
		if edge.Kind == "blocking" {
			edges = append(edges, [2]string{edge.From, edge.To})
		}
	}
	order, e := controller.DependencyOrder(p.Tasks, edges)
	if e != nil {
		return nil, e
	}
	id := fmt.Sprintf("landing-%d", time.Now().UnixNano())
	gated := "refs/fae/gated/" + strings.TrimPrefix(target, "refs/heads/")
	requests, verdicts := []controller.LandRequest{}, map[string][]string{}
	for i, task := range order {
		req, refs, e := d.landingRequest(ctx, id, i, task, target, gated, p)
		if e != nil {
			return nil, fmt.Errorf("task %s: %w", task, e)
		}
		requests, verdicts[task] = append(requests, req), refs
	}
	rec := LandingRecord{ID: id, State: "running", Target: target, Gated: gated, Tasks: order, Salvage: p.Salvage, Message: p.AuthorizingMessage, Actor: principal.Actor, Started: time.Now().UTC(), Results: []controller.LandResult{}}
	raw, _ := json.Marshal(rec)
	audit := controlplane.Audit{Actor: principal.Actor, Action: "Land", MessageID: p.AuthorizingMessage, Inputs: r.Params, Outcome: "started"}
	if e = d.store.PutMarker(authorized, audit, landingKey(id), raw); e != nil {
		return nil, e
	}
	started, launched = true, true
	state.running.Store(id, true)
	go d.runLanding(rec, requests, verdicts, audit)
	return rec, nil
}

// landingRequest binds one task's own work and pinned policy into a request.
func (d *Daemon) landingRequest(ctx context.Context, id string, n int, task, target, gated string, p LandParams) (controller.LandRequest, []string, error) {
	snapshot, ok, err := d.lockedStoredJob(ctx, task)
	if err != nil {
		return controller.LandRequest{}, nil, err
	}
	if !ok {
		return controller.LandRequest{}, nil, errors.New("unknown task")
	}
	var env envelope
	if e := decodeEnvelope(snapshot.Payload, &env); e != nil {
		return controller.LandRequest{}, nil, e
	}
	j := env.Job
	if j.ExecutionKind != "live" {
		return controller.LandRequest{}, nil, errors.New("landing requires live own-commit evidence")
	}
	scope, e := d.live.resolveLiveTaskScope(task, j.ApprovedScope, workflow.Stage{}, func() (envelope, error) { return env, nil })
	if e != nil {
		return controller.LandRequest{}, nil, e
	}
	policy := controller.DefaultLandPolicy()
	// FAE-CTL-030: flakes and frozen checks come from the pinned target.
	declared, e := d.pinnedLandingPolicy(ctx, env.Admission)
	if e != nil {
		return controller.LandRequest{}, nil, e
	}
	for _, argv := range declared.Frozen {
		if !targetAllows(TargetConfig{AllowedCommands: d.live.config.AllowedCommands}, argv) {
			return controller.LandRequest{}, nil, errors.New("frozen-region check is not an allowed instance command vector")
		}
	}
	policy.Flakes, policy.Frozen = declared.Flakes, declared.Frozen
	// The derived-scope RED identity is one test's name: the filter gate
	// takes it quoted and anchored, naming exactly that test.
	files, filter := scope.Files, ""
	if scope.TestID != "" {
		filter = controller.ExactTestFilter(scope.TestID)
	}
	// An item's declared files and required test filter (FAE-CLI-014) bind
	// its landing. The filter is the regular expression admission accepted,
	// handed to go test -run as written (controller.TestFilter).
	if len(env.Admission.DeclaredFiles) > 0 {
		files = env.Admission.DeclaredFiles
	}
	if env.Admission.TestFilter != "" {
		filter = env.Admission.TestFilter
	}
	var docChecks []controller.DocumentCheck
	for _, dc := range declared.DocumentChecks {
		docChecks = append(docChecks, controller.DocumentCheck{
			ID:         dc.ID,
			Command:    slices.Clone(dc.Command),
			Format:     dc.Format,
			Output:     dc.Output,
			CountField: dc.CountField,
			Writes:     slices.Clone(dc.Writes),
		})
	}
	var reqTests [][]string
	for _, rt := range requiredFor(TargetConfig{RequiredTests: declared.RequiredTests}, files) {
		reqTests = append(reqTests, slices.Clone(rt.Argv))
	}
	policy.DocumentGlobs = slices.Clone(declared.DocumentGlobs)
	policy.DocumentChecks = slices.Clone(docChecks)
	req := controller.LandRequest{ID: fmt.Sprintf("%s-%d", id, n), Task: task, Workspace: d.config.Workspace, Target: target, Files: files, Filter: filter, QA: qaChecks(j.Document), RequiredTests: reqTests, DocumentGlobs: slices.Clone(declared.DocumentGlobs), DocumentChecks: docChecks, Policy: policy, Scratch: filepath.Join(d.layout.Root, "landings", fmt.Sprintf("%s-%d", id, n)), Held: fmt.Sprintf("refs/heads/fae/held/%s-%d", id, n), Gated: gated, Environment: SanitizedEnvironment(os.Environ()), RedactOutput: d.redactLandingOutput}
	if pinnedTgt, tErr := d.pinnedTarget(ctx, env.Admission); tErr == nil && pinnedTgt != nil {
		req.CommitIdentity = pinnedTgt.EffectsCommitIdentity()
		req.ContentPolicy = pinnedTgt.EffectsContentPolicy()
	}
	refs := []string{}
	if env.Job.TaskID == "" {
		env.Job.TaskID = task
	}
	if p.Salvage {
		if snapshot.State != "Escalated" {
			return req, nil, errors.New("salvage requires an Escalated task")
		}
		tree, e := controller.Git(ctx, d.config.Workspace, "rev-parse", "--verify", p.SourceRef+"^{tree}")
		if e != nil {
			return req, nil, e
		}
		for _, a := range j.Evidence {
			if a.Type == "EngineRun" && a.Tree == tree && evidenceNumber(a.Fields["exit_code"]) == 0 && evidenceNumber(a.Fields["executed_count"]) >= 1 {
				refs = append(refs, a.ID)
			}
			if a.Type == "OracleVerdict" {
				refs = append(refs, a.ID)
			}
		}
		passed := slices.ContainsFunc(j.Evidence, func(a workflow.Evidence) bool {
			return a.Type == "EngineRun" && a.Tree == tree && evidenceNumber(a.Fields["exit_code"]) == 0 && evidenceNumber(a.Fields["executed_count"]) >= 1
		})
		if !passed || len(refs) == 0 {
			return req, nil, errors.New("source tree was not recorded as passing by the task's engine runs")
		}
		if j.Base == "" {
			return req, nil, errors.New("salvage requires the task base")
		}
		req.Source, req.Base = tree, j.Base
		if receipt, rErr := applyLandingTestShape(&req, env, scope, []string{"go", "test", "./..."}); rErr == nil {
			if receipt.TaskID == "" {
				receipt.TaskID = task
			}
			_ = d.recordLandingTestReceipt(ctx, id, receipt, controlplane.Audit{Actor: "daemon", MessageID: p.AuthorizingMessage})
		}
		return req, refs, nil
	}
	if snapshot.State != "Done" {
		return req, nil, errors.New("landing requires a Done task")
	}
	commits, e := ownCommitChain(ctx, d.config.Workspace, j)
	if e != nil {
		return req, nil, e
	}
	req.Commits = commits
	if receipt, rErr := applyLandingTestShape(&req, env, scope, []string{"go", "test", "./..."}); rErr == nil {
		if receipt.TaskID == "" {
			receipt.TaskID = task
		}
		_ = d.recordLandingTestReceipt(ctx, id, receipt, controlplane.Audit{Actor: "daemon", MessageID: p.AuthorizingMessage})
	}
	return req, refs, nil
}

func evidenceNumber(v any) float64 {
	switch n := v.(type) {
	case json.Number:
		f, e := n.Float64()
		if e == nil {
			return f
		}
	case float64:
		return n
	case int:
		return float64(n)
	case string:
		if f, e := strconv.ParseFloat(n, 64); e == nil {
			return f
		}
	}
	return -1
}

// qaChecks are the QA-declared HostCommand checks of the task's pinned
// workflow, re-run on the landing tip rather than cited from the verdict.
func qaChecks(doc workflow.Document) [][]string {
	ids := []string{}
	for id, s := range doc.Stages {
		if s.Primitive == workflow.HostCommand && (strings.EqualFold(s.Role, "qa") || strings.Contains(strings.ToLower(id), "qa")) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	out := [][]string{}
	for _, id := range ids {
		argv := doc.Stages[id].Command
		if !slices.ContainsFunc(out, func(x []string) bool { return slices.Equal(x, argv) }) {
			out = append(out, argv)
		}
	}
	return out
}

// ownCommitChain is the linear chain of this task's own broker commits ending
// at its latest commit evidence.
func ownCommitChain(ctx context.Context, ws string, j engine.Job) ([]string, error) {
	recorded := map[string]bool{}
	for _, p := range j.Provenance {
		if p.Primitive != workflow.VcsCommit || p.ExecutionKind != "live" || p.TaskID != j.TaskID {
			continue
		}
		for _, a := range j.Evidence {
			if sha, _ := a.Fields["sha"].(string); a.ID == p.ArtifactID && sha != "" {
				recorded[sha] = true
			}
		}
	}
	head := j.LatestCommitHead()
	if head == "" || !recorded[head] {
		return nil, errors.New("task has no own commit evidence")
	}
	chain := []string{head}
	for {
		parent, e := controller.Git(ctx, ws, "rev-parse", "--verify", chain[0]+"^")
		if e != nil || !recorded[parent] || slices.Contains(chain, parent) {
			return chain, nil
		}
		chain = append([]string{parent}, chain...)
	}
}

func (d *Daemon) landingAdmission(id string) func(context.Context) (func() error, error) {
	return func(ctx context.Context) (func() error, error) {
		g := d.live.guard
		pressure, e := g.pressure()
		if e != nil {
			return nil, e
		}
		_, cfg, e := g.host.HostLimits(ctx)
		if e != nil {
			return nil, e
		}
		observe := g.observe
		if observe == nil {
			observe = hostguard.Observe
		}
		key := fmt.Sprintf("%x", sha256.Sum256([]byte(d.layout.Root+"\x00landing\x00"+id)))
		hard := func(s hostguard.Sample) {
			_ = g.refuse("landing", key, "", 1, hostguard.Step{Outcome: hostguard.OutcomeStop, Reason: "HostHardLimit", Reading: hostguard.Observation{Sample: s}})
		}
		admission := HeavyAdmission{Ledger: g.host, Owner: g.owner, Observe: observe, Clock: pressure.clock, Policy: pressure.policy, Config: cfg, MemoryFloor: LandingFloor, Weight: 2, Probe: g.probe, Hard: hard, Alert: func(line string) {
			g.logf("%s\n", line)
			body, _ := json.Marshal(map[string]string{"landing": id, "detail": line})
			// Each admission line keeps its own outcome: a hold or lease wait,
			// the reading that lifts a hold, the hard breaker's refusal and an
			// expired wait.
			outcome := "waiting"
			switch {
			case strings.HasPrefix(line, "gate lifted "):
				outcome = "lifted"
			case strings.HasPrefix(line, "gate refused "):
				outcome = "refused"
			case strings.HasPrefix(line, "gate timeout "):
				outcome = "timeout"
			}
			_ = d.store.ControllerAction(ctx, controlplane.Audit{Actor: "daemon", Action: "landing-admission-wait", Outcome: outcome}, body, true)
			d.notifyEvents()
		}}
		return admission.Acquire(ctx, key)
	}
}

func (d *Daemon) runLanding(rec LandingRecord, requests []controller.LandRequest, verdicts map[string][]string, started controlplane.Audit) {
	state := d.landings()
	defer d.workers.Done()
	defer state.mu.Unlock()
	defer state.running.Delete(rec.ID)
	ctx := d.ctx
	audit := controlplane.Audit{Actor: started.Actor, MessageID: started.MessageID, Outcome: "ok"}
	save := func() {
		raw, _ := json.Marshal(rec)
		write, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		a := audit
		a.Action = "landing-progress"
		if e := d.store.PutMarker(write, a, landingKey(rec.ID), raw); e != nil {
			d.live.guard.logf("landing %s record failed: %v\n", rec.ID, e)
		}
	}
	for i := range requests {
		requests[i].Admit = d.landingAdmission(requests[i].ID)
		requests[i].TempLeft = d.logScratchLeft
		requests[i].Advance = func(ctx context.Context, res controller.LandResult) error {
			return d.recordGatedProof(ctx, audit, GatedProof{Ref: rec.Gated, SHA: res.Tip, Target: rec.Target, Landing: rec.ID, Task: res.Task, Gates: res.Gates, At: time.Now().UTC()})
		}
	}
	done := func(res controller.LandResult) {
		if rec.Salvage && res.Outcome == controller.LandLanded {
			if e := d.salvageTrail(ctx, &rec, res, verdicts[res.Task], audit); e != nil {
				rec.Error = "salvage landed but its evidence trail or retirement failed: " + e.Error()
			}
		}
		res.Patch = ""
		rec.Results = append(rec.Results, res)
		body, _ := json.Marshal(res)
		a := audit
		a.Action, a.Outcome = "landing-"+res.Outcome, res.Outcome
		write, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = d.store.ControllerAction(write, a, body, res.Outcome != controller.LandLanded)
		cancel()
		d.notifyEvents()
		save()
	}
	_, e := controller.LandBatch(ctx, requests, done)
	rec.State, rec.Finished = "finished", time.Now().UTC()
	if e != nil {
		rec.State, rec.Error = "failed", errors.Join(e, errorText(rec.Error)).Error()
		body, _ := json.Marshal(map[string]string{"landing": rec.ID, "error": rec.Error})
		a := audit
		a.Action, a.Outcome = "landing-failed", "failed"
		write, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = d.store.ControllerAction(write, a, body, true)
		cancel()
		d.notifyEvents()
	}
	save()
}

func errorText(s string) error {
	if s == "" {
		return nil
	}
	return errors.New(s)
}

// salvageTrail stores FAE-CTL-031's evidence as a task artifact, then retires
// the task as contained at the landed sha through the audited retire verb.
func (d *Daemon) salvageTrail(ctx context.Context, rec *LandingRecord, res controller.LandResult, verdicts []string, audit controlplane.Audit) error {
	content, _ := json.Marshal(map[string]any{"landing": rec.ID, "source_tree": res.SourceTree, "base": res.Base, "patch": res.Patch, "gates": res.Gates, "verdicts": verdicts, "landed": res.Landed, "target": rec.Target})
	provenance, _ := json.Marshal(map[string]any{"landing": rec.ID, "actor": rec.Actor, "message": rec.Message})
	a := audit
	a.Action = "landing-salvage-evidence"
	artifact, e := d.store.RecordArtifact(ctx, a, res.Task, "LandingSalvage", "fae.landing.salvage", 1, content, provenance, func(b []byte) error {
		var trail struct {
			SourceTree string                  `json:"source_tree"`
			Base       string                  `json:"base"`
			Patch      string                  `json:"patch"`
			Landed     string                  `json:"landed"`
			Gates      []controller.GateResult `json:"gates"`
			Verdicts   []string                `json:"verdicts"`
		}
		if e := json.Unmarshal(b, &trail); e != nil || trail.SourceTree == "" || trail.Base == "" || trail.Patch == "" || trail.Landed == "" || len(trail.Verdicts) == 0 || !controller.LandingGate(trail.Gates).Passed || !slices.ContainsFunc(trail.Gates, func(g controller.GateResult) bool { return g.Gate == "own-hunk" && g.Passed && g.Count > 0 }) {
			return errors.New("incomplete salvage evidence trail")
		}
		return nil
	})
	if e != nil {
		return e
	}
	rec.Evidence = artifact
	receipt, _ := json.Marshal(map[string]string{"landing": rec.ID, "sha": res.Landed, "evidence": artifact})
	a.Action = "landing-salvage-receipt"
	if e = d.store.PutMarker(ctx, a, salvageKey(res.Task), receipt); e != nil {
		return e
	}
	params, _ := json.Marshal(TaskControlParams{ID: res.Task, Reason: "contained " + res.Landed, AuthorizingMessage: rec.Message})
	retire := controlplane.Audit{Actor: rec.Actor, Action: "Retire", MessageID: rec.Message, Inputs: params, Outcome: "ok"}
	d.mu.Lock()
	defer d.mu.Unlock()
	defer d.publishStatus()
	if _, e = d.taskControl(ctx, "Retire", params, retire); e != nil {
		return e
	}
	rec.Retired = "contained " + res.Landed
	return nil
}

// salvageLanded reports this task's own salvage landing receipt, which makes
// its landed sha task-owned containment evidence.
func (d *Daemon) salvageLanded(ctx context.Context, task, sha string) bool {
	if d.store == nil || sha == "" {
		return false
	}
	raw, e := d.store.GetMarker(ctx, salvageKey(task))
	if e != nil {
		return false
	}
	var receipt struct{ SHA string }
	return json.Unmarshal(raw, &receipt) == nil && receipt.SHA != "" && receipt.SHA == sha
}
