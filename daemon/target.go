package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/controlplane"
	agentruntime "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

// DefaultInlineFileBytes, DefaultInlineTurnBytes, and DefaultCheckpointIntervalSec
// are the default target bounds for inline brief files/turns (FAE-RT-017) and
// periodic agentic turn checkpoints (FAE-RT-019).
const (
	DefaultInlineFileBytes       = 1 << 20
	DefaultInlineTurnBytes       = 4 << 20
	DefaultCheckpointIntervalSec = 300
)

// TargetConfig versions bind exact installed policy; v1 retains Local-only behavior.
// Unknown schema fields are rejected rather than stored as unenforced policy.
// Schema 4 adds typed command shapes, required QA tests, a compile check and a
// diff classifier; earlier schemas refuse them, so stored revisions keep
// their exact meaning.
type TargetConfig struct {
	RuntimeMountIdentities        map[string]string   `json:"runtime_mount_identities,omitempty"`
	RuntimeMounts                 []string            `json:"runtime_mounts,omitempty"`
	CacheDirs                     []string            `json:"cache_dirs,omitempty"`
	ShellRuntimeMountsWithoutView bool                `json:"shell_runtime_mounts_without_view,omitempty"`
	RestartOnly                   bool                `json:"restart_only,omitempty"`
	CaptureBytes                  int                 `json:"capture_bytes,omitempty"`
	Normal                        *NormalTargetPolicy `json:"normal,omitempty"`
	SchemaVersion                 int                 `json:"schema_version"`
	Workspace                     string              `json:"workspace"`
	IntegrationRef                string              `json:"integration_ref"`
	ProtectedBranches             []string            `json:"protected_branches"`
	AllowedCommands               [][]string          `json:"allowed_commands"`

	RequiredTests  []RequiredTest            `json:"required_tests,omitempty"`
	CompileCheck   *TargetCommand            `json:"compile_check,omitempty"`
	DiffClassifier *DiffClassifier           `json:"diff_classifier,omitempty"`
	DocumentGlobs  []string                  `json:"document_globs,omitempty"`
	DocumentChecks []DocumentCheck           `json:"document_checks,omitempty"`
	Landing        *LandingPolicy            `json:"landing,omitempty"`
	ReadRoots      map[string]ReadRootConfig `json:"read_roots,omitempty"`
	ContentPolicy  ContentPolicyConfig       `json:"content_policy,omitempty"`
	DenyPaths      []string                  `json:"deny_paths,omitempty"`
	CommitIdentity *CommitIdentityConfig     `json:"commit_identity,omitempty"`
	Push           *PushConfig               `json:"push,omitempty"`

	InlineFileBytes       int `json:"inline_file_bytes,omitempty"`
	InlineTurnBytes       int `json:"inline_turn_bytes,omitempty"`
	CheckpointIntervalSec int `json:"checkpoint_interval_sec,omitempty"`
}

// DefaultTargetConfig returns a TargetConfig populated with the FAE-RT-017
// inline brief defaults (1 MiB per file, 4 MiB per turn) and FAE-RT-019
// checkpoint interval default (300 s).
func DefaultTargetConfig() TargetConfig {
	return TargetConfig{
		InlineFileBytes:       DefaultInlineFileBytes,
		InlineTurnBytes:       DefaultInlineTurnBytes,
		CheckpointIntervalSec: DefaultCheckpointIntervalSec,
	}
}

func (c TargetConfig) EffectiveInlineFileBytes() int {
	if c.InlineFileBytes > 0 {
		return c.InlineFileBytes
	}
	return DefaultInlineFileBytes
}

func (c TargetConfig) EffectiveInlineTurnBytes() int {
	if c.InlineTurnBytes > 0 {
		return c.InlineTurnBytes
	}
	return DefaultInlineTurnBytes
}

func (c TargetConfig) EffectiveCheckpointIntervalSec() int {
	if c.CheckpointIntervalSec > 0 {
		return c.CheckpointIntervalSec
	}
	return DefaultCheckpointIntervalSec
}

// DocumentCheck configures a structured check command for document-only
// landings (FAE-CTL-052, FAE-WF-095).
type DocumentCheck struct {
	ID         string   `json:"id"`
	Command    []string `json:"command"`
	Format     string   `json:"format"`           // "json" | "lines"
	Output     string   `json:"output,omitempty"` // "stdout" or path
	CountField string   `json:"count_field"`
	Writes     []string `json:"writes,omitempty"`
}

// RequiredTest is an always-required QA command, optionally scoped to tasks
// whose diff touches one of Paths (FAE-WF-085).
type RequiredTest struct {
	Name           string   `json:"name"`
	Argv           []string `json:"argv"`
	Paths          []string `json:"paths,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

// TargetCommand is one literal allowlisted command with its bound.
type TargetCommand struct {
	Argv           []string `json:"argv"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

// DiffClassifier decides whether a changed path is production code: it
// matches a Code pattern and no NotCode pattern (FAE-WF-073).
type DiffClassifier struct {
	Code    []string `json:"code"`
	NotCode []string `json:"not_code,omitempty"`
}
type TargetPutParams struct {
	ID                 string       `json:"id"`
	Document           TargetConfig `json:"document"`
	AuthorizingMessage string       `json:"authorizing_message"`
}

func parseTarget(raw []byte) (c TargetConfig, err error) {
	if err = decode(raw, &c); err != nil {
		return
	}
	if (c.SchemaVersion < 1 || c.SchemaVersion > 4) || !utf8.ValidString(c.Workspace) || strings.ContainsRune(c.Workspace, 0) || !filepath.IsAbs(c.Workspace) || filepath.Clean(c.Workspace) != c.Workspace || !strings.HasPrefix(c.IntegrationRef, "refs/heads/") || len(c.AllowedCommands) == 0 || !slices.Contains(c.ProtectedBranches, c.IntegrationRef) {
		return c, errors.New("target v1 requires canonical absolute workspace, integration ref declared protected, and nonempty command allowlist")
	}
	if c.InlineFileBytes < 0 || c.InlineTurnBytes < 0 || c.CheckpointIntervalSec < 0 {
		return c, errors.New("target inline bounds and checkpoint interval must be nonnegative")
	}
	if err = validateNormalTargetShape(c); err != nil {
		return c, err
	}
	seen := map[string]bool{}
	for _, b := range c.ProtectedBranches {
		if !strings.HasPrefix(b, "refs/heads/") || strings.ContainsAny(b, " \t\n~^:?*[\\") || strings.Contains(b, "..") || seen[b] {
			return c, errors.New("invalid or duplicate protected branch")
		}
		seen[b] = true
	}
	for _, argv := range c.AllowedCommands {
		if len(argv) == 0 {
			return c, errors.New("empty command shape")
		}
		for i, arg := range argv {
			// Schema 4 shapes (and read-root slots) may hold whole-argument {{type}} slots after the program.
			if slotType, slot := workflow.Slot(arg); slot && i > 0 && (c.SchemaVersion >= 4 || slotType == "read-root") {
				continue
			}
			if arg == "" || strings.ContainsAny(arg, "\x00\n\r") || strings.Contains(arg, "{{") {
				return c, errors.New("target command must be a literal argument vector")
			}
		}
	}
	if err = validateLandingPolicy(c); err != nil {
		return c, err
	}
	if err = validateTargetSecurityAndRoots(c); err != nil {
		return c, err
	}
	forbidden := []string{c.Workspace}
	for _, root := range c.ReadRoots {
		forbidden = append(forbidden, root.Path)
	}
	identities, mountErr := agentruntime.RuntimeMountIdentities(c.RuntimeMounts, forbidden)
	if mountErr != nil {
		return c, mountErr
	}
	for path, pinned := range c.RuntimeMountIdentities {
		if identities[path] != pinned {
			return c, fmt.Errorf("runtime mount identity changed: %s", path)
		}
	}
	if err = agentruntime.ValidateCacheDirectories(c.CacheDirs, append(append([]string{}, forbidden...), c.RuntimeMounts...)); err != nil {
		return c, err
	}
	for _, cache := range c.CacheDirs {
		if !filepath.IsAbs(cache) || filepath.Clean(cache) == "/" {
			return c, errors.New("cache directory must be absolute and non-root")
		}
	}
	return c, validateTargetPolicy(c)
}
func (d *Daemon) validateTargetBinding(c TargetConfig) error {
	if _, err := agentruntime.RuntimeMountIdentities(c.RuntimeMounts, []string{d.layout.Root}); err != nil {
		return err
	}
	physical, err := filepath.EvalSymlinks(c.Workspace)
	if err != nil {
		return fmt.Errorf("target workspace binding: %w", err)
	}
	info, err := os.Stat(physical)
	if err != nil || !info.IsDir() || physical != c.Workspace || c.Workspace != d.config.Workspace {
		return errors.New("target workspace binding differs from daemon canonical workspace")
	}
	if d.live != nil && c.IntegrationRef != d.live.config.IntegrationRef {
		return errors.New("target integration ref differs from live broker")
	}
	return nil
}
func targetAllows(c TargetConfig, argv []string) bool {
	for _, shape := range c.AllowedCommands {
		if slices.Equal(shape, argv) {
			return true
		}
	}
	return false
}
func (d *Daemon) pinTarget(ctx context.Context, p *AdmitParams, doc workflow.Document) error {
	referencedRoots := workflowReferencedReadRoots(doc)
	if p.TargetConfig == "" {
		if len(referencedRoots) > 0 {
			return fmt.Errorf("workflow references read_roots %v but no target_config was provided", referencedRoots)
		}
		if p.TargetRevision != 0 || p.TargetHash != "" {
			return errors.New("target selector required")
		}
		return nil
	}
	r, raw, err := d.store.GetTarget(ctx, p.TargetConfig, p.TargetRevision)
	if err != nil {
		return err
	}
	c, err := parseTarget(raw)
	if err != nil {
		return err
	}
	if err = d.validateTargetBinding(c); err != nil {
		return err
	}
	for _, f := range p.DeclaredFiles {
		if idx := workflow.MatchDenyPathIndex(c.DenyPaths, f); idx >= 0 {
			return fmt.Errorf("declared file %q refused by deny_paths[%d]", f, idx)
		}
	}
	for _, name := range referencedRoots {
		if _, ok := c.ReadRoots[name]; !ok {
			return fmt.Errorf("workflow references unconfigured read_root %q", name)
		}
	}
	if err = d.validateNormalTarget(ctx, c, *p, doc, false); err != nil {
		return err
	}
	for name, s := range doc.Stages {
		if s.Primitive == workflow.HostCommand && !TargetCommandAuthorized(true, targetAllowsStage(c, s)) {
			return fmt.Errorf("target disallows HostCommand stage %s", name)
		}
	}
	if unmet := workflow.UnmetTargetRequirements(doc.TargetRequirements, c.AllowedCommands, c.DiffClassifier != nil, len(c.RequiredTests) > 0 || c.CompileCheck != nil); len(unmet) > 0 {
		return fmt.Errorf("target lacks the workflow's requirements: %s", strings.Join(unmet, "; "))
	}
	if d.live != nil {
		// The target's own checks run through the instance broker too.
		for _, t := range c.RequiredTests {
			if !slices.ContainsFunc(d.live.config.AllowedCommands, func(v []string) bool { return slices.Equal(v, t.Argv) }) {
				return fmt.Errorf("instance allowlist lacks required test %s", t.Name)
			}
		}
		if k := c.CompileCheck; k != nil && !slices.ContainsFunc(d.live.config.AllowedCommands, func(v []string) bool { return slices.Equal(v, k.Argv) }) {
			return errors.New("instance allowlist lacks the target compile check")
		}
	}
	p.TargetRevision = r.Revision
	p.TargetHash = r.Hash
	return nil
}
func (d *Daemon) checkPinnedTarget(ctx context.Context, p AdmitParams, doc workflow.Document) error {
	if p.TargetConfig == "" {
		return nil
	}
	revision, hash := p.TargetRevision, p.TargetHash
	if revision < 1 || hash == "" {
		return errors.New("missing immutable target binding")
	}
	if err := d.pinTarget(ctx, &p, doc); err != nil {
		return err
	}
	if p.TargetRevision != revision || p.TargetHash != hash {
		return errors.New("target content identity mismatch")
	}
	return nil
}
func (d *Daemon) putTarget(ctx context.Context, a controlplane.Audit, raw json.RawMessage) (any, error) {
	var p TargetPutParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if p.Document.Normal != nil && p.Document.Normal.Adapter != nil {
		if err := d.store.AdapterActive(ctx, p.Document.Normal.Adapter.ID); err != nil {
			return nil, err
		}
		b, _, err := d.resolveAdapter(ctx, *p.Document.Normal.Adapter)
		if err != nil {
			return nil, err
		}
		p.Document.Normal.Adapter = &b
	}
	mountIdentities, mountErr := agentruntime.RuntimeMountIdentities(p.Document.RuntimeMounts, nil)
	if mountErr != nil {
		return nil, mountErr
	}
	p.Document.RuntimeMountIdentities = mountIdentities
	b, _ := json.Marshal(p.Document)
	return d.store.WriteTarget(ctx, a, p.ID, b, func(raw []byte) error {
		c, err := parseTarget(raw)
		if err != nil {
			return err
		}
		if err = d.validateTargetBinding(c); err != nil {
			return err
		}
		return d.validateNormalTarget(ctx, c, AdmitParams{}, workflow.Document{}, true)
	})
}
