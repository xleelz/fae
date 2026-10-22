package workflow

import (
	"encoding/json"
	"testing"
)

func TestArtifactJSONSchemaAndFiringControls(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","additionalProperties":false,"required":["value","kind"],"properties":{"value":{"$ref":"#/$defs/positive"},"kind":{"enum":["run"]}},"$defs":{"positive":{"type":"integer","minimum":1}}}`)
	if e := ValidateArtifact(schema, json.RawMessage(`{"kind":"run","value":9007199254740993}`)); e != nil {
		t.Fatal(e)
	}
	for _, raw := range []string{`{"kind":"run","value":0}`, `{"kind":"wrong","value":1}`, `{"kind":"run","value":"1"}`, `{"kind":"run","value":1,"x":1}`, `{"kind":"run"}`, `{"kind":"run","value":1,"value":2}`} {
		if ValidateArtifact(schema, json.RawMessage(raw)) == nil {
			t.Fatal("accepted", raw)
		}
	}
	for _, s := range []string{`{"type":"nonsense"}`, `{"$ref":"file:///etc/passwd"}`, `{"$ref":"https://example.com/remote.json"}`, `{"minimum":"bad"}`} {
		if ValidateArtifact(json.RawMessage(s), json.RawMessage(`{}`)) == nil {
			t.Fatal("accepted schema", s)
		}
	}
}
