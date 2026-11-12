package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
)

func adapterCommand(ctx context.Context, verb string, f *flag.FlagSet, o *common, args []string, out io.Writer) error {
	reason := f.String("reason", "", "retirement reason")
	id := f.String("id", "", "adapter identity")
	revision := f.Int64("revision", 0, "immutable adapter revision; zero selects latest")
	file := f.String("content", "", "adapter document JSON")
	if err := parse(f, args); err != nil {
		return err
	}
	switch verb {
	case "adapter-retire":
		if *id == "" || *reason == "" {
			return usage("adapter-retire requires --id and --reason")
		}
		if err := requireMessage(verb, o); err != nil {
			return err
		}
		return rpc(ctx, *o, "AdapterRetire", "", map[string]any{"id": *id, "reason": *reason, "authorizing_message": o.message}, out)
	case "adapter-list":
		return rpc(ctx, *o, "AdapterList", "", map[string]any{}, out)
	case "adapter-get":
		if *id == "" || *revision < 0 {
			return usage("adapter-get requires --id and a nonnegative --revision")
		}
		return rpc(ctx, *o, "AdapterGet", "", map[string]any{"id": *id, "revision": *revision}, out)
	default:
		if *id == "" || *file == "" {
			return usage("adapter-put requires --id and --content")
		}
		if err := requireMessage(verb, o); err != nil {
			return err
		}
		raw, err := readBounded(*file)
		if err != nil {
			return err
		}
		return rpc(ctx, *o, "AdapterPut", "", map[string]any{"id": *id, "document": json.RawMessage(raw), "authorizing_message": o.message}, out)
	}
}
