package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
)

func targetCommand(ctx context.Context, verb string, f *flag.FlagSet, opts *common, args []string, out io.Writer) error {
	reason := f.String("reason", "", "retirement reason")
	id := f.String("id", "", "target identity")
	revision := f.Int64("revision", 0, "target revision; zero selects latest")
	file := f.String("content", "", "target document JSON")
	if err := parse(f, args); err != nil {
		return err
	}
	switch verb {
	case "target-config-retire":
		if *id == "" || *reason == "" || opts.message == "" {
			return fmt.Errorf("target retirement requires id, reason, authorizing-message")
		}
		return rpc(ctx, *opts, "TargetConfigRetire", "", map[string]any{"id": *id, "reason": *reason, "authorizing_message": opts.message}, out)
	case "target-config-list":
		return rpc(ctx, *opts, "TargetConfigList", "", map[string]any{}, out)
	case "target-config-get":
		if *id == "" || *revision < 0 {
			return fmt.Errorf("target-config-get requires ID and nonnegative revision")
		}
		return rpc(ctx, *opts, "TargetConfigGet", "", map[string]any{"id": *id, "revision": *revision}, out)
	default:
		if *id == "" || *file == "" || opts.message == "" {
			return fmt.Errorf("target-config-put requires id, content and authorizing-message")
		}
		raw, err := readBounded(*file)
		if err != nil {
			return err
		}
		return rpc(ctx, *opts, "TargetConfigPut", "", map[string]any{"id": *id, "document": json.RawMessage(raw), "authorizing_message": opts.message}, out)
	}
}
