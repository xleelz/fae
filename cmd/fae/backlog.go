package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/xleelz/fae/daemon"
	"io"
	"os"
)

func backlogCommand(ctx context.Context, args []string, c common, out io.Writer) error {
	if len(args) == 0 {
		return usage("backlog admit|edge add|edge remove|show")
	}
	action, args := args[0], args[1:]
	if action == "edge" {
		if len(args) == 0 || (args[0] != "add" && args[0] != "remove") {
			return usage("backlog edge add|remove")
		}
		action = "edge-" + args[0]
		args = args[1:]
	}
	f, o := flags("backlog "+action, c)
	file := f.String("file", "", "JSON payload file")
	includeRetired := f.Bool("include-retired", false, "include retired tasks")
	state := f.String("state", "", "state filter")
	p := daemon.BacklogEdgeParams{}
	f.StringVar(&p.From, "from", "", "prerequisite")
	f.StringVar(&p.To, "to", "", "dependent")
	f.StringVar(&p.Kind, "kind", "blocking", "blocking or relates")
	f.BoolVar(&p.AcceptInFlight, "accept-in-flight", false, "accept hold after active dispatch")
	if err := parse(f, args); err != nil {
		return err
	}
	if action == "show" {
		return rpc(ctx, *o, "BacklogShow", "", map[string]any{"state": *state, "include_retired": *includeRetired}, out)
	}
	if o.message == "" {
		return usage("authorizing message required")
	}
	switch action {
	case "admit":
		if *file == "" {
			return usage("--file required")
		}
		b, err := os.ReadFile(*file)
		if err != nil {
			return err
		}
		var payload daemon.BacklogParams
		decoder := json.NewDecoder(bytes.NewReader(b))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&payload); err != nil {
			return err
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			return usage("trailing backlog JSON")
		}
		payload.AuthorizingMessage = o.message
		return rpc(ctx, *o, "BacklogAdmit", "", payload, out)
	case "edge-add", "edge-remove":
		p.Remove = action == "edge-remove"
		p.AuthorizingMessage = o.message
		return rpc(ctx, *o, "BacklogEdge", "", p, out)
	}
	return usage("unknown backlog action")
}
func reconcileDoneCommand(ctx context.Context, args []string, c common, out io.Writer) error {
	f, o := flags("reconcile-done", c)
	dry := f.Bool("dry-run", false, "report without writes")
	if err := parse(f, args); err != nil {
		return err
	}
	return rpc(ctx, *o, "ReconcileDone", "", map[string]any{"dry_run": *dry, "authorizing_message": o.message}, out)
}
