package workflow_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/workflow"
)

// FAE-WF-070, FAE-VER-004: a generated property test per kernel floor. Each
// case takes a valid base (the shipped default or a random protected graph),
// generates content that tries to weaken one floor in a random way, and
// requires validation to refuse it with the floor's own rule. Floors 2, 4 and 5
// have no content switch: content can only reach them through the levers
// listed in their families (pinned child revisions and revision-shaped keys;
// HostCommand environment, profile overrides and fan regions; command weight,
// timeout and role caps), and every other attempt is an unknown key that strict
// parsing refuses. Every refused mutation also changes the content hash, so a
// weakened document can never pass as the revision it was derived from.
const (
	floorSeed  = 0x070F
	floorCases = 250 // per floor
)

// floorBase is a valid base and the stage roles a weakening may target.
type floorBase struct {
	doc       workflow.Document
	producer  string   // the change-producing role
	review    string   // the independent review before the protected effects
	effects   []string // VcsCommit, Integrate and ExternalEffect stages
	external  string   // an ExternalEffect stage, if any
	preReview []string // turns and commands on paths before the review
	testGates []string
	chains    [][]string // Loop-free forward chains (a back edge makes a cycle)
	fanned    bool
}

type floorWant struct{ path, message string }

// floorFamily weakens one floor in a random way and names the rule that must
// refuse it. A family returns ok=false when the base offers no target.
type floorFamily func(r *rand.Rand, b *floorBase, d *workflow.Document) (want floorWant, ok bool)

func TestKernelFloorWeakeningsAreRefused(t *testing.T) {
	floors := []struct {
		name     string
		families []floorFamily
	}{
		{"1 fail-closed parsing and validation", nil}, // JSON-level, below
		{"2 immutable molecules and revisions", []floorFamily{floorUnpinnedChild}},
		{"3 audited transitions", []floorFamily{floorKernelEdge, floorKernelDestination, floorUnknownEndpoint}},
		{"4 task worktree isolation", []floorFamily{floorReservedEnvironment, floorWidenedProfile, floorFanProducer}},
		{"5 host guard and leases", []floorFamily{floorUnleasedCommand, floorRoleCap}},
		{"6 attempt caps", []floorFamily{floorTaskLimits, floorUncappedLoop, floorUnboundedCycle}},
		{"7 no-bypass", []floorFamily{floorUnresolvableInput, floorUnknownSelector}},
		{"8 positive-evidence gates", []floorFamily{floorVacuousGate, floorUnboundedTestGate}},
		{"9 protected-commit floor", []floorFamily{floorBypassReview, floorSelfReview, floorFailedGateRoute, floorLateChange}},
		{"10 external-effect floor", []floorFamily{floorUnpinnedEffect, floorUngatedEffect}},
	}
	for f, floor := range floors {
		t.Run(floor.name, func(t *testing.T) {
			t.Parallel()
			rng := rand.New(rand.NewSource(floorSeed + int64(f)))
			refused := map[string]int{}
			for i := 0; i < floorCases; i++ {
				b := floorBaseFor(rng, i)
				before, _ := workflow.Hash(b.doc)
				if f == 0 {
					want, raw := floorParseAttack(rng, b.doc)
					refused[want.message]++
					if _, err := workflow.Parse(raw); !floorRefused(err, want) {
						t.Fatalf("case %d: parse accepted %v at %s: %v", i, want.message, want.path, err)
					}
					continue
				}
				d := floorClone(b.doc)
				var want floorWant
				ok := false
				for tries := 0; !ok && tries < 20; tries++ {
					d = floorClone(b.doc)
					want, ok = floor.families[rng.Intn(len(floor.families))](rng, &b, &d)
				}
				if !ok {
					t.Fatalf("case %d: no weakening applies to base %s", i, b.doc.ID)
				}
				refused[want.message]++
				issues := workflow.Validate(d)
				if !floorHas(issues, want) {
					t.Fatalf("case %d (base %s): weakening not refused by %s: %s; issues %v", i, b.doc.ID, want.path, want.message, issues)
				}
				// The same weakening is refused through strict parsing, and
				// it changed the content address.
				raw, err := json.Marshal(d)
				if err == nil {
					_, err = workflow.Parse(raw)
				}
				if !floorRefused(err, want) {
					t.Fatalf("case %d: parse accepted the weakened document: %v", i, err)
				}
				if after, _ := workflow.Hash(d); after == before {
					t.Fatalf("case %d: weakened document kept the base hash", i)
				}
			}
			if len(refused) < 2 {
				t.Errorf("floor %s exercised too few rules: %v", floor.name, refused)
			}
			t.Logf("%d refusals by rule: %v", floorCases, refused)
		})
	}
}

func floorHas(issues []workflow.Issue, want floorWant) bool {
	for _, i := range issues {
		if (i.Path == want.path || strings.HasSuffix(want.path, ".") && strings.HasPrefix(i.Path, want.path)) && strings.Contains(i.Message, want.message) {
			return true
		}
	}
	return false
}

func floorRefused(err error, want floorWant) bool {
	var v *workflow.ValidationError
	if !errors.As(err, &v) {
		return err != nil && want.path == ""
	}
	return want.path == "" || floorHas(v.Issues, want)
}

func floorClone(d workflow.Document) workflow.Document {
	raw, _ := json.Marshal(d)
	var out workflow.Document
	_ = json.Unmarshal(raw, &out)
	return out
}

func floorBaseFor(r *rand.Rand, i int) floorBase {
	if i%4 == 0 {
		return floorBase{doc: defaults.Workflow(), producer: "coder", review: "reviews", effects: []string{"commit", "integrate"},
			preReview: []string{"intake", "spec", "plan", "test-first", "run-red", "implement", "run-tests", "compare-red", "format"},
			testGates: []string{"green-gate", "qa-gate", "post-merge-gate"},
			chains:    [][]string{{"spec", "spec-review", "spec-gate"}, {"test-first", "run-red", "red-gate"}, {"implement", "run-tests", "compare-red", "green-gate", "format", "format-gate", "reviews", "reviews-gate", "oracle"}}}
	}
	return floorGenerated(r, i)
}

// floorGenerated is a random valid protected graph: planning turns (one in a
// bounded retry), a change, a test run and its gate, optional notes, lint
// runs, a fan of reviews and extra gates, an independent review (perhaps
// aggregated), a protected commit or integration, and perhaps an external
// effect.
func floorGenerated(r *rand.Rand, i int) floorBase {
	role := func(outputs ...string) workflow.Role {
		return workflow.Role{Instructions: "generated", Briefs: map[string]string{"default": "brief"}, Routes: []workflow.Binding{{Provider: "scripted", Model: "fixture"}}, Scope: "task-bound", Cap: 1 + r.Intn(3), Profile: workflow.Profile{Allowed: []string{"read", "handoff"}, Denied: []string{"network"}}, Outputs: outputs}
	}
	schema := json.RawMessage(`{"type":"object"}`)
	d := workflow.Document{SchemaVersion: 1, ID: fmt.Sprintf("floor-base-%d", i), Limits: workflow.Limits{Attempts: 1 + r.Intn(20), TestRuns: 1 + r.Intn(8), ScopeWidens: 1 + r.Intn(3)},
		Roles:     map[string]workflow.Role{"planner": role("Note"), "coder": role("Change"), "reviewer": role("Verdict"), "auditor": role("Verdict")},
		Artifacts: map[string]workflow.ArtifactType{}, Stages: map[string]workflow.Stage{"stop": {Primitive: workflow.Escalate}}}
	for _, a := range []string{"Note", "Change", "Run", "Verdict", "Receipt", "Effect"} {
		d.Artifacts[a] = workflow.ArtifactType{Version: 1, Schema: schema}
	}
	b := floorBase{producer: "coder", review: "review"}
	var order []string
	add := func(id string, s workflow.Stage) {
		d.Stages[id] = s
		if len(order) > 0 {
			d.Edges = append(d.Edges, workflow.Edge{From: order[len(order)-1], To: id})
		}
		order = append(order, id)
	}
	chain := []string{}
	cut := func() {
		if len(chain) > 1 {
			b.chains = append(b.chains, chain)
		}
		chain = nil
	}
	for k := 0; k < 1+r.Intn(3); k++ {
		id := fmt.Sprintf("plan%d", k)
		if k == 0 && r.Intn(2) == 0 {
			add("plan-loop", workflow.Stage{Primitive: workflow.Loop, MaxIterations: 1 + r.Intn(4)})
			add(id, workflow.Stage{Primitive: workflow.AgentTurn, Role: "planner", Output: "Note"})
			x := eqField(id, "ok", true)
			add("plan-gate", workflow.Stage{Primitive: workflow.Gate, Expr: &x, Failure: "plan-loop"})
			d.Edges = append(d.Edges, workflow.Edge{From: "plan-gate", To: "plan-loop"})
			b.preReview = append(b.preReview, id)
			chain = append(chain, id, "plan-gate")
			continue
		}
		add(id, workflow.Stage{Primitive: workflow.AgentTurn, Role: "planner", Output: "Note"})
		b.preReview = append(b.preReview, id)
		chain = append(chain, id)
	}
	cut()
	fix := r.Intn(2) == 0
	if fix {
		add("fix", workflow.Stage{Primitive: workflow.Loop, MaxIterations: 1 + r.Intn(5)})
	}
	add("change", workflow.Stage{Primitive: workflow.AgentTurn, Role: "coder", Output: "Change", ChangeProducer: true, Inputs: []workflow.Input{{Name: "plan", Type: "Note", Stage: "plan0", Version: 1}}})
	add("test", workflow.Stage{Primitive: workflow.HostCommand, Output: "Run", Command: []string{"go", "test", "-json", "-count=1", "./..."}, Weight: 1 + r.Intn(2), TimeoutSeconds: 60 + r.Intn(600), TestRun: true})
	failure := "stop"
	if fix {
		failure = "fix"
	}
	g := and(eqField("test", "exit_code", 0), workflow.Expr{Op: "ge", Args: []workflow.Expr{{Op: "field", Stage: "test", Path: []string{"executed_count"}}, lit(1)}})
	add("test-gate", workflow.Stage{Primitive: workflow.Gate, Expr: &g, TestStage: "test", Failure: failure})
	d.Edges = append(d.Edges, workflow.Edge{From: "test-gate", To: failure})
	b.preReview = append(b.preReview, "change", "test")
	b.testGates = append(b.testGates, "test-gate")
	chain = append(chain, "change", "test", "test-gate")
	for k := 0; k < r.Intn(3); k++ {
		switch r.Intn(3) {
		case 0:
			id := fmt.Sprintf("note%d", k)
			add(id, workflow.Stage{Primitive: workflow.AgentTurn, Role: "planner", Output: "Note", Inputs: []workflow.Input{{Name: "change", Type: "Change", Stage: "change", Version: 1}}})
			b.preReview = append(b.preReview, id)
			chain = append(chain, id)
		case 1:
			id := fmt.Sprintf("lint%d", k)
			add(id, workflow.Stage{Primitive: workflow.HostCommand, Output: "Run", Command: []string{"make", "lint"}, Weight: 1, TimeoutSeconds: 120, Environment: map[string]string{"LINT_LEVEL": "strict"}})
			b.preReview = append(b.preReview, id)
			chain = append(chain, id)
		default:
			fan, join := fmt.Sprintf("fan%d", k), fmt.Sprintf("join%d", k)
			a, c := fmt.Sprintf("look%d", k), fmt.Sprintf("audit%d", k)
			add(fan, workflow.Stage{Primitive: workflow.FanOut, Join: join, Branches: []workflow.FanBranch{{ID: "look", Start: a}, {ID: "audit", Start: c}}})
			d.Stages[a] = workflow.Stage{Primitive: workflow.AgentTurn, Role: "planner", Output: "Note"}
			d.Stages[c] = workflow.Stage{Primitive: workflow.Review, Role: "auditor", Output: "Verdict"}
			d.Stages[join] = workflow.Stage{Primitive: workflow.FanIn, Rule: workflow.JoinAll, Failure: "stop"}
			d.Edges = append(d.Edges[:len(d.Edges)-1], workflow.Edge{From: order[len(order)-2], To: fan}, workflow.Edge{From: fan, To: a}, workflow.Edge{From: fan, To: c}, workflow.Edge{From: a, To: join}, workflow.Edge{From: c, To: join}, workflow.Edge{From: join, To: "stop"})
			order = append(order[:len(order)-1], join)
			cut()
			b.fanned = true
		}
	}
	review := workflow.Stage{Primitive: workflow.Review, Role: "reviewer", Output: "Verdict"}
	if r.Intn(3) == 0 {
		review.Reviewers = []string{"reviewer", "auditor"}
	}
	if r.Intn(3) == 0 {
		review.Aggregation, review.Failure = workflow.AllPass, failure
		d.Edges = append(d.Edges, workflow.Edge{From: "review", To: failure})
	}
	add("review", review)
	chain = append(chain, "review")
	if r.Intn(2) == 0 {
		x := workflow.Expr{Op: "ge", Args: []workflow.Expr{{Op: "count", Stage: "review", Args: []workflow.Expr{eqField("review", "approved", true)}}, lit(1)}}
		add("approval", workflow.Stage{Primitive: workflow.Gate, Expr: &x, Failure: "stop"})
		d.Edges = append(d.Edges, workflow.Edge{From: "approval", To: "stop"})
		chain = append(chain, "approval")
	}
	cut()
	if r.Intn(3) == 0 {
		add("land", workflow.Stage{Primitive: workflow.Integrate, Output: "Receipt"})
		b.effects = append(b.effects, "land")
	} else {
		add("commit", workflow.Stage{Primitive: workflow.VcsCommit, Output: "Receipt", Protected: true})
		b.effects = append(b.effects, "commit")
	}
	if r.Intn(2) == 0 {
		add("publish", workflow.Stage{Primitive: workflow.ExternalEffect, Output: "Effect", NormalOnly: true, Pins: map[string]string{"adapter": "review-host"}, IdempotenceKey: "publish-{task}"})
		b.effects = append(b.effects, "publish")
		b.external = "publish"
	}
	d.Start = order[0]
	d.Edges = append(d.Edges, workflow.Edge{From: order[len(order)-1], To: "Done"})
	if !strings.Contains(fmt.Sprint(d.Edges), " stop}") {
		delete(d.Stages, "stop")
	}
	b.doc = d
	if issues := workflow.Validate(d); len(issues) > 0 {
		panic(fmt.Sprintf("generated floor base %d is invalid: %v", i, issues))
	}
	return b
}

func floorPick[T any](r *rand.Rand, list []T) T { return list[r.Intn(len(list))] }
func floorStages(d *workflow.Document, keep func(workflow.Stage) bool) []string {
	ids := []string{}
	for id, s := range d.Stages {
		if keep(s) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// Floor 1: strict parsing. A random struct-typed object gains an unknown key,
// a random typed leaf gets a value of the wrong JSON type, or the document
// becomes malformed JSON or repeats a key.
func floorParseAttack(r *rand.Rand, d workflow.Document) (floorWant, []byte) {
	raw, _ := json.Marshal(d)
	var tree any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	_ = dec.Decode(&tree)
	var objects, leaves []floorSlot
	floorWalk(tree, reflect.TypeOf(d), "", nil, "", &objects, &leaves)
	switch r.Intn(6) {
	case 0:
		return floorWant{}, append([]byte(`{"id":"shadow",`), raw[1:]...) // a repeated key
	case 1:
		return floorWant{}, raw[:1+r.Intn(len(raw)-1)] // truncated
	case 2, 3:
		o := floorPick(r, objects)
		key := floorPick(r, []string{"mutable", "audit", "skip_floors", "unbounded", "isolation", "host_guard", "bypass", "revision", "molecule", "parent", "attempts", "Primitive"}) + fmt.Sprint(r.Intn(3))
		o.obj[key] = true
		out, _ := json.Marshal(tree)
		return floorWant{floorChild(o.path, key), "unknown field"}, out
	}
	l := floorPick(r, leaves)
	wrong := map[reflect.Kind]any{reflect.String: json.Number("7"), reflect.Bool: "yes", reflect.Int: "seven", reflect.Struct: []any{}, reflect.Slice: map[string]any{}, reflect.Map: []any{}}
	message := map[reflect.Kind]string{reflect.String: "expected string", reflect.Bool: "expected boolean", reflect.Int: "expected integer", reflect.Struct: "expected object", reflect.Slice: "expected array", reflect.Map: "expected object"}
	l.obj[l.key] = wrong[l.kind]
	out, _ := json.Marshal(tree)
	return floorWant{l.path, message[l.kind]}, out
}

type floorSlot struct {
	path, key string
	obj       map[string]any
	kind      reflect.Kind
}

func floorChild(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// floorWalk records every JSON object decoded into a struct (for unknown keys)
// and every typed struct field (for wrong types), with the parser's paths.
func floorWalk(v any, t reflect.Type, path string, parent map[string]any, key string, objects, leaves *[]floorSlot) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if v == nil || t == reflect.TypeOf(json.RawMessage(nil)) || t.Kind() == reflect.Interface {
		return
	}
	if parent != nil {
		switch t.Kind() {
		case reflect.String, reflect.Bool, reflect.Struct, reflect.Slice, reflect.Map:
			*leaves = append(*leaves, floorSlot{path: path, key: key, obj: parent, kind: t.Kind()})
		case reflect.Int:
			*leaves = append(*leaves, floorSlot{path: path, key: key, obj: parent, kind: reflect.Int})
		}
	}
	switch t.Kind() {
	case reflect.Struct:
		m, ok := v.(map[string]any)
		if !ok {
			return
		}
		*objects = append(*objects, floorSlot{path: path, obj: m})
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if child, ok := m[name]; ok && f.IsExported() {
				floorWalk(child, f.Type, floorChild(path, name), m, name, objects, leaves)
			}
		}
	case reflect.Map:
		if m, ok := v.(map[string]any); ok {
			for k, e := range m {
				floorWalk(e, t.Elem(), floorChild(path, k), nil, "", objects, leaves)
			}
		}
	case reflect.Slice:
		if list, ok := v.([]any); ok {
			for i, e := range list {
				floorWalk(e, t.Elem(), fmt.Sprintf("%s[%d]", path, i), nil, "", objects, leaves)
			}
		}
	}
}

// Floor 2: a sub-workflow binds a pinned child revision; content may not bind
// an unpinned, invalid or self revision.
func floorUnpinnedChild(r *rand.Rand, b *floorBase, d *workflow.Document) (floorWant, bool) {
	ref := floorPick(r, []workflow.WorkflowRef{{ID: "child", Revision: 0}, {ID: "child", Revision: -1 - r.Intn(3)}, {ID: "", Revision: 1}, {ID: d.ID, Revision: 1 + r.Intn(3)}})
	at := floorPick(r, b.preReview)
	d.Artifacts["Consult"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}
	d.Stages["consult"] = workflow.Stage{Primitive: workflow.SubWorkflow, Workflow: &ref, Output: "Consult", OutputMap: []workflow.ArtifactMap{{Name: "note", Stage: "draft"}}}
	if !floorSplice(d, at, "consult") {
		return floorWant{}, false
	}
	if ref.ID == d.ID {
		return floorWant{"stages.consult.workflow", "cannot run itself"}, true
	}
	return floorWant{"stages.consult.workflow", "pinned child workflow id and revision >= 1 required"}, true
}

// floorSplice inserts a single-route stage on at's success edge.
func floorSplice(d *workflow.Document, at, id string) bool {
	for i, e := range d.Edges {
		if e.From == at && e.To != d.Stages[at].Failure {
			d.Edges[i].From = id
			d.Edges = append(d.Edges, workflow.Edge{From: at, To: id})
			return true
		}
	}
	return false
}

var floorKernel = []string{"Pending", "Blocked", "Parked", "Escalated", "Done", "Retired", "Cancelled"}

// Floor 3: the kernel's audited edges cannot be overridden by content.
func floorKernelEdge(r *rand.Rand, b *floorBase, d *workflow.Document) (floorWant, bool) {
	from := floorPick(r, []string{"Pending", "Parked", "Escalated", "Done", "Retired", "Cancelled"})
	d.Edges = append(d.Edges, workflow.Edge{From: from, To: floorPick(r, floorStages(d, func(workflow.Stage) bool { return true }))})
	return floorWant{fmt.Sprintf("edges[%d]", len(d.Edges)-1), "kernel edges cannot be overridden"}, true
}
func floorKernelDestination(r *rand.Rand, b *floorBase, d *workflow.Document) (floorWant, bool) {
	to := floorPick(r, []string{"Pending", "Parked", "Escalated", "Retired", "Cancelled"})
	d.Edges = append(d.Edges, workflow.Edge{From: floorPick(r, b.preReview), To: to})
	return floorWant{fmt.Sprintf("edges[%d]", len(d.Edges)-1), "kernel destination cannot be overridden"}, true
}
func floorUnknownEndpoint(r *rand.Rand, b *floorBase, d *workflow.Document) (floorWant, bool) {
	e := workflow.Edge{From: floorPick(r, b.preReview), To: "ghost-" + fmt.Sprint(r.Intn(9))}
	if r.Intn(2) == 0 {
		e = workflow.Edge{From: e.To, To: e.From}
	}
	d.Edges = append(d.Edges, e)
	return floorWant{fmt.Sprintf("edges[%d]", len(d.Edges)-1), "unknown endpoint"}, true
}

// Floor 4: content reaches the worktree only through a HostCommand's explicit
// environment and stage profiles; it may not redirect Git, the instance, the
// tool paths or credentials, widen a role's profile, or produce change inside
// a parallel branch.
func floorReservedEnvironment(r *rand.Rand, b *floorBase, d *workflow.Document) (floorWant, bool) {
	hosts := floorStages(d, func(s workflow.Stage) bool { return s.Primitive == workflow.HostCommand })
	id := floorPick(r, hosts)
	name := floorPick(r, []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "FAE_WORKSPACE", "FAE_HEAVY_OP_PERMIT", "HOME", "PATH", "TMPDIR", "PWD", "LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "GH_TOKEN", "NPM_AUTH", "DB_PASSWORD", "AWS_PROFILE", "CLAUDE_CONFIG_DIR", "DOLT_ROOT_PATH", "LC_ALL"})
	stage(d, id, func(s *workflow.Stage) {
		if s.Environment == nil {
			s.Environment = map[string]string{}
		}
		s.Environment[name] = "/elsewhere"
	})
	return floorWant{"stages." + id + ".environment." + name, "non-reserved"}, true
}
func floorWidenedProfile(r *rand.Rand, b *floorBase, d *workflow.Document) (floorWant, bool) {
	turns := floorStages(d, func(s workflow.Stage) bool {
		return (s.Primitive == workflow.AgentTurn || s.Primitive == workflow.Review) && len(s.Reviewers) == 0
	})
	id := floorPick(r, turns)
	role := d.Roles[d.Stages[id].Role]
	p := workflow.Profile{Allowed: append([]string{}, role.Profile.Allowed...), Denied: append([]string{}, role.Profile.Denied...), Build: role.Profile.Build, Test: role.Profile.Test, Format: role.Profile.Format}
	switch widened := false; r.Intn(4) {
	case 0:
		if !role.Profile.Build {
			p.Build, widened = true, true
		}
		fallthrough
	case 1:
		if !widened && len(p.Denied) > 0 {
			p.Denied, widened = p.Denied[1:], true
		}
		fallthrough
	case 2:
		for _, c := range []string{"edit", "read", "handoff"} {
			if !widened && !contains(role.Profile.Allowed, c) {
				p.Allowed, widened = append(p.Allowed, c), true
			}
		}
		fallthrough
	default:
		if !widened {
			p.Test, p.Format = true, true
			if role.Profile.Test && role.Profile.Format {
				return floorWant{}, false
			}
		}
	}
	stage(d, id, func(s *workflow.Stage) { s.Profile = &p })
	return floorWant{"stages." + id + ".profile", "may only tighten"}, true
}
func floorFanProducer(r *rand.Rand, b *floorBase, d *workflow.Document) (floorWant, bool) {
	starts := []string{}
	for _, s := range d.Stages {
		for _, br := range s.Branches {
			if d.Stages[br.Start].Primitive == workflow.AgentTurn {
				starts = append(starts, br.Start)
			}
		}
	}
	if len(starts) == 0 {
		return floorWant{}, false
	}
	sort.Strings(starts)
	id := floorPick(r, starts)
	stage(d, id, func(s *workflow.Stage) { s.Role, s.Output, s.ChangeProducer = b.producer, "Change", true })
	return floorWant{"stages." + id, "holds no change producers"}, true
}

// Floor 5: every host command takes a positive lease weight and a bounded
// timeout; every role a positive (or explicitly unbounded) session cap.
func floorUnleasedCommand(r *rand.Rand, b *floorBase, d *workflow.Document) (floorWant, bool) {
	id := floorPick(r, floorStages(d, func(s workflow.Stage) bool { return s.Primitive == workflow.HostCommand }))
	stage(d, id, func(s *workflow.Stage) {
		switch r.Intn(4) {
		case 0:
			s.Weight = -r.Intn(3)
		case 1:
			s.TimeoutSeconds = -r.Intn(3)
		case 2:
			s.Command = nil
		default:
			s.Command = append([]string{""}, s.Command...)
		}
	})
	return floorWant{"stages." + id, "positive weight and timeout required"}, true
}
func floorRoleCap(r *rand.Rand, b *floorBase, d *workflow.Document) (floorWant, bool) {
	names := []string{}
	for n := range d.Roles {
		names = append(names, n)
	}
	sort.Strings(names)
	n := floorPick(r, names)
	role(d, n, func(x *workflow.Role) { x.Cap = floorPick(r, []int{0, -2, -3 - r.Intn(100)}) })
	return floorWant{"roles." + n + ".cap", "positive or -1 required"}, true
}

// Floor 6: per-task limits, a finite cap on every loop, and no cycle that
// avoids a bounded Loop.
func floorTaskLimits(r *rand.Rand, b *floorBase, d *workflow.Document) (floorWant, bool) {
	v := -r.Intn(3)
	switch r.Intn(3) {
	case 0:
		d.Limits.Attempts = v
		return floorWant{"limits.attempts", "must be positive"}, true
	case 1:
		d.Limits.TestRuns = v
		return floorWant{"limits.test_runs", "must be positive"}, true
	}
	d.Limits.ScopeWidens = v
	return floorWant{"limits.scope_widens", "must be positive"}, true
}
func floorUncappedLoop(r *rand.Rand, b *floorBase, d *workflow.Document) (floorWant, bool) {
	loops := floorStages(d, func(s workflow.Stage) bool { return s.Primitive == workflow.Loop })
	if len(loops) == 0 {
		return floorWant{}, false
	}
	id := floorPick(r, loops)
	stage(d, id, func(s *workflow.Stage) { s.MaxIterations = -r.Intn(3) })
	return floorWant{"stages." + id + ".max_iterations", "finite positive cap required"}, true
}
func floorUnboundedCycle(r *rand.Rand, b *floorBase, d *workflow.Document) (floorWant, bool) {
	if len(b.chains) == 0 {
		return floorWant{}, false
	}
	chain := floorPick(r, b.chains)
	i := r.Intn(len(chain) - 1)
	j := i + 1 + r.Intn(len(chain)-1-i)
	d.Edges = append(d.Edges, workflow.Edge{From: chain[j], To: chain[i]})
	return floorWant{"stages.", "cycle avoids bounded Loop"}, true
}

// Floor 7: every input binding must resolve to a declared artifact type and
// version, a known producer stage and a name; every gate selector must name
// a declared stage or type.
func floorUnresolvableInput(r *rand.Rand, b *floorBase, d *workflow.Document) (floorWant, bool) {
	turns := floorStages(d, func(s workflow.Stage) bool { return s.Primitive == workflow.AgentTurn })
	id := floorPick(r, turns)
	in := workflow.Input{Name: "context", Type: floorPick(r, []string{"Note", "Change", "Verdict"}), Stage: floorPick(r, turns), Version: 1}
	want := floorWant{fmt.Sprintf("stages.%s.inputs[%d]", id, len(d.Stages[id].Inputs)), "unknown artifact, wrong version, or missing name"}
	switch r.Intn(4) {
	case 0:
		in.Type = "Undeclared"
	case 1:
		in.Version = d.Artifacts[in.Type].Version + 1 + r.Intn(3)
	case 2:
		in.Name = ""
	default:
		in.Stage = "nowhere"
		want.message = "unknown producer stage"
	}
	stage(d, id, func(s *workflow.Stage) { s.Inputs = append(s.Inputs, in) })
	return want, true
}
func floorUnknownSelector(r *rand.Rand, b *floorBase, d *workflow.Document) (floorWant, bool) {
	gates := floorStages(d, func(s workflow.Stage) bool { return s.Primitive == workflow.Gate })
	id := floorPick(r, gates)
	sel := workflow.Expr{Op: "count", Stage: "ghost"}
	message := "unknown stage ghost"
	if r.Intn(2) == 0 {
		sel, message = workflow.Expr{Op: "count", Type: "Phantom"}, "unknown artifact type Phantom"
	}
	stage(d, id, func(s *workflow.Stage) {
		x := and(*s.Expr, workflow.Expr{Op: "ge", Args: []workflow.Expr{sel, lit(1)}})
		s.Expr = &x
	})
	return floorWant{"stages." + id + ".expression", message}, true
}

// Floor 8: a gate that could pass on empty evidence, or a test-run gate
// without a positive executed count, is refused even when marked empty-allowed
// without a reason.
func floorVacuousGate(r *rand.Rand, b *floorBase, d *workflow.Document) (floorWant, bool) {
	gates := floorStages(d, func(s workflow.Stage) bool { return s.Primitive == workflow.Gate && s.TestStage == "" })
	if len(gates) == 0 {
		return floorWant{}, false
	}
	id := floorPick(r, gates)
	sel := workflow.Expr{Op: "count", Stage: floorPick(r, floorStages(d, func(s workflow.Stage) bool { return s.Output != "" && s.Primitive != workflow.HostCommand }))}
	vacuous := []workflow.Expr{
		lit(true),
		{Op: "not", Args: []workflow.Expr{{Op: "exists", Stage: sel.Stage}}},
		{Op: "eq", Args: []workflow.Expr{sel, lit(0)}},
		{Op: "ge", Args: []workflow.Expr{sel, lit(0)}},
		{Op: "le", Args: []workflow.Expr{sel, lit(r.Intn(5))}},
		{Op: "lt", Args: []workflow.Expr{sel, lit(1 + r.Intn(5))}},
		{Op: "or", Args: []workflow.Expr{{Op: "ge", Args: []workflow.Expr{sel, lit(1)}}, lit(true)}},
		{Op: "or", Args: []workflow.Expr{{Op: "exists", Stage: sel.Stage}, {Op: "eq", Args: []workflow.Expr{sel, lit(0)}}}},
	}
	x := floorPick(r, vacuous)
	stage(d, id, func(s *workflow.Stage) {
		s.Expr = &x
		if r.Intn(3) == 0 {
			s.EmptyAllowed = true // without a reason
		}
	})
	return floorWant{"stages." + id + ".expression", "must assert positive evidence on every passing arm"}, true
}
func floorUnboundedTestGate(r *rand.Rand, b *floorBase, d *workflow.Document) (floorWant, bool) {
	id := floorPick(r, b.testGates)
	run := d.Stages[id].TestStage
	exit := eqField(run, "exit_code", 0)
	weak := floorPick(r, []workflow.Expr{
		exit,
		and(exit, workflow.Expr{Op: "ge", Args: []workflow.Expr{{Op: "field", Stage: run, Path: []string{"executed_count"}}, lit(0)}}),
		and(exit, workflow.Expr{Op: "exists", Stage: run}),
		and(exit, workflow.Expr{Op: "ge", Args: []workflow.Expr{{Op: "count", Stage: run}, lit(1)}}),
	})
	stage(d, id, func(s *workflow.Stage) {
		s.Expr = &weak
		if r.Intn(2) == 0 {
			s.EmptyAllowed, s.EmptyReason = true, "flaky suite"
		}
	})
	return floorWant{"stages." + id + ".expression", "executed_count >= 1"}, true
}

// Floor 9: on every path to a protected effect, a passing test gate and an
// independent review after the last change.
func floorBypassReview(r *rand.Rand, b *floorBase, d *workflow.Document) (floorWant, bool) {
	effect := floorPick(r, b.effects)
	d.Edges = append(d.Edges, workflow.Edge{From: floorPick(r, b.preReview), To: effect})
	return floorWant{"stages." + effect, "without independent review and passing test gate"}, true
}
func floorSelfReview(r *rand.Rand, b *floorBase, d *workflow.Document) (floorWant, bool) {
	if s := d.Stages[b.review]; len(s.Reviewers) > 0 && r.Intn(2) == 0 {
		stage(d, b.review, func(s *workflow.Stage) { s.Reviewers = append(s.Reviewers, b.producer) })
		return floorWant{"stages." + b.review + ".reviewers", "change-producing reviewer"}, true
	}
	stage(d, b.review, func(s *workflow.Stage) { s.Role = b.producer })
	return floorWant{"stages." + b.review + ".role", "reviewer must differ from every change producer"}, true
}
func floorFailedGateRoute(r *rand.Rand, b *floorBase, d *workflow.Document) (floorWant, bool) {
	// A failed test gate's route reaches the protected effect.
	id := floorPick(r, b.testGates)
	effect := floorPick(r, b.effects)
	stage(d, id, func(s *workflow.Stage) { s.Failure = effect })
	d.Edges = append(d.Edges, workflow.Edge{From: id, To: effect})
	return floorWant{"stages." + effect, "without independent review and passing test gate"}, true
}
func floorLateChange(r *rand.Rand, b *floorBase, d *workflow.Document) (floorWant, bool) {
	// A change after the review and before the protected effect.
	effect := b.effects[0]
	var before string
	for _, e := range d.Edges {
		if e.To == effect && d.Stages[e.From].Failure != effect {
			before = e.From
		}
	}
	output := floorPick(r, d.Roles[b.producer].Outputs)
	d.Stages["late-change"] = workflow.Stage{Primitive: workflow.AgentTurn, Role: b.producer, Output: output, ChangeProducer: true, Phase: d.Stages[floorPick(r, floorStages(d, func(s workflow.Stage) bool { return s.ChangeProducer }))].Phase}
	if before == "" || !floorSplice(d, before, "late-change") {
		return floorWant{}, false
	}
	return floorWant{"stages." + effect, "without independent review and passing test gate"}, true
}

// Floor 10: an external effect is Normal-only, pinned and idempotent, and
// gated like a protected commit on every path.
func floorUnpinnedEffect(r *rand.Rand, b *floorBase, d *workflow.Document) (floorWant, bool) {
	id := b.external
	if id == "" {
		id = "publish"
		d.Artifacts["Effect"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}
		d.Stages[id] = workflow.Stage{Primitive: workflow.ExternalEffect, Output: "Effect", NormalOnly: true, Pins: map[string]string{"adapter": "x"}, IdempotenceKey: "k"}
		if !floorSplice(d, b.effects[len(b.effects)-1], id) {
			return floorWant{}, false
		}
	}
	stage(d, id, func(s *workflow.Stage) {
		switch r.Intn(3) {
		case 0:
			s.NormalOnly = false
		case 1:
			s.Pins = nil
		default:
			s.IdempotenceKey = ""
		}
	})
	return floorWant{"stages." + id, "external effect requires Normal-only mode, pins and idempotence key"}, true
}
func floorUngatedEffect(r *rand.Rand, b *floorBase, d *workflow.Document) (floorWant, bool) {
	if b.external == "" {
		return floorWant{}, false
	}
	d.Edges = append(d.Edges, workflow.Edge{From: floorPick(r, b.preReview), To: b.external})
	return floorWant{"stages." + b.external, "without independent review and passing test gate"}, true
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
