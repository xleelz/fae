package provider

import (
	"encoding/json"
	"github.com/xleelz/fae/domain"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Tool metadata is deliberately independent of arguments, file contents and
// returned output. Allowed calls are bounded; refusals bypass the rate limit.
func (t *AgenticToolbox) recordToolEvent(call AgenticToolCall, allowed bool) {
	if t.ToolAuditPath == "" {
		return
	}
	t.toolEventMu.Lock()
	defer t.toolEventMu.Unlock()
	now := time.Now().UTC()
	if now.Sub(t.toolEventWindow) >= time.Second {
		t.toolEventWindow = now
		t.toolEventRate = 0
	}
	if allowed && (t.toolEventCount >= 200 || t.toolEventRate >= 20) {
		return
	}
	if allowed {
		t.toolEventCount++
		t.toolEventRate++
	}
	path := call.Path
	if filepath.IsAbs(path) {
		rel, err := filepath.Rel(t.Worktree, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			path = "<outside-worktree>"
			if t.Guard != nil {
				best := ""
				for name, root := range t.Guard.ReadRoots {
					relative, e := filepath.Rel(root.Path, call.Path)
					if e == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && len(root.Path) > len(best) {
						best = root.Path
						path = name + ":" + filepath.ToSlash(relative)
					}
				}
			}
		} else {
			path = filepath.ToSlash(rel)
		}
	}
	if len(path) > 1024 {
		path = path[:1024]
	}
	if strings.ContainsAny(path, "\n\r\x00") {
		path = "<invalid-path>"
	}
	paths := []string{}
	if path != "" {
		paths = append(paths, path)
	}
	event := domain.ToolEvent{Tool: normalizeToolCapability(call.Tool), Paths: paths, Allowed: allowed, At: now}
	f, err := os.OpenFile(t.ToolAuditPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	_ = json.NewEncoder(f).Encode(event)
}
