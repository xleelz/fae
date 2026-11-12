package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/xleelz/fae/daemon"
	"github.com/xleelz/fae/symphony"
	"github.com/xleelz/fae/tracker"
)

// RunSymphonyCLI runs the Symphony CLI entrypoint and returns an OS exit code (Symphony §17.7).
func RunSymphonyCLI(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runSymphony(ctx, args, stdout, stderr)
}

func runSymphony(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runSymphonyInternal(ctx, args, stdout, stderr, nil, nil)
}

func runSymphonyInternal(
	ctx context.Context,
	args []string,
	stdout, stderr io.Writer,
	customAdapter tracker.Adapter,
	onReady func(orch *daemon.LinearOrchestrator, srv *daemon.SymphonyHTTPServer),
) int {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}

	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}

	fs := flag.NewFlagSet("symphony", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	portFlag := -1
	onceFlag := false
	workflowFlag := ""
	fs.IntVar(&portFlag, "port", -1, "HTTP observability port override")
	fs.BoolVar(&onceFlag, "once", false, "run one poll/reconcile cycle and exit")
	fs.StringVar(&workflowFlag, "workflow", "", "path to WORKFLOW.md")
	fs.StringVar(&workflowFlag, "workflow-md", "", "path to WORKFLOW.md")

	var positional []string
	remaining := args
	for len(remaining) > 0 {
		if err := fs.Parse(remaining); err != nil {
			fmt.Fprintf(stderr, "symphony flag error: %v\n", err)
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		if fs.Arg(0) == "serve" && len(positional) == 0 {
			remaining = fs.Args()[1:]
			continue
		}
		positional = append(positional, fs.Arg(0))
		remaining = fs.Args()[1:]
	}

	if len(positional) > 1 {
		fmt.Fprintf(stderr, "symphony: unexpected extra arguments: %s\n", strings.Join(positional[1:], " "))
		return 2
	}

	workflowPath := strings.TrimSpace(workflowFlag)
	if len(positional) == 1 && strings.TrimSpace(positional[0]) != "" {
		if workflowPath != "" && workflowPath != strings.TrimSpace(positional[0]) {
			fmt.Fprintf(stderr, "symphony: conflicting workflow paths %q and %q\n", workflowPath, positional[0])
			return 2
		}
		workflowPath = strings.TrimSpace(positional[0])
	}
	workflowPath = symphony.ResolveWorkflowPath(workflowPath)

	wfDef, svcCfg, err := loadAndValidateWorkflowFile(workflowPath)
	if err != nil {
		fmt.Fprintf(stderr, "symphony startup error: %v\n", err)
		return 1
	}

	cfgManager, _ := symphony.NewConfigManager(
		wfDef.Path,
		symphony.WithSupportedTrackers("linear", "memory", "fae"),
	)

	if portFlag >= 0 {
		p := portFlag
		svcCfg.Server.Port = &p
	}

	adapter := customAdapter
	if adapter == nil {
		adapter, err = buildCLIAdapter(svcCfg)
		if err != nil {
			fmt.Fprintf(stderr, "symphony tracker error: %v\n", err)
			return 1
		}
	}

	orch := daemon.NewLinearOrchestrator(daemon.OrchestratorConfig{
		Adapter:               adapter,
		ServiceConfig:         svcCfg,
		PromptTemplate:        wfDef.PromptTemplate,
		ConfigManager:         cfgManager,
		EnableAppServerRunner: true,
	})

	if err := orch.ValidateDispatchConfig(); err != nil {
		fmt.Fprintf(stderr, "symphony validation error: %v\n", err)
		return 1
	}

	var httpSrv *daemon.SymphonyHTTPServer
	if svcCfg.Server.Port != nil {
		host := svcCfg.Server.Host
		if strings.TrimSpace(host) == "" {
			host = "127.0.0.1"
		}
		httpSrv = daemon.NewSymphonyHTTPServer(orch, host, *svcCfg.Server.Port)
		if err := httpSrv.Start(); err != nil {
			fmt.Fprintf(stderr, "symphony http server error: %v\n", err)
			return 1
		}
		defer func() {
			_ = httpSrv.Close(context.Background())
		}()
		fmt.Fprintf(stdout, "symphony listening on %s (workflow=%s)\n", httpSrv.Addr(), wfDef.Path)
	}

	if onReady != nil {
		onReady(orch, httpSrv)
	}

	if err := orch.RunLoop(ctx, onceFlag); err != nil {
		fmt.Fprintf(stderr, "symphony runtime error: %v\n", err)
		return 1
	}
	return 0
}

func loadAndValidateWorkflowFile(path string) (symphony.WorkflowDefinition, symphony.ServiceConfig, error) {
	def, err := symphony.LoadWorkflow(path)
	if err != nil {
		return symphony.WorkflowDefinition{}, symphony.ServiceConfig{}, err
	}
	if err := symphony.ValidatePromptTemplate(def.PromptTemplate); err != nil {
		return symphony.WorkflowDefinition{}, symphony.ServiceConfig{}, err
	}
	cfg, err := symphony.ResolveServiceConfig(def, os.Getenv)
	if err != nil {
		return symphony.WorkflowDefinition{}, symphony.ServiceConfig{}, err
	}
	if err := validateStartupServiceConfig(*cfg); err != nil {
		return symphony.WorkflowDefinition{}, symphony.ServiceConfig{}, err
	}
	return *def, *cfg, nil
}

func validateStartupServiceConfig(cfg symphony.ServiceConfig) error {
	return symphony.ValidateDispatchConfig(&cfg, "linear", "memory", "fae")
}

func buildCLIAdapter(cfg symphony.ServiceConfig) (tracker.Adapter, error) {
	if strings.EqualFold(strings.TrimSpace(cfg.Tracker.Kind), "linear") {
		if mock, _ := cfg.Tracker.Provider["_mock"].(bool); !mock {
			if mockStr, _ := cfg.Tracker.Provider["_mock"].(string); !strings.EqualFold(strings.TrimSpace(mockStr), "true") {
				return tracker.NewAdapter(cfg.Tracker, os.Getenv, nil)
			}
		}
		// Validate adapter config even in _mock mode
		if _, err := tracker.NewAdapter(cfg.Tracker, os.Getenv, nil); err != nil {
			return nil, err
		}
	}
	return &cliTrackerAdapter{
		kind:           strings.ToLower(strings.TrimSpace(cfg.Tracker.Kind)),
		activeStates:   append([]string(nil), cfg.Tracker.ActiveStates...),
		terminalStates: append([]string(nil), cfg.Tracker.TerminalStates...),
		issues:         make(map[string]symphony.Issue),
	}, nil
}

type cliTrackerAdapter struct {
	mu             sync.RWMutex
	kind           string
	activeStates   []string
	terminalStates []string
	issues         map[string]symphony.Issue
}

func (a *cliTrackerAdapter) Kind() string             { return a.kind }
func (a *cliTrackerAdapter) ActiveStates() []string   { return a.activeStates }
func (a *cliTrackerAdapter) TerminalStates() []string { return a.terminalStates }

func (a *cliTrackerAdapter) FetchIssuesByStates(_ context.Context, stateNames []string) ([]symphony.Issue, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var out []symphony.Issue
	for _, iss := range a.issues {
		if symphony.StateInList(iss.State, stateNames) {
			out = append(out, iss)
		}
	}
	return out, nil
}

func (a *cliTrackerAdapter) FetchIssuesByIDs(_ context.Context, issueIDs []string) ([]symphony.Issue, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var out []symphony.Issue
	for _, id := range issueIDs {
		if iss, ok := a.issues[id]; ok {
			out = append(out, iss)
		}
	}
	return out, nil
}

func (a *cliTrackerAdapter) TransitionIssueState(_ context.Context, issueID string, targetStateName string) (symphony.Issue, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	iss, ok := a.issues[issueID]
	if !ok {
		return symphony.Issue{}, fmt.Errorf("issue %s not found", issueID)
	}
	iss.State = targetStateName
	a.issues[issueID] = iss
	return iss, nil
}

func (a *cliTrackerAdapter) AddIssueComment(_ context.Context, _ string, _ string) (string, error) {
	return "comment-1", nil
}

func (a *cliTrackerAdapter) AgentToolSpecs() []tracker.ToolSpec {
	return nil
}

func (a *cliTrackerAdapter) SecretEnvironmentNames() []string {
	return []string{"LINEAR_API_KEY"}
}

func (a *cliTrackerAdapter) ExecuteAgentTool(_ context.Context, name string, _ map[string]any, _ tracker.ToolContext) tracker.ToolResult {
	return tracker.ToolResult{Success: true, Output: map[string]any{"tool": name}}
}
