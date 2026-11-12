package workflow

import (
	"strings"
	"testing"
)

func TestCheckCommandValidationAndNonVacuousGates(t *testing.T) {
	baseDoc := func() Document {
		d := protected()
		d.Stages["check-models"] = Stage{
			Primitive:      HostCommand,
			Command:        []string{"model-check", "--json"},
			Weight:         1,
			TimeoutSeconds: 30,
			Output:         "report",
			Check: &CheckSpec{
				Format:     "json",
				Output:     "stdout",
				CountField: "checked_models",
				Writes:     []string{"build/check-cache/**"},
			},
		}
		return d
	}

	// 1. Gate reading a Check stage with positive lower bound (>= 1) on CountField passes validation.
	d := baseDoc()
	goodExpr := Expr{
		Op: "and",
		Args: []Expr{
			{
				Op: "eq",
				Args: []Expr{
					{Op: "field", Type: "report", Stage: "check-models", Path: []string{"exit_code"}},
					{Op: "literal", Value: 0},
				},
			},
			{
				Op: "ge",
				Args: []Expr{
					{Op: "field", Type: "report", Stage: "check-models", Path: []string{"checked_models"}},
					{Op: "literal", Value: 1},
				},
			},
		},
	}
	d.Stages["model-gate"] = Stage{
		Primitive: Gate,
		Name:      "model-gate",
		TestStage: "check-models",
		Expr:      &goodExpr,
		Failure:   "stop",
	}
	d.Edges = []Edge{
		{From: "change", To: "check-models"},
		{From: "check-models", To: "model-gate"},
		{From: "model-gate", To: "test"},
		{From: "model-gate", To: "stop"},
		{From: "test", To: "gate"},
		{From: "gate", To: "review"},
		{From: "gate", To: "stop"},
		{From: "review", To: "commit"},
		{From: "commit", To: "Done"},
	}
	if issues := Validate(d); len(issues) != 0 {
		t.Fatalf("expected valid check command workflow, got issues: %v", issues)
	}

	// 2. Gate reading a Check stage without a positive lower bound on CountField is refused (FAE-WF-062).
	vacuousDoc := baseDoc()
	vacuousExpr := Expr{
		Op: "eq",
		Args: []Expr{
			{Op: "field", Type: "report", Stage: "check-models", Path: []string{"exit_code"}},
			{Op: "literal", Value: 0},
		},
	}
	vacuousDoc.Stages["model-gate"] = Stage{
		Primitive: Gate,
		Name:      "model-gate",
		Expr:      &vacuousExpr,
		Failure:   "stop",
	}
	vacuousDoc.Edges = d.Edges
	issues := Validate(vacuousDoc)
	if len(issues) == 0 {
		t.Fatal("expected vacuous check gate without count_field >= 1 bound to be rejected")
	}

	// 3. Invalid CheckSpec format or empty CountField is rejected.
	badFormat := baseDoc()
	st := badFormat.Stages["check-models"]
	st.Check = &CheckSpec{Format: "xml", CountField: "checked_models"}
	badFormat.Stages["check-models"] = st
	if issues := Validate(badFormat); len(issues) == 0 {
		t.Fatal("expected invalid check format to be rejected")
	}

	badCount := baseDoc()
	st2 := badCount.Stages["check-models"]
	st2.Check = &CheckSpec{Format: "lines", CountField: ""}
	badCount.Stages["check-models"] = st2
	if issues := Validate(badCount); len(issues) == 0 {
		t.Fatal("expected empty count_field to be rejected")
	}
}

func TestParseCheckOutputJSONAndLines(t *testing.T) {
	// JSON format
	jsonSpec := CheckSpec{
		Format:     "json",
		CountField: "summary.checked",
	}
	payload, count, err := ParseCheckOutput(jsonSpec, `{"summary":{"checked":4},"ok":true}`, 0, false, false, "sha256:abc")
	if err != nil {
		t.Fatalf("ParseCheckOutput json failed: %v", err)
	}
	if count != 4 {
		t.Fatalf("expected count 4, got %v", count)
	}
	if payload["exit_code"] != 0 {
		t.Fatalf("expected exit_code 0 in payload, got %v", payload["exit_code"])
	}

	// Missing count_field in JSON fails
	if _, _, err := ParseCheckOutput(jsonSpec, `{"ok":true}`, 0, false, false, ""); err == nil {
		t.Fatal("expected missing count_field in JSON output to fail")
	}

	// Lines format
	linesSpec := CheckSpec{
		Format:     "lines",
		CountField: "checked_files",
	}
	lPayload, lCount, err := ParseCheckOutput(linesSpec, "\nmodel/a.yaml\nmodel/b.yaml\n\nmodel/c.yaml\n", 0, false, false, "sha256:def")
	if err != nil {
		t.Fatalf("ParseCheckOutput lines failed: %v", err)
	}
	if lCount != 3 {
		t.Fatalf("expected line count 3, got %v", lCount)
	}
	if lPayload["checked_files"] != 3 || lPayload["line_count"] != 3 {
		t.Fatalf("expected checked_files=3 and line_count=3, got %v", lPayload)
	}
	lines, ok := lPayload["lines"].([]string)
	if !ok || len(lines) != 3 {
		t.Fatalf("expected 3 non-empty lines, got %v", lPayload["lines"])
	}
}

func TestFanOutBranchesFromAndChangeProducingValidation(t *testing.T) {
	approved := Expr{
		Op: "eq",
		Args: []Expr{
			{Op: "count", Stage: "branch-review", Args: []Expr{
				{Op: "eq", Args: []Expr{
					{Op: "field", Stage: "branch-review", Path: []string{"approved"}},
					{Op: "literal", Value: true},
				}},
			}},
			{Op: "literal", Value: 1},
		},
	}

	d := protected()
	d.Start = "plan"
	d.Stages["plan"] = Stage{
		Primitive: AgentTurn,
		Role:      "reviewer",
		Output:    "report",
	}
	d.Stages["fan"] = Stage{
		Primitive: FanOut,
		Join:      "join",
		Parallel:  true,
		BranchesFrom: &BranchesFromSpec{
			Stage: "plan",
			Field: "items",
			Min:   0,
			Max:   64,
			Empty: "change",
		},
		BranchTemplate: &FanBranch{
			ID:         "template",
			Start:      "branch-impl",
			Pass:       &approved,
			WriteScope: []string{"pkg/{{item.pkg}}/**"},
		},
	}
	d.Stages["branch-impl"] = Stage{
		Primitive:      AgentTurn,
		Role:           "coder",
		Output:         "report",
		ChangeProducer: true,
	}
	d.Stages["branch-review"] = Stage{
		Primitive: Review,
		Role:      "reviewer",
		Output:    "report",
	}
	d.Stages["join"] = Stage{
		Primitive: FanIn,
		Rule:      JoinAll,
		Failure:   "stop",
	}
	d.Edges = append(d.Edges,
		Edge{From: "plan", To: "fan"},
		Edge{From: "fan", To: "branch-impl"},
		Edge{From: "fan", To: "change"},
		Edge{From: "branch-impl", To: "branch-review"},
		Edge{From: "branch-review", To: "join"},
		Edge{From: "join", To: "test"},
		Edge{From: "join", To: "stop"},
	)
	if issues := Validate(d); len(issues) != 0 {
		t.Fatalf("expected valid dynamic parallel change-producing FanOut, got issues: %v", issues)
	}

	// Missing write_scope on a change-producing FanOut branch must be rejected.
	noScope := d
	noScopeStages := map[string]Stage{}
	for k, v := range d.Stages {
		noScopeStages[k] = v
	}
	fanSt := noScopeStages["fan"]
	fanSt.BranchTemplate = &FanBranch{
		ID:    "template",
		Start: "branch-impl",
		Pass:  &approved,
	}
	noScopeStages["fan"] = fanSt
	noScope.Stages = noScopeStages
	issues := Validate(noScope)
	foundWriteScopeErr := false
	for _, iss := range issues {
		if strings.Contains(iss.Message, "write_scope") || strings.Contains(iss.Message, "change producers") {
			foundWriteScopeErr = true
		}
	}
	if !foundWriteScopeErr {
		t.Fatalf("expected missing write_scope on change-producing branch to be rejected, got: %v", issues)
	}

	// Min == 0 without Empty route must be rejected.
	noEmpty := d
	noEmptyStages := map[string]Stage{}
	for k, v := range d.Stages {
		noEmptyStages[k] = v
	}
	fanNoEmpty := noEmptyStages["fan"]
	fanNoEmpty.BranchesFrom = &BranchesFromSpec{
		Stage: "plan",
		Field: "items",
		Min:   0,
		Max:   16,
	}
	noEmptyStages["fan"] = fanNoEmpty
	noEmpty.Stages = noEmptyStages
	if issues := Validate(noEmpty); len(issues) == 0 {
		t.Fatal("expected Min==0 without Empty route to be rejected")
	}

	// Change-producing FanOut branch with parallel: false must be rejected (FAE-WF-096 item 3).
	seqChange := d
	seqStages := map[string]Stage{}
	for k, v := range d.Stages {
		seqStages[k] = v
	}
	fanSeq := seqStages["fan"]
	fanSeq.Parallel = false
	seqStages["fan"] = fanSeq
	seqChange.Stages = seqStages
	if issues := Validate(seqChange); len(issues) == 0 {
		t.Fatal("expected change-producing FanOut with parallel: false to be rejected")
	}

	// HostCommand Check.Writes overlapping a stage or branch WriteScope must be rejected (FAE-WF-095(e)).
	overlapDoc := d
	overlapStages := map[string]Stage{}
	for k, v := range d.Stages {
		overlapStages[k] = v
	}
	overlapStages["lint-check"] = Stage{
		Primitive:      HostCommand,
		Command:        []string{"model-check", "--json"},
		Weight:         1,
		TimeoutSeconds: 30,
		Output:         "report",
		Check: &CheckSpec{
			Format:     "json",
			Output:     "stdout",
			CountField: "checked",
			Writes:     []string{"pkg/**"},
		},
	}
	overlapDoc.Stages = overlapStages
	overlapIssues := Validate(overlapDoc)
	foundOverlap := false
	for _, iss := range overlapIssues {
		if strings.Contains(iss.Path, "check.writes") && strings.Contains(iss.Message, "overlaps") {
			foundOverlap = true
		}
	}
	if !foundOverlap {
		t.Fatalf("expected check.writes overlapping branch write_scope to be rejected, got: %v", overlapIssues)
	}

	// CheckDocumentWritesDisjoint rejects task declaredFiles overlapping any stage's Check.Writes.
	cleanCheckDoc := protected()
	cleanCheckDoc.Stages["lint_check"] = Stage{
		Primitive:      HostCommand,
		Command:        []string{"model-check", "--json"},
		Weight:         1,
		TimeoutSeconds: 30,
		Output:         "report",
		Check: &CheckSpec{
			Format:     "json",
			Output:     "stdout",
			CountField: "checked",
			Writes:     []string{"reports/*.json"},
		},
	}
	if err := CheckDocumentWritesDisjoint(cleanCheckDoc, []string{"pkg/foo.go"}); err != nil {
		t.Fatalf("expected disjoint declaredFiles to pass CheckDocumentWritesDisjoint, got: %v", err)
	}
	if err := CheckDocumentWritesDisjoint(cleanCheckDoc, []string{"reports/out.json"}); err == nil {
		t.Fatal("expected overlapping declaredFiles to be rejected by CheckDocumentWritesDisjoint")
	}
}
