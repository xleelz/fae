package workflow

import (
	"encoding/json"
	"fmt"
	"github.com/xleelz/fae/domain"
	"strconv"
	"strings"
	"time"
)

// Scenario is shared by offline simulation and the scripted daemon adapter.
// Rules use first-match order; task and branch qualifiers never change stages.
type Scenario struct {
	ScratchRoot string              `json:"scratch_root,omitempty"`
	Seed        int64               `json:"seed"`
	Task        string              `json:"task,omitempty"`
	Inputs      map[string]any      `json:"inputs,omitempty"`
	Workflows   map[string]Document `json:"workflows,omitempty"`
	Rules       []ScenarioRule      `json:"rules"`
}
type ScenarioRule struct {
	Task          string                 `json:"task,omitempty"`
	Stage         string                 `json:"stage"`
	Role          string                 `json:"role,omitempty"`
	TaskGlob      string                 `json:"task_glob,omitempty"`
	BranchIndex   *int                   `json:"branch_index,omitempty"`
	Output        json.RawMessage        `json:"output,omitempty"`
	Reviews       []json.RawMessage      `json:"reviews,omitempty"`
	Failure       string                 `json:"failure,omitempty"`
	Delay         time.Duration          `json:"delay,omitempty"`
	Decision      string                 `json:"decision,omitempty"`
	Items         []ItemDecision         `json:"items,omitempty"`
	Detached      []ScenarioAdmission    `json:"detached,omitempty"`
	ChangeRequest *ScenarioChangeRequest `json:"change_request,omitempty"`
}
type ScenarioAdmission struct {
	Revision    int               `json:"revision,omitempty"`
	Workflow    string            `json:"workflow"`
	Inputs      map[string]any    `json:"inputs,omitempty"`
	FromParent  map[string]string `json:"from_parent,omitempty"`
	MaxSessions int               `json:"max_sessions,omitempty"`
}
type ScenarioChangeRequest struct {
	Target   string `json:"target"`
	Artifact string `json:"artifact"`
	Value    any    `json:"value"`
}
type SimulationBranch struct {
	FanStage string       `json:"fan_stage"`
	Visit    int          `json:"visit"`
	Path     string       `json:"path"`
	ID       string       `json:"id"`
	Parent   string       `json:"parent"`
	State    domain.State `json:"state"`
	Outcome  string       `json:"outcome"`
	Evidence []Evidence   `json:"evidence,omitempty"`
}

type SimulationTask struct {
	IncomingRequests []json.RawMessage    `json:"incoming_change_requests,omitempty"`
	FailureRoutes    []FailureRouteReport `json:"failure_routes,omitempty"`
	Refusals         []RefusalReport      `json:"refusals,omitempty"`
	Workflow         string               `json:"workflow"`
	Revision         int                  `json:"revision"`
	ChangeRequests   []json.RawMessage    `json:"change_requests,omitempty"`
	Branches         []SimulationBranch   `json:"branches,omitempty"`
	Task             string               `json:"task"`
	Parent           string               `json:"parent,omitempty"`
	Root             string               `json:"root"`
	Depth            int                  `json:"depth"`
	Detached         bool                 `json:"detached,omitempty"`
	State            domain.State         `json:"state"`
	Reason           string               `json:"reason,omitempty"`
	Evidence         []Evidence           `json:"evidence"`
	Path             []string             `json:"path"`
	Generated        []string             `json:"generated,omitempty"`
}
type SimulationRefusal struct {
	Parent string `json:"parent"`
	Task   string `json:"task"`
	Reason string `json:"reason"`
}

// ScenarioCatalogRevision interprets id as revision1 and id@N as an exact pin.
func ScenarioCatalogRevision(key string, doc Document) (string, int, error) {
	if key == doc.ID {
		return doc.ID, 1, nil
	}
	prefix := doc.ID + "@"
	if !strings.HasPrefix(key, prefix) {
		return "", 0, fmt.Errorf("scenario catalog key %s differs from workflow id %s", key, doc.ID)
	}
	revision, err := strconv.Atoi(strings.TrimPrefix(key, prefix))
	if err != nil || revision < 1 {
		return "", 0, fmt.Errorf("invalid scenario catalog revision %s", key)
	}
	return doc.ID, revision, nil
}
