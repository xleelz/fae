package provider

import (
	"bytes"
	"encoding/json"
	"github.com/xleelz/fae/commandshim"
	"github.com/xleelz/fae/workflow"
	"path/filepath"
	"slices"
	"strings"
)

type claudeToolBlock struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	Input struct {
		FilePath string `json:"file_path"`
		Path     string `json:"path"`
		Command  string `json:"command"`
	} `json:"input"`
}

// Native announcements use the same permission predicates as the tool surface
// and shim hook. No native call is executed by the event collector.
func (s *claudeStream) recordNativeTools(blocks []claudeToolBlock) {
	if s.toolbox == nil {
		return
	}
	t := s.toolbox
	for _, b := range blocks {
		if b.Type != "tool_use" {
			continue
		}
		if b.ID != "" {
			if s.nativeToolIDs == nil {
				s.nativeToolIDs = map[string]bool{}
			}
			if s.nativeToolIDs[b.ID] {
				continue
			}
			if len(s.nativeToolIDs) < 10000 {
				s.nativeToolIDs[b.ID] = true
			}
		}
		path := b.Input.FilePath
		if path == "" {
			path = b.Input.Path
		}
		call := AgenticToolCall{Tool: b.Name, Path: path}
		capability := normalizeToolCapability(b.Name)
		allowed := !s.budgetExhausted && s.toolAllowed(b.Name) && slices.Contains(t.AllowedTools(), capability)
		rel := path
		if filepath.IsAbs(path) {
			if r, e := filepath.Rel(t.Worktree, path); e == nil {
				rel = filepath.ToSlash(r)
			} else {
				allowed = false
			}
		}
		if allowed && path != "" {
			switch capability {
			case "read", "list", "search_regex":
				if t.Guard != nil {
					_, _, _, err := t.Guard.resolveReadTarget(path)
					allowed = err == nil
				}
			case "edit", "write":
				_, matched, err := t.taskDirectoryPath(path, true)
				if matched || err != nil {
					allowed = err == nil
				} else {
					_, err = t.checkWritablePath(rel)
					allowed = err == nil
				}
			}
		}
		if allowed && capability == "shell" {
			_, err := workflow.CheckShellCommand(t.Profile, b.Input.Command, t.DeniedTestCommands)
			allowed = err == nil
			if allowed && t.ClaudeSettings != "" {
				raw, _ := json.Marshal(map[string]any{"tool_name": "Bash", "tool_input": map[string]string{"command": b.Input.Command}})
				decision, err := commandshim.PreToolDecision(bytes.NewReader(raw), filepath.Join(filepath.Dir(t.ClaudeSettings), "command-shims"))
				allowed = err == nil && !strings.Contains(string(decision), `"permissionDecision":"deny"`)
			}
		}
		t.recordToolEvent(call, allowed)
	}
}
