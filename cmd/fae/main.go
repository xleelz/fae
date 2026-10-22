// Command fae exposes the implemented local workflow authoring tools.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
	"io"
	"os"
	"os/signal"
	"syscall"
)

func offline(args []string, out io.Writer) error {
	if len(args) == 1 && args[0] == "version" {
		fmt.Fprintln(out, "fae development (partial requirements implementation)")
		return nil
	}
	if len(args) < 3 || args[0] != "workflow" {
		return fmt.Errorf("usage: fae version | workflow validate FILE | workflow simulate FILE SCRIPT TASK")
	}
	raw, err := os.ReadFile(args[2])
	if err != nil {
		return err
	}
	d, err := workflow.Parse(raw)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	switch args[1] {
	case "validate":
		if len(args) != 3 {
			return fmt.Errorf("usage: fae workflow validate FILE")
		}
		hash, err := workflow.Hash(d)
		if err != nil {
			return err
		}
		return enc.Encode(map[string]any{"valid": true, "content_hash": hash, "workflow": d.ID})
	case "simulate":
		if len(args) != 5 {
			return fmt.Errorf("usage: fae workflow simulate FILE SCRIPT TASK")
		}
		raw, err := os.ReadFile(args[3])
		if err != nil {
			return err
		}
		raw, err = domain.CanonicalJSON(raw)
		if err != nil {
			return err
		}
		var script workflow.Script
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		dec.UseNumber()
		if err = dec.Decode(&script); err != nil {
			return err
		}
		var trailing any
		if dec.Decode(&trailing) != io.EOF {
			return fmt.Errorf("trailing script content")
		}
		result, err := workflow.Simulate(d, args[4], script)
		if err != nil {
			return err
		}
		return enc.Encode(result)
	default:
		return fmt.Errorf("unsupported workflow command %q", args[1])
	}
}
func run(args []string, out io.Writer) error { return execute(context.Background(), args, out) }
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := execute(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(exitCode(err))
	}
}
