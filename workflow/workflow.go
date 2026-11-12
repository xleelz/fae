// Package workflow is a pure, fail-closed graph validator and evidence interpreter.
package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/proof"
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
	ChangeRequest  Primitive = "ChangeRequest"
)

type Binding struct {
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	Effort       string `json:"effort,omitempty"`
	AutoApproval bool   `json:"auto_approval,omitempty"`
}
type Profile struct {
	ShellAllow   [][]string `json:"shell_allow,omitempty"`
	DenyTestRuns bool       `json:"deny_test_runs,omitempty"`
	Allowed      []string   `json:"allowed"`
	Denied       []string   `json:"denied"`
	Build        bool       `json:"build"`
	Test         bool       `json:"test"`
	Format       bool       `json:"format"`
	// Shims names the role command shims the session may run (FAE-WF-042);
	// absent or empty leaves every shim, each still bound by build, test and
	// format. Effects are the external-effect subjects (Section 9) the role
	// holds grants for; an ExternalEffect stage acts only under one.
	Shims   []string `json:"shims,omitempty"`
	Effects []string `json:"effects,omitempty"`
}
type Role struct {
	CapScope     string            `json:"cap_scope,omitempty"`
	Writes       []string          `json:"writes,omitempty"`
	Scratch      []string          `json:"scratch,omitempty"`
	Staging      string            `json:"staging,omitempty"`
	Instructions string            `json:"instructions"`
	Briefs       map[string]string `json:"briefs"`
	Routes       []Binding         `json:"routes"`
	Scope        string            `json:"scope"`
	Fresh        bool              `json:"fresh,omitempty"`
	Cap          int               `json:"cap"`
	Profile      Profile           `json:"profile"`
	Outputs      []string          `json:"outputs"`
	Session      string            `json:"session,omitempty"`
	TurnBudget   *TurnBudgetSpec   `json:"turn_budget,omitempty"`
	ReadRoots    []string          `json:"read_roots,omitempty"`
	Reads        *ReadPolicy       `json:"reads,omitempty"`
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
	Formal            *proof.Config     `json:"formal,omitempty"`
	TurnBudget        *TurnBudgetSpec   `json:"turn_budget,omitempty"`
	RunStats          string            `json:"run_stats,omitempty"`
	SchemaRetries     *int              `json:"schema_retries,omitempty"`
	OnChildEscalation string            `json:"on_child_escalation,omitempty"`
	OperatorMaySupply bool              `json:"operator_may_supply,omitempty"`
	ItemsFrom         *Projection       `json:"items_from,omitempty"`
	ItemOptions       []DecisionOption  `json:"item_options,omitempty"`
	Target            string            `json:"target,omitempty"`
	Artifact          string            `json:"artifact,omitempty"`
	Value             *Projection       `json:"value,omitempty"`
	Brief             string            `json:"brief,omitempty"`
	Writes            []string          `json:"writes,omitempty"`
	Scratch           []string          `json:"scratch,omitempty"`
	Staging           string            `json:"staging,omitempty"`
	Generate          *GenerateSpec     `json:"generate,omitempty"`
	Capture           []string          `json:"capture,omitempty"`
	Accept            bool              `json:"accept,omitempty"`
	Reverify          string            `json:"reverify,omitempty"`
	Primitive         Primitive         `json:"primitive"`
	Role              string            `json:"role,omitempty"`
	Inputs            []Input           `json:"inputs,omitempty"`
	Output            string            `json:"output,omitempty"`
	Expr              *Expr             `json:"expression,omitempty"`
	Failure           string            `json:"failure,omitempty"`
	BranchFailure     string            `json:"branch_failure,omitempty"`
	Default           string            `json:"default,omitempty"`
	MaxIterations     int               `json:"max_iterations,omitempty"`
	CommandAdapter    string            `json:"command_adapter,omitempty"`
	Command           []string          `json:"command,omitempty"`
	Weight            int               `json:"weight,omitempty"`
	TimeoutSeconds    int               `json:"timeout_seconds,omitempty"`
	TestRun           bool              `json:"test_run,omitempty"`
	TestStage         string            `json:"test_stage,omitempty"`
	TestShape         string            `json:"test_shape,omitempty"`
	EmptyReason       string            `json:"empty_reason,omitempty"`
	EmptyAllowed      bool              `json:"empty_allowed,omitempty"`
	Protected         bool              `json:"protected,omitempty"`
	NormalOnly        bool              `json:"normal_only,omitempty"`
	Pins              map[string]string `json:"pins,omitempty"`
	IdempotenceKey    string            `json:"idempotence_key,omitempty"`
	ChangeProducer    bool              `json:"change_producer,omitempty"`
	// Reviewers declares isolated reviewer sessions. Count defaults to one.
	Reviewers []string `json:"reviewers,omitempty"`
	Count     int      `json:"count,omitempty"`
	Cases     []Case   `json:"cases,omitempty"`
	Subject   string   `json:"subject,omitempty"`
	Phase     string   `json:"phase,omitempty"`
	// Kind is the Escalate stage's Appendix B kind; omitted means HumanDecisionRequired.
	Kind domain.EscalationKind `json:"kind,omitempty"`
	// FAE-WF-030 primitive configuration, validated in primitives.go.
	// HumanDecision: a declared question and options, each routed to an
	// outgoing edge; on_timeout is "stay-waiting" (never auto-decide).
	Question  string           `json:"question,omitempty"`
	Options   []DecisionOption `json:"options,omitempty"`
	OnTimeout string           `json:"on_timeout,omitempty"`
	// Escalate: reason template; {task}, {stage} and {attempt} are substituted.
	Reason string `json:"reason,omitempty"`
	// Review: aggregation rule (all-pass, k-of-n, any-blocking-fails) and the
	// k of k-of-n; a failed or unknown aggregate verdict takes the failure route.
	Aggregation string `json:"aggregation,omitempty"`
	Quorum      int    `json:"quorum,omitempty"`
	// Loop: the Gate whose failure re-enters it, the delay before each repeated
	// iteration, and the escalation kind raised when it is exhausted.
	ExitGate       string                `json:"exit_gate,omitempty"`
	BackoffSeconds int                   `json:"backoff_seconds,omitempty"`
	OnExhaustion   domain.EscalationKind `json:"on_exhaustion,omitempty"`
	// HostCommand: the expected result shape (a mismatch takes the failure
	// route) and explicit environment settings; nothing else is inherited.
	ExpectedShape *CommandShape     `json:"expected_shape,omitempty"`
	Environment   map[string]string `json:"environment,omitempty"`
	// AgentTurn and Review: a tool profile that may only tighten the role's.
	Profile *Profile `json:"profile,omitempty"`
	// Gate: human-readable name, recorded with every result.
	Name string `json:"name,omitempty"`
	// FanOut: the parallel branches and the FanIn that joins them (fan.go).
	Branches       []FanBranch       `json:"branches,omitempty"`
	BranchTemplate *FanBranch        `json:"branch_template,omitempty"`
	BranchesFrom   *BranchesFromSpec `json:"branches_from,omitempty"`
	Parallel       bool              `json:"parallel,omitempty"`
	WriteScope     []string          `json:"write_scope,omitempty"`
	Join           string            `json:"join,omitempty"`
	// FanIn: join rule all, any or k-of-n (with quorum); a failed join takes
	// the failure route.
	Rule string `json:"rule,omitempty"`
	// SubWorkflow: the pinned child workflow revision, parent artifacts bound
	// into the child task's content, and child artifacts mapped back into the
	// stage's output artifact (subworkflow.go).
	Workflow  *WorkflowRef  `json:"workflow,omitempty"`
	InputMap  []ArtifactMap `json:"input_map,omitempty"`
	OutputMap []ArtifactMap `json:"output_map,omitempty"`
	// Bindings types the command's {{name}} placeholders (FAE-WF-035).
	Bindings     map[string]ArgBinding  `json:"bindings,omitempty"`
	ArgTemplates map[string]ArgTemplate `json:"arg_templates,omitempty"`
	// Check configures a HostCommand stage's structured check mode (FAE-WF-095).
	Check *CheckSpec `json:"check,omitempty"`
	// Progress discounts a Loop iteration that reduces open findings.
	Progress *Progress `json:"progress,omitempty"`
	// FollowUps selects output elements recorded as follow-ups (followups.go).
	FollowUps *FollowUps `json:"follow_ups,omitempty"`
	// ReadOnlyFiles names repo-relative files bound as read-only inputs for
	// the stage's turns, separate from editable write_scope (FAE-RT-017).
	ReadOnlyFiles []string `json:"read_only_files,omitempty"`
	// Reads narrows the role's read policy for this stage (FAE-SEC-008).
	Reads *ReadPolicy `json:"reads,omitempty"`
	// EffectKind names an engine-run ExternalEffect such as "git-push" (FAE-EXT-023).
	EffectKind string `json:"effect_kind,omitempty"`
}

// WorkflowRef pins a child workflow by id and revision.
type WorkflowRef struct {
	ID       string `json:"id"`
	Revision int    `json:"revision"`
}

// ArtifactMap names one artifact selected by producing stage and/or type.
type ArtifactMap struct {
	As         string      `json:"as,omitempty"`
	Projection *Projection `json:"projection,omitempty"`
	Input      string      `json:"input,omitempty"`
	Output     string      `json:"output,omitempty"`
	Field      string      `json:"field,omitempty"`
	Name       string      `json:"name"`
	Stage      string      `json:"stage,omitempty"`
	Type       string      `json:"type,omitempty"`
}

// FanBranch is one parallel sub-path of a FanOut, entered at Start. Pass is
// the branch verdict, evaluated over the branch's evidence when it reaches the
// join; without it a branch passes unless it arrives by a failure route.
type FanBranch struct {
	Output     string   `json:"output,omitempty"`
	ID         string   `json:"id"`
	Start      string   `json:"start"`
	Pass       *Expr    `json:"pass,omitempty"`
	Verdict    string   `json:"verdict,omitempty"`
	WriteScope []string `json:"write_scope,omitempty"`
}

// CommandShape declares what a HostCommand's engine-run artifact must show:
// exit "zero" or "nonzero", an exact or minimum executed_count, and an exact
// matched_count (for example exactly one matched test). Zero means unset.
type CommandShape struct {
	Exit        string `json:"exit,omitempty"`
	Executed    int    `json:"executed,omitempty"`
	MinExecuted int    `json:"min_executed,omitempty"`
	Matched     int    `json:"matched,omitempty"`
}

// DecisionOption is one declared answer of a HumanDecision stage.
type DecisionOption struct {
	ID          string `json:"id"`
	Next        string `json:"next"`
	Description string `json:"description,omitempty"`
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
	maxSessionsSet             bool
	sessionCountSet            bool
	ValidationRecursionCeiling int                     `json:"-"`
	SlotScope                  string                  `json:"slot_scope,omitempty"`
	MaxSessions                int                     `json:"max_sessions,omitempty"`
	SessionCount               int                     `json:"session_count,omitempty"`
	Signature                  *Signature              `json:"signature,omitempty"`
	Recursion                  *Recursion              `json:"recursion,omitempty"`
	Produces                   string                  `json:"produces,omitempty"`
	Staging                    bool                    `json:"staging,omitempty"`
	SchemaVersion              int                     `json:"schema_version"`
	ID                         string                  `json:"id"`
	Start                      string                  `json:"start"`
	Roles                      map[string]Role         `json:"roles"`
	Artifacts                  map[string]ArtifactType `json:"artifacts"`
	Stages                     map[string]Stage        `json:"stages"`
	Edges                      []Edge                  `json:"edges"`
	Limits                     Limits                  `json:"limits"`
	CodeBearing                bool                    `json:"code_bearing,omitempty"`
	// MoleculeConcurrency bounds this workflow's running molecules (FAE-WF-001).
	MoleculeConcurrency *MoleculeConcurrency `json:"molecule_concurrency,omitempty"`
	// NoRedStages are the pre-code stages whose artifact may declare the
	// task no-red with a boolean no_red field (FAE-WF-073).
	NoRedStages []string `json:"no_red_stages,omitempty"`
	// TaskClasses are the task-class overlays (FAE-WF-001, FAE-WF-044); a
	// task admitted without a class takes DefaultClass.
	TaskClasses  map[string]TaskClass `json:"task_classes,omitempty"`
	DefaultClass string               `json:"default_class,omitempty"`
	// Requests governs scope-widen and RED-rework requests (FAE-WF-078).
	Requests *RequestPolicy `json:"requests,omitempty"`
	// TargetRequirements are the default target-configuration requirements
	// (FAE-WF-001), required from schema 2 (profile.go).
	TargetRequirements *TargetRequirements `json:"target_requirements,omitempty"`
}

// MoleculeConcurrency is the content's own running-molecule maximum; −1 is
// unbounded. The scheduler applies the tighter of it and the operator's
// per-workflow limit (FAE-SCH-007).
type MoleculeConcurrency struct {
	MaxRunning int `json:"max_running"`
}

// MaxRunningMolecules is the declared maximum, −1 when the section is absent.
func MaxRunningMolecules(d Document) int {
	if d.MoleculeConcurrency == nil {
		return -1
	}
	return d.MoleculeConcurrency.MaxRunning
}

type Issue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (i Issue) Error() string { return i.Path + ": " + i.Message }

// Parse decodes canonical JSON strictly. Unknown keys and wrong types are
// reported with their paths together with every semantic issue; a document
// with structural issues is still decoded leniently so that one refusal lists
// all errors (*ValidationError).
func Parse(raw []byte) (Document, error) {
	return ParseWithRecursionCeiling(raw, 0)
}

func ParseWithRecursionCeiling(raw []byte, ceiling int) (Document, error) {
	d, issues, err := decode(raw)
	if err != nil {
		return d, err
	}
	d.ValidationRecursionCeiling = ceiling
	issues = append(issues, Validate(d)...)
	if len(issues) > 0 {
		return d, &ValidationError{Issues: sortIssues(issues)}
	}
	return d, nil
}

// ParseWrite strictly validates newly authored content without tightening the
// compatibility rules Parse and Load apply to stored revisions.
func ParseWrite(raw []byte) (Document, error) {
	return ParseWriteWithRecursionCeiling(raw, 0)
}

func ParseWriteWithRecursionCeiling(raw []byte, ceiling int) (Document, error) {
	d, issues, err := decode(raw)
	if err != nil {
		return d, err
	}
	d.ValidationRecursionCeiling = ceiling
	issues = append(issues, ValidateWrite(d)...)
	if len(issues) > 0 {
		return d, &ValidationError{Issues: sortIssues(issues)}
	}
	// Pin defaults into new revisions; stored documents keep the recovery
	// policy they were authored with under Parse/Load compatibility.
	for id, s := range d.Stages {
		if s.Primitive == AgentTurn && s.SchemaRetries == nil {
			n := 1
			s.SchemaRetries = &n
			d.Stages[id] = s
		}
	}
	return d, nil
}

// Load decodes a stored revision as Parse does but applies only the rules
// that say whether this binary can read it: canonical JSON, no unknown keys
// or wrong types, and a schema version it knows. It is the first of the two
// classes a stored revision falls in (FAE-WF-093, operator decision of
// 2026-10-30): unreadable (fails Load) or unrunnable (loads but fails
// Parse: a structural rule the engine relies on, such as a Gate's expr, the
// start and edge endpoints, primitives and Loop caps, or a rule added after
// it was stored, such as brief keys, caps and floors). Load carries none of
// the structural rules on purpose: reading and running are different
// questions, and the daemon never drives a task on a document that fails
// Parse. It refuses to start while an active task is pinned to an unrunnable
// revision, or quarantines those tasks under
// `serve --quarantine-unrunnable-revisions`; an unrunnable revision no active
// task is pinned to is listed, and admission and migration, which Parse,
// refuse it.
func Load(raw []byte) (Document, error) {
	d, issues, err := decode(raw)
	if err != nil {
		return d, err
	}
	if d.SchemaVersion != 1 && d.SchemaVersion != CurrentSchema {
		issues = append(issues, Issue{"schema_version", "must be 1 or 2"})
	}
	if len(issues) > 0 {
		return d, &ValidationError{Issues: sortIssues(issues)}
	}
	return d, nil
}

// decode is Parse's decoding: the document and its structural issues, or the
// error of content that is not canonical JSON.
func decode(raw []byte) (Document, []Issue, error) {
	var d Document
	b, err := domain.CanonicalJSON(raw)
	if err != nil {
		return d, nil, err
	}
	var generic any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err = dec.Decode(&generic); err != nil {
		return d, nil, err
	}
	issues := []Issue{}
	structural(generic, reflect.TypeOf(d), "", func(path, msg string) { issues = append(issues, Issue{path, msg}) })
	dec = json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if len(issues) == 0 {
		dec.DisallowUnknownFields()
	}
	if err = dec.Decode(&d); err != nil && len(issues) == 0 {
		return d, nil, err
	}
	return d, issues, nil
}

// CurrentSchema is the schema every newly written revision uses. Schema 2
// makes the FAE-WF-030 configuration a Gate name and an Escalate kind and
// reason required; schema 1 revisions stay loadable without them (FAE-WF-093).
const CurrentSchema = 2

// ValidateWrite is Validate for a newly written revision. Parse runs Validate
// on every stored revision, so a rule added after revisions were stored that
// only keeps new content runnable belongs here instead, where it cannot make
// a stored revision unloadable (FAE-WF-093): a new revision must use the
// current schema, so its primitives carry their required configuration, and
// may not bind an effort its route's provider refuses (RouteEffortIssues).
// ParseWrite is used by every new install and authoring-validation path.
func ValidateWrite(d Document) []Issue {
	issues := append(Validate(d), RouteEffortIssues(d)...)
	issues = append(issues, JoinOutputIssues(d)...)
	issues = append(issues, FootprintIssues(d)...)
	if d.SchemaVersion == 1 {
		issues = append(issues, Issue{"schema_version", "a new revision must use schema 2; schema 1 revisions stay loadable but are not written"})
	}
	return sortIssues(issues)
}

// RouteEffortIssues lists the route bindings, and the task-class overlays
// applied to them, whose effort the route's provider refuses (see
// providerRefusesEffort). It is a write rule, not a Validate rule: wave 9
// added it after revisions were stored, and a stored revision that breaks it
// still loads. Such a binding cannot run: the bridge's preflight refuses it
// before the provider starts, as a binding refusal, and the turn moves to the
// role's next route.
func RouteEffortIssues(d Document) []Issue {
	issues := []Issue{}
	bad := func(path, msg string) { issues = append(issues, Issue{path, msg}) }
	for name, r := range d.Roles {

		p := "roles." + name
		for i, b := range r.Routes {
			if providerRefusesEffort(b.Provider, b.Effort) {
				bad(fmt.Sprintf("%s.routes[%d]", p, i), "effort "+b.Effort+" is not supported by provider "+b.Provider)
			}
		}
	}
	overlayEffortIssues(d, bad)
	return sortIssues(issues)
}

func Hash(d Document) (string, error) {
	b, e := json.Marshal(d)
	if e != nil {
		return "", e
	}
	return domain.ContentHash(b)
}

// CanTransition is the one transition-legality function (FAE-WF-008): the
// bound content's stage edges plus exactly the kernel edges (FAE-WF-020/-021).
// Every non-terminal state reaches Escalated; Escalated reaches only Retired
// and the first stage (restart, also taken by an AttemptCap resume after a
// raise); Pending and Blocked reach Cancelled; Pending is released to the
// first stage, or to a carry entry when it carries accepted pre-code evidence
// (FAE-WF-086); a stage reaches Blocked (operator Block) and Blocked re-enters
// at a remediation route (FAE-OPS-010, Appendix C). Authorization, evidence,
// stale projections and atomic audit persistence must be checked by the caller.
func CanTransition(d Document, from, to domain.State, resumeStage ...string) bool {
	if from.Terminal() || from == domain.Parked || to == domain.Parked {
		return false
	}
	if !known(d, from) || !known(d, to) {
		return false
	}
	if from == domain.Escalated {
		return to == domain.Retired || string(to) == d.Start || len(resumeStage) == 1 && resumeStage[0] != "" && string(to) == resumeStage[0] && !to.Kernel()
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
		return string(to) == d.Start || CarryEntry(d, string(to))
	}
	if to == domain.Blocked && !from.Kernel() || from == domain.Blocked && remediation(d, string(to)) {
		return true
	}
	if fanReentry(d, string(from), string(to)) {
		return true
	}
	if BranchFailureJoin(d, string(from)) == string(to) && string(to) != "" {
		return true
	}
	if TurnBudgetExhaustionTarget(d, string(from)) == string(to) && string(to) != "" {
		return true
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
	case AgentTurn, Review, Gate, HostCommand, FreezeArtifact, CompareFrozen, VcsCommit, Merge, Integrate, ExternalEffect, Escalate, HumanDecision, Loop, Branch, FanOut, FanIn, SubWorkflow, ChangeRequest:
		return true
	}
	return false
}

// providerRefusesEffort reports an effort the workflow schema accepts but the
// provider's bridge refuses before the provider runs: Claude Code has no
// "minimal" effort (provider/claude.go). Such a binding could never run, and
// at run time it would only fail over to the role's next route. Writes refuse
// it (RouteEffortIssues); stored revisions holding it still load.
func providerRefusesEffort(provider, effort string) bool {
	return provider == "claude" && effort == "minimal"
}

func Validate(d Document) []Issue {
	issues := []Issue{}
	bad := func(path, msg string) { issues = append(issues, Issue{path, msg}) }
	if d.SchemaVersion != 1 && d.SchemaVersion != CurrentSchema {
		bad("schema_version", "must be 1 or 2")
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
	if n := MaxRunningMolecules(d); n != -1 && (n < 1 || n > 1000000) {
		bad("molecule_concurrency.max_running", "must be -1 (unbounded) or 1..1000000")
	}
	for name, r := range d.Roles {
		if r.CapScope != "" && r.CapScope != "workflow" && r.CapScope != "tree" && r.CapScope != "task" {
			bad("roles."+name+".cap_scope", "must be workflow, tree or task")
		}
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
		// Runtime bridges grant exactly these capabilities; denials are free.
		for _, c := range r.Profile.Allowed {
			if !ValidToolCapability(c) {
				bad(p+".profile.allowed", "unknown capability "+c)
			}
		}
	}
	// Brief templates are selected by stage phase, else "default"; any other key
	// can never be rendered (FAE-WF-040, FAE-WF-043).
	phases := map[string]map[string]bool{}
	for _, s := range d.Stages {
		if s.Primitive != AgentTurn && s.Primitive != Review {
			continue
		}
		for _, role := range stageRoles(s) {
			if phases[role] == nil {
				phases[role] = map[string]bool{}
			}
			phases[role][s.Phase] = true
		}
	}
	for name, r := range d.Roles {
		for key, text := range r.Briefs {
			p := "roles." + name + ".briefs." + key
			if key != "default" && (key == "" || !phases[name][key]) {
				bad(p, "brief key must be default or a declared phase of a stage using this role")
			}
			if strings.TrimSpace(text) == "" {
				bad(p, "empty brief template")
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
		if ProducesChanges(d, s) {
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
		if s.Kind != "" && (s.Primitive != Escalate || !s.Kind.Valid()) {
			bad(p+".kind", "escalation kind must be an Appendix B kind on an Escalate stage")
		}
		if id == "" || id == "detached" || domain.State(id).Kernel() || strings.Contains(id, ".") {
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
			for _, role := range stageRoles(s) {
				if r, ok := d.Roles[role]; ok && r.Briefs[s.Phase] == "" && r.Briefs["default"] == "" {
					bad(p+".role", fmt.Sprintf("role %s has no brief template for phase %s", role, phaseName(s.Phase)))
				}
			}
		}
		if s.TestShape != "" && !testShapeValid(s) {
			bad(p+".test_shape", "declared test shape requires a test-run HostCommand of a supported form ("+strings.Join(TestShapes, ", ")+") matching its command")
		}
		if s.Output != "" {
			if _, ok := d.Artifacts[s.Output]; !ok {
				bad(p+".output", "unknown artifact")
			}
		}
		switch s.Primitive {
		case HostCommand, FreezeArtifact, CompareFrozen, VcsCommit, Merge, Integrate, ExternalEffect:
			// The engine records every dispatch result as a typed artifact.
			if s.Output == "" {
				bad(p+".output", "required")
			}
		}
		for i, in := range s.Inputs {
			ip := fmt.Sprintf("%s.inputs[%d]", p, i)
			a, ok := ArtifactDeclaration(d, in.Type)
			if !ok || in.Version != a.Version || in.Name == "" {
				bad(ip, "unknown artifact, wrong version, or missing name")
			}
			if in.Stage != "" {
				if _, ok := ProducerDeclaration(d, in.Stage); !ok {
					bad(ip, "unknown producer stage")
				}
			}
		}
		if s.BranchFailure != "" && (s.Primitive != FanOut || s.BranchFailure != "fail-branch") {
			bad(p+".branch_failure", "only FanOut branch_failure: fail-branch is supported")
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
		validateBindings(d, id, s, bad)
		validateCheck(d, id, s, bad)
		validateProgress(d, id, s, bad)
		if s.Primitive == Gate && s.Expr != nil {
			selectors(d, *s.Expr, p+".expression", bad)
		}
		for i, arm := range s.Cases {
			selectors(d, arm.When, fmt.Sprintf("%s.cases[%d]", p, i), bad)
		}
		if (s.Primitive == FreezeArtifact || s.Primitive == CompareFrozen) && s.Subject != "" {
			if _, ok := d.Stages[s.Subject]; !ok {
				bad(p+".subject", "unknown stage "+s.Subject)
			}
		}
		if s.Primitive == Gate {
			if s.Expr == nil {
				bad(p+".expression", "required")
			} else {
				if err := ValidateExpr(*s.Expr); err != nil {
					bad(p+".expression", err.Error())
				}
				if !(s.EmptyAllowed && s.EmptyReason != "") && !positiveCollections(*s.Expr) && !FormalPositiveGate(d, *s.Expr) {
					bad(p+".expression", "must assert positive evidence on every passing arm")
				}
				if s.TestStage != "" {
					h, ok := d.Stages[s.TestStage]
					if !ok || h.Primitive != HostCommand || (!h.TestRun && h.Check == nil && h.Formal == nil) || !testOrCheckGate(*s.Expr, s.TestStage, h) {
						bad(p+".expression", "test gate must assert exit zero and executed_count >= 1")
					}
				}
				// Any gate reading test-run evidence needs a positive executed
				// count for that selector, whether or not it names test_stage.
				for _, sel := range testRunSelectors(d, *s.Expr) {
					if !executedBound(*s.Expr, sel) {
						bad(p+".expression", "test-run gate must assert executed_count >= 1 for "+sel.label())
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
		if s.Primitive == ExternalEffect && !s.IsGitPush() && (!s.NormalOnly || len(s.Pins) == 0 || s.IdempotenceKey == "") {
			bad(p, "external effect requires Normal-only mode, pins and idempotence key")
		}
	}
	validatePrimitives(d, outgoing, bad)
	validateRecoveryExtensions(d, outgoing, bad)
	validateFormal(d, bad)
	validateCommandAdapters(d, bad)
	validateFans(d, outgoing, bad)
	validateSignature(d, bad)
	validateSubWorkflows(d, outgoing, bad)
	validateClasses(d, outgoing, phases, bad)
	validateRequests(d, outgoing, bad)
	validateGrants(d, bad)
	validateFollowUps(d, bad)
	validateAgentic(d, outgoing, bad)
	validateFilesystem(d, bad)
	validateBriefTemplates(d, bad)
	validateReadsAndPush(d, bad)
	if anyCodeBearing(d) {
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
	ids := make([]string, 0, len(d.Stages))
	for id := range d.Stages {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		cycle(id)
	}
	// FAE-WF-090: every stage the engine would leave by its success route has
	// exactly one, and every non-Escalate stage can still reach Done.
	toDone := map[string]bool{string(domain.Done): true}
	for changed := true; changed; {
		changed = false
		for _, e := range d.Edges {
			if toDone[e.To] && !toDone[e.From] {
				toDone[e.From] = true
				changed = true
			}
		}
	}
	for _, id := range ids {
		s := d.Stages[id]
		if s.Primitive == Escalate || s.Primitive == Branch || !primitive(s.Primitive) || IsRequestJudge(d, id) {
			continue
		}
		if !toDone[id] {
			bad("stages."+id, "no path to Done")
		}
		if configRouted(s) {
			continue
		}
		success := []string{}
		for _, to := range outgoing[id] {
			if to == s.Reverify && s.Reverify != "" || to == s.Failure && s.Failure != "" && (FailureRouted(s) || s.Primitive == ExternalEffect) && !SharedJoinSuccess(d, id, to) || RequestEdge(d, id, to) || TurnBudgetExhaustionEdge(d, id, to) && !SharedJoinSuccess(d, id, to) {
				continue
			}
			if !contains(success, to) {
				success = append(success, to)
			}
		}
		if len(success) == 0 {
			bad("stages."+id, "no success route")
		}
		if len(success) > 1 {
			bad("stages."+id, "ambiguous success route: "+strings.Join(success, ", "))
		}
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
		if ProducesChanges(d, s) {
			q.host = ""
			q.gate = false
			q.review = false
		}
		if s.Primitive == HostCommand && (s.TestRun || s.Check != nil) {
			q.host = q.node
		}
		if s.Primitive == Gate && s.Expr != nil && q.host != "" {
			if q.host == s.TestStage && s.TestStage != "" && testOrCheckGate(*s.Expr, s.TestStage, d.Stages[s.TestStage]) {
				q.gate = true
			} else if h := d.Stages[q.host]; h.Check != nil && checkGate(*s.Expr, q.host, h) {
				q.gate = true
			}
		}
		reviewed := q.review
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
			if s.Primitive == Review && s.Aggregation != "" && to == s.Failure {
				next.review = reviewed // a failed aggregate verdict confers no review
			}
			queue = append(queue, next)
		}
	}
	for id, stage := range d.Stages {
		if stage.RunStats != "" && stage.RunStats != "tree" && stage.RunStats != "task" {
			bad("stages."+id+".run_stats", "must be tree or task")
		}
	}
	issues = append(issues, RunStatsIssues(d)...)
	return sortIssues(issues)
}
func stageRoles(s Stage) []string {
	if s.Primitive == Review && len(s.Reviewers) > 0 {
		return s.Reviewers
	}
	return []string{s.Role}
}
func phaseName(phase string) string {
	if phase == "" {
		return "(none)"
	}
	return phase
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

// selectors reports lookups naming an undeclared stage or artifact type,
// which would otherwise only read as unknown at run time (FAE-WF-004).
func selectors(d Document, x Expr, path string, bad func(string, string)) {
	if x.Op == "field" || x.Op == "count" || x.Op == "exists" {
		if _, ok := ProducerDeclaration(d, x.Stage); x.Stage != "" && !ok {
			bad(path, "unknown stage "+x.Stage)
		}
		if _, ok := ArtifactDeclaration(d, x.Type); x.Type != "" && !ok {
			bad(path, "unknown artifact type "+x.Type)
		}
	}
	for _, a := range x.Args {
		selectors(d, a, path, bad)
	}
}

type selector struct{ Type, Stage string }

func (s selector) label() string {
	if s.Stage != "" {
		return "stage " + s.Stage
	}
	return "type " + s.Type
}

// testRunSelectors lists the lookups of x that can select test-run evidence.
func testRunSelectors(d Document, x Expr) []selector {
	runs := map[string]bool{}
	for id, s := range d.Stages {
		if s.Primitive == HostCommand && s.TestRun {
			runs["\x00"+id] = true
			runs[s.Output+"\x00"] = true
			runs[s.Output+"\x00"+id] = true
		}
	}
	found := map[selector]bool{}
	out := []selector{}
	var walk func(Expr)
	walk = func(e Expr) {
		if (e.Op == "field" || e.Op == "count" || e.Op == "exists") && runs[e.Type+"\x00"+e.Stage] {
			sel := selector{e.Type, e.Stage}
			if !found[sel] {
				found[sel] = true
				out = append(out, sel)
			}
		}
		for _, a := range e.Args {
			walk(a)
		}
	}
	walk(x)
	sort.Slice(out, func(i, j int) bool { return out[i].label() < out[j].label() })
	return out
}

// executedBound requires a top-level conjunct bounding executed_count >= 1 on
// exactly this selector (FAE-WF-062, FAE-ROB-033).
func executedBound(x Expr, sel selector) bool {
	return conjunct(x, func(a Expr) bool {
		if len(a.Args) != 2 || a.Args[0].Op != "field" || a.Args[0].All || a.Args[0].Type != sel.Type || a.Args[0].Stage != sel.Stage || len(a.Args[0].Path) != 1 || a.Args[0].Path[0] != "executed_count" || a.Args[1].Op != "literal" {
			return false
		}
		n, ok := number(a.Args[1].Value)
		return ok && (a.Op == "ge" && n >= 1 || a.Op == "gt" && n >= 0 || a.Op == "eq" && n >= 1)
	})
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
