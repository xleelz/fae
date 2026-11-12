package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
)

func targetCommand(ctx context.Context, verb string, f *flag.FlagSet, opts *common, args []string, out io.Writer) error {
	reason := f.String("reason", "", "retirement reason")
	id := f.String("id", "", "target identity")
	revision := f.Int64("revision", 0, "target revision; zero selects latest")
	file := f.String("content", "", "target document JSON")
	var from, to *int64
	var changed, commands *string
	if verb == "target-config-diff" {
		from, to = f.Int64("from", 0, "older revision"), f.Int64("to", 0, "newer revision")
	}
	if verb == "target-config-simulate" {
		changed = f.String("changed", "", "comma-separated changed paths of the proposed change")
		commands = f.String("commands", "", "JSON file of command vectors to check against the allowlist")
	}
	if err := parse(f, args); err != nil {
		return err
	}
	switch verb {
	case "target-config-validate":
		if *file == "" {
			return usage("target-config-validate requires --content")
		}
		raw, err := readBounded(*file)
		if err != nil {
			return err
		}
		return rpc(ctx, *opts, "TargetConfigValidate", "", map[string]any{"document": json.RawMessage(raw)}, out)
	case "target-config-diff":
		if *id == "" || *from < 1 || *to < 1 {
			return usage("target-config-diff requires --id, --from and --to")
		}
		return rpc(ctx, *opts, "TargetConfigDiff", "", map[string]any{"id": *id, "from": *from, "to": *to}, out)
	case "target-config-simulate":
		if (*id == "") == (*file == "") || *revision < 0 {
			return usage("target-config-simulate requires exactly one of --id or --content")
		}
		p := map[string]any{"changed": splitList(*changed)}
		if *id != "" {
			p["id"], p["revision"] = *id, *revision
		} else {
			raw, err := readBounded(*file)
			if err != nil {
				return err
			}
			p["document"] = json.RawMessage(raw)
		}
		if *commands != "" {
			raw, err := readBounded(*commands)
			if err != nil {
				return err
			}
			var vectors [][]string
			if err = json.Unmarshal(raw, &vectors); err != nil {
				return errors.New("--commands file must hold a JSON array of command vectors")
			}
			p["commands"] = vectors
		}
		return rpc(ctx, *opts, "TargetConfigSimulate", "", p, out)
	case "target-config-retire":
		if *id == "" || *reason == "" {
			return usage("target-config-retire requires --id and --reason")
		}
		if err := requireMessage(verb, opts); err != nil {
			return err
		}
		return rpc(ctx, *opts, "TargetConfigRetire", "", map[string]any{"id": *id, "reason": *reason, "authorizing_message": opts.message}, out)
	case "target-config-list":
		return rpc(ctx, *opts, "TargetConfigList", "", map[string]any{}, out)
	case "target-config-get":
		if *id == "" || *revision < 0 {
			return usage("target-config-get requires --id and a nonnegative --revision")
		}
		return rpc(ctx, *opts, "TargetConfigGet", "", map[string]any{"id": *id, "revision": *revision}, out)
	default:
		if *id == "" || *file == "" {
			return usage("target-config-put requires --id and --content")
		}
		if err := requireMessage(verb, opts); err != nil {
			return err
		}
		raw, err := readBounded(*file)
		if err != nil {
			return err
		}
		return rpc(ctx, *opts, "TargetConfigPut", "", map[string]any{"id": *id, "document": json.RawMessage(raw), "authorizing_message": opts.message}, out)
	}
}
