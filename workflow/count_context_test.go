package workflow

import "testing"

func TestElementFilterReadsOtherInputEvidenceAndReplays(t *testing.T) {
	x := Expr{Op: "eq", Args: []Expr{{Op: "count", Stage: "areas", Path: []string{"items"}, Args: []Expr{{Op: "in", Args: []Expr{{Op: "item", Path: []string{"id"}}, {Op: "field", Stage: "input:selected", Path: []string{"value"}}}}}}, {Op: "literal", Value: 1}}}
	evidence := []Evidence{{ID: "areas-id", Stage: "areas", Fields: map[string]any{"items": []any{map[string]any{"id": "a"}, map[string]any{"id": "b"}}}}, {ID: "selected-id", Stage: "input:selected", Fields: map[string]any{"value": []any{"b"}}}}
	record := Evaluate(RecordGate, "check", "", 0, "tree", x, evidence)
	if !record.Result.Passed || len(record.Result.EvidenceIDs) != 2 {
		t.Fatal(record.Result)
	}
	if err := Replay(record, evidence); err != nil {
		t.Fatal(err)
	}
	evidence = evidence[:1]
	if Eval(x, evidence).Value != Unknown {
		t.Fatal("missing cross-input evidence passed")
	}
}
