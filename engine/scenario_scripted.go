package engine

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"github.com/xleelz/fae/workflow"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ScenarioActionSink func(context.Context, Dispatch, workflow.ScenarioRule) error
type ScenarioScripted struct {
	scenario  workflow.Scenario
	mu        sync.Mutex
	generated map[string]bool
	sink      ScenarioActionSink
}

func NewScenarioScripted(s workflow.Scenario) *ScenarioScripted {
	raw, _ := json.Marshal(s)
	out := &ScenarioScripted{generated: map[string]bool{}}
	_ = json.Unmarshal(raw, &out.scenario)
	return out
}
func (s *ScenarioScripted) SetActionSink(sink ScenarioActionSink) {
	s.mu.Lock()
	s.sink = sink
	s.mu.Unlock()
}
func (s *ScenarioScripted) Generated(dispatch string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generated[dispatch]
}
func scenarioRule(s workflow.Scenario, d Dispatch, role string) (workflow.ScenarioRule, bool, error) {
	for _, r := range s.Rules {
		if r.Stage != d.Stage {
			continue
		}
		if r.Role != "" && r.Role != role {
			continue
		}
		pattern := r.TaskGlob
		if r.Task != "" {
			pattern = r.Task
		}
		if pattern != "" {
			ok, err := path.Match(pattern, d.TaskID)
			if err != nil {
				return r, false, fmt.Errorf("scenario task_glob: %w", err)
			}
			if !ok {
				continue
			}
		}
		if r.BranchIndex != nil {
			parts := strings.Split(d.BranchID, "/")
			index, err := strconv.Atoi(strings.TrimPrefix(parts[len(parts)-1], "branch-"))
			if d.BranchIndex != nil {
				index = *d.BranchIndex
				err = nil
			}
			if err != nil || index != *r.BranchIndex {
				continue
			}
		}
		return r, true, nil
	}
	return workflow.ScenarioRule{}, false, nil
}
func scenarioSeed(seed int64, d Dispatch, role string) int64 {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d/%s/%s/%s/%d/%s", seed, d.TaskID, d.Stage, d.BranchID, d.Visit, role)))
	return int64(binary.LittleEndian.Uint64(sum[:8]))
}
func (s *ScenarioScripted) Execute(ctx context.Context, d Dispatch) (Result, error) {
	return s.execute(ctx, d, true)
}
func (s *ScenarioScripted) execute(ctx context.Context, d Dispatch, delay bool) (Result, error) {
	if len(d.Sessions) == 0 {
		return Result{}, fmt.Errorf("scenario dispatch has no sessions")
	}
	r := Result{ExecutionKind: d.ExecutionKind, DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, ProducedAt: time.Now().UTC()}
	rule, matched, err := scenarioRule(s.scenario, d, d.Sessions[0].Role)
	if err != nil {
		return r, err
	}
	if delay && rule.Delay > 0 {
		timer := time.NewTimer(rule.Delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return r, ctx.Err()
		case <-timer.C:
		}
	}
	s.mu.Lock()
	sink := s.sink
	s.mu.Unlock()
	if sink != nil && matched {
		if err := sink(ctx, d, rule); err != nil {
			return r, err
		}
	}
	if rule.Failure != "" {
		r.Class = rule.Failure
		r.Detail = "scenario failure"
		return r, nil
	}
	if d.Config.Primitive == workflow.FreezeArtifact || d.Config.Primitive == workflow.CompareFrozen {
		r.Output, err = rehearsedIdentity(d)
		return r, err
	}
	output := func(role string, literal json.RawMessage) (json.RawMessage, error) {
		if len(literal) > 0 {
			return append(json.RawMessage(nil), literal...), nil
		}
		raw, err := workflow.SchemaValidFixture(d.OutputSchema, scenarioSeed(s.scenario.Seed, d, role))
		if err == nil {
			s.mu.Lock()
			s.generated[d.ID] = true
			r.Generated = true
			s.mu.Unlock()
		}
		return raw, err
	}
	if d.Config.Primitive == workflow.Review {
		for i, session := range d.Sessions {
			rr, _, e := scenarioRule(s.scenario, d, session.Role)
			if e != nil {
				return r, e
			}
			literal := rr.Output
			if len(rule.Reviews) > i {
				literal = rule.Reviews[i]
			}
			raw, e := output(session.Role, literal)
			if e != nil {
				return r, e
			}
			r.Reviews = append(r.Reviews, ReviewResult{Session: session.ID, Role: session.Role, Output: raw})
		}
		return r, nil
	}
	r.Output, err = output(d.Sessions[0].Role, rule.Output)
	return r, err
}

func (s *ScenarioScripted) Match(d Dispatch, role string) (workflow.ScenarioRule, bool, error) {
	return scenarioRule(s.scenario, d, role)
}
func (s *ScenarioScripted) Scenario() workflow.Scenario {
	raw, _ := json.Marshal(s.scenario)
	var out workflow.Scenario
	_ = json.Unmarshal(raw, &out)
	return out
}
