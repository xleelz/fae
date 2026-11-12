package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/execution"
	agentruntime "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/scheduler"
	"github.com/xleelz/fae/workflow"
)

type sharedWriteGrants struct {
	mu    sync.Mutex
	state workflow.SharedWriteRegistry
}

func (a *liveAdmission) acquireSharedWrite(d engine.Dispatch, b execution.Binding, s engine.Session) (func() error, error) {
	if b.SharedWriteRegistry == "" {
		return func() error { return nil }, nil
	}
	role := b.Document.Roles[s.Role]
	if s.Role == "" {
		role = b.Document.Roles[d.Config.Role]
	}
	layers, err := execution.BoundWriteScopes(role, d, b)
	if err != nil {
		return nil, err
	}
	if b.ArtifactsOnly && len(layers) == 0 && agentSession(d.Config.Primitive) {
		for _, tool := range agentruntime.AllowedAgenticTools(workflow.EffectiveProfile(role.Profile, d.Config), true) {
			if tool == "write" || tool == "edit" || tool == "shell" {
				layers = [][]string{{"**"}}
				break
			}
		}
	}
	root := b.Worktree
	if b.ArtifactsOnly {
		root = b.OutputDir
	}
	scopes := []workflow.SharedWriteScope{}
	add := func(root string, layers [][]string) {
		if root != "" && len(layers) > 0 {
			scopes = append(scopes, workflow.SharedWriteScope{Root: filepath.Clean(root), Layers: layers})
		}
	}
	add(root, layers)
	scratch := append(append(append([]string{}, b.Scratch...), role.Scratch...), d.Config.Scratch...)
	if len(scratch) > 0 {
		add(root, [][]string{scratch})
		if root != b.Worktree {
			add(b.Worktree, [][]string{scratch})
		}
	}
	if role.Staging == "write" || d.Config.Staging == "write" {
		add(b.StagingDir, [][]string{{"**"}})
	}
	for _, scope := range scopes {
		if !filepath.IsAbs(scope.Root) {
			return nil, errors.New("shared write root must be absolute")
		}
	}
	grant := workflow.SharedWriteGrant{Session: s.ID, Dispatch: d.ID, Active: true, Scopes: scopes}
	if grant.Session == "" || grant.Dispatch == "" {
		return nil, errors.New("shared write grant requires session and dispatch identity")
	}
	g := &a.sharedWrites
	g.mu.Lock()
	defer g.mu.Unlock()
	active := false
	for _, old := range g.state.Grants {
		if !old.Active {
			continue
		}
		active = true
		if old.Session == grant.Session {
			return nil, errors.New("duplicate shared write grant")
		}
		for _, left := range old.Scopes {
			for _, right := range scopes {
				if workflow.SharedWriteScopesOverlap(left, right) {
					return nil, &scheduler.AdmissionRefusal{Reason: fmt.Sprintf("SharedWriteScope: session %s overlaps %s", s.ID, old.Session)}
				}
			}
		}
	}
	if !active {
		g.state.Epoch++
		g.state.Grants = nil
	}
	if len(g.state.Grants) >= 16384 {
		return nil, &scheduler.AdmissionRefusal{Reason: "SharedWriteHistory: active collection epoch reached bounded history"}
	}
	g.state.Grants = append(g.state.Grants, grant)
	raw, _ := json.Marshal(g.state)
	// Leave one byte per historical grant for Active true -> false changes.
	if len(raw) > (1<<20)-16384 {
		g.state.Grants = g.state.Grants[:len(g.state.Grants)-1]
		return nil, &scheduler.AdmissionRefusal{Reason: "SharedWriteHistory: collection registry reached byte bound"}
	}
	if err = g.write(b.SharedWriteRegistry); err != nil {
		g.state.Grants = g.state.Grants[:len(g.state.Grants)-1]
		return nil, err
	}
	var once sync.Once
	var releaseErr error
	return func() error {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			active := false
			for i := range g.state.Grants {
				if g.state.Grants[i].Session == grant.Session {
					g.state.Grants[i].Active = false
				}
				active = active || g.state.Grants[i].Active
			}
			if !active {
				g.state.Grants = nil
			}
			releaseErr = g.write(b.SharedWriteRegistry)
		})
		return releaseErr
	}, nil
}
func (g *sharedWriteGrants) write(path string) error {
	raw, err := json.Marshal(g.state)
	if len(raw) > 1<<20 {
		return errors.New("shared write registry exceeds collector byte bound")
	}
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".shared-write-")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(raw)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(temp, path)
}
