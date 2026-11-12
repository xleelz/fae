package engine

// recordTurnStats persists every collected session attempt, including a failed
// provider attempt refunded by the execution budget. Numbering never rewinds.
func recordTurnStats(j *Job, d Dispatch, r Result, events *[]Event) {
	for _, stat := range r.TurnStats {
		if stat.Dispatch != d.ID || stat.Task != j.TaskID {
			continue
		}
		duplicate := false
		for _, old := range j.TurnStats {
			if old.Dispatch == stat.Dispatch && old.Session == stat.Session && old.Started.Equal(stat.Started) && old.Finished.Equal(stat.Finished) {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		stat.Sequence = len(j.TurnStats) + 1
		stat.Attempt = 1
		for _, old := range j.TurnStats {
			if old.Stage == stat.Stage && old.Visit == stat.Visit && old.Branch == stat.Branch {
				stat.Attempt++
			}
		}
		j.TurnStats = append(j.TurnStats, stat)
		copy := stat
		event(j, Event{Kind: "turn-stats", Stage: d.Stage, DispatchID: d.ID, Attempt: stat.Attempt, TurnStats: &copy}, events)
	}
}
