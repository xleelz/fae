package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"github.com/xleelz/fae/daemon"
	"io"
)

// backlogEdgeUsage is the backlog edge sub-verbs' usage; --accept-in-flight
// belongs to add alone (FAE-CLI-015).
const backlogEdgeUsage = "add --from ID --to ID [--kind blocking|relates] [--accept-in-flight] --authorizing-message ID | remove --from ID --to ID [--kind blocking|relates] --authorizing-message ID"

func backlogCommand(ctx context.Context, args []string, c common, out io.Writer) error {
	if len(args) == 0 {
		return usage("backlog admit|edge add|edge remove|show")
	}
	action, args := args[0], args[1:]
	if action == "edge" {
		if len(args) > 0 && isHelp(args[0]) {
			return subcommandHelp("backlog edge", backlogEdgeUsage, c.json || wantsJSON(args), out)
		}
		if len(args) == 0 || (args[0] != "add" && args[0] != "remove") {
			return usage("backlog edge add|remove")
		}
		action = "edge " + args[0]
		args = args[1:]
	}
	// Each action takes only its own flags, so a flag that means nothing to it
	// (--accept-in-flight on edge remove) is a usage error, not ignored.
	f, o := flags("backlog "+action, c)
	var file, state *string
	var inputFlags taskInputFlags
	var includeRetired *bool
	p := daemon.BacklogEdgeParams{}
	switch action {
	case "admit":
		file = f.String("file", "", "JSON payload file")
		inputFlags = addTaskInputFlags(f)
	case "show":
		state = f.String("state", "", "state filter")
		includeRetired = f.Bool("include-retired", false, "include retired tasks")
	case "edge add", "edge remove":
		f.StringVar(&p.From, "from", "", "prerequisite")
		f.StringVar(&p.To, "to", "", "dependent")
		f.StringVar(&p.Kind, "kind", "blocking", "blocking or relates")
		if action == "edge add" {
			f.BoolVar(&p.AcceptInFlight, "accept-in-flight", false, "operator override: add the edge although the dependent was already dispatched; it holds the dependent's next release, not its active turn")
		}
	default:
		return usage("unknown backlog action %s; backlog admit|edge add|edge remove|show", action)
	}
	if err := parse(f, args); err != nil {
		return err
	}
	// A missing operand flag is a usage error (2), found before the citation
	// check's refusal (1) and without dialling the daemon.
	switch {
	case action == "admit" && *file == "":
		return usage("backlog admit requires --file")
	case action != "admit" && action != "show" && (p.From == "" || p.To == ""):
		return usage("backlog %s requires --from and --to", action)
	case action != "admit" && action != "show" && p.Kind != "blocking" && p.Kind != "relates":
		return usage("backlog %s --kind must be blocking or relates", action)
	}
	if action == "show" {
		return rpc(ctx, *o, "BacklogShow", "", map[string]any{"state": *state, "include_retired": *includeRetired}, out)
	}
	if err := requireMessage("backlog "+action, o); err != nil {
		return err
	}
	if action != "admit" {
		p.Remove = action == "edge remove"
		p.AuthorizingMessage = o.message
		return rpc(ctx, *o, "BacklogEdge", "", p, out)
	}
	// The daemon refuses a payload past its audited 64 KiB bound; refuse a
	// larger file before reading it whole.
	b, err := readLimited(*file, 64<<10)
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
		return errors.New("trailing backlog JSON")
	}
	hasInputFlags := false
	f.Visit(func(flag *flag.Flag) {
		switch flag.Name {
		case "input", "from-parent", "parent", "max-sessions":
			hasInputFlags = true
		}
	})
	if hasInputFlags {
		if len(payload.Items) != 1 {
			return usage("input and parent flags require exactly one backlog item; multi-item files carry these fields per item")
		}
		if err = inputFlags.apply(&payload.Items[0].AdmitParams); err != nil {
			return err
		}
	}
	payload.AuthorizingMessage = o.message
	return rpc(ctx, *o, "BacklogAdmit", "", payload, out)
}
func reconcileDoneCommand(ctx context.Context, args []string, c common, out io.Writer) error {
	f, o := flags("reconcile-done", c)
	dry := f.Bool("dry-run", false, "report without writes")
	if err := parse(f, args); err != nil {
		return err
	}
	if err := requireMessage("reconcile-done", o); err != nil {
		return err
	}
	return rpc(ctx, *o, "ReconcileDone", "", map[string]any{"dry_run": *dry, "authorizing_message": o.message}, out)
}
