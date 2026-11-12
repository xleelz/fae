package provider

import (
	"encoding/json"
	"fmt"
	"github.com/xleelz/fae/scratchfs"
	"github.com/xleelz/fae/workflow"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// NativeDirectoryCollector keeps restoration bytes on disk, never in daemon
// memory. It is used when SEC012 permits the CLI/collector launch path.
type NativeDirectoryCollector struct {
	changed                 []string
	digest                  string
	ownershipRegistry, task string
	registry, owner         string
	source, backup          string
	cleanupRoot             string
	before                  DirectorySnapshot
	allow                   func(string) bool
}

func NewNativeDirectoryCollector(source, logDir string, allow func(string) bool) (*NativeDirectoryCollector, error) {
	if source == "" {
		return nil, nil
	}
	if err := PhysicalTaskDirectory(source); err != nil {
		return nil, err
	}
	backup, err := os.MkdirTemp(logDir, "directory-check-")
	if err != nil {
		return nil, err
	}
	if err = scratchfs.RegisterSessionCopy(backup, logDir); err != nil {
		_ = scratchfs.RemoveSessionCopy(backup)
		return nil, err
	}
	c := &NativeDirectoryCollector{source: source, backup: filepath.Join(backup, "content"), cleanupRoot: backup, allow: allow}
	if err = copySessionTree(source, c.backup, func(string) bool { return true }); err != nil {
		c.Close()
		return nil, err
	}
	c.before, err = snapshotViewDirectory(source)
	if err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}
func (c *NativeDirectoryCollector) Close() error {
	if c == nil || c.backup == "" {
		return nil
	}
	path := c.cleanupRoot
	c.cleanupRoot = ""
	c.backup = ""
	return scratchfs.RemoveSessionCopy(path)
}
func (c *NativeDirectoryCollector) Verify() error {
	if c == nil {
		return nil
	}
	unlock, lockErr := LockOutputOwnership(c.ownershipRegistry)
	if lockErr != nil {
		return lockErr
	}
	defer unlock()
	var registry *workflow.SharedWriteRegistry
	if c.registry != "" {
		file, e := os.Open(c.registry)
		if e != nil {
			return fmt.Errorf("shared write registry unavailable: %w", e)
		}
		raw, e := io.ReadAll(io.LimitReader(file, (1<<20)+1))
		file.Close()
		if e != nil {
			return e
		}
		if len(raw) > 1<<20 {
			return fmt.Errorf("shared write registry exceeds bound")
		}
		var decoded workflow.SharedWriteRegistry
		if e = json.Unmarshal(raw, &decoded); e != nil {
			return e
		}
		found := false
		for _, grant := range decoded.Grants {
			if grant.Active && (grant.Session == c.owner || grant.Dispatch == c.owner) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("collector has no active shared-write identity")
		}
		registry = &decoded
	}
	after, err := snapshotViewDirectory(c.source)
	if err != nil {
		return err
	}
	names := map[string]bool{}
	for name := range c.before {
		names[name] = true
	}
	for name := range after {
		names[name] = true
	}
	ordered := []string{}
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	refused := []string{}
	ownNames := []string{}
	for _, name := range ordered {
		a, aok := c.before[name]
		b, bok := after[name]
		if aok == bok && a.mode == b.mode && viewFileDigest(a) == viewFileDigest(b) {
			continue
		}
		if registry != nil && workflow.ForeignSharedWrite(*registry, c.owner, filepath.Join(c.source, name)) {
			continue
		}
		owned, ownershipErr := OutputWriteAllowed(c.ownershipRegistry, c.task, filepath.Join(c.source, name))
		if ownershipErr != nil {
			return ownershipErr
		}
		if owned && c.allow != nil && c.allow(filepath.ToSlash(name)) {
			ownNames = append(ownNames, name)
			continue
		}
		refused = append(refused, filepath.ToSlash(name))
		target, err := safePath(c.source, name)
		if err != nil {
			candidate := filepath.Join(c.source, name)
			info, statErr := os.Lstat(candidate)
			if statErr != nil || info.Mode()&os.ModeSymlink == 0 {
				return err
			}
			if parentErr := PhysicalTaskDirectory(filepath.Dir(candidate)); parentErr != nil {
				return parentErr
			}
			if removeErr := os.Remove(candidate); removeErr != nil {
				return removeErr
			}
			target = candidate
		}
		if !aok {
			if err = os.Remove(target); err != nil && !os.IsNotExist(err) {
				return err
			}
			continue
		}
		if err = EnsurePhysicalTaskDirectory(filepath.Dir(target)); err != nil {
			return err
		}
		if a.mode&os.ModeSymlink != 0 {
			if err = os.Remove(target); err != nil && !os.IsNotExist(err) {
				return err
			}
			link := string(a.data)
			if err = os.Symlink(link, target); err != nil {
				return err
			}
		} else {
			if err = copyCollectedFile(filepath.Join(c.backup, name), target, a.mode, viewFileDigest(a)); err != nil {
				return err
			}
		}
	}
	if len(refused) > 0 {
		return fmt.Errorf("task directory writes outside effective scope: %v", refused)
	}
	c.changed, c.digest, err = digestViewChanges(c.source, c.before, after, ownNames)
	return err
}

func (c *NativeDirectoryCollector) SharedWrites(registry, owner string) {
	if c != nil {
		c.registry, c.owner = registry, owner
	}
}

func (c *NativeDirectoryCollector) OutputOwnership(registry, task string) {
	if c != nil {
		c.ownershipRegistry, c.task = registry, task
	}
}

func (c *NativeDirectoryCollector) DirectoryEvidence(path string) ([]string, string, bool) {
	if c != nil && filepath.Clean(path) == filepath.Clean(c.source) {
		return append([]string{}, c.changed...), c.digest, true
	}
	return nil, "", false
}
