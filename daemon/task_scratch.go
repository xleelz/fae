package daemon

import (
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/execution"
	"github.com/xleelz/fae/workflow"
	"slices"
)

func taskScratch(doc workflow.Document) []string {
	var out []string
	add := func(patterns []string) {
		for _, p := range patterns {
			if !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	for _, r := range doc.Roles {
		add(r.Scratch)
	}
	for _, s := range doc.Stages {
		add(s.Scratch)
		if s.Check != nil && s.Generate == nil {
			add(s.Check.Writes)
		}
	}
	return out
}

func commandCaptureBytes(target *TargetConfig) int {
	if target != nil && target.CaptureBytes > 0 {
		return target.CaptureBytes
	}
	return 256 << 10
}
func taskDirectoryEnvironment(b execution.Binding) []string {
	var env []string
	if b.OutputDir != "" {
		env = append(env, "FAE_OUTPUT_DIR="+b.OutputDir)
	}
	if b.StagingDir != "" {
		env = append(env, "FAE_STAGING_DIR="+b.StagingDir)
	}
	return env
}
func taskTestCommands(doc workflow.Document, target *TargetConfig) [][]string {
	var out [][]string
	for _, stage := range doc.Stages {
		if stage.TestRun && len(stage.Command) > 0 {
			out = append(out, stage.Command)
		}
	}
	if target != nil {
		for _, test := range target.RequiredTests {
			out = append(out, test.Argv)
		}
	}
	return out
}

func applyStageWriteScopes(d engine.Dispatch, e envelope, scope *liveTaskScope) error {
	role, _ := workflow.EffectiveRole(e.Job.Document, e.Job.TaskClass, d.Config.Role)
	layers, err := execution.BoundWriteScopes(role, d, execution.Binding{TemplateJob: &e.Job, TaskWrites: e.Admission.Writes, Scope: scope.briefScope(), OutputDir: e.Job.OutputDir, StagingDir: e.Job.StagingDir})
	if err != nil {
		return err
	}
	explicitScopes := len(role.Writes) > 0 || len(d.Config.Writes) > 0 || d.Config.Generate != nil || len(d.WriteScope) > 0 || len(e.Admission.Writes) > 0
	if explicitScopes && len(layers) > 0 && (d.Config.ChangeProducer || d.Config.Generate != nil || len(role.Writes) > 0 || len(d.Config.Writes) > 0) {
		scope.Files = append([]string{}, layers[len(layers)-1]...)
		scope.Derived = true
	}
	return nil
}
