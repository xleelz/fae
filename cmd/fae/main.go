// Command fae exposes the implemented local workflow authoring tools.
package main

import (
	"context"
	"fmt"
	"github.com/xleelz/fae/daemon"
	"github.com/xleelz/fae/workflow"
	"io"
	"os"
	"os/signal"
	"syscall"
)

func offline(args []string, out io.Writer) error {
	if len(args) < 3 || args[0] != "workflow" || (args[1] != "validate" && args[1] != "simulate") {
		return usage("usage: fae workflow validate FILE [--catalog FILE]... | workflow simulate FILE SCRIPT TASK [--catalog FILE]... [--tree]")
	}
	base := 3
	if args[1] == "simulate" {
		base = 5
	}
	if len(args) < base {
		return usage("workflow simulate requires FILE SCRIPT TASK")
	}
	paths := []string{}
	tree := false
	for i := base; i < len(args); i++ {
		switch args[i] {
		case "--catalog":
			i++
			if i >= len(args) {
				return usage("--catalog requires FILE")
			}
			paths = append(paths, args[i])
		case "--tree":
			tree = true
		default:
			return usage("unknown workflow option %s", args[i])
		}
	}
	catalog, err := loadWorkflowCatalogs(paths)
	if err != nil {
		return err
	}

	raw, err := readBounded(args[2])
	if err != nil {
		return err
	}
	if args[1] == "validate" {

		return validationResult(daemon.ValidateWorkflowCatalog(raw, catalog), out)
	}
	d, err := workflow.Parse(raw)
	if err != nil {
		return err
	}
	script, err := readBounded(args[3])
	if err != nil {
		return err
	}
	script, err = mergeSimulationCatalog(script, catalog)
	if err != nil {
		return err
	}
	validation := daemon.ValidateWorkflowCatalog(raw, catalogWithScenario(script, catalog))
	if !validation.Valid {
		return validationResult(validation, out)
	}
	result, err := daemon.SimulateWorkflow(d, args[4], script)
	if err != nil {
		return err
	}
	return simulationResult(result, tree, out)
}
func run(args []string, out io.Writer) error { return execute(context.Background(), args, out) }
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := execute(ctx, os.Args[1:], os.Stdout); err != nil {
		// A passed-through child exit code (heavy-op run) carries no message.
		if err.Error() != "" {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(exitCode(err))
	}
}
