package workflow

import (
	"encoding/json"
	"testing"
)

func TestThreeValuedTables(t *testing.T) {
	vals := []Expr{lit(true), lit(false), field("absent", "x")}
	truths := []Truth{True, False, Unknown}
	and := [][]Truth{{True, False, Unknown}, {False, False, False}, {Unknown, False, Unknown}}
	or := [][]Truth{{True, True, True}, {True, False, Unknown}, {True, Unknown, Unknown}}
	for i, a := range vals {
		for j, b := range vals {
			if got := Eval(bin("and", a, b), nil).Value; got != and[i][j] {
				t.Fatal(i, j, got)
			}
			if got := Eval(bin("or", a, b), nil).Value; got != or[i][j] {
				t.Fatal(i, j, got)
			}
		}
		want := Unknown
		if truths[i] == True {
			want = False
		}
		if truths[i] == False {
			want = True
		}
		if Eval(Expr{Op: "not", Args: []Expr{a}}, nil).Value != want {
			t.Fatal(i)
		}
	}
}
func TestGateOperationsAndTrace(t *testing.T) {
	ev := []Evidence{{ID: "a", Stage: "run", Type: "Run", Fields: map[string]any{"n": 2, "status": "passed"}}, {ID: "b", Stage: "run", Type: "Run", Fields: map[string]any{"n": 3, "status": "passed"}}}
	cases := []Expr{bin("eq", field("run", "n"), lit(3)), bin("ne", lit(1), lit(2)), bin("lt", lit(1), lit(2)), bin("le", lit(2), lit(2)), bin("gt", lit(3), lit(2)), bin("ge", lit(2), lit(2)), bin("prefix", lit("prefix"), lit("pre")), bin("in", lit("x"), lit([]any{"x", "y"})), bin("eq", Expr{Op: "count", Type: "Run", Args: []Expr{bin("ge", field("run", "n"), lit(2))}}, lit(2)), Expr{Op: "exists", Type: "Run"}}
	for _, x := range cases {
		if e := ValidateExpr(x); e != nil {
			t.Fatal(e)
		}
		r := Eval(x, ev)
		if !r.Passed || len(r.Leaves) == 0 {
			t.Fatal(x, r)
		}
	}
	r := Eval(bin("eq", field("run", "n"), lit(3)), ev)
	if len(r.EvidenceIDs) != 1 || r.EvidenceIDs[0] != "b" {
		t.Fatal(r)
	}
}
func TestUnknownCauses(t *testing.T) {
	ev := []Evidence{{ID: "a", Stage: "run", Fields: map[string]any{"status": "alien", "number": 1}}}
	status := field("run", "status")
	status.Enum = []string{"passed", "failed"}
	for _, x := range []Expr{field("missing", "x"), field("run", "missing"), status, bin("eq", field("run", "number"), lit("1")), bin("prefix", lit(1), lit("a")), {Op: "bad"}} {
		r := Eval(x, ev)
		if r.Passed || r.Value != Unknown {
			t.Fatal(x, r)
		}
		found := false
		for _, l := range r.Leaves {
			found = found || l.Unknown != ""
		}
		if !found {
			t.Fatal("unknown without reason")
		}
	}
}
func TestPositiveGateChecks(t *testing.T) {
	exists := Expr{Op: "exists", Type: "Run"}
	for _, x := range []Expr{bin("eq", Expr{Op: "count", Type: "Run"}, lit(0)), bin("or", exists, lit(true)), {Op: "not", Args: []Expr{exists}}} {
		if positive(x) {
			t.Fatal("vacuous accepted", x)
		}
	}
	if !positive(bin("and", exists, lit(true))) {
		t.Fatal("positive rejected")
	}
}
func FuzzGateBounded(f *testing.F) {
	f.Add([]byte(`{"op":"literal","value":true}`))
	f.Add([]byte(`{"op":"count","stage":"fixture","path":["items"],"args":[{"op":"gt","args":[{"op":"count","stage":"fixture","path":["items"],"args":[{"op":"eq","args":[{"op":"item","path":["id"]},{"op":"item","item_depth":1,"path":["id"]}]}]},{"op":"literal","value":0}]}]}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		var x Expr
		if json.Unmarshal(raw, &x) != nil {
			return
		}
		r := Eval(x, []Evidence{{ID: "fixture", Stage: "fixture", Provenance: true, Fields: map[string]any{"items": []any{map[string]any{"id": "a"}, map[string]any{"id": "b"}}}}})
		if r.Steps > 10100 {
			t.Fatal("unbounded", r.Steps)
		}
		if r.Passed && r.Value != True {
			t.Fatal("fail open")
		}
	})
}
func TestNumericPrecision(t *testing.T) {
	a := lit(json.Number("9007199254740992"))
	b := lit(json.Number("9007199254740993"))
	if Eval(bin("eq", a, b), nil).Value != False {
		t.Fatal("rounded unequal integers")
	}
	if !Eval(bin("lt", a, b), nil).Passed {
		t.Fatal("lost numeric ordering")
	}
	if !Eval(bin("eq", lit(json.Number("1e2")), lit(100)), nil).Passed {
		t.Fatal("equivalent numbers differ")
	}
}
func TestPositivePerCollection(t *testing.T) {
	a := Expr{Op: "exists", Type: "A"}
	b := bin("eq", Expr{Op: "count", Type: "B"}, lit(0))
	if positiveCollections(bin("and", a, b)) {
		t.Fatal("A evidence excused empty B")
	}
}
