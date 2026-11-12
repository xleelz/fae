package workflow

// The engine owns these producers; workflow content cannot impersonate them.
func RunStatsIssues(d Document) []Issue {
	out := []Issue{}
	if _, ok := d.Stages["engine:run_stats"]; ok {
		out = append(out, Issue{"stages.engine:run_stats", "reserved engine-provided stage"})
	}
	if _, ok := d.Artifacts["engine:run_stats"]; ok {
		out = append(out, Issue{"artifacts.engine:run_stats", "reserved engine-provided artifact type"})
	}
	return out
}

const runStatsSchema = `{"type":"object","required":["sequence","tree_root","nodes","tasks","descendants","max_depth"],"properties":{"sequence":{"type":"integer"},"tree_root":{"type":"string"},"tasks":{"type":"integer"},"descendants":{"type":"integer"},"max_depth":{"type":"integer"},"nodes":{"type":"array","items":{"type":"object"}},"stats":{"type":"object","properties":{"sessions":{"type":"integer"},"provider_cost_usd":{"type":"object","properties":{"value":{"type":"number"},"label":{"type":"string"},"source":{"type":"string"}}},"assistant_turns":{"type":"object","properties":{"value":{"type":"number"},"label":{"type":"string"}}},"wall_seconds":{"type":"object","properties":{"value":{"type":"number"},"label":{"type":"string"}}}}}}}`
