// Package provider supplies restricted real Codex and Claude Code CLI bridges.
// Model processes are read-only and command tools are disabled; only validated
// proposed edits are applied by this trusted bridge after the model has exited.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/commandshim"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/xleelz/fae/domain"
	agentruntime "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/secretshape"
	"github.com/xleelz/fae/workflow"
)

type TaskConfig struct {
	Files    []string `json:"files"`
	TestFile string   `json:"test_file"`
}

type Config struct {
	// Provider selects the CLI protocol: "codex" (default) or "claude". The
	// workflow binding must name the same provider.
	Provider       string
	SessionRoot    string
	TaskScopesJSON string
	Binary         string
	Files          []string
	TestFile       string
	Timeout        time.Duration
	// StartTimeout bounds the provider's acknowledgement of the prompt
	// (FAE-RT-004); zero uses DefaultStartTimeout.
	StartTimeout time.Duration
	// WaitDelay bounds Wait after the provider exits or is stopped while a
	// descendant still holds stdout/stderr open; zero uses ProviderWaitDelay.
	WaitDelay time.Duration
	// Reads, DenyPaths, ReadRoots and OnRefusedRead enforce FAE-SEC-008,
	// FAE-SEC-009 and FAE-RT-016 across inline files and <repository>.
	Reads         *workflow.ReadPolicy
	DenyPaths     []string
	ReadRoots     map[string]ReadRootView
	OnRefusedRead func(task, role, path string)
}

func (c Config) provider() string {
	if c.Provider == "" {
		return "codex"
	}
	return c.Provider
}

func regularRead(path string, optional bool) ([]byte, error) {
	f, e := os.Lstat(path)
	if os.IsNotExist(e) && optional {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	if !f.Mode().IsRegular() || f.Size() > 1<<20 {
		return nil, errors.New("input must be a bounded regular file")
	}
	return os.ReadFile(path)
}
func safePath(root, name string) (string, error) {
	if name == "" || filepath.IsAbs(name) || filepath.Clean(name) != name || strings.HasPrefix(name, "../") || name == ".." || (name == "." || name == ".git" || strings.HasPrefix(name, ".git/")) {
		return "", errors.New("unsafe proposed path")
	}
	p := filepath.Join(root, name)
	for a := p; a != root; a = filepath.Dir(a) {
		st, e := os.Lstat(a)
		if e == nil && st.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("symlink path refused")
		}
		if e != nil && !os.IsNotExist(e) {
			return "", e
		}
	}
	return p, nil
}
func parseBrief(raw json.RawMessage) (json.RawMessage, string, error) {
	var blocks []struct {
		Kind, Stage  string
		OutputSchema json.RawMessage `json:"output_schema"`
	}
	if e := json.Unmarshal(raw, &blocks); e != nil {
		return nil, "", e
	}
	var schema json.RawMessage
	stage := ""
	for _, b := range blocks {
		if b.Kind == "kernel_contract" {
			schema = b.OutputSchema
		}
		if b.Kind == "stage" {
			stage = b.Stage
		}
	}
	if len(schema) == 0 {
		return nil, "", errors.New("output schema missing")
	}
	return schema, stage, nil
}
func validateEdits(c Config, l agentruntime.BridgeConfig, stage string, r Response) error {
	allowed := map[string]bool{}
	for _, p := range c.Files {
		allowed[p] = true
	}
	for _, p := range l.ReadOnlyFiles {
		delete(allowed, p)
	}
	canEdit := false
	for _, p := range l.Profile.Allowed {
		switch p {
		case "edit":
			canEdit = true
		case "write":
			if l.Session != "agentic" {
				return fmt.Errorf("unsupported bridge capability %s", p)
			}
			canEdit = true
		case "handoff", "read":
		case "list", "search_glob", "search_regex", "shell":
			if l.Session != "agentic" {
				return fmt.Errorf("unsupported bridge capability %s", p)
			}
		default:
			return fmt.Errorf("unsupported bridge capability %s", p)
		}
	}
	if (l.Profile.Build || l.Profile.Test || l.Profile.Format) && !(l.Session == "agentic" && agentruntime.AgenticShellAllowed(l.Profile)) {
		return errors.New("provider cannot execute host commands; use engine broker")
	}
	for _, p := range l.Profile.Denied {
		if p == "edit" || p == "write" {
			canEdit = false
		}
	}
	changeStage := l.ChangeProducer || stage == "test-first" || stage == "implement" || len(l.WriteScopes) > 0 || l.ArtifactsOnly
	// Test-first writes one RED test: the task's test file once a RED report
	// names it, else exactly one Go test file in scope.
	if stage == "test-first" && c.TestFile == "" && len(r.Edits) > 1 {
		return errors.New("RED may edit exactly one test file")
	}
	maxContentBytes := maxEditBytes
	if l.InlineFileBytes > maxContentBytes {
		maxContentBytes = l.InlineFileBytes
	}
	seen := map[string]bool{}
	for i, edit := range r.Edits {
		if slices.Contains(l.ReadOnlyFiles, edit.Path) {
			return errors.New("unauthorized edit: read_only_inputs cannot be edited")
		}
		if !canEdit || !allowed[edit.Path] || seen[edit.Path] || edit.Content != nil && len(*edit.Content) > maxContentBytes {
			return errors.New("unauthorized edit")
		}
		if (edit.Content == nil) == (len(edit.Replacements) == 0) {
			return fmt.Errorf("edit %d (%q) must use exactly one of content or replacements", i, edit.Path)
		}
		switch {
		case stage == "test-first" && c.TestFile != "" && edit.Path != c.TestFile:
			return errors.New("RED may edit only the task's test file")
		case stage == "test-first" && c.TestFile == "" && !workflow.TestSourceFile(edit.Path, ""):
			return errors.New("RED may edit only a Go test file")
		case stage == "implement" && edit.Path == c.TestFile:
			return errors.New("implementation cannot edit frozen test")
		case !changeStage:
			return errors.New("stage may not edit files")
		}
		if _, e := safePath(l.Worktree, edit.Path); e != nil {
			return e
		}
		seen[edit.Path] = true
	}
	return nil
}

// goTestFile reports whether name is a file the go tool builds as a test.
func goTestFile(name string) bool {
	base := filepath.Base(name)
	return strings.HasSuffix(base, "_test.go") && len(base) > len("_test.go") && base[0] != '_' && base[0] != '.'
}
func codexArgs(l agentruntime.BridgeConfig, session *providerSession, schemaFile, answer string) []string {
	args := []string{"exec", "--ignore-user-config", "--ignore-rules", "--ephemeral", "--skip-git-repo-check", "--sandbox", "read-only", "--disable", "shell_tool", "--disable", "apps", "--disable", "browser_use", "-c", "web_search=\"disabled\"", "-c", "approval_policy=\"never\"", "--model", l.Binding.Model, "--json", "--output-schema", schemaFile, "--output-last-message", answer, "-"}
	if l.Binding.Effort != "" {
		args = append(args[:len(args)-1], "-c", "model_reasoning_effort="+fmt.Sprintf("%q", l.Binding.Effort), "-")
	}
	resumeID := ""
	if !l.FreshRole {
		if l.ResumeSession != "" && threadIDPattern.MatchString(l.ResumeSession) {
			resumeID = l.ResumeSession
		} else if session != nil && session.resumed {
			resumeID = session.state.ThreadID
		}
	}
	if session != nil || resumeID != "" {
		filtered := args[:0]
		for _, arg := range args {
			if arg != "--ephemeral" {
				filtered = append(filtered, arg)
			}
		}
		args = filtered
		if resumeID != "" {
			// Sandbox is an exec option, before the resume subcommand. All
			// per-turn model/tool/schema restrictions are repeated on resume.
			filtered = []string{"exec", "--sandbox", "read-only", "resume"}
			for i := 1; i < len(args)-1; i++ {
				if args[i] == "--sandbox" {
					i++
					continue
				}
				filtered = append(filtered, args[i])
			}
			args = append(filtered, resumeID, "-")
		}
	}
	return args
}

// Run runs one fae-agent-v1 turn. A turn it refuses with a class (see
// Refusal) is recorded in the session's log directory before Run returns it.
func Run(ctx context.Context, c Config, configPath string) error {
	var l agentruntime.BridgeConfig
	err := run(ctx, c, configPath, &l)
	if refusal, ok := err.(*Refusal); ok {
		return recordRefusal(l, refusal)
	}
	return err
}

func run(ctx context.Context, c Config, configPath string, config *agentruntime.BridgeConfig) (retErr error) {
	// Where the turn stopped decides a refusal's class. Only a step before the
	// provider's launch refuses locally, and only a completed provider turn's
	// answer is refused as output; every other failure stays untyped.
	providerLaunched, providerCompleted := false, false
	local := func(e error) error {
		if providerLaunched {
			return e
		}
		return &Refusal{Class: agentruntime.RefusedBeforeProvider, Err: e}
	}
	refused := func(e error) error {
		if !providerCompleted {
			return e
		}
		return &Refusal{Class: agentruntime.RefusedOutput, Err: e}
	}
	// A binding this bridge cannot run as configured is refused as a binding:
	// no retry of the same binding can succeed until the configuration
	// changes, so the engine moves the turn to the next route.
	refusedBinding := func(e error) error {
		if providerLaunched {
			return e
		}
		return &Refusal{Class: agentruntime.RefusedBinding, Err: e}
	}
	// A provider that never acknowledged the prompt is typed apart from one
	// that failed: see the launch below.
	notStarted := func(e error) error {
		return &Refusal{Class: agentruntime.ProviderNotStarted, Err: e}
	}
	providerFailed := func(e error) error {
		return &Refusal{Class: agentruntime.ProviderFailed, Err: e}
	}
	raw, e := regularRead(configPath, false)
	if e != nil {
		return e
	}
	raw, e = domain.CanonicalJSON(raw)
	if e != nil {
		return e
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if e = dec.Decode(config); e != nil {
		return e
	}
	l := *config
	isProphet := l.WorkflowID == "fae-prophet"
	if isProphet {
		c.SessionRoot = ""
		c.Files = nil
		c.TestFile = ""
		c.TaskScopesJSON = ""
		if !l.FreshContext || l.Role != "prophet" {
			return local(errors.New("Prophet requires isolated fresh context"))
		}
		for _, capability := range l.Profile.Allowed {
			if capability != "handoff" {
				return local(errors.New("Prophet cannot access tools or files"))
			}
		}
	}
	isLoop := strings.HasPrefix(l.WorkflowID, "fae-loop:")
	if isLoop {
		if c.SessionRoot == "" || l.SessionPolicy != "task-bound" || l.FreshContext {
			return local(errors.New("provider loop requires persistent session root and task-bound context"))
		}
		for _, capability := range l.Profile.Allowed {
			if capability != "read" && capability != "handoff" {
				return local(errors.New("provider loop is read-only"))
			}
		}
		if l.Profile.Build || l.Profile.Test || l.Profile.Format {
			return local(errors.New("provider loop cannot execute commands"))
		}
	}
	pinned := false
	if c.TaskScopesJSON != "" && !isLoop {
		if len(c.TaskScopesJSON) > 1<<20 {
			return local(errors.New("provider scope configuration too large"))
		}
		raw, e := domain.CanonicalJSON([]byte(c.TaskScopesJSON))
		if e != nil {
			return local(e)
		}
		var tasks map[string]TaskConfig
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if e = dec.Decode(&tasks); e != nil {
			return local(e)
		}
		task, ok := tasks[l.TaskID]
		if !ok || len(task.Files) == 0 || task.TestFile == "" {
			return local(errors.New("pinned provider task scope required"))
		}
		c.Files = task.Files
		c.TestFile = task.TestFile
		pinned = true
	} else if slices.ContainsFunc(c.Files, func(name string) bool { return name != "" }) {
		pinned = true
	} else {
		// An absent --files splits to [""], which pins nothing.
		c.Files = nil
	}
	if p := c.provider(); p != "codex" && p != "claude" {
		return refusedBinding(fmt.Errorf("unsupported provider %q", p))
	}
	if l.Protocol != "fae-agent-v1" {
		return local(fmt.Errorf("%s bridge binding/configuration required", c.provider()))
	}
	if l.Binding.Provider != c.provider() {
		return refusedBinding(fmt.Errorf("this %s bridge does not serve %q bindings", c.provider(), l.Binding.Provider))
	}
	if l.Binding.Model == "" || !filepath.IsAbs(c.Binary) || c.Timeout <= 0 || c.StartTimeout < 0 {
		return refusedBinding(fmt.Errorf("%s bridge binding/configuration required", c.provider()))
	}
	if e = validateProviderStateDirectories(os.Environ()); e != nil {
		return refusedBinding(e)
	}
	if c.provider() == "claude" {
		// The effort, model, configuration directory and credentials.
		if e = claudePreflight(l, os.Environ()); e != nil {
			return refusedBinding(e)
		}
	}
	// Refuse stale evidence before launching a provider or changing task
	// files. Another writer's handoff is no task-scope fact: it stays untyped.
	if _, e := os.Lstat(l.Handoff); e == nil {
		return errors.New("handoff already exists")
	} else if !os.IsNotExist(e) {
		return e
	}
	root, e := filepath.EvalSymlinks(l.Worktree)
	if e != nil || root != l.Worktree {
		return local(errors.New("canonical worktree required"))
	}
	schema, stage, e := parseBrief(l.Brief)
	if e != nil {
		return local(e)
	}
	approved, e := briefApprovedScope(l.Brief)
	if e != nil {
		return local(e)
	}
	if !isProphet {
		if block, err := taskScopeFromBrief(l.Brief); err == nil && block != nil && len(block.ReadOnlyFiles) > 0 {
			for _, ro := range block.ReadOnlyFiles {
				if !slices.Contains(l.ReadOnlyFiles, ro) {
					l.ReadOnlyFiles = append(l.ReadOnlyFiles, ro)
				}
			}
		}
		if c.Files, c.TestFile, e = settleTaskScope(root, l.Brief, c.Files, c.TestFile, pinned, stage == "test-first" || stage == "implement", approved); e != nil {
			return local(e)
		}
	}
	c.Files = append(append([]string(nil), c.Files...), briefScope(l.Brief, c.Files)...)
	guard := &ReadGuard{
		TaskID:        l.TaskID,
		Role:          l.Role,
		Worktree:      root,
		Policy:        c.Reads,
		DenyPaths:     c.DenyPaths,
		ReadRoots:     c.ReadRoots,
		LogDirectory:  l.LogDirectory,
		OnRefusedRead: c.OnRefusedRead,
	}
	if tcBlock, _ := taskScopeFromBrief(l.Brief); tcBlock != nil {
		if tcBlock.Reads != nil {
			guard.Policy = workflow.ComposeReadPolicy(nil, guard.Policy, tcBlock.Reads)
		}
		for _, denied := range tcBlock.DenyPaths {
			if !slices.Contains(guard.DenyPaths, denied) {
				guard.DenyPaths = append(guard.DenyPaths, denied)
			}
		}
		if len(guard.ReadRoots) == 0 && len(tcBlock.ReadRoots) > 0 {
			guard.ReadRoots = tcBlock.ReadRoots
		}
	}
	// A runtime-bound empty root list means this role has no external roots;
	// client-wide defaults cannot widen a session's declared bindings.
	if l.ReadRootPaths != nil {
		permitted := map[string]ReadRootView{}
		if block, blockErr := taskScopeFromBrief(l.Brief); blockErr == nil && block != nil {
			for name, view := range block.ReadRoots {
				if slices.Contains(l.ReadRootPaths, view.Path) {
					permitted[name] = view
				}
			}
		}
		guard.ReadRoots = permitted
	}
	if len(l.NativeReadDirs) > 0 {
		roots := map[string]ReadRootView{}
		for name, rootView := range guard.ReadRoots {
			roots[name] = rootView
		}
		for i, path := range l.NativeReadDirs {
			roots[fmt.Sprintf("ancestor-output-%d", i)] = ReadRootView{Name: fmt.Sprintf("ancestor-output-%d", i), Path: path}
		}
		guard.ReadRoots = roots
	}
	if guard.Policy != nil || len(guard.DenyPaths) > 0 {
		c.Files = guard.FilterInlineFiles(c.Files)
		l.ReadOnlyFiles = guard.FilterInlineFiles(l.ReadOnlyFiles)
		if c.TestFile != "" && !guard.AllowPath(c.TestFile) {
			guard.auditRefusal(c.TestFile)
			c.TestFile = ""
		}
	}
	if e = validateEdits(c, l, stage, Response{}); e != nil {
		return local(e)
	}
	if l.Session == "agentic" && agentruntime.AgenticShellAllowed(l.Profile) {
		if e = agentruntime.VerifyAgenticShellLaunch(os.Environ(), l.ClaudeSettings); e != nil {
			return local(e)
		}
	}
	var files map[string]string
	var original map[string][]byte
	var exists map[string]bool
	if l.Session == "agentic" || l.InlineFileBytes > 0 || l.InlineTurnBytes > 0 || len(l.ReadOnlyFiles) > 0 {
		captured, err := CaptureTaskScope(root, TaskScope{
			Session:         l.Session,
			Files:           c.Files,
			ReadOnlyFiles:   l.ReadOnlyFiles,
			TestFile:        c.TestFile,
			InlineFileBytes: l.InlineFileBytes,
			InlineTurnBytes: l.InlineTurnBytes,
		})
		if err != nil {
			return local(err)
		}
		files = captured.WriteScope
		original = map[string][]byte{}
		exists = map[string]bool{}
		for name, content := range captured.WriteScope {
			path, err := safePath(root, name)
			if err != nil {
				return local(err)
			}
			_, statErr := os.Lstat(path)
			exists[name] = statErr == nil
			original[name] = []byte(content)
		}
		for _, meta := range captured.Metadata {
			if meta.Scope != "write_scope" {
				continue
			}
			path, err := safePath(root, meta.Path)
			if err != nil {
				return local(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return local(err)
			}
			exists[meta.Path] = true
			original[meta.Path] = data
		}
		if len(captured.UnavailableScope) > 0 {
			c.Files = slices.DeleteFunc(slices.Clone(c.Files), func(name string) bool {
				return slices.ContainsFunc(captured.UnavailableScope, func(u UnavailableScopeEntry) bool { return u.Path == name })
			})
		}
		if l.Brief, e = withBoundedTaskScope(l.Brief, captured); e != nil {
			return local(e)
		}
	} else {
		// An approved widen file the turn can't load is listed, not sent and not
		// editable, instead of refusing every later turn (loadScopeFiles).
		scope, err := loadScopeFiles(root, c.Files, approved, c.TestFile)
		if err != nil {
			return local(err)
		}
		files, original, exists = scope.files, scope.original, scope.exists
		if len(scope.unavailable) > 0 {
			c.Files = scope.editable(c.Files)
			if l.Brief, e = withUnavailableScope(l.Brief, scope.unavailable); e != nil {
				return local(e)
			}
		}
	}
	// Every turn but the Prophet's sees the repository layout; a provider
	// loop sees it only when it already gets files.
	var repository json.RawMessage
	if !isProphet && (!isLoop || len(files) > 0) {
		if repository, e = listRepositoryFiltered(ctx, root, l.LogDirectory, guard.Policy, guard.DenyPaths); e != nil {
			return local(e)
		}
	}
	toolRule := "Never use tools."
	if l.Session == "agentic" {
		allowed := agentruntime.AllowedAgenticTools(l.Profile, l.ChangeProducer || stage == "test-first" || stage == "implement" || len(l.WriteScopes) > 0 || l.ArtifactsOnly)
		toolRule = "Agentic session: use only allowed tools (" + strings.Join(allowed, ", ") + ") and complete with structured handoff output."
	} else if c.provider() == "claude" {
		// Claude Code returns schema-bound output through its only tool.
		toolRule = "Return the result only as the structured output; no other tools exist."
	}
	prompt, e := assemblePrompt(l, root, files, repository, outputProtocol(toolRule))
	if e != nil {
		return local(e)
	}
	schemaRaw := responseSchema(schema)
	dir, e := os.MkdirTemp(l.LogDirectory, "provider-")
	if e != nil {
		return local(e)
	}
	schemaFile := filepath.Join(dir, "schema.json")
	answer := filepath.Join(dir, "answer.json")
	if e = os.WriteFile(schemaFile, schemaRaw, 0600); e != nil {
		return local(e)
	}
	session, e := openProviderSession(ctx, c.SessionRoot, l)
	if e != nil {
		return local(e)
	}
	if session != nil {
		if l.FreshRole {
			session.resumed = false
		} else if l.ResumeSession != "" {
			session.state.ThreadID = l.ResumeSession
			session.resumed = true
		}
	}
	defer session.close()
	// Empty private cwd prevents repository-local provider configuration/hooks.
	cwd := filepath.Join(dir, "cwd")
	if session != nil {
		cwd = filepath.Join(session.dir, "cwd")
	} else if e = os.Mkdir(cwd, 0700); e != nil {
		return local(e)
	}
	changeStage := l.ChangeProducer || stage == "test-first" || stage == "implement" || len(l.WriteScopes) > 0 || l.ArtifactsOnly
	editableScope := slices.Clone(c.Files)
	if stage == "implement" && c.TestFile != "" {
		editableScope = slices.DeleteFunc(editableScope, func(p string) bool { return p == c.TestFile })
	}
	// Read-only directories are protected by private read-only namespace
	// mounts. Writable shared directories use private scoped writeback below;
	// never diff/restore another concurrently running branch's whole directory.
	var startTree string
	if l.Session == "agentic" {
		if l.ResumeTree != "" {
			if e = RestoreCheckpointPreservingScratch(ctx, root, l.ResumeTree, l.Scratch); e != nil {
				return local(e)
			}
		}
		if startTree, e = SnapshotWorktreeTree(ctx, root); e != nil {
			return local(e)
		}
	}
	var args []string
	var stream *claudeStream
	defer func() {
		if stream != nil {
			writeProviderStats(l.LogDirectory, stream)
		}
	}()
	expectedThread := ""
	var toolbox *AgenticToolbox
	if l.Session == "agentic" {
		instanceRoot := l.InstanceRoot
		if instanceRoot == "" {
			instanceRoot = c.SessionRoot
		}
		if instanceRoot == "" && l.LogDirectory != "" {
			instanceRoot = filepath.Dir(l.LogDirectory)
		}
		toolbox = &AgenticToolbox{
			ToolAuditPath:           filepath.Join(l.LogDirectory, "tool-events.jsonl"),
			OutputOwnershipRegistry: l.OutputOwnershipRegistry, ExcludedOutputDirs: l.ExcludedOutputDirs, OutputDir: l.OutputDir, StagingDir: l.StagingDir, StagingWrite: l.StagingWrite, ArtifactsOnly: l.ArtifactsOnly,
			Scratch: l.Scratch, WriteScopes: l.WriteScopes, DeniedTestCommands: l.DeniedTestCommands,
			Worktree:       root,
			Profile:        l.Profile,
			ChangeProducer: changeStage,
			Stage:          stage,
			WriteScope:     editableScope,
			ReadOnlyFiles:  l.ReadOnlyFiles,
			TestFile:       c.TestFile,
			OutputSchema:   schema,
			Env:            os.Environ(),
			ClaudeSettings: l.ClaudeSettings,
			Guard:          guard,
			InstanceRoot:   instanceRoot,
			TaskID:         l.TaskID,
		}
	}
	var sessionView atomic.Pointer[SessionView]
	recordCheckpoint := func(sid string, turns int) {
		if view := sessionView.Load(); view != nil {
			if err := view.Collect(); err != nil {
				return
			}
		}
		if sid == "" {
			sid = expectedThread
		}
		if sid == "" && stream == nil {
			sid, _ = announcedThread(filepath.Join(dir, "events.jsonl"))
		}
		if l.Session != "agentic" && sid == "" {
			return
		}
		tree, err := SnapshotScopedTree(context.Background(), root, startTree, editableScope, l.Scratch)
		if err != nil {
			return
		}
		_ = SaveTurnCheckpointFile(l.LogDirectory, agentruntime.TurnCheckpoint{
			ProviderSession: sid,
			WorktreeTree:    tree,
			AssistantTurns:  turns,
		})
		if session != nil && sid != "" {
			_ = session.checkpoint(sid)
		}
	}
	if l.ReadRootPaths == nil {
		for _, view := range guard.ReadRoots {
			if !slices.Contains(l.NativeReadDirs, view.Path) && !slices.Contains(l.ReadRootPaths, view.Path) {
				l.ReadRootPaths = append(l.ReadRootPaths, view.Path)
			}
		}
	}
	if c.provider() == "claude" {
		if args, expectedThread, e = claudeArgs(l, session, schemaRaw); e != nil {
			return local(e)
		}
		stream = &claudeStream{toolbox: toolbox, onCheckpoint: recordCheckpoint}
		if l.Session == "agentic" {
			allowedMap := map[string]bool{}
			for _, tName := range agentruntime.ClaudeNativeTools(l.Profile, changeStage) {
				allowedMap[tName] = true
			}
			stream.allowedTools = allowedMap
			if l.TurnBudget != nil && l.TurnBudget.MaxAssistantTurns > 0 {
				stream.maxAssistantTurns = l.TurnBudget.MaxAssistantTurns
			}
		}
	} else {
		args = codexArgs(l, session, schemaFile, answer)
		if !l.FreshRole {
			if l.ResumeSession != "" && threadIDPattern.MatchString(l.ResumeSession) {
				expectedThread = l.ResumeSession
			} else if session != nil && session.resumed {
				expectedThread = session.state.ThreadID
			}
		}
	}
	effectiveTimeout := c.Timeout
	if l.WallSeconds > 0 {
		effectiveTimeout = time.Duration(l.WallSeconds) * time.Second
	}
	if l.TurnBudget != nil && l.TurnBudget.WallSeconds > 0 {
		effectiveTimeout = time.Duration(l.TurnBudget.WallSeconds)*time.Second + 5*time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, effectiveTimeout)
	defer cancel()
	// The provider runs under a context of its own, so the start bound can
	// stop it alone; any stop retires its process tree (stopProviderTree).
	providerCtx, stopProvider := context.WithCancel(ctx)
	defer stopProvider()
	if stream != nil {
		stream.onBudgetExhausted = stopProvider
	}
	var wallBudgetExpired atomic.Bool
	if l.TurnBudget != nil && l.TurnBudget.WallSeconds > 0 {
		wallTimer := time.AfterFunc(time.Duration(l.TurnBudget.WallSeconds)*time.Second, func() {
			wallBudgetExpired.Store(true)
			sid := expectedThread
			turns := 0
			if stream != nil {
				if cur := stream.currentSessionID(); cur != "" {
					sid = cur
				}
				turns = stream.turns()
			}
			recordCheckpoint(sid, turns)
			stopProvider()
		})
		defer wallTimer.Stop()
	}
	if l.Session == "agentic" {
		intervalSec := l.CheckpointIntervalSec
		if intervalSec <= 0 {
			intervalSec = 300
		}
		stopTicker := make(chan struct{})
		defer close(stopTicker)
		go func() {
			ticker := time.NewTicker(time.Duration(intervalSec) * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-stopTicker:
					return
				case <-providerCtx.Done():
					return
				case <-ticker.C:
					sid := expectedThread
					turns := 0
					if stream != nil {
						if cur := stream.currentSessionID(); cur != "" {
							sid = cur
						}
						turns = stream.turns()
					}
					recordCheckpoint(sid, turns)
				}
			}
		}()
	}
	cmd := exec.CommandContext(providerCtx, c.Binary, args...)
	cmd.Cancel = func() error { return stopProviderTree(cmd.Process) }
	// A descendant that left the provider's process tree before the start
	// bound's census (a setsid) and holds the event stream open would keep
	// Wait blocked after the provider itself was stopped or exited; the
	// delay bounds that wait and closes the pipes (ProviderWaitDelay).
	cmd.WaitDelay = ProviderWaitDelay
	if c.WaitDelay > 0 {
		cmd.WaitDelay = c.WaitDelay
	}
	if l.Session == "agentic" && agentruntime.AgenticShellAllowed(l.Profile) {
		cmd.Env = slices.Clone(os.Environ())
	} else {
		shims, e := commandshim.InstallDenials(dir)
		if e != nil {
			return local(e)
		}
		cmd.Env = commandshim.PrependPath(os.Environ(), shims)
	}
	if stream != nil {
		cmd.Env = claudeEnvironment(cmd.Env)
	}
	if toolbox != nil {
		toolbox.Env = cmd.Env
	}
	if l.Session == "agentic" {
		cwd = root
	}
	cmd.Dir = cwd
	if l.OutputDir != "" {
		cmd.Env = append(cmd.Env, "FAE_OUTPUT_DIR="+l.OutputDir)
	}
	if l.StagingDir != "" {
		cmd.Env = append(cmd.Env, "FAE_STAGING_DIR="+l.StagingDir)
	}
	if toolbox != nil {
		toolbox.Env = cmd.Env
	}
	cmd.Stdin = bytes.NewReader(prompt)
	events, e := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return local(e)
	}
	defer events.Close()
	stderr, e := os.OpenFile(filepath.Join(dir, "stderr.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return local(e)
	}
	defer stderr.Close()
	// FAE-SEC-003: the raw logs keep no provider-shaped credential, OAuth
	// token field value or credential-named value of the provider's
	// environment (secretshape.Writer); they hold whole lines until Flush.
	logged := secretshape.EnvironmentValues(cmd.Env)
	eventLog := secretshape.NewWriter(&boundedLog{destination: events, remaining: maxProviderLogBytes}, logged)
	stderrLog := secretshape.NewWriter(&boundedLog{destination: stderr, remaining: maxProviderLogBytes}, logged)
	cmd.Stdout = eventLog
	if stream != nil {
		// The event log is a bounded diagnostic prefix; the stream reader sees
		// every record so a long turn cannot truncate its terminal result.
		cmd.Stdout = io.MultiWriter(cmd.Stdout, stream)
	}
	ack := &providerAcknowledgement{}
	cmd.Stdout = io.MultiWriter(ack, cmd.Stdout)
	cmd.Stderr = stderrLog
	// FAE-RT-004: a provider that provably never took the turn is typed
	// ProviderNotStarted, which the executor relaunches within the
	// agent-start bound and charges to the binding only when that bound is
	// exhausted: its executable did not start, or it ended without writing
	// anything to its event stream (without acknowledging the prompt), with
	// no answer, whether it exited on its own, the start bound stopped it or
	// this bridge's timeout did. The start bound (StartTimeout, Appendix A's
	// 180 s) stops a provider that has not acknowledged the prompt in time,
	// with its process tree. Claude Code (stream-json) and Codex (--json)
	// acknowledge a turn with their first event, the session they start,
	// before the model is asked anything (providerAcknowledgement); and the
	// model has no tools here, and its edits reach the task only through a
	// validated answer.
	// FAE-WF-034: a provider that acknowledged the prompt and then exited with
	// a failure on its own, or that this bridge's timeout stopped, is
	// ProviderFailed: no answer, and nothing written, since edits are written
	// only below, from a completed and validated answer. A Claude Code turn
	// is typed so only when its announced surface is the allowed one. A
	// cancelled bridge and a Claude surface refusal stay untyped. The detail
	// names no per-turn path, so a repeated failure counts toward SameGate.
	mounts := []string{dir}
	viewGuard := *guard
	viewGuard.OutputOwnershipRegistry = l.OutputOwnershipRegistry
	viewGuard.RuntimeReadDirs = append(viewGuard.RuntimeReadDirs, l.RuntimeMounts...)
	viewGuard.RuntimeCacheDirs = l.CacheMounts
	viewGuard.HasShell = agentruntime.AgenticShellAllowed(l.Profile)
	scopeMayWrite := len(l.WriteScopes) > 0
	for _, layer := range l.WriteScopes {
		if len(layer) == 0 {
			scopeMayWrite = false
			break
		}
	}
	viewGuard.WritableWorktree = !l.ArtifactsOnly && ((changeStage && (len(l.WriteScopes) == 0 || scopeMayWrite)) || len(l.Scratch) > 0)
	viewGuard.RuntimeShellWithoutView = l.ShellRuntimeMountsWithoutView && RuntimeShellWithoutView(l.Profile, l.RuntimeMounts)
	viewGuard.CanWrite = func(rel string) bool {
		if l.ArtifactsOnly || workflow.MatchesAnyGlob(rel, l.ReadOnlyFiles) {
			return false
		}
		if _, protected := workflow.ProtectedPath(rel); protected {
			return false
		}
		if workflow.MatchesAnyGlob(rel, l.Scratch) {
			return true
		}
		if len(l.WriteScopes) > 0 {
			return workflow.AllowsWriteScopes(rel, l.WriteScopes)
		}
		return changeStage && workflow.MatchesAnyGlob(rel, editableScope)
	}

	viewGuard.WritableViews = map[string]func(string) bool{}
	if l.ArtifactsOnly && l.OutputDir != "" {
		viewGuard.WritableViews[l.OutputDir] = func(rel string) bool {
			if allowed, e := OutputWriteAllowed(l.OutputOwnershipRegistry, l.TaskID, filepath.Join(l.OutputDir, filepath.FromSlash(rel))); e != nil || !allowed {
				return false
			}
			for _, excluded := range l.ExcludedOutputDirs {
				if physicalWithin(excluded, filepath.Join(l.OutputDir, filepath.FromSlash(rel))) {
					return false
				}
			}
			return len(l.WriteScopes) == 0 || workflow.AllowsWriteScopes(rel, l.WriteScopes)
		}
	}
	if l.StagingWrite && l.StagingDir != "" {
		viewGuard.WritableViews[l.StagingDir] = func(string) bool { return true }
	}
	viewGuard.ReadRoots = map[string]ReadRootView{}
	for name, rv := range guard.ReadRoots {
		viewGuard.ReadRoots[name] = rv
	}
	for _, entry := range []struct {
		path  string
		write bool
	}{{l.OutputDir, l.ArtifactsOnly}, {l.StagingDir, l.StagingWrite}} {
		if entry.path == "" {
			continue
		}
		if entry.write {
			mounts = append(mounts, entry.path)
		} else {
			viewGuard.ReadRoots["task-directory-"+filepath.Base(entry.path)] = ReadRootView{Path: entry.path}
		}
	}

	if l.Session == "agentic" {
		configuredView, viewErr := viewGuard.ConfigureSessionView(cmd, l.LogDirectory, mounts)
		if viewErr != nil {
			return local(viewErr)
		}
		sessionView.Store(configuredView)
		defer configuredView.Close()
	}
	var nativeCollectors []*NativeDirectoryCollector
	if active := sessionView.Load(); active != nil && active.ownedPrivate == "" {
		for _, entry := range []struct {
			path  string
			allow func(string) bool
		}{
			{l.OutputDir, func(rel string) bool {
				for _, excluded := range l.ExcludedOutputDirs {
					if physicalWithin(excluded, filepath.Join(l.OutputDir, filepath.FromSlash(rel))) {
						return false
					}
				}
				return l.ArtifactsOnly && (len(l.WriteScopes) == 0 || workflow.AllowsWriteScopes(rel, l.WriteScopes))
			}},
			{l.StagingDir, func(string) bool { return l.StagingWrite }},
		} {
			if entry.path == "" {
				continue
			}
			collector, collectorErr := NewNativeDirectoryCollector(entry.path, l.LogDirectory, entry.allow)
			if collectorErr != nil {
				return local(collectorErr)
			}
			collector.SharedWrites(l.SharedWriteRegistry, l.SessionID)
			collector.OutputOwnership(l.OutputOwnershipRegistry, l.TaskID)
			nativeCollectors = append(nativeCollectors, collector)
			defer collector.Close()
		}
	}
	if e = cmd.Start(); e != nil {
		return notStarted(fmt.Errorf("provider executable did not start (logs %s): %w", dir, e))
	}
	providerLaunched = true
	startTimeout := c.StartTimeout
	if startTimeout == 0 {
		startTimeout = DefaultStartTimeout
	}
	startBound := time.AfterFunc(startTimeout, func() {
		if ack.expire() {
			stopProvider()
		}
	})
	waitErr := cmd.Wait()
	for _, collector := range nativeCollectors {
		if collectorErr := collector.Verify(); collectorErr != nil {
			return &Refusal{Class: agentruntime.RefusedOutput, Err: collectorErr}
		}
	}
	startBound.Stop()
	if scopeErr := sessionView.Load().Collect(); scopeErr != nil {
		return &Refusal{Class: agentruntime.RefusedOutput, Err: scopeErr}
	}
	if errors.Is(waitErr, exec.ErrWaitDelay) {
		// The provider exited with success; only a process it left behind
		// held its output open past the delay. Its exit status stands.
		waitErr = nil
	}
	// Both logs are flushed either way; a flush failure after a clean exit
	// is the bridge's own failure, and stays untyped.
	logErr := errors.Join(eventLog.Flush(), stderrLog.Flush())
	if e = waitErr; e == nil {
		e = logErr
	}
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	cancelled := ctx.Err() != nil && !timedOut
	if wallBudgetExpired.Load() || (l.Session == "agentic" && stream != nil && stream.exhausted()) {
		if l.Session == "agentic" {
			_, _ = VerifyAndRevertScopedWorktree(context.Background(), root, startTree, editableScope, l.ReadOnlyFiles, changeStage, l.WriteScopes, l.Scratch, l.ArtifactsOnly)
		}
		sid := expectedThread
		turns := 0
		if stream != nil {
			if cur := stream.currentSessionID(); cur != "" {
				sid = cur
			}
			turns = stream.turns()
		}
		recordCheckpoint(sid, turns)
		return &Refusal{Class: agentruntime.TurnBudgetExhausted, Err: errors.New("agentic turn_budget exhausted")}
	}
	if cancelled {
		if l.Session == "agentic" {
			_, _ = VerifyAndRevertScopedWorktree(context.Background(), root, startTree, editableScope, l.ReadOnlyFiles, changeStage, l.WriteScopes, l.Scratch, l.ArtifactsOnly)
		}
		sid := expectedThread
		turns := 0
		if stream != nil {
			if cur := stream.currentSessionID(); cur != "" {
				sid = cur
			}
			turns = stream.turns()
		}
		recordCheckpoint(sid, turns)
	}
	if !cancelled && (ack.stoppedUnacknowledged() || !ack.acknowledged()) && (waitErr != nil || logErr == nil && !providerAnswered(answer)) {
		switch {
		case ack.stoppedUnacknowledged():
			return notStarted(fmt.Errorf("provider did not acknowledge the prompt within the start bound of %s (logs %s)", startTimeout, dir))
		case timedOut:
			return notStarted(fmt.Errorf("provider did not acknowledge the prompt before the bridge timeout of %s (logs %s)", c.Timeout, dir))
		case waitErr == nil:
			return notStarted(fmt.Errorf("provider exited without acknowledging the prompt or answering (logs %s)", dir))
		}
		return notStarted(fmt.Errorf("provider exited before acknowledging the prompt (logs %s): %w", dir, e))
	}
	if e != nil {
		if l.Session == "agentic" {
			_, _ = VerifyAndRevertScopedWorktree(context.Background(), root, startTree, editableScope, l.ReadOnlyFiles, changeStage, l.WriteScopes, l.Scratch, l.ArtifactsOnly)
			sid := expectedThread
			turns := 0
			if stream != nil {
				if cur := stream.currentSessionID(); cur != "" {
					sid = cur
				}
				turns = stream.turns()
			}
			recordCheckpoint(sid, turns)
		}
		if timedOut {
			// The bridge timeout is shorter than an outer runtime deadline in
			// some configurations; preserve its structured cause in the same log.
			_, _ = events.Write([]byte("{\"type\":\"error\",\"error\":{\"code\":\"timeout\"}}\n"))
		}
		if waitErr != nil && !cancelled && (stream == nil || stream.failedSurfaceAllowed()) {
			if timedOut {
				return providerFailed(fmt.Errorf("provider timed out after %s: %w", c.Timeout, e))
			}
			return providerFailed(fmt.Errorf("provider exited with a failure after acknowledging the prompt: %w", e))
		}
		return fmt.Errorf("provider execution failed (logs %s): %w", dir, e)
	}
	providerCompleted = true
	var providerThread string
	if stream != nil {
		// Validates the announced tool surface and session before any answer
		// exists, then publishes only the schema-bound result as answer.json.
		// A turn that ended without a usable result is refused output; a
		// session or tool-surface failure stays untyped.
		if providerThread, e = stream.finish(expectedThread, answer); e != nil {
			if l.Session == "agentic" {
				_, _ = VerifyAndRevertScopedWorktree(context.Background(), root, startTree, editableScope, l.ReadOnlyFiles, changeStage, l.WriteScopes, l.Scratch, l.ArtifactsOnly)
				sid := expectedThread
				if cur := stream.currentSessionID(); cur != "" {
					sid = cur
				}
				recordCheckpoint(sid, stream.turns())
			}
			var unusable *claudeUnusableResult
			if errors.As(e, &unusable) {
				return refused(e)
			}
			return fmt.Errorf("provider output refused (logs %s): %w", dir, e)
		}
	}
	if l.Session == "agentic" {
		offending, revErr := VerifyAndRevertScopedWorktree(ctx, root, startTree, editableScope, l.ReadOnlyFiles, changeStage, l.WriteScopes, l.Scratch, l.ArtifactsOnly)
		if revErr != nil {
			return revErr
		}
		if len(offending) > 0 {
			return refused(fmt.Errorf("out-of-scope worktree modifications reverted: %s", strings.Join(offending, ", ")))
		}
	}
	if session != nil {
		if e = events.Sync(); e != nil {
			return e
		}
		if stream == nil {
			providerThread, e = announcedThread(filepath.Join(dir, "events.jsonl"))
			if e != nil {
				return e
			}
		}
		if session.resumed && providerThread != session.state.ThreadID {
			return errors.New("provider resumed a different thread")
		}
		audit, _ := json.Marshal(SessionAudit{Scope: session.state.Scope, ThreadID: providerThread, Resumed: session.resumed, TaskID: l.TaskID, DispatchID: l.DispatchID})
		if e = os.WriteFile(filepath.Join(dir, "provider-session.json"), audit, 0600); e != nil {
			return e
		}
	} else {
		if stream == nil && (l.Session == "agentic" || expectedThread != "") {
			_ = events.Sync()
			providerThread, _ = announcedThread(filepath.Join(dir, "events.jsonl"))
		}
		if providerThread != "" {
			scopeName := l.SessionPolicy
			if scopeName == "" {
				scopeName = "ephemeral"
			}
			resumed := !l.FreshRole && l.ResumeSession != ""
			resumable := l.Session == "agentic" || resumed
			audit, _ := json.Marshal(SessionAudit{Resumable: &resumable, Scope: scopeName, ThreadID: providerThread, Resumed: resumed, TaskID: l.TaskID, DispatchID: l.DispatchID})
			_ = os.WriteFile(filepath.Join(dir, "provider-session.json"), audit, 0600)
		}
	}
	// Every check of the answer runs before any task file is written, so a
	// refused answer leaves the worktree as it was.
	raw, e = regularRead(answer, false)
	if os.IsNotExist(e) && toolbox != nil && len(toolbox.HandoffOutput()) > 0 {
		raw, e = domain.CanonicalJSON(toolbox.HandoffOutput())
	} else {
		if os.IsNotExist(e) {
			return refused(errors.New("provider returned no answer"))
		}
		if e != nil {
			return refused(fmt.Errorf("answer: %w", e))
		}
		raw, e = domain.CanonicalJSON(raw)
	}
	if e != nil {
		return refused(fmt.Errorf("answer: %w", e))
	}
	var response Response
	dec = json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if e = dec.Decode(&response); e != nil {
		if l.Session == "agentic" && workflow.ValidateArtifact(schema, raw) == nil {
			response = Response{Output: raw}
		} else {
			return refused(fmt.Errorf("answer: %w", e))
		}
	}
	if e = workflow.ValidateArtifact(schema, response.Output); e != nil {
		return refused(e)
	}
	if e = validateEdits(c, l, stage, response); e != nil {
		return refused(e)
	}
	contents, e := resolveEdits(response.Edits, original, exists)
	if e != nil {
		return refused(e)
	}
	if l.Session != "agentic" {
		// Recheck every declared file before any write; model has no filesystem tools.
		for name, before := range original {
			path, e := safePath(root, name)
			if e != nil {
				return e
			}
			now, e := regularRead(path, true)
			if e != nil {
				return e
			}
			if !bytes.Equal(before, now) {
				return errors.New("worktree changed during provider turn")
			}
		}
	} else {
		turns := 0
		if stream != nil {
			turns = stream.turns()
		}
		recordCheckpoint(providerThread, turns)
	}
	// Reserve the handoff before edits too: another writer appearing during
	// inference must not turn an otherwise refused collection into task edits.
	f, e := os.OpenFile(l.Handoff, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	committed := false
	defer func() {
		_ = f.Close()
		if !committed {
			_ = os.Remove(l.Handoff)
		}
	}()
	// The edits and the handoff are written all or nothing (applyEdits): a
	// failed write restores the worktree and types the turn EditsNotApplied.
	// A new file may start a new package directory.
	e = applyEdits(root, response.Edits, contents, original, func() error {
		var checkpointTree string
		turns := 0
		if stream != nil {
			turns = stream.turns()
		}
		if l.Session == "agentic" {
			checkpointTree, _ = SnapshotScopedTree(context.Background(), root, startTree, editableScope, l.Scratch)
			recordCheckpoint(providerThread, turns)
		}
		handoff, _ := json.Marshal(agentruntime.Handoff{
			DispatchID:            l.DispatchID,
			SessionID:             l.SessionID,
			TaskID:                l.TaskID,
			Output:                response.Output,
			AssistantTurns:        turns,
			ProviderSession:       providerThread,
			CheckpointTree:        checkpointTree,
			ResumedFromCheckpoint: !l.FreshRole && (l.ResumeSession != "" || l.ResumeTree != ""),
		})
		_, e := f.Write(handoff)
		return errors.Join(e, f.Close())
	})
	committed = e == nil
	if committed && session != nil {
		return session.complete(providerThread)
	}
	return e
}

// Keep a bounded prefix while draining the subprocess completely. Discarded
// bytes count as consumed so logging limits never block or fail a model turn.
const maxProviderLogBytes int64 = 1 << 20

type boundedLog struct {
	destination io.Writer
	remaining   int64
}

func (w *boundedLog) Write(p []byte) (int, error) {
	original := len(p)
	if int64(len(p)) > w.remaining {
		p = p[:w.remaining]
	}
	if len(p) > 0 {
		n, e := w.destination.Write(p)
		w.remaining -= int64(n)
		if e != nil {
			return n, e
		}
		if n != len(p) {
			return n, io.ErrShortWrite
		}
	}
	return original, nil
}

// outputProtocol is the bridge's instruction text, added to the kernel
// contract. It is not part of the hashed workflow.
func outputProtocol(toolRule string) string {
	return "Produce the requested role output plus proposed file edits. " + toolRule + " " + outputProtocolRules
}

const outputProtocolRules = "Blocks: task_scope lists the task's files and its RED test_file once a RED report names one. The files block holds the current content of every file this turn may read and edit (a missing file is empty). repository lists the worktree's tracked paths, sorted; truncated=true means the list was cut. engine_feedback, when present, holds excerpts of the engine's own output from the failed runs that sent this task back. " +
	"Edit only in edit-capable test-first or implement turns, and only files in the files block; return edits=[] for every reviewing or planning turn. Each edit has path, content and replacements and uses exactly one form: whole-file content with replacements=[], or content=null with replacements=[{old,new}], where each old must occur exactly once in the file after the edit's earlier replacements. Prefer replacements for large files; only whole-file content creates a file. " +
	"test-first: edit exactly one Go test file (*_test.go), the test_file once set. The RED test must compile and fail at run time on the existing production code, with the declared expected_failure text in its output; a compile failure executes no test and fails the RED gate. The RED is one top-level test function without t.Run subtests: the RED gate needs exactly one executed test, and each subtest counts as one, so a RedReport whose test calls t.Run is refused. Its test name must be unique across the repository: the engine runs it as -run '^Name$' ./... over every package. The test must report its own failure (t.Error, t.Fatal or a panic in the test's own goroutine): a test whose test binary ends under it, by os.Exit or log.Fatal, a panic in another goroutine or a signal, reports no result and does not count as executed. A RED should fail, not hang: a hanging test fails only when go test's own -timeout, half the test stage's deadline, fires minutes later; a test that waits should bound the wait itself and fail with its own message. " +
	"expected_failure is matched literally against that test's own output, without the goroutine stacks in it and without go test's === RUN and --- FAIL: lines, and must say why the test fails: quote the assertion's message or values (got 0 want 42, expected error), a panic's message (assignment to entry in nil map) or, for a test that hangs, the timeout (test timed out). Quote the failure itself: text the test logs before it fails would match a failure for any other reason too. Unless the operator pinned the task's RED identity, a RedReport is refused, costing an attempt, when its expected_failure holds nothing beyond the test's name or part of it, the test file's name, a file:line position or part of one (go:9), go test's framing (=== RUN, --- FAIL:, a duration, exit status N), a panic's generic text (runtime error, goroutine N, stack frames and source positions), these words: " + RedReasonTraceWords + ", or numbers alone (a number counts beside a word or a comparison, as in 0 != 42). Any other word makes it a reason: expected error is one, error alone is not. " +
	"implement: edit any file in scope except the frozen test_file. " +
	"A spec or plan names every file the task needs in declared_files, chosen from the repository listing. Do not claim tests or Git effects; the engine runs them."

// RedReasonTraceWords are the words the output protocol states a derived
// RED's expected_failure may not consist of: the words go test and the Go
// runtime print around any failing test whatever its reason, and an
// assertion's bare labels. The daemon's reason rule refuses exactly these
// (daemon liveRedTraceWords, held to this list by its tests).
const RedReasonTraceWords = "fail, failed, pass, passed, ok, run, running, skip, skipped, pause, cont, name, bench, exit, status, test, tests, panic, panicked, recovered, repanicked, goroutine, runtime, testing, trunner, func, created, by, in, t, go, src, usr, local, got, want, expected, actual"

// assemblePrompt orders the provider prompt as FAE-WF-043 composes a brief:
// the task header (FAE-WF-045), the role instructions, the stage template with
// its bound artifacts, the task scope and engine feedback, then the repository
// listing (when given) and the task's files as quoted data blocks, the
// operator context, and last the kernel contract, completed with the absolute
// handoff path and the bridge's output protocol. Every executor contract field
// is kept verbatim. Task text only ever appears as quoted JSON data.
func assemblePrompt(l agentruntime.BridgeConfig, root string, files map[string]string, repository json.RawMessage, protocol string) ([]byte, error) {
	if !filepath.IsAbs(l.Handoff) {
		return nil, errors.New("absolute handoff path required")
	}
	var blocks []json.RawMessage
	if e := json.Unmarshal(l.Brief, &blocks); e != nil {
		return nil, e
	}
	header, _ := json.Marshal(map[string]any{"kind": "task_header", "task_id": l.TaskID, "dispatch_id": l.DispatchID, "worktree": root, "context_policy": "This task header and supplied files are the complete current task scope. Do not reuse previous task working files or treat prior outputs as current evidence."})
	filesBlock, _ := json.Marshal(map[string]any{"kind": "files", "files": files})
	data := []json.RawMessage{filesBlock}
	if repository != nil {
		data = []json.RawMessage{repository, filesBlock}
	}
	out := []json.RawMessage{header}
	var contract map[string]json.RawMessage
	for _, b := range blocks {
		var kind struct{ Kind string }
		if e := json.Unmarshal(b, &kind); e != nil {
			return nil, e
		}
		if kind.Kind == "kernel_contract" {
			if contract != nil || json.Unmarshal(b, &contract) != nil {
				return nil, errors.New("exactly one kernel contract object required")
			}
			continue
		}
		if (kind.Kind == "notifications" || kind.Kind == "operator_context") && data != nil {
			out, data = append(out, data...), nil
		}
		out = append(out, b)
	}
	if contract == nil {
		return nil, errors.New("output schema missing")
	}
	if data != nil {
		out = append(out, data...)
	}
	contract["handoff_path"], _ = json.Marshal(l.Handoff)
	contract["output_protocol"], _ = json.Marshal(protocol)
	last, _ := json.Marshal(contract)
	return json.Marshal(append(out, last))
}

// briefScope returns the daemon-approved scope-widen files the brief's stage
// block carries (FAE-WF-078) that the declared file list lacks.
func briefScope(raw json.RawMessage, declared []string) []string {
	var blocks []struct {
		Kind  string   `json:"kind"`
		Scope []string `json:"approved_scope"`
	}
	_ = json.Unmarshal(raw, &blocks)
	var out []string
	for _, b := range blocks {
		for _, name := range b.Scope {
			if b.Kind == "stage" && !slices.Contains(declared, name) && !slices.Contains(out, name) {
				out = append(out, name)
			}
		}
	}
	return out
}
