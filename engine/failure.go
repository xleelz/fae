package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/secretshape"
	"github.com/xleelz/fae/workflow"
)

func failureText(text string) string {
	b := []byte(text)
	spans := secretshape.Spans(b)
	for i := len(spans) - 1; i >= 0; i-- {
		p := spans[i]
		b = append(append(append([]byte(nil), b[:p[0]]...), []byte("[REDACTED]")...), b[p[1]:]...)
	}
	return bounded(string(b))
}

// routeFailure records kernel evidence and its transition in the same Job
// checkpoint. It never alters the workflow's content or waives a floor.
func routeFailure(j *Job, reason, detail, dispatch string, events *[]Event) error {
	return routeFailureTo(j, j.Document.Stages[string(j.State)].Failure, reason, detail, dispatch, events)
}

func routeFailureTo(j *Job, target, reason, detail, dispatch string, events *[]Event) error {
	stage := string(j.State)
	s := j.Document.Stages[stage]
	if dispatch == "" && j.InFlight != nil {
		dispatch = j.InFlight.ID
	}
	if dispatch == "" {
		dispatch = fmt.Sprintf("%s/%s/failure/%d", j.TaskID, stage, j.Sequence)
	}
	if target == "" && j.BranchContext != nil {
		target = workflow.BranchFailureJoin(j.Document, stage)
	}
	failure := &FailureRoute{Stage: stage, Visit: j.Counts[stage], Reason: reason, Detail: failureText(detail), Handler: target}
	if j.BranchContext != nil {
		failure.Branch = j.BranchContext.Path
	}
	if target == "" {
		event(j, Event{Kind: "stage-failure", Stage: stage, DispatchID: dispatch, Cause: reason + ": " + failureText(detail), Failure: failure}, events)
		stop(j, domain.HumanDecisionRequired, reason+": "+failureText(detail), events)
		return nil
	}
	if j.Attempts >= j.AttemptLimit() {
		stop(j, domain.AttemptCap, "failure route exhausted task attempt limit", events)
		return nil
	}
	fields := map[string]any{"stage": stage, "visit": j.Counts[stage], "attempt": j.stagedAttempt(), "reason": reason, "error": failureText(detail)}
	raw, _ := json.Marshal(fields)
	id, err := domain.ContentHash(raw)
	if err != nil {
		return err
	}
	name := "failure:" + stage
	session := dispatch + "/failure"
	j.Evidence = append(j.Evidence, workflow.Evidence{ID: id, TaskID: j.TaskID, Type: "failure", Version: 1, Stage: name, Tree: j.Tree, Role: "engine", Session: session, Provenance: true, Fields: fields})
	j.Provenance = append(j.Provenance, Provenance{ExecutionKind: j.ExecutionKind, ArtifactID: id, DispatchID: dispatch, TaskID: j.TaskID, Stage: name, Primitive: s.Primitive, Role: "engine", Session: session, Tree: j.Tree, ProducedAt: time.Now().UTC(), WorkflowHash: j.WorkflowHash, WorkflowRevision: j.Revision})
	if j.HandlerAliases == nil {
		j.HandlerAliases = map[string]string{}
	}
	j.HandlerAliases[stage] = target
	j.InFlight = nil
	j.Parked = false
	if j.BranchContext != nil {
		j.Attempts++
		j.BranchAttemptPaid = true
		j.NewAttempt = false
	} else {
		j.NewAttempt = true
	}
	event(j, Event{Kind: "failure-route", Failure: failure, Stage: stage, DispatchID: dispatch, Attempt: j.stagedAttempt(), Cause: reason + ": " + failureText(detail)}, events)
	return move(j, target, "failure "+reason, events)
}

func validateFailureEvidence(j Job, a workflow.Evidence) error {
	stage := strings.TrimPrefix(a.Stage, "failure:")
	s, ok := j.Document.Stages[stage]
	if !ok || workflow.FailureTarget(j.Document, stage) == "" && workflow.BranchFailureJoin(j.Document, stage) == "" || a.Type != "failure" || a.Version != 1 || a.Role != "engine" || !a.Provenance || a.TaskID != j.TaskID {
		return errors.New("invalid failure evidence")
	}
	raw, err := json.Marshal(a.Fields)
	if err != nil {
		return err
	}
	id, err := domain.ContentHash(raw)
	if err != nil || id != a.ID {
		return errors.New("failure artifact identity mismatch")
	}
	if a.Fields["stage"] != stage {
		return errors.New("failure source mismatch")
	}
	for _, p := range j.Provenance {
		if p.ArtifactID == a.ID && p.Stage == a.Stage && p.Session == a.Session && p.TaskID == j.TaskID && p.Tree == a.Tree && p.Role == "engine" && p.DispatchID != "" && p.Primitive == s.Primitive {
			return nil
		}
	}
	return errors.New("failure provenance missing")
}

func handlerEvidence(j Job, in []workflow.Evidence) []workflow.Evidence {
	out := make([]workflow.Evidence, 0, len(in))
	for _, a := range in {
		if a.Type == "failure" && strings.HasPrefix(a.Stage, "failure:") && !workflow.FailureHandlerOnly(j.Document, strings.TrimPrefix(a.Stage, "failure:"), string(j.State)) {
			continue
		}
		out = append(out, a)
		for original, handler := range j.HandlerAliases {
			source := j.Document.Stages[original]
			if a.Stage == handler && a.Type == source.Output {
				alias := a
				alias.Stage = original
				out = append(out, alias)
			}
		}
	}
	return out
}

// ResolveProjection evaluates bounded typed selection, never executable text.
func ResolveProjection(j Job, p workflow.Projection) (any, []string, error) {
	return resolveProjection(j, p, nil)
}
func resolveProjection(j Job, p workflow.Projection, item *any) (any, []string, error) {
	return projectionDetails(j, p, item, nil)
}
func projectionDetails(j Job, p workflow.Projection, item *any, defaults *[]string) (any, []string, error) {
	steps := 0
	ids := []string{}
	var eval func(workflow.Projection, int) (any, error)
	eval = func(p workflow.Projection, depth int) (result any, err error) {
		defer func() {
			if err != nil && len(p.Default) > 0 && strings.Contains(err.Error(), "missing") {
				dec := json.NewDecoder(bytes.NewReader(p.Default))
				dec.UseNumber()
				err = dec.Decode(&result)
				if err == nil && defaults != nil {
					*defaults = append(*defaults, string(p.Default))
				}
			}
		}()
		steps++
		if steps > 10000 || depth > 64 {
			return nil, errors.New("projection bound exceeded")
		}
		if len(p.Literal) > 0 {
			var v any
			dec := json.NewDecoder(bytes.NewReader(p.Literal))
			dec.UseNumber()
			err := dec.Decode(&v)
			return v, err
		}
		if p.Lookup != nil {
			if p.Lookup.Key == nil {
				return nil, errors.New("lookup requires key projection")
			}
			key, err := eval(*p.Lookup.Key, depth+1)
			if err != nil {
				return nil, err
			}
			text, ok := key.(string)
			if !ok {
				return nil, errors.New("lookup key must be string")
			}
			raw, ok := p.Lookup.Table[text]
			if !ok {
				return nil, fmt.Errorf("projection lookup key %s is missing", text)
			}
			var v any
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.UseNumber()
			err = dec.Decode(&v)
			return v, err
		}
		if p.Object != nil {
			out := map[string]any{}
			for k, v := range p.Object {
				value, err := eval(v, depth+1)
				if err != nil {
					return nil, err
				}
				out[k] = value
			}
			return out, nil
		}
		stage, typ := p.Stage, p.Type
		if p.Input != "" {
			stage = "input:" + p.Input
		}
		if p.Source == "tree" {
			stage = "tree"
		}
		var value any
		found := false
		if p.Source == "change_requests" {
			if j.Document.Stages[string(j.State)].Primitive != workflow.ChangeRequest {
				return nil, errors.New("incoming requests may only bind a ChangeRequest stage")
			}
			raw, _ := json.Marshal(j.IncomingRequests)
			_ = json.Unmarshal(raw, &value)
			found = true
			for _, r := range j.IncomingRequests {
				ids = append(ids, "change-request:"+r.ID)
			}
		} else if p.Source == "item" {
			if item != nil {
				value = *item
				found = true
			} else {
				if j.BranchContext != nil {
					value = j.BranchContext.Item
					found = true
				}
				if j.Fan != nil {
					if _, b := j.Fan.activeBranch(); b != nil {
						value = b.Item
						found = true
					}
				}
			}
		} else {
			for _, a := range latestInputs(j) {
				steps++
				if steps > 10000 {
					return nil, errors.New("projection bound exceeded")
				}
				if p.Source == "failure" && a.Type != "failure" {
					continue
				}
				if stage != "" && a.Stage != stage || typ != "" && a.Type != typ {
					continue
				}
				value = a.Fields
				if len(a.Value) > 0 {
					dec := json.NewDecoder(bytes.NewReader(a.Value))
					dec.UseNumber()
					if err := dec.Decode(&value); err != nil {
						return nil, err
					}
				}
				if fields, ok := value.(map[string]any); ok && p.Input != "" && len(fields) == 1 && inputUsesValueWrapper(j, p.Input) {
					if v, exists := fields["value"]; exists {
						value = v
					}
				}
				ids = append(ids, a.ID)
				found = true
			}
		}
		if !found {
			return nil, errors.New("projection source is missing")
		}
		if p.Field != "" {
			for _, key := range strings.Split(p.Field, ".") {
				m, ok := value.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("projection field %s is missing", p.Field)
				}
				value, ok = m[key]
				if !ok {
					return nil, fmt.Errorf("projection field %s is missing", p.Field)
				}
			}
		}
		if p.Where != nil {
			items, ok := value.([]any)
			if !ok {
				return nil, errors.New("projection where requires list")
			}
			out := []any{}
			for _, item := range items {
				steps++
				if steps > 10000 {
					return nil, errors.New("projection bound exceeded")
				}
				x := workflow.Expr{Op: "eq", Args: []workflow.Expr{{Op: "count", Stage: "projection", Path: []string{"items"}, Args: []workflow.Expr{*p.Where}}, {Op: "literal", Value: 1}}}
				evidence := append(latestInputs(j), workflow.Evidence{ID: "projection-item", Stage: "projection", Provenance: true, Fields: map[string]any{"items": []any{item}}})
				g := workflow.Eval(x, evidence)
				for _, id := range g.EvidenceIDs {
					if id != "projection-item" {
						ids = append(ids, id)
					}
				}
				if g.Value == workflow.Unknown {
					return nil, errors.New("projection filter read a missing field")
				}
				if g.Passed {
					out = append(out, item)
				}
			}
			value = out
		}
		if p.Index != nil {
			items, ok := value.([]any)
			if !ok {
				return nil, errors.New("projection index requires list")
			}
			i := *p.Index
			if i < 0 {
				i += len(items)
			}
			if i < 0 || i >= len(items) {
				return nil, fmt.Errorf("projection index %d is missing", *p.Index)
			}
			value = items[i]
		}
		if p.Then != "" {
			for _, key := range strings.Split(p.Then, ".") {
				m, ok := value.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("projection then field %s is missing", p.Then)
				}
				value, ok = m[key]
				if !ok {
					return nil, fmt.Errorf("projection then field %s is missing", p.Then)
				}
			}
		}
		return value, nil
	}
	value, err := eval(p, 0)
	sort.Strings(ids)
	unique := ids[:0]
	for _, id := range ids {
		if len(unique) == 0 || unique[len(unique)-1] != id {
			unique = append(unique, id)
		}
	}
	return value, unique, err
}

func inputUsesValueWrapper(j Job, name string) bool {
	if j.Document.Signature != nil {
		var schema map[string]any
		_ = json.Unmarshal(j.Document.Signature.Inputs[name], &schema)
		if schema["type"] == "object" {
			return false
		}
	}
	if _, object := j.Inputs[name].(map[string]any); object {
		return false
	}
	return true
}

func recordProjection(j *Job, stage, name string, p workflow.Projection, events *[]Event) error {
	return recordProjectionItem(j, stage, name, p, nil, nil, events)
}
func recordProjectionItem(j *Job, stage, name string, p workflow.Projection, item *any, itemIDs []string, events *[]Event) error {
	defaults := []string{}
	_, ids, err := projectionDetails(*j, p, item, &defaults)
	if err != nil {
		return err
	}
	ids = append(ids, itemIDs...)
	seen := map[string]bool{}
	unique := []string{}
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	sort.Strings(unique)
	for _, a := range latestInputs(*j) {
		if a.Stage == "tree" && seen[a.ID] {
			recordTaskDataEvidence(j, []workflow.Evidence{a})
		}
	}
	event(j, Event{Kind: "projection", Stage: stage, Attempt: j.stagedAttempt(), Cause: name, EvidenceIDs: unique}, events)
	for _, value := range defaults {
		event(j, Event{Kind: "projection-default", Stage: stage, Attempt: j.stagedAttempt(), Cause: name + ": " + value, EvidenceIDs: unique}, events)
	}
	return nil
}
