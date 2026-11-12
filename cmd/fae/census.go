package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/xleelz/fae/daemon"
)

// censusDefaults: census apply's pacing between retirements is an Appendix A
// configurable default (2 s, FAE-CTL-041); zero is allowed.
var censusDefaults = map[string]valueRange{
	"gap": {"0s..1m0s", func(v any) bool { d := v.(time.Duration); return d >= 0 && d <= time.Minute }},
}

func censusCommand(ctx context.Context, args []string, c common, out io.Writer) error {
	apply := len(args) > 0 && args[0] == "apply"
	if apply {
		args = args[1:]
	}
	f, opts := flags("census", c)
	file := f.String("file", "", "proposal JSON for census; reviewed artifact JSON for apply")
	blocked := f.Bool("include-blocked", false, "include Blocked tasks in classification")
	gap := f.Duration("gap", 2*time.Second, "gap between retirement requests (0s..1m0s)")
	fallBack := configurableFlags(f, censusDefaults)
	if err := parse(f, args); err != nil {
		return err
	}
	fallBack()
	if !apply {
		p := daemon.CensusParams{IncludeBlocked: *blocked}
		if *file != "" {
			raw, err := os.ReadFile(*file)
			if err != nil {
				return err
			}
			if err = json.Unmarshal(raw, &p.Proposals); err != nil {
				return err
			}
		}
		return rpc(ctx, *opts, "Census", "", p, out)
	}
	if *file == "" {
		return usage("census apply requires --file")
	}
	if err := requireMessage("census apply", opts); err != nil {
		return err
	}
	raw, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	var a daemon.CensusArtifact
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&a); err != nil {
		return err
	}
	if err = dec.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("trailing census artifact data")
	}
	if err = daemon.ValidateCensusArtifact(a); err != nil {
		return err
	}
	result := struct {
		Completed []string `json:"completed"`
		Failed    string   `json:"failed,omitempty"`
		Error     string   `json:"error,omitempty"`
		Digest    string   `json:"digest"`
	}{Completed: []string{}, Digest: a.Digest}
	finish := func(id string, err error) error {
		result.Failed = id
		if err != nil {
			result.Error = err.Error()
		}
		if outputErr := jsonOut(out, result); outputErr != nil {
			return outputErr
		}
		return err
	}
	for _, row := range a.Rows {
		if row.Classification == "live" {
			continue
		}
		if len(result.Completed) > 0 {
			timer := time.NewTimer(*gap)
			select {
			case <-ctx.Done():
				timer.Stop()
				return finish(row.ID, ctx.Err())
			case <-timer.C:
			}
		}
		p := daemon.CensusApplyParams{Artifact: a, RowID: row.ID, AuthorizingMessage: opts.message}
		if err = rpc(ctx, *opts, "CensusApply", "", p, io.Discard); err != nil {
			return finish(row.ID, err)
		}
		result.Completed = append(result.Completed, row.ID)
	}
	return finish("", nil)
}
