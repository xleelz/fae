package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/daemon"
)

// taskUsage is the `fae task` group's usage (daemon/TASKS.md).
const taskUsage = "create --id ID [--title TEXT] --content TEXT|--content-file FILE [--priority N>=10] [--source-ref REF] [--write-class CLASS] [--declared-file PATH]... [--test-filter REGEX] [--test-shape SHAPE] [--workflow ID [--revision N]] [--target-config ID [--target-revision N]] [--task-class CLASS] [--mode local | --mode normal --dry-run|--commit-external] [--depends-on ID]... [--blocks ID]... [--relates-to ID]... [--accept-in-flight] [--idempotence-key KEY] --authorizing-message ID | create --file JSON [--idempotence-key KEY] --authorizing-message ID | update --id ID [--title TEXT] [--content TEXT|--content-file FILE] [--priority N>=10] [--source-ref REF] [--write-class CLASS] [--declared-file PATH]... [--clear-declared-files] [--test-filter REGEX] [--test-shape SHAPE] [--expected-version N] [--idempotence-key KEY] --authorizing-message ID | update --file JSON [--idempotence-key KEY] --authorizing-message ID | delete --id ID --reason TEXT [--superseded-by ID] [--expected-version N] [--idempotence-key KEY] --authorizing-message ID | show --id ID [--include-retired]"

// taskVerbs maps each `fae task` verb to its one RPC (FAE-API-010).
var taskVerbs = map[string]string{"create": "TaskCreate", "update": "TaskUpdate", "delete": "TaskDelete", "show": "TaskGet"}

// idList is a repeatable task-identity flag.
type idList []string

func (l *idList) String() string { return strings.Join(*l, ",") }
func (l *idList) Set(id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("a task identity cannot be empty")
	}
	*l = append(*l, id)
	return nil
}

// taskCommand implements `fae task create|update|delete|show`, thin clients
// of TaskCreate, TaskUpdate, TaskDelete and TaskGet.
func taskCommand(ctx context.Context, args []string, c common, out io.Writer) error {
	if len(args) == 0 {
		return usage("task create|update|delete|show (fae task --help lists their flags)")
	}
	verb, args := args[0], args[1:]
	method, ok := taskVerbs[verb]
	if !ok {
		return usage("unknown task verb %s; task create|update|delete|show", verb)
	}
	f, o := flags("task "+verb, c)
	id := f.String("id", "", "task ID")
	key := ""
	if verb != "show" {
		f.StringVar(&key, "idempotence-key", "", "request key: a retry with the same key and request returns the original result and writes nothing")
	}
	switch verb {
	case "show":
		includeRetired := f.Bool("include-retired", false, "read a retired (deleted) task")
		if err := parse(f, args); err != nil {
			return err
		}
		if *id == "" {
			return usage("task show requires --id")
		}
		return rpc(ctx, *o, method, "", daemon.TaskGetParams{ID: *id, IncludeRetired: *includeRetired}, out)
	case "delete":
		p := daemon.TaskDeleteParams{}
		f.StringVar(&p.Reason, "reason", "", "why the task is deleted: the retirement's obsolete ruling, or with --superseded-by (retirement superseded-by ID) the escalation's reason and the audit's input")
		f.StringVar(&p.SupersededBy, "superseded-by", "", "an existing task that takes over the deleted task's blocking dependents")
		f.Int64Var(&p.ExpectedVersion, "expected-version", 0, "refuse unless the task is at this version (fae task show's version)")
		if err := parse(f, args); err != nil {
			return err
		}
		if *id == "" || strings.TrimSpace(p.Reason) == "" {
			return usage("task delete requires --id and --reason")
		}
		if err := requireMessage("task delete", o); err != nil {
			return err
		}
		p.ID, p.AuthorizingMessage = *id, o.message
		return rpc(ctx, *o, method, key, p, out)
	}
	file := f.String("file", "", "JSON parameters ("+method+"), in place of the field flags")
	title := f.String("title", "", "task title")
	content := f.String("content", "", "the task's description: the acceptance text its briefs carry")
	contentFile := f.String("content-file", "", "file holding the content (excludes --content)")
	priority := f.Int("priority", 10, "priority >= 10, larger first (below 10 is refused; default 10 on create; unchanged when omitted on update)")
	source := f.String("source-ref", "", "work item source reference")
	writeClass := f.String("write-class", "", "write class tags")
	declared := &declaredFiles{}
	f.Var(declared, "declared-file", "a file the task declares; repeat for each")
	testFilter := f.String("test-filter", "", "the required test filter: a regular expression as go test -run reads it (unanchored matches by substring; ^ and $ anchor; a / names subtests)")
	testShape := f.String("test-shape", "", "the test command shape, one of the supported test shapes (go-unit, go-integration, rust-unit, python-pytest, node-test, script, runner); recorded, not yet consumed")
	var create struct {
		workflow, target, taskClass, mode *string
		revision, targetRevision          *int64
		dryRun, commitExternal, accept    *bool
		dependsOn, blocks, relates        idList
	}
	var update struct {
		clear   *bool
		version *int64
	}
	if verb == "create" {
		create.workflow = f.String("workflow", "", "workflow selector (default the instance default workflow)")
		create.revision = f.Int64("revision", 0, "pinned workflow revision; 0 means latest non-retired")
		create.target = f.String("target-config", "", "target configuration selector")
		create.targetRevision = f.Int64("target-revision", 0, "target revision; zero selects latest")
		create.taskClass = f.String("task-class", "", "declared task class (default: the workflow's default class)")
		create.mode = f.String("mode", "", "local or normal (default: the instance --default-mode)")
		create.dryRun = f.Bool("dry-run", false, "normal mode recording intended external effects only")
		create.commitExternal = f.Bool("commit-external", false, "normal mode with pinned live external effects")
		create.accept = f.Bool("accept-in-flight", false, "operator override: a --blocks edge may hold an already dispatched dependent's next release")
		f.Var(&create.dependsOn, "depends-on", "a blocking prerequisite of the new task; repeat for each")
		f.Var(&create.blocks, "blocks", "a task the new task blocks (its blocking dependent); repeat for each")
		f.Var(&create.relates, "relates-to", "a task the new task relates to (never gates); repeat for each")
	} else {
		update.clear = f.Bool("clear-declared-files", false, "remove every declared file")
		update.version = f.Int64("expected-version", 0, "refuse unless the task is at this version (fae task show's version)")
	}
	if err := parse(f, args); err != nil {
		return err
	}
	set := map[string]bool{}
	f.Visit(func(x *flag.Flag) { set[x.Name] = true })
	if *file != "" {
		for name := range set {
			if name != "file" && name != "idempotence-key" && name != "authorizing-message" && name != "json" && name != "home" && name != "instance" {
				return usage("task %s --file excludes --%s", verb, name)
			}
		}
	}
	if *content != "" && *contentFile != "" {
		return usage("--content and --content-file are exclusive")
	}
	// FAE-CLI-002: a priority below 10 is refused before the daemon is
	// dialled, as run refuses it; only an omitted priority takes 10.
	if set["priority"] && *priority < 10 {
		return usage("task %s --priority %d is below 10: priority must be at least 10, larger first (omit it for 10)", verb, *priority)
	}
	if verb == "update" && *update.clear && set["declared-file"] {
		return usage("--clear-declared-files excludes --declared-file")
	}
	if *file == "" && *id == "" {
		return usage("task %s requires --id (or --file)", verb)
	}
	if verb == "create" && *file == "" && *content == "" && *contentFile == "" {
		return usage("task create requires --content or --content-file (or --file)")
	}
	if *contentFile != "" {
		text, err := inputText("", *contentFile)
		if err != nil {
			return err
		}
		*content, set["content"] = text, true
	}
	if err := requireMessage("task "+verb, o); err != nil {
		return err
	}
	if *file != "" {
		raw, err := readBounded(*file)
		if err != nil {
			return err
		}
		var params any = &daemon.TaskUpdateParams{}
		if verb == "create" {
			params = &daemon.TaskCreateParams{}
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(params); err != nil {
			return err
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			return errors.New("trailing task JSON")
		}
		var given *int
		switch p := params.(type) {
		case *daemon.TaskCreateParams:
			p.AuthorizingMessage, given = o.message, p.Priority
		case *daemon.TaskUpdateParams:
			p.AuthorizingMessage, given = o.message, p.Priority
		}
		if given != nil && *given < 10 {
			return fmt.Errorf("task %s --file: priority %d is below 10: priority must be at least 10, larger first (leave it out for 10)", verb, *given)
		}
		return rpc(ctx, *o, method, key, params, out)
	}
	if verb == "create" {
		if *create.dryRun && *create.commitExternal {
			return usage("--dry-run and --commit-external are mutually exclusive")
		}
		mode := strings.ToLower(*create.mode)
		if mode == "" && (*create.dryRun || *create.commitExternal) {
			mode = "normal"
		}
		if mode != "" && mode != "local" && mode != "normal" {
			return usage("--mode must be local or normal")
		}
		if mode == "local" && (*create.dryRun || *create.commitExternal) {
			return usage("--dry-run and --commit-external require --mode normal")
		}
		if !daemon.SubmissionModeComplete(mode == "normal", *create.dryRun, *create.commitExternal) {
			return usage("--mode normal requires exactly one of --dry-run or --commit-external")
		}
		if *create.revision < 0 || *create.targetRevision < 0 {
			return usage("revisions must be nonnegative")
		}
		p := daemon.TaskCreateParams{BacklogItemParams: daemon.BacklogItemParams{AdmitParams: daemon.AdmitParams{ID: *id, Title: *title, Content: *content, WorkflowID: *create.workflow, Revision: *create.revision,
			TargetConfig: *create.target, TargetRevision: *create.targetRevision, TaskClass: *create.taskClass, Mode: map[string]string{"local": "Local", "normal": "Normal"}[mode], DryRun: *create.dryRun, CommitExternal: *create.commitExternal,
			DeclaredFiles: []string(*declared), TestFilter: *testFilter, TestShape: *testShape, AuthorizingMessage: o.message}, Source: *source, WriteClass: *writeClass}, AcceptInFlight: *create.accept}
		if set["priority"] {
			// An omitted priority is sent as absent; the daemon takes 10.
			p.Priority = priority
		}
		for _, edge := range []struct {
			ids      idList
			from, to bool
			kind     string
		}{{create.dependsOn, true, false, "blocking"}, {create.blocks, false, true, "blocking"}, {create.relates, false, true, "relates"}} {
			for _, other := range edge.ids {
				e := controlplane.BacklogEdge{From: *id, To: other, Kind: edge.kind}
				if edge.from {
					e.From, e.To = other, *id
				}
				p.Edges = append(p.Edges, e)
			}
		}
		return rpc(ctx, *o, method, key, p, out)
	}
	p := daemon.TaskUpdateParams{ID: *id, ExpectedVersion: *update.version, AuthorizingMessage: o.message}
	// Only the flags given change their fields.
	given := func(name string, value *string) *string {
		if set[name] {
			return value
		}
		return nil
	}
	p.Title, p.Content, p.Source, p.WriteClass, p.TestFilter, p.TestShape = given("title", title), given("content", content), given("source-ref", source), given("write-class", writeClass), given("test-filter", testFilter), given("test-shape", testShape)
	if set["priority"] {
		p.Priority = priority
	}
	if set["declared-file"] || *update.clear {
		files := []string(*declared)
		if files == nil {
			files = []string{}
		}
		p.DeclaredFiles = &files
	}
	return rpc(ctx, *o, method, key, p, out)
}
