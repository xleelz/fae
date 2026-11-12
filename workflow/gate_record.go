package workflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/xleelz/fae/domain"
)

// Gate record kinds: a Gate stage, one evaluated Branch arm, a Review
// aggregate verdict, a HostCommand expected-shape check and a fan-out
// branch's verdict at its join.
const (
	RecordGate      = "gate"
	RecordBranchArm = "branch-arm"
	RecordAggregate = "review-aggregate"
	RecordShape     = "expected-shape"
	RecordBranch    = "branch-verdict"
)

// EvidenceRef pins one evidence item a gate was given, in evaluation order.
// The id is the item's content hash, so the reference fixes its content.
type EvidenceRef struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Stage   string `json:"stage"`
	Session string `json:"session,omitempty"`
}

// GateRecord is a separately content-addressed gate result (FAE-WF-063): the
// expression, the ordered evidence it read candidates from, and the result
// with every leaf value and the ids it read. Replay re-evaluates it.
type GateRecord struct {
	ID         string        `json:"id"`
	Kind       string        `json:"kind"`
	Stage      string        `json:"stage"`
	Name       string        `json:"name,omitempty"`
	Arm        int           `json:"arm,omitempty"`
	Tree       string        `json:"tree"`
	Expression Expr          `json:"expression"`
	Evidence   []EvidenceRef `json:"evidence"`
	Result     GateResult    `json:"result"`
}

// selects reports whether some lookup of x can select a. Evidence no lookup
// selects cannot change a result, so records keep only selectable evidence.
func selects(x Expr, a Evidence) bool {
	if (x.Op == "field" || x.Op == "count" || x.Op == "exists") && (x.Type == "" || x.Type == a.Type) && (x.Stage == "" || x.Stage == a.Stage) {
		return true
	}
	for _, arg := range x.Args {
		if selects(arg, a) {
			return true
		}
	}
	return false
}

// Evaluate evaluates x over evidence and returns its content-addressed record.
func Evaluate(kind, stage, name string, arm int, tree string, x Expr, evidence []Evidence) GateRecord {
	r := GateRecord{Kind: kind, Stage: stage, Name: name, Arm: arm, Tree: tree, Expression: x, Evidence: []EvidenceRef{}}
	used := []Evidence{}
	for _, a := range evidence {
		if selects(x, a) {
			used = append(used, a)
			r.Evidence = append(r.Evidence, EvidenceRef{ID: a.ID, Type: a.Type, Stage: a.Stage, Session: a.Session})
		}
	}
	r.Result = Eval(x, used)
	r.ID, _ = r.Digest()
	return r
}

// Digest is the content hash of the record without its id.
func (r GateRecord) Digest() (string, error) {
	r.ID = ""
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	return domain.ContentHash(b)
}

// Replay resolves the record's evidence references from recorded evidence,
// re-evaluates the expression and requires the identical result.
func Replay(r GateRecord, recorded []Evidence) error {
	if id, err := r.Digest(); err != nil || id != r.ID {
		return errors.New("gate record content address mismatch")
	}
	type key struct{ id, typ, stage, session string }
	index := map[key]Evidence{}
	for _, a := range recorded {
		index[key{a.ID, a.Type, a.Stage, a.Session}] = a
	}
	evidence := make([]Evidence, 0, len(r.Evidence))
	for _, ref := range r.Evidence {
		a, ok := index[key{ref.ID, ref.Type, ref.Stage, ref.Session}]
		if !ok {
			return fmt.Errorf("gate record %s cites unrecorded evidence %s", r.ID, ref.ID)
		}
		evidence = append(evidence, a)
	}
	got, _ := json.Marshal(Eval(r.Expression, evidence))
	want, _ := json.Marshal(r.Result)
	if !bytes.Equal(got, want) {
		return fmt.Errorf("gate record %s replays differently: %s != %s", r.ID, got, want)
	}
	return nil
}
