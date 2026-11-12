package workflow

import "encoding/json"

// Request kinds a role may raise through its handoff (FAE-WF-078).
const (
	RequestScopeWiden = "scope-widen"
	RequestRedRework  = "red-rework"
)

// RequestPolicy governs role requests. A From stage's artifact may carry a
// typed "request"; the engine then routes to Judge, an independent role's
// turn whose artifact decides it with a boolean "approved". An approved
// scope widen continues at Widen with the files carried forward; an approved
// RED rework continues at Rework. Rejection and cap exhaustion escalate.
// Scope widens are capped by limits.scope_widens, RED reworks by ReworkCap.
type RequestPolicy struct {
	From      []string `json:"from"`
	Judge     string   `json:"judge"`
	Widen     string   `json:"widen"`
	Rework    string   `json:"rework,omitempty"`
	ReworkCap int      `json:"rework_cap,omitempty"`
}

// RequestEdge reports an engine-routed edge from a requesting stage to the
// judge; it is never the stage's success route.
func RequestEdge(d Document, from, to string) bool {
	return d.Requests != nil && to == d.Requests.Judge && contains(d.Requests.From, from)
}

// IsRequestJudge reports the stage whose routes the engine chooses.
func IsRequestJudge(d Document, id string) bool {
	return d.Requests != nil && id == d.Requests.Judge
}

func schemaProperty(d Document, artifact, name string) (any, bool) {
	a, ok := d.Artifacts[artifact]
	if !ok {
		return nil, true
	}
	var schema struct {
		Properties map[string]struct {
			Type any `json:"type"`
		} `json:"properties"`
	}
	if json.Unmarshal(a.Schema, &schema) != nil || schema.Properties == nil {
		return nil, true
	}
	p, ok := schema.Properties[name]
	return p.Type, ok
}

func validateRequests(d Document, outgoing map[string][]string, bad func(string, string)) {
	p := d.Requests
	if p == nil {
		return
	}
	producerRoles := map[string]bool{}
	for _, s := range d.Stages {
		if ProducesChanges(d,s) {
			producerRoles[s.Role] = true
		}
	}
	if len(p.From) == 0 {
		bad("requests.from", "required")
	}
	fromRoles := map[string]bool{}
	for i, id := range p.From {
		path := "requests.from[" + itoa(i) + "]"
		s, ok := d.Stages[id]
		if !ok || s.Primitive != AgentTurn {
			bad(path, "must name an AgentTurn stage")
			continue
		}
		fromRoles[s.Role] = true
		if _, ok := schemaProperty(d, s.Output, "request"); !ok {
			bad(path, "output artifact must declare a request field")
		}
		if !contains(outgoing[id], p.Judge) {
			bad(path, "requesting stage needs an edge to the judge")
		}
	}
	judge, ok := d.Stages[p.Judge]
	if !ok || judge.Primitive != AgentTurn || contains(p.From, p.Judge) {
		bad("requests.judge", "must name an AgentTurn stage other than the requesting stages")
	} else {
		if fromRoles[judge.Role] || producerRoles[judge.Role] {
			bad("requests.judge", "judge role must differ from every requesting and change-producing role")
		}
		if t, ok := schemaProperty(d, judge.Output, "approved"); !ok || t != nil && t != "boolean" {
			bad("requests.judge", "judge artifact must declare a boolean approved field")
		}
	}
	routes := []string{p.Widen}
	if p.Rework != "" {
		routes = append(routes, p.Rework)
		if p.ReworkCap < 1 {
			bad("requests.rework_cap", "a RED rework route needs a positive cap")
		}
	} else if p.ReworkCap != 0 {
		bad("requests.rework_cap", "requires a RED rework route")
	}
	for _, r := range routes {
		if !contains(outgoing[p.Judge], r) {
			bad("requests", "judge needs an edge to route "+r)
		}
	}
	for _, to := range outgoing[p.Judge] {
		if !contains(routes, to) {
			bad("requests.judge", "judge routes only to the widen and rework routes, not "+to)
		}
	}
}
