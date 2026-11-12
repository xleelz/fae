package engine

import (
	"fmt"
	"github.com/xleelz/fae/workflow"
	"path/filepath"
	"sort"
	"strings"
)

func scenarioDirectories(j *Job, parent *Job, scratch string) error {
	if !filepath.IsAbs(scratch) || j.TaskID == "." || strings.ContainsAny(j.TaskID, "/\\") {
		return fmt.Errorf("scenario requires an absolute scratch root and a path-safe task id")
	}
	if parent == nil {
		j.OutputDir = filepath.Join(scratch, j.TaskID, "out")
	} else {
		j.OutputDir = filepath.Join(parent.OutputDir, j.TaskID)
	}
	if j.Document.Staging {
		j.StagingDir = filepath.Join(scratch, j.TaskID, "staging")
	}
	return nil
}

func scenarioIncoming(jobs map[string]Job, target string) []AncestorChangeRequest {
	byID := map[string]AncestorChangeRequest{}
	for _, job := range jobs {
		for _, request := range job.AncestorRequests {
			if request.Target == target {
				byID[request.ID] = request
			}
		}
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]AncestorChangeRequest, 0, len(ids))
	for _, id := range ids {
		out = append(out, byID[id])
	}
	return out
}

func declaredRequestArtifact(d workflow.Document, name string) bool {
	if _, ok := workflow.ArtifactDeclaration(d, name); ok {
		return true
	}
	if d.Signature != nil {
		if _, ok := d.Signature.Inputs[name]; ok {
			return true
		}
		if _, ok := d.Signature.Outputs[name]; ok {
			return true
		}
	}
	return false
}
