package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"github.com/xleelz/fae/cron"
	"github.com/xleelz/fae/daemon"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/loops"
	"github.com/xleelz/fae/workflow"
	"io"
	"time"
)

func scheduleCommand(ctx context.Context, args []string, c common, out io.Writer) error {
	if len(args) == 0 {
		return usage("schedule requires create|list|inspect|pause|resume|retire")
	}
	action := args[0]
	f, opts := flags("schedule "+action, c)
	id := f.String("id", "", "schedule identity")
	kind := f.String("kind", "loop", "schedule kind (cron or loop)")
	role := f.String("role", "", "target role")
	prompt := f.String("prompt", "", "repeated prompt")
	provider := f.String("provider", "", "provider binding")
	model := f.String("model", "", "model binding")
	effort := f.String("effort", "", "reasoning effort")
	interval := f.Int64("interval-seconds", 0, "cadence; zero self-paced")
	max := f.Int64("max-iterations", 0, "optional finite maximum")
	specFile := f.String("spec", "", "cron specification JSON file")
	anchor := f.String("anchor", "", "cron first occurrence (RFC3339)")
	end := f.String("end", "", "cron exclusive end (RFC3339)")
	catchup := f.Int("catch-up-limit", cron.DefaultCatchUp, "maximum retained missed cron occurrences")
	workflowID := f.String("workflow", "default", "cron workflow template")
	revision := f.Int64("revision", 0, "cron workflow revision; zero pins latest on creation")
	title := f.String("title", "", "cron item title")
	tree := f.String("tree", "", "scripted item tree; live uses pinned worktree")
	run := f.String("run-id", "", "cron item run identity")
	priority := f.Int("priority", 10, "cron item priority")
	expiry := f.Duration("expires-in", loops.DefaultExpiry, "mandatory safety expiry duration")
	if err := parse(f, args[1:]); err != nil {
		return err
	}
	if action == "create" && *kind == "cron" {
		var incompatible string
		f.Visit(func(option *flag.Flag) {
			name := option.Name
			if *specFile != "" {
				switch name {
				case "id", "kind", "spec", "home", "instance", "authorizing-message", "json":
				default:
					incompatible = name
				}
			} else {
				switch name {
				case "role", "provider", "model", "effort", "max-iterations", "expires-in":
					incompatible = name
				}
			}
		})
		if incompatible != "" {
			return usage("--%s conflicts with cron specification", incompatible)
		}
	}
	p := daemon.ScheduleParams{Action: action, ID: *id, AuthorizingMessage: opts.message}
	switch action {
	case "create":
		if *kind == "cron" {
			var spec cron.Spec
			if *specFile != "" {
				raw, e := readBounded(*specFile)
				if e != nil {
					return e
				}
				raw, e = domain.CanonicalJSON(raw)
				if e != nil {
					return usage("invalid cron specification: %v", e)
				}
				dec := json.NewDecoder(bytes.NewReader(raw))
				dec.DisallowUnknownFields()
				if e = dec.Decode(&spec); e != nil {
					return usage("invalid cron specification: %v", e)
				}
				var extra any
				if dec.Decode(&extra) != io.EOF {
					return usage("trailing cron specification data")
				}
				if *id != "" && *id != spec.ID {
					return usage("cron id differs from specification")
				}
			} else {
				start, e := time.Parse(time.RFC3339Nano, *anchor)
				if e != nil {
					return usage("valid cron --anchor required")
				}
				var finish time.Time
				if *end != "" {
					finish, e = time.Parse(time.RFC3339Nano, *end)
					if e != nil {
						return usage("invalid cron --end")
					}
				}
				spec = cron.Spec{ID: *id, Kind: "cron", Anchor: start, IntervalSeconds: *interval, End: finish, CatchUpLimit: *catchup, Template: cron.Template{WorkflowID: *workflowID, Revision: *revision, Title: *title, Content: *prompt, Tree: *tree, RunID: *run, Priority: *priority, Mode: "Local"}}
			}
			if spec.Kind != "cron" {
				return usage("cron specification kind must be cron")
			}
			p.Cron = &spec
		} else if *kind == "loop" {
			if *specFile != "" {
				return usage("--spec is only supported for cron")
			}
			if *expiry <= 0 {
				return usage("positive expiry required")
			}
			p.Spec = &loops.Spec{ID: *id, Kind: *kind, Role: *role, Prompt: *prompt, Binding: workflow.Binding{Provider: *provider, Model: *model, Effort: *effort}, IntervalSeconds: *interval, MaxIterations: *max, ExpiresAt: time.Now().UTC().Add(*expiry)}
		} else {
			return usage("unsupported schedule kind")
		}
	case "list", "inspect", "pause", "resume", "retire":
	default:
		return usage("unsupported schedule action")
	}
	return rpc(ctx, *opts, "Schedule", "", p, out)
}
