package runtime

import (
	"encoding/json"
	"github.com/xleelz/fae/workflow"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ConfigureClaudeTaskSettings grants the installed CLI native tool vocabulary;
// the pre-tool hook and collection diff independently enforce these bounds.
func ConfigureClaudeTaskSettings(file string, l Launch) error {
	raw, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err = json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	allow := []string{}
	readPatterns := []string{"**"}
	if l.Reads != nil {
		if l.Reads.Allow != nil {
			readPatterns = slices.Clone(l.Reads.Allow)
		}
		for _, layer := range l.Reads.AllowSets {
			if readPatterns == nil {
				readPatterns = slices.Clone(layer)
				continue
			}
			narrow := []string{}
			for _, candidate := range append(slices.Clone(readPatterns), layer...) {
				if workflow.GlobScopeNarrows([]string{candidate}, readPatterns) && workflow.GlobScopeNarrows([]string{candidate}, layer) && !slices.Contains(narrow, candidate) {
					narrow = append(narrow, candidate)
				}
			}
			readPatterns = narrow
		}
	}
	for _, pattern := range readPatterns {
		allow = append(allow, "Read("+filepath.ToSlash(filepath.Join(l.Worktree, pattern))+")")
	}

	for _, dir := range []string{l.OutputDir, l.StagingDir} {
		if dir != "" {
			allow = append(allow, "Read("+filepath.ToSlash(dir)+"/**)")
		}
	}
	deny := []string{}
	for _, excluded := range l.ExcludedOutputDirs {
		deny = append(deny, "Write("+filepath.ToSlash(excluded)+"/**)", "Edit("+filepath.ToSlash(excluded)+"/**)")
	}
	denyReads := slices.Clone(l.DenyPaths)
	if l.Reads != nil {
		denyReads = append(denyReads, l.Reads.Deny...)
	}
	for _, pattern := range denyReads {
		for _, tool := range []string{"Read", "Glob", "Grep"} {
			deny = append(deny, tool+"("+filepath.ToSlash(filepath.Join(l.Worktree, pattern))+")")
		}
	}

	for _, pattern := range []string{".git/**", ".claude/**", ".codex/**", ".cursor/**", ".mcp.json"} {
		for _, tool := range []string{"Write", "Edit"} {
			deny = append(deny, tool+"("+filepath.ToSlash(filepath.Join(l.Worktree, pattern))+")")
		}
	}

	for _, root := range append(slices.Clone(l.ReadRootPaths), l.NativeReadDirs...) {
		if root != "" {
			allow = append(allow, "Read("+filepath.ToSlash(root)+"/**)")
		}
	}
	for _, root := range l.ReadRootPaths {
		if root != "" {
			deny = append(deny, "Write("+filepath.ToSlash(root)+"/**)", "Edit("+filepath.ToSlash(root)+"/**)")
		}
	}
	patterns := []string{}
	for _, layer := range l.WriteScopes {
		for _, candidate := range layer {
			valid := true
			for _, bound := range l.WriteScopes {
				if !workflow.GlobScopeNarrows([]string{candidate}, bound) {
					valid = false
				}
			}
			if valid && !slices.Contains(patterns, candidate) {
				patterns = append(patterns, candidate)
			}
		}
	}
	if l.ChangeProducer && len(l.WriteScopes) == 0 {
		patterns = []string{"**"}
	}
	outputPatterns := slices.Clone(patterns)
	if l.ArtifactsOnly {
		patterns = nil
	}
	for _, p := range patterns {
		allow = append(allow, "Write("+filepath.ToSlash(filepath.Join(l.Worktree, p))+")", "Edit("+filepath.ToSlash(filepath.Join(l.Worktree, p))+")")
	}
	scratch := l.Scratch
	if l.ArtifactsOnly {
		scratch = nil
	}
	for _, p := range scratch {
		allow = append(allow, "Write("+filepath.ToSlash(filepath.Join(l.Worktree, p))+")", "Edit("+filepath.ToSlash(filepath.Join(l.Worktree, p))+")")
	}
	for _, dir := range []struct {
		path  string
		write bool
	}{{l.OutputDir, l.ArtifactsOnly}, {l.StagingDir, l.StagingWrite}} {
		if dir.path == l.OutputDir && l.ArtifactsOnly && len(l.WriteScopes) > 0 {
			for _, pattern := range outputPatterns {
				allow = append(allow, "Write("+filepath.ToSlash(filepath.Join(dir.path, pattern))+")", "Edit("+filepath.ToSlash(filepath.Join(dir.path, pattern))+")")
			}
			continue
		}
		if dir.path != "" && dir.write {
			allow = append(allow, "Write("+filepath.ToSlash(dir.path)+"/**)", "Edit("+filepath.ToSlash(dir.path)+"/**)")
		}
	}
	if AgenticShellAllowed(l.Profile) {
		if l.Profile.ShellAllow == nil {
			allow = append(allow, "Bash")
		} else {
			for _, shape := range l.Profile.ShellAllow {
				allow = append(allow, "Bash("+strings.Join(shape, " ")+")")
			}
		}
	}
	// Claude's absolute-path permission prefix is //, not a single slash.
	for _, rules := range [][]string{allow, deny} {
		for i, rule := range rules {
			if at := strings.Index(rule, "(/"); at >= 0 {
				rules[i] = rule[:at+1] + "/" + rule[at+1:]
			}
		}
	}
	doc["permissions"] = map[string]any{"allow": allow, "deny": deny}
	raw, err = json.Marshal(doc)
	if err != nil {
		return err
	}
	return os.WriteFile(file, raw, 0600)
}
