package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/workflow"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

// TargetConfig versions bind exact installed policy; v1 retains Local-only behavior.
// Unknown schema fields are rejected rather than stored as unenforced policy.
type TargetConfig struct {
	Normal            *NormalTargetPolicy `json:"normal,omitempty"`
	SchemaVersion     int                 `json:"schema_version"`
	Workspace         string              `json:"workspace"`
	IntegrationRef    string              `json:"integration_ref"`
	ProtectedBranches []string            `json:"protected_branches"`
	AllowedCommands   [][]string          `json:"allowed_commands"`
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
	if (c.SchemaVersion != 1 && c.SchemaVersion != 2 && c.SchemaVersion != 3) || !utf8.ValidString(c.Workspace) || strings.ContainsRune(c.Workspace, 0) || !filepath.IsAbs(c.Workspace) || filepath.Clean(c.Workspace) != c.Workspace || !strings.HasPrefix(c.IntegrationRef, "refs/heads/") || len(c.AllowedCommands) == 0 || !slices.Contains(c.ProtectedBranches, c.IntegrationRef) {
		return c, errors.New("target v1 requires canonical absolute workspace, integration ref declared protected, and nonempty command allowlist")
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
		for _, arg := range argv {
			if arg == "" || strings.ContainsAny(arg, "\x00\n\r") || strings.Contains(arg, "{{") {
				return c, errors.New("target command must be a literal argument vector")
			}
		}
	}
	return
}
func (d *Daemon) validateTargetBinding(c TargetConfig) error {
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
	if p.TargetConfig == "" {
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
	if err = d.validateNormalTarget(ctx, c, *p, doc, false); err != nil {
		return err
	}
	for name, s := range doc.Stages {
		if s.Primitive == workflow.HostCommand && !TargetCommandAuthorized(true, targetAllows(c, s.Command)) {
			return fmt.Errorf("target disallows HostCommand stage %s", name)
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
