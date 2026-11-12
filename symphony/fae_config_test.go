package symphony

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFaeConfigDefaultsPreserveStandaloneRunner(t *testing.T) {
	config, err := ResolveServiceConfig(&WorkflowDefinition{Config: map[string]any{}}, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if config.Fae.Mode != "Local" || config.Fae.SyncState || config.Fae.PostComments || config.Fae.WorkflowID != "" || config.Codex.Command != DefaultCodexCommand {
		t.Fatalf("unsafe or incompatible defaults: %+v", config.Fae)
	}
	if err = ValidateFaeConfig(config.Fae); err == nil {
		t.Fatal("embedded bridge accepted missing installed workflow/target/authority")
	}
}
func TestFaeConfigResolvesTypedSelectorsAndExplicitWriteOptIns(t *testing.T) {
	definition := &WorkflowDefinition{Config: map[string]any{"fae": map[string]any{"workflow_id": "$WORKFLOW", "workflow_revision": "$REVISION", "target_config": "target", "target_revision": int64(4), "authorizing_message": "$AUTHORITY", "mode": "local", "sync_state": true, "post_comments": true}}}
	values := map[string]string{"WORKFLOW": "typed-workflow", "REVISION": "3", "AUTHORITY": "operator-message"}
	config, err := ResolveServiceConfig(definition, func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	if config.Fae.WorkflowID != "typed-workflow" || config.Fae.WorkflowRevision != 3 || config.Fae.TargetConfig != "target" || config.Fae.TargetRevision != 4 || config.Fae.AuthorizingMessage != "operator-message" || config.Fae.Mode != "Local" || !config.Fae.SyncState || !config.Fae.PostComments {
		t.Fatalf("incorrect typed settings: %+v", config.Fae)
	}
	if err = ValidateFaeConfig(config.Fae); err != nil {
		t.Fatal(err)
	}
}
func TestFaeConfigRejectsAmbiguousTypesAndDeployment(t *testing.T) {
	for _, tc := range []struct {
		key   string
		value any
	}{{"sync_state", "false"}, {"post_comments", 1}, {"workflow_revision", 1.5}, {"target_revision", -1}, {"workflow_revision", float64(9007199254740992)}, {"workflow_id", true}, {"mode", "Normal"}, {"syncState", true}} {
		t.Run(tc.key+"-"+strings.ReplaceAll(tc.key, "_", ""), func(t *testing.T) {
			_, err := ResolveServiceConfig(&WorkflowDefinition{Config: map[string]any{"fae": map[string]any{tc.key: tc.value}}}, func(string) string { return "" })
			if err == nil {
				t.Fatal("invalid FAE configuration accepted", tc.key)
			}
		})
	}
	if _, err := ResolveServiceConfig(&WorkflowDefinition{Config: map[string]any{"fae": "not-a-map"}}, nil); err == nil {
		t.Fatal("non-map FAE configuration accepted")
	}
	good := FaeConfig{WorkflowID: "workflow", TargetConfig: "target", AuthorizingMessage: "message", Mode: "Local"}
	for _, change := range []func(*FaeConfig){func(c *FaeConfig) { c.WorkflowID = "" }, func(c *FaeConfig) { c.TargetConfig = "" }, func(c *FaeConfig) { c.AuthorizingMessage = "" }, func(c *FaeConfig) { c.WorkflowRevision = -1 }, func(c *FaeConfig) { c.Mode = "Normal" }} {
		bad := good
		change(&bad)
		if err := ValidateFaeConfig(bad); err == nil {
			t.Fatal("embedded preflight accepted invalid selector/authority", bad)
		}
	}
}
func TestLinearWorkflowExampleContainsOnlySecretReferencesAndTypedDefaults(t *testing.T) {
	path := filepath.Join("..", "examples", "LINEAR_WORKFLOW.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := ParseWorkflowBytes(raw, path)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{"LINEAR_API_KEY": "synthetic-test-token", "LINEAR_TEAM_KEY": "TEAM", "LINEAR_PROJECT_SLUG": "project", "FAE_LINEAR_WORKFLOW_ID": "workflow", "FAE_LINEAR_TARGET_CONFIG": "target", "FAE_LINEAR_AUTHORIZING_MESSAGE": "message"}
	config, err := ResolveServiceConfig(definition, func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateDispatchConfig(config, "linear"); err != nil {
		t.Fatal(err)
	}
	if err = ValidateFaeConfig(config.Fae); err != nil {
		t.Fatal(err)
	}
	if config.Tracker.Provider["_api_key_env"] != "LINEAR_API_KEY" || !strings.Contains(string(raw), "api_key: $LINEAR_API_KEY") || strings.Contains(string(raw), "synthetic-test-token") {
		t.Fatal("example does not retain a secret environment reference")
	}
	if config.Fae.SyncState || config.Fae.PostComments || config.Fae.Mode != "Local" || config.Agent.MaxConcurrentAgents != 2 || len(config.Tracker.RequiredLabels) != 1 || config.Tracker.RequiredLabels[0] != "Fae" {
		t.Fatal("example opt-ins or routing defaults incorrect")
	}
	missing, err := ResolveServiceConfig(definition, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateFaeConfig(missing.Fae); err == nil {
		t.Fatal("unchanged placeholder example could select an embedded workflow")
	}
}
