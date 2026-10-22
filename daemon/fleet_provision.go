package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/external"
)

// Attempted is written before the one allowed POST. Once true, recovery is
// read-only until the adapter proves the exact effect; a 404 never permits replay.
type ReviewProvisionRecord struct {
	Intent    external.Intent   `json:"intent"`
	Attempted bool              `json:"attempted"`
	Receipt   *external.Receipt `json:"receipt,omitempty"`
}

type FleetProvisionBinding struct {
	Package        string `json:"package"`
	Workspace      string `json:"workspace"`
	Branch         string `json:"branch"`
	IntegrationTip string `json:"integration_tip"`
}

func provisionBinding(r ReviewWorkspaceRecord) FleetProvisionBinding {
	return FleetProvisionBinding{r.Policy.Package, r.Workspace, r.Branch, r.IntegrationTip}
}
func initializeReviewProvision(r *ReviewWorkspaceRecord, bundle AdapterDocument) error {
	alias := r.Policy.ProvisionTool
	if alias == "" {
		if r.Provision != nil {
			return errors.New("unexpected fleet provisioning intent")
		}
		return nil
	}
	if r.Policy.IntegrationTip != r.IntegrationTip {
		return errors.New("fleet integration baseline binding differs")
	}
	tool, ok := bundle.Tools[alias]
	if !ok || tool.DiscoveredSchema == nil {
		return errors.New("fleet provisioning requires installed discovered tool")
	}
	expected := map[string]string{"package": r.Policy.Package, "workspace": r.Workspace, "branch": r.Branch, "integration_tip": r.IntegrationTip}
	for key, value := range expected {
		if value == "" || tool.Arguments[key] != value {
			return fmt.Errorf("fleet provisioning exact %s pin differs", key)
		}
	}
	intent, err := external.NewIntent(tool, r.Task, "review-workspace-provision", reviewWorkspaceKey(r.Task), tool.Role, r.IntegrationTip, "review-prepare/"+controlplane.Digest([]byte(r.Task)), tool.Arguments)
	if err != nil {
		return err
	}
	if r.Provision == nil {
		if r.State == "ready" {
			return errors.New("ready workspace lacks fleet provisioning proof")
		}
		r.Provision = &ReviewProvisionRecord{Intent: intent}
		return nil
	}
	if !targetEqual(r.Provision.Intent, intent) {
		return errors.New("immutable fleet provisioning intent differs")
	}
	if r.State == "ready" && r.Provision.Receipt == nil {
		return errors.New("ready workspace lacks fleet receipt")
	}
	if r.Provision.Receipt != nil {
		if !r.Provision.Attempted {
			return errors.New("fleet receipt without durable attempt")
		}
		return matchProvision(tool, *r, *r.Provision.Receipt)
	}
	return nil
}
func matchProvision(tool external.Tool, record ReviewWorkspaceRecord, receipt external.Receipt) error {
	if err := external.Match(tool, record.Provision.Intent, receipt); err != nil {
		return err
	}
	var binding FleetProvisionBinding
	if err := decode(receipt.Data, &binding); err != nil {
		return fmt.Errorf("fleet binding receipt: %w", err)
	}
	if binding != provisionBinding(record) {
		return errors.New("fleet receipt binding differs from immutable package workspace")
	}
	return nil
}
func (d *Daemon) saveReviewPreparation(ctx context.Context, record ReviewWorkspaceRecord, audit controlplane.Audit) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return d.store.PutMarker(ctx, audit, reviewWorkspaceKey(record.Task), raw)
}
func (d *Daemon) provisionReviewWorkspace(ctx context.Context, record *ReviewWorkspaceRecord, bundle AdapterDocument, audit controlplane.Audit) error {
	if record.Provision == nil || record.Provision.Receipt != nil {
		return nil
	}
	tool := bundle.Tools[record.Policy.ProvisionTool]
	var receipt external.Receipt
	var err error
	if record.Provision.Attempted {
		var found bool
		receipt, found, err = external.Query(ctx, tool, record.Provision.Intent)
		if err != nil {
			return fmt.Errorf("fleet provisioning uncertain; query failed: %w", err)
		}
		if !found {
			return errors.New("fleet provisioning uncertain; absent receipt does not authorize replay")
		}
	} else {
		record.Provision.Attempted = true
		if err = d.saveReviewPreparation(ctx, *record, audit); err != nil {
			return err
		}
		receipt, err = external.Perform(ctx, tool, record.Provision.Intent)
		if err != nil {
			return fmt.Errorf("fleet provisioning uncertain; retry preparation to query receipt: %w", err)
		}
	}
	if err = matchProvision(tool, *record, receipt); err != nil {
		return err
	}
	record.Provision.Receipt = &receipt
	return d.saveReviewPreparation(ctx, *record, audit)
}

func validateFleetProvisionTools(c LiveConfig) error {
	for task, policy := range c.ReviewPolicies {
		if policy.ProvisionTool == "" {
			continue
		}
		tool, ok := c.ExternalTools[policy.ProvisionTool]
		if !ok || tool.DiscoveredSchema == nil {
			return fmt.Errorf("fleet policy %s requires discovered provision tool", task)
		}
		for key, value := range map[string]string{"package": policy.Package, "workspace": policy.Workspace, "branch": policy.Branch, "integration_tip": policy.IntegrationTip} {
			if value == "" || tool.Arguments[key] != value {
				return fmt.Errorf("fleet policy %s requires exact %s argument", task, key)
			}
		}
	}
	return nil
}

// Provisioning capability belongs exclusively to explicit operator preparation,
// including aliases of the same installed vendor operation.
func isFleetProvisionTool(c LiveConfig, alias string) bool {
	candidate, ok := c.ExternalTools[alias]
	if !ok {
		return false
	}
	for _, policy := range c.ReviewPolicies {
		if policy.ProvisionTool == "" {
			continue
		}
		provision, present := c.ExternalTools[policy.ProvisionTool]
		if alias == policy.ProvisionTool || (present && strings.TrimRight(candidate.Endpoint, "/") == strings.TrimRight(provision.Endpoint, "/") && candidate.Server == provision.Server && candidate.Name == provision.Name) {
			return true
		}
	}
	return false
}
