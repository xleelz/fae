package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/xleelz/fae/cron"
	"github.com/xleelz/fae/daemon"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/loops"
	"github.com/xleelz/fae/workflow"
	"io"
	"time"
)

// loopDefaults: a loop's safety expiry is an Appendix A configurable default
// (7 days, FAE-RT-014), in whole seconds up to the year loops.New accepts.
var loopDefaults = map[string]valueRange{
	"expires-in": {"a whole number of seconds from 1s to 8760h0m0s", func(v any) bool {
		d := v.(time.Duration)
		return d >= time.Second && d <= maxLoopExpiry && d%time.Second == 0
	}},
}

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
	priority := f.Int("priority", 10, "cron item priority >= 10, larger first (below 10 is refused; omitted takes 10)")
	expiry := f.Duration("expires-in", loops.DefaultExpiry, "mandatory safety expiry (whole seconds, 1s..8760h0m0s), counted by the daemon clock at creation")
	expiresAt := f.String("expires-at", "", "explicit loop expiry (RFC3339), recorded as given; excludes --expires-in")
	fallBack := configurableFlags(f, loopDefaults)
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
				case "role", "provider", "model", "effort", "max-iterations", "expires-in", "expires-at":
					incompatible = name
				}
			}
		})
		if incompatible != "" {
			return usage("--%s conflicts with cron specification", incompatible)
		}
	}
	if action == "create" || action == "pause" || action == "resume" || action == "retire" {
		if err := requireMessage("schedule "+action, opts); err != nil {
			return err
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
				// The file's content is validated input: refusals exit 1, not 2.
				raw, e = domain.CanonicalJSON(raw)
				if e != nil {
					return fmt.Errorf("invalid cron specification: %v", e)
				}
				dec := json.NewDecoder(bytes.NewReader(raw))
				dec.DisallowUnknownFields()
				if e = dec.Decode(&spec); e != nil {
					return fmt.Errorf("invalid cron specification: %v", e)
				}
				var extra any
				if dec.Decode(&extra) != io.EOF {
					return errors.New("trailing cron specification data")
				}
				if *id != "" && *id != spec.ID {
					return errors.New("cron id differs from specification")
				}
				if spec.Template.Priority < 10 {
					return fmt.Errorf("cron priority %d is below 10: priority must be at least 10, larger first (leave it out for 10)", spec.Template.Priority)
				}
			} else {
				if *priority < 10 {
					return usage("schedule create --priority %d is below 10: priority must be at least 10, larger first (omit it for 10)", *priority)
				}
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
				return errors.New("cron specification kind must be cron")
			}
			p.Cron = &spec
		} else if *kind == "loop" {
			if *specFile != "" {
				return usage("--spec is only supported for cron")
			}
			p.Spec = &loops.Spec{ID: *id, Kind: *kind, Role: *role, Prompt: *prompt, Binding: workflow.Binding{Provider: *provider, Model: *model, Effort: *effort}, IntervalSeconds: *interval, MaxIterations: *max}
			explicitIn := false
			f.Visit(func(option *flag.Flag) { explicitIn = explicitIn || option.Name == "expires-in" })
			if *expiresAt != "" {
				at, e := time.Parse(time.RFC3339Nano, *expiresAt)
				if e != nil || explicitIn {
					return usage("--expires-at takes an RFC3339 time and excludes --expires-in")
				}
				p.Spec.ExpiresAt = at.UTC()
			} else {
				// FAE-CLI-002: the daemon computes the expiry from its own
				// clock. An empty or out-of-range --expires-in falls back to
				// the 7-day default (FAE-RT-014).
				fallBack()
				p.ExpiresInSeconds = int64(*expiry / time.Second)
			}
		} else {
			return usage("unsupported schedule kind")
		}
	case "list", "inspect", "pause", "resume", "retire":
	default:
		return usage("unsupported schedule action")
	}
	return rpc(ctx, *opts, "Schedule", "", p, out)
}
