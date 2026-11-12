package workflow

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xleelz/fae/domain"
)

func TestEscalateKindIsClosedAndEscalateOnly(t *testing.T) {
	for _, kind := range domain.EscalationKinds {
		d := protected()
		s := d.Stages["stop"]
		s.Kind = kind
		d.Stages["stop"] = s
		if issues := Validate(d); len(issues) > 0 {
			t.Fatal(kind, issues)
		}
	}
	for name, mutate := range map[string]func(*Document){
		"open kind":          func(d *Document) { s := d.Stages["stop"]; s.Kind = "DispatchRecoveryCap"; d.Stages["stop"] = s },
		"kind off escalate":  func(d *Document) { s := d.Stages["review"]; s.Kind = domain.LoopCap; d.Stages["review"] = s },
		"lower-case variant": func(d *Document) { s := d.Stages["stop"]; s.Kind = "loopcap"; d.Stages["stop"] = s },
	} {
		d := protected()
		mutate(&d)
		if issues := Validate(d); len(issues) == 0 || !strings.Contains(issues[0].Path, ".kind") {
			t.Fatal(name, issues)
		}
	}
	// An omitted kind keeps legacy documents (and their hashes) valid.
	d := protected()
	raw, _ := json.Marshal(d.Stages["stop"])
	if strings.Contains(string(raw), "kind") || len(Validate(d)) > 0 {
		t.Fatal(string(raw))
	}
}
