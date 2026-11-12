package engine_test

import (
	"context"
	"encoding/json"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"testing"
	"time"
)

func TestTurnStatsExactAbsentAndAttemptSequenceAcrossResume(t *testing.T) {
	j, err := engine.New("stats", 1, defaults.Workflow(), "tree")
	if err != nil {
		t.Fatal(err)
	}
	exec := engine.NewScripted(defaults.HappyScript())
	count := 0
	for step := 0; step < 80 && count < 2; step++ {
		next, d, _, err := engine.Prepare(j)
		if err != nil {
			t.Fatal(err)
		}
		j = next
		if d == nil {
			continue
		}
		result, err := exec.Execute(context.Background(), *d)
		if err != nil {
			t.Fatal(err)
		}
		var provider *domain.ProviderSessionStats
		if count == 0 {
			turns := 7
			cost := 0.42
			provider = &domain.ProviderSessionStats{NumTurns: &turns, CostUSD: &cost}
		}
		stat := domain.NewTurnStat(provider, time.Now().Add(-time.Second), time.Now())
		stat.Task = j.TaskID
		stat.Stage = d.Stage
		stat.Role = d.Sessions[0].Role
		stat.Session = d.Sessions[0].ID
		stat.Dispatch = d.ID
		stat.Visit = d.Visit
		result.TurnStats = []domain.TurnStat{stat}
		j, _, err = engine.Complete(j, d.ID, result)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(j)
		if err = json.Unmarshal(raw, &j); err != nil {
			t.Fatal(err)
		}
		count++
	}
	if len(j.TurnStats) != 2 || j.TurnStats[0].Attempt != 1 || j.TurnStats[1].Attempt != 1 || j.TurnStats[1].Sequence != 2 {
		t.Fatal(j.TurnStats)
	}
	if j.TurnStats[0].ProviderCostUSD.Value == nil || *j.TurnStats[0].ProviderCostUSD.Value != 0.42 || j.TurnStats[0].ProviderCostUSD.Label != "exact" || j.TurnStats[1].ProviderCostUSD.Value != nil || j.TurnStats[1].ProviderCostUSD.Label != "absent" {
		t.Fatal("provider reading invented or lost", j.TurnStats)
	}
	events := 0
	for _, e := range j.History {
		if e.Kind == "turn-stats" {
			events++
		}
	}
	if events != 2 {
		t.Fatal("stats events", events)
	}
}
