package workflow

import (
	"slices"
	"strings"
	"testing"
)

func TestReadPolicyCompositionAndNarrowing(t *testing.T) {
	rolePolicy := &ReadPolicy{
		Allow: []string{"src/**", "docs/**"},
		Deny:  []string{"src/secret/**"},
	}
	if !rolePolicy.Allows("src/main.go") {
		t.Fatalf("expected src/main.go to be allowed")
	}
	if rolePolicy.Allows("src/secret/key.txt") {
		t.Fatalf("expected deny to win over allow for src/secret/key.txt")
	}
	if rolePolicy.Allows("other/file.go") {
		t.Fatalf("expected path outside allow to be denied")
	}

	// Valid narrowing stage policy
	stagePolicy := &ReadPolicy{
		Allow: []string{"src/pkg/**"},
		Deny:  []string{"src/pkg/internal/**"},
	}
	if err := ValidateReadPolicyNarrowing(rolePolicy, stagePolicy); err != nil {
		t.Fatalf("expected valid narrowing, got: %v", err)
	}

	// Widening outside role allow must be refused
	wideningAllow := &ReadPolicy{
		Allow: []string{"other/**"},
	}
	if err := ValidateReadPolicyNarrowing(rolePolicy, wideningAllow); err == nil {
		t.Fatalf("expected widening outside role allow to be refused")
	}

	// Widening a path denied by role must be refused
	wideningDenied := &ReadPolicy{
		Allow: []string{"src/secret/public.txt"},
	}
	if err := ValidateReadPolicyNarrowing(rolePolicy, wideningDenied); err == nil {
		t.Fatalf("expected allow matching role deny to be refused")
	}

	// Effective composition with task policy and target deny_paths
	taskPolicy := &ReadPolicy{
		Deny: []string{"src/pkg/private.go"},
	}
	eff := ComposeReadPolicy([]string{"**/*.pem"}, rolePolicy, stagePolicy, taskPolicy)
	if !eff.Allows("src/pkg/api.go") {
		t.Fatalf("expected src/pkg/api.go to be allowed by effective policy")
	}
	if eff.Allows("src/pkg/internal/x.go") {
		t.Fatalf("expected src/pkg/internal/x.go to be denied by stage policy")
	}
	if eff.Allows("src/pkg/private.go") {
		t.Fatalf("expected src/pkg/private.go to be denied by task policy")
	}
	if eff.Allows("src/pkg/cert.pem") {
		t.Fatalf("expected src/pkg/cert.pem to be denied by target deny_paths")
	}
	if eff.Allows("docs/readme.md") {
		t.Fatalf("expected docs/readme.md to be denied because stage narrowed allow to src/pkg/**")
	}

	// Stage input binding restriction: consumer cannot bind producer output if producer
	// could read a path denied to consumer.
	ok, reason := CanBindStageReadPolicy(rolePolicy, eff)
	if ok || reason == "" {
		t.Fatalf("expected consumer with narrower read policy to refuse producer with broader read policy")
	}
	ok, reason = CanBindStageReadPolicy(eff, rolePolicy)
	if !ok || reason != "" {
		t.Fatalf("expected broader consumer to accept narrower producer, got ok=%v reason=%q", ok, reason)
	}
}

func TestReadRootArgTemplateAndGitPushValidation(t *testing.T) {
	s := Stage{
		Primitive: HostCommand,
		Command:   []string{"diff", "-u", "{{ext}}", "local.txt"},
		ArgTemplates: map[string]ArgTemplate{
			"ext": {Kind: "read-root", Key: "upstream"},
		},
	}
	partial, err := BindCommand(s, nil)
	if err != nil || !slices.Equal(partial, []string{"diff", "-u", "{{ext}}", "local.txt"}) {
		t.Fatalf("BindCommand partial: %v, err=%v", partial, err)
	}
	resolved, err := BindCommandWithReadRoots(s, nil, map[string]string{"upstream": "/var/tmp/upstream-root"})
	if err != nil || !slices.Equal(resolved, []string{"diff", "-u", "/var/tmp/upstream-root", "local.txt"}) {
		t.Fatalf("BindCommandWithReadRoots: %v, err=%v", resolved, err)
	}
	shape := []string{"diff", "-u", "{{read-root}}", "local.txt"}
	if !MatchTemplate(shape, s) {
		t.Fatalf("expected MatchTemplate to match {{read-root}} slot")
	}
	if !MatchShape(shape, resolved) {
		t.Fatalf("expected MatchShape to match resolved read-root path")
	}

	// Also verify ArgBinding with Kind: "read-root"
	s2 := Stage{
		Primitive: HostCommand,
		Command:   []string{"check", "{{ref_root}}"},
		Bindings: map[string]ArgBinding{
			"ref_root": {Kind: "read-root", Key: "sdk"},
		},
	}
	res2, err := BindCommandWithReadRoots(s2, nil, map[string]string{"sdk": "/opt/sdk"})
	if err != nil || !slices.Equal(res2, []string{"check", "/opt/sdk"}) {
		t.Fatalf("BindCommandWithReadRoots via Bindings: %v, err=%v", res2, err)
	}

	// Verify git-push ExternalEffect validation in Local mode (Role == "" required)
	pushStage := Stage{
		Primitive:  ExternalEffect,
		EffectKind: "git-push",
		Subject:    "git-push",
		Output:     "Receipt",
	}
	if !pushStage.IsGitPush() {
		t.Fatalf("expected pushStage.IsGitPush() to be true")
	}
	var issues []string
	bad := func(path, msg string) {
		issues = append(issues, path+": "+msg)
	}
	doc := Document{
		Roles: map[string]Role{
			"coder": {ReadRoots: []string{"sdk"}, Reads: &ReadPolicy{Allow: []string{"src/**"}}},
		},
		Stages: map[string]Stage{
			"push": pushStage,
		},
	}
	validateReadsAndPush(doc, bad)
	if len(issues) != 0 {
		t.Fatalf("expected valid git-push stage with empty role, got issues: %v", issues)
	}

	// Non-empty Role on git-push must be refused
	pushStageWithRole := pushStage
	pushStageWithRole.Role = "coder"
	doc.Stages["push"] = pushStageWithRole
	validateReadsAndPush(doc, bad)
	if len(issues) == 0 || !strings.Contains(issues[0], "must not declare a role") {
		t.Fatalf("expected git-push with role to fail validation, got: %v", issues)
	}
}
