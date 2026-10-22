package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/external"
	"os"
	"path/filepath"
)

type AdapterBinding struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	Hash     string `json:"hash"`
}
type AdapterDocument struct {
	SchemaVersion             int                              `json:"schema_version"`
	Tools                     map[string]external.Tool         `json:"tools"`
	ReviewPolicies            map[string]external.ReviewPolicy `json:"review_policies,omitempty"`
	TicketWorkflow            *TicketWorkflowConfig            `json:"ticket_workflow,omitempty"`
	DisableReviewPublishMerge bool                             `json:"disable_review_publish_merge,omitempty"`
}
type AdapterPutParams struct {
	ID                 string          `json:"id"`
	Document           AdapterDocument `json:"document"`
	AuthorizingMessage string          `json:"authorizing_message"`
}
type installedAdapter struct {
	Document AdapterDocument                `json:"document"`
	Schemas  map[string]external.ToolSchema `json:"schemas"`
}

func adapterConfig(c LiveConfig, doc AdapterDocument) LiveConfig {
	c.ExternalTools = doc.Tools
	c.ReviewPolicies = doc.ReviewPolicies
	c.TicketWorkflow = doc.TicketWorkflow
	c.DisableReviewPublishMerge = doc.DisableReviewPublishMerge
	return c
}
func (d *Daemon) putAdapter(ctx context.Context, a controlplane.Audit, raw json.RawMessage) (any, error) {
	var p AdapterPutParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if p.Document.SchemaVersion != 1 || len(p.Document.Tools) == 0 || len(p.Document.Tools) > 100 {
		return nil, errors.New("adapter v1 requires 1..100 exact tools")
	}
	c := adapterConfig(LiveConfig{}, p.Document)
	if err := validateTicketConfig(c); err != nil {
		return nil, err
	}
	for alias, tool := range c.ExternalTools {
		if alias == "" {
			return nil, errors.New("empty adapter alias")
		}
		if err := tool.Validate(); err != nil {
			return nil, fmt.Errorf("adapter %s: %w", alias, err)
		}
	}
	// Installation is the only schema-discovery/provisioning boundary. Runtime
	// resolves immutable descriptors and never rewrites the shared instance config.
	if err := ValidateExternalSchemas(ctx, c); err != nil {
		return nil, err
	}
	saved := installedAdapter{Document: p.Document, Schemas: map[string]external.ToolSchema{}}
	for alias, t := range c.ExternalTools {
		if t.DiscoveredSchema == nil {
			return nil, errors.New("adapter schema absent")
		}
		saved.Schemas[alias] = *t.DiscoveredSchema
	}
	b, _ := json.Marshal(saved)
	return d.store.WriteAdapter(ctx, a, p.ID, b)
}
func (d *Daemon) resolveAdapter(ctx context.Context, b AdapterBinding) (AdapterBinding, AdapterDocument, error) {
	var zero AdapterDocument
	r, raw, err := d.store.GetAdapter(ctx, b.ID, b.Revision)
	if err != nil {
		return b, zero, err
	}
	if !AdapterBindingValid(b.Revision == 0 || b.Revision == r.Revision, b.Hash == "" || b.Hash == r.Hash) {
		return b, zero, errors.New("adapter revision/hash mismatch")
	}
	var saved installedAdapter
	if err = decode(raw, &saved); err != nil {
		return b, zero, err
	}
	if saved.Document.SchemaVersion != 1 || len(saved.Document.Tools) == 0 || len(saved.Schemas) != len(saved.Document.Tools) {
		return b, zero, errors.New("invalid installed adapter document")
	}
	for alias, t := range saved.Document.Tools {
		schema, ok := saved.Schemas[alias]
		if !ok {
			return b, zero, errors.New("installed adapter schema missing")
		}
		t.DiscoveredSchema = &schema
		saved.Document.Tools[alias] = t
	}
	return AdapterBinding{ID: r.ID, Revision: r.Revision, Hash: r.Hash}, saved.Document, nil
}
func (d *Daemon) liveForTarget(ctx context.Context, c TargetConfig) (*liveExecutor, error) {
	if d.live == nil {
		return nil, errors.New("live adapter runtime missing")
	}
	if c.Normal == nil || c.Normal.Adapter == nil {
		return d.live, nil
	}
	b := *c.Normal.Adapter
	if b.Revision < 1 || b.Hash == "" {
		return nil, errors.New("target adapter must pin revision and hash")
	}
	_, doc, err := d.resolveAdapter(ctx, b)
	if err != nil {
		return nil, err
	}
	selected := *d.live
	selected.config = adapterConfig(d.live.config, doc)
	return &selected, nil
}
func (d *Daemon) liveForAdmission(ctx context.Context, p AdmitParams) (*liveExecutor, error) {
	if p.TargetConfig == "" {
		return d.live, nil
	}
	r, raw, err := d.store.GetTarget(ctx, p.TargetConfig, p.TargetRevision)
	if err != nil {
		return nil, err
	}
	if p.TargetRevision < 1 || p.TargetHash == "" || r.Hash != p.TargetHash {
		return nil, errors.New("target identity missing or changed")
	}
	c, err := parseTarget(raw)
	if err != nil {
		return nil, err
	}
	return d.liveForTarget(ctx, c)
}
func (d *Daemon) liveForTask(ctx context.Context, task string) (*liveExecutor, error) {
	d.mu.Lock()
	snapshot, ok := d.jobs[task]
	d.mu.Unlock()
	if !ok {
		return nil, errors.New("adapter task missing")
	}
	var e envelope
	if err := decodeEnvelope(snapshot.Payload, &e); err != nil {
		return nil, err
	}
	return d.liveForAdmission(ctx, e.Admission)
}
func (d *Daemon) getAdapter(ctx context.Context, id string, revision int64) (any, error) {
	b, doc, err := d.resolveAdapter(ctx, AdapterBinding{ID: id, Revision: revision})
	if err != nil {
		return nil, err
	}
	retired := false
	if e := d.store.AdapterActive(ctx, id); errors.Is(e, controlplane.ErrAdapterRetired) {
		retired = true
	} else if e != nil {
		return nil, e
	}
	return struct {
		Retired  bool            `json:"retired"`
		Binding  AdapterBinding  `json:"binding"`
		Document AdapterDocument `json:"document"`
	}{retired, b, doc}, nil
}

// Network/schema preparation happens outside the daemon writer mutex; the
// audited immutable install commits under the store transaction event clock.
func (d *Daemon) adapterInstallRPC(ctx context.Context, r api.Request, principal api.Principal) (any, error) {
	if principal.Actor != "operator" || principal.Task != "" {
		return nil, errors.New("adapter installation requires instance operator")
	}
	var p AdapterPutParams
	if err := decode(r.Params, &p); err != nil {
		return nil, err
	}
	d.mu.Lock()
	stopping := d.ctx.Err() != nil
	d.mu.Unlock()
	if stopping {
		return nil, errors.New("shutting-down")
	}
	if _, err := os.Stat(filepath.Join(d.layout.Root, "maintenance")); err == nil {
		return nil, errors.New("maintenance")
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	ctx, err := d.store.AuthorizeAction(ctx, p.AuthorizingMessage, principal.Actor, "AdapterPut", "governance", false, actionClasses())
	if err != nil {
		return nil, err
	}
	return d.putAdapter(ctx, controlplane.Audit{Actor: principal.Actor, Action: "AdapterPut", MessageID: p.AuthorizingMessage, Inputs: r.Params, Outcome: "ok"}, r.Params)
}
