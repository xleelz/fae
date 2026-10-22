package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xleelz/fae/domain"
)

type ContextEntry struct {
	Kind   string `json:"kind"`
	Text   string `json:"text"`
	SHA256 string `json:"sha256"`
}
type Control struct {
	Method, Reason, Information, Context string
	Until                                time.Time
	Priority, AttemptLimit               int
}

func (j Job) AttemptLimit() int {
	if j.TaskAttemptLimit > 0 {
		return j.TaskAttemptLimit
	}
	return j.Document.Limits.Attempts
}
func (j Job) LaterContext() string {
	var b strings.Builder
	for _, x := range j.ContextEntries {
		b.WriteString("\n\n" + x.Kind + ":\n" + x.Text)
	}
	return b.String()
}
func ContextHash(text string) string {
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:])
}

// ApplyControl is the task-control state machine. A held in-flight dispatch is
// retained verbatim until its result is collected or explicitly reconciled.
func ApplyControl(input Job, c Control, now time.Time) (Job, []Event, error) {
	j := clone(input)
	events := []Event{}
	fail := func(s string) (Job, []Event, error) { return input, nil, errors.New(s) }
	if err := Validate(j); err != nil {
		return input, nil, err
	}
	allows := c.Method == "Escalate" || c.Method == "Block" || c.Method == "Reprioritize" || c.Method == "SetAttemptLimit" || c.Method == "ResumeWithContext"
	if !CanChangeTask(j.State.Terminal(), j.InFlight != nil, allows) {
		return fail("task control illegal for terminal or in-flight task")
	}
	from := j.State
	switch c.Method {
	case "Escalate", "Block":
		if strings.TrimSpace(c.Reason) == "" {
			return fail("reason required")
		}
		if c.Method == "Block" {
			stage, ok := j.Document.Stages[string(j.State)]
			if !ok {
				return fail("block requires stage state")
			}
			j.BlockedFrom = string(j.State)
			j.BlockedRoute = stage.Failure
			j.State = domain.Blocked
		} else {
			j.State = domain.Escalated
		}
		j.Reason = "Operator" + c.Method + ": " + c.Reason
		j.Parked = true
		if j.InFlight != nil {
			j.OperatorStage = j.InFlight.Stage
			j.OperatorReason = j.Reason
			if input.Reason == "UncertainDispatch" {
				j.Reason = "UncertainDispatch"
			}
		}
	case "Retire":
		if j.State != domain.Escalated {
			return fail("retire requires Escalated")
		}
		if _, _, err := ParseRetirement(c.Reason); err != nil {
			return fail(err.Error())
		}
		j.State = domain.Retired
		j.Reason = strings.TrimSpace(c.Reason)
		j.Parked = false
	case "Cancel":
		if j.State != domain.Pending && j.State != domain.Blocked {
			return fail("cancel requires Pending or Blocked")
		}
		if strings.TrimSpace(c.Reason) == "" {
			return fail("reason required")
		}
		j.State = domain.Cancelled
		j.Reason = c.Reason
		j.Parked = false
	case "Defer":
		if j.State != domain.Pending {
			return fail("defer requires Pending")
		}
		if !c.Until.IsZero() && !c.Until.After(now) {
			return fail("defer time must be in future")
		}
		j.Deferred = true
		j.DeferredUntil = c.Until
	case "Undefer":
		if j.State != domain.Pending || !j.Deferred {
			return fail("undefer requires deferred Pending")
		}
		j.Deferred = false
		j.DeferredUntil = time.Time{}
	case "Reprioritize":
		if c.Priority < 10 {
			return fail("priority must be at least 10")
		}
	case "SetAttemptLimit":
		if c.AttemptLimit < 1 {
			return fail("attempt limit must be positive")
		}
		j.TaskAttemptLimit = c.AttemptLimit
	case "RestartWithInformation":
		if j.State != domain.Pending && j.State != domain.Escalated {
			return fail("restart requires Pending or Escalated")
		}
		if strings.TrimSpace(c.Information) == "" {
			return fail("information required")
		}
		if j.State == domain.Escalated {
			if j.Reason == "AttemptCap" && j.AttemptLimit() <= j.Attempts {
				return fail("raise attempt limit before restart")
			}
			j.NewAttempt = true
			j.State = domain.State(j.Document.Start)
			j.Parked = false
			j.Reason = ""
		}
		j.ContextEntries = append(j.ContextEntries, ContextEntry{"information", c.Information, ContextHash(c.Information)})
	case "Resume":
		if j.InFlight != nil {
			return fail("resume refuses unresolved dispatch")
		}
		switch j.State {
		case domain.Blocked:
			if _, ok := j.Document.Stages[j.BlockedRoute]; !ok {
				return fail("missing remediation route")
			}
			j.State = domain.State(j.BlockedRoute)
		case domain.Escalated:
			if j.Reason != "AttemptCap" {
				return fail("non-attempt escalation requires restart with information")
			}
			if j.AttemptLimit() <= j.Attempts {
				return fail("raise attempt limit before resume")
			}
			j.NewAttempt = true
			if _, ok := j.Document.Stages[j.AttemptCapStage]; !ok {
				return fail("missing attempt-cap stage; restart requires explicit information")
			}
			j.State = domain.State(j.AttemptCapStage)
		default:
			return fail("resume requires Blocked or AttemptCap escalation")
		}
		j.Parked = false
		j.Reason = ""
	case "ResumeWithContext":
		if strings.TrimSpace(c.Context) == "" {
			return fail("context required")
		}
		j.ContextEntries = append(j.ContextEntries, ContextEntry{"context", c.Context, ContextHash(c.Context)})
		// Resume attaches information without changing stage. Restart is the
		// separate explicit transition out of escalation.
	default:
		return fail("unknown task control")
	}
	if j.State != domain.Pending {
		j.Deferred = false
		j.DeferredUntil = time.Time{}
	}
	if from != j.State {
		event(&j, Event{Kind: "transition", Stage: string(from), From: from, To: j.State, Attempt: max(1, j.Attempts), Cause: "Operator" + c.Method + ": " + c.Reason}, &events)
	}
	if c.Method == "Retire" {
		event(&j, Event{Kind: "retired", From: from, To: j.State, Cause: j.Reason, Attempt: max(1, j.Attempts)}, &events)
	}
	event(&j, Event{Kind: "task-control", Stage: string(from), Attempt: max(1, j.Attempts), Cause: fmt.Sprintf("%s: %s", c.Method, c.Reason)}, &events)
	return j, events, nil
}

// Complete preserves operator holds while collecting the original dispatch's
// evidence and effect outcome; it never launches a replacement turn.
func Complete(input Job, id string, r Result) (Job, []Event, error) {
	if input.OperatorStage == "" {
		return complete(input, id, r)
	}
	if err := Validate(input); err != nil {
		return input, nil, err
	}
	original := clone(input)
	original.State = domain.State(input.OperatorStage)
	original.OperatorStage = ""
	original.OperatorReason = ""
	out, events, err := complete(original, id, r)
	if err != nil {
		return input, nil, err
	}
	for n := range events {
		if events[n].Kind == "transition" {
			events[n].Kind = "held-completion-transition"
			events[n].From = ""
			events[n].To = ""
		}
	}
	for n := len(input.History); n < len(out.History); n++ {
		if out.History[n].Kind == "transition" {
			out.History[n].Kind = "held-completion-transition"
			out.History[n].From = ""
			out.History[n].To = ""
		}
	}
	out.State = input.State
	out.Parked = true
	if out.InFlight != nil {
		out.OperatorStage = input.OperatorStage
		out.OperatorReason = input.OperatorReason
	} else {
		out.OperatorStage = ""
		out.OperatorReason = ""
	}
	if out.Reason != "UncertainDispatch" {
		out.Reason = input.OperatorReason
	}
	return out, events, nil
}
