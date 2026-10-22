// Package runtime contains provider-neutral asynchronous turn contracts.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

type Turn struct{ ID, Task, Role, Worktree, Brief string }
type Outcome struct {
	TurnID string
	Output json.RawMessage
	Class  string
	Err    error
}
type Adapter interface {
	Dispatch(context.Context, Turn) (<-chan Outcome, error)
}
type Script struct {
	Output json.RawMessage
	Delay  time.Duration
	Class  string
}
type Scripted struct {
	mu      sync.Mutex
	Scripts map[string][]Script
	seen    map[string]bool
}

func (s *Scripted) Dispatch(ctx context.Context, t Turn) (<-chan Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.ID == "" || t.Task == "" || t.Role == "" {
		return nil, errors.New("turn identity required")
	}
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	if s.seen[t.ID] {
		return nil, errors.New("uncertain dispatch: turn already dispatched")
	}
	queue := s.Scripts[t.Role]
	if len(queue) == 0 {
		return nil, fmt.Errorf("no scripted output for %s", t.Role)
	}
	step := queue[0]
	step.Output = append(json.RawMessage(nil), step.Output...)
	s.Scripts[t.Role] = queue[1:]
	s.seen[t.ID] = true
	ch := make(chan Outcome, 1)
	go func() {
		defer close(ch)
		timer := time.NewTimer(step.Delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			ch <- Outcome{TurnID: t.ID, Class: "interrupted", Err: ctx.Err()}
		case <-timer.C:
			o := Outcome{TurnID: t.ID, Output: step.Output, Class: step.Class}
			if !json.Valid(o.Output) {
				o.Class = "malformed-output"
				o.Err = errors.New("handoff is not valid JSON")
			}
			ch <- o
		}
	}()
	return ch, nil
}

type EnvironmentDelta struct{ Removed, Set []string }

// Sanitize removes instance selection and permit variables; explicit values win.
func Sanitize(environ []string, explicit map[string]string, prefixes []string) ([]string, EnvironmentDelta, error) {
	values := map[string]string{}
	delta := EnvironmentDelta{}
	prefixes = append([]string{"FAE_"}, prefixes...)
	for _, prefix := range prefixes {
		if prefix == "" {
			return nil, delta, errors.New("empty runtime prefix")
		}
	}
	removed := map[string]bool{}
	for _, item := range environ {
		k, v, ok := strings.Cut(item, "=")
		if !ok || k == "" || strings.ContainsRune(item, 0) {
			return nil, delta, errors.New("invalid environment entry")
		}
		drop := false
		for _, p := range prefixes {
			if p == "" {
				return nil, delta, errors.New("empty runtime prefix")
			}
			if strings.HasPrefix(k, p) {
				drop = true
			}
		}
		if drop {
			removed[k] = true
		} else {
			values[k] = v
		}
	}
	for k, v := range explicit {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) {
			return nil, delta, fmt.Errorf("invalid explicit environment key %q", k)
		}
		values[k] = v
		delta.Set = append(delta.Set, k)
	}
	for k := range removed {
		delta.Removed = append(delta.Removed, k)
	}
	sort.Strings(delta.Removed)
	sort.Strings(delta.Set)
	result := make([]string, 0, len(values))
	for k, v := range values {
		result = append(result, k+"="+v)
	}
	sort.Strings(result)
	return result, delta, nil
}

// CheckCommand authorizes an exact vector. It never invokes a shell or interprets arguments.
func CheckCommand(args []string, allowed [][]string) error {
	if len(args) == 0 {
		return errors.New("empty command")
	}
	for _, a := range args {
		if strings.ContainsRune(a, 0) {
			return errors.New("NUL argument")
		}
	}
	for _, shape := range allowed {
		if len(args) != len(shape) {
			continue
		}
		match := true
		for i := range args {
			if args[i] != shape[i] {
				match = false
				break
			}
		}
		if match {
			return nil
		}
	}
	return errors.New("command shape not allowed")
}
