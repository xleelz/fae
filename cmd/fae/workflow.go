package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/xleelz/fae/daemon"
	"github.com/xleelz/fae/workflow"
)

// workflowCommand implements the template workflow-* verbs (FAE-CLI-020). Writes
// and stored reads go through the daemon; validate is offline.
func workflowCommand(ctx context.Context, verb string, f *flag.FlagSet, opts *common, args []string, out io.Writer) error {
	id := f.String("workflow", "", "workflow ID")
	name := f.String("name", "", "display name (put; default the workflow ID)")
	content := f.String("content", "", "workflow JSON file")
	revision := f.Int64("revision", 0, "revision (get requires one; simulate defaults to latest)")
	from := f.Int64("from", 0, "diff base revision")
	to := f.Int64("to", 0, "diff target revision")
	reason := f.String("reason", "", "retirement reason")
	scenario := f.String("scenario", "", "simulation scenario JSON file: the simulator's records, or a scripted-adapter scenario such as fae workflow scenario prints")
	catalog := catalogFiles{}
	f.Var(&catalog, "catalog", "offline child catalog; repeatable")
	tree := f.Bool("tree", false, "render rehearsed tree")
	task := f.String("task", "", "simulation task ID (default: the single task named by the scenario)")
	if err := parse(f, args); err != nil {
		return err
	}
	allowed := map[string]string{"workflow-put": "workflow name content", "workflow-get": "workflow revision", "workflow-latest": "workflow", "workflow-list": "workflow", "workflow-retire": "workflow reason", "workflow-diff": "workflow from to", "workflow-validate": "content catalog", "workflow-simulate": "workflow revision content scenario task catalog tree"}[verb]
	var stray string
	f.Visit(func(x *flag.Flag) {
		switch x.Name {
		case "home", "instance", "authorizing-message", "json":
		default:
			if !strings.Contains(" "+allowed+" ", " "+x.Name+" ") {
				stray = x.Name
			}
		}
	})
	if stray != "" {
		return usage("%s does not accept --%s", verb, stray)
	}
	p := daemon.WorkflowParams{ID: *id}
	switch verb {
	case "workflow-put":
		if *content == "" {
			return usage("workflow-put requires --content")
		}
		if err := requireMessage(verb, opts); err != nil {
			return err
		}
		raw, err := readBounded(*content)
		if err != nil {
			return err
		}
		doc, err := workflowPutDocument(raw)
		if err != nil {
			return err
		}
		if *id == "" {
			*id = doc.ID
		}
		if *id != doc.ID {
			return fmt.Errorf("workflow ID differs from document ID")
		}
		return rpc(ctx, *opts, "WorkflowInstall", "", daemon.InstallParams{ID: *id, Name: *name, Document: doc, AuthorizingMessage: opts.message}, out)
	case "workflow-get", "workflow-latest":
		if *id == "" || (verb == "workflow-get" && *revision < 1) {
			return usage("%s requires --workflow%s", verb, map[bool]string{true: " and a positive --revision"}[verb == "workflow-get"])
		}
		p.Revision = *revision
		return rpc(ctx, *opts, "Workflows", "", p, out)
	case "workflow-list":
		return rpc(ctx, *opts, "WorkflowList", "", p, out)
	case "workflow-retire":
		if *id == "" || strings.TrimSpace(*reason) == "" {
			return usage("workflow-retire requires --workflow and --reason")
		}
		if err := requireMessage(verb, opts); err != nil {
			return err
		}
		p.Reason, p.AuthorizingMessage = *reason, opts.message
		return rpc(ctx, *opts, "WorkflowRetire", "", p, out)
	case "workflow-diff":
		if *id == "" || *from < 1 || *to < 1 {
			return usage("workflow-diff requires --workflow and positive --from and --to")
		}
		p.From, p.To = *from, *to
		return rpc(ctx, *opts, "WorkflowDiff", "", p, out)
	case "workflow-validate":
		if *content == "" {
			return usage("workflow-validate requires --content")
		}
		raw, err := readBounded(*content)
		if err != nil {
			return err
		}
		catalogDocs, e := loadWorkflowCatalogs(catalog)
		if e != nil {
			return e
		}
		return validationResult(daemon.ValidateWorkflowCatalog(raw, catalogDocs), out)
	default: // workflow-simulate
		if (*content == "") == (*id == "") || *scenario == "" || *revision < 0 || (*content != "" && *revision != 0) {
			return usage("workflow-simulate requires --scenario and exactly one of --content or --workflow [--revision]")
		}
		raw, err := readBounded(*scenario)
		if err != nil {
			return err
		}
		catalogDocs, e := loadWorkflowCatalogs(catalog)
		if e != nil {
			return e
		}
		raw, e = mergeSimulationCatalog(raw, catalogDocs)
		if e != nil {
			return e
		}
		p.Scenario, p.Revision, p.Task = raw, *revision, *task
		if *content != "" {
			if p.Document, err = readBounded(*content); err != nil {
				return err
			}
			if !json.Valid(p.Document) {
				return fmt.Errorf("workflow content is not JSON")
			}
		}
		if !json.Valid(p.Scenario) {
			return fmt.Errorf("scenario is not JSON")
		}
		var buf bytes.Buffer
		copyOpts := *opts
		copyOpts.json = true
		callErr := rpc(ctx, copyOpts, "WorkflowSimulate", "", p, &buf)
		if buf.Len() == 0 {
			return callErr
		}
		var result workflow.Simulation
		if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
			return err
		}
		return simulationResult(result, *tree, out)

	}
}

// workflowPutDocument applies the new-write rules while stored revisions
// remain readable through workflow.Parse and workflow.Load.
func workflowPutDocument(raw []byte) (workflow.Document, error) {
	return workflow.ParseWrite(raw)
}

// validationResult prints the full report and refuses (exit 1) when invalid.
func validationResult(v daemon.WorkflowValidation, out io.Writer) error {
	if err := jsonOut(out, v); err != nil {
		return err
	}
	if v.Valid {
		return nil
	}
	issues := []string{}
	for _, i := range v.Issues {
		issues = append(issues, i.Error())
	}
	return fmt.Errorf("invalid workflow: %s", strings.Join(issues, "; "))
}
