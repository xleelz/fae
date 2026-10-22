package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
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
		if *id == "" || *reason == "" || o.message == "" {
			return fmt.Errorf("adapter-retire requires id, reason and authorizing-message")
		}
		return rpc(ctx, *o, "AdapterRetire", "", map[string]any{"id": *id, "reason": *reason, "authorizing_message": o.message}, out)
	case "adapter-list":
		return rpc(ctx, *o, "AdapterList", "", map[string]any{}, out)
	case "adapter-get":
		if *id == "" || *revision < 0 {
			return fmt.Errorf("adapter-get requires id and nonnegative revision")
		}
		return rpc(ctx, *o, "AdapterGet", "", map[string]any{"id": *id, "revision": *revision}, out)
	default:
		if *id == "" || *file == "" || o.message == "" {
			return fmt.Errorf("adapter-put requires id, content and authorizing-message")
		}
		raw, err := readBounded(*file)
		if err != nil {
			return err
		}
		return rpc(ctx, *o, "AdapterPut", "", map[string]any{"id": *id, "document": json.RawMessage(raw), "authorizing_message": o.message}, out)
	}
}
