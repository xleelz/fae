package daemon

import (
	"context"
	"fmt"

	"github.com/xleelz/fae/external"
	"github.com/xleelz/fae/workflow"
)

// normalContent decides what a task's disposition can reach before the task
// pins its document. A Local task pins the Local projection, so it never
// reaches a Normal-only stage (FAE-WF-080, FAE-MODE-002). A Normal task binds
// every TargetPins stage to the installed tool for its subject: the target
// configuration's exact tools, else the instance's (FAE-EXT-002). A missing
// tool or a differing role refuses admission, naming the stage and subject.
func normalContent(doc workflow.Document, mode string, tools map[string]external.Tool) (workflow.Document, error) {
	if mode != "Normal" {
		return workflow.LocalProjection(doc)
	}
	stages := make(map[string]workflow.Stage, len(doc.Stages))
	for id, s := range doc.Stages {
		if s.Primitive == workflow.ExternalEffect && workflow.IsTargetPinned(s.Pins) {
			t, ok := tools[s.Subject]
			if !ok {
				return doc, fmt.Errorf("Normal stage %s requires an installed tool for subject %s", id, s.Subject)
			}
			if t.Role != s.Role {
				return doc, fmt.Errorf("Normal stage %s role %s differs from installed tool %s role %s", id, s.Role, s.Subject, t.Role)
			}
			s.Pins = map[string]string{}
			for k, v := range t.Arguments {
				s.Pins[k] = v
			}
		}
		stages[id] = s
	}
	doc.Stages = stages
	return doc, nil
}

// admissionTools returns the tools a task's Normal stages bind: its pinned
// target configuration's exact Normal tools, else the instance's installed ones.
func (d *Daemon) admissionTools(ctx context.Context, p AdmitParams) (map[string]external.Tool, error) {
	if p.TargetConfig != "" {
		_, raw, err := d.store.GetTarget(ctx, p.TargetConfig, p.TargetRevision)
		if err != nil {
			return nil, err
		}
		c, err := parseTarget(raw)
		if err != nil {
			return nil, err
		}
		if c.Normal != nil {
			return c.Normal.Tools, nil
		}
	}
	if d.live == nil {
		return nil, nil
	}
	return d.live.config.ExternalTools, nil
}
