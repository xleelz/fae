package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

// FAE-WF-001, -SCH-007: content declares molecule concurrency, the maximum of
// this workflow's molecules running at once; −1 is unbounded.
func TestMoleculeConcurrencySection(t *testing.T) {
	for n, want := range map[int]bool{-1: true, 1: true, 4: true, 0: false, -2: false} {
		d := minimal()
		d.MoleculeConcurrency = &MoleculeConcurrency{MaxRunning: n}
		issues := Validate(d)
		if (len(issues) == 0) != want || !want && issues[0].Path != "molecule_concurrency.max_running" {
			t.Fatal(n, issues)
		}
	}
	d := minimal()
	d.MoleculeConcurrency = &MoleculeConcurrency{MaxRunning: 2}
	b, _ := json.Marshal(d)
	parsed, e := Parse(b)
	if e != nil || parsed.MoleculeConcurrency == nil || parsed.MoleculeConcurrency.MaxRunning != 2 {
		t.Fatal(parsed.MoleculeConcurrency, e)
	}
	if _, e = Parse([]byte(strings.Replace(string(b), `"max_running":2`, `"max_running":2,"typo":1`, 1))); e == nil {
		t.Fatal("unknown molecule concurrency key accepted")
	}
	if MaxRunningMolecules(minimal()) != -1 || MaxRunningMolecules(d) != 2 {
		t.Fatal("declared limit")
	}
}
