package daemon

import (
	"errors"
	"path/filepath"
	"strings"
)

// LiveTaskConfig pins each task's declared scope and genuine RED identity.
// Overrides are part of the instance configuration hash, never model supplied.
type LiveTaskConfig struct {
	Files           []string `json:"files"`
	TestFile        string   `json:"test_file"`
	TestID          string   `json:"test_id"`
	ExpectedFailure string   `json:"expected_failure"`
}

func validateLiveTasks(tasks map[string]LiveTaskConfig) error {
	for id, task := range tasks {
		if id == "" || len(task.Files) == 0 || task.TestFile == "" || task.TestID == "" || task.ExpectedFailure == "" {
			return errors.New("task override requires ID, files and complete RED identity")
		}
		seen := map[string]bool{}
		for _, name := range task.Files {
			if name == "." || name == ".." || filepath.IsAbs(name) || filepath.Clean(name) != name || strings.HasPrefix(name, "../") || name == ".git" || strings.HasPrefix(name, ".git/") || seen[name] {
				return errors.New("task override has unsafe or duplicate file")
			}
			seen[name] = true
		}
		if !seen[task.TestFile] {
			return errors.New("task RED file must be declared")
		}
	}
	return nil
}

func (x *liveExecutor) forTask(id string) *liveExecutor {
	task, ok := x.config.Tasks[id]
	if !ok {
		return x
	}
	selected := *x
	selected.config.Files = append([]string(nil), task.Files...)
	selected.config.TestFile = task.TestFile
	selected.config.TestID = task.TestID
	selected.config.ExpectedFailure = task.ExpectedFailure
	return &selected
}
