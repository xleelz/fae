package main

import (
	"context"
	"encoding/json"
	"github.com/xleelz/fae/daemon"
	"io"
)

func supplyArtifactCommand(ctx context.Context, verb string, args []string, c common, out io.Writer) error {
	f, o := flags(verb, c)
	p := daemon.SupplyArtifactParams{}
	f.StringVar(&p.ID, "task", "", "task ID")
	f.StringVar(&p.Stage, "stage", "", "pending supplyable AgentTurn")
	file := f.String("file", "", "JSON artifact file")
	if err := parse(f, args); err != nil {
		return err
	}
	if p.ID == "" || *file == "" {
		return usage("supply-artifact requires --task and --file")
	}
	if err := requireMessage(verb, o); err != nil {
		return err
	}
	raw, err := readBounded(*file)
	if err != nil {
		return err
	}
	if !json.Valid(raw) {
		return usage("supplied artifact is not JSON")
	}
	p.Artifact = raw
	p.AuthorizingMessage = o.message
	return rpc(ctx, *o, "SupplyArtifact", "", p, out)
}
