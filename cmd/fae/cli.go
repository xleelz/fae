package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/daemon"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

type commandError struct {
	code int
	err  error
}

func (e *commandError) Error() string { return e.err.Error() }
func (e *commandError) Unwrap() error { return e.err }
func exitCode(err error) int {
	var ce *commandError
	if errors.As(err, &ce) {
		return ce.code
	}
	return 1
}
func usage(format string, args ...any) error { return &commandError{2, fmt.Errorf(format, args...)} }
func unreachable(err error) error {
	return &commandError{3, fmt.Errorf("daemon unreachable; start it with fae serve --workspace PATH (in the bound workspace): %w", err)}
}

// requireMessage refuses (exit 1) a mutating verb without a citation (FAE-OPS-002).
func requireMessage(verb string, c *common) error {
	if c.message == "" {
		return fmt.Errorf("%s refused: --authorizing-message (a recorded operator message ID) is required", verb)
	}
	return nil
}

// helpRequest carries -h/--help from any flag set back to execute.
type helpRequest struct{ f *flag.FlagSet }

func (h *helpRequest) Error() string { return "help requested" }

type common struct {
	home, instance, message string
	json                    bool
}

func flags(name string, c common) (*flag.FlagSet, *common) {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(&bytes.Buffer{})
	f.StringVar(&c.home, "home", c.home, "instance home (default ~/.fae/instances)")
	f.StringVar(&c.instance, "instance", c.instance, "instance name")
	f.StringVar(&c.message, "authorizing-message", c.message, "previously recorded operator message ID")
	f.BoolVar(&c.json, "json", c.json, "JSON output")
	return f, &c
}

// parse parses a verb's flags. An empty --home or --instance keeps its
// default (commonDefaults).
func parse(f *flag.FlagSet, args []string) error {
	fallBack := configurableFlags(f, commonDefaults)
	if err := f.Parse(args); errors.Is(err, flag.ErrHelp) {
		return &helpRequest{f}
	} else if err != nil {
		return usage("%s: %v (fae %s --help lists its flags)", f.Name(), err, f.Name())
	}
	if f.NArg() != 0 {
		return usage("unexpected arguments: %s", strings.Join(f.Args(), " "))
	}
	fallBack()
	return nil
}

// offlineWorkflowOperands parses the offline workflow forms (validate FILE,
// simulate FILE SCRIPT TASK) with the common flag set every verb uses, so
// -json, --json=true, -instance x, -instance=x and --authorizing-message M
// are accepted as on any other verb, before, between or after the operands.
// The forms read no instance; the flags are parsed, not used. It returns the
// operands in order.
func offlineWorkflowOperands(args []string, c common) ([]string, error) {
	f, _ := flags("workflow", c)
	catalog := catalogFiles{}
	f.Var(&catalog, "catalog", "offline child workflow catalog; repeatable")
	tree := f.Bool("tree", false, "render rehearsed run tree")
	fallBack := configurableFlags(f, commonDefaults)
	var operands []string
	for {
		if err := f.Parse(args); errors.Is(err, flag.ErrHelp) {
			return nil, &helpRequest{f}
		} else if err != nil {
			return nil, usage("workflow: %v (fae workflow --help lists its forms)", err)
		}
		if f.NArg() == 0 {
			break
		}
		operands, args = append(operands, f.Arg(0)), f.Args()[1:]
	}
	fallBack()
	for _, path := range catalog {
		operands = append(operands, "--catalog", path)
	}
	if *tree {
		operands = append(operands, "--tree")
	}

	return operands, nil
}
func identity(prefix string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b), nil
}
func jsonOut(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// executed, when set, observes each execute call with its exit code. Tests
// set it so a conformance proof can show what a cited test ran.
var executed func(args []string, code int)

func execute(ctx context.Context, args []string, out io.Writer) (err error) {
	if executed != nil {
		defer func() {
			code := 0
			if err != nil {
				code = exitCode(err)
			}
			executed(args, code)
		}()
	}
	err = dispatch(ctx, args, out)
	var help *helpRequest
	if errors.As(err, &help) {
		return commandUsage(help.f, wantsJSON(args), out)
	}
	return err
}

// needsNoHome reports the commands that read no instance state, so they run
// without $HOME (FAE-CLI-001): help, version, the offline workflow forms,
// doctor (which reports a missing home as a check) and the offline gate
// self-test.
func needsNoHome(verb string, args []string) bool {
	switch verb {
	case "help", "version", "workflow", "doctor", "symphony", "herdr":
		return true
	case "gates":
		return slices.Contains(args, "--offline") || slices.Contains(args, "-offline") || slices.Contains(args, "--offline=true")
	case "template":
		return len(args) > 0 && args[0] == "workflow-validate"
	}
	return false
}
func preVerbJSON(arg string) (val, ok bool) {
	switch arg {
	case "--json", "-json", "--json=true", "-json=true":
		return true, true
	case "--json=false", "-json=false":
		return false, true
	default:
		return false, false
	}
}

func isPreVerbJSON(arg string) bool {
	_, ok := preVerbJSON(arg)
	return ok
}

func wantsJSON(args []string) bool {
	want := false
	for _, a := range args {
		if val, ok := preVerbJSON(a); ok {
			want = val
		}
	}
	return want
}
func dispatch(ctx context.Context, args []string, out io.Writer) error {
	// A missing $HOME is reported once the verb is known, so the pre-tool hook
	// can fail closed with 2 instead of exiting 1 (see heavyOpHook), and only
	// when the command needs the default home: an explicit --home and the
	// commands that read no instance state (needsNoHome) do not (FAE-CLI-001).
	userhome, homeErr := os.UserHomeDir()
	builtin := common{instance: "default"}
	if homeErr == nil {
		builtin.home = filepath.Join(userhome, ".fae", "instances")
	}
	c := builtin
	// Common flags are accepted before the verb as well as after it, as
	// --flag VALUE or --flag=VALUE; help requested before any verb is the
	// global help (FAE-CLI-001: exit 0). An empty --home or --instance keeps
	// its default, with a warning once the verb is known.
	emptied := map[string]bool{}
	for len(args) > 0 && (strings.HasPrefix(args[0], "--") || isHelp(args[0]) || isPreVerbJSON(args[0])) {
		if isHelp(args[0]) {
			return globalHelp(c.json || wantsJSON(args), out)
		}
		if val, ok := preVerbJSON(args[0]); ok {
			c.json = val
			args = args[1:]
			continue
		}
		name, value, inline := strings.Cut(args[0], "=")
		switch name {
		case "--home", "--instance", "--authorizing-message":
		default:
			return usage("unknown global flag %s; run fae help", args[0])
		}
		if inline {
			args = args[1:]
		} else if len(args) < 2 {
			return usage("missing value for %s; run fae help", args[0])
		} else {
			value, args = args[1], args[2:]
		}
		empty := strings.TrimSpace(value) == ""
		switch name {
		case "--home":
			c.home, emptied["home"] = value, empty
			if empty {
				c.home = builtin.home
			}
		case "--instance":
			c.instance, emptied["instance"] = value, empty
			if empty {
				c.instance = builtin.instance
			}
		case "--authorizing-message":
			c.message = value
		}
	}
	if len(args) == 0 {
		return usage("usage: fae COMMAND [flags]\n%s", globalHelpText())
	}
	verb, args := args[0], args[1:]
	if emptied["home"] && builtin.home != "" {
		warnFallback(c.instance, verb, "home", "", "", builtin.home)
	}
	if emptied["instance"] {
		warnFallback(c.instance, verb, "instance", "", "", builtin.instance)
	}
	if verb == "heavy-op" && len(args) > 0 && args[0] == "hook" {
		return heavyOpHook(args[1:], c, homeErr, out)
	}
	if homeErr != nil && c.home == "" && !needsNoHome(verb, args) && !slices.ContainsFunc(args, func(a string) bool {
		return isHelp(a) || a == "--home" || a == "-home" || strings.HasPrefix(a, "--home=") || strings.HasPrefix(a, "-home=")
	}) {
		return homeErr
	}
	if len(args) > 0 && (args[0] == "--help" || args[0] == "-h") && groupHelp(verb, c.json || wantsJSON(args), out) {
		return nil
	}
	if verb == "workspace" {
		return reviewWorkspaceCommand(ctx, args, c, out)
	}
	if verb == "herdr" {
		return herdrCommand(ctx, args, c, out)
	}
	if verb == "symphony" {
		if err := loadLinearEnv(); err != nil {
			return err
		}
		var errBuf bytes.Buffer
		if code := RunSymphonyCLI(ctx, args, out, &errBuf); code != 0 {
			msg := strings.TrimSpace(errBuf.String())
			if msg == "" {
				msg = "symphony exited with error"
			}
			return &commandError{code: code, err: errors.New(msg)}
		}
		return nil
	}
	if verb == "change-request" {
		return changeRequestCommand(ctx, args, c, out)
	}
	if verb == "workflow-limit" {
		return workflowLimitCommand(ctx, args, c, out)
	}
	if verb == "tree-limit" {
		return treeLimitCommand(ctx, args, c, out)
	}
	if verb == "tree" {
		return treeCommand(ctx, args, c, out)
	}
	if verb == "census" {
		return censusCommand(ctx, args, c, out)
	}
	if verb == "ui" {
		return uiCommand(ctx, args, c, out)
	}
	if verb == "alerts" || verb == "metrics" || verb == "host-metrics" {
		return observabilityCommand(ctx, verb, args, c, out)
	}
	if verb == "cleanup" || verb == "backup" {
		return cleanupCommand(ctx, verb, args, c, out)
	}
	if verb == "backlog" {
		return backlogCommand(ctx, args, c, out)
	}
	if verb == "pool" {
		return poolCommand(ctx, args, c, out)
	}
	if verb == "quota" {
		return quotaCommand(ctx, args, c, out)
	}
	if verb == "pause" {
		return pauseCommand(ctx, args, c, out)
	}
	if verb == "unpause" {
		return unpauseCommand(ctx, args, c, out)
	}
	if verb == "import-legacy" {
		return importLegacyCommand(ctx, args, c, out)
	}
	if verb == "task" {
		if len(args) >= 2 && args[0] == "show" && !strings.HasPrefix(args[1], "-") {
			rewritten := []string{"show", "--id", args[1]}
			rewritten = append(rewritten, args[2:]...)
			args = rewritten
		}
		return taskCommand(ctx, args, c, out)
	}
	if verb == "reconcile-done" {
		return reconcileDoneCommand(ctx, args, c, out)
	}
	if command, ok := operatorCommands[verb]; ok {
		return command(ctx, verb, args, c, out)
	}
	if verb == "prophet" && len(args) > 0 && (args[0] == "restart" || args[0] == "resume") {
		sub := args[0]
		if sub == "resume" {
			sub = "resume-with-context"
		}
		return taskControlCommand(ctx, sub, args[1:], c, out)
	}
	if verb == "prophet" {
		return prophetCommand(ctx, args, c, out)
	}
	if _, ok := taskControlMethods[verb]; ok {
		return taskControlCommand(ctx, verb, args, c, out)
	}
	if verb == "notify" {
		return notifyCommand(ctx, args, c, out)
	}
	if verb == "follow-up" {
		return followUpCommand(ctx, args, c, out)
	}
	if verb == "review-health" {
		return reviewHealthCommand(ctx, args, c, out)
	}
	if verb == "drive" || verb == "maintenance" || verb == "roll" || verb == "watchdog" || verb == "runtime-launcher" {
		return controllerCommand(ctx, verb, args, c, out)
	}
	if verb == "schedule" {
		return scheduleCommand(ctx, args, c, out)
	}
	if verb == "gates" || verb == "gate" {
		return gatesCommand(ctx, verb, args, c, out)
	}
	if verb == "land" {
		return landCommand(ctx, args, c, out)
	}
	if verb == "version" {
		f, opts := flags(verb, c)
		if err := parse(f, args); err != nil {
			return err
		}
		v := daemon.BuildVersion()
		if opts.json {
			v["status"] = "partial requirements implementation"
			return jsonOut(out, v)
		}
		_, err := fmt.Fprintf(out, "fae %v (commit %v; partial requirements implementation)\n", v["version"], v["commit"])
		return err
	}
	if verb == "workflow" {
		if len(args) > 0 && (args[0] == "default" || args[0] == "scenario") {
			f, _ := flags("workflow "+args[0], c)
			local := f.Bool("local", false, "print the Local projection (no Normal-only stages)")
			if err := parse(f, args[1:]); err != nil {
				return err
			}
			if args[0] == "default" && *local {
				return jsonOut(out, defaults.Workflow())
			}
			if args[0] == "default" {
				return jsonOut(out, defaults.Shipped())
			}
			return jsonOut(out, defaults.HappyScript())
		}
		if slices.ContainsFunc(args, isHelp) {
			return subcommandHelp(verb, "default [--local] | scenario | validate FILE [--catalog JSON]... | simulate FILE SCRIPT TASK [--catalog JSON]... [--tree] (offline; positional operands; SCRIPT is the simulator's records or a scripted-adapter scenario such as scenario prints)", c.json || wantsJSON(args), out)
		}
		operands, err := offlineWorkflowOperands(args, c)
		if err != nil {
			return err
		}
		return offline(append([]string{verb}, operands...), out)
	}
	if verb == "help" {
		f, opts := flags(verb, c)
		if err := parse(f, args); err != nil {
			return err
		}
		return globalHelp(opts.json, out)
	}
	if verb == "template" {
		if len(args) == 0 || !templateVerbs[args[0]] {
			return usage("template requires one of: %s", strings.Join(sortedKeys(templateVerbs), ", "))
		}
		verb, args = args[0], args[1:]
	}
	f, opts := flags(verb, c)
	switch verb {
	case "serve":
		scratch := f.Bool("scratch", false, "mark a newly created disposable instance")
		workspace := f.String("workspace", "", "canonical workspace; must equal current directory")
		binary := f.String("dolt", "dolt", "Dolt executable")
		adapter := f.String("adapter", "multiplexer", "adapter: multiplexer, process (direct) or scripted")
		liveFile := f.String("live-config", "", "pinned live target and trusted bridge JSON")
		scenario := f.String("scenario", "", "scripted stage outputs JSON; default built-in rehearsal")
		runID := f.String("run-id", "backlog-drain", "run ID")
		prophetRoutesFile := f.String("prophet-routes", "", "ordered Prophet binding JSON file (maximum 8)")
		prophetFailovers := f.Int("prophet-max-failovers", 0, "Prophet failovers: 0 defaults to one, -1 disables, maximum 7")
		prophetProvider := f.String("prophet-provider", "claude", "Prophet provider binding")
		prophetModel := f.String("prophet-model", "claude-opus-5-5", "Prophet provider model")
		principalConfig := f.String("principal-config", "", "owner-only delegated credential configuration")
		eventAge := f.Duration("event-retention-age", 24*time.Hour, "minimum retained event age window")
		eventCount := f.Int("event-retention-count", 100000, "minimum retained event count window")
		messageMaxAge := f.Duration("message-max-age", 24*time.Hour, "maximum age of operator message authorization (positive duration)")
		doltStopWait := f.Duration("dolt-stop-wait", 30*time.Second, "bounded graceful Dolt stop before a forced kill (positive duration)")
		recursionCeiling := f.Int("recursion-ceiling", 8, "maximum run-tree depth (1..64)")
		max := f.Int("max-tasks-in-flight", -1, "maximum concurrent tasks; -1 is unbounded")
		maxMolecules := f.Int("max-molecules", -1, "maximum running molecules; -1 is unbounded")
		maxWorkflowMolecules := f.Int("max-workflow-molecules", -1, "maximum running molecules per workflow; -1 is unbounded")
		port := f.Int("dolt-port", 0, "private Dolt port (0 derives from data directory)")
		foreground := f.Bool("foreground", false, "stay attached; without it serve detaches once ready and logs to HOME/INSTANCE.serve.log")
		defaultMode := f.String("default-mode", "local", "disposition for submissions that state none: local or normal (dry-run)")
		maxPanes := f.Int("max-panes", 0, "pane limit (multiplexer; 0 keeps the live configuration or 60)")
		maxHeavy := f.Int("max-heavy-weight", 0, "heavy-operation capacity in weight units (live adapters; 0 keeps the live configuration or 2)")
		rearmBudget := f.Int("rearm-budget", 3, "durable drive re-arms per cause per --rearm-window")
		rearmWindow := f.Duration("rearm-window", time.Hour, "drive re-arm budget window")
		inlineLimit := f.Int("inline-limit", controlplane.DefaultInlineLimit, "largest plane row column in bytes (4096..65536); a longer payload is stored as a content-addressed blob")
		quarantineUnrunnable := f.Bool("quarantine-unrunnable-revisions", false, "this start only: quarantine the active tasks pinned to a stored workflow revision this binary cannot run, instead of refusing to start; then migrate or retire them (a no-op with no such task)")
		doltRestartAttempts := addDoltRestartAttemptsFlag(f)
		uiOpts := addUIFlags(f)
		tuiOpts := addTUIFlags(f)
		symphonyOpts := addSymphonyFlags(f)
		fallBack := configurableFlags(f, serveDefaults)
		if err := parse(f, args); err != nil {
			return err
		}
		fallBack()
		if err := validateDoltRestartAttempts(*doltRestartAttempts); err != nil {
			return err
		}
		uiConfig, err := uiOpts.config(f)
		if err != nil {
			return err
		}
		tuiConfig, err := tuiOpts.config(f)
		if err != nil {
			return err
		}
		if err := loadLinearEnv(); err != nil {
			return err
		}
		symphonyConfig, err := symphonyOpts.config(f)
		if err != nil {
			return err
		}
		if *workspace == "" {
			return usage("serve requires --workspace")
		}
		if *adapter == "process" {
			*adapter = "direct"
		}
		if *adapter != "scripted" && *adapter != "direct" && *adapter != "multiplexer" {
			return usage("adapter %q is not implemented; select multiplexer, process (direct) or scripted", *adapter)
		}
		mode := map[string]string{"local": "Local", "normal": "Normal"}[strings.ToLower(*defaultMode)]
		if mode == "" || mode == "Normal" && *adapter == "scripted" {
			return usage("--default-mode must be local, or normal with a live adapter")
		}
		if *maxPanes > 0 && *adapter != "multiplexer" || *maxHeavy > 0 && *adapter == "scripted" {
			return usage("--max-panes requires the multiplexer adapter and --max-heavy-weight a live adapter")
		}
		var live *daemon.LiveConfig
		if *adapter == "direct" || *adapter == "multiplexer" {
			if *liveFile == "" || *scenario != "" {
				return usage("live adapter requires --live-config and forbids --scenario")
			}
			raw, e := readSecretFile(*liveFile)
			if e != nil {
				return e
			}
			raw, e = domain.CanonicalJSON(raw)
			if e != nil {
				return e
			}
			live = &daemon.LiveConfig{}
			dec := json.NewDecoder(strings.NewReader(string(raw)))
			dec.DisallowUnknownFields()
			if e = dec.Decode(live); e != nil {
				return e
			}
			if live.Adapter != "" && live.Adapter != *adapter {
				return errors.New("live configuration adapter differs from --adapter")
			}
			live.Adapter = *adapter
			applyServeLimits(live, *maxPanes, *maxHeavy)
		} else if *liveFile != "" {
			return usage("--live-config requires --adapter direct or multiplexer")
		}
		var executor engine.Executor
		if *scenario != "" {
			raw, e := readBounded(*scenario)
			if e != nil {
				return e
			}
			raw, e = domain.CanonicalJSON(raw)
			if e != nil {
				return e
			}
			var steps map[string][]engine.ScriptStep
			dec := json.NewDecoder(strings.NewReader(string(raw)))
			dec.DisallowUnknownFields()
			if e = dec.Decode(&steps); e != nil {
				var plan workflow.Scenario
				decoder := json.NewDecoder(strings.NewReader(string(raw)))
				decoder.DisallowUnknownFields()
				decoder.UseNumber()
				if scenarioErr := decoder.Decode(&plan); scenarioErr != nil {
					return e
				}
				if plan.Rules == nil {
					return e
				}
				executor = engine.NewScenarioScripted(plan)
			} else {
				executor = engine.NewScripted(steps)
			}
		}
		var prophetRoutes []workflow.Binding
		if *prophetRoutesFile != "" {
			raw, e := os.ReadFile(*prophetRoutesFile)
			if e != nil {
				return e
			}
			if e = json.Unmarshal(raw, &prophetRoutes); e != nil {
				return e
			}
			if len(prophetRoutes) == 0 {
				return errors.New("Prophet routes file must not be empty")
			}
		}
		if !*foreground {
			return detachServe(ctx, opts, args, out)
		}
		if tuiConfig != nil && tuiConfig.TTY == "" && os.Getenv("HERDR_PANE_ID") == "" {
			tuiConfig.TTY = interactiveTTYPath(os.Stdin, out)
		}
		var workflowMD, symphonyHTTPAddr string
		if symphonyConfig != nil {
			workflowMD = symphonyConfig.WorkflowPath
			symphonyHTTPAddr = symphonyConfig.HTTPAddr
		}
		d, err := startDaemon(ctx, daemon.Config{RecursionCeiling: *recursionCeiling, DefaultMode: mode, Scratch: *scratch, ProphetRoutes: prophetRoutes, ProphetMaxFailovers: *prophetFailovers, ProphetProvider: *prophetProvider, ProphetModel: *prophetModel, EventRetentionAge: *eventAge, EventRetentionCount: *eventCount, PrincipalConfig: *principalConfig, MessageMaxAge: *messageMaxAge, Executor: executor, Live: live, UI: uiConfig, TUI: tuiConfig, WorkflowMD: workflowMD, SymphonyHTTPAddr: symphonyHTTPAddr, Symphony: symphonyConfig, Root: opts.home, Instance: opts.instance, Workspace: *workspace, DoltBinary: *binary, RunID: *runID, Port: *port, MaxInFlight: *max, MaxMolecules: *maxMolecules, MaxWorkflowMolecules: *maxWorkflowMolecules, DoltStopWait: *doltStopWait, DoltRestartAttempts: *doltRestartAttempts, RearmBudget: *rearmBudget, RearmWindow: *rearmWindow, InlineLimit: *inlineLimit, QuarantineUnrunnableRevisions: *quarantineUnrunnable})
		if err != nil {
			return err
		}
		if e := recordServe(opts, args); e != nil {
			warnLine(os.Stderr, opts.instance, "fae serve: run --start cannot reuse this invocation: "+e.Error())
		}
		plane, socket := d.Endpoints()
		var uiURL any
		if u := d.UIURL(); u != "" {
			uiURL = u
		}
		if err = jsonOut(out, map[string]any{"instance": opts.instance, "adapter": *adapter, "plane": plane, "socket": socket, "ui": uiURL, "pid": os.Getpid(), "ready": true}); err != nil {
			_ = d.Close(context.Background())
			return err
		}
		return d.Run(ctx)
	case "provider-health":
		window := f.Duration("window", 15*time.Minute, "health time-series window (1s..24h)")
		fallBack := configurableFlags(f, providerHealthDefaults)
		if err := parse(f, args); err != nil {
			return err
		}
		fallBack()
		return rpc(ctx, *opts, "ProviderHealth", "", map[string]any{"window_seconds": int64(window.Seconds())}, out)
	case "leases", "panes":
		if err := parse(f, args); err != nil {
			return err
		}
		return rpc(ctx, *opts, map[string]string{"leases": "Leases", "panes": "Panes"}[verb], "", map[string]any{}, out)
	case "provider-log":
		after := f.Int64("after", 0, "resume after audit sequence")
		limit := f.Int("limit", 100, "page size 1..500")
		if err := parse(f, args); err != nil {
			return err
		}
		if *after < 0 || *limit < 1 || *limit > 500 {
			return usage("provider-log cursor must be nonnegative and limit 1..500")
		}
		return rpc(ctx, *opts, "ProviderObservations", "", map[string]any{"after": *after, "limit": *limit}, out)
	case "status", "board":
		includeRetired := f.Bool("include-retired", false, "include retired tasks")
		var after *string
		var limit *int
		tasks := new(bool)
		filter := daemon.BoardParams{}
		var states string
		if verb == "board" {
			after = f.String("after", "", "resume after task ID")
			limit = f.Int("limit", 100, "page size 1..500")
			f.StringVar(&states, "state", "", "comma-separated states or stages")
			f.StringVar(&filter.Target, "target", "", "only this target configuration")
			f.StringVar(&filter.Workflow, "workflow", "", "only this workflow")
			f.StringVar(&filter.Mode, "mode", "", "only this disposition mode (local or normal)")
			f.StringVar(&filter.Molecule, "molecule", "", "only this molecule")
			f.StringVar(&filter.RunID, "run-id", "", "only this run")
			f.IntVar(&filter.MinPriority, "min-priority", 0, "only tasks at or above this priority")
			f.BoolVar(&filter.InFlight, "in-flight", false, "only tasks with a turn in flight")
		}
		if verb == "status" {
			tasks = f.Bool("tasks", false, "include per-task rows")
		}
		if err := parse(f, args); err != nil {
			return err
		}
		if verb == "board" {
			if *limit < 1 || *limit > 500 || filter.MinPriority < 0 {
				return usage("board limit must be 1..500 and --min-priority nonnegative")
			}
			filter.After, filter.Limit, filter.IncludeRetired, filter.States = *after, *limit, *includeRetired, splitList(states)
			return rpc(ctx, *opts, "Board", "", filter, out)
		}
		return rpc(ctx, *opts, "Status", "", map[string]any{"include_retired": *includeRetired, "tasks": *tasks}, out)
	case "history":
		includeRetired := f.Bool("include-retired", false, "include retired tasks")
		task := f.String("task", "", "task ID")
		summary := f.Bool("summary", false, "bounded history event page and artifact references")
		records := f.Bool("records", false, "page the plane's transitions, artifacts and decisions")
		after := f.Int64("after", 0, "resume after event sequence (transition id with --records)")
		limit := f.Int("limit", 100, "history page size 1..500")
		if err := parse(f, args); err != nil {
			return err
		}
		if *task == "" {
			return usage("%s requires --task", verb)
		}
		if *after < 0 || *limit < 1 || *limit > 500 {
			return usage("history cursor must be nonnegative and limit 1..500")
		}
		params := map[string]any{"id": *task, "include_retired": *includeRetired}
		if *records && *summary {
			return usage("history takes --summary or --records, not both")
		}
		if *records {
			params["records"], params["after"], params["limit"] = true, *after, *limit
		} else if *summary {
			params["summary"], params["after"], params["limit"] = true, *after, *limit
		} else if *after != 0 || *limit != 100 {
			return usage("history pagination requires --summary")
		}
		return rpc(ctx, *opts, "History", "", params, out)
	case "topology":
		workflow := f.String("workflow", "", "only this workflow's molecules")
		after := f.String("after", "", "resume after molecule id")
		limit := f.Int("limit", 100, "molecules per page 1..500")
		includeRetired := f.Bool("include-retired", false, "list a running molecule's retired tasks too")
		if err := parse(f, args); err != nil {
			return err
		}
		if *limit < 1 || *limit > 500 {
			return usage("topology limit must be 1..500")
		}
		return rpc(ctx, *opts, "Topology", "", map[string]any{"workflow_id": *workflow, "after": *after, "limit": *limit, "include_retired": *includeRetired}, out)
	case "provenance":
		commit := f.String("commit", "", "commit or landing sha")
		limit := f.Int("limit", 1000, "maximum hops 1..10000")
		if err := parse(f, args); err != nil {
			return err
		}
		if *commit == "" || *limit < 1 || *limit > 10000 {
			return usage("provenance requires --commit and a limit 1..10000")
		}
		return rpc(ctx, *opts, "ProvenanceChain", "", map[string]any{"commit": *commit, "limit": *limit}, out)
	case "events":
		taskFilter := f.String("task", "", "filter one task")
		treeFilter := f.String("tree-root", "", "filter one run tree")
		runFilter := f.String("run-id", "", "filter run id")
		workflowFilter := f.String("workflow", "", "filter workflow id")
		kindFilter := f.String("kind", "", "filter event kind")
		includeRetired := f.Bool("include-retired", false, "include retired tasks")
		after := f.Int64("after", 0, "resume after sequence")
		follow := f.Bool("follow", false, "stream committed events until interrupted")
		limit := f.Int("limit", 100, "maximum events")
		if err := parse(f, args); err != nil {
			return err
		}
		if *after < 0 || *limit < 1 || *limit > 1000 {
			return usage("events cursor must be nonnegative and limit 1..1000")
		}
		if *follow {
			return streamEventsFiltered(ctx, *opts, *after, out, *includeRetired, daemon.EventFilter{Task: *taskFilter, TreeRoot: *treeFilter, RunID: *runFilter, WorkflowID: *workflowFilter, Kind: *kindFilter})
		}
		return rpc(ctx, *opts, "Events", "", map[string]any{"after": *after, "limit": *limit, "include_retired": *includeRetired, "task": *taskFilter, "tree_root": *treeFilter, "run_id": *runFilter, "workflow_id": *workflowFilter, "kind": *kindFilter}, out)
	case "resolve-uncertain":
		id := f.String("id", "", "task ID")
		task := f.String("task", "", "task ID (alias of --id)")
		dispatch := f.String("dispatch", "", "original dispatch ID (looked up from task when omitted)")
		action := f.String("action", "", "redispatch, accept or escalate")
		result := f.String("result", "", "recorded completion JSON file for accept")
		if err := parse(f, args); err != nil {
			return err
		}
		if *id != "" && *task != "" && *id != *task {
			return usage("resolve-uncertain takes --id or --task, not two different IDs")
		}
		if *id == "" {
			*id = *task
		}
		if *id == "" || (*action != "redispatch" && *action != "accept" && *action != "escalate") {
			return usage("resolve-uncertain requires --id (or --task) and --action redispatch|accept|escalate")
		}
		if err := requireMessage(verb, opts); err != nil {
			return err
		}
		p := daemon.ResolveUncertainParams{ID: *id, DispatchID: *dispatch, Action: *action, AuthorizingMessage: opts.message}
		if *result != "" {
			b, err := os.ReadFile(*result)
			if err != nil {
				return err
			}
			if err = json.Unmarshal(b, &p.Result); err != nil {
				return err
			}
		}
		if *action != "accept" && p.Result != nil {
			return usage("only accept permits a recorded provider result")
		}
		if p.DispatchID == "" {
			d, err := lookupUncertainDispatch(ctx, *opts, *id)
			if err != nil {
				return err
			}
			p.DispatchID = d
		}
		return rpc(ctx, *opts, "ResolveUncertain", "", p, out)
	case "runtime-config":
		nested := f.String("nested-commands", "", "nested host commands: refuse or account; applies to future dispatches")
		limit := f.Int("pane-limit", 0, "dynamic pane limit")
		heavy := f.Int("heavy-capacity", 0, "dynamic heavy-operation capacity")
		tasks := f.Int("tasks-in-flight", 0, "dynamic task concurrency; -1 is unbounded")
		host := f.String("host-thresholds", "", "JSON hostguard.Config with all host thresholds")
		if err := parse(f, args); err != nil {
			return err
		}
		if (*limit == 0 && *heavy == 0 && *tasks == 0 && *host == "" && *nested == "") || (*limit != 0 && (*limit < 2 || *limit > 1000000)) || *heavy < 0 || *heavy > 1000000 || *tasks < -1 || *tasks > 1000000 || (*nested != "" && *nested != "refuse" && *nested != "account") {
			return usage("runtime-config requires valid limits")
		}
		if err := requireMessage(verb, opts); err != nil {
			return err
		}
		c := daemon.RuntimeConfig{NestedCommands: *nested, PaneLimit: *limit, HeavyCapacity: *heavy, TasksInFlight: *tasks, AuthorizingMessage: opts.message}
		if *host != "" {
			if err := json.Unmarshal([]byte(*host), &c.HostThresholds); err != nil || c.HostThresholds == nil {
				return usage("invalid host-thresholds JSON")
			}
		}
		return rpc(ctx, *opts, "RuntimeConfig", "", c, out)
	case "shutdown", "drain":
		if err := parse(f, args); err != nil {
			return err
		}
		if err := requireMessage(verb, opts); err != nil {
			return err
		}
		method := "Shutdown"
		if verb == "drain" {
			method = "Drain"
		}
		return rpc(ctx, *opts, method, "", map[string]string{"authorizing_message": opts.message}, out)
	case "adapter-put", "adapter-get", "adapter-list", "adapter-retire":
		return adapterCommand(ctx, verb, f, opts, args, out)
	case "target-config-put", "target-config-get", "target-config-list", "target-config-retire", "target-config-validate", "target-config-diff", "target-config-simulate":
		return targetCommand(ctx, verb, f, opts, args, out)
	case "workflow-put", "workflow-get", "workflow-latest", "workflow-list", "workflow-retire", "workflow-diff", "workflow-validate", "workflow-simulate":
		return workflowCommand(ctx, verb, f, opts, args, out)
	case "run", "start-work-item":
		return submitCommand(ctx, verb, f, opts, args, out)
	case "resume":
		return resumeCommand(ctx, f, opts, args, out)
	case "show":
		return showCommand(ctx, f, opts, normalizeShowArgs(args), out)
	default:
		return usage("unsupported command %q; run fae help", verb)
	}
}

func normalizeShowArgs(args []string) []string {
	hasTask := false
	for _, a := range args {
		if a == "--task" || a == "-task" || strings.HasPrefix(a, "--task=") || strings.HasPrefix(a, "-task=") {
			hasTask = true
			break
		}
	}
	if hasTask {
		return args
	}
	out := make([]string, len(args))
	for i, a := range args {
		switch {
		case a == "--id" || a == "-id":
			out[i] = "--task"
		case strings.HasPrefix(a, "--id="):
			out[i] = "--task=" + strings.TrimPrefix(a, "--id=")
		case strings.HasPrefix(a, "-id="):
			out[i] = "-task=" + strings.TrimPrefix(a, "-id=")
		default:
			out[i] = a
		}
	}
	return out
}

func lookupUncertainDispatch(ctx context.Context, opts common, id string) (string, error) {
	quiet := opts
	quiet.json = true
	var buf bytes.Buffer
	if err := rpc(ctx, quiet, "TaskGet", "", daemon.TaskGetParams{ID: id}, &buf); err != nil {
		return "", err
	}
	var doc struct {
		Dispatch      string          `json:"dispatch"`
		DispatchID    string          `json:"dispatch_id"`
		InFlight      json.RawMessage `json:"in_flight"`
		PendingDelete *struct {
			Dispatch string `json:"dispatch"`
		} `json:"pending_delete"`
		Task *struct {
			Dispatch   string `json:"dispatch"`
			DispatchID string `json:"dispatch_id"`
		} `json:"task"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err == nil {
		var inFlightObj struct {
			ID         string `json:"id"`
			Dispatch   string `json:"dispatch"`
			DispatchID string `json:"dispatch_id"`
		}
		_ = json.Unmarshal(doc.InFlight, &inFlightObj)
		for _, candidate := range []string{
			doc.Dispatch,
			doc.DispatchID,
			inFlightObj.ID,
			inFlightObj.Dispatch,
			inFlightObj.DispatchID,
			func() string {
				if doc.PendingDelete != nil {
					return doc.PendingDelete.Dispatch
				}
				return ""
			}(),
			func() string {
				if doc.Task != nil {
					if doc.Task.Dispatch != "" {
						return doc.Task.Dispatch
					}
					return doc.Task.DispatchID
				}
				return ""
			}(),
		} {
			if candidate != "" {
				return candidate, nil
			}
		}
	}
	buf.Reset()
	if err := rpc(ctx, quiet, "Status", "", map[string]any{"include_retired": false, "tasks": true}, &buf); err == nil {
		var st struct {
			Tasks []struct {
				ID         string `json:"id"`
				Dispatch   string `json:"dispatch"`
				DispatchID string `json:"dispatch_id"`
			} `json:"tasks"`
		}
		if json.Unmarshal(buf.Bytes(), &st) == nil {
			for _, t := range st.Tasks {
				if t.ID == id {
					if t.Dispatch != "" {
						return t.Dispatch, nil
					}
					if t.DispatchID != "" {
						return t.DispatchID, nil
					}
				}
			}
		}
	}
	return "", fmt.Errorf("resolve-uncertain: task %s has no current in-flight or uncertain dispatch; pass --dispatch ID", id)
}
func readBounded(path string) ([]byte, error) { return readLimited(path, 1<<20) }

// readLimited reads a file of at most limit bytes, never more.
func readLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		if limit == 1<<20 {
			return nil, fmt.Errorf("input exceeds 1 MiB")
		}
		return nil, fmt.Errorf("input exceeds %d bytes", limit)
	}
	return raw, nil
}
func inputText(text, file string) (string, error) {
	if (text == "") == (file == "") {
		return "", usage("provide exactly one of --prompt/--text or --file")
	}
	if file != "" {
		raw, err := readBounded(file)
		if err != nil {
			return "", err
		}
		text = string(raw)
	}
	if len(text) == 0 || len(text) > 1<<20 {
		return "", fmt.Errorf("content must contain 1..1048576 bytes")
	}
	return text, nil
}
func readCredential(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() != 32 {
		return nil, fmt.Errorf("unsafe credential file")
	}
	b := make([]byte, 32)
	_, err = io.ReadFull(f, b)
	return b, err
}
func rpc(ctx context.Context, c common, method, key string, params any, out io.Writer) error {
	layout, err := daemon.Paths(c.home, c.instance)
	if err != nil {
		return err
	}
	credential, err := readCredential(layout.Credential)
	if err != nil {
		return dialError(err)
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	raw, err = domain.CanonicalJSON(raw)
	if err != nil {
		return err
	}
	result, err := api.Call(ctx, layout.Socket, credential, api.Request{Version: api.Version, Method: method, Key: key, Params: raw})
	if err != nil {
		return dialError(err)
	}
	if result.Version != api.Version {
		return fmt.Errorf("unsupported RPC response version %d", result.Version)
	}
	if method == "Tree" && !c.json {
		var page daemon.TreePage
		if err := json.Unmarshal(result.Result, &page); err != nil {
			return err
		}
		renderRunTree(out, page)
		return nil
	}
	if method == "WorkflowSimulate" {
		var simulated workflow.Simulation
		if err := json.Unmarshal(result.Result, &simulated); err != nil {
			return err
		}
		return simulationResult(simulated, false, out)
	}
	return jsonOut(out, result.Result)
}

// dialError maps an absent credential or a failed socket dial to exit 3.
func dialError(err error) error {
	var neterr *net.OpError
	if errors.Is(err, os.ErrNotExist) || errors.As(err, &neterr) {
		return unreachable(err)
	}
	return err
}

// streamEvents is read-only: it never starts a daemon or creates credentials.
func streamEvents(ctx context.Context, c common, after int64, out io.Writer, include ...bool) error {
	return streamEventsFiltered(ctx, c, after, out, len(include) > 0 && include[0], daemon.EventFilter{})
}
func streamEventsFiltered(ctx context.Context, c common, after int64, out io.Writer, include bool, filter daemon.EventFilter) error {
	layout, err := daemon.Paths(c.home, c.instance)
	if err != nil {
		return err
	}
	credential, err := readCredential(layout.Credential)
	if err != nil {
		if os.IsNotExist(err) {
			return unreachable(err)
		}
		return err
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", layout.Socket)
	}}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, "GET", "http://unix/v1/events?"+url.Values{"after": {strconv.FormatInt(after, 10)}, "include_retired": {strconv.FormatBool(include)}, "task": {filter.Task}, "tree_root": {filter.TreeRoot}, "run_id": {filter.RunID}, "workflow_id": {filter.WorkflowID}, "kind": {filter.Kind}}.Encode(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", hex.EncodeToString(credential))
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		var network *net.OpError
		if errors.As(err, &network) {
			return unreachable(err)
		}
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("event stream HTTP status %d", response.StatusCode)
	}
	decoder := json.NewDecoder(response.Body)
	for {
		var event json.RawMessage
		err = decoder.Decode(&event)
		if errors.Is(err, io.EOF) || ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintln(out, string(event)); err != nil {
			return err
		}
	}
}
