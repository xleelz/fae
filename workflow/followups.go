package workflow

import "errors"

// FollowUps declares which elements of an AgentTurn's output the engine
// records as durable follow-up records (FAE-WF-084): the array at Path whose
// elements satisfy When, evaluated with each element as item.
type FollowUps struct {
	Path []string `json:"path"`
	When Expr     `json:"when"`
}

// Elements returns the elements of the array at path in a's fields that
// satisfy filter; an unknown filter or a non-array field is an error.
func Elements(a Evidence, path []string, filter Expr) ([]any, error) {
	v, reason := lookup(a.Fields, path, nil)
	if reason != "" {
		return nil, errors.New(reason)
	}
	list, ok := v.([]any)
	if !ok {
		return nil, errors.New("field type mismatch")
	}
	out := []any{}
	for _, element := range list {
		e := evaluator{evidence: []Evidence{a}, ids: map[string]bool{}, items: []any{element}}
		switch truth(e.eval(filter, 1)) {
		case True:
			out = append(out, element)
		case Unknown:
			return nil, errors.New("unknown follow-up filter")
		}
	}
	return out, nil
}

func validateFollowUps(d Document, bad func(string, string)) {
	for id, s := range d.Stages {
		f := s.FollowUps
		if f == nil {
			continue
		}
		p := "stages." + id + ".follow_ups"
		if s.Primitive != AgentTurn || len(f.Path) == 0 {
			bad(p, "follow-ups select an array field of an AgentTurn's output")
		}
		if err := validateExpr(f.When, 1, 1); err != nil {
			bad(p+".when", err.Error())
		}
	}
}
