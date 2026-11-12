package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// TargetPinsKey marks ExternalEffect pins that content cannot know. The daemon
// binds such a stage per task, before the task is pinned, to the arguments of
// the target configuration's installed tool for the stage subject
// (FAE-EXT-002: the daemon pins every external argument). An unbound marker
// never reaches an effect: it is not a vendor schema key.
const TargetPinsKey = "$target"

// TargetPins is the marker content declares for daemon-bound pins.
func TargetPins() map[string]string { return map[string]string{TargetPinsKey: "subject"} }

// IsTargetPinned reports whether pins are exactly the TargetPins marker.
func IsTargetPinned(pins map[string]string) bool {
	return len(pins) == 1 && pins[TargetPinsKey] == "subject"
}

// HasNormalOnly reports whether any stage exists only for Normal dispositions.
func HasNormalOnly(d Document) bool {
	for _, s := range d.Stages {
		if s.NormalOnly && !s.IsGitPush() {
			return true
		}
	}
	return false
}

// LocalProjection removes every Normal-only stage, which is what a Local task
// pins (FAE-WF-080: the code-review sub-workflow is Normal mode only), so a
// Local task never reaches one. Each removed stage's incoming edges, branch
// arms, failure routes and the start move to its success successor; inputs
// bound to removed stages are dropped, as are artifact types and role outputs
// only removed stages used. A document without Normal-only stages is returned
// unchanged. The result must validate.
func LocalProjection(d Document) (Document, error) {
	if !HasNormalOnly(d) {
		return d, nil
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return d, err
	}
	var out Document
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err = dec.Decode(&out); err != nil {
		return d, err
	}
	out.ValidationRecursionCeiling=d.ValidationRecursionCeiling
	removed := map[string]bool{}
	for id, s := range out.Stages {
		if s.NormalOnly && !s.IsGitPush() {
			removed[id] = true
		}
	}
	successor := map[string]string{}
	var resolve func(string, map[string]bool) (string, error)
	resolve = func(id string, seen map[string]bool) (string, error) {
		if !removed[id] {
			return id, nil
		}
		if next, ok := successor[id]; ok {
			return next, nil
		}
		if seen[id] {
			return "", fmt.Errorf("stages.%s: Normal-only stages form a cycle without a Local successor", id)
		}
		seen[id] = true
		s := out.Stages[id]
		if s.Primitive == Branch {
			return "", fmt.Errorf("stages.%s: a Normal-only Branch has no single Local successor", id)
		}
		next := []string{}
		for _, e := range out.Edges {
			if e.From != id || e.To == s.Reverify && s.Reverify != "" || e.To == s.Failure && s.Failure != "" && (s.Primitive == Gate || s.Primitive == ExternalEffect) {
				continue
			}
			if !contains(next, e.To) {
				next = append(next, e.To)
			}
		}
		if len(next) != 1 {
			return "", fmt.Errorf("stages.%s: a Normal-only stage needs exactly one success successor", id)
		}
		to, err := resolve(next[0], seen)
		if err != nil {
			return "", err
		}
		successor[id] = to
		return to, nil
	}
	ids := make([]string, 0, len(removed))
	for id := range removed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if _, err = resolve(id, map[string]bool{}); err != nil {
			return d, err
		}
	}
	route := func(id string) string {
		if removed[id] {
			return successor[id]
		}
		return id
	}
	edges := []Edge{}
	for _, e := range out.Edges {
		if removed[e.From] {
			continue
		}
		e.To = route(e.To)
		duplicate := false
		for _, kept := range edges {
			duplicate = duplicate || kept == e
		}
		if !duplicate {
			edges = append(edges, e)
		}
	}
	out.Edges = edges
	out.Start = route(out.Start)
	dropped, grants := map[string]bool{}, map[string]bool{}
	for _, id := range ids {
		s := out.Stages[id]
		if s.Output != "" {
			dropped[s.Output] = true
		}
		if s.Primitive == ExternalEffect {
			grants[s.Subject] = true
		}
		delete(out.Stages, id)
	}
	for _, s := range out.Stages {
		if s.Primitive == ExternalEffect {
			delete(grants, s.Subject)
		}
	}
	// Effect grants only removed stages used go with them.
	for name, r := range out.Roles {
		var kept []string
		for _, g := range r.Profile.Effects {
			if !grants[g] {
				kept = append(kept, g)
			}
		}
		r.Profile.Effects = kept
		out.Roles[name] = r
	}
	used := map[string]bool{}
	for id, s := range out.Stages {
		s.Failure, s.Default, s.Reverify = route(s.Failure), route(s.Default), route(s.Reverify)
		if s.Cases != nil {
			cases := make([]Case, len(s.Cases))
			for i, c := range s.Cases {
				c.Next = route(c.Next)
				cases[i] = c
			}
			s.Cases = cases
		}
		var inputs []Input
		for _, in := range s.Inputs {
			if !removed[in.Stage] {
				inputs = append(inputs, in)
				used[in.Type] = true
			}
		}
		s.Inputs = inputs
		if s.Output != "" {
			used[s.Output] = true
		}
		for _, x := range append([]*Expr{s.Expr}, casesExprs(s.Cases)...) {
			if x != nil && mentions(*x, removed, used) {
				return d, fmt.Errorf("stages.%s: an expression reads a Normal-only stage", id)
			}
		}
		out.Stages[id] = s
	}
	for name := range dropped {
		if used[name] {
			continue
		}
		delete(out.Artifacts, name)
		for role, r := range out.Roles {
			var outputs []string
			for _, o := range r.Outputs {
				if o != name {
					outputs = append(outputs, o)
				}
			}
			r.Outputs = outputs
			out.Roles[role] = r
		}
	}
	if issues := Validate(out); len(issues) > 0 {
		return d, &ValidationError{Issues: issues}
	}
	return out, nil
}

func casesExprs(cases []Case) []*Expr {
	out := []*Expr{}
	for i := range cases {
		out = append(out, &cases[i].When)
	}
	return out
}

// mentions reports a lookup of a removed stage and records looked-up types.
func mentions(x Expr, removed, types map[string]bool) bool {
	if x.Type != "" {
		types[x.Type] = true
	}
	found := removed[x.Stage]
	for _, a := range x.Args {
		found = mentions(a, removed, types) || found
	}
	return found
}
