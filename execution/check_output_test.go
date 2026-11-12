package execution

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/xleelz/fae/engine"
)

// CheckOutput sees a change turn's collected output once CheckChange passes,
// and its refusal is typed exactly like a CheckChange refusal: a bounded
// ValidationFailed on an unchanged tree, a rejected change with edits. A
// turn CheckChange refuses never reaches it; an accepted output is recorded.
func TestCheckOutputRefusalIsTypedLikeCheckChange(t *testing.T) {
	for _, tc := range []struct {
		name           string
		changed        bool
		check, refusal error
		want           string
		calls          int
	}{
		{name: "accepted", want: "", calls: 1},
		{name: "unchanged tree", refusal: errors.New("RedReport refused"), want: engine.ClassValidationFailed, calls: 1},
		{name: "with edits", changed: true, refusal: errors.New("RedReport refused"), want: engine.ClassRejectedChange, calls: 1},
		{name: "change refused first", check: errors.New("RED turn changed a non-test file"), refusal: errors.New("unreached"), want: engine.ClassValidationFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x, d, a := fixture(t)
			d.Config.ChangeProducer = true
			calls := 0
			x.Tree = func(context.Context, Binding) (string, error) {
				if calls++; calls > 1 && tc.changed {
					return "changed", nil
				}
				return "tree", nil
			}
			x.CheckChange = func(context.Context, engine.Dispatch, Binding) error { return tc.check }
			seen := 0
			x.CheckOutput = func(_ context.Context, _ engine.Dispatch, _ Binding, output json.RawMessage) error {
				if seen++; string(output) != `{"pass":true}` {
					t.Errorf("CheckOutput saw %s", output)
				}
				return tc.refusal
			}
			r, e := x.Execute(context.Background(), d)
			if seen != tc.calls || a.released != 1 {
				t.Fatalf("CheckOutput calls %d, released %d", seen, a.released)
			}
			if tc.want == "" {
				if e != nil || string(r.Output) != `{"pass":true}` || r.NewTree != "tree" {
					t.Fatal(r, e)
				}
				return
			}
			if e == nil || r.Class != tc.want || r.Detail == "" || len(r.Output) != 0 || r.NewTree != "" {
				t.Fatalf("class %q detail %q err %v output %s tree %q", r.Class, r.Detail, e, r.Output, r.NewTree)
			}
		})
	}
}
