package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"sort"
	"strings"
	"time"
)

type TreeParams struct {
	Task  string `json:"task"`
	Stats bool   `json:"stats,omitempty"`
}
type TreeNode struct {
	FailureRoutes     []workflow.FailureRouteReport `json:"failure_routes"`
	Refusals          []workflow.RefusalReport      `json:"refusals"`
	ID                string                        `json:"id"`
	Workflow          string                        `json:"workflow_id"`
	Revision          int                           `json:"revision"`
	Parent            string                        `json:"parent_task,omitempty"`
	Detached          bool                          `json:"detached"`
	State             string                        `json:"state"`
	Stage             string                        `json:"stage"`
	Depth             int                           `json:"depth"`
	Reason            string                        `json:"reason,omitempty"`
	RecursionRefusals []engine.Event                `json:"recursion_refusals"`
	ChangeRequests    any                           `json:"change_requests,omitempty"`
	Outputs           []workflow.Evidence           `json:"mapped_outputs"`
	Branches          map[string]int                `json:"branches"`
	Stats             *TreeStats                    `json:"stats,omitempty"`
}
type TreeStats struct {
	Handlers          map[string]*TreeStats `json:"handlers,omitempty"`
	started, finished time.Time
	Sessions          int                   `json:"sessions"`
	Turns             domain.NumberStat     `json:"assistant_turns"`
	Cost              domain.NumberStat     `json:"provider_cost_usd"`
	ProviderDuration  domain.NumberStat     `json:"provider_duration_ms"`
	SessionWall       domain.NumberStat     `json:"session_wall_seconds"`
	Wall              domain.NumberStat     `json:"wall_seconds"`
	MissingCost       int                   `json:"sessions_without_cost"`
	Stages            map[string]*TreeStats `json:"stages,omitempty"`
	StageDetails      map[string]*TreeStats `json:"stage_details,omitempty"`
}
type TreePage struct {
	Root        string     `json:"tree_root"`
	Sequence    int64      `json:"sequence"`
	Nodes       []TreeNode `json:"nodes"`
	Tasks       int        `json:"tasks"`
	Descendants int        `json:"descendants"`
	MaxDepth    int        `json:"max_depth"`
	Stats       *TreeStats `json:"stats,omitempty"`
}

func newTreeStats() *TreeStats {
	return &TreeStats{Turns: domain.NumberStat{Label: "absent"}, Cost: domain.NumberStat{Label: "absent"}, ProviderDuration: domain.NumberStat{Label: "absent"}, SessionWall: domain.ExactNumber(0, "fae-measured"), Wall: domain.ExactNumber(0, "fae-measured"), Stages: map[string]*TreeStats{}}
}
func addStat(sum *domain.NumberStat, value domain.NumberStat) {
	if value.Value == nil {
		return
	}
	if sum.Value == nil {
		*sum = domain.ExactNumber(0, value.Source)
	}
	*sum.Value += *value.Value
}
func addSpan(sum *TreeStats, started, finished time.Time) {
	if started.IsZero() || finished.IsZero() || finished.Before(started) {
		return
	}
	if sum.started.IsZero() || started.Before(sum.started) {
		sum.started = started
	}
	if finished.After(sum.finished) {
		sum.finished = finished
	}
	sum.Wall = domain.ExactNumber(sum.finished.Sub(sum.started).Seconds(), "plane-recorded-timestamps")
}
func addTurn(sum *TreeStats, stat domain.TurnStat) {
	sum.Sessions++
	addStat(&sum.Turns, stat.AssistantTurns)
	addStat(&sum.Cost, stat.ProviderCostUSD)
	addStat(&sum.ProviderDuration, stat.ProviderDurationMS)
	addStat(&sum.SessionWall, stat.WallSeconds)
	if stat.ProviderCostUSD.Value == nil {
		sum.MissingCost++
	}
}
func (d *Daemon) treeSnapshot(ctx context.Context, task string, stats bool) (TreePage, error) {
	if task == "" {
		return TreePage{}, errors.New("tree task required")
	}
	for retry := 0; retry < 3; retry++ {
		before, err := d.store.Sequence(ctx)
		if err != nil {
			return TreePage{}, err
		}
		rows, err := d.store.TreeJobs(ctx, task)
		if err != nil {
			return TreePage{}, err
		}
		jobs := map[string]envelope{}
		for _, row := range rows {
			var e envelope
			if err := decodeEnvelope(row.Payload, &e); err != nil {
				return TreePage{}, err
			}
			jobs[row.ID] = e
		}
		selected, ok := jobs[task]
		if !ok {
			return TreePage{}, errors.New("tree task not found")
		}
		root := engine.RootTask(selected.Job)
		page := TreePage{Root: root, Sequence: before, Nodes: []TreeNode{}}
		if stats {
			page.Stats = newTreeStats()
		}
		ids := []string{}
		for id, e := range jobs {
			if engine.RootTask(e.Job) == root {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		for _, id := range ids {
			e := jobs[id]
			j := e.Job
			n := TreeNode{ID: id, Workflow: j.Document.ID, Revision: j.Revision, Detached: j.Detached, State: string(j.State), Stage: string(j.State), Depth: engine.Depth(j), Reason: j.Reason, RecursionRefusals: []engine.Event{}, Outputs: []workflow.Evidence{}, Branches: map[string]int{}}
			n.FailureRoutes, n.Refusals = engine.FailureReports(j)
			if j.Parent != nil {
				n.Parent = j.Parent.Task
				n.Detached = n.Detached || j.Parent.Detached
			}
			for _, event := range j.History {
				if isRecursionRefusal(event) {
					n.RecursionRefusals = append(n.RecursionRefusals, event)
				}
			}
			requests, err := d.changeRequestsForTask(ctx, id)
			if err != nil {
				return TreePage{}, err
			}
			n.ChangeRequests = requests
			for _, evidence := range j.Evidence {
				if j.Document.Signature == nil {
					n.Outputs = append(n.Outputs, evidence)
					continue
				}
				for _, output := range j.Document.Signature.Outputs {
					if evidence.Type == output.Type {
						n.Outputs = append(n.Outputs, evidence)
						break
					}
				}
			}
			if j.Fan != nil {
				n.Branches[j.Fan.Stage] = len(j.Fan.Branches)
			}
			for stageID, stage := range j.Document.Stages {
				if stage.Primitive != workflow.FanOut {
					continue
				}
				if stage.BranchesFrom == nil {
					n.Branches[stageID] = len(stage.Branches)
				}
				for _, artifact := range j.Evidence {
					if artifact.Stage == stage.Join {
						if items, ok := artifact.Fields["items"].([]any); ok {
							n.Branches[stageID] = max(n.Branches[stageID], len(items))
						}
					}
				}
			}

			if stats {
				branchIDs := map[string]map[string]bool{}
				handlerDispatches := handlerDispatchPrimaries(j)
				n.Stats = newTreeStats()
				for _, stat := range j.TurnStats {
					addTurn(n.Stats, stat)
					addTurn(page.Stats, stat)
					stage := stat.Stage
					if stat.FanStage != "" {
						stage = stat.FanStage
					}
					handlerPrimary := handlerDispatches[stat.Dispatch]
					if handlerPrimary != "" {
						stage = handlerPrimary
					}
					stage = topLevelStatsStage(j.Document, stage)
					for _, summary := range []*TreeStats{n.Stats, page.Stats} {
						recordStageDetails(summary, j.Document, stat.Stage, &stat, stat.Started, stat.Finished)
					}
					if n.Stats.Stages[stage] == nil {
						n.Stats.Stages[stage] = newTreeStats()
					}
					if handlerPrimary != "" {
						addHandlerStat(n.Stats.Stages[stage], stat)
						if page.Stats.Stages[stage] == nil {
							page.Stats.Stages[stage] = newTreeStats()
						}
						addHandlerStat(page.Stats.Stages[stage], stat)
					}
					addTurn(n.Stats.Stages[stage], stat)
					addSpan(n.Stats.Stages[stage], stat.Started, stat.Finished)
					if page.Stats.Stages[stage] == nil {
						page.Stats.Stages[stage] = newTreeStats()
					}
					addTurn(page.Stats.Stages[stage], stat)
					addSpan(page.Stats.Stages[stage], stat.Started, stat.Finished)
					if stat.Branch != "" {
						if branchIDs[stage] == nil {
							branchIDs[stage] = map[string]bool{}
						}
						branchIDs[stage][strings.Split(stat.Branch, "/")[0]] = true
						n.Branches[stage] = max(n.Branches[stage], len(branchIDs[stage]))
					}
				}
				dispatchStage := map[string]string{}
				for _, p := range j.Provenance {
					dispatchStage[p.DispatchID] = p.Stage
				}
				runs, err := d.store.RunTimings(ctx, id)
				if err != nil {
					return TreePage{}, err
				}
				for _, run := range runs {
					detail := dispatchStage[run.Dispatch]
					primary := handlerDispatches[run.Dispatch]
					if primary != "" {
						detail = primary
					}
					stage := topLevelStatsStage(j.Document, detail)
					if stage == "" {
						continue
					}
					for _, summary := range []*TreeStats{n.Stats, page.Stats} {
						recordStageDetails(summary, j.Document, detail, nil, run.Started, run.Finished)
					}
					if n.Stats.Stages[stage] == nil {
						n.Stats.Stages[stage] = newTreeStats()
					}
					if page.Stats.Stages[stage] == nil {
						page.Stats.Stages[stage] = newTreeStats()
					}
					addSpan(n.Stats.Stages[stage], run.Started, run.Finished)
					addSpan(page.Stats.Stages[stage], run.Started, run.Finished)
				}
				created, updated, err := d.store.TaskTimes(ctx, id)
				if err != nil {
					return TreePage{}, err
				}

				n.Stats.Wall = domain.ExactNumber(max(0, updated.Sub(created).Seconds()), "plane-task-timestamps")
				addSpan(page.Stats, created, updated)
			}
			page.Nodes = append(page.Nodes, n)
			page.MaxDepth = max(page.MaxDepth, n.Depth)
		}
		page.Tasks = len(page.Nodes)
		page.Descendants = max(0, page.Tasks-1)
		after, err := d.store.Sequence(ctx)
		if err != nil {
			return page, err
		}
		if before == after {
			return page, nil
		}
	}
	return TreePage{}, &api.RPCError{Code: "read-busy", Message: "tree changed while taking coherent plane snapshot; retry"}
}
func (d *Daemon) treeRead(ctx context.Context, raw json.RawMessage) (any, error) {
	var p TreeParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	return d.treeSnapshot(ctx, p.Task, p.Stats)
}

func isRecursionRefusal(e engine.Event) bool {
	if e.Kind == "recursion-budget" || e.Kind == "recursion_budget" || e.Cause == "recursion-budget" {
		return true
	}
	if e.Kind != "sub-workflow-failed" && e.Kind != "failure-route" {
		return false
	}
	return strings.Contains(e.Cause, "recursion_budget:") || strings.Contains(e.Cause, "recursion-budget:")
}
func topLevelStatsStage(d workflow.Document, stage string) string {
	current := stage
	seen := map[string]bool{}
	for !seen[current] {
		seen[current] = true
		owner := ""
		for id, s := range d.Stages {
			if s.Primitive != workflow.FanOut || id == current {
				continue
			}
			if _, ok := workflow.BranchOf(d, id)[current]; ok {
				if owner == "" || id < owner {
					owner = id
				}
			}
		}
		if owner == "" {
			return current
		}
		current = owner
	}
	return stage
}

func recordStageDetails(summary *TreeStats, doc workflow.Document, raw string, stat *domain.TurnStat, start, finish time.Time) {
	if summary.StageDetails == nil {
		summary.StageDetails = map[string]*TreeStats{}
	}
	details := map[string]bool{raw: true}
	top := topLevelStatsStage(doc, raw)
	for id, stage := range doc.Stages {
		if stage.Primitive == workflow.FanOut && id != top {
			if _, ok := workflow.BranchOf(doc, id)[raw]; ok {
				details[id] = true
			}
		}
	}
	for detail := range details {
		if summary.StageDetails[detail] == nil {
			summary.StageDetails[detail] = newTreeStats()
		}
		if stat != nil {
			addTurn(summary.StageDetails[detail], *stat)
		}
		addSpan(summary.StageDetails[detail], start, finish)
	}
}

func handlerPrimaryStage(routes []workflow.FailureRouteReport, stage, branch string) string {
	for i := len(routes) - 1; i >= 0; i-- {
		r := routes[i]
		if r.Handler == stage && r.Branch == branch {
			return r.Stage
		}
	}
	return ""
}
func addHandlerStat(sum *TreeStats, stat domain.TurnStat) {
	if sum.Handlers == nil {
		sum.Handlers = map[string]*TreeStats{}
	}
	if sum.Handlers[stat.Stage] == nil {
		sum.Handlers[stat.Stage] = newTreeStats()
	}
	addTurn(sum.Handlers[stat.Stage], stat)
	addSpan(sum.Handlers[stat.Stage], stat.Started, stat.Finished)
}

func handlerDispatchPrimaries(j engine.Job) map[string]string {
	active := map[string]string{}
	out := map[string]string{}
	for _, e := range j.History {
		if e.Failure != nil && e.Failure.Handler != "" {
			active[e.Failure.Branch+"\x00"+e.Failure.Handler] = e.Failure.Stage
		}
		if e.Kind == "dispatch" {
			if primary := active[e.BranchID+"\x00"+e.Stage]; primary != "" {
				out[e.DispatchID] = primary
			}
		}
		if e.Kind == "transition" {
			delete(active, e.BranchID+"\x00"+string(e.From))
		}
	}
	return out
}
