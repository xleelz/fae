package daemon

import (
	"encoding/json"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
	"slices"
	"sort"
)

// currentWorkflowFixture gives fresh-install fixtures current authoring
// metadata. Historical stored-revision fixtures intentionally don't use it.
func currentWorkflowFixture(d workflow.Document) workflow.Document {
	d.SchemaVersion = workflow.CurrentSchema
	if d.TargetRequirements == nil {
		d.TargetRequirements = &workflow.TargetRequirements{}
	}
	d.TargetRequirements.Commands = nil
	ids := make([]string, 0, len(d.Stages))
	for id := range d.Stages {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		s := d.Stages[id]
		if s.Primitive == workflow.Gate && s.Name == "" {
			s.Name = id
		}
		if s.Primitive == workflow.Escalate {
			if s.Kind == "" {
				s.Kind = domain.HumanDecisionRequired
			}
			if s.Reason == "" {
				s.Reason = "fixture escalation at {stage}"
			}
		}
		if s.Primitive == workflow.FanIn && s.Output == "" {
			s.Output = "JoinResult_" + id
			if d.Artifacts == nil {
				d.Artifacts = map[string]workflow.ArtifactType{}
			}
			d.Artifacts[s.Output] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"array","items":{"type":"object"}}`)}
		}
		if s.Primitive == workflow.HostCommand {
			d.TargetRequirements.Commands = append(d.TargetRequirements.Commands, s.Command)
		}
		if s.Primitive == workflow.ExternalEffect {
			r := d.Roles[s.Role]
			if !slices.Contains(r.Profile.Effects, s.Subject) {
				r.Profile.Effects = append(r.Profile.Effects, s.Subject)
			}
			d.Roles[s.Role] = r
		}
		d.Stages[id] = s
	}
	return d
}
