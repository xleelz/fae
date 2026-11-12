package engine_test

import (
	"fmt"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
	"testing"
)

func TestRunStatsOnlyDeclaredStageGetsImmutableEngineInput(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		doc := defaults.Workflow()
		start := doc.Stages[doc.Start]
		if enabled {
			start.RunStats = "tree"
			doc.Stages[doc.Start] = start
		}
		j, err := engine.New("stats-input", 1, doc, "tree")
		if err != nil {
			t.Fatal(err)
		}
		j.RunStats = map[string]any{"tree": map[string]any{"sequence": float64(42), "stats": map[string]any{"sessions": float64(7)}}}
		found := false
		for step := 0; step < 10; step++ {
			next, d, _, err := engine.Prepare(j)
			if err != nil {
				t.Fatal(err)
			}
			j = next
			if d == nil {
				continue
			}
			for _, a := range d.Inputs {
				if a.Stage == "engine:run_stats" {
					found = true
					if fmt.Sprint(a.Fields["sequence"]) != "42" || !a.Provenance || a.Role != "engine" {
						t.Fatal(a)
					}
				}
			}
			break
		}
		if found != enabled {
			t.Fatalf("declared=%v received=%v", enabled, found)
		}
		if err = engine.Validate(j); err != nil {
			t.Fatal("stats evidence invalid", err)
		}
	}
}
