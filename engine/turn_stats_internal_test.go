package engine

import (
	"encoding/json"
	"github.com/xleelz/fae/domain"
	"testing"
	"time"
)

func TestTurnStatsSameSessionAttemptsWithinVisitAndReplay(t *testing.T) {
	j := Job{TaskID: "task"}
	d := Dispatch{ID: "dispatch", Stage: "agent"}
	events := []Event{}
	started := time.Now()
	a := domain.NewTurnStat(nil, started, started.Add(time.Second))
	a.Dispatch = d.ID
	a.Task = j.TaskID
	a.Stage = "agent"
	a.Session = "session"
	a.Visit = 2
	b := a
	b.Started = b.Started.Add(time.Second)
	b.Finished = b.Finished.Add(time.Second)
	recordTurnStats(&j, d, Result{TurnStats: []domain.TurnStat{a}}, &events)
	raw, _ := json.Marshal(j)
	if err := json.Unmarshal(raw, &j); err != nil {
		t.Fatal(err)
	}
	recordTurnStats(&j, d, Result{TurnStats: []domain.TurnStat{b}}, &events)
	recordTurnStats(&j, d, Result{TurnStats: []domain.TurnStat{a, b}}, &events)
	if len(j.TurnStats) != 2 || j.TurnStats[0].Attempt != 1 || j.TurnStats[1].Attempt != 2 || j.TurnStats[1].Sequence != 2 || len(events) != 2 {
		t.Fatal(j.TurnStats, events)
	}
	c := b
	c.Visit = 3
	c.Started = c.Started.Add(time.Second)
	c.Finished = c.Finished.Add(time.Second)
	recordTurnStats(&j, d, Result{TurnStats: []domain.TurnStat{c}}, &events)
	if j.TurnStats[2].Attempt != 1 || j.TurnStats[2].Sequence != 3 {
		t.Fatal(j.TurnStats)
	}
}
