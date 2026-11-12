package domain

import "time"

// ProviderSessionStats contains only terminal provider readings, never estimates.
// Pointers distinguish an omitted reading from a provider-reported zero.
type ProviderSessionStats struct {
	SessionID  *string  `json:"session_id,omitempty"`
	Subtype    *string  `json:"subtype,omitempty"`
	IsError    *bool    `json:"is_error,omitempty"`
	NumTurns   *int     `json:"num_turns,omitempty"`
	DurationMS *int64   `json:"duration_ms,omitempty"`
	CostUSD    *float64 `json:"total_cost_usd,omitempty"`
}
type NumberStat struct {
	Value  *float64 `json:"value,omitempty"`
	Label  string   `json:"label"`
	Source string   `json:"source,omitempty"`
}
type TextStat struct {
	Value  *string `json:"value,omitempty"`
	Label  string  `json:"label"`
	Source string  `json:"source,omitempty"`
}
type BoolStat struct {
	Value  *bool  `json:"value,omitempty"`
	Label  string `json:"label"`
	Source string `json:"source,omitempty"`
}
type TurnStat struct {
	RuntimeMounts      map[string]string `json:"runtime_mounts,omitempty"`
	ViewConstructionMS int64             `json:"view_construction_ms,omitempty"`
	ViewCopiedBytes    int64             `json:"view_copied_bytes,omitempty"`
	IsolationPolicy    string            `json:"isolation_policy,omitempty"`
	Task               string            `json:"task"`
	Stage              string            `json:"stage"`
	FanStage           string            `json:"fan_stage,omitempty"`
	Branch             string            `json:"branch,omitempty"`
	Role               string            `json:"role"`
	Dispatch           string            `json:"dispatch"`
	Session            string            `json:"session"`
	Attempt            int               `json:"attempt"`
	Sequence           int               `json:"sequence"`
	DispatchAttempt    int               `json:"dispatch_attempt"`
	Visit              int               `json:"visit"`
	Started            time.Time         `json:"started"`
	Finished           time.Time         `json:"finished"`
	ProviderSession    TextStat          `json:"provider_session"`
	Subtype            TextStat          `json:"result_subtype"`
	Error              BoolStat          `json:"error"`
	AssistantTurns     NumberStat        `json:"assistant_turns"`
	ProviderDurationMS NumberStat        `json:"provider_duration_ms"`
	ProviderCostUSD    NumberStat        `json:"provider_cost_usd"`
	WallSeconds        NumberStat        `json:"wall_seconds"`
}

func ExactNumber(value float64, source string) NumberStat {
	return NumberStat{Value: &value, Label: "exact", Source: source}
}
func NewTurnStat(provider *ProviderSessionStats, started, finished time.Time) TurnStat {
	s := TurnStat{Started: started, Finished: finished, ProviderSession: TextStat{Label: "absent"}, Subtype: TextStat{Label: "absent"}, Error: BoolStat{Label: "absent"}, AssistantTurns: NumberStat{Label: "absent"}, ProviderDurationMS: NumberStat{Label: "absent"}, ProviderCostUSD: NumberStat{Label: "absent"}, WallSeconds: ExactNumber(finished.Sub(started).Seconds(), "fae-measured")}
	if provider == nil {
		return s
	}
	if provider.SessionID != nil {
		s.ProviderSession = TextStat{provider.SessionID, "exact", "provider-stated"}
	}
	if provider.Subtype != nil {
		s.Subtype = TextStat{provider.Subtype, "exact", "provider-stated"}
	}
	if provider.IsError != nil {
		s.Error = BoolStat{provider.IsError, "exact", "provider-stated"}
	}
	if provider.NumTurns != nil {
		s.AssistantTurns = ExactNumber(float64(*provider.NumTurns), "provider-stated")
	}
	if provider.DurationMS != nil {
		s.ProviderDurationMS = ExactNumber(float64(*provider.DurationMS), "provider-stated")
	}
	if provider.CostUSD != nil {
		s.ProviderCostUSD = ExactNumber(*provider.CostUSD, "provider-stated")
	}
	return s
}

type ToolEvent struct {
	Tool    string    `json:"tool"`
	Paths   []string  `json:"paths"`
	Allowed bool      `json:"allowed"`
	At      time.Time `json:"at"`
}
