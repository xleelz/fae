package main

import (
	"context"
	"io"
	"strings"
	"time"

	"github.com/xleelz/fae/daemon"
)

func poolCommand(ctx context.Context, args []string, c common, out io.Writer) error {
	if len(args) == 0 {
		return usage("pool: resize --pool ID [--provider P] [--credential C] --cap N [--actor ACTOR] --reason REASON --authorizing-message ID")
	}
	sub := args[0]
	if sub != "resize" {
		return usage("pool: unknown subcommand %q (expected resize); run fae pool --help", sub)
	}
	f, opts := flags("pool resize", c)
	poolID := f.String("pool", "", "provider concurrency pool ID")
	provider := f.String("provider", "", "provider name to match")
	credential := f.String("credential", "", "credential identity to match")
	capVal := f.Int("cap", 0, "maximum concurrent sessions (>= 1)")
	actor := f.String("actor", "", "actor recording the resize")
	reason := f.String("reason", "", "audit rationale for resizing the pool")
	if err := parse(f, args[1:]); err != nil {
		return err
	}
	if strings.TrimSpace(*poolID) == "" || *capVal < 1 || strings.TrimSpace(*reason) == "" {
		return usage("pool resize requires --pool ID, --cap N (>= 1), and --reason TEXT")
	}
	if err := requireMessage("pool resize", opts); err != nil {
		return err
	}
	return rpc(ctx, *opts, "PoolResize", "", daemon.PoolResizeParams{
		Pool:               strings.TrimSpace(*poolID),
		Provider:           strings.TrimSpace(*provider),
		CredentialIdentity: strings.TrimSpace(*credential),
		Cap:                *capVal,
		Actor:              strings.TrimSpace(*actor),
		Reason:             strings.TrimSpace(*reason),
		AuthorizingMessage: opts.message,
	}, out)
}

func quotaCommand(ctx context.Context, args []string, c common, out io.Writer) error {
	if len(args) == 0 {
		return usage("quota: set|clear [--pool ID | --provider P --model M] [...] --authorizing-message ID")
	}
	sub := args[0]
	switch sub {
	case "set":
		f, opts := flags("quota set", c)
		poolID := f.String("pool", "", "provider pool ID")
		provider := f.String("provider", "", "provider name")
		model := f.String("model", "", "model name")
		until := f.String("until", "", "quota reset time T (RFC3339)")
		duration := f.String("duration", "", "duration until quota reset time T (e.g. 2h)")
		grace := f.String("grace", "60s", "grace duration after T before dispatch resumes (default 60s)")
		cutOff := f.String("cut-off-task", "", "cut-off task ID to re-dispatch first at T")
		actor := f.String("actor", "", "actor recording the quota mark")
		reason := f.String("reason", "operator quota set", "audit rationale")
		if err := parse(f, args[1:]); err != nil {
			return err
		}
		if strings.TrimSpace(*poolID) == "" && (strings.TrimSpace(*provider) == "" || strings.TrimSpace(*model) == "") {
			return usage("quota set requires --pool ID or both --provider P and --model M")
		}
		if (*until == "") == (*duration == "") {
			return usage("quota set requires exactly one of --until RFC3339 or --duration D")
		}
		var resetAt time.Time
		if *until != "" {
			t, err := time.Parse(time.RFC3339, strings.TrimSpace(*until))
			if err != nil {
				return usage("quota set: invalid --until RFC3339 time: %v", err)
			}
			resetAt = t.UTC()
		} else {
			d, err := time.ParseDuration(strings.TrimSpace(*duration))
			if err != nil || d <= 0 {
				return usage("quota set: invalid positive --duration: %v", err)
			}
			resetAt = time.Now().UTC().Add(d)
		}
		graceSec := 60
		if strings.TrimSpace(*grace) != "" {
			gd, err := time.ParseDuration(strings.TrimSpace(*grace))
			if err != nil || gd < 0 {
				return usage("quota set: invalid --grace duration: %v", err)
			}
			graceSec = int(gd / time.Second)
			if graceSec == 0 && gd == 0 {
				graceSec = -1
			}
		}
		if err := requireMessage("quota set", opts); err != nil {
			return err
		}
		return rpc(ctx, *opts, "QuotaSet", "", daemon.QuotaSetParams{
			Pool:               strings.TrimSpace(*poolID),
			Provider:           strings.TrimSpace(*provider),
			Model:              strings.TrimSpace(*model),
			ResetAt:            resetAt,
			GraceSeconds:       graceSec,
			CutOffTaskID:       strings.TrimSpace(*cutOff),
			Actor:              strings.TrimSpace(*actor),
			Reason:             strings.TrimSpace(*reason),
			AuthorizingMessage: opts.message,
		}, out)
	case "clear":
		f, opts := flags("quota clear", c)
		poolID := f.String("pool", "", "provider pool ID")
		provider := f.String("provider", "", "provider name")
		model := f.String("model", "", "model name")
		actor := f.String("actor", "", "actor clearing the quota mark")
		reason := f.String("reason", "operator quota clear", "audit rationale")
		if err := parse(f, args[1:]); err != nil {
			return err
		}
		if strings.TrimSpace(*poolID) == "" && (strings.TrimSpace(*provider) == "" || strings.TrimSpace(*model) == "") {
			return usage("quota clear requires --pool ID or both --provider P and --model M")
		}
		if err := requireMessage("quota clear", opts); err != nil {
			return err
		}
		return rpc(ctx, *opts, "QuotaClear", "", daemon.QuotaClearParams{
			Pool:               strings.TrimSpace(*poolID),
			Provider:           strings.TrimSpace(*provider),
			Model:              strings.TrimSpace(*model),
			Actor:              strings.TrimSpace(*actor),
			Reason:             strings.TrimSpace(*reason),
			AuthorizingMessage: opts.message,
		}, out)
	default:
		return usage("quota: unknown subcommand %q (expected set or clear); run fae quota --help", sub)
	}
}

func pauseCommand(ctx context.Context, args []string, c common, out io.Writer) error {
	if len(args) > 0 && args[0] == "list" {
		f, opts := flags("pause list", c)
		if err := parse(f, args[1:]); err != nil {
			return err
		}
		return rpc(ctx, *opts, "DispatchPauseList", "", struct{}{}, out)
	}
	f, opts := flags("pause", c)
	scope := f.String("scope", "", "pause scope: instance, workflow, task_set, or pool")
	target := f.String("target", "", "scope target ID")
	workflowID := f.String("workflow", "", "workflow ID (for workflow scope)")
	runID := f.String("run-id", "", "run ID (for task_set scope)")
	tasks := f.String("tasks", "", "comma-separated task IDs (for task_set scope)")
	poolID := f.String("pool", "", "provider pool ID (for pool scope)")
	actor := f.String("actor", "", "actor recording the pause")
	reason := f.String("reason", "", "audit rationale for pausing dispatch")
	if err := parse(f, args); err != nil {
		return err
	}
	if strings.TrimSpace(*reason) == "" {
		return usage("pause requires --reason TEXT (or 'fae pause list')")
	}
	s := strings.TrimSpace(*scope)
	if s != "" && s != "instance" && s != "workflow" && s != "task_set" && s != "pool" {
		return usage("pause: invalid --scope %q (expected instance, workflow, task_set, or pool)", s)
	}
	if err := requireMessage("pause", opts); err != nil {
		return err
	}
	return rpc(ctx, *opts, "DispatchPause", "", daemon.DispatchPauseParams{
		Scope:              s,
		Target:             strings.TrimSpace(*target),
		WorkflowID:         strings.TrimSpace(*workflowID),
		RunID:              strings.TrimSpace(*runID),
		TaskIDs:            splitList(*tasks),
		Pool:               strings.TrimSpace(*poolID),
		Actor:              strings.TrimSpace(*actor),
		Reason:             strings.TrimSpace(*reason),
		AuthorizingMessage: opts.message,
	}, out)
}

func unpauseCommand(ctx context.Context, args []string, c common, out io.Writer) error {
	f, opts := flags("unpause", c)
	scope := f.String("scope", "", "pause scope: instance, workflow, task_set, or pool")
	target := f.String("target", "", "scope target ID")
	workflowID := f.String("workflow", "", "workflow ID (for workflow scope)")
	runID := f.String("run-id", "", "run ID (for task_set scope)")
	tasks := f.String("tasks", "", "comma-separated task IDs (for task_set scope)")
	poolID := f.String("pool", "", "provider pool ID (for pool scope)")
	actor := f.String("actor", "", "actor recording the unpause")
	reason := f.String("reason", "operator dispatch unpause", "audit rationale for unpausing dispatch")
	if err := parse(f, args); err != nil {
		return err
	}
	s := strings.TrimSpace(*scope)
	if s != "" && s != "instance" && s != "workflow" && s != "task_set" && s != "pool" {
		return usage("unpause: invalid --scope %q (expected instance, workflow, task_set, or pool)", s)
	}
	if err := requireMessage("unpause", opts); err != nil {
		return err
	}
	return rpc(ctx, *opts, "DispatchUnpause", "", daemon.DispatchPauseParams{
		Scope:              s,
		Target:             strings.TrimSpace(*target),
		WorkflowID:         strings.TrimSpace(*workflowID),
		RunID:              strings.TrimSpace(*runID),
		TaskIDs:            splitList(*tasks),
		Pool:               strings.TrimSpace(*poolID),
		Actor:              strings.TrimSpace(*actor),
		Reason:             strings.TrimSpace(*reason),
		AuthorizingMessage: opts.message,
	}, out)
}
