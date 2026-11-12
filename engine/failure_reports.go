package engine

import (
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
	"strings"
)

// FailureReports reads the engine's recorded reasons, independently of event
// names. Transition/evaluation recovery supports earlier stored revisions.
func FailureReports(j Job) ([]workflow.FailureRouteReport, []workflow.RefusalReport) {
	routes := []workflow.FailureRouteReport{}
	refusals := []workflow.RefusalReport{}
	visits := map[string]int{}
	for i, e := range j.History {
		if e.Kind == "dispatch" || e.Kind == "sub-workflow" || e.Kind == "gate" || e.Kind == "branch-arm" {
			visits[e.BranchID+"\x00"+e.Stage]++
		}
		var f *FailureRoute = e.Failure
		if f == nil && e.Kind == "transition" && e.To != "" {
			s := j.Document.Stages[e.Stage]
			if s.Failure != "" && string(e.To) == s.Failure && !workflow.SharedJoinSuccess(j.Document, e.Stage, string(e.To)) {
				f = &FailureRoute{Stage: e.Stage, Visit: visits[e.BranchID+"\x00"+e.Stage], Branch: e.BranchID, Reason: "gate", Detail: e.Cause, Handler: string(e.To)}
			}
		}
		if f == nil {
			continue
		}
		// The structured event and its following transition describe one edge.
		if e.Failure == nil && i > 0 && j.History[i-1].Failure != nil {
			continue
		}
		if f.Reason == "recursion_budget" || f.Reason == "width" || f.Reason == "binding" {
			refusals = append(refusals, workflow.RefusalReport{Stage: f.Stage, Visit: f.Visit, Branch: f.Branch, Reason: f.Reason, Detail: f.Detail, Handler: f.Handler, Escalated: f.Handler == ""})
		}
		if f.Handler == "" {
			continue
		}
		r := workflow.FailureRouteReport{Stage: f.Stage, Visit: f.Visit, Branch: f.Branch, Reason: f.Reason, Detail: f.Detail, Handler: f.Handler}
		for k := i - 1; k >= 0; k-- {
			prev := j.History[k]
			if prev.BranchID != f.Branch {
				continue
			}
			if prev.Stage == f.Stage && prev.Gate != nil && !prev.Gate.Passed && prev.Record != "" {
				r.Records = append(r.Records, prev.Record)
			}
			if prev.Kind == "transition" && prev.Stage == f.Stage {
				break
			}
		}
		for _, next := range j.History[i+1:] {
			if next.BranchID != f.Branch {
				continue
			}
			if next.Failure != nil && next.Failure.Stage == f.Handler {
				v := false
				r.HandlerPassed = &v
				break
			}
			if next.Kind == "transition" && string(next.From) == f.Handler {
				v := next.To != domain.Escalated && next.To != domain.Retired && next.To != domain.Blocked
				if next.To == domain.State(j.Document.Stages[f.Handler].Failure) && j.Document.Stages[f.Handler].Failure != "" && !workflow.SharedJoinSuccess(j.Document, f.Handler, string(next.To)) {
					v = false
				}
				r.HandlerPassed = &v
				break
			}
		}
		routes = append(routes, r)
	}
	// Legacy refusals were recorded only as an escalation reason. Parse the
	// typed reason prefix, never infer refusal from an event kind.
	for _, e := range j.History {
		if e.Failure != nil {
			continue
		}
		detail := e.Cause
		reason := ""
		for _, name := range []string{"recursion_budget", "width", "binding"} {
			if strings.HasPrefix(detail, name+":") || strings.HasPrefix(detail, "SubWorkflowFailed: "+name+":") {
				reason = name
				break
			}
		}
		if reason == "" {
			continue
		}
		duplicate := false
		for _, r := range refusals {
			duplicate = duplicate || (r.Stage == e.Stage && strings.Contains(detail, r.Detail))
		}
		if !duplicate {
			refusals = append(refusals, workflow.RefusalReport{Stage: e.Stage, Visit: visits[e.BranchID+"\x00"+e.Stage], Branch: e.BranchID, Reason: reason, Detail: detail, Escalated: e.To == domain.Escalated || e.EscalationKind != ""})
		}
	}
	return routes, refusals
}
