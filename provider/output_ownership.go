package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"syscall"
)

func descendantOutputOwnership(registry, task string) ([]string, error) {
	if registry == "" {
		return nil, nil
	}
	file, err := os.Open(registry)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, (16<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > 16<<20 {
		return nil, fmt.Errorf("output ownership registry exceeds bound")
	}
	var document struct {
		Version int `json:"version"`
		Owners  []struct {
			Task      string   `json:"task"`
			OutputDir string   `json:"output_dir"`
			Ancestors []string `json:"ancestors"`
		} `json:"owners"`
	}
	if err = json.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	if document.Version != 1 {
		return nil, fmt.Errorf("unsupported output ownership registry")
	}
	found := false
	excluded := []string{}
	for _, owner := range document.Owners {
		if owner.Task == task {
			found = true
		}
		if slices.Contains(owner.Ancestors, task) {
			if !filepath.IsAbs(owner.OutputDir) {
				return nil, fmt.Errorf("invalid descendant output path")
			}
			excluded = append(excluded, owner.OutputDir)
		}
	}
	if !found {
		return nil, fmt.Errorf("task missing from output ownership registry")
	}
	return excluded, nil
}
func OutputWriteAllowed(registry, task, absolute string) (bool, error) {
	dirs, err := descendantOutputOwnership(registry, task)
	if err != nil {
		return false, err
	}
	for _, dir := range dirs {
		if physicalWithin(dir, filepath.Clean(absolute)) {
			return false, nil
		}
	}
	return true, nil
}

// LockOutputOwnership pins the ownership epoch across source comparison and
// collection. Admission publishes under the same file's exclusive lock.
func LockOutputOwnership(registry string) (func(), error) {
	if registry == "" {
		return func() {}, nil
	}
	file, err := os.OpenFile(registry+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_SH); err != nil {
		file.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN); _ = file.Close() }, nil
}
