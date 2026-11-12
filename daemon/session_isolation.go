package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/provider"
	"github.com/xleelz/fae/workflow"
	"os"
	"path/filepath"
	"slices"
	"sort"
)

const isolationFactFile = "session-isolation.json"

// ProbeIsolation records the host-local fact used by serve and doctor. Doctor
// may run without an existing instance; in that case its JSON output is the
// diagnostic record and no instance is created as a side effect.
func ProbeIsolation(ctx context.Context, home, instance string) provider.IsolationCapability {
	l, err := Paths(home, instance)
	root := os.TempDir()
	if err == nil {
		if info, e := os.Stat(l.Run); e == nil && info.IsDir() {
			root = l.Run
		}
	}
	fact := provider.ProbeSessionIsolation(ctx, root)
	if err == nil {
		if info, e := os.Stat(l.Run); e == nil && info.IsDir() {
			_ = writeIsolationFact(l.Run, fact)
		}
	}
	return fact
}

func writeIsolationFact(run string, fact provider.IsolationCapability) error {
	raw, err := json.Marshal(fact)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(run, ".isolation-fact-")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.Write(raw); err == nil {
		err = file.Sync()
	}
	err2 := file.Close()
	if err == nil {
		err = err2
	}
	if err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(run, isolationFactFile))
}
func (d *Daemon) probeIsolation(ctx context.Context) error {
	probe := d.config.IsolationProbe
	if probe == nil {
		probe = provider.ProbeSessionIsolation
	}
	fact := probe(ctx, d.layout.Run)
	d.isolationFact.Store(fact)
	if err := writeIsolationFact(d.layout.Run, fact); err != nil {
		return err
	}
	raw, _ := json.Marshal(fact)
	return d.store.PutMarker(ctx, controlplane.Audit{Actor: "daemon", Action: "session isolation host fact", Outcome: "ok"}, "host-fact/session-isolation", raw)
}
func (d *Daemon) currentIsolation() provider.IsolationCapability {
	if d.layout.Run != "" {
		if raw, err := os.ReadFile(filepath.Join(d.layout.Run, isolationFactFile)); err == nil {
			var fact provider.IsolationCapability
			if json.Unmarshal(raw, &fact) == nil {
				return fact
			}
		}
	}
	if value := d.isolationFact.Load(); value != nil {
		return value.(provider.IsolationCapability)
	}
	return provider.IsolationCapability{Step: "not-probed", Reason: "session isolation has not been probed", Remedy: "restart fae serve or run fae doctor"}
}

type IsolationAdmissionError struct {
	Stage string
	Fact  provider.IsolationCapability
}

func (e *IsolationAdmissionError) Error() string {
	return fmt.Sprintf("session-isolation-unavailable: stage %s requires %s: %s; %s", e.Stage, e.Fact.Step, e.Fact.Reason, e.Fact.Remedy)
}
func (e *IsolationAdmissionError) Code() string { return "session-isolation-unavailable" }
func (e *IsolationAdmissionError) Unwrap() error {
	return &api.RPCError{Code: e.Code(), Message: e.Error()}
}

func (d *Daemon) validateWorkflowIsolation(ctx context.Context, p AdmitParams, doc workflow.Document) error {
	if d.live == nil {
		return nil
	} // Scripted rehearsal launches no provider/command.
	fact := d.currentIsolation()
	if fact.Available {
		return nil
	}
	target, err := d.pinnedTarget(ctx, p)
	if err != nil {
		return err
	}
	return validateIsolationStages(p, doc, target, fact)
}

func validateIsolationStages(p AdmitParams, doc workflow.Document, target *TargetConfig, fact provider.IsolationCapability) error {
	denies := []string{}
	roots := map[string]provider.ReadRootView{}
	if target != nil {
		denies = target.DenyPaths
		for name, root := range target.ReadRoots {
			roots[name] = provider.ReadRootView{Name: name, Path: root.Path, Deny: root.Deny}
		}
	}
	stages := make([]string, 0, len(doc.Stages))
	for id := range doc.Stages {
		stages = append(stages, id)
	}
	sort.Strings(stages)
	for _, id := range stages {
		stage := doc.Stages[id]
		if stage.Primitive == workflow.HostCommand {
			// Match authorizeCommandWithTask: only role roots and explicit
			// binding/template roots are exposed to this command.
			selected := map[string]provider.ReadRootView{}
			for _, name := range workflow.StageReadRoots(doc, p.TaskClass, stage) {
				if root, ok := roots[name]; ok {
					selected[name] = root
				}
			}
			if provider.RequiresSessionIsolation(false, true, stage.Reads, denies, selected) {
				return &IsolationAdmissionError{Stage: id, Fact: fact}
			}
			continue
		}
		if stage.Primitive != workflow.AgentTurn && stage.Primitive != workflow.Review {
			continue
		}
		roles := []string{stage.Role}
		if stage.Primitive == workflow.Review {
			roles = stage.Reviewers
		}
		for _, name := range roles {
			role, ok := workflow.EffectiveRole(doc, p.TaskClass, name)
			if !ok || role.Session != "agentic" {
				continue
			}
			profile := workflow.EffectiveProfile(role.Profile, stage)
			shell := slices.Contains(profile.Allowed, "shell") && !slices.Contains(profile.Denied, "shell")
			selected := map[string]provider.ReadRootView{}
			for _, root := range role.ReadRoots {
				if value, ok := roots[root]; ok {
					selected[root] = value
				}
			}
			policy := workflow.EffectiveRoleStageReadPolicy(doc, p.TaskClass, name, id, p.Reads, denies)
			if !provider.RequiresSessionIsolation(shell, false, policy, denies, selected) {
				continue
			}
			if target != nil && target.ShellRuntimeMountsWithoutView && provider.RuntimeShellWithoutView(profile, target.RuntimeMounts) {
				continue
			}
			return &IsolationAdmissionError{Stage: id, Fact: fact}
		}
	}
	return nil
}
