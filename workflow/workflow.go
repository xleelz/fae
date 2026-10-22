// Package workflow is a pure, fail-closed graph validator and evidence interpreter.
package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/xleelz/fae/domain"
)

type Primitive string

const (
	AgentTurn      Primitive = "AgentTurn"
	Review         Primitive = "Review"
	Gate           Primitive = "Gate"
	HostCommand    Primitive = "HostCommand"
	FreezeArtifact Primitive = "FreezeArtifact"
	CompareFrozen  Primitive = "CompareFrozen"
	VcsCommit      Primitive = "VcsCommit"
	Merge          Primitive = "Merge"
	Integrate      Primitive = "Integrate"
	ExternalEffect Primitive = "ExternalEffect"
	Escalate       Primitive = "Escalate"
	HumanDecision  Primitive = "HumanDecision"
	Loop           Primitive = "Loop"
	Branch         Primitive = "Branch"
	FanOut         Primitive = "FanOut"
	FanIn          Primitive = "FanIn"
	SubWorkflow    Primitive = "SubWorkflow"
)

type Binding struct {
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	Effort       string `json:"effort,omitempty"`
	AutoApproval bool   `json:"auto_approval,omitempty"`
}
type Profile struct {
	Allowed []string `json:"allowed"`
	Denied  []string `json:"denied"`
	Build   bool     `json:"build"`
	Test    bool     `json:"test"`
	Format  bool     `json:"format"`
}
type Role struct {
	Instructions string            `json:"instructions"`
	Briefs       map[string]string `json:"briefs"`
	Routes       []Binding         `json:"routes"`
	Scope        string            `json:"scope"`
	Cap          int               `json:"cap"`
	Profile      Profile           `json:"profile"`
	Outputs      []string          `json:"outputs"`
}
type ArtifactType struct {
	Version int             `json:"version"`
	Schema  json.RawMessage `json:"schema"`
}
type Input struct {
	Optional bool   `json:"optional,omitempty"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Stage    string `json:"stage"`
	Version  int    `json:"version"`
}
type Stage struct {
	Reverify       string            `json:"reverify,omitempty"`
	Primitive      Primitive         `json:"primitive"`
	Role           string            `json:"role,omitempty"`
	Inputs         []Input           `json:"inputs,omitempty"`
	Output         string            `json:"output,omitempty"`
	Expr           *Expr             `json:"expression,omitempty"`
	Failure        string            `json:"failure,omitempty"`
	Default        string            `json:"default,omitempty"`
	MaxIterations  int               `json:"max_iterations,omitempty"`
	Command        []string          `json:"command,omitempty"`
	Weight         int               `json:"weight,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
	TestRun        bool              `json:"test_run,omitempty"`
	TestStage      string            `json:"test_stage,omitempty"`
	EmptyReason    string            `json:"empty_reason,omitempty"`
	EmptyAllowed   bool              `json:"empty_allowed,omitempty"`
	Protected      bool              `json:"protected,omitempty"`
	NormalOnly     bool              `json:"normal_only,omitempty"`
	Pins           map[string]string `json:"pins,omitempty"`
	IdempotenceKey string            `json:"idempotence_key,omitempty"`
	ChangeProducer bool              `json:"change_producer,omitempty"`
	// Reviewers declares isolated reviewer sessions. Count defaults to one.
	Reviewers []string `json:"reviewers,omitempty"`
	Count     int      `json:"count,omitempty"`
	Cases     []Case   `json:"cases,omitempty"`
	Subject   string   `json:"subject,omitempty"`
	Phase     string   `json:"phase,omitempty"`
}
type Case struct {
	When Expr   `json:"when"`
	Next string `json:"next"`
}
type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
}
type Limits struct {
	Attempts    int `json:"attempts"`
	TestRuns    int `json:"test_runs"`
	ScopeWidens int `json:"scope_widens"`
}
type Document struct {
	SchemaVersion int                     `json:"schema_version"`
	ID            string                  `json:"id"`
	Start         string                  `json:"start"`
	Roles         map[string]Role         `json:"roles"`
	Artifacts     map[string]ArtifactType `json:"artifacts"`
	Stages        map[string]Stage        `json:"stages"`
	Edges         []Edge                  `json:"edges"`
	Limits        Limits                  `json:"limits"`
	CodeBearing   bool                    `json:"code_bearing,omitempty"`
}
type Issue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (i Issue) Error() string { return i.Path + ": " + i.Message }
func Parse(raw []byte) (Document, error) {
	var d Document
	b, err := domain.CanonicalJSON(raw)
	if err != nil {
		return d, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err = dec.Decode(&d); err != nil {
		return d, err
	}
	issues := Validate(d)
	if len(issues) > 0 {
		return d, fmt.Errorf("invalid workflow: %v", issues)
	}
	return d, nil
}
func Hash(d Document) (string, error) {
	b, e := json.Marshal(d)
	if e != nil {
		return "", e
	}
	return domain.ContentHash(b)
}

// CanTransition handles graph legality only. Authorization, evidence, stale
// projections and atomic audit persistence must be checked by the caller.
func CanTransition(d Document, from, to domain.State) bool {
	if from.Terminal() || from == domain.Parked || to == domain.Parked {
		return false
	}
	if !known(d, from) || !known(d, to) {
		return false
	}
	if from == domain.Escalated {
		return to == domain.Retired || string(to) == d.Start
	}
	if to == domain.Escalated {
		return true
	}
	if to == domain.Retired {
		return false
	}
	if to == domain.Cancelled {
		return from == domain.Pending || from == domain.Blocked
	}
	if from == domain.Pending {
		return string(to) == d.Start
	}
	for _, e := range d.Edges {
		if e.From == string(from) && e.To == string(to) {
			return true
		}
	}
	return false
}
func known(d Document, s domain.State) bool {
	if s.Kernel() {
		return true
	}
	_, ok := d.Stages[string(s)]
	return ok
}
func primitive(p Primitive) bool {
	switch p {
	case AgentTurn, Review, Gate, HostCommand, FreezeArtifact, CompareFrozen, VcsCommit, Merge, Integrate, ExternalEffect, Escalate, HumanDecision, Loop, Branch, FanOut, FanIn, SubWorkflow:
		return true
	}
	return false
}
func Validate(d Document) []Issue {
	issues := []Issue{}
	bad := func(path, msg string) { issues = append(issues, Issue{path, msg}) }
	if d.SchemaVersion != 1 {
		bad("schema_version", "must be 1")
	}
	if d.ID == "" {
		bad("id", "required")
	}
	if _, ok := d.Stages[d.Start]; !ok {
		bad("start", "unknown stage")
	}
	if len(d.Stages) == 0 {
		bad("stages", "required")
	}
	if d.Roles == nil {
		bad("roles", "required")
	}
	if d.Artifacts == nil {
		bad("artifacts", "required")
	}
	if d.Limits.Attempts < 1 {
		bad("limits.attempts", "must be positive")
	}
	if d.Limits.TestRuns < 1 {
		bad("limits.test_runs", "must be positive")
	}
	if d.Limits.ScopeWidens < 1 {
		bad("limits.scope_widens", "must be positive")
	}
	for name, r := range d.Roles {
		p := "roles." + name
		if name == "" {
			bad(p, "empty role name")
		}
		if r.Instructions == "" || len(r.Briefs) == 0 {
			bad(p, "instructions and briefs required")
		}
		if r.Cap != -1 && r.Cap < 1 {
			bad(p+".cap", "positive or -1 required")
		}
		if r.Scope != "workflow-global" && r.Scope != "task-bound" {
			bad(p+".scope", "unknown scope")
		}
		if len(r.Routes) == 0 {
			bad(p+".routes", "required")
		}
		for i, b := range r.Routes {
			if b.Provider == "" || b.Model == "" {
				bad(fmt.Sprintf("%s.routes[%d]", p, i), "provider and model required")
			}
			switch b.Effort {
			case "", "minimal", "low", "medium", "high", "xhigh":
			default:
				bad(p+".routes", "unknown effort")
			}
		}
		for _, a := range r.Outputs {
			if _, ok := d.Artifacts[a]; !ok {
				bad(p+".outputs", "unknown artifact "+a)
			}
		}
	}
	for name, a := range d.Artifacts {
		if name == "" || a.Version < 1 || len(a.Schema) == 0 || !json.Valid(a.Schema) {
			bad("artifacts."+name, "name, positive version and JSON schema required")
		}
		if _, err := compileArtifact(a.Schema); err != nil {
			bad("artifacts."+name+".schema", err.Error())
		}
	}
	outgoing := map[string][]string{}
	for i, e := range d.Edges {
		p := fmt.Sprintf("edges[%d]", i)
		if !known(d, domain.State(e.From)) || !known(d, domain.State(e.To)) {
			bad(p, "unknown endpoint")
			continue
		}
		if domain.State(e.From).Kernel() && e.From != string(domain.Blocked) {
			bad(p, "kernel edges cannot be overridden")
		}
		if domain.State(e.To).Kernel() && e.To != string(domain.Done) && e.To != string(domain.Blocked) {
			bad(p, "kernel destination cannot be overridden")
		}
		outgoing[e.From] = append(outgoing[e.From], e.To)
	}
	producerRoles := map[string]bool{}
	for _, s := range d.Stages {
		if s.ChangeProducer {
			producerRoles[s.Role] = true
		}
	}
	for id, s := range d.Stages {
		p := "stages." + id
		if s.Reverify != "" {
			target, ok := d.Stages[s.Reverify]
			if (s.Primitive != Merge && s.Primitive != Integrate) || !ok || target.Primitive != Loop || !contains(outgoing[id], s.Reverify) {
				bad(p+".reverify", "must be an outgoing bounded Loop of a Merge or Integrate stage")
			}
		}
		if s.Phase != "" && s.Phase != "test-first" && s.Phase != "implement" {
			bad(p+".phase", "unknown phase")
		}
		if id == "" || domain.State(id).Kernel() {
			bad(p, "reserved or empty stage name")
		}
		if !primitive(s.Primitive) {
			bad(p+".primitive", "unknown primitive")
		}
		if s.Primitive == AgentTurn || s.Primitive == Review {
			if _, ok := d.Roles[s.Role]; !ok {
				bad(p+".role", "unknown role")
			}
			if s.Output == "" {
				bad(p+".output", "required")
			}
			if r, ok := d.Roles[s.Role]; ok && !contains(r.Outputs, s.Output) {
				bad(p+".output", "output not permitted by role")
			}
		}
		if s.Output != "" {
			if _, ok := d.Artifacts[s.Output]; !ok {
				bad(p+".output", "unknown artifact")
			}
		}
		for i, in := range s.Inputs {
			ip := fmt.Sprintf("%s.inputs[%d]", p, i)
			a, ok := d.Artifacts[in.Type]
			if !ok || in.Version != a.Version || in.Name == "" {
				bad(ip, "unknown artifact, wrong version, or missing name")
			}
			if in.Stage != "" {
				if _, ok := d.Stages[in.Stage]; !ok {
					bad(ip, "unknown producer stage")
				}
			}
		}
		if s.Primitive == Loop && s.MaxIterations < 1 {
			bad(p+".max_iterations", "finite positive cap required")
		}
		if s.Primitive == Branch {
			if s.Default == "" || !contains(outgoing[id], s.Default) {
				bad(p+".default", "default arm must be an edge")
			}
			for i, arm := range s.Cases {
				if err := ValidateExpr(arm.When); err != nil || !contains(outgoing[id], arm.Next) {
					bad(fmt.Sprintf("%s.cases[%d]", p, i), "invalid expression or missing edge")
				}
			}
		}
		if s.Primitive == HostCommand && (len(s.Command) == 0 || s.Command[0] == "" || s.Weight < 1 || s.TimeoutSeconds < 1) {
			bad(p, "command vector, positive weight and timeout required")
		}
		if s.Primitive == Gate {
			if s.Expr == nil {
				bad(p+".expression", "required")
			} else {
				if err := ValidateExpr(*s.Expr); err != nil {
					bad(p+".expression", err.Error())
				}
				if !(s.EmptyAllowed && s.EmptyReason != "") && !positiveCollections(*s.Expr) {
					bad(p+".expression", "must assert positive evidence on every passing arm")
				}
				if s.TestStage != "" {
					h, ok := d.Stages[s.TestStage]
					if !ok || h.Primitive != HostCommand || !h.TestRun || !testGate(*s.Expr, s.TestStage) {
						bad(p+".expression", "test gate must assert exit zero and executed_count >= 1")
					}
				}
			}
			if !contains(outgoing[id], s.Failure) {
				bad(p+".failure", "failure route must be an edge")
			}
		}
		if s.Primitive == Review && producerRoles[s.Role] {
			bad(p+".role", "reviewer must differ from every change producer")
		}
		if s.Count < 0 || s.Count > 16 {
			bad(p+".count", "review count must be between 0 and 16")
		}
		for _, role := range s.Reviewers {
			if _, ok := d.Roles[role]; !ok || producerRoles[role] {
				bad(p+".reviewers", "unknown or change-producing reviewer")
			}
			if r, ok := d.Roles[role]; ok && !contains(r.Outputs, s.Output) {
				bad(p+".reviewers", "output not permitted by reviewer role")
			}
		}
		if s.Primitive == ExternalEffect && (!s.NormalOnly || len(s.Pins) == 0 || s.IdempotenceKey == "") {
			bad(p, "external effect requires Normal-only mode, pins and idempotence key")
		}
		if s.Primitive == SubWorkflow || s.Primitive == FanOut || s.Primitive == FanIn || s.Primitive == HumanDecision {
			bad(p, "primitive is reserved but not supported by this implementation")
		}
	}
	if d.CodeBearing {
		issues = append(issues, validateRedPaths(d, outgoing)...)
	}
	reached := map[string]bool{}
	var visit func(string)
	visit = func(n string) {
		if reached[n] {
			return
		}
		reached[n] = true
		for _, to := range outgoing[n] {
			visit(to)
		}
	}
	visit(d.Start)
	for id := range d.Stages {
		if !reached[id] {
			bad("stages."+id, "unreachable")
		}
	}
	// Removing bounded Loop vertices must leave a DAG; otherwise a cycle can
	// avoid every iteration counter even if it belongs to a larger bounded SCC.
	colors := map[string]int{}
	var cycle func(string)
	cycle = func(n string) {
		if d.Stages[n].Primitive == Loop {
			return
		}
		if colors[n] == 1 {
			bad("stages."+n, "cycle avoids bounded Loop")
			return
		}
		if colors[n] == 2 {
			return
		}
		colors[n] = 1
		for _, to := range outgoing[n] {
			cycle(to)
		}
		colors[n] = 2
	}
	for id := range d.Stages {
		cycle(id)
	}
	// Explore product graph of node and safety facts, checking every path.
	type point struct {
		node         string
		host         string
		gate, review bool
	}
	seen := map[point]bool{}
	queue := []point{{node: d.Start}}
	for len(queue) > 0 {
		q := queue[0]
		queue = queue[1:]
		if seen[q] {
			continue
		}
		seen[q] = true
		s := d.Stages[q.node]
		if s.ChangeProducer {
			q.host = ""
			q.gate = false
			q.review = false
		}
		if s.Primitive == HostCommand && s.TestRun {
			q.host = q.node
		}
		if s.Primitive == Gate && q.host == s.TestStage && s.TestStage != "" && s.Expr != nil && testGate(*s.Expr, s.TestStage) {
			q.gate = true
		}
		if s.Primitive == Review && !producerRoles[s.Role] {
			q.review = true
		}
		if (s.Primitive == VcsCommit || s.Primitive == Integrate || s.Primitive == ExternalEffect) && (!q.gate || !q.review) {
			bad("stages."+q.node, "protected effect has a path without independent review and passing test gate")
		}
		for _, to := range outgoing[q.node] { // A failed gate does not confer a passing-gate safety fact.
			next := point{to, q.host, q.gate, q.review}
			if s.Primitive == Gate && to == s.Failure {
				next.gate = false
			}
			queue = append(queue, next)
		}
	}
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Path == issues[j].Path {
			return issues[i].Message < issues[j].Message
		}
		return issues[i].Path < issues[j].Path
	})
	unique := issues[:0]
	for _, i := range issues {
		if len(unique) == 0 || unique[len(unique)-1] != i {
			unique = append(unique, i)
		}
	}
	return unique
}
func contains(a []string, s string) bool {
	for _, v := range a {
		if v == s {
			return true
		}
	}
	return false
}
func positive(x Expr) bool {
	if x.Op == "and" && len(x.Args) == 2 {
		return positive(x.Args[0]) || positive(x.Args[1])
	}
	if x.Op == "or" && len(x.Args) == 2 {
		return positive(x.Args[0]) && positive(x.Args[1])
	}
	if x.Op == "exists" {
		return true
	}
	if (x.Op == "gt" || x.Op == "ge" || x.Op == "eq") && len(x.Args) == 2 && (x.Args[0].Op == "count" || x.Args[0].Op == "field") && x.Args[1].Op == "literal" {
		n, ok := number(x.Args[1].Value)
		return ok && (x.Op == "gt" && n >= 0 || n >= 1)
	}
	return false
}
func conjunct(x Expr, predicate func(Expr) bool) bool {
	if predicate(x) {
		return true
	}
	return x.Op == "and" && len(x.Args) == 2 && (conjunct(x.Args[0], predicate) || conjunct(x.Args[1], predicate))
}
func testGate(x Expr, stage string) bool {
	field := func(x Expr, path string) bool {
		return x.Op == "field" && x.Stage == stage && len(x.Path) == 1 && x.Path[0] == path && !x.All
	}
	exit := conjunct(x, func(a Expr) bool {
		if a.Op != "eq" || len(a.Args) != 2 || !field(a.Args[0], "exit_code") || a.Args[1].Op != "literal" {
			return false
		}
		n, ok := number(a.Args[1].Value)
		return ok && n == 0
	})
	count := conjunct(x, func(a Expr) bool {
		if len(a.Args) != 2 || !field(a.Args[0], "executed_count") || a.Args[1].Op != "literal" {
			return false
		}
		n, ok := number(a.Args[1].Value)
		return ok && (a.Op == "ge" && n >= 1 || a.Op == "gt" && n >= 0 || a.Op == "eq" && n >= 1)
	})
	return exit && count
}

// CheckInputs is the no-bypass admission check. Evidence must be task-local,
// version-matched, and provenance-validated; an artifact of another task is
// never a substitute for a missing input.
func CheckInputs(stage Stage, task string, evidence []Evidence) []string {
	missing := []string{}
	for _, in := range stage.Inputs {
		found := false
		for _, a := range evidence {
			if a.ID != "" && a.TaskID == task && a.Type == in.Type && a.Version == in.Version && a.Provenance && (in.Stage == "" || a.Stage == in.Stage) {
				found = true
				break
			}
		}
		if !found && !in.Optional {
			missing = append(missing, in.Name)
		}
	}
	return missing
}

// Every referenced collection needs its own non-vacuity witness on each
// passing disjunct; a positive count for A cannot justify an empty B.
func positiveCollections(x Expr) bool {
	selectors := map[string]bool{}
	var collect func(Expr)
	collect = func(e Expr) {
		if e.Op == "field" || e.Op == "count" || e.Op == "exists" {
			selectors[e.Type+"\x00"+e.Stage] = true
		}
		for _, a := range e.Args {
			collect(a)
		}
	}
	collect(x)
	if len(selectors) == 0 {
		return false
	}
	var proves func(Expr, string) bool
	proves = func(e Expr, key string) bool {
		if len(e.Args) == 2 && e.Op == "and" {
			return proves(e.Args[0], key) || proves(e.Args[1], key)
		}
		if len(e.Args) == 2 && e.Op == "or" {
			return proves(e.Args[0], key) && proves(e.Args[1], key)
		}
		if e.Op == "exists" {
			return e.Type+"\x00"+e.Stage == key
		}
		if len(e.Args) == 2 && e.Args[0].Type+"\x00"+e.Args[0].Stage == key {
			if e.Args[0].Op == "field" {
				switch e.Op {
				case "eq", "ne", "lt", "le", "gt", "ge", "prefix", "in":
					return true
				}
			}
			return positive(e)
		}
		return false
	}
	for key := range selectors {
		if !proves(x, key) {
			return false
		}
	}
	return true
}
