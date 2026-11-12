package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/provider"
	"github.com/xleelz/fae/workflow"
	"os"
	"sync/atomic"
	"testing"
)

func TestIsolationAdmissionRefusesOnlyStagesRequiringView(t *testing.T) {
	d := &Daemon{live: &liveExecutor{}}
	d.isolationFact.Store(provider.IsolationCapability{Step: "namespace-tool", Reason: "bubblewrap unavailable", Remedy: "install bubblewrap"})
	doc := slotWorkflow("isolation", "tree")
	role := doc.Roles["author"]
	role.Session = "agentic"
	role.Profile.Allowed = []string{"read", "shell", "handoff"}
	role.Reads = &workflow.ReadPolicy{Deny: []string{"private/**"}}
	doc.Roles["author"] = role
	err := d.validateWorkflowIsolation(context.Background(), AdmitParams{}, doc)
	var refusal *IsolationAdmissionError
	var rpc *api.RPCError
	if !errors.As(err, &refusal) || refusal.Stage != "make" || !errors.As(err, &rpc) || rpc.Code != "session-isolation-unavailable" {
		t.Fatal("missing typed stage refusal", err)
	}
	role.Profile.Allowed = []string{"read", "handoff"}
	doc.Roles["author"] = role
	if err := d.validateWorkflowIsolation(context.Background(), AdmitParams{}, doc); err != nil {
		t.Fatal("shell-less agentic turn unnecessarily requires namespace", err)
	}
	d.isolationFact.Store(provider.IsolationCapability{Available: true, Step: "ready"})
	role.Profile.Allowed = append(role.Profile.Allowed, "shell")
	doc.Roles["author"] = role
	if err := d.validateWorkflowIsolation(context.Background(), AdmitParams{}, doc); err != nil {
		t.Fatal("available isolation refused", err)
	}
}

func TestIsolationFactPersistsAndDoctorRefreshCanClearIt(t *testing.T) {
	run := t.TempDir()
	d := &Daemon{}
	d.layout.Run = run
	missing := provider.IsolationCapability{Step: "namespace-tool", Reason: "missing bubblewrap", Remedy: "install bubblewrap"}
	if err := writeIsolationFact(run, missing); err != nil {
		t.Fatal(err)
	}
	if d.currentIsolation().Available || d.currentIsolation().Step != "namespace-tool" {
		t.Fatal(d.currentIsolation())
	}
	if err := writeIsolationFact(run, provider.IsolationCapability{Available: true, Step: "ready"}); err != nil {
		t.Fatal(err)
	}
	if !d.currentIsolation().Available {
		t.Fatal("re-probed fact did not clear missing capability", d.currentIsolation())
	}
}

func TestServeRecordsIsolationHostFactNative(t *testing.T) {
	cfg, ctx := fixture(t)
	var probes atomic.Int32
	cfg.IsolationProbe = func(context.Context, string) provider.IsolationCapability {
		probes.Add(1)
		return provider.IsolationCapability{Step: "namespace-tool", Reason: "controlled missing tool", Remedy: "install bubblewrap"}
	}
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	raw, err := d.store.GetMarker(ctx, "host-fact/session-isolation")
	if err != nil {
		t.Fatal(err)
	}
	var fact provider.IsolationCapability
	if json.Unmarshal(raw, &fact) != nil || fact.Step != "namespace-tool" || fact.Available || probes.Load() != 1 {
		t.Fatal(string(raw), probes.Load())
	}
	if d.currentIsolation().Step != "namespace-tool" {
		t.Fatal(d.currentIsolation())
	}
}

func TestIsolationAdmissionHostCommandUsesOnlySelectedReadRootsNative(t *testing.T) {
	cfg, ctx := fixture(t)
	running, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, running)
	writable, readonly := t.TempDir(), t.TempDir()
	if err = os.Chmod(readonly, 0555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(readonly, 0700)
	digest, err := ComputeReadRootDigest(writable, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	readonlyDigest, err := ComputeReadRootDigest(readonly, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	target := targetFixture(cfg.Workspace)
	target.ReadRoots = map[string]ReadRootConfig{"writable": {Path: writable, Digest: digest}, "readonly": {Path: readonly, Digest: readonlyDigest}}
	raw, _ := json.Marshal(target)
	revision, err := running.store.WriteTarget(ctx, controlplane.Audit{Actor: "test", Action: "pin isolation test target", Outcome: "ok"}, "isolation-root-selection", raw, func(raw []byte) error { _, e := parseTarget(raw); return e })
	if err != nil {
		t.Fatal(err)
	}
	d := &Daemon{store: running.store, live: &liveExecutor{}}
	d.isolationFact.Store(provider.IsolationCapability{Step: "namespace-tool", Reason: "controlled missing bubblewrap", Remedy: "install bubblewrap"})
	params := AdmitParams{TargetConfig: revision.ID, TargetRevision: revision.Revision, TargetHash: revision.Hash}
	cases := []struct {
		name   string
		stage  workflow.Stage
		roles  map[string]workflow.Role
		refuse bool
	}{
		{name: "unused-writable-root", stage: workflow.Stage{Primitive: workflow.HostCommand}},
		{name: "selected-readonly-root", stage: workflow.Stage{Primitive: workflow.HostCommand, Bindings: map[string]workflow.ArgBinding{"root": {Kind: "read-root", Key: "readonly"}}}},
		{name: "selected-role-root", stage: workflow.Stage{Primitive: workflow.HostCommand, Role: "checker"}, roles: map[string]workflow.Role{"checker": {ReadRoots: []string{"writable"}}}, refuse: true},
		{name: "selected-binding-root", stage: workflow.Stage{Primitive: workflow.HostCommand, Bindings: map[string]workflow.ArgBinding{"root": {Kind: "read-root", Key: "writable"}}}, refuse: true},
		{name: "selected-template-root", stage: workflow.Stage{Primitive: workflow.HostCommand, ArgTemplates: map[string]workflow.ArgTemplate{"root": {Kind: "read-root", Key: "writable"}}}, refuse: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := workflow.Document{Stages: map[string]workflow.Stage{"check": tc.stage}, Roles: tc.roles}
			err := d.validateWorkflowIsolation(ctx, params, doc)
			var refused *IsolationAdmissionError
			if tc.refuse {
				if !errors.As(err, &refused) || refused.Stage != "check" {
					t.Fatalf("selected writable root not refused: %v", err)
				}
			} else if err != nil {
				t.Fatalf("unused writable root required isolation: %v", err)
			}
		})
	}
}

func TestIsolationHostCommandRootSelectionWithoutDaemon(t *testing.T) {
	writable, readonly := t.TempDir(), t.TempDir()
	if err := os.Chmod(readonly, 0555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(readonly, 0700)
	target := &TargetConfig{ReadRoots: map[string]ReadRootConfig{"writable": {Path: writable}, "readonly": {Path: readonly}}}
	fact := provider.IsolationCapability{Step: "namespace-tool", Reason: "controlled missing tool"}
	for _, tc := range []struct {
		name   string
		stage  workflow.Stage
		roles  map[string]workflow.Role
		refuse bool
	}{
		{name: "unused-writable-root", stage: workflow.Stage{Primitive: workflow.HostCommand}},
		{name: "readonly-root", stage: workflow.Stage{Primitive: workflow.HostCommand, Bindings: map[string]workflow.ArgBinding{"root": {Kind: "read-root", Key: "readonly"}}}},
		{name: "role-root", stage: workflow.Stage{Primitive: workflow.HostCommand, Role: "checker"}, roles: map[string]workflow.Role{"checker": {ReadRoots: []string{"writable"}}}, refuse: true},
		{name: "binding-root", stage: workflow.Stage{Primitive: workflow.HostCommand, Bindings: map[string]workflow.ArgBinding{"root": {Kind: "read-root", Key: "writable"}}}, refuse: true},
		{name: "template-root", stage: workflow.Stage{Primitive: workflow.HostCommand, ArgTemplates: map[string]workflow.ArgTemplate{"root": {Kind: "read-root", Key: "writable"}}}, refuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateIsolationStages(AdmitParams{}, workflow.Document{Stages: map[string]workflow.Stage{"check": tc.stage}, Roles: tc.roles}, target, fact)
			var refused *IsolationAdmissionError
			if tc.refuse {
				if !errors.As(err, &refused) || refused.Stage != "check" {
					t.Fatal("selected root not refused", err)
				}
			} else if err != nil {
				t.Fatal("unselected writable root refused unrelated command", err)
			}
		})
	}
}
