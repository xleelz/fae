package workflow

import "testing"

func TestNestedElementCountCanReadEnclosingItem(t *testing.T) {
	inner := Expr{Op: "count", Stage: "model", Path: []string{"components"}, Args: []Expr{{Op: "eq", Args: []Expr{{Op: "item", Path: []string{"id"}}, {Op: "item", ItemDepth: 1, Path: []string{"id"}}}}}}
	outer := Expr{Op: "count", Stage: "input:zoom", Path: []string{"requested"}, Args: []Expr{{Op: "gt", Args: []Expr{inner, {Op: "literal", Value: 0}}}}}
	gate := Expr{Op: "eq", Args: []Expr{outer, {Op: "count", Stage: "input:zoom", Path: []string{"requested"}}}}
	if err := ValidateExpr(gate); err != nil {
		t.Fatal(err)
	}
	evidence := []Evidence{{ID: "zoom", Stage: "input:zoom", Provenance: true, Fields: map[string]any{"requested": []any{map[string]any{"id": "a"}, map[string]any{"id": "b"}}}}, {ID: "model", Stage: "model", Provenance: true, Fields: map[string]any{"components": []any{map[string]any{"id": "b"}, map[string]any{"id": "a"}}}}}
	if r := Eval(gate, evidence); r.Value != True || r.Steps > 10000 {
		t.Fatal(r)
	}
	inner.Args[0].Args[1] = Expr{Op: "outer", Path: []string{"id"}}
	outer.Args[0].Args[0] = inner
	gate.Args[0] = outer
	if err := ValidateExpr(gate); err != nil {
		t.Fatal("named outer reference rejected", err)
	}
	if r := Eval(gate, evidence); r.Value != True {
		t.Fatal("named outer reference failed", r)
	}
	evidence[1].Fields["components"] = []any{map[string]any{"id": "a"}}
	if r := Eval(gate, evidence); r.Value != False {
		t.Fatal("unknown requested id accepted", r)
	}
	bad := inner
	bad.Args = []Expr{{Op: "item", ItemDepth: 2}}
	if err := ValidateExpr(bad); err == nil {
		t.Fatal("out-of-scope item depth accepted")
	}
}

func TestWholeElementCannotBeComparedWithItsOwnProperty(t *testing.T) {
	for _, op := range []string{"item", "outer"} {
		cmp := Expr{Op: "eq", Args: []Expr{{Op: op}, {Op: op, Path: []string{"id"}}}}
		filter := Expr{Op: "count", Stage: "model", Path: []string{"items"}, Args: []Expr{cmp}}
		if op == "outer" {
			filter = Expr{Op: "count", Stage: "input", Path: []string{"items"}, Args: []Expr{filter}}
		}
		if err := ValidateExpr(filter); err == nil {
			t.Fatalf("whole %s compared with its own property", op)
		}
	}
}
