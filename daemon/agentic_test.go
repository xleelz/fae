package daemon

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/xleelz/fae/workflow"
)

func TestTargetConfigInlineBoundsAndCheckpointIntervalDefaults(t *testing.T) {
	def := DefaultTargetConfig()
	if def.InlineFileBytes != 1<<20 || def.InlineTurnBytes != 4<<20 || def.CheckpointIntervalSec != 300 {
		t.Fatalf("unexpected defaults: %+v", def)
	}
	var zero TargetConfig
	if zero.EffectiveInlineFileBytes() != 1<<20 || zero.EffectiveInlineTurnBytes() != 4<<20 || zero.EffectiveCheckpointIntervalSec() != 300 {
		t.Fatalf("unexpected effective defaults on zero TargetConfig")
	}

	raw, err := json.Marshal(TargetConfig{
		SchemaVersion:         4,
		Workspace:             "/tmp/workspace",
		IntegrationRef:        "refs/heads/fae/main",
		ProtectedBranches:     []string{"refs/heads/fae/main"},
		AllowedCommands:       [][]string{{"go", "test", "./..."}},
		InlineFileBytes:       2 << 20,
		InlineTurnBytes:       8 << 20,
		CheckpointIntervalSec: 120,
	})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseTarget(raw)
	if err != nil {
		t.Fatalf("parseTarget: %v", err)
	}
	if parsed.EffectiveInlineFileBytes() != 2<<20 || parsed.EffectiveInlineTurnBytes() != 8<<20 || parsed.EffectiveCheckpointIntervalSec() != 120 {
		t.Fatalf("unexpected parsed effective bounds: %+v", parsed)
	}

	negRaw, _ := json.Marshal(TargetConfig{
		SchemaVersion:     4,
		Workspace:         "/tmp/workspace",
		IntegrationRef:    "refs/heads/fae/main",
		ProtectedBranches: []string{"refs/heads/fae/main"},
		AllowedCommands:   [][]string{{"go", "test", "./..."}},
		InlineFileBytes:   -1,
	})
	if _, err := parseTarget(negRaw); err == nil {
		t.Fatal("expected error for negative InlineFileBytes")
	}
}

func TestAdmitReadOnlyFilesValidationAndLiveScope(t *testing.T) {
	if err := validateStructuredFields(AdmitParams{
		DeclaredFiles: []string{"pkg/a.go"},
		ReadOnlyFiles: []string{"docs/spec.md", "pkg/ref.go"},
	}); err != nil {
		t.Fatalf("expected valid ReadOnlyFiles, got %v", err)
	}
	if err := validateStructuredFields(AdmitParams{
		ReadOnlyFiles: []string{"../escape.md"},
	}); err == nil {
		t.Fatal("expected error for unsafe ReadOnlyFiles path")
	}
	if err := validateStructuredFields(AdmitParams{
		ReadOnlyFiles: []string{".git/config"},
	}); err == nil {
		t.Fatal("expected error for .git ReadOnlyFiles path")
	}
	if err := validateStructuredFields(AdmitParams{
		ReadOnlyFiles: []string{"dup.md", "dup.md"},
	}); err == nil {
		t.Fatal("expected error for duplicate ReadOnlyFiles path")
	}

	x := &liveExecutor{}
	stage := workflow.Stage{
		Primitive:     workflow.AgentTurn,
		ReadOnlyFiles: []string{"stage_ro.md"},
	}
	scope, err := x.resolveLiveTaskScope("task-ro", nil, stage, func() (envelope, error) {
		return envelope{
			Admission: AdmitParams{
				DeclaredFiles: []string{"pkg/edit.go"},
				ReadOnlyFiles: []string{"admit_ro.md"},
			},
		}, nil
	})
	if err != nil {
		t.Fatalf("resolveLiveTaskScope: %v", err)
	}
	brief := scope.briefScope()
	if !slices.Equal(brief.Files, []string{"pkg/edit.go"}) {
		t.Fatalf("unexpected Files: %v", brief.Files)
	}
	if !slices.Equal(brief.ReadOnlyFiles, []string{"admit_ro.md", "stage_ro.md"}) {
		t.Fatalf("unexpected ReadOnlyFiles: %v", brief.ReadOnlyFiles)
	}
}
