package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/execution"
	"github.com/xleelz/fae/providerhealth"
	agentruntime "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
)

func prophetRoutes(c Config) ([]workflow.Binding, error) {
	routes := append([]workflow.Binding(nil), c.ProphetRoutes...)
	if len(routes) == 0 {
		routes = []workflow.Binding{{Provider: prophetProvider(c), Model: prophetModel(c), Effort: "medium"}}
	}
	if len(routes) > 8 || c.ProphetMaxFailovers < -1 || c.ProphetMaxFailovers > 7 {
		return nil, errors.New("Prophet routes require at most 8 bindings and -1..7 failovers")
	}
	seen := map[[2]string]bool{}
	for i, r := range routes {
		key := [2]string{r.Provider, r.Model}
		if strings.TrimSpace(r.Provider) == "" || strings.TrimSpace(r.Model) == "" || seen[key] || r.AutoApproval {
			return nil, errors.New("Prophet routes require unique nonblank provider/model bindings without auto approval")
		}
		seen[key] = true
		if r.Effort == "" {
			routes[i].Effort = "medium"
		}
	}
	return routes, nil
}

type prophetAttempt struct {
	collected  agentruntime.Collected
	err        error
	cleanupErr error
	cleanupOK  bool
}

// routeProphet is bounded, read-only failover. A failed durable observation or
// unconfirmed retirement ends the turn, so uncertain work cannot be replayed.
func routeProphet(ctx context.Context, c Config, health func(context.Context) ([]providerhealth.Health, error), run func(context.Context, workflow.Binding, int) prophetAttempt, observe func(context.Context, workflow.Binding, int, providerhealth.Outcome) error) (json.RawMessage, error) {
	routes, err := prophetRoutes(c)
	if err != nil {
		return nil, err
	}
	budget := c.ProphetMaxFailovers
	if budget == 0 {
		budget = 1
	}
	if budget < 0 {
		budget = 0
	}
	attempts := 0
	var last error
	for _, route := range routes {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		hs, e := health(ctx)
		if e != nil {
			return nil, fmt.Errorf("Prophet route health unavailable: %w", e)
		}
		eligible := true
		for _, h := range hs {
			if h.Provider == route.Provider && h.Model == route.Model {
				eligible = h.Eligible(time.Now())
				break
			}
		}
		if !eligible {
			continue
		}
		a := run(ctx, route, attempts)
		attempts++
		runErr := a.err
		if runErr == nil {
			runErr = workflow.ValidateArtifact(json.RawMessage(prophetSchema), a.collected.Output)
			var answer ProphetAnswer
			if runErr == nil {
				runErr = decode(a.collected.Output, &answer)
			}
			if runErr == nil && (len(a.collected.Output) > 1<<20 || answer.Reply == "" || len(answer.Reply) > 65536 || len(answer.Proposals) > 32 || len(answer.Information) > 65536 || len(answer.Context) > 65536) {
				runErr = errors.New("invalid bounded Prophet response")
			}
		}
		observed := true
		if !a.collected.NonExecution {
			var turnError *agentruntime.TurnError
			stalled := errors.As(runErr, &turnError) && turnError.Class == "stalled"
			outcome := providerhealth.Classify(a.collected.LogDirectory, runErr == nil, errors.Is(runErr, context.DeadlineExceeded) || stalled)
			e = observe(ctx, route, attempts-1, outcome)
			observed = e == nil
			runErr = errors.Join(runErr, e)
		}
		if !a.cleanupOK || !a.collected.ProcessesRetired {
			return nil, errors.Join(runErr, a.cleanupErr, errors.New("Prophet process retirement or lease release unconfirmed"))
		}
		if runErr == nil {
			return a.collected.Output, nil
		}
		last = runErr
		if !CanFailoverProphet(observed, a.cleanupOK && a.collected.ProcessesRetired, attempts <= budget) {
			return nil, last
		}
	}
	return nil, errors.Join(last, errors.New("Prophet all-routes-unavailable; submit a new operator turn after recovery"))
}

func (d *Daemon) runProphet(ctx context.Context, task, id string, brief json.RawMessage) (json.RawMessage, error) {
	x := d.live
	dir := filepath.Join(d.layout.Root, "prophet-context")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if task == "" {
		task = "global"
	}
	return routeProphet(ctx, d.config, d.store.ProviderHealth, func(ctx context.Context, route workflow.Binding, attempt int) prophetAttempt {
		sid := id
		if attempt > 0 {
			sid = fmt.Sprintf("%s-route-%d", id, attempt)
		}
		dispatch := engine.Dispatch{ID: sid, TaskID: task, Config: workflow.Stage{Primitive: workflow.Review, Weight: 1}}
		session := engine.Session{ID: sid, Role: "prophet"}
		release, err := x.guard.Acquire(ctx, dispatch, execution.Binding{Worktree: dir, Document: workflow.Document{}}, session)
		if err != nil {
			return prophetAttempt{err: err, collected: agentruntime.Collected{NonExecution: true, ProcessesRetired: true}, cleanupOK: true}
		}
		if err = x.guard.Launching(ctx, sid); err != nil {
			e := release()
			return prophetAttempt{err: errors.Join(err, e), collected: agentruntime.Collected{NonExecution: true, ProcessesRetired: true}, cleanupOK: e == nil}
		}
		result, runErr := x.runner.Run(ctx, agentruntime.Launch{WorkflowID: "fae-prophet", DispatchID: sid, SessionID: sid, TaskID: task, Role: "prophet", Worktree: dir, Mode: "local", FreshContext: true, Profile: workflow.Profile{Allowed: []string{"handoff"}, Denied: []string{"edit", "exec"}}, Binding: route, Brief: brief})
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		retiredErr := x.guard.Retired(cleanup, sid, result.ProcessesRetired)
		releaseErr := release()
		return prophetAttempt{collected: result, err: runErr, cleanupErr: errors.Join(retiredErr, releaseErr), cleanupOK: retiredErr == nil && releaseErr == nil}
	}, func(ctx context.Context, route workflow.Binding, attempt int, outcome providerhealth.Outcome) error {
		sid := id
		if attempt > 0 {
			sid = fmt.Sprintf("%s-route-%d", id, attempt)
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		err := d.store.ObserveProvider(ctx, controlplane.ProviderObservation{ID: sid, TaskID: task, Provider: route.Provider, Model: route.Model, Outcome: outcome, At: time.Now().UTC()})
		if err == nil {
			d.notifyEvents()
		}
		return err
	})
}
