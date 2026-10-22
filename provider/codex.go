// Package provider supplies a restricted real Codex CLI bridge. Model processes
// are read-only and command tools are disabled; only validated proposed edits
// are applied by this trusted bridge after the model has exited.
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
	"strings"
	"time"

	"github.com/xleelz/fae/domain"
	agentruntime "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
)

type TaskConfig struct {
	Files    []string `json:"files"`
	TestFile string   `json:"test_file"`
}

type Config struct {
	SessionRoot    string
	TaskScopesJSON string
	Binary         string
	Files          []string
	TestFile       string
	Timeout        time.Duration
}
type Edit struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}
type Response struct {
	Output json.RawMessage `json:"output"`
	Edits  []Edit          `json:"edits"`
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
	canEdit := false
	for _, p := range l.Profile.Allowed {
		switch p {
		case "edit":
			canEdit = true
		case "handoff", "read":
		default:
			return fmt.Errorf("unsupported bridge capability %s", p)
		}
	}
	if l.Profile.Build || l.Profile.Test || l.Profile.Format {
		return errors.New("provider cannot execute host commands; use engine broker")
	}
	for _, p := range l.Profile.Denied {
		if p == "edit" {
			canEdit = false
		}
	}
	seen := map[string]bool{}
	for _, edit := range r.Edits {
		if !canEdit || !allowed[edit.Path] || seen[edit.Path] || len(edit.Content) > 1<<20 {
			return errors.New("unauthorized edit")
		}
		if stage == "test-first" && edit.Path != c.TestFile {
			return errors.New("RED may edit only pinned test file")
		}
		if stage == "implement" && edit.Path == c.TestFile {
			return errors.New("implementation cannot edit frozen test")
		}
		if stage != "test-first" && stage != "implement" {
			return errors.New("stage may not edit files")
		}
		if _, e := safePath(l.Worktree, edit.Path); e != nil {
			return e
		}
		seen[edit.Path] = true
	}
	return nil
}
func Run(ctx context.Context, c Config, configPath string) error {
	raw, e := regularRead(configPath, false)
	if e != nil {
		return e
	}
	raw, e = domain.CanonicalJSON(raw)
	if e != nil {
		return e
	}
	var l agentruntime.BridgeConfig
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if e = dec.Decode(&l); e != nil {
		return e
	}
	isProphet := l.WorkflowID == "fae-prophet"
	if isProphet {
		c.SessionRoot = ""
		c.Files = nil
		c.TaskScopesJSON = ""
		if !l.FreshContext || l.Role != "prophet" {
			return errors.New("Prophet requires isolated fresh context")
		}
		for _, capability := range l.Profile.Allowed {
			if capability != "handoff" {
				return errors.New("Prophet cannot access tools or files")
			}
		}
	}
	isLoop := strings.HasPrefix(l.WorkflowID, "fae-loop:")
	if isLoop {
		if c.SessionRoot == "" || l.SessionPolicy != "task-bound" || l.FreshContext {
			return errors.New("provider loop requires persistent session root and task-bound context")
		}
		for _, capability := range l.Profile.Allowed {
			if capability != "read" && capability != "handoff" {
				return errors.New("provider loop is read-only")
			}
		}
		if l.Profile.Build || l.Profile.Test || l.Profile.Format {
			return errors.New("provider loop cannot execute commands")
		}
	}
	if c.TaskScopesJSON != "" && !isLoop {
		if len(c.TaskScopesJSON) > 1<<20 {
			return errors.New("provider scope configuration too large")
		}
		raw, e := domain.CanonicalJSON([]byte(c.TaskScopesJSON))
		if e != nil {
			return e
		}
		var tasks map[string]TaskConfig
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if e = dec.Decode(&tasks); e != nil {
			return e
		}
		task, ok := tasks[l.TaskID]
		if !ok || len(task.Files) == 0 || task.TestFile == "" {
			return errors.New("pinned provider task scope required")
		}
		c.Files = task.Files
		c.TestFile = task.TestFile
	}
	if l.Protocol != "fae-agent-v1" || l.Binding.Provider != "codex" || l.Binding.Model == "" || !filepath.IsAbs(c.Binary) || c.Timeout <= 0 {
		return errors.New("codex bridge binding/configuration required")
	}
	// Refuse stale evidence before launching a provider or changing task files.
	if _, e := os.Lstat(l.Handoff); e == nil {
		return errors.New("handoff already exists")
	} else if !os.IsNotExist(e) {
		return e
	}
	root, e := filepath.EvalSymlinks(l.Worktree)
	if e != nil || root != l.Worktree {
		return errors.New("canonical worktree required")
	}
	schema, stage, e := parseBrief(l.Brief)
	if e != nil {
		return e
	}
	if e = validateEdits(c, l, stage, Response{}); e != nil {
		return e
	}
	files := map[string]string{}
	original := map[string][]byte{}
	for _, name := range c.Files {
		path, e := safePath(root, name)
		if e != nil {
			return e
		}
		data, e := regularRead(path, true)
		if e != nil {
			return e
		}
		files[name] = string(data)
		original[name] = data
	}
	prompt, _ := json.Marshal(map[string]any{"task_header": map[string]string{"task_id": l.TaskID, "dispatch_id": l.DispatchID, "worktree": root, "context_policy": "This task header and supplied files are the complete current task scope. Do not reuse previous task working files or treat prior outputs as current evidence."}, "brief": json.RawMessage(l.Brief), "files": files, "instructions": "Produce the requested role output plus proposed whole-file edits. Never use tools. No edits except declared edit-capable test-first/implement turns. RED test must fail on existing production code for the exact requested reason; implementation changes production only. Return edits=[] for all reviewing/planning turns. Do not claim tests or Git effects; the engine runs them."})
	schemaRaw, _ := json.Marshal(map[string]any{"type": "object", "properties": map[string]any{"output": json.RawMessage(schema), "edits": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]string{"type": "string"}, "content": map[string]string{"type": "string"}}, "required": []string{"path", "content"}, "additionalProperties": false}}}, "required": []string{"output", "edits"}, "additionalProperties": false})
	dir, e := os.MkdirTemp(l.LogDirectory, "provider-")
	if e != nil {
		return e
	}
	schemaFile := filepath.Join(dir, "schema.json")
	answer := filepath.Join(dir, "answer.json")
	if e = os.WriteFile(schemaFile, schemaRaw, 0600); e != nil {
		return e
	}
	session, e := openProviderSession(ctx, c.SessionRoot, l)
	if e != nil {
		return e
	}
	defer session.close()
	// Empty private cwd prevents repository-local provider configuration/hooks.
	cwd := filepath.Join(dir, "cwd")
	if session != nil {
		cwd = filepath.Join(session.dir, "cwd")
	} else if e = os.Mkdir(cwd, 0700); e != nil {
		return e
	}
	args := []string{"exec", "--ignore-user-config", "--ignore-rules", "--ephemeral", "--skip-git-repo-check", "--sandbox", "read-only", "--disable", "shell_tool", "--disable", "apps", "--disable", "browser_use", "-c", "web_search=\"disabled\"", "-c", "approval_policy=\"never\"", "--model", l.Binding.Model, "--json", "--output-schema", schemaFile, "--output-last-message", answer, "-"}
	if l.Binding.Effort != "" {
		args = append(args[:len(args)-1], "-c", "model_reasoning_effort="+fmt.Sprintf("%q", l.Binding.Effort), "-")
	}
	if session != nil {
		filtered := args[:0]
		for _, arg := range args {
			if arg != "--ephemeral" {
				filtered = append(filtered, arg)
			}
		}
		args = filtered
		if session.resumed {
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
			args = append(filtered, session.state.ThreadID, "-")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Binary, args...)
	shims, e := commandshim.InstallDenials(dir)
	if e != nil {
		return e
	}
	cmd.Env = commandshim.PrependPath(os.Environ(), shims)
	cmd.Dir = cwd
	cmd.Stdin = bytes.NewReader(prompt)
	events, e := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer events.Close()
	stderr, e := os.OpenFile(filepath.Join(dir, "stderr.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer stderr.Close()
	cmd.Stdout = &boundedLog{destination: events, remaining: maxProviderLogBytes}
	cmd.Stderr = &boundedLog{destination: stderr, remaining: maxProviderLogBytes}
	if e = cmd.Run(); e != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			// The bridge timeout is shorter than an outer runtime deadline in
			// some configurations; preserve its structured cause in the same log.
			_, _ = events.Write([]byte("{\"type\":\"error\",\"error\":{\"code\":\"timeout\"}}\n"))
		}
		return fmt.Errorf("provider execution failed (logs %s): %w", dir, e)
	}
	var providerThread string
	if session != nil {
		if e = events.Sync(); e != nil {
			return e
		}
		providerThread, e = announcedThread(filepath.Join(dir, "events.jsonl"))
		if e != nil {
			return e
		}
		if session.resumed && providerThread != session.state.ThreadID {
			return errors.New("provider resumed a different thread")
		}
		audit, _ := json.Marshal(SessionAudit{Scope: session.state.Scope, ThreadID: providerThread, Resumed: session.resumed, TaskID: l.TaskID, DispatchID: l.DispatchID})
		if e = os.WriteFile(filepath.Join(dir, "provider-session.json"), audit, 0600); e != nil {
			return e
		}
	}
	raw, e = regularRead(answer, false)
	if e != nil {
		return e
	}
	raw, e = domain.CanonicalJSON(raw)
	if e != nil {
		return e
	}
	var response Response
	dec = json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if e = dec.Decode(&response); e != nil {
		return e
	}
	if e = workflow.ValidateArtifact(schema, response.Output); e != nil {
		return e
	}
	if e = validateEdits(c, l, stage, response); e != nil {
		return e
	}
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
	for _, edit := range response.Edits {
		path, _ := safePath(root, edit.Path)
		if e = os.WriteFile(path, []byte(edit.Content), 0600); e != nil {
			return e
		}
	}
	handoff, _ := json.Marshal(agentruntime.Handoff{DispatchID: l.DispatchID, SessionID: l.SessionID, TaskID: l.TaskID, Output: response.Output})
	_, e = f.Write(handoff)
	closeErr := f.Close()
	committed = e == nil && closeErr == nil
	if committed && session != nil {
		return session.complete(providerThread)
	}
	return errors.Join(e, closeErr)
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
