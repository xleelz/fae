package workflow

import (
	"encoding/json"
	"testing"
)

// normalTail appends a Normal-only external chain after commit, with a
// failure route into a Normal-only loop and an optional input on change.
func normalTail() Document {
	d := protected()
	d.Artifacts["receipt"] = ArtifactType{1, json.RawMessage(`{"type":"object"}`)}
	r := d.Roles["coder"]
	r.Outputs = append(r.Outputs, "receipt")
	d.Roles["coder"] = r
	effect := func(subject, failure string) Stage {
		return Stage{Primitive: ExternalEffect, Role: "coder", NormalOnly: true, Subject: subject, Pins: TargetPins(), IdempotenceKey: subject, Output: "receipt", Failure: failure}
	}
	d.Stages["create"] = effect("create", "")
	d.Stages["read"] = effect("read", "again")
	d.Stages["again"] = Stage{Primitive: Loop, MaxIterations: 5, NormalOnly: true}
	d.Edges[len(d.Edges)-1].To = "create"
	d.Edges = append(d.Edges, Edge{"create", "read"}, Edge{"read", "Done"}, Edge{"read", "again"}, Edge{"again", "change"})
	change := d.Stages["change"]
	change.Inputs = []Input{{Name: "read", Type: "receipt", Stage: "read", Version: 1, Optional: true}}
	d.Stages["change"] = change
	return d
}

func TestLocalProjectionRemovesNormalOnlyStages(t *testing.T) {
	if !IsTargetPinned(TargetPins()) || IsTargetPinned(map[string]string{TargetPinsKey: "subject", "x": "y"}) || IsTargetPinned(map[string]string{"draft": "true"}) {
		t.Fatal("target pin marker")
	}
	plain := protected()
	same, err := LocalProjection(plain)
	if err != nil || HasNormalOnly(plain) {
		t.Fatal(err)
	}
	a, _ := json.Marshal(same)
	b, _ := json.Marshal(plain)
	if string(a) != string(b) {
		t.Fatal("document without Normal-only stages changed")
	}
	d := normalTail()
	if issues := Validate(d); len(issues) != 0 {
		t.Fatal(issues)
	}
	local, err := LocalProjection(d)
	if err != nil {
		t.Fatal(err)
	}
	a, _ = json.Marshal(local)
	if string(a) != string(b) {
		t.Fatalf("projection is not the document without its Normal tail:\n%s\n%s", a, b)
	}
	if !HasNormalOnly(d) || len(d.Stages) != 9 {
		t.Fatal("projection mutated its input")
	}
}

func TestLocalProjectionRefusesUnprojectableContent(t *testing.T) {
	for name, mutate := range map[string]func(*Document){
		"Normal-only branch": func(d *Document) {
			d.Stages["pick"] = Stage{Primitive: Branch, NormalOnly: true, Default: "create", Cases: []Case{{When: lit(true), Next: "Done"}}}
			d.Edges[5].To = "pick"
			d.Edges = append(d.Edges, Edge{"pick", "create"}, Edge{"pick", "Done"})
		},
		"expression reads a removed stage": func(d *Document) {
			x := bin("and", goodGate(), Expr{Op: "exists", Stage: "read"})
			gate := d.Stages["gate"]
			gate.Expr = &x
			d.Stages["gate"] = gate
		},
		"ambiguous success": func(d *Document) {
			d.Edges = append(d.Edges, Edge{"create", "Done"})
		},
	} {
		d := normalTail()
		mutate(&d)
		if _, err := LocalProjection(d); err == nil {
			t.Errorf("%s: projected", name)
		}
	}
}
