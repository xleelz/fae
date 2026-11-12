package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/xleelz/fae/daemon"
)

type submission struct {
	taskInputs                                                                                                                   taskInputFlags
	prompt, content, file, title, id, target, wf, runID, mode, ticketRoom, destination, reviewBranch, pushNamespace, key, source *string
	targetRev                                                                                                                    *int64
	rev, priority                                                                                                                *int
	critical, commitExternal, dryRun, start                                                                                      *bool
	taskClass, carryFrom                                                                                                         *string
	declared                                                                                                                     *declaredFiles
}

// declaredFiles is the repeatable --declared-file flag (FAE-CLI-014). It
// refuses what admission and the scope resolver refuse (one rule,
// daemon.DeclaredPathSafe: never in .git at any depth) as a usage error
// before anything is sent.
type declaredFiles []string

func (f *declaredFiles) String() string {
	if f == nil {
		return ""
	}
	return strings.Join(*f, ",")
}

func (f *declaredFiles) Set(name string) error {
	if !daemon.DeclaredPathSafe(name) {
		return fmt.Errorf("declared file %q must be a clean relative path inside the worktree, outside .git", name)
	}
	if slices.Contains(*f, name) {
		return fmt.Errorf("declared file %q given twice", name)
	}
	if len(*f) >= 1000 {
		return errors.New("at most 1000 declared files")
	}
	*f = append(*f, name)
	return nil
}

func submissionFlags(f *flag.FlagSet) *submission {
	declared := &declaredFiles{}
	f.Var(declared, "declared-file", "a file the task declares; repeat for each. A live instance without pinned files takes the task's scope from them")
	return &submission{declared: declared, taskInputs: addTaskInputFlags(f),
		prompt: f.String("prompt", "", "task content"), content: f.String("content", "", "task content (alias of --prompt)"), file: f.String("file", "", "task content file (markdown)"),
		title: f.String("title", "", "task title"), id: f.String("id", "", "task/item ID (default generated)"),
		target: f.String("target-config", "", "target configuration selector"), targetRev: f.Int64("target-revision", 0, "target revision; zero selects latest"),
		wf: f.String("workflow", "", "workflow selector (default the instance default workflow)"), rev: f.Int("revision", 0, "pinned workflow revision; 0 means latest non-retired"),
		critical: f.Bool("critical", false, "protect task from timed deployment overlap overrides"), priority: f.Int("priority", 10, "priority >=10, larger first"),
		runID: f.String("run-id", "", "run ID (default the instance drain run)"), mode: f.String("mode", "", "local or normal (default: the instance --default-mode)"),
		dryRun: f.Bool("dry-run", false, "normal mode recording intended external effects only (exactly one of --dry-run or --commit-external with --mode normal)"), commitExternal: f.Bool("commit-external", false, "normal mode with pinned live external effects"),
		ticketRoom: f.String("ticket-room", "", "pinned ticket room"), reviewBranch: f.String("review-destination-branch", "", "pinned code-review destination branch"),
		destination: f.String("destination", "", "deprecated alias of --review-destination-branch"), pushNamespace: f.String("push-namespace", "", "pinned push namespace"),
		key: f.String("idempotence-key", "", "admission key"), source: f.String("source-ref", "", "work item source reference (start-work-item)"),
		start: f.Bool("start", false, "run only: start the stopped instance from the bound workspace with its last serve arguments"), taskClass: f.String("task-class", "", "declared task class (default: the workflow's default class)"), carryFrom: f.String("carry-from", "", "re-admit an inactive earlier task of the same item, carrying its accepted pre-code evidence"),
	}
}

// params composes and checks a submission; content precedence is local only.
func (s *submission) params(verb string, opts *common) (daemon.AdmitParams, error) {
	var p daemon.AdmitParams
	if *s.prompt != "" && *s.content != "" {
		return p, usage("provide one of --prompt or --content")
	}
	if *s.dryRun && *s.commitExternal {
		return p, usage("--dry-run and --commit-external are mutually exclusive")
	}
	mode := strings.ToLower(*s.mode)
	if mode == "" && (*s.dryRun || *s.commitExternal) {
		mode = "normal"
	}
	if mode != "" && mode != "local" && mode != "normal" {
		return p, usage("--mode must be local or normal")
	}
	if mode == "local" && (*s.dryRun || *s.commitExternal) {
		return p, usage("--dry-run and --commit-external require --mode normal")
	}
	if !daemon.SubmissionModeComplete(mode == "normal", *s.dryRun, *s.commitExternal) {
		return p, usage("--mode normal requires exactly one of --dry-run or --commit-external")
	}
	branch := *s.reviewBranch
	if *s.destination != "" {
		if branch != "" && branch != *s.destination {
			return p, usage("--destination differs from --review-destination-branch")
		}
		branch = *s.destination
	}
	if verb != "start-work-item" && *s.source != "" {
		return p, usage("--source-ref applies to start-work-item")
	}
	if verb != "run" && *s.start {
		return p, usage("--start applies to run")
	}
	content, err := inputText(*s.prompt+*s.content, *s.file)
	if err != nil {
		return p, err
	}
	if err = requireMessage(verb, opts); err != nil {
		return p, err
	}
	if *s.priority < 10 || *s.rev < 0 || *s.targetRev < 0 {
		return p, fmt.Errorf("priority must be >=10 and revisions nonnegative")
	}
	if *s.id == "" && *s.key != "" && *s.taskInputs.Parent == "" {
		sum := sha256.Sum256([]byte(*s.key))
		*s.id = "task-" + hex.EncodeToString(sum[:16])
	}
	if *s.id == "" && *s.taskInputs.Parent == "" {
		if *s.id, err = identity("task-"); err != nil {
			return p, err
		}
	}
	if *s.key == "" {
		*s.key = *s.id
		if *s.key == "" {
			*s.key, err = identity("detached-admission-")
			if err != nil {
				return p, err
			}
		}
	}
	p = daemon.AdmitParams{ID: *s.id, WorkflowID: *s.wf, Revision: int64(*s.rev), TargetConfig: *s.target, TargetRevision: *s.targetRev, Title: *s.title, Content: content, Critical: *s.critical, Priority: *s.priority, RunID: *s.runID, Mode: map[string]string{"local": "Local", "normal": "Normal"}[mode], CommitExternal: *s.commitExternal, DryRun: *s.dryRun, TicketRoom: *s.ticketRoom, Destination: branch, PushNamespace: *s.pushNamespace, AuthorizingMessage: opts.message, TaskClass: *s.taskClass, CarryFrom: *s.carryFrom, DeclaredFiles: []string(*s.declared)}
	if err = s.taskInputs.apply(&p); err != nil {
		return p, err
	}
	return p, nil
}

// submitCommand implements run (FAE-CLI-011) and start-work-item, which queues
// the item through backlog admission so it appears in the backlog (FAE-CLI-031).
func submitCommand(ctx context.Context, verb string, f *flag.FlagSet, opts *common, args []string, out io.Writer) error {
	s := submissionFlags(f)
	if err := parse(f, args); err != nil {
		return err
	}
	p, err := s.params(verb, opts)
	if err != nil {
		return err
	}
	if verb == "run" {
		err = rpc(ctx, *opts, "Admit", *s.key, p, out)
		if *s.start && exitCode(err) == 3 {
			if err = startRecorded(ctx, opts); err != nil {
				return err
			}
			err = rpc(ctx, *opts, "Admit", *s.key, p, out)
		}
		return err
	}
	var result bytes.Buffer
	if err = rpc(ctx, *opts, "BacklogAdmit", "", daemon.BacklogParams{Items: []daemon.BacklogItemParams{{AdmitParams: p, Source: *s.source}}, Edges: nil, AuthorizingMessage: opts.message}, &result); err != nil {
		return err
	}
	var counts map[string]any
	if err = json.Unmarshal(result.Bytes(), &counts); err != nil {
		return err
	}
	counts["id"] = p.ID
	return jsonOut(out, counts)
}

// resumeCommand resumes one task or every resumable task of a run (FAE-CLI-013).
func resumeCommand(ctx context.Context, f *flag.FlagSet, opts *common, args []string, out io.Writer) error {
	p := daemon.ResumeRunParams{}
	f.StringVar(&p.RunID, "run-id", "", "run ID (default the latest run)")
	f.StringVar(&p.Task, "task", "", "resume only this task")
	if err := parse(f, args); err != nil {
		return err
	}
	if err := requireMessage("resume", opts); err != nil {
		return err
	}
	p.AuthorizingMessage = opts.message
	return rpc(ctx, *opts, "ResumeRun", "", p, out)
}

// showCommand prints a read-only snapshot of a task or a run (FAE-CLI-030):
// JSON with --json, otherwise text.
func showCommand(ctx context.Context, f *flag.FlagSet, opts *common, args []string, out io.Writer) error {
	p := daemon.ShowParams{}
	f.StringVar(&p.Task, "task", "", "task ID")
	f.StringVar(&p.RunID, "run-id", "", "run ID")
	f.BoolVar(&p.IncludeRetired, "include-retired", false, "include retired tasks")
	if err := parse(f, args); err != nil {
		return err
	}
	if (p.Task == "") == (p.RunID == "") {
		return usage("show requires exactly one of --task or --run-id")
	}
	if opts.json {
		return rpc(ctx, *opts, "Show", "", p, out)
	}
	var raw bytes.Buffer
	if err := rpc(ctx, *opts, "Show", "", p, &raw); err != nil {
		return err
	}
	var shown struct {
		RunID string            `json:"run_id"`
		Tasks []daemon.ShowTask `json:"tasks"`
	}
	if err := json.Unmarshal(raw.Bytes(), &shown); err != nil {
		return err
	}
	var b strings.Builder
	if shown.RunID != "" {
		fmt.Fprintf(&b, "run %s: %d task(s)\n", shown.RunID, len(shown.Tasks))
	}
	for _, t := range shown.Tasks {
		fmt.Fprintf(&b, "task %s [%s] stage=%s workflow=%s@%d run=%s mode=%s live=%v priority=%d version=%d", t.ID, t.State, t.Stage, t.WorkflowID, t.WorkflowRevision, t.RunID, t.Disposition.Mode, t.Disposition.Live, t.Priority, t.Version)
		if t.InFlight {
			b.WriteString(" in-flight")
		}
		if t.Parked {
			b.WriteString(" parked")
		}
		b.WriteString("\n")
		if t.Escalation != "" {
			fmt.Fprintf(&b, "  escalation: %s\n", t.Escalation)
		} else if t.Reason != "" {
			fmt.Fprintf(&b, "  reason: %s\n", t.Reason)
		}
		for _, a := range t.Artifacts {
			fmt.Fprintf(&b, "  %s: %s role=%s produced=%s\n", a.Stage, a.ArtifactID, a.Role, a.ProducedAt.Format("2006-01-02T15:04:05Z07:00"))
		}
	}
	_, err := io.WriteString(out, b.String())
	return err
}
