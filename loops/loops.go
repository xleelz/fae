// Package loops implements provider-neutral schedules outside the workflow engine.
package loops

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xleelz/fae/workflow"
	"regexp"
	"time"
)

const (
	DefaultExpiry = 7 * 24 * time.Hour
	StartAttempts = 3
)

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,79}$`)

type Spec struct {
	ID              string           `json:"id"`
	Kind            string           `json:"kind"`
	Role            string           `json:"role"`
	Prompt          string           `json:"prompt"`
	Binding         workflow.Binding `json:"binding"`
	IntervalSeconds int64            `json:"interval_seconds,omitempty"`
	MaxIterations   int64            `json:"max_iterations,omitempty"`
	ExpiresAt       time.Time        `json:"expires_at"`
}
type Schedule struct {
	Dispatches         int64           `json:"dispatches"`
	Spec               Spec            `json:"spec"`
	SessionID          string          `json:"session_id"`
	State              string          `json:"state"`
	Iterations         int64           `json:"iterations"`
	StartupNotExecuted int64           `json:"startup_not_executed,omitempty"`
	StartupFailures    int64           `json:"startup_failures,omitempty"`
	InFlight           bool            `json:"in_flight"`
	Next               time.Time       `json:"next"`
	Context            json.RawMessage `json:"context,omitempty"`
	LastOutput         json.RawMessage `json:"last_output,omitempty"`
	Error              string          `json:"error,omitempty"`
}
type Request struct {
	Attempt   int64
	Spec      Spec
	SessionID string
	Iteration int64
	Context   json.RawMessage
}
type Result struct {
	Context            json.RawMessage
	Output             json.RawMessage
	StartupNotExecuted int64
}

// DeferredError is a trusted adapter's positive proof that no provider execution
// occurred and all acquired resources were released. Joined errors lose this proof.
type DeferredError struct{ Cause error }

func (e *DeferredError) Error() string { return "loop admission deferred: " + e.Cause.Error() }

// NotStartedError is a trusted adapter's positive proof that a loop turn
// failed before starting (the transport never ran the bridge, the bridge
// refused before launching the provider, or the provider never acknowledged
// the prompt) and all acquired resources were released. Joined errors lose
// this proof.
type NotStartedError struct{ Cause error }

func (e *NotStartedError) Error() string { return "loop start not executed: " + e.Cause.Error() }
func (e *NotStartedError) Unwrap() error { return e.Cause }

func (s *Schedule) RecordStartupNotExecuted() { s.StartupNotExecuted++ }

type Runner func(context.Context, Request) (Result, error)

func New(s Spec, now time.Time) (Schedule, error) {
	if s.ExpiresAt.IsZero() {
		s.ExpiresAt = now.Add(DefaultExpiry)
	}
	if !identifier.MatchString(s.ID) || !identifier.MatchString(s.Role) || s.Kind != "loop" || s.Prompt == "" || len(s.Prompt) > 65536 || s.IntervalSeconds < 0 || s.IntervalSeconds > 604800 || s.MaxIterations < 0 || !s.ExpiresAt.After(now) || s.ExpiresAt.After(now.Add(365*24*time.Hour)) {
		return Schedule{}, errors.New("invalid loop specification (kind=loop, identity, role, prompt, bounded cadence and future expiry required)")
	}
	return Schedule{Spec: s, SessionID: "loop-" + s.ID, State: "active", Next: now}, nil
}
func (s Schedule) Due(now time.Time) bool {
	return CanDispatch(s.State == "active", s.InFlight, now.Before(s.Spec.ExpiresAt), !now.Before(s.Next), s.Spec.MaxIterations == 0 || s.Iterations < s.Spec.MaxIterations)
}
func (s *Schedule) Bound(now time.Time) {
	if s.State == "active" && !s.InFlight {
		if !now.Before(s.Spec.ExpiresAt) {
			s.State = "expired"
		} else if s.Spec.MaxIterations > 0 && s.Iterations >= s.Spec.MaxIterations {
			s.State = "completed"
		}
	}
}
func (s *Schedule) Recover() {
	if s.InFlight {
		s.State = "uncertain"
		s.Error = "daemon restarted during iteration; explicitly retire schedule"
	}
}
func (s *Schedule) Complete(r Result, err error, now time.Time) {
	s.InFlight = false
	if r.StartupNotExecuted > 0 {
		s.StartupNotExecuted += r.StartupNotExecuted
	}
	if err != nil {
		if _, safe := err.(*DeferredError); safe {
			s.Error = err.Error()
			s.Next = now.Add(5 * time.Second)
			s.Bound(now)
			return
		}
		if _, safe := err.(*NotStartedError); safe {
			if r.StartupNotExecuted == 0 {
				s.StartupNotExecuted++
			}
			s.StartupFailures++
			if s.StartupFailures < StartAttempts {
				s.Error = err.Error()
				s.Next = now
				s.Bound(now)
				return
			}
		}
		s.State = "uncertain"
		s.Error = err.Error()
		return
	}
	if !json.Valid(r.Context) || !json.Valid(r.Output) || len(r.Context) > 1<<20 || len(r.Output) > 1<<20 {
		s.State = "uncertain"
		s.Error = "invalid or oversized loop result"
		return
	}
	s.Context = append(json.RawMessage(nil), r.Context...)
	s.LastOutput = append(json.RawMessage(nil), r.Output...)
	s.Iterations++
	s.StartupFailures = 0
	s.Error = ""
	s.Next = now.Add(time.Duration(s.Spec.IntervalSeconds) * time.Second)
	s.Bound(now)
}

// Scripted is a runnable provider adapter whose durable context counts prompts.
func Scripted(_ context.Context, r Request) (Result, error) {
	var c struct {
		Session string `json:"session"`
		Prompts int64  `json:"prompts"`
	}
	if len(r.Context) > 0 {
		if err := json.Unmarshal(r.Context, &c); err != nil {
			return Result{}, err
		}
		if c.Session != r.SessionID || c.Prompts != r.Iteration-1 {
			return Result{}, errors.New("scripted context mismatch")
		}
	}
	c.Session = r.SessionID
	c.Prompts++
	raw, _ := json.Marshal(c)
	out, _ := json.Marshal(map[string]any{"prompt": r.Spec.Prompt, "iteration": c.Prompts})
	return Result{Context: raw, Output: out}, nil
}

// Validate rejects corrupted durable records before any provider can launch.
func (s Schedule) Validate() error {
	if _, err := New(s.Spec, s.Spec.ExpiresAt.Add(-time.Second)); err != nil {
		return err
	}
	if s.Spec.ExpiresAt.IsZero() || s.SessionID != "loop-"+s.Spec.ID || s.Iterations < 0 || s.StartupNotExecuted < 0 || s.StartupFailures < 0 || s.Spec.MaxIterations > 0 && s.Iterations > s.Spec.MaxIterations || s.Next.IsZero() {
		return errors.New("invalid durable loop bounds or identity")
	}
	switch s.State {
	case "active", "paused", "uncertain", "retired", "completed", "expired":
	default:
		return errors.New("invalid durable loop state")
	}
	if s.State == "completed" && (s.Spec.MaxIterations == 0 || s.Iterations != s.Spec.MaxIterations || s.InFlight) {
		return errors.New("invalid completed loop")
	}
	if len(s.Context) > 1<<20 || len(s.LastOutput) > 1<<20 || len(s.Context) > 0 && !json.Valid(s.Context) || len(s.LastOutput) > 0 && !json.Valid(s.LastOutput) {
		return errors.New("invalid durable loop context")
	}
	return nil
}
