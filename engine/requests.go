package engine

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
)

// Request is a role's typed scope-widen or RED-rework request, durable on
// the job from its handoff until the judge decides it (FAE-WF-078).
type Request struct {
	Kind       string   `json:"kind"`
	Files      []string `json:"files,omitempty"`
	Reason     string   `json:"reason"`
	Stage      string   `json:"stage"`
	DispatchID string   `json:"dispatch_id"`
}

// parseRequest reads the request a requesting stage's handoff carries. A
// present but malformed request is a malformed output. A widen file must
// also pass workflow.DeclaredPathSafe, the rule the live scope resolver
// holds every approved file to: a widen the resolver would refuse never
// reaches the judge, so no approval can leave every later dispatch refused.
// Nor may it name a protected path (workflow.ProtectedPath, FAE-SEC-005):
// a widen is model chosen, so it never adds CI configuration, a file that
// makes the operator's tools run programs, or a credential-like file.
func parseRequest(j Job, d Dispatch, records []workflow.Evidence) (*Request, error) {
	p := j.Document.Requests
	if p == nil || !slices.Contains(p.From, d.Stage) || len(records) != 1 {
		return nil, nil
	}
	raw, present := records[0].Fields["request"]
	if !present || raw == nil {
		return nil, nil
	}
	fields, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("request must be an object")
	}
	r := &Request{Stage: d.Stage, DispatchID: d.ID}
	r.Kind, _ = fields["kind"].(string)
	r.Reason, _ = fields["reason"].(string)
	files, _ := fields["files"].([]any)
	for _, f := range files {
		name, _ := f.(string)
		if err := workflow.CheckArg("relative-path", name); err != nil {
			return nil, fmt.Errorf("request file %q: %w", name, err)
		}
		if !workflow.DeclaredPathSafe(name) {
			return nil, fmt.Errorf("request file %q is inside .git: no task file may be repository metadata at any depth", name)
		}
		if why, protected := workflow.ProtectedPath(name); protected {
			return nil, fmt.Errorf("request file %q is %s: a scope widen never adds a protected path; the operator can declare it at admission", name, why)
		}
		if idx := workflow.MatchDenyPathIndex(j.DenyPaths, name); idx >= 0 {
			return nil, fmt.Errorf("request file %q refused by deny_paths[%d]", name, idx)
		} else if workflow.MatchesAnyGlob(name, j.DenyPaths) {
			return nil, fmt.Errorf("request file %q refused by deny_paths", name)
		}
		r.Files = append(r.Files, name)
	}
	for key := range fields {
		if key != "kind" && key != "reason" && key != "files" {
			return nil, errors.New("unknown request field " + key)
		}
	}
	switch {
	case strings.TrimSpace(r.Reason) == "":
		return nil, errors.New("request needs a reason")
	case r.Kind == workflow.RequestScopeWiden && len(r.Files) == 0:
		return nil, errors.New("scope widen needs additional files")
	case r.Kind == workflow.RequestRedRework && (len(files) != 0 || p.Rework == ""):
		return nil, errors.New("RED rework takes no files and needs a declared rework route")
	case r.Kind != workflow.RequestScopeWiden && r.Kind != workflow.RequestRedRework:
		return nil, errors.New("unknown request kind")
	}
	slices.Sort(r.Files)
	return r, nil
}

func requestCap(p workflow.RequestPolicy, limits workflow.Limits, kind string) int {
	if kind == workflow.RequestRedRework {
		return p.ReworkCap
	}
	return limits.ScopeWidens
}

// rejectRequest escalates a refused or cap-exhausted request: a scope widen
// is ScopeWidenRejected (Appendix B); a RED rework needs the operator.
func rejectRequest(j *Job, r Request, detail string, events *[]Event) {
	j.PendingRequest = nil
	if r.Kind == workflow.RequestScopeWiden {
		stop(j, domain.ScopeWidenRejected, detail, events)
		return
	}
	stop(j, domain.HumanDecisionRequired, "RedRework: "+detail, events)
}

// routeRequest records a new request against its durable cap and sends it to
// the judge, or applies the judge's decision. It overrides the completion's
// route and reports whether the task escalated.
func routeRequest(j *Job, d Dispatch, records []workflow.Evidence, r *Request, next *string, routeErr *error, events *[]Event) bool {
	p := j.Document.Requests
	if p == nil {
		return false
	}
	if r != nil {
		if j.RequestCounts == nil {
			j.RequestCounts = map[string]int{}
		}
		count := j.RequestCounts[r.Kind] + 1
		if limit := requestCap(*p, j.Document.Limits, r.Kind); count > limit {
			rejectRequest(j, *r, fmt.Sprintf("%s cap %d exhausted", r.Kind, limit), events)
			return true
		}
		j.RequestCounts[r.Kind] = count
		j.PendingRequest = r
		*next, *routeErr = p.Judge, nil
		event(j, Event{Kind: "request", Stage: d.Stage, DispatchID: d.ID, Attempt: d.Attempt, Cause: bounded(r.Kind + ": " + r.Reason)}, events)
		return false
	}
	if d.Stage != p.Judge {
		return false
	}
	pending := j.PendingRequest
	if pending == nil {
		stop(j, domain.HumanDecisionRequired, "request judge ran without a pending request", events)
		return true
	}
	if len(records) != 1 || records[0].Fields["approved"] != true {
		rejectRequest(j, *pending, "judge rejected: "+pending.Reason, events)
		return true
	}
	j.PendingRequest = nil
	*next, *routeErr = p.Widen, nil
	if pending.Kind == workflow.RequestRedRework {
		*next = p.Rework
	} else {
		for _, f := range pending.Files {
			if idx := workflow.MatchDenyPathIndex(j.DenyPaths, f); idx >= 0 {
				rejectRequest(j, *pending, fmt.Sprintf("request file %q refused by deny_paths[%d]", f, idx), events)
				return true
			} else if workflow.MatchesAnyGlob(f, j.DenyPaths) {
				rejectRequest(j, *pending, fmt.Sprintf("request file %q refused by deny_paths", f), events)
				return true
			}
		}
		j.ApprovedScope = ScopedFiles(j.ApprovedScope, pending.Files)
	}
	event(j, Event{Kind: "request-approved", Stage: d.Stage, DispatchID: d.ID, Attempt: d.Attempt, Cause: bounded(pending.Kind + ": " + strings.Join(pending.Files, ", "))}, events)
	return false
}

// ScopeDroppedEvent is the history event that records approved scope-widen
// paths dropped because the path rules now refuse them
// (dropRefusedApprovedScope).
const ScopeDroppedEvent = "scope-dropped"

// dropRefusedApprovedScope removes from the approved scope every path that
// a widen may no longer add: one inside .git at any depth
// (workflow.DeclaredPathSafe) or a protected path (workflow.ProtectedPath).
// Only a job stored before parseRequest applied those rules can hold one,
// such as a pre-wave-9 widen into a nested .git. The live resolver and the
// bridges never let such a path into a dispatch's scope, so while the job
// kept it every later dispatch was refused, and Retire was the only way out
// (FAE-WF-078). Dropping it records a scope-dropped event naming each path
// and why, and the task continues on the rest of its approved scope. It runs
// only between dispatches, since an in-flight dispatch's scope must equal
// the approved scope (validRequests), and never changes the request counts.
func dropRefusedApprovedScope(j *Job, events *[]Event) {
	if j.InFlight != nil {
		return
	}
	var kept, dropped []string
	for _, name := range j.ApprovedScope {
		why, refused := "inside .git", !workflow.DeclaredPathSafe(name)
		if !refused {
			why, refused = workflow.ProtectedPath(name)
		}
		if !refused {
			if idx := workflow.MatchDenyPathIndex(j.DenyPaths, name); idx >= 0 {
				why, refused = fmt.Sprintf("deny_paths[%d]", idx), true
			}
		}
		if !refused {
			kept = append(kept, name)
			continue
		}
		dropped = append(dropped, fmt.Sprintf("%s (%s)", name, why))
	}
	if len(dropped) == 0 {
		return
	}
	j.ApprovedScope = kept
	event(j, Event{Kind: ScopeDroppedEvent, Stage: string(j.State), Attempt: j.stagedAttempt(), Cause: bounded("approved scope-widen paths the path rules now refuse were dropped from the approved scope; the task continues on the rest of its scope: " + strings.Join(dropped, ", "))}, events)
}

// ScopedFiles is the sorted union of declared and approved files.
func ScopedFiles(declared, approved []string) []string {
	out := append(append([]string(nil), declared...), approved...)
	slices.Sort(out)
	return slices.Compact(out)
}

// validRequests checks the durable request state of a job.
func validRequests(j Job) error {
	for _, n := range j.RequestCounts {
		if n < 0 {
			return errors.New("negative request counter")
		}
	}
	for _, name := range j.ApprovedScope {
		if workflow.CheckArg("relative-path", name) != nil {
			return errors.New("invalid approved scope")
		}
	}
	if !slices.IsSorted(j.ApprovedScope) || len(slices.Compact(slices.Clone(j.ApprovedScope))) != len(j.ApprovedScope) {
		return errors.New("approved scope must be sorted and unique")
	}
	if r := j.PendingRequest; r != nil && (j.Document.Requests == nil || r.Kind != workflow.RequestScopeWiden && r.Kind != workflow.RequestRedRework) {
		return errors.New("invalid pending request")
	}
	if d := j.InFlight; d != nil && !slices.Equal(d.Scope, j.ApprovedScope) {
		return errors.New("in-flight dispatch scope differs from approved scope")
	}
	return nil
}
