package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"reflect"
	"runtime"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// FAE-VER-003 generated-workflow property suite. Every case is a random valid
// graph built from the primitive library (AgentTurn, Gate, Branch, Loop,
// Review with and without aggregation, HostCommand test runs, shaped and plain
// commands, FreezeArtifact/CompareFrozen, HumanDecision, FanOut/FanIn,
// SubWorkflow, Escalate, and a protected VcsCommit or Integrate optionally
// followed by an ExternalEffect), driven on the scripted adapter with random
// role outputs and host-command results. Every case runs with persistence
// round trips and a crash or graceful recovery at random boundaries and checks
// the FAE-VER-003 properties. Every fifth case is a larger graph (up to five
// blocks) that also runs uninterrupted and must reach the same outcome; the
// others put one library block (each kind in turn) between a first turn and an
// optional protected commit. The default is FAE-VER-003's 10,000 cases per run
// (about 15 s). Under the race detector, where each engine call's clone and
// hash costs far more, the default is a quarter of that; scripts/test.sh also
// runs the suite without it at the full default. Larger campaigns:
// -args -generated.cases=N.
const (
	generatedSeed         = 0xFAE3
	generatedCases        = 10000
	generatedSteps        = 900
	generatedCompareEvery = 5
)

var (
	generatedCase  = flag.Int("generated.case", -1, "run only this FAE-VER-003 generated case index")
	generatedCount = flag.Int("generated.cases", generatedDefault(), "FAE-VER-003 generated full-library cases per run")
)

func generatedDefault() int {
	if raceDetector {
		return generatedCases / 4
	}
	return generatedCases
}

func TestGeneratedWorkflowPropertiesFullLibrary(t *testing.T) {
	first, last := 0, *generatedCount
	if *generatedCase >= 0 {
		first, last = *generatedCase, *generatedCase+1
	}
	type failure struct {
		index int
		err   error
	}
	var mu sync.Mutex
	var failures []failure
	seen := map[workflow.Primitive]int{}
	outcomes := map[string]int{}
	// The engine clones and hashes the job on every call; fewer collections
	// keep the corpus affordable under the race detector.
	defer debug.SetGCPercent(debug.SetGCPercent(400))
	work := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < max(1, min(8, runtime.GOMAXPROCS(0))); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				c := generateCase(i)
				outcome, err := c.check()
				mu.Lock()
				if err != nil {
					failures = append(failures, failure{i, err})
				}
				for _, s := range c.doc.Stages {
					seen[s.Primitive]++
				}
				outcomes[outcome]++
				mu.Unlock()
			}
		}()
	}
	for i := first; i < last; i++ {
		work <- i
	}
	close(work)
	wg.Wait()
	sort.Slice(failures, func(a, b int) bool { return failures[a].index < failures[b].index })
	for i, f := range failures {
		if i == 5 {
			t.Errorf("... %d more failing cases", len(failures)-5)
			break
		}
		t.Errorf("generated case %d (seed %#x, rerun with -args -generated.case=%d): %v", f.index, generatedSeed, f.index, f.err)
	}
	if *generatedCase >= 0 || *generatedCount < generatedDefault() {
		return
	}
	// Every primitive of the library appears in the corpus, and the runs reach
	// every terminating outcome class.
	for _, p := range []workflow.Primitive{workflow.AgentTurn, workflow.Review, workflow.Gate, workflow.HostCommand, workflow.FreezeArtifact, workflow.CompareFrozen, workflow.VcsCommit, workflow.Integrate, workflow.ExternalEffect, workflow.Escalate, workflow.HumanDecision, workflow.Loop, workflow.Branch, workflow.FanOut, workflow.FanIn, workflow.SubWorkflow} {
		if seen[p] < last/50 {
			t.Errorf("primitive %s appears in only %d generated stages", p, seen[p])
		}
	}
	if n := deletionProbes.Load(); n < int64(last/10) {
		t.Errorf("only %d runs deleted a required input", n)
	}
	for _, o := range []string{"Done", "Escalated", "Retired", "Cancelled"} {
		if outcomes[o] < last/100 {
			t.Errorf("outcome %s reached by only %d generated cases: %v", o, outcomes[o], outcomes)
		}
	}
	t.Logf("cases %d, outcomes %v, deleted-input probes %d", last-first, outcomes, deletionProbes.Load())
}

// genCase is one generated workflow with its scripted outputs and the operator
// plan: the option chosen at each decision visit, each child's outcome, and
// what the operator does with an escalation.
type genCase struct {
	index     int
	rng       *rand.Rand
	doc       workflow.Document
	script    map[string][]engine.ScriptStep
	decisions map[string][]string
	children  map[string][]bool
	escalated int // 0 leave, 1 retire, 2 raise the limit and restart once
	cancel    bool
	n         int
	spine     []string // Report stages that ran before every later block
	halts     []string
	change    string // the change-producing role
	branches  map[string]bool
	probed    bool
}

// genExit is a stage whose success route continues to the next block; option
// names the HumanDecision option that routes there.
type genExit struct{ stage, option string }

const genVisits = 24

var genSchemas = map[string]string{
	"Report":         `{"type":"object","required":["ok"],"properties":{"ok":{"type":"boolean"},"n":{"type":"integer"}}}`,
	"Verdict":        `{"type":"object","required":["approved","findings"],"properties":{"approved":{"type":"boolean"},"findings":{"type":"array"}}}`,
	"Run":            `{"type":"object","required":["exit_code","executed_count"],"properties":{"exit_code":{"type":"integer"},"executed_count":{"type":"integer"}}}`,
	"Source":         `{"type":"object","required":["source","test_id"]}`,
	"FrozenIdentity": `{"type":"object","required":["identity"]}`,
	"Comparison":     `{"type":"object","required":["identity","unchanged"],"properties":{"unchanged":{"type":"boolean"}}}`,
	"Decision":       `{"type":"object","required":["decision_id","option","authorizing_message","decided_by"]}`,
	"Consult":        `{"type":"object","required":["child_task","outputs"],"properties":{"outputs":{"type":"object","required":["note"]}}}`,
	"Receipt":        `{"type":"object","required":["sha"]}`,
	"Effect":         `{"type":"object"}`,
}

func generateCase(index int) *genCase {
	c := &genCase{index: index, rng: rand.New(rand.NewSource(int64(generatedSeed)*1_000_003 + int64(index))), script: map[string][]engine.ScriptStep{}, decisions: map[string][]string{}, children: map[string][]bool{}, change: "coder", branches: map[string]bool{}}
	r := c.rng
	role := func(outputs ...string) workflow.Role {
		return workflow.Role{Instructions: "generated", Briefs: map[string]string{"default": "generated brief"}, Routes: []workflow.Binding{{Provider: "scripted", Model: "fixture"}}, Scope: "task-bound", Cap: 1, Outputs: outputs}
	}
	c.doc = workflow.Document{SchemaVersion: 1, ID: fmt.Sprintf("generated-library-%d", index), Limits: workflow.Limits{Attempts: 1 + r.Intn(14), TestRuns: 1 + r.Intn(4), ScopeWidens: 1},
		Roles: map[string]workflow.Role{"worker": role("Report", "Source"), "coder": role("Report"), "rev1": role("Verdict"), "rev2": role("Verdict")}, Artifacts: map[string]workflow.ArtifactType{}, Stages: map[string]workflow.Stage{}}
	c.escalated = r.Intn(3)
	c.cancel = r.Intn(40) == 0
	blocks := []func() (string, []genExit){c.turnBlock, c.gateBlock, c.branchBlock, c.loopBlock, c.reviewBlock, c.hostBlock, c.freezeBlock, c.decisionBlock, c.fanBlock, c.subBlock}
	entry, exits := c.turnBlock()
	c.doc.Start = entry
	large := index%generatedCompareEvery == 0
	kinds := []int{(index / generatedCompareEvery) % len(blocks)}
	if large {
		kinds = kinds[:0]
		for k := r.Intn(5); k > 0; k-- {
			kinds = append(kinds, r.Intn(len(blocks)))
		}
	}
	for _, k := range kinds {
		next, more := blocks[k]()
		c.link(exits, next)
		exits = more
	}
	if large && r.Intn(2) == 0 || !large && r.Intn(4) == 0 {
		next, more := c.commitBlock()
		c.link(exits, next)
		exits = more
	}
	c.link(exits, string(domain.Done))
	// Declare only the artifact types the stages and roles use.
	used := map[string]bool{}
	for _, s := range c.doc.Stages {
		used[s.Output] = true
	}
	for name, r := range c.doc.Roles {
		r.Outputs = slices.DeleteFunc(r.Outputs, func(o string) bool { return !used[o] })
		if len(r.Outputs) == 0 {
			delete(c.doc.Roles, name)
			continue
		}
		c.doc.Roles[name] = r
	}
	for name, schema := range genSchemas {
		if used[name] {
			c.doc.Artifacts[name] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(schema)}
		}
	}
	return c
}

func (c *genCase) id(prefix string) string { c.n++; return fmt.Sprintf("%s%d", prefix, c.n) }
func (c *genCase) edge(from, to string) {
	c.doc.Edges = append(c.doc.Edges, workflow.Edge{From: from, To: to})
}
func (c *genCase) link(exits []genExit, to string) {
	for _, x := range exits {
		c.edge(x.stage, to)
		if x.option != "" {
			s := c.doc.Stages[x.stage]
			for i := range s.Options {
				if s.Options[i].ID == x.option {
					s.Options[i].Next = to
				}
			}
			c.doc.Stages[x.stage] = s
		}
	}
}

// halt is an Escalate stage; failure routes share or create one.
func (c *genCase) halt() string {
	if len(c.halts) > 0 && c.rng.Intn(2) == 0 {
		return c.halts[c.rng.Intn(len(c.halts))]
	}
	id := c.id("halt")
	s := workflow.Stage{Primitive: workflow.Escalate}
	kinds := []domain.EscalationKind{"", domain.HumanDecisionRequired, domain.ReviewGateFailed, domain.CoordinatorVeto, domain.NoRedViolation}
	s.Kind = kinds[c.rng.Intn(len(kinds))]
	if c.rng.Intn(2) == 0 {
		s.Reason = "generated halt of {task} at {stage} attempt {attempt}"
	}
	c.doc.Stages[id] = s
	c.halts = append(c.halts, id)
	return id
}

func (c *genCase) pick(list []string) string { return list[c.rng.Intn(len(list))] }
func (c *genCase) report() json.RawMessage {
	switch c.rng.Intn(20) {
	case 0:
		return json.RawMessage(`{"ok":"maybe"}`) // malformed: a bounded stage failure
	case 1, 2, 3, 4, 5:
		return json.RawMessage(fmt.Sprintf(`{"ok":false,"n":%d}`, c.rng.Intn(9)))
	}
	return json.RawMessage(fmt.Sprintf(`{"ok":true,"n":%d}`, c.rng.Intn(9)))
}
func (c *genCase) outputs(stage string, f func() json.RawMessage) {
	for i := 0; i < genVisits; i++ {
		c.script[stage] = append(c.script[stage], engine.ScriptStep{Output: f()})
	}
}
func (c *genCase) agent(id, role, output string, producer bool) {
	s := workflow.Stage{Primitive: workflow.AgentTurn, Role: role, Output: output, ChangeProducer: producer}
	if len(c.spine) > 0 && c.rng.Intn(2) == 0 {
		s.Inputs = append(s.Inputs, workflow.Input{Name: "prior", Type: "Report", Stage: c.pick(c.spine), Version: 1})
	}
	if c.rng.Intn(4) == 0 {
		var any []string
		for other, o := range c.doc.Stages {
			if o.Output == "Report" && !c.branches[other] {
				any = append(any, other)
			}
		}
		if len(any) > 0 {
			sort.Strings(any)
			// Usually optional; sometimes required from a stage that need not
			// have run (a branch arm), so the stage must park, not run.
			s.Inputs = append(s.Inputs, workflow.Input{Name: "maybe", Type: "Report", Stage: c.pick(any), Version: 1, Optional: c.rng.Intn(4) != 0})
		}
	}
	c.doc.Stages[id] = s
	if output == "Report" {
		c.outputs(id, c.report)
	}
}

func okExpr(stage string) workflow.Expr {
	return bin("eq", fieldOf(stage, "ok"), lit(true))
}
func approvedAtLeastOnce(stage string) workflow.Expr {
	return bin("ge", workflow.Expr{Op: "count", Stage: stage, Args: []workflow.Expr{bin("eq", fieldOf(stage, "approved"), lit(true))}}, lit(1))
}

func (c *genCase) turnBlock() (string, []genExit) {
	id := c.id("turn")
	c.agent(id, "worker", "Report", false)
	c.spine = append(c.spine, id)
	return id, []genExit{{stage: id}}
}

func (c *genCase) gateBlock() (string, []genExit) {
	id := c.id("gate")
	x := okExpr(c.pick(c.spine))
	if c.rng.Intn(3) == 0 {
		x = bin("and", x, bin("ge", fieldOf(c.pick(c.spine), "n"), lit(c.rng.Intn(5))))
	}
	s := workflow.Stage{Primitive: workflow.Gate, Expr: &x, Failure: c.halt()}
	if c.rng.Intn(2) == 0 {
		s.Name = "generated gate " + id
	}
	c.doc.Stages[id] = s
	c.edge(id, s.Failure)
	return id, []genExit{{stage: id}}
}

func (c *genCase) branchBlock() (string, []genExit) {
	id, yes, no := c.id("branch"), c.id("arm"), c.id("arm")
	c.agent(yes, "worker", "Report", false)
	exits := []genExit{{stage: yes}}
	other := no
	if c.rng.Intn(4) == 0 {
		other = c.halt()
	} else {
		c.agent(no, "worker", "Report", false)
		exits = append(exits, genExit{stage: no})
	}
	cases := []workflow.Case{{When: okExpr(c.pick(c.spine)), Next: yes}}
	if c.rng.Intn(2) == 0 {
		cases = append([]workflow.Case{{When: bin("gt", fieldOf(c.pick(c.spine), "n"), lit(6)), Next: other}}, cases...)
	}
	c.doc.Stages[id] = workflow.Stage{Primitive: workflow.Branch, Cases: cases, Default: other}
	c.edge(id, yes)
	c.edge(id, other)
	return id, exits
}

func (c *genCase) loopBlock() (string, []genExit) {
	loop, work, check := c.id("loop"), c.id("work"), c.id("check")
	s := workflow.Stage{Primitive: workflow.Loop, MaxIterations: 1 + c.rng.Intn(3)}
	if c.rng.Intn(2) == 0 {
		s.ExitGate = check
	}
	if c.rng.Intn(3) == 0 {
		s.OnExhaustion = domain.CoordinatorVeto
	}
	c.doc.Stages[loop] = s
	c.agent(work, "worker", "Report", false)
	x := okExpr(work)
	c.doc.Stages[check] = workflow.Stage{Primitive: workflow.Gate, Expr: &x, Failure: loop}
	c.edge(loop, work)
	c.edge(work, check)
	c.edge(check, loop)
	c.spine = append(c.spine, work)
	return loop, []genExit{{stage: check}}
}

func (c *genCase) review(id string) {
	s := workflow.Stage{Primitive: workflow.Review, Role: "rev1", Output: "Verdict"}
	if c.rng.Intn(3) == 0 {
		s.Reviewers = []string{"rev1", "rev2"}
	} else if c.rng.Intn(2) == 0 {
		s.Count = 2
	}
	c.doc.Stages[id] = s
	n := workflow.ReviewSessions(s)
	for i := 0; i < genVisits; i++ {
		step := engine.ScriptStep{}
		for k := 0; k < n; k++ {
			severity := c.pick([]string{"non-blocking", "non-blocking", "blocking", "unclear"})
			step.Reviews = append(step.Reviews, json.RawMessage(fmt.Sprintf(`{"approved":%t,"findings":[{"severity":%q}]}`, c.rng.Intn(4) != 0, severity)))
		}
		if c.rng.Intn(25) == 0 {
			step.Reviews[0] = json.RawMessage(`{"approved":true}`) // malformed: no findings
		}
		c.script[id] = append(c.script[id], step)
	}
}
func (c *genCase) aggregate(id string) []genExit {
	s := c.doc.Stages[id]
	s.Aggregation = c.pick([]string{workflow.AllPass, workflow.KOfN, workflow.AnyBlockingFails})
	if s.Aggregation == workflow.KOfN {
		s.Quorum = 1 + c.rng.Intn(workflow.ReviewSessions(s))
	}
	s.Failure = c.halt()
	c.doc.Stages[id] = s
	c.edge(id, s.Failure)
	return []genExit{{stage: id}}
}

func (c *genCase) reviewBlock() (string, []genExit) {
	id := c.id("review")
	c.review(id)
	switch c.rng.Intn(3) {
	case 0:
		return id, c.aggregate(id)
	case 1:
		gate := c.id("approval")
		x := approvedAtLeastOnce(id)
		c.doc.Stages[gate] = workflow.Stage{Primitive: workflow.Gate, Expr: &x, Failure: c.halt()}
		c.edge(id, gate)
		c.edge(gate, c.doc.Stages[gate].Failure)
		return id, []genExit{{stage: gate}}
	}
	return id, []genExit{{stage: id}}
}

func (c *genCase) run(fresh bool) json.RawMessage {
	exit := 0
	if c.rng.Intn(4) == 0 {
		exit = 1
	}
	return json.RawMessage(fmt.Sprintf(`{"exit_code":%d,"executed_count":%d,"matched_count":%d,"fresh_build":%t}`, exit, c.rng.Intn(4), c.rng.Intn(3), fresh || c.rng.Intn(30) != 0))
}
func (c *genCase) testRun(id string) {
	c.doc.Stages[id] = workflow.Stage{Primitive: workflow.HostCommand, Output: "Run", Command: []string{"go", "test", "-json", "-count=1", "./..."}, Weight: 1, TimeoutSeconds: 30, TestRun: true}
	c.outputs(id, func() json.RawMessage { return c.run(false) })
}
func (c *genCase) testGate(id, run string) {
	x := bin("and", bin("eq", fieldOf(run, "exit_code"), lit(0)), bin("ge", fieldOf(run, "executed_count"), lit(1)))
	c.doc.Stages[id] = workflow.Stage{Primitive: workflow.Gate, Expr: &x, TestStage: run, Failure: c.halt()}
	c.edge(id, c.doc.Stages[id].Failure)
}

func (c *genCase) hostBlock() (string, []genExit) {
	id := c.id("host")
	switch c.rng.Intn(3) {
	case 0:
		c.testRun(id)
		gate := c.id("tested")
		c.edge(id, gate)
		c.testGate(gate, id)
		return id, []genExit{{stage: gate}}
	case 1:
		shapes := []workflow.CommandShape{{Exit: "zero"}, {MinExecuted: 1}, {Exit: "nonzero", Matched: 1}, {Exit: "zero", Executed: 2}}
		shape := shapes[c.rng.Intn(len(shapes))]
		s := workflow.Stage{Primitive: workflow.HostCommand, Output: "Run", Command: []string{"./scripts/check.sh"}, Weight: 1, TimeoutSeconds: 30, ExpectedShape: &shape, Failure: c.halt(), Environment: map[string]string{"GENERATED": "1"}}
		c.doc.Stages[id] = s
		c.edge(id, s.Failure)
		c.outputs(id, func() json.RawMessage { return c.run(true) })
	default:
		c.doc.Stages[id] = workflow.Stage{Primitive: workflow.HostCommand, Output: "Run", Command: []string{"make", "lint"}, Weight: 2, TimeoutSeconds: 60}
		c.outputs(id, func() json.RawMessage { return c.run(true) })
	}
	return id, []genExit{{stage: id}}
}

// freezeBlock freezes a test's identity and compares a later rewrite of it:
// a formatting-only rewrite is unchanged, a changed body is not.
func (c *genCase) freezeBlock() (string, []genExit) {
	author, freeze, rewrite, compare, gate := c.id("author"), c.id("freeze"), c.id("rewrite"), c.id("compare"), c.id("unchanged")
	c.agent(author, "worker", "Source", false)
	c.agent(rewrite, "worker", "Source", false)
	sources := []string{"package p\nfunc TestRed(t *T) { assert(1 == 2) }", "package p\n\nfunc TestRed(t *T) {\n\tassert(1 == 2)\n}\n", "package p\nfunc TestRed(t *T) { assert(1 == 1) }"}
	original := sources[0]
	for _, id := range []string{author, rewrite} {
		for i := 0; i < genVisits; i++ {
			text := original
			if id == rewrite {
				text = c.pick(sources)
			}
			out, _ := json.Marshal(map[string]string{"source": text, "test_id": "TestRed"})
			c.script[id] = append(c.script[id], engine.ScriptStep{Output: out})
		}
	}
	c.doc.Stages[freeze] = workflow.Stage{Primitive: workflow.FreezeArtifact, Subject: author, Output: "FrozenIdentity", Inputs: []workflow.Input{{Name: "subject", Type: "Source", Stage: author, Version: 1}}}
	c.doc.Stages[compare] = workflow.Stage{Primitive: workflow.CompareFrozen, Subject: rewrite, Output: "Comparison", Inputs: []workflow.Input{{Name: "subject", Type: "Source", Stage: rewrite, Version: 1}, {Name: "frozen", Type: "FrozenIdentity", Stage: freeze, Version: 1}}}
	x := bin("eq", fieldOf(compare, "unchanged"), lit(true))
	c.doc.Stages[gate] = workflow.Stage{Primitive: workflow.Gate, Expr: &x, Failure: c.halt()}
	for _, id := range []string{freeze, compare} {
		c.outputs(id, func() json.RawMessage { return json.RawMessage(`{}`) })
	}
	c.edge(author, freeze)
	c.edge(freeze, rewrite)
	c.edge(rewrite, compare)
	c.edge(compare, gate)
	c.edge(gate, c.doc.Stages[gate].Failure)
	return author, []genExit{{stage: gate}}
}

func (c *genCase) decisionBlock() (string, []genExit) {
	id, stop := c.id("decide"), c.halt()
	s := workflow.Stage{Primitive: workflow.HumanDecision, Question: "Proceed with " + id + "?", Options: []workflow.DecisionOption{{ID: "proceed"}, {ID: "halt", Next: stop, Description: "stop here"}}, OnTimeout: workflow.StayWaiting, Output: "Decision"}
	if c.rng.Intn(2) == 0 {
		s.Inputs = []workflow.Input{{Name: "context", Type: "Report", Stage: c.pick(c.spine), Version: 1}}
	}
	c.doc.Stages[id] = s
	c.edge(id, stop)
	for i := 0; i < genVisits; i++ {
		c.decisions[id] = append(c.decisions[id], c.pick([]string{"proceed", "proceed", "proceed", "halt"}))
	}
	return id, []genExit{{stage: id, option: "proceed"}}
}

func (c *genCase) fanBlock() (string, []genExit) {
	fan, join := c.id("fan"), c.id("join")
	s := workflow.Stage{Primitive: workflow.FanOut, Join: join}
	for b := 0; b < 2+c.rng.Intn(2); b++ {
		start := c.id("branch")
		var pass *workflow.Expr
		if c.rng.Intn(2) == 0 {
			c.agent(start, "worker", "Report", false)
			x := okExpr(start)
			pass = &x
		} else {
			c.review(start)
			x := approvedAtLeastOnce(start)
			pass = &x
		}
		if c.rng.Intn(3) == 0 {
			pass = nil
		}
		s.Branches = append(s.Branches, workflow.FanBranch{ID: fmt.Sprintf("b%d", b), Start: start, Pass: pass})
		c.branches[start] = true
		c.edge(fan, start)
		c.edge(start, join)
	}
	c.doc.Stages[fan] = s
	in := workflow.Stage{Primitive: workflow.FanIn, Rule: c.pick([]string{workflow.JoinAll, workflow.JoinAny, workflow.KOfN}), Failure: c.halt()}
	if in.Rule == workflow.KOfN {
		in.Quorum = 1 + c.rng.Intn(len(s.Branches))
	}
	c.doc.Stages[join] = in
	c.edge(join, in.Failure)
	return fan, []genExit{{stage: join}}
}

func (c *genCase) subBlock() (string, []genExit) {
	id := c.id("sub")
	s := workflow.Stage{Primitive: workflow.SubWorkflow, Workflow: &workflow.WorkflowRef{ID: "child", Revision: 1}, Output: "Consult", OutputMap: []workflow.ArtifactMap{{Name: "note", Stage: "draft", Type: "Note"}}}
	if c.rng.Intn(2) == 0 {
		s.InputMap = []workflow.ArtifactMap{{Name: "context", Stage: c.pick(c.spine)}}
	}
	if c.rng.Intn(2) == 0 {
		s.Failure = c.halt()
		c.edge(id, s.Failure)
	}
	c.doc.Stages[id] = s
	for i := 0; i < genVisits; i++ {
		c.children[id] = append(c.children[id], c.rng.Intn(4) != 0)
	}
	return id, []genExit{{stage: id}}
}

// commitBlock is the protected floor: change, test run, test gate, an
// independent review, then a protected commit or integration, optionally
// followed by an ExternalEffect that a Local run must refuse.
func (c *genCase) commitBlock() (string, []genExit) {
	change, test, gate, review := c.id("change"), c.id("ctest"), c.id("cgate"), c.id("creview")
	c.agent(change, c.change, "Report", true)
	c.testRun(test)
	c.testGate(gate, test)
	c.review(review)
	exits := []genExit{{stage: review}}
	if c.rng.Intn(2) == 0 {
		exits = c.aggregate(review)
	}
	effect := c.id("commit")
	if c.rng.Intn(3) == 0 {
		c.doc.Stages[effect] = workflow.Stage{Primitive: workflow.Integrate, Output: "Receipt"}
	} else {
		c.doc.Stages[effect] = workflow.Stage{Primitive: workflow.VcsCommit, Output: "Receipt", Protected: true}
	}
	c.outputs(effect, func() json.RawMessage { return json.RawMessage(fmt.Sprintf(`{"sha":"%040x"}`, c.rng.Int63())) })
	c.edge(change, test)
	c.edge(test, gate)
	c.edge(gate, review)
	c.link(exits, effect)
	exits = []genExit{{stage: effect}}
	if c.rng.Intn(3) == 0 {
		publish := c.id("publish")
		c.doc.Stages[publish] = workflow.Stage{Primitive: workflow.ExternalEffect, Output: "Effect", NormalOnly: true, Pins: map[string]string{"adapter": "generated"}, IdempotenceKey: "generated-" + publish}
		c.edge(effect, publish)
		exits = []genExit{{stage: publish}}
	}
	return change, exits
}

// check validates the generated document, runs it uninterrupted and again
// with interruptions, checks every property on both runs and compares them.
func (c *genCase) check() (string, error) {
	if issues := workflow.Validate(c.doc); len(issues) > 0 {
		return "", fmt.Errorf("generated workflow is invalid: %v", issues)
	}
	raw, err := json.Marshal(c.doc)
	if err != nil {
		return "", err
	}
	parsed, err := workflow.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("generated workflow does not parse strictly: %v", err)
	}
	want, _ := workflow.Hash(c.doc)
	if got, _ := workflow.Hash(parsed); got != want {
		return "", errors.New("generated workflow changed through strict parsing")
	}
	irng := rand.New(rand.NewSource(int64(c.index)*7919 + 17))
	if c.index%generatedCompareEvery != 0 {
		// Single-run class: interrupted at random boundaries, every property
		// except the comparison with an uninterrupted run.
		k := irng.Intn(16)
		resumed, _, err := c.drive(&interrupter{rng: irng, at: k}, true)
		if err != nil {
			return "", fmt.Errorf("interrupted run (forced interruption at boundary %d): %w", k, err)
		}
		return label(resumed), nil
	}
	base, steps, err := c.drive(nil, true)
	if err != nil {
		return "", fmt.Errorf("uninterrupted run: %w", err)
	}
	k := irng.Intn(max(1, steps))
	resumed, _, err := c.drive(&interrupter{rng: irng, at: k}, false)
	if err != nil {
		return "", fmt.Errorf("interrupted run (forced interruption at boundary %d): %w", k, err)
	}
	a, b := outcomeOf(base), outcomeOf(resumed)
	if !reflect.DeepEqual(a, b) {
		return "", fmt.Errorf("resumed run diverged after interruption at boundary %d:\nuninterrupted %+v\nresumed       %+v", k, a, b)
	}
	return label(base), nil
}

func label(j engine.Job) string {
	if j.Parked {
		return "Parked"
	}
	return string(j.State)
}

// interrupter persists and reloads the job at random boundaries and always at
// boundary at; a reload with a dispatch in flight recovers it gracefully or as
// a crash (at most twice, the dispatch-recovery bound).
type interrupter struct {
	rng     *rand.Rand
	at, n   int
	crashes int
}

func (x *interrupter) hit() bool {
	if x == nil {
		return false
	}
	x.n++
	return x.n-1 == x.at || x.rng.Intn(5) == 0
}

func reload(j engine.Job) (engine.Job, error) {
	b, err := json.Marshal(j)
	if err != nil {
		return j, err
	}
	var k engine.Job
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	return k, dec.Decode(&k)
}

// drive runs the case to its end: Done, Escalated (left for the operator),
// Retired or Cancelled. Parked decisions and sub-workflows stay parked until
// the operator decides or the child finishes, and then resume.
func (c *genCase) drive(x *interrupter, probe bool) (engine.Job, int, error) {
	j, err := engine.New("task", 1, c.doc, "tree")
	if err != nil {
		return j, 0, err
	}
	exec := engine.NewScripted(c.script)
	prev := j
	restarted, deleted := false, false
	if c.cancel {
		if j, _, err = engine.ApplyControl(j, engine.Control{Method: "Cancel", Reason: "generated cancellation"}, time.Now()); err != nil {
			return j, 0, err
		}
	}
	for step := 0; step < generatedSteps; step++ {
		if err := floors(c, prev, j); err != nil {
			return j, step, err
		}
		prev = j
		if x.hit() {
			if j, err = reload(j); err != nil {
				return j, step, err
			}
		}
		switch {
		case j.State.Terminal():
			return j, step, finalChecks(c, j, probe)
		case j.State == domain.Escalated:
			// No-op probes run on the uninterrupted run only; they do not
			// change the outcome the resumed run must reproduce.
			if !probe {
			} else if next, d, events, err := engine.Prepare(j); err != nil || d != nil || len(events) != 0 || !reflect.DeepEqual(next.History, j.History) {
				return j, step, fmt.Errorf("an escalated task moved without the operator: %v %v", events, err)
			}
			switch {
			case c.escalated == 1:
				j, _, err = engine.ApplyControl(j, engine.Control{Method: "Retire", Reason: "obsolete generated escalation"}, time.Now())
			case c.escalated == 2 && !restarted:
				restarted = true
				if j, _, err = engine.ApplyControl(j, engine.Control{Method: "SetAttemptLimit", AttemptLimit: j.Attempts + c.doc.Limits.Attempts}, time.Now()); err == nil {
					j, _, err = engine.ApplyControl(j, engine.Control{Method: "RestartWithInformation", Information: "generated operator restart"}, time.Now())
				}
			default:
				return j, step, finalChecks(c, j, probe)
			}
			if err != nil {
				return j, step, fmt.Errorf("operator control: %w", err)
			}
			continue
		case j.Parked:
			// A parked task never moves until its cause clears.
			if !probe {
			} else if next, d, events, err := engine.Prepare(j); err != nil || d != nil || len(events) != 0 || !next.Parked {
				return j, step, fmt.Errorf("parked task (%s) moved before its cause cleared: %v %v", j.Reason, events, err)
			}
			if req, ok := engine.PendingDecision(j); ok {
				option := c.decisions[req.Stage][req.Visit-1]
				if j, _, _, err = engine.Decide(j, engine.Decision{Option: option, Message: "generated message", Actor: "operator"}); err != nil {
					return j, step, fmt.Errorf("decision: %w", err)
				}
				continue
			}
			if ref, ok := engine.PendingChild(j); ok {
				child, err := runChild(ref, j.TaskID, c.children[ref.Stage][ref.Visit-1])
				if err != nil {
					return j, step, err
				}
				if j, _, err = engine.BindChild(j, child); err != nil {
					return j, step, fmt.Errorf("bind child: %w", err)
				}
				continue
			}
			if !strings.HasPrefix(j.Reason, "MissingInput") {
				return j, step, fmt.Errorf("parked on an unresolvable cause: %s", j.Reason)
			}
			// A stage whose required input was never produced parks until it
			// is, naming exactly the inputs no stage produced.
			missing := []string{}
			for _, in := range c.doc.Stages[string(j.State)].Inputs {
				produced := false
				for _, a := range j.Evidence {
					produced = produced || a.Type == in.Type && a.Stage == in.Stage
				}
				if !in.Optional && !produced {
					missing = append(missing, in.Name)
				}
			}
			if len(missing) == 0 || j.Reason != fmt.Sprintf("MissingInput: %v", missing) {
				return j, step, fmt.Errorf("%s parked on %s; unproduced required inputs %v", j.State, j.Reason, missing)
			}
			return j, step, finalChecks(c, j, probe)
		}
		if probe && !deleted {
			if done, err := deletedInputParks(c, j); err != nil {
				return j, step, err
			} else if done {
				deleted = true
			}
		}
		next, d, _, err := engine.Prepare(j)
		if err != nil {
			return j, step, fmt.Errorf("prepare at %s: %w", j.State, err)
		}
		j = next
		if d != nil && x.hit() {
			if j, err = reload(j); err != nil {
				return j, step, err
			}
			graceful := x.crashes >= 2 || x.rng.Intn(2) == 0
			if !graceful {
				x.crashes++
			}
			var replay *engine.Dispatch
			if j, replay, err = engine.Recover(j, graceful, true); err != nil || replay == nil || replay.ID != d.ID {
				return j, step, fmt.Errorf("mid-stage recovery of %s: %v", d.ID, err)
			}
			d = replay
		}
		if d == nil {
			continue
		}
		if err := dispatchChecks(c, j, *d); err != nil {
			return j, step, err
		}
		r, err := exec.Execute(context.Background(), *d)
		if err != nil {
			return j, step, fmt.Errorf("scripted %s: %w", d.Stage, err)
		}
		if j, _, err = engine.Complete(j, d.ID, r); err != nil {
			return j, step, fmt.Errorf("complete %s: %w", d.ID, err)
		}
	}
	return j, generatedSteps, errors.New("run did not terminate within the step bound")
}

func runChild(ref engine.ChildRef, parent string, ok bool) (engine.Job, error) {
	c, err := engine.New(ref.Task, ref.Revision, childDoc(), "tree")
	if err != nil {
		return c, err
	}
	c.Parent = &engine.ParentRef{Task: parent, Stage: ref.Stage, Visit: ref.Visit}
	output := `{"ok":true,"text":"consulted"}`
	if !ok {
		output = `{"ok":false}`
	}
	exec := engine.NewScripted(map[string][]engine.ScriptStep{"draft": {{Output: json.RawMessage(output)}}})
	for i := 0; i < 20 && !c.State.Terminal(); i++ {
		if c.State == domain.Escalated {
			c, _, err = engine.ApplyControl(c, engine.Control{Method: "Retire", Reason: "obsolete child failed"}, time.Now())
			return c, err
		}
		next, d, _, err := engine.Prepare(c)
		if err != nil {
			return c, err
		}
		c = next
		if d == nil {
			continue
		}
		r, err := exec.Execute(context.Background(), *d)
		if err != nil {
			return c, err
		}
		if c, _, err = engine.Complete(c, d.ID, r); err != nil {
			return c, err
		}
	}
	return c, nil
}

// outcome is what a resumed run must reproduce exactly.
type outcome struct {
	State                      domain.State
	Reason, Detail             string
	Kind                       domain.EscalationKind
	Attempts, TestRuns, Seq    int
	Counts                     map[string]int
	Evidence, Records, Changes []string
}

func outcomeOf(j engine.Job) outcome {
	o := outcome{State: j.State, Reason: j.Reason, Detail: j.EscalationDetail, Kind: j.EscalationKind, Attempts: j.Attempts, TestRuns: j.TestRuns, Seq: j.Sequence, Counts: j.Counts}
	for _, a := range j.Evidence {
		o.Evidence = append(o.Evidence, a.Stage+"/"+a.Type+"/"+a.ID)
	}
	for _, r := range j.GateRecords {
		o.Records = append(o.Records, r.ID)
	}
	for _, e := range j.History {
		if e.Kind == "transition" {
			o.Changes = append(o.Changes, fmt.Sprintf("%s>%s:%s", e.From, e.To, e.Cause))
		}
	}
	return o
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, true
	}
	return 0, false
}

// floors holds on every step: the attempt, test-run and loop caps; append-only
// evidence, provenance, history and gate records; the bound revision.
func floors(c *genCase, prev, j engine.Job) error {
	if j.Attempts > j.AttemptLimit() {
		return fmt.Errorf("attempts %d exceed the limit %d", j.Attempts, j.AttemptLimit())
	}
	if j.TestRuns > c.doc.Limits.TestRuns {
		return fmt.Errorf("test runs %d exceed the cap %d", j.TestRuns, c.doc.Limits.TestRuns)
	}
	for id, s := range c.doc.Stages {
		if s.Primitive == workflow.Loop && j.Counts[id] > s.MaxIterations {
			return fmt.Errorf("loop %s ran %d of %d iterations", id, j.Counts[id], s.MaxIterations)
		}
	}
	if len(j.Evidence) < len(prev.Evidence) || len(j.History) < len(prev.History) || len(j.Provenance) < len(prev.Provenance) || len(j.GateRecords) < len(prev.GateRecords) {
		return errors.New("a durable record was removed")
	}
	for i := range prev.Evidence {
		if prev.Evidence[i].ID != j.Evidence[i].ID || prev.Evidence[i].Stage != j.Evidence[i].Stage {
			return errors.New("recorded evidence was rewritten")
		}
	}
	for i := range prev.History {
		if prev.History[i].Kind != j.History[i].Kind || prev.History[i].Cause != j.History[i].Cause || prev.History[i].To != j.History[i].To {
			return errors.New("audit history was rewritten")
		}
	}
	for i := range prev.GateRecords {
		if prev.GateRecords[i].ID != j.GateRecords[i].ID {
			return errors.New("a gate record was rewritten")
		}
	}
	if prev.State.Terminal() && (j.State != prev.State || len(j.History) != len(prev.History)) {
		return fmt.Errorf("terminal %s transitioned again", prev.State)
	}
	return nil
}

// dispatchChecks: no stage runs without its inputs, never with altered
// configuration, and a protected effect only with the floor's evidence.
func dispatchChecks(c *genCase, j engine.Job, d engine.Dispatch) error {
	got, _ := json.Marshal(d.Config)
	want, _ := json.Marshal(c.doc.Stages[d.Stage])
	owner := j
	if j.Fan != nil && j.Fan.Baseline != nil && d.BranchID != "" {
		var ok bool
		owner, ok = engine.BranchJob(j, d.BranchID)
		if !ok {
			return fmt.Errorf("dispatch %s has no authoritative branch", d.ID)
		}
	}
	if d.Stage != string(owner.State) || string(got) != string(want) {
		return fmt.Errorf("dispatch %s does not run the bound stage configuration", d.ID)
	}
	recorded := map[string]bool{}
	for _, a := range j.Evidence {
		recorded[a.ID] = true
	}
	for _, in := range d.Inputs {
		if !recorded[in.ID] || in.TaskID != j.TaskID || !in.Provenance {
			return fmt.Errorf("%s consumed evidence %s that was not recorded for this task", d.Stage, in.ID)
		}
	}
	for _, in := range d.Config.Inputs {
		found := false
		for _, a := range d.Inputs {
			found = found || a.Type == in.Type && a.Version == in.Version && (in.Stage == "" || a.Stage == in.Stage)
		}
		if !found && !in.Optional {
			return fmt.Errorf("%s ran without its input %s", d.Stage, in.Name)
		}
	}
	switch d.Config.Primitive {
	case workflow.ExternalEffect:
		return fmt.Errorf("a Local scripted run dispatched external effect %s", d.Stage)
	case workflow.VcsCommit, workflow.Integrate:
		return protectedEvidence(c, j)
	}
	return nil
}

// protectedEvidence re-derives the protected-commit floor (FAE-WF-071) from the
// recorded order: after the last change, a passing positive test run cited by
// a passing test gate and an approved review by another role.
func protectedEvidence(c *genCase, j engine.Job) error {
	last := -1
	for i, a := range j.Evidence {
		if c.doc.Stages[a.Stage].ChangeProducer {
			last = i
		}
	}
	if last < 0 {
		return errors.New("protected effect without a recorded change")
	}
	runs, reviewed := map[string]bool{}, false
	for _, a := range j.Evidence[last+1:] {
		s := c.doc.Stages[a.Stage]
		exit, eok := number(a.Fields["exit_code"])
		count, cok := number(a.Fields["executed_count"])
		if s.Primitive == workflow.HostCommand && s.TestRun && eok && cok && exit == 0 && count >= 1 {
			runs[a.ID] = true
		}
		if s.Primitive == workflow.Review && a.Fields["approved"] == true && a.Role != c.change {
			reviewed = true
		}
	}
	gated := false
	for _, e := range j.History {
		if e.Kind == "gate" && e.Gate != nil && e.Gate.Passed && c.doc.Stages[e.Stage].TestStage != "" {
			for _, id := range e.Gate.EvidenceIDs {
				gated = gated || runs[id]
			}
		}
	}
	if !gated || !reviewed {
		return fmt.Errorf("protected effect dispatched without its floor (gated test %v, independent approval %v)", gated, reviewed)
	}
	return nil
}

// finalChecks: the run ended in a terminating state; every transition is
// audited, legal and chained; every passing check read evidence that exists;
// records replay; the task is immutable once terminal.
func finalChecks(c *genCase, j engine.Job, probe bool) error {
	switch j.State {
	case domain.Done, domain.Escalated, domain.Retired, domain.Cancelled:
	default:
		if !j.Parked {
			return fmt.Errorf("run ended in non-terminating state %s", j.State)
		}
	}
	missing := j.Parked && strings.HasPrefix(j.Reason, "MissingInput") && !j.State.Kernel()
	if j.Parked && !missing || j.InFlight != nil {
		return fmt.Errorf("run ended parked (%s) or with a dispatch in flight", j.Reason)
	}
	if err := engine.Validate(j); err != nil {
		return fmt.Errorf("final job does not validate: %w", err)
	}
	if err := engine.ReplayGates(j); err != nil {
		return fmt.Errorf("gate records do not replay: %w", err)
	}
	if h, _ := workflow.Hash(c.doc); j.WorkflowHash != h {
		return errors.New("the bound workflow revision changed")
	}
	if h, _ := workflow.Hash(j.Document); j.WorkflowHash != h {
		return errors.New("the bound workflow revision changed")
	}
	state := domain.Pending
	for _, e := range j.History {
		if e.Kind == "fan-cursor" {
			fan := c.doc.Stages[e.Stage]
			if e.From != state || fan.Primitive != workflow.FanOut || string(e.To) != fan.Join {
				return fmt.Errorf("invalid fan cursor projection %s>%s", e.From, e.To)
			}
			state = e.To
			continue
		}
		if e.Kind != "transition" {
			continue
		}
		if e.Attempt < 1 || e.Cause == "" || e.From != state {
			return fmt.Errorf("transition %s>%s lacks its audit context or chain (from %s)", e.From, e.To, state)
		}
		if !workflow.CanTransition(c.doc, e.From, e.To) {
			return fmt.Errorf("illegal transition %s>%s (%s)", e.From, e.To, e.Cause)
		}
		state = e.To
	}
	if state != j.State {
		return fmt.Errorf("audit chain ends at %s, task is %s", state, j.State)
	}
	// Routing follows the recorded check: a failed gate, aggregate verdict or
	// shape check leaves by its failure route (or escalates), a passed one never.
	for i, e := range j.History {
		if e.Gate == nil || e.Kind != "gate" && e.Kind != "review-aggregate" && e.Kind != "expected-shape" {
			continue
		}
		for _, next := range j.History[i+1:] {
			if next.Kind != "transition" {
				continue
			}
			failure := c.doc.Stages[e.Stage].Failure
			toFailure := next.To == domain.State(failure)
			if next.From != domain.State(e.Stage) || e.Gate.Passed && toFailure || !e.Gate.Passed && !toFailure && next.To != domain.Escalated {
				return fmt.Errorf("%s %s (passed %v) routed %s>%s", e.Kind, e.Stage, e.Gate.Passed, next.From, next.To)
			}
			break
		}
	}
	recorded := map[string]bool{}
	for _, a := range j.Evidence {
		recorded[a.ID] = true
		found := false
		for _, p := range j.Provenance {
			found = found || p.ArtifactID == a.ID && p.Stage == a.Stage && p.Session == a.Session && p.TaskID == j.TaskID
		}
		if !found || a.TaskID != j.TaskID || !a.Provenance {
			return fmt.Errorf("evidence %s at %s lacks provenance", a.ID, a.Stage)
		}
	}
	for _, r := range j.GateRecords {
		if r.Kind == workflow.RecordBranchArm || !r.Result.Passed {
			continue
		}
		if len(r.Result.EvidenceIDs) == 0 {
			return fmt.Errorf("%s %s passed without reading evidence", r.Kind, r.Stage)
		}
		for _, id := range r.Result.EvidenceIDs {
			if !recorded[id] {
				return fmt.Errorf("%s %s read unrecorded evidence %s", r.Kind, r.Stage, id)
			}
		}
	}
	if !probe {
		return nil
	}
	// Terminal and escalated tasks never move on their own; a terminal task
	// refuses every completion, control and decision.
	next, d, events, err := engine.Prepare(j)
	if err != nil || d != nil || len(events) != 0 || next.State != j.State || len(next.History) != len(j.History) {
		return fmt.Errorf("a %s task moved: %v %v", j.State, events, err)
	}
	if j.State.Terminal() && !c.probed {
		// Probed once per case (on the uninterrupted run), one control each.
		c.probed = true
		if j.Sequence > 0 {
			id := fmt.Sprintf("%s/%s/%d", j.TaskID, j.WorkflowHash, j.Sequence)
			if _, _, err := engine.Complete(j, id, engine.Result{DispatchID: id, TaskID: j.TaskID, Tree: j.Tree}); err == nil {
				return errors.New("a terminal task accepted a completion")
			}
		}
		controls := []engine.Control{{Method: "Retire", Reason: "obsolete again"}, {Method: "RestartWithInformation", Information: "again"}, {Method: "Resume"}, {Method: "Cancel", Reason: "again"}, {Method: "Block", Reason: "again"}}
		if m := controls[c.index%len(controls)]; func() bool { _, _, err := engine.ApplyControl(j, m, time.Now()); return err == nil }() {
			return fmt.Errorf("a terminal task accepted %s", m.Method)
		}
		if _, _, _, err := engine.Decide(j, engine.Decision{Option: "proceed", Message: "again"}); err == nil {
			return errors.New("a terminal task accepted a decision")
		}
	}
	return nil
}

// deletedInputParks is the FAE-WF-031 property on a copy of the run: with a
// random required input's recorded artifacts deleted, the stage about to run
// never dispatches and parks naming exactly that input.
func deletedInputParks(c *genCase, j engine.Job) (bool, error) {
	s, ok := c.doc.Stages[string(j.State)]
	if !ok || j.InFlight != nil || j.Fan != nil {
		return false, nil
	}
	var required []workflow.Input
	for _, in := range s.Inputs {
		if !in.Optional {
			required = append(required, in)
		}
	}
	if len(required) == 0 || s.Primitive == workflow.Gate || s.Primitive == workflow.Branch || s.Primitive == workflow.Loop {
		return false, nil
	}
	in := required[c.index%len(required)]
	k, err := reload(j)
	if err != nil {
		return false, err
	}
	// Equal content from different stages shares an id; key on the record.
	gone := map[string]bool{}
	k.Evidence = slices.DeleteFunc(k.Evidence, func(a workflow.Evidence) bool {
		if a.Type == in.Type && (in.Stage == "" || a.Stage == in.Stage) {
			gone[a.ID+"\x00"+a.Stage+"\x00"+a.Session] = true
			return true
		}
		return false
	})
	if len(gone) == 0 {
		return false, nil
	}
	k.Provenance = slices.DeleteFunc(k.Provenance, func(p engine.Provenance) bool { return gone[p.ArtifactID+"\x00"+p.Stage+"\x00"+p.Session] })
	next, d, _, err := engine.Prepare(k)
	if err != nil || d != nil || !next.Parked || !strings.HasPrefix(next.Reason, "MissingInput: ") || !strings.Contains(next.Reason, in.Name) {
		return false, fmt.Errorf("%s ran or did not park naming %s after its input artifact was deleted: %q %v", j.State, in.Name, next.Reason, err)
	}
	deletionProbes.Add(1)
	return true, nil
}

var deletionProbes atomic.Int64
