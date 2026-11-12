package workflow_test

import (
	"bytes"
	"encoding/json"
	"github.com/xleelz/fae/workflow"
	"testing"
)

func TestSeededFixtureValidatesRequiredTypesAndRefusesUnsatisfiableConstraints(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","required":["n","list","pick","nested"],"additionalProperties":false,"properties":{"n":{"type":"integer","minimum":3,"multipleOf":2},"list":{"type":"array","minItems":2,"items":{"type":"string","minLength":4}},"pick":{"enum":["one","two"]},"nested":{"$ref":"#/$defs/entry"}},"$defs":{"entry":{"type":"object","required":["ok"],"properties":{"ok":{"type":"boolean"}}}}}`)
	a, err := workflow.SchemaValidFixture(schema, 7)
	if err != nil || workflow.ValidateArtifact(schema, a) != nil {
		t.Fatalf("invalid fixture%s: %v", a, err)
	}
	b, err := workflow.SchemaValidFixture(schema, 7)
	if err != nil || !bytes.Equal(a, b) {
		t.Fatal("seed not deterministic", err)
	}
	impossible := json.RawMessage(`{"type":"string","allOf":[{"const":"a"},{"const":"b"}]}`)
	if raw, err := workflow.SchemaValidFixture(impossible, 7); err == nil {
		t.Fatalf("invalid generated evidence%s", raw)
	}
}
