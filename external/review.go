package external

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ReviewPolicy binds an operator-provisioned package workspace and the read
// tools used to prove its baseline and each subsequent review gate. Optional
// provisioning is invoked only by explicit operator workspace preparation.
type ReviewPolicy struct {
	PollSeconds  int               `json:"poll_seconds,omitempty"`
	MaxPolls     int               `json:"max_polls,omitempty"`
	Operations   map[string]string `json:"operations,omitempty"`
	ArgumentKeys map[string]string `json:"argument_keys,omitempty"`

	Package             string `json:"package"`
	Workspace           string `json:"workspace"`
	Branch              string `json:"branch"`
	IntegrationTip      string `json:"integration_tip"`
	ProvisionTool       string `json:"provision_tool,omitempty"`
	BaselineTool        string `json:"baseline_tool"`
	StatusTool          string `json:"status_tool"`
	BaselineApproved    bool   `json:"baseline_approved"`
	DisablePublishMerge bool   `json:"disable_publish_merge,omitempty"`
	RemediationStage    string `json:"remediation_stage,omitempty"`
	MaxRevisions        int    `json:"max_revisions,omitempty"`
}

type ReviewAnalyzer struct {
	Name        string   `json:"name"`
	Status      string   `json:"status"`
	RequiredFor []string `json:"required_for"`
	Cause       string   `json:"cause,omitempty"`
}
type ReviewComment struct {
	ID        string `json:"id"`
	Blocking  bool   `json:"blocking"`
	Addressed bool   `json:"addressed"`
}

// ReviewStatus is the schema-validated adapter read contract. A vendor adapter
// must preserve every analyzer's own requirements and the automated comment
// severity semantics discovered from its installed vendor schema.
type ReviewStatus struct {
	ReviewID  string           `json:"review_id"`
	Tree      string           `json:"tree"`
	Build     ReviewAnalyzer   `json:"build"`
	Analyzers []ReviewAnalyzer `json:"analyzers"`
	Comments  []ReviewComment  `json:"comments"`
}

type ReviewDecision struct {
	Publish   bool
	Merge     bool
	Remediate bool
}

func EvaluateReview(s ReviewStatus) (ReviewDecision, error) {
	var d ReviewDecision
	if s.ReviewID == "" || s.Tree == "" || s.Build.Name == "" || s.Analyzers == nil || s.Comments == nil {
		return d, errors.New("ReviewGateFailed: incomplete review read")
	}
	publish, merge := s.Build.Status == "Pass", true
	seen := map[string]bool{}
	for _, a := range append([]ReviewAnalyzer{s.Build}, s.Analyzers...) {
		if a.Name == "" || seen[a.Name] || a.RequiredFor == nil {
			return d, errors.New("ReviewGateFailed: missing or duplicate analyzer declaration")
		}
		seen[a.Name] = true
		switch a.Status {
		case "Pass", "Fail", "Blocked", "Skipped":
		default:
			return d, fmt.Errorf("ReviewGateFailed: unknown or absent status for %s", a.Name)
		}
		for _, required := range a.RequiredFor {
			switch required {
			case "publish":
				publish = publish && a.Status == "Pass"
			case "merge":
				merge = merge && a.Status == "Pass"
			default:
				return d, fmt.Errorf("ReviewGateFailed: unknown requirement %q", required)
			}
		}
		if a.Status == "Fail" && a.Cause == "change" {
			d.Remediate = true
		}
	}
	seen = map[string]bool{}
	for _, c := range s.Comments {
		if c.ID == "" || seen[c.ID] {
			return d, errors.New("ReviewGateFailed: invalid comment identity")
		}
		seen[c.ID] = true
		if c.Blocking && !c.Addressed {
			merge = false
			d.Remediate = true
		}
	}
	d.Publish = MayAdvanceReview(publish, true, true)
	d.Merge = MayAdvanceReview(merge, !d.Remediate, true)
	return d, nil
}

func DecodeReviewStatus(raw json.RawMessage) (ReviewStatus, error) {
	var s ReviewStatus
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return s, err
	}
	var comments []map[string]json.RawMessage
	if err := json.Unmarshal(fields["comments"], &comments); err != nil {
		return s, err
	}
	for _, c := range comments {
		for _, key := range []string{"blocking", "addressed"} {
			v, ok := c[key]
			if !ok || string(v) == "null" {
				return s, fmt.Errorf("ReviewGateFailed: missing comment %s", key)
			}
			var flag bool
			if err := json.Unmarshal(v, &flag); err != nil {
				return s, err
			}
		}
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, err
	}
	_, err := EvaluateReview(s)
	return s, err
}

func (p ReviewPolicy) ArgumentKey(semantic string) string {
	if key := p.ArgumentKeys[semantic]; key != "" {
		return key
	}
	return semantic
}
func (p ReviewPolicy) Operation(alias, name string) string {
	if op := p.Operations[alias]; op != "" {
		return op
	}
	return name
}
